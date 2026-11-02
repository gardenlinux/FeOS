// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// The answers of the API's calls into the controller: on any instance,
// not fenced; a write wakes the leader's env-reconcile.

// maxLogLines bounds a LogTail.
const maxLogLines = 10000

// LogTail returns the last lines lines of an object's log, oldest first,
// and their source: the main container's log of replica replica of an
// image service, of an addon (replica 0), or of a job's latest run (its
// stored tail once the run's row is gone) (SourceContainer); the process
// log of an attached service (SourceProcess). With --sink-url, a
// container's log is the sink's first (sinkLog), the node's when the sink
// fails or has no line. It never wakes an instance (ErrPaused, unless the
// sink has its lines); ErrNoObject (its text names a replica the object does not
// have), ErrNotRunning for a replica the target deployment has but that has
// no instance yet (the deployment waits for its addons or a job, or for
// capacity), ErrNoContainerLogs.
func (c *Controller) LogTail(ctx context.Context, envID uuid.UUID, kind, name string, replica, lines int) ([]string, string, error) {
	if lines <= 0 {
		lines = 200
	}
	lines = min(lines, maxLogLines)
	v, err := c.load(ctx, envID)
	if errors.Is(err, store.ErrNoRow) || (err == nil && v == nil) {
		return nil, "", ErrNoObject
	}
	if err != nil {
		return nil, "", err
	}
	desired := 1
	switch kind {
	case spec.KindService:
		svc, ok := v.target.Services[name]
		if !ok {
			return nil, "", ErrNoObject
		}
		if runtimeOf(svc) == spec.RuntimeAttached {
			return c.processLog(ctx, v, name, lines)
		}
		desired = replicasOf(svc)
	case spec.KindAddon:
		if _, ok := v.target.Addons[name]; !ok {
			return nil, "", ErrNoObject
		}
	case spec.KindJob:
		if _, ok := v.target.Jobs[name]; !ok {
			return nil, "", ErrNoObject
		}
		return c.jobLog(ctx, v, name, lines)
	default:
		return nil, "", ErrNoObject
	}
	var row *store.EnvInstanceRow
	for _, r := range v.rowsOf(kind, name) {
		if r.Instance.Replica != replica {
			continue
		}
		if row == nil || (row.Instance.Phase == model.PhaseStopping && r.Instance.Phase != model.PhaseStopping) ||
			r.Instance.CreatedAt.After(row.Instance.CreatedAt) {
			r := r
			row = &r
		}
	}
	switch {
	case row != nil:
	case replica < desired:
		return nil, "", notRunning("%s %s has no instance of replica %d yet: no log to read", kind, name, replica)
	case desired == 0:
		return nil, "", notRunning("%s %s runs no replica (replicas: 0): no log to read", kind, name)
	default:
		return nil, "", &stateError{msg: fmt.Sprintf("%s %s has no replica %d (it has %d)", kind, name, replica, desired),
			kind: ErrNoObject}
	}
	// A row without log labels (inserted before ADR 0026) is not shipped.
	if len(row.Sandbox.LogLabels) > 0 {
		if out, ok := c.sinkLog(ctx, "instance", row.Sandbox.ID.String(), row.Sandbox.CreatedAt, lines); ok {
			return out, SourceContainer, nil
		}
	}
	out, err := c.instanceLog(ctx, row.Sandbox, lines)
	return out, SourceContainer, err
}

// instanceLog reads a running instance's container log; a paused or
// hibernated one is ErrPaused (never woken).
func (c *Controller) instanceLog(ctx context.Context, sb store.Sandbox, lines int) ([]string, error) {
	switch {
	case sb.ClientState == model.StatePaused || sb.ClientState == model.StateHibernated ||
		sb.ClientState == model.StatePausing || sb.ClientState == model.StateHibernating:
		return nil, ErrPaused
	case sb.ClientState != model.StateRunning || sb.Node == "":
		return nil, notRunning("the instance is %s: no log to read", sb.ClientState)
	}
	return c.containerLog(ctx, lease(sb), lines, answerLog)
}

// jobLog is a job's latest run's log: the sink's (sinkLog), else its
// container's while its row runs, else the tail its run kept.
func (c *Controller) jobLog(ctx context.Context, v *view, name string, lines int) ([]string, string, error) {
	run, err := c.st.LatestJobRun(ctx, v.env.ID, name)
	if errors.Is(err, store.ErrNoRow) {
		return []string{}, SourceContainer, nil
	}
	if err != nil {
		return nil, "", err
	}
	if !run.SandboxID.IsZero() {
		if out, ok := c.sinkLog(ctx, "instance", run.SandboxID.String(), run.StartedAt, lines); ok {
			return out, SourceContainer, nil
		}
	}
	if !run.SandboxID.IsZero() && run.Phase == model.RunRunning {
		if sb, err := c.st.GetSandbox(ctx, run.SandboxID); err == nil && sb.ClientState == model.StateRunning {
			out, err := c.instanceLog(ctx, sb, lines)
			return out, SourceContainer, err
		}
	}
	tail := run.LogTail
	if len(tail) > lines {
		tail = tail[len(tail)-lines:]
	}
	return slices.Clone(tail), SourceContainer, nil
}

// processLog is an attached service's process log.
func (c *Controller) processLog(ctx context.Context, v *view, name string, lines int) ([]string, string, error) {
	a, ok := v.attached[name]
	if !ok {
		return []string{}, SourceProcess, nil
	}
	sb, ok := c.usableSandbox(ctx, v.env, a)
	switch {
	case !ok:
		return nil, "", notRunning("%s: no log to read", goneDetail(a.SandboxID))
	case a.ProcessID == "":
		return []string{}, SourceProcess, nil
	case sb.ClientState == model.StatePaused || sb.ClientState == model.StateHibernated ||
		sb.ClientState == model.StatePausing || sb.ClientState == model.StateHibernating:
		return nil, "", ErrPaused
	case !steady(sb):
		return nil, "", notRunning("sandbox %s is %s: no log to read", sb.ID, sb.ClientState)
	case c.d.Runner == nil:
		return nil, "", errors.New("process logs need the tools' runner, which this instance lacks")
	}
	out, err := c.d.Runner.ProcessLogsDetached(ctx, lease(sb), a.ProcessID, lines)
	if errors.Is(err, tools.ErrNoProcess) {
		return []string{}, SourceProcess, nil
	}
	return out, SourceProcess, err
}

// RestartService restarts a service of the environment's target deployment:
// an attached service's process is stopped and started again (never its
// sandbox's pod); an image service's replicas are restarted one at a
// time. It returns once the restart is under way; ErrNoObject, ErrPaused
// and ErrNotRunning (attached), model.ErrBusy (an op in flight).
func (c *Controller) RestartService(ctx context.Context, envID uuid.UUID, name string) error {
	v, err := c.load(ctx, envID)
	if errors.Is(err, store.ErrNoRow) || (err == nil && v == nil) {
		return ErrNoObject
	}
	if err != nil {
		return err
	}
	svc, ok := v.target.Services[name]
	if !ok {
		return ErrNoObject
	}
	if runtimeOf(svc) == spec.RuntimeAttached {
		return c.restartAttached(ctx, v, name)
	}
	var rows []store.EnvInstanceRow
	for _, r := range v.rowsOf(spec.KindService, name) {
		if r.Instance.Phase != model.PhaseStopping {
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, byReplica)
	if len(rows) == 0 {
		return nil
	}
	ids := make([]podid.ID, len(rows))
	for i, r := range rows {
		ids[i] = r.Sandbox.ID
	}
	// The first restart begins now; the others follow one at a time, each
	// once the one before is ready again, by the rule the rollouts count
	// with (live).
	idles := idleOf(svc) > 0
	if err := c.restartReplica(ctx, ids[0]); err != nil {
		return err
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for i := 1; i < len(ids); i++ {
			if !c.waitReady(c.bg, ids[i-1], idles) {
				return
			}
			if err := c.restartReplica(c.bg, ids[i]); err != nil && c.bg.Err() == nil {
				c.rl.warn("restart", "cannot restart a replica", "sandbox", ids[i], "err", err)
				return
			}
		}
	}()
	return nil
}

// restartReplica restarts one replica's pod: its phase is starting again
// (no traffic until it is ready), then Engine.Restart. A row that is not
// running is left to the controller (Ensure brings it up anew); one with
// an op in flight is model.ErrBusy, and its instance stays as it was (a
// restart the engine refuses puts it back), so that a refused restart
// takes no traffic away.
func (c *Controller) restartReplica(ctx context.Context, id podid.ID) error {
	sb, err := c.st.GetSandbox(ctx, id)
	switch {
	case err != nil:
		return err
	case sb.Op != "":
		return fmt.Errorf("an op %s of instance %s is in flight: %w", sb.Op, sb.OpOwner, model.ErrBusy)
	case sb.ClientState != model.StateRunning:
		return nil
	}
	var before store.EnvInstance
	err = c.st.InTx(ctx, func(tx *store.Store) error {
		inst, err := tx.LockEnvInstance(ctx, id, store.ForUpdate)
		if err != nil {
			return err
		}
		before = inst
		after := inst
		after.Phase, after.ReadySince, after.ProbeFailures, after.Detail = model.PhaseStarting, time.Time{}, 0, "restarting"
		_, err = tx.UpdateEnvInstance(ctx, inst, after)
		return err
	})
	if err != nil {
		return err
	}
	err = c.d.Engine.Restart(ctx, id, "restart requested")
	switch {
	case errors.Is(err, model.ErrPrecondition):
		return nil
	case errors.Is(err, model.ErrBusy):
		c.unrestart(ctx, before)
	}
	return err
}

// unrestart puts an instance back as it was before a restart the engine
// refused, unless something wrote it meanwhile.
func (c *Controller) unrestart(ctx context.Context, before store.EnvInstance) {
	err := c.st.InTx(ctx, func(tx *store.Store) error {
		cur, err := tx.LockEnvInstance(ctx, before.SandboxID, store.ForUpdate)
		if err != nil || cur.Phase != model.PhaseStarting || cur.Detail != "restarting" {
			return err
		}
		after := cur
		after.Phase, after.ReadySince, after.ProbeFailures, after.Detail = before.Phase, before.ReadySince, before.ProbeFailures,
			before.Detail
		_, err = tx.UpdateEnvInstance(ctx, cur, after)
		return err
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("restart", "cannot put back an instance whose restart was refused", "sandbox", before.SandboxID, "err", err)
	}
}

// waitReady waits until an instance is live again (true: its phase ready
// and its row running without an op, or, for a service that idles
// (idles), paused or hibernated without one), or gone, or restartReady
// passed (false).
func (c *Controller) waitReady(ctx context.Context, id podid.ID, idles bool) bool {
	deadline := time.Now().Add(c.t.restartReady)
	for time.Now().Before(deadline) {
		inst, err := c.st.GetEnvInstance(ctx, id)
		if err != nil {
			return false
		}
		if inst.Phase == model.PhaseReady {
			sb, err := c.st.GetSandbox(ctx, id)
			if err != nil {
				return false
			}
			if live(store.EnvInstanceRow{Instance: inst, Sandbox: sb}, idles) {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

// restartAttached stops an attached service's process and has the
// supervisor start it again at once. A sandbox that may keep the process
// without running it (paused, hibernated, or with an op in flight) is
// ErrPaused: the process would survive a warm resume beside the new one
// (§13.5); one that cannot keep it (stopped, failed, …) needs no stop.
// The row forgets the process before it stops: a look of the supervisor
// that read the row before finds the process gone, and what it would
// make of that (an exit: a restart, its backoff) does not apply to a row
// that no longer records the process (writeAttached). A stop that fails
// leaves the process to the supervisor, which stops what its row does
// not record before it starts a new one (stopStrays).
func (c *Controller) restartAttached(ctx context.Context, v *view, name string) error {
	a, ok := v.attached[name]
	if !ok {
		return nil
	}
	sb, usable := c.usableSandbox(ctx, v.env, a)
	if !usable {
		return notRunning("%s: no process to restart", goneDetail(a.SandboxID))
	}
	if keepsProcesses(sb) && !steady(sb) {
		return ErrPaused
	}
	l := lease(sb)
	stop := func(id string) error {
		if !steady(sb) || id == "" || c.d.Runner == nil {
			return nil
		}
		if err := c.d.Runner.StopProcessDetached(ctx, l, id); err != nil && !errors.Is(err, tools.ErrNoProcess) {
			return err
		}
		return nil
	}
	var cleared string
	err := c.st.InTx(ctx, func(tx *store.Store) error {
		cur, err := tx.LockAttached(ctx, v.env.ID, name, store.ForUpdate)
		if err != nil {
			return err
		}
		if cur.SandboxID != a.SandboxID {
			return nil // moved meanwhile: the move stopped it
		}
		cleared = cur.ProcessID
		after := cur
		after.ProcessID, after.Phase, after.ReadySince, after.ProbeFailures = "", model.PhaseStarting, time.Time{}, 0
		after.NextRestartAt, after.Detail = time.Time{}, "restarting"
		if _, err := tx.UpdateAttached(ctx, cur, after); err != nil {
			return err
		}
		return tx.Notify(ctx, db.ChanLeader, HintReconcile)
	})
	if err != nil {
		return err
	}
	return stop(cleared)
}

// keepsProcesses reports whether a sandbox in its state keeps its
// processes (or may, an op in flight): running, or suspended in a way a
// warm resume or restore brings back.
func keepsProcesses(sb store.Sandbox) bool {
	switch sb.ClientState {
	case model.StateRunning, model.StatePausing, model.StatePaused, model.StateResuming, model.StateHibernating,
		model.StateHibernated:
		return true
	}
	return sb.Op != ""
}
