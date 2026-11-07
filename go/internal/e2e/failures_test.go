// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// failureTimings are the timings of the node failure scenarios: a node is
// down after 10 s without a report (two telemetry intervals), its
// sandboxes count as lost 2 s later.
func failureTimings(cfg *config.Config) {
	cfg.NodeTTL = 10 * time.Second
	cfg.LostGrace = 2 * time.Second
}

// incarnationOf is the incarnation label of a pod's record.
func incarnationOf(p *podv1.PodInfo) string { return p.GetSpec().GetLabels()["ferun.incarnation"] }

// waitNodeStatus waits until the node's row has status.
func (c *cluster) waitNodeStatus(name, status string, within time.Duration) {
	c.t.Helper()
	eventually(c.t, within, "node "+name+" "+status, func() bool {
		row, err := c.st.GetNode(bg, name)
		return err == nil && row.Status == status
	})
}

// downFor is how long the node has been down, on the database's clock.
func (c *cluster) downFor(name string) time.Duration {
	c.t.Helper()
	ms := c.count("SELECT (extract(epoch FROM now() - down_since) * 1000)::int FROM nodes WHERE name = $1", name)
	return time.Duration(ms) * time.Millisecond
}

// waitGone waits, waking the leader's loops, until every sandbox of ids
// has left its node: lost, or hibernated when it has a checkpoint.
func (c *cluster) waitGone(within time.Duration, ids ...podid.ID) {
	c.t.Helper()
	eventually(c.t, within, "the sandboxes past the lost grace", func() bool {
		c.wake("sandbox-down-sweep", "sandbox-lost-grace")
		for _, id := range ids {
			row := c.row(id)
			if row.Op != "" || (row.ClientState != model.StateLost && row.ClientState != model.StateHibernated) {
				return false
			}
		}
		return true
	})
}

// countCalls counts the calls of a PodService method on the node.
func countCalls(n *node, method string) *atomic.Int32 {
	var calls atomic.Int32
	n.Script().SetHook(method, func(context.Context, string, any) error {
		calls.Add(1)
		return nil
	})
	return &calls
}

// Scenario 7: a node crashes: after the node TTL it is down, its
// sandboxes unknown, and after the lost grace a sandbox with a checkpoint
// is hibernated and one without is lost; the hibernated one is restored
// elsewhere on its next call.
func TestNodeCrash(t *testing.T) {
	t.Parallel()
	c := newCluster(t, func(cfg *config.Config) {
		failureTimings(cfg)
		cfg.HibernateAfter = time.Second
	})
	c.start("a")
	c.start("b")
	n1 := c.join("n1")
	s1 := c.create(nil)
	c.hibernated(n1, s1)
	if out := c.run(nil, s1, "cat keep"); out != "data\n" {
		t.Fatalf("restored: %q", out)
	}
	s2 := c.create(nil)
	c.run(nil, s2, "true")
	for _, id := range []podid.ID{s1.id, s2.id} {
		if row := c.row(id); row.Node != "n1" || row.ClientState != model.StateRunning {
			t.Fatalf("row %+v", row)
		}
	}
	c.join("n2")

	n1.Crash()
	c.waitNodeStatus("n1", model.NodeDown, 60*time.Second)
	// Every sandbox of the node is unknown first, and leaves that for
	// lost or hibernated only once the lost grace has passed since the
	// node went down. The lost grace loop is woken only once both were
	// seen unknown.
	unknown := map[podid.ID]bool{}
	eventually(t, 90*time.Second, "the sandboxes unknown, then past the lost grace", func() bool {
		c.wake("sandbox-down-sweep")
		if len(unknown) == 2 {
			c.wake("sandbox-lost-grace")
		}
		done := true
		for _, id := range []podid.ID{s1.id, s2.id} {
			row := c.row(id)
			switch {
			case row.Op != "":
				done = false
			case row.ClientState == model.StateUnknown:
				if row.Node != "n1" {
					t.Fatalf("unknown on another node %+v", row)
				}
				unknown[id] = true
				done = false
			case row.ClientState == model.StateLost || row.ClientState == model.StateHibernated:
				if !unknown[id] {
					t.Fatalf("sandbox %s became %s without being unknown first", id, row.ClientState)
				}
				if down := c.downFor("n1"); down < c.cfg.LostGrace {
					t.Fatalf("sandbox %s became %s %v after its node went down, within the lost grace", id,
						row.ClientState, down)
				}
			default:
				done = false
			}
		}
		return done
	})
	r1, r2 := c.row(s1.id), c.row(s2.id)
	if r1.ClientState != model.StateHibernated || r1.LostFrom != "n1" || !r1.HasCheckpoint() {
		t.Errorf("the sandbox with a checkpoint %+v", r1)
	}
	if r2.ClientState != model.StateLost || r2.LostFrom != "n1" {
		t.Errorf("the sandbox without one %+v", r2)
	}
	if n := c.count("SELECT count(*) FROM bookings WHERE pod_id = ANY($1)", []podid.ID{s1.id, s2.id}); n != 0 {
		t.Errorf("%d bookings left on the crashed node", n)
	}
	if out := c.run(nil, s1, "cat keep"); out != "data\n" {
		t.Fatalf("restored elsewhere: %q", out)
	}
	if row := c.row(s1.id); row.Node != "n2" || row.ClientState != model.StateRunning {
		t.Fatalf("after the restore %+v", row)
	}
	r := c.exec(nil, s2, "true")
	if r.code != http.StatusBadGateway || !strings.Contains(r.body, "ensure sandbox: sandbox lost") {
		t.Errorf("the lost sandbox: %d %s", r.code, r.body)
	}
}

// Scenario 7: a partition past the lost grace, then the heal: the pod
// that kept running is adopted, nothing is deleted.
func TestPartitionHeal(t *testing.T) {
	t.Parallel()
	c := newCluster(t, failureTimings)
	c.start("a")
	c.start("b")
	n1 := c.join("n1", func(cfg *testnode.Config) { cfg.ObjectStore = nil })
	sb := c.create(nil)
	c.run(nil, sb, "echo data > keep")
	inc := c.row(sb.id).Incarnation
	deletes := countCalls(n1, podv1.PodService_DeletePod_FullMethodName)

	n1.Partition(true)
	c.waitNodeStatus("n1", model.NodeDown, 60*time.Second)
	c.waitGone(60*time.Second, sb.id)
	if row := c.row(sb.id); row.ClientState != model.StateLost || row.Node != "n1" || row.LostFrom != "n1" {
		t.Fatalf("lost %+v", row)
	}

	n1.Partition(false)
	c.waitNode(n1)
	eventually(t, 120*time.Second, "the live pod adopted", func() bool {
		row := c.row(sb.id)
		return row.ClientState == model.StateRunning && row.Op == "" && row.LostFrom == ""
	})
	if row := c.row(sb.id); row.Incarnation != inc || row.Node != "n1" {
		t.Fatalf("adopted %+v", row)
	}
	if n := c.count("SELECT count(*) FROM bookings WHERE pod_id = $1 AND node = 'n1'", sb.id); n != 1 {
		t.Errorf("%d bookings of the adopted pod", n)
	}
	if p, ok := n1.pod(sb.id); !ok || p.GetState() != podv1.PodState_POD_STATE_RUNNING {
		t.Fatalf("the pod %v", p)
	}
	if n := deletes.Load(); n != 0 {
		t.Fatalf("%d DeletePod calls", n)
	}
	if out := c.run(nil, sb, "cat keep"); out != "data\n" {
		t.Fatalf("the adopted disk: %q", out)
	}
}

// retryRun runs command until it succeeds, as FeCode retries a call that
// failed for capacity, and returns its output.
func (c *cluster) retryRun(sb sandbox, command string, within time.Duration) string {
	c.t.Helper()
	var out string
	eventually(c.t, within, "the call to succeed", func() bool {
		r := c.exec(nil, sb, command)
		res, err := decodeExec(r)
		if err != nil {
			if r.code != http.StatusBadGateway {
				c.t.Fatalf("exec: %v", err)
			}
			time.Sleep(500 * time.Millisecond)
			return false
		}
		out = res.Output
		return true
	})
	return out
}

// checkRecreated checks that the sandbox runs as its new incarnation on
// the node, after every tombstone is done, and stays there.
func (c *cluster) checkRecreated(n *node, sb sandbox, old uuid.UUID) {
	c.t.Helper()
	row := c.row(sb.id)
	if row.Incarnation == old || row.Node != n.name || row.ClientState != model.StateRunning {
		c.t.Fatalf("the new sandbox %+v", row)
	}
	eventually(c.t, 60*time.Second, "the tombstones done", func() bool {
		c.wake("sandbox-tombstones")
		return c.count("SELECT count(*) FROM pod_tombstones WHERE pod_id = $1", sb.id) == 0
	})
	never(c.t, 3*time.Second, "the new pod deleted", func() bool {
		p, ok := n.pod(sb.id)
		return !ok || incarnationOf(p) != row.Incarnation.String() || p.GetState() != podv1.PodState_POD_STATE_RUNNING
	})
	if out := c.run(nil, sb, "echo still"); out != "still\n" {
		c.t.Fatalf("exec %q", out)
	}
}

// Scenario 7: a sandbox lost with its partitioned node is deleted and
// created again with the same ID (as no client can since ADR 0019, but a
// drawn ID may be one used before) while the node returns: the new sandbox
// never gets the old disk, and the tombstone of the old pod never deletes
// the new one.
func TestLostDeleteRecreate(t *testing.T) {
	t.Parallel()
	c := newCluster(t, failureTimings)
	c.start("a")
	c.start("b")
	n1 := c.join("n1", func(cfg *testnode.Config) { cfg.ObjectStore = nil })
	sb := c.create(nil)
	c.run(nil, sb, "echo old > marker")
	old := c.row(sb.id).Incarnation

	n1.Partition(true)
	c.waitNodeStatus("n1", model.NodeDown, 60*time.Second)
	c.waitGone(60*time.Second, sb.id)
	r := c.call(nil, "DELETE", sb.path(""), tenantAuth, "")
	if r.code != http.StatusOK || !strings.Contains(r.body, `"clientState":"absent"`) {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if n := c.count("SELECT count(*) FROM pod_tombstones WHERE pod_id = $1 AND node = 'n1' AND incarnation = $2",
		sb.id, old); n != 1 {
		t.Fatalf("%d tombstones of the old pod", n)
	}

	n1.Partition(false)
	again := c.createID(sb.id, "box", false)
	if out := c.retryRun(again, "cat marker 2>/dev/null || echo none", 90*time.Second); out != "none\n" {
		t.Fatalf("the new sandbox sees %q", out)
	}
	c.checkRecreated(n1, again, old)
}

// Scenario 7: delete and create again at once, on a single node.
func TestDeleteRecreateSingleNode(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	c.start("a")
	c.start("b")
	n1 := c.join("n1")
	sb := c.create(nil)
	c.run(nil, sb, "echo old > marker")
	old := c.row(sb.id).Incarnation
	if r := c.call(nil, "DELETE", sb.path(""), tenantAuth, ""); r.code != http.StatusOK {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	again := c.createID(sb.id, "box", false)
	if out := c.run(nil, again, "cat marker 2>/dev/null || echo none"); out != "none\n" {
		t.Fatalf("the new sandbox sees %q", out)
	}
	c.checkRecreated(n1, again, old)
}

// ADR 0019: a sandbox made before with a UUID keeps working beside the
// short IDs, and only UUIDs share a TAP device's name: a second UUID with
// the same first 8 hex digits finds no room on the only node, a short ID
// that begins with the same 8 characters does.
func TestLegacyUUIDSandboxes(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	c.start("a")
	n1 := c.join("n1")
	legacy := c.createID(podid.ID("abcdef12-0000-4000-8000-"+uuid.NewString()[24:]), "box", false)
	short := c.createID("abcdef12xyz", "box", false)
	minted := c.create(nil)
	for _, sb := range []sandbox{legacy, short, minted} {
		if out := c.run(nil, sb, "echo hi"); out != "hi\n" {
			t.Fatalf("exec in %s: %q", sb.id, out)
		}
		if p, ok := n1.pod(sb.id); !ok || p.GetState() != podv1.PodState_POD_STATE_RUNNING {
			t.Fatalf("the pod of %s: %v %v", sb.id, p, ok)
		}
		if r := c.call(nil, "GET", sb.path(""), tenantAuth, ""); r.code != http.StatusOK ||
			!strings.Contains(r.body, `"sandboxId":"`+sb.id.String()+`"`) {
			t.Fatalf("GET %s: %d %s", sb.id, r.code, r.body)
		}
	}
	// An uppercase UUID in the path names the same sandbox.
	if r := c.call(nil, "GET", "/api/v1/sandboxes/"+strings.ToUpper(legacy.id.String()), tenantAuth, ""); r.code != http.StatusOK {
		t.Fatalf("GET of the uppercase UUID: %d %s", r.code, r.body)
	}
	twin := c.createID(podid.ID("abcdef12-1111-4000-8000-"+uuid.NewString()[24:]), "box", false)
	ctx, cancel := context.WithTimeout(bg, time.Minute)
	defer cancel()
	r, err := c.execErr(ctx, nil, twin, "true")
	if err != nil || r.code != http.StatusBadGateway || !strings.Contains(r.body, "no capacity") {
		t.Fatalf("the UUID twin: %v %d %s", err, r.code, r.body)
	}
	if row := c.row(twin.id); row.Node != "" {
		t.Fatalf("the UUID twin was placed: %+v", row)
	}
	// Deleted, the legacy sandbox frees the name.
	if r := c.call(nil, "DELETE", legacy.path(""), tenantAuth, ""); r.code != http.StatusOK {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	eventually(t, 60*time.Second, "the legacy pod deleted", func() bool {
		c.wake("sandbox-tombstones")
		_, ok := n1.pod(legacy.id)
		return !ok && c.count("SELECT count(*) FROM pod_tombstones WHERE pod_id = $1", legacy.id) == 0
	})
	if out := c.retryRun(twin, "echo twin", 90*time.Second); out != "twin\n" {
		t.Fatalf("the UUID twin after the delete: %q", out)
	}
}
