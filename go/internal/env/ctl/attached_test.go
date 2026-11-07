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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// processRoot is where the process scripts keep their records in a
// container; the tests point it at a directory of their own (the test
// node runs commands on the host).
const processRoot = "/var/lib/sandbox-processes"

// hostProcesses makes the node run the process scripts with their records
// under root and their working directory beneath the pod's directory,
// which stands in for the container's root.
func hostProcesses(n *testnode.Node, root string) {
	n.Script().SetExec(func(_ context.Context, pod podid.ID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		argv := req.Command
		if len(argv) > 2 && argv[0] == "python3" {
			start := strings.Contains(argv[2], "no such working directory")
			argv[2] = strings.ReplaceAll(argv[2], processRoot, root)
			if start && len(argv) > 6 && argv[6] != "" {
				argv[6] = filepath.Join(n.PodDir(pod), argv[6])
			}
		}
		return 0, false, nil
	})
}

// attachable makes an attachable sandbox of the tenant (the template the
// API gives it) and brings it up.
func (h *harness) attachable() store.Sandbox {
	h.t.Helper()
	return h.attachableAs(testTenant, podid.Mint())
}

// attachableAs makes a sandbox of owner in its environment dev (ADR
// 0035) with the ID id and brings it up.
func (h *harness) attachableAs(owner string, id podid.ID) store.Sandbox {
	h.t.Helper()
	return h.attachableIn(h.envOf(owner, "dev"), id)
}

// attachableIn makes a sandbox of env with the ID id and brings it up.
func (h *harness) attachableIn(env store.Environment, id podid.ID) store.Sandbox {
	h.t.Helper()
	if _, _, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: id, Owner: env.Owner, Image: testImage, ImageRef: testImage,
		VCPUs: 1, MemoryMiB: 512, EnvironmentID: env.ID, PodTemplate: &model.PodTemplate{Command: []string{"sleep", "infinity"},
			SecurityGroups: []string{model.SandboxGroupID(id)}, NameTables: []string{model.SandboxTableID(id)}}}); err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	if _, err := h.eng.Ensure(ctx, id); err != nil {
		h.t.Fatalf("ensure the attachable sandbox: %v", err)
	}
	sb, err := h.st.GetSandbox(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return sb
}

func attachedSpec(sandbox podid.ID, cwd, greeting string, withWeb bool) string {
	web := ""
	if withWeb {
		web = fmt.Sprintf(`,"web":{"runtime":"attached","sandbox":%q,"command":["sh","-c","echo started $GREETING $API; exec sleep 60"],
			"cwd":%q,"env":{"GREETING":%q,"API":{"service":"api","port":"http","key":"url"}},
			"ports":[{"name":"http","port":5173}]}`, sandbox, cwd, greeting)
	}
	return fmt.Sprintf(`{"services":{"api":{"image":%q,"ports":[{"name":"http","port":8080}]}%s}}`, testImage, web)
}

// An attached service is a supervised process in an attachable sandbox:
// started (once its working directory exists) with its env, named in the
// sandbox's name table on loopback beside the environment's image
// services (which the env table names, and the image pods resolve), the
// sandbox in the environment's group and its group reaching that group;
// restarted on request and on a changed template; removed with its
// process and its name. The attachable sandbox's own group and name table
// are put before its pod is created, also without an environment.
func TestAttachedService(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	hostProcesses(n1, t.TempDir())
	sb := h.attachable()
	// The prepare hook puts the group with its rules before the pod is
	// created, when the pod has no address yet; env-groups adds the
	// address as its member once the create's answer recorded it (the
	// hint of SetSandboxAddress), which may be after Ensure returned.
	sbx := h.group("n1", model.SandboxGroupID(sb.ID))
	if sbx == nil || len(sbx.Rules) != 2 {
		t.Fatalf("the sandbox's group %v", sbx)
	}
	eventually(t, 30*time.Second, "the sandbox's address the member of its group", func() bool {
		sbx = h.group("n1", model.SandboxGroupID(sb.ID))
		return sbx != nil && len(sbx.Rules) == 2 && slices.Equal(sbx.Members, []string{sb.Address})
	})
	if tbl := h.table("n1", model.SandboxTableID(sb.ID)); tbl == nil || len(tbl.Entries) != 0 || len(tbl.Includes) != 0 {
		t.Fatalf("the sandbox's name table %v", tbl)
	}

	env := h.newEnv("dev")
	r1 := h.apply(env.ID, attachedSpec(sb.ID, "/app", "hi", true))
	eventually(t, 30*time.Second, "the missing cwd", func() bool {
		a, err := h.st.GetAttached(bg, env.ID, "web")
		return err == nil && a.Phase == model.PhaseStarting && a.Detail == "cwd /app does not exist yet" && a.ProcessID == ""
	})
	if err := os.MkdirAll(filepath.Join(n1.PodDir(sb.ID), "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.Phase != model.PhaseReady || a.ProcessID == "" || a.SandboxIncarnation != sb.Incarnation ||
		a.Restarts != 0 {
		t.Fatalf("attached %+v %v", a, err)
	}
	ps, err := h.runner.ProcessesDetached(bg, lease(sb), "")
	if err != nil || len(ps) != 1 || ps[0].ID != a.ProcessID || ps[0].Name != "ferun-env-dev-web" || !ps[0].Running {
		t.Fatalf("processes %+v %v", ps, err)
	}
	var lines []string
	eventually(t, 10*time.Second, "the process's log", func() bool {
		var source string
		lines, source, err = h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 10)
		return err == nil && source == SourceProcess && len(lines) == 1
	})
	if lines[0] != "started hi http://api:8080" {
		t.Errorf("log %q", lines)
	}
	api := h.waitRows(env.ID, spec.KindService, "api", 1, 10*time.Second, "ready", ready)[0]
	eventually(t, 10*time.Second, "the names", func() bool {
		return resolves(n1, sb.ID, "web", "127.0.0.1", "::1") && resolves(n1, sb.ID, "API", api.Sandbox.Address) &&
			resolves(n1, api.Sandbox.ID, "api", api.Sandbox.Address) && resolves(n1, api.Sandbox.ID, "web")
	})
	if pod := h.pod("n1", api.Sandbox.ID); !slices.Equal(pod.GetSpec().GetNetwork().GetNameTables(),
		[]string{model.EnvTableID(env.ID)}) {
		t.Errorf("the image instance's name tables %v", pod.GetSpec().GetNetwork().GetNameTables())
	}
	if tbl := h.table("n1", model.SandboxTableID(sb.ID)); tbl == nil ||
		!slices.Equal(tbl.Includes, []string{model.EnvTableID(env.ID)}) {
		t.Errorf("the sandbox's name table %v", tbl)
	}
	// Groups: the sandbox is a member of the env group, and its group
	// reaches the env group.
	eventually(t, 30*time.Second, "the groups", func() bool {
		eg := h.group("n1", model.EnvGroupID(env.ID))
		sg := h.group("n1", model.SandboxGroupID(sb.ID))
		return eg != nil && sg != nil && slices.Contains(eg.Members, sb.Address) && slices.Contains(eg.Members, api.Sandbox.Address) &&
			slices.ContainsFunc(sg.Rules, func(r *networkv1.Rule) bool {
				return r.GetGroup() == model.EnvGroupID(env.ID) && r.Direction == networkv1.Direction_DIRECTION_EGRESS
			})
	})

	// A restart on request starts a new process.
	if err := h.c.RestartService(bg, env.ID, "web"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "a new process", func() bool {
		b, err := h.st.GetAttached(bg, env.ID, "web")
		return err == nil && b.ProcessID != "" && b.ProcessID != a.ProcessID && b.Phase == model.PhaseReady
	})
	// A changed template replaces the process.
	b, _ := h.st.GetAttached(bg, env.ID, "web")
	r2 := h.apply(env.ID, attachedSpec(sb.ID, "/app", "hello", true))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 60*time.Second)
	c, _ := h.st.GetAttached(bg, env.ID, "web")
	if c.ProcessID == b.ProcessID || c.Phase != model.PhaseReady {
		t.Fatalf("after the template changed %+v", c)
	}
	ps, _ = h.runner.ProcessesDetached(bg, lease(sb), "")
	runningProcs := 0
	for _, p := range ps {
		if p.Running {
			runningProcs++
			if p.ID != c.ProcessID {
				t.Errorf("an old process runs: %+v", p)
			}
		}
	}
	if runningProcs != 1 {
		t.Errorf("%d processes run: %+v", runningProcs, ps)
	}
	// A process that exits is restarted after its backoff.
	if err := h.runner.StopProcessDetached(bg, lease(sb), c.ProcessID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the restart after the exit", func() bool {
		d, err := h.st.GetAttached(bg, env.ID, "web")
		return err == nil && d.Restarts == 1 && d.ProcessID != "" && d.ProcessID != c.ProcessID
	})

	// Removed: the process stops, the row and the name go.
	d, _ := h.st.GetAttached(bg, env.ID, "web")
	r3 := h.apply(env.ID, attachedSpec(sb.ID, "/app", "", false))
	h.waitDeployment(env.ID, r3, model.DeploymentDeployed, 60*time.Second)
	if _, err := h.st.GetAttached(bg, env.ID, "web"); !errors.Is(err, store.ErrNoRow) {
		t.Errorf("the attached row: %v", err)
	}
	ps, _ = h.runner.ProcessesDetached(bg, lease(sb), "")
	for _, p := range ps {
		if p.Running {
			t.Errorf("a process still runs: %+v (last %s)", p, d.ProcessID)
		}
	}
	eventually(t, 10*time.Second, "the sandbox's names after the removal", func() bool {
		tbl := h.table("n1", model.SandboxTableID(sb.ID))
		return tbl != nil && len(tbl.Entries) == 0 && len(tbl.Includes) == 0 && resolves(n1, sb.ID, "web") &&
			resolves(n1, sb.ID, "api")
	})
	eventually(t, 30*time.Second, "the sandbox out of the env group", func() bool {
		eg := h.group("n1", model.EnvGroupID(env.ID))
		return eg != nil && !slices.Contains(eg.Members, sb.Address)
	})
}

// An attached service in a paused sandbox: the controller wakes it to
// start the process (and never for a probe).
func TestAttachedWakesPausedSandbox(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	hostProcesses(n1, t.TempDir())
	sb := h.attachable()
	if err := h.eng.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "paused", func() bool {
		s, _ := h.st.GetSandbox(bg, sb.ID)
		return s.ClientState == model.StatePaused && s.Op == ""
	})
	env := h.newEnv("dev")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"web":{"runtime":"attached","sandbox":%q,
		"command":["sleep","60"]}}}`, sb.ID))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.ProcessID == "" || a.Phase != model.PhaseReady {
		t.Fatalf("attached %+v %v", a, err)
	}
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 5); err != nil {
		t.Errorf("logs of a running process: %v", err)
	}
	// Paused again, its logs are not read (it is not woken).
	if err := h.eng.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "paused again", func() bool {
		s, _ := h.st.GetSandbox(bg, sb.ID)
		return s.ClientState == model.StatePaused && s.Op == ""
	})
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 5); !errors.Is(err, ErrPaused) {
		t.Errorf("logs of a paused sandbox: %v", err)
	}
	never(t, 3*time.Second, "the probes woke the sandbox", func() bool {
		s, _ := h.st.GetSandbox(bg, sb.ID)
		return s.ClientState != model.StatePaused
	})
}

// startLog counts the process starts the node ran, per pod.
type startLog struct {
	mu     sync.Mutex
	starts map[podid.ID]int
}

func (l *startLog) count(pod podid.ID) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts[pod]
}

// hostProcessesCounting is hostProcesses that also counts the starts.
func hostProcessesCounting(n *testnode.Node, root string) *startLog {
	l := &startLog{starts: map[podid.ID]int{}}
	n.Script().SetExec(func(_ context.Context, pod podid.ID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		argv := req.Command
		if len(argv) > 2 && argv[0] == "python3" {
			start := strings.Contains(argv[2], "no such working directory")
			argv[2] = strings.ReplaceAll(argv[2], processRoot, root)
			if start {
				l.mu.Lock()
				l.starts[pod]++
				l.mu.Unlock()
				if len(argv) > 6 && argv[6] != "" {
					argv[6] = filepath.Join(n.PodDir(pod), argv[6])
				}
			}
		}
		return 0, false, nil
	})
	return l
}

// An attached service's sandbox deleted and its ID taken by another
// tenant's attachable sandbox: the service fails ("gone or not this
// tenant's"); nothing starts in the new sandbox, it is given no names of
// the environment, it joins no group of the environment and its own group
// reaches none; its logs and restarts answer ErrNotRunning.
func TestAttachedSandboxReusedByAnotherTenant(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	starts := hostProcessesCounting(n1, t.TempDir())
	sb := h.attachable()
	env := h.newEnv("dev")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"api":{"image":%q},
		"web":{"runtime":"attached","sandbox":%q,"command":["sleep","60"]}}}`, testImage, sb.ID))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.ProcessID == "" || a.SandboxIncarnation != sb.Incarnation {
		t.Fatalf("attached %+v %v", a, err)
	}

	// The tenant deletes its sandbox; another tenant takes its ID.
	if _, _, err := h.eng.Delete(bg, testTenant, sb.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the sandbox's pod gone", func() bool { return h.pod("n1", sb.ID) == nil })
	if _, _, err := h.st.CreateTenant(bg, "t2"); err != nil {
		t.Fatal(err)
	}
	before := starts.count(sb.ID)
	other := h.attachableAs("t2", sb.ID)
	eventually(t, 30*time.Second, "the service failed", func() bool {
		b, err := h.st.GetAttached(bg, env.ID, "web")
		return err == nil && b.Phase == model.PhaseFailed && b.ProcessID == "" &&
			b.Detail == fmt.Sprintf("sandbox %s is gone or not this tenant's", sb.ID)
	})
	never(t, 3*time.Second, "something happened in the other tenant's sandbox", func() bool {
		_, named := n1.Resolve(sb.ID, "web")
		_, api := n1.Resolve(sb.ID, "api")
		return starts.count(sb.ID) != before || named || api
	})
	if tbl := h.table("n1", model.SandboxTableID(sb.ID)); tbl == nil || len(tbl.Entries) != 0 || len(tbl.Includes) != 0 {
		t.Errorf("the other tenant's sandbox's name table %v", tbl)
	}
	if b, _ := h.st.GetAttached(bg, env.ID, "web"); b.SandboxIncarnation != sb.Incarnation {
		t.Errorf("the recorded incarnation changed: %+v", b)
	}
	// Groups: the other sandbox is no member of the environment's group,
	// and its own group reaches no group of the environment.
	if b, err := h.c.buildGroup(bg, model.EnvGroupID(env.ID), newGroupPass()); err != nil || slices.Contains(b.g.Members, other.Address) {
		t.Fatalf("the env group %v %v", b, err)
	}
	if g := h.group("n1", model.SandboxGroupID(sb.ID)); g == nil || slices.ContainsFunc(g.Rules, func(r *networkv1.Rule) bool {
		return r.GetGroup() == model.EnvGroupID(env.ID)
	}) {
		t.Fatalf("the other sandbox's group %v", g)
	}
	eventually(t, 30*time.Second, "the node's env group without the other sandbox", func() bool {
		h.c.notify(bg, HintGroups)
		g := h.group("n1", model.EnvGroupID(env.ID))
		return g != nil && !slices.Contains(g.Members, other.Address) && len(g.Members) == 1
	})
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 5); !errors.Is(err, ErrNotRunning) {
		t.Errorf("logs of the gone sandbox: %v", err)
	}
	if err := h.c.RestartService(bg, env.ID, "web"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("a restart in the gone sandbox: %v", err)
	}
	if starts.count(sb.ID) != before {
		t.Error("a process started in the other tenant's sandbox")
	}
}

// runningNamed are the IDs of the sandbox's running processes named name.
func (h *harness) runningNamed(sb store.Sandbox, name string) []string {
	h.t.Helper()
	ps, err := h.runner.ProcessesDetached(bg, lease(sb), name)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, p := range ps {
		if p.Running && p.Name == name {
			out = append(out, p.ID)
		}
	}
	return out
}

// An attached service's processes the row does not record never run on:
// one with its name is stopped before a new one starts (a look whose
// write was lost), one a look started but could not record is stopped at
// once; a restart in a paused sandbox is refused (ErrPaused) instead of
// leaving the process to survive the resume beside a new one.
func TestAttachedStrayProcesses(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	hostProcesses(n1, t.TempDir())
	sb := h.attachable()
	env := h.newEnv("dev")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"web":{"runtime":"attached","sandbox":%q,"command":["sleep","60"]}}}`, sb.ID))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.ProcessID == "" {
		t.Fatalf("attached %+v %v", a, err)
	}

	// A row that lost its process's ID: the next start stops the process.
	h.exec(`UPDATE env_attached SET process_id = '' WHERE env_id = $1 AND service = 'web'`, env.ID)
	var b store.EnvAttached
	eventually(t, 30*time.Second, "a new process", func() bool {
		b, err = h.st.GetAttached(bg, env.ID, "web")
		return err == nil && b.ProcessID != "" && b.ProcessID != a.ProcessID && b.Phase == model.PhaseReady
	})
	if got := h.runningNamed(sb, "ferun-env-dev-web"); !slices.Equal(got, []string{b.ProcessID}) {
		t.Fatalf("running %v, the row records %s", got, b.ProcessID)
	}

	// A process a look started but could not record (the row moved).
	orphan, err := h.runner.StartProcessDetached(bg, lease(sb), tools.ProcessStart{Argv: []string{"sleep", "60"}, Name: "orphan"})
	if err != nil {
		t.Fatal(err)
	}
	before := b
	before.SandboxID = podid.Mint()
	after := before
	after.ProcessID = orphan
	h.c.writeAttached(bg, before, after, lease(sb))
	if got := h.runningNamed(sb, "orphan"); len(got) != 0 {
		t.Fatalf("the unrecorded process runs: %v", got)
	}
	if c, _ := h.st.GetAttached(bg, env.ID, "web"); c.ProcessID != b.ProcessID {
		t.Fatalf("the row changed: %+v", c)
	}

	// A restart in a paused sandbox is refused and wakes nothing.
	if err := h.eng.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "paused", func() bool {
		s, _ := h.st.GetSandbox(bg, sb.ID)
		return s.ClientState == model.StatePaused && s.Op == ""
	})
	if err := h.c.RestartService(bg, env.ID, "web"); !errors.Is(err, ErrPaused) {
		t.Fatalf("a restart in a paused sandbox: %v", err)
	}
	never(t, 3*time.Second, "the refused restart changed something", func() bool {
		s, _ := h.st.GetSandbox(bg, sb.ID)
		c, _ := h.st.GetAttached(bg, env.ID, "web")
		return s.ClientState != model.StatePaused || c.ProcessID != b.ProcessID
	})
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	if _, err := h.eng.Ensure(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.runningNamed(sb, "ferun-env-dev-web"); !slices.Equal(got, []string{b.ProcessID}) {
		t.Fatalf("after the resume: running %v, the row records %s", got, b.ProcessID)
	}
	// Running again, the restart goes through: one new process.
	if err := h.c.RestartService(bg, env.ID, "web"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the restarted process", func() bool {
		c, err := h.st.GetAttached(bg, env.ID, "web")
		return err == nil && c.ProcessID != "" && c.ProcessID != b.ProcessID &&
			slices.Equal(h.runningNamed(sb, "ferun-env-dev-web"), []string{c.ProcessID})
	})
}

// A restart on request is no exit: a look of the supervisor that read the
// row before the restart and lists the processes after it finds the
// process gone, and must not count that as an exit (a restart, its
// backoff, the last exit) onto the row the restart cleared.
func TestAttachedRestartIsNoExit(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	root := t.TempDir()
	// Armed, the next listing of the service's processes waits for
	// release: the look that lists is then between reading its row and
	// learning what runs.
	var armed atomic.Bool
	listing, release := make(chan struct{}), make(chan struct{})
	n1.Script().SetExec(func(ctx context.Context, pod podid.ID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		argv := req.Command
		if len(argv) > 2 && argv[0] == "python3" {
			if len(argv) == 4 && strings.HasPrefix(argv[2], "import json,") && argv[3] == "ferun-env-dev-web" &&
				armed.CompareAndSwap(true, false) {
				close(listing)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			start := strings.Contains(argv[2], "no such working directory")
			argv[2] = strings.ReplaceAll(argv[2], processRoot, root)
			if start && len(argv) > 6 && argv[6] != "" {
				argv[6] = filepath.Join(n1.PodDir(pod), argv[6])
			}
		}
		return 0, false, nil
	})
	sb := h.attachable()
	env := h.newEnv("dev")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"web":{"runtime":"attached","sandbox":%q,"command":["sleep","60"]}}}`, sb.ID))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.ProcessID == "" || a.Restarts != 0 {
		t.Fatalf("attached %+v %v", a, err)
	}

	armed.Store(true)
	select {
	case <-listing:
	case <-time.After(30 * time.Second):
		t.Fatal("no look listed the processes within 30s")
	}
	err = h.c.RestartService(bg, env.ID, "web")
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	var b store.EnvAttached
	eventually(t, 30*time.Second, "the restarted process", func() bool {
		b, err = h.st.GetAttached(bg, env.ID, "web")
		return err == nil && b.ProcessID != "" && b.ProcessID != a.ProcessID && b.Phase == model.PhaseReady
	})
	if b.Restarts != 0 || b.LastExit != nil {
		t.Errorf("the restart counted as an exit: %+v", b)
	}
	if got := h.runningNamed(sb, "ferun-env-dev-web"); !slices.Equal(got, []string{b.ProcessID}) {
		t.Errorf("running %v, the row records %s", got, b.ProcessID)
	}
}

// An attached service that goes while its sandbox sleeps — removed from
// the spec, or its environment deleted — is stopped in the sandbox woken
// for it before its row goes (and the environment's, once nothing of it is
// left): a process left there would run on with the environment's secrets
// after a resume. Its name leaves the sandbox's table with the row; the
// sleeping sandbox resolves its names all along, as its node answers them.
func TestAttachedGoesFromSleepingSandbox(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	hostProcesses(n1, t.TempDir())
	sb := h.attachable()
	sleep := func() {
		t.Helper()
		if err := h.eng.Pause(bg, sb.ID); err != nil {
			t.Fatal(err)
		}
		eventually(t, 30*time.Second, "paused", func() bool {
			s, _ := h.st.GetSandbox(bg, sb.ID)
			return s.ClientState == model.StatePaused && s.Op == ""
		})
	}
	up := func(envID uuid.UUID, r int) store.EnvAttached {
		t.Helper()
		h.waitDeployment(envID, r, model.DeploymentDeployed, 60*time.Second)
		a, err := h.st.GetAttached(bg, envID, "web")
		if err != nil || a.ProcessID == "" {
			t.Fatalf("attached %+v %v", a, err)
		}
		return a
	}
	web := fmt.Sprintf(`"web":{"runtime":"attached","sandbox":%q,"command":["sleep","120"]}`, sb.ID)
	other := `"api":{"image":"` + testImage + `","ports":[{"name":"http","port":8080}]}`

	// Removed from the spec while the sandbox is paused.
	dev := h.newEnv("dev")
	a := up(dev.ID, h.apply(dev.ID, `{"services":{`+web+`,`+other+`}}`))
	eventually(t, 10*time.Second, "the names", func() bool {
		return resolves(n1, sb.ID, "web", "127.0.0.1", "::1")
	})
	sleep()
	if !resolves(n1, sb.ID, "web", "127.0.0.1", "::1") {
		t.Fatal("the sleeping sandbox lost its names")
	}
	r2 := h.apply(dev.ID, `{"services":{`+other+`}}`)
	h.waitDeployment(dev.ID, r2, model.DeploymentDeployed, 60*time.Second)
	if _, err := h.st.GetAttached(bg, dev.ID, "web"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("the removed service's row: %v", err)
	}
	cur := h.attachableUp(sb.ID)
	if got := h.runningNamed(cur, "ferun-env-dev-web"); len(got) != 0 {
		t.Fatalf("the removed service's process %s runs on: %v", a.ProcessID, got)
	}
	eventually(t, 10*time.Second, "the removed service's name gone", func() bool {
		return resolves(n1, sb.ID, "web")
	})

	// Its environment deleted while its sandbox is paused (as a tenant's
	// deletion does; the API refuses it while the environment has
	// sandboxes, ADR 0035): the service goes first, the environment once
	// its sandbox has gone too.
	stage := h.newEnv("stage")
	box := h.attachableIn(stage, podid.Mint())
	up(stage.ID, h.apply(stage.ID, fmt.Sprintf(`{"services":{"web":{"runtime":"attached","sandbox":%q,
		"command":["sleep","120"]}}}`, box.ID)))
	h.sleepUntilPaused(box.ID)
	h.exec(`UPDATE environments SET deleting = true WHERE id = $1`, stage.ID)
	eventually(t, 60*time.Second, "the attached service gone", func() bool {
		h.c.notify(bg, LoopDelete)
		_, err := h.st.GetAttached(bg, stage.ID, "web")
		return errors.Is(err, store.ErrNoRow)
	})
	cur = h.attachableUp(box.ID)
	if got := h.runningNamed(cur, "ferun-env-stage-web"); len(got) != 0 {
		t.Fatalf("the deleted environment's process runs on: %v", got)
	}
	eventually(t, 10*time.Second, "the deleted environment's names gone", func() bool {
		return resolves(n1, box.ID, "web")
	})
	if _, err := h.st.GetEnvironment(bg, stage.ID); err != nil {
		t.Fatalf("the environment went before its sandbox: %v", err)
	}
	if _, _, err := h.eng.Delete(bg, testTenant, box.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "the environment deleted", func() bool {
		h.c.notify(bg, LoopDelete)
		_, err := h.st.GetEnvironment(bg, stage.ID)
		return errors.Is(err, store.ErrNoRow)
	})
}

// attachableUp brings an attachable sandbox up (it may sleep) and reads
// its row.
func (h *harness) attachableUp(id podid.ID) store.Sandbox {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	if _, err := h.eng.Ensure(ctx, id); err != nil {
		h.t.Fatal(err)
	}
	sb, err := h.st.GetSandbox(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return sb
}
