// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// groupsRemedy names the groups an object may name.
func (v *validator) groupsRemedy() string {
	return "name groups of securityGroups: " + strings.Join(GroupNames(v.s), ", ")
}

// groupRefs checks the groups an object names (nil: DefaultSecurityGroup):
// one to MaxObjectGroups, each once, each a group of the spec.
func (v *validator) groupRefs(path string, names []string) {
	if names == nil {
		return
	}
	if len(names) == 0 {
		v.errorf(path, "name at least one group, or leave securityGroups out for "+DefaultSecurityGroup,
			"securityGroups is empty: every pod is in at least one group")
		return
	}
	if len(names) > MaxObjectGroups {
		v.errorf(path, fmt.Sprintf("name at most %d groups; a group may have several rules", MaxObjectGroups),
			"%d groups; at most %d are allowed", len(names), MaxObjectGroups)
	}
	seen := map[string]bool{}
	for i, n := range names {
		ip := index(path, i)
		switch {
		case seen[n]:
			v.errorf(ip, "name each group once", "group %s is named twice", n)
		case !v.hasGroup(n):
			v.errorf(ip, "declare it under securityGroups, or "+v.groupsRemedy(), "no security group %q", n)
		}
		seen[n] = true
	}
}

// hasGroup reports whether the spec has group name (DefaultSecurityGroup
// always).
func (v *validator) hasGroup(name string) bool {
	if name == DefaultSecurityGroup {
		return true
	}
	_, ok := v.s.SecurityGroups[name]
	return ok
}

// sandboxGroups are the groups of an attached service's sandbox, as its
// context reports them.
func (v *validator) sandboxGroups(sandbox string) []string {
	if id, err := podid.Parse(sandbox); err == nil {
		if info, ok := v.vc.Sandbox(id.String()); ok {
			return GroupsOf(info.SecurityGroups)
		}
	}
	return []string{DefaultSecurityGroup}
}

// securityGroups checks the groups: their names, their rules' counts,
// protocols, ports and peers (a group of the spec, a block of one family
// that --deny-prefix does not hold, or the internet).
func (v *validator) securityGroups() {
	var deny []netip.Prefix
	if d, ok := v.vc.(interface{ DenyPrefixes() []netip.Prefix }); ok {
		deny = d.DenyPrefixes()
	}
	for _, name := range slices.Sorted(maps.Keys(v.s.SecurityGroups)) {
		g := v.s.SecurityGroups[name]
		p := "securityGroups." + name
		if !IsGroupName(name) {
			v.errorf(p, "use 1 to 32 lowercase letters, digits and '-', beginning with a letter or a digit",
				"%q is not a security group's name", name)
		}
		if n := len(g.In) + len(g.Out); n > MaxGroupRules {
			v.errorf(p, fmt.Sprintf("use at most %d rules, in and out together: a rule takes several ports, or split "+
				"the group", MaxGroupRules), "%d rules; at most %d are allowed", n, MaxGroupRules)
		}
		for i, r := range g.In {
			v.rule(index(p+".in", i), r, true, deny)
		}
		for i, r := range g.Out {
			v.rule(index(p+".out", i), r, false, deny)
		}
	}
}

// rule checks one rule of a group: in (its peer is from) or out (to).
func (v *validator) rule(path string, r Rule, in bool, deny []netip.Prefix) {
	switch r.Protocol {
	case "", ProtocolTCP, ProtocolUDP:
		for i, pr := range r.Ports {
			pp := index(path+".ports", i)
			switch {
			case pr.First < 1 || pr.First > 65535 || pr.Last < 1 || pr.Last > 65535:
				v.errorf(pp, "use ports of 1..65535", "port %s is out of range", pr)
			case pr.First > pr.Last:
				v.errorf(pp, fmt.Sprintf(`write the range as "%d-%d"`, pr.Last, pr.First),
					"range %s ends before it begins", pr)
			}
		}
	case ProtocolICMP, ProtocolAny:
		if len(r.Ports) > 0 {
			v.errorf(path+".ports", `remove ports, or use protocol "tcp" or "udp"`, "protocol %s has no ports", r.Protocol)
		}
	default:
		v.errorf(path+".protocol", `use "tcp" (the default), "udp", "icmp" or "any"`, "protocol %q is unknown", r.Protocol)
	}
	peer, key, wrong, other, dir := r.From, "from", r.To, "to", "an in"
	if !in {
		peer, key, wrong, other, dir = r.To, "to", r.From, "from", "an out"
	}
	if wrong != nil {
		v.errorf(path+"."+other, "write the peer as "+key, "%s rule's peer is %s, not %s", dir, key, other)
	}
	if peer == nil {
		if wrong == nil {
			v.errorf(path+"."+key, `name the peer: {"group": name}, {"cidr": prefix} or "internet"`,
				"a rule needs a peer")
		}
		return
	}
	pp := path + "." + key
	switch {
	case peer.Group != "":
		if !v.hasGroup(peer.Group) {
			v.errorf(pp+".group", "declare it under securityGroups, or "+v.groupsRemedy(), "no security group %q",
				peer.Group)
		} else if !in {
			v.add(SeverityInfo, pp, "remove the rule, and let the group's own in rules admit this one",
				"out to a group of the environment is implied: the receiver's in rules decide")
		}
	case peer.CIDR != "":
		v.block(pp, *peer, in, deny)
	}
}

// block checks a rule's address block: a prefix in its canonical form,
// its exceptions inside it, none of it denied by --deny-prefix alone.
func (v *validator) block(path string, peer Peer, in bool, deny []netip.Prefix) {
	cidr, ok := v.prefix(path+".cidr", peer.CIDR)
	if !ok {
		return
	}
	var excepts []netip.Prefix
	for i, e := range peer.Except {
		ep := index(path+".except", i)
		x, ok := v.prefix(ep, e)
		switch {
		case !ok:
		case x.Addr().Is4() != cidr.Addr().Is4():
			v.errorf(ep, "write an exception of the block's family", "%s is of another family than %s", e, peer.CIDR)
		case x.Bits() < cidr.Bits() || !cidr.Contains(x.Addr()):
			v.errorf(ep, "write an exception inside the block, or remove it", "%s is not inside %s", e, peer.CIDR)
		default:
			excepts = append(excepts, x)
		}
	}
	var held []string
	for _, d := range deny {
		if d.Addr().Is4() != cidr.Addr().Is4() {
			continue
		}
		switch {
		case d.Bits() <= cidr.Bits() && d.Contains(cidr.Addr()):
			v.errorf(path+".cidr", "remove the rule: --deny-prefix comes first, and no rule opens it",
				"%s is inside %s, which no pod of the cluster reaches (--deny-prefix)", peer.CIDR, d)
			return
		case cidr.Bits() < d.Bits() && cidr.Contains(d.Addr()) && !excepted(d, excepts):
			held = append(held, d.String())
		}
	}
	if len(held) > 0 {
		v.add(SeverityInfo, path+".cidr", "", "%s stays closed inside %s: --deny-prefix comes first", peer.CIDR,
			strings.Join(held, ", "))
	}
	if in {
		v.add(SeverityInfo, path+".cidr", "", "a block in an in rule admits what reaches the pods directly; clients "+
			"through the ingress arrive from the ingress's addresses, and the ingress does not check blocks yet")
	}
}

// excepted reports whether p lies in one of excepts.
func excepted(p netip.Prefix, excepts []netip.Prefix) bool {
	for _, x := range excepts {
		if x.Bits() <= p.Bits() && x.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// prefix parses a prefix in its canonical form: no host bits, IPv4 not
// written as IPv6.
func (v *validator) prefix(path, s string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		if a, aerr := netip.ParseAddr(s); aerr == nil {
			v.errorf(path, fmt.Sprintf("write %s/%d for the address alone", a, a.BitLen()), "%q has no prefix length", s)
			return netip.Prefix{}, false
		}
		v.errorf(path, "write a prefix: 203.0.113.0/24, 2001:db8::/32", "%q is not a prefix", s)
		return netip.Prefix{}, false
	}
	if p.Addr().Is4In6() {
		v.errorf(path, "write it as an IPv4 prefix", "%s is IPv4 written as IPv6", s)
		return netip.Prefix{}, false
	}
	if m := p.Masked(); m != p {
		v.errorf(path, "write "+m.String()+", or the address with its full length", "%s has bits set past its length", s)
		return netip.Prefix{}, false
	}
	return p, true
}

// groupUsers refuses a spec without a group that a sandbox or a running
// tenant's job of the environment names (ADR 0035): they are pods of its
// groups that no deployment makes.
func (v *validator) groupUsers() {
	if v.mode == ModeSpec {
		return
	}
	gu, ok := v.vc.(interface{ GroupUsers() map[string][]string })
	if !ok {
		return
	}
	users := gu.GroupUsers()
	for _, name := range slices.Sorted(maps.Keys(users)) {
		if v.hasGroup(name) || len(users[name]) == 0 {
			continue
		}
		pods := slices.Sorted(slices.Values(users[name]))
		v.errorf("securityGroups."+name, "keep the group, or move them to other groups first (PUT /api/v1/sandboxes/"+
			"{id}/security-groups; a job's groups are fixed: let it end)", "security group %s is named by %s",
			name, strings.Join(pods, ", "))
	}
}

// podRef is an object as the reach check sees it.
type podRef struct {
	kind, name string
}

func (r podRef) String() string { return r.kind + " " + r.name }

// groupsOfObject are the groups an object's pods are in: its own, or its
// sandbox's for an attached service; ok false for an object that is not
// in the spec or names a group it does not have (an error already).
func (v *validator) groupsOfObject(o podRef) ([]string, bool) {
	var names []string
	switch o.kind {
	case KindService:
		svc, ok := v.s.Services[o.name]
		if !ok {
			return nil, false
		}
		if svc.Runtime == RuntimeAttached {
			return v.sandboxGroups(svc.Sandbox), true
		}
		names = svc.SecurityGroups
	case KindJob:
		j, ok := v.s.Jobs[o.name]
		if !ok {
			return nil, false
		}
		names = j.SecurityGroups
	case KindAddon:
		a, ok := v.s.Addons[o.name]
		if !ok {
			return nil, false
		}
		names = a.SecurityGroups
	default:
		return nil, false
	}
	if names != nil && len(names) == 0 {
		return nil, false
	}
	for _, n := range names {
		if !v.hasGroup(n) {
			return nil, false
		}
	}
	return normGroupNames(names), true
}

// admits reports whether an in rule of one of target's groups lets one of
// source's groups reach port over TCP (0: some port).
func (v *validator) admits(target, source []string, port int) bool {
	for _, g := range target {
		for _, r := range v.s.SecurityGroups[g].In {
			if r.From != nil && r.From.Group != "" && slices.Contains(source, r.From.Group) && r.admitsTCP(port) {
				return true
			}
		}
	}
	return false
}

// reach warns of an object that uses another one, an addon's output or a
// service's address in its env or the host of a URL there, while no in
// rule of the target's groups admits the object's groups on that port
// (ADR 0034: nothing is open by default, and the plan says what is
// missing). The warning names the rule to write.
func (v *validator) reach() {
	type use struct {
		path           string
		source, target podRef
		port           int
	}
	var uses []use
	collect := func(path string, source podRef, env map[string]EnvValue) {
		for _, key := range slices.Sorted(maps.Keys(env)) {
			val := env[key]
			kp := path + "." + key
			switch val.Kind {
			case EnvAddon:
				if a, ok := v.s.Addons[val.Addon]; ok {
					uses = append(uses, use{kp, source, podRef{KindAddon, val.Addon}, v.addonPort(val.Addon, a)})
				}
			case EnvService:
				if t, ok := v.s.Services[val.Service]; ok {
					port, _ := PortNumber(t.Ports, val.Port)
					uses = append(uses, use{kp, source, podRef{KindService, val.Service}, port})
				}
			case EnvString, EnvLiteral:
				for _, u := range v.urlTargets(val.Value) {
					uses = append(uses, use{kp, source, u.target, u.port})
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(v.s.Services)) {
		collect("services."+name+".env", podRef{KindService, name}, v.s.Services[name].Env)
	}
	for _, name := range slices.Sorted(maps.Keys(v.s.Jobs)) {
		collect("jobs."+name+".env", podRef{KindJob, name}, v.s.Jobs[name].Env)
	}
	warned := map[string]bool{}
	for _, u := range uses {
		if u.source == u.target || v.sameSandbox(u.source, u.target) {
			continue
		}
		src, ok1 := v.groupsOfObject(u.source)
		dst, ok2 := v.groupsOfObject(u.target)
		if !ok1 || !ok2 || v.admits(dst, src, u.port) {
			continue
		}
		k := u.source.String() + "→" + u.target.String() + ":" + strconv.Itoa(u.port)
		if warned[k] {
			continue
		}
		warned[k] = true
		on := ""
		if u.port > 0 {
			on = " on port " + strconv.Itoa(u.port)
		}
		rule := Rule{Protocol: ProtocolTCP, From: &Peer{Group: src[0]}}
		if u.port > 0 {
			rule.Ports = []PortRange{{First: u.port, Last: u.port}}
		}
		b, _ := json.Marshal(rule)
		v.warnf(u.path, fmt.Sprintf("add to securityGroups.%s.in: %s", dst[0], b),
			"%s reaches %s%s, but no in rule of %s's groups (%s) admits %s's (%s): its connections are dropped",
			u.source, u.target, on, u.target.name, strings.Join(dst, ", "), u.source.name, strings.Join(src, ", "))
	}
}

// sameSandbox reports whether two objects are attached services of one
// sandbox, which reach each other over its loopback.
func (v *validator) sameSandbox(a, b podRef) bool {
	if a.kind != KindService || b.kind != KindService {
		return false
	}
	sa, sb := v.s.Services[a.name], v.s.Services[b.name]
	return sa.Runtime == RuntimeAttached && sb.Runtime == RuntimeAttached && sa.Sandbox != "" &&
		strings.EqualFold(sa.Sandbox, sb.Sandbox)
}

// addonPort is the port addon name serves on (0: its blueprint does not
// say).
func (v *validator) addonPort(name string, a Addon) int {
	if b, ok := v.vc.Blueprint(a.Type); ok {
		return b.Port(name, a)
	}
	return 0
}

type urlTarget struct {
	target podRef
	port   int
}

// urlTargets are the objects of the spec that a string names as the host
// of a URL ("http://api:9000/v1", "postgres://u@db/x"), with the port: the
// URL's, else its scheme's (http 80, https 443), else the target's only
// port, else 0 (some port).
func (v *validator) urlTargets(s string) []urlTarget {
	var out []urlTarget
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == ';' }) {
		if !strings.Contains(f, "://") {
			continue
		}
		u, err := url.Parse(f)
		if err != nil || u.Host == "" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		var t podRef
		var ports []int
		if svc, ok := v.s.Services[host]; ok {
			t = podRef{KindService, host}
			for _, p := range svc.Ports {
				ports = append(ports, p.Port)
			}
		} else if a, ok := v.s.Addons[host]; ok {
			t = podRef{KindAddon, host}
			if p := v.addonPort(host, a); p > 0 {
				ports = append(ports, p)
			}
		} else {
			continue
		}
		port := 0
		switch n, err := strconv.Atoi(u.Port()); {
		case err == nil:
			port = n
		case u.Scheme == "http":
			port = 80
		case u.Scheme == "https":
			port = 443
		case len(ports) == 1:
			port = ports[0]
		}
		out = append(out, urlTarget{t, port})
	}
	return out
}
