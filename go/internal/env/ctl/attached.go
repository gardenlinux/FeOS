// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

// An attached service (§4.6) is a supervised process in the main
// container of a sandbox of its environment (ADR 0035), in the sandbox's
// security groups (it has none of its own). The supervisor looks at it every
// second in env-probe while the sandbox runs (it never wakes a paused one
// for that) and starts, restarts (with the backoff of §4.4), replaces (a
// changed template) and checks its process; the reconcile step records it
// and, when its process must be (re)started in a sandbox that does not
// run, wakes the sandbox with an exec hold for the start and the first
// readiness. Its name is in the environment's name table with the
// sandbox's address while its row names the sandbox and the sandbox has
// one (env-names), which needs nothing of the sandbox. A service
// that goes (removed, moved to another sandbox, or its environment
// deleted) has its process stopped first, in the pool with an exec hold
// that wakes a sleeping sandbox and keeps a running one from pausing
// meanwhile (cleanOld); its row, and with it its name, goes or moves only
// after that.

// stepAttached keeps the attached services of T (§4.3 step 5).
func (c *Controller) stepAttached(ctx context.Context, s *stepState) {
	v := s.v
	for _, name := range slices.Sorted(maps.Keys(v.target.Services)) {
		svc := v.target.Services[name]
		if runtimeOf(svc) != spec.RuntimeAttached {
			continue
		}
		if c.stepOneAttached(ctx, s, name, svc) || !deploying(v) {
			continue
		}
		start, ok := s.start()
		budget := duration("", spec.DefaultRolloutTimeout)
		if !ok || !v.now.After(start.Add(budget)) {
			continue
		}
		reason := fmt.Sprintf("attached service %s did not become ready within %s", name, budget)
		if a, err := c.st.GetAttached(ctx, v.env.ID, name); err == nil && a.Detail != "" {
			reason += ": " + a.Detail
		}
		if err := c.failDeployment(ctx, v, reason, objectName(spec.KindService, name), model.ConditionRolloutFailed,
			evidence{Diff: c.deploymentDiff(ctx, v)}); err == nil {
			s.failed = true
			return
		}
	}
}

// stepOneAttached keeps one attached service of T and reports whether it
// is ready: recorded (with its sandbox's incarnation), moved to the
// sandbox T names, started anew in a new incarnation of its sandbox, its
// sandbox woken for a (re)start. A sandbox it may not use
// (attachedUsable) fails it, and nothing is done in that sandbox.
func (c *Controller) stepOneAttached(ctx context.Context, s *stepState, name string, svc spec.Service) bool {
	v := s.v
	p := c.planAttached(v, name, svc)
	if !p.ok() {
		s.wait(objectName(spec.KindService, name), "attached service %s: %s", name, p.why())
		return false
	}
	sb, err := c.st.GetSandbox(ctx, p.sandbox)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		s.wait(objectName(spec.KindService, name), "attached service %s: %v", name, err)
		return false
	}
	found := err == nil
	a, ok := v.attached[name]
	if !ok {
		c.recordAttached(ctx, s, p, sb, found)
		return false
	}
	if c.d.Runner == nil {
		c.attachedDetail(ctx, v.env.ID, name, "attached services need the tools' runner, which this instance lacks")
		s.wait(objectName(spec.KindService, name), "attached service %s: no tools' runner", name)
		return false
	}
	if a.SandboxID != p.sandbox {
		c.moveAttached(ctx, v, a, p, sb, found)
		s.wait(objectName(spec.KindService, name), "attached service %s moves to sandbox %s", name, p.sandbox)
		return false
	}
	if !found || !attachedUsable(v.env, a, sb) {
		c.attachedGone(ctx, v.env.ID, a)
		s.wait(objectName(spec.KindService, name), "attached service %s: %s", name, goneDetail(p.sandbox))
		return false
	}
	if newIncarnation(a, sb) {
		c.renewAttached(ctx, v.env.ID, a, sb)
		s.wait(objectName(spec.KindService, name), "attached service %s starts anew in a new incarnation of sandbox %s",
			name, sb.ID)
		return false
	}
	needsStart := a.ProcessID == "" || a.TemplateHash != p.hash
	if needsStart && !steady(sb) && sb.Op == "" && (a.NextRestartAt.IsZero() || !v.now.Before(a.NextRestartAt)) {
		c.wakeAttached(ctx, v.env.ID, name, sb.ID)
	}
	if a.Phase != model.PhaseReady || a.TemplateHash != p.hash || a.ProcessID == "" {
		s.wait(objectName(spec.KindService, name), "attached service %s is %s", name, a.Phase)
		return false
	}
	return true
}

// goneDetail is the detail of an attached service whose sandbox it may
// not use (§13.5).
func goneDetail(id podid.ID) string {
	return fmt.Sprintf("sandbox %s is gone or not this tenant's", id)
}

// attachedUsable reports whether an attached service of env may use the
// sandbox row sb, the one it names (§13.6): a sandbox of env (ADR 0035),
// not an instance row or a tenant's job's pod. A sandbox of another
// tenant under a reused ID is never the service's: it is never started,
// checked, named or made a member there. The owner's sandbox deleted and created again under the
// same ID (FeCode replaces a lost workspace sandbox so) is a new
// incarnation, in which the service starts anew (newIncarnation).
// store.EnvCtlMembers and EnvCtlWithAttached apply the same condition.
func attachedUsable(env store.Environment, a store.EnvAttached, sb store.Sandbox) bool {
	return sb.ID == a.SandboxID && sb.Owner == env.Owner && sb.EnvironmentID == env.ID && sb.IsOwn()
}

// newIncarnation reports whether sb, the sandbox a names, is another
// incarnation than the one a recorded (none recorded: no): the process a
// records ran in a sandbox that is gone, with its disk.
func newIncarnation(a store.EnvAttached, sb store.Sandbox) bool {
	return a.SandboxIncarnation != uuid.Nil && a.SandboxIncarnation != sb.Incarnation
}

// renewed is a reset for a new incarnation of its sandbox, sb (§13.6): the
// new incarnation recorded, no process (the old one went with the old
// incarnation), phase starting, restarts, next_restart_at and
// probe_failures reset, so that the process starts again at once.
func renewed(a store.EnvAttached, sb store.Sandbox) store.EnvAttached {
	after := a
	after.SandboxIncarnation, after.ProcessID, after.Phase, after.ReadySince, after.Detail =
		sb.Incarnation, "", model.PhaseStarting, time.Time{}, ""
	after.Restarts, after.NextRestartAt, after.ProbeFailures = 0, time.Time{}, 0
	return after
}

// renewAttached writes renewed onto an attached service's row, while the
// row still names a's sandbox and incarnation.
func (c *Controller) renewAttached(ctx context.Context, envID uuid.UUID, a store.EnvAttached, sb store.Sandbox) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockAttached(ctx, envID, a.Service, store.ForUpdate)
		if err != nil || cur.SandboxID != a.SandboxID || cur.SandboxIncarnation != a.SandboxIncarnation {
			return err
		}
		if _, err := tx.UpdateAttached(ctx, cur, renewed(cur, sb)); err != nil {
			return err
		}
		c.log.Info("an attached service starts anew in a new incarnation of its sandbox", "environment", envID,
			"service", a.Service, "sandbox", sb.ID)
		return tx.Notify(ctx, db.ChanLeader, HintReconcile)
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("attached", "cannot renew an attached service", "service", a.Service, "err", err)
	}
	c.forget("a/" + attachedKey(envID, a.Service))
}

// usableSandbox reads an attached service's sandbox, and reports whether
// the service may use it.
func (c *Controller) usableSandbox(ctx context.Context, env store.Environment, a store.EnvAttached) (store.Sandbox, bool) {
	sb, err := c.st.GetSandbox(ctx, a.SandboxID)
	return sb, err == nil && attachedUsable(env, a, sb)
}

// attachedGone fails an attached service whose sandbox it may not use:
// phase failed, the detail saying so, no process (its process ran in a
// sandbox that is gone); the incarnation it recorded stays, so that a
// sandbox of the owner under its ID again is a new incarnation, in which
// the service starts anew with its counters reset (renewed).
func (c *Controller) attachedGone(ctx context.Context, envID uuid.UUID, a store.EnvAttached) {
	detail := goneDetail(a.SandboxID)
	if a.Phase == model.PhaseFailed && a.Detail == detail && a.ProcessID == "" {
		return
	}
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockAttached(ctx, envID, a.Service, store.ForUpdate)
		if err != nil || cur.SandboxID != a.SandboxID {
			return err
		}
		after := cur
		after.Phase, after.Detail, after.ProcessID, after.ReadySince = model.PhaseFailed, detail, "", time.Time{}
		after.ProbeFailures, after.NextRestartAt = 0, time.Time{}
		if after == cur {
			return nil
		}
		if _, err := tx.UpdateAttached(ctx, cur, after); err != nil {
			return err
		}
		c.log.Warn("an attached service's sandbox is gone or not its tenant's", "environment", envID,
			"service", a.Service, "sandbox", a.SandboxID)
		if err := tx.Notify(ctx, db.ChanLeader, HintGroups); err != nil {
			return err
		}
		return tx.Notify(ctx, db.ChanLeader, HintNames)
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("attached", "cannot fail an attached service", "service", a.Service, "err", err)
	}
	c.forget("a/" + attachedKey(envID, a.Service))
}

// recordAttached records an attached service of T, with the incarnation
// of its sandbox when it may use it (found and the owner's), else failed.
func (c *Controller) recordAttached(ctx context.Context, s *stepState, p *attachedPlan, sb store.Sandbox, found bool) {
	a := store.EnvAttached{EnvID: s.v.env.ID, Service: p.name, SandboxID: p.sandbox, Phase: model.PhaseStarting}
	if found && attachedUsable(s.v.env, a, sb) {
		a.SandboxIncarnation = sb.Incarnation
	} else {
		a.Phase, a.Detail = model.PhaseFailed, goneDetail(p.sandbox)
	}
	err := c.fenced(ctx, func(tx *store.Store) error {
		if _, err := tx.InsertAttached(ctx, a); err != nil {
			return err
		}
		// The sandbox joins the environment's group, and the service's
		// name its table.
		if err := tx.Notify(ctx, db.ChanLeader, HintGroups); err != nil {
			return err
		}
		return tx.Notify(ctx, db.ChanLeader, HintNames)
	})
	if err != nil && !errors.Is(err, store.ErrExists) {
		s.wait(objectName(spec.KindService, p.name), "attached service %s: %v", p.name, err)
		return
	}
	s.wait(objectName(spec.KindService, p.name), "attached service %s is recorded", p.name)
}

// moveAttached moves an attached service to another sandbox: its process
// in the old one stops first (cleanOld: a sleeping old sandbox is woken
// for it, and until that is done the row stays where it is), then the
// row, and with it the service's name, moves to the new sandbox's table
// and starts over, with the new sandbox's incarnation when the service
// may use it. A row that
// changed since the view was read (a process the supervisor recorded
// meanwhile) is left for a later round, which cleans that too.
func (c *Controller) moveAttached(ctx context.Context, v *view, a store.EnvAttached, p *attachedPlan, sb store.Sandbox, found bool) {
	if !c.cleanOld(ctx, v, a, c.clearAttached) {
		return
	}
	inc := uuid.Nil
	if found && attachedUsable(v.env, store.EnvAttached{SandboxID: p.sandbox}, sb) {
		inc = sb.Incarnation
	}
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockAttached(ctx, v.env.ID, a.Service, store.ForUpdate)
		if err != nil {
			return err
		}
		if cur.SandboxID != a.SandboxID || cur.ProcessID != a.ProcessID {
			return nil
		}
		after := cur
		after.SandboxID, after.SandboxIncarnation, after.ProcessID, after.TemplateHash = p.sandbox, inc, "", ""
		after.Phase, after.ReadySince, after.Restarts, after.LastExit = model.PhaseStarting, time.Time{}, 0, nil
		after.ProbeFailures, after.NextRestartAt, after.Detail = 0, time.Time{}, ""
		if _, err := tx.UpdateAttached(ctx, cur, after); err != nil {
			return err
		}
		if err := tx.Notify(ctx, db.ChanLeader, HintGroups); err != nil {
			return err
		}
		return tx.Notify(ctx, db.ChanLeader, HintNames)
	})
	if err != nil && ctx.Err() == nil {
		c.rl.warn("attached", "cannot move an attached service", "environment", v.env.ID, "service", a.Service, "err", err)
	}
}

// stopAttachedIn stops an attached service's process in its sandbox, with
// the sandbox's lease. A process that is not there or not running any more
// counts as stopped.
func (c *Controller) stopAttachedIn(ctx context.Context, a store.EnvAttached, l model.Lease) error {
	if a.ProcessID == "" {
		return nil
	}
	if err := c.d.Runner.StopProcessDetached(ctx, l, a.ProcessID); err != nil && !errors.Is(err, tools.ErrNoProcess) {
		return fmt.Errorf("stop process %s: %w", a.ProcessID, err)
	}
	return nil
}

// leftNothing reports whether the sandbox row sb, the one a names, holds
// nothing of a's to clean: it is not a's to use (another tenant's under a
// reused ID, an instance row), it is a new incarnation (a's process went
// with the old one, and its disk with it), or it is stopped, failed or
// lost (no process outlives a boot).
func leftNothing(env store.Environment, a store.EnvAttached, sb store.Sandbox) bool {
	return !attachedUsable(env, a, sb) || newIncarnation(a, sb) ||
		sb.ClientState == model.StateStopped || sb.ClientState == model.StateFailed || sb.ClientState == model.StateLost
}

// cleanOld stops the process of an attached service in the sandbox its
// row names, before the row goes (removed, or its environment deleted) or
// moves to another sandbox (§13.7), and reports whether nothing is left to
// clean there: the caller may then delete or move the row, while it is
// still as a has it, and the service's name leaves the sandbox's table
// with it. Nothing is left when the service never started, when the
// sandbox is gone, or when leftNothing says so. A running or
// sleeping (paused, hibernated) sandbox is cleaned in the pool
// (cleanInPool), which calls done with the row as it cleaned it; a
// process left in a sleeping sandbox would run on with the environment's
// secrets once it runs again. A sandbox in an op, between states or on a
// node out of reach is looked at again in a later round.
func (c *Controller) cleanOld(ctx context.Context, v *view, a store.EnvAttached,
	done func(context.Context, store.EnvAttached)) bool {
	if a.ProcessID == "" || c.d.Runner == nil {
		return true
	}
	sb, err := c.st.GetSandbox(ctx, a.SandboxID)
	switch {
	case errors.Is(err, model.ErrNotFound):
		return true
	case err != nil:
		return false
	case leftNothing(v.env, a, sb):
		return true
	case sb.Op != "", steady(sb) && !c.connected(sb.Node):
		// A later round.
	case steady(sb), sb.ClientState == model.StatePaused, sb.ClientState == model.StateHibernated:
		c.cleanInPool(ctx, v, a, done)
	}
	return false
}

// cleanKey is the pool's key of the cleanup of an attached service.
func cleanKey(envID uuid.UUID, service string) string {
	return "attached-clean/" + attachedKey(envID, service)
}

// cleanInPool cleans an attached service's sandbox for cleanOld, off the
// loop: Acquire with an exec hold (it wakes a paused or hibernated
// sandbox, and a running one cannot pause until it is released), then,
// under the service's supervision lock and with the row as it is now,
// stop the process (stopAttachedIn), and call done with that row. A sandbox that turns out to hold nothing of the
// service's (leftNothing) is done without a stop. A failure leaves the
// row for a later round, after a backoff (cleanDue).
func (c *Controller) cleanInPool(ctx context.Context, v *view, a store.EnvAttached, done func(context.Context, store.EnvAttached)) {
	key := cleanKey(a.EnvID, a.Service)
	if !c.cleanDue(key) {
		return
	}
	c.pool.run(ctx, key, func(ctx context.Context) {
		failed := func(msg string, err error) {
			if ctx.Err() == nil {
				c.cleanFailed(key)
				c.rl.warn("attached", msg, "environment", a.EnvID, "service", a.Service, "sandbox", a.SandboxID, "err", err)
			}
		}
		l, release, err := c.d.Engine.Acquire(ctx, a.SandboxID, model.HoldExec)
		if err != nil {
			failed("cannot hold a sandbox to stop an attached service that goes", err)
			return
		}
		defer release()
		mu := c.attachedLock(a.EnvID, a.Service)
		mu.Lock()
		defer mu.Unlock()
		cur, err := c.st.GetAttached(ctx, a.EnvID, a.Service)
		if err != nil || cur.SandboxID != a.SandboxID {
			return // gone, or moved meanwhile
		}
		sb, err := c.st.GetSandbox(ctx, a.SandboxID)
		switch {
		case errors.Is(err, model.ErrNotFound):
		case err != nil:
			failed("cannot read the sandbox of an attached service that goes", err)
			return
		case leftNothing(v.env, cur, sb):
		case sb.Incarnation != l.Incarnation:
			return // another incarnation came meanwhile: a later round
		default:
			if err := c.stopAttachedIn(ctx, cur, l); err != nil {
				failed("cannot stop an attached service that goes", err)
				return
			}
			c.log.Info("attached service stopped before its row goes or moves", "environment", a.EnvID, "service", a.Service,
				"sandbox", a.SandboxID, "process", cur.ProcessID)
		}
		c.cleanOK(key)
		done(ctx, cur)
	})
}

// cleanWait is the backoff of an attached service's cleanup that failed.
type cleanWait struct {
	failures int
	next     time.Time
}

// cleanDue reports whether the cleanup of key may run now: not before the
// backoff of its last failure (§4.4's restart backoff) has passed, so that
// a sandbox whose wake keeps failing is not woken every round.
func (c *Controller) cleanDue(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.cleanWaits[key]
	return !ok || !time.Now().Before(w.next)
}

// cleanFailed records a failed cleanup of key.
func (c *Controller) cleanFailed(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cleanWaits == nil {
		c.cleanWaits = map[string]cleanWait{}
	}
	w := c.cleanWaits[key]
	w.failures++
	w.next = time.Now().Add(c.backoff(w.failures))
	c.cleanWaits[key] = w
}

// cleanOK forgets the failures of key's cleanup.
func (c *Controller) cleanOK(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cleanWaits, key)
}

// dropAttached deletes an attached service's row while it still names a's
// sandbox and process, and reports whether the row is gone (also when it
// was gone already); a row changed meanwhile stays for a later round.
func (c *Controller) dropAttached(ctx context.Context, a store.EnvAttached) (bool, error) {
	gone := false
	err := c.fenced(ctx, func(tx *store.Store) error {
		gone = false
		cur, err := tx.LockAttached(ctx, a.EnvID, a.Service, store.ForUpdate)
		switch {
		case errors.Is(err, store.ErrNoRow):
			gone = true
			return nil
		case err != nil:
			return err
		case cur.SandboxID != a.SandboxID || cur.ProcessID != a.ProcessID:
			return nil
		}
		if _, err := tx.DeleteAttached(ctx, a.EnvID, a.Service); err != nil {
			return err
		}
		gone = true
		for _, hint := range []string{HintGroups, HintNames} {
			if err := tx.Notify(ctx, db.ChanLeader, hint); err != nil {
				return err
			}
		}
		return tx.Notify(ctx, db.ChanLeader, HintReconcile)
	})
	if err != nil {
		return false, err
	}
	if gone {
		c.forget("a/" + attachedKey(a.EnvID, a.Service))
		c.cleanOK(cleanKey(a.EnvID, a.Service))
	}
	return gone, nil
}

// dropCleaned is dropAttached as cleanOld's done.
func (c *Controller) dropCleaned(ctx context.Context, a store.EnvAttached) {
	if _, err := c.dropAttached(ctx, a); err != nil && ctx.Err() == nil {
		c.rl.warn("attached", "cannot delete an attached service", "environment", a.EnvID, "service", a.Service, "err", err)
	}
}

// clearAttached is cleanOld's done for a move: the row forgets the process
// it stopped (while it still names it), so that the next round finds
// nothing left in the old sandbox and moves the row.
func (c *Controller) clearAttached(ctx context.Context, a store.EnvAttached) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockAttached(ctx, a.EnvID, a.Service, store.ForUpdate)
		if err != nil || cur.SandboxID != a.SandboxID || cur.ProcessID != a.ProcessID {
			return err
		}
		after := cur
		after.ProcessID, after.Phase, after.ReadySince = "", model.PhaseStarting, time.Time{}
		if _, err := tx.UpdateAttached(ctx, cur, after); err != nil {
			return err
		}
		return tx.Notify(ctx, db.ChanLeader, HintReconcile)
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("attached", "cannot clear a moving attached service", "environment", a.EnvID, "service", a.Service, "err", err)
	}
}

// removeAttached removes an attached service T no longer has: its
// process, then its row, with which its name goes.
func (c *Controller) removeAttached(ctx context.Context, v *view, a store.EnvAttached) {
	if c.cleanOld(ctx, v, a, c.dropCleaned) {
		c.dropCleaned(ctx, a)
	}
}

// attachedDetail writes an attached service's detail.
func (c *Controller) attachedDetail(ctx context.Context, envID uuid.UUID, service, detail string) {
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockAttached(ctx, envID, service, store.ForUpdate)
		if err != nil {
			return err
		}
		after := cur
		after.Detail = detail
		_, err = tx.UpdateAttached(ctx, cur, after)
		return err
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("attached", "cannot write an attached service's detail", "service", service, "err", err)
	}
}

// wakeAttached wakes a sandbox for the (re)start of an attached service's
// process, off the loop: Acquire with an exec hold, supervise until the
// process is ready (or attachedReady passed), release.
func (c *Controller) wakeAttached(ctx context.Context, envID uuid.UUID, service string, sandboxID podid.ID) {
	if c.d.Runner == nil {
		return
	}
	c.pool.run(ctx, "attached/"+attachedKey(envID, service), func(ctx context.Context) {
		// Never wake a sandbox the service may not use.
		env, err := c.st.GetEnvironment(ctx, envID)
		if err != nil {
			return
		}
		if a, err := c.st.GetAttached(ctx, envID, service); err != nil || a.SandboxID != sandboxID {
			return
		} else if _, ok := c.usableSandbox(ctx, env, a); !ok {
			return
		}
		l, release, err := c.d.Engine.Acquire(ctx, sandboxID, model.HoldExec)
		if err != nil {
			if ctx.Err() == nil {
				c.attachedDetail(ctx, envID, service, fmt.Sprintf("sandbox %s could not be woken: %v", sandboxID, err))
			}
			return
		}
		defer release()
		deadline := time.Now().Add(c.t.attachedReady)
		for ctx.Err() == nil && time.Now().Before(deadline) {
			v, err := c.load(ctx, envID)
			if err != nil || v == nil {
				return
			}
			svc, ok := v.target.Services[service]
			if !ok || runtimeOf(svc) != spec.RuntimeAttached {
				return
			}
			sb, err := c.st.GetSandbox(ctx, sandboxID)
			if err != nil {
				return
			}
			p := c.planAttached(v, service, svc)
			if !p.ok() || p.sandbox != sandboxID {
				return
			}
			c.superviseAttached(ctx, v, p, sb, l, true)
			a, err := c.st.GetAttached(ctx, envID, service)
			if err != nil || (a.Phase == model.PhaseReady && a.ProcessID != "") || a.Phase == model.PhaseFailed {
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	})
}

// backoff is the wait before the n-th restart: 2^(n-1) units, at most
// restartMax.
func (c *Controller) backoff(n int) time.Duration {
	d := c.t.restartUnit
	for i := 1; i < n && d < c.t.restartMax; i++ {
		d *= 2
	}
	return min(d, c.t.restartMax)
}

// superviseAttached is one look of the supervisor at an attached service
// in its running sandbox, with the sandbox's lease (§4.6), when the
// service may use the sandbox (attachedUsable; else the reconcile step
// fails the service): a new incarnation of the sandbox starts the service
// anew (renewed); a changed template stops the old process; a process
// that is gone counts as an exit, restarted after the backoff; a working
// directory that does not exist yet keeps it starting; a running process
// is checked (readiness). It lists only the processes of the service's
// name. wait: wait for another look
// in flight instead of leaving it to that one.
func (c *Controller) superviseAttached(ctx context.Context, v *view, p *attachedPlan, sb store.Sandbox, l model.Lease, wait bool) {
	if c.d.Runner == nil {
		return
	}
	mu := c.attachedLock(v.env.ID, p.name)
	if wait {
		mu.Lock()
	} else if !mu.TryLock() {
		return
	}
	defer mu.Unlock()
	a, err := c.st.GetAttached(ctx, v.env.ID, p.name)
	if err != nil || !attachedUsable(v.env, a, sb) || l.SandboxID != sb.ID || l.Incarnation != sb.Incarnation {
		return
	}
	after := a
	if newIncarnation(a, sb) {
		after = renewed(a, sb)
		c.log.Info("an attached service starts anew in a new incarnation of its sandbox", "environment", v.env.ID,
			"service", p.name, "sandbox", sb.ID)
	}
	now := c.dbNow(ctx)
	if after.ProcessID != "" && after.TemplateHash != p.hash {
		if err := c.d.Runner.StopProcessDetached(ctx, l, after.ProcessID); err != nil && !errors.Is(err, tools.ErrNoProcess) {
			after.Detail = "stop the old process: " + err.Error()
			c.writeAttached(ctx, a, after, l)
			return
		}
		after.ProcessID, after.Phase, after.ReadySince, after.ProbeFailures = "", model.PhaseStarting, time.Time{}, 0
		after.NextRestartAt = time.Time{}
	}
	if after.ProcessID != "" {
		// The recorded process by its ID too: one started under the
		// environment's name, by a ferund before processName took its ID,
		// is the service's process still.
		ps, err := c.d.Runner.ProcessesDetached(ctx, l, processName(v.env.ID, p.name), after.ProcessID)
		if err != nil {
			after.Detail = "list the processes: " + err.Error()
			c.writeAttached(ctx, a, after, l)
			return
		}
		var st *tools.ProcessStatus
		for i := range ps {
			if ps[i].ID == after.ProcessID {
				st = &ps[i]
			}
		}
		if st == nil || !st.Running {
			after.Restarts++
			after.LastExit, after.Detail = nil, "the process is gone"
			if st != nil && st.ExitCode != nil {
				code := *st.ExitCode
				after.LastExit, after.Detail = &code, fmt.Sprintf("the process exited with code %d", code)
			}
			after.ProcessID, after.Phase, after.ReadySince, after.ProbeFailures = "", model.PhaseExited, time.Time{}, 0
			after.NextRestartAt = now.Add(c.backoff(after.Restarts))
		}
	}
	if after.ProcessID == "" && (after.NextRestartAt.IsZero() || !now.Before(after.NextRestartAt)) {
		if err := c.stopStrays(ctx, v, p.name, sb, l); err != nil {
			after.Detail = "stop the service's other processes: " + err.Error()
			c.writeAttached(ctx, a, after, l)
			return
		}
		c.startAttached(ctx, v, p, sb, l, &after)
	}
	if after.ProcessID != "" {
		c.attachedReadiness(ctx, p, l, a, &after, now)
	}
	keepAwake(&after, a, p, now, duration("", spec.DefaultRolloutTimeout))
	c.writeAttached(ctx, a, after, l)
}

// keepAwake sets what the service claims of its sandbox (ADR 0018, "An
// attached service idles as an image service does"): a process started
// by this look keeps it running until it is ready, for at most budget
// from its start; a ready one, or one whose process is gone, no longer
// does; once it runs, its idle (the plan's) applies.
func keepAwake(after *store.EnvAttached, before store.EnvAttached, p *attachedPlan, now time.Time, budget time.Duration) {
	switch {
	case after.ProcessID == "" || after.Phase == model.PhaseReady:
		after.AwakeUntil = time.Time{}
	case after.ProcessID != before.ProcessID:
		after.AwakeUntil = now.Add(budget)
	}
	if (p.idle == nil) != (before.IdleAfterS == nil) || (p.idle != nil && *p.idle != *before.IdleAfterS) {
		after.IdleAfterS = p.idle
	} else {
		after.IdleAfterS = before.IdleAfterS // the same: no write
	}
}

// processName is the name of an attached service's process (§13.4):
// of its environment's ID, which a rename leaves (ADR 0037), so that the
// supervisor finds the process under the same name after one.
func processName(env uuid.UUID, service string) string {
	return "ferun-env-" + env.String() + "-" + service
}

// legacyProcessName is the name a ferund before ADR 0037 gave the process,
// of the environment's name. stopStrays looks for it too, until no such
// process is left; one of a name the environment had before a rename is
// found only while its row records it (superviseAttached lists it by ID).
func legacyProcessName(env, service string) string { return "ferun-env-" + env + "-" + service }

// stopStrays stops the processes of an attached service that its row does
// not record, before a new one starts (§13.5): running processes in the
// sandbox with its name (or its legacy name), but those another attached
// service of the sandbox records (an environment and service whose legacy
// names join to the same name). Such a process was started by a look
// whose write was lost (a leader change, a row moved or restarted
// meanwhile), or survived a restart of a paused sandbox; it would hold
// the port and the secrets.
func (c *Controller) stopStrays(ctx context.Context, v *view, service string, sb store.Sandbox, l model.Lease) error {
	var strays []string
	for _, name := range []string{processName(v.env.ID, service), legacyProcessName(v.env.Name, service)} {
		ps, err := c.d.Runner.ProcessesDetached(ctx, l, name)
		if err != nil {
			return err
		}
		for _, pr := range ps {
			if pr.Running && pr.Name == name {
				strays = append(strays, pr.ID)
			}
		}
	}
	if len(strays) == 0 {
		return nil
	}
	others, err := c.st.AttachedOfSandbox(ctx, sb.ID)
	if err != nil {
		return err
	}
	for _, id := range strays {
		if slices.ContainsFunc(others, func(o store.EnvAttached) bool {
			return o.ProcessID == id && (o.EnvID != v.env.ID || o.Service != service)
		}) {
			continue
		}
		if err := c.d.Runner.StopProcessDetached(ctx, l, id); err != nil && !errors.Is(err, tools.ErrNoProcess) {
			return err
		}
		c.log.Warn("an attached service's stray process stopped", "environment", v.env.ID, "service", service,
			"sandbox", sb.ID, "process", id)
	}
	return nil
}

// startAttached starts an attached service's process: its env resolved
// here (secrets too), never written to disk.
func (c *Controller) startAttached(ctx context.Context, v *view, p *attachedPlan, sb store.Sandbox, l model.Lease, after *store.EnvAttached) {
	env, err := c.resolveEnv(ctx, v.env.ID, p.env)
	if err != nil {
		after.Phase, after.Detail = model.PhaseStarting, "its env: "+err.Error()
		return
	}
	id, err := c.d.Runner.StartProcessDetached(ctx, l, tools.ProcessStart{Argv: p.argv, Cwd: p.cwd, Env: env.vars,
		Name: processName(v.env.ID, p.name)})
	switch {
	case errors.Is(err, tools.ErrNoCwd):
		after.Phase, after.Detail = model.PhaseStarting, fmt.Sprintf("cwd %s does not exist yet", p.cwd)
	case err != nil:
		after.Detail = "start the process: " + redact(err.Error(), env.secrets)
	default:
		after.ProcessID, after.SandboxIncarnation, after.TemplateHash = id, sb.Incarnation, p.hash
		after.Phase, after.ReadySince, after.ProbeFailures, after.NextRestartAt, after.Detail =
			model.PhaseStarting, time.Time{}, 0, time.Time{}, ""
		c.log.Info("attached service started", "environment", v.env.ID, "service", p.name, "sandbox", sb.ID, "process", id)
	}
}

// attachedReadiness checks a running process's readiness (§4.4) and moves
// its phase.
func (c *Controller) attachedReadiness(ctx context.Context, p *attachedPlan, l model.Lease, before store.EnvAttached,
	after *store.EnvAttached, now time.Time) {
	class := "up"
	if after.Phase != model.PhaseReady && after.Phase != model.PhaseUnready {
		class = "starting"
	}
	key := "a/" + attachedKey(before.EnvID, before.Service)
	seen, due := c.seenDue(key, after.ProcessID+"/"+class, p.check)
	if !due {
		return
	}
	ok, why := c.runCheck(ctx, l, p.check, seen)
	if ok || why != errNoVerdict.Error() {
		applyCheck(ok, why, p.check, now, &after.Phase, &after.ReadySince, &after.ProbeFailures, &after.Detail)
	}
	if after.Phase == model.PhaseReady && after.Restarts > 0 && !after.ReadySince.IsZero() &&
		now.Sub(after.ReadySince) >= c.t.crashWindow {
		after.Restarts = 0
	}
}

// writeAttached writes what the supervisor changed of an attached
// service onto the row as it is now, while the row still names the
// sandbox and records the process the look read: a row that forgot that
// process meanwhile (a restart on request, a failed or renewed service)
// is not the look's to write, and the look's finding (the process gone:
// an exit) is not about the row's process. A process the look started
// (with the lease l) whose ID the row does not get (the row moved, went
// or changed meanwhile, the write failed) is stopped: nothing would know
// of it.
func (c *Controller) writeAttached(ctx context.Context, before, after store.EnvAttached, l model.Lease) {
	if before == after {
		return
	}
	written := false
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockAttached(ctx, before.EnvID, before.Service, store.ForUpdate)
		if err != nil {
			return err
		}
		if cur.SandboxID != before.SandboxID || cur.ProcessID != before.ProcessID {
			return nil // moved, or its process changed meanwhile
		}
		written = true
		next := cur
		next.SandboxIncarnation, next.TemplateHash, next.ProcessID = after.SandboxIncarnation, after.TemplateHash, after.ProcessID
		next.Phase, next.ReadySince, next.Restarts, next.LastExit = after.Phase, after.ReadySince, after.Restarts, after.LastExit
		next.ProbeFailures, next.NextRestartAt, next.Detail = after.ProbeFailures, after.NextRestartAt, after.Detail
		next.AwakeUntil, next.IdleAfterS = after.AwakeUntil, after.IdleAfterS
		if _, err := tx.UpdateAttached(ctx, cur, next); err != nil {
			return err
		}
		if cur.Phase != next.Phase {
			return tx.Notify(ctx, db.ChanLeader, HintReconcile)
		}
		return nil
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("attached", "cannot write an attached service", "service", before.Service, "err", err)
	}
	if (err != nil || !written) && after.ProcessID != "" && after.ProcessID != before.ProcessID {
		if err := c.d.Runner.StopProcessDetached(ctx, l, after.ProcessID); err != nil && !errors.Is(err, tools.ErrNoProcess) &&
			ctx.Err() == nil {
			c.rl.warn("attached", "cannot stop a process whose start was not recorded", "service", before.Service, "err", err)
		}
	}
}
