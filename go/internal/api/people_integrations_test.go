// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	fapi "github.com/gardenlinux/feos/go/ferun/api"

	"github.com/gardenlinux/feos/go/internal/store"
)

// asIntegration is a request with an integration's bearer in project,
// naming person when it is not "".
func asIntegration(method, path, jwt, project, person string, body any) request {
	h := http.Header{"Authorization": {bearerOf(jwt)}}
	if project != "" {
		h.Set(projectHeader, project)
	}
	if person != "" {
		h.Set(personHeader, person)
	}
	return request{method: method, path: path, body: body, header: h}
}

// asSession is a request of a session's cookie in testProject.
func asSession(method, path, cookie string, body any) request {
	return request{method: method, path: path, body: body, cookies: []*http.Cookie{{Name: cookieSession, Value: cookie}},
		header: http.Header{projectHeader: {testProject}, csrfHeader: {csrfValue}}}
}

// codeOf is an answer's code.
func codeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[struct{ Code string }](t, w).Code
}

// The provider's keys out of reach: an integration's bearer is 502
// provider_unreachable, not a refusal.
func TestIntegrationProviderUnreachable(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	expect(t, w.e.do("PUT", "/api/v1/admin/integrations/fecode", adminAuth(), map[string]any{"subject": "client-fecode"}),
		http.StatusCreated, "")
	jwt := w.p.AccessToken("client-fecode", map[string]any{"aud": testPublicURL})
	w.p.Mu.Lock()
	w.p.JWKSDown = true
	w.p.Mu.Unlock()
	expect(t, w.e.serve(asIntegration("GET", "/api/v1/project", jwt, testProject, "", nil)), http.StatusBadGateway,
		`{"error":"the provider's keys cannot be read","code":"provider_unreachable"}`+"\n")
	expect(t, w.e.do("GET", "/auth/me", bearerOf(jwt), nil), http.StatusBadGateway,
		`{"error":"the provider's keys cannot be read","code":"provider_unreachable"}`+"\n")
}

// An operator registers an integration, which then signs in with the
// provider's access token: a token of another audience, a person's token
// and the token of no registered integration open nothing; a project that
// gave it no grant is unknown to it, and its project's admin grants it.
func TestIntegrationRegistrationAndSignIn(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	put := func(name string, body any) *httptest.ResponseRecorder {
		return e.do("PUT", "/api/v1/admin/integrations/"+name, adminAuth(), body)
	}
	reg := map[string]any{"title": "FeCode", "url": "https://fecode.dev", "subject": "client-fecode"}
	resp := put("fecode", reg)
	expect(t, resp, http.StatusCreated, "")
	got := decode[adminIntegration](t, resp)
	if got.Issuer != w.p.Issuer || got.Subject != "client-fecode" || got.MadeBy != "basic:"+testAdmin || got.Disabled {
		t.Fatalf("registered %+v", got)
	}
	expect(t, put("fecode", reg), http.StatusOK, "")
	resp = put("other", map[string]any{"subject": "client-fecode"})
	expect(t, resp, http.StatusConflict, "")
	if codeOf(t, resp) != codeSubjectTaken {
		t.Fatalf("taken: %s", resp.Body)
	}
	resp = put("other", map[string]any{"subject": "x", "issuer": "https://evil.example/"})
	expect(t, resp, http.StatusUnprocessableEntity, `{"error":"this installation verifies tokens of `+w.p.Issuer+
		` only","code":"issuer_refused"}`+"\n")
	expect(t, put("Bad_Name", map[string]any{"subject": "x"}), http.StatusBadRequest, "")
	expect(t, put("other", map[string]any{"subject": "x", "url": "http://plain.example"}), http.StatusBadRequest, "")
	expect(t, put("other", map[string]any{"subject": ""}), http.StatusBadRequest, "")
	// A person is no operator.
	admin := w.person(t, "adm", false, store.Grant{Role: "admin"})
	expect(t, e.serve(asSession("PUT", "/api/v1/admin/integrations/x", admin, map[string]any{"subject": "x"})),
		http.StatusForbidden, errBody("operators only"))

	jwt := w.p.AccessToken("client-fecode", map[string]any{"aud": testPublicURL})
	get := func(tok, project, person string) *httptest.ResponseRecorder {
		return e.serve(asIntegration("GET", "/api/v1/project", tok, project, person, nil))
	}
	expect(t, get(jwt, testProject, ""), http.StatusNotFound, errBody("no such project"))
	expect(t, get(jwt, "", ""), http.StatusBadRequest, errBody("an integration names its project in the FeRun-Project header"))
	for _, c := range []struct {
		name, tok, code string
	}{
		{"another audience", w.p.AccessToken("client-fecode", map[string]any{"aud": "https://other.example"}), codeTokenRefused},
		{"a person's", w.p.AccessToken("client-fecode", map[string]any{"aud": testPublicURL, "client_id": "ferun"}),
			codeTokenRefused},
		{"expired", w.p.AccessToken("client-fecode", map[string]any{"aud": testPublicURL, "exp": 1}), codeTokenRefused},
		{"no integration", w.p.AccessToken("client-nobody", map[string]any{"aud": testPublicURL}), codeUnknownIntegration},
	} {
		resp := get(c.tok, testProject, "")
		if resp.Code != http.StatusUnauthorized || codeOf(t, resp) != c.code ||
			resp.Header().Get("WWW-Authenticate") != `Bearer realm="ferun"` {
			t.Errorf("%s: %d %s %v", c.name, resp.Code, resp.Body, resp.Header())
		}
	}
	// The project's admin grants it; GET /api/v1/integrations lists it
	// for every person.
	resp = e.serve(asSession("GET", "/api/v1/integrations", admin, nil))
	expect(t, resp, http.StatusOK, `{"integrations":[{"name":"fecode","title":"FeCode","url":"https://fecode.dev"}]}`+"\n")
	writeFixture(t, "GET_integrations.200", w.p.Issuer, "GET", "/api/v1/integrations", nil, nil, resp)
	resp = e.serve(asSession("PUT", "/api/v1/project/integrations/fecode/grants", admin, map[string]string{"role": "write"}))
	expect(t, resp, http.StatusOK, `{"integration":"fecode","role":"write","environment":""}`+"\n")
	expect(t, e.serve(asSession("PUT", "/api/v1/project/integrations/nope/grants", admin, map[string]string{"role": "write"})),
		http.StatusNotFound, errBody("no such integration"))
	resp = e.serve(asSession("PUT", "/api/v1/project/integrations/fecode/grants", admin,
		map[string]string{"role": "admin", "environment": "stage"}))
	expect(t, resp, http.StatusOK, `{"integration":"fecode","role":"admin","environment":"`+w.stage.String()+`"}`+"\n")
	expect(t, get(jwt, testProject, ""), http.StatusOK, `{"project":"acme","role":"write","environments":["`+
		w.stage.String()+`"]}`+"\n")
	expect(t, get(jwt, "other", ""), http.StatusNotFound, errBody("no such project"))
	// A project's writer may not grant it.
	writer := w.person(t, "wr", false, store.Grant{Role: "write"})
	expect(t, e.serve(asSession("PUT", "/api/v1/project/integrations/fecode/grants", writer, map[string]string{"role": "read"})),
		http.StatusForbidden, "")

	// The operators' list, then disabled: it signs in and reaches nothing,
	// and its grants stay for when it is enabled.
	resp = e.do("GET", "/api/v1/admin/integrations", adminAuth(), nil)
	expect(t, resp, http.StatusOK, "")
	if l := decode[struct{ Integrations []adminIntegration }](t, resp).Integrations; len(l) != 1 || l[0].Projects != 1 {
		t.Fatalf("admin list %s", resp.Body)
	}
	writeFixture(t, "GET_admin_integrations.200", w.p.Issuer, "GET", "/api/v1/admin/integrations", nil, nil, resp)
	expect(t, e.do("POST", "/api/v1/admin/integrations/fecode/disable", adminAuth(), nil), http.StatusOK,
		`{"name":"fecode","disabled":true}`+"\n")
	expect(t, get(jwt, testProject, ""), http.StatusNotFound, errBody("no such project"))
	resp = e.do("GET", "/auth/me", bearerOf(jwt), nil)
	expect(t, resp, http.StatusOK, `{"kind":"integration","integration":{"name":"fecode","title":"FeCode",`+
		`"url":"https://fecode.dev","disabled":true},"projects":[],"accounts":[]}`+"\n")
	resp = e.serve(asSession("GET", "/api/v1/integrations", admin, nil))
	expect(t, resp, http.StatusOK, `{"integrations":[]}`+"\n")
	expect(t, e.do("POST", "/api/v1/admin/integrations/fecode/enable", adminAuth(), nil), http.StatusOK,
		`{"name":"fecode","disabled":false}`+"\n")
	expect(t, get(jwt, testProject, ""), http.StatusOK, "")
	// Removed: its grants go, and it is unknown.
	expect(t, e.do("DELETE", "/api/v1/admin/integrations/fecode", adminAuth(), nil), http.StatusOK, `{"deleted":true}`+"\n")
	expect(t, e.do("DELETE", "/api/v1/admin/integrations/fecode", adminAuth(), nil), http.StatusNotFound,
		errBody("no such integration"))
	resp = get(jwt, testProject, "")
	if resp.Code != http.StatusUnauthorized || codeOf(t, resp) != codeUnknownIntegration {
		t.Fatalf("removed: %d %s", resp.Code, resp.Body)
	}
	var kinds []string
	rows, err := e.st.Q().Query(context.Background(), "SELECT kind FROM activity WHERE kind LIKE 'integration_%' ORDER BY ts_ns")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		kinds = append(kinds, k)
	}
	rows.Close()
	if strings.Join(kinds, ",") != "integration_registered,integration_changed,integration_disabled,integration_enabled,"+
		"integration_removed" {
		t.Fatalf("activity %v", kinds)
	}
}

// Without --oidc-issuer there are no integrations: every JWT bearer is
// refused and the integration routes answer 503.
func TestIntegrationsWithoutIssuer(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	resp := e.do("GET", "/api/v1/admin/integrations", adminAuth(), nil)
	expect(t, resp, http.StatusServiceUnavailable, "")
	if codeOf(t, resp) != codeIntegrationsUnavailable {
		t.Fatalf("%s", resp.Body)
	}
	resp = e.serve(asIntegration("GET", "/api/v1/project", "a.b.c", testProject, "", nil))
	if resp.Code != http.StatusUnauthorized || codeOf(t, resp) != codeTokenRefused {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
}

// The person an integration names bounds the call: the lower of two
// roles; a person FeRun does not know, one disabled and one without a
// grant there are an unknown project; a value that is no subject is 400;
// a session or a project token that names a person is 400.
func TestIntegrationNamesAPerson(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	jwt := w.integration(t, "fecode", store.Grant{Role: "admin"})
	w.providerPerson(t, "reader", store.Grant{Role: "read"})
	w.providerPerson(t, "stager", store.Grant{Role: "write", EnvironmentID: w.stage})
	off := w.providerPerson(t, "off", store.Grant{Role: "admin"})
	w.providerPerson(t, "elsewhere", store.Grant{ProjectID: "other", Role: "admin"})
	if _, err := e.st.SetPersonDisabled(context.Background(), off, true); err != nil {
		t.Fatal(err)
	}
	get := func(person string) *httptest.ResponseRecorder {
		return e.serve(asIntegration("GET", "/api/v1/project?filter=environments", jwt, testProject, person, nil))
	}
	expect(t, get(""), http.StatusOK, `{"project":"acme","role":"admin","environments":[]}`+"\n")
	expect(t, get("reader"), http.StatusOK, `{"project":"acme","role":"read","environments":[]}`+"\n")
	expect(t, get("stager"), http.StatusOK, `{"project":"acme","role":"","environments":["`+w.stage.String()+`"]}`+"\n")
	for _, p := range []string{"nobody", "off", "elsewhere"} {
		expect(t, get(p), http.StatusNotFound, errBody("no such project"))
	}
	for _, p := range []string{"bad\x01", strings.Repeat("x", 256)} {
		resp := get(p)
		if resp.Code != http.StatusBadRequest || codeOf(t, resp) != codePersonHeaderRefused {
			t.Errorf("%q: %d %s", p, resp.Code, resp.Body)
		}
	}
	// The named person's role decides a write: a reader's call is 403.
	resp := e.serve(asIntegration("PUT", "/api/v1/environments/stage/tags/v1", jwt, testProject, "reader", nil))
	expect(t, resp, http.StatusForbidden, "")
	resp = e.serve(asIntegration("PUT", "/api/v1/environments/prod/tags/v1", jwt, testProject, "stager", nil))
	expect(t, resp, http.StatusNotFound, errBody("no such environment"))
	// Only an integration names a person.
	refused := `{"error":"only an integration names a person","code":"person_header_refused"}` + "\n"
	tok := e.serve(request{method: "GET", path: "/api/v1/project", header: http.Header{"Authorization": {bearerOf(e.projectToken)},
		personHeader: {"reader"}}})
	expect(t, tok, http.StatusBadRequest, refused)
	sess := asSession("GET", "/api/v1/project", w.person(t, "s", false, store.Grant{Role: "read"}), nil)
	sess.header.Set(personHeader, "reader")
	expect(t, e.serve(sess), http.StatusBadRequest, refused)
	sess.path = "/auth/me"
	expect(t, e.serve(sess), http.StatusBadRequest, refused)
	sess.path = "/api/v1/integrations"
	expect(t, e.serve(sess), http.StatusBadRequest, refused)
	op := asSession("GET", "/api/v1/admin/integrations", w.person(t, "op", true), nil)
	op.header.Set(personHeader, "reader")
	expect(t, e.serve(op), http.StatusBadRequest, refused)
	basic := request{method: "GET", path: "/api/v1/admin/integrations", header: http.Header{"Authorization": {adminAuth()},
		personHeader: {"reader"}}}
	expect(t, e.serve(basic), http.StatusBadRequest, refused)

	// What it makes records the integration and the person it named.
	in, err := e.st.GetIntegration(context.Background(), "fecode")
	if err != nil {
		t.Fatal(err)
	}
	resp = e.serve(asIntegration("POST", "/api/v1/sandboxes", jwt, testProject, "stager", map[string]any{"environment": "stage"}))
	expect(t, resp, http.StatusCreated, "")
	id := decode[createSandboxResponse](t, resp).Status.SandboxID
	var by string
	if err := e.st.Q().QueryRow(context.Background(), `SELECT s.made_by || '|' || t.made_by FROM sandboxes s
		JOIN sandbox_tokens t ON t.sandbox_id = s.id WHERE s.id = $1`, id).Scan(&by); err != nil {
		t.Fatal(err)
	}
	want := "integration:" + in.ID.String() + "/person:" + w.persons["stager"].String()
	if by != want+"|"+want {
		t.Fatalf("made_by %q, want %q", by, want)
	}
}

// An integration changes no principal: its own grants it gives up, one by
// one or by leaving, and another's it does not touch; what its role
// allows otherwise it does.
func TestIntegrationOwnGrants(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	jwt := w.integration(t, "fecode", store.Grant{Role: "admin"}, store.Grant{Role: "write", EnvironmentID: w.stage})
	w.integration(t, "other", store.Grant{Role: "read"})
	do := func(method, path string, body any) *httptest.ResponseRecorder {
		return e.serve(asIntegration(method, path, jwt, testProject, "", body))
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/v1/project/members/" + uuid.NewString() + "/grants", map[string]string{"role": "read"}},
		{"GET", "/api/v1/project/invitations", nil},
		{"POST", "/api/v1/project/invitations", map[string]string{"role": "read"}},
		{"GET", "/api/v1/project/tokens", nil},
		{"POST", "/api/v1/project/tokens", map[string]string{"role": "read"}},
		{"PUT", "/api/v1/project/tokens/" + store.HashToken(e.projectToken) + "/grants", map[string]string{"role": "read"}},
		{"PUT", "/api/v1/project/integrations/fecode/grants", map[string]string{"role": "admin"}},
		{"DELETE", "/api/v1/project/integrations/other/grants", nil},
		{"POST", "/api/v1/projects", map[string]string{"name": "mine"}},
	} {
		resp := do(c.method, c.path, c.body)
		if resp.Body.String() != integrationRefused {
			t.Errorf("%s %s: %d %s", c.method, c.path, resp.Code, resp.Body)
		}
	}
	// It reads the members, and creates environments.
	expect(t, do("GET", "/api/v1/project/members", nil), http.StatusOK, "")
	expect(t, do("GET", "/api/v1/project/quota", nil), http.StatusOK, "")
	resp := do("DELETE", "/api/v1/project/integrations/fecode/grants?environment=stage", nil)
	expect(t, resp, http.StatusOK, `{"deleted":true}`+"\n")
	expect(t, do("POST", "/api/v1/project/leave", nil), http.StatusOK, `{"left":true}`+"\n")
	expect(t, do("GET", "/api/v1/project", nil), http.StatusNotFound, errBody("no such project"))
	// The other integration's grant is there still.
	if is, _ := e.st.ListIntegrationGrants(context.Background(), testProject); len(is) != 1 || is[0].Name != "other" {
		t.Fatalf("grants left %+v", is)
	}
}

// A sandbox token an integration mints goes with its access: refused once
// its grant is lowered below write, and with the integration disabled or
// removed; minted for a person in a protected environment it stays while
// the integration holds write there itself.
func TestIntegrationMintedSandboxToken(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	ctx := context.Background()
	jwt := w.integration(t, "fecode", store.Grant{Role: "write"})
	resp := e.serve(asIntegration("POST", "/api/v1/sandboxes", jwt, testProject, "", map[string]any{"environment": "stage"}))
	expect(t, resp, http.StatusCreated, "")
	created := decode[createSandboxResponse](t, resp)
	resolves := func(want bool, what string) {
		t.Helper()
		_, err := ResolveSandboxToken(ctx, e.st.Q(), created.Token)
		if (err == nil) != want {
			t.Fatalf("%s: %v, want good %v", what, err, want)
		}
		r := e.do("GET", "/api/v1/sandboxes/"+created.Status.SandboxID+"/processes", bearerOf(created.Token), nil)
		if (r.Code != http.StatusNotFound) != want {
			t.Fatalf("%s: the sandbox class %d %s", what, r.Code, r.Body)
		}
	}
	resolves(true, "minted")
	in, err := e.st.GetIntegration(ctx, "fecode")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.PutIntegrationGrant(ctx, testProject, in.ID, uuid.Nil, "read", ""); err != nil {
		t.Fatal(err)
	}
	resolves(false, "lowered to read")
	if _, err := e.st.PutIntegrationGrant(ctx, testProject, in.ID, uuid.Nil, "write", ""); err != nil {
		t.Fatal(err)
	}
	resolves(true, "write again")
	if _, err := e.st.SetIntegrationDisabled(ctx, "fecode", true); err != nil {
		t.Fatal(err)
	}
	resolves(false, "disabled")
	if _, err := e.st.SetIntegrationDisabled(ctx, "fecode", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.st.SetEnvironmentProtected(ctx, w.stage, true); err != nil {
		t.Fatal(err)
	}
	resolves(false, "project write on a protected environment")
	if _, err := e.st.PutIntegrationGrant(ctx, testProject, in.ID, w.stage, "write", ""); err != nil {
		t.Fatal(err)
	}
	resolves(true, "write on the protected environment")
	expect(t, e.do("DELETE", "/api/v1/admin/integrations/fecode", adminAuth(), nil), http.StatusOK, "")
	resolves(false, "removed")
}

// envNamed is the environment of that name of an /auth/me project.
func envNamed(p fapi.MeProject, name string) fapi.MeEnvironment {
	for _, e := range p.Environments {
		if e.Name == name {
			return e
		}
	}
	return fapi.MeEnvironment{}
}

// GET /auth/me answers each kind of caller with its effective roles: a
// person's session, an operator's, a project token, an integration on its
// own and naming a person; protection and the plan's exception show in
// each environment's entry.
func TestAuthMeShapes(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	ctx := context.Background()
	if _, _, err := e.st.SetEnvironmentProtected(ctx, w.prod, true); err != nil {
		t.Fatal(err)
	}
	me := func(rq request) *httptest.ResponseRecorder {
		rq.method, rq.path = "GET", "/auth/me"
		return e.serve(rq)
	}
	// A person with write on the project: prod, protected, they read and
	// plan; stage they change.
	ada := w.person(t, "ada", false, store.Grant{Role: "write"})
	resp := me(asSession("", "", ada, nil))
	expect(t, resp, http.StatusOK, "")
	m := decode[fapi.Me](t, resp)
	if m.Kind != "session" || len(m.Projects) != 1 || m.Projects[0].Role != "write" ||
		envNamed(m.Projects[0], "prod") != (fapi.MeEnvironment{ID: w.prod.String(), Name: "prod", Role: "read", Protected: true,
			Plan: true}) ||
		envNamed(m.Projects[0], "stage") != (fapi.MeEnvironment{ID: w.stage.String(), Name: "stage", Role: "write", Plan: true,
			Delete: true}) || m.Accounts == nil {
		t.Fatalf("session %s", resp.Body)
	}
	writeFixture(t, "GET_auth_me.session.200", w.p.Issuer, "GET", "/auth/me", nil, nil, resp)

	resp = me(asSession("", "", w.person(t, "op", true), nil))
	expect(t, resp, http.StatusOK, "")
	m = decode[fapi.Me](t, resp)
	if !m.Admin || len(m.Projects) != 2 || m.Projects[0].Role != "admin" ||
		!envNamed(m.Projects[0], "prod").Delete || envNamed(m.Projects[0], "prod").Role != "admin" {
		t.Fatalf("operator %s", resp.Body)
	}
	writeFixture(t, "GET_auth_me.operator.200", w.p.Issuer, "GET", "/auth/me", nil, nil, resp)

	// A token of prod with write: protected, its grant on prod decides.
	tok := w.token(t, "write", w.prod)
	resp = e.do("GET", "/auth/me", bearerOf(tok), nil)
	expect(t, resp, http.StatusOK, `{"kind":"token","token":{"label":"","role":"write","environment":"`+w.prod.String()+
		`"},"projects":[{"project":"acme","role":"","environments":[{"id":"`+w.prod.String()+`","name":"prod",`+
		`"role":"write","protected":true,"plan":true,"delete":false}]}],"accounts":[]}`+"\n")
	writeFixture(t, "GET_auth_me.token.200", w.p.Issuer, "GET", "/auth/me", nil, nil, resp)
	expect(t, e.do("GET", "/auth/me", bearerOf("0123"), nil), http.StatusUnauthorized, errBody("unknown project token"))

	// An integration with write on the project: prod it reads, alone.
	jwt := w.integration(t, "fecode", store.Grant{Role: "write"}, store.Grant{Role: "admin", EnvironmentID: w.prod})
	w.integration(t, "elsewhere", store.Grant{ProjectID: "other", Role: "read"})
	resp = e.do("GET", "/auth/me", bearerOf(jwt), nil)
	expect(t, resp, http.StatusOK, "")
	m = decode[fapi.Me](t, resp)
	if m.Kind != "integration" || m.Integration == nil || m.Integration.Name != "fecode" || m.Named != nil ||
		len(m.Projects) != 1 || m.Projects[0].Role != "write" || m.Projects[0].IntegrationRole != "write" ||
		envNamed(m.Projects[0], "prod") != (fapi.MeEnvironment{ID: w.prod.String(), Name: "prod", Role: "read",
			IntegrationRole: "admin", Protected: true, Plan: true}) ||
		envNamed(m.Projects[0], "stage") != (fapi.MeEnvironment{ID: w.stage.String(), Name: "stage", Role: "write",
			IntegrationRole: "write", Plan: true, Delete: true}) {
		t.Fatalf("integration %s", resp.Body)
	}
	writeFixture(t, "GET_auth_me.integration.200", w.p.Issuer, "GET", "/auth/me", nil, nil, resp)

	// Naming a person with write on the project and admin on prod: prod is
	// theirs to change with it, and to delete.
	w.providerPerson(t, "ada-sub", store.Grant{Role: "write"}, store.Grant{Role: "admin", EnvironmentID: w.prod},
		store.Grant{ProjectID: "other", Role: "admin"})
	named := asIntegration("GET", "/auth/me", jwt, "", "ada-sub", nil)
	resp = e.serve(named)
	expect(t, resp, http.StatusOK, "")
	m = decode[fapi.Me](t, resp)
	if m.Named == nil || !m.Named.Known || m.Named.Person != w.persons["ada-sub"].String() || len(m.Projects) != 1 ||
		m.Projects[0].PersonRole != "write" ||
		envNamed(m.Projects[0], "prod") != (fapi.MeEnvironment{ID: w.prod.String(), Name: "prod", Role: "admin",
			IntegrationRole: "admin", PersonRole: "admin", Protected: true, Plan: true, Delete: true}) {
		t.Fatalf("integration naming a person %s", resp.Body)
	}
	writeFixture(t, "GET_auth_me.integration_person.200", w.p.Issuer, "GET", "/auth/me", nil, nil, resp)
	// Unknown: known false, no project.
	resp = e.serve(asIntegration("GET", "/auth/me", jwt, "", "nobody", nil))
	expect(t, resp, http.StatusOK, `{"kind":"integration","integration":{"name":"fecode","title":"Integration fecode",`+
		`"url":"https://fecode.example.test","disabled":false},"named":{"sub":"nobody","known":false},"projects":[],"accounts":[]}`+"\n")
	// The plan route takes what /auth/me says, and so do the checks.
	expect(t, e.serve(asIntegration("PUT", "/api/v1/environments/prod/tags/v1", jwt, testProject, "", nil)),
		http.StatusForbidden, "")
}

// GET /api/v1/project/members lists people with their issuer and subject
// and who gave each grant, and the integrations beside them.
func TestMembersWithIntegrations(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	ada := w.person(t, "ada", false, store.Grant{Role: "admin"})
	w.person(t, "bob", false)
	w.integration(t, "fecode")
	expect(t, e.serve(asSession("PUT", "/api/v1/project/members/"+w.persons["bob"].String()+"/grants", ada,
		map[string]string{"role": "read", "environment": "stage"})), http.StatusNotFound, errBody("no such member"))
	if _, err := e.st.PutGrant(context.Background(), store.Grant{ProjectID: testProject, PersonID: w.persons["bob"],
		EnvironmentID: w.stage, Role: "write", MadeBy: "person:" + w.persons["ada"].String()}); err != nil {
		t.Fatal(err)
	}
	expect(t, e.serve(asSession("PUT", "/api/v1/project/integrations/fecode/grants", ada, map[string]string{"role": "admin"})),
		http.StatusOK, "")
	resp := e.serve(asSession("GET", "/api/v1/project/members", ada, nil))
	expect(t, resp, http.StatusOK, "")
	ms := decode[fapi.Members](t, resp)
	if len(ms.Members) != 2 || ms.Members[0].Name != "ada" || ms.Members[0].Issuer != "https://id.example.test/" ||
		ms.Members[0].Subject != "ada" || ms.Members[1].Environments[0].MadeByName != "ada" || len(ms.Integrations) != 1 ||
		ms.Integrations[0].Integration != "fecode" || ms.Integrations[0].Role != "admin" || ms.Integrations[0].MadeByName != "ada" {
		t.Fatalf("members %s", resp.Body)
	}
	writeFixture(t, "GET_project_members.200", w.p.Issuer, "GET", "/api/v1/project/members", nil, nil, resp)
}

// A protected environment: its admin sets and lifts it; the change is
// recorded, the same again changes nothing; the dialog's list names who
// changes it by a project grant; a token is given a grant on it.
func TestProtectEnvironment(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	ctx := context.Background()
	ada := w.person(t, "ada", false, store.Grant{Role: "admin"})
	tw := w.person(t, "tw", false, store.Grant{Role: "write"})
	w.person(t, "sw", false, store.Grant{Role: "write"}, store.Grant{Role: "write", EnvironmentID: w.stage})
	w.integration(t, "fecode", store.Grant{Role: "write"})
	tok, hash, err := mintProjectToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.InsertProjectToken(ctx, store.ProjectToken{Hash: hash, ProjectID: testProject, Label: "landscape release",
		Role: "write"}); err != nil {
		t.Fatal(err)
	}
	body := map[string]bool{"protected": true}
	expect(t, e.serve(asSession("PUT", "/api/v1/environments/stage/protected", tw, body)), http.StatusForbidden, "")
	expect(t, e.serve(asSession("PUT", "/api/v1/environments/stage/protected", ada, map[string]any{})),
		http.StatusBadRequest, errBody(`protected is required: {"protected": true} or {"protected": false}`))
	resp := e.serve(asSession("PUT", "/api/v1/environments/stage/protected", ada, body))
	expect(t, resp, http.StatusOK, `{"id":"`+w.stage.String()+`","name":"stage","protected":true,"changed":true}`+"\n")
	writeFixture(t, "PUT_environments_env_protected.200", w.p.Issuer, "PUT", "/api/v1/environments/stage/protected", nil,
		body, resp)
	expect(t, e.serve(asSession("PUT", "/api/v1/environments/stage/protected", ada, body)), http.StatusOK,
		`{"id":"`+w.stage.String()+`","name":"stage","protected":true,"changed":false}`+"\n")
	// The project's writer reads it now: a tag is 403, the plan passes.
	expect(t, e.serve(asSession("PUT", "/api/v1/environments/stage/tags/v1", tw, nil)), http.StatusForbidden, "")
	expect(t, e.serve(asSession("POST", "/api/v1/environments/stage/plan", tw, map[string]any{})), http.StatusOK, "")
	expect(t, e.serve(asSession("DELETE", "/api/v1/environments/stage", tw, nil)), http.StatusForbidden, "")

	resp = e.serve(asSession("GET", "/api/v1/environments/stage/principals", ada, nil))
	expect(t, resp, http.StatusOK, "")
	ps := decode[fapi.Principals](t, resp).Principals
	loses := map[string]bool{}
	for _, p := range ps {
		loses[p.Kind+":"+p.Label] = p.LosesWrite
	}
	if !loses["person:tw <tw@example.test>"] || loses["person:sw <sw@example.test>"] || loses["person:ada <ada@example.test>"] ||
		!loses["token:landscape release"] || !loses["integration:Integration fecode"] {
		t.Fatalf("principals %s", resp.Body)
	}
	writeFixture(t, "GET_environments_env_principals.200", w.p.Issuer, "GET", "/api/v1/environments/stage/principals", nil,
		nil, resp)
	expect(t, e.serve(asSession("GET", "/api/v1/environments/stage/principals", tw, nil)), http.StatusForbidden, "")

	// The dialog gives the token write on stage; its grant goes again.
	resp = e.serve(asSession("PUT", "/api/v1/project/tokens/"+hash+"/grants", ada, map[string]string{"role": "write",
		"environment": "stage"}))
	expect(t, resp, http.StatusOK, `{"token":"`+hash+`","role":"write","environment":"`+w.stage.String()+`"}`+"\n")
	expect(t, e.do("PUT", "/api/v1/environments/stage/tags/v1", bearerOf(tok), nil), http.StatusOK, "")
	// The tokens' list shows it beside the token's own, to be removed there.
	resp = e.serve(asSession("GET", "/api/v1/project/tokens", ada, nil))
	expect(t, resp, http.StatusOK, "")
	listed := false
	for _, lt := range decode[struct {
		Tokens []struct {
			Hash, Role string
			Grants     []struct{ Role, Environment string }
		}
	}](t, resp).Tokens {
		if lt.Hash == hash {
			listed = lt.Role == "write" && len(lt.Grants) == 1 && lt.Grants[0].Role == "write" &&
				lt.Grants[0].Environment == w.stage.String()
		}
	}
	if !listed {
		t.Fatalf("the token's grant is not listed: %s", resp.Body)
	}
	expect(t, e.serve(asSession("PUT", "/api/v1/project/tokens/"+strings.Repeat("0", 64)+"/grants", ada,
		map[string]string{"role": "read"})), http.StatusNotFound, errBody("no such token"))
	expect(t, e.serve(asSession("DELETE", "/api/v1/project/tokens/"+hash+"/grants?environment=stage", ada, nil)),
		http.StatusOK, `{"deleted":true}`+"\n")
	expect(t, e.do("PUT", "/api/v1/environments/stage/tags/v1", bearerOf(tok), nil), http.StatusForbidden, "")

	// Lifted: recorded too.
	expect(t, e.serve(asSession("PUT", "/api/v1/environments/stage/protected", ada, map[string]bool{"protected": false})),
		http.StatusOK, "")
	var kinds string
	if err := e.st.Q().QueryRow(ctx, `SELECT string_agg(kind, ',' ORDER BY ts_ns) FROM activity
		WHERE kind LIKE 'environment_%protected'`).Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	if kinds != "environment_protected,environment_unprotected" {
		t.Fatalf("activity %q", kinds)
	}
}

// A token's grant beside its own keeps the project's last admin: the one
// admin a project has, by a grant, cannot lose it.
func TestTokenGrantKeepsAdmin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	mustProject(t, e, "solo")
	tok, hash, err := mintProjectToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.InsertProjectToken(ctx, store.ProjectToken{Hash: hash, ProjectID: "solo", Role: "read"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.PutGrant(ctx, store.Grant{ProjectID: "solo", TokenHash: hash, Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	resp := e.do("DELETE", "/api/v1/project/tokens/"+hash+"/grants", bearerOf(tok), nil)
	expect(t, resp, http.StatusConflict, "")
	if codeOf(t, resp) != CodeLastAdmin {
		t.Fatalf("%s", resp.Body)
	}
}
