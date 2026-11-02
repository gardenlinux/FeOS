// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// after is the time from the domain's last answer (checked_at, the
// check's transaction time) to its next check.
func after(v store.DomainView) time.Duration { return v.NextCheckAt.Sub(v.CheckedAt) }

// A new claim is unverified and due at once; checks without its token
// back off from a minute, doubling, and a manual check starts again at a
// minute; the token verifies it, and a verified domain is checked
// hourly. What the lookup found is recorded.
func TestVerifyAndBackoff(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	d := h.claim("acme", "App.Example.com.")
	if d.Name != "app.example.com" || d.Verification != store.VerificationUnverified || d.State != StateUnverified ||
		d.Reason != ReasonUnverified || d.NextCheckAt.After(d.Now) || d.CheckBackoffS != 60 {
		t.Fatalf("new %+v", d)
	}
	h.dns.setTXT("app.example.com", "someone-else")
	v := h.check(d.ID)
	if v.Verification != store.VerificationUnverified || after(v) != time.Minute || v.CheckBackoffS != 120 ||
		v.CheckedAt.IsZero() || !slices.Equal(v.Found.TXT, []string{"someone-else"}) {
		t.Fatalf("first %+v", v)
	}
	v = h.check(d.ID)
	if after(v) != 2*time.Minute || v.CheckBackoffS != 240 {
		t.Fatalf("second %+v", v)
	}
	h.exec("UPDATE domains SET check_backoff_s = 3600 WHERE id = $1", d.ID)
	if v = h.check(d.ID); after(v) != time.Hour || v.CheckBackoffS != 3600 {
		t.Fatalf("capped %+v", v)
	}
	if _, err := h.s.CheckDomain(bg, "acme", d.ID.String()); err != nil {
		t.Fatal(err)
	}
	if v = h.row(d.ID); after(v) != time.Minute || v.CheckBackoffS != 120 {
		t.Fatalf("manual %+v", v)
	}
	h.dns.setTXT("app.example.com", "someone-else", d.Token)
	h.dns.routeToEdge("app.example.com")
	v = h.check(d.ID)
	if v.Verification != store.VerificationVerified || !v.VerifiedAt.Equal(v.CheckedAt) || v.State != StateUnbound ||
		after(v) != time.Hour || !v.Routed || v.WasRouted || !slices.Equal(v.Found.CNAME, []string{testEdge}) {
		t.Fatalf("verified %+v", v)
	}
}

// A bind starts no grace and has the domain checked at once; routed, it
// waits for its certificate (issuing); its environment's
// changes show through EnvChanged at once and through the settle loop.
// Every change of the state tells the ingress's table.
func TestBindRoute(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	e := h.env("acme", "prod", true)
	d := h.claim("acme", "shop.example.com")
	h.dns.setTXT("shop.example.com", d.Token)
	h.check(d.ID)
	notes, unsubscribe := h.d.Subscribe(db.ChanIngress)
	defer unsubscribe()
	h.exec("UPDATE domains SET next_check_at = now() + interval '1 hour', check_backoff_s = 3600 WHERE id = $1", d.ID)
	v := h.bind("acme", d.ID, "prod")
	if v.EnvID != e.ID || v.BoundAt.IsZero() || v.State != StateWaitingForDNS || v.NextCheckAt.After(v.Now) ||
		v.CheckBackoffS != 60 || v.WasRouted {
		t.Fatalf("bound %+v", v)
	}
	waitNote(t, notes, store.IngressEnvPayload(e.ID))
	// Binding where it is bound changes nothing.
	if again := h.bind("acme", d.ID, e.ID.String()); !again.BoundAt.Equal(v.BoundAt) {
		t.Fatalf("bound again %+v", again)
	}
	h.dns.routeTo("shop.example.com", "192.0.2.10", "2001:db8::10") // A and AAAA of the edge
	v = h.check(d.ID)
	if !v.Routed || !v.WasRouted || v.State != StateIssuing || v.Reason != ReasonIssuing {
		t.Fatalf("routed %+v", v)
	}
	// Only some of the addresses the edge's: not routed.
	h.dns.routeTo("shop.example.com", "192.0.2.10", "198.51.100.7")
	if v = h.check(d.ID); v.Routed || v.State != StateWaitingForDNS || v.DNSMovedSince.IsZero() {
		t.Fatalf("partly routed %+v", v)
	}
	h.dns.routeToEdge("shop.example.com")
	if v = h.check(d.ID); v.State != StateIssuing || !slices.Equal(v.Found.CNAME, []string{testEdge}) {
		t.Fatalf("routed again %+v", v)
	}
	// Unpublished in the environment's transaction: unpublished at once.
	err := h.st.InTx(bg, func(tx *store.Store) error {
		if _, err := tx.SetEnvironmentPublic(bg, e.ID, false); err != nil {
			return err
		}
		return h.s.EnvChanged(bg, tx, e.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if v = h.row(d.ID); v.State != StateUnpublished || v.Reason != ReasonNotPublic {
		t.Fatalf("unpublished %+v", v)
	}
	// Published without the hook: the settle loop catches up.
	if _, err := h.st.SetEnvironmentPublic(bg, e.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := h.s.settleAll(bg); err != nil {
		t.Fatal(err)
	}
	if v = h.row(d.ID); v.State != StateIssuing {
		t.Fatalf("published again %+v", v)
	}
}

// waitNote waits for a notification with payload.
func waitNote(t *testing.T, notes <-chan string, payload string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case p := <-notes:
			if p == payload {
				return
			}
		case <-timeout:
			t.Fatalf("no notification %q", payload)
		}
	}
}

// A routed domain whose name resolves elsewhere starts its routing grace
// (waiting-for-dns, an incident, a check every quarter of an hour, and at
// the grace's end); still elsewhere when the grace has run out, by the
// database's clock, it is inactive; routed again, it waits for its
// certificate again. Only a domain that was routed since its bind starts
// a grace.
func TestRoutingGrace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	d, e := h.routed("www.example.com")
	h.dns.routeTo("www.example.com", "198.51.100.7")
	v := h.check(d.ID)
	if v.DNSMovedSince.IsZero() || !v.DNSMovedSince.Equal(v.CheckedAt) || v.Routed || v.State != StateWaitingForDNS ||
		after(v) != 15*time.Minute {
		t.Fatalf("moved %+v", v)
	}
	if got := h.incidents(e.ID, d.Name); got[model.ConditionDomainRecordWrong] != [2]int{1, 0} {
		t.Fatalf("incidents %v", got)
	}
	since := v.DNSMovedSince
	if v = h.check(d.ID); !v.DNSMovedSince.Equal(since) {
		t.Fatalf("the grace started again %+v", v)
	}
	// Five minutes before its end the next check is at the end.
	h.exec("UPDATE domains SET dns_moved_since = now() - interval '72 hours' + interval '5 minutes' WHERE id = $1", d.ID)
	v = h.check(d.ID)
	if end := v.DNSMovedSince.Add(72 * time.Hour); !v.NextCheckAt.Equal(end) || v.State != StateWaitingForDNS {
		t.Fatalf("at the end %v, want %v: %+v", v.NextCheckAt, end, v)
	}
	// The sweep leaves a grace's end to the check.
	h.exec("UPDATE domains SET dns_moved_since = now() - interval '72 hours' WHERE id = $1", d.ID)
	if err := h.s.settleAll(bg); err != nil {
		t.Fatal(err)
	}
	if v = h.row(d.ID); v.State != StateWaitingForDNS {
		t.Fatalf("the sweep ended the grace %+v", v)
	}
	v = h.check(d.ID)
	if v.State != StateInactive || v.Verification != store.VerificationVerified || v.EnvID != e.ID ||
		after(v) != time.Hour {
		t.Fatalf("inactive %+v", v)
	}
	got := h.incidents(e.ID, d.Name)
	if got[model.ConditionDomainRecordWrong] != [2]int{0, 1} || got[model.ConditionDomainDeactivated] != [2]int{1, 0} {
		t.Fatalf("incidents %v", got)
	}
	h.dns.routeToEdge("www.example.com")
	v = h.check(d.ID)
	if !v.DNSMovedSince.IsZero() || v.State != StateIssuing || !v.Routed {
		t.Fatalf("routed again %+v", v)
	}
	if got := h.incidents(e.ID, d.Name); got[model.ConditionDomainDeactivated] != [2]int{0, 1} {
		t.Fatalf("incidents %v", got)
	}

	// Bound but never routed: no grace.
	n := h.claim("acme", "never.example.com")
	h.dns.setTXT("never.example.com", n.Token)
	h.dns.routeTo("never.example.com", "198.51.100.7")
	h.check(n.ID)
	h.bind("acme", n.ID, e.Name)
	if v = h.check(n.ID); !v.DNSMovedSince.IsZero() || v.State != StateWaitingForDNS || after(v) != time.Hour {
		t.Fatalf("never routed %+v", v)
	}
}

// A verified domain whose TXT record loses its token starts its TXT
// grace, with an incident; the token back ends it. Gone for the whole
// grace, the claim lapses, its binding kept; the token back verifies it
// again.
func TestTXTGraceAndLapse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	d, e := h.routed("api.example.com")
	h.dns.setTXT("api.example.com")
	v := h.check(d.ID)
	if !v.RecordMissingSince.Equal(v.CheckedAt) || v.State != StateIssuing || after(v) != 15*time.Minute {
		t.Fatalf("missing %+v", v)
	}
	if got := h.incidents(e.ID, d.Name); got[model.ConditionDomainRecordWrong] != [2]int{1, 0} {
		t.Fatalf("incidents %v", got)
	}
	ans, err := h.s.GetDomain(bg, "acme", d.ID.String())
	if err != nil || ans.Warnings.RecordMissingSince == nil || ans.Warnings.DeactivatesAt == nil ||
		*ans.Warnings.DeactivatesAt != rfc3339(v.RecordMissingSince.Add(72*time.Hour)) || ans.Certificate != nil {
		t.Fatalf("answer %+v %v", ans.Warnings, err)
	}
	h.dns.setTXT("api.example.com", d.Token)
	if v = h.check(d.ID); !v.RecordMissingSince.IsZero() || after(v) != time.Hour {
		t.Fatalf("back %+v", v)
	}
	if got := h.incidents(e.ID, d.Name); got[model.ConditionDomainRecordWrong] != [2]int{0, 1} {
		t.Fatalf("incidents %v", got)
	}
	h.dns.setTXT("api.example.com")
	h.check(d.ID)
	h.exec("UPDATE domains SET record_missing_since = now() - interval '72 hours' WHERE id = $1", d.ID)
	v = h.check(d.ID)
	if v.Verification != store.VerificationLapsed || v.State != StateLapsed || v.EnvID != e.ID || after(v) != time.Minute {
		t.Fatalf("lapsed %+v", v)
	}
	got := h.incidents(e.ID, d.Name)
	if got[model.ConditionDomainRecordWrong] != [2]int{0, 2} || got[model.ConditionDomainDeactivated] != [2]int{1, 0} {
		t.Fatalf("incidents %v", got)
	}
	h.dns.setTXT("api.example.com", d.Token)
	v = h.check(d.ID)
	if v.Verification != store.VerificationVerified || v.State != StateIssuing || !v.RecordMissingSince.IsZero() {
		t.Fatalf("verified again %+v", v)
	}
	if got := h.incidents(e.ID, d.Name); got[model.ConditionDomainDeactivated] != [2]int{0, 1} {
		t.Fatalf("incidents %v", got)
	}
}

// A failed lookup changes nothing but check_error and
// check_failing_since: no grace starts or ends, nothing is verified, what
// was found stays. A grace that ran out still deactivates at its end,
// since the state follows the time.
func TestFailedLookupChangesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	d, e := h.routed("app.example.com")
	h.dns.fail("app.example.com")
	v := h.check(d.ID)
	if v.CheckError == "" || !strings.Contains(v.CheckError, "SERVFAIL") || v.CheckFailingSince.IsZero() ||
		!v.CheckedAt.Equal(d.CheckedAt) || v.State != StateIssuing || !v.Routed || !v.RecordMissingSince.IsZero() ||
		!v.DNSMovedSince.IsZero() || !slices.Equal(v.Found.CNAME, d.Found.CNAME) || !slices.Equal(v.Found.TXT, d.Found.TXT) {
		t.Fatalf("failed %+v", v)
	}
	first := v.CheckFailingSince
	h.exec("UPDATE domains SET record_missing_since = now() - interval '1 hour' WHERE id = $1", d.ID)
	if v = h.check(d.ID); !v.CheckFailingSince.Equal(first) || v.RecordMissingSince.IsZero() {
		t.Fatalf("a running grace %+v", v)
	}
	ans, err := h.s.GetDomain(bg, "acme", d.ID.String())
	if err != nil || ans.Warnings.CheckFailing == "" {
		t.Fatalf("answer %+v %v", ans.Warnings, err)
	}
	// An unverified claim stays so.
	u := h.claim("acme", "new.example.com")
	h.dns.fail("new.example.com")
	if v := h.check(u.ID); v.Verification != store.VerificationUnverified || v.CheckError == "" {
		t.Fatalf("unverified %+v", v)
	}
	// The edge unknown: addresses decide nothing.
	h.dns.mu.Lock()
	h.dns.edge = nil
	h.dns.mu.Unlock()
	h.s.edge.mu.Lock()
	h.s.edge.addrs, h.s.edge.resolvedOnce, h.s.edge.tried = nil, false, time.Time{}
	h.s.edge.mu.Unlock()
	a := h.claim("acme", "addr.example.com")
	h.dns.setTXT("addr.example.com", a.Token)
	h.dns.routeTo("addr.example.com", "192.0.2.10")
	if v := h.check(a.ID); v.Verification != store.VerificationVerified || v.Routed ||
		!strings.Contains(v.CheckError, "edge's addresses are not known") {
		t.Fatalf("no edge %+v", v)
	}
	h.dns.mu.Lock()
	h.dns.edge = testEdgeAddrs
	h.dns.mu.Unlock()
	h.s.edge.mu.Lock()
	h.s.edge.addrs, h.s.edge.resolvedOnce, h.s.edge.tried = nil, false, time.Time{}
	h.s.edge.mu.Unlock()
	// Answers again: the error goes.
	h.dns.setTXT("app.example.com", d.Token)
	h.dns.routeToEdge("app.example.com")
	if v = h.check(d.ID); v.CheckError != "" || !v.CheckFailingSince.IsZero() || !v.RecordMissingSince.IsZero() {
		t.Fatalf("answered %+v", v)
	}
	// At the grace's end a failed lookup deactivates.
	h.exec("UPDATE domains SET record_missing_since = now() - interval '73 hours' WHERE id = $1", d.ID)
	h.dns.fail("app.example.com")
	if v = h.check(d.ID); v.Verification != store.VerificationLapsed {
		t.Fatalf("at the end %+v", v)
	}
	_ = e
}

// Whoever DNS shows holds the name: a verified claim whose token is gone
// while another's is there is superseded at once (its binding ends, its
// environment gets an incident); of
// several new claims shown, the oldest wins; a verified claim shown
// beside others stays verified; a superseded claim is not checked and
// cannot be bound.
func TestSupersede(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.UnverifiedMax = 10 })
	a, e := h.routed("x.example.com")
	b := h.claim("rival", "x.example.com")
	c := h.claim("acme", "x.example.com")
	// The verified one's token beside the others': it stays.
	h.dns.setTXT("x.example.com", a.Token, b.Token, c.Token)
	if v := h.check(c.ID); v.Verification != store.VerificationUnverified {
		t.Fatalf("c %+v", v)
	}
	if v := h.check(a.ID); v.Verification != store.VerificationVerified || v.State != StateIssuing {
		t.Fatalf("a %+v", v)
	}
	// Its token gone: the oldest other claim shown takes the name.
	h.dns.setTXT("x.example.com", c.Token, b.Token)
	va := h.check(a.ID)
	vb, vc := h.row(b.ID), h.row(c.ID)
	if va.Verification != store.VerificationSuperseded || va.SupersededBy != b.ID || va.EnvID != uuid.Nil ||
		va.State != StateSuperseded {
		t.Fatalf("a %+v", va)
	}
	if vb.Verification != store.VerificationVerified || vb.NextCheckAt.After(vb.Now) || vb.State != StateUnbound {
		t.Fatalf("b %+v", vb)
	}
	if vc.Verification != store.VerificationUnverified {
		t.Fatalf("c %+v", vc)
	}
	if got := h.incidents(e.ID, a.Name); got[model.ConditionDomainSuperseded] != [2]int{1, 0} {
		t.Fatalf("incidents %v", got)
	}
	// c's check while b holds the name and is shown: nothing.
	if v := h.check(c.ID); v.Verification != store.VerificationUnverified {
		t.Fatalf("c %+v", v)
	}
	// The superseded claim is not due, not checked, and not bound.
	h.exec("UPDATE domains SET next_check_at = now() - interval '1 minute'")
	var due []store.Domain
	err := h.st.InTx(bg, func(tx *store.Store) error {
		var err error
		due, err = tx.ClaimDueDomains(bg, 10, time.Minute)
		return err
	})
	if err != nil || len(due) != 2 || slices.ContainsFunc(due, func(d store.Domain) bool { return d.ID == a.ID }) {
		t.Fatalf("due %v %v", due, err)
	}
	if v := h.check(a.ID); v.Verification != store.VerificationSuperseded || !v.CheckedAt.Equal(va.CheckedAt) {
		t.Fatalf("checked %+v", v)
	}
	_, err = h.s.BindDomain(bg, "acme", a.ID.String(), e.Name)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict || ae.Code != CodeSuperseded {
		t.Fatalf("bind %v", err)
	}

	// Two new claims shown at once, none verified: the oldest wins, also
	// when the younger one is checked.
	o := h.claim("acme", "y.example.com")
	y := h.claim("rival", "y.example.com")
	h.dns.setTXT("y.example.com", y.Token, o.Token)
	h.check(y.ID)
	if vo, vy := h.row(o.ID), h.row(y.ID); vo.Verification != store.VerificationVerified ||
		vy.Verification != store.VerificationUnverified {
		t.Fatalf("o %s, y %s", vo.Verification, vy.Verification)
	}
}

// A tenant holds at most --domain-unverified-max claims that are not
// verified; names are checked and reserved zones refused; with domains
// off nothing can be claimed.
func TestCreateRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	code := func(err error) (int, string) {
		var ae *apierr.Error
		if !errors.As(err, &ae) {
			t.Fatalf("not an API error: %v", err)
		}
		return ae.Status, ae.Code
	}
	for name, want := range map[string]string{
		"bücher.example": CodeInvalidName, "*.example.com": CodeInvalidName, "10.0.0.1": CodeInvalidName,
		"apps.fecode.test": CodeReserved, "a.apps.fecode.test": CodeReserved, "fecode.dev": CodeReserved,
		"run.fecode.dev": CodeReserved, testEdge: CodeReserved, "x." + testEdge: CodeReserved,
	} {
		_, err := h.s.CreateDomain(bg, "acme", CreateRequest{Name: name})
		status, c := code(err)
		wantStatus := http.StatusBadRequest
		if want == CodeReserved {
			wantStatus = http.StatusUnprocessableEntity
		}
		if status != wantStatus || c != want {
			t.Errorf("%q: %d %q (%v)", name, status, c, err)
		}
	}
	labels := map[string]string{}
	for i := range 65 {
		labels[string(rune('a'+i%26))+strings.Repeat("x", i)] = "v"
	}
	if status, _ := code(errOf(h.s.CreateDomain(bg, "acme", CreateRequest{Name: "l.example.com", Labels: labels}))); status != 400 {
		t.Fatalf("labels %d", status)
	}
	// The cap counts every claim not verified, of the tenant only.
	first := h.claim("acme", "one.example.com")
	h.claim("acme", "one.example.com")
	h.claim("acme", "two.example.com")
	if status, c := code(errOf(h.s.CreateDomain(bg, "acme", CreateRequest{Name: "four.example.com"}))); status != http.StatusConflict ||
		c != CodeTooManyUnverified {
		t.Fatalf("cap %d %q", status, c)
	}
	h.claim("rival", "four.example.com")
	h.dns.setTXT("one.example.com", first.Token)
	h.check(first.ID)
	h.claim("acme", "four.example.com")
	// Off.
	off := newHarness(t, func(c *Config) { c.EdgeName = "" })
	if status, c := code(errOf(off.s.CreateDomain(bg, "acme", CreateRequest{Name: "a.example.com"}))); status != http.StatusConflict ||
		c != CodeDomainsOff {
		t.Fatalf("off %d %q", status, c)
	}
	if len(off.s.Loops()) != 0 || len(h.s.Loops()) != 2 {
		t.Fatal("loops")
	}
}

func errOf(_ Domain, err error) error { return err }

// Unbinding and deleting close the incidents the domain left in its
// environment; a move keeps the state; an environment's deletion unbinds
// its domains, and the settle loop derives their state.
func TestUnbindDeleteEnvDeletion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	d, e := h.routed("u.example.com")
	h.dns.setTXT("u.example.com")
	h.check(d.ID) // a TXT grace: an incident
	if _, err := h.s.UnbindDomain(bg, "acme", d.ID.String()); err != nil {
		t.Fatal(err)
	}
	v := h.row(d.ID)
	if v.EnvID != uuid.Nil || !v.BoundAt.IsZero() || v.State != StateUnbound || v.WasRouted {
		t.Fatalf("unbound %+v", v)
	}
	if got := h.incidents(e.ID, d.Name); got[model.ConditionDomainRecordWrong] != [2]int{0, 1} {
		t.Fatalf("incidents %v", got)
	}
	if _, err := h.s.UnbindDomain(bg, "acme", d.ID.String()); err != nil {
		t.Fatalf("unbound again: %v", err)
	}

	// A move keeps the state.
	g, _ := h.routed("g.example.com")
	e3 := h.env("acme", "other", true)
	if v := h.bind("acme", g.ID, e3.Name); v.EnvID != e3.ID || v.State != StateIssuing {
		t.Fatalf("moved %+v", v)
	}

	// Delete: the row goes.
	if err := h.s.DeleteDomain(bg, "acme", g.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.GetDomain(bg, "acme", g.ID.String()); err == nil {
		t.Fatal("deleted domain answered")
	}
	if err := h.s.DeleteDomain(bg, "acme", g.ID.String()); err == nil {
		t.Fatal("deleted twice")
	}

	// The environment's deletion: unbound by the foreign key; the settle
	// loop derives the state.
	k, ek := h.routed("k.example.com")
	if _, err := h.st.DeleteEnvironment(bg, ek.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.s.settleAll(bg); err != nil {
		t.Fatal(err)
	}
	if v := h.row(k.ID); v.EnvID != uuid.Nil || v.State != StateUnbound {
		t.Fatalf("environment deleted %+v", v)
	}
	// Another tenant's domain is absent.
	if _, err := h.s.GetDomain(bg, "rival", k.ID.String()); err == nil {
		t.Fatal("foreign domain answered")
	}
	if _, err := h.s.BindDomain(bg, "acme", k.ID.String(), "nope"); err == nil || !strings.Contains(err.Error(), "no such environment") {
		t.Fatalf("unknown environment %v", err)
	}
	rivalEnv := h.env("rival", "prod", true)
	if _, err := h.s.BindDomain(bg, "acme", k.ID.String(), rivalEnv.ID.String()); err == nil ||
		!strings.Contains(err.Error(), "no such environment") {
		t.Fatalf("foreign environment %v", err)
	}
	h.exec("UPDATE environments SET deleting = true WHERE id = $1", e.ID)
	if _, err := h.s.BindDomain(bg, "acme", k.ID.String(), e.Name); err == nil ||
		!strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("deleting environment %v", err)
	}
}

// The check loop claims the due domains, not the others, checks them and
// writes each result.
func TestCheckDue(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.UnverifiedMax = 10 })
	a := h.claim("acme", "a.example.com")
	b := h.claim("acme", "b.example.com")
	later := h.claim("acme", "later.example.com")
	h.exec("UPDATE domains SET next_check_at = now() + interval '1 hour' WHERE id = $1", later.ID)
	h.dns.setTXT("a.example.com", a.Token)
	if err := h.s.checkDue(bg); err != nil {
		t.Fatal(err)
	}
	if v := h.row(a.ID); v.Verification != store.VerificationVerified {
		t.Fatalf("a %+v", v)
	}
	if v := h.row(b.ID); v.CheckedAt.IsZero() || after(v) != time.Minute {
		t.Fatalf("b %+v", v)
	}
	if v := h.row(later.ID); !v.CheckedAt.IsZero() {
		t.Fatalf("later %+v", v)
	}
	h.dns.mu.Lock()
	n := len(h.dns.lookups)
	h.dns.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d lookups", n)
	}
}

// Both records wrong: one domain_record_wrong incident, its summary
// rewritten to name both and the earlier end; closed once both are right.
func TestBothGracesOneIncident(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	d, e := h.routed("both.example.com")
	h.dns.setTXT("both.example.com")
	h.check(d.ID)
	h.dns.routeTo("both.example.com", "198.51.100.7")
	v := h.check(d.ID)
	list, err := h.st.ListIncidents(bg, e.ID, true)
	if err != nil || len(list) != 1 || list[0].Condition != model.ConditionDomainRecordWrong ||
		!strings.Contains(list[0].Summary, "TXT record _ferun-challenge.both.example.com") ||
		!strings.Contains(list[0].Summary, "resolved elsewhere than "+testEdge) ||
		!strings.Contains(list[0].Summary, rfc3339(v.RecordMissingSince.Add(72*time.Hour))) || list[0].Deployment != 1 {
		t.Fatalf("incidents %+v %v", list, err)
	}
	h.dns.setTXT("both.example.com", d.Token)
	h.check(d.ID)
	if got := h.incidents(e.ID, d.Name); got[model.ConditionDomainRecordWrong] != [2]int{1, 0} {
		t.Fatalf("one grace still runs: %v", got)
	}
	h.dns.routeToEdge("both.example.com")
	h.check(d.ID)
	if got := h.incidents(e.ID, d.Name); got[model.ConditionDomainRecordWrong] != [2]int{0, 1} {
		t.Fatalf("both right: %v", got)
	}
}
