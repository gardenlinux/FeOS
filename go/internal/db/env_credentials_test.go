// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// 0035 (ADR 0033, corrected) runs on a database that has 0036 and later
// already, as every one of a ferund before it has: Migrate applies what
// it lacks. While the projects' credentials hold a row it refuses, naming
// the table, the projects and what to do, and leaves nothing behind; once
// the table is empty it makes env_credentials and keeps the old table for
// the release before, whose writes there fail from then on. The SQL and
// the refusal speak the schema of 0040 (KEEP-TENANT: tenant_credentials).
func TestEnvCredentialsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 35)
	migrateRange(t, d, 36, 40)
	// 0041 drops the old table; recorded as applied with those after it
	// (which speak its schema), Migrate below applies 0035 alone, as the
	// release of 0035 did.
	if _, err := d.Pool.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES (41), (42)"); err != nil {
		t.Fatal(err)
	}

	exec := func(q string) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	applied := func() (bool, bool) {
		t.Helper()
		var recorded, table bool
		err := d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 35),
			to_regclass('env_credentials') IS NOT NULL`).Scan(&recorded, &table)
		if err != nil {
			t.Fatal(err)
		}
		return recorded, table
	}
	exec("INSERT INTO tenants (id) VALUES ('acme'), ('beta')")
	exec(`INSERT INTO tenant_credentials (tenant, name, version, upstream, mode, credential_kind, sealed) VALUES
		('acme', 'github', 1, 'https://api.github.com', 'name', 'headers', '\x01'),
		('acme', 'github', 2, 'https://api.github.com', 'name', 'headers', '\x01'),
		('beta', 'erp', 1, 'https://erp.example.com', 'intercept', 'basic', '\x01')`)

	err := d.Migrate(ctx, nil)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "P0001" {
		t.Fatalf("Migrate with the projects' credentials = %v, want the migration's refusal", err)
	}
	for _, want := range []string{"migration 0035_env_credentials.sql", "tenant_credentials holds 3 rows (tenants: acme, beta)", // KEEP-TENANT: 0035's refusal
		"DELETE /api/v1/credentials/{name}", "PUT /api/v1/environments/{env}/credentials/{name}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if recorded, table := applied(); recorded || table {
		t.Fatalf("after the refusal: 0035 recorded %v, env_credentials made %v; want neither", recorded, table)
	}

	exec("DELETE FROM tenant_credentials")
	if err := d.Migrate(ctx, nil); err != nil {
		t.Fatalf("Migrate without the projects' credentials: %v", err)
	}
	if recorded, table := applied(); !recorded || !table {
		t.Fatalf("0035 recorded %v, env_credentials made %v; want both", recorded, table)
	}
	var kept bool
	if err := d.Pool.QueryRow(ctx, "SELECT to_regclass('tenant_credentials') IS NOT NULL").Scan(&kept); err != nil || !kept {
		t.Fatalf("old table kept: %v, %v; want it kept for the release before", kept, err)
	}

	// The release before writes no row there any more: an insert and an
	// update fail, naming why; a delete works.
	refusedWrite := func(q string) {
		t.Helper()
		_, err := d.Pool.Exec(ctx, q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "P0001" ||
			!strings.Contains(pe.Message, "written by a ferund older than migration 0035: credentials are an environment's now") {
			t.Fatalf("%s: %v, want the trigger's refusal", q, err)
		}
	}
	insert := `INSERT INTO tenant_credentials (tenant, name, version, upstream, mode, credential_kind, sealed)
		VALUES ('acme', 'github', 1, 'https://api.github.com', 'name', 'headers', '\x01')`
	refusedWrite(insert)
	exec("ALTER TABLE tenant_credentials DISABLE TRIGGER tenant_credentials_refuse")
	exec(insert)
	exec("ALTER TABLE tenant_credentials ENABLE TRIGGER tenant_credentials_refuse")
	refusedWrite("UPDATE tenant_credentials SET upstream = 'https://evil.example'")
	exec("DELETE FROM tenant_credentials")
}
