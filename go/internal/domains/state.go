// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/store"
)

// The states of a domain (domains.state, the answers' state). The first
// three are its verification's when it is not verified.
const (
	StateUnverified    = store.VerificationUnverified
	StateLapsed        = store.VerificationLapsed
	StateSuperseded    = store.VerificationSuperseded
	StateUnbound       = "unbound"
	StateUnpublished   = "unpublished"
	StateWaitingForDNS = "waiting-for-dns"
	StateIssuing       = "issuing"
	StateServed        = "served"
	StateDNSMoved      = "dns-moved"
	StateFailed        = "failed"
	StateInactive      = "inactive"
)

// The reasons of the states that wait for something.
const (
	ReasonUnverified    = "waiting for the TXT record with the claim's token"
	ReasonLapsed        = "the TXT record with the claim's token was missing for longer than the grace; put it back to verify the claim again"
	ReasonSuperseded    = "another claim's token took the name; this claim is no longer checked"
	ReasonUnbound       = "not bound to an environment"
	ReasonNotPublic     = "the environment is not public"
	ReasonEnvDeleting   = "the environment is being deleted"
	ReasonInactive      = "the name resolved elsewhere than the edge for longer than the grace; route it to the edge again"
	ReasonWaitingForDNS = "waiting for the name to resolve to the edge: a CNAME to the edge's name, or A and AAAA records of its addresses"
	ReasonIssuing       = "waiting for its certificate"
	ReasonDNSMoved      = "the name resolves elsewhere than the edge; it is served with its certificate until the grace ends, and its certificate is not renewed meanwhile"
)

// The reasons of a wildcard's routing (ADR 0045), which names two
// records: the names below it, and its delegation.
const (
	ReasonWildcardWaitingForDNS        = "waiting for the names below it to resolve to the edge: a wildcard CNAME to the edge's name, or A and AAAA records of its addresses"
	ReasonWildcardWaitingForDelegation = "waiting for its delegation: the CNAME of its _acme-challenge label to its target in the platform's ACME zone"
	ReasonWildcardInactive             = "the names below it resolved elsewhere than the edge for longer than the grace; route them to the edge again"
	ReasonWildcardUndelegated          = "its delegation, the CNAME of its _acme-challenge label to its target, was wrong for longer than the grace; put it back"
	ReasonWildcardDNSMoved             = "the names below it resolve elsewhere than the edge; it is served with its certificate until the grace ends, and its certificate is not renewed meanwhile"
	ReasonWildcardDelegationMoved      = "its delegation, the CNAME of its _acme-challenge label to its target, is wrong; it is served with its certificate until the grace ends, and its certificate is not renewed meanwhile"
)

// Facts are what a domain's state is derived from: its row, its
// environment, the platform's grace and the database's time.
type Facts struct {
	Verification string // store.Verification*
	Bound        bool
	// EnvPublic and EnvDeleting are the bound environment's.
	EnvPublic, EnvDeleting bool
	// DNSMovedSince is the routing grace's start; zero: none runs.
	DNSMovedSince time.Time
	// Grace is --domain-grace.
	Grace time.Duration
	// Now is the database's time.
	Now    time.Time
	Routed bool
	// Wildcard: the domain is a wildcard, whose routing is two records;
	// Delegated whether the last answer showed its delegation right.
	Wildcard, Delegated bool
	// CertValid: it has a certificate whose not_after is after Now.
	CertValid bool
	// IssueError is the last order's failure; "" after a success or
	// before the first order.
	IssueError string
}

// FactsOf are the facts of a domain's view, with the platform's grace.
func FactsOf(v store.DomainView, grace time.Duration) Facts {
	return Facts{Verification: v.Verification, Bound: v.EnvID != uuid.Nil, EnvPublic: v.EnvPublic,
		EnvDeleting: v.EnvDeleting, DNSMovedSince: v.DNSMovedSince, Grace: grace, Now: v.Now, Routed: v.Routed,
		Wildcard: IsWildcard(v.Name), Delegated: v.Delegated,
		CertValid: v.Issuance.CertValid(v.Now), IssueError: v.Issuance.IssueError}
}

// Derive is a domain's state and reason, the first rule that matches:
//
//  1. not verified: its verification (unverified, lapsed, superseded);
//  2. unbound;
//  3. its environment is not public, or is being deleted: unpublished;
//  4. its name has resolved elsewhere for the grace: inactive;
//  5. a routing grace runs and it has a valid certificate: dns-moved,
//     still served;
//  6. not routed: waiting-for-dns (also while a routing grace runs
//     without a valid certificate);
//  7. a valid certificate: served;
//  8. its last order failed: failed, the failure the reason (orders go
//     on after their backoff);
//  9. issuing: its first certificate, or one after the last expired, is
//     on its way.
//
// A wildcard's routing reasons name the record that is wrong: its
// delegation when the last answer did not show it, else the names below
// it.
func Derive(f Facts) (state, reason string) {
	state, reason = derive(f)
	if f.Wildcard {
		reason = wildcardReason(state, reason, f.Delegated)
	}
	return state, reason
}

// wildcardReason is a wildcard's reason of state in place of a name's.
func wildcardReason(state, reason string, delegated bool) string {
	pick := func(names, delegation string) string {
		if delegated {
			return names
		}
		return delegation
	}
	switch {
	case state == StateInactive:
		return pick(ReasonWildcardInactive, ReasonWildcardUndelegated)
	case state == StateDNSMoved:
		return pick(ReasonWildcardDNSMoved, ReasonWildcardDelegationMoved)
	case state == StateWaitingForDNS && reason == ReasonWaitingForDNS:
		return pick(ReasonWildcardWaitingForDNS, ReasonWildcardWaitingForDelegation)
	}
	return reason
}

// derive is Derive's rules for a name.
func derive(f Facts) (state, reason string) {
	switch {
	case f.Verification == store.VerificationUnverified:
		return StateUnverified, ReasonUnverified
	case f.Verification == store.VerificationLapsed:
		return StateLapsed, ReasonLapsed
	case f.Verification != store.VerificationVerified:
		return StateSuperseded, ReasonSuperseded
	case !f.Bound:
		return StateUnbound, ReasonUnbound
	case f.EnvDeleting:
		return StateUnpublished, ReasonEnvDeleting
	case !f.EnvPublic:
		return StateUnpublished, ReasonNotPublic
	case !f.DNSMovedSince.IsZero() && !f.DNSMovedSince.Add(f.Grace).After(f.Now):
		return StateInactive, ReasonInactive
	case !f.DNSMovedSince.IsZero() && f.CertValid:
		return StateDNSMoved, ReasonDNSMoved
	case !f.Routed:
		return StateWaitingForDNS, ReasonWaitingForDNS
	case f.CertValid:
		return StateServed, ""
	case f.IssueError != "":
		return StateFailed, f.IssueError
	}
	return StateIssuing, ReasonIssuing
}

// deactivated reports whether a domain in state was deactivated by a
// grace that ran out.
func deactivated(state string) bool { return state == StateLapsed || state == StateInactive }
