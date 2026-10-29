// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/addons"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The bounds of what a release carries besides its spec.
const (
	maxAnnotationBytes = 4 << 10
	maxMessageBytes    = 64 << 10
	maxAuthorBytes     = 1 << 10
	// imageWorkers bounds the images one plan resolves at once.
	imageWorkers = 4
)

// validateCtx is the spec.ValidateContext of one plan: what the parsed
// spec names, read before it is validated.
type validateCtx struct {
	mode      spec.Mode
	sandboxes map[string]spec.SandboxInfo
	secrets   map[string]spec.SecretInfo
	images    map[string]imageAnswer
	// volumes are what the environment's addon volumes that are not being
	// deleted hold, by addon name.
	volumes   map[string]spec.AddonData
	blueprint func(typ string) (addons.Blueprint, bool)
	// zone is the apps zone (Deps.AppsZone); host the environment's
	// default host under it ("" without an environment, or for one never
	// published), public whether the environment is public.
	zone, host string
	public     bool
}

type imageAnswer struct {
	pinned string
	err    error
}

func (v *validateCtx) Mode() spec.Mode { return v.mode }

func (v *validateCtx) Sandbox(id string) (spec.SandboxInfo, bool) {
	sb, ok := v.sandboxes[id]
	return sb, ok
}

func (v *validateCtx) Secret(name string) (spec.SecretInfo, bool) {
	sec, ok := v.secrets[name]
	return sec, ok
}

func (v *validateCtx) Volume(addon string) (spec.AddonData, bool) {
	d, ok := v.volumes[addon]
	return d, ok
}

func (v *validateCtx) Blueprint(typ string) (spec.Blueprint, bool) {
	b, ok := v.blueprint(typ)
	if !ok {
		return nil, false
	}
	return specBlueprint{b}, true
}

func (v *validateCtx) Image(image string) (string, error) {
	if spec.IsDigestRef(image) {
		return image, nil
	}
	a, ok := v.images[image]
	if !ok {
		return "", errors.New("it was not resolved")
	}
	return a.pinned, a.err
}

// IngressHost is the environment's default host and whether it is public;
// ok false when the platform has no apps zone, so that no ingress serves
// any route.
func (v *validateCtx) IngressHost() (host string, public, ok bool) {
	return v.host, v.public, v.zone != ""
}

// BlueprintTypes are the registered addon types.
func (v *validateCtx) BlueprintTypes() []string {
	var out []string
	for _, t := range addons.Types() {
		if _, ok := v.blueprint(t); ok {
			out = append(out, t)
		}
	}
	return out
}

var _ spec.ValidateContext = (*validateCtx)(nil)

// images lists the images of a spec's image services and jobs that are
// not digest references, by name.
func imagesOf(s spec.Spec) []imageAsk {
	set := map[string]*spec.ImageCredentials{}
	add := func(image string, creds *spec.ImageCredentials) {
		if image == "" || spec.IsDigestRef(image) {
			return
		}
		if prev, ok := set[image]; !ok || prev == nil {
			set[image] = creds
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Services)) {
		if svc := s.Services[name]; svc.Runtime != spec.RuntimeAttached {
			add(svc.Image, svc.ImageCredentials)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Jobs)) {
		add(s.Jobs[name].Image, s.Jobs[name].ImageCredentials)
	}
	out := make([]imageAsk, 0, len(set))
	for _, image := range slices.Sorted(maps.Keys(set)) {
		out = append(out, imageAsk{image: image, creds: set[image]})
	}
	return out
}

// imageAsk is an image a spec names, with the login of the first service
// or job (by name) that names it with one (ADR 0025).
type imageAsk struct {
	image string
	creds *spec.ImageCredentials
}

// credentialsOf is the login c names, its secrets those of env at their
// latest versions; without env, a secret does not resolve.
func (s *Service) credentialsOf(ctx context.Context, env *store.Environment, c *spec.ImageCredentials) (
	model.RegistryCredentials, error) {
	if c == nil {
		return model.RegistryCredentials{}, nil
	}
	value := func(v spec.EnvValue) (string, error) {
		if v.Kind != spec.EnvSecret {
			return v.Value, nil
		}
		if env == nil {
			return "", fmt.Errorf("secret %s: the environment does not exist yet", v.Secret)
		}
		return s.ctl.ResolveSecret(ctx, env.ID, v.Secret, 0)
	}
	user, err := value(c.Username)
	if err != nil {
		return model.RegistryCredentials{}, err
	}
	password, err := value(c.Password)
	if err != nil {
		return model.RegistryCredentials{}, err
	}
	return model.RegistryCredentials{Username: user, Password: password}, nil
}

// resolveImages pins the images through the engine, each with its login,
// a few at a time, outside any transaction.
func (s *Service) resolveImages(ctx context.Context, env *store.Environment, asks []imageAsk) map[string]imageAnswer {
	out := make(map[string]imageAnswer, len(asks))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, imageWorkers)
	for _, ask := range asks {
		img := ask.image
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				out[img] = imageAnswer{err: ctx.Err()}
				mu.Unlock()
				return
			}
			auth, err := s.credentialsOf(ctx, env, ask.creds)
			pinned := ""
			if err == nil {
				pinned, err = s.d.Engine.ResolveImage(ctx, img, auth)
			} else {
				err = fmt.Errorf("its credentials: %w", err)
			}
			<-sem
			mu.Lock()
			out[img] = imageAnswer{pinned: pinned, err: err}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// loadContext reads what a spec names for its validation, and the pins
// of its normalisation: the tenant's sandboxes its attached services run
// in (with the ports other environments' attached services use there,
// and the names those environments give them),
// the environment's secrets (secrets, when not nil, stands in for them:
// a clone's), what its addon volumes hold, the addon types, the images,
// and its default host under the apps zone. env is nil for a spec without
// an environment.
func (s *Service) loadContext(ctx context.Context, tenant string, env *store.Environment, sp spec.Spec, mode spec.Mode,
	secrets map[string]spec.SecretInfo) (*validateCtx, spec.Pins, error) {
	vc := &validateCtx{mode: mode, sandboxes: map[string]spec.SandboxInfo{}, secrets: secrets,
		volumes: map[string]spec.AddonData{}, blueprint: s.d.Blueprint, zone: s.d.AppsZone}
	if env != nil {
		vc.public = env.Public
		if vc.zone != "" && env.HostLabel != "" {
			vc.host = env.HostLabel + "." + vc.zone
		}
	}
	pins := spec.Pins{Images: map[string]string{}, Secrets: map[string]int{}, Plans: map[string]string{}}

	vc.images = s.resolveImages(ctx, env, imagesOf(sp))
	for img, a := range vc.images {
		if a.err == nil && a.pinned != "" {
			pins.Images[img] = a.pinned
		}
	}
	for _, a := range sp.Addons {
		if b, ok := s.d.Blueprint(a.Type); ok {
			pins.Plans[a.Type] = b.DefaultPlan()
		}
	}

	var ids []string
	for _, svc := range sp.Services {
		if svc.Runtime != spec.RuntimeAttached {
			continue
		}
		id, err := podid.Parse(svc.Sandbox)
		if err != nil || vc.sandboxes[id.String()].ID != "" {
			continue
		}
		sb, err := s.st.GetSandbox(ctx, id)
		if errors.Is(err, model.ErrNotFound) || (err == nil && (sb.Owner != tenant || sb.IsInstance())) {
			continue
		}
		if err != nil {
			return nil, pins, fmt.Errorf("sandbox %s: %w", id, err)
		}
		vc.sandboxes[id.String()] = spec.SandboxInfo{ID: id.String(), Attachable: attachable(sb)}
		ids = append(ids, id.String())
	}
	if len(ids) > 0 {
		exclude := uuid.Nil
		if env != nil {
			exclude = env.ID
		}
		others, err := s.st.AttachedSpecs(ctx, tenant, exclude, ids)
		if err != nil {
			return nil, pins, err
		}
		for _, o := range others {
			other, err := parseStored(o.Spec)
			if err != nil {
				s.log.Warn("a stored spec does not parse", "env", o.EnvID, "err", err)
				continue
			}
			named := map[string]bool{}
			for _, name := range slices.Sorted(maps.Keys(other.Services)) {
				svc := other.Services[name]
				sb := strings.ToLower(svc.Sandbox)
				info, ok := vc.sandboxes[sb]
				if svc.Runtime != spec.RuntimeAttached || !ok {
					continue
				}
				for _, p := range svc.Ports {
					info.AttachedPorts = append(info.AttachedPorts, spec.AttachedPort{Env: o.EnvName, Service: name, Port: p.Port})
				}
				if !named[sb] {
					named[sb] = true
					for _, hn := range spec.SandboxNames(*other, sb) {
						info.Names = append(info.Names, spec.SandboxName{Env: o.EnvName, Name: hn})
					}
				}
				vc.sandboxes[sb] = info
			}
		}
	}

	// An environment without a release has no addon volumes. A clone
	// plans for one that is not there yet, under an ID that may be
	// another tenant's environment's (its insert refuses that later):
	// what that one's volumes hold is no business of the clone's.
	if env != nil && env.Release > 0 && len(sp.Addons) > 0 {
		vols, err := s.st.ListEnvVolumes(ctx, env.ID)
		if err != nil {
			return nil, pins, fmt.Errorf("volumes: %w", err)
		}
		for _, vol := range vols {
			name, ok := strings.CutPrefix(vol.Object, spec.KindAddon+"/")
			if ok && !vol.Deleting && vol.AddonType != "" {
				vc.volumes[name] = spec.AddonData{Type: vol.AddonType, Version: vol.DataVersion}
			}
		}
	}

	if secrets == nil && env != nil {
		versions, err := s.st.TenantSecretVersions(ctx, env.ID)
		if err != nil {
			return nil, pins, fmt.Errorf("secrets: %w", err)
		}
		vc.secrets = make(map[string]spec.SecretInfo, len(versions))
		for name, v := range versions {
			info := spec.SecretInfo{Versions: v.Versions, Deleted: v.Deleted}
			if len(v.Versions) > 0 {
				info.Latest = v.Versions[len(v.Versions)-1]
			}
			vc.secrets[name] = info
		}
	}
	for name, info := range vc.secrets {
		if info.Latest > 0 {
			pins.Secrets[name] = info.Latest
		}
	}
	return vc, pins, nil
}

// attachable reports whether a sandbox was created attachable: its pod
// has its own sbx- group.
func attachable(sb store.Sandbox) bool {
	return sb.PodTemplate != nil && slices.Contains(sb.PodTemplate.SecurityGroups, model.SandboxGroupID(sb.ID))
}

// planned is a plan of a spec for an environment.
type planned struct {
	findings []spec.Finding
	// parsed: the spec could be read (else norm and diff are empty).
	parsed bool
	norm   spec.Spec
	diff   []spec.DiffEntry
	// base is the release the diff is against, current the
	// environment's current release when the plan read it, currentSpec
	// its spec (nil without one).
	base, current int
	currentSpec   *spec.Spec
}

func (p planned) valid() bool { return p.parsed && !spec.HasErrors(p.findings) }

func (p planned) answer() PlanAnswer {
	return PlanAnswer{Valid: p.valid(), Findings: nonNilFindings(p.findings), Diff: nonNilDiff(p.diff), Base: p.base,
		Current: p.current}
}

// plan parses, validates and normalises raw for the environment env and
// diffs it against release base (nil: the current one). secrets, when
// not nil, stands in for the environment's secrets.
func (s *Service) plan(ctx context.Context, tenant string, env store.Environment, raw []byte, base *int, mode spec.Mode,
	secrets map[string]spec.SecretInfo) (planned, error) {
	p := planned{base: env.Release, current: env.Release}
	if base != nil {
		if *base < 0 || *base > env.Release {
			return planned{}, errNoRelease()
		}
		p.base = *base
	}
	if env.Release > 0 {
		cur, err := s.releaseSpec(ctx, env, env.Release)
		if err != nil {
			return planned{}, err
		}
		p.currentSpec = cur
	}
	sp, fs := spec.Parse(raw)
	if spec.HasErrors(fs) {
		p.findings = fs
		return p, nil
	}
	vc, pins, err := s.loadContext(ctx, tenant, &env, sp, mode, secrets)
	if err != nil {
		return planned{}, err
	}
	fs = append(fs, spec.Validate(vc, sp, p.currentSpec)...)
	norm, nfs := spec.Normalise(sp, pins)
	fs = append(fs, nfs...)
	baseSpec := p.currentSpec
	if p.base != p.current {
		baseSpec = nil
		if p.base > 0 {
			if baseSpec, err = s.releaseSpec(ctx, env, p.base); err != nil {
				return planned{}, err
			}
		}
	}
	p.parsed, p.norm = true, norm
	p.diff = spec.DiffWith(baseSpec, &norm, s.addonEffect)
	if p.currentSpec != nil && spec.EqualNormalised(norm, *p.currentSpec) {
		fs = append(fs, spec.Finding{Severity: spec.SeverityInfo, Path: "",
			Problem: fmt.Sprintf("nothing changes: the spec equals release %d's", env.Release), Remedy: ""})
	}
	p.findings = spec.SortFindings(fs)
	return p, nil
}

// applyArgs are what an apply makes a release of.
type applyArgs struct {
	raw             []byte
	base            int
	author, message string
	annotations     map[string]string
	origin          string
	originRef       string
}

// errReplan is an apply whose plan is stale: the environment got a new
// release between the plan and the transaction, which the request's base
// already names, or a secret version the plan pins was deleted.
var errReplan = errors.New("the current release changed since the plan")

// apply plans raw (ModeApply: an image that cannot be resolved is an
// error), refuses error findings, and in one transaction (the tenant FOR
// SHARE, the environment FOR UPDATE) answers the current release
// unchanged when the spec equals it and it did not fail, refuses a base
// that is not the current release, inserts the next release pending,
// supersedes the older pending and rolling ones and wakes the
// controller.
func (s *Service) apply(ctx context.Context, tenant, ref string, a applyArgs) (ApplyAnswer, error) {
	for range 3 {
		ans, err := s.applyOnce(ctx, tenant, ref, a)
		if !errors.Is(err, errReplan) {
			return ans, err
		}
	}
	return ApplyAnswer{}, errors.New("the environment keeps getting new releases; try again")
}

func (s *Service) applyOnce(ctx context.Context, tenant, ref string, a applyArgs) (ApplyAnswer, error) {
	env, err := resolveEnv(ctx, s.st, tenant, ref, store.NoLock)
	if err != nil {
		return ApplyAnswer{}, err
	}
	if env.Deleting {
		return ApplyAnswer{}, errDeleting(env.Name)
	}
	p, err := s.plan(ctx, tenant, env, a.raw, nil, spec.ModeApply, nil)
	if err != nil {
		return ApplyAnswer{}, err
	}
	if !p.valid() {
		return ApplyAnswer{}, &InvalidSpecError{Findings: p.findings}
	}
	specJSON, err := json.Marshal(p.norm)
	if err != nil {
		return ApplyAnswer{}, fmt.Errorf("the normalised spec: %w", err)
	}
	if s.afterPlan != nil {
		s.afterPlan()
	}
	var ans ApplyAnswer
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if err := lockTenant(ctx, tx, tenant); err != nil {
			return err
		}
		e, err := tx.LockEnvironment(ctx, env.ID, store.ForUpdate)
		if errors.Is(err, store.ErrNoRow) {
			return errNoEnvironment()
		}
		if err != nil {
			return err
		}
		if e.Deleting {
			return errDeleting(e.Name)
		}
		if e.Release != p.current {
			if a.base == e.Release {
				return errReplan
			}
			return &BaseMismatchError{Base: a.base, Current: e.Release}
		}
		// The plan read the secrets before the lock: a DeleteSecret, which
		// takes the same lock, may have deleted a version it pins since.
		if err := checkPins(ctx, tx, e.ID, p.norm.Secrets); err != nil {
			return err
		}
		if e.Release > 0 && spec.EqualNormalised(p.norm, *p.currentSpec) {
			cur, err := tx.GetRelease(ctx, e.ID, e.Release)
			if err != nil {
				return fmt.Errorf("release %d: %w", e.Release, err)
			}
			if cur.State != model.ReleaseFailed {
				tags, err := tx.ListTags(ctx, e.ID)
				if err != nil {
					return err
				}
				var names []string
				for _, t := range tags {
					if t.Number == cur.Number {
						names = append(names, t.Tag)
					}
				}
				ans = ApplyAnswer{ReleaseInfo: releaseInfo(cur, names), Unchanged: true, Diff: []spec.DiffEntry{},
					Findings: nonNilFindings(p.findings)}
				return nil
			}
		}
		if a.base != e.Release {
			return &BaseMismatchError{Base: a.base, Current: e.Release}
		}
		number := e.Release + 1
		r, err := tx.InsertRelease(ctx, store.EnvRelease{EnvID: e.ID, Number: number, Spec: specJSON, Base: a.base,
			Author: a.author, Message: a.message, Annotations: a.annotations, Origin: a.origin, OriginRef: a.originRef,
			State: model.ReleasePending})
		if err != nil {
			return err
		}
		after := e
		after.Release = number
		if _, err := tx.UpdateEnvironment(ctx, e, after); err != nil {
			return err
		}
		if _, err := tx.SupersedeReleases(ctx, e.ID, number, model.ReleasePending, model.ReleaseRolling); err != nil {
			return err
		}
		if err := wake(ctx, tx, ctl.HintReconcile, ctl.HintGroups); err != nil {
			return err
		}
		ans = ApplyAnswer{ReleaseInfo: releaseInfo(r, nil), Diff: nonNilDiff(p.diff), Findings: nonNilFindings(p.findings)}
		return nil
	})
	if err != nil {
		return ApplyAnswer{}, err
	}
	return ans, nil
}

// checkPins answers errReplan when a secret version a normalised spec
// pins is no longer there or deleted: inside the apply's transaction,
// after the environment's lock.
func checkPins(ctx context.Context, tx *store.Store, envID uuid.UUID, pins map[string]spec.SecretRef) error {
	if len(pins) == 0 {
		return nil
	}
	versions, err := tx.TenantSecretVersions(ctx, envID)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	for name, ref := range pins {
		if !slices.Contains(versions[name].Versions, ref.Version) {
			return errReplan
		}
	}
	return nil
}

// checkReleaseMeta refuses an author, a message or annotations that a
// release cannot carry.
func checkReleaseMeta(author, message string, annotations map[string]string) error {
	if len(author) > maxAuthorBytes {
		return errBadRequest("author has %d bytes; at most %d are allowed", len(author), maxAuthorBytes)
	}
	if len(message) > maxMessageBytes {
		return errBadRequest("message has %d bytes; at most %d are allowed", len(message), maxMessageBytes)
	}
	for k := range annotations {
		if k == "" {
			return errBadRequest("an annotation needs a key")
		}
	}
	if b, _ := json.Marshal(annotations); len(annotations) > 0 && len(b) > maxAnnotationBytes {
		return errBadRequest("annotations take %d bytes; at most %d are allowed", len(b), maxAnnotationBytes)
	}
	return nil
}

// annotationRevertOf is the annotation of a revert: the number of the
// release it reverts to.
const annotationRevertOf = "ferun.revertOf"

// revertAnnotations are release n's annotations plus ferun.revertOf.
func revertAnnotations(r store.EnvRelease) map[string]string {
	out := maps.Clone(r.Annotations)
	if out == nil {
		out = map[string]string{}
	}
	out[annotationRevertOf] = strconv.Itoa(r.Number)
	return out
}

// cloneAnnotations are the source release's annotations for a clone's
// release 1, without ferun.revertOf: its number names a release of the
// source, which the clone does not have.
func cloneAnnotations(a map[string]string) map[string]string {
	out := maps.Clone(a)
	delete(out, annotationRevertOf)
	return out
}

// objectMaps are the spec's maps by object kind.
var objectMaps = map[string]string{
	spec.KindService: "services", spec.KindJob: "jobs", spec.KindAddon: "addons", spec.KindRoute: "routes",
	spec.KindSecret: "secrets",
}

// withObject is the current release's spec (raw JSON, "" for none) with
// object set (nil: removed) under kind's map, and whether the object was
// there.
func withObject(current []byte, kind, name string, object json.RawMessage) ([]byte, bool, error) {
	key, ok := objectMaps[kind]
	if !ok {
		return nil, false, errBadRequest("kind must be service, job, addon, route or secret, not %q", kind)
	}
	top := map[string]json.RawMessage{}
	if len(current) > 0 {
		if err := json.Unmarshal(current, &top); err != nil {
			return nil, false, fmt.Errorf("the current spec: %w", err)
		}
	}
	objs := map[string]json.RawMessage{}
	if raw, ok := top[key]; ok {
		if err := json.Unmarshal(raw, &objs); err != nil {
			return nil, false, fmt.Errorf("the current spec's %s: %w", key, err)
		}
	}
	_, had := objs[name]
	if object == nil {
		delete(objs, name)
	} else {
		objs[name] = object
	}
	if len(objs) == 0 {
		delete(top, key)
	} else {
		b, err := json.Marshal(objs)
		if err != nil {
			return nil, false, errBadRequest("object: %v", err)
		}
		top[key] = b
	}
	b, err := json.Marshal(top)
	if err != nil {
		return nil, false, err
	}
	return b, had, nil
}
