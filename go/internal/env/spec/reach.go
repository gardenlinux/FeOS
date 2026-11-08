// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// ErrSecurityGroupsGone is the problem of a spec, a sandbox or a job that
// still names security groups.
const ErrSecurityGroupsGone = "securityGroups is gone (ADR 0034, revised): what an object references opens its " +
	"port; use egress, uses or a route's from"

// The modes of an egress.
const (
	// EgressInternet is the internet (an egress left out): the egress
	// table's blocks.
	EgressInternet = "internet"
	// EgressNone is nothing outside the environment.
	EgressNone = "none"
	// EgressRestricted is the domains and blocks of the object form.
	EgressRestricted = "restricted"
)

// Egress is what a service's or a job's pods (or a sandbox's or a
// tenant's job's pod) may reach outside their environment: the string
// "internet" or "none", or {"domains": […], "blocks": […]}, which allows
// those alone ({} is none).
type Egress struct {
	// Mode is EgressInternet or EgressNone for the string forms, "" for
	// the object form.
	Mode string
	// Domains are exact names or "*.suffix" (every name below suffix).
	Domains []string
	// Blocks are address blocks with their exceptions.
	Blocks []Block
}

// Block is an address block of either family with exceptions inside it.
type Block struct {
	CIDR   string   `json:"cidr"`
	Except []string `json:"except,omitempty"`
}

// EffectiveMode is the egress's mode: EgressInternet for nil, the string
// form's, EgressRestricted for an object with a domain or a block, and
// EgressNone for one without.
func (e *Egress) EffectiveMode() string {
	switch {
	case e == nil:
		return EgressInternet
	case e.Mode != "":
		return e.Mode
	case len(e.Domains) == 0 && len(e.Blocks) == 0:
		return EgressNone
	}
	return EgressRestricted
}

// RestrictsNames reports whether the pod's resolver answers upstream
// names only for the egress's domains (PodNetwork.restrict_egress_names):
// for none and restricted.
func (e *Egress) RestrictsNames() bool { return e.EffectiveMode() != EgressInternet }

// NameDomains are the domains the pod's resolver answers when it
// restricts names (PodNetwork.egress_domains): the restricted mode's,
// sorted; none otherwise.
func (e *Egress) NameDomains() []string {
	if e.EffectiveMode() != EgressRestricted || len(e.Domains) == 0 {
		return nil
	}
	out := slices.Clone(e.Domains)
	slices.Sort(out)
	return slices.Compact(out)
}

// MarshalJSON writes the string form, or the object form with the lists
// that are not empty.
func (e Egress) MarshalJSON() ([]byte, error) {
	if e.Mode != "" {
		return json.Marshal(e.Mode)
	}
	return json.Marshal(struct {
		Domains []string `json:"domains,omitempty"`
		Blocks  []Block  `json:"blocks,omitempty"`
	}{e.Domains, e.Blocks})
}

const egressForms = `want "internet", "none" or {"domains": [name, …], "blocks": [{"cidr": prefix, "except": [prefix, …]}, …]}`

// UnmarshalJSON reads one of the forms strictly: another string, an
// unknown key and a value of the wrong type are errors.
func (e *Egress) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s != EgressInternet && s != EgressNone {
			return fmt.Errorf("egress %q: %s", s, egressForms)
		}
		*e = Egress{Mode: s}
		return nil
	}
	if len(b) == 0 || b[0] != '{' {
		return errors.New(egressForms)
	}
	var obj struct {
		Domains []string `json:"domains"`
		Blocks  []Block  `json:"blocks"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obj); err != nil {
		return fmt.Errorf("egress: %v; %s", err, egressForms)
	}
	*e = Egress{Domains: obj.Domains, Blocks: obj.Blocks}
	return nil
}

// Use is an entry of a service's or a job's uses, and of a sandbox's or
// a tenant's job's reaches: {"service": name, "port": name or number} or
// {"addon": name}.
type Use struct {
	Service string  `json:"service,omitempty"`
	Port    PortRef `json:"port,omitzero"`
	Addon   string  `json:"addon,omitempty"`
}

// String is the entry as a finding names it.
func (u Use) String() string {
	if u.Addon != "" {
		return "addon " + u.Addon
	}
	return "service " + u.Service + " port " + u.Port.String()
}

// Reaches is what of its environment a sandbox or a tenant's job reaches
// (ADR 0035, revised): "all" (every port of every pod of the
// environment) or a list of entries; the zero value reaches nothing.
type Reaches struct {
	All     bool
	Entries []Use
}

// ReachesAll is the string form of Reaches.
const ReachesAll = "all"

// MarshalJSON writes "all" or the list ([] for nothing).
func (r Reaches) MarshalJSON() ([]byte, error) {
	if r.All {
		return json.Marshal(ReachesAll)
	}
	if r.Entries == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(r.Entries)
}

// UnmarshalJSON reads "all" or a list of entries, strictly.
func (r *Reaches) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s != ReachesAll {
			return fmt.Errorf(`reaches %q: want "all" or a list of {"service": name, "port": port} and {"addon": name}`, s)
		}
		*r = Reaches{All: true}
		return nil
	}
	if len(b) == 0 || b[0] != '[' {
		return errors.New(`reaches: want "all" or a list of {"service": name, "port": port} and {"addon": name}`)
	}
	var entries []Use
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return fmt.Errorf("reaches: %v", err)
	}
	*r = Reaches{Entries: entries}
	return nil
}

// Reachability is the plan's and a deployment's answer of what the spec
// admits (ADR 0034, revised): its edges, each object's egress and each
// route's clients.
type Reachability struct {
	Edges  []Edge         `json:"edges"`
	Egress []ObjectEgress `json:"egress"`
	Routes []RouteClients `json:"routes"`
}

// The caller of a route's edge.
const EdgeIngress = "ingress"

// Edge is one call the spec admits: From ("<kind>/<name>" or
// EdgeIngress) to To ("<kind>/<name>") on the port named Port, number
// Number, because of Via ("env <VAR>", "uses", "route <name>").
type Edge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Port   string `json:"port"`
	Number int    `json:"number"`
	Via    string `json:"via"`
}

// ObjectEgress is an image service's or a job's egress as resolved.
type ObjectEgress struct {
	Object  string   `json:"object"`
	Mode    string   `json:"mode"`
	Domains []string `json:"domains"`
	Blocks  []Block  `json:"blocks"`
}

// RouteClients is a route's target and the blocks of its clients (none:
// every client).
type RouteClients struct {
	Route   string   `json:"route"`
	Service string   `json:"service"`
	Port    string   `json:"port"`
	From    []string `json:"from"`
}

// AddonPort is the port addon name of a serves on (0: unknown), as its
// blueprint says.
type AddonPort func(name string, a Addon) int

// ObjectRef is "<kind>/<name>".
func ObjectRef(kind, name string) string { return kind + "/" + name }

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
// tenant's job's reaches that the environment's deployed deployment s
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
		have = append(have, "service "+name+" ("+orNone(ports)+")")
	}
	for _, name := range slices.Sorted(maps.Keys(s.Addons)) {
		have = append(have, "addon "+name)
	}
	return fmt.Sprintf("reaches: the environment's deployed deployment has no %s; it has %s",
		strings.Join(unknown, ", "), orNone(have))
}

// CheckReaches checks the form of a sandbox's or a tenant's job's
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
