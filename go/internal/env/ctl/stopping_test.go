// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/sandbox"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// holdStops holds every StopPod of node n until release is called (or the
// call's context ends); the test's end releases it.
func holdStops(t *testing.T, n *testnode.Node) (release func()) {
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	n.Script().SetHook(podv1.PodService_StopPod_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		select {
		case <-ch:
		case <-ctx.Done():
		}
		return nil
	})
	t.Cleanup(release)
	return release
}

// stoppingRows are the environment's stopping rows of object, whose pods
// are in their stop (the op stop).
func (h *harness) stoppingRows(envID uuid.UUID, kind, name string) []store.EnvInstanceRow {
	var out []store.EnvInstanceRow
	for _, r := range h.rows(envID) {
		if r.Instance.Kind == kind && r.Instance.Object == name && r.Instance.Phase == model.PhaseStopping &&
			r.Sandbox.Op == opStop {
			out = append(out, r)
		}
	}
	return out
}

// An old replica keeps its row while its pod stops (ADR 0018): once its
// traffic drained, its pod gets the stop op, during which the row is
// running, booked and a member of its workload's group, and its name no
// longer resolves to it; the rollout of 2 replicas is deployed while both
// old pods are still in their stop grace, and their rows go as soon as
// their pods have stopped.
func TestOldReplicasKeepTheirRowsUntilTheirPodsEnded(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(2, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	v1 := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)

	release := holdStops(t, n1)
	r2 := h.apply(env.ID, serviceSpec(2, "2", ""))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	v2 := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	var old []store.EnvInstanceRow
	eventually(t, 10*time.Second, "both old pods in their stop", func() bool {
		old = h.stoppingRows(env.ID, spec.KindService, "web")
		return len(old) == 2
	})
	group := model.WorkloadGroupID(env.ID, spec.KindService, "web")
	var addrs []string
	for _, r := range old {
		if !slices.ContainsFunc(v1, func(o store.EnvInstanceRow) bool { return o.Sandbox.ID == r.Sandbox.ID }) {
			t.Fatalf("a stopping row %s is not an old replica", r.Sandbox.ID)
		}
		if r.Sandbox.ClientState != model.StateRunning || r.Sandbox.Address == "" {
			t.Fatalf("an old replica in its stop %+v", r.Sandbox)
		}
		if b, err := h.st.GetBooking(bg, r.Sandbox.ID); err != nil || b.Node != "n1" {
			t.Fatalf("an old replica in its stop is not booked: %+v %v", b, err)
		}
		addrs = append(addrs, canonical(t, r.Sandbox.Address))
	}
	for _, r := range v2 {
		addrs = append(addrs, canonical(t, r.Sandbox.Address))
	}
	slices.Sort(addrs)
	eventually(t, 15*time.Second, "the old and the new replicas members of their group", func() bool {
		g := h.group("n1", group)
		return g != nil && slices.Equal(g.GetMembers(), addrs)
	})
	var fresh []string
	for _, r := range v2 {
		fresh = append(fresh, r.Sandbox.Address)
	}
	slices.Sort(fresh)
	eventually(t, 10*time.Second, "the name with the new replicas only", func() bool {
		return resolves(n1, v2[0].Sandbox.ID, "web", fresh...)
	})
	// The status says why the row is there.
	if d := instanceDetail(t, h, env.ID, old[0].Sandbox.ID); d != "stopping: its containers have SIGTERM and 660 s to exit" {
		t.Errorf("an old replica's detail %q", d)
	}

	release()
	eventually(t, 10*time.Second, "the old rows gone once their pods stopped", func() bool {
		for _, r := range old {
			if _, err := h.st.GetSandbox(bg, r.Sandbox.ID); !errors.Is(err, model.ErrNotFound) {
				return false
			}
		}
		return true
	})
	eventually(t, 15*time.Second, "the old replicas out of their group", func() bool {
		g := h.group("n1", group)
		return g != nil && len(g.GetMembers()) == 2
	})
}

// canonical is an address in its canonical form, as a group's members
// are.
func canonical(t *testing.T, addr string) string {
	t.Helper()
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		t.Fatal(err)
	}
	return ip.String()
}

// instanceDetail is the detail the controller's row of an instance and its
// sandbox give it, as the status shows it.
func instanceDetail(t *testing.T, h *harness, envID uuid.UUID, id podid.ID) string {
	t.Helper()
	for _, r := range h.rows(envID) {
		if r.Sandbox.ID == id {
			if r.Instance.Detail != "" {
				return r.Instance.Detail
			}
			return r.Sandbox.StatusDescription
		}
	}
	t.Fatalf("no row %s", id)
	return ""
}

// A stopping instance whose pod does not stop goes once its traffic's
// drain, its stop grace and the margin have passed: the row is deleted
// while the stop still runs, and its pod goes by tombstone.
func TestStoppingInstanceGoesPastItsBound(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	v1 := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)
	release := holdStops(t, n1)
	r2 := h.apply(env.ID, serviceSpec(1, "2", ""))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	eventually(t, 10*time.Second, "the old pod in its stop", func() bool {
		return len(h.stoppingRows(env.ID, spec.KindService, "web")) == 1
	})
	never(t, time.Second, "the row went while its pod stops", func() bool {
		_, err := h.st.GetSandbox(bg, v1[0].Sandbox.ID)
		return err != nil
	})
	h.exec(`UPDATE env_instances SET retire_at = now() - make_interval(secs => $2) WHERE sandbox_id = $1`,
		v1[0].Sandbox.ID, (model.ServiceStopGrace + h.c.t.stopMargin + time.Second).Seconds())
	eventually(t, 10*time.Second, "the row gone past its bound", func() bool {
		_, err := h.st.GetSandbox(bg, v1[0].Sandbox.ID)
		return errors.Is(err, model.ErrNotFound)
	})
	release()
	eventually(t, 15*time.Second, "the old pod gone by its tombstone", func() bool {
		return h.pod("n1", v1[0].Sandbox.ID) == nil
	})
}

// endStep's decisions for a stopping instance past its traffic's drain,
// on the database's clock: a running pod is stopped, a stop in flight and
// an unknown pod on a node that is up are waited for; everything else
// goes, as does every row on a node that is down or gone, and every row
// once its drain, its grace and the margin have passed.
func TestEndStep(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	h.exec(`INSERT INTO nodes (name, status) VALUES ('down1', 'down')`)
	now, err := h.st.Now(bg)
	if err != nil {
		t.Fatal(err)
	}
	margin := h.c.t.stopMargin
	row := func(kind, state, op, node string, retiredAgo time.Duration) store.EnvInstanceRow {
		return store.EnvInstanceRow{
			Instance: store.EnvInstance{Kind: kind, Object: "x", Phase: model.PhaseStopping, RetireAt: now.Add(-retiredAgo)},
			Sandbox: store.Sandbox{ID: podid.Mint(), EnvironmentID: uuid.New(), Instance: true, ClientState: state,
				Op: op, Node: node, LogLabels: map[string]string{model.LogKind: kind, model.LogObject: "x"}},
		}
	}
	svc := spec.KindService
	for _, c := range []struct {
		name string
		r    store.EnvInstanceRow
		want endAction
	}{
		{"running", row(svc, model.StateRunning, "", "n1", time.Second), endStop},
		{"running on a node that is down", row(svc, model.StateRunning, "", "down1", time.Second), endDelete},
		{"running on a node that is gone", row(svc, model.StateRunning, "", "gone", time.Second), endDelete},
		{"in its stop", row(svc, model.StateRunning, opStop, "n1", 5*time.Minute), endWait},
		{"in its stop on a node that is down", row(svc, model.StateRunning, opStop, "down1", time.Second), endDelete},
		{"in its stop past its bound", row(svc, model.StateRunning, opStop, "n1", model.ServiceStopGrace+margin),
			endDelete},
		{"a job's in its stop", row(spec.KindJob, model.StateRunning, opStop, "n1", margin/2), endWait},
		{"a job's in its stop past its bound", row(spec.KindJob, model.StateRunning, opStop, "n1", margin), endDelete},
		{"pausing", row(svc, model.StatePausing, "pause", "n1", time.Second), endDelete},
		{"restarting", row(svc, model.StateCreating, "repair", "n1", time.Second), endDelete},
		{"stopped", row(svc, model.StateStopped, "", "n1", time.Second), endDelete},
		{"failed", row(svc, model.StateFailed, "", "n1", time.Second), endDelete},
		{"paused", row(svc, model.StatePaused, "", "n1", time.Second), endDelete},
		{"hibernated", row(svc, model.StateHibernated, "", "", time.Second), endDelete},
		{"lost", row(svc, model.StateLost, "", "n1", time.Second), endDelete},
		{"pending", row(svc, model.StatePending, "", "", time.Second), endDelete},
		{"unknown", row(svc, model.StateUnknown, "", "n1", time.Second), endWait},
		{"unknown on a node that is down", row(svc, model.StateUnknown, "", "down1", time.Second), endDelete},
		{"unknown past its bound", row(svc, model.StateUnknown, "", "n1", model.ServiceStopGrace+margin), endDelete},
	} {
		if got := h.c.endStep(bg, now, c.r); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// The controller's names of the sandboxes' op and hint are theirs.
func TestStopNames(t *testing.T) {
	if opStop != string(sandbox.OpStop) || HintReconcile != store.HintEnvReconcile {
		t.Fatalf("op %q, hint %q", opStop, HintReconcile)
	}
}

// An environment's deletion ends its instances as a rollout ends its old
// ones, without the traffic's drain: a running service's pod is stopped
// and keeps its row, booked, while it drains, and the environment goes
// once the pods have ended.
func TestEnvDeletionStopsItsPods(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	v1 := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)
	release := holdStops(t, n1)
	h.exec(`UPDATE environments SET deleting = true WHERE id = $1`, env.ID)
	h.c.notify(bg, LoopDelete)
	eventually(t, 20*time.Second, "the pod in its stop", func() bool {
		return len(h.stoppingRows(env.ID, spec.KindService, "web")) == 1
	})
	never(t, time.Second, "the environment went while its pod stops", func() bool {
		_, err := h.st.GetEnvironment(bg, env.ID)
		return err != nil
	})
	if _, err := h.st.GetBooking(bg, v1[0].Sandbox.ID); err != nil {
		t.Fatalf("a pod that stops is not booked: %v", err)
	}
	release()
	eventually(t, 60*time.Second, "the environment gone", func() bool {
		_, err := h.st.GetEnvironment(bg, env.ID)
		return errors.Is(err, store.ErrNoRow)
	})
	if info := h.pod("n1", v1[0].Sandbox.ID); info != nil {
		t.Fatalf("the pod stays: %v", info.State)
	}
}
