// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gardenlinux/feos/go/ferun/api"
)

// An environment's secrets of both kinds (ADR 0055).

// Secrets is GET /api/v1/environments/{env}/secrets: every secret at its
// latest version, never a value.
func (c *Client) Secrets(ctx context.Context, env string) (api.SecretList, error) {
	var out api.SecretList
	return out, c.Do(ctx, http.MethodGet, envPath(env, "secrets"), nil, nil, &out)
}

// Secret is GET /api/v1/environments/{env}/secrets/{name}.
func (c *Client) Secret(ctx context.Context, env, name string) (api.SecretInfo, error) {
	var out api.SecretInfo
	return out, c.Do(ctx, http.MethodGet, envPath(env, "secrets", seg(name)), nil, nil, &out)
}

// PutSecret is PUT /api/v1/environments/{env}/secrets/{name}: the version,
// and whether it was made (201) or the same definition again (200).
// SecretVersion.Created is not in the JSON; the status carries it, and
// PutSecret sets it.
func (c *Client) PutSecret(ctx context.Context, env, name string, req api.PutSecretRequest) (api.SecretVersion, bool,
	error) {
	var out api.SecretVersion
	status, err := c.DoStatus(ctx, http.MethodPut, envPath(env, "secrets", seg(name)), nil, req, &out)
	if err != nil {
		return api.SecretVersion{}, false, err
	}
	out.Created = status == http.StatusCreated
	return out, out.Created, nil
}

// CheckSecret is POST /api/v1/environments/{env}/secrets/{name}/check: a
// request through the HTTP secret's binding, its answer as it came.
func (c *Client) CheckSecret(ctx context.Context, env, name string, req any) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.Do(ctx, http.MethodPost, envPath(env, "secrets", seg(name), "check"), nil, req, &out)
}

// DeleteSecret is DELETE /api/v1/environments/{env}/secrets/{name}.
func (c *Client) DeleteSecret(ctx context.Context, env, name string) error {
	return c.Do(ctx, http.MethodDelete, envPath(env, "secrets", seg(name)), nil, nil, nil)
}
