// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package routing

import "testing"

// A wildcard covers the names one label below its name, and no other.
func TestCovers(t *testing.T) {
	for _, c := range []struct {
		pattern, host string
		want          bool
	}{
		{"app.example.com", "app.example.com", true},
		{"app.example.com", "x.app.example.com", false},
		{"*.boxed.example.com", "8080--abc.boxed.example.com", true},
		{"*.boxed.example.com", "boxed.example.com", false},
		{"*.boxed.example.com", "a.b.boxed.example.com", false},
		{"*.boxed.example.com", "x.other.example.com", false},
		{"", "", false},
	} {
		if got := Covers(c.pattern, c.host); got != c.want {
			t.Errorf("Covers(%q, %q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
	for host, want := range map[string]string{"a.b.c": "*.b.c", "b.c": "*.c", "c": "", "*.b.c": "", ".c": "", "c.": ""} {
		if got := WildcardOf(host); got != want {
			t.Errorf("WildcardOf(%q) = %q, want %q", host, got, want)
		}
	}
}

// A host is compared in lower case, without its port and its trailing
// dot.
func TestCanonicalHost(t *testing.T) {
	for in, want := range map[string]string{"App.Example.COM": "app.example.com", "app.example.com.": "app.example.com",
		"app.example.com:8443": "app.example.com", "App.Example.com.:443": "app.example.com", "": "",
		"*.Boxed.Example.com": "*.boxed.example.com", "[fd00::1]:80": "fd00::1"} {
		if got := CanonicalHost(in); got != want {
			t.Errorf("CanonicalHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// A route's host ranks its own name above its wildcard above none; a
// host it does not cover ranks below every route.
func TestHostRank(t *testing.T) {
	for _, c := range []struct {
		route, host string
		want        int
	}{
		{"", "fecode.dev", RankAny},
		{"fecode.dev", "fecode.dev", RankExact},
		{"git.fecode.dev", "fecode.dev", RankNone},
		{"*.boxed.fecode.dev", "8080--a.boxed.fecode.dev", RankWildcard},
		{"*.boxed.fecode.dev", "boxed.fecode.dev", RankNone},
		{"*.boxed.fecode.dev", "a.b.boxed.fecode.dev", RankNone},
		{"api.boxed.fecode.dev", "api.boxed.fecode.dev", RankExact},
	} {
		if got := HostRank(c.route, c.host); got != c.want {
			t.Errorf("HostRank(%q, %q) = %d, want %d", c.route, c.host, got, c.want)
		}
	}
}

// FeCode's environment (ADR 0045): the longest prefix among the routes
// that answer on the host wins, and of one prefix the route whose host
// names the host most closely.
func TestMatchHost(t *testing.T) {
	type route struct{ name, host, prefix string }
	routes := []route{
		{"api", "fecode.dev", "/api/v1"},
		{"boxed", "*.boxed.fecode.dev", "/"},
		{"docs", "fecode.dev", "/docs"},
		{"git", "git.fecode.dev", "/"},
		{"health", "", "/healthz"},
		{"own", "own.boxed.fecode.dev", "/"},
		{"rest", "", "/"},
		{"ui", "fecode.dev", "/"},
	}
	var prefixes, hosts []string
	for _, r := range routes {
		prefixes, hosts = append(prefixes, r.prefix), append(hosts, r.host)
	}
	for _, c := range []struct{ host, raw, want string }{
		{"fecode.dev", "/", "ui"},
		{"fecode.dev", "/app/settings", "ui"},
		{"fecode.dev", "/api/v1/projects", "api"},
		{"fecode.dev", "/api/v2", "ui"},
		{"fecode.dev", "/docs", "docs"},
		{"fecode.dev", "/healthz", "health"},
		{"git.fecode.dev", "/api/v1/projects", "git"},
		{"git.fecode.dev", "/healthz", "health"},
		{"8080--abc.boxed.fecode.dev", "/docs", "boxed"},
		{"own.boxed.fecode.dev", "/x", "own"},
		{"boxed.fecode.dev", "/x", "rest"},
		{"other.example", "/docs", "rest"},
	} {
		i, _, ok := MatchHost(prefixes, hosts, c.host, c.raw)
		if !ok || routes[i].name != c.want {
			got := ""
			if ok {
				got = routes[i].name
			}
			t.Errorf("%s%s: %q, want %q", c.host, c.raw, got, c.want)
		}
	}
	// Without a route that answers on the host, nothing matches.
	if _, _, ok := MatchHost([]string{"/"}, []string{"fecode.dev"}, "git.fecode.dev", "/"); ok {
		t.Error("a route of another host matched")
	}
	// Of one prefix and one rank, the first wins, as in Match.
	if i, _, ok := MatchHost([]string{"/x", "/x/"}, []string{"a.example", "a.example"}, "a.example", "/x/y"); !ok || i != 0 {
		t.Errorf("tie: %d %v", i, ok)
	}
}
