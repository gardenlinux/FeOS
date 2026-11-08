// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
)

// EnvDeployment is a row of env_deployments: a numbered, immutable desired state
// of an environment. Only its state, reason and times change.
type EnvDeployment struct {
	EnvID  uuid.UUID
	Number int
	// Spec is the normalised deployment spec; nil in ListDeployments.
	Spec            json.RawMessage
	Base            int // the deployment it was made on; 0: none
	Author, Message string
	Annotations     map[string]string
	Origin          string // model.Origin*
	OriginRef       string
	State           string // model.Deployment*
	Reason          string
	StartedAt       time.Time // zero: NULL
	FinishedAt      time.Time // zero: NULL
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// MadeBy is the principal that made it (ADR 0031), beside the
	// client's Author; "": ferund.
	MadeBy string
}

const deploymentSummaryCols = `env_id, number, base, author, message, annotations, origin, origin_ref, state, reason,
	started_at, finished_at, created_at, updated_at, made_by`

// deploymentCols are the summary's columns and the spec, last.
const deploymentCols = deploymentSummaryCols + ", spec"

func scanDeployment(row pgx.Row, withSpec bool) (EnvDeployment, error) {
	var r EnvDeployment
	var started, finished *time.Time
	dst := []any{&r.EnvID, &r.Number, &r.Base, &r.Author, &r.Message, &r.Annotations, &r.Origin, &r.OriginRef,
		&r.State, &r.Reason, &started, &finished, &r.CreatedAt, &r.UpdatedAt, &r.MadeBy}
	var spec []byte
	if withSpec {
		dst = append(dst, &spec)
	}
	if err := row.Scan(dst...); err != nil {
		return EnvDeployment{}, err
	}
	r.StartedAt, r.FinishedAt = timeOf(started), timeOf(finished)
	r.Annotations = stringMap(r.Annotations)
	if withSpec {
		r.Spec = json.RawMessage(spec)
	}
	return r, nil
}

// InsertDeployment inserts a deployment (state waiting when empty); the caller
// holds its environment FOR UPDATE and sets environments.deployment with it.
// A number the environment has is ErrExists, an unknown environment
// ErrNoRow.
func (s *Store) InsertDeployment(ctx context.Context, r EnvDeployment) (EnvDeployment, error) {
	if r.State == "" {
		r.State = model.DeploymentWaiting
	}
	ann, err := jsonText(stringMap(r.Annotations))
	if err != nil {
		return EnvDeployment{}, fmt.Errorf("insert deployment %d: %w", r.Number, err)
	}
	out, err := scanDeployment(s.q.QueryRow(ctx, `INSERT INTO env_deployments (env_id, number, spec, base, author, message,
			annotations, origin, origin_ref, state, reason, started_at, finished_at, made_by)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13, $14) RETURNING `+deploymentCols,
		r.EnvID, r.Number, string(r.Spec), r.Base, r.Author, r.Message, ann, r.Origin, r.OriginRef, r.State, r.Reason,
		nullTime(r.StartedAt), nullTime(r.FinishedAt), r.MadeBy), true)
	if err != nil {
		return EnvDeployment{}, insertError(fmt.Sprintf("insert deployment %d of environment %s", r.Number, r.EnvID), err)
	}
	return out, nil
}

// GetDeployment reads a deployment with its spec; ErrNoRow if there is none.
func (s *Store) GetDeployment(ctx context.Context, envID uuid.UUID, number int) (EnvDeployment, error) {
	return s.LockDeployment(ctx, envID, number, NoLock)
}

// LockDeployment reads a deployment with its spec and locks its row with lock;
// ErrNoRow if there is none.
func (s *Store) LockDeployment(ctx context.Context, envID uuid.UUID, number int, lock RowLock) (EnvDeployment, error) {
	r, err := scanDeployment(s.q.QueryRow(ctx, "SELECT "+deploymentCols+` FROM env_deployments
		WHERE env_id = $1 AND number = $2 `+string(lock), envID, number), true)
	if err != nil {
		return EnvDeployment{}, noRow(err, ErrNoRow)
	}
	return r, nil
}

// ListDeployments returns up to limit deployments of the environment without
// their specs, newest first.
func (s *Store) ListDeployments(ctx context.Context, envID uuid.UUID, limit int) ([]EnvDeployment, error) {
	rows, err := s.q.Query(ctx, "SELECT "+deploymentSummaryCols+` FROM env_deployments
		WHERE env_id = $1 ORDER BY number DESC LIMIT $2`, envID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvDeployment, error) { return scanDeployment(r, false) })
}

// UpdateDeployment writes the state, reason, started_at and finished_at in
// which after differs from before (updated_at = now()); every other column
// is immutable. When nothing differs, before is returned unwritten.
func (s *Store) UpdateDeployment(ctx context.Context, before, after EnvDeployment) (EnvDeployment, error) {
	what := fmt.Sprintf("deployment %d of environment %s", before.Number, before.EnvID)
	if err := fixed(what, map[string]bool{"env_id": before.EnvID != after.EnvID, "number": before.Number != after.Number,
		"base": before.Base != after.Base, "author": before.Author != after.Author, "message": before.Message != after.Message,
		"annotations": !maps.Equal(before.Annotations, after.Annotations), "origin": before.Origin != after.Origin,
		"origin_ref": before.OriginRef != after.OriginRef}); err != nil {
		return EnvDeployment{}, err
	}
	u := newUpdates(before.EnvID, before.Number)
	u.set("state", before.State != after.State, after.State)
	u.set("reason", before.Reason != after.Reason, after.Reason)
	u.set("started_at", !before.StartedAt.Equal(after.StartedAt), nullTime(after.StartedAt))
	u.set("finished_at", !before.FinishedAt.Equal(after.FinishedAt), nullTime(after.FinishedAt))
	if u.empty() {
		return before, nil
	}
	r, err := scanDeployment(s.q.QueryRow(ctx, "UPDATE env_deployments SET "+u.clause(true)+
		" WHERE env_id = $1 AND number = $2 RETURNING "+deploymentCols, u.args...), true)
	if err != nil {
		return EnvDeployment{}, fmt.Errorf("update %s: %w", what, noRow(err, ErrNoRow))
	}
	return r, nil
}

// ReplaceDeployments marks the deployments of the environment numbered below
// below whose state is one of states replaced (finished_at = now() where
// unset) and returns their numbers, ascending.
func (s *Store) ReplaceDeployments(ctx context.Context, envID uuid.UUID, below int, states ...string) ([]int, error) {
	rows, err := s.q.Query(ctx, `UPDATE env_deployments SET state = 'replaced', finished_at = coalesce(finished_at, now()),
			updated_at = now()
		WHERE env_id = $1 AND number < $2 AND state = ANY($3) RETURNING number`, envID, below, nonNil(states))
	if err != nil {
		return nil, err
	}
	nums, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, err
	}
	slices.Sort(nums)
	return nums, nil
}

// EnvTag is a row of env_tags: a movable name of a deployment.
type EnvTag struct {
	// MadeBy is the principal that set it last (ADR 0031); written, not
	// read back.
	MadeBy  string
	EnvID   uuid.UUID
	Tag     string
	Number  int
	MovedBy string
	MovedAt time.Time
}

const tagCols = "env_id, tag, number, moved_by, moved_at"

func scanTag(row pgx.Row) (EnvTag, error) {
	var t EnvTag
	err := row.Scan(&t.EnvID, &t.Tag, &t.Number, &t.MovedBy, &t.MovedAt)
	return t, err
}

// PutTag points a tag at a deployment (moved_at = now()), creating or moving
// it; a deployment the environment does not have is ErrNoRow.
func (s *Store) PutTag(ctx context.Context, t EnvTag) (EnvTag, error) {
	out, err := scanTag(s.q.QueryRow(ctx, `INSERT INTO env_tags (env_id, tag, number, moved_by, made_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (env_id, tag) DO UPDATE SET number = excluded.number, moved_by = excluded.moved_by,
			made_by = excluded.made_by, moved_at = now()
		RETURNING `+tagCols, t.EnvID, t.Tag, t.Number, t.MovedBy, t.MadeBy))
	if err != nil {
		return EnvTag{}, insertError(fmt.Sprintf("put tag %s of environment %s", t.Tag, t.EnvID), err)
	}
	return out, nil
}

// GetTag reads a tag; ErrNoRow if there is none.
func (s *Store) GetTag(ctx context.Context, envID uuid.UUID, tag string) (EnvTag, error) {
	t, err := scanTag(s.q.QueryRow(ctx, "SELECT "+tagCols+" FROM env_tags WHERE env_id = $1 AND tag = $2", envID, tag))
	if err != nil {
		return EnvTag{}, noRow(err, ErrNoRow)
	}
	return t, nil
}

// ListTags returns the environment's tags by name.
func (s *Store) ListTags(ctx context.Context, envID uuid.UUID) ([]EnvTag, error) {
	rows, err := s.q.Query(ctx, "SELECT "+tagCols+" FROM env_tags WHERE env_id = $1 ORDER BY tag", envID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvTag, error) { return scanTag(r) })
}

// DeleteTag removes a tag and reports whether it was there.
func (s *Store) DeleteTag(ctx context.Context, envID uuid.UUID, tag string) (bool, error) {
	ct, err := s.q.Exec(ctx, "DELETE FROM env_tags WHERE env_id = $1 AND tag = $2", envID, tag)
	return ct.RowsAffected() > 0, err
}
