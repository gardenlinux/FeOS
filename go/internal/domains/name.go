// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf8"
)

// The bounds of a name (RFC 1035): a label of 1 to 63 bytes, a name of at
// most 253 without the trailing dot.
const (
	maxLabel = 63
	maxName  = 253
)

// NameError is a name that cannot be claimed as it is written: Msg says
// why.
type NameError struct{ Msg string }

func (e *NameError) Error() string { return e.Msg }

func nameError(format string, args ...any) error {
	return &NameError{Msg: fmt.Sprintf(format, args...)}
}

// Normalize returns the name a claim stores (ADR 0021): trimmed,
// lowercased, one trailing dot dropped, and checked to be a host name of
// ASCII letters, digits and hyphens (LDH) of at least two labels, whose
// last label begins with a letter, so that it is neither a wildcard nor
// an IP address. An internationalized name is given as its A-label
// (xn--…); one written in Unicode is refused with that hint.
func Normalize(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimSuffix(n, ".")
	if n == "" {
		return "", nameError("name is required")
	}
	for i := 0; i < len(n); i++ {
		if n[i] >= utf8.RuneSelf {
			return "", nameError("name %q is not ASCII: write an internationalized name as its A-label (xn--…)", name)
		}
	}
	if _, err := netip.ParseAddr(n); err == nil {
		return "", nameError("name %q is an IP address, which cannot be claimed", name)
	}
	if strings.Contains(n, "*") {
		return "", nameError("name %q is a wildcard: claim each name on its own, or *.<name> as a wildcard", name)
	}
	if len(n) > maxName {
		return "", nameError("name %q is longer than %d characters", name, maxName)
	}
	labels := strings.Split(n, ".")
	if len(labels) < 2 {
		return "", nameError("name %q has a single label: a domain has at least two (app.example.com)", name)
	}
	for _, l := range labels {
		if err := checkLabel(l); err != nil {
			return "", nameError("name %q: %v", name, err)
		}
	}
	if last := labels[len(labels)-1]; len(last) < 2 || last[0] < 'a' || last[0] > 'z' {
		return "", nameError("name %q does not end in a top-level domain (a label of at least two characters "+
			"that begins with a letter); an IP address cannot be claimed", name)
	}
	return n, nil
}

// wildcardPrefix is the first label of a wildcard and its dot.
const wildcardPrefix = "*."

// NormalizeClaim returns the name a claim stores (ADR 0045): a name as
// Normalize has it, or a wildcard, *.<name>, whose * is the whole first
// label and whose <name> is a name as Normalize has it. Its second value
// reports whether it is a wildcard.
func NormalizeClaim(name string) (string, bool, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	rest, ok := strings.CutPrefix(n, wildcardPrefix)
	if !ok {
		out, err := Normalize(name)
		return out, false, err
	}
	base, err := Normalize(rest)
	if err != nil {
		var ne *NameError
		if errors.As(err, &ne) {
			return "", false, nameError("wildcard %q: %s", name, ne.Msg)
		}
		return "", false, err
	}
	if len(wildcardPrefix)+len(base) > maxName {
		return "", false, nameError("wildcard %q is longer than %d characters", name, maxName)
	}
	return wildcardPrefix + base, true, nil
}

// IsWildcard reports whether a stored name is a wildcard, *.<name>.
func IsWildcard(name string) bool { return strings.HasPrefix(name, wildcardPrefix) }

// Base is the name a stored name stands on: <name> of a wildcard
// *.<name>, else the name itself. A claim's TXT record and a wildcard's
// delegation stand beside it.
func Base(name string) string { return strings.TrimPrefix(name, wildcardPrefix) }

// checkLabel accepts a lowercase LDH label: 1 to 63 letters, digits and
// hyphens, neither first nor last a hyphen.
func checkLabel(l string) error {
	if l == "" {
		return fmt.Errorf("an empty label")
	}
	if len(l) > maxLabel {
		return fmt.Errorf("label %q is longer than %d characters", l, maxLabel)
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return fmt.Errorf("label %q has %q: only letters, digits and hyphens are allowed", l, string(c))
		}
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return fmt.Errorf("label %q begins or ends with a hyphen", l)
	}
	return nil
}

// Under reports whether name is zone or a name below it; both are
// normalized names.
func Under(name, zone string) bool {
	return zone != "" && (name == zone || strings.HasSuffix(name, "."+zone))
}

// tokenEncoding is lowercase base32 without padding: 20 random bytes are
// 32 characters of [a-z2-7].
var tokenEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewToken makes a claim's token: 32 characters of lowercase base32 from
// 160 random bits.
func NewToken() (string, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("make a token: %w", err)
	}
	return tokenEncoding.EncodeToString(b[:]), nil
}
