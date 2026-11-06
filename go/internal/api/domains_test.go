// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/domains"
	envs "github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/store"
)

// fakeDomains is the domains service of the tests that do not use it:
// every route answers 404.
type fakeDomains struct{}

func (fakeDomains) CreateDomain(context.Context, string, domains.CreateRequest) (domains.Domain, error) {
	return domains.Domain{}, errNoDomain()
}
func (fakeDomains) ListDomains(context.Context, string, string, map[string]string) (domains.List, error) {
	return domains.List{Domains: []domains.Domain{}}, nil
}
func (fakeDomains) GetDomain(context.Context, string, string) (domains.Domain, error) {
	return domains.Domain{}, errNoDomain()
}
func (fakeDomains) BindDomain(context.Context, string, string, string) (domains.Domain, error) {
	return domains.Domain{}, errNoDomain()
}
func (fakeDomains) UnbindDomain(context.Context, string, string) (domains.Domain, error) {
	return domains.Domain{}, errNoDomain()
}
func (fakeDomains) CheckDomain(context.Context, string, string) (domains.Domain, error) {
	return domains.Domain{}, errNoDomain()
}
func (fakeDomains) DeleteDomain(context.Context, string, string) error { return errNoDomain() }

// domainDNS is the DNS of the domains' end-to-end test: per name a TXT
// value and whether it CNAMEs to the edge.
type domainDNS struct {
	mu     sync.Mutex
	txt    map[string][]string
	routed map[string]bool
}

func (f *domainDNS) Lookup(_ context.Context, q domains.Query) domains.Answer {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := domains.Answer{TXT: slices.Clone(f.txt[q.TXTName])}
	if f.routed[q.Name] {
		a.CNAME = []string{"edge.fecode.dev"}
		a.Addresses = []netip.Addr{netip.MustParseAddr("194.11.242.65")}
	}
	return a
}

func (f *domainDNS) Resolve(context.Context, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("194.11.242.65")}, nil
}

func (f *domainDNS) set(name, token string, routed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txt["_fecode-challenge."+name] = []string{token}
	f.routed[name] = routed
}

// The domains' routes on the real service, through the API's mux with a
// tenant's token: create, the answer's shape, refusals, list and its
// filters, bind (to an environment made public through its own route,
// whose change the domain follows at once), check, unbind, delete;
// another tenant's domain is absent.
func TestDomainsEndToEnd(t *testing.T) {
	t.Parallel()
	dns := &domainDNS{txt: map[string][]string{}, routed: map[string]bool{}}
	var dsvc *domains.Service
	e := newEnv(t, envOptions{
		domains: func(d *db.DB, st *store.Store) Domains {
			var err error
			dsvc, err = domains.New(domains.Config{EdgeName: "edge.fecode.dev", ChallengeLabel: "_fecode-challenge",
				Grace: 72 * time.Hour, UnverifiedMax: 5, Reserved: []string{"apps.fecode.dev", "fecode.dev"},
				Log: dbtest.Logger(t)}, domains.Deps{Store: st, DNS: dns})
			if err != nil {
				t.Fatal(err)
			}
			return dsvc
		},
		environments: func(d *db.DB, st *store.Store) Environments {
			svc, err := envs.New(envs.Deps{InstanceID: testInstance, DB: d, Store: st, Engine: envEngine{},
				Ports: envPorts{}, AppsZone: "apps.fecode.dev", Log: dbtest.Logger(t),
				EnvChanged: func(ctx context.Context, tx *store.Store, id uuid.UUID) error {
					return dsvc.EnvChanged(ctx, tx, id)
				}})
			if err != nil {
				t.Fatal(err)
			}
			return svc
		},
	})
	tok := bearerOf(e.tenantToken)
	do := func(method, path string, body any) *recorderT {
		t.Helper()
		return e.do(method, path, tok, body)
	}

	w := do("POST", "/api/v1/domains", map[string]any{"name": "Dashboard.FeCore.dev.",
		"labels": map[string]string{"fecode.project": "p1"}})
	expect(t, w, http.StatusCreated, "")
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &shape); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range shape {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"boundAt", "certificate", "checkedAt", "createdAt", "environment", "found", "id", "labels",
		"name", "nextCheckAt", "reason", "records", "state", "verification", "verifiedAt", "warnings"}; !slices.Equal(keys, want) {
		t.Fatalf("keys %v", keys)
	}
	for k, want := range map[string]string{"environment": "null", "certificate": "null", "verifiedAt": "null",
		"boundAt": "null", "checkedAt": "null",
		"found":    `{"txt":[],"cname":[],"addresses":[]}`,
		"warnings": `{"recordMissingSince":null,"dnsMovedSince":null,"deactivatesAt":null,"checkFailing":"","certificateFailing":""}`} {
		if string(shape[k]) != want {
			t.Errorf("%s: %s, want %s", k, shape[k], want)
		}
	}
	d := decode[domains.Domain](t, w)
	if d.Name != "dashboard.fecore.dev" || d.Verification != "unverified" || d.State != "unverified" ||
		d.Labels["fecode.project"] != "p1" || d.Records.TXT.Name != "_fecode-challenge.dashboard.fecore.dev" ||
		len(d.Records.TXT.Value) != 32 || d.Records.CNAME != "edge.fecode.dev" ||
		!slices.Equal(d.Records.Addresses, []string{"194.11.242.65"}) || d.Records.Apex != nil {
		t.Fatalf("created %+v", d)
	}
	if !strings.Contains(string(shape["records"]), `"apex":null`) {
		t.Fatalf("records %s", shape["records"])
	}

	// Refusals.
	expect(t, do("POST", "/api/v1/domains", map[string]any{"name": "bücher.example"}), http.StatusBadRequest, "")
	if w := do("POST", "/api/v1/domains", map[string]any{"name": "bücher.example"}); !strings.Contains(w.Body.String(),
		`"code":"invalid_name"`) || !strings.Contains(w.Body.String(), "xn--") {
		t.Fatalf("idn %s", w.Body)
	}
	w = do("POST", "/api/v1/domains", map[string]any{"name": "x.apps.fecode.dev"})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"code":"reserved"`) {
		t.Fatalf("reserved %d %s", w.Code, w.Body)
	}
	expect(t, do("POST", "/api/v1/domains", "nope"), http.StatusBadRequest, "")

	// The list and its filters.
	do("POST", "/api/v1/domains", map[string]any{"name": "other.example.com"})
	list := decode[domains.List](t, do("GET", "/api/v1/domains", nil))
	if len(list.Domains) != 2 || list.Domains[0].ID != d.ID {
		t.Fatalf("list %+v", list)
	}
	list = decode[domains.List](t, do("GET", "/api/v1/domains?label=fecode.project=p1", nil))
	if len(list.Domains) != 1 || list.Domains[0].ID != d.ID {
		t.Fatalf("labels %+v", list)
	}
	expect(t, do("GET", "/api/v1/domains?label=nope", nil), http.StatusBadRequest, "")
	expect(t, do("GET", "/api/v1/domains?environment=prod", nil), http.StatusNotFound, errBody("no such environment"))

	// Bind to an environment, before verification; publish it.
	w = do("POST", "/api/v1/environments", map[string]any{"name": "prod"})
	expect(t, w, http.StatusCreated, "")
	envID := decode[envs.EnvStatus](t, w).ID
	expect(t, do("PUT", "/api/v1/domains/"+d.ID+"/environment", map[string]any{}), http.StatusBadRequest, "")
	expect(t, do("PUT", "/api/v1/domains/"+d.ID+"/environment", map[string]any{"environment": "nope"}),
		http.StatusNotFound, errBody("no such environment"))
	w = do("PUT", "/api/v1/domains/"+d.ID+"/environment", map[string]any{"environment": "prod"})
	expect(t, w, http.StatusOK, "")
	if b := decode[domains.Domain](t, w); b.Environment == nil || *b.Environment != envID || b.BoundAt == nil ||
		b.State != "unverified" {
		t.Fatalf("bound %+v", b)
	}
	list = decode[domains.List](t, do("GET", "/api/v1/domains?environment="+envID, nil))
	if len(list.Domains) != 1 || list.Domains[0].ID != d.ID {
		t.Fatalf("by environment %+v", list)
	}
	expect(t, do("PUT", "/api/v1/environments/prod/public", map[string]any{"public": true}), http.StatusOK, "")

	// The records in DNS, a check now: verified and routed; it waits for
	// its certificate.
	dns.set("dashboard.fecore.dev", d.Records.TXT.Value, true)
	w = do("POST", "/api/v1/domains/"+d.ID+"/check", nil)
	expect(t, w, http.StatusOK, "")
	if !strings.Contains(w.Body.String(), `"certificate":null`) || !strings.Contains(w.Body.String(), `"apex":null`) {
		t.Fatalf("checked %s", w.Body)
	}
	c := decode[domains.Domain](t, w)
	if c.Verification != "verified" || c.State != "issuing" || c.Reason != domains.ReasonIssuing ||
		c.VerifiedAt == nil || c.CheckedAt == nil || !slices.Equal(c.Found.CNAME, []string{"edge.fecode.dev"}) ||
		!slices.Equal(c.Found.Addresses, []string{"194.11.242.65"}) || c.Records.Apex != nil {
		t.Fatalf("checked %+v", c)
	}
	// ACME keeps its certificate (internal/acme): the next check serves
	// it, and the answer shows the certificate; a renewal that fails shows
	// as a warning while it stays served.
	if _, err := e.st.Q().Exec(t.Context(), `INSERT INTO domain_certs (domain_id, chain, key_sealed, not_before, not_after,
			renew_after, serial) VALUES ($1, ARRAY['\x01'::bytea], '\x02', now() - interval '1 hour',
			now() + interval '143 hours', now() + interval '71 hours', 'abc')`, d.ID); err != nil {
		t.Fatal(err)
	}
	s := decode[domains.Domain](t, do("POST", "/api/v1/domains/"+d.ID+"/check", nil))
	if s.State != "served" || s.Reason != "" || s.Certificate == nil || s.Certificate.Serial != "abc" ||
		s.Certificate.RenewAfter == "" || s.Warnings.CertificateFailing != "" {
		t.Fatalf("served %+v %+v", s, s.Certificate)
	}
	if _, err := e.st.Q().Exec(t.Context(), `UPDATE domains SET issue_error = 'rate limited by the CA: x',
			issue_failing_since = now() WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if s := decode[domains.Domain](t, do("GET", "/api/v1/domains/"+d.ID, nil)); s.State != "served" ||
		s.Warnings.CertificateFailing != "rate limited by the CA: x" {
		t.Fatalf("a failing renewal %+v", s)
	}

	// Unpublished through its route: the stored state follows in the same
	// transaction.
	expect(t, do("PUT", "/api/v1/environments/prod/public", map[string]any{"public": false}), http.StatusOK, "")
	if v, err := e.st.LockDomainView(t.Context(), uuid.MustParse(d.ID), store.NoLock); err != nil || v.State != "unpublished" {
		t.Fatalf("stored %s %v", v.State, err)
	}
	if g := decode[domains.Domain](t, do("GET", "/api/v1/domains/"+d.ID, nil)); g.State != "unpublished" {
		t.Fatalf("get %+v", g)
	}

	// Another tenant sees nothing of it.
	other := bearerOf(e.mintTenantToken(mustTenant(t, e, "rival")))
	for _, rq := range [][2]string{{"GET", ""}, {"DELETE", ""}, {"POST", "/check"}, {"DELETE", "/environment"}} {
		expect(t, e.do(rq[0], "/api/v1/domains/"+d.ID+rq[1], other, nil), http.StatusNotFound, errBody("no such domain"))
	}
	expect(t, e.do("PUT", "/api/v1/domains/"+d.ID+"/environment", other, map[string]any{"environment": envID}),
		http.StatusNotFound, "")
	if l := decode[domains.List](t, e.do("GET", "/api/v1/domains", other, nil)); len(l.Domains) != 0 {
		t.Fatalf("rival's list %+v", l)
	}
	expect(t, do("GET", "/api/v1/domains/not-a-uuid", nil), http.StatusNotFound, errBody("no such domain"))

	// Unbind, delete.
	w = do("DELETE", "/api/v1/domains/"+d.ID+"/environment", nil)
	if u := decode[domains.Domain](t, w); w.Code != 200 || u.Environment != nil || u.BoundAt != nil || u.State != "unbound" {
		t.Fatalf("unbound %d %+v", w.Code, u)
	}
	expect(t, do("DELETE", "/api/v1/domains/"+d.ID, nil), http.StatusNoContent, "")
	expect(t, do("GET", "/api/v1/domains/"+d.ID, nil), http.StatusNotFound, errBody("no such domain"))
}

// Without --edge-name, nothing can be claimed or checked: 409
// domains_off; listing still answers.
func TestDomainsOff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{domains: func(d *db.DB, st *store.Store) Domains {
		svc, err := domains.New(domains.Config{Log: dbtest.Logger(t)}, domains.Deps{Store: st,
			DNS: &domainDNS{txt: map[string][]string{}, routed: map[string]bool{}}})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}})
	tok := bearerOf(e.tenantToken)
	expect(t, e.do("POST", "/api/v1/domains", tok, map[string]any{"name": "a.example.com"}), http.StatusConflict,
		`{"error":"the platform serves no domains (--edge-name)","code":"domains_off"}`+"\n")
	expect(t, e.do("GET", "/api/v1/domains", tok, nil), http.StatusOK, `{"domains":[]}`+"\n")
}
