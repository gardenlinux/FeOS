// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

func TestEnsureCreates(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.sandbox(512)
	lease := h.ensure(sb.ID)
	if lease.Node != "n1" || lease.SandboxID != sb.ID || lease.Incarnation != sb.Incarnation {
		t.Fatalf("lease %+v", lease)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if !row.Materialised || row.ImageRef != testImage || row.RunID == "" || row.Node != "n1" {
		t.Fatalf("row after create %+v", row)
	}
	b, ok := h.booking(sb.ID)
	if !ok || b.Node != "n1" || !b.Gated {
		t.Fatalf("booking %+v %v", b, ok)
	}
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	if podIncarnation(info) != sb.Incarnation || info.GetSpec().GetLabels()[LabelProject] != testProject {
		t.Fatalf("pod labels %v", info.GetSpec().GetLabels())
	}
	// Running: Ensure returns at once with the same lease.
	if again := h.ensure(sb.ID); again != lease {
		t.Fatalf("lease %+v then %+v", lease, again)
	}
}

func TestEnsureErrorsByState(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	ensure := func(id podid.ID) error {
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		_, err := e.Ensure(ctx, id)
		return err
	}
	// Unknown sandbox.
	wantErr(t, ensure(podid.Mint()), model.ErrNotFound, "")

	// A stopped sandbox whose node is down.
	sb := h.running(512)
	h.settle(sb.ID)
	h.exec("UPDATE sandboxes SET client_state = 'stopped', gen = gen + 1 WHERE id = $1", sb.ID)
	h.exec("UPDATE nodes SET status = 'down' WHERE name = 'n1'")
	wantErr(t, ensure(sb.ID), model.ErrNodeUnavailable, "node n1 is down")
	h.exec("UPDATE nodes SET status = 'ready' WHERE name = 'n1'")

	// An unknown sandbox whose node is not connected.
	n.Partition(true)
	eventually(t, 10*time.Second, "n1 disconnected", func() bool { return !h.ins[0].srv.Connected("n1") })
	h.exec("UPDATE sandboxes SET client_state = 'unknown', status_description = 'node n1 down since then', gen = gen + 1 WHERE id = $1", sb.ID)
	wantErr(t, ensure(sb.ID), model.ErrNodeUnavailable, "node n1 is unavailable: node n1 down since then")
	// A running one too.
	h.exec("UPDATE sandboxes SET client_state = 'running', gen = gen + 1 WHERE id = $1", sb.ID)
	wantErr(t, ensure(sb.ID), model.ErrNodeUnavailable, "node n1 is not connected to instance i1")
	n.Partition(false)
	eventually(t, 10*time.Second, "n1 back", func() bool { return h.ins[0].srv.Connected("n1") })
	h.ensure(sb.ID)
}

func TestEnsureRestorePlacesOnceMore(t *testing.T) {
	h := newHarness(t)
	n1 := h.addNode("n1")
	h.addNode("n2")
	sb := h.hibernatedOn("n1")
	h.settle(sb.ID)
	// The restore on the record holder finds the record gone and fails:
	// hibernated without a node, and Ensure places once more elsewhere.
	n1.Script().SetHook(podv1.PodService_RestorePod_FullMethodName, func(context.Context, string, any) error {
		_, err := h.pods("n1").DeletePod(bg, &podv1.DeletePodRequest{PodId: sb.ID.String()})
		if err != nil {
			return err
		}
		return status.Error(codes.NotFound, "no pod")
	})
	lease := h.ensure(sb.ID)
	if lease.Node != "n2" {
		t.Fatalf("restored on %s", lease.Node)
	}
	h.placements(sb.ID, "", 3) // the create, the restore on n1, the one on n2
}

func TestPauseAndResumeREST(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	if err := e.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(sb.ID), model.StatePaused)
	// Nothing to do for paused, hibernated and pending.
	if err := e.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Pause(bg, h.sandbox(512).ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)
	// A failed sandbox cannot be paused.
	if err := n.Script().FailPod(sb.ID, "crashed"); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StateFailed, 10*time.Second)
	wantErr(t, e.Pause(bg, sb.ID), nil, "sandbox is failed")
	// A pause whose SuspendPod fails is an error.
	h.ensure(sb.ID)
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.Internal, "snapshot failed")
	})
	wantErr(t, e.Pause(bg, sb.ID), nil, "pause on node n1 failed: pause failed: snapshot failed")
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, nil)
	// A pause that takes longer than the wait goes on.
	e.t.pauseWait = time.Second
	n.Script().SetDelay(podv1.PodService_SuspendPod_FullMethodName, 3*time.Second)
	if err := e.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	h.waitState(sb.ID, model.StatePaused, 10*time.Second)
	if _, _, err := e.State(bg, podid.Mint()); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("state of an unknown sandbox: %v", err)
	}
	if st, node, err := e.State(bg, sb.ID); err != nil || st != model.StatePaused || node != "n1" {
		t.Fatalf("state %q %q %v", st, node, err)
	}
}

func TestEnsureResumeRefusedForCapacity(t *testing.T) {
	t.Run("object store", testResumeRefusedHibernates)
	t.Run("no object store", testResumeRefused)
}

func testResumeRefusedHibernates(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	sb := h.running(512)
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	// n1 is full now: the resume is refused under the node's lock; with an
	// object store there the sandbox is hibernated and restored on another
	// node.
	h.exec("UPDATE nodes SET slots = 0 WHERE name = 'n1'")
	h.addNode("n2")
	lease := h.ensure(sb.ID)
	if lease.Node != "n2" {
		t.Fatalf("resumed on %s", lease.Node)
	}
	row := h.row(sb.ID)
	if row.CheckpointHead == "" {
		t.Fatal("not hibernated on the way")
	}

}

// testResumeRefused: without an object store, the refusal is the caller's.
func testResumeRefused(t *testing.T) {
	h2 := newHarness(t)
	h2.addNode("bare", func(c *testnode.Config) { c.ObjectStore = nil })
	sb2 := h2.running(512)
	h2.startOp(sb2.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "bare"})
	h2.exec("UPDATE nodes SET slots = 0 WHERE name = 'bare'")
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	_, err := h2.eng().Ensure(ctx, sb2.ID)
	wantErr(t, err, model.ErrNoCapacity, "no capacity on node bare for sandbox "+sb2.ID.String()+" (retryable): no slots")
	if row := h2.row(sb2.ID); row.ClientState != model.StatePaused || row.Op != "" {
		t.Fatalf("row after a refused resume %+v", row)
	}
	if _, ok := h2.booking(sb2.ID); ok {
		t.Fatal("a refused admission booked")
	}
}

// TestRecreateWhileOldOpRuns: a sandbox deleted and created again with the
// same ID while its old incarnation's create is slow on this instance: the
// new row's Ensure begins an op of its own at once instead of waiting for
// the old one, and the old pod goes when its op ends.
func TestRecreateWhileOldOpRuns(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	h.addNode("n2")
	e := h.eng()
	id := podid.Mint()
	old := h.sandboxWithID(id, 512)
	arrived := make(chan string, 2)
	held := make(chan struct{})
	hold := func(node string) testnode.Hook {
		return func(ctx context.Context, _ string, req any) error {
			r, ok := req.(*podv1.CreatePodRequest)
			if !ok || r.GetSpec().GetLabels()[LabelIncarnation] != old.Incarnation.String() {
				return nil
			}
			arrived <- node
			select {
			case <-held:
			case <-ctx.Done():
			}
			return nil
		}
	}
	for _, name := range []string{"n1", "n2"} {
		h.node(name).Script().SetHook(podv1.PodService_CreatePod_FullMethodName, hold(name))
	}
	first := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(bg, 60*time.Second)
		defer cancel()
		_, err := e.Ensure(ctx, id)
		first <- err
	}()
	var oldNode string
	select {
	case oldNode = <-arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("the old incarnation's create did not arrive")
	}
	if _, _, err := e.Delete(bg, testProject, id); err != nil {
		t.Fatal(err)
	}
	sb := h.sandboxWithID(id, 512)
	ctx, cancel := context.WithTimeout(bg, 20*time.Second)
	defer cancel()
	lease, err := e.Ensure(ctx, id)
	close(held)
	if err != nil {
		t.Fatalf("ensure of the new incarnation: %v", err)
	}
	if lease.Incarnation != sb.Incarnation || lease.Node == oldNode {
		t.Fatalf("lease %+v (the old create went to %s)", lease, oldNode)
	}
	<-first
	// The old op ends fenced out; its pod goes through its tombstone.
	h.noPod(oldNode, id)
	if info := h.podState(lease.Node, id, podv1.PodState_POD_STATE_RUNNING); podIncarnation(info) != sb.Incarnation {
		t.Fatal("the new pod is not the new incarnation's")
	}
}

// ensureFor runs Ensure on e within d and reports how long it took.
func ensureFor(e *Engine, id podid.ID, d time.Duration) (model.Lease, time.Duration, error) {
	ctx, cancel := context.WithTimeout(bg, d)
	defer cancel()
	start := time.Now()
	lease, err := e.Ensure(ctx, id)
	return lease, time.Since(start), err
}

// wantHibernating checks the answer of an Ensure that met a hibernate it
// did not wait for: ErrBusy with the exact text, after the hibernate wait
// and well before the budget.
func wantHibernating(t *testing.T, id podid.ID, took time.Duration, err error) {
	t.Helper()
	want := "sandbox " + id.String() + " is hibernating; retry in a minute"
	if err == nil || err.Error() != want || !errors.Is(err, model.ErrBusy) {
		t.Fatalf("got error %v, want ErrBusy %q", err, want)
	}
	// It asks for a retry in a minute, as it says, also as the tools wrap
	// it: the REST tool routes' Retry-After.
	if got := apierr.RetryAfterOf(fmt.Errorf("ensure sandbox: %w", err), 5); got != 60 {
		t.Fatalf("Retry-After %d, want 60", got)
	}
	if took < 300*time.Millisecond || took > 10*time.Second {
		t.Fatalf("the answer took %v", took)
	}
}

// TestEnsureMeetsHibernate: Ensure meeting a hibernate of this instance in
// flight (one it joins, one it begins itself) waits for it within the
// hibernate wait only: a hibernate that does not end by then is ErrBusy at
// once and goes on, and a later Ensure restores the sandbox. A hibernate
// that ends within the wait is waited for, and the sandbox restored.
func TestEnsureMeetsHibernate(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	e.t.hibernateWait = 300 * time.Millisecond
	sb := h.running(512)
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})

	// Joined: the hibernate is held at the node.
	arrived, release := h.gate("n1", podv1.PodService_HibernatePod_FullMethodName)
	defer release()
	hd, err := h.begin(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, arrived, "the hibernate's call")
	_, took, err := ensureFor(e, sb.ID, time.Minute)
	wantHibernating(t, sb.ID, took, err)
	if row := h.row(sb.ID); row.ClientState != model.StateHibernating || row.Op != string(OpHibernate) {
		t.Fatalf("the hibernate did not go on: %+v", row)
	}
	release()
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if res, err := hd.Wait(ctx); err != nil || res.State != model.StateHibernated {
		t.Fatalf("the hibernate's end %+v %v", res, err)
	}
	if lease := h.ensure(sb.ID); lease.Node != "n1" {
		t.Fatalf("restored on %s", lease.Node)
	}
	wantState(t, h.row(sb.ID), model.StateRunning)

	// Begun by Ensure itself: the resume is refused for capacity and the
	// sandbox is hibernated, to be restored elsewhere.
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.exec("UPDATE nodes SET slots = 0 WHERE name = 'n1'")
	arrived, release2 := h.gate("n1", podv1.PodService_HibernatePod_FullMethodName)
	defer release2()
	_, took, err = ensureFor(e, sb.ID, time.Minute)
	wantHibernating(t, sb.ID, took, err)
	waitClosed(t, arrived, "the hibernate's call")
	h.exec("UPDATE nodes SET slots = 50 WHERE name = 'n1'")
	release2()
	h.waitState(sb.ID, model.StateHibernated, 15*time.Second)
	if lease := h.ensure(sb.ID); lease.Node != "n1" {
		t.Fatalf("restored on %s", lease.Node)
	}

	// A hibernate that ends within the wait: Ensure waits for it and
	// restores the sandbox.
	e.t.hibernateWait = 20 * time.Second
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	before := h.row(sb.ID)
	arrived, release3 := h.gate("n1", podv1.PodService_HibernatePod_FullMethodName)
	defer release3()
	if _, err := h.begin(sb.ID, OpIntent{Kind: OpHibernate, Trigger: TriggerExplicit, Target: "n1"}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, arrived, "the hibernate's call")
	done := make(chan error, 1)
	go func() {
		_, _, err := ensureFor(e, sb.ID, time.Minute)
		done <- err
	}()
	never(t, 500*time.Millisecond, "Ensure returned during the hibernate", func() bool { return len(done) > 0 })
	release3()
	if err := <-done; err != nil {
		t.Fatalf("Ensure after a hibernate within the wait: %v", err)
	}
	after := h.row(sb.ID)
	wantState(t, after, model.StateRunning)
	if after.CheckpointHead == "" || !after.CheckpointAt.After(before.CheckpointAt) {
		t.Fatalf("not restored from a new checkpoint: %+v (before %+v)", after, before)
	}
}

// endingCtx is a ctx whose Done ends the op h as the caller takes the
// channel, so that the op's end and the wait's are ready at once in the
// caller's select. Its done is nil (it never ends), or closed: it has
// ended by its deadline.
type endingCtx struct {
	context.Context
	h    *OpHandle
	once sync.Once
	done chan struct{}
}

func (c *endingCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.h.done) })
	return c.done
}

func (c *endingCtx) Err() error {
	if c.done != nil {
		return context.DeadlineExceeded
	}
	return nil
}

// TestEnsureHibernateEndsAtTheBound: a hibernate this instance runs that
// ends as the hibernate wait does (at its bound, or at the end of
// Ensure's budget) has ended: Ensure takes its end and goes on, and does
// not answer that the sandbox is hibernating.
func TestEnsureHibernateEndsAtTheBound(t *testing.T) {
	e := &Engine{t: timings{hibernateWait: time.Minute}}
	for i := range 100 {
		for _, budget := range []bool{false, true} {
			h := &OpHandle{done: make(chan struct{}), kind: OpHibernate, target: "n1",
				res: OpResult{State: model.StateHibernated}}
			en := &ensurer{e: e, id: podid.Mint(), ops: map[string]OpResult{}}
			ctx := &endingCtx{Context: bg, h: h}
			if budget {
				ctx.done = make(chan struct{})
				close(ctx.done)
			} else {
				// The bound has passed: its timer is ready at once.
				en.hibernateSince = time.Now().Add(-2 * e.t.hibernateWait)
			}
			if err := en.await(ctx, h); !errors.Is(err, errContinue) {
				t.Fatalf("round %d (the budget ended: %v): a hibernate that ended gave %v", i, budget, err)
			}
		}
	}
}

// TestEnsurePreemptsHibernate: Ensure meeting a hibernate of this instance
// whose node says that a resume abandons it now begins a resume in its
// place: the sandbox runs again on its node, from its snapshot, well
// within the hibernate wait; the hibernate ends fenced out ("abandoned")
// and writes nothing.
func TestEnsurePreemptsHibernate(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb, hd, upload := h.hibernatingAtUpload("n1")
	h.wantHibernation("n1", sb.ID, podv1.HibernationPhase_HIBERNATION_PHASE_UPLOADING_DISK, true, hd.opID.String())
	if reqs := upload.hibernateRequests(); len(reqs) != 1 || reqs[0].GetRequestId() != hd.opID.String() || reqs[0].GetNoAbandon() {
		t.Fatalf("the hibernate's request %v", reqs)
	}
	lease, took, err := ensureFor(e, sb.ID, time.Minute)
	if err != nil || lease.Node != "n1" {
		t.Fatalf("ensure: %+v %v", lease, err)
	}
	if took > e.t.hibernateWait/3 {
		t.Fatalf("the preempt took %v", took)
	}
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if res, err := hd.Wait(ctx); err != nil || !res.fenced {
		t.Fatalf("the preempted hibernate's end %+v %v", res, err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if row.Op != "" || row.CheckpointHead != "" || row.CheckpointEpoch != 0 || row.Node != "n1" {
		t.Fatalf("row after the preempt %+v", row)
	}
	if b, ok := h.booking(sb.ID); !ok || b.Node != "n1" || !b.Gated {
		t.Fatalf("booking after the preempt %+v %v", b, ok)
	}
	if origin := h.runOrigin(sb.ID); origin != "snapshot" {
		t.Fatalf("run origin %q", origin)
	}
	start := h.activityNamed(sb.ID, "lifecycle_start", "resume on node n1 (abandons the hibernate of instance i1)")
	if start["abandons"] != hd.opID.String() || start["from"] != model.StatePaused || start["trigger"] != string(TriggerEnsure) {
		t.Fatalf("the preempt's start %v", start)
	}
	end := h.activityNamed(sb.ID, "lifecycle_end", "hibernate on node n1: abandoned")
	if errText, _ := end["error"].(string); !strings.Contains(errText, "its hibernation was abandoned") {
		t.Fatalf("the hibernate's end %v", end)
	}
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	if info.GetHibernation() != nil || info.GetCheckpoint().GetHeadKey() != "" {
		t.Fatalf("pod after the preempt %v", info)
	}
	if upload.count() != 1 {
		t.Fatalf("%d hibernations", upload.count())
	}
}
