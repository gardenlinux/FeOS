// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/oidc/oidctest"
)

// Every provider has a redirect URI of its own: /callback/ferun for kind
// ferun, /callback/<tenant>/<provider> for kind oidc.
func TestRedirectURIPerProvider(t *testing.T) {
	h := newHarness(t, withFerunIssuer)
	h.ferund.view("f", kindFerun, nil)
	h.ferund.view("o", kindOIDC, h.oidcOf(rule{Anyone: true}))
	for sealed, want := range map[string]string{"f": "/callback/ferun", "o": "/callback/acme/sso"} {
		loc, _ := h.toIssuer(h.visit(sealed))
		if got := cbPath(t, loc); got != want {
			t.Errorf("%s: redirect URI path %s, want %s", sealed, got, want)
		}
	}
}

// The mix-up attack: a tenant's issuer A sends the victim on to issuer B
// with the state of A's flow, so that B's code comes back to A's flow. B
// answers only at its own redirect URI, which is not the flow's: the
// callback refuses before anything is exchanged, and the flow is gone.
// The code at A's own redirect URI with A's state is exchanged at A, and
// so never reaches ferund for B.
func TestCallbackMixUp(t *testing.T) {
	b := oidctest.New(t, "RS256")
	h := newHarness(t, opt(func(c *Config) { c.roots.AddCert(b.Srv.Certificate()) }))
	// The victim's flow at the attacker's provider acme/evil (issuer A,
	// the harness's).
	va := h.ferund.view("a", kindOIDC, h.oidcOf(rule{Anyone: true}))
	h.ferund.set(func(*fakeFerund) { va.Provider.Name = "evil" })
	locA, stateA := h.toIssuer(h.visit("a"))
	if cbPath(t, locA) != "/callback/acme/evil" {
		t.Fatalf("A's redirect URI %s", cbPath(t, locA))
	}
	// B's code, issued for B's own redirect URI (other/sso), with the
	// state of A's flow.
	bAuth, _ := url.Parse(locA)
	q := bAuth.Query()
	q.Set("redirect_uri", testPublic+"/callback/other/sso")
	bAuth.RawQuery = q.Encode()
	codeB := b.Issue(bAuth.String(), claims())
	rec := h.do("GET", "/callback/other/sso?code="+url.QueryEscape(codeB)+"&state="+url.QueryEscape(stateA), navigation, "")
	wantPage(t, rec, http.StatusBadRequest, msgCannotFinish)
	if n := h.ferund.callCount(); n != 1 {
		t.Fatalf("%d calls to ferund; want only the /visit's open", n)
	}
	// The flow was used up: A's own path finds nothing either.
	rec = h.do("GET", "/callback/acme/evil?code="+url.QueryEscape(codeB)+"&state="+url.QueryEscape(stateA), navigation, "")
	wantPage(t, rec, http.StatusBadRequest, msgCannotFinish)
	if len(h.ferund.accepts) != 0 {
		t.Fatalf("accepts %+v", h.ferund.accepts)
	}
	if !strings.Contains(h.logs.String(), "another provider's redirect URI") {
		t.Fatalf("no log of the refusal:\n%s", h.logs)
	}
}

// A callback finishes only the provider its flow began with: a challenge
// whose environment now names another issuer or client is refused.
func TestCallbackProviderChanged(t *testing.T) {
	for name, change := range map[string]func(v *challengeView){
		"client": func(v *challengeView) {
			v.OIDC = &oidcProvider{Issuer: v.OIDC.Issuer, ClientID: "other", ClientSecret: "x"}
		},
		"issuer": func(v *challengeView) {
			v.OIDC = &oidcProvider{Issuer: "https://other.example/", ClientID: v.OIDC.ClientID, ClientSecret: "x"}
		},
		"provider": func(v *challengeView) { v.Provider.Name = "sso2" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			v := h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
			loc, state := h.toIssuer(h.visit("s"))
			h.ferund.set(func(*fakeFerund) { change(v) })
			rec := h.callback(loc, state, claims())
			wantPage(t, rec, http.StatusBadRequest, msgCannotFinish)
			if len(h.ferund.accepts) != 0 || len(h.ferund.rejects) != 0 {
				t.Fatalf("answered: %+v %+v", h.ferund.accepts, h.ferund.rejects)
			}
		})
	}
}

// RFC 9207: an issuer whose discovery says it names itself must; one that
// does not say so may leave iss out.
func TestCallbackIssRequired(t *testing.T) {
	h := newHarness(t)
	h.issuer.Mu.Lock()
	h.issuer.IssParameter = true
	h.issuer.Mu.Unlock()
	h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
	loc, state := h.toIssuer(h.visit("s"))
	wantPage(t, h.callback(loc, state, claims()), http.StatusBadRequest, msgCannotFinish)
	loc, state = h.toIssuer(h.visit("s"))
	code := h.issuer.Issue(loc, claims())
	rec := h.do("GET", cbPath(t, loc)+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state)+"&iss="+
		url.QueryEscape(h.issuer.Issuer), navigation, "")
	if rec.Code != http.StatusSeeOther || len(h.ferund.accepts) != 1 {
		t.Fatalf("with iss: %d %s, accepts %d", rec.Code, rec.Body, len(h.ferund.accepts))
	}
}

// A failed discovery is answered from memory for 30 s: an issuer that
// fails costs one fetch, however many browsers are sent to it.
func TestFailedDiscoveryCached(t *testing.T) {
	var fetches atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	h := newHarness(t, func(*harness) harnessOpt {
		return func(c *Config) {
			c.roots = x509.NewCertPool()
			c.roots.AddCert(srv.Certificate())
		}
	})
	h.ferund.view("s", kindOIDC, &oidcProvider{Issuer: srv.URL + "/", ClientID: "c", ClientSecret: "x", Rule: rule{Anyone: true}})
	for range 3 {
		wantPage(t, h.visit("s"), http.StatusBadGateway, msgUnavailable)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("%d discovery fetches, want 1", n)
	}
	h.clock.add(failedTTL + time.Second)
	wantPage(t, h.visit("s"), http.StatusBadGateway, msgUnavailable)
	if n := fetches.Load(); n != 2 {
		t.Fatalf("%d discovery fetches after %v, want 2", n, failedTTL)
	}
}

// At most maxFlights sign-ins talk to one issuer at once; another gets
// the 502 page without a fetch, and a flight given back makes room.
func TestIssuerFlights(t *testing.T) {
	h := newHarness(t)
	var dones []func()
	for range maxFlights {
		done, ok := h.srv.issuers.enter(h.issuer.Issuer)
		if !ok {
			t.Fatal("a flight below the bound refused")
		}
		dones = append(dones, done)
	}
	if _, ok := h.srv.issuers.enter("https://another.example/"); !ok {
		t.Fatal("another issuer's flight refused")
	}
	h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
	wantPage(t, h.visit("s"), http.StatusBadGateway, msgUnavailable)
	dones[0]()
	dones[0]() // given back once only
	h.toIssuer(h.visit("s"))
	// The /visit gave its flight back: one is free, and then none.
	if _, ok := h.srv.issuers.enter(h.issuer.Issuer); !ok {
		t.Fatal("the flight given back is still taken")
	}
	if _, ok := h.srv.issuers.enter(h.issuer.Issuer); ok {
		t.Fatal("a flight past the bound taken")
	}
}

// A back-channel call whose logout_token is not shaped as a JWS is 400
// before ferund is asked for the provider (and its secret).
func TestBackchannelShapeFirst(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{"", logoutForm(""), logoutForm("garbage"), logoutForm("a.b"), logoutForm("a..c"),
		logoutForm("a.b.c.d"), logoutForm("a.b+.c")} {
		rec := h.do("POST", "/backchannel/acme/sso", nil, body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_request"`) {
			t.Errorf("%q: %d %s", body, rec.Code, rec.Body)
		}
	}
	if n := h.ferund.callCount(); n != 0 {
		t.Fatalf("%d calls to ferund", n)
	}
}

// A session ID ferund would refuse is left out of the accept, and the log
// says so without its value.
func TestDroppedSessionLogged(t *testing.T) {
	h := newHarness(t)
	h.ferund.view("s", kindOIDC, h.oidcOf(rule{Anyone: true}))
	loc, state := h.toIssuer(h.visit("s"))
	c := claims()
	long := strings.Repeat("s", maxSession+1)
	c["sid"] = long
	if rec := h.callback(loc, state, c); rec.Code != http.StatusSeeOther {
		t.Fatalf("callback %d %s", rec.Code, rec.Body)
	}
	if len(h.ferund.accepts) != 1 || h.ferund.accepts[0].Session != "" {
		t.Fatalf("accepts %+v", h.ferund.accepts)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "back-channel logout cannot end the visit") || strings.Contains(logs, long) {
		t.Fatalf("logs:\n%s", logs)
	}
}
