// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// The process scripts' stop, by a line only it has.
const stopScriptMark = "os.killpg(pgid, signal.SIGTERM)"

// podProcesses is hostProcesses with each pod's records in a directory
// of its own (root/<pod>), as each container has its own, and with stops
// that fail while failStops is set (an exec cap, a node that does not
// answer).
type podProcesses struct {
	root      string
	failStops atomic.Bool
	stops     atomic.Int64
}

func (pp *podProcesses) dir(pod podid.ID) string { return filepath.Join(pp.root, pod.String()) }

func hostProcessesPerPod(n *testnode.Node, root string) *podProcesses {
	pp := &podProcesses{root: root}
	n.Script().SetExec(func(_ context.Context, pod podid.ID, req *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		argv := req.Command
		if len(argv) > 2 && argv[0] == "python3" {
			if strings.Contains(argv[2], stopScriptMark) {
				pp.stops.Add(1)
				if pp.failStops.Load() {
					return 1, true, out.Stderr([]byte("injected failure\n"))
				}
			}
			start := strings.Contains(argv[2], "no such working directory")
			argv[2] = strings.ReplaceAll(argv[2], processRoot, pp.dir(pod))
			if start && len(argv) > 6 && argv[6] != "" {
				argv[6] = filepath.Join(n.PodDir(pod), argv[6])
			}
		}
		return 0, false, nil
	})
	return pp
}

// sleepUntilPaused pauses a sandbox and waits until it is paused.
func (h *harness) sleepUntilPaused(id podid.ID) {
	h.t.Helper()
	if err := h.eng.Pause(bg, id); err != nil {
		h.t.Fatal(err)
	}
	eventually(h.t, 30*time.Second, "paused", func() bool {
		s, _ := h.st.GetSandbox(bg, id)
		return s.ClientState == model.StatePaused && s.Op == ""
	})
}

// FeCode replaces a lost workspace sandbox under the same ID: the
// attached service in it comes back in the new incarnation (§13.6), with
// restarts, next_restart_at and probe_failures reset and the new
// incarnation recorded; its process starts there, the new sandbox's pod
// resolves its name and the new sandbox is a member of the environment's
// group.
func TestAttachedNewIncarnation(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	hostProcessesPerPod(n1, t.TempDir())
	sb := h.attachable()
	env := h.newEnv("dev")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"api":{"image":%q,"ports":[{"name":"http","port":8080}]},
		"web":{"runtime":"attached","sandbox":%q,"command":["sleep","120"]}}}`, testImage, sb.ID))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.ProcessID == "" || a.SandboxIncarnation != sb.Incarnation {
		t.Fatalf("attached %+v %v", a, err)
	}
	// An exit, so that there is a restart to reset.
	if err := h.runner.StopProcessDetached(bg, lease(sb), a.ProcessID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the restart after the exit", func() bool {
		b, err := h.st.GetAttached(bg, env.ID, "web")
		return err == nil && b.Restarts == 1 && b.ProcessID != "" && b.ProcessID != a.ProcessID && b.Phase == model.PhaseReady
	})
	b, _ := h.st.GetAttached(bg, env.ID, "web")

	// The sandbox is lost and replaced under the same ID, by the same
	// tenant.
	if _, _, err := h.eng.Delete(bg, testTenant, sb.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the sandbox's pod gone", func() bool { return h.pod("n1", sb.ID) == nil })
	again := h.attachableAs(testTenant, sb.ID)
	if again.Incarnation == sb.Incarnation {
		t.Fatal("the same incarnation")
	}
	var c store.EnvAttached
	eventually(t, 60*time.Second, "the service back in the new incarnation", func() bool {
		c, err = h.st.GetAttached(bg, env.ID, "web")
		return err == nil && c.SandboxIncarnation == again.Incarnation && c.ProcessID != "" && c.ProcessID != b.ProcessID &&
			c.Phase == model.PhaseReady
	})
	if c.Restarts != 0 || c.ProbeFailures != 0 || !c.NextRestartAt.IsZero() || c.Detail != "" {
		t.Errorf("not reset: %+v", c)
	}
	if got := h.runningNamed(again, "ferun-env-dev-web"); !slices.Equal(got, []string{c.ProcessID}) {
		t.Errorf("running %v, the row records %s", got, c.ProcessID)
	}
	eventually(t, 10*time.Second, "the new sandbox's names", func() bool {
		return resolves(n1, sb.ID, "web", "127.0.0.1", "::1") && resolves(n1, sb.ID, "nope")
	})
	eventually(t, 30*time.Second, "the new sandbox in the env group", func() bool {
		h.c.notify(bg, HintGroups)
		g := h.group("n1", model.EnvGroupID(env.ID))
		return g != nil && slices.Contains(g.Members, again.Address)
	})
}

// An attached service whose process cannot be stopped keeps its row (and
// its environment keeps its own) until the stop succeeds: removed from
// the spec, or its environment deleted, while its sandbox runs. The
// running sandbox is held for the stop (an exec hold), so that it cannot
// pause between the look and the stop.
func TestAttachedStopFailureKeepsRow(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	pp := hostProcessesPerPod(n1, t.TempDir())
	sb := h.attachable()
	web := fmt.Sprintf(`"web":{"runtime":"attached","sandbox":%q,"command":["sleep","120"]}`, sb.ID)
	api := `"api":{"image":"` + testImage + `","ports":[{"name":"http","port":8080}]}`

	// Removed from the spec.
	dev := h.newEnv("dev")
	h.waitDeployment(dev.ID, h.apply(dev.ID, `{"services":{`+web+`,`+api+`}}`), model.DeploymentDeployed, 60*time.Second)
	a, err := h.st.GetAttached(bg, dev.ID, "web")
	if err != nil || a.ProcessID == "" {
		t.Fatalf("attached %+v %v", a, err)
	}
	pp.failStops.Store(true)
	before := pp.stops.Load()
	r2 := h.apply(dev.ID, `{"services":{`+api+`}}`)
	eventually(t, 30*time.Second, "a stop tried", func() bool { return pp.stops.Load() > before })
	never(t, 3*time.Second, "the row went while the stop fails", func() bool {
		_, err := h.st.GetAttached(bg, dev.ID, "web")
		return err != nil
	})
	if got := h.runningNamed(sb, "ferun-env-dev-web"); !slices.Equal(got, []string{a.ProcessID}) {
		t.Fatalf("running %v", got)
	}
	if r := h.deployment(dev.ID, r2); r.State == model.DeploymentDeployed {
		t.Fatalf("deployed while the service is not removed: %+v", r)
	}
	pp.failStops.Store(false)
	h.waitDeployment(dev.ID, r2, model.DeploymentDeployed, 60*time.Second)
	if _, err := h.st.GetAttached(bg, dev.ID, "web"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("the removed service's row: %v", err)
	}
	if got := h.runningNamed(sb, "ferun-env-dev-web"); len(got) != 0 {
		t.Fatalf("the removed service's process runs on: %v", got)
	}
	eventually(t, 10*time.Second, "the removed service's name gone", func() bool {
		return resolves(n1, sb.ID, "web") && resolves(n1, sb.ID, "api")
	})

	// Its environment deleted (as a tenant's deletion does; the API
	// refuses it while the environment has sandboxes, ADR 0035).
	stage := h.newEnv("stage")
	sb = h.attachableIn(stage, podid.Mint())
	web = fmt.Sprintf(`"web":{"runtime":"attached","sandbox":%q,"command":["sleep","120"]}`, sb.ID)
	h.waitDeployment(stage.ID, h.apply(stage.ID, `{"services":{`+web+`}}`), model.DeploymentDeployed, 60*time.Second)
	b, err := h.st.GetAttached(bg, stage.ID, "web")
	if err != nil || b.ProcessID == "" {
		t.Fatalf("attached %+v %v", b, err)
	}
	pp.failStops.Store(true)
	before = pp.stops.Load()
	h.exec(`UPDATE environments SET deleting = true WHERE id = $1`, stage.ID)
	h.c.notify(bg, LoopDelete)
	eventually(t, 30*time.Second, "a stop tried", func() bool { return pp.stops.Load() > before })
	never(t, 6*time.Second, "the environment or its attached row went while the stop fails", func() bool {
		_, err := h.st.GetAttached(bg, stage.ID, "web")
		_, eerr := h.st.GetEnvironment(bg, stage.ID)
		return err != nil || eerr != nil
	})
	if got := h.runningNamed(sb, "ferun-env-stage-web"); !slices.Equal(got, []string{b.ProcessID}) {
		t.Fatalf("running %v", got)
	}
	pp.failStops.Store(false)
	eventually(t, 60*time.Second, "the attached service gone", func() bool {
		h.c.notify(bg, LoopDelete)
		_, err := h.st.GetAttached(bg, stage.ID, "web")
		return errors.Is(err, store.ErrNoRow)
	})
	if got := h.runningNamed(sb, "ferun-env-stage-web"); len(got) != 0 {
		t.Fatalf("the deleted environment's process runs on: %v", got)
	}
	// The environment goes once its sandbox has.
	if _, _, err := h.eng.Delete(bg, testTenant, sb.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "the environment deleted", func() bool {
		h.c.notify(bg, LoopDelete)
		_, err := h.st.GetEnvironment(bg, stage.ID)
		return errors.Is(err, store.ErrNoRow)
	})
}

// An attached service moved away from a sandbox that sleeps: the old
// sandbox is woken, the process stopped there, and only then does the row
// move, its name with it from the old sandbox's table (which keeps the
// services that stay) to the new one's; the service then runs in the new
// sandbox.
func TestAttachedMovesFromSleepingSandbox(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	hostProcessesPerPod(n1, t.TempDir())
	s1, s2 := h.attachable(), h.attachable()
	attached := func(name string, sb podid.ID) string {
		return fmt.Sprintf(`%q:{"runtime":"attached","sandbox":%q,"command":["sleep","120"]}`, name, sb)
	}
	env := h.newEnv("dev")
	r1 := h.apply(env.ID, `{"services":{`+attached("web", s1.ID)+`,`+attached("worker", s1.ID)+`}}`)
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.ProcessID == "" {
		t.Fatalf("attached %+v %v", a, err)
	}
	eventually(t, 10*time.Second, "the first sandbox's names", func() bool {
		return resolves(n1, s1.ID, "web", "127.0.0.1", "::1") && resolves(n1, s1.ID, "worker", "127.0.0.1", "::1")
	})
	h.sleepUntilPaused(s1.ID)

	r2 := h.apply(env.ID, `{"services":{`+attached("web", s2.ID)+`,`+attached("worker", s1.ID)+`}}`)
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	b, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || b.SandboxID != s2.ID || b.ProcessID == "" || b.Phase != model.PhaseReady {
		t.Fatalf("moved %+v %v", b, err)
	}
	cur := h.attachableUp(s1.ID)
	if got := h.runningNamed(cur, "ferun-env-dev-web"); len(got) != 0 {
		t.Fatalf("the moved service's process %s runs on in the old sandbox: %v", a.ProcessID, got)
	}
	if got := h.runningNamed(cur, "ferun-env-dev-worker"); len(got) != 1 {
		t.Fatalf("the service that stays: %v", got)
	}
	eventually(t, 30*time.Second, "the moved service's name in the new sandbox alone", func() bool {
		return resolves(n1, s1.ID, "web") && resolves(n1, s1.ID, "worker", "127.0.0.1", "::1") &&
			resolves(n1, s2.ID, "web", "127.0.0.1", "::1") && resolves(n1, s2.ID, "worker")
	})
	if got := h.runningNamed(h.attachableUp(s2.ID), "ferun-env-dev-web"); !slices.Equal(got, []string{b.ProcessID}) {
		t.Fatalf("the new sandbox runs %v, the row records %s", got, b.ProcessID)
	}
}

// A sandbox under an attached service's ID that is a new incarnation, or
// stopped, holds nothing of the service's to clean: its row may go
// without a wake (the process went with the old incarnation). The
// incarnation the process runs in is woken and cleaned when it sleeps.
func TestAttachedCleanLeavesNewIncarnation(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	pp := hostProcessesPerPod(n1, t.TempDir())
	sb := h.attachable()
	h.sleepUntilPaused(sb.ID)
	env := h.newEnv("dev")
	v := &view{env: env, target: emptySpec(), latestSpec: emptySpec()}
	a, err := h.st.InsertAttached(bg, store.EnvAttached{EnvID: env.ID, Service: "web", SandboxID: sb.ID,
		SandboxIncarnation: uuid.New(), ProcessID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	var done atomic.Int64
	count := func(context.Context, store.EnvAttached) { done.Add(1) }
	if !h.c.cleanOld(bg, v, a, count) {
		t.Fatal("a new incarnation is to be cleaned")
	}
	never(t, 2*time.Second, "the new incarnation woken", func() bool {
		s, _ := h.st.GetSandbox(bg, sb.ID)
		return s.ClientState != model.StatePaused || done.Load() != 0 || pp.stops.Load() != 0
	})
	// The incarnation the process runs in, asleep: woken and cleaned in
	// the pool, which then hands the row on.
	after := a
	after.SandboxIncarnation = sb.Incarnation
	if a, err = h.st.UpdateAttached(bg, a, after); err != nil {
		t.Fatal(err)
	}
	if h.c.cleanOld(bg, v, a, count) {
		t.Fatal("nothing to clean in the sandbox the process runs in")
	}
	eventually(t, 30*time.Second, "cleaned in the woken sandbox", func() bool { return done.Load() == 1 && pp.stops.Load() == 1 })
	if !leftNothing(env, store.EnvAttached{SandboxID: sb.ID}, store.Sandbox{ID: sb.ID, Owner: env.Owner,
		EnvironmentID: env.ID, ClientState: model.StateStopped}) {
		t.Error("a stopped sandbox holds something")
	}
	if leftNothing(env, store.EnvAttached{SandboxID: sb.ID}, store.Sandbox{ID: sb.ID, Owner: env.Owner,
		EnvironmentID: env.ID, ClientState: model.StatePaused}) {
		t.Error("a paused sandbox holds nothing")
	}
}

// seedRecords writes n records of processes that are over, named name,
// under dir, as the start script writes them.
func seedRecords(t *testing.T, dir, prefix, name string, n int) {
	t.Helper()
	for i := range n {
		d := filepath.Join(dir, fmt.Sprintf("%s%06d", prefix, i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range map[string]string{"name": name, "cmd": strings.Repeat("node server.js --port 3000 ", 7), "pgid": "0",
			"timeout": "0", "started_at": strconv.Itoa(1000 + i), "exit": "1"} {
			if err := os.WriteFile(filepath.Join(d, k), []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Process records pile up in a sandbox (the agent's own processes, and
// every restart of an attached service): the supervisor lists only its
// service's, so that it keeps supervising however many there are, and a
// start removes its name's records that are over but the newest few.
func TestAttachedSupervisedPastManyRecords(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	pp := hostProcessesPerPod(n1, t.TempDir())
	sb := h.attachable()
	env := h.newEnv("dev")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"web":{"runtime":"attached","sandbox":%q,"command":["sleep","120"]}}}`, sb.ID))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.ProcessID == "" {
		t.Fatalf("attached %+v %v", a, err)
	}
	seedRecords(t, pp.dir(sb.ID), "000000-agent", "", 300)
	seedRecords(t, pp.dir(sb.ID), "000000-web", "ferun-env-dev-web", 10)

	if err := h.runner.StopProcessDetached(bg, lease(sb), a.ProcessID); err != nil {
		t.Fatal(err)
	}
	var b store.EnvAttached
	eventually(t, 30*time.Second, "the restart after the exit", func() bool {
		b, err = h.st.GetAttached(bg, env.ID, "web")
		return err == nil && b.Restarts == 1 && b.ProcessID != "" && b.ProcessID != a.ProcessID && b.Phase == model.PhaseReady
	})
	ps, err := h.runner.ProcessesDetached(bg, lease(sb), "ferun-env-dev-web")
	if err != nil {
		t.Fatal(err)
	}
	var over []string
	for _, p := range ps {
		if !p.Running {
			over = append(over, p.ID)
		}
	}
	// The newest three that are over: the process stopped above and the
	// two newest seeded ones.
	if want := []string{"000000-web000008", "000000-web000009", a.ProcessID}; !slices.Equal(over, want) || len(ps) != 4 {
		t.Fatalf("records of the service %+v, over %v, want %v", ps, over, want)
	}
	if all, err := h.runner.ProcessesDetached(bg, lease(sb), ""); err != nil || len(all) != 304 {
		t.Fatalf("every record: %d %v", len(all), err)
	}
}

// A cleanup that failed waits for the restart backoff before it runs
// again (a sandbox whose wake keeps failing is not woken every round);
// one that succeeded forgets its failures.
func TestCleanBackoff(t *testing.T) {
	c := &Controller{t: defaultTimings()}
	c.t.restartUnit, c.t.restartMax = time.Hour, 4*time.Hour
	const key = "attached-clean/x"
	if !c.cleanDue(key) {
		t.Fatal("a cleanup that never failed waits")
	}
	c.cleanFailed(key)
	if c.cleanDue(key) {
		t.Fatal("a cleanup that failed runs again at once")
	}
	c.cleanFailed(key)
	if w := c.cleanWaits[key]; w.failures != 2 || time.Until(w.next) < time.Hour+50*time.Minute {
		t.Fatalf("the backoff after two failures %+v", w)
	}
	c.cleanOK(key)
	if !c.cleanDue(key) {
		t.Fatal("a cleanup that succeeded still waits")
	}
}
