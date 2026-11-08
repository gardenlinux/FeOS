// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package addons holds the addon blueprints of environments (ADR 0018):
// what a managed backing service (postgres, valkey, nats) of a given
// type, version and plan runs as, which credentials it has, what a
// reference to it resolves to, and how it is provisioned. A blueprint is
// pure: it renders, the environment controller (internal/env/ctl) runs.
//
// Credentials are never values here: a blueprint names them by key
// ("password", "role.expense_svc.password") in TemplatePart.Credential;
// the controller generates each once as the system secret
// "addon.<addon>.<key>", version 1, and maps the parts to it.
//
// Every blueprint registers itself (Register) from an init function of
// its file; Get and Types read the registry.
package addons

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

// Blueprint is an addon type.
type Blueprint interface {
	// Type is the type's name, as a spec's addons.*.type names it:
	// "postgres".
	Type() string
	// Versions are the versions a spec may name, oldest first.
	Versions() []string
	// Plans are the plans by name ("dev", "small", …).
	Plans() map[string]Plan
	// DefaultPlan is the plan of an addon that names none.
	DefaultPlan() string
	// Validate checks an addon against the current deployment's same addon
	// (prev nil: none): version, plan and options. Paths are relative to
	// the addon: "version", "options.roles[1]". A major version change is
	// an error, a plan change that would resize the volume a warning (the
	// volume keeps its size).
	Validate(prev *Addon, next Addon) []spec.Finding
	// Credentials are the keys of the addon's credentials, e.g.
	// "password", "role.expense_svc.password".
	Credentials(a Addon) []string
	// Render is the addon's pod.
	Render(a Addon, c Context) (Instance, error)
	// Outputs is the value of a reference to the addon: literal parts and
	// credential parts (with Escape where they sit in a URL), given the
	// instance's address (c.Address, "[addr]" in URLs). An unknown key, or
	// a field that names nothing (a role not in the options), is an error.
	Outputs(a Addon, c Context, ref Ref) ([]model.TemplatePart, error)
	// OutputKeys are the keys a reference may name, for validation and
	// docs.
	OutputKeys() []string
	// OutputFields are the fields a reference may name besides addon and
	// key ("role", "database"); none: nil. Outputs refuses any other.
	OutputFields() []string
	// Options are the options the blueprint declares, in the order a form
	// shows them. Validate reads its options by these declarations (their
	// names, defaults, values and constraints), and the catalog answers
	// them (GET /api/v1/addons), so that the two cannot drift; any other
	// option is an error finding.
	Options() []Option
	// Provision are the steps that bring a running addon to its options
	// (databases, roles), run in its container in order, each idempotent;
	// none: nil. Their hash decides whether they run again.
	Provision(a Addon, c Context) []Step
}

// Addon is an addon of a spec, with its name (the key of spec.addons).
type Addon struct {
	Name, Type, Version, Plan string
	Options                   map[string]any
}

// Option is an option a blueprint declares: a key of an addon's
// options, its kind, its default and what its values must be.
type Option struct {
	// Name is the key in the addon's options: "databases".
	Name string `json:"name"`
	// Kind is OptionString, OptionList (a list of strings), OptionBoolean
	// or OptionOneOf (a string among Values).
	Kind string `json:"kind"`
	// Required: the addon must set it. Else Default applies.
	Required bool `json:"required"`
	// Default is the value of an addon that leaves it out (or sets it to
	// null): a string, a list of strings or a boolean, as Kind says.
	Default any `json:"default"`
	// Description says what the option does, for a form and the docs.
	Description string `json:"description"`
	// Values are a OneOf's values.
	Values []string `json:"values"`
	// Pattern is a regular expression (RE2, anchored) that a string, or
	// each element of a list, must match; "": any.
	Pattern string `json:"pattern"`
	// Reserved are the values a string or an element cannot be, and
	// ReservedPrefixes the prefixes it cannot start with.
	Reserved         []string `json:"reserved"`
	ReservedPrefixes []string `json:"reservedPrefixes"`
	// Unique: a list's elements are distinct.
	Unique bool `json:"unique"`
}

// The kinds of an option (Option.Kind).
const (
	OptionString  = "string"
	OptionList    = "list"
	OptionBoolean = "boolean"
	OptionOneOf   = "oneOf"
)

// Plan is a size of an addon.
type Plan struct {
	// VCPUs and MemoryMib size the pod.
	VCPUs, MemoryMib int
	// VolumeMib is the volume's size when it is first created; it is
	// never resized.
	VolumeMib int
}

// Context is what a blueprint renders against. Credentials are
// referenced by key, never passed as values.
type Context struct {
	// Env is the environment's name, Name the addon's.
	Env, Name string
	// Address is the instance's IPv6 address ("" before it has one).
	Address string
	// TLS is the server certificate and the grants an addon of a
	// CertificateServer is provisioned with (ADR 0038); nil before the
	// controller has them, which renders no TLS step.
	TLS *TLS
}

// TLSKey is the credential that holds the private key of an addon that
// serves TLS (ADR 0038): Ed25519, PKCS#8 PEM, generated once by the
// controller, never on the network.
const TLSKey = "tls.key"

// TLS is what an addon that admits workloads by their certificates is
// provisioned with (ADR 0038).
type TLS struct {
	// CertPEM is its server certificate's chain (leaf, issuer), which the
	// controller issues for the addon's ID; CAPEM is what it trusts of its
	// clients (the platform's roots and the issuer).
	CertPEM, CAPEM string
	// Grants are the deployment's references to the addon's roles: one
	// per workload and role, sorted by workload, then role.
	Grants []Grant
}

// Grant is a workload's reference to a role of an addon: the role its
// certificate may become (ADR 0038).
type Grant struct {
	// Workload is the workload's ID as its certificate's OU carries it
	// (ADR 0036): wl-<32 hex> for a service or a job, wl-s-<id> for the
	// sandbox an attached service runs in.
	Workload string `json:"workload"`
	// ID is the workload's SPIFFE ID, which the map names in a comment.
	ID string `json:"id"`
	// Role is the role the reference names.
	Role string `json:"role"`
}

// CertificateServer is a blueprint whose addon terminates TLS with a
// server certificate the controller issues, and admits a client by its
// certificate, mapped to the role its references grant (ADR 0038). Its
// Credentials include TLSKey.
type CertificateServer interface {
	Blueprint
	// GrantedRole is the role a reference to the addon grants the
	// workload that holds it ("": none).
	GrantedRole(ref Ref) string
}

// Instance is an addon's pod as its blueprint renders it.
type Instance struct {
	// Image is the image to run, as written ("postgres:17-alpine").
	Image string
	// Command replaces the image's entrypoint and cmd; empty: they run.
	Command []string
	// Env are the container's variables: literals, or credential parts
	// that the controller maps to system secrets.
	Env map[string]model.TemplateValue
	// Port is the port it serves on.
	Port int
	// Mount is where the addon's volume is mounted; "": no volume.
	Mount string
	// Probe is its readiness check: an argv run in the container with
	// Env, ready on exit 0.
	Probe []string
}

// Step is one provisioning step: an argv run in the addon's container
// with Env (credentials as parts).
type Step struct {
	Name string
	Argv []string
	Env  map[string]model.TemplateValue
}

// Ref is a reference to an addon's output: its key and the blueprint's
// fields ({"addon": "postgres", "key": "url", "role": "r"} → Key "url",
// Fields {"role": "r"}).
type Ref struct {
	Key    string
	Fields map[string]string
}

var (
	mu       sync.RWMutex
	registry = map[string]Blueprint{}
)

// Register adds a blueprint to the registry, from an init function of
// its file. A nil blueprint, an empty type and a type registered before
// are refused.
func Register(b Blueprint) error {
	if b == nil {
		return errors.New("addons: register a nil blueprint")
	}
	t := b.Type()
	if t == "" {
		return errors.New("addons: register a blueprint without a type")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := registry[t]; ok {
		return fmt.Errorf("addons: a blueprint of type %q is registered already", t)
	}
	registry[t] = b
	return nil
}

// Get returns the blueprint of type typ; ok false when none is
// registered.
func Get(typ string) (b Blueprint, ok bool) {
	mu.RLock()
	defer mu.RUnlock()
	b, ok = registry[typ]
	return b, ok
}

// Types are the registered types, sorted.
func Types() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// CredentialSecret is the system secret a credential key of addon is
// kept as: "addon.<addon>.<key>", version 1.
func CredentialSecret(addon, key string) string { return "addon." + addon + "." + key }
