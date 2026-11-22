// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// The types of reachability from declarations (ADR 0034, revised): the
// egress of an object, the entries of uses and reaches, and the answer
// of what a spec admits. Deriving it from a spec and checking it are
// ferund's (internal/env/spec/reach.go).

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
// project's job's pod) may reach outside their environment: the string
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
// unknown key and a value of the wrong type are errors; null is left out.
func (e *Egress) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		return nil
	}
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
// a project's job's reaches: {"service": name, "port": name or number} or
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

// Reaches is what of its environment a sandbox or a project's job reaches
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

// UnmarshalJSON reads "all" or a list of entries, strictly; null is
// left out (nothing).
func (r *Reaches) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		return nil
	}
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

// ObjectRef is "<kind>/<name>".
func ObjectRef(kind, name string) string { return kind + "/" + name }
