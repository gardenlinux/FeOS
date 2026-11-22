// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/gardenlinux/feos/go/ferun/api"
)

// The people answers (FeOS ADR 0056, 0060): who the caller is, its
// project, the project's members and integrations, the registered
// integrations, and an environment's protection.

// Me is GET /auth/me: the caller by its credential, and the projects it
// reaches with its effective roles; with ForPerson, those of the person
// named too.
func (c *Client) Me(ctx context.Context) (api.Me, error) {
	var out api.Me
	return out, c.Do(ctx, http.MethodGet, "/auth/me", nil, nil, &out)
}

// Project is GET /api/v1/project: the caller's role in the project and
// the environments it reads beside it.
func (c *Client) Project(ctx context.Context) (api.Project, error) {
	var out api.Project
	return out, c.Do(ctx, http.MethodGet, "/api/v1/project", url.Values{"filter": {"environments"}}, nil, &out)
}

// DeleteProject is DELETE /api/v1/project {name}: the project goes, with
// everything in it (202). name is the project's name again; the caller
// needs Me's Delete for it.
func (c *Client) DeleteProject(ctx context.Context, name string) (api.DeleteProjectAnswer, error) {
	var out api.DeleteProjectAnswer
	return out, c.Do(ctx, http.MethodDelete, "/api/v1/project", nil, api.DeleteProjectRequest{Name: name}, &out)
}

// Members is GET /api/v1/project/members.
func (c *Client) Members(ctx context.Context) (api.Members, error) {
	var out api.Members
	return out, c.Do(ctx, http.MethodGet, "/api/v1/project/members", nil, nil, &out)
}

// Integrations is GET /api/v1/integrations (a person's session).
func (c *Client) Integrations(ctx context.Context) (api.IntegrationList, error) {
	var out api.IntegrationList
	return out, c.Do(ctx, http.MethodGet, "/api/v1/integrations", nil, nil, &out)
}

// SetProtected is PUT /api/v1/environments/{env}/protected.
func (c *Client) SetProtected(ctx context.Context, env string, protected bool) (api.ProtectedAnswer, error) {
	var out api.ProtectedAnswer
	return out, c.Do(ctx, http.MethodPut, "/api/v1/environments/"+url.PathEscape(env)+"/protected", nil,
		api.ProtectedRequest{Protected: protected}, &out)
}

// Principals is GET /api/v1/environments/{env}/principals.
func (c *Client) Principals(ctx context.Context, env string) (api.Principals, error) {
	var out api.Principals
	return out, c.Do(ctx, http.MethodGet, "/api/v1/environments/"+url.PathEscape(env)+"/principals", nil, nil, &out)
}
