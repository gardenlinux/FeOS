// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"

	"github.com/gardenlinux/feos/go/ferun/spec"
)

// The request and answer bodies of the environments' REST routes (the
// contract's §4.9 and §5), as the API reads and writes them: camelCase,
// times as RFC 3339 in seconds, UTC ("" for none), deployment numbers as
// JSON numbers, incident IDs as strings. Answers carry every field, so
// that clients see the same shape every time.

// CreateEnvRequest is POST /api/v1/environments.
type CreateEnvRequest struct {
	// Name is a DNS label, never a UUID, unique per project.
	Name string `json:"name"`
	// ID is the environment's UUID; "": a new one. Creating the same ID
	// and name again answers 200 with the environment.
	ID     string            `json:"id,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// RenameEnvRequest is PATCH /api/v1/environments/{env} (ADR 0037).
type RenameEnvRequest struct {
	// Name is the environment's new name, as a create's.
	Name string `json:"name"`
}

// EnvList is GET /api/v1/environments[?label=k=v…].
type EnvList struct {
	Environments []EnvSummary `json:"environments"`
}

// EnvSummary is an environment in the list.
type EnvSummary struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	Deployment int               `json:"deployment"`
	Deployed   int               `json:"deployed"`
	// State is the latest deployment's state (Deployment…),
	// EnvNone without a deployment, EnvDeleting.
	State string `json:"state"`
	// Public, Hosts, HostLabel, Access and Provider are as EnvStatus has
	// them.
	Public    bool     `json:"public"`
	Hosts     []string `json:"hosts"`
	HostLabel string   `json:"hostLabel,omitempty"`
	Access    string   `json:"access"`
	Provider  string   `json:"provider"`
	// Protected is as EnvStatus has it.
	Protected     bool   `json:"protected"`
	OpenIncidents int    `json:"openIncidents"`
	CreatedAt     string `json:"createdAt"`
}

// EnvStatus is GET /api/v1/environments/{env}, and the answer of a create
// and a clone.
type EnvStatus struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	Deployment int               `json:"deployment"`
	Deployed   int               `json:"deployed"`
	Deleting   bool              `json:"deleting"`
	// Public: its project made it public (PUT …/public), and the ingress
	// serves its image services' routes without a route token (ADR 0021).
	Public bool `json:"public"`
	// Hosts are the hosts the ingress serves it at while it is public: its
	// served domains in the order they were bound, else its default host
	// under the apps zone while the platform has one; empty otherwise, and
	// while it is being deleted.
	Hosts []string `json:"hosts"`
	// HostLabel is the label of its default host, given at its first
	// publish or chosen by its project; empty before. It stays while a
	// domain is its host.
	HostLabel string `json:"hostLabel,omitempty"`
	// Access is who the ingress admits while it is public (ADR 0054):
	// AccessAnyone or AccessVisitors, as stored, public or
	// not (a never published one reads anyone); Provider, with visitors,
	// the provider's name (ProviderFerun for the builtin), "" with
	// anyone.
	Access   string `json:"access"`
	Provider string `json:"provider"`
	// Protected: the environment is changed by a grant on itself alone
	// (ADR 0060), not by a project's write.
	Protected bool `json:"protected"`
	// Rollout is the latest deployment's; null without a deployment.
	Rollout       *RolloutStatus `json:"rollout"`
	Objects       []ObjectStatus `json:"objects"`
	Routes        []RouteStatus  `json:"routes"`
	OpenIncidents int            `json:"openIncidents"`
	CreatedAt     string         `json:"createdAt"`
}

// RolloutStatus is the latest deployment's rollout.
type RolloutStatus struct {
	Deployment int    `json:"deployment"`
	State      string `json:"state"`
	StartedAt  string `json:"startedAt"`
	Reason     string `json:"reason"`
}

// ObjectStatus is an object of the environment's target deployment, as the
// status shows it. Its JSON has the fields of its kind only:
//
//	image service     kind, name, runtime, desired, ready, instances
//	attached service  kind, name, runtime, sandbox, phase, restarts, lastExit, detail
//	addon             kind, name, type, version, plan, phase, provisioned, instances
//	job               kind, name, lastRun (null before its first run)
type ObjectStatus struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Runtime is a service's (spec.RuntimeImage, spec.RuntimeAttached).
	Runtime string `json:"runtime,omitempty"`
	// Desired and Ready count an image service's replicas.
	Desired int `json:"desired,omitempty"`
	Ready   int `json:"ready,omitempty"`
	// Type, Version and Plan are an addon's.
	Type    string `json:"type,omitempty"`
	Version string `json:"version,omitempty"`
	Plan    string `json:"plan,omitempty"`
	// Phase is an addon's instance's or an attached service's
	// (Phase…).
	Phase string `json:"phase,omitempty"`
	// Provisioned: an addon's provisioning ran for its options.
	Provisioned bool `json:"provisioned,omitempty"`
	// Sandbox, Restarts, LastExit and Detail are an attached service's.
	Sandbox  string `json:"sandbox,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
	LastExit *int   `json:"lastExit,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// Instances are an image service's or an addon's pods.
	Instances []InstanceStatus `json:"instances,omitempty"`
	// LastRun is a job's latest run.
	LastRun *JobRunStatus `json:"lastRun,omitempty"`
}

// MarshalJSON writes the fields of the object's kind, all of them.
func (o ObjectStatus) MarshalJSON() ([]byte, error) {
	inst := o.Instances
	if inst == nil {
		inst = []InstanceStatus{}
	}
	switch {
	case o.Kind == spec.KindService && o.Runtime == spec.RuntimeAttached:
		return json.Marshal(struct {
			Kind     string `json:"kind"`
			Name     string `json:"name"`
			Runtime  string `json:"runtime"`
			Sandbox  string `json:"sandbox"`
			Phase    string `json:"phase"`
			Restarts int    `json:"restarts"`
			LastExit *int   `json:"lastExit"`
			Detail   string `json:"detail"`
		}{o.Kind, o.Name, o.Runtime, o.Sandbox, o.Phase, o.Restarts, o.LastExit, o.Detail})
	case o.Kind == spec.KindService:
		return json.Marshal(struct {
			Kind      string           `json:"kind"`
			Name      string           `json:"name"`
			Runtime   string           `json:"runtime"`
			Desired   int              `json:"desired"`
			Ready     int              `json:"ready"`
			Instances []InstanceStatus `json:"instances"`
		}{o.Kind, o.Name, o.Runtime, o.Desired, o.Ready, inst})
	case o.Kind == spec.KindAddon:
		return json.Marshal(struct {
			Kind        string           `json:"kind"`
			Name        string           `json:"name"`
			Type        string           `json:"type"`
			Version     string           `json:"version"`
			Plan        string           `json:"plan"`
			Phase       string           `json:"phase"`
			Provisioned bool             `json:"provisioned"`
			Instances   []InstanceStatus `json:"instances"`
		}{o.Kind, o.Name, o.Type, o.Version, o.Plan, o.Phase, o.Provisioned, inst})
	case o.Kind == spec.KindJob:
		return json.Marshal(struct {
			Kind    string        `json:"kind"`
			Name    string        `json:"name"`
			LastRun *JobRunStatus `json:"lastRun"`
		}{o.Kind, o.Name, o.LastRun})
	}
	type plain ObjectStatus
	return json.Marshal(plain(o))
}

// InstanceStatus is a pod of an image service or an addon.
type InstanceStatus struct {
	// ID is the instance's sandbox row's ID.
	ID      string `json:"id"`
	Replica int    `json:"replica"`
	// Phase is the controller's (Phase…), State the row's client
	// state (the sandboxes' states).
	Phase      string `json:"phase"`
	State      string `json:"state"`
	Node       string `json:"node"`
	Address    string `json:"address"`
	Deployment int    `json:"deployment"`
	Restarts   int    `json:"restarts"`
	LastExit   *int   `json:"lastExit"`
	ReadySince string `json:"readySince"`
	Detail     string `json:"detail"`
	// Image is the image the instance runs, registry/repository@sha256:…
	// as its deployment resolved it.
	Image string `json:"image"`
}

// JobRunStatus is a run of a job.
type JobRunStatus struct {
	Deployment int `json:"deployment"`
	Attempt    int `json:"attempt"`
	// Phase is RunRunning, RunSucceeded or RunFailed.
	Phase      string `json:"phase"`
	ExitCode   *int   `json:"exitCode"`
	FinishedAt string `json:"finishedAt"`
	// Detail says why a run failed without an exit code ("the node did not
	// report an exit status", "timed out after 10m0s").
	Detail string `json:"detail"`
}

// RouteStatus is a route of the deployed deployment (the latest when none
// is deployed), its port resolved to a number.
type RouteStatus struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Host is the host it answers on, canonical (ADR 0045); "": every
	// host of the environment.
	Host    string `json:"host,omitempty"`
	Service string `json:"service"`
	Port    int    `json:"port"`
	// Door is DoorIngress for a route to an image service of a public
	// environment on a platform with an apps zone, which the ingress and
	// the route door both serve; DoorRoute for every other, which the
	// route door alone serves (with a route token).
	Door string `json:"door"`
	// URL is where the ingress serves an ingress route,
	// "https://<host><prefix>": at its host while the environment is
	// served there, a route without a host at the environment's first
	// address where it answers its path; "" for a route door route, and
	// for a route whose host is a wildcard or not served now.
	URL string `json:"url,omitempty"`
}

// The doors of a route (RouteStatus.Door).
const (
	DoorIngress = "ingress"
	DoorRoute   = "route"
)

// CloneRequest is POST /api/v1/environments/{env}/clone: a new
// environment whose deployment 1 is the source's deployment (or Spec).
type CloneRequest struct {
	Name string `json:"name"`
	// ID is the new environment's UUID; "": a new one.
	ID string `json:"id,omitempty"`
	// Deployment is the source's deployment to clone; nil: its current one.
	Deployment *int `json:"deployment,omitempty"`
	// Spec, when set, is applied instead of the source deployment's spec
	// (whose attached services a clone drops).
	Spec   json.RawMessage   `json:"spec,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	// Secrets are the new environment's secrets: copied from the source
	// (its latest version) or set.
	Secrets map[string]CloneSecret `json:"secrets,omitempty"`
	// Credentials are the new environment's credentials (ADR 0033): each
	// the source's latest version, copied as the clone's version 1.
	Credentials map[string]CloneCredential `json:"credentials,omitempty"`
	Author      string                     `json:"author,omitempty"`
	// Data asks for a copy of the addons' data, which is not available: an
	// error finding.
	Data bool `json:"data,omitempty"`
}

// CloneCredential is a credential of a clone: {"copy": true}, the only
// way a credential gets there with the clone.
type CloneCredential struct {
	Copy bool `json:"copy,omitempty"`
}

// CloneSecret is a secret of a clone: exactly one of {"copy": true} and
// {"value": "s"}.
type CloneSecret struct {
	Copy  bool    `json:"copy,omitempty"`
	Value *string `json:"value,omitempty"`
}

// IncidentList is GET /api/v1/environments/{env}/incidents[?open=1],
// newest first.
type IncidentList struct {
	Incidents []Incident `json:"incidents"`
}

// Incident is a condition of an object that needs a look.
type Incident struct {
	// ID is the incident's number, as a string.
	ID string `json:"id"`
	// Object is "<kind>/<name>".
	Object     string `json:"object"`
	Deployment int    `json:"deployment"`
	// Condition is Condition….
	Condition string `json:"condition"`
	OpenedAt  string `json:"openedAt"`
	// ClosedAt is "" while it is open.
	ClosedAt string           `json:"closedAt"`
	Summary  string           `json:"summary"`
	Evidence IncidentEvidence `json:"evidence"`
}

// IncidentEvidence is what the controller saw.
type IncidentEvidence struct {
	// Exits are the container's last exits.
	Exits []Exit `json:"exits,omitempty"`
	// LogTail is the end of its log.
	LogTail []string `json:"logTail,omitempty"`
	// Diff is a failed rollout's diff against the deployed deployment.
	Diff []spec.DiffEntry `json:"diff,omitempty"`
	// Placement is why an instance could not be placed.
	Placement string `json:"placement,omitempty"`
	// Pending is what a deployment past its deadline still waited for, and
	// the details of those objects' instances (credentials redacted).
	Pending []string `json:"pending,omitempty"`
}

// Exit is an exit of a container (env_instances.exits): when, and its
// code (null when the node reported none). A start that failed counts as
// an exit: notStarted is then why the container did not start.
type Exit struct {
	At         string `json:"at"`
	Code       *int   `json:"code"`
	NotStarted string `json:"notStarted,omitempty"`
}

// LogsAnswer is GET /api/v1/environments/{env}/objects/{kind}/{name}/logs.
type LogsAnswer struct {
	// Lines are the last lines, oldest first.
	Lines []string `json:"lines"`
	// Source is "container" (an image pod's main container) or "process"
	// (an attached service's process).
	Source string `json:"source"`
}

// PublicRequest is PUT /api/v1/environments/{env}/public: whether the
// ingress serves the environment (ADR 0021). Public is required.
// HostLabel, with public true, is the default host's label its project
// chooses (a DNS label with "--" in it; for a project that owns the apps
// zone also one without); without it the environment keeps its label, or
// gets one at its first publish. Access and Provider, only with public
// true, say who the ingress admits (ADR 0054).
type PublicRequest struct {
	Public    bool   `json:"public"`
	HostLabel string `json:"hostLabel,omitempty"`
	// Access is who the ingress admits (ADR 0054): AccessAnyone or
	// AccessVisitors; "" says nothing (see below).
	Access string `json:"access,omitempty"`
	// Provider, with access visitors, is one of the project's providers, or
	// ProviderFerun; "": ferun.
	Provider string `json:"provider,omitempty"`
}

// PublicAnswer is its answer.
type PublicAnswer struct {
	Public bool `json:"public"`
	// Hosts are the environment's hosts while it is public; empty
	// otherwise.
	Hosts []string `json:"hosts"`
	// Rolling: this call published the environment for the first time, so
	// its image services roll once, to name their ingress groups.
	Rolling bool `json:"rolling"`
	// Access and Provider are as EnvStatus has them, after the call.
	Access   string `json:"access"`   // AccessAnyone | AccessVisitors, always
	Provider string `json:"provider"` // with visitors its name ("ferun" for the builtin); "" with anyone
}

// RouteToken is POST /api/v1/environments/{env}/route-tokens's answer:
// the token, shown once (only its hash is kept).
type RouteToken struct {
	Token string `json:"token"`
}
