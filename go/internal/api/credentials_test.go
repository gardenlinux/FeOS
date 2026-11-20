// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/bindings"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The credentials' routes act in the environment the path names, by name
// or ID, of the principal's tenant only (ADR 0033); the tenant's routes
// of before are gone.
func TestCredentialRoutesResolveTheEnvironment(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{bindings: &fakeBindings{on: true}})
	id := e.mkEnv(testTenant, "prod")
	mustTenant(t, e, "other")
	foreign := e.mkEnv("other", "prod")
	authz := bearerOf(e.tenantToken)

	want := "ListCredentials " + testTenant + "/prod/" + id.String()
	for _, ref := range []string{"prod", id.String()} {
		w := e.do(http.MethodGet, "/api/v1/environments/"+ref+"/credentials", authz, nil)
		if w.Code != http.StatusOK || w.Body.String() != want {
			t.Fatalf("%s: %d %q, want %q", ref, w.Code, w.Body.String(), want)
		}
	}
	w := e.do(http.MethodPut, "/api/v1/environments/prod/credentials/erp", authz, map[string]any{})
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "PutCredential "+testTenant+"/prod/") {
		t.Fatalf("put: %d %q", w.Code, w.Body.String())
	}
	for _, ref := range []string{"nope", foreign.String()} {
		w := e.do(http.MethodGet, "/api/v1/environments/"+ref+"/credentials/erp", authz, nil)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no such environment") {
			t.Fatalf("%s: %d %q", ref, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/credentials", "/api/v1/credentials/erp"} {
		if w := e.do(http.MethodGet, path, authz, nil); w.Code != http.StatusNotFound || w.Header().Get("X-Door") == "bindings" {
			t.Fatalf("%s: %d %q", path, w.Code, w.Body.String())
		}
	}
}

// An environment being deleted shows its credentials, and takes no write
// of them: 409.
func TestCredentialRoutesWhileTheEnvironmentIsDeleted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{bindings: &fakeBindings{on: true}})
	id := e.mkEnv(testTenant, "prod")
	if _, err := e.st.Q().Exec(context.Background(), "UPDATE environments SET deleting = true WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	authz := bearerOf(e.tenantToken)
	for _, path := range []string{"/api/v1/environments/prod/credentials", "/api/v1/environments/prod/credentials/erp"} {
		if w := e.do(http.MethodGet, path, authz, nil); w.Code != http.StatusOK {
			t.Errorf("GET %s: %d %q", path, w.Code, w.Body.String())
		}
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/environments/prod/credentials/erp"},
		{http.MethodDelete, "/api/v1/environments/prod/credentials/erp"},
		{http.MethodPost, "/api/v1/environments/prod/credentials/erp/check"},
	} {
		w := e.do(c.method, c.path, authz, map[string]any{})
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "environment prod is being deleted") {
			t.Errorf("%s %s: %d %q", c.method, c.path, w.Code, w.Body.String())
		}
	}
}

// With a grant on the environment alone, a route acts on the environment
// its grant was checked on: allow hands the ID it resolved to the
// handler, which does not read the path's name again, so that a name
// that names another environment by then does not lead there.
func TestCredentialRoutesActWhereTheGrantWasChecked(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{bindings: &fakeBindings{on: true}})
	dev := e.mkEnv(testTenant, "dev")
	prod := e.mkEnv(testTenant, "prod")
	tok, hash, err := mintTenantToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.InsertTenantToken(context.Background(), store.TenantToken{Hash: hash, TenantID: testTenant,
		Role: "write", EnvironmentID: dev}); err != nil {
		t.Fatal(err)
	}
	authz := bearerOf(tok)
	w := e.do(http.MethodPut, "/api/v1/environments/dev/credentials/erp", authz, map[string]any{})
	if want := "PutCredential " + testTenant + "/dev/" + dev.String(); w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("put on dev: %d %q, want %q", w.Code, w.Body.String(), want)
	}
	if w := e.do(http.MethodPut, "/api/v1/environments/prod/credentials/erp", authz, map[string]any{}); w.Code != http.StatusNotFound {
		t.Fatalf("put on prod: %d %q", w.Code, w.Body.String())
	}

	// As if "dev" had named prod when allow checked it: the handler gets
	// prod, the environment checked, not dev, which the name names now.
	var got bindings.Env
	h := e.srv.inEnv(func(_ http.ResponseWriter, _ *http.Request, env bindings.Env) { got = env })
	r := httptest.NewRequest(http.MethodPut, "/api/v1/environments/dev/credentials/erp", nil)
	r.SetPathValue("env", "dev")
	ctx := auth.WithTenant(r.Context(), auth.Tenant{Name: testTenant})
	ctx = context.WithValue(ctx, checkedEnvKey{}, checkedEnvRef{ref: "dev", id: prod})
	if err := h(httptest.NewRecorder(), r.WithContext(ctx)); err != nil || got.ID != prod || got.Name != "prod" {
		t.Fatalf("the handler got %+v %v, want prod", got, err)
	}
}

// A sandbox names HTTP secrets (ADR 0055) by secrets at its create and on
// its own route; credentials stays their alias, and both at once is 400.
func TestSandboxSecretsRoutes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{bindings: &fakeBindings{on: true}})
	authz := bearerOf(e.tenantToken)
	w := e.do("POST", "/api/v1/sandboxes", authz, map[string]any{"environment": "box", "secrets": []string{"s4"},
		"credentials": []string{"s4"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not both") {
		t.Fatalf("secrets and credentials: %d %s", w.Code, w.Body.String())
	}
	w = e.do("POST", "/api/v1/sandboxes", authz, map[string]any{"environment": "box", "secrets": []string{"s4"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("create with secrets: %d %s", w.Code, w.Body.String())
	}
	id, _ := e.createSandbox()
	if w := e.do("PUT", "/api/v1/sandboxes/"+id+"/secrets", authz, map[string]any{"secrets": []string{"s4"}}); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"secrets":[]`) {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("GET", "/api/v1/sandboxes/"+id+"/secrets", authz, nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"secrets":[]`) {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
}
