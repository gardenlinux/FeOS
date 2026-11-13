// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/quota"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A tenant's job (ADR 0018, "A tenant's job") is a job outside its
// environment's spec (ADR 0035), run exactly once: its pod is a row of sandboxes under the
// job's ID (tenant_job), whose template the create renders, its secrets
// and image credentials sealed in the job's row (jobs.sealed) and
// resolved by RowSecret. env-probe ends the jobs that have not ended as
// it ends the environments' job runs (exitOf, outcome, keptTail,
// deleteInstance): an exit (its result file read first), the deadline, a
// lost node, a pod that went or stopped once it had run. A create, a
// cancel and a delete run on any instance; env-probe also deletes the
// pods a job's end left (LeftTenantJobPods), and env-delete sweeps the
// jobs that ended TenantJobRetention ago.

// TenantJobRetention is how long a job's record stays after its end.
const TenantJobRetention = 7 * 24 * time.Hour

// jobTailLines is how many lines of its log a job keeps (within
// storedLog's 64 KiB).
const jobTailLines = 1000

// MaxJobResult is how much of its result file a job keeps.
const MaxJobResult = 4 << 10

// The names a job's image credentials have among its sealed secrets: no
// environment variable's name has a dot.
const (
	jobCredUser     = "imageCredentials.username"
	jobCredPassword = "imageCredentials.password"
)

// ErrJobKeyConflict: the create's Idempotency-Key names a job of the
// tenant made with another body (422).
var ErrJobKeyConflict = errors.New("the Idempotency-Key names a job made with another request")

// ErrJobImage: the job's image does not resolve with its credentials, the
// registry's answer in the text (422).
var ErrJobImage = errors.New("the image does not resolve")

// TenantJobSpec is a job's create, validated by the caller.
type TenantJobSpec struct {
	// EnvID is the environment it runs in (ADR 0035); Reaches what of it
	// its pod reaches, whose entries its deployed deployment must have,
	// and Egress its egress (nil: the internet), ADR 0035 revised.
	EnvID            uuid.UUID
	Reaches          spec.Reaches
	Egress           *spec.Egress
	Image            string
	Command          []string
	Env, Secrets     map[string]string
	ImageCredentials *model.RegistryCredentials
	VCPUs, MemoryMiB int
	Deadline         time.Duration
	ResultPath       string
	Labels           map[string]string
	// LogRetentionDays is how long the sink keeps the job's log (1 to
	// 400); 0: its default.
	LogRetentionDays int
	// Key is the create's Idempotency-Key ("": none), Hash its body's
	// hash, which a repeat with the key must have.
	Key, Hash string
}

// tenantJobAAD is the AAD of a job's sealed secrets.
func tenantJobAAD(id podid.ID) []byte { return []byte("jobs|" + id.String()) }

// CreateTenantJob makes a job of owner: its image pinned with its credentials,
// its pod's row and its record in one transaction; env-probe brings the
// pod up. A create with a key that a job of owner was made with answers
// with that job (false) when its hash is the job's, else
// ErrJobKeyConflict.
func (c *Controller) CreateTenantJob(ctx context.Context, owner string, rs TenantJobSpec) (store.TenantJob, bool, error) {
	if r, ok, err := c.tenantJobOfKey(ctx, owner, rs); ok || err != nil {
		return r, false, err
	}
	secrets := maps.Clone(rs.Secrets)
	if rs.ImageCredentials != nil {
		if secrets == nil {
			secrets = map[string]string{}
		}
		secrets[jobCredUser], secrets[jobCredPassword] = rs.ImageCredentials.Username, rs.ImageCredentials.Password
	}
	if len(secrets) > 0 && c.d.Sealer == nil {
		return store.TenantJob{}, false, errNoSealer
	}
	madeBy := auth.MadeBy(ctx)
	var auth model.RegistryCredentials
	if rs.ImageCredentials != nil {
		auth = *rs.ImageCredentials
	}
	ref, err := c.d.Engine.ResolveImage(ctx, rs.Image, auth)
	if err != nil {
		if ctx.Err() != nil {
			return store.TenantJob{}, false, ctx.Err()
		}
		return store.TenantJob{}, false, &stateError{msg: fmt.Sprintf("image %s: %v", rs.Image, err), kind: ErrJobImage}
	}
	tmpl := model.PodTemplate{Command: slices.Clone(rs.Command)}
	if len(rs.Env)+len(rs.Secrets) > 0 {
		tmpl.Env = map[string]model.TemplateValue{}
		for k, v := range rs.Env {
			tmpl.Env[k] = model.TemplateValue{Value: v}
		}
		for k := range rs.Secrets {
			tmpl.Env[k] = model.TemplateValue{Secret: k}
		}
	}
	if rs.ImageCredentials != nil {
		tmpl.ImageCredentials = &model.TemplateCredentials{Username: model.TemplateValue{Secret: jobCredUser},
			Password: model.TemplateValue{Secret: jobCredPassword}}
	}
	idle := 0
	var job store.TenantJob
	err = c.st.InTx(ctx, func(tx *store.Store) error {
		t, err := quota.LockAndCheck(ctx, tx, owner, quota.Use{RunningJobs: 1, VCPUs: int64(rs.VCPUs),
			MemoryMiB: int64(rs.MemoryMiB)})
		if err != nil {
			return err
		}
		if t.Deleting() {
			return errTenantDeleting
		}
		if err := LockPodEnvironment(ctx, tx, owner, rs.EnvID, rs.Reaches); err != nil {
			return err
		}
		// Its pod is in env-<environment> and its own workload's group,
		// resolves the environment's names, and its egress's names (ADR
		// 0035, revised); fixed for its life.
		tmpl.NameTables = []string{model.EnvTableID(rs.EnvID)}
		tmpl.RestrictEgressNames, tmpl.EgressDomains = rs.Egress.RestrictsNames(), rs.Egress.NameDomains()
		row, err := tx.CreateMintedSandbox(ctx, func(id podid.ID) store.NewSandbox {
			t := tmpl
			t.SecurityGroups = model.PodGroups(rs.EnvID, model.JobWorkloadGroupID(id.String()))
			return store.NewSandbox{ID: id, Owner: owner, Image: rs.Image, VCPUs: rs.VCPUs, MemoryMiB: rs.MemoryMiB,
				ImageRef: ref, EnvironmentID: rs.EnvID, Reaches: rs.Reaches, Egress: rs.Egress, PodTemplate: &t,
				IdleAfterS: &idle, TenantJob: true, LogLabels: tenantJobLogLabels(owner, id, rs.EnvID, rs.LogRetentionDays)}
		})
		if err != nil {
			return err
		}
		var sealed []byte
		if len(secrets) > 0 {
			plain, err := json.Marshal(secrets)
			if err != nil {
				return err
			}
			if sealed, err = c.d.Sealer.Seal(plain, tenantJobAAD(row.ID)); err != nil {
				return fmt.Errorf("seal the job's secrets: %w", err)
			}
		}
		job, err = tx.InsertTenantJob(ctx, store.TenantJob{ID: row.ID, Owner: owner, EnvID: rs.EnvID,
			Reaches: rs.Reaches, Egress: rs.Egress, Key: rs.Key, RequestHash: rs.Hash,
			Image: rs.Image, ImageRef: ref, VCPUs: rs.VCPUs, MemoryMiB: rs.MemoryMiB, ResultPath: rs.ResultPath,
			Labels: rs.Labels, LogRetentionDays: rs.LogRetentionDays, MadeBy: madeBy}, sealed, rs.Deadline)
		return err
	})
	if errors.Is(err, store.ErrExists) && rs.Key != "" {
		// A create with the same key won the race.
		if r, ok, err := c.tenantJobOfKey(ctx, owner, rs); ok || err != nil {
			return r, false, err
		}
	}
	if err != nil {
		return store.TenantJob{}, false, err
	}
	c.log.Info("tenant job created", "tenant", owner, "job", job.ID, "image", ref)
	return job, true, nil
}

// ErrEnvironmentDeleting: the environment a sandbox or a tenant's job is
// made in is being deleted (409).
var ErrEnvironmentDeleting = errors.New("the environment is being deleted")

// ErrNoEnvironment: the environment a sandbox or a tenant's job is made
// in is none of the tenant's (404).
var ErrNoEnvironment = errors.New("no such environment")

// ErrUnknownReaches: a sandbox's or a tenant's job's reaches names an
// object or a port its environment's deployed deployment does not have
// (422).
var ErrUnknownReaches = errors.New("reaches names what the environment lacks")

// LockPodEnvironment locks environment envID of owner FOR SHARE, so that
// its delete waits for the insert of a pod of it, and checks that the
// entries of reaches are objects and ports of its deployed deployment
// (ADR 0035, revised): ErrNoEnvironment for none of owner's,
// ErrEnvironmentDeleting, ErrUnknownReaches.
func LockPodEnvironment(ctx context.Context, tx *store.Store, owner string, envID uuid.UUID, reaches spec.Reaches) error {
	e, err := tx.LockEnvironment(ctx, envID, store.ForShare)
	if errors.Is(err, store.ErrNoRow) || (err == nil && e.Owner != owner) {
		return ErrNoEnvironment
	}
	if err != nil {
		return err
	}
	if e.Deleting {
		return &stateError{msg: fmt.Sprintf("environment %s is being deleted", e.Name), kind: ErrEnvironmentDeleting}
	}
	return CheckReaches(ctx, tx, envID, reaches)
}

// CheckReaches checks the entries of reaches against environment envID's
// deployed deployment: ErrUnknownReaches naming what it has.
func CheckReaches(ctx context.Context, tx *store.Store, envID uuid.UUID, reaches spec.Reaches) error {
	if reaches.All || len(reaches.Entries) == 0 {
		return nil
	}
	raw, err := tx.DeployedSpec(ctx, envID)
	if err != nil {
		return err
	}
	var deployed *spec.Spec
	if raw != nil {
		deployed = &spec.Spec{}
		if err := json.Unmarshal(raw, deployed); err != nil {
			return fmt.Errorf("the deployed deployment's spec: %w", err)
		}
	}
	if msg := spec.UnknownReaches(reaches, deployed); msg != "" {
		return &stateError{msg: msg, kind: ErrUnknownReaches}
	}
	return nil
}

// tenantJobOfKey is the job of owner a create with rs's key made: ok when there
// is one with rs's hash, ErrJobKeyConflict when its hash differs.
func (c *Controller) tenantJobOfKey(ctx context.Context, owner string, rs TenantJobSpec) (store.TenantJob, bool, error) {
	if rs.Key == "" {
		return store.TenantJob{}, false, nil
	}
	r, err := c.st.TenantJobByKey(ctx, owner, rs.Key)
	switch {
	case errors.Is(err, store.ErrNoRow):
		return store.TenantJob{}, false, nil
	case err != nil:
		return store.TenantJob{}, false, err
	case r.RequestHash != rs.Hash:
		return store.TenantJob{}, false, ErrJobKeyConflict
	}
	return r, true, nil
}

// RowSecret is the engine's secret resolver (sandbox.SecretResolver): a
// secret of the row's environment (ResolveSecret), or of its job for a
// tenant's job's pod, whose secrets have no versions.
func (c *Controller) RowSecret(ctx context.Context, row store.Sandbox, name string, version int) (string, error) {
	if !row.TenantJob {
		return c.ResolveSecret(ctx, row.EnvironmentID, name, version)
	}
	secrets, err := c.tenantJobSecrets(ctx, row.ID)
	if err != nil {
		return "", err
	}
	v, ok := secrets[name]
	if !ok {
		return "", fmt.Errorf("job %s has no secret %s", row.ID, name)
	}
	return v, nil
}

// tenantJobSecrets are a job's secrets and image credentials, unsealed; an
// error once it has ended (they are dropped then).
func (c *Controller) tenantJobSecrets(ctx context.Context, id podid.ID) (map[string]string, error) {
	if c.d.Sealer == nil {
		return nil, errNoSealer
	}
	sealed, err := c.st.TenantJobSealed(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("job %s: %w", id, err)
	}
	if sealed == nil {
		return nil, fmt.Errorf("job %s has no secrets (any more)", id)
	}
	plain, err := c.d.Sealer.Open(sealed, tenantJobAAD(id))
	if err != nil {
		return nil, fmt.Errorf("job %s: open its secrets: %w", id, err)
	}
	var out map[string]string
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, fmt.Errorf("job %s: its secrets: %w", id, err)
	}
	return out, nil
}

// tenantJobSecretValues are the values a job's log is redacted of: its secrets
// and image credentials, none when they cannot be read (a job without
// any, or one that has ended).
func (c *Controller) tenantJobSecretValues(ctx context.Context, id podid.ID) []string {
	secrets, err := c.tenantJobSecrets(ctx, id)
	if err != nil {
		return nil
	}
	return slices.Sorted(maps.Values(secrets))
}

// probeTenantJobs is env-probe's pass over the jobs that have not ended, each
// through run, the pass's scheduler (false: the pass ends), then over
// the pods their ends left.
func (c *Controller) probeTenantJobs(ctx context.Context, run func(func()) bool) bool {
	jobs, err := c.st.OpenTenantJobs(ctx)
	if err != nil {
		c.rl.warn("tenant-jobs", "cannot read the jobs", "err", err)
		return true
	}
	now := c.dbNow(ctx)
	for _, r := range jobs {
		if !run(func() { c.stepTenantJob(ctx, r, now) }) {
			return false
		}
	}
	left, err := c.st.LeftTenantJobPods(ctx)
	if err != nil {
		c.rl.warn("tenant-jobs", "cannot read the pods of ended jobs", "err", err)
		return true
	}
	for _, sb := range left {
		if err := c.deleteInstance(ctx, sb); err != nil && ctx.Err() == nil {
			c.rl.warn("tenant-jobs", "cannot delete an ended job's pod", "job", sb.ID, "err", err)
		}
	}
	return true
}

// stepTenantJob is one look at a job that has not ended, at now (the
// database's): its end when its deadline passed, its pod went, was lost,
// or stopped after it ran, or its process exited; else its start once
// its container runs, or its pod brought up (a start that fails ends it,
// as an exit would).
func (c *Controller) stepTenantJob(ctx context.Context, r store.TenantJob, now time.Time) {
	sb, err := c.st.GetSandbox(ctx, r.ID)
	if errors.Is(err, model.ErrNotFound) {
		c.endTenantJob(ctx, r, nil, store.TenantJobEnd{Phase: model.JobFailed, Reason: model.JobReasonNoExitStatus,
			Detail: "its pod is gone"}, true)
		return
	}
	if err != nil {
		c.rl.warn("tenant-jobs", "cannot read a job's pod", "job", r.ID, "err", err)
		return
	}
	switch {
	case now.After(r.DeadlineAt):
		c.endTenantJob(ctx, r, &sb, store.TenantJobEnd{Phase: model.JobDeadlineExceeded,
			Detail: "its deadline of " + spell(r.DeadlineAt.Sub(r.CreatedAt)) + " passed"}, true)
		return
	case sb.Op == "" && sb.ClientState == model.StateLost:
		c.endTenantJob(ctx, r, &sb, store.TenantJobEnd{Phase: model.JobFailed, Reason: model.JobReasonNodeLost,
			Detail: "its pod was lost with node " + lostNode(sb)}, true)
		return
	case steady(sb) && c.connected(sb.Node):
		info, err := c.getPod(ctx, sb.Node, sb.ID)
		if err != nil || info.GetState() != podv1.PodState_POD_STATE_RUNNING {
			return
		}
		mc := mainContainer(info)
		if exited, code := exitOf(mc); exited {
			c.exitedTenantJob(ctx, r, sb, code)
			return
		}
		if r.Phase == model.JobQueued && mc.GetState() == podv1.ContainerState_CONTAINER_STATE_RUNNING {
			if _, err := c.st.StartTenantJob(ctx, r.ID); err != nil && ctx.Err() == nil {
				c.rl.warn("tenant-jobs", "cannot mark a job running", "job", r.ID, "err", err)
			}
		}
		return
	case !r.StartedAt.IsZero() && sb.Op == "" &&
		(sb.ClientState == model.StateStopped || sb.ClientState == model.StateFailed):
		// A job is never run again: a pod that stopped once its process
		// ran is its end.
		c.endTenantJob(ctx, r, &sb, store.TenantJobEnd{Phase: model.JobFailed, Reason: model.JobReasonNoExitStatus,
			Detail: "its pod " + sb.ClientState + " before its process exited"}, false)
		return
	}
	c.healThen(ctx, sb, false, func(ctx context.Context, err error) {
		if errors.Is(err, model.ErrImageRefused) {
			c.endTenantJob(ctx, r, &sb, store.TenantJobEnd{Phase: model.JobFailed, Reason: model.JobReasonImage,
				Detail: err.Error()}, false)
			return
		}
		// A start that failed ends the job as an exit does: the Ensure
		// moved the row and left it failed, its container not started.
		cur, gerr := c.st.GetSandbox(ctx, sb.ID)
		if gerr != nil || cur.Gen == sb.Gen || cur.Op != "" || cur.ClientState != model.StateFailed {
			return
		}
		why := cur.StatusDescription
		if why == "" {
			why = "its pod failed"
		}
		why = storedLog.cut(redact(why, c.tenantJobSecretValues(ctx, r.ID)))
		c.endTenantJob(ctx, r, &cur, store.TenantJobEnd{Phase: model.JobFailed, Reason: model.JobReasonNoExitStatus,
			Detail: "its container did not start: " + why}, false)
	})
}

// exitedTenantJob ends a job whose process exited with code (nil: the node
// reported none): succeeded for 0, failed (exit) for another code, failed
// (no_exit_status) for none; its result file read first.
func (c *Controller) exitedTenantJob(ctx context.Context, r store.TenantJob, sb store.Sandbox, code *int) {
	succeeded, detail := outcome(code, "")
	end := store.TenantJobEnd{Phase: model.JobFailed, Reason: model.JobReasonExit, ExitCode: code, Detail: detail, Exited: true}
	switch {
	case succeeded:
		end.Phase, end.Reason = model.JobSucceeded, ""
	case code == nil:
		end.Reason = model.JobReasonNoExitStatus
	}
	if r.ResultPath != "" {
		result, err := c.readResult(ctx, sb, r.ResultPath)
		if err != nil {
			end.Detail = "its result " + r.ResultPath + " cannot be read: " + err.Error()
		}
		end.Result = redact(result, c.tenantJobSecretValues(ctx, r.ID))
	}
	c.endTenantJob(ctx, r, &sb, end, true)
}

// readResult reads a job's result file, cut to MaxJobResult at a rune's
// start.
func (c *Controller) readResult(ctx context.Context, sb store.Sandbox, path string) (string, error) {
	if c.d.Runner == nil {
		return "", errors.New("this instance has no tools' runner")
	}
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	s, err := c.d.Runner.ReadDetached(ctx, lease(sb), path)
	if err != nil {
		return "", err
	}
	if len(s) > MaxJobResult {
		n := MaxJobResult
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return s, nil
}

// endTenantJob ends a job as end says, with the tail of its pod's log when
// fromPod and the pod runs (read before the end drops its secrets), and
// deletes its pod (sb; nil: it has none). The end is written once: a job
// another call ended keeps that end, and its pod is deleted all the same.
func (c *Controller) endTenantJob(ctx context.Context, r store.TenantJob, sb *store.Sandbox, end store.TenantJobEnd, fromPod bool) {
	if sb != nil && fromPod && steady(*sb) && c.connected(sb.Node) {
		end.LogTail = c.keptTail(ctx, *sb, jobTailLines, func() []string { return c.tenantJobSecretValues(ctx, r.ID) })
	}
	ended, err := c.st.EndTenantJob(ctx, r.ID, end)
	if err != nil {
		if ctx.Err() == nil {
			c.rl.warn("tenant-jobs", "cannot end a job", "job", r.ID, "err", err)
		}
		return
	}
	if ended {
		c.log.Info("tenant job ended", "tenant", r.Owner, "job", r.ID, "phase", end.Phase, "reason", end.Reason,
			"detail", end.Detail)
	}
	if sb == nil {
		return
	}
	// The pod's delete is a mutating call: never on a caller's context.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := c.deleteInstance(dctx, *sb); err != nil {
		c.rl.warn("tenant-jobs", "cannot delete an ended job's pod (env-probe tries again)", "job", r.ID, "err", err)
	}
}

// CancelTenantJob ends a job of owner that has not ended as cancelled, its
// log's tail kept and its pod deleted, and returns it; a job that has
// ended is returned as it is. ErrNoObject: owner has no such job.
func (c *Controller) CancelTenantJob(ctx context.Context, owner string, id podid.ID) (store.TenantJob, error) {
	r, err := c.st.GetTenantJob(ctx, owner, id)
	if errors.Is(err, store.ErrNoRow) {
		return store.TenantJob{}, ErrNoObject
	}
	if err != nil || r.Ended() {
		return r, err
	}
	var sbp *store.Sandbox
	if sb, err := c.st.GetSandbox(ctx, id); err == nil {
		sbp = &sb
	} else if !errors.Is(err, model.ErrNotFound) {
		return store.TenantJob{}, err
	}
	c.endTenantJob(ctx, r, sbp, store.TenantJobEnd{Phase: model.JobCancelled, Detail: "cancelled"}, true)
	return c.st.GetTenantJob(ctx, owner, id)
}

// DeleteTenantJob cancels a job of owner that has not ended and removes its
// record. ErrNoObject: owner has no such job.
func (c *Controller) DeleteTenantJob(ctx context.Context, owner string, id podid.ID) error {
	r, err := c.CancelTenantJob(ctx, owner, id)
	if err != nil {
		return err
	}
	if !r.Ended() {
		return fmt.Errorf("job %s did not end", id)
	}
	if ok, err := c.st.DeleteTenantJob(ctx, owner, id); err != nil {
		return err
	} else if !ok {
		return ErrNoObject
	}
	return nil
}

// TenantJobLog is the last lines lines of a job's log: the sink's
// (sinkLog), whose lines its node redacted of the job's secrets
// (LogRedact), redacted here again while the job runs; else, once it has
// ended, its kept tail; else its pod's, redacted of its secrets; none
// while its pod does not run yet. ErrNoObject: owner has no such job.
func (c *Controller) TenantJobLog(ctx context.Context, owner string, id podid.ID, lines int) ([]string, error) {
	r, err := c.st.GetTenantJob(ctx, owner, id)
	if errors.Is(err, store.ErrNoRow) {
		return nil, ErrNoObject
	}
	if err != nil {
		return nil, err
	}
	if out, ok := c.sinkLog(ctx, "job", id.String(), r.CreatedAt, lines); ok {
		if !r.Ended() {
			secrets := c.tenantJobSecretValues(ctx, id)
			for i := range out {
				out[i] = redact(out[i], secrets)
			}
		}
		return out, nil
	}
	if r.Ended() {
		return r.LogTail[max(len(r.LogTail)-lines, 0):], nil
	}
	sb, err := c.st.GetSandbox(ctx, id)
	if errors.Is(err, model.ErrNotFound) || (err == nil && !(steady(sb) && c.connected(sb.Node))) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out, err := c.containerLog(ctx, lease(sb), lines, answerLog)
	if err != nil {
		return nil, err
	}
	secrets := c.tenantJobSecretValues(ctx, id)
	for i := range out {
		out[i] = redact(out[i], secrets)
	}
	return out, nil
}

// sweepTenantJobs removes the jobs that ended TenantJobRetention ago (env-delete).
func (c *Controller) sweepTenantJobs(ctx context.Context) error {
	n, err := c.st.SweepTenantJobs(ctx, TenantJobRetention)
	if n > 0 {
		c.log.Info("ended tenant jobs swept", "jobs", n)
	}
	return err
}
