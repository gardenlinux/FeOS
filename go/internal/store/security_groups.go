// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// The security groups of a tenant (ADR 0034): a row of security_groups
// per name an environment's deployments introduced, its ID the group's on
// the nodes (model.SecurityGroupID). InsertDeployment adds the rows of a
// spec's groups, InsertEnvironment the row of default; nothing deletes a
// row but the environment's delete (a cleanup of names no deployment has
// any more is left for later).

// defaultGroup is the group every environment has (spec.DefaultSecurityGroup).
const defaultGroup = "default"

// ensureSpecGroups is the CTE that adds the rows of the groups of the spec
// $3 (jsonb) of environment $1, and default.
const ensureSpecGroups = `groups AS (INSERT INTO security_groups (env_id, name)
		SELECT $1::uuid, g FROM jsonb_object_keys(CASE WHEN jsonb_typeof($3::jsonb->'securityGroups') = 'object'
			THEN $3::jsonb->'securityGroups' ELSE '{}'::jsonb END) AS g
		UNION SELECT $1::uuid, '` + defaultGroup + `'
		ON CONFLICT (env_id, name) DO NOTHING)`

// EnsureSecurityGroup is the ID of environment envID's group name, its
// row added when it has none (a sandbox made in an environment without a
// deployment is in default, ADR 0035). ErrNoRow for an environment that
// does not exist.
func (s *Store) EnsureSecurityGroup(ctx context.Context, envID uuid.UUID, name string) (uuid.UUID, error) {
	var id uuid.UUID
	// The SELECT does not see a row a concurrent insert committed after
	// the statement's snapshot, which made this one's insert do nothing:
	// a second statement does.
	for range 2 {
		err := s.q.QueryRow(ctx, `WITH ins AS (INSERT INTO security_groups (env_id, name) VALUES ($1, $2)
				ON CONFLICT (env_id, name) DO NOTHING RETURNING id)
			SELECT id FROM ins UNION ALL SELECT id FROM security_groups WHERE env_id = $1 AND name = $2 LIMIT 1`,
			envID, name).Scan(&id)
		switch {
		case err == nil:
			return id, nil
		case isForeignKeyViolation(err):
			return uuid.Nil, ErrNoRow
		case !errors.Is(err, pgx.ErrNoRows):
			return uuid.Nil, fmt.Errorf("security group %s of environment %s: %w", name, envID, err)
		}
	}
	return uuid.Nil, fmt.Errorf("security group %s of environment %s: not found after its insert", name, envID)
}

// SecurityGroupID is the ID of environment envID's group name; ErrNoRow
// when it has no row.
func (s *Store) SecurityGroupID(ctx context.Context, envID uuid.UUID, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.q.QueryRow(ctx, `SELECT id FROM security_groups WHERE env_id = $1 AND name = $2`, envID, name).Scan(&id)
	if err != nil {
		return uuid.Nil, noRow(err, ErrNoRow)
	}
	return id, nil
}

// SecurityGroupIDs are the IDs of environment envID's groups, by name.
func (s *Store) SecurityGroupIDs(ctx context.Context, envID uuid.UUID) (map[string]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, `SELECT name, id FROM security_groups WHERE env_id = $1`, envID)
	if err != nil {
		return nil, fmt.Errorf("security groups of environment %s: %w", envID, err)
	}
	out := map[string]uuid.UUID{}
	var name string
	var id uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&name, &id}, func() error {
		out[name] = id
		return nil
	}); err != nil {
		return nil, fmt.Errorf("security groups of environment %s: %w", envID, err)
	}
	return out, nil
}

// The kinds of a GroupUser.
const (
	GroupUserSandbox = "sandbox"
	GroupUserJob     = "job"
)

// GroupUser is a pod of an environment outside its spec that names its
// security groups (ADR 0035): a sandbox, or a tenant's job that is queued
// or running.
type GroupUser struct {
	// Kind is GroupUserSandbox or GroupUserJob.
	Kind string
	ID   podid.ID
	// SecurityGroups are the names of the groups it names, sorted.
	SecurityGroups []string
}

// SecurityGroupUsers are the sandboxes and the queued and running
// tenants' jobs of environment envID with the groups they name, which a
// deployment may not remove (the plan refuses it): a sandbox's groups are
// those it names now (sandboxes.security_groups), also when a move to
// them is pending; a job's are its record's.
func (s *Store) SecurityGroupUsers(ctx context.Context, envID uuid.UUID) ([]GroupUser, error) {
	rows, err := s.q.Query(ctx, `SELECT $2::text, id, security_groups FROM sandboxes
			WHERE environment_id = $1 AND NOT instance AND NOT tenant_job
		UNION ALL SELECT $3::text, id, security_groups FROM jobs
			WHERE environment_id = $1 AND finished_at IS NULL
		ORDER BY 1, 2`, envID, GroupUserSandbox, GroupUserJob)
	if err != nil {
		return nil, fmt.Errorf("security group users of environment %s: %w", envID, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (GroupUser, error) {
		var u GroupUser
		err := r.Scan(&u.Kind, &u.ID, &u.SecurityGroups)
		slices.Sort(u.SecurityGroups)
		return u, err
	})
}

// OwnSandbox is a sandbox of a tenant as an environment's plan sees it
// (ADR 0035): the environment it is in and the groups it names there.
type OwnSandbox struct {
	ID      podid.ID
	EnvID   uuid.UUID
	EnvName string
	// SecurityGroups are the names of the groups it names, sorted.
	SecurityGroups []string
}

// GetOwnSandbox reads owner's sandbox id (not an environment's instance,
// not a tenant's job's pod) with its environment and groups; ErrNoRow
// when owner has none of that ID.
func (s *Store) GetOwnSandbox(ctx context.Context, owner string, id podid.ID) (OwnSandbox, error) {
	var out OwnSandbox
	err := s.q.QueryRow(ctx, `SELECT s.id, s.environment_id, e.name, s.security_groups
		FROM sandboxes s JOIN environments e ON e.id = s.environment_id
		WHERE s.id = $2 AND s.owner = $1 AND NOT s.instance AND NOT s.tenant_job`, owner, id).
		Scan(&out.ID, &out.EnvID, &out.EnvName, &out.SecurityGroups)
	if err != nil {
		return OwnSandbox{}, noRow(err, ErrNoRow)
	}
	slices.Sort(out.SecurityGroups)
	return out, nil
}

// DeployedSecurityGroups are the names of the groups of environment
// envID's deployed deployment, sorted, default always among them (an
// environment without a deployment has it alone): the groups a sandbox
// or a tenant's job of it may name. ErrNoRow when there is no such
// environment.
func (s *Store) DeployedSecurityGroups(ctx context.Context, envID uuid.UUID) ([]string, error) {
	var names []string
	err := s.q.QueryRow(ctx, `SELECT ARRAY(SELECT g FROM (
			SELECT jsonb_object_keys(CASE WHEN jsonb_typeof(d.spec->'securityGroups') = 'object'
				THEN d.spec->'securityGroups' ELSE '{}'::jsonb END) AS g
			FROM env_deployments d WHERE d.env_id = e.id AND d.number = e.deployed
			UNION SELECT '`+defaultGroup+`') n ORDER BY g)
		FROM environments e WHERE e.id = $1`, envID).Scan(&names)
	if err != nil {
		return nil, noRow(err, ErrNoRow)
	}
	return names, nil
}

// SetSandboxGroups writes a sandbox's groups (ADR 0035) and whether its
// pod must be moved to them (pending), and reports whether the row was
// there. Not a lifecycle write: its gen stays. pending wakes the
// environments' groups loop (ferun_leader HintEnvGroups), whose
// controller moves the sandbox.
func (s *Store) SetSandboxGroups(ctx context.Context, id podid.ID, groups []string, pending bool) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE sandboxes SET security_groups = $2, groups_pending = $3, updated_at = now()
		WHERE id = $1 AND NOT instance AND NOT tenant_job`, id, nonNil(groups), pending)
	if err != nil {
		return false, fmt.Errorf("security groups of sandbox %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if pending {
		return true, s.Notify(ctx, db.ChanLeader, HintEnvGroups)
	}
	return true, nil
}

// InUse are the sandboxes and the queued and running tenants' jobs of an
// environment, by ID: what keeps it from being deleted (ADR 0035).
type InUse struct {
	Sandboxes []podid.ID
	Jobs      []podid.ID
}

// Empty reports whether nothing is in use.
func (u InUse) Empty() bool { return len(u.Sandboxes) == 0 && len(u.Jobs) == 0 }

// EnvironmentInUse reads what of environment envID keeps it (InUse).
func (s *Store) EnvironmentInUse(ctx context.Context, envID uuid.UUID) (InUse, error) {
	var out InUse
	err := s.q.QueryRow(ctx, `SELECT
			ARRAY(SELECT id FROM sandboxes WHERE environment_id = $1 AND NOT instance AND NOT tenant_job ORDER BY id),
			ARRAY(SELECT id FROM jobs WHERE environment_id = $1 AND finished_at IS NULL ORDER BY id)`, envID).
		Scan(&out.Sandboxes, &out.Jobs)
	if err != nil {
		return InUse{}, fmt.Errorf("what environment %s holds: %w", envID, err)
	}
	return out, nil
}

// EnvironmentNames are the names of the environments of ids that exist.
func (s *Store) EnvironmentNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := s.q.Query(ctx, `SELECT id, name FROM environments WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("environment names: %w", err)
	}
	out := map[uuid.UUID]string{}
	var id uuid.UUID
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&id, &name}, func() error {
		out[id] = name
		return nil
	}); err != nil {
		return nil, fmt.Errorf("environment names: %w", err)
	}
	return out, nil
}
