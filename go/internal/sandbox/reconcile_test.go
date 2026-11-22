// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// labelled is a pod spec of ferund's for id of incarnation inc.
func labelled(id podid.ID, inc uuid.UUID) *podv1.PodSpec {
	return podSpec(store.Sandbox{ID: id, Owner: testProject, Incarnation: inc, VCPUs: 1, MemoryMiB: 256, ImageRef: testImage})
}

func TestReconcileOrphansAndIgnored(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	h.addNode("n2")
	e := h.eng()
	// Hold DeletePod on n1, so that the tombstones are seen before they
	// are processed.
	gate := make(chan struct{})
	h.node("n1").Script().SetHook(podv1.PodService_DeletePod_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return nil
	})
	pc := h.pods("n1")
	// An orphan: ferund's label, no row.
	// A pod made before ADR 0019, with a UUID.
	orphan := legacyID()
	orphanInc := uuid.New()
	if _, err := pc.CreatePod(bg, &podv1.CreatePodRequest{PodId: orphan.String(), Spec: labelled(orphan, orphanInc)}); err != nil {
		t.Fatal(err)
	}
	// A pod not of ferund's: never touched.
	foreign := podid.Mint()
	if _, err := pc.CreatePod(bg, &podv1.CreatePodRequest{PodId: foreign.String(), Spec: &podv1.PodSpec{
		Containers: []*podv1.ContainerSpec{{Name: "main", ImageRef: testImage}}, Network: &podv1.PodNetwork{None: true},
	}}); err != nil {
		t.Fatal(err)
	}
	// A stale record: a row of another incarnation on this node without
	// an op.
	stale := h.running(512)
	h.settle(stale.ID)
	staleInc := uuid.New()
	h.exec("UPDATE sandboxes SET incarnation = $2 WHERE id = $1", stale.ID, staleInc)
	// A pod whose sandbox is placed on another node.
	elsewhere := h.sandbox(512)
	h.exec("UPDATE sandboxes SET node = 'n2', client_state = 'stopped', materialised = true WHERE id = $1", elsewhere.ID)
	if _, err := pc.CreatePod(bg, &podv1.CreatePodRequest{PodId: elsewhere.ID.String(), Spec: labelled(elsewhere.ID, elsewhere.Incarnation)}); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileNode(bg, "n1"); err != nil {
		t.Fatal(err)
	}
	want := map[podid.ID]uuid.UUID{orphan: orphanInc, stale.ID: stale.Incarnation, elsewhere.ID: elsewhere.Incarnation}
	for id, inc := range want {
		ts := h.tombstones(id)
		if len(ts) != 1 || ts[0].Node != "n1" || ts[0].Incarnation != inc {
			t.Fatalf("tombstones of %s %+v", id, ts)
		}
	}
	if ts := h.tombstones(foreign); len(ts) != 0 {
		t.Fatalf("a pod without ferund's label got a tombstone %+v", ts)
	}
	// The stale row itself is not touched by the orphan of its id.
	if row := h.row(stale.ID); row.ClientState != model.StateRunning {
		t.Fatalf("stale row %+v", row)
	}
	close(gate)
	for id := range want {
		h.noPod("n1", id)
	}
	h.podState("n1", foreign, podv1.PodState_POD_STATE_RUNNING)
}

func TestReconcileSnapshot(t *testing.T) {
	h := newHarness(t)
	n := h.addNode("n1")
	e := h.eng()
	// A sandbox whose create begins after the snapshot and whose pod the
	// list lacks: the reconcile leaves it alone.
	listing := make(chan struct{})
	begun := make(chan struct{})
	n.Script().SetHook(podv1.PodService_ListPods_FullMethodName, func(context.Context, string, any) error {
		close(listing)
		<-begun
		return nil
	})
	listed := make(chan struct{})
	n.Script().SetHook(podv1.PodService_CreatePod_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		select {
		case <-listed:
		case <-ctx.Done():
		}
		return nil
	})
	sb := h.sandbox(512)
	recon := make(chan error, 1)
	go func() { recon <- e.ReconcileNode(bg, "n1") }()
	<-listing
	ensured := make(chan error, 1)
	go func() {
		_, err := e.Ensure(bg, sb.ID)
		ensured <- err
	}()
	eventually(t, 10*time.Second, "the create began", func() bool { return h.row(sb.ID).Op == string(OpCreate) })
	close(begun)
	if err := <-recon; err != nil {
		t.Fatal(err)
	}
	if row := h.row(sb.ID); row.ClientState == model.StateLost || row.Op != string(OpCreate) {
		t.Fatalf("the reconcile touched a sandbox created during ListPods: %+v", row)
	}
	close(listed)
	if err := <-ensured; err != nil {
		t.Fatal(err)
	}

	// A row of the snapshot whose pod is gone: NOT_FOUND (lost), its
	// process holds end.
	h.settle(sb.ID)
	if err := e.HoldProcess(bg, sb.ID, "p", 0); err != nil {
		t.Fatal(err)
	}
	n.Script().SetHook(podv1.PodService_ListPods_FullMethodName, nil)
	// A row that says its pod runs on the node, which does not have it:
	// taken unchanged into the snapshot, lacked by the list.
	ghost := h.sandbox(512)
	h.exec(`UPDATE sandboxes SET node = 'n1', client_state = 'running', materialised = true, gen = gen + 1 WHERE id = $1`, ghost.ID)
	if err := e.HoldProcess(bg, ghost.ID, "p", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileNode(bg, "n1"); err != nil {
		t.Fatal(err)
	}
	row := h.row(ghost.ID)
	wantState(t, row, model.StateLost)
	if row.StatusDescription != "not on node n1" || row.Node != "n1" {
		t.Fatalf("lost row %+v", row)
	}
	if hs := h.holds(ghost.ID); len(hs) != 0 {
		t.Fatalf("process holds of a lost sandbox %+v", hs)
	}
	if hs := h.holds(sb.ID); len(hs) != 1 {
		t.Fatalf("process holds of a running sandbox %+v", hs)
	}
}

func TestReconcileAdoptsAfterLostGrace(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LostGrace = time.Minute })
	n := h.addNode("n1")
	e := h.eng()
	sb := h.running(1024)
	h.settle(sb.ID)
	if err := e.HoldProcess(bg, sb.ID, "p", 0); err != nil {
		t.Fatal(err)
	}
	// The node is cut off, expires, and stays down past the lost grace.
	n.Partition(true)
	eventually(t, 10*time.Second, "n1 disconnected", func() bool { return !h.ins[0].srv.Connected("n1") })
	h.exec(`UPDATE nodes SET status = 'down', down_since = now() - interval '2 minutes',
		status_description = 'heartbeat expired' WHERE name = 'n1'`)
	if err := e.downSweep(bg); err != nil {
		t.Fatal(err)
	}
	row := h.row(sb.ID)
	wantState(t, row, model.StateUnknown)
	if row.StatusDescription == "" || row.Node != "n1" {
		t.Fatalf("swept row %+v", row)
	}
	if _, ok := h.booking(sb.ID); !ok {
		t.Fatal("the down sweep took the booking")
	}
	if err := e.lostGrace(bg); err != nil {
		t.Fatal(err)
	}
	row = h.row(sb.ID)
	wantState(t, row, model.StateLost)
	if row.LostFrom != "n1" || row.Node != "n1" || row.RunID != "" {
		t.Fatalf("lost row %+v", row)
	}
	if _, ok := h.booking(sb.ID); ok {
		t.Fatal("a lost sandbox is booked")
	}
	if hs := h.holds(sb.ID); len(hs) != 0 {
		t.Fatalf("process holds after lost grace %+v", hs)
	}
	if ts := h.tombstones(sb.ID); len(ts) != 0 {
		t.Fatalf("lost grace wrote tombstones %+v", ts)
	}
	// Meanwhile the node filled up: the pod it still runs is booked all
	// the same when it comes back (reality wins over the books).
	h.exec("UPDATE nodes SET slots = 0 WHERE name = 'n1'")
	n.Partition(false)
	eventually(t, 20*time.Second, "n1 back and ready", func() bool {
		nd, err := h.st.GetNode(bg, "n1")
		return h.ins[0].srv.Connected("n1") && err == nil && nd.Status == model.NodeReady
	})
	if err := e.ReconcileNode(bg, "n1"); err != nil {
		t.Fatal(err)
	}
	row = h.row(sb.ID)
	wantState(t, row, model.StateRunning)
	if row.LostFrom != "" || row.RunID == "" {
		t.Fatalf("adopted row %+v", row)
	}
	b, ok := h.booking(sb.ID)
	if !ok || b.Gated || b.Node != "n1" {
		t.Fatalf("booking of the adopted pod %+v %v", b, ok)
	}
	eventually(t, 10*time.Second, "the overbooking recorded", func() bool {
		var c int
		h.scalar(&c, "SELECT count(*) FROM activity WHERE sandbox_id = $1 AND kind = 'overbooked'", sb.ID)
		return c == 1
	})
	h.podState("n1", sb.ID, podv1.PodState_POD_STATE_RUNNING)
}

func TestReconciledHashAndRegistryHash(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1", func(c *testnode.Config) { c.PendingDelay = 10 * time.Millisecond })
	e := h.eng()
	empty := sha256.Sum256(nil)
	if got, err := e.RegistryHash(bg, "n1"); err != nil || got != hex.EncodeToString(empty[:]) {
		t.Fatalf("hash of an empty node %q %v", got, err)
	}
	h.running(512)
	b := h.running(512)
	h.startOp(b.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	got, err := e.RegistryHash(bg, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if want := testnode.InventoryHash(h.node("n1").Pods()); got != want {
		t.Fatalf("registry hash %s, node's %s", got, want)
	}
	// The node's last report is what the reconcile records.
	eventually(t, 15*time.Second, "the node reported its pods", func() bool {
		var reported string
		h.scalar(&reported, "SELECT inventory_hash FROM node_telemetry WHERE node = 'n1'")
		return reported == got
	})
	if err := e.ReconcileNode(bg, "n1"); err != nil {
		t.Fatal(err)
	}
	var seen string
	h.scalar(&seen, "SELECT reconciled_hash FROM node_telemetry WHERE node = 'n1'")
	if seen != got {
		t.Fatalf("reconciled_hash %q, want %q", seen, got)
	}
	// A reconcile needs the node's connection.
	h.exec("INSERT INTO nodes (name, status) VALUES ('far', 'ready')")
	wantErr(t, e.ReconcileNode(bg, "far"), model.ErrNodeUnavailable, "")
}
