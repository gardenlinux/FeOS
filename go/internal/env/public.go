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
// textZoneOff one of a project whose zone the platform does not serve.
const (
	textNoAppsZone = "the platform serves no apps zone (--apps-zone); an environment cannot be public"
	textZoneOff    = "the platform serves no zone %s, project %s's; an environment cannot be public until an operator moves the project"
)

// The refusals of a chosen host label.
const (
	textHostLabelNotPublic = "hostLabel is given only with public: true"
	textBadHostLabel       = "hostLabel %q: a chosen host label is <choice>--%s, the choice without \"--\", the whole a DNS label of at most 63 lowercase letters, digits and hyphens, not with \"--\" at its third and fourth characters; %s alone is the project's address, and the environment's fallback label is its own"
	textHostLabelTaken     = "host label %s is another project's or another environment's"
	// textNoProjectLabels refuses a label of a project whose name is no
	// label of its own (model.ProjectLabel): it has no address, no natural
	// and no chosen labels (ADR 0063).
	textNoProjectLabels = "hostLabel %q: project %s has no labels of its own, its name having a dot or \"--\" in it or ending in a hyphen and 16 hex characters; its environments get their fallback labels, and a domain gives one an address"
)

// The refusals of an access (ADR 0054).
const (
	textBadAccess          = "access is anyone or visitors"
	textProviderNoVisitors = "a provider goes with access visitors"
	textAccessNotPublic    = "access goes with public true"
	textVisitsNeedKey      = "this installation guards no environments: ferund runs without --secret-key-file"
	textUnknownProvider    = "no provider %s in this project"
	textNeedsFerunAuth     = "visitors of %s need ferun-auth, which this installation does not run"
)

// The codes of the refusals of an access (ADR 0054).
const (
	CodeInvalidAccess       = "invalid_access"
	CodeVisitsNeedSecretKey = "visits_need_secret_key"
	CodeUnknownProvider     = "unknown_provider"
	CodeFerunAuthRequired   = "ferun_auth_required"
)

// SetPublic is PUT /api/v1/environments/{env}/public: the project's
// decision whether the ingress serves the environment at its default host
// without a route token (ADR 0021), and whom it admits there (ADR 0054).
// The first publish gives the
// environment its host label and its ingress group, so that its image
// services roll once (Rolling); unpublishing keeps both, and publishing
// again rolls nothing. hostLabel, only with public, is a label the project
// chose (model.ChosenHostLabel, "<choice>--<project>") or the project's
// name, its address (model.ProjectLabel; ADR 0063), or the environment's
// own fallback (model.GivenHostLabel), else 400, in its
// project's zone (ADR 0031): the environment gets it in place of the label
// it had or would get, which stays the project's; a chosen label another
// project or environment holds is a 409, and so is the project's label
// when another project holds it. The project's address moves: the
// environment that held it gets its natural label or its fallback in the
// same transaction. Publishing is refused with
// 409 while the platform has no apps zone or does not serve the project's
// zone, and either is while the environment or its project is being
// deleted.
//
// The access, only with public true (400 invalid_access otherwise, and for
// an access other than anyone or visitors, or a provider without
// visitors), is anyone, or visitors of one of the project's providers or of
// the builtin ferun. Without access a publish of an environment that is
// not public admits visitors of ferun, and one of a public environment
// keeps its access; visitors without a provider are of ferun. Visitors are
// refused while no visits service runs (409 visits_need_secret_key,
// Deps.VisitKinds nil), of a provider the project does not have (422
// unknown_provider), and of a provider whose kind the installation does
// not serve (409 ferun_auth_required). Unpublishing ends the
// environment's visits, and so does a change of its access or provider.
// A call that changes nothing writes nothing and answers the same.
func (s *Service) SetPublic(ctx context.Context, project, env string, req PublicRequest) (PublicAnswer, error) {
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
	address := hostLabel != "" && hostLabel == project
	if hostLabel != "" && !model.ProjectLabel(project) {
		return PublicAnswer{}, apierr.New(http.StatusBadRequest, textNoProjectLabels, hostLabel, project)
	}
	// Taking the project's address moves it from another environment,
	// whose row it locks: the project's row FOR UPDATE keeps two such
	// moves apart.
	mode := store.ForShare
	if address {
		mode = store.ForUpdate
	}
	var before, after store.Environment
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		if err := lockProjectFor(ctx, tx, project, mode); err != nil {
			return err
		}
		if err := s.checkZone(ctx, tx, project, public); err != nil {
			return err
		}
		var err error
		if before, err = resolveEnv(ctx, tx, project, env, store.ForUpdate); err != nil {
			return err
		}
		if before.Deleting {
			return errDeleting(before.Name)
		}
		// The environment's own fallback is a label too: giving the
		// address up names it where the name gives no natural label.
		if hostLabel != "" && !address && !model.ChosenHostLabel(hostLabel, project) &&
			!model.GivenHostLabel(hostLabel, project, before.ID) {
			return apierr.New(http.StatusBadRequest, textBadHostLabel, hostLabel, project, project)
		}
		access, provider := before.Access, before.AccessProvider
		if public {
			access, provider = accessAfter(before, req)
			if err := s.checkAccess(ctx, tx, project, access, provider); err != nil {
				return err
			}
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
	ans := PublicAnswer{Public: after.Public, Hosts: hosts, HostLabel: after.HostLabel,
		ProjectAddress: holdsAddress(after), Rolling: public && !before.IngressGroup}
	ans.Access, ans.Provider = accessOf(after)
	if before.Public != public || before.HostLabel != after.HostLabel || before.Access != after.Access ||
		before.AccessProvider != after.AccessProvider {
		s.log.Info("environment public", "project", project, "env", after.Name, "id", after.ID, "public", public,
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
// provider project does not have (422 unknown_provider), and of one whose
// kind it does not serve (409 ferun_auth_required). provider is "" for
// ferun. tx holds the project's row.
func (s *Service) checkAccess(ctx context.Context, tx *store.Store, project, access, provider string) error {
	if access != model.AccessVisitors {
		return nil
	}
	if s.d.VisitKinds == nil {
		return apierr.WithCode(http.StatusConflict, CodeVisitsNeedSecretKey, textVisitsNeedKey)
	}
	name, kind := model.ProviderFerun, model.ProviderKindFerun
	if provider != "" {
		p, err := tx.GetAccessProvider(ctx, project, provider)
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

// checkZone refuses a publish of a project whose zone the platform does not
// serve (409). tx holds the project's row.
func (s *Service) checkZone(ctx context.Context, tx *store.Store, project string, public bool) error {
	t, err := tx.GetProject(ctx, project)
	if err != nil {
		return fmt.Errorf("project %s: %w", project, err)
	}
	if public && s.zones().Of(t.Zone) == "" {
		return apierr.New(http.StatusConflict, textZoneOff, t.Zone, project)
	}
	return nil
}
