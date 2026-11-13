// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package visit is the format of ADR 0054's visits, shared by ferund and
// the ingress: the sealed challenge, the visit's token and the code,
// ferund's signed question to a provider of kind tenant (the ask), the
// cookies' and headers' names, and the JWKS of the token's key. It imports
// nothing but the standard library.
package visit

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	TokenLifetime = 15 * time.Minute
	RenewBefore   = 5 * time.Minute
	// AskEvery is less than TokenLifetime − RenewBefore minus clock skew, so
	// every renewal at RenewBefore asks; it only coalesces replicas and tabs.
	AskEvery          = 8 * time.Minute
	AskTimeout        = 3 * time.Second
	ChallengeLifetime = 10 * time.Minute
	ChallengeLeeway   = 2 * time.Minute // ferund: expired when now > exp + ChallengeLeeway
	CodeLifetime      = 2 * time.Minute
	RedeemWindow      = 60 * time.Second
	Leeway            = 60 * time.Second // the ingress, on a token's and a code's exp
	MaxVisit          = 12 * time.Hour
	MaxPath           = 1024
	MaxSealed         = 4096
	MaxJWT            = 4096
	Issuer            = "ferun"
	TypToken          = "ferun-visit+jwt"
	TypCode           = "ferun-code+jwt"
)

// The cookies the ingress sets on a host (always Secure, HttpOnly,
// SameSite=Lax, Path=/, no Domain: what the __Host- prefix demands).
const (
	CookieVisit     = "__Host-ferun_visit"     // VisitCookie(token, visit secret); Max-Age: until the visit's end
	CookieChallenge = "__Host-ferun_challenge" // the binding secret; Max-Age: 600
)

// The headers the application receives on a request of a visit; the
// ingress and the route door drop every client header that IsVisitorHeader
// names, on every host.
const (
	HeaderSubject = "FeRun-Visitor"
	HeaderEmail   = "FeRun-Visitor-Email" // only where the provider gave one
	HeaderToken   = "FeRun-Visit-Token"   // the token alone, never the cookie's secret
)

// Outcomes of a code.
const (
	OutcomeAccepted = "accepted"
	OutcomeRejected = "rejected"
)

var (
	ErrMalformed  = errors.New("malformed") // not one of ours: wrong shape, size or encoding
	ErrUnknownKey = errors.New("signed or sealed with another key")
	ErrSignature  = errors.New("signature does not verify")
	ErrType       = errors.New("of another type") // a code where a token is wanted, or the other way
	ErrNoSigner   = errors.New("keys without the private key")
)

// b64 is the encoding of everything this package writes: base64url
// without padding, strict, so that every value has one spelling.
var b64 = base64.RawURLEncoding.Strict()

// VisitCookie is the visit cookie's value: token "~" secret, where the
// token's vbnd is BindingHash(secret).
func VisitCookie(token, secret string) string {
	return token + "~" + secret
}

// ParseVisitCookie splits a visit cookie's value at its last "~"; ok is
// false unless both parts are there, the token at most MaxJWT and the
// secret ValidBindingSecret.
func ParseVisitCookie(v string) (token, secret string, ok bool) {
	i := strings.LastIndexByte(v, '~')
	if i < 0 {
		return "", "", false
	}
	token, secret = v[:i], v[i+1:]
	if token == "" || len(token) > MaxJWT || !ValidBindingSecret(secret) {
		return "", "", false
	}
	return token, secret, true
}

// visitorPrefix is what a header name folded by IsVisitorHeader starts
// with when it is, or could be read as, a visitor header.
const visitorPrefix = "ferun-visit"

// IsVisitorHeader reports whether a request header name is one of the
// visitor headers or could be read as one: case-insensitive, with every
// byte outside [A-Za-z0-9] read as '-' (as CGI-style servers map names),
// every name starting with "ferun-visit" (so "FeRun_Visitor",
// "FeRun.Visitor", "ferun-visitor-email", "FERUN-VISIT-TOKEN" and
// "FeRun-Visits" all are).
func IsVisitorHeader(name string) bool {
	if len(name) < len(visitorPrefix) {
		return false
	}
	for i := 0; i < len(visitorPrefix); i++ {
		c := name[i]
		switch {
		case 'A' <= c && c <= 'Z':
			c += 'a' - 'A'
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		default:
			c = '-'
		}
		if c != visitorPrefix[i] {
			return false
		}
	}
	return true
}

// DropVisitorHeaders deletes every header IsVisitorHeader names from h
// (iterating h's keys, so non-canonical keys go too) and returns how many.
func DropVisitorHeaders(h http.Header) int {
	n := 0
	for k := range h {
		if IsVisitorHeader(k) {
			delete(h, k)
			n++
		}
	}
	return n
}

// NewChallengeID makes a challenge's ID: 16 random bytes, base64url
// without padding (22 characters).
func NewChallengeID() (string, error) {
	return randomString(16)
}

// NewBindingSecret makes the challenge cookie's secret and the visit
// cookie's secret alike: 32 random bytes, base64url without padding (43
// characters).
func NewBindingSecret() (string, error) {
	return randomString(32)
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b64.EncodeToString(b), nil
}

// ValidBindingSecret reports whether s has a binding secret's shape: 43
// characters of the base64url alphabet.
func ValidBindingSecret(s string) bool {
	return len(s) == 43 && isBase64URL(s)
}

// validChallengeID reports whether s has a challenge ID's shape: 22
// characters of the base64url alphabet.
func validChallengeID(s string) bool {
	return len(s) == 22 && isBase64URL(s)
}

func isBase64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// BindingHash is base64url(sha256(secret)) without padding: what a
// challenge, a code and a token carry of a cookie's secret.
func BindingHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return b64.EncodeToString(sum[:])
}

// SafePath returns p when it is a path to send a browser back to on the
// same host: starts with "/", not with "//" or "/\", no control
// characters, at most MaxPath bytes; "/" otherwise. A byte outside ASCII
// is refused as well: the path goes into a Location header as it is, and
// an escaped path and query hold none.
func SafePath(p string) string {
	if len(p) == 0 || len(p) > MaxPath || p[0] != '/' {
		return "/"
	}
	if len(p) > 1 && (p[1] == '/' || p[1] == '\\') {
		return "/"
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x20 || c >= 0x7f {
			return "/"
		}
	}
	return p
}
