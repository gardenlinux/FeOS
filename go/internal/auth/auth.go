// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package auth carries who a request is from through its context. The REST
// router authenticates a request by the credential its route takes (a
// sandbox's token, a tenant's token, or an operator's password or sign-in
// session) and puts the result here; the handlers of the other packages
// read it and never look at the credential themselves.
package auth

import (
	"context"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// Sandbox is a request made with a sandbox's token: the token is the
// sandbox.
type Sandbox struct {
	ID podid.ID
	// The tenant that owns the sandbox.
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

// Principal is who acts in a tenant: a person (their session) or a
// tenant token. An operator acting by Basic credentials is neither and
// names its user. The zero value is nobody: ferund itself, or a caller
// within the process.
type Principal struct {
	// Person is the person's ID (people.id); uuid.Nil for a token.
	Person uuid.UUID
	// TokenHash is the tenant token's hash; "" for a person.
	TokenHash string
	// Basic is the --admin-users user of an operator by Basic.
	Basic string
	// Operator: the principal is an operator (an admin session), with the
	// rights of an admin in every tenant.
	Operator bool
}

// IsZero reports the zero principal.
func (p Principal) IsZero() bool { return p.Person == uuid.Nil && p.TokenHash == "" && p.Basic == "" }

// String is the principal as made_by columns record it:
// "person:<id>", "token:<hash>", "basic:<user>", or "" for nobody.
func (p Principal) String() string {
	switch {
	case p.Person != uuid.Nil:
		return "person:" + p.Person.String()
	case p.TokenHash != "":
		return "token:" + p.TokenHash
	case p.Basic != "":
		return "basic:" + p.Basic
	}
	return ""
}

// Tenant is a request made in a tenant: with a tenant's token, or with a
// person's session that names the tenant (FeRun-Tenant).
type Tenant struct {
	Name string
	// Token is the bearer the request came with: a read on the tenant's
	// behalf passes it on (the sink, ADR 0026), and nothing keeps it. ""
	// for a session.
	Token string
	// Principal is who acts; the zero value for a caller that is not a
	// request (and then no role check applies).
	Principal Principal
	// Role is the principal's tenant grant (an operator's: admin), and
	// Envs its grants on environments, by environment ID.
	Role Role
	Envs map[uuid.UUID]Role
}

// EnvRole is the principal's role on an environment: the higher of its
// tenant grant and its grant on the environment.
func (t Tenant) EnvRole(env uuid.UUID) Role { return max(t.Role, t.Envs[env]) }

// MayOnTenant reports whether the principal holds role on the tenant
// itself; a caller without a principal (not a request) may.
func (t Tenant) MayOnTenant(role Role) bool { return t.Principal.IsZero() || t.Role >= role }

// MadeBy is the principal of a context's tenant request as made_by
// records it; "" without one.
func MadeBy(ctx context.Context) string {
	t, _ := TenantFrom(ctx)
	return t.Principal.String()
}

// Admin is a request made by an operator: with a password of
// --admin-users, or with the session of a person who signed in through
// OIDC and is in the admin group.
type Admin struct {
	// User is who acted, as the admin views record it (join tokens'
	// created_by, tenant creation, node removal): the Basic user, or the
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

type sandboxKey struct{}
type tenantKey struct{}
type adminKey struct{}

func WithSandbox(ctx context.Context, s Sandbox) context.Context {
	return context.WithValue(ctx, sandboxKey{}, s)
}

// SandboxFrom returns the sandbox a request was made as, if it was.
func SandboxFrom(ctx context.Context) (Sandbox, bool) {
	s, ok := ctx.Value(sandboxKey{}).(Sandbox)
	return s, ok
}

func WithTenant(ctx context.Context, t Tenant) context.Context {
	return context.WithValue(ctx, tenantKey{}, t)
}

// TenantFrom returns the tenant a request was made as, if it was.
func TenantFrom(ctx context.Context) (Tenant, bool) {
	t, ok := ctx.Value(tenantKey{}).(Tenant)
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
