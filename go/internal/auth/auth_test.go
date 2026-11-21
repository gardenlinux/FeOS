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
