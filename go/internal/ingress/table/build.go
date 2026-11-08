// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package table

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// EnvRows is one public environment as the table is built from it: the
// environment and its service instances that are not stopping.
type EnvRows struct {
	ID        uuid.UUID
	Tenant    string
	Name      string
	HostLabel string
	// Zone is its tenant's zone (tenants.zone; "": the apps zone).
	Zone string
	// Deployment is the latest desired deployment, Deployed the latest
	// deployed one (0: none).
	Deployment, Deployed int
	CreatedAt            time.Time
	Instances            []Instance
	// Domains are the environment's domains the ingress serves, with
	// their certificates and keys opened (store.IngressDomains), in the
	// order their entry takes them.
	Domains []wire.Domain
}

// MaxDomainBytes is the most one domain takes in an environment's entry
// coded, its name in the hosts included: a certificate's chain is a few
// KiB, and a domain beyond this is left out.
const MaxDomainBytes = 64 << 10

// Instance is one service instance of an environment, with its sandbox
// and the node it runs on.
type Instance struct {
	ID      podid.ID
	Service string // the service's name in the spec
	Replica int
	// Deployment is the instance's own deployment: the one whose target it was
	// last kept for.
	Deployment  int
	Phase       string // model.Phase*
	ClientState string // model.State*
	Op          string // "": none
	Address     string // the pod's IPv6; "": none observed
	Node        string // "": none
	// IngressPorts are the ports its node admits to the ingress (the
	// group ing-<id> as put on or listed by the node).
	IngressPorts []int
	// GroupVersion is the version of the security group ferun-ingress
	// that its node holds.
	GroupVersion uint64
	// Idle: the idle loop may pause its sandbox (idle_after_s NULL, the
	// platform's default, or above 0).
	Idle bool
}

// Group turns the rows of store.IngressRouting into environments, in the
// rows' order, each with its instances.
func Group(rows []store.IngressRoutingRow) []EnvRows {
	var out []EnvRows
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1].ID != r.EnvID {
			out = append(out, EnvRows{ID: r.EnvID, Tenant: r.Owner, Name: r.Name, HostLabel: r.HostLabel,
				Zone: r.Zone, Deployment: r.Deployment, Deployed: r.Deployed, CreatedAt: r.CreatedAt})
		}
		if !r.HasInstance {
			continue
		}
		e := &out[len(out)-1]
		e.Instances = append(e.Instances, Instance{ID: r.SandboxID, Service: r.Object, Replica: r.Replica,
			Deployment: r.InstanceDeployment, Phase: r.Phase, ClientState: r.ClientState, Op: r.Op, Address: r.Address,
			Node: r.Node, IngressPorts: r.IngressPorts, GroupVersion: uint64(max(r.NodeGroupVersion, 0)),
			Idle: r.IdleAfterS == nil || *r.IdleAfterS > 0})
	}
	return out
}

// RoutingDeployment is the deployment whose routes the ingress serves: the
// deployed one, else (before a first rollout ends) the latest desired
// one.
func RoutingDeployment(deployment, deployed int) int {
	if deployed > 0 {
		return deployed
	}
	return deployment
}

// Class is how the table counts a service instance (wire.Service).
type Class int

const (
	// ClassNone is an instance that is stopped, failed, unknown or lost
	// without an operation that wakes it: not counted; the controller
	// heals it.
	ClassNone Class = iota
	// ClassReady is ready, running and without an operation: an
	// endpoint once it has an address, a node and an admitted port,
	// pending until then.
	ClassReady
	// ClassAsleep is paused, hibernated, pausing or hibernating, or
	// ready and running with an operation (a pause on its way): Wake
	// resumes it.
	ClassAsleep
	// ClassWaking is resuming, or has an operation resume or restore.
	ClassWaking
	// ClassPending is pending or creating, or running but not ready.
	ClassPending
)

// The operations that wake a sandbox (sandbox.OpResume, sandbox.OpRestore).
const (
	opResume  = "resume"
	opRestore = "restore"
)

// Classify returns the class of a service instance of phase whose
// sandbox is in clientState with op in flight ("": none). The classes
// are checked in their order: ready, asleep, waking, pending.
func Classify(phase, clientState, op string) Class {
	ready, running := phase == model.PhaseReady, clientState == model.StateRunning
	switch {
	case ready && running && op == "":
		return ClassReady
	case ready && running:
		return ClassAsleep
	}
	switch clientState {
	case model.StatePaused, model.StateHibernated, model.StatePausing, model.StateHibernating:
		return ClassAsleep
	}
	switch {
	case clientState == model.StateResuming || op == opResume || op == opRestore:
		return ClassWaking
	case clientState == model.StatePending || clientState == model.StateCreating || running:
		return ClassPending
	}
	return ClassNone
}

// Asleep reports whether a service instance is asleep (ClassAsleep): the
// instances Wake resumes, as the route door picks them.
func Asleep(phase, clientState, op string) bool {
	return Classify(phase, clientState, op) == ClassAsleep
}

// BuildEnv builds the table's entry of environment e for the platform's
// zones: its default host is in its tenant's zone (zones.Of; "": no
// default host, so the ingress serves nothing of it but its domains), and returns
// the sandboxes of the instances it counted, whose changes change it.
//
//   - Its routes are those of its routing deployment (RoutingDeployment), by
//     name; a route's Path is its prefix as the route door matches it
//     (spec.RoutePrefix). A route to an attached service is the route
//     door's alone (Door). Its From are the spec route's clients' blocks,
//     canonical (routeFrom).
//   - Each image service that a route sends to is a Service.
//     An instance of it is an endpoint when it is ready, running,
//     without an operation, with an address and a node, and its node
//     admits at least one of its routed ports; its ports are named by
//     the routes' Port and numbered by the instance's own deployment, else
//     by the routing deployment. The others are counted by their Class; a
//     ready one without an admitted port is pending.
//   - Its hosts are its domains (e.Domains), with their certificates in
//     Domains, in their order, as long as the entry stays within
//     MaxEnvBytes and each within MaxDomainBytes: a domain that does not
//     fit is left out, never the environment. A name twice is taken once.
//     Its default host, <label>.<zone>, is its host only while none of its
//     domains is: an environment on a domain of its own is not served in
//     its zone too (ADR 0021).
//   - Its hash identifies the rest of its content, the certificates
//     included, so that a renewed one is sent again.
//
// An error wrapping ErrSpec means the routing deployment has no usable spec,
// one wrapping ErrTooLarge that the entry would take more than
// MaxEnvBytes coded: either way the environment cannot be routed. Other
// errors are specs' read errors.
func BuildEnv(zones model.Zones, e EnvRows, specs SpecFunc) (wire.Env, []podid.ID, error) {
	number := RoutingDeployment(e.Deployment, e.Deployed)
	sp, err := specs(number)
	if err != nil {
		return wire.Env{}, nil, fmt.Errorf("environment %s, routing deployment %d: %w", e.ID, number, err)
	}
	defaultHosts := []string{}
	if zone := zones.Of(e.Zone); zone != "" && e.HostLabel != "" {
		defaultHosts = append(defaultHosts, e.HostLabel+"."+zone)
	}
	out := wire.Env{ID: e.ID.String(), Tenant: e.Tenant, Name: e.Name, Hosts: defaultHosts, Deployment: number,
		Routes: []wire.Route{}, Services: []wire.Service{}}
	byService := map[string][]spec.RouteView{}
	for _, r := range spec.IngressRoutes(sp) {
		out.Routes = append(out.Routes, wire.Route{Name: r.Name, Path: r.Prefix, Service: r.Service, Port: r.Port.String(),
			Strip: r.Strip, Door: r.Door, From: routeFrom(sp.Routes[r.Name].From)})
		if target, ok := sp.Services[r.Service]; ok && !r.Door && !spec.IsAttached(target) {
			byService[r.Service] = append(byService[r.Service], r)
		}
	}
	// The own deployments' specs, read once each; nil: none usable, so the
	// routing deployment numbers the ports.
	own := map[int]*spec.Spec{number: sp}
	ownSpec := func(n int) (*spec.Spec, error) {
		if s, ok := own[n]; ok {
			return s, nil
		}
		s, err := specs(n)
		if err != nil && !errors.Is(err, ErrSpec) {
			return nil, fmt.Errorf("environment %s, deployment %d: %w", e.ID, n, err)
		}
		own[n] = s
		return s, nil
	}
	var referenced []podid.ID
	for _, name := range slices.Sorted(maps.Keys(byService)) {
		svc := wire.Service{Name: name, Idle: sp.Services[name].Idle != "", Endpoints: []wire.Endpoint{}}
		for _, in := range e.Instances {
			if in.Service != name {
				continue
			}
			referenced = append(referenced, in.ID)
			svc.Idle = svc.Idle || in.Idle
			switch Classify(in.Phase, in.ClientState, in.Op) {
			case ClassReady:
				var ports map[string]int
				if in.Address != "" && in.Node != "" {
					s, err := ownSpec(in.Deployment)
					if err != nil {
						return wire.Env{}, nil, err
					}
					ports = admittedPorts(name, in.IngressPorts, byService[name], s, sp)
				}
				if len(ports) == 0 {
					svc.Pending++
					continue
				}
				svc.Endpoints = append(svc.Endpoints, wire.Endpoint{Instance: in.ID.String(), Address: in.Address,
					Node: in.Node, Ports: ports, GroupVersion: in.GroupVersion})
			case ClassAsleep:
				svc.Asleep++
			case ClassWaking:
				svc.Waking++
			case ClassPending:
				svc.Pending++
			}
		}
		slices.SortFunc(svc.Endpoints, func(a, b wire.Endpoint) int { return strings.Compare(a.Instance, b.Instance) })
		out.Services = append(out.Services, svc)
	}
	h, size, err := digest(out)
	if err != nil {
		return wire.Env{}, nil, fmt.Errorf("environment %s: %w", e.ID, err)
	}
	if size > MaxEnvBytes {
		return wire.Env{}, nil, fmt.Errorf("environment %s (%s of tenant %s), routing deployment %d: %d bytes, at most %d: %w",
			e.ID, e.Name, e.Tenant, number, size, MaxEnvBytes, ErrTooLarge)
	}
	if with, ok, err := withDomains(out, e.Domains, size); err != nil {
		return wire.Env{}, nil, fmt.Errorf("environment %s: %w", e.ID, err)
	} else if ok {
		out = with
		if h, size, err = digest(out); err != nil {
			return wire.Env{}, nil, fmt.Errorf("environment %s: %w", e.ID, err)
		}
		if size > MaxEnvBytes {
			// The estimate was short: the domains stay out.
			out.Hosts, out.Domains = defaultHosts, nil
			if h, _, err = digest(out); err != nil {
				return wire.Env{}, nil, fmt.Errorf("environment %s: %w", e.ID, err)
			}
		}
	}
	out.Hash = h
	return out, referenced, nil
}

// withDomains returns e, which takes size bytes coded, with the domains
// of ds that fit as its Hosts and Domains in place of its default host
// (BuildEnv), and whether any did. The budget counts the default host,
// which errs on the safe side.
func withDomains(e wire.Env, ds []wire.Domain, size int) (wire.Env, bool, error) {
	if len(ds) == 0 {
		return e, false, nil
	}
	// ,"domains":[] once, then each domain and its host with a comma.
	budget := MaxEnvBytes - size - len(`,"domains":[]`)
	hosts := []string{}
	var domains []wire.Domain
	for _, d := range ds {
		if d.Name == "" || slices.Contains(hosts, d.Name) {
			continue
		}
		dn, err := wire.Size(&d)
		if err != nil {
			return wire.Env{}, false, err
		}
		hn, err := wire.Size(d.Name)
		if err != nil {
			return wire.Env{}, false, err
		}
		n := dn + hn + 2
		if n > MaxDomainBytes || n > budget {
			continue
		}
		budget -= n
		hosts = append(hosts, d.Name)
		domains = append(domains, d)
	}
	if len(domains) == 0 {
		return e, false, nil
	}
	e.Hosts, e.Domains = hosts, domains
	return e, true, nil
}

// routeFrom returns a route's clients' blocks canonical (masked), in
// their order; nil when it has none. The plan refuses a block that does
// not parse, so only a spec stored without the plan has one: it goes as
// written, and the ingress, which cannot parse it either, admits no
// client by it. Dropping it would open a route whose blocks all are such.
func routeFrom(from []string) []string {
	var out []string
	for _, f := range from {
		if p, err := netip.ParsePrefix(f); err == nil {
			f = p.Masked().String()
		}
		out = append(out, f)
	}
	return out
}

// admittedPorts returns the ports of an instance of service that its node
// admits (admitted), under the names routes give them (Route.Port): each
// route's port numbered in own, the instance's own deployment, else in
// routing.
func admittedPorts(service string, admitted []int, routes []spec.RouteView, own, routing *spec.Spec) map[string]int {
	ports := map[string]int{}
	for _, r := range routes {
		n := 0
		if own != nil {
			n, _ = spec.PortNumber(own.Services[service].Ports, r.Port)
		}
		if n <= 0 {
			n, _ = spec.PortNumber(routing.Services[service].Ports, r.Port)
		}
		if n > 0 && slices.Contains(admitted, n) {
			ports[r.Port.String()] = n
		}
	}
	return ports
}

// Hash returns the hash of e's content: sha256 of its JSON as the wire
// codes it (wire.Codec), with Hash empty, in hex.
func Hash(e wire.Env) (string, error) {
	h, _, err := digest(e)
	return h, err
}

// digest returns e's hash (Hash) and the bytes e takes coded with that
// hash, from one coding.
func digest(e wire.Env) (string, int, error) {
	e.Hash = ""
	b, err := wire.Codec{}.Marshal(e)
	if err != nil {
		return "", 0, fmt.Errorf("hash: %w", err)
	}
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	// The hash takes its place between the quotes of "hash":"".
	return h, len(b) + len(h), nil
}

// BuildMesh returns the ingress's WireGuard peers of peers: the nodes,
// by name, without the ingress instances (mesh.IsIngressPeer), at peers'
// version.
func BuildMesh(peers mesh.Peers) wire.Mesh {
	out := wire.Mesh{Version: peers.Version, Peers: []wire.Peer{}}
	for _, name := range slices.Sorted(maps.Keys(peers.Nodes)) {
		if mesh.IsIngressPeer(name) {
			continue
		}
		p := peers.Nodes[name]
		out.Peers = append(out.Peers, wire.Peer{Name: name, PublicKey: slices.Clone(p.PublicKey), Endpoint: p.Endpoint,
			Prefix: p.PodPrefix})
	}
	return out
}
