// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import fs "github.com/gardenlinux/feos/go/ferun/spec"

// The spec as FeRun publishes it (ADR 0057): the types, the words, the
// strict parser and the merge patch are the module's
// github.com/gardenlinux/feos/go/ferun/spec. These aliases are the same
// definitions under ferund's names, not copies, so that ferund's code
// reads spec.X whichever side of the cut X is on.

// The types of a spec, of its findings and of a diff.
type (
	Spec              = fs.Spec
	Service           = fs.Service
	Port              = fs.Port
	Resources         = fs.Resources
	Readiness         = fs.Readiness
	HTTPCheck         = fs.HTTPCheck
	TCPCheck          = fs.TCPCheck
	ExecCheck         = fs.ExecCheck
	Rollout           = fs.Rollout
	Job               = fs.Job
	Binding           = fs.Binding
	CredentialRef     = fs.CredentialRef
	BindingRateLimit  = fs.BindingRateLimit
	BindingCredential = fs.BindingCredential
	ImageCredentials  = fs.ImageCredentials
	Addon             = fs.Addon
	Route             = fs.Route
	SecretRef         = fs.SecretRef
	PortRef           = fs.PortRef
	EnvKind           = fs.EnvKind
	EnvValue          = fs.EnvValue
	Severity          = fs.Severity
	Finding           = fs.Finding
	DiffEntry         = fs.DiffEntry
	FieldChange       = fs.FieldChange
)

// The types of reachability.
type (
	Egress       = fs.Egress
	Block        = fs.Block
	Use          = fs.Use
	Reaches      = fs.Reaches
	Reachability = fs.Reachability
	Edge         = fs.Edge
	ObjectEgress = fs.ObjectEgress
	RouteClients = fs.RouteClients
)

// The words of a spec.
const (
	EnvString  = fs.EnvString
	EnvLiteral = fs.EnvLiteral
	EnvSecret  = fs.EnvSecret
	EnvAddon   = fs.EnvAddon
	EnvService = fs.EnvService

	ServiceKeyURL  = fs.ServiceKeyURL
	ServiceKeyHost = fs.ServiceKeyHost
	ServiceKeyPort = fs.ServiceKeyPort
	SecretKeyURL   = fs.SecretKeyURL
	SecretKeyHost  = fs.SecretKeyHost
	SecretTypeHTTP = fs.SecretTypeHTTP

	SecretTypeValue     = fs.SecretTypeValue
	SecretModeName      = fs.SecretModeName
	SecretModeIntercept = fs.SecretModeIntercept

	RuntimeImage    = fs.RuntimeImage
	RuntimeAttached = fs.RuntimeAttached

	JobOnDeploy  = fs.JobOnDeploy
	JobOnRelease = fs.JobOnRelease
	JobOnManual  = fs.JobOnManual
	JobOnStored  = fs.JobOnStored

	KindService = fs.KindService
	KindJob     = fs.KindJob
	KindAddon   = fs.KindAddon
	KindRoute   = fs.KindRoute
	KindSecret  = fs.KindSecret
	KindBinding = fs.KindBinding

	MaxSpecBytes = fs.MaxSpecBytes
	IdleNever    = fs.IdleNever

	SeverityError   = fs.SeverityError
	SeverityWarning = fs.SeverityWarning
	SeverityInfo    = fs.SeverityInfo

	ChangeAdd    = fs.ChangeAdd
	ChangeRemove = fs.ChangeRemove
	ChangeUpdate = fs.ChangeUpdate

	EffectInPlace      = fs.EffectInPlace
	EffectRestarts     = fs.EffectRestarts
	EffectRecreates    = fs.EffectRecreates
	EffectDeletes      = fs.EffectDeletes
	EffectReprovisions = fs.EffectReprovisions

	ErrSecurityGroupsGone = fs.ErrSecurityGroupsGone
	EgressInternet        = fs.EgressInternet
	EgressNone            = fs.EgressNone
	EgressRestricted      = fs.EgressRestricted
	ReachesAll            = fs.ReachesAll
	EdgeIngress           = fs.EdgeIngress
)

// Parse reads a spec strictly (fs.Parse).
func Parse(raw []byte) (Spec, []Finding) { return fs.Parse(raw) }

// ParseYAML reads a spec in YAML strictly (fs.ParseYAML).
func ParseYAML(raw []byte) (Spec, []Finding) { return fs.ParseYAML(raw) }

// YAMLToJSON is the JSON a YAML document stands for (fs.YAMLToJSON).
func YAMLToJSON(raw []byte) ([]byte, []Finding) { return fs.YAMLToJSON(raw) }

// Portable is s carried to another place (fs.Portable): its pins keep
// what the code expects and lose their versions and upstreams.
func Portable(s Spec) Spec { return fs.Portable(s) }

// MergePatch applies an RFC 7396 merge patch to a JSON document
// (fs.MergePatch).
func MergePatch(doc, patch []byte) ([]byte, error) { return fs.MergePatch(doc, patch) }

// SortFindings orders findings by path (fs.SortFindings).
func SortFindings(f []Finding) []Finding { return fs.SortFindings(f) }

// HasErrors reports whether any finding is an error (fs.HasErrors).
func HasErrors(f []Finding) bool { return fs.HasErrors(f) }

// String is a plain string value (fs.String).
func String(s string) EnvValue { return fs.String(s) }

// SecretEnv is a reference to a value secret (fs.SecretEnv).
func SecretEnv(name string) EnvValue { return fs.SecretEnv(name) }

// HTTPSecretEnv is a reference to an HTTP secret's key (fs.HTTPSecretEnv).
func HTTPSecretEnv(name, key string) EnvValue { return fs.HTTPSecretEnv(name, key) }

// IsJobOnDeploy reports whether on runs a job with every deployment
// (fs.IsJobOnDeploy).
func IsJobOnDeploy(on string) bool { return fs.IsJobOnDeploy(on) }

// EffectReplaces is the effect of a rolling replacement of n pods
// (fs.EffectReplaces).
func EffectReplaces(n int) string { return fs.EffectReplaces(n) }

// ObjectRef is "<kind>/<name>" (fs.ObjectRef).
func ObjectRef(kind, name string) string { return fs.ObjectRef(kind, name) }
