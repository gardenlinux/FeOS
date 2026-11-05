// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EnvIncident is a row of env_incidents: a condition of an object that
// needs a look. At most one is open per (environment, object, deployment,
// condition).
type EnvIncident struct {
	ID         int64
	EnvID      uuid.UUID
	Object     string // "<kind>/<name>"
	Deployment int
	Condition  string // model.Condition*
	OpenedAt   time.Time
	ClosedAt   time.Time // zero: open
	Summary    string
	// Evidence is a JSON object ({exits, logTail, diff, placement}); nil
	// is {}.
	Evidence json.RawMessage
}

const incidentCols = "id, env_id, object, deployment, condition, opened_at, closed_at, summary, evidence"

func scanIncident(row pgx.Row) (EnvIncident, error) {
	var i EnvIncident
	var closed *time.Time
	var evidence []byte
	if err := row.Scan(&i.ID, &i.EnvID, &i.Object, &i.Deployment, &i.Condition, &i.OpenedAt, &closed, &i.Summary,
		&evidence); err != nil {
		return EnvIncident{}, err
	}
	i.ClosedAt, i.Evidence = timeOf(closed), json.RawMessage(evidence)
	return i, nil
}

func collectIncidents(rows pgx.Rows, err error) ([]EnvIncident, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvIncident, error) { return scanIncident(r) })
}

func evidenceText(e json.RawMessage) string {
	if len(e) == 0 {
		return "{}"
	}
	return string(e)
}

// OpenIncident opens an incident unless one is open for its environment,
// object, deployment and condition: it returns the new one and true, or the
// open one (as it is) and false. An unknown environment is ErrNoRow.
func (s *Store) OpenIncident(ctx context.Context, i EnvIncident) (EnvIncident, bool, error) {
	for range 3 {
		out, err := scanIncident(s.q.QueryRow(ctx, `INSERT INTO env_incidents (env_id, object, deployment, condition, summary,
				evidence)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb)
			ON CONFLICT (env_id, object, deployment, condition) WHERE closed_at IS NULL DO NOTHING RETURNING `+incidentCols,
			i.EnvID, i.Object, i.Deployment, i.Condition, i.Summary, evidenceText(i.Evidence)))
		if err == nil {
			return out, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return EnvIncident{}, false, insertError("open incident "+i.Condition+" of "+i.Object, err)
		}
		out, err = scanIncident(s.q.QueryRow(ctx, "SELECT "+incidentCols+` FROM env_incidents
			WHERE env_id = $1 AND object = $2 AND deployment = $3 AND condition = $4 AND closed_at IS NULL`,
			i.EnvID, i.Object, i.Deployment, i.Condition))
		if err == nil {
			return out, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return EnvIncident{}, false, err
		}
		// Closed between the insert and the read: open it again.
	}
	return EnvIncident{}, false, fmt.Errorf("open incident %s of %s: it keeps being closed", i.Condition, i.Object)
}

// GetIncident reads an incident of the environment; ErrNoRow if there is
// none.
func (s *Store) GetIncident(ctx context.Context, envID uuid.UUID, id int64) (EnvIncident, error) {
	i, err := scanIncident(s.q.QueryRow(ctx, "SELECT "+incidentCols+" FROM env_incidents WHERE env_id = $1 AND id = $2",
		envID, id))
	if err != nil {
		return EnvIncident{}, noRow(err, ErrNoRow)
	}
	return i, nil
}

// LockIncident reads an incident and locks it with lock; ErrNoRow if
// there is none.
func (s *Store) LockIncident(ctx context.Context, id int64, lock RowLock) (EnvIncident, error) {
	i, err := scanIncident(s.q.QueryRow(ctx, "SELECT "+incidentCols+" FROM env_incidents WHERE id = $1 "+string(lock), id))
	if err != nil {
		return EnvIncident{}, noRow(err, ErrNoRow)
	}
	return i, nil
}

// ListIncidents returns the environment's incidents (only the open ones
// with open), newest first.
func (s *Store) ListIncidents(ctx context.Context, envID uuid.UUID, open bool) ([]EnvIncident, error) {
	return collectIncidents(s.q.Query(ctx, "SELECT "+incidentCols+` FROM env_incidents
		WHERE env_id = $1 AND (NOT $2 OR closed_at IS NULL) ORDER BY id DESC`, envID, open))
}

// CountOpenIncidents counts the environment's open incidents.
func (s *Store) CountOpenIncidents(ctx context.Context, envID uuid.UUID) (int64, error) {
	var n int64
	err := s.q.QueryRow(ctx, "SELECT count(*) FROM env_incidents WHERE env_id = $1 AND closed_at IS NULL", envID).Scan(&n)
	return n, err
}

// UpdateIncident writes the summary, evidence and closed_at in which after
// differs from before; the other columns are refused. When nothing
// differs, before is returned unwritten.
func (s *Store) UpdateIncident(ctx context.Context, before, after EnvIncident) (EnvIncident, error) {
	what := fmt.Sprintf("incident %d", before.ID)
	if err := fixed(what, map[string]bool{"id": before.ID != after.ID, "env_id": before.EnvID != after.EnvID,
		"object": before.Object != after.Object, "deployment": before.Deployment != after.Deployment,
		"condition": before.Condition != after.Condition, "opened_at": !before.OpenedAt.Equal(after.OpenedAt)}); err != nil {
		return EnvIncident{}, err
	}
	u := newUpdates(before.ID)
	u.set("summary", before.Summary != after.Summary, after.Summary)
	if !sameJSON(before.Evidence, after.Evidence) {
		u.args = append(u.args, evidenceText(after.Evidence))
		u.sets = append(u.sets, fmt.Sprintf("evidence = $%d::jsonb", len(u.args)))
	}
	u.set("closed_at", !before.ClosedAt.Equal(after.ClosedAt), nullTime(after.ClosedAt))
	if u.empty() {
		return before, nil
	}
	i, err := scanIncident(s.q.QueryRow(ctx, "UPDATE env_incidents SET "+u.clause(false)+" WHERE id = $1 RETURNING "+incidentCols,
		u.args...))
	if err != nil {
		return EnvIncident{}, fmt.Errorf("update %s: %w", what, noRow(err, ErrNoRow))
	}
	return i, nil
}

// sameJSON reports whether two JSON texts are equal after compaction (an
// empty one is {}).
func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, []byte(evidenceText(a))) != nil || json.Compact(&cb, []byte(evidenceText(b))) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// CloseIncident closes an open incident (closed_at = now()) and reports
// whether it was open.
func (s *Store) CloseIncident(ctx context.Context, id int64) (bool, error) {
	ct, err := s.q.Exec(ctx, "UPDATE env_incidents SET closed_at = now() WHERE id = $1 AND closed_at IS NULL", id)
	return ct.RowsAffected() > 0, err
}

// HoldIncidents records that the open incidents of condition of an object
// of the environment still hold, now by the database's clock (held_at),
// and returns how many.
func (s *Store) HoldIncidents(ctx context.Context, envID uuid.UUID, object, condition string) (int64, error) {
	ct, err := s.q.Exec(ctx, `UPDATE env_incidents SET held_at = now()
		WHERE env_id = $1 AND object = $2 AND condition = $3 AND closed_at IS NULL`, envID, object, condition)
	return ct.RowsAffected(), err
}

// CloseUnheldIncidents closes the open incidents of condition of an object
// of the environment, of any deployment, whose condition was last seen to
// hold (held_at, else opened_at) clean or longer ago by the database's
// clock, and returns how many; clean 0 closes them all.
func (s *Store) CloseUnheldIncidents(ctx context.Context, envID uuid.UUID, object, condition string,
	clean time.Duration) (int64, error) {
	ct, err := s.q.Exec(ctx, `UPDATE env_incidents SET closed_at = now()
		WHERE env_id = $1 AND object = $2 AND condition = $3 AND closed_at IS NULL
			AND coalesce(held_at, opened_at) <= now() - $4::interval`, envID, object, condition, interval(clean))
	return ct.RowsAffected(), err
}

// ReopenIncident opens again the newest incident of i's environment,
// object, deployment and condition that closed within the last within by
// the database's clock, unless one of them is open: with i's summary and
// evidence, held now. It reports whether it reopened one.
func (s *Store) ReopenIncident(ctx context.Context, i EnvIncident, within time.Duration) (bool, error) {
	ct, err := s.q.Exec(ctx, `UPDATE env_incidents SET closed_at = NULL, held_at = now(), summary = $5,
			evidence = $6::jsonb
		WHERE id = (SELECT id FROM env_incidents WHERE env_id = $1 AND object = $2 AND deployment = $3 AND condition = $4
				AND closed_at > now() - $7::interval ORDER BY closed_at DESC, id DESC LIMIT 1)
			AND NOT EXISTS (SELECT 1 FROM env_incidents WHERE env_id = $1 AND object = $2 AND deployment = $3
				AND condition = $4 AND closed_at IS NULL)`,
		i.EnvID, i.Object, i.Deployment, i.Condition, i.Summary, evidenceText(i.Evidence), interval(within))
	if err != nil {
		return false, fmt.Errorf("reopen incident %s of %s: %w", i.Condition, i.Object, err)
	}
	return ct.RowsAffected() > 0, nil
}

// DeleteClosedIncidents deletes the incidents of one of conditions that
// closed more than keep ago by the database's clock, and returns how many.
func (s *Store) DeleteClosedIncidents(ctx context.Context, conditions []string, keep time.Duration) (int64, error) {
	ct, err := s.q.Exec(ctx, `DELETE FROM env_incidents WHERE closed_at < now() - $2::interval AND condition = ANY($1)`,
		nonNil(conditions), interval(keep))
	return ct.RowsAffected(), err
}
