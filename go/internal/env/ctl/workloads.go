// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"

	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	"github.com/gardenlinux/feos/go/internal/env/addons"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The workloads' memberships (ADR 0034 revised, ADR 0035 revised): every
// pod of an environment is a member of env-<env id> and of its
// workload's group, model.WorkloadGroupID for an object's pods,
// model.SandboxWorkloadGroupID for a sandbox's (and the attached services
// that run in it), model.JobWorkloadGroupID for a tenant's job's. The
// group's rules are derived, never written:
//
//   - in: for every edge caller → it on port n that its environment's
//     specs declare (spec.Derive; the ingress's go through ing-<id>), TCP
//     on n from the caller's group; for every sandbox and tenant's job of
//     the environment whose reaches names it, TCP on the port named from
//     that pod's group; for every one whose reaches is "all", anything
//     from that pod's group, on every workload of the environment;
//   - out: its egress. The internet is the egress table's blocks, none is
//     nothing, restricted its blocks (any protocol); its domains are the
//     pod's (PodNetwork.egress_domains), which feosd's resolver admits.
//
// The specs are the environment's target (its latest deployment, unless
// that failed: then its deployed one) and its deployed deployment: while
// a deployment rolls, the calls of both run, and an edge either declares
// is admitted. An object's egress is the target's (the deployed one's for
// an object the target lacks); an addon's is none.

// isWorkloadGroup reports whether rest (an ID after "wl-") is a
// workload's: 32 lowercase hex digits, s-<sandbox ID> or j-<job ID>.
func isWorkloadGroup(rest string) bool {
	if id, ok := strings.CutPrefix(rest, "s-"); ok {
		return podid.Valid(id)
	}
	if id, ok := strings.CutPrefix(rest, "j-"); ok {
		return podid.Valid(id)
	}
	if len(rest) != 32 {
		return false
	}
	for _, c := range rest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// peerPorts are what one peer may reach of a workload: anything, or TCP
// on ports.
type peerPorts struct {
	any   bool
	ports map[int]bool
}

// envReach are the workloads' rules of one environment, as one pass of
// env-groups (or one prepare hook) derived them.
type envReach struct {
	// in are the peers of each workload by its group, and what each may
	// reach of it.
	in map[string]map[string]*peerPorts
	// egress is each workload's egress; absent: the internet.
	egress map[string]*spec.Egress
	// all are the groups of the sandboxes and jobs that reach everything.
	all []string
}

func (r *envReach) admit(target, peer string, port int) {
	if target == peer || port <= 0 {
		return
	}
	pp := r.peer(target, peer)
	if pp.ports == nil {
		pp.ports = map[int]bool{}
	}
	pp.ports[port] = true
}

func (r *envReach) peer(target, peer string) *peerPorts {
	m := r.in[target]
	if m == nil {
		m = map[string]*peerPorts{}
		r.in[target] = m
	}
	pp := m[peer]
	if pp == nil {
		pp = &peerPorts{}
		m[peer] = pp
	}
	return pp
}

// addonPort is the port addon name serves on, as its blueprint renders it
// (0: unknown).
func (c *Controller) addonPort(name string, a spec.Addon) int {
	bp, ok := c.d.Blueprint(a.Type)
	if !ok {
		return 0
	}
	inst, err := bp.Render(specAddon(bp, name, a), addons.Context{Name: name})
	if err != nil {
		return 0
	}
	return inst.Port
}

// workloadOf is the group of object kind/name of spec sp: its sandbox's
// for an attached service; "" for an object sp lacks.
func workloadOf(envID uuid.UUID, sp *spec.Spec, kind, name string) string {
	switch kind {
	case spec.KindService:
		svc, ok := sp.Services[name]
		if !ok {
			return ""
		}
		if svc.Runtime == spec.RuntimeAttached {
			id, err := podid.Parse(svc.Sandbox)
			if err != nil {
				return ""
			}
			return model.SandboxWorkloadGroupID(id.String())
		}
	case spec.KindJob:
		if _, ok := sp.Jobs[name]; !ok {
			return ""
		}
	case spec.KindAddon:
		if _, ok := sp.Addons[name]; !ok {
			return ""
		}
	default:
		return ""
	}
	return model.WorkloadGroupID(envID, kind, name)
}

// reacherGroup is the group of a sandbox's or a tenant's job's pod.
func reacherGroup(r store.Reacher) string {
	if r.TenantJob {
		return model.JobWorkloadGroupID(r.ID.String())
	}
	return model.SandboxWorkloadGroupID(r.ID.String())
}

// envReachOf derives the workloads' rules of environment envID, once per
// pass; nil when the environment is gone.
func (c *Controller) envReachOf(ctx context.Context, pass *groupPass, envID uuid.UUID) (*envReach, error) {
	if r, ok := pass.reach[envID]; ok {
		return r, nil
	}
	env, err := c.passEnvironment(ctx, pass, envID)
	if err != nil || env == nil {
		return nil, err
	}
	target := env.Deployment
	if env.Deployment > 0 {
		latest, err := c.st.ListDeployments(ctx, envID, 1)
		if err != nil {
			return nil, err
		}
		if len(latest) == 1 && latest[0].Number == env.Deployment && latest[0].State == model.DeploymentFailed {
			target = env.Deployed
		}
	}
	var specs []*spec.Spec
	for i, n := range []int{target, env.Deployed} {
		if n <= 0 || (i == 1 && n == target) {
			continue
		}
		sp, err := c.deploymentSpec(ctx, *env, n)
		if err != nil {
			return nil, err
		}
		specs = append(specs, sp)
	}
	reachers, err := c.st.EnvCtlReachers(ctx, envID)
	if err != nil {
		return nil, err
	}
	r := deriveEnvReach(envID, specs, reachers, c.addonPort)
	pass.reach[envID] = r
	return r, nil
}

// deriveEnvReach derives the workloads' rules of environment envID from
// its specs (the target first) and its sandboxes' and tenants' jobs' pods.
func deriveEnvReach(envID uuid.UUID, specs []*spec.Spec, reachers []store.Reacher, addonPort spec.AddonPort) *envReach {
	r := &envReach{in: map[string]map[string]*peerPorts{}, egress: map[string]*spec.Egress{}}
	none := &spec.Egress{Mode: spec.EgressNone}
	var known []string
	setEgress := func(wl string, e *spec.Egress) {
		if _, ok := r.egress[wl]; ok || wl == "" {
			return
		}
		r.egress[wl] = e
		known = append(known, wl)
	}
	for _, sp := range specs {
		for _, name := range slices.Sorted(maps.Keys(sp.Services)) {
			svc := sp.Services[name]
			if svc.Runtime != spec.RuntimeAttached {
				setEgress(workloadOf(envID, sp, spec.KindService, name), svc.Egress)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(sp.Jobs)) {
			setEgress(workloadOf(envID, sp, spec.KindJob, name), sp.Jobs[name].Egress)
		}
		for _, name := range slices.Sorted(maps.Keys(sp.Addons)) {
			setEgress(workloadOf(envID, sp, spec.KindAddon, name), none)
		}
		reach, _ := spec.Derive(*sp, addonPort)
		for _, e := range reach.Edges {
			if e.From == spec.EdgeIngress {
				continue
			}
			fk, fn, _ := strings.Cut(e.From, "/")
			tk, tn, _ := strings.Cut(e.To, "/")
			from, to := workloadOf(envID, sp, fk, fn), workloadOf(envID, sp, tk, tn)
			if from != "" && to != "" {
				r.admit(to, from, e.Number)
			}
		}
	}
	var first *spec.Spec
	if len(specs) > 0 {
		first = specs[0]
	} else {
		first = &spec.Spec{}
	}
	for _, rc := range reachers {
		wl := reacherGroup(rc)
		setEgress(wl, rc.Egress)
		if rc.Reaches.All {
			r.all = append(r.all, wl)
			continue
		}
		for _, u := range rc.Reaches.Entries {
			switch {
			case u.Addon != "":
				a, ok := first.Addons[u.Addon]
				if !ok || addonPort == nil {
					continue
				}
				r.admit(workloadOf(envID, first, spec.KindAddon, u.Addon), wl, addonPort(u.Addon, a))
			case u.Service != "":
				svc, ok := first.Services[u.Service]
				if !ok {
					continue
				}
				if n, ok := spec.PortNumber(svc.Ports, u.Port); ok {
					r.admit(workloadOf(envID, first, spec.KindService, u.Service), wl, n)
				}
			}
		}
	}
	for _, peer := range r.all {
		for _, wl := range known {
			if wl != peer {
				r.peer(wl, peer).any = true
			}
		}
	}
	return r
}

// rules are workload wl's rules on the nodes and the groups they name.
// A group the environment does not know (an object no spec has any more,
// whose pods are going) is reached by the "all" sandboxes alone and
// reaches the internet.
func (r *envReach) rules(wl string) ([]*networkv1.Rule, []string) {
	peers := r.in[wl]
	for _, a := range r.all {
		if a != wl && (peers == nil || peers[a] == nil) {
			if peers == nil {
				peers = map[string]*peerPorts{}
			}
			peers[a] = &peerPorts{any: true}
		}
	}
	var rules []*networkv1.Rule
	var refs []string
	for _, peer := range slices.Sorted(maps.Keys(peers)) {
		pp := peers[peer]
		refs = append(refs, peer)
		if pp.any {
			rules = append(rules, groupRule(networkv1.Direction_DIRECTION_INGRESS, peer))
			continue
		}
		rule := &networkv1.Rule{Direction: networkv1.Direction_DIRECTION_INGRESS, Protocol: networkv1.Protocol_PROTOCOL_TCP,
			Peer: &networkv1.Rule_Group{Group: peer}}
		for _, p := range slices.Sorted(maps.Keys(pp.ports)) {
			rule.Ports = append(rule.Ports, &networkv1.PortRange{First: uint32(p)})
		}
		rules = append(rules, rule)
	}
	e, ok := r.egress[wl]
	switch {
	case !ok, e.EffectiveMode() == spec.EgressInternet:
		rules = append(rules, egressTable()...)
	case e.EffectiveMode() == spec.EgressRestricted:
		for _, b := range e.Blocks {
			rules = append(rules, &networkv1.Rule{Direction: networkv1.Direction_DIRECTION_EGRESS,
				Protocol: networkv1.Protocol_PROTOCOL_ANY,
				Peer:     &networkv1.Rule_Block{Block: &networkv1.AddressBlock{Cidr: b.CIDR, Except: slices.Clone(b.Except)}}})
		}
	}
	return rules, refs
}

// buildWorkloadGroup is workload group id of environment envID: its
// derived rules (envReach.rules), the groups they name, and its members
// (store.EnvCtlGroupMembers).
func (c *Controller) buildWorkloadGroup(ctx context.Context, id string, envID uuid.UUID, pass *groupPass) (*builtGroup, error) {
	r, err := c.envReachOf(ctx, pass, envID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errNoGroup
	}
	rules, refs := r.rules(id)
	addrs, err := c.st.EnvCtlGroupMembers(ctx, envID, id)
	if err != nil {
		return nil, err
	}
	g := &networkv1.SecurityGroup{Id: id, Rules: rules, Members: members(addrs)}
	g.Version = hashVersion(g)
	return &builtGroup{g: g, kind: groupWorkload, refs: refs}, nil
}
