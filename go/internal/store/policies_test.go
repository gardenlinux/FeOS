// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

func ids(sbs []store.Sandbox) []podid.ID {
	out := make([]podid.ID, len(sbs))
	for i, sb := range sbs {
		out[i] = sb.ID
	}
	return out
}

// The down sweep moves what its down node held to unknown, leaves rows
// with an op to the op, and keeps bookings; lost grace then resolves them.
func TestDownSweepAndLostGrace(t *testing.T) {
	s, d := newStore(t)
	mkProject(t, s, "acme")
	mkNode(t, s, "n1")
	mkNode(t, s, "n2")
	mkNode(t, s, "n3")
	mkNode(t, s, "n4")
	running := mkSandbox(t, s, "acme")
	paused := mkSandbox(t, s, "acme")
	withOp := mkSandbox(t, s, "acme")
	hibernated := mkSandbox(t, s, "acme")
	elsewhere := mkSandbox(t, s, "acme")
	onRemoved := mkSandbox(t, s, "acme")
	onDownRemoved := mkSandbox(t, s, "acme")
	neverSeen := mkSandbox(t, s, "acme")
	place(t, s, running.ID, "n1", model.StateRunning)
	place(t, s, paused.ID, "n1", model.StatePaused)
	place(t, s, withOp.ID, "n1", model.StateResuming)
	startOp(t, s, withOp.ID, "resume", "i1", "n1", time.Minute)
	place(t, s, hibernated.ID, "n1", model.StateHibernated)
	place(t, s, elsewhere.ID, "n2", model.StateRunning)
	place(t, s, onRemoved.ID, "n3", model.StateRunning)
	place(t, s, onDownRemoved.ID, "n4", model.StatePaused)
	// Each of these had its pod seen on its node; neverSeen's first create
	// is under way there.
	exec(t, s, "UPDATE sandboxes SET materialised = true WHERE id <> $1", neverSeen.ID)
	place(t, s, neverSeen.ID, "n1", model.StateCreating)
	must(t, s.UpsertBooking(bg, store.Booking{PodID: neverSeen.ID, Node: "n1", VCPUs: 2, MemoryMiB: 512, Gated: true}))
	exec(t, s, `UPDATE sandboxes SET checkpoint_head = 'head-4', checkpoint_epoch = 1,
		checkpoint_at = '2026-01-01T00:00:00Z' WHERE id = $1`, onDownRemoved.ID)
	exec(t, s, "UPDATE sandboxes SET checkpoint_head = 'head-1', checkpoint_epoch = 1, checkpoint_at = now() WHERE id = $1", paused.ID)
	must(t, s.UpsertBooking(bg, store.Booking{PodID: running.ID, Node: "n1", VCPUs: 2, MemoryMiB: 512, Gated: true}))
	must(t, s.HoldProcess(bg, running.ID, "p1", 0))
	exec(t, s, "UPDATE nodes SET status = 'down', down_since = now() WHERE name = 'n1'")
	// n3 is removed while ready, n4 after it was down: the descriptions
	// name the time each went out of service.
	n3, err := s.RemoveNode(bg, "n3")
	must(t, err)
	n3Since := n3.RemovedAt.UTC().Format("2006-01-02T15:04:05Z")
	exec(t, s, "UPDATE nodes SET status = 'down', down_since = '2026-01-02T03:04:05Z' WHERE name = 'n4'")
	_, err = s.RemoveNode(bg, "n4")
	must(t, err)
	ch, unsubscribe := d.Subscribe(db.ChanSandbox)
	defer unsubscribe()

	var swept []podid.ID
	tx(t, s, func(tx *store.Store) (err error) {
		swept, err = tx.DownSweep(bg, 256)
		return err
	})
	want := []podid.ID{running.ID, paused.ID, onRemoved.ID, onDownRemoved.ID, neverSeen.ID}
	slices.SortFunc(want, func(a, b podid.ID) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(swept, want) {
		t.Fatalf("swept %v, want %v", swept, want)
	}
	for _, id := range want {
		expectPayload(t, ch, id.String())
	}
	r := get(t, s, running.ID)
	if r.ClientState != model.StateUnknown || r.Gen != running.Gen+1 || r.Node != "n1" ||
		!strings.HasPrefix(r.StatusDescription, "node n1 down since ") || !strings.HasSuffix(r.StatusDescription, "; it was running") {
		t.Fatalf("swept row %+v", r)
	}
	if d, want := get(t, s, onRemoved.ID).StatusDescription, "node n3 down since "+n3Since+"; it was running"; d != want {
		t.Fatalf("removed description %q, want %q", d, want)
	}
	if d, want := get(t, s, onDownRemoved.ID).StatusDescription, "node n4 down since 2026-01-02T03:04:05Z; it was paused"; d != want {
		t.Fatalf("down-then-removed description %q, want %q", d, want)
	}
	if _, err := s.GetBooking(bg, running.ID); err != nil {
		t.Fatal("the sweep removed a booking")
	}
	if get(t, s, withOp.ID).ClientState != model.StateResuming || get(t, s, hibernated.ID).ClientState != model.StateHibernated {
		t.Fatal("the sweep touched a row with an op or a hibernated one")
	}
	// Level-triggered: the next pass finds nothing new.
	if again, err := s.DownSweep(bg, 256); err != nil || len(again) != 0 {
		t.Fatalf("second sweep %v %v", again, err)
	}

	// Lost grace: not yet for n1 (down for less than the grace), at once
	// for the removed n3 and n4.
	lost, err := s.LostGrace(bg, time.Hour, 256)
	if err != nil || len(lost) != 2 {
		t.Fatalf("lost grace %+v %v", lost, err)
	}
	for _, l := range lost {
		if !(l.ID == onRemoved.ID && l.State == model.StateLost) && !(l.ID == onDownRemoved.ID && l.State == model.StateHibernated) {
			t.Fatalf("lost grace on removed nodes %+v", l)
		}
	}
	if d, want := get(t, s, onRemoved.ID).StatusDescription, "node n3 down since "+n3Since; d != want {
		t.Fatalf("lost on a removed node: %q, want %q", d, want)
	}
	if d, want := get(t, s, onDownRemoved.ID).StatusDescription, "node n4 down since 2026-01-02T03:04:05Z; "+
		"restored from the checkpoint of 2026-01-01T00:00:00Z on the next use"; d != want {
		t.Fatalf("hibernated on a removed node: %q, want %q", d, want)
	}
	exec(t, s, "UPDATE nodes SET down_since = now() - interval '2 hours' WHERE name = 'n1'")
	exec(t, s, "UPDATE sandboxes SET run_id = 'run-1' WHERE id = $1", running.ID)
	exec(t, s, "UPDATE sandboxes SET pending_since = now() WHERE id = $1", neverSeen.ID)
	lost, err = s.LostGrace(bg, time.Hour, 256)
	if err != nil || len(lost) != 3 {
		t.Fatalf("lost grace on n1 %+v %v", lost, err)
	}
	for _, l := range lost {
		switch l.ID {
		case running.ID:
			if l.State != model.StateLost || l.RunID != "run-1" || l.Node != "n1" || l.Owner != "acme" {
				t.Fatalf("running → %+v", l)
			}
		case paused.ID:
			if l.State != model.StateHibernated {
				t.Fatalf("paused with a checkpoint → %+v", l)
			}
		case neverSeen.ID:
			if l.State != model.StatePending || l.Node != "n1" {
				t.Fatalf("never seen → %+v", l)
			}
		default:
			t.Fatalf("unexpected %+v", l)
		}
	}
	// A sandbox whose pod was never seen lost nothing: pending, without a
	// node, created on its next use.
	ns := get(t, s, neverSeen.ID)
	if ns.ClientState != model.StatePending || ns.Node != "" || ns.LostFrom != "" || ns.Materialised ||
		!ns.PendingSince.IsZero() || !strings.HasPrefix(ns.StatusDescription, "node n1 down since ") ||
		!strings.HasSuffix(ns.StatusDescription, "; its pod was never seen there; created on the next use") {
		t.Fatalf("never seen %+v", ns)
	}
	if _, err := s.GetBooking(bg, neverSeen.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatal("lost grace kept the booking of a pending sandbox")
	}
	r = get(t, s, running.ID)
	if r.Node != "n1" || r.LostFrom != "n1" || r.StatusDescription == "" {
		t.Fatalf("lost row %+v", r)
	}
	if !strings.Contains(get(t, s, paused.ID).StatusDescription, "restored from the checkpoint of") {
		t.Fatalf("hibernated description %q", get(t, s, paused.ID).StatusDescription)
	}
	if _, err := s.GetBooking(bg, running.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatal("lost grace kept a booking")
	}
	if holds, _ := s.ListHolds(bg, running.ID); len(holds) != 0 {
		t.Fatal("lost grace kept a process hold")
	}
	if tomb, _ := s.TombstonesOfPod(bg, running.ID); len(tomb) != 0 {
		t.Fatal("lost grace wrote a tombstone")
	}
}

func TestOpsOfInstances(t *testing.T) {
	s, d := newStore(t)
	mkProject(t, s, "acme")
	mkNode(t, s, "n1")
	must(t, s.UpsertInstance(bg, store.InstanceInfo{ID: "live"}))
	must(t, s.UpsertInstance(bg, store.InstanceInfo{ID: "stopping"}))
	must(t, s.SetInstanceStopping(bg, "stopping"))
	a := mkSandbox(t, s, "acme")
	b := mkSandbox(t, s, "acme")
	c := mkSandbox(t, s, "acme")
	e := mkSandbox(t, s, "acme")
	for _, sb := range []store.Sandbox{a, b, c, e} {
		place(t, s, sb.ID, "n1", model.StatePausing)
	}
	startOp(t, s, a.ID, "pause", "live", "n1", time.Minute)
	startOp(t, s, b.ID, "pause", "stopping", "n1", time.Minute)
	startOp(t, s, c.ID, "pause", "gone", "n1", time.Minute)
	startOp(t, s, e.ID, "pause", "live", "n1", -10*time.Minute)

	// Stale: the stopping and the dead owner's, and the live owner's op
	// far past its deadline.
	stale, err := s.StaleOps(bg, 5*time.Minute, 10)
	must(t, err)
	got := ids(stale)
	if len(got) != 3 || !slices.Contains(got, b.ID) || !slices.Contains(got, c.ID) || !slices.Contains(got, e.ID) {
		t.Fatalf("stale %v", got)
	}
	if live, _ := s.InstanceLive(bg, "live"); !live {
		t.Fatal("live")
	}

	// The stopping instance releases its own ops.
	ch, unsubscribe := d.Subscribe(db.ChanSandbox)
	defer unsubscribe()
	released, err := s.ReleaseOpsOf(bg, "stopping")
	if err != nil || len(released) != 1 || released[0] != b.ID {
		t.Fatalf("released %v %v", released, err)
	}
	expectPayload(t, ch, b.ID.String())
	r := get(t, s, b.ID)
	if r.Op != "" || r.OpID != uuid.Nil || r.OpOwner != "" || !r.OpDeadline.IsZero() || r.ClientState != model.StateUnknown ||
		r.StatusDescription != "op pause of stopping instance stopping was released" || r.Gen != b.Gen+1 || r.Node != "n1" {
		t.Fatalf("released row %+v", r)
	}

	// An instance's ops, by deadline and ID, a page at a time, without
	// those it names (the ones it runs).
	startOp(t, s, c.ID, "pause", "live", "n1", 2*time.Minute)
	opID := func(id podid.ID) uuid.UUID { return get(t, s, id).OpID }
	page, err := s.OpsOf(bg, "live", nil, store.OpCursor{}, 2)
	if err != nil || !slices.Equal(ids(page), []podid.ID{e.ID, a.ID}) {
		t.Fatalf("first page %v %v", ids(page), err)
	}
	next := store.OpCursor{}.Next(page)
	if page, err := s.OpsOf(bg, "live", nil, next, 2); err != nil || !slices.Equal(ids(page), []podid.ID{c.ID}) {
		t.Fatalf("second page %v %v", ids(page), err)
	}
	if page, err := s.OpsOf(bg, "live", []uuid.UUID{opID(e.ID), opID(a.ID)}, store.OpCursor{}, 2); err != nil ||
		!slices.Equal(ids(page), []podid.ID{c.ID}) {
		t.Fatalf("without the ops it runs %v %v", ids(page), err)
	}
}

func TestLoopCandidates(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "acme")
	mkNode(t, s, "n1")
	mkNode(t, s, "n2")
	exec(t, s, "UPDATE nodes SET object_store_id = 'store-1' WHERE name = 'n1'")
	idle := mkSandbox(t, s, "acme")
	busy := mkSandbox(t, s, "acme")
	fresh := mkSandbox(t, s, "acme")
	other := mkSandbox(t, s, "acme")
	for _, sb := range []store.Sandbox{idle, busy, fresh} {
		place(t, s, sb.ID, "n1", model.StateRunning)
	}
	place(t, s, other.ID, "n2", model.StateRunning)
	exec(t, s, "UPDATE sandboxes SET last_active_at = now() - interval '1 minute' WHERE id = ANY($1)",
		[]podid.ID{idle.ID, busy.ID, other.ID})
	must(t, s.RenewHold(bg, busy.ID, model.HoldExec, "i1", time.Minute))

	got, err := s.IdleSandboxes(bg, 10*time.Second, []string{"n1"}, 64)
	if err != nil || !slices.Equal(ids(got), []podid.ID{idle.ID}) {
		t.Fatalf("idle %v %v", ids(got), err)
	}
	// An expired hold does not keep a sandbox awake.
	exec(t, s, "UPDATE sandbox_holds SET lease_until = now() - interval '1 second'")
	if got, _ := s.IdleSandboxes(bg, 10*time.Second, []string{"n1", "n2"}, 64); len(got) != 3 {
		t.Fatalf("idle with an expired hold %v", ids(got))
	}
	if got, _ := s.IdleSandboxes(bg, 10*time.Second, nil, 64); len(got) != 0 {
		t.Fatal("idle without nodes")
	}
	// A failed hibernate-after does not back the idle pause off.
	exec(t, s, `UPDATE sandboxes SET policy_failed = 'hibernate_after', policy_failures = 3,
		policy_not_before = now() + interval '1 minute' WHERE id = $1`, other.ID)
	if got, _ := s.IdleSandboxes(bg, 10*time.Second, []string{"n1", "n2"}, 64); len(got) != 3 {
		t.Fatalf("idle while the hibernate-after is backed off %v", ids(got))
	}
	if ok, err := s.IdleRecheck(bg, other.ID, 10*time.Second); !ok || err != nil {
		t.Fatalf("idle recheck while the hibernate-after is backed off: %v %v", ok, err)
	}
	// A failed idle pause backs the policy off until policy_not_before.
	exec(t, s, `UPDATE sandboxes SET policy_failed = 'idle', policy_failures = 1,
		policy_not_before = now() + interval '1 minute' WHERE id = $1`, other.ID)
	if got, _ := s.IdleSandboxes(bg, 10*time.Second, []string{"n1", "n2"}, 64); len(got) != 2 || slices.Contains(ids(got), other.ID) {
		t.Fatalf("idle while backed off %v", ids(got))
	}
	if ok, err := s.IdleRecheck(bg, other.ID, 10*time.Second); ok || err != nil {
		t.Fatalf("idle recheck while backed off: %v %v", ok, err)
	}
	exec(t, s, "UPDATE sandboxes SET policy_not_before = now() - interval '1 second' WHERE id = $1", other.ID)
	if got, _ := s.IdleSandboxes(bg, 10*time.Second, []string{"n1", "n2"}, 64); len(got) != 3 {
		t.Fatalf("idle after the backoff %v", ids(got))
	}
	if ok, err := s.IdleRecheck(bg, other.ID, 10*time.Second); !ok || err != nil {
		t.Fatalf("idle recheck after the backoff: %v %v", ok, err)
	}

	// Hibernate-after: paused long enough, on a node with an object store.
	place(t, s, idle.ID, "n1", model.StatePaused)
	place(t, s, other.ID, "n2", model.StatePaused)
	exec(t, s, "UPDATE sandboxes SET paused_at = now() - interval '1 hour' WHERE id = ANY($1)", []podid.ID{idle.ID, other.ID})
	got, err = s.HibernateCandidates(bg, 30*time.Minute, []string{"n1", "n2"}, 10)
	if err != nil || !slices.Equal(ids(got), []podid.ID{idle.ID}) {
		t.Fatalf("hibernate %v %v", ids(got), err)
	}
	if ok, err := s.HibernateAfterRecheck(bg, idle.ID, 30*time.Minute); !ok || err != nil {
		t.Fatalf("hibernate-after recheck: %v %v", ok, err)
	}
	// A failed idle pause does not back the hibernate-after off; a failed
	// hibernate-after does.
	exec(t, s, `UPDATE sandboxes SET policy_failed = 'idle', policy_failures = 3,
		policy_not_before = now() + interval '1 minute' WHERE id = $1`, idle.ID)
	if got, _ := s.HibernateCandidates(bg, 30*time.Minute, []string{"n1", "n2"}, 10); len(got) != 1 {
		t.Fatalf("hibernate while the idle pause is backed off %v", ids(got))
	}
	if ok, err := s.HibernateAfterRecheck(bg, idle.ID, 30*time.Minute); !ok || err != nil {
		t.Fatalf("hibernate-after recheck while the idle pause is backed off: %v %v", ok, err)
	}
	exec(t, s, "UPDATE sandboxes SET policy_failed = 'hibernate_after', policy_failures = 1 WHERE id = $1", idle.ID)
	if got, _ := s.HibernateCandidates(bg, 30*time.Minute, []string{"n1", "n2"}, 10); len(got) != 0 {
		t.Fatalf("hibernate while backed off %v", ids(got))
	}
	if ok, err := s.HibernateAfterRecheck(bg, idle.ID, 30*time.Minute); ok || err != nil {
		t.Fatalf("hibernate-after recheck while backed off: %v %v", ok, err)
	}
	if sb := get(t, s, idle.ID); sb.PolicyFailed != store.PolicyHibernateAfter || sb.PolicyFailures != 1 ||
		sb.PolicyNotBefore.IsZero() {
		t.Fatalf("the backoff columns read %q %d %v", sb.PolicyFailed, sb.PolicyFailures, sb.PolicyNotBefore)
	}

	// Unknown sweep: unknown rows without an op on ready nodes.
	place(t, s, busy.ID, "n1", model.StateUnknown)
	place(t, s, other.ID, "n2", model.StateUnknown)
	exec(t, s, "UPDATE nodes SET status = 'down' WHERE name = 'n2'")
	got, err = s.UnknownSandboxes(bg, []string{"n1", "n2"}, 128)
	if err != nil || !slices.Equal(ids(got), []podid.ID{busy.ID}) {
		t.Fatalf("unknown %v %v", ids(got), err)
	}

	// Repair: stuck PENDING, or an unreachable guest, past their limits.
	place(t, s, fresh.ID, "n1", model.StateCreating)
	exec(t, s, "UPDATE sandboxes SET pending_since = now() - interval '20 minutes' WHERE id = $1", fresh.ID)
	place(t, s, busy.ID, "n1", model.StateRunning)
	exec(t, s, "UPDATE sandboxes SET unreachable_since = now() - interval '1 minute' WHERE id = $1", busy.ID)
	got, err = s.RepairCandidates(bg, 10*time.Minute, 2*time.Minute, []string{"n1"}, 10)
	if err != nil || !slices.Equal(ids(got), []podid.ID{fresh.ID}) {
		t.Fatalf("repair %v %v", ids(got), err)
	}
	exec(t, s, "UPDATE sandboxes SET unreachable_since = now() - interval '5 minutes' WHERE id = $1", busy.ID)
	if got, _ := s.RepairCandidates(bg, 10*time.Minute, 2*time.Minute, []string{"n1"}, 10); len(got) != 2 {
		t.Fatalf("repair with an unreachable guest %v", ids(got))
	}

	// Pending re-description.
	p := mkSandbox(t, s, "acme")
	exec(t, s, "UPDATE sandboxes SET status_description = 'no capacity (retryable): no feasible node' WHERE id = $1", p.ID)
	if got, err := s.PendingWithoutCapacity(bg, 10); err != nil || !slices.Equal(ids(got), []podid.ID{p.ID}) {
		t.Fatalf("pending without capacity %v %v", ids(got), err)
	}
}

func TestInTx(t *testing.T) {
	s, d := newStore(t)
	mkProject(t, s, "acme")
	boom := errors.New("boom")
	var id podid.ID
	err := s.InTx(bg, func(tx *store.Store) error {
		sb := mkSandbox(t, tx, "acme")
		id = sb.ID
		// A nested InTx runs on the same transaction.
		return tx.InTx(bg, func(inner *store.Store) error {
			if inner.Q() != tx.Q() {
				t.Error("nested InTx opened another transaction")
			}
			return boom
		})
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if _, err := s.GetSandbox(bg, id); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("a rolled back insert is visible")
	}

	// On wraps a transaction the caller owns.
	must(t, d.Tx(bg, func(tx pgx.Tx) error {
		on := store.On(tx)
		sb := mkSandbox(t, on, "acme")
		id = sb.ID
		return on.InTx(bg, func(inner *store.Store) error {
			_, err := inner.GetSandbox(bg, id)
			return err
		})
	}))
	if _, err := s.GetSandbox(bg, id); err != nil {
		t.Fatal(err)
	}
	// On a pool, InTx begins its own transaction.
	pool := store.On(d.Pool)
	err = pool.InTx(bg, func(tx *store.Store) error {
		mkSandbox(t, tx, "acme")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("pool InTx: %v", err)
	}
	if n, _ := s.CountSandboxesOfOwner(bg, "acme"); n != 1 {
		t.Fatalf("%d sandboxes, want 1", n)
	}

	// Now is the transaction's clock.
	tx(t, s, func(tx *store.Store) error {
		a, err := tx.Now(bg)
		if err != nil {
			return err
		}
		time.Sleep(5 * time.Millisecond)
		b, err := tx.Now(bg)
		if err == nil && !a.Equal(b) {
			t.Errorf("now() moved within a transaction: %s, %s", a, b)
		}
		return tx.SetLocalTimeouts(bg, time.Second, 3*time.Second)
	})
}
