// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

const (
	envA   = "00000000-0000-0000-0000-00000000000a"
	envA2  = "00000000-0000-0000-0000-0000000000a2"
	envB   = "00000000-0000-0000-0000-00000000000b"
	hostA  = "a-evil." + testZone
	hostA2 = "a2-evil." + testZone
	hostB  = "shop-acme." + testZone
)

// twoEnvs is a state of two environments of two tenants, at hostA and
// hostB, each with a root route to its service web.
func twoEnvs(a, b wire.Service) *wire.State {
	s := wire.NewState()
	s.Self = wire.Self{Name: "ingress-0", AppsZone: testZone}
	routes := []wire.Route{{Name: "root", Path: "/", Service: "web", Port: "http"}}
	s.Envs[envA] = wire.Env{ID: envA, Tenant: "evil", Name: "a", Hosts: []string{hostA}, Deployment: 1,
		Routes: routes, Services: []wire.Service{a}}
	s.Envs[envB] = wire.Env{ID: envB, Tenant: "acme", Name: "shop", Hosts: []string{hostB}, Deployment: 1,
		Routes: routes, Services: []wire.Service{b}}
	return s
}

// silentUpstream routes the endpoint address addr (port 8080) to a
// listener that accepts connections and never reads or answers: a
// tenant's app that passed its readiness check and then stays silent.
func silentUpstream(t *testing.T, fn *fakeNet, addr string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = lis.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	fn.route(net.JoinHostPort(addr, "8080"), lis.Addr().String())
}

// blockingUpstream routes addr (port 8080) to a server whose requests
// wait until release is closed (or their client goes away), then answer
// with the upstream's name; arrived counts the requests that reached it.
func blockingUpstream(t *testing.T, fn *fakeNet, name, addr string, release <-chan struct{}, arrived *atomic.Int64) {
	t.Helper()
	handlerAt(t, fn, addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			_, _ = io.WriteString(w, name)
			return
		}
		arrived.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, name)
	}))
}

// quick sends a GET that must be answered within 2 s.
func quick(t *testing.T, srv *httptest.Server, host string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := send(ctx, srv, "GET", host, "/", nil)
	if r.err != nil {
		t.Fatalf("GET %s: %v", host, r.err)
	}
	return r
}

// releaser returns a function that closes release once; it also runs
// when the test ends, before the servers close, so that a failed test
// leaves no request waiting.
func releaser(t *testing.T, release chan struct{}) func() {
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(free)
	return free
}

// F1: requests to an upstream that never answers are bounded. Beyond the
// environment's share of forwarded requests they are answered 503 at
// once; of those admitted, the ones that got no connection (beyond
// MaxConnsPerEndpoint) are answered 503 after the wait bound; only those
// with a connection stay, so goroutines and memory do not grow with the
// requests sent.
func TestStuckUpstreamBounded(t *testing.T) {
	const (
		n         = 300
		perEnv    = 30
		conns     = 6
		waitBound = 300 * time.Millisecond
	)
	fn := newFakeNet()
	silentUpstream(t, fn, "fd00::1")
	p, srv := newTestProxy(t, Config{MaxInflightPerEnv: perEnv, MaxConnsPerEndpoint: conns, ConnWaitTimeout: waitBound},
		Deps{}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))

	runtime.GC()
	g0 := runtime.NumGoroutine()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	type answer struct {
		c          net.Conn
		status     int
		body       string
		retryAfter string
		err        error
	}
	answers := make(chan answer, n)
	var readers sync.WaitGroup
	var all []net.Conn
	t.Cleanup(func() {
		for _, c := range all {
			_ = c.Close()
		}
	})
	for range n {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, c)
		if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+testHost+"\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		readers.Go(func() {
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			res, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				answers <- answer{c: c, err: err}
				return
			}
			b, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			answers <- answer{c: c, status: res.StatusCode, body: string(b), retryAfter: res.Header.Get("Retry-After")}
		})
	}
	if got := p.inflight.count(); got > perEnv {
		t.Errorf("forwarded %d requests, more than the share %d", got, perEnv)
	}
	readers.Wait()
	close(answers)
	busy, noConn, stuck := 0, 0, 0
	for a := range answers {
		switch {
		case a.err != nil:
			stuck++
			continue
		case a.status == http.StatusServiceUnavailable && a.body == textBusyHere+"\n" && a.retryAfter == "5":
			busy++
		case a.status == http.StatusServiceUnavailable && a.body == textNoConn+"\n" && a.retryAfter == "5":
			noConn++
		default:
			t.Errorf("answer %d %q (Retry-After %q)", a.status, a.body, a.retryAfter)
		}
		_ = a.c.Close()
	}
	// Only the stuck requests are exact: when the requests arrive slower
	// than the bound (-race on one CPU), the first admitted ones give up
	// before the last arrive, and their places go to later ones, which
	// give up in turn.
	if stuck != conns || busy+noConn != n-conns || noConn < perEnv-conns {
		t.Errorf("%d answered over the share, %d without a connection in time, %d stuck; want %d in all, "+
			"at least %d, %d", busy, noConn, stuck, n-conns, perEnv-conns, conns)
	}
	if got := p.up.open(); got > conns {
		t.Errorf("%d upstream connections, more than %d", got, conns)
	}
	if p.m.rejectedInflightEnv.Load() != uint64(busy) || p.m.waitErrors.Load() != uint64(noConn) ||
		p.m.dialErrors.Load() != 0 {
		t.Errorf("rejected %d, waits %d, dial errors %d", p.m.rejectedInflightEnv.Load(), p.m.waitErrors.Load(),
			p.m.dialErrors.Load())
	}
	eventually(t, "the requests without a connection to end", func() bool { return p.inflight.count() == conns })

	// What stays is what the stuck requests hold, not what was sent.
	var g1 int
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		if g1 = runtime.NumGoroutine(); g1-g0 <= 10*conns || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if g1-g0 > 10*conns {
		t.Errorf("goroutines grew by %d for %d stuck requests", g1-g0, conns)
	}
	t.Logf("%d requests: %d stuck, goroutines +%d, heap %+d KiB", n, stuck, g1-g0,
		(int64(after.HeapInuse)-int64(before.HeapInuse))>>10)
}

// F1: an environment's share of forwarded requests leaves the others
// theirs, and the instance's cap stops them all.
func TestInflightLimits(t *testing.T) {
	fn := newFakeNet()
	release := make(chan struct{})
	var arrivedA, arrivedB atomic.Int64
	blockingUpstream(t, fn, "a", "fd00::a", release, &arrivedA)
	blockingUpstream(t, fn, "b", "fd00::b", release, &arrivedB)
	p, srv := newTestProxy(t, Config{MaxInflightPerEnv: 3, MaxInflight: 5}, Deps{}, fn, "n1")
	free := releaser(t, release)
	p.SetState(twoEnvs(svc("web", ep("a1", "fd00::a", "n1")), svc("web", ep("b1", "fd00::b", "n1"))))

	done := make(chan result, 5)
	for range 3 {
		go func() { done <- send(context.Background(), srv, "GET", hostA, "/", nil) }()
	}
	eventually(t, "A's share in flight", func() bool { return arrivedA.Load() == 3 })
	r := quick(t, srv, hostA)
	wantPlain(t, r, 503, textBusyHere)
	if r.header.Get("Retry-After") != "5" {
		t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
	}
	// B is not affected by A's share.
	for range 2 {
		go func() { done <- send(context.Background(), srv, "GET", hostB, "/", nil) }()
	}
	eventually(t, "B's requests in flight", func() bool { return arrivedB.Load() == 2 })
	if p.inflight.of(envA) != 3 || p.inflight.of(envB) != 2 {
		t.Errorf("in flight: A %d, B %d", p.inflight.of(envA), p.inflight.of(envB))
	}
	// The instance's cap is taken now, for B as well.
	r = quick(t, srv, hostB)
	wantPlain(t, r, 503, textBusy)
	if r.header.Get("Retry-After") != "1" {
		t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
	}
	free()
	for range 5 {
		if r := <-done; r.err != nil || r.status != http.StatusOK {
			t.Errorf("held request: %d %q %v", r.status, r.body, r.err)
		}
	}
	if r := get(t, srv, hostB, "/"); r.status != http.StatusOK || r.body != "b" {
		t.Errorf("after the release: %d %q", r.status, r.body)
	}
	// A request leaves its count after its answer reached the client.
	eventually(t, "the requests to end", func() bool { return p.inflight.count() == 0 })
	if p.m.rejectedInflightEnv.Load() != 1 || p.m.rejectedInflight.Load() != 1 || p.m.rejectedInflightTenant.Load() != 0 {
		t.Errorf("rejected %d %d %d", p.m.rejectedInflightEnv.Load(), p.m.rejectedInflight.Load(),
			p.m.rejectedInflightTenant.Load())
	}
}

// threeEnvs is twoEnvs with a second environment of A's tenant at
// hostA2.
func threeEnvs(a, a2, b wire.Service) *wire.State {
	s := twoEnvs(a, b)
	e := s.Envs[envA]
	e.ID, e.Name, e.Hosts, e.Services = envA2, "a2", []string{hostA2}, []wire.Service{a2}
	s.Envs[envA2] = e
	return s
}

// F1, F2: a tenant's environments together have the tenant's share, of
// the forwarded and of the held requests, so that a tenant does not fill
// the instance's cap with more environments; another tenant keeps its
// own.
func TestTenantShare(t *testing.T) {
	fn := newFakeNet()
	release := make(chan struct{})
	var arrivedA, arrivedA2, arrivedB atomic.Int64
	blockingUpstream(t, fn, "a", "fd00::a", release, &arrivedA)
	blockingUpstream(t, fn, "a2", "fd00::a2", release, &arrivedA2)
	blockingUpstream(t, fn, "b", "fd00::b", release, &arrivedB)
	p, srv := newTestProxy(t, Config{MaxInflightPerEnv: 2, MaxInflightPerTenant: 3, MaxInflight: 10}, Deps{}, fn, "n1")
	free := releaser(t, release)
	p.SetState(threeEnvs(svc("web", ep("a1", "fd00::a", "n1")), svc("web", ep("a21", "fd00::a2", "n1")),
		svc("web", ep("b1", "fd00::b", "n1"))))

	done := make(chan result, 5)
	for range 2 {
		go func() { done <- send(context.Background(), srv, "GET", hostA, "/", nil) }()
	}
	eventually(t, "A's share in flight", func() bool { return arrivedA.Load() == 2 })
	go func() { done <- send(context.Background(), srv, "GET", hostA2, "/", nil) }()
	eventually(t, "A2's request in flight", func() bool { return arrivedA2.Load() == 1 })
	// A2 has room of its own, its tenant has none.
	r := quick(t, srv, hostA2)
	wantPlain(t, r, 503, textBusyHere)
	if r.header.Get("Retry-After") != "5" {
		t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
	}
	if p.inflight.ofTenant("evil") != 3 || p.inflight.of(envA2) != 1 {
		t.Errorf("in flight: tenant %d, A2 %d", p.inflight.ofTenant("evil"), p.inflight.of(envA2))
	}
	// The other tenant is not affected.
	for range 2 {
		go func() { done <- send(context.Background(), srv, "GET", hostB, "/", nil) }()
	}
	eventually(t, "B's requests in flight", func() bool { return arrivedB.Load() == 2 })
	free()
	for range 5 {
		if r := <-done; r.err != nil || r.status != http.StatusOK {
			t.Errorf("forwarded request: %d %q %v", r.status, r.body, r.err)
		}
	}
	eventually(t, "the requests to end", func() bool { return p.inflight.count() == 0 })
	if p.m.rejectedInflightTenant.Load() != 1 || p.m.rejectedInflightEnv.Load() != 0 || p.m.rejectedInflight.Load() != 0 {
		t.Errorf("rejected: tenant %d, env %d, cap %d", p.m.rejectedInflightTenant.Load(),
			p.m.rejectedInflightEnv.Load(), p.m.rejectedInflight.Load())
	}

	// Held requests: shares of 2 per environment and 3 per tenant.
	pending := svc("web")
	pending.Pending = 1
	p2, srv2 := newTestProxy(t, Config{MaxHeld: 10, MaxHeldPerTenant: 3, MaxHeldPerEnv: 2, PendingHold: 10 * time.Second},
		Deps{}, fn, "n1")
	p2.SetState(threeEnvs(pending, pending, pending))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for _, h := range []string{hostA, hostA, hostA2, hostB} {
		wg.Go(func() { send(ctx, srv2, "GET", h, "/", nil) })
	}
	eventually(t, "the requests held", func() bool { return p2.held.count() == 4 })
	r = quick(t, srv2, hostA2)
	wantPlain(t, r, 503, textTooManyHere)
	if p2.held.ofTenant("evil") != 3 || p2.m.rejectedHeldTenant.Load() != 1 || p2.m.rejectedHeldEnv.Load() != 0 {
		t.Errorf("held: tenant %d, rejected: tenant %d, env %d", p2.held.ofTenant("evil"),
			p2.m.rejectedHeldTenant.Load(), p2.m.rejectedHeldEnv.Load())
	}
	cancel()
	wg.Wait()
}

// F1: a request that waits longer than the bound for a connection to an
// instance goes to another usable instance, as nothing of it was sent,
// and is answered 503 when there is none. The busy instance does not
// cool down.
func TestConnWait(t *testing.T) {
	fn := newFakeNet()
	release := make(chan struct{})
	var arrived1 atomic.Int64
	blockingUpstream(t, fn, "one", "fd00::1", release, &arrived1)
	upstream(t, fn, "two", "fd00::2")
	const bound = 200 * time.Millisecond
	p, srv := newTestProxy(t, Config{MaxConnsPerEndpoint: 1, ConnWaitTimeout: bound}, Deps{}, fn, "n1")
	free := releaser(t, release)
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"), ep("w2", "fd00::2", "n1"))))

	// Round robin: w1 (stays busy), w2, then w1 again, which has no
	// connection free, so the request goes to w2.
	done := make(chan result, 1)
	go func() { done <- send(context.Background(), srv, "GET", testHost, "/", nil) }()
	eventually(t, "the request on w1", func() bool { return arrived1.Load() == 1 })
	if e := decode(t, get(t, srv, testHost, "/")); e.Upstream != "two" {
		t.Fatalf("second request on %s", e.Upstream)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := decode(t, send(ctx, srv, "POST", testHost, "/", strings.NewReader("body")))
	if e.Upstream != "two" || e.Body != "body" || time.Since(start) < bound {
		t.Errorf("third request on %s with %q after %v", e.Upstream, e.Body, time.Since(start))
	}
	if p.m.waitErrors.Load() != 1 || p.m.retries.Load() != 1 || p.m.dialErrors.Load() != 0 {
		t.Errorf("waits %d, retries %d, dial errors %d", p.m.waitErrors.Load(), p.m.retries.Load(), p.m.dialErrors.Load())
	}
	p.mu.Lock()
	cooling := len(p.cooling)
	p.mu.Unlock()
	if cooling != 0 {
		t.Errorf("%d endpoints cool down", cooling)
	}

	// With w1 alone there is nowhere else to go.
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	r := quick(t, srv, testHost)
	wantPlain(t, r, 503, textNoConn)
	if r.header.Get("Retry-After") != "5" {
		t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
	}
	free()
	if r := <-done; r.status != http.StatusOK || r.body != "one" {
		t.Errorf("request on w1: %d %q %v", r.status, r.body, r.err)
	}
	// The wait bounds only the wait: an answer may take longer.
	slow := make(chan struct{})
	handlerAt(t, fn, "fd00::3", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-slow
		_, _ = io.WriteString(w, "slow")
	}))
	p.SetState(testState(svc("web", ep("w3", "fd00::3", "n1"))))
	go func() {
		time.Sleep(3 * bound)
		close(slow)
	}()
	if r := get(t, srv, testHost, "/"); r.status != http.StatusOK || r.body != "slow" {
		t.Errorf("slow answer: %d %q", r.status, r.body)
	}
}

// F1: an HTTP/2 connection carries at most 63 requests at once: the
// server says so in its settings, and refuses the streams beyond (Go's
// default is 250). The client is raw frames, as an attacker's would be.
func TestHTTP2Streams(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "*."+testZone)
	certs, err := LoadPair(certFile, keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	fn := newFakeNet()
	release := make(chan struct{})
	var arrived atomic.Int64
	blockingUpstream(t, fn, "web", "fd00::1", release, &arrived)
	p := New(Config{}, Deps{Dial: fn.dial})
	p.SetMeshHealth(map[string]bool{"n1": true})
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	srv := p.HTTPSServer(certs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer func() {
		close(release)
		_ = srv.Close()
		p.CloseUpstreams()
	}()

	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatalf("protocol %q", c.ConnectionState().NegotiatedProtocol)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	const (
		frameHeaders  = 0x1
		frameRST      = 0x3
		frameSettings = 0x4
		flagAck       = 0x1
		flagEnd       = 0x1 | 0x4 // END_STREAM, END_HEADERS
		settingMaxStr = 0x3
	)
	if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(h2Frame(frameSettings, 0, 0, nil)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	typ, flags, _, payload, err := readH2Frame(br)
	if err != nil || typ != frameSettings || flags&flagAck != 0 {
		t.Fatalf("first frame %d %d: %v", typ, flags, err)
	}
	maxStreams := uint32(0)
	for i := 0; i+6 <= len(payload); i += 6 {
		if binary.BigEndian.Uint16(payload[i:]) == settingMaxStr {
			maxStreams = binary.BigEndian.Uint32(payload[i+2:])
		}
	}
	if maxStreams != maxConcurrentStreams {
		t.Errorf("SETTINGS_MAX_CONCURRENT_STREAMS %d", maxStreams)
	}
	if _, err := c.Write(h2Frame(frameSettings, flagAck, 0, nil)); err != nil {
		t.Fatal(err)
	}
	var refused atomic.Int64
	go func() {
		for {
			typ, _, _, _, err := readH2Frame(br)
			if err != nil {
				return
			}
			if typ == frameRST {
				refused.Add(1)
			}
		}
	}()
	// GET /block on the test host: :method GET, :scheme https (static
	// table), :path and :authority as literals without indexing.
	block := []byte{0x82, 0x87, 0x04, byte(len("/block"))}
	block = append(block, "/block"...)
	block = append(block, 0x01, byte(len(testHost)))
	block = append(block, testHost...)
	const n = 150
	var frames []byte
	for i := range n {
		frames = append(frames, h2Frame(frameHeaders, flagEnd, uint32(2*i+1), block)...)
	}
	if _, err := c.Write(frames); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the streams", func() bool {
		return arrived.Load() == maxConcurrentStreams && refused.Load() == n-maxConcurrentStreams
	})
	time.Sleep(200 * time.Millisecond)
	if got := arrived.Load(); got != maxConcurrentStreams {
		t.Errorf("%d requests at once on one connection", got)
	}
}

// h2Frame encodes an HTTP/2 frame.
func h2Frame(typ, flags byte, stream uint32, payload []byte) []byte {
	b := []byte{byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload)), typ, flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[5:], stream)
	return append(b, payload...)
}

// readH2Frame reads an HTTP/2 frame.
func readH2Frame(r io.Reader) (typ, flags byte, stream uint32, payload []byte, err error) {
	var h [9]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return 0, 0, 0, nil, err
	}
	payload = make([]byte, int(h[0])<<16|int(h[1])<<8|int(h[2]))
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, 0, 0, nil, err
	}
	return h[3], h[4], binary.BigEndian.Uint32(h[5:]) & 0x7fffffff, payload, nil
}

// F2: an environment whose service never becomes ready fills only its
// share of the held requests; another environment's cold start is still
// held and served. The instance's cap stays as a backstop.
func TestHeldShare(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "b", "fd00::b")
	waker := &fakeWaker{}
	const maxHeld, perEnv = 64, 16
	p, srv := newTestProxy(t, Config{MaxHeld: maxHeld, MaxHeldPerEnv: perEnv, PendingHold: 10 * time.Second},
		Deps{Waker: waker}, fn, "n1")
	pending := svc("web")
	pending.Pending = 1
	asleep := svc("web")
	asleep.Idle, asleep.Asleep = true, 1
	p.SetState(twoEnvs(pending, asleep))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	var rejected atomic.Int64
	for range maxHeld {
		wg.Go(func() {
			if r := send(ctx, srv, "GET", hostA, "/", nil); r.status == http.StatusServiceUnavailable &&
				r.body == textTooManyHere+"\n" && r.header.Get("Retry-After") == "1" {
				rejected.Add(1)
			}
		})
	}
	eventually(t, "A's share held and the rest rejected", func() bool {
		return p.held.of(envA) == perEnv && rejected.Load() == maxHeld-perEnv
	})
	// B's request is held while its service wakes, then served.
	done := make(chan result, 1)
	go func() { done <- send(context.Background(), srv, "GET", hostB, "/", nil) }()
	eventually(t, "B's request held", func() bool { return p.held.of(envB) == 1 && waker.count() >= 1 })
	p.SetState(twoEnvs(pending, svc("web", ep("b1", "fd00::b", "n1"))))
	if e := decode(t, <-done); e.Upstream != "b" {
		t.Errorf("B served by %s", e.Upstream)
	}
	if p.m.rejectedHeldEnv.Load() != maxHeld-perEnv || p.m.rejectedHeld.Load() != 0 {
		t.Errorf("rejected %d %d", p.m.rejectedHeldEnv.Load(), p.m.rejectedHeld.Load())
	}
	cancel()
	wg.Wait()
	eventually(t, "the held requests to end", func() bool { return p.held.count() == 0 })

	// The instance's cap: shares of 3 each, 4 in all.
	p2, srv2 := newTestProxy(t, Config{MaxHeld: 4, MaxHeldPerEnv: 3, PendingHold: 10 * time.Second}, Deps{}, fn, "n1")
	p2.SetState(twoEnvs(pending, pending))
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	var wg2 sync.WaitGroup
	for _, h := range []string{hostA, hostA, hostA, hostB} {
		wg2.Go(func() { send(ctx2, srv2, "GET", h, "/", nil) })
	}
	eventually(t, "the cap held", func() bool { return p2.held.count() == 4 })
	wantPlain(t, get(t, srv2, hostB, "/"), 503, textTooMany)
	if p2.m.rejectedHeld.Load() != 1 {
		t.Errorf("rejected at the cap %d", p2.m.rejectedHeld.Load())
	}
	cancel2()
	wg2.Wait()
}

// upgrade opens a connection through srv to host, switches it to an echo
// protocol at path and returns it with its reader.
func upgrade(t *testing.T, srv *httptest.Server, host, path string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: "+host+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", res, err)
	}
	return c, br
}

// echoes reports whether msg comes back through an upgraded connection.
func echoes(c net.Conn, br *bufio.Reader, msg string) bool {
	if _, err := io.WriteString(c, msg); err != nil {
		return false
	}
	buf := make([]byte, len(msg))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := io.ReadFull(br, buf)
	return err == nil && string(buf) == msg
}

// closed reports whether the connection ends within 2 s.
func closed(c net.Conn, br *bufio.Reader) bool {
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := io.ReadAll(br)
	var ne net.Error
	return !(errors.As(err, &ne) && ne.Timeout())
}

// wsEcho is an upstream that switches to an echo protocol, or streams
// one event and then waits for its client.
func wsEcho() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: one\n\n")
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
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
	})
}

// F3: unpublishing an environment ends its upgraded connections and its
// streaming answers, not only new requests.
func TestUnpublishCutsConnections(t *testing.T) {
	fn := newFakeNet()
	handlerAt(t, fn, "fd00::1", wsEcho())
	handlerAt(t, fn, "fd00::b", wsEcho())
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.SetState(twoEnvs(svc("web", ep("a1", "fd00::1", "n1")), svc("web", ep("b1", "fd00::b", "n1"))))
	c, br := upgrade(t, srv, hostA, "/ws")
	if !echoes(c, br, "hello") {
		t.Fatal("no echo")
	}
	other, otherBr := upgrade(t, srv, hostB, "/ws")
	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.Host = hostA
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	events := bufio.NewReader(res.Body)
	if line, _ := events.ReadString('\n'); line != "data: one\n" {
		t.Fatalf("first event %q", line)
	}
	if p.up.open() != 3 {
		t.Fatalf("open upstream connections %d", p.up.open())
	}

	// A is unpublished: it leaves the table.
	s := twoEnvs(svc("web"), svc("web", ep("b1", "fd00::b", "n1")))
	delete(s.Envs, envA)
	p.SetState(s)
	wantPlain(t, get(t, srv, hostA, "/"), 404, textNoHost)
	if !closed(c, br) {
		t.Error("the upgraded connection outlived the unpublish")
	}
	streamEnded := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(events)
		streamEnded <- err
	}()
	select {
	case <-streamEnded:
	case <-time.After(2 * time.Second):
		t.Error("the streaming answer outlived the unpublish")
	}
	// B's connection is untouched.
	if !echoes(other, otherBr, "still") {
		t.Error("another environment's connection was cut")
	}
	eventually(t, "A's connections closed", func() bool { return p.up.open() == 1 })
	if p.m.connsCut.Load() != 2 {
		t.Errorf("cut %d", p.m.connsCut.Load())
	}
}

// F3: an instance that leaves a service that stays routed (a replica
// stops) keeps its open connections for the grace, so that its
// requests end with its pod; an instance that loses the route's port
// loses its connections on that port at once.
func TestEndpointLeavesCutsConnections(t *testing.T) {
	fn := newFakeNet()
	handlerAt(t, fn, "fd00::1", wsEcho())
	handlerAt(t, fn, "fd00::2", wsEcho())
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.grace = 300 * time.Millisecond
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"), ep("w2", "fd00::2", "n1"))))
	c, br := upgrade(t, srv, testHost, "/ws") // w1, by round robin
	if fn.dialed("[fd00::1]:8080") != 1 || !echoes(c, br, "hello") {
		t.Fatal("not on w1")
	}

	// w1 stops.
	p.SetState(testState(svc("web", ep("w2", "fd00::2", "n1"))))
	if !echoes(c, br, "still") {
		t.Fatal("cut before the grace")
	}
	p.cutConns(p.cur.Load().t, time.Now())
	if !echoes(c, br, "here") {
		t.Fatal("cut before the grace")
	}
	time.Sleep(p.grace)
	p.cutConns(p.cur.Load().t, time.Now())
	if !closed(c, br) {
		t.Error("the connection outlived the grace")
	}

	// w2 loses the route's port: its connections close at once.
	c2, br2 := upgrade(t, srv, testHost, "/ws")
	if !echoes(c2, br2, "hello") {
		t.Fatal("no echo")
	}
	noPort := ep("w2", "fd00::2", "n1")
	noPort.Ports = map[string]int{"http": 0, "admin": 9000}
	p.SetState(testState(svc("web", noPort)))
	if !closed(c2, br2) {
		t.Error("the connection outlived its port")
	}
	if p.m.connsCut.Load() != 2 {
		t.Errorf("cut %d", p.m.connsCut.Load())
	}
}

// F3: a request that picked an instance just before the table dropped
// it does not dial it.
func TestDialOutsideTable(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	p, _ := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	c, err := p.up.dialContext(context.Background(), "tcp", "[fd00::1]:8080")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	_, err = p.up.dialContext(context.Background(), "tcp", "[fd00::9]:8080")
	var de *dialError
	if !errors.As(err, &de) || !errors.Is(err, errNotRouted) {
		t.Errorf("dial outside the table: %v", err)
	}
	if fn.dialed("[fd00::9]:8080") != 0 {
		t.Error("dialed")
	}
}

// F3: a dial that passed the table's check before an unpublish and
// connects after it does not join the pool: the request routes again by
// the new table, and no tunnel to the unpublished environment opens.
// The unpublish closes the attempt itself (visitBook.retable), so the
// answer may come before the dial under way returns; the counts are read
// once it has.
func TestDialDuringUnpublish(t *testing.T) {
	fn := newFakeNet()
	handlerAt(t, fn, "fd00::1", wsEcho())
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	var gatedDone atomic.Int64
	p.up.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "[fd00::1]:8080" {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-gate
			defer gatedDone.Add(1)
		}
		return fn.dial(ctx, network, addr)
	}
	p.SetState(twoEnvs(svc("web", ep("a1", "fd00::1", "n1")), svc("web")))

	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "GET /ws HTTP/1.1\r\nHost: "+hostA+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	<-entered
	// A is unpublished while the dial is under way.
	s := twoEnvs(svc("web"), svc("web"))
	delete(s.Envs, envA)
	p.SetState(s)
	close(gate)
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusNotFound || string(b) != textNoHost+"\n" {
		t.Errorf("after the unpublish: %d %q", res.StatusCode, b)
	}
	eventually(t, "the dial under way to return", func() bool { return gatedDone.Load() == 1 })
	if fn.dialed("[fd00::1]:8080") != 1 || p.up.open() != 0 || p.m.dialErrors.Load() != 0 {
		t.Errorf("dials %d, open upstream connections %d, dial errors %d", fn.dialed("[fd00::1]:8080"),
			p.up.open(), p.m.dialErrors.Load())
	}
}

// F1: the transport sends a GET once more by itself when the reused
// connection it went out on closes without an answer; that second wait
// for a connection is bounded as well.
func TestConnWaitOnTransportRetry(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var mu sync.Mutex
	var conns []net.Conn
	var blocks, dies atomic.Int64
	gotDie := make(chan struct{}, 1)
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func() {
				br := bufio.NewReader(c)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					_, _ = io.Copy(io.Discard, req.Body)
					switch req.URL.Path {
					case "/warm":
						time.Sleep(200 * time.Millisecond)
						_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 1\r\n\r\nw")
					case "/block":
						blocks.Add(1)
						<-stop
						return
					case "/die":
						// It goes away without an answer.
						dies.Add(1)
						gotDie <- struct{}{}
						time.Sleep(150 * time.Millisecond)
						_ = c.Close()
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = lis.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	fn := newFakeNet()
	fn.route("[fd00::1]:8080", lis.Addr().String())
	const bound = 400 * time.Millisecond
	p, srv := newTestProxy(t, Config{MaxConnsPerEndpoint: 2, ConnWaitTimeout: bound}, Deps{}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))

	// Two pooled connections.
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if r := get(t, srv, testHost, "/warm"); r.status != http.StatusOK {
				t.Errorf("/warm %d", r.status)
			}
		})
	}
	wg.Wait()
	if p.up.open() != 2 {
		t.Fatalf("open %d", p.up.open())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go send(ctx, srv, "GET", testHost, "/block", nil)
	eventually(t, "the first block", func() bool { return blocks.Load() == 1 })
	// /die takes the other connection; while it is on it, a second
	// /block queues for a connection and takes the place /die's frees.
	done := make(chan result, 1)
	go func() { done <- send(ctx, srv, "GET", testHost, "/die", nil) }()
	<-gotDie
	time.Sleep(50 * time.Millisecond)
	go send(ctx, srv, "GET", testHost, "/block", nil)
	eventually(t, "the second block", func() bool { return blocks.Load() == 2 })
	select {
	case r := <-done:
		wantPlain(t, r, 503, textNoConn)
	case <-time.After(bound + 3*time.Second):
		t.Fatalf("/die still waits for a connection; waits %d", p.m.waitErrors.Load())
	}
	if p.m.waitErrors.Load() != 1 || dies.Load() != 1 {
		t.Errorf("waits %d, /die at the upstream %d times", p.m.waitErrors.Load(), dies.Load())
	}
}

// F3: a request whose instance left the table while the request was on
// its way (here: during the idle hold) routes again by the new table.
func TestRerouteAfterTableChange(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "one", "fd00::1")
	upstream(t, fn, "two", "fd00::2")
	holder := &fakeHolder{block: make(chan struct{})}
	p, srv := newTestProxy(t, Config{}, Deps{Holder: holder}, fn, "n1")
	p.SetState(testState(idleSvc("web", ep("w1", "fd00::1", "n1"))))
	done := make(chan result, 1)
	go func() { done <- send(context.Background(), srv, "GET", testHost, "/", nil) }()
	eventually(t, "the hold of w1", func() bool { return holder.count() == 1 })
	p.SetState(testState(idleSvc("web", ep("w2", "fd00::2", "n1"))))
	close(holder.block)
	if e := decode(t, <-done); e.Upstream != "two" {
		t.Errorf("served by %s", e.Upstream)
	}
	if fn.dialed("[fd00::1]:8080") != 0 || p.m.dialErrors.Load() != 0 {
		t.Errorf("dials of w1 %d, dial errors %d", fn.dialed("[fd00::1]:8080"), p.m.dialErrors.Load())
	}

	// Unpublished meanwhile: no host any more.
	holder2 := &fakeHolder{block: make(chan struct{})}
	p2, srv2 := newTestProxy(t, Config{}, Deps{Holder: holder2}, fn, "n1")
	p2.SetState(testState(idleSvc("web", ep("w1", "fd00::1", "n1"))))
	go func() { done <- send(context.Background(), srv2, "GET", testHost, "/", nil) }()
	eventually(t, "the hold of w1", func() bool { return holder2.count() == 1 })
	empty := wire.NewState()
	empty.Self = wire.Self{Name: "ingress-0", AppsZone: testZone}
	p2.SetState(empty)
	close(holder2.block)
	wantPlain(t, <-done, 404, textNoHost)
	if fn.dialed("[fd00::1]:8080") != 0 {
		t.Errorf("dials of w1 %d", fn.dialed("[fd00::1]:8080"))
	}
}

func TestBudget(t *testing.T) {
	// Shares of 2 per environment and 3 per tenant, 4 in all.
	b := newBudget(2, 3, 4)
	var s1, s2, s3, s4, s5, s6 slot
	for _, c := range []struct {
		s           *slot
		tenant, env string
		want        full
	}{
		{&s1, "t", "a", fits}, {&s1, "t", "a", fits}, {&s2, "t", "a", fits}, {&s3, "t", "a", envFull},
		{&s3, "t", "b", fits}, {&s4, "t", "c", tenantFull}, {&s4, "u", "d", fits}, {&s5, "v", "e", allFull},
	} {
		if got := b.enter(c.s, c.tenant, c.env); got != c.want {
			t.Errorf("enter %s/%s: %d, want %d", c.tenant, c.env, got, c.want)
		}
	}
	if b.count() != 4 || b.of("a") != 2 || b.of("b") != 1 || b.ofTenant("t") != 3 || b.ofTenant("u") != 1 {
		t.Errorf("count %d, a %d, b %d, t %d, u %d", b.count(), b.of("a"), b.of("b"), b.ofTenant("t"), b.ofTenant("u"))
	}
	b.leave(&s1)
	b.leave(&s1)
	b.leave(&s6)
	if b.count() != 3 || b.of("a") != 1 || b.ofTenant("t") != 2 {
		t.Errorf("after leave: count %d, a %d, t %d", b.count(), b.of("a"), b.ofTenant("t"))
	}
	if b.enter(&s5, "v", "e") != fits {
		t.Error("no room after leave")
	}
	for _, s := range []*slot{&s2, &s3, &s4, &s5} {
		b.leave(s)
	}
	if b.count() != 0 || len(b.envs) != 0 || len(b.tenants) != 0 {
		t.Errorf("after all left: count %d, envs %v, tenants %v", b.count(), b.envs, b.tenants)
	}
}

// liveMemory is the heap and the stacks in use after a collection.
func liveMemory() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapInuse + m.StackInuse)
}

// h2Raw is an HTTP/2 client connection of raw frames, as an attacker's
// would be, that keeps to the server's flow control: the windows it was
// granted come from the server's SETTINGS and WINDOW_UPDATE frames.
type h2Raw struct {
	c  *tls.Conn
	br *bufio.Reader
	// streamWindow is the server's SETTINGS_INITIAL_WINDOW_SIZE (65535
	// when it sends none), connWindow the connection's window,
	// headerList its SETTINGS_MAX_HEADER_LIST_SIZE.
	streamWindow, connWindow, headerList int
	// resets counts RST_STREAM and GOAWAY frames.
	resets int
}

const (
	h2Data         = 0x0
	h2Headers      = 0x1
	h2RST          = 0x3
	h2Settings     = 0x4
	h2Ping         = 0x6
	h2GoAway       = 0x7
	h2WindowUpdate = 0x8
	h2FlagAck      = 0x1
)

// The flags of HEADERS and DATA frames.
const (
	h2FlagEndStream = 0x1
	h2FlagEndHdrs   = 0x4
)

// dialH2 connects to addr, exchanges the settings and waits for the
// answer of a PING, so that the server's first frames are read.
func dialH2(t *testing.T, addr string) *h2Raw {
	t.Helper()
	c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if c.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatalf("protocol %q", c.ConnectionState().NegotiatedProtocol)
	}
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	h := &h2Raw{c: c, br: bufio.NewReader(c), streamWindow: 65535, connWindow: 65535}
	h.write(t, append([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), h2Frame(h2Settings, 0, 0, nil)...))
	h.ping(t)
	return h
}

func (h *h2Raw) write(t *testing.T, b []byte) {
	t.Helper()
	if _, err := h.c.Write(b); err != nil {
		t.Fatal(err)
	}
}

// ping sends a PING and reads frames up to its answer: the server reads
// a connection's frames in order, so it has taken every frame sent
// before.
func (h *h2Raw) ping(t *testing.T) {
	t.Helper()
	h.write(t, h2Frame(h2Ping, 0, 0, make([]byte, 8)))
	for {
		if typ, flags, _, _ := h.frame(t); typ == h2Ping && flags&h2FlagAck != 0 {
			return
		}
	}
}

// frame reads the next frame and applies what it says of the
// connection: it takes the server's SETTINGS and acknowledges them, adds
// a WINDOW_UPDATE of the connection to connWindow, and counts RST_STREAM
// and GOAWAY.
func (h *h2Raw) frame(t *testing.T) (typ, flags byte, stream uint32, payload []byte) {
	t.Helper()
	typ, flags, stream, payload, err := readH2Frame(h.br)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case typ == h2Settings && flags&h2FlagAck == 0:
		for i := 0; i+6 <= len(payload); i += 6 {
			v := int(binary.BigEndian.Uint32(payload[i+2:]))
			switch binary.BigEndian.Uint16(payload[i:]) {
			case 0x4:
				h.streamWindow = v
			case 0x6:
				h.headerList = v
			}
		}
		h.write(t, h2Frame(h2Settings, h2FlagAck, 0, nil))
	case typ == h2WindowUpdate && stream == 0:
		h.connWindow += int(binary.BigEndian.Uint32(payload) & 0x7fffffff)
	case typ == h2RST || typ == h2GoAway:
		h.resets++
	}
	return typ, flags, stream, payload
}

// body writes n bytes of stream's body in DATA frames of at most 16 KiB,
// the last one with END_STREAM when end, and takes them from connWindow.
func (h *h2Raw) body(t *testing.T, stream uint32, n int, end bool) {
	t.Helper()
	chunk := make([]byte, 16<<10)
	var frames []byte
	for left := n; left > 0; {
		k := min(left, len(chunk))
		left -= k
		var flags byte
		if end && left == 0 {
			flags = h2FlagEndStream
		}
		frames = append(frames, h2Frame(h2Data, flags, stream, chunk[:k])...)
	}
	h.write(t, frames)
	h.connWindow -= n
}

// postBlock is the header block of a POST of path on the test host:
// :method POST and :scheme https from the static table, :path and
// :authority as literals without indexing.
func postBlock(path string) []byte {
	block := []byte{0x83, 0x87, 0x04, byte(len(path))}
	block = append(block, path...)
	block = append(block, 0x01, byte(len(testHost)))
	return append(block, testHost...)
}

// F1: an HTTP/2 client may send at most 64 KiB of a request's body
// before the request reads it. A held request reads nothing, and the
// server keeps what came: with Go's default window of 1 MiB, each held
// request's body took about 1.1 MiB, an environment's default share 140
// MiB, a tenant's 280 MiB, the cap 1.1 GiB. Each request here is on a
// connection of its own and sends as much of a 1 MiB body as the server
// lets it.
func TestHTTP2BodyWindow(t *testing.T) {
	const (
		n       = 32
		bodyMax = 1 << 20
		// perRequest bounds what a held request's body that came
		// takes, the server's read buffers included.
		perRequest = 256 << 10
	)
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "*."+testZone)
	certs, err := LoadPair(certFile, keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{PendingHold: time.Minute}, Deps{Dial: newFakeNet().dial})
	pending := svc("web")
	pending.Pending = 1
	p.SetState(testState(pending))
	srv := p.HTTPSServer(certs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer srv.Close()

	// POST /upload on the test host without END_STREAM.
	block := postBlock("/upload")
	conns := make([]*h2Raw, n)
	for i := range conns {
		h := dialH2(t, ln.Addr().String())
		if i == 0 && (h.streamWindow != streamReceiveWindow || h.headerList != maxHeaderBytes+320) {
			t.Errorf("SETTINGS_INITIAL_WINDOW_SIZE %d, SETTINGS_MAX_HEADER_LIST_SIZE %d", h.streamWindow, h.headerList)
		}
		h.write(t, h2Frame(h2Headers, 0x4, 1, block))
		conns[i] = h
	}
	eventually(t, "the requests held", func() bool { return p.held.count() == n })

	before := liveMemory()
	chunk := make([]byte, 16<<10)
	sent := 0
	for _, h := range conns {
		left := min(bodyMax, h.streamWindow, h.connWindow)
		sent = left
		var frames []byte
		for left > 0 {
			k := min(left, len(chunk))
			frames = append(frames, h2Frame(h2Data, 0, 1, chunk[:k])...)
			left -= k
		}
		h.write(t, frames)
		h.ping(t)
		if h.resets != 0 {
			t.Fatalf("the server reset the stream or the connection")
		}
	}
	grew := liveMemory() - before
	if got := p.held.count(); got != n {
		t.Fatalf("%d requests held after their bodies, want %d", got, n)
	}
	t.Logf("%d held requests sent %d KiB of body each: %+d KiB, %d KiB per request", n, sent>>10, grew>>10,
		grew/n>>10)
	if grew > n*perRequest {
		t.Errorf("held requests took %d KiB each for their bodies, more than %d KiB", grew/n>>10, perRequest>>10)
	}
}

// F1: the held requests of one HTTP/2 connection do not stall its other
// uploads, and take no more memory together than each its window. The
// connection's window has room for every stream's: here 62 requests
// held on one connection send all they may of their bodies, and an
// upload of 1 MiB on the same connection to an instance that reads it
// still gets through. With Go's default connection window of 1 MiB, the
// first 16 held requests took all of it, and the upload never started.
func TestHTTP2ConnWindow(t *testing.T) {
	const (
		held   = maxConcurrentStreams - 1
		upload = 1 << 20
		// perRequest bounds what a held request's body that came takes
		// on a shared connection: its window and the buffers' slack.
		perRequest = 2 * streamReceiveWindow
	)
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "*."+testZone)
	certs, err := LoadPair(certFile, keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	fn := newFakeNet()
	// api's instance reads the whole body and answers its length.
	handlerAt(t, fn, "fd00::2", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, strconv.FormatInt(n, 10))
	}))
	p := New(Config{PendingHold: time.Minute}, Deps{Dial: fn.dial})
	p.SetMeshHealth(map[string]bool{"n1": true})
	pending := svc("web")
	pending.Pending = 1
	p.SetState(testState(pending, svc("api", ep("a1", "fd00::2", "n1"))))
	srv := p.HTTPSServer(certs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer func() {
		_ = srv.Close()
		p.CloseUpstreams()
	}()

	h := dialH2(t, ln.Addr().String())
	if h.connWindow != connReceiveWindow || h.streamWindow != streamReceiveWindow {
		t.Fatalf("connection window %d, stream window %d; want %d, %d", h.connWindow, h.streamWindow,
			connReceiveWindow, streamReceiveWindow)
	}
	// POST /upload, to the pending web: held.
	block := postBlock("/upload")
	for i := range held {
		h.write(t, h2Frame(h2Headers, h2FlagEndHdrs, uint32(2*i+1), block))
	}
	eventually(t, "the requests held", func() bool { return p.held.count() == held })
	before := liveMemory()
	for i := range held {
		h.body(t, uint32(2*i+1), min(h.streamWindow, h.connWindow), false)
	}
	h.ping(t)
	grew := liveMemory() - before
	if h.resets != 0 {
		t.Fatalf("the server reset a stream or the connection")
	}
	if got := p.held.count(); got != held {
		t.Fatalf("%d requests held after their bodies, want %d", got, held)
	}
	t.Logf("%d held requests on one connection sent %d KiB of body each: %+d KiB, %d KiB per request", held,
		streamReceiveWindow>>10, grew>>10, grew/held>>10)
	if grew > held*perRequest {
		t.Errorf("held requests took %d KiB each for their bodies, more than %d KiB", grew/held>>10, perRequest>>10)
	}
	if h.connWindow < h.streamWindow {
		t.Fatalf("the held requests' bodies left %d bytes of the connection's window, less than a stream's %d",
			h.connWindow, h.streamWindow)
	}

	// POST /api/upload, to api, sends 1 MiB as the server grants it.
	id := uint32(2*held + 1)
	h.write(t, h2Frame(h2Headers, h2FlagEndHdrs, id, postBlock("/api/upload")))
	window, left := h.streamWindow, upload
	var status, answer []byte
	for done := false; !done; {
		if k := min(left, window, h.connWindow); k > 0 {
			h.body(t, id, k, k == left)
			left -= k
			window -= k
			continue
		}
		typ, flags, stream, payload := h.frame(t)
		if stream != id {
			continue
		}
		switch typ {
		case h2WindowUpdate:
			window += int(binary.BigEndian.Uint32(payload) & 0x7fffffff)
		case h2Headers:
			status = payload
		case h2Data:
			answer = append(answer, payload...)
			done = flags&h2FlagEndStream != 0
		case h2RST:
			t.Fatalf("the server reset the upload")
		}
	}
	// :status 200 is entry 8 of HPACK's static table.
	if len(status) == 0 || status[0] != 0x88 || string(answer) != strconv.Itoa(upload) {
		t.Errorf("upload: header block %x, answer %q; want status 200 and %d", status, answer, upload)
	}
	if got := p.held.count(); got != held {
		t.Errorf("%d requests held after the upload, want %d", got, held)
	}
}

// F1: what a request keeps of its headers is bounded: more than 100
// fields is 431, more than 32 KiB is 431 from the server itself, and
// trailers are refused, since HTTP/2 would keep up to 900 fields of them
// while the request is held.
func TestHeaderLimits(t *testing.T) {
	fn := newFakeNet()
	upstream(t, fn, "web", "fd00::1")
	p, srv := newTestProxy(t, Config{}, Deps{}, fn, "n1")
	p.SetState(testState(svc("web", ep("w1", "fd00::1", "n1"))))
	withFields := func(k int) result {
		req, _ := http.NewRequest("GET", srv.URL+"/", nil)
		req.Host = testHost
		// Go's client adds User-Agent and Accept-Encoding.
		for i := range k - 2 {
			req.Header.Set("X-F"+strconv.Itoa(i), "v")
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return result{status: res.StatusCode, body: string(b)}
	}
	decode(t, withFields(maxHeaderFields))
	wantPlain(t, withFields(maxHeaderFields+1), http.StatusRequestHeaderFieldsTooLarge, textTooManyFields)

	req, _ := http.NewRequest("POST", srv.URL+"/", io.MultiReader(strings.NewReader("body")))
	req.Host = testHost
	req.Trailer = http.Header{"X-Checksum": nil}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	wantPlain(t, result{status: res.StatusCode, body: string(b)}, http.StatusBadRequest, textTrailers)
	if p.m.tooManyFields.Load() != 1 || p.m.trailers.Load() != 1 {
		t.Errorf("rejected for fields %d, for trailers %d", p.m.tooManyFields.Load(), p.m.trailers.Load())
	}

	// The HTTPS server: HTTP/1.1 headers beyond 32 KiB, and trailers
	// over HTTP/2.
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "*."+testZone)
	certs, err := LoadPair(certFile, keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	hs := p.HTTPSServer(certs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = hs.ServeTLS(ln, "", "") }()
	defer hs.Close()
	base := "https://" + ln.Addr().String() + "/"
	big, _ := http.NewRequest("GET", base, nil)
	// Go's server lets 4 KiB more through.
	big.Header.Set("X-Big", strings.Repeat("x", maxHeaderBytes+4<<10))
	trailers, _ := http.NewRequest("POST", base, strings.NewReader("body"))
	trailers.Trailer = http.Header{"X-Checksum": nil}
	for _, c := range []struct {
		req    *http.Request
		proto  int
		status int
	}{
		{big, 1, http.StatusRequestHeaderFieldsTooLarge},
		{trailers, 2, http.StatusBadRequest},
	} {
		var protos http.Protocols
		protos.SetHTTP1(c.proto == 1)
		protos.SetHTTP2(c.proto == 2)
		client := &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: testHost}, Protocols: &protos}}
		c.req.Host = testHost
		res, err := client.Do(c.req)
		if err != nil {
			t.Fatalf("HTTP/%d: %v", c.proto, err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != c.status || res.ProtoMajor != c.proto {
			t.Errorf("HTTP/%d: %s %d %q, want %d", c.proto, res.Proto, res.StatusCode, b, c.status)
		}
	}
	if p.m.trailers.Load() != 2 {
		t.Errorf("rejected for trailers %d, want 2", p.m.trailers.Load())
	}
}
