// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visit

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// keyIDBytes is how much of a key's hash its ID takes.
const keyIDBytes = 12

// challengeInfo is the challenge seal's label: in its key ID, as HKDF's
// info and as the AAD.
const challengeInfo = "ferun-visit-challenge/1"

// sealVersion is the first byte of a sealed challenge.
const sealVersion = 0x01

// saltBytes is the size of a sealed challenge's salt.
const saltBytes = 16

// Keys are the installation's two keys: the Ed25519 key of tokens and codes
// (its private half only at ferund) and the AES-256 key of challenges.
type Keys struct {
	priv         ed25519.PrivateKey // nil: verify only
	pub          ed25519.PublicKey
	tokenKID     string
	challengeKey []byte
	challengeID  [keyIDBytes]byte
	challengeKID string
}

// NewKeys makes ferund's keys from the cluster secrets: tokenSeed is the
// 32-byte Ed25519 seed (visits.SecretToken, opened), challengeKey the
// 32-byte AES key (visits.SecretChallenge). They can sign.
func NewKeys(tokenSeed, challengeKey []byte) (*Keys, error) {
	if len(tokenSeed) != ed25519.SeedSize {
		return nil, fmt.Errorf("visit: the token seed has %d bytes, not %d: %w", len(tokenSeed), ed25519.SeedSize, ErrMalformed)
	}
	priv := ed25519.NewKeyFromSeed(tokenSeed)
	k, err := newKeys(priv.Public().(ed25519.PublicKey), challengeKey)
	if err != nil {
		return nil, err
	}
	k.priv = priv
	return k, nil
}

// PublicKeys makes the ingress's keys from its table: the token's public
// key and the challenge key with their IDs, which must be the IDs of those
// keys (ErrMalformed otherwise). They verify and seal, and sign nothing.
func PublicKeys(tokenKID string, tokenKey []byte, challengeKID string, challengeKey []byte) (*Keys, error) {
	if len(tokenKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("visit: the token key has %d bytes, not %d: %w", len(tokenKey), ed25519.PublicKeySize, ErrMalformed)
	}
	k, err := newKeys(ed25519.PublicKey(slices.Clone(tokenKey)), challengeKey)
	if err != nil {
		return nil, err
	}
	if k.tokenKID != tokenKID {
		return nil, fmt.Errorf("visit: the token key's ID is %q, not %q: %w", k.tokenKID, tokenKID, ErrMalformed)
	}
	if k.challengeKID != challengeKID {
		return nil, fmt.Errorf("visit: the challenge key's ID is not %q: %w", challengeKID, ErrMalformed)
	}
	return k, nil
}

func newKeys(pub ed25519.PublicKey, challengeKey []byte) (*Keys, error) {
	if len(challengeKey) != 32 {
		return nil, fmt.Errorf("visit: the challenge key has %d bytes, not 32: %w", len(challengeKey), ErrMalformed)
	}
	k := &Keys{pub: pub, challengeKey: slices.Clone(challengeKey)}
	sum := sha256.Sum256(pub)
	k.tokenKID = b64.EncodeToString(sum[:keyIDBytes])
	sum = sha256.Sum256(append([]byte(challengeInfo+"\x00"), challengeKey...))
	copy(k.challengeID[:], sum[:keyIDBytes])
	k.challengeKID = b64.EncodeToString(k.challengeID[:])
	return k, nil
}

// TokenKID is base64url(sha256(public key)[:12]): 16 characters.
func (k *Keys) TokenKID() string { return k.tokenKID }

// TokenKey is the token's Ed25519 public key (a copy).
func (k *Keys) TokenKey() ed25519.PublicKey { return slices.Clone(k.pub) }

// ChallengeKID is base64url(sha256("ferun-visit-challenge/1\x00" || key)[:12]):
// 16 characters.
func (k *Keys) ChallengeKID() string { return k.challengeKID }

// ChallengeKey is the challenge's AES-256 key (a copy).
func (k *Keys) ChallengeKey() []byte { return slices.Clone(k.challengeKey) }

// CanSign reports whether the keys hold the token key's private half.
func (k *Keys) CanSign() bool { return k.priv != nil }

// jwk is the token key as a JSON Web Key, in the order JWKS writes it.
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// JWKS is the token key as a JSON Web Key Set:
// {"keys":[{"kty":"OKP","crv":"Ed25519","x":"<base64url>","kid":"<TokenKID>","alg":"EdDSA","use":"sig"}]}
func (k *Keys) JWKS() []byte {
	b, _ := json.Marshal(struct {
		Keys []jwk `json:"keys"`
	}{[]jwk{{Kty: "OKP", Crv: "Ed25519", X: b64.EncodeToString(k.pub), Kid: k.tokenKID, Alg: "EdDSA", Use: "sig"}}})
	return b // marshalling strings cannot fail
}

// Challenge is what the ingress seals for a navigation without a visit.
type Challenge struct {
	ID       string `json:"id"`   // NewChallengeID: 22 base64url characters
	Env      string `json:"env"`  // the environment's ID
	Host     string `json:"host"` // the canonical host the navigation came to
	Path     string `json:"path"` // escaped path and "?query" to return to; starts with "/"
	Binding  string `json:"bnd"`  // BindingHash of the challenge cookie's secret
	IssuedAt int64  `json:"iat"`  // Unix seconds, the ingress's clock
	Expires  int64  `json:"exp"`  // IssuedAt + ChallengeLifetime
}

// valid reports whether c has every field in its shape.
func (c *Challenge) valid() bool {
	return validChallengeID(c.ID) && c.Env != "" && c.Host != "" && strings.HasPrefix(c.Path, "/") &&
		len(c.Binding) == 43 && isBase64URL(c.Binding) && c.IssuedAt > 0 && c.Expires > 0
}

// challengeAEAD is the GCM of one challenge's key, derived from the
// challenge key and the challenge's salt.
func (k *Keys) challengeAEAD(salt []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, k.challengeKey, salt, challengeInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// zeroNonce is the nonce of every challenge's seal: each has a key of its
// own.
var zeroNonce = make([]byte, 12)

// SealChallenge seals c: base64url without padding of
// 0x01 || challenge key ID's 12 bytes || salt(16 random) || AES-256-GCM(JSON of c)
// under the key HKDF-SHA256(challenge key, salt, "ferun-visit-challenge/1")
// (crypto/hkdf), a nonce of 12 zero bytes and the AAD
// "ferun-visit-challenge/1": a key per challenge, so random nonces never
// repeat under one key however many challenges are made. A challenge
// without every field in its shape, or one whose seal would be longer
// than MaxSealed (a path that JSON escapes to several times its length),
// is ErrMalformed: the ingress seals it again with the path "/".
func (k *Keys) SealChallenge(c Challenge) (string, error) {
	if !c.valid() {
		return "", fmt.Errorf("visit: a challenge without its fields: %w", ErrMalformed)
	}
	var js bytes.Buffer
	enc := json.NewEncoder(&js)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return "", fmt.Errorf("visit: code the challenge: %w", err)
	}
	plain := bytes.TrimSuffix(js.Bytes(), []byte("\n"))
	out := make([]byte, 1+keyIDBytes+saltBytes, 1+keyIDBytes+saltBytes+len(plain)+16)
	out[0] = sealVersion
	copy(out[1:], k.challengeID[:])
	salt := out[1+keyIDBytes:]
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("visit: salt: %w", err)
	}
	aead, err := k.challengeAEAD(salt)
	if err != nil {
		return "", fmt.Errorf("visit: the challenge's key: %w", err)
	}
	out = aead.Seal(out, zeroNonce, plain, []byte(challengeInfo))
	sealed := b64.EncodeToString(out)
	if len(sealed) > MaxSealed {
		return "", fmt.Errorf("visit: the sealed challenge takes %d characters, more than %d: %w", len(sealed), MaxSealed, ErrMalformed)
	}
	return sealed, nil
}

// OpenChallenge opens a sealed challenge. It does not look at Expires: a
// challenge past its end still names its host. ErrMalformed, ErrUnknownKey.
func (k *Keys) OpenChallenge(sealed string) (Challenge, error) {
	if len(sealed) == 0 || len(sealed) > MaxSealed {
		return Challenge{}, ErrMalformed
	}
	raw, err := b64.DecodeString(sealed)
	if err != nil || len(raw) < 1+keyIDBytes+saltBytes+16 || raw[0] != sealVersion {
		return Challenge{}, ErrMalformed
	}
	if subtle.ConstantTimeCompare(raw[1:1+keyIDBytes], k.challengeID[:]) != 1 {
		return Challenge{}, ErrUnknownKey
	}
	salt := raw[1+keyIDBytes : 1+keyIDBytes+saltBytes]
	aead, err := k.challengeAEAD(salt)
	if err != nil {
		return Challenge{}, ErrMalformed
	}
	plain, err := aead.Open(nil, zeroNonce, raw[1+keyIDBytes+saltBytes:], []byte(challengeInfo))
	if err != nil {
		return Challenge{}, ErrMalformed
	}
	var c Challenge
	if err := json.Unmarshal(plain, &c); err != nil || !c.valid() {
		return Challenge{}, ErrMalformed
	}
	return c, nil
}
