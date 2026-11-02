// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package acme

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	xacme "golang.org/x/crypto/acme"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/ingress/proxy"
	"github.com/gardenlinux/feos/go/internal/ingress/table"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/seal"
	"github.com/gardenlinux/feos/go/internal/store"
)

var bg = context.Background()

// fixture is a database with the tenant acme and its public environment
// web at deployment 1, a sealer and a recording Refresher.
type fixture struct {
	t   *testing.T
	d   *db.DB
	st  *store.Store
	sl  *seal.Sealer
	env store.Environment
	ref *refresher
}

// pebble is the ACME directory of FERUN_TEST_ACME_DIRECTORY, Let's
// Encrypt's test server Pebble (hack/go/with-pebble), and a client that
// trusts its certificate unchecked; the test is skipped without it.
// Pebble accepts every challenge without fetching it, so the tests check
// the ingress's side themselves: the challenge reaches every instance's
// port 80 before it is accepted.
func pebble(t *testing.T) (string, *http.Client) {
	t.Helper()
	dir := os.Getenv("FERUN_TEST_ACME_DIRECTORY")
	if dir == "" {
		t.Skip("FERUN_TEST_ACME_DIRECTORY is not set (hack/go/with-pebble runs Pebble)")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // Pebble's own test certificate
	return dir, &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

type refresher struct {
	mu  sync.Mutex
	ids []uuid.UUID
}

func (r *refresher) Refresh(_ context.Context, id uuid.UUID) error {
	r.mu.Lock()
	r.ids = append(r.ids, id)
	r.mu.Unlock()
	return nil
}

func (r *refresher) got() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ids)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Parallel()
	d := dbtest.New(t)
	k := make([]byte, seal.KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	sl, err := seal.New(k)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, d: d, st: store.New(d), sl: sl, ref: &refresher{}}
	if _, _, err := f.st.CreateTenant(bg, "acme"); err != nil {
		t.Fatal(err)
	}
	f.env = f.mkEnv("web", true)
	return f
}

// mkEnv makes an environment of acme at deployment 1, public when public.
func (f *fixture) mkEnv(name string, public bool) store.Environment {
	f.t.Helper()
	e, err := f.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: "acme", Name: name})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.st.InsertDeployment(bg, store.EnvDeployment{EnvID: e.ID, Number: 1, Origin: model.OriginApply,
		Spec: json.RawMessage(`{"services": {"web": {"runtime": "image", "image": "web@sha256:1",
			"ports": [{"name": "http", "port": 8080}]}}, "routes": {"root": {"path": "/", "service": "web", "port": "http"}}}`)}); err != nil {
		f.t.Fatal(err)
	}
	after := e
	after.Deployment = 1
	if e, err = f.st.UpdateEnvironment(bg, e, after); err != nil {
		f.t.Fatal(err)
	}
	if public {
		if e, err = f.st.SetEnvironmentPublic(bg, e.ID, true); err != nil {
			f.t.Fatal(err)
		}
	}
	return e
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.d.Pool.Exec(bg, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// mkDomain inserts a domain of acme as the core keeps it once it is due
// for its first certificate: verified, bound to the environment web,
// routed, issuing; sets, when not empty, is an UPDATE's SET clause
// applied after.
func (f *fixture) mkDomain(name, sets string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO domains (id, owner, name, token, env_id, bound_at, verification, verified_at, routed, was_routed, state)
		VALUES ($1, 'acme', $2, 'abcdefghijklmnopqrstuvwxyz234567', $3, now(), 'verified', now(), true, true, 'issuing')`,
		id, name, f.env.ID)
	if sets != "" {
		f.exec("UPDATE domains SET "+sets+" WHERE id = $1", id)
	}
	return id
}

// domainState is what the ACME side keeps in a domain's row, with
// next_issue_at as seconds from now.
type domainState struct {
	err      string
	backoff  int
	nextIn   float64 // -1: NULL
	failing  bool
	challeng int
}

func (f *fixture) state(id uuid.UUID) domainState {
	f.t.Helper()
	var s domainState
	if err := f.d.Pool.QueryRow(bg, `SELECT issue_error, issue_backoff_s,
			coalesce(extract(epoch FROM next_issue_at - now())::float8, -1), issue_failing_since IS NOT NULL,
			(SELECT count(*) FROM acme_challenges c WHERE c.domain_id = d.id)
		FROM domains d WHERE id = $1`, id).Scan(&s.err, &s.backoff, &s.nextIn, &s.failing, &s.challeng); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// issuer returns an issuer against Pebble, for six-day certificates,
// with a delivery wait that a loaded test machine keeps.
func (f *fixture) issuer(mod ...func(c *Config)) *Issuer {
	f.t.Helper()
	dir, client := pebble(f.t)
	cfg := Config{Directory: dir, Email: "ops@fecore.dev", Profile: "shortlived", HTTPClient: client,
		DeliveryWait: 10 * time.Second, Log: dbtest.Logger(f.t)}
	for _, m := range mod {
		m(&cfg)
	}
	s, err := New(cfg, Deps{Store: f.st, Sealer: f.sl, Refresher: f.ref})
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) run(s *Issuer) {
	f.t.Helper()
	if err := s.Run(bg); err != nil {
		f.t.Fatalf("run: %v", err)
	}
}

var nextSubnet = struct {
	sync.Mutex
	n int
}{}

// mkIngressRow inserts an ingress instance that is joined, on the mesh
// and seen now, as its Hello leaves it.
func (f *fixture) mkIngressRow(name string) {
	f.t.Helper()
	nextSubnet.Lock()
	nextSubnet.n++
	n := nextSubnet.n
	nextSubnet.Unlock()
	f.exec(`INSERT INTO ingresses (name, mesh_subnet, mesh_prefix, mesh_address, last_seen)
		VALUES ($1, $2, $3, $4, now())`, name, n, fmt.Sprintf("fd00:0:0:%x::/64", n), fmt.Sprintf("fd00:0:0:%x::1", n))
}

// fakeIngress is an ingress instance as far as ACME sees it: the table
// builder of ferund, the stream's part that hands the table to the proxy
// and records the challenges it holds, and the proxy's port 80. served
// are the challenges its port 80 answered when they were recorded.
type fakeIngress struct {
	name   string
	b      *table.Builder
	p      *proxy.Proxy
	mu     sync.Mutex
	served map[string]string // token: key authorization
}

// answered is how many challenges its port 80 answered.
func (in *fakeIngress) answered() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.served)
}

func (f *fixture) startIngress(name string) *fakeIngress {
	f.t.Helper()
	f.mkIngressRow(name)
	b := table.NewBuilder(f.d, f.st, "apps.example", dbtest.Logger(f.t))
	b.Debounce, b.Full, b.Sealer = 10*time.Millisecond, time.Hour, f.sl
	in := &fakeIngress{name: name, b: b, p: proxy.New(proxy.Config{}, proxy.Deps{}), served: map[string]string{}}
	ctx, cancel := context.WithCancel(bg)
	var wg sync.WaitGroup
	f.t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = b.Run(ctx)
	}()
	release := b.Use()
	go func() {
		defer wg.Done()
		defer release()
		var recorded []string
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			ch := b.Changed()
			if snap := b.Current(); snap != nil {
				s := wire.NewState()
				s.Envs, s.Challenges = snap.Envs, snap.Challenges
				in.p.SetState(s)
				tokens := []string{}
				for _, c := range snap.Challenges {
					tokens = append(tokens, c.Token)
					// What the CA would fetch, before the instance
					// says it holds the challenge.
					if k, err := in.validate(c.Host, c.Token); err == nil {
						in.mu.Lock()
						in.served[c.Token] = k
						in.mu.Unlock()
					}
				}
				if recorded == nil || !slices.Equal(tokens, recorded) {
					if err := f.st.RecordIngressChallenges(ctx, name, tokens); err == nil {
						recorded = tokens
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ch:
			case <-tick.C:
				_ = f.st.SeenIngress(ctx, name, "")
			}
		}
	}()
	return in
}

// validate fetches a challenge's answer from the ingress's port 80, as
// the CA does over HTTP.
func (in *fakeIngress) validate(host, token string) (string, error) {
	rec := httptest.NewRecorder()
	in.p.ServeHTTPRedirect().ServeHTTP(rec, httptest.NewRequest("GET", "http://"+host+"/.well-known/acme-challenge/"+token, nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 {
		return "", fmt.Errorf("%d %s", rec.Code, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

// certOf reads the domain's certificate and checks it: its chain parses,
// its leaf is for name, its sealed key opens with the domain's additional
// data and is the leaf's, renew_after is at half its lifetime.
func (f *fixture) certOf(id uuid.UUID, name string) (store.DomainCert, *x509.Certificate) {
	f.t.Helper()
	c, err := f.st.GetDomainCert(bg, id)
	if err != nil {
		f.t.Fatalf("certificate of %s: %v", name, err)
	}
	if len(c.Chain) < 2 {
		f.t.Fatalf("chain of %d", len(c.Chain))
	}
	leaf, err := x509.ParseCertificate(c.Chain[0])
	if err != nil || leaf.VerifyHostname(name) != nil {
		f.t.Fatalf("leaf %v %v", leaf, err)
	}
	der, err := f.sl.Open(c.KeySealed, store.DomainCertAAD(id))
	if err != nil {
		f.t.Fatalf("the key does not open: %v", err)
	}
	if _, err := f.sl.Open(c.KeySealed, store.DomainCertAAD(uuid.New())); err == nil {
		f.t.Fatal("the key opens with another domain's additional data")
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		f.t.Fatal(err)
	}
	if !k.(crypto.Signer).Public().(interface{ Equal(crypto.PublicKey) bool }).Equal(leaf.PublicKey) {
		f.t.Fatal("the key is not the leaf's")
	}
	want := leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) / 2)
	if d := c.RenewAfter.Sub(want); d < -time.Second || d > time.Second || !c.NotAfter.Equal(leaf.NotAfter) ||
		c.Serial != strings.ToLower(leaf.SerialNumber.Text(16)) {
		f.t.Fatalf("renew_after %v (want %v), not_after %v, serial %s", c.RenewAfter, want, c.NotAfter, c.Serial)
	}
	return c, leaf
}

// An order end to end: the account is registered once, the challenge
// reaches the ingress's table and its port 80 before the CA validates
// it, and the six-day certificate of the profile shortlived is kept with
// its key sealed and renew_after at half its lifetime; the challenge is
// gone and the core is told. Once the core serves it and its renewal is
// due, a restarted instance with another contact reuses the account,
// updates the contact, renews, and the ingress's table carries the new
// certificate.
func TestOrder(t *testing.T) {
	f := newFixture(t)
	in := f.startIngress("in-0")
	id := f.mkDomain("dashboard.fecore.dev", "")

	s := f.issuer()
	f.run(s)
	first, leaf := f.certOf(id, "dashboard.fecore.dev")
	if got := f.state(id); got != (domainState{nextIn: -1}) {
		t.Fatalf("state after success %+v", got)
	}
	if got := f.ref.got(); !slices.Equal(got, []uuid.UUID{id}) {
		t.Fatalf("refreshed %v", got)
	}
	if n := in.answered(); n != 1 {
		t.Fatalf("port 80 answered %d challenges", n)
	}
	dir, _ := pebble(t)
	a, err := f.st.GetACMEAccount(bg, dir)
	if err != nil || a.URI == "" || a.URI != s.accountURI() || a.Email != "ops@fecore.dev" {
		t.Fatalf("account %+v (client %s) %v", a, s.accountURI(), err)
	}
	if life := leaf.NotAfter.Sub(leaf.NotBefore); life < 6*24*time.Hour-time.Minute || life > 7*24*time.Hour {
		t.Fatalf("lifetime %v, want the six days of shortlived", life)
	}

	// Nothing is due now.
	f.run(s)
	if again, _ := f.certOf(id, "dashboard.fecore.dev"); again.Serial != first.Serial {
		t.Fatal("a second run ordered again")
	}

	// The core serves it; the renewal comes due. Another instance (a
	// restart) with another contact.
	f.exec(`UPDATE domains SET state = 'served' WHERE id = $1`, id)
	f.exec(`UPDATE domain_certs SET renew_after = now() - interval '1 second' WHERE domain_id = $1`, id)
	s2 := f.issuer(func(c *Config) { c.Email = "certs@fecore.dev" })
	f.run(s2)
	second, _ := f.certOf(id, "dashboard.fecore.dev")
	if second.Serial == first.Serial || bytes.Equal(second.KeySealed, first.KeySealed) {
		t.Fatal("the renewal kept the certificate")
	}
	if s2.accountURI() != a.URI {
		t.Fatalf("a second account %s, want %s", s2.accountURI(), a.URI)
	}
	if a, _ := f.st.GetACMEAccount(bg, dir); a.Email != "certs@fecore.dev" {
		t.Fatalf("account email %q", a.Email)
	}
	// The ingress's table: the served domain with the renewed chain.
	deadline := time.Now().Add(5 * time.Second)
	for {
		d := in.b.Current().Envs[f.env.ID.String()].Domains
		if len(d) == 1 && bytes.Equal(d[0].Chain[0], second.Chain[0]) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the table's domains %+v", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Failures record a readable issue_error, a backoff of an hour doubling
// to six (a six-day certificate has three days left when its renewal
// starts), next_issue_at and issue_failing_since, delete the challenge
// and tell the core; a domain is not ordered again before next_issue_at;
// a success clears it all. A profile the CA does not list fails the
// order before anything is asked of the ingress.
func TestOrderFailures(t *testing.T) {
	f := newFixture(t)
	id := f.mkDomain("dashboard.fecore.dev", "")
	s := f.issuer(func(c *Config) { c.DeliveryWait = 200 * time.Millisecond })

	// No ingress instance to answer the challenge.
	f.run(s)
	got := f.state(id)
	if got.err != errNoIngress.Error() || got.backoff != 3600 || got.nextIn < 3590 || got.nextIn > 3610 || !got.failing ||
		got.challeng != 0 {
		t.Fatalf("after a failed order %+v", got)
	}
	if !slices.Equal(f.ref.got(), []uuid.UUID{id}) {
		t.Fatalf("refreshed %v", f.ref.got())
	}
	if _, err := f.st.GetDomainCert(bg, id); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("a certificate after a failure: %v", err)
	}
	f.run(s)
	if n := len(f.ref.got()); n != 1 {
		t.Fatalf("ordered again before next_issue_at: %d", n)
	}
	f.exec(`UPDATE domains SET next_issue_at = now() - interval '1 second' WHERE id = $1`, id)
	f.run(s)
	if got := f.state(id); got.backoff != 7200 || got.nextIn < 7190 || got.nextIn > 7210 {
		t.Fatalf("the second failure %+v", got)
	}
	f.exec(`UPDATE domains SET next_issue_at = now() - interval '1 second', issue_backoff_s = 14400 WHERE id = $1`, id)
	f.run(s)
	if got := f.state(id); got.backoff != 21600 || got.nextIn < 21590 || got.nextIn > 21610 {
		t.Fatalf("at the cap %+v", got)
	}

	// A profile the CA does not list.
	other := f.mkDomain("www.fecore.dev", "")
	bad := f.issuer(func(c *Config) { c.Profile = "eternal" })
	f.run(bad)
	if got := f.state(other); !strings.Contains(got.err, "eternal") || got.backoff != 3600 {
		t.Fatalf("an unknown profile %+v", got)
	}

	// An ingress answers now: the next order succeeds and clears the
	// failure.
	f.startIngress("in-0")
	f.exec(`UPDATE domains SET next_issue_at = now() - interval '1 second' WHERE id = $1`, id)
	f.run(f.issuer())
	f.certOf(id, "dashboard.fecore.dev")
	if got := f.state(id); got != (domainState{nextIn: -1}) {
		t.Fatalf("after the success %+v", got)
	}
}

// A challenge is accepted only once every ingress instance connected
// holds it: with none connected, or one connected that does not take it
// within the wait, the order fails without asking the CA to validate;
// an instance that left does not count.
func TestOrderWaitsForTheIngress(t *testing.T) {
	f := newFixture(t)
	s := f.issuer(func(c *Config) { c.DeliveryWait = 500 * time.Millisecond })
	id := f.mkDomain("dashboard.fecore.dev", "")
	f.run(s)
	if got := f.state(id); got.err != errNoIngress.Error() || got.challeng != 0 {
		t.Fatalf("without an ingress %+v", got)
	}

	f.mkIngressRow("in-stuck")
	in := f.startIngress("in-1")
	f.exec(`UPDATE domains SET next_issue_at = NULL WHERE id = $1`, id)
	f.run(s)
	if got := f.state(id); got.err != "the ingress instances in-stuck did not get the HTTP-01 challenge within 500ms" {
		t.Fatalf("with an instance that does not take it %+v", got)
	}

	f.exec(`UPDATE ingresses SET left_at = now() WHERE name = 'in-stuck'`)
	f.exec(`UPDATE domains SET next_issue_at = NULL WHERE id = $1`, id)
	f.run(f.issuer())
	f.certOf(id, "dashboard.fecore.dev")
	if in.answered() == 0 {
		t.Fatal("accepted before the ingress's port 80 answered it")
	}
}

// The candidates are exactly the contract's: verified, bound to a public
// environment, routed, the TXT record not missing, next_issue_at passed,
// and issuing or failed without a valid certificate, or a certificate
// past its renew_after; new domains before renewals. A claim holds the
// domain.
func TestCandidates(t *testing.T) {
	f := newFixture(t)
	hidden := f.mkEnv("hidden", false)
	cert := func(id uuid.UUID, notAfter, renewAfter string) {
		f.exec(`INSERT INTO domain_certs (domain_id, chain, key_sealed, not_before, not_after, renew_after, serial)
			VALUES ($1, ARRAY['\x01'::bytea], '\x02', now() - interval '60 days', now() + $2::interval, now() + $3::interval, '1')`,
			id, notAfter, renewAfter)
	}
	f.mkDomain("issuing.example", "")
	f.mkDomain("failed.example", "state = 'failed', issue_error = 'x', next_issue_at = now() - interval '1 second'")
	renew := f.mkDomain("renew.example", "state = 'served'")
	cert(renew, "30 days", "-1 hour")
	cert(f.mkDomain("fresh.example", "state = 'served'"), "60 days", "30 days")
	expired := f.mkDomain("expired.example", "state = 'issuing'")
	cert(expired, "-1 hour", "-30 days")
	f.mkDomain("unverified.example", "verification = 'unverified'")
	f.mkDomain("unbound.example", "env_id = NULL, state = 'unbound'")
	f.mkDomain("unrouted.example", "routed = false")
	f.mkDomain("missing.example", "record_missing_since = now()")
	f.mkDomain("later.example", "next_issue_at = now() + interval '1 minute'")
	f.mkDomain("hidden.example", "env_id = '"+hidden.ID.String()+"', state = 'unpublished'")
	f.mkDomain("waiting.example", "state = 'waiting-for-dns'")
	cert(f.mkDomain("stale.example", "state = 'issuing'"), "30 days", "20 days")

	var got []string
	for {
		o, ok, err := f.st.ClaimACMEOrder(bg, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got = append(got, o.Name)
		if o.Renewal != (o.Name == "renew.example") {
			t.Errorf("%s: renewal %v", o.Name, o.Renewal)
		}
		if len(got) > 10 {
			t.Fatal("the claim does not hold")
		}
	}
	slices.Sort(got[:3])
	if !slices.Equal(got, []string{"expired.example", "failed.example", "issuing.example", "renew.example"}) {
		t.Fatalf("candidates %v", got)
	}
	if st := f.state(renew); st.nextIn < 3590 || st.nextIn > 3610 {
		t.Fatalf("the claim's next_issue_at in %v s", st.nextIn)
	}
}

// A served domain whose renewals have failed for a day gets the incident
// certificate_failing on its environment, once; one failing for less
// does not; a renewal that succeeds closes it.
func TestIncident(t *testing.T) {
	f := newFixture(t)
	failing := f.mkDomain("dashboard.fecore.dev", `state = 'served', issue_error = 'rate limited by the CA: slow down',
		issue_backoff_s = 86400, issue_failing_since = now() - interval '25 hours', next_issue_at = now() + interval '1 hour'`)
	recent := f.mkDomain("www.fecore.dev", `state = 'served', issue_error = 'x', issue_backoff_s = 3600,
		issue_failing_since = now() - interval '1 hour', next_issue_at = now() + interval '1 hour'`)
	for _, id := range []uuid.UUID{failing, recent} {
		f.exec(`INSERT INTO domain_certs (domain_id, chain, key_sealed, not_before, not_after, renew_after, serial)
			VALUES ($1, ARRAY['\x01'::bytea], '\x02', now() - interval '80 days', now() + interval '10 days',
				now() - interval '20 days', '1')`, id)
	}
	s := f.issuer()
	f.run(s)
	f.run(s)
	open, err := f.st.ListIncidents(bg, f.env.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Object != "domain/dashboard.fecore.dev" || open[0].Condition != ConditionCertificateFailing ||
		open[0].Deployment != f.env.Deployment || !strings.Contains(open[0].Summary, "rate limited by the CA: slow down") ||
		!strings.Contains(open[0].Summary, "has not been renewed since") {
		t.Fatalf("incidents %+v", open)
	}

	f.startIngress("in-0")
	f.exec(`UPDATE domains SET next_issue_at = NULL WHERE id = $1`, failing)
	f.run(s)
	f.certOf(failing, "dashboard.fecore.dev")
	if open, _ := f.st.ListIncidents(bg, f.env.ID, true); len(open) != 0 {
		t.Fatalf("open after the renewal %+v", open)
	}
}

// Two instances that need the account at once register one: the
// directory's row is the lock, and the loser takes the winner's key.
func TestAccountOnce(t *testing.T) {
	f := newFixture(t)
	a, b := f.issuer(), f.issuer()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, s := range []*Issuer{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.account(bg)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("account: %v", errs)
	}
	if a.accountURI() == "" || a.accountURI() != b.accountURI() {
		t.Fatalf("accounts %q %q", a.accountURI(), b.accountURI())
	}
	var n int
	if err := f.d.Pool.QueryRow(bg, `SELECT count(*) FROM acme_accounts`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d rows, %v", n, err)
	}
	// The key is sealed for its directory alone.
	dir, _ := pebble(t)
	acct, _ := f.st.GetACMEAccount(bg, dir)
	if _, err := f.sl.Open(acct.KeySealed, store.ACMEAccountAAD("https://other.example/dir")); err == nil {
		t.Fatal("the account's key opens for another directory")
	}

	// A CA that cannot be reached: the run fails, and the account is not
	// tried again before its backoff.
	c := f.issuer(func(c *Config) { c.Directory = "https://127.0.0.1:1/dir" })
	if err := c.Run(bg); err == nil || !strings.Contains(err.Error(), "the ACME account at") {
		t.Fatalf("an unreachable CA: %v", err)
	}
	if err := c.Run(bg); err != nil {
		t.Fatalf("the second run tried again at once: %v", err)
	}
}

// Without a directory nothing is ordered and no sealer is needed; the
// loop still deletes expired challenges. With a directory a sealer is.
func TestNoDirectory(t *testing.T) {
	f := newFixture(t)
	id := f.mkDomain("dashboard.fecore.dev", "")
	f.exec(`INSERT INTO acme_challenges (token, host, key_auth, domain_id, expires_at)
		VALUES ('old', 'dashboard.fecore.dev', 'old.k', $1, now() - interval '1 second'),
			('new', 'dashboard.fecore.dev', 'new.k', $1, now() + interval '1 minute')`, id)
	s, err := New(Config{Log: dbtest.Logger(t)}, Deps{Store: f.st})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Run(bg); err != nil {
		t.Fatal(err)
	}
	var tokens []string
	if err := f.d.Pool.QueryRow(bg, `SELECT array_agg(token) FROM acme_challenges`).Scan(&tokens); err != nil ||
		!slices.Equal(tokens, []string{"new"}) {
		t.Fatalf("challenges %v %v", tokens, err)
	}
	if n := len(f.ref.got()); n != 0 || f.state(id).err != "" {
		t.Fatal("something was ordered without a directory")
	}
	if _, err := New(Config{Directory: "https://acme.example/dir"}, Deps{Store: f.st}); err == nil ||
		!strings.Contains(err.Error(), "--secret-key-file") {
		t.Fatalf("a directory without a sealer: %v", err)
	}
	if l := s.Loops(); len(l) != 1 || l[0].Name != LoopName || !strings.Contains(l[0].Name, Hint) || l[0].Interval != DefaultInterval {
		t.Fatalf("loops %+v", l)
	}
}

// The texts of issue_error.
func TestDescribe(t *testing.T) {
	const name = "dashboard.fecore.dev"
	caa := &xacme.Error{ProblemType: "urn:ietf:params:acme:error:caa", Detail: "CAA record prevents issuance"}
	for _, c := range []struct {
		err  error
		want string
		wait time.Duration
	}{
		{fmt.Errorf("wait: %w", &xacme.AuthorizationError{Errors: []error{caa}}),
			"a CAA record of dashboard.fecore.dev forbids the CA to issue a certificate: CAA record prevents issuance", 0},
		{&xacme.AuthorizationError{Errors: []error{&xacme.Error{ProblemType: "urn:ietf:params:acme:error:connection",
			Detail: "Timeout during connect"}, &xacme.Error{ProblemType: "urn:ietf:params:acme:error:dns", Detail: "NXDOMAIN"}}},
			"the CA's HTTP-01 challenge for dashboard.fecore.dev failed: Timeout during connect (connection); NXDOMAIN (dns)", 0},
		{&xacme.OrderError{Status: "invalid", Problem: caa},
			"a CAA record of dashboard.fecore.dev forbids the CA to issue a certificate: CAA record prevents issuance", 0},
		{&xacme.OrderError{Status: "invalid"}, "the CA's order for dashboard.fecore.dev became invalid", 0},
		{&xacme.Error{ProblemType: "urn:ietf:params:acme:error:rateLimited", Detail: "slow down",
			Header: map[string][]string{"Retry-After": {"120"}}}, "rate limited by the CA: slow down", 2 * time.Minute},
		{&xacme.Error{ProblemType: "urn:ietf:params:acme:error:rejectedIdentifier", Detail: "blocked"},
			"the CA refuses to issue a certificate for dashboard.fecore.dev: blocked", 0},
		{&xacme.Error{ProblemType: "urn:ietf:params:acme:error:serverInternal", Detail: "oops"}, "the CA refused: oops (serverinternal)", 0},
		{fmt.Errorf("x: %w", context.DeadlineExceeded), "the order did not finish within 5m0s", 0},
		{errNoIngress, errNoIngress.Error(), 0},
		{errors.New("dial tcp: refused"), "the order failed: dial tcp: refused", 0},
	} {
		got, wait := describe(name, c.err, 5*time.Minute)
		if got != c.want || wait != c.wait {
			t.Errorf("%v: %q %v, want %q %v", c.err, got, wait, c.want, c.wait)
		}
	}
	long := strings.Repeat("é", 2000)
	if got, _ := describe(name, errors.New(long), time.Minute); len(got) > maxErrorText || !strings.HasSuffix(got, "…") {
		t.Errorf("a long text: %d bytes", len(got))
	}
}
