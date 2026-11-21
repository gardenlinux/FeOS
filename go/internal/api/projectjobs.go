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

// TenantJobs is the tenants' jobs (*env.Service, ADR 0018 "A tenant's job"):
// one method per job route, tenant class. nil: no job route is
// registered.
type TenantJobs interface {
	CreateTenantJob(ctx context.Context, tenant, key string, req envs.CreateTenantJobRequest) (envs.TenantJobStatus, bool, error)
	ListTenantJobs(ctx context.Context, tenant string, labels map[string]string, envIDs []uuid.UUID) (envs.TenantJobList,
		error)
	GetTenantJob(ctx context.Context, tenant, id string) (envs.TenantJobStatus, error)
	TenantJobLogs(ctx context.Context, tenant, id string, lines int) (envs.TenantJobLogsAnswer, error)
	CancelTenantJob(ctx context.Context, tenant, id string) (envs.TenantJobStatus, error)
	DeleteTenantJob(ctx context.Context, tenant, id string) error
}

// tenantJobRoutes registers the jobs' routes, tenant class, each a thin adapter
// of a request to one method of Deps.TenantJobs; none without the service.
// A tenant's job runs in an environment (ADR 0035): its routes need the
// role on it, an environment's grant or the tenant's; the list shows what
// the principal reads.
func (s *server) tenantJobRoutes(tenant func(n need, pattern string, h apiHandler)) {
	if s.deps.TenantJobs == nil {
		return
	}
	tenant(byHandler(write), "POST /api/v1/jobs", s.createTenantJob)
	tenant(member, "GET /api/v1/jobs", s.listTenantJobs)
	tenant(onJob(read), "GET /api/v1/jobs/{id}", s.getTenantJob)
	tenant(onJob(write), "DELETE /api/v1/jobs/{id}", s.deleteTenantJob)
	tenant(onJob(read), "GET /api/v1/jobs/{id}/logs", s.tenantJobLogs)
	tenant(onJob(write), "POST /api/v1/jobs/{id}/cancel", s.cancelTenantJob)
}

// createTenantJob is POST /api/v1/jobs, with an Idempotency-Key as the tool
// calls take one: 201 for a new job, 200 with the job a create of the
// same request and key made. It needs write on the job's environment.
func (s *server) createTenantJob(w http.ResponseWriter, r *http.Request) error {
	key, err := tools.IdempotencyKey(r)
	if err != nil {
		return err
	}
	var req envs.CreateTenantJobRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.Environment) != "" {
		if _, err := s.writableEnv(r, req.Environment); err != nil {
			return err
		}
	}
	job, created, err := s.deps.TenantJobs.CreateTenantJob(r.Context(), tenantOf(r), key, req)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return writeJSON(w, status, job)
}

// listTenantJobs is GET /api/v1/jobs?label=k=v…[&environment=]: every
// label must match; the jobs of the environments the principal reads.
func (s *server) listTenantJobs(w http.ResponseWriter, r *http.Request) error {
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
	list, err := s.deps.TenantJobs.ListTenantJobs(r.Context(), tenantOf(r), labels, envIDs)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *server) getTenantJob(w http.ResponseWriter, r *http.Request) error {
	job, err := s.deps.TenantJobs.GetTenantJob(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, job)
}

func (s *server) deleteTenantJob(w http.ResponseWriter, r *http.Request) error {
	if err := s.deps.TenantJobs.DeleteTenantJob(r.Context(), tenantOf(r), r.PathValue("id")); err != nil {
		return err
	}
	return noContent(w, http.StatusNoContent)
}

// tenantJobLogs is GET /api/v1/jobs/{id}/logs?lines=N.
func (s *server) tenantJobLogs(w http.ResponseWriter, r *http.Request) error {
	lines, err := queryInt(r, "lines")
	if err != nil {
		return err
	}
	out, err := s.deps.TenantJobs.TenantJobLogs(r.Context(), tenantOf(r), r.PathValue("id"), lines)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *server) cancelTenantJob(w http.ResponseWriter, r *http.Request) error {
	job, err := s.deps.TenantJobs.CancelTenantJob(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, job)
}
