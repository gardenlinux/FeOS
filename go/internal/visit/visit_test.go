// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visit

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func testKeys(t *testing.T, seedByte, keyByte byte) *Keys {
	t.Helper()
	k, err := NewKeys(bytes.Repeat([]byte{seedByte}, 32), bytes.Repeat([]byte{keyByte}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testToken() Token {
	return Token{Subject: "u-123", Env: "00000000-0000-4000-8000-000000000001", Visit: "00000000-0000-4000-8000-0000000000a1",
		Host: "stage--kino.apps.example", Provider: "fecode", Email: "ada@example.com", Name: "Ada",
		Binding: BindingHash("secret"), IssuedAt: 1000, Expires: 1900, VisitEnds: 5000}
}

func testCode() Code {
	return Code{Challenge: strings.Repeat("c", 22), Env: "00000000-0000-4000-8000-000000000001", Host: "stage--kino.apps.example",
		Path: "/menu?x=1", Binding: BindingHash("secret"), Outcome: OutcomeAccepted,
		Visit: "00000000-0000-4000-8000-0000000000a1", IssuedAt: 1000, Expires: 1120}
}

// A token and a code verify as what they are, and each only as itself;
// the claims come back as signed, with iss set.
func TestJWTRoundTrip(t *testing.T) {
	k := testKeys(t, 1, 2)
	tok, err := k.SignToken(testToken())
	if err != nil {
		t.Fatal(err)
	}
	got, err := k.VerifyToken(tok)
	want := testToken()
	want.Issuer = Issuer
	if err != nil || got != want {
		t.Fatalf("token: %+v %v", got, err)
	}
	code, err := k.SignCode(testCode())
	if err != nil {
		t.Fatal(err)
	}
	gc, err := k.VerifyCode(code)
	wc := testCode()
	wc.Issuer = Issuer
	if err != nil || gc != wc {
		t.Fatalf("code: %+v %v", gc, err)
	}
	rejected := testCode()
	rejected.Outcome, rejected.Visit = OutcomeRejected, ""
	rc, err := k.SignCode(rejected)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := k.VerifyCode(rc); err != nil || c.Outcome != OutcomeRejected || c.Visit != "" {
		t.Fatalf("rejected code: %+v %v", c, err)
	}
	if _, err := k.VerifyToken(code); !errors.Is(err, ErrType) {
		t.Errorf("a code as a token: %v", err)
	}
	if _, err := k.VerifyCode(tok); !errors.Is(err, ErrType) {
		t.Errorf("a token as a code: %v", err)
	}
	// The header is exactly the documented one.
	h, _ := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[0])
	if string(h) != `{"alg":"EdDSA","kid":"`+k.TokenKID()+`","typ":"ferun-visit+jwt"}` {
		t.Errorf("header %s", h)
	}
	// Times are never checked here.
	old := testToken()
	old.IssuedAt, old.Expires, old.VisitEnds = 1, 2, 3
	if tok, err := k.SignToken(old); err != nil {
		t.Fatal(err)
	} else if _, err := k.VerifyToken(tok); err != nil {
		t.Errorf("a token long past its end: %v", err)
	}
}

// Every refusal has its error.
func TestJWTRefusals(t *testing.T) {
	k := testKeys(t, 1, 2)
	tok, err := k.SignToken(testToken())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	flip := func(s string, i int) string {
		b := []byte(s)
		if b[i] == 'A' {
			b[i] = 'B'
		} else {
			b[i] = 'A'
		}
		return string(b)
	}
	if _, err := k.VerifyToken(parts[0] + "." + flip(parts[1], 5) + "." + parts[2]); !errors.Is(err, ErrSignature) {
		t.Errorf("a flipped byte in the claims: %v", err)
	}
	if _, err := k.VerifyToken(parts[0] + "." + parts[1] + "." + flip(parts[2], 3)); !errors.Is(err, ErrSignature) {
		t.Errorf("a flipped byte in the signature: %v", err)
	}
	other := testKeys(t, 3, 2)
	if _, err := other.VerifyToken(tok); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("another key: %v", err)
	}
	// The same key ID with another key cannot be had; a header that names
	// the right kid but is spelled otherwise is malformed.
	enc := base64.RawURLEncoding.EncodeToString
	spaced := enc([]byte(`{"alg":"EdDSA", "kid":"` + k.TokenKID() + `","typ":"ferun-visit+jwt"}`))
	for name, raw := range map[string]string{
		"oversize":       strings.Repeat("a", MaxJWT+1),
		"empty":          "",
		"two parts":      parts[0] + "." + parts[1],
		"four parts":     tok + ".x",
		"padding":        tok + "=",
		"alg none":       enc([]byte(`{"alg":"none","kid":"`+k.TokenKID()+`","typ":"ferun-visit+jwt"}`)) + "." + parts[1] + ".",
		"another alg":    enc([]byte(`{"alg":"HS256","kid":"`+k.TokenKID()+`","typ":"ferun-visit+jwt"}`)) + "." + parts[1] + "." + parts[2],
		"unknown typ":    enc([]byte(`{"alg":"EdDSA","kid":"`+k.TokenKID()+`","typ":"JWT"}`)) + "." + parts[1] + "." + parts[2],
		"other spelling": spaced + "." + parts[1] + "." + parts[2],
		"short sig":      parts[0] + "." + parts[1] + "." + enc([]byte("short")),
	} {
		if _, err := k.VerifyToken(raw); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
	}
	// Claims that miss a field do not verify, even signed by the key.
	missing := []byte(`{"iss":"ferun","sub":"u","aud":"e","vid":"v","hst":"h","prv":"p","iat":1,"exp":2,"vend":3}`)
	input := k.header(TypToken) + "." + enc(missing)
	forged := input + "." + enc(ed25519.Sign(k.priv, []byte(input)))
	if _, err := k.VerifyToken(forged); !errors.Is(err, ErrMalformed) {
		t.Errorf("claims without vbnd: %v", err)
	}
	wrongIss := []byte(`{"iss":"other","sub":"u","aud":"e","vid":"v","hst":"h","prv":"p","vbnd":"b","iat":1,"exp":2,"vend":3}`)
	input = k.header(TypToken) + "." + enc(wrongIss)
	if _, err := k.VerifyToken(input + "." + enc(ed25519.Sign(k.priv, []byte(input)))); !errors.Is(err, ErrMalformed) {
		t.Errorf("another iss: %v", err)
	}
	// Signing refuses what verifying would.
	bad := testToken()
	bad.Binding = ""
	if _, err := k.SignToken(bad); !errors.Is(err, ErrMalformed) {
		t.Errorf("signing a token without vbnd: %v", err)
	}
	accepted := testCode()
	accepted.Visit = ""
	if _, err := k.SignCode(accepted); !errors.Is(err, ErrMalformed) {
		t.Errorf("signing an accepted code without its visit: %v", err)
	}
	odd := testCode()
	odd.Outcome = "maybe"
	if _, err := k.SignCode(odd); !errors.Is(err, ErrMalformed) {
		t.Errorf("signing a code of another outcome: %v", err)
	}
	huge := testToken()
	huge.Name = strings.Repeat("n", MaxJWT)
	if _, err := k.SignToken(huge); !errors.Is(err, ErrMalformed) {
		t.Errorf("signing an oversize token: %v", err)
	}
}

// The ingress's keys verify and seal, and sign nothing; their IDs must be
// those of their keys.
func TestPublicKeys(t *testing.T) {
	k := testKeys(t, 1, 2)
	if !k.CanSign() || len(k.TokenKID()) != 16 || len(k.ChallengeKID()) != 16 || len(k.TokenKey()) != 32 ||
		len(k.ChallengeKey()) != 32 {
		t.Fatalf("keys %q %q", k.TokenKID(), k.ChallengeKID())
	}
	p, err := PublicKeys(k.TokenKID(), k.TokenKey(), k.ChallengeKID(), k.ChallengeKey())
	if err != nil {
		t.Fatal(err)
	}
	if p.CanSign() {
		t.Fatal("public keys can sign")
	}
	tok, _ := k.SignToken(testToken())
	if _, err := p.VerifyToken(tok); err != nil {
		t.Errorf("verify with the public keys: %v", err)
	}
	if _, err := p.SignToken(testToken()); !errors.Is(err, ErrNoSigner) {
		t.Errorf("sign with the public keys: %v", err)
	}
	if _, err := p.SignCode(testCode()); !errors.Is(err, ErrNoSigner) {
		t.Errorf("sign a code with the public keys: %v", err)
	}
	other := testKeys(t, 3, 4)
	for name, c := range map[string][4]any{
		"token kid":     {other.TokenKID(), k.TokenKey(), k.ChallengeKID(), k.ChallengeKey()},
		"challenge kid": {k.TokenKID(), k.TokenKey(), other.ChallengeKID(), k.ChallengeKey()},
		"token key":     {k.TokenKID(), []byte{1, 2}, k.ChallengeKID(), k.ChallengeKey()},
		"challenge key": {k.TokenKID(), k.TokenKey(), k.ChallengeKID(), []byte{1}},
	} {
		tk, _ := c[1].([]byte)
		if pk, ok := c[1].(ed25519.PublicKey); ok {
			tk = pk
		}
		if _, err := PublicKeys(c[0].(string), tk, c[2].(string), c[3].([]byte)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := NewKeys([]byte{1}, make([]byte, 32)); !errors.Is(err, ErrMalformed) {
		t.Errorf("a short seed: %v", err)
	}
	// The copies are copies.
	k.ChallengeKey()[0] ^= 0xff
	k.TokenKey()[0] ^= 0xff
	if !bytes.Equal(k.ChallengeKey(), bytes.Repeat([]byte{2}, 32)) {
		t.Error("ChallengeKey shares its bytes")
	}
	if _, err := p.VerifyToken(tok); err != nil {
		t.Error("TokenKey shares its bytes")
	}
}

// The JWKS of a fixed seed, byte for byte.
func TestJWKS(t *testing.T) {
	k := testKeys(t, 7, 8)
	x := base64.RawURLEncoding.EncodeToString(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)).Public().(ed25519.PublicKey))
	want := `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + x + `","kid":"` + k.TokenKID() + `","alg":"EdDSA","use":"sig"}]}`
	if got := string(k.JWKS()); got != want {
		t.Fatalf("JWKS\n got %s\nwant %s", got, want)
	}
	// Pinned (checked with another implementation): a change of the
	// format or of the IDs shows here.
	if want := `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"6kpsY-KcUgq-9VB7Ey7F-ZVHdq6-vnuSQh7qaRRG0iw","kid":"` + k.TokenKID() + `","alg":"EdDSA","use":"sig"}]}`; string(k.JWKS()) != want {
		t.Errorf("JWKS %s", k.JWKS())
	}
	if k.TokenKID() != "_oEsEvOrTOasXbaa" {
		t.Errorf("TokenKID %s", k.TokenKID())
	}
	if k.ChallengeKID() != "zrYc9xfmZ8fbqvcM" {
		t.Errorf("ChallengeKID %s", k.ChallengeKID())
	}
}

func testChallenge(t *testing.T) Challenge {
	t.Helper()
	id, err := NewChallengeID()
	if err != nil {
		t.Fatal(err)
	}
	return Challenge{ID: id, Env: "00000000-0000-4000-8000-000000000001", Host: "stage--kino.apps.example",
		Path: "/menu?x=1&y=<2>", Binding: BindingHash("secret"), IssuedAt: 1000, Expires: 1600}
}

// A sealed challenge opens with the keys that sealed it, and the ingress's
// public keys, as it was; two seals of one differ; an expired one opens.
func TestChallengeSeal(t *testing.T) {
	k := testKeys(t, 1, 2)
	c := testChallenge(t)
	if len(c.ID) != 22 {
		t.Fatalf("challenge ID %q", c.ID)
	}
	a, err := k.SealChallenge(c)
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.SealChallenge(c)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two seals of one challenge are equal")
	}
	p, _ := PublicKeys(k.TokenKID(), k.TokenKey(), k.ChallengeKID(), k.ChallengeKey())
	for _, s := range []string{a, b} {
		for _, keys := range []*Keys{k, p} {
			got, err := keys.OpenChallenge(s)
			if err != nil || got != c {
				t.Fatalf("open: %+v %v", got, err)
			}
		}
	}
	// The issued and expiry times are the ingress's, and nobody checks
	// them here: one long past opens.
	c.IssuedAt, c.Expires = 1, 2
	old, err := k.SealChallenge(c)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := k.OpenChallenge(old); err != nil || got.Expires != 2 {
		t.Errorf("an expired challenge: %+v %v", got, err)
	}
	// The seal's layout.
	raw, _ := base64.RawURLEncoding.DecodeString(a)
	kid, _ := base64.RawURLEncoding.DecodeString(k.ChallengeKID())
	if raw[0] != 1 || !bytes.Equal(raw[1:13], kid) {
		t.Errorf("seal begins %x", raw[:13])
	}
}

func TestChallengeRefusals(t *testing.T) {
	k := testKeys(t, 1, 2)
	c := testChallenge(t)
	s, err := k.SealChallenge(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testKeys(t, 1, 9).OpenChallenge(s); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("another challenge key: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(s)
	enc := base64.RawURLEncoding.EncodeToString
	tampered := bytes.Clone(raw)
	tampered[len(tampered)-20] ^= 1
	salted := bytes.Clone(raw)
	salted[14] ^= 1
	version := bytes.Clone(raw)
	version[0] = 2
	for name, v := range map[string]string{
		"empty":    "",
		"oversize": strings.Repeat("A", MaxSealed+1),
		"base64":   s + "!",
		"short":    enc(raw[:40]),
		"tampered": enc(tampered),
		"salt":     enc(salted),
		"version":  enc(version),
	} {
		if _, err := k.OpenChallenge(v); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, mod := range map[string]func(*Challenge){
		"id":      func(c *Challenge) { c.ID = "short" },
		"env":     func(c *Challenge) { c.Env = "" },
		"host":    func(c *Challenge) { c.Host = "" },
		"path":    func(c *Challenge) { c.Path = "menu" },
		"binding": func(c *Challenge) { c.Binding = "x" },
		"times":   func(c *Challenge) { c.Expires = 0 },
		// Bytes JSON writes as six do not fit the bound.
		"huge": func(c *Challenge) { c.Path = "/" + strings.Repeat("\x01", MaxPath-1) },
	} {
		bad := c
		mod(&bad)
		if _, err := k.SealChallenge(bad); !errors.Is(err, ErrMalformed) {
			t.Errorf("seal %s: %v", name, err)
		}
	}
	// The longest path made of characters that JSON leaves alone fits.
	long := c
	long.Path = "/" + strings.Repeat("a&b", (MaxPath-1)/3)
	if s, err := k.SealChallenge(long); err != nil || len(s) > MaxSealed {
		t.Errorf("a path of MaxPath: %d %v", len(s), err)
	}
}

func TestBindings(t *testing.T) {
	s, err := NewBindingSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 43 || !ValidBindingSecret(s) {
		t.Fatalf("secret %q", s)
	}
	t2, _ := NewBindingSecret()
	if s == t2 {
		t.Fatal("two secrets are equal")
	}
	if h := BindingHash(s); len(h) != 43 || h == s || !ValidBindingSecret(h) {
		t.Errorf("hash %q", h)
	}
	// sha256("abc"), base64url without padding.
	if h := BindingHash("abc"); h != "ungWv48Bz-pBQUDeXa4iI7ADYaOWF3qctBD_YfIAFa0" {
		t.Errorf("BindingHash(abc) = %s", h)
	}
	for _, bad := range []string{"", s[:42], s + "A", strings.Repeat("+", 43), strings.Repeat("=", 43)} {
		if ValidBindingSecret(bad) {
			t.Errorf("%q is a secret", bad)
		}
	}
}

func TestVisitCookie(t *testing.T) {
	k := testKeys(t, 1, 2)
	tok, _ := k.SignToken(testToken())
	sec, _ := NewBindingSecret()
	v := VisitCookie(tok, sec)
	if gt, gs, ok := ParseVisitCookie(v); !ok || gt != tok || gs != sec {
		t.Fatalf("round trip: %q %q %v", gt, gs, ok)
	}
	for name, bad := range map[string]string{
		"no tilde":    tok + sec,
		"bad secret":  tok + "~" + sec[:42],
		"no token":    "~" + sec,
		"no secret":   tok + "~",
		"long token":  strings.Repeat("a", MaxJWT+1) + "~" + sec,
		"secret char": tok + "~" + sec[:42] + ".",
	} {
		if _, _, ok := ParseVisitCookie(bad); ok {
			t.Errorf("%s: parsed", name)
		}
	}
	// The last "~" splits.
	if gt, gs, ok := ParseVisitCookie("a~b~" + sec); !ok || gt != "a~b" || gs != sec {
		t.Errorf("two tildes: %q %q %v", gt, gs, ok)
	}
}

func TestIsVisitorHeader(t *testing.T) {
	for _, h := range []string{"FeRun-Visitor", "ferun_visitor", "FeRun.Visitor", "FeRun~Visit~Token", "FeRun+Visitor-Email",
		"FeRun-Visit-Token", "FERUN_VISIT_TOKEN", "FeRun-Visits", "ferun-visitor-email", "ferun-visit"} {
		if !IsVisitorHeader(h) {
			t.Errorf("%s is not a visitor header", h)
		}
	}
	for _, h := range []string{"X-FeRun-Visitor", "FeRun-Tenant", "FeRun-Visi", "", "Visitor", "FeRunVisitor", "FeRun-Vis1t"} {
		if IsVisitorHeader(h) {
			t.Errorf("%s is a visitor header", h)
		}
	}
}

func TestDropVisitorHeaders(t *testing.T) {
	h := http.Header{
		"Ferun_visitor":     {"admin"}, // a non-canonical key, as a map literal keeps it
		"Ferun-Visit-Token": {"x"},
		"Ferun.visitor":     {"y"},
		"Accept":            {"text/html"},
		"X-Ferun-Visitor":   {"kept"},
	}
	if n := DropVisitorHeaders(h); n != 3 {
		t.Errorf("dropped %d", n)
	}
	want := http.Header{"Accept": {"text/html"}, "X-Ferun-Visitor": {"kept"}}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("left %v", h)
	}
	if n := DropVisitorHeaders(nil); n != 0 {
		t.Errorf("nil: %d", n)
	}
}

func TestSafePath(t *testing.T) {
	for in, want := range map[string]string{
		"/":                                      "/",
		"/a?b":                                   "/a?b",
		"/menu/x?y=1&z=%2F":                      "/menu/x?y=1&z=%2F",
		"//evil.example/x":                       "/",
		`/\evil`:                                 "/",
		"":                                       "/",
		"evil.example":                           "/",
		"https://evil.example/":                  "/",
		"/a\tb":                                  "/",
		"/\t/evil.example":                       "/",
		"/a\nb":                                  "/",
		"/a\x7fb":                                "/",
		"/caf\xc3\xa9":                           "/",
		"/" + strings.Repeat("a", 1999):          "/",
		"/" + strings.Repeat("a", MaxPath-1):     "/" + strings.Repeat("a", MaxPath-1),
		"/" + strings.Repeat("a", MaxPath):       "/",
		"/a/\\b":                                 "/a/\\b",
		"/./evil":                                "/./evil",
		"/%2F%2Fevil.example":                    "/%2F%2Fevil.example",
		"/?next=//evil.example":                  "/?next=//evil.example",
		"/" + strings.Repeat("b", 2000) + "?q=1": "/",
	} {
		if got := SafePath(in); got != want {
			t.Errorf("SafePath(%.40q) = %.40q, want %.40q", in, got, want)
		}
	}
}
