// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package domains

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// DNS is what a check asks of DNS: *Resolver, and fakes in tests.
type DNS interface {
	// Lookup asks for a domain's TXT record and its addresses.
	Lookup(ctx context.Context, q Query) Answer
	// Resolve asks for name's addresses (the edge's). A name that does
	// not exist is no error: no addresses.
	Resolve(ctx context.Context, name string) ([]netip.Addr, error)
}

// Query is one check's question.
type Query struct {
	// Name is the name whose addresses route the domain: the domain's
	// name, or for a wildcard a fresh name below it; TXTName the name of
	// its TXT record, <label>.<name>.
	Name, TXTName string
	// Delegation is a wildcard's _acme-challenge.<name>, whose CNAME
	// chain is asked too; "" for a name.
	Delegation string
}

// Answer is what one check's lookups found. A part whose lookup failed
// (a timeout, SERVFAIL, no server reached) has its error set; an answer
// that denies the name or the record (NXDOMAIN, no such records) is no
// error, but an empty part.
type Answer struct {
	// TXT are the values of the TXT records at TXTName.
	TXT    []string
	TXTErr error
	// Addresses are what Name resolves to, through any CNAME or ALIAS;
	// CNAME is its canonical name when that differs from Name, shown for
	// information only.
	Addresses []netip.Addr
	CNAME     []string
	RouteErr  error
	// Delegation is the CNAME chain of the query's Delegation, each
	// asked at its zone's servers; empty when it has no CNAME.
	Delegation    []string
	DelegationErr error
}

// Resolver is the DNS of the checks, on Go's own resolver (net.Resolver
// with PreferGo). A domain's records are asked of the servers of the zone
// that holds them, not of a cache: a record just set counts at once, and
// no negative answer a recursive resolver keeps for its zone's negative
// TTL (up to an hour) holds a claim back. Only the way to those servers
// comes through the recursive resolver (--dns-resolver, else the
// system's): the zone's NS records and their addresses, which rarely
// change. A CNAME into another zone is followed at that zone's servers.
// The edge's own addresses (Resolve) come from the recursive resolver.
// Names are asked absolute (with a trailing dot), so that no search
// domain is tried.
type Resolver struct {
	r       *net.Resolver
	timeout time.Duration
}

// maxCNAMEs bounds the CNAMEs a name's addresses are followed through.
const maxCNAMEs = 8

// NewResolver returns the checks' resolver: over server (host:port) when
// it is not "", else over the system's resolvers; timeout bounds one dial.
func NewResolver(server string, timeout time.Duration) *Resolver {
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
	return &Resolver{r: r, timeout: timeout}
}

// absolute is name with a trailing dot.
func absolute(name string) string { return strings.TrimSuffix(name, ".") + "." }

// notFound reports whether err is an answer that denies the name or the
// record: NXDOMAIN, or no records of the type. Every other error, a
// timeout, SERVFAIL, a refusal or no server reached, is a failed lookup.
func notFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound && !de.IsTimeout && !de.IsTemporary
}

// Lookup answers q at the servers of the zones that hold its names: the
// TXT record, the addresses Name ends in through its CNAMEs, which it
// lists, and a wildcard's delegation: the CNAMEs of q.Delegation.
func (r *Resolver) Lookup(ctx context.Context, q Query) Answer {
	var a Answer
	a.TXT, a.TXTErr = r.txt(ctx, q.TXTName)
	a.Addresses, a.CNAME, a.RouteErr = r.route(ctx, q.Name)
	if q.Delegation != "" {
		// Where the chain ends need not exist: the ACME zone holds a
		// record at a target only while an order runs.
		_, a.Delegation, a.DelegationErr = r.route(ctx, q.Delegation)
	}
	return a
}

// txt is the TXT record at name, asked of its zone's servers.
func (r *Resolver) txt(ctx context.Context, name string) ([]string, error) {
	zone, at, err := r.zone(ctx, name)
	if err != nil {
		return nil, err
	}
	txt, err := at.LookupTXT(ctx, absolute(name))
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, failed(name, zone, err)
	}
	return txt, nil
}

// route follows name's CNAMEs, each at the servers of its own zone, and
// returns the addresses it ends in (none: the name or its end does not
// exist) and the names it went through.
func (r *Resolver) route(ctx context.Context, name string) ([]netip.Addr, []string, error) {
	var chain []string
	n := name
	for range maxCNAMEs + 1 {
		zone, at, err := r.zone(ctx, n)
		if err != nil {
			return nil, chain, err
		}
		cn, err := at.LookupCNAME(ctx, absolute(n))
		if err != nil && !notFound(err) {
			return nil, chain, failed(n, zone, err)
		}
		if cn = clean(cn); err == nil && cn != "" && cn != n {
			chain = append(chain, cn)
			n = cn
			continue
		}
		ips, err := at.LookupIPAddr(ctx, absolute(n))
		if err != nil {
			if notFound(err) {
				return nil, chain, nil
			}
			return nil, chain, failed(n, zone, err)
		}
		out := make([]netip.Addr, 0, len(ips))
		for _, ip := range ips {
			if a, ok := netip.AddrFromSlice(ip.IP); ok {
				out = append(out, a)
			}
		}
		return sortAddrs(out), chain, nil
	}
	return nil, chain, fmt.Errorf("%s leads through more than %d CNAMEs", name, maxCNAMEs)
}

// zone finds the zone that holds name, the nearest of name and its
// parents that has NS records and owns no CNAME (an NS query for a
// CNAME's owner answers with its target's NS records), and returns its
// name and a resolver that asks its servers, IPv4 ones when it has any.
func (r *Resolver) zone(ctx context.Context, name string) (string, *net.Resolver, error) {
	for n := name; strings.Contains(n, "."); n = n[strings.IndexByte(n, '.')+1:] {
		if cn, err := r.r.LookupCNAME(ctx, absolute(n)); err == nil && clean(cn) != "" && clean(cn) != n {
			continue
		}
		nss, err := r.r.LookupNS(ctx, absolute(n))
		if err != nil {
			if notFound(err) {
				continue
			}
			return "", nil, fmt.Errorf("find the zone of %s: %w", name, err)
		}
		if len(nss) == 0 {
			continue
		}
		var v4, v6 []string
		for _, ns := range nss {
			addrs, err := r.Resolve(ctx, clean(ns.Host))
			if err != nil {
				continue
			}
			for _, a := range addrs {
				hp := net.JoinHostPort(a.String(), "53")
				if a.Is4() {
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
			return "", nil, fmt.Errorf("find the zone of %s: no address for the servers of %s", name, n)
		}
		return n, r.at(servers), nil
	}
	return "", nil, fmt.Errorf("find the zone of %s: no NS records for it or a parent", name)
}

// at is a resolver that asks servers (ip:port), the next one at every
// dial, so that a retry goes to another server.
func (r *Resolver) at(servers []string) *net.Resolver {
	var next atomic.Uint32
	d := net.Dialer{Timeout: r.timeout}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, servers[int(next.Add(1)-1)%len(servers)])
	}}
}

// clean is a DNS name lowercased, without its trailing dot.
func clean(name string) string { return strings.ToLower(strings.TrimSuffix(name, ".")) }

// failed is a failed lookup of name at zone's servers. Go's DNSError
// names the system's resolver as the server, also when the dial went to
// the zone's: the text names the zone instead.
func failed(name, zone string, err error) error {
	var de *net.DNSError
	if errors.As(err, &de) {
		return fmt.Errorf("lookup %s at the servers of %s: %s", name, zone, de.Err)
	}
	return fmt.Errorf("lookup %s at the servers of %s: %w", name, zone, err)
}

// Resolve returns name's A and AAAA records, IPv4 ones unmapped, sorted.
func (r *Resolver) Resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	ips, err := r.r.LookupIPAddr(ctx, absolute(name))
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip.IP); ok {
			out = append(out, a)
		}
	}
	return sortAddrs(out), nil
}

// sortAddrs sorts and dedups addresses, IPv4 ones unmapped.
func sortAddrs(as []netip.Addr) []netip.Addr {
	for i, a := range as {
		as[i] = a.Unmap()
	}
	slices.SortFunc(as, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(as)
}
