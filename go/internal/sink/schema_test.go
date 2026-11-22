// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// The names before 0009, which the release before writes.
const (
	oldColumn = "tenant"       // KEEP-TENANT: the column before 0009
	oldLabel  = "ferun.tenant" // KEEP-TENANT: the label before 0009
)

// 0009 renames the old column of the streams and of the usage tables to
// project, keeps their rows, and rewrites a stream's old label as
// ferun.project, leaving every other label and a stream without it alone;
// the indexes on the column follow it.
func TestProjectsMigration(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d := dbtest.Open(t)

	// The sink of the release before: its migrations up to 0008.
	before := fstest.MapFS{}
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := strings.TrimPrefix(f, "migrations/")
		if name >= "0009" {
			continue
		}
		b, err := migrationFS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = &fstest.MapFile{Data: b}
	}
	if err := d.MigrateFS(ctx, before, nil); err != nil {
		t.Fatalf("migrate to 0008: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO log_streams (sid, pod, container, log_id, `+oldColumn+`, node, environment, labels) VALUES
		('s1', 'p1', 'app', 'l1', 'acme', 'n1', 'prod', jsonb_build_object($1::text, 'acme', 'ferun.environment', 'prod')),
		('s2', 'p2', 'app', 'l2', 'shop', 'n1', '', '{"service.name": "x"}')`, oldLabel)
	exec("INSERT INTO log_usage (" + oldColumn + ", day, bytes) VALUES ('acme', '2026-11-20', 1234)")
	exec("INSERT INTO trace_usage (" + oldColumn + ", day, bytes) VALUES ('acme', '2026-11-20', 567)")

	if err := Migrate(ctx, d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, err := d.Pool.Query(ctx, "SELECT sid, project, labels::text FROM log_streams ORDER BY sid")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var sid, project, labels string
		if err := rows.Scan(&sid, &project, &labels); err != nil {
			t.Fatal(err)
		}
		got = append(got, sid+" "+project+" "+labels)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{`s1 acme {"ferun.project": "acme", "ferun.environment": "prod"}`, `s2 shop {"service.name": "x"}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("streams:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var logBytes, traceBytes int64
	if err := d.Pool.QueryRow(ctx, `SELECT (SELECT bytes FROM log_usage WHERE project = 'acme'),
		(SELECT bytes FROM trace_usage WHERE project = 'acme')`).Scan(&logBytes, &traceBytes); err != nil ||
		logBytes != 1234 || traceBytes != 567 {
		t.Fatalf("usage: logs %d traces %d, %v", logBytes, traceBytes, err)
	}
	var oldCols int
	var idx string
	if err := d.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND column_name = $1),
		(SELECT string_agg(indexdef, E'\n' ORDER BY indexname) FROM pg_indexes WHERE tablename = 'log_streams'
			AND indexname IN ('log_streams_env', 'log_streams_job', 'log_streams_instance', 'log_streams_last'))`, oldColumn).
		Scan(&oldCols, &idx); err != nil {
		t.Fatal(err)
	}
	if oldCols != 0 || strings.Count(idx, "(project,") != 4 {
		t.Fatalf("columns of the old name: %d; indexes:\n%s", oldCols, idx)
	}
}
