// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// insertTenantJob inserts a job of t1 with its pod's row, as a create does.
func insertTenantJob(t *testing.T, s *store.Store, key string, labels map[string]string) store.TenantJob {
	t.Helper()
	var job store.TenantJob
	env := boxEnv(t, s, "t1")
	err := s.InTx(bg, func(tx *store.Store) error {
		idle := 0
		sb, err := tx.CreateMintedSandbox(bg, func(id podid.ID) store.NewSandbox {
			return store.NewSandbox{ID: id, Owner: "t1", Image: "img", VCPUs: 1, MemoryMiB: 512, ImageRef: "img@sha256:00",
				EnvironmentID: env, PodTemplate: &model.PodTemplate{}, IdleAfterS: &idle, TenantJob: true}
		})
		if err != nil {
			return err
		}
		job, err = tx.InsertTenantJob(bg, store.TenantJob{ID: sb.ID, Owner: "t1", EnvID: env, Key: key, RequestHash: "h", Image: "img",
			ImageRef: "img@sha256:00", VCPUs: 1, MemoryMiB: 512, Labels: labels}, []byte("sealed"), time.Minute)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// A job's pod is no sandbox of its tenant; a key is unique per tenant; a
// job ends once, which drops its secrets; the pods of ended jobs are
// left for deletion; the sweep takes the jobs that ended long enough ago.
func TestTenantJobs(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "t1")
	a := insertTenantJob(t, s, "k1", map[string]string{"project": "p1"})
	b := insertTenantJob(t, s, "", nil)
	if a.Phase != model.JobQueued || a.Key != "k1" || a.DeadlineAt.Sub(a.CreatedAt) != time.Minute || a.Ended() {
		t.Fatalf("inserted %+v", a)
	}
	if list, err := s.ListSandboxes(bg, "t1", nil); err != nil || len(list) != 0 {
		t.Fatalf("the tenant's sandboxes %d %v", len(list), err)
	}
	if err := s.InTx(bg, func(tx *store.Store) error {
		_, err := tx.InsertTenantJob(bg, store.TenantJob{ID: podid.Mint(), Owner: "t1", EnvID: a.EnvID, Key: "k1", RequestHash: "h2", Image: "i",
			ImageRef: "i"}, nil, time.Minute)
		return err
	}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("a second job under k1: %v", err)
	}
	if r, err := s.TenantJobByKey(bg, "t1", "k1"); err != nil || r.ID != a.ID {
		t.Fatalf("by key %+v %v", r, err)
	}
	if list, err := s.ListTenantJobs(bg, "t1", map[string]string{"project": "p1"}, nil); err != nil || len(list) != 1 ||
		list[0].ID != a.ID {
		t.Fatalf("by label %+v %v", list, err)
	}
	if a.EnvName != "box" || len(a.SecurityGroups) != 1 || a.SecurityGroups[0] != "default" {
		t.Fatalf("environment and groups %+v", a)
	}
	if list, err := s.ListTenantJobs(bg, "t1", nil, []uuid.UUID{}); err != nil || len(list) != 0 {
		t.Fatalf("of no environment %+v %v", list, err)
	}
	if list, err := s.ListTenantJobs(bg, "t1", nil, []uuid.UUID{a.EnvID}); err != nil || len(list) != 2 {
		t.Fatalf("of its environment %+v %v", list, err)
	}
	if ok, err := s.StartTenantJob(bg, a.ID); err != nil || !ok {
		t.Fatalf("start %v %v", ok, err)
	}
	code := 0
	end := store.TenantJobEnd{Phase: model.JobSucceeded, ExitCode: &code, Result: "sha256:1", LogTail: []string{"done"},
		Exited: true}
	if ok, err := s.EndTenantJob(bg, a.ID, end); err != nil || !ok {
		t.Fatalf("end %v %v", ok, err)
	}
	if ok, err := s.EndTenantJob(bg, a.ID, store.TenantJobEnd{Phase: model.JobCancelled}); err != nil || ok {
		t.Fatalf("a second end %v %v", ok, err)
	}
	got, err := s.GetTenantJob(bg, "t1", a.ID)
	if err != nil || got.Phase != model.JobSucceeded || got.Result != "sha256:1" || got.StartedAt.IsZero() ||
		!got.Ended() || len(got.LogTail) != 1 {
		t.Fatalf("ended %+v %v", got, err)
	}
	if sealed, err := s.TenantJobSealed(bg, a.ID); err != nil || sealed != nil {
		t.Fatalf("sealed after the end %v %v", sealed, err)
	}
	if _, err := s.GetTenantJob(bg, "t2", a.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("another tenant's read: %v", err)
	}
	// An exit never seen running gets its start at its end.
	if ok, err := s.EndTenantJob(bg, b.ID, store.TenantJobEnd{Phase: model.JobFailed, Reason: model.JobReasonExit, ExitCode: &code,
		Exited: true}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got, _ := s.GetTenantJob(bg, "t1", b.ID); got.StartedAt.IsZero() {
		t.Error("the exit has no start")
	}
	left, err := s.LeftTenantJobPods(bg)
	if err != nil || len(left) != 2 {
		t.Fatalf("left pods %d %v", len(left), err)
	}
	exec(t, s, "UPDATE jobs SET finished_at = now() - interval '8 days' WHERE id = $1", a.ID)
	if n, err := s.SweepTenantJobs(bg, 7*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("swept %d %v", n, err)
	}
	if _, err := s.GetTenantJob(bg, "t1", a.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("the swept job: %v", err)
	}
}
