// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// addressesOf are the addresses of rows, as a name table holds them.
func addressesOf(rows []store.EnvInstanceRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Sandbox.Address)
	}
	return addressSet(out)
}

// A service's name resolves, in each of the environment's pods, to the
// addresses of all its live replicas, and follows a roll: the env table
// on the node changes with the replicas, the pods keep their spec.
func TestServiceNamesFollowReplicas(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(2, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	v1 := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	want := addressesOf(v1)
	eventually(t, 10*time.Second, "both replicas named", func() bool {
		return resolves(n1, v1[0].Sandbox.ID, "web", want...) && resolves(n1, v1[1].Sandbox.ID, "WEB", want...)
	})
	if pod := h.pod("n1", v1[0].Sandbox.ID); !slices.Equal(pod.GetSpec().GetNetwork().GetNameTables(),
		[]string{model.EnvTableID(env.ID)}) {
		t.Fatalf("the pod's name tables %v", pod.GetSpec().GetNetwork().GetNameTables())
	}
	before := h.table("n1", model.EnvTableID(env.ID))

	// A roll: the name ends at the new replicas.
	r2 := h.apply(env.ID, serviceSpec(2, "2", ""))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	v2 := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	want = addressesOf(v2)
	eventually(t, 20*time.Second, "the new replicas named", func() bool {
		return resolves(n1, v2[0].Sandbox.ID, "web", want...)
	})
	if after := h.table("n1", model.EnvTableID(env.ID)); after == nil || after.Version == before.GetVersion() ||
		after.Version != newTable(after.Id, map[string][]string{"web": want}, nil).Version {
		t.Errorf("the env table after the roll %v (before %v)", after, before)
	}
}

// A node whose feosd predates the NameService (UNIMPLEMENTED) runs the
// environment's pods without names: the deployment is deployed, names_unserved
// opens for the objects with pods there, naming the node, and closes once
// they have none there.
func TestNamesUnserved(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1", func(cfg *testnode.Config) { cfg.NoNameService = true })
	env := h.newEnv("dev")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	rows := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)
	if _, ok := n1.Resolve(rows[0].Sandbox.ID, "web"); ok || len(n1.NameTables()) != 0 {
		t.Fatalf("names on a node without the NameService: %v", n1.NameTables())
	}
	var open store.EnvIncident
	eventually(t, 20*time.Second, "names_unserved", func() bool {
		for _, i := range h.incidents(env.ID, true) {
			if i.Condition == model.ConditionNamesUnserved && i.Object == "service/web" {
				open = i
				return true
			}
		}
		return false
	})
	if !strings.Contains(open.Summary, "node n1") {
		t.Errorf("summary %q", open.Summary)
	}

	// Without the service, nothing of it runs there.
	r2 := h.apply(env.ID, `{"services":{}}`)
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 30*time.Second)
	eventually(t, 20*time.Second, "names_unserved closed", func() bool {
		for _, i := range h.incidents(env.ID, true) {
			if i.Condition == model.ConditionNamesUnserved {
				return false
			}
		}
		return true
	})
}
