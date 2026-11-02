// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The controller reads a release's spec as the store keeps it (normalised
// by the service's apply), but never relies on the normalisation: every
// default of the contract's §3 is applied here where a field is empty.

// specKey names a release: of the environment of its ID created when
// (an environment's ID is free again once it is deleted, and the client
// chooses it, so the ID alone does not name the environment).
type specKey struct {
	env     uuid.UUID
	created time.Time
	number  int
}

// specCache keeps parsed specs: a release's spec never changes.
type specCache struct {
	mu sync.Mutex
	m  map[specKey]*spec.Spec
}

func newSpecCache() *specCache { return &specCache{m: map[specKey]*spec.Spec{}} }

// maxCachedSpecs bounds the cache; past it, it starts anew.
const maxCachedSpecs = 1024

// releaseSpec reads (and caches) the spec of release number of the
// environment.
func (c *Controller) releaseSpec(ctx context.Context, env store.Environment, number int) (*spec.Spec, error) {
	k := specKey{env: env.ID, created: env.CreatedAt, number: number}
	c.specs.mu.Lock()
	s, ok := c.specs.m[k]
	c.specs.mu.Unlock()
	if ok {
		return s, nil
	}
	r, err := c.st.GetRelease(ctx, env.ID, number)
	if err != nil {
		return nil, fmt.Errorf("release %d: %w", number, err)
	}
	s = &spec.Spec{}
	if err := json.Unmarshal(r.Spec, s); err != nil {
		return nil, fmt.Errorf("release %d of environment %s: its spec: %w", r.Number, r.EnvID, err)
	}
	c.specs.mu.Lock()
	defer c.specs.mu.Unlock()
	if len(c.specs.m) >= maxCachedSpecs {
		c.specs.m = map[specKey]*spec.Spec{}
	}
	c.specs.m[k] = s
	return s, nil
}

// runtimeOf is a service's runtime: as written, else image when it has an
// image, else attached.
func runtimeOf(s spec.Service) string {
	switch {
	case s.Runtime != "":
		return s.Runtime
	case s.Image != "":
		return spec.RuntimeImage
	}
	return spec.RuntimeAttached
}

// replicasOf is an image service's replicas: 1 when unset.
func replicasOf(s spec.Service) int {
	if s.Replicas == nil {
		return spec.DefaultReplicas
	}
	return max(0, *s.Replicas)
}

// resourcesOf is a pod's size: 1 vCPU and 512 MiB where unset.
func resourcesOf(r *spec.Resources) (vcpus, memoryMiB int) {
	vcpus, memoryMiB = spec.DefaultVCPUs, spec.DefaultMemoryMib
	if r != nil {
		if r.VCPUs > 0 {
			vcpus = r.VCPUs
		}
		if r.MemoryMib > 0 {
			memoryMiB = r.MemoryMib
		}
	}
	return vcpus, memoryMiB
}

// duration parses a Go duration, def when empty or invalid.
func duration(s, def string) time.Duration {
	if s == "" {
		s = def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		d, _ = time.ParseDuration(def)
	}
	return d
}

// idleOf is an image service's idle_after_s: the seconds of its idle, 0
// (never) when it has none.
func idleOf(s spec.Service) int {
	if s.Idle == "" {
		return 0
	}
	d, err := time.ParseDuration(s.Idle)
	if err != nil || d <= 0 {
		return 0
	}
	return int(d / time.Second)
}

// attachedIdleOf is an attached service's idle_after_s: the seconds of
// its idle, 0 for never, nil when it has none (no claim: its sandbox's
// own interval applies).
func attachedIdleOf(s spec.Service) *int {
	if s.Idle == spec.IdleNever {
		n := 0
		return &n
	}
	if s.Idle == "" {
		return nil
	}
	d, err := time.ParseDuration(s.Idle)
	if err != nil || d <= 0 {
		return nil
	}
	n := int(d / time.Second)
	return &n
}

// rolloutOf is an image service's rollout budget: 10m when unset.
func rolloutOf(s spec.Service) time.Duration {
	if s.Rollout == nil {
		return duration("", spec.DefaultRolloutTimeout)
	}
	return duration(s.Rollout.Timeout, spec.DefaultRolloutTimeout)
}

// jobOn is a job's trigger: release when unset.
func jobOn(j spec.Job) string {
	if j.On == "" {
		return spec.JobOnRelease
	}
	return j.On
}

// jobTimeout is a job attempt's bound: 10m when unset, at most 2h.
func jobTimeout(j spec.Job) time.Duration {
	return min(duration(j.Timeout, spec.DefaultJobTimeout), duration(spec.MaxJobTimeout, spec.MaxJobTimeout))
}

// portOf resolves a port reference among a service's ports: by name, or
// a number it lists; 0 when it names none.
func portOf(s spec.Service, ref spec.PortRef) int {
	for _, p := range s.Ports {
		if (ref.Name != "" && p.Name == ref.Name) || (ref.Name == "" && p.Port == ref.Number) {
			return p.Port
		}
	}
	if ref.Name == "" && ref.Number > 0 {
		return ref.Number
	}
	return 0
}

// check is a readiness check as the probes run it.
type check struct {
	// kind is "http", "tcp", "exec", or "" (none: steady for a while).
	kind      string
	port      int
	path      string
	argv      []string
	delay     time.Duration
	period    time.Duration
	threshold int
}

// checkOf is a service's readiness check with its timing defaults.
func checkOf(s spec.Service) check {
	c := check{period: duration("", spec.DefaultPeriod), threshold: spec.DefaultFailureThreshold}
	r := s.Readiness
	if r == nil {
		return c
	}
	c.delay = duration(r.InitialDelay, spec.DefaultInitialDelay)
	c.period = duration(r.Period, spec.DefaultPeriod)
	if r.FailureThreshold > 0 {
		c.threshold = r.FailureThreshold
	}
	switch {
	case r.HTTP != nil:
		c.kind, c.port, c.path = "http", portOf(s, r.HTTP.Port), r.HTTP.Path
		if c.path == "" {
			c.path = "/"
		}
	case r.TCP != nil:
		c.kind, c.port = "tcp", portOf(s, r.TCP.Port)
	case r.Exec != nil:
		c.kind, c.argv = "exec", slices.Clone(r.Exec.Command)
	}
	return c
}

// secretVersion is the version of a tenant secret a spec pins; 0 when it
// pins none (the caller takes the latest).
func secretVersion(s *spec.Spec, name string) int {
	if s == nil {
		return 0
	}
	return s.Secrets[name].Version
}

// view is what one pass knows of an environment: its row, its latest
// release R and its target T, its instances, volumes and attached
// services, and the database's time.
type view struct {
	env store.Environment
	// latest is R; latestSpec its spec.
	latest     store.EnvRelease
	latestSpec *spec.Spec
	// target is T, targetNum its release (0: none, an empty spec);
	// targetIsLatest: T is R.
	target         *spec.Spec
	targetNum      int
	targetIsLatest bool
	rows           []store.EnvInstanceRow
	volumes        map[string]store.EnvVolume
	attached       map[string]store.EnvAttached
	// secrets are the latest versions of the tenant's secrets, for a spec
	// that pins none.
	secrets map[string]int
	// open are the environment's open incidents.
	open []store.EnvIncident
	now  time.Time
}

// isOpen reports whether an incident of condition is open for object.
func (v *view) isOpen(object, condition string) bool {
	for _, i := range v.open {
		if i.Object == object && i.Condition == condition {
			return true
		}
	}
	return false
}

// load reads an environment's view; nil when it has no release or is
// being deleted.
func (c *Controller) load(ctx context.Context, envID uuid.UUID) (*view, error) {
	env, err := c.st.GetEnvironment(ctx, envID)
	if err != nil {
		return nil, err
	}
	if env.Deleting || env.Release == 0 {
		return nil, nil
	}
	v := &view{env: env}
	// The latest release without its spec (up to 1 MiB, read every
	// second by the probes): the spec comes from the cache.
	latest, err := c.st.ListReleases(ctx, envID, 1)
	if err != nil {
		return nil, err
	}
	if len(latest) == 1 && latest[0].Number == env.Release {
		v.latest = latest[0]
	} else if v.latest, err = c.st.GetRelease(ctx, envID, env.Release); err != nil {
		return nil, fmt.Errorf("release %d: %w", env.Release, err)
	}
	if v.latestSpec, err = c.releaseSpec(ctx, env, env.Release); err != nil {
		return nil, err
	}
	switch {
	case v.latest.State != model.ReleaseFailed:
		v.target, v.targetNum, v.targetIsLatest = v.latestSpec, env.Release, true
	case env.Deployed > 0:
		if v.target, err = c.releaseSpec(ctx, env, env.Deployed); err != nil {
			return nil, err
		}
		v.targetNum = env.Deployed
	default:
		v.target = &spec.Spec{}
	}
	if v.rows, err = c.st.ListEnvInstanceRows(ctx, envID); err != nil {
		return nil, err
	}
	vols, err := c.st.ListEnvVolumes(ctx, envID)
	if err != nil {
		return nil, err
	}
	v.volumes = map[string]store.EnvVolume{}
	for _, vol := range vols {
		v.volumes[vol.Object] = vol
	}
	att, err := c.st.ListAttached(ctx, envID)
	if err != nil {
		return nil, err
	}
	v.attached = map[string]store.EnvAttached{}
	for _, a := range att {
		v.attached[a.Service] = a
	}
	secrets, err := c.st.ListSecrets(ctx, envID, false)
	if err != nil {
		return nil, err
	}
	v.secrets = map[string]int{}
	for _, s := range secrets {
		v.secrets[s.Name] = s.Version
	}
	if v.open, err = c.st.ListIncidents(ctx, envID, true); err != nil {
		return nil, err
	}
	if v.now, err = c.st.Now(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

// rowsOf returns the instance rows of an object, by replica and creation.
func (v *view) rowsOf(kind, name string) []store.EnvInstanceRow {
	var out []store.EnvInstanceRow
	for _, r := range v.rows {
		if r.Instance.Kind == kind && r.Instance.Object == name {
			out = append(out, r)
		}
	}
	return out
}

// addonRow is the current instance row of an addon: the one not stopping,
// preferring the newest.
func (v *view) addonRow(name string) (store.EnvInstanceRow, bool) {
	var out store.EnvInstanceRow
	found := false
	for _, r := range v.rowsOf(spec.KindAddon, name) {
		if r.Instance.Phase == model.PhaseStopping {
			continue
		}
		if !found || r.Instance.CreatedAt.After(out.Instance.CreatedAt) {
			out, found = r, true
		}
	}
	return out, found
}

// volumeObject is the env_volumes object of an addon.
func volumeObject(addon string) string { return spec.KindAddon + "/" + addon }

// itoa is strconv.Itoa.
func itoa(n int) string { return strconv.Itoa(n) }
