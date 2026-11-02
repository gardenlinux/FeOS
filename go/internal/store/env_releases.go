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

// EnvRelease is a row of env_releases: a numbered, immutable desired state
// of an environment. Only its state, reason and times change.
type EnvRelease struct {
	EnvID  uuid.UUID
	Number int
	// Spec is the normalised release spec; nil in ListReleases.
	Spec            json.RawMessage
	Base            int // the release it was made on; 0: none
	Author, Message string
	Annotations     map[string]string
	Origin          string // model.Origin*
	OriginRef       string
	State           string // model.Release*
	Reason          string
	StartedAt       time.Time // zero: NULL
	FinishedAt      time.Time // zero: NULL
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

const releaseSummaryCols = `env_id, number, base, author, message, annotations, origin, origin_ref, state, reason,
	started_at, finished_at, created_at, updated_at`

// releaseCols are the summary's columns and the spec, last.
const releaseCols = releaseSummaryCols + ", spec"

func scanRelease(row pgx.Row, withSpec bool) (EnvRelease, error) {
	var r EnvRelease
	var started, finished *time.Time
	dst := []any{&r.EnvID, &r.Number, &r.Base, &r.Author, &r.Message, &r.Annotations, &r.Origin, &r.OriginRef,
		&r.State, &r.Reason, &started, &finished, &r.CreatedAt, &r.UpdatedAt}
	var spec []byte
	if withSpec {
		dst = append(dst, &spec)
	}
	if err := row.Scan(dst...); err != nil {
		return EnvRelease{}, err
	}
	r.StartedAt, r.FinishedAt = timeOf(started), timeOf(finished)
	r.State = currentWord(earlierReleaseStates, r.State)
	r.Annotations = stringMap(r.Annotations)
	if withSpec {
		r.Spec = json.RawMessage(spec)
	}
	return r, nil
}

// InsertRelease inserts a release (state waiting when empty); the caller
// holds its environment FOR UPDATE and sets environments.release with it.
// A number the environment has is ErrExists, an unknown environment
// ErrNoRow.
func (s *Store) InsertRelease(ctx context.Context, r EnvRelease) (EnvRelease, error) {
	if r.State == "" {
		r.State = model.ReleaseWaiting
	}
	ann, err := jsonText(stringMap(r.Annotations))
	if err != nil {
		return EnvRelease{}, fmt.Errorf("insert release %d: %w", r.Number, err)
	}
	out, err := scanRelease(s.q.QueryRow(ctx, `INSERT INTO env_releases (env_id, number, spec, base, author, message,
			annotations, origin, origin_ref, state, reason, started_at, finished_at)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13) RETURNING `+releaseCols,
		r.EnvID, r.Number, string(r.Spec), r.Base, r.Author, r.Message, ann, r.Origin, r.OriginRef,
		storedWord(earlierReleaseStates, r.State), r.Reason,
		nullTime(r.StartedAt), nullTime(r.FinishedAt)), true)
	if err != nil {
		return EnvRelease{}, insertError(fmt.Sprintf("insert release %d of environment %s", r.Number, r.EnvID), err)
	}
	return out, nil
}

// GetRelease reads a release with its spec; ErrNoRow if there is none.
func (s *Store) GetRelease(ctx context.Context, envID uuid.UUID, number int) (EnvRelease, error) {
	return s.LockRelease(ctx, envID, number, NoLock)
}

// LockRelease reads a release with its spec and locks its row with lock;
// ErrNoRow if there is none.
func (s *Store) LockRelease(ctx context.Context, envID uuid.UUID, number int, lock RowLock) (EnvRelease, error) {
	r, err := scanRelease(s.q.QueryRow(ctx, "SELECT "+releaseCols+` FROM env_releases
		WHERE env_id = $1 AND number = $2 `+string(lock), envID, number), true)
	if err != nil {
		return EnvRelease{}, noRow(err, ErrNoRow)
	}
	return r, nil
}

// ListReleases returns up to limit releases of the environment without
// their specs, newest first.
func (s *Store) ListReleases(ctx context.Context, envID uuid.UUID, limit int) ([]EnvRelease, error) {
	rows, err := s.q.Query(ctx, "SELECT "+releaseSummaryCols+` FROM env_releases
		WHERE env_id = $1 ORDER BY number DESC LIMIT $2`, envID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvRelease, error) { return scanRelease(r, false) })
}

// UpdateRelease writes the state, reason, started_at and finished_at in
// which after differs from before (updated_at = now()); every other column
// is immutable. When nothing differs, before is returned unwritten.
func (s *Store) UpdateRelease(ctx context.Context, before, after EnvRelease) (EnvRelease, error) {
	what := fmt.Sprintf("release %d of environment %s", before.Number, before.EnvID)
	if err := fixed(what, map[string]bool{"env_id": before.EnvID != after.EnvID, "number": before.Number != after.Number,
		"base": before.Base != after.Base, "author": before.Author != after.Author, "message": before.Message != after.Message,
		"annotations": !maps.Equal(before.Annotations, after.Annotations), "origin": before.Origin != after.Origin,
		"origin_ref": before.OriginRef != after.OriginRef}); err != nil {
		return EnvRelease{}, err
	}
	u := newUpdates(before.EnvID, before.Number)
	u.set("state", before.State != after.State, storedWord(earlierReleaseStates, after.State))
	u.set("reason", before.Reason != after.Reason, after.Reason)
	u.set("started_at", !before.StartedAt.Equal(after.StartedAt), nullTime(after.StartedAt))
	u.set("finished_at", !before.FinishedAt.Equal(after.FinishedAt), nullTime(after.FinishedAt))
	if u.empty() {
		return before, nil
	}
	r, err := scanRelease(s.q.QueryRow(ctx, "UPDATE env_releases SET "+u.clause(true)+
		" WHERE env_id = $1 AND number = $2 RETURNING "+releaseCols, u.args...), true)
	if err != nil {
		return EnvRelease{}, fmt.Errorf("update %s: %w", what, noRow(err, ErrNoRow))
	}
	return r, nil
}

// ReplaceReleases marks the releases of the environment numbered below
// below whose state is one of states replaced (finished_at = now() where
// unset) and returns their numbers, ascending.
func (s *Store) ReplaceReleases(ctx context.Context, envID uuid.UUID, below int, states ...string) ([]int, error) {
	rows, err := s.q.Query(ctx, `UPDATE env_releases SET state = $4, finished_at = coalesce(finished_at, now()),
			updated_at = now()
		WHERE env_id = $1 AND number < $2 AND state = ANY($3) RETURNING number`, envID, below,
		bothWords(earlierReleaseStates, states), storedWord(earlierReleaseStates, model.ReleaseReplaced))
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

// EnvTag is a row of env_tags: a movable name of a release.
type EnvTag struct {
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

// PutTag points a tag at a release (moved_at = now()), creating or moving
// it; a release the environment does not have is ErrNoRow.
func (s *Store) PutTag(ctx context.Context, t EnvTag) (EnvTag, error) {
	out, err := scanTag(s.q.QueryRow(ctx, `INSERT INTO env_tags (env_id, tag, number, moved_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (env_id, tag) DO UPDATE SET number = excluded.number, moved_by = excluded.moved_by, moved_at = now()
		RETURNING `+tagCols, t.EnvID, t.Tag, t.Number, t.MovedBy))
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
