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

// The security groups of environments (§4.5), and the ingress's (ADR
// 0021):
//
//   - env-<env id> for the pods of an environment (its instances and the
//     sandboxes its attached services run in): the internet (the egress
//     table), and its members reach each other;
//   - sbx-<sandbox id> for an attachable sandbox: the internet, and the
//     env groups it is attached to (no ingress: image pods never reach a
//     sandbox);
//   - ing-<sandbox id>, the own group of an image service's pod of an
//     environment that was ever public (model.PodTemplate.IngressGroup):
//     while the environment is public, TCP in on the routed ports from
//     ferun-ingress, else no rules; no members;
//   - ferun-ingress (model.IngressPeersGroup): the mesh addresses of the
//     ingress instances that are not removed, at the version
//     pod_network.ingress_group_version, which follows commit order; no
//     rules, and never deleted.
//
// What a node holds of the ingress's groups is recorded as it is put or
// listed (recordAdmission): the ingress sends to an instance only on the
// ports and at the group version its node holds.

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

// envRules are an env group's rules: the egress table, and any traffic
// between its members.
func envRules(id string) []*networkv1.Rule {
	return append(egressTable(),
		groupRule(networkv1.Direction_DIRECTION_INGRESS, id),
		groupRule(networkv1.Direction_DIRECTION_EGRESS, id))
}

// sbxRules are an sbx group's rules: the egress table, and egress to each
// env group the sandbox is attached to.
func sbxRules(envGroups []string) []*networkv1.Rule {
	rules := egressTable()
	for _, g := range envGroups {
		rules = append(rules, groupRule(networkv1.Direction_DIRECTION_EGRESS, g))
	}
	return rules
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

// hashVersion is an sbx or ing group's version: from its content, so
// that equal content has an equal version on every node.
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
	groupSbx                               // sbx-<sandbox ID>
	groupIng                               // ing-<sandbox ID>
	groupIngressPeers                      // ferun-ingress
)

// groupRef is a group ID of the controller's as parseGroup read it: its
// kind, and its environment's ID (groupEnv) or its sandbox's (groupSbx,
// groupIng).
type groupRef struct {
	kind groupKind
	env  uuid.UUID
	sbx  podid.ID
}

// parseGroup reads a group ID of the controller's: env-<environment
// UUID>, sbx-<sandbox ID> or ing-<sandbox ID> (short or a UUID), each in
// its canonical form, or model.IngressPeersGroup; ok false for any other
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
	case "env":
		u, err := uuid.Parse(rest)
		if err != nil || u.String() != rest {
			return groupRef{}, false
		}
		return groupRef{kind: groupEnv, env: u}, true
	case "sbx", "ing":
		if !podid.Valid(rest) {
			return groupRef{}, false
		}
		k := groupSbx
		if kind == "ing" {
			k = groupIng
		}
		return groupRef{kind: k, sbx: podid.ID(rest)}, true
	}
	return groupRef{}, false
}

// errNoGroup: the group's environment or sandbox does not exist.
var errNoGroup = errors.New("the group's environment or sandbox does not exist")

// buildGroup is a group as the database says it should be, with its
// version (an env group's gen, an sbx or ing group's hash, ferun-ingress's
// counter), the groups its rules name and, for an ing group, the ports
// its rules open.
func (c *Controller) buildGroup(ctx context.Context, id string, pass *groupPass) (*builtGroup, error) {
	ref, ok := parseGroup(id)
	if !ok {
		return nil, fmt.Errorf("%s is not a group of the environments", id)
	}
	switch ref.kind {
	case groupEnv:
		env, err := c.st.GetEnvironment(ctx, ref.env)
		if errors.Is(err, store.ErrNoRow) {
			return nil, errNoGroup
		}
		if err != nil {
			return nil, err
		}
		addrs, err := c.st.EnvCtlMembers(ctx, ref.env)
		if err != nil {
			return nil, err
		}
		g := &networkv1.SecurityGroup{Id: id, Version: uint64(max(env.Gen, 0)), Rules: envRules(id), Members: members(addrs)}
		return &builtGroup{g: g, kind: groupEnv, refs: []string{id}}, nil
	case groupIng:
		return c.buildIngGroup(ctx, id, ref.sbx, pass)
	case groupIngressPeers:
		addrs, version, err := c.st.IngressGroup(ctx)
		if err != nil {
			return nil, err
		}
		g := &networkv1.SecurityGroup{Id: id, Version: uint64(max(version, 0)), Members: members(addrs)}
		return &builtGroup{g: g, kind: groupIngressPeers}, nil
	}
	sb, err := c.st.GetSandbox(ctx, ref.sbx)
	if errors.Is(err, model.ErrNotFound) {
		return nil, errNoGroup
	}
	if err != nil {
		return nil, err
	}
	envs, err := c.st.EnvCtlWithAttached(ctx, ref.sbx)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, e := range envs {
		refs = append(refs, model.EnvGroupID(e))
	}
	g := &networkv1.SecurityGroup{Id: id, Rules: sbxRules(refs), Members: members([]string{sb.Address})}
	g.Version = hashVersion(g)
	return &builtGroup{g: g, kind: groupSbx, refs: refs}, nil
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
}

func newGroupPass() *groupPass {
	return &groupPass{built: map[string]*builtGroup{}, inputs: map[podid.ID]store.IngressInput{},
		read: map[podid.ID]bool{}, envs: map[uuid.UUID]*store.Environment{}}
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
// controller's groups of environments and sandboxes that no longer exist
// are deleted, and the ing groups of sandboxes gone or on another node (a
// pod that still names one keeps it: FAILED_PRECONDITION). ferun-ingress
// is never deleted.
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
	// Groups of environments and sandboxes that are gone, and ing groups
	// of sandboxes that are gone or whose pod is on another node.
	var envs []uuid.UUID
	var sbxs, ings []podid.ID
	for id := range have {
		if _, wanted := want[id]; wanted {
			continue
		}
		ref, ok := parseGroup(id)
		switch {
		case !ok:
		case ref.kind == groupEnv:
			envs = append(envs, ref.env)
		case ref.kind == groupSbx:
			sbxs = append(sbxs, ref.sbx)
		case ref.kind == groupIng:
			ings = append(ings, ref.sbx)
		}
	}
	envOK, sbxOK, err := c.st.EnvCtlExisting(ctx, envs, sbxs)
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
	for _, sbx := range sbxs {
		if !sbxOK[sbx] {
			_, err := c.deleteGroup(ctx, node, model.SandboxGroupID(sbx))
			errs = append(errs, err)
		}
	}
	for _, sbx := range ings {
		if at, ok := ingNodes[sbx]; !ok || (at != "" && at != node) {
			_, err := c.deleteGroup(ctx, node, model.IngressGroupID(sbx))
			errs = append(errs, err)
		}
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
