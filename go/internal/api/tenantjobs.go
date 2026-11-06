// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/gardenlinux/feos/go/internal/apierr"
	envs "github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// TenantJobs is the tenants' jobs (*env.Service, ADR 0018 "A tenant's job"):
// one method per job route, tenant class. nil: no job route is
// registered.
type TenantJobs interface {
	CreateTenantJob(ctx context.Context, tenant, key string, req envs.CreateTenantJobRequest) (envs.TenantJobStatus, bool, error)
	ListTenantJobs(ctx context.Context, tenant string, labels map[string]string) (envs.TenantJobList, error)
	GetTenantJob(ctx context.Context, tenant, id string) (envs.TenantJobStatus, error)
	TenantJobLogs(ctx context.Context, tenant, id string, lines int) (envs.TenantJobLogsAnswer, error)
	CancelTenantJob(ctx context.Context, tenant, id string) (envs.TenantJobStatus, error)
	DeleteTenantJob(ctx context.Context, tenant, id string) error
}

// tenantJobRoutes registers the jobs' routes, tenant class, each a thin adapter
// of a request to one method of Deps.TenantJobs; none without the service.
// A tenant's job is the tenant's (ADR 0031): its routes need a tenant
// grant.
func (s *server) tenantJobRoutes(tenant func(n need, pattern string, h apiHandler)) {
	if s.deps.TenantJobs == nil {
		return
	}
	tenant(onTenant(write), "POST /api/v1/jobs", s.createTenantJob)
	tenant(onTenant(read), "GET /api/v1/jobs", s.listTenantJobs)
	tenant(onTenant(read), "GET /api/v1/jobs/{id}", s.getTenantJob)
	tenant(onTenant(write), "DELETE /api/v1/jobs/{id}", s.deleteTenantJob)
	tenant(onTenant(read), "GET /api/v1/jobs/{id}/logs", s.tenantJobLogs)
	tenant(onTenant(write), "POST /api/v1/jobs/{id}/cancel", s.cancelTenantJob)
}

// createTenantJob is POST /api/v1/jobs, with an Idempotency-Key as the tool
// calls take one: 201 for a new job, 200 with the job a create of the
// same request and key made.
func (s *server) createTenantJob(w http.ResponseWriter, r *http.Request) error {
	key, err := tools.IdempotencyKey(r)
	if err != nil {
		return err
	}
	var req envs.CreateTenantJobRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
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

// listTenantJobs is GET /api/v1/jobs?label=k=v…: every label must match.
func (s *server) listTenantJobs(w http.ResponseWriter, r *http.Request) error {
	labels := map[string]string{}
	for _, l := range r.URL.Query()["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return apierr.New(http.StatusBadRequest, "label must be key=value, got %q", l)
		}
		labels[k] = v
	}
	list, err := s.deps.TenantJobs.ListTenantJobs(r.Context(), tenantOf(r), labels)
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
