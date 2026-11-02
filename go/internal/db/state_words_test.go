// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// 0022 stores the release states and the stopping phase in their new
// words, rewriting the rows, while the CHECKs still admit the earlier
// words for the version of 0021; environments.realised goes with its
// trigger.
func TestStateWordsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 22)

	exec := func(q string) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("INSERT INTO tenants (id) VALUES ('acme')")
	exec(`INSERT INTO environments (id, owner, name, release, deployed) VALUES
		('00000000-0000-4000-8000-000000000001', 'acme', 'stage', 6, 5)`)
	exec(`INSERT INTO env_releases (env_id, number, spec, base, origin, state, updated_at)
		SELECT '00000000-0000-4000-8000-000000000001', n, '{}', n - 1, 'apply', s, '2026-11-01T00:00:00Z'
		FROM (VALUES (1, 'superseded'), (2, 'failed'), (3, 'superseded'), (4, 'pending'), (5, 'realised'),
			(6, 'rolling')) AS r(n, s)`)
	exec(`INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib) VALUES
		('abcdefghjk0', 'acme', 'img', 1, 512), ('abcdefghjk1', 'acme', 'img', 1, 512)`)
	exec(`INSERT INTO env_instances (sandbox_id, env_id, kind, object, template_hash, release, phase) VALUES
		('abcdefghjk0', '00000000-0000-4000-8000-000000000001', 'service', 'web', 'h', 6, 'retiring'),
		('abcdefghjk1', '00000000-0000-4000-8000-000000000001', 'service', 'web', 'h', 6, 'ready')`)
	migrateRange(t, d, 22, 23)

	rows, err := d.Pool.Query(ctx, `SELECT state FROM env_releases WHERE updated_at = '2026-11-01T00:00:00Z'
		ORDER BY number`)
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		states = append(states, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"replaced", "failed", "replaced", "waiting", "deployed", "deploying"}
	if len(states) != len(want) {
		t.Fatalf("states %v (updated_at changed?), want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("states %v, want %v", states, want)
		}
	}
	var stopping, ready int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE phase = 'stopping'),
		count(*) FILTER (WHERE phase = 'ready') FROM env_instances`).Scan(&stopping, &ready); err != nil ||
		stopping != 1 || ready != 1 {
		t.Fatalf("phases: %d stopping, %d ready, %v", stopping, ready, err)
	}

	// The version of 0021 beside it: its earlier words are admitted, its
	// writes of deployed need no trigger, realised is gone.
	exec(`INSERT INTO env_releases (env_id, number, spec, base, origin, state)
		VALUES ('00000000-0000-4000-8000-000000000001', 7, '{}', 6, 'apply', 'pending')`)
	exec("UPDATE env_releases SET state = 'rolling' WHERE number = 7")
	exec("UPDATE env_instances SET phase = 'retiring' WHERE sandbox_id = 'abcdefghjk1'")
	exec("UPDATE environments SET deployed = 6")
	exec(`INSERT INTO env_releases (env_id, number, spec, base, origin)
		VALUES ('00000000-0000-4000-8000-000000000001', 8, '{}', 7, 'apply')`)
	var def string
	if err := d.Pool.QueryRow(ctx, "SELECT state FROM env_releases WHERE number = 8").Scan(&def); err != nil ||
		def != "waiting" {
		t.Fatalf("default %q %v", def, err)
	}
	if _, err := d.Pool.Exec(ctx, "UPDATE env_releases SET state = 'gone' WHERE number = 8"); err == nil {
		t.Fatal("an unknown state was admitted")
	}
	var left int
	if err := d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'environments' AND column_name = 'realised')
		+ (SELECT count(*) FROM pg_trigger WHERE tgname = 'environments_deployed_sync')
		+ (SELECT count(*) FROM pg_proc WHERE proname = 'environments_deployed_sync')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("realised, its trigger or its function left: %d %v", left, err)
	}
}

// 0023 rewrites what the version of 0021 wrote in the earlier words
// during 0022's rollout, and lets the CHECKs admit the new words only.
func TestStateWordsOnlyMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 23)

	exec := func(q string) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("INSERT INTO tenants (id) VALUES ('acme')")
	exec(`INSERT INTO environments (id, owner, name, release, deployed) VALUES
		('00000000-0000-4000-8000-000000000001', 'acme', 'stage', 5, 4)`)
	exec(`INSERT INTO env_releases (env_id, number, spec, base, origin, state)
		SELECT '00000000-0000-4000-8000-000000000001', n, '{}', n - 1, 'apply', s
		FROM (VALUES (1, 'superseded'), (2, 'replaced'), (3, 'failed'), (4, 'realised'), (5, 'rolling')) AS r(n, s)`)
	exec(`INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib) VALUES
		('abcdefghjk0', 'acme', 'img', 1, 512), ('abcdefghjk1', 'acme', 'img', 1, 512)`)
	exec(`INSERT INTO env_instances (sandbox_id, env_id, kind, object, template_hash, release, phase) VALUES
		('abcdefghjk0', '00000000-0000-4000-8000-000000000001', 'service', 'web', 'h', 5, 'retiring'),
		('abcdefghjk1', '00000000-0000-4000-8000-000000000001', 'service', 'web', 'h', 5, 'stopping')`)
	migrateRange(t, d, 23, 24)

	var states string
	if err := d.Pool.QueryRow(ctx, "SELECT string_agg(state, ',' ORDER BY number) FROM env_releases").
		Scan(&states); err != nil || states != "replaced,replaced,failed,deployed,deploying" {
		t.Fatalf("states %q %v", states, err)
	}
	var stopping int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM env_instances WHERE phase = 'stopping'").
		Scan(&stopping); err != nil || stopping != 2 {
		t.Fatalf("stopping %d %v", stopping, err)
	}
	for _, q := range []string{
		"UPDATE env_releases SET state = 'pending' WHERE number = 5",
		"UPDATE env_releases SET state = 'superseded' WHERE number = 1",
		"UPDATE env_instances SET phase = 'retiring' WHERE sandbox_id = 'abcdefghjk0'",
	} {
		if _, err := d.Pool.Exec(ctx, q); err == nil {
			t.Errorf("an earlier word was admitted: %s", q)
		}
	}
}
