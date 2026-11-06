// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file is the typed access to the domains (migration 0012, ADR
// 0021): a tenant's claims on names, for internal/domains.
//
// The writes the ingress's table follows send ferun_ingress themselves:
// UpdateDomain "env:<id>" of the environment before and after when the
// binding or the state changed, DeleteDomain that of the bound
// environment.

// The verifications of a domain (domains.verification).
const (
	VerificationUnverified = "unverified"
	VerificationVerified   = "verified"
	VerificationLapsed     = "lapsed"
	VerificationSuperseded = "superseded"
)

// DomainFound is what a domain's last answer showed (domains.found): the
// TXT strings at its challenge name, its CNAME chain and its addresses.
type DomainFound struct {
	TXT       []string `json:"txt"`
	CNAME     []string `json:"cname"`
	Addresses []string `json:"addresses"`
}

// Domain is a row of domains. Zero times and uuid.Nil are NULL.
type Domain struct {
	ID        uuid.UUID
	Owner     string // the tenant
	Name      string // lowercase A-labels, no trailing dot
	Labels    map[string]string
	Token     string // the TXT record's value
	CreatedAt time.Time

	// EnvID is the environment it is bound to, uuid.Nil while unbound;
	// BoundAt the last bind or move.
	EnvID   uuid.UUID
	BoundAt time.Time

	Verification string // Verification*
	VerifiedAt   time.Time
	SupersededBy uuid.UUID

	NextCheckAt       time.Time
	CheckBackoffS     int
	CheckedAt         time.Time
	CheckError        string
	CheckFailingSince time.Time
	// (The column apex is read and written by nothing: ferund does not
	// look up a name's zone.)
	Found                             DomainFound
	Routed, WasRouted                 bool
	RecordMissingSince, DNSMovedSince time.Time

	State, Reason string
}

// domainColumns are the columns of a Domain, each prefixed with p and a
// dot.
func domainColumns(p string) string {
	return prefixed(p, []string{"id", "owner", "name", "labels", "token", "created_at", "env_id", "bound_at",
		"verification", "verified_at", "superseded_by", "next_check_at", "check_backoff_s", "checked_at", "check_error",
		"check_failing_since", "found", "routed", "was_routed", "record_missing_since", "dns_moved_since",
		"state", "reason"})
}

var domainCols = domainColumns("")

// domainScan holds the scan targets of a Domain's nullable columns.
type domainScan struct {
	env, supersededBy                          *uuid.UUID
	bound, verified, checked, failing, missing *time.Time
	moved                                      *time.Time
	found                                      []byte
}

func (d *Domain) targets(sc *domainScan) []any {
	return []any{&d.ID, &d.Owner, &d.Name, &d.Labels, &d.Token, &d.CreatedAt, &sc.env, &sc.bound,
		&d.Verification, &sc.verified, &sc.supersededBy, &d.NextCheckAt, &d.CheckBackoffS, &sc.checked, &d.CheckError,
		&sc.failing, &sc.found, &d.Routed, &d.WasRouted, &sc.missing, &sc.moved,
		&d.State, &d.Reason}
}

func (d *Domain) finish(sc *domainScan) error {
	d.Labels = stringMap(d.Labels)
	d.EnvID, d.SupersededBy = uuidOf(sc.env), uuidOf(sc.supersededBy)
	d.BoundAt, d.VerifiedAt, d.CheckedAt = timeOf(sc.bound), timeOf(sc.verified), timeOf(sc.checked)
	d.CheckFailingSince, d.RecordMissingSince = timeOf(sc.failing), timeOf(sc.missing)
	d.DNSMovedSince = timeOf(sc.moved)
	d.Found = DomainFound{}
	if len(sc.found) > 0 {
		if err := json.Unmarshal(sc.found, &d.Found); err != nil {
			return fmt.Errorf("domain %s: found: %w", d.ID, err)
		}
	}
	d.Found = d.Found.nonNil()
	return nil
}

// nonNil returns f with empty lists for nil ones.
func (f DomainFound) nonNil() DomainFound {
	return DomainFound{TXT: nonNil(f.TXT), CNAME: nonNil(f.CNAME), Addresses: nonNil(f.Addresses)}
}

// Equal reports whether f and g show the same.
func (f DomainFound) Equal(g DomainFound) bool {
	return slices.Equal(f.TXT, g.TXT) && slices.Equal(f.CNAME, g.CNAME) && slices.Equal(f.Addresses, g.Addresses)
}

func scanDomain(row pgx.Row) (Domain, error) {
	var d Domain
	var sc domainScan
	if err := row.Scan(d.targets(&sc)...); err != nil {
		return Domain{}, err
	}
	return d, d.finish(&sc)
}

func collectDomains(rows pgx.Rows, err error) ([]Domain, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Domain, error) { return scanDomain(r) })
}

// DomainView is a domain with what its state is derived from beyond its
// own row: its environment's publicity, deletion, name and latest
// deployment, its certificate and orders, and the database's now() of the
// read.
type DomainView struct {
	Domain
	// The bound environment; zero while unbound.
	EnvName                string
	EnvPublic, EnvDeleting bool
	EnvDeployment          int
	Issuance               DomainIssuance
	Now                    time.Time
}

// DomainIssuance is what ACME keeps of a domain (migration 0015): its
// certificate's times and serial, zero while it has none, and its
// orders' last failure and schedule.
type DomainIssuance struct {
	CertNotBefore, CertNotAfter, CertRenewAfter, CertIssuedAt time.Time
	CertSerial                                                string
	// IssueError is the last order's failure, "" after a success.
	IssueError                     string
	IssueFailingSince, NextIssueAt time.Time
}

// CertValid reports whether the domain has a certificate whose not_after
// is after now.
func (i DomainIssuance) CertValid(now time.Time) bool {
	return !i.CertNotAfter.IsZero() && i.CertNotAfter.After(now)
}

const domainViewFrom = ` FROM domains d
	LEFT JOIN environments e ON e.id = d.env_id
	LEFT JOIN domain_certs c ON c.domain_id = d.id`

var domainViewCols = domainColumns("d") + `, coalesce(e.name, ''), coalesce(e.public, false), coalesce(e.deleting, false),
	coalesce(e.deployment, 0), ` + issuanceCols + `, now()`

// issuanceCols are a DomainIssuance's columns of domains d and
// domain_certs c.
const issuanceCols = `c.not_before, c.not_after, c.renew_after, c.issued_at, coalesce(c.serial, ''),
	d.issue_error, d.issue_failing_since, d.next_issue_at`

// issuanceScan holds the scan targets of a DomainIssuance.
type issuanceScan struct{ notBefore, notAfter, renewAfter, issuedAt, failing, next *time.Time }

func (i *DomainIssuance) targets(sc *issuanceScan) []any {
	return []any{&sc.notBefore, &sc.notAfter, &sc.renewAfter, &sc.issuedAt, &i.CertSerial, &i.IssueError, &sc.failing,
		&sc.next}
}

func (i *DomainIssuance) finish(sc *issuanceScan) {
	i.CertNotBefore, i.CertNotAfter, i.CertRenewAfter = timeOf(sc.notBefore), timeOf(sc.notAfter), timeOf(sc.renewAfter)
	i.CertIssuedAt, i.IssueFailingSince, i.NextIssueAt = timeOf(sc.issuedAt), timeOf(sc.failing), timeOf(sc.next)
}

func scanDomainView(row pgx.Row) (DomainView, error) {
	var v DomainView
	var sc domainScan
	var isc issuanceScan
	targets := append(v.Domain.targets(&sc), &v.EnvName, &v.EnvPublic, &v.EnvDeleting, &v.EnvDeployment)
	targets = append(targets, v.Issuance.targets(&isc)...)
	if err := row.Scan(append(targets, &v.Now)...); err != nil {
		return DomainView{}, err
	}
	v.Issuance.finish(&isc)
	return v, v.Domain.finish(&sc)
}

// GetDomainIssuance reads what ACME keeps of a domain (zero for a domain
// that is gone).
func (s *Store) GetDomainIssuance(ctx context.Context, id uuid.UUID) (DomainIssuance, error) {
	var i DomainIssuance
	var sc issuanceScan
	err := s.q.QueryRow(ctx, "SELECT "+issuanceCols+` FROM domains d LEFT JOIN domain_certs c ON c.domain_id = d.id
		WHERE d.id = $1`, id).Scan(i.targets(&sc)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return DomainIssuance{}, nil
	}
	if err != nil {
		return DomainIssuance{}, fmt.Errorf("read the certificate of domain %s: %w", id, err)
	}
	i.finish(&sc)
	return i, nil
}

func collectDomainViews(rows pgx.Rows, err error) ([]DomainView, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DomainView, error) { return scanDomainView(r) })
}

// NewDomain is what a claim's create sets; the other columns take their
// defaults (unverified, checked at once).
type NewDomain struct {
	// MadeBy is the principal that claims it (ADR 0031).
	MadeBy        string
	ID            uuid.UUID
	Owner         string
	Name          string
	Labels        map[string]string
	Token         string
	State, Reason string
}

// InsertDomain inserts a claim. An unknown owner is ErrNoRow, a taken ID
// ErrExists.
func (s *Store) InsertDomain(ctx context.Context, n NewDomain) (Domain, error) {
	labels, err := jsonText(stringMap(n.Labels))
	if err != nil {
		return Domain{}, fmt.Errorf("insert domain %s: %w", n.Name, err)
	}
	found, err := jsonText(DomainFound{}.nonNil())
	if err != nil {
		return Domain{}, fmt.Errorf("insert domain %s: %w", n.Name, err)
	}
	d, err := scanDomain(s.q.QueryRow(ctx, `INSERT INTO domains (id, owner, name, labels, token, found, state, reason,
			made_by)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6::jsonb, $7, $8, $9) RETURNING `+domainCols,
		n.ID, n.Owner, n.Name, labels, n.Token, found, n.State, n.Reason, n.MadeBy))
	if err != nil {
		return Domain{}, insertError("insert domain "+n.Name, err)
	}
	return d, nil
}

// GetDomain reads a domain; ErrNoRow if there is none.
func (s *Store) GetDomain(ctx context.Context, id uuid.UUID) (Domain, error) {
	d, err := scanDomain(s.q.QueryRow(ctx, "SELECT "+domainCols+" FROM domains WHERE id = $1", id))
	if err != nil {
		return Domain{}, noRow(err, ErrNoRow)
	}
	return d, nil
}

// LockDomainView reads a domain with its view and locks the domain's row
// with lock (the environment's and the certificate's rows are not
// locked); ErrNoRow if there is none.
func (s *Store) LockDomainView(ctx context.Context, id uuid.UUID, lock RowLock) (DomainView, error) {
	q := "SELECT " + domainViewCols + domainViewFrom + " WHERE d.id = $1"
	if lock != NoLock {
		q += " " + string(lock) + " OF d"
	}
	v, err := scanDomainView(s.q.QueryRow(ctx, q, id))
	if err != nil {
		return DomainView{}, noRow(err, ErrNoRow)
	}
	return v, nil
}

// ListDomainViews returns the domains of owner bound to envID (any for
// uuid.Nil) that carry every label of labels, oldest first.
func (s *Store) ListDomainViews(ctx context.Context, owner string, envID uuid.UUID, labels map[string]string) ([]DomainView, error) {
	want, err := jsonText(stringMap(labels))
	if err != nil {
		return nil, err
	}
	return collectDomainViews(s.q.Query(ctx, "SELECT "+domainViewCols+domainViewFrom+`
		WHERE d.owner = $1 AND ($2::uuid IS NULL OR d.env_id = $2) AND d.labels @> $3::jsonb
		ORDER BY d.created_at, d.id`, owner, nullUUID(envID), want))
}

// DomainViewsToSettle returns the domains whose state may have changed
// without a write of their own: the verified ones, whose environment's
// publicity and deletion and whose graces' deadlines change it.
func (s *Store) DomainViewsToSettle(ctx context.Context) ([]DomainView, error) {
	return collectDomainViews(s.q.Query(ctx, "SELECT "+domainViewCols+domainViewFrom+`
		WHERE d.verification = 'verified' ORDER BY d.id`))
}

// LockDomainsOfName reads every claim of name and locks the rows FOR
// UPDATE, oldest first: the order every writer of several claims of one
// name locks them in.
func (s *Store) LockDomainsOfName(ctx context.Context, name string) ([]Domain, error) {
	return collectDomains(s.q.Query(ctx, "SELECT "+domainCols+` FROM domains WHERE name = $1
		ORDER BY created_at, id FOR UPDATE`, name))
}

// LockDomainsOfEnv locks the rows of the domains bound to the environment
// FOR UPDATE, oldest first (as LockDomainsOfName), and returns their IDs.
func (s *Store) LockDomainsOfEnv(ctx context.Context, envID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, `SELECT id FROM domains WHERE env_id = $1 ORDER BY created_at, id FOR UPDATE`, envID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// CountUnverifiedDomains counts the claims of owner whose verification is
// not verified.
func (s *Store) CountUnverifiedDomains(ctx context.Context, owner string) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, "SELECT count(*) FROM domains WHERE owner = $1 AND verification <> 'verified'", owner).Scan(&n)
	return n, err
}

// ClaimDueDomains takes up to limit domains whose check is due
// (next_check_at <= now(), superseded ones never), the most overdue
// first, skipping rows another transaction holds, and moves their
// next_check_at lease ahead, so that a check that never writes its result
// is due again then. It returns the rows as they are after the move.
func (s *Store) ClaimDueDomains(ctx context.Context, limit int, lease time.Duration) ([]Domain, error) {
	return collectDomains(s.q.Query(ctx, `WITH due AS (
			SELECT id, next_check_at FROM domains
			WHERE next_check_at <= now() AND verification <> 'superseded'
			ORDER BY next_check_at, id LIMIT $1 FOR UPDATE SKIP LOCKED)
		UPDATE domains d SET next_check_at = now() + $2::interval FROM due WHERE d.id = due.id
		RETURNING `+domainColumns("d"), limit, interval(lease)))
}

// UpdateDomain writes the columns the core owns in which after differs
// from before, the row as the caller read it under its lock: env_id,
// bound_at, verification, verified_at, superseded_by, the checks'
// columns, the graces, state and reason. id, owner, name, labels, token,
// created_at are never written. When
// nothing differs, before is returned unwritten. A change of the binding
// or the state sends ferun_ingress "env:<id>" for the environment before
// and the one after.
func (s *Store) UpdateDomain(ctx context.Context, before, after Domain) (Domain, error) {
	what := "domain " + before.ID.String()
	if err := fixed(what, map[string]bool{"id": before.ID != after.ID, "owner": before.Owner != after.Owner,
		"name": before.Name != after.Name, "labels": !maps.Equal(before.Labels, after.Labels),
		"token": before.Token != after.Token, "created_at": !before.CreatedAt.Equal(after.CreatedAt)}); err != nil {
		return Domain{}, err
	}
	u := newUpdates(before.ID)
	u.set("env_id", before.EnvID != after.EnvID, nullUUID(after.EnvID))
	u.set("bound_at", !before.BoundAt.Equal(after.BoundAt), nullTime(after.BoundAt))
	u.set("verification", before.Verification != after.Verification, after.Verification)
	u.set("verified_at", !before.VerifiedAt.Equal(after.VerifiedAt), nullTime(after.VerifiedAt))
	u.set("superseded_by", before.SupersededBy != after.SupersededBy, nullUUID(after.SupersededBy))
	u.set("next_check_at", !before.NextCheckAt.Equal(after.NextCheckAt), after.NextCheckAt)
	u.set("check_backoff_s", before.CheckBackoffS != after.CheckBackoffS, after.CheckBackoffS)
	u.set("checked_at", !before.CheckedAt.Equal(after.CheckedAt), nullTime(after.CheckedAt))
	u.set("check_error", before.CheckError != after.CheckError, after.CheckError)
	u.set("check_failing_since", !before.CheckFailingSince.Equal(after.CheckFailingSince), nullTime(after.CheckFailingSince))
	if err := u.setJSON("found", !before.Found.Equal(after.Found), after.Found.nonNil()); err != nil {
		return Domain{}, err
	}
	u.set("routed", before.Routed != after.Routed, after.Routed)
	u.set("was_routed", before.WasRouted != after.WasRouted, after.WasRouted)
	u.set("record_missing_since", !before.RecordMissingSince.Equal(after.RecordMissingSince), nullTime(after.RecordMissingSince))
	u.set("dns_moved_since", !before.DNSMovedSince.Equal(after.DNSMovedSince), nullTime(after.DNSMovedSince))
	u.set("state", before.State != after.State, after.State)
	u.set("reason", before.Reason != after.Reason, after.Reason)
	if u.empty() {
		return before, nil
	}
	d, err := scanDomain(s.q.QueryRow(ctx, "UPDATE domains SET "+u.clause(false)+" WHERE id = $1 RETURNING "+domainCols,
		u.args...))
	if err != nil {
		return Domain{}, fmt.Errorf("update %s: %w", what, noRow(err, ErrNoRow))
	}
	if before.EnvID != after.EnvID || before.State != after.State {
		if err := s.notifyIngressEnvs(ctx, nonNilUUIDs(before.EnvID, after.EnvID)); err != nil {
			return Domain{}, fmt.Errorf("update %s: %w", what, err)
		}
	}
	return d, nil
}

// nonNilUUIDs returns the distinct IDs of ids that are not uuid.Nil.
func nonNilUUIDs(ids ...uuid.UUID) []uuid.UUID {
	var out []uuid.UUID
	for _, id := range ids {
		if id != uuid.Nil && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// DeleteDomain deletes a claim. It reports whether a row was there, and
// sends ferun_ingress "env:<id>" of the environment it was bound to.
func (s *Store) DeleteDomain(ctx context.Context, id uuid.UUID) (bool, error) {
	var env *uuid.UUID
	err := s.q.QueryRow(ctx, "DELETE FROM domains WHERE id = $1 RETURNING env_id", id).Scan(&env)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("delete domain %s: %w", id, err)
	}
	if err := s.notifyIngressEnvs(ctx, nonNilUUIDs(uuidOf(env))); err != nil {
		return false, fmt.Errorf("delete domain %s: %w", id, err)
	}
	return true, nil
}

// OpenIncidentOf returns the open incident of condition of the
// environment's object, of any deployment, and whether there is one.
func (s *Store) OpenIncidentOf(ctx context.Context, envID uuid.UUID, object, condition string) (EnvIncident, bool, error) {
	i, err := scanIncident(s.q.QueryRow(ctx, "SELECT "+incidentCols+` FROM env_incidents
		WHERE env_id = $1 AND object = $2 AND condition = $3 AND closed_at IS NULL ORDER BY id DESC LIMIT 1`,
		envID, object, condition))
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvIncident{}, false, nil
	}
	if err != nil {
		return EnvIncident{}, false, err
	}
	return i, true, nil
}
