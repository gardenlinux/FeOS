// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visits

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/visit"
)

func TestNewAndKinds(t *testing.T) {
	t.Parallel()
	st := store.On(nil)
	if _, err := New(Config{}, Deps{Store: st}); err == nil {
		t.Fatal("New without a sealer")
	}
	if _, err := New(Config{}, Deps{Sealer: nopSealer{}}); err == nil {
		t.Fatal("New without a store")
	}
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{}, "tenant"},
		{Config{Issuer: testIssuer}, "tenant"},
		{Config{AuthURL: testAuth + "/"}, "tenant,oidc"},
		{Config{AuthURL: testAuth, Issuer: testIssuer}, "tenant,ferun,oidc"},
	} {
		s, err := New(c.cfg, Deps{Store: st, Sealer: nopSealer{}})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(s.Kinds(), ","); got != c.want {
			t.Fatalf("%+v: kinds %s, want %s", c.cfg, got, c.want)
		}
		if l := s.Loops(); len(l) != 1 || l[0].Name != "visits-sweep" || l[0].Interval != 10*time.Minute {
			t.Fatalf("loops %+v", l)
		}
	}
}

// nopSealer is a sealer for tests that seal nothing.
type nopSealer struct{}

func (nopSealer) Seal(p, _ []byte) ([]byte, error) { return p, nil }
func (nopSealer) Open(p, _ []byte) ([]byte, error) { return p, nil }

// Two instances on one empty database end with the same keys; the seed
// is stored sealed.
func TestKeys(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	a := newFxOn(t, d, Config{})
	b := newFxOn(t, d, Config{})
	var wg sync.WaitGroup
	var ka, kb *visit.Keys
	var ea, eb error
	wg.Go(func() { ka, ea = a.s.Keys(a.ctx) })
	wg.Go(func() { kb, eb = b.s.Keys(b.ctx) })
	wg.Wait()
	if ea != nil || eb != nil {
		t.Fatal(ea, eb)
	}
	if ka.TokenKID() != kb.TokenKID() || ka.ChallengeKID() != kb.ChallengeKID() || !ka.CanSign() {
		t.Fatalf("keys differ: %s %s / %s %s", ka.TokenKID(), kb.TokenKID(), ka.ChallengeKID(), kb.ChallengeKID())
	}
	if again, _ := a.s.Keys(a.ctx); again != ka {
		t.Fatal("keys not cached")
	}
	stored, err := a.st.ClusterSecret(a.ctx, SecretToken)
	if err != nil || len(stored) != 61 {
		t.Fatalf("stored seed %d bytes %v", len(stored), err)
	}
	seed, err := a.sealer.Open(stored, []byte("cluster_secrets|visit-token"))
	if err != nil || bytes.Equal(seed, stored) {
		t.Fatalf("the seed does not open: %v", err)
	}
	if k, err := visit.NewKeys(seed, mustSecret(t, a, SecretChallenge)); err != nil || k.TokenKID() != ka.TokenKID() {
		t.Fatalf("the stored seed is not the key's: %v", err)
	}
	// A sealer of another key opens nothing: no keys, and no start error.
	other, err := newService(Config{Log: dbtest.Logger(t)}, Deps{Store: a.st, Sealer: wrongSealer{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Keys(a.ctx); err == nil {
		t.Fatal("keys from a seed sealed with another key")
	}
}

type wrongSealer struct{}

func (wrongSealer) Seal(p, _ []byte) ([]byte, error) { return p, nil }
func (wrongSealer) Open([]byte, []byte) ([]byte, error) {
	return nil, errors.New("does not open")
}

func mustSecret(t *testing.T, f *fx, name string) []byte {
	t.Helper()
	v, err := f.st.ClusterSecret(f.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpenChallenge(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	sealed, _ := f.challenge(e, host, "/menu?x=1")
	ch, err := f.s.OpenChallenge(f.ctx, tenantCaller, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Environment.ID != e.ID.String() || ch.Environment.Name != "stage" || ch.Environment.Tenant != testTenant ||
		ch.Environment.Labels["fecode.project"] != "p1" || ch.Host != host || ch.Path != "/menu?x=1" || ch.Expired ||
		ch.Answered || ch.Restart != "https://"+host+"/menu?x=1" || ch.Provider != (ProviderRef{"fecode", "tenant"}) ||
		ch.OIDC != nil || ch.ExpiresAt == "" {
		t.Fatalf("open %+v", ch)
	}
	notFound := func(what string, c Caller, sealed string) {
		t.Helper()
		_, err := f.s.OpenChallenge(f.ctx, c, sealed)
		wantAPI(t, err, http.StatusNotFound, "", "no such challenge")
		_, err = f.s.AcceptChallenge(f.ctx, c, Accept{Challenge: sealed, Subject: "u"})
		wantAPI(t, err, http.StatusNotFound, "", "no such challenge")
		_, err = f.s.RejectChallenge(f.ctx, c, sealed)
		wantAPI(t, err, http.StatusNotFound, "", "no such challenge")
		_ = what
	}
	// Forged, flipped, oversize, empty.
	notFound("forged", tenantCaller, "AAAA")
	notFound("flipped", tenantCaller, sealed[:len(sealed)-2]+"AA")
	notFound("oversize", tenantCaller, strings.Repeat("A", visit.MaxSealed+1))
	notFound("empty", tenantCaller, "")
	// A host that is not the environment's.
	other, _ := f.challenge(e, "evil.example", "/")
	notFound("foreign host", tenantCaller, other)
	// Callers that may not: not admin, another tenant, ferun-auth for a
	// tenant provider.
	notFound("reader", readerCaller, sealed)
	notFound("foreign tenant", foreignCaller, sealed)
	notFound("ferun-auth on kind tenant", authCaller, sealed)
	notFound("no admin func", Caller{Tenant: testTenant}, sealed)
	// A tenant principal for an environment of kind ferun.
	fe, fhost := f.env("ferun-env", "")
	fs, _ := f.challenge(fe, fhost, "/")
	notFound("tenant on kind ferun", tenantCaller, fs)
	if ch, err := f.s.OpenChallenge(f.ctx, authCaller, fs); err != nil || ch.Provider != (ProviderRef{"ferun", "ferun"}) {
		t.Fatalf("ferun-auth on kind ferun: %+v %v", ch, err)
	}
	// ferun-auth sees an oidc provider's client with its secret.
	f.oidcProvider("sso", "s3cret")
	oe, ohost := f.env("oidc-env", "sso")
	os, _ := f.challenge(oe, ohost, "/")
	ch, err = f.s.OpenChallenge(f.ctx, authCaller, os)
	if err != nil || ch.OIDC == nil || ch.OIDC.ClientSecret != "s3cret" || ch.OIDC.ClientID != "client" ||
		ch.OIDC.Rule.Domains[0] != "example.com" || ch.Provider.Kind != "oidc" {
		t.Fatalf("oidc %+v %v", ch, err)
	}
	notFound("tenant on kind oidc", tenantCaller, os)

	// The environment unpublished, open to anyone, or another tenant's.
	_, err = f.st.SetEnvironmentPublic(f.ctx, oe.ID, false)
	must(t, err)
	notFound("unpublished", authCaller, os)
	_, _, err = f.st.SetEnvironmentAccess(f.ctx, fe.ID, model.AccessAnyone, "")
	must(t, err)
	notFound("anyone", authCaller, fs)

	// Expired: answered with its restart address; the accept is a 409.
	f.exec("UPDATE environments SET labels = '{}' WHERE id = $1", e.ID)
	id, _ := visit.NewChallengeID()
	secret, _ := visit.NewBindingSecret()
	old := time.Now().Add(-13 * time.Minute).Unix()
	late, err := f.keys().SealChallenge(visit.Challenge{ID: id, Env: e.ID.String(), Host: host, Path: "//evil.example/x",
		Binding: visit.BindingHash(secret), IssuedAt: old, Expires: old + 600})
	must(t, err)
	ch, err = f.s.OpenChallenge(f.ctx, tenantCaller, late)
	if err != nil || !ch.Expired || ch.Restart != "https://"+host+"/" || ch.Environment.Labels == nil {
		t.Fatalf("expired %+v %v", ch, err)
	}
	_, err = f.s.AcceptChallenge(f.ctx, tenantCaller, Accept{Challenge: late, Subject: "u"})
	wantAPI(t, err, http.StatusConflict, CodeChallengeExpired, "restart")
	_, err = f.s.RejectChallenge(f.ctx, tenantCaller, late)
	wantAPI(t, err, http.StatusConflict, CodeChallengeExpired, "restart")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcceptChallenge(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	sealed, chSecret := f.challenge(e, host, "/menu?x=1")
	for _, bad := range []Accept{
		{Challenge: sealed},
		{Challenge: sealed, Subject: strings.Repeat("u", 256)},
		{Challenge: sealed, Subject: "u\n1"},
		{Challenge: sealed, Subject: "u", Email: strings.Repeat("e", 255)},
		{Challenge: sealed, Subject: "u", Name: strings.Repeat("n", 201)},
		{Challenge: sealed, Subject: "u", Session: strings.Repeat("s", 256)},
		{Challenge: sealed, Subject: "u", Name: "a\x00"},
		{Challenge: sealed, Subject: "u", EndsAt: "tomorrow"},
		{Challenge: sealed, Subject: "u", EndsAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)},
	} {
		_, err := f.s.AcceptChallenge(f.ctx, tenantCaller, bad)
		wantAPI(t, err, http.StatusBadRequest, CodeInvalidAccept, "")
	}
	ends := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	a, err := f.s.AcceptChallenge(f.ctx, tenantCaller, Accept{Challenge: sealed, Subject: "u-123",
		Email: "a@example.com", Name: "Ada", Session: "s-1", EndsAt: ends.Format(time.RFC3339)})
	if err != nil || !a.Admitted || a.Visit == "" {
		t.Fatalf("accept %+v %v", a, err)
	}
	code, err := f.keys().VerifyCode(f.codeOf(a, host))
	if err != nil || code.Outcome != visit.OutcomeAccepted || code.Visit != a.Visit || code.Env != e.ID.String() ||
		code.Host != host || code.Path != "/menu?x=1" || code.Binding != visit.BindingHash(chSecret) ||
		code.Expires-code.IssuedAt != int64(visit.CodeLifetime/time.Second) {
		t.Fatalf("code %+v %v", code, err)
	}
	v := f.row(uuid.MustParse(a.Visit))
	if v.Subject != "u-123" || v.Email != "a@example.com" || v.Name != "Ada" || v.Session != "s-1" ||
		!v.EndsAt.Equal(ends) || v.Provider != "fecode" || v.ProviderKind != "tenant" || v.Host != host ||
		v.Binding != "" || !v.Open() {
		t.Fatalf("visit %+v", v)
	}
	var by string
	must(t, f.d.Pool.QueryRow(f.ctx, "SELECT answered_by FROM visit_answers WHERE challenge_id = $1",
		code.Challenge).Scan(&by))
	if by != "token:abc" {
		t.Fatalf("answered by %q", by)
	}
	// Once: a second accept or a reject is a 409, and open says answered.
	_, err = f.s.AcceptChallenge(f.ctx, tenantCaller, Accept{Challenge: sealed, Subject: "u-123"})
	wantAPI(t, err, http.StatusConflict, CodeChallengeAnswered, "")
	_, err = f.s.RejectChallenge(f.ctx, tenantCaller, sealed)
	wantAPI(t, err, http.StatusConflict, CodeChallengeAnswered, "")
	if ch, err := f.s.OpenChallenge(f.ctx, tenantCaller, sealed); err != nil || !ch.Answered {
		t.Fatalf("answered %+v %v", ch, err)
	}

	// The end is at most 12 hours away; without one, 12 hours.
	s2, _ := f.challenge(e, host, "/")
	a, err = f.s.AcceptChallenge(f.ctx, tenantCaller, Accept{Challenge: s2, Subject: "u",
		EndsAt: time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)})
	must(t, err)
	if v := f.row(uuid.MustParse(a.Visit)); v.EndsAt.Sub(v.CreatedAt) != visit.MaxVisit {
		t.Fatalf("capped end %v", v.EndsAt.Sub(v.CreatedAt))
	}
	s3, _ := f.challenge(e, host, "/")
	a, err = f.s.AcceptChallenge(f.ctx, tenantCaller, Accept{Challenge: s3, Subject: "u"})
	must(t, err)
	if v := f.row(uuid.MustParse(a.Visit)); v.EndsAt.Sub(v.CreatedAt) != visit.MaxVisit {
		t.Fatalf("no end %v", v.EndsAt.Sub(v.CreatedAt))
	}

	// A reject: a rejected code, no visit.
	s4, _ := f.challenge(e, host, "/x")
	a, err = f.s.RejectChallenge(f.ctx, tenantCaller, s4)
	if err != nil || a.Admitted || a.Visit != "" {
		t.Fatalf("reject %+v %v", a, err)
	}
	if code, err := f.keys().VerifyCode(f.codeOf(a, host)); err != nil || code.Outcome != visit.OutcomeRejected ||
		code.Visit != "" {
		t.Fatalf("rejected code %+v %v", code, err)
	}
	var visits int
	must(t, f.d.Pool.QueryRow(f.ctx, "SELECT count(*) FROM visits").Scan(&visits))
	if visits != 3 {
		t.Fatalf("%d visits", visits)
	}
}

// Two instances accept one challenge at once: one wins.
func TestAcceptOnce(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	a := newFxOn(t, d, Config{AuthURL: testAuth, Issuer: testIssuer})
	b := newFxOn(t, d, Config{AuthURL: testAuth, Issuer: testIssuer})
	e, host := a.env("stage", "fecode")
	for range 5 {
		sealed, _ := a.challenge(e, host, "/")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, f := range []*fx{a, b} {
			wg.Go(func() {
				_, errs[i] = f.s.AcceptChallenge(f.ctx, tenantCaller, Accept{Challenge: sealed, Subject: "u"})
			})
		}
		wg.Wait()
		won := 0
		for _, err := range errs {
			if err == nil {
				won++
				continue
			}
			wantAPI(t, err, http.StatusConflict, CodeChallengeAnswered, "")
		}
		if won != 1 {
			t.Fatalf("%d accepts won: %v", won, errs)
		}
	}
}

// Kind ferun: ferund admits by grant, never makes a person.
func TestAcceptFerun(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "")
	p, err := f.st.UpsertPerson(f.ctx, testIssuer, "ada", "", "")
	must(t, err)
	sealed, _ := f.challenge(e, host, "/")
	a, err := f.s.AcceptChallenge(f.ctx, authCaller, Accept{Challenge: sealed, Subject: "ada", Session: "sid"})
	if err != nil || a.Admitted || a.Visit != "" {
		t.Fatalf("no grant: %+v %v", a, err)
	}
	if code, err := f.keys().VerifyCode(f.codeOf(a, host)); err != nil || code.Outcome != visit.OutcomeRejected {
		t.Fatalf("no grant's code %+v %v", code, err)
	}
	_, err = f.st.PutGrant(f.ctx, store.Grant{TenantID: testTenant, PersonID: p.ID, EnvironmentID: e.ID, Role: "read"})
	must(t, err)
	s2, _ := f.challenge(e, host, "/")
	if a, err := f.s.AcceptChallenge(f.ctx, authCaller, Accept{Challenge: s2, Subject: "nobody"}); err != nil ||
		a.Admitted {
		t.Fatalf("unknown subject: %+v %v", a, err)
	}
	var n int
	must(t, f.d.Pool.QueryRow(f.ctx, "SELECT count(*) FROM people").Scan(&n))
	if n != 1 {
		t.Fatalf("%d people", n)
	}
	tok, secret, vid := f.visitIn(e, host, authCaller, "ada")
	if v := f.row(vid); v.PersonID != p.ID || v.ProviderKind != "ferun" || v.Provider != "ferun" {
		t.Fatalf("visit %+v", v)
	}
	// The grant is looked up at every renewal, without waiting for an ask.
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeRenewed {
		t.Fatalf("renew %+v", r)
	}
	f.exec("DELETE FROM grants WHERE person_id = $1", p.ID)
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeEnded || r.Reason != model.VisitProviderNo {
		t.Fatalf("after the grant went %+v", r)
	}
	// A disabled person is refused at the accept.
	_, err = f.st.PutGrant(f.ctx, store.Grant{TenantID: testTenant, PersonID: p.ID, Role: "admin"})
	must(t, err)
	_, err = f.st.SetPersonDisabled(f.ctx, p.ID, true)
	must(t, err)
	s3, _ := f.challenge(e, host, "/")
	if a, err := f.s.AcceptChallenge(f.ctx, authCaller, Accept{Challenge: s3, Subject: "ada"}); err != nil || a.Admitted {
		t.Fatalf("disabled: %+v %v", a, err)
	}
	// Without the installation's issuer kind ferun is not served.
	g := newFxOn(t, f.d, Config{AuthURL: testAuth})
	s4, _ := g.challenge(e, host, "/")
	_, err = g.s.OpenChallenge(g.ctx, authCaller, s4)
	wantAPI(t, err, http.StatusNotFound, "", "no such challenge")
}

func TestRedeem(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	sealed, chSecret := f.challenge(e, host, "/menu?x=1")
	a, err := f.s.AcceptChallenge(f.ctx, tenantCaller, Accept{Challenge: sealed, Subject: "u-1", Email: "a@example.com"})
	must(t, err)
	code := f.codeOf(a, host)
	vs1, _ := visit.NewBindingSecret()
	vs2, _ := visit.NewBindingSecret()
	redeem := func(code, binding, visitBinding, host string) wire.RedeemVisitResponse {
		t.Helper()
		r, err := f.s.Redeem(f.ctx, wire.RedeemVisitRequest{Code: code, Binding: binding, VisitBinding: visitBinding,
			Host: host})
		if err != nil {
			t.Fatalf("redeem: %v", err)
		}
		return r
	}
	if _, err := f.s.Redeem(f.ctx, wire.RedeemVisitRequest{Code: code, Binding: chSecret, VisitBinding: "short",
		Host: host}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a malformed visit binding: %v", err)
	}
	// Refused before any write: another host, another browser, a forged
	// code.
	other, _ := visit.NewBindingSecret()
	for _, r := range []wire.RedeemVisitResponse{
		redeem(code, chSecret, visit.BindingHash(vs1), "other.apps.test"),
		redeem(code, other, visit.BindingHash(vs1), host),
		redeem(code, "bad", visit.BindingHash(vs1), host),
		redeem("x.y.z", chSecret, visit.BindingHash(vs1), host),
	} {
		if r.Outcome != wire.OutcomeRefused || r.Reason != "unknown" {
			t.Fatalf("refusal %+v", r)
		}
	}
	r := redeem(code, chSecret, visit.BindingHash(vs1), host)
	if r.Outcome != wire.OutcomeAdmitted {
		t.Fatalf("redeem %+v", r)
	}
	tok, err := f.keys().VerifyToken(r.Token)
	if err != nil || tok.Binding != visit.BindingHash(vs1) || tok.Visit != a.Visit || tok.Env != e.ID.String() ||
		tok.Host != host || tok.Provider != "fecode" || tok.Subject != "u-1" || tok.Email != "a@example.com" {
		t.Fatalf("token %+v %v", tok, err)
	}
	if l := tok.Expires - tok.IssuedAt; l != int64(visit.TokenLifetime/time.Second) {
		t.Fatalf("token lifetime %d", l)
	}
	vid := uuid.MustParse(a.Visit)
	if v := f.row(vid); v.Binding != visit.BindingHash(vs1) || tok.VisitEnds != v.EndsAt.Unix() {
		t.Fatalf("binding %+v", v)
	}
	// Again within the window, same binding: a token, and the visit takes
	// the last binding.
	r = redeem(code, chSecret, visit.BindingHash(vs2), host)
	if r.Outcome != wire.OutcomeAdmitted {
		t.Fatalf("again %+v", r)
	}
	if v := f.row(vid); v.Binding != visit.BindingHash(vs2) {
		t.Fatalf("the binding stayed %s", v.Binding)
	}
	// After the window: used.
	f.exec("UPDATE visit_answers SET redeemed_at = now() - interval '61 seconds'")
	if r := redeem(code, chSecret, visit.BindingHash(vs1), host); r.Outcome != wire.OutcomeRefused || r.Reason != "used" {
		t.Fatalf("after the window %+v", r)
	}

	// A code past its end: expired.
	s2, cs2 := f.challenge(e, host, "/")
	a2, err := f.s.AcceptChallenge(f.ctx, tenantCaller, Accept{Challenge: s2, Subject: "u"})
	must(t, err)
	f.exec("UPDATE visit_answers SET code_expires_at = now() - interval '1 second' WHERE visit_id = $1", a2.Visit)
	if r := redeem(f.codeOf(a2, host), cs2, visit.BindingHash(vs1), host); r.Outcome != wire.OutcomeRefused ||
		r.Reason != "expired" {
		t.Fatalf("expired code %+v", r)
	}
	// A rejected code: refused.
	s3, cs3 := f.challenge(e, host, "/")
	a3, err := f.s.RejectChallenge(f.ctx, tenantCaller, s3)
	must(t, err)
	if r := redeem(f.codeOf(a3, host), cs3, visit.BindingHash(vs1), host); r.Outcome != wire.OutcomeRefused {
		t.Fatalf("rejected code %+v", r)
	}
	// The environment unpublished before the redeem: ended.
	s4, cs4 := f.challenge(e, host, "/")
	a4, err := f.s.AcceptChallenge(f.ctx, tenantCaller, Accept{Challenge: s4, Subject: "u"})
	must(t, err)
	_, _, err = f.st.SetEnvironmentAccess(f.ctx, e.ID, model.AccessVisitors, "")
	must(t, err)
	if r := redeem(f.codeOf(a4, host), cs4, visit.BindingHash(vs1), host); r.Outcome != wire.OutcomeRefused ||
		r.Reason != "ended" {
		t.Fatalf("ended visit %+v", r)
	}
}

func TestRenew(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	tok, secret, vid := f.visitIn(e, host, tenantCaller, "u-1")
	before := f.row(vid)

	// A wrong or malformed secret: no visit in this cookie, the row as it
	// was.
	other, _ := visit.NewBindingSecret()
	if r := f.renew(tok, other); r.Outcome != wire.OutcomeEnded || r.Reason != "unknown" {
		t.Fatalf("wrong secret %+v", r)
	}
	if _, err := f.s.Renew(f.ctx, wire.RenewVisitRequest{Token: tok}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no secret: %v", err)
	}
	if _, err := f.s.Renew(f.ctx, wire.RenewVisitRequest{Token: "x.y.z", Secret: secret}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a forged token: %v", err)
	}
	if after := f.row(vid); !after.Open() || !after.AskedAt.Equal(before.AskedAt) || after.Binding != before.Binding {
		t.Fatalf("a refused renewal changed the row: %+v", after)
	}
	if f.prov.count() != 0 {
		t.Fatalf("%d asks", f.prov.count())
	}

	// 7:59 after the ask: no call; 8:01: one, with nothing but the visit.
	f.exec("UPDATE visits SET asked_at = now() - interval '7 minutes 59 seconds' WHERE id = $1", vid)
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeRenewed || f.prov.count() != 0 {
		t.Fatalf("7:59: %+v, %d asks", r, f.prov.count())
	}
	f.exec("UPDATE visits SET asked_at = now() - interval '8 minutes 1 second' WHERE id = $1", vid)
	r := f.renew(tok, secret)
	if r.Outcome != wire.OutcomeRenewed || f.prov.count() != 1 {
		t.Fatalf("8:01: %+v, %d asks", r, f.prov.count())
	}
	f.prov.mu.Lock()
	h, body := f.prov.header, f.prov.body
	f.prov.mu.Unlock()
	if body != `{"visit":"`+vid.String()+`"}` || h.Get("Content-Type") != "application/json" ||
		h.Get("Accept") != "application/json" || h.Get("User-Agent") != "ferund-visit" || h.Get("Authorization") != "" ||
		h.Get("Cookie") != "" {
		t.Fatalf("the ask: %s %v", body, h)
	}
	next, err := f.keys().VerifyToken(r.Token)
	if err != nil || next.Binding != visit.BindingHash(secret) || next.Visit != vid.String() {
		t.Fatalf("next token %+v %v", next, err)
	}
	// A token renewed without an ask lives at most 15 minutes past the
	// last one.
	f.exec("UPDATE visits SET asked_at = now() - interval '7 minutes' WHERE id = $1", vid)
	r = f.renew(tok, secret)
	v := f.row(vid)
	next, _ = f.keys().VerifyToken(r.Token)
	if f.prov.count() != 1 || next.Expires > v.AskedAt.Add(visit.TokenLifetime).Unix() ||
		next.Expires < v.AskedAt.Add(visit.TokenLifetime).Unix()-1 {
		t.Fatalf("a token past its last ask: exp %d, asked %v", next.Expires, v.AskedAt)
	}

	// A token past its exp, while the visit lives: renewed.
	keys := f.keys()
	old, err := keys.VerifyToken(tok)
	must(t, err)
	old.IssuedAt, old.Expires = time.Now().Add(-time.Hour).Unix(), time.Now().Add(-40*time.Minute).Unix()
	stale, err := keys.SignToken(old)
	must(t, err)
	if r := f.renew(stale, secret); r.Outcome != wire.OutcomeRenewed {
		t.Fatalf("a stale token %+v", r)
	}

	// The provider's no ends the visit, and renewals after say so.
	f.prov.set(func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"admitted": false}`)) })
	f.exec("UPDATE visits SET asked_at = now() - interval '9 minutes' WHERE id = $1", vid)
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeEnded || r.Reason != model.VisitProviderNo {
		t.Fatalf("provider's no %+v", r)
	}
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeEnded || r.Reason != model.VisitProviderNo ||
		f.prov.count() != 2 {
		t.Fatalf("after the no %+v, %d asks", r, f.prov.count())
	}
}

// Anything but 200 with admitted false is no answer: the visit lives on.
func TestRenewNoAnswer(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	tok, secret, vid := f.visitIn(e, host, tenantCaller, "u-1")
	answers := map[string]func(w http.ResponseWriter){
		"500": func(w http.ResponseWriter) { w.WriteHeader(500); _, _ = w.Write([]byte(`{"admitted": false}`)) },
		"404": func(w http.ResponseWriter) { w.WriteHeader(404); _, _ = w.Write([]byte(`{"admitted": false}`)) },
		"302": func(w http.ResponseWriter) {
			w.Header().Set("Location", "https://example.com/")
			w.WriteHeader(302)
		},
		"bad JSON": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"admitted": fals`)) },
		"a string": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"admitted": "false"}`)) },
		"an array": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`[false]`)) },
		"too long": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"admitted": false, "x": "` + strings.Repeat("x", 70<<10) + `"}`))
		},
		"timeout (4s)": func(w http.ResponseWriter) { time.Sleep(4 * time.Second) },
	}
	n := 0
	for name, answer := range answers {
		f.prov.set(answer)
		f.exec("UPDATE visits SET asked_at = now() - interval '9 minutes' WHERE id = $1", vid)
		if r := f.renew(tok, secret); r.Outcome != wire.OutcomeRenewed {
			t.Fatalf("%s: %+v", name, r)
		}
		n++
		if f.prov.count() != n {
			t.Fatalf("%s: %d asks", name, f.prov.count())
		}
		if !f.row(vid).Open() {
			t.Fatalf("%s ended the visit", name)
		}
	}
	// A provider at a denied address: no call, no answer, the visit kept.
	f.exec("UPDATE access_providers SET url = 'https://10.1.2.3/visit'")
	f.exec("UPDATE visits SET asked_at = now() - interval '9 minutes' WHERE id = $1", vid)
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeRenewed || f.prov.count() != n || !f.row(vid).Open() {
		t.Fatalf("denied address: %+v", r)
	}
}

// Renewals at once ask once.
func TestRenewOneAsk(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	tok, secret, vid := f.visitIn(e, host, tenantCaller, "u-1")
	gate := make(chan struct{})
	f.prov.mu.Lock()
	f.prov.gate = gate
	f.prov.mu.Unlock()
	f.exec("UPDATE visits SET asked_at = now() - interval '9 minutes' WHERE id = $1", vid)
	var wg sync.WaitGroup
	out := make([]wire.RenewVisitResponse, 6)
	for i := range out {
		wg.Go(func() {
			r, err := f.s.Renew(f.ctx, wire.RenewVisitRequest{Token: tok, Secret: secret})
			if err != nil {
				t.Error(err)
			}
			out[i] = r
		})
	}
	select {
	case <-f.prov.arrived:
	case <-time.After(10 * time.Second):
		close(gate)
		wg.Wait()
		t.Fatalf("no ask: %+v", out)
	}
	time.Sleep(200 * time.Millisecond)
	close(gate)
	wg.Wait()
	if f.prov.count() != 1 {
		t.Fatalf("%d asks", f.prov.count())
	}
	asked := f.row(vid).AskedAt
	for _, r := range out {
		next, err := f.keys().VerifyToken(r.Token)
		if r.Outcome != wire.OutcomeRenewed || err != nil || next.Expires > asked.Add(visit.TokenLifetime).Unix() {
			t.Fatalf("renewal %+v %v", r, err)
		}
	}
}

// A provider's no is recorded even when the ingress gave up on the
// renewal while the provider was still deciding.
func TestRenewNoAfterIngressGaveUp(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	tok, secret, vid := f.visitIn(e, host, tenantCaller, "u-1")
	gate := make(chan struct{})
	f.prov.mu.Lock()
	f.prov.gate = gate
	f.prov.answer = func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"admitted": false}`)) }
	f.prov.mu.Unlock()
	f.exec("UPDATE visits SET asked_at = now() - interval '9 minutes' WHERE id = $1", vid)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.s.Renew(ctx, wire.RenewVisitRequest{Token: tok, Secret: secret})
	}()
	select {
	case <-f.prov.arrived:
	case <-time.After(10 * time.Second):
		close(gate)
		<-done
		t.Fatal("no ask")
	}
	cancel()
	close(gate)
	<-done
	if v := f.row(vid); v.Open() || v.EndReason != model.VisitProviderNo {
		t.Fatalf("the no after the ingress gave up: %+v", v)
	}
}

func TestRenewEnds(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	tok, secret, vid := f.visitIn(e, host, tenantCaller, "u-1")
	// Unpublished meanwhile (by a release that does not end visits).
	f.exec("UPDATE environments SET public = false WHERE id = $1", e.ID)
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeEnded || r.Reason != model.VisitUnpublished {
		t.Fatalf("unpublished %+v", r)
	}
	if v := f.row(vid); v.EndReason != model.VisitUnpublished {
		t.Fatalf("row %+v", v)
	}
	f.exec("UPDATE environments SET public = true WHERE id = $1", e.ID)
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeEnded || r.Reason != model.VisitUnpublished {
		t.Fatalf("an ended visit %+v", r)
	}
	// Its time.
	tok, secret, vid = f.visitIn(e, host, tenantCaller, "u-2")
	f.exec("UPDATE visits SET ends_at = now() - interval '1 second' WHERE id = $1", vid)
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeEnded || r.Reason != model.VisitExpired {
		t.Fatalf("expired %+v", r)
	}
	// Another provider.
	tok, secret, _ = f.visitIn(e, host, tenantCaller, "u-3")
	f.exec("UPDATE environments SET access_provider = NULL WHERE id = $1", e.ID)
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeEnded || r.Reason != model.VisitProviderChanged {
		t.Fatalf("provider changed %+v", r)
	}
	if f.prov.count() != 0 {
		t.Fatalf("%d asks", f.prov.count())
	}
}

// Kind oidc renews without asking anyone.
func TestRenewOIDC(t *testing.T) {
	f := newFx(t)
	f.oidcProvider("sso", "s")
	e, host := f.env("stage", "sso")
	tok, secret, vid := f.visitIn(e, host, authCaller, "u-1")
	f.exec("UPDATE visits SET asked_at = now() - interval '1 hour' WHERE id = $1", vid)
	r := f.renew(tok, secret)
	next, err := f.keys().VerifyToken(r.Token)
	if r.Outcome != wire.OutcomeRenewed || err != nil || next.Expires-next.IssuedAt != int64(visit.TokenLifetime/time.Second) ||
		f.prov.count() != 0 {
		t.Fatalf("oidc %+v %v", r, err)
	}
	// Its session's back-channel logout ends it.
	if n, err := f.s.EndBySession(f.ctx, EndSession{Tenant: testTenant, Provider: "sso", Session: "s-1"}); err != nil ||
		n != 1 {
		t.Fatalf("end by session %d %v", n, err)
	}
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeEnded || r.Reason != model.VisitBackchannelLogout {
		t.Fatalf("after the logout %+v", r)
	}
}

func TestEnd(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	tok, secret, vid := f.visitIn(e, host, tenantCaller, "u-1")
	other, _ := visit.NewBindingSecret()
	for _, s := range []string{other, "", "bad"} {
		if _, err := f.s.End(f.ctx, wire.EndVisitRequest{Token: tok, Secret: s}); err != nil {
			t.Fatal(err)
		}
	}
	if !f.row(vid).Open() {
		t.Fatal("a wrong secret ended the visit")
	}
	if _, err := f.s.End(f.ctx, wire.EndVisitRequest{Token: "x", Secret: secret}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a forged token: %v", err)
	}
	for range 2 {
		if _, err := f.s.End(f.ctx, wire.EndVisitRequest{Token: tok, Secret: secret}); err != nil {
			t.Fatal(err)
		}
	}
	if v := f.row(vid); v.EndReason != model.VisitSignedOut {
		t.Fatalf("signed out %+v", v)
	}
	if r := f.renew(tok, secret); r.Outcome != wire.OutcomeEnded || r.Reason != model.VisitSignedOut {
		t.Fatalf("renew after %+v", r)
	}
}

func TestGetVisit(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	_, _, vid := f.visitIn(e, host, tenantCaller, "u-1")
	v, err := f.s.GetVisit(f.ctx, tenantCaller, vid.String())
	if err != nil || v.ID != vid.String() || v.Environment.Name != "stage" || v.Host != host || v.Subject != "u-1" ||
		v.Session != "s-1" || v.Provider != (ProviderRef{"fecode", "tenant"}) || v.EndedAt != "" || v.EndReason != "" ||
		v.CreatedAt == "" || v.EndsAt == "" {
		t.Fatalf("visit %+v %v", v, err)
	}
	for _, c := range []Caller{readerCaller, foreignCaller, authCaller} {
		_, err := f.s.GetVisit(f.ctx, c, vid.String())
		wantAPI(t, err, http.StatusNotFound, "", "no such visit")
	}
	for _, id := range []string{"nope", uuid.NewString()} {
		_, err := f.s.GetVisit(f.ctx, tenantCaller, id)
		wantAPI(t, err, http.StatusNotFound, "", "no such visit")
	}
	_, err = f.st.EndVisit(f.ctx, vid, model.VisitSignedOut)
	must(t, err)
	if v, err := f.s.GetVisit(f.ctx, tenantCaller, vid.String()); err != nil || v.EndReason != "signed_out" ||
		v.EndedAt == "" {
		t.Fatalf("ended %+v %v", v, err)
	}
}

func TestEndBySession(t *testing.T) {
	f := newFx(t)
	f.oidcProvider("sso", "s")
	fe, fhost := f.env("ferun-env", "")
	oe, ohost := f.env("oidc-env", "sso")
	p, err := f.st.UpsertPerson(f.ctx, testIssuer, "ada", "", "")
	must(t, err)
	_, err = f.st.PutGrant(f.ctx, store.Grant{TenantID: testTenant, PersonID: p.ID, Role: "read"})
	must(t, err)
	_, _, fv := f.visitIn(fe, fhost, authCaller, "ada")
	_, _, ov := f.visitIn(oe, ohost, authCaller, "u")
	for _, bad := range []EndSession{
		{Provider: "ferun"},
		{Session: "s-1"},
		{Tenant: testTenant, Provider: "ferun", Session: "s-1"},
		{Provider: "sso", Session: "s-1"},
	} {
		_, err := f.s.EndBySession(f.ctx, bad)
		wantAPI(t, err, http.StatusBadRequest, CodeInvalidRequest, "")
	}
	for _, nf := range []EndSession{
		{Tenant: testTenant, Provider: "fecode", Session: "s-1"},
		{Tenant: testTenant, Provider: "nope", Session: "s-1"},
	} {
		_, err := f.s.EndBySession(f.ctx, nf)
		wantAPI(t, err, http.StatusNotFound, "", "no such provider")
	}
	if n, err := f.s.EndBySession(f.ctx, EndSession{Provider: "ferun", Session: "s-1"}); err != nil || n != 1 {
		t.Fatalf("ferun %d %v", n, err)
	}
	if f.row(fv).EndReason != model.VisitBackchannelLogout || !f.row(ov).Open() {
		t.Fatal("the wrong visits ended")
	}
	if n, err := f.s.EndBySession(f.ctx, EndSession{Tenant: testTenant, Provider: "sso", Session: "s-1"}); err != nil ||
		n != 1 || f.row(ov).EndReason != model.VisitBackchannelLogout {
		t.Fatalf("oidc %d %v", n, err)
	}
}

func TestSweep(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	_, _, vid := f.visitIn(e, host, tenantCaller, "u-1")
	f.exec("UPDATE visits SET ends_at = now() - interval '1 minute' WHERE id = $1", vid)
	must(t, f.s.Loops()[0].Run(f.ctx))
	if v := f.row(vid); v.EndReason != model.VisitExpired {
		t.Fatalf("swept %+v", v)
	}
	f.exec("UPDATE visits SET ended_at = now() - interval '25 hours', ends_at = now() - interval '25 hours'")
	f.exec("UPDATE visit_answers SET expires_at = now() - interval '2 hours'")
	must(t, f.s.Loops()[0].Run(f.ctx))
	var n int
	must(t, f.d.Pool.QueryRow(f.ctx, "SELECT (SELECT count(*) FROM visits) + (SELECT count(*) FROM visit_answers)").Scan(&n))
	if n != 0 {
		t.Fatalf("%d rows left", n)
	}
}
