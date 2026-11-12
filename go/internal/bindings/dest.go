// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"time"

	"github.com/gardenlinux/feos/go/internal/egress"
)

// DefaultDenyPrefixes is the destination deny set when --deny-prefix names
// none: egress.DefaultDeny, the private, shared, link-local, loopback,
// "this network", multicast and reserved IPv4 ranges. A binding's server
// is external by definition; the platform's own networks are
// infrastructure no binding may name.
func DefaultDenyPrefixes() []netip.Prefix { return egress.DefaultDeny() }

// DenyPolicy is the destination rule of bindings and preflights as a
// function, for the control plane's other outbound clients (the image
// resolver): nil for an IPv4 address outside deny, else an error saying
// why not.
func DenyPolicy(deny []netip.Prefix) func(netip.Addr) error { return denyPolicy(deny) }

// denyPolicy judges one address: IPv4 outside the deny set, or an error
// saying why not (egress.Rule). The platform reaches servers over IPv4
// only, so an IPv6 address is never a destination either.
func denyPolicy(deny []netip.Prefix) func(netip.Addr) error {
	return egress.Rule{Deny: deny}.Judge
}

// destRule is the destination rule as a value: the resolver and two
// policies. allowDest judges the addresses a server resolves to at
// validation; allowDial judges the address a preflight actually dials, so
// that a name resolving differently a moment later is judged again. Both
// are the deny policy in production; tests replace them to reach a
// loopback test server.
type destRule struct {
	lookup    func(ctx context.Context, host string) ([]netip.Addr, error)
	allowDest func(netip.Addr) error
	allowDial func(netip.Addr) error
}

func newDestRule(deny []netip.Prefix) destRule {
	p := denyPolicy(deny)
	return destRule{lookup: lookupIPv4, allowDest: p, allowDial: p}
}

// lookupIPv4 resolves a host's A records.
func lookupIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
}

// checkDestination applies the rule to a server host at validation: an
// address as it is, a name by every address it resolves to now. One
// internal address is enough to refuse: a name that answers with both can
// be made to answer with the internal one only. A name's refusal names no
// address it resolved to, so that the rule is no resolver of the
// platform's own networks.
func (d *destRule) checkDestination(ctx context.Context, host string) *bindingErr {
	if a, err := netip.ParseAddr(host); err == nil {
		if err := d.allowDest(a); err != nil {
			return bindingErrf(codeDestinationDenied, "server %s: %v", host, err)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	addrs, err := d.lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		why := "no IPv4 address"
		if err != nil {
			why = err.Error()
		}
		return bindingErrf(codeDestinationDenied,
			"server host %s does not resolve (%s); the destination rule needs its addresses", host, why)
	}
	for _, a := range addrs {
		if err := d.allowDest(a); err != nil {
			return bindingErrf(codeDestinationDenied, "server host %s resolves to an internal address", host)
		}
	}
	return nil
}

// errDialDenied marks a dial the rule refused: egress's refusal, under
// the type the preflights look for.
type errDialDenied struct{ err error }

func (e *errDialDenied) Error() string { return "destination denied: " + e.err.Error() }

// dialer applies the rule to every address it is about to connect to
// (egress.Dialer).
func (d *destRule) dialer() *net.Dialer {
	nd := egress.Dialer(d.allowDial, 5*time.Second)
	control := nd.Control
	nd.Control = func(network, address string, c syscall.RawConn) error {
		err := control(network, address, c)
		if denied := (*egress.DeniedError)(nil); errors.As(err, &denied) {
			return &errDialDenied{denied.Err}
		}
		return err
	}
	return nd
}
