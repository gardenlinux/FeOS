// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visit

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
)

// Token is the visit's token's claims (a JWT, alg EdDSA, typ TypToken).
type Token struct {
	Issuer    string `json:"iss"` // Issuer; set by SignToken
	Subject   string `json:"sub"`
	Env       string `json:"aud"` // the environment's ID
	Visit     string `json:"vid"`
	Host      string `json:"hst"`
	Provider  string `json:"prv"` // the provider's name
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
	Binding   string `json:"vbnd"` // BindingHash of the visit cookie's secret (S12)
	IssuedAt  int64  `json:"iat"`
	Expires   int64  `json:"exp"`
	VisitEnds int64  `json:"vend"`
}

func (t *Token) complete() bool {
	return t.Issuer == Issuer && t.Subject != "" && t.Env != "" && t.Visit != "" && t.Host != "" &&
		t.Provider != "" && t.Binding != "" && t.IssuedAt != 0 && t.Expires != 0 && t.VisitEnds != 0
}

// Code is a provider's answer to a challenge as ferund signed it
// (a JWT, alg EdDSA, typ TypCode).
type Code struct {
	Issuer    string `json:"iss"`
	Challenge string `json:"cid"`
	Env       string `json:"aud"`
	Host      string `json:"hst"`
	Path      string `json:"pth"`           // the challenge's path
	Binding   string `json:"bnd"`           // the challenge's binding
	Outcome   string `json:"out"`           // OutcomeAccepted | OutcomeRejected
	Visit     string `json:"vid,omitempty"` // with accepted
	IssuedAt  int64  `json:"iat"`
	Expires   int64  `json:"exp"`
}

// complete reports whether every field without omitempty is set, the
// outcome is one of the two and an accepted code names its visit.
func (c *Code) complete() bool {
	if c.Issuer != Issuer || c.Challenge == "" || c.Env == "" || c.Host == "" || c.Path == "" || c.Binding == "" ||
		c.IssuedAt == 0 || c.Expires == 0 {
		return false
	}
	switch c.Outcome {
	case OutcomeAccepted:
		return c.Visit != ""
	case OutcomeRejected:
		return true
	}
	return false
}

// jwtHeader is a JWT's protected header, in the order this package writes
// it.
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// askHeader is an ask's protected header, in the order of its contract
// (ADR 0054): alg, typ, kid.
type askHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// encodeHeader is the encoded header of a JWT of typ under the key kid.
func encodeHeader(typ, kid string) string {
	var b []byte
	if typ == TypAsk {
		b, _ = json.Marshal(askHeader{Alg: "EdDSA", Typ: typ, Kid: kid})
	} else {
		b, _ = json.Marshal(jwtHeader{Alg: "EdDSA", Kid: kid, Typ: typ})
	}
	return b64.EncodeToString(b)
}

// The JWTs: base64url (no padding) of the header
// {"alg":"EdDSA","kid":"<TokenKID>","typ":"<typ>"}, ".", base64url of the
// claims' JSON, ".", base64url of the Ed25519 signature over the first two
// parts. Verify checks shape and size (MaxJWT), alg, typ, kid, the signature,
// iss and that every field without omitempty is set; never a time.

// SignToken signs t's claims with Issuer as iss; ErrNoSigner without the
// private key, ErrMalformed when a field without omitempty is missing.
func (k *Keys) SignToken(t Token) (string, error) {
	t.Issuer = Issuer
	if !t.complete() {
		return "", fmt.Errorf("visit: a token without its fields: %w", ErrMalformed)
	}
	return k.sign(TypToken, t)
}

// VerifyToken checks a token and returns its claims.
func (k *Keys) VerifyToken(raw string) (Token, error) {
	var t Token
	if err := k.verify(raw, TypToken, &t); err != nil {
		return Token{}, err
	}
	if !t.complete() {
		return Token{}, ErrMalformed
	}
	return t, nil
}

// SignCode signs c's claims with Issuer as iss; ErrNoSigner without the
// private key, ErrMalformed when a field without omitempty is missing or
// the outcome is not one of the two (an accepted code names its visit).
func (k *Keys) SignCode(c Code) (string, error) {
	c.Issuer = Issuer
	if !c.complete() {
		return "", fmt.Errorf("visit: a code without its fields: %w", ErrMalformed)
	}
	return k.sign(TypCode, c)
}

// VerifyCode checks a code and returns its claims.
func (k *Keys) VerifyCode(raw string) (Code, error) {
	var c Code
	if err := k.verify(raw, TypCode, &c); err != nil {
		return Code{}, err
	}
	if !c.complete() {
		return Code{}, ErrMalformed
	}
	return c, nil
}

func (k *Keys) sign(typ string, claims any) (string, error) {
	if k.priv == nil {
		return "", ErrNoSigner
	}
	// Without HTML escaping, as SealChallenge: a name of 200 & would
	// otherwise take six bytes each and push the token past its cookie.
	var js bytes.Buffer
	enc := json.NewEncoder(&js)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(claims); err != nil {
		return "", fmt.Errorf("visit: code the claims: %w", err)
	}
	payload := bytes.TrimSuffix(js.Bytes(), []byte("\n"))
	input := encodeHeader(typ, k.tokenKID) + "." + b64.EncodeToString(payload)
	jwt := input + "." + b64.EncodeToString(ed25519.Sign(k.priv, []byte(input)))
	if len(jwt) > MaxJWT {
		return "", fmt.Errorf("visit: the JWT takes %d characters, more than %d: %w", len(jwt), MaxJWT, ErrMalformed)
	}
	return jwt, nil
}

// verify checks raw as a JWT of typ under k's key and decodes its claims
// into v.
func (k *Keys) verify(raw, typ string, v any) error {
	return verifyJWT(raw, typ, map[string]ed25519.PublicKey{k.tokenKID: k.pub}, v)
}

// verifyJWT checks raw as a JWT of typ (one of this package's other types
// is ErrType) signed by one of keys, by kid, and decodes its claims into
// v.
func verifyJWT(raw, typ string, keys map[string]ed25519.PublicKey, v any) error {
	if len(raw) == 0 || len(raw) > MaxJWT {
		return ErrMalformed
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ErrMalformed
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return ErrMalformed
	}
	var h jwtHeader
	if err := json.Unmarshal(hb, &h); err != nil || h.Alg != "EdDSA" {
		return ErrMalformed
	}
	switch h.Typ {
	case typ:
	case TypToken, TypCode, TypAsk:
		return ErrType
	default:
		return ErrMalformed
	}
	pub, ok := keys[h.Kid]
	if !ok {
		return ErrUnknownKey
	}
	// Only the header this package writes: nothing else in it.
	if parts[0] != encodeHeader(typ, h.Kid) {
		return ErrMalformed
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ErrMalformed
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return ErrSignature
	}
	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return ErrMalformed
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return ErrMalformed
	}
	return nil
}
