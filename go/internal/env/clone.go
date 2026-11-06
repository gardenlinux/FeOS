// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/env/envdeps"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/quota"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The bounds of an environment's labels and a secret's value.
const (
	maxLabels      = 64
	maxLabelBytes  = 253
	maxSecretBytes = 64 << 10
)

// checkNewEnv validates a new environment's name, ID ("" mints one) and
// labels and returns its ID.
func checkNewEnv(name, id string, labels map[string]string) (uuid.UUID, error) {
	if _, isUUID := parseUUID(name); !spec.IsName(name) || isUUID {
		return uuid.Nil, errBadRequest("name %q must be a DNS label (lowercase letters, digits and '-', at most 63) "+
			"and not a UUID", name)
	}
	if len(labels) > maxLabels {
		return uuid.Nil, errBadRequest("%d labels; at most %d are allowed", len(labels), maxLabels)
	}
	for k, v := range labels {
		if k == "" || len(k) > maxLabelBytes || len(v) > maxLabelBytes {
			return uuid.Nil, errBadRequest("label %q: keys are 1..%d bytes, values at most %d", k, maxLabelBytes,
				maxLabelBytes)
		}
	}
	if id == "" {
		return uuid.NewV7()
	}
	u, ok := parseUUID(id)
	if !ok {
		return uuid.Nil, errBadRequest("id %q must be a UUID", id)
	}
	return u, nil
}

// errRace is an insert that lost to another one between its checks and
// its write: the transaction runs again.
var errRace = errors.New("another request created the environment first")

// insertEnv inserts an environment in tx (whose tenant it locked): the
// existing one and false when idempotent and the same ID and name are
// the tenant's already; 409 id_taken or name_taken otherwise.
func insertEnv(ctx context.Context, tx *store.Store, tenant string, id uuid.UUID, name string, labels map[string]string,
	idempotent bool) (store.Environment, bool, error) {
	byID, err := tx.GetEnvironment(ctx, id)
	switch {
	case err == nil && idempotent && byID.Owner == tenant && byID.Name == name:
		return byID, false, nil
	case err == nil && byID.Owner == tenant && byID.Name == name:
		return store.Environment{}, false, apierr.WithCode(http.StatusConflict, CodeIDTaken,
			"an environment of id %s exists", id)
	case err == nil:
		return store.Environment{}, false, apierr.WithCode(http.StatusConflict, CodeIDTaken,
			"an environment of id %s exists with another name", id)
	case !errors.Is(err, store.ErrNoRow):
		return store.Environment{}, false, err
	}
	if _, err := tx.LockEnvironmentByName(ctx, tenant, name, store.NoLock); err == nil {
		return store.Environment{}, false, apierr.WithCode(http.StatusConflict, CodeNameTaken,
			"an environment named %s exists with another id", name)
	} else if !errors.Is(err, store.ErrNoRow) {
		return store.Environment{}, false, err
	}
	e, err := tx.InsertEnvironment(ctx, store.NewEnvironment{ID: id, Owner: tenant, Name: name, Labels: labels})
	if errors.Is(err, store.ErrExists) {
		return store.Environment{}, false, errRace
	}
	if err != nil {
		return store.Environment{}, false, err
	}
	return e, true, nil
}

// inTxRetry runs fn in a transaction, again while it loses a race.
func (s *Service) inTxRetry(ctx context.Context, fn func(tx *store.Store) error) error {
	var err error
	for range 3 {
		if err = s.st.InTx(ctx, fn); !errors.Is(err, errRace) {
			return err
		}
	}
	return err
}

// cloneSecret is a secret a clone sets: its value, sealed later for the
// new environment.
type cloneSecret struct {
	name  string
	value []byte
}

// secretRefPaths are the paths of a spec's secret references, by secret,
// each list sorted.
func secretRefPaths(sp spec.Spec) map[string][]string {
	out := map[string][]string{}
	add := func(prefix string, env map[string]spec.EnvValue) {
		for k, v := range env {
			if v.Kind == spec.EnvSecret {
				out[v.Secret] = append(out[v.Secret], prefix+".env."+k)
			}
		}
	}
	for name, svc := range sp.Services {
		add("services."+name, svc.Env)
	}
	for name, j := range sp.Jobs {
		add("jobs."+name, j.Env)
	}
	for _, paths := range out {
		slices.Sort(paths)
	}
	return out
}

// cloneSpec is the source deployment's spec as a clone applies it: without
// its attached services (and the routes to them), which run in a
// sandbox of the source, and with its secrets pinned to nothing (the
// clone's own versions are pinned).
func cloneSpec(stored []byte) ([]byte, []spec.Finding, error) {
	sp, err := parseStored(stored)
	if err != nil {
		return nil, nil, err
	}
	var fs []spec.Finding
	removed := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(sp.Services)) {
		if sp.Services[name].Runtime == spec.RuntimeAttached {
			removed[name] = true
			delete(sp.Services, name)
			fs = append(fs, spec.Finding{Severity: spec.SeverityInfo, Path: "services." + name,
				Problem: "attached service " + name + " is not cloned: it runs in a sandbox of the source",
				Remedy:  "pass a spec that runs it as an image"})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(sp.Routes)) {
		if svc := sp.Routes[name].Service; removed[svc] {
			delete(sp.Routes, name)
			fs = append(fs, spec.Finding{Severity: spec.SeverityInfo, Path: "routes." + name,
				Problem: "route " + name + " is not cloned: its service " + svc + " is not", Remedy: ""})
		}
	}
	for name := range sp.Secrets {
		sp.Secrets[name] = spec.SecretRef{}
	}
	b, err := json.Marshal(sp)
	return b, fs, err
}

// clone is Clone.
func (s *Service) clone(ctx context.Context, tenant, ref string, req CloneRequest) (EnvStatus, error) {
	src, err := resolveEnv(ctx, s.st, tenant, ref, store.NoLock)
	if err != nil {
		return EnvStatus{}, err
	}
	id, err := checkNewEnv(req.Name, req.ID, req.Labels)
	if err != nil {
		return EnvStatus{}, err
	}
	if err := checkDeploymentMeta(req.Author, "", nil); err != nil {
		return EnvStatus{}, err
	}
	// A taken ID is refused before anything is planned under it: it may
	// be another tenant's environment. insertEnv checks again.
	if req.ID != "" {
		byID, err := s.st.GetEnvironment(ctx, id)
		switch {
		case err == nil && byID.Owner == tenant && byID.Name == req.Name:
			return EnvStatus{}, apierr.WithCode(http.StatusConflict, CodeIDTaken, "an environment of id %s exists", id)
		case err == nil:
			return EnvStatus{}, apierr.WithCode(http.StatusConflict, CodeIDTaken,
				"an environment of id %s exists with another name", id)
		case !errors.Is(err, store.ErrNoRow):
			return EnvStatus{}, err
		}
	}
	var fs []spec.Finding
	if req.Data {
		fs = append(fs, spec.Finding{Severity: spec.SeverityError, Path: "data", Problem: "copying data is not available",
			Remedy: "clone without data; the addons start empty"})
	}
	number := src.Deployment
	if req.Deployment != nil {
		number = *req.Deployment
	}
	var srcRel *store.EnvDeployment
	if number < 0 || (number == 0 && req.Deployment != nil && *req.Deployment != 0) {
		return EnvStatus{}, errNoDeployment()
	}
	if number > 0 {
		r, err := s.st.GetDeployment(ctx, src.ID, number)
		if errors.Is(err, store.ErrNoRow) {
			return EnvStatus{}, errNoDeployment()
		}
		if err != nil {
			return EnvStatus{}, fmt.Errorf("deployment %d: %w", number, err)
		}
		srcRel = &r
	}
	raw := []byte(req.Spec)
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = nil
		if srcRel == nil {
			fs = append(fs, spec.Finding{Severity: spec.SeverityError, Path: "deployment",
				Problem: "environment " + src.Name + " has no deployment to clone", Remedy: "pass a spec, or apply a deployment first"})
		} else {
			var cfs []spec.Finding
			if raw, cfs, err = cloneSpec(srcRel.Spec); err != nil {
				return EnvStatus{}, err
			}
			fs = append(fs, cfs...)
		}
	}

	// The secrets: copied (the source's latest version) or set.
	if len(req.Secrets) > 0 && s.d.Sealer == nil {
		return EnvStatus{}, errNoSecrets()
	}
	var latest map[string]store.EnvSecret
	var secrets []cloneSecret
	vcSecrets := map[string]spec.SecretInfo{}
	for _, name := range slices.Sorted(maps.Keys(req.Secrets)) {
		cs := req.Secrets[name]
		p := "secrets." + name
		switch {
		case !spec.IsSecretName(name):
			fs = append(fs, spec.Finding{Severity: spec.SeverityError, Path: p,
				Problem: fmt.Sprintf("%q is not a secret name you can use", name),
				Remedy:  "secret names are lowercase letters, digits, '.', '_' and '-'; \"addon.\" is reserved"})
			continue
		case cs.Copy == (cs.Value != nil):
			fs = append(fs, spec.Finding{Severity: spec.SeverityError, Path: p,
				Problem: "a secret of a clone is {\"copy\": true} or {\"value\": …}", Remedy: "give exactly one of them"})
			continue
		case cs.Value != nil:
			if len(*cs.Value) > maxSecretBytes {
				return EnvStatus{}, errBadRequest("secret %s has %d bytes; at most %d are allowed", name, len(*cs.Value),
					maxSecretBytes)
			}
			secrets = append(secrets, cloneSecret{name: name, value: []byte(*cs.Value)})
		default:
			if latest == nil {
				rows, err := s.st.ListSecrets(ctx, src.ID, false)
				if err != nil {
					return EnvStatus{}, fmt.Errorf("secrets of %s: %w", src.Name, err)
				}
				latest = map[string]store.EnvSecret{}
				for _, r := range rows {
					latest[r.Name] = r
				}
			}
			row, ok := latest[name]
			if !ok {
				fs = append(fs, spec.Finding{Severity: spec.SeverityError, Path: p,
					Problem: "environment " + src.Name + " has no secret " + name + " to copy",
					Remedy:  "set it with a value instead"})
				continue
			}
			value, err := s.d.Sealer.Open(row.Sealed, envdeps.SecretAAD(src.ID, name, row.Version))
			if err != nil {
				return EnvStatus{}, fmt.Errorf("secret %s version %d of %s does not open: %w", name, row.Version, src.Name, err)
			}
			secrets = append(secrets, cloneSecret{name: name, value: value})
		}
		vcSecrets[name] = spec.SecretInfo{Latest: 1, Versions: []int{1}}
	}
	if raw != nil {
		if sp, pfs := spec.Parse(raw); !spec.HasErrors(pfs) {
			refs := secretRefPaths(sp)
			for _, name := range slices.Sorted(maps.Keys(refs)) {
				if _, ok := vcSecrets[name]; ok || !spec.IsSecretName(name) {
					continue
				}
				fs = append(fs, spec.Finding{Severity: spec.SeverityError, Path: refs[name][0],
					Problem: "the new environment has no secret " + name,
					Remedy:  fmt.Sprintf(`pass secrets: {"%s": {"copy": true}} or a value`, name)})
				// Validate says no more of it.
				vcSecrets[name] = spec.SecretInfo{Latest: 1, Versions: []int{1}}
			}
		}
	}

	// The plan, as deployment 1 of an environment that is not there yet.
	var norm spec.Spec
	if raw != nil {
		p, err := s.plan(ctx, tenant, store.Environment{ID: id, Owner: tenant, Name: req.Name}, raw, nil, spec.ModeApply,
			vcSecrets)
		if err != nil {
			return EnvStatus{}, err
		}
		fs = append(fs, p.findings...)
		if !p.parsed {
			fs = append(fs, spec.Finding{Severity: spec.SeverityError, Path: "spec", Problem: "the spec cannot be read",
				Remedy: "fix the findings above"})
		}
		norm = p.norm
	}
	fs = spec.SortFindings(fs)
	if spec.HasErrors(fs) {
		return EnvStatus{}, &InvalidSpecError{Findings: fs}
	}
	specJSON, err := json.Marshal(norm)
	if err != nil {
		return EnvStatus{}, err
	}
	sealed := make([][]byte, len(secrets))
	for i, sec := range secrets {
		if sealed[i], err = s.d.Sealer.Seal(sec.value, envdeps.SecretAAD(id, sec.name, 1)); err != nil {
			return EnvStatus{}, fmt.Errorf("seal secret %s: %w", sec.name, err)
		}
	}
	author := req.Author
	if author == "" {
		author = tenant
	}
	var annotations map[string]string
	if srcRel != nil {
		annotations = cloneAnnotations(srcRel.Annotations)
	}
	var row store.Environment
	err = s.inTxRetry(ctx, func(tx *store.Store) error {
		t, err := lockQuota(ctx, tx, tenant)
		if err != nil {
			return err
		}
		var next *spec.Spec
		if raw != nil {
			next = &norm
		}
		if err := checkQuota(ctx, tx, t, quota.Use{Environments: 1}, id, nil, next); err != nil {
			return err
		}
		e, _, err := insertEnv(ctx, tx, tenant, id, req.Name, req.Labels, false)
		if err != nil {
			return err
		}
		for i, sec := range secrets {
			if _, err := tx.InsertSecret(ctx, store.EnvSecret{EnvID: e.ID, Name: sec.name, Version: 1, Sealed: sealed[i],
				CreatedBy: author, MadeBy: auth.MadeBy(ctx)}); err != nil {
				return err
			}
		}
		if _, err := tx.InsertDeployment(ctx, store.EnvDeployment{EnvID: e.ID, Number: 1, Spec: specJSON, Author: req.Author,
			Annotations: annotations, Origin: model.OriginClone, OriginRef: fmt.Sprintf("%s/%d", src.ID, number),
			State: model.DeploymentWaiting, MadeBy: auth.MadeBy(ctx)}); err != nil {
			return err
		}
		after := e
		after.Deployment = 1
		if row, err = tx.UpdateEnvironment(ctx, e, after); err != nil {
			return err
		}
		return wake(ctx, tx)
	})
	if err != nil {
		return EnvStatus{}, err
	}
	return s.status(ctx, row)
}
