// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"net/http"

	"github.com/gardenlinux/feos/go/internal/env/spec"
)

// The request and answer bodies of the environments' REST routes (the
// contract's §4.9 and §5), as the API reads and writes them: camelCase,
// times as RFC 3339 in seconds, UTC ("" for none), deployment numbers as
// JSON numbers, incident IDs as strings. Answers carry every field, so
// that clients see the same shape every time.

// CreateEnvRequest is POST /api/v1/environments.
type CreateEnvRequest struct {
	// Name is a DNS label, never a UUID, unique per tenant.
	Name string `json:"name"`
	// ID is the environment's UUID; "": a new one. Creating the same ID
	// and name again answers 200 with the environment.
	ID     string            `json:"id,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
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
	// State is the latest deployment's state (model.Deployment…),
	// model.EnvNone without a deployment, model.EnvDeleting.
	State string `json:"state"`
	// Public, Hosts and HostLabel are as EnvStatus has them.
	Public        bool     `json:"public"`
	Hosts         []string `json:"hosts"`
	HostLabel     string   `json:"hostLabel,omitempty"`
	OpenIncidents int      `json:"openIncidents"`
	CreatedAt     string   `json:"createdAt"`
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
	// Public: its tenant made it public (PUT …/public), and the ingress
	// serves its image services' routes without a route token (ADR 0021).
	Public bool `json:"public"`
	// Hosts are the hosts the ingress serves it at while it is public: its
	// served domains in the order they were bound, else its default host
	// under the apps zone while the platform has one; empty otherwise, and
	// while it is being deleted.
	Hosts []string `json:"hosts"`
	// HostLabel is the label of its default host, given at its first
	// publish or chosen by its tenant; empty before. It stays while a
	// domain is its host.
	HostLabel string `json:"hostLabel,omitempty"`
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
	// (model.Phase…).
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
	// Phase is the controller's (model.Phase…), State the row's client
	// state (model.State…).
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
	// Phase is model.RunRunning, RunSucceeded or RunFailed.
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
	Name    string `json:"name"`
	Path    string `json:"path"`
	Service string `json:"service"`
	Port    int    `json:"port"`
	// Door is DoorIngress for a route to an image service of a public
	// environment on a platform with an apps zone, which the ingress and
	// the route door both serve; DoorRoute for every other, which the
	// route door alone serves (with a route token).
	Door string `json:"door"`
	// URL is where the ingress serves an ingress route:
	// "https://<host><prefix>"; "" for a route door route.
	URL string `json:"url,omitempty"`
}

// The doors of a route (RouteStatus.Door).
const (
	DoorIngress = "ingress"
	DoorRoute   = "route"
)

// PlanSpecRequest is POST /api/v1/environments/plan: validation without
// an environment.
type PlanSpecRequest struct {
	Spec json.RawMessage `json:"spec"`
}

// PlanSpecAnswer is its answer.
type PlanSpecAnswer struct {
	// Valid: no error finding.
	Valid    bool           `json:"valid"`
	Findings []spec.Finding `json:"findings"`
}

// PlanRequest is POST /api/v1/environments/{env}/plan.
type PlanRequest struct {
	Spec json.RawMessage `json:"spec"`
	// Base is the deployment to diff against; nil: the current one.
	Base *int `json:"base,omitempty"`
}

// PlanAnswer is a plan's answer.
type PlanAnswer struct {
	Valid    bool             `json:"valid"`
	Findings []spec.Finding   `json:"findings"`
	Diff     []spec.DiffEntry `json:"diff"`
	// Base is the deployment the diff is against, Current the environment's
	// current deployment (0: none); an apply passes Current as its base.
	Base    int `json:"base"`
	Current int `json:"current"`
}

// ApplyRequest is POST /api/v1/environments/{env}/deployments.
type ApplyRequest struct {
	Spec json.RawMessage `json:"spec"`
	// Base must be the environment's current deployment (0: none), or the
	// apply is refused (BaseMismatchError).
	Base    int    `json:"base"`
	Author  string `json:"author,omitempty"`
	Message string `json:"message,omitempty"`
	// Annotations are the client's, stored with the deployment: a string map
	// of at most 4 KiB ("fecode.dev/revision", …).
	Annotations map[string]string `json:"annotations,omitempty"`
}

// DeploymentList is GET /api/v1/environments/{env}/deployments, newest first.
type DeploymentList struct {
	Deployments []DeploymentInfo `json:"deployments"`
}

// DeploymentInfo is a deployment without its spec.
type DeploymentInfo struct {
	Number int `json:"number"`
	// State is model.Deployment….
	State string `json:"state"`
	// Origin is model.Origin…; OriginRef the reverted deployment's number,
	// the cloned "<env id>/<number>" or the object "<kind>/<name>".
	Origin      string            `json:"origin"`
	OriginRef   string            `json:"originRef"`
	Author      string            `json:"author"`
	Message     string            `json:"message"`
	Annotations map[string]string `json:"annotations"`
	Base        int               `json:"base"`
	// Reason is a failed deployment's.
	Reason     string `json:"reason"`
	CreatedAt  string `json:"createdAt"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	// Tags are the tags on it.
	Tags []string `json:"tags"`
}

// DeploymentDetail is GET /api/v1/environments/{env}/deployments/{n}: the
// deployment, its normalised spec and its diff against deployment n-1.
type DeploymentDetail struct {
	DeploymentInfo
	Spec json.RawMessage  `json:"spec"`
	Diff []spec.DiffEntry `json:"diff"`
}

// ApplyAnswer is the answer of an apply, a revert and an object put or
// delete: 201 with the new deployment, or 200 with the current one and
// Unchanged when the normalised spec equals the current deployment's (and
// that one did not fail).
type ApplyAnswer struct {
	DeploymentInfo
	Unchanged bool             `json:"unchanged,omitempty"`
	Diff      []spec.DiffEntry `json:"diff"`
	Findings  []spec.Finding   `json:"findings"`
}

// StatusCode is the answer's HTTP status.
func (a ApplyAnswer) StatusCode() int {
	if a.Unchanged {
		return http.StatusOK
	}
	return http.StatusCreated
}

// RevertRequest is POST /api/v1/environments/{env}/deployments/{n}/revert.
type RevertRequest struct {
	Base    int    `json:"base"`
	Author  string `json:"author,omitempty"`
	Message string `json:"message,omitempty"`
}

// PutObjectRequest is PUT /api/v1/environments/{env}/objects/{kind}/{name}:
// the current deployment's spec with the object set.
type PutObjectRequest struct {
	// Object is the object as a spec has it under its kind's map.
	Object  json.RawMessage `json:"object"`
	Base    int             `json:"base"`
	Author  string          `json:"author,omitempty"`
	Message string          `json:"message,omitempty"`
	// DryRun plans instead of applying.
	DryRun bool `json:"dryRun,omitempty"`
}

// DeleteObjectRequest is DELETE
// /api/v1/environments/{env}/objects/{kind}/{name}?base=&dryRun=1[&author=&message=],
// read from the query.
type DeleteObjectRequest struct {
	Base    int
	DryRun  bool
	Author  string
	Message string
}

// ObjectAnswer is an object put's or delete's answer: a plan's (DryRun)
// or an apply's.
type ObjectAnswer struct {
	DryRun bool
	Plan   PlanAnswer
	Apply  ApplyAnswer
}

// MarshalJSON writes the plan or the apply answer.
func (o ObjectAnswer) MarshalJSON() ([]byte, error) {
	if o.DryRun {
		return json.Marshal(o.Plan)
	}
	return json.Marshal(o.Apply)
}

// StatusCode is the answer's HTTP status.
func (o ObjectAnswer) StatusCode() int {
	if o.DryRun {
		return http.StatusOK
	}
	return o.Apply.StatusCode()
}

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
	Author  string                 `json:"author,omitempty"`
	// Data asks for a copy of the addons' data, which is not available: an
	// error finding.
	Data bool `json:"data,omitempty"`
}

// CloneSecret is a secret of a clone: exactly one of {"copy": true} and
// {"value": "s"}.
type CloneSecret struct {
	Copy  bool    `json:"copy,omitempty"`
	Value *string `json:"value,omitempty"`
}

// TagList is GET /api/v1/environments/{env}/tags.
type TagList struct {
	Tags []Tag `json:"tags"`
}

// Tag is a movable name of a deployment.
type Tag struct {
	Tag        string `json:"tag"`
	Deployment int    `json:"deployment"`
	MovedBy    string `json:"movedBy"`
	MovedAt    string `json:"movedAt"`
}

// PutTagRequest is PUT /api/v1/environments/{env}/tags/{tag}.
type PutTagRequest struct {
	Deployment int    `json:"deployment"`
	Author     string `json:"author,omitempty"`
}

// SecretList is GET /api/v1/environments/{env}/secrets: the tenant's
// secrets of the environment at their latest versions (never a value,
// never a system secret).
type SecretList struct {
	Secrets []SecretInfo `json:"secrets"`
}

// SecretInfo is a secret's latest version.
type SecretInfo struct {
	Name      string `json:"name"`
	Version   int    `json:"version"`
	CreatedBy string `json:"createdBy"`
	CreatedAt string `json:"createdAt"`
}

// PutSecretRequest is PUT /api/v1/environments/{env}/secrets/{name}: a new
// version.
type PutSecretRequest struct {
	Value  string `json:"value"`
	Author string `json:"author,omitempty"`
}

// SecretVersion is a secret put's answer.
type SecretVersion struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
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
	// Condition is model.Condition….
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
// code (null when the node reported none).
type Exit struct {
	At   string `json:"at"`
	Code *int   `json:"code"`
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
// HostLabel, with public true, is the default host's label its tenant
// chooses (model.ChosenHostLabel: a DNS label with "--" in it; for a
// tenant that owns the apps zone also model.PlainHostLabel, one without);
// without it the environment keeps its label, or gets one at its first
// publish.
type PublicRequest struct {
	Public    bool   `json:"public"`
	HostLabel string `json:"hostLabel,omitempty"`
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
}

// RouteToken is POST /api/v1/environments/{env}/route-tokens's answer:
// the token, shown once (only its hash is kept).
type RouteToken struct {
	Token string `json:"token"`
}
