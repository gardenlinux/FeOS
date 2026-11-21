// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// integrationRefused is the body of 403 integration_refused.
const integrationRefused = `{"error":"an integration changes no principal of a project","code":"integration_refused"}` + "\n"

// A need says in the route table what it says of integrations: that it
// refuses them, or lets one through on itself.
func TestNeedIntegrationWords(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		n    need
		want string
	}{
		{noIntegration(onProject(auth.RoleAdmin)), "admin project, no integration"},
		{noIntegration(member), "member, no integration"},
		{noIntegration(byHandler(auth.RoleAdmin)), "admin checked by the handler, no integration"},
		{noIntegration(onEnv(auth.RoleWrite)), "write environment, no integration"},
		{orOwnIntegration(byHandler(auth.RoleAdmin)), "admin checked by the handler or the integration itself"},
		{onProject(auth.RoleAdmin), "admin project"},
		{member, "member"},
	} {
		if got := c.n.String(); got != c.want {
			t.Errorf("%q, want %q", got, c.want)
		}
	}
}

// A need that refuses integrations refuses one whatever its role and
// whether it names a person, before any role is looked at; every other
// principal, and an integration on any other need, goes on to the role
// check.
func TestAllowRefusesIntegration(t *testing.T) {
	t.Parallel()
	s := &server{}
	in, person := uuid.New(), uuid.New()
	integration := auth.Principal{Integration: in}
	naming := auth.Principal{Integration: in, For: person}
	for _, c := range []struct {
		name string
		p    auth.Principal
		n    need
		want string // "" passes, else the code
	}{
		{"integration, refusing need", integration, noIntegration(onProject(auth.RoleAdmin)), codeIntegrationRefused},
		{"integration naming a person, refusing need", naming, noIntegration(member), codeIntegrationRefused},
		{"integration, refusing need on an environment", integration, noIntegration(onEnv(auth.RoleRead)), codeIntegrationRefused},
		{"integration, plain need", integration, onProject(auth.RoleAdmin), ""},
		{"integration, own need", integration, orOwnIntegration(byHandler(auth.RoleAdmin)), ""},
		{"person, refusing need", auth.Principal{Person: person}, noIntegration(onProject(auth.RoleAdmin)), ""},
		{"token, refusing need", auth.Principal{TokenHash: "ab"}, noIntegration(onProject(auth.RoleAdmin)), ""},
		{"operator, refusing need", auth.Principal{Person: person, Operator: true}, noIntegration(member), ""},
	} {
		r := httptest.NewRequest("GET", "/api/v1/project", nil)
		_, err := s.allow(r, auth.Project{Name: testProject, Principal: c.p, Role: auth.RoleAdmin}, c.n)
		var got string
		if err != nil {
			e, ok := err.(*apierr.Error)
			if !ok {
				t.Fatalf("%s: %v", c.name, err)
			}
			got = e.Code
			if e.Status != http.StatusForbidden {
				t.Errorf("%s: status %d", c.name, e.Status)
			}
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// An integration is no person and no provider of visitors: its bearer (a
// JWT) on every route of the person and provider classes is 403
// integration_refused, before any session or token is looked at; another
// bearer on the provider class is still a project token's.
func TestIntegrationBearerRefusedByClass(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{visitors: &fakeVisitors{}})
	jwt := bearerOf("eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln")
	want := map[string]class{}
	for p, c := range routeClasses {
		want[p] = c
	}
	for p, c := range visitorRouteClasses {
		want[p] = c
	}
	n := 0
	for pattern, c := range want {
		if c != classPerson && c != classProvider {
			continue
		}
		n++
		method, path := concrete(pattern, podid.Mint().String())
		w := e.do(method, path, jwt, nil)
		if w.Code != http.StatusForbidden || w.Body.String() != integrationRefused {
			t.Errorf("%s: %d %s", pattern, w.Code, w.Body.String())
		}
	}
	if n < 7 {
		t.Fatalf("%d routes of the person and provider classes", n)
	}
	w := e.do("POST", "/api/v1/challenges/open", bearerOf("0123abcd"), nil)
	expect(t, w, http.StatusUnauthorized, errBody("unknown project token"))
	if isJWT("feos-auth-abc_DEF-123") || isJWT(e.projectToken) || !isJWT("a.b.c") || isJWT("a.b") || isJWT("a.b.c.d") {
		t.Fatal("isJWT")
	}
}
