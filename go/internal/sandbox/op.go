// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// OpKind is a lifecycle op (§9 ops table); the sandboxes.op column.
type OpKind string

const (
	OpCreate    OpKind = "create"
	OpStart     OpKind = "start"
	OpResume    OpKind = "resume"
	OpRestore   OpKind = "restore"
	OpPause     OpKind = "pause"
	OpHibernate OpKind = "hibernate"
	OpRepair    OpKind = "repair"
)

// OpTrigger says who asked for an op: it selects begin step 3's rechecks
// and the run close reason, and is written into the placement audit.
type OpTrigger string

const (
	TriggerEnsure         OpTrigger = "ensure"
	TriggerExplicit       OpTrigger = "explicit"
	TriggerIdle           OpTrigger = "idle"
	TriggerHibernateAfter OpTrigger = "hibernate_after"
	TriggerDrain          OpTrigger = "drain"
	TriggerDrainForced    OpTrigger = "drain_forced"
	TriggerRepair         OpTrigger = "repair"
	// TriggerRestart is Restart's: a repair of a running row whose
	// container exited, with no recheck.
	TriggerRestart OpTrigger = "restart"
)

// waits reports whether ops of the trigger wait in Before begin for a node
// that is not connected; the loops' ops never wait.
func (t OpTrigger) waits() bool {
	return t == TriggerEnsure || t == TriggerExplicit || t == ""
}

// OpIntent is what a caller asks the runner to do.
type OpIntent struct {
	Kind    OpKind
	Trigger OpTrigger
	// Candidates are the nodes a create or restore may go to, in order;
	// the begin claims the first that passes the gates under its lock.
	Candidates []string
	// Target is the node of every other op (the row's node), and of a
	// takeover (the row's op_target).
	Target string
	// Incarnation is the one the caller read; a row of another is not
	// found.
	Incarnation uuid.UUID
	// ImageRef is the resolved image of a create, written only when the
	// row's is empty.
	ImageRef string

	// reason, when set, is the row's status description while the op
	// runs (Restart's).
	reason string

	// verdicts is the placement's verdict per node, for the audit row.
	verdicts []store.PlacementCandidate
	// takeover, when set, is the op id of a stale op the caller means to
	// take over: the begin takes it over or fails with ErrPrecondition,
	// and never begins an op of its own.
	takeover uuid.UUID
	// preempt, when set, is the op id of a hibernate that a resume is to
	// take the place of, because its node said that a resume abandons the
	// hibernation now: the begin replaces that op with the resume or fails
	// with ErrPrecondition, and never begins an op beside it.
	preempt uuid.UUID
}

// OpResult is how an op ended: the sandbox's new state, node and
// status_description, or the error that ended it.
type OpResult struct {
	State, Node, Description string
	Err                      error

	// fenced: the op was no longer the row's when it finished (taken
	// over, released, the row deleted); it wrote nothing of its outcome.
	fenced bool
	// busy: the node's VM was busy (vmBusy), the pod read after it
	// answered, and the op did not reach its goal; the same op later may.
	busy bool
}

// OpHandle is an op in flight on this instance.
type OpHandle struct {
	done chan struct{}
	res  OpResult

	id       podid.ID
	opID     uuid.UUID
	inc      uuid.UUID
	kind     OpKind
	trigger  OpTrigger
	target   string
	takeover bool
	// preempted is the op id of the hibernate a resume took the place of.
	preempted uuid.UUID
	// from is the owner of the op a takeover or a preempt took the place
	// of, which the begin logs once it has committed.
	from string
	// noAbandon: a hibernate's HibernatePod says that a resume may not
	// abandon the hibernation (a drain's, or one off a draining node).
	noAbandon bool
	// deadline is the op's deadline on this instance's clock.
	deadline time.Time
	started  time.Time
	// row is the row as the begin wrote it.
	row store.Sandbox
	// read is what the begin read, for a rollback.
	read struct {
		state, node, lostFrom string
		materialised          bool
	}
	// tombstones are those the begin wrote (a restore elsewhere), for a
	// rollback and for their processing after the op.
	tombstones []store.TombstoneKey
	// span is the op's activity span; parent the run it began in.
	span, parent string
	// spec is a create's pod spec, built before its call (prepare).
	spec *podv1.PodSpec
	// layers are the baked layers of a create's image it names (prepare,
	// ADR 0025); nil: none, and no bake.
	layers *podv1.ImageLayers
	// pullToken is the guest's pull token of a create with image
	// credentials (prepare, ADR 0025); "": anonymous.
	pullToken string

	mu sync.Mutex
	// dirty: a pod event arrived while the op ran; the finish syncs.
	dirty bool
	// settled: the finish has looked at dirty; later events are synced
	// by whoever reports them.
	settled bool
}

// Wait waits for the op's end, or ctx; ctx ends the wait, never the op.
// An op that has ended gives its result, also when ctx has ended.
func (h *OpHandle) Wait(ctx context.Context) (OpResult, error) {
	select {
	case <-h.done:
		return h.res, nil
	default:
	}
	select {
	case <-h.done:
		return h.res, nil
	case <-ctx.Done():
		return OpResult{}, ctx.Err()
	}
}

// Done is closed when the op has ended.
func (h *OpHandle) Done() <-chan struct{} { return h.done }

// Kind is the op's kind; for a takeover, the kind of the op taken over.
func (h *OpHandle) Kind() OpKind { return h.kind }

// Target is the node the op calls.
func (h *OpHandle) Target() string { return h.target }

// markDirty notes a pod event during the op; false when the finish has
// looked already, and the caller must sync itself.
func (h *OpHandle) markDirty() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.settled {
		return false
	}
	h.dirty = true
	return true
}

// settle ends the dirty window; it reports whether an event arrived.
func (h *OpHandle) settle() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settled = true
	return h.dirty
}

// opRunner is the instance's op runner (§9 "The runner").
type opRunner interface {
	// Start waits (ctx) only in Before begin; single flight per sandbox;
	// ErrStopping, ErrBusy, ErrPrecondition, ErrNodeUnavailable.
	Start(ctx context.Context, id podid.ID, in OpIntent) (*OpHandle, error)
	Stop(ctx context.Context) error
}

var _ opRunner = (*runner)(nil)

// runner runs the ops of one instance.
type runner struct {
	e *Engine

	mu       sync.Mutex
	ops      map[podid.ID]*OpHandle // the op in flight per sandbox
	begins   map[podid.ID]*beginLock
	refusing bool
	// beginning counts the begins in progress: registered under mu while
	// the runner still accepts ops, so that Stop waits for a begin that
	// may yet commit before it releases the instance's ops.
	beginning int
	// begun is closed when beginning drops to 0 while Stop waits for it.
	begun chan struct{}
	// wg counts the ops from their begin on; Add happens only under mu
	// while the runner accepts ops, so it never races the final Wait.
	wg sync.WaitGroup
}

// beginLock serialises the begins of one sandbox on this instance, so
// that two callers that race never both begin: the second finds the
// first's op.
type beginLock struct {
	ch   chan struct{}
	refs int
}

func newRunner(e *Engine) *runner {
	return &runner{e: e, ops: map[podid.ID]*OpHandle{}, begins: map[podid.ID]*beginLock{}}
}

// Start returns the op in flight for the sandbox on this instance (of the
// incarnation in names; joinable), or begins one for in and runs it in a
// goroutine of its own. Ops of a pinned target whose trigger waits first
// wait (ctx) until this instance is connected to the target; those of the
// loops' triggers only check it.
func (r *runner) Start(ctx context.Context, id podid.ID, in OpIntent) (*OpHandle, error) {
	return r.start(ctx, id, in, in.Trigger.waits())
}

// Running returns the op in flight for the sandbox on this instance.
func (r *runner) Running(id podid.ID) *OpHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ops[id]
}

// runs reports whether this instance runs the op of row. An op that row
// says is this instance's and that it does not run is orphaned: its begin
// committed, but no goroutine ran it (a COMMIT whose answer was lost and
// whose row could not be read then, or an earlier process under the same
// instance ID). Reliable under the sandbox's begin lock, where every
// begin of this instance that committed has registered its op.
func (r *runner) runs(row store.Sandbox) bool {
	h := r.Running(row.ID)
	return h != nil && h.opID == row.OpID
}

// opIDs returns the op IDs of the ops this instance runs.
func (r *runner) opIDs() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]uuid.UUID, 0, len(r.ops))
	for _, h := range r.ops {
		ids = append(ids, h.opID)
	}
	return ids
}

func (r *runner) isRefusing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refusing
}

func (r *runner) start(ctx context.Context, id podid.ID, in OpIntent, wait bool) (*OpHandle, error) {
	if r.isRefusing() {
		return nil, r.stoppingError()
	}
	if h := r.joinable(id, in); h != nil {
		return h, nil
	}
	// Before begin: an op on a pinned node is begun only towards a node
	// this instance can reach, so that one that never reached its node
	// does not leave the row unknown.
	if target := pinnedTarget(in); target != "" {
		if wait {
			if err := r.e.conns.WaitConnected(ctx, target); err != nil {
				return nil, r.notConnected(target)
			}
		} else if !r.e.conns.Connected(target) {
			return nil, r.notConnected(target)
		}
	}
	unlock, err := r.lockBegin(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if h := r.joinable(id, in); h != nil {
		return h, nil
	}
	if r.e.preBeginHook != nil {
		r.e.preBeginHook(ctx, id, in.Kind)
	}
	if !r.enterBegin() {
		return nil, r.stoppingError()
	}
	h, err := r.begin(ctx, id, in)
	r.leaveBegin(id, h)
	if err != nil {
		r.wg.Done()
		return nil, err
	}
	starter := context.WithoutCancel(ctx)
	go r.run(starter, h)
	return h, nil
}

// joinable returns this instance's op in flight for the sandbox when the
// caller may join it instead of beginning one: an op of the incarnation
// the caller read (the new row of a sandbox deleted and created again
// never waits for its old row's op), for a takeover the op it means to
// take over, and for a preempt any op but the hibernate it means to
// preempt (a resume another caller began in its place is joined).
func (r *runner) joinable(id podid.ID, in OpIntent) *OpHandle {
	h := r.Running(id)
	if h == nil {
		return nil
	}
	if in.Incarnation != uuid.Nil && h.inc != in.Incarnation {
		return nil
	}
	if in.takeover != uuid.Nil && h.opID != in.takeover {
		return nil
	}
	if in.preempt != uuid.Nil && h.opID == in.preempt {
		return nil
	}
	return h
}

// enterBegin registers a begin in progress, and the op it may begin with
// the runner's WaitGroup; false once the runner refuses ops.
func (r *runner) enterBegin() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refusing {
		return false
	}
	r.beginning++
	r.wg.Add(1)
	return true
}

// leaveBegin ends a begin in progress; h, the op it began (nil when it
// failed), is in flight from now on.
func (r *runner) leaveBegin(id podid.ID, h *OpHandle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beginning--
	if h != nil {
		r.ops[id] = h
	}
	if r.beginning == 0 && r.begun != nil {
		close(r.begun)
		r.begun = nil
	}
}

// refuse makes the runner refuse new ops (ErrStopping) and returns a
// channel that is closed when no begin is in progress any more.
func (r *runner) refuse() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusing = true
	if r.beginning == 0 {
		done := make(chan struct{})
		close(done)
		return done
	}
	if r.begun == nil {
		r.begun = make(chan struct{})
	}
	return r.begun
}

func (r *runner) stoppingError() error {
	return errorf(model.ErrStopping, "instance %s is stopping", r.e.id)
}

func (r *runner) notConnected(node string) error {
	return errorf(model.ErrNodeUnavailable, "node %s is not connected to instance %s", node, r.e.id)
}

// pinnedTarget is the node an op is bound to before its begin: every op's
// but a create's or restore's that comes with candidates.
func pinnedTarget(in OpIntent) string {
	if (in.Kind == OpCreate || in.Kind == OpRestore) && len(in.Candidates) > 0 {
		return ""
	}
	return in.Target
}

// lockBegin takes the sandbox's begin lock, waiting at most for ctx.
func (r *runner) lockBegin(ctx context.Context, id podid.ID) (func(), error) {
	r.mu.Lock()
	bl := r.begins[id]
	if bl == nil {
		bl = &beginLock{ch: make(chan struct{}, 1)}
		r.begins[id] = bl
	}
	bl.refs++
	r.mu.Unlock()
	release := func() {
		r.mu.Lock()
		bl.refs--
		if bl.refs == 0 {
			delete(r.begins, id)
		}
		r.mu.Unlock()
	}
	select {
	case bl.ch <- struct{}{}:
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
	return func() {
		<-bl.ch
		release()
	}, nil
}

// Stop refuses new ops from now on (ErrStopping), waits for the begins in
// progress and then for this instance's ops until ctx ends, and releases
// those still in flight then (§7 steps 3 and 4). A begin that commits
// after the release would leave its row in an op nobody finishes in time,
// so the release waits for the begins first.
func (r *runner) Stop(ctx context.Context) error {
	select {
	case <-r.refuse():
	case <-ctx.Done():
	}
	r.mu.Lock()
	ops := make([]*OpHandle, 0, len(r.ops))
	for _, h := range r.ops {
		ops = append(ops, h)
	}
	r.mu.Unlock()
	for _, h := range ops {
		select {
		case <-h.done:
		case <-ctx.Done():
		}
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var ids []podid.ID
	err := r.e.st.InTx(rctx, func(tx *store.Store) error {
		var err error
		ids, err = tx.ReleaseOpsOf(rctx, r.e.id)
		return err
	})
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		r.e.log.Info("released the ops of the stopping instance", "sandboxes", len(ids))
	}
	return nil
}

// run is an op's goroutine: call and settle, then finish (or roll back
// when nothing was sent). What the op leaves behind, the processing of
// its tombstones and the sync it asks for, has begun when its waiters
// return; neither waits.
func (r *runner) run(starter context.Context, h *OpHandle) {
	defer r.wg.Done()
	ctx, cancel := r.e.detached(starter, h.deadline)
	res, obs, sent := r.e.execute(ctx, h)
	cancel()
	var result OpResult
	var after afterCommit
	if !sent {
		// A connection that could not be had leaves cause nil.
		var cause *prepareError
		errors.As(res.err, &cause)
		result = r.rollback(h, cause)
	} else {
		result, after = r.finish(h, res, obs)
	}
	dirty := h.settle()
	r.mu.Lock()
	if r.ops[h.id] == h {
		delete(r.ops, h.id)
	}
	r.mu.Unlock()
	h.res = result
	r.e.processAfterCommit(after)
	if dirty || after.sync {
		r.e.requestSync(h.id)
	}
	close(h.done)
}

// callResult is what an op's node calls answered.
type callResult struct {
	err error
	// skipped: a repair made no call: it found the pod no longer in the
	// state it was begun for, or its prepare hook failed before its first
	// call (err is then the *prepareError); the observation decides alone.
	skipped bool
	// postponed: a repair whose StopPod met the VM busy (vmBusy): it left
	// the pod as it was and did not start it; err is the busy answer.
	postponed bool
	// restored: a restore's RestorePodResponse arrived; memoryRestored
	// and reason are its fields.
	restored       bool
	memoryRestored bool
	reason         string
	checkpoint     *podv1.Checkpoint
	// pod is the pod a CreatePod or RestorePod answered with.
	pod *podv1.PodInfo
}

// execute runs the op's node calls and waits for the pod to settle. sent
// is false when nothing was sent: no connection to the target could be
// had, or what comes before the pod call failed (prepareSpec before the
// connection, prepareNode after it: a *prepareError).
func (e *Engine) execute(ctx context.Context, h *OpHandle) (callResult, observation, bool) {
	if e.connHook != nil {
		if err := e.connHook(h); err != nil {
			return callResult{err: err}, observation{}, false
		}
	}
	if err := e.prepareSpec(ctx, h); err != nil {
		return callResult{err: err}, observation{}, false
	}
	pc, err := e.podClient(ctx, h.target)
	if err != nil {
		return callResult{err: err}, observation{}, false
	}
	if err := e.prepareNode(ctx, h); err != nil {
		return callResult{err: err}, observation{}, false
	}
	res := e.callOp(ctx, pc, h)
	if res.pod != nil {
		e.noteAddress(h, res.pod)
	}
	obs := e.watchPod(ctx, h.target, h.id, h.inc, func() {
		mctx, cancel := context.WithTimeout(e.bg, 10*time.Second)
		defer cancel()
		if _, err := e.st.MarkPending(mctx, h.id, h.inc); err != nil {
			e.rl.warn("pending-since", "cannot record a pod's first PENDING observation", "sandbox", h.id, "err", err)
		}
	})
	if !obs.answered {
		// The deadline came before the node answered: one more question,
		// so that a node that answers now is not taken for one that never
		// did.
		gctx, cancel := context.WithTimeout(e.bg, e.t.settleGrace)
		if o, err := e.getPod(gctx, h.target, h.id); err == nil {
			obs = o
		}
		cancel()
	}
	return res, obs, true
}

// callOp makes the op's calls (§9 ops table).
func (e *Engine) callOp(ctx context.Context, pc podv1.PodServiceClient, h *OpHandle) callResult {
	id := h.id.String()
	var res callResult
	switch h.kind {
	case OpCreate:
		resp, err := pc.CreatePod(ctx, &podv1.CreatePodRequest{PodId: id, Spec: h.spec, ImageLayers: h.layers,
			PullToken: h.pullToken})
		res.err = err
		if err == nil {
			res.pod = resp.GetPod()
		}
	case OpStart:
		_, res.err = pc.StartPod(ctx, &podv1.StartPodRequest{PodId: id, PullToken: e.startToken(ctx, h.row)})
	case OpResume:
		_, res.err = pc.ResumePod(ctx, &podv1.ResumePodRequest{PodId: id})
	case OpRestore:
		resp, err := pc.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id, HeadKey: h.row.CheckpointHead})
		res.err = err
		if err == nil {
			res.restored, res.memoryRestored, res.reason = true, resp.GetMemoryRestored(), resp.GetReason()
			res.pod = resp.GetPod()
		}
	case OpPause:
		_, res.err = pc.SuspendPod(ctx, &podv1.SuspendPodRequest{PodId: id})
	case OpHibernate:
		// The op's id names the call, so that a resume preempts only a
		// hibernation its own call reached (Ensure), and feosd fails a late
		// call of a hibernation it abandoned.
		resp, err := pc.HibernatePod(ctx, &podv1.HibernatePodRequest{PodId: id, RequestId: h.opID.String(),
			NoAbandon: h.noAbandon})
		res.err = err
		if err == nil {
			res.checkpoint = resp.GetCheckpoint()
		}
	case OpRepair:
		res = e.callRepair(ctx, pc, h)
	}
	return res
}

// callRepair repairs a pod stuck PENDING or whose guest stays
// unreachable, or restarts one whose container exited (Restart): StopPod
// then StartPod; a pod stuck in a restore ("restoring from …") is deleted
// and restored from its checkpoint again, unless it has volumes (a
// deleted pod's volume would no longer restore). A pod the node does not
// hold, or one of another incarnation, is left as it is; so is one no
// longer in the state the repair was begun for (it came up, or was
// stopped, meanwhile), so that a healthy pod is never restarted. A
// takeover may find the pod stopped halfway by the first owner and starts
// it; it leaves one that the first owner brought up running. A StopPod
// that meets the VM busy postpones the repair: no StartPod follows. A row with a
// template has the node prepared before the pod calls; a failure there
// makes none (skipped), and the observation decides.
func (e *Engine) callRepair(ctx context.Context, pc podv1.PodServiceClient, h *OpHandle) callResult {
	id := h.id.String()
	var obs observation
	backoff := e.t.retryMin
	for {
		o, err := e.getPod(ctx, h.target, h.id)
		if err == nil {
			obs = o
			break
		}
		if !sleepCtx(ctx, backoff) {
			return callResult{err: err}
		}
		backoff = min(backoff*2, e.t.retryMax)
	}
	if !obs.ours(h.inc) {
		return callResult{skipped: true}
	}
	var res callResult
	info := obs.info
	switch st := info.GetState(); h.read.state {
	case model.StateCreating:
		if st == podv1.PodState_POD_STATE_RUNNING || (st != podv1.PodState_POD_STATE_PENDING && !h.takeover) {
			return callResult{skipped: true}
		}
	case model.StateRunning:
		if st != podv1.PodState_POD_STATE_RUNNING && !h.takeover {
			return callResult{skipped: true}
		}
	}
	if err := e.prepareRepair(ctx, h); err != nil {
		return callResult{err: err, skipped: true}
	}
	if info.GetState() == podv1.PodState_POD_STATE_PENDING && strings.HasPrefix(info.GetReason(), "restoring from") &&
		h.row.CheckpointHead != "" && !hasVolumes(h.row) {
		if _, err := pc.DeletePod(ctx, &podv1.DeletePodRequest{PodId: id}); err != nil && grpcCode(err) != codes.NotFound {
			res.err = err
		}
		resp, err := pc.RestorePod(ctx, &podv1.RestorePodRequest{PodId: id, HeadKey: h.row.CheckpointHead})
		if err != nil {
			res.err = err
		} else {
			res.restored, res.memoryRestored, res.reason = true, resp.GetMemoryRestored(), resp.GetReason()
		}
		return res
	}
	if _, err := pc.StopPod(ctx, &podv1.StopPodRequest{PodId: id}); err != nil {
		res.err = err
		if vmBusy(err) {
			// The VM took no change (a late snapshot): the pod runs on as
			// it was, its processes too, and the repair is tried again.
			res.postponed = true
			return res
		}
	}
	if _, err := pc.StartPod(ctx, &podv1.StartPodRequest{PodId: id, PullToken: e.startToken(ctx, h.row)}); err != nil {
		res.err = err
	}
	return res
}

// errNoTarget is a caller's mistake: an op without a node.
var errNoTarget = errors.New("sandbox: an op without a target node")
