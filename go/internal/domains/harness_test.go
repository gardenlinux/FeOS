// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/store"
)

var bg = context.Background()

// The edge of the tests.
const testEdge = "edge.fecode.test"

var testEdgeAddrs = []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")}

// fakeDNS answers what a test set: per name its TXT values and routing,
// or a failure of either.
type fakeDNS struct {
	mu       sync.Mutex
	txt      map[string][]string // by the TXT record's name
	txtErr   map[string]error
	cname    map[string][]string
	addrs    map[string][]netip.Addr
	routeErr map[string]error
	edge     []netip.Addr
	edgeErr  error
	lookups  []string
}

func newFakeDNS() *fakeDNS {
	return &fakeDNS{txt: map[string][]string{}, txtErr: map[string]error{}, cname: map[string][]string{},
		addrs: map[string][]netip.Addr{}, routeErr: map[string]error{}, edge: testEdgeAddrs}
}

func (f *fakeDNS) Lookup(_ context.Context, q Query) Answer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, q.Name)
	a := Answer{TXT: slices.Clone(f.txt[q.TXTName]), TXTErr: f.txtErr[q.TXTName],
		CNAME: slices.Clone(f.cname[q.Name]), Addresses: slices.Clone(f.addrs[q.Name]), RouteErr: f.routeErr[q.Name]}
	if a.TXTErr != nil {
		a.TXT = nil
	}
	if a.RouteErr != nil {
		a.CNAME, a.Addresses = nil, nil
	}
	return a
}

func (f *fakeDNS) Resolve(context.Context, string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.edge), f.edgeErr
}

// setTXT sets the TXT values of name's claims' record.
func (f *fakeDNS) setTXT(name string, values ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txt["_ferun-challenge."+name] = values
	delete(f.txtErr, "_ferun-challenge."+name)
}

// routeToEdge has name CNAME to the edge: it resolves to the edge's
// addresses.
func (f *fakeDNS) routeToEdge(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cname[name], f.addrs[name] = []string{testEdge}, slices.Clone(testEdgeAddrs)
	delete(f.routeErr, name)
}

// routeTo has name resolve to addrs, without a CNAME.
func (f *fakeDNS) routeTo(name string, addrs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cname[name], f.addrs[name] = nil, nil
	for _, a := range addrs {
		f.addrs[name] = append(f.addrs[name], netip.MustParseAddr(a))
	}
	delete(f.routeErr, name)
}

// fail makes both lookups of name fail.
func (f *fakeDNS) fail(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txtErr["_ferun-challenge."+name] = errors.New("TXT: no answer within 2s")
	f.routeErr[name] = errors.New("A: SERVFAIL")
}

// harness is a domains service on a fresh database whose instance leads,
// with the tenants acme and rival, and a fake DNS. Its loops are not
// registered: the tests run them.
type harness struct {
	t   *testing.T
	d   *db.DB
	st  *store.Store
	dns *fakeDNS
	s   *Service
}

func newHarness(t *testing.T, mod func(*Config)) *harness {
	t.Helper()
	d := dbtest.New(t)
	st := store.New(d)
	log := dbtest.Logger(t)
	el := leader.NewElector(d, "i1", log)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = el.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(20 * time.Second)
	for !el.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("not leading")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cfg := Config{EdgeName: testEdge, ChallengeLabel: "_ferun-challenge", Grace: 72 * time.Hour, UnverifiedMax: 3,
		Reserved: []string{"apps.fecode.test", "fecode.dev"}, Log: log}
	if mod != nil {
		mod(&cfg)
	}
	f := newFakeDNS()
	s, err := New(cfg, Deps{Store: st, Elector: el, DNS: f})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, d: d, st: st, dns: f, s: s}
	for _, tenant := range []string{"acme", "rival"} {
		if _, _, err := st.CreateTenant(bg, tenant); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.d.Pool.Exec(bg, sql, args...); err != nil {
		h.t.Fatalf("%s: %v", sql, err)
	}
}

// env makes an environment of tenant, public or not, with deployment 1.
func (h *harness) env(tenant, name string, public bool) store.Environment {
	h.t.Helper()
	e, err := h.st.InsertEnvironment(bg, store.NewEnvironment{ID: uuid.New(), Owner: tenant, Name: name})
	if err != nil {
		h.t.Fatal(err)
	}
	h.exec("UPDATE environments SET deployment = 1 WHERE id = $1", e.ID)
	if public {
		if e, err = h.st.SetEnvironmentPublic(bg, e.ID, true); err != nil {
			h.t.Fatal(err)
		}
	}
	return e
}

// claim claims name for tenant and returns its row.
func (h *harness) claim(tenant, name string) store.DomainView {
	h.t.Helper()
	d, err := h.s.CreateDomain(bg, tenant, CreateRequest{Name: name})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.row(uuid.MustParse(d.ID))
}

func (h *harness) row(id uuid.UUID) store.DomainView {
	h.t.Helper()
	v, err := h.st.LockDomainView(bg, id, store.NoLock)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

// check runs one check of the domain as the leader's loop does.
func (h *harness) check(id uuid.UUID) store.DomainView {
	h.t.Helper()
	if err := h.s.check(bg, h.row(id).Domain, false, true); err != nil {
		h.t.Fatal(err)
	}
	return h.row(id)
}

func (h *harness) bind(tenant string, id uuid.UUID, env string) store.DomainView {
	h.t.Helper()
	if _, err := h.s.BindDomain(bg, tenant, id.String(), env); err != nil {
		h.t.Fatal(err)
	}
	return h.row(id)
}

// incidents returns the environment's incidents of the domain's object
// by condition: how many are open, how many closed.
func (h *harness) incidents(envID uuid.UUID, name string) map[string][2]int {
	h.t.Helper()
	list, err := h.st.ListIncidents(bg, envID, false)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string][2]int{}
	for _, i := range list {
		if i.Object != "domain/"+name {
			continue
		}
		c := out[i.Condition]
		if i.ClosedAt.IsZero() {
			c[0]++
		} else {
			c[1]++
		}
		out[i.Condition] = c
	}
	return out
}

// routed makes a verified domain of acme, bound to a public environment,
// routed: the state issuing, which waits for certificates.
func (h *harness) routed(name string) (store.DomainView, store.Environment) {
	h.t.Helper()
	e := h.env("acme", "prod-"+uuid.NewString()[:8], true)
	d := h.claim("acme", name)
	h.dns.setTXT(name, d.Token)
	h.dns.routeToEdge(name)
	h.check(d.ID)
	h.bind("acme", d.ID, e.ID.String())
	v := h.check(d.ID)
	if v.State != StateIssuing || v.Reason != ReasonIssuing || !v.WasRouted {
		h.t.Fatalf("not routed: %+v", v)
	}
	return v, e
}
