// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// startOp begins an op on the first instance and waits for its end.
func (h *harness) startOp(id podid.ID, in OpIntent) OpResult {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	if in.Incarnation == uuid.Nil {
		in.Incarnation = h.row(id).Incarnation
	}
	hd, err := h.eng().runner.Start(ctx, id, in)
	if err != nil {
		h.t.Fatalf("start %s: %v", in.Kind, err)
	}
	res, err := hd.Wait(ctx)
	if err != nil {
		h.t.Fatalf("wait for %s: %v", in.Kind, err)
	}
	return res
}

func TestOpKinds(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.running(1024)
	if sb.LastActiveAt.IsZero() || sb.AvoidNodes == nil || len(sb.AvoidNodes) != 0 {
		t.Fatalf("after create %+v", sb)
	}

	// pause: SuspendPod; the run closes, the booking goes.
	res := h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	if res.State != model.StatePaused || res.Err != nil || res.Node != "n1" {
		t.Fatalf("pause: %+v", res)
	}
	row := h.row(sb.ID)
	if row.RunID != "" || row.PausedAt.IsZero() || row.Op != "" || row.OpID != uuid.Nil {
		t.Fatalf("paused row %+v", row)
	}
	if _, ok := h.booking(sb.ID); ok {
		t.Fatal("a paused sandbox is booked")
	}
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_SUSPENDED)

	// resume through Ensure: ResumePod, booked again, a new run.
	h.ensure(sb.ID)
	row = h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if row.RunID == "" {
		t.Fatal("no run after the resume")
	}
	if b, ok := h.booking(sb.ID); !ok || !b.Gated || b.Node != "n1" {
		t.Fatalf("booking after the resume %+v %v", b, ok)
	}

	// hibernate of a running sandbox is the drain's only: refused here.
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().runner.Start(ctx, sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1",
		Incarnation: row.Incarnation})
	wantErr(t, err, model.ErrPrecondition, "is running")

	// pause, then hibernate: a checkpoint of the node's object store.
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	res = h.startOp(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	if res.State != model.StateHibernated || res.Node != "n1" {
		t.Fatalf("hibernate: %+v", res)
	}
	row = h.row(sb.ID)
	if row.CheckpointHead == "" || row.CheckpointEpoch == 0 || !row.CheckpointMemory || row.CheckpointStore != h.objs.ID() ||
		row.CheckpointAt.IsZero() {
		t.Fatalf("hibernated row %+v", row)
	}
	if _, ok := h.booking(sb.ID); ok {
		t.Fatal("a hibernated sandbox is booked")
	}

	// restore through Ensure, on the node that holds the record.
	h.ensure(sb.ID)
	row = h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if row.Node != "n1" {
		t.Fatalf("restored on %q", row.Node)
	}

	// start: the pod failed; a Sync sees it, Ensure starts it on its disk.
	if err := h.node("n1").Script().FailPod(sb.ID, "the microVM crashed"); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateFailed, 10*time.Second)
	if d := h.row(sb.ID).StatusDescription; d != "the microVM crashed" {
		t.Fatalf("failed description %q", d)
	}
	h.ensure(sb.ID)
	wantState(t, h.row(sb.ID), model.StateRunning)

	// stopped: a StopPod outside ferund, then Ensure starts it again.
	if _, err := h.pods("n1").StopPod(bg, &podv1.StopPodRequest{PodId: sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateStopped, 10*time.Second)
	h.ensure(sb.ID)
	wantState(t, h.row(sb.ID), model.StateRunning)
}

func TestOutcomePendingAtDeadline(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	release := make(chan struct{})
	n.Script().SetBringUp(func(ctx context.Context, _ podid.ID) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	h.eng().t.opDeadline = func(OpKind, int) time.Duration { return 1500 * time.Millisecond }
	sb := h.sandbox(512)
	res := h.startOp(sb.ID, OpIntent{Kind: OpCreate, Trigger: TriggerEnsure, Candidates: []string{"n1"}, ImageRef: testImage})
	if res.State != model.StateCreating || !strings.HasPrefix(res.Description, "still starting on node n1 after") {
		t.Fatalf("create at the deadline: %+v", res)
	}
	row := h.row(sb.ID)
	if !row.Materialised || row.PendingSince.IsZero() || row.Op != "" {
		t.Fatalf("row %+v", row)
	}
	if b, ok := h.booking(sb.ID); !ok || b.Node != "n1" {
		t.Fatal("a creating sandbox is not booked")
	}
	// The pod comes up: Ensure waits for it (creating without an op) and
	// syncs.
	close(release)
	h.ensure(sb.ID)
	row = h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if !row.PendingSince.IsZero() {
		t.Fatal("pending_since kept after the pod ran")
	}
}

func TestPendingSinceAtFirstObservation(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	release := make(chan struct{})
	n.Script().SetBringUp(func(ctx context.Context, _ podid.ID) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	sb := h.sandbox(512)
	done := make(chan error, 1)
	go func() {
		_, err := h.eng().Ensure(bg, sb.ID)
		done <- err
	}()
	// While the create's op still waits for the pod, the waiter recorded
	// the first PENDING observation (a write of its own, no gen bump).
	eventually(t, 10*time.Second, "pending_since set during the op", func() bool {
		row := h.row(sb.ID)
		return row.Op == string(OpCreate) && !row.PendingSince.IsZero()
	})
	row := h.row(sb.ID)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after := h.row(sb.ID)
	if after.Gen <= row.Gen {
		t.Fatalf("gen %d then %d", row.Gen, after.Gen)
	}
	wantState(t, after, model.StateRunning)
}

func TestOutcomeFailedEnsureReturns(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	n.Script().SetBringUp(func(context.Context, podid.ID) error { return errors.New("image pull failed") })
	sb := h.sandbox(512)
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, nil, "create on node n1 failed: image pull failed")
	row := h.row(sb.ID)
	wantState(t, row, model.StateFailed)
	if row.StatusDescription != "image pull failed" || row.RunID != "" {
		t.Fatalf("failed row %+v", row)
	}
	// Ensure did not go on with a start after the failed create.
	var starts int
	h.scalar(&starts, "SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'lifecycle_start' AND detail->>'op' = 'start'", sb.ID)
	if starts != 0 {
		t.Fatalf("%d starts after a failed create", starts)
	}
	if _, ok := h.booking(sb.ID); ok {
		t.Fatal("a failed sandbox is booked")
	}
}

func TestOutcomePauseAndHibernateFailures(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	sb := h.running(512)

	// A pause whose SuspendPod fails leaves the pod running.
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.Internal, "snapshot failed")
	})
	res := h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	if res.State != model.StateRunning || res.Description != "pause failed: snapshot failed" {
		t.Fatalf("failed pause: %+v", res)
	}
	if b, ok := h.booking(sb.ID); !ok || b.Node != "n1" {
		t.Fatal("a running sandbox lost its booking")
	}
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, nil)

	// A hibernation that fails leaves the pod suspended.
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	n.Script().SetHook(podv1.PodService_HibernatePod_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.Unavailable, "upload failed")
	})
	res = h.startOp(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	if res.State != model.StatePaused || res.Description != "hibernation failed: upload failed" {
		t.Fatalf("failed hibernate: %+v", res)
	}
	if h.row(sb.ID).CheckpointHead != "" {
		t.Fatal("a failed hibernation wrote a checkpoint")
	}
}

func TestOutcomeStaleRecord(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.sandbox(512)
	// A pod of this ID from an earlier incarnation is still on the node.
	old := uuid.New()
	stale := podSpec(store.Sandbox{ID: sb.ID, Owner: testProject, Incarnation: old, VCPUs: 1, MemoryMiB: 512, ImageRef: testImage})
	if _, err := h.pods("n1").CreatePod(bg, &podv1.CreatePodRequest{PodId: sb.ID.String(), Spec: stale}); err != nil {
		t.Fatal(err)
	}
	// Hold the stale record's deletion, so that the tombstone is seen.
	deletes := make(chan struct{})
	h.node("n1").Script().SetHook(podv1.PodService_DeletePod_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		select {
		case <-deletes:
		case <-ctx.Done():
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	wantErr(t, err, nil, "found a stale pod of incarnation "+old.String())
	row := h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if row.Node != "" || len(row.AvoidNodes) != 0 {
		t.Fatalf("pending row %+v", row)
	}
	// The stale record got a tombstone of its own incarnation, which keeps
	// the pod's ID off the node until it is processed.
	ts := h.tombstones(sb.ID)
	if len(ts) != 1 || ts[0].Incarnation != old || ts[0].Node != "n1" {
		t.Fatalf("tombstones %+v", ts)
	}
	p, err := h.eng().place(bg, row, OpCreate)
	if err != nil || len(p.candidates) != 0 || len(p.tombstoneOnly) != 1 {
		t.Fatalf("placement with the tombstone %+v %v", p, err)
	}
	close(deletes)
	// The next Ensure waits for (or has) the tombstone processed, then
	// creates.
	h.ensure(sb.ID)
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	if podIncarnation(info) != sb.Incarnation {
		t.Fatal("the stale pod survived")
	}
	if len(h.tombstones(sb.ID)) != 0 {
		t.Fatal("the tombstone stayed")
	}
}

func TestOutcomeTAPCollisionPlacesElsewhere(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	h.addNode("n2")
	// Binpack prefers the fuller node: book something on n1 so that it is
	// the first candidate.
	filler := h.running(4096)
	if filler.Node != "n1" {
		h.t.Skipf("filler placed on %s", filler.Node)
	}
	// Only pods with UUIDs (made before ADR 0019) share TAP prefixes.
	sb := h.sandboxWithID(legacyID(), 512)
	// A pod that ferund does not know holds the TAP prefix on n1.
	other := podid.ID(sb.ID.String()[:8] + "-0000-4000-8000-000000000000")
	if other == sb.ID {
		t.Skip("collision with itself")
	}
	if _, err := h.pods("n1").CreatePod(bg, &podv1.CreatePodRequest{PodId: other.String(), Spec: &podv1.PodSpec{
		Resources:  &podv1.Resources{Vcpus: 1, MemoryMib: 256},
		Containers: []*podv1.ContainerSpec{{Name: "main", ImageRef: testImage}},
	}}); err != nil {
		t.Fatal(err)
	}
	lease := h.ensure(sb.ID)
	if lease.Node != "n2" {
		t.Fatalf("placed on %s", lease.Node)
	}
	row := h.row(sb.ID)
	if len(row.AvoidNodes) != 0 {
		t.Fatalf("avoid_nodes after a run %v", row.AvoidNodes)
	}
	eventually(t, 10*time.Second, "the pending state with the avoidance recorded", func() bool {
		var avoided int
		h.scalar(&avoided, "SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'sandbox_state' AND detail->>'description' LIKE '%placed elsewhere on the next use'", sb.ID)
		return avoided > 0
	})
}

func TestOutcomeNotFound(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	// A materialised sandbox whose pod is gone: lost.
	sb := h.running(512)
	// The pod goes while the pause is under way.
	vanish := func(id podid.ID) func(context.Context, string, any) error {
		return func(context.Context, string, any) error {
			_, err := h.pods("n1").DeletePod(bg, &podv1.DeletePodRequest{PodId: id.String()})
			return err
		}
	}
	h.node("n1").Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, vanish(sb.ID))
	res := h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.node("n1").Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, nil)
	if res.State != model.StateLost || res.Description != "not on node n1" || res.Node != "n1" {
		t.Fatalf("pause of a gone pod: %+v", res)
	}
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	if !errors.Is(err, model.ErrLost) || err.Error() != "sandbox lost" {
		t.Fatalf("ensure of a lost sandbox: %v", err)
	}

	// One with a checkpoint: hibernated without a node, then restored.
	sb2 := h.running(512)
	h.startOp(sb2.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.startOp(sb2.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	h.ensure(sb2.ID)
	h.startOp(sb2.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.node("n1").Script().SetHook(podv1.PodService_ResumePod_FullMethodName, vanish(sb2.ID))
	res = h.startOp(sb2.ID, OpIntent{Kind: OpResume, Trigger: TriggerEnsure, Target: "n1"})
	h.node("n1").Script().SetHook(podv1.PodService_ResumePod_FullMethodName, nil)
	if res.State != model.StateHibernated || res.Node != "" {
		t.Fatalf("resume of a gone pod with a checkpoint: %+v", res)
	}
	h.ensure(sb2.ID)
	wantState(t, h.row(sb2.ID), model.StateRunning)

	// A create whose call fails before a pod exists with an error of the
	// pod's (not the node's): pending, the node not avoided.
	sb3 := h.sandbox(512)
	h.exec("UPDATE sandboxes SET image_ref = 'not a valid ref' WHERE id = $1", sb3.ID)
	h.node("n1").Script().SetHook(podv1.PodService_CreatePod_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.InvalidArgument, "bad image")
	})
	res = h.startOp(sb3.ID, OpIntent{Kind: OpCreate, Trigger: TriggerEnsure, Candidates: []string{"n1"}})
	if res.State != model.StatePending || res.Description != "create on node n1 failed: bad image" {
		t.Fatalf("failed create: %+v", res)
	}
	if row := h.row(sb3.ID); row.Node != "" || len(row.AvoidNodes) != 0 || row.Materialised {
		t.Fatalf("row after a failed create %+v", row)
	}
	if _, ok := h.booking(sb3.ID); ok {
		t.Fatal("a pending sandbox is booked")
	}
}

func TestOutcomeUnknown(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	e.t.opDeadline = func(OpKind, int) time.Duration { return time.Second }
	e.t.settleGrace = 300 * time.Millisecond
	block := func(ctx context.Context, _ string, _ any) error {
		<-ctx.Done()
		return status.FromContextError(ctx.Err()).Err()
	}
	sb := h.sandbox(512)
	n.Script().SetHook(podv1.PodService_CreatePod_FullMethodName, block)
	n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, block)
	res := h.startOp(sb.ID, OpIntent{Kind: OpCreate, Trigger: TriggerEnsure, Candidates: []string{"n1"}, ImageRef: testImage})
	if res.State != model.StateUnknown || !strings.HasPrefix(res.Description, "create on node n1: outcome unknown (") {
		t.Fatalf("create without an answer: %+v", res)
	}
	row := h.row(sb.ID)
	if row.Node != "n1" {
		t.Fatalf("unknown row lost its node: %+v", row)
	}
	if b, ok := h.booking(sb.ID); !ok || b.Node != "n1" {
		t.Fatal("the booking of an unknown sandbox changed")
	}
	// The node answers again: Sync resolves it (no pod was made).
	n.Script().SetHook(podv1.PodService_CreatePod_FullMethodName, nil)
	n.Script().SetHook(podv1.PodService_GetPod_FullMethodName, nil)
	if err := e.Sync(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	row = h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if row.Node != "" {
		t.Fatalf("pending with a node %+v", row)
	}
	e.t.opDeadline = OpDeadline
	h.ensure(sb.ID)
}

// TestPauseCoversFeosdSuspend: a pause waits feosd's whole suspend
// budget and 30 s more, so that feosd answers, the suspend or "the VM is
// busy", before the pause gives up; a hibernate, whose feosd suspends a
// running pod first, as long and its upload's time more. The budget is
// feosd's (microvm_service::vmm::budget, whose test has the same table):
// 190 s and 5 s per GiB, of which the snapshot's wait is 120 s and 4 s per
// GiB.
func TestPauseCoversFeosdSuspend(t *testing.T) {
	for _, c := range []struct {
		mem    int
		budget time.Duration
	}{{512, 195 * time.Second}, {1024, 195 * time.Second}, {1025, 200 * time.Second}, {4096, 210 * time.Second},
		{16384, 270 * time.Second}} {
		if got := feosdSuspendBudget(c.mem); got != c.budget {
			t.Errorf("feosd's suspend of %d MiB: %v, want %v", c.mem, got, c.budget)
		}
		if got := OpDeadline(OpPause, c.mem); got != c.budget+30*time.Second {
			t.Errorf("pause of %d MiB: %v, want feosd's %v and 30s", c.mem, got, c.budget)
		}
		if got := OpDeadline(OpHibernate, c.mem); got < OpDeadline(OpPause, c.mem)+DefaultHibernateTimeout {
			t.Errorf("hibernate of %d MiB: %v, shorter than a pause and the hibernate timeout", c.mem, got)
		}
	}
}

// TestTombstoneCoversASuspend: a tombstone's DeletePod waits behind a
// suspend that holds the pod's lock in feosd, and has its own steps
// after it, the pod's stop grace among them: its deadline is a pause's,
// the tombstone's base and the grace more.
func TestTombstoneCoversASuspend(t *testing.T) {
	const base = 2 * time.Minute
	for _, mem := range []int{1024, 4096, 16384} {
		for _, grace := range []time.Duration{0, model.ServiceStopGrace} {
			if got, pause := tombstoneDeadline(base, mem, grace), OpDeadline(OpPause, mem); got < pause+base+grace {
				t.Errorf("tombstone of %d MiB with a grace of %v: %v, shorter than a pause's %v, %v and the grace",
					mem, grace, got, pause, base)
			}
		}
	}
}

func TestOpDeadlines(t *testing.T) {
	cases := []struct {
		kind OpKind
		mem  int
		want time.Duration
	}{
		{OpCreate, 512, 7*time.Minute + 5*time.Second},
		{OpStart, 65536, 7*time.Minute + 5*time.Second},
		{OpResume, 1025, 7*time.Minute + 5*time.Second + 16*time.Second},
		{OpRestore, 2048, 7*time.Minute + 5*time.Second + 16*time.Second},
		{OpPause, 512, 3*time.Minute + 40*time.Second + 5*time.Second},
		{OpPause, 4096, 3*time.Minute + 40*time.Second + 20*time.Second},
		{OpHibernate, 65536, 3*time.Minute + 40*time.Second + 64*5*time.Second + time.Hour + 64*16*time.Second},
		{OpRepair, 1024, 7*time.Minute + 5*time.Second + 8*time.Second},
	}
	for _, c := range cases {
		if got := OpDeadline(c.kind, c.mem); got != c.want {
			t.Errorf("%s of %d MiB: %v, want %v", c.kind, c.mem, got, c.want)
		}
	}
	// A resume may wait for a hibernation and then restore the pod: it has
	// the restore's deadline.
	for _, mem := range []int{0, 512, 1024, 1025, 65536} {
		if r, s := deadlineFor(OpResume, mem, time.Hour), deadlineFor(OpRestore, mem, time.Hour); r != s {
			t.Errorf("resume of %d MiB: %v, restore %v", mem, r, s)
		}
	}
}

// TestHibernateNoAbandonPerTrigger: every HibernatePod carries the op's id
// as its request id, and no_abandon for a drain's hibernate (drain,
// drain_forced), Ensure's off a draining node, an explicit one, and a
// takeover on a draining node; not for a hibernate-after's, Ensure's after
// the node refused the resume for capacity, or a takeover on a node that
// does not drain.
func TestHibernateNoAbandonPerTrigger(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	calls := h.watchCalls("n1", podv1.PodService_HibernatePod_FullMethodName, false)
	// paused brings the sandbox back to n1 and pauses it, for long.
	paused := func() {
		t.Helper()
		h.exec("UPDATE nodes SET draining = false, slots = 50 WHERE name = 'n1'")
		h.ensure(sb.ID)
		h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
		h.exec("UPDATE sandboxes SET paused_at = now() - interval '1 hour' WHERE id = $1", sb.ID)
	}
	check := func(what string, n int, opID uuid.UUID, want bool) {
		t.Helper()
		reqs := calls.hibernateRequests()
		if len(reqs) != n+1 {
			t.Fatalf("%s: %d HibernatePods, want %d", what, len(reqs), n+1)
		}
		if r := reqs[n]; r.GetNoAbandon() != want || r.GetRequestId() != opID.String() {
			t.Errorf("%s: no_abandon %v, request id %q; want %v, %s", what, r.GetNoAbandon(), r.GetRequestId(), want, opID)
		}
	}
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	wait := func(what string, hd *OpHandle) {
		t.Helper()
		if res, err := hd.Wait(ctx); err != nil || res.State != model.StateHibernated {
			t.Fatalf("%s: %+v %v", what, res, err)
		}
	}

	for _, c := range []struct {
		trigger OpTrigger
		want    bool
	}{{TriggerHibernateAfter, false}, {TriggerDrain, true}, {TriggerDrainForced, true}, {TriggerExplicit, true}} {
		paused()
		n := calls.count()
		hd, err := h.begin(sb.ID, OpIntent{Kind: OpHibernate, Trigger: c.trigger, Target: "n1"})
		if err != nil {
			t.Fatalf("%s: %v", c.trigger, err)
		}
		wait(string(c.trigger), hd)
		check(string(c.trigger), n, hd.opID, c.want)
	}

	// Ensure's: the node drains, or has no slot for the resume. The restore
	// that follows finds no node; only the hibernate's call matters here.
	for _, c := range []struct {
		what, sql string
		want      bool
	}{
		{"ensure off a draining node", "UPDATE nodes SET draining = true WHERE name = 'n1'", true},
		{"ensure after a refusal for capacity", "UPDATE nodes SET slots = 0 WHERE name = 'n1'", false},
	} {
		paused()
		n := calls.count()
		h.exec(c.sql)
		if _, _, err := ensureFor(e, sb.ID, 30*time.Second); err == nil {
			t.Fatalf("%s: restored", c.what)
		}
		// An op's start reaches the activity log after its begin: wait
		// until the op of every HibernatePod has its start there.
		eventually(t, 10*time.Second, c.what+": the hibernate's start recorded", func() bool {
			var starts int
			h.scalar(&starts, `SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'lifecycle_start'
				AND detail->>'op' = 'hibernate'`, sb.ID)
			return starts == n+1
		})
		var span string
		h.scalar(&span, `SELECT span_id FROM activity WHERE sandbox_id = $1 AND kind = 'lifecycle_start'
			AND detail->>'op' = 'hibernate' ORDER BY ts_ns DESC LIMIT 1`, sb.ID)
		check(c.what, n, uuid.MustParse(strings.TrimPrefix(span, "op-")), c.want)
	}

	// A takeover: the op's trigger is not in the row; the node's draining
	// decides.
	h.deadInstance("gone")
	for _, draining := range []bool{true, false} {
		paused()
		h.settle(sb.ID)
		row := h.opOfDeadOwner(h.row(sb.ID), OpHibernate, "gone")
		h.exec("UPDATE nodes SET draining = $1 WHERE name = 'n1'", draining)
		n := calls.count()
		hd, err := h.begin(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerRepair, Target: "n1", takeover: row.OpID})
		if err != nil || !hd.takeover {
			t.Fatalf("takeover: %v", err)
		}
		wait("takeover", hd)
		check(fmt.Sprintf("takeover (draining %v)", draining), n, hd.opID, draining)
	}
}

// TestRepairCoversTheStopGrace: a repair's and a stop's StopPod lets a
// running pod's containers exit after SIGTERM: an image service's
// instance's repair and stop have its stop grace more than the table's;
// every other op, and a repair or stop of any other pod, the table's. A
// stop's own deadline covers a pause's, as its StopPod may wait for the
// pod's lock behind a suspend feosd still runs, and its own steps.
func TestRepairCoversTheStopGrace(t *testing.T) {
	e := &Engine{t: defaultTimings(DefaultHibernateTimeout)}
	service := store.Sandbox{ID: podid.Mint(), MemoryMiB: 2048, EnvironmentID: uuid.New(), Instance: true,
		LogLabels: map[string]string{model.LogKind: "service", model.LogObject: "api"}}
	addon := service
	addon.LogLabels = map[string]string{model.LogKind: "addon", model.LogObject: "db"}
	for _, k := range []OpKind{OpRepair, OpStop} {
		if got, want := e.opDeadlineOf(k, service), OpDeadline(k, 2048)+model.ServiceStopGrace; got != want {
			t.Errorf("%s of a service's instance: %v, want %v", k, got, want)
		}
		if got := e.opDeadlineOf(k, addon); got != OpDeadline(k, 2048) {
			t.Errorf("%s of an addon: %v", k, got)
		}
	}
	if got, pause := OpDeadline(OpStop, 2048), OpDeadline(OpPause, 2048); got < pause+time.Minute {
		t.Errorf("a stop's deadline %v leaves less than a minute past a pause's %v", got, pause)
	}
	for _, k := range []OpKind{OpCreate, OpStart, OpResume, OpRestore, OpPause, OpHibernate} {
		if got := e.opDeadlineOf(k, service); got != OpDeadline(k, 2048) {
			t.Errorf("%s of a service's instance: %v, want %v", k, got, OpDeadline(k, 2048))
		}
	}
}

// TestHibernateDeadline: a hibernate op's deadline is a pause's plus the
// instance's hibernate timeout and 16 s per GiB, in a fresh begin and in a
// takeover; the other kinds keep theirs, and an engine without one has the
// default.
func TestHibernateDeadline(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.HibernateTimeout = 2 * time.Hour })
	h.addNode("n1")
	e := h.eng()
	want := OpDeadline(OpPause, 1024) + 2*time.Hour + 16*time.Second
	if got := e.t.opDeadline(OpHibernate, 1024); got != want {
		t.Fatalf("hibernate of 1 GiB: %v, want %v", got, want)
	}
	for _, k := range []OpKind{OpCreate, OpStart, OpResume, OpRestore, OpPause, OpRepair} {
		if got := e.t.opDeadline(k, 1024); got != OpDeadline(k, 1024) {
			t.Errorf("%s: %v, want %v", k, got, OpDeadline(k, 1024))
		}
	}
	def, err := New(Config{InstanceID: "i9"}, Deps{DB: h.d, Conns: h.ins[0].srv})
	if err != nil {
		t.Fatal(err)
	}
	if got := def.t.opDeadline(OpHibernate, 1024); got != OpDeadline(OpPause, 1024)+DefaultHibernateTimeout+16*time.Second ||
		got != OpDeadline(OpHibernate, 1024) {
		t.Fatalf("default hibernate deadline %v", got)
	}

	// The row's op_deadline, as begin and takeover write it.
	opLeft := func(id podid.ID) time.Duration {
		t.Helper()
		var secs float64
		h.scalar(&secs, "SELECT extract(epoch FROM op_deadline - now()) FROM sandboxes WHERE id = $1", id)
		return time.Duration(secs * float64(time.Second))
	}
	near := func(what string, got time.Duration) {
		t.Helper()
		if got > want || got < want-30*time.Second {
			t.Fatalf("%s: op deadline %v ahead, want %v", what, got, want)
		}
	}
	sb := h.running(1024)
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	arrived, release := h.gate("n1", podv1.PodService_HibernatePod_FullMethodName)
	defer release()
	hd, err := h.begin(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, arrived, "the hibernate's call")
	near("begin", opLeft(sb.ID))
	if d := time.Until(hd.deadline); d > want || d < want-30*time.Second {
		t.Fatalf("the node call's deadline is %v ahead, want %v", d, want)
	}
	release()
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if res, err := hd.Wait(ctx); err != nil || res.State != model.StateHibernated {
		t.Fatalf("hibernate %+v %v", res, err)
	}

	// A takeover of a dead owner's hibernate gets a deadline of the same
	// length.
	h.settle(sb.ID)
	h.deadInstance("gone")
	row := h.opOfDeadOwner(h.row(sb.ID), OpHibernate, "gone")
	if left := opLeft(sb.ID); left > time.Minute {
		t.Fatalf("the dead owner's deadline is %v ahead", left)
	}
	hd, err = h.begin(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerRepair, Target: "n1", takeover: row.OpID})
	if err != nil {
		t.Fatal(err)
	}
	if !hd.takeover {
		t.Fatal("not a takeover")
	}
	near("takeover", opLeft(sb.ID))
	if res, err := hd.Wait(ctx); err != nil || res.State != model.StateHibernated {
		t.Fatalf("taken over hibernate %+v %v", res, err)
	}
}

func podReq(id podid.ID) *podv1.GetPodRequest { return &podv1.GetPodRequest{PodId: id.String()} }

// TestCreateFailsWithInternal: a create whose call fails with INTERNAL
// (an error that may pass) does not keep the sandbox off its node, and
// Ensure's error names the op once.
func TestCreateFailsWithInternal(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	sb := h.sandbox(512)
	var failed atomic.Bool
	n.Script().SetHook(podv1.PodService_CreatePod_FullMethodName, func(context.Context, string, any) error {
		if failed.Swap(true) {
			return nil
		}
		return status.Error(codes.Internal, "diskd is busy")
	})
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	_, err := h.eng().Ensure(ctx, sb.ID)
	if err == nil || err.Error() != "create on node n1 failed: diskd is busy" {
		t.Fatalf("ensure after a failed create: %v", err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StatePending)
	if len(row.AvoidNodes) != 0 {
		t.Fatalf("avoid_nodes after INTERNAL %v", row.AvoidNodes)
	}
	// The next use creates it on the same node.
	if lease := h.ensure(sb.ID); lease.Node != "n1" {
		t.Fatalf("lease %+v", lease)
	}
}
