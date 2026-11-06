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
		{"a session", Tenant{Name: "acme", Session: "s1"}, http.Header{"Cookie": {"ferun_session=s1"}, "Ferun-Tenant": {"acme"}}},
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
