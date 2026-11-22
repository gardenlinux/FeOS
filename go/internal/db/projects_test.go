// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// 0041 (ADR 0061) renames what said the old word: after it no table, partition,
// index, sequence, column, constraint, trigger or function of the public
// schema has the word in its name. The provider kind, a sandbox's log
// label key, the activity kinds, their detail key and the names of the
// three renamed kinds are rewritten, the lease of the renamed loop goes, and the projects' credentials table of
// 0033 goes, refusing while it holds a row. The SQL before 0041 speaks the
// schema of 0040.
func TestProjectsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 41)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const env = "0193a000-aaaa-7bbb-8ccc-000000000041"
	// The partitions of activity and traffic_records are made before, so
	// that the rename's recursion into them is checked too.
	now := time.Now().UTC()
	if err := d.EnsurePartitions(ctx, now.AddDate(0, 0, -1), now.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO tenants (id) VALUES ('acme')")
	exec("INSERT INTO tenant_tokens (hash, tenant_id, label) VALUES ('h1', 'acme', 'release')")
	exec(`INSERT INTO environments (id, owner, name) VALUES ('` + env + `', 'acme', 'prod')`)
	exec(`INSERT INTO access_providers (tenant, name, kind, url) VALUES ('acme', 'fecode', 'tenant', 'https://fecode.example')`)
	exec(`INSERT INTO visits (id, env_id, tenant, provider, provider_kind, host, subject, ends_at)
		VALUES (gen_random_uuid(), '` + env + `', 'acme', 'fecode', 'tenant', 'prod--acme.apps.example', 'u-1', now() + interval '1 hour')`)
	exec(`INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib, environment_id, tenant_job, log_labels) VALUES
		('0199f0a2-0000-4000-8000-000000000041', 'acme', 'img', 1, 512, '` + env + `', true,
		 '{"ferun.tenant": "acme", "ferun.environment": "` + env + `"}')`)
	exec(`INSERT INTO activity (ts_ns, source, kind, tenant_id, name, detail) VALUES
		(1, 'ferund', 'tenant_created', 'acme', 'tenant acme created by ada', '{"tenant": "acme", "by": "ada"}'),
		(2, 'ferund', 'tenant_zone_set', 'acme', 'tenant acme moved to zone z by ada', '{"tenant": "acme", "zone": "z"}'),
		(3, 'ferund', 'tenant_limits', 'acme', 'limits of tenant acme set by ada', '{"tenant": "acme"}'),
		(4, 'ferund', 'sandbox_created', 'acme', 'sandbox s of tenant acme', '{"sandbox": "s"}')`)
	exec(`INSERT INTO leases (name, holder, epoch, acquired_at, renewed_at, expires_at) VALUES
		('sandbox-tenant-deletion', 'i1', 1, now(), now(), now()), ('sandbox-reaper', 'i1', 1, now(), now(), now())`)

	// A row in the old credentials table (its trigger refuses a write since 0035)
	// makes 0041 refuse, leaving nothing behind.
	exec("ALTER TABLE tenant_credentials DISABLE TRIGGER tenant_credentials_refuse")
	exec(`INSERT INTO tenant_credentials (tenant, name, version, upstream, mode, credential_kind, sealed)
		VALUES ('acme', 'github', 1, 'https://api.github.com', 'name', 'headers', '\x01')`)
	exec("ALTER TABLE tenant_credentials ENABLE TRIGGER tenant_credentials_refuse")
	err := d.Migrate(ctx, nil)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "P0001" || !strings.Contains(pe.Message, "tenant_credentials holds rows") { // KEEP-TENANT: 0041's refusal
		t.Fatalf("Migrate with a row in the old credentials table = %v, want the migration's refusal", err)
	}
	var recorded, renamed bool
	if err := d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 41),
		to_regclass('projects') IS NOT NULL`).Scan(&recorded, &renamed); err != nil {
		t.Fatal(err)
	}
	if recorded || renamed {
		t.Fatalf("after the refusal: 0041 recorded %v, projects made %v; want neither", recorded, renamed)
	}
	exec("DELETE FROM tenant_credentials")

	if err := d.Migrate(ctx, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// No name of the public schema says the old word.
	rows, err := d.Pool.Query(ctx, `
		SELECT 'column ' || table_name || '.' || column_name FROM information_schema.columns
			WHERE table_schema = 'public' AND (column_name ~* 'tenant' OR table_name ~* 'tenant') -- KEEP-TENANT: the word looked for
		UNION ALL SELECT 'relation ' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relname ~* 'tenant' -- KEEP-TENANT
		UNION ALL SELECT 'constraint ' || c.conname FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace
			WHERE n.nspname = 'public' AND c.conname ~* 'tenant' -- KEEP-TENANT
		UNION ALL SELECT 'trigger ' || t.tgname FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND t.tgname ~* 'tenant' -- KEEP-TENANT
		UNION ALL SELECT 'function ' || p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public' AND p.proname ~* 'tenant' -- KEEP-TENANT
		`)
	if err != nil {
		t.Fatal(err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("names that still say tenant: %v", left) // KEEP-TENANT: the word the test looks for
	}
	// The partitions are there, and renamed with their parents.
	var partitionCols int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns c
		JOIN pg_class r ON r.relname = c.table_name AND r.relispartition
		WHERE c.table_schema = 'public' AND c.column_name = 'project_id'`).Scan(&partitionCols); err != nil {
		t.Fatal(err)
	}
	if partitionCols < 2 {
		t.Errorf("%d partitions with project_id, want those of activity and traffic_records", partitionCols)
	}

	// The rows came along, and the stored words are rewritten.
	var token, provider, visit string
	if err := d.Pool.QueryRow(ctx, "SELECT project_id FROM project_tokens WHERE hash = 'h1'").Scan(&token); err != nil || token != "acme" {
		t.Errorf("project token: %q, %v", token, err)
	}
	if err := d.Pool.QueryRow(ctx, "SELECT kind FROM access_providers WHERE project = 'acme'").Scan(&provider); err != nil || provider != "project" {
		t.Errorf("provider kind: %q, %v; want project", provider, err)
	}
	if err := d.Pool.QueryRow(ctx, "SELECT provider_kind FROM visits WHERE project = 'acme'").Scan(&visit); err != nil || visit != "project" {
		t.Errorf("visit provider kind: %q, %v; want project", visit, err)
	}
	var job bool
	var labels []byte
	if err := d.Pool.QueryRow(ctx, "SELECT project_job, log_labels FROM sandboxes").Scan(&job, &labels); err != nil {
		t.Fatal(err)
	}
	var lm map[string]string
	if err := json.Unmarshal(labels, &lm); err != nil {
		t.Fatal(err)
	}
	if _, old := lm["ferun.tenant"]; !job || old || // KEEP-TENANT: the key 0041 rewrites
		lm["ferun.project"] != "acme" || lm["ferun.environment"] != env {
		t.Errorf("sandbox: project_job %v, log labels %v; want ferun.project acme beside ferun.environment", job, lm)
	}
	rows, err = d.Pool.Query(ctx, "SELECT kind || ' ' || detail::text || ' ' || name FROM activity WHERE project_id = 'acme' ORDER BY ts_ns")
	if err != nil {
		t.Fatal(err)
	}
	acts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`project_created {"by": "ada", "project": "acme"} project acme created by ada`,
		`project_zone_set {"zone": "z", "project": "acme"} project acme moved to zone z by ada`,
		`project_limits {"project": "acme"} limits of project acme set by ada`,
		`sandbox_created {"sandbox": "s"} sandbox s of tenant acme`, // KEEP-TENANT: only the three kinds' names are rewritten
	}
	if strings.Join(acts, "\n") != strings.Join(want, "\n") {
		t.Errorf("activity:\n%s\nwant:\n%s", strings.Join(acts, "\n"), strings.Join(want, "\n"))
	}
	rows, err = d.Pool.Query(ctx, "SELECT name FROM leases ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	leases, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(leases, ",") != "sandbox-reaper" {
		t.Errorf("leases %v, want sandbox-reaper alone", leases)
	}

	// The rebuilt checks hold the new word and refuse the old.
	_, err = d.Pool.Exec(ctx, `INSERT INTO access_providers (project, name, kind, url) VALUES ('acme', 'old', 'tenant', 'https://x.example')`) // KEEP-TENANT: the kind of before
	if !errors.As(err, &pe) || pe.Code != "23514" {
		t.Errorf("provider of the old kind: %v, want a check violation", err)
	}
	_, err = d.Pool.Exec(ctx, `INSERT INTO access_providers (project, name, kind) VALUES ('acme', 'nourl', 'project')`)
	if !errors.As(err, &pe) || pe.Code != "23514" || pe.ConstraintName != "access_providers_project_kind" {
		t.Errorf("provider of kind project without a URL: %v, want access_providers_project_kind", err)
	}
}
