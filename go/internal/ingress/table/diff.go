// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package table

import (
	"slices"
	"strings"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// Caps are what a stream's ingress named in its Hello's capabilities,
// which decide the environments it is given (Visible).
type Caps struct {
	// Visits: it checks visits (wire.CapabilityVisits, ADR 0054).
	Visits bool
	// RouteHosts: it serves a route on its host alone
	// (wire.CapabilityRouteHosts, ADR 0045).
	RouteHosts bool
}

// CapsOf are the capabilities a Hello named.
func CapsOf(capabilities []string) Caps {
	return Caps{Visits: slices.Contains(capabilities, wire.CapabilityVisits),
		RouteHosts: slices.Contains(capabilities, wire.CapabilityRouteHosts)}
}

// Diff returns what turns the environments a stream sent (their hashes by
// ID) into those of cur: the environments of cur that are new or whose
// hash differs, and the IDs of those sent that cur no longer has, each
// sorted by ID. An environment its ingress must not be given (Visible:
// one not open to anyone to an ingress that does not check visits, ADR
// 0054; one with a route that has a host to an ingress that does not
// serve hosts, ADR 0045) counts as one cur does not have: it is never
// put, and removed once it turns so. The puts are cur's values, shared:
// never change them. A nil snapshot (no table yet) changes nothing.
func Diff(sent map[string]string, cur *Snapshot, caps Caps) (puts []wire.Env, removed []string) {
	if cur == nil {
		return nil, nil
	}
	for id, e := range cur.Envs {
		if !Visible(e, caps) {
			continue
		}
		if h, ok := sent[id]; !ok || h != e.Hash {
			puts = append(puts, e)
		}
	}
	for id := range sent {
		if e, ok := cur.Envs[id]; !ok || !Visible(e, caps) {
			removed = append(removed, id)
		}
	}
	slices.SortFunc(puts, func(a, b wire.Env) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(removed)
	return puts, removed
}

// Visible reports whether a stream gives environment e to its ingress:
// one open to anyone to every ingress, any other only to one that checks
// visits; one with a route that has a host only to one that serves a
// route on its host alone, as an older one would serve every route on
// every host and send a host's requests to another host's service.
func Visible(e wire.Env, caps Caps) bool {
	return (caps.Visits || e.Access == "") && (caps.RouteHosts || !HostRoutes(e))
}

// HostRoutes reports whether a route of e has a host.
func HostRoutes(e wire.Env) bool {
	return slices.ContainsFunc(e.Routes, func(r wire.Route) bool { return r.Host != "" })
}

// Hashes returns the hashes of envs by ID: what a stream that sent envs
// passes to Diff next.
func Hashes(envs map[string]wire.Env) map[string]string {
	out := make(map[string]string, len(envs))
	for id, e := range envs {
		out[id] = e.Hash
	}
	return out
}
