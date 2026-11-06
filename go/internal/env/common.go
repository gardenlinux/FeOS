// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/quota"
	"github.com/gardenlinux/feos/go/internal/store"
)

// rfc3339 is a time as the answers write it: RFC 3339 in seconds, UTC;
// "" for none.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseUUID parses a UUID in its hyphenated form, in either case.
func parseUUID(s string) (uuid.UUID, bool) {
	if len(s) != 36 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	return id, err == nil
}

// resolveEnv reads the environment of tenant that ref names (its UUID or
// its name) and locks its row with lock; an environment the tenant does
// not have, foreign or absent, is errNoEnvironment.
func resolveEnv(ctx context.Context, st *store.Store, tenant, ref string, lock store.RowLock) (store.Environment, error) {
	var e store.Environment
	var err error
	if id, ok := parseUUID(ref); ok {
		if e, err = st.GetEnvironment(ctx, id); err == nil && e.Owner == tenant && lock != store.NoLock {
			e, err = st.LockEnvironment(ctx, id, lock)
		}
	} else {
		if !spec.IsName(ref) {
			return store.Environment{}, errNoEnvironment()
		}
		e, err = st.LockEnvironmentByName(ctx, tenant, ref, lock)
	}
	if errors.Is(err, store.ErrNoRow) || (err == nil && e.Owner != tenant) {
		return store.Environment{}, errNoEnvironment()
	}
	if err != nil {
		return store.Environment{}, fmt.Errorf("environment %s: %w", ref, err)
	}
	return e, nil
}

// lockTenant takes the tenant's row FOR SHARE and refuses a deleting
// one: what an environment's create, deployment and secret need.
func lockTenant(ctx context.Context, tx *store.Store, tenant string) error {
	t, err := tx.LockTenant(ctx, tenant, store.ForShare)
	if errors.Is(err, store.ErrNoRow) {
		return apierr.New(http.StatusUnauthorized, "unknown tenant token")
	}
	if err != nil {
		return fmt.Errorf("tenant %s: %w", tenant, err)
	}
	if t.Deleting() {
		return apierr.New(http.StatusConflict, "tenant %s is being deleted", tenant)
	}
	return nil
}

// lockQuota is lockTenant for what counts against the tenant's limits
// (ADR 0031): the row FOR NO KEY UPDATE when it has any (quota.Lock), so
// that two creates do not pass a limit together. It returns the tenant.
func lockQuota(ctx context.Context, tx *store.Store, tenant string) (store.Tenant, error) {
	t, err := quota.Lock(ctx, tx, tenant)
	if errors.Is(err, store.ErrNoRow) {
		return store.Tenant{}, apierr.New(http.StatusUnauthorized, "unknown tenant token")
	}
	if err != nil {
		return store.Tenant{}, fmt.Errorf("tenant %s: %w", tenant, err)
	}
	if t.Deleting() {
		return store.Tenant{}, apierr.New(http.StatusConflict, "tenant %s is being deleted", tenant)
	}
	return t, nil
}

// checkQuota refuses in tx what ask asks of a tenant with limits beyond
// them (422 quota_exceeded), and a deployment of next in place of the
// environment exclude's current (nil: none, a new environment) whose sums
// would pass them (an InvalidSpecError). The caller holds lockQuota.
func checkQuota(ctx context.Context, tx *store.Store, t store.Tenant, ask quota.Use, exclude uuid.UUID,
	current, next *spec.Spec) error {
	if !t.Limits.Any() {
		return nil
	}
	use, err := quota.Of(ctx, tx, t.ID, uuid.Nil)
	if err != nil {
		return err
	}
	if err := quota.Check(t.Limits, use, ask); err != nil {
		return err
	}
	if next == nil {
		return nil
	}
	others, err := quota.Of(ctx, tx, t.ID, exclude)
	if err != nil {
		return err
	}
	if fs := quota.SpecFindings(t.Limits, others, current, next); len(fs) > 0 {
		return &InvalidSpecError{Findings: fs}
	}
	return nil
}

// wake asks the controller to act on the environments: inside the
// writing transaction.
func wake(ctx context.Context, tx *store.Store, hints ...string) error {
	if len(hints) == 0 {
		hints = []string{ctl.HintReconcile}
	}
	for _, h := range hints {
		if err := db.Notify(ctx, tx.Q(), db.ChanLeader, h); err != nil {
			return fmt.Errorf("notify %s: %w", h, err)
		}
	}
	return nil
}

// envChanged runs the EnvChanged hook in tx, when there is one.
func (s *Service) envChanged(ctx context.Context, tx *store.Store, id uuid.UUID) error {
	if s.d.EnvChanged == nil {
		return nil
	}
	if err := s.d.EnvChanged(ctx, tx, id); err != nil {
		return fmt.Errorf("the domains of environment %s: %w", id, err)
	}
	return nil
}

// parseStored reads a stored (normalised) spec.
func parseStored(raw json.RawMessage) (*spec.Spec, error) {
	var sp spec.Spec
	if err := json.Unmarshal(raw, &sp); err != nil {
		return nil, fmt.Errorf("a stored spec: %w", err)
	}
	return &sp, nil
}

// deploymentSpec returns deployment number of environment e's parsed spec, for
// reading only (cached: a deployment never changes); errNoDeployment when there
// is none. The cache knows the environment by its ID and its creation:
// an ID is free again once its environment is deleted, and the next
// environment of that ID has deployments of its own.
func (s *Service) deploymentSpec(ctx context.Context, e store.Environment, number int) (*spec.Spec, error) {
	k := deploymentKey{env: e.ID, created: e.CreatedAt.UnixMicro(), number: number}
	if sp, ok := s.specs.get(k); ok {
		return sp, nil
	}
	r, err := s.st.GetDeployment(ctx, e.ID, number)
	if errors.Is(err, store.ErrNoRow) {
		return nil, errNoDeployment()
	}
	if err != nil {
		return nil, fmt.Errorf("deployment %d: %w", number, err)
	}
	sp, err := parseStored(r.Spec)
	if err != nil {
		return nil, fmt.Errorf("deployment %d: %w", number, err)
	}
	s.specs.put(k, sp)
	return sp, nil
}

// deploymentInfo is a deployment as the answers show it.
func deploymentInfo(r store.EnvDeployment, tags []string) DeploymentInfo {
	if tags == nil {
		tags = []string{}
	}
	ann := r.Annotations
	if ann == nil {
		ann = map[string]string{}
	}
	return DeploymentInfo{Number: r.Number, State: r.State, Origin: r.Origin, OriginRef: r.OriginRef, Author: r.Author,
		MadeBy: r.MadeBy, Message: r.Message, Annotations: ann, Base: r.Base, Reason: r.Reason, CreatedAt: rfc3339(r.CreatedAt),
		StartedAt: rfc3339(r.StartedAt), FinishedAt: rfc3339(r.FinishedAt), Tags: tags}
}

// tagsByDeployment are the environment's tags by the deployment they name.
func (s *Service) tagsByDeployment(ctx context.Context, envID uuid.UUID) (map[int][]string, error) {
	tags, err := s.st.ListTags(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	out := map[int][]string{}
	for _, t := range tags {
		out[t.Number] = append(out[t.Number], t.Tag)
	}
	return out, nil
}

// nonNilDiff is d, or an empty diff.
func nonNilDiff(d []spec.DiffEntry) []spec.DiffEntry {
	if d == nil {
		return []spec.DiffEntry{}
	}
	return d
}
