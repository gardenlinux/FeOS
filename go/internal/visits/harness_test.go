// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visits

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/egress"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/visit"
)

// The tests' installation.
const (
	testTenant = "acme"
	testIssuer = "https://id.test/"
	testAuth   = "https://auth.test"
)

// denyButLoopback is the built-in deny set without 127/8, so that the
// tests' providers on loopback are reachable and every other block is
// not.
func denyButLoopback() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range egress.DefaultDeny() {
		if p.Addr() != netip.MustParseAddr("127.0.0.0") {
			out = append(out, p)
		}
	}
	return out
}

// fakeProvider is a tenant's provider on an httptest TLS server: it
// answers with answer (200 {"admitted": true} when nil), after gate (when
// set) lets it, and counts its calls.
type fakeProvider struct {
	srv *httptest.Server

	mu      sync.Mutex
	calls   int
	header  http.Header
	body    string
	answer  func(w http.ResponseWriter)
	gate    chan struct{}
	arrived chan struct{}
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{arrived: make(chan struct{}, 64)}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1024)
		n, _ := r.Body.Read(b)
		p.mu.Lock()
		p.calls++
		p.header, p.body = r.Header.Clone(), string(b[:n])
		answer, gate := p.answer, p.gate
		p.mu.Unlock()
		p.arrived <- struct{}{}
		if gate != nil {
			<-gate
		}
		if answer == nil {
			_, _ = w.Write([]byte(`{"admitted": true}`))
			return
		}
		answer(w)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProvider) url() string { return p.srv.URL + "/api/v1/visit" }

func (p *fakeProvider) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(p.srv.Certificate())
	return pool
}

func (p *fakeProvider) set(answer func(w http.ResponseWriter)) {
	p.mu.Lock()
	p.answer = answer
	p.mu.Unlock()
}

func (p *fakeProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// fx is a visits service over a fresh database, with a tenant, its
// provider "fecode" of kind tenant at a fake provider, and the
// installation's keys as the ingress holds them.
type fx struct {
	t      *testing.T
	ctx    context.Context
	d      *db.DB
	st     *store.Store
	sealer *seal.Sealer
	s      *Service
	prov   *fakeProvider
}

func newFx(t *testing.T) *fx {
	t.Helper()
	t.Parallel()
	return newFxOn(t, dbtest.New(t), Config{AuthURL: testAuth, Issuer: testIssuer})
}

// newFxOn is a service on d with cfg (its zones, deny set and log set by
// the harness when cfg leaves them out).
func newFxOn(t *testing.T, d *db.DB, cfg Config) *fx {
	t.Helper()
	st := store.New(d)
	sl, err := seal.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Zones.Apps == "" {
		cfg.Zones = model.Zones{Apps: "apps.test"}
	}
	if cfg.Deny == nil {
		cfg.Deny = denyButLoopback()
	}
	if cfg.Log == nil {
		cfg.Log = dbtest.Logger(t)
	}
	f := &fx{t: t, ctx: t.Context(), d: d, st: st, sealer: sl, prov: newFakeProvider(t)}
	if f.s, err = newService(cfg, Deps{Store: st, Sealer: sl}, f.prov.roots()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateTenant(f.ctx, testTenant); err != nil && !errors.Is(err, store.ErrExists) {
		t.Fatal(err)
	}
	if _, err := st.GetAccessProvider(f.ctx, testTenant, "fecode"); errors.Is(err, store.ErrNoRow) {
		if _, err := st.InsertAccessProvider(f.ctx, store.AccessProvider{Tenant: testTenant, Name: "fecode",
			Kind: model.ProviderKindTenant, URL: f.prov.url()}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fx) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.d.Pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// oidcProvider inserts an oidc provider of the tenant, its secret sealed.
func (f *fx) oidcProvider(name, secret string) {
	f.t.Helper()
	sealed, err := f.sealer.Seal([]byte(secret), store.AccessProviderAAD(testTenant, name))
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.st.InsertAccessProvider(f.ctx, store.AccessProvider{Tenant: testTenant, Name: name,
		Kind: model.ProviderKindOIDC, Issuer: "https://login.example/", ClientID: "client", ClientSecretSealed: sealed,
		Rule: []byte(`{"domains":["example.com"]}`)}); err != nil {
		f.t.Fatal(err)
	}
}

// env makes a public environment of the tenant guarded by provider ("":
// ferun) and returns it with its host.
func (f *fx) env(name, provider string) (store.Environment, string) {
	f.t.Helper()
	e, err := f.st.InsertEnvironment(f.ctx, store.NewEnvironment{ID: uuid.New(), Owner: testTenant, Name: name,
		Labels: map[string]string{"fecode.project": "p1"}})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.st.SetEnvironmentPublic(f.ctx, e.ID, true); err != nil {
		f.t.Fatal(err)
	}
	if e, _, err = f.st.SetEnvironmentAccess(f.ctx, e.ID, model.AccessVisitors, provider); err != nil {
		f.t.Fatal(err)
	}
	return e, e.HostLabel + ".apps.test"
}

// keys are the installation's keys.
func (f *fx) keys() *visit.Keys {
	f.t.Helper()
	k, err := f.s.Keys(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return k
}

// challenge seals a challenge as the ingress does, for a navigation to
// path on host, and returns it with the challenge cookie's secret.
func (f *fx) challenge(e store.Environment, host, path string) (string, string) {
	f.t.Helper()
	secret, err := visit.NewBindingSecret()
	if err != nil {
		f.t.Fatal(err)
	}
	id, err := visit.NewChallengeID()
	if err != nil {
		f.t.Fatal(err)
	}
	now := time.Now().Unix()
	sealed, err := f.keys().SealChallenge(visit.Challenge{ID: id, Env: e.ID.String(), Host: host, Path: path,
		Binding: visit.BindingHash(secret), IssuedAt: now, Expires: now + int64(visit.ChallengeLifetime/time.Second)})
	if err != nil {
		f.t.Fatal(err)
	}
	return sealed, secret
}

// The callers: a tenant principal that is admin on every environment,
// one that is admin on none, and ferun-auth.
var (
	tenantCaller  = Caller{Tenant: testTenant, Admin: func(uuid.UUID) bool { return true }, MadeBy: "token:abc"}
	readerCaller  = Caller{Tenant: testTenant, Admin: func(uuid.UUID) bool { return false }, MadeBy: "person:p"}
	foreignCaller = Caller{Tenant: "other", Admin: func(uuid.UUID) bool { return true }, MadeBy: "token:o"}
	authCaller    = Caller{Auth: true, MadeBy: "service:auth"}
)

// codeOf is the code of an answer's redirect, checked for its host.
func (f *fx) codeOf(a Answer, host string) string {
	f.t.Helper()
	u, err := url.Parse(a.Redirect)
	if err != nil || u.Scheme != "https" || u.Host != host || u.Path != "/.ferun/auth" {
		f.t.Fatalf("redirect %q", a.Redirect)
	}
	return u.Query().Get("code")
}

// visitIn is a visit made by the whole sign-in: a challenge accepted by
// c with the subject, its code redeemed with the challenge cookie and a
// fresh visit secret. It returns the token, the visit cookie's secret and
// the visit.
func (f *fx) visitIn(e store.Environment, host string, c Caller, subject string) (string, string, uuid.UUID) {
	f.t.Helper()
	sealed, chSecret := f.challenge(e, host, "/menu?x=1")
	a, err := f.s.AcceptChallenge(f.ctx, c, Accept{Challenge: sealed, Subject: subject, Email: "a@example.com",
		Name: "Ada", Session: "s-1"})
	if err != nil || !a.Admitted {
		f.t.Fatalf("accept %+v %v", a, err)
	}
	secret, _ := visit.NewBindingSecret()
	r, err := f.s.Redeem(f.ctx, wire.RedeemVisitRequest{Code: f.codeOf(a, host), Binding: chSecret,
		VisitBinding: visit.BindingHash(secret), Host: host})
	if err != nil || r.Outcome != wire.OutcomeAdmitted {
		f.t.Fatalf("redeem %+v %v", r, err)
	}
	return r.Token, secret, uuid.MustParse(a.Visit)
}

// renew renews a token with the visit cookie's secret.
func (f *fx) renew(tok, secret string) wire.RenewVisitResponse {
	f.t.Helper()
	r, err := f.s.Renew(f.ctx, wire.RenewVisitRequest{Token: tok, Secret: secret})
	if err != nil {
		f.t.Fatalf("renew: %v", err)
	}
	return r
}

// row reads a visit.
func (f *fx) row(id uuid.UUID) store.Visit {
	f.t.Helper()
	v, err := f.st.GetVisit(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// wantAPI checks an error's status, code and text.
func wantAPI(t *testing.T, err error, status int, code, msg string) {
	t.Helper()
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != status || ae.Code != code || !strings.Contains(ae.Msg, msg) {
		t.Fatalf("error %v, want %d %q %q", err, status, code, msg)
	}
}
