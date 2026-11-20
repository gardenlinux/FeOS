// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"net"
	"strings"
)

// The hosts of an environment are names and wildcards (ADR 0045): a
// wildcard *.<name> serves every name exactly one label below <name>, as
// a certificate's wildcard covers, and not <name> itself. A name's own
// host wins over a wildcard, so a host is looked up as it is first, and
// only then as its wildcard.

// IsWildcard reports whether host, canonical, is a wildcard *.<name>.
func IsWildcard(host string) bool { return strings.HasPrefix(host, "*.") }

// WildcardOf is the wildcard that covers host, canonical: "*." and its
// parent, one label up; "" for a host of one label, or a wildcard.
func WildcardOf(host string) string {
	if IsWildcard(host) {
		return ""
	}
	i := strings.IndexByte(host, '.')
	if i <= 0 || i == len(host)-1 {
		return ""
	}
	return "*" + host[i:]
}

// Covers reports whether a host of an environment, a name or a wildcard,
// serves the canonical host: the same name, or a wildcard of its parent.
func Covers(pattern, host string) bool {
	return pattern != "" && (pattern == host || (IsWildcard(pattern) && WildcardOf(host) == pattern))
}

// CanonicalHost is a host as the routing compares it: in lower case,
// without a port and without a trailing dot. A request's Host, the Host
// the route door gives the guest and a route's host are all compared in
// this form.
func CanonicalHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// The ranks of a route's host for a request's host (HostRank): how
// closely the route's host names it. A route without a host answers on
// every host, below a route whose host is the host's wildcard, below one
// whose host is the host itself.
const (
	RankNone     = -1 // the route's host does not cover the request's
	RankAny      = 0  // the route has no host
	RankWildcard = 1  // the route's host is the wildcard that covers it
	RankExact    = 2  // the route's host is the request's host
)

// HostRank is the rank of a route's host, canonical ("": none), for the
// canonical host of a request (ADR 0045).
func HostRank(routeHost, host string) int {
	switch {
	case routeHost == "":
		return RankAny
	case routeHost == host:
		return RankExact
	case IsWildcard(routeHost) && WildcardOf(host) == routeHost:
		return RankWildcard
	}
	return RankNone
}
