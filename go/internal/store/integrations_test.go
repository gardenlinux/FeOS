// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

const testIssuer = "https://id.example/"

// An integration is registered by its name, changed by it, and unique by
// (issuer, subject); it is found by the subject of its tokens, disabled
// and enabled again, and removed with its grants.
func TestIntegrationRegistration(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "acme")
	in, created, err := s.PutIntegration(bg, store.IntegrationIn{Name: "fecode", Title: "FeCode", URL: "https://fecode.dev",
		Issuer: testIssuer, Subject: "client-1", MadeBy: "basic:ops"})
	if err != nil || !created || in.ID == uuid.Nil || in.MadeBy != "basic:ops" || in.Disabled() {
		t.Fatalf("register %+v %v %v", in, created, err)
	}
	again, created, err := s.PutIntegration(bg, store.IntegrationIn{Name: "fecode", Title: "FeCode 2", Issuer: testIssuer,
		Subject: "client-1", MadeBy: "basic:other"})
	if err != nil || created || again.ID != in.ID || again.Title != "FeCode 2" || again.MadeBy != "basic:ops" {
		t.Fatalf("change %+v %v %v", again, created, err)
	}
	if _, _, err := s.PutIntegration(bg, store.IntegrationIn{Name: "other", Issuer: testIssuer, Subject: "client-1"}); !errors.Is(err,
		store.ErrExists) {
		t.Fatalf("subject taken: %v", err)
	}
	if got, err := s.IntegrationBySubject(bg, testIssuer, "client-1"); err != nil || got.ID != in.ID {
		t.Fatalf("by subject %+v %v", got, err)
	}
	if _, err := s.IntegrationBySubject(bg, "https://other.example/", "client-1"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("other issuer: %v", err)
	}
	if _, err := s.PutIntegrationGrant(bg, "acme", in.ID, uuid.Nil, "write", "person:x"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListIntegrations(bg)
	if err != nil || len(list) != 1 || list[0].Projects != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	if d, err := s.SetIntegrationDisabled(bg, "fecode", true); err != nil || !d.Disabled() {
		t.Fatalf("disable %+v %v", d, err)
	}
	if d, err := s.SetIntegrationDisabled(bg, "fecode", false); err != nil || d.Disabled() {
		t.Fatalf("enable %+v %v", d, err)
	}
	if _, err := s.SetIntegrationDisabled(bg, "nope", true); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown: %v", err)
	}
	must(t, s.TouchIntegration(bg, in.ID))
	if got, _ := s.GetIntegration(bg, "fecode"); got.LastSeen.IsZero() {
		t.Fatal("not touched")
	}
	if ok, err := s.DeleteIntegration(bg, "fecode"); err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
	if gs, _ := s.ListGrants(bg, "acme"); len(gs) != 0 {
		t.Fatalf("grants after the removal %+v", gs)
	}
}

// An integration's grants: one per scope, changed in place, of an
// environment of the project only; read per project and for /auth/me; not
// counted among the admins a project keeps; and the queries of people and
// tokens do not see them.
func TestIntegrationGrants(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "acme")
	mkProject(t, s, "beta")
	stage := boxEnv(t, s, "acme")
	other := boxEnv(t, s, "beta")
	in, _, err := s.PutIntegration(bg, store.IntegrationIn{Name: "fecode", Issuer: testIssuer, Subject: "client-1"})
	must(t, err)
	if _, err := s.PutIntegrationGrant(bg, "acme", in.ID, uuid.Nil, "admin", "person:a"); err != nil {
		t.Fatal(err)
	}
	g, err := s.PutIntegrationGrant(bg, "acme", in.ID, uuid.Nil, "write", "person:b")
	if err != nil || g.Role != "write" || g.EnvironmentID != uuid.Nil {
		t.Fatalf("again %+v %v", g, err)
	}
	if g, err := s.PutIntegrationGrant(bg, "acme", in.ID, stage, "admin", "person:a"); err != nil || g.EnvironmentID != stage {
		t.Fatalf("on stage %+v %v", g, err)
	}
	if _, err := s.PutIntegrationGrant(bg, "acme", in.ID, other, "read", ""); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("another project's environment: %v", err)
	}
	if _, err := s.PutIntegrationGrant(bg, "nope", in.ID, uuid.Nil, "read", ""); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown project: %v", err)
	}
	roles, exists, err := s.IntegrationAccess(bg, "acme", in.ID)
	if err != nil || !exists || len(roles) != 2 {
		t.Fatalf("access %+v %v %v", roles, exists, err)
	}
	if _, exists, _ := s.IntegrationAccess(bg, "nope", in.ID); exists {
		t.Fatal("an unknown project exists")
	}
	ms, err := s.ListIntegrationGrants(bg, "acme")
	if err != nil || len(ms) != 1 || len(ms[0].Grants) != 2 || ms[0].Grants[0].Role != "write" || ms[0].Grants[0].MadeBy != "person:b" ||
		ms[0].Grants[1].EnvironmentName != "box" {
		t.Fatalf("list %+v %v", ms, err)
	}
	pg, err := s.IntegrationGrants(bg, in.ID)
	if err != nil || len(pg) != 2 || pg[0].Project != "acme" || pg[0].EnvironmentID != uuid.Nil {
		t.Fatalf("grants %+v %v", pg, err)
	}
	if n, err := s.CountAdmins(bg, "acme"); err != nil || n != 0 {
		t.Fatalf("admins %d %v", n, err)
	}
	if ms, _ := s.ListMembers(bg, "acme"); len(ms) != 0 {
		t.Fatalf("members %+v", ms)
	}
	if gs, err := s.ListGrants(bg, "acme"); err != nil || len(gs) != 2 || gs[0].PersonID != uuid.Nil || gs[0].TokenHash != "" {
		t.Fatalf("grants of the project %+v %v", gs, err)
	}
	if ok, err := s.DeleteIntegrationGrantAt(bg, "acme", in.ID, stage); err != nil || !ok {
		t.Fatalf("delete at %v %v", ok, err)
	}
	if n, err := s.DeleteIntegrationGrants(bg, "acme", in.ID); err != nil || n != 1 {
		t.Fatalf("delete all %d %v", n, err)
	}
	// The principals' names of made_by.
	p, err := s.UpsertPerson(bg, testIssuer, "ada", "ada@example", "Ada")
	must(t, err)
	names, err := s.PrincipalNames(bg, []string{"person:" + p.ID.String(), "integration:" + in.ID.String(),
		"integration:" + in.ID.String() + "/person:" + p.ID.String(), "token:ab", "basic:ops", "person:" + uuid.NewString()})
	if err != nil || len(names) != 3 || names["person:"+p.ID.String()] != "Ada" || names["integration:"+in.ID.String()] != "fecode" ||
		names["integration:"+in.ID.String()+"/person:"+p.ID.String()] != "Ada" {
		t.Fatalf("names %v %v", names, err)
	}
	if got, err := s.PersonBySubject(bg, testIssuer, "ada"); err != nil || got.ID != p.ID {
		t.Fatalf("person by subject %+v %v", got, err)
	}
}

// An environment is protected and lifted; the protected set of a project
// names it, not one being deleted; the listing of environments carries it.
func TestEnvironmentProtected(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "acme")
	env := boxEnv(t, s, "acme")
	e, changed, err := s.SetEnvironmentProtected(bg, env, true)
	if err != nil || !changed || !e.Protected {
		t.Fatalf("protect %+v %v %v", e, changed, err)
	}
	if _, changed, err := s.SetEnvironmentProtected(bg, env, true); err != nil || changed {
		t.Fatalf("again %v %v", changed, err)
	}
	if _, _, err := s.SetEnvironmentProtected(bg, uuid.New(), true); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown: %v", err)
	}
	if ids, err := s.ProtectedEnvironments(bg, "acme"); err != nil || len(ids) != 1 || ids[0] != env {
		t.Fatalf("protected %v %v", ids, err)
	}
	if rows, err := s.EnvSummaries(bg, "acme", nil); err != nil || len(rows) != 1 || !rows[0].Protected {
		t.Fatalf("summaries %+v %v", rows, err)
	}
	if es, err := s.ProjectEnvironments(bg, []string{"acme", "beta"}); err != nil || len(es) != 1 || !es[0].Protected ||
		es[0].Name != "box" {
		t.Fatalf("project environments %+v %v", es, err)
	}
	exec(t, s, "UPDATE environments SET deleting = true WHERE id = $1", env)
	if ids, _ := s.ProtectedEnvironments(bg, "acme"); len(ids) != 0 {
		t.Fatalf("a deleting one is protected: %v", ids)
	}
}

// A route or sandbox token an integration minted resolves while the
// integration is enabled and holds write on its environment by its own
// grants, protection counted: the grant lowered, removed, the integration
// disabled or removed, or the environment protected under a project write
// all refuse it; a token an operator by Basic minted is never touched
// (TestTokensOfProjectWriteStopOnProtection in api has the other minters).
func TestIntegrationTokens(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "acme")
	env := boxEnv(t, s, "acme")
	sb := mkSandboxIn(t, s, "acme", env, podid.Mint())
	in, _, err := s.PutIntegration(bg, store.IntegrationIn{Name: "fecode", Issuer: testIssuer, Subject: "client-1"})
	must(t, err)
	_, err = s.MintRouteToken(bg, "r-int", env, "integration:"+in.ID.String(), in.ID)
	must(t, err)
	_, err = s.InsertRouteToken(bg, "r-plain", env, "basic:ops")
	must(t, err)
	_, err = s.MintSandboxToken(bg, "s-int", sb.ID, "a", "integration:"+in.ID.String(), in.ID)
	must(t, err)
	_, err = s.CreateSandboxToken(bg, "s-plain", sb.ID, "b", "basic:ops")
	must(t, err)
	good := func(want bool, what string) {
		t.Helper()
		_, rerr := s.ResolveRouteToken(bg, "r-int")
		_, _, serr := s.ResolveSandboxToken(bg, "s-int")
		if (rerr == nil) != want || (serr == nil) != want {
			t.Fatalf("%s: route %v, sandbox %v; want good %v", what, rerr, serr, want)
		}
		if rerr != nil && !errors.Is(rerr, store.ErrNoRow) || serr != nil && !errors.Is(serr, model.ErrUnknownToken) {
			t.Fatalf("%s: route %v, sandbox %v", what, rerr, serr)
		}
		if _, err := s.ResolveRouteToken(bg, "r-plain"); err != nil {
			t.Fatalf("%s: the plain route token: %v", what, err)
		}
		if _, _, err := s.ResolveSandboxToken(bg, "s-plain"); err != nil {
			t.Fatalf("%s: the plain sandbox token: %v", what, err)
		}
	}
	good(false, "no grant")
	_, err = s.PutIntegrationGrant(bg, "acme", in.ID, uuid.Nil, "write", "")
	must(t, err)
	good(true, "project write")
	_, _, err = s.SetEnvironmentProtected(bg, env, true)
	must(t, err)
	good(false, "project write, protected")
	_, err = s.PutIntegrationGrant(bg, "acme", in.ID, env, "write", "")
	must(t, err)
	good(true, "environment write, protected")
	_, err = s.PutIntegrationGrant(bg, "acme", in.ID, env, "read", "")
	must(t, err)
	_, err = s.PutIntegrationGrant(bg, "acme", in.ID, uuid.Nil, "admin", "")
	must(t, err)
	good(true, "project admin, protected")
	_, err = s.PutIntegrationGrant(bg, "acme", in.ID, uuid.Nil, "read", "")
	must(t, err)
	good(false, "lowered to read")
	_, err = s.PutIntegrationGrant(bg, "acme", in.ID, env, "admin", "")
	must(t, err)
	good(true, "environment admin")
	_, err = s.SetIntegrationDisabled(bg, "fecode", true)
	must(t, err)
	good(false, "disabled")
	_, err = s.SetIntegrationDisabled(bg, "fecode", false)
	must(t, err)
	good(true, "enabled again")
	_, err = s.DeleteIntegration(bg, "fecode")
	must(t, err)
	good(false, "removed")
	var n int
	if err := s.Q().QueryRow(bg, `SELECT (SELECT count(*) FROM env_route_tokens) + (SELECT count(*) FROM sandbox_tokens)`).Scan(&n); err != nil ||
		n != 2 {
		t.Fatalf("tokens left %d %v", n, err)
	}
}
