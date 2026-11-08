// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/model"
)

// The shipped blueprints and an addon of each with every option, for the
// checks that hold for all.
// fields are those a reference needs (postgres: a role, ADR 0038).
var shipped = []struct {
	name, raw string
	fields    map[string]string
}{
	{"postgres", `{"type":"postgres","version":"17","plan":"dev",
		"options":{"databases":["expensa","reports"],"roles":["expense_svc","approval_svc"]}}`,
		map[string]string{"role": "expense_svc"}},
	{"valkey", `{"type":"valkey","version":"9","plan":"dev","options":{"maxmemoryPolicy":"allkeys-lfu"}}`, nil},
	{"nats", `{"type":"nats","version":"2","plan":"dev","options":{"jetstream":true}}`, nil},
}

func TestShippedTypes(t *testing.T) {
	for _, typ := range []string{"nats", "postgres", "valkey"} {
		if !slices.Contains(Types(), typ) {
			t.Errorf("%s is not registered: %v", typ, Types())
		}
	}
}

// TestShippedContracts: what every blueprint must hold to, beyond its
// own tests.
func TestShippedContracts(t *testing.T) {
	for _, s := range shipped {
		t.Run(s.name, func(t *testing.T) {
			b, ok := Get(s.name)
			if !ok {
				t.Fatal("not registered")
			}
			a := addon(t, s.name, s.raw)
			if b.Type() != a.Type {
				t.Fatalf("type %q", b.Type())
			}
			if fs := b.Validate(nil, a); len(fs) != 0 {
				t.Fatalf("a valid addon has findings:\n%s", findingText(fs))
			}
			if fs := b.Validate(ptr(a), a); len(fs) != 0 {
				t.Fatalf("an unchanged addon has findings:\n%s", findingText(fs))
			}
			if _, ok := b.Plans()[b.DefaultPlan()]; !ok {
				t.Fatalf("default plan %q is not a plan", b.DefaultPlan())
			}
			for name, p := range b.Plans() {
				// The node's floor and the spec's limits.
				if p.VCPUs < 1 || p.MemoryMib < 128 || p.VolumeMib < 1 {
					t.Errorf("plan %s: %+v", name, p)
				}
			}
			c := Context{Env: "stage", Name: s.name, Address: "fd6b::1"}
			inst, err := b.Render(a, c)
			if err != nil {
				t.Fatal(err)
			}
			if inst.Image == "" || inst.Port == 0 || len(inst.Probe) == 0 {
				t.Fatalf("instance %+v", inst)
			}
			if inst.Mount != "" && (!strings.HasPrefix(inst.Mount, "/") || inst.Mount == "/") {
				t.Fatalf("mount %q", inst.Mount)
			}
			// Every credential a rendering, a step or an output names is
			// one of the addon's, and no value is a secret: blueprints
			// only name credentials.
			var parts [][]model.TemplatePart
			for _, v := range inst.Env {
				noSecret(t, v.Secret, v.Parts)
			}
			for _, st := range b.Provision(a, c) {
				for _, v := range st.Env {
					noSecret(t, v.Secret, v.Parts)
					parts = append(parts, v.Parts)
				}
			}
			for _, key := range b.OutputKeys() {
				for _, ctx := range []Context{c, {Env: "stage", Name: s.name}} {
					out, err := b.Outputs(a, ctx, Ref{Key: key, Fields: s.fields})
					if err != nil {
						t.Fatalf("output %s: %v", key, err)
					}
					noSecret(t, "", out)
					parts = append(parts, out)
				}
			}
			creds := b.Credentials(a)
			for _, k := range credentialKeys(inst.Env, parts...) {
				if !slices.Contains(creds, k) {
					t.Errorf("credential %q is named but not among %q", k, creds)
				}
			}
			for _, k := range creds {
				// The system secret's name must pass env_secrets' CHECK.
				if n := CredentialSecret(a.Name, k); len(n) > 127 || strings.ContainsFunc(n, func(r rune) bool {
					return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
				}) {
					t.Errorf("credential secret %q", n)
				}
			}
			// Deterministic.
			again, _ := b.Render(a, c)
			if jsonOf(t, again) != jsonOf(t, inst) {
				t.Fatal("Render is not deterministic")
			}
		})
	}
}

// noSecret fails on a secret in a blueprint's value: blueprints name
// credentials, the controller maps them to secrets.
func noSecret(t *testing.T, secret string, parts []model.TemplatePart) {
	t.Helper()
	if secret != "" || slices.ContainsFunc(parts, func(p model.TemplatePart) bool { return p.Secret != "" }) {
		t.Fatalf("a blueprint named a secret: %q %v", secret, parts)
	}
}

func TestEscapeUserinfo(t *testing.T) {
	cases := map[string]string{
		"":                                 "",
		"0123456789abcdef0123456789abcdef": "0123456789abcdef0123456789abcdef",
		"AZaz09-._~":                       "AZaz09-._~",
		"p@ss:w/rd":                        "p%40ss%3Aw%2Frd",
		"%?#[]":                            "%25%3F%23%5B%5D",
		"!$&'()*+,;=":                      "%21%24%26%27%28%29%2A%2B%2C%3B%3D",
		" ü":                               "%20%C3%BC",
	}
	for in, want := range cases {
		got := escapeUserinfo(in)
		if got != want {
			t.Errorf("escapeUserinfo(%q) = %q, want %q", in, got, want)
		}
		u, err := url.Parse("x://" + got + ":" + got + "@h/")
		if err != nil {
			t.Fatalf("%q: %v", got, err)
		}
		if pw, _ := u.User.Password(); u.User.Username() != in || pw != in {
			t.Errorf("%q reads back as %q, %q", got, u.User.Username(), pw)
		}
	}
}

func TestShellWord(t *testing.T) {
	cases := map[string]string{
		"expensa":     "expensa",
		"a_b-c.d/e:f": "a_b-c.d/e:f",
		"":            "''",
		"a b":         "'a b'",
		"it's":        `'it'\''s'`,
		"$x":          "'$x'",
		"a;b":         "'a;b'",
		"*":           "'*'",
	}
	for in, want := range cases {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestURLHost(t *testing.T) {
	cases := []struct {
		c    Context
		want string
	}{
		{Context{Name: "db", Address: "fd6b::5"}, "[fd6b::5]"},
		{Context{Name: "db", Address: "[fd6b::5]"}, "[fd6b::5]"},
		{Context{Name: "db", Address: "10.1.2.3"}, "10.1.2.3"},
		{Context{Name: "db"}, "db"},
		{Context{}, ""},
	}
	for _, c := range cases {
		if got := urlHost(c.c); got != c.want {
			t.Errorf("urlHost(%+v) = %q, want %q", c.c, got, c.want)
		}
	}
	if h := host(Context{Name: "db", Address: "fd6b::5"}); h != "fd6b::5" {
		t.Fatal(h)
	}
}

func TestOptionForms(t *testing.T) {
	// A []string (a caller building an Addon by hand) reads like a
	// JSON list.
	a := Addon{Name: "pg", Type: "postgres", Version: "17",
		Options: map[string]any{"databases": []string{"d"}, "roles": []string{"r"}}}
	if fs := (postgres{}).Validate(nil, a); len(fs) != 0 {
		t.Fatalf("findings:\n%s", findingText(fs))
	}
	if got := (postgres{}).Credentials(a); !slices.Equal(got, []string{"password", TLSKey}) {
		t.Fatal(got)
	}
	for _, v := range []any{nil, true, 1.5, "s", []any{}, map[string]any{}} {
		if describe(v) == "" {
			t.Errorf("describe(%v) is empty", v)
		}
	}
}
