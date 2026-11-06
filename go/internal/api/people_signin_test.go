// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/store"
)

// mkEnvRow makes an environment row of tenant and returns its ID.
func mkEnvRow(t *testing.T, e *env, tenant, name string) uuid.UUID {
	t.Helper()
	row, err := e.st.InsertEnvironment(context.Background(), store.NewEnvironment{ID: uuid.New(), Owner: tenant, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}

// withSID is claims with the provider's session id.
func withSID(c map[string]any, sid string) map[string]any {
	c["sid"] = sid
	return c
}

// sessionOf reads the session of a cookie.
func sessionOf(t *testing.T, e *env, c *http.Cookie) store.UISession {
	t.Helper()
	sess, err := e.st.UISessionOf(context.Background(), store.HashToken(c.Value))
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// Everyone the provider lets in gets a session: a person who is no
// operator is recorded, sees their tenants with their roles at /auth/me
// and is refused the operators' routes; a disabled person loses their
// sessions and gets none.
func TestSignInPerson(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t, algRS256)
	a, b := oidcEnvs(t, p, false)
	sc := signIn(t, a, b, p, withSID(userClaims(), "sid-bob"))
	sess := sessionOf(t, a, sc)
	if sess.Admin || sess.SID != "sid-bob" || sess.PersonID == uuid.Nil {
		t.Fatalf("session %+v", sess)
	}
	person, err := a.st.GetPerson(context.Background(), sess.PersonID)
	if err != nil || person.Issuer != p.issuer || person.Sub != "0b8c5ef1-0000-4000-8000-000000000002" ||
		person.Email != "bob@example.test" || person.Name != "Ada" {
		t.Fatalf("person %+v %v", person, err)
	}
	getMe := func() me {
		w := b.serve(withCookie(request{method: "GET", path: "/auth/me"}, sc))
		expect(t, w, http.StatusOK, "")
		return decode[me](t, w)
	}
	if m := getMe(); m.Admin || m.Person != person.ID.String() || len(m.Tenants) != 0 || m.Tenants == nil {
		t.Fatalf("me %+v", m)
	}
	expect(t, a.serve(withCookie(request{method: "GET", path: "/api/v1/admin/tenants"}, sc)), http.StatusForbidden,
		errBody("operators only"))

	// A second sign-in is the same person.
	sc2 := signIn(t, a, b, p, userClaims())
	if s2 := sessionOf(t, a, sc2); s2.PersonID != person.ID || s2.SID != "" {
		t.Fatalf("second session %+v", s2)
	}

	stage := mkEnvRow(t, a, testTenant, "stage")
	mustTenant(t, a, "other")
	for _, g := range []store.Grant{{TenantID: testTenant, PersonID: person.ID, EnvironmentID: stage, Role: "write"},
		{TenantID: "other", PersonID: person.ID, Role: "read"}} {
		if _, err := a.st.PutGrant(context.Background(), g); err != nil {
			t.Fatal(err)
		}
	}
	m := getMe()
	if len(m.Tenants) != 2 || m.Tenants[0].Tenant != testTenant || m.Tenants[0].Role != "" ||
		len(m.Tenants[0].Environments) != 1 || m.Tenants[0].Environments[0] != (meEnvironment{ID: stage.String(),
		Name: "stage", Role: "write"}) || m.Tenants[1].Tenant != "other" || m.Tenants[1].Role != "read" ||
		len(m.Tenants[1].Environments) != 0 {
		t.Fatalf("me %+v", m)
	}

	// Disabled: every session ends, and a sign-in gets none.
	if _, err := a.st.SetPersonDisabled(context.Background(), person.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*http.Cookie{sc, sc2} {
		expect(t, b.serve(withCookie(request{method: "GET", path: "/auth/me"}, c)), http.StatusUnauthorized,
			errBody("not signed in"))
	}
	loc, lc := login(t, a, "/")
	w := callback(t, b, loc, p.issue(loc, userClaims()), lc)
	expect(t, w, http.StatusForbidden, "")
	if !strings.Contains(w.Body.String(), "Your account bob@example.test is disabled in FeRun.") ||
		cookieNamed(w, cookieSession) != nil {
		t.Fatalf("disabled sign-in: %s", w.Body.String())
	}
	// Enabled again, the grants are there.
	if _, err := a.st.SetPersonDisabled(context.Background(), person.ID, false); err != nil {
		t.Fatal(err)
	}
	sc = signIn(t, a, b, p, userClaims())
	if m := getMe(); len(m.Tenants) != 2 {
		t.Fatalf("me after enabling %+v", m)
	}
}

// logoutToken signs a back-channel logout token for ferund's client with
// claims over the valid defaults (nil deletes one).
func (p *testProvider) logoutToken(over map[string]any) string {
	p.t.Helper()
	c := map[string]any{"iss": p.issuer, "aud": testClientID, "iat": time.Now().Unix(), "jti": randomTestString(p.t),
		"sid": "sid-1", "events": map[string]any{backchannelEvent: map[string]any{}}}
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	payload, err := json.Marshal(c)
	if err != nil {
		p.t.Fatal(err)
	}
	p.mu.Lock()
	signer := p.signer
	p.mu.Unlock()
	return signer.sign(p.t, map[string]any{"alg": signer.alg, "kid": signer.kid, "typ": "logout+jwt"}, payload)
}

// backchannel posts a logout token.
func backchannel(e *env, token string) *httptest.ResponseRecorder {
	form := url.Values{}
	if token != "" {
		form.Set("logout_token", token)
	}
	return e.serve(request{method: "POST", path: "/auth/backchannel-logout", body: form.Encode(),
		header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}})
}

// The provider's back-channel logout ends every session of its sid on
// every instance, and only those; a token that does not verify is 400;
// neither answer is cached.
func TestBackchannelLogout(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t, algRS256)
	a, b := oidcEnvs(t, p, false)
	s1 := signIn(t, a, b, p, withSID(userClaims(), "sid-1"))
	s1b := signIn(t, b, a, p, withSID(userClaims(), "sid-1"))
	s2 := signIn(t, a, b, p, withSID(adminClaims(), "sid-2"))

	for _, c := range []struct {
		name string
		tok  string
		want string
	}{
		{"no token", "", "logout_token is required"},
		{"another issuer", p.logoutToken(map[string]any{"iss": "https://evil.example/"}),
			"the token's issuer is not the configured one"},
		{"another client", p.logoutToken(map[string]any{"aud": "fecode"}), "the token is not for this client (aud)"},
		{"old", p.logoutToken(map[string]any{"iat": time.Now().Add(-11 * time.Minute).Unix()}),
			"the token's iat is not recent"},
		{"future", p.logoutToken(map[string]any{"iat": time.Now().Add(5 * time.Minute).Unix()}),
			"the token's iat is not recent"},
		{"expired", p.logoutToken(map[string]any{"exp": time.Now().Add(-5 * time.Minute).Unix()}), "the token has expired"},
		{"no events", p.logoutToken(map[string]any{"events": nil}), "the token has no events"},
		{"another event", p.logoutToken(map[string]any{"events": map[string]any{"urn:x": map[string]any{}}}),
			"the token is not a back-channel logout"},
		{"a nonce", p.logoutToken(map[string]any{"nonce": "n"}), "a logout token has no nonce"},
		{"no sid", p.logoutToken(map[string]any{"sid": nil}), "the token names no session (sid)"},
		{"bad signature", p.logoutToken(nil)[:20] + "x" + p.logoutToken(nil)[21:], ""},
	} {
		w := backchannel(a, c.tok)
		if w.Code != http.StatusBadRequest || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %q %s", c.name, w.Code, w.Header().Get("Cache-Control"), w.Body.String())
		}
		var raw map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &raw)
		if raw["error"] != "invalid_request" || (c.want != "" && raw["error_description"] != c.want) {
			t.Fatalf("%s: %s", c.name, w.Body.String())
		}
	}
	for _, c := range []*http.Cookie{s1, s1b, s2} {
		expect(t, a.serve(withCookie(request{method: "GET", path: "/auth/me"}, c)), http.StatusOK, "")
	}

	w := backchannel(b, p.logoutToken(nil))
	expect(t, w, http.StatusOK, "")
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	for _, c := range []*http.Cookie{s1, s1b} {
		for _, e := range []*env{a, b} {
			expect(t, e.serve(withCookie(request{method: "GET", path: "/auth/me"}, c)), http.StatusUnauthorized,
				errBody("not signed in"))
		}
	}
	expect(t, a.serve(withCookie(request{method: "GET", path: "/auth/me"}, s2)), http.StatusOK, "")
	// A repeat is no error; a sub of another person ends nothing.
	expect(t, backchannel(a, p.logoutToken(nil)), http.StatusOK, "")
	expect(t, backchannel(a, p.logoutToken(map[string]any{"sid": "sid-2", "sub": "someone"})), http.StatusOK, "")
	expect(t, a.serve(withCookie(request{method: "GET", path: "/auth/me"}, s2)), http.StatusOK, "")
	expect(t, backchannel(a, p.logoutToken(map[string]any{"sid": "sid-2", "sub": adminClaims()["sub"]})),
		http.StatusOK, "")
	expect(t, a.serve(withCookie(request{method: "GET", path: "/auth/me"}, s2)), http.StatusUnauthorized,
		errBody("not signed in"))
}
