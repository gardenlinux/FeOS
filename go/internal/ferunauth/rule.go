// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/gardenlinux/feos/go/internal/oidc"
)

// rule is a provider of kind oidc's rule over the ID token's claims, as
// ferund keeps it: exactly one of its fields is set.
type rule struct {
	Anyone  bool     `json:"anyone,omitempty"`
	Emails  []string `json:"emails,omitempty"`
	Domains []string `json:"domains,omitempty"`
	Groups  []string `json:"groups,omitempty"`
}

// visitor is what an issuer said of who signed in.
type visitor struct {
	Sub, Email, Name, Session string
	// EmailVerified is the issuer's email_verified for Email; missing
	// counts as false.
	EmailVerified bool
	Groups        []string
}

// visitorOf reads the visitor from the ID token's and userinfo's claims:
// the person as ferund's own sign-in reads them (oidc.PersonOf), the
// email's email_verified from the claims the email came from, and the
// session (sid) from the ID token.
func visitorOf(id, info oidc.Claims) visitor {
	p := oidc.PersonOf(id, info)
	src := id
	if info.Str("email") != "" {
		src = info
	}
	return visitor{Sub: p.Sub, Email: p.Email, Name: p.Name, Session: id.Str("sid"),
		EmailVerified: isTrue(src["email_verified"]), Groups: p.Groups}
}

// isTrue reads a boolean claim: the JSON true, or the string "true" that
// some issuers send.
func isTrue(raw json.RawMessage) bool {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && s == "true"
}

// admits reports whether the rule admits the visitor. A rule with none or
// more than one of its fields set admits nobody. Emails and domains need a
// verified email; they compare exactly after lower-casing ASCII only, and
// a value with a byte outside ASCII matches nothing (no Unicode folding:
// the Kelvin sign is no "k"). The domain is the part after the email's
// last "@". Groups compare exactly.
func (r rule) admits(v visitor) bool {
	set := 0
	for _, b := range []bool{r.Anyone, len(r.Emails) > 0, len(r.Domains) > 0, len(r.Groups) > 0} {
		if b {
			set++
		}
	}
	if set != 1 {
		return false
	}
	switch {
	case r.Anyone:
		return true
	case len(r.Groups) > 0:
		for _, g := range v.Groups {
			if g != "" && slices.Contains(r.Groups, g) {
				return true
			}
		}
		return false
	}
	if !v.EmailVerified {
		return false
	}
	email, ok := asciiLower(v.Email)
	if !ok {
		return false
	}
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return false
	}
	if len(r.Emails) > 0 {
		return matchesOne(email, r.Emails)
	}
	return matchesOne(email[at+1:], r.Domains)
}

// matchesOne reports whether s, lower-cased already, equals one of list
// after lower-casing.
func matchesOne(s string, list []string) bool {
	for _, x := range list {
		if l, ok := asciiLower(x); ok && l != "" && l == s {
			return true
		}
	}
	return false
}

// asciiLower lower-cases the ASCII letters of s; ok is false when s holds
// a byte outside ASCII.
func asciiLower(s string) (string, bool) {
	b := []byte(s)
	for i, c := range b {
		if c >= 0x80 {
			return "", false
		}
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b), true
}
