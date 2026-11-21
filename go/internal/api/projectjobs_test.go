// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	envs "github.com/gardenlinux/feos/go/internal/env"
)

var (
	_ TenantJobs = (*fakeTenantJobs)(nil)
	_ TenantJobs = (*envs.Service)(nil)
)

// fakeTenantJobs answers the job routes from a map, and records the calls.
type fakeTenantJobs struct {
	mu    sync.Mutex
	jobs  map[string]envs.TenantJobStatus
	keys  map[string]string // key → job ID
	calls []string
	lines int
}

func (f *fakeTenantJobs) call(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeTenantJobs) CreateTenantJob(_ context.Context, tenant, key string, req envs.CreateTenantJobRequest) (envs.TenantJobStatus, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobs == nil {
		f.jobs, f.keys = map[string]envs.TenantJobStatus{}, map[string]string{}
	}
	if id, ok := f.keys[key]; ok && key != "" {
		return f.jobs[id], false, nil
	}
	r := envs.TenantJobStatus{ID: "a0000000001", Environment: envs.EnvRef{Name: req.Environment}, Image: req.Image,
		Phase: "queued", Labels: req.Labels}
	f.jobs[r.ID], f.keys[key] = r, r.ID
	f.calls = append(f.calls, "CreateTenantJob "+tenant+" "+key)
	return r, true, nil
}

func (f *fakeTenantJobs) ListTenantJobs(_ context.Context, tenant string, labels map[string]string, envIDs []uuid.UUID) (
	envs.TenantJobList, error) {
	f.call(fmt.Sprintf("ListTenantJobs %s %s %v", tenant, labels["project"], envIDs))
	return envs.TenantJobList{TenantJobs: []envs.TenantJobStatus{}}, nil
}

func (f *fakeTenantJobs) get(id string) (envs.TenantJobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.jobs[id]
	if !ok {
		return envs.TenantJobStatus{}, apierr.New(http.StatusNotFound, "no such job")
	}
	return r, nil
}

func (f *fakeTenantJobs) GetTenantJob(_ context.Context, _, id string) (envs.TenantJobStatus, error) {
	return f.get(id)
}

func (f *fakeTenantJobs) TenantJobLogs(_ context.Context, _, id string, lines int) (envs.TenantJobLogsAnswer, error) {
	if _, err := f.get(id); err != nil {
		return envs.TenantJobLogsAnswer{}, err
	}
	f.mu.Lock()
	f.lines = lines
	f.mu.Unlock()
	return envs.TenantJobLogsAnswer{Lines: []string{"done"}}, nil
}

func (f *fakeTenantJobs) CancelTenantJob(_ context.Context, _, id string) (envs.TenantJobStatus, error) {
	r, err := f.get(id)
	r.Phase = "cancelled"
	return r, err
}

func (f *fakeTenantJobs) DeleteTenantJob(_ context.Context, _, id string) error {
	_, err := f.get(id)
	return err
}

// The job routes: a create is 201, a repeat with its Idempotency-Key 200
// with the same job, a key outside the alphabet 400; get, logs (lines
// passed), cancel and delete adapt one call each; a tenant token only.
func TestTenantJobRoutes(t *testing.T) {
	e := newEnv(t, envOptions{})
	tok := bearerOf(e.tenantToken)
	create := func(key string) *requestResult {
		h := http.Header{"Authorization": {tok}}
		if key != "" {
			h.Set("Idempotency-Key", key)
		}
		w := e.serve(request{method: "POST", path: "/api/v1/jobs", header: h,
			body: map[string]any{"environment": "box", "image": "builder:1", "labels": map[string]string{"project": "p1"}}})
		return &requestResult{code: w.Code, body: w.Body.String()}
	}
	first := create("build-1")
	if first.code != http.StatusCreated {
		t.Fatalf("create: %d %s", first.code, first.body)
	}
	if again := create("build-1"); again.code != http.StatusOK || again.body != first.body {
		t.Fatalf("repeat: %d %s", again.code, again.body)
	}
	if bad := create("no spaces"); bad.code != http.StatusBadRequest {
		t.Fatalf("bad key: %d %s", bad.code, bad.body)
	}
	expect(t, e.do("GET", "/api/v1/jobs/a0000000001", tok, nil), http.StatusOK, "")
	expect(t, e.do("GET", "/api/v1/jobs/zzz", tok, nil), http.StatusNotFound, errBody("no such job"))
	expect(t, e.do("GET", "/api/v1/jobs/a0000000001/logs?lines=5", tok, nil), http.StatusOK, `{"lines":["done"]}`+"\n")
	if e.tenantJobs.lines != 5 {
		t.Errorf("logs asked for %d lines", e.tenantJobs.lines)
	}
	expect(t, e.do("GET", "/api/v1/jobs?label=project=p1", tok, nil), http.StatusOK, `{"jobs":[]}`+"\n")
	expect(t, e.do("GET", "/api/v1/jobs?environment=box", tok, nil), http.StatusOK, `{"jobs":[]}`+"\n")
	expect(t, e.do("GET", "/api/v1/jobs?environment=nope", tok, nil), http.StatusNotFound, errBody("no such environment"))
	// A job in an environment of none of the tenant's is none.
	w := e.serve(request{method: "POST", path: "/api/v1/jobs", header: http.Header{"Authorization": {tok}},
		body: map[string]any{"environment": "nope", "image": "builder:1"}})
	expect(t, w, http.StatusNotFound, errBody("no such environment"))
	if calls := e.tenantJobs.calls; !slices.Contains(calls, "ListTenantJobs "+testTenant+" p1 []") ||
		!slices.Contains(calls, fmt.Sprintf("ListTenantJobs %s  [%s]", testTenant, e.box)) {
		t.Fatalf("calls %v", calls)
	}
	expect(t, e.do("GET", "/api/v1/jobs?label=nokey", tok, nil), http.StatusBadRequest, "")
	w = e.do("POST", "/api/v1/jobs/a0000000001/cancel", tok, nil)
	expect(t, w, http.StatusOK, "")
	expect(t, e.do("DELETE", "/api/v1/jobs/a0000000001", tok, nil), http.StatusNoContent, "")
	expect(t, e.do("GET", "/api/v1/jobs", "", nil), http.StatusUnauthorized, "")
}

// requestResult is a response's status and body.
type requestResult struct {
	code int
	body string
}
