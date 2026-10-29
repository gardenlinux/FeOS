// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/addons"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// podPlan is an object's pod as the target renders it.
type podPlan struct {
	kind, name string
	// image is the image as the spec (or the blueprint) writes it.
	image           string
	vcpus, memoryMB int
	// template has its labels, hash is its template hash.
	template model.PodTemplate
	hash     string
	// idle is the rows' idle_after_s: an image service's idle, else 0.
	idle  int
	check check
	// wait says why the pod cannot be rendered yet (an addon it refers to
	// has no address); err why it cannot be rendered at all.
	wait string
	err  error
}

// object is the plan's object name, "<kind>/<name>".
func (p *podPlan) object() string { return objectName(p.kind, p.name) }

// ok reports whether the plan rendered.
func (p *podPlan) ok() bool { return p.wait == "" && p.err == nil }

// why says why the plan did not render.
func (p *podPlan) why() string {
	if p.err != nil {
		return p.err.Error()
	}
	return p.wait
}

// minPodMemoryMiB is the least memory a sandbox row has
// (sandboxes.memory_mib's CHECK, --default-memory-mib's floor): a spec's
// or a plan's smaller size (128 MiB, valkey's and nats's dev plans) gets
// a pod of this size.
const minPodMemoryMiB = 256

// finish sizes the pod, hashes the template and labels it.
func (p *podPlan) finish(envID uuid.UUID) {
	p.memoryMB = max(p.memoryMB, minPodMemoryMiB)
	p.template.Labels = nil
	p.hash = p.template.Hash(p.image, p.vcpus, p.memoryMB)
	p.template.Labels = map[string]string{
		model.LabelEnvironment: envID.String(),
		model.LabelObject:      p.object(),
		model.LabelTemplate:    p.hash,
	}
}

// addonPlan is an addon's pod with what its blueprint says of it.
type addonPlan struct {
	podPlan
	bp    addons.Blueprint
	addon addons.Addon
	inst  addons.Instance
	plan  addons.Plan
	// credentials are the keys of its credentials.
	credentials []string
	// steps are its provisioning steps against its current address, their
	// env mapped to system secrets; stepsHash their hash.
	steps     []addons.Step
	stepsHash string
}

// attachedPlan is an attached service's process as the target renders it.
type attachedPlan struct {
	name    string
	sandbox podid.ID
	argv    []string
	cwd     string
	env     map[string]model.TemplateValue
	check   check
	hash    string
	wait    string
	err     error
}

func (p *attachedPlan) ok() bool { return p.wait == "" && p.err == nil }

func (p *attachedPlan) why() string {
	if p.err != nil {
		return p.err.Error()
	}
	return p.wait
}

// mapCredentials maps an addon's credential parts to its system secrets
// ("addon.<addon>.<key>", version 1), keeping their escapes.
func mapCredentials(addon string, parts []model.TemplatePart) []model.TemplatePart {
	out := make([]model.TemplatePart, len(parts))
	for i, p := range parts {
		if p.Credential != "" {
			p = model.TemplatePart{Secret: addons.CredentialSecret(addon, p.Credential), Version: 1, Escape: p.Escape}
		}
		out[i] = p
	}
	return out
}

// mapValue maps a blueprint's value (credential parts) to a template's.
func mapValue(addon string, v model.TemplateValue) model.TemplateValue {
	if v.Parts == nil {
		return v
	}
	return model.TemplateValue{Parts: mapCredentials(addon, v.Parts)}
}

// mapEnv maps a blueprint's env to a template's.
func mapEnv(addon string, env map[string]model.TemplateValue) map[string]model.TemplateValue {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]model.TemplateValue, len(env))
	for k, v := range env {
		out[k] = mapValue(addon, v)
	}
	return out
}

// specAddon is an addon of a spec as the blueprint takes it, its plan
// filled with the blueprint's default when it names none.
func specAddon(bp addons.Blueprint, name string, a spec.Addon) addons.Addon {
	plan := a.Plan
	if plan == "" {
		plan = bp.DefaultPlan()
	}
	return addons.Addon{Name: name, Type: a.Type, Version: a.Version, Plan: plan, Options: a.Options}
}

// hashJSON is the hex SHA-256 of v's JSON.
func hashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(fmt.Sprintf("unmarshalable: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// planAddon renders an addon of the target: its pod (with the volume
// when it has one; wait while its volume row is not there yet), its
// credentials and its provisioning steps against address.
func (c *Controller) planAddon(v *view, name string, a spec.Addon, vol *store.EnvVolume, address string) *addonPlan {
	p := &addonPlan{podPlan: podPlan{kind: spec.KindAddon, name: name}}
	bp, ok := c.d.Blueprint(a.Type)
	if !ok {
		p.err = fmt.Errorf("addon %s: no addon type %q", name, a.Type)
		return p
	}
	p.bp, p.addon = bp, specAddon(bp, name, a)
	plan, ok := bp.Plans()[p.addon.Plan]
	if !ok {
		p.err = fmt.Errorf("addon %s: %s has no plan %q", name, a.Type, p.addon.Plan)
		return p
	}
	p.plan = plan
	ctx := addons.Context{Env: v.env.Name, Name: name}
	inst, err := bp.Render(p.addon, ctx)
	if err != nil {
		p.err = err
		return p
	}
	p.inst = inst
	p.image, p.vcpus, p.memoryMB = inst.Image, plan.VCPUs, plan.MemoryMib
	p.credentials = bp.Credentials(p.addon)
	p.template = model.PodTemplate{
		Command:        slices.Clone(inst.Command),
		Env:            mapEnv(name, inst.Env),
		SecurityGroups: []string{model.EnvGroupID(v.env.ID)},
	}
	if inst.Mount != "" {
		if vol == nil {
			p.wait = "its volume is being recorded"
			return p
		}
		p.template.Volumes = []model.TemplateVolume{{Name: "data", VolumeID: vol.VolumeID}}
		p.template.Mounts = []model.TemplateMount{{Volume: "data", Path: inst.Mount}}
	}
	p.check = check{kind: "exec", argv: slices.Clone(inst.Probe), period: duration("", spec.DefaultPeriod),
		threshold: spec.DefaultFailureThreshold}
	if len(inst.Probe) == 0 {
		p.check.kind = ""
	}
	p.finish(v.env.ID)
	ctx.Address = address
	for _, s := range bp.Provision(p.addon, ctx) {
		p.steps = append(p.steps, addons.Step{Name: s.Name, Argv: slices.Clone(s.Argv), Env: mapEnv(name, s.Env)})
	}
	p.stepsHash = hashJSON(struct {
		Scheme int           `json:"scheme"`
		Steps  []addons.Step `json:"steps"`
	}{1, p.steps})
	return p
}

// planImage renders an image service or a job of the target: its pod is
// a member of the env group and resolves the env table's names (an
// addon's pod reaches nothing by name, and names no table). An image
// service's pod of an environment that was ever public also names its own
// group ing-<sandbox id> (IngressGroup; the insert appends the group), so
// that the first publish rolls the service once and later ones never.
func (c *Controller) planImage(v *view, kind, name, image string, command []string, env map[string]spec.EnvValue,
	res *spec.Resources, creds *spec.ImageCredentials) *podPlan {
	p := &podPlan{kind: kind, name: name, image: image}
	p.vcpus, p.memoryMB = resourcesOf(res)
	vals, wait, err := c.renderEnv(v, env)
	if err != nil || wait != "" {
		p.err, p.wait = err, wait
		return p
	}
	p.template = model.PodTemplate{
		Command:        slices.Clone(command),
		Env:            vals,
		SecurityGroups: []string{model.EnvGroupID(v.env.ID)},
		NameTables:     []string{model.EnvTableID(v.env.ID)},
		IngressGroup:   kind == spec.KindService && v.env.IngressGroup,
	}
	p.template.ImageCredentials = templateCredentials(creds)
	p.finish(v.env.ID)
	return p
}

// templateCredentials is a login for an image's registry as a pod's
// template keeps it (ADR 0025): a string as it is, a secret by its name
// at version 0, its latest when a pod is created.
func templateCredentials(c *spec.ImageCredentials) *model.TemplateCredentials {
	if c == nil {
		return nil
	}
	val := func(e spec.EnvValue) model.TemplateValue {
		if e.Kind == spec.EnvSecret {
			return model.TemplateValue{Secret: e.Secret}
		}
		return model.TemplateValue{Value: e.Value}
	}
	return &model.TemplateCredentials{Username: val(c.Username), Password: val(c.Password)}
}

// planService renders an image service of the target.
func (c *Controller) planService(v *view, name string, s spec.Service) *podPlan {
	p := c.planImage(v, spec.KindService, name, s.Image, s.Command, s.Env, s.Resources, s.ImageCredentials)
	p.idle = idleOf(s)
	p.check = checkOf(s)
	return p
}

// planJob renders a job of the target.
func (c *Controller) planJob(v *view, name string, j spec.Job) *podPlan {
	return c.planImage(v, spec.KindJob, name, j.Image, j.Command, j.Env, j.Resources, j.ImageCredentials)
}

// planAttached renders an attached service of the target: its process.
func (c *Controller) planAttached(v *view, name string, s spec.Service) *attachedPlan {
	p := &attachedPlan{name: name, argv: slices.Clone(s.Command), cwd: s.Cwd, check: checkOf(s)}
	id, err := podid.Parse(s.Sandbox)
	if err != nil {
		p.err = fmt.Errorf("service %s: sandbox %q is not a sandbox ID", name, s.Sandbox)
		return p
	}
	p.sandbox = id
	if len(p.argv) == 0 {
		p.err = fmt.Errorf("service %s has no command", name)
		return p
	}
	if p.env, p.wait, p.err = c.renderEnv(v, s.Env); p.err != nil || p.wait != "" {
		return p
	}
	p.hash = hashJSON(struct {
		Scheme  int                            `json:"scheme"`
		Sandbox podid.ID                       `json:"sandbox"`
		Command []string                       `json:"command"`
		Cwd     string                         `json:"cwd"`
		Env     map[string]model.TemplateValue `json:"env"`
	}{1, p.sandbox, p.argv, p.cwd, nonNilMap(p.env)})
	return p
}

func nonNilMap[V any](m map[string]V) map[string]V {
	if m == nil {
		return map[string]V{}
	}
	return m
}

// renderEnv renders a spec's env (§4.7): literals; a secret as its pinned
// version (the latest when the spec pins none); an addon's output against
// its current instance's address (wait while it has none), its
// credentials mapped to system secrets; a service as its name, its port
// number and http://<name>:<port>.
func (c *Controller) renderEnv(v *view, env map[string]spec.EnvValue) (map[string]model.TemplateValue, string, error) {
	if len(env) == 0 {
		return nil, "", nil
	}
	out := make(map[string]model.TemplateValue, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		e := env[k]
		switch e.Kind {
		case spec.EnvString, spec.EnvLiteral:
			out[k] = model.Literal(e.Value)
		case spec.EnvSecret:
			version := secretVersion(v.target, e.Secret)
			if version == 0 {
				version = v.secrets[e.Secret]
			}
			if version == 0 {
				return nil, "", fmt.Errorf("env %s: no secret %s", k, e.Secret)
			}
			out[k] = model.SecretValue(e.Secret, version, model.EscapeNone)
		case spec.EnvAddon:
			a, ok := v.target.Addons[e.Addon]
			if !ok {
				return nil, "", fmt.Errorf("env %s: no addon %s", k, e.Addon)
			}
			bp, ok := c.d.Blueprint(a.Type)
			if !ok {
				return nil, "", fmt.Errorf("env %s: no addon type %q", k, a.Type)
			}
			row, ok := v.addonRow(e.Addon)
			if !ok || row.Sandbox.Address == "" {
				return nil, fmt.Sprintf("waits for the address of addon %s", e.Addon), nil
			}
			parts, err := bp.Outputs(specAddon(bp, e.Addon, a),
				addons.Context{Env: v.env.Name, Name: e.Addon, Address: row.Sandbox.Address},
				addons.Ref{Key: e.Key, Fields: e.Fields})
			if err != nil {
				return nil, "", fmt.Errorf("env %s: %w", k, err)
			}
			out[k] = model.TemplateValue{Parts: mapCredentials(e.Addon, parts)}
		case spec.EnvService:
			s, ok := v.target.Services[e.Service]
			if !ok {
				return nil, "", fmt.Errorf("env %s: no service %s", k, e.Service)
			}
			port := portOf(s, e.Port)
			switch e.Key {
			case spec.ServiceKeyHost:
				out[k] = model.Literal(e.Service)
			case spec.ServiceKeyPort:
				out[k] = model.Literal(strconv.Itoa(port))
			default:
				out[k] = model.Literal("http://" + e.Service + ":" + strconv.Itoa(port))
			}
		default:
			return nil, "", fmt.Errorf("env %s: a value of unknown kind %q", k, e.Kind)
		}
	}
	return out, "", nil
}
