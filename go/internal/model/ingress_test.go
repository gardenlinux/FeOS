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

// unambiguous reports whether the natural label of (name, project) names
// them alone: the name has no "--", and the project is a label of its own
// (no '.', no "--", not ending as a fallback does).
func unambiguous(name, project string) bool {
	return name != "" && !strings.Contains(name, "--") && !strings.HasSuffix(name, "-") && validHostLabel(project) &&
		!strings.Contains(project, "--") && !fallbackRE.MatchString(project)
}

// A project's name is its label when it is a host label without "--"
// that does not end as a fallback does.
func TestProjectLabel(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"kino": true, "hb-baxxter": true, "ironcore-dashboard": true, "a1": true, strings.Repeat("k", 63): true,
		"x0123456789abcdef": true, "x-0123456789abcde": true, "0123456789abcdef": true,
		"shop.io": false, "a--b": false, "xn--kino": false, "kino-": false, "-kino": false, "Kino": false,
		"x-0123456789abcdef": false, strings.Repeat("k", 64): false, "": false,
	} {
		if got := ProjectLabel(in); got != want {
			t.Errorf("ProjectLabel(%q) = %v, want %v", in, got, want)
		}
	}
}

// A chosen label is "<choice>--<project>" of a project with a label of its
// own; no other project's label, natural label or fallback is one.
func TestChosenHostLabel(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		label, project string
		want           bool
	}{
		{"stage--kino", "kino", true},
		{"production--ironcore-dashboard", "ironcore-dashboard", true},
		{"blue-green--kino", "kino", true},
		{"a--b", "b", true},
		{"stage--kino", "other", false},
		{"stage-kino", "kino", false},
		{"kino", "kino", false},
		{"qa--kino", "kino", false},
		{"Stage--kino", "kino", false},
		{"--kino", "kino", false},
		{"x---kino", "kino", false},
		{"a--b--kino", "kino", false},
		{"a--b--kino", "b--kino", false},
		{"web--shop.io", "shop.io", false},
		{"web--x-0123456789abcdef", "x-0123456789abcdef", false},
		{"stage--" + strings.Repeat("k", 57), strings.Repeat("k", 57), false},
	} {
		if got := ChosenHostLabel(c.label, c.project); got != c.want {
			t.Errorf("ChosenHostLabel(%q, %q) = %v, want %v", c.label, c.project, got, c.want)
		}
	}
	for _, in := range [][2]string{{"web", "acme"}, {"web", "a--b"}, {"web-prod", "acme"}, {"ab--x", "acme"}} {
		_, _, fallback := HostLabels(in[0], in[1], uuid.New())
		if ChosenHostLabel(fallback, in[1]) || ChosenHostLabel(fallback, "acme") {
			t.Errorf("%q: fallback %q reads as chosen", in, fallback)
		}
	}
}

// NaturalPair splits at the one "--"; a usable natural label splits into
// its own pair.
func TestNaturalPair(t *testing.T) {
	t.Parallel()
	for in, want := range map[string][3]string{
		"ironcore--dashboard": {"ironcore", "dashboard", "ok"},
		"web--acme-io":        {"web", "acme-io", "ok"},
		"blue-green--kino":    {"blue-green", "kino", "ok"},
		"dev--hb-baxxter":     {"dev", "hb-baxxter", "ok"},
		"kino":                {"kino", "", ""},
		"web-kino":            {"web-kino", "", ""},
		"--kino":              {"", "kino", ""},
		"kino--":              {"kino", "", ""},
		"a--b--c":             {"a", "b--c", ""},
		"":                    {"", "", ""},
	} {
		name, project, ok := NaturalPair(in)
		if name != want[0] || project != want[1] || ok != (want[2] == "ok") {
			t.Errorf("%q: (%q, %q, %v), want %q", in, name, project, ok, want)
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
		// A project name starts with a letter or a digit (projects.id).
		name, project := word("ab1", 1)+word("ab1-", r.IntN(8)), word("ab1", 1)+word("ab1-.", 1+r.IntN(10))
		natural, ok, _ := HostLabels(name, project, uuid.Nil)
		if !ok {
			continue
		}
		if !ChosenHostLabel(natural, project) || ProjectLabel(natural) {
			t.Fatalf("%q %q: natural %q is no chosen label's form, or a project's label", name, project, natural)
		}
		if n, tn, ok := NaturalPair(natural); !ok || n != name || tn != project {
			t.Fatalf("%q %q: natural %q splits into (%q, %q, %v)", name, project, natural, n, tn, ok)
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
		desc, name, project string
		natural             string
		naturalOK           bool
		fallback            string
	}{
		{"natural", "web", "acme", "web--acme", true, "web-" + hash},
		{"hyphenated project", "dev", "hb-baxxter", "dev--hb-baxxter", true, "dev-" + hash},
		{"hyphenated name", "web-prod", "acme", "web-prod--acme", true, "web-prod-" + hash},
		{"dotted project", "shop", "team.acme.io", "shop--team.acme.io", false, "shop-" + hash},
		{"project with --", "shop", "a--b", "shop--a--b", false, "shop-" + hash},
		{"name with --", "a--b", "acme", "a--b--acme", false, "a--b-" + hash},
		{"a project of 16 hex characters", "web", "0123456789abcdef", "web--0123456789abcdef", true, "web-" + hash},
		{"a project ending in a hyphen and 16 hex characters", "web", "x-0123456789abcdef",
			"web--x-0123456789abcdef", false, "web-" + hash},
		{"a project ending in a hyphen and 15 hex characters", "web", "x-0123456789abcde",
			"web--x-0123456789abcde", true, "web-" + hash},
		{"exactly 63 characters", strings.Repeat("a", 50), strings.Repeat("t", 11),
			strings.Repeat("a", 50) + "--" + strings.Repeat("t", 11), true, strings.Repeat("a", 46) + "-" + hash},
		{"64 characters", strings.Repeat("a", 50), strings.Repeat("t", 12),
			strings.Repeat("a", 50) + "--" + strings.Repeat("t", 12), false, strings.Repeat("a", 46) + "-" + hash},
		{"R-LDH name", "xn--abc", strings.Repeat("t", 10),
			"xn--abc--" + strings.Repeat("t", 10), false, "e-xn--abc-" + hash},
		{"one character", "a", "b", "a--b", true, "a-" + hash},
		{"two-character name, R-LDH natural", "qa", "kino", "qa--kino", false, "qa-" + hash},
		{"three-character name", "dev", "kino", "dev--kino", true, "dev-" + hash},
		{"hyphens at the cut are trimmed", strings.Repeat("a", 45) + "-" + strings.Repeat("b", 10), "t",
			strings.Repeat("a", 45) + "-" + strings.Repeat("b", 10) + "--t", true, strings.Repeat("a", 45) + "-" + hash},
		{"a project with an invalid character", "web", "Acme", "web--Acme", false, "web-" + hash},
		{"a project that ends in a dot", "web", "acme.", "web--acme.", false, "web-" + hash},
	} {
		natural, ok, fallback := HostLabels(c.name, c.project, id)
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
	// The fallback depends on the environment, not on the project.
	_, _, a := HostLabels("web", "acme", id)
	_, _, b := HostLabels("web", "other", id)
	_, _, c := HostLabels("web", "acme", uuid.MustParse("9e3a1c2b-4d5f-4a6b-8c7d-0e1f2a3b4c5d"))
	if a != b || a == c {
		t.Errorf("fallbacks %q %q %q", a, b, c)
	}
}

// A label HostLabels gave an environment under any name, the one it has
// or one it had before a rename, is its own, and so is its project's
// label; another's, a chosen one or another project's label is not.
func TestGivenHostLabel(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("0b8f4c1e-7a2d-4e6f-9c3b-5d1a2e7f8a90")
	other := uuid.MustParse("0b8f4c1e-7a2d-4e6f-9c3b-5d1a2e7f8a91")
	_, _, fallback := HostLabels("kino-prod-9101dfcb", "fecode", id)
	_, _, renamed := HostLabels("kino--prod", "fecode", id)
	_, _, others := HostLabels("kino-prod-9101dfcb", "fecode", other)
	for _, c := range []struct {
		label, project string
		want           bool
	}{
		{fallback, "fecode", true},
		{renamed, "fecode", true},
		{others, "fecode", false},
		{"web--fecode", "fecode", true},
		{"web-prod--fecode", "fecode", true},
		{"web--fecode", "acme", false},
		{"web-fecode", "fecode", false},
		{"qa--fecode", "fecode", false},
		{"fecode", "fecode", true},
		{"kino", "fecode", false},
		{"shop.io", "shop.io", false},
		{"", "fecode", false},
	} {
		if got := GivenHostLabel(c.label, c.project, id); got != c.want {
			t.Errorf("GivenHostLabel(%q, %q) = %v, want %v", c.label, c.project, got, c.want)
		}
	}
}

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

// For random environment names and projects, the natural label is taken
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
		project := label(1 + r.IntN(30))
		if r.IntN(3) == 0 {
			project += "." + label(1+r.IntN(20))
		}
		natural, ok, fallback := HostLabels(name, project, uuid.New())
		if ok != (unambiguous(name, project) && validHostLabel(natural)) {
			t.Fatalf("%q %q: natural %q ok %v", name, project, natural, ok)
		}
		if len(fallback) > 63 || !validHostLabel(fallback) {
			t.Fatalf("%q %q: fallback %q (%d)", name, project, fallback, len(fallback))
		}
	}
}

// The collisions of F4 and F14 of the security review: pairs whose
// "<name>-<project with '.' as '-'>" was the same get distinct labels, at
// most one of each group the natural one, while simple names keep it.
func TestHostLabelsCollisions(t *testing.T) {
	t.Parallel()
	for _, group := range [][][2]string{
		{{"web", "acme.io"}, {"web", "acme-io"}},
		{{"web-prod", "acme"}, {"web", "prod-acme"}},
		{{"shop", "acme.io"}, {"shop", "acme-io"}, {"shop-acme", "io"}},
		{{"a--b", "c"}, {"a", "b--c"}},
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
		if naturals > len(group) {
			t.Errorf("%q: %d natural labels", group, naturals)
		}
	}
	for _, p := range [][2]string{{"web", "acme"}, {"web", "acme-io"}, {"shop", "acme-corp"}, {"prod", "t1"}} {
		if natural, ok, _ := HostLabels(p[0], p[1], uuid.New()); !ok || natural != p[0]+"--"+p[1] {
			t.Errorf("%q: natural %q, ok %v", p, natural, ok)
		}
	}
}

// No two distinct (name, project) pairs share a usable natural label, and
// none has the form of a fallback: every pair over a small alphabet, where
// the old scheme collided often, and random pairs that end in hex.
func TestHostLabelsInjective(t *testing.T) {
	t.Parallel()
	owner := map[string][2]string{}
	check := func(name, project string) {
		natural, ok, _ := HostLabels(name, project, uuid.Nil)
		if !ok {
			return
		}
		if fallbackRE.MatchString(natural) {
			t.Fatalf("%q %q: natural %q has a fallback's form", name, project, natural)
		}
		p := [2]string{name, project}
		if q, dup := owner[natural]; dup && q != p {
			t.Fatalf("%q and %q share the natural label %q", q, p, natural)
		}
		if ProjectLabel(natural) {
			t.Fatalf("%q %q: natural %q is a project's label", name, project, natural)
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
	for _, n := range all("a1-", 1, 6) {
		if labelRE.MatchString(n) {
			names = append(names, n)
		}
	}
	projectRE := regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,62}$`)
	var projects []string
	for _, n := range all("a1-.", 2, 5) {
		if projectRE.MatchString(n) {
			projects = append(projects, n)
		}
	}
	for _, n := range names {
		for _, tn := range projects {
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
		project := word("ab.-", 1+r.IntN(6))
		if r.IntN(2) == 0 {
			project += word("-"+hexDigits, 14+r.IntN(4)) + word(hexDigits, 2)
		}
		check(name, project)
	}
}
