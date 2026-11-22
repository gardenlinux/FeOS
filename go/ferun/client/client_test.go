// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gardenlinux/feos/go/ferun/api"
	"github.com/gardenlinux/feos/go/ferun/spec"
)

// seen is a request as the test server saw it.
type seen struct {
	method, path, query string
	header              http.Header
	body                string
}

// server answers each request with answer's status and body, recording
// the requests.
func server(t *testing.T, answer func(r *http.Request) (int, string)) (*httptest.Server, func() []seen) {
	t.Helper()
	var mu sync.Mutex
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), string(b)})
		mu.Unlock()
		status, body := answer(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seen {
		mu.Lock()
		defer mu.Unlock()
		return append([]seen(nil), got...)
	}
}

// The headers: the bearer, the project (the call's over the client's)
// and the person; JSON bodies; lenient answers.
func TestHeadersAndBodies(t *testing.T) {
	srv, got := server(t, func(r *http.Request) (int, string) {
		return http.StatusOK, `{"id": "e1", "name": "stage", "protected": true, "fieldOfLater": 1}`
	})
	c := New(srv.URL+"/", WithBearer("tok"), WithProject("kino"))
	st, err := c.Environment(context.Background(), "stage")
	if err != nil || st.ID != "e1" || !st.Protected {
		t.Fatalf("%+v %v", st, err)
	}
	ctx := ForPerson(InProject(context.Background(), "other"), "sub-1")
	if _, err := c.CreateEnvironment(ctx, api.CreateEnvRequest{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	g := got()
	if g[0].method != "GET" || g[0].path != "/api/v1/environments/stage" || g[0].header.Get("Authorization") != "Bearer tok" ||
		g[0].header.Get(api.ProjectHeader) != "kino" || g[0].header.Get(api.PersonHeader) != "" || g[0].body != "" {
		t.Fatalf("first %+v", g[0])
	}
	if g[1].method != "POST" || g[1].header.Get(api.ProjectHeader) != "other" || g[1].header.Get(api.PersonHeader) != "sub-1" ||
		g[1].header.Get("Content-Type") != "application/json" || g[1].body != `{"name":"new"}` {
		t.Fatalf("second %+v", g[1])
	}
	// A caller's transport sets the headers itself: the client sets none.
	bare := New(srv.URL, WithHTTPClient(srv.Client()))
	if _, err := bare.Environments(context.Background(), map[string]string{"b": "2", "a": "1"}); err != nil {
		t.Fatal(err)
	}
	last := got()[2]
	if last.header.Get("Authorization") != "" || last.header.Get(api.ProjectHeader) != "" ||
		last.query != "label=a%3D1&label=b%3D2" {
		t.Fatalf("bare %+v", last)
	}
}

// A refusal is an *Error with FeRun's body; a body that is no JSON is
// its text.
func TestRefusals(t *testing.T) {
	answers := map[string]struct {
		status int
		body   string
	}{
		"/api/v1/environments/a/plan":        {422, `{"error": "the spec has an error", "code": "invalid_spec", "findings": [{"severity": "error", "path": "services.x", "problem": "p", "remedy": "r"}]}`},
		"/api/v1/environments/a/deployments": {409, `{"error": "base 1 is not 2", "code": "base_mismatch", "current": 2}`},
		"/api/v1/environments/a":             {404, `{"error": "no such environment"}`},
		"/api/v1/environments/a/tags/t":      {403, `{"error": "e", "code": "role_required", "role": "write", "scope": "environment"}`},
		"/api/v1/environments/a/secrets/s":   {502, "bad gateway\n"},
	}
	srv, _ := server(t, func(r *http.Request) (int, string) {
		a := answers[r.URL.Path]
		return a.status, a.body
	})
	c := New(srv.URL)
	ctx := context.Background()

	_, err := c.Plan(ctx, "a", api.PlanRequest{Spec: json.RawMessage(`{}`)})
	e, ok := AsError(fmt.Errorf("wrapped: %w", err))
	if !ok || e.Status != 422 || !IsCode(err, api.CodeInvalidSpec) || len(e.Body.Findings) != 1 ||
		e.Body.Findings[0] != (spec.Finding{Severity: spec.SeverityError, Path: "services.x", Problem: "p", Remedy: "r"}) {
		t.Fatalf("invalid spec %+v %v", e, err)
	}
	_, err = c.Apply(ctx, "a", api.ApplyRequest{Base: 1})
	if e, _ := AsError(err); e == nil || e.Body.Current == nil || *e.Body.Current != 2 || !IsCode(err, api.CodeBaseMismatch) {
		t.Fatalf("base mismatch %v", err)
	}
	_, err = c.Environment(ctx, "a")
	if !IsNotFound(err) || err.Error() != "no such environment" || IsCode(err, api.CodeInvalidSpec) {
		t.Fatalf("not found %v", err)
	}
	_, err = c.Tag(ctx, "a", "t")
	if e, _ := AsError(err); e == nil || e.Body.Role != api.RoleWrite || e.Body.Scope != api.ScopeEnvironment {
		t.Fatalf("role %v", err)
	}
	err = c.DeleteSecret(ctx, "a", "s")
	if e, _ := AsError(err); e == nil || e.Status != 502 || err.Error() != "bad gateway" || string(e.Raw) != "bad gateway\n" {
		t.Fatalf("plain %v", err)
	}
	if _, ok := AsError(errors.New("x")); ok || IsNotFound(nil) {
		t.Fatal("no refusal")
	}
	if (&Error{Status: 503}).Error() != "503 Service Unavailable" {
		t.Fatal("status text")
	}
}

// A secret's put says whether it made a version; raw answers stay as
// they came; a stream is the body as it comes.
func TestStatusRawAndStream(t *testing.T) {
	srv, got := server(t, func(r *http.Request) (int, string) {
		switch {
		case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/new"):
			return http.StatusCreated, `{"name": "new", "version": 2, "type": "http"}`
		case r.Method == "PUT":
			return http.StatusOK, `{"name": "same", "version": 1}`
		case r.URL.Path == "/api/v1/logs" && r.URL.Query().Get("follow") == "1":
			return http.StatusOK, "{\"line\": \"a\"}\n{\"line\": \"b\"}\n"
		}
		return http.StatusOK, `{"data": [ 1, 2 ]}`
	})
	c := New(srv.URL)
	ctx := context.Background()
	v, created, err := c.PutSecret(ctx, "e", "new", api.PutSecretRequest{Type: api.SecretHTTP})
	if err != nil || !created || !v.Created || v.Version != 2 {
		t.Fatalf("new %+v %v %v", v, created, err)
	}
	v, created, err = c.PutSecret(ctx, "e", "same", api.PutSecretRequest{Value: "x"})
	if err != nil || created || v.Version != 1 {
		t.Fatalf("same %+v %v %v", v, created, err)
	}
	raw, err := c.Traces(ctx, url.Values{"service": {"api"}})
	if err != nil || string(raw) != `{"data": [ 1, 2 ]}` {
		t.Fatalf("raw %s %v", raw, err)
	}
	rc, err := c.FollowLogs(ctx, url.Values{"environment": {"e"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "{\"line\": \"a\"}\n{\"line\": \"b\"}\n" {
		t.Fatalf("stream %q", b)
	}
	if q := got()[len(got())-1].query; q != "environment=e&follow=1" {
		t.Fatalf("follow query %q", q)
	}
	// Names are escaped as path segments.
	if _, err := c.Tag(ctx, "e", "v1/2"); err != nil {
		t.Fatal(err)
	}
	if p := got()[len(got())-1].path; p != "/api/v1/environments/e/tags/v1%2F2" {
		t.Fatalf("path %q", p)
	}
}

// The context ends a call.
func TestContextEndsCall(t *testing.T) {
	block := make(chan struct{})
	srv, _ := server(t, func(r *http.Request) (int, string) {
		select {
		case <-r.Context().Done():
		case <-block:
		}
		return 200, `{}`
	})
	defer close(block)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(srv.URL).Addons(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}
