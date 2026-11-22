// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/gardenlinux/feos/go/ferun/api"
)

// The environments' routes (ADR 0018): one method per route; env is an
// environment's ID or its name, as the API takes it.

// Environments is GET /api/v1/environments, those with every label of
// label (none: all).
func (c *Client) Environments(ctx context.Context, label map[string]string) (api.EnvList, error) {
	var q url.Values
	for _, k := range slices.Sorted(maps.Keys(label)) {
		if q == nil {
			q = url.Values{}
		}
		q.Add("label", k+"="+label[k])
	}
	var out api.EnvList
	return out, c.Do(ctx, http.MethodGet, "/api/v1/environments", q, nil, &out)
}

// Environment is GET /api/v1/environments/{env}.
func (c *Client) Environment(ctx context.Context, env string) (api.EnvStatus, error) {
	var out api.EnvStatus
	return out, c.Do(ctx, http.MethodGet, envPath(env), nil, nil, &out)
}

// CreateEnvironment is POST /api/v1/environments: 201 for a new one, 200
// for the same ID and name again.
func (c *Client) CreateEnvironment(ctx context.Context, req api.CreateEnvRequest) (api.EnvStatus, error) {
	var out api.EnvStatus
	return out, c.Do(ctx, http.MethodPost, "/api/v1/environments", nil, req, &out)
}

// RenameEnvironment is PATCH /api/v1/environments/{env}.
func (c *Client) RenameEnvironment(ctx context.Context, env string, req api.RenameEnvRequest) (api.EnvStatus, error) {
	var out api.EnvStatus
	return out, c.Do(ctx, http.MethodPatch, envPath(env), nil, req, &out)
}

// DeleteEnvironment is DELETE /api/v1/environments/{env} (202: being
// deleted).
func (c *Client) DeleteEnvironment(ctx context.Context, env string) error {
	return c.Do(ctx, http.MethodDelete, envPath(env), nil, nil, nil)
}

// PlanSpec is POST /api/v1/environments/plan: the spec judged without an
// environment.
func (c *Client) PlanSpec(ctx context.Context, spec json.RawMessage) (api.PlanSpecAnswer, error) {
	var out api.PlanSpecAnswer
	return out, c.Do(ctx, http.MethodPost, "/api/v1/environments/plan", nil, api.PlanSpecRequest{Spec: spec}, &out)
}

// Plan is POST /api/v1/environments/{env}/plan.
func (c *Client) Plan(ctx context.Context, env string, req api.PlanRequest) (api.PlanAnswer, error) {
	var out api.PlanAnswer
	return out, c.Do(ctx, http.MethodPost, envPath(env, "plan"), nil, req, &out)
}

// Apply is POST /api/v1/environments/{env}/deployments: 201 with the new
// deployment, or 200 Unchanged with the current one.
func (c *Client) Apply(ctx context.Context, env string, req api.ApplyRequest) (api.ApplyAnswer, error) {
	var out api.ApplyAnswer
	return out, c.Do(ctx, http.MethodPost, envPath(env, "deployments"), nil, req, &out)
}

// Deployments is GET /api/v1/environments/{env}/deployments, newest
// first; limit 0 is FeRun's default.
func (c *Client) Deployments(ctx context.Context, env string, limit int) (api.DeploymentList, error) {
	var q url.Values
	if limit > 0 {
		q = url.Values{"limit": {strconv.Itoa(limit)}}
	}
	var out api.DeploymentList
	return out, c.Do(ctx, http.MethodGet, envPath(env, "deployments"), q, nil, &out)
}

// Deployment is GET /api/v1/environments/{env}/deployments/{n}.
func (c *Client) Deployment(ctx context.Context, env string, n int) (api.DeploymentDetail, error) {
	var out api.DeploymentDetail
	return out, c.Do(ctx, http.MethodGet, envPath(env, "deployments", strconv.Itoa(n)), nil, nil, &out)
}

// Revert is POST /api/v1/environments/{env}/deployments/{n}/revert.
func (c *Client) Revert(ctx context.Context, env string, n int, req api.RevertRequest) (api.ApplyAnswer, error) {
	var out api.ApplyAnswer
	return out, c.Do(ctx, http.MethodPost, envPath(env, "deployments", strconv.Itoa(n), "revert"), nil, req, &out)
}

// Clone is POST /api/v1/environments/{env}/clone: the new environment.
func (c *Client) Clone(ctx context.Context, env string, req api.CloneRequest) (api.EnvStatus, error) {
	var out api.EnvStatus
	return out, c.Do(ctx, http.MethodPost, envPath(env, "clone"), nil, req, &out)
}

// Tags is GET /api/v1/environments/{env}/tags.
func (c *Client) Tags(ctx context.Context, env string) (api.TagList, error) {
	var out api.TagList
	return out, c.Do(ctx, http.MethodGet, envPath(env, "tags"), nil, nil, &out)
}

// Tag is GET /api/v1/environments/{env}/tags/{tag}.
func (c *Client) Tag(ctx context.Context, env, tag string) (api.Tag, error) {
	var out api.Tag
	return out, c.Do(ctx, http.MethodGet, envPath(env, "tags", seg(tag)), nil, nil, &out)
}

// PutTag is PUT /api/v1/environments/{env}/tags/{tag}.
func (c *Client) PutTag(ctx context.Context, env, tag string, req api.PutTagRequest) (api.Tag, error) {
	var out api.Tag
	return out, c.Do(ctx, http.MethodPut, envPath(env, "tags", seg(tag)), nil, req, &out)
}

// DeleteTag is DELETE /api/v1/environments/{env}/tags/{tag}.
func (c *Client) DeleteTag(ctx context.Context, env, tag string) error {
	return c.Do(ctx, http.MethodDelete, envPath(env, "tags", seg(tag)), nil, nil, nil)
}

// Incidents is GET /api/v1/environments/{env}/incidents, the open ones
// alone with open.
func (c *Client) Incidents(ctx context.Context, env string, open bool) (api.IncidentList, error) {
	var q url.Values
	if open {
		q = url.Values{"open": {"1"}}
	}
	var out api.IncidentList
	return out, c.Do(ctx, http.MethodGet, envPath(env, "incidents"), q, nil, &out)
}

// Incident is GET /api/v1/environments/{env}/incidents/{id}.
func (c *Client) Incident(ctx context.Context, env, id string) (api.Incident, error) {
	var out api.Incident
	return out, c.Do(ctx, http.MethodGet, envPath(env, "incidents", seg(id)), nil, nil, &out)
}

// ObjectLogs is GET /api/v1/environments/{env}/objects/{kind}/{name}/logs
// (q: lines, replica).
func (c *Client) ObjectLogs(ctx context.Context, env, kind, name string, q url.Values) (api.LogsAnswer, error) {
	var out api.LogsAnswer
	return out, c.Do(ctx, http.MethodGet, envPath(env, "objects", seg(kind), seg(name), "logs"), q, nil, &out)
}

// RestartService is POST /api/v1/environments/{env}/services/{name}/restart.
func (c *Client) RestartService(ctx context.Context, env, name string) error {
	return c.Do(ctx, http.MethodPost, envPath(env, "services", seg(name), "restart"), nil, nil, nil)
}

// RunJob is POST /api/v1/environments/{env}/jobs/{name}/run.
func (c *Client) RunJob(ctx context.Context, env, name string) error {
	return c.Do(ctx, http.MethodPost, envPath(env, "jobs", seg(name), "run"), nil, nil, nil)
}

// CreateRouteToken is POST /api/v1/environments/{env}/route-tokens: the
// token, shown once.
func (c *Client) CreateRouteToken(ctx context.Context, env string) (api.RouteToken, error) {
	var out api.RouteToken
	return out, c.Do(ctx, http.MethodPost, envPath(env, "route-tokens"), nil, nil, &out)
}

// RevokeRouteTokens is DELETE /api/v1/environments/{env}/route-tokens.
func (c *Client) RevokeRouteTokens(ctx context.Context, env string) error {
	return c.Do(ctx, http.MethodDelete, envPath(env, "route-tokens"), nil, nil, nil)
}

// SetPublic is PUT /api/v1/environments/{env}/public.
func (c *Client) SetPublic(ctx context.Context, env string, req api.PublicRequest) (api.PublicAnswer, error) {
	var out api.PublicAnswer
	return out, c.Do(ctx, http.MethodPut, envPath(env, "public"), nil, req, &out)
}

// Addons is GET /api/v1/addons: the catalog.
func (c *Client) Addons(ctx context.Context) (api.AddonCatalog, error) {
	var out api.AddonCatalog
	return out, c.Do(ctx, http.MethodGet, "/api/v1/addons", nil, nil, &out)
}

// Quota is GET /api/v1/project/quota.
func (c *Client) Quota(ctx context.Context) (api.Quota, error) {
	var out api.Quota
	return out, c.Do(ctx, http.MethodGet, "/api/v1/project/quota", nil, nil, &out)
}
