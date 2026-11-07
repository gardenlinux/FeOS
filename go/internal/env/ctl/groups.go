// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The security groups of environments (§4.5, ADR 0034, 0035), and the
// ingress's (ADR 0021):
//
//   - env-<env id>, the set of all the pods of an environment: its
//     instances, its sandboxes and its tenants' jobs' pods. No in rules;
//     its one rule is out to itself, the implied rule: a pod may send to
//     every pod of its environment, and the receiver's groups decide;
//   - sg-<row id>, a tenant's group (store's security_groups): its rules
//     as the environment's spec writes them (groupSpec), peers that are
//     groups as their sg-<row id>, blocks as address blocks, the internet
//     as the egress table's blocks; its members the pods whose template
//     names it, an instance's as its object names it, a sandbox's and a
//     job's pod's as it was made in it (an attached service is in its
//     sandbox's groups);
//   - ing-<sandbox id>, the own group of an image service's pod of an
//     environment that was ever public (model.PodTemplate.IngressGroup):
//     while the environment is public, TCP in on the routed ports from
//     ferun-ingress, else no rules; no members;
//   - ferun-ingress (model.IngressPeersGroup): the mesh addresses of the
//     ingress instances that are not removed, at the version
//     pod_network.ingress_group_version, which follows commit order; no
//     rules, and never deleted.
//
// Groups are put whole (env-groups every 10 s and on a hint, and the
// prepare hook before a pod that names them); the deltas, the batch push
// and the propagation metric of ADR 0034 are later work. What a node
// holds of the ingress's groups is recorded as it is put or listed
// (recordAdmission): the ingress sends to an instance only on the ports
// and at the group version its node holds.

// internet4Except are the IPv4 prefixes the egress table leaves out of
// 0.0.0.0/0: what is not the internet (feosd's internet4 set).
var internet4Except = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24",
	"192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4",
}

// internet6Except are the IPv6 prefixes the egress table leaves out of
// 2000::/3 (feosd's internet6 set). The contract's list has 5f00::/16
// too (SRv6 SIDs), which lies outside 2000::/3: feosd refuses an
// exception outside its block, and the block leaves it out anyway.
var internet6Except = []string{"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"}

// egressTable are the rules that let a pod reach the internet, of either
// family: never ::/0 or a bare 0.0.0.0/0, which would open the pods'
// network and the private ones wherever --deny-prefix is unset. DNS and
// the metadata service need no rule.
func egressTable() []*networkv1.Rule {
	return []*networkv1.Rule{
		{Direction: networkv1.Direction_DIRECTION_EGRESS, Protocol: networkv1.Protocol_PROTOCOL_ANY,
			Peer: &networkv1.Rule_Block{Block: &networkv1.AddressBlock{Cidr: "0.0.0.0/0", Except: slices.Clone(internet4Except)}}},
		{Direction: networkv1.Direction_DIRECTION_EGRESS, Protocol: networkv1.Protocol_PROTOCOL_ANY,
			Peer: &networkv1.Rule_Block{Block: &networkv1.AddressBlock{Cidr: "2000::/3", Except: slices.Clone(internet6Except)}}},
	}
}

// groupRule is a rule of any protocol to or from a group's members.
func groupRule(dir networkv1.Direction, group string) *networkv1.Rule {
	return &networkv1.Rule{Direction: dir, Protocol: networkv1.Protocol_PROTOCOL_ANY, Peer: &networkv1.Rule_Group{Group: group}}
}

// envRules are an env group's rules: out to its members, the implied rule
// (ADR 0034). Nothing comes in through it.
func envRules(id string) []*networkv1.Rule {
	return []*networkv1.Rule{groupRule(networkv1.Direction_DIRECTION_EGRESS, id)}
}

// members are the IPv6 ones of addresses, as a sorted set in canonical
// form (as feosd keeps them).
func members(addresses []string) []string {
	set := map[netip.Addr]bool{}
	for _, a := range addresses {
		if ip, err := netip.ParseAddr(a); err == nil && ip.Is6() && !ip.Is4In6() && ip.Zone() == "" {
			set[ip] = true
		}
	}
	ips := make([]netip.Addr, 0, len(set))
	for ip := range set {
		ips = append(ips, ip)
	}
	slices.SortFunc(ips, func(a, b netip.Addr) int { return a.Compare(b) })
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}

// sameContent reports whether two groups have the same rules and members.
func sameContent(a, b *networkv1.SecurityGroup) bool {
	return slices.EqualFunc(a.GetRules(), b.GetRules(), func(x, y *networkv1.Rule) bool { return proto.Equal(x, y) }) &&
		slices.Equal(members(a.GetMembers()), members(b.GetMembers()))
}

// hashVersion is a tenant's or an ing group's version: from its content,
// so that equal content has an equal version on every node.
func hashVersion(g *networkv1.SecurityGroup) uint64 {
	c := proto.Clone(g).(*networkv1.SecurityGroup)
	c.Version = 0
	c.Members = members(c.Members)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err != nil {
		b = []byte(g.GetId())
	}
	sum := sha256.Sum256(b)
	return binary.BigEndian.Uint64(sum[:8]) >> 1
}

// groupKind is the kind of a group of the controller's.
type groupKind int

const (
	groupEnv          groupKind = iota + 1 // env-<environment UUID>
	groupTenant                            // sg-<security_groups row UUID>
	groupIng                               // ing-<sandbox ID>
	groupIngressPeers                      // ferun-ingress
)

// groupRef is a group ID of the controller's as parseGroup read it: its
// kind, and its environment's ID (groupEnv), its row's (groupTenant) or
// its sandbox's (groupIng).
type groupRef struct {
	kind groupKind
	env  uuid.UUID
	sg   uuid.UUID
	sbx  podid.ID
}

// parseGroup reads a group ID of the controller's: env-<environment
// UUID>, sg-<row UUID> or ing-<sandbox ID> (short or a UUID), each in its
// canonical form, or model.IngressPeersGroup; ok false for any other
// group.
func parseGroup(id string) (groupRef, bool) {
	if id == model.IngressPeersGroup {
		return groupRef{kind: groupIngressPeers}, true
	}
	kind, rest, found := strings.Cut(id, "-")
	if !found {
		return groupRef{}, false
	}
	switch kind {
	case "env", "sg":
		u, err := uuid.Parse(rest)
		if err != nil || u.String() != rest {
			return groupRef{}, false
		}
		if kind == "sg" {
			return groupRef{kind: groupTenant, sg: u}, true
		}
		return groupRef{kind: groupEnv, env: u}, true
	case "ing":
		if !podid.Valid(rest) {
			return groupRef{}, false
		}
		return groupRef{kind: groupIng, sbx: podid.ID(rest)}, true
	}
	return groupRef{}, false
}

// legacySandboxPrefix is the prefix of the group and the name table an
// attachable sandbox had before ADR 0035 (sbx-<sandbox ID>): nothing makes
// them any more, and env-groups and env-names delete them from the nodes
// once no pod names them.
const legacySandboxPrefix = "sbx-"

// errNoGroup: the group's environment, row or sandbox does not exist.
var errNoGroup = errors.New("the group's environment, row or sandbox does not exist")

// buildGroup is a group as the database says it should be, with its
// version (an env group's gen, a tenant's or an ing group's hash,
// ferun-ingress's counter), the groups its rules name and, for an ing
// group, the ports its rules open.
func (c *Controller) buildGroup(ctx context.Context, id string, pass *groupPass) (*builtGroup, error) {
	ref, ok := parseGroup(id)
	if !ok {
		return nil, fmt.Errorf("%s is not a group of the environments", id)
	}
	switch ref.kind {
	case groupEnv:
		env, err := c.passEnvironment(ctx, pass, ref.env)
		if err != nil {
			return nil, err
		}
		if env == nil {
			return nil, errNoGroup
		}
		addrs, err := c.st.EnvCtlMembers(ctx, ref.env)
		if err != nil {
			return nil, err
		}
		g := &networkv1.SecurityGroup{Id: id, Version: uint64(max(env.Gen, 0)), Rules: envRules(id), Members: members(addrs)}
		return &builtGroup{g: g, kind: groupEnv, refs: []string{id}}, nil
	case groupTenant:
		return c.buildTenantGroup(ctx, id, ref.sg, pass)
	case groupIng:
		return c.buildIngGroup(ctx, id, ref.sbx, pass)
	}
	addrs, version, err := c.st.IngressGroup(ctx)
	if err != nil {
		return nil, err
	}
	g := &networkv1.SecurityGroup{Id: id, Version: uint64(max(version, 0)), Members: members(addrs)}
	return &builtGroup{g: g, kind: groupIngressPeers}, nil
}

// buildTenantGroup is a tenant's group sg-<row> (id): its rules as its
// environment's spec writes them (groupSpec, renderRules), the groups
// they name, and its members (store.EnvCtlGroupMembers).
func (c *Controller) buildTenantGroup(ctx context.Context, id string, row uuid.UUID, pass *groupPass) (*builtGroup, error) {
	envID, name, err := c.st.SecurityGroupRow(ctx, row)
	if errors.Is(err, store.ErrNoRow) {
		return nil, errNoGroup
	}
	if err != nil {
		return nil, err
	}
	eg, err := c.envGroups(ctx, pass, envID)
	if err != nil {
		return nil, err
	}
	if eg == nil {
		return nil, errNoGroup
	}
	rules, refs := renderRules(eg.group(name), eg.ids)
	addrs, err := c.st.EnvCtlGroupMembers(ctx, envID, id)
	if err != nil {
		return nil, err
	}
	g := &networkv1.SecurityGroup{Id: id, Rules: rules, Members: members(addrs)}
	g.Version = hashVersion(g)
	return &builtGroup{g: g, kind: groupTenant, refs: refs}, nil
}

// envGroupSet is what the tenant's groups of one environment are rendered
// from: the specs that may define them, the first that has a group
// winning, and the IDs of its groups' rows by name.
type envGroupSet struct {
	specs []*spec.Spec
	ids   map[string]uuid.UUID
}

// group is the group name as the first spec that has it writes it; else
// default's as normalisation writes it (nothing in, the internet out: an
// environment without a deployment has it), else a group with no rules.
func (s *envGroupSet) group(name string) spec.SecurityGroup {
	for _, sp := range s.specs {
		if g, ok := sp.SecurityGroups[name]; ok {
			return g
		}
	}
	if name == spec.DefaultSecurityGroup {
		return spec.SecurityGroup{}
	}
	return spec.SecurityGroup{Out: []spec.Rule{}}
}

// envGroups reads what the tenant's groups of an environment are rendered
// from, once per pass; nil when the environment is gone. The specs are
// its target's first (its latest deployment, unless that failed: then its
// deployed one), then the other of the two: a group the target no longer
// has keeps the rules of the deployment that had it while pods still name
// it (a roll, a sandbox the plan let keep it).
func (c *Controller) envGroups(ctx context.Context, pass *groupPass, envID uuid.UUID) (*envGroupSet, error) {
	if s, ok := pass.groups[envID]; ok {
		return s, nil
	}
	env, err := c.passEnvironment(ctx, pass, envID)
	if err != nil || env == nil {
		return nil, err
	}
	set := &envGroupSet{}
	numbers := []int{env.Deployment, env.Deployed}
	if env.Deployment > 0 {
		latest, err := c.st.ListDeployments(ctx, envID, 1)
		if err != nil {
			return nil, err
		}
		if len(latest) == 1 && latest[0].Number == env.Deployment && latest[0].State == model.DeploymentFailed {
			numbers = []int{env.Deployed, env.Deployment}
		}
	}
	for i, n := range numbers {
		if n <= 0 || (i == 1 && n == numbers[0]) {
			continue
		}
		sp, err := c.deploymentSpec(ctx, *env, n)
		if err != nil {
			return nil, err
		}
		set.specs = append(set.specs, sp)
	}
	if set.ids, err = c.st.SecurityGroupIDs(ctx, envID); err != nil {
		return nil, err
	}
	pass.groups[envID] = set
	return set, nil
}

// renderRules are a tenant's group's rules on the nodes (ADR 0034), and
// the groups they name: an in rule is an ingress rule from its peer, an
// out rule an egress rule to it; a group peer is its sg-<row> (a name
// without a row names nobody: the rule is left out), a block an address
// block with its exceptions, the internet the egress table's two blocks.
// A group's out left out (nil) is the internet. icmp is ICMP towards an
// IPv4 block and ICMPv6 otherwise (the members of a group are IPv6).
func renderRules(g spec.SecurityGroup, ids map[string]uuid.UUID) ([]*networkv1.Rule, []string) {
	out := g.Out
	if out == nil {
		out = []spec.Rule{{Protocol: spec.ProtocolAny, To: &spec.Peer{Internet: true}}}
	}
	var rules []*networkv1.Rule
	var refs []string
	add := func(dir networkv1.Direction, r spec.Rule, p *spec.Peer) {
		if p == nil {
			return
		}
		ports := make([]*networkv1.PortRange, 0, len(r.Ports))
		for _, pr := range r.Ports {
			last := uint32(pr.Last)
			if pr.Last == pr.First {
				last = 0
			}
			ports = append(ports, &networkv1.PortRange{First: uint32(pr.First), Last: last})
		}
		rule := func(v6 bool, peer any) *networkv1.Rule {
			nr := &networkv1.Rule{Direction: dir, Protocol: ruleProtocol(r.ProtocolOf(), v6)}
			if len(ports) > 0 {
				nr.Ports = ports
			}
			switch pv := peer.(type) {
			case string:
				nr.Peer = &networkv1.Rule_Group{Group: pv}
			case *networkv1.AddressBlock:
				nr.Peer = &networkv1.Rule_Block{Block: pv}
			}
			return nr
		}
		switch {
		case p.Internet:
			rules = append(rules,
				rule(false, &networkv1.AddressBlock{Cidr: "0.0.0.0/0", Except: slices.Clone(internet4Except)}),
				rule(true, &networkv1.AddressBlock{Cidr: "2000::/3", Except: slices.Clone(internet6Except)}))
		case p.Group != "":
			id, ok := ids[p.Group]
			if !ok {
				return
			}
			gid := model.SecurityGroupID(id)
			rules = append(rules, rule(true, gid))
			if !slices.Contains(refs, gid) {
				refs = append(refs, gid)
			}
		case p.CIDR != "":
			pf, err := netip.ParsePrefix(p.CIDR)
			if err != nil {
				return
			}
			rules = append(rules, rule(pf.Addr().Is6(), &networkv1.AddressBlock{Cidr: p.CIDR, Except: slices.Clone(p.Except)}))
		}
	}
	for _, r := range g.In {
		add(networkv1.Direction_DIRECTION_INGRESS, r, r.From)
	}
	for _, r := range out {
		add(networkv1.Direction_DIRECTION_EGRESS, r, r.To)
	}
	return rules, refs
}

// ruleProtocol is a spec's protocol on the nodes; icmp of the family.
func ruleProtocol(p string, v6 bool) networkv1.Protocol {
	switch p {
	case spec.ProtocolTCP:
		return networkv1.Protocol_PROTOCOL_TCP
	case spec.ProtocolUDP:
		return networkv1.Protocol_PROTOCOL_UDP
	case spec.ProtocolICMP:
		if v6 {
			return networkv1.Protocol_PROTOCOL_ICMPV6
		}
		return networkv1.Protocol_PROTOCOL_ICMP
	}
	return networkv1.Protocol_PROTOCOL_ANY
}

// buildIngGroup is the group ing-<sbx> (id): one rule, TCP in on the
// ports ingressPorts gives from the members of ferun-ingress, when there
// are any; else no rules. It has no members: no rule names it.
func (c *Controller) buildIngGroup(ctx context.Context, id string, sbx podid.ID, pass *groupPass) (*builtGroup, error) {
	in, ok, err := c.ingressInput(ctx, pass, sbx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errNoGroup
	}
	ports, err := c.ingressPorts(ctx, pass, in)
	if err != nil {
		return nil, err
	}
	b := &builtGroup{g: &networkv1.SecurityGroup{Id: id}, kind: groupIng, ports: ports}
	if len(ports) > 0 {
		r := &networkv1.Rule{Direction: networkv1.Direction_DIRECTION_INGRESS, Protocol: networkv1.Protocol_PROTOCOL_TCP,
			Peer: &networkv1.Rule_Group{Group: model.IngressPeersGroup}}
		for _, p := range ports {
			r.Ports = append(r.Ports, &networkv1.PortRange{First: uint32(p)})
		}
		b.g.Rules = []*networkv1.Rule{r}
		b.refs = []string{model.IngressPeersGroup}
	}
	b.g.Version = hashVersion(b.g)
	return b, nil
}

// ingressPorts are the ports the group ing-<id> of a sandbox opens to the
// ingress: none unless the sandbox is an instance of an image service
// (in) of an environment that is public and not being deleted; else the
// routed ports of its service (spec.RoutedPorts): of every route of the
// environment's deployed and latest deployment that targets it, the port's
// number in the instance's own deployment.
func (c *Controller) ingressPorts(ctx context.Context, pass *groupPass, in store.IngressInput) ([]int, error) {
	if in.EnvID == uuid.Nil || in.Kind != spec.KindService {
		return nil, nil
	}
	env, err := c.passEnvironment(ctx, pass, in.EnvID)
	if err != nil || env == nil || !env.Public || env.Deleting {
		return nil, err
	}
	var own *spec.Spec
	if in.Deployment > 0 {
		if own, err = c.deploymentSpec(ctx, *env, in.Deployment); err != nil {
			return nil, err
		}
	}
	numbers := []int{env.Deployed}
	if env.Deployment != env.Deployed {
		numbers = append(numbers, env.Deployment)
	}
	var routing []*spec.Spec
	for _, n := range numbers {
		if n <= 0 {
			continue
		}
		sp, err := c.deploymentSpec(ctx, *env, n)
		if err != nil {
			return nil, err
		}
		routing = append(routing, sp)
	}
	return spec.RoutedPorts(in.Object, own, routing...), nil
}

// builtGroup is a group as buildGroup made it: its kind, the groups its
// rules name, and for an ing group the ports its rules open (none: no
// rules).
type builtGroup struct {
	g     *networkv1.SecurityGroup
	kind  groupKind
	refs  []string
	ports []int
}

// groupPass is what one pass of env-groups, or one prepare hook, built
// and read: the groups by ID, the inputs of the ing groups by sandbox
// (readAhead for a node's rows, else one by one), and the environments
// they name.
type groupPass struct {
	built map[string]*builtGroup
	// inputs are the inputs read; a sandbox in read and not in inputs is
	// gone.
	inputs map[podid.ID]store.IngressInput
	read   map[podid.ID]bool
	// envs are the environments read; nil: gone.
	envs map[uuid.UUID]*store.Environment
	// groups are what the tenant's groups of each environment are
	// rendered from (envGroups).
	groups map[uuid.UUID]*envGroupSet
}

func newGroupPass() *groupPass {
	return &groupPass{built: map[string]*builtGroup{}, inputs: map[podid.ID]store.IngressInput{},
		read: map[podid.ID]bool{}, envs: map[uuid.UUID]*store.Environment{}, groups: map[uuid.UUID]*envGroupSet{}}
}

// readAhead reads the inputs of the ing groups that rows name and the
// pass has not built or read yet, in one query.
func (c *Controller) readAhead(ctx context.Context, pass *groupPass, rows []store.Sandbox) error {
	var ids []podid.ID
	seen := map[podid.ID]bool{}
	for _, sb := range rows {
		if sb.PodTemplate == nil {
			continue
		}
		for _, id := range sb.PodTemplate.SecurityGroups {
			if ref, ok := parseGroup(id); ok && ref.kind == groupIng && pass.built[id] == nil && !pass.read[ref.sbx] &&
				!seen[ref.sbx] {
				seen[ref.sbx] = true
				ids = append(ids, ref.sbx)
			}
		}
	}
	return c.readInputs(ctx, pass, ids)
}

// readInputs reads the inputs of the ing groups of ids into the pass.
func (c *Controller) readInputs(ctx context.Context, pass *groupPass, ids []podid.ID) error {
	if len(ids) == 0 {
		return nil
	}
	got, err := c.st.EnvCtlIngressInputs(ctx, ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		pass.read[id] = true
		if in, ok := got[id]; ok {
			pass.inputs[id] = in
		}
	}
	return nil
}

// ingressInput is what the group ing-<sbx> is built from; false when the
// sandbox is gone.
func (c *Controller) ingressInput(ctx context.Context, pass *groupPass, sbx podid.ID) (store.IngressInput, bool, error) {
	if !pass.read[sbx] {
		if err := c.readInputs(ctx, pass, []podid.ID{sbx}); err != nil {
			return store.IngressInput{}, false, err
		}
	}
	in, ok := pass.inputs[sbx]
	return in, ok, nil
}

// passEnvironment is an environment as the pass read it; nil when it is
// gone.
func (c *Controller) passEnvironment(ctx context.Context, pass *groupPass, id uuid.UUID) (*store.Environment, error) {
	if e, ok := pass.envs[id]; ok {
		return e, nil
	}
	e, err := c.st.GetEnvironment(ctx, id)
	switch {
	case errors.Is(err, store.ErrNoRow):
		pass.envs[id] = nil
		return nil, nil
	case err != nil:
		return nil, err
	}
	pass.envs[id] = &e
	return &e, nil
}

// recordAdmission records what node holds of a group it was put on or
// listed with (D9 of the ingress's design): an ing group's ports on its
// instance (env_instances.ingress_ports), ferun-ingress's version on the
// node (nodes.ingress_group_version). The other groups record nothing.
func (c *Controller) recordAdmission(ctx context.Context, node string, b *builtGroup) error {
	switch b.kind {
	case groupIng:
		ref, _ := parseGroup(b.g.GetId())
		_, err := c.st.SetIngressPorts(ctx, ref.sbx, b.ports)
		return err
	case groupIngressPeers:
		_, err := c.st.SetNodeIngressGroupVersion(ctx, node, int64(b.g.GetVersion()))
		return err
	}
	return nil
}

// groupsOf are the groups a row's pod names and the groups their rules
// name, each once, in order; pass keeps the groups built in a pass.
func (c *Controller) groupsOf(ctx context.Context, sb store.Sandbox, pass *groupPass) (map[string]*networkv1.SecurityGroup, []string, error) {
	built := map[string]*networkv1.SecurityGroup{}
	var order []string
	var visit func(id string) error
	visit = func(id string) error {
		if _, ok := built[id]; ok {
			return nil
		}
		b, ok := pass.built[id]
		if !ok {
			var err error
			if b, err = c.buildGroup(ctx, id, pass); err != nil {
				return fmt.Errorf("security group %s: %w", id, err)
			}
			pass.built[id] = b
		}
		built[id] = b.g
		order = append(order, id)
		for _, r := range b.refs {
			if err := visit(r); err != nil {
				return err
			}
		}
		return nil
	}
	if sb.PodTemplate != nil {
		for _, id := range sb.PodTemplate.SecurityGroups {
			if err := visit(id); err != nil {
				return nil, nil, err
			}
		}
	}
	return built, order, nil
}

// syncGroup puts want on node unless the node has it (have, nil when
// absent) with the same content and version, and returns the group as put.
// An env group whose content changed under the same version gets a new
// version first (BumpEnvironmentGen): Put ignores versions, so they are
// compared, never assumed to grow.
func (c *Controller) syncGroup(ctx context.Context, node string, want, have *networkv1.SecurityGroup) (*networkv1.SecurityGroup, error) {
	if have != nil && have.GetVersion() == want.GetVersion() && sameContent(want, have) {
		return want, nil
	}
	if have != nil && have.GetVersion() == want.GetVersion() {
		if ref, _ := parseGroup(want.Id); ref.kind == groupEnv {
			gen, err := c.st.BumpEnvironmentGen(ctx, ref.env)
			if err != nil {
				return nil, err
			}
			want = proto.Clone(want).(*networkv1.SecurityGroup)
			want.Version = uint64(max(gen, 0))
		}
	}
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, node)
	if err != nil {
		return nil, err
	}
	if _, err := networkv1.NewSecurityGroupServiceClient(cc).PutSecurityGroup(ctx,
		&networkv1.PutSecurityGroupRequest{Group: want}); err != nil {
		return nil, err
	}
	return want, nil
}

// nodeGroup reads a group from a node; nil when the node does not have it.
func (c *Controller) nodeGroup(ctx context.Context, node, id string) (*networkv1.SecurityGroup, error) {
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, node)
	if err != nil {
		return nil, err
	}
	g, err := networkv1.NewSecurityGroupServiceClient(cc).GetSecurityGroup(ctx, &networkv1.GetSecurityGroupRequest{Id: id})
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	return g, err
}

// putGroups puts a row's groups, and the groups their rules name, on node
// where it lacks them or has them otherwise, and records what the node
// then holds of the ingress's (recordAdmission; a failure is logged, the
// next env-groups pass records again): the prepare hook's first part.
func (c *Controller) putGroups(ctx context.Context, node string, sb store.Sandbox) error {
	pass := newGroupPass()
	built, order, err := c.groupsOf(ctx, sb, pass)
	if err != nil {
		return err
	}
	for _, id := range order {
		have, err := c.nodeGroup(ctx, node, id)
		if err != nil {
			return fmt.Errorf("read security group %s: %w", id, err)
		}
		put, err := c.syncGroup(ctx, node, built[id], have)
		if err != nil {
			return fmt.Errorf("put security group %s: %w", id, err)
		}
		b := pass.built[id]
		b.g = put
		if err := c.recordAdmission(ctx, node, b); err != nil && ctx.Err() == nil {
			c.rl.warn("admission/"+node, "cannot record what a node holds of a security group", "node", node, "group", id,
				"err", err)
		}
	}
	return nil
}

// groupsLoop is env-groups: per connected node, the groups that the
// templates of its rows name (and their rules name) are compared with
// what the node lists and re-put where they differ, and what the node
// holds of the ingress's groups is recorded (recordAdmission); the
// controller's groups of environments and tenant's groups' rows that no
// longer exist are deleted, the ing groups of sandboxes gone or on another
// node, and the sbx groups of before ADR 0035 (a pod that still names one
// keeps it: FAILED_PRECONDITION). ferun-ingress is never deleted.
func (c *Controller) groupsLoop(ctx context.Context) error {
	if c.d.Conns == nil {
		return nil
	}
	var errs []error
	pass := newGroupPass()
	for _, node := range c.d.Conns.Nodes() {
		if err := c.syncNodeGroups(ctx, node, pass); err != nil && ctx.Err() == nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node, err))
		}
	}
	return errors.Join(errs...)
}

// syncNodeGroups is env-groups for one node; pass has the groups built
// in the pass (with the versions they were put with).
func (c *Controller) syncNodeGroups(ctx context.Context, node string, pass *groupPass) error {
	rows, err := c.st.EnvCtlTemplatedOnNode(ctx, node)
	if err != nil {
		return err
	}
	if err := c.readAhead(ctx, pass, rows); err != nil {
		return err
	}
	want := map[string]*networkv1.SecurityGroup{}
	for _, sb := range rows {
		built, _, err := c.groupsOf(ctx, sb, pass)
		if err != nil {
			continue
		}
		for id, g := range built {
			want[id] = g
		}
	}
	lctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	cc, err := c.conn(lctx, node)
	if err != nil {
		cancel()
		return err
	}
	list, err := networkv1.NewSecurityGroupServiceClient(cc).ListSecurityGroups(lctx, &networkv1.ListSecurityGroupsRequest{})
	cancel()
	if err != nil {
		return err
	}
	have := map[string]*networkv1.SecurityGroup{}
	for _, g := range list.GetGroups() {
		have[g.GetId()] = g
	}
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(want)) {
		put, err := c.syncGroup(ctx, node, want[id], have[id])
		if err != nil {
			errs = append(errs, fmt.Errorf("put %s: %w", id, err))
			continue
		}
		// A bumped version is the pass's for the other nodes too.
		b := pass.built[id]
		b.g = put
		if err := c.recordAdmission(ctx, node, b); err != nil {
			errs = append(errs, fmt.Errorf("record %s: %w", id, err))
		}
	}
	// Groups of environments and rows that are gone, ing groups of
	// sandboxes that are gone or whose pod is on another node, and the
	// sbx groups of before ADR 0035.
	var envs, sgs []uuid.UUID
	var ings []podid.ID
	var legacy []string
	for id := range have {
		if _, wanted := want[id]; wanted {
			continue
		}
		if strings.HasPrefix(id, legacySandboxPrefix) {
			legacy = append(legacy, id)
			continue
		}
		ref, ok := parseGroup(id)
		switch {
		case !ok:
		case ref.kind == groupEnv:
			envs = append(envs, ref.env)
		case ref.kind == groupTenant:
			sgs = append(sgs, ref.sg)
		case ref.kind == groupIng:
			ings = append(ings, ref.sbx)
		}
	}
	envOK, sgOK, err := c.st.EnvCtlExisting(ctx, envs, sgs)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	ingNodes, err := c.st.EnvCtlSandboxNodes(ctx, ings)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, u := range envs {
		if !envOK[u] {
			_, err := c.deleteGroup(ctx, node, model.EnvGroupID(u))
			errs = append(errs, err)
		}
	}
	for _, u := range sgs {
		if !sgOK[u] {
			_, err := c.deleteGroup(ctx, node, model.SecurityGroupID(u))
			errs = append(errs, err)
		}
	}
	for _, sbx := range ings {
		if at, ok := ingNodes[sbx]; !ok || (at != "" && at != node) {
			_, err := c.deleteGroup(ctx, node, model.IngressGroupID(sbx))
			errs = append(errs, err)
		}
	}
	for _, id := range legacy {
		_, err := c.deleteGroup(ctx, node, id)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// deleteGroup deletes a group from a node and reports whether it is gone
// (NOT_FOUND too); FAILED_PRECONDITION (a pod on the node still names it)
// is not gone yet, and no error: a later pass tries again.
func (c *Controller) deleteGroup(ctx context.Context, node, id string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, node)
	if err != nil {
		return false, err
	}
	_, err = networkv1.NewSecurityGroupServiceClient(cc).DeleteSecurityGroup(ctx, &networkv1.DeleteSecurityGroupRequest{Id: id})
	switch status.Code(err) {
	case codes.OK, codes.NotFound:
		return true, nil
	case codes.FailedPrecondition:
		return false, nil
	}
	return false, err
}
