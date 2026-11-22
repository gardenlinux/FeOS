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

// ProjectLabel reports whether a project's name is a label of its own in
// its zone, the project's address (ADR 0063): a DNS label as isHostLabel
// has it, without "--" and not ending as a fallback does. A project with a
// dot or a "--" in its name has none, and no natural or chosen labels
// either.
func ProjectLabel(project string) bool {
	return isHostLabel(project) && !strings.Contains(project, "--") && !hasHashSuffix(project)
}

// HostLabels returns the candidates for an environment's default host
// label in its project's zone (ADR 0063). natural is
// "<name>--<project>"; it is usable when naturalOK, that is when it names
// one pair and is a valid label:
//
//   - the label splits at its one "--" into the name and the project: the
//     name has no "--" and does not end in a hyphen;
//   - the project has a label of its own (ProjectLabel), so the project
//     after the "--" is the whole rest;
//   - it is a DNS label of at most 63 characters whose third and fourth
//     characters are not "--" (RFC 5891's reserved R-LDH form): an
//     environment whose name has two characters gets its fallback.
//
// So no two (name, project) pairs share a usable natural label, none is a
// project's label (which has no "--") and none another environment's
// fallback (its project would end as a fallback does). fallback is at
// most 46 characters of the name (prefixed "e-" when the name itself is
// R-LDH, trailing hyphens trimmed), "-" and the first 16 hex characters of
// the SHA-256 of the environment's UUID as text, which is a valid label and
// never R-LDH for any valid environment name. It never panics, whatever its
// input.
func HostLabels(name, project string, id uuid.UUID) (natural string, naturalOK bool, fallback string) {
	natural = name + "--" + project
	n, t, ok := NaturalPair(natural)
	naturalOK = ok && n == name && t == project && ProjectLabel(project) && isHostLabel(natural)
	p := name
	if isRLDH(p) {
		p = "e-" + p
	}
	p = strings.TrimRight(p[:min(fallbackStem, len(p))], "-")
	sum := sha256.Sum256([]byte(id.String()))
	fallback = p + "-" + hex.EncodeToString(sum[:])[:hashLen]
	return natural, naturalOK, fallback
}

// GivenHostLabel reports whether label is one the environment id of
// project holds by its name or its project, the label it may keep when its
// project moves zones: a fallback with id's hash, the usable natural label
// of a name it has or had before a rename (ADR 0037), or the project's
// label.
func GivenHostLabel(label, project string, id uuid.UUID) bool {
	sum := sha256.Sum256([]byte(id.String()))
	if strings.HasSuffix(label, "-"+hex.EncodeToString(sum[:])[:hashLen]) && isHostLabel(label) {
		return true
	}
	if label == project {
		return ProjectLabel(project)
	}
	name, t, ok := NaturalPair(label)
	if !ok || t != project {
		return false
	}
	natural, naturalOK, _ := HostLabels(name, project, id)
	return naturalOK && natural == label
}

// ChosenHostLabel reports whether s may be a default host label that
// project chose for one of its environments (PUT …/public with a
// hostLabel, ADR 0063): "<choice>--<project>", a DNS label as isHostLabel
// has it, the choice without "--" and project one with a label of its own
// (ProjectLabel). Its form is a natural label's, of project alone; one of
// a name no environment of project has yet leaves that environment its
// fallback.
func ChosenHostLabel(s, project string) bool {
	_, t, ok := NaturalPair(s)
	return ok && t == project && ProjectLabel(project) && isHostLabel(s)
}

// NaturalPair splits a label at its first "--" into the environment's name
// (or a project's choice) and the project ("ironcore--dashboard":
// "ironcore" of "dashboard"); ok is false for a label without "--", with
// nothing on either side of it, or with "--" after it as well. A project
// with a label of its own has no "--" (ProjectLabel), so a label of its
// has exactly one.
func NaturalPair(label string) (name, project string, ok bool) {
	name, project, ok = strings.Cut(label, "--")
	return name, project, ok && name != "" && project != "" && !strings.Contains(project, "--")
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
