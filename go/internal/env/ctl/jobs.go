// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// stepJobs keeps the running runs going (timeouts, healing, leftovers)
// and, while R rolls, runs its release jobs (§4.3 step 3): a job runs
// when no run of its template hash succeeded, once every addon is
// provisioned; a failed attempt is retried while attempt ≤ retries; a
// failed last attempt fails R (job_failed). A job runs once at a time
// across releases (§13.7): while a run of it that is not R's runs (a
// replaced release's, a manual one, another template's), R's waits for
// it to end, never cancelling it; the time R waited so moves the start
// its budgets count from (jobWait), on every step, also once the job is
// done.
func (c *Controller) stepJobs(ctx context.Context, s *stepState) {
	v := s.v
	c.stepRuns(ctx, s)
	if !deploying(v) {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(v.target.Jobs)) {
		j := v.target.Jobs[name]
		if jobOn(j) != spec.JobOnRelease {
			continue
		}
		p := c.planJob(v, name, j)
		c.jobWait(ctx, s, name, p)
		if !p.ok() {
			s.jobsDone = false
			s.wait(objectName(spec.KindJob, name), "job %s: %s", name, p.why())
			continue
		}
		if !s.addonsReady {
			s.jobsDone = false
			s.wait(objectName(spec.KindJob, name), "job %s waits for the addons", name)
			continue
		}
		done, err := c.st.JobSucceeded(ctx, v.env.ID, name, p.hash)
		if err != nil {
			s.jobsDone = false
			s.wait(objectName(spec.KindJob, name), "job %s: %v", name, err)
			continue
		}
		if done {
			continue
		}
		s.jobsDone = false
		runs, err := c.st.ListJobRuns(ctx, v.env.ID, name, 100)
		if err != nil {
			s.wait(objectName(spec.KindJob, name), "job %s: %v", name, err)
			continue
		}
		var last, other *store.EnvJobRun
		for i := range runs {
			switch {
			case runs[i].TemplateHash == p.hash && runs[i].Release == v.targetNum:
				if last == nil {
					last = &runs[i]
				}
			case runs[i].Phase == model.RunRunning && other == nil:
				other = &runs[i]
			}
		}
		switch {
		case other != nil:
			s.wait(objectName(spec.KindJob, name), "job %s waits for its run of release %d (attempt %d)", name, other.Release,
				other.Attempt)
		case last == nil:
			c.startRun(ctx, s, p, 1)
		case last.Phase == model.RunRunning:
			s.wait(objectName(spec.KindJob, name), "job %s runs (attempt %d)", name, last.Attempt)
		case last.Phase == model.RunFailed && last.Attempt <= j.Retries:
			c.startRun(ctx, s, p, last.Attempt+1)
		case last.Phase == model.RunFailed:
			summary := fmt.Sprintf("job %s failed its last attempt (%d of %d)%s", name, last.Attempt, j.Retries+1, runOutcome(*last))
			ev := evidence{Exits: []store.EnvExit{{At: last.FinishedAt, Code: last.ExitCode}}, LogTail: last.LogTail}
			if err := c.failRelease(ctx, v, summary, objectName(spec.KindJob, name), model.ConditionJobFailed, ev); err != nil {
				s.wait(objectName(spec.KindJob, name), "job %s: %v", name, err)
				continue
			}
			s.failed = true
			return
		}
	}
}

// jobWait moves R's start, which its budgets count from, on by the time
// R waited for runs of the job that were not its own (§13.7): until the
// latest end of such a run that ended after R began (now while one
// runs), unless a run of R's template had succeeded before R began. It
// is worked out on every step, whether the job is done, the addons are
// ready or its template renders, so that R keeps the time once its
// own run is done; when it cannot be read, no budget is judged in the
// step.
func (c *Controller) jobWait(ctx context.Context, s *stepState, name string, p *podPlan) {
	v := s.v
	if v.latest.StartedAt.IsZero() {
		return
	}
	hash := ""
	if p.ok() {
		hash = p.hash
	}
	until, err := c.st.EnvCtlJobWaitedUntil(ctx, v.env.ID, name, hash, v.targetNum, v.latest.StartedAt)
	if err != nil {
		s.shiftUnknown = true
		s.wait(objectName(spec.KindJob, name), "job %s: %v", name, err)
		return
	}
	if d := until.Sub(v.latest.StartedAt); d > s.shift {
		s.shift = d
	}
}

// runOutcome says how a failed run ended: " with code N", or its detail.
func runOutcome(r store.EnvJobRun) string {
	if r.ExitCode != nil {
		return " with code " + strconv.Itoa(*r.ExitCode)
	}
	if r.Detail != "" {
		return ": " + r.Detail
	}
	return ""
}

// startRun inserts a run of a job (attempt n) with its instance row,
// refused while a run of the job runs (once: checked under the
// environment's row lock, as RunJob is, so that a run the API inserted
// meanwhile is waited for, never run beside).
func (c *Controller) startRun(ctx context.Context, s *stepState, p *podPlan, attempt int) {
	v := s.v
	run := &store.EnvJobRun{EnvID: v.env.ID, Job: p.name, TemplateHash: p.hash, Release: v.targetNum, Attempt: attempt,
		Phase: model.RunRunning}
	if _, err := c.insertInstance(ctx, v, insert{plan: p, release: v.targetNum, run: run, once: true}); err != nil {
		if errors.Is(err, ErrJobRunning) {
			s.wait(objectName(spec.KindJob, p.name), "%v", err)
			return
		}
		s.wait(objectName(spec.KindJob, p.name), "job %s: %v", p.name, err)
		return
	}
	s.wait(objectName(spec.KindJob, p.name), "job %s starts (attempt %d)", p.name, attempt)
}

// stepRuns keeps the running runs of the environment going, release and
// manual ones: a run past its job's timeout fails, a lost instance fails
// its run, a row that is not up is brought up; the rows of finished runs
// go.
func (c *Controller) stepRuns(ctx context.Context, s *stepState) {
	v := s.v
	runs, err := c.st.EnvCtlRunningJobRuns(ctx, v.env.ID)
	if err != nil {
		s.wait("", "job runs: %v", err)
		return
	}
	rows := map[podid.ID]store.EnvInstanceRow{}
	for _, r := range v.rows {
		if r.Instance.Kind == spec.KindJob {
			rows[r.Sandbox.ID] = r
		}
	}
	for _, run := range runs {
		r, ok := rows[run.SandboxID]
		if !ok {
			c.failOrphanRun(ctx, run)
			continue
		}
		j, ok := v.target.Jobs[run.Job]
		if !ok {
			j = v.latestSpec.Jobs[run.Job]
		}
		timeout := jobTimeout(j)
		switch {
		case v.now.After(run.StartedAt.Add(timeout)):
			c.finishJob(ctx, v, r, nil, "timed out after "+timeout.String())
		case c.heal(ctx, r.Sandbox, false):
			c.finishJob(ctx, v, r, nil, "its instance was lost with node "+lostNode(r.Sandbox))
		}
	}
	for _, r := range rows {
		if r.Instance.Phase == model.PhaseSucceeded || r.Instance.Phase == model.PhaseFailed {
			if err := c.deleteInstance(ctx, r.Sandbox); err != nil && ctx.Err() == nil {
				c.rl.warn("job", "cannot delete a finished job's instance", "sandbox", r.Sandbox.ID, "err", err)
			}
		}
	}
}

// failOrphanRun fails a running run whose instance row is gone.
func (c *Controller) failOrphanRun(ctx context.Context, run store.EnvJobRun) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockJobRun(ctx, run.ID, store.ForUpdate)
		if err != nil || cur.Phase != model.RunRunning {
			return err
		}
		if _, err := tx.GetSandbox(ctx, cur.SandboxID); err == nil {
			return nil // inserted after the view was read
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		after := cur
		after.Phase, after.Detail, after.FinishedAt = model.RunFailed, "its instance is gone", now
		_, err = tx.UpdateJobRun(ctx, cur, after)
		return err
	})
	if err != nil && ctx.Err() == nil {
		c.rl.warn("job", "cannot fail a run without an instance", "run", run.ID, "err", err)
	}
}

// finishJob ends a job's run (§4.3 step 3): exit code 0 → succeeded;
// another code, none ("the node did not report an exit status"), a
// timeout or a lost instance → failed. Its log tail (the last 50 lines,
// redacted and bounded: storedTail) is kept in the run before its row
// goes.
func (c *Controller) finishJob(ctx context.Context, v *view, r store.EnvInstanceRow, code *int, detail string) {
	var tail []string
	if steady(r.Sandbox) && c.connected(r.Sandbox.Node) {
		tail = c.storedTail(ctx, v, r.Sandbox)
	}
	succeeded, detail := outcome(code, detail)
	err := c.fenced(ctx, func(tx *store.Store) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		run, err := tx.JobRunOfSandbox(ctx, r.Sandbox.ID, store.ForUpdate)
		switch {
		case errors.Is(err, store.ErrNoRow):
		case err != nil:
			return err
		case run.Phase == model.RunRunning:
			after := run
			after.Phase, after.ExitCode, after.Detail, after.FinishedAt = model.RunFailed, code, detail, now
			if succeeded {
				after.Phase = model.RunSucceeded
			}
			if tail != nil {
				after.LogTail = tail
			}
			if _, err := tx.UpdateJobRun(ctx, run, after); err != nil {
				return err
			}
		}
		inst, err := tx.LockEnvInstance(ctx, r.Sandbox.ID, store.ForUpdate)
		if errors.Is(err, store.ErrNoRow) {
			return nil
		}
		if err != nil {
			return err
		}
		after := inst
		after.Phase, after.LastExit, after.Detail = model.PhaseFailed, code, detail
		if succeeded {
			after.Phase = model.PhaseSucceeded
		}
		after.Exits = appendExit(inst.Exits, store.EnvExit{At: now, Code: code})
		if _, err := tx.UpdateEnvInstance(ctx, inst, after); err != nil {
			return err
		}
		return tx.Notify(ctx, db.ChanLeader, HintReconcile)
	})
	if err != nil {
		if ctx.Err() == nil {
			c.rl.warn("job", "cannot finish a job's run", "sandbox", r.Sandbox.ID, "err", err)
		}
		return
	}
	c.log.Info("job run finished", "environment", v.env.ID, "job", r.Instance.Object, "sandbox", r.Sandbox.ID,
		"succeeded", succeeded, "detail", detail)
	if err := c.deleteInstance(ctx, r.Sandbox); err != nil && ctx.Err() == nil {
		c.rl.warn("job", "cannot delete a finished job's instance", "sandbox", r.Sandbox.ID, "err", err)
	}
}

// noExitStatus is the detail of an exit without a code.
const noExitStatus = "the node did not report an exit status"

// outcome is the end of an environment job's run or of a tenant's job,
// from its exit code and the detail of an end that is no exit ("" for an
// exit): succeeded for a 0 without a detail; an exit without a code gets
// noExitStatus.
func outcome(code *int, detail string) (succeeded bool, why string) {
	if code == nil && detail == "" {
		return false, noExitStatus
	}
	return code != nil && *code == 0 && detail == "", detail
}

// maxExits is how many exits an instance keeps (env_instances.exits).
const maxExits = 5

// appendExit adds an exit, keeping the last maxExits.
func appendExit(exits []store.EnvExit, e store.EnvExit) []store.EnvExit {
	out := append(slices.Clone(exits), e)
	if len(out) > maxExits {
		out = out[len(out)-maxExits:]
	}
	return out
}

// RunJob starts a run of a job of the environment's target release (what
// jobs on "manual" are for): a new attempt 1. It returns once the run's
// row is inserted; ErrNoObject, ErrNotReady, and ErrJobRunning (a
// model.ErrBusy) while a run of the job runs, of any release (a repeated
// request never runs it twice at once).
func (c *Controller) RunJob(ctx context.Context, envID uuid.UUID, name string) error {
	v, err := c.load(ctx, envID)
	if errors.Is(err, store.ErrNoRow) || (err == nil && v == nil) {
		return ErrNoObject
	}
	if err != nil {
		return err
	}
	j, ok := v.target.Jobs[name]
	if !ok {
		return ErrNoObject
	}
	p := c.planJob(v, name, j)
	if !p.ok() {
		return &stateError{msg: fmt.Sprintf("job %s cannot run yet: %s", name, p.why()), kind: ErrNotReady}
	}
	run := &store.EnvJobRun{EnvID: v.env.ID, Job: name, TemplateHash: p.hash, Release: v.targetNum, Attempt: 1,
		Phase: model.RunRunning}
	_, err = c.insertFrom(ctx, v, insert{plan: p, release: v.targetNum, run: run, once: true}, false)
	return err
}

// noRunOf refuses with ErrJobRunning while a run of the job runs.
func noRunOf(ctx context.Context, tx *store.Store, envID uuid.UUID, job string) error {
	runs, err := tx.EnvCtlRunningJobRuns(ctx, envID)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Job == job {
			return &stateError{msg: fmt.Sprintf("job %s runs already (release %d, attempt %d)", job, r.Release, r.Attempt),
				kind: ErrJobRunning}
		}
	}
	return nil
}
