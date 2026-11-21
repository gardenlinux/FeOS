// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	fapi "github.com/gardenlinux/feos/go/ferun/api"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/store"
)

// This file is ADR 0031's "People make projects and invite each other":
// the projects people make, a project's members and their grants,
// invitations, the project's tokens as its admins mint them, leaving a
// project, and the operators' switch of a person.

// The values of Config.ProjectCreation (--project-creation).
const (
	ProjectCreationPeople    = "people"
	ProjectCreationOperators = "operators"
)

// invitationTTL is how long an invitation's link can be used.
const invitationTTL = 7 * 24 * time.Hour

// fixedReservedProjectNames are the names no project made by a person may
// have, beside Config.ReservedProjectNames.
var fixedReservedProjectNames = []string{store.OperatorProject, "admin", "api", "www"}

// CodeLastAdmin is the code of a 409 for a change that would leave a
// project without an admin.
const CodeLastAdmin = "last_admin"

// CodeReservedName is the code of a 422 for a project name the platform
// keeps.
const CodeReservedName = "reserved_name"

// memberRoutes adds the routes of people in projects.
func (s *server) memberRoutes(operator func(string, apiHandler), person func(string, apiHandler),
	project func(need, string, apiHandler)) {
	operator("GET /api/v1/admin/people", s.listPeople)
	operator("POST /api/v1/admin/people/{person}/disable", s.setPersonDisabled(true))
	operator("POST /api/v1/admin/people/{person}/enable", s.setPersonDisabled(false))

	person("POST /api/v1/projects", s.createPersonProject)
	person("GET /api/v1/invitations/{token}", s.previewInvitation)
	person("POST /api/v1/invitations/{token}/accept", s.acceptInvitation)

	// Members see each other in their project; a grant is changed by an
	// admin of its scope (the project, or the body's or query's
	// environment), which the handler checks. An integration changes no
	// principal of a project (ADR 0056): members, invitations and tokens
	// refuse it whatever its role; it reads the members and leaves.
	grantor := noIntegration(byScope(admin))
	project(member, "GET /api/v1/project/members", s.listMembers)
	project(grantor, "PUT /api/v1/project/members/{person}/grants", s.putMemberGrant)
	project(grantor, "DELETE /api/v1/project/members/{person}/grants", s.deleteMemberGrant)
	project(member, "POST /api/v1/project/leave", s.leaveProject)

	project(noIntegration(member), "GET /api/v1/project/invitations", s.listInvitations)
	project(grantor, "POST /api/v1/project/invitations", s.createInvitation)
	project(grantor, "DELETE /api/v1/project/invitations/{id}", s.revokeInvitation)

	project(noIntegration(onProject(admin)), "GET /api/v1/project/tokens", s.listOwnTokens)
	project(noIntegration(onProject(admin)), "POST /api/v1/project/tokens", s.mintOwnToken)
	project(noIntegration(onProject(admin)), "DELETE /api/v1/project/tokens/{hash}", s.revokeOwnToken)
	// A token's grants beside its own, as a member's (ADR 0060: the
	// protect dialog gives a token a grant on the environment).
	project(grantor, "PUT /api/v1/project/tokens/{hash}/grants", s.putTokenGrant)
	project(grantor, "DELETE /api/v1/project/tokens/{hash}/grants", s.deleteTokenGrant)
}

// ----------------------------------------------------------- person class --

type personKey struct{}

// person is the person class: a signed-in person's session (an
// operator's too) with a session's CSRF rules, in no project. An
// integration's bearer (a JWT) is refused, 403 integration_refused: an
// integration is no person (ADR 0056).
func (s *server) person(h apiHandler) http.Handler {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		if isJWT(bearer(r)) {
			return errIntegrationRefused()
		}
		sess, ok, err := s.session(r)
		if err != nil {
			return err
		}
		if !ok || sess.PersonID == uuid.Nil {
			return apierr.New(http.StatusUnauthorized, "sign-in required")
		}
		if err := s.checkCSRF(r); err != nil {
			return err
		}
		return h(w, r.WithContext(context.WithValue(r.Context(), personKey{}, sess)))
	})
}

// personSession is the person-class request's session.
func personSession(r *http.Request) store.UISession {
	sess, _ := r.Context().Value(personKey{}).(store.UISession)
	return sess
}

// sessionPrincipal is a session as a principal.
func sessionPrincipal(sess store.UISession) auth.Principal {
	return auth.Principal{Person: sess.PersonID, Operator: sess.Admin}
}

// ---------------------------------------------------------------- projects --

// reservedProjectName reports whether a person may not make a project of
// that name.
func (s *server) reservedProjectName(name string) bool {
	return slices.Contains(fixedReservedProjectNames, name) || slices.Contains(s.cfg.ReservedProjectNames, name)
}

// createPersonProject is POST /api/v1/projects {name}: a signed-in person
// makes a project and holds admin on it (ADR 0031). --project-creation
// operators keeps it to operators; --projects-per-person bounds the
// projects a person made that are not being deleted (operators are not
// bound). The project gets the default limits (--project-default-*) and its
// default hosts in the project zone (--project-zone; the apps zone without
// one), an operator's as well: what this route makes is a person's
// project. No token: the session acts.
func (s *server) createPersonProject(w http.ResponseWriter, r *http.Request) error {
	sess := personSession(r)
	var req struct {
		Name string `json:"name"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if s.cfg.ProjectCreation == ProjectCreationOperators && !sess.Admin {
		return apierr.New(http.StatusForbidden, "on this platform only operators make projects")
	}
	if !store.ProjectNamePattern.MatchString(req.Name) {
		return apierr.New(http.StatusBadRequest, "project name must match %s", store.ProjectNamePattern)
	}
	if s.reservedProjectName(req.Name) {
		return apierr.WithCode(http.StatusUnprocessableEntity, CodeReservedName,
			"the project name %s is reserved by the platform", req.Name)
	}
	by := sessionPrincipal(sess).String()
	var t store.Project
	err := s.st.InTx(r.Context(), func(tx *store.Store) error {
		if _, err := tx.LockPerson(r.Context(), sess.PersonID); err != nil {
			return err
		}
		if max := s.cfg.ProjectsPerPerson; max > 0 && !sess.Admin {
			n, err := tx.CountProjectsMadeBy(r.Context(), sess.PersonID)
			if err != nil {
				return err
			}
			if n >= max {
				return apierr.QuotaExceeded("projects", int64(max), int64(n), 1)
			}
		}
		var created bool
		var err error
		t, created, err = tx.MakeProject(r.Context(), req.Name, store.NewProject{Zone: s.cfg.Zones.ForNewProject(true),
			MadeBy: sess.PersonID, Limits: s.cfg.ProjectDefaults})
		if err != nil {
			return err
		}
		if !created {
			if t.Deleting() {
				return apierr.New(http.StatusConflict, "project %s is being deleted", req.Name)
			}
			return apierr.New(http.StatusConflict, "project %s already exists", req.Name)
		}
		_, err = tx.PutGrant(r.Context(), store.Grant{ProjectID: t.ID, PersonID: sess.PersonID,
			Role: auth.RoleAdmin.String(), MadeBy: by})
		return err
	})
	if err != nil {
		return passOr(err, "create project")
	}
	s.log.Info("project created", "project", t.ID, "by", by)
	s.record(r.Context(), store.ActivityRow{ProjectID: t.ID, Kind: "project_created", Category: "lifecycle",
		Name: "project " + t.ID + " created by " + by, Detail: detail(map[string]any{"project": t.ID, "by": by})})
	return writeJSON(w, http.StatusCreated, struct {
		Name      string     `json:"name"`
		CreatedAt string     `json:"createdAt"`
		Role      string     `json:"role"`
		Limits    limitsView `json:"limits"`
	}{t.ID, rfc3339(t.CreatedAt), auth.RoleAdmin.String(), limitsOf(t.Limits)})
}

// ---------------------------------------------------------------- members --

// grantView is a grant as the members' routes show it: the environment's
// ID and name, "" for the project.
type grantView struct {
	Role            string `json:"role"`
	Environment     string `json:"environment,omitempty"`
	EnvironmentName string `json:"environmentName,omitempty"`
}

// idOrEmpty is an ID as answers write it, "" for uuid.Nil.
func idOrEmpty(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// listMembers is GET /api/v1/project/members: the people who hold a grant
// in the project, with their names, e-mail addresses, issuers and
// subjects, which only the project's members see (ADR 0031, 0056), and the
// integrations that hold one. Of the grants on environments, those the
// principal may not read are left out; each grant names its principal and
// that principal's name.
func (s *server) listMembers(w http.ResponseWriter, r *http.Request) error {
	ctx, project := r.Context(), projectOf(r)
	ms, err := s.st.ListMembers(ctx, project)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}
	is, err := s.st.ListIntegrationGrants(ctx, project)
	if err != nil {
		return fmt.Errorf("list integrations: %w", err)
	}
	var made []string
	for _, m := range ms {
		for _, g := range m.Grants {
			made = append(made, g.MadeBy)
		}
	}
	for _, i := range is {
		for _, g := range i.Grants {
			made = append(made, g.MadeBy)
		}
	}
	names, err := s.st.PrincipalNames(ctx, made)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}
	// grants splits grants into the project's role and its made_by, and
	// the readable environments' grants.
	grants := func(gs []store.MemberGrant) (role, madeBy string, envs []fapi.EnvGrant) {
		envs = []fapi.EnvGrant{}
		for _, g := range gs {
			switch {
			case g.EnvironmentID == uuid.Nil:
				role, madeBy = g.Role, g.MadeBy
			case mayReadEnv(r, g.EnvironmentID.String()):
				envs = append(envs, fapi.EnvGrant{ID: g.EnvironmentID.String(), Name: g.EnvironmentName, Role: g.Role,
					MadeBy: g.MadeBy, MadeByName: names[g.MadeBy]})
			}
		}
		return role, madeBy, envs
	}
	out := fapi.Members{Members: make([]fapi.Member, 0, len(ms)), Integrations: make([]fapi.IntegrationGrant, 0, len(is))}
	for _, m := range ms {
		role, by, envs := grants(m.Grants)
		out.Members = append(out.Members, fapi.Member{Person: m.ID.String(), Name: m.Name, Email: m.Email, Issuer: m.Issuer,
			Subject: m.Sub, Disabled: m.Disabled(), Role: role, MadeBy: by, MadeByName: names[by], Environments: envs})
	}
	for _, i := range is {
		role, by, envs := grants(i.Grants)
		out.Integrations = append(out.Integrations, fapi.IntegrationGrant{Integration: i.Name, Title: i.Title, URL: i.URL,
			Disabled: i.Disabled(), Role: role, MadeBy: by, MadeByName: names[by], Environments: envs})
	}
	return writeJSON(w, http.StatusOK, out)
}

func errNoMember() error { return apierr.New(http.StatusNotFound, "no such member") }

// adminScope resolves a grant's or an invitation's scope, the
// environment ref names (its ID or name; "": the project), and checks that
// the principal is admin of it: on the project 403 role_required; on an
// environment it does not see 404, one it holds a lower role on 403.
func (s *server) adminScope(ctx context.Context, t auth.Project, ref string) (uuid.UUID, error) {
	if ref == "" {
		return uuid.Nil, mayAdminScope(t, uuid.Nil)
	}
	env, err := s.st.EnvironmentRef(ctx, t.Name, ref)
	if isNoRow(err) {
		return uuid.Nil, errNoEnvironment()
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("environment %s: %w", ref, err)
	}
	return env, mayAdminScope(t, env)
}

// memberOf reads the path's {person}, who must be a member of the project.
func memberOf(ctx context.Context, tx *store.Store, r *http.Request) (uuid.UUID, error) {
	person, err := uuid.Parse(r.PathValue("person"))
	if err != nil {
		return uuid.Nil, errNoMember()
	}
	ok, err := tx.IsMember(ctx, projectOf(r), person)
	if err != nil {
		return uuid.Nil, err
	}
	if !ok {
		return uuid.Nil, errNoMember()
	}
	return person, nil
}

// keepAdmin runs change with the project's row FOR NO KEY UPDATE, which
// serialises the changes of its admins, and refuses (409 last_admin) a
// change that leaves a project that had an admin without one: a project
// always has an admin (ADR 0031). A person who is not disabled or a
// token, with admin on the project itself, is one.
func keepAdmin(ctx context.Context, tx *store.Store, project string, change func() error) error {
	t, err := tx.LockProject(ctx, project, store.ForNoKeyUpdate)
	if isNoRow(err) {
		return errNoProject()
	}
	if err != nil {
		return err
	}
	if t.Deleting() {
		return apierr.New(http.StatusConflict, "project %s is being deleted", project)
	}
	before, err := tx.CountAdmins(ctx, project)
	if err != nil {
		return err
	}
	if err := change(); err != nil {
		return err
	}
	after, err := tx.CountAdmins(ctx, project)
	if err != nil {
		return err
	}
	if before > 0 && after == 0 {
		return apierr.WithCode(http.StatusConflict, CodeLastAdmin,
			"project %s would have no admin left: make another member admin first", project)
	}
	return nil
}

// putMemberGrant is PUT /api/v1/project/members/{person}/grants {role,
// environment?}: the member's role on the project, or on the environment
// (its ID or name), by an admin of that scope. Only a member is given a
// grant: a person joins by an invitation.
func (s *server) putMemberGrant(w http.ResponseWriter, r *http.Request) error {
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
	var g store.Grant
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		return keepAdmin(ctx, tx, projectOf(r), func() error {
			person, err := memberOf(ctx, tx, r)
			if err != nil {
				return err
			}
			g, err = tx.PutGrant(ctx, store.Grant{ProjectID: projectOf(r), PersonID: person, EnvironmentID: env,
				Role: req.Role, MadeBy: auth.MadeBy(ctx)})
			return err
		})
	})
	if err != nil {
		return passOr(err, "grant")
	}
	return writeJSON(w, http.StatusOK, struct {
		Person string `json:"person"`
		grantView
	}{g.PersonID.String(), grantView{Role: g.Role, Environment: idOrEmpty(g.EnvironmentID)}})
}

// deleteMemberGrant is DELETE /api/v1/project/members/{person}/grants
// [?environment=<ID or name>]: the member's grant on the project, or on
// the environment, goes, by an admin of that scope, unless it is the
// project's last admin's. Without grants left, they are no member.
func (s *server) deleteMemberGrant(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	t, _ := auth.ProjectFrom(ctx)
	env, err := s.adminScope(ctx, t, r.URL.Query().Get("environment"))
	if err != nil {
		return err
	}
	var deleted bool
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		return keepAdmin(ctx, tx, projectOf(r), func() error {
			person, err := memberOf(ctx, tx, r)
			if err != nil {
				return err
			}
			deleted, err = tx.DeletePersonGrantAt(ctx, projectOf(r), person, env)
			return err
		})
	})
	if err != nil {
		return passOr(err, "delete grant")
	}
	return writeJSON(w, http.StatusOK, struct {
		Deleted bool `json:"deleted"`
	}{deleted})
}

// leaveProject is POST /api/v1/project/leave: a person gives up every grant
// of theirs in the project, unless they are its last admin; an integration
// gives up every grant of its own there (ADR 0056). A token does not
// leave: it is revoked.
func (s *server) leaveProject(w http.ResponseWriter, r *http.Request) error {
	t, _ := auth.ProjectFrom(r.Context())
	ctx := r.Context()
	if t.Principal.IsIntegration() {
		if _, err := s.st.DeleteIntegrationGrants(ctx, t.Name, t.Principal.Integration); err != nil {
			return fmt.Errorf("leave: %w", err)
		}
		return writeJSON(w, http.StatusOK, struct {
			Left bool `json:"left"`
		}{true})
	}
	if t.Principal.Person == uuid.Nil {
		return apierr.New(http.StatusBadRequest, "a token does not leave its project: revoke it")
	}
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		return keepAdmin(ctx, tx, t.Name, func() error {
			_, err := tx.DeletePersonGrants(ctx, t.Name, t.Principal.Person)
			return err
		})
	})
	if err != nil {
		return passOr(err, "leave")
	}
	return writeJSON(w, http.StatusOK, struct {
		Left bool `json:"left"`
	}{true})
}

// ------------------------------------------------------------ invitations --

// invitationView is an invitation as listed: its ID is the hash of its
// secret, never the secret.
type invitationView struct {
	ID string `json:"id"`
	grantView
	MadeBy     string `json:"madeBy"`
	CreatedAt  string `json:"createdAt"`
	ExpiresAt  string `json:"expiresAt"`
	UsedAt     string `json:"usedAt,omitempty"`
	UsedBy     string `json:"usedBy,omitempty"`
	UsedByName string `json:"usedByName,omitempty"`
}

func invitationOf(i store.Invitation) invitationView {
	v := invitationView{ID: i.Hash, grantView: grantView{Role: i.Role, Environment: idOrEmpty(i.EnvironmentID),
		EnvironmentName: i.EnvironmentName}, MadeBy: i.MadeBy, CreatedAt: rfc3339(i.CreatedAt),
		ExpiresAt: rfc3339(i.ExpiresAt), UsedAt: rfc3339(i.UsedAt), UsedByName: i.UsedByName}
	if i.UsedBy != uuid.Nil {
		v.UsedBy = i.UsedBy.String()
	}
	return v
}

// inviteURL is the UI's page of an invitation, <public URL>/invite/<token>.
func (s *server) inviteURL(token string) string { return s.cfg.PublicURL + "/invite/" + token }

// createInvitation is POST /api/v1/project/invitations {role,
// environment?}: a single-use link that gives whoever opens it signed in
// the role on the project or the environment (its ID or name), valid for 7
// days, by an admin of that scope. The answer holds the secret (token)
// and the UI's page of it (url) once; only its hash is stored. FeRun
// sends no mail: the admin passes the link on.
func (s *server) createInvitation(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Role        string `json:"role"`
		Environment string `json:"environment"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	ctx, project := r.Context(), projectOf(r)
	t, _ := auth.ProjectFrom(ctx)
	env, err := s.adminScope(ctx, t, req.Environment)
	if err != nil {
		return err
	}
	if _, ok := auth.ParseRole(req.Role); !ok {
		return apierr.New(http.StatusBadRequest, "role must be read, write or admin, got %q", req.Role)
	}
	secret, hash, err := mintProjectToken()
	if err != nil {
		return fmt.Errorf("invitation: %w", err)
	}
	var inv store.Invitation
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if err := lockProjectForToken(ctx, tx, project); err != nil {
			return err
		}
		var err error
		inv, err = tx.InsertInvitation(ctx, store.Invitation{Hash: hash, ProjectID: project, EnvironmentID: env,
			Role: req.Role, MadeBy: auth.MadeBy(ctx)}, invitationTTL)
		if isNoRow(err) {
			return errNoEnvironment()
		}
		return err
	})
	if err != nil {
		return passOr(err, "invitation")
	}
	return writeJSON(w, http.StatusCreated, struct {
		invitationView
		Token string `json:"token"`
		URL   string `json:"url"`
	}{invitationOf(inv), secret, s.inviteURL(secret)})
}

// mayAdminScope checks admin on an invitation's scope: the project, or its
// environment (absent when the principal holds no role there).
func mayAdminScope(t auth.Project, env uuid.UUID) error {
	if env == uuid.Nil {
		if t.MayOnProject(auth.RoleAdmin) {
			return nil
		}
		return apierr.RoleRequired(auth.RoleAdmin.String(), "project")
	}
	if t.Principal.IsZero() {
		return nil
	}
	return allowEnvID(t, env, auth.RoleAdmin)
}

// listInvitations is GET /api/v1/project/invitations: the project's
// invitations, open, used and expired, whose scope the principal is
// admin of.
func (s *server) listInvitations(w http.ResponseWriter, r *http.Request) error {
	invs, err := s.st.ListInvitations(r.Context(), projectOf(r))
	if err != nil {
		return fmt.Errorf("list invitations: %w", err)
	}
	t, _ := auth.ProjectFrom(r.Context())
	out := struct {
		Invitations []invitationView `json:"invitations"`
	}{Invitations: []invitationView{}}
	for _, i := range invs {
		if mayAdminScope(t, i.EnvironmentID) == nil {
			out.Invitations = append(out.Invitations, invitationOf(i))
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

func errNoInvitation() error { return apierr.New(http.StatusNotFound, "no such invitation") }

// revokeInvitation is DELETE /api/v1/project/invitations/{id}, by an admin of its
// scope; an invitation the principal cannot see is absent.
func (s *server) revokeInvitation(w http.ResponseWriter, r *http.Request) error {
	inv, err := s.st.GetInvitation(r.Context(), r.PathValue("id"))
	if isNoRow(err) || (err == nil && inv.ProjectID != projectOf(r)) {
		return errNoInvitation()
	}
	if err != nil {
		return fmt.Errorf("invitation: %w", err)
	}
	t, _ := auth.ProjectFrom(r.Context())
	if err := mayAdminScope(t, inv.EnvironmentID); err != nil {
		if inv.EnvironmentID != uuid.Nil && t.EnvRole(inv.EnvironmentID) == auth.RoleNone {
			return errNoInvitation()
		}
		return err
	}
	ok, err := s.st.DeleteInvitation(r.Context(), inv.ProjectID, inv.Hash)
	if err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	return writeJSON(w, http.StatusOK, struct {
		Revoked bool `json:"revoked"`
	}{ok})
}

// previewInvitation is GET /api/v1/invitations/{token}: what an
// invitation gives, to the signed-in person who holds its link, while it
// is open or theirs: {project, role, environment?, environmentName?,
// expiresAt}; otherwise 404, as an unknown one.
func (s *server) previewInvitation(w http.ResponseWriter, r *http.Request) error {
	sess := personSession(r)
	inv, err := s.st.GetInvitation(r.Context(), store.HashToken(r.PathValue("token")))
	if isNoRow(err) {
		return errNoInvitation()
	}
	if err != nil {
		return fmt.Errorf("invitation: %w", err)
	}
	t, err := s.st.GetProject(r.Context(), inv.ProjectID)
	if err != nil && !isNoRow(err) {
		return fmt.Errorf("invitation: %w", err)
	}
	if isNoRow(err) || t.Deleting() || (inv.UsedBy != sess.PersonID && !inv.Open(time.Now())) {
		return errNoInvitation()
	}
	return writeJSON(w, http.StatusOK, struct {
		Project string `json:"project"`
		grantView
		ExpiresAt string `json:"expiresAt"`
	}{inv.ProjectID, grantView{Role: inv.Role, Environment: idOrEmpty(inv.EnvironmentID),
		EnvironmentName: inv.EnvironmentName}, rfc3339(inv.ExpiresAt)})
}

// acceptInvitation is POST /api/v1/invitations/{token}/accept: the
// signed-in person takes an open invitation and holds its grant, or the
// higher role they hold there already. Once taken, it is gone for
// everyone else (410); the person who took it gets the same answer again.
func (s *server) acceptInvitation(w http.ResponseWriter, r *http.Request) error {
	sess := personSession(r)
	ctx, hash := r.Context(), store.HashToken(r.PathValue("token"))
	inv, err := s.st.GetInvitation(ctx, hash)
	if isNoRow(err) {
		return errNoInvitation()
	}
	if err != nil {
		return fmt.Errorf("invitation: %w", err)
	}
	var g store.Grant
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		t, err := tx.LockProject(ctx, inv.ProjectID, store.ForShare)
		if isNoRow(err) || (err == nil && t.Deleting()) {
			return errNoInvitation()
		}
		if err != nil {
			return err
		}
		used, err := tx.UseInvitation(ctx, hash, sess.PersonID)
		if isNoRow(err) {
			cur, err := tx.GetInvitation(ctx, hash)
			switch {
			case isNoRow(err):
				return errNoInvitation()
			case err != nil:
				return err
			case cur.UsedBy == sess.PersonID:
				g, err = tx.PersonGrantAt(ctx, cur.ProjectID, sess.PersonID, cur.EnvironmentID)
				if isNoRow(err) {
					return apierr.New(http.StatusGone, "this invitation was used")
				}
				return err
			case cur.UsedBy != uuid.Nil:
				return apierr.New(http.StatusGone, "this invitation was used")
			}
			return apierr.New(http.StatusGone, "this invitation expired")
		}
		if err != nil {
			return err
		}
		role := used.Role
		if have, err := tx.PersonGrantAt(ctx, used.ProjectID, sess.PersonID, used.EnvironmentID); err == nil {
			a, _ := auth.ParseRole(have.Role)
			b, _ := auth.ParseRole(used.Role)
			role = max(a, b).String()
		} else if !isNoRow(err) {
			return err
		}
		g, err = tx.PutGrant(ctx, store.Grant{ProjectID: used.ProjectID, PersonID: sess.PersonID,
			EnvironmentID: used.EnvironmentID, Role: role, MadeBy: used.MadeBy})
		if isNoRow(err) {
			return errNoInvitation()
		}
		return err
	})
	if err != nil {
		return passOr(err, "accept invitation")
	}
	s.log.Info("invitation accepted", "project", inv.ProjectID, "person", sess.PersonID, "role", g.Role,
		"environment", idOrEmpty(g.EnvironmentID))
	return writeJSON(w, http.StatusOK, struct {
		Project string `json:"project"`
		grantView
	}{inv.ProjectID, grantView{Role: g.Role, Environment: idOrEmpty(g.EnvironmentID),
		EnvironmentName: inv.EnvironmentName}})
}

// ----------------------------------------------------------------- tokens --

// listOwnTokens is GET /api/v1/project/tokens: the project's tokens, by its admins.
func (s *server) listOwnTokens(w http.ResponseWriter, r *http.Request) error {
	toks, err := s.st.ListProjectTokens(r.Context(), projectOf(r))
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	out := struct {
		Tokens []projectTokenInfo `json:"tokens"`
	}{Tokens: make([]projectTokenInfo, 0, len(toks))}
	for _, t := range toks {
		out.Tokens = append(out.Tokens, projectTokenOf(t))
	}
	return writeJSON(w, http.StatusOK, out)
}

// mintOwnToken is POST /api/v1/project/tokens {label?, role, environment?}: a
// token of the project with its own grant, by an admin of the project, never
// above the minter's role on the token's scope (ADR 0031). It is shown
// once.
func (s *server) mintOwnToken(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Label       string `json:"label"`
		Role        string `json:"role"`
		Environment string `json:"environment"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	role, ok := auth.ParseRole(req.Role)
	if !ok {
		return apierr.New(http.StatusBadRequest, "role must be read, write or admin, got %q", req.Role)
	}
	ctx := r.Context()
	t, _ := auth.ProjectFrom(ctx)
	env := uuid.Nil
	if req.Environment != "" {
		id, err := s.st.EnvironmentRef(ctx, t.Name, req.Environment)
		if isNoRow(err) {
			return errNoEnvironment()
		}
		if err != nil {
			return fmt.Errorf("environment %s: %w", req.Environment, err)
		}
		env = id
	}
	switch {
	case env == uuid.Nil && !t.MayOnProject(role):
		return apierr.RoleRequired(role.String(), "project")
	case env != uuid.Nil && !t.Principal.IsZero() && t.EnvRole(env) < role:
		return apierr.RoleRequired(role.String(), "environment")
	}
	token, hash, err := mintProjectToken()
	if err != nil {
		return fmt.Errorf("create token: %w", err)
	}
	var tok store.ProjectToken
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if err := lockProjectForToken(ctx, tx, t.Name); err != nil {
			return err
		}
		var err error
		tok, err = tx.InsertProjectToken(ctx, store.ProjectToken{Hash: hash, ProjectID: t.Name, Label: req.Label,
			Role: role.String(), EnvironmentID: env, MadeBy: auth.MadeBy(ctx)})
		if isNoRow(err) {
			return errNoEnvironment()
		}
		return err
	})
	if err != nil {
		return passOr(err, "create token")
	}
	return writeJSON(w, http.StatusCreated, struct {
		projectTokenInfo
		Token string `json:"token"`
	}{projectTokenOf(tok), token})
}

// revokeOwnToken is DELETE /api/v1/project/tokens/{hash}, by an admin of the
// project, unless the token is its last admin.
func (s *server) revokeOwnToken(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var ok bool
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		return keepAdmin(ctx, tx, projectOf(r), func() error {
			var err error
			ok, err = tx.DeleteProjectToken(ctx, projectOf(r), r.PathValue("hash"))
			return err
		})
	})
	if err != nil {
		return passOr(err, "revoke token")
	}
	return writeJSON(w, http.StatusOK, struct {
		Revoked bool `json:"revoked"`
	}{ok})
}

func errNoToken() error { return apierr.New(http.StatusNotFound, "no such token") }

// tokenOf reads the path's {hash}, a token of the project.
func tokenOf(r *http.Request, tx *store.Store) (store.ProjectToken, error) {
	tok, err := tx.GetProjectToken(r.Context(), projectOf(r), r.PathValue("hash"))
	if isNoRow(err) {
		return store.ProjectToken{}, errNoToken()
	}
	return tok, err
}

// putTokenGrant is PUT /api/v1/project/tokens/{hash}/grants {role,
// environment?}: a grant of the token beside its own, on the project or on
// the environment (its ID or name), by an admin of that scope, as a
// member's grant is given (ADR 0060's dialog gives a token write on the
// environment it protects).
func (s *server) putTokenGrant(w http.ResponseWriter, r *http.Request) error {
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
	var g store.Grant
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		return keepAdmin(ctx, tx, projectOf(r), func() error {
			tok, err := tokenOf(r, tx)
			if err != nil {
				return err
			}
			g, err = tx.PutGrant(ctx, store.Grant{ProjectID: projectOf(r), TokenHash: tok.Hash, EnvironmentID: env,
				Role: req.Role, MadeBy: auth.MadeBy(ctx)})
			return err
		})
	})
	if err != nil {
		return passOr(err, "grant")
	}
	return writeJSON(w, http.StatusOK, struct {
		Token       string `json:"token"`
		Role        string `json:"role"`
		Environment string `json:"environment"`
	}{g.TokenHash, g.Role, idOrEmpty(g.EnvironmentID)})
}

// deleteTokenGrant is DELETE /api/v1/project/tokens/{hash}/grants
// [?environment=<ID or name>]: the token's grant (of those beside its own)
// on the project or on the environment goes, by an admin of that scope,
// unless it is the project's last admin's.
func (s *server) deleteTokenGrant(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	t, _ := auth.ProjectFrom(ctx)
	env, err := s.adminScope(ctx, t, r.URL.Query().Get("environment"))
	if err != nil {
		return err
	}
	var deleted bool
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		return keepAdmin(ctx, tx, projectOf(r), func() error {
			tok, err := tokenOf(r, tx)
			if err != nil {
				return err
			}
			deleted, err = tx.DeleteTokenGrantAt(ctx, projectOf(r), tok.Hash, env)
			return err
		})
	})
	if err != nil {
		return passOr(err, "delete grant")
	}
	return writeJSON(w, http.StatusOK, struct {
		Deleted bool `json:"deleted"`
	}{deleted})
}

// ----------------------------------------------------------------- people --

// personView is a person as the operators see them.
type personView struct {
	ID         string `json:"id"`
	Issuer     string `json:"issuer"`
	Sub        string `json:"sub"`
	Email      string `json:"email"`
	Name       string `json:"name"`
	FirstSeen  string `json:"firstSeen"`
	LastSeen   string `json:"lastSeen"`
	DisabledAt string `json:"disabledAt,omitempty"`
}

func personViewOf(p store.Person) personView {
	return personView{ID: p.ID.String(), Issuer: p.Issuer, Sub: p.Sub, Email: p.Email, Name: p.Name,
		FirstSeen: rfc3339(p.FirstSeen), LastSeen: rfc3339(p.LastSeen), DisabledAt: rfc3339(p.DisabledAt)}
}

// listPeople is GET /api/v1/admin/people: everyone who signed in, the
// last seen first.
func (s *server) listPeople(w http.ResponseWriter, r *http.Request) error {
	ps, err := s.st.ListPeople(r.Context())
	if err != nil {
		return fmt.Errorf("list people: %w", err)
	}
	out := struct {
		People []personView `json:"people"`
	}{People: make([]personView, 0, len(ps))}
	for _, p := range ps {
		out.People = append(out.People, personViewOf(p))
	}
	return writeJSON(w, http.StatusOK, out)
}

// setPersonDisabled is POST /api/v1/admin/people/{person}/disable and
// …/enable: a disabled person loses every session at once and signs in no
// more; their grants stay, so enabling them restores what they had.
func (s *server) setPersonDisabled(disabled bool) apiHandler {
	return func(w http.ResponseWriter, r *http.Request) error {
		id, err := uuid.Parse(r.PathValue("person"))
		if err != nil {
			return apierr.New(http.StatusNotFound, "no such person")
		}
		p, err := s.st.SetPersonDisabled(r.Context(), id, disabled)
		if isNoRow(err) {
			return apierr.New(http.StatusNotFound, "no such person")
		}
		if err != nil {
			return fmt.Errorf("person %s: %w", id, err)
		}
		what, by := "enabled", adminOfRequest(r)
		if disabled {
			what = "disabled"
		}
		s.log.Info("person "+what, "person", id, "by", by)
		s.record(r.Context(), store.ActivityRow{Kind: "person_" + what, Category: "lifecycle",
			Name:   "person " + p.Email + " " + what + " by " + by,
			Detail: detail(map[string]any{"person": id.String(), "by": by})})
		return writeJSON(w, http.StatusOK, personViewOf(p))
	}
}
