// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ServiceToken is a row of service_tokens (ADR 0026): a long-lived token
// of one of the platform's services, kind sink an instance of
// ferun-sink. Only the hash of the token is stored.
type ServiceToken struct {
	Hash       string
	ID         uuid.UUID
	Kind, Name string
	CreatedBy  string
	CreatedAt  time.Time
	LastUsedAt time.Time // zero: never used
}

const serviceTokenCols = "hash, id, kind, name, created_by, created_at, last_used_at"

func scanServiceToken(row pgx.Row) (ServiceToken, error) {
	var t ServiceToken
	var used *time.Time
	if err := row.Scan(&t.Hash, &t.ID, &t.Kind, &t.Name, &t.CreatedBy, &t.CreatedAt, &used); err != nil {
		return ServiceToken{}, err
	}
	t.LastUsedAt = timeOf(used)
	return t, nil
}

// CreateServiceToken stores a service token by its hash.
func (s *Store) CreateServiceToken(ctx context.Context, hash, kind, name, createdBy string) (ServiceToken, error) {
	return scanServiceToken(s.q.QueryRow(ctx, `INSERT INTO service_tokens (hash, kind, name, created_by)
		VALUES ($1, $2, $3, $4) RETURNING `+serviceTokenCols, hash, kind, name, createdBy))
}

// ListServiceTokens returns every service token, oldest first.
func (s *Store) ListServiceTokens(ctx context.Context) ([]ServiceToken, error) {
	rows, err := s.q.Query(ctx, "SELECT "+serviceTokenCols+" FROM service_tokens ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ServiceToken, error) { return scanServiceToken(r) })
}

// DeleteServiceToken revokes a service token by its id and returns it;
// ErrNoRow when there is none.
func (s *Store) DeleteServiceToken(ctx context.Context, id uuid.UUID) (ServiceToken, error) {
	t, err := scanServiceToken(s.q.QueryRow(ctx, "DELETE FROM service_tokens WHERE id = $1 RETURNING "+serviceTokenCols, id))
	return t, noRow(err, ErrNoRow)
}

// UseServiceToken counts a certificate issued under the token of hash:
// last_used_at now(), and one more in the token's window, which starts
// anew once it is window old. ok is false when the window holds more
// than limit certificates with this one, which is then not to be issued.
// ErrNoRow: no such token (unknown or revoked).
func (s *Store) UseServiceToken(ctx context.Context, hash string, limit int, window time.Duration) (ServiceToken, bool, error) {
	var count int
	var t ServiceToken
	var used *time.Time
	err := s.q.QueryRow(ctx, `UPDATE service_tokens SET last_used_at = now(),
			cert_count = CASE WHEN cert_window <= now() - $2::interval THEN 1 ELSE cert_count + 1 END,
			cert_window = CASE WHEN cert_window <= now() - $2::interval THEN now() ELSE cert_window END
		WHERE hash = $1 RETURNING `+serviceTokenCols+", cert_count", hash, interval(window)).Scan(&t.Hash, &t.ID, &t.Kind,
		&t.Name, &t.CreatedBy, &t.CreatedAt, &used, &count)
	if err != nil {
		return ServiceToken{}, false, noRow(err, ErrNoRow)
	}
	t.LastUsedAt = timeOf(used)
	return t, count <= limit, nil
}

// ServiceTokenOf is the service token of hash, to authenticate a request
// with it (ferun-auth's, of kind auth: ADR 0054); unlike UseServiceToken
// it counts no certificate and writes nothing. ErrNoRow: no such token
// (unknown or revoked).
func (s *Store) ServiceTokenOf(ctx context.Context, hash string) (ServiceToken, error) {
	t, err := scanServiceToken(s.q.QueryRow(ctx, "SELECT "+serviceTokenCols+" FROM service_tokens WHERE hash = $1", hash))
	return t, noRow(err, ErrNoRow)
}

// TouchServiceToken sets the token's last_used_at to now(), unless it
// was used within the last minute: a request's touch is a write at most
// once a minute per token, whichever instance serves it.
func (s *Store) TouchServiceToken(ctx context.Context, hash string) error {
	_, err := s.q.Exec(ctx, `UPDATE service_tokens SET last_used_at = now()
		WHERE hash = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, hash)
	return err
}
