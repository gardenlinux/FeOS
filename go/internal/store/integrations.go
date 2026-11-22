// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file holds the typed queries of integrations (ADR 0056, migration
// 0042): their registration, their grants in projects, what they may do
// there, the person one names, and the protected environments of a
// project (ADR 0060) that every role on an environment looks at.

// Integration is a row of integrations: software an operator registered,
// which signs in with its provider's access tokens of (Issuer, Subject).
type Integration struct {
	ID                   uuid.UUID
	Name, Title, URL     string
	Issuer, Subject      string
	DisabledAt           time.Time // zero: enabled
	MadeBy               string
	CreatedAt, UpdatedAt time.Time
	LastSeen             time.Time // zero: never
	// Projects is the number of projects that grant it, as listed.
	Projects int
}

// Disabled reports whether an operator disabled the integration.
func (i Integration) Disabled() bool { return !i.DisabledAt.IsZero() }

const integrationCols = "id, name, title, url, issuer, subject, disabled_at, made_by, created_at, updated_at, last_seen"

func scanIntegration(row pgx.Row, extra ...any) (Integration, error) {
	var i Integration
	var disabled, seen *time.Time
	targets := append([]any{&i.ID, &i.Name, &i.Title, &i.URL, &i.Issuer, &i.Subject, &disabled, &i.MadeBy, &i.CreatedAt,
		&i.UpdatedAt, &seen}, extra...)
	if err := row.Scan(targets...); err != nil {
		return Integration{}, err
	}
	i.DisabledAt, i.LastSeen = timeOf(disabled), timeOf(seen)
	return i, nil
}

// IntegrationBySubject reads the integration of (issuer, subject), the
// principal of an access token; ErrNoRow if none is registered.
func (s *Store) IntegrationBySubject(ctx context.Context, issuer, subject string) (Integration, error) {
	i, err := scanIntegration(s.q.QueryRow(ctx, "SELECT "+integrationCols+" FROM integrations WHERE issuer = $1 AND subject = $2",
		issuer, subject))
	return i, noRow(err, ErrNoRow)
}

// GetIntegration reads an integration by its name; ErrNoRow if none.
func (s *Store) GetIntegration(ctx context.Context, name string) (Integration, error) {
	i, err := scanIntegration(s.q.QueryRow(ctx, "SELECT "+integrationCols+" FROM integrations WHERE name = $1", name))
	return i, noRow(err, ErrNoRow)
}

// ListIntegrations returns every integration by name, each with the
// number of projects that grant it.
func (s *Store) ListIntegrations(ctx context.Context) ([]Integration, error) {
	rows, err := s.q.Query(ctx, "SELECT "+prefixed("i", strings.Split(integrationCols, ", "))+`,
		(SELECT count(DISTINCT g.project_id) FROM grants g WHERE g.integration_id = i.id)
		FROM integrations i ORDER BY i.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Integration, error) {
		var n int
		i, err := scanIntegration(r, &n)
		i.Projects = n
		return i, err
	})
}

// IntegrationIn is what a registration sets.
type IntegrationIn struct {
	Name, Title, URL, Issuer, Subject string
	// MadeBy is the operator, kept from the first registration.
	MadeBy string
}

// PutIntegration registers an integration by its name, or changes its
// title, URL, issuer and subject; created reports a new one. An (issuer,
// subject) another integration has is ErrExists.
func (s *Store) PutIntegration(ctx context.Context, in IntegrationIn) (Integration, bool, error) {
	var created bool
	i, err := scanIntegration(s.q.QueryRow(ctx, `INSERT INTO integrations (name, title, url, issuer, subject, made_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (name) DO UPDATE SET title = excluded.title, url = excluded.url, issuer = excluded.issuer,
			subject = excluded.subject, updated_at = now()
		RETURNING `+integrationCols+", xmax = 0", in.Name, in.Title, in.URL, in.Issuer, in.Subject, in.MadeBy), &created)
	if isUniqueViolation(err) {
		return Integration{}, false, fmt.Errorf("integration %s: subject %s of %s: %w", in.Name, in.Subject, in.Issuer, ErrExists)
	}
	return i, created, err
}

// SetIntegrationDisabled disables an integration (disabled_at = now()
// unless set) or enables it again; its grants stay. ErrNoRow if none.
func (s *Store) SetIntegrationDisabled(ctx context.Context, name string, disabled bool) (Integration, error) {
	i, err := scanIntegration(s.q.QueryRow(ctx, `UPDATE integrations SET disabled_at = CASE WHEN $2
		THEN coalesce(disabled_at, now()) END, updated_at = now() WHERE name = $1 RETURNING `+integrationCols, name, disabled))
	return i, noRow(err, ErrNoRow)
}

// DeleteIntegration removes an integration, and with it its grants and
// the route and sandbox tokens it minted. It reports whether there was
// one.
func (s *Store) DeleteIntegration(ctx context.Context, name string) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM integrations WHERE name = $1", name)
	return tag.RowsAffected() > 0, err
}

// TouchIntegration sets last_seen = now(), at most once a minute.
func (s *Store) TouchIntegration(ctx context.Context, id uuid.UUID) error {
	_, err := s.q.Exec(ctx, `UPDATE integrations SET last_seen = now() WHERE id = $1
		AND (last_seen IS NULL OR last_seen < now() - interval '`+tokenTouchInterval+`')`, id)
	return err
}

// IntegrationAccess returns an integration's grants in a project that
// exists and is not being deleted, and whether it does.
func (s *Store) IntegrationAccess(ctx context.Context, project string, id uuid.UUID) ([]RoleAt, bool, error) {
	var exists bool
	if err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND deleting_at IS NULL)",
		project).Scan(&exists); err != nil || !exists {
		return nil, false, err
	}
	roles, err := collectRoles(s.q.Query(ctx, `SELECT role, environment_id FROM grants
		WHERE project_id = $1 AND integration_id = $2`, project, id))
	return roles, true, err
}

// PersonBySubject reads the person of (issuer, sub), disabled or not;
// ErrNoRow if FeRun does not know them. It makes no row.
func (s *Store) PersonBySubject(ctx context.Context, issuer, sub string) (Person, error) {
	p, err := scanPerson(s.q.QueryRow(ctx, "SELECT "+personCols+" FROM people WHERE issuer = $1 AND sub = $2", issuer, sub))
	return p, noRow(err, ErrNoRow)
}

// PutIntegrationGrant gives an integration its role in a scope of project
// (env uuid.Nil: the project): a new grant, or the role and made_by of the
// one it holds there. An environment of another project, or an unknown
// project or integration, is ErrNoRow.
func (s *Store) PutIntegrationGrant(ctx context.Context, project string, id, env uuid.UUID, role, madeBy string) (RoleAt, error) {
	var out RoleAt
	var outEnv *uuid.UUID
	err := s.q.QueryRow(ctx, `INSERT INTO grants (project_id, integration_id, environment_id, role, made_by)
		SELECT $1, $2, $3, $4, $5
		WHERE $3::uuid IS NULL OR EXISTS (SELECT 1 FROM environments WHERE id = $3 AND owner = $1)
		ON CONFLICT (project_id, integration_id, (coalesce(environment_id::text, ''))) WHERE integration_id IS NOT NULL
		DO UPDATE SET role = excluded.role, made_by = excluded.made_by, updated_at = now()
		RETURNING role, environment_id`, project, id, nullUUID(env), role, madeBy).Scan(&out.Role, &outEnv)
	if errors.Is(err, pgx.ErrNoRows) || isForeignKeyViolation(err) {
		return RoleAt{}, ErrNoRow
	}
	out.EnvironmentID = uuidOf(outEnv)
	return out, err
}

// DeleteIntegrationGrantAt removes an integration's grant in project on
// env (uuid.Nil: the project). It reports whether there was one.
func (s *Store) DeleteIntegrationGrantAt(ctx context.Context, project string, id, env uuid.UUID) (bool, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM grants WHERE project_id = $1 AND integration_id = $2
		AND environment_id IS NOT DISTINCT FROM $3`, project, id, nullUUID(env))
	return tag.RowsAffected() > 0, err
}

// DeleteIntegrationGrants removes every grant of an integration in
// project. It returns how many there were.
func (s *Store) DeleteIntegrationGrants(ctx context.Context, project string, id uuid.UUID) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM grants WHERE project_id = $1 AND integration_id = $2", project, id)
	return tag.RowsAffected(), err
}

// IntegrationMember is an integration that holds a grant in a project,
// with those grants.
type IntegrationMember struct {
	Integration
	Grants []MemberGrant
}

// ListIntegrationGrants returns the integrations that hold a grant in
// project, by name, each with its grants: the project's first, then by
// environment name.
func (s *Store) ListIntegrationGrants(ctx context.Context, project string) ([]IntegrationMember, error) {
	rows, err := s.q.Query(ctx, `SELECT `+prefixed("i", strings.Split(integrationCols, ", "))+`, g.role,
		g.environment_id, coalesce(e.name, ''), g.made_by, g.updated_at
		FROM grants g JOIN integrations i ON i.id = g.integration_id
		LEFT JOIN environments e ON e.id = g.environment_id
		WHERE g.project_id = $1
		ORDER BY i.name, g.environment_id IS NOT NULL, e.name`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntegrationMember
	for rows.Next() {
		var g MemberGrant
		var env *uuid.UUID
		i, err := scanIntegration(rows, &g.Role, &env, &g.EnvironmentName, &g.MadeBy, &g.UpdatedAt)
		if err != nil {
			return nil, err
		}
		g.EnvironmentID = uuidOf(env)
		if len(out) == 0 || out[len(out)-1].ID != i.ID {
			out = append(out, IntegrationMember{Integration: i})
		}
		last := &out[len(out)-1]
		last.Grants = append(last.Grants, g)
	}
	return out, rows.Err()
}

// IntegrationGrants returns an integration's grants in the projects that
// are not being deleted, by project, the project's grant first, then by
// environment name: /auth/me's.
func (s *Store) IntegrationGrants(ctx context.Context, id uuid.UUID) ([]PersonProjectGrant, error) {
	rows, err := s.q.Query(ctx, `SELECT g.project_id, g.role, g.environment_id, coalesce(e.name, '')
		FROM grants g JOIN projects t ON t.id = g.project_id AND t.deleting_at IS NULL
		LEFT JOIN environments e ON e.id = g.environment_id
		WHERE g.integration_id = $1 ORDER BY g.project_id, g.environment_id IS NOT NULL, e.name`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PersonProjectGrant, error) {
		var g PersonProjectGrant
		var env *uuid.UUID
		err := r.Scan(&g.Project, &g.Role, &env, &g.EnvironmentName)
		g.EnvironmentID = uuidOf(env)
		return g, err
	})
}

// ProtectedEnvironments returns the protected environments of project
// that are not being deleted (ADR 0060), read on every request in it.
func (s *Store) ProtectedEnvironments(ctx context.Context, project string) ([]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, "SELECT id FROM environments WHERE owner = $1 AND protected AND NOT deleting", project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// EnvName is an environment of a project, as the answer about a caller
// lists it.
type EnvName struct {
	ID                  uuid.UUID
	Project             string
	Name                string
	Protected, Deleting bool
}

// ProjectEnvironments returns the environments of the projects, by
// project and name.
func (s *Store) ProjectEnvironments(ctx context.Context, projects []string) ([]EnvName, error) {
	rows, err := s.q.Query(ctx, `SELECT id, owner, name, protected, deleting FROM environments WHERE owner = ANY($1)
		ORDER BY owner, name`, projects)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvName, error) {
		var e EnvName
		err := r.Scan(&e.ID, &e.Project, &e.Name, &e.Protected, &e.Deleting)
		return e, err
	})
}

// ProjectsCreated answers when each of projects was made, by name; a
// project that is not there is not in the map.
func (s *Store) ProjectsCreated(ctx context.Context, projects []string) (map[string]time.Time, error) {
	rows, err := s.q.Query(ctx, `SELECT id, created_at FROM projects WHERE id = ANY($1)`, projects)
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	var id string
	var at time.Time
	_, err = pgx.ForEachRow(rows, []any{&id, &at}, func() error {
		out[id] = at.UTC()
		return nil
	})
	return out, err
}

// PrincipalNames names the principals of made_by values: a person's name
// (else e-mail address) for person:<id> and integration:<id>/person:<id>,
// an integration's name for integration:<id>. Others, and those it does
// not know, are not in the map.
func (s *Store) PrincipalNames(ctx context.Context, madeBy []string) (map[string]string, error) {
	var people, integrations []uuid.UUID
	for _, m := range madeBy {
		kind, rest, _ := strings.Cut(m, ":")
		switch kind {
		case "person":
			if id, err := uuid.Parse(rest); err == nil {
				people = append(people, id)
			}
		case "integration":
			in, person, named := strings.Cut(rest, "/person:")
			if named {
				if id, err := uuid.Parse(person); err == nil {
					people = append(people, id)
				}
				continue
			}
			if id, err := uuid.Parse(in); err == nil {
				integrations = append(integrations, id)
			}
		}
	}
	names := map[uuid.UUID]string{}
	if len(people)+len(integrations) > 0 {
		rows, err := s.q.Query(ctx, `SELECT id, coalesce(nullif(name, ''), email) FROM people WHERE id = ANY($1)
			UNION ALL SELECT id, name FROM integrations WHERE id = ANY($2)`, people, integrations)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id uuid.UUID
			var n string
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, err
			}
			names[id] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out := map[string]string{}
	for _, m := range madeBy {
		kind, rest, _ := strings.Cut(m, ":")
		var key string
		switch kind {
		case "person":
			key = rest
		case "integration":
			in, person, named := strings.Cut(rest, "/person:")
			key = in
			if named {
				key = person
			}
		default:
			continue
		}
		if id, err := uuid.Parse(key); err == nil {
			if n, ok := names[id]; ok {
				out[m] = n
			}
		}
	}
	return out, nil
}
