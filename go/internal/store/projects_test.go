// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

func TestProjectsAndTokens(t *testing.T) {
	s, _ := newStore(t)
	tn, created, err := s.CreateProject(bg, "acme")
	if err != nil || !created || tn.ID != "acme" || tn.Deleting() {
		t.Fatalf("create %+v %v %v", tn, created, err)
	}
	if _, created, err := s.CreateProject(bg, "acme"); err != nil || created {
		t.Fatalf("create twice %v %v", created, err)
	}
	if _, _, err := s.CreateProject(bg, "Bad_Name"); err == nil {
		t.Fatal("a bad project name was accepted")
	}
	if !store.ProjectNamePattern.MatchString("acme") || store.ProjectNamePattern.MatchString("a") {
		t.Fatal("project name pattern")
	}
	mkProject(t, s, "beta")

	tokA := store.HashToken("token-a")
	if len(tokA) != 64 {
		t.Fatalf("hash %q", tokA)
	}
	if _, err := s.CreateProjectToken(bg, tokA, "acme", "ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProjectToken(bg, store.HashToken("x"), "nobody", ""); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("token of a missing project: %v", err)
	}
	if project, err := s.ResolveProjectToken(bg, tokA); err != nil || project != "acme" {
		t.Fatalf("resolve %q %v", project, err)
	}
	if _, err := s.ResolveProjectToken(bg, store.HashToken("nope")); !errors.Is(err, model.ErrUnknownToken) {
		t.Fatalf("resolve unknown: %v", err)
	}
	must(t, s.TouchProjectToken(bg, tokA))
	toks, err := s.ListProjectTokens(bg, "acme")
	if err != nil || len(toks) != 1 || toks[0].Label != "ci" || toks[0].LastUsedAt.IsZero() {
		t.Fatalf("tokens %+v %v", toks, err)
	}
	// A second touch within the minute writes nothing.
	first := toks[0].LastUsedAt
	must(t, s.TouchProjectToken(bg, tokA))
	if toks, _ := s.ListProjectTokens(bg, "acme"); !toks[0].LastUsedAt.Equal(first) {
		t.Fatal("touch is not throttled")
	}

	sb := mkSandbox(t, s, "acme")
	mkSandbox(t, s, "acme")
	list, err := s.ListProjects(bg)
	if err != nil || len(list) != 2 || list[0].ID != "acme" || list[0].SandboxCount != 2 || len(list[0].Tokens) != 1 ||
		list[1].SandboxCount != 0 || list[1].Tokens == nil {
		t.Fatalf("list %+v %v", list, err)
	}

	// Sandbox tokens.
	sbTok := store.HashToken("sandbox-token")
	st, err := s.CreateSandboxToken(bg, sbTok, sb.ID, "default", "")
	if err != nil || st.SandboxID != sb.ID {
		t.Fatalf("sandbox token %+v %v", st, err)
	}
	if _, err := s.CreateSandboxToken(bg, store.HashToken("y"), podid.Mint(), "", ""); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("token of a missing sandbox: %v", err)
	}
	id, owner, err := s.ResolveSandboxToken(bg, sbTok)
	if err != nil || id != sb.ID || owner != "acme" {
		t.Fatalf("resolve sandbox token %v %q %v", id, owner, err)
	}
	must(t, s.TouchSandboxToken(bg, sbTok))
	if got, err := s.GetSandboxToken(bg, sbTok); err != nil || got.LastUsedAt.IsZero() {
		t.Fatalf("get %+v %v", got, err)
	}
	if l, err := s.ListSandboxTokens(bg, sb.ID); err != nil || len(l) != 1 {
		t.Fatalf("list sandbox tokens %v %v", l, err)
	}

	// Deleting: the project's tokens go, its sandbox tokens stop resolving,
	// and it stays while it has sandboxes.
	tx(t, s, func(tx *store.Store) error {
		if _, err := tx.LockProject(bg, "acme", store.ForUpdate); err != nil {
			return err
		}
		ok, err := tx.MarkProjectDeleting(bg, "acme")
		if err == nil && !ok {
			t.Error("mark deleting: no project")
		}
		return err
	})
	if _, err := s.ResolveProjectToken(bg, tokA); !errors.Is(err, model.ErrUnknownToken) {
		t.Fatalf("project token of a deleting project: %v", err)
	}
	if _, _, err := s.ResolveSandboxToken(bg, sbTok); !errors.Is(err, model.ErrUnknownToken) {
		t.Fatalf("sandbox token of a deleting project: %v", err)
	}
	if tn, _ := s.GetProject(bg, "acme"); !tn.Deleting() {
		t.Fatal("not deleting")
	}
	if names, err := s.DeletingProjects(bg); err != nil || len(names) != 1 || names[0] != "acme" {
		t.Fatalf("deleting projects %v %v", names, err)
	}
	if ok, err := s.DeleteProjectIfEmpty(bg, "acme"); err != nil || ok {
		t.Fatalf("deleted with sandboxes: %v %v", ok, err)
	}
	exec(t, s, "DELETE FROM sandboxes WHERE owner = 'acme'")
	exec(t, s, "DELETE FROM environments WHERE owner = 'acme'")
	if ok, err := s.DeleteProjectIfEmpty(bg, "acme"); err != nil || !ok {
		t.Fatalf("delete empty: %v %v", ok, err)
	}
	if _, err := s.GetProject(bg, "acme"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("deleted project: %v", err)
	}
	if ok, err := s.MarkProjectDeleting(bg, "acme"); err != nil || ok {
		t.Fatalf("mark a missing project: %v %v", ok, err)
	}

	// Revocation by hash.
	tokB := store.HashToken("token-b")
	_, err = s.CreateProjectToken(bg, tokB, "beta", "")
	must(t, err)
	if ok, err := s.DeleteProjectToken(bg, "acme", tokB); err != nil || ok {
		t.Fatalf("revoke another project's token: %v %v", ok, err)
	}
	if ok, err := s.DeleteProjectToken(bg, "beta", tokB); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
}

// Starting a project's deletion takes its environments out of the
// ingress's table: MarkProjectDeleting sends ferun_ingress for each of
// them, with its transaction's commit, and only when it starts the
// deletion.
func TestMarkProjectDeletingNotifiesIngress(t *testing.T) {
	s, d := newStore(t)
	mkProject(t, s, "acme")
	mkProject(t, s, "beta")
	web := mkEnv(t, s, "acme", "web")
	api := mkEnv(t, s, "acme", "api")
	other := mkEnv(t, s, "beta", "web")
	ch, unsubscribe := d.Subscribe(db.ChanIngress)
	defer unsubscribe()
	// seen collects the payloads until every one of want arrived (5 s at
	// most), then for 300 ms more.
	seen := func(want ...string) map[string]bool {
		t.Helper()
		got := map[string]bool{}
		missing := func() bool {
			for _, w := range want {
				if !got[w] {
					return true
				}
			}
			return false
		}
		deadline := time.After(5 * time.Second)
		for missing() {
			select {
			case p := <-ch:
				got[p] = true
			case <-deadline:
				t.Fatalf("payloads %v, want %v", got, want)
			}
		}
		quiet := time.After(300 * time.Millisecond)
		for {
			select {
			case p := <-ch:
				got[p] = true
			case <-quiet:
				return got
			}
		}
	}

	errRollback := errors.New("rolled back")
	if err := s.InTx(bg, func(tx *store.Store) error {
		if _, err := tx.MarkProjectDeleting(bg, "acme"); err != nil {
			return err
		}
		return errRollback
	}); !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("a rolled back mark sent %v", got)
	}

	tx(t, s, func(tx *store.Store) error {
		if _, err := tx.LockProject(bg, "acme", store.ForUpdate); err != nil {
			return err
		}
		ok, err := tx.MarkProjectDeleting(bg, "acme")
		if err == nil && !ok {
			t.Error("mark deleting: no project")
		}
		return err
	})
	if got := seen(ingressEnv(web.ID), ingressEnv(api.ID)); len(got) != 2 || got[ingressEnv(other.ID)] {
		t.Fatalf("payloads %v", got)
	}
	// Marked again: deleting_at stays, and nothing is sent.
	if ok, err := s.MarkProjectDeleting(bg, "acme"); err != nil || !ok {
		t.Fatalf("mark again: %v %v", ok, err)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("a second mark sent %v", got)
	}
}

func TestBootstrapOperator(t *testing.T) {
	s, _ := newStore(t)
	h := store.HashToken("bootstrap")
	var created bool
	tx(t, s, func(tx *store.Store) (err error) {
		created, err = tx.BootstrapOperator(bg, h)
		return err
	})
	if !created {
		t.Fatal("no operator project")
	}
	if project, err := s.ResolveProjectToken(bg, h); err != nil || project != store.OperatorProject {
		t.Fatalf("bootstrap token %q %v", project, err)
	}
	// Once any project exists, nothing more is made.
	if created, err := s.BootstrapOperator(bg, store.HashToken("other")); err != nil || created {
		t.Fatalf("second bootstrap %v %v", created, err)
	}
}

func TestSandboxTokenOfDeletedSandbox(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "acme")
	sb := mkSandbox(t, s, "acme")
	h := store.HashToken("t")
	_, err := s.CreateSandboxToken(bg, h, sb.ID, "", "")
	must(t, err)
	_, err = s.DeleteSandbox(bg, sb.ID)
	must(t, err)
	if _, _, err := s.ResolveSandboxToken(bg, h); !errors.Is(err, model.ErrUnknownToken) {
		t.Fatalf("token of a deleted sandbox: %v", err)
	}
	if ok, err := s.DeleteSandboxToken(bg, sb.ID, h); err != nil || ok {
		t.Fatalf("revoke after delete: %v %v", ok, err)
	}
}
