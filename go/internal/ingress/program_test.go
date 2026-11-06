// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ingress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/agent"
	"github.com/gardenlinux/feos/go/internal/ingress/proxy"
	"github.com/gardenlinux/feos/go/internal/ingress/wg"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

func TestReadiness(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	ok := agentState{hasState: true, meshConfigured: true}
	fresh := proxy.DataPath{EndpointNodes: []string{"n1"}, FreshAdmittedNodes: []string{"n1"}, Endpoints: 2}
	mixed := proxy.DataPath{EndpointNodes: []string{"n1", "n2"}, FreshAdmittedNodes: []string{"n1"}, Endpoints: 3}
	staleOnly := proxy.DataPath{EndpointNodes: []string{"n2"}, Endpoints: 1}
	var many []string
	for i := range 8 {
		many = append(many, fmt.Sprintf("n%d", i))
	}
	for _, c := range []struct {
		name   string
		a      agentState
		dp     proxy.DataPath
		first  time.Time
		ready  bool
		reason string
	}{
		{"no state", agentState{meshConfigured: true}, proxy.DataPath{}, time.Time{}, false, "no routing table"},
		{"mesh not configured", agentState{hasState: true}, proxy.DataPath{}, now.Add(-time.Hour), false, "mesh device is not configured"},
		{"draining", agentState{hasState: true, meshConfigured: true, draining: true}, fresh, now.Add(-time.Hour), false, "shutting down"},
		{"removed", agentState{hasState: true, meshConfigured: true, removed: true, removedText: "ingress ing-0 was removed"},
			fresh, now.Add(-time.Hour), false, "removed the ingress (ingress ing-0 was removed)"},
		{"no endpoints", ok, proxy.DataPath{}, now, true, ""},
		{"every endpoint node fresh", ok, fresh, now, true, ""},
		{"endpoints on a stale node only", ok, staleOnly, now.Add(-10 * time.Second), false, "none of the 1 nodes with endpoints has a fresh handshake and admits the ingress: n2"},
		{"endpoints on a stale node only, after 60 s", ok, staleOnly, now.Add(-time.Hour), false, "none of the 1 nodes"},
		{"one fresh and one stale node before 60 s", ok, mixed, now.Add(-59 * time.Second), false, "first table, for nodes with endpoints to have a fresh handshake and admit the ingress: n2"},
		{"one fresh and one stale node at 60 s", ok, mixed, now.Add(-60 * time.Second), true, ""},
		{"one fresh and one stale node after 60 s", ok, mixed, now.Add(-5 * time.Minute), true, ""},
		{"one fresh and one stale node without a first table", ok, mixed, time.Time{}, false, "n2"},
		{"many stale nodes", ok, proxy.DataPath{EndpointNodes: many, Endpoints: 8}, now, false, "n0, n1, n2, n3, n4 and 3 more"},
	} {
		ready, reason := readiness(c.a, c.dp, c.first, now)
		if ready != c.ready || !strings.Contains(reason, c.reason) || (ready && reason != "") {
			t.Errorf("%s: ready %v %q; want %v %q", c.name, ready, reason, c.ready, c.reason)
		}
	}
}

func TestFreshNodes(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	got := freshNodes(map[string]time.Time{
		"recent": now.Add(-5 * time.Second),
		"edge":   now.Add(-179 * time.Second),
		"old":    now.Add(-180 * time.Second),
		"none":   {},
	}, now)
	want := map[string]bool{"recent": true, "edge": true, "old": false, "none": false}
	if len(got) != len(want) {
		t.Fatalf("fresh %v", got)
	}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("node %s: fresh %v, want %v", n, got[n], w)
		}
	}
}

// CopyJoinToken copies the instance's own token, readable by its owner
// alone, and removes the copy when the Secret holds none for it.
func TestCopyJoinToken(t *testing.T) {
	t.Parallel()
	secret, dir := t.TempDir(), t.TempDir()
	src, dst := filepath.Join(secret, "ferun-ingress-0"), filepath.Join(dir, "token")
	if err := os.WriteFile(src, []byte("feos-ingress-join-x.y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "ferun-ingress-1"), []byte("the other's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs syncBuf
	if err := CopyJoinToken(src, dst, NewLogger(&logs, LogFormatJSON, 0)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil || fi.Mode().Perm() != 0o400 {
		t.Fatalf("the copy %v %v", fi, err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "feos-ingress-join-x.y\n" {
		t.Fatalf("the copy holds %q %v", b, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the volume holds %v", entries)
	}
	if !strings.Contains(logs.String(), "copied the instance's join token") {
		t.Fatalf("log %s", logs.String())
	}
	// Again over the copy, as after a restart of the pod.
	if err := os.WriteFile(src, []byte("feos-ingress-join-z.y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CopyJoinToken(src, dst, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "feos-ingress-join-z.y\n" {
		t.Fatalf("the second copy holds %q", b)
	}
	// None in the Secret: no copy.
	if err := CopyJoinToken(filepath.Join(secret, "ferun-ingress-2"), dst, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("the copy without a token: %v", err)
	}
	if err := CopyJoinToken(filepath.Join(secret, "ferun-ingress-2"), dst, nil); err != nil {
		t.Fatalf("again without a token: %v", err)
	}
	// A file far larger than a token.
	if err := os.WriteFile(src, make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CopyJoinToken(src, dst, nil); err == nil {
		t.Fatal("copied 5000 bytes")
	}
}

func TestPrepare(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for conf, v := range map[string]string{"all": "1\n", "default": "0\n"} {
		if err := os.MkdirAll(filepath.Join(dir, conf), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, conf, "disable_ipv6"), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var logs syncBuf
	if err := Prepare(dir, NewLogger(&logs, LogFormatJSON, 0)); err != nil {
		t.Fatal(err)
	}
	for _, conf := range []string{"all", "default"} {
		if b, err := os.ReadFile(filepath.Join(dir, conf, "disable_ipv6")); err != nil || strings.TrimSpace(string(b)) != "0" {
			t.Fatalf("%s: %q %v", conf, b, err)
		}
	}
	if !strings.Contains(logs.String(), `"msg":"ferun-ingress prepare: enabled IPv6"`) || !strings.Contains(logs.String(), `"was":"1"`) {
		t.Fatalf("log %s", logs.String())
	}
	// Without the settings (no IPv6 in the namespace) there is nothing to do.
	if err := Prepare(filepath.Join(dir, "missing"), nil); err != nil {
		t.Fatal(err)
	}
}

// The test environment: one environment with a root route to web, whose
// one instance runs on progNode.
const (
	progName = "ferun-ingress-0"
	progZone = "apps.test"
	progHost = "app.apps.test"
	progEnv  = "8a6b3c62-6d4f-4b55-9a49-3a1c2c1b0f01"
	progPod  = "fd00:1:2:1::5"
	progNode = "node-a"
)

// fakeMesh is a mesh device that keeps the peers it is given and reports
// a handshake of now with each of them.
type fakeMesh struct {
	mu      sync.Mutex
	configs int
	peers   []wg.Peer
}

func (m *fakeMesh) Configure(agent.MeshConfig) error {
	m.mu.Lock()
	m.configs++
	m.mu.Unlock()
	return nil
}

func (m *fakeMesh) SetPeers(p []wg.Peer) error {
	m.mu.Lock()
	m.peers = slices.Clone(p)
	m.mu.Unlock()
	return nil
}

func (m *fakeMesh) Status() (wg.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := wg.Device{Name: agent.DefaultMeshDevice}
	for _, p := range m.peers {
		d.Peers = append(d.Peers, wg.PeerState{Peer: p, LastHandshake: time.Now()})
	}
	return d, nil
}

// syncBuf is a log sink for goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// progHarness runs a Program against a fake control plane, with a fake
// mesh device and a dial hook that maps pod addresses to local servers.
type progHarness struct {
	t      *testing.T
	fake   *agent.FakeControlPlane
	cfg    Config
	pool   *x509.CertPool
	logs   *syncBuf
	mu     sync.Mutex
	routes map[string]string
	// mesh, when set, is the program's mesh device in place of a
	// fakeMesh.
	mesh   agent.MeshDevice
	prog   *Program
	cancel context.CancelFunc
	done   chan error
}

func newProgHarness(t *testing.T) *progHarness {
	t.Helper()
	logs := &syncBuf{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	fake, err := agent.NewFakeControlPlane(t.TempDir(), agent.FakeConfig{AppsZone: progZone})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	dir := t.TempDir()
	token, err := fake.Token(progName)
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	certFile, keyFile, pool := wildcardPair(t, dir, "*."+progZone)
	cfg := Default()
	cfg.Name = progName
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.JoinTokenFile = tokenFile
	cfg.ControlPlane = []string{fake.Addr()}
	cfg.ControlPlaneName = fake.ServerName()
	cfg.TrustDomain = fake.TrustDomain()
	cfg.ListenHTTP, cfg.ListenHTTPS, cfg.HealthListen = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
	cfg.TLSCert, cfg.TLSKey = certFile, keyFile
	cfg.MeshEndpoint = "127.0.0.1:51820"
	cfg.ShutdownDelay = 3 * time.Second
	cfg.ShutdownGrace = 3 * time.Second
	cfg.Version = "test"

	key, err := wg.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := key.PublicKey()
	fake.SetMesh(wire.Mesh{Version: 1, Peers: []wire.Peer{{
		Name: progNode, PublicKey: pub[:], Endpoint: "10.250.0.5:51820", Prefix: "fd00:1:2:1::/64",
	}}})
	fake.SetAdmittedVersion(progName, 3)
	fake.PutEnv(wire.Env{
		ID: progEnv, Tenant: "acme", Name: "app", Hosts: []string{progHost}, Deployment: 1, Hash: "h1",
		Routes: []wire.Route{{Name: "root", Path: "/", Service: "web", Port: "http"}},
		Services: []wire.Service{{Name: "web", Endpoints: []wire.Endpoint{{
			Instance: "inst-1", Address: progPod, Node: progNode, Ports: map[string]int{"http": 8080}, GroupVersion: 3,
		}}}},
	})
	return &progHarness{t: t, fake: fake, cfg: cfg, pool: pool, logs: logs, routes: map[string]string{}}
}

// upstream serves h as the instance's port 8080.
func (h *progHarness) upstream(handler http.Handler) {
	srv := httptest.NewServer(handler)
	h.t.Cleanup(srv.Close)
	h.mu.Lock()
	h.routes[net.JoinHostPort(progPod, "8080")] = srv.Listener.Addr().String()
	h.mu.Unlock()
}

func (h *progHarness) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	h.mu.Lock()
	real, ok := h.routes[addr]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no route to %s", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", real)
}

// start makes the program and runs it until the test ends or stop.
func (h *progHarness) start() {
	h.t.Helper()
	var mesh agent.MeshDevice = &fakeMesh{}
	if h.mesh != nil {
		mesh = h.mesh
	}
	prog, err := New(h.cfg, Deps{Mesh: mesh, Dial: h.dial, Log: NewLogger(h.logs, LogFormatText, -4)})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.prog, h.cancel, h.done = prog, cancel, make(chan error, 1)
	go func() { h.done <- prog.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(30 * time.Second):
			h.t.Error("the program did not stop")
		}
	})
}

// waitReady waits for /readyz to answer 200.
func (h *progHarness) waitReady() {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		code, body := h.health("/readyz")
		if code == http.StatusOK {
			return
		}
		last = fmt.Sprintf("%d %s", code, body)
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("not ready: %s", last)
}

// waitNotReady waits for /readyz to answer 503 with a reason that holds
// want.
func (h *progHarness) waitNotReady(want string) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		code, body := h.health("/readyz")
		if code == http.StatusServiceUnavailable && strings.Contains(body, want) {
			return
		}
		last = fmt.Sprintf("%d %s", code, body)
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("not unready with %q: %s", want, last)
}

// health GETs a path of the health listener.
func (h *progHarness) health(path string) (int, string) {
	resp, err := http.Get("http://" + h.prog.Addrs().Health + path)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// client is an HTTPS client that reaches progHost at the HTTPS listener,
// checking the test's wildcard certificate.
func (h *progHarness) client() *http.Client {
	addr := h.prog.Addrs().HTTPS
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: h.pool, ServerName: progHost},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2: true,
	}
	h.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func get(c *http.Client, url string) (int, string, error) {
	resp, err := c.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// echoHandler answers what it got.
func echoHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "host=%s path=%s proto=%s for=%s", r.Host, r.URL.Path, r.Header.Get("X-Forwarded-Proto"), r.Header.Get("X-Forwarded-For"))
}

// A program joins the fake control plane, configures the mesh, routes the
// table it streams and serves a request over HTTPS with the wildcard
// certificate to the instance, through the dial hook; the health listener
// reports it.
func TestProgramServes(t *testing.T) {
	t.Parallel()
	h := newProgHarness(t)
	h.upstream(http.HandlerFunc(echoHandler))
	h.cfg.ShutdownDelay = 50 * time.Millisecond
	h.start()

	if code, body := h.health("/healthz"); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("healthz %d %q", code, body)
	}
	h.waitReady()

	c := h.client()
	code, body, err := get(c, "https://"+progHost+"/hello")
	if err != nil || code != http.StatusOK {
		t.Fatalf("GET: %d %q %v", code, body, err)
	}
	if !strings.HasPrefix(body, "host="+progHost+" path=/hello proto=https for=127.0.0.1") {
		t.Fatalf("upstream got %q", body)
	}
	if code, body, err := get(c, "https://other.apps.test/"); err != nil || code != http.StatusNotFound {
		t.Fatalf("unknown host: %d %q %v", code, body, err)
	}

	// HTTP redirects a known host to https.
	plain := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodGet, "http://"+h.prog.Addrs().HTTP+"/a?b=c", nil)
	req.Host = progHost
	resp, err := plain.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "https://"+progHost+"/a?b=c" {
		t.Fatalf("redirect %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	code, body = h.health("/statusz")
	if code != http.StatusOK {
		t.Fatalf("statusz %d %s", code, body)
	}
	var st Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Name != progName || st.Version != "test" || !st.Ready || st.Reason != "" || st.Draining || !st.Connected ||
		st.Instance == "" || !slices.Equal(st.ControlPlanes, h.cfg.ControlPlane) || st.AdmittedVersion != 3 ||
		st.TableSeq == 0 || st.CertSerial == "" || st.CertNotAfter.IsZero() || !st.MeshConfigured || st.Address == "" ||
		st.FirstStateAt.IsZero() || st.DataPath.Endpoints != 1 || !slices.Equal(st.DataPath.FreshAdmittedNodes, []string{progNode}) ||
		!slices.Equal(st.TLSNames, []string{"*." + progZone}) || st.TLSNotAfter.IsZero() || st.Held == nil {
		t.Fatalf("status %s", body)
	}
	if len(st.MeshPeers) != 1 || st.MeshPeers[0].Node != progNode || !st.MeshPeers[0].Fresh ||
		st.MeshPeers[0].Endpoint != "10.250.0.5:51820" || st.MeshPeers[0].LastHandshake.IsZero() {
		t.Fatalf("mesh peers %+v", st.MeshPeers)
	}
	if strings.Count(body, `"meshPeers"`) != 1 {
		t.Fatalf("meshPeers twice in %s", body)
	}

	code, body = h.health("/metrics")
	for _, want := range []string{
		`ferun_ingress_requests_total{class="2xx"} 1`,
		`ferun_ingress_requests_total{class="4xx"} 1`,
		`ferun_ingress_requests_total{class="3xx"} 1`,
		"ferun_ingress_ready 1",
		"ferun_ingress_control_connected 1",
		"ferun_ingress_mesh_peers 1",
		`ferun_ingress_mesh_handshake_age_seconds{node="node-a"}`,
		`ferun_ingress_info{name="ferun-ingress-0",version="test"} 1`,
		"ferun_ingress_tls_not_after_seconds",
		"ferun_ingress_identity_not_after_seconds",
	} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
	if hellos := h.fake.Hellos(); len(hellos) == 0 || hellos[0].Request.Version != "test" || hellos[0].Request.Endpoint != "127.0.0.1:51820" {
		t.Fatalf("hellos %+v", hellos)
	}
}

// SIGTERM: /readyz fails at once, requests are still served during the
// delay, upgraded connections are closed at the end, and the control
// plane gets Hold{Leaving}.
func TestProgramShutdown(t *testing.T) {
	t.Parallel()
	h := newProgHarness(t)
	h.upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "echo" {
			echoHandler(w, r)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		for {
			line, err := rw.ReadString('\n')
			if err != nil {
				return
			}
			_, _ = rw.WriteString(line)
			_ = rw.Flush()
		}
	}))
	h.start()
	h.waitReady()
	c := h.client()
	if code, body, err := get(c, "https://"+progHost+"/before"); err != nil || code != http.StatusOK {
		t.Fatalf("GET: %d %q %v", code, body, err)
	}

	// An upgraded connection through the HTTPS listener.
	conn, err := tls.Dial("tcp", h.prog.Addrs().HTTPS, &tls.Config{RootCAs: h.pool, ServerName: progHost, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: "+progHost+"\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	echo := func(s string) error {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(conn, s+"\n"); err != nil {
			return err
		}
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if line != s+"\n" {
			return fmt.Errorf("echo %q, want %q", line, s)
		}
		return nil
	}
	if err := echo("one"); err != nil {
		t.Fatal(err)
	}

	stopped := time.Now()
	h.cancel()
	// /readyz fails at once.
	deadline := stopped.Add(time.Second)
	for {
		code, body := h.health("/readyz")
		if code == http.StatusServiceUnavailable && strings.Contains(body, "shutting down") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("readyz %d %q after the stop", code, body)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Requests are served during the delay, on a new connection as well.
	if code, body, err := get(c, "https://"+progHost+"/during"); err != nil || code != http.StatusOK || !strings.Contains(body, "path=/during") {
		t.Fatalf("GET during the delay: %d %q %v", code, body, err)
	}
	if code, _, err := get(h.client(), "https://"+progHost+"/new"); err != nil || code != http.StatusOK {
		t.Fatalf("GET on a new connection during the delay: %d %v", code, err)
	}
	if err := echo("two"); err != nil {
		t.Fatalf("the upgraded connection during the delay: %v", err)
	}
	if time.Since(stopped) >= h.cfg.ShutdownDelay {
		t.Fatalf("the checks took longer than the delay (%s); the test proves nothing", time.Since(stopped))
	}

	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		h.done <- nil // for the cleanup
	case <-time.After(20 * time.Second):
		t.Fatal("the program did not stop")
	}
	if took := time.Since(stopped); took < h.cfg.ShutdownDelay {
		t.Fatalf("stopped after %s, before the delay", took)
	}

	// The upgraded connection is closed.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("the upgraded connection is still open")
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatalf("the upgraded connection is still open: %v", err)
	}
	// The control plane got the leave.
	leaving := false
	for _, hold := range h.fake.Holds() {
		if hold.Name == progName && hold.Request.Leaving {
			leaving = true
		}
	}
	if !leaving {
		t.Fatalf("no Hold{Leaving}: %+v", h.fake.Holds())
	}
	// The listeners are closed.
	for _, addr := range []string{h.prog.Addrs().HTTPS, h.prog.Addrs().HTTP, h.prog.Addrs().Health} {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			_ = c.Close()
			t.Errorf("%s still accepts connections", addr)
		}
	}
}

// New fails without a valid TLS pair or with a taken address, and Close
// releases the listeners of a program that never ran.
func TestNewAndClose(t *testing.T) {
	t.Parallel()
	h := newProgHarness(t)

	bad := h.cfg
	bad.TLSKey = filepath.Join(t.TempDir(), "missing.key")
	if _, err := New(bad, Deps{Mesh: &fakeMesh{}}); err == nil || !strings.Contains(err.Error(), "TLS key") {
		t.Fatalf("New without the key: %v", err)
	}
	bad = h.cfg
	bad.TenantZoneTLSCert, bad.TenantZoneTLSKey = h.cfg.TLSCert, filepath.Join(t.TempDir(), "missing.key")
	if _, err := New(bad, Deps{Mesh: &fakeMesh{}}); err == nil || !strings.Contains(err.Error(), "tenant zone") {
		t.Fatalf("New without the tenant zone's key: %v", err)
	}
	if _, err := New(h.cfg, Deps{}); err == nil || !strings.Contains(err.Error(), "mesh") {
		t.Fatalf("New without a mesh: %v", err)
	}
	invalid := h.cfg
	invalid.ListenHTTP = "0.0.0.0:0"
	if _, err := New(invalid, Deps{Mesh: &fakeMesh{}}); err == nil || !strings.Contains(err.Error(), "--listen-http") {
		t.Fatalf("New with a wildcard: %v", err)
	}

	p, err := New(h.cfg, Deps{Mesh: &fakeMesh{}})
	if err != nil {
		t.Fatal(err)
	}
	taken := h.cfg
	taken.HealthListen = p.Addrs().Health
	if _, err := New(taken, Deps{Mesh: &fakeMesh{}}); err == nil || !strings.Contains(err.Error(), "health listener") {
		t.Fatalf("New on a taken address: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("Run after Close")
	}
	// The addresses are free again.
	q, err := New(taken, Deps{Mesh: &fakeMesh{}})
	if err != nil {
		t.Fatal(err)
	}
	_ = q.Close()
}

// wildcardPair writes a self-signed certificate for name and its key, and
// returns a pool that trusts it.
func wildcardPair(t *testing.T, dir, name string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

// --memory-limit sets the Go runtime's soft limit to 90% of the
// container's, unless GOMEMLIMIT set one.
func TestSetMemoryLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	if prev != math.MaxInt64 {
		t.Skip("GOMEMLIMIT is set")
	}
	if got := setMemoryLimit(0); got != 0 || debug.SetMemoryLimit(-1) != math.MaxInt64 {
		t.Errorf("no limit: %d, runtime %d", got, debug.SetMemoryLimit(-1))
	}
	const want = int64(512<<20) / 100 * 90
	if got := setMemoryLimit(512 << 20); got != want || debug.SetMemoryLimit(-1) != want {
		t.Errorf("512 MiB: %d, runtime %d, want %d", got, debug.SetMemoryLimit(-1), want)
	}
	// A limit set already, as GOMEMLIMIT sets it, stays.
	if got := setMemoryLimit(1 << 30); got != want || debug.SetMemoryLimit(-1) != want {
		t.Errorf("a second limit: %d, runtime %d, want %d", got, debug.SetMemoryLimit(-1), want)
	}
}
