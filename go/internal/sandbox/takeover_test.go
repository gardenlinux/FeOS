// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// hibernatedOn makes a sandbox that is hibernated with its record on node.
func (h *harness) hibernatedOn(node string) store.Sandbox {
	h.t.Helper()
	sb := h.running(512)
	if sb.Node != node {
		h.t.Fatalf("placed on %s, not %s", sb.Node, node)
	}
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: node})
	h.startOp(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: node})
	return h.waitState(sb.ID, model.StateHibernated, 10*time.Second)
}

func TestRollbackFreshBegin(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	h.addNode("n2")
	sb := h.hibernatedOn("n1")
	h.settle(sb.ID)
	// Lost grace left it with lost_from; n1 drains, so the restore goes
	// to n2, which this instance turns out not to reach.
	h.exec("UPDATE sandboxes SET lost_from = node WHERE id = $1", sb.ID)
	h.exec("UPDATE nodes SET draining = true WHERE name = 'n1'")
	before := h.row(sb.ID)
	var during store.Sandbox
	h.failNextOp(func() { during = h.row(sb.ID) })
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, model.ErrNodeUnavailable, "node n2 is not connected to instance i1")
	// The begin had moved the row to n2 and tombstoned the record on n1.
	if during.Node != "n2" || during.LostFrom != "" || during.Materialised || during.Op != string(OpRestore) {
		t.Fatalf("row during the op %+v", during)
	}
	after := h.row(sb.ID)
	wantState(t, after, model.StateHibernated)
	if after.Node != "n1" || after.LostFrom != "n1" || after.Materialised != before.Materialised || after.Op != "" {
		t.Fatalf("row after the rollback %+v (before %+v)", after, before)
	}
	if after.StatusDescription != "node n2 is not connected to instance i1; nothing was sent" {
		t.Fatalf("description %q", after.StatusDescription)
	}
	if ts := h.tombstones(sb.ID); len(ts) != 0 {
		t.Fatalf("tombstones after the rollback %+v", ts)
	}
	if _, ok := h.booking(sb.ID); ok {
		t.Fatal("the admission's booking stayed")
	}
	// The record on n1 stayed, and the sandbox comes back once n1 takes
	// work again.
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_HIBERNATED)
	h.exec("UPDATE nodes SET draining = false WHERE name = 'n1'")
	if lease := h.ensure(sb.ID); lease.Node != "n1" {
		t.Fatalf("restored on %s", lease.Node)
	}
}

// opOfDeadOwner writes an op of instance owner into the row, as its begin
// would have.
func (h *harness) opOfDeadOwner(sb store.Sandbox, kind OpKind, owner string) store.Sandbox {
	h.t.Helper()
	h.exec(`UPDATE sandboxes SET op = $2, op_id = gen_random_uuid(), op_owner = $3, op_deadline = now() + interval '1 minute',
		op_target = node, op_from_state = client_state, client_state = $4, gen = gen + 1 WHERE id = $1`,
		sb.ID, string(kind), owner, transitional(kind))
	return h.row(sb.ID)
}

func TestRollbackTakeover(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.running(512)
	h.settle(sb.ID)
	h.deadInstance("gone")
	h.opOfDeadOwner(sb, OpPause, "gone")
	h.failNextOp(nil)
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, model.ErrNodeUnavailable, "node n1 is not connected to instance i1")
	row := h.row(sb.ID)
	wantState(t, row, model.StateUnknown)
	if row.Node != "n1" || row.StatusDescription != "node n1 is not connected to instance i1; the outcome of pause is unknown" {
		t.Fatalf("row after the takeover's rollback %+v", row)
	}
	if b, ok := h.booking(sb.ID); !ok || b.Node != "n1" {
		t.Fatal("the booking went")
	}
	// Sync resolves it: the pod still runs.
	h.ensure(sb.ID)
	wantState(t, h.row(sb.ID), model.StateRunning)
}

func TestRollbackRowChangedMeanwhile(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.running(512)
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.settle(sb.ID)
	// Between the begin and the call, the op is released (as by a stopping
	// instance's step 4): the rollback finds the op not its own.
	h.failNextOp(func() {
		if err := h.st.InTx(bg, func(tx *store.Store) error {
			_, err := tx.ReleaseOpsOf(bg, "i1")
			return err
		}); err != nil {
			t.Error(err)
		}
	})
	res := h.startOp(sb.ID, OpIntent{Kind: OpResume, Trigger: TriggerEnsure, Target: "n1"})
	if !res.fenced {
		t.Fatalf("rollback result %+v", res)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateUnknown)
	if row.StatusDescription != "op resume of stopping instance i1 was released" {
		t.Fatalf("the rollback wrote over a changed row: %+v", row)
	}
}

func TestFencedOutCreateTombstone(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	arrived, release := h.gate("n1", podv1.PodService_CreatePod_FullMethodName)
	defer release()
	sb := h.sandbox(512)
	done := make(chan error, 1)
	go func() {
		_, err := h.eng().Ensure(bg, sb.ID)
		done <- err
	}()
	waitClosed(t, arrived, "the create's call")
	row, tombstoned, err := h.eng().Delete(bg, testProject, sb.ID)
	if err != nil || !tombstoned || row.ID != sb.ID {
		t.Fatalf("delete: %v %v", tombstoned, err)
	}
	// A live op may still make the pod: not before its deadline.
	ts := h.tombstones(sb.ID)
	if len(ts) != 1 || ts[0].Node != "n1" || ts[0].Incarnation != sb.Incarnation {
		t.Fatalf("tombstones %+v", ts)
	}
	if ts[0].NotBefore.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("tombstone due at %v, before the op's deadline", ts[0].NotBefore)
	}
	release()
	wantErr(t, <-done, model.ErrNotFound, "")
	// The finish was fenced out (the row is gone): its tombstone is due
	// now and processed, the pod deleted.
	h.noPod("n1", sb.ID)
	eventually(t, 10*time.Second, "the tombstone processed", func() bool { return len(h.tombstones(sb.ID)) == 0 })
}

func TestFencedOutCheckpointMerge(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.running(512)
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	arrived, release := h.gate("n1", podv1.PodService_HibernatePod_FullMethodName)
	defer release()
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	hd, err := h.eng().runner.Start(ctx, sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1",
		Incarnation: sb.Incarnation})
	if err != nil {
		t.Fatal(err)
	}
	// Another (live) instance took the op over meanwhile.
	waitClosed(t, arrived, "the hibernation's call")
	h.liveInstance("other")
	h.exec("UPDATE sandboxes SET op_id = gen_random_uuid(), op_owner = 'other', gen = gen + 1 WHERE id = $1", sb.ID)
	release()
	res, err := hd.Wait(ctx)
	if err != nil || !res.fenced {
		t.Fatalf("fenced finish %+v %v", res, err)
	}
	row := h.row(sb.ID)
	if row.Op != string(OpHibernate) || row.OpOwner != "other" || row.ClientState != model.StateHibernating {
		t.Fatalf("the fenced finish wrote over the op: %+v", row)
	}
	if row.CheckpointHead == "" || row.CheckpointEpoch == 0 || row.CheckpointStore != h.objs.ID() {
		t.Fatalf("checkpoint not merged: %+v", row)
	}
	if ts := h.tombstones(sb.ID); len(ts) != 0 {
		t.Fatalf("a hibernate wrote tombstones %+v", ts)
	}

	// A hibernate that a resume preempted after its hibernation had reached
	// its head: the row is the resume's, running on the node, and the
	// checkpoint the pod was restored from is merged into it. On another
	// node it is not.
	h.exec("INSERT INTO nodes (name, status) VALUES ('far', 'ready')")
	for _, c := range []struct {
		node   string
		merged bool
	}{{"n1", true}, {"far", false}} {
		sb := h.running(512)
		h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
		arrived, release := h.gate("n1", podv1.PodService_HibernatePod_FullMethodName)
		hd, err := h.begin(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
		if err != nil {
			t.Fatal(err)
		}
		waitClosed(t, arrived, "the hibernation's call")
		h.exec(`UPDATE sandboxes SET op = '', op_id = NULL, op_owner = '', op_deadline = NULL, op_target = '',
			op_from_state = '', client_state = 'running', node = $2, gen = gen + 1 WHERE id = $1`, sb.ID, c.node)
		release()
		if res, err := hd.Wait(ctx); err != nil || !res.fenced {
			t.Fatalf("fenced finish on %s %+v %v", c.node, res, err)
		}
		row := h.row(sb.ID)
		if merged := row.CheckpointHead != "" && row.CheckpointEpoch > 0 && row.CheckpointStore == h.objs.ID(); merged != c.merged {
			t.Fatalf("row on %s after the fenced finish (merged %v): %+v", c.node, c.merged, row)
		}
	}
}

// TestEnsurePreemptsADeadOwnersHibernate: a dead owner's hibernate that a
// resume abandons now is preempted rather than taken over: no HibernatePod
// is sent again, the dead owner's call (still at the node) is answered
// ABORTED, and the sandbox runs from its snapshot.
func TestEnsurePreemptsADeadOwnersHibernate(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.pausedLong("n1")
	h.settle(sb.ID)
	h.deadInstance("gone")
	row := h.opOfDeadOwner(sb, OpHibernate, "gone")
	upload := h.watchCalls("n1", testnode.HibernateUpload, true)
	pc := h.pods("n1")
	call := make(chan error, 1)
	go func() {
		_, err := pc.HibernatePod(bg, &podv1.HibernatePodRequest{PodId: sb.ID.String(), RequestId: row.OpID.String()})
		call <- err
	}()
	waitClosed(t, upload.arrived, "the dead owner's upload")
	calls := h.watchCalls("n1", podv1.PodService_HibernatePod_FullMethodName, false)
	if lease := h.ensure(sb.ID); lease.Node != "n1" {
		t.Fatalf("lease %+v", lease)
	}
	if calls.count() != 0 {
		t.Fatalf("%d HibernatePods sent again", calls.count())
	}
	select {
	case err := <-call:
		if status.Code(err) != codes.Aborted || status.Convert(err).Message() != abandoned(sb.ID) {
			t.Fatalf("the dead owner's call: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the dead owner's call was not answered")
	}
	h.activityNamed(sb.ID, "lifecycle_start", "resume on node n1 (abandons the hibernate of instance gone)")
	after := h.row(sb.ID)
	wantState(t, after, model.StateRunning)
	if after.Op != "" || after.CheckpointHead != "" {
		t.Fatalf("row %+v", after)
	}
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
}

// TestPreemptRollback: a preempting resume that gets no connection to its
// node after its begin sent nothing, and the hibernate it took the place
// of goes on at the node: the row is unknown, on its node, booked; a Sync
// resolves it (paused while the pod hibernates), and the hibernation's end
// reaches the row.
func TestPreemptRollback(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb, hd, upload := h.hibernatingAtUpload("n1")
	// The rollback's row is read before a sync resolves it (a sync of the
	// pause's pod event may come at any time). The pod reads are held
	// from the resume's look for its connection on, after Ensure's
	// preempt asked the node.
	var releaseReads func()
	h.failNextOp(func() { releaseReads = h.holdPodReads("n1") })
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, model.ErrNodeUnavailable, "node n1 is not connected to instance i1")
	row := h.row(sb.ID)
	wantState(t, row, model.StateUnknown)
	if row.Node != "n1" ||
		row.StatusDescription != "node n1 is not connected to instance i1; the hibernate the resume was to abandon goes on" {
		t.Fatalf("row after the preempt's rollback %+v", row)
	}
	if b, ok := h.booking(sb.ID); !ok || b.Node != "n1" {
		t.Fatal("the booking went")
	}
	releaseReads()
	h.wantHibernation("n1", sb.ID, podv1.HibernationPhase_HIBERNATION_PHASE_UPLOADING_DISK, true, hd.opID.String())
	if err := h.eng().Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(sb.ID), model.StatePaused)
	upload.release()
	if res, err := hd.Wait(ctx); err != nil || !res.fenced {
		t.Fatalf("the hibernate's end %+v %v", res, err)
	}
	if after := h.waitState(sb.ID, model.StateHibernated, 15*time.Second); after.CheckpointHead == "" {
		t.Fatalf("hibernated without a checkpoint %+v", after)
	}
}

func TestTakeoverByLiveness(t *testing.T) {
	for _, owner := range []string{"dead", "stopping"} {
		t.Run(owner, func(t *testing.T) {
			h := newHarness(t)
			h.addNode("n1")
			sb := h.running(512)
			h.settle(sb.ID)
			if owner == "dead" {
				h.deadInstance(owner)
			} else {
				h.liveInstance(owner)
				h.exec("UPDATE instances SET stopping = now() WHERE id = $1", owner)
			}
			before := h.opOfDeadOwner(sb, OpPause, owner)
			arrived, release := h.gate("n1", podv1.PodService_SuspendPod_FullMethodName)
			defer release()
			done := make(chan error, 1)
			go func() {
				_, err := h.eng().Ensure(bg, sb.ID)
				done <- err
			}()
			waitClosed(t, arrived, "the taken-over pause's call")
			during := h.row(sb.ID)
			if during.OpOwner != "i1" {
				t.Fatalf("not taken over: %+v", during)
			}
			// Only the op's ownership was written.
			if during.Op != before.Op || during.OpTarget != before.OpTarget || during.OpFromState != before.OpFromState ||
				during.ClientState != before.ClientState || during.Node != before.Node || during.OpID == before.OpID ||
				during.Gen != before.Gen+1 || during.ImageRef != before.ImageRef || during.Materialised != before.Materialised {
				t.Fatalf("takeover wrote more than the op's ownership:\n%+v\n%+v", before, during)
			}
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			// The pause it took over ran; Ensure then resumed the sandbox.
			wantState(t, h.row(sb.ID), model.StateRunning)
		})
	}
}

func TestNoTakeoverOfLiveOwner(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.running(512)
	h.liveInstance("other")
	row := h.opOfDeadOwner(sb, OpPause, "other")
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	_, err := h.eng().runner.Start(ctx, sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerEnsure, Target: "n1",
		Incarnation: sb.Incarnation})
	wantErr(t, err, model.ErrBusy, "")
	// Ensure waits for the owner; when the op ends, it goes on.
	done := make(chan error, 1)
	go func() {
		_, err := h.eng().Ensure(bg, sb.ID)
		done <- err
	}()
	never(t, 500*time.Millisecond, "Ensure took over a live owner's op", func() bool { return h.row(sb.ID).OpOwner != "other" })
	h.exec(`UPDATE sandboxes SET op = '', op_id = NULL, op_owner = '', op_deadline = NULL, op_target = '',
		op_from_state = '', client_state = 'running', gen = gen + 1 WHERE id = $1 AND op_id = $2`, sb.ID, row.OpID)
	h.exec("SELECT pg_notify('ferun_sandbox', $1)", sb.ID.String())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStaleOpLoop(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	h.deadInstance("dead")
	h.liveInstance("other")

	// A dead owner's op towards a node the leader reaches: taken over.
	a := h.running(512)
	h.opOfDeadOwner(a, OpPause, "dead")

	// A dead owner's op towards a node the leader does not reach:
	// released.
	b := h.running(512)
	h.exec("INSERT INTO nodes (name, status) VALUES ('far', 'ready')")
	h.exec("UPDATE sandboxes SET node = 'far' WHERE id = $1", b.ID)
	h.exec("UPDATE bookings SET node = 'far' WHERE pod_id = $1", b.ID)
	h.opOfDeadOwner(h.row(b.ID), OpPause, "dead")

	// A live owner's op 5 minutes past its deadline: released too.
	c := h.running(512)
	h.opOfDeadOwner(c, OpPause, "other")
	h.exec("UPDATE sandboxes SET op_deadline = now() - interval '6 minutes' WHERE id = $1", c.ID)

	// A live owner's op past its deadline by less: left alone.
	d := h.running(512)
	h.opOfDeadOwner(d, OpPause, "other")
	h.exec("UPDATE sandboxes SET op_deadline = now() - interval '1 minute' WHERE id = $1", d.ID)

	if err := e.staleOps(bg); err != nil {
		t.Fatal(err)
	}
	h.waitState(a.ID, model.StatePaused, 10*time.Second)
	rb := h.row(b.ID)
	wantState(t, rb, model.StateUnknown)
	if rb.StatusDescription != "op pause of instance dead was abandoned" || rb.Node != "far" {
		t.Fatalf("released row %+v", rb)
	}
	if bk, ok := h.booking(b.ID); !ok || bk.Node != "far" {
		t.Fatal("a released op's booking went")
	}
	// Released (a Sync may have resolved it since: its pod runs).
	if rc := h.row(c.ID); rc.Op != "" {
		t.Fatalf("overdue op kept %+v", rc)
	}
	eventually(t, 10*time.Second, "the release recorded", func() bool {
		var n int
		h.scalar(&n, `SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'sandbox_state'
			AND detail->>'description' = 'op pause of instance other was abandoned'`, c.ID)
		return n == 1
	})
	if rd := h.row(d.ID); rd.Op != string(OpPause) || rd.OpOwner != "other" {
		t.Fatalf("a live owner's op was touched: %+v", rd)
	}
}

func TestBeforeBeginWaitsForRollout(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	sb := h.running(512)
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})

	// Without a rollout deadline, a resume towards a node that is gone
	// fails after the node wait, with nothing begun.
	n.Crash()
	eventually(t, 10*time.Second, "n1 disconnected", func() bool { return !h.ins[0].srv.Connected("n1") })
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	_, err := h.eng().Ensure(ctx, sb.ID)
	cancel()
	wantErr(t, err, model.ErrNodeUnavailable, "node n1 is not connected to instance i1")
	if row := h.row(sb.ID); row.ClientState != model.StatePaused || row.Op != "" {
		t.Fatalf("row after a refused begin %+v", row)
	}

	// The node announced a restart: the wait goes up to the rollout
	// deadline, and the resume runs once the node is back.
	h.exec("UPDATE nodes SET stopping_until = now() + interval '30 seconds' WHERE name = 'n1'")
	h.ins[0].srv.SetStoppingUntil("n1", time.Now().Add(30*time.Second))
	done := make(chan error, 1)
	go func() {
		_, err := h.eng().Ensure(bg, sb.ID)
		done <- err
	}()
	time.Sleep(1500 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Ensure returned while the node was away: %v", err)
	default:
	}
	n.Restart(false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)
}

func TestTakeoverCreateKeepsImageAndSpec(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.sandbox(512)
	// A create of a dead instance: begun (node, image_ref, booking), its
	// pod made on the node with the same spec.
	h.deadInstance("dead")
	h.exec(`UPDATE sandboxes SET image_ref = $2, node = 'n1', op = 'create', op_id = gen_random_uuid(), op_owner = 'dead',
		op_deadline = now() + interval '1 minute', op_target = 'n1', op_from_state = 'pending', client_state = 'creating',
		gen = gen + 1 WHERE id = $1`, sb.ID, testImage)
	h.exec("INSERT INTO bookings (pod_id, node, vcpus, memory_mib, gated) VALUES ($1, 'n1', 1, 512, true)", sb.ID)
	row := h.row(sb.ID)
	if _, err := h.pods("n1").CreatePod(bg, &podv1.CreatePodRequest{PodId: sb.ID.String(), Spec: podSpec(row)}); err != nil {
		t.Fatal(err)
	}
	lease := h.ensure(sb.ID)
	if lease.Node != "n1" {
		t.Fatalf("lease %+v", lease)
	}
	row = h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	h.activityNamed(sb.ID, activity.KindLifecycleStart, "create on node n1 (taken over from instance dead)")
	var placements int
	h.scalar(&placements, "SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'placement'", sb.ID)
	if placements != 0 {
		t.Fatalf("a takeover wrote %d placement rows", placements)
	}
	// The pod is the one the dead instance made (the same spec: the
	// create was idempotent).
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	if podIncarnation(info) != sb.Incarnation || info.GetSpec().GetContainers()[0].GetImageRef() != testImage {
		t.Fatalf("pod %+v", info.GetSpec())
	}
}

func TestTakeoverIntentNeverBeginsAnOp(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.running(512)
	// The op a caller meant to take over ended before the begin: nothing
	// is begun in its place.
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	_, err := h.eng().runner.start(ctx, sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerRepair, Target: "n1",
		Incarnation: sb.Incarnation, takeover: sb.Incarnation}, false)
	wantErr(t, err, model.ErrPrecondition, "the op to take over ended")
	if row := h.row(sb.ID); row.Op != "" || row.ClientState != model.StateRunning {
		t.Fatalf("row %+v", row)
	}
	// Another op is in flight than the one meant: not taken over either.
	h.deadInstance("dead")
	h.opOfDeadOwner(sb, OpPause, "dead")
	_, err = h.eng().runner.start(ctx, sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerRepair, Target: "n1",
		Incarnation: sb.Incarnation, takeover: sb.Incarnation}, false)
	wantErr(t, err, model.ErrPrecondition, "the op to take over ended")
	if row := h.row(sb.ID); row.OpOwner != "dead" {
		t.Fatalf("taken over: %+v", row)
	}
}

// A begin's transaction is not its caller's: a caller whose context ends
// while the begin's transaction runs (a client that hangs up, a leader
// term that ends) neither cuts the transaction, which would leave a COMMIT
// on its way without an answer, nor loses the op.
func TestBeginOutlivesItsCaller(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	e.beginHook = func(_ context.Context, id podid.ID, kind OpKind) {
		if id == sb.ID && kind == OpPause {
			cancel()
		}
	}
	hd, err := e.runner.Start(ctx, sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1",
		Incarnation: sb.Incarnation})
	if err != nil {
		t.Fatalf("a begin whose caller hung up: %v", err)
	}
	wctx, wcancel := context.WithTimeout(bg, 30*time.Second)
	defer wcancel()
	if res, err := hd.Wait(wctx); err != nil || res.State != model.StatePaused {
		t.Fatalf("the op: %+v %v", res, err)
	}
	wantState(t, h.row(sb.ID), model.StatePaused)
}

// A begin whose COMMIT's answer was lost reads the row: the op committed,
// and it runs; nothing is left in an op that nobody runs.
func TestBeginAdoptsALostCommit(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	var once sync.Once
	e.beginTxHook = func(id podid.ID, err error) error {
		if id == sb.ID && err == nil {
			once.Do(func() { err = errors.New("unexpected EOF") })
		}
		return err
	}
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	hd, err := e.runner.Start(ctx, sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1",
		Incarnation: sb.Incarnation})
	if err != nil {
		t.Fatalf("a begin that committed: %v", err)
	}
	if res, err := hd.Wait(ctx); err != nil || res.State != model.StatePaused {
		t.Fatalf("the op: %+v %v", res, err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StatePaused)
	if row.Op != "" {
		t.Fatalf("an op was left: %+v", row)
	}
}

// An op the row says is this instance's and that its runner does not run
// (its begin committed, but nothing ran it) is not waited for: Ensure
// takes it over at once, as a dead owner's, and so does the instance on
// its own, or releases it when it cannot reach the op's node. An op the
// runner runs is left alone.
func TestOrphanedOps(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	a := h.running(512)
	b := h.running(512)
	c := h.running(512)
	d := h.running(512)
	for _, sb := range []store.Sandbox{a, b, c, d} {
		h.settle(sb.ID)
	}

	// Ensure.
	before := h.opOfDeadOwner(a, OpPause, "i1")
	ctx, cancel := context.WithTimeout(bg, 20*time.Second)
	defer cancel()
	if _, err := e.Ensure(ctx, a.ID); err != nil {
		t.Fatalf("Ensure meeting an orphaned op: %v", err)
	}
	wantState(t, h.row(a.ID), model.StateRunning)
	detail := h.activityNamed(a.ID, activity.KindLifecycleStart, "pause on node n1 (taken over from instance i1)")
	if detail["trigger"] != string(TriggerEnsure) {
		t.Fatalf("taken over by %v", detail["trigger"])
	}
	if row := h.row(a.ID); row.OpID == before.OpID {
		t.Fatal("the orphaned op stayed")
	}

	// The instance: b's is taken over and ends; c's, whose node it cannot
	// reach, is released; d's runs, held at its call, and stays.
	h.opOfDeadOwner(b, OpPause, "i1")
	h.opOfDeadOwner(c, OpPause, "i1")
	h.exec("UPDATE sandboxes SET op_target = 'nowhere' WHERE id = $1", c.ID)
	arrived, release := h.gate("n1", podv1.PodService_SuspendPod_FullMethodName)
	defer release()
	hd, err := e.runner.Start(ctx, d.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1",
		Incarnation: d.Incarnation})
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, arrived, "d's pause at its call")
	running := h.row(d.ID)
	// c's release is read before anything resolves it: a sync of c, or a
	// reconcile of n1, which fleet asks for once the node's inventory hash
	// has differed from the registry's (c's op is in its row) for
	// mismatchSettle.
	releaseReads := h.holdPodReads("n1", c.ID)
	if err := e.orphans(bg); err != nil {
		t.Fatal(err)
	}
	if row := h.row(d.ID); row.OpID != running.OpID || row.ClientState != model.StatePausing {
		t.Fatalf("an op that runs was touched: %+v", row)
	}
	release()
	if res, err := hd.Wait(ctx); err != nil || res.State != model.StatePaused {
		t.Fatalf("d's pause: %+v %v", res, err)
	}
	h.waitState(b.ID, model.StatePaused, 10*time.Second)
	rc := h.row(c.ID)
	wantState(t, rc, model.StateUnknown)
	if rc.Op != "" || rc.Node != "n1" || rc.StatusDescription != "op pause of instance i1 was abandoned" {
		t.Fatalf("released row %+v", rc)
	}
	releaseReads()
}

// An orphan is found however many ops of the instance run before it in
// deadline order, and however many orphans there are: the look reads
// them all, a page at a time, without the ops it runs.
func TestOrphansPastABatch(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	e.t.orphanBatch = 1
	var sbs []store.Sandbox
	for range 4 {
		sb := h.running(512)
		h.settle(sb.ID)
		sbs = append(sbs, sb)
	}
	// Two pauses run, held at their calls, their deadlines first.
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	defer release()
	var arrived atomic.Int64
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		arrived.Add(1)
		select {
		case <-held:
		case <-ctx.Done():
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	var handles []*OpHandle
	for _, sb := range sbs[:2] {
		hd, err := e.runner.Start(ctx, sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1",
			Incarnation: sb.Incarnation})
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, hd)
	}
	eventually(t, 10*time.Second, "both pauses at their calls", func() bool { return arrived.Load() == 2 })
	// Two orphans of this instance, their deadlines (a hibernate's) later.
	var orphaned []store.Sandbox
	for i, sb := range sbs[2:] {
		h.opOfDeadOwner(sb, OpPause, "i1")
		h.exec("UPDATE sandboxes SET op_deadline = now() + $2::interval WHERE id = $1", sb.ID,
			fmt.Sprintf("%d hours", i+1))
		orphaned = append(orphaned, h.row(sb.ID))
	}

	if err := e.orphans(bg); err != nil {
		t.Fatal(err)
	}
	for _, o := range orphaned {
		row := h.row(o.ID)
		if row.OpID == o.OpID || !e.runner.runs(row) {
			t.Fatalf("orphan %s not taken over: %+v", o.ID, row)
		}
	}
	for i, hd := range handles {
		if row := h.row(sbs[i].ID); row.OpID != hd.opID {
			t.Fatalf("a pause that runs was touched: %+v", row)
		}
	}
	release()
	for _, sb := range sbs {
		h.waitState(sb.ID, model.StatePaused, 10*time.Second)
	}
}
