// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"

	"github.com/gardenlinux/feos/go/ferun/spec"
)

// The plans of a spec, with an environment and without one (ADR 0032).

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
	// Images are the pins, as PlanAnswer's.
	Images map[string]string `json:"images"`
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
	// Images are the pins the plan resolved (ADR 0032): every image of an
	// image service or a job that is not a digest reference, as written,
	// to the digest reference it resolved to
	// ("<registry>/<repository>@sha256:…"), which the normalised spec
	// runs. A client that writes them into its spec (image the pin,
	// imageAsWritten the key) applies what the plan showed, even when a
	// tag moves before the apply. An image that did not resolve is not a
	// key (the plan warned at its path); a digest reference is never one
	// (it stays as written). {} when the spec did not parse.
	Images map[string]string `json:"images"`
	// Reachability is what the normalised spec admits (ADR 0034,
	// revised): its edges, each object's egress, each route's clients;
	// empty lists when the spec did not parse.
	Reachability spec.Reachability `json:"reachability"`
}
