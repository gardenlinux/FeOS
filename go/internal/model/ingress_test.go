// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// labelRE is the DNS label of the host_labels table's check, which also
// refuses R-LDH (validHostLabel).
var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validHostLabel(s string) bool {
	return labelRE.MatchString(s) && !(len(s) >= 4 && s[2:4] == "--")
}

// fallbackRE is the end of every fallback label: "-" and 16 hex
// characters.
var fallbackRE = regexp.MustCompile(`-[0-9a-f]{16}$`)

// unambiguous reports whether the natural label of (name, tenant) names
// them alone: the name has no '-', the tenant no '.', and the label does
// not end as a fallback does.
func unambiguous(name, tenant string) bool {
	return !strings.Contains(name, "-") && !strings.Contains(tenant, ".") && !strings.Contains(tenant, "--") &&
		!fallbackRE.MatchString(name+"-"+tenant)
}

// A chosen label has "--", is a host label and does not end as a
// fallback; no natural label HostLabels takes and no fallback is one.
func TestChosenHostLabel(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"stage--kino": true, "production--ironcore-dashboard": true, "a--b": true,
		"stage-kino": false, "qa--kino": false, "Stage--kino": false, "stage--kino-": false, "--kino": false,
		"stage--kino-0123456789abcdef": false, "stage--" + strings.Repeat("k", 57): false,
	} {
		if got := ChosenHostLabel(in); got != want {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
	for _, in := range [][2]string{{"web", "acme"}, {"web", "a--b"}, {"web-prod", "acme"}, {"ab", "c--d"}} {
		natural, ok, fallback := HostLabels(in[0], in[1], uuid.New())
		if (ok && ChosenHostLabel(natural)) || ChosenHostLabel(fallback) {
			t.Errorf("%q: natural %q (%v) or fallback %q reads as chosen", in, natural, ok, fallback)
		}
	}
}

// A plain label, a zone owner's, is a host label without "--" that does
// not end as a fallback; a fallback is never one, and every label with
// "--" is a chosen one or neither.
func TestPlainHostLabel(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"kino": true, "ironcore-dashboard": true, "a": true, "x1": true, "web-acme": true,
		strings.Repeat("k", 63): true, "kino-0123456789abcde": true, "kino-0123456789abcdeg": true,
		"stage--kino": false, "xn--kino": false, "ab--": false, "Kino": false, "kino-": false, "-kino": false,
		"kino-0123456789abcdef": false, "0123456789abcdef": true, "-0123456789abcdef": false,
		strings.Repeat("k", 64): false, "": false, "ki_no": false, "kino.dev": false,
	} {
		if got := PlainHostLabel(in); got != want {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
		if PlainHostLabel(in) && ChosenHostLabel(in) {
			t.Errorf("%q is plain and chosen", in)
		}
	}
	for _, in := range [][2]string{{"web", "acme"}, {"web-prod", "acme"}, {"ab--x", "acme"}, {"web", "acme.io"}} {
		if _, _, fallback := HostLabels(in[0], in[1], uuid.New()); PlainHostLabel(fallback) {
			t.Errorf("%q: fallback %q reads as plain", in, fallback)
		}
	}
}

// NaturalPair splits at the first hyphen; a usable natural label splits
// into its own pair, so a plain label is the natural label of that pair
// alone.
func TestNaturalPair(t *testing.T) {
	t.Parallel()
	for in, want := range map[string][3]string{
		"ironcore-dashboard": {"ironcore", "dashboard", "ok"},
		"web-acme-io":        {"web", "acme-io", "ok"},
		"kino":               {"kino", "", ""},
		"-kino":              {"", "kino", ""},
		"kino-":              {"kino", "", ""},
		"":                   {"", "", ""},
	} {
		name, tenant, ok := NaturalPair(in)
		if name != want[0] || tenant != want[1] || ok != (want[2] == "ok") {
			t.Errorf("%q: (%q, %q, %v), want %q", in, name, tenant, ok, want)
		}
	}
	r := rand.New(rand.NewPCG(5, 6))
	word := func(set string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = set[r.IntN(len(set))]
		}
		return string(b)
	}
	for range 5000 {
		// A tenant name starts with a letter or a digit (tenants.id).
		name, tenant := word("ab1-", 1+r.IntN(8)), word("ab1", 1)+word("ab1-.", 1+r.IntN(10))
		natural, ok, _ := HostLabels(name, tenant, uuid.Nil)
		if !ok {
			continue
		}
		if !PlainHostLabel(natural) {
			t.Fatalf("%q %q: natural %q is not plain", name, tenant, natural)
		}
		if n, tn, ok := NaturalPair(natural); !ok || n != name || tn != tenant {
			t.Fatalf("%q %q: natural %q splits into (%q, %q, %v)", name, tenant, natural, n, tn, ok)
		}
	}
}

func TestIngressGroupID(t *testing.T) {
	t.Parallel()
	short := podid.ID("k3m9q7a2b4c")
	if got := IngressGroupID(short); got != "ing-k3m9q7a2b4c" || len(got) != 15 {
		t.Fatalf("%q", got)
	}
	long := podid.ID(uuid.MustParse("6f1c2a9e-0b7d-4c3e-9a51-2d8e7f6a5b4c").String())
	if got := IngressGroupID(long); got != "ing-6f1c2a9e-0b7d-4c3e-9a51-2d8e7f6a5b4c" || len(got) != 40 {
		t.Fatalf("%q", got)
	}
}

// The natural label when it is unambiguous and a valid label that is not
// R-LDH; else the fallback, which always is.
func TestHostLabels(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("0b8f4c1e-7a2d-4e6f-9c3b-5d1a2e7f8a90")
	sum := sha256.Sum256([]byte(id.String()))
	hash := hex.EncodeToString(sum[:])[:16]
	for _, c := range []struct {
		desc, name, tenant string
		natural            string
		naturalOK          bool
		fallback           string
	}{
		{"natural", "web", "acme", "web-acme", true, "web-" + hash},
		{"hyphenated tenant", "web", "acme-io", "web-acme-io", true, "web-" + hash},
		{"dotted tenant", "shop", "team.acme.io", "shop-team.acme.io", false, "shop-" + hash},
		{"hyphenated name", "web-prod", "acme", "web-prod-acme", false, "web-prod-" + hash},
		{"a tenant of 16 hex characters", "web", "0123456789abcdef", "web-0123456789abcdef", false, "web-" + hash},
		{"a tenant ending in a hyphen and 16 hex characters", "web", "x-0123456789abcdef",
			"web-x-0123456789abcdef", false, "web-" + hash},
		{"a tenant ending in 16 hex characters after a letter", "web", "x0123456789abcdef",
			"web-x0123456789abcdef", true, "web-" + hash},
		{"a tenant ending in a hyphen and 15 hex characters", "web", "x-0123456789abcde",
			"web-x-0123456789abcde", true, "web-" + hash},
		{"exactly 63 characters", strings.Repeat("a", 50), strings.Repeat("t", 12),
			strings.Repeat("a", 50) + "-" + strings.Repeat("t", 12), true, strings.Repeat("a", 46) + "-" + hash},
		{"64 characters", strings.Repeat("a", 50), strings.Repeat("t", 13),
			strings.Repeat("a", 50) + "-" + strings.Repeat("t", 13), false, strings.Repeat("a", 46) + "-" + hash},
		{"R-LDH natural", "ab--x", "acme", "ab--x-acme", false, "e-ab--x-" + hash},
		{"R-LDH natural from the tenant", "ab", "-x", "ab--x", false, "ab-" + hash},
		{"R-LDH name, natural over long", "xn--abc", strings.Repeat("t", 60),
			"xn--abc-" + strings.Repeat("t", 60), false, "e-xn--abc-" + hash},
		{"one character", "a", "b", "a-b", true, "a-" + hash},
		{"two-character name", "ab", "cd", "ab-cd", true, "ab-" + hash},
		{"hyphens at the cut are trimmed", strings.Repeat("a", 45) + "-" + strings.Repeat("b", 10), "t",
			strings.Repeat("a", 45) + "-" + strings.Repeat("b", 10) + "-t", false, strings.Repeat("a", 45) + "-" + hash},
		{"a tenant with an invalid character", "web", "Acme", "web-Acme", false, "web-" + hash},
		{"a tenant that ends in a dot", "web", "acme.", "web-acme.", false, "web-" + hash},
	} {
		natural, ok, fallback := HostLabels(c.name, c.tenant, id)
		if natural != c.natural || ok != c.naturalOK || fallback != c.fallback {
			t.Errorf("%s: (%q, %v, %q), want (%q, %v, %q)", c.desc, natural, ok, fallback, c.natural, c.naturalOK, c.fallback)
		}
		if ok && !validHostLabel(natural) {
			t.Errorf("%s: natural %q is not a host label", c.desc, natural)
		}
		if !validHostLabel(fallback) {
			t.Errorf("%s: fallback %q is not a host label", c.desc, fallback)
		}
	}
	// xn--abc: the fallback starts with e-xn--abc-, valid and not R-LDH.
	if _, _, fb := HostLabels("xn--abc", "acme", id); !strings.HasPrefix(fb, "e-xn--abc-") || !validHostLabel(fb) {
		t.Errorf("xn--abc: %q", fb)
	}
	// The fallback depends on the environment, not on the tenant.
	_, _, a := HostLabels("web", "acme", id)
	_, _, b := HostLabels("web", "other", id)
	_, _, c := HostLabels("web", "acme", uuid.MustParse("9e3a1c2b-4d5f-4a6b-8c7d-0e1f2a3b4c5d"))
	if a != b || a == c {
		t.Errorf("fallbacks %q %q %q", a, b, c)
	}
}

// Any input, however malformed, yields an answer without a panic.
func TestHostLabelsNeverPanics(t *testing.T) {
	t.Parallel()
	for _, in := range [][2]string{
		{"", ""}, {"", "acme"}, {"web", ""}, {"-", "-"}, {"--", "."}, {"---", ""}, {"ab--", ""}, {"a--", ""},
		{strings.Repeat("-", 100), ""}, {strings.Repeat("é", 40), "ü.ö"}, {"\x00\xff", "\x00"},
	} {
		natural, ok, fallback := HostLabels(in[0], in[1], uuid.Nil)
		if ok && !validHostLabel(natural) {
			t.Errorf("%q: natural %q taken", in, natural)
		}
		if len(fallback) > 63 {
			t.Errorf("%q: fallback %q is longer than 63", in, fallback)
		}
	}
}

// For random environment names and tenants, the natural label is taken
// exactly when it is unambiguous and a host label, and the fallback is
// always one.
func TestHostLabelsRandom(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	const inner = "abcdefghijklmnopqrstuvwxyz0123456789-"
	label := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			set := inner
			if i == 0 || i == n-1 {
				set = inner[:36]
			}
			b[i] = set[r.IntN(len(set))]
		}
		return string(b)
	}
	for range 5000 {
		name := label(1 + r.IntN(63))
		if r.IntN(8) == 0 && len(name) >= 4 {
			name = name[:2] + "--" + name[4:]
		}
		tenant := label(1 + r.IntN(30))
		if r.IntN(3) == 0 {
			tenant += "." + label(1+r.IntN(20))
		}
		natural, ok, fallback := HostLabels(name, tenant, uuid.New())
		if ok != (unambiguous(name, tenant) && validHostLabel(natural)) {
			t.Fatalf("%q %q: natural %q ok %v", name, tenant, natural, ok)
		}
		if len(fallback) > 63 || !validHostLabel(fallback) {
			t.Fatalf("%q %q: fallback %q (%d)", name, tenant, fallback, len(fallback))
		}
	}
}

// The collisions of F4 and F14 of the security review: pairs whose
// "<name>-<tenant with '.' as '-'>" was the same get distinct labels, at
// most one of each group the natural one, while simple names keep it.
func TestHostLabelsCollisions(t *testing.T) {
	t.Parallel()
	for _, group := range [][][2]string{
		{{"web", "acme.io"}, {"web", "acme-io"}},
		{{"web-prod", "acme"}, {"web", "prod.acme"}},
		{{"shop", "acme.io"}, {"shop", "acme-io"}, {"shop-acme", "io"}},
		{{"shop", "acme-corp"}, {"shop-acme", "corp"}},
	} {
		seen := map[string][2]string{}
		naturals := 0
		for _, p := range group {
			natural, ok, fallback := HostLabels(p[0], p[1], uuid.New())
			label := fallback
			if ok {
				label, naturals = natural, naturals+1
			}
			if q, dup := seen[label]; dup {
				t.Errorf("%q and %q both get %q", q, p, label)
			}
			seen[label] = p
		}
		if naturals > 1 {
			t.Errorf("%q: %d natural labels, want at most 1", group, naturals)
		}
	}
	for _, p := range [][2]string{{"web", "acme"}, {"web", "acme-io"}, {"shop", "acme-corp"}, {"prod", "t1"}} {
		if natural, ok, _ := HostLabels(p[0], p[1], uuid.New()); !ok || natural != p[0]+"-"+p[1] {
			t.Errorf("%q: natural %q, ok %v", p, natural, ok)
		}
	}
}

// No two distinct (name, tenant) pairs share a usable natural label, and
// none has the form of a fallback: every pair over a small alphabet, where
// the old scheme collided often, and random pairs that end in hex.
func TestHostLabelsInjective(t *testing.T) {
	t.Parallel()
	owner := map[string][2]string{}
	check := func(name, tenant string) {
		natural, ok, _ := HostLabels(name, tenant, uuid.Nil)
		if !ok {
			return
		}
		if fallbackRE.MatchString(natural) {
			t.Fatalf("%q %q: natural %q has a fallback's form", name, tenant, natural)
		}
		p := [2]string{name, tenant}
		if q, dup := owner[natural]; dup && q != p {
			t.Fatalf("%q and %q share the natural label %q", q, p, natural)
		}
		owner[natural] = p
	}
	// All strings over alphabet of length lo to hi.
	all := func(alphabet string, lo, hi int) []string {
		var out []string
		cur := []string{""}
		for n := 1; n <= hi; n++ {
			var next []string
			for _, c := range cur {
				for i := 0; i < len(alphabet); i++ {
					next = append(next, c+alphabet[i:i+1])
				}
			}
			if n >= lo {
				out = append(out, next...)
			}
			cur = next
		}
		return out
	}
	var names []string
	for _, n := range all("a1-", 1, 5) {
		if labelRE.MatchString(n) {
			names = append(names, n)
		}
	}
	tenantRE := regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,62}$`)
	var tenants []string
	for _, n := range all("a1-.", 2, 5) {
		if tenantRE.MatchString(n) {
			tenants = append(tenants, n)
		}
	}
	for _, n := range names {
		for _, tn := range tenants {
			check(n, tn)
		}
	}
	r := rand.New(rand.NewPCG(3, 4))
	const hexDigits = "0123456789abcdef"
	word := func(set string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = set[r.IntN(len(set))]
		}
		return string(b)
	}
	for range 20000 {
		name := word("ab-", 1+r.IntN(6))
		tenant := word("ab.-", 1+r.IntN(6))
		if r.IntN(2) == 0 {
			tenant += word("-"+hexDigits, 14+r.IntN(4)) + word(hexDigits, 2)
		}
		check(name, tenant)
	}
}
