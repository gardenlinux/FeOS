// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/store"
)

const testPublicURL = "https://run.example.test"

// oidcEnvs are two instances on one database with the same provider.
func oidcEnvs(t *testing.T, p *testProvider, basic bool) (*env, *env) {
	t.Helper()
	o := envOptions{oidc: p.config(), publicURL: testPublicURL, noAdminUsers: !basic}
	a := newEnv(t, o)
	o.db = a.d
	b := newEnv(t, o)
	b.tenantToken = a.tenantToken
	return a, b
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// login begins a sign-in on e and returns the provider's authorization
// URL and the login cookie.
func login(t *testing.T, e *env, next string) (string, *http.Cookie) {
	t.Helper()
	w := e.serve(request{method: "GET", path: "/auth/login?next=" + url.QueryEscape(next)})
	expect(t, w, http.StatusFound, "")
	c := cookieNamed(w, cookieLogin)
	if c == nil {
		t.Fatal("no login cookie")
	}
	return w.Header().Get("Location"), c
}

// callback completes a sign-in on e.
func callback(t *testing.T, e *env, authURL, code string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	u, _ := url.Parse(authURL)
	q := url.Values{"code": {code}, "state": {u.Query().Get("state")}}
	var cookies []*http.Cookie
	if c != nil {
		cookies = []*http.Cookie{c}
	}
	return e.serve(request{method: "GET", path: "/auth/callback?" + q.Encode(), cookies: cookies})
}

// signIn signs claims in, beginning on a and completing on b, and returns
// the session cookie.
func signIn(t *testing.T, a, b *env, p *testProvider, claims map[string]any) *http.Cookie {
	t.Helper()
	loc, lc := login(t, a, "/nodes")
	w := callback(t, b, loc, p.issue(loc, claims), lc)
	expect(t, w, http.StatusFound, "")
	sc := cookieNamed(w, cookieSession)
	if sc == nil {
		t.Fatalf("no session cookie: %s", w.Body.String())
	}
	// __Host-: Secure, Path=/, no Domain, even on http; the old name is
	// cleared.
	if !sc.Secure || sc.Path != "/" || sc.Domain != "" || !sc.HttpOnly {
		t.Fatalf("session cookie %+v", sc)
	}
	if old := cookieNamed(w, auth.OldSessionCookie); old == nil || old.MaxAge >= 0 {
		t.Fatalf("old session cookie not cleared: %+v", old)
	}
	return sc
}

func withCookie(rq request, c *http.Cookie) request {
	rq.cookies = append(rq.cookies, c)
	return rq
}

// A sign-in begun on one instance completes on another: the cookies'
// attributes, the redirect to where it began, the session on every
// instance, /auth/me, the admin routes with the CSRF rules, Basic winning,
// a replayed callback refused, and the sign-out everywhere.
func TestSignIn(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t, algRS256)
	a, b := oidcEnvs(t, p, true)

	loc, lc := login(t, a, "/nodes")
	if !strings.HasPrefix(loc, p.srv.URL+"/oauth2/auth?") {
		t.Fatalf("location %s", loc)
	}
	u, _ := url.Parse(loc)
	if u.Query().Get("redirect_uri") != testPublicURL+"/auth/callback" {
		t.Fatalf("redirect_uri %s", u.Query().Get("redirect_uri"))
	}
	if lc.Path != "/auth/" || !lc.HttpOnly || !lc.Secure || lc.SameSite != http.SameSiteLaxMode || lc.MaxAge != 600 ||
		lc.Value != u.Query().Get("state") {
		t.Fatalf("login cookie %+v", lc)
	}
	code := p.issue(loc, adminClaims())
	w := callback(t, b, loc, code, lc)
	expect(t, w, http.StatusFound, "")
	if w.Header().Get("Location") != "/nodes" {
		t.Fatalf("redirect %s", w.Header().Get("Location"))
	}
	sc := cookieNamed(w, cookieSession)
	if sc == nil || sc.Path != "/" || !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteLaxMode || sc.MaxAge != 3600 {
		t.Fatalf("session cookie %+v", sc)
	}
	if c := cookieNamed(w, cookieLogin); c == nil || c.MaxAge >= 0 {
		t.Fatalf("login cookie not cleared: %+v", c)
	}
	// The same callback again: the sign-in is used up.
	w = callback(t, b, loc, code, lc)
	expect(t, w, http.StatusBadRequest, "")
	if !strings.Contains(w.Body.String(), "This sign-in has expired. Start again.") {
		t.Fatalf("replay: %s", w.Body.String())
	}

	// The session works on both instances.
	for _, e := range []*env{a, b} {
		w = e.serve(withCookie(request{method: "GET", path: "/auth/me"}, sc))
		expect(t, w, http.StatusOK, "")
		if me := decode[me](t, w); me.Sub != "0b8c5ef1-0000-4000-8000-000000000001" || me.Email != "ada@example.test" ||
			me.Name != "Ada" || me.Username != "ada" || me.Picture != "https://example.test/ada.png" || !me.Admin ||
			me.Method != "oidc" || me.Person == "" || len(me.Tenants) != 1 || me.Tenants[0].Tenant != testTenant ||
			me.Tenants[0].Role != "admin" {
			t.Fatalf("me %+v", me)
		}
		expect(t, e.serve(withCookie(request{method: "GET", path: "/api/v1/admin/tenants"}, sc)), http.StatusOK, "")
	}
	sess, err := a.st.UISessionOf(context.Background(), store.HashToken(sc.Value))
	if err != nil || strings.Join(sess.Groups, ",") != "admins" || sess.IDToken == "" {
		t.Fatalf("session %+v %v", sess, err)
	}
	acts, err := a.st.Q().Query(context.Background(), "SELECT detail->>'username' FROM activity WHERE kind = 'sign_in'")
	if err != nil {
		t.Fatal(err)
	}
	var who []string
	for acts.Next() {
		var s string
		_ = acts.Scan(&s)
		who = append(who, s)
	}
	if strings.Join(who, ",") != "ada" {
		t.Fatalf("sign_in rows %v", who)
	}

	// CSRF: a cookie's change needs the header, and a foreign Origin is
	// refused.
	post := func(h http.Header) *httptest.ResponseRecorder {
		return a.serve(withCookie(request{method: "POST", path: "/api/v1/admin/join-tokens", header: h}, sc))
	}
	expect(t, post(http.Header{}), http.StatusForbidden, errBody("missing X-Requested-With header"))
	expect(t, post(http.Header{"X-Requested-With": {"ferun"}, "Origin": {"https://evil.example"}}), http.StatusForbidden,
		errBody("cross-origin request refused"))
	// Past the checks (no fleet in this test: 503).
	expect(t, post(http.Header{"X-Requested-With": {"ferun"}, "Origin": {testPublicURL}}), http.StatusServiceUnavailable, "")
	expect(t, post(http.Header{"X-Requested-With": {"ferun"}}), http.StatusServiceUnavailable, "")
	// Basic wins over the cookie: wrong Basic with a good cookie is a 401;
	// Basic needs no CSRF header.
	w = a.serve(withCookie(request{method: "GET", path: "/api/v1/admin/tenants",
		header: http.Header{"Authorization": {basicOf(testAdmin, "wrong")}}}, sc))
	expect(t, w, http.StatusUnauthorized, errBody(`admin basic auth failed for user "admin"`))
	w = a.serve(request{method: "POST", path: "/api/v1/admin/join-tokens", header: http.Header{"Authorization": {adminAuth()}}})
	expect(t, w, http.StatusServiceUnavailable, "")
	w = a.serve(withCookie(request{method: "GET", path: "/auth/me", header: http.Header{"Authorization": {adminAuth()}}}, sc))
	expect(t, w, http.StatusOK, `{"username":"admin","admin":true,"method":"basic"}`+"\n")

	// The session under the cookie's old name (a session of the release
	// before) is honoured, for one release.
	old := &http.Cookie{Name: auth.OldSessionCookie, Value: sc.Value}
	if w := a.serve(withCookie(request{method: "GET", path: "/auth/me"}, old)); w.Code != http.StatusOK {
		t.Fatalf("old cookie: %d %s", w.Code, w.Body.String())
	}

	// Sign-out: CSRF first, then the session ends everywhere and the
	// browser goes to the provider's end-session endpoint.
	w = b.serve(withCookie(request{method: "POST", path: "/auth/logout"}, sc))
	expect(t, w, http.StatusForbidden, errBody("missing X-Requested-With header"))
	w = b.serve(withCookie(request{method: "POST", path: "/auth/logout", header: http.Header{"X-Requested-With": {"ferun"}}}, sc))
	expect(t, w, http.StatusOK, "")
	redirect := decode[struct{ Redirect string }](t, w).Redirect
	ru, err := url.Parse(redirect)
	if err != nil || !strings.HasPrefix(redirect, p.srv.URL+"/oauth2/sessions/logout?") ||
		ru.Query().Get("id_token_hint") != sess.IDToken || ru.Query().Get("post_logout_redirect_uri") != testPublicURL+"/" {
		t.Fatalf("redirect %s", redirect)
	}
	if c := cookieNamed(w, cookieSession); c == nil || c.MaxAge >= 0 {
		t.Fatalf("session cookie not cleared: %+v", c)
	}
	if c := cookieNamed(w, auth.OldSessionCookie); c == nil || c.MaxAge >= 0 {
		t.Fatalf("old session cookie not cleared: %+v", c)
	}
	expect(t, a.serve(withCookie(request{method: "GET", path: "/auth/me"}, sc)), http.StatusUnauthorized, errBody("not signed in"))
	w = a.serve(withCookie(request{method: "GET", path: "/api/v1/admin/tenants"}, sc))
	expect(t, w, http.StatusUnauthorized, errBody("admin sign-in required"))
}

// Basic credentials a browser cached from its own dialog do not carry a
// cross-site page's POST: a request the browser marks as cross-origin is
// refused, while a script's (no Origin, no Sec-Fetch-Site), the UI's and
// a same-origin one go through.
func TestBasicCrossOrigin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{publicURL: testPublicURL})
	ctx := context.Background()
	if _, err := e.st.InsertNode(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	drain := func(h http.Header) *httptest.ResponseRecorder {
		h.Set("Authorization", adminAuth())
		return e.serve(request{method: "POST", path: "/api/v1/admin/nodes/n1/drain", header: h})
	}
	for name, h := range map[string]http.Header{
		"foreign origin":           {"Origin": {"https://evil.example"}},
		"cross-site":               {"Sec-Fetch-Site": {"cross-site"}, "Origin": {"https://evil.example"}},
		"same-site":                {"Sec-Fetch-Site": {"same-site"}, "Origin": {"https://chat.example.test"}},
		"opaque origin":            {"Origin": {"null"}},
		"cross-site with a header": {"Sec-Fetch-Site": {"cross-site"}, "X-Requested-With": {"ferun"}},
	} {
		expect(t, drain(h), http.StatusForbidden, errBody("cross-origin request refused"))
		if n, err := e.st.GetNode(ctx, "n1"); err != nil || n.Draining {
			t.Fatalf("%s: node %+v %v", name, n, err)
		}
	}
	// A safe method is not checked.
	w := e.serve(request{method: "GET", path: "/api/v1/admin/nodes",
		header: http.Header{"Authorization": {adminAuth()}, "Sec-Fetch-Site": {"cross-site"}, "Origin": {"https://evil.example"}}})
	expect(t, w, http.StatusOK, "")
	for name, h := range map[string]http.Header{
		"script":          {},
		"UI":              {"Sec-Fetch-Site": {"same-origin"}, "Origin": {testPublicURL}, "X-Requested-With": {"ferun"}},
		"typed":           {"Sec-Fetch-Site": {"none"}},
		"old, host":       {"Origin": {"http://example.com"}},
		"old, public URL": {"Origin": {testPublicURL}},
	} {
		if w := drain(h); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	// Wrong credentials stay a 401, whatever the origin.
	w = e.serve(request{method: "POST", path: "/api/v1/admin/nodes/n1/undrain",
		header: http.Header{"Authorization": {basicOf(testAdmin, "wrong")}, "Origin": {"https://evil.example"}}})
	expect(t, w, http.StatusUnauthorized, "")
}

// ES256 and EdDSA keys sign in as RS256 ones do.
func TestSignInKeyTypes(t *testing.T) {
	t.Parallel()
	for _, alg := range []string{algES256, algEdDSA} {
		t.Run(alg, func(t *testing.T) {
			t.Parallel()
			p := newTestProvider(t, alg)
			a, b := oidcEnvs(t, p, false)
			sc := signIn(t, a, b, p, adminClaims())
			expect(t, a.serve(withCookie(request{method: "GET", path: "/api/v1/admin/nodes"}, sc)), http.StatusOK, "")
		})
	}
}

// Every way a callback is refused: a page with a short reason, never the
// token's values, and no session.
func TestSignInRefusals(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		prepare func(p *testProvider)
		claims  map[string]any
		status  int
		text    string
		// absent is a value of the token the page must not show.
		absent string
	}{
		{"wrong nonce", func(p *testProvider) {
			p.mutate = func(c, _ map[string]any) { c["nonce"] = "other" }
		}, adminClaims(), 400, "the token&#39;s nonce is not the sign-in&#39;s", ""},
		{"wrong audience", func(p *testProvider) {
			p.mutate = func(c, _ map[string]any) { c["aud"] = "someone-else" }
		}, adminClaims(), 400, "the token is not for this client (aud)", ""},
		{"wrong azp", func(p *testProvider) {
			p.mutate = func(c, _ map[string]any) { c["azp"] = "someone-else" }
		}, adminClaims(), 400, "the token was issued to another party (azp)", ""},
		{"wrong issuer", func(p *testProvider) {
			p.mutate = func(c, _ map[string]any) { c["iss"] = strings.TrimSuffix(p.issuer, "/") }
		}, adminClaims(), 400, "the token&#39;s issuer is not the configured one.", ""},
		{"foreign issuer", func(p *testProvider) {
			p.mutate = func(c, _ map[string]any) { c["iss"] = "https://issuer.evil.example/" }
		}, adminClaims(), 400, "the token&#39;s issuer is not the configured one.", "issuer.evil.example"},
		{"expired", func(p *testProvider) {
			p.mutate = func(c, _ map[string]any) { c["exp"] = time.Now().Add(-2 * time.Minute).Unix() }
		}, adminClaims(), 400, "the token has expired", ""},
		{"issued in the future", func(p *testProvider) {
			p.mutate = func(c, _ map[string]any) { c["iat"] = time.Now().Add(5 * time.Minute).Unix() }
		}, adminClaims(), 400, "the token is issued in the future", ""},
		{"bad signature", func(p *testProvider) {
			p.corrupt = func(tok string) string {
				i := strings.LastIndexByte(tok, '.')
				sig := []byte(tok[i+1:])
				if sig[3] == 'A' {
					sig[3] = 'B'
				} else {
					sig[3] = 'A'
				}
				return tok[:i+1] + string(sig)
			}
		}, adminClaims(), 400, "the token&#39;s signature does not verify", ""},
		{"alg none", func(p *testProvider) {
			p.mutate = func(_, h map[string]any) { h["alg"] = "none" }
			p.corrupt = func(tok string) string { return tok[:strings.LastIndexByte(tok, '.')+1] }
		}, adminClaims(), 400, "the token is not signed (alg none)", ""},
		{"another algorithm", func(p *testProvider) {
			p.mutate = func(_, h map[string]any) { h["alg"] = "HS256" }
		}, adminClaims(), 400, "the token&#39;s algorithm is not accepted (RS256, ES256 or EdDSA).", "HS256"},
		{"critical header", func(p *testProvider) {
			p.mutate = func(_, h map[string]any) { h["crit"] = []string{"x-secret-param"} }
		}, adminClaims(), 400, "the token names critical header parameters.", "x-secret-param"},
		{"unknown key", func(p *testProvider) {
			p.signer = newTestKey(p.t, "k9", algRS256)
		}, adminClaims(), 400, "the token&#39;s key is not in the provider&#39;s key set", ""},
		{"userinfo of another subject", func(p *testProvider) {
			p.userinfoSub = "someone-else"
		}, adminClaims(), 400, "userinfo names another subject than the ID token", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := newTestProvider(t, algRS256)
			a, b := oidcEnvs(t, p, false)
			if c.prepare != nil {
				p.mu.Lock()
				c.prepare(p)
				p.mu.Unlock()
			}
			loc, lc := login(t, a, "/")
			w := callback(t, b, loc, p.issue(loc, c.claims), lc)
			expect(t, w, c.status, "")
			if !strings.Contains(w.Body.String(), c.text) {
				t.Fatalf("page %s", w.Body.String())
			}
			if c.absent != "" && strings.Contains(w.Body.String(), c.absent) {
				t.Fatalf("the page shows the token's %q: %s", c.absent, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
				t.Fatalf("Content-Type %q", ct)
			}
			if cookieNamed(w, cookieSession) != nil {
				t.Fatal("a session cookie")
			}
			var n int
			if err := a.st.Q().QueryRow(context.Background(), "SELECT count(*) FROM ui_sessions").Scan(&n); err != nil || n != 0 {
				t.Fatalf("sessions %d %v", n, err)
			}
		})
	}
}

// me is the answer of /auth/me.
type me struct {
	Sub, Person, Email, Name, Username, Picture, Method string
	Admin                                               bool
	Tenants                                             []meTenant
}

// The browser's state: a missing or another login cookie is refused, an
// expired sign-in too; the provider's own error is shown.
func TestSignInState(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t, algRS256)
	a, b := oidcEnvs(t, p, false)
	loc, lc := login(t, a, "/x")
	code := p.issue(loc, adminClaims())
	for _, c := range []*http.Cookie{nil, {Name: cookieLogin, Value: "other"}} {
		w := callback(t, b, loc, code, c)
		expect(t, w, http.StatusBadRequest, "")
		if !strings.Contains(w.Body.String(), "This sign-in did not start in this browser") {
			t.Fatalf("page %s", w.Body.String())
		}
	}
	// The refusals took nothing: the sign-in still completes.
	expect(t, callback(t, b, loc, code, lc), http.StatusFound, "")

	loc, lc = login(t, a, "/x")
	if _, err := a.st.Q().Exec(context.Background(), "UPDATE auth_logins SET created_at = now() - interval '11 minutes'"); err != nil {
		t.Fatal(err)
	}
	w := callback(t, b, loc, p.issue(loc, adminClaims()), lc)
	expect(t, w, http.StatusBadRequest, "")
	if !strings.Contains(w.Body.String(), "This sign-in has expired. Start again.") || !strings.Contains(w.Body.String(), `href="/auth/login"`) {
		t.Fatalf("page %s", w.Body.String())
	}

	w = b.serve(request{method: "GET", path: "/auth/callback?error=access_denied&error_description=The+user+said+no"})
	expect(t, w, http.StatusBadRequest, "")
	if !strings.Contains(w.Body.String(), "The sign-in provider refused the sign-in: access_denied: The user said no") {
		t.Fatalf("page %s", w.Body.String())
	}
	// next must be a local path.
	for next, want := range map[string]string{
		"//evil.example/x": "/", `/\evil`: "/", "https://evil.example": "/", "/ok?a=1": "/ok?a=1",
		`/./\evil.example`: "/", `/a/../\evil.example`: "/", `/x\y`: "/", "/a/../b": "/b",
	} {
		loc, lc := login(t, a, next)
		w := callback(t, b, loc, p.issue(loc, adminClaims()), lc)
		expect(t, w, http.StatusFound, "")
		if w.Header().Get("Location") != want {
			t.Fatalf("next %q: redirect %q", next, w.Header().Get("Location"))
		}
	}
}

// A rotated key set is fetched again for a token of an unknown key id, at
// most once a minute.
func TestSignInKeyRotation(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t, algRS256)
	a, b := oidcEnvs(t, p, false)
	signIn(t, a, b, p, adminClaims())
	p.mu.Lock()
	fetches := p.jwksFetches
	p.signer = newTestKey(t, "k2", algES256)
	p.published = []testKey{p.signer}
	p.mu.Unlock()
	// Within the minute of b's first fetch, the unknown kid fetches
	// nothing and is refused.
	loc, lc := login(t, a, "/")
	expect(t, callback(t, b, loc, p.issue(loc, adminClaims()), lc), http.StatusBadRequest, "")
	p.mu.Lock()
	if p.jwksFetches != fetches {
		t.Fatalf("fetches %d, want %d", p.jwksFetches, fetches)
	}
	p.mu.Unlock()
	// A minute on, b holds the old set: the unknown kid makes it fetch
	// the new one.
	b.srv.oidc.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	signIn(t, a, b, p, adminClaims())
	p.mu.Lock()
	if p.jwksFetches != fetches+1 {
		t.Fatalf("fetches %d, want %d", p.jwksFetches, fetches+1)
	}
	p.signer = newTestKey(t, "k3", algRS256)
	p.mu.Unlock()
	// Another unknown kid within the minute: no fetch, refused.
	loc, lc = login(t, a, "/")
	w := callback(t, b, loc, p.issue(loc, adminClaims()), lc)
	expect(t, w, http.StatusBadRequest, "")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.jwksFetches != fetches+1 {
		t.Fatalf("fetches %d, want %d", p.jwksFetches, fetches+1)
	}
}

// The provider's failures are a 502: a discovery that names another
// issuer, a token endpoint that refuses the client.
func TestSignInProviderFailures(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t, algRS256)
	p.discoveryIssuer = "https://other.example/"
	a := newEnv(t, envOptions{oidc: p.config(), publicURL: testPublicURL})
	w := a.serve(request{method: "GET", path: "/auth/login"})
	expect(t, w, http.StatusBadGateway, "")
	if !strings.Contains(w.Body.String(), "The sign-in provider cannot be reached") {
		t.Fatalf("page %s", w.Body.String())
	}

	p2 := newTestProvider(t, algRS256)
	cfg := p2.config()
	cfg.ClientSecret = "wrong"
	b := newEnv(t, envOptions{oidc: cfg, publicURL: testPublicURL})
	loc, lc := login(t, b, "/")
	w = callback(t, b, loc, p2.issue(loc, adminClaims()), lc)
	expect(t, w, http.StatusBadGateway, "")
	if !strings.Contains(w.Body.String(), "The sign-in provider could not be reached or gave an answer FeRun cannot use.") {
		t.Fatalf("page %s", w.Body.String())
	}
}

// Without an end-session endpoint the sign-out sends the browser home; a
// sign-out without a session clears the cookie all the same.
func TestSignOutWithoutEndSession(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t, algRS256)
	p.endSession = false
	a, b := oidcEnvs(t, p, false)
	sc := signIn(t, a, b, p, adminClaims())
	w := a.serve(withCookie(request{method: "POST", path: "/auth/logout", header: http.Header{"X-Requested-With": {"ferun"}}}, sc))
	expect(t, w, http.StatusOK, `{"redirect":"/"}`+"\n")
	w = a.serve(request{method: "POST", path: "/auth/logout"})
	expect(t, w, http.StatusOK, `{"redirect":"/"}`+"\n")
}

// Without OIDC the sign-out exists too (the UI signs out of Basic with
// it): it clears the cookie and sends the browser home; the OIDC flow's
// routes are unknown.
func TestSignOutWithoutOIDC(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	for _, rq := range []request{
		{method: "POST", path: "/auth/logout"},
		withCookie(request{method: "POST", path: "/auth/logout"}, &http.Cookie{Name: cookieSession, Value: "stale"}),
		withCookie(request{method: "POST", path: "/auth/logout", header: http.Header{"X-Requested-With": {"ferun"},
			"Origin": {"http://localhost:8100"}}}, &http.Cookie{Name: cookieSession, Value: "stale"}),
	} {
		w := e.serve(rq)
		expect(t, w, http.StatusOK, `{"redirect":"/"}`+"\n")
		if c := cookieNamed(w, cookieSession); c == nil || c.MaxAge >= 0 {
			t.Fatalf("session cookie not cleared: %+v", c)
		}
	}
	for _, path := range []string{"/auth/login", "/auth/callback"} {
		expect(t, e.serve(request{method: "GET", path: path}), http.StatusNotFound, errBody("no such route: GET "+path))
	}
}

// /auth/config names the ways in; the Basic prompt appears only where
// Basic is a way in.
func TestSignInConfig(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t, algRS256)
	only := newEnv(t, envOptions{oidc: p.config(), publicURL: testPublicURL, noAdminUsers: true})
	expect(t, only.do("GET", "/auth/config", "", nil), http.StatusOK, `{"oidc":true,"basic":false}`+"\n")
	w := only.do("GET", "/api/v1/admin/nodes", "", nil)
	expect(t, w, http.StatusUnauthorized, errBody("admin sign-in required"))
	if h := w.Header().Get("WWW-Authenticate"); h != "" {
		t.Fatalf("WWW-Authenticate %q with OIDC only", h)
	}
	// Basic credentials are no way in when Basic is off.
	expect(t, only.do("GET", "/api/v1/admin/nodes", adminAuth(), nil), http.StatusUnauthorized, errBody("admin sign-in required"))
	expect(t, only.do("GET", "/auth/me", adminAuth(), nil), http.StatusUnauthorized, errBody("not signed in"))

	both := newEnv(t, envOptions{oidc: p.config(), publicURL: testPublicURL})
	expect(t, both.do("GET", "/auth/config", "", nil), http.StatusOK, `{"oidc":true,"basic":true}`+"\n")
	w = both.do("GET", "/api/v1/admin/nodes", "", nil)
	if h := w.Header().Get("WWW-Authenticate"); h != `Basic realm="ferun"` {
		t.Fatalf("WWW-Authenticate %q", h)
	}

	basic := newEnv(t, envOptions{})
	expect(t, basic.do("GET", "/auth/config", "", nil), http.StatusOK, `{"oidc":false,"basic":true}`+"\n")
	expect(t, basic.do("GET", "/auth/me", adminAuth(), nil), http.StatusOK, `{"username":"admin","admin":true,"method":"basic"}`+"\n")
	expect(t, basic.do("GET", "/auth/me", "", nil), http.StatusUnauthorized, errBody("not signed in"))
}

// The UI's requests (X-Requested-With: ferun) never get the Basic
// challenge, so that a session that expires brings up the UI's sign-in
// screen and not the browser's login dialog; a script's request without
// the header still gets it while Basic is on.
func TestBasicChallengeNotForUI(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t, algRS256)
	for _, o := range []envOptions{{}, {oidc: p.config(), publicURL: testPublicURL}} {
		e := newEnv(t, o)
		expired := &http.Cookie{Name: cookieSession, Value: "expired-or-unknown"}
		for _, c := range []struct {
			method, authz string
			ui            bool
			cookie        *http.Cookie
			challenge     bool
			msg           string
		}{
			{"GET", "", false, nil, true, "admin sign-in required"},
			{"GET", "", true, nil, false, "admin sign-in required"},
			{"POST", "", true, nil, false, "admin sign-in required"},
			{"GET", "", true, expired, false, "admin sign-in required"},
			{"GET", "", false, expired, true, "admin sign-in required"},
			{"GET", basicOf(testAdmin, "wrong"), false, nil, true, `admin basic auth failed for user "admin"`},
			{"GET", basicOf(testAdmin, "wrong"), true, nil, false, `admin basic auth failed for user "admin"`},
		} {
			h := http.Header{}
			if c.authz != "" {
				h.Set("Authorization", c.authz)
			}
			if c.ui {
				h.Set(csrfHeader, csrfValue)
			}
			rq := request{method: c.method, path: "/api/v1/admin/join-tokens", header: h}
			if c.cookie != nil {
				rq.cookies = []*http.Cookie{c.cookie}
			}
			w := e.serve(rq)
			expect(t, w, http.StatusUnauthorized, errBody(c.msg))
			want := ""
			if c.challenge {
				want = `Basic realm="ferun"`
			}
			if got := w.Header().Get("WWW-Authenticate"); got != want {
				t.Fatalf("oidc %v, %s ui %v cookie %v authz %q: WWW-Authenticate %q, want %q",
					o.oidc != nil, c.method, c.ui, c.cookie != nil, c.authz, got, want)
			}
		}
		// The UI's request with good credentials passes as before.
		w := e.serve(request{method: "GET", path: "/api/v1/admin/join-tokens",
			header: http.Header{"Authorization": {adminAuth()}, csrfHeader: {csrfValue}}})
		expect(t, w, http.StatusOK, "")
	}
	// Without Basic, no request gets the challenge, the UI's or not.
	only := newEnv(t, envOptions{oidc: p.config(), publicURL: testPublicURL, noAdminUsers: true})
	for _, ui := range []bool{false, true} {
		h := http.Header{}
		if ui {
			h.Set(csrfHeader, csrfValue)
		}
		w := only.serve(request{method: "GET", path: "/api/v1/admin/nodes", header: h})
		expect(t, w, http.StatusUnauthorized, errBody("admin sign-in required"))
		if got := w.Header().Get("WWW-Authenticate"); got != "" {
			t.Fatalf("ui %v: WWW-Authenticate %q with OIDC only", ui, got)
		}
	}
}

func TestLocalPath(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"": "/", "/": "/", "/nodes": "/nodes", "//x": "/", `/\x`: "/", "x": "/", "http://x/": "/",
		"/a\nb": "/", "/tenants/acme?tab=1#x": "/tenants/acme?tab=1#x",
	} {
		if got := localPath(in); got != want {
			t.Errorf("localPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The leader's clean-up deletes stale sign-ins and expired sessions.
func TestAuthCleanupLoop(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	if err := e.st.InsertAuthLogin(ctx, store.AuthLogin{StateHash: "old", Verifier: "v", Nonce: "n", Next: "/"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertAuthLogin(ctx, store.AuthLogin{StateHash: "new", Verifier: "v", Nonce: "n", Next: "/"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Q().Exec(ctx, "UPDATE auth_logins SET created_at = now() - interval '11 minutes' WHERE state_hash = 'old'"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		id  string
		ttl time.Duration
	}{{"gone", time.Millisecond}, {"kept", time.Hour}} {
		if _, err := e.st.InsertUISession(ctx, store.UISession{IDHash: s.id, Admin: true}, s.ttl); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(10 * time.Millisecond)
	loops := Loops(Deps{DB: e.d})
	if len(loops) != 1 || loops[0].Interval != 10*time.Minute {
		t.Fatalf("loops %+v", loops)
	}
	if err := loops[0].Run(ctx); err != nil {
		t.Fatal(err)
	}
	var logins, sessions string
	if err := e.st.Q().QueryRow(ctx, "SELECT string_agg(state_hash, ',') FROM auth_logins").Scan(&logins); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Q().QueryRow(ctx, "SELECT string_agg(id_hash, ',') FROM ui_sessions").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if logins != "new" || sessions != "kept" {
		t.Fatalf("logins %s sessions %s", logins, sessions)
	}
}

// A sign-in begun with the address typed on the sign-in screen passes it on
// as login_hint, normalised; anything that is not an address is dropped.
func TestLoginHint(t *testing.T) {
	t.Parallel()
	p := newTestProvider(t, algRS256)
	e, _ := oidcEnvs(t, p, false)
	for _, c := range []struct{ in, want string }{
		{" Ann@Example.COM ", "ann@example.com"},
		{"ann@example.com", "ann@example.com"},
		{"", ""},
		{"ann", ""},
		{"@example.com", ""},
		{"ann@", ""},
		{"a@b@c", ""},
		{"ann @example.com", ""},
		{"ann@example.com\n", "ann@example.com"},
	} {
		w := e.serve(request{method: "GET", path: "/auth/login?next=%2F&login_hint=" + url.QueryEscape(c.in)})
		expect(t, w, http.StatusFound, "")
		u, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		got, ok := u.Query()["login_hint"]
		switch {
		case c.want == "" && ok:
			t.Errorf("%q: login_hint %q, want none", c.in, got)
		case c.want != "" && (!ok || got[0] != c.want):
			t.Errorf("%q: login_hint %q, want %q", c.in, got, c.want)
		}
	}
}
