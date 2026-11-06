// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/store"
)

// A tenant's admins are its people with admin on it who are not
// disabled and its tokens admin on it; members come with their grants;
// an invitation is used once, and not after it expired.
func TestMembersAndInvitations(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	stage := mkEnv(t, s, "acme", "stage")
	admins := func(want int) {
		t.Helper()
		if n, err := s.CountAdmins(bg, "acme"); err != nil || n != want {
			t.Fatalf("admins %d %v, want %d", n, err, want)
		}
	}
	admins(0)
	a, err := s.UpsertPerson(bg, "https://id", "a", "a@x", "Ann")
	must(t, err)
	b, err := s.UpsertPerson(bg, "https://id", "b", "b@x", "Bob")
	must(t, err)
	for _, g := range []store.Grant{{PersonID: a.ID, Role: "admin"}, {PersonID: b.ID, Role: "admin", EnvironmentID: stage.ID},
		{PersonID: b.ID, Role: "read"}} {
		g.TenantID = "acme"
		_, err := s.PutGrant(bg, g)
		must(t, err)
	}
	admins(1)
	_, err = s.InsertTenantToken(bg, store.TenantToken{Hash: "h-admin", TenantID: "acme", Role: "admin"})
	must(t, err)
	_, err = s.InsertTenantToken(bg, store.TenantToken{Hash: "h-stage", TenantID: "acme", Role: "admin",
		EnvironmentID: stage.ID})
	must(t, err)
	admins(2)
	_, err = s.PutGrant(bg, store.Grant{TenantID: "acme", TokenHash: "h-stage", Role: "admin"})
	must(t, err)
	admins(3)
	_, err = s.SetPersonDisabled(bg, a.ID, true)
	must(t, err)
	admins(2)

	ms, err := s.ListMembers(bg, "acme")
	must(t, err)
	if len(ms) != 2 || ms[0].Name != "Ann" || ms[1].Name != "Bob" || len(ms[1].Grants) != 2 ||
		ms[1].Grants[0].EnvironmentID != uuid.Nil || ms[1].Grants[1].EnvironmentName != "stage" {
		t.Fatalf("members %+v", ms)
	}
	if n, err := s.DeletePersonGrants(bg, "acme", b.ID); err != nil || n != 2 {
		t.Fatalf("delete %d %v", n, err)
	}
	if ok, err := s.IsMember(bg, "acme", b.ID); err != nil || ok {
		t.Fatalf("still a member %v %v", ok, err)
	}

	inv, err := s.InsertInvitation(bg, store.Invitation{Hash: "i1", TenantID: "acme", EnvironmentID: stage.ID,
		Role: "write", MadeBy: "person:" + a.ID.String()}, time.Hour)
	if err != nil || inv.EnvironmentName != "stage" || !inv.Open(time.Now()) {
		t.Fatalf("invitation %+v %v", inv, err)
	}
	if _, err := s.InsertInvitation(bg, store.Invitation{Hash: "i2", TenantID: "acme", EnvironmentID: uuid.New(),
		Role: "read", MadeBy: "x"}, time.Hour); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("foreign environment: %v", err)
	}
	used, err := s.UseInvitation(bg, "i1", b.ID)
	if err != nil || used.UsedBy != b.ID || used.UsedByName != "Bob" || used.Open(time.Now()) {
		t.Fatalf("use %+v %v", used, err)
	}
	if _, err := s.UseInvitation(bg, "i1", a.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("second use: %v", err)
	}
	_, err = s.InsertInvitation(bg, store.Invitation{Hash: "i3", TenantID: "acme", Role: "read", MadeBy: "x"}, time.Hour)
	must(t, err)
	exec(t, s, "UPDATE invitations SET expires_at = now() - interval '1 second' WHERE hash = 'i3'")
	if _, err := s.UseInvitation(bg, "i3", a.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("expired use: %v", err)
	}
	if all, err := s.ListInvitations(bg, "acme"); err != nil || len(all) != 2 || all[0].Open(time.Now()) ||
		all[1].Open(time.Now()) {
		t.Fatalf("invitations %+v %v", all, err)
	}

	// The tenants a person made.
	tn, created, err := s.MakeTenant(bg, "mine", store.NewTenant{MadeBy: a.ID})
	if err != nil || !created || tn.MadeBy != a.ID {
		t.Fatalf("insert %+v %v %v", tn, created, err)
	}
	if n, err := s.CountTenantsMadeBy(bg, a.ID); err != nil || n != 1 {
		t.Fatalf("made %d %v", n, err)
	}
	if _, created, err := s.MakeTenant(bg, "mine", store.NewTenant{}); err != nil || created {
		t.Fatalf("again %v %v", created, err)
	}
}
