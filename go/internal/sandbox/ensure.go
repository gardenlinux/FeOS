// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Ensure brings the sandbox to running and returns its lease (§9
// "Ensure"): before every tool call, forward and explicit resume. It is
// bounded by the preparation budget or ctx, whichever ends first. It
// starts at most one op of each kind per node per call, and returns the
// error of an op it started or joined that failed, instead of trying
// again: "<op> on node <n> failed: <description>". Its ops carry the
// trigger ensure; they run on when Ensure stops waiting.
//
// A hibernate in flight of a paused sandbox whose node says that a resume
// abandons the hibernation now (feosd's PodInfo.hibernation: abandonable,
// and the op's own call among its request ids) is preempted, whoever owns
// it: a resume takes its place, and the sandbox resumes on its node from
// its snapshot. A drain's hibernate, and Ensure's off a draining node,
// are never abandoned. Any other hibernate in flight (this instance's,
// another live instance's, or one Ensure began itself; also every one on
// a feosd that does not report its hibernations) is waited for at most
// hibernateWait: a hibernation uploads the pod's disk and memory to the
// object store and takes minutes, longer than a caller should wait. One
// that has not ended by then is ErrBusy, "sandbox <id> is hibernating;
// retry in a minute", and goes on; a later Ensure restores the sandbox.
func (e *Engine) Ensure(ctx context.Context, id podid.ID) (model.Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, e.t.prepBudget)
	defer cancel()
	en := &ensurer{e: e, id: id, ops: map[string]OpResult{}, tombTried: map[store.TombstoneKey]bool{},
		preemptRefused: map[uuid.UUID]bool{}}
	ch, unsubscribe := e.d.Subscribe(db.ChanSandbox)
	defer unsubscribe()
	en.changes = ch
	return en.run(ctx)
}

// ensurer is one call of Ensure.
type ensurer struct {
	e       *Engine
	id      podid.ID
	changes <-chan string
	// ops are the results of the ops this call started or joined, by
	// "<kind>@<node>".
	ops map[string]OpResult
	// last is the last op's failure, for a bound that stops the loop.
	last error
	// waitNext: wait for the row to change (or the poll) before the next
	// read.
	waitNext bool
	// syncedGen is the gen a Sync of this call left; -1 when none ran.
	syncedGen  int64
	lostSynced bool
	tombTried  map[store.TombstoneKey]bool
	lastDesc   string
	// hibernateSince is when this call first met a hibernate in flight;
	// it waits for hibernates until hibernateWait after it.
	hibernateSince time.Time
	// preemptNext is when this call may next ask the node whether a resume
	// abandons the hibernate in flight; preemptMaybe: the last answer may
	// still change (no hibernation reported yet, the op's call not in it
	// yet, no answer), so the waits for the hibernate end by then.
	preemptNext  time.Time
	preemptMaybe bool
	// preemptRefused are the hibernates whose preempting resume this call
	// had refused by its admission: not begun again.
	preemptRefused map[uuid.UUID]bool
}

// errContinue makes the loop read the row again.
var errContinue = errors.New("continue")

// errNoPreempt: Ensure does not preempt the hibernate in flight, and waits
// for it.
var errNoPreempt = errors.New("no preempt")

func (en *ensurer) run(ctx context.Context) (model.Lease, error) {
	e := en.e
	en.syncedGen = -1
	for {
		if en.waitNext {
			en.waitChange(ctx)
			en.waitNext = false
		}
		if err := ctx.Err(); err != nil {
			return model.Lease{}, en.timedOut(err)
		}
		row, err := e.st.GetSandbox(ctx, en.id)
		if err != nil {
			if ctx.Err() != nil {
				return model.Lease{}, en.timedOut(ctx.Err())
			}
			return model.Lease{}, err
		}
		if en.syncedGen >= 0 && row.Gen == en.syncedGen && row.Op == "" {
			// The last Sync changed nothing: wait for a change or the
			// poll before acting on the same row again.
			en.syncedGen = -1
			en.waitChange(ctx)
			continue
		}
		en.syncedGen = -1
		en.lastDesc = row.StatusDescription
		lease, err := en.step(ctx, row)
		if errors.Is(err, errContinue) {
			continue
		}
		if err != nil && ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return model.Lease{}, en.timedOut(ctx.Err())
		}
		return lease, err
	}
}

// timedOut is the error of an Ensure whose budget or ctx ended.
func (en *ensurer) timedOut(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		desc := en.lastDesc
		if desc == "" {
			desc = "not running yet"
		}
		return fmt.Errorf("sandbox %s did not get ready in time: %s: %w", en.id, desc, err)
	}
	return err
}

// waitChange waits for a notification of the sandbox (or "" or "*"), the
// poll timer, or ctx.
func (en *ensurer) waitChange(ctx context.Context) {
	t := time.NewTimer(en.e.t.waitPoll)
	defer t.Stop()
	key := en.id.String()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			return
		case p := <-en.changes:
			if p == key || p == "" || p == "*" {
				return
			}
		}
	}
}

// step acts on the row as read: it returns the lease, an error, or
// errContinue to read the row again.
func (en *ensurer) step(ctx context.Context, row store.Sandbox) (model.Lease, error) {
	e := en.e
	if row.Op != "" {
		return model.Lease{}, en.opInFlight(ctx, row)
	}
	switch row.ClientState {
	case model.StateRunning:
		return en.running(ctx, row)
	case model.StateCreating:
		return model.Lease{}, en.creating(ctx, row)
	case model.StatePaused:
		return model.Lease{}, en.paused(ctx, row)
	case model.StateHibernated:
		return model.Lease{}, en.place(ctx, row, OpRestore)
	case model.StateStopped, model.StateFailed:
		return model.Lease{}, en.stopped(ctx, row)
	case model.StatePending:
		auth, err := e.registryAuth(ctx, row)
		if err != nil {
			return model.Lease{}, err
		}
		if row.ImageRef == "" {
			ref, err := e.resolveImage(ctx, row.Image, auth)
			if err != nil {
				return model.Lease{}, fmt.Errorf("resolve image %s: %w", row.Image, err)
			}
			row.ImageRef = ref
		}
		if err := en.askRegistry(ctx, row, auth); err != nil {
			return model.Lease{}, err
		}
		return model.Lease{}, en.place(ctx, row, OpCreate)
	case model.StateLost:
		return model.Lease{}, en.lost(ctx, row)
	}
	// unknown, and a transitional state without an op, which cannot
	// persist: the truth is to be asked for.
	return model.Lease{}, en.unknown(ctx, row)
}

// opInFlight joins this instance's op when it is the row's, takes over
// one whose owner is not live, or waits for the row to change; a hibernate
// that a resume abandons now it preempts, whoever owns it, and waits for
// another of a live owner only within the hibernate wait. An op of this
// instance's that is not the row's (taken over by another instance, or of
// an earlier incarnation) is not waited for. An op the row says is this
// instance's and that it does not run is orphaned, and taken over as a
// dead owner's: the takeover's begin, under the sandbox's begin lock,
// joins it instead when its begin had not registered it yet.
func (en *ensurer) opInFlight(ctx context.Context, row store.Sandbox) error {
	e := en.e
	if h := e.runner.Running(en.id); h != nil && h.opID == row.OpID {
		if h.kind == OpHibernate {
			if err := en.preempt(ctx, row); !errors.Is(err, errNoPreempt) {
				return err
			}
		}
		return en.await(ctx, h)
	}
	live := false
	if row.OpOwner != e.id {
		var err error
		if live, err = e.st.InstanceLive(ctx, row.OpOwner); err != nil {
			return err
		}
	}
	if OpKind(row.Op) == OpHibernate {
		// A dead owner's hibernate that a resume abandons is preempted
		// rather than taken over.
		if err := en.preempt(ctx, row); !errors.Is(err, errNoPreempt) {
			return err
		}
		if live {
			return en.waitHibernateRow(ctx)
		}
	}
	if live {
		en.waitNext = true
		return errContinue
	}
	// A takeover whose op ended meanwhile fails its precondition, and the
	// loop reads the row again (runOp).
	return en.runOp(ctx, row, OpIntent{Kind: OpKind(row.Op), Trigger: TriggerEnsure, Target: row.OpTarget,
		Incarnation: row.Incarnation, takeover: row.OpID})
}

// preempt begins a resume in the place of the row's hibernate when its
// node says that a resume abandons the hibernation now: the hibernate is
// a paused sandbox's on its node, the node is connected and not draining,
// and GetPod reports the hibernation abandonable with the op's own call
// among its request ids (a call still on its way, or a hibernation an
// earlier call left, is not this op's: feosd would start the op's late
// call anew behind the resume). It returns errNoPreempt when it begins
// nothing, and the caller waits for the hibernate as before; it asks the
// node at most once per preemptRecheck, and not again for a hibernate
// whose resume this call had refused.
func (en *ensurer) preempt(ctx context.Context, row store.Sandbox) error {
	e := en.e
	if OpKind(row.Op) != OpHibernate || row.OpFromState != model.StatePaused || row.ClientState != model.StateHibernating ||
		row.OpTarget != row.Node || en.preemptRefused[row.OpID] {
		en.preemptMaybe = false
		return errNoPreempt
	}
	if time.Now().Before(en.preemptNext) {
		return errNoPreempt
	}
	en.preemptNext = time.Now().Add(e.t.preemptRecheck)
	n, err := e.st.GetNode(ctx, row.Node)
	if err != nil || n.Draining || !e.conns.Connected(row.Node) {
		en.preemptMaybe = false
		return errNoPreempt
	}
	gctx, cancel := context.WithTimeout(ctx, e.t.preemptGet)
	obs, err := e.getPod(gctx, row.Node, row.ID)
	cancel()
	if err != nil {
		// The node may answer the next look.
		en.preemptMaybe = true
		return errNoPreempt
	}
	if !obs.ours(row.Incarnation) {
		en.preemptMaybe = false
		return errNoPreempt
	}
	hb := obs.info.GetHibernation()
	ownCall := slices.Contains(hb.GetRequestIds(), row.OpID.String())
	// No hibernation on the node yet, or not this op's call in it yet (the
	// call is on its way), or a feosd that does not report hibernations:
	// look again within preemptRecheck.
	en.preemptMaybe = (hb == nil || !ownCall) && obs.state() != podv1.PodState_POD_STATE_HIBERNATED
	if !hb.GetAbandonable() || !ownCall {
		return errNoPreempt
	}
	err = en.runOp(ctx, row, OpIntent{Kind: OpResume, Trigger: TriggerEnsure, Target: row.Node,
		Incarnation: row.Incarnation, preempt: row.OpID})
	var a *admission
	if errors.As(err, &a) {
		en.preemptRefused[row.OpID] = true
		return errNoPreempt
	}
	return err
}

// running returns the lease of a running sandbox, or moves it off a
// draining node.
func (en *ensurer) running(ctx context.Context, row store.Sandbox) (model.Lease, error) {
	e := en.e
	n, err := e.st.GetNode(ctx, row.Node)
	if err != nil && !errors.Is(err, store.ErrNoRow) {
		return model.Lease{}, err
	}
	if err == nil && n.Draining {
		return model.Lease{}, en.drainingRunning(ctx, row, n)
	}
	if e.conns.Connected(row.Node) {
		return model.Lease{SandboxID: row.ID, Incarnation: row.Incarnation, Node: row.Node}, nil
	}
	if err := e.conns.WaitConnected(ctx, row.Node); err != nil {
		return model.Lease{}, errorf(model.ErrNodeUnavailable, "node %s is not connected to instance %s", row.Node, e.id)
	}
	return model.Lease{}, errContinue
}

// drainingRunning starts no new work on a draining node: a sandbox
// without an exec hold is paused and hibernated, to be restored elsewhere
// (its forwards are closed as it leaves running); one whose execs still
// run waits until the row moves, within --drain-timeout of the drain's
// start, past which the drain cancels them.
func (en *ensurer) drainingRunning(ctx context.Context, row store.Sandbox, n store.Node) error {
	e := en.e
	if !hasObjectStore(n) {
		return errorf(model.ErrNoCapacity, "node %s is draining", n.Name)
	}
	held, err := e.st.HasLiveHold(ctx, row.ID, model.HoldExec)
	if err != nil {
		return err
	}
	if !held {
		err := en.runOp(ctx, row, OpIntent{Kind: OpPause, Trigger: TriggerEnsure, Target: row.Node, Incarnation: row.Incarnation})
		if !errors.Is(err, model.ErrPrecondition) {
			return err
		}
	}
	// Held: wait until the row moves, within the budget.
	err = e.d.WaitFor(ctx, db.ChanSandbox, row.ID.String(), e.t.waitPoll, func(ctx context.Context) (bool, error) {
		cur, err := e.st.GetSandbox(ctx, row.ID)
		if err != nil {
			return false, err
		}
		return cur.Gen != row.Gen || cur.Op != "", nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return errorf(model.ErrNoCapacity, "node %s is draining: the sandbox is evacuating; retry shortly", n.Name)
		}
		return err
	}
	return errContinue
}

// creating waits for a pod observed PENDING without an op, then syncs;
// one PENDING for longer than the repair threshold is repaired.
func (en *ensurer) creating(ctx context.Context, row store.Sandbox) error {
	e := en.e
	due, err := e.st.OlderThan(ctx, row.PendingSince, e.cfg.PendingRepairAfter)
	if err != nil {
		return err
	}
	if due {
		return en.runOp(ctx, row, OpIntent{Kind: OpRepair, Trigger: TriggerEnsure, Target: row.Node, Incarnation: row.Incarnation})
	}
	if row.Node == "" {
		return en.syncAndContinue(ctx, row)
	}
	if !e.conns.Connected(row.Node) {
		if err := e.conns.WaitConnected(ctx, row.Node); err != nil {
			return errorf(model.ErrNodeUnavailable, "node %s is not connected to instance %s", row.Node, e.id)
		}
	}
	wctx := ctx
	if !row.PendingSince.IsZero() {
		now, err := e.st.Now(ctx)
		if err != nil {
			return err
		}
		left := e.cfg.PendingRepairAfter - now.Sub(row.PendingSince)
		var cancel context.CancelFunc
		wctx, cancel = context.WithTimeout(ctx, max(left, 0)+time.Second)
		defer cancel()
	}
	e.watchPod(wctx, row.Node, row.ID, row.Incarnation, nil)
	return en.syncAndContinue(ctx, row)
}

// syncAndContinue syncs the sandbox and reads it again (after a wait when
// the Sync changed nothing).
func (en *ensurer) syncAndContinue(ctx context.Context, row store.Sandbox) error {
	if err := en.e.Sync(ctx, row.ID); err != nil && ctx.Err() == nil {
		en.e.rl.warn("sync", "a sync failed", "sandbox", row.ID, "err", err)
		en.waitNext = true
	}
	cur, err := en.e.st.GetSandbox(ctx, row.ID)
	if err == nil && cur.Gen == row.Gen {
		en.syncedGen = cur.Gen
	}
	return errContinue
}

// paused resumes a paused sandbox on its node; a node that refuses it
// (capacity, draining) hibernates it when it has an object store, to be
// restored elsewhere.
func (en *ensurer) paused(ctx context.Context, row store.Sandbox) error {
	e := en.e
	n, err := e.st.GetNode(ctx, row.Node)
	if err != nil && !errors.Is(err, store.ErrNoRow) {
		return err
	}
	if err == nil && n.Draining && hasObjectStore(n) {
		return en.runOp(ctx, row, OpIntent{Kind: OpHibernate, Trigger: TriggerEnsure, Target: row.Node, Incarnation: row.Incarnation})
	}
	err = en.runOp(ctx, row, OpIntent{Kind: OpResume, Trigger: TriggerEnsure, Target: row.Node, Incarnation: row.Incarnation})
	return en.refusedOnNode(ctx, row, err)
}

// stopped starts a stopped or failed sandbox on its node, or hibernates
// it (its disk) off a draining node.
func (en *ensurer) stopped(ctx context.Context, row store.Sandbox) error {
	e := en.e
	n, err := e.st.GetNode(ctx, row.Node)
	if errors.Is(err, store.ErrNoRow) || (err == nil && n.Status != model.NodeReady) {
		status := model.NodeDown
		if err == nil && n.Status != model.NodeRemoved {
			status = n.Status
		}
		return errorf(model.ErrNodeUnavailable, "node %s is %s", row.Node, status)
	}
	if err != nil {
		return err
	}
	if n.Draining {
		if !hasObjectStore(n) {
			return errorf(model.ErrNoCapacity, "node %s is draining", row.Node)
		}
		return en.runOp(ctx, row, OpIntent{Kind: OpHibernate, Trigger: TriggerEnsure, Target: row.Node, Incarnation: row.Incarnation})
	}
	return en.runOp(ctx, row, OpIntent{Kind: OpStart, Trigger: TriggerEnsure, Target: row.Node, Incarnation: row.Incarnation})
}

// refusedOnNode handles a refused resume on the sandbox's node: with an
// object store there, the paused sandbox is hibernated and placed
// elsewhere.
func (en *ensurer) refusedOnNode(ctx context.Context, row store.Sandbox, err error) error {
	var a *admission
	if !errors.As(err, &a) {
		return err
	}
	switch a.refusal.gate {
	case gateDraining, gateSlots, gateCPU, gateMemory:
	default:
		return err
	}
	n, nerr := en.e.st.GetNode(ctx, row.Node)
	if nerr != nil || !hasObjectStore(n) {
		return err
	}
	return en.runOp(ctx, row, OpIntent{Kind: OpHibernate, Trigger: TriggerEnsure, Target: row.Node, Incarnation: row.Incarnation})
}

// registryRetry is when a caller whose create the image's registry did
// not answer should come back.
const registryRetry = 15 * time.Second

// askRegistry asks the registry of the row's pinned image whether the
// create may pull it with auth, the row's credentials, before the create
// claims a node (ADR 0025): every create of a new pod asks, cached image
// or not. A refusal, or an image the registry does not have, fails the
// create with the answer; no answer is no as well, retryable. Either says
// why in the row's status.
func (en *ensurer) askRegistry(ctx context.Context, row store.Sandbox, auth model.RegistryCredentials) error {
	e := en.e
	err := e.imageAccess()(ctx, row.ImageRef, auth)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	var na *noAnswerError
	var out error
	switch {
	case errors.As(err, &na):
		out = laterf(registryRetry, model.ErrRegistryUnavailable,
			"image access not confirmed (retryable): %s: %v", row.ImageRef, err)
	case errors.Is(err, errImageNotThere):
		out = errorf(model.ErrImageRefused, "image %s: %v for this create", row.ImageRef, err)
	default:
		out = errorf(model.ErrImageRefused, "image %s: %v", row.ImageRef, err)
	}
	if serr := e.st.SetStatusDescription(ctx, row.ID, row.Incarnation, out.Error()); serr != nil {
		return serr
	}
	return out
}

// hasObjectStore reports whether a node can hibernate pods.
func hasObjectStore(n store.Node) bool { return n.ObjectStoreID != "" || n.ObjectStore }

// place places a pending sandbox (create) or a hibernated one (restore):
// the candidates of a snapshot, without the nodes where this call already
// ran the same op; a node that only a tombstone of the pod keeps it off
// has the tombstone processed first, and is placed on again.
func (en *ensurer) place(ctx context.Context, row store.Sandbox, kind OpKind) error {
	e := en.e
	p, err := e.place(ctx, row, kind)
	if err != nil {
		return err
	}
	var cands []string
	for _, c := range p.candidates {
		if _, ran := en.ops[opKey(kind, c)]; !ran {
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		if len(p.candidates) > 0 && en.last != nil {
			return en.last
		}
		if len(p.tombstoneOnly) > 0 {
			cleared, err := en.clearTombstones(ctx, row, p.tombstoneOnly)
			if err != nil {
				return err
			}
			if cleared {
				return errContinue
			}
		}
		desc := p.noCapacity()
		if err := e.st.SetStatusDescription(ctx, row.ID, row.Incarnation, desc); err != nil {
			return err
		}
		return errorf(model.ErrNoCapacity, "%s", desc)
	}
	err = en.runOp(ctx, row, OpIntent{Kind: kind, Trigger: TriggerEnsure, Candidates: cands, Incarnation: row.Incarnation,
		ImageRef: row.ImageRef, verdicts: p.verdicts})
	var ke *kindError
	if errors.As(err, &ke) && ke.kind == model.ErrNoCapacity {
		// Every candidate's claim was refused under its lock.
		if serr := e.st.SetStatusDescription(ctx, row.ID, row.Incarnation, err.Error()); serr != nil {
			return serr
		}
	}
	return err
}

// clearTombstones has the tombstones of the pod on nodes processed (each
// once per call), waiting for one whose not_before lies ahead within the
// budget. It reports whether one went.
func (en *ensurer) clearTombstones(ctx context.Context, row store.Sandbox, nodes []string) (bool, error) {
	e := en.e
	for {
		ts, err := e.st.TombstonesOfPod(ctx, row.ID)
		if err != nil {
			return false, err
		}
		var due []store.Tombstone
		var ahead time.Time
		left := false
		for _, t := range ts {
			if !slices.Contains(nodes, t.Node) {
				continue
			}
			left = true
			if en.tombTried[t.Key()] || !e.conns.Connected(t.Node) {
				continue
			}
			now, err := e.st.Now(ctx)
			if err != nil {
				return false, err
			}
			if t.NotBefore.After(now) {
				if ahead.IsZero() || t.NotBefore.Before(ahead) {
					ahead = t.NotBefore
				}
				continue
			}
			due = append(due, t)
		}
		if !left {
			// Processed meanwhile (by whoever wrote them, or the
			// leader): the gate is open.
			return true, nil
		}
		if len(due) == 0 && ahead.IsZero() {
			return false, nil
		}
		if len(due) == 0 {
			now, err := e.st.Now(ctx)
			if err != nil {
				return false, err
			}
			if !sleepCtx(ctx, min(ahead.Sub(now)+50*time.Millisecond, time.Second)) {
				return false, ctx.Err()
			}
			continue
		}
		cleared := false
		for _, t := range due {
			en.tombTried[t.Key()] = true
			if err := e.processTombstone(t.Key()).wait(ctx); err == nil {
				cleared = true
			} else if ctx.Err() != nil {
				return false, ctx.Err()
			}
		}
		return cleared, nil
	}
}

// unknown syncs a sandbox whose truth is not known, through its node.
func (en *ensurer) unknown(ctx context.Context, row store.Sandbox) error {
	e := en.e
	if row.Node == "" {
		return errorf(model.ErrNodeUnavailable, "sandbox %s is %s on no node: %s", row.ID, row.ClientState, row.StatusDescription)
	}
	if !e.conns.Connected(row.Node) {
		if err := e.conns.WaitConnected(ctx, row.Node); err != nil {
			return errorf(model.ErrNodeUnavailable, "node %s is unavailable: %s", row.Node, row.StatusDescription)
		}
	}
	return en.syncAndContinue(ctx, row)
}

// lost syncs a lost sandbox once when its node is ready and connected (it
// may have come back with the pod); still lost, it is lost.
func (en *ensurer) lost(ctx context.Context, row store.Sandbox) error {
	e := en.e
	if !en.lostSynced && row.Node != "" && e.conns.Connected(row.Node) {
		if n, err := e.st.GetNode(ctx, row.Node); err == nil && n.Status == model.NodeReady {
			en.lostSynced = true
			if err := e.Sync(ctx, row.ID); err != nil && ctx.Err() == nil {
				e.rl.warn("sync", "a sync failed", "sandbox", row.ID, "err", err)
			}
			return errContinue
		}
	}
	return model.ErrLost
}

// opKey names an op of this call for its bound.
func opKey(kind OpKind, node string) string { return string(kind) + "@" + node }

// runOp starts an op (or joins this instance's) for the row as this call
// read it, and waits for it. It returns errContinue when the loop is to
// read the row again, and the op's failure otherwise.
func (en *ensurer) runOp(ctx context.Context, row store.Sandbox, in OpIntent) error {
	e := en.e
	if pinnedTarget(in) != "" {
		if _, ran := en.ops[opKey(in.Kind, in.Target)]; ran {
			if en.last != nil {
				return en.last
			}
			return fmt.Errorf("%s on node %s did not bring sandbox %s up", in.Kind, in.Target, en.id)
		}
	}
	h, err := e.runner.Start(ctx, en.id, in)
	switch {
	case err == nil:
	case errors.Is(err, model.ErrBusy):
		en.waitNext = true
		return errContinue
	case errors.Is(err, model.ErrPrecondition):
		return en.preconditionFailed(ctx, row, err)
	default:
		return err
	}
	return en.await(ctx, h)
}

// preconditionFailed judges a begin whose precondition failed. When the
// row moved since this call read it (an op of another instance began or
// ended between the read and the begin's lock: a resume, a create, a
// pause), the loop reads it again and acts on what it is now. When it did
// not move, a recheck failed on the row as read (a hold, an age), and the
// caller gets the error.
func (en *ensurer) preconditionFailed(ctx context.Context, read store.Sandbox, err error) error {
	cur, gerr := en.e.st.GetSandbox(ctx, en.id)
	if gerr != nil {
		return gerr
	}
	if cur.Incarnation != read.Incarnation || cur.Gen != read.Gen || cur.Op != "" {
		return errContinue
	}
	return err
}

// hibernateDeadline is when this call stops waiting for hibernates:
// hibernateWait after the first one it met.
func (en *ensurer) hibernateDeadline() time.Time {
	if en.hibernateSince.IsZero() {
		en.hibernateSince = time.Now()
	}
	return en.hibernateSince.Add(en.e.t.hibernateWait)
}

// hibernatingRetry is when a caller told that the sandbox is hibernating
// should come back: a hibernation takes minutes, and a later Ensure
// restores the sandbox.
const hibernatingRetry = time.Minute

// hibernating is the answer of a call whose hibernate did not end within
// the wait: ErrBusy, retryable after a minute (Retry-After: 60 where a
// door sends one); the op goes on.
func (en *ensurer) hibernating() error {
	return laterf(hibernatingRetry, model.ErrBusy, "sandbox %s is hibernating; retry in a minute", en.id)
}

// waitHibernateOp waits for a hibernate this instance runs (joined, or
// begun by this call, a takeover among them) until it ends, within the
// hibernate wait. The end of ctx's deadline counts as the wait's: Ensure
// never waits longer than its budget, and its caller learns that the
// sandbox is hibernating. A hibernate that has ended when the wait ends
// has ended, as the row path's read after the wait finds. While the
// node's answer to a preempt may still change, the wait ends after
// preemptRecheck with errContinue, so that the loop looks again.
func (en *ensurer) waitHibernateOp(ctx context.Context, h *OpHandle) error {
	select {
	case <-h.Done():
		return nil
	default:
	}
	deadline, capped := en.hibernateDeadline(), false
	if next := time.Now().Add(en.e.t.preemptRecheck); en.preemptMaybe && next.Before(deadline) {
		deadline, capped = next, true
	}
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-h.Done():
		return nil
	case <-t.C:
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ctx.Err()
		}
		capped = false
	}
	select {
	case <-h.Done():
		return nil
	default:
	}
	if capped {
		return errContinue
	}
	return en.hibernating()
}

// waitHibernateRow waits for a hibernate of a live owner that this
// instance does not run (another instance's): for a change of the row (or
// the poll), within the hibernate wait, after which the loop reads the row
// again.
func (en *ensurer) waitHibernateRow(ctx context.Context) error {
	deadline := en.hibernateDeadline()
	if !time.Now().Before(deadline) {
		return en.hibernating()
	}
	wctx, cancel := context.WithDeadline(ctx, deadline)
	en.waitChange(wctx)
	cancel()
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return en.hibernating()
		}
		return err
	}
	return errContinue
}

// await waits for an op this call started or joined and judges its end;
// a hibernate only within the hibernate wait.
func (en *ensurer) await(ctx context.Context, h *OpHandle) error {
	if h.kind == OpHibernate {
		if err := en.waitHibernateOp(ctx, h); err != nil {
			return err
		}
	}
	res, err := h.Wait(ctx)
	if err != nil {
		return err
	}
	if res.fenced {
		return errContinue
	}
	en.ops[opKey(h.kind, h.target)] = res
	if res.busy {
		// The node's VM takes the op later: a later call does it.
		en.last = laterf(busyRetry, model.ErrBusy, "%s on node %s: %s; retry in %s", h.kind, h.target, res.Description,
			busyRetry)
		return en.last
	}
	if res.Err != nil {
		en.last = fmt.Errorf("%s on node %s failed: %w", h.kind, h.target, res.Err)
		return en.last
	}
	if res.State == opGoal(h.kind) {
		return errContinue
	}
	en.last = opFailed(h, res)
	switch {
	case h.kind == OpCreate && res.State == model.StatePending:
		// A create that failed before its pod existed avoids the node
		// from now on: place once more elsewhere.
		row, err := en.e.st.GetSandbox(ctx, en.id)
		if err == nil && slices.Contains(row.AvoidNodes, h.target) {
			return errContinue
		}
	case h.kind == OpRestore && res.State == model.StateHibernated && res.Node == "":
		return errContinue
	}
	return en.last
}

// opFailed is the error of an op that ended short of its goal: "<op> on
// node <n> failed: <status_description>", or the description as it is when
// it names the op and its node already (a failed call's, an unknown
// outcome's).
func opFailed(h *OpHandle, res OpResult) error {
	desc := describe(res)
	prefix := fmt.Sprintf("%s on node %s", h.kind, h.target)
	if rest, ok := strings.CutPrefix(desc, prefix); ok && (strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, " ")) {
		return errors.New(desc)
	}
	return fmt.Errorf("%s failed: %s", prefix, desc)
}

func describe(res OpResult) string {
	if res.Description != "" {
		return res.Description
	}
	return "the sandbox is " + res.State
}

// opGoal is the state an op brings the sandbox to.
func opGoal(kind OpKind) string {
	switch kind {
	case OpPause:
		return model.StatePaused
	case OpHibernate:
		return model.StateHibernated
	}
	return model.StateRunning
}

// Pause is the REST pause (§11): a running sandbox is paused with the
// trigger explicit (no hold or idle check, as the old pause), waiting up
// to a minute (the op goes on when the wait ends); a paused, hibernated or
// pending one has nothing to do; another state is an error. A recheck that
// finds the row changed is ErrPrecondition. An op that began after the
// read (an idle pause, a resume of another instance) is waited for, and
// the sandbox looked at again.
func (e *Engine) Pause(ctx context.Context, id podid.ID) error {
	ctx, cancel := context.WithTimeout(ctx, e.t.pauseWait)
	defer cancel()
	for {
		row, err := e.st.GetSandbox(ctx, id)
		if err != nil {
			return err
		}
		switch row.ClientState {
		case model.StatePaused, model.StateHibernated, model.StatePending:
			return nil
		case model.StateRunning:
		default:
			return fmt.Errorf("sandbox is %s", row.ClientState)
		}
		h, err := e.runner.Start(ctx, id, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: row.Node,
			Incarnation: row.Incarnation})
		if errors.Is(err, model.ErrBusy) {
			if werr := e.waitOpEnd(ctx, row); werr != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if h.kind != OpPause {
			// This instance's op of another kind, which ended after the
			// read: its end is the sandbox's state to look at.
			if _, werr := h.Wait(ctx); werr != nil {
				return fmt.Errorf("sandbox is %s", transitional(h.kind))
			}
			continue
		}
		res, err := h.Wait(ctx)
		if err != nil {
			return nil // the op goes on
		}
		if res.Err != nil {
			return res.Err
		}
		if res.busy {
			return laterf(busyRetry, model.ErrBusy, "pause on node %s: %s; retry in %s", h.target, res.Description,
				busyRetry)
		}
		if !res.fenced && h.kind == OpPause && res.State != model.StatePaused {
			return fmt.Errorf("pause on node %s failed: %s", h.target, describe(res))
		}
		return nil
	}
}

// waitOpEnd waits until the sandbox read has no op in flight (or is gone,
// or another incarnation), or ctx.
func (e *Engine) waitOpEnd(ctx context.Context, read store.Sandbox) error {
	return e.d.WaitFor(ctx, db.ChanSandbox, read.ID.String(), e.t.waitPoll, func(ctx context.Context) (bool, error) {
		cur, err := e.st.GetSandbox(ctx, read.ID)
		if errors.Is(err, model.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return cur.Incarnation != read.Incarnation || cur.Op == "", nil
	})
}

// Resume is the REST resume (§11): Ensure, then activity (Touch).
func (e *Engine) Resume(ctx context.Context, id podid.ID) error {
	if _, err := e.Ensure(ctx, id); err != nil {
		return err
	}
	e.Touch(id)
	return nil
}

// WaitRunning waits for a sandbox whose pod is PENDING (a call into its
// guest found it still starting) until the pod leaves PENDING, syncs it,
// and returns its lease as Ensure does.
func (e *Engine) WaitRunning(ctx context.Context, id podid.ID) (model.Lease, error) {
	row, err := e.st.GetSandbox(ctx, id)
	if err != nil {
		return model.Lease{}, err
	}
	if row.Op == "" && row.Node != "" && e.conns.Connected(row.Node) &&
		(row.ClientState == model.StateRunning || row.ClientState == model.StateCreating) {
		wctx, cancel := context.WithTimeout(ctx, e.t.prepBudget)
		e.watchPod(wctx, row.Node, row.ID, row.Incarnation, nil)
		cancel()
		if err := e.Sync(ctx, id); err != nil && ctx.Err() != nil {
			return model.Lease{}, err
		}
	}
	return e.Ensure(ctx, id)
}
