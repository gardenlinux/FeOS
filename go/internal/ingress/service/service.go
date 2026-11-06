// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/ingress/table"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Engine is what Wake needs of the sandbox engine (*sandbox.Engine).
type Engine interface {
	Ensure(ctx context.Context, id podid.ID) (model.Lease, error)
	Sync(ctx context.Context, id podid.ID) error
}

// Config is the ingress service's part of the instance's configuration.
type Config struct {
	InstanceID, TrustDomain string
	// AppsZone is the zone of public environments' default hosts
	// (--apps-zone); "" serves no host.
	AppsZone string
	// TenantZone is the zone of the default hosts of the tenants people
	// make (--tenant-zone, ADR 0031); "": none.
	TenantZone string
	// CertTTL is the lifetime of an ingress's certificates
	// (--node-cert-ttl); renewed at half of it.
	CertTTL time.Duration
	// SupersedeGrace is how long a certificate stays valid after a newer
	// one of the same ingress was presented in a Hello (10 minutes).
	SupersedeGrace time.Duration
	// PodNetwork is the cluster's ULA when its pods are on a mesh (ADR
	// 0017); the zero prefix when its network is routed, where no ingress
	// can say Hello.
	PodNetwork netip.Prefix
	Log        *slog.Logger
}

// Deps are the parts of the instance the ingress service works with. DB
// and Issuer are required; Store is made from DB when nil. Links is nil
// only where nothing is served (`ferund join-token`).
type Deps struct {
	DB     *db.DB
	Store  *store.Store
	Issuer *pki.Issuer
	Links  *nodelink.Server
	// Engine wakes asleep instances (Wake); nil: none is woken.
	Engine Engine
	// Builder keeps the ingress's table that Sync streams; Run runs it.
	// Nil: one on DB and Store for Config's zones.
	Builder *table.Builder
}

// Defaults of Config's durations when zero.
const (
	defaultCertTTL        = 168 * time.Hour
	defaultSupersedeGrace = 10 * time.Minute
)

// defaultRenewEarly is how long before a certificate's renew time a
// renewal from it is taken: the ingress renews at that time by its own
// clock, which may run ahead of the database's.
const defaultRenewEarly = 10 * time.Minute

// The service's fixed timings and limits.
const (
	// joinRetryWindow is how long a used join token answers again a Join
	// with the same key, whose answer was lost.
	joinRetryWindow = 10 * time.Minute
	// rpcTimeout bounds the database work of one unary call.
	rpcTimeout = 15 * time.Second
	// joinRate and joinBurst are Join's token bucket; joinDBSlots its
	// database slots, waited for at most joinWait.
	joinRate, joinBurst = 20, 20
	joinDBSlots         = 4
	joinWait            = 2 * time.Second
	// maxVersionLen bounds the version a Hello records.
	maxVersionLen = 128
)

// DefaultJoinTokenTTL is a join token's lifetime when none is given.
const DefaultJoinTokenTTL = time.Hour

// Service implements ferun.ingress.v1.IngressService and the ingress's
// part of the nodes' port.
type Service struct {
	wire.UnimplementedIngressServer

	cfg    Config
	d      *db.DB
	st     *store.Store
	iss    *pki.Issuer
	links  *nodelink.Server
	engine Engine
	tbl    *table.Builder
	log    *slog.Logger
	rl     *rateLog

	// heartbeat is how often a Sync stream sends an empty table and
	// records the ingress as seen (defaultHeartbeat); rowTimeout bounds a
	// stream's read of the ingress's row and the heartbeat's write of it
	// (defaultRowTimeout); holdTimeout bounds each statement of a Hold
	// (defaultHoldTimeout). Tests shorten them before the first call.
	heartbeat, rowTimeout, holdTimeout time.Duration
	// partBytes is the most a message of a Sync stream takes: a larger
	// table goes in parts (wire.PartBytes; tests make it smaller).
	partBytes int
	// renewEarly is how long before a certificate's renew time
	// RenewCertificate takes it (defaultRenewEarly; tests make it longer).
	renewEarly time.Duration

	// Join's own limits: a token bucket, and database slots waited for at
	// most joinSlotWait.
	joins        *tokenBucket
	joinSlots    chan struct{}
	joinSlotWait time.Duration

	// Background work (re-checks, the Hello registry's cleanup, wakes)
	// belongs to Run: it stops with Run's context. bgRunning: Run began.
	bg        context.Context
	bgCancel  context.CancelFunc
	bgMu      sync.Mutex
	bgRunning bool
	bgClosed  bool
	wg        sync.WaitGroup

	rechecks coalescer // per ingress name: one re-check pass on this instance at a time

	// hellos are the connections (nodelink.IngressPeer.Conn) on which a
	// Hello succeeded; a connection leaves when it closes.
	hellosMu sync.Mutex
	hellos   map[uint64]struct{}

	// stopping: BeginStopping was called (or Run ended); it is set under
	// streamsMu, so that no stream registers after it.
	stopping atomic.Bool
	// streams are the Sync streams on this instance, by ingress name.
	streamsMu sync.Mutex
	streams   map[string]map[*syncStream]struct{}

	// waking are the sandboxes a Wake is bringing up: one at a time each.
	wakingMu sync.Mutex
	waking   map[podid.ID]bool
}

var _ wire.IngressServer = (*Service)(nil)

// New returns the ingress service.
func New(cfg Config, deps Deps) *Service {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.CertTTL <= 0 {
		cfg.CertTTL = defaultCertTTL
	}
	if cfg.SupersedeGrace <= 0 {
		cfg.SupersedeGrace = defaultSupersedeGrace
	}
	log := cfg.Log.With("component", "ingress")
	st := deps.Store
	if st == nil {
		st = store.New(deps.DB)
	}
	tbl := deps.Builder
	if tbl == nil {
		tbl = table.NewBuilder(deps.DB, st, model.Zones{Apps: cfg.AppsZone, Tenant: cfg.TenantZone}, cfg.Log)
	}
	s := &Service{
		cfg: cfg, d: deps.DB, st: st, iss: deps.Issuer, links: deps.Links, engine: deps.Engine, tbl: tbl,
		log: log, rl: newRateLog(log),
		heartbeat: defaultHeartbeat, rowTimeout: defaultRowTimeout, holdTimeout: defaultHoldTimeout,
		partBytes:    wire.PartBytes,
		renewEarly:   defaultRenewEarly,
		joins:        newTokenBucket(joinRate, joinBurst),
		joinSlots:    make(chan struct{}, joinDBSlots),
		joinSlotWait: joinWait,
		hellos:       map[uint64]struct{}{},
		streams:      map[string]map[*syncStream]struct{}{},
		waking:       map[podid.ID]bool{},
	}
	s.bg, s.bgCancel = context.WithCancel(context.Background())
	return s
}

// Register registers IngressService on the nodes' port: on its ingress
// server, and on its ingress join server, which serves Join alone. Call it
// before the port serves.
func (s *Service) Register() {
	if s.links == nil {
		return
	}
	wire.RegisterIngressServer(s.links.Ingress(), s)
	wire.RegisterIngressServer(s.links.IngressJoin(), s)
}

// Run runs the table's builder and follows ferun_ingress until ctx ends:
// "ingress:<name>" re-checks the connections of that ingress on this
// instance (a join again, a removal, a supersession, a revocation) and
// tells the ingress's streams; "" (the listener reconnected) and "*"
// re-check every ingress connected here and tell every stream. The
// re-checks run in the background, one pass per name at a time; they and
// the wakes end with Run, and so do the streams (UNAVAILABLE, as at
// BeginStopping).
func (s *Service) Run(ctx context.Context) error {
	notes, unsubscribe := s.d.Subscribe(db.ChanIngress)
	defer unsubscribe()
	defer s.stop()
	s.bgMu.Lock()
	s.bgRunning = true
	s.bgMu.Unlock()
	built := make(chan struct{})
	go func() {
		defer close(built)
		if err := s.tbl.Run(ctx); err != nil {
			s.log.Error("the ingress's table stopped", "err", err)
		}
	}()
	defer func() { <-built }()
	prefix := store.IngressNamePayload("")
	for {
		select {
		case <-ctx.Done():
			return nil
		case p := <-notes:
			switch {
			case p == "" || p == store.IngressPayloadAll:
				if s.links != nil {
					for _, name := range s.links.IngressNames() {
						s.recheck(name)
					}
				}
				s.onEveryIngressChanged()
			case strings.HasPrefix(p, prefix) && len(p) > len(prefix):
				// The streams first: a removed ingress's stream then
				// mostly ends with its refusal before the re-check closes
				// the connection.
				name := strings.TrimPrefix(p, prefix)
				s.onIngressChanged(name)
				s.recheck(name)
			}
		}
	}
}

// recheck re-checks the connections of ingress name on this instance
// (nodelink's RecheckIngress), in the background, coalesced per name.
func (s *Service) recheck(name string) {
	if s.links == nil {
		return
	}
	s.coalesce(&s.rechecks, name, func(ctx context.Context) { s.links.RecheckIngress(ctx, name) })
}

// ingressPeer returns the ingress of a call on the ingress server, from
// its certificate.
func ingressPeer(ctx context.Context) (nodelink.IngressPeer, error) {
	p, ok := nodelink.IngressFromContext(ctx)
	if !ok || p.Name == "" {
		return nodelink.IngressPeer{}, status.Error(codes.Unauthenticated, "no ingress certificate on this connection")
	}
	return p, nil
}

// peerAddress is the remote address of the call's connection.
func peerAddress(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

// markHello records that a Hello succeeded on the connection of p, until
// the connection closes.
func (s *Service) markHello(p nodelink.IngressPeer) {
	s.hellosMu.Lock()
	_, known := s.hellos[p.Conn]
	s.hellos[p.Conn] = struct{}{}
	s.hellosMu.Unlock()
	if known || p.Done == nil {
		return
	}
	forget := func() {
		s.hellosMu.Lock()
		delete(s.hellos, p.Conn)
		s.hellosMu.Unlock()
	}
	if !s.goBackground(func(ctx context.Context) {
		select {
		case <-p.Done:
		case <-ctx.Done():
		}
		forget()
	}) {
		forget()
	}
}

// saidHello reports whether a Hello succeeded on the connection of p:
// what a Sync needs first.
func (s *Service) saidHello(p nodelink.IngressPeer) bool {
	s.hellosMu.Lock()
	defer s.hellosMu.Unlock()
	_, ok := s.hellos[p.Conn]
	return ok
}

// rpcError is what a call answers for err: a gRPC status as it is, the
// caller's cancellation as such, anything else (the database) as
// UNAVAILABLE, so that the ingress tries again, on this or another
// instance.
func rpcError(err error, what string) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok {
		return st.Err()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return status.Errorf(codes.Canceled, "%s: %v", what, err)
	case errors.Is(err, context.DeadlineExceeded):
		return status.Errorf(codes.DeadlineExceeded, "%s: %v", what, err)
	}
	return status.Errorf(codes.Unavailable, "%s: %v", what, err)
}

// dbError is what a call answers when a database step under its own
// timeout failed: once the call itself has ended (ctx, the caller's
// cancellation or deadline), what rpcError answers; while it goes on, a
// gRPC status as it is and anything else as UNAVAILABLE, the step's own
// timeout included, so that the ingress tries again and a stream keeps
// its last state (D18: only the table's health ends the streams).
func dbError(ctx context.Context, err error, what string) error {
	if err == nil || ctx.Err() != nil {
		return rpcError(err, what)
	}
	if _, ok := status.FromError(err); ok {
		return rpcError(err, what)
	}
	return status.Errorf(codes.Unavailable, "%s: %v", what, err)
}

// refused reports an error that is a gRPC status other than UNAVAILABLE:
// a decision, not a failure.
func refused(err error) (*status.Status, bool) {
	st, ok := status.FromError(err)
	if !ok || st.Code() == codes.Unavailable {
		return nil, false
	}
	return st, true
}

// goBackground runs f in a goroutine owned by Run; false once Run ended.
func (s *Service) goBackground(f func(ctx context.Context)) bool {
	return s.goOwned(false, f)
}

// goRunning is goBackground for work that needs Run's context (a wake):
// false before Run began, too.
func (s *Service) goRunning(f func(ctx context.Context)) bool {
	return s.goOwned(true, f)
}

func (s *Service) goOwned(needRun bool, f func(ctx context.Context)) bool {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgClosed || (needRun && !s.bgRunning) {
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f(s.bg)
	}()
	return true
}

// running reports whether Run began and has not ended.
func (s *Service) running() bool {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	return s.bgRunning && !s.bgClosed
}

// stop ends the streams and the background work and waits for the
// latter.
func (s *Service) stop() {
	s.BeginStopping()
	s.bgMu.Lock()
	s.bgClosed = true
	s.bgMu.Unlock()
	s.bgCancel()
	s.wg.Wait()
}

// coalescer runs one job per key at a time; a request while one runs
// makes exactly one more run after it, which covers every request that
// arrived meanwhile.
type coalescer struct {
	mu      sync.Mutex
	running map[string]bool
	again   map[string]bool
}

// request reports whether the caller starts the key's job; otherwise the
// running job runs once more.
func (c *coalescer) request(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running == nil {
		c.running, c.again = map[string]bool{}, map[string]bool{}
	}
	if c.running[key] {
		c.again[key] = true
		return false
	}
	c.running[key] = true
	return true
}

// next reports whether the key's job must run once more; when not, the
// job is over.
func (c *coalescer) next(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.again[key] {
		delete(c.again, key)
		return true
	}
	delete(c.running, key)
	return false
}

// abort ends the key's job without another run.
func (c *coalescer) abort(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.running, key)
	delete(c.again, key)
}

// coalesce starts job for key in the background unless it runs already,
// and runs it again as long as requests arrive while it runs.
func (s *Service) coalesce(c *coalescer, key string, job func(ctx context.Context)) {
	if !c.request(key) {
		return
	}
	if !s.goBackground(func(ctx context.Context) {
		for {
			job(ctx)
			if ctx.Err() != nil {
				c.abort(key)
				return
			}
			if !c.next(key) {
				return
			}
		}
	}) {
		c.abort(key)
	}
}

// tokenBucket admits rate events per second with a burst.
type tokenBucket struct {
	mu          sync.Mutex
	rate, burst float64
	tokens      float64
	last        time.Time
}

func newTokenBucket(rate, burst float64) *tokenBucket {
	return &tokenBucket{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed.Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rateLog logs a kind of warning at most once a second, with the number
// of those suppressed since: log lines that a remote party can trigger.
type rateLog struct {
	log   *slog.Logger
	mu    sync.Mutex
	kinds map[string]*rateKind
}

type rateKind struct {
	last       time.Time
	suppressed int64
}

func newRateLog(log *slog.Logger) *rateLog {
	return &rateLog{log: log, kinds: map[string]*rateKind{}}
}

func (r *rateLog) warn(kind, msg string, args ...any) {
	r.mu.Lock()
	k := r.kinds[kind]
	if k == nil {
		k = &rateKind{}
		r.kinds[kind] = k
	}
	now := time.Now()
	if now.Sub(k.last) < time.Second {
		k.suppressed++
		r.mu.Unlock()
		return
	}
	suppressed := k.suppressed
	k.last, k.suppressed = now, 0
	r.mu.Unlock()
	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	r.log.Warn(msg, args...)
}
