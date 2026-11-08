// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// AddonEffect says what an update of an addon's options does to its pod
// (its version, plan and type unchanged): EffectRecreates when the pod's
// template changes, EffectReprovisions when only the provisioning steps
// do, EffectInPlace when neither. The environment service answers it
// with the addon's blueprint; "" counts as EffectRecreates.
type AddonEffect func(name string, prev, next Addon) string

// Diff lists the changes from prev to next, both normalised, one entry
// per changed object, ordered by kind (services, jobs, addons, routes,
// secrets, bindings, security groups) and name; nil prev is an environment without a deployment (every
// object an add), nil next one whose objects all go. An update lists its
// changed fields and the effect on the running pods: "in place" (adds
// and removes but an addon's, routes, secrets, replicas, ports,
// readiness, idle, rollout, a job's on, timeout and retries), "restarts"
// (an attached service's process, a job that runs again, a service that
// changes its runtime), EffectReplaces(n) (an image service's template:
// image, command, env, resources: a rolling replacement of its n
// replicas), "recreates (downtime)" (an addon's version, plan or
// options), "deletes data" (an addon removed or changing its type).
// Secret values never appear; versions do: a service whose referenced
// secret moves to another version shows the change in its env, with the
// effect of a changed template. An image shows as written with its
// digest: "ghcr.io/acme/x:1.4 (sha256:…)". A binding (ADR 0028) whose
// credential or options change, a referenced secret's version among
// them, is sealed to the running pods again: "in place"; one whose
// upstream or mode changes replaces the pods of the image services that
// name it and restarts the jobs, as their own entries show (a binding
// named, its upstream and its mode are part of their template). A
// security group (ADR 0034) whose rules change is put whole on the nodes
// that hold it: "in place"; the groups an object names are part of its
// pods' template, so a changed list replaces an image service's pods,
// restarts a job and recreates an addon.
func Diff(prev, next *Spec) []DiffEntry { return DiffWith(prev, next, nil) }

// DiffWith is Diff with the effects of addons' option updates told by
// addonEffect (nil: every such update recreates).
func DiffWith(prev, next *Spec, addonEffect AddonEffect) []DiffEntry {
	if prev == nil {
		prev = &Spec{}
	}
	if next == nil {
		next = &Spec{}
	}
	d := differ{prev: prev, next: next, addonEffect: addonEffect}
	for _, name := range unionKeys(prev.Services, next.Services) {
		a, inA := prev.Services[name]
		b, inB := next.Services[name]
		d.object(KindService, name, inA, inB, func() ([]FieldChange, string) { return d.service(a, b) })
	}
	for _, name := range unionKeys(prev.Jobs, next.Jobs) {
		a, inA := prev.Jobs[name]
		b, inB := next.Jobs[name]
		d.object(KindJob, name, inA, inB, func() ([]FieldChange, string) { return d.job(a, b) })
	}
	for _, name := range unionKeys(prev.Addons, next.Addons) {
		a, inA := prev.Addons[name]
		b, inB := next.Addons[name]
		d.object(KindAddon, name, inA, inB, func() ([]FieldChange, string) { return d.addon(name, a, b) })
	}
	for _, name := range unionKeys(prev.Routes, next.Routes) {
		a, inA := prev.Routes[name]
		b, inB := next.Routes[name]
		d.object(KindRoute, name, inA, inB, func() ([]FieldChange, string) {
			return changes(view(a), view(b)), EffectInPlace
		})
	}
	for _, name := range unionKeys(prev.Secrets, next.Secrets) {
		a, inA := prev.Secrets[name]
		b, inB := next.Secrets[name]
		d.object(KindSecret, name, inA, inB, func() ([]FieldChange, string) {
			return changes(view(a), view(b)), EffectInPlace
		})
	}
	for _, name := range unionKeys(prev.Bindings, next.Bindings) {
		a, inA := prev.Bindings[name]
		b, inB := next.Bindings[name]
		d.object(KindBinding, name, inA, inB, func() ([]FieldChange, string) { return d.binding(name, a, b) })
	}
	if d.out == nil {
		return []DiffEntry{}
	}
	return d.out
}

// differ is one Diff.
type differ struct {
	prev, next  *Spec
	addonEffect AddonEffect
	out         []DiffEntry
}

// object adds the entry of one object: an add, a remove, or an update
// when update finds changed fields.
func (d *differ) object(kind, name string, inPrev, inNext bool, update func() ([]FieldChange, string)) {
	e := DiffEntry{Kind: kind, Name: name, Fields: []FieldChange{}, Effect: EffectInPlace}
	switch {
	case !inPrev:
		e.Change = ChangeAdd
	case !inNext:
		e.Change = ChangeRemove
		if kind == KindAddon {
			e.Effect = EffectDeletes
		}
	default:
		fields, effect := update()
		if len(fields) == 0 {
			return
		}
		e.Change, e.Fields, e.Effect = ChangeUpdate, fields, effect
	}
	d.out = append(d.out, e)
}

func (d *differ) service(a, b Service) ([]FieldChange, string) {
	va, vb := d.serviceView(a, d.prev), d.serviceView(b, d.next)
	fields := changes(va, vb)
	ta, tb := d.template(a, d.prev), d.template(b, d.next)
	changed := !reflect.DeepEqual(ta, tb)
	if len(fields) == 0 && changed {
		// Only what it references changed: a referenced service's port, a
		// named binding's upstream or mode.
		fields = changes(map[string]any{"env": ta["env"]}, map[string]any{"env": tb["env"]})
		if !reflect.DeepEqual(ta["bindings"], tb["bindings"]) {
			fields = append(fields, FieldChange{Path: "bindings", From: ta["bindings"], To: tb["bindings"]})
		}
	}
	switch {
	case runtimeOf(a) != runtimeOf(b):
		return fields, EffectRestarts
	case !changed:
		return fields, EffectInPlace
	case runtimeOf(b) == RuntimeAttached:
		return fields, EffectRestarts
	}
	replicas := DefaultReplicas
	if b.Replicas != nil {
		replicas = *b.Replicas
	}
	return fields, EffectReplaces(replicas)
}

func (d *differ) job(a, b Job) ([]FieldChange, string) {
	va, vb := view(a), view(b)
	// A deploy job's on shows in its new word, whichever word is stored.
	if IsJobOnDeploy(a.On) {
		va["on"] = JobOnDeploy
	}
	if IsJobOnDeploy(b.On) {
		vb["on"] = JobOnDeploy
	}
	delete(va, "imageAsWritten")
	delete(vb, "imageAsWritten")
	va["image"], vb["image"] = imageDisplay(a.Image, a.ImageAsWritten), imageDisplay(b.Image, b.ImageAsWritten)
	va["env"], vb["env"] = envView(a.Env, d.prev, false), envView(b.Env, d.next, false)
	fields := changes(clean(va), clean(vb))
	if reflect.DeepEqual(va["bindings"], vb["bindings"]) && !reflect.DeepEqual(bindingsTemplate(a.Bindings, d.prev),
		bindingsTemplate(b.Bindings, d.next)) {
		fields = append(fields, FieldChange{Path: "bindings", From: bindingsTemplate(a.Bindings, d.prev),
			To: bindingsTemplate(b.Bindings, d.next)})
	}
	ta := map[string]any{"image": a.Image, "command": a.Command, "env": envView(a.Env, d.prev, true),
		"resources": view(a.Resources), "imageCredentials": toAny(a.ImageCredentials),
		"bindings": bindingsTemplate(a.Bindings, d.prev), "egress": egressTemplate(a.Egress)}
	tb := map[string]any{"image": b.Image, "command": b.Command, "env": envView(b.Env, d.next, true),
		"resources": view(b.Resources), "imageCredentials": toAny(b.ImageCredentials),
		"bindings": bindingsTemplate(b.Bindings, d.next), "egress": egressTemplate(b.Egress)}
	if reflect.DeepEqual(ta, tb) {
		return fields, EffectInPlace
	}
	if len(fields) == 0 {
		fields = changes(map[string]any{"env": ta["env"]}, map[string]any{"env": tb["env"]})
	}
	return fields, EffectRestarts
}

func (d *differ) addon(name string, a, b Addon) ([]FieldChange, string) {
	fields := changes(view(a), view(b))
	switch {
	case len(fields) == 0:
		return nil, EffectInPlace
	case a.Type != b.Type:
		return fields, EffectDeletes
	case a.Version != b.Version || a.Plan != b.Plan:
		return fields, EffectRecreates
	}
	if d.addonEffect != nil {
		if e := d.addonEffect(name, a, b); e != "" {
			return fields, e
		}
	}
	return fields, EffectRecreates
}

func runtimeOf(s Service) string {
	if s.Runtime == "" {
		return RuntimeImage
	}
	return s.Runtime
}

// serviceView is a service as its diff shows it: the image as written
// with its digest, secret references with their versions.
func (d *differ) serviceView(s Service, sp *Spec) map[string]any {
	v := view(s)
	delete(v, "imageAsWritten")
	if s.Image != "" {
		v["image"] = imageDisplay(s.Image, s.ImageAsWritten)
	}
	v["env"] = envView(s.Env, sp, false)
	return clean(v)
}

// template is what a service's pods are made from: what changes them.
func (d *differ) template(s Service, sp *Spec) map[string]any {
	if runtimeOf(s) == RuntimeAttached {
		return map[string]any{"sandbox": strings.ToLower(s.Sandbox), "command": s.Command, "cwd": s.Cwd,
			"env": envView(s.Env, sp, true)}
	}
	return map[string]any{"image": s.Image, "command": s.Command, "env": envView(s.Env, sp, true),
		"resources": view(s.Resources), "imageCredentials": toAny(s.ImageCredentials),
		"bindings": bindingsTemplate(s.Bindings, sp), "egress": egressTemplate(s.Egress)}
}

// egressTemplate is what of an egress a pod's template holds (ADR 0034,
// revised): whether its resolver restricts names, and the names; nil for
// the internet. Its blocks are rules, changed in place.
func egressTemplate(e *Egress) any {
	if !e.RestrictsNames() {
		return nil
	}
	return map[string]any{"restrictNames": true, "domains": toAny(e.NameDomains())}
}

// bindingsTemplate is what of an object's bindings its template holds:
// each binding it names with its upstream and mode, by name.
func bindingsTemplate(names []string, sp *Spec) any {
	if len(names) == 0 {
		return nil
	}
	out := make([]any, 0, len(names))
	for _, n := range names {
		b, ok := sp.Bindings[n]
		if !ok {
			out = append(out, map[string]any{"name": n})
			continue
		}
		up, mode := BindingTemplate(b)
		out = append(out, map[string]any{"name": n, "upstream": up, "mode": mode})
	}
	return out
}

// binding is a binding's update: its fields with the secrets' versions,
// and its effect, in place unless its upstream or mode changed.
func (d *differ) binding(name string, a, b Binding) ([]FieldChange, string) {
	fields := changes(bindingView(a, d.prev), bindingView(b, d.next))
	if len(fields) == 0 {
		return nil, EffectInPlace
	}
	ua, ma := BindingTemplate(a)
	ub, mb := BindingTemplate(b)
	if ua == ub && ma == mb {
		return fields, EffectInPlace
	}
	replicas, jobs := 0, false
	for _, svc := range d.next.Services {
		if runtimeOf(svc) == RuntimeImage && slices.Contains(svc.Bindings, name) {
			if svc.Replicas != nil {
				replicas += *svc.Replicas
			} else {
				replicas += DefaultReplicas
			}
		}
	}
	for _, j := range d.next.Jobs {
		jobs = jobs || slices.Contains(j.Bindings, name)
	}
	switch {
	case replicas > 0:
		return fields, EffectReplaces(replicas)
	case jobs:
		return fields, EffectRestarts
	}
	return fields, EffectInPlace
}

// bindingView is a binding as its diff shows it: its credential's values
// in their JSON form, a secret with the version sp pins.
func bindingView(b Binding, sp *Spec) map[string]any {
	v := view(b)
	if b.Credential == nil {
		return v
	}
	c := view(b.Credential)
	for path, val := range b.Credential.Values() {
		shown := toAny(val)
		if val.Kind == EnvSecret {
			shown = map[string]any{"secret": val.Secret, "version": float64(sp.Secrets[val.Secret].Version)}
		}
		if h, ok := strings.CutPrefix(path, "headers."); ok {
			hs, _ := c["headers"].(map[string]any)
			if hs != nil {
				hs[h] = shown
			}
			continue
		}
		c[path] = shown
	}
	v["credential"] = c
	return v
}

// envView is env as a diff shows it: each value in its JSON form, a
// secret reference with the version sp pins, and, resolved, a service
// reference with its port's number.
func envView(env map[string]EnvValue, sp *Spec, resolved bool) any {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]any, len(env))
	for k, val := range env {
		switch val.Kind {
		case EnvSecret:
			out[k] = map[string]any{"secret": val.Secret, "version": float64(sp.Secrets[val.Secret].Version)}
		case EnvService:
			m, _ := toAny(val).(map[string]any)
			if resolved && m != nil {
				if n, ok := PortNumber(sp.Services[val.Service].Ports, val.Port); ok {
					m["port"] = float64(n)
				}
			}
			out[k] = m
		default:
			out[k] = toAny(val)
		}
	}
	return out
}

// imageDisplay is an image as a diff shows it: "<as written> (<digest>)"
// when pinned, else as it is.
func imageDisplay(image, asWritten string) string {
	if asWritten != "" && asWritten != image && IsDigestRef(image) {
		return asWritten + " (" + image[strings.Index(image, "@")+1:] + ")"
	}
	return image
}

// view is v's JSON as a map (nil for null).
func view(v any) map[string]any {
	m, _ := toAny(v).(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// toAny is v's JSON decoded into an any.
func toAny(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// clean removes the members of m whose value is nil.
func clean(m map[string]any) map[string]any {
	for k, v := range m {
		if v == nil {
			delete(m, k)
		}
	}
	return m
}

// changes lists the fields in which b differs from a, members of objects
// by their dotted paths (an env or options object member by member, also
// when the other side has none), anything else whole.
func changes(a, b map[string]any) []FieldChange {
	var out []FieldChange
	walkChanges("", a, b, &out)
	return out
}

func walkChanges(path string, a, b any, out *[]FieldChange) {
	am, aIsMap := a.(map[string]any)
	bm, bIsMap := b.(map[string]any)
	parent, key := "", path
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		parent, key = path[:i], path[i+1:]
	}
	parentKey := parent[strings.LastIndexByte(parent, '.')+1:]
	// An env value is one field, whatever its form, and so is a value of
	// a binding's credential (a header's among them).
	value := parent != "" && (parentKey == "env" || parentKey == "headers" || (parentKey == "credential" && key != "headers"))
	byMember := aIsMap && bIsMap && !value
	if !byMember && (aIsMap || bIsMap) && (a == nil || b == nil) {
		byMember = key == "env" || key == "options"
	}
	if byMember || path == "" {
		for _, k := range unionKeys(am, bm) {
			p := k
			if path != "" {
				p = path + "." + k
			}
			walkChanges(p, am[k], bm[k], out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, FieldChange{Path: path, From: a, To: b})
	}
}

// unionKeys are the keys of two maps, sorted.
func unionKeys[V any](a, b map[string]V) []string {
	keys := slices.Collect(maps.Keys(a))
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}
