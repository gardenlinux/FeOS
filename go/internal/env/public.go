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

// textNoAppsZone refuses a publish on a platform without an apps zone;
// textZoneOff one of a tenant whose zone the platform does not serve.
const (
	textNoAppsZone = "the platform serves no apps zone (--apps-zone); an environment cannot be public"
	textZoneOff    = "the platform serves no zone %s, tenant %s's; an environment cannot be public until an operator moves the tenant"
)

// The refusals of a chosen host label.
const (
	textHostLabelNotPublic = "hostLabel is given only with public: true"
	textBadHostLabel       = "hostLabel %q: a chosen host label is a DNS label of at most 63 lowercase letters, digits and hyphens with \"--\" in it, not at its third and fourth characters, and not ending in a hyphen and 16 hex characters"
	textHostLabelTaken     = "host label %s is another tenant's or another environment's"
	// textBadOwnedHostLabel refuses a label of a tenant that owns the apps
	// zone, which may leave out the "--".
	textBadOwnedHostLabel = "hostLabel %q: a chosen host label is a DNS label of at most 63 lowercase letters, digits and hyphens, not with \"--\" at its third and fourth characters, and not ending in a hyphen and 16 hex characters"
	// textPlainLabelZone refuses a plain label of a zone owner whose
	// hosts are in the tenant zone, which has no owners (ADR 0031).
	textPlainLabelZone = "hostLabel %q: a plain host label is only for the apps zone; tenant %s has its hosts in %s, where a chosen host label has \"--\" in it"
	// textHostLabelNatural refuses a plain label that is the natural label
	// of an existing tenant's environment.
	textHostLabelNatural = "host label %s is the natural label of environment %s of tenant %s"
)

// The refusals of an access (ADR 0054).
const (
	textBadAccess          = "access is anyone or visitors"
	textProviderNoVisitors = "a provider goes with access visitors"
	textAccessNotPublic    = "access goes with public true"
	textVisitsNeedKey      = "this installation guards no environments: ferund runs without --secret-key-file"
	textUnknownProvider    = "no provider %s in this tenant"
	textNeedsFerunAuth     = "visitors of %s need ferun-auth, which this installation does not run"
)

// The codes of the refusals of an access (ADR 0054).
const (
	CodeInvalidAccess       = "invalid_access"
	CodeVisitsNeedSecretKey = "visits_need_secret_key"
	CodeUnknownProvider     = "unknown_provider"
	CodeFerunAuthRequired   = "ferun_auth_required"
)

// SetPublic is PUT /api/v1/environments/{env}/public: the tenant's
// decision whether the ingress serves the environment at its default host
// without a route token (ADR 0021), and whom it admits there (ADR 0054).
// The first publish gives the
// environment its host label and its ingress group, so that its image
// services roll once (Rolling); unpublishing keeps both, and publishing
// again rolls nothing. hostLabel, only with public, is a label the tenant
// chose (model.ChosenHostLabel, or, for a tenant of Deps.AppsZoneOwners
// whose hosts are in the apps zone, also model.PlainHostLabel; else 400)
// in its tenant's zone (ADR 0031): the environment gets it in place
// of the label it had or would get, which stays the tenant's; one another
// tenant or environment holds is a 409, and so is a plain label that is
// the natural label of an existing tenant's environment, unless the
// tenant holds it already (checkPlainLabel). Publishing is refused with
// 409 while the platform has no apps zone or does not serve the tenant's
// zone, and either is while the environment or its tenant is being
// deleted.
//
// The access, only with public true (400 invalid_access otherwise, and for
// an access other than anyone or visitors, or a provider without
// visitors), is anyone, or visitors of one of the tenant's providers or of
// the builtin ferun. Without access a publish of an environment that is
// not public admits visitors of ferun, and one of a public environment
// keeps its access; visitors without a provider are of ferun. Visitors are
// refused while no visits service runs (409 visits_need_secret_key,
// Deps.VisitKinds nil), of a provider the tenant does not have (422
// unknown_provider), and of a provider whose kind the installation does
// not serve (409 ferun_auth_required). Unpublishing ends the
// environment's visits, and so does a change of its access or provider.
// A call that changes nothing writes nothing and answers the same.
func (s *Service) SetPublic(ctx context.Context, tenant, env string, req PublicRequest) (PublicAnswer, error) {
	public, hostLabel := req.Public, req.HostLabel
	switch {
	case req.Access != "" && req.Access != model.AccessAnyone && req.Access != model.AccessVisitors:
		return PublicAnswer{}, apierr.WithCode(http.StatusBadRequest, CodeInvalidAccess, textBadAccess)
	case req.Provider != "" && req.Access != model.AccessVisitors:
		return PublicAnswer{}, apierr.WithCode(http.StatusBadRequest, CodeInvalidAccess, textProviderNoVisitors)
	case !public && (req.Access != "" || req.Provider != ""):
		return PublicAnswer{}, apierr.WithCode(http.StatusBadRequest, CodeInvalidAccess, textAccessNotPublic)
	}
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
		if err := s.checkZone(ctx, tx, tenant, public, plain, hostLabel); err != nil {
			return err
		}
		var err error
		if before, err = resolveEnv(ctx, tx, tenant, env, store.ForUpdate); err != nil {
			return err
		}
		if before.Deleting {
			return errDeleting(before.Name)
		}
		access, provider := before.Access, before.AccessProvider
		if public {
			access, provider = accessAfter(before, req)
			if err := s.checkAccess(ctx, tx, tenant, access, provider); err != nil {
				return err
			}
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
		if err != nil {
			return err
		}
		if public {
			if after, _, err = tx.SetEnvironmentAccess(ctx, cur.ID, access, provider); err != nil {
				return err
			}
		}
		if before.Public == public && before.HostLabel == after.HostLabel {
			return nil
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
	ans.Access, ans.Provider = accessOf(after)
	if before.Public != public || before.HostLabel != after.HostLabel || before.Access != after.Access ||
		before.AccessProvider != after.AccessProvider {
		s.log.Info("environment public", "tenant", tenant, "env", after.Name, "id", after.ID, "public", public,
			"hosts", ans.Hosts, "rolling", ans.Rolling, "label_before", before.HostLabel, "access", ans.Access,
			"provider", ans.Provider)
	}
	return ans, nil
}

// accessAfter is the access a publish (public true) gives environment e
// (stored, as e has it: provider "" for ferun): what req says; without
// access, visitors of ferun for an environment that is not public, and
// its own access for one that is; visitors without a provider (or of
// "ferun") are of ferun.
func accessAfter(e store.Environment, req PublicRequest) (access, provider string) {
	switch req.Access {
	case model.AccessAnyone:
		return model.AccessAnyone, ""
	case model.AccessVisitors:
		if req.Provider == model.ProviderFerun {
			return model.AccessVisitors, ""
		}
		return model.AccessVisitors, req.Provider
	}
	if e.Public {
		return e.Access, e.AccessProvider
	}
	return model.AccessVisitors, ""
}

// checkAccess refuses an access of visitors this installation cannot
// guard: without a visits service (409 visits_need_secret_key), of a
// provider tenant does not have (422 unknown_provider), and of one whose
// kind it does not serve (409 ferun_auth_required). provider is "" for
// ferun. tx holds the tenant's row.
func (s *Service) checkAccess(ctx context.Context, tx *store.Store, tenant, access, provider string) error {
	if access != model.AccessVisitors {
		return nil
	}
	if s.d.VisitKinds == nil {
		return apierr.WithCode(http.StatusConflict, CodeVisitsNeedSecretKey, textVisitsNeedKey)
	}
	name, kind := model.ProviderFerun, model.ProviderKindFerun
	if provider != "" {
		p, err := tx.GetAccessProvider(ctx, tenant, provider)
		if errors.Is(err, store.ErrNoRow) {
			return apierr.WithCode(http.StatusUnprocessableEntity, CodeUnknownProvider, textUnknownProvider, provider)
		}
		if err != nil {
			return fmt.Errorf("provider %s: %w", provider, err)
		}
		name, kind = p.Name, p.Kind
	}
	if !slices.Contains(s.d.VisitKinds, kind) {
		return apierr.WithCode(http.StatusConflict, CodeFerunAuthRequired, textNeedsFerunAuth, name)
	}
	return nil
}

// checkZone refuses a publish of a tenant whose zone the platform does not
// serve (409), and a plain label of a tenant whose hosts are not in the
// apps zone (400): the tenant zone has no owners. tx holds the tenant's
// row.
func (s *Service) checkZone(ctx context.Context, tx *store.Store, tenant string, public, plain bool, label string) error {
	t, err := tx.GetTenant(ctx, tenant)
	if err != nil {
		return fmt.Errorf("tenant %s: %w", tenant, err)
	}
	z := s.zones()
	switch {
	case public && z.Of(t.Zone) == "":
		return apierr.New(http.StatusConflict, textZoneOff, t.Zone, tenant)
	case plain && !z.PlainLabels(tenant, t.Zone):
		return apierr.New(http.StatusBadRequest, textPlainLabelZone, label, tenant, z.Of(t.Zone))
	}
	return nil
}

// checkPlainLabel refuses with 409 host_label_taken a plain label, in the
// apps zone, that is the natural label of an existing tenant's
// environment: split at its first hyphen (model.NaturalPair), the rest names a tenant, the zone's
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
	owner, err := tx.HostLabelOwner(ctx, "", label)
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
