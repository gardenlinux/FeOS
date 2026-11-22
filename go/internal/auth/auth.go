// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package auth carries who a request is from through its context. The REST
// router authenticates a request by the credential its route takes (a
// sandbox's token, a project's token, a platform service's token, or an
// operator's password or sign-in session) and puts the result here; the handlers of the other packages
// read it and never look at the credential themselves.
package auth

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// Sandbox is a request made with a sandbox's token: the token is the
// sandbox.
type Sandbox struct {
	ID podid.ID
	// The project that owns the sandbox.
	Owner string
	// The SHA-256 of the token, hex: what identifies the caller across
	// requests, for example an MCP call to cancel.
	TokenHash string
}

// Role is a grant's role (ADR 0031): read, write or admin, each including
// the one before. RoleNone is no role.
type Role int

const (
	RoleNone Role = iota
	RoleRead
	RoleWrite
	RoleAdmin
)

// String is the role's word, as grants and answers write it ("" for none).
func (r Role) String() string {
	switch r {
	case RoleRead:
		return "read"
	case RoleWrite:
		return "write"
	case RoleAdmin:
		return "admin"
	}
	return ""
}

// ParseRole reads a role's word; ok false for anything else.
func ParseRole(s string) (Role, bool) {
	switch s {
	case "read":
		return RoleRead, true
	case "write":
		return RoleWrite, true
	case "admin":
		return RoleAdmin, true
	}
	return RoleNone, false
}

// Principal is who acts in a project: a person (their session), a
// project token, or an integration (ADR 0056), which may name the person
// it acts for. An operator acting by Basic credentials is none of them
// and names its user. The zero value is nobody: ferund itself, or a
// caller within the process.
type Principal struct {
	// Person is the person's ID (people.id); uuid.Nil for a token and for
	// an integration.
	Person uuid.UUID
	// TokenHash is the project token's hash; "" for a person.
	TokenHash string
	// Basic is the --admin-users user of an operator by Basic.
	Basic string
	// Operator: the principal is an operator (an admin session), with the
	// rights of an admin in every project. An integration never is one.
	Operator bool
	// Integration is the integration's ID (integrations.id); uuid.Nil for
	// every other principal.
	Integration uuid.UUID
	// For is the person an integration names for the request (people.id),
	// whose grants bound what it does; uuid.Nil when it acts on its own,
	// and for every other principal.
	For uuid.UUID
}

// IsZero reports the zero principal.
func (p Principal) IsZero() bool {
	return p.Person == uuid.Nil && p.TokenHash == "" && p.Basic == "" && p.Integration == uuid.Nil
}

// IsIntegration reports whether the principal is an integration, with a
// person named or without.
func (p Principal) IsIntegration() bool { return p.Integration != uuid.Nil }

// String is the principal as made_by columns record it:
// "person:<id>", "token:<hash>", "basic:<user>", "integration:<id>",
// "integration:<id>/person:<id>" for an integration that names a person,
// or "" for nobody.
func (p Principal) String() string {
	switch {
	case p.Integration != uuid.Nil && p.For != uuid.Nil:
		return "integration:" + p.Integration.String() + "/person:" + p.For.String()
	case p.Integration != uuid.Nil:
		return "integration:" + p.Integration.String()
	case p.Person != uuid.Nil:
		return "person:" + p.Person.String()
	case p.TokenHash != "":
		return "token:" + p.TokenHash
	case p.Basic != "":
		return "basic:" + p.Basic
	}
	return ""
}

// SessionCookie is the cookie of a person's session, and ProjectHeader
// the header in which a session names its project (ADR 0031). The sink
// takes both too and asks ferund with them (ADR 0026). The cookie is
// __Host- (Secure, Path=/, no Domain): the projects' apps live in zones
// below ferund's host (apps.run.<domain>), and the prefix keeps the
// browser from taking a cookie for ferund's host that one of them set.
//
// OldSessionCookie is its name before (ferun_session), still read and
// passed on for one release, so that the sessions of the release before
// survive its rollout and a sink or ferund of that release still finds
// the session: drop it, SessionOf's fallback and SessionCookieHeader's
// second cookie in the release after.
const (
	SessionCookie    = "__Host-ferun_session"
	OldSessionCookie = "ferun_session"
	ProjectHeader    = "FeRun-Project"
)

// SessionOf is the value of r's session cookie, SessionCookie or else
// OldSessionCookie; "" when it has neither.
func SessionOf(r *http.Request) string {
	for _, name := range []string{SessionCookie, OldSessionCookie} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// SessionCookieHeader is a Cookie header that passes the session on, under
// both names for the release that reads both (OldSessionCookie).
func SessionCookieHeader(session string) string {
	return (&http.Cookie{Name: SessionCookie, Value: session}).String() + "; " +
		(&http.Cookie{Name: OldSessionCookie, Value: session}).String()
}

// PersonHeader is the header in which an integration names the person a
// call is made for (ADR 0056): their subject at the integration's issuer.
// Only an integration sends it.
const PersonHeader = "FeRun-Person"

// Grants are one principal's roles in a project: its grant on the project
// (RoleNone: none) and its grants on environments, by environment ID.
type Grants struct {
	Project Role
	Envs    map[uuid.UUID]Role
}

// counted is what a project grant g counts for on an environment that is
// protected or not (ADR 0060): admin as admin, write and read as read on a
// protected one.
func counted(g Role, protected bool) Role {
	if protected && g != RoleAdmin {
		return min(g, RoleRead)
	}
	return g
}

// On is the role on env by ADR 0060: the higher of what the project grant
// counts for there and the grant on env.
func (g Grants) On(env uuid.UUID, protected bool) Role {
	return max(counted(g.Project, protected), g.Envs[env])
}

// Plan is the role on env were it not protected: the higher of the
// project grant and the grant on env.
func (g Grants) Plan(env uuid.UUID) Role { return max(g.Project, g.Envs[env]) }

// Project is a request made in a project: with a project's token, with a
// person's session that names the project (FeRun-Project), or with an
// integration's bearer that names it, and maybe the person the call is
// made for (FeRun-Person).
type Project struct {
	Name string
	// Token is the bearer the request came with: a read on the project's
	// behalf passes it on (the sink, ADR 0026), and nothing keeps it. ""
	// for a session.
	Token string
	// Session is the session cookie's value of a session's request,
	// passed on as Token is; "" for a token.
	Session string
	// Principal is who acts; the zero value for a caller that is not a
	// request (and then no role check applies).
	Principal Principal
	// Role is the principal's own project grant (an operator's: admin),
	// and Envs its own grants on environments, by environment ID. For an
	// integration they are the integration's.
	Role Role
	Envs map[uuid.UUID]Role
	// Named is the person an integration names (Principal.For), with their
	// grants in the project; nil for every other request.
	Named *NamedPerson
	// Protected are the project's protected environments (ADR 0060).
	Protected map[uuid.UUID]bool
}

// NamedPerson is the person an integration names for a call: the subject
// it sent, and the person's own grants (an operator's everywhere-admin is
// a session's and never comes through an integration).
type NamedPerson struct {
	Sub string
	Grants
}

// own is the principal's own grants.
func (t Project) own() Grants { return Grants{Project: t.Role, Envs: t.Envs} }

// alone reports an integration that names nobody: it changes no protected
// environment (ADR 0060).
func (t Project) alone() bool { return t.Principal.IsIntegration() && t.Named == nil }

// ProjectRole is the role on the project itself (the contract's §2.2): the
// principal's project grant, for an integration naming a person the lower
// of its and the person's.
func (t Project) ProjectRole() Role {
	if t.Named != nil {
		return min(t.Role, t.Named.Project)
	}
	return t.Role
}

// EnvRole is the role on an environment (ADR 0031, 0056, 0060): for each
// side (the principal; for an integration naming a person, it and the
// person) the higher of what its project grant counts for there, admin as
// admin and below it read on a protected environment, and its grant on the
// environment; the lower of the two sides; and read at most on a
// protected environment for an integration that names nobody.
func (t Project) EnvRole(env uuid.UUID) Role {
	p := t.Protected[env]
	r := t.own().On(env, p)
	if t.Named != nil {
		r = min(r, t.Named.On(env, p))
	}
	if p && t.alone() {
		r = min(r, RoleRead)
	}
	return r
}

// PlanRole is the role on an environment were it not protected: what a
// plan against it needs (ADR 0060), the lower of both sides with a person.
func (t Project) PlanRole(env uuid.UUID) Role {
	r := t.own().Plan(env)
	if t.Named != nil {
		r = min(r, t.Named.Plan(env))
	}
	return r
}

// OwnEnvRole is the principal's own role on an environment by ADR 0060,
// without the person it names and without the cap of an integration that
// names nobody: an integration's integrationRole.
func (t Project) OwnEnvRole(env uuid.UUID) Role { return t.own().On(env, t.Protected[env]) }

// PersonEnvRole is the named person's role on an environment by ADR 0060;
// RoleNone without a person.
func (t Project) PersonEnvRole(env uuid.UUID) Role {
	if t.Named == nil {
		return RoleNone
	}
	return t.Named.On(env, t.Protected[env])
}

// MayDelete reports whether the principal may delete and rename an
// environment: admin on it when it is protected, else admin on it or
// write on the project.
func (t Project) MayDelete(env uuid.UUID) bool {
	if t.Principal.IsZero() {
		return true
	}
	if t.EnvRole(env) >= RoleAdmin {
		return true
	}
	return !t.Protected[env] && t.ProjectRole() >= RoleWrite
}

// MayDeleteProject reports whether the principal may delete the project
// (ADR 0031, 0056): its role on the project is admin (an operator's
// session is), it is no project token, an integration names a person (as
// admin itself, both sides admin), and it may delete each of envs
// (MayDelete), the project's environments, since the project's deletion
// deletes every one of them, a protected one too. The DELETE route and
// /auth/me's delete both ask it.
func (t Project) MayDeleteProject(envs []uuid.UUID) bool {
	if t.Principal.IsZero() {
		return true
	}
	if t.Principal.TokenHash != "" || t.alone() || t.ProjectRole() < RoleAdmin {
		return false
	}
	for _, id := range envs {
		if !t.MayDelete(id) {
			return false
		}
	}
	return true
}

// EveryEnv reports whether the principal holds role on every environment
// of the project by its project role alone, so that no environment needs
// to be looked at: always below write, and from write on unless an
// environment is protected and a side's project grant is below admin there
// (or the principal is an integration that names nobody).
func (t Project) EveryEnv(role Role) bool {
	if t.Principal.IsZero() {
		return true
	}
	if t.ProjectRole() < role {
		return false
	}
	if role <= RoleRead || len(t.Protected) == 0 {
		return true
	}
	return !t.alone() && t.Role == RoleAdmin && (t.Named == nil || t.Named.Project == RoleAdmin)
}

// EnvGrants are the environments either side holds a grant on: those
// beside the project role a principal may see.
func (t Project) EnvGrants() []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	add := func(m map[uuid.UUID]Role) {
		for id := range m {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	add(t.Envs)
	if t.Named != nil {
		add(t.Named.Envs)
	}
	return out
}

// Visible reports whether the project is the principal's to see (the
// contract's §2.2): a project role, or a role on one of its environments.
func (t Project) Visible() bool {
	if t.ProjectRole() > RoleNone {
		return true
	}
	for _, id := range t.EnvGrants() {
		if t.EnvRole(id) >= RoleRead {
			return true
		}
	}
	return false
}

// Passable reports whether the request has credentials to pass on
// (PassOn): a token, or a session; a caller within the process has none.
func (t Project) Passable() bool { return t.Token != "" || (t.Session != "" && t.Name != "") }

// PassOn sets on h the credentials of a request made on the project's
// behalf (a read of the sink, ADR 0026): the caller's token, or its
// session's cookie and the project in FeRun-Project, so that the one asked
// answers what the caller may read, as ferund's GET /api/v1/project tells
// it; an integration's bearer with the project, and the person it names
// in FeRun-Person. Without credentials (Passable) it sets nothing.
func (t Project) PassOn(h http.Header) {
	switch {
	case t.Token != "" && t.Principal.IsIntegration():
		h.Set("Authorization", "Bearer "+t.Token)
		h.Set(ProjectHeader, t.Name)
		if t.Named != nil {
			h.Set(PersonHeader, t.Named.Sub)
		}
	case t.Token != "":
		h.Set("Authorization", "Bearer "+t.Token)
	case t.Passable():
		h.Set("Cookie", SessionCookieHeader(t.Session))
		h.Set(ProjectHeader, t.Name)
	}
}

// MayOnProject reports whether the principal holds role on the project
// itself; a caller without a principal (not a request) may.
func (t Project) MayOnProject(role Role) bool { return t.Principal.IsZero() || t.ProjectRole() >= role }

// MadeBy is the principal of a context's project request as made_by
// records it; "" without one.
func MadeBy(ctx context.Context) string {
	t, _ := ProjectFrom(ctx)
	return t.Principal.String()
}

// IsIntegration reports whether a context's project request is made by
// an integration (ADR 0056); false without a project request.
func IsIntegration(ctx context.Context) bool {
	t, _ := ProjectFrom(ctx)
	return t.Principal.IsIntegration()
}

// Admin is a request made by an operator: with a password of
// --admin-users, or with the session of a person who signed in through
// OIDC and is in the admin group.
type Admin struct {
	// User is who acted, as the admin views record it (join tokens'
	// created_by, project creation, node removal): the Basic user, or the
	// signed-in person's username, else their email.
	User string
	// Sub is the OIDC subject of a signed-in person; "" for Basic.
	Sub string
	// Method is how the operator authenticated: AdminBasic or AdminOIDC.
	Method string
	// Person is the signed-in person (people.id); uuid.Nil for Basic and
	// for a session of a version before people.
	Person uuid.UUID
}

// Principal is the operator as a principal: their person, or the Basic
// user.
func (a Admin) Principal() Principal {
	if a.Person != uuid.Nil {
		return Principal{Person: a.Person, Operator: true}
	}
	return Principal{Basic: a.User, Operator: true}
}

// The methods an operator authenticates with.
const (
	AdminBasic = "basic"
	AdminOIDC  = "oidc"
)

// Service is a request made with a platform service's token
// (service_tokens): ferun-auth's, of kind auth, on the routes of the
// visits (ADR 0054).
type Service struct {
	// Kind is the token's kind (model.ServiceAuth), Name its holder's
	// name.
	Kind, Name string
	// ID is the token's row ID, which logs and answered_by name.
	ID uuid.UUID
	// TokenHash is the SHA-256 of the token, hex.
	TokenHash string
}

type sandboxKey struct{}
type projectKey struct{}
type adminKey struct{}
type serviceKey struct{}

func WithSandbox(ctx context.Context, s Sandbox) context.Context {
	return context.WithValue(ctx, sandboxKey{}, s)
}

// SandboxFrom returns the sandbox a request was made as, if it was.
func SandboxFrom(ctx context.Context) (Sandbox, bool) {
	s, ok := ctx.Value(sandboxKey{}).(Sandbox)
	return s, ok
}

func WithProject(ctx context.Context, t Project) context.Context {
	return context.WithValue(ctx, projectKey{}, t)
}

// ProjectFrom returns the project a request was made as, if it was.
func ProjectFrom(ctx context.Context) (Project, bool) {
	t, ok := ctx.Value(projectKey{}).(Project)
	return t, ok
}

func WithAdmin(ctx context.Context, a Admin) context.Context {
	return context.WithValue(ctx, adminKey{}, a)
}

// AdminFrom returns the operator a request was made as, if it was.
func AdminFrom(ctx context.Context) (Admin, bool) {
	a, ok := ctx.Value(adminKey{}).(Admin)
	return a, ok
}

func WithService(ctx context.Context, s Service) context.Context {
	return context.WithValue(ctx, serviceKey{}, s)
}

// ServiceFrom returns the platform service a request was made as, if it
// was.
func ServiceFrom(ctx context.Context) (Service, bool) {
	s, ok := ctx.Value(serviceKey{}).(Service)
	return s, ok
}
