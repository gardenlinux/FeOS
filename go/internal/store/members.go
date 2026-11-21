// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file holds the typed queries of a project's members (ADR 0031,
// "People make projects and invite each other"): the projects people make,
// the members and their grants by scope, the admins a project keeps, and
// the invitations.

// LockPerson reads a person and locks the row FOR UPDATE: a project's
// creation counts the projects its maker made under it. ErrNoRow if there
// is none.
func (s *Store) LockPerson(ctx context.Context, id uuid.UUID) (Person, error) {
	p, err := scanPerson(s.q.QueryRow(ctx, "SELECT "+personCols+" FROM people WHERE id = $1 FOR UPDATE", id))
	return p, noRow(err, ErrNoRow)
}

// ListPeople returns everyone who signed in, the last seen first.
func (s *Store) ListPeople(ctx context.Context) ([]Person, error) {
	rows, err := s.q.Query(ctx, "SELECT "+personCols+" FROM people ORDER BY last_seen DESC, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Person, error) { return scanPerson(r) })
}

// CountProjectsMadeBy counts the projects a person made that are not being
// deleted (--projects-per-person).
func (s *Store) CountProjectsMadeBy(ctx context.Context, person uuid.UUID) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, "SELECT count(*) FROM projects WHERE made_by = $1 AND deleting_at IS NULL", person).Scan(&n)
	return n, err
}

// MemberGrant is one grant of a member, with its environment's name.
type MemberGrant struct {
	Role            string
	EnvironmentID   uuid.UUID // uuid.Nil: the project
	EnvironmentName string
	MadeBy          string
	UpdatedAt       time.Time
}

// Member is a person who holds a grant in a project, with those grants.
type Member struct {
	Person
	Grants []MemberGrant
}

// ListMembers returns the people who hold a grant in project, by name and
// e-mail address, each with their grants: the project's first, then by
// environment name.
func (s *Store) ListMembers(ctx context.Context, project string) ([]Member, error) {
	rows, err := s.q.Query(ctx, `SELECT `+prefixed("p", []string{"id", "issuer", "sub", "email", "name", "first_seen",
		"last_seen", "disabled_at"})+`, g.role, g.environment_id, coalesce(e.name, ''), g.made_by, g.updated_at
		FROM grants g JOIN people p ON p.id = g.person_id
		LEFT JOIN environments e ON e.id = g.environment_id
		WHERE g.project_id = $1
		ORDER BY lower(p.name), lower(p.email), p.id, g.environment_id IS NOT NULL, e.name`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var p Person
		var disabled *time.Time
		var g MemberGrant
		var env *uuid.UUID
		if err := rows.Scan(&p.ID, &p.Issuer, &p.Sub, &p.Email, &p.Name, &p.FirstSeen, &p.LastSeen, &disabled, &g.Role,
			&env, &g.EnvironmentName, &g.MadeBy, &g.UpdatedAt); err != nil {
			return nil, err
		}
		p.DisabledAt, g.EnvironmentID = timeOf(disabled), uuidOf(env)
		if len(out) == 0 || out[len(out)-1].ID != p.ID {
			out = append(out, Member{Person: p})
		}
		last := &out[len(out)-1]
		last.Grants = append(last.Grants, g)
	}
	return out, rows.Err()
}

// IsMember reports whether a person holds any grant in project.
func (s *Store) IsMember(ctx context.Context, project string, person uuid.UUID) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM grants WHERE project_id = $1 AND person_id = $2)",
		project, person).Scan(&ok)
	return ok, err
}

// PersonGrantAt reads a person's grant in project on env (uuid.Nil: the
// project); ErrNoRow if they hold none there.
func (s *Store) PersonGrantAt(ctx context.Context, project string, person, env uuid.UUID) (Grant, error) {
	g, err := scanGrant(s.q.QueryRow(ctx, "SELECT "+grantCols+` FROM grants
		WHERE project_id = $1 AND person_id = $2 AND environment_id IS NOT DISTINCT FROM $3`,
		project, person, nullUUID(env)))
	return g, noRow(err, ErrNoRow)
}

// DeletePersonGrantAt removes a person's grant in project on env (uuid.Nil:
// the project). It reports whether there was one.
func (s *Store) DeletePersonGrantAt(ctx context.Context, project string, person, env uuid.UUID) (bool, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM grants WHERE project_id = $1 AND person_id = $2
		AND environment_id IS NOT DISTINCT FROM $3`, project, person, nullUUID(env))
	return tag.RowsAffected() > 0, err
}

// DeletePersonGrants removes every grant of a person in project: they are
// no member any more. It returns how many there were.
func (s *Store) DeletePersonGrants(ctx context.Context, project string, person uuid.UUID) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM grants WHERE project_id = $1 AND person_id = $2", project, person)
	return tag.RowsAffected(), err
}

// CountAdmins counts the principals that hold admin on project itself:
// the people with a project grant admin who are not disabled, and the
// tokens that are admin on the project, by their own grant or by one of
// grants. A project always keeps one (ADR 0031).
func (s *Store) CountAdmins(ctx context.Context, project string) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM grants g JOIN people p ON p.id = g.person_id
			WHERE g.project_id = $1 AND g.environment_id IS NULL AND g.role = 'admin' AND p.disabled_at IS NULL)
		+ (SELECT count(*) FROM project_tokens t WHERE t.project_id = $1 AND (
			(t.role = 'admin' AND t.environment_id IS NULL)
			OR EXISTS (SELECT 1 FROM grants g WHERE g.token_hash = t.hash AND g.environment_id IS NULL
				AND g.role = 'admin')))`, project).Scan(&n)
	return n, err
}

// Invitation is a row of invitations: a single-use link's grant, its
// secret stored as a hash. EnvironmentID uuid.Nil is the project.
type Invitation struct {
	Hash            string
	ProjectID       string
	EnvironmentID   uuid.UUID
	EnvironmentName string // as read, "" for the project
	Role            string
	MadeBy          string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	UsedBy          uuid.UUID // uuid.Nil: not used
	UsedByName      string    // as read: the taker's name, else e-mail address
	UsedAt          time.Time
}

// Open reports whether the invitation can still be used at now.
func (i Invitation) Open(now time.Time) bool { return i.UsedBy == uuid.Nil && now.Before(i.ExpiresAt) }

const invitationCols = `i.hash, i.project_id, i.environment_id, coalesce(e.name, ''), i.role, i.made_by, i.created_at,
	i.expires_at, i.used_by, coalesce(nullif(u.name, ''), u.email, ''), i.used_at`

// invitationJoins are the joins of invitationCols, after FROM's i.
const invitationJoins = ` LEFT JOIN environments e ON e.id = i.environment_id LEFT JOIN people u ON u.id = i.used_by`

func scanInvitation(row pgx.Row) (Invitation, error) {
	var i Invitation
	var env, usedBy *uuid.UUID
	var usedAt *time.Time
	if err := row.Scan(&i.Hash, &i.ProjectID, &env, &i.EnvironmentName, &i.Role, &i.MadeBy, &i.CreatedAt, &i.ExpiresAt,
		&usedBy, &i.UsedByName, &usedAt); err != nil {
		return Invitation{}, err
	}
	i.EnvironmentID, i.UsedBy, i.UsedAt = uuidOf(env), uuidOf(usedBy), timeOf(usedAt)
	return i, nil
}

// InsertInvitation stores an invitation by its hash, valid for ttl from
// now() (the database's clock). ErrNoRow when the project does not exist
// or the scope is not an environment of it.
func (s *Store) InsertInvitation(ctx context.Context, n Invitation, ttl time.Duration) (Invitation, error) {
	inv, err := scanInvitation(s.q.QueryRow(ctx, `WITH i AS (INSERT INTO invitations (hash, project_id,
			environment_id, role, made_by, expires_at)
		SELECT $1, $2, $3, $4, $5, now() + make_interval(secs => $6)
		WHERE $3::uuid IS NULL OR EXISTS (SELECT 1 FROM environments WHERE id = $3 AND owner = $2)
		RETURNING *)
		SELECT `+invitationCols+` FROM i`+invitationJoins,
		n.Hash, n.ProjectID, nullUUID(n.EnvironmentID), n.Role, n.MadeBy, ttl.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) || isForeignKeyViolation(err) {
		return Invitation{}, ErrNoRow
	}
	return inv, err
}

// ListInvitations returns a project's invitations, open, used and expired,
// oldest first.
func (s *Store) ListInvitations(ctx context.Context, project string) ([]Invitation, error) {
	rows, err := s.q.Query(ctx, `SELECT `+invitationCols+` FROM invitations i`+invitationJoins+`
		WHERE i.project_id = $1 ORDER BY i.created_at, i.hash`, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invitation, error) { return scanInvitation(r) })
}

// GetInvitation reads an invitation by its hash; ErrNoRow if none.
func (s *Store) GetInvitation(ctx context.Context, hash string) (Invitation, error) {
	inv, err := scanInvitation(s.q.QueryRow(ctx, `SELECT `+invitationCols+` FROM invitations i`+invitationJoins+`
		WHERE i.hash = $1`, hash))
	return inv, noRow(err, ErrNoRow)
}

// DeleteInvitation revokes an invitation of project. It reports whether
// there was one.
func (s *Store) DeleteInvitation(ctx context.Context, project, hash string) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM invitations WHERE project_id = $1 AND hash = $2", project, hash)
	return tag.RowsAffected() > 0, err
}

// UseInvitation marks an open invitation used by person, now(): once
// only, whoever comes second finds it used. ErrNoRow when it is not
// open (unknown, used or expired).
func (s *Store) UseInvitation(ctx context.Context, hash string, person uuid.UUID) (Invitation, error) {
	inv, err := scanInvitation(s.q.QueryRow(ctx, `WITH i AS (UPDATE invitations SET used_by = $2, used_at = now()
		WHERE hash = $1 AND used_at IS NULL AND expires_at > now() RETURNING *)
		SELECT `+invitationCols+` FROM i`+invitationJoins, hash, person))
	return inv, noRow(err, ErrNoRow)
}
