// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// deletingDomains is boundDomains whose DeleteDomain succeeds and counts.
type deletingDomains struct {
	boundDomains
	deleted *atomic.Int32
}

func (d deletingDomains) DeleteDomain(context.Context, string, string) error {
	d.deleted.Add(1)
	return nil
}

// Deleting a domain unbinds it, so it needs admin on the environment it is
// bound to, as unbinding does: an integration that names nobody, admin of
// the project, deletes no domain of a protected environment (ADR 0060);
// with an admin named, and a person who is admin of the project, it goes.
func TestDeleteDomainOfProtectedEnvironment(t *testing.T) {
	t.Parallel()
	bound, n := new(string), new(atomic.Int32)
	w := newPeopleWorld(t, envOptions{domains: func(*db.DB, *store.Store) Domains {
		return deletingDomains{boundDomains{env: bound}, n}
	}})
	*bound = w.prod.String()
	if _, _, err := w.e.st.SetEnvironmentProtected(context.Background(), w.prod, true); err != nil {
		t.Fatal(err)
	}
	w.providerPerson(t, "boss", store.Grant{Role: "admin"})
	admin := w.person(t, "admin", false, store.Grant{Role: "admin"})
	jwt := w.integration(t, "ci", store.Grant{Role: "admin"})

	resp := w.e.serve(asIntegration("DELETE", "/api/v1/domains/d1", jwt, testProject, "", nil))
	expect(t, resp, http.StatusForbidden, "")
	if c := codeOf(t, resp); c != "role_required" || n.Load() != 0 {
		t.Fatalf("integration alone: code %q, deleted %d times", c, n.Load())
	}
	expect(t, w.e.serve(asIntegration("DELETE", "/api/v1/domains/d1", jwt, testProject, "boss", nil)),
		http.StatusNoContent, "")
	expect(t, w.e.serve(asSession("DELETE", "/api/v1/domains/d1", admin, nil)), http.StatusNoContent, "")
	if n.Load() != 2 {
		t.Fatalf("deleted %d times, want 2", n.Load())
	}
}

// On a protected environment a project's write gives no sandbox and no
// route token (ADR 0060): those a person or a project token minted there
// by that write before stop, and work again once it is unprotected; those
// of a minter with write on the environment itself or admin on the
// project, or of an operator by Basic, go on.
func TestTokensOfProjectWriteStopOnProtection(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	ctx := context.Background()
	writer := w.person(t, "writer", false, store.Grant{Role: "write"})
	envWriter := w.person(t, "envwriter", false, store.Grant{Role: "write"},
		store.Grant{Role: "write", EnvironmentID: w.prod})
	admin := w.person(t, "admin", false, store.Grant{Role: "admin"})
	ptok := w.token(t, "write", uuid.Nil)
	sb := w.podIn(t, "POST /api/v1/sandboxes/{id}/tokens", w.prod)

	path := "/api/v1/sandboxes/" + sb + "/tokens"
	mint := func(rq request) string {
		t.Helper()
		resp := w.e.serve(rq)
		expect(t, resp, http.StatusCreated, "")
		return decode[struct{ Token string }](t, resp).Token
	}
	byToken := func(tok, label string) request {
		return request{method: "POST", path: path, body: map[string]string{"label": label},
			header: http.Header{"Authorization": {bearerOf(tok)}}}
	}
	sandboxTokens := []struct {
		who   string
		tok   string
		stops bool
	}{
		{"project writer", mint(asSession("POST", path, writer, map[string]string{"label": "w"})), true},
		{"project token with write", mint(byToken(ptok, "t")), true},
		{"writer of the environment", mint(asSession("POST", path, envWriter, map[string]string{"label": "e"})), false},
		{"admin of the project", mint(asSession("POST", path, admin, map[string]string{"label": "a"})), false},
		{"admin token", mint(byToken(w.e.projectToken, "at")), false},
		{"operator by Basic", "operator's sandbox token", false},
	}
	if _, err := w.e.st.CreateSandboxToken(ctx, store.HashToken(sandboxTokens[len(sandboxTokens)-1].tok), podid.ID(sb),
		"o", "basic:ops"); err != nil {
		t.Fatal(err)
	}
	routeTokens := map[string]bool{"person:" + w.persons["writer"].String(): true,
		"person:" + w.persons["envwriter"].String(): false, "": true}
	for by := range routeTokens {
		if _, err := w.e.st.InsertRouteToken(ctx, store.HashToken("route "+by), w.prod, by); err != nil {
			t.Fatal(err)
		}
	}
	check := func(protected bool) {
		t.Helper()
		for _, c := range sandboxTokens {
			_, err := ResolveSandboxToken(ctx, w.e.st.Q(), c.tok)
			if refused := err != nil; refused != (protected && c.stops) {
				t.Errorf("protected %v: sandbox token of the %s: resolve err %v", protected, c.who, err)
			}
		}
		for by, stops := range routeTokens {
			_, err := w.e.st.ResolveRouteToken(ctx, store.HashToken("route "+by))
			if refused := err != nil; refused != (protected && stops) {
				t.Errorf("protected %v: route token by %q: resolve err %v", protected, by, err)
			}
		}
	}
	check(false)
	if _, _, err := w.e.st.SetEnvironmentProtected(ctx, w.prod, true); err != nil {
		t.Fatal(err)
	}
	check(true)
	// The sandbox class answers a stopped token as one it does not know.
	dirs := func(tok string) *httptest.ResponseRecorder {
		return w.e.serve(request{method: "GET", path: "/api/v1/sandboxes/" + sb + "/dirs/",
			header: http.Header{"Authorization": {bearerOf(tok)}}})
	}
	stopped, unknown := dirs(sandboxTokens[0].tok), dirs("no-such-token")
	if stopped.Code != unknown.Code || stopped.Body.String() != unknown.Body.String() {
		t.Errorf("the sandbox class takes a stopped token: %d %s, unknown %d %s", stopped.Code, stopped.Body.String(),
			unknown.Code, unknown.Body.String())
	}
	if _, _, err := w.e.st.SetEnvironmentProtected(ctx, w.prod, false); err != nil {
		t.Fatal(err)
	}
	check(false)
}

// An integration that names a person gives up its own grants only where
// that person is an admin, as they would to remove them themselves: a
// reader of the project takes no admin's integration out of it (ADR
// 0056). Naming nobody, it gives them up as it likes.
func TestIntegrationGivesUpGrantsForAnAdminOnly(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	w.providerPerson(t, "reader", store.Grant{Role: "read"})
	w.providerPerson(t, "stageadmin", store.Grant{Role: "read"}, store.Grant{Role: "admin", EnvironmentID: w.stage})
	w.providerPerson(t, "boss", store.Grant{Role: "admin"})
	jwt := w.integration(t, "fecode", store.Grant{Role: "admin"},
		store.Grant{Role: "admin", EnvironmentID: w.stage}, store.Grant{Role: "admin", EnvironmentID: w.prod})
	grants := "/api/v1/project/integrations/fecode/grants"

	for _, rq := range []request{
		asIntegration("DELETE", grants+"?environment=stage", jwt, testProject, "reader", nil),
		asIntegration("DELETE", grants, jwt, testProject, "reader", nil),
		asIntegration("DELETE", grants, jwt, testProject, "stageadmin", nil),
		asIntegration("POST", "/api/v1/project/leave", jwt, testProject, "reader", nil),
		asIntegration("POST", "/api/v1/project/leave", jwt, testProject, "stageadmin", nil),
	} {
		resp := w.e.serve(rq)
		expect(t, resp, http.StatusForbidden, "")
		if c := codeOf(t, resp); c != "role_required" {
			t.Errorf("%s %s for %s: code %q", rq.method, rq.path, rq.header.Get(personHeader), c)
		}
	}
	expect(t, w.e.serve(asIntegration("GET", "/api/v1/project", jwt, testProject, "", nil)), http.StatusOK, "")

	expect(t, w.e.serve(asIntegration("DELETE", grants+"?environment=stage", jwt, testProject, "stageadmin", nil)),
		http.StatusOK, "{\"deleted\":true}\n")
	expect(t, w.e.serve(asIntegration("DELETE", grants+"?environment=prod", jwt, testProject, "", nil)),
		http.StatusOK, "{\"deleted\":true}\n")
	expect(t, w.e.serve(asIntegration("POST", "/api/v1/project/leave", jwt, testProject, "boss", nil)),
		http.StatusOK, "{\"left\":true}\n")
	expect(t, w.e.serve(asIntegration("GET", "/api/v1/project", jwt, testProject, "", nil)), http.StatusNotFound, "")
}

// A disabled integration reaches nothing and learns no more of the person
// it names than the subject it sent.
func TestDisabledIntegrationLearnsNoPerson(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	w.providerPerson(t, "someone", store.Grant{Role: "read"})
	jwt := w.integration(t, "old", store.Grant{Role: "read"})
	resp := w.e.serve(asIntegration("GET", "/auth/me", jwt, "", "someone", nil))
	expect(t, resp, http.StatusOK, "")
	if !strings.Contains(resp.Body.String(), "someone@example.test") {
		t.Fatalf("enabled: the person is not told: %s", resp.Body.String())
	}
	expect(t, w.e.do("POST", "/api/v1/admin/integrations/old/disable", adminAuth(), nil), http.StatusOK, "")
	resp = w.e.serve(asIntegration("GET", "/auth/me", jwt, "", "someone", nil))
	expect(t, resp, http.StatusOK, "")
	var me struct {
		Named    json.RawMessage
		Projects []json.RawMessage
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if string(me.Named) != `{"sub":"someone","known":false}` || len(me.Projects) != 0 {
		t.Errorf("disabled: named %s, projects %d", me.Named, len(me.Projects))
	}
}

// The protect dialog's principals name the project's tokens to a person,
// not to an integration, which may not list them.
func TestPrincipalsNameNoTokenToAnIntegration(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	w.token(t, "write", uuid.Nil)
	w.providerPerson(t, "envadmin", store.Grant{Role: "admin", EnvironmentID: w.stage})
	envAdmin := w.person(t, "envadmin-session", false, store.Grant{Role: "admin", EnvironmentID: w.stage})
	jwt := w.integration(t, "fecode", store.Grant{Role: "admin", EnvironmentID: w.stage})
	path := "/api/v1/environments/stage/principals"
	kinds := func(resp *httptest.ResponseRecorder) map[string]int {
		t.Helper()
		expect(t, resp, http.StatusOK, "")
		out := map[string]int{}
		for _, p := range decode[struct{ Principals []struct{ Kind string } }](t, resp).Principals {
			out[p.Kind]++
		}
		return out
	}
	if k := kinds(w.e.serve(asIntegration("GET", path, jwt, testProject, "envadmin", nil))); k["token"] != 0 ||
		k["person"] == 0 || k["integration"] != 1 {
		t.Errorf("integration: %v", k)
	}
	if k := kinds(w.e.serve(asSession("GET", path, envAdmin, nil))); k["token"] != 2 {
		t.Errorf("person: %v", k)
	}
}
