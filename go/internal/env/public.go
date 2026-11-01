// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env/ctl"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// textNoAppsZone refuses a publish on a platform without an apps zone.
const textNoAppsZone = "the platform serves no apps zone (--apps-zone); an environment cannot be public"

// The refusals of a chosen host label.
const (
	textHostLabelNotPublic = "hostLabel is given only with public: true"
	textBadHostLabel       = "hostLabel %q: a chosen host label is a DNS label of at most 63 lowercase letters, digits and hyphens with \"--\" in it, not at its third and fourth characters, and not ending in a hyphen and 16 hex characters"
	textHostLabelTaken     = "host label %s is another tenant's or another environment's"
	// textBadOwnedHostLabel refuses a label of a tenant that owns the apps
	// zone, which may leave out the "--".
	textBadOwnedHostLabel = "hostLabel %q: a chosen host label is a DNS label of at most 63 lowercase letters, digits and hyphens, not with \"--\" at its third and fourth characters, and not ending in a hyphen and 16 hex characters"
	// textHostLabelNatural refuses a plain label that is the natural label
	// of an existing tenant's environment.
	textHostLabelNatural = "host label %s is the natural label of environment %s of tenant %s"
)

// SetPublic is PUT /api/v1/environments/{env}/public: the tenant's
// decision whether the ingress serves the environment at its default host
// without a route token (ADR 0021). The first publish gives the
// environment its host label and its ingress group, so that its image
// services roll once (Rolling); unpublishing keeps both, and publishing
// again rolls nothing. hostLabel, only with public, is a label the tenant
// chose (model.ChosenHostLabel, or, for a tenant of Deps.AppsZoneOwners,
// also model.PlainHostLabel; else 400): the environment gets it in place
// of the label it had or would get, which stays the tenant's; one another
// tenant or environment holds is a 409, and so is a plain label that is
// the natural label of an existing tenant's environment, unless the
// tenant holds it already (checkPlainLabel). Publishing is refused with
// 409 while the platform has no apps zone, and either is while the
// environment or its tenant is being deleted. A call that changes nothing
// writes nothing and answers the same.
func (s *Service) SetPublic(ctx context.Context, tenant, env string, public bool, hostLabel string) (PublicAnswer, error) {
	if public && s.d.AppsZone == "" {
		return PublicAnswer{}, apierr.New(http.StatusConflict, textNoAppsZone)
	}
	if hostLabel != "" && !public {
		return PublicAnswer{}, apierr.New(http.StatusBadRequest, textHostLabelNotPublic)
	}
	zoneOwner := slices.Contains(s.d.AppsZoneOwners, tenant)
	plain := hostLabel != "" && zoneOwner && model.PlainHostLabel(hostLabel)
	if hostLabel != "" && !plain && !model.ChosenHostLabel(hostLabel) {
		if zoneOwner {
			return PublicAnswer{}, apierr.New(http.StatusBadRequest, textBadOwnedHostLabel, hostLabel)
		}
		return PublicAnswer{}, apierr.New(http.StatusBadRequest, textBadHostLabel, hostLabel)
	}
	var before, after store.Environment
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		if err := lockTenant(ctx, tx, tenant); err != nil {
			return err
		}
		var err error
		if before, err = resolveEnv(ctx, tx, tenant, env, store.ForUpdate); err != nil {
			return err
		}
		if before.Deleting {
			return errDeleting(before.Name)
		}
		cur := before
		if plain {
			if err := checkPlainLabel(ctx, tx, tenant, hostLabel); err != nil {
				return err
			}
		}
		if hostLabel != "" {
			cur, err = tx.SetEnvironmentHostLabel(ctx, before, hostLabel)
			if errors.Is(err, store.ErrHostLabelTaken) {
				return apierr.WithCode(http.StatusConflict, CodeHostLabelTaken, textHostLabelTaken, hostLabel)
			}
			if err != nil {
				return err
			}
		}
		after, err = tx.SetEnvironmentPublic(ctx, cur.ID, public)
		if errors.Is(err, store.ErrNoRow) {
			return errNoEnvironment()
		}
		if err != nil || (before.Public == public && before.HostLabel == after.HostLabel) {
			return err
		}
		if err := s.envChanged(ctx, tx, before.ID); err != nil {
			return err
		}
		return wake(ctx, tx, ctl.HintReconcile, ctl.HintGroups)
	})
	if err != nil {
		return PublicAnswer{}, err
	}
	hosts, err := s.hosts(ctx, after)
	if err != nil {
		return PublicAnswer{}, fmt.Errorf("hosts: %w", err)
	}
	ans := PublicAnswer{Public: after.Public, Hosts: hosts, Rolling: public && !before.IngressGroup}
	if before.Public != public || before.HostLabel != after.HostLabel {
		s.log.Info("environment public", "tenant", tenant, "env", after.Name, "id", after.ID, "public", public,
			"hosts", ans.Hosts, "rolling", ans.Rolling, "label_before", before.HostLabel)
	}
	return ans, nil
}

// checkPlainLabel refuses with 409 host_label_taken a plain label that is
// the natural label of an existing tenant's environment: split at its
// first hyphen (model.NaturalPair), the rest names a tenant, the zone's
// owner itself included. So a zone owner never takes the host that
// reads as an existing tenant's; a tenant made later, whose natural label
// the owner holds, gets its fallback (store.ClaimHostLabel). A label the
// tenant holds already (host_labels) stays its own and is not refused, so
// that choosing it again, after that tenant came, changes nothing.
func checkPlainLabel(ctx context.Context, tx *store.Store, tenant, label string) error {
	name, other, ok := model.NaturalPair(label)
	if !ok {
		return nil
	}
	owner, err := tx.HostLabelOwner(ctx, label)
	if err == nil && owner == tenant {
		return nil
	}
	if err != nil && !errors.Is(err, store.ErrNoRow) {
		return err
	}
	_, err = tx.GetTenant(ctx, other)
	if errors.Is(err, store.ErrNoRow) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tenant %s: %w", other, err)
	}
	return apierr.WithCode(http.StatusConflict, CodeHostLabelTaken, textHostLabelNatural, label, name, other)
}
