// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The environment controller's queries.
func TestEnvCtlQueries(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkNode(t, s, "n1")
	env := mkEnv(t, s, "acme", "stage")
	other := mkEnv(t, s, "acme", "prod")
	web := model.SecurityGroupID(uuid.New())
	a := mkInstance(t, s, env, func(n *store.NewSandbox) {
		n.PodTemplate.SecurityGroups = append(n.PodTemplate.SecurityGroups, web)
	})
	b := mkInstance(t, s, env)
	mkInstance(t, s, other)
	sbx := mkSandboxIn(t, s, "acme", env.ID, podid.Mint())
	exec(t, s, `UPDATE sandboxes SET pod_template = $2 WHERE id = $1`, sbx.ID,
		`{"securityGroups": ["env-`+env.ID.String()+`", "`+web+`"]}`)
	elsewhere := mkSandbox(t, s, "acme")
	exec(t, s, `UPDATE sandboxes SET address = 'fd00::2' WHERE id = $1`, a.ID)
	exec(t, s, `UPDATE sandboxes SET address = 'fd00::9' WHERE id = $1`, sbx.ID)
	exec(t, s, `UPDATE sandboxes SET address = 'fd00::6' WHERE id = $1`, elsewhere.ID)

	// env-<id>'s members: every row of the environment with an address,
	// its sandboxes too (ADR 0035); not another environment's.
	if m, err := s.EnvCtlMembers(bg, env.ID); err != nil || !slices.Equal(m, []string{"fd00::2", "fd00::9"}) {
		t.Fatalf("members %v %v", m, err)
	}
	exec(t, s, `UPDATE sandboxes SET address = 'fd00::8' WHERE id = $1`, b.ID)
	if m, err := s.EnvCtlMembers(bg, env.ID); err != nil || !slices.Equal(m, []string{"fd00::2", "fd00::8", "fd00::9"}) {
		t.Fatalf("members with b %v %v", m, err)
	}
	if m, err := s.EnvCtlMembers(bg, other.ID); err != nil || len(m) != 0 {
		t.Fatalf("members of another environment %v %v", m, err)
	}
	// A tenant's group's members: the rows whose template names it.
	if m, err := s.EnvCtlGroupMembers(bg, env.ID, web); err != nil || !slices.Equal(m, []string{"fd00::2", "fd00::9"}) {
		t.Fatalf("group members %v %v", m, err)
	}
	if m, err := s.EnvCtlGroupMembers(bg, other.ID, web); err != nil || len(m) != 0 {
		t.Fatalf("group members in another environment %v %v", m, err)
	}
	if rows, err := s.EnvCtlSandboxes(bg, env.ID); err != nil || len(rows) != 2 {
		t.Fatalf("instance rows %v %v", rows, err)
	}

	// Pin and idle: written without a lifecycle write.
	before := get(t, s, a.ID)
	if changed, err := s.EnvCtlSetPin(bg, a.ID, "n1"); err != nil || !changed {
		t.Fatalf("pin %v %v", changed, err)
	}
	if changed, err := s.EnvCtlSetPin(bg, a.ID, "n1"); err != nil || changed {
		t.Fatalf("the same pin again %v %v", changed, err)
	}
	if changed, err := s.EnvCtlSetIdle(bg, a.ID, 900); err != nil || !changed {
		t.Fatalf("idle %v %v", changed, err)
	}
	if changed, err := s.EnvCtlSetIdle(bg, a.ID, 900); err != nil || changed {
		t.Fatalf("the same idle again %v %v", changed, err)
	}
	after := get(t, s, a.ID)
	if after.PinNode != "n1" || after.IdleAfterS == nil || *after.IdleAfterS != 900 || after.Gen != before.Gen {
		t.Fatalf("row %+v", after)
	}

	// Templated rows on a node: by node, op target or lost_from.
	place(t, s, a.ID, "n1", model.StateRunning)
	exec(t, s, `UPDATE sandboxes SET pod_template = '{"command":["sleep","infinity"]}'::jsonb WHERE id = $1`, sbx.ID)
	place(t, s, sbx.ID, "n1", model.StateRunning)
	on, err := s.EnvCtlTemplatedOnNode(bg, "n1")
	if err != nil || len(on) != 2 {
		t.Fatalf("on n1 %v %v", on, err)
	}

	// Starved rows: pending without capacity for long enough.
	exec(t, s, `UPDATE sandboxes SET status_description = 'no capacity (retryable): no feasible node',
		pending_since = now() - interval '3 minutes' WHERE id = $1`, b.ID)
	starved, err := s.EnvCtlStarved(bg, env.ID, 2*time.Minute)
	if err != nil || len(starved) != 1 || starved[b.ID] == "" {
		t.Fatalf("starved %v %v", starved, err)
	}
	if starved, _ := s.EnvCtlStarved(bg, env.ID, 5*time.Minute); len(starved) != 0 {
		t.Fatalf("starved for 5 minutes %v", starved)
	}

	// Incidents closed by deployment and by object.
	open := func(object string, deployment int, condition string) int64 {
		i, _, err := s.OpenIncident(bg, store.EnvIncident{EnvID: env.ID, Object: object, Deployment: deployment, Condition: condition})
		if err != nil {
			t.Fatal(err)
		}
		return i.ID
	}
	open("service/web", 1, model.ConditionRolloutFailed)
	open("job/m", 2, model.ConditionJobFailed)
	open("service/web", 3, model.ConditionRolloutFailed)
	open("service/web", 1, model.ConditionCrashLoop)
	if n, err := s.EnvCtlCloseIncidents(bg, env.ID, 3, model.ConditionRolloutFailed, model.ConditionJobFailed); err != nil || n != 2 {
		t.Fatalf("closed %d %v", n, err)
	}
	if n, err := s.EnvCtlCloseObjectIncidents(bg, env.ID, "service/web", model.ConditionCrashLoop); err != nil || n != 1 {
		t.Fatalf("closed %d %v", n, err)
	}
	if left, _ := s.CountOpenIncidents(bg, env.ID); left != 1 {
		t.Fatalf("%d open", left)
	}

	// Running runs.
	if _, err := s.InsertJobRun(bg, store.EnvJobRun{EnvID: env.ID, Job: "m", TemplateHash: "h", Deployment: 1}); err != nil {
		t.Fatal(err)
	}
	if runs, err := s.EnvCtlRunningJobRuns(bg, env.ID); err != nil || len(runs) != 1 {
		t.Fatalf("runs %v %v", runs, err)
	}

	// A deployment's wait for its job: the latest end of the runs of other
	// deployments that ended after its start (now() while one runs).
	var start time.Time
	if err := s.Q().QueryRow(bg, "SELECT now() - interval '1 hour'").Scan(&start); err != nil {
		t.Fatal(err)
	}
	waited := func(hash string, deployment int) time.Time {
		t.Helper()
		until, err := s.EnvCtlJobWaitedUntil(bg, env.ID, "w", hash, deployment, start)
		if err != nil {
			t.Fatal(err)
		}
		return until
	}
	if until := waited("h2", 2); !until.IsZero() {
		t.Fatalf("waited until %v without a run", until)
	}
	old, err := s.InsertJobRun(bg, store.EnvJobRun{EnvID: env.ID, Job: "w", TemplateHash: "h1", Deployment: 1})
	if err != nil {
		t.Fatal(err)
	}
	if until := waited("h2", 2); until.Before(start.Add(time.Hour)) {
		t.Fatalf("a running run of deployment 1: waited until %v", until)
	}
	if until := waited("h1", 1); !until.IsZero() {
		t.Fatalf("its own deployment's run is waited for: %v", until)
	}
	exec(t, s, `UPDATE env_job_runs SET phase = 'failed', finished_at = $2 WHERE id = $1`, old.ID, start.Add(9*time.Second))
	if until := waited("h2", 2); !until.Equal(start.Add(9 * time.Second)) {
		t.Fatalf("a run that ended after the start: waited until %v", until)
	}
	exec(t, s, `UPDATE env_job_runs SET finished_at = $2 WHERE id = $1`, old.ID, start.Add(-time.Second))
	if until := waited("h2", 2); !until.IsZero() {
		t.Fatalf("a run that ended before the start: waited until %v", until)
	}
	// A template that had succeeded before the start was never waited
	// for, unless the hash is not known.
	exec(t, s, `UPDATE env_job_runs SET finished_at = $2 WHERE id = $1`, old.ID, start.Add(9*time.Second))
	done, err := s.InsertJobRun(bg, store.EnvJobRun{EnvID: env.ID, Job: "w", TemplateHash: "h2", Deployment: 1,
		Phase: model.RunSucceeded})
	if err != nil {
		t.Fatal(err)
	}
	exec(t, s, `UPDATE env_job_runs SET finished_at = $2 WHERE id = $1`, done.ID, start.Add(-time.Minute))
	if until := waited("h2", 2); !until.IsZero() {
		t.Fatalf("a template that had succeeded: waited until %v", until)
	}
	if until := waited("", 2); !until.Equal(start.Add(9 * time.Second)) {
		t.Fatalf("an unknown hash: waited until %v", until)
	}

	// Volumes marked deleting.
	if _, err := s.InsertEnvVolume(bg, store.EnvVolume{EnvID: env.ID, Object: "addon/pg", VolumeID: uuid.New(), SizeMiB: 1024}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.EnvCtlMarkVolumesDeleting(bg, env.ID); err != nil || n != 1 {
		t.Fatalf("marked %d %v", n, err)
	}
	if n, _ := s.EnvCtlMarkVolumesDeleting(bg, env.ID); n != 0 {
		t.Fatalf("marked again %d", n)
	}
	// A touched volume goes to the end of the deleting ones.
	if _, err := s.InsertEnvVolume(bg, store.EnvVolume{EnvID: env.ID, Object: "addon/kv", VolumeID: uuid.New(), SizeMiB: 256,
		Deleting: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnvCtlTouchVolume(bg, env.ID, "addon/pg"); err != nil {
		t.Fatal(err)
	}
	if vols, err := s.DeletingEnvVolumes(bg, 10); err != nil || len(vols) != 2 || vols[0].Object != "addon/kv" {
		t.Fatalf("deleting volumes %+v %v", vols, err)
	}
	if err := s.EnvCtlTouchVolume(bg, env.ID, "addon/none"); err != store.ErrNoRow {
		t.Fatalf("touch a volume that is not there: %v", err)
	}
	kv, _ := s.GetEnvVolume(bg, env.ID, "addon/kv")
	byID, err := s.EnvCtlVolumesByID(bg, []uuid.UUID{kv.VolumeID, uuid.New()})
	if err != nil || len(byID) != 1 || byID[kv.VolumeID].Object != "addon/kv" {
		t.Fatalf("volumes by id %+v %v", byID, err)
	}
	if byID, err := s.EnvCtlVolumesByID(bg, nil); err != nil || len(byID) != 0 {
		t.Fatalf("volumes by no id %+v %v", byID, err)
	}

	// A touched deleting environment goes to the end of the deleting
	// ones; one that is not deleting is not touched.
	exec(t, s, `UPDATE environments SET deleting = true, updated_at = now() - interval '1 minute' WHERE id IN ($1, $2)`,
		env.ID, other.ID)
	first := env.ID
	if other.ID.String() < env.ID.String() {
		first = other.ID
	}
	if d, err := s.DeletingEnvironments(bg, 10); err != nil || len(d) != 2 || d[0].ID != first {
		t.Fatalf("deleting environments %+v %v", d, err)
	}
	if err := s.EnvCtlTouchEnvironment(bg, first); err != nil {
		t.Fatal(err)
	}
	if d, err := s.DeletingEnvironments(bg, 10); err != nil || len(d) != 2 || d[1].ID != first {
		t.Fatalf("deleting environments after a touch %+v %v", d, err)
	}
	if d, err := s.DeletingEnvironments(bg, 1); err != nil || len(d) != 1 || d[0].ID == first {
		t.Fatalf("the first deleting environment after a touch %+v %v", d, err)
	}
	live := mkEnv(t, s, "acme", "live")
	if err := s.EnvCtlTouchEnvironment(bg, live.ID); err != store.ErrNoRow {
		t.Fatalf("touch an environment that is not deleting: %v", err)
	}
	if err := s.EnvCtlTouchEnvironment(bg, uuid.New()); err != store.ErrNoRow {
		t.Fatalf("touch an environment that is not there: %v", err)
	}

	// Which environments and groups' rows exist.
	gone, goneGroup := uuid.New(), uuid.New()
	row, err := s.SecurityGroupID(bg, env.ID, "default")
	if err != nil {
		t.Fatal(err)
	}
	envs, groups, err := s.EnvCtlExisting(bg, []uuid.UUID{env.ID, gone}, []uuid.UUID{row, goneGroup})
	if err != nil || !envs[env.ID] || envs[gone] || !groups[row] || groups[goneGroup] {
		t.Fatalf("existing %v %v %v", envs, groups, err)
	}
	if envs, groups, err := s.EnvCtlExisting(bg, nil, nil); err != nil || len(envs) != 0 || len(groups) != 0 {
		t.Fatalf("existing of none %v %v %v", envs, groups, err)
	}

	goneSbx, goneLegacy := podid.Mint(), legacyID()
	// The inputs of ing groups: an instance's environment, kind, object
	// and deployment; a sandbox without an instance row with none; a sandbox
	// that is gone missing.
	if _, err := s.InsertEnvInstance(bg, store.EnvInstance{SandboxID: a.ID, EnvID: env.ID, Kind: "service", Object: "web",
		TemplateHash: "h", Deployment: 3}); err != nil {
		t.Fatal(err)
	}
	inputs, err := s.EnvCtlIngressInputs(bg, []podid.ID{a.ID, b.ID, sbx.ID, goneSbx, goneLegacy})
	if err != nil || len(inputs) != 3 ||
		inputs[a.ID] != (store.IngressInput{EnvID: env.ID, Kind: "service", Object: "web", Deployment: 3}) ||
		inputs[b.ID] != (store.IngressInput{}) || inputs[sbx.ID] != (store.IngressInput{}) {
		t.Fatalf("ingress inputs %+v %v", inputs, err)
	}
	if inputs, err := s.EnvCtlIngressInputs(bg, nil); err != nil || len(inputs) != 0 {
		t.Fatalf("ingress inputs of none %+v %v", inputs, err)
	}
	// The sandboxes' nodes: "" while a sandbox has none; a sandbox that
	// is gone missing.
	nodes, err := s.EnvCtlSandboxNodes(bg, []podid.ID{a.ID, b.ID, goneSbx, goneLegacy})
	if err != nil || len(nodes) != 2 || nodes[a.ID] != "n1" || nodes[b.ID] != "" {
		t.Fatalf("sandbox nodes %v %v", nodes, err)
	}
	if nodes, err := s.EnvCtlSandboxNodes(bg, nil); err != nil || len(nodes) != 0 {
		t.Fatalf("sandbox nodes of none %v %v", nodes, err)
	}
}
