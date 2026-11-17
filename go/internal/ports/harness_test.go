// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ports

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	portforwardv1 "github.com/gardenlinux/feos/go/gen/feos/portforward/v1"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/tools/toolstest"
)

// The guest ports of the tests: guestPort is served by the world's guest
// server; the others are mapped by the tests that need them.
const guestPort = 5173

// engine is the door's engine in the tests: toolstest's fake (it brings
// the pod up on the test node and counts its holds), with Ensure, Touch
// and a gate on Acquire.
type engine struct {
	*toolstest.Engine

	mu      sync.Mutex
	touches int
	ensures int
	// gate, when set, runs at the start of every Acquire with its context.
	gate func(ctx context.Context)
}

func (e *engine) Acquire(ctx context.Context, id podid.ID, kind model.HoldKind) (model.Lease, func(), error) {
	e.mu.Lock()
	gate := e.gate
	e.mu.Unlock()
	if gate != nil {
		gate(ctx)
	}
	if kind != model.HoldForward {
		return model.Lease{}, nil, errors.New("the door took a hold that is not a forward hold")
	}
	return e.Engine.Acquire(ctx, id, kind)
}

func (e *engine) Ensure(ctx context.Context, id podid.ID) (model.Lease, error) {
	e.mu.Lock()
	e.ensures++
	e.mu.Unlock()
	lease, release, err := e.Engine.Acquire(ctx, id, model.HoldExec)
	if err == nil {
		release()
	}
	return lease, err
}

func (e *engine) Touch(podid.ID) {
	e.mu.Lock()
	e.touches++
	e.mu.Unlock()
}

func (e *engine) counts() (touches, ensures int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.touches, e.ensures
}

// kubePorts is the bindings' answer in the tests.
type kubePorts struct {
	mu    sync.Mutex
	ports []int
	err   error
}

func (k *kubePorts) KubePorts(context.Context, podid.ID) ([]int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.ports, k.err
}

// seen is what the guest's server saw of a request.
type seen struct {
	Method, Host, Path, Query, Authorization, ProxyAuthorization, Body string
	// RouteToken is the route door's own header; GuestHost the header
	// that names the Host, which stops at the door.
	RouteToken, GuestHost string
}

// guest is the HTTP server on the guest's port: it answers what it saw,
// holds requests to /hold until released, upgrades /ws to an echo, and
// counts its connections and requests in flight.
type guest struct {
	srv *httptest.Server

	delay    atomic.Int64 // ns every request waits first
	inFlight atomic.Int64
	maxIn    atomic.Int64
	open     atomic.Int64 // connections open
	opened   atomic.Int64 // connections ever opened
	release  chan struct{}

	mu   sync.Mutex
	last seen
}

func newGuest(t *testing.T) *guest {
	g := &guest{release: make(chan struct{})}
	g.srv = httptest.NewUnstartedServer(http.HandlerFunc(g.serve))
	g.srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		switch st {
		case http.StateNew:
			g.open.Add(1)
			g.opened.Add(1)
		case http.StateClosed, http.StateHijacked:
			g.open.Add(-1)
		}
	}
	g.srv.Start()
	t.Cleanup(g.srv.Close)
	return g
}

func (g *guest) addr() string { return g.srv.Listener.Addr().String() }

func (g *guest) serve(w http.ResponseWriter, r *http.Request) {
	n := g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	for {
		m := g.maxIn.Load()
		if n <= m || g.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	body, _ := io.ReadAll(r.Body)
	s := seen{Method: r.Method, Host: r.Host, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
		Authorization: r.Header.Get("Authorization"), ProxyAuthorization: r.Header.Get("Proxy-Authorization"),
		Body: string(body), RouteToken: r.Header.Get("X-FeRun-Route-Token"), GuestHost: r.Header.Get(HostHeader)}
	g.mu.Lock()
	g.last = s
	g.mu.Unlock()
	if d := g.delay.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
	switch r.URL.Path {
	case "/hold":
		select {
		case <-g.release:
		case <-r.Context().Done():
			return
		}
	case "/ws":
		g.echo(w, r)
		return
	case "/switch":
		g.switchAnyway(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s)
}

// echo switches to a protocol that sends back what it gets.
func (g *guest) echo(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "upgrade required", http.StatusUpgradeRequired)
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	if brw.Flush() != nil {
		return
	}
	_, _ = io.Copy(conn, brw.Reader)
}

// switchAnyway switches to a protocol nobody asked for, whatever the
// request, and then echoes: a 101 the proxy refuses.
func (g *guest) switchAnyway(w http.ResponseWriter) {
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: foo\r\nConnection: Upgrade\r\n\r\n")
	if brw.Flush() != nil {
		return
	}
	_, _ = io.Copy(conn, brw.Reader)
}

func (g *guest) lastSeen() seen {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

// world is a test's port door: a test node with a running pod, the door
// in front of it on an HTTP server with the forward routes, and the guest
// server on the pod's port guestPort.
type world struct {
	t     *testing.T
	env   *toolstest.Env
	eng   *engine
	kube  *kubePorts
	h     *Handler
	front *httptest.Server
	guest *guest
	sb    auth.Sandbox
	token string
	// forwards counts the Forward streams the node opened.
	forwards atomic.Int64
	// failLookup makes the token lookup fail.
	failLookup atomic.Bool
	// failTenantLookup makes the lookup of a tenant's token fail.
	failTenantLookup atomic.Bool
}

func newWorld(t *testing.T, tune ...func(*timings)) *world {
	t.Helper()
	env := toolstest.New(t, toolstest.Options{PendingDelay: 20 * time.Millisecond})
	w := &world{t: t, env: env, eng: &engine{Engine: env.Engine}, kube: &kubePorts{}}
	w.sb = env.NewSandbox()
	w.token = "token-of-" + w.sb.ID.String()
	other := env.NewSandbox()
	resolve := func(_ context.Context, bearer string) (auth.Sandbox, error) {
		switch {
		case w.failLookup.Load():
			return auth.Sandbox{}, errors.New("the database is gone")
		case bearer == w.token:
			return w.sb, nil
		case bearer == "token-of-other":
			return other, nil
		}
		return auth.Sandbox{}, model.ErrUnknownToken
	}
	w.h = New(w.eng, env.Port, w.kube, resolve, env.DB, env.Log)
	tenantOf := w.h.tenantOf
	w.h.tenantOf = func(ctx context.Context, hash string) (string, error) {
		if w.failTenantLookup.Load() {
			return "", errors.New("the database is gone")
		}
		return tenantOf(ctx, hash)
	}
	for _, f := range tune {
		f(&w.h.t)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.h.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	mux := http.NewServeMux()
	for _, p := range []string{"/fwd/{id}/{port}/{rest...}", "/fwd/{id}/{port}",
		"/api/v1/sandboxes/{id}/ports/{port}/{rest...}", "/api/v1/sandboxes/{id}/ports/{port}"} {
		mux.Handle(p, w.h)
	}
	w.front = httptest.NewServer(mux)
	t.Cleanup(w.front.Close)

	w.guest = newGuest(t)
	env.Node.Script().SetPortAddr(guestPort, w.guest.addr())
	env.Node.Script().SetHook(portforwardv1.PortForwardService_Forward_FullMethodName,
		func(context.Context, string, any) error {
			w.forwards.Add(1)
			return nil
		})
	// The pod runs before the first request.
	lease, release, err := env.Engine.Acquire(context.Background(), w.sb.ID, model.HoldExec)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if lease.Node != toolstest.NodeName {
		t.Fatalf("lease %+v", lease)
	}
	return w
}

// url is the front door's URL of the sandbox's port and path.
func (w *world) url(port int, path string) string {
	return w.front.URL + "/fwd/" + w.sb.ID.String() + "/" + strconv.Itoa(port) + path
}

// do sends a request with the sandbox's token unless the header says
// otherwise, and returns the status, the Retry-After and the body.
func (w *world) do(method, url string, header http.Header, body string) (int, string, string) {
	w.t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	if body == "" {
		req.Body = http.NoBody
		req.ContentLength = 0
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	for k, vs := range header {
		req.Header[k] = vs
	}
	if h := header.Get("Host"); h != "" {
		req.Host = h
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Retry-After"), string(b)
}

// noHoldsLeft waits until every hold of the sandbox is released: a client
// may read the end of an answer before its handler has released its hold.
func (w *world) noHoldsLeft() {
	w.t.Helper()
	eventually(w.t, 5*time.Second, "the holds released", func() bool { return w.eng.Holds(w.sb.ID) == 0 })
}

// eventually waits up to within for cond.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rawUpgrade opens a raw connection to the front door and asks for an
// upgrade of path; it returns the connection after the 101.
func (w *world) rawUpgrade(path string) (net.Conn, *bufio.Reader) {
	w.t.Helper()
	conn, err := net.Dial("tcp", w.front.Listener.Addr().String())
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { conn.Close() })
	req := "GET /fwd/" + w.sb.ID.String() + "/" + strconv.Itoa(guestPort) + path + " HTTP/1.1\r\n" +
		"Host: 5173--preview.example.test\r\nAuthorization: Bearer " + w.token + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		w.t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(resp.Body)
		w.t.Fatalf("upgrade: %d %s", resp.StatusCode, b)
	}
	return conn, br
}
