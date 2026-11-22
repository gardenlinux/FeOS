// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// podState waits until the node's record of the pod is in state.
func podState(t *testing.T, n *node, id podid.ID, state podv1.PodState, within time.Duration) *podv1.PodInfo {
	t.Helper()
	var p *podv1.PodInfo
	eventually(t, within, "pod "+id.String()+" "+state.String()+" on "+n.name, func() bool {
		var ok bool
		p, ok = n.pod(id)
		return ok && p.GetState() == state
	})
	return p
}

// Scenario 3: a REST pause and resume, through different instances, move
// the pod on its node; the next tool call resumes a paused sandbox by
// itself.
func TestPauseResume(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	a := c.start("a")
	b := c.start("b")
	n1 := c.join("n1")
	sb := c.create(nil)
	c.run(nil, sb, "echo data > keep")

	c.pause(a, sb)
	if row := c.row(sb.id); row.ClientState != model.StatePaused || row.Node != "n1" || row.PausedAt.IsZero() {
		t.Fatalf("after the pause %+v", row)
	}
	podState(t, n1, sb.id, podv1.PodState_POD_STATE_SUSPENDED, 5*time.Second)
	if r := c.call(b, "POST", sb.path("/resume"), projectAuth, ""); r.code != 200 || !strings.Contains(r.body, `"clientState":"paused"`) {
		t.Fatalf("resume: %d %s", r.code, r.body)
	}
	if row := c.row(sb.id); row.ClientState != model.StateRunning || row.Op != "" {
		t.Fatalf("after the resume %+v", row)
	}
	podState(t, n1, sb.id, podv1.PodState_POD_STATE_RUNNING, 5*time.Second)

	c.pause(b, sb)
	podState(t, n1, sb.id, podv1.PodState_POD_STATE_SUSPENDED, 5*time.Second)
	if out := c.run(a, sb, "cat keep"); out != "data\n" {
		t.Fatalf("after the implicit resume: %q", out)
	}
	if row := c.row(sb.id); row.ClientState != model.StateRunning {
		t.Fatalf("after the implicit resume %+v", row)
	}
	// A pause of a paused sandbox has nothing to do.
	c.pause(a, sb)
	c.pause(b, sb)
	if row := c.row(sb.id); row.ClientState != model.StatePaused {
		t.Fatalf("after two pauses %+v", row)
	}
}

// guard counts, per pod, the execs a test node runs in its guests and the
// suspends it gets, and flags a suspend of a pod while an exec runs in it.
type guard struct {
	violations atomic.Int32

	mu          sync.Mutex
	inflight    map[string]int
	suspends    map[string]int
	lastSuspend map[string]time.Time
}

// slowPrefix starts the commands the guard answers itself: they take
// the duration that follows ("e2e-slow 1500ms"), then print "slow done".
const slowPrefix = "e2e-slow "

func newGuard(n *node) *guard {
	g := &guard{inflight: map[string]int{}, suspends: map[string]int{}, lastSuspend: map[string]time.Time{}}
	n.Script().SetExec(func(ctx context.Context, pod podid.ID, req *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		argv := req.GetCommand()
		cmd := argv[len(argv)-1]
		if !strings.HasPrefix(cmd, slowPrefix) {
			return 0, false, nil
		}
		d, err := time.ParseDuration(strings.TrimPrefix(cmd, slowPrefix))
		if err != nil {
			return 0, false, err
		}
		g.mu.Lock()
		g.inflight[pod.String()]++
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			g.inflight[pod.String()]--
			g.mu.Unlock()
		}()
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
		return 0, true, out.Stdout([]byte("slow done\n"))
	})
	n.Script().SetHook(podv1.PodService_SuspendPod_FullMethodName, func(_ context.Context, _ string, req any) error {
		pod := req.(*podv1.SuspendPodRequest).GetPodId()
		g.mu.Lock()
		defer g.mu.Unlock()
		g.suspends[pod]++
		g.lastSuspend[pod] = time.Now()
		if g.inflight[pod] > 0 {
			g.violations.Add(1)
		}
		return nil
	})
	return g
}

// suspended is the number of suspends of the pod, and the time of the
// last.
func (g *guard) suspended(id podid.ID) (int, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.suspends[id.String()], g.lastSuspend[id.String()]
}

// lockWaiters is the number of statements of the database that wait for a
// lock on a sandbox row with lock: "FOR UPDATE" (an op's begin, a Sync) or
// "FOR NO KEY UPDATE" (a hold's admission).
func (c *cluster) lockWaiters(lock string) int {
	return c.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
		AND wait_event_type = 'Lock' AND query LIKE '%FROM sandboxes WHERE id = $1 '||$1`, lock)
}

// Scenario 3: the leader pauses an idle sandbox; an idle pause and an
// admission interleaved at the sandbox's row lock (the test holds it while
// both queue up, in either order) never suspend a running exec; a long
// exec is not paused right after it ends.
func TestIdlePause(t *testing.T) {
	t.Parallel()
	const idle = time.Second
	c := newCluster(t, func(cfg *config.Config) { cfg.IdlePause = idle })
	c.start("a")
	c.start("b")
	n1 := c.join("n1")
	g := newGuard(n1)

	// The leader pauses an idle sandbox, and the next call resumes it.
	sb := c.create(nil)
	c.run(nil, sb, "true")
	c.waitState(sb.id, model.StatePaused, 30*time.Second)
	podState(t, n1, sb.id, podv1.PodState_POD_STATE_SUSPENDED, 5*time.Second)
	if out := c.run(nil, sb, "echo awake"); out != "awake\n" {
		t.Fatalf("after the idle pause: %q", out)
	}

	for _, pauseFirst := range []bool{true, false} {
		sb := c.create(nil)
		c.run(nil, sb, "true")
		// The row is held before the sandbox is idle, so that the idle
		// pause's begin and the exec's admission queue behind the test.
		tx, err := c.db.Pool.Begin(bg)
		if err != nil {
			t.Fatal(err)
		}
		// A check that fails below ends the test before the rollback, and
		// the cleanup's close of the pool would wait for this connection.
		defer func() { _ = tx.Rollback(bg) }()
		if _, err := tx.Exec(bg, "SELECT 1 FROM sandboxes WHERE id = $1 FOR UPDATE", sb.id); err != nil {
			t.Fatal(err)
		}
		locked := time.Now()
		// The idle loop runs every second: two seconds past the idle time
		// its pause's begin waits. A Sync after a late pod event of the
		// create may wait before it, with the same lock; it changes
		// nothing. Every wait ends within the lock timeout (5 s).
		idleWaits := func() bool {
			return time.Since(locked) > idle+2*time.Second && c.lockWaiters("FOR UPDATE") >= 1
		}
		admissionWaits := func() bool { return c.lockWaiters("FOR NO KEY UPDATE") >= 1 }
		type result struct {
			r   reply
			err error
		}
		done := make(chan result, 1)
		execute := func() {
			go func() {
				r, err := c.execErr(bg, nil, sb, slowPrefix+"1500ms")
				done <- result{r, err}
			}()
		}
		if pauseFirst {
			eventually(t, 4*time.Second, "the idle pause waiting", idleWaits)
			execute()
			eventually(t, 1500*time.Millisecond, "the admission waiting", admissionWaits)
		} else {
			execute()
			eventually(t, 2*time.Second, "the admission waiting", admissionWaits)
			eventually(t, 4*time.Second, "the idle pause waiting", idleWaits)
		}
		if err := tx.Rollback(bg); err != nil {
			t.Fatal(err)
		}
		res := <-done
		if res.err != nil {
			t.Fatalf("pause first %v: %v", pauseFirst, res.err)
		}
		if out, err := decodeExec(res.r); err != nil || out.Output != "slow done\n" {
			t.Fatalf("pause first %v: %+v %v", pauseFirst, out, err)
		}
		if n, _ := g.suspended(sb.id); pauseFirst && n == 0 {
			t.Error("the idle pause did not run first")
		}
		if v := g.violations.Load(); v != 0 {
			t.Fatalf("pause first %v: %d suspends while an exec ran", pauseFirst, v)
		}
	}

	// A long exec holds the sandbox; when it ends, the idle time starts
	// anew.
	sb = c.create(nil)
	c.run(nil, sb, "true")
	begun := time.Now()
	if out := c.run(nil, sb, slowPrefix+"3s"); out != "slow done\n" {
		t.Fatalf("long exec: %q", out)
	}
	if n, _ := g.suspended(sb.id); n != 0 {
		t.Fatal("a suspend during the long exec")
	}
	// The hold's release at the end set the last activity (the database's
	// clock is the test's: one machine).
	last := c.row(sb.id).LastActiveAt
	if last.Before(begun.Add(3*time.Second - 200*time.Millisecond)) {
		t.Errorf("last activity %v, the exec began %v", last, begun)
	}
	c.waitState(sb.id, model.StatePaused, 30*time.Second)
	if _, at := g.suspended(sb.id); at.Sub(last) < idle-100*time.Millisecond {
		t.Errorf("paused %v after the long exec ended; the idle time is %v", at.Sub(last), idle)
	}
	if v := g.violations.Load(); v != 0 {
		t.Fatalf("%d suspends while an exec ran", v)
	}
}

// Scenario 12: a process started with timeout 0 keeps its sandbox awake
// past the idle time; process_stop releases it, and the idle pause
// follows.
func TestProcessHold(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process tools need it")
	}
	const idle = time.Second
	c := newCluster(t, func(cfg *config.Config) { cfg.IdlePause = idle })
	c.start("a")
	c.start("b")
	n1 := c.join("n1")
	g := newGuard(n1)
	root := t.TempDir()
	// The process tools' records go to a directory of the test: the test
	// node runs the commands on the host.
	n1.Script().SetExec(func(_ context.Context, _ podid.ID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		if argv := req.Command; len(argv) > 2 && argv[0] == "python3" {
			argv[2] = strings.ReplaceAll(argv[2], processRoot, root)
		}
		return 0, false, nil
	})
	sb := c.create(nil)
	cs := c.mcpSession(lbURL+"/mcp", sb.token, nil)
	text, isError, err := callTool(cs, "process_start", map[string]any{"command": "sleep 120", "timeout": 0, "name": "dev"})
	if err != nil || isError {
		t.Fatalf("process_start: %q %v %v", text, isError, err)
	}
	var handle struct {
		ID string `json:"id"`
	}
	line, _, _ := strings.Cut(text, "\n")
	if err := json.Unmarshal([]byte(line), &handle); err != nil || handle.ID == "" {
		t.Fatalf("process_start answered %q", text)
	}
	if n := c.count("SELECT count(*) FROM sandbox_holds WHERE sandbox_id = $1 AND kind = 'process' AND lease_until = 'infinity'",
		sb.id); n != 1 {
		t.Fatalf("%d process holds without a limit", n)
	}
	never(t, 5*idle, "the sandbox paused with its process running", func() bool {
		n, _ := g.suspended(sb.id)
		return n != 0 || c.row(sb.id).ClientState != model.StateRunning
	})
	text, isError, err = callTool(cs, "process_stop", map[string]any{"id": handle.ID})
	if err != nil || isError || text != "process "+handle.ID+" stopped\n" {
		t.Fatalf("process_stop: %q %v %v", text, isError, err)
	}
	c.waitState(sb.id, model.StatePaused, 30*time.Second)
	if n := c.count("SELECT count(*) FROM sandbox_holds WHERE sandbox_id = $1", sb.id); n != 0 {
		t.Fatalf("%d holds left", n)
	}
}

// hibernated brings a sandbox to hibernated: it runs, is paused over
// REST, and the leader hibernates it (--hibernate-after).
func (c *cluster) hibernated(n *node, sb sandbox) {
	c.t.Helper()
	c.run(nil, sb, "echo data > keep")
	c.pause(nil, sb)
	eventually(c.t, 60*time.Second, "the sandbox hibernated", func() bool {
		c.wake("sandbox-hibernate-after")
		row := c.row(sb.id)
		return row.ClientState == model.StateHibernated && row.Op == ""
	})
	podState(c.t, n, sb.id, podv1.PodState_POD_STATE_HIBERNATED, 5*time.Second)
}

// slowRestore makes the node's RestorePod take d after the pod went
// PENDING; started receives once per RestorePod that began.
func slowRestore(n *node, d time.Duration) <-chan struct{} {
	started := make(chan struct{}, 8)
	n.Script().SetDelay(podv1.PodService_RestorePod_FullMethodName, d)
	n.Script().SetHook(podv1.PodService_RestorePod_FullMethodName, func(context.Context, string, any) error {
		select {
		case started <- struct{}{}:
		default:
		}
		return nil
	})
	return started
}

// Scenario 4: a REST client that hangs up 1 s into a slow RestorePod
// cancels nothing: the node, which aborts a cancelled call as today's
// feosd does, completes the restore, and the sandbox runs on its disk.
func TestRestoreOutlivesClient(t *testing.T) {
	t.Parallel()
	c := newCluster(t, func(cfg *config.Config) { cfg.HibernateAfter = time.Second })
	c.start("a")
	c.start("b")
	n1 := c.join("n1", func(cfg *testnode.Config) { cfg.AbortOnCancel = true })
	sb := c.create(nil)
	c.hibernated(n1, sb)

	started := slowRestore(n1, 3*time.Second)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		r, err := c.execErr(ctx, nil, sb, "cat keep")
		if err == nil {
			err = fmt.Errorf("the exec answered: %d %s", r.code, r.body)
		}
		answered <- err
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("no RestorePod began")
	}
	time.Sleep(time.Second)
	cancel()
	if err := <-answered; !errors.Is(err, context.Canceled) {
		t.Fatalf("the client's exec: %v", err)
	}
	c.waitState(sb.id, model.StateRunning, 30*time.Second)
	podState(t, n1, sb.id, podv1.PodState_POD_STATE_RUNNING, 5*time.Second)
	if out := c.run(nil, sb, "cat keep"); out != "data\n" {
		t.Fatalf("the restored disk: %q", out)
	}
}

// Scenario 4: the node connection drops in the middle of a slow restore
// (the node restarts); the op finds the pod again on the new connection,
// and the tool call that waited for it succeeds.
func TestNodeDropMidOp(t *testing.T) {
	t.Parallel()
	c := newCluster(t, func(cfg *config.Config) { cfg.HibernateAfter = time.Second })
	c.start("a")
	c.start("b")
	n1 := c.join("n1")
	sb := c.create(nil)
	c.hibernated(n1, sb)

	started := slowRestore(n1, 3*time.Second)
	type result struct {
		r   reply
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := c.execErr(bg, nil, sb, "cat keep")
		done <- result{r, err}
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("no RestorePod")
	}
	time.Sleep(time.Second)
	n1.Restart(false)
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if out, err := decodeExec(res.r); err != nil || out.Output != "data\n" {
		t.Fatalf("exec across the drop: %+v %v", out, err)
	}
	if row := c.row(sb.id); row.ClientState != model.StateRunning || row.Node != "n1" {
		t.Fatalf("row %+v", row)
	}
}
