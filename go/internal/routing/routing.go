// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package routing is the path and host handling an environment's routes
// share between the route door of ferund and ferun-ingress (ADR 0021):
// the refusal of dot segments, the segment-aware longest prefix match
// over the routes that answer on a host (ADR 0045), and the stripping of
// a route's path. Both serve the same routes, so both must read a path
// and a host the same way.
package routing

import (
	"net/url"
	"strings"
)

// DotSegment is ".." or "." when a segment of a path's escaped form is
// that, as it is or decoded once or twice, else "". Each decode is
// lenient, as an upstream that decodes again may be: a valid %XX becomes
// its byte and any other "%" stays, so a "%" that the first decode made
// ("/q/100%25") is a character, not a reason to refuse, and still hides
// no "%2e%2e" beside it.
func DotSegment(raw string) string {
	p := raw
	for i := range 3 {
		for seg := range strings.SplitSeq(p, "/") {
			if seg == "." || seg == ".." {
				return seg
			}
		}
		if i < 2 {
			p = LenientUnescape(p)
		}
	}
	return ""
}

// LenientUnescape decodes each valid %XX of s and leaves any other "%"
// as it is.
func LenientUnescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && IsHex(s[i+1]) && IsHex(s[i+2]) {
			b.WriteByte(Unhex(s[i+1])<<4 | Unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// IsHex reports whether c is a hexadecimal digit.
func IsHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// Unhex is the value of the hexadecimal digit c.
func Unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// Match returns the index of the prefix that is the longest
// segment-aware prefix of raw's segments (each unescaped), and how many
// segments it has; ok false when none matches. A prefix is a route's
// path: "/" matches every path with no segment, and a trailing "/" is
// not a segment ("/api/" is "/api"). Of prefixes of one length, the
// first wins.
func Match(prefixes []string, raw string) (idx, segs int, ok bool) {
	return MatchHost(prefixes, nil, "", raw)
}

// MatchHost is Match over the routes that answer on host, a request's
// canonical host (CanonicalHost): hosts[i] is the host of the route of
// prefixes[i], canonical, "" for none (nil: no route has one). A route
// answers on host when it has no host or its host covers host (HostRank).
// The longest prefix wins, whatever the hosts; of prefixes of one length,
// the route whose host names host most closely, its own name before its
// wildcard before none (ADR 0045); of those, the first.
func MatchHost(prefixes, hosts []string, host, raw string) (idx, segs int, ok bool) {
	parts := strings.Split(strings.TrimPrefix(raw, "/"), "/")
	idx, segs = -1, -1
	rank := RankNone
	for i, prefix := range prefixes {
		r := RankAny
		if i < len(hosts) {
			if r = HostRank(hosts[i], host); r == RankNone {
				continue
			}
		}
		var want []string
		if p := strings.TrimSuffix(prefix, "/"); p != "" {
			want = strings.Split(strings.TrimPrefix(p, "/"), "/")
		}
		if len(want) > len(parts) || len(want) < segs || (len(want) == segs && r <= rank) {
			continue
		}
		match := true
		for j, w := range want {
			if seg, err := url.PathUnescape(parts[j]); err != nil || seg != w {
				match = false
				break
			}
		}
		if match {
			idx, segs, rank = i, len(want), r
		}
	}
	if idx < 0 {
		return -1, 0, false
	}
	return idx, segs, true
}

// StripSegments removes the first n segments of a path's escaped form
// and returns what remains, unescaped and escaped ("" when it needs no
// escaping).
func StripSegments(raw string, n int) (string, string, error) {
	segs := strings.Split(strings.TrimPrefix(raw, "/"), "/")
	rest := "/" + strings.Join(segs[n:], "/")
	path, err := url.PathUnescape(rest)
	if err != nil {
		return "", "", err
	}
	return path, RawOf(path, rest), nil
}

// RawOf is the escaped form of a path, "" when it is the default
// escaping.
func RawOf(path, raw string) string {
	if (&url.URL{Path: path}).EscapedPath() == raw {
		return ""
	}
	return raw
}
