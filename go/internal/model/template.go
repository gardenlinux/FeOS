// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// The labels an environment's instance pods carry beside the sandbox
// labels (ADR 0018). There is no release label: an object that a release
// leaves unchanged keeps its pods.
const (
	// LabelEnvironment is the environment's ID.
	LabelEnvironment = "ferun.environment"
	// LabelObject is "<kind>/<name>": "service/web", "addon/postgres",
	// "job/migrate".
	LabelObject = "ferun.object"
	// LabelTemplate is the template's hash (PodTemplate.Hash).
	LabelTemplate = "ferun.template"
)

// EnvGroupID is the security group of an environment's pods:
// "env-<environment UUID>", 40 characters.
func EnvGroupID(envID uuid.UUID) string { return "env-" + envID.String() }

// SandboxGroupID is the security group of an attachable sandbox:
// "sbx-<sandbox ID>", 15 characters (40 for a sandbox with a UUID).
func SandboxGroupID(id podid.ID) string { return "sbx-" + id.String() }

// EnvTableID is the name table of an environment's pods (feosd's
// NameService): its addons and image services. It is the ID of the
// environment's security group.
func EnvTableID(envID uuid.UUID) string { return EnvGroupID(envID) }

// SandboxTableID is the name table of an attachable sandbox: its own
// attached services, and the env tables of the environments they belong
// to. It is the ID of the sandbox's security group.
func SandboxTableID(id podid.ID) string { return SandboxGroupID(id) }

// The escapes of a secret in a template value.
const (
	// EscapeNone puts the secret's value in as it is.
	EscapeNone = ""
	// EscapeURL escapes the value for the userinfo of a URL (RFC 3986),
	// as in postgres://user:<password>@host/db.
	EscapeURL = "url"
)

// PodTemplate is what an instance row's pod is built from
// (sandboxes.pod_template, ADR 0018 §2.3). The row gives the image (its
// image_ref, pinned to a digest) and the resources; the template gives
// the rest of the pod's one container "main" and of its network. A row
// without a template is an ordinary sandbox.
//
// It holds no secret value: an environment variable names a secret by its
// name and version, and the engine unseals it when it builds the pod's
// spec (Engine.SetSecretResolver). A template the controller stores never
// has a credential part (TemplatePart.Credential): those are mapped to
// system secrets first (HasCredential).
type PodTemplate struct {
	// Command replaces the image's entrypoint and cmd; empty: they run.
	Command []string `json:"command,omitempty"`
	// Env are the container's environment variables.
	Env map[string]TemplateValue `json:"env,omitempty"`
	// Volumes are the pod's volumes, by the name the mounts use.
	Volumes []TemplateVolume `json:"volumes,omitempty"`
	// Mounts are the volumes the container mounts.
	Mounts []TemplateMount `json:"mounts,omitempty"`
	// SecurityGroups are the pod's security groups (EnvGroupID,
	// SandboxGroupID, IngressGroupID); none: feosd's default group.
	SecurityGroups []string `json:"securityGroups,omitempty"`
	// NameTables are the name tables whose names feosd's resolver answers
	// the pod (EnvTableID, SandboxTableID), in order; none: no names but
	// the upstreams'.
	NameTables []string `json:"nameTables,omitempty"`
	// Labels are added to the sandbox labels (which win on a clash).
	// Hash leaves them out.
	Labels map[string]string `json:"labels,omitempty"`
	// IngressGroup: the pod is an image service's of an environment that
	// was ever public (environments.ingress_group), and names its own
	// group IngressGroupID(<its sandbox ID>), which lets the ingress in on
	// the routed ports while the environment is public. The controller
	// appends that group to the row's SecurityGroups when it inserts the
	// row, where the ID is minted; the plan's template, which Hash takes,
	// has the flag only.
	IngressGroup bool `json:"ingressGroup,omitempty"`
	// ImageCredentials are the pod's login for its image's registry (ADR
	// 0025); nil: anonymous. Hash takes them: a pod made with other
	// credentials is replaced, while a secret's new value is the same
	// template (version 0).
	ImageCredentials *TemplateCredentials `json:"imageCredentials,omitempty"`
}

// TemplateCredentials are an environment's pod's login for its image's
// registry: a username and a password, each a literal or a secret of the
// environment. A secret here has version 0, its latest when a pod is
// created: they are access, not configuration, and a token rotated or
// revoked reaches the next replica of an old release.
type TemplateCredentials struct {
	Username TemplateValue `json:"username"`
	Password TemplateValue `json:"password"`
}

// TemplateVolume is a volume of the pod (feos.volume.v1): its name in the
// pod and the volume's ID on the node.
type TemplateVolume struct {
	Name     string    `json:"name"`
	VolumeID uuid.UUID `json:"volumeId"`
}

// TemplateMount mounts a volume of the pod into the container.
type TemplateMount struct {
	// Volume is a TemplateVolume's name.
	Volume string `json:"volume"`
	// Path is where the container sees it: absolute, not "/".
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// TemplateValue is the value of one environment variable of a template,
// exactly one of:
//
//	{"value": "s"}                                  a literal (Value)
//	{"secret": "n", "version": 3[, "escape": "url"]} a secret (Secret, Version, Escape)
//	{"parts": [part, …]}                             the parts' values joined (Parts)
//
// Parts non-nil selects the parts form, else a non-empty Secret the
// secret form, else the literal form; the zero value is the literal "".
// It marshals to that form only and unmarshals strictly: unknown keys,
// two forms at once and a form's missing keys are errors.
type TemplateValue struct {
	Value   string
	Secret  string
	Version int
	Escape  string
	Parts   []TemplatePart
}

// TemplatePart is one part of a TemplateValue's parts, exactly one of:
//
//	{"value": "s"}                                    a literal
//	{"secret": "n", "version": 1[, "escape": "url"]}   a secret of the environment
//	{"credential": "k"[, "escape": "url"]}             an addon's credential
//
// A credential appears only in what an addon blueprint renders: the
// controller maps key k of addon a to the system secret
// "addon.<a>.<k>", version 1, before it stores a template. A non-empty
// Credential selects the credential form, else a non-empty Secret the
// secret form, else the literal form.
type TemplatePart struct {
	Value      string
	Secret     string
	Version    int
	Escape     string
	Credential string
}

// Literal is a TemplateValue of the literal s.
func Literal(s string) TemplateValue { return TemplateValue{Value: s} }

// SecretValue is a TemplateValue of a secret's value, escaped by escape
// (EscapeNone, EscapeURL).
func SecretValue(name string, version int, escape string) TemplateValue {
	return TemplateValue{Secret: name, Version: version, Escape: escape}
}

// CredentialValue is a TemplateValue of an addon's credential key, as a
// blueprint renders it.
func CredentialValue(key string) TemplateValue {
	return TemplateValue{Parts: []TemplatePart{{Credential: key}}}
}

// HasCredential reports whether the value has a credential part, which a
// stored template must not have.
func (v TemplateValue) HasCredential() bool {
	for _, p := range v.Parts {
		if p.Credential != "" {
			return true
		}
	}
	return false
}

// HasCredential reports whether any of the template's values has a
// credential part.
func (t PodTemplate) HasCredential() bool {
	for _, v := range t.Env {
		if v.HasCredential() {
			return true
		}
	}
	return false
}

// Hash is the template hash of ADR 0018 §4.3, which an instance stores
// and its pod carries as LabelTemplate: the lowercase hex SHA-256 of a
// canonical JSON of what the pod runs, that is the image (pinned to its
// digest), the resources, and the template without its labels (command,
// env with secrets as name and version, volumes, mounts, security groups
// in sorted order, then name tables in order and the ingress group flag,
// each only when there are any or it is set, so that a template without
// them hashes as before they were). Equal pods hash equal: a nil and an
// empty list or map hash alike. What an instance is not rebuilt for (its
// labels, the idle policy, the release) is not in it.
func (t PodTemplate) Hash(image string, vcpus, memoryMiB int) string {
	groups := slices.Clone(t.SecurityGroups)
	slices.Sort(groups)
	h := struct {
		Scheme         int                      `json:"scheme"`
		Image          string                   `json:"image"`
		VCPUs          int                      `json:"vcpus"`
		MemoryMiB      int                      `json:"memoryMib"`
		Command        []string                 `json:"command"`
		Env            map[string]TemplateValue `json:"env"`
		Volumes        []TemplateVolume         `json:"volumes"`
		Mounts         []TemplateMount          `json:"mounts"`
		SecurityGroups []string                 `json:"securityGroups"`
		NameTables     []string                 `json:"nameTables,omitempty"`
		IngressGroup   bool                     `json:"ingressGroup,omitempty"`
		// Without credentials, a template hashes as before them.
		ImageCredentials *TemplateCredentials `json:"imageCredentials,omitempty"`
	}{
		Scheme: 1, Image: image, VCPUs: vcpus, MemoryMiB: memoryMiB,
		Command: nonNil(t.Command), Env: t.Env, Volumes: nonNil(t.Volumes), Mounts: nonNil(t.Mounts),
		SecurityGroups: nonNil(groups), NameTables: t.NameTables, IngressGroup: t.IngressGroup,
		ImageCredentials: t.ImageCredentials,
	}
	if h.Env == nil {
		h.Env = map[string]TemplateValue{}
	}
	b, err := json.Marshal(h)
	if err != nil {
		// Every field marshals; a value of a form that does not marshal
		// still hashes, apart from every valid one.
		b = []byte(fmt.Sprintf("unmarshalable template: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// The JSON forms of TemplateValue and TemplatePart: each field is present
// exactly in its form.
type (
	jsonLiteral struct {
		Value string `json:"value"`
	}
	jsonSecret struct {
		Secret  string `json:"secret"`
		Version int    `json:"version"`
		Escape  string `json:"escape,omitempty"`
	}
	jsonParts struct {
		Parts []TemplatePart `json:"parts"`
	}
	jsonCredential struct {
		Credential string `json:"credential"`
		Escape     string `json:"escape,omitempty"`
	}
)

// MarshalJSON writes the value's one form.
func (v TemplateValue) MarshalJSON() ([]byte, error) {
	switch {
	case v.Parts != nil:
		return json.Marshal(jsonParts{Parts: v.Parts})
	case v.Secret != "":
		return json.Marshal(jsonSecret{Secret: v.Secret, Version: v.Version, Escape: v.Escape})
	}
	return json.Marshal(jsonLiteral{Value: v.Value})
}

// UnmarshalJSON reads one of the value's forms, strictly.
func (v *TemplateValue) UnmarshalJSON(b []byte) error {
	keys, err := objectKeys(b)
	if err != nil {
		return fmt.Errorf("template value: %w", err)
	}
	var out TemplateValue
	switch {
	case keys.only("value"):
		var l jsonLiteral
		err = strictDecode(b, &l)
		out.Value = l.Value
	case keys.only("secret", "version", "escape") && keys.has("secret", "version"):
		var s jsonSecret
		err = strictDecode(b, &s)
		out.Secret, out.Version, out.Escape = s.Secret, s.Version, s.Escape
		if err == nil {
			err = checkSecret(s.Secret, s.Escape)
		}
	case keys.only("parts"):
		var p jsonParts
		err = strictDecode(b, &p)
		out.Parts = p.Parts
		if err == nil && out.Parts == nil {
			out.Parts = []TemplatePart{}
		}
	default:
		err = errors.New(`want exactly one of {"value"}, {"secret", "version"[, "escape"]} or {"parts"}`)
	}
	if err != nil {
		return fmt.Errorf("template value: %w", err)
	}
	*v = out
	return nil
}

// MarshalJSON writes the part's one form.
func (p TemplatePart) MarshalJSON() ([]byte, error) {
	switch {
	case p.Credential != "":
		return json.Marshal(jsonCredential{Credential: p.Credential, Escape: p.Escape})
	case p.Secret != "":
		return json.Marshal(jsonSecret{Secret: p.Secret, Version: p.Version, Escape: p.Escape})
	}
	return json.Marshal(jsonLiteral{Value: p.Value})
}

// UnmarshalJSON reads one of the part's forms, strictly.
func (p *TemplatePart) UnmarshalJSON(b []byte) error {
	keys, err := objectKeys(b)
	if err != nil {
		return fmt.Errorf("template part: %w", err)
	}
	var out TemplatePart
	switch {
	case keys.only("value"):
		var l jsonLiteral
		err = strictDecode(b, &l)
		out.Value = l.Value
	case keys.only("secret", "version", "escape") && keys.has("secret", "version"):
		var s jsonSecret
		err = strictDecode(b, &s)
		out.Secret, out.Version, out.Escape = s.Secret, s.Version, s.Escape
		if err == nil {
			err = checkSecret(s.Secret, s.Escape)
		}
	case keys.only("credential", "escape") && keys.has("credential"):
		var c jsonCredential
		err = strictDecode(b, &c)
		out.Credential, out.Escape = c.Credential, c.Escape
		if err == nil && c.Credential == "" {
			err = errors.New("empty credential")
		}
		if err == nil {
			err = checkEscape(c.Escape)
		}
	default:
		err = errors.New(`want exactly one of {"value"}, {"secret", "version"[, "escape"]} or {"credential"[, "escape"]}`)
	}
	if err != nil {
		return fmt.Errorf("template part: %w", err)
	}
	*p = out
	return nil
}

func checkSecret(name, escape string) error {
	if name == "" {
		return errors.New("empty secret name")
	}
	return checkEscape(escape)
}

func checkEscape(escape string) error {
	if escape != EscapeNone && escape != EscapeURL {
		return fmt.Errorf("escape %q: want \"\" or %q", escape, EscapeURL)
	}
	return nil
}

// keySet is the set of keys of a JSON object.
type keySet map[string]bool

// only reports whether every key is one of names (and there is one).
func (k keySet) only(names ...string) bool {
	if len(k) == 0 {
		return false
	}
	for key := range k {
		if !slices.Contains(names, key) {
			return false
		}
	}
	return true
}

// has reports whether every one of names is a key.
func (k keySet) has(names ...string) bool {
	for _, n := range names {
		if !k[n] {
			return false
		}
	}
	return true
}

// objectKeys returns the keys of the JSON object b; anything but an
// object is an error.
func objectKeys(b []byte) (keySet, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, errors.New("not a JSON object")
	}
	keys := keySet{}
	for k := range m {
		keys[k] = true
	}
	return keys, nil
}

// strictDecode decodes b into v, refusing unknown fields.
func strictDecode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
