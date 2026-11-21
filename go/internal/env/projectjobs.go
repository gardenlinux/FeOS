// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// CodeJobKeyConflict is a create whose Idempotency-Key names a job made
// with another request (422).
const CodeJobKeyConflict = "job_key_conflict"

// CreateTenantJobRequest is POST /api/v1/jobs (ADR 0018, "A tenant's job").
type CreateTenantJobRequest struct {
	// Environment names the environment the job runs in (ADR 0035), by
	// name or ID; required.
	Environment string `json:"environment"`
	// Reaches is what of the environment its pod reaches (ADR 0035,
	// revised): "all" or entries of its deployed deployment; left out:
	// nothing. Egress is its egress as a service's; left out: the
	// internet.
	Reaches spec.Reaches `json:"reaches,omitzero"`
	Egress  *spec.Egress `json:"egress,omitempty"`
	// SecurityGroups is gone (ADR 0034, revised): sent, it is 400.
	SecurityGroups json.RawMessage `json:"securityGroups,omitempty"`
	// Image is pinned to its digest at the create, asked with
	// ImageCredentials.
	Image string `json:"image"`
	// Command replaces the image's entrypoint and command; empty: they
	// run.
	Command []string `json:"command,omitempty"`
	// Env and Secrets are the container's environment variables; the
	// secrets' values are sealed, never returned and redacted from the
	// log.
	Env     map[string]string `json:"env,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	// ImageCredentials are the login for the image's registry (ADR 0025),
	// sealed; nil: anonymous.
	ImageCredentials *model.RegistryCredentials `json:"imageCredentials,omitempty"`
	Vcpus            int                        `json:"vcpus,omitempty"`
	MemoryMib        int                        `json:"memoryMib,omitempty"`
	// Deadline counts from the create, the queue included; "": 10m, at
	// most 2h.
	Deadline string `json:"deadline,omitempty"`
	// ResultPath is a file of the container that is read once its
	// process exited, its first 4 KiB the job's result; "": none.
	ResultPath string            `json:"resultPath,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	// LogRetentionDays is how long the sink keeps the job's log (ADR 0026),
	// 1 to 400 days; 0: the sink's default.
	LogRetentionDays int `json:"logRetentionDays,omitempty"`
}

// TenantJobStatus is a job as the answers show it.
type TenantJobStatus struct {
	ID string `json:"id"`
	// Environment is the environment it runs in; Reaches what of it its
	// pod reaches ("all", or the entries, [] for nothing) and Egress its
	// egress ("internet" when the create left it out), ADR 0035 revised.
	Environment EnvRef       `json:"environment"`
	Reaches     spec.Reaches `json:"reaches"`
	Egress      spec.Egress  `json:"egress"`
	// Image is as asked, ImageRef pinned.
	Image    string `json:"image"`
	ImageRef string `json:"imageRef"`
	// Phase is queued, running, succeeded, failed, deadline_exceeded or
	// cancelled; Reason, of a failed one, exit, image, node_lost or
	// no_exit_status.
	Phase      string            `json:"phase"`
	Reason     string            `json:"reason"`
	Detail     string            `json:"detail"`
	ExitCode   *int              `json:"exitCode"`
	Result     string            `json:"result"`
	ResultPath string            `json:"resultPath"`
	Vcpus      int               `json:"vcpus"`
	MemoryMib  int               `json:"memoryMib"`
	Labels     map[string]string `json:"labels"`
	CreatedAt  string            `json:"createdAt"`
	StartedAt  string            `json:"startedAt"`
	FinishedAt string            `json:"finishedAt"`
	DeadlineAt string            `json:"deadlineAt"`
	// LogRetentionDays is what the create asked; 0 (absent): the sink's
	// default.
	LogRetentionDays int `json:"logRetentionDays,omitempty"`
}

// EnvRef names an environment in a view of what runs in it.
type EnvRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TenantJobList is GET /api/v1/jobs.
type TenantJobList struct {
	TenantJobs []TenantJobStatus `json:"jobs"`
}

// TenantJobLogsAnswer is GET /api/v1/jobs/{id}/logs.
type TenantJobLogsAnswer struct {
	Lines []string `json:"lines"`
}

func tenantJobStatus(r store.TenantJob) TenantJobStatus {
	return TenantJobStatus{ID: r.ID.String(), Environment: EnvRef{ID: r.EnvID.String(), Name: r.EnvName},
		Reaches: r.Reaches, Egress: egressView(r.Egress), Image: r.Image, ImageRef: r.ImageRef, Phase: r.Phase, Reason: r.Reason,
		Detail: r.Detail, ExitCode: r.ExitCode, Result: r.Result, ResultPath: r.ResultPath, Vcpus: r.VCPUs,
		MemoryMib: r.MemoryMiB, Labels: r.Labels, CreatedAt: rfc3339(r.CreatedAt), StartedAt: rfc3339(r.StartedAt),
		FinishedAt: rfc3339(r.FinishedAt), DeadlineAt: rfc3339(r.DeadlineAt), LogRetentionDays: r.LogRetentionDays}
}

// CreateTenantJob is POST /api/v1/jobs: a job of the tenant, created (true) or,
// for a key a create with the same request made, the job that create
// made (false). A key that names a job of another request is 422
// job_key_conflict; the secrets and image credentials need
// --secret-key-file (503). Its environment must exist and not be being
// deleted (404, 409), the entries of its reaches its deployed
// deployment's (422), its egress's blocks outside --deny-prefix (400).
func (s *Service) CreateTenantJob(ctx context.Context, tenant, key string, req CreateTenantJobRequest) (TenantJobStatus, bool, error) {
	rs, err := tenantJobSpec(req)
	if err != nil {
		return TenantJobStatus{}, false, err
	}
	if msg := spec.CheckPod(nil, rs.Reaches, rs.Egress, s.d.DenyPrefixes); msg != "" {
		return TenantJobStatus{}, false, errBadRequest("%s", msg)
	}
	e, err := resolveEnv(ctx, s.st, tenant, req.Environment, store.NoLock)
	if err != nil {
		return TenantJobStatus{}, false, err
	}
	rs.EnvID = e.ID
	rs.Key, rs.Hash = key, requestHash(req)
	r, created, err := s.ctl.CreateTenantJob(ctx, tenant, rs)
	if err != nil {
		return TenantJobStatus{}, false, tenantJobError(err)
	}
	return tenantJobStatus(r), created, nil
}

// requestHash is a create's hash for its key: the request without the
// values of its secrets and image credentials, which a caller mints anew
// when it sends a create again.
func requestHash(req CreateTenantJobRequest) string {
	names := slices.Sorted(maps.Keys(req.Secrets))
	req.Secrets = nil
	hasCreds := req.ImageCredentials != nil
	req.ImageCredentials = nil
	b, _ := json.Marshal(struct {
		CreateTenantJobRequest
		SecretNames []string `json:"secretNames"`
		Credentials bool     `json:"credentials"`
	}{req, names, hasCreds})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// tenantJobSpec validates a create (400 for what it gets wrong) and fills in
// the defaults.
func tenantJobSpec(req CreateTenantJobRequest) (ctl.TenantJobSpec, error) {
	rs := ctl.TenantJobSpec{Image: strings.TrimSpace(req.Image), Command: req.Command, Env: req.Env, Secrets: req.Secrets,
		ImageCredentials: req.ImageCredentials, ResultPath: req.ResultPath, Labels: req.Labels}
	if rs.Image == "" {
		return rs, errBadRequest("image is required")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return rs, errBadRequest("environment is required: the name or ID of the environment the job runs in (ADR 0035)")
	}
	if msg := spec.CheckPod(req.SecurityGroups, req.Reaches, req.Egress, nil); msg != "" {
		return rs, errBadRequest("%s", msg)
	}
	rs.Reaches, rs.Egress = req.Reaches, req.Egress
	for _, m := range []map[string]string{req.Env, req.Secrets} {
		for k := range m {
			if !spec.ValidEnvName(k) {
				return rs, errBadRequest("%q is not an environment variable's name", k)
			}
		}
	}
	for k := range req.Secrets {
		if _, ok := req.Env[k]; ok {
			return rs, errBadRequest("%s is in env and in secrets", k)
		}
	}
	if c := req.ImageCredentials; c != nil && (c.Username == "" || c.Password == "") {
		return rs, errBadRequest("imageCredentials need a username and a password")
	}
	rs.VCPUs, rs.MemoryMiB = spec.DefaultVCPUs, spec.DefaultMemoryMib
	if req.Vcpus != 0 {
		if req.Vcpus < spec.MinVCPUs || req.Vcpus > spec.MaxVCPUs {
			return rs, errBadRequest("vcpus %d is out of range: use %d..%d", req.Vcpus, spec.MinVCPUs, spec.MaxVCPUs)
		}
		rs.VCPUs = req.Vcpus
	}
	if req.MemoryMib != 0 {
		if req.MemoryMib < spec.MinMemoryMib || req.MemoryMib > spec.MaxMemoryMib {
			return rs, errBadRequest("memoryMib %d is out of range: use %d..%d", req.MemoryMib, spec.MinMemoryMib,
				spec.MaxMemoryMib)
		}
		rs.MemoryMiB = req.MemoryMib
	}
	deadline := req.Deadline
	if deadline == "" {
		deadline = spec.DefaultJobTimeout
	}
	d, err := time.ParseDuration(deadline)
	limit, _ := time.ParseDuration(spec.MaxJobTimeout)
	if err != nil || d <= 0 || d > limit {
		return rs, errBadRequest("deadline %q is not a duration of up to %s (90s, 15m, 1h30m)", req.Deadline, limit)
	}
	rs.Deadline = d
	if p := req.ResultPath; p != "" {
		clean := path.Clean(p)
		if !path.IsAbs(p) || clean == "/" || clean != p || clean == "/dev" || strings.HasPrefix(clean, "/dev/") {
			return rs, errBadRequest("resultPath %q must be an absolute, clean path of a file outside /dev", p)
		}
	}
	for k := range req.Labels {
		if k == "" {
			return rs, errBadRequest("a label's key is empty")
		}
	}
	if d := req.LogRetentionDays; d != 0 {
		if d < model.MinLogRetentionDays || d > model.MaxLogRetentionDays {
			return rs, errBadRequest("logRetentionDays %d is out of range: use %d..%d", d, model.MinLogRetentionDays,
				model.MaxLogRetentionDays)
		}
		rs.LogRetentionDays = d
	}
	return rs, nil
}

// tenantJobError maps the controller's errors of the jobs.
func tenantJobError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ctl.ErrNoEnvironment):
		return errNoEnvironment()
	case errors.Is(err, ctl.ErrNoObject), errors.Is(err, store.ErrNoRow):
		return errNoTenantJob()
	case errors.Is(err, ctl.ErrJobKeyConflict):
		return apierr.WithCode(http.StatusUnprocessableEntity, CodeJobKeyConflict, "%s", err.Error())
	case errors.Is(err, ctl.ErrJobImage):
		return apierr.New(http.StatusUnprocessableEntity, "%s", err.Error())
	case errors.Is(err, ctl.ErrEnvironmentDeleting):
		return apierr.New(http.StatusConflict, "%s", err.Error())
	case errors.Is(err, ctl.ErrUnknownReaches):
		return apierr.New(http.StatusUnprocessableEntity, "%s", err.Error())
	case errors.Is(err, ctl.ErrNoContainerLogs):
		return apierr.New(http.StatusNotImplemented, "%s", err.Error())
	case err.Error() == "secrets need --secret-key-file":
		return errNoSecrets()
	case err.Error() == "the tenant is being deleted":
		return apierr.New(http.StatusConflict, "%s", err.Error())
	}
	return err
}

func errNoTenantJob() error { return apierr.New(http.StatusNotFound, "no such job") }

// tenantJobID parses a job's ID; one that is none is no job (404).
func tenantJobID(s string) (podid.ID, error) {
	id, err := podid.Parse(s)
	if err != nil || len(id) != podid.Len {
		return "", errNoTenantJob()
	}
	return id, nil
}

// GetTenantJob is GET /api/v1/jobs/{id}.
func (s *Service) GetTenantJob(ctx context.Context, tenant, id string) (TenantJobStatus, error) {
	rid, err := tenantJobID(id)
	if err != nil {
		return TenantJobStatus{}, err
	}
	r, err := s.st.GetTenantJob(ctx, tenant, rid)
	if err != nil {
		return TenantJobStatus{}, tenantJobError(err)
	}
	return tenantJobStatus(r), nil
}

// ListTenantJobs is GET /api/v1/jobs?label=k=v…: the tenant's kept jobs that
// carry every label, newest first; envs, when not nil, keeps those of
// these environments.
func (s *Service) ListTenantJobs(ctx context.Context, tenant string, labels map[string]string, envs []uuid.UUID) (
	TenantJobList, error) {
	rows, err := s.st.ListTenantJobs(ctx, tenant, labels, envs)
	if err != nil {
		return TenantJobList{}, err
	}
	out := TenantJobList{TenantJobs: make([]TenantJobStatus, 0, len(rows))}
	for _, r := range rows {
		out.TenantJobs = append(out.TenantJobs, tenantJobStatus(r))
	}
	return out, nil
}

// TenantJobLogs is GET /api/v1/jobs/{id}/logs?lines=N (200 when not above 0, at
// most 10000): from the job's pod while it runs, its kept tail once it
// has ended.
func (s *Service) TenantJobLogs(ctx context.Context, tenant, id string, lines int) (TenantJobLogsAnswer, error) {
	rid, err := tenantJobID(id)
	if err != nil {
		return TenantJobLogsAnswer{}, err
	}
	if lines <= 0 {
		lines = defaultLogLines
	}
	out, err := s.ctl.TenantJobLog(ctx, tenant, rid, min(lines, maxLogLines))
	if err != nil {
		return TenantJobLogsAnswer{}, tenantJobError(err)
	}
	if out == nil {
		out = []string{}
	}
	return TenantJobLogsAnswer{Lines: out}, nil
}

// CancelTenantJob is POST /api/v1/jobs/{id}/cancel: the job as it is after the
// cancel, which changes nothing of a job that has ended.
func (s *Service) CancelTenantJob(ctx context.Context, tenant, id string) (TenantJobStatus, error) {
	rid, err := tenantJobID(id)
	if err != nil {
		return TenantJobStatus{}, err
	}
	r, err := s.ctl.CancelTenantJob(ctx, tenant, rid)
	if err != nil {
		return TenantJobStatus{}, tenantJobError(err)
	}
	return tenantJobStatus(r), nil
}

// DeleteTenantJob is DELETE /api/v1/jobs/{id}: a job that has not ended is
// cancelled first; its record goes.
func (s *Service) DeleteTenantJob(ctx context.Context, tenant, id string) error {
	rid, err := tenantJobID(id)
	if err != nil {
		return err
	}
	return tenantJobError(s.ctl.DeleteTenantJob(ctx, tenant, rid))
}

// egressView is an egress as the views show it: "internet" for none
// given.
func egressView(e *spec.Egress) spec.Egress {
	if e == nil {
		return spec.Egress{Mode: spec.EgressInternet}
	}
	return *e
}
