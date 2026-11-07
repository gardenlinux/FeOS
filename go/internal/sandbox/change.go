// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// change is one transaction's lifecycle write of a sandbox row: the row as
// the transaction locked it, the row to write, and what goes with the
// write (tombstones, the booking, the end of processes, run spans).
type change struct {
	e      *Engine
	ctx    context.Context
	tx     *store.Store
	now    time.Time // the transaction's clock
	before store.Sandbox
	after  store.Sandbox

	// endsProcesses: the change ends the sandbox's processes besides a
	// state that does (a repair, a cold boot).
	endsProcesses bool
	// openRun opens a run with this origin when the sandbox runs and none
	// is open; closeRun closes the open run with this reason.
	openRun, closeRun string
	// tombstones are written with the change.
	tombstones []store.Tombstone

	written store.Sandbox
	events  []activity.Event
}

func (e *Engine) newChange(ctx context.Context, tx *store.Store, row store.Sandbox) (*change, error) {
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	return &change{e: e, ctx: ctx, tx: tx, now: now, before: row, after: row}, nil
}

// tombstone adds a tombstone for the sandbox's pod of incarnation inc on
// node.
func (c *change) tombstone(node string, inc uuid.UUID, reason string) {
	c.tombstones = append(c.tombstones, store.Tombstone{PodID: c.before.ID, Node: node, Incarnation: inc, Reason: reason})
}

// commit writes the change in its transaction: the tombstones, the row
// (gen + 1 and NOTIFY on a lifecycle write), the booking that follows the
// state, the end of processes, and the run spans; the activity rows are
// kept for after the commit. A new address of a security group's member
// (an instance, a row whose pod has groups) wakes the environments' group
// loop (ferun_leader store.HintEnvGroups).
func (c *change) commit(ctx context.Context) error {
	a := &c.after
	if a.ClientState == model.StateRunning && a.RunID == "" && c.openRun == "" {
		c.openRun = "adopted"
	}
	if c.closeRun != "" && a.RunID != "" {
		c.events = append(c.events, c.runEnd(a.RunID, c.closeRun))
		a.RunID = ""
	}
	if a.ClientState == model.StateRunning && a.RunID == "" && c.openRun != "" {
		a.RunID = newRunID()
		c.events = append(c.events, c.runStart(a.RunID, c.openRun))
	}
	for _, t := range c.tombstones {
		if _, err := c.tx.UpsertTombstone(ctx, t); err != nil {
			return err
		}
		c.events = append(c.events, activity.Event{SandboxID: t.PodID, TenantID: c.before.Owner, Node: t.Node,
			Kind: activity.KindTombstone, Name: fmt.Sprintf("tombstone on node %s: %s", t.Node, t.Reason),
			Detail: map[string]any{"node": t.Node, "incarnation": t.Incarnation.String(), "reason": t.Reason}})
	}
	written, err := c.tx.UpdateSandbox(ctx, c.before, c.after)
	if err != nil {
		return err
	}
	c.written = written
	if written.Address != c.before.Address {
		if err := c.tx.Notify(ctx, db.ChanLeader, store.HintEnvGroups); err != nil {
			return err
		}
	}
	evs, err := c.e.syncBooking(ctx, c.tx, c.before, written, c.endsProcesses)
	if err != nil {
		return err
	}
	c.events = append(c.events, evs...)
	if c.before.ClientState != written.ClientState {
		parent := written.RunID
		if parent == "" {
			parent = c.before.RunID
		}
		detail := map[string]any{"from": c.before.ClientState, "to": written.ClientState}
		if written.StatusDescription != "" {
			detail["description"] = written.StatusDescription
		}
		c.events = append(c.events, activity.Event{SandboxID: written.ID, TenantID: written.Owner, Node: written.Node,
			Kind: activity.KindSandboxState, ParentSpanID: parent,
			Name: fmt.Sprintf("%s → %s", c.before.ClientState, written.ClientState), Detail: detail})
	}
	return nil
}

// tombstoneKeys are the keys of the change's tombstones.
func (c *change) tombstoneKeys() []store.TombstoneKey {
	ks := make([]store.TombstoneKey, len(c.tombstones))
	for i, t := range c.tombstones {
		ks[i] = t.Key()
	}
	return ks
}

// newRunID mints a run's span id: "run-<uuid>", a UUIDv7, whose time is
// the run's start, so that its end knows its duration without a column.
func newRunID() string {
	u, err := uuid.NewV7()
	if err != nil {
		u = uuid.New()
	}
	return "run-" + u.String()
}

// runStarted is the start of a run its id names, or the zero time.
func runStarted(runID string) time.Time {
	u, err := uuid.Parse(runID[min(len(runID), len("run-")):])
	if err != nil || u.Version() != 7 {
		return time.Time{}
	}
	var b [8]byte
	copy(b[2:], u[:6])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(b[:])))
}

func (c *change) runStart(runID, origin string) activity.Event {
	a := &c.after
	name := fmt.Sprintf("run on %s", a.Node)
	switch origin {
	case "image":
		name += " from image " + a.ImageRef
	case "checkpoint":
		name += " restored from checkpoint"
	case "snapshot":
		name += " resumed from its snapshot"
	case "disk":
		name += " booted on its disk"
	case "adopted":
		name += " (adopted)"
	default:
		name += " (" + origin + ")"
	}
	return activity.Event{SandboxID: a.ID, TenantID: a.Owner, Node: a.Node, Kind: activity.KindRunStart,
		SpanID: runID, Name: name, Detail: map[string]any{"node": a.Node, "origin": origin, "image": a.ImageRef,
			"vcpus": a.VCPUs, "memory_mib": a.MemoryMiB}}
}

func (c *change) runEnd(runID, reason string) activity.Event {
	ev := activity.Event{SandboxID: c.before.ID, TenantID: c.before.Owner, Node: c.before.Node, Kind: activity.KindRunEnd,
		SpanID: runID, Name: "ended: " + reason, Detail: map[string]any{"reason": reason}}
	if start := runStarted(runID); !start.IsZero() {
		d := max(time.Since(start), 0).Nanoseconds()
		ev.DurationNs = &d
	} else {
		var zero int64
		ev.DurationNs = &zero
	}
	return ev
}

// observe applies an observation of the pod on node to the change, as
// Sync and Reconcile do (§9 Sync): the node holds the pod of the row's
// incarnation in a state, holds another incarnation's (a stale record,
// tombstoned), or does not hold it.
func (c *change) observe(node string, obs observation) {
	switch {
	case obs.ours(c.before.Incarnation):
		c.observeOurs(node, obs.info)
	case obs.stale(c.before.Incarnation):
		c.tombstone(node, podIncarnation(obs.info), fmt.Sprintf("stale record of incarnation %s", podIncarnation(obs.info)))
		c.notFound(node, "")
	default:
		c.notFound(node, "")
	}
}

// observeOurs applies the state of the row's own pod on node. A newer
// checkpoint the pod reports is merged in any state: a pod restored from a
// hibernation that a resume waited for runs on it.
func (c *change) observeOurs(node string, info *podv1.PodInfo) {
	a := &c.after
	a.Node, a.Materialised, a.LostFrom = node, true, ""
	a.Address = podAddress(info)
	c.mergeCheckpoint(info.GetCheckpoint(), node)
	switch info.GetState() {
	case podv1.PodState_POD_STATE_PENDING:
		a.ClientState = model.StateCreating
		if a.PendingSince.IsZero() {
			a.PendingSince = c.now
		}
		return
	case podv1.PodState_POD_STATE_RUNNING:
		a.ClientState = model.StateRunning
		if c.before.ClientState != model.StateRunning {
			a.StatusDescription = ""
		}
	case podv1.PodState_POD_STATE_SUSPENDED:
		a.ClientState = model.StatePaused
		// Paused now, unless the row was paused already: a pod that
		// feosd suspended after a pause gave up on it, or a node that
		// came back, does not keep the paused_at of an earlier pause,
		// which would hibernate it at once.
		if a.PausedAt.IsZero() || c.before.ClientState != model.StatePaused {
			a.PausedAt = c.now
		}
		if c.before.ClientState != model.StatePaused {
			a.StatusDescription = ""
		}
		c.closeRun = "paused"
	case podv1.PodState_POD_STATE_HIBERNATED:
		a.ClientState = model.StateHibernated
		if c.before.ClientState != model.StateHibernated {
			a.StatusDescription = ""
		}
		c.closeRun = "hibernated"
	case podv1.PodState_POD_STATE_STOPPED:
		a.ClientState = model.StateStopped
		a.StatusDescription = info.GetReason()
		c.closeRun = "stopped"
	case podv1.PodState_POD_STATE_FAILED:
		a.ClientState = model.StateFailed
		a.StatusDescription = info.GetReason()
		c.closeRun = "failed"
	default:
		return
	}
	a.PendingSince = time.Time{}
}

// notFound applies a node's NOT_FOUND for the row's pod on node: with a
// checkpoint the sandbox is hibernated without a node, a pod that was seen
// is lost (its node kept), and one never seen is pending again. why, if
// set, describes a pending sandbox.
func (c *change) notFound(node, why string) {
	a := &c.after
	a.Address = ""
	switch {
	case a.HasCheckpoint():
		a.ClientState, a.Node, a.Materialised = model.StateHibernated, "", false
		if c.before.ClientState != model.StateHibernated {
			a.StatusDescription = fmt.Sprintf("not on node %s; restored from the checkpoint of %s on the next use",
				node, rfc3339(a.CheckpointAt))
		}
		c.closeRun = "hibernated"
	case a.Materialised:
		a.ClientState = model.StateLost
		a.StatusDescription = "not on node " + node
		c.closeRun = "lost"
	default:
		a.ClientState, a.Node, a.Materialised = model.StatePending, "", false
		if why == "" {
			why = "not on node " + node + "; created on the next use"
		}
		a.StatusDescription = why
		c.closeRun = "lost"
	}
	a.PendingSince = time.Time{}
}

// podAddress is the address of a pod as its record shows it: none while
// it is hibernated (it holds none on the node then).
func podAddress(info *podv1.PodInfo) string {
	if info.GetState() == podv1.PodState_POD_STATE_HIBERNATED {
		return ""
	}
	return info.GetIpv6Address()
}

// mergeCheckpoint writes the checkpoint a pod on node reports when it is
// not older than the row's (never back to an older epoch), and reports
// whether it did.
func (c *change) mergeCheckpoint(cp *podv1.Checkpoint, node string) bool {
	if cp.GetHeadKey() == "" {
		return false
	}
	a := &c.after
	epoch := int64(min(cp.GetEpoch(), uint64(1<<63-1)))
	if epoch < a.CheckpointEpoch {
		return false
	}
	if epoch == a.CheckpointEpoch && cp.GetHeadKey() == a.CheckpointHead {
		return false
	}
	a.CheckpointHead, a.CheckpointEpoch, a.CheckpointMemory, a.CheckpointAt = cp.GetHeadKey(), epoch, cp.GetMemory(), c.now
	a.CheckpointStore = c.objectStoreOf(node)
	return true
}

// objectStoreOf is the object store a node reports; "" if unknown.
func (c *change) objectStoreOf(node string) string {
	n, err := c.tx.GetNode(c.ctx, node)
	if err != nil {
		return ""
	}
	return n.ObjectStoreID
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	return t.UTC().Format(time.RFC3339)
}

// bookingOf is what syncBooking does for a state (§9 states table).
type bookingRule int

const (
	bookNone      bookingRule = iota // no booking
	bookMust                         // a booking on node must exist
	bookUnchanged                    // as it is
)

func bookingOf(state string) bookingRule {
	switch state {
	case model.StateCreating, model.StateRunning, model.StatePausing, model.StateResuming:
		return bookMust
	case model.StateHibernating, model.StateUnknown:
		return bookUnchanged
	}
	return bookNone
}

// endsProcesses reports whether a write of state ends the sandbox's
// processes: their microVM is gone.
func endsProcesses(state string) bool {
	switch state {
	case model.StateStopped, model.StateFailed, model.StateHibernated, model.StateLost, model.StatePending:
		return true
	}
	return false
}

// syncBooking makes the booking follow the state after a write of
// client_state or node (§9 "Bookings follow resource use"), with the
// sandbox's row locked: a state that uses no node has no booking; one that
// uses its node has a booking there, inserted with the capacity gates
// evaluated under the node's row lock and kept even when they fail
// (reality wins over the books; an overbooked row is logged). It also
// ends the process holds of a transition that ends the processes. It
// returns the activity rows to record.
func (e *Engine) syncBooking(ctx context.Context, tx *store.Store, before, after store.Sandbox, ends bool) ([]activity.Event, error) {
	var events []activity.Event
	switch bookingOf(after.ClientState) {
	case bookNone:
		if _, err := tx.DeleteBooking(ctx, after.ID); err != nil {
			return nil, err
		}
	case bookMust:
		if after.Node == "" {
			break
		}
		b, err := tx.GetBooking(ctx, after.ID)
		if err == nil && b.Node == after.Node {
			break
		}
		if err != nil && err != store.ErrNoRow {
			return nil, err
		}
		n, err := tx.LockNode(ctx, after.Node, store.ForUpdate)
		if err != nil {
			return nil, err
		}
		sums, err := tx.SumBookings(ctx, after.Node, after.ID)
		if err != nil {
			return nil, err
		}
		gate := capacityRefusal(n, after, sums)
		if err := tx.UpsertBooking(ctx, store.Booking{PodID: after.ID, Node: after.Node, VCPUs: after.VCPUs,
			MemoryMiB: after.MemoryMiB, Gated: gate == ""}); err != nil {
			return nil, err
		}
		if gate != "" {
			msg := fmt.Sprintf("node %s overbooked by sandbox %s: %s", after.Node, after.ID, gate)
			e.log.Warn(msg)
			events = append(events, activity.Event{SandboxID: after.ID, TenantID: after.Owner, Node: after.Node,
				Kind: activity.KindOverbooked, Name: msg, Detail: map[string]any{"node": after.Node, "gate": gate}})
		}
	}
	if ends || (endsProcesses(after.ClientState) && before.ClientState != after.ClientState) {
		if _, err := tx.DeleteProcessHolds(ctx, after.ID); err != nil {
			return nil, err
		}
	}
	return events, nil
}
