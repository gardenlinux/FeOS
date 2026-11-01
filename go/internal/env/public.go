// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"errors"
	"net/http"

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
)

// SetPublic is PUT /api/v1/environments/{env}/public: the tenant's
// decision whether the ingress serves the environment at its default host
// without a route token (ADR 0021). The first publish gives the
// environment its host label and its ingress group, so that its image
// services roll once (Rolling); unpublishing keeps both, and publishing
// again rolls nothing. hostLabel, only with public, is a label the tenant
// chose (model.ChosenHostLabel, else 400): the environment gets it in
// place of the label it had or would get, which stays the tenant's; one
// another tenant or environment holds is a 409. Publishing is refused with
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
	if hostLabel != "" && !model.ChosenHostLabel(hostLabel) {
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
	ans := PublicAnswer{Public: after.Public, Hosts: hostsOf(after, s.d.AppsZone), Rolling: public && !before.IngressGroup}
	if before.Public != public || before.HostLabel != after.HostLabel {
		s.log.Info("environment public", "tenant", tenant, "env", after.Name, "id", after.ID, "public", public,
			"hosts", ans.Hosts, "rolling", ans.Rolling, "label_before", before.HostLabel)
	}
	return ans, nil
}
