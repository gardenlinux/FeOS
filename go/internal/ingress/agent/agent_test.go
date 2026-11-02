// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wg"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/pki"
)

const testName = "ing-0"

// fakeMesh is a MeshDevice that records what it is told and reports the
// peers a test gives it.
type fakeMesh struct {
	mu        sync.Mutex
	configs   []MeshConfig
	peers     [][]wg.Peer
	status    wg.Device
	configErr error
}

func (m *fakeMesh) Configure(c MeshConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.configErr != nil {
		return m.configErr
	}
	m.configs = append(m.configs, c)
	return nil
}

func (m *fakeMesh) SetPeers(p []wg.Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peers = append(m.peers, slices.Clone(p))
	return nil
}

func (m *fakeMesh) Status() (wg.Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.status
	d.Peers = slices.Clone(d.Peers)
	return d, nil
}

func (m *fakeMesh) setStatus(d wg.Device) {
	m.mu.Lock()
	m.status = d
	m.mu.Unlock()
}

func (m *fakeMesh) lastConfig() (MeshConfig, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.configs) == 0 {
		return MeshConfig{}, 0
	}
	return m.configs[len(m.configs)-1], len(m.configs)
}

func (m *fakeMesh) lastPeers() ([]wg.Peer, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.peers) == 0 {
		return nil, 0
	}
	return m.peers[len(m.peers)-1], len(m.peers)
}

// recorder keeps what OnState and OnMesh were given.
type recorder struct {
	mu     sync.Mutex
	states []*wire.State
	meshes []map[string]time.Time
}

func (r *recorder) onState(s *wire.State) {
	r.mu.Lock()
	r.states = append(r.states, s)
	r.mu.Unlock()
}

func (r *recorder) onMesh(m map[string]time.Time) {
	r.mu.Lock()
	r.meshes = append(r.meshes, m)
	r.mu.Unlock()
}

func (r *recorder) lastState() (*wire.State, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.states) == 0 {
		return nil, 0
	}
	return r.states[len(r.states)-1], len(r.states)
}

func (r *recorder) lastMesh() map[string]time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.meshes) == 0 {
		return nil
	}
	return r.meshes[len(r.meshes)-1]
}

// logBuf collects the agent's log; a failed test prints it.
type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (b *logBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.lines = append(b.lines, string(p))
	b.mu.Unlock()
	return len(p), nil
}

func (b *logBuf) contains(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.lines {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

func (b *logBuf) dump(t *testing.T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.lines {
		t.Log(strings.TrimRight(l, "\n"))
	}
}

type harness struct {
	t    *testing.T
	fake *FakeControlPlane
	cfg  Config
	// nextMesh, when set, is the mesh device of the next agent started.
	nextMesh *fakeMesh
	// prepare, when set, changes the next agent started before it runs.
	prepare func(*Agent)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake, err := NewFakeControlPlane(t.TempDir(), FakeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	dir := t.TempDir()
	return &harness{t: t, fake: fake, cfg: Config{
		Name:             testName,
		StateDir:         filepath.Join(dir, "state"),
		JoinTokenFile:    filepath.Join(dir, "token"),
		ControlPlane:     []string{fake.Addr()},
		ControlPlaneName: fake.ServerName(),
		TrustDomain:      fake.TrustDomain(),
		MeshEndpoint:     "10.0.0.5:51820",
		MeshMTU:          1380,
		Version:          "test",
	}}
}

// fastTiming shortens every interval of the agent for tests.
func fastTiming() timing {
	tm := defaultTiming()
	tm.backoffMin, tm.backoffMax = 10*time.Millisecond, 100*time.Millisecond
	tm.dialTimeout, tm.handshakeTimeout = 2*time.Second, 2*time.Second
	tm.callTimeout, tm.joinTimeout = 5*time.Second, 5*time.Second
	tm.tokenPoll = 50 * time.Millisecond
	tm.keyFixedRetry = 150 * time.Millisecond
	tm.renewRetry = 150 * time.Millisecond
	tm.meshPoll = 30 * time.Millisecond
	tm.wakeEvery = 300 * time.Millisecond
	tm.cacheDelay = 20 * time.Millisecond
	return tm
}

// writeToken puts a token into the token file.
func (h *harness) writeToken(tok string) {
	h.t.Helper()
	if err := os.WriteFile(h.cfg.JoinTokenFile, []byte(tok+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) mintToken() string {
	h.t.Helper()
	tok, err := h.fake.Token(testName)
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

// testAgent is an agent of a test with its collaborators.
type testAgent struct {
	*Agent
	mesh *fakeMesh
	rec  *recorder
	logs *logBuf
	stop func()
}

// start makes an agent with cfg and runs it until stop or the end of the
// test.
func (h *harness) start(cfg Config, tune func(*timing)) *testAgent {
	h.t.Helper()
	ta := &testAgent{mesh: &fakeMesh{}, rec: &recorder{}, logs: &logBuf{}}
	if h.nextMesh != nil {
		ta.mesh, h.nextMesh = h.nextMesh, nil
	}
	a, err := New(cfg, Deps{
		Mesh:    ta.mesh,
		OnState: ta.rec.onState,
		OnMesh:  ta.rec.onMesh,
		Log:     slog.New(slog.NewTextHandler(ta.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	a.tune = fastTiming()
	if tune != nil {
		tune(&a.tune)
	}
	if h.prepare != nil {
		h.prepare(a)
		h.prepare = nil
	}
	ta.Agent = a
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var once sync.Once
	ta.stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					h.t.Errorf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				h.t.Error("Run did not return after its context ended")
			}
		})
	}
	h.t.Cleanup(func() {
		ta.stop()
		if h.t.Failed() {
			ta.logs.dump(h.t)
		}
	})
	return ta
}

// joined starts an agent with a fresh token and waits until it is
// connected and has the whole table.
func (h *harness) joined(tune func(*timing)) *testAgent {
	h.t.Helper()
	h.writeToken(h.mintToken())
	ta := h.start(h.cfg, tune)
	eventually(h.t, "connected with a table", func() bool {
		s, _ := ta.rec.lastState()
		return ta.Connected() && s != nil
	})
	return ta
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// deadAddr is a loopback address nobody listens on.
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// dialRecorder dials the control plane for an agent and records the
// addresses it dialled, in order.
type dialRecorder struct {
	mu    sync.Mutex
	addrs []string
}

func (r *dialRecorder) dial(ctx context.Context, addr string) (net.Conn, error) {
	r.mu.Lock()
	r.addrs = append(r.addrs, addr)
	r.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (r *dialRecorder) dials() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.addrs)
}

func (r *dialRecorder) reset() {
	r.mu.Lock()
	r.addrs = nil
	r.mu.Unlock()
}

// ageCache moves table.json's savedAt d into the past.
func ageCache(t *testing.T, dir string, d time.Duration) {
	t.Helper()
	sd := &stateDir{path: dir}
	s, _, err := sd.loadTable()
	if err != nil || s == nil {
		t.Fatalf("table.json %v, %v", s, err)
	}
	if err := sd.saveTable(s, time.Now().Add(-d)); err != nil {
		t.Fatal(err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func lostFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, lostPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func testEnv(id string) wire.Env {
	return wire.Env{
		ID: id, Tenant: "acme", Name: "prod", Hosts: []string{"prod-acme.apps.test"}, Deployment: 1, Hash: "h-" + id,
		Routes: []wire.Route{{Name: "web", Path: "/", Service: "web", Port: "http"}},
		Services: []wire.Service{{Name: "web", Endpoints: []wire.Endpoint{{
			Instance: "i-" + id, Address: "fd00:1:2:10::5", Node: "n1", Ports: map[string]int{"http": 8080}, GroupVersion: 1,
		}}}},
	}
}

func newWGKey(t *testing.T) wg.Key {
	t.Helper()
	k, err := wg.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestJoinWithPin(t *testing.T) {
	h := newHarness(t)
	tok := h.mintToken()
	h.writeToken(tok)
	ta := h.start(h.cfg, nil)
	eventually(t, "connected", ta.Connected)

	sent, _, err := pki.SplitIngressJoinToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	joins := h.fake.Joins()
	if len(joins) != 1 || joins[0].Name != testName || joins[0].TokenHash != pki.HashJoinSecret(sent) || joins[0].Refusal != "" {
		t.Fatalf("joins %+v", joins)
	}
	if !fileExists(filepath.Join(h.cfg.StateDir, certFile)) {
		t.Error("no cert.json after the join")
	}
	tried, err := (&stateDir{path: h.cfg.StateDir}).loadTried()
	if err != nil || !tried[pki.HashJoinSecret(sent)] {
		t.Errorf("join-tried %v, %v: the token is not recorded", tried, err)
	}
	st := ta.Status()
	if st.CertSerial == "" || st.Joining || !st.Connected || st.Instance != "fake" {
		t.Errorf("status %+v", st)
	}
	for _, name := range []string{keyFile, certFile, triedFile, wgKeyFile, meshFile} {
		fi, err := os.Stat(filepath.Join(h.cfg.StateDir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if fi.Mode().Perm() != fileMode {
			t.Errorf("%s has mode %v, want %v", name, fi.Mode().Perm(), os.FileMode(fileMode))
		}
	}
}

func TestJoinWrongPinIsRefusedBeforeTheTokenIsSent(t *testing.T) {
	h := newHarness(t)
	tok := h.mintToken()
	bad := tok[:strings.LastIndex(tok, ".")+1] + strings.Repeat("ab", 32)
	h.writeToken(bad)
	ta := h.start(h.cfg, nil)
	eventually(t, "the pin check fails", func() bool { return ta.logs.contains("not the one the join token pins") })
	time.Sleep(200 * time.Millisecond)
	if joins := h.fake.Joins(); len(joins) != 0 {
		t.Fatalf("the token was sent to a server that failed the pin: %+v", joins)
	}
	if ta.Connected() || !ta.Status().Joining {
		t.Fatalf("status %+v", ta.Status())
	}
	// The token was not spent: the right pin joins with it.
	h.writeToken(tok)
	eventually(t, "connected", ta.Connected)
}

func TestJoinRefusedTokenIsNotRetried(t *testing.T) {
	h := newHarness(t)
	// A token the control plane does not know, as a used one is refused.
	unknown, sent, _, err := pki.NewIngressJoinToken(h.fake.Issuer().JoinPin())
	if err != nil {
		t.Fatal(err)
	}
	h.writeToken(unknown)
	ta := h.start(h.cfg, nil)
	eventually(t, "the refusal", func() bool { return len(h.fake.Joins()) == 1 })
	// Several polls of the token file later, it was not sent again.
	time.Sleep(300 * time.Millisecond)
	if joins := h.fake.Joins(); len(joins) != 1 || joins[0].TokenHash != pki.HashJoinSecret(sent) || joins[0].Refusal == "" {
		t.Fatalf("joins %+v", joins)
	}
	if ta.Connected() {
		t.Fatal("connected without a certificate")
	}
	// A new token in the file is used.
	h.writeToken(h.mintToken())
	eventually(t, "connected with the new token", ta.Connected)
	if n := len(h.fake.Joins()); n != 2 {
		t.Errorf("%d joins, want 2", n)
	}
}

func TestJoinAnswerFailingThePinSpendsTheToken(t *testing.T) {
	h := newHarness(t)
	// The answer's trust bundle holds another CA's root, not the pinned
	// one; the TLS handshake passed the pin check, so the token was sent.
	other, err := NewFakeControlPlane(t.TempDir(), FakeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	h.fake.SetJoinTrustBundle(other.Issuer().BundleDER())
	tok := h.mintToken()
	h.writeToken(tok)
	ta := h.start(h.cfg, nil)
	eventually(t, "the answer refused", func() bool { return ta.logs.contains("lacks the root the token pins") })
	// Several backoffs and polls of the token file later, it was not sent
	// again.
	time.Sleep(300 * time.Millisecond)
	sent, _, err := pki.SplitIngressJoinToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	hash := pki.HashJoinSecret(sent)
	if joins := h.fake.Joins(); len(joins) != 1 || joins[0].TokenHash != hash {
		t.Fatalf("joins %+v", joins)
	}
	if ta.logs.contains("the token was not sent") {
		t.Error("a spent token logged as not sent")
	}
	tried, err := (&stateDir{path: h.cfg.StateDir}).loadTried()
	if err != nil || !tried[hash] {
		t.Errorf("join-tried %v, %v: the spent token is not recorded", tried, err)
	}
	if ta.Connected() || !ta.Status().Joining || fileExists(filepath.Join(h.cfg.StateDir, certFile)) {
		t.Fatalf("status %+v", ta.Status())
	}
	// A new token, answered with the pinned root, joins.
	h.fake.SetJoinTrustBundle(nil)
	h.writeToken(h.mintToken())
	eventually(t, "connected with the new token", ta.Connected)
	if n := len(h.fake.Joins()); n != 2 {
		t.Errorf("%d joins, want 2", n)
	}
}

func TestHelloConfiguresTheMesh(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)

	key, ok, err := (&stateDir{path: h.cfg.StateDir}).loadWGKey()
	if err != nil || !ok {
		t.Fatalf("wg.key: %v %v", ok, err)
	}
	hellos := h.fake.Hellos()
	if len(hellos) == 0 {
		t.Fatal("no Hello")
	}
	hr := hellos[0].Request
	if pub := key.PublicKey(); !slices.Equal(hr.PublicKey, pub[:]) || hr.Endpoint != h.cfg.MeshEndpoint || hr.Version != "test" {
		t.Errorf("Hello %+v", hr)
	}
	if hellos[0].Serial != ta.Status().CertSerial {
		t.Errorf("Hello presented serial %s, the agent holds %s", hellos[0].Serial, ta.Status().CertSerial)
	}
	_, addr, ok := h.fake.Place(testName)
	if !ok {
		t.Fatal("no place on the mesh")
	}
	mc, n := ta.mesh.lastConfig()
	want := MeshConfig{
		Device: DefaultMeshDevice, PrivateKey: key, ListenPort: DefaultMeshPort, MTU: 1380,
		Endpoint:   netip.MustParseAddrPort(h.cfg.MeshEndpoint),
		Address:    netip.PrefixFrom(addr, 128),
		PodNetwork: netip.MustParsePrefix("fd00:1:2::/48"),
	}
	if n != 1 || mc != want {
		t.Errorf("%d configures, last\n got %+v\nwant %+v", n, mc, want)
	}
	if !ta.MeshConfigured() {
		t.Error("the mesh is not configured")
	}
	place, err := (&stateDir{path: h.cfg.StateDir}).loadMesh()
	if err != nil || place == nil || place.Address != addr.String() || place.PodNetwork != "fd00:1:2::/48" {
		t.Errorf("mesh.json %+v, %v", place, err)
	}
	st := ta.Status()
	if st.Address != addr.String() || st.AppsZone != "apps.test" || st.MeshMTU != 1380 {
		t.Errorf("status %+v", st)
	}
}

func TestMeshConfigureFailureIsRetriedWhileTheTableFlows(t *testing.T) {
	h := newHarness(t)
	h.fake.PutEnv(testEnv("e1"))
	h.writeToken(h.mintToken())
	h.nextMesh = &fakeMesh{configErr: errors.New("IPv6 is disabled")}
	ta := h.start(h.cfg, nil)
	eventually(t, "the table without a mesh", func() bool {
		s, _ := ta.rec.lastState()
		return s != nil && ta.Connected()
	})
	if ta.MeshConfigured() {
		t.Fatal("the mesh reported configured")
	}
	eventually(t, "the failure logged", func() bool { return ta.logs.contains("configure the mesh device; retrying") })
	ta.mesh.mu.Lock()
	ta.mesh.configErr = nil
	ta.mesh.mu.Unlock()
	eventually(t, "the mesh configured by the retry", ta.MeshConfigured)
	if _, n := ta.mesh.lastConfig(); n != 1 {
		t.Errorf("%d configures, want 1", n)
	}
	if hellos := len(h.fake.Hellos()); hellos != 1 {
		t.Errorf("%d Hellos: the session did not survive the mesh failure", hellos)
	}
}

func TestSyncWholeDeltasPeersAndCache(t *testing.T) {
	h := newHarness(t)
	k1, k2 := newWGKey(t).PublicKey(), newWGKey(t).PublicKey()
	h.fake.PutEnv(testEnv("e1"))
	h.fake.SetMesh(wire.Mesh{Version: 3, Peers: []wire.Peer{
		{Name: "n1", PublicKey: k1[:], Endpoint: "10.250.0.2:51820", Prefix: "fd00:1:2:10::/64"},
		{Name: "n2", PublicKey: k2[:], Endpoint: "10.250.0.3:51820", Prefix: "fd00:1:2:11::/64"},
	}})
	ta := h.joined(nil)

	s, _ := ta.rec.lastState()
	if _, ok := s.Envs["e1"]; !ok || s.Mesh.Version != 3 || s.Self.Name != testName || s.Self.AppsZone != "apps.test" {
		t.Fatalf("state %+v", s)
	}
	peers, _ := ta.mesh.lastPeers()
	want := []wg.Peer{
		{PublicKey: k1, Endpoint: netip.MustParseAddrPort("10.250.0.2:51820"), AllowedIPs: []netip.Prefix{netip.MustParsePrefix("fd00:1:2:10::/64")}, Keepalive: 25 * time.Second},
		{PublicKey: k2, Endpoint: netip.MustParseAddrPort("10.250.0.3:51820"), AllowedIPs: []netip.Prefix{netip.MustParsePrefix("fd00:1:2:11::/64")}, Keepalive: 25 * time.Second},
	}
	if len(peers) != 2 || !peerEqual(peers[0], want[0]) || !peerEqual(peers[1], want[1]) {
		t.Errorf("peers\n got %+v\nwant %+v", peers, want)
	}

	// A delta puts an environment: OnState gets a copy, the cache follows.
	h.fake.PutEnv(testEnv("e2"))
	eventually(t, "the delta", func() bool {
		s, _ := ta.rec.lastState()
		_, ok := s.Envs["e2"]
		return ok && len(s.Envs) == 2
	})
	d := &stateDir{path: h.cfg.StateDir}
	eventually(t, "the cache", func() bool {
		c, _, err := d.loadTable()
		return err == nil && c != nil && len(c.Envs) == 2
	})
	// The copy handed over is the agent's no longer.
	s, _ = ta.rec.lastState()
	s.Envs["x"] = wire.Env{}
	if ta.Status().Envs != 2 {
		t.Error("OnState's state is shared with the agent")
	}
	// The same mesh version sets no peers again; a new one does.
	_, before := ta.mesh.lastPeers()
	h.fake.RemoveEnv("e1")
	eventually(t, "the removal", func() bool {
		s, _ := ta.rec.lastState()
		_, ok := s.Envs["e1"]
		return !ok
	})
	if _, after := ta.mesh.lastPeers(); after != before {
		t.Errorf("SetPeers called again without a new mesh version")
	}
	h.fake.SetMesh(wire.Mesh{Version: 4, Peers: []wire.Peer{{Name: "n1", PublicKey: k1[:], Endpoint: "10.250.0.2:51820", Prefix: "fd00:1:2:10::/64"}}})
	eventually(t, "the new peers", func() bool {
		p, n := ta.mesh.lastPeers()
		return n == before+1 && len(p) == 1
	})
	// A heartbeat changes nothing for OnState.
	_, calls := ta.rec.lastState()
	seq := ta.Status().TableSeq
	h.fake.Heartbeat()
	eventually(t, "the heartbeat", func() bool { return ta.Status().TableSeq == seq+1 })
	if _, n := ta.rec.lastState(); n != calls {
		t.Errorf("OnState called for a heartbeat")
	}
}

// A table in parts reaches OnState and the cache whole, once its last
// part arrived, and never a part alone: the whole table at the start,
// and a delta in parts later; the sequence goes on from the last part.
func TestSyncTableInParts(t *testing.T) {
	h := newHarness(t)
	h.fake.SetPartBytes(1) // every environment in a part of its own
	var envs, more []wire.Env
	for i := range 5 {
		envs = append(envs, testEnv(fmt.Sprintf("e%d", i)))
		more = append(more, testEnv(fmt.Sprintf("m%d", i)))
	}
	h.fake.PutEnv(envs...)
	ta := h.joined(nil)
	if seq := ta.Status().TableSeq; seq != 5 {
		t.Fatalf("seq %d after the whole table in 5 parts", seq)
	}
	h.fake.PutEnv(more...)
	eventually(t, "the delta", func() bool {
		s, _ := ta.rec.lastState()
		return len(s.Envs) == 10
	})
	if seq := ta.Status().TableSeq; seq != 10 {
		t.Fatalf("seq %d after the delta in 5 parts", seq)
	}
	ta.rec.mu.Lock()
	for i, s := range ta.rec.states {
		if len(s.Envs) != 5 && len(s.Envs) != 10 {
			t.Errorf("OnState %d got a part: %d environments", i, len(s.Envs))
		}
	}
	ta.rec.mu.Unlock()
	d := &stateDir{path: h.cfg.StateDir}
	eventually(t, "the cache", func() bool {
		c, _, err := d.loadTable()
		return err == nil && c != nil && len(c.Envs) == 10 && c.Seq == 10
	})
	h.fake.Heartbeat()
	eventually(t, "the heartbeat", func() bool { return ta.Status().TableSeq == 11 })
}

func peerEqual(a, b wg.Peer) bool {
	return a.PublicKey == b.PublicKey && a.Endpoint == b.Endpoint && a.Keepalive == b.Keepalive && slices.Equal(a.AllowedIPs, b.AllowedIPs)
}

func TestSyncGapReconnectsWhole(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)
	hellos := len(h.fake.Hellos())
	h.fake.SkipSeq()
	h.fake.PutEnv(testEnv("e1"))
	eventually(t, "a new connection with a whole table", func() bool {
		s, _ := ta.rec.lastState()
		_, ok := s.Envs["e1"]
		return ok && len(h.fake.Hellos()) > hellos && ta.Connected()
	})
	if !ta.logs.contains("out of sequence") {
		t.Error("the gap was not noticed")
	}
}

func TestServerCloseReconnects(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)
	for i := range 3 {
		hellos := len(h.fake.Hellos())
		h.fake.CloseStreams(nil)
		eventually(t, "a new connection", func() bool {
			return len(h.fake.Hellos()) > hellos && ta.Connected() && h.fake.Streams() == 1
		})
		if i == 1 {
			h.fake.CloseConns()
		}
	}
}

func TestControlPlaneListDeadFirst(t *testing.T) {
	h := newHarness(t)
	dead, live := deadAddr(t), h.fake.Addr()
	cfg := h.cfg
	cfg.ControlPlane = []string{dead, live}
	dr := &dialRecorder{}
	h.prepare = func(a *Agent) {
		a.deps.DialControlPlane = dr.dial
		// The dead address comes first in every attempt: an attempt that
		// gave up at it would never connect.
		a.order = func(addrs []string) []string { return slices.Clone(addrs) }
	}
	h.writeToken(h.mintToken())
	ta := h.start(cfg, nil)
	eventually(t, "connected", func() bool { return ta.Connected() && h.fake.Streams() == 1 })
	for range 4 {
		hellos := len(h.fake.Hellos())
		h.fake.CloseStreams(nil)
		eventually(t, "connected again", func() bool {
			return len(h.fake.Hellos()) > hellos && ta.Connected() && h.fake.Streams() == 1
		})
	}
	// The join and five connections: each attempt dialled the dead
	// address and went on to the live one.
	dials := dr.dials()
	if len(dials) < 12 || len(dials)%2 != 0 {
		t.Fatalf("dials %v", dials)
	}
	for i := 0; i < len(dials); i += 2 {
		if dials[i] != dead || dials[i+1] != live {
			t.Fatalf("attempt %d dialled %v; all dials %v", i/2, dials[i:i+2], dials)
		}
	}
}

func TestConnectTriesTheAddressesInRandomOrder(t *testing.T) {
	h := newHarness(t)
	live := h.fake.Addr()
	cfg := h.cfg
	cfg.ControlPlane = []string{deadAddr(t), deadAddr(t), live}
	dr := &dialRecorder{}
	h.prepare = func(a *Agent) { a.deps.DialControlPlane = dr.dial }
	h.writeToken(h.mintToken())
	ta := h.start(cfg, nil)
	eventually(t, "connected", ta.Connected)
	// Every attempt connects, wherever the live address falls in its
	// order, and dials it last.
	livePos := map[int]int{}
	for i := range 30 {
		dr.reset()
		s, err := ta.connect(context.Background())
		if err != nil {
			t.Fatalf("attempt %d: %v (dials %v)", i, err, dr.dials())
		}
		s.close()
		dials := dr.dials()
		if n := len(dials); n == 0 || dials[n-1] != live || slices.Contains(dials[:n-1], live) {
			t.Fatalf("attempt %d dialled %v", i, dials)
		}
		livePos[len(dials)-1]++
	}
	if len(livePos) < 2 {
		t.Errorf("the live address was always dialled at the same position %v: the order is not random", livePos)
	}
}

func TestRenewReconnectsWithTheNewCertificate(t *testing.T) {
	h := newHarness(t)
	h.fake.SetRenewAfter(300 * time.Millisecond)
	ta := h.joined(func(tm *timing) { tm.renewMinGap = time.Hour })
	first := ta.Status().CertSerial
	eventually(t, "a renewal", func() bool { return len(h.fake.Renewals()) > 0 })
	r := h.fake.Renewals()[0]
	if r.Name != testName || r.Old != first || r.New == "" || r.New == first {
		t.Fatalf("renewal %+v, first serial %s", r, first)
	}
	eventually(t, "a Hello with the new certificate, the old connection gone", func() bool {
		for _, hl := range h.fake.Hellos() {
			if hl.Serial == r.New {
				return ta.Status().CertSerial == r.New && ta.Connected() && h.fake.Streams() == 1
			}
		}
		return false
	})
	c, err := (&stateDir{path: h.cfg.StateDir}).loadCert()
	if err != nil || c == nil || certSerial(*c) != r.New {
		t.Errorf("cert.json holds %v, %v; want serial %s", c, err, r.New)
	}
	if n := len(h.fake.Renewals()); n != 1 {
		t.Errorf("%d renewals, want 1", n)
	}
}

func TestSingleRefusalKeepsTheCertificate(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)
	h.fake.Refuse(testName, "certificate 1 of ingress ing-0 was superseded; reconnect with the newest certificate")
	eventually(t, "refused attempts", func() bool { return ta.Status().Refusals >= 3 })
	if !fileExists(filepath.Join(h.cfg.StateDir, certFile)) {
		t.Fatal("cert.json is gone after refusals within the window")
	}
	if removed, _ := ta.IdentityRemoved(); removed || ta.Status().CertSerial == "" {
		t.Fatalf("status %+v", ta.Status())
	}
	h.fake.Unrefuse(testName)
	eventually(t, "connected again", ta.Connected)
	if st := ta.Status(); st.Refusals != 0 || st.RefusalsTotal < 3 || !strings.Contains(st.LastRefusal, "superseded") {
		t.Errorf("status %+v", st)
	}
	if n := len(h.fake.Joins()); n != 1 {
		t.Errorf("%d joins, want 1", n)
	}
}

func TestRemovedLosesTheIdentityAndRejoinsWithANewToken(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)
	keyBefore, err := os.ReadFile(filepath.Join(h.cfg.StateDir, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	const text = "ingress ing-0 was removed; join it again"
	h.fake.Refuse(testName, text)
	eventually(t, "removed", func() bool {
		removed, got := ta.IdentityRemoved()
		return removed && got == text
	})
	if fileExists(filepath.Join(h.cfg.StateDir, certFile)) {
		t.Error("cert.json is still there")
	}
	if lost := lostFiles(t, h.cfg.StateDir); len(lost) != 1 {
		t.Errorf("lost certificates %v", lost)
	}
	keyAfter, err := os.ReadFile(filepath.Join(h.cfg.StateDir, keyFile))
	if err != nil || string(keyAfter) == string(keyBefore) {
		t.Errorf("tls.key was not replaced (%v)", err)
	}
	if got, err := os.ReadFile(filepath.Join(h.cfg.StateDir, removedFile)); err != nil || string(got) != text {
		t.Errorf("the removal file holds %q, %v", got, err)
	}
	// The removal survives a restart.
	ta.stop()
	ta = h.start(h.cfg, nil)
	eventually(t, "removed after the restart", func() bool {
		removed, got := ta.IdentityRemoved()
		return removed && got == text && ta.Status().IdentityRemoved
	})
	eventually(t, "joining", func() bool { return ta.Status().Joining })
	// The token in the file was used: it is never sent again.
	time.Sleep(300 * time.Millisecond)
	if n := len(h.fake.Joins()); n != 1 || ta.Connected() {
		t.Fatalf("%d joins, connected %v", n, ta.Connected())
	}
	// The operator hands a new token.
	h.writeToken(h.mintToken())
	eventually(t, "joined again", ta.Connected)
	if removed, _ := ta.IdentityRemoved(); removed {
		t.Error("still removed after a join")
	}
	if fileExists(filepath.Join(h.cfg.StateDir, removedFile)) {
		t.Error("the removal file is still there after a join")
	}
	joins := h.fake.Joins()
	if len(joins) != 2 || joins[0].TokenHash == joins[1].TokenHash {
		t.Errorf("joins %+v", joins)
	}
	tried, _ := (&stateDir{path: h.cfg.StateDir}).loadTried()
	if len(tried) != 2 {
		t.Errorf("join-tried %v", tried)
	}
}

// Every join makes a new WireGuard key before the first Hello after it:
// a join again after a lost identity never brings back the key a copy of
// the state directory holds.
func TestJoinMakesANewMeshKey(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)
	first := h.fake.Hellos()[0].Request.PublicKey
	h.fake.Refuse(testName, "ingress ing-0 was removed; join it again")
	eventually(t, "removed", func() bool { removed, _ := ta.IdentityRemoved(); return removed })
	h.writeToken(h.mintToken())
	eventually(t, "joined again", ta.Connected)

	hellos := h.fake.Hellos()
	last := hellos[len(hellos)-1].Request.PublicKey
	key, ok, err := (&stateDir{path: h.cfg.StateDir}).loadWGKey()
	if err != nil || !ok {
		t.Fatalf("wg.key: %v %v", ok, err)
	}
	pub := key.PublicKey()
	if slices.Equal(last, first) || !slices.Equal(last, pub[:]) {
		t.Fatalf("the Hello after the join again sent %x; before %x, wg.key %x", last, first, pub[:])
	}
	if mc, _ := ta.mesh.lastConfig(); mc.PrivateKey != key {
		t.Error("the device does not have the new key")
	}
	if !ta.logs.contains("made a new WireGuard key") {
		t.Error("the new key was not logged")
	}
}

// A Hello refused for its key ("…; make a new one": the key the last join
// again retired, as after a crash between the join and the new key, or
// another peer's) makes a new key and connects with it.
func TestRetiredMeshKeyMakesANewOne(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)
	old := slices.Clone(h.fake.Hellos()[0].Request.PublicKey)
	h.fake.RetireKey(testName)
	h.fake.CloseConns()
	eventually(t, "a Hello with a new key", func() bool {
		hellos := h.fake.Hellos()
		last := hellos[len(hellos)-1].Request.PublicKey
		return !slices.Equal(last, old) && ta.Connected()
	})
	refused := false
	for _, hl := range h.fake.Hellos()[1:] {
		refused = refused || slices.Equal(hl.Request.PublicKey, old)
	}
	if !refused || !ta.logs.contains("refused the mesh key; made a new one") {
		t.Errorf("no refused Hello with the retired key: %+v", h.fake.Hellos())
	}
	if !fileExists(filepath.Join(h.cfg.StateDir, certFile)) || ta.Status().Refusals != 0 || len(h.fake.Joins()) != 1 {
		t.Error("a retired key cost the identity")
	}
}

func TestPersistentRefusalsLoseTheIdentity(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(func(tm *timing) { tm.refusalWindow = 300 * time.Millisecond })
	h.fake.Refuse(testName, "certificate 1 of ingress ing-0 was revoked (rejoined); join it again")
	eventually(t, "lost", func() bool {
		return !fileExists(filepath.Join(h.cfg.StateDir, certFile)) && ta.Status().Joining
	})
	if removed, _ := ta.IdentityRemoved(); removed {
		t.Error("a loss without a removal reported as removed")
	}
	if lost := lostFiles(t, h.cfg.StateDir); len(lost) != 1 {
		t.Errorf("lost certificates %v", lost)
	}
}

// runAndCache runs an agent until the table with env e1 and a mesh is
// cached, then stops it.
func runAndCache(t *testing.T, h *harness, peer wg.Key) {
	t.Helper()
	h.fake.PutEnv(testEnv("e1"))
	h.fake.SetMesh(wire.Mesh{Version: 7, Peers: []wire.Peer{{Name: "n1", PublicKey: peer[:], Endpoint: "10.250.0.2:51820", Prefix: "fd00:1:2:10::/64"}}})
	ta := h.joined(nil)
	d := &stateDir{path: h.cfg.StateDir}
	eventually(t, "the cache", func() bool {
		c, _, err := d.loadTable()
		return err == nil && c != nil && len(c.Envs) == 1 && c.Mesh.Version == 7
	})
	ta.stop()
}

func TestRestartServesTheCacheWithoutTheControlPlane(t *testing.T) {
	h := newHarness(t)
	peer := newWGKey(t).PublicKey()
	runAndCache(t, h, peer)
	_, addr, _ := h.fake.Place(testName)

	cfg := h.cfg
	cfg.ControlPlane = []string{deadAddr(t)}
	ta := h.start(cfg, nil)
	eventually(t, "the cached table", func() bool {
		s, _ := ta.rec.lastState()
		return s != nil
	})
	s, _ := ta.rec.lastState()
	if _, ok := s.Envs["e1"]; !ok || s.Mesh.Version != 7 {
		t.Fatalf("state %+v", s)
	}
	mc, n := ta.mesh.lastConfig()
	if n != 1 || mc.Address != netip.PrefixFrom(addr, 128) || mc.PodNetwork.String() != "fd00:1:2::/48" {
		t.Errorf("configure %+v (%d)", mc, n)
	}
	peers, _ := ta.mesh.lastPeers()
	if len(peers) != 1 || peers[0].PublicKey != peer {
		t.Errorf("peers %+v", peers)
	}
	if !ta.HasState() || !ta.MeshConfigured() || ta.Connected() {
		t.Errorf("hasState %v, meshConfigured %v, connected %v", ta.HasState(), ta.MeshConfigured(), ta.Connected())
	}
	if ta.Status().TableAt.IsZero() {
		t.Error("no table time from the cache")
	}
}

func TestHeartbeatsRefreshTheCache(t *testing.T) {
	h := newHarness(t)
	const refresh = 500 * time.Millisecond
	ta := h.joined(func(tm *timing) { tm.cacheRefresh = refresh })
	d := &stateDir{path: h.cfg.StateDir}
	var first time.Time
	eventually(t, "the cache", func() bool {
		s, at, err := d.loadTable()
		first = at
		return err == nil && s != nil
	})
	heartbeat := func() {
		t.Helper()
		seq := ta.Status().TableSeq
		h.fake.Heartbeat()
		eventually(t, "the heartbeat", func() bool { return ta.Status().TableSeq == seq+1 })
	}
	// A heartbeat within cacheRefresh of the last write leaves it alone.
	heartbeat()
	time.Sleep(100 * time.Millisecond)
	if time.Since(first) < refresh-100*time.Millisecond {
		if _, at, err := d.loadTable(); err != nil || !at.Equal(first) {
			t.Errorf("table.json saved at %v (%v) after a heartbeat within the interval; first at %v", at, err, first)
		}
	}
	// One after it writes the cache, though nothing changed.
	time.Sleep(time.Until(first.Add(refresh + 50*time.Millisecond)))
	heartbeat()
	eventually(t, "the cache refreshed", func() bool {
		_, at, err := d.loadTable()
		return err == nil && at.After(first)
	})
}

func TestOldCacheWarnsOnlyWhenTheControlPlaneIsOutOfReach(t *testing.T) {
	h := newHarness(t)
	runAndCache(t, h, newWGKey(t).PublicKey())

	// The control plane answers: no warning, however old the cache.
	ageCache(t, h.cfg.StateDir, 2*time.Hour)
	ta := h.start(h.cfg, nil)
	eventually(t, "connected with a fresh table", func() bool {
		return ta.Connected() && time.Since(ta.Status().TableAt) < time.Minute
	})
	time.Sleep(100 * time.Millisecond) // several mesh polls
	if ta.logs.contains("the table is old") {
		t.Error("an old cache warned before the control plane was out of reach")
	}
	ta.stop()

	// Out of reach, it warns.
	ageCache(t, h.cfg.StateDir, 2*time.Hour)
	cfg := h.cfg
	cfg.ControlPlane = []string{deadAddr(t)}
	tb := h.start(cfg, nil)
	eventually(t, "the warning", func() bool { return tb.logs.contains("the table is old") })
	if age := time.Since(tb.Status().TableAt); age < 2*time.Hour {
		t.Errorf("table age %v, want the cache's 2 h", age)
	}
}

func TestFixedMeshKeyKeepsServingTheCache(t *testing.T) {
	h := newHarness(t)
	runAndCache(t, h, newWGKey(t).PublicKey())
	hellos := len(h.fake.Hellos())
	// wg.key changes without a re-join.
	nk := newWGKey(t)
	if err := os.WriteFile(filepath.Join(h.cfg.StateDir, wgKeyFile), nk[:], 0o600); err != nil {
		t.Fatal(err)
	}
	ta := h.start(h.cfg, nil)
	eventually(t, "Hello retried", func() bool { return len(h.fake.Hellos()) >= hellos+3 })
	if !ta.logs.contains("wg.key changed without a re-join") {
		t.Error("the fixed key was not reported")
	}
	s, _ := ta.rec.lastState()
	if s == nil || len(s.Envs) != 1 || ta.Connected() || !ta.HasState() {
		t.Fatalf("state %v, connected %v", s, ta.Connected())
	}
	if !strings.Contains(ta.Status().LastError, "is fixed") {
		t.Errorf("last error %q", ta.Status().LastError)
	}
	if !fileExists(filepath.Join(h.cfg.StateDir, certFile)) || ta.Status().Refusals != 0 {
		t.Error("a fixed key counted as a refusal")
	}
}

func TestWakeIsDeduplicated(t *testing.T) {
	h := newHarness(t)
	h.fake.SetWakeDelay(100 * time.Millisecond)
	ta := h.joined(func(tm *timing) { tm.wakeEvery = 500 * time.Millisecond })
	start := time.Now()
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ta.Wake("e1", "web")
		}()
	}
	wg.Wait()
	ta.Wake("e1", "api")
	eventually(t, "the wakes", func() bool { return len(h.fake.Wakes()) == 2 })
	time.Sleep(200 * time.Millisecond)
	// In flight or sent within wakeEvery: not sent again.
	ta.Wake("e1", "web")
	if time.Since(start) < 450*time.Millisecond {
		time.Sleep(100 * time.Millisecond)
		if n := len(h.fake.Wakes()); n != 2 {
			t.Fatalf("%d wakes within the interval, want 2", n)
		}
	}
	time.Sleep(time.Until(start.Add(600 * time.Millisecond)))
	ta.Wake("e1", "web")
	eventually(t, "the wake again", func() bool { return len(h.fake.Wakes()) == 3 })
	wakes := h.fake.Wakes()
	if wakes[2] != (wire.WakeRequest{Env: "e1", Service: "web"}) {
		t.Errorf("wakes %+v", wakes)
	}
}

func TestWakeAndHoldWithoutConnection(t *testing.T) {
	h := newHarness(t)
	cfg := h.cfg
	cfg.ControlPlane = []string{deadAddr(t)}
	ta := h.start(cfg, nil)
	ta.Wake("e1", "web") // dropped
	if _, err := ta.Hold(context.Background(), wire.HoldRequest{Hold: []string{"a"}}); !errors.Is(err, ErrNotConnected) {
		t.Errorf("Hold: %v, want ErrNotConnected", err)
	}
	if err := ta.Leave(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Errorf("Leave: %v, want ErrNotConnected", err)
	}
	if n := len(h.fake.Wakes()); n != 0 {
		t.Errorf("%d wakes", n)
	}
}

func TestHoldAndLeave(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)
	h.fake.RefuseHold("b", "busy")
	resp, err := ta.Hold(context.Background(), wire.HoldRequest{Hold: []string{"a", "b"}, Release: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resp.Held, []string{"a"}) || len(resp.Refused) != 1 || resp.Refused[0] != (wire.HoldRefusal{Instance: "b", Reason: "busy"}) || resp.LeaseSeconds != 30 {
		t.Errorf("answer %+v", resp)
	}
	if err := ta.Leave(context.Background()); err != nil {
		t.Fatal(err)
	}
	holds := h.fake.Holds()
	if len(holds) != 2 || holds[0].Name != testName || !slices.Equal(holds[0].Request.Release, []string{"c"}) || !holds[1].Request.Leaving {
		t.Errorf("holds %+v", holds)
	}
}

func TestMeshStatusReportsHandshakesByNode(t *testing.T) {
	h := newHarness(t)
	k1, k2, k3 := newWGKey(t).PublicKey(), newWGKey(t).PublicKey(), newWGKey(t).PublicKey()
	h.fake.SetMesh(wire.Mesh{Version: 1, Peers: []wire.Peer{
		{Name: "n1", PublicKey: k1[:], Endpoint: "10.250.0.2:51820", Prefix: "fd00:1:2:10::/64"},
		{Name: "n2", PublicKey: k2[:], Endpoint: "10.250.0.3:51820", Prefix: "fd00:1:2:11::/64"},
	}})
	ta := h.joined(nil)
	t1 := time.Now().Add(-7 * time.Second).Round(0)
	ta.mesh.setStatus(wg.Device{Name: DefaultMeshDevice, Peers: []wg.PeerState{
		{Peer: wg.Peer{PublicKey: k1, Endpoint: netip.MustParseAddrPort("10.250.0.2:51820")}, LastHandshake: t1},
		{Peer: wg.Peer{PublicKey: k2}},
		{Peer: wg.Peer{PublicKey: k3}, LastHandshake: t1}, // not a node of the table
	}})
	eventually(t, "the handshakes", func() bool {
		m := ta.rec.lastMesh()
		return len(m) == 2 && m["n1"].Equal(t1) && m["n2"].IsZero()
	})
	peers := ta.Status().MeshPeers
	if len(peers) != 2 || peers[0].Node != "n1" || peers[0].Endpoint != "10.250.0.2:51820" || !peers[0].LastHandshake.Equal(t1) || peers[1].Node != "n2" {
		t.Errorf("status peers %+v", peers)
	}
}

func TestRefusalPolicy(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	const other = "certificate 1 of ingress ing-0 was revoked (rejoined); join it again"

	r := newRefusals(5*time.Minute, 3)
	r.observe(other, at(0))
	if lost, _ := r.lost(); lost {
		t.Fatal("one refusal lost the identity")
	}
	r.observe(other, at(time.Minute))
	r.observe(other, at(2*time.Minute))
	if lost, _ := r.lost(); lost {
		t.Fatal("three refusals within the window lost the identity")
	}
	r.observe(other, at(5*time.Minute))
	if lost, removed := r.lost(); !lost || removed {
		t.Fatalf("lost %v removed %v after 4 refusals over 5 minutes", lost, removed)
	}

	// Two attempts are not enough, however long apart.
	r = newRefusals(5*time.Minute, 3)
	r.observe(other, at(0))
	r.observe(other, at(time.Hour))
	if lost, _ := r.lost(); lost {
		t.Fatal("two refusals lost the identity")
	}

	// A successful Hello starts over.
	r = newRefusals(5*time.Minute, 3)
	r.observe(other, at(0))
	r.observe(other, at(2*time.Minute))
	r.connected(at(3 * time.Minute))
	r.observe(other, at(6*time.Minute))
	r.observe(other, at(7*time.Minute))
	r.observe(other, at(8*time.Minute))
	if lost, _ := r.lost(); lost {
		t.Fatal("refusals before a successful Hello counted")
	}
	r.observe(other, at(11*time.Minute))
	if lost, _ := r.lost(); !lost {
		t.Fatal("persistent refusals after a Hello did not lose the identity")
	}
	if r.total != 6 || r.lastText != other || !r.lastAt.Equal(at(11*time.Minute)) {
		t.Errorf("total %d, last %q at %v", r.total, r.lastText, r.lastAt)
	}
	r.reset()
	if lost, _ := r.lost(); lost || r.count != 0 || r.total != 6 {
		t.Errorf("after reset: lost %v, count %d, total %d", lost, r.count, r.total)
	}

	// A removal loses it at once.
	r = newRefusals(5*time.Minute, 3)
	r.observe("ingress ing-0 was removed; join it again", at(0))
	if lost, removed := r.lost(); !lost || !removed {
		t.Fatalf("lost %v removed %v after a removal", lost, removed)
	}
}

func TestComputeMTU(t *testing.T) {
	for in, want := range map[int]int{1500: 1420, 9000: 1420, 1450: 1370, 1360: 1280, 1280: 1280, 0: 1280} {
		if got := ComputeMTU(in); got != want {
			t.Errorf("ComputeMTU(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestWGPeers(t *testing.T) {
	k1, k2 := newWGKey(t).PublicKey(), newWGKey(t).PublicKey()
	peers, problems := wgPeers(wire.Mesh{Version: 1, Peers: []wire.Peer{
		{Name: "n1", PublicKey: k1[:], Endpoint: "[2001:db8::1]:51820", Prefix: "fd00:1:2:10::1/64"},
		{Name: "short", PublicKey: []byte{1, 2}, Endpoint: "10.0.0.1:51820", Prefix: "fd00:1:2:11::/64"},
		{Name: "dup", PublicKey: k1[:], Endpoint: "10.0.0.1:51820", Prefix: "fd00:1:2:12::/64"},
		{Name: "host", PublicKey: k2[:], Endpoint: "node:51820", Prefix: "fd00:1:2:13::/64"},
		{Name: "noprefix", PublicKey: k2[:], Endpoint: "10.0.0.1:51820", Prefix: "x"},
		{Name: "n2", PublicKey: k2[:], Prefix: "fd00:1:2:14::/64"},
	}})
	if len(problems) != 4 {
		t.Errorf("problems %q", problems)
	}
	want := []wg.Peer{
		{PublicKey: k1, Endpoint: netip.MustParseAddrPort("[2001:db8::1]:51820"), AllowedIPs: []netip.Prefix{netip.MustParsePrefix("fd00:1:2:10::/64")}, Keepalive: 25 * time.Second},
		{PublicKey: k2, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("fd00:1:2:14::/64")}, Keepalive: 25 * time.Second},
	}
	if len(peers) != len(want) {
		t.Fatalf("peers %+v", peers)
	}
	for i := range want {
		if !peerEqual(peers[i], want[i]) {
			t.Errorf("peer %d\n got %+v\nwant %+v", i, peers[i], want[i])
		}
	}
}

func TestNewChecksTheConfig(t *testing.T) {
	good := Config{
		Name: testName, StateDir: t.TempDir(), ControlPlane: []string{"127.0.0.1:7443"}, ControlPlaneName: "ferun.ingress.test",
		TrustDomain: "ingress.test", MeshEndpoint: "10.0.0.5:51820",
	}
	if _, err := New(good, Deps{Mesh: &fakeMesh{}}); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config, *Deps){
		"name":         func(c *Config, _ *Deps) { c.Name = "Not_A_Label" },
		"state dir":    func(c *Config, _ *Deps) { c.StateDir = "" },
		"no addresses": func(c *Config, _ *Deps) { c.ControlPlane = nil },
		"address":      func(c *Config, _ *Deps) { c.ControlPlane = []string{"nohost"} },
		"cp name":      func(c *Config, _ *Deps) { c.ControlPlaneName = "" },
		"trust domain": func(c *Config, _ *Deps) { c.TrustDomain = "" },
		"endpoint":     func(c *Config, _ *Deps) { c.MeshEndpoint = "node:51820" },
		"wildcard":     func(c *Config, _ *Deps) { c.MeshEndpoint = "0.0.0.0:51820" },
		"mtu":          func(c *Config, _ *Deps) { c.MeshMTU = 1000 },
		"port":         func(c *Config, _ *Deps) { c.MeshPort = 70000 },
		"no mesh":      func(_ *Config, d *Deps) { d.Mesh = nil },
	} {
		c, d := good, Deps{Mesh: &fakeMesh{}}
		mod(&c, &d)
		if _, err := New(c, d); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
