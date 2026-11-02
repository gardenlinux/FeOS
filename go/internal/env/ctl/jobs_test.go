// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	containerv1 "github.com/gardenlinux/feos/go/gen/feos/container/v1"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/testnode"
)

// jobSpec is a spec with a release job of command (and a service that
// waits for it).
func jobSpec(command string, retries int) string {
	return fmt.Sprintf(`{"jobs":{"migrate":{"image":%q,"command":["/app","migrate",%q],"retries":%d}},
		"services":{"api":{"image":%q,"command":["/app","serve"]}}}`, testImage, command, retries, testImage)
}

// runningJob waits for the one running instance of the job migrate, its
// container up, and returns its sandbox's ID.
func runningJob(t *testing.T, h *harness, envID uuid.UUID) podid.ID {
	t.Helper()
	rows := h.waitRows(envID, spec.KindJob, "migrate", 1, 30*time.Second, "running", func(r store.EnvInstanceRow) bool {
		if !running(r) {
			return false
		}
		p := h.pod("n1", r.Sandbox.ID)
		return p != nil && len(p.Containers) == 1
	})
	return rows[0].Sandbox.ID
}

// finishRun waits for the job's running instance, gives its container
// log lines and makes it exit with code.
func finishRun(t *testing.T, h *harness, n *testnode.Node, envID uuid.UUID, code int, lines ...string) podid.ID {
	t.Helper()
	id := runningJob(t, h, envID)
	for _, l := range lines {
		if err := n.Script().AppendLog(id, "main", l); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Script().ExitContainer(id, "main", code); err != nil {
		t.Fatal(err)
	}
	return id
}

// A release job runs before the services, once per template: its exit 0
// is a succeeded run with its log kept; a failing one is retried and its
// failed last attempt fails the release (job_failed), which leaves the
// environment at its deployed release until a new one succeeds.
func TestReleaseJobs(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, jobSpec("v1", 1))
	// The service waits for the job.
	never(t, time.Second, "the service started before the job ran", func() bool {
		return len(h.rowsOf(env.ID, spec.KindService, "api")) > 0
	})
	first := finishRun(t, h, n1, env.ID, 0, "applying 0001", "done")
	h.waitRelease(env.ID, r1, model.ReleaseDeployed, 30*time.Second)
	runs, err := h.st.ListJobRuns(bg, env.ID, "migrate", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %+v %v", runs, err)
	}
	run := runs[0]
	if run.Phase != model.RunSucceeded || run.ExitCode == nil || *run.ExitCode != 0 || run.Attempt != 1 || run.Release != r1 ||
		!slices.Equal(run.LogTail, []string{"applying 0001", "done"}) || run.FinishedAt.IsZero() || run.SandboxID != first {
		t.Fatalf("run %+v", run)
	}
	if _, err := h.st.GetSandbox(bg, first); err == nil {
		t.Error("the finished job's row stays")
	}
	if len(h.rowsOf(env.ID, spec.KindService, "api")) != 1 {
		t.Error("the service did not start after the job")
	}
	// A release that changes nothing of the job does not run it again.
	r2 := h.apply(env.ID, strings.Replace(jobSpec("v1", 1), `"serve"`, `"serve","--v2"`, 1))
	h.waitRelease(env.ID, r2, model.ReleaseDeployed, 30*time.Second)
	if runs, _ := h.st.ListJobRuns(bg, env.ID, "migrate", 10); len(runs) != 1 {
		t.Fatalf("the unchanged job ran again: %+v", runs)
	}

	// A changed job that fails twice (retries 1) fails its release.
	r3 := h.apply(env.ID, jobSpec("v2", 1))
	finishRun(t, h, n1, env.ID, 3, "migrate: boom")
	eventually(t, 30*time.Second, "the second attempt", func() bool {
		runs, _ := h.st.ListJobRuns(bg, env.ID, "migrate", 10)
		return len(runs) == 3 && runs[0].Attempt == 2
	})
	finishRun(t, h, n1, env.ID, 3, "migrate: boom again")
	rel := h.waitRelease(env.ID, r3, model.ReleaseFailed, 30*time.Second)
	if rel.Reason != "job migrate failed its last attempt (2 of 2) with code 3" || rel.FinishedAt.IsZero() {
		t.Errorf("failed release %+v", rel)
	}
	inc := h.incident(env.ID, "job/migrate", model.ConditionJobFailed, 10*time.Second)
	var ev evidence
	if err := json.Unmarshal(inc.Evidence, &ev); err != nil || inc.Release != r3 || inc.Summary != rel.Reason ||
		!slices.Equal(ev.LogTail, []string{"migrate: boom again"}) || len(ev.Exits) != 1 || ev.Exits[0].Code == nil ||
		*ev.Exits[0].Code != 3 {
		t.Fatalf("incident %+v %+v %v", inc, ev, err)
	}
	// The failed release is not retried; the deployed one keeps serving.
	never(t, 2*time.Second, "the failed job ran again", func() bool {
		runs, _ := h.st.ListJobRuns(bg, env.ID, "migrate", 10)
		return len(runs) != 3
	})
	if e, _ := h.st.GetEnvironment(bg, env.ID); e.Deployed != r2 {
		t.Errorf("deployed %d", e.Deployed)
	}
	if rows := h.rowsOf(env.ID, spec.KindService, "api"); len(rows) != 1 || rows[0].Instance.Release != r2 {
		t.Errorf("the service after the failure %+v", rows)
	}

	// A new release that succeeds is deployed and closes the incident.
	r4 := h.apply(env.ID, jobSpec("v3", 0))
	finishRun(t, h, n1, env.ID, 0)
	h.waitRelease(env.ID, r4, model.ReleaseDeployed, 30*time.Second)
	if open := h.incidents(env.ID, true); len(open) != 0 {
		t.Errorf("open incidents %+v", open)
	}
	if rel := h.release(env.ID, r3); rel.State != model.ReleaseFailed {
		t.Errorf("the failed release became %s", rel.State)
	}
}

// A run past its job's timeout fails; one whose node reports no exit
// status fails with that detail; a manual job runs only when asked.
func TestJobTimeoutAndManual(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"jobs":{"migrate":{"image":%q,"timeout":"1s"},
		"backfill":{"image":%q,"on":"manual"}}}`, testImage, testImage))
	rel := h.waitRelease(env.ID, r1, model.ReleaseFailed, 30*time.Second)
	if !strings.Contains(rel.Reason, "timed out after 1s") {
		t.Errorf("reason %q", rel.Reason)
	}
	if runs, _ := h.st.ListJobRuns(bg, env.ID, "backfill", 10); len(runs) != 0 {
		t.Errorf("the manual job ran by itself: %+v", runs)
	}
	r2 := h.apply(env.ID, fmt.Sprintf(`{"jobs":{"migrate":{"image":%q,"command":["x"]},
		"backfill":{"image":%q,"on":"manual"}}}`, testImage, testImage))
	finishRun(t, h, n1, env.ID, -1)
	rel = h.waitRelease(env.ID, r2, model.ReleaseFailed, 30*time.Second)
	if !strings.Contains(rel.Reason, "the node did not report an exit status") {
		t.Errorf("reason %q", rel.Reason)
	}
	if err := h.c.RunJob(bg, env.ID, "nope"); err != ErrNoObject {
		t.Errorf("run an unknown job: %v", err)
	}
	// The target is the deployed release: none, so no job to run.
	if err := h.c.RunJob(bg, env.ID, "backfill"); err != ErrNoObject {
		t.Errorf("run a job of no target: %v", err)
	}
	r3 := h.apply(env.ID, fmt.Sprintf(`{"jobs":{"backfill":{"image":%q,"on":"manual"}}}`, testImage))
	h.waitRelease(env.ID, r3, model.ReleaseDeployed, 30*time.Second)
	if err := h.c.RunJob(bg, env.ID, "backfill"); err != nil {
		t.Fatal(err)
	}
	// Asked again while it runs: refused, never a second run at once.
	if err := h.c.RunJob(bg, env.ID, "backfill"); !errors.Is(err, model.ErrBusy) || !errors.Is(err, ErrJobRunning) ||
		err.Error() != fmt.Sprintf("job backfill runs already (release %d, attempt 1)", r3) {
		t.Fatalf("a second run while one runs: %v", err)
	}
	var id podid.ID
	eventually(t, 30*time.Second, "the manual run", func() bool {
		for _, r := range h.rowsOf(env.ID, spec.KindJob, "backfill") {
			if running(r) {
				id = r.Sandbox.ID
				return h.pod("n1", id) != nil
			}
		}
		return false
	})
	if err := n1.Script().ExitContainer(id, "main", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the manual run succeeded", func() bool {
		r, err := h.st.LatestJobRun(bg, env.ID, "backfill")
		return err == nil && r.Phase == model.RunSucceeded && r.Attempt == 1 && r.Release == r3
	})
	// Once it ended, it runs again.
	if err := h.c.RunJob(bg, env.ID, "backfill"); err != nil {
		t.Fatalf("a run after the first ended: %v", err)
	}
	if runs, _ := h.st.ListJobRuns(bg, env.ID, "backfill", 10); len(runs) != 2 {
		t.Errorf("runs %+v", runs)
	}
}

// runningRuns counts the running runs of the job migrate.
func runningRuns(h *harness, envID uuid.UUID) int {
	runs, _ := h.st.EnvCtlRunningJobRuns(bg, envID)
	n := 0
	for _, r := range runs {
		if r.Job == "migrate" {
			n++
		}
	}
	return n
}

// A job runs once at a time across releases (§13.7): a release that
// replaces a deploying one while its job runs waits for that run, never
// runs a second one beside it, never cancels it. The old run's success
// with the same template is the new release's; a changed job runs once
// the old run ended; a manual run that runs when a release rolls is
// waited for the same way.
func TestReleaseJobReplacedWhileRunning(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, jobSpec("v1", 0))
	first := runningJob(t, h, env.ID)
	// r2 changes only the service: migrate keeps its template.
	r2 := h.apply(env.ID, strings.Replace(jobSpec("v1", 0), `"serve"`, `"serve","--v2"`, 1))
	never(t, 3*time.Second, "a second run of migrate beside the replaced release's", func() bool {
		return runningRuns(h, env.ID) > 1
	})
	if rel := h.release(env.ID, r2); rel.State != model.ReleaseDeploying {
		t.Fatalf("release %d is %s while it waits for the job", r2, rel.State)
	}
	if err := n1.Script().ExitContainer(first, "main", 0); err != nil {
		t.Fatal(err)
	}
	h.waitRelease(env.ID, r2, model.ReleaseDeployed, 30*time.Second)
	if runs, _ := h.st.ListJobRuns(bg, env.ID, "migrate", 10); len(runs) != 1 || runs[0].Release != r1 ||
		runs[0].Phase != model.RunSucceeded {
		t.Fatalf("the replaced release's success is not the new one's: %+v", runs)
	}

	// A changed job waits for the old template's run, whose failure does
	// not fail the new release.
	r3 := h.apply(env.ID, jobSpec("v2", 0))
	old := runningJob(t, h, env.ID)
	r4 := h.apply(env.ID, jobSpec("v3", 0))
	never(t, 3*time.Second, "a run of the changed job beside the old one", func() bool {
		return runningRuns(h, env.ID) > 1
	})
	if err := n1.Script().ExitContainer(old, "main", 1); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the changed job's run", func() bool {
		r, err := h.st.LatestJobRun(bg, env.ID, "migrate")
		return err == nil && r.Release == r4 && r.Attempt == 1
	})
	finishRun(t, h, n1, env.ID, 0)
	h.waitRelease(env.ID, r4, model.ReleaseDeployed, 30*time.Second)
	if rel := h.release(env.ID, r3); rel.State == model.ReleaseFailed {
		t.Errorf("the replaced release failed with its job: %+v", rel)
	}

	// A manual run that runs when a release rolls is waited for too.
	if err := h.c.RunJob(bg, env.ID, "migrate"); err != nil {
		t.Fatal(err)
	}
	manual := runningJob(t, h, env.ID)
	r5 := h.apply(env.ID, jobSpec("v4", 0))
	never(t, 3*time.Second, "a release's run beside a manual one", func() bool {
		return runningRuns(h, env.ID) > 1
	})
	if err := n1.Script().ExitContainer(manual, "main", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the release's own run", func() bool {
		r, err := h.st.LatestJobRun(bg, env.ID, "migrate")
		return err == nil && r.Release == r5
	})
	finishRun(t, h, n1, env.ID, 0)
	h.waitRelease(env.ID, r5, model.ReleaseDeployed, 30*time.Second)
}

// A release job that waited for another release's run keeps its time:
// the release may roll until that run ended and its own attempts had
// theirs, beyond its deadline.
func TestReleaseJobWaitKeepsItsTime(t *testing.T) {
	h := newHarness(t, options{timings: func(t *timings) { t.releaseDeadline = time.Second }})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	jobOnly := func(command string) string {
		return fmt.Sprintf(`{"jobs":{"migrate":{"image":%q,"command":["/app","migrate",%q],"timeout":"15s"}}}`, testImage, command)
	}
	h.apply(env.ID, jobOnly("v1"))
	first := runningJob(t, h, env.ID)
	// The same template: its deadline is the job's 15 s.
	r2 := h.apply(env.ID, jobOnly("v1"))
	var started time.Time
	eventually(t, 10*time.Second, "release 2 deploying", func() bool {
		rel := h.release(env.ID, r2)
		started = rel.StartedAt
		return rel.State == model.ReleaseDeploying && !started.IsZero()
	})
	time.Sleep(time.Until(started.Add(8 * time.Second)))
	// The old run fails 8 s into release 2: its own run starts then, and
	// may take its 15 s.
	if err := n1.Script().ExitContainer(first, "main", 1); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "release 2's own run", func() bool {
		r, err := h.st.LatestJobRun(bg, env.ID, "migrate")
		return err == nil && r.Release == r2 && r.Phase == model.RunRunning
	})
	own := runningJob(t, h, env.ID)
	never(t, time.Until(started.Add(17500*time.Millisecond)), "release 2 failed past its deadline while its run runs", func() bool {
		return h.release(env.ID, r2).State == model.ReleaseFailed
	})
	if err := n1.Script().ExitContainer(own, "main", 0); err != nil {
		t.Fatal(err)
	}
	h.waitRelease(env.ID, r2, model.ReleaseDeployed, 30*time.Second)
}

// deployingSince waits for release n to deploy and returns its start.
func deployingSince(t *testing.T, h *harness, envID uuid.UUID, n int) time.Time {
	t.Helper()
	var started time.Time
	eventually(t, 10*time.Second, fmt.Sprintf("release %d deploying", n), func() bool {
		rel := h.release(envID, n)
		started = rel.StartedAt
		return rel.State == model.ReleaseDeploying && !started.IsZero()
	})
	return started
}

// webAndMigrate is a spec with a service web (its rollout budget, more
// fields in extra) and a release job migrate of command with its timeout.
func webAndMigrate(command, timeout, version, rollout, extra string) string {
	return fmt.Sprintf(`{"jobs":{"migrate":{"image":%q,"command":["/app","migrate",%q],"timeout":%q}},
		"services":{"web":{"image":%q,"command":["/app"],"env":{"VERSION":%q},"rollout":{"timeout":%q}%s}}}`,
		testImage, command, timeout, testImage, version, rollout, extra)
}

// The time a release waited for another release's run of its job moves
// the start its services' rollout budgets count from (§13.7), also once
// the job is done: a service whose budget passed during the wait still
// rolls after the release's own run, and after a run under the same
// template that served the release.
func TestReleaseJobWaitShiftsTheRolloutBudget(t *testing.T) {
	h := newHarness(t, options{timings: func(t *timings) { t.releaseDeadline = time.Second }})
	n1 := h.addNode("n1")
	env := h.newEnv("stage")
	failed := func(n int) func() bool {
		return func() bool { return h.release(env.ID, n).State == model.ReleaseFailed }
	}
	// The test ends the runs itself, up to 11 s into the next release,
	// which starts some time after the run (seconds under load); the
	// target's job timeout bounds every run, so it is far longer than
	// that. Web's budget, not the release's deadline, is what the test is
	// about.
	const job = "60s"
	h.apply(env.ID, webAndMigrate("v1", job, "1", "10s", ""))
	first := runningJob(t, h, env.ID)
	// A changed job: release 1's run ends 9 s into release 2, whose own
	// run then succeeds at once, near web's 10 s from release 2's start.
	r2 := h.apply(env.ID, webAndMigrate("v2", job, "2", "10s", ""))
	started := deployingSince(t, h, env.ID, r2)
	time.Sleep(time.Until(started.Add(9 * time.Second)))
	if err := n1.Script().ExitContainer(first, "main", 1); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "release 2's own run", func() bool {
		r, err := h.st.LatestJobRun(bg, env.ID, "migrate")
		return err == nil && r.Release == r2 && r.Phase == model.RunRunning
	})
	finishRun(t, h, n1, env.ID, 0)
	never(t, 3*time.Second, "release 2 failed right after its own run", failed(r2))
	h.waitRelease(env.ID, r2, model.ReleaseDeployed, 30*time.Second)

	// The same template: release 3's run succeeds 11 s into release 4,
	// past web's 10 s from release 4's start, and serves it.
	r3 := h.apply(env.ID, webAndMigrate("v3", job, "3", "10s", ""))
	eventually(t, 10*time.Second, "release 3's run", func() bool {
		r, err := h.st.LatestJobRun(bg, env.ID, "migrate")
		return err == nil && r.Release == r3 && r.Phase == model.RunRunning
	})
	third := runningJob(t, h, env.ID)
	r4 := h.apply(env.ID, webAndMigrate("v3", job, "4", "10s", ""))
	started = deployingSince(t, h, env.ID, r4)
	time.Sleep(time.Until(started.Add(11 * time.Second)))
	if err := n1.Script().ExitContainer(third, "main", 0); err != nil {
		t.Fatal(err)
	}
	never(t, 3*time.Second, "release 4 failed once release 3's run served it", failed(r4))
	h.waitRelease(env.ID, r4, model.ReleaseDeployed, 30*time.Second)
	if r, err := h.st.LatestJobRun(bg, env.ID, "migrate"); err != nil || r.Release != r3 {
		t.Fatalf("release 4 ran its job again: %+v %v", r, err)
	}
}

// The release's deadline counts from the same moved start, also once its
// jobs are done: a service that is slow to become ready after the wait
// rolls on past the deadline as counted from the release's own start.
func TestReleaseJobWaitShiftsTheDeadline(t *testing.T) {
	h := newHarness(t, options{timings: func(t *timings) { t.releaseDeadline = time.Second }})
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
	// Web's budget and the job's timeout are 15 s, and so the deadline.
	const check = `,"readiness":{"exec":{"command":["check"]},"period":"1s"}`
	h.apply(env.ID, webAndMigrate("v1", "15s", "1", "15s", check))
	first := runningJob(t, h, env.ID)
	r2 := h.apply(env.ID, webAndMigrate("v2", "15s", "2", "15s", check))
	started := deployingSince(t, h, env.ID, r2)
	time.Sleep(time.Until(started.Add(9 * time.Second)))
	if err := n1.Script().ExitContainer(first, "main", 1); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "release 2's own run", func() bool {
		r, err := h.st.LatestJobRun(bg, env.ID, "migrate")
		return err == nil && r.Release == r2 && r.Phase == model.RunRunning
	})
	finishRun(t, h, n1, env.ID, 0)
	// Web's check passes 20 s into release 2: past the 15 s from its own
	// start, within the 15 s from the end of its 9 s wait.
	never(t, time.Until(started.Add(20*time.Second)), "release 2 failed past its unmoved deadline", func() bool {
		return h.release(env.ID, r2).State == model.ReleaseFailed
	})
	code.Store(0)
	h.waitRelease(env.ID, r2, model.ReleaseDeployed, 30*time.Second)
}

// The leader's insert of a release job's run is refused while a run of
// the job runs, checked under the environment's row lock: a run the API
// inserted after the step read the runs is waited for, never run beside.
func TestStartRunRefusedWhileARunRuns(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"jobs":{"backfill":{"image":%q,"on":"manual"}}}`, testImage))
	h.waitRelease(env.ID, r1, model.ReleaseDeployed, 30*time.Second)
	if err := h.c.RunJob(bg, env.ID, "backfill"); err != nil {
		t.Fatal(err)
	}
	v, err := h.c.load(bg, env.ID)
	if err != nil || v == nil {
		t.Fatalf("view %v", err)
	}
	p := h.c.planJob(v, "backfill", v.target.Jobs["backfill"])
	s := &stepState{v: v}
	h.c.startRun(bg, s, p, 1)
	if runs, _ := h.st.ListJobRuns(bg, env.ID, "backfill", 10); len(runs) != 1 {
		t.Fatalf("a second run beside the first: %+v", runs)
	}
	if want := fmt.Sprintf("job backfill runs already (release %d, attempt 1)", r1); len(s.pending) != 1 ||
		s.pending[0].text != want {
		t.Fatalf("pending %+v, want %q", s.pending, want)
	}
}
