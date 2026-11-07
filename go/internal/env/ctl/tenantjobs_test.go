// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// tenantJobSpecOf is a job of the test image with a deadline of d.
func tenantJobSpecOf(d time.Duration) TenantJobSpec {
	return TenantJobSpec{Image: testImage, Command: []string{"/build"}, VCPUs: 1, MemoryMiB: 512, Deadline: d, Hash: "h",
		SecurityGroups: []string{"default"}}
}

// jobSpecOf is tenantJobSpecOf in the tenant's environment builds.
func (h *harness) jobSpecOf(d time.Duration) TenantJobSpec {
	rs := tenantJobSpecOf(d)
	rs.EnvID = h.newEnv("builds").ID
	return rs
}

// createTenantJob creates a job of the tenant in its environment builds
// (one without an environment: there) and returns it.
func (h *harness) createTenantJob(rs TenantJobSpec) store.TenantJob {
	h.t.Helper()
	if rs.EnvID == uuid.Nil {
		rs.EnvID = h.newEnv("builds").ID
	}
	r, created, err := h.c.CreateTenantJob(bg, testTenant, rs)
	if err != nil || !created {
		h.t.Fatalf("create a tenant job: %v (created %v)", err, created)
	}
	return r
}

// waitTenantJob waits until the job is in phase, and returns it.
func (h *harness) waitTenantJob(id podid.ID, phase string, within time.Duration) store.TenantJob {
	h.t.Helper()
	var r store.TenantJob
	eventually(h.t, within, "job "+id.String()+" "+phase, func() bool {
		var err error
		r, err = h.st.GetTenantJob(bg, testTenant, id)
		return err == nil && r.Phase == phase
	})
	return r
}

// runningPod waits for a job's pod to run on n1 with its container, and
// returns the pod's spec.
func (h *harness) runningPod(id podid.ID) {
	h.t.Helper()
	eventually(h.t, 30*time.Second, "the job's pod runs", func() bool {
		sb, err := h.st.GetSandbox(bg, id)
		if err != nil || !steady(sb) {
			return false
		}
		p := h.pod("n1", id)
		return p != nil && len(p.Containers) == 1
	})
}

// podGone waits until a job's pod's row is gone.
func (h *harness) podGone(id podid.ID) {
	h.t.Helper()
	eventually(h.t, 30*time.Second, "the job's pod is deleted", func() bool {
		_, err := h.st.GetSandbox(bg, id)
		return errors.Is(err, model.ErrNotFound)
	})
}

// A job's pod is a row the tenant does not see, built from the job's
// template with its secrets and never idle; its exit 0 ends it succeeded
// with its result file and its log's tail, redacted of its secrets, and
// its pod goes. A create with its key and body answers with it; with
// another body it is refused.
func TestTenantJobSucceeds(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	const token = "s3cr3t-token-value"
	rs := tenantJobSpecOf(time.Minute)
	rs.Env = map[string]string{"COMMIT": "abc123"}
	rs.Secrets = map[string]string{"GIT_TOKEN": token}
	rs.ImageCredentials = &model.RegistryCredentials{Username: "build", Password: "registry-password"}
	rs.ResultPath = "/run/fecode-build/digest"
	rs.Key, rs.Labels = "build-1", map[string]string{"project": "p1"}
	job := h.createTenantJob(rs)
	if job.Phase != model.JobQueued || job.ImageRef != testImage || job.Ended() ||
		job.DeadlineAt.Sub(job.CreatedAt) != time.Minute {
		t.Fatalf("created %+v", job)
	}
	again, created, err := h.c.CreateTenantJob(bg, testTenant, rs)
	if err != nil || created || again.ID != job.ID {
		t.Fatalf("the repeat: %+v %v %v", again, created, err)
	}
	other := rs
	other.Hash = "another body"
	if _, _, err := h.c.CreateTenantJob(bg, testTenant, other); !errors.Is(err, ErrJobKeyConflict) {
		t.Fatalf("another body under the key: %v", err)
	}
	if list, err := h.st.ListSandboxes(bg, testTenant, nil); err != nil || len(list) != 0 {
		t.Fatalf("the tenant sees %d sandboxes (%v)", len(list), err)
	}

	h.runningPod(job.ID)
	sb, _ := h.st.GetSandbox(bg, job.ID)
	if !sb.TenantJob || sb.IsOwn() || sb.Instance || sb.EnvironmentID != job.EnvID || sb.IdleAfterS == nil ||
		*sb.IdleAfterS != 0 || !slices.Equal(sb.SecurityGroups, []string{"default"}) {
		t.Fatalf("the pod's row %+v", sb)
	}
	spec := h.pod("n1", job.ID).GetSpec().GetContainers()[0]
	if spec.GetEnv()["GIT_TOKEN"] != token || spec.GetEnv()["COMMIT"] != "abc123" ||
		!slices.Equal(spec.GetCommand(), []string{"/build"}) {
		t.Fatalf("the pod's container %+v", spec)
	}
	h.waitTenantJob(job.ID, model.JobRunning, 10*time.Second)

	dir := filepath.Join(n1.PodDir(job.ID), "run", "fecode-build")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "digest"), []byte("sha256:0123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"cloning with " + token, "pushed"} {
		if err := n1.Script().AppendLog(job.ID, "main", l); err != nil {
			t.Fatal(err)
		}
	}
	if err := n1.Script().ExitContainer(job.ID, "main", 0); err != nil {
		t.Fatal(err)
	}
	done := h.waitTenantJob(job.ID, model.JobSucceeded, 30*time.Second)
	if done.ExitCode == nil || *done.ExitCode != 0 || done.Reason != "" || done.Result != "sha256:0123\n" ||
		!slices.Equal(done.LogTail, []string{"cloning with [redacted]", "pushed"}) || done.FinishedAt.IsZero() ||
		done.StartedAt.IsZero() {
		t.Fatalf("ended %+v", done)
	}
	h.podGone(job.ID)
	if sealed, err := h.st.TenantJobSealed(bg, job.ID); err != nil || sealed != nil {
		t.Errorf("the ended job keeps its secrets: %v %v", sealed, err)
	}
	if lines, err := h.c.TenantJobLog(bg, testTenant, job.ID, 1); err != nil || !slices.Equal(lines, []string{"pushed"}) {
		t.Errorf("the ended job's log %v %v", lines, err)
	}
	// Cancelling a job that ended changes nothing.
	if r, err := h.c.CancelTenantJob(bg, testTenant, job.ID); err != nil || r.Phase != model.JobSucceeded {
		t.Errorf("cancel after the end: %+v %v", r, err)
	}
	if err := h.c.DeleteTenantJob(bg, testTenant, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetTenantJob(bg, testTenant, job.ID); !errors.Is(err, store.ErrNoRow) {
		t.Errorf("the deleted job stays: %v", err)
	}
}

// Another exit code fails the job (exit); a missing result file is its
// detail.
func TestTenantJobFails(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	rs := tenantJobSpecOf(time.Minute)
	rs.ResultPath = "/run/none"
	job := h.createTenantJob(rs)
	h.runningPod(job.ID)
	if err := n1.Script().AppendLog(job.ID, "main", "error: boom"); err != nil {
		t.Fatal(err)
	}
	if err := n1.Script().ExitContainer(job.ID, "main", 3); err != nil {
		t.Fatal(err)
	}
	done := h.waitTenantJob(job.ID, model.JobFailed, 30*time.Second)
	if done.Reason != model.JobReasonExit || done.ExitCode == nil || *done.ExitCode != 3 ||
		!slices.Equal(done.LogTail, []string{"error: boom"}) || !strings.HasPrefix(done.Detail, "its result /run/none") {
		t.Fatalf("ended %+v", done)
	}
	h.podGone(job.ID)
}

// A job whose deadline passes ends deadline_exceeded, its tail kept.
func TestTenantJobDeadline(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	job := h.createTenantJob(tenantJobSpecOf(3 * time.Second))
	h.runningPod(job.ID)
	if err := n1.Script().AppendLog(job.ID, "main", "still building"); err != nil {
		t.Fatal(err)
	}
	done := h.waitTenantJob(job.ID, model.JobDeadlineExceeded, 30*time.Second)
	if done.ExitCode != nil || done.Detail != "its deadline of 3s passed" ||
		!slices.Equal(done.LogTail, []string{"still building"}) {
		t.Fatalf("ended %+v", done)
	}
	h.podGone(job.ID)
}

// A cancel ends a running job cancelled, its tail kept and its pod
// deleted; a delete of a running job cancels it and removes it.
func TestTenantJobCancel(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	job := h.createTenantJob(tenantJobSpecOf(time.Minute))
	h.runningPod(job.ID)
	if err := n1.Script().AppendLog(job.ID, "main", "step 1"); err != nil {
		t.Fatal(err)
	}
	r, err := h.c.CancelTenantJob(bg, testTenant, job.ID)
	if err != nil || r.Phase != model.JobCancelled || !slices.Equal(r.LogTail, []string{"step 1"}) {
		t.Fatalf("cancelled %+v %v", r, err)
	}
	h.podGone(job.ID)

	second := h.createTenantJob(tenantJobSpecOf(time.Minute))
	h.runningPod(second.ID)
	if err := h.c.DeleteTenantJob(bg, testTenant, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetTenantJob(bg, testTenant, second.ID); !errors.Is(err, store.ErrNoRow) {
		t.Errorf("the deleted job stays: %v", err)
	}
	h.podGone(second.ID)
	if _, err := h.c.CancelTenantJob(bg, testTenant, second.ID); !errors.Is(err, ErrNoObject) {
		t.Errorf("cancel of a deleted job: %v", err)
	}
}

// A job whose pod is lost with its node fails node_lost and is not run
// again.
func TestTenantJobNodeLost(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	job := h.createTenantJob(tenantJobSpecOf(time.Minute))
	h.runningPod(job.ID)
	if err := n1.Script().ForgetPod(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.Sync(bg, job.ID); err != nil { // finds it lost, sooner than the engine's loops
		t.Fatal(err)
	}
	done := h.waitTenantJob(job.ID, model.JobFailed, 30*time.Second)
	if done.Reason != model.JobReasonNodeLost || done.Detail != "its pod was lost with node n1" {
		t.Fatalf("ended %+v", done)
	}
	h.podGone(job.ID)
	if p := h.pod("n1", job.ID); p != nil {
		t.Error("the lost job runs again")
	}
}

// The sweep removes the jobs that ended TenantJobRetention ago, and only those.
func TestTenantJobSweep(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	old := h.createTenantJob(tenantJobSpecOf(time.Minute))
	fresh := h.createTenantJob(tenantJobSpecOf(time.Minute))
	for _, id := range []podid.ID{old.ID, fresh.ID} {
		if _, err := h.c.CancelTenantJob(bg, testTenant, id); err != nil {
			t.Fatal(err)
		}
	}
	h.exec("UPDATE jobs SET finished_at = now() - interval '8 days' WHERE id = $1", old.ID)
	if err := h.c.sweepTenantJobs(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetTenantJob(bg, testTenant, old.ID); !errors.Is(err, store.ErrNoRow) {
		t.Errorf("the old job stays: %v", err)
	}
	if _, err := h.st.GetTenantJob(bg, testTenant, fresh.ID); err != nil {
		t.Errorf("the fresh job went: %v", err)
	}
}

// A job whose image its registry refuses to the pod's create fails
// (image).
func TestTenantJobImageRefused(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	h.eng.SetImageAccess(func(context.Context, string, model.RegistryCredentials) error {
		return errors.New("401: denied")
	})
	job := h.createTenantJob(tenantJobSpecOf(time.Minute))
	done := h.waitTenantJob(job.ID, model.JobFailed, 30*time.Second)
	if done.Reason != model.JobReasonImage || !strings.Contains(done.Detail, "denied") {
		t.Fatalf("ended %+v", done)
	}
	h.podGone(job.ID)
}
