// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The environment service's own queries (internal/env): the listing with
// each environment's state, the secrets' versions a validation checks,
// the jobs' latest runs a status shows, and the attached services of
// other environments in a sandbox.

// EnvSummaryRow is an environment as its tenant's listing shows it.
type EnvSummaryRow struct {
	Environment
	// ReleaseState is the state of its latest release; "" without one.
	ReleaseState  string
	OpenIncidents int64
}

// EnvSummaries returns the environments of owner that carry every label
// of labels, with their latest release's state and their number of open
// incidents, by name.
func (s *Store) EnvSummaries(ctx context.Context, owner string, labels map[string]string) ([]EnvSummaryRow, error) {
	want, err := jsonText(stringMap(labels))
	if err != nil {
		return nil, err
	}
	rows, err := s.q.Query(ctx, "SELECT "+environmentColumns("e")+`, coalesce(r.state, ''),
			(SELECT count(*) FROM env_incidents i WHERE i.env_id = e.id AND i.closed_at IS NULL)
		FROM environments e LEFT JOIN env_releases r ON r.env_id = e.id AND r.number = e.release
		WHERE e.owner = $1 AND e.labels @> $2::jsonb ORDER BY e.name`, owner, want)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvSummaryRow, error) {
		var out EnvSummaryRow
		targets, finish := environmentTargets(&out.Environment)
		if err := r.Scan(append(targets, &out.ReleaseState, &out.OpenIncidents)...); err != nil {
			return EnvSummaryRow{}, err
		}
		finish()
		return out, nil
	})
}

// SecretVersions are the versions of a tenant secret.
type SecretVersions struct {
	// Versions are those not deleted, Deleted the deleted ones, both
	// ascending.
	Versions, Deleted []int
}

// TenantSecretVersions returns the versions of every tenant secret
// (system false) the environment ever had, by name.
func (s *Store) TenantSecretVersions(ctx context.Context, envID uuid.UUID) (map[string]SecretVersions, error) {
	rows, err := s.q.Query(ctx, `SELECT name, version, deleted_at IS NOT NULL FROM env_secrets
		WHERE env_id = $1 AND NOT system ORDER BY name, version`, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SecretVersions{}
	for rows.Next() {
		var name string
		var version int
		var deleted bool
		if err := rows.Scan(&name, &version, &deleted); err != nil {
			return nil, err
		}
		v := out[name]
		if deleted {
			v.Deleted = append(v.Deleted, version)
		} else {
			v.Versions = append(v.Versions, version)
		}
		out[name] = v
	}
	return out, rows.Err()
}

// LatestJobRuns returns each job's newest run of the environment, by job.
func (s *Store) LatestJobRuns(ctx context.Context, envID uuid.UUID) ([]EnvJobRun, error) {
	return collectJobRuns(s.q.Query(ctx, "SELECT DISTINCT ON (job) "+jobRunCols+` FROM env_job_runs
		WHERE env_id = $1 ORDER BY job, id DESC`, envID))
}

// AttachedSpecRow is the latest release's spec of an environment that
// has an attached service in a sandbox.
type AttachedSpecRow struct {
	EnvID   uuid.UUID
	EnvName string
	Spec    json.RawMessage
}

// AttachedSpecs returns the latest releases' specs of owner's
// environments but exclude (uuid.Nil: none) that are not being deleted
// and have an attached service in one of the sandboxes (their IDs as
// the specs write them, lowercase), by environment name.
func (s *Store) AttachedSpecs(ctx context.Context, owner string, exclude uuid.UUID, sandboxIDs []string) ([]AttachedSpecRow, error) {
	if len(sandboxIDs) == 0 {
		return nil, nil
	}
	rows, err := s.q.Query(ctx, `SELECT e.id, e.name, r.spec FROM environments e
		JOIN env_releases r ON r.env_id = e.id AND r.number = e.release
		WHERE e.owner = $1 AND e.id <> $2 AND NOT e.deleting
			AND EXISTS (SELECT 1 FROM jsonb_each(coalesce(r.spec->'services', '{}'::jsonb)) svc
				WHERE svc.value->>'runtime' = 'attached' AND lower(svc.value->>'sandbox') = ANY($3))
		ORDER BY e.name`, owner, exclude, sandboxIDs)
	if err != nil {
		return nil, fmt.Errorf("attached specs: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AttachedSpecRow, error) {
		var out AttachedSpecRow
		var spec []byte
		err := r.Scan(&out.EnvID, &out.EnvName, &spec)
		out.Spec = json.RawMessage(spec)
		return out, err
	})
}
