// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// stepServices brings the image services of T to their replicas (§4.3
// step 4).
func (c *Controller) stepServices(ctx context.Context, s *stepState) {
	for _, name := range slices.Sorted(maps.Keys(s.v.target.Services)) {
		svc := s.v.target.Services[name]
		if runtimeOf(svc) != spec.RuntimeImage {
			continue
		}
		c.stepService(ctx, s, name, svc)
		if s.failed {
			return
		}
	}
}

// byReplica orders rows by replica, then age.
func byReplica(a, b store.EnvInstanceRow) int {
	if n := cmp.Compare(a.Instance.Replica, b.Instance.Replica); n != 0 {
		return n
	}
	return a.Instance.CreatedAt.Compare(b.Instance.CreatedAt)
}

// stepService keeps an image service at T: rows of T's template are kept
// (deployment and idle updated in place) and healed; rows of another
// template are replaced one at a time, surge 1 (a new one first, ready,
// then an old one stops); missing replicas are added, surplus ones
// stop, the highest first. While R rolls the service waits for the
// addons and the deploy jobs, and its rollout budget counts from R's
// start, moved on by the time the deploy jobs waited for runs that were
// not R's (stepState.start): past it, or when a new instance exited 3
// times, R fails (rollout_failed) and R's new instances stop.
func (c *Controller) stepService(ctx context.Context, s *stepState, name string, svc spec.Service) {
	v := s.v
	var cur, old, stopping []store.EnvInstanceRow
	p := c.planService(v, name, svc)
	for _, r := range v.rowsOf(spec.KindService, name) {
		switch {
		case r.Instance.Phase == model.PhaseStopping:
			stopping = append(stopping, r)
		case p.ok() && r.Instance.TemplateHash == p.hash:
			cur = append(cur, r)
		default:
			old = append(old, r)
		}
	}
	slices.SortFunc(cur, byReplica)
	slices.SortFunc(old, byReplica)
	idles := idleOf(svc) > 0
	// The old keep serving while the new come.
	for _, r := range old {
		c.healInstance(ctx, v, r, idles)
	}
	if !p.ok() {
		s.wait(objectName(spec.KindService, name), "service %s: %s", name, p.why())
		return
	}
	if deploying(v) && (!s.addonsReady || !s.jobsDone) {
		for _, r := range cur {
			c.healInstance(ctx, v, r, idles)
		}
		s.wait(objectName(spec.KindService, name), "service %s waits for the addons and the deploy jobs", name)
		return
	}
	var lost []store.EnvInstanceRow
	for _, r := range cur {
		c.keep(ctx, v, r, p.idle)
		if c.healInstance(ctx, v, r, idles) {
			lost = append(lost, r)
		}
	}
	if len(lost) > 0 {
		// A lost replica is replaced: a new row takes its replica.
		for _, r := range lost {
			if err := c.deleteInstance(ctx, r.Sandbox); err != nil && ctx.Err() == nil {
				c.rl.warn("replace", "cannot delete a lost instance", "sandbox", r.Sandbox.ID, "err", err)
			}
		}
		cur = slices.DeleteFunc(cur, func(r store.EnvInstanceRow) bool {
			return slices.ContainsFunc(lost, func(l store.EnvInstanceRow) bool { return l.Sandbox.ID == r.Sandbox.ID })
		})
	}
	desired := replicasOf(svc)
	if deploying(v) && c.rolloutFailed(ctx, s, name, svc, p, cur, old) {
		return
	}
	ready := 0
	for _, r := range cur {
		if live(r, idles) {
			ready++
		}
	}
	switch {
	case len(old) == 0 && len(cur) < desired:
		for _, replica := range freeReplicas(cur, desired-len(cur)) {
			if _, err := c.insertInstance(ctx, v, insert{plan: p, replica: replica, deployment: v.targetNum}); err != nil {
				s.wait(objectName(spec.KindService, name), "service %s: %v", name, err)
				return
			}
		}
	case len(old) == 0 && len(cur) > desired:
		if err := c.stopInstances(ctx, v, true, cur[desired:]...); err != nil {
			s.wait(objectName(spec.KindService, name), "service %s: %v", name, err)
		}
	case len(old) > 0 && ready < len(cur):
		// A new one is coming up: wait for it.
	case len(old) > 0 && len(cur) < desired && len(cur)+len(old) <= desired:
		replica := freeReplicas(cur, 1)
		if _, err := c.insertInstance(ctx, v, insert{plan: p, replica: replica[0], deployment: v.targetNum}); err != nil {
			s.wait(objectName(spec.KindService, name), "service %s: %v", name, err)
			return
		}
	case len(old) > 0:
		// Every new one is ready: an old one goes, the highest first.
		if err := c.stopInstances(ctx, v, true, old[len(old)-1]); err != nil {
			s.wait(objectName(spec.KindService, name), "service %s: %v", name, err)
		}
	}
	if len(old) > 0 || len(cur) != desired || ready != len(cur) || len(stopping) > 0 {
		s.wait(objectName(spec.KindService, name), "service %s: %d of %d ready, %d old, %d stopping", name, ready, desired,
			len(old), len(stopping))
	}
}

// freeReplicas are the n lowest replica numbers that no row of rows has.
func freeReplicas(rows []store.EnvInstanceRow, n int) []int {
	used := map[int]bool{}
	for _, r := range rows {
		used[r.Instance.Replica] = true
	}
	var out []int
	for i := 0; len(out) < n; i++ {
		if !used[i] {
			out = append(out, i)
		}
	}
	return out
}

// rolloutFailed fails R when the service's rollout ran past its budget, or
// when one of its new instances exited 3 times (a start that failed counts
// as an exit); R's new instances of the service stop, the old keep
// serving. It reports whether R failed.
func (c *Controller) rolloutFailed(ctx context.Context, s *stepState, name string, svc spec.Service, p *podPlan,
	cur, old []store.EnvInstanceRow) bool {
	v := s.v
	// The service rolls when it has old instances, is new to R, or scales
	// to another count (a scale-up adds under the same budget).
	deployed, inDeployed := c.deployedService(ctx, v, name)
	rollingService := len(old) > 0 || !inDeployed || replicasOf(deployed) != replicasOf(svc)
	if !rollingService {
		return false
	}
	// R's new instances: those inserted since it began (a scale-up keeps
	// the others, with whatever exits they had before).
	var fresh []store.EnvInstanceRow
	for _, r := range cur {
		if !r.Instance.CreatedAt.Before(v.latest.StartedAt) {
			fresh = append(fresh, r)
		}
	}
	var reason string
	var culprit *store.EnvInstanceRow
	for i, r := range fresh {
		if len(r.Instance.Exits) >= c.t.crashExits {
			reason = fmt.Sprintf("service %s: a new instance (replica %d) %s, %s", name, r.Instance.Replica,
				timesText(r.Instance.Exits), lastText(r.Instance.Exits[len(r.Instance.Exits)-1]))
			culprit = &fresh[i]
			break
		}
	}
	budget := rolloutOf(svc)
	if start, ok := s.start(); reason == "" && ok && v.now.After(start.Add(budget)) {
		ready := 0
		for _, r := range cur {
			if live(r, idleOf(svc) > 0) {
				ready++
			}
		}
		if len(old) == 0 && ready == len(cur) && len(cur) == replicasOf(svc) {
			return false
		}
		reason = fmt.Sprintf("service %s did not roll out within %s", name, budget)
		if len(fresh) > 0 {
			culprit = &fresh[0]
		}
	}
	if reason == "" {
		return false
	}
	ev := evidence{Diff: c.deploymentDiff(ctx, v)}
	if culprit != nil {
		ev.Exits = culprit.Instance.Exits
		if steady(culprit.Sandbox) && c.connected(culprit.Sandbox.Node) {
			ev.LogTail = c.storedTail(ctx, v, culprit.Sandbox)
		}
	}
	if err := c.failDeployment(ctx, v, reason, objectName(spec.KindService, name), model.ConditionRolloutFailed, ev); err != nil {
		s.wait(objectName(spec.KindService, name), "service %s: %v", name, err)
		return false
	}
	s.failed = true
	if err := c.stopInstances(ctx, v, true, fresh...); err != nil && ctx.Err() == nil {
		c.rl.warn("rollout", "cannot stop a failed rollout's instances", "environment", v.env.ID, "service", name, "err", err)
	}
	return true
}

// deployedService is the image service as the deployed deployment has it,
// and whether it has it (true when the deployed deployment cannot be read:
// the budget is not guessed).
func (c *Controller) deployedService(ctx context.Context, v *view, name string) (spec.Service, bool) {
	if v.env.Deployed == 0 {
		return spec.Service{}, false
	}
	sp, err := c.deploymentSpec(ctx, v.env, v.env.Deployed)
	if err != nil {
		return v.target.Services[name], true
	}
	svc, ok := sp.Services[name]
	return svc, ok && runtimeOf(svc) == spec.RuntimeImage
}
