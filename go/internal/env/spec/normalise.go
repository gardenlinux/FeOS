// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"time"
)

// Pins are what Normalise pins a spec to, resolved by the caller outside
// any transaction.
type Pins struct {
	// Images maps an image as written to its digest reference
	// ("<registry>/<repo>@sha256:…"); an image missing here is kept as
	// written (a plan that could not resolve it warned).
	Images map[string]string
	// Secrets maps a secret's name to the version a reference without a
	// version is pinned to: its latest.
	Secrets map[string]int
	// Plans maps an addon type to its blueprint's default plan
	// (addons.Blueprint.DefaultPlan), which an addon without a plan gets:
	// a later default never changes a running addon.
	Plans map[string]string
}

// Normalise returns the spec's normalised form, the one ferund stores,
// hashes and diffs: defaults filled (replicas, resources, readiness
// timing, rollout and job timeouts, job on, an addon's plan), Runtime
// explicit on every service, durations as Go's time.Duration.String
// writes them ("15m0s"), Image pinned (and ImageAsWritten kept) for every
// image found in p.Images, a digest reference kept as written, every
// referenced secret in Secrets with a version (p.Secrets for those
// without), unreferenced Secrets entries dropped (an info finding each).
// Fields that do not apply are cleared (an attached service has no
// replicas), and the kinds that are not available yet (volumes, mounts)
// are dropped. s should have passed Validate without errors; a spec that
// did not is normalised as far as it goes (for a plan's diff), never
// with a panic.
func Normalise(s Spec, p Pins) (Spec, []Finding) {
	var out Spec
	var fs []Finding
	if len(s.Services) > 0 {
		out.Services = make(map[string]Service, len(s.Services))
		for name, svc := range s.Services {
			out.Services[name] = normService(svc, p)
		}
	}
	if len(s.Jobs) > 0 {
		out.Jobs = make(map[string]Job, len(s.Jobs))
		for name, j := range s.Jobs {
			out.Jobs[name] = normJob(j, p)
		}
	}
	if len(s.Addons) > 0 {
		out.Addons = make(map[string]Addon, len(s.Addons))
		for name, a := range s.Addons {
			if a.Plan == "" {
				a.Plan = p.Plans[a.Type]
			}
			out.Addons[name] = a
		}
	}
	if len(s.Routes) > 0 {
		out.Routes = maps.Clone(s.Routes)
	}
	referenced := Referenced(s)
	for _, name := range slices.Sorted(maps.Keys(s.Secrets)) {
		if !slices.Contains(referenced, name) {
			fs = append(fs, Finding{Severity: SeverityInfo, Path: "secrets." + name,
				Problem: "secret " + name + " is not referenced: dropped", Remedy: ""})
		}
	}
	for _, name := range referenced {
		v := s.Secrets[name].Version
		if v <= 0 {
			v = p.Secrets[name]
		}
		if v <= 0 {
			// Validate refused the reference; nothing to pin.
			continue
		}
		if out.Secrets == nil {
			out.Secrets = map[string]SecretRef{}
		}
		out.Secrets[name] = SecretRef{Version: v}
	}
	return out, SortFindings(fs)
}

// normDuration writes a duration as Go does, def when empty; one that is
// none stays as written (Validate refused it).
func normDuration(s, def string) string {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return s
	}
	return d.String()
}

// pin returns the image pinned and the image as written.
func pin(image, asWritten string, p Pins) (string, string) {
	if image == "" || IsDigestRef(image) {
		return image, asWritten
	}
	if pinned, ok := p.Images[image]; ok && pinned != "" {
		return pinned, image
	}
	return image, asWritten
}

func normResources(r *Resources) *Resources {
	out := Resources{VCPUs: DefaultVCPUs, MemoryMib: DefaultMemoryMib}
	if r != nil {
		if r.VCPUs != 0 {
			out.VCPUs = r.VCPUs
		}
		if r.MemoryMib != 0 {
			out.MemoryMib = r.MemoryMib
		}
	}
	return &out
}

func normReadiness(r *Readiness) *Readiness {
	if r == nil {
		return nil
	}
	out := *r
	out.InitialDelay = normDuration(r.InitialDelay, DefaultInitialDelay)
	out.Period = normDuration(r.Period, DefaultPeriod)
	if out.FailureThreshold == 0 {
		out.FailureThreshold = DefaultFailureThreshold
	}
	return &out
}

func normService(svc Service, p Pins) Service {
	out := svc
	out.Env = maps.Clone(svc.Env)
	out.Mounts = nil
	out.Readiness = normReadiness(svc.Readiness)
	if out.Runtime == "" {
		out.Runtime = RuntimeImage
	}
	if out.Runtime == RuntimeAttached {
		out.Image, out.ImageAsWritten, out.ImageCredentials = "", "", nil
		out.Replicas, out.Resources, out.Idle, out.Rollout = nil, nil, "", nil
		return out
	}
	out.Image, out.ImageAsWritten = pin(svc.Image, svc.ImageAsWritten, p)
	replicas := DefaultReplicas
	if svc.Replicas != nil {
		replicas = *svc.Replicas
	}
	out.Replicas = &replicas
	out.Resources = normResources(svc.Resources)
	if svc.Idle != "" {
		out.Idle = normDuration(svc.Idle, "")
	}
	timeout := ""
	if svc.Rollout != nil {
		timeout = svc.Rollout.Timeout
	}
	out.Rollout = &Rollout{Timeout: normDuration(timeout, DefaultRolloutTimeout)}
	return out
}

func normJob(j Job, p Pins) Job {
	out := j
	out.Env = maps.Clone(j.Env)
	out.Image, out.ImageAsWritten = pin(j.Image, j.ImageAsWritten, p)
	if out.On == "" {
		out.On = JobOnRelease
	}
	out.Timeout = normDuration(j.Timeout, DefaultJobTimeout)
	out.Resources = normResources(j.Resources)
	return out
}

// Hash is the SHA-256 (hex) of a spec's canonical JSON: the identity of a
// normalised spec.
func Hash(s Spec) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
