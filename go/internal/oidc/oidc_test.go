// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/oidc/oidctest"
)

// authURL is an authorization request as a client sends it, with the
// PKCE challenge of verifier.
func authURL(d *Discovery, clientID, state, nonce string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://rp.example/callback"},
		"scope": {"openid profile email"}, "state": {state}, "nonce": {nonce},
		// sha256 of "verifier", base64url.
		"code_challenge": {"iMnq5o6zALKXGivsnlom_0F5_WYda32GHkxlV7mq7hQ"}, "code_challenge_method": {"S256"}}
	return d.AuthorizationEndpoint + "?" + q.Encode()
}

// The exported surface end to end, as ferun-auth uses it: discovery, the
// code exchange, the ID token with its nonce, userinfo, the person, and a
// back-channel logout token.
func TestProviderFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := oidctest.New(t, AlgEdDSA)
	rp := New(Config{Issuer: p.Issuer, ClientID: p.ClientID, ClientSecret: p.ClientSecret, HTTPClient: p.Client})
	d, err := rp.Discovery(ctx)
	if err != nil || d.Issuer != p.Issuer || d.EndSessionEndpoint == "" {
		t.Fatalf("discovery %+v %v", d, err)
	}
	code := p.Issue(authURL(d, p.ClientID, "st", "n-1"), oidctest.UserClaims())
	toks, err := rp.Exchange(ctx, d, code, "https://rp.example/callback", "verifier")
	if err != nil || toks.IDToken == "" || toks.AccessToken == "" {
		t.Fatalf("exchange %+v %v", toks, err)
	}
	if _, err := rp.Verify(ctx, d, toks.IDToken, "n-2"); err == nil {
		t.Fatal("another nonce verified")
	}
	idt, err := rp.Verify(ctx, d, toks.IDToken, "n-1")
	if err != nil || idt.Raw != toks.IDToken || idt.Claims.Str("sub") != "0b8c5ef1-0000-4000-8000-000000000002" {
		t.Fatalf("verify %+v %v", idt, err)
	}
	info, err := rp.Userinfo(ctx, d, toks.AccessToken, idt.Claims.Str("sub"))
	if err != nil {
		t.Fatal(err)
	}
	if who := PersonOf(idt.Claims, info); who.Email != "bob@example.test" || who.Username != "bob" || who.GitHubID != "4242" ||
		len(who.Groups) != 0 {
		t.Fatalf("person %+v", who)
	}
	if _, err := rp.Userinfo(ctx, d, toks.AccessToken, "someone-else"); err == nil {
		t.Fatal("userinfo of another subject")
	}

	sid, sub, err := rp.VerifyLogout(ctx, d, p.LogoutToken(map[string]any{"sub": "u-1"}))
	if err != nil || sid != "sid-1" || sub != "u-1" {
		t.Fatalf("logout %q %q %v", sid, sub, err)
	}
	var re *ErrRefused
	if _, _, err := rp.VerifyLogout(ctx, d, p.LogoutToken(map[string]any{"sid": nil})); !errors.As(err, &re) ||
		re.Reason != "the token names no session (sid)" {
		t.Fatalf("logout without sid: %v", err)
	}
	if _, _, err := rp.VerifyLogout(ctx, d, p.LogoutToken(map[string]any{"nonce": "x"})); !errors.As(err, &re) {
		t.Fatalf("logout with a nonce: %v", err)
	}
	if err := Refused("x"); err.Error() != "x" {
		t.Errorf("Refused %q", err)
	}
}

// Config.Now is the clock of the checks: an ID token is expired on a
// clock ten minutes ahead.
func TestProviderClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := oidctest.New(t, AlgES256)
	rp := New(Config{Issuer: p.Issuer, ClientID: p.ClientID, ClientSecret: p.ClientSecret, HTTPClient: p.Client,
		Now: func() time.Time { return time.Now().Add(10 * time.Minute) }})
	d, err := rp.Discovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	toks, err := rp.Exchange(ctx, d, p.Issue(authURL(d, p.ClientID, "st", "n"), oidctest.AdminClaims()),
		"https://rp.example/callback", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	var re *ErrRefused
	if _, err := rp.Verify(ctx, d, toks.IDToken, "n"); !errors.As(err, &re) || re.Reason != "the token has expired" {
		t.Fatalf("verify ahead: %v", err)
	}
}

// A provider that cannot be used is an ErrProvider: a discovery naming
// another issuer, a token endpoint refusing the client.
func TestProviderErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := oidctest.New(t, AlgRS256)
	p.DiscoveryIssuer = "https://other.example/"
	var pe *ErrProvider
	if _, err := New(Config{Issuer: p.Issuer, ClientID: p.ClientID, HTTPClient: p.Client}).Discovery(ctx); !errors.As(err, &pe) {
		t.Fatalf("discovery of another issuer: %v", err)
	}
	q := oidctest.New(t, AlgRS256)
	rp := New(Config{Issuer: q.Issuer, ClientID: q.ClientID, ClientSecret: "wrong", HTTPClient: q.Client})
	d, err := rp.Discovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	code := q.Issue(authURL(d, q.ClientID, "s", "n"), oidctest.AdminClaims())
	if _, err := rp.Exchange(ctx, d, code, "https://rp.example/callback", "verifier"); !errors.As(err, &pe) {
		t.Fatalf("exchange with a wrong secret: %v", err)
	}
}

// An access token of the client credentials grant opens only as the
// client's own, for the API's audience, in its time: another audience, an
// expired one, one not valid yet or issued ahead, a person's (client_id is
// not sub), one of another issuer, a forged signature and alg none are
// refused; a token of a rotated key verifies after one refetch of the key
// set; the keys out of reach are the provider's failure.
func TestVerifyAccessToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := oidctest.New(t, AlgRS256)
	var mu sync.Mutex
	clock := time.Now()
	rp := New(Config{Issuer: p.Issuer, ClientID: p.ClientID, ClientSecret: p.ClientSecret, HTTPClient: p.Client,
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }})
	const aud = oidctest.AccessAudience
	c, err := rp.VerifyAccessToken(ctx, p.AccessToken("client-1", nil), aud)
	if err != nil || c.Sub != "client-1" || c.ClientID != "client-1" {
		t.Fatalf("verify %+v %v", c, err)
	}
	if _, err := rp.VerifyAccessToken(ctx, p.AccessToken("client-1", map[string]any{"aud": aud}), aud); err != nil {
		t.Fatalf("aud as a string: %v", err)
	}
	now := time.Now().Unix()
	for _, tc := range []struct {
		name, reason string
		over         map[string]any
		aud          string
	}{
		{"another audience", "the token is not for this API (aud)", map[string]any{"aud": []string{"https://other.example"}}, aud},
		{"an audience by prefix", "the token is not for this API (aud)", nil, aud + "/"},
		{"no audience asked", "the token is not for this API (aud)", nil, ""},
		{"expired", "the token has expired", map[string]any{"exp": now - 120}, aud},
		{"no exp", "the token has expired", map[string]any{"exp": nil}, aud},
		{"nbf ahead", "the token is not valid yet (nbf)", map[string]any{"nbf": now + 600}, aud},
		{"iat ahead", "the token has no iat or is issued in the future", map[string]any{"iat": now + 600}, aud},
		{"no iat", "the token has no iat or is issued in the future", map[string]any{"iat": nil}, aud},
		{"a person's", "the token is not a client's own (client_id is not sub)", map[string]any{"client_id": "fecode"}, aud},
		{"no client_id", "the token is not a client's own (client_id is not sub)", map[string]any{"client_id": nil}, aud},
		{"no subject", "the token names no subject", map[string]any{"sub": nil, "client_id": nil}, aud},
		{"another issuer", "the token's issuer is not the configured one", map[string]any{"iss": "https://evil.example/"}, aud},
	} {
		var re *ErrRefused
		_, err := rp.VerifyAccessToken(ctx, p.AccessToken("client-1", tc.over), tc.aud)
		if !errors.As(err, &re) || re.Reason != tc.reason {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.reason)
		}
	}
	// Within the skew it passes.
	if _, err := rp.VerifyAccessToken(ctx, p.AccessToken("client-1", map[string]any{"exp": now - 30, "nbf": now + 30}),
		aud); err != nil {
		t.Fatalf("within the skew: %v", err)
	}
	// alg none, and a token signed by a key the provider never published.
	tok := p.AccessToken("client-1", nil)
	parts := strings.Split(tok, ".")
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + parts[1] + "."
	var re *ErrRefused
	if _, err := rp.VerifyAccessToken(ctx, none, aud); !errors.As(err, &re) {
		t.Fatalf("alg none: %v", err)
	}
	forger := oidctest.NewKey(t, "k1", AlgRS256)
	forged := forger.Sign(t, map[string]any{"alg": AlgRS256, "kid": "k1"}, []byte(`{"iss":"`+p.Issuer+`","sub":"c","client_id":"c",`+
		`"aud":"`+aud+`","exp":`+strconv.FormatInt(now+600, 10)+`}`))
	if _, err := rp.VerifyAccessToken(ctx, forged, aud); !errors.As(err, &re) {
		t.Fatalf("forged: %v", err)
	}
	// A rotated key: the set is fetched again for its unknown kid, once a
	// minute at most.
	mu.Lock()
	clock = clock.Add(61 * time.Second)
	mu.Unlock()
	p.Mu.Lock()
	p.Signer = oidctest.NewKey(t, "k2", AlgRS256)
	p.Published = append(p.Published, p.Signer)
	fetches := p.JWKSFetches
	p.Mu.Unlock()
	if _, err := rp.VerifyAccessToken(ctx, p.AccessToken("client-1", nil), aud); err != nil {
		t.Fatalf("rotated: %v", err)
	}
	p.Mu.Lock()
	if p.JWKSFetches != fetches+1 {
		t.Errorf("%d fetches of the key set for the rotated key, want 1", p.JWKSFetches-fetches)
	}
	p.Mu.Unlock()
	// The provider out of reach.
	p.Srv.Close()
	down := New(Config{Issuer: p.Issuer, ClientID: p.ClientID, HTTPClient: p.Client})
	var pe *ErrProvider
	if _, err := down.VerifyAccessToken(ctx, tok, aud); !errors.As(err, &pe) {
		t.Fatalf("provider down: %v", err)
	}
}

// A failed discovery, and a failed fetch of the key set while none is
// held, answer every token for failedRetryAfter without asking the
// provider again; then it is asked once more.
func TestProviderFailureKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := oidctest.New(t, AlgRS256)
	var mu sync.Mutex
	clock := time.Now()
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	rp := New(Config{Issuer: p.Issuer, ClientID: p.ClientID, ClientSecret: p.ClientSecret, HTTPClient: p.Client,
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }})
	const aud = oidctest.AccessAudience
	tok := p.AccessToken("client-1", nil)
	fetches := func() (int, int) { p.Mu.Lock(); defer p.Mu.Unlock(); return p.DiscoveryFetches, p.JWKSFetches }
	set := func(disc, jwks bool) { p.Mu.Lock(); p.DiscoveryDown, p.JWKSDown = disc, jwks; p.Mu.Unlock() }
	var pe *ErrProvider
	set(true, false)
	for range 3 {
		if _, err := rp.VerifyAccessToken(ctx, tok, aud); !errors.As(err, &pe) {
			t.Fatalf("discovery down: %v", err)
		}
	}
	if d, _ := fetches(); d != 1 {
		t.Errorf("%d discovery fetches while it failed, want 1", d)
	}
	set(false, true)
	advance(failedRetryAfter)
	for range 3 {
		if _, err := rp.VerifyAccessToken(ctx, tok, aud); !errors.As(err, &pe) {
			t.Fatalf("key set down: %v", err)
		}
	}
	if d, k := fetches(); d != 2 || k != 1 {
		t.Errorf("%d discovery and %d key set fetches, want 2 and 1", d, k)
	}
	set(false, false)
	advance(failedRetryAfter - time.Second)
	if _, err := rp.VerifyAccessToken(ctx, tok, aud); !errors.As(err, &pe) {
		t.Fatalf("within failedRetryAfter: %v", err)
	}
	advance(time.Second)
	if _, err := rp.VerifyAccessToken(ctx, tok, aud); err != nil {
		t.Fatalf("after failedRetryAfter: %v", err)
	}
	if _, k := fetches(); k != 2 {
		t.Errorf("%d key set fetches, want 2", k)
	}
}
