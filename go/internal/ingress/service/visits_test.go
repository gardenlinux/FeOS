// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/ingress/table"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/visit"
	"github.com/gardenlinux/feos/go/internal/visits"
)

// visitKeys are visit keys of a fixed seed, as the table carries them.
func visitKeys(t *testing.T) (*visit.Keys, wire.VisitKeys) {
	t.Helper()
	k, err := visit.NewKeys(bytes.Repeat([]byte{7}, 32), bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k, wire.VisitKeys{TokenKID: k.TokenKID(), TokenKey: k.TokenKey(), ChallengeKID: k.ChallengeKID(),
		ChallengeKey: k.ChallengeKey()}
}

// withVisits has the harness's table carry the keys k and sign visitors
// of ferun in at https://auth.example.test/visit.
func withVisits(k *visit.Keys) func(b *table.Builder) {
	return func(b *table.Builder) {
		b.AuthURL = "https://auth.example.test"
		b.VisitKeys = func(context.Context) (*visit.Keys, error) { return k, nil }
	}
}

// guard makes env admit the visitors of the tenant's provider fecode
// (kind tenant), made when missing, and tells the table.
func (h *harness) guard(env uuid.UUID) {
	h.t.Helper()
	h.exec(`INSERT INTO access_providers (tenant, name, kind, url) VALUES ('acme', 'fecode', 'tenant', 'https://fecode.example/api/v1/visit')
		ON CONFLICT DO NOTHING`)
	h.exec("UPDATE environments SET access = 'visitors', access_provider = 'fecode' WHERE id = $1", env)
	h.exec("SELECT pg_notify($1, $2)", db.ChanIngress, "env:"+env.String())
}

// An ingress that does not name the capability visits never sees an
// environment that is not open to anyone, nor the keys of visits, and
// sees one it was sent removed when it turns guarded; one that names it
// sees it with Access, Provider and SignIn, and the keys in its first
// table. A second Hello on a connection does not change what its first
// said.
func TestSyncVisits(t *testing.T) {
	k, keys := visitKeys(t)
	h := newHarness(t, harnessOpts{tableMod: withVisits(k)})
	h.world()
	h.meshNode("n1", 9)
	open := h.mkEnv("open", webSpec, true)
	ferun := h.mkEnv("ferun", webSpec, true)
	h.exec("UPDATE environments SET access = 'visitors' WHERE id = $1", ferun.ID)
	later := h.mkEnv("later", webSpec, true)

	_, old, _ := h.connect("in-old", 1, "10.0.0.1:51820")
	_, cur, _ := h.connect("in-new", 2, "10.0.0.2:51820", "other", wire.CapabilityVisits)
	// A second Hello on the old one's connection names visits: it changes
	// nothing.
	if _, err := old.hello(wgKey(1), "10.0.0.1:51820", wire.CapabilityVisits); err != nil {
		t.Fatal(err)
	}
	// And one on the new one's connection names nothing.
	if _, err := cur.hello(wgKey(2), "10.0.0.2:51820"); err != nil {
		t.Fatal(err)
	}

	so, sn := h.sync(old), h.sync(cur)
	first := so.next(10 * time.Second)
	if !first.Whole || first.VisitKeys != nil || len(first.Envs) != 2 || so.state.Envs[ferun.ID.String()].ID != "" {
		t.Fatalf("old ingress, first table: keys %v, envs %+v", first.VisitKeys, so.state.Envs)
	}
	for _, e := range first.Envs {
		if e.Access != "" {
			t.Fatalf("old ingress got a guarded environment %+v", e)
		}
	}
	first = sn.next(10 * time.Second)
	if !first.Whole || first.VisitKeys == nil || !table.SameVisitKeys(first.VisitKeys, &keys) || len(first.Envs) != 3 {
		t.Fatalf("new ingress, first table: keys %+v, %d envs", first.VisitKeys, len(first.Envs))
	}
	if e := sn.state.Envs[ferun.ID.String()]; e.Access != wire.AccessVisitors || e.Provider != model.ProviderFerun ||
		e.SignIn != "https://auth.example.test/visit" {
		t.Fatalf("new ingress, ferun's environment %+v", e)
	}
	if e := sn.state.Envs[open.ID.String()]; e.Access != "" || e.Provider != "" || e.SignIn != "" {
		t.Fatalf("new ingress, the open environment %+v", e)
	}

	// later turns guarded: removed for the old ingress, put with its
	// access for the new one.
	h.guard(later.ID)
	gone := so.until(2*time.Second, "later removed", func(t *wire.Table) bool { return len(t.Removed) == 1 })
	if gone.Removed[0] != later.ID.String() || len(gone.Envs) != 0 || len(so.state.Envs) != 1 {
		t.Fatalf("old ingress: %+v, state %d envs", gone, len(so.state.Envs))
	}
	sn.until(2*time.Second, "later guarded", func(*wire.Table) bool {
		e := sn.state.Envs[later.ID.String()]
		return e.Access == wire.AccessVisitors && e.Provider == "fecode" && e.SignIn == "https://fecode.example/api/v1/visit"
	})

	// Open again: put again for the old ingress.
	h.exec("UPDATE environments SET access = 'anyone', access_provider = NULL WHERE id = $1", later.ID)
	h.exec("SELECT pg_notify($1, $2)", db.ChanIngress, "env:"+later.ID.String())
	so.until(2*time.Second, "later back", func(*wire.Table) bool { _, ok := so.state.Envs[later.ID.String()]; return ok })
	if e := so.state.Envs[later.ID.String()]; e.Access != "" {
		t.Fatalf("old ingress, later open again: %+v", e)
	}
	// A stream opened later on the old connection is the same.
	so2 := h.sync(old)
	if first := so2.next(10 * time.Second); first.VisitKeys != nil || len(first.Envs) != 2 {
		t.Fatalf("old ingress, second stream: keys %v, %d envs", first.VisitKeys, len(first.Envs))
	}
	if so.state.VisitKeys != nil {
		t.Fatal("old ingress got keys")
	}
}

// Keys that come later reach a stream of an ingress that checks visits
// as a delta; a stream of one that does not never gets them.
func TestSyncVisitKeysLater(t *testing.T) {
	k, keys := visitKeys(t)
	var mu sync.Mutex
	ready := false
	h := newHarness(t, harnessOpts{tableMod: func(b *table.Builder) {
		b.Full = 100 * time.Millisecond
		b.VisitKeys = func(context.Context) (*visit.Keys, error) {
			mu.Lock()
			defer mu.Unlock()
			if !ready {
				return nil, errors.New("not yet")
			}
			return k, nil
		}
	}})
	_, old, _ := h.connect("in-old", 1, "10.0.0.1:51820")
	_, cur, _ := h.connect("in-new", 2, "10.0.0.2:51820", wire.CapabilityVisits)
	so, sn := h.sync(old), h.sync(cur)
	if first := sn.next(10 * time.Second); first.VisitKeys != nil {
		t.Fatalf("keys before VisitKeys answered: %+v", first.VisitKeys)
	}
	so.next(10 * time.Second)
	mu.Lock()
	ready = true
	mu.Unlock()
	got := sn.until(3*time.Second, "the keys", func(t *wire.Table) bool { return t.VisitKeys != nil })
	if got.Whole || !table.SameVisitKeys(got.VisitKeys, &keys) || !table.SameVisitKeys(sn.state.VisitKeys, &keys) {
		t.Fatalf("delta %+v", got)
	}
	// Sent once: the next full reads send nothing more of them.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case m := <-sn.msgs:
			if m.VisitKeys != nil {
				t.Fatalf("keys sent again: %+v", m)
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	for {
		select {
		case m := <-so.msgs:
			if m.VisitKeys != nil {
				t.Fatalf("the old ingress got keys: %+v", m)
			}
			continue
		default:
		}
		break
	}
}

// fakeVisits answers the calls of visits as told, and records them.
type fakeVisits struct {
	mu      sync.Mutex
	err     error
	redeems []wire.RedeemVisitRequest
	renews  []wire.RenewVisitRequest
	ends    []wire.EndVisitRequest
	redeem  wire.RedeemVisitResponse
	renew   wire.RenewVisitResponse
	// stall: Renew waits for its context's end and returns its error.
	stall bool
}

func (f *fakeVisits) Redeem(_ context.Context, req wire.RedeemVisitRequest) (wire.RedeemVisitResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redeems = append(f.redeems, req)
	return f.redeem, f.err
}

func (f *fakeVisits) Renew(ctx context.Context, req wire.RenewVisitRequest) (wire.RenewVisitResponse, error) {
	f.mu.Lock()
	f.renews = append(f.renews, req)
	stall := f.stall
	f.mu.Unlock()
	if stall {
		<-ctx.Done()
		return wire.RenewVisitResponse{}, fmt.Errorf("read a visit: %w", ctx.Err())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renew, f.err
}

func (f *fakeVisits) End(_ context.Context, req wire.EndVisitRequest) (wire.EndVisitResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends = append(f.ends, req)
	return wire.EndVisitResponse{}, f.err
}

func (f *fakeVisits) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeVisits) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.redeems) + len(f.renews) + len(f.ends)
}

// secret43 is a binding secret's or hash's length of b.
func secret43(b byte) string { return strings.Repeat(string(b), bindingLen) }

// calls makes the three calls of visits on c with well-formed fields.
func visitCalls(c *client) map[string]func() error {
	ctx := bg
	return map[string]func() error{
		"redeem": func() error {
			_, err := c.api.RedeemVisit(ctx, &wire.RedeemVisitRequest{Code: "a.b.c", Binding: secret43('b'), VisitBinding: secret43('v'),
				Host: "web-acme." + testZone})
			return err
		},
		"renew": func() error {
			_, err := c.api.RenewVisit(ctx, &wire.RenewVisitRequest{Token: "a.b.c", Secret: secret43('s')})
			return err
		},
		"end": func() error {
			_, err := c.api.EndVisit(ctx, &wire.EndVisitRequest{Token: "a.b.c", Secret: secret43('s')})
			return err
		},
	}
}

// The three calls pass the visits service's outcomes through as they
// are; visits.ErrInvalid is INVALID_ARGUMENT, any other error
// UNAVAILABLE; fields out of their sizes are INVALID_ARGUMENT without a
// call; and while the instance stops they answer UNAVAILABLE.
func TestVisitCalls(t *testing.T) {
	f := &fakeVisits{
		redeem: wire.RedeemVisitResponse{Outcome: wire.OutcomeAdmitted, Token: "t.o.k"},
		renew:  wire.RenewVisitResponse{Outcome: wire.OutcomeEnded, Reason: model.VisitProviderNo},
	}
	h := newHarness(t, harnessOpts{visits: f})
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820", wire.CapabilityVisits)

	host := "web-acme." + testZone
	redeemReq := &wire.RedeemVisitRequest{Code: "a.b.c", Binding: secret43('b'), VisitBinding: secret43('v'), Host: host}
	r, err := c.api.RedeemVisit(bg, redeemReq)
	if err != nil || r.Outcome != wire.OutcomeAdmitted || r.Token != "t.o.k" || len(f.redeems) != 1 || f.redeems[0] != *redeemReq {
		t.Fatalf("redeem: %+v %v, calls %+v", r, err, f.redeems)
	}
	f.redeem = wire.RedeemVisitResponse{Outcome: wire.OutcomeRefused, Reason: "used"}
	if r, err := c.api.RedeemVisit(bg, redeemReq); err != nil || r.Outcome != wire.OutcomeRefused || r.Reason != "used" {
		t.Fatalf("refused redeem: %+v %v", r, err)
	}
	n, err := c.api.RenewVisit(bg, &wire.RenewVisitRequest{Token: "a.b.c", Secret: secret43('s')})
	if err != nil || n.Outcome != wire.OutcomeEnded || n.Reason != model.VisitProviderNo || f.renews[0].Secret != secret43('s') {
		t.Fatalf("renew: %+v %v", n, err)
	}
	f.renew = wire.RenewVisitResponse{Outcome: wire.OutcomeRenewed, Token: "n.e.w"}
	if n, err := c.api.RenewVisit(bg, &wire.RenewVisitRequest{Token: "a.b.c", Secret: secret43('s')}); err != nil ||
		n.Outcome != wire.OutcomeRenewed || n.Token != "n.e.w" {
		t.Fatalf("renewed: %+v %v", n, err)
	}
	if _, err := c.api.EndVisit(bg, &wire.EndVisitRequest{Token: "a.b.c", Secret: secret43('s')}); err != nil ||
		len(f.ends) != 1 || f.ends[0].Token != "a.b.c" {
		t.Fatalf("end: %v %+v", err, f.ends)
	}

	// The service's errors.
	for _, tc := range []struct {
		err  error
		code codes.Code
	}{
		{fmt.Errorf("visitBinding: %w", visits.ErrInvalid), codes.InvalidArgument},
		{errors.New("the database cannot be reached"), codes.Unavailable},
	} {
		f.set(tc.err)
		for name, call := range visitCalls(c) {
			if err := call(); status.Code(err) != tc.code {
				t.Errorf("%s with %v: %v, want %s", name, tc.err, err, tc.code)
			}
		}
	}
	f.set(nil)

	// Sizes: refused before any call.
	long := strings.Repeat("x", visit.MaxJWT+1)
	before := f.calls()
	for name, req := range map[string]any{
		"an empty code":         &wire.RedeemVisitRequest{Binding: secret43('b'), VisitBinding: secret43('v'), Host: host},
		"a long code":           &wire.RedeemVisitRequest{Code: long, Binding: secret43('b'), VisitBinding: secret43('v'), Host: host},
		"a short binding":       &wire.RedeemVisitRequest{Code: "a.b.c", Binding: "b", VisitBinding: secret43('v'), Host: host},
		"a long visit binding":  &wire.RedeemVisitRequest{Code: "a.b.c", Binding: secret43('b'), VisitBinding: secret43('v') + "v", Host: host},
		"no host":               &wire.RedeemVisitRequest{Code: "a.b.c", Binding: secret43('b'), VisitBinding: secret43('v')},
		"a long host":           &wire.RedeemVisitRequest{Code: "a.b.c", Binding: secret43('b'), VisitBinding: secret43('v'), Host: strings.Repeat("h", 254)},
		"renew: a long token":   &wire.RenewVisitRequest{Token: long, Secret: secret43('s')},
		"renew: no token":       &wire.RenewVisitRequest{Secret: secret43('s')},
		"renew: a short secret": &wire.RenewVisitRequest{Token: "a.b.c", Secret: "s"},
		"end: a long token":     &wire.EndVisitRequest{Token: long, Secret: secret43('s')},
		"end: no secret":        &wire.EndVisitRequest{Token: "a.b.c"},
	} {
		var err error
		switch r := req.(type) {
		case *wire.RedeemVisitRequest:
			_, err = c.api.RedeemVisit(bg, r)
		case *wire.RenewVisitRequest:
			_, err = c.api.RenewVisit(bg, r)
		case *wire.EndVisitRequest:
			_, err = c.api.EndVisit(bg, r)
		}
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", name, err)
		}
	}
	if f.calls() != before {
		t.Fatalf("refused calls reached the service: %d, want %d", f.calls(), before)
	}

	h.s.BeginStopping()
	for name, call := range visitCalls(c) {
		wantCode(t, name+" while stopping", call(), codes.Unavailable, stoppingText)
	}
}

// When ferund's own step timeout ends while the ingress still waits (the
// database hangs), the answer is UNAVAILABLE, ferund away, not the call's
// DEADLINE_EXCEEDED. (It takes rpcTimeout.)
func TestVisitCallStepTimeout(t *testing.T) {
	f := &fakeVisits{stall: true}
	h := newHarness(t, harnessOpts{visits: f})
	_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820", wire.CapabilityVisits)
	ctx, cancel := context.WithTimeout(bg, rpcTimeout+30*time.Second)
	defer cancel()
	_, err := c.api.RenewVisit(ctx, &wire.RenewVisitRequest{Token: "a.b.c", Secret: secret43('s')})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("renew past the step's timeout: %v, want UNAVAILABLE", err)
	}
}

// Without visits (no Deps.Visits, or a nil *visits.Service, as ferund
// without --secret-key-file has) the calls answer UNAVAILABLE.
func TestVisitCallsOff(t *testing.T) {
	for name, v := range map[string]Visits{"none": nil, "a nil service": (*visits.Service)(nil)} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{visits: v})
			_, c, _ := h.connect("in-0", 1, "10.0.0.1:51820", wire.CapabilityVisits)
			for call, f := range visitCalls(c) {
				wantCode(t, call, f(), codes.Unavailable, visitsOffText)
			}
		})
	}
}
