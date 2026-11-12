// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// This file verifies ID tokens: JWS compact serialisation (RFC 7515) with
// the keys of a JWK set (RFC 7517), by the standard library alone. Three
// algorithms are accepted, RS256, ES256 and EdDSA; anything else, and
// "none" above all, is refused.

// The JWS algorithms ferund verifies.
const (
	algRS256 = "RS256"
	algES256 = "ES256"
	algEdDSA = "EdDSA"
)

// minRSABits is the smallest RSA key accepted.
const minRSABits = 2048

// errUnknownKey is a token signed with a key id the key set lacks: the
// provider may have rotated its keys.
var errUnknownKey = refused("the token's key is not in the provider's key set")

// jwk is one key of a JWK set, as published.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// verifyKey is a usable public key of the set: its id, the algorithm it
// verifies, and the key.
type verifyKey struct {
	kid string
	alg string
	pub crypto.PublicKey
}

// parseKeySet parses a JWK set. Keys of other types or uses, and keys that
// do not parse, are skipped (a provider may publish keys ferund has no use
// for); a set without any usable key is an error.
func parseKeySet(b []byte) ([]verifyKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(b, &set); err != nil {
		return nil, fmt.Errorf("the key set is not a JWK set: %w", err)
	}
	var keys []verifyKey
	for _, k := range set.Keys {
		if vk, err := parseJWK(k); err == nil {
			keys = append(keys, vk)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("the key set holds no RS256, ES256 or EdDSA signing key")
	}
	return keys, nil
}

// parseJWK makes a verifyKey of a JWK. The algorithm is the key's own when
// it names one, else the one its type and curve allow.
func parseJWK(k jwk) (verifyKey, error) {
	if k.Use != "" && k.Use != "sig" {
		return verifyKey{}, fmt.Errorf("key %q is not a signing key", k.Kid)
	}
	switch k.Kty {
	case "RSA":
		if k.Alg != "" && k.Alg != algRS256 {
			return verifyKey{}, fmt.Errorf("RSA key %q is for %s", k.Kid, k.Alg)
		}
		n, err := b64(k.N)
		if err != nil {
			return verifyKey{}, err
		}
		e, err := b64(k.E)
		if err != nil {
			return verifyKey{}, err
		}
		if len(e) == 0 || len(e) > 4 {
			return verifyKey{}, fmt.Errorf("RSA key %q: bad exponent", k.Kid)
		}
		exp := 0
		for _, c := range e {
			exp = exp<<8 | int(c)
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
		if pub.N.BitLen() < minRSABits || exp < 3 {
			return verifyKey{}, fmt.Errorf("RSA key %q is too weak", k.Kid)
		}
		return verifyKey{kid: k.Kid, alg: algRS256, pub: pub}, nil
	case "EC":
		if k.Crv != "P-256" || (k.Alg != "" && k.Alg != algES256) {
			return verifyKey{}, fmt.Errorf("EC key %q is not a P-256 key for ES256", k.Kid)
		}
		x, err := b64(k.X)
		if err != nil {
			return verifyKey{}, err
		}
		y, err := b64(k.Y)
		if err != nil {
			return verifyKey{}, err
		}
		if len(x) != 32 || len(y) != 32 {
			return verifyKey{}, fmt.Errorf("EC key %q: bad coordinates", k.Kid)
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			return verifyKey{}, fmt.Errorf("EC key %q: %w", k.Kid, err)
		}
		return verifyKey{kid: k.Kid, alg: algES256, pub: pub}, nil
	case "OKP":
		if k.Crv != "Ed25519" || (k.Alg != "" && k.Alg != algEdDSA && k.Alg != "Ed25519") {
			return verifyKey{}, fmt.Errorf("OKP key %q is not an Ed25519 key for EdDSA", k.Kid)
		}
		x, err := b64(k.X)
		if err != nil {
			return verifyKey{}, err
		}
		if len(x) != ed25519.PublicKeySize {
			return verifyKey{}, fmt.Errorf("Ed25519 key %q: bad length", k.Kid)
		}
		return verifyKey{kid: k.Kid, alg: algEdDSA, pub: ed25519.PublicKey(x)}, nil
	}
	return verifyKey{}, fmt.Errorf("key %q of type %q", k.Kid, k.Kty)
}

// b64 decodes base64url without padding (tolerating padding).
func b64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// jwsHeader is the protected header of a JWS.
type jwsHeader struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid"`
	Crit []string `json:"crit"`
}

// splitJWS parses a compact JWS into its header, payload and signature.
func splitJWS(token string) (h jwsHeader, signingInput string, payload, sig []byte, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return h, "", nil, nil, refused("the token is not a compact JWS")
	}
	hb, err := b64(parts[0])
	if err != nil {
		return h, "", nil, nil, refused("the token's header is not base64url")
	}
	if err := json.Unmarshal(hb, &h); err != nil {
		return h, "", nil, nil, refused("the token's header is not JSON")
	}
	if payload, err = b64(parts[1]); err != nil {
		return h, "", nil, nil, refused("the token's payload is not base64url")
	}
	if sig, err = b64(parts[2]); err != nil {
		return h, "", nil, nil, refused("the token's signature is not base64url")
	}
	return h, parts[0] + "." + parts[1], payload, sig, nil
}

// verifyJWS checks a compact JWS against the keys and returns its payload.
// The header's alg must be RS256, ES256 or EdDSA and match the key's;
// a token with a kid is verified with that key only (errUnknownKey when
// the set lacks it), one without with every key of its algorithm.
func verifyJWS(token string, keys []verifyKey) ([]byte, error) {
	h, input, payload, sig, err := splitJWS(token)
	if err != nil {
		return nil, err
	}
	switch h.Alg {
	case algRS256, algES256, algEdDSA:
	case "", "none":
		return nil, refused("the token is not signed (alg none)")
	default:
		return nil, refusedf("the token's algorithm is not accepted (RS256, ES256 or EdDSA)", "%q", h.Alg)
	}
	if len(h.Crit) > 0 {
		return nil, refusedf("the token names critical header parameters", "%q", h.Crit)
	}
	tried := false
	for _, k := range keys {
		if (h.Kid != "" && k.kid != h.Kid) || k.alg != h.Alg {
			continue
		}
		tried = true
		if verifySignature(k, input, sig) {
			return payload, nil
		}
	}
	if !tried {
		if h.Kid != "" && !hasKid(keys, h.Kid) {
			return nil, errUnknownKey
		}
		return nil, refusedf("no key of the provider verifies the token's algorithm", "%s", h.Alg)
	}
	return nil, refused("the token's signature does not verify")
}

func hasKid(keys []verifyKey, kid string) bool {
	for _, k := range keys {
		if k.kid == kid {
			return true
		}
	}
	return false
}

// verifySignature checks sig over input with k.
func verifySignature(k verifyKey, input string, sig []byte) bool {
	switch pub := k.pub.(type) {
	case *rsa.PublicKey:
		sum := sha256.Sum256([]byte(input))
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) == nil
	case *ecdsa.PublicKey:
		if len(sig) != 64 {
			return false
		}
		sum := sha256.Sum256([]byte(input))
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		return ecdsa.Verify(pub, sum[:], r, s)
	case ed25519.PublicKey:
		return ed25519.Verify(pub, []byte(input), sig)
	}
	return false
}
