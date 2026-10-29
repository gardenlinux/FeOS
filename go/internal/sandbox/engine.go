// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/bindings"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Config is the engine's part of the instance's configuration (§3).
// TrustDomain, DefaultImage, DefaultVCPUs and DefaultMemoryMiB describe
// the instance; the engine takes a sandbox's image and size from its row.
// NodeWait is the nodes' port's (--node-wait): the engine waits for a
// node's connection through Conns, which applies it.
type Config struct {
	InstanceID, TrustDomain, DefaultImage string
	// Placement is "binpack" (the fullest feasible node wins, the default)
	// or "spread".
	Placement                      string
	DefaultVCPUs, DefaultMemoryMiB int
	// DiskBudget is the per-pod disk gate of placement, in bytes
	// (--disk-budget).
	DiskBudget int64
	// IdlePause and HibernateAfter switch their policies off at 0; a row
	// with idle_after_s idles after it whatever IdlePause is.
	IdlePause, HibernateAfter, LostGrace, NodeWait, DrainTimeout time.Duration
	PendingRepairAfter, UnreachableRepairAfter                   time.Duration
	// HibernateTimeout is how long a hibernate's node call may run
	// (--hibernate-timeout): the hibernate op's deadline is this plus a
	// pause's deadline and 16 seconds per GiB of memory. It is how long
	// ferund waits: a feosd that
	// runs the hibernation in a task of its own goes on after the call
	// ends, and its end arrives as the pod's event.
	HibernateTimeout time.Duration
	// ImageBakes lets a create have its node bake the pod's image into a
	// layer that later creates start on (--image-bakes, ADR 0025).
	ImageBakes bool
	// DenyPrefixes are the destinations the image resolver may not reach,
	// as bindings and preflights may not (--deny-prefix); nil:
	// bindings.DefaultDenyPrefixes.
	DenyPrefixes []netip.Prefix
	Log          *slog.Logger
}

// Defaults of Config when zero (§3).
const (
	defaultDiskBudget             = 4 << 30
	defaultLostGrace              = 10 * time.Minute
	defaultNodeWait               = 5 * time.Second
	defaultDrainTimeout           = time.Hour
	defaultPendingRepairAfter     = 10 * time.Minute
	defaultUnreachableRepairAfter = 2 * time.Minute
	// DefaultHibernateTimeout is --hibernate-timeout's default.
	DefaultHibernateTimeout = time.Hour
)

// The placement policies.
const (
	PlacementBinpack = "binpack"
	PlacementSpread  = "spread"
)

// ProcessLister lists the process_start ids still running in a sandbox,
// without a hold and without counting as activity (*tools.Runner).
type ProcessLister interface {
	RunningProcesses(ctx context.Context, lease model.Lease) (map[string]bool, error)
}

// Deps are the parts of the instance the engine works with. DB and Conns
// are required; Store and Activity are made from DB when nil; without an
// Elector the fenced loops refuse (leader.ErrNotLeader).
type Deps struct {
	DB       *db.DB
	Store    *store.Store
	Conns    nodelink.Conns
	Elector  *leader.Elector
	Activity *activity.Recorder
	// Processes is set by the server after both exist (SetProcessLister).
	Processes ProcessLister
}

// timings are the engine's fixed durations (§3, §9), fields so that the
// package's tests can shorten them.
type timings struct {
	// prepBudget bounds Ensure (the preparation budget).
	prepBudget time.Duration
	// opDeadline is an op's deadline for its kind and the sandbox's size.
	opDeadline func(kind OpKind, memoryMiB int) time.Duration
	// pollEvery is how often a PENDING pod is asked for besides its
	// events.
	pollEvery time.Duration
	// retryMin and retryMax bound the backoff of transient node errors.
	retryMin, retryMax time.Duration
	// settleGrace bounds the one GetPod after an op's deadline when the
	// node did not answer before it.
	settleGrace time.Duration
	// finishMin and finishMax bound the backoff of a finish that fails.
	finishMin, finishMax time.Duration
	// holdLease and holdRenew are the lease of an exec or forward hold and
	// its renewal.
	holdLease, holdRenew time.Duration
	// processSlack is what a process hold adds to its process's timeout.
	processSlack time.Duration
	// deferredEvery re-checks the sandboxes whose sync waits for another
	// instance's op.
	deferredEvery time.Duration
	// syncTimeout bounds one sync.
	syncTimeout time.Duration
	// notifyEvery spaces this instance's ferun_reconcile for one node.
	notifyEvery time.Duration
	// staleOverdue is how long past its deadline a live owner's op is
	// released.
	staleOverdue time.Duration
	// tombstoneGet bounds a tombstone's GetPod; tombstoneBase is its
	// DeletePod's own part of the deadline, beside the suspend it may
	// wait behind and the per-GiB part (tombstoneDeadline).
	tombstoneGet, tombstoneBase time.Duration
	// pauseWait is how long the REST pause waits for its op.
	pauseWait time.Duration
	// touchEvery throttles Touch per sandbox.
	touchEvery time.Duration
	// waitPoll is the timer of the waits on another instance's write.
	waitPoll time.Duration
	// hibernateWait is how long Ensure waits for a hibernate in flight
	// before it tells its caller to come back (ErrBusy): a hibernation
	// uploads the pod's disk and memory and takes minutes.
	hibernateWait time.Duration
	// preemptRecheck is how often Ensure asks the node again whether a
	// resume would abandon the hibernate it waits for, while the answer
	// may still change (no hibernation on the node yet, the op's call not
	// in it yet, the node did not answer); preemptGet bounds one such
	// GetPod.
	preemptRecheck, preemptGet time.Duration
	// orphanEvery is how often an instance looks for its orphaned ops;
	// orphanBatch is how many ops one read of that look returns.
	orphanEvery time.Duration
	orphanBatch int
	// policyBackoffMin and policyBackoffMax bound the backoff of a
	// policy whose op failed (backOff).
	policyBackoffMin, policyBackoffMax time.Duration
	// outageGuard is how long the leader must have led, and the database
	// gone without a connection failure, before the loops that act on
	// expired leases (idle pause, stale ops, expired holds) act: long
	// enough for every live instance to renew its holds (holdRenew) and
	// its heartbeat after an outage or a stall. 0 turns the guard off.
	outageGuard time.Duration
}

// defaultTimings are the timings of an instance whose hibernates may run
// for hibernateTimeout.
func defaultTimings(hibernateTimeout time.Duration) timings {
	return timings{
		prepBudget: PreparationBudget,
		opDeadline: func(kind OpKind, memoryMiB int) time.Duration {
			return deadlineFor(kind, memoryMiB, hibernateTimeout)
		},
		pollEvery:     5 * time.Second,
		retryMin:      100 * time.Millisecond,
		retryMax:      2 * time.Second,
		settleGrace:   10 * time.Second,
		finishMin:     100 * time.Millisecond,
		finishMax:     5 * time.Second,
		holdLease:     30 * time.Second,
		holdRenew:     10 * time.Second,
		processSlack:  15 * time.Second,
		deferredEvery: 5 * time.Second,
		syncTimeout:   time.Minute,
		notifyEvery:   5 * time.Second,
		staleOverdue:  5 * time.Minute,
		tombstoneGet:  30 * time.Second,
		tombstoneBase: 2 * time.Minute,
		pauseWait:     time.Minute,
		touchEvery:    time.Second,
		waitPoll:      db.DefaultPoll,
		hibernateWait: 30 * time.Second,

		preemptRecheck: time.Second,
		preemptGet:     5 * time.Second,
		orphanEvery:    10 * time.Second,
		orphanBatch:    loopBatch,

		policyBackoffMin: 30 * time.Second,
		policyBackoffMax: 30 * time.Minute,

		// holdRenew and 5 s: every live instance renews its holds within
		// holdRenew and its heartbeat within 5 s (leader.HeartbeatInterval).
		outageGuard: 15 * time.Second,
	}
}

// PreparationBudget bounds Ensure: the time a sandbox may take to be
// placed, created or brought back (REST advertises preparationWaitMs
// 425000).
const PreparationBudget = 7*time.Minute + 5*time.Second

// OpDeadline is the deadline of an op of kind for a sandbox of memoryMiB
// (§9 ops table), a hibernate's with DefaultHibernateTimeout; GiB =
// ceil(memory_mib / 1024).
func OpDeadline(kind OpKind, memoryMiB int) time.Duration {
	return deadlineFor(kind, memoryMiB, DefaultHibernateTimeout)
}

// feosdSuspendBudget is how long feosd may take for a SuspendPod of a pod
// of memoryMiB, from its call to its answer: 190 s and 5 s per GiB, of
// which its wait for the snapshot is 120 s and 4 s per GiB (feosd's
// microvm_service::vmm::budget, node.md "The suspend's budget"). Past the
// snapshot's wait, feosd answers UNAVAILABLE "the VM is busy" and leaves
// the snapshot to Firecracker.
func feosdSuspendBudget(memoryMiB int) time.Duration {
	return 190*time.Second + 5*time.Second*gib(memoryMiB)
}

// pauseMargin is what a pause waits past feosd's suspend budget: for the
// call to reach feosd and its answer to come back, and for the pod's read
// after it, so that feosd answers, the suspend or "busy", before the pause
// gives up.
const pauseMargin = 30 * time.Second

// deadlineFor is OpDeadline with the instance's hibernate timeout. A pause
// waits feosd's whole suspend budget and pauseMargin more. A hibernate's
// deadline is a pause's, since feosd suspends a running pod first, plus
// hibernateTimeout and 16 seconds per GiB for the upload. A resume has the
// restore's: a ResumePod that meets a hibernation it cannot abandon (past
// its point of no return, a drain's, a pod without memory) waits for it
// and then restores the pod from the new checkpoint, inside the call.
func deadlineFor(kind OpKind, memoryMiB int, hibernateTimeout time.Duration) time.Duration {
	g := gib(memoryMiB)
	switch kind {
	case OpCreate, OpStart:
		return PreparationBudget
	case OpResume, OpRestore, OpRepair:
		return PreparationBudget + 8*time.Second*g
	case OpPause:
		return feosdSuspendBudget(memoryMiB) + pauseMargin
	case OpHibernate:
		return feosdSuspendBudget(memoryMiB) + pauseMargin + hibernateTimeout + 16*time.Second*g
	}
	return PreparationBudget
}

// Engine is the sandbox engine of one instance.
type Engine struct {
	cfg   Config
	id    string
	d     *db.DB
	st    *store.Store
	conns nodelink.Conns
	el    *leader.Elector
	act   *activity.Recorder
	// ownAct: the engine made the recorder and runs it.
	ownAct bool
	log    *slog.Logger
	t      timings

	// leadingSince and lastFailure are the elector's LeadingSince and the
	// database's LastFailure, which settled reads; tests replace them.
	leadingSince func() time.Time
	lastFailure  func() time.Time

	// resolveImage pins an image to its digest (image.go); tests replace
	// it.
	resolveImage func(ctx context.Context, image string) (string, error)
	// checkImage asks a pinned image's registry before a create
	// (image.go, ADR 0025); SetImageAccess replaces it.
	checkImage func(ctx context.Context, pinned string) error

	// hooks are the environments' hooks (hooks.go).
	hooks engineHooks

	runner *runner
	syncs  syncer

	// bg owns the engine's background work (finishes, syncs, renewals,
	// detached node calls); it ends when Run returns.
	bg       context.Context
	bgCancel context.CancelFunc
	bgMu     sync.Mutex
	bgDone   bool
	wg       sync.WaitGroup

	mu          sync.Mutex
	procs       ProcessLister
	stopping    bool                  // no work of the instance's own initiative any more
	deferred    map[podid.ID]struct{} // syncs waiting for another instance's op
	notified    map[string]time.Time  // node → last ferun_reconcile sent for it
	touched     map[podid.ID]time.Time
	unreachable map[podid.ID]*reach // sandboxes this instance marked unreachable
	bakes       map[string]bool     // bakes this instance recorded (bakes.go)
	// evacuations are the drain's workers in flight, sandbox → node, and
	// evacuatingOn their number per node (at most drainPerNode).
	evacuations  map[podid.ID]string
	evacuatingOn map[string]int

	holdsMu sync.Mutex
	holds   map[holdKey]*holdState

	tombsMu sync.Mutex
	tombs   map[store.TombstoneKey]*tombRun

	rl *rateLog

	// preBeginHook, when set, runs in Start right before the begin's
	// transaction, after Before begin: tests let another instance act
	// between a caller's read and its begin there.
	preBeginHook func(ctx context.Context, id podid.ID, kind OpKind)
	// beginHook, when set, runs inside a fresh begin's transaction after
	// the row is locked, before its rechecks: tests inject delays there.
	beginHook func(ctx context.Context, id podid.ID, kind OpKind)
	// admitHook, when set, runs inside Acquire's transaction after the
	// row is locked: tests inject delays there.
	admitHook func(ctx context.Context, id podid.ID)
	// connHook, when set, runs where an op gets its connection; an error
	// is taken for a connection that could not be had: tests inject it.
	connHook func(h *OpHandle) error
	// beginTxHook, when set, replaces the error of a begin's transaction:
	// tests make a COMMIT's answer get lost.
	beginTxHook func(id podid.ID, err error) error
}

// New returns the engine of an instance.
func New(cfg Config, deps Deps) (*Engine, error) {
	if deps.DB == nil || deps.Conns == nil {
		return nil, errors.New("sandbox: DB and Conns are required")
	}
	if cfg.InstanceID == "" {
		return nil, errors.New("sandbox: no instance id")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	switch cfg.Placement {
	case "":
		cfg.Placement = PlacementBinpack
	case PlacementBinpack, PlacementSpread:
	default:
		return nil, fmt.Errorf("sandbox: unknown placement %q (binpack or spread)", cfg.Placement)
	}
	defaults := []struct {
		v *time.Duration
		d time.Duration
	}{
		{&cfg.LostGrace, defaultLostGrace}, {&cfg.NodeWait, defaultNodeWait}, {&cfg.DrainTimeout, defaultDrainTimeout},
		{&cfg.PendingRepairAfter, defaultPendingRepairAfter}, {&cfg.UnreachableRepairAfter, defaultUnreachableRepairAfter},
		{&cfg.HibernateTimeout, DefaultHibernateTimeout},
	}
	for _, x := range defaults {
		if *x.v <= 0 {
			*x.v = x.d
		}
	}
	if cfg.DiskBudget <= 0 {
		cfg.DiskBudget = defaultDiskBudget
	}
	log := cfg.Log.With("component", "sandbox")
	st := deps.Store
	if st == nil {
		st = store.New(deps.DB)
	}
	e := &Engine{
		cfg: cfg, id: cfg.InstanceID, d: deps.DB, st: st, conns: deps.Conns, el: deps.Elector,
		act: deps.Activity, log: log, t: defaultTimings(cfg.HibernateTimeout), procs: deps.Processes,
		deferred: map[podid.ID]struct{}{}, notified: map[string]time.Time{}, touched: map[podid.ID]time.Time{},
		unreachable: map[podid.ID]*reach{}, evacuations: map[podid.ID]string{}, evacuatingOn: map[string]int{},
		holds: map[holdKey]*holdState{}, tombs: map[store.TombstoneKey]*tombRun{}, rl: newRateLog(log),
	}
	e.leadingSince = func() time.Time { return time.Time{} }
	if e.el != nil {
		e.leadingSince = e.el.LeadingSince
	}
	e.lastFailure = deps.DB.LastFailure
	if e.act == nil {
		e.act = activity.NewRecorder(st, cfg.InstanceID, cfg.Log)
		e.ownAct = true
	}
	deny := cfg.DenyPrefixes
	if deny == nil {
		deny = bindings.DefaultDenyPrefixes()
	}
	res := newResolver(deny)
	e.resolveImage, e.checkImage = res.resolve, res.access
	e.runner = newRunner(e)
	e.syncs.m = map[podid.ID]*syncEntry{}
	e.bakes = map[string]bool{}
	e.bg, e.bgCancel = context.WithCancel(context.Background())
	return e, nil
}

// settled reports whether the loops that act on expired leases may act:
// the leader has led for outageGuard and the database has not failed
// within it, so that after an outage, a stall (which ends the leader's
// term) or a failover every live instance has renewed its holds and its
// heartbeat first (as fleet's node expiry waits for the node TTL).
func (e *Engine) settled() bool {
	if e.t.outageGuard <= 0 {
		return true
	}
	since := e.leadingSince()
	if since.IsZero() || time.Since(since) < e.t.outageGuard {
		return false
	}
	if f := e.lastFailure(); !f.IsZero() && time.Since(f) < e.t.outageGuard {
		return false
	}
	return true
}

// SetProcessLister sets what the process-holds loop lists processes with:
// the tools' runner, which is made after the engine (it needs the engine).
func (e *Engine) SetProcessLister(p ProcessLister) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.procs = p
}

func (e *Engine) processLister() ProcessLister {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.procs
}

// Run runs the engine's background work until ctx ends: the deferred
// syncs (on ferun_sandbox and every 5 s), the renewals of this instance's
// holds, the look for its orphaned ops (every 10 s), and the activity
// recorder when the engine made it. When ctx ends it ends the engine's
// background work, detached node calls included, and waits for it, so ctx
// is the instance's last (§7 step 5), after Stop.
func (e *Engine) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	if e.ownAct {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.act.Run(ctx)
		}()
	}
	wg.Add(3)
	go func() {
		defer wg.Done()
		e.followSandboxes(ctx)
	}()
	go func() {
		defer wg.Done()
		e.renewHolds(ctx)
	}()
	go func() {
		defer wg.Done()
		e.followOrphans(ctx)
	}()
	<-ctx.Done()
	// No op begins any more, so that the runner's WaitGroup is not added
	// to while it is waited for.
	e.runner.refuse()
	e.bgMu.Lock()
	e.bgDone = true
	e.bgMu.Unlock()
	e.bgCancel()
	e.wg.Wait()
	e.runner.wg.Wait()
	wg.Wait()
	return nil
}

// Stop is §7 steps 3 and 4: the runner refuses new ops (ErrStopping), the
// instance's own ops are waited for until ctx ends, and those still in
// flight then are released (unknown, for Sync to resolve). No work of the
// instance's own initiative starts any more.
func (e *Engine) Stop(ctx context.Context) error {
	e.BeginStopping()
	return e.runner.Stop(ctx)
}

// BeginStopping is §7 step 1 for the engine: the instance begins no work
// of its own initiative any more (the processing of the tombstones it
// writes), while its runner still begins the ops that requests need.
func (e *Engine) BeginStopping() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopping = true
}

func (e *Engine) isStopping() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopping
}

// goBackground runs f in a goroutine the engine owns, with the engine's
// background context; false once Run ended.
func (e *Engine) goBackground(f func(ctx context.Context)) bool {
	e.bgMu.Lock()
	defer e.bgMu.Unlock()
	if e.bgDone {
		return false
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		f(e.bg)
	}()
	return true
}

// detached returns a context for node calls that must not end with the
// caller's: it keeps ctx's values, drops its cancellation, ends at
// deadline, and ends when the engine's Run ends (the instance is gone).
func (e *Engine) detached(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	dctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	stop := context.AfterFunc(e.bg, cancel)
	return dctx, func() {
		stop()
		cancel()
	}
}

// State reads a sandbox's client state and node; it never wakes it.
func (e *Engine) State(ctx context.Context, id podid.ID) (state, node string, err error) {
	sb, err := e.st.GetSandbox(ctx, id)
	if err != nil {
		return "", "", err
	}
	return sb.ClientState, sb.Node, nil
}

// Touch records activity of the sandbox (last_active_at = now()), at most
// once a second per sandbox on this instance: a forwarded request that
// ends while a hold remains, an explicit resume.
func (e *Engine) Touch(id podid.ID) {
	now := time.Now()
	e.mu.Lock()
	if last, ok := e.touched[id]; ok && now.Sub(last) < e.t.touchEvery {
		e.mu.Unlock()
		return
	}
	e.touched[id] = now
	if len(e.touched) > 4096 {
		for k, t := range e.touched {
			if now.Sub(t) >= e.t.touchEvery {
				delete(e.touched, k)
			}
		}
	}
	e.mu.Unlock()
	e.goBackground(func(ctx context.Context) {
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := e.st.TouchSandbox(tctx, id); err != nil {
			e.rl.warn("touch", "cannot record a sandbox's activity", "sandbox", id, "err", err)
		}
	})
}

// Unreachable records that the guest of a running sandbox did not answer
// (unreachable_since = coalesce(unreachable_since, now()), which the repair
// loop acts on). The next healthy call clears it (Reachable).
func (e *Engine) Unreachable(id podid.ID) { e.setReach(id, true) }

// Reachable records a successful call into the sandbox's guest: it clears
// what Unreachable recorded, and writes nothing when this instance did not
// mark the sandbox.
func (e *Engine) Reachable(id podid.ID) { e.setReach(id, false) }

// reach is what this instance last wished recorded of a sandbox's guest.
// One worker at a time writes the wishes of a sandbox, in order, so that a
// mark and the clear of a later healthy call never pass each other (a mark
// that lands last would leave a healthy sandbox to the repair loop).
type reach struct {
	marked bool // the last wish: Unreachable true, Reachable false
	busy   bool // a worker writes
}

// setReach records a wish and starts the sandbox's worker unless it runs;
// a running worker writes the wish after its current write.
func (e *Engine) setReach(id podid.ID, marked bool) {
	e.mu.Lock()
	r := e.unreachable[id]
	if r == nil {
		if !marked {
			e.mu.Unlock()
			return
		}
		r = &reach{}
		e.unreachable[id] = r
	}
	r.marked = marked
	if r.busy {
		e.mu.Unlock()
		return
	}
	r.busy = true
	e.mu.Unlock()
	if !e.goBackground(func(ctx context.Context) { e.writeReach(ctx, id, r) }) {
		e.mu.Lock()
		r.busy = false
		e.mu.Unlock()
	}
}

// writeReach writes the sandbox's last wish until no newer one came during
// a write. It forgets the sandbox when nothing of this instance's is
// recorded any more (cleared, or a mark that found it not running); a
// failed write keeps it marked, so that the next healthy call clears.
func (e *Engine) writeReach(ctx context.Context, id podid.ID, r *reach) {
	for {
		e.mu.Lock()
		marked := r.marked
		e.mu.Unlock()
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		applied, err := true, error(nil)
		if marked {
			applied, err = e.st.MarkUnreachable(tctx, id)
		} else {
			err = e.st.ClearUnreachable(tctx, id)
		}
		cancel()
		if err != nil {
			e.rl.warn("unreachable", "cannot record whether a guest answers", "sandbox", id, "unreachable", marked, "err", err)
		}
		e.mu.Lock()
		if r.marked != marked {
			e.mu.Unlock()
			continue
		}
		r.busy = false
		switch {
		case err == nil && (!marked || !applied):
			if e.unreachable[id] == r {
				delete(e.unreachable, id)
			}
		case err != nil:
			r.marked = true
		}
		e.mu.Unlock()
		return
	}
}

// record queues activity rows; it never blocks.
func (e *Engine) record(events ...activity.Event) {
	for _, ev := range events {
		e.act.Record(ev)
	}
}

// rateLog logs a kind of warning at most once a second, with the number
// of those suppressed since.
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
