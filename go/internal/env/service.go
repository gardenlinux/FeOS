// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/env/envdeps"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/quota"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The operations of the environments' REST routes (the contract's §5),
// one method each. tenant is the request's tenant (auth.Tenant.Name); env
// is the path's {env}: the environment's UUID or its name. An environment
// the tenant does not have, foreign or absent, is 404 "no such
// environment"; a change of one being deleted is 409. Errors are
// *apierr.Error with their statuses, or an InvalidSpecError (422) or a
// BaseMismatchError (409), which WriteError answers; any other error is a
// 500.

// The bounds of the listings.
const (
	defaultDeployments = 50
	maxDeployments     = 1000
	defaultLogLines    = 200
	maxLogLines        = 10000
)

// tagName is a tag's rule (env_tags).
var tagName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// CreateEnv is POST /api/v1/environments: the new environment's status
// and created true (201), or the existing one's and false when the same
// ID and name exist (200); 409 CodeNameTaken or CodeIDTaken. A deleting
// tenant is refused.
func (s *Service) CreateEnv(ctx context.Context, tenant string, req CreateEnvRequest) (EnvStatus, bool, error) {
	id, err := checkNewEnv(req.Name, req.ID, req.Labels)
	if err != nil {
		return EnvStatus{}, false, err
	}
	var row store.Environment
	var created bool
	err = s.inTxRetry(ctx, func(tx *store.Store) error {
		t, err := lockQuota(ctx, tx, tenant)
		if err != nil {
			return err
		}
		var use quota.Use
		if t.Limits.Any() {
			if use, err = quota.Of(ctx, tx, tenant, uuid.Nil); err != nil {
				return err
			}
		}
		row, created, err = insertEnv(ctx, tx, tenant, id, req.Name, req.Labels, true)
		if err != nil || !created {
			return err
		}
		// Counted before the insert: an environment that exists already
		// asks for nothing.
		return quota.Check(t.Limits, use, quota.Use{Environments: 1})
	})
	if err != nil {
		return EnvStatus{}, false, err
	}
	st, err := s.status(ctx, row)
	return st, created, err
}

// ListEnvs is GET /api/v1/environments: the tenant's environments with
// every label of labels (?label=k=v, repeatable, AND), by name.
func (s *Service) ListEnvs(ctx context.Context, tenant string, labels map[string]string) (EnvList, error) {
	rows, err := s.st.EnvSummaries(ctx, tenant, labels)
	if err != nil {
		return EnvList{}, fmt.Errorf("list environments: %w", err)
	}
	var public []uuid.UUID
	for _, r := range rows {
		if r.Public && !r.Deleting {
			public = append(public, r.ID)
		}
	}
	domains, err := s.st.ServedDomainNames(ctx, public)
	if err != nil {
		return EnvList{}, fmt.Errorf("list environments: %w", err)
	}
	out := EnvList{Environments: make([]EnvSummary, 0, len(rows))}
	for _, r := range rows {
		out.Environments = append(out.Environments, summaryOf(r, s.d.AppsZone, domains[r.ID]))
	}
	return out, nil
}

// GetEnv is GET /api/v1/environments/{env}: its status (§4.9), a read
// that wakes nothing.
func (s *Service) GetEnv(ctx context.Context, tenant, env string) (EnvStatus, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return EnvStatus{}, err
	}
	return s.status(ctx, e)
}

// DeleteEnv is DELETE /api/v1/environments/{env}: marks it deleting
// (202); the controller deletes its instances, volumes and groups, then
// the environment. Deleting one that is being deleted is no error.
func (s *Service) DeleteEnv(ctx context.Context, tenant, env string) error {
	return s.st.InTx(ctx, func(tx *store.Store) error {
		e, err := resolveEnv(ctx, tx, tenant, env, store.ForUpdate)
		if err != nil || e.Deleting {
			return err
		}
		after := e
		after.Deleting = true
		if _, err := tx.UpdateEnvironment(ctx, e, after); err != nil {
			return err
		}
		if err := s.envChanged(ctx, tx, e.ID); err != nil {
			return err
		}
		return wake(ctx, tx, ctl.LoopDelete, ctl.HintReconcile)
	})
}

// PlanSpec is POST /api/v1/environments/plan: a spec validated without an
// environment (spec.ModeSpec): no secrets, no deployment to compare with, but
// the tenant's sandboxes and the images.
func (s *Service) PlanSpec(ctx context.Context, tenant string, req PlanSpecRequest) (PlanSpecAnswer, error) {
	sp, fs := spec.Parse(req.Spec)
	if !spec.HasErrors(fs) {
		vc, pins, err := s.loadContext(ctx, tenant, nil, sp, spec.ModeSpec, map[string]spec.SecretInfo{})
		if err != nil {
			return PlanSpecAnswer{}, err
		}
		fs = append(fs, spec.Validate(vc, sp, nil)...)
		_, nfs := spec.Normalise(sp, pins)
		fs = append(fs, nfs...)
	}
	fs = spec.SortFindings(fs)
	return PlanSpecAnswer{Valid: !spec.HasErrors(fs), Findings: nonNilFindings(fs)}, nil
}

// Plan is POST /api/v1/environments/{env}/plan: parse, validate, resolve
// the images (unresolvable: a warning), normalise, and diff against
// req.Base or the current deployment. It changes nothing.
func (s *Service) Plan(ctx context.Context, tenant, env string, req PlanRequest) (PlanAnswer, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return PlanAnswer{}, err
	}
	p, err := s.plan(ctx, tenant, e, req.Spec, req.Base, spec.ModePlan, nil)
	if err != nil {
		return PlanAnswer{}, err
	}
	return p.answer(), nil
}

// Apply is POST /api/v1/environments/{env}/deployments: plan, refuse on any
// error (InvalidSpecError), pin the images (unresolvable: an error), and
// in one transaction check req.Base against the current deployment
// (BaseMismatchError), insert deployment current+1 waiting, replace the
// older waiting and deploying ones and wake the controller. A normalised
// spec equal to the current deployment's answers that deployment Unchanged
// (200), unless it failed.
func (s *Service) Apply(ctx context.Context, tenant, env string, req ApplyRequest) (ApplyAnswer, error) {
	if err := checkDeploymentMeta(req.Author, req.Message, req.Annotations); err != nil {
		return ApplyAnswer{}, err
	}
	return s.apply(ctx, tenant, env, applyArgs{raw: req.Spec, base: req.Base, author: req.Author, message: req.Message,
		annotations: req.Annotations, origin: model.OriginApply})
}

// Deployments is GET /api/v1/environments/{env}/deployments: the newest limit
// deployments (default 50), newest first.
func (s *Service) Deployments(ctx context.Context, tenant, env string, limit int) (DeploymentList, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return DeploymentList{}, err
	}
	if limit <= 0 {
		limit = defaultDeployments
	}
	limit = min(limit, maxDeployments)
	rows, err := s.st.ListDeployments(ctx, e.ID, limit)
	if err != nil {
		return DeploymentList{}, fmt.Errorf("deployments: %w", err)
	}
	tags, err := s.tagsByDeployment(ctx, e.ID)
	if err != nil {
		return DeploymentList{}, err
	}
	out := DeploymentList{Deployments: make([]DeploymentInfo, 0, len(rows))}
	for _, r := range rows {
		out.Deployments = append(out.Deployments, deploymentInfo(r, tags[r.Number]))
	}
	return out, nil
}

// Deployment is GET /api/v1/environments/{env}/deployments/{n}: the deployment,
// its spec and its diff against deployment n-1; 404 "no such deployment".
func (s *Service) Deployment(ctx context.Context, tenant, env string, number int) (DeploymentDetail, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return DeploymentDetail{}, err
	}
	r, err := s.st.GetDeployment(ctx, e.ID, number)
	if errors.Is(err, store.ErrNoRow) {
		return DeploymentDetail{}, errNoDeployment()
	}
	if err != nil {
		return DeploymentDetail{}, fmt.Errorf("deployment %d: %w", number, err)
	}
	sp, err := s.deploymentSpec(ctx, e, number)
	if err != nil {
		return DeploymentDetail{}, err
	}
	var prev *spec.Spec
	if number > 1 {
		if prev, err = s.deploymentSpec(ctx, e, number-1); err != nil && !isNotFound(err) {
			return DeploymentDetail{}, err
		}
	}
	tags, err := s.tagsByDeployment(ctx, e.ID)
	if err != nil {
		return DeploymentDetail{}, err
	}
	return DeploymentDetail{DeploymentInfo: deploymentInfo(r, tags[number]), Spec: r.Spec,
		Diff: spec.DiffWith(prev, sp, s.addonEffect)}, nil
}

// isNotFound reports an answer 404.
func isNotFound(err error) bool {
	var ae *apierr.Error
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// Revert is POST /api/v1/environments/{env}/deployments/{n}/revert: Apply of
// deployment n's spec with origin revert, originRef n, and n's annotations
// plus "ferun.revertOf": n. A secret version n pins that was deleted is an
// error finding.
func (s *Service) Revert(ctx context.Context, tenant, env string, number int, req RevertRequest) (ApplyAnswer, error) {
	if err := checkDeploymentMeta(req.Author, req.Message, nil); err != nil {
		return ApplyAnswer{}, err
	}
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return ApplyAnswer{}, err
	}
	r, err := s.st.GetDeployment(ctx, e.ID, number)
	if errors.Is(err, store.ErrNoRow) {
		return ApplyAnswer{}, errNoDeployment()
	}
	if err != nil {
		return ApplyAnswer{}, fmt.Errorf("deployment %d: %w", number, err)
	}
	return s.apply(ctx, tenant, e.ID.String(), applyArgs{raw: r.Spec, base: req.Base, author: req.Author,
		message: req.Message, annotations: revertAnnotations(r), origin: model.OriginRevert,
		originRef: strconv.Itoa(number)})
}

// currentRaw is the current deployment's spec as stored, nil without one.
func (s *Service) currentRaw(ctx context.Context, e store.Environment) ([]byte, error) {
	if e.Deployment == 0 {
		return nil, nil
	}
	r, err := s.st.GetDeployment(ctx, e.ID, e.Deployment)
	if err != nil {
		return nil, fmt.Errorf("deployment %d: %w", e.Deployment, err)
	}
	return r.Spec, nil
}

// PutObject is PUT /api/v1/environments/{env}/objects/{kind}/{name}: the
// current deployment's spec with the object set, applied (origin object,
// originRef "<kind>/<name>") or, with DryRun, planned against req.Base.
// kind is spec.KindService, KindJob, KindAddon, KindRoute or KindSecret.
func (s *Service) PutObject(ctx context.Context, tenant, env, kind, name string, req PutObjectRequest) (ObjectAnswer, error) {
	if len(req.Object) == 0 {
		return ObjectAnswer{}, errBadRequest("object is required: the object as a spec has it under its kind")
	}
	return s.objectChange(ctx, tenant, env, kind, name, req.Object, req.Base, req.DryRun, req.Author, req.Message)
}

// DeleteObject is DELETE /api/v1/environments/{env}/objects/{kind}/{name}:
// the current deployment's spec without the object, applied or planned; 404
// "no such object" when the current deployment has none.
func (s *Service) DeleteObject(ctx context.Context, tenant, env, kind, name string, req DeleteObjectRequest) (ObjectAnswer, error) {
	return s.objectChange(ctx, tenant, env, kind, name, nil, req.Base, req.DryRun, req.Author, req.Message)
}

// objectChange is PutObject (object set) and DeleteObject (nil).
func (s *Service) objectChange(ctx context.Context, tenant, env, kind, name string, object []byte, base int, dryRun bool,
	author, message string) (ObjectAnswer, error) {
	if err := checkDeploymentMeta(author, message, nil); err != nil {
		return ObjectAnswer{}, err
	}
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return ObjectAnswer{}, err
	}
	current, err := s.currentRaw(ctx, e)
	if err != nil {
		return ObjectAnswer{}, err
	}
	raw, had, err := withObject(current, kind, name, object)
	if err != nil {
		return ObjectAnswer{}, err
	}
	if object == nil && !had {
		return ObjectAnswer{}, errNoObject(kind, name)
	}
	if dryRun {
		p, err := s.plan(ctx, tenant, e, raw, &base, spec.ModePlan, nil)
		if err != nil {
			return ObjectAnswer{}, err
		}
		return ObjectAnswer{DryRun: true, Plan: p.answer()}, nil
	}
	a, err := s.apply(ctx, tenant, e.ID.String(), applyArgs{raw: raw, base: base, author: author, message: message,
		origin: model.OriginObject, originRef: kind + "/" + name})
	if err != nil {
		return ObjectAnswer{}, err
	}
	return ObjectAnswer{Apply: a}, nil
}

// Clone is POST /api/v1/environments/{env}/clone: a new environment
// (201, its status) with the secrets of req.Secrets (copied: the
// source's latest version, unsealed and sealed again for the new
// environment; or set), whose deployment 1 (origin clone, originRef "<env
// id>/<number>", annotations copied) is req.Spec or the source deployment's
// spec without its attached services. No data is copied (Data is an
// error finding); a secret reference the new environment would not have
// is one too.
func (s *Service) Clone(ctx context.Context, tenant, env string, req CloneRequest) (EnvStatus, error) {
	return s.clone(ctx, tenant, env, req)
}

// tagOf is a tag as the answers show it.
func tagOf(t store.EnvTag) Tag {
	return Tag{Tag: t.Tag, Deployment: t.Number, MovedBy: t.MovedBy, MovedAt: rfc3339(t.MovedAt)}
}

// Tags is GET /api/v1/environments/{env}/tags.
func (s *Service) Tags(ctx context.Context, tenant, env string) (TagList, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return TagList{}, err
	}
	tags, err := s.st.ListTags(ctx, e.ID)
	if err != nil {
		return TagList{}, fmt.Errorf("tags: %w", err)
	}
	out := TagList{Tags: make([]Tag, 0, len(tags))}
	for _, t := range tags {
		out.Tags = append(out.Tags, tagOf(t))
	}
	return out, nil
}

// Tag is GET /api/v1/environments/{env}/tags/{tag}; 404 "no such tag".
func (s *Service) Tag(ctx context.Context, tenant, env, tag string) (Tag, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return Tag{}, err
	}
	t, err := s.st.GetTag(ctx, e.ID, tag)
	if errors.Is(err, store.ErrNoRow) {
		return Tag{}, errNoTag()
	}
	if err != nil {
		return Tag{}, fmt.Errorf("tag %s: %w", tag, err)
	}
	return tagOf(t), nil
}

// PutTag is PUT /api/v1/environments/{env}/tags/{tag}: set or move the
// tag to a deployment (movedBy the author, else the tenant); 404 "no such
// deployment".
func (s *Service) PutTag(ctx context.Context, tenant, env, tag string, req PutTagRequest) (Tag, error) {
	if !tagName.MatchString(tag) {
		return Tag{}, errBadRequest("tag %q must match %s", tag, tagName)
	}
	if err := checkDeploymentMeta(req.Author, "", nil); err != nil {
		return Tag{}, err
	}
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return Tag{}, err
	}
	if req.Deployment < 1 {
		return Tag{}, errNoDeployment()
	}
	by := req.Author
	if by == "" {
		by = tenant
	}
	t, err := s.st.PutTag(ctx, store.EnvTag{EnvID: e.ID, Tag: tag, Number: req.Deployment, MovedBy: by,
		MadeBy: auth.MadeBy(ctx)})
	if errors.Is(err, store.ErrNoRow) {
		return Tag{}, errNoDeployment()
	}
	if err != nil {
		return Tag{}, fmt.Errorf("tag %s: %w", tag, err)
	}
	return tagOf(t), nil
}

// DeleteTag is DELETE /api/v1/environments/{env}/tags/{tag} (204); 404
// "no such tag".
func (s *Service) DeleteTag(ctx context.Context, tenant, env, tag string) error {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return err
	}
	ok, err := s.st.DeleteTag(ctx, e.ID, tag)
	if err != nil {
		return fmt.Errorf("tag %s: %w", tag, err)
	}
	if !ok {
		return errNoTag()
	}
	return nil
}

// Secrets is GET /api/v1/environments/{env}/secrets: the tenant's
// secrets at their latest versions, never a value; 503 without a sealer.
func (s *Service) Secrets(ctx context.Context, tenant, env string) (SecretList, error) {
	if s.d.Sealer == nil {
		return SecretList{}, errNoSecrets()
	}
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return SecretList{}, err
	}
	rows, err := s.st.ListSecrets(ctx, e.ID, false)
	if err != nil {
		return SecretList{}, fmt.Errorf("secrets: %w", err)
	}
	out := SecretList{Secrets: make([]SecretInfo, 0, len(rows))}
	for _, r := range rows {
		out.Secrets = append(out.Secrets, SecretInfo{Name: r.Name, Version: r.Version, CreatedBy: r.CreatedBy,
			CreatedAt: rfc3339(r.CreatedAt)})
	}
	return out, nil
}

// PutSecret is PUT /api/v1/environments/{env}/secrets/{name}: a new
// version (the highest ever plus one), sealed under its AAD (200); 503
// without a sealer. A deployment pins it at its next apply.
func (s *Service) PutSecret(ctx context.Context, tenant, env, name string, req PutSecretRequest) (SecretVersion, error) {
	if s.d.Sealer == nil {
		return SecretVersion{}, errNoSecrets()
	}
	if !spec.IsSecretName(name) {
		return SecretVersion{}, errBadRequest("secret name %q must be lowercase letters, digits, '.', '_' and '-' "+
			"(at most 63, not beginning with \"addon.\")", name)
	}
	if len(req.Value) > maxSecretBytes {
		return SecretVersion{}, errBadRequest("the value has %d bytes; at most %d are allowed", len(req.Value),
			maxSecretBytes)
	}
	if err := checkDeploymentMeta(req.Author, "", nil); err != nil {
		return SecretVersion{}, err
	}
	by := req.Author
	if by == "" {
		by = tenant
	}
	var version int
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		if err := lockTenant(ctx, tx, tenant); err != nil {
			return err
		}
		e, err := resolveEnv(ctx, tx, tenant, env, store.ForUpdate)
		if err != nil {
			return err
		}
		if e.Deleting {
			return errDeleting(e.Name)
		}
		version = 1
		if last, err := tx.LatestSecret(ctx, e.ID, name); err == nil {
			version = last.Version + 1
		} else if !errors.Is(err, store.ErrNoRow) {
			return err
		}
		sealed, err := s.d.Sealer.Seal([]byte(req.Value), envdeps.SecretAAD(e.ID, name, version))
		if err != nil {
			return fmt.Errorf("seal secret %s: %w", name, err)
		}
		_, err = tx.InsertSecret(ctx, store.EnvSecret{EnvID: e.ID, Name: name, Version: version, Sealed: sealed,
			CreatedBy: by, MadeBy: auth.MadeBy(ctx)})
		return err
	})
	if err != nil {
		return SecretVersion{}, err
	}
	return SecretVersion{Name: name, Version: version}, nil
}

// DeleteSecret is DELETE /api/v1/environments/{env}/secrets/{name}: every
// version soft-deleted (204); 409 while the latest deployment pins it, 404
// "no such secret", 503 without a sealer.
func (s *Service) DeleteSecret(ctx context.Context, tenant, env, name string) error {
	if s.d.Sealer == nil {
		return errNoSecrets()
	}
	if !spec.IsSecretName(name) {
		// A system secret is no tenant's to delete.
		return errNoSecret()
	}
	return s.st.InTx(ctx, func(tx *store.Store) error {
		e, err := resolveEnv(ctx, tx, tenant, env, store.ForUpdate)
		if err != nil {
			return err
		}
		if e.Deployment > 0 {
			r, err := tx.GetDeployment(ctx, e.ID, e.Deployment)
			if err != nil {
				return fmt.Errorf("deployment %d: %w", e.Deployment, err)
			}
			sp, err := parseStored(r.Spec)
			if err != nil {
				return err
			}
			if _, pinned := sp.Secrets[name]; pinned || slices.Contains(spec.Referenced(*sp), name) {
				return apierr.New(http.StatusConflict, "deployment %d pins secret %s: apply a deployment without it first",
					e.Deployment, name)
			}
		}
		n, err := tx.DeleteSecret(ctx, e.ID, name)
		if err != nil {
			return err
		}
		if n == 0 {
			return errNoSecret()
		}
		return nil
	})
}

// Incidents is GET /api/v1/environments/{env}/incidents: newest first,
// only the open ones with openOnly.
func (s *Service) Incidents(ctx context.Context, tenant, env string, openOnly bool) (IncidentList, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return IncidentList{}, err
	}
	rows, err := s.st.ListIncidents(ctx, e.ID, openOnly)
	if err != nil {
		return IncidentList{}, fmt.Errorf("incidents: %w", err)
	}
	out := IncidentList{Incidents: make([]Incident, 0, len(rows))}
	for _, r := range rows {
		out.Incidents = append(out.Incidents, incidentOf(r))
	}
	return out, nil
}

// Incident is GET /api/v1/environments/{env}/incidents/{id}; 404 "no
// such incident".
func (s *Service) Incident(ctx context.Context, tenant, env string, id int64) (Incident, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return Incident{}, err
	}
	r, err := s.st.GetIncident(ctx, e.ID, id)
	if errors.Is(err, store.ErrNoRow) {
		return Incident{}, errNoIncident()
	}
	if err != nil {
		return Incident{}, fmt.Errorf("incident %d: %w", id, err)
	}
	return incidentOf(r), nil
}

// The codes of the controller's refusals of expected states (409).
const (
	// CodeBusy: the object has an operation in flight (a restart while
	// an Ensure, a pause or the probe's own restart runs); Retry-After.
	CodeBusy = "busy"
	// CodeNotRunning: the instance does not run (pending, stopped,
	// failed, its sandbox gone), so it has no log to read. A paused one
	// answers "paused" without a code.
	CodeNotRunning = "not_running"
	// CodeNotReady: a job cannot run yet (its template does not render:
	// an addon without an address).
	CodeNotReady = "not_ready"
)

// ctlError maps the controller's answers: ErrNoObject 404, ErrPaused
// 409 "paused", ErrNoContainerLogs 501, and its refusals of expected
// states 409 with a code (CodeBusy with Retry-After, CodeNotRunning,
// CodeNotReady) or plain (an environment or tenant being deleted), never
// 500. Some of those refusals carry no sentinel of the controller's, and
// are known by their texts.
func ctlError(err error, kind, name string) error {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ctl.ErrNoObject) && msg != ctl.ErrNoObject.Error():
		return apierr.New(http.StatusNotFound, "%s", msg)
	case errors.Is(err, ctl.ErrNoObject):
		return errNoObject(kind, name)
	case errors.Is(err, ctl.ErrPaused):
		return apierr.New(http.StatusConflict, "paused")
	case errors.Is(err, ctl.ErrNoContainerLogs):
		return apierr.New(http.StatusNotImplemented, "%s", ctl.ErrNoContainerLogs.Error())
	case errors.Is(err, ctl.ErrJobRunning):
		// Its own text (the run's deployment and attempt); a run takes
		// minutes, not seconds.
		return apierr.WithCode(http.StatusConflict, CodeBusy, "%s", msg).RetryAfter(apierr.RetryAfterOf(err, 30))
	case errors.Is(err, model.ErrBusy):
		// The generic text: an op's own names instances and sandboxes.
		return apierr.WithCode(http.StatusConflict, CodeBusy, "%s %s is busy with another operation; try again shortly",
			kind, name).RetryAfter(apierr.RetryAfterOf(err, 5))
	case errors.Is(err, model.ErrNotFound):
		return apierr.WithCode(http.StatusConflict, CodeNotRunning, "%s %s is not running: its sandbox is gone", kind, name)
	case errors.Is(err, store.ErrNoRow):
		// An instance or process row the controller read went before it
		// locked it (stopped, replaced).
		return apierr.WithCode(http.StatusConflict, CodeBusy, "%s %s changed meanwhile; try again", kind, name).
			RetryAfter(5)
	case errors.Is(err, ctl.ErrNotRunning):
		return apierr.WithCode(http.StatusConflict, CodeNotRunning, "%s", msg)
	case errors.Is(err, ctl.ErrNotReady):
		return apierr.WithCode(http.StatusConflict, CodeNotReady, "%s", msg)
	case msg == "the environment is being deleted" || msg == "the tenant is being deleted":
		return apierr.New(http.StatusConflict, "%s", msg)
	case strings.HasPrefix(msg, "process logs need the tools' runner"):
		return apierr.New(http.StatusServiceUnavailable, "%s", msg)
	}
	return err
}

// Logs is GET /api/v1/environments/{env}/objects/{kind}/{name}/logs: the
// last lines lines (default 200, at most 10000) of replica replica's log
// (ctl.Controller.LogTail); 501 when the node serves no container logs,
// 409 "paused" for a paused instance (never woken), 409 CodeNotRunning
// for one that does not run or is not there yet, 404 naming the replica
// for one the object does not have.
func (s *Service) Logs(ctx context.Context, tenant, env, kind, name string, lines, replica int) (LogsAnswer, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return LogsAnswer{}, err
	}
	switch kind {
	case spec.KindService, spec.KindAddon, spec.KindJob:
	default:
		return LogsAnswer{}, errNoObject(kind, name)
	}
	if replica < 0 {
		return LogsAnswer{}, errBadRequest("replica %d is negative", replica)
	}
	if lines <= 0 {
		lines = defaultLogLines
	}
	lines = min(lines, maxLogLines)
	out, source, err := s.ctl.LogTail(ctx, e.ID, kind, name, replica, lines)
	if err != nil {
		return LogsAnswer{}, ctlError(err, kind, name)
	}
	if out == nil {
		out = []string{}
	}
	return LogsAnswer{Lines: out, Source: source}, nil
}

// RestartService is POST /api/v1/environments/{env}/services/{name}/restart
// (202): an attached service's process, an image service's replicas one
// at a time; 409 CodeBusy (Retry-After) while the first has an op in
// flight.
func (s *Service) RestartService(ctx context.Context, tenant, env, name string) error {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return err
	}
	if e.Deleting {
		return errDeleting(e.Name)
	}
	return ctlError(s.ctl.RestartService(ctx, e.ID, name), spec.KindService, name)
}

// RunJob is POST /api/v1/environments/{env}/jobs/{name}/run (202): a new
// run, attempt 1; 409 CodeBusy while a run of the job runs, of any deployment
// ("job X runs already (deployment N, attempt M)", Retry-After 30); 409
// CodeNotReady while its template cannot render.
func (s *Service) RunJob(ctx context.Context, tenant, env, name string) error {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return err
	}
	if e.Deleting {
		return errDeleting(e.Name)
	}
	return ctlError(s.ctl.RunJob(ctx, e.ID, name), spec.KindJob, name)
}

// CreateRouteToken is POST /api/v1/environments/{env}/route-tokens: a new
// token of the route door (32 random bytes, base64url), shown once (201);
// only its SHA-256 is kept.
func (s *Service) CreateRouteToken(ctx context.Context, tenant, env string) (RouteToken, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return RouteToken{}, err
	}
	tok, hash, err := mintRouteToken()
	if err != nil {
		return RouteToken{}, fmt.Errorf("mint a route token: %w", err)
	}
	if _, err := s.st.InsertRouteToken(ctx, hash, e.ID, auth.MadeBy(ctx)); errors.Is(err, store.ErrNoRow) {
		return RouteToken{}, errNoEnvironment()
	} else if err != nil {
		return RouteToken{}, err
	}
	return RouteToken{Token: tok}, nil
}

// RevokeRouteTokens is DELETE /api/v1/environments/{env}/route-tokens:
// every route token of the environment revoked (204).
func (s *Service) RevokeRouteTokens(ctx context.Context, tenant, env string) error {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return err
	}
	_, err = s.st.DeleteRouteTokens(ctx, e.ID)
	return err
}
