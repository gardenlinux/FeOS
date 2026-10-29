// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	envs "github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
)

// envCall is a call of the fake environments service.
type envCall struct {
	method, tenant, env string
	// args are the call's other arguments: its request, kind, name, …
	args []any
}

// fakeEnvs is the environments service of the API's tests: it records
// its calls and answers what answer gives, decoded strictly into the
// method's answer.
type fakeEnvs struct {
	mu      sync.Mutex
	calls   []envCall
	created bool
	answer  func(c envCall) (json.RawMessage, error)
}

func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (f *fakeEnvs) do(c envCall, out any) error {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	answer := f.answer
	f.mu.Unlock()
	if answer == nil {
		return nil
	}
	raw, err := answer(c)
	if err != nil || out == nil || raw == nil {
		return err
	}
	if err := strictDecode(raw, out); err != nil {
		return fmt.Errorf("the fake's answer does not decode into %T: %w", out, err)
	}
	return nil
}

func (f *fakeEnvs) last(t *testing.T) envCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 {
		t.Fatalf("calls %+v", f.calls)
	}
	return f.calls[0]
}

func (f *fakeEnvs) CreateEnv(_ context.Context, tenant string, req envs.CreateEnvRequest) (envs.EnvStatus, bool, error) {
	var st envs.EnvStatus
	err := f.do(envCall{"CreateEnv", tenant, "", []any{req}}, &st)
	return st, f.created, err
}
func (f *fakeEnvs) ListEnvs(_ context.Context, tenant string, labels map[string]string) (envs.EnvList, error) {
	var out envs.EnvList
	return out, f.do(envCall{"ListEnvs", tenant, "", []any{labels}}, &out)
}
func (f *fakeEnvs) GetEnv(_ context.Context, tenant, env string) (envs.EnvStatus, error) {
	var out envs.EnvStatus
	return out, f.do(envCall{"GetEnv", tenant, env, nil}, &out)
}
func (f *fakeEnvs) DeleteEnv(_ context.Context, tenant, env string) error {
	return f.do(envCall{"DeleteEnv", tenant, env, nil}, nil)
}
func (f *fakeEnvs) PlanSpec(_ context.Context, tenant string, req envs.PlanSpecRequest) (envs.PlanSpecAnswer, error) {
	var out envs.PlanSpecAnswer
	return out, f.do(envCall{"PlanSpec", tenant, "", []any{req}}, &out)
}
func (f *fakeEnvs) Plan(_ context.Context, tenant, env string, req envs.PlanRequest) (envs.PlanAnswer, error) {
	var out envs.PlanAnswer
	return out, f.do(envCall{"Plan", tenant, env, []any{req}}, &out)
}
func (f *fakeEnvs) Apply(_ context.Context, tenant, env string, req envs.ApplyRequest) (envs.ApplyAnswer, error) {
	var out envs.ApplyAnswer
	return out, f.do(envCall{"Apply", tenant, env, []any{req}}, &out)
}
func (f *fakeEnvs) Releases(_ context.Context, tenant, env string, limit int) (envs.ReleaseList, error) {
	var out envs.ReleaseList
	return out, f.do(envCall{"Releases", tenant, env, []any{limit}}, &out)
}
func (f *fakeEnvs) Release(_ context.Context, tenant, env string, n int) (envs.ReleaseDetail, error) {
	var out envs.ReleaseDetail
	return out, f.do(envCall{"Release", tenant, env, []any{n}}, &out)
}
func (f *fakeEnvs) Revert(_ context.Context, tenant, env string, n int, req envs.RevertRequest) (envs.ApplyAnswer, error) {
	var out envs.ApplyAnswer
	return out, f.do(envCall{"Revert", tenant, env, []any{req, n}}, &out)
}

// objectAnswer reads an object route's answer: a plan's (it has "valid")
// or an apply's.
type objectAnswer struct{ a envs.ObjectAnswer }

func (o *objectAnswer) UnmarshalJSON(b []byte) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	if _, ok := keys["valid"]; ok {
		o.a.DryRun = true
		return strictDecode(b, &o.a.Plan)
	}
	return strictDecode(b, &o.a.Apply)
}

func (f *fakeEnvs) PutObject(_ context.Context, tenant, env, kind, name string, req envs.PutObjectRequest) (envs.ObjectAnswer, error) {
	var out objectAnswer
	return out.a, f.do(envCall{"PutObject", tenant, env, []any{req, kind, name}}, &out)
}
func (f *fakeEnvs) DeleteObject(_ context.Context, tenant, env, kind, name string, req envs.DeleteObjectRequest) (envs.ObjectAnswer, error) {
	var out objectAnswer
	return out.a, f.do(envCall{"DeleteObject", tenant, env, []any{req, kind, name}}, &out)
}
func (f *fakeEnvs) Clone(_ context.Context, tenant, env string, req envs.CloneRequest) (envs.EnvStatus, error) {
	var out envs.EnvStatus
	return out, f.do(envCall{"Clone", tenant, env, []any{req}}, &out)
}
func (f *fakeEnvs) Tags(_ context.Context, tenant, env string) (envs.TagList, error) {
	var out envs.TagList
	return out, f.do(envCall{"Tags", tenant, env, nil}, &out)
}
func (f *fakeEnvs) Tag(_ context.Context, tenant, env, tag string) (envs.Tag, error) {
	var out envs.Tag
	return out, f.do(envCall{"Tag", tenant, env, []any{tag}}, &out)
}
func (f *fakeEnvs) PutTag(_ context.Context, tenant, env, tag string, req envs.PutTagRequest) (envs.Tag, error) {
	var out envs.Tag
	return out, f.do(envCall{"PutTag", tenant, env, []any{req, tag}}, &out)
}
func (f *fakeEnvs) DeleteTag(_ context.Context, tenant, env, tag string) error {
	return f.do(envCall{"DeleteTag", tenant, env, []any{tag}}, nil)
}
func (f *fakeEnvs) Secrets(_ context.Context, tenant, env string) (envs.SecretList, error) {
	var out envs.SecretList
	return out, f.do(envCall{"Secrets", tenant, env, nil}, &out)
}
func (f *fakeEnvs) PutSecret(_ context.Context, tenant, env, name string, req envs.PutSecretRequest) (envs.SecretVersion, error) {
	var out envs.SecretVersion
	return out, f.do(envCall{"PutSecret", tenant, env, []any{req, name}}, &out)
}
func (f *fakeEnvs) DeleteSecret(_ context.Context, tenant, env, name string) error {
	return f.do(envCall{"DeleteSecret", tenant, env, []any{name}}, nil)
}
func (f *fakeEnvs) Incidents(_ context.Context, tenant, env string, open bool) (envs.IncidentList, error) {
	var out envs.IncidentList
	return out, f.do(envCall{"Incidents", tenant, env, []any{open}}, &out)
}
func (f *fakeEnvs) Incident(_ context.Context, tenant, env string, id int64) (envs.Incident, error) {
	var out envs.Incident
	return out, f.do(envCall{"Incident", tenant, env, []any{id}}, &out)
}
func (f *fakeEnvs) Logs(_ context.Context, tenant, env, kind, name string, lines, replica int) (envs.LogsAnswer, error) {
	var out envs.LogsAnswer
	return out, f.do(envCall{"Logs", tenant, env, []any{kind, name, lines, replica}}, &out)
}
func (f *fakeEnvs) RestartService(_ context.Context, tenant, env, name string) error {
	return f.do(envCall{"RestartService", tenant, env, []any{name}}, nil)
}
func (f *fakeEnvs) RunJob(_ context.Context, tenant, env, name string) error {
	return f.do(envCall{"RunJob", tenant, env, []any{name}}, nil)
}
func (f *fakeEnvs) CreateRouteToken(_ context.Context, tenant, env string) (envs.RouteToken, error) {
	var out envs.RouteToken
	return out, f.do(envCall{"CreateRouteToken", tenant, env, nil}, &out)
}
func (f *fakeEnvs) RevokeRouteTokens(_ context.Context, tenant, env string) error {
	return f.do(envCall{"RevokeRouteTokens", tenant, env, nil}, nil)
}
func (f *fakeEnvs) SetPublic(_ context.Context, tenant, env string, public bool) (envs.PublicAnswer, error) {
	var out envs.PublicAnswer
	return out, f.do(envCall{"SetPublic", tenant, env, []any{envs.PublicRequest{Public: public}}}, &out)
}
func (f *fakeEnvs) ServeRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Door", "route")
	w.WriteHeader(http.StatusTeapot)
	_, _ = w.Write([]byte("route " + r.PathValue("env") + " " + r.PathValue("path")))
}

var (
	_ Environments = (*fakeEnvs)(nil)
	_ Environments = (*envs.Service)(nil)
)

// fixture is one of the contract's REST fixtures (.claude/caas/rest).
type fixture struct {
	Method              string              `json:"method"`
	Path                string              `json:"path"`
	Query               map[string][]string `json:"query"`
	Status              int                 `json:"status"`
	Request             json.RawMessage     `json:"request"`
	Response            json.RawMessage     `json:"response"`
	Headers             map[string]string   `json:"headers"`
	ResponseContentType string              `json:"responseContentType"`
	Note                string              `json:"note"`
}

// fixtureMethods are the service's methods by the fixtures' names.
var fixtureMethods = map[string]string{
	"POST_environments":                           "CreateEnv",
	"GET_environments":                            "ListEnvs",
	"GET_environments_env":                        "GetEnv",
	"DELETE_environments_env":                     "DeleteEnv",
	"POST_environments_plan":                      "PlanSpec",
	"POST_environments_env_plan":                  "Plan",
	"POST_environments_env_releases":              "Apply",
	"GET_environments_env_releases":               "Releases",
	"GET_environments_env_releases_n":             "Release",
	"POST_environments_env_releases_n_revert":     "Revert",
	"PUT_environments_env_objects_kind_name":      "PutObject",
	"DELETE_environments_env_objects_kind_name":   "DeleteObject",
	"GET_environments_env_objects_kind_name_logs": "Logs",
	"GET_environments_env_tags":                   "Tags",
	"GET_environments_env_tags_tag":               "Tag",
	"PUT_environments_env_tags_tag":               "PutTag",
	"DELETE_environments_env_tags_tag":            "DeleteTag",
	"POST_environments_env_clone":                 "Clone",
	"GET_environments_env_secrets":                "Secrets",
	"PUT_environments_env_secrets_name":           "PutSecret",
	"DELETE_environments_env_secrets_name":        "DeleteSecret",
	"GET_environments_env_incidents":              "Incidents",
	"GET_environments_env_incidents_id":           "Incident",
	"POST_environments_env_services_name_restart": "RestartService",
	"POST_environments_env_jobs_name_run":         "RunJob",
	"POST_environments_env_route-tokens":          "CreateRouteToken",
	"DELETE_environments_env_route-tokens":        "RevokeRouteTokens",
	"PUT_environments_env_public":                 "SetPublic",
}

func loadFixtures(t *testing.T) map[string]fixture {
	t.Helper()
	files, err := filepath.Glob("testdata/ferun/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("fixtures: %v %v", files, err)
	}
	out := map[string]fixture{}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var f fixture
		if err := strictDecode(b, &f); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		out[filepath.Base(file)] = f
	}
	return out
}

// canonicalJSON is b with sorted keys and no space; "" for none.
func canonicalJSON(t *testing.T, b []byte) string {
	t.Helper()
	if len(bytes.TrimSpace(b)) == 0 || string(bytes.TrimSpace(b)) == "null" {
		return ""
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// fixtureError is the error the service answers a fixture's error with.
func fixtureError(t *testing.T, f fixture) error {
	t.Helper()
	var body struct {
		Error    string            `json:"error"`
		Code     string            `json:"code"`
		Findings []json.RawMessage `json:"findings"`
		Current  int               `json:"current"`
	}
	if err := json.Unmarshal(f.Response, &body); err != nil {
		t.Fatalf("%s %s: %v", f.Method, f.Path, err)
	}
	switch body.Code {
	case envs.CodeInvalidSpec:
		ise := &envs.InvalidSpecError{}
		b, _ := json.Marshal(body.Findings)
		if err := strictDecode(b, &ise.Findings); err != nil {
			t.Fatal(err)
		}
		return ise
	case envs.CodeBaseMismatch:
		var req struct {
			Base int `json:"base"`
		}
		_ = json.Unmarshal(f.Request, &req)
		return &envs.BaseMismatchError{Base: req.Base, Current: body.Current}
	}
	return apierr.WithCode(f.Status, body.Code, "%s", body.Error)
}

// Every ferun fixture but the route door's goes through its route: the
// request decodes into what the service is called with (encoding back to
// the fixture's request), and the service's answer, or error, encodes to
// the fixture's response and status.
func TestEnvironmentFixtures(t *testing.T) {
	t.Parallel()
	fixtures := loadFixtures(t)
	e := newEnv(t, envOptions{})
	n := 0
	for name, f := range fixtures {
		if strings.HasPrefix(name, "ANY_") {
			continue
		}
		n++
		t.Run(name, func(t *testing.T) {
			key := strings.SplitN(name, ".", 2)[0]
			method, ok := fixtureMethods[key]
			if !ok {
				t.Fatalf("no method for %s", key)
			}
			fe := &fakeEnvs{created: f.Status == http.StatusCreated}
			fe.answer = func(envCall) (json.RawMessage, error) {
				if f.Status >= 400 {
					return nil, fixtureError(t, f)
				}
				if canonicalJSON(t, f.Response) == "" {
					return nil, nil
				}
				return f.Response, nil
			}
			e.srv.deps.Environments = fe
			srv := newServer(e.srv.cfg, e.srv.deps)
			target := f.Path
			if len(f.Query) > 0 {
				target += "?" + url.Values(f.Query).Encode()
			}
			var body any
			if canonicalJSON(t, f.Request) != "" {
				body = []byte(f.Request)
			}
			r := request{method: f.Method, path: target, body: body,
				header: http.Header{"Authorization": {bearerOf(e.tenantToken)}}}
			w := e.serveWith(srv, r)
			if w.Code != f.Status {
				t.Fatalf("status %d, want %d: %s", w.Code, f.Status, w.Body)
			}
			if got, want := canonicalJSON(t, w.Body.Bytes()), canonicalJSON(t, f.Response); got != want {
				t.Fatalf("answer\n%s\nwant\n%s", got, want)
			}
			c := fe.last(t)
			if c.method != method || c.tenant != testTenant {
				t.Fatalf("call %+v, want %s", c, method)
			}
			if parts := strings.Split(f.Path, "/"); len(parts) > 4 && parts[4] != "plan" && c.env != parts[4] {
				t.Fatalf("env %q of %s", c.env, f.Path)
			}
			if want := canonicalJSON(t, f.Request); want != "" {
				got, _ := json.Marshal(c.args[0])
				if canonicalJSON(t, got) != want {
					t.Fatalf("request\n%s\nwant\n%s", got, want)
				}
			}
		})
	}
	if n < 50 {
		t.Fatalf("only %d fixtures", n)
	}
}

// recorderT is a recorded answer.
type recorderT = httptest.ResponseRecorder

// serveWith serves a request with another server of the env.
func (e *env) serveWith(srv *server, rq request) *recorderT {
	saved := e.h
	e.h = srv.mux
	defer func() { e.h = saved }()
	return e.serve(rq)
}

// The arguments of the routes that read them from the path and query.
func TestEnvironmentArguments(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	tok := bearerOf(e.tenantToken)
	cases := []struct {
		method, path string
		body         any
		want         envCall
		status       int
	}{
		{"GET", "/api/v1/environments?label=a=b&label=c=d%3De", nil,
			envCall{"ListEnvs", testTenant, "", []any{map[string]string{"a": "b", "c": "d=e"}}}, 200},
		{"DELETE", "/api/v1/environments/stage/objects/route/api?base=7&dryRun=1&author=me&message=hi", nil,
			envCall{"DeleteObject", testTenant, "stage", []any{envs.DeleteObjectRequest{Base: 7, DryRun: true, Author: "me",
				Message: "hi"}, "route", "api"}}, 201},
		{"GET", "/api/v1/environments/stage/objects/service/api/logs?lines=50&replica=1", nil,
			envCall{"Logs", testTenant, "stage", []any{"service", "api", 50, 1}}, 200},
		{"GET", "/api/v1/environments/stage/objects/addon/pg/logs", nil,
			envCall{"Logs", testTenant, "stage", []any{"addon", "pg", 0, 0}}, 200},
		{"GET", "/api/v1/environments/stage/releases?limit=5", nil, envCall{"Releases", testTenant, "stage", []any{5}}, 200},
		{"GET", "/api/v1/environments/stage/releases", nil, envCall{"Releases", testTenant, "stage", []any{0}}, 200},
		{"GET", "/api/v1/environments/stage/releases/7", nil, envCall{"Release", testTenant, "stage", []any{7}}, 200},
		{"GET", "/api/v1/environments/stage/incidents?open=1", nil, envCall{"Incidents", testTenant, "stage", []any{true}}, 200},
		{"GET", "/api/v1/environments/stage/incidents", nil, envCall{"Incidents", testTenant, "stage", []any{false}}, 200},
		{"GET", "/api/v1/environments/stage/incidents/12", nil, envCall{"Incident", testTenant, "stage", []any{int64(12)}}, 200},
		{"GET", "/api/v1/environments/stage/tags/v1.4", nil, envCall{"Tag", testTenant, "stage", []any{"v1.4"}}, 200},
		{"DELETE", "/api/v1/environments/stage/secrets/token", nil, envCall{"DeleteSecret", testTenant, "stage",
			[]any{"token"}}, 204},
		{"POST", "/api/v1/environments/stage/services/api/restart", nil, envCall{"RestartService", testTenant, "stage",
			[]any{"api"}}, 202},
		{"POST", "/api/v1/environments/stage/jobs/migrate/run", nil, envCall{"RunJob", testTenant, "stage",
			[]any{"migrate"}}, 202},
		{"POST", "/api/v1/environments/stage/releases/6/revert", map[string]any{"base": 7},
			envCall{"Revert", testTenant, "stage", []any{envs.RevertRequest{Base: 7}, 6}}, 201},
		{"POST", "/api/v1/environments/plan", map[string]any{"spec": map[string]any{}},
			envCall{"PlanSpec", testTenant, "", []any{envs.PlanSpecRequest{Spec: json.RawMessage(`{}`)}}}, 200},
		{"PUT", "/api/v1/environments/stage/public", map[string]any{"public": true},
			envCall{"SetPublic", testTenant, "stage", []any{envs.PublicRequest{Public: true}}}, 200},
		{"PUT", "/api/v1/environments/stage/public", map[string]any{"public": false},
			envCall{"SetPublic", testTenant, "stage", []any{envs.PublicRequest{Public: false}}}, 200},
	}
	for _, c := range cases {
		fe := &fakeEnvs{}
		e.srv.deps.Environments = fe
		w := e.serveWith(newServer(e.srv.cfg, e.srv.deps), request{method: c.method, path: c.path, body: c.body,
			header: http.Header{"Authorization": {tok}}})
		if w.Code != c.status {
			t.Fatalf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
		got := fe.last(t)
		gb, _ := json.Marshal(got.args)
		wb, _ := json.Marshal(c.want.args)
		if got.method != c.want.method || got.env != c.want.env || string(gb) != string(wb) {
			t.Fatalf("%s %s: %+v %s, want %+v %s", c.method, c.path, got, gb, c.want, wb)
		}
	}
	// What cannot be read is a 400 before the service is asked.
	for _, c := range []struct{ method, path, want string }{
		{"GET", "/api/v1/environments?label=nolabel", `label must be key=value, got \"nolabel\"`},
		{"GET", "/api/v1/environments/stage/releases/x", `release \"x\" is not a number`},
		{"GET", "/api/v1/environments/stage/releases?limit=x", "limit must be an integer"},
		{"GET", "/api/v1/environments/stage/incidents/x", `incident \"x\" is not a number`},
		{"GET", "/api/v1/environments/stage/incidents?open=maybe", "open must be a boolean"},
		{"DELETE", "/api/v1/environments/stage/objects/route/api?base=x", "base must be an integer"},
		{"POST", "/api/v1/environments/stage/releases", "request body must be a JSON object"},
	} {
		fe := &fakeEnvs{}
		e.srv.deps.Environments = fe
		w := e.serveWith(newServer(e.srv.cfg, e.srv.deps), request{method: c.method, path: c.path, body: "[1]",
			header: http.Header{"Authorization": {tok}}})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) || len(fe.calls) != 0 {
			t.Fatalf("%s %s: %d %s %+v", c.method, c.path, w.Code, w.Body, fe.calls)
		}
	}
	// PUT …/public needs its field: a body without it, or with null, is a
	// 400, and so is one that is not a boolean.
	for _, c := range []struct {
		body any
		want string
	}{
		{nil, "public is required"}, {"{}", "public is required"}, {`{"public": null}`, "public is required"},
		{`{"other": true}`, "public is required"}, {`{"public": "yes"}`, "invalid request body"},
	} {
		fe := &fakeEnvs{}
		e.srv.deps.Environments = fe
		w := e.serveWith(newServer(e.srv.cfg, e.srv.deps), request{method: "PUT", path: "/api/v1/environments/stage/public",
			body: c.body, header: http.Header{"Authorization": {tok}}})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) || len(fe.calls) != 0 {
			t.Fatalf("PUT public %v: %d %s %+v", c.body, w.Code, w.Body, fe.calls)
		}
	}
	// A plain error is a 500 with its text.
	fe := &fakeEnvs{answer: func(envCall) (json.RawMessage, error) { return nil, errors.New("boom") }}
	e.srv.deps.Environments = fe
	w := e.serveWith(newServer(e.srv.cfg, e.srv.deps), request{method: "GET", path: "/api/v1/environments/stage",
		header: http.Header{"Authorization": {tok}}})
	expect(t, w, http.StatusInternalServerError, errBody("boom"))
	// Without the service there is no environment route.
	e.srv.deps.Environments = nil
	w = e.serveWith(newServer(e.srv.cfg, e.srv.deps), request{method: "GET", path: "/api/v1/environments",
		header: http.Header{"Authorization": {tok}}})
	expect(t, w, http.StatusNotFound, errBody("no such route: GET /api/v1/environments"))
}

// The route door's class: the service's handler with its path values,
// whatever the credentials (it checks its own route tokens).
func TestRouteDoorClass(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		w := e.do(method, "/api/v1/environments/stage/http/api/expense/reports?x=1", bearerOf(e.tenantToken), nil)
		expect(t, w, http.StatusTeapot, "route stage api/expense/reports")
	}
	expect(t, e.do("GET", "/api/v1/environments/stage/http/", "", nil), http.StatusTeapot, "route stage ")
}

// envEngine is what the real environments service needs of the engine.
type envEngine struct{}

func (envEngine) Ensure(context.Context, podid.ID) (model.Lease, error) { return model.Lease{}, nil }
func (envEngine) Delete(context.Context, string, podid.ID) (store.Sandbox, bool, error) {
	return store.Sandbox{}, false, nil
}
func (envEngine) Restart(context.Context, podid.ID, string) error { return nil }
func (envEngine) ResolveImage(_ context.Context, image string, _ model.RegistryCredentials) (string, error) {
	repo, _, _ := strings.Cut(image, ":")
	return repo + "@sha256:" + strings.Repeat("ab", 32), nil
}
func (envEngine) Acquire(_ context.Context, id podid.ID, _ model.HoldKind) (model.Lease, func(), error) {
	return model.Lease{SandboxID: id}, func() {}, nil
}
func (envEngine) Touch(podid.ID)                       {}
func (envEngine) Sync(context.Context, podid.ID) error { return nil }
func (envEngine) State(context.Context, podid.ID) (string, string, error) {
	return model.StateRunning, "", nil
}

// envPorts forwards by saying where to.
type envPorts struct{}

func (envPorts) Probe(context.Context, model.Lease, int, string) (int, error) {
	return 0, errors.New("no")
}
func (envPorts) DialPod(context.Context, model.Lease, int) (net.Conn, error) {
	return nil, errors.New("no")
}
func (envPorts) ForwardTo(w http.ResponseWriter, r *http.Request, id podid.ID, port int, path, rawPath string,
	credentials []string) {
	_, _ = fmt.Fprintf(w, "forwarded %d %s %s auth=%q strip=%q", port, path, r.URL.RawQuery, r.Header.Get("Authorization"),
		credentials)
}

// The real service behind the routes: create, secrets, plan, apply,
// status, releases, tags, route tokens and the door, delete.
func TestEnvironmentsEndToEnd(t *testing.T) {
	t.Parallel()
	var svc *envs.Service
	e := newEnv(t, envOptions{environments: func(d *db.DB, st *store.Store) Environments {
		sl, err := seal.New(make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		svc, err = envs.New(envs.Deps{InstanceID: testInstance, DB: d, Store: st, Engine: envEngine{}, Ports: envPorts{},
			Sealer: sl, Log: dbtest.Logger(t)})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}})
	tok := bearerOf(e.tenantToken)
	do := func(method, path string, body any) *recorderT {
		t.Helper()
		return e.do(method, path, tok, body)
	}
	w := do("POST", "/api/v1/environments", map[string]any{"name": "stage", "labels": map[string]string{"a": "b"}})
	expect(t, w, http.StatusCreated, "")
	st := decode[envs.EnvStatus](t, w)
	w = do("POST", "/api/v1/environments", map[string]any{"name": "stage", "id": st.ID})
	expect(t, w, http.StatusOK, "")
	w = do("POST", "/api/v1/environments", map[string]any{"name": "stage"})
	expect(t, w, http.StatusConflict, `{"error":"an environment named stage exists with another id","code":"name_taken"}`+"\n")
	w = do("GET", "/api/v1/environments?label=a=b", nil)
	if list := decode[envs.EnvList](t, w); w.Code != 200 || len(list.Environments) != 1 || list.Environments[0].State != "none" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	expect(t, do("PUT", "/api/v1/environments/stage/secrets/token", map[string]any{"value": "s3cret"}), http.StatusOK,
		`{"name":"token","version":1}`+"\n")
	spec := map[string]any{"services": map[string]any{"api": map[string]any{"image": "ghcr.io/acme/api:1",
		"env": map[string]any{"T": map[string]any{"secret": "token"}}, "ports": []any{map[string]any{"name": "http", "port": 8080}}}},
		"routes": map[string]any{"api": map[string]any{"path": "/api", "service": "api", "port": "http", "strip": true}}}
	w = do("POST", "/api/v1/environments/stage/plan", map[string]any{"spec": spec})
	if p := decode[envs.PlanAnswer](t, w); w.Code != 200 || !p.Valid || len(p.Diff) != 3 {
		t.Fatalf("plan %d %s", w.Code, w.Body)
	}
	w = do("POST", "/api/v1/environments/"+st.ID+"/releases", map[string]any{"spec": spec, "base": 0})
	if a := decode[envs.ApplyAnswer](t, w); w.Code != 201 || a.Number != 1 {
		t.Fatalf("apply %d %s", w.Code, w.Body)
	}
	w = do("POST", "/api/v1/environments/stage/releases", map[string]any{"spec": spec, "base": 1})
	if a := decode[envs.ApplyAnswer](t, w); w.Code != 200 || !a.Unchanged {
		t.Fatalf("unchanged %d %s", w.Code, w.Body)
	}
	spec["services"].(map[string]any)["api"].(map[string]any)["replicas"] = 2
	w = do("POST", "/api/v1/environments/stage/releases", map[string]any{"spec": spec, "base": 0})
	expect(t, w, http.StatusConflict, `{"error":"base 0 is not the current release 1: plan again on release 1",`+
		`"code":"base_mismatch","current":1}`+"\n")
	w = do("POST", "/api/v1/environments/stage/releases", map[string]any{"spec": map[string]any{"nope": 1}, "base": 1})
	if w.Code != 422 || !strings.Contains(w.Body.String(), `"code":"invalid_spec"`) ||
		!strings.Contains(w.Body.String(), `"path":"nope"`) {
		t.Fatalf("invalid %d %s", w.Code, w.Body)
	}
	w = do("GET", "/api/v1/environments/stage", nil)
	if s := decode[envs.EnvStatus](t, w); w.Code != 200 || s.Rollout == nil || s.Rollout.State != "pending" ||
		len(s.Objects) != 1 || s.Routes[0].Port != 8080 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	w = do("GET", "/api/v1/environments/stage/releases/1", nil)
	if d := decode[envs.ReleaseDetail](t, w); w.Code != 200 || len(d.Spec) == 0 || len(d.Diff) != 3 {
		t.Fatalf("release %d %s", w.Code, w.Body)
	}
	expect(t, do("GET", "/api/v1/environments/stage/releases/9", nil), http.StatusNotFound, errBody("no such release"))
	expect(t, do("PUT", "/api/v1/environments/stage/tags/v1", map[string]any{"release": 1}), http.StatusOK, "")
	expect(t, do("DELETE", "/api/v1/environments/stage/tags/v1", nil), http.StatusNoContent, "")
	expect(t, do("GET", "/api/v1/environments/stage/secrets", nil), http.StatusOK, "")
	expect(t, do("DELETE", "/api/v1/environments/stage/secrets/token", nil), http.StatusConflict,
		errBody("release 1 pins secret token: apply a release without it first"))
	// Without an apps zone nothing can be public; unpublishing is no error.
	expect(t, do("PUT", "/api/v1/environments/stage/public", map[string]any{"public": true}), http.StatusConflict,
		errBody("the platform serves no apps zone (--apps-zone); an environment cannot be public"))
	expect(t, do("PUT", "/api/v1/environments/stage/public", map[string]any{"public": false}), http.StatusOK,
		`{"public":false,"hosts":[],"rolling":false}`+"\n")

	// The route door through the API's mux, with its own token.
	w = do("POST", "/api/v1/environments/stage/route-tokens", nil)
	expect(t, w, http.StatusCreated, "")
	rt := decode[envs.RouteToken](t, w)
	expect(t, e.do("GET", "/api/v1/environments/stage/http/api/x", tok, nil), http.StatusUnauthorized,
		"route token required\n")
	expect(t, e.do("GET", "/api/v1/environments/stage/http/api/x", "", nil), http.StatusUnauthorized,
		"route token required\n")
	w = e.do("GET", "/api/v1/environments/stage/http/api/x?y=1", bearerOf(rt.Token), nil)
	expect(t, w, http.StatusServiceUnavailable, "no ready instance of api\n")
	expect(t, do("DELETE", "/api/v1/environments/stage/route-tokens", nil), http.StatusNoContent, "")
	expect(t, e.do("GET", "/api/v1/environments/stage/http/api/x", bearerOf(rt.Token), nil), http.StatusUnauthorized,
		"route token required\n")

	// Another tenant's environment is absent; delete is a 202.
	other := e.mintTenantToken(mustTenant(t, e, "rival"))
	expect(t, e.do("GET", "/api/v1/environments/stage", bearerOf(other), nil), http.StatusNotFound,
		errBody("no such environment"))
	expect(t, do("DELETE", "/api/v1/environments/stage", nil), http.StatusAccepted, "")
	if !svc.SecretsEnabled() {
		t.Fatal("no secrets")
	}
}
