// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/env/addons"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/quota"
	"github.com/gardenlinux/feos/go/internal/routing"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The bounds of what a deployment carries besides its spec.
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
	// zone is the apps zone (Deps.AppsZone); host the environment's first
	// host (hostsOf), else its default host under its tenant's zone (""
	// without an environment, or for one never published), public whether
	// the environment is public.
	zone, host string
	public     bool
	// routeHosts are the hosts a route may name (RouteHosts), served those
	// the environment is served at now (hostsOf); hostsKnown: there is an
	// environment to judge a route's host against.
	routeHosts, served []string
	hostsKnown         bool
	// bindingsOn: HTTP bindings are on (Deps.Bindings); destinations are
	// the destination rule's words on the hosts of the spec's bindings,
	// asked before it is validated.
	bindingsOn   bool
	destinations map[string]error
	// credentials are the environment's credentials (ADR 0033), by name;
	// none for a spec without an environment.
	credentials map[string]spec.CredentialInfo
	// deny are the prefixes no pod reaches (Deps.DenyPrefixes).
	deny []netip.Prefix
	// stored: the spec is a stored deployment's (a revert's, or a clone's
	// of its source's deployment), whose bindings stated in it are
	// accepted as they ran (ADR 0055).
	stored bool
}

type imageAnswer struct {
	pinned string
	err    error
}

func (v *validateCtx) Mode() spec.Mode { return v.mode }

// Credential is spec.CredentialContext.
func (v *validateCtx) Credential(name string) (spec.CredentialInfo, bool) {
	c, ok := v.credentials[name]
	return c, ok
}

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

// IngressHost is the environment's first host and whether it is public;
// ok false when the platform has no apps zone, so that no ingress serves
// any route.
func (v *validateCtx) IngressHost() (host string, public, ok bool) {
	return v.host, v.public, v.zone != ""
}

// RouteHosts are the hosts a route of the environment may name, those it
// is served at now, and whether there is an environment (ADR 0045).
func (v *validateCtx) RouteHosts() (hosts, served []string, ok bool) {
	return v.routeHosts, v.served, v.hostsKnown
}

// HTTPBindings reports whether HTTP bindings are on (ADR 0028).
func (v *validateCtx) HTTPBindings() bool { return v.bindingsOn }

// RefusesStatedBindings reports whether a binding stated in the spec that
// the current deployment does not have unchanged is refused (ADR 0055):
// in every plan but a stored deployment's.
func (v *validateCtx) RefusesStatedBindings() bool { return !v.stored }

// Destination is the destination rule's word on a binding's host.
func (v *validateCtx) Destination(host string) error {
	if err, ok := v.destinations[host]; ok {
		return err
	}
	return nil
}

// DenyPrefixes are the prefixes no pod of the cluster reaches, which no
// egress opens.
func (v *validateCtx) DenyPrefixes() []netip.Prefix { return v.deny }

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
// in (the environment each is in, ADR 0035), the prefixes no pod reaches,
// the environment's secrets (secrets, when not nil, stands in for them:
// a clone's) and credentials (creds likewise), what its addon volumes
// hold, the addon types, the images, and its default host under the apps
// zone. env is nil for a spec without an environment, which has no
// credentials.
func (s *Service) loadContext(ctx context.Context, tenant string, env *store.Environment, sp spec.Spec, mode spec.Mode,
	secrets map[string]spec.SecretInfo, creds map[string]spec.CredentialInfo) (*validateCtx, spec.Pins, error) {
	vc := &validateCtx{mode: mode, sandboxes: map[string]spec.SandboxInfo{}, secrets: secrets,
		volumes: map[string]spec.AddonData{}, blueprint: s.d.Blueprint, zone: s.d.AppsZone,
		bindingsOn: s.d.Bindings != nil && s.d.Bindings.Enabled(), destinations: map[string]error{},
		deny: s.d.DenyPrefixes}
	if vc.bindingsOn {
		for _, host := range bindingHosts(sp) {
			vc.destinations[host] = s.d.Bindings.CheckDestination(ctx, host)
		}
	}
	if env != nil {
		vc.public = env.Public
		zone, err := s.hostZone(ctx, s.st, env.Owner)
		if err != nil {
			return nil, spec.Pins{}, err
		}
		if zone != "" && env.HostLabel != "" {
			vc.host = env.HostLabel + "." + zone
		}
		// A public environment on a domain of its own is served there.
		hosts, err := s.hosts(ctx, *env)
		if err != nil {
			return nil, spec.Pins{}, fmt.Errorf("hosts: %w", err)
		}
		if hostRoutes(sp) {
			if vc.routeHosts, err = s.routeHosts(ctx, *env, vc.host); err != nil {
				return nil, spec.Pins{}, err
			}
			vc.served, vc.hostsKnown = slices.Clone(hosts), true
		}
		if hosts = slices.DeleteFunc(hosts, routing.IsWildcard); len(hosts) > 0 {
			vc.host = hosts[0]
		}
	}
	pins := spec.Pins{Images: map[string]string{}, Secrets: map[string]int{}, Plans: map[string]string{}}
	switch {
	case creds != nil:
		vc.credentials, pins.Credentials = creds, creds
	case env != nil && vc.bindingsOn && (namesCredentials(sp) || referencesSecrets(sp)):
		creds, err := s.envCredentials(ctx, s.st, env.ID)
		if err != nil {
			return nil, pins, err
		}
		vc.credentials, pins.Credentials = creds, creds
	}

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

	for _, svc := range sp.Services {
		if svc.Runtime != spec.RuntimeAttached {
			continue
		}
		id, err := podid.Parse(svc.Sandbox)
		if err != nil || vc.sandboxes[id.String()].ID != "" {
			continue
		}
		sb, err := s.st.GetOwnSandbox(ctx, tenant, id)
		if errors.Is(err, store.ErrNoRow) {
			continue
		}
		if err != nil {
			return nil, pins, fmt.Errorf("sandbox %s: %w", id, err)
		}
		info := spec.SandboxInfo{ID: id.String()}
		// A spec planned without an environment cannot tell.
		if env != nil && sb.EnvID != env.ID {
			info.OtherEnvironment = sb.EnvName
		}
		if info.HTTPNames, err = s.sandboxHTTPNames(ctx, id); err != nil {
			return nil, pins, err
		}
		if vc.bindingsOn {
			if info.Secrets, err = s.st.SandboxCredentialNames(ctx, id); err != nil {
				return nil, pins, fmt.Errorf("the HTTP secrets of sandbox %s: %w", id, err)
			}
		}
		vc.sandboxes[id.String()] = info
	}

	// An environment without a deployment has no addon volumes. A clone
	// plans for one that is not there yet, under an ID that may be
	// another tenant's environment's (its insert refuses that later):
	// what that one's volumes hold is no business of the clone's.
	if env != nil && env.Deployment > 0 && len(sp.Addons) > 0 {
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

// hostRoutes reports whether a route of sp has a host.
func hostRoutes(sp spec.Spec) bool {
	for _, r := range sp.Routes {
		if r.Host != "" {
			return true
		}
	}
	return false
}

// routeHosts are the hosts a route of environment e may name (ADR 0045):
// its default host, defaultHost ("" before it has a label), and the names
// of the domains bound to it, wildcards among them, whatever their state.
// These are the names the environment answers on, or will answer on
// without another deployment, once a domain is verified, routed and has
// its certificate, or once none is served and the default host is its
// host again; what it is served at this moment changes without a write,
// and a spec judged by it would pass or fail by timing.
func (s *Service) routeHosts(ctx context.Context, e store.Environment, defaultHost string) ([]string, error) {
	var out []string
	if defaultHost != "" {
		out = append(out, defaultHost)
	}
	ds, err := s.st.ListDomainViews(ctx, e.Owner, e.ID, nil)
	if err != nil {
		return nil, fmt.Errorf("the domains: %w", err)
	}
	for _, d := range ds {
		if d.EnvID == e.ID && !slices.Contains(out, d.Name) {
			out = append(out, d.Name)
		}
	}
	return out, nil
}

// planned is a plan of a spec for an environment.
type planned struct {
	findings []spec.Finding
	// parsed: the spec could be read (else norm and diff are empty).
	parsed bool
	norm   spec.Spec
	diff   []spec.DiffEntry
	// base is the deployment the diff is against, current the
	// environment's current deployment when the plan read it, currentSpec
	// its spec (nil without one).
	base, current int
	currentSpec   *spec.Spec
	// images are the pins of the spec's images (spec.Pins.Images).
	images map[string]string
	// reach is what the normalised spec admits.
	reach spec.Reachability
}

func (p planned) valid() bool { return p.parsed && !spec.HasErrors(p.findings) }

func (p planned) answer() PlanAnswer {
	reach := p.reach
	if reach.Edges == nil {
		reach = spec.Reachability{Edges: []spec.Edge{}, Egress: []spec.ObjectEgress{}, Routes: []spec.RouteClients{}}
	}
	return PlanAnswer{Valid: p.valid(), Findings: nonNilFindings(p.findings), Diff: nonNilDiff(p.diff), Base: p.base,
		Current: p.current, Images: nonNilImages(p.images), Reachability: reach}
}

// reachability is what spec sp admits (spec.Derive), addons' ports by
// their blueprints.
func (s *Service) reachability(sp spec.Spec) spec.Reachability {
	r, _ := spec.Derive(sp, func(name string, a spec.Addon) int {
		b, ok := s.d.Blueprint(a.Type)
		if !ok {
			return 0
		}
		return specBlueprint{b}.Port(name, a)
	})
	return r
}

// nonNilImages is images, {} for none.
func nonNilImages(images map[string]string) map[string]string {
	if images == nil {
		return map[string]string{}
	}
	return images
}

// plan parses, validates and normalises raw for the environment env and
// diffs it against deployment base (nil: the current one). secrets and
// creds, when not nil, stand in for the environment's secrets and
// credentials. stored: raw is a stored deployment's spec (a revert's, a
// clone's of its source's), whose bindings stated in it are accepted.
func (s *Service) plan(ctx context.Context, tenant string, env store.Environment, raw []byte, base *int, mode spec.Mode,
	secrets map[string]spec.SecretInfo, creds map[string]spec.CredentialInfo, stored bool) (planned, error) {
	p := planned{base: env.Deployment, current: env.Deployment}
	if base != nil {
		if *base < 0 || *base > env.Deployment {
			return planned{}, errNoDeployment()
		}
		p.base = *base
	}
	if env.Deployment > 0 {
		cur, err := s.deploymentSpec(ctx, env, env.Deployment)
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
	vc, pins, err := s.loadContext(ctx, tenant, &env, sp, mode, secrets, creds)
	if err != nil {
		return planned{}, err
	}
	vc.stored = stored
	fs = append(fs, spec.Validate(vc, sp, p.currentSpec)...)
	norm, nfs := spec.Normalise(sp, pins)
	fs = append(fs, nfs...)
	if secrets == nil && vc.bindingsOn && !spec.HasErrors(fs) {
		fs = append(fs, s.bindingValueFindings(ctx, env.ID, norm)...)
	}
	qfs, err := s.quotaFindings(ctx, tenant, env.ID, p.currentSpec, &norm)
	if err != nil {
		return planned{}, err
	}
	fs = append(fs, qfs...)
	baseSpec := p.currentSpec
	if p.base != p.current {
		baseSpec = nil
		if p.base > 0 {
			if baseSpec, err = s.deploymentSpec(ctx, env, p.base); err != nil {
				return planned{}, err
			}
		}
	}
	p.parsed, p.norm, p.images = true, norm, pins.Images
	p.reach = s.reachability(norm)
	p.diff = spec.DiffWith(baseSpec, &norm, s.addonEffect)
	if p.currentSpec != nil && spec.EqualNormalised(norm, *p.currentSpec) {
		fs = append(fs, spec.Finding{Severity: spec.SeverityInfo, Path: "",
			Problem: fmt.Sprintf("nothing changes: the spec equals deployment %d's", env.Deployment), Remedy: ""})
	}
	p.findings = spec.SortFindings(fs)
	return p, nil
}

// applyArgs are what an apply makes a deployment of.
type applyArgs struct {
	raw             []byte
	base            int
	author, message string
	annotations     map[string]string
	origin          string
	originRef       string
}

// errReplan is an apply whose plan is stale: the environment got a new
// deployment between the plan and the transaction, which the request's base
// already names, or a secret version the plan pins was deleted.
var errReplan = errors.New("the current deployment changed since the plan")

// apply plans raw (ModeApply: an image that cannot be resolved is an
// error), refuses error findings, and in one transaction (the tenant FOR
// SHARE, the environment FOR UPDATE) answers the current deployment
// unchanged when the spec equals it and it did not fail, refuses a base
// that is not the current deployment, inserts the next deployment waiting,
// replaces the older waiting and deploying ones and wakes the
// controller.
func (s *Service) apply(ctx context.Context, tenant, ref string, a applyArgs) (ApplyAnswer, error) {
	for range 3 {
		ans, err := s.applyOnce(ctx, tenant, ref, a)
		if !errors.Is(err, errReplan) {
			return ans, err
		}
	}
	return ApplyAnswer{}, errors.New("the environment keeps getting new deployments; try again")
}

func (s *Service) applyOnce(ctx context.Context, tenant, ref string, a applyArgs) (ApplyAnswer, error) {
	env, err := resolveEnv(ctx, s.st, tenant, ref, store.NoLock)
	if err != nil {
		return ApplyAnswer{}, err
	}
	if env.Deleting {
		return ApplyAnswer{}, errDeleting(env.Name)
	}
	p, err := s.plan(ctx, tenant, env, a.raw, nil, spec.ModeApply, nil, nil, a.origin == model.OriginRevert)
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
		t, err := lockQuota(ctx, tx, tenant)
		if err != nil {
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
		if e.Deployment != p.current {
			if a.base == e.Deployment {
				return errReplan
			}
			return &BaseMismatchError{Base: a.base, Current: e.Deployment}
		}
		// The plan read the secrets before the lock: a DeleteSecret, which
		// takes the same lock, may have deleted a version it pins since.
		if err := checkPins(ctx, tx, e.ID, p.norm.Secrets); err != nil {
			return err
		}
		// And the credentials, the HTTP secrets: a DeleteCredential
		// checks its uses under the credential's lock, which this takes
		// until the deployment that pins it is committed.
		if err := checkCredentialPins(ctx, tx, e.ID, p.norm); err != nil {
			return err
		}
		// The plan's quota finding counted before the lock too.
		if err := checkQuota(ctx, tx, t, quota.Use{}, e.ID, p.currentSpec, &p.norm); err != nil {
			return err
		}
		if e.Deployment > 0 && spec.EqualNormalised(p.norm, *p.currentSpec) {
			cur, err := tx.GetDeployment(ctx, e.ID, e.Deployment)
			if err != nil {
				return fmt.Errorf("deployment %d: %w", e.Deployment, err)
			}
			if cur.State != model.DeploymentFailed {
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
				ans = ApplyAnswer{DeploymentInfo: deploymentInfo(cur, names), Unchanged: true, Diff: []spec.DiffEntry{},
					Findings: nonNilFindings(p.findings)}
				return nil
			}
		}
		if a.base != e.Deployment {
			return &BaseMismatchError{Base: a.base, Current: e.Deployment}
		}
		number := e.Deployment + 1
		r, err := tx.InsertDeployment(ctx, store.EnvDeployment{EnvID: e.ID, Number: number, Spec: specJSON, Base: a.base,
			Author: a.author, Message: a.message, Annotations: a.annotations, Origin: a.origin, OriginRef: a.originRef,
			State: model.DeploymentWaiting, MadeBy: auth.MadeBy(ctx)})
		if err != nil {
			return err
		}
		after := e
		after.Deployment = number
		if _, err := tx.UpdateEnvironment(ctx, e, after); err != nil {
			return err
		}
		if _, err := tx.ReplaceDeployments(ctx, e.ID, number, model.DeploymentWaiting, model.DeploymentDeploying); err != nil {
			return err
		}
		if err := wake(ctx, tx, ctl.HintReconcile, ctl.HintGroups); err != nil {
			return err
		}
		ans = ApplyAnswer{DeploymentInfo: deploymentInfo(r, nil), Diff: nonNilDiff(p.diff), Findings: nonNilFindings(p.findings)}
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
		if ref.IsHTTP() {
			// checkCredentialPins'.
			continue
		}
		if !slices.Contains(versions[name].Versions, ref.Version) {
			return errReplan
		}
	}
	return nil
}

// checkCredentialPins takes the lock of each credential a normalised
// spec pins, by its bindings (ADR 0033) or as an HTTP secret its
// variables reference (ADR 0055), in name order, and is errReplan when a
// pinned version is gone.
func checkCredentialPins(ctx context.Context, tx *store.Store, envID uuid.UUID, sp spec.Spec) error {
	pins := map[string][]int{}
	for _, b := range sp.Bindings {
		if ref := b.CredentialRef; ref != nil {
			pins[ref.Name] = append(pins[ref.Name], ref.Version)
		}
	}
	for name, ref := range sp.Secrets {
		if ref.IsHTTP() {
			pins[name] = append(pins[name], ref.Version)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(pins)) {
		if err := tx.LockEnvCredential(ctx, envID, name); err != nil {
			return fmt.Errorf("credential %s: %w", name, err)
		}
		for _, v := range pins[name] {
			_, err := tx.EnvCredential(ctx, envID, name, v)
			if errors.Is(err, model.ErrNotFound) {
				return errReplan
			}
			if err != nil {
				return fmt.Errorf("credential %s: %w", name, err)
			}
		}
	}
	return nil
}

// checkDeploymentMeta refuses an author, a message or annotations that a
// deployment cannot carry.
func checkDeploymentMeta(author, message string, annotations map[string]string) error {
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
// deployment it reverts to.
const annotationRevertOf = "ferun.revertOf"

// revertAnnotations are deployment n's annotations plus ferun.revertOf.
func revertAnnotations(r store.EnvDeployment) map[string]string {
	out := maps.Clone(r.Annotations)
	if out == nil {
		out = map[string]string{}
	}
	out[annotationRevertOf] = strconv.Itoa(r.Number)
	return out
}

// cloneAnnotations are the source deployment's annotations for a clone's
// deployment 1, without ferun.revertOf: its number names a deployment of the
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

// withObject is the current deployment's spec (raw JSON, "" for none) with
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

// quotaFindings are a plan's findings of the tenant's limits (ADR 0031):
// an error when the deployment of next in place of the environment's
// current one would bring its vCPUs or memory past them.
func (s *Service) quotaFindings(ctx context.Context, tenant string, env uuid.UUID, current, next *spec.Spec) (
	[]spec.Finding, error) {
	t, err := s.st.GetTenant(ctx, tenant)
	if errors.Is(err, store.ErrNoRow) || (err == nil && t.Limits.VCPUs == nil && t.Limits.MemoryMiB == nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tenant %s: %w", tenant, err)
	}
	others, err := quota.Of(ctx, s.st, tenant, env)
	if err != nil {
		return nil, err
	}
	return quota.SpecFindings(t.Limits, others, current, next), nil
}

// referencesSecrets reports whether a spec references a secret: an HTTP
// secret it must pin, or a name that may be one (ADR 0055), which a
// value's reference cannot be.
func referencesSecrets(sp spec.Spec) bool {
	return len(spec.Referenced(sp)) > 0 || len(spec.ReferencedHTTP(sp)) > 0
}

// namesCredentials reports whether a spec's bindings name a credential of
// the environment.
func namesCredentials(sp spec.Spec) bool {
	for _, b := range sp.Bindings {
		if b.CredentialRef != nil {
			return true
		}
	}
	return false
}

// envCredentials are an environment's credentials as a spec's bindings
// see them (ADR 0033).
func (s *Service) envCredentials(ctx context.Context, st *store.Store, env uuid.UUID) (
	map[string]spec.CredentialInfo, error) {
	versions, err := st.EnvCredentialVersions(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	out := make(map[string]spec.CredentialInfo, len(versions))
	for name, vs := range versions {
		info := spec.CredentialInfo{Versions: map[int]spec.CredentialTemplate{}}
		for _, v := range vs {
			info.Versions[v.Version] = spec.CredentialTemplate{Upstream: v.Upstream, Mode: v.Mode}
			info.Latest = max(info.Latest, v.Version)
		}
		out[name] = info
	}
	return out, nil
}
