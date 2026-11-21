// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"

	"github.com/gardenlinux/feos/go/ferun/spec"
)

// An environment's deployments, its objects' changes and its tags.

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
	// State is Deployment….
	State string `json:"state"`
	// Origin is Origin…; OriginRef the reverted deployment's number,
	// the cloned "<env id>/<number>" or the object "<kind>/<name>".
	Origin    string `json:"origin"`
	OriginRef string `json:"originRef"`
	Author    string `json:"author"`
	// MadeBy is the principal that made it, as ferund saw it (ADR 0031):
	// "person:<id>", "token:<hash>", "basic:<user>", "integration:<id>" or
	// "integration:<id>/person:<id>" (ADR 0056); "" for ferund's own,
	// then left out. Author is the client's free text beside it.
	MadeBy      string            `json:"madeBy,omitempty"`
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
// deployment, its normalised spec, its diff against deployment n-1 and
// what its spec admits (ADR 0034, revised).
type DeploymentDetail struct {
	DeploymentInfo
	Spec         json.RawMessage   `json:"spec"`
	Diff         []spec.DiffEntry  `json:"diff"`
	Reachability spec.Reachability `json:"reachability"`
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
