// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// reconcileConcurrency is how many environments one pass steps at once.
const reconcileConcurrency = 4

// reconcile is env-reconcile: one step per environment that has a
// release and is not being deleted (env-delete has those).
func (c *Controller) reconcile(ctx context.Context) error {
	envs, err := c.st.ListEnvironments(ctx, "", nil)
	if err != nil {
		return err
	}
	sem := make(chan struct{}, reconcileConcurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, e := range envs {
		if e.Deleting || e.Release == 0 {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		wg.Add(1)
		go func(id uuid.UUID) {
			defer func() {
				<-sem
				wg.Done()
			}()
			if err := c.step(ctx, id); err != nil && ctx.Err() == nil && !errors.Is(err, leader.ErrNotLeader) &&
				!errors.Is(err, store.ErrNoRow) {
				c.rl.warn("step/"+id.String(), "an environment's step failed", "environment", id, "err", err)
			}
		}(e.ID)
	}
	return nil
}

// stepState is what one step learns while it goes.
type stepState struct {
	v      *view
	addons map[string]*addonPlan
	// addonsReady: every addon of T is ready and provisioned.
	addonsReady bool
	// jobsDone: the release jobs of T succeeded (or T is not rolling).
	jobsDone bool
	// pending lists what the rolling release still waits for.
	pending []pending
	// reason is shown on a rolling release that cannot go on ("secrets
	// need --secret-key-file"); "" for none.
	reason string
	// failed: the step failed R.
	failed bool
	// shift is how long R's release jobs waited for runs that were not
	// R's (jobWait): R's budgets count from its start moved on by it.
	shift time.Duration
	// shiftUnknown: the shift could not be read in this step, so no
	// budget is judged in it.
	shiftUnknown bool
}

// start is when R's budgets count from (its deadline, its services'
// rollout budgets): R's start moved on by the time its release jobs
// waited for runs that were not R's; false while R has not started or
// the shift is not known.
func (s *stepState) start() (time.Time, bool) {
	if s.v.latest.StartedAt.IsZero() || s.shiftUnknown {
		return time.Time{}, false
	}
	return s.v.latest.StartedAt.Add(s.shift), true
}

// pending is one thing a rolling release waits for: the object ("" for
// none) and what of it.
type pending struct {
	object, text string
}

// wait records that the release waits for object (format and args say
// what of it).
func (s *stepState) wait(object, format string, args ...any) {
	s.pending = append(s.pending, pending{object: object, text: fmt.Sprintf(format, args...)})
}

// texts are what the pending say.
func (s *stepState) texts() []string {
	out := make([]string, len(s.pending))
	for i, p := range s.pending {
		out[i] = p.text
	}
	return out
}

// rolling reports whether T is R and R is pending or rolling: its jobs
// run, its budget counts, and it may become realised.
func rolling(v *view) bool {
	return v.targetIsLatest && (v.latest.State == model.ReleasePending || v.latest.State == model.ReleaseRolling)
}

// step is one reconcile step of an environment (§4.3).
func (c *Controller) step(ctx context.Context, envID uuid.UUID) error {
	v, err := c.load(ctx, envID)
	if err != nil || v == nil {
		return err
	}
	if v.targetIsLatest && v.latest.State == model.ReleasePending {
		if err := c.startRollout(ctx, v); err != nil {
			return err
		}
	}
	s := &stepState{v: v, addonsReady: true, jobsDone: true}
	c.stepAddons(ctx, s)
	c.stepJobs(ctx, s)
	c.stepServices(ctx, s)
	c.stepAttached(ctx, s)
	c.stepRemoved(ctx, s)
	c.stepRetiring(ctx, s)
	c.stepStarved(ctx, s)
	c.stepNames(ctx, s)
	c.logPending(v, s.texts())
	switch {
	case s.failed || !rolling(v):
	case len(s.pending) == 0:
		return c.realise(ctx, v)
	case s.reason == "" && c.pastDeadline(s):
		return c.failLate(ctx, s)
	case s.reason != v.latest.Reason:
		return c.setReason(ctx, v, s.reason)
	}
	return nil
}

// deadline is how long R may roll (§13.5): the longest of T's image
// services' rollout budgets, of T's release jobs' attempts (timeout ×
// (retries+1)), and the floor (10 minutes).
func (c *Controller) deadline(v *view) time.Duration {
	d := c.t.releaseDeadline
	for _, svc := range v.target.Services {
		if runtimeOf(svc) == spec.RuntimeImage {
			d = max(d, rolloutOf(svc))
		}
	}
	for _, j := range v.target.Jobs {
		if jobOn(j) == spec.JobOnRelease {
			d = max(d, jobTimeout(j)*time.Duration(max(j.Retries, 0)+1))
		}
	}
	return d
}

// within is how long R may roll from its own start: its deadline, plus
// the time its release jobs waited for runs that were not R's
// (stepState.shift).
func (c *Controller) within(s *stepState) time.Duration {
	return c.deadline(s.v) + s.shift
}

// pastDeadline reports whether R has rolled longer than its deadline,
// counted from its shifted start (stepState.start).
func (c *Controller) pastDeadline(s *stepState) bool {
	start, ok := s.start()
	return ok && s.v.now.After(start.Add(c.deadline(s.v)))
}

// failLate fails a rolling R past its deadline (rollout_failed), naming
// the first object it still waits for; the evidence has what it waits
// for and the details of those objects' instances, credentials redacted.
// A release that waits for addons or jobs, or for a scale-up without
// capacity, would otherwise roll forever.
func (c *Controller) failLate(ctx context.Context, s *stepState) error {
	v := s.v
	first := s.pending[0]
	for _, p := range s.pending {
		if p.object != "" {
			first = p
			break
		}
	}
	var secrets []string
	for _, name := range slices.Sorted(maps.Keys(v.target.Addons)) {
		secrets = append(secrets, c.addonSecrets(ctx, v, name)...)
	}
	waits := map[string]bool{}
	var ev evidence
	for _, p := range s.pending {
		ev.Pending = append(ev.Pending, redact(p.text, secrets))
		waits[p.object] = true
	}
	for _, r := range v.rows {
		if obj := objectName(r.Instance.Kind, r.Instance.Object); waits[obj] && r.Instance.Detail != "" {
			ev.Pending = append(ev.Pending, redact(fmt.Sprintf("%s replica %d: %s", obj, r.Instance.Replica, r.Instance.Detail), secrets))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(v.attached)) {
		if a := v.attached[name]; waits[objectName(spec.KindService, name)] && a.Detail != "" {
			ev.Pending = append(ev.Pending, redact(fmt.Sprintf("service/%s: %s", name, a.Detail), secrets))
		}
	}
	ev.Diff = c.releaseDiff(ctx, v)
	reason := redact(fmt.Sprintf("release %d did not roll out within %s: %s", v.latest.Number,
		c.within(s).Round(time.Second), first.text), secrets)
	if err := c.failRelease(ctx, v, reason, first.object, model.ConditionRolloutFailed, ev); err != nil {
		return err
	}
	s.failed = true
	return nil
}

// logPending logs what a rolling release waits for, when that changed.
func (c *Controller) logPending(v *view, pending []string) {
	if !rolling(v) {
		return
	}
	text := strings.Join(pending, "; ")
	key := "pending/" + v.env.ID.String()
	c.mu.Lock()
	last, ok := c.lastPending[key]
	c.lastPending[key] = text
	c.mu.Unlock()
	if !ok || last != text {
		c.log.Debug("the release waits", "environment", v.env.ID, "release", v.latest.Number, "for", text)
	}
}

// startRollout moves R from pending to rolling.
func (c *Controller) startRollout(ctx context.Context, v *view) error {
	return c.fenced(ctx, func(tx *store.Store) error {
		r, err := tx.LockRelease(ctx, v.env.ID, v.latest.Number, store.ForUpdate)
		if err != nil {
			return err
		}
		if r.State != model.ReleasePending {
			v.latest = r
			return nil
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		after := r
		after.State, after.StartedAt = model.ReleaseRolling, now
		if v.latest, err = tx.UpdateRelease(ctx, r, after); err != nil {
			return err
		}
		c.log.Info("rollout begins", "environment", v.env.ID, "release", r.Number)
		return nil
	})
}

// setReason writes why a rolling release does not go on.
func (c *Controller) setReason(ctx context.Context, v *view, reason string) error {
	return c.fenced(ctx, func(tx *store.Store) error {
		r, err := tx.LockRelease(ctx, v.env.ID, v.latest.Number, store.ForUpdate)
		if err != nil || r.State != model.ReleaseRolling {
			return err
		}
		after := r
		after.Reason = reason
		_, err = tx.UpdateRelease(ctx, r, after)
		return err
	})
}

// realise marks R realised (§4.3 step 7): the environment's realised
// release, older realised and pending ones superseded, the open
// rollout_failed, job_failed and crash_loop incidents of older releases
// closed (every instance of R is ready), env-groups woken.
func (c *Controller) realise(ctx context.Context, v *view) error {
	n := v.latest.Number
	return c.fenced(ctx, func(tx *store.Store) error {
		env, err := tx.LockEnvironment(ctx, v.env.ID, store.ForUpdate)
		if err != nil {
			return err
		}
		if env.Release != n || env.Deleting {
			return nil
		}
		r, err := tx.LockRelease(ctx, env.ID, n, store.ForUpdate)
		if err != nil {
			return err
		}
		if r.State != model.ReleasePending && r.State != model.ReleaseRolling {
			return nil
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		after := r
		after.State, after.Reason, after.FinishedAt = model.ReleaseRealised, "", now
		if after.StartedAt.IsZero() {
			after.StartedAt = now
		}
		if _, err := tx.UpdateRelease(ctx, r, after); err != nil {
			return err
		}
		ea := env
		ea.Realised = n
		if _, err := tx.UpdateEnvironment(ctx, env, ea); err != nil {
			return err
		}
		if _, err := tx.SupersedeReleases(ctx, env.ID, n, model.ReleaseRealised, model.ReleasePending,
			model.ReleaseRolling); err != nil {
			return err
		}
		if _, err := tx.EnvCtlCloseIncidents(ctx, env.ID, n, model.ConditionRolloutFailed, model.ConditionJobFailed,
			model.ConditionCrashLoop); err != nil {
			return err
		}
		// The routes of the releases before R no longer count for the
		// ing- groups' ports: env-groups shrinks them.
		if err := tx.Notify(ctx, db.ChanLeader, HintGroups); err != nil {
			return err
		}
		c.log.Info("release realised", "environment", env.ID, "release", n)
		return nil
	})
}

// failRelease fails R with reason and opens its incident.
func (c *Controller) failRelease(ctx context.Context, v *view, reason, object, condition string, ev evidence) error {
	return c.fenced(ctx, func(tx *store.Store) error {
		r, err := tx.LockRelease(ctx, v.env.ID, v.latest.Number, store.ForUpdate)
		if err != nil {
			return err
		}
		if r.State != model.ReleasePending && r.State != model.ReleaseRolling {
			return nil
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		after := r
		after.State, after.Reason, after.FinishedAt = model.ReleaseFailed, reason, now
		if after.StartedAt.IsZero() {
			after.StartedAt = now
		}
		if _, err := tx.UpdateRelease(ctx, r, after); err != nil {
			return err
		}
		if _, err := openIncident(ctx, tx, v.env.ID, object, r.Number, condition, reason, ev); err != nil {
			return err
		}
		c.log.Info("release failed", "environment", v.env.ID, "release", r.Number, "reason", reason)
		return nil
	})
}

// releaseDiff is a failed release's diff against the realised one.
func (c *Controller) releaseDiff(ctx context.Context, v *view) []spec.DiffEntry {
	var prev *spec.Spec
	if v.env.Realised > 0 {
		prev, _ = c.releaseSpec(ctx, v.env, v.env.Realised)
	}
	return spec.Diff(prev, v.latestSpec)
}

// insert is an instance row to insert.
type insert struct {
	plan    *podPlan
	replica int
	release int
	pin     string
	// run is a job's run, inserted with the row.
	run *store.EnvJobRun
	// once: a run of a job that is refused while a run of the job runs
	// (RunJob), checked under the environment's row lock.
	once bool
}

// insertInstance inserts an instance row and the controller's row of it
// (and a job's run) in one fenced transaction, which holds the tenant FOR
// SHARE and refuses a deleting tenant or environment, then queues its
// Ensure. The image is pinned first (outside the transaction): every row
// gets the digest its image has now.
func (c *Controller) insertInstance(ctx context.Context, v *view, in insert) (podid.ID, error) {
	return c.insertFrom(ctx, v, in, true)
}

// insertFrom is insertInstance on the leader (fenced, then Ensure), or,
// for a call of the API on any instance, in a plain transaction that
// wakes env-reconcile, whose step brings the row up.
func (c *Controller) insertFrom(ctx context.Context, v *view, in insert, leaderSide bool) (podid.ID, error) {
	p := in.plan
	auth, err := p.template.ImageCredentials.Resolve(func(name string) (string, error) {
		return c.ResolveSecret(ctx, v.env.ID, name, 0)
	})
	if err != nil {
		return "", fmt.Errorf("the image credentials of %s: %w", p.image, err)
	}
	ref, err := c.d.Engine.ResolveImage(ctx, p.image, auth)
	if err != nil {
		return "", fmt.Errorf("pin image %s: %w", p.image, err)
	}
	// The row's ID is minted by the insert: a short ID (ADR 0019). A
	// template with the ingress group flag names the row's own group
	// ing-<id> (model.IngressGroupID) beside the plan's.
	var id podid.ID
	tmpl := p.template
	tmpl.Labels = maps.Clone(p.template.Labels)
	tmpl.SecurityGroups = slices.Clone(p.template.SecurityGroups)
	idle := p.idle
	write := func(tx *store.Store) error {
		t, err := tx.LockTenant(ctx, v.env.Owner, store.ForShare)
		if err != nil {
			return err
		}
		if t.Deleting() {
			return errTenantDeleting
		}
		lock := store.ForShare
		if in.once {
			lock = store.ForUpdate
		}
		e, err := tx.LockEnvironment(ctx, v.env.ID, lock)
		if err != nil {
			return err
		}
		if e.Deleting {
			return errEnvDeleting
		}
		if in.once && in.run != nil {
			if err := noRunOf(ctx, tx, v.env.ID, in.run.Job); err != nil {
				return err
			}
		}
		row, err := tx.CreateMintedSandbox(ctx, func(id podid.ID) store.NewSandbox {
			own := tmpl
			if own.IngressGroup {
				own.SecurityGroups = append(slices.Clone(tmpl.SecurityGroups), model.IngressGroupID(id))
			}
			return store.NewSandbox{ID: id, Owner: v.env.Owner, Image: p.image, VCPUs: p.vcpus, MemoryMiB: p.memoryMB,
				ImageRef: ref, EnvironmentID: v.env.ID, PodTemplate: &own, PinNode: in.pin, IdleAfterS: &idle}
		})
		if err != nil {
			return err
		}
		id = row.ID
		if _, err := tx.InsertEnvInstance(ctx, store.EnvInstance{SandboxID: id, EnvID: v.env.ID, Kind: p.kind,
			Object: p.name, Replica: in.replica, TemplateHash: p.hash, Release: in.release,
			Phase: model.PhaseStarting}); err != nil {
			return err
		}
		if in.run != nil {
			run := *in.run
			run.SandboxID = id
			if _, err := tx.InsertJobRun(ctx, run); err != nil {
				return err
			}
		}
		if !leaderSide {
			return tx.Notify(ctx, db.ChanLeader, HintReconcile)
		}
		return nil
	}
	if leaderSide {
		err = c.fenced(ctx, write)
	} else {
		err = c.st.InTx(ctx, write)
	}
	if err != nil {
		return "", err
	}
	c.log.Info("instance inserted", "environment", v.env.ID, "object", p.object(), "replica", in.replica,
		"sandbox", id, "release", in.release)
	if leaderSide {
		c.ensure(ctx, id)
	}
	return id, nil
}

// keep updates a kept instance in place: its release, and its row's
// idle_after_s.
func (c *Controller) keep(ctx context.Context, v *view, r store.EnvInstanceRow, idle int) {
	needRelease := r.Instance.Release != v.targetNum && v.targetNum > 0
	needIdle := r.Sandbox.IdleAfterS == nil || *r.Sandbox.IdleAfterS != idle
	if !needRelease && !needIdle {
		return
	}
	err := c.fenced(ctx, func(tx *store.Store) error {
		if needIdle {
			if _, err := tx.EnvCtlSetIdle(ctx, r.Sandbox.ID, idle); err != nil {
				return err
			}
		}
		if needRelease {
			inst, err := tx.LockEnvInstance(ctx, r.Sandbox.ID, store.ForUpdate)
			if err != nil {
				return err
			}
			after := inst
			after.Release = v.targetNum
			_, err = tx.UpdateEnvInstance(ctx, inst, after)
			return err
		}
		return nil
	})
	if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
		c.rl.warn("keep", "cannot update a kept instance", "sandbox", r.Sandbox.ID, "err", err)
	}
}

// retire marks instances retiring: routes stop sending to them and the
// env table drops them (env-names, woken); stepRetiring deletes them after
// grace.
func (c *Controller) retire(ctx context.Context, v *view, grace bool, rows ...store.EnvInstanceRow) error {
	if len(rows) == 0 {
		return nil
	}
	return c.fenced(ctx, func(tx *store.Store) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			inst, err := tx.LockEnvInstance(ctx, r.Sandbox.ID, store.ForUpdate)
			if errors.Is(err, store.ErrNoRow) {
				continue
			}
			if err != nil {
				return err
			}
			if inst.Phase == model.PhaseRetiring {
				continue
			}
			after := inst
			after.Phase, after.RetireAt = model.PhaseRetiring, now
			if grace {
				after.RetireAt = now.Add(c.t.retireGrace)
			}
			if _, err := tx.UpdateEnvInstance(ctx, inst, after); err != nil {
				return err
			}
			c.log.Info("instance retiring", "environment", v.env.ID, "object", objectName(inst.Kind, inst.Object),
				"replica", inst.Replica, "sandbox", inst.SandboxID)
		}
		return tx.Notify(ctx, db.ChanLeader, HintNames)
	})
}

// deleteInstance deletes an instance row through the engine (its pod goes
// by tombstone) and wakes env-groups and env-names when its address
// leaves the group and the table.
func (c *Controller) deleteInstance(ctx context.Context, sb store.Sandbox) error {
	if _, _, err := c.d.Engine.Delete(ctx, "", sb.ID); err != nil && !errors.Is(err, model.ErrNotFound) {
		return err
	}
	c.forget(sb.ID.String())
	if sb.Address != "" {
		c.notify(ctx, HintGroups)
		c.notify(ctx, HintNames)
	}
	return nil
}

// stepRetiring deletes the retiring instances whose time passed.
func (c *Controller) stepRetiring(ctx context.Context, s *stepState) {
	for _, r := range s.v.rows {
		if r.Instance.Phase != model.PhaseRetiring {
			continue
		}
		obj := objectName(r.Instance.Kind, r.Instance.Object)
		s.wait(obj, "%s replica %d is retiring", obj, r.Instance.Replica)
		if !r.Instance.RetireAt.IsZero() && s.v.now.Before(r.Instance.RetireAt) {
			continue
		}
		if err := c.deleteInstance(ctx, r.Sandbox); err != nil && ctx.Err() == nil {
			c.rl.warn("retire", "cannot delete a retired instance", "sandbox", r.Sandbox.ID, "err", err)
		}
	}
}

// lostNode is the node a lost row was lost with: lost_from when its node
// went down, else its node, which is up but no longer has the pod.
func lostNode(sb store.Sandbox) string {
	if sb.LostFrom != "" {
		return sb.LostFrom
	}
	return sb.Node
}

// heal brings a desired instance's row back up (§4.3): Ensure for a row
// that is stopped, failed or pending, and for one that is unknown, paused
// or hibernated when it does not idle (an idle one wakes on traffic; an
// unknown one may be paused in truth, and the engine's unknown sweep or
// lost grace finds out without waking it, so that the next pass acts on
// what it is). It reports whether the row is lost (the caller replaces
// it, or keeps an addon's).
func (c *Controller) heal(ctx context.Context, r store.EnvInstanceRow, idles bool) (lost bool) {
	sb := r.Sandbox
	if sb.Op != "" {
		return false
	}
	switch sb.ClientState {
	case model.StateLost:
		return true
	case model.StateStopped, model.StateFailed, model.StatePending:
		c.ensure(ctx, sb.ID)
	case model.StateUnknown, model.StatePaused, model.StateHibernated:
		if !idles {
			c.ensure(ctx, sb.ID)
		}
	}
	return false
}

// stepRemoved retires and deletes what T no longer has (§4.3 step 6):
// instances of objects not in T; an addon also gone from R loses its
// volume (deleting) and its credentials (an addon of a failed R that T
// lacks stays as it is, so that a failed first release deletes no data);
// attached services lose their process, then their row and name.
func (c *Controller) stepRemoved(ctx context.Context, s *stepState) {
	v := s.v
	var retire []store.EnvInstanceRow
	for _, r := range v.rows {
		inst := r.Instance
		if inst.Phase == model.PhaseRetiring {
			continue
		}
		switch inst.Kind {
		case spec.KindService:
			if svc, ok := v.target.Services[inst.Object]; ok && runtimeOf(svc) == spec.RuntimeImage {
				continue
			}
			retire = append(retire, r)
		case spec.KindJob:
			if _, ok := v.target.Jobs[inst.Object]; ok {
				continue
			}
			c.finishJob(ctx, v, r, nil, "the job was removed")
		case spec.KindAddon:
			if _, ok := v.target.Addons[inst.Object]; ok {
				continue
			}
			if _, ok := v.latestSpec.Addons[inst.Object]; ok {
				continue
			}
			s.wait(objectName(spec.KindAddon, inst.Object), "addon %s is being removed", inst.Object)
			if err := c.deleteInstance(ctx, r.Sandbox); err != nil && ctx.Err() == nil {
				c.rl.warn("remove", "cannot delete a removed addon's instance", "sandbox", r.Sandbox.ID, "err", err)
			}
		}
	}
	if err := c.retire(ctx, v, true, retire...); err != nil && ctx.Err() == nil {
		c.rl.warn("remove", "cannot retire removed instances", "environment", v.env.ID, "err", err)
	}
	for _, r := range retire {
		obj := objectName(r.Instance.Kind, r.Instance.Object)
		s.wait(obj, "%s is being removed", obj)
	}
	// The removed addons' volumes and credentials.
	for _, object := range slices.Sorted(maps.Keys(v.volumes)) {
		vol := v.volumes[object]
		name := object[len(spec.KindAddon)+1:]
		_, inT := v.target.Addons[name]
		_, inR := v.latestSpec.Addons[name]
		if inT || inR {
			continue
		}
		if vol.Deleting {
			// Gone from its node once no pod holds it: env-volumes would
			// see to it within its interval.
			if len(v.rowsOf(spec.KindAddon, name)) == 0 {
				if _, err := c.deleteVolume(ctx, vol); err != nil && ctx.Err() == nil {
					c.rl.warn("remove", "cannot delete a removed addon's volume", "environment", v.env.ID, "addon", name, "err", err)
				}
			}
			continue
		}
		err := c.fenced(ctx, func(tx *store.Store) error {
			cur, err := tx.LockEnvVolume(ctx, v.env.ID, object, store.ForUpdate)
			if err != nil {
				return err
			}
			after := cur
			after.Deleting = true
			if _, err := tx.UpdateEnvVolume(ctx, cur, after); err != nil {
				return err
			}
			_, err = tx.PurgeSecrets(ctx, v.env.ID, "addon."+name+".")
			return err
		})
		if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNoRow) {
			c.rl.warn("remove", "cannot mark a removed addon's volume deleting", "environment", v.env.ID, "addon", name, "err", err)
		}
	}
	// The removed addons' credentials, with a volume or without.
	c.purgeCredentials(ctx, v)
	for _, name := range slices.Sorted(maps.Keys(v.attached)) {
		if svc, ok := v.target.Services[name]; ok && runtimeOf(svc) == spec.RuntimeAttached {
			continue
		}
		s.wait(objectName(spec.KindService, name), "attached service %s is being removed", name)
		c.removeAttached(ctx, v, v.attached[name])
	}
	c.closeRemovedIncidents(ctx, v)
}

// closeRemovedIncidents closes the open crash_loop, volume_unavailable and
// addon_unhealthy incidents of objects that neither T nor R has: nothing
// closes them otherwise (no_capacity closes in stepStarved, rollout_failed
// and job_failed on a realisation).
func (c *Controller) closeRemovedIncidents(ctx context.Context, v *view) {
	done := map[[2]string]bool{}
	for _, i := range v.open {
		switch i.Condition {
		case model.ConditionCrashLoop, model.ConditionVolumeUnavailable, model.ConditionAddonUnhealthy:
		default:
			continue
		}
		k := [2]string{i.Object, i.Condition}
		if done[k] || hasObject(v.target, i.Object) || hasObject(v.latestSpec, i.Object) {
			continue
		}
		done[k] = true
		c.closeIncidents(ctx, v, i.Object, i.Condition)
	}
}

// hasObject reports whether a spec has an object ("<kind>/<name>").
func hasObject(s *spec.Spec, object string) bool {
	kind, name, _ := strings.Cut(object, "/")
	switch kind {
	case spec.KindService:
		_, ok := s.Services[name]
		return ok
	case spec.KindAddon:
		_, ok := s.Addons[name]
		return ok
	case spec.KindJob:
		_, ok := s.Jobs[name]
		return ok
	}
	return false
}

// purgeCredentials deletes the system secrets of addons that neither T
// nor R has.
func (c *Controller) purgeCredentials(ctx context.Context, v *view) {
	secrets, err := c.st.ListSecrets(ctx, v.env.ID, true)
	if err != nil {
		return
	}
	gone := map[string]bool{}
	for _, sec := range secrets {
		rest, ok := strings.CutPrefix(sec.Name, "addon.")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, ".")
		_, inT := v.target.Addons[name]
		_, inR := v.latestSpec.Addons[name]
		if !inT && !inR {
			gone[name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(gone)) {
		err := c.fenced(ctx, func(tx *store.Store) error {
			_, err := tx.PurgeSecrets(ctx, v.env.ID, "addon."+name+".")
			return err
		})
		if err != nil && ctx.Err() == nil {
			c.rl.warn("remove", "cannot delete a removed addon's credentials", "environment", v.env.ID, "addon", name, "err", err)
		}
	}
}

// stepStarved opens no_capacity for instances pending without capacity
// for long, and closes it for objects that have none.
func (c *Controller) stepStarved(ctx context.Context, s *stepState) {
	v := s.v
	starved, err := c.st.EnvCtlStarved(ctx, v.env.ID, c.t.noCapacityAfter)
	if err != nil {
		return
	}
	hungry := map[string]bool{}
	for _, r := range v.rows {
		desc, ok := starved[r.Sandbox.ID]
		if !ok {
			continue
		}
		obj := objectName(r.Instance.Kind, r.Instance.Object)
		hungry[obj] = true
		if v.isOpen(obj, model.ConditionNoCapacity) {
			continue
		}
		c.incident(ctx, v.env.ID, obj, r.Instance.Release, model.ConditionNoCapacity,
			fmt.Sprintf("%s replica %d found no capacity for %s", obj, r.Instance.Replica, c.t.noCapacityAfter),
			evidence{Placement: desc})
	}
	for _, i := range v.open {
		if i.Condition == model.ConditionNoCapacity && !hungry[i.Object] {
			c.closeIncidents(ctx, v, i.Object, model.ConditionNoCapacity)
		}
	}
}

// nodeReady reports whether a node is ready (joined, reporting, not
// draining).
func (c *Controller) nodeReady(ctx context.Context, node string) bool {
	if node == "" {
		return false
	}
	n, err := c.st.GetNode(ctx, node)
	return err == nil && n.Status == model.NodeReady && !n.Draining
}
