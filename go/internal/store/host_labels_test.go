// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Host labels across projects. A dotted project or a hyphenated name gets
// the fallback, so the pairs whose natural labels once read the same
// (F4, F14 of the security review) get distinct ones. A label stays with
// the project that first claimed it: another project never gets it, while
// the first project gets it back for a new environment of the same name.
func TestClaimHostLabel(t *testing.T) {
	s, _ := newStore(t)
	for _, project := range []string{"acme-io", "acme.io", "io"} {
		mkProject(t, s, project)
	}
	web := mkEnv(t, s, "acme-io", "web")
	if e := publish(t, s, web.ID, true); e.HostLabel != "web-acme-io" {
		t.Fatalf("published %+v", e)
	}

	// The environments whose natural label once read web-acme-io, in a
	// dotted project and with a hyphenated name: each its fallback.
	seen := map[string]bool{"web-acme-io": true}
	for _, p := range [][2]string{{"acme.io", "web"}, {"io", "web-acme"}} {
		other := mkEnv(t, s, p[0], p[1])
		_, ok, fallback := model.HostLabels(other.Name, other.Owner, other.ID)
		if ok {
			t.Fatalf("%q: the natural label is usable", p)
		}
		o := publish(t, s, other.ID, true)
		if o.HostLabel != fallback || seen[o.HostLabel] {
			t.Fatalf("%q: %+v (fallback %s)", p, o, fallback)
		}
		seen[o.HostLabel] = true
	}

	// No two pairs share a natural label any more, but a label another
	// project holds, written here by hand, still goes to the fallback.
	exec(t, s, "INSERT INTO host_labels (label, owner) VALUES ('api-acme-io', 'io')")
	api := mkEnv(t, s, "acme-io", "api")
	_, ok, fallback := model.HostLabels(api.Name, api.Owner, api.ID)
	if !ok {
		t.Fatal("the natural label is not usable")
	}
	if a := publish(t, s, api.ID, true); a.HostLabel != fallback {
		t.Fatalf("a label another project holds %+v (fallback %s)", a, fallback)
	}

	// The first project deletes its environments and makes them again
	// under the same names: it gets its own label back, and a label
	// another project holds stays out of reach.
	for _, id := range []uuid.UUID{web.ID, api.ID} {
		if ok, err := s.DeleteEnvironment(bg, id); err != nil || !ok {
			t.Fatalf("delete %s: %v %v", id, ok, err)
		}
	}
	web2 := mkEnv(t, s, "acme-io", "web")
	if e := publish(t, s, web2.ID, true); e.HostLabel != "web-acme-io" {
		t.Fatalf("re-created %+v", e)
	}
	api2 := mkEnv(t, s, "acme-io", "api")
	_, _, fallback2 := model.HostLabels(api2.Name, api2.Owner, api2.ID)
	if a := publish(t, s, api2.ID, true); a.HostLabel != fallback2 {
		t.Fatalf("re-created api %+v (fallback %s)", a, fallback2)
	}
}

// A label the project chose replaces the one it had: the old one stays
// the project's, free for it alone; a chosen label another project owns or
// another environment holds is ErrHostLabelTaken, and the same label
// changes nothing.
func TestSetEnvironmentHostLabel(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "acme")
	mkProject(t, s, "other")
	web := publish(t, s, mkEnv(t, s, "acme", "web").ID, true)
	if web.HostLabel != "web-acme" {
		t.Fatalf("published %+v", web)
	}
	e, err := s.SetEnvironmentHostLabel(bg, web, "web--shop")
	if err != nil || e.HostLabel != "web--shop" || !e.Public {
		t.Fatalf("chosen %+v %v", e, err)
	}
	if again, err := s.SetEnvironmentHostLabel(bg, e, "web--shop"); err != nil || again.HostLabel != "web--shop" {
		t.Fatalf("again %+v %v", again, err)
	}
	// The old label is free for acme alone.
	api := mkEnv(t, s, "acme", "api")
	if _, err := s.SetEnvironmentHostLabel(bg, api, "web--shop"); !errors.Is(err, store.ErrHostLabelTaken) {
		t.Fatalf("held by web: %v", err)
	}
	x := mkEnv(t, s, "other", "x")
	if _, err := s.SetEnvironmentHostLabel(bg, x, "web-acme"); !errors.Is(err, store.ErrHostLabelTaken) {
		t.Fatalf("acme's old label to another project: %v", err)
	}
	if a, err := s.SetEnvironmentHostLabel(bg, api, "web-acme"); err != nil || a.HostLabel != "web-acme" {
		t.Fatalf("acme's old label to acme: %+v %v", a, err)
	}
	// Whose a label is: the project that first claimed it, held or not.
	for label, want := range map[string]string{"web-acme": "acme", "web--shop": "acme"} {
		if owner, err := s.HostLabelOwner(bg, "", label); err != nil || owner != want {
			t.Fatalf("owner of %s: %q %v", label, owner, err)
		}
	}
	if owner, err := s.HostLabelOwner(bg, "", "kino"); !errors.Is(err, store.ErrNoRow) || owner != "" {
		t.Fatalf("owner of an unclaimed label: %q %v", owner, err)
	}
}

// tz is the project zone of the tests.
const tz = "apps.run.example.com"

// A label is a name in a zone (ADR 0031): a project of the project zone
// takes labels there, which a project of the apps zone may hold too, and a
// chosen label taken in one zone is free in the other. A project made with
// the project zone has it, and its maker.
func TestHostLabelsPerZone(t *testing.T) {
	s, _ := newStore(t)
	mkProject(t, s, "fecode")
	person := uuid.New()
	exec(t, s, "INSERT INTO people (id, issuer, sub) VALUES ($1, 'https://id.example.com', 'p1')", person)
	tn, created, err := s.MakeProject(bg, "acme", store.NewProject{Zone: tz, MadeBy: person})
	if err != nil || !created || tn.Zone != tz || tn.MadeBy != person {
		t.Fatalf("made %+v %v %v", tn, created, err)
	}
	if again, created, err := s.MakeProject(bg, "acme", store.NewProject{}); err != nil || created || again.Zone != tz {
		t.Fatalf("made twice %+v %v %v", again, created, err)
	}
	if op, err := s.GetProject(bg, "fecode"); err != nil || op.Zone != "" || op.MadeBy != uuid.Nil {
		t.Fatalf("an operator's project %+v %v", op, err)
	}

	web := publish(t, s, mkEnv(t, s, "acme", "web").ID, true)
	if web.HostLabel != "web-acme" {
		t.Fatalf("published %+v", web)
	}
	if owner, err := s.HostLabelOwner(bg, tz, "web-acme"); err != nil || owner != "acme" {
		t.Fatalf("owner in the project zone %q %v", owner, err)
	}
	if _, err := s.HostLabelOwner(bg, "", "web-acme"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("the label is the apps zone's too: %v", err)
	}
	// The same chosen label in both zones, one project each.
	shop, err := s.SetEnvironmentHostLabel(bg, web, "stage--shop")
	if err != nil || shop.HostLabel != "stage--shop" {
		t.Fatalf("chosen in the project zone %+v %v", shop, err)
	}
	stage := mkEnv(t, s, "fecode", "stage")
	if e, err := s.SetEnvironmentHostLabel(bg, stage, "stage--shop"); err != nil || e.HostLabel != "stage--shop" {
		t.Fatalf("chosen in the apps zone %+v %v", e, err)
	}
	// Within a zone it is still first come.
	other := mkEnv(t, s, "acme", "other")
	if _, err := s.SetEnvironmentHostLabel(bg, other, "stage--shop"); !errors.Is(err, store.ErrHostLabelTaken) {
		t.Fatalf("held by web: %v", err)
	}

	// The ingress's rows carry the project's zone.
	exec(t, s, "UPDATE environments SET deployment = 1 WHERE id = $1", web.ID)
	rows, err := s.IngressRouting(bg, []uuid.UUID{web.ID})
	if err != nil || len(rows) != 1 || rows[0].Zone != tz || rows[0].HostLabel != "stage--shop" {
		t.Fatalf("routing %+v %v", rows, err)
	}
}

// Moving a project to another zone moves its environments' labels: each
// keeps its label where it may, a label another project holds in the new
// zone gives way to the natural label, a plain label goes into no zone
// without owners, and the labels left behind stay the project's there.
// Every environment moved is notified; a move to the zone it has changes
// nothing.
func TestSetProjectZone(t *testing.T) {
	s, d := newStore(t)
	mkProject(t, s, "fecode")
	mkProject(t, s, "other")
	ch, unsubscribe := d.Subscribe(db.ChanIngress)
	defer unsubscribe()

	natural := publish(t, s, mkEnv(t, s, "fecode", "web").ID, true)
	chosen, err := s.SetEnvironmentHostLabel(bg, publish(t, s, mkEnv(t, s, "fecode", "stage").ID, true), "stage--kino")
	must(t, err)
	plain, err := s.SetEnvironmentHostLabel(bg, publish(t, s, mkEnv(t, s, "fecode", "prod").ID, true), "kino")
	must(t, err)
	taken, err := s.SetEnvironmentHostLabel(bg, publish(t, s, mkEnv(t, s, "fecode", "demo").ID, true), "demo--kino")
	must(t, err)
	never := mkEnv(t, s, "fecode", "draft") // never published: no label
	// demo--kino is another project's in the project zone.
	exec(t, s, "INSERT INTO host_labels (zone, label, owner) VALUES ($1, 'demo--kino', 'other')", tz)
	drain(ch)

	moves, err := s.SetProjectZone(bg, "fecode", tz, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"demo":  {"demo--kino", "demo-fecode"},
		"prod":  {"kino", "prod-fecode"},
		"stage": {"stage--kino", "stage--kino"},
		"web":   {"web-fecode", "web-fecode"},
	}
	if len(moves) != len(want) {
		t.Fatalf("moves %+v", moves)
	}
	for _, m := range moves {
		if w := want[m.Name]; m.From != w[0] || m.To != w[1] {
			t.Errorf("%s moved %s → %s, want %v", m.Name, m.From, m.To, w)
		}
		e, err := s.GetEnvironment(bg, m.EnvID)
		if err != nil || e.HostLabel != m.To {
			t.Errorf("%s reads %+v %v", m.Name, e, err)
		}
		if owner, err := s.HostLabelOwner(bg, tz, m.To); err != nil || owner != "fecode" {
			t.Errorf("%s: owner of %s in the project zone %q %v", m.Name, m.To, owner, err)
		}
		// What it held in the apps zone stays fecode's, held by nobody.
		var envID *uuid.UUID
		must(t, s.Q().QueryRow(bg, "SELECT env_id FROM host_labels WHERE zone = '' AND label = $1", m.From).Scan(&envID))
		if envID != nil {
			t.Errorf("%s: %s is still held in the apps zone", m.Name, m.From)
		}
	}
	expectPayloads(t, ch, ingressEnv(natural.ID), ingressEnv(chosen.ID), ingressEnv(plain.ID), ingressEnv(taken.ID))
	if tn, err := s.GetProject(bg, "fecode"); err != nil || tn.Zone != tz {
		t.Fatalf("project %+v %v", tn, err)
	}
	if e, err := s.GetEnvironment(bg, never.ID); err != nil || e.HostLabel != "" {
		t.Fatalf("an environment never published %+v %v", e, err)
	}
	// A first publish there claims in the project zone.
	if e := publish(t, s, never.ID, true); e.HostLabel != "draft-fecode" {
		t.Fatalf("published in the project zone %+v", e)
	}
	if owner, err := s.HostLabelOwner(bg, tz, "draft-fecode"); err != nil || owner != "fecode" {
		t.Fatalf("draft's label %q %v", owner, err)
	}
	drain(ch)

	// The same zone again: nothing.
	if moves, err := s.SetProjectZone(bg, "fecode", tz, false); err != nil || len(moves) != 0 {
		t.Fatalf("again %+v %v", moves, err)
	}
	expectNoPayload(t, ch, ingressEnv(natural.ID))

	// Back to the apps zone, an owner's (plainOK): its labels of the
	// project zone move; the plain label it gave up is free for it there.
	moves, err = s.SetProjectZone(bg, "fecode", "", true)
	if err != nil || len(moves) != 5 {
		t.Fatalf("back %+v %v", moves, err)
	}
	if tn, err := s.GetProject(bg, "fecode"); err != nil || tn.Zone != "" {
		t.Fatalf("project %+v %v", tn, err)
	}
	p, err := s.GetEnvironment(bg, plain.ID)
	must(t, err)
	if p, err = s.SetEnvironmentHostLabel(bg, p, "kino"); err != nil || p.HostLabel != "kino" {
		t.Fatalf("the plain label again %+v %v", p, err)
	}
	if _, err := s.SetProjectZone(bg, "nope", tz, false); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown project: %v", err)
	}
}

// A rename writes the name alone (ADR 0037): the host label claimed from
// the name before stays, and a zone move keeps it as the environment's
// own, a natural label as a fallback. A name another environment of the
// owner has is ErrExists, an unknown ID ErrNoRow.
func TestRenameEnvironment(t *testing.T) {
	s, d := newStore(t)
	mkProject(t, s, "fecode")
	ch, unsubscribe := d.Subscribe(db.ChanIngress)
	defer unsubscribe()
	natural := publish(t, s, mkEnv(t, s, "fecode", "web").ID, true)
	fallback := publish(t, s, mkEnv(t, s, "fecode", "kino-prod-9101dfcb").ID, true)
	if !strings.HasPrefix(fallback.HostLabel, "kino-prod-9101dfcb-") {
		t.Fatalf("fallback %+v", fallback)
	}
	drain(ch)

	e, err := s.RenameEnvironment(bg, natural.ID, "web--prod")
	if err != nil || e.Name != "web--prod" || e.HostLabel != "web-fecode" || !e.Public {
		t.Fatalf("rename %+v %v", e, err)
	}
	expectPayloads(t, ch, ingressEnv(natural.ID))
	if e, err := s.RenameEnvironment(bg, fallback.ID, "kino--prod"); err != nil || e.HostLabel != fallback.HostLabel {
		t.Fatalf("rename %+v %v", e, err)
	}
	if _, err := s.RenameEnvironment(bg, fallback.ID, "web--prod"); !errors.Is(err, store.ErrExists) {
		t.Fatalf("a name taken: %v", err)
	}
	if _, err := s.RenameEnvironment(bg, uuid.New(), "x"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("no environment: %v", err)
	}

	moves, err := s.SetProjectZone(bg, "fecode", tz, false)
	if err != nil || len(moves) != 2 {
		t.Fatalf("moves %+v %v", moves, err)
	}
	for _, m := range moves {
		if m.From != m.To {
			t.Errorf("%s moved %s → %s, want its label kept", m.Name, m.From, m.To)
		}
	}
}

// drain empties a subscription's channel.
func drain(ch <-chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
