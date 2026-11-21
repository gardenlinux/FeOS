// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"

	"github.com/google/uuid"

	fapi "github.com/gardenlinux/feos/go/ferun/api"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/store"
)

// This file is ADR 0056's integrations as the API serves them: their
// registration by an operator, the list every signed-in person reads, and
// a project's grants to them. Their sign-in is the project class's
// (integrationPrincipal).

// integrationName is the rule of an integration's name.
var integrationName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// The codes of the integration routes.
const (
	codeIntegrationsUnavailable = fapi.CodeIntegrationsUnavailable
	codeSubjectTaken            = fapi.CodeSubjectTaken
	codeIssuerRefused           = fapi.CodeIssuerRefused
)

// integrationRoutes adds the routes of integrations.
func (s *server) integrationRoutes(operator func(string, apiHandler), person func(string, apiHandler),
	project func(need, string, apiHandler)) {
	person("GET /api/v1/integrations", s.withIntegrations(s.listIntegrationsForPeople))
	operator("GET /api/v1/admin/integrations", s.withIntegrations(s.listIntegrations))
	operator("PUT /api/v1/admin/integrations/{integration}", s.withIntegrations(s.putIntegration))
	operator("POST /api/v1/admin/integrations/{integration}/disable", s.withIntegrations(s.setIntegrationDisabled(true)))
	operator("POST /api/v1/admin/integrations/{integration}/enable", s.withIntegrations(s.setIntegrationDisabled(false)))
	operator("DELETE /api/v1/admin/integrations/{integration}", s.withIntegrations(s.deleteIntegration))

	// A project's admin of the scope grants an integration a role; the
	// integration may give up a grant of its own and change none.
	project(noIntegration(byScope(admin)), "PUT /api/v1/project/integrations/{integration}/grants",
		s.withIntegrations(s.putIntegrationGrant))
	project(orOwnIntegration(byScope(admin)), "DELETE /api/v1/project/integrations/{integration}/grants",
		s.deleteIntegrationGrant)
}

// withIntegrations answers 503 integrations_unavailable on an
// installation without --oidc-issuer: no integration can sign in there.
func (s *server) withIntegrations(h apiHandler) apiHandler {
	return func(w http.ResponseWriter, r *http.Request) error {
		if s.oidc == nil {
			return apierr.WithCode(http.StatusServiceUnavailable, codeIntegrationsUnavailable,
				"this installation has no integrations: it verifies no provider's tokens (--oidc-issuer)")
		}
		return h(w, r)
	}
}

func errNoIntegration() error { return apierr.New(http.StatusNotFound, "no such integration") }

// adminIntegration is an integration as the operators see it.
type adminIntegration struct {
	Name       string `json:"name"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Issuer     string `json:"issuer"`
	Subject    string `json:"subject"`
	Disabled   bool   `json:"disabled"`
	DisabledAt string `json:"disabledAt,omitempty"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
	LastSeen   string `json:"lastSeen,omitempty"`
	MadeBy     string `json:"madeBy"`
	Projects   int    `json:"projects"`
}

func adminIntegrationOf(i store.Integration) adminIntegration {
	return adminIntegration{Name: i.Name, Title: i.Title, URL: i.URL, Issuer: i.Issuer, Subject: i.Subject,
		Disabled: i.Disabled(), DisabledAt: rfc3339(i.DisabledAt), CreatedAt: rfc3339(i.CreatedAt),
		UpdatedAt: rfc3339(i.UpdatedAt), LastSeen: rfc3339(i.LastSeen), MadeBy: i.MadeBy, Projects: i.Projects}
}

// listIntegrationsForPeople is GET /api/v1/integrations: the enabled
// integrations by name, which every signed-in person reads (the grant
// dialog's choice).
func (s *server) listIntegrationsForPeople(w http.ResponseWriter, r *http.Request) error {
	is, err := s.st.ListIntegrations(r.Context())
	if err != nil {
		return fmt.Errorf("list integrations: %w", err)
	}
	out := fapi.IntegrationList{Integrations: []fapi.IntegrationInfo{}}
	for _, i := range is {
		if !i.Disabled() {
			out.Integrations = append(out.Integrations, fapi.IntegrationInfo{Name: i.Name, Title: i.Title, URL: i.URL})
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

// listIntegrations is GET /api/v1/admin/integrations: every integration,
// with the number of projects that grant it.
func (s *server) listIntegrations(w http.ResponseWriter, r *http.Request) error {
	is, err := s.st.ListIntegrations(r.Context())
	if err != nil {
		return fmt.Errorf("list integrations: %w", err)
	}
	out := struct {
		Integrations []adminIntegration `json:"integrations"`
	}{Integrations: make([]adminIntegration, 0, len(is))}
	for _, i := range is {
		out.Integrations = append(out.Integrations, adminIntegrationOf(i))
	}
	return writeJSON(w, http.StatusOK, out)
}

// integrationOfPath reads the integration the path's {integration} names;
// 400 for a name that breaks the rule, 404 for an unknown one.
func (s *server) integrationOfPath(r *http.Request) (store.Integration, error) {
	name := r.PathValue("integration")
	if !integrationName.MatchString(name) {
		return store.Integration{}, apierr.New(http.StatusBadRequest, "integration name must match %s", integrationName)
	}
	i, err := s.st.GetIntegration(r.Context(), name)
	if isNoRow(err) {
		return store.Integration{}, errNoIntegration()
	}
	if err != nil {
		return store.Integration{}, fmt.Errorf("integration %s: %w", name, err)
	}
	return i, nil
}

// putIntegration is PUT /api/v1/admin/integrations/{integration} {title,
// url, issuer?, subject}: 201 with an integration registered, 200 with
// one changed. The issuer is the installation's (--oidc-issuer), whose
// keys it verifies tokens with; another is 422 issuer_refused. An
// (issuer, subject) of another integration is 409 subject_taken.
func (s *server) putIntegration(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("integration")
	if !integrationName.MatchString(name) {
		return apierr.New(http.StatusBadRequest, "integration name must match %s", integrationName)
	}
	var req struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Issuer  string `json:"issuer"`
		Subject string `json:"subject"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.URL != "" {
		if u, err := url.Parse(req.URL); err != nil || u.Scheme != "https" || u.Host == "" {
			return apierr.New(http.StatusBadRequest, "url must be empty or an https URL, got %q", req.URL)
		}
	}
	if n := len(req.Subject); n < 1 || n > 255 {
		return apierr.New(http.StatusBadRequest, "subject must have 1 to 255 characters")
	}
	issuer := s.cfg.OIDC.Issuer
	if req.Issuer != "" && req.Issuer != issuer {
		return apierr.WithCode(http.StatusUnprocessableEntity, codeIssuerRefused,
			"this installation verifies tokens of %s only", issuer)
	}
	a, _ := auth.AdminFrom(r.Context())
	i, created, err := s.st.PutIntegration(r.Context(), store.IntegrationIn{Name: name, Title: req.Title, URL: req.URL,
		Issuer: issuer, Subject: req.Subject, MadeBy: a.Principal().String()})
	if errors.Is(err, store.ErrExists) {
		return apierr.WithCode(http.StatusConflict, codeSubjectTaken,
			"another integration signs in with the subject %s", req.Subject)
	}
	if err != nil {
		return fmt.Errorf("integration %s: %w", name, err)
	}
	kind, what, status := "integration_changed", "changed", http.StatusOK
	if created {
		kind, what, status = "integration_registered", "registered", http.StatusCreated
	}
	s.recordIntegration(r, kind, what, i)
	if i, err = s.st.GetIntegration(r.Context(), name); err != nil {
		return fmt.Errorf("integration %s: %w", name, err)
	}
	all, err := s.st.ListIntegrations(r.Context())
	if err != nil {
		return fmt.Errorf("integration %s: %w", name, err)
	}
	for _, x := range all {
		if x.ID == i.ID {
			i.Projects = x.Projects
		}
	}
	return writeJSON(w, status, adminIntegrationOf(i))
}

// recordIntegration writes the activity row of an operator's act on an
// integration.
func (s *server) recordIntegration(r *http.Request, kind, what string, i store.Integration) {
	by := adminOfRequest(r)
	s.log.Info("integration "+what, "integration", i.Name, "by", by)
	s.record(r.Context(), store.ActivityRow{Kind: kind, Category: "lifecycle",
		Name:   "integration " + i.Name + " " + what + " by " + by,
		Detail: detail(map[string]any{"integration": i.Name, "id": i.ID.String(), "by": by})})
}

// setIntegrationDisabled is POST /api/v1/admin/integrations/{integration}/
// disable and …/enable: a disabled integration signs in and reaches no
// project; its grants stay, so enabling it restores what it had.
func (s *server) setIntegrationDisabled(disabled bool) apiHandler {
	return func(w http.ResponseWriter, r *http.Request) error {
		if _, err := s.integrationOfPath(r); err != nil {
			return err
		}
		i, err := s.st.SetIntegrationDisabled(r.Context(), r.PathValue("integration"), disabled)
		if isNoRow(err) {
			return errNoIntegration()
		}
		if err != nil {
			return fmt.Errorf("integration %s: %w", r.PathValue("integration"), err)
		}
		kind, what := "integration_enabled", "enabled"
		if disabled {
			kind, what = "integration_disabled", "disabled"
		}
		s.recordIntegration(r, kind, what, i)
		return writeJSON(w, http.StatusOK, struct {
			Name     string `json:"name"`
			Disabled bool   `json:"disabled"`
		}{i.Name, i.Disabled()})
	}
}

// deleteIntegration is DELETE /api/v1/admin/integrations/{integration}:
// its grants, and the route and sandbox tokens it minted, go with it.
// What it made stays the projects'.
func (s *server) deleteIntegration(w http.ResponseWriter, r *http.Request) error {
	i, err := s.integrationOfPath(r)
	if err != nil {
		return err
	}
	ok, err := s.st.DeleteIntegration(r.Context(), i.Name)
	if err != nil {
		return fmt.Errorf("integration %s: %w", i.Name, err)
	}
	if !ok {
		return errNoIntegration()
	}
	s.recordIntegration(r, "integration_removed", "removed", i)
	return writeJSON(w, http.StatusOK, struct {
		Deleted bool `json:"deleted"`
	}{true})
}

// putIntegrationGrant is PUT /api/v1/project/integrations/{integration}/
// grants {role, environment?}: the integration's role on the project, or
// on the environment (its ID or name), by an admin of that scope. There is
// no invitation: the admin picks a registered integration, a disabled one
// too (the grant takes effect once it is enabled).
func (s *server) putIntegrationGrant(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Role        string `json:"role"`
		Environment string `json:"environment"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	ctx := r.Context()
	t, _ := auth.ProjectFrom(ctx)
	env, err := s.adminScope(ctx, t, req.Environment)
	if err != nil {
		return err
	}
	if _, ok := auth.ParseRole(req.Role); !ok {
		return apierr.New(http.StatusBadRequest, "role must be read, write or admin, got %q", req.Role)
	}
	i, err := s.integrationOfPath(r)
	if err != nil {
		return err
	}
	g, err := s.st.PutIntegrationGrant(ctx, t.Name, i.ID, env, req.Role, auth.MadeBy(ctx))
	if isNoRow(err) {
		return errNoEnvironment()
	}
	if err != nil {
		return fmt.Errorf("grant: %w", err)
	}
	return writeJSON(w, http.StatusOK, struct {
		Integration string `json:"integration"`
		Role        string `json:"role"`
		Environment string `json:"environment"`
	}{i.Name, g.Role, idOrEmpty(g.EnvironmentID)})
}

// deleteIntegrationGrant is DELETE /api/v1/project/integrations/
// {integration}/grants[?environment=<ID or name>]: the integration's grant
// on the project, or on the environment, goes, by an admin of that scope,
// or by the integration itself, which may give up its own grants and
// touch no other integration's (403 integration_refused).
func (s *server) deleteIntegrationGrant(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	t, _ := auth.ProjectFrom(ctx)
	ref := r.URL.Query().Get("environment")
	i, err := s.integrationOfPath(r)
	if t.Principal.IsIntegration() {
		if err != nil || i.ID != t.Principal.Integration {
			return errIntegrationRefused()
		}
	} else if err != nil {
		return err
	}
	env := uuid.Nil
	switch {
	case t.Principal.IsIntegration() && ref != "":
		if env, err = s.envRef(r, t, ref); err != nil {
			return err
		}
	case !t.Principal.IsIntegration():
		if env, err = s.adminScope(ctx, t, ref); err != nil {
			return err
		}
	}
	deleted, err := s.st.DeleteIntegrationGrantAt(ctx, t.Name, i.ID, env)
	if err != nil {
		return fmt.Errorf("delete grant: %w", err)
	}
	return writeJSON(w, http.StatusOK, struct {
		Deleted bool `json:"deleted"`
	}{deleted})
}
