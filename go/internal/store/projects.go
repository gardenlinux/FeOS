// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OperatorProject is the project the bootstrap token belongs to.
const OperatorProject = "operator"

// ProjectNamePattern is the rule for project names (projects.id).
var ProjectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,62}$`)

// Project is a row of projects.
type Project struct {
	ID         string // the project's name
	CreatedAt  time.Time
	DeletingAt time.Time // zero unless a DELETE is under way
	// Zone is the zone of its default hosts (ADR 0031); "": the apps
	// zone.
	Zone string
	// MadeBy is the person who made it; uuid.Nil: an operator's project.
	MadeBy uuid.UUID
	Limits ProjectLimits
}

// ProjectLimits are a project's limits on what it asks for (ADR 0031,
// "Quotas per project"); nil: no limit.
type ProjectLimits struct {
	Sandboxes, Environments, RunningJobs, VCPUs *int
	MemoryMiB                                   *int64
}

// Deleting reports whether the project is being deleted.
func (t *Project) Deleting() bool { return !t.DeletingAt.IsZero() }

const projectCols = `id, created_at, deleting_at, coalesce(zone, ''), made_by, limit_sandboxes, limit_environments,
	limit_running_jobs, limit_vcpus, limit_memory_mib`

// projectTargets are the scan targets of projectCols for t, and the
// function that finishes the scan.
func projectTargets(t *Project) ([]any, func()) {
	var deleting *time.Time
	var madeBy *uuid.UUID
	l := &t.Limits
	return []any{&t.ID, &t.CreatedAt, &deleting, &t.Zone, &madeBy, &l.Sandboxes, &l.Environments, &l.RunningJobs,
			&l.VCPUs, &l.MemoryMiB}, func() {
			t.DeletingAt, t.MadeBy = timeOf(deleting), uuidOf(madeBy)
		}
}

func scanProject(row pgx.Row) (Project, error) {
	var t Project
	targets, finish := projectTargets(&t)
	if err := row.Scan(targets...); err != nil {
		return Project{}, err
	}
	finish()
	return t, nil
}

// CreateProject inserts an operator's project, in the apps zone. It returns
// the new row and true, or the existing row and false when the name is
// taken.
func (s *Store) CreateProject(ctx context.Context, id string) (Project, bool, error) {
	return s.MakeProject(ctx, id, NewProject{})
}

// NewProject is what a project gets at its creation beside its name: its
// zone (projects.zone, model.Zones.ForNewProject: the project zone for one a
// person makes; "": the apps zone), the person who made it (uuid.Nil:
// an operator) and its limits (--project-default-* for a person's).
type NewProject struct {
	Zone   string
	MadeBy uuid.UUID
	Limits ProjectLimits
}

// MakeProject inserts a project with n. It returns the new row and true, or
// the existing row, unchanged, and false when the name is taken.
func (s *Store) MakeProject(ctx context.Context, id string, n NewProject) (Project, bool, error) {
	l := n.Limits
	t, err := scanProject(s.q.QueryRow(ctx, `INSERT INTO projects (id, zone, made_by, limit_sandboxes,
			limit_environments, limit_running_jobs, limit_vcpus, limit_memory_mib)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (id) DO NOTHING RETURNING `+projectCols,
		id, nullString(n.Zone), nullUUID(n.MadeBy), l.Sandboxes, l.Environments, l.RunningJobs, l.VCPUs, l.MemoryMiB))
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Project{}, false, err
	}
	t, err = s.GetProject(ctx, id)
	return t, false, err
}

// GetProject reads a project; ErrNoRow if there is none.
func (s *Store) GetProject(ctx context.Context, id string) (Project, error) {
	return s.LockProject(ctx, id, NoLock)
}

// LockProject reads a project and locks its row; ErrNoRow if there is none.
func (s *Store) LockProject(ctx context.Context, id string, lock RowLock) (Project, error) {
	t, err := scanProject(s.q.QueryRow(ctx, "SELECT "+projectCols+" FROM projects WHERE id = $1 "+string(lock), id))
	return t, noRow(err, ErrNoRow)
}

// ProjectInfo is a project with what the admin list shows of it.
type ProjectInfo struct {
	Project
	SandboxCount int64
	// EnvironmentCount is its environments that are not being deleted:
	// a project's deletion takes them, and their addons' data, along.
	EnvironmentCount int64
	Tokens           []ProjectToken
}

// ListProjects returns every project ordered by name, with its number of
// sandboxes (an environment's instance rows are none), of environments
// (those being deleted are none) and its tokens (oldest first).
func (s *Store) ListProjects(ctx context.Context) ([]ProjectInfo, error) {
	rows, err := s.q.Query(ctx, `SELECT `+prefixed("t", []string{"id", "created_at", "deleting_at"})+`,
			coalesce(t.zone, ''), t.made_by, t.limit_sandboxes, t.limit_environments, t.limit_running_jobs,
			t.limit_vcpus, t.limit_memory_mib,
			(SELECT count(*) FROM sandboxes s WHERE s.owner = t.id AND NOT s.instance AND NOT s.project_job),
			(SELECT count(*) FROM environments e WHERE e.owner = t.id AND NOT e.deleting)
		FROM projects t ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	projects, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProjectInfo, error) {
		var ti ProjectInfo
		targets, finish := projectTargets(&ti.Project)
		err := r.Scan(append(targets, &ti.SandboxCount, &ti.EnvironmentCount)...)
		finish()
		return ti, err
	})
	if err != nil {
		return nil, err
	}
	tokens, err := s.listProjectTokens(ctx, "")
	if err != nil {
		return nil, err
	}
	byProject := map[string][]ProjectToken{}
	for _, tk := range tokens {
		byProject[tk.ProjectID] = append(byProject[tk.ProjectID], tk)
	}
	for i := range projects {
		projects[i].Tokens = nonNil(byProject[projects[i].ID])
	}
	return projects, nil
}

// MarkProjectDeleting starts a project's deletion: deleting_at = now()
// unless set, and every project token deleted, so that its requests and its
// sandbox tokens stop resolving at once. Setting deleting_at takes the
// project's environments out of the ingress's table, so it sends
// ferun_ingress "env:<id>" for each of them ("*" for more than
// notifyBatch), all in one transaction. The caller holds the project's row
// FOR UPDATE. It reports whether the project exists.
func (s *Store) MarkProjectDeleting(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.InTx(ctx, func(tx *Store) error {
		exists = false
		var started bool
		err := tx.q.QueryRow(ctx, `WITH old AS (SELECT deleting_at FROM projects WHERE id = $1)
			UPDATE projects SET deleting_at = coalesce(projects.deleting_at, now()) WHERE id = $1
			RETURNING (SELECT deleting_at IS NULL FROM old)`, id).Scan(&started)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		exists = true
		if _, err := tx.q.Exec(ctx, "DELETE FROM project_tokens WHERE project_id = $1", id); err != nil {
			return err
		}
		if !started {
			return nil
		}
		rows, err := tx.q.Query(ctx, "SELECT id FROM environments WHERE owner = $1", id)
		if err != nil {
			return err
		}
		envs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		return tx.notifyIngressEnvs(ctx, envs)
	})
	if err != nil {
		return false, err
	}
	return exists, nil
}

// DeleteProjectIfEmpty removes the project when it has no sandboxes and no
// environments left (the environments' controller deletes those of a
// deleting project). It reports whether it did.
func (s *Store) DeleteProjectIfEmpty(ctx context.Context, id string) (bool, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM projects WHERE id = $1
		AND NOT EXISTS (SELECT 1 FROM sandboxes WHERE owner = $1)
		AND NOT EXISTS (SELECT 1 FROM environments WHERE owner = $1)`, id)
	return tag.RowsAffected() > 0, err
}

// DeletingProjects returns the projects whose deletion has begun, oldest
// first.
func (s *Store) DeletingProjects(ctx context.Context) ([]string, error) {
	rows, err := s.q.Query(ctx, "SELECT id FROM projects WHERE deleting_at IS NOT NULL ORDER BY deleting_at, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// BootstrapOperator creates the project "operator" with a token of hash
// tokenHash (label "bootstrap"), but only when no project exists at all. It
// reports whether it created them.
func (s *Store) BootstrapOperator(ctx context.Context, tokenHash string) (bool, error) {
	var id string
	err := s.q.QueryRow(ctx, `INSERT INTO projects (id) SELECT $1
		WHERE NOT EXISTS (SELECT 1 FROM projects) ON CONFLICT DO NOTHING RETURNING id`, OperatorProject).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = s.q.Exec(ctx, `INSERT INTO project_tokens (hash, project_id, label) VALUES ($1, $2, 'bootstrap')
		ON CONFLICT DO NOTHING`, tokenHash, OperatorProject)
	return err == nil, err
}
