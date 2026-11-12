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

// 0034 (ADR 0054) leaves every environment as it was: a public one and a
// private one from before admit anyone, with no provider, and the
// release before still inserts and publishes environments with its
// column lists. Service tokens keep their rows and take the kind auth.
// The new tables refuse what the contracts rule out: a provider of kind
// tenant without a URL, the reserved name ferun, an environment naming a
// provider its tenant lacks, the delete of a provider an environment
// names, and a visit's binding that is not a hash.
func TestVisitorsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 34)

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
		public  = "00000000-0000-4000-8000-000000000001"
		private = "00000000-0000-4000-8000-000000000002"
		later   = "00000000-0000-4000-8000-000000000003"
		visitID = "00000000-0000-4000-8000-0000000000a1"
		hash43  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	)
	exec("INSERT INTO tenants (id) VALUES ('acme'), ('other')")
	exec(`INSERT INTO environments (id, owner, name, public, ingress_group, host_label) VALUES
		($1, 'acme', 'stage', true, true, 'stage--acme'), ($2, 'acme', 'dev', false, false, NULL)`, public, private)
	exec("INSERT INTO service_tokens (hash, kind, name) VALUES ('h-sink', 'sink', 'sink-0')")

	migrateRange(t, d, 34, 35)

	for _, id := range []string{public, private} {
		var access string
		var provider *string
		if err := d.Pool.QueryRow(ctx, "SELECT access, access_provider FROM environments WHERE id = $1", id).
			Scan(&access, &provider); err != nil {
			t.Fatal(err)
		}
		if access != "anyone" || provider != nil {
			t.Errorf("environment %s: access %q provider %v, want anyone and NULL", id, access, provider)
		}
	}
	var kind string
	if err := d.Pool.QueryRow(ctx, "SELECT kind FROM service_tokens WHERE hash = 'h-sink'").Scan(&kind); err != nil || kind != "sink" {
		t.Fatalf("the sink token: %q %v", kind, err)
	}
	exec("INSERT INTO service_tokens (hash, kind, name) VALUES ('h-auth', 'auth', 'ferun-auth')")
	refused("23514", "INSERT INTO service_tokens (hash, kind, name) VALUES ('h-x', 'other', 'x')")

	// The release before: its insert's column list, and its publish.
	exec("INSERT INTO environments (id, owner, name, labels) VALUES ($1, 'acme', 'prod', '{}'::jsonb)", later)
	exec("UPDATE environments SET public = true, ingress_group = true, host_label = 'prod--acme' WHERE id = $1", later)
	var access string
	if err := d.Pool.QueryRow(ctx, "SELECT access FROM environments WHERE id = $1", later).Scan(&access); err != nil ||
		access != "anyone" {
		t.Fatalf("an environment the release before made: %q %v", access, err)
	}

	// Providers.
	exec("INSERT INTO access_providers (tenant, name, kind, url) VALUES ('acme', 'fecode', 'tenant', 'https://fecode.example/api/v1/visit')")
	exec(`INSERT INTO access_providers (tenant, name, kind, issuer, client_id, client_secret_sealed, rule)
		VALUES ('acme', 'sso', 'oidc', 'https://login.example/', 'abc', '\x01'::bytea, '{"anyone": true}')`)
	refused("23514", "INSERT INTO access_providers (tenant, name, kind) VALUES ('acme', 'nourl', 'tenant')")
	refused("23514", `INSERT INTO access_providers (tenant, name, kind, url, issuer) VALUES
		('acme', 'mixed', 'tenant', 'https://x.example/', 'https://login.example/')`)
	refused("23514", "INSERT INTO access_providers (tenant, name, kind, issuer, client_id) VALUES ('acme', 'nosecret', 'oidc', 'https://l.example/', 'abc')")
	refused("23514", "INSERT INTO access_providers (tenant, name, kind, url) VALUES ('acme', 'ferun', 'tenant', 'https://x.example/')")
	refused("23514", "INSERT INTO access_providers (tenant, name, kind, url) VALUES ('acme', 'Bad_Name', 'tenant', 'https://x.example/')")
	refused("23514", "INSERT INTO access_providers (tenant, name, kind) VALUES ('acme', 'ferun-like', 'ferun')")

	// An environment names a provider of its own tenant, or ferun (NULL),
	// and only with visitors.
	exec("UPDATE environments SET access = 'visitors', access_provider = 'fecode' WHERE id = $1", public)
	exec("UPDATE environments SET access = 'visitors', access_provider = NULL WHERE id = $1", later)
	refused("23503", "UPDATE environments SET access = 'visitors', access_provider = 'missing' WHERE id = $1", private)
	exec("INSERT INTO access_providers (tenant, name, kind, url) VALUES ('other', 'theirs', 'tenant', 'https://o.example/')")
	refused("23503", "UPDATE environments SET access = 'visitors', access_provider = 'theirs' WHERE id = $1", private)
	refused("23514", "UPDATE environments SET access = 'anyone', access_provider = 'fecode' WHERE id = $1", private)
	refused("23514", "UPDATE environments SET access = 'everyone' WHERE id = $1", private)
	refused("23503", "DELETE FROM access_providers WHERE tenant = 'acme' AND name = 'fecode'")
	exec("DELETE FROM access_providers WHERE tenant = 'acme' AND name = 'sso'")

	// Visits and answers.
	insertVisit := func(binding string) error {
		_, err := d.Pool.Exec(ctx, `INSERT INTO visits (id, env_id, tenant, provider, provider_kind, host, subject, ends_at, binding)
			VALUES ($1, $2, 'acme', 'fecode', 'tenant', 'stage--acme.apps.example', 'u-1', now() + interval '1 hour', $3)`,
			visitID, public, binding)
		return err
	}
	var pe *pgconn.PgError
	if err := insertVisit(hash43[:42]); !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatalf("a binding of 42 characters: %v", err)
	}
	if err := insertVisit("+" + hash43[1:]); !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatalf("a binding outside base64url: %v", err)
	}
	if err := insertVisit(""); err != nil {
		t.Fatalf("a visit not redeemed yet: %v", err)
	}
	exec("UPDATE visits SET binding = $2 WHERE id = $1", visitID, hash43)
	refused("23514", "UPDATE visits SET ended_at = now() WHERE id = $1", visitID)
	refused("23514", "UPDATE visits SET end_reason = 'expired' WHERE id = $1", visitID)
	exec("UPDATE visits SET ended_at = now(), end_reason = 'signed_out' WHERE id = $1", visitID)
	refused("23514", `INSERT INTO visits (id, env_id, tenant, provider, provider_kind, host, subject, ends_at)
		VALUES (gen_random_uuid(), $1, 'acme', 'ferun', 'ferun', 'h', 'u', now())`, public)
	refused("23514", `INSERT INTO visits (id, env_id, tenant, provider, provider_kind, host, subject, ends_at)
		VALUES (gen_random_uuid(), $1, 'acme', 'fecode', 'tenant', 'h', '', now())`, public)
	exec(`INSERT INTO visit_answers (challenge_id, env_id, host, outcome, visit_id, binding, code_expires_at, expires_at)
		VALUES ($1, $2, 'h', 'accepted', $3, $4, now(), now())`, strings.Repeat("a", 22), public, visitID, hash43)
	refused("23505", `INSERT INTO visit_answers (challenge_id, env_id, host, outcome, binding, code_expires_at, expires_at)
		VALUES ($1, $2, 'h', 'rejected', $3, now(), now())`, strings.Repeat("a", 22), public, hash43)
	refused("23514", `INSERT INTO visit_answers (challenge_id, env_id, host, outcome, binding, code_expires_at, expires_at)
		VALUES ($1, $2, 'h', 'accepted', $3, now(), now())`, strings.Repeat("b", 22), public, hash43)
	refused("23514", `INSERT INTO visit_answers (challenge_id, env_id, host, outcome, binding, code_expires_at, expires_at)
		VALUES ($1, $2, 'h', 'rejected', $3, now(), now())`, strings.Repeat("c", 21), public, hash43)

	// The environment's delete takes its visits and answers along.
	exec("DELETE FROM environments WHERE id = $1", public)
	var n int
	if err := d.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM visits) + (SELECT count(*) FROM visit_answers)").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left after the environment's delete: %d %v", n, err)
	}
}
