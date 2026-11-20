// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package acme

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The ACME zone of the tests, and the token mkDomain gives a domain.
const (
	testZone   = "acme.fecore.test"
	testTarget = "abcdefghijklmnopqrstuvwxyz234567." + testZone
)

// fakeZone is the ACME zone's provider and its servers: it keeps the TXT
// records written, and its servers answer one only after it was asked
// lag times since it was written.
type fakeZone struct {
	mu      sync.Mutex
	records map[string]string
	calls   []string
	lag     int
	asked   int
	setErr  error
}

func newFakeZone() *fakeZone { return &fakeZone{records: map[string]string{}} }

func (z *fakeZone) SetTXT(_ context.Context, name, value string) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.calls = append(z.calls, "set "+name)
	if z.setErr != nil {
		return z.setErr
	}
	z.records[name], z.asked = value, 0
	return nil
}

func (z *fakeZone) DeleteTXT(_ context.Context, name, value string) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.calls = append(z.calls, "delete "+name)
	if z.records[name] == value {
		delete(z.records, name)
	}
	return nil
}

func (z *fakeZone) Missing(_ context.Context, zone, name, value string) ([]string, int, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.calls = append(z.calls, "ask "+zone+" "+name)
	z.asked++
	if z.records[name] != value || z.asked <= z.lag {
		return []string{"192.0.2.53:53"}, 2, nil
	}
	return nil, 2, nil
}

func (z *fakeZone) log() []string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return slices.Clone(z.calls)
}

// wildcardIssuer is an issuer against Pebble with the ACME zone of z.
func (f *fixture) wildcardIssuer(z *fakeZone, mod ...func(c *Config)) *Issuer {
	f.t.Helper()
	dir, client := pebble(f.t)
	cfg := Config{Directory: dir, Profile: "shortlived", HTTPClient: client, ACMEZone: testZone, ZonePoll: 10 * time.Millisecond,
		ZoneWait: 10 * time.Second, Log: dbtest.Logger(f.t)}
	for _, m := range mod {
		m(&cfg)
	}
	d := Deps{Store: f.st, Sealer: f.sl, Refresher: f.ref, ZoneServers: z}
	if z != nil {
		d.Zone = z
	}
	s, err := New(cfg, d)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// A wildcard's order goes over DNS-01 in the ACME zone: the record at its
// target is written, accepted only once the zone's servers answer it, and
// deleted when the order ends; the certificate is for the wildcard. No
// ingress instance is asked, and no HTTP-01 challenge kept.
func TestWildcardOrder(t *testing.T) {
	f := newFixture(t)
	id := f.mkDomain("*.boxed.fecore.dev", "")
	z := newFakeZone()
	z.lag = 3
	f.run(f.wildcardIssuer(z))
	_, leaf := f.certOf(id, "*.boxed.fecore.dev")
	if !slices.Equal(leaf.DNSNames, []string{"*.boxed.fecore.dev"}) {
		t.Fatalf("names %v", leaf.DNSNames)
	}
	calls := z.log()
	want := []string{"set " + testTarget}
	for range z.lag + 1 {
		want = append(want, "ask "+testZone+" "+testTarget)
	}
	want = append(want, "delete "+testTarget)
	if !slices.Equal(calls, want) {
		t.Fatalf("calls\n %v\nwant %v", calls, want)
	}
	if len(z.records) != 0 {
		t.Fatalf("records left %v", z.records)
	}
	if got := f.state(id); got != (domainState{nextIn: -1}) {
		t.Fatalf("state %+v", got)
	}
}

// A wildcard's order fails readably without an ACME zone, when its record
// cannot be written, and when the zone's servers do not answer it in
// time; the record is deleted then too.
func TestWildcardOrderFailures(t *testing.T) {
	f := newFixture(t)
	id := f.mkDomain("*.boxed.fecore.dev", "")
	f.run(f.wildcardIssuer(nil))
	if got := f.state(id); got.err != errNoZone.Error() || got.backoff != 3600 {
		t.Fatalf("no zone %+v", got)
	}

	z := newFakeZone()
	z.setErr = errors.New("route53: 403 AccessDenied: no")
	f.exec(`UPDATE domains SET next_issue_at = NULL WHERE id = $1`, id)
	f.run(f.wildcardIssuer(z))
	if got := f.state(id); !strings.Contains(got.err, "write the DNS-01 record "+testTarget) ||
		!strings.Contains(got.err, "AccessDenied") {
		t.Fatalf("not written %+v", got)
	}

	z = newFakeZone()
	z.lag = 1 << 30
	f.exec(`UPDATE domains SET next_issue_at = NULL WHERE id = $1`, id)
	f.run(f.wildcardIssuer(z, func(c *Config) { c.ZoneWait = 100 * time.Millisecond }))
	got := f.state(id)
	if !strings.Contains(got.err, "did not answer the DNS-01 record "+testTarget) {
		t.Fatalf("not published %+v", got)
	}
	if calls := z.log(); calls[len(calls)-1] != "delete "+testTarget || len(z.records) != 0 {
		t.Fatalf("calls %v, records %v", calls, z.records)
	}
	if _, err := f.st.GetDomainCert(bg, id); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("a certificate: %v", err)
	}
}
