// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// A log budget cuts each line to its line bytes, the marker included and
// at a rune's start, and drops the oldest lines beyond its total.
func TestLogBudget(t *testing.T) {
	b := logBudget{line: 16, total: 40}
	if got := b.cut("short"); got != "short" {
		t.Errorf("cut %q", got)
	}
	if got := b.cut(strings.Repeat("x", 17)); got != "xxxxxxx"+cutMark || len(got) != 16 {
		t.Errorf("cut %q (%d bytes)", got, len(got))
	}
	// "é" is 2 bytes: the cut never splits it.
	if got := b.cut("abcdefé" + strings.Repeat("x", 20)); got != "abcdef"+cutMark {
		t.Errorf("cut a rune: %q", got)
	}
	lines := []string{"0123456789", "1123456789", strings.Repeat("y", 30), "3123456789", "4123456789"}
	got := b.keep(lines)
	want := []string{"yyyyyyy" + cutMark, "3123456789", "4123456789"}
	if !slices.Equal(got, want) {
		t.Errorf("keep %q, want %q", got, want)
	}
	if b.keep(nil) != nil || len(b.keep([]string{})) != 0 {
		t.Error("keep of nothing")
	}
	// The newest line stays, whatever its size.
	if got := (logBudget{line: 16, total: 4}).keep([]string{"a", strings.Repeat("z", 20)}); len(got) != 1 ||
		got[0] != "zzzzzzz"+cutMark {
		t.Errorf("keep over the total %q", got)
	}
	// Reading many lines holds no more than the budget's.
	tl := logTail{b: logBudget{line: 8, total: 64}, max: 1000}
	for i := range 10000 {
		tl.add(fmt.Sprintf("%07d", i))
	}
	if r := tl.result(); len(r) != 9 || r[8] != "0009999" || len(tl.lines) > 200 {
		t.Errorf("result %q, %d held", r, len(tl.lines))
	}
}

// bytesOf is what lines hold.
func bytesOf(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l)
	}
	return n
}

// A logs answer is bounded in bytes as well as lines (§13.7): each line
// cut to 8 KiB with the marker, the oldest dropped beyond 4 MiB, the
// newest kept.
func TestLogTailBytes(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, serviceSpec(1, "1", ""))
	h.waitRelease(env.ID, r1, model.ReleaseDeployed, 30*time.Second)
	id := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0].Sandbox.ID
	const n = 600
	for i := range n {
		if err := n1.Script().AppendLog(id, "main", fmt.Sprintf("%04d ", i)+strings.Repeat("x", 10<<10)); err != nil {
			t.Fatal(err)
		}
	}
	lines, source, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, maxLogLines)
	if err != nil || source != SourceContainer {
		t.Fatalf("logs: %v (%s)", err, source)
	}
	if total := bytesOf(lines); total > answerLog.total || len(lines) == 0 || len(lines) >= n {
		t.Fatalf("%d lines of %d bytes", len(lines), total)
	}
	for _, l := range lines {
		if len(l) > answerLog.line || !strings.HasSuffix(l, cutMark) {
			t.Fatalf("a line of %d bytes: %.20q…", len(l), l)
		}
	}
	if !strings.HasPrefix(lines[len(lines)-1], fmt.Sprintf("%04d ", n-1)) {
		t.Errorf("the newest line is not last: %.20q", lines[len(lines)-1])
	}
}

// checkStored fails unless a stored tail holds none of values, has the
// redaction mark and stays within storedLog.
func checkStored(t *testing.T, what string, tail []string, values ...string) {
	t.Helper()
	all := strings.Join(tail, "\n")
	for _, v := range values {
		if v == "" || strings.Contains(all, v) || strings.Contains(all, escapeUserinfo(v)) {
			t.Errorf("%s holds a secret value %q", what, v)
		}
	}
	if !strings.Contains(all, redactedMark) {
		t.Errorf("%s has nothing redacted: %.200q", what, all)
	}
	if total := bytesOf(tail); total > storedLog.total {
		t.Errorf("%s holds %d bytes", what, total)
	}
	for _, l := range tail {
		if len(l) > storedLog.line {
			t.Errorf("%s has a line of %d bytes", what, len(l))
			break
		}
	}
}

// longLines are lines of 64 KiB, which a stored tail cuts.
func longLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%02d ", i) + strings.Repeat("y", 64<<10)
	}
	return out
}

// A job's run keeps its log tail redacted of the addon credentials and
// the tenant secrets it was given, and bounded (§13.7); its job_failed
// evidence is the same tail.
func TestJobLogTailRedacted(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	execs := &execLog{}
	n1.Script().SetExec(execs.fn)
	env := h.newEnv("stage")
	const token = "tok/en+value-42"
	h.putSecret(env.ID, "token", token)
	r1 := h.apply(env.ID, fmt.Sprintf(`{"addons":{"postgres":{"type":"postgres","version":"17","plan":"dev",
		"options":{"databases":["app"],"roles":["svc"]}}},
		"jobs":{"migrate":{"image":%q,"command":["/app","migrate"],"env":{
			"DATABASE_URL":{"addon":"postgres","key":"url","role":"svc","database":"app"},"TOKEN":{"secret":"token"}}}}}`,
		testImage))
	id := runningJob(t, h, env.ID)
	pw, err := h.c.ResolveSecret(bg, env.ID, "addon.postgres.role.svc.password", 1)
	if err != nil || pw == "" {
		t.Fatalf("credential %q %v", pw, err)
	}
	if sb, _ := h.st.GetSandbox(bg, id); sb.PodTemplate == nil || sb.PodTemplate.Env["TOKEN"].Secret != "token" {
		t.Fatalf("the job's template has not the secret: %+v", sb.PodTemplate)
	}
	lines := append(longLines(48),
		"connecting to postgres://svc:"+pw+"@[fd00::1]:5432/app: refused",
		"TOKEN="+token+" escaped "+escapeUserinfo(token))
	for _, l := range lines {
		if err := n1.Script().AppendLog(id, "main", l); err != nil {
			t.Fatal(err)
		}
	}
	if err := n1.Script().ExitContainer(id, "main", 1); err != nil {
		t.Fatal(err)
	}
	h.waitRelease(env.ID, r1, model.ReleaseFailed, 30*time.Second)
	run, err := h.st.LatestJobRun(bg, env.ID, "migrate")
	if err != nil || run.Phase != model.RunFailed {
		t.Fatalf("run %+v %v", run, err)
	}
	checkStored(t, "the run's tail", run.LogTail, pw, token)
	if n := len(run.LogTail); n == 0 || run.LogTail[n-1] != "TOKEN="+redactedMark+" escaped "+redactedMark {
		t.Errorf("the newest line %q", run.LogTail)
	}
	inc := h.incident(env.ID, "job/migrate", model.ConditionJobFailed, 10*time.Second)
	var ev evidence
	if err := json.Unmarshal(inc.Evidence, &ev); err != nil || !slices.Equal(ev.LogTail, run.LogTail) {
		t.Fatalf("evidence %+v %v", ev, err)
	}
}

// exitRunning makes the service's running container exit with code once
// its row runs and its container is up, its log given lines first.
func exitRunning(t *testing.T, h *harness, n *testnode.Node, envID uuid.UUID, id podid.ID, code int, lines ...string) {
	t.Helper()
	eventually(t, 30*time.Second, "the container running", func() bool {
		p := h.pod("n1", id)
		for _, r := range h.rowsOf(envID, spec.KindService, "web") {
			if r.Sandbox.ID == id {
				return p != nil && len(p.Containers) == 1 &&
					p.Containers[0].State.String() == "CONTAINER_STATE_RUNNING" && running(r) &&
					r.Instance.Phase != model.PhaseExited
			}
		}
		return false
	})
	for _, l := range lines {
		if err := n.Script().AppendLog(id, "main", l); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Script().ExitContainer(id, "main", code); err != nil {
		t.Fatal(err)
	}
}

// A service's crash_loop and rollout_failed evidence keep its log tail
// redacted of the secrets it was given, and bounded (§13.7), as an
// addon's always was.
func TestServiceTailsRedacted(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	const token = "s3cr3t/value+x"
	h.putSecret(env.ID, "token", token)
	webSpec := func(version, extra string) string {
		return fmt.Sprintf(`{"services":{"web":{"image":%q,"command":["/app"],"env":{"VERSION":%q,"TOKEN":{"secret":"token"}},
			"ports":[{"name":"http","port":8080}]%s}}}`, testImage, version, extra)
	}
	r1 := h.apply(env.ID, webSpec("1", ""))
	h.waitRelease(env.ID, r1, model.ReleaseDeployed, 30*time.Second)
	id := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)[0].Sandbox.ID
	for i := 1; i <= 3; i++ {
		var lines []string
		if i == 1 {
			lines = longLines(40)
		}
		lines = append(lines, fmt.Sprintf("crash %d: bad token %s", i, token))
		exitRunning(t, h, n1, env.ID, id, 1, lines...)
		eventually(t, 10*time.Second, fmt.Sprintf("exit %d recorded", i), func() bool {
			inst, _ := h.st.GetEnvInstance(bg, id)
			return inst.Restarts == i
		})
	}
	inc := h.incident(env.ID, "service/web", model.ConditionCrashLoop, 10*time.Second)
	var ev evidence
	if err := json.Unmarshal(inc.Evidence, &ev); err != nil {
		t.Fatal(err)
	}
	checkStored(t, "crash_loop evidence", ev.LogTail, token)
	if n := len(ev.LogTail); n == 0 || ev.LogTail[n-1] != "crash 3: bad token "+redactedMark {
		t.Errorf("the newest line %q", ev.LogTail)
	}
	h.waitRows(env.ID, spec.KindService, "web", 1, 30*time.Second, "ready again", ready)

	// A new instance that never gets ready fails the release at its
	// rollout budget; the evidence has its tail, redacted.
	r2 := h.apply(env.ID, webSpec("2", `,"readiness":{"tcp":{"port":8080},"period":"1s"},"rollout":{"timeout":"5s"}`))
	var fresh podid.ID
	eventually(t, 30*time.Second, "the new instance running", func() bool {
		for _, r := range h.rowsOf(env.ID, spec.KindService, "web") {
			if r.Sandbox.ID != id && running(r) && h.pod("n1", r.Sandbox.ID) != nil {
				fresh = r.Sandbox.ID
				return true
			}
		}
		return false
	})
	for _, l := range append(longLines(40), "starting with "+token) {
		if err := n1.Script().AppendLog(fresh, "main", l); err != nil {
			t.Fatal(err)
		}
	}
	h.waitRelease(env.ID, r2, model.ReleaseFailed, 30*time.Second)
	inc = h.incident(env.ID, "service/web", model.ConditionRolloutFailed, 10*time.Second)
	ev = evidence{}
	if err := json.Unmarshal(inc.Evidence, &ev); err != nil {
		t.Fatal(err)
	}
	checkStored(t, "rollout_failed evidence", ev.LogTail, token)
	if n := len(ev.LogTail); n == 0 || ev.LogTail[n-1] != "starting with "+redactedMark {
		t.Errorf("the newest line %q", ev.LogTail)
	}
}
