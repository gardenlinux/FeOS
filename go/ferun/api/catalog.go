// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

// The catalog of the addon types (ADR 0032).

// AddonCatalog is GET /api/v1/addons: the addon types this ferund runs,
// by type (ADR 0032), as their blueprints declare them.
type AddonCatalog struct {
	Addons []AddonType `json:"addons"`
}

// AddonType is a blueprint in the catalog.
type AddonType struct {
	// Type is what a spec's addons.*.type names.
	Type string `json:"type"`
	// Versions are the versions a spec may name, oldest first;
	// DefaultVersion the newest, which a new addon is offered with (a
	// spec must name its version: a default would change under a running
	// addon).
	Versions       []string `json:"versions"`
	DefaultVersion string   `json:"defaultVersion"`
	// Plans are the sizes, smallest first; DefaultPlan the plan of an
	// addon that names none.
	Plans       []AddonPlan `json:"plans"`
	DefaultPlan string      `json:"defaultPlan"`
	// OutputKeys are the keys a reference ({addon, key}) may name,
	// OutputFields the fields it may name besides them ([] for none).
	OutputKeys   []string `json:"outputKeys"`
	OutputFields []string `json:"outputFields"`
	// ByIdentity: the addon admits a referencing workload by its pod's
	// certificate (ADR 0036, 0038; the blueprint binds by TLS),
	// so a reference hands the container no password; false: the
	// reference's values carry one (nats).
	ByIdentity bool `json:"byIdentity"`
	// IdentityBy is the part of the certificate such an addon maps to a
	// user of its own: "ou" (the subject's OU, the workload's set:
	// postgres's role) or "uri" (the SPIFFE ID: valkey's ACL user); ""
	// for an addon that admits by a password.
	IdentityBy string `json:"identityBy"`
	// Options are the options the blueprint declares; any other is an
	// error of the plan.
	Options []Option `json:"options"`
}

// AddonPlan is a plan of an addon type: the pod's size and the volume's
// when it is first created (never resized).
type AddonPlan struct {
	Name      string `json:"name"`
	VCPUs     int    `json:"vcpus"`
	MemoryMib int    `json:"memoryMib"`
	VolumeMib int    `json:"volumeMib"`
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
