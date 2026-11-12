// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey is the advisory lock that serialises migrations of all
// instances of one database ("ferun" and a 1).
const migrationLockKey int64 = 0x666572756e_0001

// MinServerVersion is the oldest PostgreSQL ferund runs on: the schema
// declares identity columns on partitioned tables (server_version_num).
const MinServerVersion = 170000

// ErrServerVersion is Migrate's refusal of an older PostgreSQL.
var ErrServerVersion = errors.New("ferund needs PostgreSQL 17 or newer")

// SecretMCPSession names the cluster secret that keys MCP session ids.
const SecretMCPSession = "mcp-session"

// migration is one embedded file, NNNN_name.sql.
type migration struct {
	version int
	name    string
	sql     string
}

func migrations() ([]migration, error) {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	return readMigrations(sub)
}

// readMigrations reads the files NNNN_name.sql at the root of fsys, in
// the order of their numbers. Two files of one number are refused:
// Migrate keys schema_migrations by the number alone, so a database that
// has one of them would skip the other without a word.
func readMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var ms []migration
	seen := map[int]string{}
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			return nil, fmt.Errorf("migration %s: name is not NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if other, ok := seen[v]; ok {
			return nil, fmt.Errorf("migrations %s and %s: one number for two migrations", other, e.Name())
		}
		seen[v] = e.Name()
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

// CheckServerVersion refuses a server_version_num below MinServerVersion.
func CheckServerVersion(num int) error {
	if num < MinServerVersion {
		return fmt.Errorf("%w (the server's version number is %d)", ErrServerVersion, num)
	}
	return nil
}

// Migrate refuses PostgreSQL below 17, then, in one transaction under an
// advisory lock, applies the embedded migrations the database lacks and
// records them in schema_migrations, creates the cluster secret
// "mcp-session" if missing, creates the activity partitions of today and
// tomorrow (DB clock, UTC) and runs bootstrap (nil: none). Instances
// starting together serialise on the lock: one applies, the others find the
// schema applied.
func (d *DB) Migrate(ctx context.Context, bootstrap func(ctx context.Context, tx pgx.Tx) error) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	return d.migrate(ctx, ms, func(ctx context.Context, tx pgx.Tx) error {
		if err := ensureClusterSecret(ctx, tx, SecretMCPSession); err != nil {
			return err
		}
		var today time.Time
		if err := tx.QueryRow(ctx, "SELECT (now() AT TIME ZONE 'utc')::date").Scan(&today); err != nil {
			return err
		}
		if err := ensurePartitions(ctx, tx, today, today.AddDate(0, 0, 1)); err != nil {
			return err
		}
		if bootstrap != nil {
			if err := bootstrap(ctx, tx); err != nil {
				return fmt.Errorf("bootstrap: %w", err)
			}
		}
		return nil
	})
}

// MigrateFS is Migrate for a database of another program (ferun-sink):
// the migrations are the files NNNN_name.sql at the root of fsys, and
// then (nil: none) runs in the same transaction after them. Nothing of
// ferund's own is created.
func (d *DB) MigrateFS(ctx context.Context, fsys fs.FS, then func(ctx context.Context, tx pgx.Tx) error) error {
	ms, err := readMigrations(fsys)
	if err != nil {
		return err
	}
	return d.migrate(ctx, ms, then)
}

// migrate refuses PostgreSQL below 17, then applies ms under the advisory
// lock in one transaction, records them in schema_migrations and runs
// then (nil: none).
func (d *DB) migrate(ctx context.Context, ms []migration, then func(ctx context.Context, tx pgx.Tx) error) error {
	var num int
	if err := d.Pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
		return fmt.Errorf("read the server version: %w", err)
	}
	if err := CheckServerVersion(num); err != nil {
		return err
	}
	return d.Tx(ctx, func(tx pgx.Tx) error {
		// The lock may be held by another instance's migration, which
		// takes longer than the session's lock_timeout allows.
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '2min'"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '5min'"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
			return fmt.Errorf("migration lock: %w", err)
		}
		applied := map[int]bool{}
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
			return err
		}
		if exists {
			rows, err := tx.Query(ctx, "SELECT version FROM schema_migrations")
			if err != nil {
				return err
			}
			vs, err := pgx.CollectRows(rows, pgx.RowTo[int32])
			if err != nil {
				return err
			}
			for _, v := range vs {
				applied[int(v)] = true
			}
		}
		for _, m := range ms {
			if applied[m.version] {
				continue
			}
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return fmt.Errorf("migration %s: %w", m.name, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", m.version); err != nil {
				return fmt.Errorf("record migration %s: %w", m.name, err)
			}
			d.log.Info("applied migration", "migration", m.name)
		}
		if then != nil {
			return then(ctx, tx)
		}
		return nil
	})
}

// ensureClusterSecret creates the secret name with 32 random bytes unless
// it exists.
func ensureClusterSecret(ctx context.Context, q Querier, name string) error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, "INSERT INTO cluster_secrets (name, value) VALUES ($1, $2) ON CONFLICT DO NOTHING", name, b); err != nil {
		return fmt.Errorf("cluster secret %s: %w", name, err)
	}
	return nil
}
