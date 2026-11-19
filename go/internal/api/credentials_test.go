// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
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
