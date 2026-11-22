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
	_ ProjectJobs = (*fakeProjectJobs)(nil)
	_ ProjectJobs = (*envs.Service)(nil)
)

// fakeProjectJobs answers the job routes from a map, and records the calls.
type fakeProjectJobs struct {
	mu    sync.Mutex
	jobs  map[string]envs.ProjectJobStatus
	keys  map[string]string // key → job ID
	calls []string
	lines int
}

func (f *fakeProjectJobs) call(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeProjectJobs) CreateProjectJob(_ context.Context, project, key string, req envs.CreateProjectJobRequest) (envs.ProjectJobStatus, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobs == nil {
		f.jobs, f.keys = map[string]envs.ProjectJobStatus{}, map[string]string{}
	}
	if id, ok := f.keys[key]; ok && key != "" {
		return f.jobs[id], false, nil
	}
	r := envs.ProjectJobStatus{ID: "a0000000001", Environment: envs.EnvRef{Name: req.Environment}, Image: req.Image,
		Phase: "queued", Labels: req.Labels}
	f.jobs[r.ID], f.keys[key] = r, r.ID
	f.calls = append(f.calls, "CreateProjectJob "+project+" "+key)
	return r, true, nil
}

func (f *fakeProjectJobs) ListProjectJobs(_ context.Context, project string, labels map[string]string, envIDs []uuid.UUID) (
	envs.ProjectJobList, error) {
	f.call(fmt.Sprintf("ListProjectJobs %s %s %v", project, labels["project"], envIDs))
	return envs.ProjectJobList{ProjectJobs: []envs.ProjectJobStatus{}}, nil
}

func (f *fakeProjectJobs) get(id string) (envs.ProjectJobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.jobs[id]
	if !ok {
		return envs.ProjectJobStatus{}, apierr.New(http.StatusNotFound, "no such job")
	}
	return r, nil
}

func (f *fakeProjectJobs) GetProjectJob(_ context.Context, _, id string) (envs.ProjectJobStatus, error) {
	return f.get(id)
}

func (f *fakeProjectJobs) ProjectJobLogs(_ context.Context, _, id string, lines int) (envs.ProjectJobLogsAnswer, error) {
	if _, err := f.get(id); err != nil {
		return envs.ProjectJobLogsAnswer{}, err
	}
	f.mu.Lock()
	f.lines = lines
	f.mu.Unlock()
	return envs.ProjectJobLogsAnswer{Lines: []string{"done"}}, nil
}

func (f *fakeProjectJobs) CancelProjectJob(_ context.Context, _, id string) (envs.ProjectJobStatus, error) {
	r, err := f.get(id)
	r.Phase = "cancelled"
	return r, err
}

func (f *fakeProjectJobs) DeleteProjectJob(_ context.Context, _, id string) error {
	_, err := f.get(id)
	return err
}

// The job routes: a create is 201, a repeat with its Idempotency-Key 200
// with the same job, a key outside the alphabet 400; get, logs (lines
// passed), cancel and delete adapt one call each; a project token only.
func TestProjectJobRoutes(t *testing.T) {
	e := newEnv(t, envOptions{})
	tok := bearerOf(e.projectToken)
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
	if e.projectJobs.lines != 5 {
		t.Errorf("logs asked for %d lines", e.projectJobs.lines)
	}
	expect(t, e.do("GET", "/api/v1/jobs?label=project=p1", tok, nil), http.StatusOK, `{"jobs":[]}`+"\n")
	expect(t, e.do("GET", "/api/v1/jobs?environment=box", tok, nil), http.StatusOK, `{"jobs":[]}`+"\n")
	expect(t, e.do("GET", "/api/v1/jobs?environment=nope", tok, nil), http.StatusNotFound, errBody("no such environment"))
	// A job in an environment of none of the project's is none.
	w := e.serve(request{method: "POST", path: "/api/v1/jobs", header: http.Header{"Authorization": {tok}},
		body: map[string]any{"environment": "nope", "image": "builder:1"}})
	expect(t, w, http.StatusNotFound, errBody("no such environment"))
	if calls := e.projectJobs.calls; !slices.Contains(calls, "ListProjectJobs "+testProject+" p1 []") ||
		!slices.Contains(calls, fmt.Sprintf("ListProjectJobs %s  [%s]", testProject, e.box)) {
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
