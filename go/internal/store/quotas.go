// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file holds the queries of a project's quotas (ADR 0031, "Quotas
// per project"): its limits, and the rows its use is counted from.

// Any reports whether the limits limit anything.
func (l ProjectLimits) Any() bool {
	return l.Sandboxes != nil || l.Environments != nil || l.RunningJobs != nil || l.VCPUs != nil || l.MemoryMiB != nil
}

// SetProjectLimits replaces a project's limits (nil: none); ErrNoRow if
// there is no project.
func (s *Store) SetProjectLimits(ctx context.Context, id string, l ProjectLimits) (Project, error) {
	t, err := scanProject(s.q.QueryRow(ctx, `UPDATE projects SET limit_sandboxes = $2, limit_environments = $3,
		limit_running_jobs = $4, limit_vcpus = $5, limit_memory_mib = $6 WHERE id = $1 RETURNING `+projectCols,
		id, l.Sandboxes, l.Environments, l.RunningJobs, l.VCPUs, l.MemoryMiB))
	return t, noRow(err, ErrNoRow)
}

// ProjectUseRows are what a project's use is counted from: its sandboxes
// (not an environment's instance, not a project job's), its environments
// that are not being deleted, its projects' jobs that have not finished
// (queued or running), and the specs of its environments' current
// deployments, which the caller sums.
type ProjectUseRows struct {
	Sandboxes, SandboxVCPUs, SandboxMemoryMiB int64
	Environments                              int64
	RunningJobs, JobVCPUs, JobMemoryMiB       int64
	// Specs are the current deployments' specs of the environments not
	// being deleted, but exclude's.
	Specs []json.RawMessage
}

// ProjectUse reads a project's ProjectUseRows; exclude (uuid.Nil: none) is
// an environment whose spec is left out, the one a plan replaces.
func (s *Store) ProjectUse(ctx context.Context, project string, exclude uuid.UUID) (ProjectUseRows, error) {
	var u ProjectUseRows
	err := s.q.QueryRow(ctx, `SELECT s.n, s.v, s.m, e.n, j.n, j.v, j.m FROM
		(SELECT count(*) n, coalesce(sum(vcpus), 0) v, coalesce(sum(memory_mib), 0) m FROM sandboxes
			WHERE owner = $1 AND NOT instance AND NOT project_job AND client_state <> 'expired') s,
		(SELECT count(*) n FROM environments WHERE owner = $1 AND NOT deleting) e,
		(SELECT count(*) n, coalesce(sum(vcpus), 0) v, coalesce(sum(memory_mib), 0) m FROM jobs
			WHERE owner = $1 AND finished_at IS NULL) j`, project).Scan(&u.Sandboxes, &u.SandboxVCPUs,
		&u.SandboxMemoryMiB, &u.Environments, &u.RunningJobs, &u.JobVCPUs, &u.JobMemoryMiB)
	if err != nil {
		return ProjectUseRows{}, err
	}
	rows, err := s.q.Query(ctx, `SELECT d.spec FROM environments e
		JOIN env_deployments d ON d.env_id = e.id AND d.number = e.deployment
		WHERE e.owner = $1 AND NOT e.deleting AND e.id <> $2 ORDER BY e.id`, project, exclude)
	if err != nil {
		return ProjectUseRows{}, err
	}
	u.Specs, err = pgx.CollectRows(rows, pgx.RowTo[json.RawMessage])
	return u, err
}
