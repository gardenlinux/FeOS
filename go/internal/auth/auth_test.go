// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// A read on a project's behalf passes on the caller's token, or its
// session's cookie and the project; a caller within the process has
// neither.
func TestPassOn(t *testing.T) {
	for _, c := range []struct {
		name string
		t    Project
		want http.Header
	}{
		{"a token", Project{Name: "acme", Token: "tok"}, http.Header{"Authorization": {"Bearer tok"}}},
		{"a session", Project{Name: "acme", Session: "s1"}, http.Header{"Cookie": {"__Host-ferun_session=s1; ferun_session=s1"},
			"Ferun-Project": {"acme"}}},
		{"nobody", Project{Name: "acme"}, http.Header{}},
		{"an integration", Project{Name: "acme", Token: "a.b.c", Principal: Principal{Integration: uuid.New()}},
			http.Header{"Authorization": {"Bearer a.b.c"}, "Ferun-Project": {"acme"}}},
		{"an integration naming a person", Project{Name: "acme", Token: "a.b.c",
			Principal: Principal{Integration: uuid.New(), For: uuid.New()}, Named: &NamedPerson{Sub: "ada"}},
			http.Header{"Authorization": {"Bearer a.b.c"}, "Ferun-Project": {"acme"}, "Ferun-Person": {"ada"}}},
	} {
		h := http.Header{}
		c.t.PassOn(h)
		if c.t.Passable() != (len(c.want) > 0) || len(h) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.name, h, c.want)
		}
		for k, v := range c.want {
			if h.Get(k) != v[0] {
				t.Errorf("%s: %s %q, want %q", c.name, k, h.Get(k), v[0])
			}
		}
	}
}

// The session's cookie is read under its name, else under its old one
// (for one release); the new name wins.
func TestSessionOf(t *testing.T) {
	for _, c := range []struct {
		cookies []*http.Cookie
		want    string
	}{
		{[]*http.Cookie{{Name: SessionCookie, Value: "new"}}, "new"},
		{[]*http.Cookie{{Name: OldSessionCookie, Value: "old"}}, "old"},
		{[]*http.Cookie{{Name: OldSessionCookie, Value: "old"}, {Name: SessionCookie, Value: "new"}}, "new"},
		{[]*http.Cookie{{Name: SessionCookie, Value: ""}, {Name: OldSessionCookie, Value: "old"}}, "old"},
		{nil, ""},
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		for _, ck := range c.cookies {
			r.AddCookie(ck)
		}
		if got := SessionOf(r); got != c.want {
			t.Errorf("%v: %q, want %q", c.cookies, got, c.want)
		}
	}
	// What SessionCookieHeader sends, a request reads.
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", SessionCookieHeader("s1"))
	if a, err := r.Cookie(SessionCookie); err != nil || a.Value != "s1" {
		t.Errorf("new name: %v %v", a, err)
	}
	if b, err := r.Cookie(OldSessionCookie); err != nil || b.Value != "s1" {
		t.Errorf("old name: %v %v", b, err)
	}
}

// A principal is recorded as made_by says: an integration by its ID, and
// with the person it names after it; an integration is no zero principal,
// and only it is one.
func TestPrincipalString(t *testing.T) {
	in := uuid.MustParse("0199a0b1-0000-7000-8000-000000000001")
	p := uuid.MustParse("0199a0b1-0000-7000-8000-000000000002")
	for _, c := range []struct {
		name        string
		p           Principal
		want        string
		zero, isInt bool
	}{
		{"nobody", Principal{}, "", true, false},
		{"a person", Principal{Person: p}, "person:" + p.String(), false, false},
		{"an operator", Principal{Person: p, Operator: true}, "person:" + p.String(), false, false},
		{"a token", Principal{TokenHash: "ab12"}, "token:ab12", false, false},
		{"basic", Principal{Basic: "ops", Operator: true}, "basic:ops", false, false},
		{"an integration", Principal{Integration: in}, "integration:" + in.String(), false, true},
		{"an integration naming a person", Principal{Integration: in, For: p},
			"integration:" + in.String() + "/person:" + p.String(), false, true},
	} {
		if got := c.p.String(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if got := c.p.IsZero(); got != c.zero {
			t.Errorf("%s: IsZero %v", c.name, got)
		}
		if got := c.p.IsIntegration(); got != c.isInt {
			t.Errorf("%s: IsIntegration %v", c.name, got)
		}
		ctx := WithProject(context.Background(), Project{Name: "acme", Principal: c.p})
		if got := IsIntegration(ctx); got != c.isInt {
			t.Errorf("%s: IsIntegration(ctx) %v", c.name, got)
		}
		if got := MadeBy(ctx); got != c.want {
			t.Errorf("%s: MadeBy %q, want %q", c.name, got, c.want)
		}
	}
	if IsIntegration(context.Background()) {
		t.Error("IsIntegration without a project request")
	}
}

// The effective role (the contract's §2.2) for every combination of
// grants, protection and principal kind: what auth.Project answers is the
// table's rule, written out here once more; EveryEnv never claims a role
// an environment does not give; MayDelete is admin on a protected
// environment, admin or project write otherwise.
func TestEffectiveRoles(t *testing.T) {
	roles := []Role{RoleNone, RoleRead, RoleWrite, RoleAdmin}
	env := uuid.MustParse("0199a0b1-0000-7000-8000-0000000000e1")
	capped := func(g Role, p bool) Role {
		switch {
		case g == RoleAdmin || !p:
			return g
		case g == RoleNone:
			return RoleNone
		}
		return RoleRead
	}
	n := 0
	for _, kind := range []string{"person", "token", "integration", "integration naming"} {
		for _, gx := range roles {
			for _, ex := range roles {
				for _, gp := range roles {
					for _, ep := range roles {
						for _, prot := range []bool{false, true} {
							if kind != "integration naming" && (gp != RoleNone || ep != RoleNone) {
								continue
							}
							n++
							tp := Project{Name: "acme", Role: gx, Envs: map[uuid.UUID]Role{env: ex}}
							switch kind {
							case "person":
								tp.Principal = Principal{Person: uuid.New()}
							case "token":
								tp.Principal = Principal{TokenHash: "h"}
							case "integration":
								tp.Principal = Principal{Integration: uuid.New()}
							default:
								tp.Principal = Principal{Integration: uuid.New(), For: uuid.New()}
								tp.Named = &NamedPerson{Sub: "s", Grants: Grants{Project: gp, Envs: map[uuid.UUID]Role{env: ep}}}
							}
							if prot {
								tp.Protected = map[uuid.UUID]bool{env: true}
							}
							rx, px := max(capped(gx, prot), ex), max(gx, ex)
							wantRole, wantPlan, wantProject := rx, px, gx
							switch kind {
							case "integration":
								if prot {
									wantRole = min(wantRole, RoleRead)
								}
							case "integration naming":
								wantRole = min(rx, max(capped(gp, prot), ep))
								wantPlan = min(px, max(gp, ep))
								wantProject = min(gx, gp)
							}
							name := kind
							if got := tp.EnvRole(env); got != wantRole {
								t.Errorf("%s G=%v g=%v P=%v p=%v protected %v: role %v, want %v", name, gx, ex, gp, ep, prot, got, wantRole)
							}
							if got := tp.PlanRole(env); got != wantPlan {
								t.Errorf("%s G=%v g=%v P=%v p=%v protected %v: plan %v, want %v", name, gx, ex, gp, ep, prot, got, wantPlan)
							}
							if got := tp.ProjectRole(); got != wantProject {
								t.Errorf("%s: project %v, want %v", name, got, wantProject)
							}
							if got := tp.OwnEnvRole(env); got != rx {
								t.Errorf("%s: own %v, want %v", name, got, rx)
							}
							wantDelete := wantRole >= RoleAdmin || (!prot && wantProject >= RoleWrite)
							if got := tp.MayDelete(env); got != wantDelete {
								t.Errorf("%s G=%v g=%v P=%v p=%v protected %v: delete %v", name, gx, ex, gp, ep, prot, got)
							}
							for _, r := range roles[1:] {
								// Another environment, of no grant, protected too.
								other := uuid.New()
								if prot {
									tp.Protected[other] = true
								}
								if tp.EveryEnv(r) && (tp.EnvRole(env) < r || tp.EnvRole(other) < r) {
									t.Errorf("%s G=%v g=%v P=%v p=%v protected %v: EveryEnv(%v) but not on each", name, gx, ex, gp, ep,
										prot, r)
								}
							}
							if tp.Visible() != (wantProject > RoleNone || wantRole >= RoleRead) {
								t.Errorf("%s G=%v g=%v P=%v p=%v protected %v: visible %v", name, gx, ex, gp, ep, prot, tp.Visible())
							}
						}
					}
				}
			}
		}
	}
	if n < 500 {
		t.Fatalf("%d cases", n)
	}
}
