// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// tokenTouchInterval is how often a token's last_used_at is written at
// most.
const tokenTouchInterval = "1 minute"

// TenantToken is a row of tenant_tokens. Only the hash is stored. Role and
// EnvironmentID are the token's own grant, minted with it (ADR 0031):
// EnvironmentID uuid.Nil is the tenant.
type TenantToken struct {
	Hash          string
	TenantID      string
	Label         string
	Role          string
	EnvironmentID uuid.UUID
	MadeBy        string
	CreatedAt     time.Time
	LastUsedAt    time.Time // zero: never used
}

const tenantTokenCols = "hash, tenant_id, label, role, environment_id, made_by, created_at, last_used_at"

func scanTenantToken(row pgx.Row) (TenantToken, error) {
	var t TenantToken
	var used *time.Time
	var env *uuid.UUID
	if err := row.Scan(&t.Hash, &t.TenantID, &t.Label, &t.Role, &env, &t.MadeBy, &t.CreatedAt, &used); err != nil {
		return TenantToken{}, err
	}
	t.LastUsedAt, t.EnvironmentID = timeOf(used), uuidOf(env)
	return t, nil
}

// CreateTenantToken stores a tenant token by its hash, admin on its
// tenant. ErrNoRow when the tenant does not exist.
func (s *Store) CreateTenantToken(ctx context.Context, hash, tenant, label string) (TenantToken, error) {
	return s.InsertTenantToken(ctx, TenantToken{Hash: hash, TenantID: tenant, Label: label, Role: "admin"})
}

// InsertTenantToken stores a tenant token by its hash with its role, scope
// and maker (the fields the database sets are ignored). ErrNoRow when the
// tenant does not exist or the scope is not an environment of it.
func (s *Store) InsertTenantToken(ctx context.Context, n TenantToken) (TenantToken, error) {
	t, err := scanTenantToken(s.q.QueryRow(ctx, `INSERT INTO tenant_tokens (hash, tenant_id, label, role,
			environment_id, made_by)
		SELECT $1, $2, $3, $4, $5, $6
		WHERE $5::uuid IS NULL OR EXISTS (SELECT 1 FROM environments WHERE id = $5 AND owner = $2)
		RETURNING `+tenantTokenCols, n.Hash, n.TenantID, n.Label, n.Role, nullUUID(n.EnvironmentID), n.MadeBy))
	if isForeignKeyViolation(err) || errors.Is(err, pgx.ErrNoRows) {
		return TenantToken{}, ErrNoRow
	}
	return t, err
}

// ListTenantTokens returns a tenant's tokens, oldest first.
func (s *Store) ListTenantTokens(ctx context.Context, tenant string) ([]TenantToken, error) {
	return s.listTenantTokens(ctx, tenant)
}

// listTenantTokens returns the tokens of tenant, or of every tenant for "".
func (s *Store) listTenantTokens(ctx context.Context, tenant string) ([]TenantToken, error) {
	rows, err := s.q.Query(ctx, `SELECT `+tenantTokenCols+` FROM tenant_tokens
		WHERE $1 = '' OR tenant_id = $1 ORDER BY tenant_id, created_at, hash`, tenant)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TenantToken, error) { return scanTenantToken(r) })
}

// DeleteTenantToken revokes a tenant's token by its hash. It reports
// whether there was one.
func (s *Store) DeleteTenantToken(ctx context.Context, tenant, hash string) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM tenant_tokens WHERE tenant_id = $1 AND hash = $2", tenant, hash)
	return tag.RowsAffected() > 0, err
}

// ResolveTenantToken returns the tenant of a token hash, ignoring a tenant
// being deleted; model.ErrUnknownToken when there is none.
func (s *Store) ResolveTenantToken(ctx context.Context, hash string) (string, error) {
	var tenant string
	err := s.q.QueryRow(ctx, `SELECT t.tenant_id FROM tenant_tokens t JOIN tenants n ON n.id = t.tenant_id
		WHERE t.hash = $1 AND n.deleting_at IS NULL`, hash).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", model.ErrUnknownToken
	}
	return tenant, err
}

// TouchTenantToken sets last_used_at = now(), at most once a minute.
func (s *Store) TouchTenantToken(ctx context.Context, hash string) error {
	_, err := s.q.Exec(ctx, `UPDATE tenant_tokens SET last_used_at = now() WHERE hash = $1
		AND (last_used_at IS NULL OR last_used_at < now() - interval '`+tokenTouchInterval+`')`, hash)
	return err
}

// SandboxToken is a row of sandbox_tokens. Only the hash is stored.
type SandboxToken struct {
	Hash       string
	SandboxID  podid.ID
	Label      string
	CreatedAt  time.Time
	LastUsedAt time.Time // zero: never used
}

func scanSandboxToken(row pgx.Row) (SandboxToken, error) {
	var t SandboxToken
	var used *time.Time
	if err := row.Scan(&t.Hash, &t.SandboxID, &t.Label, &t.CreatedAt, &used); err != nil {
		return SandboxToken{}, err
	}
	t.LastUsedAt = timeOf(used)
	return t, nil
}

// CreateSandboxToken stores a sandbox token by its hash, made by madeBy
// (a principal, ADR 0031; "" for ferund); model.ErrNotFound when the
// sandbox does not exist.
func (s *Store) CreateSandboxToken(ctx context.Context, hash string, sandboxID podid.ID, label, madeBy string) (SandboxToken, error) {
	t, err := scanSandboxToken(s.q.QueryRow(ctx, `INSERT INTO sandbox_tokens (hash, sandbox_id, label, made_by)
		VALUES ($1, $2, $3, $4) RETURNING hash, sandbox_id, label, created_at, last_used_at`, hash, sandboxID, label, madeBy))
	if isForeignKeyViolation(err) {
		return SandboxToken{}, model.ErrNotFound
	}
	return t, err
}

// ListSandboxTokens returns a sandbox's tokens, oldest first.
func (s *Store) ListSandboxTokens(ctx context.Context, sandboxID podid.ID) ([]SandboxToken, error) {
	rows, err := s.q.Query(ctx, `SELECT hash, sandbox_id, label, created_at, last_used_at FROM sandbox_tokens
		WHERE sandbox_id = $1 ORDER BY created_at, hash`, sandboxID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SandboxToken, error) { return scanSandboxToken(r) })
}

// GetSandboxToken reads a sandbox token by its hash; ErrNoRow if none.
func (s *Store) GetSandboxToken(ctx context.Context, hash string) (SandboxToken, error) {
	t, err := scanSandboxToken(s.q.QueryRow(ctx, `SELECT hash, sandbox_id, label, created_at, last_used_at
		FROM sandbox_tokens WHERE hash = $1`, hash))
	return t, noRow(err, ErrNoRow)
}

// DeleteSandboxToken revokes a sandbox's token by its hash. It reports
// whether the sandbox had it.
func (s *Store) DeleteSandboxToken(ctx context.Context, sandboxID podid.ID, hash string) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM sandbox_tokens WHERE sandbox_id = $1 AND hash = $2", sandboxID, hash)
	return tag.RowsAffected() > 0, err
}

// ResolveSandboxToken returns the sandbox and owner of a token hash,
// ignoring a tenant being deleted and an environment's instance row (the
// sandbox doors treat it as absent; it has no token of its own);
// model.ErrUnknownToken when there is none.
func (s *Store) ResolveSandboxToken(ctx context.Context, hash string) (podid.ID, string, error) {
	var id podid.ID
	var owner string
	err := s.q.QueryRow(ctx, `SELECT t.sandbox_id, s.owner FROM sandbox_tokens t
		JOIN sandboxes s ON s.id = t.sandbox_id JOIN tenants n ON n.id = s.owner
		WHERE t.hash = $1 AND n.deleting_at IS NULL AND s.environment_id IS NULL AND NOT s.tenant_job`, hash).Scan(&id, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", model.ErrUnknownToken
	}
	return id, owner, err
}

// TouchSandboxToken sets last_used_at = now(), at most once a minute.
func (s *Store) TouchSandboxToken(ctx context.Context, hash string) error {
	_, err := s.q.Exec(ctx, `UPDATE sandbox_tokens SET last_used_at = now() WHERE hash = $1
		AND (last_used_at IS NULL OR last_used_at < now() - interval '`+tokenTouchInterval+`')`, hash)
	return err
}
