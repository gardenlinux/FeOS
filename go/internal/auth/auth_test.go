// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"net/http"
	"testing"
)

// A read on a tenant's behalf passes on the caller's token, or its
// session's cookie and the tenant; a caller within the process has
// neither.
func TestPassOn(t *testing.T) {
	for _, c := range []struct {
		name string
		t    Tenant
		want http.Header
	}{
		{"a token", Tenant{Name: "acme", Token: "tok"}, http.Header{"Authorization": {"Bearer tok"}}},
		{"a session", Tenant{Name: "acme", Session: "s1"}, http.Header{"Cookie": {"__Host-ferun_session=s1; ferun_session=s1"},
			"Ferun-Tenant": {"acme"}}},
		{"nobody", Tenant{Name: "acme"}, http.Header{}},
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
