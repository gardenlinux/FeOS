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

// 0040 (ADR 0055): a secret is a value or an HTTP API, one name space of
// both per environment. While a name of an environment is both a value
// secret that is not deleted and a credential the migration refuses,
// naming them; a deleted value and a system secret do not count. Once it
// has run, the insert of either kind of a name the other holds fails with
// the constraint secret_kind, which ferund answers as 409.
func TestSecretKindsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 40)

	exec := func(q string) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const env = "0193a000-aaaa-7bbb-8ccc-000000000040"
	exec("INSERT INTO tenants (id) VALUES ('acme')")
	exec(`INSERT INTO environments (id, owner, name) VALUES ('` + env + `', 'acme', 'prod')`)
	cred := func(name string) string {
		return `INSERT INTO env_credentials (env_id, name, version, upstream, mode, credential_kind, sealed)
			VALUES ('` + env + `', '` + name + `', 1, 'https://api.example.com', 'name', 'headers', '\x01')`
	}
	value := func(name string, system bool, deleted bool) string {
		del := "NULL"
		if deleted {
			del = "now()"
		}
		sys := "false"
		if system {
			sys = "true"
		}
		return `INSERT INTO env_secrets (env_id, name, version, sealed, system, deleted_at)
			VALUES ('` + env + `', '` + name + `', 1, '\x01', ` + sys + `, ` + del + `)`
	}
	exec(cred("s4"))
	exec(cred("gone"))
	exec(value("s4", false, false))
	exec(value("gone", false, true))

	err := d.Migrate(ctx, nil)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "P0001" {
		t.Fatalf("Migrate with a name of both kinds = %v, want the migration's refusal", err)
	}
	for _, want := range []string{"migration 0040_secret_kinds.sql", "1 names are both a secret and a credential",
		"s4 in environment " + env, "DELETE /api/v1/environments/{env}/secrets/{name}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "gone in environment") {
		t.Errorf("the refusal names a deleted value: %v", err)
	}

	exec("DELETE FROM env_secrets WHERE name = 's4'")
	if err := d.Migrate(ctx, nil); err != nil {
		t.Fatalf("Migrate with one kind per name: %v", err)
	}

	refused := func(q, says string) {
		t.Helper()
		_, err := d.Pool.Exec(ctx, q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.ConstraintName != "secret_kind" || !strings.Contains(pe.Message, says) {
			t.Fatalf("%s: %v, want the trigger's refusal saying %q", q, err, says)
		}
	}
	refused(value("s4", false, false), "secret s4 is an HTTP secret")
	exec(value("token", false, false))
	refused(cred("token"), "secret token is a value")
	// A deleted value's name and a system secret's are free.
	exec(cred("gone2"))
	exec(value("gone2", false, true))
	exec(value("addon.db.password", true, false))
	exec("UPDATE env_secrets SET deleted_at = now() WHERE name = 'token'")
	exec(cred("token"))
}
