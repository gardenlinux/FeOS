// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gardenlinux/feos/go/ferun/api"
)

// Client is a client of one FeRun. It is safe for concurrent use.
type Client struct {
	base    string
	hc      *http.Client
	bearer  string
	project string
}

// Option sets up a Client.
type Option func(*Client)

// New is a client of the FeRun at baseURL ("https://run.fecore.dev").
func New(baseURL string, opts ...Option) *Client {
	c := &Client{base: strings.TrimSuffix(baseURL, "/"), hc: http.DefaultClient}
	for _, o := range opts {
		o(c)
	}
	return c
}

// WithHTTPClient sends every request through hc; a caller's transport
// may set Authorization, FeRun-Project and FeRun-Person itself.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.hc = hc
		}
	}
}

// WithBearer sets "Authorization: Bearer <token>" on every request (a
// project token; a static integration token in tests).
func WithBearer(token string) Option { return func(c *Client) { c.bearer = token } }

// WithProject sets FeRun-Project on every request.
func WithProject(name string) Option { return func(c *Client) { c.project = name } }

type ctxKey int

const (
	projectKey ctxKey = iota
	personKey
)

// InProject sets FeRun-Project for the calls made with ctx; it wins over
// WithProject. A caller whose transport sets the header uses neither.
func InProject(ctx context.Context, project string) context.Context {
	return context.WithValue(ctx, projectKey, project)
}

// ForPerson sets FeRun-Person, the subject of the person an
// integration acts for (ADR 0056), for the calls made with ctx.
func ForPerson(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, personKey, subject)
}

// Do sends one request: path is "/api/v1/…", query may be nil, body is
// marshalled as JSON when not nil, out is decoded leniently (unknown
// fields ignored) when not nil; *json.RawMessage keeps the body as it
// came. A status ≥ 400 returns *Error.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	_, err := c.DoStatus(ctx, method, path, query, body, out)
	return err
}

// DoStatus is Do that also answers the status of a success (201 vs 200).
func (c *Client) DoStatus(ctx context.Context, method, path string, query url.Values, body, out any) (int, error) {
	resp, err := c.send(ctx, method, path, query, body)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("%s %s: read the answer: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, refusal(resp.StatusCode, raw)
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if rm, ok := out.(*json.RawMessage); ok {
		*rm = append((*rm)[:0], raw...)
		return resp.StatusCode, nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("%s %s: the answer does not read: %w", method, path, err)
	}
	return resp.StatusCode, nil
}

// stream sends one request and answers its body as it comes, for a
// status below 400; the caller closes it.
func (c *Client) stream(ctx context.Context, method, path string, query url.Values) (io.ReadCloser, error) {
	resp, err := c.send(ctx, method, path, query, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusal))
		return nil, refusal(resp.StatusCode, raw)
	}
	return resp.Body, nil
}

// maxRefusal bounds what is read of a refused stream's body.
const maxRefusal = 1 << 20

func (c *Client) send(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%s %s: the request: %w", method, path, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	project := c.project
	if p, ok := ctx.Value(projectKey).(string); ok {
		project = p
	}
	if project != "" {
		req.Header.Set(api.ProjectHeader, project)
	}
	if p, ok := ctx.Value(personKey).(string); ok && p != "" {
		req.Header.Set(api.PersonHeader, p)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// Error is a refusal: the status and FeRun's body.
type Error struct {
	Status int
	Body   api.ErrorBody
	Raw    []byte
}

// Error is the body's error, else the status text.
func (e *Error) Error() string {
	if e.Body.Error != "" {
		return e.Body.Error
	}
	return fmt.Sprintf("%d %s", e.Status, http.StatusText(e.Status))
}

// refusal is the *Error of a status ≥ 400: its body decoded leniently,
// a body that is no JSON object its text.
func refusal(status int, raw []byte) *Error {
	e := &Error{Status: status, Raw: raw}
	if json.Unmarshal(raw, &e.Body) != nil {
		e.Body = api.ErrorBody{Error: strings.TrimSpace(string(raw))}
	}
	return e
}

// AsError is the refusal in err's chain.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool {
	e, ok := AsError(err)
	return ok && e.Status == http.StatusNotFound
}

// IsCode reports a refusal with the code.
func IsCode(err error, code string) bool {
	e, ok := AsError(err)
	return ok && e.Body.Code == code
}

// seg is a path segment, escaped.
func seg(s string) string { return url.PathEscape(s) }

// envPath is "/api/v1/environments/{env}" and the rest.
func envPath(env string, rest ...string) string {
	p := "/api/v1/environments/" + seg(env)
	for _, r := range rest {
		p += "/" + r
	}
	return p
}
