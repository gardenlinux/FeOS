// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package table

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/mesh"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/sandbox"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Deployment 1: web (image; http 8080, admin 9090) behind / and /admin/,
// dev (attached) behind /dev, worker (image) without a route. Deployment 2
// moves web's http to 8081 and adds api (image, idle) behind /api.
func deployment1() *spec.Spec {
	return &spec.Spec{
		Services: map[string]spec.Service{
			"web":    {Runtime: spec.RuntimeImage, Image: "web@sha256:1", Ports: []spec.Port{{Name: "http", Port: 8080}, {Name: "admin", Port: 9090}}},
			"dev":    {Runtime: spec.RuntimeAttached, Sandbox: "sbx", Command: []string{"npm", "run", "dev"}, Ports: []spec.Port{{Name: "http", Port: 3000}}},
			"worker": {Runtime: spec.RuntimeImage, Image: "worker@sha256:1", Ports: []spec.Port{{Name: "http", Port: 7070}}},
		},
		Routes: map[string]spec.Route{
			"root":  {Path: "/", Service: "web", Port: spec.PortRef{Name: "http"}},
			"admin": {Path: "/admin/", Service: "web", Port: spec.PortRef{Number: 9090}, Strip: true},
			"dev":   {Path: "/dev", Service: "dev", Port: spec.PortRef{Name: "http"}},
		},
	}
}

func deployment2() *spec.Spec {
	sp := deployment1()
	sp.Services["web"] = spec.Service{Runtime: spec.RuntimeImage, Image: "web@sha256:2",
		Ports: []spec.Port{{Name: "http", Port: 8081}, {Name: "admin", Port: 9090}}}
	sp.Services["api"] = spec.Service{Runtime: spec.RuntimeImage, Image: "api@sha256:2", Idle: "5m0s",
		Ports: []spec.Port{{Name: "grpc", Port: 7000}}}
	sp.Routes["api"] = spec.Route{Path: "/api", Service: "api", Port: spec.PortRef{Name: "grpc"}}
	return sp
}

// specsOf serves deployments from a map; a missing one has no usable spec.
func specsOf(m map[int]*spec.Spec) SpecFunc {
	return func(n int) (*spec.Spec, error) {
		if sp, ok := m[n]; ok {
			return sp, nil
		}
		return nil, fmt.Errorf("deployment %d: %w", n, ErrSpec)
	}
}

var envID = uuid.MustParse("0b1f6b54-3c6e-4d2f-9d7b-5d1c0e7c9a11")

func baseEnv(deployment, deployed int, instances ...Instance) EnvRows {
	return EnvRows{ID: envID, Tenant: "acme", Name: "web", HostLabel: "web-acme", Deployment: deployment, Deployed: deployed,
		CreatedAt: time.Unix(1700000000, 0), Instances: instances}
}

// ready is a ready, running instance of service on node n1 at its
// deployment, with an address and the given admitted ports.
func ready(id, service string, deployment int, ports ...int) Instance {
	return Instance{ID: podid.ID(id), Service: service, Deployment: deployment, Phase: model.PhaseReady,
		ClientState: model.StateRunning, Address: "fd00:0:0:1::" + id[len(id)-1:], Node: "n1", IngressPorts: ports,
		GroupVersion: 3}
}

func build(t *testing.T, zone string, e EnvRows, specs SpecFunc) (wire.Env, []podid.ID) {
	t.Helper()
	got, ids, err := BuildEnv(zone, e, specs)
	if err != nil {
		t.Fatal(err)
	}
	return got, ids
}

func service(t *testing.T, e wire.Env, name string) wire.Service {
	t.Helper()
	for _, s := range e.Services {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no service %s in %+v", name, e.Services)
	return wire.Service{}
}

// The routing deployment is the deployed one, else the latest; a route to
// an attached service is the door's, and only image services that a
// route sends to are services.
func TestBuildEnvRoutes(t *testing.T) {
	specs := specsOf(map[int]*spec.Spec{1: deployment1(), 2: deployment2()})
	got, _ := build(t, "apps.example", baseEnv(2, 1), specs)
	want := []wire.Route{
		{Name: "admin", Path: "/admin", Service: "web", Port: "9090", Strip: true},
		{Name: "dev", Path: "/dev", Service: "dev", Port: "http", Door: true},
		{Name: "root", Path: "/", Service: "web", Port: "http"},
	}
	if got.Deployment != 1 || !reflect.DeepEqual(got.Routes, want) {
		t.Fatalf("deployed: deployment %d, routes %+v", got.Deployment, got.Routes)
	}
	if len(got.Services) != 1 || got.Services[0].Name != "web" || got.Services[0].Idle {
		t.Fatalf("services %+v", got.Services)
	}
	if got.ID != envID.String() || got.Tenant != "acme" || got.Name != "web" {
		t.Fatalf("env %+v", got)
	}

	// Before a first rollout ends: the latest deployment, with api.
	got, _ = build(t, "apps.example", baseEnv(2, 0), specs)
	if got.Deployment != 2 || len(got.Routes) != 4 || got.Routes[0].Name != "admin" || got.Routes[1] !=
		(wire.Route{Name: "api", Path: "/api", Service: "api", Port: "grpc"}) {
		t.Fatalf("latest: deployment %d, routes %+v", got.Deployment, got.Routes)
	}
	if names := []string{got.Services[0].Name, got.Services[1].Name}; len(got.Services) != 2 ||
		!slices.Equal(names, []string{"api", "web"}) || !got.Services[0].Idle || got.Services[1].Idle {
		t.Fatalf("latest: services %+v", got.Services)
	}

	// The routing deployment without a usable spec: ErrSpec.
	if _, _, err := BuildEnv("apps.example", baseEnv(3, 0), specs); !errors.Is(err, ErrSpec) {
		t.Fatalf("missing spec: %v", err)
	}
	// Another error passes through without ErrSpec.
	boom := errors.New("boom")
	if _, _, err := BuildEnv("apps.example", baseEnv(1, 0), func(int) (*spec.Spec, error) { return nil, boom }); !errors.Is(err, boom) ||
		errors.Is(err, ErrSpec) {
		t.Fatalf("read error: %v", err)
	}
}

// Classify, in its order: ready; asleep (paused, hibernated, pausing,
// hibernating, or ready and running with an op); waking (resuming, or a
// resume or restore op); pending (pending, creating, or running but not
// ready); the rest not counted.
func TestClassify(t *testing.T) {
	for _, c := range []struct {
		phase, state, op string
		want             Class
	}{
		{model.PhaseReady, model.StateRunning, "", ClassReady},
		{model.PhaseReady, model.StateRunning, "pause", ClassAsleep},
		{model.PhaseReady, model.StateRunning, "hibernate", ClassAsleep},
		{model.PhaseReady, model.StatePaused, "", ClassAsleep},
		{model.PhaseUnready, model.StateHibernated, "", ClassAsleep},
		{model.PhaseReady, model.StatePausing, "pause", ClassAsleep},
		{model.PhaseReady, model.StateHibernating, "hibernate", ClassAsleep},
		{model.PhaseReady, model.StateResuming, "resume", ClassWaking},
		{model.PhaseReady, model.StateCreating, "restore", ClassWaking},
		{model.PhaseStarting, model.StateCreating, "create", ClassPending},
		{model.PhaseStarting, model.StatePending, "", ClassPending},
		{model.PhaseStarting, model.StateRunning, "", ClassPending},
		{model.PhaseUnready, model.StateRunning, "", ClassPending},
		{model.PhaseStarting, model.StateRunning, "pause", ClassPending},
		{model.PhaseFailed, model.StateStopped, "", ClassNone},
		{model.PhaseReady, model.StateFailed, "", ClassNone},
		{model.PhaseReady, model.StateUnknown, "", ClassNone},
		{model.PhaseReady, model.StateLost, "", ClassNone},
	} {
		if got := Classify(c.phase, c.state, c.op); got != c.want {
			t.Errorf("Classify(%s, %s, %q) = %d, want %d", c.phase, c.state, c.op, got, c.want)
		}
		if got := Asleep(c.phase, c.state, c.op); got != (c.want == ClassAsleep) {
			t.Errorf("Asleep(%s, %s, %q) = %v", c.phase, c.state, c.op, got)
		}
	}
	if opResume != string(sandbox.OpResume) || opRestore != string(sandbox.OpRestore) {
		t.Fatalf("op kinds %q %q", sandbox.OpResume, sandbox.OpRestore)
	}
}

// Endpoints are ready instances with an address, a node and an admitted
// port, each port named by the route and numbered by the instance's own
// deployment (else the routing deployment's), only when admitted; the others
// are counted.
func TestBuildEnvInstances(t *testing.T) {
	specs := specsOf(map[int]*spec.Spec{1: deployment1(), 2: deployment2()})
	asleepOp := ready("a0000000006", "web", 1, 8080)
	asleepOp.Op = "pause"
	noAddress := ready("a0000000004", "web", 1, 8080)
	noAddress.Address = ""
	noNode := ready("a0000000005", "web", 1, 8080)
	noNode.Node = ""
	gone := ready("a0000000007", "web", 9, 8080, 9090) // its deployment has no usable spec
	state := func(id, phase, clientState, op string) Instance {
		return Instance{ID: podid.ID(id), Service: "web", Deployment: 1, Phase: phase, ClientState: clientState, Op: op}
	}
	e := baseEnv(2, 1,
		ready("a0000000001", "web", 1, 9090, 8080),
		ready("a0000000002", "web", 2, 8081), // own deployment: http 8081
		ready("a0000000003", "web", 1),       // nothing admitted
		noAddress, noNode, asleepOp, gone,
		state("b0000000001", model.PhaseReady, model.StatePaused, ""),
		state("b0000000002", model.PhaseReady, model.StateHibernated, ""),
		state("b0000000003", model.PhaseReady, model.StatePausing, "pause"),
		state("b0000000004", model.PhaseReady, model.StateHibernating, "hibernate"),
		state("c0000000001", model.PhaseReady, model.StateResuming, "resume"),
		state("c0000000002", model.PhaseReady, model.StateCreating, "restore"),
		state("d0000000001", model.PhaseStarting, model.StateCreating, "create"),
		state("d0000000002", model.PhaseUnready, model.StateRunning, ""),
		state("d0000000003", model.PhaseStarting, model.StatePending, ""),
		state("e0000000001", model.PhaseFailed, model.StateStopped, ""),
		state("e0000000002", model.PhaseReady, model.StateFailed, ""),
		state("e0000000003", model.PhaseReady, model.StateLost, ""),
		state("e0000000004", model.PhaseReady, model.StateUnknown, ""),
		ready("f0000000001", "worker", 1, 7070), // no route
	)
	got, ids := build(t, "apps.example", e, specs)
	web := service(t, got, "web")
	wantEndpoints := []wire.Endpoint{
		{Instance: "a0000000001", Address: "fd00:0:0:1::1", Node: "n1", Ports: map[string]int{"http": 8080, "9090": 9090}, GroupVersion: 3},
		{Instance: "a0000000002", Address: "fd00:0:0:1::2", Node: "n1", Ports: map[string]int{"http": 8081}, GroupVersion: 3},
		{Instance: "a0000000007", Address: "fd00:0:0:1::7", Node: "n1", Ports: map[string]int{"http": 8080, "9090": 9090}, GroupVersion: 3},
	}
	if !reflect.DeepEqual(web.Endpoints, wantEndpoints) {
		t.Fatalf("endpoints %+v", web.Endpoints)
	}
	// Asleep: four states and ready+running with an op. Waking: resuming,
	// restore. Pending: not admitted, no address, no node, creating,
	// unready, pending. Stopped, failed, lost, unknown: not counted.
	if web.Asleep != 5 || web.Waking != 2 || web.Pending != 6 {
		t.Fatalf("counts asleep %d, waking %d, pending %d", web.Asleep, web.Waking, web.Pending)
	}
	if len(got.Services) != 1 {
		t.Fatalf("an unrouted service is listed: %+v", got.Services)
	}
	// Every instance of the routed service is referenced, the unrouted
	// worker's not.
	var want []podid.ID
	for _, in := range e.Instances {
		if in.Service == "web" {
			want = append(want, in.ID)
		}
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("referenced %v, want %v", ids, want)
	}

	// An instance of deployment 2 whose node admits only the old port:
	// pending. An instance whose node admits a port of a route of
	// another service only: pending.
	got, _ = build(t, "", baseEnv(2, 1, ready("a0000000001", "web", 2, 8080), ready("a0000000002", "web", 1, 7070)), specs)
	if web := service(t, got, "web"); len(web.Endpoints) != 0 || web.Pending != 2 {
		t.Fatalf("old port only: %+v", web)
	}
}

// Idle: the routing deployment's service has an idle policy, or one of its
// instances may idle-pause.
func TestBuildEnvIdle(t *testing.T) {
	specs := specsOf(map[int]*spec.Spec{1: deployment1()})
	inst := ready("a0000000001", "web", 1, 8080)
	got, _ := build(t, "z", baseEnv(1, 1, inst), specs)
	if service(t, got, "web").Idle {
		t.Fatal("idle without a policy")
	}
	inst.Idle = true
	got, _ = build(t, "z", baseEnv(1, 1, inst), specs)
	if !service(t, got, "web").Idle {
		t.Fatal("an instance that may idle-pause: not idle")
	}
}

// Hosts: the label under the zone; none without a zone. The hash
// identifies the content: equal for equal input, whatever the order of
// the spec's maps, and different when anything changes.
func TestBuildEnvHostsAndHash(t *testing.T) {
	specs := specsOf(map[int]*spec.Spec{1: deployment1()})
	e := baseEnv(1, 1, ready("a0000000002", "web", 1, 8080), ready("a0000000001", "web", 1, 8080))
	got, _ := build(t, "apps.example", e, specs)
	if !slices.Equal(got.Hosts, []string{"web-acme.apps.example"}) {
		t.Fatalf("hosts %v", got.Hosts)
	}
	noZone, _ := build(t, "", e, specs)
	if noZone.Hosts == nil || len(noZone.Hosts) != 0 || noZone.Hash == got.Hash {
		t.Fatalf("without a zone: hosts %v, hash %s", noZone.Hosts, noZone.Hash)
	}

	h, err := Hash(got)
	if err != nil || h != got.Hash || len(got.Hash) != 64 {
		t.Fatalf("hash %s, recomputed %s %v", got.Hash, h, err)
	}
	// The same input in another order (instances, a fresh spec): the
	// same table and hash.
	swapped := baseEnv(1, 1, e.Instances[1], e.Instances[0])
	again, _ := build(t, "apps.example", swapped, specsOf(map[int]*spec.Spec{1: deployment1()}))
	if !reflect.DeepEqual(again, got) {
		t.Fatalf("not stable:\n%+v\n%+v", again, got)
	}
	// A JSON round trip (what the ingress receives) hashes the same.
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var back wire.Env
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if h, _ := Hash(back); h != got.Hash {
		t.Fatalf("round trip hash %s, want %s", h, got.Hash)
	}
	// Any change changes it.
	changed := e
	changed.Instances = []Instance{e.Instances[0]}
	other, _ := build(t, "apps.example", changed, specs)
	if other.Hash == got.Hash {
		t.Fatal("a removed endpoint kept the hash")
	}
	moved := ready("a0000000001", "web", 1, 8080)
	moved.GroupVersion = 4
	other, _ = build(t, "apps.example", baseEnv(1, 1, e.Instances[0], moved), specs)
	if other.Hash == got.Hash {
		t.Fatal("a new group version kept the hash")
	}
}

// routesSpec is an image service web behind routes routes, each with a
// path of n characters of '&' and its number.
func routesSpec(routes, n int) *spec.Spec {
	sp := &spec.Spec{
		Services: map[string]spec.Service{"web": {Runtime: spec.RuntimeImage, Image: "web@sha256:1",
			Ports: []spec.Port{{Name: "http", Port: 8080}}}},
		Routes: map[string]spec.Route{},
	}
	for i := range routes {
		sp.Routes[fmt.Sprintf("r%03d", i)] = spec.Route{Path: fmt.Sprintf("/%s%03d", strings.Repeat("&", n), i),
			Service: "web", Port: spec.PortRef{Name: "http"}}
	}
	return sp
}

// F13: an environment whose entry would take more than MaxEnvBytes coded
// cannot be routed (ErrTooLarge), as a deployment stored before route paths
// were bounded can make it, so that the builder leaves it out. The
// largest valid spec, with the most endpoints, stays far below; the size
// counted is what the wire codes.
func TestBuildEnvTooLarge(t *testing.T) {
	huge := routesSpec(spec.MaxRoutes, 20000)
	_, _, err := BuildEnv("apps.example", baseEnv(1, 0), specsOf(map[int]*spec.Spec{1: huge}))
	if !errors.Is(err, ErrTooLarge) || errors.Is(err, ErrSpec) || !unroutable(err) ||
		!strings.Contains(err.Error(), "web of tenant acme") {
		t.Fatalf("huge: %v", err)
	}
	if !unroutable(fmt.Errorf("x: %w", ErrSpec)) || unroutable(errors.New("read")) {
		t.Fatal("unroutable")
	}

	var instances []Instance
	for i := range spec.MaxReplicas * 3 {
		instances = append(instances, ready(fmt.Sprintf("a%010d", i), "web", 1, 8080))
	}
	largest := routesSpec(spec.MaxRoutes, spec.MaxRoutePathBytes-4)
	got, _ := build(t, "apps.example", baseEnv(1, 0, instances...), specsOf(map[int]*spec.Spec{1: largest}))
	n, err := wire.Size(got)
	if err != nil {
		t.Fatal(err)
	}
	if _, size, err := digest(got); err != nil || size != n {
		t.Fatalf("digest's size %d, coded %d: %v", size, n, err)
	}
	if n > MaxEnvBytes/4 {
		t.Fatalf("the largest valid environment takes %d bytes", n)
	}
	t.Logf("128 routes of %d bytes, %d endpoints: %d bytes", spec.MaxRoutePathBytes, len(instances), n)
}

// Group turns the store's rows into environments, those without an
// instance with none.
func TestGroup(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	idle := 0
	at := time.Unix(1700000000, 0)
	rows := []store.IngressRoutingRow{
		{EnvID: a, Owner: "acme", Name: "a", HostLabel: "a-acme", Deployment: 2, Deployed: 1, CreatedAt: at,
			HasInstance: true, SandboxID: "a0000000001", Object: "web", Replica: 0, InstanceDeployment: 1, Phase: model.PhaseReady,
			IngressPorts: []int{80}, Address: "fd00::1", ClientState: model.StateRunning, Node: "n1", NodeGroupVersion: 5},
		{EnvID: a, Owner: "acme", Name: "a", HostLabel: "a-acme", Deployment: 2, Deployed: 1, CreatedAt: at,
			HasInstance: true, SandboxID: "a0000000002", Object: "web", Replica: 1, InstanceDeployment: 2, Phase: model.PhaseStarting,
			ClientState: model.StateCreating, Op: "create", IdleAfterS: &idle},
		{EnvID: b, Owner: "acme", Name: "b", HostLabel: "b-acme", Deployment: 1, CreatedAt: at},
	}
	got := Group(rows)
	want := []EnvRows{
		{ID: a, Tenant: "acme", Name: "a", HostLabel: "a-acme", Deployment: 2, Deployed: 1, CreatedAt: at, Instances: []Instance{
			{ID: "a0000000001", Service: "web", Deployment: 1, Phase: model.PhaseReady, ClientState: model.StateRunning,
				Address: "fd00::1", Node: "n1", IngressPorts: []int{80}, GroupVersion: 5, Idle: true},
			{ID: "a0000000002", Service: "web", Replica: 1, Deployment: 2, Phase: model.PhaseStarting,
				ClientState: model.StateCreating, Op: "create"},
		}},
		{ID: b, Tenant: "acme", Name: "b", HostLabel: "b-acme", Deployment: 1, CreatedAt: at},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// BuildMesh lists the nodes by name, without the ingress instances.
func TestBuildMesh(t *testing.T) {
	peers := mesh.Peers{Version: 9, Nodes: map[string]mesh.Peer{
		"n2":                          {Node: "n2", PublicKey: []byte{2}, Endpoint: "10.0.0.2:51820", PodPrefix: "fd00:0:0:2::/64"},
		"n1":                          {Node: "n1", PublicKey: []byte{1}, Endpoint: "10.0.0.1:51820", PodPrefix: "fd00:0:0:1::/64"},
		mesh.IngressPeerName("ing-0"): {Node: mesh.IngressPeerName("ing-0"), PublicKey: []byte{3}, Endpoint: "10.0.0.3:51820", PodPrefix: "fd00:0:0:3::/64"},
	}}
	got := BuildMesh(peers)
	want := wire.Mesh{Version: 9, Peers: []wire.Peer{
		{Name: "n1", PublicKey: []byte{1}, Endpoint: "10.0.0.1:51820", Prefix: "fd00:0:0:1::/64"},
		{Name: "n2", PublicKey: []byte{2}, Endpoint: "10.0.0.2:51820", Prefix: "fd00:0:0:2::/64"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mesh %+v", got)
	}
	if empty := BuildMesh(mesh.Peers{}); empty.Peers == nil || len(empty.Peers) != 0 {
		t.Fatalf("empty mesh %+v", empty)
	}
}
