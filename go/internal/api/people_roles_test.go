// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/domains"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// routeNeeds is the role and scope of every tenant-class route (ADR
// 0031): the contract of which role may call what. A tenant route missing
// here, or with another need in the table, fails TestRouteNeeds.
var routeNeeds = map[string]string{
	"GET /api/v1/tools":                                         "read tenant",
	"POST /api/v1/sandboxes":                                    "write tenant",
	"GET /api/v1/sandboxes":                                     "read tenant",
	"GET /api/v1/sandboxes/{id}":                                "read tenant",
	"DELETE /api/v1/sandboxes/{id}":                             "write tenant",
	"POST /api/v1/sandboxes/{id}/pause":                         "write tenant",
	"POST /api/v1/sandboxes/{id}/resume":                        "write tenant",
	"PUT /api/v1/sandboxes/{id}/image-credentials":              "write tenant",
	"DELETE /api/v1/sandboxes/{id}/image-credentials":           "write tenant",
	"GET /api/v1/sandboxes/{id}/tokens":                         "read tenant",
	"POST /api/v1/sandboxes/{id}/tokens":                        "write tenant",
	"DELETE /api/v1/sandboxes/{id}/tokens/{hash}":               "write tenant",
	"GET /api/v1/sandboxes/{id}/access":                         "read tenant",
	"POST /api/v1/kube/preflight":                               "write tenant",
	"POST /api/v1/ssh/preflight":                                "write tenant",
	"POST /api/v1/http/preflight":                               "write tenant",
	"GET /api/v1/tenant":                                        "member",
	"POST /api/v1/environments":                                 "write tenant",
	"GET /api/v1/environments":                                  "member",
	"POST /api/v1/environments/plan":                            "read tenant",
	"GET /api/v1/environments/{env}":                            "read environment",
	"DELETE /api/v1/environments/{env}":                         "admin environment or write tenant",
	"POST /api/v1/environments/{env}/plan":                      "write environment",
	"POST /api/v1/environments/{env}/deployments":               "write environment",
	"GET /api/v1/environments/{env}/deployments":                "read environment",
	"GET /api/v1/environments/{env}/deployments/{n}":            "read environment",
	"POST /api/v1/environments/{env}/deployments/{n}/revert":    "write environment",
	"PUT /api/v1/environments/{env}/objects/{kind}/{name}":      "write environment",
	"DELETE /api/v1/environments/{env}/objects/{kind}/{name}":   "write environment",
	"GET /api/v1/environments/{env}/objects/{kind}/{name}/logs": "read environment",
	"GET /api/v1/environments/{env}/tags":                       "read environment",
	"GET /api/v1/environments/{env}/tags/{tag}":                 "read environment",
	"PUT /api/v1/environments/{env}/tags/{tag}":                 "write environment",
	"DELETE /api/v1/environments/{env}/tags/{tag}":              "write environment",
	"POST /api/v1/environments/{env}/clone":                     "write tenant",
	"GET /api/v1/environments/{env}/secrets":                    "read environment",
	"PUT /api/v1/environments/{env}/secrets/{name}":             "write environment",
	"DELETE /api/v1/environments/{env}/secrets/{name}":          "write environment",
	"GET /api/v1/environments/{env}/incidents":                  "read environment",
	"GET /api/v1/environments/{env}/incidents/{id}":             "read environment",
	"GET /api/v1/environments/{env}/access-logs":                "read environment",
	"GET /api/v1/environments/{env}/traffic":                    "read environment",
	"POST /api/v1/environments/{env}/services/{name}/restart":   "write environment",
	"POST /api/v1/environments/{env}/jobs/{name}/run":           "write environment",
	"POST /api/v1/environments/{env}/bindings/{name}/check":     "write environment",
	"POST /api/v1/environments/{env}/route-tokens":              "admin environment",
	"DELETE /api/v1/environments/{env}/route-tokens":            "admin environment",
	"PUT /api/v1/environments/{env}/public":                     "admin environment",
	"POST /api/v1/domains":                                      "admin tenant",
	"GET /api/v1/domains":                                       "member",
	"GET /api/v1/domains/{id}":                                  "member",
	"DELETE /api/v1/domains/{id}":                               "admin tenant",
	"PUT /api/v1/domains/{id}/environment":                      "admin checked by the handler",
	"DELETE /api/v1/domains/{id}/environment":                   "admin checked by the handler",
	"POST /api/v1/domains/{id}/check":                           "admin tenant",
	"POST /api/v1/jobs":                                         "write tenant",
	"GET /api/v1/jobs":                                          "read tenant",
	"GET /api/v1/jobs/{id}":                                     "read tenant",
	"DELETE /api/v1/jobs/{id}":                                  "write tenant",
	"GET /api/v1/jobs/{id}/logs":                                "read tenant",
	"POST /api/v1/jobs/{id}/cancel":                             "write tenant",

	"GET /api/v1/tenant/quota":                      "member",
	"GET /api/v1/tenant/members":                    "member",
	"PUT /api/v1/tenant/members/{person}/grants":    "admin checked by the handler",
	"DELETE /api/v1/tenant/members/{person}/grants": "admin checked by the handler",
	"POST /api/v1/tenant/leave":                     "member",
	"GET /api/v1/tenant/invitations":                "member",
	"POST /api/v1/tenant/invitations":               "admin checked by the handler",
	"DELETE /api/v1/tenant/invitations/{id}":        "admin checked by the handler",
	"GET /api/v1/tenant/tokens":                     "admin tenant",
	"POST /api/v1/tenant/tokens":                    "admin tenant",
	"DELETE /api/v1/tenant/tokens/{hash}":           "admin tenant",
}

// roleSkips are the routes TestRoles does not call: their effect would
// change the principals it goes on with (TestMembers covers them).
var roleSkips = map[string]bool{"POST /api/v1/tenant/leave": true}

// Every tenant-class route states its need in the table, as this test
// does; no other class has one.
func TestRouteNeeds(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{bindings: &fakeBindings{on: true}})
	var diff []string
	seen := map[string]bool{}
	for _, rt := range e.srv.routes() {
		switch {
		case rt.class != classTenant && rt.need != nil:
			diff = append(diff, rt.pattern+": a need on class "+string(rt.class))
		case rt.class == classTenant && rt.need == nil:
			diff = append(diff, rt.pattern+": no need")
		case rt.class == classTenant:
			seen[rt.pattern] = true
			if want, ok := routeNeeds[rt.pattern]; !ok {
				diff = append(diff, rt.pattern+": in the table, not in the test")
			} else if got := rt.need.String(); got != want {
				diff = append(diff, rt.pattern+": "+got+", want "+want)
			}
		}
	}
	for p := range routeNeeds {
		if !seen[p] {
			diff = append(diff, p+": in the test, not in the table")
		}
	}
	sort.Strings(diff)
	if len(diff) > 0 {
		t.Fatal(strings.Join(diff, "\n"))
	}
}

// principal is a test's caller in testTenant: a token or a person's
// session, and the roles the test expects it to have.
type principal struct {
	name string
	// token is the bearer; "" for a session.
	token string
	// cookie is the session's cookie value.
	cookie string
	tenant auth.Role
	envs   map[string]auth.Role // by environment name
}

// request is a request of p: the bearer, or the cookie with FeRun-Tenant
// and the UI's CSRF header.
func (p principal) request(method, path string, body any) request {
	h := http.Header{}
	rq := request{method: method, path: path, body: body, header: h}
	if p.token != "" {
		h.Set("Authorization", bearerOf(p.token))
		return rq
	}
	h.Set(tenantHeader, testTenant)
	h.Set(csrfHeader, csrfValue)
	rq.cookies = []*http.Cookie{{Name: cookieSession, Value: p.cookie}}
	return rq
}

// peopleWorld is an API with OIDC on, testTenant's environments stage and
// prod, and principals of each kind.
type peopleWorld struct {
	e           *env
	stage, prod uuid.UUID
	// persons by principal name
	persons map[string]uuid.UUID
}

// newPeopleWorld makes the world; o's OIDC and public URL are set.
func newPeopleWorld(t *testing.T, o envOptions) *peopleWorld {
	t.Helper()
	p := newTestProvider(t, algRS256)
	o.oidc, o.publicURL = p.config(), testPublicURL
	e := newEnv(t, o)
	w := &peopleWorld{e: e, stage: mkEnvRow(t, e, testTenant, "stage"), prod: mkEnvRow(t, e, testTenant, "prod"),
		persons: map[string]uuid.UUID{}}
	mustTenant(t, e, "other")
	mkEnvRow(t, e, "other", "stage")
	return w
}

// token mints a token of testTenant with its own grant.
func (w *peopleWorld) token(t *testing.T, role string, env uuid.UUID) string {
	t.Helper()
	tok, hash, err := mintTenantToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.st.InsertTenantToken(context.Background(), store.TenantToken{Hash: hash, TenantID: testTenant,
		Role: role, EnvironmentID: env}); err != nil {
		t.Fatal(err)
	}
	return tok
}

// person makes a person with a session (an operator's with operator) and
// the grants, and returns the session's cookie value.
func (w *peopleWorld) person(t *testing.T, name string, operator bool, grants ...store.Grant) string {
	t.Helper()
	ctx := context.Background()
	p, err := w.e.st.UpsertPerson(ctx, "https://id.example.test/", name, name+"@example.test", name)
	if err != nil {
		t.Fatal(err)
	}
	w.persons[name] = p.ID
	for _, g := range grants {
		g.PersonID = p.ID
		if g.TenantID == "" {
			g.TenantID = testTenant
		}
		if _, err := w.e.st.PutGrant(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	cookie := randomTestString(t)
	if _, err := w.e.st.InsertUISession(ctx, store.UISession{IDHash: store.HashToken(cookie), Sub: name,
		PersonID: p.ID, Admin: operator}, time.Hour); err != nil {
		t.Fatal(err)
	}
	return cookie
}

// principals are the callers of the role tests.
func (w *peopleWorld) principals(t *testing.T) []principal {
	R, W, A := auth.RoleRead, auth.RoleWrite, auth.RoleAdmin
	return []principal{
		{name: "admin token (FeCode's)", token: w.e.tenantToken, tenant: A},
		{name: "read token", token: w.token(t, "read", uuid.Nil), tenant: R},
		{name: "write token", token: w.token(t, "write", uuid.Nil), tenant: W},
		{name: "stage write token", token: w.token(t, "write", w.stage), envs: map[string]auth.Role{"stage": W}},
		{name: "operator", cookie: w.person(t, "op", true), tenant: A},
		{name: "tenant read, stage admin", cookie: w.person(t, "ra", false,
			store.Grant{Role: "read"}, store.Grant{Role: "admin", EnvironmentID: w.stage}), tenant: R,
			envs: map[string]auth.Role{"stage": A}},
		{name: "tenant write", cookie: w.person(t, "tw", false, store.Grant{Role: "write"}), tenant: W},
		{name: "tenant admin", cookie: w.person(t, "ta", false, store.Grant{Role: "admin"}), tenant: A},
		{name: "stage read", cookie: w.person(t, "sr", false, store.Grant{Role: "read", EnvironmentID: w.stage}),
			envs: map[string]auth.Role{"stage": R}},
		{name: "prod write", cookie: w.person(t, "pw", false, store.Grant{Role: "write", EnvironmentID: w.prod}),
			envs: map[string]auth.Role{"prod": W}},
	}
}

// expected is what the role model says of p on a route of need want (the
// path's environment "stage"): "pass", "403" or "404".
func expected(p principal, want string) string {
	role := func(s string) auth.Role { r, _ := auth.ParseRole(s); return r }
	words := strings.Fields(want)
	switch {
	case want == "member":
		return "pass"
	case strings.HasSuffix(want, " tenant") && len(words) == 2:
		if p.tenant >= role(words[0]) {
			return "pass"
		}
		return "403"
	case strings.Contains(want, "environment"):
		eff := max(p.tenant, p.envs["stage"])
		if eff >= role(words[0]) || (len(words) == 5 && p.tenant >= role(words[3])) {
			return "pass"
		}
		if eff == auth.RoleNone {
			return "404"
		}
		return "403"
	}
	return ""
}

// outcome reads an answer as the role check's: refused 403 with
// role_required, 404 for an environment or tenant not seen, else pass.
func outcome(w *httptest.ResponseRecorder) string {
	var b struct{ Error, Code, Role, Scope string }
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	switch {
	case w.Code == http.StatusForbidden && b.Code == "role_required":
		return "403"
	case w.Code == http.StatusNotFound && (b.Error == "no such environment" || b.Error == "no such tenant"):
		return "404"
	}
	return "pass"
}

// Every tenant route answers each principal as the role model says: the
// tenant's grant for what reaches beyond one environment, the higher of
// the tenant's and the environment's grant on an environment's route, an
// environment without a grant absent (404), a role too low 403
// role_required; a token's own scope counts as a person's grant does, an
// operator is admin everywhere, and FeCode's token reaches everything.
func TestRoles(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{bindings: &fakeBindings{on: true}})
	var bad []string
	for _, p := range w.principals(t) {
		for pattern, want := range routeNeeds {
			if strings.HasSuffix(want, "checked by the handler") || roleSkips[pattern] {
				continue
			}
			method, path := concrete(pattern, podid.Mint().String())
			got := outcome(w.e.serve(p.request(method, path, nil)))
			if exp := expected(p, want); got != exp {
				bad = append(bad, p.name+": "+pattern+": "+got+", want "+exp)
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatal(strings.Join(bad, "\n"))
	}
	// The answer names the role and the scope.
	sr := w.principals(t)[8]
	resp := w.e.serve(sr.request("PUT", "/api/v1/environments/stage/tags/v1", nil))
	expect(t, resp, http.StatusForbidden, `{"error":"this needs the role write on the environment",`+
		`"code":"role_required","role":"write","scope":"environment"}`+"\n")
	resp = w.e.serve(sr.request("GET", "/api/v1/sandboxes", nil))
	expect(t, resp, http.StatusForbidden, `{"error":"this needs the role read on the tenant",`+
		`"code":"role_required","role":"read","scope":"tenant"}`+"\n")
	// The environment by its ID is the same environment.
	resp = w.e.serve(sr.request("GET", "/api/v1/environments/"+w.stage.String(), nil))
	if outcome(resp) != "pass" {
		t.Fatalf("by ID: %d %s", resp.Code, resp.Body.String())
	}
	resp = w.e.serve(sr.request("GET", "/api/v1/environments/"+w.prod.String(), nil))
	expect(t, resp, http.StatusNotFound, errBody("no such environment"))
}

// The tenant of a request: a token's is its own, and a FeRun-Tenant that
// names another is an unknown tenant; a session names it, needs a grant
// there (or to be an operator's) and the CSRF rules of a session.
func TestTenantPrincipal(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	tok := func(tenant string) request {
		return request{method: "GET", path: "/api/v1/tenant", header: http.Header{"Authorization": {bearerOf(e.tenantToken)},
			tenantHeader: {tenant}}}
	}
	expect(t, e.serve(tok(testTenant)), http.StatusOK, `{"tenant":"acme"}`+"\n")
	expect(t, e.serve(tok("other")), http.StatusNotFound, errBody("no such tenant"))
	// Another tenant's token never reaches this one's environments.
	other := e.mintTenantToken("other")
	expect(t, e.do("GET", "/api/v1/environments/"+w.stage.String(), bearerOf(other), nil), http.StatusOK, "")
	if got := e.envs.last(t); got.tenant != "other" {
		t.Fatalf("the call went to %q", got.tenant)
	}

	member := w.person(t, "m", false, store.Grant{Role: "write"})
	sess := func(method, path, tenant string, h http.Header) *httptest.ResponseRecorder {
		if h == nil {
			h = http.Header{}
		}
		if tenant != "" {
			h.Set(tenantHeader, tenant)
		}
		return e.serve(request{method: method, path: path, header: h,
			cookies: []*http.Cookie{{Name: cookieSession, Value: member}}})
	}
	expect(t, sess("GET", "/api/v1/tenant", testTenant, nil), http.StatusOK, `{"tenant":"acme"}`+"\n")
	expect(t, sess("GET", "/api/v1/tenant", "", nil), http.StatusBadRequest,
		errBody("a session names its tenant in the FeRun-Tenant header"))
	expect(t, sess("GET", "/api/v1/tenant", "other", nil), http.StatusNotFound, errBody("no such tenant"))
	expect(t, sess("GET", "/api/v1/tenant", "nope", nil), http.StatusNotFound, errBody("no such tenant"))
	// CSRF: a change needs the header, and an Origin the public URL's.
	expect(t, sess("POST", "/api/v1/environments", testTenant, nil), http.StatusForbidden,
		errBody("missing X-Requested-With header"))
	expect(t, sess("POST", "/api/v1/environments", testTenant, http.Header{csrfHeader: {csrfValue},
		"Origin": {"https://evil.example"}}), http.StatusForbidden, errBody("cross-origin request refused"))
	expect(t, sess("POST", "/api/v1/environments", testTenant, http.Header{csrfHeader: {csrfValue},
		"Origin": {testPublicURL}}), http.StatusOK, "")
	// An operator is admin in every tenant.
	op := w.person(t, "op", true)
	w2 := e.serve(request{method: "GET", path: "/api/v1/tenant", header: http.Header{tenantHeader: {"other"}},
		cookies: []*http.Cookie{{Name: cookieSession, Value: op}}})
	expect(t, w2, http.StatusOK, `{"tenant":"other"}`+"\n")
	// A session of the version before people (no person) is no
	// principal; nor is an unknown cookie.
	if _, err := e.st.InsertUISession(context.Background(), store.UISession{IDHash: store.HashToken("old"), Admin: true},
		time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"old", "nope"} {
		w2 = e.serve(request{method: "GET", path: "/api/v1/tenant", header: http.Header{tenantHeader: {testTenant}},
			cookies: []*http.Cookie{{Name: cookieSession, Value: c}}})
		expect(t, w2, http.StatusUnauthorized, errBody("tenant bearer required"))
	}
	// A disabled person's session is none.
	if _, err := e.st.SetPersonDisabled(context.Background(), w.persons["m"], true); err != nil {
		t.Fatal(err)
	}
	expect(t, sess("GET", "/api/v1/tenant", testTenant, nil), http.StatusUnauthorized, errBody("tenant bearer required"))
}

// boundDomains is a domains service with one domain bound to *env and
// one unbound.
type boundDomains struct {
	fakeDomains
	env *string
}

func (b boundDomains) domain() domains.Domain {
	env := *b.env
	return domains.Domain{ID: "d1", Name: "shop.example.com", Environment: &env}
}
func (b boundDomains) ListDomains(context.Context, string, string, map[string]string) (domains.List, error) {
	return domains.List{Domains: []domains.Domain{b.domain(), {ID: "d2", Name: "free.example.com"}}}, nil
}
func (b boundDomains) GetDomain(context.Context, string, string) (domains.Domain, error) {
	return b.domain(), nil
}
func (b boundDomains) BindDomain(context.Context, string, string, string) (domains.Domain, error) {
	return b.domain(), nil
}
func (b boundDomains) UnbindDomain(context.Context, string, string) (domains.Domain, error) {
	return b.domain(), nil
}

// The lists answer what the principal may read: the environments it
// holds a role on, the domains bound to them; a domain's binding needs
// admin on the environment it leaves and on the one it joins.
func TestListsAndBindings(t *testing.T) {
	t.Parallel()
	bound := new(string)
	w := newPeopleWorld(t, envOptions{domains: func(*db.DB, *store.Store) Domains { return boundDomains{env: bound} }})
	*bound = w.stage.String()
	e := w.e
	e.envs.answer = func(c envCall) (json.RawMessage, error) {
		if c.method != "ListEnvs" {
			return nil, nil
		}
		return json.RawMessage(`{"environments":[{"id":"` + w.stage.String() + `","name":"stage"},{"id":"` +
			w.prod.String() + `","name":"prod"}]}`), nil
	}
	ps := w.principals(t)
	names := func(p principal, path, key string) string {
		resp := e.serve(p.request("GET", path, nil))
		expect(t, resp, http.StatusOK, "")
		var out map[string][]struct{ Name string }
		if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var ns []string
		for _, x := range out[key] {
			ns = append(ns, x.Name)
		}
		return strings.Join(ns, ",")
	}
	for _, c := range []struct {
		p            principal
		envs, domain string
	}{
		{ps[0], "stage,prod", "shop.example.com,free.example.com"},
		{ps[3], "stage", "shop.example.com"},
		{ps[8], "stage", "shop.example.com"},
		{ps[9], "prod", ""},
		{ps[5], "stage,prod", "shop.example.com,free.example.com"},
	} {
		if got := names(c.p, "/api/v1/environments", "environments"); got != c.envs {
			t.Errorf("%s: environments %q, want %q", c.p.name, got, c.envs)
		}
		if got := names(c.p, "/api/v1/domains", "domains"); got != c.domain {
			t.Errorf("%s: domains %q, want %q", c.p.name, got, c.domain)
		}
	}
	// A domain bound to an environment the principal does not read is
	// absent.
	expect(t, e.serve(ps[9].request("GET", "/api/v1/domains/d1", nil)), http.StatusNotFound, errBody("no such domain"))
	expect(t, e.serve(ps[8].request("GET", "/api/v1/domains/d1", nil)), http.StatusOK, "")

	bind := func(p principal, env string) *httptest.ResponseRecorder {
		return e.serve(p.request("PUT", "/api/v1/domains/d1/environment", map[string]string{"environment": env}))
	}
	// Admin on stage (bound) but not on prod (joined): refused.
	expect(t, bind(ps[5], "prod"), http.StatusForbidden, "")
	expect(t, bind(ps[5], "stage"), http.StatusOK, "")
	expect(t, e.serve(ps[5].request("DELETE", "/api/v1/domains/d1/environment", nil)), http.StatusOK, "")
	// stage read only: the binding is visible, the role too low.
	expect(t, bind(ps[8], "stage"), http.StatusForbidden, "")
	expect(t, e.serve(ps[8].request("DELETE", "/api/v1/domains/d1/environment", nil)), http.StatusForbidden, "")
	// prod write: the domain is not seen at all.
	expect(t, bind(ps[9], "prod"), http.StatusNotFound, errBody("no such domain"))
	// The tenant's admin binds anywhere.
	expect(t, bind(ps[7], "prod"), http.StatusOK, "")
	expect(t, bind(ps[0], "prod"), http.StatusOK, "")
}

// ferund records the principal on what it makes: a sandbox and its token
// by a person's session (an operator's as theirs), a tenant token by an
// operator by Basic.
func TestMadeBy(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	ps := w.principals(t)
	madeBy := func(id string) string {
		var by string
		if err := e.st.Q().QueryRow(context.Background(),
			"SELECT s.made_by || '|' || t.made_by FROM sandboxes s JOIN sandbox_tokens t ON t.sandbox_id = s.id WHERE s.id = $1",
			id).Scan(&by); err != nil {
			t.Fatal(err)
		}
		return by
	}
	for _, c := range []struct {
		p    principal
		want string
	}{
		{ps[6], "person:" + w.persons["tw"].String()},
		{ps[4], "person:" + w.persons["op"].String()},
		{ps[2], "token:" + store.HashToken(ps[2].token)},
	} {
		resp := e.serve(c.p.request("POST", "/api/v1/sandboxes", map[string]any{}))
		expect(t, resp, http.StatusCreated, "")
		id := decode[createSandboxResponse](t, resp).Status.SandboxID
		if got := madeBy(id); got != c.want+"|"+c.want {
			t.Fatalf("%s: made_by %q, want %q", c.p.name, got, c.want)
		}
	}
	resp := e.do("POST", "/api/v1/admin/tenants/acme/tokens", adminAuth(), map[string]string{"role": "read",
		"environment": "stage", "label": "ci"})
	expect(t, resp, http.StatusCreated, "")
	toks, err := e.st.ListTenantTokens(context.Background(), testTenant)
	if err != nil {
		t.Fatal(err)
	}
	last := toks[len(toks)-1]
	if last.Label != "ci" || last.Role != "read" || last.EnvironmentID != w.stage || last.MadeBy != "basic:"+testAdmin {
		t.Fatalf("token %+v", last)
	}
	resp = e.do("POST", "/api/v1/admin/tenants/acme/tokens", adminAuth(), map[string]string{"role": "owner"})
	expect(t, resp, http.StatusBadRequest, errBody(`role must be read, write or admin, got "owner"`))
	resp = e.do("POST", "/api/v1/admin/tenants/acme/tokens", adminAuth(), map[string]string{"environment": "nope"})
	expect(t, resp, http.StatusNotFound, errBody("tenant acme has no environment nope"))
}
