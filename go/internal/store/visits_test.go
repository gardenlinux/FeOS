// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// visTenantProvider is a provider of kind tenant at url.
func visTenantProvider(tenant, name, url string) store.AccessProvider {
	return store.AccessProvider{Tenant: tenant, Name: name, Kind: model.ProviderKindTenant, URL: url}
}

// visVisit inserts an open visit of env's of kind kind (a person of its
// own for ferun) and returns it.
func visVisit(t *testing.T, s *store.Store, env store.Environment, provider, kind, session string) store.Visit {
	t.Helper()
	n := store.NewVisit{ID: uuid.New(), EnvID: env.ID, Tenant: env.Owner, Provider: provider, ProviderKind: kind,
		Host: "h.apps.test", Subject: "u-1", Session: session, MaxVisit: 12 * time.Hour}
	if kind == model.ProviderKindFerun {
		p, err := s.UpsertPerson(bg, "https://id.test/", "s-"+n.ID.String(), "", "")
		must(t, err)
		n.PersonID = p.ID
	}
	v, err := s.InsertVisit(bg, n)
	must(t, err)
	return v
}

// visEnd is a visit's end reason; "" while it is open.
func visEnd(t *testing.T, s *store.Store, id uuid.UUID) string {
	t.Helper()
	v, err := s.GetVisit(bg, id)
	must(t, err)
	if v.Open() != (v.EndReason == "") {
		t.Fatalf("visit %s: ended %v with reason %q", id, v.EndedAt, v.EndReason)
	}
	return v.EndReason
}

func TestAccessProviders(t *testing.T) {
	s, d := newStore(t)
	mkTenant(t, s, "acme")
	mkTenant(t, s, "other")
	p, err := s.InsertAccessProvider(bg, visTenantProvider("acme", "fecode", "https://fecode.dev/api/v1/visit"))
	if err != nil || p.Kind != "tenant" || p.URL != "https://fecode.dev/api/v1/visit" || string(p.Rule) != "{}" ||
		p.CreatedAt.IsZero() {
		t.Fatalf("insert %+v %v", p, err)
	}
	if _, err := s.InsertAccessProvider(bg, visTenantProvider("acme", "fecode", "https://x.example/")); !errors.Is(err,
		store.ErrExists) {
		t.Fatalf("again: %v", err)
	}
	if _, err := s.InsertAccessProvider(bg, visTenantProvider("nope", "fecode", "https://x.example/")); !errors.Is(err,
		store.ErrNoRow) {
		t.Fatalf("unknown tenant: %v", err)
	}
	// The database refuses ferun, and a kind without its fields.
	if _, err := s.InsertAccessProvider(bg, visTenantProvider("acme", "ferun", "https://x.example/")); err == nil {
		t.Fatal("made a provider ferun")
	}
	if _, err := s.InsertAccessProvider(bg, visTenantProvider("acme", "nourl", "")); err == nil {
		t.Fatal("made a tenant provider without a url")
	}
	oidc := store.AccessProvider{Tenant: "acme", Name: "sso", Kind: model.ProviderKindOIDC,
		Issuer: "https://login.example/", ClientID: "c", ClientSecretSealed: []byte{1, 2, 3},
		Rule: json.RawMessage(`{"domains": ["example.com"]}`), MadeBy: "token:x"}
	if _, err := s.InsertAccessProvider(bg, oidc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertAccessProvider(bg, visTenantProvider("other", "fecode", "https://o.example/")); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAccessProviders(bg, "acme")
	if err != nil || len(list) != 2 || list[0].Name != "fecode" || list[1].Name != "sso" ||
		!bytes.Equal(list[1].ClientSecretSealed, []byte{1, 2, 3}) || list[1].MadeBy != "token:x" {
		t.Fatalf("list %+v %v", list, err)
	}
	var rule map[string][]string
	if err := json.Unmarshal(list[1].Rule, &rule); err != nil || rule["domains"][0] != "example.com" {
		t.Fatalf("rule %s %v", list[1].Rule, err)
	}
	if n, err := s.CountAccessProviders(bg, "acme"); err != nil || n != 2 {
		t.Fatalf("count %d %v", n, err)
	}
	if _, err := s.GetAccessProvider(bg, "acme", "nope"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("get unknown: %v", err)
	}

	// An environment that names a provider holds it; ferun is NULL.
	stage := mkEnv(t, s, "acme", "stage")
	prod := mkEnv(t, s, "acme", "prod")
	if _, _, err := s.SetEnvironmentAccess(bg, stage.ID, model.AccessVisitors, "fecode"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetEnvironmentAccess(bg, prod.ID, model.AccessVisitors, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetEnvironmentAccess(bg, prod.ID, model.AccessVisitors, "nope"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("a missing provider: %v", err)
	}
	// Another tenant's provider of the same name is no provider of acme's.
	other := mkEnv(t, s, "other", "web")
	if _, _, err := s.SetEnvironmentAccess(bg, other.ID, model.AccessVisitors, "sso"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("a provider of another tenant: %v", err)
	}
	if ok, err := s.DeleteAccessProvider(bg, "acme", "fecode"); !errors.Is(err, store.ErrInUse) || ok {
		t.Fatalf("delete a named provider: %v %v", ok, err)
	}
	ids, err := s.EnvironmentsNamingProvider(bg, "acme", "fecode")
	if err != nil || len(ids) != 1 || ids[0] != stage.ID {
		t.Fatalf("naming fecode %v %v", ids, err)
	}
	ids, err = s.EnvironmentsNamingProvider(bg, "acme", "")
	if err != nil || len(ids) != 1 || ids[0] != prod.ID {
		t.Fatalf("naming ferun %v %v", ids, err)
	}
	pe, err := s.ProviderEnvironments(bg, "acme")
	if err != nil || len(pe) != 2 || pe[0].Name != "prod" || pe[0].Provider != "" || pe[1].Name != "stage" ||
		pe[1].Provider != "fecode" {
		t.Fatalf("provider environments %+v %v", pe, err)
	}

	// An update writes and tells the ingress of the environments naming it.
	ch, unsubscribe := d.Subscribe(db.ChanIngress)
	defer unsubscribe()
	p.URL, p.MadeBy = "https://fecode.dev/v2/visit", "token:y"
	up, err := s.UpdateAccessProvider(bg, p)
	if err != nil || up.URL != "https://fecode.dev/v2/visit" || up.MadeBy != "token:y" || !up.UpdatedAt.After(p.UpdatedAt) {
		t.Fatalf("update %+v %v", up, err)
	}
	expectPayload(t, ch, store.IngressEnvPayload(stage.ID))
	if _, err := s.UpdateAccessProvider(bg, visTenantProvider("acme", "nope", "https://x.example/")); !errors.Is(err,
		store.ErrNoRow) {
		t.Fatalf("update unknown: %v", err)
	}

	if _, _, err := s.SetEnvironmentAccess(bg, stage.ID, model.AccessAnyone, ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteAccessProvider(bg, "acme", "fecode"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, err := s.DeleteAccessProvider(bg, "acme", "fecode"); err != nil || ok {
		t.Fatalf("delete again: %v %v", ok, err)
	}
}

// The publish's writes: unpublishing ends the open visits, a change of
// access ends them, and a call that changes nothing writes and tells
// nothing.
func TestEnvironmentAccess(t *testing.T) {
	s, d := newStore(t)
	mkTenant(t, s, "acme")
	_, err := s.InsertAccessProvider(bg, visTenantProvider("acme", "fecode", "https://fecode.dev/api/v1/visit"))
	must(t, err)
	e := mkEnv(t, s, "acme", "stage")
	if e.Access != model.AccessAnyone || e.AccessProvider != "" {
		t.Fatalf("a new environment %q %q", e.Access, e.AccessProvider)
	}
	// UpdateEnvironment never writes the access.
	changed := e
	changed.Access = model.AccessVisitors
	if _, err := s.UpdateEnvironment(bg, e, changed); err == nil {
		t.Fatal("UpdateEnvironment wrote access")
	}
	changed = e
	changed.AccessProvider = "fecode"
	if _, err := s.UpdateEnvironment(bg, e, changed); err == nil {
		t.Fatal("UpdateEnvironment wrote access_provider")
	}

	_, err = s.SetEnvironmentPublic(bg, e.ID, true)
	must(t, err)
	got, ok, err := s.SetEnvironmentAccess(bg, e.ID, model.AccessVisitors, "")
	if err != nil || !ok || got.Access != model.AccessVisitors || got.AccessProvider != "" || !got.Public {
		t.Fatalf("visitors of ferun %+v %v %v", got, ok, err)
	}
	v1 := visVisit(t, s, got, model.ProviderFerun, model.ProviderKindFerun, "")

	// Unchanged: nothing written, no notification, the visit stays.
	ch, unsubscribe := d.Subscribe(db.ChanIngress)
	defer unsubscribe()
	before, _ := s.GetEnvironment(bg, e.ID)
	got, ok, err = s.SetEnvironmentAccess(bg, e.ID, model.AccessVisitors, "")
	if err != nil || ok || !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("unchanged %+v %v %v", got, ok, err)
	}
	if _, err := s.SetEnvironmentPublic(bg, e.ID, true); err != nil {
		t.Fatal(err)
	}
	expectNoPayload(t, ch, store.IngressEnvPayload(e.ID))
	if r := visEnd(t, s, v1.ID); r != "" {
		t.Fatalf("an unchanged publish ended a visit: %s", r)
	}

	// A change of provider: the visit ends, the ingress is told.
	got, ok, err = s.SetEnvironmentAccess(bg, e.ID, model.AccessVisitors, "fecode")
	if err != nil || !ok || got.AccessProvider != "fecode" {
		t.Fatalf("visitors of fecode %+v %v %v", got, ok, err)
	}
	expectPayload(t, ch, store.IngressEnvPayload(e.ID))
	if r := visEnd(t, s, v1.ID); r != model.VisitProviderChanged {
		t.Fatalf("visit after the change %q", r)
	}
	v2 := visVisit(t, s, got, "fecode", model.ProviderKindTenant, "s-1")
	// To anyone: the visit ends too.
	if _, _, err := s.SetEnvironmentAccess(bg, e.ID, model.AccessAnyone, ""); err != nil {
		t.Fatal(err)
	}
	if r := visEnd(t, s, v2.ID); r != model.VisitProviderChanged {
		t.Fatalf("visit after anyone %q", r)
	}
	// Unpublishing ends the open visits and keeps the access.
	if _, _, err := s.SetEnvironmentAccess(bg, e.ID, model.AccessVisitors, "fecode"); err != nil {
		t.Fatal(err)
	}
	v3 := visVisit(t, s, got, "fecode", model.ProviderKindTenant, "")
	got, err = s.SetEnvironmentPublic(bg, e.ID, false)
	if err != nil || got.Public || got.Access != model.AccessVisitors || got.AccessProvider != "fecode" {
		t.Fatalf("unpublish %+v %v", got, err)
	}
	if r := visEnd(t, s, v3.ID); r != model.VisitUnpublished {
		t.Fatalf("visit after unpublishing %q", r)
	}
	// The database keeps a provider only with visitors.
	if _, err := s.Q().Exec(bg, "UPDATE environments SET access = 'anyone' WHERE id = $1", e.ID); err == nil {
		t.Fatal("anyone with a provider")
	}
}

func TestPersonMayVisit(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkTenant(t, s, "other")
	stage := mkEnv(t, s, "acme", "stage")
	prod := mkEnv(t, s, "acme", "prod")
	const iss = "https://id.test/"
	tenantWide, err := s.UpsertPerson(bg, iss, "tenant-wide", "", "")
	must(t, err)
	onStage, err := s.UpsertPerson(bg, iss, "on-stage", "", "")
	must(t, err)
	disabled, err := s.UpsertPerson(bg, iss, "disabled", "", "")
	must(t, err)
	elsewhere, err := s.UpsertPerson(bg, iss, "elsewhere", "", "")
	must(t, err)
	for _, g := range []store.Grant{
		{TenantID: "acme", PersonID: tenantWide.ID, Role: "read"},
		{TenantID: "acme", PersonID: onStage.ID, EnvironmentID: stage.ID, Role: "read"},
		{TenantID: "acme", PersonID: disabled.ID, Role: "admin"},
		{TenantID: "other", PersonID: elsewhere.ID, Role: "admin"},
	} {
		_, err := s.PutGrant(bg, g)
		must(t, err)
	}
	_, err = s.SetPersonDisabled(bg, disabled.ID, true)
	must(t, err)
	for _, c := range []struct {
		sub  string
		env  uuid.UUID
		want bool
	}{
		{"tenant-wide", stage.ID, true},
		{"tenant-wide", prod.ID, true},
		{"on-stage", stage.ID, true},
		{"on-stage", prod.ID, false},
		{"disabled", stage.ID, false},
		{"elsewhere", stage.ID, false},
		{"nobody", stage.ID, false},
	} {
		id, ok, err := s.PersonMayVisit(bg, iss, c.sub, "acme", c.env)
		if err != nil || ok != c.want || (ok && id == uuid.Nil) {
			t.Fatalf("%s on %s: %v %v %v, want %v", c.sub, c.env, id, ok, err, c.want)
		}
		if ok {
			if by, err := s.PersonMayVisitByID(bg, id, "acme", c.env); err != nil || !by {
				t.Fatalf("%s by ID: %v %v", c.sub, by, err)
			}
		}
	}
	// Another issuer's sub is someone else; nobody is made.
	if _, ok, err := s.PersonMayVisit(bg, "https://other.test/", "tenant-wide", "acme", stage.ID); err != nil || ok {
		t.Fatalf("another issuer: %v %v", ok, err)
	}
	var n int
	must(t, s.Q().QueryRow(bg, "SELECT count(*) FROM people").Scan(&n))
	if n != 4 {
		t.Fatalf("%d people", n)
	}
	if ok, err := s.PersonMayVisitByID(bg, disabled.ID, "acme", stage.ID); err != nil || ok {
		t.Fatalf("disabled by ID: %v %v", ok, err)
	}
	// A tenant being deleted admits nobody.
	_, err = s.MarkTenantDeleting(bg, "acme")
	must(t, err)
	if _, ok, err := s.PersonMayVisit(bg, iss, "tenant-wide", "acme", stage.ID); err != nil || ok {
		t.Fatalf("deleting tenant: %v %v", ok, err)
	}
}

func TestVisits(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	e := mkEnv(t, s, "acme", "stage")

	// The end: the provider's, at most MaxVisit away.
	soon := time.Now().Add(time.Hour)
	v, err := s.InsertVisit(bg, store.NewVisit{ID: uuid.New(), EnvID: e.ID, Tenant: "acme", Provider: "fecode",
		ProviderKind: model.ProviderKindTenant, Host: "h", Subject: "u", Email: "a@example.com", Session: "s",
		EndsAt: soon, MaxVisit: 12 * time.Hour})
	if err != nil || v.EndsAt.Sub(soon).Abs() > time.Millisecond || !v.Open() || v.Binding != "" ||
		v.AskedAt.IsZero() || v.Now.IsZero() || v.Email != "a@example.com" {
		t.Fatalf("insert %+v %v", v, err)
	}
	far, err := s.InsertVisit(bg, store.NewVisit{ID: uuid.New(), EnvID: e.ID, Tenant: "acme", Provider: "fecode",
		ProviderKind: model.ProviderKindTenant, Host: "h", Subject: "u", EndsAt: time.Now().Add(48 * time.Hour),
		MaxVisit: 12 * time.Hour})
	if err != nil || far.EndsAt.Sub(far.CreatedAt) != 12*time.Hour {
		t.Fatalf("capped %v %v", far.EndsAt.Sub(far.CreatedAt), err)
	}
	none, err := s.InsertVisit(bg, store.NewVisit{ID: uuid.New(), EnvID: e.ID, Tenant: "acme", Provider: "fecode",
		ProviderKind: model.ProviderKindTenant, Host: "h", Subject: "u", MaxVisit: 12 * time.Hour})
	if err != nil || none.EndsAt.Sub(none.CreatedAt) != 12*time.Hour {
		t.Fatalf("no end %v %v", none.EndsAt.Sub(none.CreatedAt), err)
	}
	// A visit of kind ferun names its person; the database says so.
	if _, err := s.InsertVisit(bg, store.NewVisit{ID: uuid.New(), EnvID: e.ID, Tenant: "acme", Provider: "ferun",
		ProviderKind: model.ProviderKindFerun, Host: "h", Subject: "u", MaxVisit: time.Hour}); err == nil {
		t.Fatal("a ferun visit without its person")
	}

	// The binding: set while open, 43 characters.
	b := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if got, err := s.BindVisit(bg, v.ID, b); err != nil || got.Binding != b {
		t.Fatalf("bind %+v %v", got, err)
	}
	if _, err := s.BindVisit(bg, v.ID, "short"); err == nil {
		t.Fatal("bound a short binding")
	}

	// The ask: claimed once within every.
	if ok, err := s.ClaimVisitAsk(bg, v.ID, 8*time.Minute); err != nil || ok {
		t.Fatalf("claim right after the accept: %v %v", ok, err)
	}
	exec(t, s, "UPDATE visits SET asked_at = now() - interval '9 minutes' WHERE id = $1", v.ID)
	var wg sync.WaitGroup
	var mu sync.Mutex
	claims := 0
	for range 8 {
		wg.Go(func() {
			ok, err := s.ClaimVisitAsk(bg, v.ID, 8*time.Minute)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				claims++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if claims != 1 {
		t.Fatalf("%d claims of one ask", claims)
	}

	// Ending: once.
	if ok, err := s.EndVisit(bg, v.ID, model.VisitSignedOut); err != nil || !ok {
		t.Fatalf("end %v %v", ok, err)
	}
	if ok, err := s.EndVisit(bg, v.ID, model.VisitExpired); err != nil || ok {
		t.Fatalf("end again %v %v", ok, err)
	}
	if r := visEnd(t, s, v.ID); r != model.VisitSignedOut {
		t.Fatalf("reason %q", r)
	}
	if _, err := s.BindVisit(bg, v.ID, b); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("bound an ended visit: %v", err)
	}
	if ok, err := s.ClaimVisitAsk(bg, v.ID, 0); err != nil || ok {
		t.Fatalf("claimed an ended visit: %v %v", ok, err)
	}
	if _, err := s.GetVisit(bg, uuid.New()); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown visit: %v", err)
	}
}

func TestVisitSessions(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkTenant(t, s, "other")
	e := mkEnv(t, s, "acme", "stage")
	o := mkEnv(t, s, "other", "stage")
	f1 := visVisit(t, s, e, "ferun", model.ProviderKindFerun, "sid")
	f2 := visVisit(t, s, o, "ferun", model.ProviderKindFerun, "sid")
	f3 := visVisit(t, s, o, "ferun", model.ProviderKindFerun, "other-sid")
	o1 := visVisit(t, s, e, "sso", model.ProviderKindOIDC, "sid")
	o2 := visVisit(t, s, o, "sso", model.ProviderKindOIDC, "sid")
	t1 := visVisit(t, s, e, "fecode", model.ProviderKindTenant, "sid")
	if _, err := s.EndSessionVisits(bg, model.ProviderKindFerun, "", "", "", model.VisitBackchannelLogout); err == nil {
		t.Fatal("ended the visits of no session")
	}
	n, err := s.EndSessionVisits(bg, model.ProviderKindFerun, "", "", "sid", model.VisitBackchannelLogout)
	if err != nil || n != 2 {
		t.Fatalf("ferun's session: %d %v", n, err)
	}
	n, err = s.EndSessionVisits(bg, model.ProviderKindOIDC, "acme", "sso", "sid", model.VisitBackchannelLogout)
	if err != nil || n != 1 {
		t.Fatalf("acme's sso session: %d %v", n, err)
	}
	for id, want := range map[uuid.UUID]string{f1.ID: model.VisitBackchannelLogout, f2.ID: model.VisitBackchannelLogout,
		f3.ID: "", o1.ID: model.VisitBackchannelLogout, o2.ID: "", t1.ID: ""} {
		if r := visEnd(t, s, id); r != want {
			t.Fatalf("visit %s: %q, want %q", id, r, want)
		}
	}
	n, err = s.EndProviderVisits(bg, "other", "sso", model.VisitProviderChanged)
	if err != nil || n != 1 || visEnd(t, s, o2.ID) != model.VisitProviderChanged {
		t.Fatalf("provider's visits: %d %v", n, err)
	}
	n, err = s.EndEnvironmentVisits(bg, e.ID, model.VisitUnpublished)
	if err != nil || n != 1 || visEnd(t, s, t1.ID) != model.VisitUnpublished {
		t.Fatalf("environment's visits: %d %v", n, err)
	}
}

func TestVisitAnswers(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	e := mkEnv(t, s, "acme", "stage")
	v := visVisit(t, s, e, "fecode", model.ProviderKindTenant, "")
	const bnd = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	const cid = "CCCCCCCCCCCCCCCCCCCCCC"
	n := store.NewVisitAnswer{ChallengeID: cid, EnvID: e.ID, Host: "h", Outcome: "accepted", VisitID: v.ID,
		Binding: bnd, AnsweredBy: "token:x", CodeLifetime: 2 * time.Minute, ExpiresAt: time.Now().Add(10 * time.Minute)}
	a, err := s.InsertVisitAnswer(bg, n)
	if err != nil || a.CodeExpiresAt.Sub(a.CreatedAt) != 2*time.Minute || !a.RedeemedAt.IsZero() || a.VisitID != v.ID {
		t.Fatalf("insert %+v %v", a, err)
	}
	if _, err := s.InsertVisitAnswer(bg, n); !errors.Is(err, store.ErrExists) {
		t.Fatalf("answered twice: %v", err)
	}
	// The first redeem, again within the window, another binding not.
	for range 2 {
		if id, ok, err := s.RedeemVisitAnswer(bg, cid, bnd, time.Minute); err != nil || !ok || id != v.ID {
			t.Fatalf("redeem %v %v %v", id, ok, err)
		}
	}
	if _, ok, err := s.RedeemVisitAnswer(bg, cid, "x"+bnd[1:], time.Minute); err != nil || ok {
		t.Fatalf("another binding: %v %v", ok, err)
	}
	exec(t, s, "UPDATE visit_answers SET redeemed_at = now() - interval '61 seconds' WHERE challenge_id = $1", cid)
	if _, ok, err := s.RedeemVisitAnswer(bg, cid, bnd, time.Minute); err != nil || ok {
		t.Fatalf("after the window: %v %v", ok, err)
	}
	got, err := s.GetVisitAnswer(bg, cid)
	if err != nil || got.RedeemedAt.IsZero() || !got.Now.Before(got.CodeExpiresAt) {
		t.Fatalf("read %+v %v", got, err)
	}
	// A rejected answer names no visit, and is never redeemed.
	r := store.NewVisitAnswer{ChallengeID: "DDDDDDDDDDDDDDDDDDDDDD", EnvID: e.ID, Host: "h", Outcome: "rejected",
		Binding: bnd, CodeLifetime: 2 * time.Minute, ExpiresAt: time.Now().Add(10 * time.Minute)}
	if _, err := s.InsertVisitAnswer(bg, r); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.RedeemVisitAnswer(bg, r.ChallengeID, bnd, time.Minute); err != nil || ok {
		t.Fatalf("redeemed a rejection: %v %v", ok, err)
	}
	r.ChallengeID, r.VisitID = "EEEEEEEEEEEEEEEEEEEEEE", v.ID
	if _, err := s.InsertVisitAnswer(bg, r); err == nil {
		t.Fatal("a rejection with a visit")
	}
	if _, err := s.GetVisitAnswer(bg, "FFFFFFFFFFFFFFFFFFFFFF"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("unknown answer: %v", err)
	}
}

func TestSweepVisits(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	e := mkEnv(t, s, "acme", "stage")
	live := visVisit(t, s, e, "fecode", model.ProviderKindTenant, "")
	past := visVisit(t, s, e, "fecode", model.ProviderKindTenant, "")
	exec(t, s, "UPDATE visits SET ends_at = now() - interval '1 minute' WHERE id = $1", past.ID)
	old := visVisit(t, s, e, "fecode", model.ProviderKindTenant, "")
	exec(t, s, `UPDATE visits SET ended_at = now() - interval '25 hours', end_reason = 'signed_out' WHERE id = $1`, old.ID)
	oldOpen := visVisit(t, s, e, "fecode", model.ProviderKindTenant, "")
	exec(t, s, `UPDATE visits SET created_at = now() - interval '40 hours', ends_at = now() - interval '28 hours'
		WHERE id = $1`, oldOpen.ID)
	for i, exp := range []string{"now() - interval '2 hours'", "now() - interval '10 minutes'"} {
		exec(t, s, `INSERT INTO visit_answers (challenge_id, env_id, host, outcome, binding, code_expires_at, expires_at)
			VALUES ($1, $2, 'h', 'rejected', 'b', now(), `+exp+`)`, []string{"AAAAAAAAAAAAAAAAAAAAAA",
			"BBBBBBBBBBBBBBBBBBBBBB"}[i], e.ID)
	}
	sw, err := s.SweepVisits(bg, model.VisitExpired, 24*time.Hour, time.Hour)
	if err != nil || sw.Ended != 2 || sw.Deleted != 2 || sw.Answers != 1 {
		t.Fatalf("sweep %+v %v", sw, err)
	}
	if r := visEnd(t, s, live.ID); r != "" {
		t.Fatalf("live visit %q", r)
	}
	if r := visEnd(t, s, past.ID); r != model.VisitExpired {
		t.Fatalf("past visit %q", r)
	}
	for _, id := range []uuid.UUID{old.ID, oldOpen.ID} {
		if _, err := s.GetVisit(bg, id); !errors.Is(err, store.ErrNoRow) {
			t.Fatalf("old visit kept: %v", err)
		}
	}
	if _, err := s.GetVisitAnswer(bg, "BBBBBBBBBBBBBBBBBBBBBB"); err != nil {
		t.Fatalf("a young answer swept: %v", err)
	}
}

func TestEnsureClusterSecret(t *testing.T) {
	s, _ := newStore(t)
	a := bytes.Repeat([]byte{1}, 32)
	b := bytes.Repeat([]byte{2}, 32)
	var wg sync.WaitGroup
	got := make([][]byte, 8)
	for i := range got {
		wg.Go(func() {
			v := a
			if i%2 == 1 {
				v = b
			}
			out, err := s.EnsureClusterSecret(bg, "visit-challenge", v)
			if err != nil {
				t.Error(err)
			}
			got[i] = out
		})
	}
	wg.Wait()
	for _, g := range got {
		if !bytes.Equal(g, got[0]) {
			t.Fatalf("instances disagree: %x %x", g, got[0])
		}
	}
	// The sealed seed is 61 bytes; only visit-token may hold that.
	sealed := bytes.Repeat([]byte{3}, 61)
	if out, err := s.EnsureClusterSecret(bg, "visit-token", sealed); err != nil || !bytes.Equal(out, sealed) {
		t.Fatalf("visit-token %x %v", out, err)
	}
	if _, err := s.EnsureClusterSecret(bg, "other", sealed); err == nil {
		t.Fatal("a 61-byte secret of another name")
	}
	if _, err := s.EnsureClusterSecret(bg, "visit-token", b); err != nil {
		t.Fatal(err)
	}
	if out, _ := s.ClusterSecret(bg, "visit-token"); !bytes.Equal(out, sealed) {
		t.Fatal("a second make replaced the secret")
	}
}
