// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// afterCommit is what follows a committed end of an op: the tombstones to
// process now (best effort), whether the sandbox is to be synced, and the
// push of its pod's TLS bindings (nil: none).
type afterCommit struct {
	tombstones []store.TombstoneKey
	sync       bool
	tls        *tlsPush
}

// finish is §9 "Finish": one transaction, retried with backoff until it
// commits (or finds the op fenced out) for as long as the instance runs.
func (r *runner) finish(h *OpHandle, res callResult, obs observation) (OpResult, afterCommit) {
	return r.retry(h, func(ctx context.Context) (OpResult, afterCommit, error) {
		return r.finishOnce(ctx, h, res, obs)
	})
}

// rollback is §9 "Rollback": the op could not get a connection to its
// target after its begin, or what comes before its pod call failed
// (cause, nil for the former), so this run sent nothing.
func (r *runner) rollback(h *OpHandle, cause *prepareError) OpResult {
	res, _ := r.retry(h, func(ctx context.Context) (OpResult, afterCommit, error) {
		return r.rollbackOnce(ctx, h, cause)
	})
	return res
}

// retry runs an op's end transaction until it commits or the engine
// stops.
func (r *runner) retry(h *OpHandle, once func(ctx context.Context) (OpResult, afterCommit, error)) (OpResult, afterCommit) {
	e := r.e
	backoff := e.t.finishMin
	for {
		ctx, cancel := context.WithTimeout(e.bg, 30*time.Second)
		res, ac, err := once(ctx)
		cancel()
		if err == nil {
			return res, ac
		}
		if e.bg.Err() != nil {
			return OpResult{Err: fmt.Errorf("%s of sandbox %s not finished: the instance stops: %w", h.kind, h.id, err)}, afterCommit{}
		}
		e.rl.warn("finish", "cannot finish an op; retrying", "sandbox", h.id, "op", h.kind, "err", err)
		if !sleepCtx(e.bg, backoff) {
			return OpResult{Err: fmt.Errorf("%s of sandbox %s not finished: the instance stops: %w", h.kind, h.id, err)}, afterCommit{}
		}
		backoff = min(backoff*2, e.t.finishMax)
	}
}

func (r *runner) finishOnce(ctx context.Context, h *OpHandle, res callResult, obs observation) (OpResult, afterCommit, error) {
	e := r.e
	var result OpResult
	var ac afterCommit
	var events []activity.Event
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		result, ac, events = OpResult{}, afterCommit{}, nil
		row, err := tx.LockSandbox(ctx, h.id, store.ForUpdate)
		gone := errors.Is(err, model.ErrNotFound)
		if err != nil && !gone {
			return err
		}
		if gone || row.Incarnation != h.inc || row.OpID != h.opID {
			result, ac, events, err = r.fencedOut(ctx, tx, h, row, gone, res, obs)
			// The op's span ends without a state of its own; a hibernate
			// whose node abandoned it for a resume says so.
			label := "fenced out"
			if h.kind == OpHibernate && grpcCode(res.err) == codes.Aborted {
				label = "abandoned"
			}
			events = append(events, h.endEvent(store.Sandbox{Owner: h.row.Owner, ClientState: label}, res))
			return err
		}
		c, err := e.newChange(ctx, tx, row)
		if err != nil {
			return err
		}
		applyOutcome(c, h, res, obs)
		if err := c.commit(ctx); err != nil {
			return err
		}
		if h.kind == OpStop && row.Instance {
			// The controller deletes an instance's row once its pod has
			// stopped: it need not wait for its next pass.
			if err := tx.Notify(ctx, db.ChanLeader, store.HintEnvReconcile); err != nil {
				return err
			}
		}
		w := c.written
		result = OpResult{State: w.ClientState, Node: w.Node, Description: w.StatusDescription,
			busy: vmBusy(res.err) && w.ClientState != model.StateUnknown && w.ClientState != opGoal(h.kind)}
		ac.tombstones = append(c.tombstoneKeys(), h.tombstones...)
		ac.tls = c.tls
		events = append(c.events, h.endEvent(w, res))
		return nil
	})
	if err != nil {
		return OpResult{}, afterCommit{}, err
	}
	e.record(events...)
	return result, ac, nil
}

// fencedOut is a finish that finds the op no longer the row's (finish
// step 2): it writes a tombstone for a pod a create, start, resume,
// restore or repair may have made for a row that is gone, of another
// incarnation, or placed elsewhere; or merges a checkpoint newer than the
// row's into a row on the target, in any state. Nothing else: a hibernate
// that a resume preempted leaves the resume's row as it is, unless its
// hibernation reached its head first, whose checkpoint the pod then
// restored from.
func (r *runner) fencedOut(ctx context.Context, tx *store.Store, h *OpHandle, row store.Sandbox, gone bool,
	res callResult, obs observation) (OpResult, afterCommit, []activity.Event, error) {
	result := OpResult{fenced: true}
	if !gone {
		result.State, result.Node, result.Description = row.ClientState, row.Node, row.StatusDescription
	}
	var ac afterCommit
	var events []activity.Event
	makes := h.kind == OpCreate || h.kind == OpStart || h.kind == OpResume || h.kind == OpRestore || h.kind == OpRepair
	if makes && (gone || row.Incarnation != h.inc || (row.Node != h.target && row.OpTarget != h.target)) {
		t := store.Tombstone{PodID: h.id, Node: h.target, Incarnation: h.inc, Reason: fmt.Sprintf("fenced-out %s", h.kind)}
		if _, err := tx.UpsertTombstone(ctx, t); err != nil {
			return OpResult{}, ac, nil, err
		}
		ac.tombstones = []store.TombstoneKey{t.Key()}
		events = append(events, activity.Event{SandboxID: h.id, ProjectID: h.row.Owner, Node: h.target,
			Kind: activity.KindTombstone, Name: fmt.Sprintf("tombstone on node %s: %s", h.target, t.Reason),
			Detail: map[string]any{"node": h.target, "incarnation": h.inc.String(), "reason": t.Reason}})
		return result, ac, events, nil
	}
	if gone || row.Incarnation != h.inc {
		return result, ac, nil, nil
	}
	cps := []*podv1.Checkpoint{res.checkpoint}
	if obs.ours(h.inc) {
		cps = append(cps, obs.info.GetCheckpoint())
	}
	newer := false
	for _, cp := range cps {
		newer = newer || (cp.GetHeadKey() != "" && int64(min(cp.GetEpoch(), 1<<63-1)) > row.CheckpointEpoch)
	}
	if newer && row.Node == h.target {
		c, err := r.e.newChange(ctx, tx, row)
		if err != nil {
			return OpResult{}, ac, nil, err
		}
		for _, cp := range cps {
			c.mergeCheckpoint(cp, h.target)
		}
		written, err := tx.UpdateSandbox(ctx, row, c.after)
		if err != nil {
			return OpResult{}, ac, nil, err
		}
		result.State, result.Node, result.Description = written.ClientState, written.Node, written.StatusDescription
		ac.sync = true
	}
	return result, ac, events, nil
}

// rollbackOnce is one transaction of a rollback. Its error and the row's
// description name why nothing was sent: the node not connected, or the
// cause (a prepare hook's failure, a secret that did not resolve). A
// fresh unpinned create whose prepare hook found the node unfit
// (nodeUnfit) avoids the node from now on, as a create the node refused
// does.
func (r *runner) rollbackOnce(ctx context.Context, h *OpHandle, cause *prepareError) (OpResult, afterCommit, error) {
	e := r.e
	why := fmt.Sprintf("node %s is not connected to instance %s", h.target, e.id)
	failure := errorf(model.ErrNodeUnavailable, "%s", why)
	if cause != nil {
		why, failure = cause.Error(), cause
	}
	result := OpResult{Err: failure}
	var events []activity.Event
	err := e.st.InTx(ctx, func(tx *store.Store) error {
		result, events = OpResult{Err: failure}, nil
		row, err := tx.LockSandbox(ctx, h.id, store.ForUpdate)
		gone := errors.Is(err, model.ErrNotFound)
		if err != nil && !gone {
			return err
		}
		if gone || row.Incarnation != h.inc || row.OpID != h.opID {
			result.fenced = true
			if !gone {
				result.State, result.Node, result.Description = row.ClientState, row.Node, row.StatusDescription
			}
			events = []activity.Event{h.endEvent(store.Sandbox{Owner: h.row.Owner, ClientState: "fenced out"},
				callResult{err: failure})}
			return nil
		}
		c, err := e.newChange(ctx, tx, row)
		if err != nil {
			return err
		}
		a := &c.after
		clearOp(a)
		switch {
		case h.preempted != uuid.Nil:
			// The hibernate it took the place of goes on at the node, its
			// op fenced out: what the pod is decides.
			a.ClientState = model.StateUnknown
			a.StatusDescription = why + "; the hibernate the resume was to abandon goes on"
		case !h.takeover:
			a.ClientState, a.Node, a.LostFrom, a.Materialised = h.read.state, h.read.node, h.read.lostFrom, h.read.materialised
			a.StatusDescription = why + "; nothing was sent"
			if nodeUnfit(cause) && h.kind == OpCreate && a.PinNode == "" {
				a.AvoidNodes = appendUnique(a.AvoidNodes, h.target)
				a.StatusDescription += "; placed elsewhere on the next use"
			}
			for _, k := range h.tombstones {
				if _, err := tx.DeleteTombstone(ctx, k); err != nil {
					return err
				}
			}
		default:
			// The previous owner may have sent the call.
			a.ClientState = model.StateUnknown
			a.StatusDescription = fmt.Sprintf("%s; the outcome of %s is unknown", why, h.kind)
		}
		if err := c.commit(ctx); err != nil {
			return err
		}
		w := c.written
		result.State, result.Node, result.Description = w.ClientState, w.Node, w.StatusDescription
		events = append(c.events, h.endEvent(w, callResult{err: failure}))
		return nil
	})
	if err != nil {
		return OpResult{}, afterCommit{}, err
	}
	e.record(events...)
	return result, afterCommit{}, nil
}

// nodeUnfit reports whether a rollback's cause says that the node itself
// cannot take the pod: a prepare hook's error that wraps ErrNodeUnfit and
// not model.ErrNodeUnavailable. Any other cause may pass, as a create's
// error that is not a placementFailure may.
func nodeUnfit(cause *prepareError) bool {
	return cause != nil && cause.hook && errors.Is(cause, ErrNodeUnfit) && !errors.Is(cause, model.ErrNodeUnavailable)
}

// clearOp clears the op columns of a row.
func clearOp(a *store.Sandbox) {
	a.Op, a.OpID, a.OpOwner, a.OpDeadline, a.OpTarget, a.OpFromState = "", uuid.Nil, "", time.Time{}, "", ""
}

// applyOutcome is the outcome table of §9: what the op's target holds of
// the pod after the op's calls decides the sandbox's state. Call errors
// are never mapped directly; the observation after them decides.
func applyOutcome(c *change, h *OpHandle, res callResult, obs observation) {
	a := &c.after
	clearOp(a)
	target := h.target
	switch {
	case !obs.answered:
		a.ClientState = model.StateUnknown
		a.StatusDescription = fmt.Sprintf("%s on node %s: outcome unknown (%s)", h.kind, target,
			errText(res.err, "the node did not answer"))
	case obs.ours(h.inc):
		applyOurs(c, h, res, obs.info)
	case obs.stale(h.inc):
		inc := podIncarnation(obs.info)
		c.tombstone(target, inc, fmt.Sprintf("stale record of incarnation %s", inc))
		c.notFound(target, fmt.Sprintf("%s on node %s found a stale pod of incarnation %s there; it is deleted first",
			h.kind, target, inc))
	default:
		why := ""
		if res.err != nil {
			why = fmt.Sprintf("%s on node %s failed: %s", h.kind, target, callText(res.err))
			if h.kind == OpCreate && placementFailure(res.err) && !a.HasCheckpoint() && !a.Materialised {
				a.AvoidNodes = appendUnique(a.AvoidNodes, target)
				why += "; placed elsewhere on the next use"
			}
		}
		c.notFound(target, why)
	}
	backOff(c, h)
}

// backOff makes a policy's op that failed push that policy back: an idle
// pause that left the pod running, a hibernate-after that left it
// suspended. Its loop (and its begin's recheck) begins none before
// policy_not_before, policyBackoffMin after the first failure, doubling
// per consecutive one of the same policy up to policyBackoffMax, so that
// a node that cannot snapshot, or an object store that refuses uploads,
// is not asked again every pass. The other policy is not held back: a
// sandbox whose hibernation failed and that was resumed since is paused
// when it idles. A pause that ends paused and a hibernate that ends
// hibernated, of any trigger, clear the backoff.
func backOff(c *change, h *OpHandle) {
	a := &c.after
	var policy string
	switch {
	case h.kind == OpPause && a.ClientState == model.StatePaused,
		h.kind == OpHibernate && a.ClientState == model.StateHibernated:
		a.PolicyFailed, a.PolicyFailures, a.PolicyNotBefore = "", 0, time.Time{}
		return
	case h.trigger == TriggerIdle && h.kind == OpPause && a.ClientState == model.StateRunning:
		policy = store.PolicyIdle
	case h.trigger == TriggerHibernateAfter && h.kind == OpHibernate && a.ClientState == model.StatePaused:
		policy = store.PolicyHibernateAfter
	default:
		return
	}
	if a.PolicyFailed != policy {
		a.PolicyFailed, a.PolicyFailures = policy, 0
	}
	a.PolicyFailures++
	a.PolicyNotBefore = c.now.Add(policyBackoff(a.PolicyFailures, c.e.t.policyBackoffMin, c.e.t.policyBackoffMax))
}

// policyBackoff is the backoff after the nth consecutive failure: least
// doubled n-1 times, at most most.
func policyBackoff(n int, least, most time.Duration) time.Duration {
	d := least
	for i := 1; i < n && d < most; i++ {
		d *= 2
	}
	return min(d, most)
}

// applyOurs applies the state of the op's own pod on its target. The
// pod's checkpoint is merged in every state: a resume that met a
// hibernation past its point of no return restored the pod from the
// hibernation's checkpoint.
func applyOurs(c *change, h *OpHandle, res callResult, info *podv1.PodInfo) {
	a := &c.after
	a.Node, a.Materialised, a.LostFrom = h.target, true, ""
	a.Address = podAddress(info)
	c.mergeCheckpoint(info.GetCheckpoint(), h.target)
	// A repair restarts the pod and ends its processes, unless it made no
	// call (it found the pod healthy, or its prepare hook failed first) or
	// its StopPod met the VM busy and left the pod as it was; a takeover
	// cannot know what the first owner did.
	if h.kind == OpRepair && ((!res.skipped && !res.postponed) || h.takeover) {
		c.endsProcesses = true
	}
	switch info.GetState() {
	case podv1.PodState_POD_STATE_RUNNING:
		a.ClientState = model.StateRunning
		a.AvoidNodes = []string{}
		a.PendingSince, a.UnreachableSince = time.Time{}, time.Time{}
		switch h.kind {
		case OpCreate, OpStart, OpResume, OpRestore, OpRepair:
			a.LastActiveAt = c.now
		}
		a.StatusDescription = ""
		switch h.kind {
		case OpPause:
			a.StatusDescription = "pause failed: " + errText(res.err, "the pod still runs")
		case OpStop:
			a.StatusDescription = "stop failed: " + errText(res.err, "the pod still runs")
		}
		var pe *prepareError
		if h.kind == OpRepair && errors.As(res.err, &pe) {
			// Its prepare hook failed before the repair's first call: the
			// pod runs as it did, not restarted.
			a.StatusDescription = "repair not done: " + pe.Error()
		}
		c.openRun = runOrigin(h.kind, res)
		if h.kind == OpResume && newerCheckpoint(info.GetCheckpoint(), h.row) {
			// The pod's checkpoint moved since the resume began: it was
			// restored from a hibernation the resume waited for.
			c.openRun = "checkpoint"
		}
		if coldBoot(h.kind, res) {
			c.endsProcesses = true
		}
	case podv1.PodState_POD_STATE_PENDING:
		a.ClientState = model.StateCreating
		if a.PendingSince.IsZero() {
			a.PendingSince = c.now
		}
		a.StatusDescription = fmt.Sprintf("still starting on node %s after %s", h.target,
			time.Since(h.started).Round(time.Second))
	case podv1.PodState_POD_STATE_SUSPENDED:
		a.ClientState = model.StatePaused
		a.PendingSince = time.Time{}
		if h.kind == OpPause || a.PausedAt.IsZero() {
			a.PausedAt = c.now
		}
		a.StatusDescription = ""
		if h.kind == OpHibernate {
			a.StatusDescription = "hibernation failed: " + errText(res.err, "the pod stays suspended")
			if grpcCode(res.err) == codes.Aborted {
				// A ResumePod from elsewhere abandoned it, not a preempt
				// of ferund's (that one's finish is fenced out).
				a.StatusDescription = "hibernation abandoned for a resume"
			}
		}
		c.closeRun = "paused"
		if h.trigger == TriggerIdle {
			c.closeRun = "idle"
		}
	case podv1.PodState_POD_STATE_HIBERNATED:
		a.ClientState = model.StateHibernated
		a.PendingSince = time.Time{}
		if info.GetCheckpoint().GetHeadKey() == "" {
			c.mergeCheckpoint(res.checkpoint, h.target)
		}
		a.StatusDescription = ""
		c.closeRun = "hibernated"
	case podv1.PodState_POD_STATE_STOPPED:
		a.ClientState = model.StateStopped
		a.PendingSince = time.Time{}
		a.StatusDescription = info.GetReason()
		c.closeRun = "stopped"
	case podv1.PodState_POD_STATE_FAILED:
		a.ClientState = model.StateFailed
		a.PendingSince = time.Time{}
		a.StatusDescription = info.GetReason()
		c.closeRun = "failed"
	default:
		a.ClientState = model.StateUnknown
		a.StatusDescription = fmt.Sprintf("%s on node %s: the pod is in state %s", h.kind, h.target, info.GetState())
	}
	if vmBusy(res.err) && a.ClientState != model.StateUnknown && (a.ClientState != opGoal(h.kind) || h.kind == OpRepair) {
		// The VM took no change and takes it later: the op is tried
		// again, by its policy after its backoff, by a drain or a repair
		// on its next pass, by a caller after busyRetry.
		a.StatusDescription = fmt.Sprintf("%s postponed: %s", opNoun(h.kind), callText(res.err))
	}
}

// opNoun names an op in a description.
func opNoun(kind OpKind) string {
	if kind == OpHibernate {
		return "hibernation"
	}
	return string(kind)
}

// newerCheckpoint reports whether cp is newer than the checkpoint of row.
func newerCheckpoint(cp *podv1.Checkpoint, row store.Sandbox) bool {
	if cp.GetHeadKey() == "" {
		return false
	}
	epoch := int64(min(cp.GetEpoch(), uint64(1<<63-1)))
	return epoch > row.CheckpointEpoch || (epoch == row.CheckpointEpoch && cp.GetHeadKey() != row.CheckpointHead)
}

// placementFailure reports whether a create's error, with no pod after
// it, is the node's and not the pod's (§9 outcome table): no address
// (RESOURCE_EXHAUSTED), the network (FAILED_PRECONDITION), a TAP prefix
// another pod has (ALREADY_EXISTS). Any other error, INTERNAL among them,
// may pass; avoiding the node for it would leave a single-node system
// unplaceable, since only a run clears avoid_nodes.
func placementFailure(err error) bool {
	switch grpcCode(err) {
	case codes.ResourceExhausted, codes.FailedPrecondition, codes.AlreadyExists:
		return true
	}
	return false
}

// runOrigin is the origin of the run an op opens.
func runOrigin(kind OpKind, res callResult) string {
	switch kind {
	case OpCreate:
		return "image"
	case OpStart, OpRepair:
		return "disk"
	case OpResume:
		return "snapshot"
	case OpRestore:
		if res.restored && !res.memoryRestored && res.reason != "" {
			return "coldboot: " + res.reason
		}
		return "checkpoint"
	}
	return "adopted"
}

// coldBoot reports whether the op's pod came up booted anew, without its
// memory: a start always, a restore that says so.
func coldBoot(kind OpKind, res callResult) bool {
	switch kind {
	case OpStart:
		return true
	case OpRestore:
		return res.restored && !res.memoryRestored && res.reason != ""
	}
	return false
}

func errText(err error, otherwise string) string {
	if err == nil {
		return otherwise
	}
	return callText(err)
}

func appendUnique(ss []string, s string) []string {
	for _, x := range ss {
		if x == s {
			return ss
		}
	}
	return append(append([]string{}, ss...), s)
}

// endEvent is the activity row of the op's end.
func (h *OpHandle) endEvent(w store.Sandbox, res callResult) activity.Event {
	d := time.Since(h.started).Nanoseconds()
	parent := w.RunID
	if parent == "" {
		parent = h.parent
	}
	detail := map[string]any{"op": string(h.kind), "state": w.ClientState, "node": h.target}
	if w.StatusDescription != "" {
		detail["description"] = w.StatusDescription
	}
	if res.err != nil {
		detail["error"] = callText(res.err)
	}
	return activity.Event{SandboxID: h.id, ProjectID: w.Owner, Node: h.target, Kind: activity.KindLifecycleEnd,
		SpanID: h.span, ParentSpanID: parent, DurationNs: &d,
		Name: fmt.Sprintf("%s on node %s: %s", h.kind, h.target, w.ClientState), Detail: detail}
}
