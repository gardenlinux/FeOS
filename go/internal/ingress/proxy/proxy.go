// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// Defaults of Config's fields left zero (the flags' defaults, §7.1).
const (
	DefaultWakeHold            = 2 * time.Minute
	DefaultPendingHold         = 10 * time.Second
	DefaultMaxHeld             = 1024
	DefaultDialTimeout         = 3 * time.Second
	DefaultIdleTimeout         = 4 * time.Second
	DefaultMaxBody             = 64 << 20
	DefaultMaxConnsPerEndpoint = 256
)

// Defaults of the limits that keep one tenant from taking the instance:
// a share per tenant of the held and of the forwarded requests, a
// quarter of the cap, so that it takes four tenants to fill it, and
// within it a share per environment, an eighth; caps that fit the
// instance's memory limit (a held request takes at most about 250 KiB,
// HTTP/2 with its body's window, streamReceiveWindow, full and its path
// and headers at their bound, a forwarded one about 350 KiB, the same to
// an instance that reads nothing; so the 1024 held take about 250 MiB
// and the 2048 forwarded about 700 MiB, together about 950 MiB of the
// 1.5 GiB on Kubernetes, beside the client connections); and the bound
// of the wait for a connection to an instance.
const (
	DefaultMaxHeldPerTenant     = DefaultMaxHeld / 4
	DefaultMaxHeldPerEnv        = DefaultMaxHeld / 8
	DefaultMaxInflight          = 2048
	DefaultMaxInflightPerTenant = DefaultMaxInflight / 4
	DefaultMaxInflightPerEnv    = DefaultMaxInflight / 8
	DefaultConnWaitTimeout      = 10 * time.Second
)

// The servers' limits (D14). There is no write timeout and no timeout
// for an upstream's response headers: responses stream, and long polls
// wait.
const (
	readHeaderTimeout = 10 * time.Second
	serverIdleTimeout = 120 * time.Second
	// maxHeaderBytes bounds a request's headers (HTTP/1.1) or its header
	// list (HTTP/2, and the trailers there), maxHeaderFields the number
	// of its header fields. A request keeps its headers while it is held
	// or forwarded, and many small fields take far more memory than
	// their bytes: Go's default of 1 MiB let one request take several
	// MiB. Beyond, 431.
	maxHeaderBytes  = 32 << 10
	maxHeaderFields = 100
	// cooldown is how long an endpoint whose dial failed is not used.
	cooldown = 5 * time.Second
	// maxConcurrentStreams bounds the requests of one HTTP/2 connection
	// (Go's default is 250). 63, so that connReceiveWindow, every
	// stream's window, stays within what net/http documents as valid for
	// a connection's window (less than 4 MiB).
	maxConcurrentStreams = 63
	// streamReceiveWindow is how much of a request's body an HTTP/2
	// client may send before the request reads it (Go's default is 1
	// MiB). A held request reads none, nor does one whose instance reads
	// nothing, and the server keeps what came: with 1 MiB the default
	// caps could pin over 3 GiB. 64 KiB is HTTP/2's own initial window
	// and the least that works, since a client may send that much before
	// it has the server's settings. The smaller window slows HTTP/2
	// uploads over long round trips: 64 KiB per round trip, 5 Mbit/s at
	// 100 ms; HTTP/1.1 uploads keep TCP's window.
	streamReceiveWindow = 64 << 10
	// connReceiveWindow is how much of the bodies of one HTTP/2
	// connection's requests may have come unread together: every
	// stream's window, so that a connection's held requests never stall
	// its other uploads. A browser may carry every app under the zone on
	// one connection (one wildcard certificate, one address); with Go's
	// default of 1 MiB, 16 requests held there that sent their 64 KiB
	// stopped every upload on it until they ended. It adds no memory:
	// streamReceiveWindow bounds each request's part, and the caps count
	// the requests.
	connReceiveWindow = maxConcurrentStreams * streamReceiveWindow
	// endpointGrace is how long the connections to an instance that left
	// a service which stays routed live on (a replica stops, or is no
	// longer ready): a stopping pod goes after 10 s, and its requests
	// end with it. Connections to what is no longer routed at all close
	// at once.
	endpointGrace = 30 * time.Second
)

// Config is the proxy's settings. A zero field takes its default, except
// MaxBody: 0 means no limit.
type Config struct {
	// WakeHold bounds a request held while instances of its service are
	// asleep or waking; PendingHold one held while they are only
	// pending. Both are measured from the request's start.
	WakeHold, PendingHold time.Duration
	// MaxHeld is how many requests may be held at once, MaxHeldPerTenant
	// how many of them one tenant's, MaxHeldPerEnv how many one
	// environment's.
	MaxHeld, MaxHeldPerTenant, MaxHeldPerEnv int
	// MaxInflight is how many requests may be forwarded at once, waiting
	// for a connection to an instance or for its answer, upgraded ones
	// included; MaxInflightPerTenant how many of them one tenant's,
	// MaxInflightPerEnv how many one environment's.
	MaxInflight, MaxInflightPerTenant, MaxInflightPerEnv int
	// ConnWaitTimeout bounds each wait for a connection to an instance,
	// the dial included; the answer's time is not bounded.
	ConnWaitTimeout time.Duration
	// DialTimeout bounds a dial to an instance; IdleTimeout closes a
	// pooled connection that was idle that long. Keep IdleTimeout below
	// the upstreams' keep-alive timeouts, so that the ingress closes
	// first.
	DialTimeout, IdleTimeout time.Duration
	// MaxBody is the largest request body in bytes (413 beyond).
	MaxBody int64
	// MaxConnsPerEndpoint bounds the connections to one instance's
	// port; requests beyond wait for one.
	MaxConnsPerEndpoint int
}

func (c Config) withDefaults() Config {
	if c.WakeHold <= 0 {
		c.WakeHold = DefaultWakeHold
	}
	if c.PendingHold <= 0 {
		c.PendingHold = DefaultPendingHold
	}
	if c.MaxHeld <= 0 {
		c.MaxHeld = DefaultMaxHeld
	}
	if c.MaxHeldPerTenant <= 0 {
		c.MaxHeldPerTenant = DefaultMaxHeldPerTenant
	}
	if c.MaxHeldPerEnv <= 0 {
		c.MaxHeldPerEnv = DefaultMaxHeldPerEnv
	}
	if c.MaxInflight <= 0 {
		c.MaxInflight = DefaultMaxInflight
	}
	if c.MaxInflightPerTenant <= 0 {
		c.MaxInflightPerTenant = DefaultMaxInflightPerTenant
	}
	if c.MaxInflightPerEnv <= 0 {
		c.MaxInflightPerEnv = DefaultMaxInflightPerEnv
	}
	if c.ConnWaitTimeout <= 0 {
		c.ConnWaitTimeout = DefaultConnWaitTimeout
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.MaxBody < 0 {
		c.MaxBody = 0
	}
	if c.MaxConnsPerEndpoint <= 0 {
		c.MaxConnsPerEndpoint = DefaultMaxConnsPerEndpoint
	}
	return c
}

// Waker asks the control plane to wake the asleep instances of a service
// of an environment (its ID). It must not block: the agent sends one
// call per service at a time and drops it while it is not connected.
type Waker interface {
	Wake(env, service string)
}

// Holder holds and releases instances at the control plane (the agent's
// Hold).
type Holder interface {
	Hold(ctx context.Context, req wire.HoldRequest) (wire.HoldResponse, error)
}

// Visits are the calls about visits at the control plane (ADR 0054; the
// agent's). An error is no answer: one whose gRPC code (status.Code) is
// Unavailable, Unimplemented or DeadlineExceeded means the control plane
// is away, which lets a visit's token count past its end until the
// visit's end; any other, a local busy (ResourceExhausted) among them,
// does not.
type Visits interface {
	RedeemVisit(ctx context.Context, req wire.RedeemVisitRequest) (wire.RedeemVisitResponse, error)
	RenewVisit(ctx context.Context, req wire.RenewVisitRequest) (wire.RenewVisitResponse, error)
	EndVisit(ctx context.Context, req wire.EndVisitRequest) (wire.EndVisitResponse, error)
}

// Deps is what the proxy uses. Without a Waker nothing is woken; without
// a Holder no instance is held; without an AccessLog nothing is
// recorded; without Visits a guarded environment serves nobody. Dial
// defaults to a net.Dialer; tests map endpoint addresses with it.
type Deps struct {
	Waker  Waker
	Holder Holder
	// Visits redeems, renews and ends visits; nil: guarded environments
	// serve nobody.
	Visits Visits
	// AccessLog takes a record of each request routed to an environment.
	AccessLog AccessLog
	Dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	Log       *slog.Logger
}

// DataPath is what the routing offers, for readiness: the nodes that run
// endpoints, those of them with a fresh handshake whose group version
// admits this ingress, and the number of endpoints.
type DataPath struct {
	EndpointNodes      []string `json:"endpointNodes"`
	FreshAdmittedNodes []string `json:"freshAdmittedNodes"`
	Endpoints          int      `json:"endpoints"`
}

// Proxy is the ingress's data path.
type Proxy struct {
	cfg    Config
	d      Deps
	log    *slog.Logger
	errLog *log.Logger
	rl     *rateLog
	m      metrics
	up     *upstreams
	holds  *holds
	// cool is how long an endpoint whose dial failed is not used.
	cool time.Duration
	// grace is endpointGrace (tests shorten it).
	grace time.Duration

	cur atomic.Pointer[view]
	// mu serializes SetState and SetMeshHealth and guards the fields
	// below.
	mu       sync.Mutex
	skipped  map[string]bool
	cooling  map[string]time.Time
	stateAt  time.Time
	cookieAt map[string]time.Time

	held      *budget
	inflight  *budget
	drain     chan struct{}
	drainOnce sync.Once

	// visits are what the proxy knows of visits (visits.go), redeems
	// the codes it redeemed (ferunpath.go).
	visits  *visitBook
	redeems *redeemBook
}

// view is the routing and the mesh's health as one value. changed is
// closed when a newer view replaces it.
type view struct {
	t       *table
	fresh   map[string]bool
	changed chan struct{}
}

// New returns a proxy without a routing: it answers 503 until the first
// SetState.
func New(cfg Config, d Deps) *Proxy {
	cfg = cfg.withDefaults()
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	p := &Proxy{
		cfg:      cfg,
		d:        d,
		log:      d.Log,
		errLog:   slog.NewLogLogger(d.Log.Handler(), slog.LevelDebug),
		rl:       newRateLog(d.Log),
		cool:     cooldown,
		grace:    endpointGrace,
		skipped:  map[string]bool{},
		cooling:  map[string]time.Time{},
		cookieAt: map[string]time.Time{},
		held:     newBudget(cfg.MaxHeldPerEnv, cfg.MaxHeldPerTenant, cfg.MaxHeld),
		inflight: newBudget(cfg.MaxInflightPerEnv, cfg.MaxInflightPerTenant, cfg.MaxInflight),
		drain:    make(chan struct{}),
	}
	p.up = newUpstreams(cfg, d.Dial, p.route)
	p.holds = newHolds(d.Holder, d.Log, &p.m)
	p.visits = newVisitBook()
	p.redeems = newRedeemBook()
	p.cur.Store(&view{changed: make(chan struct{})})
	return p
}

// SetState replaces the routing with the table of s. A state whose
// environments, Self, challenges and visits' keys did not change (a
// heartbeat) keeps the routing. A change forgets the instances whose
// hold was refused and lets held requests pick again; when endpoints
// left, the pool's idle connections are closed, and so are the open ones
// to what is no longer routed (cutConns). The live connections of visits
// whose environment is gone or admits another provider now are closed
// (visitBook.retable).
func (p *Proxy) SetState(s *wire.State) {
	if s == nil {
		return
	}
	p.mu.Lock()
	p.stateAt = time.Now()
	old := p.cur.Load()
	if old.t != nil && old.t.src.Self == s.Self && reflect.DeepEqual(old.t.src.Envs, s.Envs) &&
		slices.Equal(old.t.src.Challenges, s.Challenges) && reflect.DeepEqual(old.t.src.VisitKeys, s.VisitKeys) {
		p.mu.Unlock()
		return
	}
	t := buildTable(s, p.log, old.t)
	p.cur.Store(&view{t: t, fresh: old.fresh, changed: make(chan struct{})})
	clear(p.skipped)
	left := false
	if old.t != nil {
		for d := range old.t.dials {
			if _, ok := t.dials[d]; !ok {
				left = true
				break
			}
		}
	}
	p.mu.Unlock()
	close(old.changed)
	if left {
		p.up.closeIdle()
		p.cutConns(t, time.Now())
	}
	if n := p.visits.retable(t); n > 0 {
		p.log.Info("closed the connections of visits whose environment no longer admits them", "attempts", n)
	}
}

// route tells what a dial address reaches in the current table.
func (p *Proxy) route(addr string) (dialTarget, bool) {
	t := p.cur.Load().t
	if t == nil {
		return dialTarget{}, false
	}
	to, ok := t.dials[addr]
	return to, ok
}

// cutConns closes the open connections, upgraded ones and streaming
// answers included, to instances that the table t does not route to: at
// once when what they reached is no longer routed (the environment was
// unpublished or deleted, the service lost its routes, or the instance
// its port), so that unpublishing ends every way in; after the grace
// when only the instance left a service that stays routed, so that the
// requests of a stopping replica end with its pod, not before.
func (p *Proxy) cutConns(t *table, now time.Time) {
	n := p.up.closeIf(func(c *trackedConn) bool {
		if _, ok := t.dials[c.addr]; ok {
			c.leftAt = time.Time{}
			return false
		}
		if !t.addrs[c.to.address] && t.routed[envService(c.to.env, c.to.service)] {
			if c.leftAt.IsZero() {
				c.leftAt = now
			}
			return now.Sub(c.leftAt) >= p.grace
		}
		return true
	})
	if n > 0 {
		p.m.connsCut.Add(uint64(n))
		p.log.Info("closed connections to instances the table no longer routes to", "connections", n)
	}
}

// SetMeshHealth sets which nodes the ingress has a fresh WireGuard
// handshake with, by name. A change lets held requests pick again.
func (p *Proxy) SetMeshHealth(fresh map[string]bool) {
	p.mu.Lock()
	old := p.cur.Load()
	if maps.Equal(old.fresh, fresh) {
		p.mu.Unlock()
		return
	}
	p.cur.Store(&view{t: old.t, fresh: maps.Clone(fresh), changed: make(chan struct{})})
	p.mu.Unlock()
	close(old.changed)
}

// Ready reports whether the proxy has a routing.
func (p *Proxy) Ready() error {
	if p.cur.Load().t == nil {
		return errors.New(textNoTable)
	}
	return nil
}

// Drain ends every held request with 503 and Retry-After 1, holds no
// request from now on and stops admitting idle holds. Requests that are
// forwarded go on.
func (p *Proxy) Drain() {
	p.drainOnce.Do(func() { close(p.drain) })
	p.holds.drain()
}

func (p *Proxy) draining() bool {
	select {
	case <-p.drain:
		return true
	default:
		return false
	}
}

// Held lists the instances the ingress holds validly at the control
// plane, sorted.
func (p *Proxy) Held() []string {
	return p.holds.held()
}

// CloseUpstreams closes every connection to the instances, idle or not:
// upgraded connections and long polls end.
func (p *Proxy) CloseUpstreams() {
	p.up.closeAll()
}

// Run renews and releases the idle holds every 10 s until ctx ends, and
// closes the connections to instances that left the table longer ago
// than the grace. Beside it, every 10 s, the visits with live
// connections on this instance are renewed when their token nears its
// end, and those that ended are closed (renewLive).
func (p *Proxy) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.visitLoop(ctx)
	}()
	defer wg.Wait()
	t := time.NewTicker(holdEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.holds.pass(ctx)
			p.prune()
			if tb := p.cur.Load().t; tb != nil {
				p.cutConns(tb, time.Now())
			}
		}
	}
}

// prune forgets cooldowns that ended and cookie log times older than a
// minute.
func (p *Proxy) prune() {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	maps.DeleteFunc(p.cooling, func(_ string, until time.Time) bool { return !now.Before(until) })
	maps.DeleteFunc(p.cookieAt, func(_ string, at time.Time) bool { return now.Sub(at) >= time.Minute })
}

// DataPath returns what the routing offers now.
func (p *Proxy) DataPath() DataPath {
	v := p.cur.Load()
	var dp DataPath
	if v.t == nil {
		return dp
	}
	nodes := map[string]bool{}
	seen := map[*envRoutes]bool{}
	for _, e := range v.t.hosts {
		if seen[e] {
			continue
		}
		seen[e] = true
		for _, svc := range e.services {
			for _, ep := range svc.endpoints {
				dp.Endpoints++
				admitted := ep.groupVersion >= v.t.admitted && v.fresh[ep.node]
				nodes[ep.node] = nodes[ep.node] || admitted
			}
		}
	}
	for _, n := range slices.Sorted(maps.Keys(nodes)) {
		dp.EndpointNodes = append(dp.EndpointNodes, n)
		if nodes[n] {
			dp.FreshAdmittedNodes = append(dp.FreshAdmittedNodes, n)
		}
	}
	return dp
}

// ServeHTTPS is the handler of the HTTPS listener.
func (p *Proxy) ServeHTTPS() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		defer func() { p.m.request(rec.class()) }()
		p.serve(rec, r)
	})
}

// ServeHTTPRedirect is the handler of the HTTP listener: ACME's HTTP-01
// challenges first (answerChallenge), on every host; then 308 to https
// for a known host, 404 for any other.
func (p *Proxy) ServeHTTPRedirect() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		defer func() { p.m.request(rec.class()) }()
		host := canonicalHost(r.Host)
		if strings.HasPrefix(r.URL.Path, challengePath) {
			p.answerChallenge(rec, r, host)
			return
		}
		if t := p.cur.Load().t; t == nil || t.hosts[host] == nil {
			reply{http.StatusNotFound, textNoHost, 0}.write(rec)
			return
		}
		http.Redirect(rec, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

// challengePath is the path under which ACME's HTTP-01 challenges are
// asked for (RFC 8555 §8.3).
const challengePath = "/.well-known/acme-challenge/"

// textNoChallenge answers a challenge the table does not hold for the
// request's host.
const textNoChallenge = "no such challenge"

// answerChallenge answers GET (and HEAD) /.well-known/acme-challenge/<token>
// with the challenge's key authorization, text/plain, when the table
// holds the token for the request's host; anything else under the path
// is 404.
func (p *Proxy) answerChallenge(w http.ResponseWriter, r *http.Request, host string) {
	token := strings.TrimPrefix(r.URL.Path, challengePath)
	c, ok := p.cur.Load().t.challenge(token)
	if !ok || canonicalHost(c.Host) != host || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		p.m.acmeUnknown.Add(1)
		reply{http.StatusNotFound, textNoChallenge, 0}.write(w)
		return
	}
	p.m.acmeAnswered.Add(1)
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", strconv.Itoa(len(c.KeyAuth)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, c.KeyAuth)
}

// GetCertificate is the HTTPS listener's choice of certificate: by SNI,
// a domain's own from the table, else the first of zones (the tenant
// zone's wildcard, ADR 0031) that covers the name, else the operator's
// wildcard of c, the apps zone's.
func (p *Proxy) GetCertificate(c *Certs, zones ...*Certs) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello.ServerName != "" {
			name := canonicalHost(hello.ServerName)
			if cert := p.cur.Load().t.cert(name); cert != nil {
				return cert, nil
			}
			for _, z := range zones {
				if z != nil && z.covers(name) {
					return z.GetCertificate(hello)
				}
			}
		}
		return c.GetCertificate(hello)
	}
}

// HTTPSServer returns the HTTPS server with its limits, serving
// ServeHTTPS with the domains' certificates of the table by SNI, else a
// certificate of zones that covers the name, else the certificate of c
// (GetCertificate). The caller sets the address or serves a listener
// (ServeTLS with empty file names).
func (p *Proxy) HTTPSServer(c *Certs, zones ...*Certs) *http.Server {
	tc := c.TLSConfig()
	tc.GetCertificate = p.GetCertificate(c, zones...)
	return &http.Server{
		Handler:           p.ServeHTTPS(),
		TLSConfig:         tc,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams:          maxConcurrentStreams,
			MaxReceiveBufferPerStream:     streamReceiveWindow,
			MaxReceiveBufferPerConnection: connReceiveWindow,
		},
		ErrorLog: p.errLog,
	}
}

// HTTPServer returns the HTTP server with its limits, serving
// ServeHTTPRedirect.
func (p *Proxy) HTTPServer() *http.Server {
	return &http.Server{
		Handler:           p.ServeHTTPRedirect(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          p.errLog,
	}
}

// request is one request on its way through the proxy.
type request struct {
	p    *Proxy
	w    *recorder
	r    *http.Request
	host string
	raw  string
	body *body
	wait waiter
	// inflight is its place among the forwarded requests.
	inflight slot
	// trace is its server span (W3C Trace Context); env, its tenant and
	// service are where it was routed, "" before; instance is the last attempt's,
	// attempts counts them, held is how long it waited for an instance.
	// reason says why the proxy answered it itself, "" when it did not
	// (wire.AccessLogEntry.Reason). They make its access log record.
	trace    trace
	env      string
	tenant   string
	service  string
	instance string
	attempts int
	held     time.Duration
	reason   string
	// client is the client's address, the connection's peer (clientAddr).
	client netip.Addr
	// visit is the request's visit, which passed the visit check of a
	// guarded environment; nil on any other request (visits.go).
	visit *visitReq
}

// serve routes a request, holds it while no endpoint is usable, and
// forwards it, once more to another endpoint when that is safe.
func (p *Proxy) serve(w *recorder, r *http.Request) {
	if r.Method == http.MethodConnect {
		reply{http.StatusMethodNotAllowed, textConnect, 0}.write(w)
		return
	}
	if headerFields(r.Header) > maxHeaderFields {
		p.m.tooManyFields.Add(1)
		reply{http.StatusRequestHeaderFieldsTooLarge, textTooManyFields, 0}.write(w)
		return
	}
	// HTTP/2 keeps every field of the trailers a request announced until
	// its body is read, past maxHeaderFields: a held request would keep
	// up to 900 of them. Browsers send no request trailers.
	if r.Trailer != nil {
		p.m.trailers.Add(1)
		reply{http.StatusBadRequest, textTrailers, 0}.write(w)
		return
	}
	q := &request{p: p, w: w, r: r, host: canonicalHost(r.Host), raw: r.URL.EscapedPath(),
		wait: waiter{start: time.Now()}}
	defer p.leave(&q.wait)
	defer p.inflight.leave(&q.inflight)
	v := p.cur.Load()
	q.client = clientAddr(r)
	if e := v.t.env(q.host); e != nil {
		if sub, ok := ferunPath(q.raw); ok {
			q.trace = newTrace(r.Header)
			defer q.record()
			q.ferun(v, e, sub)
			return
		}
	}
	tg, rep := v.t.route(q.host, q.raw, q.client)
	if rep != nil && tg.env == nil {
		rep.write(w)
		return
	}
	q.trace = newTrace(r.Header)
	defer q.record()
	if rep != nil {
		q.refused(tg, rep)
		return
	}
	if !q.checkVisit(v, tg) {
		return
	}
	if tg, rep = v.t.bind(tg, q.raw); rep != nil {
		rep.write(w)
		return
	}
	q.env, q.tenant, q.service = tg.env.id, tg.env.tenant, tg.svc.name
	if r.ContentLength != 0 && r.Body != nil && r.Body != http.NoBody {
		if p.cfg.MaxBody > 0 && r.ContentLength > p.cfg.MaxBody {
			q.tooLarge()
			return
		}
		q.body = &body{rc: r.Body, limit: p.cfg.MaxBody}
		q.r = r.WithContext(r.Context())
		q.r.Body = q.body
	}
	q.run(v, tg)
}

// headerFields counts the fields of h, every value of a name.
func headerFields(h http.Header) int {
	n := 0
	for _, vv := range h {
		n += len(vv)
	}
	return n
}

// refused answers a request whose route does not admit its client (rep,
// 403, from resolve), before any instance is woken or held, and notes
// that in its access log record.
func (q *request) refused(tg target, rep *reply) {
	q.p.m.refusedClient.Add(1)
	q.env, q.tenant, q.service, q.reason = tg.env.id, tg.env.tenant, tg.route.Service, wire.ReasonRouteRefusedClient
	rep.write(q.w)
}

func (q *request) tooLarge() {
	q.p.m.tooLarge.Add(1)
	reply{http.StatusRequestEntityTooLarge,
		fmt.Sprintf("the request body is larger than %d bytes", q.p.cfg.MaxBody), 0}.write(q.w)
}

// run picks an endpoint and forwards to it until the request is
// answered: an instance whose hold is refused is skipped, a failed
// attempt that may be repeated goes to another endpoint once, and while
// none is usable the request is held. A request counts among the
// forwarded ones from its first pick to its end.
func (q *request) run(v *view, tg target) {
	p := q.p
	var exclude map[string]bool
	retried := false
	for {
		if ep, port, info := p.pick(v, tg, exclude); ep != nil {
			p.leave(&q.wait)
			if !q.admit(tg) {
				return
			}
			release, refused := p.acquire(q.r.Context(), tg.svc, ep)
			if !refused {
				if !q.forward(v, tg, ep, port, exclude, !retried, release) {
					return
				}
				p.m.retries.Add(1)
				retried = true
				if exclude == nil {
					exclude = map[string]bool{}
				}
				exclude[ep.instance] = true
			} else {
				p.skip(ep.instance)
			}
		} else {
			t0 := time.Now()
			goOn := q.hold(v, tg, info)
			q.held += time.Since(t0)
			if !goOn {
				return
			}
		}
		v = p.cur.Load()
		var rep *reply
		if tg, rep = v.t.route(q.host, q.raw, q.client); rep != nil {
			if tg.env != nil {
				q.refused(tg, rep)
				return
			}
			rep.write(q.w)
			return
		}
		// The environment may have turned guarded, or admit another
		// provider, while the request was held.
		if !q.checkVisit(v, tg) {
			return
		}
		if tg, rep = v.t.bind(tg, q.raw); rep != nil {
			rep.write(q.w)
			return
		}
		q.service = tg.svc.name
	}
}

// admit counts the request among the forwarded ones: against its
// environment's share, MaxInflightPerEnv, its tenant's,
// MaxInflightPerTenant, and MaxInflight. It answers 503 and returns false
// when one is taken.
func (q *request) admit(tg target) bool {
	p := q.p
	switch p.inflight.enter(&q.inflight, tg.env.tenant, tg.env.id) {
	case envFull:
		p.m.rejectedInflightEnv.Add(1)
		p.rl.warn("inflight-env", "an environment's share of forwarded requests is taken",
			"host", q.host, "env", tg.env.id, "max", p.cfg.MaxInflightPerEnv)
		reply{http.StatusServiceUnavailable, textBusyHere, 5}.write(q.w)
		return false
	case tenantFull:
		p.m.rejectedInflightTenant.Add(1)
		p.rl.warn("inflight-tenant", "a tenant's share of forwarded requests is taken",
			"host", q.host, "tenant", tg.env.tenant, "max", p.cfg.MaxInflightPerTenant)
		reply{http.StatusServiceUnavailable, textBusyHere, 5}.write(q.w)
		return false
	case allFull:
		p.m.rejectedInflight.Add(1)
		p.rl.warn("inflight", "the instance's forwarded requests are at their cap",
			"host", q.host, "max", p.cfg.MaxInflight)
		reply{http.StatusServiceUnavailable, textBusy, 1}.write(q.w)
		return false
	}
	return true
}

// acquire holds an instance of a service with an idle policy before a
// request goes to it; other services need no hold.
func (p *Proxy) acquire(ctx context.Context, svc *service, ep *endpoint) (release func(), refused bool) {
	if !svc.idle || p.d.Holder == nil {
		return func() {}, false
	}
	return p.holds.acquire(ctx, ep.instance)
}

// skip leaves out an instance whose hold was refused until the routing
// changes.
func (p *Proxy) skip(instance string) {
	p.mu.Lock()
	p.skipped[instance] = true
	p.mu.Unlock()
}

// pickInfo is what a pick that found nothing usable saw: endpoints that
// serve once the routing changes (not admitted yet, without the route's
// port, or skipped until the routing changes), which count as pending,
// and the earliest end of a cooldown, when the held request picks again.
type pickInfo struct {
	waiting   int
	coolUntil time.Time
}

// pick chooses an endpoint of the target's service round robin among the
// usable ones, else among the last resort (a node without a fresh
// handshake), and returns it with the route's port on it.
func (p *Proxy) pick(v *view, tg target, exclude map[string]bool) (*endpoint, int, pickInfo) {
	eps, last, info := p.candidates(v, tg, exclude)
	if len(eps) == 0 {
		return nil, 0, info
	}
	if last {
		p.m.lastResort.Add(1)
	}
	ep := eps[(tg.svc.next.Add(1)-1)%uint64(len(eps))]
	return ep, ep.ports[tg.route.Port], info
}

// candidates returns the usable endpoints of the target's service, else
// the last resort ones (last true).
func (p *Proxy) candidates(v *view, tg target, exclude map[string]bool) (eps []*endpoint, last bool, info pickInfo) {
	now := time.Now()
	var stale []*endpoint
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range tg.svc.endpoints {
		ep := &tg.svc.endpoints[i]
		if exclude[ep.instance] {
			continue
		}
		if ep.ports[tg.route.Port] <= 0 || ep.groupVersion < v.t.admitted || p.skipped[ep.instance] {
			info.waiting++
			continue
		}
		if until, ok := p.cooling[ep.address]; ok && now.Before(until) {
			if info.coolUntil.IsZero() || until.Before(info.coolUntil) {
				info.coolUntil = until
			}
			continue
		}
		if v.fresh[ep.node] {
			eps = append(eps, ep)
		} else {
			stale = append(stale, ep)
		}
	}
	if len(eps) == 0 && len(stale) > 0 {
		return stale, true, info
	}
	return eps, false, info
}

// forward sends the request to one endpoint. It reports whether the
// attempt failed so that the request goes to another endpoint, which
// then answers; otherwise the request is answered.
//
// release ends the request's count at the hold manager when the attempt
// ends, also when it ends in a panic: ReverseProxy aborts a response
// whose body it cannot copy (the client went away mid-stream, or the
// upstream reset mid-body) with panic(http.ErrAbortHandler), which
// net/http recovers. Such a request counts as aborted.
//
// Each wait for a connection to the instance, the dial included, is
// bounded by ConnWaitTimeout (connWait): the transport queues a request
// beyond MaxConnsPerEndpoint without a bound of its own. What comes
// after, the answer, is not bounded.
func (q *request) forward(v *view, tg target, ep *endpoint, port int, exclude map[string]bool,
	mayRetry bool, release func()) (again bool) {
	p := q.p
	q.attempts++
	q.instance = ep.instance
	returned := false
	defer func() {
		release()
		if !returned {
			q.w.aborted = true
		}
	}()
	ctx, cancel := context.WithCancelCause(q.r.Context())
	defer cancel(nil)
	if q.visit != nil {
		// The visit's end closes the attempt, an upgraded connection
		// and a streaming answer among them.
		att := p.visits.attach(q.visit, tg.env, q.host, cancel)
		defer p.visits.detach(att)
	}
	cw := &connWait{bound: p.cfg.ConnWaitTimeout, cancel: cancel}
	defer cw.stop()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GetConn: cw.getConn, GotConn: cw.gotConn})
	// responded: a response came. ReverseProxy calls ErrorHandler after
	// one on its upgrade path only (another protocol than the one asked
	// for, or handing the connection over failed); the app saw the
	// request then, so it goes nowhere else.
	responded := false
	cookies := func(h http.Header) {
		dropOurSetCookies(h)
		if v.t.underZone(q.host) {
			if n := hostOnlyCookies(h, q.host); n > 0 {
				p.cookiesRewritten(q.host, n)
			}
		}
	}
	q.w.informational = cookies
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort(ep.address, strconv.Itoa(port))
			pr.Out.URL.Path = tg.path
			pr.Out.URL.RawPath = tg.rawPath
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			// The app sees the host the client named.
			pr.Out.Host = pr.In.Host
			setForwarded(pr)
			// The ingress's cookies and the visitor headers are its own,
			// on every host: the application sees neither a client's nor
			// the cookie, only the visit's headers.
			stripOurCookies(pr.Out.Header)
			setVisitor(pr.Out.Header, q.visit)
			q.trace.set(pr.Out.Header)
		},
		Transport:     p.up.tr,
		FlushInterval: -1, // event streams and long polls stream
		ModifyResponse: func(res *http.Response) error {
			responded = true
			cookies(res.Header)
			q.visitCookieOn(res.Header)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			again = q.failed(v, tg, ep, port, exclude, mayRetry, responded, cw.gaveUp(), err)
		},
		ErrorLog: p.errLog,
	}
	rp.ServeHTTP(q.w, q.r.WithContext(ctx))
	returned = true
	return again
}

// errConnWait ends an attempt that waited ConnWaitTimeout for a
// connection to the instance.
var errConnWait = errors.New("no connection to the instance came free in time")

// connWait bounds each wait of one attempt for a connection to the
// instance (httptrace's GetConn to GotConn). The transport waits once
// per try, and tries again by itself when a reused connection closed
// without an answer and the request may be repeated (a GET, or nothing
// of it was written), so each wait has its own bound. Giving up on a
// later wait counts as not sent, as the transport's own retry does.
type connWait struct {
	bound  time.Duration
	cancel context.CancelCauseFunc

	mu    sync.Mutex
	state connState
	// gen tells the current wait's timer from those of earlier waits.
	gen   int
	timer *time.Timer
}

// connState is where an attempt is in its wait for a connection.
type connState int

const (
	// connNone: no wait began.
	connNone connState = iota
	// connWaiting: it waits for a connection.
	connWaiting
	// connHave: it has one.
	connHave
	// connGaveUp: it gave up waiting.
	connGaveUp
	// connLate: one came after it gave up; the request may have been
	// written then, so it counts as sent.
	connLate
)

func (w *connWait) getConn(string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == connGaveUp || w.state == connLate {
		return
	}
	w.state = connWaiting
	w.gen++
	gen := w.gen
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(w.bound, func() { w.expire(gen) })
}

// expire gives up the wait gen if it still goes on.
func (w *connWait) expire(gen int) {
	w.mu.Lock()
	give := w.gen == gen && w.state == connWaiting
	if give {
		w.state = connGaveUp
	}
	w.mu.Unlock()
	if give {
		w.cancel(errConnWait)
	}
}

func (w *connWait) gotConn(httptrace.GotConnInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch w.state {
	case connWaiting:
		w.state = connHave
		w.timer.Stop()
	case connGaveUp:
		w.state = connLate
	}
}

// gaveUp reports whether the attempt ended because a wait for a
// connection ran out.
func (w *connWait) gaveUp() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state == connGaveUp
}

// stop ends the bound when the attempt ends.
func (w *connWait) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gen++
	if w.timer != nil {
		w.timer.Stop()
	}
}

// failed reads a failed attempt (D14). It reports whether the request
// goes to another endpoint: when the dial failed or no connection came
// free in time (waited), and nothing of the body was read, or when a
// GET, HEAD or OPTIONS without a body got no response, and another
// endpoint is usable; and always when the table no longer routes to the
// instance. Otherwise it answers. responded: a response came (a failed
// upgrade), so it answers.
func (q *request) failed(v *view, tg target, ep *endpoint, port int, exclude map[string]bool, mayRetry,
	responded, waited bool, err error) bool {
	p := q.p
	if responded {
		p.m.responseErrors.Add(1)
		p.rl.warn("upgrade", "upgrade failed", "host", q.host, "instance", ep.instance, "err", err)
		if q.w.hijacked || q.r.Context().Err() != nil {
			// The connection was handed over and is gone, or the client
			// went away: nothing more reaches the client.
			q.w.aborted = true
		} else {
			reply{http.StatusBadGateway, textBadUpgrade, 0}.write(q.w)
		}
		return false
	}
	if q.body.tooLarge() {
		q.tooLarge()
		return false
	}
	if q.r.Context().Err() != nil {
		q.w.aborted = true
		return false
	}
	if errors.Is(err, errNotRouted) {
		// The table dropped the instance after the request picked it:
		// nothing was sent, and the request routes again by the new
		// table, which may not serve it at all any more.
		return true
	}
	// sent: the attempt had a connection, so the request may have
	// reached the app. One that waited too long for a connection is busy,
	// not broken: it does not cool down.
	var de *dialError
	sent := !waited && !errors.As(err, &de)
	switch {
	case waited:
		p.m.waitErrors.Add(1)
	case sent:
		p.m.responseErrors.Add(1)
	default:
		p.m.dialErrors.Add(1)
		p.mu.Lock()
		p.cooling[ep.address] = time.Now().Add(p.cool)
		p.mu.Unlock()
	}
	repeatable := !sent && q.body.untouched() || sent && q.body == nil && idempotent(q.r.Method)
	if repeatable && mayRetry && p.alternative(v, tg, exclude, ep.instance) {
		return true
	}
	if waited {
		p.rl.warn("wait", "no connection to the instance came free in time", "host", q.host,
			"instance", ep.instance, "address", ep.address, "port", port, "wait", p.cfg.ConnWaitTimeout)
		reply{http.StatusServiceUnavailable, textNoConn, 5}.write(q.w)
		return false
	}
	p.rl.warn("forward", "forward failed", "host", q.host, "instance", ep.instance, "address", ep.address,
		"port", port, "err", err)
	if !sent && errors.Is(err, syscall.ECONNREFUSED) {
		p.m.refused.Add(1)
		reply{http.StatusBadGateway,
			fmt.Sprintf("the service refused the connection; it must listen on [::]:%d", port), 0}.write(q.w)
		return false
	}
	if !sent {
		// A dial that failed closes the client's connection: its next
		// request comes on a new one, which the balancer may send to
		// another instance of the ingress, one that may reach the node.
		reply{http.StatusBadGateway, textNoAnswer, 0}.writeClosing(q.w)
		return false
	}
	reply{http.StatusBadGateway, textNoAnswer, 0}.write(q.w)
	return false
}

// alternative reports whether an endpoint other than failed is usable
// or the last resort.
func (p *Proxy) alternative(v *view, tg target, exclude map[string]bool, failed string) bool {
	ex := maps.Clone(exclude)
	if ex == nil {
		ex = map[string]bool{}
	}
	ex[failed] = true
	eps, _, _ := p.candidates(v, tg, ex)
	return len(eps) > 0
}

// idempotent reports whether a request of the method may be sent again
// when it got no response.
func idempotent(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// reply is a plain answer of the proxy, with Retry-After in seconds when
// that is not 0.
type reply struct {
	status     int
	text       string
	retryAfter int
}

func (r reply) write(w http.ResponseWriter) {
	if r.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(r.retryAfter))
	}
	http.Error(w, r.text, r.status)
}

// writeClosing writes r and ends the client's connection after it:
// HTTP/1.1's Connection: close, a GOAWAY on HTTP/2.
func (r reply) writeClosing(w http.ResponseWriter) {
	w.Header().Set("Connection", "close")
	r.write(w)
}

// recorder notes a response's status and size for the metrics and the
// access log.
type recorder struct {
	http.ResponseWriter
	status int
	// hijacked: the connection was handed over for an upgrade; upgraded:
	// and the switch reached the client as far as the proxy knows.
	hijacked, upgraded bool
	aborted            bool
	// written counts the body's bytes written to the client.
	written int64
	// informational filters the header of a 1xx response that the
	// upstream sent before its final one (ReverseProxy passes those on
	// without ModifyResponse).
	informational func(http.Header)
	// final, when set, completes the header of the final response before
	// it is written: a renewed visit's cookie on the proxy's own answers
	// (an upgrade's 101 is written past it, from ModifyResponse's header).
	final func(http.Header)
}

func (r *recorder) WriteHeader(code int) {
	if code < http.StatusOK && r.informational != nil {
		r.informational(r.ResponseWriter.Header())
	}
	if code >= http.StatusOK && r.status == 0 && r.final != nil {
		r.final(r.ResponseWriter.Header())
	}
	if r.status == 0 && code >= http.StatusOK {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		if r.final != nil {
			r.final(r.ResponseWriter.Header())
		}
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController flush.
func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Hijack hands the connection over for an upgrade (ReverseProxy's, via
// http.ResponseController) and notes that it did.
func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.hijacked, r.upgraded = true, true
	}
	return c, brw, err
}

// class is the response's class for the metrics: its status class,
// "upgraded", or "aborted" when the client went away first or the
// response was cut off.
func (r *recorder) class() string {
	switch {
	case r.aborted:
		return "aborted"
	case r.upgraded:
		return "upgraded"
	case r.status == 0:
		return "2xx"
	}
	return strconv.Itoa(r.status/100) + "xx"
}

// rateLog logs at most one line per second per kind, counting what it
// suppressed: for lines a remote party can trigger.
type rateLog struct {
	log   *slog.Logger
	mu    sync.Mutex
	kinds map[string]*rateKind
}

type rateKind struct {
	last       time.Time
	suppressed int
}

func newRateLog(log *slog.Logger) *rateLog {
	return &rateLog{log: log, kinds: map[string]*rateKind{}}
}

func (r *rateLog) warn(kind, msg string, args ...any) {
	now := time.Now()
	r.mu.Lock()
	k := r.kinds[kind]
	if k == nil {
		k = &rateKind{}
		r.kinds[kind] = k
	} else if now.Sub(k.last) < time.Second {
		k.suppressed++
		r.mu.Unlock()
		return
	}
	n := k.suppressed
	k.last, k.suppressed = now, 0
	r.mu.Unlock()
	if n > 0 {
		args = append(args, "suppressed", n)
	}
	r.log.Warn(msg, args...)
}
