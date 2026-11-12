// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/visit"
)

func newVisitKeys(t *testing.T) *visit.Keys {
	t.Helper()
	seed, key := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(seed)
	_, _ = rand.Read(key)
	k, err := visit.NewKeys(seed, key)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Every Hello names the capability visits: the first of a session and
// the one that announces a new endpoint.
func TestHelloNamesVisits(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)
	ta.mu.Lock()
	ta.meshPort++
	ta.mu.Unlock()
	ta.announce()
	eventually(t, "the announcing Hello", func() bool { return len(h.fake.Hellos()) >= 2 })
	for _, hl := range h.fake.Hellos() {
		if !slices.Equal(hl.Request.Capabilities, []string{wire.CapabilityVisits}) {
			t.Errorf("Hello %+v", hl.Request)
		}
	}
}

// A delta that carries only the visits' keys reaches OnState and the
// cache, and a restart from the cache keeps them.
func TestVisitKeysAloneReachTheProxyAndTheCache(t *testing.T) {
	h := newHarness(t)
	h.fake.PutEnv(testEnv("e1"))
	ta := h.joined(nil)
	_, n := ta.rec.lastState()
	k := newVisitKeys(t)
	h.fake.SetVisits(k)
	eventually(t, "the keys", func() bool {
		s, m := ta.rec.lastState()
		return m > n && s.VisitKeys != nil
	})
	s, _ := ta.rec.lastState()
	if s.VisitKeys.TokenKID != k.TokenKID() || s.VisitKeys.ChallengeKID != k.ChallengeKID() {
		t.Fatalf("keys %+v", s.VisitKeys)
	}
	d := &stateDir{path: h.cfg.StateDir}
	eventually(t, "the keys cached", func() bool {
		c, _, err := d.loadTable()
		return err == nil && c != nil && c.VisitKeys != nil
	})
	ta.stop()

	cfg := h.cfg
	cfg.ControlPlane = []string{deadAddr(t)}
	ta2 := h.start(cfg, nil)
	eventually(t, "the cached table", func() bool {
		s, _ := ta2.rec.lastState()
		return s != nil
	})
	s, _ = ta2.rec.lastState()
	if s.VisitKeys == nil || s.VisitKeys.TokenKID != k.TokenKID() || string(s.VisitKeys.ChallengeKey) != string(k.ChallengeKey()) {
		t.Fatalf("restarted with keys %+v", s.VisitKeys)
	}
}

// The cache is table-v2.json; the first save deletes the table.json of
// the releases before, which is never read.
func TestTableCacheV2(t *testing.T) {
	d, err := openStateDir(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.write(oldTableFile, []byte(`{"savedAt":"2026-11-01T00:00:00Z","state":{"seq":9,"envs":{}}}`)); err != nil {
		t.Fatal(err)
	}
	if s, _, err := d.loadTable(); s != nil || err != nil {
		t.Fatalf("table.json was read: %+v %v", s, err)
	}
	s := wire.NewState()
	s.VisitKeys = &wire.VisitKeys{TokenKID: "k", TokenKey: []byte{1}, ChallengeKID: "c", ChallengeKey: []byte{2}}
	if err := d.saveTable(s, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.file("table-v2.json")); err != nil {
		t.Fatalf("table-v2.json: %v", err)
	}
	if _, err := os.Stat(d.file("table.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("table.json is still there: %v", err)
	}
	got, _, err := d.loadTable()
	if err != nil || got.VisitKeys == nil || got.VisitKeys.TokenKID != "k" {
		t.Fatalf("loaded %+v %v", got, err)
	}
	// A second save without the old file is fine.
	if err := d.saveTable(s, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// The visit calls without a connection fail with ErrNotConnected, which
// reads as Unavailable; ErrBusy reads as ResourceExhausted.
func TestVisitCallsWithoutConnection(t *testing.T) {
	h := newHarness(t)
	cfg := h.cfg
	cfg.ControlPlane = []string{deadAddr(t)}
	ta := h.start(cfg, nil)
	ctx := context.Background()
	if _, err := ta.RedeemVisit(ctx, wire.RedeemVisitRequest{}); !errors.Is(err, ErrNotConnected) {
		t.Errorf("RedeemVisit: %v", err)
	}
	if _, err := ta.RenewVisit(ctx, wire.RenewVisitRequest{}); !errors.Is(err, ErrNotConnected) {
		t.Errorf("RenewVisit: %v", err)
	}
	if _, err := ta.EndVisit(ctx, wire.EndVisitRequest{}); !errors.Is(err, ErrNotConnected) {
		t.Errorf("EndVisit: %v", err)
	}
	if status.Code(ErrNotConnected) != codes.Unavailable || status.Code(ErrBusy) != codes.ResourceExhausted {
		t.Errorf("codes %v %v", status.Code(ErrNotConnected), status.Code(ErrBusy))
	}
	if ErrNotConnected.Error() != "ingress agent: not connected to the control plane" {
		t.Errorf("text %q", ErrNotConnected.Error())
	}
}

// The visit calls reach the control plane on the current connection.
func TestVisitCalls(t *testing.T) {
	h := newHarness(t)
	k := newVisitKeys(t)
	h.fake.SetVisits(k)
	env := testEnv("e1")
	env.Access, env.Provider = wire.AccessVisitors, "fecode"
	h.fake.PutEnv(env)
	h.fake.SetVisitor("u-7", "u7@example.com")
	ta := h.joined(nil)
	ctx := context.Background()
	cs, _ := visit.NewBindingSecret()
	vs, _ := visit.NewBindingSecret()
	now := time.Now()
	code, err := k.SignCode(visit.Code{Challenge: "AAAAAAAAAAAAAAAAAAAAAA", Env: "e1", Host: "e1.apps.test", Path: "/",
		Binding: visit.BindingHash(cs), Outcome: visit.OutcomeAccepted, Visit: "v1", IssuedAt: now.Unix(),
		Expires: now.Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ta.RedeemVisit(ctx, wire.RedeemVisitRequest{Code: code, Binding: cs, VisitBinding: visit.BindingHash(vs), Host: "e1.apps.test"})
	if err != nil || res.Outcome != wire.OutcomeAdmitted {
		t.Fatalf("redeem %+v %v", res, err)
	}
	tok, err := k.VerifyToken(res.Token)
	if err != nil || tok.Subject != "u-7" || tok.Email != "u7@example.com" || tok.Provider != "fecode" ||
		tok.Binding != visit.BindingHash(vs) || tok.Visit != "v1" {
		t.Fatalf("token %+v %v", tok, err)
	}
	ren, err := ta.RenewVisit(ctx, wire.RenewVisitRequest{Token: res.Token, Secret: vs})
	if err != nil || ren.Outcome != wire.OutcomeRenewed {
		t.Fatalf("renew %+v %v", ren, err)
	}
	if _, err := ta.EndVisit(ctx, wire.EndVisitRequest{Token: ren.Token, Secret: vs}); err != nil {
		t.Fatal(err)
	}
	if ren, err := ta.RenewVisit(ctx, wire.RenewVisitRequest{Token: ren.Token, Secret: vs}); err != nil ||
		ren.Outcome != wire.OutcomeEnded || ren.Reason != "signed_out" {
		t.Fatalf("renew after the end %+v %v", ren, err)
	}
	if len(h.fake.Redeems()) != 1 || len(h.fake.VisitRenewals()) != 2 || len(h.fake.EndVisits()) != 1 {
		t.Fatalf("calls %d %d %d", len(h.fake.Redeems()), len(h.fake.VisitRenewals()), len(h.fake.EndVisits()))
	}
	h.fake.SetVisitError(status.Error(codes.Unavailable, "away"))
	if _, err := ta.RenewVisit(ctx, wire.RenewVisitRequest{Token: res.Token, Secret: vs}); status.Code(err) != codes.Unavailable {
		t.Fatalf("an error of the control plane: %v", err)
	}
}

// Beyond 8 renewals in flight a renewal fails at once with ErrBusy; the
// redeems and ends have their own 8.
func TestVisitCallsAreBounded(t *testing.T) {
	h := newHarness(t)
	h.fake.SetVisits(newVisitKeys(t))
	ta := h.joined(nil)
	h.fake.SetVisitDelay(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range maxVisitCalls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = ta.RenewVisit(ctx, wire.RenewVisitRequest{Token: "x", Secret: "y"})
		}()
	}
	eventually(t, "8 renewals in flight", func() bool { return len(h.fake.VisitRenewals()) == maxVisitCalls })
	start := time.Now()
	if _, err := ta.RenewVisit(context.Background(), wire.RenewVisitRequest{}); !errors.Is(err, ErrBusy) || time.Since(start) > time.Second {
		t.Fatalf("the ninth renewal: %v after %v", err, time.Since(start))
	}
	// A redeem is not crowded out.
	rctx, rcancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err := ta.RedeemVisit(rctx, wire.RedeemVisitRequest{})
	rcancel()
	if errors.Is(err, ErrBusy) || status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("a redeem beside full renewals: %v", err)
	}
	cancel()
	wg.Wait()
	// The slots are free again.
	h.fake.SetVisitDelay(0)
	if _, err := ta.RenewVisit(context.Background(), wire.RenewVisitRequest{Token: "x", Secret: "y"}); errors.Is(err, ErrBusy) {
		t.Fatalf("after the calls ended: %v", err)
	}
}
