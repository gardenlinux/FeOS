// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

const (
	testZone = "apps.example"
	testHost = "app.apps.example"
	testEnv  = "8a6b3c62-6d4f-4b55-9a49-3a1c2c1b0f01"
)

// fakeNet maps fake endpoint addresses ("[fd00::1]:8080") to real
// listeners, counts the dials per address and fails those it is told to.
type fakeNet struct {
	mu     sync.Mutex
	routes map[string]string
	fail   map[string]error
	dials  map[string]int
}

func newFakeNet() *fakeNet {
	return &fakeNet{routes: map[string]string{}, fail: map[string]error{}, dials: map[string]int{}}
}

func (f *fakeNet) route(addr, real string) {
	f.mu.Lock()
	f.routes[addr] = real
	f.mu.Unlock()
}

func (f *fakeNet) failDial(addr string, err error) {
	f.mu.Lock()
	f.fail[addr] = err
	f.mu.Unlock()
}

func (f *fakeNet) dialed(addr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials[addr]
}

func (f *fakeNet) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.dials[addr]++
	real, ok := f.routes[addr]
	err := f.fail[addr]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no route to %s", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", real)
}

// echo is what the echo upstream answers: what it got.
type echo struct {
	Upstream   string            `json:"upstream"`
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	RequestURI string            `json:"requestURI"`
	Query      string            `json:"query"`
	Host       string            `json:"host"`
	Body       string            `json:"body"`
	Headers    map[string]string `json:"headers"`
}

// upstream starts an echo server named name and routes the endpoint
// address addr (port 8080) to it.
func upstream(t *testing.T, fn *fakeNet, name, addr string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e := echo{Upstream: name, Method: r.Method, Path: r.URL.Path, RequestURI: r.RequestURI,
			Query: r.URL.RawQuery, Host: r.Host, Body: string(b), Headers: map[string]string{}}
		for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded",
			"X-Real-Ip", "True-Client-Ip", "Traceparent", "Tracestate"} {
			e.Headers[h] = strings.Join(r.Header.Values(h), ",")
		}
		_ = json.NewEncoder(w).Encode(e)
	}))
	t.Cleanup(srv.Close)
	fn.route(net.JoinHostPort(addr, "8080"), srv.Listener.Addr().String())
	return srv
}

// handlerAt routes the endpoint address addr (port 8080) to a server
// with handler h.
func handlerAt(t *testing.T, fn *fakeNet, addr string, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	fn.route(net.JoinHostPort(addr, "8080"), srv.Listener.Addr().String())
	return srv
}

func ep(instance, addr, node string) wire.Endpoint {
	return wire.Endpoint{Instance: instance, Address: addr, Node: node, Ports: map[string]int{"http": 8080}}
}

func svc(name string, eps ...wire.Endpoint) wire.Service {
	return wire.Service{Name: name, Endpoints: eps}
}

// testState is one environment at testHost with a root route to web, a
// stripped /api/ route to api and a door route /dev, plus services.
func testState(services ...wire.Service) *wire.State {
	return stateWith(wire.Self{Name: "ingress-0", AppsZone: testZone}, []string{testHost}, services...)
}

func stateWith(self wire.Self, hosts []string, services ...wire.Service) *wire.State {
	s := wire.NewState()
	s.Self = self
	s.Envs[testEnv] = wire.Env{
		ID: testEnv, Tenant: "acme", Name: "app", Hosts: hosts, Deployment: 1,
		Routes: []wire.Route{
			{Name: "root", Path: "/", Service: "web", Port: "http"},
			{Name: "api", Path: "/api/", Service: "api", Port: "http", Strip: true},
			{Name: "dev", Path: "/dev", Service: "dev", Port: "http", Door: true},
		},
		Services: services,
	}
	return s
}

type fakeWaker struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeWaker) Wake(env, service string) {
	f.mu.Lock()
	f.calls = append(f.calls, env+"/"+service)
	f.mu.Unlock()
}

func (f *fakeWaker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newTestProxy returns a proxy on fn's dials with every node in fresh
// fresh, and a server for its HTTPS handler (plain HTTP).
func newTestProxy(t *testing.T, cfg Config, d Deps, fn *fakeNet, fresh ...string) (*Proxy, *httptest.Server) {
	t.Helper()
	d.Dial = fn.dial
	p := New(cfg, d)
	m := map[string]bool{}
	for _, n := range fresh {
		m[n] = true
	}
	p.SetMeshHealth(m)
	srv := httptest.NewServer(p.ServeHTTPS())
	t.Cleanup(func() {
		srv.Close()
		p.CloseUpstreams()
	})
	return p, srv
}

type result struct {
	status int
	header http.Header
	body   string
	err    error
}

func send(ctx context.Context, srv *httptest.Server, method, host, path string, body io.Reader) result {
	req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, body)
	if err != nil {
		return result{err: err}
	}
	req.Host = host
	res, err := srv.Client().Do(req)
	if err != nil {
		return result{err: err}
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return result{status: res.StatusCode, header: res.Header, body: string(b), err: err}
}

func get(t *testing.T, srv *httptest.Server, host, path string) result {
	t.Helper()
	r := send(context.Background(), srv, "GET", host, path, nil)
	if r.err != nil {
		t.Fatalf("GET %s%s: %v", host, path, r.err)
	}
	return r
}

func decode(t *testing.T, r result) echo {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	var e echo
	if err := json.Unmarshal([]byte(r.body), &e); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return e
}

func wantPlain(t *testing.T, r result, status int, text string) {
	t.Helper()
	if r.err != nil || r.status != status || r.body != text+"\n" {
		t.Fatalf("got %d %q (%v), want %d %q", r.status, r.body, r.err, status, text)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRouting(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	upstream(t, fn, "api", "fd00::2")
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	wantPlain(t, get(t, srv, testHost, "/"), 503, textNoTable)
	if p.Ready() == nil {
		t.Error("ready without a table")
	}
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1")), svc("api", ep("a1", "fd00::2", "n1"))))
	if err := p.Ready(); err != nil {
		t.Errorf("not ready: %v", err)
	}
	for _, c := range []struct {
		host, path              string
		upstream, uri, rawQuery string
	}{
		{testHost, "/x?q=1&r=a%20b", "web", "/x?q=1&r=a%20b", "q=1&r=a%20b"},
		{testHost, "/", "web", "/", ""},
		{testHost, "/apis", "web", "/apis", ""},
		{testHost, "/api", "api", "/", ""},
		{testHost, "/api/", "api", "/", ""},
		{testHost, "/api/v1/items?x=y", "api", "/v1/items?x=y", "x=y"},
		{testHost, "/api/a%2Fb", "api", "/a%2Fb", ""},
		{testHost, "/api/a%20b", "api", "/a%20b", ""},
		{"APP.apps.example.:443", "/q", "web", "/q", ""},
		{"app.apps.example:8443", "/q", "web", "/q", ""},
	} {
		e := decode(t, get(t, srv, c.host, c.path))
		if e.Upstream != c.upstream || e.RequestURI != c.uri || e.Query != c.rawQuery {
			t.Errorf("%s%s: %s %s %q, want %s %s %q", c.host, c.path, e.Upstream, e.RequestURI, e.Query,
				c.upstream, c.uri, c.rawQuery)
		}
		if e.Host != c.host {
			t.Errorf("%s%s: the upstream got host %q", c.host, c.path, e.Host)
		}
	}
	wantPlain(t, get(t, srv, testHost, "/a/../b"), 400, textDotDot)
	wantPlain(t, get(t, srv, testHost, "/a/%2e%2e/b"), 400, textDotDot)
	wantPlain(t, get(t, srv, testHost, "/a/%2e/b"), 400, textDot)
	wantPlain(t, get(t, srv, testHost, "/dev/x"), 404, "route /dev is served by the route door")
	wantPlain(t, get(t, srv, "other.apps.example", "/"), 404, textNoHost)
	wantPlain(t, get(t, srv, "", "/"), 404, textNoHost)
	if fn.dialed("[fd00::1]:8080") == 0 || fn.dialed("[fd00::2]:8080") == 0 {
		t.Error("no dials")
	}
	// No root route: a path outside every route has none.
	s := testState(svc("api", ep("a1", "fd00::2", "n1")))
	e := s.Envs[testEnv]
	e.Routes = e.Routes[1:]
	s.Envs[testEnv] = e
	p.SetState(s)
	wantPlain(t, get(t, srv, testHost, "/web/a%20b"), 404, "no route for /web/a b")
	// A routed service missing from the services: no instance.
	r := get(t, srv, testHost, "/api/x")
	if r.status != 200 {
		t.Errorf("api: %d", r.status)
	}
	p.SetState(testState())
	r = get(t, srv, testHost, "/api/x")
	wantPlain(t, r, 503, "no ready instance of api")
	if r.header.Get("Retry-After") != "5" {
		t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
	}
	r = send(context.Background(), srv, "CONNECT", testHost, "/", nil)
	if r.status != http.StatusMethodNotAllowed {
		t.Errorf("CONNECT: %d", r.status)
	}
}

// The upstream gets X-Forwarded-For, -Proto and -Host and X-Real-IP from
// the proxy alone, no True-Client-IP, and the client's Host.
func TestForwardedHeaders(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	p := New(Config{}, Deps{Dial: fn.dial})
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	srv := httptest.NewUnstartedServer(p.ServeHTTPS())
	srv.StartTLS()
	defer srv.Close()
	req, err := http.NewRequest("GET", srv.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testHost
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("Forwarded", "for=203.0.113.9")
	req.Header.Set("X-Real-IP", "203.0.113.9")
	req.Header.Set("True-Client-IP", "203.0.113.9")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	e := decode(t, result{status: res.StatusCode, body: string(b)})
	want := map[string]string{"X-Forwarded-For": "127.0.0.1", "X-Forwarded-Host": testHost,
		"X-Forwarded-Proto": "https", "Forwarded": "", "X-Real-Ip": "127.0.0.1", "True-Client-Ip": ""}
	for h, v := range want {
		if e.Headers[h] != v {
			t.Errorf("%s: %q, want %q", h, e.Headers[h], v)
		}
	}
	if e.Host != testHost {
		t.Errorf("host %q", e.Host)
	}
}

func TestRoundRobin(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "one", "fd00::1")
	upstream(t, fn, "two", "fd00::2")
	upstream(t, fn, "three", "fd00::3")
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1", "n2", "n3")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"), ep("w2", "fd00::2", "n2"), ep("w3", "fd00::3", "n3"))))
	got := map[string]int{}
	for range 9 {
		got[decode(t, get(t, srv, testHost, "/")).Upstream]++
	}
	if got["one"] != 3 || got["two"] != 3 || got["three"] != 3 {
		t.Errorf("spread %v", got)
	}
}

// A dial that fails sends any request, body and all, to another
// endpoint; the failed endpoint cools down.
func TestRetryDialFailure(t *testing.T) {
	fn := newFakeNet()
	fn.failDial("[fd00::1]:8080", errors.New("no route to host"))
	upstream(t, fn, "two", "fd00::2")
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1", "n2")
	p.cool = 300 * time.Millisecond
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"), ep("w2", "fd00::2", "n2"))))
	payload := strings.Repeat("payload ", 1000)
	r := send(context.Background(), srv, "POST", testHost, "/submit", strings.NewReader(payload))
	e := decode(t, r)
	if e.Upstream != "two" || e.Body != payload || e.Method != "POST" {
		t.Fatalf("got %s %s with %d bytes", e.Upstream, e.Method, len(e.Body))
	}
	// A chunked body as well.
	r = send(context.Background(), srv, "PUT", testHost, "/submit", struct{ io.Reader }{strings.NewReader(payload)})
	if e := decode(t, r); e.Body != payload {
		t.Fatalf("chunked body: %d bytes", len(e.Body))
	}
	if n := fn.dialed("[fd00::1]:8080"); n != 1 {
		t.Errorf("the failed endpoint was dialed %d times while it cooled down", n)
	}
	if p.m.retries.Load() != 1 || p.m.dialErrors.Load() != 1 {
		t.Errorf("retries %d, dial errors %d", p.m.retries.Load(), p.m.dialErrors.Load())
	}
	// After the cooldown, it is tried again.
	time.Sleep(400 * time.Millisecond)
	for range 2 {
		decode(t, get(t, srv, testHost, "/"))
	}
	if n := fn.dialed("[fd00::1]:8080"); n != 2 {
		t.Errorf("after the cooldown the failed endpoint was dialed %d times, want 2", n)
	}
	// With no other endpoint, the failure is the answer.
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	wantPlain(t, get(t, srv, testHost, "/"), 503, "no ready instance of web")
	time.Sleep(400 * time.Millisecond)
	wantPlain(t, get(t, srv, testHost, "/"), 502, textNoAnswer)
}

// resetter accepts connections, reads a request's head and resets the
// connection without an answer.
func resetter(t *testing.T, fn *fakeNet, addr string) *atomic.Int64 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepted atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_, _ = http.ReadRequest(bufio.NewReader(c))
			_ = c.(*net.TCPConn).SetLinger(0)
			_ = c.Close()
		}
	}()
	fn.route(net.JoinHostPort(addr, "8080"), ln.Addr().String())
	return &accepted
}

// A request that was sent but got no response goes to another endpoint
// only when it is a GET, HEAD or OPTIONS without a body.
func TestRetryReset(t *testing.T) {
	for _, c := range []struct {
		method string
		body   io.Reader
		status int
	}{
		{"POST", strings.NewReader("data"), http.StatusBadGateway},
		{"GET", nil, http.StatusOK},
		{"HEAD", nil, http.StatusOK},
		{"DELETE", nil, http.StatusBadGateway},
	} {
		fn := newFakeNet()
		accepted := resetter(t, fn, "fd00::1")
		upstream(t, fn, "two", "fd00::2")
		p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1", "n2")
		p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"), ep("w2", "fd00::2", "n2"))))
		r := send(context.Background(), srv, c.method, testHost, "/", c.body)
		if r.err != nil || r.status != c.status {
			t.Errorf("%s: %d %q %v, want %d", c.method, r.status, r.body, r.err, c.status)
		}
		if c.status == http.StatusBadGateway && r.body != textNoAnswer+"\n" {
			t.Errorf("%s: %q", c.method, r.body)
		}
		if n := accepted.Load(); n != 1 {
			t.Errorf("%s: the resetting endpoint got %d connections", c.method, n)
		}
	}
}

// A refused connection names the address an app must listen on.
func TestRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()
	fn := newFakeNet()
	fn.route("[fd00::1]:8080", closed)
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.cool = time.Millisecond
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	wantPlain(t, get(t, srv, testHost, "/"), 502, "the service refused the connection; it must listen on [::]:8080")
	if p.m.refused.Load() != 1 {
		t.Errorf("refused %d", p.m.refused.Load())
	}
	// The same through a fake dial.
	fn.failDial("[fd00::1]:8080", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})
	time.Sleep(5 * time.Millisecond)
	wantPlain(t, get(t, srv, testHost, "/"), 502, "the service refused the connection; it must listen on [::]:8080")
}

// An endpoint on a node without a fresh handshake is used only when no
// endpoint on a fresh node is usable.
func TestGatingHandshake(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "fresh", "fd00::1")
	upstream(t, fn, "stale", "fd00::2")
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"), ep("w2", "fd00::2", "n2"))))
	for range 4 {
		if e := decode(t, get(t, srv, testHost, "/")); e.Upstream != "fresh" {
			t.Fatalf("went to %s", e.Upstream)
		}
	}
	p.SetState(testState(svc("web", ep("w2", "fd00::2", "n2"))))
	if e := decode(t, get(t, srv, testHost, "/")); e.Upstream != "stale" {
		t.Fatalf("went to %s", e.Upstream)
	}
	if p.m.lastResort.Load() != 1 {
		t.Errorf("last resort %d", p.m.lastResort.Load())
	}
	p.SetMeshHealth(map[string]bool{"n1": true, "n2": true})
	decode(t, get(t, srv, testHost, "/"))
	if p.m.lastResort.Load() != 1 {
		t.Errorf("last resort %d after the handshake", p.m.lastResort.Load())
	}
	dp := p.DataPath()
	if dp.Endpoints != 1 || len(dp.EndpointNodes) != 1 || len(dp.FreshAdmittedNodes) != 1 {
		t.Errorf("data path %+v", dp)
	}
}

// An endpoint whose node holds an older version of ferun-ingress than
// the one that admitted this ingress is never used; it counts as
// pending.
func TestGatingAdmittedVersion(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "old", "fd00::1")
	upstream(t, fn, "new", "fd00::2")
	p, srv := newTestProxy(t, Config{PendingHold: 200 * time.Millisecond}, Deps{}, fn, "n1", "n2")
	self := wire.Self{Name: "ingress-0", AppsZone: testZone, AdmittedVersion: 5}
	old := ep("w1", "fd00::1", "n1")
	old.GroupVersion = 4
	p.SetState(stateWith(self, []string{testHost}, svc("web", old)))
	start := time.Now()
	r := get(t, srv, testHost, "/")
	wantPlain(t, r, 503, "no ready instance of web")
	if time.Since(start) < 200*time.Millisecond || r.header.Get("Retry-After") != "5" {
		t.Errorf("answered after %v with Retry-After %q", time.Since(start), r.header.Get("Retry-After"))
	}
	if fn.dialed("[fd00::1]:8080") != 0 {
		t.Error("dialed an endpoint that does not admit the ingress")
	}
	dp := p.DataPath()
	if dp.Endpoints != 1 || len(dp.FreshAdmittedNodes) != 0 {
		t.Errorf("data path %+v", dp)
	}
	cur := ep("w2", "fd00::2", "n2")
	cur.GroupVersion = 5
	p.SetState(stateWith(self, []string{testHost}, svc("web", old, cur)))
	for range 3 {
		if e := decode(t, get(t, srv, testHost, "/")); e.Upstream != "new" {
			t.Fatalf("went to %s", e.Upstream)
		}
	}
	// An endpoint without the route's port is not used either.
	noPort := ep("w3", "fd00::3", "n2")
	noPort.Ports = map[string]int{"metrics": 9090}
	p.SetState(stateWith(self, []string{testHost}, svc("web", noPort)))
	wantPlain(t, get(t, srv, testHost, "/"), 503, "no ready instance of web")
}

// A request to a service whose instances are asleep wakes them and is
// held until an endpoint shows up.
func TestHoldWake(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	waker := &fakeWaker{}
	p, srv := newTestProxy(t, Config{WakeHold: 5 * time.Second, PendingHold: 100 * time.Millisecond},
		Deps{Waker: waker}, fn, "n1")
	asleep := svc("web")
	asleep.Asleep = 1
	p.SetState(testState(asleep))
	done := make(chan result, 1)
	go func() { done <- send(context.Background(), srv, "GET", testHost, "/", nil) }()
	eventually(t, "the wake", func() bool { return waker.count() == 1 })
	eventually(t, "the hold", func() bool { return p.held.count() == 1 })
	// Longer than the pending bound: the wake bound holds it.
	time.Sleep(200 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("answered while asleep: %d %q", r.status, r.body)
	default:
	}
	waking := svc("web")
	waking.Waking = 1
	p.SetState(testState(waking))
	time.Sleep(50 * time.Millisecond)
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	r := <-done
	if e := decode(t, r); e.Upstream != "web" {
		t.Fatalf("went to %s", e.Upstream)
	}
	waker.mu.Lock()
	if len(waker.calls) != 1 || waker.calls[0] != testEnv+"/web" {
		t.Errorf("wakes %v", waker.calls)
	}
	waker.mu.Unlock()
	if p.held.count() != 0 || p.m.heldTotal.Load() != 1 {
		t.Errorf("held %d, total %d", p.held.count(), p.m.heldTotal.Load())
	}
}

// A request that saw instances waking keeps the wake bound when a later
// pick sees only pending ones.
func TestHoldWakingBound(t *testing.T) {
	fn := newFakeNet()
	p, srv := newTestProxy(t, Config{WakeHold: time.Second, PendingHold: 100 * time.Millisecond}, Deps{}, fn, "n1")
	waking := svc("web")
	waking.Waking = 1
	p.SetState(testState(waking))
	start := time.Now()
	done := make(chan result, 1)
	go func() { done <- send(context.Background(), srv, "GET", testHost, "/", nil) }()
	eventually(t, "the hold", func() bool { return p.held.count() == 1 })
	pending := svc("web")
	pending.Pending = 1
	p.SetState(testState(pending))
	r := <-done
	wantPlain(t, r, 503, "no instance of web woke in time, try again")
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Errorf("answered after %v", d)
	}
	if r.header.Get("Retry-After") != "10" {
		t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
	}
	if p.m.holdTimeouts.Load() != 1 {
		t.Errorf("timeouts %d", p.m.holdTimeouts.Load())
	}
}

func TestHoldPending(t *testing.T) {
	fn := newFakeNet()
	waker := &fakeWaker{}
	p, srv := newTestProxy(t, Config{WakeHold: 5 * time.Second, PendingHold: 200 * time.Millisecond},
		Deps{Waker: waker}, fn, "n1")
	pending := svc("web")
	pending.Pending = 2
	p.SetState(testState(pending))
	start := time.Now()
	r := get(t, srv, testHost, "/")
	wantPlain(t, r, 503, "no ready instance of web")
	if d := time.Since(start); d < 200*time.Millisecond || d > 3*time.Second {
		t.Errorf("answered after %v", d)
	}
	if r.header.Get("Retry-After") != "5" {
		t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
	}
	if waker.count() != 0 {
		t.Errorf("woke pending instances: %v", waker.calls)
	}
	// Nothing asleep, waking or pending: 503 at once.
	p.SetState(testState(svc("web")))
	start = time.Now()
	r = get(t, srv, testHost, "/")
	wantPlain(t, r, 503, "no ready instance of web")
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Errorf("held %v with nothing to wait for", d)
	}
}

// A held request whose client goes away ends its hold.
func TestHoldClientGone(t *testing.T) {
	fn := newFakeNet()
	p, srv := newTestProxy(t, Config{PendingHold: 10 * time.Second}, Deps{}, fn, "n1")
	pending := svc("web")
	pending.Pending = 1
	p.SetState(testState(pending))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result, 1)
	go func() { done <- send(ctx, srv, "GET", testHost, "/", nil) }()
	eventually(t, "the hold", func() bool { return p.held.count() == 1 })
	cancel()
	if r := <-done; r.err == nil {
		t.Fatalf("answered: %d", r.status)
	}
	eventually(t, "the end of the hold", func() bool { return p.held.count() == 0 })
	eventually(t, "the count", func() bool {
		p.m.requestsMu.Lock()
		defer p.m.requestsMu.Unlock()
		return p.m.requests["aborted"] == 1
	})
}

// Drain ends every held request with 503 and Retry-After 1, and holds
// none from then on.
func TestHoldDrain(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	p, srv := newTestProxy(t, Config{PendingHold: 10 * time.Second}, Deps{}, fn, "n1")
	pending := svc("api")
	pending.Pending = 1
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1")), pending))
	done := make(chan result, 2)
	for range 2 {
		go func() { done <- send(context.Background(), srv, "GET", testHost, "/api/x", nil) }()
	}
	eventually(t, "the holds", func() bool { return p.held.count() == 2 })
	p.Drain()
	for range 2 {
		r := <-done
		wantPlain(t, r, 503, textDraining)
		if r.header.Get("Retry-After") != "1" {
			t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
		}
	}
	start := time.Now()
	wantPlain(t, get(t, srv, testHost, "/api/x"), 503, textDraining)
	if time.Since(start) > time.Second {
		t.Error("held while draining")
	}
	// Requests that need no hold are still served.
	decode(t, get(t, srv, testHost, "/"))
}

func TestMaxHeld(t *testing.T) {
	fn := newFakeNet()
	p, srv := newTestProxy(t, Config{PendingHold: 10 * time.Second, MaxHeld: 1}, Deps{}, fn, "n1")
	pending := svc("web")
	pending.Pending = 1
	p.SetState(testState(pending))
	done := make(chan result, 1)
	go func() { done <- send(context.Background(), srv, "GET", testHost, "/", nil) }()
	eventually(t, "the hold", func() bool { return p.held.count() == 1 })
	r := get(t, srv, testHost, "/")
	wantPlain(t, r, 503, textTooMany)
	p.Drain()
	wantPlain(t, <-done, 503, textDraining)
	if p.held.count() != 0 {
		t.Errorf("held %d", p.held.count())
	}
}

// An upgrade passes through, both ways, and its cookies lose a foreign
// Domain too.
func TestWebSocket(t *testing.T) {
	fn := newFakeNet()
	handlerAt(t, fn, "fd00::1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Set-Cookie: s=1; Domain=apps.example; Path=/\r\n\r\n")
		_ = brw.Flush()
		buf := make([]byte, 64)
		for {
			n, err := brw.Read(buf)
			if err != nil {
				return
			}
			if _, err := c.Write(buf[:n]); err != nil {
				return
			}
		}
	}))
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = io.WriteString(c, "GET /ws HTTP/1.1\r\nHost: "+testHost+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", res.StatusCode)
	}
	if got := res.Header.Values("Set-Cookie"); len(got) != 1 || got[0] != "s=1; Path=/" {
		t.Errorf("cookies %q", got)
	}
	for _, msg := range []string{"hello", "again"} {
		if _, err := io.WriteString(c, msg); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(br, buf); err != nil || string(buf) != msg {
			t.Fatalf("echo %q %v", buf, err)
		}
	}
	if p.up.open() != 1 {
		t.Errorf("open upstream connections %d", p.up.open())
	}
	// Closing the upstreams ends the upgraded connection.
	p.CloseUpstreams()
	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("read after close: %v", err)
	}
	eventually(t, "the count", func() bool {
		p.m.requestsMu.Lock()
		defer p.m.requestsMu.Unlock()
		return p.m.requests["upgraded"] == 1
	})
}

// An upgrade that fails after the upstream answered (another protocol
// than the one asked for) is answered 502 and goes to no other endpoint:
// the app saw the request.
func TestUpgradeFailed(t *testing.T) {
	fn := newFakeNet()
	var first, second atomic.Int64
	handlerAt(t, fn, "fd00::1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first.Add(1)
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "other")
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	handlerAt(t, fn, "fd00::2", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"), ep("w2", "fd00::2", "n1"))))
	req, _ := http.NewRequest("GET", srv.URL+"/ws", nil)
	req.Host = testHost
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadGateway || string(b) != textBadUpgrade+"\n" {
		t.Errorf("got %d %q", res.StatusCode, b)
	}
	if first.Load() != 1 || second.Load() != 0 || p.m.retries.Load() != 0 {
		t.Errorf("first endpoint %d, second %d, retries %d", first.Load(), second.Load(), p.m.retries.Load())
	}
	if p.m.responseErrors.Load() != 1 {
		t.Errorf("response errors %d", p.m.responseErrors.Load())
	}
	eventually(t, "the count", func() bool {
		p.m.requestsMu.Lock()
		defer p.m.requestsMu.Unlock()
		return p.m.requests["5xx"] == 1 && p.m.requests["upgraded"] == 0
	})
}

// An event stream reaches the client event by event.
func TestEventStream(t *testing.T) {
	fn := newFakeNet()
	release := make(chan struct{})
	handlerAt(t, fn, "fd00::1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: one\n\n")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: two\n\n")
	}))
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.Host = testHost
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	line := make(chan string, 1)
	br := bufio.NewReader(res.Body)
	go func() {
		s, _ := br.ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if s != "data: one\n" {
			t.Fatalf("first line %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event did not arrive before the upstream finished")
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if string(rest) != "\ndata: two\n\n" {
		t.Errorf("rest %q", rest)
	}
}

func TestMaxBody(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	p, srv := newTestProxy(t, Config{MaxBody: 10}, Deps{}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	text := "the request body is larger than 10 bytes"
	wantPlain(t, send(context.Background(), srv, "POST", testHost, "/", strings.NewReader("01234567890")), 413, text)
	if fn.dialed("[fd00::1]:8080") != 0 {
		t.Error("dialed for a body that is too large")
	}
	r := send(context.Background(), srv, "POST", testHost, "/", struct{ io.Reader }{strings.NewReader("0123456789abcdef")})
	wantPlain(t, r, 413, text)
	for _, b := range []io.Reader{strings.NewReader("0123456789"), struct{ io.Reader }{strings.NewReader("0123456789")}} {
		if e := decode(t, send(context.Background(), srv, "POST", testHost, "/", b)); e.Body != "0123456789" {
			t.Errorf("body %q", e.Body)
		}
	}
	if p.m.tooLarge.Load() != 2 {
		t.Errorf("too large %d", p.m.tooLarge.Load())
	}
}

// Pooled connections close after the idle timeout, and when endpoints
// leave the table.
func TestUpstreamIdle(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "one", "fd00::1")
	upstream(t, fn, "two", "fd00::2")
	p, srv := newTestProxy(t, Config{IdleTimeout: 300 * time.Millisecond}, Deps{}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	decode(t, get(t, srv, testHost, "/"))
	decode(t, get(t, srv, testHost, "/"))
	if n := fn.dialed("[fd00::1]:8080"); n != 1 {
		t.Fatalf("dials %d, want a reused connection", n)
	}
	time.Sleep(700 * time.Millisecond)
	decode(t, get(t, srv, testHost, "/"))
	if n := fn.dialed("[fd00::1]:8080"); n != 2 {
		t.Fatalf("dials %d after the idle timeout", n)
	}

	p2, srv2 := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p2.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"), ep("w2", "fd00::2", "n1"))))
	for range 4 {
		decode(t, get(t, srv2, testHost, "/"))
	}
	if a, b := fn.dialed("[fd00::1]:8080"), fn.dialed("[fd00::2]:8080"); a != 3 || b != 1 {
		t.Fatalf("dials %d %d", a, b)
	}
	// A state that changes nothing keeps the pool.
	p2.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"), ep("w2", "fd00::2", "n1"))))
	decode(t, get(t, srv2, testHost, "/"))
	if a, b := fn.dialed("[fd00::1]:8080"), fn.dialed("[fd00::2]:8080"); a != 3 || b != 1 {
		t.Fatalf("dials %d %d after a heartbeat", a, b)
	}
	// An endpoint leaves: the idle connections close.
	p2.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	decode(t, get(t, srv2, testHost, "/"))
	if n := fn.dialed("[fd00::1]:8080"); n != 4 {
		t.Fatalf("dials %d after an endpoint left", n)
	}
}

func TestRedirect(t *testing.T) {
	p := New(Config{}, Deps{})
	p.SetState(testState())
	srv := httptest.NewServer(p.ServeHTTPRedirect())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", srv.URL+"/a%20b/c?q=1", nil)
	req.Host = "App.apps.example:80"
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusPermanentRedirect || res.Header.Get("Location") != "https://"+testHost+"/a%20b/c?q=1" {
		t.Errorf("%d %q", res.StatusCode, res.Header.Get("Location"))
	}
	req, _ = http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "other.example"
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown host: %d", res.StatusCode)
	}
}

func TestHostOnlyCookies(t *testing.T) {
	const host = "shop.apps.fecode.dev"
	for _, c := range []struct{ in, want string }{
		{"a=1; Domain=fecode.dev; Path=/", "a=1; Path=/"},
		{"b=2; domain=.apps.fecode.dev", "b=2"},
		{"c=3; Domain=other.example; Secure; HttpOnly", "c=3; Secure; HttpOnly"},
		{"d=4; Domain=" + host + "; HttpOnly", "d=4; Domain=" + host + "; HttpOnly"},
		{"d=4; Domain=.SHOP.apps.fecode.dev", "d=4; Domain=.SHOP.apps.fecode.dev"},
		{"e=5; Path=/; Max-Age=60", "e=5; Path=/; Max-Age=60"},
		{"f=6;Domain = fecode.dev ;path=/", "f=6;path=/"},
		{"g=7;   DOMAIN=fecode.dev;  SameSite=Lax", "g=7;  SameSite=Lax"},
		{"h=8; Domain=", "h=8"},
		{"domain=fecode.dev; Path=/", "domain=fecode.dev; Path=/"},
		{"i=9; Domain=fecode.dev; Domain=" + host, "i=9; Domain=" + host},
		{"j=a=b; Expires=Wed, 21 Oct 2026 07:28:00 GMT; Domain=x.apps.fecode.dev",
			"j=a=b; Expires=Wed, 21 Oct 2026 07:28:00 GMT"},
	} {
		got, n := cookieWithoutDomain(c.in, host)
		if got != c.want || (n > 0) != (c.in != c.want) {
			t.Errorf("%q: %q (%d), want %q", c.in, got, n, c.want)
		}
	}
	h := http.Header{}
	h.Add("Content-Type", "text/plain")
	for _, v := range []string{"a=1; Domain=fecode.dev", "b=2", "c=3; Domain=apps.fecode.dev; Path=/x", "d=4; Domain=" + host} {
		h.Add("Set-Cookie", v)
	}
	if n := hostOnlyCookies(h, host); n != 2 {
		t.Errorf("removed %d", n)
	}
	want := []string{"a=1", "b=2", "c=3; Path=/x", "d=4; Domain=" + host}
	if got := h.Values("Set-Cookie"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("cookies %q, want %q", got, want)
	}
	if hostOnlyCookies(http.Header{}, host) != 0 {
		t.Error("removed from nothing")
	}
}

// The proxy makes cookies host-only on hosts under the apps zone, on
// every status, and leaves those of other hosts alone.
func TestCookieFilter(t *testing.T) {
	fn := newFakeNet()
	handlerAt(t, fn, "fd00::1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Link", "</app.css>; rel=preload")
		w.Header().Add("Set-Cookie", "early=1; Domain=apps.example")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.Header().Del("Set-Cookie")
		w.Header().Add("Set-Cookie", "a=1; Domain=apps.example; Path=/")
		w.Header().Add("Set-Cookie", "b=2")
		w.Header().Add("Set-Cookie", "c=3; domain=.example")
		w.Header().Add("Set-Cookie", "d=4; Domain="+r.Host)
		w.WriteHeader(http.StatusFound)
	}))
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.SetState(stateWith(wire.Self{AppsZone: "Apps.Example."}, []string{testHost, "custom.example.org"},
		svc("web", ep("w1", "fd00::1", "n1"))))
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var early []string
	do := func(host string) []string {
		early = nil
		trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			early = append(early, h.Values("Set-Cookie")...)
			return nil
		}}
		req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), "GET", srv.URL+"/", nil)
		req.Host = host
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusFound {
			t.Fatalf("status %d", res.StatusCode)
		}
		return res.Header.Values("Set-Cookie")
	}
	want := []string{"a=1; Path=/", "b=2", "c=3", "d=4; Domain=" + testHost}
	if got := do(testHost); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("cookies %q, want %q", got, want)
	}
	if len(early) != 1 || early[0] != "early=1" {
		t.Errorf("early hints' cookies %q", early)
	}
	if p.m.cookieDomains.Load() != 3 {
		t.Errorf("removed %d", p.m.cookieDomains.Load())
	}
	want = []string{"a=1; Domain=apps.example; Path=/", "b=2", "c=3; domain=.example", "d=4; Domain=custom.example.org"}
	if got := do("custom.example.org"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("outside the zone: %q, want %q", got, want)
	}
	if len(early) != 1 || early[0] != "early=1; Domain=apps.example" {
		t.Errorf("early hints' cookies outside the zone %q", early)
	}
}

func TestMetrics(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	decode(t, get(t, srv, testHost, "/"))
	wantPlain(t, get(t, srv, "nope.example", "/"), 404, textNoHost)
	var b bytes.Buffer
	p.Metrics(&b)
	out := b.String()
	for _, line := range []string{
		"# TYPE ferun_ingress_requests_total counter\n",
		`ferun_ingress_requests_total{class="2xx"} 1` + "\n",
		`ferun_ingress_requests_total{class="4xx"} 1` + "\n",
		`ferun_ingress_upstream_errors_total{kind="dial"} 0` + "\n",
		"ferun_ingress_retries_total 0\n",
		"ferun_ingress_held 0\n",
		"ferun_ingress_idle_holds 0\n",
		"ferun_ingress_endpoints 1\n",
		`ferun_ingress_mesh_nodes{handshake="fresh"} 1` + "\n",
		`ferun_ingress_endpoint_nodes{admitted="true"} 1` + "\n",
		"ferun_ingress_hosts 1\n",
		"# TYPE ferun_ingress_table_age_seconds gauge\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("no %q in\n%s", line, out)
		}
	}
	b.Reset()
	WriteMetric(&b, "x_total", "counter", "Help with \\ and\nnewline.", Sample{Labels: []string{"a", "q\"\\\n"}, Value: 1.5})
	if want := "# HELP x_total Help with \\\\ and\\nnewline.\n# TYPE x_total counter\nx_total{a=\"q\\\"\\\\\\n\"} 1.5\n"; b.String() != want {
		t.Errorf("exposition %q, want %q", b.String(), want)
	}
}

// The HTTPS server negotiates HTTP/2 and serves with the wildcard
// certificate whatever the SNI.
func TestHTTPSServer(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "*."+testZone)
	certs, err := LoadPair(certFile, keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	p := New(Config{}, Deps{Dial: fn.dial})
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	srv := p.HTTPSServer(certs)
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 120*time.Second || srv.MaxHeaderBytes != 32<<10 ||
		srv.HTTP2.MaxConcurrentStreams != 63 || srv.HTTP2.MaxReceiveBufferPerStream != 64<<10 ||
		srv.HTTP2.MaxReceiveBufferPerConnection != 63*64<<10 {
		t.Errorf("limits %v %v %d %+v", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.MaxHeaderBytes, srv.HTTP2)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer srv.Close()
	for _, sni := range []string{testHost, "unknown.example", ""} {
		tr := &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, ServerName: sni},
			ForceAttemptHTTP2: true,
		}
		client := &http.Client{Transport: tr}
		req, _ := http.NewRequest("GET", "https://"+ln.Addr().String()+"/x", nil)
		req.Host = testHost
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("sni %q: %v", sni, err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		e := decode(t, result{status: res.StatusCode, body: string(b)})
		if res.ProtoMajor != 2 || e.Headers["X-Forwarded-Proto"] != "https" {
			t.Errorf("sni %q: proto %d, forwarded proto %q", sni, res.ProtoMajor, e.Headers["X-Forwarded-Proto"])
		}
		if cn := res.TLS.PeerCertificates[0].Subject.CommonName; cn != "*."+testZone {
			t.Errorf("sni %q: certificate %q", sni, cn)
		}
		tr.CloseIdleConnections()
	}
	if p.HTTPServer().Handler == nil {
		t.Error("no redirect handler")
	}
}
