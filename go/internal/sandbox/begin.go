// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// transitional is the state an op holds the sandbox in while it runs.
func transitional(k OpKind) string {
	switch k {
	case OpResume:
		return model.StateResuming
	case OpPause:
		return model.StatePausing
	case OpHibernate:
		return model.StateHibernating
	}
	return model.StateCreating
}

// admits reports whether an op books its target in its begin (§9 begin
// step 4).
func admits(k OpKind) bool {
	return k == OpCreate || k == OpStart || k == OpResume || k == OpRestore
}

// begin begins an op (§9 "Begin"): for a create or restore with
// candidates, on the first candidate whose claim passes under its lock
// (each candidate in a transaction of its own, so that the transaction
// never holds more than one node row); otherwise on the target.
func (r *runner) begin(ctx context.Context, id podid.ID, in OpIntent) (*OpHandle, error) {
	if pinnedTarget(in) == "" {
		var refused []string
		for _, c := range in.Candidates {
			// Between two transactions nothing is in flight: a caller
			// that is gone tries no further candidate.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			h, err := r.beginOn(ctx, id, in, c)
			if rf, ok := asRefusal(err); ok {
				refused = append(refused, c+": "+rf.text)
				in.verdicts = refuseVerdict(in.verdicts, c, rf.text)
				continue
			}
			return h, err
		}
		return nil, errorf(model.ErrNoCapacity, "no capacity (retryable): no feasible node; %s", strings.Join(refused, "; "))
	}
	if in.Target == "" {
		return nil, errNoTarget
	}
	h, err := r.beginOn(ctx, id, in, in.Target)
	if rf, ok := asRefusal(err); ok {
		return nil, admissionError(rf, id)
	}
	return h, err
}

// refuseVerdict marks node infeasible in the verdicts of a placement whose
// claim it refused.
func refuseVerdict(vs []store.PlacementCandidate, node, reason string) []store.PlacementCandidate {
	out := make([]store.PlacementCandidate, len(vs))
	copy(out, vs)
	for i := range out {
		if out[i].Node == node {
			out[i].Feasible, out[i].Reason = false, reason
		}
	}
	return out
}

// beginTimeout bounds a begin's transaction, which runs on a context of
// its own.
const beginTimeout = 30 * time.Second

// beginOn is one begin transaction towards target. The transaction runs on
// a context of its own, never its caller's: a caller that hangs up, or a
// leader term that ends, while the COMMIT is on its way would leave its
// outcome unknown, and the row in an op of this instance that nobody runs.
// A COMMIT whose answer was lost all the same (the connection broke) is
// judged by the row: an op that committed is this begin's, and runs.
func (r *runner) beginOn(ctx context.Context, id podid.ID, in OpIntent, target string) (*OpHandle, error) {
	e := r.e
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), beginTimeout)
	defer cancel()
	var h *OpHandle
	var events []activity.Event
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		h, events = nil, nil
		row, err := tx.LockSandbox(ctx, id, store.ForUpdate)
		if err != nil {
			return err
		}
		if in.Incarnation != uuid.Nil && row.Incarnation != in.Incarnation {
			return model.ErrNotFound
		}
		if row.Op != "" {
			if in.preempt != uuid.Nil {
				h, events, err = r.preemptHibernate(ctx, tx, row, in, target)
				return err
			}
			if in.takeover != uuid.Nil && row.OpID != in.takeover {
				return errorf(model.ErrPrecondition, "sandbox %s: the op to take over ended", id)
			}
			h, events, err = r.takeover(ctx, tx, row, in.Trigger)
			return err
		}
		if in.takeover != uuid.Nil {
			return errorf(model.ErrPrecondition, "sandbox %s: the op to take over ended", id)
		}
		if in.preempt != uuid.Nil {
			return preemptEnded(id)
		}
		if e.beginHook != nil {
			e.beginHook(ctx, id, in.Kind)
		}
		if err := r.precheck(ctx, tx, row, in, target); err != nil {
			return err
		}
		noAbandon := false
		if in.Kind == OpHibernate {
			if noAbandon, err = r.noAbandon(ctx, tx, in.Trigger, target); err != nil {
				return err
			}
		}
		if admits(in.Kind) {
			if err := e.claim(ctx, tx, row, in.Kind, target); err != nil {
				return err
			}
			if err := tx.UpsertBooking(ctx, store.Booking{PodID: id, Node: target, VCPUs: row.VCPUs,
				MemoryMiB: row.MemoryMiB, Gated: true}); err != nil {
				return err
			}
		}
		if in.Kind == OpCreate || in.Kind == OpRestore {
			// A sandbox whose groups changed gets the template of them
			// before its pod is made (ADR 0035): the prepare hook puts its
			// groups, and the create or restore sends them.
			if row, err = tx.ApplySandboxGroups(ctx, row); err != nil {
				return err
			}
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		d := e.t.opDeadline(in.Kind, row.MemoryMiB)
		after := row
		after.Op, after.OpID, after.OpOwner = string(in.Kind), uuid.New(), e.id
		after.OpDeadline, after.OpTarget, after.OpFromState = now.Add(d), target, row.ClientState
		after.ClientState = transitional(in.Kind)
		after.StatusDescription = in.reason
		if in.Kind == OpCreate || in.Kind == OpRestore {
			after.Node = target
		}
		if after.Node != row.Node {
			// A pod on another node; a policy's backoff was the old node's.
			after.Materialised, after.LostFrom = false, ""
			after.PolicyFailed, after.PolicyFailures, after.PolicyNotBefore = "", 0, time.Time{}
		}
		if in.Kind == OpCreate && row.ImageRef == "" {
			after.ImageRef = in.ImageRef
		}
		var tombs []store.TombstoneKey
		if in.Kind == OpRestore {
			// The record holder, or the node lost grace left the pod on,
			// keeps a pod this restore makes stale.
			for _, old := range []string{row.Node, row.LostFrom} {
				if old == "" || old == target || containsKey(tombs, old) {
					continue
				}
				k := store.TombstoneKey{PodID: id, Node: old, Incarnation: row.Incarnation}
				if _, err := tx.GetTombstone(ctx, k); err == nil {
					continue // an earlier decision's; a rollback leaves it
				} else if !errors.Is(err, store.ErrNoRow) {
					return err
				}
				if _, err := tx.UpsertTombstone(ctx, store.Tombstone{PodID: id, Node: old, Incarnation: row.Incarnation,
					Reason: "restored on " + target}); err != nil {
					return err
				}
				tombs = append(tombs, k)
			}
		}
		written, err := tx.UpdateSandbox(ctx, row, after)
		if err != nil {
			return err
		}
		if _, err := e.syncBooking(ctx, tx, row, written, false); err != nil {
			return err
		}
		if in.Kind == OpCreate || in.Kind == OpRestore {
			// The placement's audit is its activity row.
			verdicts := in.verdicts
			if len(verdicts) == 0 {
				verdicts = []store.PlacementCandidate{{Node: target, Feasible: true}}
			}
			events = append(events, activity.Event{SandboxID: id, TenantID: row.Owner, Node: target,
				Kind: activity.KindPlacement, Name: fmt.Sprintf("placed on %s (%s)", target, in.Kind),
				Detail: map[string]any{"node": target, "op": string(in.Kind), "trigger": string(in.Trigger),
					"candidates": verdicts}})
		}
		h = r.newHandle(written, in.Kind, in.Trigger, target, d)
		h.read.state, h.read.node, h.read.lostFrom, h.read.materialised = row.ClientState, row.Node, row.LostFrom, row.Materialised
		h.tombstones = tombs
		h.noAbandon = noAbandon
		ev := h.startEvent(row)
		if in.reason != "" {
			ev.Detail["reason"] = in.reason
		}
		events = append(events, ev)
		return nil
	})
	if e.beginTxHook != nil {
		err = e.beginTxHook(id, err)
	}
	if err != nil && h != nil && r.committed(ctx, h) {
		// The transaction got to its COMMIT, whose answer was lost.
		e.log.Warn("the answer to a begin's commit was lost; the op committed and runs", "sandbox", id,
			"op", string(h.kind), "err", err)
		err = nil
	}
	if err != nil {
		return nil, err
	}
	e.record(events...)
	// Logged once committed: InTx may run the closure again, and a
	// transaction that does not commit took nothing over.
	switch {
	case h.preempted != uuid.Nil:
		e.log.Info("preempted a hibernate", "sandbox", id, "node", h.target, "owner", h.from, "hibernate", h.preempted)
	case h.takeover:
		e.log.Info("took over an op", "sandbox", id, "op", string(h.kind), "target", h.target, "from", h.from)
	}
	return h, nil
}

// committed reports whether the op of h, whose begin's COMMIT failed,
// committed nonetheless: the row has its op id.
func (r *runner) committed(ctx context.Context, h *OpHandle) bool {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	row, err := r.e.st.GetSandbox(rctx, h.id)
	return err == nil && row.Incarnation == h.inc && row.OpID == h.opID
}

func containsKey(ks []store.TombstoneKey, node string) bool {
	for _, k := range ks {
		if k.Node == node {
			return true
		}
	}
	return false
}

// newHandle makes the handle of an op the begin wrote into row.
func (r *runner) newHandle(row store.Sandbox, kind OpKind, trigger OpTrigger, target string, d time.Duration) *OpHandle {
	now := time.Now()
	return &OpHandle{
		done: make(chan struct{}), id: row.ID, opID: row.OpID, inc: row.Incarnation, kind: kind, trigger: trigger,
		target: target, deadline: now.Add(d), started: now, row: row, span: "op-" + row.OpID.String(), parent: row.RunID,
	}
}

// takeover takes over the op in flight in row when its owner is not live
// (dead or stopping), or is this instance and its runner does not run the
// op (orphaned): the same op towards the same target with a new op id and
// deadline. Nothing but the op's ownership is written: the row is in the
// op's transitional state, and its booking stands. A live owner finishes
// its op by its deadline: ErrBusy. It runs under the sandbox's begin lock
// (begin), so that an op of this instance's that committed is registered
// with the runner by then.
func (r *runner) takeover(ctx context.Context, tx *store.Store, row store.Sandbox, trigger OpTrigger) (*OpHandle, []activity.Event, error) {
	busy := r.runs(row)
	if row.OpOwner != r.e.id {
		live, err := tx.InstanceLive(ctx, row.OpOwner)
		if err != nil {
			return nil, nil, err
		}
		busy = live
	}
	if busy {
		return nil, nil, errorf(model.ErrBusy, "an op %s of instance %s is in flight", row.Op, row.OpOwner)
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, nil, err
	}
	kind := OpKind(row.Op)
	// The trigger of the op taken over is not in the row: a hibernate
	// taken over on a draining node is a drain's, which a resume never
	// abandons. feosd keeps a hibernation unabandonable once any of its
	// calls said so, so a takeover on a node that drains no more cannot
	// make a drain's abandonable while it runs.
	noAbandon := false
	if kind == OpHibernate {
		n, err := tx.GetNode(ctx, row.OpTarget)
		if err != nil && !errors.Is(err, store.ErrNoRow) {
			return nil, nil, err
		}
		noAbandon = err == nil && n.Draining
	}
	d := r.e.t.opDeadline(kind, row.MemoryMiB)
	after := row
	after.OpID, after.OpOwner, after.OpDeadline = uuid.New(), r.e.id, now.Add(d)
	written, err := tx.UpdateSandbox(ctx, row, after)
	if err != nil {
		return nil, nil, err
	}
	h := r.newHandle(written, kind, trigger, row.OpTarget, d)
	h.takeover = true
	h.from = row.OpOwner
	h.noAbandon = noAbandon
	h.read.state, h.read.node, h.read.lostFrom, h.read.materialised = row.OpFromState, row.Node, row.LostFrom, row.Materialised
	ev := h.startEvent(row)
	ev.Name = fmt.Sprintf("%s on node %s (taken over from instance %s)", kind, row.OpTarget, row.OpOwner)
	return h, []activity.Event{ev}, nil
}

// preemptHibernate begins a resume in the place of the hibernate in flight
// in row, whose node said that a resume abandons the hibernation now
// (Ensure's preempt): one transaction replaces the row's op with the
// resume, admitted and booked on the sandbox's node as any resume, owned by
// this instance. The hibernate's owner is not looked at (this instance, a
// live one or a dead one): its op is fenced out by its op id, and its
// finish writes nothing but a newer checkpoint. A row whose hibernate
// ended, or is not a paused sandbox's on its node, fails with
// ErrPrecondition; a refused admission is the *refusal.
func (r *runner) preemptHibernate(ctx context.Context, tx *store.Store, row store.Sandbox, in OpIntent,
	target string) (*OpHandle, []activity.Event, error) {
	e := r.e
	if row.OpID != in.preempt || OpKind(row.Op) != OpHibernate || row.ClientState != model.StateHibernating ||
		row.OpFromState != model.StatePaused || row.Node != target || row.OpTarget != target {
		return nil, nil, preemptEnded(row.ID)
	}
	if err := e.claim(ctx, tx, row, OpResume, target); err != nil {
		return nil, nil, err
	}
	if err := tx.UpsertBooking(ctx, store.Booking{PodID: row.ID, Node: target, VCPUs: row.VCPUs,
		MemoryMiB: row.MemoryMiB, Gated: true}); err != nil {
		return nil, nil, err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, nil, err
	}
	d := e.t.opDeadline(OpResume, row.MemoryMiB)
	after := row
	after.Op, after.OpID, after.OpOwner = string(OpResume), uuid.New(), e.id
	after.OpDeadline, after.OpTarget, after.OpFromState = now.Add(d), target, model.StatePaused
	after.ClientState = transitional(OpResume)
	after.StatusDescription = ""
	written, err := tx.UpdateSandbox(ctx, row, after)
	if err != nil {
		return nil, nil, err
	}
	if _, err := e.syncBooking(ctx, tx, row, written, false); err != nil {
		return nil, nil, err
	}
	h := r.newHandle(written, OpResume, in.Trigger, target, d)
	h.preempted, h.from = row.OpID, row.OpOwner
	h.read.state, h.read.node, h.read.lostFrom, h.read.materialised = model.StatePaused, row.Node, row.LostFrom, row.Materialised
	ev := h.startEvent(row)
	ev.Name = fmt.Sprintf("%s on node %s (abandons the hibernate of instance %s)", OpResume, target, row.OpOwner)
	ev.Detail["from"] = model.StatePaused
	ev.Detail["abandons"] = row.OpID.String()
	return h, []activity.Event{ev}, nil
}

// preemptEnded is the begin's answer when the hibernate a resume was to
// preempt is no longer the row's op.
func preemptEnded(id podid.ID) error {
	return errorf(model.ErrPrecondition, "sandbox %s: the hibernate to preempt ended", id)
}

// noAbandon reports whether a fresh hibernate's call tells feosd that a
// resume may not abandon the hibernation: a drain's (drain, drain_forced),
// Ensure's off a draining node (read in the begin's transaction; not one
// begun because the node had no room for a resume), and an explicit one.
// A hibernate-after's is abandoned by the next resume.
func (r *runner) noAbandon(ctx context.Context, tx *store.Store, trigger OpTrigger, target string) (bool, error) {
	if trigger == TriggerExplicit || trigger == TriggerMove {
		return true, nil
	}
	return r.drainHibernate(ctx, tx, trigger, target)
}

// precheck is begin step 3: the op's preconditions on the locked row, then
// a pause's or hibernate's rechecks per trigger, each a later statement
// with a fresh snapshot, so that a hold admitted while this transaction
// waited for the row lock is seen. A repair needs the sandbox still stuck:
// creating with its pod observed PENDING, or running with its guest
// unreachable; begun by the repair loop or Ensure, for longer than the
// threshold that made them begin it (a Sync that applied RUNNING, or a
// healthy call that cleared unreachable_since, after their read stops it).
// Restart's repair needs only a running row.
func (r *runner) precheck(ctx context.Context, tx *store.Store, row store.Sandbox, in OpIntent, target string) error {
	st := row.ClientState
	ok := false
	pinned := true
	switch in.Kind {
	case OpCreate:
		ok, pinned = st == model.StatePending, false
	case OpStart:
		ok = st == model.StateStopped || st == model.StateFailed
	case OpResume:
		ok = st == model.StatePaused
	case OpRestore:
		ok, pinned = st == model.StateHibernated && row.HasCheckpoint(), false
	case OpPause:
		ok = st == model.StateRunning
	case OpHibernate:
		ok = st == model.StatePaused || (in.Trigger == TriggerMove && movable(st))
		if !ok && (st == model.StateStopped || st == model.StateFailed || st == model.StateRunning) {
			drain, err := r.drainHibernate(ctx, tx, in.Trigger, target)
			if err != nil {
				return err
			}
			ok = drain
		}
	case OpRepair:
		ok = (st == model.StateCreating && !row.PendingSince.IsZero()) ||
			(st == model.StateRunning && !row.UnreachableSince.IsZero()) ||
			(st == model.StateRunning && in.Trigger == TriggerRestart)
	default:
		return fmt.Errorf("sandbox: unknown op %q", in.Kind)
	}
	if !ok {
		return errorf(model.ErrPrecondition, "sandbox %s is %s: no %s", row.ID, st, in.Kind)
	}
	if pinned && target != row.Node {
		return errorf(model.ErrPrecondition, "sandbox %s is on node %q, not %s", row.ID, row.Node, target)
	}
	if in.Kind == OpRepair {
		if in.Trigger != TriggerRepair && in.Trigger != TriggerEnsure {
			return nil
		}
		pass, err := tx.RepairRecheck(ctx, row.ID, r.e.cfg.PendingRepairAfter, r.e.cfg.UnreachableRepairAfter)
		if err != nil || !pass {
			return recheckFailed(row.ID, "it is no longer stuck", err)
		}
		return nil
	}
	if in.Kind != OpPause && in.Kind != OpHibernate {
		return nil
	}
	if in.Trigger == TriggerMove {
		if !row.GroupsPending {
			return recheckFailed(row.ID, "its groups are its pod's", nil)
		}
		return nil
	}
	var pass bool
	var err error
	switch in.Trigger {
	case TriggerIdle:
		if in.Kind != OpPause {
			break
		}
		if pass, err = tx.IdleRecheck(ctx, row.ID, r.e.cfg.IdlePause); err != nil || !pass {
			return recheckFailed(row.ID, "it was active since, or a failed idle pause backs it off", err)
		}
		held, err := tx.HasLiveHold(ctx, row.ID)
		if err != nil || held {
			return recheckFailed(row.ID, "it is held", err)
		}
	case TriggerHibernateAfter:
		if in.Kind != OpHibernate {
			break
		}
		if pass, err = tx.HibernateAfterRecheck(ctx, row.ID, r.e.cfg.HibernateAfter); err != nil || !pass {
			return recheckFailed(row.ID, "it was resumed since, or a failed hibernation backs it off", err)
		}
	case TriggerDrain, TriggerEnsure:
		// A forward does not keep a sandbox on a draining node: the port
		// door closes it once the sandbox leaves running.
		held, err := tx.HasLiveHold(ctx, row.ID, model.HoldExec)
		if err != nil || held {
			return recheckFailed(row.ID, "an exec holds it", err)
		}
	}
	return nil
}

// movable reports whether a sandbox in state st has a pod that a move
// hibernates (TriggerMove): running (feosd suspends it first), paused,
// stopped or failed.
func movable(st string) bool {
	switch st {
	case model.StateRunning, model.StatePaused, model.StateStopped, model.StateFailed:
		return true
	}
	return false
}

// drainHibernate reports whether a hibernate of a stopped, failed or
// running sandbox is a drain's: by a drain trigger, or by Ensure on a
// draining node.
func (r *runner) drainHibernate(ctx context.Context, tx *store.Store, trigger OpTrigger, target string) (bool, error) {
	switch trigger {
	case TriggerDrain, TriggerDrainForced:
		return true, nil
	case TriggerEnsure:
		n, err := tx.GetNode(ctx, target)
		if errors.Is(err, store.ErrNoRow) {
			return false, nil
		}
		return n.Draining, err
	}
	return false, nil
}

func recheckFailed(id podid.ID, why string, err error) error {
	if err != nil {
		return err
	}
	return errorf(model.ErrPrecondition, "sandbox %s changed: %s", id, why)
}

// startEvent is the activity row of the op's begin.
func (h *OpHandle) startEvent(before store.Sandbox) activity.Event {
	return activity.Event{
		SandboxID: h.id, TenantID: before.Owner, Node: h.target, Kind: activity.KindLifecycleStart,
		SpanID: h.span, ParentSpanID: h.parent, Name: fmt.Sprintf("%s on node %s", h.kind, h.target),
		Detail: map[string]any{"op": string(h.kind), "trigger": string(h.trigger), "node": h.target,
			"from": before.ClientState, "takeover": h.takeover},
	}
}
