// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A sandbox made before ADR 0019 keeps its UUID: it lives beside the
// short IDs through every query of its life.
func TestLegacySandboxBesideShortIDs(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	legacy := mkSandboxID(t, s, "acme", "0190a7c4-1e2f-7a3b-9c4d-5e6f708192a3")
	short := mkSandbox(t, s, "acme")
	if !legacy.ID.IsLegacy() || short.ID.IsLegacy() || !podid.Valid(string(short.ID)) {
		t.Fatalf("ids %q %q", legacy.ID, short.ID)
	}
	if got := get(t, s, legacy.ID); got.ID != legacy.ID || got.Incarnation != legacy.Incarnation {
		t.Fatalf("get %+v", got)
	}
	l, err := s.ListSandboxes(bg, "acme")
	if err != nil || len(l) != 2 || !slices.ContainsFunc(l, func(sb store.Sandbox) bool { return sb.ID == legacy.ID }) {
		t.Fatalf("list %+v %v", l, err)
	}

	// Its tokens, holds and booking.
	for _, sb := range []store.Sandbox{legacy, short} {
		hash := store.HashToken("token of " + string(sb.ID))
		if _, err := s.CreateSandboxToken(bg, hash, sb.ID, "default", ""); err != nil {
			t.Fatal(err)
		}
		if id, owner, err := s.ResolveSandboxToken(bg, hash); err != nil || id != sb.ID || owner != "acme" {
			t.Fatalf("resolve %q: %q %q %v", sb.ID, id, owner, err)
		}
		if tokens, err := s.ListSandboxTokens(bg, sb.ID); err != nil || len(tokens) != 1 || tokens[0].SandboxID != sb.ID {
			t.Fatalf("tokens of %q: %+v %v", sb.ID, tokens, err)
		}
		place(t, s, sb.ID, "n1", model.StateRunning)
		must(t, s.AdmitHold(bg, sb.ID, model.HoldExec, "i1", time.Minute))
		if live, err := s.HasLiveHold(bg, sb.ID); err != nil || !live {
			t.Fatalf("hold of %q: %v %v", sb.ID, live, err)
		}
		if holds, err := s.ListHolds(bg, sb.ID); err != nil || len(holds) != 1 || holds[0].SandboxID != sb.ID {
			t.Fatalf("holds of %q: %+v %v", sb.ID, holds, err)
		}
		must(t, s.UpsertBooking(bg, store.Booking{PodID: sb.ID, Node: "n1", VCPUs: 1, MemoryMiB: 256, Gated: true}))
		if b, err := s.GetBooking(bg, sb.ID); err != nil || b.PodID != sb.ID {
			t.Fatalf("booking of %q: %+v %v", sb.ID, b, err)
		}
	}
	if sums, err := s.SumBookings(bg, "n1", legacy.ID); err != nil || sums.Count != 1 {
		t.Fatalf("sums without the legacy pod: %+v %v", sums, err)
	}
	pods, err := s.SteadyPods(bg, "n1")
	if err != nil || len(pods) != 2 {
		t.Fatalf("steady pods %+v %v", pods, err)
	}

	// Its deletion takes its tokens, holds and booking with it.
	if deleted, err := s.DeleteSandbox(bg, legacy.ID); err != nil || !deleted {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	if _, err := s.GetSandbox(bg, legacy.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if _, _, err := s.ResolveSandboxToken(bg, store.HashToken("token of "+string(legacy.ID))); !errors.Is(err, model.ErrUnknownToken) {
		t.Fatalf("token after delete: %v", err)
	}
	if _, err := s.GetBooking(bg, legacy.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("booking after delete: %v", err)
	}
	if holds, err := s.ListHolds(bg, legacy.ID); err != nil || len(holds) != 0 {
		t.Fatalf("holds after delete: %+v %v", holds, err)
	}
	if got := get(t, s, short.ID); got.ID != short.ID {
		t.Fatalf("the short one %+v", got)
	}
}

func TestCreateMintedSandbox(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	seen := map[podid.ID]bool{}
	for range 20 {
		sb, err := s.CreateMintedSandbox(bg, func(id podid.ID) store.NewSandbox {
			return store.NewSandbox{ID: id, Owner: "acme", Image: "alpine", VCPUs: 1, MemoryMiB: 256,
				PodTemplate: &model.PodTemplate{SecurityGroups: []string{model.SandboxGroupID(id)}}}
		})
		if err != nil || !podid.Valid(string(sb.ID)) || sb.ID.IsLegacy() || seen[sb.ID] {
			t.Fatalf("minted %q: %v", sb.ID, err)
		}
		seen[sb.ID] = true
		if sb.PodTemplate == nil || sb.PodTemplate.SecurityGroups[0] != "sbx-"+string(sb.ID) {
			t.Fatalf("template of %q: %+v", sb.ID, sb.PodTemplate)
		}
		if got := get(t, s, sb.ID); got.ID != sb.ID {
			t.Fatalf("get %+v", got)
		}
	}
	// A row whose ID is not the one drawn is refused.
	_, err := s.CreateMintedSandbox(bg, func(podid.ID) store.NewSandbox {
		return store.NewSandbox{ID: podid.Mint(), Owner: "acme", Image: "alpine", VCPUs: 1, MemoryMiB: 256}
	})
	if err == nil {
		t.Fatal("a row with another ID was inserted")
	}
	if n, err := s.CountSandboxesOfOwner(bg, "acme"); err != nil || n != 20 {
		t.Fatalf("count %d %v", n, err)
	}
}

// The schema keeps every sandbox ID in its canonical form.
func TestSandboxIDCheck(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	for _, id := range []string{"NOT-AN-ID", "0190A7C4-1E2F-7A3B-9C4D-5E6F708192A3", "Abcdefghjkm", "0bcdefghjkm",
		"abcdefghjkl", "abcdefghjk", ""} {
		_, err := s.Q().Exec(bg, `INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib) VALUES ($1, 'acme', 'x', 1, 256)`, id)
		if err == nil || !strings.Contains(err.Error(), "sandboxes_id_format") {
			t.Errorf("insert of %q: %v", id, err)
		}
	}
	for _, id := range []string{"abcdefghjkm", "0190a7c4-1e2f-7a3b-9c4d-5e6f708192a3"} {
		exec(t, s, `INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib) VALUES ($1, 'acme', 'x', 1, 256)`, id)
	}
	// The columns without a foreign key have the check of their own.
	mkNode(t, s, "n1")
	if _, err := s.Q().Exec(bg, `INSERT INTO pod_tombstones (pod_id, node, incarnation) VALUES ('nope', 'n1', gen_random_uuid())`); err == nil {
		t.Error("a tombstone of an invalid ID was stored")
	}
}

// The registry orders a node's steady pods byte by byte, as feosd sorts
// its inventory, whatever the database's locale: short IDs and UUIDs
// mixed.
func TestSteadyPodsByteOrder(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	ids := []string{"abcdef12xyz", "abcdef12-0000-4000-8000-000000000001", "0190a7c4-1e2f-7a3b-9c4d-5e6f708192a3",
		"z0000000000", "a0000000000", "ffffffff-0000-4000-8000-000000000001", "b1234567890"}
	for _, id := range ids {
		mkSandboxID(t, s, "acme", podid.ID(id))
		place(t, s, podid.ID(id), "n1", model.StateRunning)
	}
	pods, err := s.SteadyPods(bg, "n1")
	must(t, err)
	var got []string
	for _, p := range pods {
		got = append(got, string(p.ID))
	}
	want := slices.Clone(ids)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("steady pods %v, want %v", got, want)
	}
}

// Placement's snapshot names a TAP conflict only between UUIDs.
func TestPlacementSnapshotTAP(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	running := mkSandboxID(t, s, "acme", "abcdef12-0000-4000-8000-000000000001")
	place(t, s, running.ID, "n1", model.StateRunning)
	shortRunning := mkSandboxID(t, s, "acme", "abcdef12xyz")
	place(t, s, shortRunning.ID, "n1", model.StateRunning)
	for _, tc := range []struct {
		pod  podid.ID
		want podid.ID
	}{
		{"abcdef12-1111-4000-8000-000000000002", running.ID}, // a UUID with the same first 8 hex digits
		{"abcdef12abc", ""},                          // a short ID with them
		{"12345678-0000-4000-8000-000000000003", ""}, // another UUID
		{podid.Mint(), ""},
	} {
		mkSandboxID(t, s, "acme", tc.pod)
		snap, err := s.PlacementSnapshot(bg, tc.pod)
		if err != nil || len(snap) != 1 || snap[0].TAPConflict != tc.want {
			t.Fatalf("snapshot of %q: %+v %v; want the conflict %q", tc.pod, snap, err, tc.want)
		}
		if m, err := s.TAPConflicts(bg, tc.pod); err != nil || m["n1"] != tc.want {
			t.Fatalf("TAPConflicts of %q: %v %v", tc.pod, m, err)
		}
	}
}
