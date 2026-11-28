// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/gardenlinux/feos/go/ferun/spec"

// ErrorBody is the body of every refusal of FeRun's API (ADR 0031):
// {"error": text} and, for the refusals that have one, a code a client
// matches; the fields beyond error and code appear by code: role and
// scope with role_required, resource, limit and use with
// quota_exceeded, sandboxes and jobs with environment_in_use, findings
// with invalid_spec and current with base_mismatch.
type ErrorBody struct {
	Error    string `json:"error"`
	Code     string `json:"code,omitempty"`
	Role     string `json:"role,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Resource string `json:"resource,omitempty"`
	Limit    *int64 `json:"limit,omitempty"`
	Use      *int64 `json:"use,omitempty"`
	// Sandboxes and Jobs are an environment_in_use answer's.
	Sandboxes *[]string `json:"sandboxes,omitempty"`
	Jobs      *[]string `json:"jobs,omitempty"`
	// Findings are an invalid_spec answer's: every finding of the spec,
	// warnings and infos too.
	Findings []spec.Finding `json:"findings,omitempty"`
	// Current is a base_mismatch answer's: the environment's current
	// deployment, on which the client plans again.
	Current *int `json:"current,omitempty"`
}

// The codes of the refusals (ErrorBody.Code), which clients match.
const (
	// CodeRoleRequired: 403, the principal sees what it names but lacks
	// the role for what it asks (role, scope).
	CodeRoleRequired = "role_required"
	// CodeQuotaExceeded: 422, a create would pass one of its project's
	// limits (resource, limit, use).
	CodeQuotaExceeded = "quota_exceeded"
	// CodeEnvironmentInUse: 409, an environment's delete while sandboxes
	// or queued or running jobs are in it (sandboxes, jobs).
	CodeEnvironmentInUse = "environment_in_use"
	// CodeExpired: 410, the sandbox expired after its idle time (ADR
	// 0065): only reading and deleting it are left.
	CodeExpired = "expired"
	// CodeInvalidSpec: 422, the spec has error findings (findings).
	CodeInvalidSpec = "invalid_spec"
	// CodeBaseMismatch: 409, the request's base is not the current
	// deployment (current).
	CodeBaseMismatch = "base_mismatch"
	// CodeNameTaken: 409, the project has an environment of that name
	// with another ID.
	CodeNameTaken = "name_taken"
	// CodeIDTaken: 409, an environment of that ID exists with another
	// name or another owner.
	CodeIDTaken = "id_taken"
	// CodeHostLabelTaken: 409, the host label a project chose is another
	// project's or another environment's (PUT …/public).
	CodeHostLabelTaken = "host_label_taken"
	// CodeNoSecret: 404 or 422, a secret the environment does not have.
	CodeNoSecret = "no_secret"
)

// The scopes of a role_required refusal (ErrorBody.Scope).
const (
	ScopeProject     = "project"
	ScopeEnvironment = "environment"
)
