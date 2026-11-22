// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Reachability from declarations (ADR 0034, revised): what a deployment
// references, it may reach. Every reference opens the port it names, from
// the object that holds it to the object it names: an env reference to a
// service's port or an addon's output, a plain string whose URL names a
// service or an addon as its host, an entry of uses, and a route (from
// the ingress). Nothing else is open between the environment's workloads.
// A service and a job choose their egress; a route names its clients.

// AddonPort is the port addon name of a serves on (0: unknown), as its
// blueprint says.
type AddonPort func(name string, a Addon) int

// Derive reads the reachability of a spec, and the findings of its
// derivation: a warning for a URL that names a service without a port it
// declares, an error for an entry of uses that names an object or a port
// the spec lacks (the paths of the spec's objects). Edges are sorted by
// from, to and number, one per (from, to, number), with the first way it
// was found; an object's reference to itself is none.
func Derive(s Spec, addonPort AddonPort) (Reachability, []Finding) {
	d := &deriver{s: s, addonPort: addonPort}
	for _, name := range slices.Sorted(maps.Keys(s.Services)) {
		svc := s.Services[name]
		from := ObjectRef(KindService, name)
		d.env("services."+name+".env", from, svc.Env)
		if svc.Runtime != RuntimeAttached {
			d.uses("services."+name+".uses", from, svc.Uses)
			d.egress(from, svc.Egress)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Jobs)) {
		j := s.Jobs[name]
		from := ObjectRef(KindJob, name)
		d.env("jobs."+name+".env", from, j.Env)
		d.uses("jobs."+name+".uses", from, j.Uses)
		d.egress(from, j.Egress)
	}
	for _, name := range slices.Sorted(maps.Keys(s.Routes)) {
		r := s.Routes[name]
		rc := RouteClients{Route: name, Service: r.Service, Port: r.Port.String(), From: nonNilStrings(r.From)}
		if svc, ok := s.Services[r.Service]; ok {
			for _, p := range svc.Ports {
				if (r.Port.Name != "" && p.Name == r.Port.Name) || (r.Port.Name == "" && p.Port == r.Port.Number) {
					rc.Port = p.Name
					d.edge(EdgeIngress, ObjectRef(KindService, r.Service), p.Name, p.Port, "route "+name)
					break
				}
			}
		}
		d.out.Routes = append(d.out.Routes, rc)
	}
	seen := map[string]bool{}
	edges := d.out.Edges[:0]
	for _, e := range d.out.Edges {
		k := e.From + "\x00" + e.To + "\x00" + strconv.Itoa(e.Number)
		if seen[k] {
			continue
		}
		seen[k] = true
		edges = append(edges, e)
	}
	slices.SortStableFunc(edges, func(a, b Edge) int {
		if c := strings.Compare(a.From, b.From); c != 0 {
			return c
		}
		if c := strings.Compare(a.To, b.To); c != 0 {
			return c
		}
		return a.Number - b.Number
	})
	d.out.Edges = edges
	if d.out.Edges == nil {
		d.out.Edges = []Edge{}
	}
	if d.out.Egress == nil {
		d.out.Egress = []ObjectEgress{}
	}
	if d.out.Routes == nil {
		d.out.Routes = []RouteClients{}
	}
	return d.out, SortFindings(d.findings)
}

// deriver is one Derive.
type deriver struct {
	s         Spec
	addonPort AddonPort
	out       Reachability
	findings  []Finding
}

func (d *deriver) edge(from, to, port string, number int, via string) {
	if from == to || number <= 0 {
		return
	}
	d.out.Edges = append(d.out.Edges, Edge{From: from, To: to, Port: port, Number: number, Via: via})
}

// portOf is the declared port of service svc that ref names.
func portOf(svc Service, ref PortRef) (Port, bool) {
	for _, p := range svc.Ports {
		if (ref.Name != "" && p.Name == ref.Name) || (ref.Name == "" && ref.Number != 0 && p.Port == ref.Number) {
			return p, true
		}
	}
	return Port{}, false
}

// addonEdge is the edge to addon name on the port of its type; false when
// the spec has no such addon or its port is unknown.
func (d *deriver) addonEdge(from, name, via string) bool {
	a, ok := d.s.Addons[name]
	if !ok {
		return false
	}
	n := 0
	if d.addonPort != nil {
		n = d.addonPort(name, a)
	}
	if n <= 0 {
		return false
	}
	d.edge(from, ObjectRef(KindAddon, name), a.Type, n, via)
	return true
}

// env adds the edges of an object's env: a service's port (every port
// it declares for a host without one), an addon's port, a URL's host.
func (d *deriver) env(path, from string, env map[string]EnvValue) {
	for _, key := range slices.Sorted(maps.Keys(env)) {
		val := env[key]
		via := "env " + key
		switch val.Kind {
		case EnvAddon:
			d.addonEdge(from, val.Addon, via)
		case EnvService:
			svc, ok := d.s.Services[val.Service]
			if !ok {
				continue
			}
			to := ObjectRef(KindService, val.Service)
			if val.Port.IsZero() {
				for _, p := range svc.Ports {
					d.edge(from, to, p.Name, p.Port, via)
				}
				continue
			}
			if p, ok := portOf(svc, val.Port); ok {
				d.edge(from, to, p.Name, p.Port, via)
			}
		case EnvString, EnvLiteral:
			d.urls(path+"."+key, from, via, val.Value)
		}
	}
}

// urls adds the edges of the URLs in a plain string whose host is a
// service or an addon of the spec ("http://api:9000/v1",
// "postgres://u@db/x"): on the URL's port, else its scheme's (http 80,
// https 443), else, for an addon, its type's. A service's URL without a
// port, or with one the service does not declare, is a warning: the call
// would fail.
func (d *deriver) urls(path, from, via, s string) {
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == ';' }) {
		if !strings.Contains(f, "://") {
			continue
		}
		u, err := url.Parse(f)
		if err != nil || u.Host == "" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		n, perr := strconv.Atoi(u.Port())
		explicit := perr == nil
		if !explicit {
			switch strings.ToLower(u.Scheme) {
			case "http", "ws":
				n = 80
			case "https", "wss":
				n = 443
			default:
				n = 0
			}
		}
		if svc, ok := d.s.Services[host]; ok {
			to := ObjectRef(KindService, host)
			if n == 0 {
				d.warn(path, "write the port in the URL: "+portNames(svc.Ports),
					"%s names service %s without a port: the call would fail", f, host)
				continue
			}
			p, ok := portOf(svc, PortRef{Number: n})
			if !ok {
				d.warn(path, "write a port the service declares in the URL, or declare it: "+portNames(svc.Ports),
					"%s names service %s on port %d, which it does not declare: the call would fail", f, host, n)
				continue
			}
			d.edge(from, to, p.Name, p.Port, via)
			continue
		}
		a, ok := d.s.Addons[host]
		if !ok {
			continue
		}
		port := 0
		if d.addonPort != nil {
			port = d.addonPort(host, a)
		}
		if port <= 0 {
			continue
		}
		if explicit && n != port {
			d.warn(path, fmt.Sprintf("write port %d, the port of a %s addon, or none", port, a.Type),
				"%s names addon %s on port %d; it serves on %d: the call would fail", f, host, n, port)
			continue
		}
		d.edge(from, ObjectRef(KindAddon, host), a.Type, port, via)
	}
}

// uses adds the edges of an object's uses; an entry naming an object or
// a port the spec lacks is an error.
func (d *deriver) uses(path, from string, uses []Use) {
	for i, u := range uses {
		ip := index(path, i)
		switch {
		case u.Service != "" && u.Addon != "":
			d.err(ip, `name a service with its port, or an addon: {"service": name, "port": port} or {"addon": name}`,
				"an entry of uses names a service and an addon")
		case u.Addon != "":
			if !u.Port.IsZero() {
				d.err(ip+".port", "remove port: an addon is reached on the port of its type", "an addon has no port to name")
				continue
			}
			if _, ok := d.s.Addons[u.Addon]; !ok {
				d.err(ip+".addon", "name an addon of the spec: "+orNone(slices.Sorted(maps.Keys(d.s.Addons))),
					"no addon %s", u.Addon)
				continue
			}
			d.addonEdge(from, u.Addon, "uses")
		case u.Service != "":
			svc, ok := d.s.Services[u.Service]
			if !ok {
				d.err(ip+".service", "name a service of the spec: "+orNone(slices.Sorted(maps.Keys(d.s.Services))),
					"no service %s", u.Service)
				continue
			}
			if u.Port.IsZero() {
				d.err(ip+".port", portNames(svc.Ports), "an entry of uses names the service's port")
				continue
			}
			p, ok := portOf(svc, u.Port)
			if !ok {
				d.err(ip+".port", portNames(svc.Ports), "service %s has no port %s", u.Service, u.Port)
				continue
			}
			d.edge(from, ObjectRef(KindService, u.Service), p.Name, p.Port, "uses")
		default:
			d.err(ip, `write {"service": name, "port": port} or {"addon": name}`, "an entry of uses names nothing")
		}
	}
}

// egress adds an object's egress as resolved.
func (d *deriver) egress(object string, e *Egress) {
	oe := ObjectEgress{Object: object, Mode: e.EffectiveMode(), Domains: []string{}, Blocks: []Block{}}
	if oe.Mode == EgressRestricted {
		oe.Domains = append(oe.Domains, e.Domains...)
		for _, b := range e.Blocks {
			oe.Blocks = append(oe.Blocks, Block{CIDR: b.CIDR, Except: nonNilStrings(b.Except)})
		}
	}
	d.out.Egress = append(d.out.Egress, oe)
}

func (d *deriver) warn(path, remedy, format string, args ...any) {
	d.findings = append(d.findings, Finding{Severity: SeverityWarning, Path: path, Problem: fmt.Sprintf(format, args...),
		Remedy: remedy})
}

func (d *deriver) err(path, remedy, format string, args ...any) {
	d.findings = append(d.findings, Finding{Severity: SeverityError, Path: path, Problem: fmt.Sprintf(format, args...),
		Remedy: remedy})
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "it has none"
	}
	return strings.Join(names, ", ")
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clone(s)
}

// UnknownReaches is the text of a 422 for entries of a sandbox's or a
// project's job's reaches that the environment's deployed deployment s
// lacks (an object, or a service's port), naming what it has; "" when
// every entry names something of it. nil s is an environment without a
// deployment.
func UnknownReaches(r Reaches, s *Spec) string {
	if r.All || len(r.Entries) == 0 {
		return ""
	}
	if s == nil {
		s = &Spec{}
	}
	var unknown []string
	for _, u := range r.Entries {
		switch {
		case u.Addon != "":
			if _, ok := s.Addons[u.Addon]; !ok {
				unknown = append(unknown, "addon "+u.Addon)
			}
		case u.Service != "":
			svc, ok := s.Services[u.Service]
			if !ok {
				unknown = append(unknown, "service "+u.Service)
			} else if _, ok := portOf(svc, u.Port); !ok {
				unknown = append(unknown, "port "+u.Port.String()+" of service "+u.Service)
			}
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	var have []string
	for _, name := range slices.Sorted(maps.Keys(s.Services)) {
		var ports []string
		for _, p := range s.Services[name].Ports {
			ports = append(ports, p.Name)
		}
		desc := "no ports"
		if len(ports) > 0 {
			desc = strings.Join(ports, ", ")
		}
		have = append(have, "service "+name+" ("+desc+")")
	}
	for _, name := range slices.Sorted(maps.Keys(s.Addons)) {
		have = append(have, "addon "+name)
	}
	has := "it has nothing"
	if len(have) > 0 {
		has = "it has " + strings.Join(have, ", ")
	}
	return fmt.Sprintf("reaches: the environment's deployed deployment has no %s; %s", strings.Join(unknown, ", "), has)
}

// CheckReaches checks the form of a sandbox's or a project's job's
// reaches: each entry a service with its port or an addon, at most
// MaxUses; the text of a 400, or "".
func CheckReaches(r Reaches) string {
	if r.All {
		return ""
	}
	if len(r.Entries) > MaxUses {
		return fmt.Sprintf("reaches has %d entries; at most %d are allowed", len(r.Entries), MaxUses)
	}
	for i, u := range r.Entries {
		switch {
		case u.Service != "" && u.Addon != "":
			return fmt.Sprintf("reaches[%d] names a service and an addon", i)
		case u.Addon != "":
			if !u.Port.IsZero() {
				return fmt.Sprintf("reaches[%d]: an addon is reached on the port of its type; remove port", i)
			}
		case u.Service != "":
			if u.Port.IsZero() {
				return fmt.Sprintf("reaches[%d]: name the service's port", i)
			}
		default:
			return fmt.Sprintf(`reaches[%d] names nothing: write {"service": name, "port": port} or {"addon": name}`, i)
		}
	}
	return ""
}

// CheckPod checks what a sandbox's or a project's job's create says of its
// network (ADR 0035, revised): securityGroups is gone (sent: its raw
// value, nil when left out), reaches has its form (CheckReaches), and the
// egress its domains and blocks, none inside deny (EgressFindings). The
// text of a 400, or "".
func CheckPod(securityGroups []byte, r Reaches, e *Egress, deny []netip.Prefix) string {
	if securityGroups != nil {
		return "securityGroups is gone (ADR 0034, revised); use reaches"
	}
	if msg := CheckReaches(r); msg != "" {
		return msg
	}
	for _, f := range EgressFindings("egress", e, deny) {
		if f.Severity == SeverityError {
			return f.Path + ": " + f.Problem
		}
	}
	return ""
}
