// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	ferunapi "github.com/gardenlinux/feos/go/ferun/api"
	"github.com/gardenlinux/feos/go/ferun/client"
)

// clientRecorder is a transport that answers every request 200 {} and keeps
// it, with the client method that sent it.
type clientRecorder struct {
	mu     sync.Mutex
	method string
	calls  []clientRecorded
}

type clientRecorded struct {
	by  string
	req *http.Request
}

func (r *clientRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.calls = append(r.calls, clientRecorded{r.method, req})
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

// clientCalls calls every method of the published client once, through
// rec. The sink's reads are named in sink.
func clientCalls(t *testing.T, rec *clientRecorder) (sink map[string]bool) {
	t.Helper()
	c := client.New("https://run.test", client.WithHTTPClient(&http.Client{Transport: rec}))
	ctx := context.Background()
	q := url.Values{"window": {"1h"}}
	calls := []struct {
		name string
		call func() error
	}{
		{"Environments", func() error { _, err := c.Environments(ctx, map[string]string{"a": "b"}); return err }},
		{"Environment", func() error { _, err := c.Environment(ctx, "stage"); return err }},
		{"CreateEnvironment", func() error {
			_, err := c.CreateEnvironment(ctx, ferunapi.CreateEnvRequest{Name: "n"})
			return err
		}},
		{"RenameEnvironment", func() error {
			_, err := c.RenameEnvironment(ctx, "stage", ferunapi.RenameEnvRequest{Name: "n"})
			return err
		}},
		{"DeleteEnvironment", func() error { return c.DeleteEnvironment(ctx, "stage") }},
		{"PlanSpec", func() error { _, err := c.PlanSpec(ctx, json.RawMessage(`{}`)); return err }},
		{"Plan", func() error { _, err := c.Plan(ctx, "stage", ferunapi.PlanRequest{}); return err }},
		{"Apply", func() error { _, err := c.Apply(ctx, "stage", ferunapi.ApplyRequest{}); return err }},
		{"Deployments", func() error { _, err := c.Deployments(ctx, "stage", 5); return err }},
		{"Deployment", func() error { _, err := c.Deployment(ctx, "stage", 3); return err }},
		{"Revert", func() error { _, err := c.Revert(ctx, "stage", 3, ferunapi.RevertRequest{}); return err }},
		{"Clone", func() error { _, err := c.Clone(ctx, "stage", ferunapi.CloneRequest{Name: "n"}); return err }},
		{"Tags", func() error { _, err := c.Tags(ctx, "stage"); return err }},
		{"Tag", func() error { _, err := c.Tag(ctx, "stage", "v1"); return err }},
		{"PutTag", func() error { _, err := c.PutTag(ctx, "stage", "v1", ferunapi.PutTagRequest{}); return err }},
		{"DeleteTag", func() error { return c.DeleteTag(ctx, "stage", "v1") }},
		{"Secrets", func() error { _, err := c.Secrets(ctx, "stage"); return err }},
		{"Secret", func() error { _, err := c.Secret(ctx, "stage", "s"); return err }},
		{"PutSecret", func() error {
			_, _, err := c.PutSecret(ctx, "stage", "s", ferunapi.PutSecretRequest{Value: "v"})
			return err
		}},
		{"CheckSecret", func() error { _, err := c.CheckSecret(ctx, "stage", "s", map[string]string{}); return err }},
		{"DeleteSecret", func() error { return c.DeleteSecret(ctx, "stage", "s") }},
		{"Incidents", func() error { _, err := c.Incidents(ctx, "stage", true); return err }},
		{"Incident", func() error { _, err := c.Incident(ctx, "stage", "7"); return err }},
		{"ObjectLogs", func() error {
			_, err := c.ObjectLogs(ctx, "stage", "service", "api", url.Values{"lines": {"5"}})
			return err
		}},
		{"RestartService", func() error { return c.RestartService(ctx, "stage", "api") }},
		{"RunJob", func() error { return c.RunJob(ctx, "stage", "migrate") }},
		{"CreateRouteToken", func() error { _, err := c.CreateRouteToken(ctx, "stage"); return err }},
		{"RevokeRouteTokens", func() error { return c.RevokeRouteTokens(ctx, "stage") }},
		{"SetPublic", func() error {
			_, err := c.SetPublic(ctx, "stage", ferunapi.PublicRequest{Public: true})
			return err
		}},
		{"Identities", func() error { _, err := c.Identities(ctx, "stage"); return err }},
		{"Traffic", func() error { _, err := c.Traffic(ctx, "stage", q); return err }},
		{"BindingUsage", func() error { _, err := c.BindingUsage(ctx, "stage", q); return err }},
		{"Addons", func() error { _, err := c.Addons(ctx); return err }},
		{"Quota", func() error { _, err := c.Quota(ctx); return err }},
		{"Domains", func() error { _, err := c.Domains(ctx); return err }},
		{"Domain", func() error { _, err := c.Domain(ctx, "d1"); return err }},
		{"CreateDomain", func() error {
			_, err := c.CreateDomain(ctx, ferunapi.CreateDomainRequest{Name: "a.example"})
			return err
		}},
		{"DeleteDomain", func() error { return c.DeleteDomain(ctx, "d1") }},
		{"BindDomain", func() error {
			_, err := c.BindDomain(ctx, "d1", ferunapi.BindDomainRequest{Environment: "stage"})
			return err
		}},
		{"UnbindDomain", func() error { _, err := c.UnbindDomain(ctx, "d1"); return err }},
		{"CheckDomain", func() error { _, err := c.CheckDomain(ctx, "d1"); return err }},
		// The people answers (F3).
		{"Me", func() error { _, err := c.Me(ctx); return err }},
		{"Project", func() error { _, err := c.Project(ctx); return err }},
		{"Members", func() error { _, err := c.Members(ctx); return err }},
		{"Integrations", func() error { _, err := c.Integrations(ctx); return err }},
		{"SetProtected", func() error { _, err := c.SetProtected(ctx, "stage", true); return err }},
		{"Principals", func() error { _, err := c.Principals(ctx, "stage"); return err }},
		// The sink's reads.
		{"Logs", func() error { _, err := c.Logs(ctx, url.Values{"environment": {"e"}}); return err }},
		{"LogStreams", func() error { _, err := c.LogStreams(ctx, nil); return err }},
		{"FollowLogs", func() error {
			rc, err := c.FollowLogs(ctx, url.Values{"environment": {"e"}})
			if err == nil {
				_ = rc.Close()
			}
			return err
		}},
		{"Traces", func() error { _, err := c.Traces(ctx, url.Values{"service": {"api"}}); return err }},
		{"Trace", func() error { _, err := c.Trace(ctx, "4bf92f3577b34da6a3ce929d0e0e4736", nil); return err }},
	}
	for _, cl := range calls {
		rec.mu.Lock()
		rec.method = cl.name
		rec.mu.Unlock()
		if err := cl.call(); err != nil {
			t.Fatalf("%s: %v", cl.name, err)
		}
	}
	return map[string]bool{"Logs": true, "LogStreams": true, "FollowLogs": true, "Traces": true, "Trace": true}
}

// sinkPatterns are the patterns the sink's API registers
// (internal/sink/api.go), read from its source: the sink's package is not
// this one's to build.
func sinkPatterns(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../sink/api.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if p, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, p)
			}
		}
		return true
	})
	if len(out) < 5 {
		t.Fatalf("the sink's patterns: %v", out)
	}
	return out
}

// muxOf is a mux of the patterns, each answering with its pattern.
func muxOf(patterns []string) *http.ServeMux {
	m := http.NewServeMux()
	for _, p := range patterns {
		m.Handle(p, http.NotFoundHandler())
	}
	return m
}

// Every request the published client sends is a route of ferund's, or,
// for the sink's reads, of the sink's: a pattern of the method's own
// (never the catch-all "/api/" or the UI).
func TestClientRoutes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{bindings: &fakeBindings{on: true}})
	var ferund []string
	for _, rt := range e.srv.routes() {
		ferund = append(ferund, rt.pattern)
	}
	muxes := map[bool]*http.ServeMux{false: muxOf(ferund), true: muxOf(sinkPatterns(t))}
	rec := &clientRecorder{}
	sink := clientCalls(t, rec)
	seen := map[string]bool{}
	for _, c := range rec.calls {
		seen[c.by] = true
		_, pattern := muxes[sink[c.by]].Handler(c.req)
		if !strings.HasPrefix(pattern, c.req.Method+" ") {
			t.Errorf("%s: %s %s is no route of its own (%q)", c.by, c.req.Method, c.req.URL.Path, pattern)
		}
	}
	for by := range sink {
		if !seen[by] {
			t.Errorf("%s sent nothing", by)
		}
	}
	if len(seen) < 45 {
		t.Fatalf("only %d methods called", len(seen))
	}
}
