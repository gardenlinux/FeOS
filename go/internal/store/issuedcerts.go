// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// IssuedCert is a row of issued_certs: a pod certificate (ADR 0014).
type IssuedCert struct {
	Serial          string
	PodID           podid.ID
	Incarnation     uuid.UUID
	Node            string
	SPIFFEID        string
	HPKEPublicKey   []byte // the run's key: what a later re-seal for a running pod needs
	BindingsVersion string
	NotAfter        time.Time
	IssuedAt        time.Time
}

const issuedCertCols = "serial, pod_id, incarnation, node, spiffe_id, hpke_public_key, bindings_version, not_after, issued_at"

func scanIssuedCert(row pgx.Row) (IssuedCert, error) {
	var c IssuedCert
	err := row.Scan(&c.Serial, &c.PodID, &c.Incarnation, &c.Node, &c.SPIFFEID, &c.HPKEPublicKey, &c.BindingsVersion,
		&c.NotAfter, &c.IssuedAt)
	return c, err
}

// InsertIssuedCert records a pod certificate (issued_at = now()).
func (s *Store) InsertIssuedCert(ctx context.Context, c IssuedCert) (IssuedCert, error) {
	return scanIssuedCert(s.q.QueryRow(ctx, `INSERT INTO issued_certs (serial, pod_id, incarnation, node, spiffe_id,
			hpke_public_key, bindings_version, not_after)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+issuedCertCols,
		c.Serial, c.PodID, c.Incarnation, c.Node, c.SPIFFEID, c.HPKEPublicKey, c.BindingsVersion, c.NotAfter))
}

// CountIssuedCerts counts the certificates issued for a pod within the
// last window: the rate limit of credential requests.
func (s *Store) CountIssuedCerts(ctx context.Context, podID podid.ID, window time.Duration) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, "SELECT count(*) FROM issued_certs WHERE pod_id = $1 AND issued_at > now() - $2::interval",
		podID, interval(window)).Scan(&n)
	return n, err
}

// LatestIssuedCert returns the newest certificate of a pod; ErrNoRow if
// none.
func (s *Store) LatestIssuedCert(ctx context.Context, podID podid.ID) (IssuedCert, error) {
	c, err := scanIssuedCert(s.q.QueryRow(ctx, "SELECT "+issuedCertCols+` FROM issued_certs WHERE pod_id = $1
		ORDER BY issued_at DESC, serial LIMIT 1`, podID))
	return c, noRow(err, ErrNoRow)
}

// ListIssuedCerts returns a pod's certificates, newest first.
func (s *Store) ListIssuedCerts(ctx context.Context, podID podid.ID) ([]IssuedCert, error) {
	rows, err := s.q.Query(ctx, "SELECT "+issuedCertCols+" FROM issued_certs WHERE pod_id = $1 ORDER BY issued_at DESC, serial", podID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IssuedCert, error) { return scanIssuedCert(r) })
}

// DeleteOldIssuedCerts removes pod certificates that expired more than
// keep ago. It returns how many.
func (s *Store) DeleteOldIssuedCerts(ctx context.Context, keep time.Duration) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM issued_certs WHERE not_after < now() - $1::interval", interval(keep))
	return tag.RowsAffected(), err
}

// LatestIssuedCerts returns the newest certificate of each of the pods
// that has one, by its ID: the identities an environment's pods hold
// (ADR 0036).
func (s *Store) LatestIssuedCerts(ctx context.Context, podIDs []podid.ID) (map[podid.ID]IssuedCert, error) {
	out := map[podid.ID]IssuedCert{}
	if len(podIDs) == 0 {
		return out, nil
	}
	rows, err := s.q.Query(ctx, "SELECT DISTINCT ON (pod_id) "+issuedCertCols+` FROM issued_certs WHERE pod_id = ANY($1)
		ORDER BY pod_id, issued_at DESC, serial`, podIDs)
	if err != nil {
		return nil, err
	}
	certs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (IssuedCert, error) { return scanIssuedCert(r) })
	if err != nil {
		return nil, err
	}
	for _, c := range certs {
		out[c.PodID] = c
	}
	return out, nil
}
