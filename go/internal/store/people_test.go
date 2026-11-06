// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A sign-in upserts its person by issuer and subject; a disabled person's
// sessions go and resolve no more, and enabling keeps the grants.
func TestPeople(t *testing.T) {
	s, _ := newStore(t)
	p, err := s.UpsertPerson(bg, "https://id", "s1", "a@x", "A")
	if err != nil || p.ID == uuid.Nil || p.Disabled() {
		t.Fatalf("upsert %+v %v", p, err)
	}
	again, err := s.UpsertPerson(bg, "https://id", "s1", "b@x", "B")
	if err != nil || again.ID != p.ID || again.Email != "b@x" || again.Name != "B" || !again.FirstSeen.Equal(p.FirstSeen) {
		t.Fatalf("again %+v %v", again, err)
	}
	other, err := s.UpsertPerson(bg, "https://other", "s1", "", "")
	if err != nil || other.ID == p.ID {
		t.Fatalf("other issuer %+v %v", other, err)
	}

	mkTenant(t, s, "acme")
	if _, err := s.PutGrant(bg, store.Grant{TenantID: "acme", PersonID: p.ID, Role: "write"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertUISession(bg, store.UISession{IDHash: "h1", Sub: "s1", PersonID: p.ID, SID: "sid1"},
		time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UISessionOf(bg, "h1"); err != nil || got.PersonID != p.ID || got.SID != "sid1" {
		t.Fatalf("session %+v %v", got, err)
	}
	// A disabled person's session resolves no more, even before it is
	// deleted.
	exec(t, s, "UPDATE people SET disabled_at = now() WHERE id = $1", p.ID)
	if _, err := s.UISessionOf(bg, "h1"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("disabled: %v", err)
	}
	exec(t, s, "UPDATE people SET disabled_at = NULL WHERE id = $1", p.ID)
	d, err := s.SetPersonDisabled(bg, p.ID, true)
	if err != nil || !d.Disabled() {
		t.Fatalf("disable %+v %v", d, err)
	}
	var n int
	must(t, s.Q().QueryRow(bg, "SELECT count(*) FROM ui_sessions").Scan(&n))
	if n != 0 {
		t.Fatalf("%d sessions after disabling", n)
	}
	if d, err := s.SetPersonDisabled(bg, p.ID, false); err != nil || d.Disabled() {
		t.Fatalf("enable %+v %v", d, err)
	}
	if roles, ok, err := s.PersonAccess(bg, "acme", p.ID); err != nil || !ok || len(roles) != 1 || roles[0].Role != "write" {
		t.Fatalf("access %+v %v %v", roles, ok, err)
	}
	if _, err := s.SetPersonDisabled(bg, uuid.New(), true); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown: %v", err)
	}
}

// A back-channel logout ends the sessions of its sid, on every instance:
// the rows go, with sub only that subject's.
func TestDeleteUISessionsBySID(t *testing.T) {
	s, _ := newStore(t)
	for _, u := range []store.UISession{{IDHash: "a", Sub: "s1", SID: "x"}, {IDHash: "b", Sub: "s1", SID: "x"},
		{IDHash: "c", Sub: "s2", SID: "x"}, {IDHash: "d", Sub: "s1", SID: "y"}, {IDHash: "e", Sub: "s1"}} {
		if _, err := s.InsertUISession(bg, u, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.DeleteUISessionsBySID(bg, "x", "s1"); err != nil || n != 2 {
		t.Fatalf("x s1: %d %v", n, err)
	}
	if n, err := s.DeleteUISessionsBySID(bg, "x", ""); err != nil || n != 1 {
		t.Fatalf("x: %d %v", n, err)
	}
	if n, err := s.DeleteUISessionsBySID(bg, "", ""); err != nil || n != 0 {
		t.Fatalf("empty: %d %v", n, err)
	}
}

// Grants: one per principal and scope (a second put changes the role),
// exactly one principal, an environment of the tenant only; an
// environment's grants go with it, every grant with its tenant.
func TestGrants(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkTenant(t, s, "other")
	p, err := s.UpsertPerson(bg, "https://id", "s1", "", "")
	must(t, err)
	stage, err := s.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "acme", Name: "stage"})
	must(t, err)
	foreign, err := s.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "other", Name: "prod"})
	must(t, err)

	g1, err := s.PutGrant(bg, store.Grant{TenantID: "acme", PersonID: p.ID, Role: "read", MadeBy: "basic:op"})
	must(t, err)
	g2, err := s.PutGrant(bg, store.Grant{TenantID: "acme", PersonID: p.ID, Role: "admin"})
	if err != nil || g2.ID != g1.ID || g2.Role != "admin" || g2.MadeBy != "" {
		t.Fatalf("second put %+v %v", g2, err)
	}
	if _, err := s.PutGrant(bg, store.Grant{TenantID: "acme", PersonID: p.ID, EnvironmentID: stage.ID,
		Role: "write"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutGrant(bg, store.Grant{TenantID: "acme", PersonID: p.ID, EnvironmentID: foreign.ID,
		Role: "write"}); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("foreign environment: %v", err)
	}
	if _, err := s.PutGrant(bg, store.Grant{TenantID: "acme", PersonID: uuid.New(), Role: "read"}); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown person: %v", err)
	}
	if _, err := s.PutGrant(bg, store.Grant{TenantID: "acme", Role: "read"}); err == nil {
		t.Fatal("a grant without a principal")
	}
	gs, err := s.ListGrants(bg, "acme")
	if err != nil || len(gs) != 2 || gs[0].EnvironmentID != uuid.Nil || gs[1].EnvironmentID != stage.ID {
		t.Fatalf("list %+v %v", gs, err)
	}
	mine, err := s.PersonGrants(bg, p.ID)
	if err != nil || len(mine) != 2 || mine[1].EnvironmentName != "stage" {
		t.Fatalf("person grants %+v %v", mine, err)
	}
	if _, err := s.DeleteEnvironment(bg, stage.ID); err != nil {
		t.Fatal(err)
	}
	if gs, _ := s.ListGrants(bg, "acme"); len(gs) != 1 {
		t.Fatalf("after the environment's deletion: %+v", gs)
	}
	if ok, err := s.DeleteGrant(bg, "other", g1.ID); err != nil || ok {
		t.Fatalf("another tenant's delete: %v %v", ok, err)
	}
	if _, ok, err := s.PersonAccess(bg, "nope", p.ID); err != nil || ok {
		t.Fatalf("unknown tenant: %v %v", ok, err)
	}
}

// A tenant token is admin on its tenant unless minted with a role and a
// scope; its grants add to its own; a scope of another tenant is refused.
func TestTokenAccess(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkTenant(t, s, "other")
	stage, err := s.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "acme", Name: "stage"})
	must(t, err)
	foreign, err := s.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "other", Name: "prod"})
	must(t, err)
	if tk, err := s.CreateTenantToken(bg, "h1", "acme", "fecode"); err != nil || tk.Role != "admin" ||
		tk.EnvironmentID != uuid.Nil {
		t.Fatalf("create %+v %v", tk, err)
	}
	// A token an instance of the previous version mints is admin too.
	exec(t, s, "INSERT INTO tenant_tokens (hash, tenant_id) VALUES ('h0', 'acme')")
	tenant, roles, err := s.TokenAccess(bg, "h0")
	if err != nil || tenant != "acme" || len(roles) != 1 || roles[0] != (store.RoleAt{Role: "admin"}) {
		t.Fatalf("old token %q %+v %v", tenant, roles, err)
	}
	if _, err := s.InsertTenantToken(bg, store.TenantToken{Hash: "h2", TenantID: "acme", Role: "read",
		EnvironmentID: stage.ID, MadeBy: "basic:op"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTenantToken(bg, store.TenantToken{Hash: "h3", TenantID: "acme", Role: "read",
		EnvironmentID: foreign.ID}); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("foreign scope: %v", err)
	}
	if _, err := s.PutGrant(bg, store.Grant{TenantID: "acme", TokenHash: "h2", Role: "read"}); err != nil {
		t.Fatal(err)
	}
	_, roles, err = s.TokenAccess(bg, "h2")
	if err != nil || len(roles) != 2 || roles[0] != (store.RoleAt{Role: "read", EnvironmentID: stage.ID}) ||
		roles[1] != (store.RoleAt{Role: "read"}) {
		t.Fatalf("scoped %+v %v", roles, err)
	}
	if _, _, err := s.TokenAccess(bg, "nope"); !errors.Is(err, model.ErrUnknownToken) {
		t.Fatalf("unknown: %v", err)
	}
	// The scope's environment takes its tokens along.
	if _, err := s.DeleteEnvironment(bg, stage.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TokenAccess(bg, "h2"); !errors.Is(err, model.ErrUnknownToken) {
		t.Fatalf("after the environment's deletion: %v", err)
	}
	if id, err := s.EnvironmentRef(bg, "other", "prod"); err != nil || id != foreign.ID {
		t.Fatalf("ref by name %v %v", id, err)
	}
	if id, err := s.EnvironmentRef(bg, "other", foreign.ID.String()); err != nil || id != foreign.ID {
		t.Fatalf("ref by id %v %v", id, err)
	}
	if _, err := s.EnvironmentRef(bg, "acme", foreign.ID.String()); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("foreign ref: %v", err)
	}
}
