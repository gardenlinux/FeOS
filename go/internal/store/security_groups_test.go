// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// An environment has default from its insert; a deployment adds the rows
// of its spec's groups, which keep their IDs; Ensure adds one on demand.
func TestSecurityGroupRows(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	env := mkEnv(t, s, "acme", "stage")
	ids, err := s.SecurityGroupIDs(bg, env.ID)
	if err != nil || len(ids) != 1 || ids["default"] == uuid.Nil {
		t.Fatalf("new environment %v %v", ids, err)
	}
	def := ids["default"]
	if _, err := s.InsertDeployment(bg, store.EnvDeployment{EnvID: env.ID, Number: 1, Origin: model.OriginApply,
		Spec: []byte(`{"securityGroups": {"default": {}, "web": {}, "db": {"out": []}}}`)}); err != nil {
		t.Fatal(err)
	}
	ids, err = s.SecurityGroupIDs(bg, env.ID)
	if err != nil || len(ids) != 3 || ids["default"] != def || ids["web"] == uuid.Nil || ids["db"] == uuid.Nil {
		t.Fatalf("after deployment 1 %v %v", ids, err)
	}
	// A later deployment without web keeps its row, and its ID.
	if _, err := s.InsertDeployment(bg, store.EnvDeployment{EnvID: env.ID, Number: 2, Base: 1, Origin: model.OriginApply,
		Spec: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if id, err := s.SecurityGroupID(bg, env.ID, "web"); err != nil || id != ids["web"] {
		t.Fatalf("web %v %v", id, err)
	}
	if _, err := s.SecurityGroupID(bg, env.ID, "nope"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("nope %v", err)
	}
	made, err := s.EnsureSecurityGroup(bg, env.ID, "cache")
	if err != nil || made == uuid.Nil {
		t.Fatalf("ensure %v %v", made, err)
	}
	if again, err := s.EnsureSecurityGroup(bg, env.ID, "cache"); err != nil || again != made {
		t.Fatalf("ensure again %v %v", again, err)
	}
	if _, err := s.EnsureSecurityGroup(bg, uuid.New(), "default"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("no environment %v", err)
	}
	// Another environment's groups are its own.
	other := mkEnv(t, s, "acme", "prod")
	if id, err := s.SecurityGroupID(bg, other.ID, "default"); err != nil || id == def {
		t.Fatalf("prod's default %v %v", id, err)
	}
}

// An own sandbox's environment and groups; the groups an environment's
// sandboxes and its queued and running jobs name.
func TestOwnSandbox(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkTenant(t, s, "other")
	dev := mkEnv(t, s, "acme", "dev")
	preview := mkEnv(t, s, "acme", "preview")
	sb := mkSandboxIn(t, s, "acme", dev.ID, podid.Mint())

	got, err := s.GetOwnSandbox(bg, "acme", sb.ID)
	if err != nil || got.ID != sb.ID || got.EnvID != dev.ID || got.EnvName != "dev" ||
		!slices.Equal(got.SecurityGroups, []string{"default"}) {
		t.Fatalf("in dev %+v %v", got, err)
	}
	if _, err := s.GetOwnSandbox(bg, "other", sb.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("another tenant's %v", err)
	}

	// An instance row is not a sandbox of the tenant's.
	inst := mkInstance(t, s, dev)
	if _, err := s.GetOwnSandbox(bg, "acme", inst.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("instance %v", err)
	}

	// Its groups, written; a move pending.
	if ok, err := s.SetSandboxGroups(bg, sb.ID, []string{"db", "web"}); err != nil || !ok {
		t.Fatalf("set %v %v", ok, err)
	}
	if ok, err := s.SetSandboxGroups(bg, inst.ID, []string{"web"}); err != nil || ok {
		t.Fatalf("an instance's groups set %v %v", ok, err)
	}
	if row := get(t, s, sb.ID); !slices.Equal(row.SecurityGroups, []string{"db", "web"}) || !row.GroupsPending {
		t.Fatalf("row %+v", row)
	}
	if got, err := s.GetOwnSandbox(bg, "acme", sb.ID); err != nil || !slices.Equal(got.SecurityGroups, []string{"db", "web"}) {
		t.Fatalf("with groups %+v %v", got, err)
	}
	// The begin of its next create or restore makes its template of them
	// (rows added for names that have none) and clears the mark; a row
	// without the mark, or an instance, stays as it is.
	pending := get(t, s, sb.ID)
	applied, err := s.ApplySandboxGroups(bg, pending)
	if err != nil || applied.GroupsPending || applied.Gen != pending.Gen {
		t.Fatalf("applied %+v %v", applied, err)
	}
	db, _ := s.SecurityGroupID(bg, dev.ID, "db")
	web, _ := s.SecurityGroupID(bg, dev.ID, "web")
	if want := model.SandboxTemplate(dev.ID, []uuid.UUID{db, web}); db == uuid.Nil || web == uuid.Nil ||
		!reflect.DeepEqual(applied.PodTemplate, want) {
		t.Fatalf("template %+v, want %+v", applied.PodTemplate, want)
	}
	if again, err := s.ApplySandboxGroups(bg, applied); err != nil || !reflect.DeepEqual(again, applied) {
		t.Fatalf("applied again %+v %v", again, err)
	}
	if _, err := s.ApplySandboxGroups(bg, pending); !errors.Is(err, store.ErrStale) {
		t.Fatalf("a stale read applied: %v", err)
	}
	if out, err := s.ApplySandboxGroups(bg, inst); err != nil || !reflect.DeepEqual(out, inst) {
		t.Fatalf("an instance applied %+v %v", out, err)
	}

	// A job of dev that runs counts, one that ended does not.
	job := func(phase string) podid.ID {
		t.Helper()
		var id podid.ID
		must(t, s.InTx(bg, func(tx *store.Store) error {
			idle := 0
			row, err := tx.CreateMintedSandbox(bg, func(id podid.ID) store.NewSandbox {
				return store.NewSandbox{ID: id, Owner: "acme", Image: "img", VCPUs: 1, MemoryMiB: 512, EnvironmentID: dev.ID,
					SecurityGroups: []string{"migrate"}, PodTemplate: &model.PodTemplate{}, IdleAfterS: &idle, TenantJob: true}
			})
			if err != nil {
				return err
			}
			_, err = tx.InsertTenantJob(bg, store.TenantJob{ID: row.ID, Owner: "acme", EnvID: dev.ID,
				SecurityGroups: []string{"migrate"}, RequestHash: "h", Image: "img", ImageRef: "img"}, nil, time.Minute)
			id = row.ID
			return err
		}))
		if phase != model.JobQueued {
			exec(t, s, "UPDATE jobs SET phase = $2, finished_at = now() WHERE id = $1", id, phase)
		}
		return id
	}
	running := job(model.JobQueued)
	job(model.JobSucceeded)
	users, err := s.SecurityGroupUsers(bg, dev.ID)
	if err != nil || len(users) != 2 || users[0].Kind != store.GroupUserJob || users[0].ID != running ||
		!slices.Equal(users[0].SecurityGroups, []string{"migrate"}) || users[1].Kind != store.GroupUserSandbox ||
		users[1].ID != sb.ID || !slices.Equal(users[1].SecurityGroups, []string{"db", "web"}) {
		t.Fatalf("users %+v %v", users, err)
	}
	if users, err := s.SecurityGroupUsers(bg, preview.ID); err != nil || len(users) != 0 {
		t.Fatalf("preview's users %+v %v", users, err)
	}
	if u, err := s.EnvironmentInUse(bg, dev.ID); err != nil || !slices.Equal(u.Sandboxes, []podid.ID{sb.ID}) ||
		!slices.Equal(u.Jobs, []podid.ID{running}) || u.Empty() {
		t.Fatalf("in use %+v %v", u, err)
	}
	if u, err := s.EnvironmentInUse(bg, preview.ID); err != nil || !u.Empty() {
		t.Fatalf("preview in use %+v %v", u, err)
	}

	// The groups a sandbox may name: the deployed deployment's that the
	// latest keeps, and default always.
	if names, err := s.DeployedSecurityGroups(bg, preview.ID); err != nil || !slices.Equal(names, []string{"default"}) {
		t.Fatalf("no deployment %v %v", names, err)
	}
	if _, err := s.InsertDeployment(bg, store.EnvDeployment{EnvID: preview.ID, Number: 1, Origin: model.OriginApply,
		Spec: []byte(`{"securityGroups": {"web": {}, "db": {}}}`)}); err != nil {
		t.Fatal(err)
	}
	exec(t, s, "UPDATE environments SET deployment = 1 WHERE id = $1", preview.ID)
	if names, err := s.DeployedSecurityGroups(bg, preview.ID); err != nil || !slices.Equal(names, []string{"default"}) {
		t.Fatalf("not deployed yet %v %v", names, err)
	}
	exec(t, s, "UPDATE environments SET deployed = 1 WHERE id = $1", preview.ID)
	if names, err := s.DeployedSecurityGroups(bg, preview.ID); err != nil ||
		!slices.Equal(names, []string{"db", "default", "web"}) {
		t.Fatalf("deployed %v %v", names, err)
	}
	// One on its way that removes web: web no longer, and the group it
	// adds not yet.
	if _, err := s.InsertDeployment(bg, store.EnvDeployment{EnvID: preview.ID, Number: 2, Origin: model.OriginApply,
		Spec: []byte(`{"securityGroups": {"db": {}, "tools": {}}}`)}); err != nil {
		t.Fatal(err)
	}
	exec(t, s, "UPDATE environments SET deployment = 2 WHERE id = $1", preview.ID)
	if names, err := s.DeployedSecurityGroups(bg, preview.ID); err != nil ||
		!slices.Equal(names, []string{"db", "default"}) {
		t.Fatalf("on its way %v %v", names, err)
	}
	if _, err := s.DeployedSecurityGroups(bg, uuid.New()); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("no environment %v", err)
	}
	if names, err := s.EnvironmentNames(bg, []uuid.UUID{dev.ID, preview.ID, uuid.New()}); err != nil || len(names) != 2 ||
		names[dev.ID] != "dev" {
		t.Fatalf("names %v %v", names, err)
	}
}
