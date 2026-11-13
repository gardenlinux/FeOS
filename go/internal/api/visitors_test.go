// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/visits"
)

// *visits.Service is what the API takes as its Visitors.
var _ Visitors = (*visits.Service)(nil)

// visitorCall is one call the fake visits service took.
type visitorCall struct {
	method string
	tenant string
	name   string
	caller visits.Caller
	sealed string
	accept visits.Accept
	in     visits.ProviderIn
	madeBy string
	end    visits.EndSession
	id     string
}

// fakeVisitors is the visits service of the API's tests: it records every
// call and answers err when set, else a canned answer; its providers live
// in a map, a PUT that changes nothing keeps updatedAt.
type fakeVisitors struct {
	mu        sync.Mutex
	calls     []visitorCall
	err       error
	providers map[string]visits.Provider
	version   int
}

func (f *fakeVisitors) record(c visitorCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return f.err
}

// last is the last call the fake took.
func (f *fakeVisitors) last(t *testing.T) visitorCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no call reached the visits service")
	}
	return f.calls[len(f.calls)-1]
}

// count is how many calls the fake took.
func (f *fakeVisitors) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeVisitors) Kinds() []string { return []string{model.ProviderKindTenant} }

func (f *fakeVisitors) ListProviders(_ context.Context, tenant string) (visits.ProviderList, error) {
	if err := f.record(visitorCall{method: "ListProviders", tenant: tenant}); err != nil {
		return visits.ProviderList{}, err
	}
	return visits.ProviderList{Providers: []visits.Provider{{Name: "ferun", Kind: "ferun", BuiltIn: true,
		Environments: []visits.EnvRef{}}}, Kinds: f.Kinds()}, nil
}

func (f *fakeVisitors) GetProvider(_ context.Context, tenant, name string) (visits.Provider, error) {
	if err := f.record(visitorCall{method: "GetProvider", tenant: tenant, name: name}); err != nil {
		return visits.Provider{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.providers[tenant+"/"+name]
	if !ok {
		return visits.Provider{}, apierr.New(http.StatusNotFound, "no such provider")
	}
	return p, nil
}

func (f *fakeVisitors) PutProvider(_ context.Context, tenant, name string, in visits.ProviderIn, madeBy string) (
	visits.Provider, bool, error) {
	if err := f.record(visitorCall{method: "PutProvider", tenant: tenant, name: name, in: in, madeBy: madeBy}); err != nil {
		return visits.Provider{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.providers == nil {
		f.providers = map[string]visits.Provider{}
	}
	old, ok := f.providers[tenant+"/"+name]
	if ok && old.Kind == in.Kind && old.URL == in.URL {
		return old, false, nil
	}
	f.version++
	p := visits.Provider{Name: name, Kind: in.Kind, URL: in.URL, Environments: []visits.EnvRef{},
		CreatedAt: old.CreatedAt, UpdatedAt: fmt.Sprintf("v%d", f.version)}
	if !ok {
		p.CreatedAt = p.UpdatedAt
	}
	f.providers[tenant+"/"+name] = p
	return p, !ok, nil
}

func (f *fakeVisitors) DeleteProvider(_ context.Context, tenant, name string) error {
	return f.record(visitorCall{method: "DeleteProvider", tenant: tenant, name: name})
}

func (f *fakeVisitors) OpenChallenge(_ context.Context, c visits.Caller, sealed string) (visits.Challenge, error) {
	if err := f.record(visitorCall{method: "OpenChallenge", caller: c, sealed: sealed}); err != nil {
		return visits.Challenge{}, err
	}
	return visits.Challenge{Host: "stage--kino.apps.example.test", Path: "/", Restart: "https://stage--kino.apps.example.test/",
		Provider: visits.ProviderRef{Name: "fecode", Kind: "tenant"}}, nil
}

func (f *fakeVisitors) AcceptChallenge(_ context.Context, c visits.Caller, in visits.Accept) (visits.Answer, error) {
	if err := f.record(visitorCall{method: "AcceptChallenge", caller: c, accept: in}); err != nil {
		return visits.Answer{}, err
	}
	return visits.Answer{Redirect: "https://stage--kino.apps.example.test/.ferun/auth?code=c", Admitted: true,
		Visit: "00000000-0000-4000-8000-0000000000a1"}, nil
}

func (f *fakeVisitors) RejectChallenge(_ context.Context, c visits.Caller, sealed string) (visits.Answer, error) {
	if err := f.record(visitorCall{method: "RejectChallenge", caller: c, sealed: sealed}); err != nil {
		return visits.Answer{}, err
	}
	return visits.Answer{Redirect: "https://stage--kino.apps.example.test/.ferun/auth?code=r"}, nil
}

func (f *fakeVisitors) GetVisit(_ context.Context, c visits.Caller, id string) (visits.Visit, error) {
	if err := f.record(visitorCall{method: "GetVisit", caller: c, id: id}); err != nil {
		return visits.Visit{}, err
	}
	return visits.Visit{ID: id, Subject: "u-1"}, nil
}

func (f *fakeVisitors) EndBySession(_ context.Context, in visits.EndSession) (int, error) {
	if err := f.record(visitorCall{method: "EndBySession", end: in}); err != nil {
		return 0, err
	}
	return 2, nil
}

func (f *fakeVisitors) OIDCProvider(_ context.Context, tenant, name string) (visits.OIDCProvider, error) {
	if err := f.record(visitorCall{method: "OIDCProvider", tenant: tenant, name: name}); err != nil {
		return visits.OIDCProvider{}, err
	}
	return visits.OIDCProvider{Tenant: tenant, Name: name, Issuer: "https://login.example.test/", ClientID: "abc",
		ClientSecret: "s3cret", Rule: visits.Rule{Anyone: true}}, nil
}

// mintServiceToken stores a new service token of kind and returns it with
// its row.
func (e *env) mintServiceToken(kind, name string) (string, store.ServiceToken) {
	e.t.Helper()
	tok, hash, err := pki.NewServiceToken(kind)
	if err != nil {
		e.t.Fatal(err)
	}
	row, err := e.st.CreateServiceToken(context.Background(), hash, kind, name, "test")
	if err != nil {
		e.t.Fatal(err)
	}
	return tok, row
}

// The provider class: ferun-auth's token reaches the challenge and visit
// routes as Caller{Auth}, a tenant's token as Caller{Tenant} whose Admin
// is its role on the environment; a sink's token, a feos-auth- token that
// is no row of kind auth and nothing at all are 401, each with its text. The auth class
// takes ferun-auth's token only.
func TestVisitorClasses(t *testing.T) {
	t.Parallel()
	fv := &fakeVisitors{}
	w := newPeopleWorld(t, envOptions{visitors: fv})
	e := w.e
	authTok, authRow := e.mintServiceToken(model.ServiceAuth, "ferun-auth")
	sinkTok, _ := e.mintServiceToken(model.ServiceSink, "sink-0")
	unknown, _, _ := pki.NewServiceToken(model.ServiceAuth)
	// A feos-auth- token whose row is of another kind is none either.
	misfiled, _, _ := pki.NewServiceToken(model.ServiceAuth)
	if _, err := e.st.CreateServiceToken(context.Background(), store.HashToken(misfiled), model.ServiceSink, "x", "test"); err != nil {
		t.Fatal(err)
	}
	stageAdmin := w.token(t, "admin", w.stage)

	providerRoutes := []struct{ method, path, body string }{
		{"POST", "/api/v1/challenges/open", `{"challenge":"sealed-1"}`},
		{"POST", "/api/v1/challenges/accept", `{"challenge":"sealed-1","subject":"u-1"}`},
		{"POST", "/api/v1/challenges/reject", `{"challenge":"sealed-1"}`},
		{"GET", "/api/v1/visits/00000000-0000-4000-8000-0000000000a1", ""},
	}
	for _, rt := range providerRoutes {
		// ferun-auth.
		n := fv.count()
		expect(t, e.do(rt.method, rt.path, bearerOf(authTok), rt.body), http.StatusOK, "")
		c := fv.last(t)
		if fv.count() != n+1 || !c.caller.Auth || c.caller.Tenant != "" || c.caller.Admin != nil ||
			c.caller.MadeBy != "service:"+authRow.ID.String() {
			t.Fatalf("%s %s as ferun-auth: %+v", rt.method, rt.path, c.caller)
		}
		// The tenant's admin token: admin on every environment.
		expect(t, e.do(rt.method, rt.path, bearerOf(e.tenantToken), rt.body), http.StatusOK, "")
		c = fv.last(t)
		if c.caller.Auth || c.caller.Tenant != testTenant || c.caller.Admin == nil || !c.caller.Admin(w.stage) ||
			c.caller.MadeBy != "token:"+store.HashToken(e.tenantToken) {
			t.Fatalf("%s %s as the tenant: %+v", rt.method, rt.path, c.caller)
		}
		// A token that is admin on stage alone.
		expect(t, e.do(rt.method, rt.path, bearerOf(stageAdmin), rt.body), http.StatusOK, "")
		c = fv.last(t)
		if c.caller.Tenant != testTenant || !c.caller.Admin(w.stage) || c.caller.Admin(w.prod) ||
			c.caller.Admin(uuid.New()) {
			t.Fatalf("%s %s as stage's admin: %+v", rt.method, rt.path, c.caller)
		}
		// Refusals reach nothing.
		n = fv.count()
		// A sink's token is no feos-auth- token: it is looked up as a
		// tenant's, and is none.
		expect(t, e.do(rt.method, rt.path, bearerOf(sinkTok), rt.body), http.StatusUnauthorized,
			errBody("unknown tenant token"))
		expect(t, e.do(rt.method, rt.path, bearerOf(unknown), rt.body), http.StatusUnauthorized,
			errBody("unknown service token"))
		expect(t, e.do(rt.method, rt.path, bearerOf(misfiled), rt.body), http.StatusUnauthorized,
			errBody("unknown service token"))
		expect(t, e.do(rt.method, rt.path, bearerOf("nope"), rt.body), http.StatusUnauthorized,
			errBody("unknown tenant token"))
		resp := e.do(rt.method, rt.path, "", rt.body)
		expect(t, resp, http.StatusUnauthorized, errBody("provider credential required"))
		if got := resp.Header().Get("WWW-Authenticate"); got != `Bearer realm="ferun"` {
			t.Fatalf("WWW-Authenticate %q", got)
		}
		if fv.count() != n {
			t.Fatalf("%s %s: a refused call reached the service", rt.method, rt.path)
		}
	}

	// The auth class.
	expect(t, e.do("POST", "/api/v1/visits/end", bearerOf(authTok), `{"provider":"ferun","session":"sid-1"}`),
		http.StatusOK, `{"ended":2}`+"\n")
	if c := fv.last(t); c.method != "EndBySession" || c.end != (visits.EndSession{Provider: "ferun", Session: "sid-1"}) {
		t.Fatalf("end %+v", c)
	}
	expect(t, e.do("GET", "/api/v1/oidc-providers/acme/sso", bearerOf(authTok), nil), http.StatusOK,
		`{"tenant":"acme","name":"sso","issuer":"https://login.example.test/","clientId":"abc","clientSecret":"s3cret",`+
			`"rule":{"anyone":true}}`+"\n")
	if c := fv.last(t); c.method != "OIDCProvider" || c.tenant != "acme" || c.name != "sso" {
		t.Fatalf("oidc provider %+v", c)
	}
	n := fv.count()
	for _, authz := range []string{"", bearerOf(sinkTok), bearerOf(unknown), bearerOf(misfiled), bearerOf(e.tenantToken),
		adminAuth()} {
		for _, rt := range []struct{ method, path string }{
			{"POST", "/api/v1/visits/end"}, {"GET", "/api/v1/oidc-providers/acme/sso"},
		} {
			resp := e.do(rt.method, rt.path, authz, `{"provider":"ferun","session":"sid-1"}`)
			expect(t, resp, http.StatusUnauthorized, errBody("ferun-auth token required"))
			if got := resp.Header().Get("WWW-Authenticate"); got != `Bearer realm="ferun"` {
				t.Fatalf("WWW-Authenticate %q", got)
			}
		}
	}
	// ferun-auth's token is no tenant's: the tenant routes refuse it.
	expect(t, e.do("GET", "/api/v1/providers", bearerOf(authTok), nil), http.StatusUnauthorized,
		errBody("unknown tenant token"))
	if fv.count() != n {
		t.Fatal("a refused call reached the service")
	}

	// Its use is touched.
	got, err := e.st.ServiceTokenOf(context.Background(), store.HashToken(authTok))
	if err != nil || got.LastUsedAt.IsZero() {
		t.Fatalf("ferun-auth's token %+v %v", got, err)
	}
}

// A person's session reaches the provider routes as a tenant's principal,
// with the CSRF rules of a session: a POST needs X-Requested-With, an
// Origin the public URL's.
func TestVisitorSession(t *testing.T) {
	t.Parallel()
	fv := &fakeVisitors{}
	w := newPeopleWorld(t, envOptions{visitors: fv})
	e := w.e
	cookie := w.person(t, "ra", false, store.Grant{Role: "read"}, store.Grant{Role: "admin", EnvironmentID: w.stage})
	sess := func(h http.Header) request {
		if h == nil {
			h = http.Header{}
		}
		h.Set(tenantHeader, testTenant)
		return request{method: "POST", path: "/api/v1/challenges/open", body: `{"challenge":"sealed-1"}`, header: h,
			cookies: []*http.Cookie{{Name: cookieSession, Value: cookie}}}
	}
	n := fv.count()
	expect(t, e.serve(sess(nil)), http.StatusForbidden, errBody("missing X-Requested-With header"))
	expect(t, e.serve(sess(http.Header{csrfHeader: {csrfValue}, "Origin": {"https://evil.example"}})),
		http.StatusForbidden, errBody("cross-origin request refused"))
	if fv.count() != n {
		t.Fatal("a refused call reached the service")
	}
	expect(t, e.serve(sess(http.Header{csrfHeader: {csrfValue}, "Origin": {testPublicURL}})), http.StatusOK, "")
	c := fv.last(t)
	if c.caller.Auth || c.caller.Tenant != testTenant || c.caller.MadeBy != "person:"+w.persons["ra"].String() ||
		!c.caller.Admin(w.stage) || c.caller.Admin(w.prod) || c.sealed != "sealed-1" {
		t.Fatalf("session caller %+v", c)
	}
	// The session names its tenant; one it has no grant in is absent.
	h := http.Header{csrfHeader: {csrfValue}}
	rq := sess(h)
	rq.header.Set(tenantHeader, "other")
	expect(t, e.serve(rq), http.StatusNotFound, errBody("no such tenant"))
}

// The bodies are decoded into the visits types (unknown fields ignored),
// at most 64 KiB; the service's refusals pass through as they are.
func TestVisitorBodiesAndErrors(t *testing.T) {
	t.Parallel()
	fv := &fakeVisitors{}
	e := newEnv(t, envOptions{visitors: fv})
	tok := bearerOf(e.tenantToken)
	expect(t, e.do("POST", "/api/v1/challenges/accept", tok, `{"challenge":"sealed-1","subject":"u-1",`+
		`"email":"a@example.test","name":"Ada","session":"s-1","endsAt":"2026-11-13T08:00:00Z","extra":true}`),
		http.StatusOK, `{"redirect":"https://stage--kino.apps.example.test/.ferun/auth?code=c","admitted":true,`+
			`"visit":"00000000-0000-4000-8000-0000000000a1"}`+"\n")
	want := visits.Accept{Challenge: "sealed-1", Subject: "u-1", Email: "a@example.test", Name: "Ada", Session: "s-1",
		EndsAt: "2026-11-13T08:00:00Z"}
	if got := fv.last(t).accept; got != want {
		t.Fatalf("accept %+v", got)
	}
	expect(t, e.do("POST", "/api/v1/challenges/reject", tok, `{"challenge":"sealed-2","other":1}`), http.StatusOK,
		`{"redirect":"https://stage--kino.apps.example.test/.ferun/auth?code=r","admitted":false}`+"\n")
	if got := fv.last(t).sealed; got != "sealed-2" {
		t.Fatalf("reject %q", got)
	}
	expect(t, e.do("GET", "/api/v1/visits/v-1", tok, nil), http.StatusOK, "")
	if got := fv.last(t).id; got != "v-1" {
		t.Fatalf("visit %q", got)
	}

	// Bodies that are not a JSON object, or too large, reach nothing.
	n := fv.count()
	expect(t, e.do("POST", "/api/v1/challenges/open", tok, `["x"]`), http.StatusBadRequest, "")
	expect(t, e.do("POST", "/api/v1/challenges/open", tok, `{"challenge":5}`), http.StatusBadRequest, "")
	big := `{"challenge":"` + strings.Repeat("a", 64<<10) + `"}`
	expect(t, e.do("POST", "/api/v1/challenges/open", tok, big), http.StatusRequestEntityTooLarge,
		errBody("request body larger than 65536 bytes"))
	if fv.count() != n {
		t.Fatal("a refused body reached the service")
	}
	// Just under the bound is read.
	fits := `{"challenge":"` + strings.Repeat("a", 64<<10-16) + `"}`
	expect(t, e.do("POST", "/api/v1/challenges/open", tok, fits), http.StatusOK, "")

	// The service's errors pass through.
	for _, c := range []struct {
		err  error
		code int
		body string
	}{
		{apierr.New(http.StatusNotFound, "no such challenge"), http.StatusNotFound, errBody("no such challenge")},
		{apierr.WithCode(http.StatusConflict, "challenge_answered", "answered"), http.StatusConflict,
			`{"error":"answered","code":"challenge_answered"}` + "\n"},
		{apierr.New(http.StatusInternalServerError, "the visits service failed"), http.StatusInternalServerError,
			errBody("the visits service failed")},
	} {
		fv.mu.Lock()
		fv.err = c.err
		fv.mu.Unlock()
		expect(t, e.do("POST", "/api/v1/challenges/open", tok, `{"challenge":"s"}`), c.code, c.body)
		expect(t, e.do("GET", "/api/v1/providers", tok, nil), c.code, c.body)
	}
}

// The tenant's providers: the routes pass the tenant, the name and the
// body; a make is 201, a change or an unchanged PUT 200, and only a make,
// a change or a delete writes an activity row.
func TestProviderRoutes(t *testing.T) {
	t.Parallel()
	fv := &fakeVisitors{}
	e := newEnv(t, envOptions{visitors: fv})
	tok := bearerOf(e.tenantToken)
	rows := func(kind string) int {
		t.Helper()
		var n int
		if err := e.d.Pool.QueryRow(context.Background(), "SELECT count(*) FROM activity WHERE kind = $1 AND tenant_id = $2",
			kind, testTenant).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	expect(t, e.do("GET", "/api/v1/providers", tok, nil), http.StatusOK,
		`{"providers":[{"name":"ferun","kind":"ferun","builtIn":true,"environments":[]}],"kinds":["tenant"]}`+"\n")
	if c := fv.last(t); c.method != "ListProviders" || c.tenant != testTenant {
		t.Fatalf("list %+v", c)
	}
	body := `{"kind":"tenant","url":"https://fecode.example.test/api/v1/visit","ignored":1}`
	expect(t, e.do("PUT", "/api/v1/providers/fecode", tok, body), http.StatusCreated, "")
	c := fv.last(t)
	if c.method != "PutProvider" || c.tenant != testTenant || c.name != "fecode" ||
		c.in.Kind != "tenant" || c.in.URL != "https://fecode.example.test/api/v1/visit" ||
		c.madeBy != "token:"+store.HashToken(e.tenantToken) {
		t.Fatalf("put %+v", c)
	}
	if rows("access_provider_put") != 1 {
		t.Fatal("no activity row for the make")
	}
	// The same PUT again: 200, nothing recorded.
	expect(t, e.do("PUT", "/api/v1/providers/fecode", tok, body), http.StatusOK, "")
	if rows("access_provider_put") != 1 {
		t.Fatal("an unchanged PUT wrote an activity row")
	}
	expect(t, e.do("PUT", "/api/v1/providers/fecode", tok, `{"kind":"tenant","url":"https://other.example.test/v"}`),
		http.StatusOK, "")
	if rows("access_provider_put") != 2 {
		t.Fatal("no activity row for the change")
	}
	resp := e.do("GET", "/api/v1/providers/fecode", tok, nil)
	expect(t, resp, http.StatusOK, "")
	if p := decode[visits.Provider](t, resp); p.Name != "fecode" || p.URL != "https://other.example.test/v" {
		t.Fatalf("get %+v", p)
	}
	expect(t, e.do("DELETE", "/api/v1/providers/fecode", tok, nil), http.StatusOK, `{"deleted":true}`+"\n")
	if c := fv.last(t); c.method != "DeleteProvider" || c.tenant != testTenant || c.name != "fecode" {
		t.Fatalf("delete %+v", c)
	}
	if rows("access_provider_deleted") != 1 {
		t.Fatal("no activity row for the delete")
	}
	// A failed PUT or DELETE records nothing.
	fv.mu.Lock()
	fv.err = apierr.WithCode(http.StatusConflict, "provider_in_use", "provider fecode is named by 1 environments")
	fv.mu.Unlock()
	expect(t, e.do("DELETE", "/api/v1/providers/fecode", tok, nil), http.StatusConflict, "")
	if rows("access_provider_deleted") != 1 {
		t.Fatal("a refused delete wrote an activity row")
	}
	// The body is never more than 64 KiB.
	expect(t, e.do("PUT", "/api/v1/providers/fecode", tok, `{"kind":"`+strings.Repeat("a", 64<<10)+`"}`),
		http.StatusRequestEntityTooLarge, "")
}

// A call with ferun-auth's token that came through the Gateway
// (X-Forwarded-For) is warned of, once a minute per token, naming the
// token's ID and never the token; a call without it is not.
func TestAuthTokenFromOutside(t *testing.T) {
	t.Parallel()
	fv := &fakeVisitors{}
	e := newEnv(t, envOptions{visitors: fv})
	var buf syncBuffer
	e.srv.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	authTok, row := e.mintServiceToken(model.ServiceAuth, "ferun-auth")
	call := func(path, body string, forwarded bool) {
		t.Helper()
		h := http.Header{"Authorization": {bearerOf(authTok)}}
		if forwarded {
			h.Set("X-Forwarded-For", "203.0.113.7")
		}
		method := "POST"
		if body == "" {
			method = "GET"
		}
		expect(t, e.serve(request{method: method, path: path, body: body, header: h}), http.StatusOK, "")
	}
	call("/api/v1/challenges/open", `{"challenge":"sealed-1"}`, false)
	if strings.Contains(buf.String(), "outside the cluster") {
		t.Fatalf("warned without X-Forwarded-For: %s", buf.String())
	}
	call("/api/v1/challenges/open", `{"challenge":"sealed-1"}`, true)
	call("/api/v1/oidc-providers/acme/sso", "", true)
	call("/api/v1/visits/end", `{"provider":"ferun","session":"s"}`, true)
	out := buf.String()
	if n := strings.Count(out, "ferun-auth's token used from outside the cluster"); n != 1 {
		t.Fatalf("%d warnings:\n%s", n, out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, row.ID.String()) || strings.Contains(out, authTok) ||
		strings.Contains(out, "sealed-1") {
		t.Fatalf("warning %s", out)
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The answers are the visits types' JSON as they come: a challenge's
// view, field for field.
func TestVisitorAnswerShape(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{visitors: &fakeVisitors{}})
	resp := e.do("POST", "/api/v1/challenges/open", bearerOf(e.tenantToken), `{"challenge":"s"}`)
	expect(t, resp, http.StatusOK, "")
	var got map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"environment", "host", "path", "expiresAt", "expired", "answered", "restart", "provider"} {
		if _, ok := got[k]; !ok {
			t.Errorf("no %q in %s", k, resp.Body.String())
		}
	}
	if _, ok := got["oidc"]; ok {
		t.Errorf("oidc without one: %s", resp.Body.String())
	}
}
