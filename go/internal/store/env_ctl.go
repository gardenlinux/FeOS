// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// The environment controller's queries (internal/env/ctl) beyond the
// basic reads and writes of the env_*.go files.

// attachedUsable is the condition under which an attached service a of
// environment e may use the sandbox s it names (§13.6): s is a sandbox of
// e's owner and not an instance row. A sandbox of the owner deleted and
// created again under the same ID (FeCode replaces a lost workspace
// sandbox so) is a new incarnation the service starts in anew; one of
// another tenant under that ID is never used.
const attachedUsable = `s.owner = e.owner AND s.environment_id IS NULL AND NOT s.tenant_job`

// EnvCtlMembers returns the members of an environment's security group:
// the addresses of its instance rows (any state, until the row is gone)
// and of the sandboxes its attached services may use (attachedUsable),
// without the empty ones, distinct and sorted. The caller keeps the IPv6
// ones.
func (s *Store) EnvCtlMembers(ctx context.Context, envID uuid.UUID) ([]string, error) {
	rows, err := s.q.Query(ctx, `SELECT address FROM sandboxes WHERE environment_id = $1 AND address <> ''
		UNION SELECT s.address FROM env_attached a JOIN environments e ON e.id = a.env_id
			JOIN sandboxes s ON s.id = a.sandbox_id
			WHERE a.env_id = $1 AND s.address <> '' AND `+attachedUsable+`
		ORDER BY 1`, envID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// EnvCtlSandboxes returns the instance rows of an environment (rows of
// sandboxes with its environment_id), also those without an
// env_instances row, by id.
func (s *Store) EnvCtlSandboxes(ctx context.Context, envID uuid.UUID) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxCols+" FROM sandboxes WHERE environment_id = $1 ORDER BY id", envID))
}

// EnvCtlCloseIncidents closes the environment's open incidents of one of
// conditions whose deployment is below below, and returns how many.
func (s *Store) EnvCtlCloseIncidents(ctx context.Context, envID uuid.UUID, below int, conditions ...string) (int64, error) {
	ct, err := s.q.Exec(ctx, `UPDATE env_incidents SET closed_at = now()
		WHERE env_id = $1 AND deployment < $2 AND condition = ANY($3) AND closed_at IS NULL`, envID, below, nonNil(conditions))
	return ct.RowsAffected(), err
}

// EnvCtlCloseObjectIncidents closes the open incidents of condition of an
// object of the environment ("<kind>/<name>"), of any deployment, and
// returns how many.
func (s *Store) EnvCtlCloseObjectIncidents(ctx context.Context, envID uuid.UUID, object, condition string) (int64, error) {
	ct, err := s.q.Exec(ctx, `UPDATE env_incidents SET closed_at = now()
		WHERE env_id = $1 AND object = $2 AND condition = $3 AND closed_at IS NULL`, envID, object, condition)
	return ct.RowsAffected(), err
}

// EnvCtlSetPin writes a row's pin_node (not a lifecycle column: its gen
// stays) and reports whether the row was there.
func (s *Store) EnvCtlSetPin(ctx context.Context, id podid.ID, node string) (bool, error) {
	ct, err := s.q.Exec(ctx, `UPDATE sandboxes SET pin_node = $2, updated_at = now() WHERE id = $1 AND pin_node <> $2`,
		id, node)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// EnvCtlSetIdle writes a row's idle_after_s (not a lifecycle column) and
// reports whether it changed.
func (s *Store) EnvCtlSetIdle(ctx context.Context, id podid.ID, idleAfterS int) (bool, error) {
	ct, err := s.q.Exec(ctx, `UPDATE sandboxes SET idle_after_s = $2, updated_at = now()
		WHERE id = $1 AND idle_after_s IS DISTINCT FROM $2`, id, idleAfterS)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// EnvCtlMarkVolumesDeleting marks every volume of the environment
// deleting and returns how many it marked.
func (s *Store) EnvCtlMarkVolumesDeleting(ctx context.Context, envID uuid.UUID) (int64, error) {
	ct, err := s.q.Exec(ctx, `UPDATE env_volumes SET deleting = true, updated_at = now() WHERE env_id = $1 AND NOT deleting`,
		envID)
	return ct.RowsAffected(), err
}

// EnvCtlVolumesByID returns the volumes of the ids that have a row, by
// volume ID.
func (s *Store) EnvCtlVolumesByID(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]EnvVolume, error) {
	out := map[uuid.UUID]EnvVolume{}
	if len(ids) == 0 {
		return out, nil
	}
	vols, err := collectEnvVolumes(s.q.Query(ctx, "SELECT "+envVolumeCols+" FROM env_volumes WHERE volume_id = ANY($1)", ids))
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		out[v.VolumeID] = v
	}
	return out, nil
}

// EnvCtlTouchVolume sets a volume's updated_at to now(), so that one that
// waits goes to the end of DeletingEnvVolumes's order; ErrNoRow when it is
// gone.
func (s *Store) EnvCtlTouchVolume(ctx context.Context, envID uuid.UUID, object string) error {
	ct, err := s.q.Exec(ctx, `UPDATE env_volumes SET updated_at = now() WHERE env_id = $1 AND object = $2`, envID, object)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNoRow
	}
	return nil
}

// EnvCtlTouchEnvironment sets a deleting environment's updated_at to
// now(), so that one whose deletion has to wait goes to the end of
// DeletingEnvironments's order; ErrNoRow when it is gone or not deleting.
func (s *Store) EnvCtlTouchEnvironment(ctx context.Context, id uuid.UUID) error {
	ct, err := s.q.Exec(ctx, `UPDATE environments SET updated_at = now() WHERE id = $1 AND deleting`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNoRow
	}
	return nil
}

// EnvCtlStarved returns the instance rows of the environment that have
// been pending without capacity (a description beginning "no capacity")
// for at least after, by the database's clock: their ids and
// descriptions.
func (s *Store) EnvCtlStarved(ctx context.Context, envID uuid.UUID, after time.Duration) (map[podid.ID]string, error) {
	rows, err := s.q.Query(ctx, `SELECT id, status_description FROM sandboxes
		WHERE environment_id = $1 AND client_state = 'pending' AND op = '' AND status_description LIKE 'no capacity%'
			AND coalesce(pending_since, created_at) <= now() - make_interval(secs => $2)`, envID, after.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[podid.ID]string{}
	for rows.Next() {
		var id podid.ID
		var desc string
		if err := rows.Scan(&id, &desc); err != nil {
			return nil, err
		}
		out[id] = desc
	}
	return out, rows.Err()
}

// EnvCtlRunningJobRuns returns the environment's runs that are running,
// oldest first.
func (s *Store) EnvCtlRunningJobRuns(ctx context.Context, envID uuid.UUID) ([]EnvJobRun, error) {
	return collectJobRuns(s.q.Query(ctx, "SELECT "+jobRunCols+` FROM env_job_runs
		WHERE env_id = $1 AND phase = 'running' ORDER BY id`, envID))
}

// EnvCtlJobWaitedUntil is when a deployment's wait for its job ended: the
// latest end of the job's runs of other deployments that ended after since
// (the deployment's start) or still run, now() for those; zero when there
// is none, or when a run of the deployment's template hash had succeeded by
// since, so that the deployment never waited for the job ("": the hash is
// not known, and the runs count).
func (s *Store) EnvCtlJobWaitedUntil(ctx context.Context, envID uuid.UUID, job, hash string, deployment int,
	since time.Time) (time.Time, error) {
	var until *time.Time
	err := s.q.QueryRow(ctx, `SELECT max(COALESCE(finished_at, now())) FROM env_job_runs
		WHERE env_id = $1 AND job = $2 AND deployment <> $4 AND COALESCE(finished_at, now()) > $5
			AND NOT EXISTS (SELECT 1 FROM env_job_runs o WHERE o.env_id = $1 AND o.job = $2
				AND o.template_hash = $3 AND o.phase = 'succeeded' AND o.finished_at <= $5)`,
		envID, job, hash, deployment, since).Scan(&until)
	return timeOf(until), err
}

// EnvCtlWithAttached returns the IDs of the environments whose attached
// services run in the sandbox and may use it (attachedUsable), sorted.
func (s *Store) EnvCtlWithAttached(ctx context.Context, sandboxID podid.ID) ([]uuid.UUID, error) {
	rows, err := s.q.Query(ctx, `SELECT DISTINCT a.env_id FROM env_attached a JOIN environments e ON e.id = a.env_id
			JOIN sandboxes s ON s.id = a.sandbox_id
		WHERE a.sandbox_id = $1 AND `+attachedUsable+` ORDER BY a.env_id`, sandboxID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// AttachedName is an attached service of an environment, by name.
type AttachedName struct {
	EnvID   uuid.UUID
	Service string
}

// EnvCtlAttachedIn returns the attached services that run in the sandbox
// and may use it (attachedUsable), by environment and name: the names of
// its name table.
func (s *Store) EnvCtlAttachedIn(ctx context.Context, sandboxID podid.ID) ([]AttachedName, error) {
	rows, err := s.q.Query(ctx, `SELECT a.env_id, a.service FROM env_attached a JOIN environments e ON e.id = a.env_id
			JOIN sandboxes s ON s.id = a.sandbox_id
		WHERE a.sandbox_id = $1 AND `+attachedUsable+` ORDER BY a.env_id, a.service`, sandboxID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[AttachedName])
}

// EnvCtlTemplatedOnNode returns the rows with a pod template whose pod is
// on node, or is being brought there by an op: instance rows and
// attachable sandboxes, by id.
func (s *Store) EnvCtlTemplatedOnNode(ctx context.Context, node string) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxCols+` FROM sandboxes
		WHERE pod_template IS NOT NULL AND (node = $1 OR op_target = $1 OR lost_from = $1) ORDER BY id`, node))
}

// EnvCtlExisting returns which of the ids are environments (exists) and
// which are sandboxes (sandboxes).
func (s *Store) EnvCtlExisting(ctx context.Context, envs []uuid.UUID, sandboxes []podid.ID) (map[uuid.UUID]bool,
	map[podid.ID]bool, error) {
	e, err := existing(ctx, s.q, "SELECT id FROM environments WHERE id = ANY($1)", envs)
	if err != nil {
		return nil, nil, err
	}
	sb, err := existing(ctx, s.q, "SELECT id FROM sandboxes WHERE id = ANY($1)", sandboxes)
	if err != nil {
		return nil, nil, err
	}
	return e, sb, nil
}

// IngressInput is what the group ing-<sandbox id> of a sandbox is built
// from (EnvCtlIngressInputs): the instance the sandbox is, as its
// env_instances row has it; EnvID uuid.Nil when it has none.
type IngressInput struct {
	EnvID      uuid.UUID
	Kind       string // "service", "addon", "job"
	Object     string
	Deployment int
}

// EnvCtlIngressInputs returns, for each of the ids that is a sandbox, what
// its group ing-<id> is built from; an ID that is no sandbox is missing
// from the map.
func (s *Store) EnvCtlIngressInputs(ctx context.Context, ids []podid.ID) (map[podid.ID]IngressInput, error) {
	out := map[podid.ID]IngressInput{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.q.Query(ctx, `SELECT s.id, i.env_id, coalesce(i.kind, ''), coalesce(i.object, ''),
			coalesce(i.deployment, 0)
		FROM sandboxes s LEFT JOIN env_instances i ON i.sandbox_id = s.id WHERE s.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id podid.ID
		var envID *uuid.UUID
		var in IngressInput
		if err := rows.Scan(&id, &envID, &in.Kind, &in.Object, &in.Deployment); err != nil {
			return nil, err
		}
		in.EnvID = uuidOf(envID)
		out[id] = in
	}
	return out, rows.Err()
}

// EnvCtlSandboxNodes returns the node of each of the ids that is a
// sandbox ("" while it has none); an ID that is no sandbox is missing from
// the map.
func (s *Store) EnvCtlSandboxNodes(ctx context.Context, ids []podid.ID) (map[podid.ID]string, error) {
	out := map[podid.ID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.q.Query(ctx, "SELECT id, coalesce(node, '') FROM sandboxes WHERE id = ANY($1)", ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id podid.ID
		var node string
		if err := rows.Scan(&id, &node); err != nil {
			return nil, err
		}
		out[id] = node
	}
	return out, rows.Err()
}

// existing runs sql, which selects those of the ids ($1) that have a row,
// and returns them as a set.
func existing[T comparable](ctx context.Context, q db.Querier, sql string, ids []T) (map[T]bool, error) {
	out := map[T]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, sql, ids)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[T])
	if err != nil {
		return nil, err
	}
	for _, id := range found {
		out[id] = true
	}
	return out, nil
}
