// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The ACME zone of the tests.
const testACMEZone = "acme.fecode.test"

func wildcardHarness(t *testing.T) *harness {
	return newHarness(t, func(c *Config) { c.ACMEZone = testACMEZone })
}

// A wildcard is *. and a name; its * is the whole first label, and the
// schema's rule (migration 0037) takes what NormalizeClaim gives.
func TestNormalizeClaim(t *testing.T) {
	t.Parallel()
	rule := regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
	for in, want := range map[string]string{"*.Boxed.Example.com.": "*.boxed.example.com", "app.example.com": "app.example.com",
		" *.a.io ": "*.a.io"} {
		got, wild, err := NormalizeClaim(in)
		if err != nil || got != want || wild != strings.HasPrefix(want, "*.") || !rule.MatchString(got) {
			t.Errorf("%q: %q %v %v", in, got, wild, err)
		}
	}
	for _, in := range []string{"*.com", "*.*.example.com", "a*.example.com", "*", "*.", "x.*.example.com",
		"*." + strings.Repeat("a.", 125) + "io"} {
		if got, _, err := NormalizeClaim(in); err == nil {
			t.Errorf("%q: %q accepted", in, got)
		}
	}
	if !IsWildcard("*.a.io") || IsWildcard("a.io") || Base("*.a.io") != "a.io" || Base("a.io") != "a.io" {
		t.Fatal("IsWildcard, Base")
	}
}

// A wildcard is claimed like a name, its TXT record beside the name it
// stands on, and its answer names three records: the TXT, the wildcard's
// CNAME to the edge and its delegation to its target in the ACME zone.
// Without an ACME zone, no wildcard can be claimed; one that covers a
// reserved zone cannot be either.
func TestWildcardClaim(t *testing.T) {
	t.Parallel()
	h := wildcardHarness(t)
	got, err := h.s.CreateDomain(bg, "acme", CreateRequest{Name: "*.Boxed.Example.com"})
	if err != nil {
		t.Fatal(err)
	}
	d := h.row(uuid.MustParse(got.ID))
	target := d.Token + "." + testACMEZone
	if got.Name != "*.boxed.example.com" || !got.Wildcard || got.Records.TXT.Name != "_ferun-challenge.boxed.example.com" ||
		got.Records.TXT.Value != d.Token || got.Records.CNAME != testEdge || got.Records.Delegation == nil ||
		*got.Records.Delegation != (CNAMERecord{Name: "_acme-challenge.boxed.example.com", Value: target}) ||
		got.Found.Delegation == nil {
		t.Fatalf("answer %+v", got)
	}
	name, err := h.s.CreateDomain(bg, "acme", CreateRequest{Name: "boxed.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if name.Wildcard || name.Records.Delegation != nil || name.Records.TXT.Name != got.Records.TXT.Name {
		t.Fatalf("the name's answer %+v", name)
	}
	// One TXT record holds both claims' tokens: each is verified, on its
	// own.
	h.dns.setTXT("boxed.example.com", d.Token, name.Records.TXT.Value)
	if v := h.check(d.ID); v.Verification != store.VerificationVerified {
		t.Fatalf("wildcard %+v", v)
	}
	if v := h.check(uuid.MustParse(name.ID)); v.Verification != store.VerificationVerified {
		t.Fatalf("name %+v", v)
	}

	code := func(err error) (int, string) {
		var ae *apierr.Error
		if !errors.As(err, &ae) {
			t.Fatalf("not an API error: %v", err)
		}
		return ae.Status, ae.Code
	}
	for name, want := range map[string]string{"*.apps.fecode.test": CodeReserved, "*.fecode.test": CodeReserved,
		"*.x.fecode.dev": CodeReserved, "*.fecode.dev": CodeReserved, "*.dev": CodeInvalidName} {
		status, c := code(errOf(h.s.CreateDomain(bg, "acme", CreateRequest{Name: name})))
		if c != want || (want == CodeReserved) != (status == http.StatusUnprocessableEntity) {
			t.Errorf("%q: %d %q", name, status, c)
		}
	}
	off := newHarness(t, nil)
	if status, c := code(errOf(off.s.CreateDomain(bg, "acme", CreateRequest{Name: "*.example.com"}))); status != http.StatusConflict ||
		c != CodeWildcardsOff {
		t.Fatalf("off %d %q", status, c)
	}
}

// A wildcard is routed when a name below it resolves to the edge, asked
// of a fresh name at every check, and its delegation names its target
// through any chain of CNAMEs; the reasons name the record that is
// wrong. A wrong delegation of a routed wildcard runs the routing grace,
// and a failed lookup of the delegation changes nothing.
func TestWildcardRouting(t *testing.T) {
	t.Parallel()
	h := wildcardHarness(t)
	e := h.env("acme", "previews", true)
	d := h.claim("acme", "*.boxed.example.com")
	target := d.Token + "." + testACMEZone
	h.dns.setTXT("boxed.example.com", d.Token)
	h.check(d.ID)
	h.bind("acme", d.ID, e.Name)
	h.dns.routeToEdge("*.boxed.example.com")
	v := h.check(d.ID)
	if v.Routed || v.Delegated || v.State != StateWaitingForDNS || v.Reason != ReasonWildcardWaitingForDelegation ||
		!slices.Equal(v.Found.CNAME, []string{testEdge}) {
		t.Fatalf("without its delegation %+v", v)
	}
	h.dns.delegate("boxed.example.com", "_acme-challenge.boxed.dns-provider.test", target)
	v = h.check(d.ID)
	if !v.Routed || !v.Delegated || !v.WasRouted || v.State != StateIssuing ||
		!slices.Equal(v.Found.Delegation, []string{"_acme-challenge.boxed.dns-provider.test", target}) {
		t.Fatalf("routed %+v", v)
	}
	var probes []string
	for _, n := range h.dns.lookups {
		if strings.HasSuffix(n, ".boxed.example.com") && n != "*.boxed.example.com" {
			probes = append(probes, n)
		}
	}
	if len(probes) < 3 || slices.Contains(probes[1:], probes[0]) || strings.Count(probes[0], ".") != 3 {
		t.Fatalf("probes %v", probes)
	}

	// The names below it elsewhere: waiting, the reason names them.
	h.dns.routeTo("*.boxed.example.com", "198.51.100.7")
	v = h.check(d.ID)
	if v.Routed || !v.Delegated || v.DNSMovedSince.IsZero() || v.Reason != ReasonWildcardWaitingForDNS {
		t.Fatalf("names elsewhere %+v", v)
	}
	h.dns.routeToEdge("*.boxed.example.com")
	if v = h.check(d.ID); !v.Routed || !v.DNSMovedSince.IsZero() {
		t.Fatalf("back %+v", v)
	}

	// The delegation to another target: the routing grace, with its
	// incident naming the CNAME.
	h.dns.delegate("boxed.example.com", "other.acme.fecode.test")
	v = h.check(d.ID)
	if v.Routed || v.Delegated || v.DNSMovedSince.IsZero() || v.Reason != ReasonWildcardWaitingForDelegation ||
		after(v) != 15*time.Minute {
		t.Fatalf("delegation moved %+v", v)
	}
	open, ok, err := h.st.OpenIncidentOf(bg, e.ID, "domain/*.boxed.example.com", model.ConditionDomainRecordWrong)
	if err != nil || !ok || !strings.Contains(open.Summary, "the CNAME _acme-challenge.boxed.example.com has not named "+target) {
		t.Fatalf("incident %+v %v %v", open, ok, err)
	}

	// A delegation that does not answer changes nothing.
	h.fail("_acme-challenge.boxed.example.com")
	before := v
	v = h.check(d.ID)
	if !v.DNSMovedSince.Equal(before.DNSMovedSince) || v.Delegated != before.Delegated ||
		!strings.Contains(v.CheckError, "delegation: ") {
		t.Fatalf("failed lookup %+v", v)
	}

	// Past the grace: inactive, the reason naming the delegation.
	h.dns.delegate("boxed.example.com", "other.acme.fecode.test")
	h.exec("UPDATE domains SET dns_moved_since = now() - interval '72 hours' WHERE id = $1", d.ID)
	if v = h.check(d.ID); v.State != StateInactive || v.Reason != ReasonWildcardUndelegated {
		t.Fatalf("inactive %+v", v)
	}
	h.dns.delegate("boxed.example.com", target)
	if v = h.check(d.ID); v.State != StateIssuing || !v.Routed {
		t.Fatalf("delegated again %+v", v)
	}
}

// fail makes the lookups of a name fail (harness_test's fail sets the
// TXT's too, which a delegation has none of).
func (h *harness) fail(name string) { h.dns.fail(name) }
