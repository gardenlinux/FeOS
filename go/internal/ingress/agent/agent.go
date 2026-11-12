// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/sinklog"
	"github.com/gardenlinux/feos/go/internal/ingress/wg"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// Defaults of Config.
const (
	DefaultMeshDevice = "ferun-mesh"
	DefaultMeshPort   = 51820
)

// ErrNotConnected is the error of Hold and the visit calls while the
// agent has no connection to the control plane. To status.Code it reads
// as codes.Unavailable: the control plane is away.
var ErrNotConnected error = &callError{code: codes.Unavailable, text: "ingress agent: not connected to the control plane"}

// callError is an error of the agent's own about a call it did not send,
// with the gRPC code that says what it means to the caller.
type callError struct {
	code codes.Code
	text string
}

func (e *callError) Error() string { return e.text }

// GRPCStatus lets status.Code read the error's code.
func (e *callError) GRPCStatus() *status.Status { return status.New(e.code, e.text) }

// Config is what the agent is given (the flags of ferun-ingress).
type Config struct {
	// Name is the instance's name, a DNS label; its certificate is
	// spiffe://<TrustDomain>/ingress/<Name>.
	Name string
	// StateDir holds the identity, the keys and the caches.
	StateDir string
	// JoinTokenFile holds a join token; it is read only while the
	// instance has no valid certificate.
	JoinTokenFile string
	// ControlPlane are the control plane's addresses (host:port of the
	// nodes' port), tried in random order.
	ControlPlane []string
	// ControlPlaneName is the DNS name the control plane's certificates
	// are verified for.
	ControlPlaneName string
	TrustDomain      string
	// MeshDevice is the WireGuard device's name; DefaultMeshDevice when
	// empty.
	MeshDevice string
	// MeshPort is its UDP port; DefaultMeshPort when 0. A repair of the
	// paths to the nodes moves the device to one of the 15 ports after it
	// (repairMesh).
	MeshPort int
	// MeshEndpoint is where the nodes reach the device, an IP address and
	// a port; it is sent in Hello, at the device's current port when a
	// repair moved it.
	MeshEndpoint string
	// MeshRepairEvery is the least time between two repairs of the paths
	// to the nodes, and how long a node's peer has to handshake before it
	// counts as lost; 0 is 60 s. Tests shorten it.
	MeshRepairEvery time.Duration
	// MeshMTU is the device's MTU; 0 lets the device compute it from the
	// interface that holds MeshEndpoint's address (ComputeMTU).
	MeshMTU int
	// Version is the program's version, sent in Hello.
	Version string
	// SinkEndpoint is host:port of the sink's ingest (ADR 0026 §8): the
	// access log goes there, with the instance's certificate. "": the
	// access log is not kept, and its records are counted as dropped.
	SinkEndpoint string
}

// Deps are the agent's collaborators.
type Deps struct {
	// Mesh is the WireGuard device; required.
	Mesh MeshDevice
	// OnState, when set, gets a copy of the table after every change,
	// the cached one first. It is called from one goroutine at a time
	// and must not block.
	OnState func(*wire.State)
	// OnMesh, when set, gets every 2 s the last handshake of each node
	// on the mesh device (the zero time for none), by node name.
	OnMesh func(map[string]time.Time)
	// DialControlPlane, when set, opens the TCP connection to a control
	// plane address in place of a dialer with TCP keepalive (15 s) and
	// TCP_USER_TIMEOUT (20 s).
	DialControlPlane func(ctx context.Context, addr string) (net.Conn, error)
	// Log is the logger; none when nil.
	Log *slog.Logger
}

// timing holds the agent's intervals; tests shorten them.
type timing struct {
	backoffMin, backoffMax        time.Duration
	dialTimeout, handshakeTimeout time.Duration
	callTimeout, joinTimeout      time.Duration
	tokenPoll                     time.Duration
	keyFixedRetry                 time.Duration
	renewRetry, renewMinGap       time.Duration
	refusalWindow                 time.Duration
	refusalAttempts               int
	meshPoll                      time.Duration
	meshStale, meshRepairEvery    time.Duration
	wakeEvery, wakeTimeout        time.Duration
	cacheDelay, cacheRefresh      time.Duration
	syncIdle                      time.Duration
	tableAgeWarn                  time.Duration
}

func defaultTiming() timing {
	return timing{
		backoffMin:       200 * time.Millisecond,
		backoffMax:       5 * time.Second,
		dialTimeout:      10 * time.Second,
		handshakeTimeout: 10 * time.Second,
		callTimeout:      10 * time.Second,
		joinTimeout:      30 * time.Second,
		tokenPoll:        30 * time.Second,
		keyFixedRetry:    60 * time.Second,
		renewRetry:       60 * time.Second,
		renewMinGap:      time.Minute,
		refusalWindow:    refusalWindow,
		refusalAttempts:  refusalAttempts,
		meshPoll:         2 * time.Second,
		meshStale:        meshStale,
		meshRepairEvery:  meshRepairEvery,
		wakeEvery:        5 * time.Second,
		wakeTimeout:      10 * time.Second,
		cacheDelay:       time.Second,
		cacheRefresh:     time.Minute,
		syncIdle:         45 * time.Second,
		tableAgeWarn:     time.Hour,
	}
}

// Agent is the control side of one ferun-ingress instance.
type Agent struct {
	cfg      Config
	deps     Deps
	log      *slog.Logger
	rl       *rateLog
	tune     timing
	endpoint netip.AddrPort
	// order is the order the control plane's addresses are tried in, per
	// attempt: shuffled; tests fix it.
	order func([]string) []string

	// dir is set by Run before any goroutine starts.
	dir       *stateDir
	cacheKick chan struct{}
	meshKick  chan struct{}

	// meshMu serializes Configure and SetPeers on the device.
	meshMu sync.Mutex

	mu          sync.Mutex
	ran         bool
	runCtx      context.Context // nil outside Run
	goroutines  sync.WaitGroup  // the Wake calls
	wgKey       wg.Key
	key         crypto.Signer
	id          *identity
	lastRenew   time.Time
	tried       map[string]bool
	joining     bool
	removed     bool
	removedText string
	refusals    refusals
	lastErr     string
	cur         *session
	place       *meshPlace
	meshWant    MeshConfig // the configuration the device should have; no Device: none yet
	meshCfg     MeshConfig // the one it was last given successfully
	meshOK      bool
	peersSet    bool
	peersVer    uint64
	meshPeers   []MeshPeer
	// meshPort is the device's UDP port: Config.MeshPort, or the one a
	// repair moved it to. announced is the endpoint the control plane
	// took in the last Hello; announcing: a Hello that tells it the
	// endpoint is under way (announce).
	meshPort   int
	announced  string
	announcing bool
	// meshSince is when each node's key became a peer the agent wants,
	// or was set anew by a repair; lastRepair and repairs count the
	// repairs (repairMesh).
	meshSince   map[wg.Key]time.Time
	lastRepair  time.Time
	repairs     uint64
	state       *wire.State
	tableAt     time.Time
	cachedAt    time.Time // the savedAt of table-v2.json, as written or loaded
	tableWarnAt time.Time
	// reachFailed: an attempt to reach the control plane (a join, a
	// connection, a session) failed since Run started.
	reachFailed bool
	wakes       map[wakeKey]*wakeEntry
	// alog is the access log's buffer; it has its own lock.
	alog *accessLog
	// sink is the client of the sink's ingest; nil without
	// Config.SinkEndpoint. Run closes it.
	sink *sinklog.Client
	// renewSlots and redeemSlots bound the visit calls in flight:
	// renewals, and redeems and ends (visit.go).
	renewSlots, redeemSlots chan struct{}
}

// New checks cfg and makes an agent; Run starts it.
func New(cfg Config, deps Deps) (*Agent, error) {
	if !pki.IsNodeName(cfg.Name) {
		return nil, fmt.Errorf("ingress agent: name %q is not a DNS label", cfg.Name)
	}
	if cfg.StateDir == "" {
		return nil, errors.New("ingress agent: no state directory")
	}
	if len(cfg.ControlPlane) == 0 {
		return nil, errors.New("ingress agent: no control-plane address")
	}
	for _, addr := range cfg.ControlPlane {
		if host, port, err := net.SplitHostPort(addr); err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("ingress agent: control-plane address %q is not host:port", addr)
		}
	}
	if cfg.ControlPlaneName == "" {
		return nil, errors.New("ingress agent: no control-plane name")
	}
	if err := pki.ValidateTrustDomain(cfg.TrustDomain); err != nil {
		return nil, fmt.Errorf("ingress agent: %w", err)
	}
	if cfg.MeshDevice == "" {
		cfg.MeshDevice = DefaultMeshDevice
	}
	if cfg.MeshPort == 0 {
		cfg.MeshPort = DefaultMeshPort
	}
	if cfg.MeshPort < 1 || cfg.MeshPort > 65535 {
		return nil, fmt.Errorf("ingress agent: mesh port %d is out of range", cfg.MeshPort)
	}
	if cfg.MeshRepairEvery < 0 {
		return nil, fmt.Errorf("ingress agent: mesh repair interval %s is negative", cfg.MeshRepairEvery)
	}
	ep, err := netip.ParseAddrPort(cfg.MeshEndpoint)
	if err != nil || ep.Port() == 0 || ep.Addr().IsUnspecified() {
		return nil, fmt.Errorf("ingress agent: mesh endpoint %q is not an IP address and a port", cfg.MeshEndpoint)
	}
	if cfg.MeshMTU != 0 && (cfg.MeshMTU < minMTU || cfg.MeshMTU > 65535) {
		return nil, fmt.Errorf("ingress agent: mesh MTU %d is below %d or above 65535", cfg.MeshMTU, minMTU)
	}
	if deps.Mesh == nil {
		return nil, errors.New("ingress agent: no mesh device")
	}
	if cfg.SinkEndpoint != "" {
		if host, port, err := net.SplitHostPort(cfg.SinkEndpoint); err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("ingress agent: sink endpoint %q is not host:port", cfg.SinkEndpoint)
		}
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg.ControlPlane = slices.Clone(cfg.ControlPlane)
	a := &Agent{
		cfg:       cfg,
		deps:      deps,
		log:       log,
		rl:        newRateLog(log, 30*time.Second),
		tune:      defaultTiming(),
		endpoint:  ep,
		order:     shuffled,
		tried:     map[string]bool{},
		wakes:     map[wakeKey]*wakeEntry{},
		alog:      newAccessLog(cfg.Name),
		cacheKick: make(chan struct{}, 1),
		meshKick:  make(chan struct{}, 1),
		meshPort:  cfg.MeshPort,
		meshSince: map[wg.Key]time.Time{},

		renewSlots:  make(chan struct{}, maxVisitCalls),
		redeemSlots: make(chan struct{}, maxVisitCalls),
	}
	if cfg.MeshRepairEvery > 0 {
		a.tune.meshRepairEvery = cfg.MeshRepairEvery
	}
	if cfg.SinkEndpoint != "" {
		c, err := sinklog.NewClient(cfg.SinkEndpoint, cfg.TrustDomain, a.sinkIdentity)
		if err != nil {
			return nil, fmt.Errorf("ingress agent: %w", err)
		}
		a.sink = c
	}
	return a, nil
}

// sinkIdentity is the certificate and bundle the access log goes to the
// sink with.
func (a *Agent) sinkIdentity() (*tls.Certificate, *x509.CertPool, error) {
	id := a.identity()
	if id == nil {
		return nil, nil, errors.New("no certificate yet")
	}
	return &id.cert, id.bundle, nil
}

// Run runs the agent until ctx ends: it loads the state, configures the
// mesh and hands over the cached table, then joins if it must, connects,
// and keeps the table, the mesh and the certificate current. It returns
// nil when ctx ends, and an error only when the state directory cannot be
// used. Every goroutine it starts ends before it returns.
func (a *Agent) Run(ctx context.Context) error {
	a.mu.Lock()
	if a.ran {
		a.mu.Unlock()
		return errors.New("ingress agent: Run called twice")
	}
	a.ran = true
	a.refusals = newRefusals(a.tune.refusalWindow, a.tune.refusalAttempts)
	a.mu.Unlock()

	if err := a.load(); err != nil {
		return err
	}
	a.startFromCache()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.runCtx = ctx
	a.mu.Unlock()
	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		a.cacheLoop(ctx)
	}()
	if a.sink != nil {
		loops.Add(1)
		go func() {
			defer loops.Done()
			a.accessLogLoop(ctx)
		}()
	} else {
		a.log.Info("ingress agent: no sink endpoint; the access log is not kept, its records are counted as dropped")
	}
	go func() {
		defer loops.Done()
		a.meshLoop(ctx)
	}()

	a.connectLoop(ctx)

	cancel()
	loops.Wait()
	a.mu.Lock()
	a.runCtx = nil
	a.mu.Unlock()
	a.goroutines.Wait()
	if a.sink != nil {
		_ = a.sink.Close()
	}
	return nil
}

// load reads the state directory. Damaged files are set aside or ignored
// with a warning; only a directory that cannot be read or written is an
// error.
func (a *Agent) load() error {
	dir, err := openStateDir(a.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("ingress agent: %w", err)
	}
	a.dir = dir
	tried, err := dir.loadTried()
	if err != nil {
		return fmt.Errorf("ingress agent: %w", err)
	}

	wgKey, ok, err := dir.loadWGKey()
	if err != nil {
		a.log.Error("ingress agent: wg.key is damaged; making a new WireGuard key, which the control plane accepts only after a join with a new pinned token", "err", err)
	}
	if !ok {
		if wgKey, err = dir.newWGKey(); err != nil {
			return fmt.Errorf("ingress agent: %w", err)
		}
	}

	key, err := dir.loadKey()
	if err != nil {
		a.log.Warn("ingress agent: tls.key is damaged; making a new key", "err", err)
		key = nil
	}
	if key == nil {
		if key, err = dir.newKey(); err != nil {
			return fmt.Errorf("ingress agent: %w", err)
		}
	}

	var id *identity
	c, err := dir.loadCert()
	switch {
	case err != nil:
		a.log.Warn("ingress agent: cert.json is damaged; joining again", "err", err)
	case c != nil:
		id, err = newIdentity(a.cfg.TrustDomain, a.cfg.Name, key, *c)
		if err == nil && id.expired(time.Now()) {
			id, err = nil, fmt.Errorf("the certificate expired at %s", id.leaf.NotAfter.Format(time.RFC3339))
		}
		if err != nil {
			a.log.Warn("ingress agent: the kept certificate is not valid; joining again", "err", err)
		}
	}
	if id == nil && (c != nil || err != nil) {
		// Set the unusable certificate aside, and start the next join
		// with a new key.
		if lost, err := dir.loseCert(time.Now()); err != nil {
			a.log.Error("ingress agent: set the kept certificate aside", "err", err)
		} else if lost != "" {
			a.log.Info("ingress agent: set the kept certificate aside", "file", lost)
		}
		if nk, err := dir.newKey(); err != nil {
			a.log.Error("ingress agent: make a new key; the next join uses the old one", "err", err)
		} else {
			key = nk
		}
	}

	place, err := dir.loadMesh()
	if err != nil {
		a.log.Warn("ingress agent: ignoring a damaged mesh.json", "err", err)
		place = nil
	}
	state, savedAt, err := dir.loadTable()
	if err != nil {
		a.log.Warn("ingress agent: ignoring a damaged table cache", "err", err)
		state = nil
	}
	removedText, removed, err := dir.loadRemoved()
	if err != nil {
		return fmt.Errorf("ingress agent: %w", err)
	}
	if removed && id != nil {
		// The certificate came from a join after the removal, which did
		// not get to clear the mark.
		if err := dir.clearRemoved(); err != nil {
			a.log.Error("ingress agent: clear the removal of an earlier identity", "err", err)
		}
		removed, removedText = false, ""
	}
	if removed {
		a.log.Error("ingress agent: the ingress was removed; it joins again only with a join token it has not tried", "refusal", removedText)
	}

	a.mu.Lock()
	a.tried = tried
	a.wgKey = wgKey
	a.key = key
	a.id = id
	a.place = place
	a.state = state
	if state != nil {
		a.tableAt, a.cachedAt = savedAt, savedAt
	}
	a.removed, a.removedText = removed, removedText
	a.mu.Unlock()
	return nil
}

// startFromCache configures the mesh from mesh.json and the cached
// table's peers, and hands the cached table to OnState: what the instance
// serves before it reaches the control plane.
func (a *Agent) startFromCache() {
	a.mu.Lock()
	place, state := a.place, a.state
	var clone *wire.State
	if state != nil {
		clone = state.Clone()
	}
	a.mu.Unlock()
	if place != nil {
		if mc, err := a.meshConfigFor(place.PodNetwork, place.Address); err != nil {
			a.log.Warn("ingress agent: ignoring mesh.json", "err", err)
		} else {
			a.wantMesh(mc)
		}
	}
	if clone != nil {
		a.syncPeers()
		if a.deps.OnState != nil {
			a.deps.OnState(clone)
		}
		a.log.Info("ingress agent: serving the cached table", "envs", len(clone.Envs))
	}
}

// connectLoop joins when the instance has no certificate, and otherwise
// connects and serves sessions, until ctx ends.
func (a *Agent) connectLoop(ctx context.Context) {
	b := newBackoff(a.tune.backoffMin, a.tune.backoffMax)
	for ctx.Err() == nil {
		id := a.identity()
		if id == nil {
			if a.joinLoop(ctx) != nil {
				return
			}
			b.reset()
			continue
		}
		if id.expired(time.Now()) {
			a.loseIdentity(fmt.Sprintf("certificate %s expired at %s", id.serial(), id.leaf.NotAfter.Format(time.RFC3339)), false)
			continue
		}
		s, err := a.connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !sleep(ctx, a.afterError(err, b)) {
				return
			}
			continue
		}
		for s != nil {
			a.setCurrent(s, nil)
			next, err := a.serve(ctx, s)
			if s.synced.Load() {
				b.reset()
			}
			a.setCurrent(next, s)
			s.close()
			if ctx.Err() != nil {
				if next != nil {
					a.setCurrent(nil, next)
					next.close()
				}
				return
			}
			if next != nil {
				a.log.Info("ingress agent: connected with the renewed certificate", "addr", next.addr, "instance", next.hello.Instance)
				s = next
				continue
			}
			s = nil
			if !sleep(ctx, a.afterError(err, b)) {
				return
			}
		}
	}
}

// afterError handles the error that ended a connection attempt or a
// session and returns how long to wait before the next attempt.
func (a *Agent) afterError(err error, b *backoff) time.Duration {
	if err == nil {
		err = errors.New("the session ended")
	}
	a.setLastError(err.Error())
	if text, ok := refusalText(err); ok {
		if a.observeRefusal(text) {
			return 0
		}
		return b.delay()
	}
	switch {
	case isKeyFixed(err):
		a.log.Error("ingress agent: the control plane refused the mesh key: wg.key changed without a re-join; issue a new pinned token. Serving from the cache meanwhile",
			"err", statusMessage(err), "retry", a.tune.keyFixedRetry)
		return a.tune.keyFixedRetry
	case isNewKeyNeeded(err):
		if kerr := a.newMeshKey(statusMessage(err)); kerr != nil {
			a.log.Error("ingress agent: the control plane refused the mesh key, and a new one cannot be kept; retrying",
				"err", kerr, "refusal", statusMessage(err), "retry", a.tune.keyFixedRetry)
			return a.tune.keyFixedRetry
		}
		a.log.Warn("ingress agent: the control plane refused the mesh key; made a new one", "refusal", statusMessage(err))
	case errors.Is(err, wire.ErrSequence):
		a.log.Info("ingress agent: the table is out of sequence; connecting again", "err", err)
	case errors.Is(err, io.EOF):
		a.log.Info("ingress agent: the control plane ended the table stream; connecting again")
	case errors.Is(err, errStalled):
		a.log.Warn("ingress agent: the table stream went silent; connecting again", "after", a.tune.syncIdle)
	case statusCode(err) == codes.Unavailable:
		a.rl.warn("unavailable", "ingress agent: the control plane is unavailable; connecting again", "err", err)
	default:
		a.rl.warn("connect", "ingress agent: no connection to the control plane; retrying", "err", err)
	}
	return b.delay()
}

// observeRefusal applies D19 to a refused connection attempt and reports
// whether the identity is now lost.
func (a *Agent) observeRefusal(text string) bool {
	a.mu.Lock()
	a.refusals.observe(text, time.Now())
	lost, removed := a.refusals.lost()
	count := a.refusals.count
	a.mu.Unlock()
	if !lost {
		a.rl.warn("refused", "ingress agent: the control plane refused the certificate; keeping it and retrying", "refusal", text, "attempts", count)
		return false
	}
	a.loseIdentity(text, removed)
	return true
}

// loseIdentity declares the identity lost: cert.json is set aside, a new
// key is made, and the agent joins again with a token it has not tried. A
// removal is kept in the state directory until the next join.
func (a *Agent) loseIdentity(reason string, removed bool) {
	kept, err := a.dir.loseCert(time.Now())
	if err != nil {
		a.log.Error("ingress agent: set the lost certificate aside", "err", err)
	}
	if removed {
		if err := a.dir.saveRemoved(reason); err != nil {
			a.log.Error("ingress agent: keep the removal; a restart forgets it", "err", err)
		}
	}
	key, err := a.dir.newKey()
	if err != nil {
		a.log.Error("ingress agent: make a new key; the next join uses the old one", "err", err)
	}
	a.mu.Lock()
	a.id = nil
	if key != nil {
		a.key = key
	}
	a.refusals.reset()
	if removed {
		a.removed, a.removedText = true, reason
	}
	a.mu.Unlock()
	a.log.Error("ingress agent: the identity is lost; it joins again only with a join token it has not tried",
		"reason", reason, "removed", removed, "kept", kept)
}

// newMeshKey makes a new WireGuard key and keeps it in wg.key; the device,
// when configured, takes it at once, and the next Hello sends it. reason
// is logged.
func (a *Agent) newMeshKey(reason string) error {
	k, err := a.dir.newWGKey()
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.wgKey = k
	configured := a.meshWant.Device != ""
	if configured {
		a.meshWant.PrivateKey = k
	}
	a.mu.Unlock()
	if configured {
		a.ensureMesh()
	}
	a.log.Info("ingress agent: made a new WireGuard key", "reason", reason, "publicKey", k.PublicKey().String())
	return nil
}

// joined installs the identity of a successful join, which ends a
// removal.
func (a *Agent) joined(id *identity) {
	if err := a.dir.clearRemoved(); err != nil {
		a.log.Error("ingress agent: clear the removal", "err", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.id = id
	a.lastRenew = time.Time{}
	a.removed, a.removedText = false, ""
	a.refusals.reset()
	a.lastErr = ""
}

func (a *Agent) identity() *identity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.id
}

// setCurrent makes s the current session, if the current one is old (or
// any when old is nil).
func (a *Agent) setCurrent(s, old *session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if old == nil || a.cur == old {
		a.cur = s
	}
}

func (a *Agent) current() *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cur
}

func (a *Agent) setJoining(v bool) {
	a.mu.Lock()
	a.joining = v
	a.mu.Unlock()
}

// setLastError records why the last attempt to reach the control plane
// failed: a join, a connection attempt or a session.
func (a *Agent) setLastError(s string) {
	a.mu.Lock()
	a.lastErr = s
	a.reachFailed = true
	a.mu.Unlock()
}

// Connected reports whether the agent holds a connection to the control
// plane on which Hello succeeded.
func (a *Agent) Connected() bool { return a.current() != nil }

// HasState reports whether the agent has a table, cached or streamed.
func (a *Agent) HasState() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state != nil
}

// MeshConfigured reports whether the mesh device was configured.
func (a *Agent) MeshConfigured() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.meshOK
}

// IdentityRemoved reports whether the control plane said the ingress was
// removed, with its refusal's text: the instance serves nobody's traffic
// until it joins again. The removal survives a restart.
func (a *Agent) IdentityRemoved() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.removed, a.removedText
}

// Status is what the agent reports of itself.
type Status struct {
	Name            string     `json:"name"`
	PodNetwork      string     `json:"podNetwork,omitempty"`
	Prefix          string     `json:"prefix,omitempty"`
	Address         string     `json:"address,omitempty"`
	AppsZone        string     `json:"appsZone,omitempty"`
	TenantZone      string     `json:"tenantZone,omitempty"`
	AdmittedVersion uint64     `json:"admittedVersion"`
	Connected       bool       `json:"connected"`
	ControlPlane    string     `json:"controlPlane,omitempty"` // the address of the current connection
	Instance        string     `json:"instance,omitempty"`     // the instance that answered its Hello
	ControlPlanes   []string   `json:"controlPlanes"`
	Joining         bool       `json:"joining"`
	HasState        bool       `json:"hasState"`
	TableSeq        uint64     `json:"tableSeq"`
	TableAt         time.Time  `json:"tableAt,omitzero"` // the last message, or the cache's savedAt
	Envs            int        `json:"envs"`
	MeshVersion     uint64     `json:"meshVersion"`
	CertSerial      string     `json:"certSerial,omitempty"`
	CertNotAfter    time.Time  `json:"certNotAfter,omitzero"`
	CertRenewAfter  time.Time  `json:"certRenewAfter,omitzero"`
	Refusals        int        `json:"refusals"` // refused attempts since the last successful Hello
	RefusalsTotal   int        `json:"refusalsTotal"`
	LastRefusal     string     `json:"lastRefusal,omitempty"`
	LastRefusalAt   time.Time  `json:"lastRefusalAt,omitzero"`
	IdentityRemoved bool       `json:"identityRemoved"`
	MeshConfigured  bool       `json:"meshConfigured"`
	MeshDevice      string     `json:"meshDevice"`
	MeshPublicKey   string     `json:"meshPublicKey"`
	MeshMTU         int        `json:"meshMTU,omitempty"`
	MeshPeers       []MeshPeer `json:"meshPeers"`
	LastError       string     `json:"lastError,omitempty"`

	// MeshPort and MeshEndpoint are the device's port and the endpoint
	// sent in Hello; MeshRepairs counts the repairs of the paths to the
	// nodes since the start, the last at LastMeshRepair (repairMesh).
	MeshPort       int       `json:"meshPort"`
	MeshEndpoint   string    `json:"meshEndpoint"`
	MeshRepairs    uint64    `json:"meshRepairs"`
	LastMeshRepair time.Time `json:"lastMeshRepair,omitzero"`
}

// MeshPeer is a node as the mesh device has it.
type MeshPeer struct {
	Node          string    `json:"node"`
	PublicKey     string    `json:"publicKey"`
	Endpoint      string    `json:"endpoint,omitempty"`
	LastHandshake time.Time `json:"lastHandshake,omitzero"`
}

// Status returns the agent's status.
func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{
		Name:            a.cfg.Name,
		Connected:       a.cur != nil,
		ControlPlanes:   slices.Clone(a.cfg.ControlPlane),
		Joining:         a.joining,
		HasState:        a.state != nil,
		TableAt:         a.tableAt,
		Refusals:        a.refusals.count,
		RefusalsTotal:   a.refusals.total,
		LastRefusal:     a.refusals.lastText,
		LastRefusalAt:   a.refusals.lastAt,
		IdentityRemoved: a.removed,
		MeshConfigured:  a.meshOK,
		MeshDevice:      a.cfg.MeshDevice,
		MeshPublicKey:   a.wgKey.PublicKey().String(),
		MeshMTU:         a.cfg.MeshMTU,
		MeshPort:        a.meshPort,
		MeshEndpoint:    a.meshEndpointLocked(),
		MeshRepairs:     a.repairs,
		LastMeshRepair:  a.lastRepair,
		MeshPeers:       slices.Clone(a.meshPeers),
		LastError:       a.lastErr,
	}
	if a.place != nil {
		st.PodNetwork, st.Prefix, st.Address = a.place.PodNetwork, a.place.Prefix, a.place.Address
	}
	if a.cur != nil {
		st.ControlPlane = a.cur.addr
		st.Instance = a.cur.hello.Instance
		st.AppsZone, st.TenantZone = a.cur.hello.AppsZone, a.cur.hello.TenantZone
	}
	if a.state != nil {
		st.TableSeq = a.state.Seq
		st.Envs = len(a.state.Envs)
		st.MeshVersion = a.state.Mesh.Version
		st.AdmittedVersion = a.state.Self.AdmittedVersion
		if st.AppsZone == "" {
			st.AppsZone, st.TenantZone = a.state.Self.AppsZone, a.state.Self.TenantZone
		}
	}
	if a.id != nil {
		st.CertSerial = a.id.serial()
		st.CertNotAfter = a.id.leaf.NotAfter
		st.CertRenewAfter = a.id.renewAfter
	}
	if st.MeshMTU == 0 {
		if r, ok := a.deps.Mesh.(interface{ MTU() int }); ok {
			st.MeshMTU = r.MTU()
		}
	}
	return st
}

// rateLog logs a kind of line at most once per interval, with the number
// of lines of that kind it suppressed since: an unreachable control plane
// would flood the log.
type rateLog struct {
	log      *slog.Logger
	interval time.Duration
	mu       sync.Mutex
	kinds    map[string]*rateKind
}

type rateKind struct {
	last       time.Time
	suppressed int
}

func newRateLog(log *slog.Logger, interval time.Duration) *rateLog {
	return &rateLog{log: log, interval: interval, kinds: map[string]*rateKind{}}
}

func (r *rateLog) warn(kind, msg string, args ...any)  { r.emit(slog.LevelWarn, kind, msg, args) }
func (r *rateLog) error(kind, msg string, args ...any) { r.emit(slog.LevelError, kind, msg, args) }

func (r *rateLog) emit(level slog.Level, kind, msg string, args []any) {
	r.mu.Lock()
	k := r.kinds[kind]
	if k == nil {
		k = &rateKind{}
		r.kinds[kind] = k
	}
	now := time.Now()
	if !k.last.IsZero() && now.Sub(k.last) < r.interval {
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
	r.log.Log(context.Background(), level, msg, args...)
}

// isKeyFixed reports whether Hello was refused because the instance's mesh
// key is fixed ("the mesh key of ingress <n> is fixed; join it again to
// change it").
func isKeyFixed(err error) bool {
	return statusCode(err) == codes.FailedPrecondition && strings.Contains(statusMessage(err), "is fixed")
}

// isNewKeyNeeded reports whether the first Hello after a join was refused
// because its mesh key may not be taken, the one the join again retired
// or another peer's ("the mesh key of ingress <n> … ; make a new one").
func isNewKeyNeeded(err error) bool {
	return statusCode(err) == codes.FailedPrecondition && strings.HasSuffix(statusMessage(err), "make a new one")
}
