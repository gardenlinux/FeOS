// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package quota is a tenant's quotas (ADR 0031, "Quotas per tenant"):
// limits on what it asks for, checked when it asks. A tenant's use is the
// number of its sandboxes, of its environments and of its tenants' jobs
// running, and the sums of vCPUs and of memory over its sandboxes, the
// replicas of its environments' services (runtime image, by their
// current deployments), its addons' plans and its running jobs. A create
// that passes a limit is 422 quota_exceeded; a deployment that would
// bring a sum past one has an error finding.
//
// Concurrent creates must not pass a limit together: Lock takes the
// tenant's row FOR NO KEY UPDATE when the tenant has limits, so that the
// counting and the insert of one create run before the next one counts.
package quota

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env/addons"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The resources, as answers and limits name them.
const (
	Sandboxes    = "sandboxes"
	Environments = "environments"
	RunningJobs  = "runningJobs"
	VCPUs        = "vcpus"
	MemoryMiB    = "memoryMib"
)

// Use is a tenant's use, or what a request asks for.
type Use struct {
	Sandboxes    int64 `json:"sandboxes"`
	Environments int64 `json:"environments"`
	RunningJobs  int64 `json:"runningJobs"`
	VCPUs        int64 `json:"vcpus"`
	MemoryMiB    int64 `json:"memoryMib"`
}

// Lock reads a tenant in tx and locks its row: FOR NO KEY UPDATE when it
// has limits, which serialises the creates that count against them, else
// FOR SHARE as before. store.ErrNoRow when there is none.
func Lock(ctx context.Context, tx *store.Store, tenant string) (store.Tenant, error) {
	t, err := tx.GetTenant(ctx, tenant)
	if err != nil {
		return store.Tenant{}, err
	}
	lock := store.ForShare
	if t.Limits.Any() {
		lock = store.ForNoKeyUpdate
	}
	return tx.LockTenant(ctx, tenant, lock)
}

// Of is a tenant's use; exclude (uuid.Nil: none) is an environment whose
// current deployment is left out, the one a plan replaces.
func Of(ctx context.Context, st *store.Store, tenant string, exclude uuid.UUID) (Use, error) {
	rows, err := st.TenantUse(ctx, tenant, exclude)
	if err != nil {
		return Use{}, fmt.Errorf("the use of tenant %s: %w", tenant, err)
	}
	u := Use{Sandboxes: rows.Sandboxes, Environments: rows.Environments, RunningJobs: rows.RunningJobs,
		VCPUs: rows.SandboxVCPUs + rows.JobVCPUs, MemoryMiB: rows.SandboxMemoryMiB + rows.JobMemoryMiB}
	for _, raw := range rows.Specs {
		var sp spec.Spec
		if err := json.Unmarshal(raw, &sp); err != nil {
			continue // a stored spec that does not read asks for nothing
		}
		v, m := SpecUse(&sp)
		u.VCPUs, u.MemoryMiB = u.VCPUs+v, u.MemoryMiB+m
	}
	return u, nil
}

// SpecUse is what a deployment spec asks for: its services' replicas
// (runtime image; an attached service runs in a sandbox, which counts as
// one) by their resources, and its addons by their plans. Defaults apply
// where a spec leaves a value out. nil asks for nothing.
func SpecUse(sp *spec.Spec) (vcpus, memoryMiB int64) {
	if sp == nil {
		return 0, 0
	}
	for _, svc := range sp.Services {
		if svc.Runtime == spec.RuntimeAttached || (svc.Runtime == "" && svc.Image == "") {
			continue
		}
		replicas := int64(spec.DefaultReplicas)
		if svc.Replicas != nil {
			replicas = int64(*svc.Replicas)
		}
		v, m := int64(spec.DefaultVCPUs), int64(spec.DefaultMemoryMib)
		if r := svc.Resources; r != nil {
			if r.VCPUs != 0 {
				v = int64(r.VCPUs)
			}
			if r.MemoryMib != 0 {
				m = int64(r.MemoryMib)
			}
		}
		vcpus, memoryMiB = vcpus+replicas*v, memoryMiB+replicas*m
	}
	for _, a := range sp.Addons {
		b, ok := addons.Get(a.Type)
		if !ok {
			continue
		}
		name := a.Plan
		if name == "" {
			name = b.DefaultPlan()
		}
		if p, ok := b.Plans()[name]; ok {
			vcpus, memoryMiB = vcpus+int64(p.VCPUs), memoryMiB+int64(p.MemoryMib)
		}
	}
	return vcpus, memoryMiB
}

// Check refuses what ask asks for when use and it would pass one of the
// limits: 422 quota_exceeded with the first such resource. A resource
// ask does not ask for passes, however far its use is past the limit.
func Check(l store.TenantLimits, use, ask Use) error {
	for _, c := range []struct {
		name     string
		limit    *int64
		use, ask int64
	}{
		{Sandboxes, intLimit(l.Sandboxes), use.Sandboxes, ask.Sandboxes},
		{Environments, intLimit(l.Environments), use.Environments, ask.Environments},
		{RunningJobs, intLimit(l.RunningJobs), use.RunningJobs, ask.RunningJobs},
		{VCPUs, intLimit(l.VCPUs), use.VCPUs, ask.VCPUs},
		{MemoryMiB, l.MemoryMiB, use.MemoryMiB, ask.MemoryMiB},
	} {
		if c.limit != nil && c.ask > 0 && c.use+c.ask > *c.limit {
			return apierr.QuotaExceeded(c.name, *c.limit, c.use, c.ask)
		}
	}
	return nil
}

// LockAndCheck locks the tenant as Lock does and, when it has limits,
// checks ask against its use (exclude as Of has it). It returns the
// tenant.
func LockAndCheck(ctx context.Context, tx *store.Store, tenant string, ask Use) (store.Tenant, error) {
	t, err := Lock(ctx, tx, tenant)
	if err != nil || !t.Limits.Any() || t.Deleting() {
		return t, err
	}
	use, err := Of(ctx, tx, tenant, uuid.Nil)
	if err != nil {
		return t, err
	}
	return t, Check(t.Limits, use, ask)
}

// SpecFindings are the error findings of a deployment of next in place
// of current (nil: none) when the tenant's sums would pass its limits:
// others is the tenant's use without the environment's current
// deployment. A deployment that asks for no more than the current one
// passes, whatever the limits.
func SpecFindings(l store.TenantLimits, others Use, current, next *spec.Spec) []spec.Finding {
	cv, cm := SpecUse(current)
	nv, nm := SpecUse(next)
	var fs []spec.Finding
	for _, c := range []struct {
		what        string
		limit       *int64
		others, cur int64
		next        int64
	}{
		{"vCPUs", intLimit(l.VCPUs), others.VCPUs, cv, nv},
		{"MiB of memory", l.MemoryMiB, others.MemoryMiB, cm, nm},
	} {
		total := c.others + c.next
		if c.limit == nil || total <= *c.limit || c.next <= c.cur {
			continue
		}
		fs = append(fs, spec.Finding{Severity: spec.SeverityError, Path: "",
			Problem: fmt.Sprintf("the deployment asks for %d %s, which brings the tenant to %d of its limit %d "+
				"(%d elsewhere)", c.next, c.what, total, *c.limit, c.others),
			Remedy: "use fewer replicas, smaller resources or plans, or ask an operator for a higher limit"})
	}
	return fs
}

func intLimit(p *int) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}
