// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visit

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The ask (ADR 0054): ferund's question to a provider of kind tenant at a
// renewal, whether a visit still holds. ferund signs it with the token key
// for the provider's address and the visit, and the provider answers only
// a question ferund signed, against the key ferund publishes at GET
// /api/v1/visit-keys (the JWKS the ingress serves at /.ferun/keys).
const (
	TypAsk = "ferun-ask+jwt"
	// AskLifetime is an ask's exp − iat as ferund signs it.
	AskLifetime = 60 * time.Second
	// AskMaxLifetime is the longest exp − iat a verifier takes.
	AskMaxLifetime = 300 * time.Second
)

// ErrClaims is an ask that verifies but is not this question: another
// issuer, another provider's address, another visit, or outside its time.
var ErrClaims = errors.New("a question for another provider, visit or time")

// Ask is an ask's claims (a JWT, alg EdDSA, typ TypAsk), in the order of
// its contract.
type Ask struct {
	Issuer   string `json:"iss"` // Issuer
	Audience string `json:"aud"` // the provider's URL exactly as stored at ferund
	Visit    string `json:"vid"` // the visit asked about
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"` // IssuedAt + AskLifetime
	ID       string `json:"jti"` // 16 random bytes, base64url: 22 characters
}

func (a *Ask) complete() bool {
	return a.Issuer == Issuer && a.Audience != "" && a.Visit != "" && a.IssuedAt != 0 && a.Expires != 0 && a.ID != ""
}

// SignAsk signs the question about visit for the provider at provider
// (its URL exactly as stored), issued at now and living AskLifetime, with
// a random jti. ErrNoSigner without the private key, ErrMalformed without
// a provider or a visit.
func (k *Keys) SignAsk(provider, visit string, now time.Time) (string, error) {
	jti, err := randomString(16)
	if err != nil {
		return "", fmt.Errorf("visit: the ask's ID: %w", err)
	}
	return k.signAsk(provider, visit, now, jti)
}

// signAsk is SignAsk with its jti.
func (k *Keys) signAsk(provider, visit string, now time.Time, jti string) (string, error) {
	iat := now.Unix()
	a := Ask{Issuer: Issuer, Audience: provider, Visit: visit, IssuedAt: iat, Expires: iat + int64(AskLifetime/time.Second), ID: jti}
	if !a.complete() {
		return "", fmt.Errorf("visit: an ask without its fields: %w", ErrMalformed)
	}
	return k.sign(TypAsk, a)
}

// VerifyAsk is a provider's check of an ask, as FeCode makes it: raw at
// most MaxJWT, header alg EdDSA and typ TypAsk with a kid of keys (ParseJWKS
// of ferund's GET /api/v1/visit-keys), the signature, iss Issuer, aud
// exactly provider (the provider's own URL), vid exactly visit, exp > now
// − Leeway, iat < now + Leeway and exp − iat at most AskMaxLifetime. A
// replay within its minute is the same question again: no jti is kept.
// ErrMalformed, ErrType, ErrUnknownKey, ErrSignature, ErrClaims.
func VerifyAsk(raw string, keys map[string]ed25519.PublicKey, provider, visit string, now time.Time) (Ask, error) {
	var a Ask
	if err := verifyJWT(raw, TypAsk, keys, &a); err != nil {
		return Ask{}, err
	}
	if !a.complete() {
		return Ask{}, ErrMalformed
	}
	leeway := int64(Leeway / time.Second)
	if a.Audience != provider || a.Visit != visit || a.Expires <= now.Unix()-leeway || a.IssuedAt >= now.Unix()+leeway ||
		a.Expires-a.IssuedAt > int64(AskMaxLifetime/time.Second) {
		return Ask{}, ErrClaims
	}
	return a, nil
}

// ParseJWKS reads a JWKS as JWKS writes it (ferund's GET
// /api/v1/visit-keys, the ingress's /.ferun/keys) into its keys by kid:
// every key of kty OKP, crv Ed25519, alg EdDSA (or none) and use sig (or
// none), whose kid is its key's ID; any other key is ErrMalformed, and so
// is a set without keys.
func ParseJWKS(raw []byte) (map[string]ed25519.PublicKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil || len(set.Keys) == 0 {
		return nil, ErrMalformed
	}
	keys := make(map[string]ed25519.PublicKey, len(set.Keys))
	for _, j := range set.Keys {
		x, err := b64.DecodeString(j.X)
		if err != nil || j.Kty != "OKP" || j.Crv != "Ed25519" || (j.Alg != "" && j.Alg != "EdDSA") ||
			(j.Use != "" && j.Use != "sig") || len(x) != ed25519.PublicKeySize {
			return nil, ErrMalformed
		}
		sum := sha256.Sum256(x)
		if j.Kid != b64.EncodeToString(sum[:keyIDBytes]) {
			return nil, ErrMalformed
		}
		keys[j.Kid] = ed25519.PublicKey(x)
	}
	return keys, nil
}
