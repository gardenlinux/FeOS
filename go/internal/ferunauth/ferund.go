// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// This file is ferun-auth's client of ferund's API: the challenges and
// visits of ADR 0054 (class provider) and its own routes (class auth),
// with the service token of kind auth. Its types are the JSON of those
// routes, not ferund's packages, which ferun-auth does not link.

// ferundTimeout bounds every call to ferund.
const ferundTimeout = 10 * time.Second

// maxFerundBody bounds what is read of ferund's answer.
const maxFerundBody = 1 << 20

// challengeView is ferund's answer to POST /api/v1/challenges/open.
type challengeView struct {
	Environment struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Tenant string `json:"tenant"`
	} `json:"environment"`
	Host     string `json:"host"`
	Expired  bool   `json:"expired"`
	Answered bool   `json:"answered"`
	Restart  string `json:"restart"`
	Provider struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"provider"`
	OIDC *oidcProvider `json:"oidc"`
}

// oidcProvider is a tenant's provider of kind oidc: the challenge's
// "oidc", and GET /api/v1/oidc-providers/{tenant}/{provider}.
type oidcProvider struct {
	Tenant       string `json:"tenant"`
	Name         string `json:"name"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Rule         rule   `json:"rule"`
}

// acceptBody is POST /api/v1/challenges/accept's body.
type acceptBody struct {
	Challenge string `json:"challenge"`
	Subject   string `json:"subject"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
	Session   string `json:"session,omitempty"`
}

// answer is ferund's answer to an accept or a reject.
type answer struct {
	Redirect string `json:"redirect"`
	Admitted bool   `json:"admitted"`
}

// endBody is POST /api/v1/visits/end's body.
type endBody struct {
	Tenant   string `json:"tenant,omitempty"`
	Provider string `json:"provider"`
	Session  string `json:"session"`
}

// ferundError is an answer of ferund that is not 200: its status and its
// error code.
type ferundError struct {
	Status int
	Code   string
}

func (e *ferundError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("ferund answered HTTP %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("ferund answered HTTP %d", e.Status)
}

// statusOf is the status of a ferundError, 0 for any other error (ferund
// not reached, or an answer that does not parse).
func statusOf(err error) int {
	var fe *ferundError
	if errors.As(err, &fe) {
		return fe.Status
	}
	return 0
}

// codeOf is the error code of a ferundError.
func codeOf(err error) string {
	var fe *ferundError
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}

// refusedToken reports whether ferund refused the service token.
func refusedToken(err error) bool {
	s := statusOf(err)
	return s == http.StatusUnauthorized || s == http.StatusForbidden
}

// ferundClient calls ferund's API with the service token. Its HTTP client
// follows no redirect and dials ferund's address as configured: it is the
// installation's, not one others name.
type ferundClient struct {
	base, token string
	http        *http.Client
}

func newFerundClient(base, token string) *ferundClient {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return &ferundClient{base: base, token: token, http: &http.Client{
		Transport:     t,
		Timeout:       ferundTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// do sends a request (in as its JSON body, when not nil) and decodes a
// 200's JSON into out. Its errors name the method and path, never a body.
func (c *ferundClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A *url.Error names the URL, which holds no secret: the
		// challenge travels in the body.
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFerundBody))
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(b, &e)
		return fmt.Errorf("%s %s: %w", method, path, &ferundError{Status: resp.StatusCode, Code: e.Code})
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("%s %s: the answer is not JSON of its kind: %w", method, path, err)
		}
	}
	return nil
}

func (c *ferundClient) openChallenge(ctx context.Context, sealed string) (challengeView, error) {
	var v challengeView
	err := c.do(ctx, http.MethodPost, "/api/v1/challenges/open", map[string]string{"challenge": sealed}, &v)
	return v, err
}

func (c *ferundClient) accept(ctx context.Context, in acceptBody) (answer, error) {
	var a answer
	err := c.do(ctx, http.MethodPost, "/api/v1/challenges/accept", in, &a)
	return a, err
}

func (c *ferundClient) reject(ctx context.Context, sealed string) (answer, error) {
	var a answer
	err := c.do(ctx, http.MethodPost, "/api/v1/challenges/reject", map[string]string{"challenge": sealed}, &a)
	return a, err
}

func (c *ferundClient) oidcProvider(ctx context.Context, tenant, name string) (oidcProvider, error) {
	var p oidcProvider
	err := c.do(ctx, http.MethodGet, "/api/v1/oidc-providers/"+url.PathEscape(tenant)+"/"+url.PathEscape(name), nil, &p)
	return p, err
}

func (c *ferundClient) endVisits(ctx context.Context, in endBody) (int, error) {
	var out struct {
		Ended int `json:"ended"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/visits/end", in, &out)
	return out.Ended, err
}
