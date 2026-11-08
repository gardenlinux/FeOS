// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// The values of the ingress's tables (migration 0010, ADR 0021).

// The kinds of a join token (join_tokens.kind): a node's or an ingress
// instance's.
const (
	JoinKindNode    = "node"
	JoinKindIngress = "ingress"
)

// The statuses of an ingress instance (ingresses.status). A removed
// ingress keeps its row.
const (
	IngressJoined  = "joined"
	IngressRemoved = "removed"
)

// IngressPeersGroup is the security group whose members are the mesh
// addresses of the ingress instances that are not removed. It has no rules
// of its own: the groups of public environments' pods name it as their
// peer.
const IngressPeersGroup = "ferun-ingress"

// IngressGroupID is the security group of an image service's pod of an
// environment that was ever public: "ing-<sandbox ID>", 15 characters (40
// for a sandbox with a UUID). While the environment is public, its rules
// let IngressPeersGroup in on the routed ports.
func IngressGroupID(id podid.ID) string { return "ing-" + id.String() }

// maxLabel is the longest DNS label; fallbackStem the longest name part of
// a fallback label, which leaves room for "-" and 16 hex characters;
// hashLen the number of hex characters of a fallback label's hash.
const (
	maxLabel     = 63
	fallbackStem = 46
	hashLen      = 16
)

// HostLabels returns the candidates for an environment's default host
// label under the apps zone (D12 of the ingress's design). natural is
// "<name>-<tenant>"; it is usable when naturalOK, that is when it is
// unambiguous and a valid label:
//
//   - the name contains no '-', so the label splits at its first '-';
//   - the tenant contains no '.', so the label holds the tenant as it is,
//     and no "--", so the label has none and is never a chosen one
//     (ChosenHostLabel);
//   - it does not end in "-" and 16 hex characters, the form of every
//     fallback label;
//   - it is a DNS label of at most 63 characters whose third and fourth
//     characters are not "--" (RFC 5891's reserved R-LDH form).
//
// So no two (name, tenant) pairs share a usable natural label, and none is
// another environment's fallback. fallback is at most 46 characters of the
// name (prefixed "e-" when the name itself is R-LDH, trailing hyphens
// trimmed), "-" and the first 16 hex characters of the SHA-256 of the
// environment's UUID as text, which is a valid label and never R-LDH for
// any valid environment name. It never panics, whatever its input.
func HostLabels(name, tenant string, id uuid.UUID) (natural string, naturalOK bool, fallback string) {
	natural = name + "-" + tenant
	naturalOK = !strings.Contains(name, "-") && !strings.Contains(tenant, ".") && !strings.Contains(tenant, "--") &&
		!hasHashSuffix(natural) && isHostLabel(natural)
	p := name
	if isRLDH(p) {
		p = "e-" + p
	}
	p = strings.TrimRight(p[:min(fallbackStem, len(p))], "-")
	sum := sha256.Sum256([]byte(id.String()))
	fallback = p + "-" + hex.EncodeToString(sum[:])[:hashLen]
	return natural, naturalOK, fallback
}

// GivenHostLabel reports whether label is one HostLabels gives the
// environment id of tenant under some name, the one it has or one it had
// before a rename (ADR 0037): a fallback with id's hash, or the usable
// natural label of a name and tenant. A zone move keeps such a label as
// the environment's own.
func GivenHostLabel(label, tenant string, id uuid.UUID) bool {
	sum := sha256.Sum256([]byte(id.String()))
	if strings.HasSuffix(label, "-"+hex.EncodeToString(sum[:])[:hashLen]) && isHostLabel(label) {
		return true
	}
	name, t, ok := NaturalPair(label)
	if !ok || t != tenant {
		return false
	}
	natural, naturalOK, _ := HostLabels(name, tenant, id)
	return naturalOK && natural == label
}

// ChosenHostLabel reports whether s may be a default host label its
// tenant chose (PUT …/public with a hostLabel): a DNS label as
// isHostLabel has it, with "--" in it and not ending as a fallback does.
// A natural label never has "--" and every fallback ends in its hash, so
// a chosen label is never one HostLabels gives.
func ChosenHostLabel(s string) bool {
	return isHostLabel(s) && strings.Contains(s, "--") && !hasHashSuffix(s)
}

// PlainHostLabel reports whether s may be a plain label, one a tenant
// that owns the apps zone chose (--apps-zone-owners, PUT …/public with a
// hostLabel): a DNS label as isHostLabel has it, without "--" and not
// ending as a fallback does. It is never a fallback, but it may read as a
// natural label; NaturalPair names the only pair it can be the natural
// label of, which the caller checks against the tenants.
func PlainHostLabel(s string) bool {
	return isHostLabel(s) && !strings.Contains(s, "--") && !hasHashSuffix(s)
}

// NaturalPair splits a label at its first '-' into the environment's name
// and the tenant whose natural label it would be ("ironcore-dashboard":
// "ironcore" of "dashboard"); ok is false for a label without a '-' or
// with nothing on either side of it. A natural label's name has no '-'
// (HostLabels), so no other pair has it as its natural label.
func NaturalPair(label string) (name, tenant string, ok bool) {
	name, tenant, ok = strings.Cut(label, "-")
	return name, tenant, ok && name != "" && tenant != ""
}

// hasHashSuffix reports whether s ends in "-" and 16 lowercase hex
// characters, as a fallback label does.
func hasHashSuffix(s string) bool {
	if len(s) < hashLen+1 || s[len(s)-hashLen-1] != '-' {
		return false
	}
	for i := len(s) - hashLen; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isHostLabel reports whether s is [a-z0-9]([a-z0-9-]{0,61}[a-z0-9])? and
// not R-LDH: what the host_labels table accepts.
func isHostLabel(s string) bool {
	if len(s) < 1 || len(s) > maxLabel || isRLDH(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// isRLDH reports whether s has "--" as its third and fourth characters.
func isRLDH(s string) bool { return len(s) >= 4 && s[2:4] == "--" }
