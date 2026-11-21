// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// ProjectJob is a project's job (jobs, ADR 0018): a container run to
// its end. Its pod is the sandbox row of the same ID (project_job) while
// it has not ended. Its sealed secrets are read apart (ProjectJobSealed).
type ProjectJob struct {
	// MadeBy is the principal that created it (ADR 0031); written by
	// InsertProjectJob, not read back.
	MadeBy string
	ID     podid.ID
	Owner  string
	// EnvID is the environment the job runs in (ADR 0035), EnvName its
	// name (read back, not written); Reaches what of it its pod reaches
	// and Egress its egress (nil: the internet), ADR 0035 revised. Fixed
	// at the insert.
	EnvID   uuid.UUID
	EnvName string
	Reaches spec.Reaches
	Egress  *spec.Egress
	// Key is the create's Idempotency-Key; "": none.
	Key string
	// RequestHash is the create's body's hash.
	RequestHash string
	// Image is as asked, ImageRef pinned to its digest.
	Image, ImageRef  string
	VCPUs, MemoryMiB int
	ResultPath       string
	Labels           map[string]string
	// LogRetentionDays is how long the job asked its log be kept (ADR
	// 0026); 0: the sink's default.
	LogRetentionDays int
	Phase            string
	Reason, Detail   string
	ExitCode         *int
	Result           string
	LogTail          []string
	CreatedAt        time.Time
	StartedAt        time.Time // zero: NULL
	FinishedAt       time.Time // zero: NULL, the job has not ended
	DeadlineAt       time.Time
}

// Ended reports whether the job has ended.
func (r *ProjectJob) Ended() bool { return !r.FinishedAt.IsZero() }

const projectJobCols = `id, owner, coalesce(idempotency_key, ''), request_hash, image, image_ref, vcpus, memory_mib,
	result_path, labels, phase, reason, detail, exit_code, result, log_tail, created_at, started_at, finished_at,
	deadline_at, coalesce(log_retention_days, 0), environment_id,
	coalesce((SELECT e.name FROM environments e WHERE e.id = environment_id), ''), reaches, egress`

func scanProjectJob(row pgx.Row) (ProjectJob, error) {
	var r ProjectJob
	var started, finished *time.Time
	var reaches, egress []byte
	if err := row.Scan(&r.ID, &r.Owner, &r.Key, &r.RequestHash, &r.Image, &r.ImageRef, &r.VCPUs, &r.MemoryMiB,
		&r.ResultPath, &r.Labels, &r.Phase, &r.Reason, &r.Detail, &r.ExitCode, &r.Result, &r.LogTail, &r.CreatedAt,
		&started, &finished, &r.DeadlineAt, &r.LogRetentionDays, &r.EnvID, &r.EnvName, &reaches, &egress); err != nil {
		return ProjectJob{}, err
	}
	r.StartedAt, r.FinishedAt = timeOf(started), timeOf(finished)
	r.Labels, r.LogTail = stringMap(r.Labels), nonNil(r.LogTail)
	var err error
	if r.Reaches, r.Egress, err = scanReach(reaches, egress); err != nil {
		return ProjectJob{}, fmt.Errorf("job %s: %w", r.ID, err)
	}
	return r, nil
}

func collectProjectJobs(rows pgx.Rows, err error) ([]ProjectJob, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProjectJob, error) { return scanProjectJob(r) })
}

// InsertProjectJob inserts a queued job, due deadline after the database's now,
// with its sealed secrets (nil: none). A key another job of the owner
// holds is ErrExists.
func (s *Store) InsertProjectJob(ctx context.Context, r ProjectJob, sealed []byte, deadline time.Duration) (ProjectJob, error) {
	labels, err := jsonText(stringMap(r.Labels))
	if err != nil {
		return ProjectJob{}, err
	}
	reaches, egress, err := reachJSON(r.Reaches, r.Egress)
	if err != nil {
		return ProjectJob{}, fmt.Errorf("insert job %s: %w", r.ID, err)
	}
	out, err := scanProjectJob(s.q.QueryRow(ctx, `INSERT INTO jobs (id, owner, idempotency_key, request_hash, image, image_ref,
			vcpus, memory_mib, result_path, labels, sealed, deadline_at, log_retention_days, made_by, environment_id,
			reaches, egress)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, now() + $12::interval, $13, $14, $15, $16::jsonb,
			$17::jsonb)
		RETURNING `+projectJobCols,
		r.ID, r.Owner, nullString(r.Key), r.RequestHash, r.Image, r.ImageRef, r.VCPUs, r.MemoryMiB, r.ResultPath, labels,
		sealed, interval(deadline), nullPositive(r.LogRetentionDays), r.MadeBy, r.EnvID, reaches, egress))
	if err != nil {
		return ProjectJob{}, insertError("insert job "+r.ID.String(), err)
	}
	return out, nil
}

// GetProjectJob reads a job of owner (any owner for ""); ErrNoRow if there is
// none.
func (s *Store) GetProjectJob(ctx context.Context, owner string, id podid.ID) (ProjectJob, error) {
	r, err := scanProjectJob(s.q.QueryRow(ctx, "SELECT "+projectJobCols+" FROM jobs WHERE id = $1 AND ($2 = '' OR owner = $2)",
		id, owner))
	return r, noRow(err, ErrNoRow)
}

// ProjectJobByKey reads the job of owner that a create with key made; ErrNoRow
// if there is none.
func (s *Store) ProjectJobByKey(ctx context.Context, owner, key string) (ProjectJob, error) {
	r, err := scanProjectJob(s.q.QueryRow(ctx, "SELECT "+projectJobCols+" FROM jobs WHERE owner = $1 AND idempotency_key = $2",
		owner, key))
	return r, noRow(err, ErrNoRow)
}

// ListProjectJobs returns the jobs of owner that carry every label of labels,
// newest first; envs, when not nil, keeps those of these environments.
func (s *Store) ListProjectJobs(ctx context.Context, owner string, labels map[string]string, envs []uuid.UUID) (
	[]ProjectJob, error) {
	want, err := jsonText(stringMap(labels))
	if err != nil {
		return nil, err
	}
	return collectProjectJobs(s.q.Query(ctx, "SELECT "+projectJobCols+` FROM jobs WHERE owner = $1 AND labels @> $2::jsonb
			AND ($3::uuid[] IS NULL OR environment_id = ANY($3))
		ORDER BY created_at DESC, id DESC`, owner, want, envs))
}

// OpenProjectJobs returns every job that has not ended, oldest first.
func (s *Store) OpenProjectJobs(ctx context.Context) ([]ProjectJob, error) {
	return collectProjectJobs(s.q.Query(ctx, "SELECT "+projectJobCols+" FROM jobs WHERE finished_at IS NULL ORDER BY created_at, id"))
}

// ProjectJobSealed reads a job's sealed secrets: nil once it has ended or when
// it has none; ErrNoRow if there is no job.
func (s *Store) ProjectJobSealed(ctx context.Context, id podid.ID) ([]byte, error) {
	var sealed []byte
	err := s.q.QueryRow(ctx, "SELECT sealed FROM jobs WHERE id = $1", id).Scan(&sealed)
	return sealed, noRow(err, ErrNoRow)
}

// StartProjectJob marks a queued job running, started now; it reports whether
// it did.
func (s *Store) StartProjectJob(ctx context.Context, id podid.ID) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE jobs SET phase = $2, started_at = now()
		WHERE id = $1 AND phase = $3`, id, model.JobRunning, model.JobQueued)
	return tag.RowsAffected() > 0, err
}

// ProjectJobEnd is how a job ended.
type ProjectJobEnd struct {
	Phase, Reason, Detail string
	ExitCode              *int
	Result                string
	LogTail               []string
	// Exited: its process ran and exited; a job not seen running before
	// gets its start now.
	Exited bool
}

// EndProjectJob ends a job that has not ended, now, and drops its sealed
// secrets. It reports whether it did: false when the job has ended
// already, or is gone.
func (s *Store) EndProjectJob(ctx context.Context, id podid.ID, e ProjectJobEnd) (bool, error) {
	switch e.Phase {
	case model.JobSucceeded, model.JobFailed, model.JobDeadlineExceeded, model.JobCancelled:
	default:
		return false, fmt.Errorf("end job %s: %q is no end", id, e.Phase)
	}
	tail, err := jsonText(nonNil(e.LogTail))
	if err != nil {
		return false, err
	}
	tag, err := s.q.Exec(ctx, `UPDATE jobs SET phase = $2, reason = $3, detail = $4, exit_code = $5, result = $6,
			log_tail = $7::jsonb, finished_at = now(), sealed = NULL,
			started_at = CASE WHEN $8 THEN coalesce(started_at, now()) ELSE started_at END
		WHERE id = $1 AND finished_at IS NULL`, id, e.Phase, e.Reason, e.Detail, nullInt(e.ExitCode), e.Result, tail,
		e.Exited)
	return tag.RowsAffected() > 0, err
}

// DeleteProjectJob removes a job of owner; it reports whether there was one.
func (s *Store) DeleteProjectJob(ctx context.Context, owner string, id podid.ID) (bool, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM jobs WHERE id = $1 AND owner = $2", id, owner)
	return tag.RowsAffected() > 0, err
}

// SweepProjectJobs removes the jobs that ended more than retention ago, by the
// database's clock, and returns how many.
func (s *Store) SweepProjectJobs(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := s.q.Exec(ctx, "DELETE FROM jobs WHERE finished_at < now() - $1::interval", interval(retention))
	return tag.RowsAffected(), err
}

// LeftProjectJobPods returns the jobs' pods (project_job) whose job has ended or
// is gone: a job's end deletes its pod, and these are the deletes that
// did not happen.
func (s *Store) LeftProjectJobPods(ctx context.Context) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxColumns("s")+` FROM sandboxes s
		LEFT JOIN jobs j ON j.id = s.id WHERE s.project_job AND (j.id IS NULL OR j.finished_at IS NOT NULL)
		ORDER BY s.id`))
}

// nullPositive is n, or NULL for 0 and less.
func nullPositive(n int) any {
	if n <= 0 {
		return nil
	}
	return n
}
