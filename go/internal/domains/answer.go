// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/store"
)

// The bodies of the domains' REST routes (the shared contract), camelCase
// JSON. Times are RFC 3339 in seconds, UTC; a time that is not there is
// null.

// CreateRequest is POST /api/v1/domains.
type CreateRequest struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

// BindRequest is PUT /api/v1/domains/{id}/environment: the environment's
// UUID or name.
type BindRequest struct {
	Environment string `json:"environment"`
}

// List is GET /api/v1/domains.
type List struct {
	Domains []Domain `json:"domains"`
}

// Domain is a domain as the answers show it.
type Domain struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Wildcard: the name is *.<name> (ADR 0045), served for every name one
	// label below <name>, with its certificate over DNS-01.
	Wildcard bool              `json:"wildcard"`
	Labels   map[string]string `json:"labels"`
	// Environment is the bound environment's ID; null while unbound.
	Environment  *string `json:"environment"`
	Verification string  `json:"verification"`
	State        string  `json:"state"`
	// Reason says why it failed, or what its state waits for; "" else.
	Reason   string   `json:"reason"`
	Records  Records  `json:"records"`
	Found    Found    `json:"found"`
	Warnings Warnings `json:"warnings"`
	// Certificate is the domain's current certificate; null while it has
	// none.
	Certificate *Certificate `json:"certificate"`
	CreatedAt   string       `json:"createdAt"`
	VerifiedAt  *string      `json:"verifiedAt"`
	BoundAt     *string      `json:"boundAt"`
	CheckedAt   *string      `json:"checkedAt"`
	NextCheckAt string       `json:"nextCheckAt"`
}

// Records are the records the name's owner puts in DNS: the TXT record
// that proves the claim, and the routing one, a CNAME to the edge's name
// or (at a zone's apex, which takes no CNAME, without an ALIAS) A and
// AAAA records of its addresses; a wildcard's at *.<name>. A wildcard
// takes a third, its delegation.
type Records struct {
	TXT       TXTRecord `json:"txt"`
	CNAME     string    `json:"cname"`
	Addresses []string  `json:"addresses"`
	// Apex is always null: ferund does not look up a name's zone, and
	// the owner picks the CNAME or the addresses.
	Apex *bool `json:"apex"`
	// Delegation is a wildcard's CNAME of _acme-challenge.<name> to its
	// target in the platform's ACME zone; null for a name.
	Delegation *CNAMERecord `json:"delegation"`
}

// CNAMERecord is a CNAME: its name and the name it points at.
type CNAMERecord struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TXTRecord is the TXT record of a claim: its name and value.
type TXTRecord struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Found is what the last answer showed: the TXT strings, the CNAME chain
// and the addresses of the name (a wildcard's: of a name below it), and
// a wildcard's delegation, the CNAME chain of _acme-challenge.<name>
// (empty for a name).
type Found struct {
	TXT        []string `json:"txt"`
	CNAME      []string `json:"cname"`
	Addresses  []string `json:"addresses"`
	Delegation []string `json:"delegation"`
}

// Warnings say what is wrong while the domain is still served: since
// when a record answers wrong, when it is deactivated for it, and why the
// last lookup failed.
type Warnings struct {
	RecordMissingSince *string `json:"recordMissingSince"`
	DNSMovedSince      *string `json:"dnsMovedSince"`
	DeactivatesAt      *string `json:"deactivatesAt"`
	// CheckFailing is the last lookup's error; "" when it answered.
	CheckFailing string `json:"checkFailing"`
	// CertificateFailing is the last order's error while the domain has
	// a certificate (its renewal fails); "" else, and while it has none
	// the state is failed with the error as its reason.
	CertificateFailing string `json:"certificateFailing"`
}

// Certificate is the domain's certificate. It is renewed from renewAfter
// on, half its lifetime.
type Certificate struct {
	NotBefore  string `json:"notBefore"`
	NotAfter   string `json:"notAfter"`
	IssuedAt   string `json:"issuedAt"`
	RenewAfter string `json:"renewAfter"`
	Serial     string `json:"serial"`
}

// rfc3339 is a time as the answers write it.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// optTime is a nullable time.
func optTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := rfc3339(t)
	return &s
}

// addrStrings are addresses as text.
func addrStrings(as []netip.Addr) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.String())
	}
	return out
}

// deactivatesAt is the earlier of the graces' ends; zero when none runs.
func deactivatesAt(d store.Domain, grace time.Duration) time.Time {
	var at time.Time
	for _, since := range []time.Time{d.RecordMissingSince, d.DNSMovedSince} {
		if since.IsZero() {
			continue
		}
		if end := since.Add(grace); at.IsZero() || end.Before(at) {
			at = end
		}
	}
	return at
}

// answer is a domain's view as the routes answer it, its state derived
// from the view (the stored one may lag behind an environment's change by
// a sweep), with the edge's addresses.
func (s *Service) answer(v store.DomainView, edge []netip.Addr) Domain {
	state, reason := Derive(FactsOf(v, s.cfg.Grace))
	d := Domain{
		ID: v.ID.String(), Name: v.Name, Wildcard: IsWildcard(v.Name), Labels: v.Labels, Verification: v.Verification,
		State: state, Reason: reason,
		Records: Records{TXT: TXTRecord{Name: s.txtName(v.Name), Value: v.Token}, CNAME: s.cfg.EdgeName,
			Addresses: addrStrings(edge)},
		Found: Found{TXT: nonNilStrings(v.Found.TXT), CNAME: nonNilStrings(v.Found.CNAME),
			Addresses: nonNilStrings(v.Found.Addresses), Delegation: nonNilStrings(v.Found.Delegation)},
		Warnings: Warnings{RecordMissingSince: optTime(v.RecordMissingSince), DNSMovedSince: optTime(v.DNSMovedSince),
			DeactivatesAt: optTime(deactivatesAt(v.Domain, s.cfg.Grace)), CheckFailing: v.CheckError},
		CreatedAt: rfc3339(v.CreatedAt), VerifiedAt: optTime(v.VerifiedAt), BoundAt: optTime(v.BoundAt),
		CheckedAt: optTime(v.CheckedAt), NextCheckAt: rfc3339(v.NextCheckAt),
	}
	if i := v.Issuance; !i.CertNotAfter.IsZero() {
		d.Certificate = &Certificate{NotBefore: rfc3339(i.CertNotBefore), NotAfter: rfc3339(i.CertNotAfter),
			IssuedAt: rfc3339(i.CertIssuedAt), RenewAfter: rfc3339(i.CertRenewAfter), Serial: i.CertSerial}
		d.Warnings.CertificateFailing = i.IssueError
	}
	if d.Wildcard {
		d.Records.Delegation = &CNAMERecord{Name: s.delegationName(v.Name), Value: s.target(v.Token)}
	}
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	if v.EnvID != uuid.Nil {
		id := v.EnvID.String()
		d.Environment = &id
	}
	return d
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
