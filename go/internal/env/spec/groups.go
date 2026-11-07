// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// DefaultSecurityGroup is the group every environment has (ADR 0034): an
// object that names no group is in it. Without rules it lets nothing in
// and reaches the internet.
const DefaultSecurityGroup = "default"

// The protocols of a rule.
const (
	ProtocolTCP = "tcp"
	ProtocolUDP = "udp"
	// ProtocolICMP is ICMP of either family (ICMPv6 for IPv6).
	ProtocolICMP = "icmp"
	// ProtocolAny is every protocol, every port.
	ProtocolAny = "any"
)

// PeerInternet is the peer "internet": the global unicast blocks of both
// families without the special-purpose ranges (ADR 0018's egress table).
const PeerInternet = "internet"

// groupName is a security group's name: 1 to 32 of lowercase letters,
// digits and '-', beginning with a letter or a digit.
var groupName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// IsGroupName reports whether name is a security group's name.
func IsGroupName(name string) bool { return groupName.MatchString(name) }

// SecurityGroup is a security group of an environment (ADR 0034): rules
// that only allow, without order, stateful. Its members are the pods of
// the objects that name it (and, ADR 0035, the sandboxes and tenants'
// jobs of the environment that do).
type SecurityGroup struct {
	// In are what may reach the group's members; none: nothing, not even
	// a pod of the same environment.
	In []Rule `json:"in,omitempty"`
	// Out are what the members may reach beyond the environment (a pod
	// may always send to a pod of its own environment, whose in rules
	// decide). nil (left out) is [{"protocol": "any", "to": "internet"}],
	// which Normalise writes; [] is none. It is never left out when
	// written, so that [] stays none.
	Out []Rule `json:"out,omitzero"`
}

// Rule is one rule of a group: a protocol, its ports and exactly one
// peer, From in an in rule, To in an out rule.
type Rule struct {
	// Protocol is ProtocolTCP (when left out; Normalise writes it),
	// ProtocolUDP, ProtocolICMP or ProtocolAny.
	Protocol string `json:"protocol,omitempty"`
	// Ports are TCP's and UDP's ports and ranges; none: every port. Not
	// for icmp and any.
	Ports []PortRange `json:"ports,omitempty"`
	// From is an in rule's peer, To an out rule's.
	From *Peer `json:"from,omitempty"`
	To   *Peer `json:"to,omitempty"`
}

// ProtocolOf is the rule's protocol, ProtocolTCP when it is left out.
func (r Rule) ProtocolOf() string {
	if r.Protocol == "" {
		return ProtocolTCP
	}
	return r.Protocol
}

// admitsTCP reports whether the rule lets TCP to port through (0: some
// port, whichever).
func (r Rule) admitsTCP(port int) bool {
	switch r.ProtocolOf() {
	case ProtocolAny:
		return true
	case ProtocolTCP:
	default:
		return false
	}
	if len(r.Ports) == 0 || port == 0 {
		return true
	}
	for _, p := range r.Ports {
		if port >= p.First && port <= p.Last {
			return true
		}
	}
	return false
}

// Peer is a rule's far end: exactly one of a group of the same
// environment ({"group": name}), an address block ({"cidr": prefix,
// "except": [prefix…]}) and the internet (the string "internet").
type Peer struct {
	Group string
	CIDR  string
	// Except are prefixes inside CIDR, of its family, that the block
	// leaves out.
	Except   []string
	Internet bool
}

// MarshalJSON writes the peer's form.
func (p Peer) MarshalJSON() ([]byte, error) {
	switch {
	case p.Internet:
		return json.Marshal(PeerInternet)
	case p.Group != "":
		return json.Marshal(struct {
			Group string `json:"group"`
		}{p.Group})
	case p.CIDR != "":
		return json.Marshal(struct {
			CIDR   string   `json:"cidr"`
			Except []string `json:"except,omitempty"`
		}{p.CIDR, p.Except})
	}
	return nil, errors.New("a peer names nothing")
}

const peerForms = `want "internet", {"group": name} or {"cidr": prefix, "except": [prefix, …]}`

// UnmarshalJSON reads one of the peer's forms, strictly: another string,
// an unknown key, two forms at once and a value of the wrong type are
// errors.
func (p *Peer) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s != PeerInternet {
			return fmt.Errorf("peer %q: %s", s, peerForms)
		}
		*p = Peer{Internet: true}
		return nil
	}
	var m map[string]json.RawMessage
	if len(b) == 0 || b[0] != '{' || json.Unmarshal(b, &m) != nil {
		return errors.New(peerForms)
	}
	str := func(k string) (string, error) {
		var s string
		if raw := bytes.TrimSpace(m[k]); len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil || s == "" {
			return "", fmt.Errorf("%s: want a non-empty string", k)
		}
		return s, nil
	}
	var out Peer
	var err error
	_, isGroup := m["group"]
	_, isCIDR := m["cidr"]
	switch {
	case isGroup && len(m) == 1:
		out.Group, err = str("group")
	case isCIDR && !isGroup:
		for k, raw := range m {
			switch k {
			case "cidr":
				out.CIDR, err = str(k)
			case "except":
				if raw = bytes.TrimSpace(raw); len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &out.Except) != nil {
					err = errors.New("except: want a list of prefixes")
				}
			default:
				err = fmt.Errorf("a block has cidr and except, not %q", k)
			}
			if err != nil {
				break
			}
		}
	default:
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return fmt.Errorf("peer with keys %s: %s", strings.Join(keys, ", "), peerForms)
	}
	if err != nil {
		return err
	}
	*p = out
	return nil
}

// String is the peer as a finding names it.
func (p Peer) String() string {
	switch {
	case p.Internet:
		return PeerInternet
	case p.Group != "":
		return "group " + p.Group
	}
	if len(p.Except) > 0 {
		return p.CIDR + " except " + strings.Join(p.Except, ", ")
	}
	return p.CIDR
}

// PortRange is a port (First == Last) or a range of ports; in JSON a
// number or the string "first-last".
type PortRange struct {
	First, Last int
}

// MarshalJSON writes a single port as a number, a range as "a-b".
func (r PortRange) MarshalJSON() ([]byte, error) {
	if r.First == r.Last {
		return json.Marshal(r.First)
	}
	return json.Marshal(strconv.Itoa(r.First) + "-" + strconv.Itoa(r.Last))
}

// UnmarshalJSON reads an integer or a string "a-b" of two integers.
func (r *PortRange) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		a, z, ok := strings.Cut(s, "-")
		first, err1 := strconv.Atoi(a)
		last, err2 := strconv.Atoi(z)
		if !ok || err1 != nil || err2 != nil || !digits(a) || !digits(z) {
			return fmt.Errorf(`port range %q: want a number, or "first-last" as "9000-9010"`, s)
		}
		*r = PortRange{First: first, Last: last}
		return nil
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || !digits(string(b)) {
		return fmt.Errorf(`port %s: want a number, or "first-last" as "9000-9010"`, b)
	}
	*r = PortRange{First: n, Last: n}
	return nil
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (r PortRange) String() string {
	if r.First == r.Last {
		return strconv.Itoa(r.First)
	}
	return strconv.Itoa(r.First) + "-" + strconv.Itoa(r.Last)
}

// internetOut is the out rule a group without out rules gets.
func internetOut() []Rule {
	return []Rule{{Protocol: ProtocolAny, To: &Peer{Internet: true}}}
}

// normGroups are a spec's groups normalised, DefaultSecurityGroup added
// when absent: every rule's protocol explicit, its ports sorted without
// duplicates, its blocks in their canonical form, a missing out the
// internet.
func normGroups(groups map[string]SecurityGroup) map[string]SecurityGroup {
	out := make(map[string]SecurityGroup, len(groups)+1)
	for name, g := range groups {
		out[name] = normGroup(g)
	}
	if _, ok := out[DefaultSecurityGroup]; !ok {
		out[DefaultSecurityGroup] = normGroup(SecurityGroup{})
	}
	return out
}

func normGroup(g SecurityGroup) SecurityGroup {
	var out SecurityGroup
	for _, r := range g.In {
		out.In = append(out.In, normRule(r))
	}
	if g.Out == nil {
		out.Out = internetOut()
		return out
	}
	out.Out = make([]Rule, 0, len(g.Out))
	for _, r := range g.Out {
		out.Out = append(out.Out, normRule(r))
	}
	return out
}

func normRule(r Rule) Rule {
	out := Rule{Protocol: r.ProtocolOf(), From: normPeer(r.From), To: normPeer(r.To)}
	if len(r.Ports) > 0 {
		out.Ports = slices.Clone(r.Ports)
		slices.SortFunc(out.Ports, func(a, b PortRange) int {
			if a.First != b.First {
				return a.First - b.First
			}
			return a.Last - b.Last
		})
		out.Ports = slices.Compact(out.Ports)
	}
	return out
}

// normPeer writes a block and its exceptions as netip does (an IPv6
// prefix in lowercase, compressed), the exceptions sorted; one that is
// none stays as written (Validate refused it).
func normPeer(p *Peer) *Peer {
	if p == nil {
		return nil
	}
	out := *p
	out.CIDR = canonPrefix(p.CIDR)
	if len(p.Except) > 0 {
		out.Except = make([]string, len(p.Except))
		for i, e := range p.Except {
			out.Except[i] = canonPrefix(e)
		}
		slices.Sort(out.Except)
		out.Except = slices.Compact(out.Except)
	}
	return &out
}

func canonPrefix(s string) string {
	if s == "" {
		return s
	}
	if p, err := netip.ParsePrefix(s); err == nil && p == p.Masked() {
		return p.String()
	}
	return s
}

// normGroupNames are an object's group names sorted, without duplicates;
// none: [DefaultSecurityGroup].
func normGroupNames(names []string) []string {
	if names == nil {
		return []string{DefaultSecurityGroup}
	}
	out := slices.Clone(names)
	slices.Sort(out)
	return slices.Compact(out)
}

// GroupsOf are the groups an object's pods are in, as a spec names them:
// its own, [DefaultSecurityGroup] when it names none.
func GroupsOf(names []string) []string {
	if len(names) == 0 {
		return []string{DefaultSecurityGroup}
	}
	return names
}

// GroupNames are the names of a spec's groups, sorted, with
// DefaultSecurityGroup, which every environment has.
func GroupNames(s Spec) []string {
	out := []string{DefaultSecurityGroup}
	for name := range s.SecurityGroups {
		out = append(out, name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
