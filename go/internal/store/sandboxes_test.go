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

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

func TestSandboxCreateAndDelete(t *testing.T) {
	s, d := newStore(t)
	mkTenant(t, s, "acme")
	mkTenant(t, s, "other")
	ch, unsubscribe := d.Subscribe(db.ChanSandbox)
	defer unsubscribe()

	id := podid.Mint()
	sb := mkSandboxID(t, s, "acme", id)
	expectPayload(t, ch, id.String())
	if sb.ClientState != model.StatePending || sb.StatusDescription != "awaiting first tool call" ||
		sb.Incarnation == uuid.Nil || sb.Gen != 0 || sb.Node != "" || sb.OpID != uuid.Nil ||
		sb.AvoidNodes == nil || sb.VCPUs != 2 || sb.MemoryMiB != 512 || sb.LastActiveAt.IsZero() {
		t.Fatalf("new row %+v", sb)
	}

	// The same ID again, by anyone: the existing row, not created.
	again, created, err := s.CreateSandbox(bg, store.NewSandbox{ID: id, Owner: "other", Image: "x", VCPUs: 1, MemoryMiB: 256,
		EnvironmentID: boxEnv(t, s, "other")})
	if err != nil || created || again.Owner != "acme" || again.Incarnation != sb.Incarnation {
		t.Fatalf("second create: %+v %v %v", again, created, err)
	}
	mkSandbox(t, s, "other")
	if l, err := s.ListSandboxes(bg, "acme", nil); err != nil || len(l) != 1 || l[0].ID != id {
		t.Fatalf("list acme: %v %v", l, err)
	}
	if l, err := s.ListSandboxes(bg, "", nil); err != nil || len(l) != 2 {
		t.Fatalf("list all: %v %v", l, err)
	}
	if n, err := s.CountSandboxesOfOwner(bg, "other"); err != nil || n != 1 {
		t.Fatalf("count: %d %v", n, err)
	}

	deleted, err := s.DeleteSandbox(bg, id)
	if err != nil || !deleted {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	expectPayload(t, ch, id.String())
	if _, err := s.GetSandbox(bg, id); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if deleted, err := s.DeleteSandbox(bg, id); err != nil || deleted {
		t.Fatalf("second delete: %v %v", deleted, err)
	}
	// Re-created with the same ID: a new incarnation.
	re := mkSandboxID(t, s, "acme", id)
	if re.Incarnation == sb.Incarnation {
		t.Fatal("a re-created sandbox kept its incarnation")
	}
}

func TestUpdateSandbox(t *testing.T) {
	s, d := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	ch, unsubscribe := d.Subscribe(db.ChanSandbox)
	defer unsubscribe()
	sb := mkSandbox(t, s, "acme")
	expectPayload(t, ch, sb.ID.String())

	// A lifecycle write: gen + 1, updated_at, notification; nullable
	// columns round-trip.
	var written store.Sandbox
	var now time.Time
	tx(t, s, func(tx *store.Store) error {
		before, err := tx.LockSandbox(bg, sb.ID, store.ForUpdate)
		if err != nil {
			return err
		}
		if now, err = tx.Now(bg); err != nil {
			return err
		}
		after := before
		after.ClientState = model.StateCreating
		after.Node = "n1"
		after.Op, after.OpID, after.OpOwner = "create", uuid.New(), "i1"
		after.OpDeadline = now.Add(7 * time.Minute)
		after.OpTarget, after.OpFromState = "n1", before.ClientState
		after.ImageRef = "docker.io/library/alpine@sha256:abc"
		after.AvoidNodes = []string{"n9"}
		written, err = tx.UpdateSandbox(bg, before, after)
		return err
	})
	expectPayload(t, ch, sb.ID.String())
	got := get(t, s, sb.ID)
	if got.Gen != 1 || got.ClientState != model.StateCreating || got.Node != "n1" || got.Op != "create" ||
		got.OpID == uuid.Nil || !got.OpDeadline.Equal(now.Add(7*time.Minute)) || got.ImageRef == "" ||
		!slices.Equal(got.AvoidNodes, []string{"n9"}) || !got.UpdatedAt.After(sb.UpdatedAt) {
		t.Fatalf("after lifecycle write: %+v", got)
	}
	if written.Gen != got.Gen || written.OpID != got.OpID {
		t.Fatalf("returned %+v, stored %+v", written, got)
	}

	// Activity and description writes bump no gen and send nothing.
	tx(t, s, func(tx *store.Store) error {
		before, err := tx.LockSandbox(bg, sb.ID, store.ForUpdate)
		if err != nil {
			return err
		}
		after := before
		after.StatusDescription = "still starting"
		after.LastActiveAt = before.LastActiveAt.Add(time.Second)
		after.RunID = "run-1"
		after.PendingSince = before.CreatedAt
		_, err = tx.UpdateSandbox(bg, before, after)
		return err
	})
	expectNoPayload(t, ch, sb.ID.String())
	got2 := get(t, s, sb.ID)
	if got2.Gen != 1 || got2.StatusDescription != "still starting" || got2.RunID != "run-1" ||
		!got2.PendingSince.Equal(sb.CreatedAt) || !got2.UpdatedAt.After(got.UpdatedAt) {
		t.Fatalf("after activity write: %+v", got2)
	}

	// Clearing nullable columns writes NULL.
	tx(t, s, func(tx *store.Store) error {
		before, err := tx.LockSandbox(bg, sb.ID, store.ForUpdate)
		if err != nil {
			return err
		}
		after := before
		after.Op, after.OpID, after.OpOwner, after.OpDeadline, after.OpTarget, after.OpFromState = "", uuid.Nil, "", time.Time{}, "", ""
		after.Node, after.ClientState, after.PendingSince = "", model.StatePending, time.Time{}
		after.AvoidNodes = nil
		_, err = tx.UpdateSandbox(bg, before, after)
		return err
	})
	got3 := get(t, s, sb.ID)
	if got3.Gen != 2 || got3.Node != "" || got3.OpID != uuid.Nil || !got3.OpDeadline.IsZero() ||
		!got3.PendingSince.IsZero() || len(got3.AvoidNodes) != 0 {
		t.Fatalf("after clearing: %+v", got3)
	}
	var nulls int
	must(t, s.Q().QueryRow(bg, "SELECT count(*) FROM sandboxes WHERE id = $1 AND node IS NULL AND op_id IS NULL AND op_deadline IS NULL",
		sb.ID).Scan(&nulls))
	if nulls != 1 {
		t.Fatal("cleared columns are not NULL")
	}

	// Nothing differs: nothing written.
	same, err := s.UpdateSandbox(bg, got3, got3)
	if err != nil || same.Gen != got3.Gen {
		t.Fatalf("no-op update: %+v %v", same, err)
	}
	// A fixed column, or a row that moved on, is refused.
	changed := got3
	changed.VCPUs = 4
	if _, err := s.UpdateSandbox(bg, got3, changed); err == nil || !strings.Contains(err.Error(), "vcpus") {
		t.Fatalf("vcpus change: %v", err)
	}
	changed = got
	changed.ClientState = model.StateRunning
	if _, err := s.UpdateSandbox(bg, got, changed); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale update: %v", err)
	}
	if get(t, s, sb.ID).ClientState != model.StatePending {
		t.Fatal("a stale update was written")
	}
}

func TestSmallSandboxWrites(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	sb := mkSandbox(t, s, "acme")
	if ok, err := s.MarkUnreachable(bg, sb.ID); err != nil || ok {
		t.Fatalf("unreachable while pending: %v %v", ok, err)
	}
	place(t, s, sb.ID, "n1", model.StateRunning)
	if ok, err := s.MarkUnreachable(bg, sb.ID); err != nil || !ok {
		t.Fatalf("unreachable: %v %v", ok, err)
	}
	first := get(t, s, sb.ID).UnreachableSince
	if ok, _ := s.MarkUnreachable(bg, sb.ID); !ok || !get(t, s, sb.ID).UnreachableSince.Equal(first) {
		t.Fatal("a second mark moved unreachable_since")
	}
	must(t, s.ClearUnreachable(bg, sb.ID))
	if !get(t, s, sb.ID).UnreachableSince.IsZero() {
		t.Fatal("unreachable_since not cleared")
	}
	if ok, err := s.MarkPending(bg, sb.ID, uuid.New()); err != nil || ok {
		t.Fatalf("pending of another incarnation: %v %v", ok, err)
	}
	if ok, err := s.MarkPending(bg, sb.ID, sb.Incarnation); err != nil || !ok || get(t, s, sb.ID).PendingSince.IsZero() {
		t.Fatalf("pending: %v %v", ok, err)
	}
	must(t, s.SetStatusDescription(bg, sb.ID, sb.Incarnation, "hello"))
	if got := get(t, s, sb.ID); got.StatusDescription != "hello" || got.Gen != 0 {
		t.Fatalf("description: %+v", got)
	}
	old := get(t, s, sb.ID).LastActiveAt
	must(t, s.TouchSandbox(bg, sb.ID))
	if !get(t, s, sb.ID).LastActiveAt.After(old) {
		t.Fatal("touch did not move last_active_at")
	}
}

func TestTAPConflict(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	mkNode(t, s, "n2")
	// Only UUIDs, the IDs of sandboxes made before ADR 0019, share a TAP
	// device's name: pod- and their first 8 hex digits.
	a := podid.ID("abcdef01-0000-4000-8000-000000000001")
	b := podid.ID("abcdef01-1111-4000-8000-000000000002")
	c := podid.ID("12345678-0000-4000-8000-000000000003")
	// A short ID whose first 8 characters are a's first 8 hex digits
	// names its TAP device whole: it shares a's no more than any other.
	short := podid.ID("abcdef01xyz")
	mkSandboxID(t, s, "acme", a)
	mkSandboxID(t, s, "acme", b)
	mkSandboxID(t, s, "acme", c)
	mkSandboxID(t, s, "acme", short)
	place(t, s, b, "n1", model.StateRunning)
	place(t, s, c, "n1", model.StateRunning)
	place(t, s, short, "n1", model.StateRunning)

	check := func(node string, want podid.ID) {
		t.Helper()
		got, err := s.TAPConflict(bg, node, a)
		if err != nil || got != want {
			t.Fatalf("TAPConflict(%s) = %v %v, want %v", node, got, err, want)
		}
	}
	check("n1", b)
	check("n2", "")
	if m, err := s.TAPConflicts(bg, a); err != nil || len(m) != 1 || m["n1"] != b {
		t.Fatalf("TAPConflicts %v %v", m, err)
	}
	// Hibernated, pending or lost pods hold no TAP device...
	for _, st := range []string{model.StateHibernated, model.StatePending, model.StateLost} {
		place(t, s, b, "n1", st)
		check("n1", "")
	}
	// ...unless an op is in flight for them.
	startOp(t, s, b, "restore", "i1", "n1", time.Minute)
	check("n1", b)
	// A sandbox never conflicts with itself.
	place(t, s, a, "n1", model.StateRunning)
	if got, _ := s.TAPConflict(bg, "n1", b); got != a {
		t.Fatalf("conflict of b: %v", got)
	}
	// A short ID conflicts with nothing, and nothing with it.
	if got, err := s.TAPConflict(bg, "n1", short); err != nil || got != "" {
		t.Fatalf("conflict of a short ID: %v %v", got, err)
	}
	if m, err := s.TAPConflicts(bg, short); err != nil || len(m) != 0 {
		t.Fatalf("TAPConflicts of a short ID: %v %v", m, err)
	}
	other := mkSandbox(t, s, "acme")
	if got, err := s.TAPConflict(bg, "n1", other.ID); err != nil || got != "" {
		t.Fatalf("conflict of a minted ID: %v %v", got, err)
	}
	place(t, s, b, "n1", model.StateHibernated)
	exec(t, s, "UPDATE sandboxes SET op = '', op_id = NULL, op_owner = '', op_target = '', op_deadline = NULL WHERE id = $1", b)
	place(t, s, a, "n2", model.StateHibernated)
	if got, err := s.TAPConflict(bg, "n1", a); err != nil || got != "" {
		t.Fatalf("a UUID beside a short ID with its first 8 characters: %v %v", got, err)
	}
}

func TestAttributePod(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	for _, n := range []string{"n1", "n2", "n3", "n4", "n5", "n6"} {
		mkNode(t, s, n)
	}
	sb := mkSandbox(t, s, "acme")
	exec(t, s, "UPDATE sandboxes SET node = 'n1', op_target = 'n2', lost_from = 'n3' WHERE id = $1", sb.ID)
	_, err := s.UpsertTombstone(bg, store.Tombstone{PodID: sb.ID, Node: "n4", Incarnation: uuid.New()})
	must(t, err)
	_, err = s.InsertIssuedCert(bg, store.IssuedCert{Serial: "01", PodID: sb.ID, Incarnation: sb.Incarnation, Node: "n5",
		SPIFFEID: "spiffe://x/y", HPKEPublicKey: make([]byte, 32), NotAfter: time.Now().Add(time.Hour)})
	must(t, err)
	for _, n := range []string{"n1", "n2", "n3", "n4", "n5"} {
		a, ok, err := s.AttributePod(bg, sb.ID, n)
		if err != nil || !ok || a.SandboxID != sb.ID || a.TenantID != "acme" {
			t.Fatalf("%s: %+v %v %v", n, a, ok, err)
		}
	}
	if _, ok, err := s.AttributePod(bg, sb.ID, "n6"); err != nil || ok {
		t.Fatalf("n6: %v %v", ok, err)
	}
	if _, ok, err := s.AttributePod(bg, podid.Mint(), "n1"); err != nil || ok {
		t.Fatalf("unknown pod: %v %v", ok, err)
	}
	// A certificate older than a day no longer attributes.
	exec(t, s, "UPDATE issued_certs SET issued_at = now() - interval '25 hours'")
	if _, ok, _ := s.AttributePod(bg, sb.ID, "n5"); ok {
		t.Fatal("an old certificate attributes")
	}
}

func TestSteadyPodsAndNodeSandboxes(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	mkNode(t, s, "n2")
	ids := []podid.ID{
		"00000000-0000-4000-8000-000000000003",
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000004",
	}
	for _, id := range ids {
		mkSandboxID(t, s, "acme", id)
	}
	place(t, s, ids[0], "n1", model.StateRunning)
	place(t, s, ids[1], "n1", model.StatePaused)
	place(t, s, ids[2], "n1", model.StateRunning)
	startOp(t, s, ids[2], "pause", "i1", "n1", time.Minute)
	exec(t, s, "UPDATE sandboxes SET lost_from = 'n1', node = 'n2', client_state = 'lost' WHERE id = $1", ids[3])
	pods, err := s.SteadyPods(bg, "n1")
	must(t, err)
	if len(pods) != 2 || pods[0].ID != ids[1] || pods[0].State != model.StatePaused || pods[1].ID != ids[0] {
		t.Fatalf("steady pods %+v", pods)
	}
	on, err := s.SandboxesOfNode(bg, "n1")
	must(t, err)
	if len(on) != 4 {
		t.Fatalf("sandboxes of n1: %d", len(on))
	}
	if n, err := s.CountSandboxesOnNode(bg, "n1"); err != nil || n != 3 {
		t.Fatalf("count on n1: %d %v", n, err)
	}
	counts, err := s.SandboxStateCounts(bg)
	must(t, err)
	if counts["n1"][model.StateRunning] != 2 || counts["n1"][model.StatePaused] != 1 || counts["n2"][model.StateLost] != 1 {
		t.Fatalf("counts %v", counts)
	}
	inState, err := s.SandboxesInState(bg, "n1", []string{model.StateRunning, model.StatePaused}, 10)
	must(t, err)
	if len(inState) != 2 {
		t.Fatalf("in state: %d", len(inState))
	}
}

func TestLockSandboxesOfOwner(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	for range 5 {
		mkSandbox(t, s, "acme")
	}
	tx(t, s, func(tx *store.Store) error {
		batch, err := tx.LockSandboxesOfOwner(bg, "acme", 3)
		if err != nil {
			return err
		}
		if len(batch) != 3 || batch[0].ID.String() > batch[1].ID.String() {
			t.Fatalf("batch %d", len(batch))
		}
		return nil
	})
}
