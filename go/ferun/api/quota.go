// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

// Quota is GET /api/v1/project/quota: the project's use against its
// limits (ADR 0031, "Quotas per project").
type Quota struct {
	Limits QuotaLimits `json:"limits"`
	Use    QuotaUse    `json:"use"`
}

// QuotaLimits are a project's limits; a limit left out is none.
type QuotaLimits struct {
	Sandboxes    *int   `json:"sandboxes,omitempty"`
	Environments *int   `json:"environments,omitempty"`
	RunningJobs  *int   `json:"runningJobs,omitempty"`
	VCPUs        *int   `json:"vcpus,omitempty"`
	MemoryMiB    *int64 `json:"memoryMib,omitempty"`
}

// QuotaUse is a project's use, or what a request asks for: its
// sandboxes, its environments, its running jobs, and the sums of vCPUs
// and of memory over its sandboxes, its environments' services and
// addons and its running jobs.
type QuotaUse struct {
	Sandboxes    int64 `json:"sandboxes"`
	Environments int64 `json:"environments"`
	RunningJobs  int64 `json:"runningJobs"`
	VCPUs        int64 `json:"vcpus"`
	MemoryMiB    int64 `json:"memoryMib"`
}

// The resources, as answers, limits and a quota_exceeded refusal name
// them.
const (
	ResourceSandboxes    = "sandboxes"
	ResourceEnvironments = "environments"
	ResourceRunningJobs  = "runningJobs"
	ResourceVCPUs        = "vcpus"
	ResourceMemoryMiB    = "memoryMib"
	ResourceProjects     = "projects"
)
