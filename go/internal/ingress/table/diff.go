// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package table

import (
	"slices"
	"strings"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// Diff returns what turns the environments a stream sent (their hashes by
// ID) into those of cur: the environments of cur that are new or whose
// hash differs, and the IDs of those sent that cur no longer has, each
// sorted by ID. For a stream whose ingress does not check visits (visits
// false: its Hello did not name wire.CapabilityVisits), an environment
// that is not open to anyone (Access set, ADR 0054) counts as one cur does
// not have: it is never put, and removed once it turns so. The puts are
// cur's values, shared: never change them. A nil snapshot (no table yet)
// changes nothing.
func Diff(sent map[string]string, cur *Snapshot, visits bool) (puts []wire.Env, removed []string) {
	if cur == nil {
		return nil, nil
	}
	for id, e := range cur.Envs {
		if !Visible(e, visits) {
			continue
		}
		if h, ok := sent[id]; !ok || h != e.Hash {
			puts = append(puts, e)
		}
	}
	for id := range sent {
		if e, ok := cur.Envs[id]; !ok || !Visible(e, visits) {
			removed = append(removed, id)
		}
	}
	slices.SortFunc(puts, func(a, b wire.Env) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(removed)
	return puts, removed
}

// Visible reports whether a stream gives environment e to its ingress:
// one open to anyone to every ingress, any other only to one that checks
// visits.
func Visible(e wire.Env, visits bool) bool { return visits || e.Access == "" }

// Hashes returns the hashes of envs by ID: what a stream that sent envs
// passes to Diff next.
func Hashes(envs map[string]wire.Env) map[string]string {
	out := make(map[string]string, len(envs))
	for id, e := range envs {
		out[id] = e.Hash
	}
	return out
}
