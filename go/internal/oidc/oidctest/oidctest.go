// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

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

// testKey is a signing key of the test provider.
type testKey struct {
	kid, alg string
	priv     crypto.Signer
}

func newTestKey(t *testing.T, kid, alg string) testKey {
	t.Helper()
	var priv crypto.Signer
	var err error
	switch alg {
	case algRS256:
		priv, err = rsa.GenerateKey(rand.Reader, 2048)
	case algES256:
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case algEdDSA:
		_, priv, err = ed25519.GenerateKey(rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	return testKey{kid: kid, alg: alg, priv: priv}
}

// jwk is the public key as the provider publishes it.
func (k testKey) jwk() map[string]string {
	enc := base64.RawURLEncoding.EncodeToString
	switch pub := k.priv.Public().(type) {
	case *rsa.PublicKey:
		return map[string]string{"kty": "RSA", "kid": k.kid, "alg": k.alg, "use": "sig", "n": enc(pub.N.Bytes()),
			"e": enc(big.NewInt(int64(pub.E)).Bytes())}
	case *ecdsa.PublicKey:
		b, _ := pub.Bytes()
		return map[string]string{"kty": "EC", "kid": k.kid, "crv": "P-256", "x": enc(b[1:33]), "y": enc(b[33:])}
	case ed25519.PublicKey:
		return map[string]string{"kty": "OKP", "kid": k.kid, "crv": "Ed25519", "x": enc(pub)}
	}
	return nil
}

// sign makes a compact JWS of payload with header.
func (k testKey) sign(t *testing.T, header map[string]any, payload []byte) string {
	t.Helper()
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	input := enc(hb) + "." + enc(payload)
	sum := sha256.Sum256([]byte(input))
	var sig []byte
	switch priv := k.priv.(type) {
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

// testProvider is an OIDC provider on an httptest TLS server: discovery,
// JWKS, token, userinfo and end-session endpoints, as Ory Hydra serves
// them (an issuer with a trailing slash, client_secret_basic, PKCE).
type testProvider struct {
	t      *testing.T
	srv    *httptest.Server
	issuer string

	mu sync.Mutex
	// published are the keys of the JWKS; signer signs the ID tokens.
	published []testKey
	signer    testKey
	// endSession: discovery names an end-session endpoint.
	endSession bool
	// discoveryIssuer overrides the issuer discovery names.
	discoveryIssuer string
	codes           map[string]grant
	access          map[string]map[string]any
	// mutate changes an ID token's claims and header before signing;
	// corrupt changes the token after.
	mutate      func(claims, header map[string]any)
	corrupt     func(token string) string
	jwksFetches int
	// noUserinfo serves no userinfo endpoint; userinfoSub overrides its
	// sub.
	userinfoSub string
}

const (
	testClientID     = "ferun"
	testClientSecret = "s3cr+et/x"
)

func newTestProvider(t *testing.T, alg string) *testProvider {
	t.Helper()
	p := &testProvider{t: t, codes: map[string]grant{}, access: map[string]map[string]any{}, endSession: true}
	p.signer = newTestKey(t, "k1", alg)
	p.published = []testKey{p.signer}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /.well-known/jwks.json", p.jwks)
	mux.HandleFunc("POST /oauth2/token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfo)
	p.srv = httptest.NewTLSServer(mux)
	t.Cleanup(p.srv.Close)
	p.issuer = p.srv.URL + "/"
	return p
}

// config is ferund's client configuration for this provider.
func (p *testProvider) config() *OIDCConfig {
	return &OIDCConfig{Issuer: p.issuer, ClientID: testClientID, ClientSecret: testClientSecret,
		Scopes: []string{"openid", "profile", "email"}, AdminGroup: "admins", HTTPClient: p.srv.Client()}
}

func (p *testProvider) discovery(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	iss := p.issuer
	if p.discoveryIssuer != "" {
		iss = p.discoveryIssuer
	}
	d := map[string]string{
		"issuer": iss, "authorization_endpoint": p.srv.URL + "/oauth2/auth", "token_endpoint": p.srv.URL + "/oauth2/token",
		"userinfo_endpoint": p.srv.URL + "/userinfo", "jwks_uri": p.srv.URL + "/.well-known/jwks.json",
	}
	if p.endSession {
		d["end_session_endpoint"] = p.srv.URL + "/oauth2/sessions/logout"
	}
	_ = json.NewEncoder(w).Encode(d)
}

func (p *testProvider) jwks(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jwksFetches++
	keys := []map[string]string{}
	for _, k := range p.published {
		keys = append(keys, k.jwk())
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

// issue hands out a code for an authorization request (the Location of
// ferund's redirect) and the person's claims.
func (p *testProvider) issue(authURL string, claims map[string]any) string {
	p.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		p.t.Fatal(err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != testClientID || q.Get("code_challenge_method") != "S256" ||
		q.Get("scope") != "openid profile email" || q.Get("state") == "" || q.Get("nonce") == "" {
		p.t.Fatalf("authorization request %s", authURL)
	}
	code := randomTestString(p.t)
	p.mu.Lock()
	p.codes[code] = grant{redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), nonce: q.Get("nonce"),
		claims: claims}
	p.mu.Unlock()
	return code
}

func (p *testProvider) token(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, e string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": e, "error_description": e + " (test provider)"})
	}
	id, secret, ok := r.BasicAuth()
	id, _ = url.QueryUnescape(id)
	secret, _ = url.QueryUnescape(secret)
	if !ok || id != testClientID || secret != testClientSecret {
		fail(401, "invalid_client")
		return
	}
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		fail(400, "invalid_request")
		return
	}
	p.mu.Lock()
	g, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || g.redirectURI != r.PostForm.Get("redirect_uri") ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		fail(400, "invalid_grant")
		return
	}
	now := time.Now()
	claims := map[string]any{"iss": p.issuer, "aud": []string{testClientID}, "exp": now.Add(5 * time.Minute).Unix(),
		"iat": now.Unix(), "nonce": g.nonce, "azp": testClientID}
	for k, v := range g.claims {
		claims[k] = v
	}
	p.mu.Lock()
	signer, mutate, corrupt := p.signer, p.mutate, p.corrupt
	p.mu.Unlock()
	header := map[string]any{"alg": signer.alg, "kid": signer.kid, "typ": "JWT"}
	if mutate != nil {
		mutate(claims, header)
	}
	payload, _ := json.Marshal(claims)
	idToken := signer.sign(p.t, header, payload)
	if corrupt != nil {
		idToken = corrupt(idToken)
	}
	access := randomTestString(p.t)
	p.mu.Lock()
	p.access[access] = g.claims
	p.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "id_token": idToken, "token_type": "bearer",
		"expires_in": 3600})
}

func (p *testProvider) userinfo(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	claims, ok := p.access[tok]
	sub := p.userinfoSub
	p.mu.Unlock()
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

func randomTestString(t *testing.T) string {
	t.Helper()
	s, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// adminClaims are an admin's claims as the provider sends them.
func adminClaims() map[string]any {
	return map[string]any{"sub": "0b8c5ef1-0000-4000-8000-000000000001", "email": "ada@example.test", "email_verified": true,
		"name": "Ada", "preferred_username": "ada", "picture": "https://example.test/ada.png", "github_id": 4242,
		"groups": []string{"admins"}}
}

func userClaims() map[string]any {
	c := adminClaims()
	c["sub"], c["email"], c["preferred_username"], c["groups"] = "0b8c5ef1-0000-4000-8000-000000000002", "bob@example.test", "bob", []string{}
	return c
}
