// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/addons"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/ports"
	"github.com/gardenlinux/feos/go/internal/store"
)

// probeConcurrency bounds the probes in flight on this instance (§4.2).
const probeConcurrency = 16

// seenState is what the probe loop remembers of an instance or an
// attached service: since when it has been seen running in the run (and
// phase class) token names, and when it was checked last.
type seenState struct {
	token string
	since time.Time
	last  time.Time
}

// seenDue returns the instance's seen state, reset when token is not the
// one it was seen in, and whether a check is due: the check's initial
// delay passed since it was first seen running, and its period since the
// last check (which it then marks done).
func (c *Controller) seenDue(key, token string, chk check) (seenState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	s := c.seen[key]
	if s == nil || s.token != token {
		s = &seenState{token: token, since: now}
		c.seen[key] = s
	}
	// Without a check, every look counts (the steady time decides).
	if now.Sub(s.since) < chk.delay || (chk.kind != "" && !s.last.IsZero() && now.Sub(s.last) < chk.period) {
		return *s, false
	}
	s.last = now
	return *s, true
}

// prune drops what the controller remembers of instances, attached
// services and environments that a probe pass no longer saw.
func (c *Controller) prune(live map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.seen {
		if !live[k] {
			delete(c.seen, k)
		}
	}
	for id := range c.provisionFailed {
		if !live[id.String()] {
			delete(c.provisionFailed, id)
		}
	}
	for id := range c.exitRun {
		if !live[id.String()] {
			delete(c.exitRun, id)
		}
	}
	for k := range c.lastPending {
		if !live[k] {
			delete(c.lastPending, k)
		}
	}
	for k, m := range c.locks {
		if !live["a/"+k] && m.TryLock() {
			delete(c.locks, k)
			m.Unlock()
		}
	}
}

// forget drops what the probe loop remembers of key.
func (c *Controller) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.seen, key)
}

// errNoVerdict is a check that could not run for a reason of this
// instance's (every probe slot of the sandbox in use): it counts neither
// way.
var errNoVerdict = errors.New("no verdict")

// runCheck runs a readiness check against the lease's pod: http (a GET
// over a port-forward, 200..399), tcp (a port-forward answered), exec
// (exit 0 in the main container); none: ready once it has run steadyAfter
// since it was first seen. A check without a verdict answers why
// errNoVerdict.Error().
func (c *Controller) runCheck(ctx context.Context, l model.Lease, chk check, seen seenState) (bool, string) {
	switch chk.kind {
	case "":
		if time.Since(seen.since) >= c.t.steadyAfter {
			return true, ""
		}
		return false, "starting"
	case "http", "tcp":
		if c.d.Ports == nil {
			return false, "the port door is not available on this instance"
		}
		if chk.port <= 0 {
			return false, "the readiness check names no port of the service"
		}
		if chk.kind == "tcp" {
			conn, err := c.d.Ports.DialPod(ctx, l, chk.port)
			switch {
			case errors.Is(err, ports.ErrProbeBusy):
				return false, errNoVerdict.Error()
			case err != nil:
				return false, fmt.Sprintf("tcp port %d: %v", chk.port, err)
			}
			conn.Close()
			return true, ""
		}
		code, err := c.d.Ports.Probe(ctx, l, chk.port, chk.path)
		switch {
		case errors.Is(err, ports.ErrProbeBusy):
			return false, errNoVerdict.Error()
		case err != nil:
			return false, fmt.Sprintf("GET %s on port %d: %v", chk.path, chk.port, err)
		case code < 200 || code > 399:
			return false, fmt.Sprintf("GET %s on port %d answered %d", chk.path, chk.port, code)
		}
		return true, ""
	case "exec":
		if c.d.Runner == nil {
			return false, "exec checks need the tools' runner, which this instance lacks"
		}
		code, stdout, stderr, err := c.d.Runner.ExecDetached(ctx, l, chk.argv, nil, "", c.t.probeTimeout)
		switch {
		case err != nil:
			return false, fmt.Sprintf("%s: %v", chk.argv[0], err)
		case code != 0:
			return false, fmt.Sprintf("%s exited %d: %s", chk.argv[0], code, lastLines(strings.TrimSpace(stderr+"\n"+stdout), 2))
		}
		return true, ""
	}
	return false, "unknown check " + chk.kind
}

// applyCheck moves a phase by a check's outcome (§4.4): starting →
// ready on the first success; ready → unready after threshold failures in
// a row; unready → ready on a success. A success clears the detail the
// checks (or the start) wrote, never one of a ready instance that passes
// its checks (a failed provisioning's, which stays until it succeeds).
func applyCheck(ok bool, why string, chk check, now time.Time, phase *string, readySince *time.Time, failures *int, detail *string) {
	if ok {
		if *phase != model.PhaseReady || *failures > 0 {
			*detail = ""
		}
		if *phase != model.PhaseReady {
			*phase, *readySince = model.PhaseReady, now
		}
		*failures = 0
		return
	}
	*failures++
	*detail = why
	if *phase == model.PhaseReady && *failures >= max(chk.threshold, 1) {
		*phase, *readySince = model.PhaseUnready, time.Time{}
	}
}

// probe is env-probe: the instances whose rows run without an op on a
// node this instance is connected to, and the attached services.
func (c *Controller) probe(ctx context.Context) error {
	envs, err := c.st.ListEnvironments(ctx, "", nil)
	if err != nil {
		return err
	}
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	// What the pass looked at: the memory of everything else goes.
	live := map[string]bool{}
	defer func() {
		wg.Wait()
		if ctx.Err() == nil {
			c.prune(live)
		}
	}()
	run := func(f func()) bool {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return false
		}
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			f()
		}()
		return true
	}
	for _, e := range envs {
		if e.Deleting || e.Deployment == 0 {
			continue
		}
		v, err := c.load(ctx, e.ID)
		if err != nil || v == nil {
			continue
		}
		for _, r := range v.rows {
			live[r.Sandbox.ID.String()] = true
			if r.Instance.Phase == model.PhaseStopping || !steady(r.Sandbox) || !c.connected(r.Sandbox.Node) {
				continue
			}
			if !run(func() { c.probeInstance(ctx, v, r) }) {
				return nil
			}
		}
		live["pending/"+e.ID.String()] = true
		for name, a := range v.attached {
			live["a/"+attachedKey(e.ID, name)] = true
			svc, ok := v.target.Services[name]
			if !ok || runtimeOf(svc) != spec.RuntimeAttached {
				continue
			}
			sb, err := c.st.GetSandbox(ctx, a.SandboxID)
			if err != nil || !attachedUsable(v.env, a, sb) || !steady(sb) || !c.connected(sb.Node) {
				continue
			}
			if !run(func() {
				p := c.planAttached(v, name, svc)
				if p.ok() && p.sandbox == sb.ID {
					c.superviseAttached(ctx, v, p, sb, lease(sb), false)
				}
			}) {
				return nil
			}
		}
	}
	c.probeTenantJobs(ctx, run)
	return nil
}

// instanceCheck is the readiness check of an instance's object: as T has
// it, else as R has it.
func (c *Controller) instanceCheck(v *view, inst store.EnvInstance) check {
	switch inst.Kind {
	case spec.KindService:
		svc, ok := v.target.Services[inst.Object]
		if !ok {
			svc = v.latestSpec.Services[inst.Object]
		}
		return checkOf(svc)
	case spec.KindAddon:
		a, ok := v.target.Addons[inst.Object]
		if !ok {
			a = v.latestSpec.Addons[inst.Object]
		}
		if bp, ok := c.d.Blueprint(a.Type); ok {
			if in, err := bp.Render(specAddon(bp, inst.Object, a), addonsContext(v, inst.Object)); err == nil && len(in.Probe) > 0 {
				return check{kind: "exec", argv: in.Probe, period: duration("", spec.DefaultPeriod),
					threshold: spec.DefaultFailureThreshold}
			}
		}
	}
	return check{period: duration("", spec.DefaultPeriod), threshold: spec.DefaultFailureThreshold}
}

// probeInstance is one look at an instance whose row runs (§4.4): the
// main container's exit (a job's end; a service's or an addon's restart
// with backoff, crash_loop), the restart when due, the readiness.
func (c *Controller) probeInstance(ctx context.Context, v *view, r store.EnvInstanceRow) {
	inst, sb := r.Instance, r.Sandbox
	info, err := c.getPod(ctx, sb.Node, sb.ID)
	if err != nil || info.GetState() != podv1.PodState_POD_STATE_RUNNING {
		return
	}
	mc := mainContainer(info)
	if exited, code := exitOf(mc); exited {
		if inst.Kind == spec.KindJob {
			c.finishJob(ctx, v, r, code, "")
			return
		}
		pid := info.GetMicrovm().GetPid()
		if inst.Phase == model.PhaseExited && inst.NextRestartAt.IsZero() && pid != 0 && c.exitRunOf(sb.ID) == pid {
			// The restart did not happen (its repair was skipped, "repair
			// not done"): the same run of the container, not another exit.
			c.restartAgain(ctx, v, r)
			return
		}
		if inst.Phase != model.PhaseExited || inst.NextRestartAt.IsZero() {
			c.instanceExited(ctx, v, r, code, pid)
			return
		}
		if !v.now.Before(inst.NextRestartAt) {
			c.restartInstance(ctx, r)
		}
		return
	}
	if mc != nil && mc.GetState() != podv1.ContainerState_CONTAINER_STATE_RUNNING {
		return
	}
	if inst.Kind == spec.KindJob {
		return
	}
	after := inst
	if inst.Phase == model.PhaseExited || inst.Phase == model.PhaseFailed {
		// Restarted: a new run of the container.
		after.Phase, after.NextRestartAt, after.ReadySince, after.ProbeFailures = model.PhaseStarting, time.Time{}, time.Time{}, 0
		after.Detail = ""
	}
	class := "up"
	if after.Phase == model.PhaseStarting {
		class = "starting"
	}
	chk := c.instanceCheck(v, inst)
	seen, due := c.seenDue(sb.ID.String(), fmt.Sprintf("%s/%d/%s", sb.Incarnation, inst.Restarts, class), chk)
	if due {
		ok, why := c.runCheck(ctx, lease(sb), chk, seen)
		if !ok && inst.Kind == spec.KindAddon {
			why = redact(why, c.addonSecrets(ctx, v, inst.Object))
		}
		if ok || why != errNoVerdict.Error() {
			applyCheck(ok, why, chk, v.now, &after.Phase, &after.ReadySince, &after.ProbeFailures, &after.Detail)
		}
	}
	if after.Phase == model.PhaseReady && after.Restarts > 0 && !after.ReadySince.IsZero() &&
		v.now.Sub(after.ReadySince) >= c.t.crashWindow {
		after.Restarts = 0
	}
	c.writeInstance(ctx, inst, after)
	// A crash loop is over once every instance of the object has been
	// ready for the crash window (the crash-looping one, or those that
	// replaced it).
	if obj := objectName(inst.Kind, inst.Object); v.isOpen(obj, model.ConditionCrashLoop) &&
		c.objectSteady(v, inst.Kind, inst.Object, after) {
		c.closeIncidents(ctx, v, obj, model.ConditionCrashLoop)
	}
}

// objectSteady reports whether every instance of an object (but those
// stopping) has been ready for the crash window, with after as one of
// them is now, each counted as live counts it (a ready phase of a row
// that does not run is not ready).
func (c *Controller) objectSteady(v *view, kind, object string, after store.EnvInstance) bool {
	idles := false
	if svc, ok := v.target.Services[object]; ok && kind == spec.KindService {
		idles = idleOf(svc) > 0
	}
	n := 0
	for _, r := range v.rowsOf(kind, object) {
		if r.Instance.SandboxID == after.SandboxID {
			r.Instance = after
		}
		i := r.Instance
		if i.Phase == model.PhaseStopping {
			continue
		}
		if !live(r, idles) || i.ReadySince.IsZero() || v.now.Sub(i.ReadySince) < c.t.crashWindow {
			return false
		}
		n++
	}
	return n > 0
}

// addonSecrets are an addon's credential values as T and R have it (an
// addon kept as R has it while R failed has R's), for redaction.
func (c *Controller) addonSecrets(ctx context.Context, v *view, addon string) []string {
	var keys []string
	var bp addons.Blueprint
	for _, sp := range []*spec.Spec{v.target, v.latestSpec} {
		a, ok := sp.Addons[addon]
		if !ok {
			continue
		}
		b, ok := c.d.Blueprint(a.Type)
		if !ok || (bp != nil && b.Type() != bp.Type()) {
			continue
		}
		bp = b
		for _, k := range b.Credentials(specAddon(b, addon, a)) {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return c.credentialValues(ctx, v.env.ID, addon, keys)
}

// writeInstance writes what a probe changed of an instance onto the row
// as it is now, and wakes env-reconcile when its phase moved.
func (c *Controller) writeInstance(ctx context.Context, before, after store.EnvInstance) {
	if before.Phase == after.Phase && before.ReadySince.Equal(after.ReadySince) && before.ProbeFailures == after.ProbeFailures &&
		before.Detail == after.Detail && before.Restarts == after.Restarts &&
		before.NextRestartAt.Equal(after.NextRestartAt) {
		return
	}
	err := c.fenced(ctx, func(tx *store.Store) error {
		cur, err := tx.LockEnvInstance(ctx, before.SandboxID, store.ForUpdate)
		if err != nil {
			return err
		}
		if cur.Phase != before.Phase || cur.Restarts != before.Restarts {
			return nil // changed meanwhile: the next look decides
		}
		next := cur
		next.Phase, next.ReadySince, next.ProbeFailures, next.Detail = after.Phase, after.ReadySince, after.ProbeFailures, after.Detail
		next.Restarts, next.NextRestartAt = after.Restarts, after.NextRestartAt
		if _, err := tx.UpdateEnvInstance(ctx, cur, next); err != nil {
			return err
		}
		if cur.Phase != next.Phase {
			// A replica that becomes ready or stops being so changes
			// its environment's names.
			if err := tx.Notify(ctx, db.ChanLeader, HintNames); err != nil {
				return err
			}
			return tx.Notify(ctx, db.ChanLeader, HintReconcile)
		}
		return nil
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("probe", "cannot write an instance's phase", "sandbox", before.SandboxID, "err", err)
	}
}

// exitRunOf is the microVM pid at the instance's last recorded exit (0:
// none known).
func (c *Controller) exitRunOf(id podid.ID) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitRun[id]
}

// restartAgain makes the restart of an exited instance due again after
// its backoff, when the last one did not happen: its exit is not counted
// again, its restarts do not grow.
func (c *Controller) restartAgain(ctx context.Context, v *view, r store.EnvInstanceRow) {
	wait := c.backoff(max(r.Instance.Restarts, 1))
	after := r.Instance
	after.NextRestartAt = v.now.Add(wait)
	after.Detail = "the restart did not happen; trying again in " + wait.String()
	if why := r.Sandbox.StatusDescription; why != "" {
		after.Detail = fmt.Sprintf("the restart did not happen (%s); trying again in %s", why, wait)
	}
	c.writeInstance(ctx, r.Instance, after)
}

// instanceExited records an exit of a service's or an addon's container
// (§4.4): restarts counted, the exit kept (the last five), the restart
// due after the backoff; 3 exits within 10 minutes open crash_loop with
// the exits and the log tail (storedTail: redacted, bounded). pid is the
// microVM's (0: unknown), remembered with the exit.
func (c *Controller) instanceExited(ctx context.Context, v *view, r store.EnvInstanceRow, code *int, pid int64) {
	tail := c.storedTail(ctx, v, r.Sandbox)
	obj := objectName(r.Instance.Kind, r.Instance.Object)
	recorded := false
	err := c.fenced(ctx, func(tx *store.Store) error {
		recorded = false
		inst, err := tx.LockEnvInstance(ctx, r.Sandbox.ID, store.ForUpdate)
		if err != nil {
			return err
		}
		if inst.Phase == model.PhaseStopping || (inst.Phase == model.PhaseExited && !inst.NextRestartAt.IsZero()) {
			return nil
		}
		recorded = true
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		after := inst
		after.Restarts++
		after.LastExit = code
		after.Exits = appendExit(inst.Exits, store.EnvExit{At: now, Code: code})
		after.Phase, after.ReadySince, after.ProbeFailures = model.PhaseExited, time.Time{}, 0
		wait := c.backoff(after.Restarts)
		after.NextRestartAt = now.Add(wait)
		after.Detail = "exited without a code; restarting in " + wait.String()
		if code != nil {
			after.Detail = fmt.Sprintf("exited with code %d; restarting in %s", *code, wait)
		}
		if _, err := tx.UpdateEnvInstance(ctx, inst, after); err != nil {
			return err
		}
		recent := 0
		for _, e := range after.Exits {
			if now.Sub(e.At) <= c.t.crashWindow {
				recent++
			}
		}
		if recent >= c.t.crashExits {
			last := "without a code"
			if code != nil {
				last = fmt.Sprintf("with code %d", *code)
			}
			summary := fmt.Sprintf("%s (deployment %d) exited %d times within %s, last %s", r.Instance.Object, inst.Deployment,
				recent, spell(c.t.crashWindow), last)
			if _, err := openIncident(ctx, tx, v.env.ID, obj, inst.Deployment, model.ConditionCrashLoop, summary,
				evidence{Exits: after.Exits, LogTail: tail}); err != nil {
				return err
			}
		}
		return tx.Notify(ctx, db.ChanLeader, HintReconcile)
	})
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
			c.rl.warn("probe", "cannot record an exit", "sandbox", r.Sandbox.ID, "err", err)
		}
		return
	}
	if !recorded {
		return
	}
	c.mu.Lock()
	c.exitRun[r.Sandbox.ID] = pid
	c.mu.Unlock()
	c.log.Info("instance exited", "environment", v.env.ID, "object", obj, "replica", r.Instance.Replica, "code", code)
}

// restartInstance restarts an exited instance's pod (Engine.Restart, a
// repair op of trigger restart: StopPod and StartPod); the restart due is
// cleared, so that its container's next exit counts anew.
func (c *Controller) restartInstance(ctx context.Context, r store.EnvInstanceRow) {
	reason := fmt.Sprintf("restart %d after the container exited", r.Instance.Restarts)
	if err := c.d.Engine.Restart(ctx, r.Sandbox.ID, reason); err != nil {
		if !errors.Is(err, model.ErrBusy) && !errors.Is(err, model.ErrPrecondition) && ctx.Err() == nil {
			c.rl.warn("restart", "cannot restart an instance", "sandbox", r.Sandbox.ID, "err", err)
		}
		return
	}
	after := r.Instance
	after.NextRestartAt = time.Time{}
	c.writeInstance(ctx, r.Instance, after)
}

// addonsContext is what an addon of the view renders against, without
// an address.
func addonsContext(v *view, name string) addons.Context {
	return addons.Context{Env: v.env.Name, Name: name}
}

// spell writes a duration as the incidents' summaries do: whole minutes
// as "10 minutes", anything else as Go writes it.
func spell(d time.Duration) string {
	switch {
	case d == time.Minute:
		return "1 minute"
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%d minutes", d/time.Minute)
	}
	return d.String()
}
