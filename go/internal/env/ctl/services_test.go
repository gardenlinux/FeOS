// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/env/envdeps"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/ports"
	"github.com/gardenlinux/feos/go/internal/sandbox"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// serviceSpec is a spec with one image service.
func serviceSpec(replicas int, version string, extra string) string {
	return fmt.Sprintf(`{"services":{"web":{"image":%q,"command":["/app"],"replicas":%d,
		"env":{"VERSION":%q},"ports":[{"name":"http","port":8080}]%s}}}`, testImage, replicas, version, extra)
}

// An image service rolls to a new template one replica at a time, a new
// one first (surge 1): never more than one replica above its count, the
// old ones stop only once the new are ready; a new count adds or
// stops replicas (the highest first) and keeps the others.
func TestImageServiceRollingAndScale(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(2, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	v1 := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	if v1[0].Instance.Replica != 0 || v1[1].Instance.Replica != 1 || v1[0].Sandbox.IdleAfterS == nil ||
		*v1[0].Sandbox.IdleAfterS != 0 || v1[0].Sandbox.MemoryMiB != 512 || v1[0].Sandbox.VCPUs != 1 {
		t.Fatalf("v1 rows %+v", v1)
	}
	// The controller mints its replicas' IDs short (ADR 0019).
	for _, r := range v1 {
		if id := r.Sandbox.ID; !podid.Valid(id.String()) || id.IsLegacy() || r.Instance.SandboxID != id {
			t.Fatalf("replica %d has the ID %q", r.Instance.Replica, id)
		}
	}
	oldHash := v1[0].Instance.TemplateHash

	// The rolling: watch the rows while it goes.
	var mu sync.Mutex
	maxRows, sawStopping := 0, false
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			rows, err := h.st.ListEnvInstanceRows(bg, env.ID)
			if err != nil {
				continue
			}
			live, newReady := 0, 0
			stoppingOld := false
			for _, r := range rows {
				if r.Instance.Phase == model.PhaseStopping {
					stoppingOld = true
					continue
				}
				live++
				if r.Instance.TemplateHash != oldHash && r.Instance.Phase == model.PhaseReady {
					newReady++
				}
			}
			mu.Lock()
			maxRows = max(maxRows, live)
			if stoppingOld {
				sawStopping = true
				if newReady == 0 {
					t.Errorf("an old replica stopped before a new one was ready")
				}
			}
			mu.Unlock()
		}
	}()
	r2 := h.apply(env.ID, serviceSpec(2, "2", ""))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	close(stop)
	<-watched
	mu.Lock()
	if maxRows > 3 || !sawStopping {
		t.Errorf("at most %d replicas at once (want 3), stopping seen %v", maxRows, sawStopping)
	}
	mu.Unlock()
	v2 := h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	for _, r := range v2 {
		if r.Instance.TemplateHash == oldHash || slices.ContainsFunc(v1, func(o store.EnvInstanceRow) bool { return o.Sandbox.ID == r.Sandbox.ID }) {
			t.Fatalf("an old replica after the rolling: %+v", r)
		}
		if r.Sandbox.PodTemplate.Env["VERSION"].Value != "2" {
			t.Errorf("template %+v", r.Sandbox.PodTemplate)
		}
	}
	eventually(t, 10*time.Second, "the old rows gone", func() bool { return len(h.rows(env.ID)) == 2 })
	for _, o := range v1 {
		if _, err := h.st.GetSandbox(bg, o.Sandbox.ID); err == nil {
			t.Errorf("old row %s stays", o.Sandbox.ID)
		}
	}

	// Scale up: the kept replicas stay, one is added.
	r3 := h.apply(env.ID, serviceSpec(3, "2", ""))
	h.waitDeployment(env.ID, r3, model.DeploymentDeployed, 30*time.Second)
	v3 := h.waitRows(env.ID, spec.KindService, "web", 3, 10*time.Second, "ready", ready)
	kept := 0
	for _, r := range v3 {
		if slices.ContainsFunc(v2, func(o store.EnvInstanceRow) bool { return o.Sandbox.ID == r.Sandbox.ID }) {
			kept++
			if r.Instance.Deployment != r3 {
				t.Errorf("a kept replica's deployment is %d", r.Instance.Deployment)
			}
		}
	}
	if kept != 2 || v3[2].Instance.Replica != 2 {
		t.Fatalf("after the scale up: %d kept, %+v", kept, v3)
	}

	// Scale down: the highest replicas stop.
	r4 := h.apply(env.ID, serviceSpec(1, "2", `,"idle":"15m"`))
	h.waitDeployment(env.ID, r4, model.DeploymentDeployed, 30*time.Second)
	v4 := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)
	if v4[0].Instance.Replica != 0 || v4[0].Sandbox.ID != v3[0].Sandbox.ID {
		t.Fatalf("after the scale down %+v", v4)
	}
	// idle is not in the template: kept, with its idle_after_s.
	if v4[0].Sandbox.IdleAfterS == nil || *v4[0].Sandbox.IdleAfterS != 900 {
		t.Errorf("idle_after_s %v", v4[0].Sandbox.IdleAfterS)
	}
	eventually(t, 10*time.Second, "the stopped rows gone", func() bool { return len(h.rows(env.ID)) == 1 })

	// Healing: a stopped replica comes back.
	if err := h.st.InTx(bg, func(tx *store.Store) error {
		_, err := tx.Q().Exec(bg, `UPDATE sandboxes SET client_state = 'stopped' WHERE id = $1`, v4[0].Sandbox.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.waitRows(env.ID, spec.KindService, "web", 1, 30*time.Second, "running again", running)
}

// A container that exits is restarted with a growing backoff, and 3 exits
// within the window open crash_loop with the exits and the log tail; a
// new instance of a deploying deployment that exits 3 times fails the deployment
// (rollout_failed), stops the new instances and keeps the old serving.
func TestCrashLoopAndRolloutFailure(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	row := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0]
	id := row.Sandbox.ID
	var gaps []time.Duration
	for i := 1; i <= 3; i++ {
		_ = n1.Script().AppendLog(id, "main", fmt.Sprintf("crash %d", i))
		eventually(t, 30*time.Second, "the container running", func() bool {
			p := h.pod("n1", id)
			return p != nil && len(p.Containers) == 1 && p.Containers[0].State.String() == "CONTAINER_STATE_RUNNING" &&
				running(h.rowsOf(env.ID, spec.KindService, "web")[0])
		})
		if err := n1.Script().ExitContainer(id, "main", 1); err != nil {
			t.Fatal(err)
		}
		var inst store.EnvInstance
		eventually(t, 10*time.Second, fmt.Sprintf("exit %d recorded", i), func() bool {
			inst, _ = h.st.GetEnvInstance(bg, id)
			return inst.Restarts == i && inst.Phase == model.PhaseExited
		})
		last := inst.Exits[len(inst.Exits)-1]
		if last.Code == nil || *last.Code != 1 || inst.LastExit == nil || *inst.LastExit != 1 ||
			!strings.HasPrefix(inst.Detail, "exited with code 1; restarting in ") {
			t.Fatalf("exit %+v", inst)
		}
		if !inst.NextRestartAt.IsZero() {
			gaps = append(gaps, inst.NextRestartAt.Sub(last.At))
		}
	}
	if len(gaps) != 3 || gaps[0] != 200*time.Millisecond || gaps[1] != 400*time.Millisecond || gaps[2] != 800*time.Millisecond {
		t.Errorf("backoffs %v", gaps)
	}
	inc := h.incident(env.ID, "service/web", model.ConditionCrashLoop, 10*time.Second)
	var ev evidence
	if err := json.Unmarshal(inc.Evidence, &ev); err != nil || len(ev.Exits) != 3 || len(ev.LogTail) != 3 ||
		inc.Deployment != r1 || inc.Summary != "web (deployment 1) exited 3 times within 10 minutes, last with code 1" {
		t.Fatalf("incident %+v %+v", inc, ev)
	}
	// Restarted once more, it is ready again.
	h.waitRows(env.ID, spec.KindService, "web", 1, 30*time.Second, "ready again", ready)

	// A deployment whose new instance crashes fails; the old one serves on.
	r2 := h.apply(env.ID, serviceSpec(1, "2", ""))
	var newID podid.ID
	for i := 1; i <= 3; i++ {
		eventually(t, 30*time.Second, "the new instance running", func() bool {
			for _, r := range h.rowsOf(env.ID, spec.KindService, "web") {
				if r.Sandbox.ID == id || !running(r) {
					continue
				}
				p := h.pod("n1", r.Sandbox.ID)
				if p != nil && len(p.Containers) == 1 && p.Containers[0].State.String() == "CONTAINER_STATE_RUNNING" &&
					r.Instance.Phase != model.PhaseExited {
					newID = r.Sandbox.ID
					return true
				}
			}
			return false
		})
		if err := n1.Script().ExitContainer(newID, "main", 2); err != nil {
			t.Fatal(err)
		}
		eventually(t, 10*time.Second, "the new instance's exit", func() bool {
			inst, err := h.st.GetEnvInstance(bg, newID)
			return err == nil && len(inst.Exits) == i
		})
	}
	rel := h.waitDeployment(env.ID, r2, model.DeploymentFailed, 30*time.Second)
	if !strings.Contains(rel.Reason, "exited 3 times") {
		t.Errorf("reason %q", rel.Reason)
	}
	inc = h.incident(env.ID, "service/web", model.ConditionRolloutFailed, 10*time.Second)
	if err := json.Unmarshal(inc.Evidence, &ev); err != nil || len(ev.Exits) != 3 || inc.Deployment != r2 {
		t.Fatalf("rollout incident %+v %+v", inc, ev)
	}
	eventually(t, 30*time.Second, "the new instance gone, the old one serving", func() bool {
		rows := h.rows(env.ID)
		return len(rows) == 1 && rows[0].Sandbox.ID == id && ready(rows[0])
	})
	if e, _ := h.st.GetEnvironment(bg, env.ID); e.Deployed != r1 {
		t.Errorf("deployed %d", e.Deployed)
	}
}

// Readiness checks move an instance's phase: http (through the port
// door to the pod's port), tcp and exec; starting → ready on the first
// success, ready → unready after failureThreshold failures, back on a
// success; the initial delay counts.
func TestReadinessPhases(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	var status atomic.Int32
	status.Store(503)
	var hosts sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts.Store(r.Host, r.URL.Path)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", `,"readiness":{"http":{"port":"http","path":"/readyz"},
		"period":"1s","failureThreshold":2,"initialDelay":"1s"}`))
	row := h.waitRows(env.ID, spec.KindService, "web", 1, 30*time.Second, "running", running)[0]
	n1.Script().SetPodPortAddr(row.Sandbox.ID, 8080, strings.TrimPrefix(srv.URL, "http://"))
	eventually(t, 30*time.Second, "a failing check's detail", func() bool {
		inst, _ := h.st.GetEnvInstance(bg, row.Sandbox.ID)
		return inst.Phase == model.PhaseStarting && strings.Contains(inst.Detail, "answered 503")
	})
	if rel := h.deployment(env.ID, r1); rel.State != model.DeploymentDeploying {
		t.Errorf("deployment %s while its instance is starting", rel.State)
	}
	status.Store(200)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	if p, ok := hosts.Load("localhost:8080"); !ok || p != "/readyz" {
		t.Error("the probe did not ask for localhost:8080/readyz")
	}
	status.Store(500)
	eventually(t, 30*time.Second, "unready", func() bool {
		inst, _ := h.st.GetEnvInstance(bg, row.Sandbox.ID)
		return inst.Phase == model.PhaseUnready && inst.ProbeFailures >= 2 && inst.ReadySince.IsZero()
	})
	status.Store(204)
	eventually(t, 30*time.Second, "ready again", func() bool {
		inst, _ := h.st.GetEnvInstance(bg, row.Sandbox.ID)
		return inst.Phase == model.PhaseReady && inst.ProbeFailures == 0 && !inst.ReadySince.IsZero()
	})

	// tcp: nothing listens → starting; a listener → ready.
	r2 := h.apply(env.ID, serviceSpec(1, "2", `,"readiness":{"tcp":{"port":8080},"period":"1s"}`))
	var tcpRow store.EnvInstanceRow
	eventually(t, 30*time.Second, "the tcp instance", func() bool {
		for _, r := range h.rowsOf(env.ID, spec.KindService, "web") {
			if r.Sandbox.ID != row.Sandbox.ID && running(r) {
				tcpRow = r
				return true
			}
		}
		return false
	})
	n1.Script().SetPodPortAddr(tcpRow.Sandbox.ID, 8080, "127.0.0.1:1")
	eventually(t, 30*time.Second, "the tcp check failing", func() bool {
		inst, _ := h.st.GetEnvInstance(bg, tcpRow.Sandbox.ID)
		return strings.Contains(inst.Detail, "tcp: ")
	})
	n1.Script().SetPodPortAddr(tcpRow.Sandbox.ID, 8080, strings.TrimPrefix(srv.URL, "http://"))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 30*time.Second)

	// exec: exit 1, then 0.
	var code atomic.Int32
	code.Store(1)
	n1.Script().SetExec(func(_ context.Context, _ podid.ID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		if len(req.Command) > 0 && req.Command[0] == "check" {
			return code.Load(), true, nil
		}
		return 0, false, nil
	})
	r3 := h.apply(env.ID, serviceSpec(1, "3", `,"readiness":{"exec":{"command":["check","--deep"]},"period":"1s"}`))
	eventually(t, 30*time.Second, "the exec check failing", func() bool {
		for _, r := range h.rowsOf(env.ID, spec.KindService, "web") {
			if strings.Contains(r.Instance.Detail, "check exited 1") {
				return true
			}
		}
		return false
	})
	code.Store(0)
	h.waitDeployment(env.ID, r3, model.DeploymentDeployed, 30*time.Second)
}

// A rollout counts only live instances (§13.7): a new instance whose
// phase says ready while its row is unknown (its node out of touch, its
// syncs failing) is not ready, so the old replica keeps serving and R
// is not deployed; once the new one runs and passes its check again,
// the rollout goes on.
func TestRolloutCountsLiveInstances(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	var code atomic.Int32
	code.Store(1)
	n1.Script().SetExec(func(_ context.Context, _ podid.ID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		if len(req.Command) > 0 && req.Command[0] == "check" {
			return code.Load(), true, nil
		}
		return 0, false, nil
	})
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	old := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0]
	r2 := h.apply(env.ID, serviceSpec(1, "2", `,"readiness":{"exec":{"command":["check"]},"period":"1s"}`))
	var fresh store.EnvInstanceRow
	eventually(t, 30*time.Second, "the new instance running, its check failing", func() bool {
		for _, r := range h.rowsOf(env.ID, spec.KindService, "web") {
			if r.Sandbox.ID != old.Sandbox.ID && running(r) && strings.Contains(r.Instance.Detail, "check exited 1") {
				fresh = r
				return true
			}
		}
		return false
	})
	// Its node falls out of touch with it: every sync fails, the row is
	// unknown; then its phase says ready (as it would have, had its check
	// passed just before).
	n1.Script().SetHook(podv1.PodService_GetPod_FullMethodName, func(_ context.Context, _ string, req any) error {
		if r, ok := req.(*podv1.GetPodRequest); ok && r.GetPodId() == fresh.Sandbox.ID.String() {
			return status.Error(codes.Unavailable, "out of touch")
		}
		return nil
	})
	h.exec(`UPDATE sandboxes SET client_state = 'unknown' WHERE id = $1`, fresh.Sandbox.ID)
	time.Sleep(1500 * time.Millisecond) // a probe of it that began before ends
	h.exec(`UPDATE env_instances SET phase = 'ready', ready_since = now(), probe_failures = 0, detail = ''
		WHERE sandbox_id = $1`, fresh.Sandbox.ID)
	never(t, 4*time.Second, "the old replica stopped while the new one is unknown", func() bool {
		for _, r := range h.rows(env.ID) {
			if r.Sandbox.ID == old.Sandbox.ID {
				return r.Instance.Phase == model.PhaseStopping
			}
		}
		return true
	})
	if rel := h.deployment(env.ID, r2); rel.State != model.DeploymentDeploying {
		t.Fatalf("deployment %s while its new instance is unknown", rel.State)
	}
	if sb, _ := h.st.GetSandbox(bg, fresh.Sandbox.ID); sb.ClientState != model.StateUnknown {
		t.Fatalf("the new instance is %s, not unknown: the test did not hold it", sb.ClientState)
	}
	// Back in touch, and its check passes: the rollout goes on.
	n1.Script().SetHook(podv1.PodService_GetPod_FullMethodName, nil)
	code.Store(0)
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	if rows := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready); rows[0].Sandbox.ID != fresh.Sandbox.ID {
		t.Errorf("web after the rollout %+v", rows[0].Sandbox)
	}
}

// RestartService waits for a restarted replica to be live before the
// next, as a rollout counts (§13.7): a ready phase on a row that does not
// run counts only for a service that idles, and then only while the row
// is paused or hibernated without an op.
func TestRestartWaitsForALiveReplica(t *testing.T) {
	h := newHarness(t, options{timings: func(t *timings) { t.restartReady = 300 * time.Millisecond }})
	// An environment without a deployment, whose rows the controller leaves.
	env := h.newEnv("stage")
	// A row made before ADR 0019, with a UUID.
	id := podid.ID(uuid.NewString())
	idle := 0
	if _, _, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: id, Owner: testTenant, Image: testImage, VCPUs: 1,
		MemoryMiB: 512, EnvironmentID: env.ID, Instance: true, PodTemplate: &model.PodTemplate{
			SecurityGroups: []string{model.EnvGroupID(env.ID)}}, IdleAfterS: &idle}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.InsertEnvInstance(bg, store.EnvInstance{SandboxID: id, EnvID: env.ID, Kind: spec.KindService,
		Object: "web", TemplateHash: "h", Deployment: 1, Phase: model.PhaseReady}); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE sandboxes SET client_state = 'hibernated' WHERE id = $1`, id)
	if h.c.waitReady(bg, id, false) {
		t.Error("a hibernated replica of a service that does not idle counted as ready")
	}
	if !h.c.waitReady(bg, id, true) {
		t.Error("a hibernated replica of a service that idles did not count as ready")
	}
	h.exec(`UPDATE sandboxes SET client_state = 'unknown' WHERE id = $1`, id)
	if h.c.waitReady(bg, id, true) {
		t.Error("an unknown replica counted as ready")
	}
}

// An unknown row is ensured only when its service does not idle (§13.7):
// an idle service's replica may be paused in truth, and the engine finds
// out without waking it; another service's unknown replica comes back
// running.
func TestUnknownIdleNotWoken(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"web":{"image":%q},"idler":{"image":%q,"idle":"15m"}}}`,
		testImage, testImage))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	web := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0]
	idler := h.waitRows(env.ID, spec.KindService, "idler", 1, 10*time.Second, "ready", ready)[0]
	if err := h.eng.Pause(bg, idler.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "paused", func() bool {
		sb, _ := h.st.GetSandbox(bg, idler.Sandbox.ID)
		return sb.ClientState == model.StatePaused && sb.Op == ""
	})
	// Both rows unknown, as after an op whose outcome was lost; the idle
	// one's node does not answer for it yet (its syncs fail), so that the
	// engine does not settle it before a step sees it.
	time.Sleep(time.Second) // the pause's own syncs land first
	n1.Script().SetHook(podv1.PodService_GetPod_FullMethodName, func(_ context.Context, _ string, req any) error {
		if r, ok := req.(*podv1.GetPodRequest); ok && r.GetPodId() == idler.Sandbox.ID.String() {
			return status.Error(codes.Unavailable, "out of touch")
		}
		return nil
	})
	h.exec(`UPDATE sandboxes SET client_state = 'unknown' WHERE id = ANY($1)`,
		[]podid.ID{web.Sandbox.ID, idler.Sandbox.ID})
	h.waitRows(env.ID, spec.KindService, "web", 1, 30*time.Second, "running again", running)
	never(t, 3*time.Second, "the idle replica was ensured", func() bool {
		sb, _ := h.st.GetSandbox(bg, idler.Sandbox.ID)
		return sb.ClientState != model.StateUnknown || sb.Op != "" || h.c.pool.busy(ensureKey(idler.Sandbox.ID))
	})
	// The engine's sync (its unknown sweep) finds it paused.
	n1.Script().SetHook(podv1.PodService_GetPod_FullMethodName, nil)
	if err := h.eng.Sync(bg, idler.Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	if sb, _ := h.st.GetSandbox(bg, idler.Sandbox.ID); sb.ClientState != model.StatePaused {
		t.Errorf("the idle replica after a sync: %s", sb.ClientState)
	}
	never(t, 3*time.Second, "the paused idle replica was woken", func() bool {
		sb, _ := h.st.GetSandbox(bg, idler.Sandbox.ID)
		return sb.ClientState != model.StatePaused
	})
}

// crash makes an instance's container exit n times (each once the
// container runs again), its restarts counting from restarts.
func crash(t *testing.T, h *harness, n *testnode.Node, envID uuid.UUID, id podid.ID, restarts, times int) {
	t.Helper()
	for i := 1; i <= times; i++ {
		eventually(t, 30*time.Second, "the container running", func() bool {
			p := h.pod("n1", id)
			inst, err := h.st.GetEnvInstance(bg, id)
			return p != nil && len(p.Containers) == 1 && p.Containers[0].State.String() == "CONTAINER_STATE_RUNNING" &&
				err == nil && inst.Phase != model.PhaseExited
		})
		if err := n.Script().ExitContainer(id, "main", 1); err != nil {
			t.Fatal(err)
		}
		eventually(t, 10*time.Second, fmt.Sprintf("exit %d recorded", i), func() bool {
			inst, _ := h.st.GetEnvInstance(bg, id)
			return inst.Restarts == restarts+i && inst.Phase == model.PhaseExited
		})
	}
}

// A crash loop's incident closes once the object has been ready for the
// crash window, and a later crash loop of the same deployment opens a new
// one.
func TestCrashLoopCloses(t *testing.T) {
	h := newHarness(t, options{timings: func(t *timings) { t.crashWindow = 12 * time.Second }})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	id := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0].Sandbox.ID
	crash(t, h, n1, env.ID, id, 0, 3)
	first := h.incident(env.ID, "service/web", model.ConditionCrashLoop, 10*time.Second)
	eventually(t, 60*time.Second, "the crash loop closed", func() bool {
		for _, i := range h.incidents(env.ID, false) {
			if i.ID == first.ID {
				return !i.ClosedAt.IsZero()
			}
		}
		return false
	})
	inst, _ := h.st.GetEnvInstance(bg, id)
	if inst.Restarts != 0 {
		t.Errorf("restarts %d after the crash window", inst.Restarts)
	}
	crash(t, h, n1, env.ID, id, 0, 3)
	second := h.incident(env.ID, "service/web", model.ConditionCrashLoop, 10*time.Second)
	if second.ID == first.ID || second.Deployment != r1 {
		t.Fatalf("the second crash loop %+v, the first %+v", second, first)
	}
}

// Incidents that nothing else closes: crash_loop, volume_unavailable and
// addon_unhealthy of objects that neither T nor R has close; a crash
// loop of an older deployment closes once a later deployment is deployed.
func TestIncidentsClose(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	open := func(object, condition string) int64 {
		i, _, err := h.st.OpenIncident(bg, store.EnvIncident{EnvID: env.ID, Object: object, Deployment: r1, Condition: condition,
			Summary: "test", Evidence: []byte("{}")})
		if err != nil {
			t.Fatal(err)
		}
		return i.ID
	}
	gone := []int64{open("addon/cache", model.ConditionVolumeUnavailable), open("service/old", model.ConditionCrashLoop),
		open("addon/cache", model.ConditionAddonUnhealthy)}
	stays := open("service/web", model.ConditionCrashLoop)
	closed := func(id int64) bool {
		for _, i := range h.incidents(env.ID, false) {
			if i.ID == id {
				return !i.ClosedAt.IsZero()
			}
		}
		return false
	}
	eventually(t, 30*time.Second, "the removed objects' incidents closed", func() bool {
		h.c.notify(bg, HintReconcile)
		return closed(gone[0]) && closed(gone[1]) && closed(gone[2])
	})
	if closed(stays) {
		t.Fatal("the crash loop of an object T has closed")
	}
	r2 := h.apply(env.ID, serviceSpec(1, "2", ""))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 30*time.Second)
	if !closed(stays) {
		t.Error("the crash loop of deployment 1 stays open after deployment 2 was deployed")
	}
}

// A restart that did not happen (its repair's prepare hook failed: the
// pod runs on with the same exited container) is tried again without
// counting another exit: one real exit stays one exit, and no crash loop
// opens.
func TestRestartNotDoneIsNoExit(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	id := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0].Sandbox.ID
	var fail atomic.Int32
	fail.Store(2)
	h.eng.SetPrepareHook(func(ctx context.Context, node string, sb store.Sandbox, kind sandbox.OpKind) error {
		if kind == sandbox.OpRepair && sb.ID == id && fail.Load() > 0 {
			fail.Add(-1)
			return errors.New("the node's groups cannot be put right now")
		}
		return h.c.PrepareHook(ctx, node, sb, kind)
	})
	crash(t, h, n1, env.ID, id, 0, 1)
	eventually(t, 30*time.Second, "two restarts not done", func() bool { return fail.Load() == 0 })
	h.waitRows(env.ID, spec.KindService, "web", 1, 30*time.Second, "ready after the third restart", ready)
	inst, err := h.st.GetEnvInstance(bg, id)
	if err != nil || inst.Restarts != 1 || len(inst.Exits) != 1 {
		t.Fatalf("instance %+v %v", inst, err)
	}
	for _, i := range h.incidents(env.ID, false) {
		if i.Condition == model.ConditionCrashLoop {
			t.Errorf("a crash loop opened: %+v", i)
		}
	}
}

// A deploying deployment that waits for an addon that never provisions fails
// at its deadline (rollout_failed, naming the addon, with what it waited
// for and the instance's detail, credentials redacted), and the old
// instance serves on. The deadline is the longest of the services'
// budgets (5s here) and the floor (10s here, 10 minutes by default).
func TestDeploymentDeadline(t *testing.T) {
	h := newHarness(t, options{timings: func(t *timings) { t.deploymentDeadline = 10 * time.Second }})
	n1 := h.addNode("n1")
	var code atomic.Int32
	code.Store(1)
	failingProvisioning(n1, &code)
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	old := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0]
	r2 := h.apply(env.ID, fmt.Sprintf(`{"addons":{"postgres":{"type":"postgres","version":"17","plan":"dev",
		"options":{"databases":["app"]}}},"services":{"web":{"image":%q,"command":["/app"],
		"env":{"DB":{"addon":"postgres","key":"url"}},"ports":[{"name":"http","port":8080}],"rollout":{"timeout":"5s"}}}}`,
		testImage))
	rel := h.waitDeployment(env.ID, r2, model.DeploymentFailed, 60*time.Second)
	if !strings.HasPrefix(rel.Reason, "deployment 2 did not roll out within 10s: addon postgres") {
		t.Errorf("reason %q", rel.Reason)
	}
	inc := h.incident(env.ID, "addon/postgres", model.ConditionRolloutFailed, 10*time.Second)
	var ev evidence
	if err := json.Unmarshal(inc.Evidence, &ev); err != nil || inc.Deployment != r2 {
		t.Fatalf("incident %+v %v", inc, err)
	}
	pw, _ := h.c.ResolveSecret(bg, env.ID, "addon.postgres.password", 1)
	all := strings.Join(ev.Pending, "\n")
	if !strings.Contains(all, "addon/postgres replica 0: provisioning step provision exited 1") ||
		!strings.Contains(all, "service web waits for the addons") || pw == "" || strings.Contains(all, pw) {
		t.Errorf("pending %q", ev.Pending)
	}
	if rows := h.rowsOf(env.ID, spec.KindService, "web"); len(rows) != 1 || rows[0].Sandbox.ID != old.Sandbox.ID || !ready(rows[0]) {
		t.Errorf("web after the failed deployment %+v", rows)
	}
}

// A scale-up counts as a rollout: replicas that cannot place fail the
// deployment at the service's budget, and the replicas it kept serve on. The
// first deployment keeps the default budget, so that its replica does not
// have to be ready within the scale-up's 3 s; the budget is no part of
// the template, so the scale-up keeps that replica.
func TestScaleUpBudget(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1", func(c *testnode.Config) { c.MemoryBytes = 8 << 30 })
	env := h.newEnv("stage")
	big := func(replicas int, extra string) string {
		return serviceSpec(replicas, "1", `,"resources":{"memoryMib":6144}`+extra)
	}
	r1 := h.apply(env.ID, big(1, ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	kept := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0]
	r2 := h.apply(env.ID, big(2, `,"rollout":{"timeout":"3s"}`))
	rel := h.waitDeployment(env.ID, r2, model.DeploymentFailed, 30*time.Second)
	if rel.Reason != "service web did not roll out within 3s" {
		t.Errorf("reason %q", rel.Reason)
	}
	eventually(t, 30*time.Second, "the kept replica alone", func() bool {
		rows := h.rows(env.ID)
		return len(rows) == 1 && rows[0].Sandbox.ID == kept.Sandbox.ID && ready(rows[0])
	})
}

// busyPorts is a port door whose probes find every probe slot taken while
// busy is set.
type busyPorts struct {
	envdeps.Ports
	busy atomic.Bool
}

func (p *busyPorts) Probe(ctx context.Context, l model.Lease, port int, path string) (int, error) {
	if p.busy.Load() {
		return 0, ports.ErrProbeBusy
	}
	return p.Ports.Probe(ctx, l, port, path)
}

func (p *busyPorts) DialPod(ctx context.Context, l model.Lease, port int) (net.Conn, error) {
	if p.busy.Load() {
		return nil, ports.ErrProbeBusy
	}
	return p.Ports.DialPod(ctx, l, port)
}

// A check that finds every probe slot of its sandbox taken has no verdict
// (§13.3): a ready instance stays ready, its failures and detail as they
// were, even with a failure threshold of 1.
func TestProbeBusyIsInconclusive(t *testing.T) {
	door := &busyPorts{}
	h := newHarness(t, options{ports: func(p envdeps.Ports) envdeps.Ports {
		door.Ports = p
		return door
	}})
	n1 := h.addNode("n1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", `,"readiness":{"http":{"port":"http","path":"/"},"period":"1s","failureThreshold":1}`))
	row := h.waitRows(env.ID, spec.KindService, "web", 1, 30*time.Second, "running", running)[0]
	n1.Script().SetPodPortAddr(row.Sandbox.ID, 8080, strings.TrimPrefix(srv.URL, "http://"))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	door.busy.Store(true)
	never(t, 4*time.Second, "a busy probe counted", func() bool {
		inst, err := h.st.GetEnvInstance(bg, row.Sandbox.ID)
		return err != nil || inst.Phase != model.PhaseReady || inst.ProbeFailures != 0 || inst.Detail != ""
	})
}

// An addon whose address changes (restored on another node) gives its
// dependents a new template: they roll as for a change of T, surge 1,
// without a new deployment.
func TestAddonAddressRollsDependents(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, valkeySpec)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	addon := h.waitRows(env.ID, spec.KindAddon, "cache", 1, 10*time.Second, "ready", ready)[0]
	// n1 takes no other pod: the service goes to n2.
	h.exec(`UPDATE nodes SET slots = 1 WHERE name = 'n1'`)
	h.addNode("n2")
	r2 := h.apply(env.ID, fmt.Sprintf(`{"addons":{"cache":{"type":"valkey","version":"8","plan":"dev"}},
		"services":{"web":{"image":%q,"env":{"CACHE":{"addon":"cache","key":"url"}}}}}`, testImage))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	web := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0]
	if web.Sandbox.Node != "n2" {
		t.Fatalf("web on %s", web.Sandbox.Node)
	}

	var mu sync.Mutex
	maxLive, gap := 0, false
	stop, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			rows, err := h.st.ListEnvInstanceRows(bg, env.ID)
			if err != nil {
				continue
			}
			live, readyWeb, stopping := 0, 0, false
			for _, r := range rows {
				if r.Instance.Object != "web" {
					continue
				}
				if r.Instance.Phase == model.PhaseStopping {
					stopping = true
					continue
				}
				live++
				if r.Instance.Phase == model.PhaseReady {
					readyWeb++
				}
			}
			mu.Lock()
			maxLive = max(maxLive, live)
			gap = gap || (stopping && readyWeb == 0)
			mu.Unlock()
		}
	}()
	h.exec("UPDATE nodes SET draining = true, drain_started_at = now() WHERE name = 'n1'")
	var moved store.EnvInstanceRow
	eventually(t, 90*time.Second, "the addon on n2 with a new address", func() bool {
		h.c.notify(bg, "drain")
		rows := h.rowsOf(env.ID, spec.KindAddon, "cache")
		if len(rows) != 1 || rows[0].Sandbox.Node != "n2" || !running(rows[0]) || rows[0].Sandbox.Address == "" {
			return false
		}
		moved = rows[0]
		return true
	})
	if moved.Sandbox.ID != addon.Sandbox.ID || moved.Sandbox.Address == addon.Sandbox.Address {
		t.Fatalf("the addon after the drain %+v, before %+v", moved.Sandbox, addon.Sandbox)
	}
	eventually(t, 60*time.Second, "web rolled to the new address", func() bool {
		rows := h.rowsOf(env.ID, spec.KindService, "web")
		if len(rows) != 1 || rows[0].Sandbox.ID == web.Sandbox.ID || !ready(rows[0]) {
			return false
		}
		p := h.pod("n2", rows[0].Sandbox.ID)
		return p != nil && strings.Contains(p.Spec.Containers[0].Env["CACHE"], "["+moved.Sandbox.Address+"]")
	})
	close(stop)
	<-watched
	mu.Lock()
	if maxLive > 2 || gap {
		t.Errorf("at most %d web instances at once (want 2), a gap without a ready one %v", maxLive, gap)
	}
	mu.Unlock()
	if e, _ := h.st.GetEnvironment(bg, env.ID); e.Deployment != r2 || e.Deployed != r2 {
		t.Errorf("environment %+v", e)
	}
}

// A restart while the replica has an op in flight is refused with
// model.ErrBusy and takes no traffic away (the instance stays ready).
func TestRefusalsOfExpectedStates(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	id := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0].Sandbox.ID
	block := make(chan struct{})
	n1.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, func(ctx context.Context, _ string, _ any) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil
	})
	defer close(block)
	go func() { _ = h.eng.Pause(bg, id) }()
	eventually(t, 10*time.Second, "a pause in flight", func() bool {
		sb, err := h.st.GetSandbox(bg, id)
		return err == nil && sb.Op != ""
	})
	if err := h.c.RestartService(bg, env.ID, "web"); !errors.Is(err, model.ErrBusy) {
		t.Fatalf("a restart while an op is in flight: %v", err)
	}
	if inst, err := h.st.GetEnvInstance(bg, id); err != nil || inst.Phase != model.PhaseReady || inst.Detail != "" {
		t.Fatalf("the refused restart changed the instance: %+v %v", inst, err)
	}
}

// A manual job whose template cannot render yet is refused with
// ErrNotReady, its text as before.
func TestRunJobNotReady(t *testing.T) {
	// Without a sealer an addon never gets an address.
	h := newHarness(t, options{noSealer: true})
	h.addNode("n1")
	env := h.newEnv("stage")
	h.apply(env.ID, fmt.Sprintf(`{"addons":{"cache":{"type":"valkey","version":"8","plan":"dev"}},
		"jobs":{"once":{"image":%q,"on":"manual","env":{"CACHE":{"addon":"cache","key":"url"}}}}}`, testImage))
	err := h.c.RunJob(bg, env.ID, "once")
	if !errors.Is(err, ErrNotReady) || err.Error() != "job once cannot run yet: waits for the address of addon cache" {
		t.Fatalf("a job that cannot run yet: %v", err)
	}
}
