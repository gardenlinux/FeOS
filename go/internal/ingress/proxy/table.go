// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"cmp"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/routing"
)

// The proxy's plain answers.
const (
	textNoTable    = "the ingress has no routing table yet"
	textNoHost     = "no such host"
	textDotDot     = "the path has a .. segment"
	textDot        = "the path has a . segment"
	textBadEscape  = "the path is not escaped correctly"
	textNoAnswer   = "the service did not answer"
	textBadUpgrade = "the service's protocol switch failed"
	textTooMany    = "too many requests waiting"
	textDraining   = "the ingress is shutting down, try again"
	textConnect    = "CONNECT is not supported"
	textTrailers   = "request trailers are not supported"
)

// textTooManyFields answers a request with more than maxHeaderFields
// header fields.
var textTooManyFields = "the request has more than " + strconv.Itoa(maxHeaderFields) + " header fields"

// The answers of the limits: an environment's share of the held and the
// forwarded requests is taken, the instance's cap of forwarded requests
// is, or no connection to the instance came free in time.
const (
	textTooManyHere = "too many requests waiting for this host"
	textBusyHere    = "too many requests to this host, try again"
	textBusy        = "the ingress is busy, try again"
	textNoConn      = "the service is busy, try again"
)

// table is the routing the proxy serves, built from the agent's state by
// SetState and never changed afterwards.
type table struct {
	// src is the state the table was built from, to tell a change of
	// the routing from a heartbeat.
	src   *wire.State
	hosts map[string]*envRoutes
	// zones are the zones of default hosts, the apps zone and the tenant
	// zone (ADR 0031), lower case and without a trailing dot; "": none.
	zones []string
	// admitted is the version of the group ferun-ingress at which this
	// ingress entered it (Self.AdmittedVersion).
	admitted uint64
	// addrs is the set of endpoint addresses, dials what the proxy
	// dials for them: an address and a port each ("[fd00::1]:8080"),
	// with what it reaches. routed holds envService of every routed
	// service. All three cover only environments that serve a host.
	addrs  map[string]bool
	dials  map[string]dialTarget
	routed map[string]bool
	// certs are the certificates of the domains (Env.Domains) by
	// canonical host; a host without one gets the wildcard.
	certs map[string]*domainCert
	// challenges are the pending ACME HTTP-01 answers by token.
	challenges map[string]wire.Challenge
}

// domainCert is a domain's certificate, parsed, with what it was parsed
// from, so that the next table reuses it while it is the same.
type domainCert struct {
	chain [][]byte
	key   []byte
	cert  *tls.Certificate
}

// dialTarget is what a dial address reaches: an endpoint (its address)
// of a service of an environment (its ID).
type dialTarget struct {
	env, service, address string
}

// envService is the key of a service of an environment in table.routed.
func envService(env, service string) string {
	return env + "/" + service
}

// envRoutes is one environment's routes: sorted by the number of their
// path's segments, longest first, then by name, with prefixes in the
// same order.
type envRoutes struct {
	id       string
	tenant   string
	routes   []wire.Route
	prefixes []string
	services map[string]*service
}

// service is one routed service: its endpoints in the table's order, the
// counts of its other instances, and the round robin's position.
type service struct {
	name      string
	idle      bool
	endpoints []endpoint
	asleep    int
	waking    int
	pending   int
	next      atomic.Uint64
}

// endpoint is one instance that answers (wire.Endpoint).
type endpoint struct {
	instance     string
	address      string
	node         string
	ports        map[string]int
	groupVersion uint64
}

// canonicalHost is a request's host as the table keys it: lower case,
// without a port and a trailing dot.
func canonicalHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// buildTable builds the routing of a state. Of two environments that
// claim one host (the control plane never sends that), the one whose ID
// sorts first serves it. A domain's certificate is taken parsed from old
// (the table before; nil: none) when it did not change.
func buildTable(s *wire.State, log *slog.Logger, old *table) *table {
	t := &table{
		src:        s.Clone(),
		hosts:      map[string]*envRoutes{},
		zones:      zonesOf(s.Self),
		admitted:   s.Self.AdmittedVersion,
		addrs:      map[string]bool{},
		dials:      map[string]dialTarget{},
		routed:     map[string]bool{},
		certs:      map[string]*domainCert{},
		challenges: map[string]wire.Challenge{},
	}
	for _, c := range s.Challenges {
		if c.Token != "" {
			t.challenges[c.Token] = c
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.Envs)) {
		e := s.Envs[id]
		er := buildEnv(e)
		served := false
		for _, h := range e.Hosts {
			h = canonicalHost(h)
			if h == "" {
				continue
			}
			if other, ok := t.hosts[h]; ok {
				log.Warn("two environments claim one host", "host", h, "env", id, "served", other.id)
				continue
			}
			t.hosts[h] = er
			served = true
		}
		if served {
			t.add(er)
		}
		for _, d := range e.Domains {
			h := canonicalHost(d.Name)
			if t.hosts[h] != er || t.certs[h] != nil {
				continue // not this environment's host, or twice
			}
			var prev *domainCert
			if old != nil {
				prev = old.certs[h]
			}
			dc, err := parseDomain(h, d, prev)
			if err != nil {
				log.Warn("a domain's certificate does not load; the host gets the wildcard", "host", h, "env", id, "err", err)
				continue
			}
			t.certs[h] = dc
		}
	}
	return t
}

// parseDomain parses a domain's certificate for host, or takes prev when
// it was parsed from the same chain and key.
func parseDomain(host string, d wire.Domain, prev *domainCert) (*domainCert, error) {
	if prev != nil && bytes.Equal(prev.key, d.Key) && slices.EqualFunc(prev.chain, d.Chain, bytes.Equal) {
		return prev, nil
	}
	if len(d.Chain) == 0 {
		return nil, errors.New("no certificate")
	}
	leaf, err := x509.ParseCertificate(d.Chain[0])
	if err != nil {
		return nil, fmt.Errorf("the certificate: %w", err)
	}
	if err := leaf.VerifyHostname(host); err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(d.Key)
	if err != nil {
		return nil, fmt.Errorf("the key: %w", err)
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("the key is a %T", k)
	}
	pub, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(leaf.PublicKey) {
		return nil, errors.New("the key is not the certificate's")
	}
	chain := make([][]byte, len(d.Chain))
	for i, c := range d.Chain {
		chain[i] = bytes.Clone(c)
	}
	return &domainCert{chain: chain, key: bytes.Clone(d.Key),
		cert: &tls.Certificate{Certificate: chain, PrivateKey: signer, Leaf: leaf}}, nil
}

// challenge returns the pending challenge of token.
func (t *table) challenge(token string) (wire.Challenge, bool) {
	if t == nil || token == "" {
		return wire.Challenge{}, false
	}
	c, ok := t.challenges[token]
	return c, ok
}

// cert returns the certificate of a domain by its canonical host; nil
// when the host has none (the wildcard serves it).
func (t *table) cert(host string) *tls.Certificate {
	if t == nil {
		return nil
	}
	if dc := t.certs[host]; dc != nil {
		return dc.cert
	}
	return nil
}

// add records the endpoints and services of an environment that serves
// a host.
func (t *table) add(er *envRoutes) {
	for _, svc := range er.services {
		t.routed[envService(er.id, svc.name)] = true
		for _, ep := range svc.endpoints {
			t.addrs[ep.address] = true
			for _, port := range ep.ports {
				if port > 0 {
					d := net.JoinHostPort(ep.address, strconv.Itoa(port))
					t.dials[d] = dialTarget{env: er.id, service: svc.name, address: ep.address}
				}
			}
		}
	}
}

func buildEnv(e wire.Env) *envRoutes {
	er := &envRoutes{id: e.ID, tenant: e.Tenant, routes: slices.Clone(e.Routes), services: map[string]*service{}}
	segments := func(r wire.Route) int {
		p := strings.Trim(r.Path, "/")
		if p == "" {
			return 0
		}
		return strings.Count(p, "/") + 1
	}
	slices.SortStableFunc(er.routes, func(a, b wire.Route) int {
		return cmp.Or(cmp.Compare(segments(b), segments(a)), cmp.Compare(a.Name, b.Name))
	})
	for _, r := range er.routes {
		er.prefixes = append(er.prefixes, r.Path)
	}
	for _, s := range e.Services {
		svc := &service{name: s.Name, idle: s.Idle, asleep: s.Asleep, waking: s.Waking, pending: s.Pending}
		for _, ep := range s.Endpoints {
			if ep.Address == "" {
				continue
			}
			svc.endpoints = append(svc.endpoints, endpoint{
				instance:     ep.Instance,
				address:      ep.Address,
				node:         ep.Node,
				ports:        maps.Clone(ep.Ports),
				groupVersion: ep.GroupVersion,
			})
		}
		er.services[s.Name] = svc
	}
	return er
}

// target is where the routing sends a request: the environment, the
// route, its service and the path the upstream gets (unescaped, and
// escaped when that is not the default escaping).
type target struct {
	env     *envRoutes
	route   *wire.Route
	svc     *service
	path    string
	rawPath string
}

// resolve routes a request by its canonical host and its escaped path,
// or returns the answer when the routing does not send it anywhere.
func (t *table) resolve(host, raw string) (target, *reply) {
	if t == nil {
		return target{}, &reply{http.StatusServiceUnavailable, textNoTable, 5}
	}
	e := t.hosts[host]
	if e == nil {
		return target{}, &reply{http.StatusNotFound, textNoHost, 0}
	}
	switch routing.DotSegment(raw) {
	case "..":
		return target{}, &reply{http.StatusBadRequest, textDotDot, 0}
	case ".":
		return target{}, &reply{http.StatusBadRequest, textDot, 0}
	}
	path, err := url.PathUnescape(raw)
	if err != nil {
		return target{}, &reply{http.StatusBadRequest, textBadEscape, 0}
	}
	i, n, ok := routing.Match(e.prefixes, raw)
	if !ok {
		return target{}, &reply{http.StatusNotFound, "no route for " + path, 0}
	}
	r := &e.routes[i]
	if r.Door {
		return target{}, &reply{http.StatusNotFound, "route " + r.Path + " is served by the route door", 0}
	}
	svc := e.services[r.Service]
	if svc == nil {
		return target{}, &reply{http.StatusServiceUnavailable, "no ready instance of " + r.Service, 5}
	}
	tg := target{env: e, route: r, svc: svc, path: path, rawPath: routing.RawOf(path, raw)}
	if r.Strip {
		if tg.path, tg.rawPath, err = routing.StripSegments(raw, n); err != nil {
			return target{}, &reply{http.StatusBadRequest, textBadEscape, 0}
		}
	}
	return tg, nil
}

// zonesOf are the zones of default hosts that s names, canonical.
func zonesOf(s wire.Self) []string {
	var out []string
	for _, z := range []string{s.AppsZone, s.TenantZone} {
		if z = canonicalHost(z); z != "" {
			out = append(out, z)
		}
	}
	return out
}

// underZone reports whether a canonical host lies under a zone of default
// hosts: the apps zone or the tenant zone.
func (t *table) underZone(host string) bool {
	if t == nil {
		return false
	}
	for _, z := range t.zones {
		if strings.HasSuffix(host, "."+z) {
			return true
		}
	}
	return false
}
