// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

// The bodies of the domains' REST routes (the shared contract), camelCase
// JSON. Times are RFC 3339 in seconds, UTC; a time that is not there is
// null.

// CreateDomainRequest is POST /api/v1/domains.
type CreateDomainRequest struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

// BindDomainRequest is PUT /api/v1/domains/{id}/environment: the environment's
// UUID or name.
type BindDomainRequest struct {
	Environment string `json:"environment"`
}

// DomainList is GET /api/v1/domains.
type DomainList struct {
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
