// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"errors"
	"net/url"
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
