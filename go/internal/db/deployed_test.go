// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// 0021 gives environments deployed, the realised release under its new
// word, and keeps realised equal to it for the previous version, which a
// rolling deploy runs beside this one: whichever of the two columns a
// version writes, both read the same.
func TestDeployedMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 21)

	for _, q := range []string{
		"INSERT INTO tenants (id) VALUES ('acme')",
		`INSERT INTO environments (id, owner, name, release, realised) VALUES
			('00000000-0000-4000-8000-000000000001', 'acme', 'stage', 7, 6),
			('00000000-0000-4000-8000-000000000002', 'acme', 'preview', 0, 0)`,
	} {
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	migrateRange(t, d, 21, 22)

	both := func(name string) (deployed, realised int) {
		t.Helper()
		if err := d.Pool.QueryRow(ctx, "SELECT deployed, realised FROM environments WHERE name = $1", name).
			Scan(&deployed, &realised); err != nil {
			t.Fatal(err)
		}
		return deployed, realised
	}
	exec := func(q string) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	check := func(what, name string, want int) {
		t.Helper()
		if deployed, realised := both(name); deployed != want || realised != want {
			t.Fatalf("%s: deployed %d, realised %d, want both %d", what, deployed, realised, want)
		}
	}

	check("the migration", "stage", 6)
	check("the migration", "preview", 0)
	// This version writes deployed.
	exec("UPDATE environments SET deployed = 7 WHERE name = 'stage'")
	check("a write of deployed", "stage", 7)
	// The previous version writes realised.
	exec("UPDATE environments SET realised = 3 WHERE name = 'preview'")
	check("a write of realised", "preview", 3)
	// A write of neither keeps both.
	exec("UPDATE environments SET gen = gen + 1 WHERE name = 'stage'")
	check("a write of neither", "stage", 7)
	// Inserts of either version.
	exec(`INSERT INTO environments (id, owner, name) VALUES ('00000000-0000-4000-8000-000000000003', 'acme', 'demo')`)
	check("an insert", "demo", 0)
	exec(`INSERT INTO environments (id, owner, name, realised) VALUES
		('00000000-0000-4000-8000-000000000004', 'acme', 'old', 2)`)
	check("an insert of realised", "old", 2)
	exec(`INSERT INTO environments (id, owner, name, deployed) VALUES
		('00000000-0000-4000-8000-000000000005', 'acme', 'new', 4)`)
	check("an insert of deployed", "new", 4)
}
