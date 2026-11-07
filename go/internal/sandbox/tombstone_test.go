// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// tombstone writes a tombstone directly.
func (h *harness) tombstone(id podid.ID, node string, inc uuid.UUID, notBefore time.Time) store.TombstoneKey {
	h.t.Helper()
	t, err := h.st.UpsertTombstone(bg, store.Tombstone{PodID: id, Node: node, Incarnation: inc, Reason: "test", NotBefore: notBefore})
	if err != nil {
		h.t.Fatal(err)
	}
	return t.Key()
}

func TestTombstoneIncarnationChecks(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	pc := h.pods("n1")
	create := func(spec *podv1.PodSpec) podid.ID {
		id := podid.Mint()
		if _, err := pc.CreatePod(bg, &podv1.CreatePodRequest{PodId: id.String(), Spec: spec}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	process := func(k store.TombstoneKey) error {
		ctx, cancel := context.WithTimeout(bg, 30*time.Second)
		defer cancel()
		return e.processTombstone(k).wait(ctx)
	}
	exists := func(k store.TombstoneKey) bool {
		_, err := h.st.GetTombstone(bg, k)
		return err == nil
	}

	// The tombstone's incarnation: the pod is deleted, then the tombstone.
	inc := uuid.New()
	id := podid.Mint()
	if _, err := pc.CreatePod(bg, &podv1.CreatePodRequest{PodId: id.String(), Spec: labelled(id, inc)}); err != nil {
		t.Fatal(err)
	}
	k := h.tombstone(id, "n1", inc, time.Time{})
	if err := process(k); err != nil || exists(k) {
		t.Fatalf("process: %v (tombstone kept %v)", err, exists(k))
	}
	h.noPod("n1", id)

	// Another incarnation: the pod stays, only the tombstone goes.
	other := uuid.New()
	id2 := podid.Mint()
	if _, err := pc.CreatePod(bg, &podv1.CreatePodRequest{PodId: id2.String(), Spec: labelled(id2, other)}); err != nil {
		t.Fatal(err)
	}
	k = h.tombstone(id2, "n1", uuid.New(), time.Time{})
	if err := process(k); err != nil || exists(k) {
		t.Fatalf("process: %v", err)
	}
	h.podState("n1", id2, podv1.PodState_POD_STATE_RUNNING)

	// A pod of ferund's without an incarnation label is deleted.
	id3 := podid.Mint()
	spec := labelled(id3, uuid.New())
	delete(spec.Labels, LabelIncarnation)
	if _, err := pc.CreatePod(bg, &podv1.CreatePodRequest{PodId: id3.String(), Spec: spec}); err != nil {
		t.Fatal(err)
	}
	k = h.tombstone(id3, "n1", uuid.New(), time.Time{})
	if err := process(k); err != nil {
		t.Fatal(err)
	}
	h.noPod("n1", id3)

	// A pod that is not ferund's is never touched.
	id4 := create(&podv1.PodSpec{Containers: []*podv1.ContainerSpec{{Name: "main", ImageRef: testImage}},
		Network: &podv1.PodNetwork{None: true}})
	k = h.tombstone(id4, "n1", uuid.Nil, time.Time{})
	if err := process(k); err != nil || exists(k) {
		t.Fatalf("process: %v", err)
	}
	h.podState("n1", id4, podv1.PodState_POD_STATE_RUNNING)

	// No pod: the tombstone goes.
	k = h.tombstone(podid.Mint(), "n1", uuid.New(), time.Time{})
	if err := process(k); err != nil || exists(k) {
		t.Fatalf("process: %v", err)
	}

	// Not due: nothing happens.
	id5 := podid.Mint()
	if _, err := pc.CreatePod(bg, &podv1.CreatePodRequest{PodId: id5.String(), Spec: labelled(id5, inc)}); err != nil {
		t.Fatal(err)
	}
	k = h.tombstone(id5, "n1", inc, time.Now().Add(time.Hour))
	if err := process(k); !errors.Is(err, errNotDue) || !exists(k) {
		t.Fatalf("a tombstone not due: %v", err)
	}
	h.podState("n1", id5, podv1.PodState_POD_STATE_RUNNING)

	// A DeletePod that fails: an attempt is counted, the tombstone stays.
	h.exec("UPDATE pod_tombstones SET not_before = now() WHERE pod_id = $1", id5)
	h.node("n1").Script().SetHook(podv1.PodService_DeletePod_FullMethodName, func(context.Context, string, any) error {
		return status.Error(codes.Internal, "diskd is busy")
	})
	if err := process(k); err == nil {
		t.Fatal("a failed DeletePod succeeded")
	}
	ts, err := h.st.GetTombstone(bg, k)
	if err != nil || ts.Attempts != 1 || ts.LastError == "" || ts.LastAttemptAt.IsZero() {
		t.Fatalf("tombstone after a failure %+v %v", ts, err)
	}
	// The leader's loop processes it once the node answers.
	h.node("n1").Script().SetHook(podv1.PodService_DeletePod_FullMethodName, nil)
	if err := e.tombstones(bg); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the loop processed the tombstone", func() bool { return !exists(k) })
	h.noPod("n1", id5)
}

func TestTombstoneProcessingIsDetached(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1", func(c *testnode.Config) { c.AbortOnCancel = true })
	e := h.eng()
	inc := uuid.New()
	id := podid.Mint()
	if _, err := h.pods("n1").CreatePod(bg, &podv1.CreatePodRequest{PodId: id.String(), Spec: labelled(id, inc)}); err != nil {
		t.Fatal(err)
	}
	h.podState("n1", id, podv1.PodState_POD_STATE_RUNNING)
	n.Script().SetDelay(podv1.PodService_DeletePod_FullMethodName, time.Second)
	k := h.tombstone(id, "n1", inc, time.Time{})
	// The caller gives up early: its wait ends, the DeletePod does not
	// (the node aborts a cancelled DeletePod halfway).
	ctx, cancel := context.WithTimeout(bg, 100*time.Millisecond)
	err := e.processTombstone(k).wait(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait: %v", err)
	}
	// A second caller joins the same run.
	if r1, r2 := e.processTombstone(k), e.processTombstone(k); r1 != r2 {
		t.Fatal("two runs of one tombstone")
	}
	h.noPod("n1", id)
	eventually(t, 10*time.Second, "the tombstone gone", func() bool { return len(h.tombstones(id)) == 0 })
}

func TestTombstonesOfRemovedNodes(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	h.exec("INSERT INTO nodes (name, status, removed_at) VALUES ('old', 'removed', now())")
	id := podid.Mint()
	h.tombstone(id, "old", uuid.New(), time.Time{})
	if err := e.tombstones(bg); err != nil {
		t.Fatal(err)
	}
	if ts := h.tombstones(id); len(ts) != 0 {
		t.Fatalf("tombstones of a removed node %+v", ts)
	}
	eventually(t, 10*time.Second, "the drop recorded", func() bool {
		var c int
		h.scalar(&c, "SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'tombstone' AND name LIKE '%removed'", id)
		return c == 1
	})
}

func TestDeleteThenCreateSameID(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	sb := h.running(512)
	h.settle(sb.ID)
	if err := e.HoldProcess(bg, sb.ID, "p", 0); err != nil {
		t.Fatal(err)
	}
	// Hold the old pod's deletion, so that the new incarnation meets its
	// tombstone.
	gate := make(chan struct{})
	h.node("n1").Script().SetHook(podv1.PodService_DeletePod_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return nil
	})
	row, tombstoned, err := e.Delete(bg, testTenant, sb.ID)
	if err != nil || !tombstoned || row.ClientState != model.StateRunning {
		t.Fatalf("delete: %+v %v %v", row, tombstoned, err)
	}
	if _, err := h.st.GetSandbox(bg, sb.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("the row stayed: %v", err)
	}
	ts := h.tombstones(sb.ID)
	if len(ts) != 1 || ts[0].Incarnation != sb.Incarnation || ts[0].Reason != "deleted" {
		t.Fatalf("tombstones %+v", ts)
	}
	// An unknown sandbox is not found.
	_, _, err = e.Delete(bg, testTenant, podid.Mint())
	wantErr(t, err, model.ErrNotFound, "unknown sandbox")

	// Created again at once: a new incarnation, kept off n1 until the
	// tombstone is done, never given the old pod.
	again := h.sandboxWithID(sb.ID, 512)
	if again.Incarnation == sb.Incarnation {
		t.Fatal("the same incarnation again")
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.Ensure(bg, sb.ID)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if row := h.row(sb.ID); row.ClientState != model.StatePending {
		t.Fatalf("placed while the old pod is there: %+v", row)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	info := h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
	if podIncarnation(info) != again.Incarnation {
		t.Fatal("the new sandbox runs the old pod")
	}
	if len(h.tombstones(sb.ID)) != 0 {
		t.Fatal("the tombstone stayed")
	}
	// The new sandbox has no process holds of the old one.
	if hs := h.holds(sb.ID); len(hs) != 0 {
		t.Fatalf("holds %+v", hs)
	}
}

func TestDeleteTenant(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	a := h.running(512)
	b := h.sandbox(512)
	// Another tenant's request does not find the sandbox.
	_, _, err := e.Delete(bg, "someone-else", a.ID)
	wantErr(t, err, model.ErrNotFound, "unknown sandbox "+a.ID.String())
	existed, err := e.DeleteTenant(bg, testTenant)
	if err != nil || !existed {
		t.Fatalf("delete tenant: %v %v", existed, err)
	}
	for _, id := range []podid.ID{a.ID, b.ID} {
		if _, err := h.st.GetSandbox(bg, id); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("sandbox %s stayed: %v", id, err)
		}
	}
	// The tenant stays while it has environments, which the
	// environments' controller deletes; the next pass then deletes it.
	if tn, err := h.st.GetTenant(bg, testTenant); err != nil || !tn.Deleting() {
		t.Fatalf("tenant %+v %v", tn, err)
	}
	h.exec("DELETE FROM environments WHERE owner = $1", testTenant)
	if err := e.tenantDeletion(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetTenant(bg, testTenant); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("tenant stayed: %v", err)
	}
	h.noPod("n1", a.ID)
	existed, err = e.DeleteTenant(bg, "nobody")
	if err != nil || existed {
		t.Fatalf("unknown tenant: %v %v", existed, err)
	}

	// The leader's loop finishes a deletion that a request began.
	if _, _, err := h.st.CreateTenant(bg, "t2"); err != nil {
		t.Fatal(err)
	}
	c, _, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: podid.Mint(), Owner: "t2", Image: testImage, VCPUs: 1, MemoryMiB: 512,
		EnvironmentID: h.boxEnv("t2")})
	if err != nil {
		t.Fatal(err)
	}
	h.exec("UPDATE tenants SET deleting_at = now() WHERE id = 't2'")
	if err := e.tenantDeletion(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetSandbox(bg, c.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("the loop left a sandbox: %v", err)
	}
	h.exec("DELETE FROM environments WHERE owner = 't2'")
	if err := e.tenantDeletion(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetTenant(bg, "t2"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("the loop left the tenant: %v", err)
	}
}
