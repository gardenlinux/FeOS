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
// it lacks. While tenant_credentials holds a row it refuses, naming the
// table, the tenants and what to do, and leaves nothing behind; once the
// table is empty it makes env_credentials and keeps tenant_credentials
// for the release before.
func TestEnvCredentialsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 35)
	migrateRange(t, d, 36, 40)

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
		t.Fatalf("Migrate with tenant credentials = %v, want the migration's refusal", err)
	}
	for _, want := range []string{"migration 0035_env_credentials.sql", "tenant_credentials holds 3 rows (tenants: acme, beta)",
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
		t.Fatalf("Migrate without tenant credentials: %v", err)
	}
	if recorded, table := applied(); !recorded || !table {
		t.Fatalf("0035 recorded %v, env_credentials made %v; want both", recorded, table)
	}
	var kept bool
	if err := d.Pool.QueryRow(ctx, "SELECT to_regclass('tenant_credentials') IS NOT NULL").Scan(&kept); err != nil || !kept {
		t.Fatalf("tenant_credentials kept: %v, %v; want it kept for the release before", kept, err)
	}
}
