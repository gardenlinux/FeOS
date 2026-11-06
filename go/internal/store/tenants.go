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

// OperatorTenant is the tenant the bootstrap token belongs to.
const OperatorTenant = "operator"

// TenantNamePattern is the rule for tenant names (tenants.id).
var TenantNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,62}$`)

// Tenant is a row of tenants.
type Tenant struct {
	ID         string // the tenant's name
	CreatedAt  time.Time
	DeletingAt time.Time // zero unless a DELETE is under way
	// Zone is the zone of its default hosts (ADR 0031); "": the apps
	// zone.
	Zone string
	// MadeBy is the person who made it; uuid.Nil: an operator's tenant.
	MadeBy uuid.UUID
	Limits TenantLimits
}

// TenantLimits are a tenant's limits on what it asks for (ADR 0031,
// "Quotas per tenant"); nil: no limit.
type TenantLimits struct {
	Sandboxes, Environments, RunningJobs, VCPUs *int
	MemoryMiB                                   *int64
}

// Deleting reports whether the tenant is being deleted.
func (t *Tenant) Deleting() bool { return !t.DeletingAt.IsZero() }

const tenantCols = `id, created_at, deleting_at, coalesce(zone, ''), made_by, limit_sandboxes, limit_environments,
	limit_running_jobs, limit_vcpus, limit_memory_mib`

// tenantTargets are the scan targets of tenantCols for t, and the
// function that finishes the scan.
func tenantTargets(t *Tenant) ([]any, func()) {
	var deleting *time.Time
	var madeBy *uuid.UUID
	l := &t.Limits
	return []any{&t.ID, &t.CreatedAt, &deleting, &t.Zone, &madeBy, &l.Sandboxes, &l.Environments, &l.RunningJobs,
			&l.VCPUs, &l.MemoryMiB}, func() {
			t.DeletingAt, t.MadeBy = timeOf(deleting), uuidOf(madeBy)
		}
}

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	targets, finish := tenantTargets(&t)
	if err := row.Scan(targets...); err != nil {
		return Tenant{}, err
	}
	finish()
	return t, nil
}

// CreateTenant inserts a tenant. It returns the new row and true, or the
// existing row and false when the name is taken.
func (s *Store) CreateTenant(ctx context.Context, id string) (Tenant, bool, error) {
	t, err := scanTenant(s.q.QueryRow(ctx, `INSERT INTO tenants (id) VALUES ($1) ON CONFLICT (id) DO NOTHING
		RETURNING `+tenantCols, id))
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, false, err
	}
	t, err = s.GetTenant(ctx, id)
	return t, false, err
}

// GetTenant reads a tenant; ErrNoRow if there is none.
func (s *Store) GetTenant(ctx context.Context, id string) (Tenant, error) {
	return s.LockTenant(ctx, id, NoLock)
}

// LockTenant reads a tenant and locks its row; ErrNoRow if there is none.
func (s *Store) LockTenant(ctx context.Context, id string, lock RowLock) (Tenant, error) {
	t, err := scanTenant(s.q.QueryRow(ctx, "SELECT "+tenantCols+" FROM tenants WHERE id = $1 "+string(lock), id))
	return t, noRow(err, ErrNoRow)
}

// TenantInfo is a tenant with what the admin list shows of it.
type TenantInfo struct {
	Tenant
	SandboxCount int64
	// EnvironmentCount is its environments that are not being deleted:
	// a tenant's deletion takes them, and their addons' data, along.
	EnvironmentCount int64
	Tokens           []TenantToken
}

// ListTenants returns every tenant ordered by name, with its number of
// sandboxes (an environment's instance rows are none), of environments
// (those being deleted are none) and its tokens (oldest first).
func (s *Store) ListTenants(ctx context.Context) ([]TenantInfo, error) {
	rows, err := s.q.Query(ctx, `SELECT `+prefixed("t", []string{"id", "created_at", "deleting_at"})+`,
			coalesce(t.zone, ''), t.made_by, t.limit_sandboxes, t.limit_environments, t.limit_running_jobs,
			t.limit_vcpus, t.limit_memory_mib,
			(SELECT count(*) FROM sandboxes s WHERE s.owner = t.id AND s.environment_id IS NULL AND NOT s.tenant_job),
			(SELECT count(*) FROM environments e WHERE e.owner = t.id AND NOT e.deleting)
		FROM tenants t ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	tenants, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TenantInfo, error) {
		var ti TenantInfo
		targets, finish := tenantTargets(&ti.Tenant)
		err := r.Scan(append(targets, &ti.SandboxCount, &ti.EnvironmentCount)...)
		finish()
		return ti, err
	})
	if err != nil {
		return nil, err
	}
	tokens, err := s.listTenantTokens(ctx, "")
	if err != nil {
		return nil, err
	}
	byTenant := map[string][]TenantToken{}
	for _, tk := range tokens {
		byTenant[tk.TenantID] = append(byTenant[tk.TenantID], tk)
	}
	for i := range tenants {
		tenants[i].Tokens = nonNil(byTenant[tenants[i].ID])
	}
	return tenants, nil
}

// MarkTenantDeleting starts a tenant's deletion: deleting_at = now()
// unless set, and every tenant token deleted, so that its requests and its
// sandbox tokens stop resolving at once. Setting deleting_at takes the
// tenant's environments out of the ingress's table, so it sends
// ferun_ingress "env:<id>" for each of them ("*" for more than
// notifyBatch), all in one transaction. The caller holds the tenant's row
// FOR UPDATE. It reports whether the tenant exists.
func (s *Store) MarkTenantDeleting(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.InTx(ctx, func(tx *Store) error {
		exists = false
		var started bool
		err := tx.q.QueryRow(ctx, `WITH old AS (SELECT deleting_at FROM tenants WHERE id = $1)
			UPDATE tenants SET deleting_at = coalesce(tenants.deleting_at, now()) WHERE id = $1
			RETURNING (SELECT deleting_at IS NULL FROM old)`, id).Scan(&started)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		exists = true
		if _, err := tx.q.Exec(ctx, "DELETE FROM tenant_tokens WHERE tenant_id = $1", id); err != nil {
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

// DeleteTenantIfEmpty removes the tenant when it has no sandboxes and no
// environments left (the environments' controller deletes those of a
// deleting tenant). It reports whether it did.
func (s *Store) DeleteTenantIfEmpty(ctx context.Context, id string) (bool, error) {
	tag, err := s.q.Exec(ctx, `DELETE FROM tenants WHERE id = $1
		AND NOT EXISTS (SELECT 1 FROM sandboxes WHERE owner = $1)
		AND NOT EXISTS (SELECT 1 FROM environments WHERE owner = $1)`, id)
	return tag.RowsAffected() > 0, err
}

// DeletingTenants returns the tenants whose deletion has begun, oldest
// first.
func (s *Store) DeletingTenants(ctx context.Context) ([]string, error) {
	rows, err := s.q.Query(ctx, "SELECT id FROM tenants WHERE deleting_at IS NOT NULL ORDER BY deleting_at, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// BootstrapOperator creates the tenant "operator" with a token of hash
// tokenHash (label "bootstrap"), but only when no tenant exists at all. It
// reports whether it created them.
func (s *Store) BootstrapOperator(ctx context.Context, tokenHash string) (bool, error) {
	var id string
	err := s.q.QueryRow(ctx, `INSERT INTO tenants (id) SELECT $1
		WHERE NOT EXISTS (SELECT 1 FROM tenants) ON CONFLICT DO NOTHING RETURNING id`, OperatorTenant).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = s.q.Exec(ctx, `INSERT INTO tenant_tokens (hash, tenant_id, label) VALUES ($1, $2, 'bootstrap')
		ON CONFLICT DO NOTHING`, tokenHash, OperatorTenant)
	return err == nil, err
}
