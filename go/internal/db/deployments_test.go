// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// 0024 renames env_releases env_deployments and gives environments,
// env_instances, env_job_runs and env_incidents a column deployment beside
// release, and keeps working the statements of the previous version, which
// a rolling deploy runs beside this one: they read and write env_releases,
// now a view, and the release columns, which triggers keep equal to the
// deployment columns. The statements below are the previous version's
// store's, as it sends them.
func TestDeploymentsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 24)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const env = "00000000-0000-4000-8000-000000000001"
	exec("INSERT INTO tenants (id) VALUES ('acme')")
	exec(`INSERT INTO environments (id, owner, name, release, deployed) VALUES ($1, 'acme', 'stage', 2, 1)`, env)
	exec(`INSERT INTO env_releases (env_id, number, spec, base, origin, state) VALUES
		($1, 1, '{"jobs":{"m":{"on":"release"}}}', 0, 'apply', 'deployed'), ($1, 2, '{}', 1, 'apply', 'deploying')`, env)
	exec(`INSERT INTO env_tags (env_id, tag, number) VALUES ($1, 'v1', 1)`, env)
	exec(`INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib) VALUES
		('abcdefghjk0', 'acme', 'img', 1, 512), ('abcdefghjk1', 'acme', 'img', 1, 512), ('abcdefghjk2', 'acme', 'img', 1, 512)`)
	exec(`INSERT INTO env_instances (sandbox_id, env_id, kind, object, template_hash, release)
		VALUES ('abcdefghjk0', $1, 'service', 'web', 'h', 2)`, env)
	exec(`INSERT INTO env_job_runs (env_id, job, template_hash, release) VALUES ($1, 'm', 'h', 1)`, env)
	exec(`INSERT INTO env_incidents (env_id, object, release, condition) VALUES ($1, 'service/web', 1, 'crash_loop')`, env)
	migrateRange(t, d, 24, 25)

	// both reads a row's release and deployment; they must be equal.
	both := func(what, q string, want int) {
		t.Helper()
		var rel, dep int
		if err := d.Pool.QueryRow(ctx, q).Scan(&rel, &dep); err != nil {
			t.Fatalf("%s: %s: %v", what, q, err)
		}
		if rel != want || dep != want {
			t.Fatalf("%s: release %d, deployment %d, want both %d", what, rel, dep, want)
		}
	}
	envRow := "SELECT release, deployment FROM environments WHERE name = 'stage'"
	both("the migration", envRow, 2)
	both("the migration", "SELECT release, deployment FROM env_instances WHERE sandbox_id = 'abcdefghjk0'", 2)
	both("the migration", "SELECT release, deployment FROM env_job_runs WHERE job = 'm'", 1)
	both("the migration", "SELECT release, deployment FROM env_incidents WHERE condition = 'crash_loop'", 1)
	var tagged int
	if err := d.Pool.QueryRow(ctx, "SELECT number FROM env_tags JOIN env_deployments USING (env_id, number)").
		Scan(&tagged); err != nil || tagged != 1 {
		t.Fatalf("a tag of the renamed table: %d %v", tagged, err)
	}

	// The previous version on env_releases, the view: its insert, with its
	// RETURNING and the table's defaults, read, lock, list, update and
	// replace, and the environments' list's join.
	const cols = `env_id, number, base, author, message, annotations, origin, origin_ref, state, reason,
	started_at, finished_at, created_at, updated_at, spec`
	var number int
	var state, author string
	if err := d.Pool.QueryRow(ctx, `INSERT INTO env_releases (env_id, number, spec, base, author, message,
			annotations, origin, origin_ref, state, reason, started_at, finished_at)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13) RETURNING number, state, author`,
		env, 3, `{}`, 2, "old", "", `{}`, "apply", "", "waiting", "", nil, nil).Scan(&number, &state, &author); err != nil ||
		number != 3 || state != "waiting" || author != "old" {
		t.Fatalf("the previous version's insert: %d %s %s %v", number, state, author, err)
	}
	var createdSet bool
	if err := d.Pool.QueryRow(ctx, `INSERT INTO env_releases (env_id, number, spec, base, origin)
		VALUES ($1, 4, '{}', 3, 'apply') RETURNING state, created_at IS NOT NULL`, env).Scan(&state, &createdSet); err != nil ||
		state != "waiting" || !createdSet {
		t.Fatalf("an insert through the view without the defaulted columns: %q %v %v", state, createdSet, err)
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, lock := range []string{"FOR UPDATE", "FOR NO KEY UPDATE", "FOR SHARE"} {
		if _, err := tx.Exec(ctx, "SELECT "+cols+` FROM env_releases WHERE env_id = $1 AND number = $2 `+lock, env, 2); err != nil {
			t.Fatalf("the previous version's lock %s: %v", lock, err)
		}
	}
	if err := tx.QueryRow(ctx, `UPDATE env_releases SET state = $3, updated_at = now() WHERE env_id = $1 AND number = $2
		RETURNING state`, env, 2, "deployed").Scan(&state); err != nil || state != "deployed" {
		t.Fatalf("the previous version's update: %s %v", state, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Pool.Query(ctx, `UPDATE env_releases SET state = 'replaced', finished_at = coalesce(finished_at, now()),
			updated_at = now()
		WHERE env_id = $1 AND number < $2 AND state = ANY($3) RETURNING number`, env, 2, []string{"deployed"})
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var listed int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM (SELECT "+cols+` FROM env_releases
		WHERE env_id = $1 ORDER BY number DESC LIMIT $2) l`, env, 10).Scan(&listed); err != nil || listed != 4 {
		t.Fatalf("the previous version's list: %d %v", listed, err)
	}
	if err := d.Pool.QueryRow(ctx, "SELECT state FROM env_deployments WHERE env_id = $1 AND number = 1", env).
		Scan(&state); err != nil || state != "replaced" {
		t.Fatalf("a replace through the view: %q %v", state, err)
	}
	if err := d.Pool.QueryRow(ctx, `SELECT coalesce(r.state, '') FROM environments e
		LEFT JOIN env_releases r ON r.env_id = e.id AND r.number = e.release WHERE e.name = 'stage'`).Scan(&state); err != nil ||
		state != "deployed" {
		t.Fatalf("the previous version's join: %q %v", state, err)
	}

	// The previous version writes release, this one deployment: each
	// column reads the other's write.
	exec("UPDATE environments SET release = 3 WHERE name = 'stage'")
	both("the previous version's write", envRow, 3)
	exec("UPDATE environments SET deployment = 4 WHERE name = 'stage'")
	both("this version's write", envRow, 4)
	exec("UPDATE environments SET gen = gen + 1 WHERE name = 'stage'")
	both("a write of neither", envRow, 4)
	exec(`INSERT INTO environments (id, owner, name) VALUES ('00000000-0000-4000-8000-000000000002', 'acme', 'new')`)
	both("an insert of neither", "SELECT release, deployment FROM environments WHERE name = 'new'", 0)

	// Instances, job runs and incidents: inserts of either version, with
	// the previous version's RETURNING, and updates.
	var rel int
	if err := d.Pool.QueryRow(ctx, `INSERT INTO env_instances (sandbox_id, env_id, kind, object, replica,
			template_hash, release, phase, ready_since, retire_at, restarts, last_exit, probe_failures, next_restart_at,
			exits, provisioned_hash, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::jsonb, $16, $17) RETURNING release`,
		"abcdefghjk1", env, "service", "web", 1, "h", 3, "starting", nil, nil, 0, nil, 0, nil, "[]", "", "").Scan(&rel); err != nil ||
		rel != 3 {
		t.Fatalf("the previous version's instance insert: %d %v", rel, err)
	}
	both("the previous version's instance", "SELECT release, deployment FROM env_instances WHERE sandbox_id = 'abcdefghjk1'", 3)
	exec(`INSERT INTO env_instances (sandbox_id, env_id, kind, object, template_hash, deployment)
		VALUES ('abcdefghjk2', $1, 'service', 'web', 'h', 4)`, env)
	both("this version's instance", "SELECT release, deployment FROM env_instances WHERE sandbox_id = 'abcdefghjk2'", 4)
	exec("UPDATE env_instances SET release = 4 WHERE sandbox_id = 'abcdefghjk1'")
	both("the previous version's instance update", "SELECT release, deployment FROM env_instances WHERE sandbox_id = 'abcdefghjk1'", 4)
	exec("UPDATE env_instances SET deployment = 5, phase = 'ready' WHERE sandbox_id = 'abcdefghjk2'")
	both("this version's instance update", "SELECT release, deployment FROM env_instances WHERE sandbox_id = 'abcdefghjk2'", 5)

	if err := d.Pool.QueryRow(ctx, `INSERT INTO env_job_runs (env_id, job, template_hash, release, attempt,
			sandbox_id, phase, exit_code, detail, log_tail, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11) RETURNING release`,
		env, "n", "h", 3, 1, nil, "running", nil, "", "[]", nil).Scan(&rel); err != nil || rel != 3 {
		t.Fatalf("the previous version's run insert: %d %v", rel, err)
	}
	both("the previous version's run", "SELECT release, deployment FROM env_job_runs WHERE job = 'n'", 3)
	exec(`INSERT INTO env_job_runs (env_id, job, template_hash, deployment) VALUES ($1, 'o', 'h', 4)`, env)
	both("this version's run", "SELECT release, deployment FROM env_job_runs WHERE job = 'o'", 4)

	// The previous version's ON CONFLICT names release (env_incidents_open),
	// this version's deployment: each finds the incident the other opened.
	openOld := func(release int) bool {
		t.Helper()
		rows, err := d.Pool.Query(ctx, `INSERT INTO env_incidents (env_id, object, release, condition, summary, evidence)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb)
			ON CONFLICT (env_id, object, release, condition) WHERE closed_at IS NULL DO NOTHING RETURNING id`,
			env, "service/api", release, "crash_loop", "", "{}")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		return rows.Next()
	}
	openNew := func(deployment int) bool {
		t.Helper()
		rows, err := d.Pool.Query(ctx, `INSERT INTO env_incidents (env_id, object, deployment, condition, summary, evidence)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb)
			ON CONFLICT (env_id, object, deployment, condition) WHERE closed_at IS NULL DO NOTHING RETURNING id`,
			env, "service/api", deployment, "crash_loop", "", "{}")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		return rows.Next()
	}
	if !openOld(3) || openNew(3) || openOld(3) {
		t.Fatal("an incident opened by the previous version")
	}
	if !openNew(4) || openOld(4) || openNew(4) {
		t.Fatal("an incident opened by this version")
	}
	both("the previous version's incident", "SELECT release, deployment FROM env_incidents WHERE object = 'service/api' AND release = 3", 3)
	both("this version's incident", "SELECT release, deployment FROM env_incidents WHERE object = 'service/api' AND deployment = 4", 4)

	// The view goes with the next version; until then it shows every row.
	var n int
	if err := d.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM env_releases) - (SELECT count(*) FROM env_deployments)").
		Scan(&n); err != nil || n != 0 {
		t.Fatalf("the view and the table differ by %d rows: %v", n, err)
	}
}

// 0025 rewrites the stored specs' deploy jobs to their new word and drops
// what 0024 kept for the version before it: the view env_releases, the
// release columns, their triggers, the function and env_incidents_open
// on release, whose name the index on deployment takes. The version of
// 0024, which a rolling deploy runs beside this one, keeps working: its
// statements name the table and the deployment columns only.
func TestDeploymentsOnlyMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 25)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const env = "00000000-0000-4000-8000-000000000001"
	exec("INSERT INTO tenants (id) VALUES ('acme')")
	exec(`INSERT INTO environments (id, owner, name, deployment, deployed) VALUES ($1, 'acme', 'stage', 4, 3)`, env)
	exec(`INSERT INTO env_deployments (env_id, number, spec, base, origin, state, updated_at) VALUES
		($1, 1, '{"services":{"web":{"image":"i"}}}', 0, 'apply', 'replaced', '2026-11-01T00:00:00Z'),
		($1, 2, '{"jobs":{}}', 1, 'apply', 'replaced', '2026-11-01T00:00:00Z'),
		($1, 3, '{"jobs":{"m":{"image":"i","on":"release","retries":1},"s":{"image":"i","on":"manual"}}}', 2,
			'apply', 'deployed', '2026-11-01T00:00:00Z'),
		($1, 4, '{"jobs":{"m":{"image":"i","on":"deploy"},"n":{"image":"i","on":"release"}}}', 3, 'apply', 'deploying',
			'2026-11-01T00:00:00Z')`, env)
	exec(`INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib) VALUES ('abcdefghjk0', 'acme', 'img', 1, 512)`)
	exec(`INSERT INTO env_instances (sandbox_id, env_id, kind, object, template_hash, deployment)
		VALUES ('abcdefghjk0', $1, 'service', 'web', 'h', 3)`, env)
	exec(`INSERT INTO env_incidents (env_id, object, deployment, condition) VALUES ($1, 'service/web', 3, 'crash_loop')`, env)
	migrateRange(t, d, 25, 26)

	specs := map[int]string{}
	rows, err := d.Pool.Query(ctx, `SELECT number, spec::text FROM env_deployments
		WHERE updated_at = '2026-11-01T00:00:00Z' ORDER BY number`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int
		var s string
		if err := rows.Scan(&n, &s); err != nil {
			t.Fatal(err)
		}
		specs[n] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[int]string{
		1: `{"services": {"web": {"image": "i"}}}`,
		2: `{"jobs": {}}`,
		3: `{"jobs": {"m": {"on": "deploy", "image": "i", "retries": 1}, "s": {"on": "manual", "image": "i"}}}`,
		4: `{"jobs": {"m": {"on": "deploy", "image": "i"}, "n": {"on": "deploy", "image": "i"}}}`,
	}
	for n, w := range want {
		if specs[n] != w {
			t.Errorf("deployment %d: spec %s (updated_at changed?), want %s", n, specs[n], w)
		}
	}

	var left int
	if err := d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_class WHERE relname = 'env_releases')
		+ (SELECT count(*) FROM information_schema.columns WHERE column_name = 'release'
			AND table_name IN ('environments', 'env_instances', 'env_job_runs', 'env_incidents'))
		+ (SELECT count(*) FROM pg_trigger WHERE tgname LIKE '%deployment_sync')
		+ (SELECT count(*) FROM pg_proc WHERE proname = 'deployment_sync')
		+ (SELECT count(*) FROM pg_class WHERE relname = 'env_incidents_open_deployment')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the view, a release column, a trigger, the function or the index's old name left: %d %v", left, err)
	}

	// The version of 0024 beside it: its writes of the deployment columns,
	// its ON CONFLICT on deployment, now env_incidents_open.
	exec("UPDATE environments SET deployment = 5 WHERE name = 'stage'")
	exec(`INSERT INTO env_job_runs (env_id, job, template_hash, deployment) VALUES ($1, 'm', 'h', 5)`, env)
	exec("UPDATE env_instances SET deployment = 5 WHERE sandbox_id = 'abcdefghjk0'")
	open := func() bool {
		t.Helper()
		rows, err := d.Pool.Query(ctx, `INSERT INTO env_incidents (env_id, object, deployment, condition, summary, evidence)
			VALUES ($1, 'service/web', 3, 'crash_loop', '', '{}')
			ON CONFLICT (env_id, object, deployment, condition) WHERE closed_at IS NULL DO NOTHING RETURNING id`, env)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		return rows.Next()
	}
	if open() {
		t.Fatal("an incident opened twice")
	}
	var idx string
	if err := d.Pool.QueryRow(ctx, "SELECT pg_get_indexdef('env_incidents_open'::regclass)").Scan(&idx); err != nil ||
		!strings.Contains(idx, "(env_id, object, deployment, condition) WHERE (closed_at IS NULL)") {
		t.Fatalf("env_incidents_open: %s %v", idx, err)
	}
}
