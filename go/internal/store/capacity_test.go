// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

func TestBookingSums(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "acme")
	mkNode(t, s, "n1")
	mkNode(t, s, "n2")
	a := mkSandbox(t, s, "acme")
	b := mkSandbox(t, s, "acme")
	c := mkSandbox(t, s, "acme")
	must(t, s.UpsertBooking(bg, store.Booking{PodID: a.ID, Node: "n1", VCPUs: 2, MemoryMiB: 512, Gated: true}))
	must(t, s.UpsertBooking(bg, store.Booking{PodID: b.ID, Node: "n1", VCPUs: 4, MemoryMiB: 1024, Gated: true}))
	must(t, s.UpsertBooking(bg, store.Booking{PodID: c.ID, Node: "n2", VCPUs: 1, MemoryMiB: 256, Gated: false}))

	sums := func(node string, exclude podid.ID) store.BookingSums {
		t.Helper()
		got, err := s.SumBookings(bg, node, exclude)
		must(t, err)
		return got
	}
	if got := sums("n1", ""); got != (store.BookingSums{Count: 2, VCPUs: 6, MemoryMiB: 1536}) {
		t.Fatalf("n1: %+v", got)
	}
	// An admission of a rechecks against the others' bookings only.
	if got := sums("n1", a.ID); got != (store.BookingSums{Count: 1, VCPUs: 4, MemoryMiB: 1024}) {
		t.Fatalf("n1 without a: %+v", got)
	}
	if got := sums("n3", ""); got != (store.BookingSums{}) {
		t.Fatalf("empty node: %+v", got)
	}
	all, err := s.SumAllBookings(bg, b.ID)
	must(t, err)
	if all["n1"] != (store.BookingSums{Count: 1, VCPUs: 2, MemoryMiB: 512}) || all["n2"].Count != 1 {
		t.Fatalf("all without b: %+v", all)
	}

	// A booking moves with its pod and replaces the old one.
	must(t, s.UpsertBooking(bg, store.Booking{PodID: b.ID, Node: "n2", VCPUs: 4, MemoryMiB: 1024, Gated: false}))
	if got := sums("n1", ""); got.Count != 1 {
		t.Fatalf("n1 after the move: %+v", got)
	}
	bk, err := s.GetBooking(bg, b.ID)
	if err != nil || bk.Node != "n2" || bk.Gated {
		t.Fatalf("moved booking %+v %v", bk, err)
	}
	if l, err := s.ListBookings(bg, "n2"); err != nil || len(l) != 2 {
		t.Fatalf("bookings of n2: %v %v", l, err)
	}
	// Deleting the sandbox frees its booking; a booking needs a sandbox.
	_, err = s.DeleteSandbox(bg, c.ID)
	must(t, err)
	if _, err := s.GetBooking(bg, c.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("booking of a deleted sandbox: %v", err)
	}
	if err := s.UpsertBooking(bg, store.Booking{PodID: c.ID, Node: "n1", VCPUs: 1, MemoryMiB: 256}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("booking without a sandbox: %v", err)
	}
	if ok, err := s.DeleteBooking(bg, a.ID); err != nil || !ok {
		t.Fatalf("delete booking: %v %v", ok, err)
	}
}

func TestPlacementSnapshot(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "acme")
	mkNode(t, s, "n1")
	mkNode(t, s, "n2")
	mkNode(t, s, "n3")
	_, err := s.RemoveNode(bg, "n3")
	must(t, err)
	pod := mkSandboxID(t, s, "acme", "abcdef01-0000-4000-8000-000000000001")
	tap := mkSandboxID(t, s, "acme", "abcdef01-2222-4000-8000-000000000002")
	other := mkSandbox(t, s, "acme")
	place(t, s, tap.ID, "n1", model.StateRunning)
	must(t, s.UpsertBooking(bg, store.Booking{PodID: tap.ID, Node: "n1", VCPUs: 2, MemoryMiB: 512, Gated: true}))
	must(t, s.UpsertBooking(bg, store.Booking{PodID: other.ID, Node: "n1", VCPUs: 4, MemoryMiB: 2048, Gated: true}))
	// The pod's own booking is not counted against it.
	must(t, s.UpsertBooking(bg, store.Booking{PodID: pod.ID, Node: "n1", VCPUs: 2, MemoryMiB: 512, Gated: true}))
	_, err = s.UpsertTombstone(bg, store.Tombstone{PodID: pod.ID, Node: "n2", Incarnation: uuid.New(), Reason: "deleted"})
	must(t, err)
	_, err = s.UpsertTelemetry(bg, "n1", store.TelemetryReport{Load1: 0.5, MemoryAvailableBytes: 1 << 30, DiskAvailableBytes: 5 << 30})
	must(t, err)

	snap, err := s.PlacementSnapshot(bg, pod.ID)
	must(t, err)
	if len(snap) != 2 || snap[0].Node.Name != "n1" || snap[1].Node.Name != "n2" {
		t.Fatalf("snapshot nodes %+v", snap)
	}
	n1, n2 := snap[0], snap[1]
	if !n1.HasTelemetry || n1.Telemetry.MemoryAvailableBytes != 1<<30 || n1.Telemetry.Load1 != 0.5 ||
		n1.Booked != (store.BookingSums{Count: 2, VCPUs: 6, MemoryMiB: 2560}) || n1.Tombstoned || n1.TAPConflict != tap.ID {
		t.Fatalf("n1 %+v", n1)
	}
	if n2.HasTelemetry || n2.Booked.Count != 0 || !n2.Tombstoned || !n2.TAPConflict.IsZero() {
		t.Fatalf("n2 %+v", n2)
	}
	if n1.Node.VCPUCapacity() != 128 || n1.Node.MemoryCapacityMiB() != 31*1024 {
		t.Fatalf("capacity %d vCPUs, %d MiB", n1.Node.VCPUCapacity(), n1.Node.MemoryCapacityMiB())
	}
}

func TestTombstones(t *testing.T) {
	s, _ := newStore(t)
	mkNode(t, s, "n1")
	mkNode(t, s, "n2")
	pod := podid.Mint()
	inc := uuid.New()
	ts, err := s.UpsertTombstone(bg, store.Tombstone{PodID: pod, Node: "n1", Incarnation: inc, Reason: "deleted"})
	must(t, err)
	if ts.NotBefore.IsZero() || ts.Reason != "deleted" || ts.Attempts != 0 {
		t.Fatalf("tombstone %+v", ts)
	}
	later, err := s.Now(bg)
	must(t, err)
	_, err = s.UpsertTombstone(bg, store.Tombstone{PodID: pod, Node: "n2", Incarnation: uuid.Nil, Reason: "orphan",
		NotBefore: later.Add(time.Hour)})
	must(t, err)

	// The gate: nodes the pod's ID is kept off.
	nodes, err := s.TombstonedNodes(bg, pod)
	if err != nil || len(nodes) != 2 || nodes[0] != "n1" || nodes[1] != "n2" {
		t.Fatalf("gate %v %v", nodes, err)
	}
	if ok, err := s.HasTombstone(bg, pod, "n2"); err != nil || !ok {
		t.Fatalf("HasTombstone n2: %v %v", ok, err)
	}
	if ok, _ := s.HasTombstone(bg, podid.Mint(), "n1"); ok {
		t.Fatal("HasTombstone of another pod")
	}
	// Only the one past its not_before is due.
	due, err := s.DueTombstones(bg, []string{"n1", "n2"}, 10)
	if err != nil || len(due) != 1 || due[0].Node != "n1" {
		t.Fatalf("due %v %v", due, err)
	}
	if due, _ := s.DueTombstones(bg, []string{"n2"}, 10); len(due) != 0 {
		t.Fatalf("due on n2: %v", due)
	}
	// A refresh moves not_before and keeps the reason.
	k := store.TombstoneKey{PodID: pod, Node: "n2", Incarnation: uuid.Nil}
	_, err = s.UpsertTombstone(bg, store.Tombstone{PodID: pod, Node: "n2", Incarnation: uuid.Nil, Reason: "fenced out"})
	must(t, err)
	got, err := s.GetTombstone(bg, k)
	if err != nil || got.Reason != "orphan" || got.NotBefore.After(later.Add(time.Minute)) {
		t.Fatalf("refreshed %+v %v", got, err)
	}
	must(t, s.NoteTombstoneAttempt(bg, k, "node said no"))
	got, _ = s.GetTombstone(bg, k)
	if got.Attempts != 1 || got.LastError != "node said no" || got.LastAttemptAt.IsZero() {
		t.Fatalf("attempt %+v", got)
	}
	if all, err := s.TombstonesOfPod(bg, pod); err != nil || len(all) != 2 {
		t.Fatalf("of pod %v %v", all, err)
	}
	if ok, err := s.DeleteTombstone(bg, k); err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
	if _, err := s.GetTombstone(bg, k); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("get deleted: %v", err)
	}
	// Tombstones of a removed node are dropped.
	_, err = s.RemoveNode(bg, "n1")
	must(t, err)
	dropped, err := s.DropRemovedNodeTombstones(bg, 64)
	if err != nil || len(dropped) != 1 || dropped[0].Node != "n1" {
		t.Fatalf("dropped %v %v", dropped, err)
	}
}

func TestHolds(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "acme")
	mkNode(t, s, "n1")
	sb := mkSandbox(t, s, "acme")
	place(t, s, sb.ID, "n1", model.StateRunning)
	exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '1 hour' WHERE id = $1", sb.ID)
	old := get(t, s, sb.ID).LastActiveAt

	// 0 → 1: the hold and last_active_at in one go.
	tx(t, s, func(tx *store.Store) error {
		if _, err := tx.LockSandbox(bg, sb.ID, store.ForNoKeyUpdate); err != nil {
			return err
		}
		return tx.AdmitHold(bg, sb.ID, model.HoldExec, "i1", 30*time.Second)
	})
	if !get(t, s, sb.ID).LastActiveAt.After(old) {
		t.Fatal("admission did not touch last_active_at")
	}
	live := func(kinds ...model.HoldKind) bool {
		t.Helper()
		ok, err := s.HasLiveHold(bg, sb.ID, kinds...)
		must(t, err)
		return ok
	}
	if !live() || !live(model.HoldExec, model.HoldForward) || live(model.HoldProcess) {
		t.Fatal("live holds after admission")
	}
	holds, err := s.ListHolds(bg, sb.ID)
	must(t, err)
	if len(holds) != 1 || holds[0].Holder != "i1:exec" || holds[0].Kind != model.HoldExec || holds[0].InstanceID != "i1" || holds[0].Infinite {
		t.Fatalf("holds %+v", holds)
	}

	// An expired hold is deleted by the sweep; a renewal re-inserts it.
	must(t, s.RenewHold(bg, sb.ID, model.HoldExec, "i1", time.Microsecond))
	time.Sleep(5 * time.Millisecond)
	if live() {
		t.Fatal("an expired hold is live")
	}
	if n, err := s.DeleteExpiredHolds(bg); err != nil || n != 1 {
		t.Fatalf("expired: %d %v", n, err)
	}
	must(t, s.RenewHold(bg, sb.ID, model.HoldExec, "i1", 30*time.Second))
	if !live(model.HoldExec) {
		t.Fatal("renewal did not re-insert the hold")
	}

	// Process holds: without a limit, or with one.
	must(t, s.HoldProcess(bg, sb.ID, "p1", 0))
	must(t, s.HoldProcess(bg, sb.ID, "p2", time.Microsecond))
	time.Sleep(5 * time.Millisecond)
	if n, err := s.DeleteExpiredHolds(bg); err != nil || n != 1 {
		t.Fatalf("expired process holds: %d %v", n, err)
	}
	holds, _ = s.ListHolds(bg, sb.ID)
	if len(holds) != 2 || holds[1].Holder != "process:p1" || !holds[1].Infinite {
		t.Fatalf("holds %+v", holds)
	}
	if id, ok := store.ProcessIDOf(holds[1].Holder); !ok || id != "p1" {
		t.Fatalf("process id %q %v", id, ok)
	}

	// 1 → 0: the hold goes and last_active_at moves.
	exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '1 hour' WHERE id = $1", sb.ID)
	tx(t, s, func(tx *store.Store) error { return tx.ReleaseHold(bg, sb.ID, model.HoldExec, "i1") })
	if live(model.HoldExec) || get(t, s, sb.ID).LastActiveAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("release")
	}

	// Only process holds left, idle: a candidate of the process-holds loop.
	exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '1 hour' WHERE id = $1", sb.ID)
	held, err := s.ProcessHeldSandboxes(bg, time.Minute, []string{"n1"}, 10)
	if err != nil || len(held) != 1 || held[0].Sandbox.ID != sb.ID || len(held[0].ProcessIDs) != 1 || held[0].ProcessIDs[0] != "p1" {
		t.Fatalf("process held %+v %v", held, err)
	}
	must(t, s.AdmitHold(bg, sb.ID, model.HoldForward, "i2", time.Minute))
	exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '1 hour' WHERE id = $1", sb.ID)
	if held, _ := s.ProcessHeldSandboxes(bg, time.Minute, []string{"n1"}, 10); len(held) != 0 {
		t.Fatal("a sandbox with a forward hold is a process-holds candidate")
	}

	if ok, err := s.ReleaseProcessHold(bg, sb.ID, "p1"); err != nil || !ok {
		t.Fatalf("release process: %v %v", ok, err)
	}
	must(t, s.HoldProcess(bg, sb.ID, "p3", time.Hour))
	if n, err := s.DeleteProcessHolds(bg, sb.ID); err != nil || n != 1 {
		t.Fatalf("delete process holds: %d %v", n, err)
	}
	if err := s.AdmitHold(bg, podid.Mint(), model.HoldExec, "i1", time.Minute); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("hold of a missing sandbox: %v", err)
	}
}
