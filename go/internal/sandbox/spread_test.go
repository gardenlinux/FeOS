// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// replica inserts an instance row of env's image service with its
// env_instances row, as the controller does: replica of the template
// hash.
func (h *harness) replica(env uuid.UUID, service, hash string, replica int) store.Sandbox {
	h.t.Helper()
	sb := h.instance(env, plainTemplate(env), func(n *store.NewSandbox) {
		n.LogLabels = map[string]string{model.LogKind: "service", model.LogObject: service}
	})
	if _, err := h.st.InsertEnvInstance(bg, store.EnvInstance{SandboxID: sb.ID, EnvID: env, Kind: "service",
		Object: service, Replica: replica, TemplateHash: hash, Deployment: 1}); err != nil {
		h.t.Fatal(err)
	}
	return sb
}

// A service's replica goes to a node without its peers under either
// policy, where the policy alone would put it beside them: binpack to the
// fuller node, spread to the emptier one. The audit names each node's
// peers. A stopping instance (running, its pod in the op stop) is no
// peer, and a replica of a new template avoids those of its own before
// the old ones.
func TestPlacementRanksByPeers(t *testing.T) {
	for _, policy := range []string{PlacementBinpack, PlacementSpread} {
		t.Run(policy, func(t *testing.T) {
			h := newHarness(t, func(c *Config) { c.Placement = policy })
			h.addNode("n1")
			h.addNode("n2")
			e := h.eng()
			env := h.environment()
			// A large sandbox on n1: binpack prefers n1 now, spread n2.
			if big := h.running(8192); big.Node != "n1" {
				t.Fatalf("the large sandbox on %s", big.Node)
			}
			r0 := h.replica(env, "web", "v1", 0)
			a := h.ensure(r0.ID).Node
			b := map[string]string{"n1": "n2", "n2": "n1"}[a]
			r1 := h.replica(env, "web", "v1", 1)
			if node := h.ensure(r1.ID).Node; node != b {
				t.Fatalf("the second replica went to %s beside the first", node)
			}
			d := h.activityNamed(r1.ID, activity.KindPlacement, fmt.Sprintf("placed on %s (create)", b))
			peers := map[string]float64{}
			cands, _ := d["candidates"].([]any)
			for _, c := range cands {
				m, _ := c.(map[string]any)
				n, _ := m["instances"].(float64)
				peers[m["node"].(string)] = n
			}
			if peers[a] != 1 || peers[b] != 0 {
				t.Fatalf("the audit's peers %v (%v)", peers, d)
			}

			// r1 goes: its pod stops (held at its node) while its row runs.
			h.exec(`UPDATE env_instances SET phase = 'stopping', retire_at = now() WHERE sandbox_id = $1`, r1.ID)
			stop := h.watchCalls(b, podv1.PodService_StopPod_FullMethodName, true)
			if err := e.StopPod(bg, r1.ID); err != nil {
				t.Fatal(err)
			}
			waitClosed(t, stop.arrived, "the stop's StopPod")
			if row := h.row(r1.ID); row.Op != string(OpStop) || row.ClientState != model.StateRunning {
				t.Fatalf("the stopping row %s/%s", row.ClientState, row.Op)
			}
			r2 := h.replica(env, "web", "v1", 2)
			p, err := e.place(bg, h.row(r2.ID), OpCreate)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.candidates) != 2 || p.candidates[0] != b {
				t.Fatalf("candidates %v beside a stopping instance on %s (%+v)", p.candidates, b, p.verdicts)
			}

			// A new template: r3 avoids r0, an old one; r4 then avoids r3,
			// its own, before r0.
			r3 := h.replica(env, "web", "v2", 0)
			if node := h.ensure(r3.ID).Node; node != b {
				t.Fatalf("the new template's first replica on %s beside an old one", node)
			}
			r4 := h.replica(env, "web", "v2", 1)
			if p, err = e.place(bg, h.row(r4.ID), OpCreate); err != nil || p.candidates[0] != a {
				t.Fatalf("candidates %v (%+v): %v", p.candidates, p.verdicts, err)
			}
		})
	}
}

// placeTogether inserts two replicas each of services, places them all
// from snapshots taken before any claims (the begins wait until all
// arrived), over two instances, and returns each service's nodes.
func placeTogether(t *testing.T, h *harness, services int) [][2]string {
	t.Helper()
	env := h.environment()
	var rows []store.Sandbox
	for s := range services {
		for i := range 2 {
			rows = append(rows, h.replica(env, fmt.Sprintf("web%d", s), "v1", i))
		}
	}
	var mu sync.Mutex
	seen := map[podid.ID]bool{}
	all := make(chan struct{})
	for _, in := range h.ins {
		in.eng.preBeginHook = func(_ context.Context, id podid.ID, kind OpKind) {
			if kind != OpCreate {
				return
			}
			mu.Lock()
			if !seen[id] {
				seen[id] = true
				if len(seen) == len(rows) {
					close(all)
				}
			}
			mu.Unlock()
			select {
			case <-all:
			case <-time.After(20 * time.Second):
			}
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, len(rows))
	for i, sb := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(bg, 60*time.Second)
			defer cancel()
			_, errs[i] = h.ins[i%2].eng.Ensure(ctx, sb.ID)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d: %v", i, err)
		}
	}
	out := make([][2]string, services)
	for s := range services {
		out[s] = [2]string{h.row(rows[2*s].ID).Node, h.row(rows[2*s+1].ID).Node}
	}
	return out
}

// Replicas placed at once from one snapshot rank the same node first; the
// claim finds a peer that came since, and the begin takes the next node,
// under either policy. On one node they go side by side: the node is
// claimed again without the check.
func TestReplicasPlacedTogether(t *testing.T) {
	for _, policy := range []string{PlacementBinpack, PlacementSpread} {
		t.Run(policy, func(t *testing.T) {
			h := newHarness(t, func(c *Config) { c.Placement = policy })
			h.addInstance("i2", func(c *Config) { c.Placement = policy })
			for _, n := range []string{"n1", "n2", "n3"} {
				h.addNode(n)
			}
			for s, nodes := range placeTogether(t, h, 3) {
				if nodes[0] == "" || nodes[0] == nodes[1] {
					t.Errorf("service web%d: both replicas on %q", s, nodes[0])
				}
			}
		})
	}
	t.Run("one node", func(t *testing.T) {
		h := newHarness(t)
		h.addInstance("i2")
		h.addNode("n1")
		for s, nodes := range placeTogether(t, h, 2) {
			if nodes != [2]string{"n1", "n1"} {
				t.Errorf("service web%d on %v", s, nodes)
			}
		}
	})
}
