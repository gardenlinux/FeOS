// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package table

import (
	"maps"
	"slices"
	"testing"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

func snapshotOf(envs ...wire.Env) *Snapshot {
	s := newSnapshot()
	for _, e := range envs {
		s.Envs[e.ID] = e
	}
	return s
}

func ids(envs []wire.Env) []string {
	var out []string
	for _, e := range envs {
		out = append(out, e.ID)
	}
	return out
}

// Diff puts the new and changed environments and removes those gone, each
// sorted by ID; after a stream sent what Diff gave, the next Diff of the
// same snapshot is empty.
func TestDiff(t *testing.T) {
	cur := snapshotOf(wire.Env{ID: "c", Hash: "c1"}, wire.Env{ID: "a", Hash: "a2"}, wire.Env{ID: "b", Hash: "b1"},
		wire.Env{ID: "e", Hash: "e1"})
	sent := map[string]string{"a": "a1", "b": "b1", "d": "d1", "f": "f1"}
	puts, removed := Diff(sent, cur, all)
	if got := ids(puts); !slices.Equal(got, []string{"a", "c", "e"}) || puts[0].Hash != "a2" {
		t.Fatalf("puts %+v", puts)
	}
	if !slices.Equal(removed, []string{"d", "f"}) {
		t.Fatalf("removed %v", removed)
	}

	// Everything to a stream that sent nothing.
	puts, removed = Diff(nil, cur, all)
	if got := ids(puts); !slices.Equal(got, []string{"a", "b", "c", "e"}) || removed != nil {
		t.Fatalf("from nothing: %v %v", got, removed)
	}
	// Nothing once it sent the snapshot.
	sent = Hashes(cur.Envs)
	if !maps.Equal(sent, map[string]string{"a": "a2", "b": "b1", "c": "c1", "e": "e1"}) {
		t.Fatalf("hashes %v", sent)
	}
	if puts, removed := Diff(sent, cur, all); puts != nil || removed != nil {
		t.Fatalf("after sending: %v %v", puts, removed)
	}
	// Everything removed for an empty table; nothing for no table.
	if puts, removed := Diff(sent, newSnapshot(), all); puts != nil || !slices.Equal(removed, []string{"a", "b", "c", "e"}) {
		t.Fatalf("empty table: %v %v", puts, removed)
	}
	if puts, removed := Diff(sent, nil, all); puts != nil || removed != nil {
		t.Fatalf("no table: %v %v", puts, removed)
	}
}

// A stream whose ingress does not check visits never gets an environment
// that is not open to anyone: it is not put, one sent before is removed
// once it turns guarded, and put again once it is open again. A stream
// that checks visits gets it.
func TestDiffVisits(t *testing.T) {
	open := wire.Env{ID: "a", Hash: "a1"}
	guarded := wire.Env{ID: "b", Hash: "b2", Access: wire.AccessVisitors, Provider: "ferun", SignIn: "https://auth.example/visit"}
	odd := wire.Env{ID: "c", Hash: "c1", Access: "members"} // an Access the ingress does not know admits nobody
	cur := snapshotOf(open, guarded, odd)

	puts, removed := Diff(nil, cur, Caps{})
	if got := ids(puts); !slices.Equal(got, []string{"a"}) || removed != nil {
		t.Fatalf("without visits: %v %v", got, removed)
	}
	// b was sent open before it turned guarded: removed.
	puts, removed = Diff(map[string]string{"a": "a1", "b": "b1"}, cur, Caps{})
	if puts != nil || !slices.Equal(removed, []string{"b"}) {
		t.Fatalf("turned guarded: %v %v", ids(puts), removed)
	}
	// Open again: put.
	reopened := snapshotOf(open, wire.Env{ID: "b", Hash: "b3"})
	if puts, removed := Diff(map[string]string{"a": "a1"}, reopened, Caps{}); !slices.Equal(ids(puts), []string{"b"}) || removed != nil {
		t.Fatalf("open again: %v %v", ids(puts), removed)
	}
	puts, removed = Diff(nil, cur, visits)
	if got := ids(puts); !slices.Equal(got, []string{"a", "b", "c"}) || removed != nil || puts[1].Access != wire.AccessVisitors {
		t.Fatalf("with visits: %v %v", got, removed)
	}
	if !Visible(open, Caps{}) || Visible(guarded, Caps{}) || Visible(odd, Caps{}) || !Visible(guarded, visits) {
		t.Fatal("Visible")
	}
}

// all and visits are the capabilities of a stream whose ingress names
// every one, or only visits.
var (
	all    = Caps{Visits: true, RouteHosts: true}
	visits = Caps{Visits: true}
)

// A stream whose ingress does not serve a route's host never gets an
// environment with a route that has one (ADR 0045), as it would serve
// every route on every host: it is not put, one sent before is removed
// once a route gains a host, and put again once none has one. A stream
// that serves hosts gets it, and the capabilities are read from a Hello.
func TestDiffRouteHosts(t *testing.T) {
	plain := wire.Env{ID: "a", Hash: "a1", Routes: []wire.Route{{Name: "root", Path: "/"}}}
	hosted := wire.Env{ID: "b", Hash: "b2", Routes: []wire.Route{{Name: "root", Path: "/"},
		{Name: "git", Path: "/", Host: "git.example.com"}}}
	cur := snapshotOf(plain, hosted)
	if puts, removed := Diff(nil, cur, visits); !slices.Equal(ids(puts), []string{"a"}) || removed != nil {
		t.Fatalf("without route hosts: %v %v", ids(puts), removed)
	}
	if puts, removed := Diff(map[string]string{"a": "a1", "b": "b1"}, cur, visits); puts != nil ||
		!slices.Equal(removed, []string{"b"}) {
		t.Fatalf("gained a host: %v %v", ids(puts), removed)
	}
	again := snapshotOf(plain, wire.Env{ID: "b", Hash: "b3", Routes: []wire.Route{{Name: "root", Path: "/"}}})
	if puts, removed := Diff(map[string]string{"a": "a1"}, again, visits); !slices.Equal(ids(puts), []string{"b"}) ||
		removed != nil {
		t.Fatalf("no host again: %v %v", ids(puts), removed)
	}
	if puts, removed := Diff(nil, cur, Caps{RouteHosts: true}); !slices.Equal(ids(puts), []string{"a", "b"}) || removed != nil {
		t.Fatalf("with route hosts: %v %v", ids(puts), removed)
	}
	// Both capabilities apply: a guarded environment with a host route
	// needs both.
	both := wire.Env{ID: "c", Hash: "c1", Access: wire.AccessVisitors, Routes: hosted.Routes}
	if Visible(both, visits) || Visible(both, Caps{RouteHosts: true}) || !Visible(both, all) {
		t.Fatal("Visible with both")
	}
	if got := CapsOf([]string{"x", wire.CapabilityRouteHosts}); got != (Caps{RouteHosts: true}) {
		t.Fatalf("CapsOf %+v", got)
	}
	if got := CapsOf([]string{wire.CapabilityVisits, wire.CapabilityRouteHosts}); got != all {
		t.Fatalf("CapsOf %+v", got)
	}
	if CapsOf(nil) != (Caps{}) {
		t.Fatal("CapsOf nil")
	}
}
