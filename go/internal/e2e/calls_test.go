// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// Tool calls that outlive their caller (ADR 0023): a call with an id runs
// on in the instance that took it when its client hangs up; any instance
// reads its record, attaches to it or answers it again; only an explicit
// cancel stops it; the call of an instance cut off its database, or of a
// stopping one, ends with an exact verdict (interrupted when it may have
// run, not_run when it cannot have); and its record goes after the
// retention. Every cluster here has a call lease of callLease, so that the
// verdicts on a lost instance come within seconds.

// callLease is the call lease of every cluster in this file.
const callLease = 3 * time.Second

// The REST headers of a call with an id.
const (
	callStateHeader    = "Ferun-Call-State"
	callReplayedHeader = "Ferun-Call-Replayed"
)

// newCallCluster is newCluster with the call lease of this file; tune
// changes the instances' configuration further.
func newCallCluster(t *testing.T, tune ...func(*config.Config)) *cluster {
	t.Helper()
	return newCluster(t, func(cfg *config.Config) {
		cfg.CallLease = callLease
		for _, f := range tune {
			f(cfg)
		}
	})
}

// keyedExec is execErr as the call with the id key (the Idempotency-Key
// header), with timeout seconds (0: the tool's default).
func (c *cluster) keyedExec(ctx context.Context, in *instance, sb sandbox, command, key string, timeout int) (reply, error) {
	if in == nil {
		if in = c.pick(); in == nil {
			return reply{}, errors.New("no instance runs")
		}
	}
	p := map[string]any{"command": command}
	if timeout > 0 {
		p["timeout"] = timeout
	}
	b, err := json.Marshal(p)
	if err != nil {
		return reply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", in.url(sb.path("/exec")), bytes.NewReader(b))
	if err != nil {
		return reply{}, err
	}
	req.Header.Set("Authorization", sb.auth())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := plainClient.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return reply{}, err
	}
	return reply{code: resp.StatusCode, body: string(body), header: resp.Header}, nil
}

// keyedExecAsync is keyedExec in the background, on a context that is
// never cancelled.
func (c *cluster) keyedExecAsync(in *instance, sb sandbox, command, key string, timeout int) <-chan timedReply {
	done := make(chan timedReply, 1)
	go func() {
		start := time.Now()
		r, err := c.keyedExec(bg, in, sb, command, key, timeout)
		done <- timedReply{r, err, time.Since(start)}
	}()
	return done
}

// hangUp sends command through in as the call with the id key and hangs
// up once the node began its exec (started, from markExecs): the client's
// request ends with its context's error, and the call runs on.
func (c *cluster) hangUp(in *instance, sb sandbox, command, key string, started <-chan struct{}) {
	c.t.Helper()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	type answer struct {
		r   reply
		err error
	}
	done := make(chan answer, 1)
	go func() {
		r, err := c.keyedExec(ctx, in, sb, command, key, 0)
		done <- answer{r, err}
	}()
	select {
	case <-started:
	case a := <-done:
		c.t.Fatalf("%s: answered before its exec began: %d %s (%v)", key, a.r.code, a.r.body, a.err)
	case <-time.After(30 * time.Second):
		c.t.Fatalf("%s: its exec did not begin", key)
	}
	cancel()
	if a := <-done; !errors.Is(a.err, context.Canceled) {
		c.t.Fatalf("%s: the request that hung up got %d %s (%v), want the client's context error", key, a.r.code, a.r.body, a.err)
	}
}

// callRecord is the record of a call with an id, as GET …/calls/{key}
// answers it; the outcome's fields are zero while it runs.
type callRecord struct {
	Key        string `json:"key"`
	Tool       string `json:"tool"`
	State      string `json:"state"`
	Instance   string `json:"instance"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	ExitCode   int    `json:"exitCode"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	IsError    bool   `json:"isError"`
	Error      string `json:"error"`
	Text       string `json:"text"`
}

// record reads the record of the sandbox's call with the id key through
// in, waiting up to wait seconds for the call's end.
func (c *cluster) record(in *instance, sb sandbox, key string, wait int) reply {
	c.t.Helper()
	return c.call(in, "GET", sb.path("/calls/"+key+"?wait="+strconv.Itoa(wait)), sb.auth(), "")
}

// recordOf is record decoded; anything but a 200 fails the test.
func (c *cluster) recordOf(in *instance, sb sandbox, key string, wait int) callRecord {
	c.t.Helper()
	r := c.record(in, sb, key, wait)
	if r.code != http.StatusOK {
		c.t.Fatalf("the record of %s on %s: %d %s", key, in.name, r.code, r.body)
	}
	var rec callRecord
	if err := json.Unmarshal([]byte(r.body), &rec); err != nil {
		c.t.Fatalf("the record of %s on %s: %v: %s", key, in.name, err, r.body)
	}
	return rec
}

// wakeCalls wakes the leader's loops of the calls with an id every second
// until the returned function is called, as an instance's own writes do;
// a failed wake is left for the next.
func (c *cluster) wakeCalls() (stop func()) {
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			_ = db.Notify(ctx, c.db.Pool, db.ChanLeader, tools.HintCalls)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// inPod reports whether the file name exists in the sandbox's root on the
// node, where its commands run.
func inPod(n *node, sb sandbox, name string) bool {
	_, err := os.Stat(filepath.Join(n.PodDir(sb.id), name))
	return err == nil
}

// marked waits for a signal of markExecs.
func marked(t *testing.T, started <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not begin", what)
	}
}

// callToolKeyed calls a tool over MCP as the call with the id key
// (_meta "ferun.callId").
func callToolKeyed(ctx context.Context, cs *mcp.ClientSession, tool string, args map[string]any, key string) (*mcp.CallToolResult, error) {
	return cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: mcp.Meta{"ferun.callId": key}})
}

// toolText is the text of a tool result.
func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// expectCallMeta checks the _meta of a keyed MCP result.
func expectCallMeta(t *testing.T, what string, res *mcp.CallToolResult, key, state string, replayed bool) {
	t.Helper()
	if res.Meta["ferun.callId"] != key || res.Meta["ferun.callState"] != state || res.Meta["ferun.callReplayed"] != replayed {
		t.Fatalf("%s: _meta %v, want call %s state %s replayed %v", what, res.Meta, key, state, replayed)
	}
}

// expectKeyedOutput checks a keyed exec's answer: a 200 with exit code 0,
// output want, state done, and the replay header when replayed.
func expectKeyedOutput(t *testing.T, what string, r reply, err error, want string, replayed bool) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	out, err := decodeExec(r)
	if err != nil || out.ExitCode != 0 || out.Output != want {
		t.Fatalf("%s: %+v %v, want %q", what, out, err, want)
	}
	if got := r.header.Get(callStateHeader); got != tools.CallDone {
		t.Fatalf("%s: %s %q, want %q", what, callStateHeader, got, tools.CallDone)
	}
	wantReplayed := ""
	if replayed {
		wantReplayed = "true"
	}
	if got := r.header.Get(callReplayedHeader); got != wantReplayed {
		t.Fatalf("%s: %s %q, want %q", what, callReplayedHeader, got, wantReplayed)
	}
}

// A call with an id outlives its caller: the client hangs up, the call
// runs to its end on the instance that took it, another instance reads its
// record and answers the same request again from it without running the
// command once more, and refuses the id for another command.
func TestCallOutlivesCaller(t *testing.T) {
	t.Parallel()
	c := newCallCluster(t)
	a := c.start("a")
	b := c.start("b")
	n1 := c.join("n1")
	sb := c.create(b)
	c.run(b, sb, "true")

	const cmd = "sleep 2; echo once >> calls.txt; echo out # e2e-k1"
	c.hangUp(a, sb, cmd, "k1", markExecs(n1, "e2e-k1"))

	rec := c.recordOf(b, sb, "k1", 30)
	if rec.State != tools.CallDone || rec.Text != "out\n" || rec.Output != "out\n" || rec.Error != "" || rec.Instance != a.ID() {
		t.Fatalf("outlives its caller: the record on b %+v, want done with out on %s", rec, a.ID())
	}
	r, err := c.keyedExec(bg, b, sb, cmd, "k1", 0)
	expectKeyedOutput(t, "outlives its caller: the same call again on b", r, err, "out\n", true)
	if out := c.run(b, sb, "wc -l < calls.txt"); out != "1\n" {
		t.Fatalf("outlives its caller: calls.txt has %q lines, want 1: the call ran again", out)
	}
	r, err = c.keyedExec(bg, b, sb, "echo other # e2e-k1", "k1", 0)
	if err != nil || r.code != http.StatusUnprocessableEntity || !strings.Contains(r.body, `"call_id_conflict"`) {
		t.Fatalf("outlives its caller: the id with another command: %d %s (%v), want 422 call_id_conflict", r.code, r.body, err)
	}
}

// A client that sent a call with an id over REST and hung up attaches to
// it over MCP at another instance, under the same id: REST and MCP hash
// the same call equally, and the MCP answer is the call's own, replayed.
func TestCallReattachOverMCP(t *testing.T) {
	t.Parallel()
	c := newCallCluster(t)
	a := c.start("a")
	b := c.start("b")
	n1 := c.join("n1")
	sb := c.create(b)
	c.run(b, sb, "true")

	const cmd = "sleep 3; echo mcp # e2e-k2"
	c.hangUp(a, sb, cmd, "k2", markExecs(n1, "e2e-k2"))

	cs := c.mcpSession(b.url("/mcp"), sb.token, nil)
	ctx, cancel := context.WithTimeout(bg, 2*time.Minute)
	defer cancel()
	res, err := callToolKeyed(ctx, cs, "bash", map[string]any{"command": cmd}, "k2")
	if err != nil || res.IsError || toolText(res) != "mcp\n" {
		t.Fatalf("reattach over MCP: %+v %v, want the call's own output mcp", res, err)
	}
	expectCallMeta(t, "reattach over MCP", res, "k2", tools.CallDone, true)
}

// Only an explicit cancel stops a call with an id: (a) a hang-up of the
// discover path, which has no session to cancel in, lets the call run to
// its end; (b) an initialize client's cancel through the SDK stops it in
// the guest, through whichever instance the cancel reaches; (c) REST's
// DELETE …/calls/{key} at another instance stops it there.
func TestCallCancelPaths(t *testing.T) {
	t.Parallel()
	c := newCallCluster(t)
	a := c.start("a")
	b := c.start("b")
	n1 := c.join("n1")
	sb := c.create(b)
	c.run(b, sb, "true")

	// (a) A hang-up is not a cancel.
	started := markExecs(n1, "e2e-k3a")
	cs := c.mcpSession(lbURL+"/mcp", sb.token, nil)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() {
		_, err := callToolKeyed(ctx, cs, "bash", map[string]any{"command": "sleep 4; echo x > d.txt # e2e-k3a"}, "k3a")
		done <- err
	}()
	marked(t, started, "hang-up over MCP: the exec of k3a")
	begun := time.Now()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("hang-up over MCP: the abandoned call returned no error")
	}
	if rec := c.recordOf(b, sb, "k3a", 30); rec.State != tools.CallDone || rec.Error != "" || rec.ExitCode != 0 {
		t.Fatalf("hang-up over MCP: the record %+v, want done without an error", rec)
	}
	time.Sleep(time.Until(begun.Add(6 * time.Second)))
	if !inPod(n1, sb, "d.txt") {
		t.Fatal("hang-up over MCP: no d.txt after 6 s: the hang-up stopped the call")
	}

	// (b) An initialize client's cancel, through the balancer.
	started = markExecs(n1, "e2e-k3b")
	cs = c.mcpSession(lbURL+"/mcp", sb.token, legacy)
	ctx, cancel = context.WithCancel(bg)
	go func() {
		_, err := callToolKeyed(ctx, cs, "bash", map[string]any{"command": "sleep 5; echo y > l.txt # e2e-k3b"}, "k3b")
		done <- err
	}()
	marked(t, started, "MCP cancel: the exec of k3b")
	begun = time.Now()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("MCP cancel: the cancelled call returned no error")
	}
	if rec := c.recordOf(b, sb, "k3b", 30); rec.State != tools.CallDone || !strings.Contains(rec.Error, "cancelled by its caller") {
		t.Fatalf("MCP cancel: the record %+v, want done, cancelled by its caller", rec)
	}
	time.Sleep(time.Until(begun.Add(7 * time.Second)))
	if inPod(n1, sb, "l.txt") {
		t.Fatal("MCP cancel: l.txt after 7 s: the cancel did not stop the command")
	}

	// (c) REST's cancel, at an instance that does not run the call.
	c.hangUp(a, sb, "sleep 5; echo z > r.txt # e2e-k3c", "k3c", markExecs(n1, "e2e-k3c"))
	begun = time.Now()
	if r := c.call(b, "DELETE", sb.path("/calls/k3c"), sb.auth(), ""); r.code != http.StatusAccepted || !strings.Contains(r.body, `"cancelled":true`) {
		t.Fatalf("REST cancel: DELETE on b: %d %s, want 202", r.code, r.body)
	}
	if rec := c.recordOf(b, sb, "k3c", 30); rec.State != tools.CallDone || !strings.Contains(rec.Error, "cancelled by its caller") {
		t.Fatalf("REST cancel: the record %+v, want done, cancelled by its caller", rec)
	}
	time.Sleep(time.Until(begun.Add(7 * time.Second)))
	if inPod(n1, sb, "r.txt") {
		t.Fatal("REST cancel: r.txt after 7 s: the cancel did not stop the command")
	}
}

// partitioned starts the cluster of the partition tests: b leads, n1 joins
// while only b runs (so that its uplink goes to b, which the cut leaves
// alone), and a runs on its database through a proxy that can cut it off.
// The sandbox runs on n1.
func partitioned(t *testing.T) (c *cluster, a, b *instance, n1 *node, proxy *dbProxy, sb sandbox) {
	t.Helper()
	c = newCallCluster(t)
	b = c.start("b")
	eventually(t, 30*time.Second, "b leading", b.Elector.IsLeader)
	n1 = c.join("n1")
	proxy = c.newDBProxy()
	a = c.start("a", func(cfg *config.Config) { cfg.DatabaseURL = proxy.dsn })
	c.waitNode(n1)
	sb = c.create(b)
	c.run(b, sb, "true")
	return c, a, b, n1, proxy, sb
}

// awaitState reads the record of key on in, waiting up to 5 s each time,
// while the leader's loops are woken every second, until the call's state
// is no longer running; within bounds the whole wait.
func (c *cluster) awaitState(what string, in *instance, sb sandbox, key string, within time.Duration) callRecord {
	c.t.Helper()
	stop := c.wakeCalls()
	defer stop()
	deadline := time.Now().Add(within)
	for {
		rec := c.recordOf(in, sb, key, 5)
		if rec.State != tools.CallRunning {
			return rec
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%s: the call still runs after %v: %+v", what, within, rec)
		}
	}
}

// An instance cut off its database while its call with an id runs in the
// guest: the leader (or a waiter) ends the row interrupted once its lease
// ran out, naming the instance, and the call never completes afterwards.
// The cut here is shorter than the instance TTL, so n1 keeps a; the call
// stops because, once healed, a's renewal misses the row and cancels it
// (a longer cut would end it earlier: n1 drops an instance whose row is
// stale, and with it the exec stream). The tools unit tests isolate the
// renewal's miss. A re-send gets the stored verdict.
func TestCallInstancePartitioned(t *testing.T) {
	t.Parallel()
	c, a, b, n1, proxy, sb := partitioned(t)

	const cmd = "sleep 20; echo late > died.txt # e2e-k4"
	started := markExecs(n1, "e2e-k4")
	onA := c.keyedExecAsync(a, sb, cmd, "k4", 0)
	marked(t, started, "partitioned: the exec of k4")
	begun := time.Now()

	proxy.cutOff()
	cut := time.Now()
	rec := c.awaitState("partitioned", b, sb, "k4", callLease+15*time.Second)
	if took := time.Since(cut); took > callLease+15*time.Second {
		t.Fatalf("partitioned: the verdict came %v after the cut; at most %v", took, callLease+15*time.Second)
	}
	if rec.State != tools.CallInterrupted || !strings.Contains(rec.Error, a.ID()) {
		t.Fatalf("partitioned: the record %+v, want interrupted naming %s", rec, a.ID())
	}

	proxy.heal()
	select {
	case res := <-onA:
		if res.err != nil || res.r.code == http.StatusOK || res.r.header.Get(callStateHeader) != tools.CallInterrupted {
			t.Fatalf("partitioned: the caller at a got %d %s %s (%v), want interrupted", res.r.code, res.r.header.Get(callStateHeader),
				res.r.body, res.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("partitioned: the caller at a got no answer after the heal")
	}
	never(t, time.Until(begun.Add(25*time.Second)), "partitioned: died.txt appeared: the call ran on after its verdict", func() bool {
		return inPod(n1, sb, "died.txt")
	})

	cs := c.mcpSession(b.url("/mcp"), sb.token, nil)
	ctx, cancel := context.WithTimeout(bg, 2*time.Minute)
	defer cancel()
	res, err := callToolKeyed(ctx, cs, "bash", map[string]any{"command": cmd}, "k4")
	if err != nil || !res.IsError || !strings.Contains(toolText(res), "interrupted") {
		t.Fatalf("partitioned: the MCP re-send: %+v %v, want the stored interrupted", res, err)
	}
	expectCallMeta(t, "partitioned: the MCP re-send", res, "k4", tools.CallInterrupted, true)
}

// An instance cut off its database before its call with an id was sent
// (the call waits for the sandbox's resume): the row ends not_run, the
// cut-off instance never sends the call afterwards, and the same call
// sent again runs, once.
func TestCallNotSentWhenPartitioned(t *testing.T) {
	t.Parallel()
	c, a, b, n1, proxy, sb := partitioned(t)
	c.pause(b, sb)
	n1.Script().SetDelay(podv1.PodService_ResumePod_FullMethodName, 20*time.Second)
	// The resume is in flight once the node has it: before, a's op still
	// prepares the node from the database (the sandbox's groups and names),
	// which the cut would fail, and nothing would resume it.
	var resumed atomic.Bool
	n1.Script().SetHook(podv1.PodService_ResumePod_FullMethodName, func(context.Context, string, any) error {
		resumed.Store(true)
		return nil
	})

	const cmd = "echo once >> sent.txt # e2e-k5"
	onA := c.keyedExecAsync(a, sb, cmd, "k5", 0)
	eventually(t, 30*time.Second, "not sent: k5 recorded and a's resume in flight", func() bool {
		if c.count(`SELECT count(*) FROM tool_calls WHERE sandbox_id = $1 AND call_key = 'k5' AND state = 'running'`, sb.id) != 1 {
			return false
		}
		row := c.row(sb.id)
		return row.Op == "resume" && row.OpOwner == a.ID() && resumed.Load()
	})

	proxy.cutOff()
	rec := c.awaitState("not sent", b, sb, "k5", 60*time.Second)
	if rec.State != tools.CallNotRun || !strings.Contains(rec.Error, a.ID()) {
		t.Fatalf("not sent: the record %+v, want not_run naming %s", rec, a.ID())
	}

	proxy.heal()
	select {
	case res := <-onA:
		if res.err != nil || res.r.code == http.StatusOK || res.r.header.Get(callStateHeader) != tools.CallNotRun {
			t.Fatalf("not sent: the caller at a got %d %s %s (%v), want not_run", res.r.code, res.r.header.Get(callStateHeader),
				res.r.body, res.err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("not sent: the caller at a got no answer after the heal")
	}
	c.waitState(sb.id, model.StateRunning, 90*time.Second)
	r, err := c.keyedExec(bg, b, sb, cmd, "k5", 0)
	expectKeyedOutput(t, "not sent: the same call again on b", r, err, "", false)
	if out := c.run(b, sb, "wc -l < sent.txt"); out != "1\n" {
		t.Fatalf("not sent: sent.txt has %q lines, want 1", out)
	}
}

// A stopping instance lets its call with an id, whose caller left, end
// before it stops; the record then holds the call's result.
func TestCallDrainFinishes(t *testing.T) {
	t.Parallel()
	c := newCallCluster(t)
	a := c.start("a", func(cfg *config.Config) { cfg.ReadinessDelay = 200 * time.Millisecond })
	b := c.start("b")
	n1 := c.join("n1")
	sb := c.create(b)
	c.run(b, sb, "true")

	c.hangUp(a, sb, "sleep 3; echo drained # e2e-k6", "k6", markExecs(n1, "e2e-k6"))
	begun := time.Now()
	if err := a.stop(); err != nil {
		t.Fatalf("drain finishes: stop: %v", err)
	}
	if took := time.Since(begun); took < 2500*time.Millisecond {
		t.Fatalf("drain finishes: a stopped after %v, before its call could end", took)
	}
	if rec := c.recordOf(b, sb, "k6", 0); rec.State != tools.CallDone || rec.Text != "drained\n" || rec.Error != "" {
		t.Fatalf("drain finishes: the record %+v, want done with drained", rec)
	}
}

// The send gate: a stopping instance does not send a call with an id that
// would not end before its shutdown grace (timeout 60 s and the margin, on
// a grace of 20 s). The caller gets not_run with a Retry-After, and the
// same call sent to another instance runs there.
func TestCallDrainGate(t *testing.T) {
	t.Parallel()
	c := newCallCluster(t)
	a := c.start("a", func(cfg *config.Config) {
		cfg.ShutdownGrace = 20 * time.Second
		cfg.ReadinessDelay = 200 * time.Millisecond
	})
	b := c.start("b")
	n1 := c.join("n1")
	sb := c.create(b)
	c.run(b, sb, "true")
	c.pause(b, sb)
	n1.Script().SetDelay(podv1.PodService_ResumePod_FullMethodName, 5*time.Second)

	const cmd = "echo gated # e2e-k7"
	onA := c.keyedExecAsync(a, sb, cmd, "k7", 60)
	// The call is in: recorded, and waiting for the resume, which ends
	// after the drain began.
	eventually(t, 30*time.Second, "drain gate: k7 recorded and a's resume in flight", func() bool {
		if c.count(`SELECT count(*) FROM tool_calls WHERE sandbox_id = $1 AND call_key = 'k7' AND state = 'running'`, sb.id) != 1 {
			return false
		}
		row := c.row(sb.id)
		return row.Op == "resume" && row.OpOwner == a.ID()
	})
	stopped := a.stopAsync()

	select {
	case res := <-onA:
		if res.err != nil || res.r.code != http.StatusBadGateway || res.r.header.Get(callStateHeader) != tools.CallNotRun ||
			res.r.header.Get("Retry-After") == "" {
			t.Fatalf("drain gate: the caller at a got %d %s=%q Retry-After=%q %s (%v), want 502 not_run with Retry-After",
				res.r.code, callStateHeader, res.r.header.Get(callStateHeader), res.r.header.Get("Retry-After"), res.r.body, res.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("drain gate: the caller at a got no answer")
	}
	r, err := c.keyedExec(bg, b, sb, cmd, "k7", 60)
	expectKeyedOutput(t, "drain gate: the same call again on b", r, err, "gated\n", false)
	if err := <-stopped; err != nil {
		t.Fatalf("drain gate: stop: %v", err)
	}
}

// A stopping instance cuts the calls with an id still running at its
// shutdown grace (1 s here): the call ends interrupted, with its output so
// far, and the instance stops within the cut's bound.
func TestCallDrainCuts(t *testing.T) {
	t.Parallel()
	c := newCallCluster(t)
	a := c.start("a", func(cfg *config.Config) { cfg.ShutdownGrace = time.Second })
	b := c.start("b")
	n1 := c.join("n1")
	sb := c.create(b)
	c.run(b, sb, "true")

	c.hangUp(a, sb, "echo begun; sleep 30; echo never # e2e-k8", "k8", markExecs(n1, "e2e-k8"))
	begun := time.Now()
	if err := a.stop(); err != nil {
		t.Fatalf("drain cuts: stop: %v", err)
	}
	if took := time.Since(begun); took > 20*time.Second {
		t.Fatalf("drain cuts: a stopped after %v; at most 20 s", took)
	}
	rec := c.recordOf(b, sb, "k8", 0)
	if rec.State != tools.CallInterrupted || !strings.HasPrefix(rec.Text, "begun") || !strings.Contains(rec.Error, "shutdown grace") {
		t.Fatalf("drain cuts: the record %+v, want interrupted with begun, at the shutdown grace", rec)
	}
}

// The record of a call with an id goes once the retention passed after
// its end; the id is then free for another call.
func TestCallRetention(t *testing.T) {
	t.Parallel()
	c := newCallCluster(t, func(cfg *config.Config) { cfg.CallRetention = 2 * time.Second })
	a := c.start("a")
	c.join("n1")
	sb := c.create(a)

	r, err := c.keyedExec(bg, a, sb, "echo one", "k9", 0)
	expectKeyedOutput(t, "retention: the first call", r, err, "one\n", false)
	c.leader()
	time.Sleep(3 * time.Second)
	c.wake(tools.HintCalls)
	woken := time.Now()
	eventually(t, 30*time.Second, "retention: the record of k9 deleted", func() bool {
		if time.Since(woken) > time.Second {
			c.wake(tools.HintCalls)
			woken = time.Now()
		}
		return c.count(`SELECT count(*) FROM tool_calls WHERE sandbox_id = $1 AND call_key = 'k9'`, sb.id) == 0
	})
	r, err = c.keyedExec(bg, a, sb, "echo two", "k9", 0)
	expectKeyedOutput(t, "retention: another call under the freed id", r, err, "two\n", false)
}
