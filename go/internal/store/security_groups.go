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
// deployment may not remove (the plan refuses it).
//
// The seam of ADR 0035's second step (the sandboxes' store and API):
// what it must return is every sandbox whose environment is envID, and
// every tenant's job of envID that has not ended, each with the names of
// the groups its pod template (or, for a hibernated sandbox moved by PUT
// …/security-groups, its next restore) names. This version reads a
// sandbox's environment from sandboxes.environment_id on a row that is
// neither an environment's instance (env_instances) nor a tenant's job,
// and its groups from its pod template's "sg-<uuid>" IDs; it knows no
// job's environment yet (jobs have no column for it), so it returns
// sandboxes only. The step that gives sandboxes and jobs their
// environment adapts the query to its columns.
func (s *Store) SecurityGroupUsers(ctx context.Context, envID uuid.UUID) ([]GroupUser, error) {
	rows, err := s.q.Query(ctx, `SELECT sb.id, coalesce(array_agg(g.name ORDER BY g.name) FILTER (WHERE g.name IS NOT NULL), '{}')
		FROM sandboxes sb
		LEFT JOIN LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(sb.pod_template->'securityGroups') = 'array'
			THEN sb.pod_template->'securityGroups' ELSE '[]'::jsonb END) AS t(group_id) ON true
		LEFT JOIN security_groups g ON g.env_id = sb.environment_id AND 'sg-' || g.id::text = t.group_id
		WHERE sb.environment_id = $1 AND NOT sb.tenant_job
			AND NOT EXISTS (SELECT 1 FROM env_instances i WHERE i.sandbox_id = sb.id)
		GROUP BY sb.id ORDER BY sb.id`, envID)
	if err != nil {
		return nil, fmt.Errorf("security group users of environment %s: %w", envID, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (GroupUser, error) {
		u := GroupUser{Kind: GroupUserSandbox}
		err := r.Scan(&u.ID, &u.SecurityGroups)
		if len(u.SecurityGroups) == 0 {
			u.SecurityGroups = []string{defaultGroup}
		}
		return u, err
	})
}

// OwnSandbox is a sandbox of a tenant as an environment's plan sees it
// (ADR 0035): the environment it is in and the groups it names there.
type OwnSandbox struct {
	ID podid.ID
	// EnvID and EnvName are its environment; uuid.Nil and "": none yet.
	EnvID   uuid.UUID
	EnvName string
	// SecurityGroups are the names of the groups it names, sorted;
	// [default] when it names none.
	SecurityGroups []string
}

// GetOwnSandbox reads owner's sandbox id (not an environment's instance,
// not a tenant's job's pod) with its environment and groups; ErrNoRow
// when owner has none of that ID.
//
// The seam of ADR 0035's second step: what it must return is the
// environment the sandbox was made in and the groups it names. This
// version reads the environment from sandboxes.environment_id and, for
// a sandbox made before ADR 0035 (environment_id NULL, a row that its
// data migration moves), the environment ADR 0035's migration puts it
// in: the one whose latest deployment's attached service names it, the
// most recently created of several; none when no environment names it.
// Its groups are its pod template's "sg-<uuid>" IDs by name.
func (s *Store) GetOwnSandbox(ctx context.Context, owner string, id podid.ID) (OwnSandbox, error) {
	var out OwnSandbox
	var envID *uuid.UUID
	var envName *string
	err := s.q.QueryRow(ctx, `WITH sb AS (
			SELECT s.id, s.pod_template, coalesce(s.environment_id, (SELECT e.id FROM environments e
				JOIN env_deployments d ON d.env_id = e.id AND d.number = e.deployment
				WHERE e.owner = s.owner AND NOT e.deleting AND EXISTS (
					SELECT 1 FROM jsonb_each(CASE WHEN jsonb_typeof(d.spec->'services') = 'object'
						THEN d.spec->'services' ELSE '{}'::jsonb END) svc
					WHERE svc.value->>'runtime' = 'attached' AND lower(svc.value->>'sandbox') = lower(s.id))
				ORDER BY e.created_at DESC, e.id LIMIT 1)) AS env_id
			FROM sandboxes s
			WHERE s.id = $2 AND s.owner = $1 AND NOT s.tenant_job
				AND NOT EXISTS (SELECT 1 FROM env_instances i WHERE i.sandbox_id = s.id))
		SELECT sb.id, sb.env_id, e.name,
			coalesce((SELECT array_agg(g.name ORDER BY g.name) FROM security_groups g
				WHERE g.env_id = sb.env_id AND 'sg-' || g.id::text IN (SELECT jsonb_array_elements_text(
					CASE WHEN jsonb_typeof(sb.pod_template->'securityGroups') = 'array'
					THEN sb.pod_template->'securityGroups' ELSE '[]'::jsonb END))), '{}')
		FROM sb LEFT JOIN environments e ON e.id = sb.env_id`, owner, id).Scan(&out.ID, &envID, &envName, &out.SecurityGroups)
	if err != nil {
		return OwnSandbox{}, noRow(err, ErrNoRow)
	}
	if envID != nil {
		out.EnvID = *envID
	}
	if envName != nil {
		out.EnvName = *envName
	}
	if len(out.SecurityGroups) == 0 {
		out.SecurityGroups = []string{defaultGroup}
	}
	out.SecurityGroups = slices.Compact(out.SecurityGroups)
	return out, nil
}
