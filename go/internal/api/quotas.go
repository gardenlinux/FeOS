// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/quota"
	"github.com/gardenlinux/feos/go/internal/store"
)

// This file is ADR 0031's "Quotas per project" at the API: a project's use
// against its limits, and the operators' setting of them. The checks are
// at the creates (internal/quota).

// limitsView is a project's limits; a limit left out is none.
type limitsView struct {
	Sandboxes    *int   `json:"sandboxes,omitempty"`
	Environments *int   `json:"environments,omitempty"`
	RunningJobs  *int   `json:"runningJobs,omitempty"`
	VCPUs        *int   `json:"vcpus,omitempty"`
	MemoryMiB    *int64 `json:"memoryMib,omitempty"`
}

func limitsOf(l store.ProjectLimits) limitsView {
	return limitsView{Sandboxes: l.Sandboxes, Environments: l.Environments, RunningJobs: l.RunningJobs,
		VCPUs: l.VCPUs, MemoryMiB: l.MemoryMiB}
}

// quotaRoutes adds the routes of the quotas.
func (s *server) quotaRoutes(operator func(string, apiHandler), project func(need, string, apiHandler)) {
	operator("PUT /api/v1/admin/projects/{project}/limits", s.putProjectLimits)
	project(member, "GET /api/v1/project/quota", s.getQuota)
}

// getQuota is GET /api/v1/project/quota: the project's use against its
// limits, {limits, use}, each {sandboxes, environments, runningJobs,
// vcpus, memoryMib}; a limit left out is none.
func (s *server) getQuota(w http.ResponseWriter, r *http.Request) error {
	t, err := s.st.GetProject(r.Context(), projectOf(r))
	if isNoRow(err) {
		return errNoProject()
	}
	if err != nil {
		return fmt.Errorf("quota: %w", err)
	}
	use, err := quota.Of(r.Context(), s.st, t.ID, uuid.Nil)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, struct {
		Limits limitsView `json:"limits"`
		Use    quota.Use  `json:"use"`
	}{limitsOf(t.Limits), use})
}

// putProjectLimits is PUT /api/v1/admin/projects/{project}/limits
// {sandboxes, environments, runningJobs, vcpus, memoryMib}: an operator
// replaces a project's limits, each a number of at least 0 or null (or
// left out) for none. What runs over a lowered limit stays; the next
// create that asks for more is refused.
func (s *server) putProjectLimits(w http.ResponseWriter, r *http.Request) error {
	var req limitsView
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	for name, v := range map[string]*int{quota.Sandboxes: req.Sandboxes, quota.Environments: req.Environments,
		quota.RunningJobs: req.RunningJobs, quota.VCPUs: req.VCPUs} {
		if v != nil && *v < 0 {
			return apierr.New(http.StatusBadRequest, "%s must be at least 0, or null for no limit", name)
		}
	}
	if req.MemoryMiB != nil && *req.MemoryMiB < 0 {
		return apierr.New(http.StatusBadRequest, "%s must be at least 0, or null for no limit", quota.MemoryMiB)
	}
	name := r.PathValue("project")
	t, err := s.st.SetProjectLimits(r.Context(), name, store.ProjectLimits{Sandboxes: req.Sandboxes,
		Environments: req.Environments, RunningJobs: req.RunningJobs, VCPUs: req.VCPUs, MemoryMiB: req.MemoryMiB})
	if isNoRow(err) {
		return apierr.New(http.StatusNotFound, "unknown project %s", name)
	}
	if err != nil {
		return fmt.Errorf("limits: %w", err)
	}
	by := adminOfRequest(r)
	s.log.Info("project limits set", "project", name, "by", by)
	s.record(r.Context(), store.ActivityRow{ProjectID: name, Kind: "project_limits", Category: "lifecycle",
		Name: "limits of project " + name + " set by " + by, Detail: detail(map[string]any{"project": name, "by": by,
			"limits": limitsOf(t.Limits)})})
	return writeJSON(w, http.StatusOK, struct {
		Name   string     `json:"name"`
		Limits limitsView `json:"limits"`
	}{t.ID, limitsOf(t.Limits)})
}
