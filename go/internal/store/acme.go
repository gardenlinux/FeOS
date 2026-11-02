// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
)

// The domains' certificates (ADR 0021): ferund is the ACME client. These
// are the queries of the tables and columns the ACME side owns
// (domain_certs, acme_accounts, acme_challenges, and domains'
// next_issue_at, issue_backoff_s, issue_error and issue_failing_since),
// and the ingress table's reads of them. Every write that changes what
// the ingress serves notifies ferun_ingress in its transaction: a
// domain's certificate "env:<the domain's environment>", the challenges
// IngressPayloadChallenges.

// IngressPayloadChallenges is the ferun_ingress payload of a change of
// acme_challenges: the table builders read the challenges again.
const IngressPayloadChallenges = "challenges"

// The bounds of an order's backoff after a failure: the first wait, and
// the most it doubles to. A six-day certificate is renewed with three
// days left, so a renewal that fails is tried again at least every six
// hours, a dozen times before it expires; Let's Encrypt allows five
// failed validations of one name an hour.
const (
	ACMEBackoffFirst = time.Hour
	ACMEBackoffMax   = 6 * time.Hour
)

// IngressDomain is a domain the ingress serves with its certificate: the
// environment it is bound to, its name, the chain (DER, leaf first) and
// the sealed key (PKCS #8 DER, sealed with DomainCertAAD).
type IngressDomain struct {
	EnvID     uuid.UUID
	DomainID  uuid.UUID
	Name      string
	Chain     [][]byte
	KeySealed []byte
	NotAfter  time.Time
}

// DomainCertAAD is the additional data a domain's key is sealed with:
// 'domain-cert:' || domain_id.
func DomainCertAAD(domainID uuid.UUID) []byte { return []byte("domain-cert:" + domainID.String()) }

// ACMEAccountAAD is the additional data the ACME account's key is sealed
// with: 'acme-account:' || directory.
func ACMEAccountAAD(directory string) []byte { return []byte("acme-account:" + directory) }

// IngressDomains returns the domains the ingress serves (ADR 0021): state
// served or dns-moved, verified, bound to a public environment of its
// tenant that is not deleting, with a certificate whose not_after is
// ahead; envIDs limits them to those environments (nil: every one;
// empty: none). They are ordered by environment, then by when they were
// bound and by name, so that an environment whose entry cannot take all
// of them keeps the oldest bindings.
func (s *Store) IngressDomains(ctx context.Context, envIDs []uuid.UUID) ([]IngressDomain, error) {
	if envIDs != nil && len(envIDs) == 0 {
		return nil, nil
	}
	rows, err := s.q.Query(ctx, `SELECT d.env_id, d.id, d.name, c.chain, c.key_sealed, c.not_after `+servedDomains+`
		ORDER BY d.env_id, d.bound_at, d.name, d.id`, envIDs)
	if err != nil {
		return nil, fmt.Errorf("read the ingress's domains: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (IngressDomain, error) {
		var d IngressDomain
		err := r.Scan(&d.EnvID, &d.DomainID, &d.Name, &d.Chain, &d.KeySealed, &d.NotAfter)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the ingress's domains: %w", err)
	}
	return out, nil
}

// servedDomains is the FROM and WHERE of the domains the ingress serves,
// $1 the environments they are limited to (IngressDomains).
const servedDomains = `FROM domains d
		JOIN environments e ON e.id = d.env_id AND e.owner = d.owner AND e.public AND NOT e.deleting
		JOIN domain_certs c ON c.domain_id = d.id AND c.not_after > now()
		WHERE d.state IN ('served','dns-moved') AND d.verification = 'verified'
			AND ($1::uuid[] IS NULL OR d.env_id = ANY($1))`

// ServedDomainNames returns the names of the domains the ingress serves
// (IngressDomains) of envIDs (empty: none), by environment, in the order
// they were bound.
func (s *Store) ServedDomainNames(ctx context.Context, envIDs []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := map[uuid.UUID][]string{}
	if len(envIDs) == 0 {
		return out, nil
	}
	rows, err := s.q.Query(ctx, `SELECT d.env_id, d.name `+servedDomains+`
		ORDER BY d.env_id, d.bound_at, d.name, d.id`, envIDs)
	if err != nil {
		return nil, fmt.Errorf("read the served domains: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("read the served domains: %w", err)
		}
		if !slices.Contains(out[id], name) {
			out[id] = append(out[id], name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the served domains: %w", err)
	}
	return out, nil
}

// ACMEChallenge is a row of acme_challenges: an HTTP-01 answer.
type ACMEChallenge struct {
	Token     string
	Host      string
	KeyAuth   string
	DomainID  uuid.UUID
	ExpiresAt time.Time
}

// IngressChallenges returns the pending HTTP-01 answers (expires_at
// ahead), sorted by token, at most limit of them.
func (s *Store) IngressChallenges(ctx context.Context, limit int) ([]ACMEChallenge, error) {
	rows, err := s.q.Query(ctx, `SELECT token, host, key_auth FROM acme_challenges WHERE expires_at > now()
		ORDER BY token LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("read the ingress's challenges: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ACMEChallenge, error) {
		var c ACMEChallenge
		err := r.Scan(&c.Token, &c.Host, &c.KeyAuth)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the ingress's challenges: %w", err)
	}
	return out, nil
}

// RecordIngressChallenges records the challenges ingress name holds: the
// tokens of the whole set its stream sent it last
// (ingresses.challenge_tokens).
func (s *Store) RecordIngressChallenges(ctx context.Context, name string, tokens []string) error {
	_, err := s.q.Exec(ctx, `UPDATE ingresses SET challenge_tokens = $2 WHERE name = $1`, name, nonNil(tokens))
	return err
}

// IngressChallengeHolders reports which ingress instances connected now
// (joined, with a mesh address, not left, seen within live) do not hold
// the challenge token (RecordIngressChallenges), how many are connected,
// and the database's now().
func (s *Store) IngressChallengeHolders(ctx context.Context, token string, live time.Duration) (missing []string,
	connected int, now time.Time, err error) {
	err = s.q.QueryRow(ctx, `SELECT count(*)::int,
			coalesce(array_agg(name ORDER BY name) FILTER (WHERE NOT $1 = ANY(challenge_tokens)), '{}'), now()
		FROM ingresses
		WHERE status = 'joined' AND mesh_address <> '' AND left_at IS NULL AND last_seen > now() - $2::interval`,
		token, interval(live)).Scan(&connected, &missing, &now)
	if err != nil {
		return nil, 0, time.Time{}, fmt.Errorf("read the ingresses' challenges: %w", err)
	}
	return missing, connected, now, nil
}

// ACMEAccount is a row of acme_accounts: the platform's account at one
// directory.
type ACMEAccount struct {
	Directory string
	KeySealed []byte // PKCS #8 DER, sealed with ACMEAccountAAD
	URI       string // the account's URL at the CA; "": not registered yet
	Email     string
	CreatedAt time.Time
}

// GetACMEAccount reads the account of directory; ErrNoRow when there is
// none.
func (s *Store) GetACMEAccount(ctx context.Context, directory string) (ACMEAccount, error) {
	var a ACMEAccount
	err := s.q.QueryRow(ctx, `SELECT directory, key_sealed, uri, email, created_at FROM acme_accounts WHERE directory = $1`,
		directory).Scan(&a.Directory, &a.KeySealed, &a.URI, &a.Email, &a.CreatedAt)
	if err != nil {
		return ACMEAccount{}, noRow(err, ErrNoRow)
	}
	return a, nil
}

// InsertACMEAccount inserts the account of a.Directory with its sealed
// key unless the directory has one, and returns the one that holds it
// then: a's, or the one another instance inserted first. The primary key
// is the lock, so two instances never keep two keys for one directory.
func (s *Store) InsertACMEAccount(ctx context.Context, a ACMEAccount) (ACMEAccount, error) {
	if _, err := s.q.Exec(ctx, `INSERT INTO acme_accounts (directory, key_sealed, email) VALUES ($1, $2, $3)
		ON CONFLICT (directory) DO NOTHING`, a.Directory, a.KeySealed, a.Email); err != nil {
		return ACMEAccount{}, fmt.Errorf("insert the ACME account of %s: %w", a.Directory, err)
	}
	return s.GetACMEAccount(ctx, a.Directory)
}

// SetACMEAccount records the account's URL at the CA and its email.
func (s *Store) SetACMEAccount(ctx context.Context, directory, uri, email string) error {
	tag, err := s.q.Exec(ctx, `UPDATE acme_accounts SET uri = $2, email = $3 WHERE directory = $1`, directory, uri, email)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("the ACME account of %s: %w", directory, ErrNoRow)
	}
	return err
}

// ACMEOrder is a domain an order is for: its ID, name, tenant and
// environment, and whether it has a certificate still valid (a renewal).
type ACMEOrder struct {
	DomainID uuid.UUID
	Name     string
	Owner    string
	EnvID    uuid.UUID
	Renewal  bool
}

// ClaimACMEOrder picks the next domain an order is due for and claims it
// until hold from now (next_issue_at), so that no other run picks it
// while the order runs, nor after a crash before hold has passed; ok
// false when none is due. Its transaction should be fenced by the
// leader.
//
// A domain is due when it is verified, bound to a public environment that
// is not deleting, of a tenant that is not deleting, routed, its TXT
// record not missing (record_missing_since), its next_issue_at passed or
// unset, and either issuing or failed without a valid certificate, or
// with a certificate whose renew_after has passed. New domains come
// before renewals, then the longest due first.
func (s *Store) ClaimACMEOrder(ctx context.Context, hold time.Duration) (ACMEOrder, bool, error) {
	var o ACMEOrder
	var valid bool
	err := s.q.QueryRow(ctx, `SELECT d.id, d.name, d.owner, d.env_id, coalesce(c.not_after > now(), false)
		FROM domains d
		JOIN environments e ON e.id = d.env_id AND e.public AND NOT e.deleting
		JOIN tenants t ON t.id = d.owner AND t.deleting_at IS NULL
		LEFT JOIN domain_certs c ON c.domain_id = d.id
		WHERE d.verification = 'verified' AND d.env_id IS NOT NULL AND d.routed AND d.record_missing_since IS NULL
			AND (d.next_issue_at IS NULL OR d.next_issue_at <= now())
			AND ((d.state IN ('issuing','failed') AND (c.domain_id IS NULL OR c.not_after <= now()))
				OR c.renew_after <= now())
		ORDER BY coalesce(c.not_after > now(), false), coalesce(c.renew_after, d.next_issue_at, d.bound_at), d.id
		LIMIT 1
		FOR UPDATE OF d SKIP LOCKED`).Scan(&o.DomainID, &o.Name, &o.Owner, &o.EnvID, &valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return ACMEOrder{}, false, nil
	}
	if err != nil {
		return ACMEOrder{}, false, fmt.Errorf("pick a domain to order a certificate for: %w", err)
	}
	o.Renewal = valid
	if _, err := s.q.Exec(ctx, `UPDATE domains SET next_issue_at = now() + $2::interval WHERE id = $1`,
		o.DomainID, interval(hold)); err != nil {
		return ACMEOrder{}, false, fmt.Errorf("claim domain %s for an order: %w", o.Name, err)
	}
	return o, true, nil
}

// PutACMEChallenge keeps an HTTP-01 answer for ttl from now and notifies
// the ingress's table.
func (s *Store) PutACMEChallenge(ctx context.Context, c ACMEChallenge, ttl time.Duration) error {
	if _, err := s.q.Exec(ctx, `INSERT INTO acme_challenges (token, host, key_auth, domain_id, expires_at)
		VALUES ($1, $2, $3, $4, now() + $5::interval)
		ON CONFLICT (token) DO UPDATE SET host = EXCLUDED.host, key_auth = EXCLUDED.key_auth,
			domain_id = EXCLUDED.domain_id, expires_at = EXCLUDED.expires_at`,
		c.Token, c.Host, c.KeyAuth, nullUUID(c.DomainID), interval(ttl)); err != nil {
		return insertError("keep the challenge for "+c.Host, err)
	}
	return s.Notify(ctx, db.ChanIngress, IngressPayloadChallenges)
}

// DeleteACMEChallenges deletes the challenges of a domain and notifies
// the ingress's table when there were any.
func (s *Store) DeleteACMEChallenges(ctx context.Context, domainID uuid.UUID) (int64, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM acme_challenges WHERE domain_id = $1`, domainID)
	if err != nil || tag.RowsAffected() == 0 {
		return 0, err
	}
	return tag.RowsAffected(), s.Notify(ctx, db.ChanIngress, IngressPayloadChallenges)
}

// ExpireACMEChallenges deletes the challenges whose expires_at passed and
// notifies the ingress's table when there were any.
func (s *Store) ExpireACMEChallenges(ctx context.Context) (int64, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM acme_challenges WHERE expires_at <= now()`)
	if err != nil || tag.RowsAffected() == 0 {
		return 0, err
	}
	return tag.RowsAffected(), s.Notify(ctx, db.ChanIngress, IngressPayloadChallenges)
}

// ACMEDomain is what an order's end reads of its domain, under its row
// lock (LockACMEDomain).
type ACMEDomain struct {
	ID           uuid.UUID
	Name         string
	Owner        string
	EnvID        uuid.UUID // uuid.Nil: unbound
	Verification string
	State        string
	// EnvDeployment is the bound environment's latest deployment (0: none or
	// unbound), the deployment of its incidents.
	EnvDeployment int
	// Valid: it has a certificate whose not_after is ahead.
	Valid             bool
	IssueBackoffS     int
	IssueError        string
	IssueFailingSince time.Time
	NextIssueAt       time.Time
}

// LockACMEDomain reads a domain and locks its row FOR UPDATE; ErrNoRow
// when it is gone.
func (s *Store) LockACMEDomain(ctx context.Context, id uuid.UUID) (ACMEDomain, error) {
	var d ACMEDomain
	var env *uuid.UUID
	var failing, next *time.Time
	err := s.q.QueryRow(ctx, `SELECT d.id, d.name, d.owner, d.env_id, d.verification, d.state,
			coalesce((SELECT e.deployment FROM environments e WHERE e.id = d.env_id), 0),
			EXISTS (SELECT 1 FROM domain_certs c WHERE c.domain_id = d.id AND c.not_after > now()),
			d.issue_backoff_s, d.issue_error, d.issue_failing_since, d.next_issue_at
		FROM domains d WHERE d.id = $1 FOR UPDATE OF d`, id).Scan(&d.ID, &d.Name, &d.Owner, &env, &d.Verification,
		&d.State, &d.EnvDeployment, &d.Valid, &d.IssueBackoffS, &d.IssueError, &failing, &next)
	if err != nil {
		return ACMEDomain{}, noRow(err, ErrNoRow)
	}
	d.EnvID, d.IssueFailingSince, d.NextIssueAt = uuidOf(env), timeOf(failing), timeOf(next)
	return d, nil
}

// DomainCert is a row of domain_certs: a domain's current certificate.
type DomainCert struct {
	DomainID   uuid.UUID
	Chain      [][]byte // DER, leaf first
	KeySealed  []byte   // PKCS #8 DER, sealed with DomainCertAAD
	NotBefore  time.Time
	NotAfter   time.Time
	RenewAfter time.Time
	IssuedAt   time.Time
	Serial     string // lowercase hex
}

// GetDomainCert reads a domain's certificate; ErrNoRow when it has none.
func (s *Store) GetDomainCert(ctx context.Context, domainID uuid.UUID) (DomainCert, error) {
	var c DomainCert
	err := s.q.QueryRow(ctx, `SELECT domain_id, chain, key_sealed, not_before, not_after, renew_after, issued_at, serial
		FROM domain_certs WHERE domain_id = $1`, domainID).Scan(&c.DomainID, &c.Chain, &c.KeySealed, &c.NotBefore,
		&c.NotAfter, &c.RenewAfter, &c.IssuedAt, &c.Serial)
	if err != nil {
		return DomainCert{}, noRow(err, ErrNoRow)
	}
	return c, nil
}

// ACMEOrderSucceeded keeps the certificate an order obtained for domain d
// (read by LockACMEDomain in the same transaction), replacing the one
// before; clears the domain's failure (issue_error, its backoff,
// issue_failing_since, next_issue_at), deletes its challenges, closes its
// incidents certificate_failing on its environment and notifies the
// ingress's table of its environment.
func (s *Store) ACMEOrderSucceeded(ctx context.Context, d ACMEDomain, c DomainCert, condition string) error {
	if _, err := s.q.Exec(ctx, `INSERT INTO domain_certs (domain_id, chain, key_sealed, not_before, not_after, renew_after, serial)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (domain_id) DO UPDATE SET chain = EXCLUDED.chain, key_sealed = EXCLUDED.key_sealed,
			not_before = EXCLUDED.not_before, not_after = EXCLUDED.not_after, renew_after = EXCLUDED.renew_after,
			issued_at = now(), serial = EXCLUDED.serial`,
		d.ID, c.Chain, c.KeySealed, c.NotBefore, c.NotAfter, c.RenewAfter, c.Serial); err != nil {
		return insertError("keep the certificate of "+d.Name, err)
	}
	if _, err := s.q.Exec(ctx, `UPDATE domains SET issue_error = '', issue_backoff_s = 0, issue_failing_since = NULL,
		next_issue_at = NULL WHERE id = $1`, d.ID); err != nil {
		return fmt.Errorf("clear the failure of %s: %w", d.Name, err)
	}
	if _, err := s.DeleteACMEChallenges(ctx, d.ID); err != nil {
		return fmt.Errorf("delete the challenges of %s: %w", d.Name, err)
	}
	if d.EnvID == uuid.Nil {
		return nil
	}
	if _, err := s.q.Exec(ctx, `UPDATE env_incidents SET closed_at = now()
		WHERE env_id = $1 AND object = $2 AND condition = $3 AND closed_at IS NULL`,
		d.EnvID, DomainObject(d.Name), condition); err != nil {
		return fmt.Errorf("close the incidents of %s: %w", d.Name, err)
	}
	return s.Notify(ctx, db.ChanIngress, IngressEnvPayload(d.EnvID))
}

// ACMEOrderFailed records that an order for domain d (read by
// LockACMEDomain in the same transaction) failed with text: issue_error,
// the backoff (ACMEBackoffFirst after a success, else twice the last one,
// at most ACMEBackoffMax), next_issue_at that backoff or wait from now,
// whichever is later, and issue_failing_since unless it is set; it
// deletes the domain's challenges. It returns the domain as it is then.
func (s *Store) ACMEOrderFailed(ctx context.Context, d ACMEDomain, text string, wait time.Duration) (ACMEDomain, error) {
	backoff := ACMEBackoffFirst
	if d.IssueBackoffS > 0 {
		backoff = min(2*time.Duration(d.IssueBackoffS)*time.Second, ACMEBackoffMax)
	}
	var failing, next time.Time
	err := s.q.QueryRow(ctx, `UPDATE domains SET issue_error = $2, issue_backoff_s = $3,
			next_issue_at = now() + GREATEST($4::interval, $5::interval),
			issue_failing_since = coalesce(issue_failing_since, now())
		WHERE id = $1 RETURNING issue_failing_since, next_issue_at`,
		d.ID, text, int(backoff/time.Second), interval(backoff), interval(wait)).Scan(&failing, &next)
	if err != nil {
		return ACMEDomain{}, fmt.Errorf("record the failure of %s: %w", d.Name, noRow(err, ErrNoRow))
	}
	if _, err := s.DeleteACMEChallenges(ctx, d.ID); err != nil {
		return ACMEDomain{}, fmt.Errorf("delete the challenges of %s: %w", d.Name, err)
	}
	d.IssueError, d.IssueBackoffS, d.IssueFailingSince, d.NextIssueAt = text, int(backoff/time.Second), failing, next
	return d, nil
}

// ACMERenewalFailing is a domain served with a valid certificate whose
// renewals have failed for a while, without an open incident of the
// condition on its environment.
type ACMERenewalFailing struct {
	DomainID      uuid.UUID
	Name          string
	EnvID         uuid.UUID
	EnvDeployment int
	IssueError    string
	FailingSince  time.Time
	NotAfter      time.Time
}

// ACMERenewalsFailing returns the domains served (state served or
// dns-moved, bound, with a certificate whose not_after is ahead) whose
// orders have failed since longer than for, and whose environment has no
// open incident condition of object domain/<name>: those whose incident
// is to be opened. At most limit.
func (s *Store) ACMERenewalsFailing(ctx context.Context, failingFor time.Duration, condition string,
	limit int) ([]ACMERenewalFailing, error) {
	rows, err := s.q.Query(ctx, `SELECT d.id, d.name, d.env_id, e.deployment, d.issue_error, d.issue_failing_since, c.not_after
		FROM domains d
		JOIN environments e ON e.id = d.env_id
		JOIN domain_certs c ON c.domain_id = d.id AND c.not_after > now()
		WHERE d.state IN ('served','dns-moved') AND d.issue_failing_since <= now() - $1::interval
			AND NOT EXISTS (SELECT 1 FROM env_incidents i WHERE i.env_id = d.env_id AND i.object = 'domain/' || d.name
				AND i.condition = $2 AND i.closed_at IS NULL)
		ORDER BY d.issue_failing_since, d.id
		LIMIT $3`, interval(failingFor), condition, limit)
	if err != nil {
		return nil, fmt.Errorf("read the failing renewals: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ACMERenewalFailing, error) {
		var f ACMERenewalFailing
		err := r.Scan(&f.DomainID, &f.Name, &f.EnvID, &f.EnvDeployment, &f.IssueError, &f.FailingSince, &f.NotAfter)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the failing renewals: %w", err)
	}
	return out, nil
}

// DomainObject is the incident object of a domain: "domain/<name>".
func DomainObject(name string) string { return "domain/" + name }
