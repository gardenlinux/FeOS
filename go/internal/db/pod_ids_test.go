// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// migrateBefore applies the migrations before version on a fresh database
// and records them, as a database of an earlier ferund has them.
func migrateBefore(t *testing.T, d *db.DB, version int) {
	t.Helper()
	migrateRange(t, d, 0, version)
}

// migrateRange applies the migrations from version from up to before
// version to and records them, as the ferund of to-1 would.
func migrateRange(t *testing.T, d *db.DB, from, to int) {
	t.Helper()
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	ctx := context.Background()
	err = d.Tx(ctx, func(tx pgx.Tx) error {
		for _, f := range files {
			num, _, _ := strings.Cut(filepath.Base(f), "_")
			v, err := strconv.Atoi(num)
			if err != nil {
				return err
			}
			if v < from || v >= to {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(b)); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", v); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 0008 turns the sandbox IDs' columns of a populated database from uuid
// into text (ADR 0019): every row keeps its ID in the lowercase form, the
// foreign keys hold and cascade again, short IDs fit beside the UUIDs,
// anything else is refused, and the TAP gate's index serves the UUIDs.
func TestPodIDMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 8)

	const (
		a = "0199f0a2-7b1c-7d3e-8f00-1234567890ab" // on n1, with every kind of dependent row
		b = "0199f0a2-ffff-4000-8000-000000000001" // on n1, the same TAP name as a's; an instance
		c = "3f2c0000-0000-4000-8000-000000000001"
	)
	var today time.Time
	if err := d.Pool.QueryRow(ctx, "SELECT (now() AT TIME ZONE 'utc')::date").Scan(&today); err != nil {
		t.Fatal(err)
	}
	setup := []string{
		"INSERT INTO tenants (id) VALUES ('acme')",
		"INSERT INTO nodes (name) VALUES ('n1')",
		"INSERT INTO environments (id, owner, name) VALUES ('11111111-1111-4111-8111-111111111111', 'acme', 'stage')",
		`INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib, client_state, node) VALUES
			('` + a + `', 'acme', 'img', 1, 512, 'running', 'n1'),
			('` + strings.ToUpper(c) + `', 'acme', 'img', 1, 512, 'pending', NULL)`,
		`INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib, client_state, node, environment_id) VALUES
			('` + b + `', 'acme', 'img', 1, 512, 'paused', 'n1', '11111111-1111-4111-8111-111111111111')`,
		"INSERT INTO sandbox_tokens (hash, sandbox_id) VALUES ('h1', '" + a + "')",
		"INSERT INTO sandbox_holds (sandbox_id, holder, kind, lease_until) VALUES ('" + a + "', 'process:p1', 'process', 'infinity')",
		"INSERT INTO bookings (pod_id, node, vcpus, memory_mib, gated) VALUES ('" + a + "', 'n1', 1, 512, true)",
		"INSERT INTO watches (id, sandbox_id, expires_at) VALUES (gen_random_uuid(), '" + a + "', now() + interval '1 hour')",
		`INSERT INTO kube_bindings (sandbox_id, name, port, server, auth_type, sealed, fingerprint)
			VALUES ('` + a + `', 'k', 6443, 'https://k', 'token', '\x01', 'sha256:x')`,
		`INSERT INTO ssh_bindings (sandbox_id, name, server, ssh_user, auth_type, key_type, fingerprint, public_key, sealed)
			VALUES ('` + a + `', 's', '', 'u', 'generate', 'ed25519', 'SHA256:x', 'ssh-ed25519 AAAA', '\x01')`,
		"INSERT INTO pod_tombstones (pod_id, node, incarnation) VALUES ('" + a + "', 'n1', gen_random_uuid())",
		`INSERT INTO issued_certs (serial, pod_id, incarnation, node, spiffe_id, hpke_public_key, not_after)
			VALUES ('s1', '` + a + `', gen_random_uuid(), 'n1', 'spiffe://x', '\x00', now())`,
		`INSERT INTO env_instances (sandbox_id, env_id, kind, object, template_hash, release)
			VALUES ('` + b + `', '11111111-1111-4111-8111-111111111111', 'service', 'web', 'h', 1)`,
		`INSERT INTO env_job_runs (env_id, job, template_hash, release, sandbox_id)
			VALUES ('11111111-1111-4111-8111-111111111111', 'j', 'h', 1, '` + b + `')`,
		`INSERT INTO env_attached (env_id, service, sandbox_id)
			VALUES ('11111111-1111-4111-8111-111111111111', 'svc', '` + a + `')`,
		`INSERT INTO env_volumes (env_id, object, volume_id, size_mib, holder)
			VALUES ('11111111-1111-4111-8111-111111111111', 'addon/pg', gen_random_uuid(), 64, '` + b + `')`,
	}
	for _, table := range []string{"activity", "traffic_records"} {
		setup = append(setup, "CREATE TABLE "+db.PartitionName(table, today)+" PARTITION OF "+table+
			" FOR VALUES FROM ('"+today.Format(time.DateOnly)+"') TO ('"+today.AddDate(0, 0, 1).Format(time.DateOnly)+"')")
	}
	setup = append(setup,
		"INSERT INTO activity (ts_ns, sandbox_id, pod_id, source, kind) VALUES (1, '"+a+"', '"+a+"', 'ferund', 'x')",
		"INSERT INTO activity (ts_ns, source, kind) VALUES (2, 'ferund', 'y')",
		`INSERT INTO traffic_records (ts_ns, node, start_id, seq, pod_id, sandbox_id, kind, detail)
			VALUES (1, 'n1', gen_random_uuid(), 1, '`+a+`', '`+a+`', 'flow', '{}')`)
	for _, q := range setup {
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	if err := d.Migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}

	// The IDs, as text in the lowercase form uuid::text wrote.
	var ids []string
	rows, err := d.Pool.Query(ctx, "SELECT id FROM sandboxes ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if ids, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != a+","+b+","+c {
		t.Fatalf("ids %v", ids)
	}
	for _, q := range []string{
		"SELECT sandbox_id FROM sandbox_tokens", "SELECT sandbox_id FROM sandbox_holds", "SELECT pod_id FROM bookings",
		"SELECT sandbox_id FROM watches", "SELECT sandbox_id FROM kube_bindings", "SELECT sandbox_id FROM ssh_bindings",
		"SELECT pod_id FROM pod_tombstones", "SELECT pod_id FROM issued_certs", "SELECT sandbox_id FROM env_attached",
		"SELECT sandbox_id FROM activity WHERE kind = 'x'", "SELECT pod_id FROM activity WHERE kind = 'x'",
		"SELECT pod_id FROM traffic_records", "SELECT sandbox_id FROM traffic_records",
	} {
		var got string
		if err := d.Pool.QueryRow(ctx, q).Scan(&got); err != nil || got != a {
			t.Fatalf("%s: %q %v", q, got, err)
		}
	}
	for _, q := range []string{"SELECT sandbox_id FROM env_instances", "SELECT sandbox_id FROM env_job_runs",
		"SELECT holder FROM env_volumes"} {
		var got string
		if err := d.Pool.QueryRow(ctx, q).Scan(&got); err != nil || got != b {
			t.Fatalf("%s: %q %v", q, got, err)
		}
	}
	var none *string
	if err := d.Pool.QueryRow(ctx, "SELECT sandbox_id FROM activity WHERE kind = 'y'").Scan(&none); err != nil || none != nil {
		t.Fatalf("a NULL sandbox_id: %v %v", none, err)
	}
	if exists := regclass(t, d, "placements"); exists {
		t.Fatal("placements is still there")
	}

	// The store reads them, and the TAP gate holds for the UUIDs.
	st := store.New(d)
	if sb, err := st.GetSandbox(ctx, podid.ID(c)); err != nil || sb.ID != podid.ID(c) {
		t.Fatalf("get %s: %+v %v", c, sb, err)
	}
	if other, err := st.TAPConflict(ctx, "n1", podid.ID(a)); err != nil || other != podid.ID(b) {
		t.Fatalf("TAP conflict of %s: %q %v", a, other, err)
	}
	if other, err := st.TAPConflict(ctx, "n1", podid.ID(c)); err != nil || other != "" {
		t.Fatalf("TAP conflict of %s: %q %v", c, other, err)
	}
	// The gate's query is served by sandboxes_tap.
	var plan strings.Builder
	err = d.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `EXPLAIN SELECT s.id FROM sandboxes s WHERE s.node = $1
			AND length($2::text) = 36 AND length(s.id) = 36 AND left(s.id, 8) = left($2::text, 8) AND s.id <> $2::text`,
			"n1", a)
		if err != nil {
			return err
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		plan.WriteString(strings.Join(lines, "\n"))
		return err
	})
	if err != nil || !strings.Contains(plan.String(), "sandboxes_tap") {
		t.Fatalf("plan %v:\n%s", err, plan.String())
	}

	// A short ID fits beside them, its dependents too; anything else is
	// refused.
	short := podid.Mint()
	if _, err := d.Pool.Exec(ctx, "INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib) VALUES ($1, 'acme', 'img', 1, 512)",
		string(short)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, "INSERT INTO sandbox_tokens (hash, sandbox_id) VALUES ('h2', $1)", string(short)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"nope", strings.ToUpper(a), strings.ToUpper(string(short)), ""} {
		if _, err := d.Pool.Exec(ctx, "INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib) VALUES ($1, 'acme', 'img', 1, 512)",
			bad); err == nil {
			t.Fatalf("the ID %q was accepted", bad)
		}
	}
	if _, err := d.Pool.Exec(ctx, "INSERT INTO sandbox_tokens (hash, sandbox_id) VALUES ('h3', $1)", string(podid.Mint())); err == nil {
		t.Fatal("a token of no sandbox was accepted")
	}

	// The foreign keys cascade again.
	if _, err := d.Pool.Exec(ctx, "DELETE FROM sandboxes WHERE id = $1", a); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"sandbox_tokens WHERE sandbox_id", "sandbox_holds WHERE sandbox_id", "bookings WHERE pod_id",
		"watches WHERE sandbox_id", "kube_bindings WHERE sandbox_id", "ssh_bindings WHERE sandbox_id"} {
		var n int
		if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM "+q+" = $1", a).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d rows of the deleted sandbox, %v", q, n, err)
		}
	}
	if _, err := d.Pool.Exec(ctx, "DELETE FROM sandboxes WHERE id = $1", b); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM env_instances").Scan(&n); err != nil || n != 0 {
		t.Fatalf("env_instances: %d %v", n, err)
	}
}

func regclass(t *testing.T, d *db.DB, name string) bool {
	t.Helper()
	var exists bool
	if err := d.Pool.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}
