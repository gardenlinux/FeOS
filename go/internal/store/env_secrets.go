// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// EnvSecret is a row of env_secrets: one version of a secret, sealed
// (internal/seal, AAD envdeps.SecretAAD). A version is never changed; a
// deleted secret's versions keep resolving while a deployment pins them.
type EnvSecret struct {
	// MadeBy is the principal that set the version (ADR 0031); written,
	// not read back.
	MadeBy  string
	EnvID   uuid.UUID
	Name    string
	Version int
	Sealed  []byte
	// System: an addon's credential, "addon.<addon>.<key>", never listed
	// to the tenant.
	System    bool
	CreatedBy string
	DeletedAt time.Time // zero: not deleted
	CreatedAt time.Time
	UpdatedAt time.Time
}

const secretCols = "env_id, name, version, sealed, system, created_by, deleted_at, created_at, updated_at"

func scanSecret(row pgx.Row) (EnvSecret, error) {
	var s EnvSecret
	var deleted *time.Time
	if err := row.Scan(&s.EnvID, &s.Name, &s.Version, &s.Sealed, &s.System, &s.CreatedBy, &deleted,
		&s.CreatedAt, &s.UpdatedAt); err != nil {
		return EnvSecret{}, err
	}
	s.DeletedAt = timeOf(deleted)
	return s, nil
}

func collectSecrets(rows pgx.Rows, err error) ([]EnvSecret, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvSecret, error) { return scanSecret(r) })
}

// InsertSecret inserts a version of a secret, sealed for its version
// already. The callers of one environment serialise their versions by the
// environment's row (FOR UPDATE): a version that exists is ErrExists, an
// unknown environment ErrNoRow.
func (s *Store) InsertSecret(ctx context.Context, sec EnvSecret) (EnvSecret, error) {
	out, err := scanSecret(s.q.QueryRow(ctx, `INSERT INTO env_secrets (env_id, name, version, sealed, system, created_by,
			made_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+secretCols, sec.EnvID, sec.Name, sec.Version, sec.Sealed,
		sec.System, sec.CreatedBy, sec.MadeBy))
	if err != nil {
		return EnvSecret{}, insertError(fmt.Sprintf("insert secret %s version %d", sec.Name, sec.Version), err)
	}
	return out, nil
}

// GetSecret reads a version of a secret, deleted or not; ErrNoRow if there
// is none.
func (s *Store) GetSecret(ctx context.Context, envID uuid.UUID, name string, version int) (EnvSecret, error) {
	sec, err := scanSecret(s.q.QueryRow(ctx, "SELECT "+secretCols+` FROM env_secrets
		WHERE env_id = $1 AND name = $2 AND version = $3`, envID, name, version))
	if err != nil {
		return EnvSecret{}, noRow(err, ErrNoRow)
	}
	return sec, nil
}

// LatestSecret reads the highest version of a secret, deleted or not (the
// next version is one more); ErrNoRow if it never had one.
func (s *Store) LatestSecret(ctx context.Context, envID uuid.UUID, name string) (EnvSecret, error) {
	sec, err := scanSecret(s.q.QueryRow(ctx, "SELECT "+secretCols+` FROM env_secrets
		WHERE env_id = $1 AND name = $2 ORDER BY version DESC LIMIT 1`, envID, name))
	if err != nil {
		return EnvSecret{}, noRow(err, ErrNoRow)
	}
	return sec, nil
}

// ListSecrets returns the latest version of each secret of the
// environment that is not deleted, the tenant's (system false) or the
// addons' (system true), by name.
func (s *Store) ListSecrets(ctx context.Context, envID uuid.UUID, system bool) ([]EnvSecret, error) {
	return collectSecrets(s.q.Query(ctx, "SELECT DISTINCT ON (name) "+secretCols+` FROM env_secrets
		WHERE env_id = $1 AND system = $2 AND deleted_at IS NULL ORDER BY name, version DESC`, envID, system))
}

// DeleteSecret soft-deletes every version of a secret (deleted_at =
// now()) and returns how many it marked.
func (s *Store) DeleteSecret(ctx context.Context, envID uuid.UUID, name string) (int64, error) {
	ct, err := s.q.Exec(ctx, `UPDATE env_secrets SET deleted_at = now(), updated_at = now()
		WHERE env_id = $1 AND name = $2 AND deleted_at IS NULL`, envID, name)
	return ct.RowsAffected(), err
}

// PurgeSecrets removes every version of the system secrets of the
// environment whose name begins with prefix ("addon.<name>.": a removed
// addon's credentials) and returns how many rows went.
func (s *Store) PurgeSecrets(ctx context.Context, envID uuid.UUID, prefix string) (int64, error) {
	ct, err := s.q.Exec(ctx, `DELETE FROM env_secrets WHERE env_id = $1 AND system AND left(name, length($2)) = $2`,
		envID, prefix)
	return ct.RowsAffected(), err
}

// ValueSecretExists reports whether the environment has a tenant's value
// secret of the name with a version that is not deleted: a name an HTTP
// secret cannot take (ADR 0055, one name space of both kinds).
func (s *Store) ValueSecretExists(ctx context.Context, envID uuid.UUID, name string) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM env_secrets
		WHERE env_id = $1 AND name = $2 AND NOT system AND deleted_at IS NULL)`, envID, name).Scan(&ok)
	return ok, err
}

// SecretKindConflict reports an insert that migration 0040's trigger
// refused: a value secret's of a name an HTTP secret of the environment
// holds, or the other way round (ADR 0055).
func SecretKindConflict(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.ConstraintName == "secret_kind"
}
