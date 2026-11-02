// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// EnvAttached is a row of env_attached: an attached service, a supervised
// process in a tenant's sandbox.
type EnvAttached struct {
	EnvID     uuid.UUID
	Service   string
	SandboxID podid.ID // the tenant's sandbox it runs in (no foreign key)
	// SandboxIncarnation is the incarnation of the sandbox the service
	// last used (its process started in, or recorded with); a new one
	// under the same ID starts the service anew (§13.6). uuid.Nil: NULL.
	SandboxIncarnation uuid.UUID
	TemplateHash       string
	ProcessID          string // "": not started
	Phase              string // model.Phase* but succeeded and stopping
	ReadySince         time.Time
	Restarts           int
	LastExit           *int
	ProbeFailures      int
	NextRestartAt      time.Time
	Detail             string
	// AwakeUntil keeps the sandbox running while a started process is not
	// ready yet, until its rollout budget is spent; zero: no claim.
	// IdleAfterS is the service's idle once it runs, in seconds (0:
	// never); nil: no claim, the sandbox's own interval applies (ADR
	// 0018, "An attached service idles as an image service does").
	AwakeUntil time.Time
	IdleAfterS *int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const attachedCols = `env_id, service, sandbox_id, sandbox_incarnation, template_hash, process_id, phase, ready_since,
	restarts, last_exit, probe_failures, next_restart_at, detail, awake_until, idle_after_s, created_at, updated_at`

func scanAttached(row pgx.Row) (EnvAttached, error) {
	var a EnvAttached
	var inc *uuid.UUID
	var ready, next, awake *time.Time
	if err := row.Scan(&a.EnvID, &a.Service, &a.SandboxID, &inc, &a.TemplateHash, &a.ProcessID, &a.Phase, &ready,
		&a.Restarts, &a.LastExit, &a.ProbeFailures, &next, &a.Detail, &awake, &a.IdleAfterS, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return EnvAttached{}, err
	}
	a.SandboxIncarnation, a.ReadySince, a.NextRestartAt, a.AwakeUntil = uuidOf(inc), timeOf(ready), timeOf(next), timeOf(awake)
	return a, nil
}

func collectAttached(rows pgx.Rows, err error) ([]EnvAttached, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvAttached, error) { return scanAttached(r) })
}

// InsertAttached inserts an attached service (phase starting when
// empty); one that exists is ErrExists, an unknown environment ErrNoRow.
func (s *Store) InsertAttached(ctx context.Context, a EnvAttached) (EnvAttached, error) {
	if a.Phase == "" {
		a.Phase = model.PhaseStarting
	}
	out, err := scanAttached(s.q.QueryRow(ctx, `INSERT INTO env_attached (env_id, service, sandbox_id, sandbox_incarnation,
			template_hash, process_id, phase, ready_since, restarts, last_exit, probe_failures, next_restart_at, detail,
			awake_until, idle_after_s)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING `+attachedCols,
		a.EnvID, a.Service, a.SandboxID, nullUUID(a.SandboxIncarnation), a.TemplateHash, a.ProcessID, a.Phase,
		nullTime(a.ReadySince), a.Restarts, nullInt(a.LastExit), a.ProbeFailures, nullTime(a.NextRestartAt), a.Detail,
		nullTime(a.AwakeUntil), nullInt(a.IdleAfterS)))
	if err != nil {
		return EnvAttached{}, insertError("insert attached service "+a.Service, err)
	}
	return out, nil
}

// GetAttached reads an attached service; ErrNoRow if there is none.
func (s *Store) GetAttached(ctx context.Context, envID uuid.UUID, service string) (EnvAttached, error) {
	return s.LockAttached(ctx, envID, service, NoLock)
}

// LockAttached reads an attached service and locks it with lock; ErrNoRow
// if there is none.
func (s *Store) LockAttached(ctx context.Context, envID uuid.UUID, service string, lock RowLock) (EnvAttached, error) {
	a, err := scanAttached(s.q.QueryRow(ctx, "SELECT "+attachedCols+` FROM env_attached
		WHERE env_id = $1 AND service = $2 `+string(lock), envID, service))
	if err != nil {
		return EnvAttached{}, noRow(err, ErrNoRow)
	}
	return a, nil
}

// ListAttached returns the environment's attached services by name.
func (s *Store) ListAttached(ctx context.Context, envID uuid.UUID) ([]EnvAttached, error) {
	return collectAttached(s.q.Query(ctx, "SELECT "+attachedCols+" FROM env_attached WHERE env_id = $1 ORDER BY service", envID))
}

// AttachedOfSandbox returns the attached services running in the sandbox,
// of every environment, by environment and name.
func (s *Store) AttachedOfSandbox(ctx context.Context, sandboxID podid.ID) ([]EnvAttached, error) {
	return collectAttached(s.q.Query(ctx, "SELECT "+attachedCols+` FROM env_attached
		WHERE sandbox_id = $1 ORDER BY env_id, service`, sandboxID))
}

// UpdateAttached writes the columns in which after differs from before
// (all but env_id, service and created_at, which it refuses); updated_at =
// now(). When nothing differs, before is returned unwritten.
func (s *Store) UpdateAttached(ctx context.Context, before, after EnvAttached) (EnvAttached, error) {
	what := fmt.Sprintf("attached service %s of environment %s", before.Service, before.EnvID)
	if err := fixed(what, map[string]bool{"env_id": before.EnvID != after.EnvID, "service": before.Service != after.Service,
		"created_at": !before.CreatedAt.Equal(after.CreatedAt)}); err != nil {
		return EnvAttached{}, err
	}
	u := newUpdates(before.EnvID, before.Service)
	u.set("sandbox_id", before.SandboxID != after.SandboxID, after.SandboxID)
	u.set("sandbox_incarnation", before.SandboxIncarnation != after.SandboxIncarnation, nullUUID(after.SandboxIncarnation))
	u.set("template_hash", before.TemplateHash != after.TemplateHash, after.TemplateHash)
	u.set("process_id", before.ProcessID != after.ProcessID, after.ProcessID)
	u.set("phase", before.Phase != after.Phase, after.Phase)
	u.set("ready_since", !before.ReadySince.Equal(after.ReadySince), nullTime(after.ReadySince))
	u.set("restarts", before.Restarts != after.Restarts, after.Restarts)
	u.set("last_exit", !sameIntPtr(before.LastExit, after.LastExit), nullInt(after.LastExit))
	u.set("probe_failures", before.ProbeFailures != after.ProbeFailures, after.ProbeFailures)
	u.set("next_restart_at", !before.NextRestartAt.Equal(after.NextRestartAt), nullTime(after.NextRestartAt))
	u.set("detail", before.Detail != after.Detail, after.Detail)
	u.set("awake_until", !before.AwakeUntil.Equal(after.AwakeUntil), nullTime(after.AwakeUntil))
	u.set("idle_after_s", !sameIntPtr(before.IdleAfterS, after.IdleAfterS), nullInt(after.IdleAfterS))
	if u.empty() {
		return before, nil
	}
	a, err := scanAttached(s.q.QueryRow(ctx, "UPDATE env_attached SET "+u.clause(true)+
		" WHERE env_id = $1 AND service = $2 RETURNING "+attachedCols, u.args...))
	if err != nil {
		return EnvAttached{}, fmt.Errorf("update %s: %w", what, noRow(err, ErrNoRow))
	}
	return a, nil
}

// DeleteAttached removes an attached service and reports whether it was
// there.
func (s *Store) DeleteAttached(ctx context.Context, envID uuid.UUID, service string) (bool, error) {
	ct, err := s.q.Exec(ctx, "DELETE FROM env_attached WHERE env_id = $1 AND service = $2", envID, service)
	return ct.RowsAffected() > 0, err
}
