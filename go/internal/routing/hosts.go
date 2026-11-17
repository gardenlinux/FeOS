// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package routing

import "strings"

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
