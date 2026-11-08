// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// An own sandbox's environment; what sandboxes and jobs reach, written
// and read back; the environment's reachers and what holds it.
func TestOwnSandboxAndReaches(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	mkTenant(t, s, "other")
	dev := mkEnv(t, s, "acme", "dev")
	preview := mkEnv(t, s, "acme", "preview")
	sb := mkSandboxIn(t, s, "acme", dev.ID, podid.Mint())

	got, err := s.GetOwnSandbox(bg, "acme", sb.ID)
	if err != nil || got.ID != sb.ID || got.EnvID != dev.ID || got.EnvName != "dev" {
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

	// Nothing reached, the internet, by default.
	if row := get(t, s, sb.ID); row.Reaches.All || len(row.Reaches.Entries) != 0 || row.Egress != nil {
		t.Fatalf("default %+v %+v", row.Reaches, row.Egress)
	}
	// Written in place; an instance's never.
	want := spec.Reaches{Entries: []spec.Use{{Service: "api", Port: spec.PortRef{Name: "http"}}, {Addon: "db"}}}
	if ok, err := s.SetSandboxReaches(bg, sb.ID, want); err != nil || !ok {
		t.Fatalf("set %v %v", ok, err)
	}
	if ok, err := s.SetSandboxReaches(bg, inst.ID, spec.Reaches{All: true}); err != nil || ok {
		t.Fatalf("an instance's reaches set %v %v", ok, err)
	}
	before := get(t, s, sb.ID)
	if b, _ := json.Marshal(before.Reaches); string(b) != `[{"service":"api","port":"http"},{"addon":"db"}]` {
		t.Fatalf("read back %s", b)
	}
	if ok, err := s.SetSandboxReaches(bg, sb.ID, spec.Reaches{All: true}); err != nil || !ok {
		t.Fatalf("set all %v %v", ok, err)
	}
	if row := get(t, s, sb.ID); !row.Reaches.All || row.Gen != before.Gen {
		t.Fatalf("all %+v gen %d → %d", row.Reaches, before.Gen, row.Gen)
	}

	// A sandbox made with an egress keeps it.
	egress := &spec.Egress{Domains: []string{"api.stripe.com"},
		Blocks: []spec.Block{{CIDR: "203.0.113.0/24", Except: []string{"203.0.113.8/29"}}}}
	var made store.Sandbox
	must(t, s.InTx(bg, func(tx *store.Store) error {
		var err error
		made, err = tx.CreateMintedSandbox(bg, func(id podid.ID) store.NewSandbox {
			return store.NewSandbox{ID: id, Owner: "acme", Image: "img", VCPUs: 1, MemoryMiB: 512, EnvironmentID: dev.ID,
				Egress: egress, Reaches: spec.Reaches{Entries: []spec.Use{{Addon: "db"}}}}
		})
		return err
	}))
	if row := get(t, s, made.ID); row.Egress == nil || !slices.Equal(row.Egress.Domains, egress.Domains) ||
		len(row.Egress.Blocks) != 1 || row.Egress.Blocks[0].Except[0] != "203.0.113.8/29" ||
		len(row.Reaches.Entries) != 1 {
		t.Fatalf("made %+v %+v", row.Egress, row.Reaches)
	}

	// A job of dev: its record and its pod carry its reaches and egress.
	var job podid.ID
	must(t, s.InTx(bg, func(tx *store.Store) error {
		idle := 0
		none := &spec.Egress{Mode: spec.EgressNone}
		row, err := tx.CreateMintedSandbox(bg, func(id podid.ID) store.NewSandbox {
			return store.NewSandbox{ID: id, Owner: "acme", Image: "img", VCPUs: 1, MemoryMiB: 512, EnvironmentID: dev.ID,
				Reaches: spec.Reaches{All: true}, Egress: none, PodTemplate: &model.PodTemplate{}, IdleAfterS: &idle,
				TenantJob: true}
		})
		if err != nil {
			return err
		}
		r, err := tx.InsertTenantJob(bg, store.TenantJob{ID: row.ID, Owner: "acme", EnvID: dev.ID,
			Reaches: spec.Reaches{All: true}, Egress: none, RequestHash: "h", Image: "img", ImageRef: "img"}, nil,
			time.Minute)
		if err == nil && (!r.Reaches.All || r.Egress.EffectiveMode() != spec.EgressNone) {
			t.Errorf("job record %+v %+v", r.Reaches, r.Egress)
		}
		job = row.ID
		return err
	}))

	reachers, err := s.EnvCtlReachers(bg, dev.ID)
	if err != nil || len(reachers) != 3 {
		t.Fatalf("reachers %+v %v", reachers, err)
	}
	by := map[podid.ID]store.Reacher{}
	for _, r := range reachers {
		by[r.ID] = r
	}
	if r := by[sb.ID]; !r.Reaches.All || r.TenantJob || r.Egress != nil {
		t.Fatalf("sandbox %+v", r)
	}
	if r := by[job]; !r.TenantJob || !r.Reaches.All || r.Egress.EffectiveMode() != spec.EgressNone {
		t.Fatalf("job %+v", r)
	}
	if r, ok := by[made.ID]; !ok || r.Egress.EffectiveMode() != spec.EgressRestricted {
		t.Fatalf("made %+v", r)
	}
	if r, err := s.EnvCtlReachers(bg, preview.ID); err != nil || len(r) != 0 {
		t.Fatalf("preview %+v %v", r, err)
	}

	if u, err := s.EnvironmentInUse(bg, dev.ID); err != nil || len(u.Sandboxes) != 2 ||
		!slices.Equal(u.Jobs, []podid.ID{job}) || u.Empty() {
		t.Fatalf("in use %+v %v", u, err)
	}
	if u, err := s.EnvironmentInUse(bg, preview.ID); err != nil || !u.Empty() {
		t.Fatalf("preview in use %+v %v", u, err)
	}

	// The deployed deployment's spec, what reaches may name.
	if raw, err := s.DeployedSpec(bg, preview.ID); err != nil || raw != nil {
		t.Fatalf("no deployment %s %v", raw, err)
	}
	if _, err := s.InsertDeployment(bg, store.EnvDeployment{EnvID: preview.ID, Number: 1, Origin: model.OriginApply,
		Spec: []byte(`{"addons": {"db": {"type": "postgres"}}}`)}); err != nil {
		t.Fatal(err)
	}
	exec(t, s, "UPDATE environments SET deployment = 1, deployed = 1 WHERE id = $1", preview.ID)
	if raw, err := s.DeployedSpec(bg, preview.ID); err != nil || len(raw) == 0 {
		t.Fatalf("deployed %s %v", raw, err)
	}
	if _, err := s.DeployedSpec(bg, uuid.New()); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("no environment %v", err)
	}
	if names, err := s.EnvironmentNames(bg, []uuid.UUID{dev.ID, preview.ID, uuid.New()}); err != nil || len(names) != 2 ||
		names[dev.ID] != "dev" {
		t.Fatalf("names %v %v", names, err)
	}
}

// A template's groups of the first version of ADR 0034 are dropped at
// the next create or restore; its gen stays, a stale read is refused.
func TestDropLegacyGroups(t *testing.T) {
	s, _ := newStore(t)
	mkTenant(t, s, "acme")
	dev := mkEnv(t, s, "acme", "dev")
	sb := mkSandboxIn(t, s, "acme", dev.ID, podid.Mint())
	legacy := "sg-" + uuid.NewString()
	wl := model.SandboxWorkloadGroupID(sb.ID.String())
	exec(t, s, `UPDATE sandboxes SET pod_template = $2 WHERE id = $1`, sb.ID,
		`{"securityGroups": ["env-`+dev.ID.String()+`", "`+legacy+`", "`+wl+`"], "nameTables": ["env-`+dev.ID.String()+`"]}`)
	row := get(t, s, sb.ID)
	out, err := s.DropLegacyGroups(bg, row)
	if err != nil || !slices.Equal(out.PodTemplate.SecurityGroups, []string{model.EnvGroupID(dev.ID), wl}) ||
		out.Gen != row.Gen || !slices.Equal(out.PodTemplate.NameTables, []string{model.EnvTableID(dev.ID)}) {
		t.Fatalf("dropped %+v %v", out.PodTemplate, err)
	}
	if again, err := s.DropLegacyGroups(bg, out); err != nil || !slices.Equal(again.PodTemplate.SecurityGroups,
		out.PodTemplate.SecurityGroups) {
		t.Fatalf("again %+v %v", again.PodTemplate, err)
	}
	exec(t, s, `UPDATE sandboxes SET pod_template = $2, gen = gen + 1 WHERE id = $1`, sb.ID,
		`{"securityGroups": ["`+legacy+`"]}`)
	if _, err := s.DropLegacyGroups(bg, row); !errors.Is(err, store.ErrStale) {
		t.Fatalf("a stale read: %v", err)
	}
	if plain := mkSandbox(t, s, "acme"); plain.PodTemplate != nil {
		if out, err := s.DropLegacyGroups(bg, plain); err != nil || out.ID != plain.ID {
			t.Fatalf("without a template %+v %v", out, err)
		}
	}
}
