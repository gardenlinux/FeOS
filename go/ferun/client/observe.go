// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/gardenlinux/feos/go/ferun/api"
)

// What an environment's workloads are and do, and the sink's reads (ADR
// 0026) at the same base URL with the same headers. The sink's answers
// stay raw: a caller passes them through.

// Identities is GET /api/v1/environments/{env}/identities.
func (c *Client) Identities(ctx context.Context, env string) (api.Identities, error) {
	var out api.Identities
	return out, c.Do(ctx, http.MethodGet, envPath(env, "identities"), nil, nil, &out)
}

// Traffic is GET /api/v1/environments/{env}/traffic (q: window).
func (c *Client) Traffic(ctx context.Context, env string, q url.Values) (api.Traffic, error) {
	var out api.Traffic
	return out, c.Do(ctx, http.MethodGet, envPath(env, "traffic"), q, nil, &out)
}

// BindingUsage is GET /api/v1/environments/{env}/bindings/usage (q:
// window).
func (c *Client) BindingUsage(ctx context.Context, env string, q url.Values) (api.BindingUsage, error) {
	var out api.BindingUsage
	return out, c.Do(ctx, http.MethodGet, envPath(env, "bindings", "usage"), q, nil, &out)
}

// Logs is the sink's GET /api/v1/logs.
func (c *Client) Logs(ctx context.Context, q url.Values) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.Do(ctx, http.MethodGet, "/api/v1/logs", q, nil, &out)
}

// LogStreams is the sink's GET /api/v1/log-streams.
func (c *Client) LogStreams(ctx context.Context, q url.Values) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.Do(ctx, http.MethodGet, "/api/v1/log-streams", q, nil, &out)
}

// FollowLogs is the sink's GET /api/v1/logs?follow: the stream as it
// comes, until ctx ends or the caller closes it.
func (c *Client) FollowLogs(ctx context.Context, q url.Values) (io.ReadCloser, error) {
	f := url.Values{}
	for k, vs := range q {
		f[k] = vs
	}
	if !f.Has("follow") {
		f.Set("follow", "1")
	}
	return c.stream(ctx, http.MethodGet, "/api/v1/logs", f)
}

// Traces is the sink's GET /api/v1/jaeger/api/traces.
func (c *Client) Traces(ctx context.Context, q url.Values) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.Do(ctx, http.MethodGet, "/api/v1/jaeger/api/traces", q, nil, &out)
}

// Trace is the sink's GET /api/v1/jaeger/api/traces/{id}.
func (c *Client) Trace(ctx context.Context, id string, q url.Values) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.Do(ctx, http.MethodGet, "/api/v1/jaeger/api/traces/"+seg(id), q, nil, &out)
}
