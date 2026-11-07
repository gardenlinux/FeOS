// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// Mode is what a validation is for: it decides which checks run and how
// hard an unresolvable image is.
type Mode string

const (
	// ModeSpec is POST /api/v1/environments/plan: no environment, so no
	// check that needs one (secrets, the current deployment); the tenant's
	// sandboxes are checked.
	ModeSpec Mode = "spec"
	// ModePlan is a plan of an environment: an image that cannot be
	// resolved is a warning.
	ModePlan Mode = "plan"
	// ModeApply is an apply (a deployment, a revert, an object put or
	// delete, a clone): an image that cannot be resolved is an error.
	ModeApply Mode = "apply"
)

// ValidateContext is what Validate looks up beyond the spec. It answers
// from memory: the caller reads what the parsed spec names (the
// sandboxes, the secrets, the images' resolution, which runs outside any
// transaction) before it validates, so that Validate makes no database,
// node or registry call and needs no context.
//
// A ValidateContext may also implement interface{ BlueprintTypes()
// []string }: the registered addon types, which a finding about an
// unknown type then lists.
//
// It may also implement interface{ IngressHost() (host string, public,
// ok bool) }: the environment's default host under the platform's apps
// zone ("" for a spec without an environment, or for an environment never
// published), whether the environment is public, and ok false when the
// platform has no apps zone, so that no ingress serves any route. With ok,
// each route whose target exists gets an info finding that says which
// doors serve it (ADR 0021): the route door alone for an attached
// service; the ingress and the route door for an image service of a
// public environment.
//
// It may also implement interface{ HTTPBindings() bool }: whether the
// platform has HTTP bindings on (ADR 0028); a spec with bindings is an
// error when it has not. Without it, they are on.
//
// It may also implement interface{ Destination(host string) error }: the
// destination rule's word on the host of a binding's upstream or token
// endpoint, which the caller asked before it validates (nil: a
// destination); an error is a finding. Without it, no host is judged.
//
// It may also implement interface{ DenyPrefixes() []netip.Prefix }: the
// prefixes no pod of the cluster reaches (--deny-prefix, ADR 0010), which
// no rule of a security group opens: a block inside one is an error.
// Without it, no block is judged.
//
// It may also implement interface{ GroupUsers() map[string][]string }:
// the environment's pods outside its spec that name its security groups
// (ADR 0035), by group: "sandbox <id>" for its sandboxes, "job <id>" for
// its tenants' jobs that are queued or running. A spec without one of
// those groups is an error. Without it (and in ModeSpec), none.
type ValidateContext interface {
	// Mode is what the validation is for.
	Mode() Mode
	// Sandbox reports the tenant's sandbox of id (its canonical form: a
	// short ID, or a UUID in lowercase); ok false when the tenant has no sandbox of that ID,
	// or it is an environment's instance row.
	Sandbox(id string) (info SandboxInfo, ok bool)
	// Secret reports a tenant secret of the environment (never a system
	// secret "addon.…"); ok false when the environment never had a version
	// of it. A secret whose versions are all deleted is ok with Latest 0.
	// Not asked in ModeSpec.
	Secret(name string) (info SecretInfo, ok bool)
	// Volume reports the data the environment's volume of addon name
	// holds (its volume that is not being deleted); ok false when it has
	// none. The controller keeps an addon's volume by the addon's name and
	// mounts it whatever the latest deployment says, so an addon is validated
	// against it rather than against the latest deployment's addon. Not asked
	// in ModeSpec.
	Volume(addon string) (data AddonData, ok bool)
	// Blueprint returns the addon type typ; ok false when no blueprint of
	// that type is registered.
	Blueprint(typ string) (b Blueprint, ok bool)
	// Image returns the image pinned to its digest
	// ("<registry>/<repo>@sha256:…"), or why it could not be resolved
	// (err non-nil). An image that already is a digest reference resolves
	// to itself.
	Image(image string) (pinned string, err error)
}

// SandboxInfo is a sandbox an attached service may run in: one of the
// environment's own (ADR 0035).
type SandboxInfo struct {
	// ID is the sandbox's ID in its canonical form (podid).
	ID string
	// OtherEnvironment is the name of the environment the sandbox is in
	// when that is not the environment validated; "": it is in this one
	// (or, in ModeSpec, wherever it is: there is no environment to
	// compare with).
	OtherEnvironment string
	// SecurityGroups are the groups of the environment the sandbox names
	// (ADR 0035), which its attached services are in; none:
	// [DefaultSecurityGroup].
	SecurityGroups []string
	// HTTPNames are the names the sandbox's own HTTP bindings (ADR 0028)
	// make its resolver answer, which no name of an environment there may
	// be.
	HTTPNames []string
}

// AddonData is what an addon's volume holds: data of addon type Type,
// written last by Version ("": not recorded yet).
type AddonData struct {
	Type, Version string
}

// SecretInfo is a tenant secret of an environment.
// CredentialInfo is a credential of the tenant (ADR 0033) as a binding
// that names it sees it: its versions with what makes a pod another.
type CredentialInfo struct {
	// Latest is its newest version.
	Latest int
	// Versions are its versions' upstreams and modes, by number.
	Versions map[int]CredentialTemplate
}

// CredentialTemplate is a credential's version's upstream (canonical)
// and mode.
type CredentialTemplate struct {
	Upstream string
	Mode     string
}

// CredentialContext is what a ValidateContext implements when the
// tenant's credentials can be named: the credential of that name; ok
// false when the tenant has none.
type CredentialContext interface {
	TenantCredential(name string) (info CredentialInfo, ok bool)
}

type SecretInfo struct {
	// Latest is its newest version that is not deleted; 0: every version
	// is deleted.
	Latest int
	// Versions are its versions that are not deleted, ascending.
	Versions []int
	// Deleted are its deleted versions, ascending.
	Deleted []int
}

// Blueprint is what Validate asks of an addon type. The environment
// service adapts an internal/env/addons blueprint to it (addons imports
// this package for Finding, so this package cannot import addons).
type Blueprint interface {
	// Validate checks addon name of this type against what it replaces
	// (prev nil: nothing, or another type): the data its volume holds,
	// else the same addon of the current deployment. Its version, plan and
	// options. Paths are relative to the addon: "version",
	// "options.roles[1]"; Validate prefixes them with "addons.<name>.".
	Validate(name string, prev *Addon, next Addon) []Finding
	// Ref checks a reference {"addon": name, "key": key, …fields} to the
	// addon a: nil when it resolves, else why not ("no key x; the keys
	// are url, host, …").
	Ref(name string, a Addon, key string, fields map[string]string) error
	// Port is the port addon name of this type serves on; 0: unknown. The
	// security groups' reach check (ADR 0034) asks it.
	Port(name string, a Addon) int
}

// The rules of names.
var (
	// dnsLabel is an object's name: services, jobs, addons, routes, ports.
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// secretName is a tenant secret's name (env_secrets' CHECK; "addon."
	// is the system secrets').
	secretName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	// envVarName is an environment variable's name.
	envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// routePathChars are the characters a route's path may have: those of
	// a path that needs no escaping.
	routePathChars = regexp.MustCompile(`^[A-Za-z0-9._~!$&'()*+,;=:@/-]*$`)
)

// ValidEnvName reports whether name is an environment variable's name, as
// a spec's env takes them (a tenant's job's env and secrets too).
func ValidEnvName(name string) bool { return envVarName.MatchString(name) }

// SystemSecretPrefix begins the names of the addons' credentials, which
// tenants cannot use.
const SystemSecretPrefix = "addon."

// IsDigestRef reports whether an image reference names its image by a
// sha256 digest, which pins it already.
func IsDigestRef(image string) bool { return strings.Contains(image, "@sha256:") }

// IsSecretName reports whether name is a tenant secret's name.
func IsSecretName(name string) bool {
	return secretName.MatchString(name) && !strings.HasPrefix(name, SystemSecretPrefix)
}

// IsName reports whether name is an object's name: a DNS label.
func IsName(name string) bool { return dnsLabel.MatchString(name) }

// Validate checks a parsed spec against the rules of the contract's §3
// and returns every finding, ordered by path. prev is the environment's
// current deployment's spec (nil: none, and always nil in ModeSpec): an
// addon is validated against its previous self (a major version change
// is an error). Findings about the spec as written; Normalise adds its
// own (dropped secrets).
func Validate(vc ValidateContext, s Spec, prev *Spec) []Finding {
	v := &validator{vc: vc, mode: vc.Mode(), s: s, prev: prev, routed: map[string]bool{}, referenced: map[string]bool{}}
	if v.mode == ModeSpec {
		v.prev = nil
	}
	v.limits()
	for _, r := range s.Routes {
		v.routed[r.Service] = true
	}
	for _, name := range slices.Sorted(maps.Keys(s.Services)) {
		v.service(name, s.Services[name])
	}
	for _, name := range slices.Sorted(maps.Keys(s.Jobs)) {
		v.job(name, s.Jobs[name])
	}
	for _, name := range slices.Sorted(maps.Keys(s.Addons)) {
		v.addon(name, s.Addons[name])
	}
	for _, name := range slices.Sorted(maps.Keys(s.Routes)) {
		v.route(name, s.Routes[name])
	}
	v.bindings()
	v.routePaths()
	v.secrets()
	v.attachedPorts()
	v.sandboxNames()
	v.securityGroups()
	v.groupUsers()
	v.reach()
	return SortFindings(v.out)
}

// validator is one Validate.
type validator struct {
	vc   ValidateContext
	mode Mode
	s    Spec
	prev *Spec
	// routed are the services a route targets.
	routed map[string]bool
	// referenced are the secrets an env value references.
	referenced map[string]bool
	out        []Finding
}

func (v *validator) add(sev Severity, path, remedy, format string, args ...any) {
	v.out = append(v.out, Finding{Severity: sev, Path: path, Problem: fmt.Sprintf(format, args...), Remedy: remedy})
}

func (v *validator) errorf(path, remedy, format string, args ...any) {
	v.add(SeverityError, path, remedy, format, args...)
}

func (v *validator) warnf(path, remedy, format string, args ...any) {
	v.add(SeverityWarning, path, remedy, format, args...)
}

func (v *validator) limits() {
	for _, l := range []struct {
		kind string
		n    int
		max  int
	}{
		{"services", len(v.s.Services), MaxServices}, {"jobs", len(v.s.Jobs), MaxJobs},
		{"addons", len(v.s.Addons), MaxAddons}, {"routes", len(v.s.Routes), MaxRoutes},
		{"bindings", len(v.s.Bindings), MaxBindings}, {"securityGroups", len(GroupNames(v.s)), MaxSecurityGroups},
	} {
		if l.n > l.max {
			v.errorf(l.kind, "split the environment", "%d %s; at most %d are allowed", l.n, l.kind, l.max)
		}
	}
	if v.s.Volumes != nil {
		v.errorf("volumes", "keep the data in an addon (postgres, valkey, nats), which has a volume of its own",
			"volumes for services are not available yet")
	}
	for _, kind := range []struct {
		name  string
		names []string
	}{
		{"services", slices.Collect(maps.Keys(v.s.Services))}, {"jobs", slices.Collect(maps.Keys(v.s.Jobs))},
		{"addons", slices.Collect(maps.Keys(v.s.Addons))}, {"routes", slices.Collect(maps.Keys(v.s.Routes))},
		{"bindings", slices.Collect(maps.Keys(v.s.Bindings))},
	} {
		for _, n := range kind.names {
			if !IsName(n) {
				v.errorf(kind.name+"."+n, "use lowercase letters, digits and '-', at most 63, beginning and ending with a "+
					"letter or a digit", "%q is not a DNS label", n)
			}
		}
	}
	for n := range v.s.Services {
		if _, ok := v.s.Addons[n]; ok {
			v.errorf("services."+n, "rename one", "a service and an addon are both named %s: services and addons share "+
				"the environment's names", n)
		}
	}
}

// image checks that an image resolves: in ModeApply an error, else a
// warning.
func (v *validator) image(path, image string) {
	if _, err := v.vc.Image(image); err != nil {
		if v.mode == ModeApply {
			v.errorf(path, "push the image or fix its name", "image %s could not be resolved: %v", image, err)
		} else {
			v.warnf(path, "push the image or fix its name; an apply refuses an image it cannot resolve",
				"image %s could not be resolved: %v", image, err)
		}
	}
}

func (v *validator) service(name string, svc Service) {
	p := "services." + name
	runtime := svc.Runtime
	if runtime == "" {
		runtime = RuntimeImage
	}
	switch runtime {
	case RuntimeImage:
		if svc.Image == "" {
			v.errorf(p+".image", `set image, or set runtime "attached" with a sandbox and a command to run it in a sandbox`,
				"an image service needs an image")
		} else {
			v.image(p+".image", svc.Image)
		}
		if svc.Sandbox != "" {
			v.errorf(p+".sandbox", `remove sandbox, or set runtime "attached"`, "sandbox is for attached services")
		}
		if svc.Cwd != "" {
			v.errorf(p+".cwd", `remove cwd, or set runtime "attached"`, "cwd is for attached services")
		}
		if svc.Replicas != nil && (*svc.Replicas < 0 || *svc.Replicas > MaxReplicas) {
			v.errorf(p+".replicas", fmt.Sprintf("use 0..%d", MaxReplicas), "replicas %d is out of range", *svc.Replicas)
		}
		v.resources(p+".resources", svc.Resources)
		v.imageCredentials(p+".imageCredentials", svc.ImageCredentials)
		v.groupRefs(p+".securityGroups", svc.SecurityGroups)
		if svc.Idle != "" {
			d, ok := v.duration(p+".idle", svc.Idle)
			switch {
			case !ok:
			case d < time.Minute:
				v.errorf(p+".idle", "use 1m or more, or remove idle", "idle %s is shorter than 1m", svc.Idle)
			case !v.routed[name]:
				v.errorf(p+".idle", "add a route to it, or remove idle", "nothing would wake it: no route targets %s", name)
			}
		}
		if svc.Rollout != nil && svc.Rollout.Timeout != "" {
			if d, ok := v.duration(p+".rollout.timeout", svc.Rollout.Timeout); ok && d <= 0 {
				v.errorf(p+".rollout.timeout", "use a positive duration, or leave it out for 10m",
					"timeout %s is not positive", svc.Rollout.Timeout)
			}
		}
	case RuntimeAttached:
		if svc.Image != "" || svc.ImageAsWritten != "" {
			v.errorf(p+".image", `remove image, or set runtime "image"`, "an attached service runs no image")
		}
		if svc.ImageCredentials != nil {
			v.errorf(p+".imageCredentials", `remove imageCredentials, or set runtime "image"`,
				"an attached service pulls no image")
		}
		v.sandbox(p+".sandbox", svc.Sandbox)
		if svc.SecurityGroups != nil {
			v.errorf(p+".securityGroups", "remove securityGroups; change the sandbox's groups (PUT /api/v1/sandboxes/"+
				"{id}/security-groups) instead", "an attached service is in its sandbox's groups (%s)",
				strings.Join(v.sandboxGroups(svc.Sandbox), ", "))
		}
		if len(svc.Command) == 0 {
			v.errorf(p+".command", "set command to the process to run in the sandbox", "an attached service needs a command")
		}
		if svc.Replicas != nil && *svc.Replicas != 1 {
			v.errorf(p+".replicas", "remove replicas, or run the service as an image", "an attached service runs once")
		}
		if svc.Resources != nil {
			v.errorf(p+".resources", "remove resources, or run the service as an image",
				"an attached service has its sandbox's resources")
		}
		if svc.Idle != "" && svc.Idle != IdleNever {
			// The sandbox's own activity wakes it, and so does a route.
			if d, ok := v.duration(p+".idle", svc.Idle); ok && d < time.Minute {
				v.errorf(p+".idle", `use 1m or more, "never", or remove idle`, "idle %s is shorter than 1m", svc.Idle)
			}
		}
		if svc.Rollout != nil {
			v.errorf(p+".rollout", "remove rollout, or run the service as an image", "an attached service has no rollout")
		}
		if svc.Cwd != "" && !strings.HasPrefix(svc.Cwd, "/") {
			v.errorf(p+".cwd", "write the directory's absolute path", "cwd %q is not an absolute path", svc.Cwd)
		}
	default:
		v.errorf(p+".runtime", `use "image" or "attached"`, "runtime %q is unknown", svc.Runtime)
		return
	}
	v.ports(p+".ports", svc.Ports)
	v.readiness(p+".readiness", svc.Readiness, svc.Ports)
	if svc.Mounts != nil {
		v.errorf(p+".mounts", "keep the data in an addon (postgres, valkey, nats), which has a volume of its own",
			"volumes for services are not available yet")
	}
	v.env(p+".env", svc.Env)
	if runtime == RuntimeAttached && len(svc.Bindings) > 0 {
		v.errorf(p+".bindings", "remove bindings: set the sandbox's HTTP bindings (PUT /api/v1/sandboxes/{id}/http), or "+
			"run the service as an image", "attached services use their sandbox's bindings")
	} else {
		v.bindingRefs(p+".bindings", svc.Bindings)
	}
}

// sandbox checks an attached service's sandbox: one of the environment's
// own (ADR 0035).
func (v *validator) sandbox(path, id string) {
	if id == "" {
		v.errorf(path, "set sandbox to the ID of a sandbox of the environment", "an attached service needs a sandbox")
		return
	}
	sid, err := podid.Parse(id)
	if err != nil {
		v.errorf(path, "set sandbox to the ID of a sandbox of the environment", "sandbox %q is not a sandbox ID", id)
		return
	}
	info, ok := v.vc.Sandbox(sid.String())
	if !ok {
		v.errorf(path, "create the sandbox in this environment first (POST /api/v1/sandboxes with its environment), "+
			"or name one of its sandboxes", "no sandbox %s of yours", id)
		return
	}
	if info.OtherEnvironment != "" {
		v.errorf(path, "name a sandbox of this environment, or create one in it (POST /api/v1/sandboxes with its "+
			"environment)", "sandbox %s is in environment %s: an attached service runs in a sandbox of its own "+
			"environment", id, info.OtherEnvironment)
	}
}

func (v *validator) resources(path string, r *Resources) {
	if r == nil {
		return
	}
	if r.VCPUs != 0 && (r.VCPUs < MinVCPUs || r.VCPUs > MaxVCPUs) {
		v.errorf(path+".vcpus", fmt.Sprintf("use %d..%d", MinVCPUs, MaxVCPUs), "vcpus %d is out of range", r.VCPUs)
	}
	if r.MemoryMib != 0 && (r.MemoryMib < MinMemoryMib || r.MemoryMib > MaxMemoryMib) {
		v.errorf(path+".memoryMib", fmt.Sprintf("use %d..%d", MinMemoryMib, MaxMemoryMib), "memoryMib %d is out of range",
			r.MemoryMib)
	}
}

// duration parses a duration, reporting one that is none.
func (v *validator) duration(path, s string) (time.Duration, bool) {
	d, err := time.ParseDuration(s)
	if err != nil {
		v.errorf(path, "write it as Go does: 90s, 15m, 1h30m", "%q is not a duration", s)
		return 0, false
	}
	return d, true
}

func (v *validator) ports(path string, ports []Port) {
	names := map[string]bool{}
	numbers := map[int]bool{}
	for i, pt := range ports {
		pp := index(path, i)
		switch {
		case pt.Name == "":
			v.errorf(pp+".name", "name the port (http, grpc, …): references and routes use it", "a port needs a name")
		case !IsName(pt.Name):
			v.errorf(pp+".name", "use lowercase letters, digits and '-'", "port name %q is not a DNS label", pt.Name)
		case names[pt.Name]:
			v.errorf(pp+".name", "give each port its own name", "port name %s is given twice", pt.Name)
		}
		names[pt.Name] = true
		switch {
		case pt.Port < 1 || pt.Port > 65535:
			v.errorf(pp+".port", "use 1..65535", "port %d is out of range", pt.Port)
		case numbers[pt.Port]:
			v.errorf(pp+".port", "list each port once", "port %d is given twice", pt.Port)
		}
		numbers[pt.Port] = true
	}
}

// PortNumber resolves a port reference among ports: by name, or by
// number when one of them has it.
func PortNumber(ports []Port, ref PortRef) (int, bool) {
	for _, p := range ports {
		if (ref.Name != "" && p.Name == ref.Name) || (ref.Name == "" && ref.Number != 0 && p.Port == ref.Number) {
			return p.Port, true
		}
	}
	return 0, false
}

// portNames lists ports for a remedy.
func portNames(ports []Port) string {
	if len(ports) == 0 {
		return "it has none: declare one under ports"
	}
	var out []string
	for _, p := range ports {
		out = append(out, p.Name+" ("+strconv.Itoa(p.Port)+")")
	}
	return "name one of its ports: " + strings.Join(out, ", ")
}

// portRef checks a reference to one of ports.
func (v *validator) portRef(path string, ref PortRef, ports []Port, owner string) {
	if ref.IsZero() {
		v.errorf(path, portNames(ports), "a port is needed")
		return
	}
	if _, ok := PortNumber(ports, ref); !ok {
		v.errorf(path, portNames(ports), "%s has no port %s", owner, ref)
	}
}

func (v *validator) readiness(path string, r *Readiness, ports []Port) {
	if r == nil {
		return
	}
	n := 0
	if r.HTTP != nil {
		n++
		v.portRef(path+".http.port", r.HTTP.Port, ports, "the service")
		if !strings.HasPrefix(r.HTTP.Path, "/") {
			v.errorf(path+".http.path", "write the path beginning with /", "path %q does not begin with /", r.HTTP.Path)
		}
	}
	if r.TCP != nil {
		n++
		v.portRef(path+".tcp.port", r.TCP.Port, ports, "the service")
	}
	if r.Exec != nil {
		n++
		if len(r.Exec.Command) == 0 {
			v.errorf(path+".exec.command", "set the command to run", "an exec check needs a command")
		}
	}
	if n != 1 {
		v.errorf(path, "give exactly one of http, tcp and exec, or remove readiness",
			"readiness needs exactly one of http, tcp and exec, not %d", n)
	}
	if r.InitialDelay != "" {
		if d, ok := v.duration(path+".initialDelay", r.InitialDelay); ok && d < 0 {
			v.errorf(path+".initialDelay", "use 0s or more", "initialDelay %s is negative", r.InitialDelay)
		}
	}
	if r.Period != "" {
		if d, ok := v.duration(path+".period", r.Period); ok && d < time.Second {
			v.errorf(path+".period", "use 1s or more", "period %s is shorter than 1s", r.Period)
		}
	}
	if r.FailureThreshold < 0 {
		v.errorf(path+".failureThreshold", "use 1 or more, or leave it out for 3", "failureThreshold %d is negative",
			r.FailureThreshold)
	}
}

// env checks environment variables. An attached service is reached like
// any other pod of the environment (ADR 0035: its name is in the
// environment's name table with its sandbox's address), where a rule of
// its sandbox's groups lets the caller in (reach).
func (v *validator) env(path string, env map[string]EnvValue) {
	for _, key := range slices.Sorted(maps.Keys(env)) {
		val := env[key]
		kp := path + "." + key
		if !envVarName.MatchString(key) {
			v.errorf(kp, "use letters, digits and '_', not beginning with a digit",
				"%q is not an environment variable's name", key)
		}
		switch val.Kind {
		case EnvSecret:
			v.secretRef(kp, val.Secret)
		case EnvAddon:
			a, ok := v.s.Addons[val.Addon]
			if !ok {
				v.errorf(kp, "declare the addon under addons, or reference one that is declared", "no addon %s", val.Addon)
				continue
			}
			if b, ok := v.vc.Blueprint(a.Type); ok {
				if err := b.Ref(val.Addon, a, val.Key, val.Fields); err != nil {
					v.errorf(kp, "fix the reference: the addon type's keys and fields are in its documentation", "%v", err)
				}
			}
		case EnvService:
			target, ok := v.s.Services[val.Service]
			if !ok {
				v.errorf(kp, "declare the service under services, or reference one that is declared", "no service %s",
					val.Service)
				continue
			}
			switch val.Key {
			case ServiceKeyURL, ServiceKeyPort:
				v.portRef(kp, val.Port, target.Ports, val.Service)
			case ServiceKeyHost:
				if !val.Port.IsZero() {
					v.portRef(kp, val.Port, target.Ports, val.Service)
				}
			default:
				v.errorf(kp, "use key url, host or port", "a service reference's key is url, host or port, not %q", val.Key)
			}
		}
	}
}

// imageCredentials checks a login for an image's registry: a username
// and a password, each a string or a secret.
func (v *validator) imageCredentials(path string, c *ImageCredentials) {
	if c == nil {
		return
	}
	for _, f := range []struct {
		name string
		val  EnvValue
	}{{"username", c.Username}, {"password", c.Password}} {
		fp := path + "." + f.name
		switch f.val.Kind {
		case EnvSecret:
			v.secretRef(fp, f.val.Secret)
		case EnvString, EnvLiteral:
			if f.val.Value == "" {
				v.errorf(fp, `set it to a string or {"secret": "<name>"}`, "the image's %s is empty", f.name)
			}
		default:
			v.errorf(fp, `use a string or {"secret": "<name>"}`, "the image's %s is a string or a secret", f.name)
		}
	}
}

// secretRef checks a reference to a tenant secret.
func (v *validator) secretRef(path, name string) {
	if !IsSecretName(name) {
		v.errorf(path, "secret names are lowercase letters, digits, '.', '_' and '-'; \"addon.\" is reserved",
			"%q is not a secret name you can use", name)
		return
	}
	v.referenced[name] = true
	if v.mode == ModeSpec {
		return
	}
	info, ok := v.vc.Secret(name)
	if !ok {
		v.errorf(path, "set it with PUT /api/v1/environments/{env}/secrets/"+name+", then plan again", "no secret %s", name)
		return
	}
	if v.s.Secrets[name].Version == 0 && info.Latest == 0 {
		v.errorf(path, "set it again, or remove the reference", "secret %s was deleted", name)
	}
}

// secrets checks the secrets' pinned versions (of those referenced; the
// others Normalise drops).
func (v *validator) secrets() {
	for _, name := range slices.Sorted(maps.Keys(v.s.Secrets)) {
		p := "secrets." + name
		ref := v.s.Secrets[name]
		if !IsSecretName(name) {
			v.errorf(p, "secret names are lowercase letters, digits, '.', '_' and '-'; \"addon.\" is reserved",
				"%q is not a secret name you can use", name)
			continue
		}
		if ref.Version < 0 {
			v.errorf(p+".version", "pin a version, or leave it out to pin the latest", "version %d is negative", ref.Version)
			continue
		}
		if !v.referenced[name] || v.mode == ModeSpec || ref.Version == 0 {
			continue
		}
		info, ok := v.vc.Secret(name)
		switch {
		case !ok:
			// The reference's finding says so.
		case slices.Contains(info.Versions, ref.Version):
		case slices.Contains(info.Deleted, ref.Version):
			v.errorf(p, "apply a spec that pins an existing version instead of reverting",
				"version %d of secret %s was deleted", ref.Version, name)
		default:
			v.errorf(p, fmt.Sprintf("pin an existing version (the latest is %d), or leave it out to pin the latest",
				info.Latest), "secret %s has no version %d", name, ref.Version)
		}
	}
}

func (v *validator) job(name string, j Job) {
	p := "jobs." + name
	if j.Image == "" {
		v.errorf(p+".image", "set the image the job runs", "a job needs an image")
	} else {
		v.image(p+".image", j.Image)
	}
	switch j.On {
	case "", JobOnDeploy, JobOnRelease, JobOnManual:
	default:
		v.errorf(p+".on", `use "deploy" or "manual"`, "on %q is unknown", j.On)
	}
	if j.Retries < 0 || j.Retries > MaxRetries {
		v.errorf(p+".retries", fmt.Sprintf("use 0..%d", MaxRetries), "retries %d is out of range", j.Retries)
	}
	if j.Timeout != "" {
		if d, ok := v.duration(p+".timeout", j.Timeout); ok {
			if limit, _ := time.ParseDuration(MaxJobTimeout); d <= 0 || d > limit {
				v.errorf(p+".timeout", "use a positive duration of at most 2h", "timeout %s is out of range", j.Timeout)
			}
		}
	}
	v.resources(p+".resources", j.Resources)
	v.imageCredentials(p+".imageCredentials", j.ImageCredentials)
	v.env(p+".env", j.Env)
	v.groupRefs(p+".securityGroups", j.SecurityGroups)
	v.bindingRefs(p+".bindings", j.Bindings)
}

func (v *validator) addon(name string, a Addon) {
	p := "addons." + name
	if a.Type == "" {
		v.errorf(p+".type", v.typesRemedy(), "an addon needs a type")
		return
	}
	v.groupRefs(p+".securityGroups", a.SecurityGroups)
	b, ok := v.vc.Blueprint(a.Type)
	if !ok {
		v.errorf(p+".type", v.typesRemedy(), "no addon type %q", a.Type)
		return
	}
	for _, f := range b.Validate(name, v.prevAddon(name, a), a) {
		if f.Path != "" {
			f.Path = p + "." + f.Path
		} else {
			f.Path = p
		}
		v.out = append(v.out, f)
	}
}

// prevAddon is what addon name of type a.Type is validated against: the
// data its volume holds when the environment has one (its type, the
// version that wrote it, the latest deployment's plan and options when that
// deployment has the addon of that type), else the latest deployment's addon;
// nil when there is neither, or either is of another type, which deletes
// the data (a warning says so).
func (v *validator) prevAddon(name string, a Addon) *Addon {
	var latest *Addon
	if v.prev != nil {
		if pa, ok := v.prev.Addons[name]; ok {
			latest = &pa
		}
	}
	p := "addons." + name
	if data, ok := v.volume(name); ok {
		if data.Type != a.Type {
			v.warnf(p+".type", "keep the type, or accept that the addon starts empty",
				"addon %s's volume holds %s data, not %s: its data is deleted", name, data.Type, a.Type)
			return nil
		}
		prev := Addon{Type: data.Type, Version: data.Version, Plan: a.Plan}
		if latest != nil && latest.Type == data.Type {
			prev.Plan, prev.Options = latest.Plan, latest.Options
			if prev.Version == "" {
				prev.Version = latest.Version
			}
		}
		return &prev
	}
	if latest == nil {
		return nil
	}
	if latest.Type != a.Type {
		v.warnf(p+".type", "keep the type, or accept that the addon starts empty",
			"addon %s changes its type from %s to %s: its data is deleted", name, latest.Type, a.Type)
		return nil
	}
	return latest
}

// volume is the data of addon name's volume, outside ModeSpec.
func (v *validator) volume(name string) (AddonData, bool) {
	if v.mode == ModeSpec {
		return AddonData{}, false
	}
	data, ok := v.vc.Volume(name)
	if !ok || data.Type == "" {
		return AddonData{}, false
	}
	return data, true
}

func (v *validator) typesRemedy() string {
	if t, ok := v.vc.(interface{ BlueprintTypes() []string }); ok {
		if types := t.BlueprintTypes(); len(types) > 0 {
			return "use one of: " + strings.Join(types, ", ")
		}
	}
	return "use a registered addon type"
}

func (v *validator) route(name string, r Route) {
	p := "routes." + name
	switch {
	case len(r.Path) > MaxRoutePathBytes:
		v.errorf(p+".path", fmt.Sprintf("shorten the path to at most %d bytes", MaxRoutePathBytes),
			"the path has %d bytes; at most %d are allowed", len(r.Path), MaxRoutePathBytes)
	case !strings.HasPrefix(r.Path, "/"):
		v.errorf(p+".path", "write the path beginning with /", "path %q does not begin with /", r.Path)
	case !routePathChars.MatchString(r.Path):
		v.errorf(p+".path", "use letters, digits and - . _ ~ in the path's segments", "path %q has a character "+
			"that needs escaping, a query or a fragment", r.Path)
	case badSegments(r.Path):
		v.errorf(p+".path", "remove the empty, . and .. segments", "path %q has an empty, . or .. segment", r.Path)
	}
	target, ok := v.s.Services[r.Service]
	if !ok {
		v.errorf(p+".service", "route to a service that is declared under services", "no service %q", r.Service)
	} else {
		v.portRef(p+".port", r.Port, target.Ports, r.Service)
		if len(r.Path) <= MaxRoutePathBytes {
			v.doors(p, r, target)
		}
	}
	if r.Host != "" {
		v.add(SeverityInfo, p+".host", "", "host is stored, not used yet: the ingress and the route door "+
			"serve the route on every host of the environment")
	}
}

// doors says which doors serve route r to target, at path p, while the
// platform has an apps zone (the context's IngressHost): nothing without
// one, where the route door serves every route.
func (v *validator) doors(p string, r Route, target Service) {
	ih, is := v.vc.(interface{ IngressHost() (string, bool, bool) })
	if !is {
		return
	}
	host, public, ok := ih.IngressHost()
	switch {
	case !ok:
	case target.Runtime == RuntimeAttached:
		v.add(SeverityInfo, p, "", "served by the route door alone: an attached service runs in a sandbox, "+
			"which the ingress does not reach")
	case public && host != "":
		v.add(SeverityInfo, p, "", "served by the ingress at https://%s%s and by the route door", host,
			RoutePrefix(r.Path))
	case public:
		v.add(SeverityInfo, p, "", "served by the ingress and by the route door")
	default:
		v.add(SeverityInfo, p, "", "served by the route door; the ingress serves it once the environment is public")
	}
}

// badSegments reports a route path with an empty segment (but the
// trailing slash), "." or "..".
func badSegments(path string) bool {
	trimmed := strings.TrimSuffix(path[1:], "/")
	if trimmed == "" {
		return path != "/"
	}
	for seg := range strings.SplitSeq(trimmed, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// RoutePrefix is a route path as the route door matches it: without a
// trailing slash, "/" for the root.
func RoutePrefix(path string) string {
	if path == "/" {
		return path
	}
	return strings.TrimSuffix(path, "/")
}

// routePaths refuses two routes of one path.
func (v *validator) routePaths() {
	seen := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(v.s.Routes)) {
		path := v.s.Routes[name].Path
		if !strings.HasPrefix(path, "/") || len(path) > MaxRoutePathBytes {
			continue
		}
		key := RoutePrefix(path)
		if other, ok := seen[key]; ok {
			v.errorf("routes."+name+".path", "give each route its own path", "path %s is route %s's too", path, other)
			continue
		}
		seen[key] = name
	}
}

// attachedPorts refuses two attached services in one sandbox on one port
// number. A sandbox is in one environment (ADR 0035), so this spec's are
// all there are.
func (v *validator) attachedPorts() {
	bySandbox := map[string]map[int]string{}
	for _, name := range slices.Sorted(maps.Keys(v.s.Services)) {
		svc := v.s.Services[name]
		if svc.Runtime != RuntimeAttached || svc.Sandbox == "" {
			continue
		}
		sb := strings.ToLower(svc.Sandbox)
		used := bySandbox[sb]
		if used == nil {
			used = map[int]string{}
			bySandbox[sb] = used
		}
		for i, pt := range svc.Ports {
			pp := index("services."+name+".ports", i) + ".port"
			if other, ok := used[pt.Port]; ok && other != name {
				v.errorf(pp, "change one's port", "port %d is attached service %s's too, in the same sandbox", pt.Port, other)
				continue
			}
			used[pt.Port] = name
		}
	}
}

// sandboxNames refuses a name the environment gives a sandbox its
// attached services run in that one of the sandbox's own HTTP bindings
// has: its resolver would answer the binding (a warning in ModeSpec).
func (v *validator) sandboxNames() {
	sandboxes := map[string]bool{}
	for _, svc := range v.s.Services {
		if svc.Runtime == RuntimeAttached && svc.Sandbox != "" {
			sandboxes[strings.ToLower(svc.Sandbox)] = true
		}
	}
	sev := SeverityError
	if v.mode == ModeSpec {
		sev = SeverityWarning
	}
	for _, sb := range slices.Sorted(maps.Keys(sandboxes)) {
		info, ok := v.vc.Sandbox(sb)
		if !ok {
			continue
		}
		for _, name := range SandboxNames(v.s, sb) {
			if !slices.Contains(info.HTTPNames, name) {
				continue
			}
			p := "services." + name
			if _, ok := v.s.Addons[name]; ok {
				p = "addons." + name
			}
			v.add(sev, p, "rename it, or rename the sandbox's HTTP binding",
				"%s is the name of an HTTP binding of sandbox %s: its resolver would answer the binding", name, sb)
		}
	}
}

// SandboxNames are the names the environment of spec s gives a sandbox
// (its ID in lowercase) one of its attached services runs in: its addons,
// its image services and its attached services in that sandbox; sorted.
func SandboxNames(s Spec, sandbox string) []string {
	var out []string
	for name := range s.Addons {
		out = append(out, name)
	}
	for name, svc := range s.Services {
		switch svc.Runtime {
		case "", RuntimeImage:
			out = append(out, name)
		case RuntimeAttached:
			if strings.EqualFold(svc.Sandbox, sandbox) {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Referenced returns the secrets the env values of the spec's services
// and jobs and the credentials of its bindings reference, by name.
func Referenced(s Spec) []string {
	set := map[string]bool{}
	add := func(env map[string]EnvValue) {
		for _, val := range env {
			if val.Kind == EnvSecret {
				set[val.Secret] = true
			}
		}
	}
	creds := func(c *ImageCredentials) {
		if c == nil {
			return
		}
		for _, val := range []EnvValue{c.Username, c.Password} {
			if val.Kind == EnvSecret {
				set[val.Secret] = true
			}
		}
	}
	for _, svc := range s.Services {
		add(svc.Env)
		creds(svc.ImageCredentials)
	}
	for _, j := range s.Jobs {
		add(j.Env)
		creds(j.ImageCredentials)
	}
	for _, b := range s.Bindings {
		add(b.Credential.Values())
	}
	return slices.Sorted(maps.Keys(set))
}
