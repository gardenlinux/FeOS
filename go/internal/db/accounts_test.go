// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// 0043 (ADR 0059) puts every project there is on the account default
// since it was made, brought by nobody; an invitation of the release
// before still inserts. The new tables refuse what the rules rule out:
// an account invitation with an environment or a project's role, a
// project invitation with an account's role, two current accounts of one
// project, an admin row on default, the delete of an account a project
// was on, and a row that leaves before it came.
func TestAccountsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 43)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	refused := func(code, q string, args ...any) {
		t.Helper()
		_, err := d.Pool.Exec(ctx, q, args...)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != code {
			t.Fatalf("%s: %v, want SQLSTATE %s", q, err, code)
		}
	}
	const (
		person = "00000000-0000-4000-8000-000000000001"
		env    = "00000000-0000-4000-8000-000000000002"
	)
	exec("INSERT INTO people (id, issuer, sub) VALUES ($1, 'https://id.example.test/', 'p')", person)
	exec("INSERT INTO projects (id, created_at) VALUES ('acme', '2026-01-02T03:04:05Z')")
	exec("INSERT INTO projects (id, made_by) VALUES ('shop', $1)", person)
	exec("INSERT INTO projects (id, deleting_at) VALUES ('gone', now())")
	exec("INSERT INTO environments (id, owner, name) VALUES ($1, 'acme', 'stage')", env)

	migrateRange(t, d, 43, 44)

	var title string
	if err := d.Pool.QueryRow(ctx, "SELECT title FROM accounts WHERE id = 'default'").Scan(&title); err != nil ||
		title != "Default" {
		t.Fatalf("default: %q %v", title, err)
	}
	rows, err := d.Pool.Query(ctx, `SELECT a.project_id, a.account_id, a.moved_by, a.moved_at = p.created_at,
			a.left_at IS NULL
		FROM project_accounts a JOIN projects p ON p.id = a.project_id ORDER BY a.project_id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var project, account, by string
		var since, current bool
		if err := rows.Scan(&project, &account, &by, &since, &current); err != nil {
			t.Fatal(err)
		}
		if account != "default" || by != "" || !since || !current {
			t.Errorf("%s: %s %q since %v current %v", project, account, by, since, current)
		}
		got = append(got, project)
	}
	if rows.Err() != nil || len(got) != 3 {
		t.Fatalf("rows %v %v", got, rows.Err())
	}

	// The release before's invitations insert as they did.
	exec(`INSERT INTO invitations (hash, project_id, environment_id, role, made_by, expires_at)
		VALUES ('i1', 'acme', $1, 'write', 'person:x', now() + interval '1 day')`, env)
	exec(`INSERT INTO invitations (hash, project_id, role, made_by, expires_at)
		VALUES ('i2', 'acme', 'admin', 'person:x', now() + interval '1 day')`)

	exec("INSERT INTO accounts (id, title, reference) VALUES ('cc-4711', 'Team X', 'CC 4711')")
	exec(`INSERT INTO invitations (hash, account_id, role, made_by, expires_at)
		VALUES ('i3', 'cc-4711', 'member', 'person:x', now() + interval '1 day')`)
	refused("23514", `INSERT INTO invitations (hash, account_id, role, made_by, expires_at)
		VALUES ('i4', 'cc-4711', 'read', '', now())`)
	refused("23514", `INSERT INTO invitations (hash, account_id, environment_id, role, made_by, expires_at)
		VALUES ('i4', 'cc-4711', $1, 'member', '', now())`, env)
	refused("23514", `INSERT INTO invitations (hash, project_id, role, made_by, expires_at)
		VALUES ('i4', 'acme', 'member', '', now())`)
	refused("23514", `INSERT INTO invitations (hash, project_id, account_id, role, made_by, expires_at)
		VALUES ('i4', 'acme', 'cc-4711', 'admin', '', now())`)
	refused("23514", `INSERT INTO invitations (hash, role, made_by, expires_at) VALUES ('i4', 'admin', '', now())`)

	refused("23514", "INSERT INTO accounts (id) VALUES ('Bad Name')")
	exec("INSERT INTO account_members (account_id, person_id, role) VALUES ('cc-4711', $1, 'admin')", person)
	exec("INSERT INTO account_members (account_id, person_id, role) VALUES ('default', $1, 'member')", person)
	refused("23514", "UPDATE account_members SET role = 'admin' WHERE account_id = 'default'")
	refused("23514", "INSERT INTO account_members (account_id, person_id, role) VALUES ('cc-4711', $1, 'owner')", person)

	refused("23505", "INSERT INTO project_accounts (project_id, account_id) VALUES ('acme', 'cc-4711')")
	refused("23514", `INSERT INTO project_accounts (project_id, account_id, moved_at, left_at)
		VALUES ('acme', 'cc-4711', now(), now() - interval '1 second')`)
	// A move: the current row leaves, the next comes at the same moment.
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"UPDATE project_accounts SET left_at = now() WHERE project_id = 'acme' AND left_at IS NULL",
		"INSERT INTO project_accounts (project_id, account_id, moved_by) VALUES ('acme', 'cc-4711', 'person:x')"} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	refused("23503", "DELETE FROM accounts WHERE id = 'cc-4711'")
	refused("23503", "DELETE FROM accounts WHERE id = 'default'")
	// An account no project was ever on goes, with its people and
	// invitations; a project's rows go with it.
	exec("INSERT INTO accounts (id) VALUES ('empty')")
	exec("DELETE FROM accounts WHERE id = 'empty'")
	exec("DELETE FROM projects WHERE id = 'gone'")
	var n int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM project_accounts WHERE project_id = 'gone'").Scan(&n); err != nil ||
		n != 0 {
		t.Fatalf("rows of a deleted project: %d %v", n, err)
	}
	var movedAt, leftAt time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT n.moved_at, o.left_at FROM project_accounts n JOIN project_accounts o
		ON o.project_id = n.project_id AND o.left_at IS NOT NULL WHERE n.project_id = 'acme' AND n.left_at IS NULL`).
		Scan(&movedAt, &leftAt); err != nil || !movedAt.Equal(leftAt) {
		t.Fatalf("the move: came %v, left %v, %v", movedAt, leftAt, err)
	}
}
