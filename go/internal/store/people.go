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
)

// This file holds the typed queries of people and their grants (ADR 0031,
// migration 0027): people, grants, and what a principal may do in a
// tenant.

// Person is a row of people: someone who signed in through the provider.
type Person struct {
	ID          uuid.UUID
	Issuer, Sub string
	// Email and Name are the last sign-in's, for display only.
	Email, Name         string
	FirstSeen, LastSeen time.Time
	DisabledAt          time.Time // zero: not disabled
}

// Disabled reports whether an operator disabled the person.
func (p Person) Disabled() bool { return !p.DisabledAt.IsZero() }

const personCols = "id, issuer, sub, email, name, first_seen, last_seen, disabled_at"

func scanPerson(row pgx.Row) (Person, error) {
	var p Person
	var disabled *time.Time
	if err := row.Scan(&p.ID, &p.Issuer, &p.Sub, &p.Email, &p.Name, &p.FirstSeen, &p.LastSeen, &disabled); err != nil {
		return Person{}, err
	}
	p.DisabledAt = timeOf(disabled)
	return p, nil
}

// UpsertPerson records a sign-in of (issuer, sub): a new person, or the
// existing one with the sign-in's e-mail address and name and last_seen =
// now(). It returns the row, disabled or not.
func (s *Store) UpsertPerson(ctx context.Context, issuer, sub, email, name string) (Person, error) {
	return scanPerson(s.q.QueryRow(ctx, `INSERT INTO people (issuer, sub, email, name) VALUES ($1, $2, $3, $4)
		ON CONFLICT (issuer, sub) DO UPDATE SET email = excluded.email, name = excluded.name, last_seen = now()
		RETURNING `+personCols, issuer, sub, email, name))
}

// GetPerson reads a person; ErrNoRow if there is none.
func (s *Store) GetPerson(ctx context.Context, id uuid.UUID) (Person, error) {
	p, err := scanPerson(s.q.QueryRow(ctx, "SELECT "+personCols+" FROM people WHERE id = $1", id))
	return p, noRow(err, ErrNoRow)
}

// SetPersonDisabled disables a person (disabled_at = now() unless set,
// and every session of theirs deleted) or enables them again; their grants
// stay. ErrNoRow if there is none.
func (s *Store) SetPersonDisabled(ctx context.Context, id uuid.UUID, disabled bool) (Person, error) {
	var p Person
	err := s.InTx(ctx, func(tx *Store) error {
		var err error
		p, err = scanPerson(tx.q.QueryRow(ctx, `UPDATE people SET disabled_at = CASE WHEN $2
			THEN coalesce(disabled_at, now()) END WHERE id = $1 RETURNING `+personCols, id, disabled))
		if err != nil {
			return noRow(err, ErrNoRow)
		}
		if disabled {
			_, err = tx.q.Exec(ctx, "DELETE FROM ui_sessions WHERE person_id = $1", id)
		}
		return err
	})
	return p, err
}

// Grant is a row of grants: a principal's role in a scope. Exactly one of
// PersonID and TokenHash is set; EnvironmentID uuid.Nil is the tenant.
type Grant struct {
	ID            int64
	TenantID      string
	PersonID      uuid.UUID
	TokenHash     string
	EnvironmentID uuid.UUID
	Role          string // read, write or admin
	MadeBy        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const grantCols = "id, tenant_id, person_id, token_hash, environment_id, role, made_by, created_at, updated_at"

func scanGrant(row pgx.Row) (Grant, error) {
	var g Grant
	var person, env *uuid.UUID
	var token *string
	if err := row.Scan(&g.ID, &g.TenantID, &person, &token, &env, &g.Role, &g.MadeBy, &g.CreatedAt,
		&g.UpdatedAt); err != nil {
		return Grant{}, err
	}
	g.PersonID, g.TokenHash, g.EnvironmentID = uuidOf(person), stringOf(token), uuidOf(env)
	return g, nil
}

// PutGrant gives the grant's principal its role in its scope: a new grant,
// or the role and made_by of the one the principal holds there. An
// environment of another tenant, or an unknown tenant, person or token, is
// ErrNoRow.
func (s *Store) PutGrant(ctx context.Context, g Grant) (Grant, error) {
	out, err := scanGrant(s.q.QueryRow(ctx, `INSERT INTO grants (tenant_id, person_id, token_hash, environment_id,
			role, made_by)
		SELECT $1, $2, $3, $4, $5, $6
		WHERE $4::uuid IS NULL OR EXISTS (SELECT 1 FROM environments WHERE id = $4 AND owner = $1)
		ON CONFLICT (tenant_id, (coalesce(person_id::text, token_hash)), (coalesce(environment_id::text, '')))
		DO UPDATE SET role = excluded.role, made_by = excluded.made_by, updated_at = now()
		RETURNING `+grantCols, g.TenantID, nullUUID(g.PersonID), nullString(g.TokenHash), nullUUID(g.EnvironmentID),
		g.Role, g.MadeBy))
	if errors.Is(err, pgx.ErrNoRows) || isForeignKeyViolation(err) {
		return Grant{}, ErrNoRow
	}
	return out, err
}

// ListGrants returns a tenant's grants, the tenant's before the
// environments', oldest first.
func (s *Store) ListGrants(ctx context.Context, tenant string) ([]Grant, error) {
	rows, err := s.q.Query(ctx, "SELECT "+grantCols+` FROM grants WHERE tenant_id = $1
		ORDER BY environment_id NULLS FIRST, created_at, id`, tenant)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Grant, error) { return scanGrant(r) })
}

// DeleteGrant removes a grant of tenant. It reports whether there was one.
func (s *Store) DeleteGrant(ctx context.Context, tenant string, id int64) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM grants WHERE tenant_id = $1 AND id = $2", tenant, id)
	return tag.RowsAffected() > 0, err
}

// RoleAt is a role in a scope: EnvironmentID uuid.Nil is the tenant.
type RoleAt struct {
	Role          string
	EnvironmentID uuid.UUID
}

func collectRoles(rows pgx.Rows, err error) ([]RoleAt, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RoleAt, error) {
		var ra RoleAt
		var env *uuid.UUID
		err := r.Scan(&ra.Role, &env)
		ra.EnvironmentID = uuidOf(env)
		return ra, err
	})
}

// TokenAccess resolves a tenant token: its tenant and its roles there, its
// own (minted with it) and those of its grants, ignoring a tenant being
// deleted; model.ErrUnknownToken when it is none.
func (s *Store) TokenAccess(ctx context.Context, hash string) (string, []RoleAt, error) {
	var tenant string
	var role string
	var env *uuid.UUID
	err := s.q.QueryRow(ctx, `SELECT t.tenant_id, t.role, t.environment_id FROM tenant_tokens t
		JOIN tenants n ON n.id = t.tenant_id WHERE t.hash = $1 AND n.deleting_at IS NULL`, hash).Scan(&tenant, &role, &env)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, model.ErrUnknownToken
	}
	if err != nil {
		return "", nil, err
	}
	roles, err := collectRoles(s.q.Query(ctx, `SELECT role, environment_id FROM grants
		WHERE tenant_id = $1 AND token_hash = $2`, tenant, hash))
	if err != nil {
		return "", nil, err
	}
	return tenant, append([]RoleAt{{Role: role, EnvironmentID: uuidOf(env)}}, roles...), nil
}

// PersonAccess returns a person's grants in a tenant that exists and is
// not being deleted, and whether it does.
func (s *Store) PersonAccess(ctx context.Context, tenant string, person uuid.UUID) ([]RoleAt, bool, error) {
	var exists bool
	if err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1 AND deleting_at IS NULL)",
		tenant).Scan(&exists); err != nil || !exists {
		return nil, false, err
	}
	roles, err := collectRoles(s.q.Query(ctx, `SELECT role, environment_id FROM grants
		WHERE tenant_id = $1 AND person_id = $2`, tenant, person))
	return roles, true, err
}

// PersonTenantGrant is one grant of a person, with its environment's name,
// as /auth/me lists them.
type PersonTenantGrant struct {
	Tenant          string
	Role            string
	EnvironmentID   uuid.UUID // uuid.Nil: the tenant
	EnvironmentName string
}

// PersonGrants returns a person's grants in the tenants that are not being
// deleted, by tenant, the tenant's grant first, then by environment name.
func (s *Store) PersonGrants(ctx context.Context, person uuid.UUID) ([]PersonTenantGrant, error) {
	rows, err := s.q.Query(ctx, `SELECT g.tenant_id, g.role, g.environment_id, coalesce(e.name, '')
		FROM grants g JOIN tenants t ON t.id = g.tenant_id AND t.deleting_at IS NULL
		LEFT JOIN environments e ON e.id = g.environment_id
		WHERE g.person_id = $1 ORDER BY g.tenant_id, g.environment_id IS NOT NULL, e.name`, person)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PersonTenantGrant, error) {
		var g PersonTenantGrant
		var env *uuid.UUID
		err := r.Scan(&g.Tenant, &g.Role, &env, &g.EnvironmentName)
		g.EnvironmentID = uuidOf(env)
		return g, err
	})
}

// LiveTenantNames returns the names of the tenants that are not being
// deleted, in order: an operator's tenants.
func (s *Store) LiveTenantNames(ctx context.Context) ([]string, error) {
	rows, err := s.q.Query(ctx, "SELECT id FROM tenants WHERE deleting_at IS NULL ORDER BY id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// EnvironmentRef resolves an environment of tenant by its ID or its name
// (the path's {env}) to its ID, for the role checks; ErrNoRow when the
// tenant has none of that ID or name.
func (s *Store) EnvironmentRef(ctx context.Context, tenant, ref string) (uuid.UUID, error) {
	var id uuid.UUID
	q := "SELECT id FROM environments WHERE owner = $1 AND name = $2"
	var arg any = ref
	if u, err := uuid.Parse(ref); err == nil && len(ref) == 36 {
		q, arg = "SELECT id FROM environments WHERE owner = $1 AND id = $2", u
	}
	err := s.q.QueryRow(ctx, q, tenant, arg).Scan(&id)
	return id, noRow(err, ErrNoRow)
}
