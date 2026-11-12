// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package oidctest is an OpenID Connect provider for tests: discovery,
// JWKS, token and userinfo endpoints on an httptest TLS server, with
// signing keys of each algorithm the oidc package verifies. It imports
// nothing of oidc, whose own tests use it.
package oidctest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Key is a signing key of the test provider: its key id, its algorithm
// ("RS256", "ES256" or "EdDSA") and the private key.
type Key struct {
	Kid, Alg string
	Priv     crypto.Signer
}

// NewKey makes a key of the algorithm alg ("RS256", "ES256" or "EdDSA").
func NewKey(t *testing.T, kid, alg string) Key {
	t.Helper()
	var priv crypto.Signer
	var err error
	switch alg {
	case "RS256":
		priv, err = rsa.GenerateKey(rand.Reader, 2048)
	case "ES256":
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "EdDSA":
		_, priv, err = ed25519.GenerateKey(rand.Reader)
	default:
		t.Fatalf("no keys of the algorithm %q", alg)
	}
	if err != nil {
		t.Fatal(err)
	}
	return Key{Kid: kid, Alg: alg, Priv: priv}
}

// JWK is the public key as the provider publishes it.
func (k Key) JWK() map[string]string {
	enc := base64.RawURLEncoding.EncodeToString
	switch pub := k.Priv.Public().(type) {
	case *rsa.PublicKey:
		return map[string]string{"kty": "RSA", "kid": k.Kid, "alg": k.Alg, "use": "sig", "n": enc(pub.N.Bytes()),
			"e": enc(big.NewInt(int64(pub.E)).Bytes())}
	case *ecdsa.PublicKey:
		b, _ := pub.Bytes()
		return map[string]string{"kty": "EC", "kid": k.Kid, "crv": "P-256", "x": enc(b[1:33]), "y": enc(b[33:])}
	case ed25519.PublicKey:
		return map[string]string{"kty": "OKP", "kid": k.Kid, "crv": "Ed25519", "x": enc(pub)}
	}
	return nil
}

// Sign makes a compact JWS of payload with header.
func (k Key) Sign(t *testing.T, header map[string]any, payload []byte) string {
	t.Helper()
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	input := enc(hb) + "." + enc(payload)
	sum := sha256.Sum256([]byte(input))
	var sig []byte
	switch priv := k.Priv.(type) {
	case *rsa.PrivateKey:
		sig, err = rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	case *ecdsa.PrivateKey:
		var r, s *big.Int
		r, s, err = ecdsa.Sign(rand.Reader, priv, sum[:])
		if err == nil {
			sig = make([]byte, 64)
			r.FillBytes(sig[:32])
			s.FillBytes(sig[32:])
		}
	case ed25519.PrivateKey:
		sig = ed25519.Sign(priv, []byte(input))
	}
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + enc(sig)
}

// grant is a code the provider handed out.
type grant struct {
	redirectURI, challenge, nonce string
	claims                        map[string]any
}

// Provider is an OIDC provider on an httptest TLS server: discovery,
// JWKS, token, userinfo and end-session endpoints, as Ory Hydra serves
// them (an issuer with a trailing slash, client_secret_basic, PKCE).
// Issuer, ClientID, ClientSecret and Client (which trusts the server's
// certificate) are what a client's configuration takes. The fields below
// Mu change what it serves; hold Mu to change or read them while it runs.
type Provider struct {
	T            *testing.T
	Srv          *httptest.Server
	Issuer       string
	ClientID     string
	ClientSecret string
	Client       *http.Client

	Mu sync.Mutex
	// Published are the keys of the JWKS; Signer signs the ID tokens.
	Published []Key
	Signer    Key
	// EndSession: discovery names an end-session endpoint.
	EndSession bool
	// DiscoveryIssuer overrides the issuer discovery names.
	DiscoveryIssuer string
	codes           map[string]grant
	access          map[string]map[string]any
	// Mutate changes an ID token's claims and header before signing;
	// Corrupt changes the token after.
	Mutate      func(claims, header map[string]any)
	Corrupt     func(token string) string
	JWKSFetches int
	// UserinfoSub overrides the sub of userinfo.
	UserinfoSub string
}

// The test provider's client.
const (
	ClientID     = "ferun"
	ClientSecret = "s3cr+et/x"
)

// New starts a provider whose ID tokens are signed with a key of alg.
func New(t *testing.T, alg string) *Provider {
	t.Helper()
	p := &Provider{T: t, ClientID: ClientID, ClientSecret: ClientSecret, codes: map[string]grant{},
		access: map[string]map[string]any{}, EndSession: true}
	p.Signer = NewKey(t, "k1", alg)
	p.Published = []Key{p.Signer}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /.well-known/jwks.json", p.jwks)
	mux.HandleFunc("POST /oauth2/token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfo)
	p.Srv = httptest.NewTLSServer(mux)
	t.Cleanup(p.Srv.Close)
	p.Issuer = p.Srv.URL + "/"
	p.Client = p.Srv.Client()
	return p
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	iss := p.Issuer
	if p.DiscoveryIssuer != "" {
		iss = p.DiscoveryIssuer
	}
	d := map[string]string{
		"issuer": iss, "authorization_endpoint": p.Srv.URL + "/oauth2/auth", "token_endpoint": p.Srv.URL + "/oauth2/token",
		"userinfo_endpoint": p.Srv.URL + "/userinfo", "jwks_uri": p.Srv.URL + "/.well-known/jwks.json",
	}
	if p.EndSession {
		d["end_session_endpoint"] = p.Srv.URL + "/oauth2/sessions/logout"
	}
	_ = json.NewEncoder(w).Encode(d)
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	p.JWKSFetches++
	keys := []map[string]string{}
	for _, k := range p.Published {
		keys = append(keys, k.JWK())
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

// Issue hands out a code for an authorization request (the Location of
// a client's redirect) and the person's claims.
func (p *Provider) Issue(authURL string, claims map[string]any) string {
	p.T.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		p.T.Fatal(err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != p.ClientID || q.Get("code_challenge_method") != "S256" ||
		q.Get("scope") != "openid profile email" || q.Get("state") == "" || q.Get("nonce") == "" {
		p.T.Fatalf("authorization request %s", authURL)
	}
	code := RandomString(p.T)
	p.Mu.Lock()
	p.codes[code] = grant{redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), nonce: q.Get("nonce"),
		claims: claims}
	p.Mu.Unlock()
	return code
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, e string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": e, "error_description": e + " (test provider)"})
	}
	id, secret, ok := r.BasicAuth()
	id, _ = url.QueryUnescape(id)
	secret, _ = url.QueryUnescape(secret)
	if !ok || id != p.ClientID || secret != p.ClientSecret {
		fail(401, "invalid_client")
		return
	}
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		fail(400, "invalid_request")
		return
	}
	p.Mu.Lock()
	g, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	p.Mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || g.redirectURI != r.PostForm.Get("redirect_uri") ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		fail(400, "invalid_grant")
		return
	}
	now := time.Now()
	claims := map[string]any{"iss": p.Issuer, "aud": []string{p.ClientID}, "exp": now.Add(5 * time.Minute).Unix(),
		"iat": now.Unix(), "nonce": g.nonce, "azp": p.ClientID}
	for k, v := range g.claims {
		claims[k] = v
	}
	p.Mu.Lock()
	signer, mutate, corrupt := p.Signer, p.Mutate, p.Corrupt
	p.Mu.Unlock()
	header := map[string]any{"alg": signer.Alg, "kid": signer.Kid, "typ": "JWT"}
	if mutate != nil {
		mutate(claims, header)
	}
	payload, _ := json.Marshal(claims)
	idToken := signer.Sign(p.T, header, payload)
	if corrupt != nil {
		idToken = corrupt(idToken)
	}
	access := RandomString(p.T)
	p.Mu.Lock()
	p.access[access] = g.claims
	p.Mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "id_token": idToken, "token_type": "bearer",
		"expires_in": 3600})
}

func (p *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.Mu.Lock()
	claims, ok := p.access[tok]
	sub := p.UserinfoSub
	p.Mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	out := map[string]any{}
	for k, v := range claims {
		out[k] = v
	}
	if sub != "" {
		out["sub"] = sub
	}
	_ = json.NewEncoder(w).Encode(out)
}

// RandomString is 32 random bytes, base64url without padding.
func RandomString(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// backchannelEvent is the member of a logout token's events claim
// (OpenID Connect Back-Channel Logout 1.0, §2.4).
const backchannelEvent = "http://schemas.openid.net/event/backchannel-logout"

// LogoutToken signs a back-channel logout token for the provider's client
// with claims over the valid defaults (sid "sid-1"; nil deletes one).
func (p *Provider) LogoutToken(over map[string]any) string {
	p.T.Helper()
	c := map[string]any{"iss": p.Issuer, "aud": p.ClientID, "iat": time.Now().Unix(), "jti": RandomString(p.T),
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
		p.T.Fatal(err)
	}
	p.Mu.Lock()
	signer := p.Signer
	p.Mu.Unlock()
	return signer.Sign(p.T, map[string]any{"alg": signer.Alg, "kid": signer.Kid, "typ": "logout+jwt"}, payload)
}

// AdminClaims are an admin's claims as the provider sends them.
func AdminClaims() map[string]any {
	return map[string]any{"sub": "0b8c5ef1-0000-4000-8000-000000000001", "email": "ada@example.test", "email_verified": true,
		"name": "Ada", "preferred_username": "ada", "picture": "https://example.test/ada.png", "github_id": 4242,
		"groups": []string{"admins"}}
}

// UserClaims are another person's claims, without the admins group.
func UserClaims() map[string]any {
	c := AdminClaims()
	c["sub"], c["email"], c["preferred_username"], c["groups"] = "0b8c5ef1-0000-4000-8000-000000000002", "bob@example.test", "bob", []string{}
	return c
}
