// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// domainName is a lowercase DNS name of at least two labels.
var domainName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// IsEgressDomain reports whether d is a domain of an egress: a lowercase
// DNS name of at least two labels, at most 253 characters, or "*." and
// one (every name below it).
func IsEgressDomain(d string) bool {
	name := strings.TrimPrefix(d, "*.")
	return len(name) <= 253 && domainName.MatchString(name)
}

// gone refuses a field that is gone: securityGroups (ADR 0034, revised).
func (v *validator) gone(path string, raw []byte) {
	if raw != nil {
		v.errorf(path, "remove it", ErrSecurityGroupsGone)
	}
}

// denyPrefixes are the prefixes no pod of the cluster reaches, as the
// context reports them (none: no block is judged against them).
func (v *validator) denyPrefixes() []netip.Prefix {
	if d, ok := v.vc.(interface{ DenyPrefixes() []netip.Prefix }); ok {
		return d.DenyPrefixes()
	}
	return nil
}

// EgressFindings are the findings of an egress at path: its domains
// names, each once, at most MaxEgressDomains; its blocks canonical
// prefixes with exceptions inside them, of their family, none inside
// deny (--deny-prefix, which no declaration opens), at most
// MaxEgressBlocks.
func EgressFindings(path string, e *Egress, deny []netip.Prefix) []Finding {
	if e == nil || e.Mode != "" {
		return nil
	}
	v := &validator{}
	if len(e.Domains) > MaxEgressDomains {
		v.errorf(path+".domains", "allow fewer names, or a suffix with *.", "%d domains; at most %d are allowed",
			len(e.Domains), MaxEgressDomains)
	}
	seen := map[string]bool{}
	for i, d := range e.Domains {
		dp := index(path+".domains", i)
		switch {
		case !IsEgressDomain(d):
			v.errorf(dp, `write a lowercase DNS name ("api.stripe.com"), or "*." before one for every name below it`,
				"%q is not a domain", d)
		case seen[d]:
			v.errorf(dp, "name each domain once", "domain %s is named twice", d)
		}
		seen[d] = true
	}
	if len(e.Blocks) > MaxEgressBlocks {
		v.errorf(path+".blocks", "join blocks, or allow names instead", "%d blocks; at most %d are allowed",
			len(e.Blocks), MaxEgressBlocks)
	}
	for i, b := range e.Blocks {
		v.block(index(path+".blocks", i), b, deny)
	}
	return v.out
}

// egressOf checks an object's egress.
func (v *validator) egressOf(path string, e *Egress) {
	v.out = append(v.out, EgressFindings(path, e, v.denyPrefixes())...)
}

// usesOf checks the size of an object's uses; Derive checks what they
// name.
func (v *validator) usesOf(path string, uses []Use) {
	if len(uses) > MaxUses {
		v.errorf(path, fmt.Sprintf("use at most %d entries", MaxUses), "%d entries; at most %d are allowed", len(uses),
			MaxUses)
	}
}

// routeFrom checks a route's client blocks: canonical prefixes of either
// family, each once, at most MaxRouteFrom.
func (v *validator) routeFrom(path string, from []string) {
	if from == nil {
		return
	}
	if len(from) == 0 {
		v.errorf(path, "name at least one block, or leave from out for every client", "from is empty: no client "+
			"could reach the route")
		return
	}
	if len(from) > MaxRouteFrom {
		v.errorf(path, "join blocks", "%d blocks; at most %d are allowed", len(from), MaxRouteFrom)
	}
	seen := map[netip.Prefix]bool{}
	for i, f := range from {
		ip := index(path, i)
		p, ok := v.prefix(ip, f)
		if !ok {
			continue
		}
		if seen[p] {
			v.errorf(ip, "name each block once", "block %s is named twice", f)
		}
		seen[p] = true
	}
}

// reachability adds the findings of the spec's derivation (Derive): URLs
// that would fail, uses that name nothing.
func (v *validator) reachability() {
	_, fs := Derive(v.s, v.addonPort)
	v.out = append(v.out, fs...)
}

// addonPort is the port addon name serves on (0: its blueprint does not
// say).
func (v *validator) addonPort(name string, a Addon) int {
	if b, ok := v.vc.Blueprint(a.Type); ok {
		return b.Port(name, a)
	}
	return 0
}

// block checks an egress block: a prefix in its canonical form, its
// exceptions inside it and of its family, none of it inside deny.
func (v *validator) block(path string, b Block, deny []netip.Prefix) {
	cidr, ok := v.prefix(path+".cidr", b.CIDR)
	if !ok {
		return
	}
	var excepts []netip.Prefix
	for i, e := range b.Except {
		ep := index(path+".except", i)
		x, ok := v.prefix(ep, e)
		switch {
		case !ok:
		case x.Addr().Is4() != cidr.Addr().Is4():
			v.errorf(ep, "write an exception of the block's family", "%s is of another family than %s", e, b.CIDR)
		case x.Bits() < cidr.Bits() || !cidr.Contains(x.Addr()):
			v.errorf(ep, "write an exception inside the block, or remove it", "%s is not inside %s", e, b.CIDR)
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
			v.errorf(path+".cidr", "remove the block: --deny-prefix comes first, and no declaration opens it",
				"%s is inside %s, which no pod of the cluster reaches (--deny-prefix)", b.CIDR, d)
			return
		case cidr.Bits() < d.Bits() && cidr.Contains(d.Addr()) && !excepted(d, excepts):
			held = append(held, d.String())
		}
	}
	if len(held) > 0 {
		v.add(SeverityInfo, path+".cidr", "", "%s stays closed inside %s: --deny-prefix comes first", b.CIDR,
			strings.Join(held, ", "))
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

// attachedReach refuses uses and egress on an attached service: it is
// its sandbox's pod (ADR 0035), which says what it reaches.
func (v *validator) attachedReach(path string, svc Service) {
	sb := svc.Sandbox
	if sb == "" {
		sb = "its sandbox"
	} else {
		sb = "sandbox " + sb
	}
	if svc.Uses != nil {
		v.errorf(path+".uses", "remove uses; say what "+sb+" reaches (PUT /api/v1/sandboxes/{id}/reaches)",
			"an attached service runs in %s's pod, which reaches what the sandbox's reaches names", sb)
	}
	if svc.Egress != nil {
		v.errorf(path+".egress", "remove egress; a sandbox's egress is set at its create",
			"an attached service runs in %s's pod and has its egress", sb)
	}
}
