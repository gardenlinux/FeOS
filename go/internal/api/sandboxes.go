// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/config"
	envs "github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/quota"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The label of the token a create returns.
const firstTokenLabel = "default"

// mintSandboxToken returns a new sandbox token and its hash: 32 random
// bytes, base64url without padding (43 characters). Only the hash is
// stored; the token is shown once.
func mintSandboxToken() (token, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b[:])
	return token, store.HashToken(token), nil
}

// tenantOf is the tenant the request was authenticated as.
func tenantOf(r *http.Request) string {
	t, _ := auth.TenantFrom(r.Context())
	return t.Name
}

// errUnknownSandbox is the answer to a sandbox the tenant does not own,
// the same whether it is foreign, absent, an environment's instance (ADR
// 0018: the environments' API shows those), or in an environment the
// principal may not read (ADR 0035).
func errUnknownSandbox(id string) error {
	return apierr.New(http.StatusNotFound, "unknown sandbox %s", id)
}

// owned reads the sandbox of the path's {id} that the request's tenant
// owns; an environment's instance row and a tenant's job's pod are none.
// The route's need (onSandbox) checked the principal's role on its
// environment.
func (s *server) owned(r *http.Request) (store.Sandbox, error) {
	id, err := pathSandboxID(r)
	if err != nil {
		return store.Sandbox{}, err
	}
	sb, err := s.st.GetSandbox(r.Context(), id)
	if errors.Is(err, model.ErrNotFound) || (err == nil && (sb.Owner != tenantOf(r) || !sb.IsOwn())) {
		return store.Sandbox{}, errUnknownSandbox(id.String())
	}
	if err != nil {
		return store.Sandbox{}, fmt.Errorf("get %s: %w", id, err)
	}
	return sb, nil
}

// createSandboxRequest is POST /api/v1/sandboxes. It has no sandboxId:
// ferund mints every sandbox's ID (ADR 0019), and refuses a request that
// names one (errClientID).
type createSandboxRequest struct {
	// Image empty: the default image.
	Image string `json:"image"`
	// The size, fixed for the sandbox's life; 0: the default.
	Vcpus     int `json:"vcpus"`
	MemoryMib int `json:"memoryMib"`
	// Environment names the environment the sandbox is in (ADR 0035), by
	// name or ID; required, fixed for its life.
	Environment string `json:"environment"`
	// Reaches is what of the environment its pod reaches (ADR 0035,
	// revised): "all", or entries of its deployed deployment; left out:
	// nothing. Egress is its egress as a service's; left out: the
	// internet. Both are fixed at the create but Reaches, which PUT
	// …/reaches changes. securityGroups is gone: sent, it is 400.
	Reaches spec.Reaches `json:"reaches"`
	Egress  *spec.Egress `json:"egress"`
	// ImageCredentials are the login for the image's registry (ADR 0025),
	// sealed and never returned; nil: anonymous.
	ImageCredentials *model.RegistryCredentials `json:"imageCredentials"`
	// Credentials are the tenant's credentials its pod gets, at their
	// latest versions (ADR 0033).
	Credentials []string `json:"credentials"`
	// Workdir is the absolute directory its tools start in, fixed for its
	// life: bash runs there, and the file tools take a relative path as
	// relative to it. Empty: the root.
	Workdir string `json:"workdir"`
}

// maxWorkdir is the longest working directory a sandbox takes, in bytes.
const maxWorkdir = 1024

// checkWorkdir validates a create's workdir: "" or an absolute, clean
// path.
func checkWorkdir(dir string) error {
	if dir == "" {
		return nil
	}
	if len(dir) > maxWorkdir || !strings.HasPrefix(dir, "/") || path.Clean(dir) != dir || strings.ContainsRune(dir, 0) {
		return apierr.New(http.StatusBadRequest,
			"workdir must be an absolute, clean path of at most %d bytes, like /workspace/app; left out: the root", maxWorkdir)
	}
	return nil
}

// sandboxView is a sandbox as the sandbox routes show it: its status, its
// environment, what of it it reaches ("all", or the entries, [] for
// nothing) and its egress ("internet" when the create left it out), ADR
// 0035 revised, and its working directory ("/" when the create left it
// out).
type sandboxView struct {
	sandboxStatus
	Environment envs.EnvRef  `json:"environment"`
	Reaches     spec.Reaches `json:"reaches"`
	Egress      spec.Egress  `json:"egress"`
	Workdir     string       `json:"workdir"`
}

// viewOf renders a row for the sandbox routes, envName its environment's
// name.
func viewOf(sb store.Sandbox, envName string) sandboxView {
	egress := spec.Egress{Mode: spec.EgressInternet}
	if sb.Egress != nil {
		egress = *sb.Egress
	}
	return sandboxView{sandboxStatus: statusOf(sb), Environment: envs.EnvRef{ID: sb.EnvironmentID.String(), Name: envName},
		Reaches: sb.Reaches, Egress: egress, Workdir: cmp.Or(sb.Workdir, "/")}
}

// view renders a row with its environment's name read.
func (s *server) view(ctx context.Context, sb store.Sandbox) (sandboxView, error) {
	e, err := s.st.GetEnvironment(ctx, sb.EnvironmentID)
	if err != nil {
		return sandboxView{}, fmt.Errorf("environment of %s: %w", sb.ID, err)
	}
	return viewOf(sb, e.Name), nil
}

// errAttachable is the answer to a create that sends attachable.
func errAttachable() error {
	return apierr.New(http.StatusBadRequest, "attachable is gone with ADR 0035: every sandbox of an environment can run "+
		"its attached services; leave it out and name the sandbox's environment")
}

type createSandboxResponse struct {
	// Status carries the new sandbox's ID, status.sandboxId, which the
	// client keeps.
	Status sandboxView `json:"status"`
	// Token is the first token.
	Token string `json:"token,omitempty"`
	// Created is always true: every create makes a new sandbox.
	Created bool `json:"created"`
}

// errClientID is the answer to a create that names its sandbox's ID.
func errClientID() error {
	return apierr.New(http.StatusBadRequest,
		"sandboxId is not accepted: ferund mints a sandbox's ID; leave it out and keep status.sandboxId of the answer")
}

// namesID reports whether a create's body names a sandbox ID: a sandboxId
// that is neither null nor "".
func namesID(sent map[string]json.RawMessage) bool {
	raw, ok := sent["sandboxId"]
	if !ok {
		return false
	}
	switch strings.TrimSpace(string(raw)) {
	case "null", `""`:
		return false
	}
	return true
}

// size validates a create's size; zeros take the defaults.
func (s *server) size(req createSandboxRequest) (vcpus, memoryMiB int, err error) {
	if req.Vcpus < 0 || req.Vcpus > config.MaxVCPUs {
		return 0, 0, apierr.New(http.StatusBadRequest, "vcpus must be between %d and %d (0 = default %d)",
			config.MinVCPUs, config.MaxVCPUs, s.cfg.DefaultVCPUs)
	}
	if req.MemoryMib != 0 && (req.MemoryMib < config.MinMemoryMiB || req.MemoryMib > config.MaxMemoryMiB) {
		return 0, 0, apierr.New(http.StatusBadRequest, "memoryMib must be between %d and %d (0 = default %d)",
			config.MinMemoryMiB, config.MaxMemoryMiB, s.cfg.DefaultMemoryMiB)
	}
	vcpus, memoryMiB = req.Vcpus, req.MemoryMib
	if vcpus == 0 {
		vcpus = s.cfg.DefaultVCPUs
	}
	if memoryMiB == 0 {
		memoryMiB = s.cfg.DefaultMemoryMiB
	}
	return vcpus, memoryMiB, nil
}

// createSandbox is POST /api/v1/sandboxes: in one transaction, the tenant
// (refused while it is being deleted), its environment (FOR SHARE,
// refused while it is being deleted, the entries of reaches checked
// against its deployed deployment, 422), a new row under an ID ferund mints (ADR 0019),
// and the first token. Nothing reaches a node: the sandbox materialises at
// its first tool call. The answer, a 201, carries the sandbox's ID
// (status.sandboxId) and its token. A create is not idempotent: a request
// that names an ID is a 400 (errClientID). It needs write on the
// environment.
func (s *server) createSandbox(w http.ResponseWriter, r *http.Request) error {
	var req createSandboxRequest
	sent, err := decodeJSON(r, &req)
	if err != nil {
		return err
	}
	if namesID(sent) {
		return errClientID()
	}
	if _, ok := sent["attachable"]; ok {
		return errAttachable()
	}
	if strings.TrimSpace(req.Environment) == "" {
		return apierr.New(http.StatusBadRequest,
			"environment is required: the name or ID of the environment the sandbox is in (ADR 0035)")
	}
	if msg := spec.CheckPod(sent["securityGroups"], req.Reaches, req.Egress, s.cfg.DenyPrefixes); msg != "" {
		return apierr.New(http.StatusBadRequest, "%s", msg)
	}
	if err := checkWorkdir(req.Workdir); err != nil {
		return err
	}
	env, err := s.writableEnv(r, req.Environment)
	if err != nil {
		return err
	}
	vcpus, memoryMiB, err := s.size(req)
	if err != nil {
		return err
	}
	if err := s.checkCredentials(req.ImageCredentials); err != nil {
		return err
	}
	if len(req.Credentials) > 0 {
		b := s.deps.Bindings
		if b == nil || !b.Enabled() {
			return apierr.New(http.StatusUnprocessableEntity, "credentials: HTTP bindings are not enabled on this platform")
		}
		if err := b.CheckSandboxCredentials(r.Context(), tenantOf(r), req.Credentials); err != nil {
			return err
		}
	}
	image := req.Image
	if image == "" {
		image = s.cfg.DefaultImage
	}
	tenant := tenantOf(r)
	token, hash, err := mintSandboxToken()
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	var row store.Sandbox
	err = s.st.InTx(r.Context(), func(tx *store.Store) error {
		t, err := quota.LockAndCheck(r.Context(), tx, tenant, quota.Use{Sandboxes: 1, VCPUs: int64(vcpus),
			MemoryMiB: int64(memoryMiB)})
		if isNoRow(err) {
			return apierr.New(http.StatusUnauthorized, "unknown tenant token")
		}
		if err != nil {
			return err
		}
		if t.Deleting() {
			return apierr.New(http.StatusConflict, "tenant %s is being deleted", tenant)
		}
		if err := podEnvError(ctl.LockPodEnvironment(r.Context(), tx, tenant, env.ID, req.Reaches)); err != nil {
			return err
		}
		row, err = tx.CreateMintedSandbox(r.Context(), func(id podid.ID) store.NewSandbox {
			// Its pod is in env-<environment> and its own workload's
			// group, and resolves its egress's names (ADR 0034, 0035
			// revised).
			tmpl := model.SandboxTemplate(env.ID, id.String(), req.Egress.RestrictsNames(), req.Egress.NameDomains())
			return store.NewSandbox{ID: id, Owner: tenant, Image: image, VCPUs: vcpus, MemoryMiB: memoryMiB,
				EnvironmentID: env.ID, Reaches: req.Reaches, Egress: req.Egress, PodTemplate: tmpl,
				LogLabels: ctl.SandboxLogLabels(tenant, id, env.ID), MadeBy: auth.MadeBy(r.Context()), Workdir: req.Workdir}
		})
		if err != nil {
			return err
		}
		if err := s.keepCredentials(r.Context(), tx, row.ID, req.ImageCredentials); err != nil {
			return err
		}
		_, err = tx.CreateSandboxToken(r.Context(), hash, row.ID, firstTokenLabel, auth.MadeBy(r.Context()))
		return err
	})
	if err != nil {
		return passOr(err, "create")
	}
	if len(req.Credentials) > 0 {
		if err := s.deps.Bindings.SetSandboxCredentials(r.Context(), tenant, row.ID, req.Credentials); err != nil {
			return passOr(err, "create: its credentials")
		}
	}
	return writeJSON(w, http.StatusCreated, createSandboxResponse{Status: viewOf(row, env.Name), Token: token, Created: true})
}

// podEnvError is the answer of ctl.LockPodEnvironment's errors.
func podEnvError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ctl.ErrNoEnvironment):
		return errNoEnvironment()
	case errors.Is(err, ctl.ErrEnvironmentDeleting):
		return apierr.New(http.StatusConflict, "%s", err.Error())
	case errors.Is(err, ctl.ErrUnknownReaches):
		return apierr.New(http.StatusUnprocessableEntity, "%s", err.Error())
	}
	return err
}

// writableEnv is the environment of the request's tenant that ref names
// (its name or ID), on which the principal holds write: none it may see
// is 404, too low a role 403.
func (s *server) writableEnv(r *http.Request, ref string) (store.Environment, error) {
	t, _ := auth.TenantFrom(r.Context())
	id, err := s.st.EnvironmentRef(r.Context(), t.Name, ref)
	if isNoRow(err) {
		return store.Environment{}, errNoEnvironment()
	}
	if err != nil {
		return store.Environment{}, fmt.Errorf("environment %s: %w", ref, err)
	}
	if err := allowEnvID(t, id, write); err != nil {
		return store.Environment{}, err
	}
	e, err := s.st.GetEnvironment(r.Context(), id)
	if isNoRow(err) {
		return store.Environment{}, errNoEnvironment()
	}
	return e, err
}

// readableEnvs is the filter of a list of what runs in environments
// (sandboxes, jobs): ?environment= names one the principal may read (404
// when it may not see it); without it nil (every one) for a reader of the
// tenant, else the environments the principal reads.
func (s *server) readableEnvs(r *http.Request) ([]uuid.UUID, error) {
	t, _ := auth.TenantFrom(r.Context())
	if ref := r.URL.Query().Get("environment"); ref != "" {
		id, err := s.st.EnvironmentRef(r.Context(), t.Name, ref)
		if isNoRow(err) {
			return nil, errNoEnvironment()
		}
		if err != nil {
			return nil, fmt.Errorf("environment %s: %w", ref, err)
		}
		if t.EnvRole(id) < read {
			return nil, errNoEnvironment()
		}
		return []uuid.UUID{id}, nil
	}
	if t.MayOnTenant(read) {
		return nil, nil
	}
	out := []uuid.UUID{}
	for id, role := range t.Envs {
		if role >= read {
			out = append(out, id)
		}
	}
	return out, nil
}

// sandboxReaches is the body of PUT /api/v1/sandboxes/{id}/reaches.
type sandboxReaches struct {
	Reaches *spec.Reaches `json:"reaches"`
}

// putSandboxReaches is PUT /api/v1/sandboxes/{id}/reaches {reaches}: what
// of its environment the sandbox reaches (ADR 0035, revised), checked as
// a create's. The change is a change of its targets' rules, which
// env-groups puts on the nodes in place: no move, no new address, no
// hibernation. 200 with the sandbox.
func (s *server) putSandboxReaches(w http.ResponseWriter, r *http.Request) error {
	id, err := pathSandboxID(r)
	if err != nil {
		return err
	}
	var req sandboxReaches
	sent, err := decodeJSON(r, &req)
	if err != nil {
		return err
	}
	if req.Reaches == nil {
		return apierr.New(http.StatusBadRequest, `reaches is required: "all" or a list of {"service": name, "port": `+
			`port} and {"addon": name}`)
	}
	if msg := spec.CheckPod(sent["securityGroups"], *req.Reaches, nil, nil); msg != "" {
		return apierr.New(http.StatusBadRequest, "%s", msg)
	}
	tenant := tenantOf(r)
	var row store.Sandbox
	err = s.st.InTx(r.Context(), func(tx *store.Store) error {
		sb, err := tx.LockSandbox(r.Context(), id, store.ForUpdate)
		if errors.Is(err, model.ErrNotFound) || (err == nil && (sb.Owner != tenant || !sb.IsOwn())) {
			return errUnknownSandbox(id.String())
		}
		if err != nil {
			return err
		}
		if err := podEnvError(ctl.LockPodEnvironment(r.Context(), tx, tenant, sb.EnvironmentID, *req.Reaches)); err != nil {
			return err
		}
		if _, err := tx.SetSandboxReaches(r.Context(), id, *req.Reaches); err != nil {
			return err
		}
		row, err = tx.GetSandbox(r.Context(), id)
		return err
	})
	if err != nil {
		return passOr(err, "reaches")
	}
	v, err := s.view(r.Context(), row)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

// sandboxCredentials is the answer of the credentials a sandbox names.
type sandboxCredentials struct {
	Credentials []string `json:"credentials"`
}

// getSandboxCredentials is GET /api/v1/sandboxes/{id}/credentials: the
// tenant's credentials the sandbox names (ADR 0033).
func (s *server) getSandboxCredentials(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	names, err := s.deps.Bindings.SandboxCredentialNames(r.Context(), sb.ID)
	if err != nil {
		return passOr(err, "credentials")
	}
	return writeJSON(w, http.StatusOK, sandboxCredentials{Credentials: names})
}

// putSandboxCredentials is PUT /api/v1/sandboxes/{id}/credentials
// {credentials}: the whole list of the tenant's credentials its pod gets;
// a running pod gets them at once.
func (s *server) putSandboxCredentials(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	var req sandboxCredentials
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.Credentials == nil {
		req.Credentials = []string{}
	}
	if err := s.deps.Bindings.SetSandboxCredentials(r.Context(), tenantOf(r), sb.ID, req.Credentials); err != nil {
		return passOr(err, "credentials")
	}
	names, err := s.deps.Bindings.SandboxCredentialNames(r.Context(), sb.ID)
	if err != nil {
		return passOr(err, "credentials")
	}
	return writeJSON(w, http.StatusOK, sandboxCredentials{Credentials: names})
}

// listSandboxes is GET /api/v1/sandboxes[?environment=]: the tenant's
// that the principal may read (readableEnvs), oldest first, without its
// environments' instances.
func (s *server) listSandboxes(w http.ResponseWriter, r *http.Request) error {
	envIDs, err := s.readableEnvs(r)
	if err != nil {
		return err
	}
	rows, err := s.st.ListSandboxes(r.Context(), tenantOf(r), envIDs)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, sb := range rows {
		ids = append(ids, sb.EnvironmentID)
	}
	names, err := s.st.EnvironmentNames(r.Context(), ids)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	out := struct {
		Sandboxes []sandboxView `json:"sandboxes"`
	}{Sandboxes: make([]sandboxView, 0, len(rows))}
	for _, sb := range rows {
		out.Sandboxes = append(out.Sandboxes, viewOf(sb, names[sb.EnvironmentID]))
	}
	return writeJSON(w, http.StatusOK, out)
}

// getSandbox is GET /api/v1/sandboxes/{id}: a read that never wakes it.
func (s *server) getSandbox(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	v, err := s.view(r.Context(), sb)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

// deleteSandbox is DELETE /api/v1/sandboxes/{id}: the row goes in one
// transaction (§9 "Delete"), its pod through tombstones. The answer is the
// status read, desired state deleted, and client state absent when a pod
// is being deleted. An environment's instance is not deleted here: its
// ID is fixed to it for its row's life, so the read before is enough.
func (s *server) deleteSandbox(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	id := sb.ID
	row, tombstoned, err := s.deps.Engine.Delete(r.Context(), tenantOf(r), id)
	if errors.Is(err, model.ErrNotFound) {
		return errUnknownSandbox(id.String())
	}
	if err != nil {
		return internalf("delete: %v", err)
	}
	st, err := s.view(r.Context(), row)
	if err != nil {
		return err
	}
	st.DesiredState = desiredDeleted
	if tombstoned {
		st.ClientState, st.State = stateAbsent, stateAbsent
	}
	return writeJSON(w, http.StatusOK, st)
}

// pauseSandbox is POST /api/v1/sandboxes/{id}/pause: a running sandbox is
// paused (waiting up to a minute; the pause goes on after); a paused,
// hibernated or pending one has nothing to do. The answer is the status
// as read before.
func (s *server) pauseSandbox(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	err = s.deps.Engine.Pause(r.Context(), sb.ID)
	switch {
	case err == nil, errors.Is(err, model.ErrPrecondition):
		// A recheck that found the row changed: someone else moved it.
	case errors.Is(err, model.ErrNotFound):
		return errUnknownSandbox(sb.ID.String())
	default:
		return internalf("pause: %v", err)
	}
	return s.writeView(w, r, sb)
}

// writeView answers a sandbox's view, 200.
func (s *server) writeView(w http.ResponseWriter, r *http.Request, sb store.Sandbox) error {
	v, err := s.view(r.Context(), sb)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

// resumeSandbox is POST /api/v1/sandboxes/{id}/resume: Ensure (within the
// preparation budget) and activity. The answer is the status as read
// before.
func (s *server) resumeSandbox(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	err = s.deps.Engine.Resume(r.Context(), sb.ID)
	switch {
	case err == nil:
	case errors.Is(err, model.ErrNotFound):
		return errUnknownSandbox(sb.ID.String())
	default:
		return internalf("resume: %v", err)
	}
	return s.writeView(w, r, sb)
}

// tokenInfo is a sandbox token as listed: its hash, never the token.
type tokenInfo struct {
	Hash       string `json:"hash"`
	HashPrefix string `json:"hashPrefix"`
	SandboxID  string `json:"sandboxId"`
	Label      string `json:"label"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
}

// listSandboxTokens is GET /api/v1/sandboxes/{id}/tokens.
func (s *server) listSandboxTokens(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	toks, err := s.st.ListSandboxTokens(r.Context(), sb.ID)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	out := struct {
		Tokens []tokenInfo `json:"tokens"`
	}{Tokens: make([]tokenInfo, 0, len(toks))}
	for _, t := range toks {
		out.Tokens = append(out.Tokens, tokenInfo{
			Hash: t.Hash, HashPrefix: t.Hash[:8], SandboxID: t.SandboxID.String(), Label: t.Label,
			CreatedAt: rfc3339(t.CreatedAt), LastUsedAt: rfc3339(t.LastUsedAt),
		})
	}
	return writeJSON(w, http.StatusOK, out)
}

// mintedToken is the answer of a token mint: the token, shown once.
type mintedToken struct {
	Token      string `json:"token"`
	HashPrefix string `json:"hashPrefix"`
	CreatedAt  string `json:"createdAt"`
}

// createSandboxToken is POST /api/v1/sandboxes/{id}/tokens {label?}, in
// one transaction with the row FOR SHARE, so that a delete cannot slip in
// between the ownership check and the insert. An environment's instance
// gets no token.
func (s *server) createSandboxToken(w http.ResponseWriter, r *http.Request) error {
	id, err := pathSandboxID(r)
	if err != nil {
		return err
	}
	var req struct {
		Label string `json:"label"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	token, hash, err := mintSandboxToken()
	if err != nil {
		return fmt.Errorf("create token: %w", err)
	}
	var tok store.SandboxToken
	err = s.st.InTx(r.Context(), func(tx *store.Store) error {
		sb, err := tx.LockSandbox(r.Context(), id, store.ForShare)
		if errors.Is(err, model.ErrNotFound) || (err == nil && (sb.Owner != tenantOf(r) || !sb.IsOwn())) {
			return errUnknownSandbox(id.String())
		}
		if err != nil {
			return err
		}
		tok, err = tx.CreateSandboxToken(r.Context(), hash, id, req.Label, auth.MadeBy(r.Context()))
		if errors.Is(err, model.ErrNotFound) {
			return errUnknownSandbox(id.String())
		}
		return err
	})
	if err != nil {
		return passOr(err, "create token")
	}
	return writeJSON(w, http.StatusCreated, mintedToken{Token: token, HashPrefix: hash[:8], CreatedAt: rfc3339(tok.CreatedAt)})
}

// revokeSandboxToken is DELETE /api/v1/sandboxes/{id}/tokens/{hash}. The
// hash names the token; a hash of another sandbox's token is not revoked
// through this path (404 unknown token).
func (s *server) revokeSandboxToken(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	hash := r.PathValue("hash")
	tok, err := s.st.GetSandboxToken(r.Context(), hash)
	switch {
	case err == nil && tok.SandboxID != sb.ID:
		return apierr.New(http.StatusNotFound, "unknown token")
	case err != nil && !isNoRow(err):
		return fmt.Errorf("revoke token: %w", err)
	}
	ok, err := s.st.DeleteSandboxToken(r.Context(), sb.ID, hash)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return writeJSON(w, http.StatusOK, struct {
		Revoked bool `json:"revoked"`
	}{ok})
}

// emptyAccess is the access route's answer while the bindings are off.
var emptyAccess = json.RawMessage(`{"kube":null,"ssh":null,"denies":[]}`)

// access is GET /api/v1/sandboxes/{id}/access: the binding sets of the
// tenant's sandbox with their use; the empty object while bindings are
// off.
func (s *server) access(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.owned(r); err != nil {
		return err
	}
	if s.deps.Bindings == nil {
		return writeJSON(w, http.StatusOK, emptyAccess)
	}
	s.deps.Bindings.Access(w, r)
	return nil
}

// listTools is GET /api/v1/tools: the tool baseline, which MCP serves as
// tools/list, and the preparation budget, after which a client that waits
// for a tool call's sandbox gives up.
func (s *server) listTools(w http.ResponseWriter, _ *http.Request) error {
	type toolInfo struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	out := struct {
		Tools             []toolInfo `json:"tools"`
		PreparationWaitMs int64      `json:"preparationWaitMs"`
	}{Tools: []toolInfo{}, PreparationWaitMs: s.cfg.PreparationWaitMs}
	if s.deps.Specs != nil {
		for _, t := range s.deps.Specs() {
			out.Tools = append(out.Tools, toolInfo{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

// checkCredentials refuses image credentials that cannot be kept: half
// given, too long, or without --secret-key-file to seal them.
func (s *server) checkCredentials(c *model.RegistryCredentials) error {
	if c == nil || c.Anonymous() {
		return nil
	}
	if err := c.Check(); err != nil {
		return apierr.New(http.StatusBadRequest, "imageCredentials: %v", err)
	}
	if s.deps.Sealer == nil {
		return apierr.New(http.StatusServiceUnavailable, "image credentials need --secret-key-file on the control plane")
	}
	return nil
}

// keepCredentials seals and keeps the sandbox's image credentials; none
// keeps nothing.
func (s *server) keepCredentials(ctx context.Context, st *store.Store, id podid.ID, c *model.RegistryCredentials) error {
	if c == nil || c.Anonymous() {
		return nil
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sealed, err := s.deps.Sealer.Seal(plain, store.ImageCredentialsAAD(id))
	if err != nil {
		return fmt.Errorf("seal the image credentials: %w", err)
	}
	return st.SetImageCredentials(ctx, id, sealed)
}

// putImageCredentials is PUT /api/v1/sandboxes/{id}/image-credentials
// {username, password}: the sandbox's login for its image's registry,
// replacing any (ADR 0025). Its next create asks the registry with it;
// a pod that runs keeps running. 204.
func (s *server) putImageCredentials(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	var c model.RegistryCredentials
	if _, err := decodeJSON(r, &c); err != nil {
		return err
	}
	if c.Anonymous() {
		return apierr.New(http.StatusBadRequest, "imageCredentials: a username and a password; DELETE removes them")
	}
	if err := s.checkCredentials(&c); err != nil {
		return err
	}
	if err := s.keepCredentials(r.Context(), s.st, sb.ID, &c); err != nil {
		return passOr(err, "image credentials")
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// deleteImageCredentials is DELETE /api/v1/sandboxes/{id}/image-credentials:
// the sandbox's creates are anonymous again. 204, also when it had none.
func (s *server) deleteImageCredentials(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	if _, err := s.st.DeleteImageCredentials(r.Context(), sb.ID); err != nil {
		return passOr(err, "image credentials")
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// passOr returns an *apierr.Error of err as it is, and anything else as a
// 500 "<what>: <err>".
func passOr(err error, what string) error {
	var e *apierr.Error
	if errors.As(err, &e) {
		return e
	}
	return internalf("%s: %v", what, err)
}
