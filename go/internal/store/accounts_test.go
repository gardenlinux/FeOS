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

// An account is made once, changed, closed and reopened (who closed it
// kept while it is closed), and deleted only while no project was ever on
// it; its people come with their roles, a disabled one holding none.
func TestAccounts(t *testing.T) {
	s, _ := newStore(t)
	a, made, err := s.CreateAccount(bg, store.Account{ID: "cc-4711", Title: "Team X", Reference: "CC 4711",
		MadeBy: "basic:root"})
	must(t, err)
	if !made || a.Title != "Team X" || a.Reference != "CC 4711" || a.MadeBy != "basic:root" || a.Closed() {
		t.Fatalf("made %v %+v", made, a)
	}
	if _, made, err := s.CreateAccount(bg, store.Account{ID: "cc-4711", Title: "other"}); err != nil || made {
		t.Fatalf("again: %v %v", made, err)
	}
	if _, err := s.GetAccount(bg, "nope"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown: %v", err)
	}
	title, yes, no := "Team Y", true, false
	a, err = s.PatchAccount(bg, "cc-4711", store.AccountChange{Title: &title, Closed: &yes}, "person:x")
	must(t, err)
	if a.Title != "Team Y" || a.Reference != "CC 4711" || !a.Closed() || a.ClosedBy != "person:x" {
		t.Fatalf("closed %+v", a)
	}
	closedAt := a.ClosedAt
	a, err = s.PatchAccount(bg, "cc-4711", store.AccountChange{Closed: &yes}, "person:y")
	must(t, err)
	if !a.ClosedAt.Equal(closedAt) || a.ClosedBy != "person:x" {
		t.Fatalf("closed again %+v", a)
	}
	a, err = s.PatchAccount(bg, "cc-4711", store.AccountChange{Closed: &no}, "person:y")
	must(t, err)
	if a.Closed() || a.ClosedBy != "" {
		t.Fatalf("reopened %+v", a)
	}
	if _, err := s.PatchAccount(bg, "nope", store.AccountChange{}, ""); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("patch unknown: %v", err)
	}

	p, err := s.UpsertPerson(bg, "https://id", "p", "p@x", "Pia")
	must(t, err)
	q, err := s.UpsertPerson(bg, "https://id", "q", "q@x", "Quinn")
	must(t, err)
	must(t, s.PutAccountMember(bg, "cc-4711", p.ID, store.AccountAdmin, "basic:root"))
	must(t, s.PutAccountMember(bg, "cc-4711", q.ID, store.AccountMember, "person:"+p.ID.String()))
	if err := s.PutAccountMember(bg, "nope", q.ID, store.AccountMember, ""); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("member of no account: %v", err)
	}
	if err := s.PutAccountMember(bg, "cc-4711", uuid.New(), store.AccountMember, ""); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("no person: %v", err)
	}
	if n, err := s.CountAccountAdmins(bg, "cc-4711"); err != nil || n != 1 {
		t.Fatalf("admins %d %v", n, err)
	}
	ms, err := s.ListAccountMembers(bg, "cc-4711")
	must(t, err)
	if len(ms) != 2 || ms[0].Name != "Pia" || ms[0].Role != "admin" || ms[1].Name != "Quinn" || ms[1].Role != "member" {
		t.Fatalf("members %+v", ms)
	}
	if r, err := s.AccountRole(bg, "cc-4711", q.ID); err != nil || r != "member" {
		t.Fatalf("role %q %v", r, err)
	}
	if r, err := s.AccountRole(bg, store.DefaultAccount, q.ID); err != nil || r != "" {
		t.Fatalf("no row %q %v", r, err)
	}
	_, err = s.SetPersonDisabled(bg, p.ID, true)
	must(t, err)
	if r, err := s.AccountRole(bg, "cc-4711", p.ID); err != nil || r != "" {
		t.Fatalf("disabled %q %v", r, err)
	}
	if n, err := s.CountAccountAdmins(bg, "cc-4711"); err != nil || n != 0 {
		t.Fatalf("admins with one disabled %d %v", n, err)
	}
	pas, err := s.PersonAccounts(bg, q.ID)
	must(t, err)
	if len(pas) != 1 || pas[0].ID != "cc-4711" || pas[0].Role != "member" {
		t.Fatalf("person's accounts %+v", pas)
	}
	if ok, err := s.DeleteAccountMember(bg, "cc-4711", q.ID); err != nil || !ok {
		t.Fatalf("delete member %v %v", ok, err)
	}

	list, err := s.ListAccounts(bg)
	must(t, err)
	if len(list) != 2 || list[0].ID != "cc-4711" || list[0].Members != 1 || list[1].ID != store.DefaultAccount {
		t.Fatalf("accounts %+v", list)
	}

	mkProject(t, s, "acme")
	_, _, err = s.MakeProject(bg, "shop", store.NewProject{Account: "cc-4711", By: "basic:root"})
	must(t, err)
	if ok, err := s.DeleteAccount(bg, "cc-4711"); !errors.Is(err, store.ErrInUse) || ok {
		t.Fatalf("delete one in use: %v %v", ok, err)
	}
	_, _, err = s.CreateAccount(bg, store.Account{ID: "spare"})
	must(t, err)
	if ok, err := s.DeleteAccount(bg, "spare"); err != nil || !ok {
		t.Fatalf("delete spare: %v %v", ok, err)
	}
	info, err := s.GetAccountInfo(bg, store.DefaultAccount)
	must(t, err)
	if info.Projects != 1 {
		t.Fatalf("default holds %d", info.Projects)
	}
}

// A project is made on an account, from its creation, by its maker; one
// without a row is on default since it was made; a move leaves one
// account and takes the next at one moment, and to the same account
// writes nothing; the moves read by project and by account, newest first,
// and an account's projects come with maker and admins.
func TestProjectAccounts(t *testing.T) {
	s, _ := newStore(t)
	_, _, err := s.CreateAccount(bg, store.Account{ID: "cc-4711", Title: "Team X"})
	must(t, err)
	maker, err := s.UpsertPerson(bg, "https://id", "m", "m@x", "Mia")
	must(t, err)
	by := "person:" + maker.ID.String()
	shop, made, err := s.MakeProject(bg, "shop", store.NewProject{MadeBy: maker.ID, Account: "cc-4711", By: by})
	must(t, err)
	if !made {
		t.Fatal("not made")
	}
	if _, _, err := s.MakeProject(bg, "lost", store.NewProject{Account: "nope"}); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("on no account: %v", err)
	}
	if _, err := s.GetProject(bg, "lost"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("a project without its account: %v", err)
	}
	a, since, err := s.ProjectAccount(bg, "shop")
	must(t, err)
	if a.ID != "cc-4711" || !since.Equal(shop.CreatedAt) {
		t.Fatalf("shop on %s since %v", a.ID, since)
	}
	// A project of the release before, without a row.
	exec(t, s, "INSERT INTO projects (id, created_at) VALUES ('old', '2026-01-01T00:00:00Z')")
	a, since, err = s.ProjectAccount(bg, "old")
	must(t, err)
	if a.ID != store.DefaultAccount || !since.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("old on %s since %v", a.ID, since)
	}
	if _, _, err := s.ProjectAccount(bg, "nope"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown: %v", err)
	}
	accts, err := s.ProjectAccounts(bg, []string{"shop", "old", "nope"})
	must(t, err)
	if len(accts) != 2 || accts["shop"].Title != "Team X" || accts["old"].ID != store.DefaultAccount {
		t.Fatalf("accounts %+v", accts)
	}

	move := func(project, account string) (string, bool) {
		t.Helper()
		var from string
		var moved bool
		must(t, s.InTx(bg, func(tx *store.Store) error {
			var err error
			from, moved, err = tx.MoveProject(bg, project, account, "basic:root")
			return err
		}))
		return from, moved
	}
	if _, _, err := s.MoveProject(bg, "shop", "default", ""); err == nil {
		t.Fatal("a move outside a transaction")
	}
	if from, moved := move("shop", "cc-4711"); moved || from != "cc-4711" {
		t.Fatalf("to the same: %s %v", from, moved)
	}
	// One without a row, to default where it is: nothing written.
	if from, moved := move("old", store.DefaultAccount); moved || from != store.DefaultAccount {
		t.Fatalf("old to default: %s %v", from, moved)
	}
	var n int
	must(t, s.Q().QueryRow(bg, "SELECT count(*) FROM project_accounts WHERE project_id = 'old'").Scan(&n))
	if n != 0 {
		t.Fatalf("old has %d rows", n)
	}
	for _, to := range []string{store.DefaultAccount, "cc-4711"} {
		if err := s.InTx(bg, func(tx *store.Store) error {
			_, _, err := tx.MoveProject(bg, "nope", to, "")
			return err
		}); !errors.Is(err, store.ErrNoRow) {
			t.Fatalf("unknown to %s: %v", to, err)
		}
	}
	if from, moved := move("old", "cc-4711"); !moved || from != store.DefaultAccount {
		t.Fatalf("old: %s %v", from, moved)
	}
	if from, moved := move("shop", store.DefaultAccount); !moved || from != "cc-4711" {
		t.Fatalf("shop: %s %v", from, moved)
	}
	// A move whose transaction began before one that committed first
	// (here: the current row taken later than this transaction's now())
	// leaves and takes at the moment that row was taken.
	exec(t, s, "UPDATE project_accounts SET moved_at = now() + interval '1 hour' WHERE project_id = 'shop' AND left_at IS NULL")
	if from, moved := move("shop", "cc-4711"); !moved || from != store.DefaultAccount {
		t.Fatalf("shop back: %s %v", from, moved)
	}
	var gap bool
	must(t, s.Q().QueryRow(bg, `SELECT bool_and(l.left_at = c.moved_at AND l.moved_at = c.moved_at)
		FROM project_accounts c JOIN project_accounts l ON l.project_id = c.project_id AND l.left_at IS NOT NULL
			AND l.account_id = 'default'
		WHERE c.project_id = 'shop' AND c.left_at IS NULL`).Scan(&gap))
	if !gap {
		t.Fatal("the later row's moment is not kept")
	}
	if from, moved := move("shop", store.DefaultAccount); !moved || from != "cc-4711" {
		t.Fatalf("shop again: %s %v", from, moved)
	}
	ms, err := s.ProjectMoves(bg, "shop")
	must(t, err)
	if len(ms) != 4 || ms[0].From != "cc-4711" || ms[0].To != store.DefaultAccount || ms[0].By != "basic:root" ||
		ms[3].From != "" || ms[3].To != "cc-4711" || ms[3].ToTitle != "Team X" || ms[3].By != by ||
		ms[3].ByName != "Mia" || !ms[3].At.Equal(shop.CreatedAt) {
		t.Fatalf("shop's moves %+v", ms)
	}
	ms, err = s.ProjectMoves(bg, "old")
	must(t, err)
	if len(ms) != 2 || ms[0].From != store.DefaultAccount || ms[0].To != "cc-4711" || ms[1].By != "" {
		t.Fatalf("old's moves %+v", ms)
	}
	ms, err = s.AccountMoves(bg, "cc-4711")
	must(t, err)
	// shop came and left, came and left; old came.
	if len(ms) != 5 || ms[0].Project != "shop" || ms[0].From != "cc-4711" || ms[4].Project != "shop" ||
		ms[4].From != "" {
		t.Fatalf("cc-4711's moves %+v", ms)
	}

	g, err := s.UpsertPerson(bg, "https://id", "g", "g@x", "Gus")
	must(t, err)
	must2 := func(_ store.Grant, err error) { t.Helper(); must(t, err) }
	must2(s.PutGrant(bg, store.Grant{ProjectID: "old", PersonID: g.ID, Role: "admin"}))
	must2(s.PutGrant(bg, store.Grant{ProjectID: "old", PersonID: maker.ID, Role: "write"}))
	ps, err := s.AccountProjects(bg, "cc-4711")
	must(t, err)
	if len(ps) != 1 || ps[0].Project != "old" || ps[0].Maker.ID != uuid.Nil || len(ps[0].Admins) != 1 ||
		ps[0].Admins[0].Name != "Gus" {
		t.Fatalf("cc-4711's projects %+v", ps)
	}
	ps, err = s.AccountProjects(bg, store.DefaultAccount)
	must(t, err)
	if len(ps) != 1 || ps[0].Project != "shop" || ps[0].Maker.Name != "Mia" || len(ps[0].Admins) != 0 {
		t.Fatalf("default's projects %+v", ps)
	}
	list, err := s.ListProjectsOn(bg, "cc-4711")
	must(t, err)
	if len(list) != 1 || list[0].ID != "old" || list[0].Account != "cc-4711" || list[0].AccountTitle != "Team X" {
		t.Fatalf("projects on cc-4711 %+v", list)
	}
	list, err = s.ListProjects(bg)
	must(t, err)
	if len(list) != 2 || list[1].Account != store.DefaultAccount {
		t.Fatalf("projects %+v", list)
	}
}

// The bootstrap's project is on default.
func TestBootstrapOnDefault(t *testing.T) {
	s, _ := newStore(t)
	made, err := s.BootstrapOperator(bg, "h-boot")
	must(t, err)
	if !made {
		t.Fatal("no bootstrap")
	}
	ms, err := s.ProjectMoves(bg, store.OperatorProject)
	must(t, err)
	if len(ms) != 1 || ms[0].To != store.DefaultAccount || ms[0].From != "" {
		t.Fatalf("moves %+v", ms)
	}
}

// An account's invitation has no project, lists and revokes by its
// account, and reads with the account's title.
func TestAccountInvitations(t *testing.T) {
	s, _ := newStore(t)
	_, _, err := s.CreateAccount(bg, store.Account{ID: "cc-4711", Title: "Team X"})
	must(t, err)
	mkProject(t, s, "acme")
	inv, err := s.InsertInvitation(bg, store.Invitation{Hash: "h1", AccountID: "cc-4711", Role: "admin",
		MadeBy: "basic:root"}, time.Hour)
	must(t, err)
	if inv.ProjectID != "" || inv.AccountID != "cc-4711" || inv.AccountTitle != "Team X" || !inv.Account() {
		t.Fatalf("invitation %+v", inv)
	}
	if _, err := s.InsertInvitation(bg, store.Invitation{Hash: "h2", AccountID: "nope", Role: "member"},
		time.Hour); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("to no account: %v", err)
	}
	_, err = s.InsertInvitation(bg, store.Invitation{Hash: "h3", ProjectID: "acme", Role: "read"}, time.Hour)
	must(t, err)
	got, err := s.GetInvitation(bg, "h3")
	must(t, err)
	if got.Account() || got.ProjectID != "acme" {
		t.Fatalf("project's %+v", got)
	}
	list, err := s.ListAccountInvitations(bg, "cc-4711")
	must(t, err)
	if len(list) != 1 || list[0].Hash != "h1" {
		t.Fatalf("list %+v", list)
	}
	if list, err := s.ListInvitations(bg, "acme"); err != nil || len(list) != 1 || list[0].Hash != "h3" {
		t.Fatalf("project's list %+v %v", list, err)
	}
	if ok, err := s.DeleteAccountInvitation(bg, "cc-4711", "h3"); err != nil || ok {
		t.Fatalf("revoke another's: %v %v", ok, err)
	}
	if ok, err := s.DeleteAccountInvitation(bg, "cc-4711", "h1"); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
}
