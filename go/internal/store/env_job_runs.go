// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// EnvJobRun is a row of env_job_runs: one attempt of a job, written with
// its instance row and kept after it (the row's ID is in SandboxID while
// it exists). A succeeded run for (job, template hash) means the job has
// run.
type EnvJobRun struct {
	ID           int64
	EnvID        uuid.UUID
	Job          string
	TemplateHash string
	Deployment   int
	Attempt      int
	SandboxID    podid.ID // "": NULL
	Phase        string   // model.Run*
	ExitCode     *int     // nil: NULL
	Detail       string
	LogTail      []string // the last 50 lines, kept when the row goes
	StartedAt    time.Time
	FinishedAt   time.Time // zero: NULL
}

const jobRunCols = `id, env_id, job, template_hash, deployment, attempt, sandbox_id, phase, exit_code, detail, log_tail,
	started_at, finished_at`

func scanJobRun(row pgx.Row) (EnvJobRun, error) {
	var r EnvJobRun
	var sandbox *podid.ID
	var finished *time.Time
	if err := row.Scan(&r.ID, &r.EnvID, &r.Job, &r.TemplateHash, &r.Deployment, &r.Attempt, &sandbox, &r.Phase,
		&r.ExitCode, &r.Detail, &r.LogTail, &r.StartedAt, &finished); err != nil {
		return EnvJobRun{}, err
	}
	r.SandboxID, r.FinishedAt, r.LogTail = idOf(sandbox), timeOf(finished), nonNil(r.LogTail)
	return r, nil
}

func collectJobRuns(rows pgx.Rows, err error) ([]EnvJobRun, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvJobRun, error) { return scanJobRun(r) })
}

// InsertJobRun inserts a run (phase running when empty, attempt 1 when
// 0) and returns it with its ID; an unknown environment is ErrNoRow.
func (s *Store) InsertJobRun(ctx context.Context, r EnvJobRun) (EnvJobRun, error) {
	if r.Phase == "" {
		r.Phase = model.RunRunning
	}
	if r.Attempt == 0 {
		r.Attempt = 1
	}
	tail, err := jsonText(nonNil(r.LogTail))
	if err != nil {
		return EnvJobRun{}, err
	}
	out, err := scanJobRun(s.q.QueryRow(ctx, `INSERT INTO env_job_runs (env_id, job, template_hash, deployment, attempt,
			sandbox_id, phase, exit_code, detail, log_tail, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11) RETURNING `+jobRunCols,
		r.EnvID, r.Job, r.TemplateHash, r.Deployment, r.Attempt, nullID(r.SandboxID), r.Phase, nullInt(r.ExitCode),
		r.Detail, tail, nullTime(r.FinishedAt)))
	if err != nil {
		return EnvJobRun{}, insertError("insert a run of job "+r.Job, err)
	}
	return out, nil
}

// GetJobRun reads a run; ErrNoRow if there is none.
func (s *Store) GetJobRun(ctx context.Context, id int64) (EnvJobRun, error) {
	return s.LockJobRun(ctx, id, NoLock)
}

// LockJobRun reads a run and locks it with lock; ErrNoRow if there is
// none.
func (s *Store) LockJobRun(ctx context.Context, id int64, lock RowLock) (EnvJobRun, error) {
	r, err := scanJobRun(s.q.QueryRow(ctx, "SELECT "+jobRunCols+" FROM env_job_runs WHERE id = $1 "+string(lock), id))
	if err != nil {
		return EnvJobRun{}, noRow(err, ErrNoRow)
	}
	return r, nil
}

// JobRunOfSandbox reads the latest run whose instance row is sandboxID and
// locks it with lock; ErrNoRow if there is none.
func (s *Store) JobRunOfSandbox(ctx context.Context, sandboxID podid.ID, lock RowLock) (EnvJobRun, error) {
	r, err := scanJobRun(s.q.QueryRow(ctx, "SELECT "+jobRunCols+` FROM env_job_runs WHERE sandbox_id = $1
		ORDER BY id DESC LIMIT 1 `+string(lock), sandboxID))
	if err != nil {
		return EnvJobRun{}, noRow(err, ErrNoRow)
	}
	return r, nil
}

// ListJobRuns returns up to limit runs of the environment's job (of every
// job for ""), newest first.
func (s *Store) ListJobRuns(ctx context.Context, envID uuid.UUID, job string, limit int) ([]EnvJobRun, error) {
	return collectJobRuns(s.q.Query(ctx, "SELECT "+jobRunCols+` FROM env_job_runs
		WHERE env_id = $1 AND ($2 = '' OR job = $2) ORDER BY id DESC LIMIT $3`, envID, job, limit))
}

// LatestJobRun reads the newest run of the environment's job; ErrNoRow if
// it never ran.
func (s *Store) LatestJobRun(ctx context.Context, envID uuid.UUID, job string) (EnvJobRun, error) {
	r, err := scanJobRun(s.q.QueryRow(ctx, "SELECT "+jobRunCols+` FROM env_job_runs
		WHERE env_id = $1 AND job = $2 ORDER BY id DESC LIMIT 1`, envID, job))
	if err != nil {
		return EnvJobRun{}, noRow(err, ErrNoRow)
	}
	return r, nil
}

// JobSucceeded reports whether a run of the job with the template hash
// succeeded: the job has run.
func (s *Store) JobSucceeded(ctx context.Context, envID uuid.UUID, job, templateHash string) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM env_job_runs
		WHERE env_id = $1 AND job = $2 AND template_hash = $3 AND phase = 'succeeded')`, envID, job, templateHash).Scan(&ok)
	return ok, err
}

// UpdateJobRun writes the sandbox_id, phase, exit_code, detail, log_tail
// and finished_at in which after differs from before; the other columns
// are refused. When nothing differs, before is returned unwritten.
func (s *Store) UpdateJobRun(ctx context.Context, before, after EnvJobRun) (EnvJobRun, error) {
	what := fmt.Sprintf("run %d of job %s", before.ID, before.Job)
	if err := fixed(what, map[string]bool{"id": before.ID != after.ID, "env_id": before.EnvID != after.EnvID,
		"job": before.Job != after.Job, "template_hash": before.TemplateHash != after.TemplateHash,
		"deployment": before.Deployment != after.Deployment, "attempt": before.Attempt != after.Attempt,
		"started_at": !before.StartedAt.Equal(after.StartedAt)}); err != nil {
		return EnvJobRun{}, err
	}
	u := newUpdates(before.ID)
	u.set("sandbox_id", before.SandboxID != after.SandboxID, nullID(after.SandboxID))
	u.set("phase", before.Phase != after.Phase, after.Phase)
	u.set("exit_code", !sameIntPtr(before.ExitCode, after.ExitCode), nullInt(after.ExitCode))
	u.set("detail", before.Detail != after.Detail, after.Detail)
	if err := u.setJSON("log_tail", !slices.Equal(before.LogTail, after.LogTail), nonNil(after.LogTail)); err != nil {
		return EnvJobRun{}, err
	}
	u.set("finished_at", !before.FinishedAt.Equal(after.FinishedAt), nullTime(after.FinishedAt))
	if u.empty() {
		return before, nil
	}
	r, err := scanJobRun(s.q.QueryRow(ctx, "UPDATE env_job_runs SET "+u.clause(false)+" WHERE id = $1 RETURNING "+jobRunCols,
		u.args...))
	if err != nil {
		return EnvJobRun{}, fmt.Errorf("update %s: %w", what, noRow(err, ErrNoRow))
	}
	return r, nil
}
