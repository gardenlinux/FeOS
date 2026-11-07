// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// 0032 puts every sandbox and job into an environment (ADR 0035): a
// sandbox that an environment's attached service names into the most
// recent such environment, every other one and every job into the
// tenant's environment "sandboxes", made with its group default (a tenant
// that has one keeps it); instances are marked so; all are in default,
// with log labels of their environment, and a sandbox with a pod is to be
// moved once.
func TestSandboxesInEnvironmentsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 32)

	exec := func(q string) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const (
		dev     = "00000000-0000-4000-8000-000000000001"
		preview = "00000000-0000-4000-8000-000000000002"
		gone    = "00000000-0000-4000-8000-000000000003"
		own     = "00000000-0000-4000-8000-000000000009" // beta's own environment "sandboxes"
	)
	exec("INSERT INTO tenants (id) VALUES ('acme'), ('beta'), ('idle')")
	exec(`INSERT INTO environments (id, owner, name, deployment, created_at) VALUES
		('` + dev + `', 'acme', 'dev', 1, now() - interval '2 hours'),
		('` + preview + `', 'acme', 'preview', 1, now() - interval '1 hour'),
		('` + gone + `', 'acme', 'gone', 1, now()),
		('` + own + `', 'beta', 'sandboxes', 0, now())`)
	exec("UPDATE environments SET deleting = true WHERE id = '" + gone + "'")
	attached := func(sbx string) string {
		return `{"services": {"vite": {"runtime": "attached", "sandbox": "` + sbx + `", "command": ["vite"]}}}`
	}
	exec(`INSERT INTO env_deployments (env_id, number, spec, base, origin, state) VALUES
		('` + dev + `', 1, '` + attached("wsa00000001") + `', 0, 'apply', 'deployed'),
		('` + preview + `', 1, '` + attached("wsa00000001") + `', 0, 'apply', 'deployed'),
		('` + gone + `', 1, '` + attached("wsb00000001") + `', 0, 'apply', 'deployed')`)
	exec(`INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib, client_state, environment_id, tenant_job) VALUES
		('wsa00000001', 'acme', 'img', 1, 512, 'running', NULL, false),
		('wsb00000001', 'acme', 'img', 1, 512, 'hibernated', NULL, false),
		('nst00000001', 'acme', 'img', 1, 512, 'running', '` + dev + `', false),
		('jb000000001', 'acme', 'img', 1, 512, 'running', NULL, true),
		('betabx00001', 'beta', 'img', 1, 512, 'paused', NULL, false)`)
	exec(`UPDATE sandboxes SET log_labels = '{"ferun.tenant": "acme", "ferun.job": "jb000000001"}' WHERE id = 'jb000000001'`)
	exec(`INSERT INTO jobs (id, owner, request_hash, image, image_ref, vcpus, memory_mib, phase, deadline_at) VALUES
		('jb000000001', 'acme', 'h', 'img', 'img', 1, 512, 'running', now() + interval '1 hour')`)
	migrateRange(t, d, 32, 33)

	var boxes string
	if err := d.Pool.QueryRow(ctx, "SELECT id FROM environments WHERE owner = 'acme' AND name = 'sandboxes'").
		Scan(&boxes); err != nil {
		t.Fatalf("acme's environment sandboxes: %v", err)
	}
	var made int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM environments WHERE name = 'sandboxes'`).Scan(&made); err != nil ||
		made != 2 {
		t.Fatalf("environments sandboxes %d %v", made, err)
	}
	var defaults int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM security_groups g JOIN environments e ON e.id = g.env_id
		WHERE e.name = 'sandboxes' AND g.name = 'default'`).Scan(&defaults); err != nil || defaults != 2 {
		t.Fatalf("their groups default %d %v", defaults, err)
	}

	type row struct {
		env      string
		instance bool
		groups   []string
		pending  bool
		labels   map[string]string
	}
	read := func(id string) row {
		t.Helper()
		var r row
		var labels []byte
		if err := d.Pool.QueryRow(ctx, `SELECT environment_id::text, instance, security_groups, groups_pending, log_labels
			FROM sandboxes WHERE id = $1`, id).Scan(&r.env, &r.instance, &r.groups, &r.pending, &labels); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if err := json.Unmarshal(labels, &r.labels); err != nil {
			t.Fatal(err)
		}
		return r
	}
	inDefault := []string{"default"}
	// Named by dev and preview: the more recent, preview; moved once.
	if r := read("wsa00000001"); r.env != preview || r.instance || !slices.Equal(r.groups, inDefault) || !r.pending ||
		r.labels["ferun.environment"] != preview || r.labels["ferun.kind"] != "sandbox" ||
		r.labels["ferun.object"] != "wsa00000001" || r.labels["ferun.tenant"] != "acme" {
		t.Fatalf("wsa00000001 %+v", r)
	}
	// Named by an environment being deleted only: sandboxes; hibernated,
	// so its restore takes its groups.
	if r := read("wsb00000001"); r.env != boxes || !slices.Equal(r.groups, inDefault) || r.pending {
		t.Fatalf("wsb00000001 %+v", r)
	}
	if r := read("nst00000001"); r.env != dev || !r.instance || len(r.groups) != 0 || r.pending || len(r.labels) != 0 {
		t.Fatalf("nst00000001 %+v", r)
	}
	if r := read("jb000000001"); r.env != boxes || r.instance || !slices.Equal(r.groups, inDefault) || r.pending ||
		r.labels["ferun.environment"] != boxes || r.labels["ferun.job"] != "jb000000001" {
		t.Fatalf("jb000000001 %+v", r)
	}
	// beta had an environment sandboxes: its sandbox is there.
	if r := read("betabx00001"); r.env != own || !r.pending {
		t.Fatalf("betabx00001 %+v", r)
	}
	var jobEnv string
	var jobGroups []string
	if err := d.Pool.QueryRow(ctx, "SELECT environment_id::text, security_groups FROM jobs WHERE id = 'jb000000001'").
		Scan(&jobEnv, &jobGroups); err != nil || jobEnv != boxes || !slices.Equal(jobGroups, inDefault) {
		t.Fatalf("the job %s %v %v", jobEnv, jobGroups, err)
	}
	// A tenant with neither sandboxes nor jobs gets no environment.
	var idle int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM environments WHERE owner = 'idle'").Scan(&idle); err != nil ||
		idle != 0 {
		t.Fatalf("idle's environments %d %v", idle, err)
	}

	// From now on every row names its environment, an instance no groups.
	if _, err := d.Pool.Exec(ctx, `INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib)
		VALUES ('nwhere00001', 'acme', 'img', 1, 512)`); err == nil {
		t.Fatal("a sandbox without an environment was inserted")
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib, environment_id, instance)
		VALUES ('nst00000002', 'acme', 'img', 1, 512, '`+dev+`', true)`); err == nil {
		t.Fatal("an instance with groups was inserted")
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib, environment_id, security_groups)
		VALUES ('ngrps000001', 'acme', 'img', 1, 512, '`+dev+`', '{}')`); err == nil {
		t.Fatal("a sandbox without groups was inserted")
	}
	// An environment's delete takes its ended jobs' records.
	exec("DELETE FROM sandboxes WHERE id = 'jb000000001'")
	exec("UPDATE jobs SET phase = 'succeeded', finished_at = now() WHERE id = 'jb000000001'")
	exec("DELETE FROM sandboxes WHERE environment_id = '" + boxes + "'")
	exec("DELETE FROM environments WHERE id = '" + boxes + "'")
	var jobs int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("jobs %d %v", jobs, err)
	}
}
