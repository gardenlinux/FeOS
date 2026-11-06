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

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/store"
)

// This file is ADR 0031's "People make tenants and invite each other":
// the tenants people make, a tenant's members and their grants,
// invitations, the tenant's tokens as its admins mint them, leaving a
// tenant, and the operators' switch of a person.

// The values of Config.TenantCreation (--tenant-creation).
const (
	TenantCreationPeople    = "people"
	TenantCreationOperators = "operators"
)

// invitationTTL is how long an invitation's link can be used.
const invitationTTL = 7 * 24 * time.Hour

// fixedReservedTenantNames are the names no tenant made by a person may
// have, beside Config.ReservedTenantNames.
var fixedReservedTenantNames = []string{store.OperatorTenant, "admin", "api", "www"}

// CodeLastAdmin is the code of a 409 for a change that would leave a
// tenant without an admin.
const CodeLastAdmin = "last_admin"

// CodeReservedName is the code of a 422 for a tenant name the platform
// keeps.
const CodeReservedName = "reserved_name"

// memberRoutes adds the routes of people in tenants.
func (s *server) memberRoutes(operator func(string, apiHandler), person func(string, apiHandler),
	tenant func(need, string, apiHandler)) {
	operator("GET /api/v1/admin/people", s.listPeople)
	operator("POST /api/v1/admin/people/{person}/disable", s.setPersonDisabled(true))
	operator("POST /api/v1/admin/people/{person}/enable", s.setPersonDisabled(false))

	person("POST /api/v1/tenants", s.createPersonTenant)
	person("GET /api/v1/invitations/{token}", s.previewInvitation)
	person("POST /api/v1/invitations/{token}/accept", s.acceptInvitation)

	// Members see each other in their tenant; a grant is changed by an
	// admin of its scope (the tenant, or the body's or query's
	// environment), which the handler checks.
	byScope := need{role: admin, scope: scopeHandler}
	tenant(member, "GET /api/v1/tenant/members", s.listMembers)
	tenant(byScope, "PUT /api/v1/tenant/members/{person}/grants", s.putMemberGrant)
	tenant(byScope, "DELETE /api/v1/tenant/members/{person}/grants", s.deleteMemberGrant)
	tenant(member, "POST /api/v1/tenant/leave", s.leaveTenant)

	tenant(member, "GET /api/v1/tenant/invitations", s.listInvitations)
	tenant(byScope, "POST /api/v1/tenant/invitations", s.createInvitation)
	tenant(byScope, "DELETE /api/v1/tenant/invitations/{id}", s.revokeInvitation)

	tenant(onTenant(admin), "GET /api/v1/tenant/tokens", s.listOwnTokens)
	tenant(onTenant(admin), "POST /api/v1/tenant/tokens", s.mintOwnToken)
	tenant(onTenant(admin), "DELETE /api/v1/tenant/tokens/{hash}", s.revokeOwnToken)
}

// ----------------------------------------------------------- person class --

type personKey struct{}

// person is the person class: a signed-in person's session (an
// operator's too) with a session's CSRF rules, in no tenant.
func (s *server) person(h apiHandler) http.Handler {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
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

// ---------------------------------------------------------------- tenants --

// reservedTenantName reports whether a person may not make a tenant of
// that name.
func (s *server) reservedTenantName(name string) bool {
	return slices.Contains(fixedReservedTenantNames, name) || slices.Contains(s.cfg.ReservedTenantNames, name)
}

// createPersonTenant is POST /api/v1/tenants {name}: a signed-in person
// makes a tenant and holds admin on it (ADR 0031). --tenant-creation
// operators keeps it to operators; --tenants-per-person bounds the
// tenants a person made that are not being deleted (operators are not
// bound). No token: the session acts.
func (s *server) createPersonTenant(w http.ResponseWriter, r *http.Request) error {
	sess := personSession(r)
	var req struct {
		Name string `json:"name"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if s.cfg.TenantCreation == TenantCreationOperators && !sess.Admin {
		return apierr.New(http.StatusForbidden, "on this platform only operators make tenants")
	}
	if !store.TenantNamePattern.MatchString(req.Name) {
		return apierr.New(http.StatusBadRequest, "tenant name must match %s", store.TenantNamePattern)
	}
	if s.reservedTenantName(req.Name) {
		return apierr.WithCode(http.StatusUnprocessableEntity, CodeReservedName,
			"the tenant name %s is reserved by the platform", req.Name)
	}
	by := sessionPrincipal(sess).String()
	var t store.Tenant
	err := s.st.InTx(r.Context(), func(tx *store.Store) error {
		if _, err := tx.LockPerson(r.Context(), sess.PersonID); err != nil {
			return err
		}
		if max := s.cfg.TenantsPerPerson; max > 0 && !sess.Admin {
			n, err := tx.CountTenantsMadeBy(r.Context(), sess.PersonID)
			if err != nil {
				return err
			}
			if n >= max {
				return apierr.QuotaExceeded("tenants", int64(max), int64(n), 1)
			}
		}
		var created bool
		var err error
		t, created, err = tx.InsertTenant(r.Context(), store.Tenant{ID: req.Name, MadeBy: sess.PersonID})
		if err != nil {
			return err
		}
		if !created {
			if t.Deleting() {
				return apierr.New(http.StatusConflict, "tenant %s is being deleted", req.Name)
			}
			return apierr.New(http.StatusConflict, "tenant %s already exists", req.Name)
		}
		_, err = tx.PutGrant(r.Context(), store.Grant{TenantID: t.ID, PersonID: sess.PersonID,
			Role: auth.RoleAdmin.String(), MadeBy: by})
		return err
	})
	if err != nil {
		return passOr(err, "create tenant")
	}
	s.log.Info("tenant created", "tenant", t.ID, "by", by)
	s.record(r.Context(), store.ActivityRow{TenantID: t.ID, Kind: "tenant_created", Category: "lifecycle",
		Name: "tenant " + t.ID + " created by " + by, Detail: detail(map[string]any{"tenant": t.ID, "by": by})})
	return writeJSON(w, http.StatusCreated, struct {
		Name      string `json:"name"`
		CreatedAt string `json:"createdAt"`
		Role      string `json:"role"`
	}{t.ID, rfc3339(t.CreatedAt), auth.RoleAdmin.String()})
}

// ---------------------------------------------------------------- members --

// grantView is a grant as the members' routes show it: the environment's
// ID and name, "" for the tenant.
type grantView struct {
	Role            string `json:"role"`
	Environment     string `json:"environment,omitempty"`
	EnvironmentName string `json:"environmentName,omitempty"`
}

func envString(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// memberView is a member: a person with their role on the tenant ("":
// none) and their roles on environments.
type memberView struct {
	Person       string        `json:"person"`
	Name         string        `json:"name"`
	Email        string        `json:"email"`
	Disabled     bool          `json:"disabled,omitempty"`
	Role         string        `json:"role"`
	Environments []envRoleView `json:"environments"`
}

// envRoleView is a role on an environment.
type envRoleView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// listMembers is GET /api/v1/tenant/members: the people who hold a grant
// in the tenant, with their names and e-mail addresses, which only the
// tenant's members see (ADR 0031). Of the grants on environments, those
// the principal may not read are left out.
func (s *server) listMembers(w http.ResponseWriter, r *http.Request) error {
	ms, err := s.st.ListMembers(r.Context(), tenantOf(r))
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}
	out := struct {
		Members []memberView `json:"members"`
	}{Members: make([]memberView, 0, len(ms))}
	for _, m := range ms {
		mv := memberView{Person: m.ID.String(), Name: m.Name, Email: m.Email, Disabled: m.Disabled(),
			Environments: []envRoleView{}}
		for _, g := range m.Grants {
			switch {
			case g.EnvironmentID == uuid.Nil:
				mv.Role = g.Role
			case mayReadEnv(r, g.EnvironmentID.String()):
				mv.Environments = append(mv.Environments, envRoleView{ID: g.EnvironmentID.String(),
					Name: g.EnvironmentName, Role: g.Role})
			}
		}
		out.Members = append(out.Members, mv)
	}
	return writeJSON(w, http.StatusOK, out)
}

func errNoMember() error { return apierr.New(http.StatusNotFound, "no such member") }

// adminScope resolves a grant's or an invitation's scope, the
// environment ref names (its ID or name; "": the tenant), and checks that
// the principal is admin of it: on the tenant 403 role_required; on an
// environment it does not see 404, one it holds a lower role on 403.
func (s *server) adminScope(ctx context.Context, t auth.Tenant, ref string) (uuid.UUID, error) {
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

// memberOf reads the path's {person}, who must be a member of the tenant.
func memberOf(ctx context.Context, tx *store.Store, r *http.Request) (uuid.UUID, error) {
	person, err := uuid.Parse(r.PathValue("person"))
	if err != nil {
		return uuid.Nil, errNoMember()
	}
	ok, err := tx.IsMember(ctx, tenantOf(r), person)
	if err != nil {
		return uuid.Nil, err
	}
	if !ok {
		return uuid.Nil, errNoMember()
	}
	return person, nil
}

// keepAdmin runs change with the tenant's row FOR NO KEY UPDATE, which
// serialises the changes of its admins, and refuses (409 last_admin) a
// change that leaves a tenant that had an admin without one: a tenant
// always has an admin (ADR 0031). A person who is not disabled or a
// token, with admin on the tenant itself, is one.
func keepAdmin(ctx context.Context, tx *store.Store, tenant string, change func() error) error {
	t, err := tx.LockTenant(ctx, tenant, store.ForNoKeyUpdate)
	if isNoRow(err) {
		return errNoTenant()
	}
	if err != nil {
		return err
	}
	if t.Deleting() {
		return apierr.New(http.StatusConflict, "tenant %s is being deleted", tenant)
	}
	before, err := tx.CountAdmins(ctx, tenant)
	if err != nil {
		return err
	}
	if err := change(); err != nil {
		return err
	}
	after, err := tx.CountAdmins(ctx, tenant)
	if err != nil {
		return err
	}
	if before > 0 && after == 0 {
		return apierr.WithCode(http.StatusConflict, CodeLastAdmin,
			"tenant %s would have no admin left: make another member admin first", tenant)
	}
	return nil
}

// putMemberGrant is PUT /api/v1/tenant/members/{person}/grants {role,
// environment?}: the member's role on the tenant, or on the environment
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
	t, _ := auth.TenantFrom(ctx)
	env, err := s.adminScope(ctx, t, req.Environment)
	if err != nil {
		return err
	}
	if _, ok := auth.ParseRole(req.Role); !ok {
		return apierr.New(http.StatusBadRequest, "role must be read, write or admin, got %q", req.Role)
	}
	var g store.Grant
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		return keepAdmin(ctx, tx, tenantOf(r), func() error {
			person, err := memberOf(ctx, tx, r)
			if err != nil {
				return err
			}
			g, err = tx.PutGrant(ctx, store.Grant{TenantID: tenantOf(r), PersonID: person, EnvironmentID: env,
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
	}{g.PersonID.String(), grantView{Role: g.Role, Environment: envString(g.EnvironmentID)}})
}

// deleteMemberGrant is DELETE /api/v1/tenant/members/{person}/grants
// [?environment=<ID or name>]: the member's grant on the tenant, or on
// the environment, goes, by an admin of that scope, unless it is the
// tenant's last admin's. Without grants left, they are no member.
func (s *server) deleteMemberGrant(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	t, _ := auth.TenantFrom(ctx)
	env, err := s.adminScope(ctx, t, r.URL.Query().Get("environment"))
	if err != nil {
		return err
	}
	var deleted bool
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		return keepAdmin(ctx, tx, tenantOf(r), func() error {
			person, err := memberOf(ctx, tx, r)
			if err != nil {
				return err
			}
			deleted, err = tx.DeletePersonGrantAt(ctx, tenantOf(r), person, env)
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

// leaveTenant is POST /api/v1/tenant/leave: a person gives up every grant
// of theirs in the tenant, unless they are its last admin. A token does
// not leave: it is revoked.
func (s *server) leaveTenant(w http.ResponseWriter, r *http.Request) error {
	t, _ := auth.TenantFrom(r.Context())
	if t.Principal.Person == uuid.Nil {
		return apierr.New(http.StatusBadRequest, "a token does not leave its tenant: revoke it")
	}
	ctx := r.Context()
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
	v := invitationView{ID: i.Hash, grantView: grantView{Role: i.Role, Environment: envString(i.EnvironmentID),
		EnvironmentName: i.EnvironmentName}, MadeBy: i.MadeBy, CreatedAt: rfc3339(i.CreatedAt),
		ExpiresAt: rfc3339(i.ExpiresAt), UsedAt: rfc3339(i.UsedAt), UsedByName: i.UsedByName}
	if i.UsedBy != uuid.Nil {
		v.UsedBy = i.UsedBy.String()
	}
	return v
}

// inviteURL is the UI's page of an invitation, <public URL>/invite/<token>.
func (s *server) inviteURL(token string) string { return s.cfg.PublicURL + "/invite/" + token }

// createInvitation is POST /api/v1/tenant/invitations {role,
// environment?}: a single-use link that gives whoever opens it signed in
// the role on the tenant or the environment (its ID or name), valid for 7
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
	ctx, tenant := r.Context(), tenantOf(r)
	t, _ := auth.TenantFrom(ctx)
	env, err := s.adminScope(ctx, t, req.Environment)
	if err != nil {
		return err
	}
	if _, ok := auth.ParseRole(req.Role); !ok {
		return apierr.New(http.StatusBadRequest, "role must be read, write or admin, got %q", req.Role)
	}
	secret, hash, err := mintTenantToken()
	if err != nil {
		return fmt.Errorf("invitation: %w", err)
	}
	var inv store.Invitation
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if err := lockTenantForToken(ctx, tx, tenant); err != nil {
			return err
		}
		var err error
		inv, err = tx.InsertInvitation(ctx, store.Invitation{Hash: hash, TenantID: tenant, EnvironmentID: env,
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

// mayAdminScope checks admin on an invitation's scope: the tenant, or its
// environment (absent when the principal holds no role there).
func mayAdminScope(t auth.Tenant, env uuid.UUID) error {
	if env == uuid.Nil {
		if t.MayOnTenant(auth.RoleAdmin) {
			return nil
		}
		return apierr.RoleRequired(auth.RoleAdmin.String(), "tenant")
	}
	if t.Principal.IsZero() {
		return nil
	}
	return allowEnvID(t, env, auth.RoleAdmin)
}

// listInvitations is GET /api/v1/tenant/invitations: the tenant's
// invitations, open, used and expired, whose scope the principal is
// admin of.
func (s *server) listInvitations(w http.ResponseWriter, r *http.Request) error {
	invs, err := s.st.ListInvitations(r.Context(), tenantOf(r))
	if err != nil {
		return fmt.Errorf("list invitations: %w", err)
	}
	t, _ := auth.TenantFrom(r.Context())
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

// revokeInvitation is DELETE /api/v1/tenant/invitations/{id}, by an admin of its
// scope; an invitation the principal cannot see is absent.
func (s *server) revokeInvitation(w http.ResponseWriter, r *http.Request) error {
	inv, err := s.st.GetInvitation(r.Context(), r.PathValue("id"))
	if isNoRow(err) || (err == nil && inv.TenantID != tenantOf(r)) {
		return errNoInvitation()
	}
	if err != nil {
		return fmt.Errorf("invitation: %w", err)
	}
	t, _ := auth.TenantFrom(r.Context())
	if err := mayAdminScope(t, inv.EnvironmentID); err != nil {
		if inv.EnvironmentID != uuid.Nil && t.EnvRole(inv.EnvironmentID) == auth.RoleNone {
			return errNoInvitation()
		}
		return err
	}
	ok, err := s.st.DeleteInvitation(r.Context(), inv.TenantID, inv.Hash)
	if err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	return writeJSON(w, http.StatusOK, struct {
		Revoked bool `json:"revoked"`
	}{ok})
}

// previewInvitation is GET /api/v1/invitations/{token}: what an
// invitation gives, to the signed-in person who holds its link, while it
// is open or theirs: {tenant, role, environment?, environmentName?,
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
	t, err := s.st.GetTenant(r.Context(), inv.TenantID)
	if err != nil && !isNoRow(err) {
		return fmt.Errorf("invitation: %w", err)
	}
	if isNoRow(err) || t.Deleting() || (inv.UsedBy != sess.PersonID && !inv.Open(time.Now())) {
		return errNoInvitation()
	}
	return writeJSON(w, http.StatusOK, struct {
		Tenant string `json:"tenant"`
		grantView
		ExpiresAt string `json:"expiresAt"`
	}{inv.TenantID, grantView{Role: inv.Role, Environment: envString(inv.EnvironmentID),
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
		t, err := tx.LockTenant(ctx, inv.TenantID, store.ForShare)
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
				g, err = tx.PersonGrantAt(ctx, cur.TenantID, sess.PersonID, cur.EnvironmentID)
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
		if have, err := tx.PersonGrantAt(ctx, used.TenantID, sess.PersonID, used.EnvironmentID); err == nil {
			a, _ := auth.ParseRole(have.Role)
			b, _ := auth.ParseRole(used.Role)
			role = max(a, b).String()
		} else if !isNoRow(err) {
			return err
		}
		g, err = tx.PutGrant(ctx, store.Grant{TenantID: used.TenantID, PersonID: sess.PersonID,
			EnvironmentID: used.EnvironmentID, Role: role, MadeBy: used.MadeBy})
		if isNoRow(err) {
			return errNoInvitation()
		}
		return err
	})
	if err != nil {
		return passOr(err, "accept invitation")
	}
	s.log.Info("invitation accepted", "tenant", inv.TenantID, "person", sess.PersonID, "role", g.Role,
		"environment", envString(g.EnvironmentID))
	return writeJSON(w, http.StatusOK, struct {
		Tenant string `json:"tenant"`
		grantView
	}{inv.TenantID, grantView{Role: g.Role, Environment: envString(g.EnvironmentID),
		EnvironmentName: inv.EnvironmentName}})
}

// ----------------------------------------------------------------- tokens --

// listOwnTokens is GET /api/v1/tenant/tokens: the tenant's tokens, by its admins.
func (s *server) listOwnTokens(w http.ResponseWriter, r *http.Request) error {
	toks, err := s.st.ListTenantTokens(r.Context(), tenantOf(r))
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	out := struct {
		Tokens []tenantTokenInfo `json:"tokens"`
	}{Tokens: make([]tenantTokenInfo, 0, len(toks))}
	for _, t := range toks {
		out.Tokens = append(out.Tokens, tenantTokenOf(t))
	}
	return writeJSON(w, http.StatusOK, out)
}

// mintOwnToken is POST /api/v1/tenant/tokens {label?, role, environment?}: a
// token of the tenant with its own grant, by an admin of the tenant, never
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
	t, _ := auth.TenantFrom(ctx)
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
	case env == uuid.Nil && !t.MayOnTenant(role):
		return apierr.RoleRequired(role.String(), "tenant")
	case env != uuid.Nil && !t.Principal.IsZero() && t.EnvRole(env) < role:
		return apierr.RoleRequired(role.String(), "environment")
	}
	token, hash, err := mintTenantToken()
	if err != nil {
		return fmt.Errorf("create token: %w", err)
	}
	var tok store.TenantToken
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if err := lockTenantForToken(ctx, tx, t.Name); err != nil {
			return err
		}
		var err error
		tok, err = tx.InsertTenantToken(ctx, store.TenantToken{Hash: hash, TenantID: t.Name, Label: req.Label,
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
		tenantTokenInfo
		Token string `json:"token"`
	}{tenantTokenOf(tok), token})
}

// revokeOwnToken is DELETE /api/v1/tenant/tokens/{hash}, by an admin of the
// tenant, unless the token is its last admin.
func (s *server) revokeOwnToken(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var ok bool
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		return keepAdmin(ctx, tx, tenantOf(r), func() error {
			var err error
			ok, err = tx.DeleteTenantToken(ctx, tenantOf(r), r.PathValue("hash"))
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
