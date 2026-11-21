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

// ProjectToken is a row of project_tokens. Only the hash is stored. Role and
// EnvironmentID are the token's own grant, minted with it (ADR 0031):
// EnvironmentID uuid.Nil is the project.
type ProjectToken struct {
	Hash          string
	ProjectID     string
	Label         string
	Role          string
	EnvironmentID uuid.UUID
	MadeBy        string
	CreatedAt     time.Time
	LastUsedAt    time.Time // zero: never used
}

const projectTokenCols = "hash, project_id, label, role, environment_id, made_by, created_at, last_used_at"

func scanProjectToken(row pgx.Row) (ProjectToken, error) {
	var t ProjectToken
	var used *time.Time
	var env *uuid.UUID
	if err := row.Scan(&t.Hash, &t.ProjectID, &t.Label, &t.Role, &env, &t.MadeBy, &t.CreatedAt, &used); err != nil {
		return ProjectToken{}, err
	}
	t.LastUsedAt, t.EnvironmentID = timeOf(used), uuidOf(env)
	return t, nil
}

// CreateProjectToken stores a project token by its hash, admin on its
// project. ErrNoRow when the project does not exist.
func (s *Store) CreateProjectToken(ctx context.Context, hash, project, label string) (ProjectToken, error) {
	return s.InsertProjectToken(ctx, ProjectToken{Hash: hash, ProjectID: project, Label: label, Role: "admin"})
}

// InsertProjectToken stores a project token by its hash with its role, scope
// and maker (the fields the database sets are ignored). ErrNoRow when the
// project does not exist or the scope is not an environment of it.
func (s *Store) InsertProjectToken(ctx context.Context, n ProjectToken) (ProjectToken, error) {
	t, err := scanProjectToken(s.q.QueryRow(ctx, `INSERT INTO project_tokens (hash, project_id, label, role,
			environment_id, made_by)
		SELECT $1, $2, $3, $4, $5, $6
		WHERE $5::uuid IS NULL OR EXISTS (SELECT 1 FROM environments WHERE id = $5 AND owner = $2)
		RETURNING `+projectTokenCols, n.Hash, n.ProjectID, n.Label, n.Role, nullUUID(n.EnvironmentID), n.MadeBy))
	if isForeignKeyViolation(err) || errors.Is(err, pgx.ErrNoRows) {
		return ProjectToken{}, ErrNoRow
	}
	return t, err
}

// ListProjectTokens returns a project's tokens, oldest first.
func (s *Store) ListProjectTokens(ctx context.Context, project string) ([]ProjectToken, error) {
	return s.listProjectTokens(ctx, project)
}

// listProjectTokens returns the tokens of project, or of every project for "".
func (s *Store) listProjectTokens(ctx context.Context, project string) ([]ProjectToken, error) {
	rows, err := s.q.Query(ctx, `SELECT `+projectTokenCols+` FROM project_tokens
		WHERE $1 = '' OR project_id = $1 ORDER BY project_id, created_at, hash`, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProjectToken, error) { return scanProjectToken(r) })
}

// DeleteProjectToken revokes a project's token by its hash. It reports
// whether there was one.
func (s *Store) DeleteProjectToken(ctx context.Context, project, hash string) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM project_tokens WHERE project_id = $1 AND hash = $2", project, hash)
	return tag.RowsAffected() > 0, err
}

// ResolveProjectToken returns the project of a token hash, ignoring a project
// being deleted; model.ErrUnknownToken when there is none.
func (s *Store) ResolveProjectToken(ctx context.Context, hash string) (string, error) {
	var project string
	err := s.q.QueryRow(ctx, `SELECT t.project_id FROM project_tokens t JOIN projects n ON n.id = t.project_id
		WHERE t.hash = $1 AND n.deleting_at IS NULL`, hash).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", model.ErrUnknownToken
	}
	return project, err
}

// TouchProjectToken sets last_used_at = now(), at most once a minute.
func (s *Store) TouchProjectToken(ctx context.Context, hash string) error {
	_, err := s.q.Exec(ctx, `UPDATE project_tokens SET last_used_at = now() WHERE hash = $1
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
// ignoring a project being deleted and an environment's instance row (the
// sandbox doors treat it as absent; it has no token of its own);
// model.ErrUnknownToken when there is none.
func (s *Store) ResolveSandboxToken(ctx context.Context, hash string) (podid.ID, string, error) {
	var id podid.ID
	var owner string
	err := s.q.QueryRow(ctx, `SELECT t.sandbox_id, s.owner FROM sandbox_tokens t
		JOIN sandboxes s ON s.id = t.sandbox_id JOIN projects n ON n.id = s.owner
		WHERE t.hash = $1 AND n.deleting_at IS NULL AND NOT s.instance AND NOT s.project_job`, hash).Scan(&id, &owner)
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
