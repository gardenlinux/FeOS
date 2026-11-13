// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/visit"
)

// A whole sign-in of kind oidc: /visit sends the browser to the issuer
// with state, nonce and PKCE in a flow cookie of its own, the callback
// accepts at ferund with what the issuer said and sends the browser to
// ferund's redirect. Nothing secret reaches the log.
func TestSignInOIDC(t *testing.T) {
	h := newHarness(t)
	sealed := "sealed-challenge-never-logged"
	h.ferund.view(sealed, kindOIDC, h.oidcOf(rule{Domains: []string{"Example.TEST"}}))

	rec := h.visit(sealed)
	loc, state := h.toIssuer(rec)
	u, _ := url.Parse(loc)
	q := u.Query()
	for k, want := range map[string]string{"response_type": "code", "client_id": h.issuer.ClientID,
		"redirect_uri": testPublic + "/callback/acme/sso", "scope": "openid profile email", "code_challenge_method": "S256"} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	nonce := q.Get("nonce")
	if len(state) != 43 || len(nonce) != 43 || len(q.Get("code_challenge")) != 43 || state == nonce {
		t.Fatalf("state %q nonce %q challenge %q", state, nonce, q.Get("code_challenge"))
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers %v", rec.Header())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies %v", cookies)
	}
	c := cookies[0]
	if c.Name != flowCookiePrefix+flowKey(state) || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode ||
		c.Path != "/" || c.MaxAge != 600 || c.Domain != "" {
		t.Fatalf("flow cookie %+v", c)
	}
	for _, secret := range []string{sealed, nonce, state} {
		if strings.Contains(c.Value, secret) {
			t.Fatalf("the flow cookie shows %q", secret)
		}
	}

	code := h.issuer.Issue(loc, claims())
	rec = h.do("GET", cbPath(t, loc)+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state)+"&iss="+url.QueryEscape(h.issuer.Issuer), navigation, "")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://"+testHost+"/.ferun/auth?code=accepted-code" {
		t.Fatalf("callback: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if _, held := h.jar[c.Name]; held {
		t.Fatal("the flow cookie is not cleared")
	}
	h.ferund.mu.Lock()
	accepts, auth := h.ferund.accepts, h.ferund.auth
	h.ferund.mu.Unlock()
	want := acceptBody{Challenge: sealed, Subject: claims()["sub"].(string), Email: "bob@example.test", Name: "Ada",
		Session: testSession}
	if len(accepts) != 1 || accepts[0] != want {
		t.Fatalf("accepts %+v, want %+v", accepts, want)
	}
	for _, a := range auth {
		if a != "Bearer "+testToken {
			t.Fatalf("Authorization %q", a)
		}
	}
	// The flow is used once.
	rec = h.do("GET", cbPath(t, loc)+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), navigation, "")
	wantPage(t, rec, http.StatusBadRequest, msgCannotFinish)

	logs := h.logs.String()
	for _, secret := range []string{sealed, state, nonce, code, testToken, testSession, h.issuer.ClientSecret, c.Value} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the log shows %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs, "challenge answered") {
		t.Fatalf("no log of the answer:\n%s", logs)
	}
}

// Kind ferun signs in at the installation's issuer and accepts whoever
// signed in: ferund decides by the person's grant.
func TestSignInFerun(t *testing.T) {
	h := newHarness(t, withFerunIssuer)
	h.ferund.view("s", kindFerun, nil)
	loc, state := h.toIssuer(h.visit("s"))
	rec := h.callback(loc, state, claims())
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback %d %s", rec.Code, rec.Body)
	}
	if len(h.ferund.accepts) != 1 || h.ferund.accepts[0].Session != testSession || h.ferund.accepts[0].Email != "bob@example.test" {
		t.Fatalf("accepts %+v", h.ferund.accepts)
	}
}

// Without --issuer, kind ferun is not served.
func TestFerunWithoutIssuer(t *testing.T) {
	h := newHarness(t)
	h.ferund.view("s", kindFerun, nil)
	wantPage(t, h.visit("s"), http.StatusServiceUnavailable, msgNoFerun)
}

// Only the browser's own top-level navigation starts a sign-in, and
// nothing is asked of ferund otherwise.
func TestVisitNavigationOnly(t *testing.T) {
	h := newHarness(t)
	h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
	for name, hdr := range map[string]map[string]string{
		"no headers":      {},
		"no dest":         {"Sec-Fetch-Mode": "navigate"},
		"a frame":         {"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"},
		"a fetch":         {"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"},
		"a prefetch":      {"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Purpose": "prefetch;prerender"},
		"an old prefetch": {"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Purpose": "prefetch"},
	} {
		rec := h.do("GET", "/visit?challenge=s", hdr, "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), msgOpenInBrowser) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	for _, target := range []string{"/visit", "/visit?challenge=" + strings.Repeat("a", visit.MaxSealed+1)} {
		wantPage(t, h.do("GET", target, navigation, ""), http.StatusBadRequest, msgNotValid)
	}
	if n := h.ferund.callCount(); n != 0 {
		t.Fatalf("%d calls to ferund", n)
	}
}

// A provider's rule that does not admit the visitor rejects the
// challenge, and the browser goes to the host with the rejected code.
func TestRuleRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		rule   rule
		claims func(map[string]any)
	}{
		"another domain":   {rule{Domains: []string{"example.com"}}, func(c map[string]any) { c["email"] = "a@evil-example.com" }},
		"a quoted local":   {rule{Domains: []string{"example.com"}}, func(c map[string]any) { c["email"] = `"x@example.com"@evil.io` }},
		"not verified":     {rule{Domains: []string{"example.test"}}, func(c map[string]any) { c["email_verified"] = false }},
		"verified missing": {rule{Emails: []string{"bob@example.test"}}, func(c map[string]any) { delete(c, "email_verified") }},
		"Kelvin sign":      {rule{Emails: []string{"kim@example.test"}}, func(c map[string]any) { c["email"] = "\u212aim@example.test" }},
		"another group":    {rule{Groups: []string{"staff"}}, func(c map[string]any) { c["groups"] = []string{"Staff", "guests"} }},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.ferund.view("s", kindOIDC, h.oidcOf(tc.rule))
			loc, state := h.toIssuer(h.visit("s"))
			c := claims()
			tc.claims(c)
			rec := h.callback(loc, state, c)
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://"+testHost+"/.ferun/auth?code=rejected-code" {
				t.Fatalf("callback %d %v", rec.Code, rec.Header())
			}
			if len(h.ferund.rejects) != 1 || len(h.ferund.accepts) != 0 {
				t.Fatalf("rejects %v accepts %v", h.ferund.rejects, h.ferund.accepts)
			}
		})
	}
}

// An email the issuer did not verify is not passed on, and a name is cut
// to what ferund takes.
func TestAcceptUnverifiedEmail(t *testing.T) {
	h := newHarness(t)
	h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
	loc, state := h.toIssuer(h.visit("s"))
	c := claims()
	c["email_verified"] = false
	c["name"] = strings.Repeat("é", 150) + "\x07"
	if rec := h.callback(loc, state, c); rec.Code != http.StatusSeeOther {
		t.Fatalf("callback %d %s", rec.Code, rec.Body)
	}
	a := h.ferund.accepts[0]
	if a.Email != "" || len(a.Name) != 200 || strings.ContainsRune(a.Name, 7) {
		t.Fatalf("accept %+v", a)
	}
}

// RFC 9207: a callback naming another issuer is refused.
func TestCallbackOtherIssuer(t *testing.T) {
	h := newHarness(t)
	h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
	loc, state := h.toIssuer(h.visit("s"))
	code := h.issuer.Issue(loc, claims())
	rec := h.do("GET", cbPath(t, loc)+"?code="+code+"&state="+state+"&iss="+url.QueryEscape("https://evil.example/"), navigation, "")
	wantPage(t, rec, http.StatusBadRequest, msgCannotFinish)
	if len(h.ferund.accepts) != 0 {
		t.Fatal("accepted")
	}
}

// An issuer's error ends the sign-in.
func TestCallbackIssuerError(t *testing.T) {
	h := newHarness(t)
	h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
	loc, state := h.toIssuer(h.visit("s"))
	rec := h.do("GET", cbPath(t, loc)+"?error=access_denied&state="+state, navigation, "")
	wantPage(t, rec, http.StatusBadRequest, msgIssuerRefused)
	if len(h.jar) != 0 {
		t.Fatalf("cookies left %v", h.jar)
	}
}

// A flow cookie that was tampered with, one past its end, and none (the
// sign-in finished in another browser) finish nothing.
func TestFlowCookieRefusals(t *testing.T) {
	for name, spoil := range map[string]func(h *harness, name string){
		"tampered": func(h *harness, name string) {
			v := []byte(h.jar[name])
			if i := len(v) / 2; v[i] == 'A' {
				v[i] = 'B'
			} else {
				v[i] = 'A'
			}
			h.jar[name] = string(v)
		},
		"another key's": func(h *harness, name string) {
			// The same flow sealed for another state's cookie.
			f, _ := h.srv.openFlow(strings.TrimPrefix(name, flowCookiePrefix), h.jar[name])
			v, _ := h.srv.sealFlow("AAAAAAAAAAAAAAAA", f)
			h.jar[name] = v
		},
		"expired":         func(h *harness, _ string) { h.clock.add(flowLifetime + time.Second) },
		"another browser": func(h *harness, name string) { delete(h.jar, name) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
			loc, state := h.toIssuer(h.visit("s"))
			spoil(h, flowCookiePrefix+flowKey(state))
			rec := h.callback(loc, state, claims())
			wantPage(t, rec, http.StatusBadRequest, msgCannotFinish)
			if len(h.ferund.accepts) != 0 {
				t.Fatal("accepted")
			}
			if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
				t.Fatalf("the flow cookie is not cleared: %v", rec.Header())
			}
		})
	}
}

// Two sign-ins in one browser at once (two tabs) both finish.
func TestTwoFlows(t *testing.T) {
	h := newHarness(t)
	h.ferund.view("s1", kindOIDC, h.oidcOf(rule{Anyone: true}))
	h.ferund.view("s2", kindOIDC, h.oidcOf(rule{Anyone: true}))
	loc1, state1 := h.toIssuer(h.visit("s1"))
	loc2, state2 := h.toIssuer(h.visit("s2"))
	if len(h.jar) != 2 {
		t.Fatalf("cookies %v", h.jar)
	}
	for _, f := range []struct{ loc, state string }{{loc2, state2}, {loc1, state1}} {
		if rec := h.callback(f.loc, f.state, claims()); rec.Code != http.StatusSeeOther {
			t.Fatalf("callback %d %s", rec.Code, rec.Body)
		}
	}
	if len(h.ferund.accepts) != 2 || h.ferund.accepts[0].Challenge != "s2" || h.ferund.accepts[1].Challenge != "s1" {
		t.Fatalf("accepts %+v", h.ferund.accepts)
	}
}

// At most eight flows: the ninth drops the oldest.
func TestFlowCap(t *testing.T) {
	h := newHarness(t)
	h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
	var first string
	for i := range maxFlows {
		_, state := h.toIssuer(h.visit("s"))
		if i == 0 {
			first = flowCookiePrefix + flowKey(state)
		}
		h.clock.add(time.Second)
	}
	if len(h.jar) != maxFlows {
		t.Fatalf("%d flows", len(h.jar))
	}
	rec := h.visit("s")
	h.toIssuer(rec)
	if _, held := h.jar[first]; held || len(h.jar) != maxFlows {
		t.Fatalf("flows %d, the oldest held: %v", len(h.jar), held)
	}
	cleared := 0
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared++
			if c.Name != first {
				t.Fatalf("cleared %s, not the oldest", c.Name)
			}
		}
	}
	if cleared != 1 {
		t.Fatalf("cleared %d", cleared)
	}
}

// A challenge with a return path of MaxPath bytes, sealed as the ingress
// seals it, keeps the flow cookie under 3800 bytes.
func TestLongPathCookie(t *testing.T) {
	keys, err := visit.NewKeys(make([]byte, 32), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := visit.NewChallengeID()
	secret, _ := visit.NewBindingSecret()
	path := "/" + strings.Repeat("%E2%82%AC", (visit.MaxPath-1)/9)
	path += strings.Repeat("a", visit.MaxPath-len(path))
	now := time.Now().Unix()
	sealed, err := keys.SealChallenge(visit.Challenge{ID: id, Env: "6f0c1d2e-0000-4000-8000-000000000001",
		Host: "a-rather-long-environment-name--a-long-tenant-name.apps.example.test", Path: path,
		Binding: visit.BindingHash(secret), IssuedAt: now, Expires: now + 600})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.ferund.view(sealed, kindOIDC, h.oidcOf(rule{Anyone: true}))
	rec := h.visit(sealed)
	h.toIssuer(rec)
	sc := rec.Header().Get("Set-Cookie")
	if len(sc) > maxFlowCookie {
		t.Fatalf("Set-Cookie of %d bytes", len(sc))
	}
	t.Logf("sealed challenge %d, Set-Cookie %d bytes", len(sealed), len(sc))
}

// What ferund answers: 404 is a link that is not valid, a refused token
// is logged at error, ferund unwell or away is 502.
func TestFerundAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		down   bool
		code   int
		msg    string
		log    string
	}{
		"404":         {status: 404, code: 400, msg: msgNotValid},
		"401":         {status: 401, code: 502, msg: msgUnavailable, log: "ferund refused ferun-auth's token"},
		"403":         {status: 403, code: 502, msg: msgUnavailable, log: "ferund refused ferun-auth's token"},
		"503":         {status: 503, code: 502, msg: msgUnavailable},
		"unreachable": {down: true, code: 502, msg: msgUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.ferund.set(func(f *fakeFerund) { f.status = tc.status })
			if tc.down {
				h.ferund.srv.Close()
			}
			wantPage(t, h.visit("s"), tc.code, tc.msg)
			logs := h.logs.String()
			if tc.log != "" && !strings.Contains(logs, `"level":"ERROR","msg":"`+tc.log+`"`) {
				t.Fatalf("no error log %q:\n%s", tc.log, logs)
			}
			if tc.log == "" && strings.Contains(logs, "refused ferun-auth's token") {
				t.Fatalf("logged a refused token:\n%s", logs)
			}
		})
	}
}

// A challenge that expired or was answered sends the browser to begin
// again, at /visit and at the callback.
func TestRestart(t *testing.T) {
	for _, field := range []string{"expired", "answered"} {
		t.Run(field, func(t *testing.T) {
			h := newHarness(t)
			v := h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
			loc, state := h.toIssuer(h.visit("s"))
			h.ferund.set(func(*fakeFerund) {
				v.Expired, v.Answered = field == "expired", field == "answered"
			})
			rec := h.visit("s")
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != v.Restart {
				t.Fatalf("/visit %d %v", rec.Code, rec.Header())
			}
			rec = h.callback(loc, state, claims())
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != v.Restart {
				t.Fatalf("callback %d %v", rec.Code, rec.Header())
			}
			if len(h.ferund.accepts) != 0 {
				t.Fatal("accepted")
			}
		})
	}
}

// A 409 at the accept (another tab answered first, or it expired
// meanwhile) opens the challenge again and begins again.
func TestAcceptConflict(t *testing.T) {
	for _, code := range []string{"challenge_answered", "challenge_expired"} {
		h := newHarness(t)
		v := h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
		loc, state := h.toIssuer(h.visit("s"))
		h.ferund.set(func(f *fakeFerund) { f.acceptStatus, f.acceptCode = 409, code })
		rec := h.callback(loc, state, claims())
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != v.Restart {
			t.Fatalf("%s: %d %v %s", code, rec.Code, rec.Header(), rec.Body)
		}
	}
}

// ferund's redirect and restart must be on the challenge's host.
func TestForeignRedirect(t *testing.T) {
	h := newHarness(t)
	v := h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
	h.ferund.set(func(*fakeFerund) { v.Restart = "https://evil.example/" })
	wantPage(t, h.visit("s"), http.StatusBadGateway, msgUnavailable)
}

// The ruled client: an issuer at a denied address (loopback without
// --allow-prefix), and a discovery naming an http token endpoint, are
// not used.
func TestRuledClient(t *testing.T) {
	t.Run("denied address", func(t *testing.T) {
		h := newHarness(t, opt(noAllow))
		h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
		wantPage(t, h.visit("s"), http.StatusBadGateway, msgUnavailable)
		if !strings.Contains(h.logs.String(), "destination denied") {
			t.Fatalf("logs:\n%s", h.logs)
		}
	})
	t.Run("http endpoint", func(t *testing.T) {
		var iss string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": iss, "authorization_endpoint": iss + "auth",
				"token_endpoint": "http://" + r.Host + "/token", "jwks_uri": iss + "jwks"})
		}))
		defer srv.Close()
		iss = srv.URL + "/"
		h := newHarness(t, func(*harness) harnessOpt {
			return func(c *Config) {
				c.roots = x509.NewCertPool()
				c.roots.AddCert(srv.Certificate())
			}
		})
		h.ferund.view("s", kindOIDC, &oidcProvider{Issuer: iss, ClientID: "c", ClientSecret: "x", Rule: rule{Anyone: true}})
		wantPage(t, h.visit("s"), http.StatusBadGateway, msgUnavailable)
	})
}

// Anything but ferun-auth's routes is 404 text/plain.
func TestNotFound(t *testing.T) {
	h := newHarness(t)
	for _, r := range [][2]string{{"GET", "/"}, {"POST", "/visit"}, {"POST", "/callback/ferun"}, {"GET", "/callback"},
		{"GET", "/callback/"}, {"GET", "/callback/acme"}, {"GET", "/callback/Acme/sso"}, {"GET", "/callback/acme/sso/x"},
		{"GET", "/backchannel/ferun"},
		{"GET", "/visit/x"}, {"HEAD", "/visit"}, {"GET", "/api/v1/challenges/open"}} {
		rec := h.do(r[0], r[1], navigation, "")
		if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("%s %s: %d %v", r[0], r[1], rec.Code, rec.Header())
		}
	}
	if h.ferund.callCount() != 0 {
		t.Fatal("ferund called")
	}
}

// The flow's key is the cookie's k: base64url(sha256(state))[:16].
func TestFlowKey(t *testing.T) {
	sum := sha256.Sum256([]byte("state"))
	if got := flowKey("state"); got != base64.RawURLEncoding.EncodeToString(sum[:])[:16] || len(got) != 16 {
		t.Fatalf("flowKey %q", got)
	}
}
