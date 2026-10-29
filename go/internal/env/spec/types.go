// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Spec is a release spec: the typed objects of an environment, each map
// keyed by the object's DNS-label name. As a client writes it, most
// fields may be left out; in its normalised form (Normalise) the defaults
// are filled in, the runtime is explicit, durations are Go's, images are
// pinned to digests and every referenced secret to a version.
type Spec struct {
	Services map[string]Service   `json:"services,omitempty"`
	Jobs     map[string]Job       `json:"jobs,omitempty"`
	Addons   map[string]Addon     `json:"addons,omitempty"`
	Routes   map[string]Route     `json:"routes,omitempty"`
	Secrets  map[string]SecretRef `json:"secrets,omitempty"`
	// Volumes is a kind that is not available yet: its presence is an
	// error finding "volumes for services are not available yet" (addons
	// have volumes of their own). It is a field so that the finding can
	// say so instead of "unknown field".
	Volumes json.RawMessage `json:"volumes,omitempty"`
}

// Service is a long-running object: replicas of an image (runtime
// "image"), or a supervised process in a sandbox of the tenant (runtime
// "attached", e.g. a dev server of a workspace).
type Service struct {
	// Runtime is RuntimeImage (the default when Image is set) or
	// RuntimeAttached.
	Runtime string `json:"runtime,omitempty"`
	// Image is the image: as written, or pinned to its digest
	// ("<registry>/<repo>@sha256:…") in the normalised form. Image
	// runtime only.
	Image string `json:"image,omitempty"`
	// ImageAsWritten is the image as the client wrote it, kept by
	// Normalise when it pins Image; informational on input.
	ImageAsWritten string `json:"imageAsWritten,omitempty"`
	// ImageCredentials are the login for the image's registry (ADR 0025);
	// nil: anonymous. Image runtime only.
	ImageCredentials *ImageCredentials `json:"imageCredentials,omitempty"`
	// Sandbox is the attached runtime's sandbox: the ID of a sandbox of
	// the tenant (not an instance row), short or a UUID.
	Sandbox string `json:"sandbox,omitempty"`
	// Command replaces the image's entrypoint and cmd (empty: they run);
	// the attached runtime's process (required there).
	Command []string `json:"command,omitempty"`
	// Cwd is the attached runtime's working directory.
	Cwd string `json:"cwd,omitempty"`
	// Env are the environment variables, plain or references.
	Env map[string]EnvValue `json:"env,omitempty"`
	// Ports are the ports it listens on (on [::], dual stack), by name.
	Ports []Port `json:"ports,omitempty"`
	// Replicas is 0..20; nil: 1. Image runtime only.
	Replicas *int `json:"replicas,omitempty"`
	// Resources are each replica's; nil: 1 vCPU, 512 MiB. Image runtime
	// only.
	Resources *Resources `json:"resources,omitempty"`
	// Readiness decides when a replica receives traffic; nil: ready when
	// it still runs 2 s after it was first seen running.
	Readiness *Readiness `json:"readiness,omitempty"`
	// Idle is how long a replica without traffic runs before it is paused
	// (at least 1m; a route wakes it); "": never. Image runtime, route
	// targets only.
	Idle string `json:"idle,omitempty"`
	// Rollout bounds a release's rollout of the service. Image runtime
	// only.
	Rollout *Rollout `json:"rollout,omitempty"`
	// Mounts is not available yet: its presence is an error finding
	// "volumes for services are not available yet".
	Mounts json.RawMessage `json:"mounts,omitempty"`
}

// Port is a named port of a service.
type Port struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// Resources are a pod's size.
type Resources struct {
	// VCPUs is 1..32; 0: 1.
	VCPUs int `json:"vcpus,omitempty"`
	// MemoryMib is 256..65536 (a pod's floor); 0: 512.
	MemoryMib int `json:"memoryMib,omitempty"`
}

// Readiness is a service's readiness check: exactly one of HTTP, TCP and
// Exec, with its timing.
type Readiness struct {
	HTTP *HTTPCheck `json:"http,omitempty"`
	TCP  *TCPCheck  `json:"tcp,omitempty"`
	Exec *ExecCheck `json:"exec,omitempty"`
	// InitialDelay passes before the first check counts; "": 0s.
	InitialDelay string `json:"initialDelay,omitempty"`
	// Period is the time between checks; "": 2s.
	Period string `json:"period,omitempty"`
	// FailureThreshold is how many failures in a row make a ready replica
	// unready; 0: 3.
	FailureThreshold int `json:"failureThreshold,omitempty"`
}

// HTTPCheck is ready on a GET of Path answered 200..399.
type HTTPCheck struct {
	Port PortRef `json:"port"`
	Path string  `json:"path"`
}

// TCPCheck is ready when the port accepts a connection.
type TCPCheck struct {
	Port PortRef `json:"port"`
}

// ExecCheck is ready when the command exits 0 in the container.
type ExecCheck struct {
	Command []string `json:"command"`
}

// Rollout bounds a release's rollout of a service.
type Rollout struct {
	// Timeout is counted from the release's start; "": 10m.
	Timeout string `json:"timeout,omitempty"`
}

// Job is a run-to-completion object: on every release whose template of
// it differs from its last succeeded run (On "release"), or when asked
// (On "manual").
type Job struct {
	Image          string              `json:"image,omitempty"`
	ImageAsWritten string              `json:"imageAsWritten,omitempty"`
	Command        []string            `json:"command,omitempty"`
	Env            map[string]EnvValue `json:"env,omitempty"`
	Resources      *Resources          `json:"resources,omitempty"`
	// ImageCredentials are the login for the image's registry (ADR 0025);
	// nil: anonymous.
	ImageCredentials *ImageCredentials `json:"imageCredentials,omitempty"`
	// On is JobOnRelease (the default) or JobOnManual.
	On string `json:"on,omitempty"`
	// Timeout bounds one attempt; "": 10m, at most 2h.
	Timeout string `json:"timeout,omitempty"`
	// Retries is how many attempts follow a failed one: 0..5.
	Retries int `json:"retries,omitempty"`
}

// ImageCredentials are a service's or job's login for its image's registry
// (ADR 0025): a username and a password, each a string or a secret of the
// environment ({"secret": n}). A secret named here is read at its latest
// version whenever a pod is created, not at the version the release pins:
// they are access, not configuration, and a token rotated or revoked
// reaches the next replica of an old release.
type ImageCredentials struct {
	Username EnvValue `json:"username"`
	Password EnvValue `json:"password"`
}

// Addon is a managed backing service of a registered type (a blueprint
// of internal/env/addons): its version and plan, and the options the type
// defines.
type Addon struct {
	Type    string         `json:"type"`
	Version string         `json:"version,omitempty"`
	Plan    string         `json:"plan,omitempty"`
	Options map[string]any `json:"options,omitempty"`
}

// Route sends HTTP requests whose path starts with Path (segment-aware) to
// a port of a service, through the route door.
type Route struct {
	// Path starts with "/"; unique among the routes.
	Path    string  `json:"path"`
	Service string  `json:"service"`
	Port    PortRef `json:"port"`
	// Strip removes Path from the request's path before it is forwarded.
	Strip bool `json:"strip,omitempty"`
	// Host is accepted and stored, not realised yet (an info finding).
	Host string `json:"host,omitempty"`
}

// SecretRef pins a tenant secret of the environment to a version; 0: the
// latest when the release is applied.
type SecretRef struct {
	Version int `json:"version,omitempty"`
}

// PortRef names a port of a service by its name ("http") or its number
// (8080); in JSON a string or a number.
type PortRef struct {
	Name   string
	Number int
}

// IsZero reports a PortRef that names nothing (omitzero).
func (p PortRef) IsZero() bool { return p.Name == "" && p.Number == 0 }

// String is the name, or the number.
func (p PortRef) String() string {
	if p.Name != "" {
		return p.Name
	}
	return strconv.Itoa(p.Number)
}

// MarshalJSON writes the name as a string, else the number.
func (p PortRef) MarshalJSON() ([]byte, error) {
	if p.Name != "" {
		return json.Marshal(p.Name)
	}
	return json.Marshal(p.Number)
}

// UnmarshalJSON reads a non-empty string or an integer.
func (p *PortRef) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			return errors.New("port: empty name")
		}
		*p = PortRef{Name: s}
		return nil
	case len(b) > 0 && (b[0] == '-' || b[0] >= '0' && b[0] <= '9'):
		n, err := strconv.Atoi(string(b))
		if err != nil {
			return fmt.Errorf("port: %s is not an integer", b)
		}
		*p = PortRef{Number: n}
		return nil
	}
	return fmt.Errorf("port: want a name or a number, not %s", b)
}

// EnvKind is the form of an EnvValue.
type EnvKind string

// The forms of an environment variable's value.
const (
	// EnvString is a plain JSON string: "[::]:8080".
	EnvString EnvKind = ""
	// EnvLiteral is {"value": "s"}.
	EnvLiteral EnvKind = "value"
	// EnvSecret is {"secret": "name"}: a tenant secret of the
	// environment, at the version the release pins.
	EnvSecret EnvKind = "secret"
	// EnvAddon is {"addon": "a", "key": "k", …fields}: an output of an
	// addon (a URL, a host, a password), with the blueprint's fields
	// ("role", "database").
	EnvAddon EnvKind = "addon"
	// EnvService is {"service": "s", "port": p, "key": "url"|"host"|"port"}:
	// how to reach a service of the environment.
	EnvService EnvKind = "service"
)

// The keys of a service reference.
const (
	ServiceKeyURL  = "url"
	ServiceKeyHost = "host"
	ServiceKeyPort = "port"
)

// EnvValue is the value of an environment variable in a spec: a plain
// string or exactly one reference form (EnvKind). It marshals to the form
// it was read from: a plain string stays a string, {"value": s} stays an
// object. Its zero value is the plain string "".
type EnvValue struct {
	Kind EnvKind
	// Value is EnvString's and EnvLiteral's string.
	Value string
	// Secret is EnvSecret's secret name.
	Secret string
	// Addon is EnvAddon's addon name.
	Addon string
	// Service and Port are EnvService's service and port.
	Service string
	Port    PortRef
	// Key is EnvAddon's and EnvService's key ("url", "host", …).
	Key string
	// Fields are EnvAddon's other fields, which its blueprint defines.
	Fields map[string]string
}

// String is an EnvValue of the plain string s.
func String(s string) EnvValue { return EnvValue{Value: s} }

// SecretEnv is an EnvValue of the secret name.
func SecretEnv(name string) EnvValue { return EnvValue{Kind: EnvSecret, Secret: name} }

// The keys an EnvAddon may not use as fields: they belong to the other
// forms.
var reservedEnvKeys = map[string]bool{"value": true, "secret": true, "service": true, "port": true}

// MarshalJSON writes the value's form.
func (v EnvValue) MarshalJSON() ([]byte, error) {
	switch v.Kind {
	case EnvString:
		return json.Marshal(v.Value)
	case EnvLiteral:
		return json.Marshal(struct {
			Value string `json:"value"`
		}{v.Value})
	case EnvSecret:
		return json.Marshal(struct {
			Secret string `json:"secret"`
		}{v.Secret})
	case EnvAddon:
		m := make(map[string]string, len(v.Fields)+2)
		for k, f := range v.Fields {
			m[k] = f
		}
		m["addon"] = v.Addon
		if v.Key != "" {
			m["key"] = v.Key
		}
		return json.Marshal(m)
	case EnvService:
		return json.Marshal(struct {
			Service string  `json:"service"`
			Port    PortRef `json:"port,omitzero"`
			Key     string  `json:"key,omitempty"`
		}{v.Service, v.Port, v.Key})
	}
	return nil, fmt.Errorf("env value of unknown kind %q", v.Kind)
}

// UnmarshalJSON reads a plain string or one reference form, strictly: a
// number, a boolean, null, a list, two forms at once, an unknown key of a
// form and a non-string value are errors.
func (v *EnvValue) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*v = EnvValue{Value: s}
		return nil
	}
	var m map[string]json.RawMessage
	if len(b) == 0 || b[0] != '{' || json.Unmarshal(b, &m) != nil {
		return errors.New(`an env value is a string or one of {"value"}, {"secret"}, {"addon", "key", …} and {"service", "port", "key"}`)
	}
	str := func(k string) (string, error) {
		var s string
		if raw := bytes.TrimSpace(m[k]); len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
			return "", fmt.Errorf("%s: want a string", k)
		}
		return s, nil
	}
	_, isAddon := m["addon"]
	_, isService := m["service"]
	var out EnvValue
	var err error
	switch {
	case len(m) == 1 && m["value"] != nil:
		out.Kind = EnvLiteral
		out.Value, err = str("value")
	case len(m) == 1 && m["secret"] != nil:
		out.Kind = EnvSecret
		out.Secret, err = str("secret")
	case isAddon && !isService:
		out.Kind = EnvAddon
		for k := range m {
			if reservedEnvKeys[k] {
				return fmt.Errorf("an addon reference cannot have %q", k)
			}
			s, err := str(k)
			if err != nil {
				return err
			}
			switch k {
			case "addon":
				out.Addon = s
			case "key":
				out.Key = s
			default:
				if out.Fields == nil {
					out.Fields = map[string]string{}
				}
				out.Fields[k] = s
			}
		}
	case isService && !isAddon:
		out.Kind = EnvService
		for k, raw := range m {
			switch k {
			case "service":
				out.Service, err = str(k)
			case "key":
				out.Key, err = str(k)
			case "port":
				err = json.Unmarshal(raw, &out.Port)
			default:
				err = fmt.Errorf("a service reference has service, port and key, not %q", k)
			}
			if err != nil {
				return err
			}
		}
	default:
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return fmt.Errorf(`env value with keys %s: want exactly one of {"value"}, {"secret"}, {"addon", "key", …} and {"service", "port", "key"}`,
			strings.Join(keys, ", "))
	}
	if err != nil {
		return err
	}
	*v = out
	return nil
}

// The runtimes of a service.
const (
	RuntimeImage    = "image"
	RuntimeAttached = "attached"
)

// When a job runs.
const (
	JobOnRelease = "release"
	JobOnManual  = "manual"
)

// The kinds of objects, as diffs, object routes (PUT …/objects/{kind}/
// {name}), the status and the pods' LabelObject name them.
const (
	KindService = "service"
	KindJob     = "job"
	KindAddon   = "addon"
	KindRoute   = "route"
	KindSecret  = "secret"
)

// The defaults and limits of the contract's §3.
const (
	MaxSpecBytes = 1 << 20
	MaxServices  = 64
	MaxJobs      = 32
	MaxAddons    = 16
	MaxRoutes    = 128
	// MaxRoutePathBytes bounds a route's path, which every instance of
	// ferun-ingress holds for every public environment.
	MaxRoutePathBytes = 1024

	DefaultReplicas  = 1
	MaxReplicas      = 20
	MinVCPUs         = 1
	MaxVCPUs         = 32
	DefaultVCPUs     = 1
	MinMemoryMib     = 256
	MaxMemoryMib     = 65536
	DefaultMemoryMib = 512
	MaxRetries       = 5

	DefaultRolloutTimeout   = "10m0s"
	DefaultJobTimeout       = "10m0s"
	MaxJobTimeout           = "2h0m0s"
	MinIdle                 = "1m0s"
	DefaultInitialDelay     = "0s"
	DefaultPeriod           = "2s"
	DefaultFailureThreshold = 3
)

// Severity is how bad a finding is.
type Severity string

const (
	// SeverityError refuses the spec: plan says valid false, apply answers
	// 422.
	SeverityError Severity = "error"
	// SeverityWarning is shown, and does not refuse.
	SeverityWarning Severity = "warning"
	// SeverityInfo is a note: "nothing changes", "dropped".
	SeverityInfo Severity = "info"
)

// Finding is one problem of a spec, or a note about it.
type Finding struct {
	Severity Severity `json:"severity"`
	// Path is where in the spec, dotted from its root with list indexes
	// in brackets: "services.web.ports[1].port",
	// "addons.postgres.options.roles[0]"; "" for the spec as a whole.
	Path string `json:"path"`
	// Problem says what is wrong, Remedy what to do about it ("" when
	// there is nothing to do).
	Problem string `json:"problem"`
	Remedy  string `json:"remedy"`
}

// HasErrors reports whether any finding is an error.
func HasErrors(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// The changes of a diff entry.
const (
	ChangeAdd    = "add"
	ChangeRemove = "remove"
	ChangeUpdate = "update"
)

// The effects of a diff entry on the running pods; "replaces N pods" is
// EffectReplaces(n).
const (
	EffectInPlace      = "in place"
	EffectRestarts     = "restarts"
	EffectRecreates    = "recreates (downtime)"
	EffectDeletes      = "deletes data"
	EffectReprovisions = "reprovisions"
)

// EffectReplaces is the effect of a rolling replacement of n pods.
func EffectReplaces(n int) string {
	if n == 1 {
		return "replaces 1 pod"
	}
	return fmt.Sprintf("replaces %d pods", n)
}

// DiffEntry is the change of one object between two releases.
type DiffEntry struct {
	// Kind is KindService, KindJob, KindAddon, KindRoute or KindSecret.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Change is ChangeAdd, ChangeRemove or ChangeUpdate.
	Change string `json:"change"`
	// Fields are the changed fields of an update (empty for an add or a
	// remove).
	Fields []FieldChange `json:"fields"`
	// Effect is what applying it does to the running pods (EffectInPlace,
	// EffectReplaces, …).
	Effect string `json:"effect"`
}

// FieldChange is one changed field of an object: its path relative to
// the object ("replicas", "env.DATABASE_URL", "image") and the values
// before and after (null when absent). A secret's value never appears;
// its version does. An image shows as written and its digest.
type FieldChange struct {
	Path string `json:"path"`
	From any    `json:"from"`
	To   any    `json:"to"`
}
