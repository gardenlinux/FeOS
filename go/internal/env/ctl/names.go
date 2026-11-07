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
	"github.com/gardenlinux/feos/go/internal/store"
)

// The names of environments (§4.6): feosd's resolver answers a pod the
// names of the name tables its spec names (NameService, ADR 0010), as the
// controller puts them on the pod's node, the way it puts security
// groups. env-<env id>, which every pod of the environment names (its
// service and job pods, its sandboxes and its tenants' jobs' pods, ADR
// 0035), holds its addons and image services with the addresses of their
// live replicas, and its attached services with their sandbox's address;
// feosd answers a pod a name whose addresses include its own with
// loopback, so that in its own sandbox an attached service is reached
// at 127.0.0.1 and ::1, as the table sbx-<id> gave. A
// table's version is a hash of its content, so that equal tables have
// equal versions on every node and a delta between two versions is the
// same wherever it applies. A node whose feosd predates the NameService
// (UNIMPLEMENTED) gets no tables: its pods start without names there,
// and names_unserved opens for the objects whose pods run there.

// nameEntries are the names of the environment's addons and image
// services with their addresses: those of the replicas live counts ready,
// or, when none is, those of the best that are there (another replica
// whose state is known, else one whose node is out of touch, its address
// maybe stale), so that a name never goes during a roll or a restart;
// never a stopping replica's. Addresses are IPv6, as the pods reach each
// other.
func nameEntries(v *view) map[string][]string {
	type best struct {
		rank  int
		addrs []string
	}
	found := map[string]*best{}
	for _, r := range v.rows {
		inst := r.Instance
		if inst.Phase == model.PhaseStopping {
			continue
		}
		ip, err := netip.ParseAddr(r.Sandbox.Address)
		if err != nil || !ip.Is6() || ip.Is4In6() || ip.Zone() != "" {
			continue
		}
		idles := false
		switch inst.Kind {
		case spec.KindAddon:
			if _, ok := v.target.Addons[inst.Object]; !ok {
				continue
			}
		case spec.KindService:
			svc, ok := v.target.Services[inst.Object]
			if !ok || runtimeOf(svc) != spec.RuntimeImage {
				continue
			}
			idles = idleOf(svc) > 0
		default:
			continue
		}
		// 3: live, 2: another replica, 1: one whose row is unknown.
		rank := 2
		switch {
		case live(r, idles):
			rank = 3
		case r.Sandbox.ClientState == model.StateUnknown:
			rank = 1
		}
		b := found[inst.Object]
		switch {
		case b == nil || rank > b.rank:
			found[inst.Object] = &best{rank: rank, addrs: []string{ip.String()}}
		case rank == b.rank:
			b.addrs = append(b.addrs, ip.String())
		}
	}
	out := make(map[string][]string, len(found))
	for name, b := range found {
		out[name] = addressSet(b.addrs)
	}
	return out
}

// addressSet is addresses as feosd keeps them: a sorted set (IPv4 before
// IPv6) in canonical form; what does not parse is left out.
func addressSet(addresses []string) []string {
	set := map[netip.Addr]bool{}
	for _, a := range addresses {
		if ip, err := netip.ParseAddr(a); err == nil && ip.Zone() == "" {
			set[ip.Unmap()] = true
		}
	}
	ips := slices.SortedFunc(maps.Keys(set), func(a, b netip.Addr) int { return a.Compare(b) })
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}

// entriesOf are names and their addresses as a table's entries, sorted
// by name, in lower case.
func entriesOf(names map[string][]string) []*networkv1.NameEntry {
	out := make([]*networkv1.NameEntry, 0, len(names))
	for _, name := range slices.Sorted(maps.Keys(names)) {
		out = append(out, &networkv1.NameEntry{Name: strings.ToLower(name), Addresses: addressSet(names[name])})
	}
	return out
}

// tableNames are a table's entries as a map, normalized as entriesOf
// writes them.
func tableNames(t *networkv1.NameTable) map[string][]string {
	out := map[string][]string{}
	for _, e := range t.GetEntries() {
		out[strings.ToLower(e.GetName())] = addressSet(e.GetAddresses())
	}
	return out
}

// sameTable reports whether two tables hold the same names, addresses and
// includes.
func sameTable(a, b *networkv1.NameTable) bool {
	return slices.Equal(a.GetIncludes(), b.GetIncludes()) &&
		maps.EqualFunc(tableNames(a), tableNames(b), slices.Equal[[]string])
}

// tableVersion is a table's version: a hash of its ID, entries and
// includes, never 0.
func tableVersion(t *networkv1.NameTable) uint64 {
	c := &networkv1.NameTable{Id: t.GetId(), Entries: entriesOf(tableNames(t)), Includes: t.GetIncludes()}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err != nil {
		b = []byte(t.GetId())
	}
	sum := sha256.Sum256(b)
	return binary.BigEndian.Uint64(sum[:8])>>1 | 1
}

// newTable is a table with its version.
func newTable(id string, names map[string][]string, includes []string) *networkv1.NameTable {
	t := &networkv1.NameTable{Id: id, Entries: entriesOf(names), Includes: includes}
	t.Version = tableVersion(t)
	return t
}

// tableDelta is what turns have into want: the names to put and those to
// remove.
func tableDelta(have, want *networkv1.NameTable) ([]*networkv1.NameEntry, []string) {
	h, w := tableNames(have), tableNames(want)
	var put []*networkv1.NameEntry
	for _, name := range slices.Sorted(maps.Keys(w)) {
		if a, ok := h[name]; !ok || !slices.Equal(a, w[name]) {
			put = append(put, &networkv1.NameEntry{Name: name, Addresses: w[name]})
		}
	}
	var remove []string
	for _, name := range slices.Sorted(maps.Keys(h)) {
		if _, ok := w[name]; !ok {
			remove = append(remove, name)
		}
	}
	return put, remove
}

// parseTable reads a name table ID of the controller's: an env group's
// ID (EnvTableID); ok false for any other.
func parseTable(id string) (groupRef, bool) {
	ref, ok := parseGroup(id)
	return ref, ok && ref.kind == groupEnv
}

// buildTable is a name table as the database says it should be, with the
// tables it includes (none). An env table of an environment that has no
// deployment or is being deleted holds no names.
func (c *Controller) buildTable(ctx context.Context, id string) (*networkv1.NameTable, []string, error) {
	ref, ok := parseTable(id)
	if !ok {
		return nil, nil, fmt.Errorf("%s is not a name table of the environments", id)
	}
	if _, err := c.st.GetEnvironment(ctx, ref.env); errors.Is(err, store.ErrNoRow) {
		return nil, nil, errNoGroup
	} else if err != nil {
		return nil, nil, err
	}
	v, err := c.load(ctx, ref.env)
	if err != nil {
		return nil, nil, err
	}
	names := map[string][]string{}
	if v != nil {
		names = nameEntries(v)
		attached, err := c.attachedEntries(ctx, v)
		if err != nil {
			return nil, nil, err
		}
		maps.Copy(names, attached)
	}
	return newTable(id, names, nil), nil, nil
}

// attachedEntries are the names of the attached services of T with the
// IPv6 address of the sandbox each runs in, where it may use it
// (attachedUsable) and the sandbox has one (ADR 0035).
func (c *Controller) attachedEntries(ctx context.Context, v *view) (map[string][]string, error) {
	out := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(v.attached)) {
		if svc, ok := v.target.Services[name]; !ok || runtimeOf(svc) != spec.RuntimeAttached {
			continue
		}
		a := v.attached[name]
		sb, err := c.st.GetSandbox(ctx, a.SandboxID)
		if errors.Is(err, model.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !attachedUsable(v.env, a, sb) {
			continue
		}
		if ip, err := netip.ParseAddr(sb.Address); err == nil && ip.Is6() && !ip.Is4In6() && ip.Zone() == "" {
			out[name] = []string{ip.String()}
		}
	}
	return out, nil
}

// builtTable is a table as buildTable made it, with the tables it
// includes.
type builtTable struct {
	t        *networkv1.NameTable
	includes []string
}

// tablesOf are the tables a row's pod names and those they include, each
// once, in order; cache keeps the tables built in a pass. A legacy
// sbx-<id> table of a sandbox not moved yet (ADR 0035) is left out, as in
// groupsOf.
func (c *Controller) tablesOf(ctx context.Context, sb store.Sandbox, cache map[string]*builtTable) (map[string]*networkv1.NameTable, []string, error) {
	built := map[string]*networkv1.NameTable{}
	var order []string
	visit := func(id string) ([]string, error) {
		if _, ok := built[id]; ok {
			return nil, nil
		}
		b, ok := cache[id]
		if !ok {
			t, includes, err := c.buildTable(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("name table %s: %w", id, err)
			}
			b = &builtTable{t: t, includes: includes}
			cache[id] = b
		}
		built[id] = b.t
		order = append(order, id)
		return b.includes, nil
	}
	if sb.PodTemplate == nil {
		return built, order, nil
	}
	for _, id := range sb.PodTemplate.NameTables {
		if strings.HasPrefix(id, legacySandboxPrefix) {
			continue
		}
		includes, err := visit(id)
		if err != nil {
			return nil, nil, err
		}
		// Includes of includes are not followed: feosd does not either.
		for _, inc := range includes {
			if _, err := visit(inc); err != nil {
				return nil, nil, err
			}
		}
	}
	return built, order, nil
}

// syncTable puts want on node unless the node has it (have, nil when
// absent) as it is: as a delta from the version the node has when only
// the names differ, else, or when the node refuses the delta (it moved
// meanwhile), whole.
func (c *Controller) syncTable(ctx context.Context, node string, want, have *networkv1.NameTable) error {
	if have != nil && have.GetVersion() == want.GetVersion() && sameTable(want, have) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, node)
	if err != nil {
		return err
	}
	client := networkv1.NewNameServiceClient(cc)
	if have != nil && have.GetVersion() != want.GetVersion() && slices.Equal(have.GetIncludes(), want.GetIncludes()) {
		put, remove := tableDelta(have, want)
		_, err := client.UpdateNameTable(ctx, &networkv1.UpdateNameTableRequest{Id: want.GetId(),
			FromVersion: have.GetVersion(), Version: want.GetVersion(), PutEntries: put, RemoveNames: remove})
		if status.Code(err) != codes.FailedPrecondition {
			return err
		}
	}
	_, err = client.PutNameTable(ctx, &networkv1.PutNameTableRequest{Table: want})
	return err
}

// nodeTable reads a table from a node; nil when the node does not have
// it.
func (c *Controller) nodeTable(ctx context.Context, node, id string) (*networkv1.NameTable, error) {
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, node)
	if err != nil {
		return nil, err
	}
	t, err := networkv1.NewNameServiceClient(cc).GetNameTable(ctx, &networkv1.GetNameTableRequest{Id: id})
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	return t, err
}

// putNames puts a row's name tables, and those they include, on node
// where it lacks them or has them otherwise: the prepare hook's part
// after the groups. A node that serves no names is no reason to keep the
// pod off it: its pod starts without names (names_unserved).
func (c *Controller) putNames(ctx context.Context, node string, sb store.Sandbox) error {
	if sb.PodTemplate == nil || len(sb.PodTemplate.NameTables) == 0 {
		return nil
	}
	built, order, err := c.tablesOf(ctx, sb, map[string]*builtTable{})
	if err != nil {
		return err
	}
	for _, id := range order {
		have, err := c.nodeTable(ctx, node, id)
		if status.Code(err) == codes.Unimplemented {
			c.namesUnserved(node, true)
			return nil
		}
		if err != nil {
			return fmt.Errorf("read name table %s: %w", id, err)
		}
		c.namesUnserved(node, false)
		if err := c.syncTable(ctx, node, built[id], have); err != nil {
			return fmt.Errorf("put name table %s: %w", id, err)
		}
	}
	return nil
}

// namesLoop is env-names: per connected node, the tables that the
// templates of its rows name (and the tables those include) are compared
// with what the node lists and put where they differ, as deltas where
// they can be; the tables of environments that no longer exist, and the
// sbx tables of before ADR 0035, are deleted (a pod that still names one
// keeps it: FAILED_PRECONDITION). A node that answers UNIMPLEMENTED is remembered
// as serving no names, for names_unserved.
func (c *Controller) namesLoop(ctx context.Context) error {
	if c.d.Conns == nil {
		return nil
	}
	var errs []error
	cache := map[string]*builtTable{}
	for _, node := range c.d.Conns.Nodes() {
		if err := c.syncNodeNames(ctx, node, cache); err != nil && ctx.Err() == nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node, err))
		}
	}
	return errors.Join(errs...)
}

// syncNodeNames is env-names for one node.
func (c *Controller) syncNodeNames(ctx context.Context, node string, cache map[string]*builtTable) error {
	rows, err := c.st.EnvCtlTemplatedOnNode(ctx, node)
	if err != nil {
		return err
	}
	want := map[string]*networkv1.NameTable{}
	for _, sb := range rows {
		built, _, err := c.tablesOf(ctx, sb, cache)
		if err != nil {
			continue
		}
		maps.Copy(want, built)
	}
	lctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	cc, err := c.conn(lctx, node)
	if err != nil {
		cancel()
		return err
	}
	list, err := networkv1.NewNameServiceClient(cc).ListNameTables(lctx, &networkv1.ListNameTablesRequest{})
	cancel()
	if status.Code(err) == codes.Unimplemented {
		if !c.namesUnserved(node, true) {
			c.log.Warn("a node's feosd serves no names (no NameService): its pods do not resolve their environments' names",
				"node", node)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if c.namesUnserved(node, false) {
		c.log.Info("a node's feosd serves names now", "node", node)
	}
	have := map[string]*networkv1.NameTable{}
	for _, t := range list.GetTables() {
		have[t.GetId()] = t
	}
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(want)) {
		if err := c.syncTable(ctx, node, want[id], have[id]); err != nil {
			errs = append(errs, fmt.Errorf("put %s: %w", id, err))
		}
	}
	// Tables of environments that are gone, and the sbx tables of before
	// ADR 0035.
	var envs []uuid.UUID
	var legacy []string
	for id := range have {
		if _, wanted := want[id]; wanted {
			continue
		}
		if strings.HasPrefix(id, legacySandboxPrefix) {
			legacy = append(legacy, id)
		} else if ref, ok := parseTable(id); ok {
			envs = append(envs, ref.env)
		}
	}
	envOK, _, err := c.st.EnvCtlExisting(ctx, envs, nil)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, u := range envs {
		if !envOK[u] {
			_, err := c.deleteTable(ctx, node, model.EnvTableID(u))
			errs = append(errs, err)
		}
	}
	for _, id := range legacy {
		_, err := c.deleteTable(ctx, node, id)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// deleteTable deletes a table from a node and reports whether it is gone
// (NOT_FOUND, and UNIMPLEMENTED of a node that has no tables, too);
// FAILED_PRECONDITION (a pod on the node still names it) is not gone yet,
// and no error: a later pass tries again.
func (c *Controller) deleteTable(ctx context.Context, node, id string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, node)
	if err != nil {
		return false, err
	}
	_, err = networkv1.NewNameServiceClient(cc).DeleteNameTable(ctx, &networkv1.DeleteNameTableRequest{Id: id})
	switch status.Code(err) {
	case codes.OK, codes.NotFound, codes.Unimplemented:
		return true, nil
	case codes.FailedPrecondition:
		return false, nil
	}
	return false, err
}

// namesUnserved records whether node's feosd serves no names, and
// reports what was recorded before.
func (c *Controller) namesUnserved(node string, unserved bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.unserved[node]
	if unserved {
		if c.unserved == nil {
			c.unserved = map[string]bool{}
		}
		c.unserved[node] = true
	} else {
		delete(c.unserved, node)
	}
	return was
}

// unservedNodes are the nodes whose feosd serves no names, as the last
// look at each found.
func (c *Controller) unservedNodes() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.unserved)
}

// stepNames opens names_unserved for the objects of T with a pod on a node
// whose feosd serves no names (an image service or job replica, an
// attached service's sandbox), and closes it for objects that have none.
func (c *Controller) stepNames(ctx context.Context, s *stepState) {
	v := s.v
	unserved := c.unservedNodes()
	affected := map[string]string{}
	deployment := map[string]int{}
	if len(unserved) > 0 {
		for _, r := range v.rows {
			inst := r.Instance
			if inst.Phase == model.PhaseStopping || !unserved[r.Sandbox.Node] || r.Sandbox.PodTemplate == nil ||
				len(r.Sandbox.PodTemplate.NameTables) == 0 {
				continue
			}
			obj := objectName(inst.Kind, inst.Object)
			if _, ok := affected[obj]; !ok {
				affected[obj], deployment[obj] = r.Sandbox.Node, inst.Deployment
			}
		}
		for _, name := range slices.Sorted(maps.Keys(v.attached)) {
			a := v.attached[name]
			sb, ok := c.usableSandbox(ctx, v.env, a)
			if !ok || !unserved[sb.Node] {
				continue
			}
			obj := objectName(spec.KindService, name)
			if _, ok := affected[obj]; !ok {
				affected[obj], deployment[obj] = sb.Node, v.targetNum
			}
		}
	}
	for _, obj := range slices.Sorted(maps.Keys(affected)) {
		if v.isOpen(obj, model.ConditionNamesUnserved) {
			continue
		}
		c.incident(ctx, v.env.ID, obj, deployment[obj], model.ConditionNamesUnserved,
			fmt.Sprintf("node %s does not serve names (its feosd predates the NameService): %s does not resolve "+
				"the environment's names there", affected[obj], obj), evidence{})
	}
	for _, i := range v.open {
		if _, ok := affected[i.Object]; i.Condition == model.ConditionNamesUnserved && !ok {
			c.closeIncidents(ctx, v, i.Object, model.ConditionNamesUnserved)
		}
	}
}
