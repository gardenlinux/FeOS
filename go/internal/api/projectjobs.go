// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	envs "github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// ProjectJobs is the projects' jobs (*env.Service, ADR 0018 "A project's job"):
// one method per job route, project class. nil: no job route is
// registered.
type ProjectJobs interface {
	CreateProjectJob(ctx context.Context, project, key string, req envs.CreateProjectJobRequest) (envs.ProjectJobStatus, bool, error)
	ListProjectJobs(ctx context.Context, project string, labels map[string]string, envIDs []uuid.UUID) (envs.ProjectJobList,
		error)
	GetProjectJob(ctx context.Context, project, id string) (envs.ProjectJobStatus, error)
	ProjectJobLogs(ctx context.Context, project, id string, lines int) (envs.ProjectJobLogsAnswer, error)
	CancelProjectJob(ctx context.Context, project, id string) (envs.ProjectJobStatus, error)
	DeleteProjectJob(ctx context.Context, project, id string) error
}

// projectJobRoutes registers the jobs' routes, project class, each a thin adapter
// of a request to one method of Deps.ProjectJobs; none without the service.
// A project's job runs in an environment (ADR 0035): its routes need the
// role on it, an environment's grant or the project's; the list shows what
// the principal reads.
func (s *server) projectJobRoutes(project func(n need, pattern string, h apiHandler)) {
	if s.deps.ProjectJobs == nil {
		return
	}
	project(byHandler(write), "POST /api/v1/jobs", s.createProjectJob)
	project(member, "GET /api/v1/jobs", s.listProjectJobs)
	project(onJob(read), "GET /api/v1/jobs/{id}", s.getProjectJob)
	project(onJob(write), "DELETE /api/v1/jobs/{id}", s.deleteProjectJob)
	project(onJob(read), "GET /api/v1/jobs/{id}/logs", s.projectJobLogs)
	project(onJob(write), "POST /api/v1/jobs/{id}/cancel", s.cancelProjectJob)
}

// createProjectJob is POST /api/v1/jobs, with an Idempotency-Key as the tool
// calls take one: 201 for a new job, 200 with the job a create of the
// same request and key made. It needs write on the job's environment.
func (s *server) createProjectJob(w http.ResponseWriter, r *http.Request) error {
	key, err := tools.IdempotencyKey(r)
	if err != nil {
		return err
	}
	var req envs.CreateProjectJobRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.Environment) != "" {
		if _, err := s.writableEnv(r, req.Environment); err != nil {
			return err
		}
	}
	job, created, err := s.deps.ProjectJobs.CreateProjectJob(r.Context(), projectOf(r), key, req)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return writeJSON(w, status, job)
}

// listProjectJobs is GET /api/v1/jobs?label=k=v…[&environment=]: every
// label must match; the jobs of the environments the principal reads.
func (s *server) listProjectJobs(w http.ResponseWriter, r *http.Request) error {
	envIDs, err := s.readableEnvs(r)
	if err != nil {
		return err
	}
	labels := map[string]string{}
	for _, l := range r.URL.Query()["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return apierr.New(http.StatusBadRequest, "label must be key=value, got %q", l)
		}
		labels[k] = v
	}
	list, err := s.deps.ProjectJobs.ListProjectJobs(r.Context(), projectOf(r), labels, envIDs)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *server) getProjectJob(w http.ResponseWriter, r *http.Request) error {
	job, err := s.deps.ProjectJobs.GetProjectJob(r.Context(), projectOf(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, job)
}

func (s *server) deleteProjectJob(w http.ResponseWriter, r *http.Request) error {
	if err := s.deps.ProjectJobs.DeleteProjectJob(r.Context(), projectOf(r), r.PathValue("id")); err != nil {
		return err
	}
	return noContent(w, http.StatusNoContent)
}

// projectJobLogs is GET /api/v1/jobs/{id}/logs?lines=N.
func (s *server) projectJobLogs(w http.ResponseWriter, r *http.Request) error {
	lines, err := queryInt(r, "lines")
	if err != nil {
		return err
	}
	out, err := s.deps.ProjectJobs.ProjectJobLogs(r.Context(), projectOf(r), r.PathValue("id"), lines)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *server) cancelProjectJob(w http.ResponseWriter, r *http.Request) error {
	job, err := s.deps.ProjectJobs.CancelProjectJob(r.Context(), projectOf(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, job)
}
