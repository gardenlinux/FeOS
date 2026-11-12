// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func logoutForm(token string) string { return url.Values{"logout_token": {token}}.Encode() }

// The installation's issuer's logout ends the visits of kind ferun of its
// session; a token that does not verify ends nothing; ferund failing is
// 503, which the issuer may try again.
func TestBackchannelFerun(t *testing.T) {
	h := newHarness(t, withFerunIssuer)
	rec := h.do("POST", "/backchannel/ferun", nil, logoutForm(h.issuer.LogoutToken(map[string]any{"sid": testSession})))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if len(h.ferund.ends) != 1 || h.ferund.ends[0] != (endBody{Provider: "ferun", Session: testSession}) {
		t.Fatalf("ends %+v", h.ferund.ends)
	}
	for name, token := range map[string]string{
		"no token":    "",
		"not a JWT":   "x.y.z",
		"no sid":      h.issuer.LogoutToken(map[string]any{"sid": nil}),
		"another aud": h.issuer.LogoutToken(map[string]any{"aud": "someone-else"}),
		"a nonce":     h.issuer.LogoutToken(map[string]any{"nonce": "n"}),
	} {
		rec := h.do("POST", "/backchannel/ferun", nil, logoutForm(token))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_request"`) ||
			rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(h.ferund.ends) != 1 {
		t.Fatalf("ends %+v", h.ferund.ends)
	}
	h.ferund.set(func(f *fakeFerund) { f.endStatus = 500 })
	rec = h.do("POST", "/backchannel/ferun", nil, logoutForm(h.issuer.LogoutToken(nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ferund failing: %d", rec.Code)
	}
	if strings.Contains(h.logs.String(), testSession) {
		t.Fatalf("the log shows the session:\n%s", h.logs)
	}
}

// Without --issuer there is no back channel of kind ferun.
func TestBackchannelFerunWithoutIssuer(t *testing.T) {
	h := newHarness(t)
	rec := h.do("POST", "/backchannel/ferun", nil, logoutForm(h.issuer.LogoutToken(nil)))
	if rec.Code != http.StatusNotFound || h.ferund.callCount() != 0 {
		t.Fatalf("%d, %d calls", rec.Code, h.ferund.callCount())
	}
}

// A tenant's issuer's logout: the provider read from ferund, the token
// verified against its issuer and client, its visits of the session ended.
func TestBackchannelTenant(t *testing.T) {
	h := newHarness(t)
	h.ferund.set(func(f *fakeFerund) {
		f.providers["acme/sso"] = oidcProvider{Tenant: "acme", Name: "sso", Issuer: h.issuer.Issuer, ClientID: h.issuer.ClientID,
			ClientSecret: h.issuer.ClientSecret, Rule: rule{Anyone: true}}
	})
	rec := h.do("POST", "/backchannel/acme/sso", nil, logoutForm(h.issuer.LogoutToken(map[string]any{"sid": testSession})))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(h.ferund.ends) != 1 || h.ferund.ends[0] != (endBody{Tenant: "acme", Provider: "sso", Session: testSession}) {
		t.Fatalf("ends %+v", h.ferund.ends)
	}
	// Unknown at ferund: 404.
	rec = h.do("POST", "/backchannel/acme/other", nil, logoutForm(h.issuer.LogoutToken(nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown provider: %d", rec.Code)
	}
	// A bad token: 400.
	rec = h.do("POST", "/backchannel/acme/sso", nil, logoutForm("x.y.z"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad token: %d", rec.Code)
	}
	// ferund failing: 503.
	h.ferund.set(func(f *fakeFerund) { f.status = 503 })
	rec = h.do("POST", "/backchannel/acme/sso", nil, logoutForm(h.issuer.LogoutToken(nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ferund failing: %d", rec.Code)
	}
}

// Path values that are no names are 404 without a call to ferund.
func TestBackchannelPathValues(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/backchannel/acme/..%2Fx", "/backchannel/..%2Fx/sso", "/backchannel/acme/sso/x",
		"/backchannel/Acme/sso", "/backchannel/acme/", "/backchannel/acme", "/backchannel/acme/s%73o", "/backchannel/-a/sso"} {
		rec := h.do("POST", p, nil, logoutForm("x"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if n := h.ferund.callCount(); n != 0 {
		t.Fatalf("%d calls to ferund", n)
	}
}
