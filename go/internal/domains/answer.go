// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/ferun/api"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The bodies of the domains' REST routes are the published module's
// (ADR 0057), under the names this package gives them.
type (
	CreateRequest = api.CreateDomainRequest
	BindRequest   = api.BindDomainRequest
	List          = api.DomainList
	Domain        = api.Domain
	Records       = api.Records
	CNAMERecord   = api.CNAMERecord
	TXTRecord     = api.TXTRecord
	Found         = api.Found
	Warnings      = api.Warnings
	Certificate   = api.Certificate
)

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
