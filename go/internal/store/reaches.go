// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// What a sandbox and a tenant's job reach (ADR 0034 revised, ADR 0035
// revised): sandboxes.reaches and jobs.reaches ("all" or a list of
// entries, [] for nothing) and their egress (NULL: the internet). A
// sandbox's reaches change in place (SetSandboxReaches); a job's are
// fixed. The controller writes them as rules of their targets.

// reachJSON are reaches and egress as their columns store them.
func reachJSON(r spec.Reaches, e *spec.Egress) (string, any, error) {
	rb, err := json.Marshal(r)
	if err != nil {
		return "", nil, fmt.Errorf("reaches: %w", err)
	}
	if e == nil {
		return string(rb), nil, nil
	}
	eb, err := json.Marshal(e)
	if err != nil {
		return "", nil, fmt.Errorf("egress: %w", err)
	}
	return string(rb), string(eb), nil
}

// scanReach reads the reaches and egress columns.
func scanReach(reaches, egress []byte) (spec.Reaches, *spec.Egress, error) {
	var r spec.Reaches
	if len(reaches) > 0 {
		if err := json.Unmarshal(reaches, &r); err != nil {
			return spec.Reaches{}, nil, fmt.Errorf("reaches: %w", err)
		}
	}
	if len(egress) == 0 {
		return r, nil, nil
	}
	var e spec.Egress
	if err := json.Unmarshal(egress, &e); err != nil {
		return spec.Reaches{}, nil, fmt.Errorf("egress: %w", err)
	}
	return r, &e, nil
}

// DropLegacyGroups drops the first version's groups (model.
// LegacyGroupPrefix) from the template of a row whose pod is made next
// (the begin of its create or restore, in its transaction, with the row
// locked): the template names its workload's group already (migration
// 0033). Not a lifecycle write: its gen stays. The row as written, or as
// it was when its template names none.
func (s *Store) DropLegacyGroups(ctx context.Context, row Sandbox) (Sandbox, error) {
	t := row.PodTemplate
	if t == nil || !slices.ContainsFunc(t.SecurityGroups, isLegacyGroup) {
		return row, nil
	}
	nt := *t
	nt.SecurityGroups = slices.DeleteFunc(slices.Clone(t.SecurityGroups), isLegacyGroup)
	tj, err := templateJSON(&nt)
	if err != nil {
		return Sandbox{}, err
	}
	out, err := scanSandbox(s.q.QueryRow(ctx, `UPDATE sandboxes SET pod_template = $3
		WHERE id = $1 AND gen = $2 RETURNING `+sandboxCols, row.ID, row.Gen, tj))
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrStale
	}
	if err != nil {
		return Sandbox{}, fmt.Errorf("legacy groups of sandbox %s: %w", row.ID, err)
	}
	return out, nil
}

func isLegacyGroup(id string) bool { return strings.HasPrefix(id, model.LegacyGroupPrefix) }

// SetSandboxReaches writes what a sandbox reaches (ADR 0035, revised): a
// change of its targets' rules, in place, no move. It reports whether the
// row was there. Not a lifecycle write: its gen stays. It wakes env-groups
// (ferun_leader HintEnvGroups), which puts the changed rules.
func (s *Store) SetSandboxReaches(ctx context.Context, id podid.ID, r spec.Reaches) (bool, error) {
	rb, _, err := reachJSON(r, nil)
	if err != nil {
		return false, err
	}
	tag, err := s.q.Exec(ctx, `UPDATE sandboxes SET reaches = $2::jsonb, updated_at = now()
		WHERE id = $1 AND NOT instance AND NOT tenant_job`, id, rb)
	if err != nil {
		return false, fmt.Errorf("reaches of sandbox %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, s.Notify(ctx, db.ChanLeader, HintEnvGroups)
}

// Reacher is a pod of an environment outside its spec (ADR 0035): a
// sandbox or a tenant's job's pod, with what it reaches and its egress.
type Reacher struct {
	ID        podid.ID
	TenantJob bool
	Reaches   spec.Reaches
	Egress    *spec.Egress
}

// EnvCtlReachers are environment envID's sandboxes and tenants' jobs'
// pods (rows that are not instances), by ID.
func (s *Store) EnvCtlReachers(ctx context.Context, envID uuid.UUID) ([]Reacher, error) {
	rows, err := s.q.Query(ctx, `SELECT id, tenant_job, reaches, egress FROM sandboxes
		WHERE environment_id = $1 AND NOT instance ORDER BY id`, envID)
	if err != nil {
		return nil, fmt.Errorf("reachers of environment %s: %w", envID, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Reacher, error) {
		var out Reacher
		var reaches, egress []byte
		if err := r.Scan(&out.ID, &out.TenantJob, &reaches, &egress); err != nil {
			return Reacher{}, err
		}
		var err error
		out.Reaches, out.Egress, err = scanReach(reaches, egress)
		return out, err
	})
}

// OwnSandbox is a sandbox of a tenant as an environment's plan sees it
// (ADR 0035): the environment it is in.
type OwnSandbox struct {
	ID      podid.ID
	EnvID   uuid.UUID
	EnvName string
}

// GetOwnSandbox reads owner's sandbox id (not an environment's instance,
// not a tenant's job's pod) with its environment; ErrNoRow when owner has
// none of that ID.
func (s *Store) GetOwnSandbox(ctx context.Context, owner string, id podid.ID) (OwnSandbox, error) {
	var out OwnSandbox
	err := s.q.QueryRow(ctx, `SELECT s.id, s.environment_id, e.name
		FROM sandboxes s JOIN environments e ON e.id = s.environment_id
		WHERE s.id = $2 AND s.owner = $1 AND NOT s.instance AND NOT s.tenant_job`, owner, id).
		Scan(&out.ID, &out.EnvID, &out.EnvName)
	if err != nil {
		return OwnSandbox{}, noRow(err, ErrNoRow)
	}
	return out, nil
}

// DeployedSpec is environment envID's deployed deployment's spec as
// stored (nil without a deployed deployment): what a sandbox's or a
// tenant's job's reaches may name. ErrNoRow when there is no such
// environment.
func (s *Store) DeployedSpec(ctx context.Context, envID uuid.UUID) ([]byte, error) {
	var raw []byte
	err := s.q.QueryRow(ctx, `SELECT d.spec FROM environments e
		LEFT JOIN env_deployments d ON d.env_id = e.id AND d.number = e.deployed
		WHERE e.id = $1`, envID).Scan(&raw)
	if err != nil {
		return nil, noRow(err, ErrNoRow)
	}
	return raw, nil
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
