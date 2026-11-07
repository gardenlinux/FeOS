// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
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

// An own sandbox's environment and groups; a sandbox made before ADR
// 0035 is in the environment that names it, the most recent of several.
func TestOwnSandbox(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkTenant(t, s, "other")
	dev := mkEnv(t, s, "acme", "dev")
	preview := mkEnv(t, s, "acme", "preview")
	sb := mkSandbox(t, s, "acme")

	got, err := s.GetOwnSandbox(bg, "acme", sb.ID)
	if err != nil || got.ID != sb.ID || got.EnvID != uuid.Nil || !slices.Equal(got.SecurityGroups, []string{"default"}) {
		t.Fatalf("unnamed %+v %v", got, err)
	}
	if _, err := s.GetOwnSandbox(bg, "other", sb.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("another tenant's %v", err)
	}
	attached := `{"services": {"vite": {"runtime": "attached", "sandbox": "` + sb.ID.String() + `", "command": ["vite"]}}}`
	for i, e := range []store.Environment{dev, preview} {
		if _, err := s.InsertDeployment(bg, store.EnvDeployment{EnvID: e.ID, Number: 1, Origin: model.OriginApply,
			Spec: []byte(attached)}); err != nil {
			t.Fatal(err)
		}
		exec(t, s, "UPDATE environments SET deployment = 1, created_at = now() + $2::int * interval '1 second' WHERE id = $1",
			e.ID, i)
	}
	if got, err := s.GetOwnSandbox(bg, "acme", sb.ID); err != nil || got.EnvID != preview.ID || got.EnvName != "preview" {
		t.Fatalf("named twice %+v %v", got, err)
	}

	// An instance row is not a sandbox of the tenant's.
	inst := mkInstance(t, s, dev)
	exec(t, s, `INSERT INTO env_instances (sandbox_id, env_id, kind, object, template_hash, deployment)
		VALUES ($1, $2, 'service', 'x', 'h', 1)`, inst.ID, dev.ID)
	if _, err := s.GetOwnSandbox(bg, "acme", inst.ID); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("instance %v", err)
	}

	// A sandbox of an environment (ADR 0035) names its groups by their IDs.
	web, err := s.EnsureSecurityGroup(bg, dev.ID, "web")
	must(t, err)
	own := mkSandbox(t, s, "acme")
	exec(t, s, `UPDATE sandboxes SET environment_id = $2, pod_template = $3 WHERE id = $1`, own.ID, dev.ID,
		`{"securityGroups": ["`+model.EnvGroupID(dev.ID)+`", "`+model.SecurityGroupID(web)+`"]}`)
	if got, err := s.GetOwnSandbox(bg, "acme", own.ID); err != nil || got.EnvID != dev.ID ||
		!slices.Equal(got.SecurityGroups, []string{"web"}) {
		t.Fatalf("in dev %+v %v", got, err)
	}
	users, err := s.SecurityGroupUsers(bg, dev.ID)
	if err != nil || len(users) != 1 || users[0].ID != own.ID || users[0].Kind != store.GroupUserSandbox ||
		!slices.Equal(users[0].SecurityGroups, []string{"web"}) {
		t.Fatalf("users %+v %v", users, err)
	}
	if users, err := s.SecurityGroupUsers(bg, preview.ID); err != nil || len(users) != 0 {
		t.Fatalf("preview's users %+v %v", users, err)
	}
}
