// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file holds the queries of a tenant's quotas (ADR 0031, "Quotas
// per tenant"): its limits, and the rows its use is counted from.

// Any reports whether the limits limit anything.
func (l TenantLimits) Any() bool {
	return l.Sandboxes != nil || l.Environments != nil || l.RunningJobs != nil || l.VCPUs != nil || l.MemoryMiB != nil
}

// SetTenantLimits replaces a tenant's limits (nil: none); ErrNoRow if
// there is no tenant.
func (s *Store) SetTenantLimits(ctx context.Context, id string, l TenantLimits) (Tenant, error) {
	t, err := scanTenant(s.q.QueryRow(ctx, `UPDATE tenants SET limit_sandboxes = $2, limit_environments = $3,
		limit_running_jobs = $4, limit_vcpus = $5, limit_memory_mib = $6 WHERE id = $1 RETURNING `+tenantCols,
		id, l.Sandboxes, l.Environments, l.RunningJobs, l.VCPUs, l.MemoryMiB))
	return t, noRow(err, ErrNoRow)
}

// TenantUseRows are what a tenant's use is counted from: its sandboxes
// (not an environment's instance, not a tenant job's), its environments
// that are not being deleted, its tenants' jobs that have not finished
// (queued or running), and the specs of its environments' current
// deployments, which the caller sums.
type TenantUseRows struct {
	Sandboxes, SandboxVCPUs, SandboxMemoryMiB int64
	Environments                              int64
	RunningJobs, JobVCPUs, JobMemoryMiB       int64
	// Specs are the current deployments' specs of the environments not
	// being deleted, but exclude's.
	Specs []json.RawMessage
}

// TenantUse reads a tenant's TenantUseRows; exclude (uuid.Nil: none) is
// an environment whose spec is left out, the one a plan replaces.
func (s *Store) TenantUse(ctx context.Context, tenant string, exclude uuid.UUID) (TenantUseRows, error) {
	var u TenantUseRows
	err := s.q.QueryRow(ctx, `SELECT s.n, s.v, s.m, e.n, j.n, j.v, j.m FROM
		(SELECT count(*) n, coalesce(sum(vcpus), 0) v, coalesce(sum(memory_mib), 0) m FROM sandboxes
			WHERE owner = $1 AND NOT instance AND NOT tenant_job) s,
		(SELECT count(*) n FROM environments WHERE owner = $1 AND NOT deleting) e,
		(SELECT count(*) n, coalesce(sum(vcpus), 0) v, coalesce(sum(memory_mib), 0) m FROM jobs
			WHERE owner = $1 AND finished_at IS NULL) j`, tenant).Scan(&u.Sandboxes, &u.SandboxVCPUs,
		&u.SandboxMemoryMiB, &u.Environments, &u.RunningJobs, &u.JobVCPUs, &u.JobMemoryMiB)
	if err != nil {
		return TenantUseRows{}, err
	}
	rows, err := s.q.Query(ctx, `SELECT d.spec FROM environments e
		JOIN env_deployments d ON d.env_id = e.id AND d.number = e.deployment
		WHERE e.owner = $1 AND NOT e.deleting AND e.id <> $2 ORDER BY e.id`, tenant, exclude)
	if err != nil {
		return TenantUseRows{}, err
	}
	u.Specs, err = pgx.CollectRows(rows, pgx.RowTo[json.RawMessage])
	return u, err
}
