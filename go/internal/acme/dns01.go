// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package acme

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	xacme "golang.org/x/crypto/acme"

	"github.com/gardenlinux/feos/go/internal/domains"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A wildcard's certificate comes over DNS-01 (ADR 0045): its owner
// delegates _acme-challenge.<name> with a CNAME to <token>.<ACME zone>,
// where the issuer writes the challenge's TXT record through the zone's
// DNS provider (Zone), waits until every server of the zone answers it
// (ZoneServers), accepts the challenge and deletes the record when the
// order ends.

// Zone writes the TXT records of DNS-01 challenges in the ACME zone: the
// operator's DNS provider (internal/route53 first).
type Zone interface {
	// SetTXT makes value the one TXT record at name, a name in the zone.
	SetTXT(ctx context.Context, name, value string) error
	// DeleteTXT deletes the TXT record value at name; one that is not
	// there is no error.
	DeleteTXT(ctx context.Context, name, value string) error
}

// ZoneServers asks the ACME zone's own servers for a TXT record.
type ZoneServers interface {
	// Missing returns the servers of zone that do not answer value at
	// name, and how many servers zone has; an error when they cannot be
	// found.
	Missing(ctx context.Context, zone, name, value string) (missing []string, servers int, err error)
}

// The failures of a wildcard's order that are ferund's, not the CA's.
var (
	// errNoZone: the platform has no ACME zone to answer DNS-01.
	errNoZone = errors.New("the platform has no ACME zone (--acme-zone) to answer a wildcard's DNS-01 challenge")
	// errNoDNS01: the CA offers no dns-01 challenge for the wildcard.
	errNoDNS01 = errors.New("the CA offers no DNS-01 challenge for the wildcard")
)

// notPublished is the failure of an order whose challenge record the ACME
// zone's servers did not all answer in time.
type notPublished struct {
	name    string
	missing []string
	wait    time.Duration
}

func (e *notPublished) Error() string {
	return fmt.Sprintf("the servers %s of the ACME zone did not answer the DNS-01 record %s within %v",
		strings.Join(e.missing, ", "), e.name, e.wait)
}

// The timings of DNS-01 unless a test sets others.
const (
	// DefaultZoneWait bounds the wait for the ACME zone's servers to
	// answer a challenge's record.
	DefaultZoneWait = 3 * time.Minute
	// zonePoll is how often that wait asks again.
	zonePoll = 5 * time.Second
	// zoneCleanup bounds the deletion of a challenge's record after its
	// order, which may have ended with its context.
	zoneCleanup = 30 * time.Second
)

// authorizeDNS01 satisfies a wildcard's authorization z with its dns-01
// challenge: the record at the claim's target in the ACME zone, accepted
// once every server of the zone answers it; the record is deleted when
// the authorization ends, however it ends.
func (s *Issuer) authorizeDNS01(ctx context.Context, cl *xacme.Client, o store.ACMEOrder, z *xacme.Authorization) error {
	if s.d.Zone == nil || s.cfg.ACMEZone == "" {
		return errNoZone
	}
	var chal *xacme.Challenge
	for _, c := range z.Challenges {
		if c.Type == "dns-01" {
			chal = c
			break
		}
	}
	if chal == nil {
		return errNoDNS01
	}
	value, err := cl.DNS01ChallengeRecord(chal.Token)
	if err != nil {
		return fmt.Errorf("the challenge's record: %w", err)
	}
	name := domains.Target(o.Token, s.cfg.ACMEZone)
	if err := s.d.Zone.SetTXT(ctx, name, value); err != nil {
		return fmt.Errorf("write the DNS-01 record %s in the ACME zone: %w", name, err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), zoneCleanup)
		defer cancel()
		if err := s.d.Zone.DeleteTXT(cctx, name, value); err != nil {
			s.log.Warn("acme: cannot delete a DNS-01 record; the next order of the domain replaces it",
				"domain", o.Name, "record", name, "err", err)
		}
	}()
	if err := s.awaitZone(ctx, name, value); err != nil {
		return err
	}
	if _, err := cl.Accept(ctx, chal); err != nil {
		return fmt.Errorf("accept the challenge: %w", err)
	}
	if _, err := cl.WaitAuthorization(ctx, z.URI); err != nil {
		return fmt.Errorf("wait for the authorization: %w", err)
	}
	return nil
}

// awaitZone waits until every server of the ACME zone answers value at
// name, at most ZoneWait.
func (s *Issuer) awaitZone(ctx context.Context, name, value string) error {
	deadline := time.Now().Add(s.cfg.ZoneWait)
	var last []string
	for {
		missing, servers, err := s.d.ZoneServers.Missing(ctx, s.cfg.ACMEZone, name, value)
		switch {
		case err == nil && servers > 0 && len(missing) == 0:
			return nil
		case err == nil:
			last = missing
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			last = []string{"(" + err.Error() + ")"}
		}
		if !time.Now().Before(deadline) {
			return &notPublished{name: name, missing: last, wait: s.cfg.ZoneWait}
		}
		t := time.NewTimer(min(s.cfg.ZonePoll, time.Until(deadline)))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// NetZoneServers is ZoneServers on Go's own resolver: the zone's NS
// records and their addresses come through the recursive resolver
// (server, host:port, or the system's when it is ""); the record is asked
// of every address, IPv4 ones when the zone has any, as the CA's
// resolvers may ask any of them.
type NetZoneServers struct {
	r       *net.Resolver
	timeout time.Duration
}

// NewNetZoneServers returns the zone's servers over server ("": the
// system's resolvers); timeout bounds one dial.
func NewNetZoneServers(server string, timeout time.Duration) *NetZoneServers {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	r := &net.Resolver{PreferGo: true}
	if server != "" {
		d := net.Dialer{Timeout: timeout}
		r.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, server)
		}
	}
	return &NetZoneServers{r: r, timeout: timeout}
}

// Missing implements ZoneServers.
func (z *NetZoneServers) Missing(ctx context.Context, zone, name, value string) ([]string, int, error) {
	nss, err := z.r.LookupNS(ctx, zone+".")
	if err != nil {
		return nil, 0, fmt.Errorf("the servers of %s: %w", zone, err)
	}
	var v4, v6 []string
	for _, ns := range nss {
		addrs, err := z.r.LookupNetIP(ctx, "ip", ns.Host)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			hp := net.JoinHostPort(a.Unmap().String(), "53")
			if a.Unmap().Is4() {
				v4 = append(v4, hp)
			} else {
				v6 = append(v6, hp)
			}
		}
	}
	servers := v4
	if len(servers) == 0 {
		servers = v6
	}
	if len(servers) == 0 {
		return nil, 0, fmt.Errorf("the servers of %s have no address", zone)
	}
	slices.Sort(servers)
	servers = slices.Compact(servers)
	var missing []string
	for _, srv := range servers {
		d := net.Dialer{Timeout: z.timeout}
		at := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, srv)
		}}
		txt, err := at.LookupTXT(ctx, name+".")
		if err != nil || !slices.Contains(txt, value) {
			missing = append(missing, srv)
		}
	}
	return missing, len(servers), nil
}
