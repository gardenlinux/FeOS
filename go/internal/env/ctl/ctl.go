// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package ctl is the environment controller (ADR 0018): the leader loops
// that bring every environment to its target deployment and keep it there,
// and the engine's prepare hook.
//
// Loops (Loops), run by the leader, fenced, never with a node call inside
// a transaction:
//
//	env-reconcile  2 s, hint "env-reconcile": one step per environment whose latest
//	               deployment is not deployed, whose instances do not match their
//	               target, or that is deleting: groups, addons (credentials,
//	               volume, instance, provisioning), deploy jobs, image services
//	               (rolling, surge 1), attached services, removed objects, deployed
//	env-probe      1 s: containers' exits and restarts with backoff, readiness
//	               (http, tcp, exec), attached services' processes; tenants'
//	               jobs (tenantjobs.go): their ends, starts and pods
//	env-groups     10 s, hint "env-groups": the security groups each connected
//	               node should have, re-put where they differ; what a node holds
//	               of the ingress's groups recorded; stale groups deleted
//	env-names      2 s, hint "env-names": the name tables each connected node
//	               should have, updated or re-put where they differ
//	env-volumes    30 s: the volumes marked deleting, deleted on their nodes
//	env-delete     5 s: environments deleting (and those of deleting tenants);
//	               tenants' jobs that ended TenantJobRetention ago
//
// The controller inserts and deletes instance rows (sandboxes with
// environment_id) and runs Engine.Ensure for the rows that should run in
// a bounded pool of its own (8 workers per instance, single flight per
// row); reconcile steps enqueue, never call Ensure themselves.
//
// The security groups (groups.go) are env-<environment>, the set of all
// an environment's pods (its instances, sandboxes and tenants' jobs'
// pods), whose one rule is out to itself; sg-<row>, a tenant's group (ADR
// 0034), its rules from the environment's spec and its members the pods
// whose template names it; ing-<sandbox> for an image service's pod of an
// environment that was ever public (environments.ingress_group,
// model.PodTemplate.IngressGroup: the first publish rolls the services
// once), whose rule lets the ingress in on the routed ports while the
// environment is public; and ferun-ingress, the mesh addresses of the
// ingress instances. The prepare hook and env-groups record the ports of
// each ing group and the version of ferun-ingress that a node holds
// (env_instances.ingress_ports, nodes.ingress_group_version): the ingress
// sends to an instance only with both.
//
// The target T of an environment is its latest deployment R, unless R
// failed: then the deployed deployment (nothing when none was deployed), so
// that a failed deployment is never retried until a new one arrives. An
// instance's template hash is model.PodTemplate.Hash over its rendered
// template; an instance whose hash differs from T's is replaced (rolled
// for image services, recreated for addons). An addon's hash takes its
// image as the blueprint writes it (its tag): the row pins the digest at
// its insert, so that a new digest under the tag never recreates it.
package ctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/addons"
	"github.com/gardenlinux/feos/go/internal/env/envdeps"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The controller's loops, and the hints that wake them: a writer that
// wants the controller to act sends db.Notify(ctx, q, db.ChanLeader,
// HintReconcile) (or HintGroups, HintNames) inside its transaction.
const (
	LoopReconcile = "env-reconcile"
	LoopProbe     = "env-probe"
	LoopGroups    = "env-groups"
	LoopNames     = "env-names"
	LoopVolumes   = "env-volumes"
	LoopDelete    = "env-delete"

	HintReconcile = LoopReconcile
	HintGroups    = LoopGroups
	HintNames     = LoopNames
)

// The loops' intervals (§4.2).
const (
	reconcileEvery = 2 * time.Second
	probeEvery     = time.Second
	groupsEvery    = 10 * time.Second
	namesEvery     = 2 * time.Second
	volumesEvery   = 30 * time.Second
	deleteEvery    = 5 * time.Second
)

// The answers of LogTail, RestartService and RunJob that the API maps to
// statuses of its own.
var (
	// ErrNoObject: the environment's target deployment has no such object
	// (or replica: then the error's text says so, and wraps it).
	ErrNoObject = errors.New("no such object")
	// ErrPaused: the instance is paused or hibernated; reading its logs
	// would wake it (409 "paused").
	ErrPaused = errors.New("paused")
	// ErrNoContainerLogs: the instance's node does not serve container
	// logs (an older feosd answers Unimplemented): 501 "this node does not
	// serve container logs yet".
	ErrNoContainerLogs = errors.New("this node does not serve container logs yet")
	// ErrNotRunning: what the call needs does not run, an expected state
	// (409, never 500): an instance pending, stopped or failed has no log
	// to read; an attached service's sandbox that is not running, gone or
	// not the tenant's has no process to read or restart. The error's text
	// says which. It wraps model.ErrNotRunning.
	ErrNotRunning = fmt.Errorf("not running: %w", model.ErrNotRunning)
	// ErrNotReady: the object cannot run yet, an expected state (409): a
	// job whose template does not render ("job X cannot run yet: …").
	ErrNotReady = errors.New("not ready")
	// ErrJobRunning: a run of the job runs already, of any deployment (409
	// busy): the error's text says which ("job X runs already (deployment
	// N, attempt M)"). It wraps model.ErrBusy.
	ErrJobRunning = fmt.Errorf("a run of the job runs: %w", model.ErrBusy)
)

// stateError is an answer for an expected state: its text as the caller
// shows it, its kind (ErrNotRunning, …) for errors.Is.
type stateError struct {
	msg  string
	kind error
}

func (e *stateError) Error() string { return e.msg }
func (e *stateError) Unwrap() error { return e.kind }

// notRunning is an ErrNotRunning with its own text.
func notRunning(format string, args ...any) error {
	return &stateError{msg: fmt.Sprintf(format, args...), kind: ErrNotRunning}
}

// The sources of LogTail's lines.
const (
	// SourceContainer: the main container's log of an image instance.
	SourceContainer = "container"
	// SourceProcess: the process log of an attached service.
	SourceProcess = "process"
)

// errNoSealer is the answer of everything that needs a secret's value
// while the instance has no --secret-key-file.
var errNoSealer = errors.New("secrets need --secret-key-file")

// errTenantDeleting refuses an instance insert of a tenant being deleted.
var errTenantDeleting = errors.New("the tenant is being deleted")

// errEnvDeleting refuses an instance insert of an environment being
// deleted.
var errEnvDeleting = errors.New("the environment is being deleted")

// Deps are the parts of the instance the controller works with. DB,
// Engine and Elector are required; Store is made from DB when nil;
// Blueprint is addons.Get when nil. Without a Runner the attached services
// and exec checks fail (with a detail saying so), without Ports the http
// and tcp checks, without a Sealer the addons' credentials.
type Deps struct {
	// InstanceID is the instance's ID.
	InstanceID string
	DB         *db.DB
	Store      *store.Store
	Elector    *leader.Elector
	Conns      nodelink.Conns
	Engine     envdeps.Engine
	Runner     envdeps.Runner
	Ports      envdeps.Ports
	Sealer     envdeps.Sealer
	// Blueprint looks up an addon type.
	Blueprint func(typ string) (addons.Blueprint, bool)
	// SinkURL is the sink's API inside the cluster (--sink-url, ADR 0026),
	// which the logs calls ask first; "": they read the nodes only.
	SinkURL string
	// SinkClient asks the sink; nil: a client with a 10 s timeout.
	SinkClient *http.Client
	Log        *slog.Logger
}

// timings are the controller's durations, fields so that the package's
// tests shorten them.
type timings struct {
	// stopGrace is how long a stopping instance stays before it is
	// deleted: routes stop sending to it at once, the env table drops it
	// within a pass of env-names, and the answers that named it expire
	// after their TTL (5 s).
	stopGrace time.Duration
	// crashWindow: so many exits within it open crash_loop; restarts
	// reset after an instance was ready for as long.
	crashWindow time.Duration
	crashExits  int
	// restartMax bounds the restart backoff (2^(n-1) s).
	restartMax time.Duration
	// restartUnit is the backoff's first step.
	restartUnit time.Duration
	// steadyAfter: without a readiness check, an instance is ready when
	// its container still runs so long after it was first seen running.
	steadyAfter time.Duration
	// noCapacityAfter: an instance pending without capacity for so long
	// opens no_capacity.
	noCapacityAfter time.Duration
	// provisionRetry is the wait after a failed provisioning step.
	provisionRetry time.Duration
	// stepTimeout bounds one provisioning step; probeTimeout an exec
	// check.
	stepTimeout, probeTimeout time.Duration
	// nodeCall bounds a node call of a loop.
	nodeCall time.Duration
	// attachedReady bounds how long a woken sandbox is held for its
	// process's first readiness.
	attachedReady time.Duration
	// restartReady bounds how long RestartService waits for a restarted
	// replica to be ready before it restarts the next.
	restartReady time.Duration
	// deploymentDeadline is the least time a deployment may roll before it fails
	// (§13.5: longer when its services' budgets or jobs need more).
	deploymentDeadline time.Duration
}

func defaultTimings() timings {
	return timings{
		stopGrace:          10 * time.Second,
		crashWindow:        10 * time.Minute,
		crashExits:         3,
		restartMax:         5 * time.Minute,
		restartUnit:        time.Second,
		steadyAfter:        2 * time.Second,
		noCapacityAfter:    2 * time.Minute,
		provisionRetry:     10 * time.Second,
		stepTimeout:        2 * time.Minute,
		probeTimeout:       10 * time.Second,
		nodeCall:           10 * time.Second,
		attachedReady:      time.Minute,
		restartReady:       5 * time.Minute,
		deploymentDeadline: 10 * time.Minute,
	}
}

// The fenced transactions' timeouts (as the engine's loops).
const (
	fencedLockTimeout = time.Second
	fencedStmtTimeout = 3 * time.Second
)

// Controller is the environment controller of one instance.
type Controller struct {
	d   Deps
	st  *store.Store
	log *slog.Logger
	rl  *rateLog
	t   timings

	pool  *pool
	specs *specCache

	// bg is the parent of the work that outlives a call (RestartService's
	// replica restarts); Run's end cancels it and waits for wg.
	bg       context.Context
	cancelBg context.CancelFunc
	wg       sync.WaitGroup

	mu sync.Mutex
	// seen is what the probe loop remembers of instances and attached
	// services: when their container (process) was first seen running in
	// the phase it observed last, and when they were last checked.
	seen map[string]*seenState
	// locks serialise the supervision of each attached service.
	locks map[string]*sync.Mutex
	// provisionFailed holds when an addon instance's provisioning failed
	// last, for provisionRetry.
	provisionFailed map[podid.ID]time.Time
	// lastPending is what each deploying deployment waited for at its last
	// step, for logPending.
	lastPending map[string]string
	// exitRun is the microVM's pid at each instance's last recorded exit:
	// a restart boots a new microVM, so the same pid with the container
	// still exited is the same run, whose restart did not happen (a leader
	// change forgets it: at most one exit too many).
	exitRun map[podid.ID]int64
	// cleanWaits is the backoff of the attached services' cleanups that
	// failed, by pool key (made when first needed).
	cleanWaits map[string]cleanWait
	// unserved are the nodes whose feosd serves no names, as env-names
	// and the prepare hook last found (made when first needed).
	unserved map[string]bool
}

// New returns the controller of an instance.
func New(deps Deps) (*Controller, error) {
	if deps.DB == nil || deps.Engine == nil {
		return nil, errors.New("ctl: DB and Engine are required")
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Blueprint == nil {
		deps.Blueprint = addons.Get
	}
	if deps.SinkClient == nil {
		deps.SinkClient = &http.Client{Timeout: 10 * time.Second}
	}
	st := deps.Store
	if st == nil {
		st = store.New(deps.DB)
	}
	log := deps.Log.With("component", "env-ctl")
	c := &Controller{d: deps, st: st, log: log, rl: newRateLog(log), t: defaultTimings(),
		specs: newSpecCache(), seen: map[string]*seenState{}, locks: map[string]*sync.Mutex{},
		provisionFailed: map[podid.ID]time.Time{}, lastPending: map[string]string{}, exitRun: map[podid.ID]int64{}}
	c.pool = newPool(poolWorkers, log)
	c.bg, c.cancelBg = context.WithCancel(context.Background())
	return c, nil
}

// Run runs the controller's background work until ctx ends and waits for
// it: what outlives a loop's run (the Ensure pool ends with the leader's
// term, inside the loops; a restart of a service's replicas with ctx).
func (c *Controller) Run(ctx context.Context) error {
	<-ctx.Done()
	c.cancelBg()
	c.wg.Wait()
	c.pool.wait()
	return nil
}

// Loops returns the controller's leader loops (§4.2).
func (c *Controller) Loops() []model.Loop {
	return []model.Loop{
		{Name: LoopReconcile, Interval: reconcileEvery, Run: c.reconcile},
		{Name: LoopProbe, Interval: probeEvery, Run: c.probe},
		{Name: LoopGroups, Interval: groupsEvery, Run: c.groupsLoop},
		{Name: LoopNames, Interval: namesEvery, Run: c.namesLoop},
		{Name: LoopVolumes, Interval: volumesEvery, Run: c.volumesLoop},
		{Name: LoopDelete, Interval: deleteEvery, Run: c.deleteLoop},
		{Name: LoopBindings, Interval: bindingsEvery, Run: c.bindingsLoop},
	}
}

// fenced runs fn in a transaction of the leader: short lock and statement
// timeouts, the lease checked first (ErrNotLeader without one).
func (c *Controller) fenced(ctx context.Context, fn func(tx *store.Store) error) error {
	if c.d.Elector == nil {
		return leader.ErrNotLeader
	}
	return c.st.InTx(ctx, func(tx *store.Store) error {
		if err := tx.SetLocalTimeouts(ctx, fencedLockTimeout, fencedStmtTimeout); err != nil {
			return err
		}
		if err := c.d.Elector.Fence(ctx, tx.Q()); err != nil {
			return err
		}
		return fn(tx)
	})
}

// notify sends a hint to the leader's loops outside a transaction.
func (c *Controller) notify(ctx context.Context, hint string) {
	if err := c.st.Notify(ctx, db.ChanLeader, hint); err != nil {
		c.rl.warn("notify", "cannot wake the leader's loops", "hint", hint, "err", err)
	}
}

// lease is the lease of a row as the row says: no Acquire.
func lease(sb store.Sandbox) model.Lease {
	return model.Lease{SandboxID: sb.ID, Incarnation: sb.Incarnation, Node: sb.Node}
}

// steady reports whether a row runs without an op: what the probes and
// the detached calls need.
func steady(sb store.Sandbox) bool {
	return sb.ClientState == model.StateRunning && sb.Op == "" && sb.Node != ""
}

// live reports whether an instance counts as ready (§13.7): its phase is
// ready and its row runs without an op. For an idle service (idles) a
// replica paused or hibernated without an op counts too, so that an idle
// service still gets deployed. A phase left from a pod that is unknown,
// stopped, failed or lost does not count: the probes only look at rows
// that run, so nothing else resets it.
func live(r store.EnvInstanceRow, idles bool) bool {
	return r.Instance.Phase == model.PhaseReady && liveRow(r.Sandbox, idles)
}

// Live reports whether an instance of the image service svc counts as
// ready, by the controller's rule (live): what the status counts.
func Live(r store.EnvInstanceRow, svc spec.Service) bool {
	return live(r, idleOf(svc) > 0)
}

// liveRow is the row's half of live.
func liveRow(sb store.Sandbox, idles bool) bool {
	if steady(sb) {
		return true
	}
	return idles && sb.Op == "" && (sb.ClientState == model.StatePaused || sb.ClientState == model.StateHibernated)
}

// materialised reports whether a row's pod sits on its node without an
// op: running, paused, stopped or failed there, where its volumes are
// attached (not lost, hibernated or unknown).
func materialised(sb store.Sandbox) bool {
	switch sb.ClientState {
	case model.StateRunning, model.StatePaused, model.StateStopped, model.StateFailed:
		return sb.Node != "" && sb.Op == "" && sb.Materialised
	}
	return false
}

// attachedKey names an attached service in the controller's maps.
func attachedKey(envID uuid.UUID, service string) string { return envID.String() + "/" + service }

// attachedLock is the lock of an attached service's supervision.
func (c *Controller) attachedLock(envID uuid.UUID, service string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := attachedKey(envID, service)
	m := c.locks[k]
	if m == nil {
		m = &sync.Mutex{}
		c.locks[k] = m
	}
	return m
}

// rateLog logs a warning of each kind at most once a minute.
type rateLog struct {
	log  *slog.Logger
	mu   sync.Mutex
	last map[string]time.Time
}

func newRateLog(log *slog.Logger) *rateLog { return &rateLog{log: log, last: map[string]time.Time{}} }

func (r *rateLog) warn(kind, msg string, args ...any) {
	r.mu.Lock()
	now := time.Now()
	if t, ok := r.last[kind]; ok && now.Sub(t) < time.Minute {
		r.mu.Unlock()
		r.log.Debug(msg, args...)
		return
	}
	r.last[kind] = now
	r.mu.Unlock()
	r.log.Warn(msg, args...)
}

// objectName is an object's name as incidents and labels write it.
func objectName(kind, name string) string { return kind + "/" + name }
