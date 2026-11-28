// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/sandbox"
	"github.com/gardenlinux/feos/go/internal/store"
)

// An addon that a drain once moved keeps its drain's checkpoint in its
// row after it runs again. Lost with its next node past the lost grace,
// it must not come back from that checkpoint (ADR 0064, "Never from a
// checkpoint it ran past"): without backups it is lost with the node that
// has its volume (ADR 0040), and when that node returns it runs there
// again, on the data it wrote since the drain. The test node's pod
// directory is its disk, and stands for the volume's data here.
func TestAddonLostAfterDrainNotRolledBack(t *testing.T) {
	h := newHarness(t, options{engine: func(c *sandbox.Config) { c.LostGrace = time.Minute }})
	execs := &execLog{}
	n1 := h.addNode("n1")
	n1.Script().SetExec(execs.fn)
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, postgresSpec)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	first := h.waitRows(env.ID, spec.KindAddon, "postgres", 1, 10*time.Second, "ready", ready)[0]
	id := first.Sandbox.ID
	if first.Sandbox.Node != "n1" {
		t.Fatalf("on %s", first.Sandbox.Node)
	}
	write := func(dir, data string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data"), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(dir string) string {
		b, err := os.ReadFile(filepath.Join(dir, "data"))
		if err != nil {
			return "<" + err.Error() + ">"
		}
		return string(b)
	}
	write(n1.PodDir(id), "at the drain")

	// The drain moves it to n2 by hibernation: its row keeps the drain's
	// checkpoint.
	n2 := h.addNode("n2")
	n2.Script().SetExec(execs.fn)
	h.exec("UPDATE nodes SET draining = true, drain_started_at = now() WHERE name = 'n1'")
	eventually(t, 90*time.Second, "the addon on n2, its volume and pin there", func() bool {
		h.c.notify(bg, "drain")
		rows := h.rowsOf(env.ID, spec.KindAddon, "postgres")
		vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/postgres")
		return len(rows) == 1 && rows[0].Sandbox.ID == id && rows[0].Sandbox.Node == "n2" && running(rows[0]) &&
			err == nil && vol.Node == "n2" && rows[0].Sandbox.PinNode == "n2"
	})
	h.waitRows(env.ID, spec.KindAddon, "postgres", 1, 30*time.Second, "ready on n2", ready)
	moved, err := h.st.GetSandbox(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	if moved.CheckpointHead == "" {
		t.Fatalf("no checkpoint after the drain's move: %+v", moved)
	}
	t.Logf("after the drain: running on %s, checkpoint %s (epoch %d, at %s), materialised %v",
		moved.Node, moved.CheckpointHead, moved.CheckpointEpoch, moved.CheckpointAt.Format(time.RFC3339), moved.Materialised)
	if got := read(n2.PodDir(id)); got != "at the drain" {
		t.Fatalf("n2's disk after the restore: %q", got)
	}
	// It writes on n2 after the drain.
	write(n2.PodDir(id), "after the drain")

	// n2 is lost: cut off and down for longer than the lost grace, while
	// n3 has room.
	n3 := h.addNode("n3")
	n3.Script().SetExec(execs.fn)
	n2.Partition(true)
	eventually(t, 10*time.Second, "n2 disconnected", func() bool { return !h.srv.Connected("n2") })
	h.exec(`UPDATE nodes SET status = 'down', down_since = now() - interval '2 minutes',
		status_description = 'heartbeat expired' WHERE name = 'n2'`)
	var restored store.Sandbox
	rolledBack := false
	eventually(t, 90*time.Second, "the addon restored elsewhere, or lost with n2", func() bool {
		h.c.notify(bg, "down")
		h.c.notify(bg, "lost-grace")
		rows := h.rowsOf(env.ID, spec.KindAddon, "postgres")
		if len(rows) == 1 && running(rows[0]) && rows[0].Sandbox.Node != "n2" {
			restored, rolledBack = rows[0].Sandbox, true
			return true
		}
		for _, i := range h.incidents(env.ID, true) {
			if i.Object == "addon/postgres" && i.Condition == model.ConditionVolumeUnavailable {
				return true
			}
		}
		return false
	})
	if rolledBack {
		t.Errorf("silent rollback: the addon lost with n2 runs on %s as sandbox %s (same row: %v), restored from "+
			"checkpoint %s (the drain's: %v) of %s; its disk there is %q, while n2 has %q; description %q",
			restored.Node, restored.ID, restored.ID == id, restored.CheckpointHead,
			restored.CheckpointHead == moved.CheckpointHead, restored.CheckpointAt.Format(time.RFC3339),
			read(h.podDir(restored.Node, id)), read(n2.PodDir(id)), restored.StatusDescription)
		if vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/postgres"); err == nil {
			t.Logf("env_volumes: node %s holder %s", vol.Node, vol.Holder)
		}
	} else {
		rows := h.rowsOf(env.ID, spec.KindAddon, "postgres")
		t.Logf("lost with n2 (correct): %s on %s, lost_from %s: %s", rows[0].Sandbox.ClientState, rows[0].Sandbox.Node,
			rows[0].Sandbox.LostFrom, rows[0].Sandbox.StatusDescription)
	}

	// n2 comes back.
	n2.Partition(false)
	eventually(t, 30*time.Second, "n2 connected and ready", func() bool {
		nd, err := h.st.GetNode(bg, "n2")
		return h.srv.Connected("n2") && err == nil && nd.Status == model.NodeReady
	})
	if rolledBack {
		// What it wrote since the drain goes with n2's stale pod and volume.
		gone := false
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			h.c.notify(bg, LoopVolumes)
			h.c.notify(bg, "tombstone")
			if len(n2.Volumes()) == 0 {
				gone = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Errorf("after n2 returned: n2's volume records %d (deleted: %v), its pods %d", len(n2.Volumes()), gone, len(n2.Pods()))
		return
	}
	back := h.waitRows(env.ID, spec.KindAddon, "postgres", 1, 60*time.Second, "ready again on n2", ready)[0]
	if back.Sandbox.ID != id || back.Sandbox.Node != "n2" {
		t.Fatalf("after n2 returned: %+v", back.Sandbox)
	}
	if got := read(n2.PodDir(id)); got != "after the drain" {
		t.Errorf("n2's disk after it returned: %q", got)
	}
}

// podDir is a node's directory of a pod (its disk on the test node).
func (h *harness) podDir(node string, id podid.ID) string {
	h.mu.Lock()
	n := h.nodes[node]
	h.mu.Unlock()
	if n == nil {
		return "<no node " + node + ">"
	}
	return n.PodDir(id)
}

// The same addon whose node stays up but no longer has its pod (a node
// that lost its record of it): the sync's NOT_FOUND must not make it
// hibernated and restore it anywhere from the drain's checkpoint; it is
// lost on that node and replaced there on its volume (§13.7).
func TestAddonGoneAfterDrainNotRolledBack(t *testing.T) {
	h := newHarness(t, options{})
	execs := &execLog{}
	n1 := h.addNode("n1")
	n1.Script().SetExec(execs.fn)
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, postgresSpec)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	first := h.waitRows(env.ID, spec.KindAddon, "postgres", 1, 10*time.Second, "ready", ready)[0]
	id := first.Sandbox.ID
	n2 := h.addNode("n2")
	n2.Script().SetExec(execs.fn)
	h.exec("UPDATE nodes SET draining = true, drain_started_at = now() WHERE name = 'n1'")
	eventually(t, 90*time.Second, "the addon on n2, its volume and pin there", func() bool {
		h.c.notify(bg, "drain")
		rows := h.rowsOf(env.ID, spec.KindAddon, "postgres")
		vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/postgres")
		return len(rows) == 1 && rows[0].Sandbox.ID == id && rows[0].Sandbox.Node == "n2" && running(rows[0]) &&
			err == nil && vol.Node == "n2" && rows[0].Sandbox.PinNode == "n2"
	})
	h.waitRows(env.ID, spec.KindAddon, "postgres", 1, 30*time.Second, "ready on n2", ready)
	moved, err := h.st.GetSandbox(bg, id)
	if err != nil || moved.CheckpointHead == "" {
		t.Fatalf("after the drain: %+v %v", moved, err)
	}
	h.addNode("n3").Script().SetExec(execs.fn)
	// n2 loses its record of the pod; its volume stays there.
	if _, err := podv1.NewPodServiceClient(h.conn("n2")).DeletePod(bg, &podv1.DeletePodRequest{PodId: id.String()}); err != nil {
		t.Fatal(err)
	}
	var got store.EnvInstanceRow
	eventually(t, 90*time.Second, "the addon up again", func() bool {
		rows := h.rowsOf(env.ID, spec.KindAddon, "postgres")
		if len(rows) == 1 && ready(rows[0]) && (rows[0].Sandbox.ID != id || rows[0].Sandbox.Node != "n2") {
			got = rows[0]
			return true
		}
		return false
	})
	if got.Sandbox.ID == id {
		t.Errorf("silent rollback: sandbox %s came back on %s from checkpoint %s (the drain's: %v), not on its volume on n2",
			id, got.Sandbox.Node, got.Sandbox.CheckpointHead, got.Sandbox.CheckpointHead == moved.CheckpointHead)
	}
	if got.Sandbox.Node != "n2" {
		t.Errorf("up on %s, not on n2 where its volume is", got.Sandbox.Node)
	}
}
