// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/gardenlinux/feos/go/ferun/api"
)

// The project's domains (ADR 0021) and their bindings to environments.

// Domains is GET /api/v1/domains.
func (c *Client) Domains(ctx context.Context) (api.DomainList, error) {
	var out api.DomainList
	return out, c.Do(ctx, http.MethodGet, "/api/v1/domains", nil, nil, &out)
}

// Domain is GET /api/v1/domains/{id}.
func (c *Client) Domain(ctx context.Context, id string) (api.Domain, error) {
	var out api.Domain
	return out, c.Do(ctx, http.MethodGet, "/api/v1/domains/"+seg(id), nil, nil, &out)
}

// CreateDomain is POST /api/v1/domains: a claim on a name.
func (c *Client) CreateDomain(ctx context.Context, req api.CreateDomainRequest) (api.Domain, error) {
	var out api.Domain
	return out, c.Do(ctx, http.MethodPost, "/api/v1/domains", nil, req, &out)
}

// DeleteDomain is DELETE /api/v1/domains/{id}.
func (c *Client) DeleteDomain(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, "/api/v1/domains/"+seg(id), nil, nil, nil)
}

// BindDomain is PUT /api/v1/domains/{id}/environment.
func (c *Client) BindDomain(ctx context.Context, id string, req api.BindDomainRequest) (api.Domain, error) {
	var out api.Domain
	return out, c.Do(ctx, http.MethodPut, "/api/v1/domains/"+seg(id)+"/environment", nil, req, &out)
}

// UnbindDomain is DELETE /api/v1/domains/{id}/environment.
func (c *Client) UnbindDomain(ctx context.Context, id string) (api.Domain, error) {
	var out api.Domain
	return out, c.Do(ctx, http.MethodDelete, "/api/v1/domains/"+seg(id)+"/environment", nil, nil, &out)
}

// CheckDomain is POST /api/v1/domains/{id}/check: its records looked up
// now.
func (c *Client) CheckDomain(ctx context.Context, id string) (api.Domain, error) {
	var out api.Domain
	return out, c.Do(ctx, http.MethodPost, "/api/v1/domains/"+seg(id)+"/check", nil, nil, &out)
}
