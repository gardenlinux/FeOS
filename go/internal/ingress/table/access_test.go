// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package table

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/visit"
)

// guarded is baseEnv admitting the visitors of provider (of kind, at
// url), signing in at signIn.
func guarded(provider, kind, url, signIn string) EnvRows {
	e := baseEnv(1, 1, ready("a0000000001", "web", 1, 8080))
	e.Access, e.Provider, e.ProviderKind, e.ProviderURL, e.SignIn = model.AccessVisitors, provider, kind, url, signIn
	return e
}

// An environment open to anyone carries none of Access, Provider and
// SignIn: its coded entry and hash are byte for byte what they were before
// ADR 0054, whether its access reads "anyone" or "". A guarded one carries
// them, and they are in its hash.
func TestBuildEnvAccess(t *testing.T) {
	specs := specsOf(map[int]*spec.Spec{1: deployment1()})
	before := baseEnv(1, 1, ready("a0000000001", "web", 1, 8080))
	old, _ := build(t, "apps.example", before, specs)
	anyone := before
	anyone.Access = model.AccessAnyone
	// Fields of an open environment's provider are ignored.
	anyone.Provider, anyone.ProviderKind, anyone.ProviderURL, anyone.SignIn = "fecode", "tenant", "https://x.example/", "https://x.example/"
	open, _ := build(t, "apps.example", anyone, specs)
	ob, err := wire.Codec{}.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	nb, err := wire.Codec{}.Marshal(open)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ob, nb) || open.Hash != old.Hash {
		t.Fatalf("anyone is not as before:\n%s\n%s", ob, nb)
	}
	if bytes.Contains(nb, []byte(`"access"`)) || bytes.Contains(nb, []byte(`"provider"`)) || bytes.Contains(nb, []byte(`"signIn"`)) {
		t.Fatalf("an open environment codes access fields: %s", nb)
	}

	for _, tc := range []struct {
		name                     string
		e                        EnvRows
		access, provider, signIn string
	}{
		{"kind tenant", guarded("fecode", model.ProviderKindTenant, "https://fecode.example/api/v1/visit", "https://fecode.example/api/v1/visit"),
			wire.AccessVisitors, "fecode", "https://fecode.example/api/v1/visit"},
		{"ferun with ferun-auth", guarded("", "", "", "https://auth.example/visit"), wire.AccessVisitors, model.ProviderFerun,
			"https://auth.example/visit"},
		{"ferun without ferun-auth", guarded("", "", "", ""), wire.AccessVisitors, model.ProviderFerun, ""},
		{"a missing provider row", guarded("gone", "", "", ""), wire.AccessVisitors, "gone", ""},
		// A value the table does not know goes as it is: the ingress
		// admits nobody by it.
		{"an unknown access", func() EnvRows { e := guarded("", "", "", ""); e.Access = "members"; return e }(), "members",
			model.ProviderFerun, ""},
	} {
		got, _, err := BuildEnv(model.Zones{Apps: "apps.example"}, tc.e, specs)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Access != tc.access || got.Provider != tc.provider || got.SignIn != tc.signIn {
			t.Errorf("%s: access %q provider %q signIn %q", tc.name, got.Access, got.Provider, got.SignIn)
		}
		if got.Hash == old.Hash {
			t.Errorf("%s: the hash of an open environment", tc.name)
		}
		if h, _ := Hash(got); h != got.Hash {
			t.Errorf("%s: hash %s, recomputed %s", tc.name, got.Hash, h)
		}
	}
}

// A guarded environment keeps its access with its domains, also when one
// of them does not fit and is left out.
func TestBuildEnvAccessDomains(t *testing.T) {
	e := guarded("fecode", model.ProviderKindTenant, "https://fecode.example/v", "https://fecode.example/v")
	e.Domains = []wire.Domain{domain("big.example", MaxDomainBytes, 1), domain("shop.example", 1024, 2)}
	got, _ := build(t, "apps.example", e, specsOf(map[int]*spec.Spec{1: deployment1()}))
	if got.Access != wire.AccessVisitors || got.Provider != "fecode" || got.SignIn != "https://fecode.example/v" ||
		len(got.Domains) != 1 || got.Domains[0].Name != "shop.example" {
		t.Fatalf("env %+v", got)
	}
}

// SignIn: kind tenant signs in at its URL; ferun and kind oidc at
// ferun-auth's /visit, and nowhere without ferun-auth; a provider whose
// row is missing nowhere; an open environment nowhere.
func TestSignIn(t *testing.T) {
	const auth = "https://auth.example"
	for _, tc := range []struct {
		name    string
		e       EnvRows
		authURL string
		want    string
	}{
		{"tenant", guarded("fecode", model.ProviderKindTenant, "https://fecode.example/api/v1/visit", ""), auth,
			"https://fecode.example/api/v1/visit"},
		{"tenant without ferun-auth", guarded("fecode", model.ProviderKindTenant, "https://fecode.example/api/v1/visit", ""), "",
			"https://fecode.example/api/v1/visit"},
		{"ferun", guarded("", "", "", ""), auth, auth + "/visit"},
		{"ferun by name", guarded(model.ProviderFerun, "", "", ""), auth, auth + "/visit"},
		{"ferun without ferun-auth", guarded("", "", "", ""), "", ""},
		{"oidc", guarded("sso", model.ProviderKindOIDC, "", ""), auth, auth + "/visit"},
		{"oidc without ferun-auth", guarded("sso", model.ProviderKindOIDC, "", ""), "", ""},
		{"a missing provider row", guarded("gone", "", "", ""), auth, ""},
		{"anyone", baseEnv(1, 1), auth, ""},
	} {
		if got := SignIn(tc.e, tc.authURL); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Group carries who an environment admits from its rows.
func TestGroupAccess(t *testing.T) {
	id := uuid.New()
	rows := []store.IngressRoutingRow{{EnvID: id, Owner: "acme", Name: "a", HostLabel: "a-acme", Deployment: 1,
		Access: model.AccessVisitors, AccessProvider: "fecode", ProviderKind: model.ProviderKindTenant, ProviderURL: "https://f.example/v"}}
	want := []EnvRows{{ID: id, Tenant: "acme", Name: "a", HostLabel: "a-acme", Deployment: 1,
		Access: model.AccessVisitors, Provider: "fecode", ProviderKind: model.ProviderKindTenant, ProviderURL: "https://f.example/v"}}
	if got := Group(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// testKeys are visit keys of a fixed seed.
func testKeys(t *testing.T) *visit.Keys {
	t.Helper()
	k, err := visit.NewKeys(bytes.Repeat([]byte{7}, 32), bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The builder: a guarded environment's entry signs in where its provider
// says (kind tenant: its URL; ferun: AuthURL's /visit); the keys of visits
// are left out while VisitKeys fails, come with the next full read once it
// succeeds, and are kept from then on, VisitKeys not called again.
func TestBuilderVisits(t *testing.T) {
	f := newFixture(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.d.Pool.Exec(bg, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec("INSERT INTO access_providers (tenant, name, kind, url) VALUES ('acme', 'fecode', 'tenant', 'https://fecode.example/api/v1/visit')")
	exec("UPDATE environments SET access = 'visitors', access_provider = 'fecode' WHERE id = $1", f.env.ID)
	ferun := f.mkEnv(t, "ferun", webSpec)
	exec("UPDATE environments SET access = 'visitors' WHERE id = $1", ferun.ID)
	open := f.mkEnv(t, "open", webSpec)

	keys := testKeys(t)
	var calls atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	b := f.builder(t, func(b *Builder) {
		b.AuthURL = "https://auth.example"
		b.VisitKeys = func(context.Context) (*visit.Keys, error) {
			calls.Add(1)
			if failing.Load() {
				return nil, errors.New("the database is away")
			}
			return keys, nil
		}
	})
	s, _ := use(t, b)
	if s.VisitKeys != nil || !s.Healthy {
		t.Fatalf("keys while VisitKeys fails: %+v, healthy %v", s.VisitKeys, s.Healthy)
	}
	if e := envOf(s, f.env.ID); e.Access != wire.AccessVisitors || e.Provider != "fecode" ||
		e.SignIn != "https://fecode.example/api/v1/visit" {
		t.Fatalf("kind tenant: %+v", e)
	}
	if e := envOf(s, ferun.ID); e.Access != wire.AccessVisitors || e.Provider != model.ProviderFerun ||
		e.SignIn != "https://auth.example/visit" {
		t.Fatalf("ferun: %+v", e)
	}
	if e := envOf(s, open.ID); e.Access != "" || e.Provider != "" || e.SignIn != "" {
		t.Fatalf("open: %+v", e)
	}

	failing.Store(false)
	exec("SELECT pg_notify($1, '*')", db.ChanIngress)
	s = await(t, b, 2*time.Second, "the keys", func(s *Snapshot) bool { return s.VisitKeys != nil })
	want := wire.VisitKeys{TokenKID: keys.TokenKID(), TokenKey: keys.TokenKey(), ChallengeKID: keys.ChallengeKID(),
		ChallengeKey: keys.ChallengeKey()}
	if !SameVisitKeys(s.VisitKeys, &want) || len(want.TokenKID) != 16 || len(want.TokenKey) != 32 || len(want.ChallengeKey) != 32 {
		t.Fatalf("keys %+v, want %+v", s.VisitKeys, want)
	}
	n := calls.Load()

	// Kept: a change of an environment and another full read carry them,
	// without asking again.
	exec("UPDATE environments SET access = 'anyone', access_provider = NULL WHERE id = $1", ferun.ID)
	exec("SELECT pg_notify($1, $2)", db.ChanIngress, "env:"+ferun.ID.String())
	s = await(t, b, 2*time.Second, "ferun's environment open", func(s *Snapshot) bool { return envOf(s, ferun.ID).Access == "" })
	if s.VisitKeys == nil {
		t.Fatal("a partial read lost the keys")
	}
	exec("SELECT pg_notify($1, '*')", db.ChanIngress)
	time.Sleep(200 * time.Millisecond)
	if s := b.Current(); s.VisitKeys == nil || calls.Load() != n {
		t.Fatalf("after a full read: keys %v, calls %d, want %d", s.VisitKeys, calls.Load(), n)
	}
}

// SameVisitKeys compares every field, and none with none.
func TestSameVisitKeys(t *testing.T) {
	a := &wire.VisitKeys{TokenKID: "t", TokenKey: []byte{1}, ChallengeKID: "c", ChallengeKey: []byte{2}}
	same := &wire.VisitKeys{TokenKID: "t", TokenKey: []byte{1}, ChallengeKID: "c", ChallengeKey: []byte{2}}
	if !SameVisitKeys(a, same) || !SameVisitKeys(nil, nil) || SameVisitKeys(a, nil) || SameVisitKeys(nil, a) {
		t.Fatal("same")
	}
	for _, o := range []wire.VisitKeys{
		{TokenKID: "x", TokenKey: []byte{1}, ChallengeKID: "c", ChallengeKey: []byte{2}},
		{TokenKID: "t", TokenKey: []byte{3}, ChallengeKID: "c", ChallengeKey: []byte{2}},
		{TokenKID: "t", TokenKey: []byte{1}, ChallengeKID: "x", ChallengeKey: []byte{2}},
		{TokenKID: "t", TokenKey: []byte{1}, ChallengeKID: "c", ChallengeKey: []byte{3}},
	} {
		if SameVisitKeys(a, &o) {
			t.Errorf("%+v is the same", o)
		}
	}
}
