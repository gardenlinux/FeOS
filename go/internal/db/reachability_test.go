// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// 0033 (ADR 0034 and 0035, revised) strips securityGroups from every
// deployment's spec, at the top and on every service, job and addon;
// turns a sandbox's and a job's groups into reaches (workspace: all);
// adds every pod template's workload group beside what it has; and drops
// the groups' table and columns. Run twice, it changes nothing more.
func TestReachabilityMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 33)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const (
		stage = "00000000-0000-4000-8000-000000000001"
		dev   = "00000000-0000-4000-8000-000000000002"
		boxes = "00000000-0000-4000-8000-000000000003"
		sg    = "11111111-0000-4000-8000-000000000001" // a group's ID on the nodes
	)
	exec("INSERT INTO tenants (id) VALUES ('acme')")
	exec(`INSERT INTO environments (id, owner, name, deployment, deployed) VALUES
		('` + stage + `', 'acme', 'stage', 3, 2), ('` + dev + `', 'acme', 'dev', 2, 2),
		('` + boxes + `', 'acme', 'sandboxes', 0, 0)`)
	exec(`INSERT INTO security_groups (env_id, name) VALUES ('` + stage + `', 'default'), ('` + stage + `', 'workspace'),
		('` + dev + `', 'default'), ('` + boxes + `', 'default')`)

	// A spec as the first version stored it, migrated or normalised.
	grouped := func(image string) string {
		return `{"securityGroups": {"default": {"in": [{"protocol": "any", "from": {"group": "default"}}],
				"out": [{"protocol": "any", "to": "internet"}]},
			"workspace": {"in": [], "out": [{"protocol": "tcp", "ports": [443], "to": {"group": "default"}}]}},
		"services": {
			"web": {"image": "` + image + `", "replicas": 2, "ports": [{"name": "http", "port": 8080}],
				"env": {"DB": "${addons.db.url}"}, "securityGroups": ["default"]},
			"worker": {"image": "` + image + `", "securityGroups": ["default", "workspace"]},
			"vite": {"runtime": "attached", "sandbox": "wsa00000001", "command": ["vite"]}},
		"jobs": {"migrate": {"image": "` + image + `", "securityGroups": ["default"]}},
		"addons": {"db": {"type": "postgres", "version": "17", "plan": "small", "securityGroups": ["default"]}},
		"routes": {"root": {"path": "/", "service": "web", "port": "http"}},
		"variables": {"securityGroups": "a variable's name, not the field"}}`
	}
	// The same without the field, as 0033 must leave it.
	stripped := func(image string) string {
		var m map[string]any
		if err := json.Unmarshal([]byte(grouped(image)), &m); err != nil {
			t.Fatal(err)
		}
		delete(m, "securityGroups")
		for _, k := range []string{"services", "jobs", "addons"} {
			for _, v := range m[k].(map[string]any) {
				delete(v.(map[string]any), "securityGroups")
			}
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// Shapes the plan never stored: a member that is not an object, and
	// jobs as a list, are left as they are; the top-level key goes.
	const odd = `{"securityGroups": {}, "services": {"x": "not an object", "y": {"securityGroups": []}},
		"jobs": [{"securityGroups": ["default"]}], "addons": null}`
	const oddAfter = `{"services": {"x": "not an object", "y": {}},
		"jobs": [{"securityGroups": ["default"]}], "addons": null}`
	const plain = `{"services": {"vite": {"runtime": "attached", "sandbox": "wsa00000001", "command": ["vite"]}}}`
	exec(`INSERT INTO env_deployments (env_id, number, spec, base, origin, state) VALUES
		($1, 1, $3, 0, 'apply', 'replaced'),
		($1, 2, $4, 1, 'apply', 'deployed'),
		($1, 3, $5, 2, 'apply', 'deploying'),
		($2, 1, $6, 0, 'apply', 'replaced'),
		($2, 2, $7, 1, 'apply', 'deployed'),
		($2, 3, '{}', 2, 'apply', 'failed')`,
		stage, dev, grouped("x@sha256:1"), grouped("x@sha256:2"), grouped("x@sha256:3"), odd, plain)

	// The pods: an instance of stage's web, one without its row in
	// env_instances, one without a template; sandboxes in workspace and
	// default, in default only (without a pod, and with a template that
	// has no groups); a tenant's job's pod in workspace.
	tmpl := func(env string, more ...string) string {
		groups := append([]string{`"env-` + env + `"`, `"sg-` + sg + `"`}, more...)
		return `{"image": "x", "securityGroups": [` + strings.Join(groups, ", ") + `],
			"nameTables": [{"name": "web", "addresses": ["fd00::1"]}], "vcpus": 1}`
	}
	exec(`INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib, environment_id, instance, tenant_job,
			security_groups, groups_pending, pod_template) VALUES
		('nst00000001', 'acme', 'img', 1, 512, $1, true, false, '{}', false, $4),
		('nst00000002', 'acme', 'img', 1, 512, $1, true, false, '{}', false, $4),
		('nst00000003', 'acme', 'img', 1, 512, $1, true, false, '{}', false, NULL),
		('wsa00000001', 'acme', 'img', 1, 512, $2, false, false, '{workspace,default}', false, $5),
		('wsb00000001', 'acme', 'img', 1, 512, $3, false, false, '{default}', true, NULL),
		('wsc00000001', 'acme', 'img', 1, 512, $3, false, false, '{default}', false, '{"image": "x"}'),
		('jb000000001', 'acme', 'img', 1, 512, $3, false, true, '{workspace}', false, $6)`,
		stage, dev, boxes, tmpl(stage, `"ing-web"`), tmpl(dev), tmpl(boxes))
	exec(`UPDATE sandboxes SET move_restore = true WHERE id = 'wsa00000001'`)
	exec(`INSERT INTO env_instances (sandbox_id, env_id, kind, object, template_hash, deployment) VALUES
		('nst00000001', $1, 'service', 'web', 'h', 2), ('nst00000003', $1, 'addon', 'db', 'h', 2)`, stage)
	exec(`INSERT INTO jobs (id, owner, request_hash, image, image_ref, vcpus, memory_mib, phase, deadline_at,
			environment_id, security_groups) VALUES
		('jb000000001', 'acme', 'h', 'img', 'img', 1, 512, 'running', now() + interval '1 hour', $1, '{workspace}'),
		('jb000000002', 'acme', 'h', 'img', 'img', 1, 512, 'queued', now() + interval '1 hour', $1, '{default}')`, boxes)

	migrateRange(t, d, 33, 34)

	// Specs: every deployment, old ones too, without the field and
	// otherwise the same.
	specIs := func(env string, n int, want string) {
		t.Helper()
		var same bool
		var raw string
		if err := d.Pool.QueryRow(ctx, `SELECT spec = $3::jsonb, spec::text FROM env_deployments
			WHERE env_id = $1 AND number = $2`, env, n, want).Scan(&same, &raw); err != nil {
			t.Fatal(err)
		}
		if !same {
			t.Fatalf("deployment %d of %s:\n%s\nwant\n%s", n, env, raw, want)
		}
	}
	specIs(stage, 1, stripped("x@sha256:1"))
	specIs(stage, 2, stripped("x@sha256:2"))
	specIs(stage, 3, stripped("x@sha256:3"))
	specIs(dev, 1, oddAfter)
	specIs(dev, 2, plain)
	specIs(dev, 3, `{}`)
	var left int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM env_deployments
		WHERE spec @? 'strict $.securityGroups' OR spec @? 'strict $.services.*.securityGroups'
			OR spec @? 'strict $.jobs.*.securityGroups' OR spec @? 'strict $.addons.*.securityGroups'`).
		Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d specs with securityGroups left (%v)", left, err)
	}

	// reaches and egress.
	type pod struct {
		reaches  string
		egress   *string
		template *string
	}
	read := func(id string) pod {
		t.Helper()
		var p pod
		if err := d.Pool.QueryRow(ctx, `SELECT reaches::text, egress::text, pod_template::text FROM sandboxes WHERE id = $1`,
			id).Scan(&p.reaches, &p.egress, &p.template); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return p
	}
	groupsOf := func(id string) []string {
		t.Helper()
		p := read(id)
		if p.template == nil {
			return nil
		}
		var m struct {
			SecurityGroups []string `json:"securityGroups"`
		}
		if err := json.Unmarshal([]byte(*p.template), &m); err != nil {
			t.Fatal(err)
		}
		return m.SecurityGroups
	}
	for id, want := range map[string]string{"nst00000001": `[]`, "wsa00000001": `"all"`, "wsb00000001": `[]`,
		"wsc00000001": `[]`, "jb000000001": `[]`} {
		if p := read(id); p.reaches != want || p.egress != nil {
			t.Fatalf("%s reaches %s egress %v, want %s", id, p.reaches, p.egress, want)
		}
	}
	jobReaches := func(id string) string {
		t.Helper()
		var r string
		var egress *string
		if err := d.Pool.QueryRow(ctx, "SELECT reaches::text, egress::text FROM jobs WHERE id = $1", id).
			Scan(&r, &egress); err != nil || egress != nil {
			t.Fatalf("job %s: %v %v", id, egress, err)
		}
		return r
	}
	if r := jobReaches("jb000000001"); r != `"all"` {
		t.Fatalf("jb000000001 reaches %s", r)
	}
	if r := jobReaches("jb000000002"); r != `[]` {
		t.Fatalf("jb000000002 reaches %s", r)
	}

	// Templates: the workload's group beside what they had. An
	// instance's is ferund's: sha256 of "<environment>/<kind>/<object>".
	sum := sha256.Sum256([]byte(stage + "/service/web"))
	web := "wl-" + hex.EncodeToString(sum[:])[:32]
	groupsAre := func(id string, want ...string) {
		t.Helper()
		if got := groupsOf(id); !slices.Equal(got, want) {
			t.Fatalf("%s's groups %v, want %v", id, got, want)
		}
	}
	groupsAre("nst00000001", "env-"+stage, "sg-"+sg, "ing-web", web)
	groupsAre("nst00000002", "env-"+stage, "sg-"+sg, "ing-web") // no env_instances row
	groupsAre("nst00000003")                                    // no template
	groupsAre("wsa00000001", "env-"+dev, "sg-"+sg, "wl-s-wsa00000001")
	groupsAre("wsb00000001")
	groupsAre("wsc00000001", "env-"+boxes, "wl-s-wsc00000001")
	groupsAre("jb000000001", "env-"+boxes, "sg-"+sg, "wl-j-jb000000001")
	var rest bool
	if err := d.Pool.QueryRow(ctx, `SELECT pod_template - 'securityGroups' = $1::jsonb - 'securityGroups'
		FROM sandboxes WHERE id = 'nst00000001'`, tmpl(stage)).Scan(&rest); err != nil || !rest {
		t.Fatalf("the rest of the template changed (%v)", err)
	}
	if p := read("wsb00000001"); p.template != nil {
		t.Fatalf("a sandbox without a pod got a template: %s", *p.template)
	}

	// What the first version added is gone.
	var gone struct {
		table   bool
		columns int
		index   bool
	}
	if err := d.Pool.QueryRow(ctx, `SELECT to_regclass('security_groups') IS NULL,
			(SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
				AND (table_name, column_name) IN (('sandboxes', 'security_groups'), ('sandboxes', 'groups_pending'),
					('sandboxes', 'move_restore'), ('jobs', 'security_groups'))),
			to_regclass('sandboxes_groups_pending') IS NULL`).Scan(&gone.table, &gone.columns, &gone.index); err != nil ||
		!gone.table || gone.columns != 0 || !gone.index {
		t.Fatalf("left: %+v %v", gone, err)
	}

	// reaches is "all" or a list.
	for _, bad := range []string{`"some"`, `{}`, `null`} {
		if _, err := d.Pool.Exec(ctx, "UPDATE sandboxes SET reaches = $1::jsonb WHERE id = 'wsb00000001'", bad); err == nil {
			t.Fatalf("a sandbox took reaches %s", bad)
		}
		if _, err := d.Pool.Exec(ctx, "UPDATE jobs SET reaches = $1::jsonb WHERE id = 'jb000000002'", bad); err == nil {
			t.Fatalf("a job took reaches %s", bad)
		}
	}
	exec(`UPDATE sandboxes SET reaches = '[{"service": "web", "port": "http"}]', egress = '"none"' WHERE id = 'wsb00000001'`)

	// A second run changes nothing.
	snapshot := func() string {
		t.Helper()
		var s string
		if err := d.Pool.QueryRow(ctx, `SELECT
			(SELECT jsonb_agg(jsonb_build_array(env_id, number, spec) ORDER BY env_id, number) FROM env_deployments)::text ||
			(SELECT jsonb_agg(jsonb_build_array(id, reaches, egress, pod_template) ORDER BY id) FROM sandboxes)::text ||
			(SELECT jsonb_agg(jsonb_build_array(id, reaches, egress) ORDER BY id) FROM jobs)::text`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	b, err := os.ReadFile("migrations/0033_reachability.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(b))
	if after := snapshot(); after != before {
		t.Fatalf("the second run changed\n%s\nto\n%s", before, after)
	}
}
