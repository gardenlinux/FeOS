// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visit

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The ask's vector, for the providers' side (FeCode verifies it in its
// own tests): the seed 00 01 … 1f, the ask about a visit for FeCode's
// address at 2026-11-13T12:00:00Z with the jti of the bytes 00 … 0f.
const (
	vectorProvider = "https://fecode.dev/api/v1/visit"
	vectorVisit    = "00000000-0000-4000-8000-0000000000a1"
	vectorIAT      = 1794571200
	vectorJTI      = "AAECAwQFBgcICQoLDA0ODw"
	vectorKID      = "Vkdap1RjR0wChd9d"
	vectorJWKS     = `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg","kid":"Vkdap1RjR0wChd9d","alg":"EdDSA","use":"sig"}]}`
	vectorJWT      = "eyJhbGciOiJFZERTQSIsInR5cCI6ImZlcnVuLWFzaytqd3QiLCJraWQiOiJWa2RhcDFSalIwd0NoZDlkIn0." +
		"eyJpc3MiOiJmZXJ1biIsImF1ZCI6Imh0dHBzOi8vZmVjb2RlLmRldi9hcGkvdjEvdmlzaXQiLCJ2aWQiOiIwMDAwMDAwMC0wMDAwLTQwMDAtODAwMC0wMDAwMDAwMDAwYTEiLCJpYXQiOjE3OTQ1NzEyMDAsImV4cCI6MTc5NDU3MTI2MCwianRpIjoiQUFFQ0F3UUZCZ2NJQ1FvTERBME9EdyJ9." +
		"owB8y44N3qMaJx4E-GrbI6JXxZSmNOOkEQ-wL-KSrljk10F9Qn83RIjogEncSmpLZk8y_CTGzsZHNJaA9WzfAQ"
)

func vectorKeys(t *testing.T) *Keys {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	k, err := NewKeys(seed, bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The vector: its bytes, its header and claims as the contract writes
// them, and its JWKS.
func TestAskVector(t *testing.T) {
	k := vectorKeys(t)
	raw, err := k.signAsk(vectorProvider, vectorVisit, time.Unix(vectorIAT, 0), vectorJTI)
	if err != nil || raw != vectorJWT {
		t.Fatalf("the vector: %s (%v)", raw, err)
	}
	if k.TokenKID() != vectorKID || string(k.JWKS()) != vectorJWKS {
		t.Fatalf("kid %s, JWKS %s", k.TokenKID(), k.JWKS())
	}
	parts := strings.Split(raw, ".")
	dec := func(s string) string {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if h := dec(parts[0]); h != `{"alg":"EdDSA","typ":"ferun-ask+jwt","kid":"`+vectorKID+`"}` {
		t.Errorf("header %s", h)
	}
	if c := dec(parts[1]); c != `{"iss":"ferun","aud":"https://fecode.dev/api/v1/visit","vid":"00000000-0000-4000-8000-0000000000a1","iat":1794571200,"exp":1794571260,"jti":"AAECAwQFBgcICQoLDA0ODw"}` {
		t.Errorf("claims %s", c)
	}
	keys, err := ParseJWKS([]byte(vectorJWKS))
	if err != nil {
		t.Fatal(err)
	}
	a, err := VerifyAsk(vectorJWT, keys, vectorProvider, vectorVisit, time.Unix(vectorIAT+30, 0))
	want := Ask{Issuer: Issuer, Audience: vectorProvider, Visit: vectorVisit, IssuedAt: vectorIAT, Expires: vectorIAT + 60, ID: vectorJTI}
	if err != nil || a != want {
		t.Fatalf("verify: %+v (%v)", a, err)
	}
}

// An ask verifies for its provider and visit within its time and a
// minute's leeway, and as nothing else.
func TestAskRefusals(t *testing.T) {
	k := vectorKeys(t)
	keys, err := ParseJWKS(k.JWKS())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(vectorIAT, 0)
	raw, err := k.SignAsk(vectorProvider, vectorVisit, now)
	if err != nil {
		t.Fatal(err)
	}
	a, err := VerifyAsk(raw, keys, vectorProvider, vectorVisit, now)
	if err != nil || len(a.ID) != 22 || a.Expires-a.IssuedAt != 60 {
		t.Fatalf("a fresh ask: %+v (%v)", a, err)
	}
	if again, _ := k.SignAsk(vectorProvider, vectorVisit, now); again == raw {
		t.Error("two asks with one jti")
	}
	// The time: exp > now − 60 s, iat < now + 60 s.
	for at, ok := range map[int64]bool{vectorIAT - 59: true, vectorIAT + 119: true, vectorIAT + 120: false, vectorIAT - 60: false} {
		_, err := VerifyAsk(raw, keys, vectorProvider, vectorVisit, time.Unix(at, 0))
		if ok != (err == nil) || (!ok && !errors.Is(err, ErrClaims)) {
			t.Errorf("at %+d s: %v", at-vectorIAT, err)
		}
	}
	for name, c := range map[string]struct{ provider, visit string }{
		"another provider":     {"https://fecode.dev/api/v1/visit/", vectorVisit},
		"another provider too": {"https://evil.example/api/v1/visit", vectorVisit},
		"another visit":        {vectorProvider, "00000000-0000-4000-8000-0000000000a2"},
	} {
		if _, err := VerifyAsk(raw, keys, c.provider, c.visit, now); !errors.Is(err, ErrClaims) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other, _ := ParseJWKS(testKeys(t, 9, 2).JWKS())
	if _, err := VerifyAsk(raw, other, vectorProvider, vectorVisit, now); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("another key: %v", err)
	}
	// A token or a code is no ask, and an ask neither of them.
	tok, _ := k.SignToken(testToken())
	if _, err := VerifyAsk(tok, keys, vectorProvider, vectorVisit, now); !errors.Is(err, ErrType) {
		t.Errorf("a token as an ask: %v", err)
	}
	if _, err := k.VerifyToken(raw); !errors.Is(err, ErrType) {
		t.Errorf("an ask as a token: %v", err)
	}
	if _, err := k.VerifyCode(raw); !errors.Is(err, ErrType) {
		t.Errorf("an ask as a code: %v", err)
	}
	// A flipped byte, an unsigned one, an oversize one.
	parts := strings.Split(raw, ".")
	enc := base64.RawURLEncoding.EncodeToString
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sig[4] ^= 1
	b := []byte(enc(sig))
	for name, c := range map[string]struct {
		raw  string
		want error
	}{
		"flipped":  {parts[0] + "." + parts[1] + "." + string(b), ErrSignature},
		"unsigned": {enc([]byte(`{"alg":"none","typ":"ferun-ask+jwt","kid":"`+vectorKID+`"}`)) + "." + parts[1] + ".", ErrMalformed},
		"oversize": {strings.Repeat("a", MaxJWT+1), ErrMalformed},
		"empty":    {"", ErrMalformed},
	} {
		if _, err := VerifyAsk(c.raw, keys, vectorProvider, vectorVisit, now); !errors.Is(err, c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Signed by the key, but living longer than 5 minutes, or without its
	// jti.
	for name, claims := range map[string]Ask{
		"301 s":  {Issuer: Issuer, Audience: vectorProvider, Visit: vectorVisit, IssuedAt: vectorIAT, Expires: vectorIAT + 301, ID: vectorJTI},
		"no jti": {Issuer: Issuer, Audience: vectorProvider, Visit: vectorVisit, IssuedAt: vectorIAT, Expires: vectorIAT + 60},
	} {
		js, _ := json.Marshal(claims)
		input := encodeHeader(TypAsk, k.tokenKID) + "." + enc(js)
		forged := input + "." + enc(ed25519.Sign(k.priv, []byte(input)))
		if _, err := VerifyAsk(forged, keys, vectorProvider, vectorVisit, now); err == nil {
			t.Errorf("%s verifies", name)
		}
	}
	// Signing needs the private key, a provider and a visit.
	pub, err := PublicKeys(k.TokenKID(), k.TokenKey(), k.ChallengeKID(), k.ChallengeKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.SignAsk(vectorProvider, vectorVisit, now); !errors.Is(err, ErrNoSigner) {
		t.Errorf("signing with the public keys: %v", err)
	}
	if _, err := k.SignAsk("", vectorVisit, now); !errors.Is(err, ErrMalformed) {
		t.Errorf("an ask without a provider: %v", err)
	}
}

// ParseJWKS takes the JWKS as written, and refuses a key of another kind
// or under another's kid.
func TestParseJWKS(t *testing.T) {
	k := vectorKeys(t)
	keys, err := ParseJWKS(k.JWKS())
	if err != nil || !reflect.DeepEqual(keys, map[string]ed25519.PublicKey{vectorKID: k.TokenKey()}) {
		t.Fatalf("%v (%v)", keys, err)
	}
	x := `"x":"A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg"`
	for name, raw := range map[string]string{
		"empty":       `{"keys":[]}`,
		"no JSON":     `keys`,
		"another kty": `{"keys":[{"kty":"EC","crv":"Ed25519",` + x + `,"kid":"Vkdap1RjR0wChd9d"}]}`,
		"another crv": `{"keys":[{"kty":"OKP","crv":"X25519",` + x + `,"kid":"Vkdap1RjR0wChd9d"}]}`,
		"another alg": `{"keys":[{"kty":"OKP","crv":"Ed25519",` + x + `,"kid":"Vkdap1RjR0wChd9d","alg":"ES256"}]}`,
		"another kid": `{"keys":[{"kty":"OKP","crv":"Ed25519",` + x + `,"kid":"AAAAAAAAAAAAAAAA"}]}`,
		"a short x":   `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"AAAA","kid":"Vkdap1RjR0wChd9d"}]}`,
		"another use": `{"keys":[{"kty":"OKP","crv":"Ed25519",` + x + `,"kid":"Vkdap1RjR0wChd9d","use":"enc"}]}`,
	} {
		if _, err := ParseJWKS([]byte(raw)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseJWKS([]byte(`{"keys":[{"kty":"OKP","crv":"Ed25519",` + x + `,"kid":"Vkdap1RjR0wChd9d"}]}`)); err != nil {
		t.Errorf("without alg and use: %v", err)
	}
}
