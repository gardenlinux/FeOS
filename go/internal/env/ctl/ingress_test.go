// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"bytes"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// testULA is the cluster's pod network in the ingress tests.
var testULA = netip.MustParsePrefix("fd5e:1b2c:93a0::/64")

// The routes of ingressSpec: web's http port at /, and its admin port at
// /admin.
const (
	webRoute   = `"web":{"path":"/","service":"web","port":"http"}`
	adminRoute = `"admin":{"path":"/admin","service":"web","port":"admin"}`
)

// ingressSpec is a spec with an image service web of two ports, an addon,
// a manual job and routes (the routes' JSON, comma-separated): its
// releases differ in their routes and replicas only, so that web's
// template stays.
func ingressSpec(replicas int, routes ...string) string {
	return fmt.Sprintf(`{"services":{"web":{"image":%q,"command":["/app"],"replicas":%d,
		"ports":[{"name":"http","port":8080},{"name":"admin","port":9090}]}},
		"addons":{"cache":{"type":"valkey","version":"8","plan":"dev"}},
		"jobs":{"once":{"image":%q,"on":"manual"}},
		"routes":{%s}}`, testImage, replicas, testImage, strings.Join(routes, ","))
}

// webSpec is a spec with the image service web alone, of two ports, and
// routes.
func webSpec(replicas int, routes ...string) string {
	return fmt.Sprintf(`{"services":{"web":{"image":%q,"command":["/app"],"replicas":%d,
		"ports":[{"name":"http","port":8080},{"name":"admin","port":9090}]}},"routes":{%s}}`,
		testImage, replicas, strings.Join(routes, ","))
}

// meshIngress makes the cluster meshed and joins an ingress to its mesh,
// as its Hello does; it returns the ingress's address.
func (h *harness) meshIngress(name string) string {
	h.t.Helper()
	if err := h.st.InsertPodNetwork(bg, testULA); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.InsertIngress(bg, name); err != nil {
		h.t.Fatal(err)
	}
	var j store.IngressMeshJoin
	err := h.st.InTx(bg, func(tx *store.Store) error {
		g, err := tx.LockIngress(bg, name, store.ForUpdate)
		if err != nil {
			return err
		}
		j, err = tx.JoinIngressMesh(bg, g, testULA, bytes.Repeat([]byte{7}, 32), "10.250.0.9:51820")
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return j.Address
}

// setPublic makes an environment public or not as the service's
// SetPublic does: with env-reconcile and env-groups woken in the same
// transaction.
func (h *harness) setPublic(envID uuid.UUID, public bool) store.Environment {
	h.t.Helper()
	var e store.Environment
	err := h.st.InTx(bg, func(tx *store.Store) error {
		var err error
		if e, err = tx.SetEnvironmentPublic(bg, envID, public); err != nil {
			return err
		}
		if err := tx.Notify(bg, db.ChanLeader, HintReconcile); err != nil {
			return err
		}
		return tx.Notify(bg, db.ChanLeader, HintGroups)
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// ingressGroupVersion is pod_network.ingress_group_version and the members
// of ferun-ingress.
func (h *harness) ingressGroupVersion() (int64, []string) {
	h.t.Helper()
	members, v, err := h.st.IngressGroup(bg)
	if err != nil {
		h.t.Fatal(err)
	}
	return v, members
}

// nodeGroupVersion is the version of ferun-ingress recorded for a node.
func (h *harness) nodeGroupVersion(node string) int64 {
	h.t.Helper()
	n, err := h.st.GetNode(bg, node)
	if err != nil {
		h.t.Fatal(err)
	}
	return n.IngressGroupVersion
}

// ids are the rows' sandbox IDs.
func ids(rows []store.EnvInstanceRow) []podid.ID {
	out := make([]podid.ID, len(rows))
	for i, r := range rows {
		out[i] = r.Sandbox.ID
	}
	slices.Sort(out)
	return out
}

// hasIngGroup reports whether a row's template names its own ing group
// and has the flag.
func hasIngGroup(r store.EnvInstanceRow) bool {
	t := r.Sandbox.PodTemplate
	return t != nil && t.IngressGroup && slices.Contains(t.SecurityGroups, model.IngressGroupID(r.Sandbox.ID))
}

// namesNoIngGroup reports whether a row's template names no ing group and
// has no flag.
func namesNoIngGroup(r store.EnvInstanceRow) bool {
	t := r.Sandbox.PodTemplate
	if t == nil || t.IngressGroup {
		return false
	}
	return !slices.ContainsFunc(t.SecurityGroups, func(g string) bool { return strings.HasPrefix(g, "ing-") })
}

// storedHash is the template hash of a row's stored template, its labels
// and its own ing group left out: what the plan hashed.
func storedHash(r store.EnvInstanceRow) string {
	t := *r.Sandbox.PodTemplate
	t.Labels = nil
	t.SecurityGroups = slices.DeleteFunc(slices.Clone(t.SecurityGroups), func(g string) bool {
		return g == model.IngressGroupID(r.Sandbox.ID)
	})
	return t.Hash(r.Sandbox.Image, r.Sandbox.VCPUs, r.Sandbox.MemoryMiB)
}

// ingRule is the one rule of an ing group that opens ports to the
// ingress.
func ingRule(ports ...int) *networkv1.Rule {
	r := &networkv1.Rule{Direction: networkv1.Direction_DIRECTION_INGRESS, Protocol: networkv1.Protocol_PROTOCOL_TCP,
		Peer: &networkv1.Rule_Group{Group: model.IngressPeersGroup}}
	for _, p := range ports {
		r.Ports = append(r.Ports, &networkv1.PortRange{First: uint32(p)})
	}
	return r
}

// opens reports whether a node has the ing group of a row with exactly
// the rule for ports (none: no rules), and no members.
func (h *harness) opens(node string, id podid.ID, ports ...int) bool {
	g := h.group(node, model.IngressGroupID(id))
	if g == nil || len(g.Members) != 0 {
		return false
	}
	if len(ports) == 0 {
		return len(g.Rules) == 0
	}
	return len(g.Rules) == 1 && proto.Equal(g.Rules[0], ingRule(ports...))
}

// admitted reports whether every row has ports recorded as admitted.
func admitted(rows []store.EnvInstanceRow, ports ...int) bool {
	for _, r := range rows {
		if !slices.Equal(r.Instance.IngressPorts, ports) {
			return false
		}
	}
	return true
}

// An environment never published names no ing group and keeps its
// hashes. Its first publish rolls its image service once: the new rows
// name ing-<id>, whose rule lets ferun-ingress in on the routed port,
// ferun-ingress lists the ingress at its version, and both are recorded
// as admitted. Unpublishing empties the rule and republishing fills it,
// without a roll; a route added or removed changes the ports without new
// rows (a removed one once its release is deployed). Addons and jobs never
// name an ing group. An ingress removed leaves ferun-ingress at a new
// version.
func TestIngressGroups(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	addr := h.meshIngress("ingress-0")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, ingressSpec(2, webRoute))
	h.waitRelease(env.ID, r1, model.ReleaseDeployed, 60*time.Second)
	before := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)

	// Never published: no flag, no ing group, the hash of the template as
	// it is stored (as before the flag was), nothing put or recorded.
	for _, r := range before {
		if !namesNoIngGroup(r) || !slices.Equal(r.Sandbox.PodTemplate.SecurityGroups, []string{model.EnvGroupID(env.ID)}) {
			t.Errorf("row %s before publishing names %v", r.Sandbox.ID, r.Sandbox.PodTemplate.SecurityGroups)
		}
		if storedHash(r) != r.Instance.TemplateHash {
			t.Errorf("row %s: hash %s, its template's %s", r.Sandbox.ID, r.Instance.TemplateHash, storedHash(r))
		}
	}
	h.c.notify(bg, HintGroups)
	never(t, 2*time.Second, "an ingress group on the node before publishing", func() bool {
		return h.group("n1", model.IngressPeersGroup) != nil || h.group("n1", model.IngressGroupID(before[0].Sandbox.ID)) != nil
	})
	if !admitted(h.rowsOf(env.ID, spec.KindService, "web")) || h.nodeGroupVersion("n1") != 0 {
		t.Error("admission recorded before publishing")
	}

	// The first publish rolls web once.
	if e := h.setPublic(env.ID, true); !e.Public || !e.IngressGroup || e.HostLabel != "stage-t1" {
		t.Fatalf("published %+v", e)
	}
	var rows []store.EnvInstanceRow
	eventually(t, 60*time.Second, "web rolled to rows with their ing groups", func() bool {
		rows = h.rowsOf(env.ID, spec.KindService, "web")
		return len(rows) == 2 && ready(rows[0]) && ready(rows[1]) && hasIngGroup(rows[0]) && hasIngGroup(rows[1])
	})
	for _, r := range rows {
		if slices.Contains(ids(before), r.Sandbox.ID) {
			t.Fatalf("row %s was kept", r.Sandbox.ID)
		}
		if storedHash(r) != r.Instance.TemplateHash {
			t.Errorf("row %s: hash %s, its template's without its group %s", r.Sandbox.ID, r.Instance.TemplateHash,
				storedHash(r))
		}
		if !slices.Equal(r.Sandbox.PodTemplate.SecurityGroups,
			[]string{model.EnvGroupID(env.ID), model.IngressGroupID(r.Sandbox.ID)}) {
			t.Errorf("row %s names %v", r.Sandbox.ID, r.Sandbox.PodTemplate.SecurityGroups)
		}
	}
	version, members := h.ingressGroupVersion()
	if !slices.Equal(members, []string{addr}) || version < 1 {
		t.Fatalf("ferun-ingress in the database: %v at %d", members, version)
	}
	eventually(t, 30*time.Second, "the ing groups with their rule, ferun-ingress, the admission recorded", func() bool {
		for _, r := range rows {
			if !h.opens("n1", r.Sandbox.ID, 8080) {
				return false
			}
		}
		g := h.group("n1", model.IngressPeersGroup)
		return g != nil && g.Version == uint64(version) && slices.Equal(g.Members, []string{addr}) && len(g.Rules) == 0 &&
			admitted(h.rowsOf(env.ID, spec.KindService, "web"), 8080) && h.nodeGroupVersion("n1") == version
	})
	published := ids(rows)
	stable := func(what string) {
		t.Helper()
		never(t, 2*time.Second, what, func() bool {
			return !slices.Equal(ids(h.rowsOf(env.ID, spec.KindService, "web")), published)
		})
	}
	stable("web rolled again after its first publish")

	// Addons and jobs never name an ing group.
	if err := h.c.RunJob(bg, env.ID, "once"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the job's row", func() bool { return len(h.rowsOf(env.ID, spec.KindJob, "once")) == 1 })
	for _, r := range h.rows(env.ID) {
		if r.Instance.Kind != spec.KindService && !namesNoIngGroup(r) {
			t.Errorf("%s %s names %v", r.Instance.Kind, r.Instance.Object, r.Sandbox.PodTemplate.SecurityGroups)
		}
	}

	// Unpublish: no rules, nothing admitted, no roll; ferun-ingress,
	// which no pod's rules name now, stays on the node.
	if e := h.setPublic(env.ID, false); e.Public || !e.IngressGroup || e.HostLabel != "stage-t1" {
		t.Fatalf("unpublished %+v", e)
	}
	eventually(t, 30*time.Second, "the ing groups without rules, nothing admitted", func() bool {
		return h.opens("n1", published[0]) && h.opens("n1", published[1]) &&
			admitted(h.rowsOf(env.ID, spec.KindService, "web"))
	})
	stable("web rolled after unpublishing")
	if h.group("n1", model.IngressPeersGroup) == nil {
		t.Error("ferun-ingress was deleted")
	}

	// Republish: the rule again, no roll.
	h.setPublic(env.ID, true)
	eventually(t, 30*time.Second, "the ing groups with their rule again", func() bool {
		return h.opens("n1", published[0], 8080) && h.opens("n1", published[1], 8080) &&
			admitted(h.rowsOf(env.ID, spec.KindService, "web"), 8080)
	})
	stable("web rolled after publishing again")

	// A route added opens its port without new rows; removed, the port
	// closes once its release is deployed.
	r2 := h.apply(env.ID, ingressSpec(2, webRoute, adminRoute))
	h.waitRelease(env.ID, r2, model.ReleaseDeployed, 30*time.Second)
	eventually(t, 30*time.Second, "both routed ports open and admitted", func() bool {
		return h.opens("n1", published[0], 8080, 9090) && h.opens("n1", published[1], 8080, 9090) &&
			admitted(h.rowsOf(env.ID, spec.KindService, "web"), 8080, 9090)
	})
	stable("web rolled for a route")
	r3 := h.apply(env.ID, ingressSpec(2, webRoute))
	h.waitRelease(env.ID, r3, model.ReleaseDeployed, 30*time.Second)
	eventually(t, 30*time.Second, "the removed route's port closed", func() bool {
		return h.opens("n1", published[0], 8080) && h.opens("n1", published[1], 8080) &&
			admitted(h.rowsOf(env.ID, spec.KindService, "web"), 8080)
	})
	stable("web rolled for a route removed")

	// An ingress removed: ferun-ingress at a new version, without it.
	if found, err := h.st.RemoveIngress(bg, "ingress-0"); err != nil || !found {
		t.Fatalf("remove the ingress: %v %v", found, err)
	}
	after, members := h.ingressGroupVersion()
	if after <= version || len(members) != 0 {
		t.Fatalf("ferun-ingress after the removal: %v at %d (before %d)", members, after, version)
	}
	eventually(t, 30*time.Second, "ferun-ingress without the ingress, recorded", func() bool {
		g := h.group("n1", model.IngressPeersGroup)
		return g != nil && g.Version == uint64(after) && len(g.Members) == 0 && h.nodeGroupVersion("n1") == after
	})
}

// The ing group of an instance that is gone, and of a sandbox whose pod is
// on another node, goes from the node; that of an instance on the node
// stays.
func TestIngressGroupsStale(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	h.addNode("n2")
	h.meshIngress("ingress-0")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, webSpec(2, webRoute))
	h.waitRelease(env.ID, r1, model.ReleaseDeployed, 30*time.Second)
	h.setPublic(env.ID, true)
	var rows []store.EnvInstanceRow
	eventually(t, 60*time.Second, "web rolled to rows with their ing groups", func() bool {
		rows = h.rowsOf(env.ID, spec.KindService, "web")
		return len(rows) == 2 && ready(rows[0]) && ready(rows[1]) && hasIngGroup(rows[0]) && hasIngGroup(rows[1]) &&
			h.group(rows[0].Sandbox.Node, model.IngressGroupID(rows[0].Sandbox.ID)) != nil &&
			h.group(rows[1].Sandbox.Node, model.IngressGroupID(rows[1].Sandbox.ID)) != nil
	})
	other := func(node string) string {
		if node == "n1" {
			return "n2"
		}
		return "n1"
	}
	put := func(node, id string) {
		t.Helper()
		if _, err := networkv1.NewSecurityGroupServiceClient(h.conn(node)).PutSecurityGroup(bg,
			&networkv1.PutSecurityGroupRequest{Group: &networkv1.SecurityGroup{Id: id, Version: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	// What a node kept of a pod that moved away, and of a sandbox that
	// never was.
	keep := rows[0]
	moved := other(keep.Sandbox.Node)
	put(moved, model.IngressGroupID(keep.Sandbox.ID))
	unknown := model.IngressGroupID(podid.Mint())
	put(keep.Sandbox.Node, unknown)
	h.c.notify(bg, HintGroups)
	eventually(t, 30*time.Second, "the moved pod's and the unknown sandbox's ing groups gone", func() bool {
		return h.group(moved, model.IngressGroupID(keep.Sandbox.ID)) == nil && h.group(keep.Sandbox.Node, unknown) == nil
	})
	if !h.opens(keep.Sandbox.Node, keep.Sandbox.ID, 8080) {
		t.Error("the ing group of a row on its node went")
	}

	// A replica stopped and deleted: its ing group goes.
	gone := rows[1]
	r2 := h.apply(env.ID, webSpec(1, webRoute))
	h.waitRelease(env.ID, r2, model.ReleaseDeployed, 30*time.Second)
	eventually(t, 30*time.Second, "the stopped replica's row gone", func() bool {
		_, err := h.st.GetSandbox(bg, gone.Sandbox.ID)
		return err != nil
	})
	h.c.notify(bg, HintGroups)
	eventually(t, 30*time.Second, "the deleted instance's ing group gone", func() bool {
		return h.group(gone.Sandbox.Node, model.IngressGroupID(gone.Sandbox.ID)) == nil
	})
	if !h.opens(keep.Sandbox.Node, keep.Sandbox.ID, 8080) {
		t.Error("the ing group of the kept replica went")
	}
}
