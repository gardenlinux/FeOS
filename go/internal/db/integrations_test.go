// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

// 0042 (ADR 0056, 0060) adds integrations, their grants and the tokens
// they mint, and protected environments, keeping every row of before: a
// grant of a person or a token stays and still conflicts on 0027's
// index; a grant has exactly one principal, an integration's grant one
// per scope; an integration's removal takes its grants and its tokens;
// an environment is unprotected unless set.
func TestIntegrationsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 42)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	fails := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, q, args...); err == nil {
			t.Fatalf("%s: no error", q)
		}
	}
	const (
		env    = "0193a000-aaaa-7bbb-8ccc-000000000042"
		person = "0193a000-aaaa-7bbb-8ccc-0000000000aa"
		sb     = "0199f0a2-0000-4000-8000-000000000042"
	)
	exec("INSERT INTO projects (id) VALUES ('acme')")
	exec(`INSERT INTO environments (id, owner, name) VALUES ('` + env + `', 'acme', 'prod')`)
	exec(`INSERT INTO people (id, issuer, sub) VALUES ('` + person + `', 'https://id.example/', 'ada')`)
	exec("INSERT INTO project_tokens (hash, project_id, label) VALUES ('h1', 'acme', 'release')")
	exec(`INSERT INTO grants (project_id, person_id, role) VALUES ('acme', '` + person + `', 'admin')`)
	exec(`INSERT INTO grants (project_id, token_hash, environment_id, role) VALUES ('acme', 'h1', '` + env + `', 'write')`)
	exec(`INSERT INTO sandboxes (id, owner, image, vcpus, memory_mib, environment_id) VALUES
		('` + sb + `', 'acme', 'img', 1, 512, '` + env + `')`)

	if err := d.Migrate(ctx, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var n int
	var protected bool
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM grants").Scan(&n); err != nil || n != 2 {
		t.Fatalf("grants after: %d, %v", n, err)
	}
	if err := d.Pool.QueryRow(ctx, "SELECT protected FROM environments").Scan(&protected); err != nil || protected {
		t.Fatalf("protected: %v, %v", protected, err)
	}
	// The release before's upsert still finds its index.
	exec(`INSERT INTO grants (project_id, person_id, role) VALUES ('acme', '` + person + `', 'read')
		ON CONFLICT (project_id, (coalesce(person_id::text, token_hash)), (coalesce(environment_id::text, '')))
		DO UPDATE SET role = excluded.role`)

	exec(`INSERT INTO integrations (name, title, issuer, subject) VALUES ('fecode', 'FeCode', 'https://id.example/', 'client-1')`)
	fails(`INSERT INTO integrations (name, issuer, subject) VALUES ('other', 'https://id.example/', 'client-1')`)
	fails(`INSERT INTO integrations (name, issuer, subject) VALUES ('Bad_Name', 'https://id.example/', 'c2')`)
	fails(`INSERT INTO integrations (name, issuer, subject) VALUES ('empty', 'https://id.example/', '')`)
	in := "(SELECT id FROM integrations WHERE name = 'fecode')"
	exec(`INSERT INTO grants (project_id, integration_id, role) VALUES ('acme', ` + in + `, 'write')`)
	exec(`INSERT INTO grants (project_id, integration_id, environment_id, role) VALUES ('acme', ` + in + `, '` + env + `', 'admin')`)
	// One grant per scope, and one principal per grant.
	fails(`INSERT INTO grants (project_id, integration_id, role) VALUES ('acme', ` + in + `, 'read')`)
	fails(`INSERT INTO grants (project_id, integration_id, person_id, role) VALUES ('acme', ` + in + `, '` + person + `', 'read')`)
	fails(`INSERT INTO grants (project_id, role) VALUES ('acme', 'read')`)

	exec(`INSERT INTO env_route_tokens (hash, env_id, integration_id) VALUES ('r1', '` + env + `', ` + in + `)`)
	exec(`INSERT INTO env_route_tokens (hash, env_id) VALUES ('r2', '` + env + `')`)
	exec(`INSERT INTO sandbox_tokens (hash, sandbox_id, label, integration_id) VALUES ('s1', '` + sb + `', 'x', ` + in + `)`)
	exec(`INSERT INTO sandbox_tokens (hash, sandbox_id, label) VALUES ('s2', '` + sb + `', 'y')`)
	exec("UPDATE environments SET protected = true")

	exec("DELETE FROM integrations WHERE name = 'fecode'")
	var grants, routes, sandboxes int
	if err := d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM grants), (SELECT count(*) FROM env_route_tokens),
		(SELECT count(*) FROM sandbox_tokens)`).Scan(&grants, &routes, &sandboxes); err != nil {
		t.Fatal(err)
	}
	if grants != 2 || routes != 1 || sandboxes != 1 {
		t.Fatalf("after the removal: %d grants, %d route tokens, %d sandbox tokens; want 2, 1, 1", grants, routes, sandboxes)
	}
}
