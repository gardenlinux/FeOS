// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package dbtest gives tests a database of their own. The PostgreSQL server
// is named by FERUN_TEST_DATABASE_URL (`make go-test` runs one in docker);
// without it the tests that need a database skip. Every call makes a new
// database on that server and drops it when the test ends, so tests may run
// in parallel.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
)

// EnvURL names the variable that points the tests at a PostgreSQL server.
const EnvURL = "FERUN_TEST_DATABASE_URL"

// URL returns the server's URL from EnvURL, or skips the test.
func URL(t testing.TB) string {
	t.Helper()
	u := os.Getenv(EnvURL)
	if u == "" {
		t.Skipf("%s is not set; skipping a test that needs PostgreSQL", EnvURL)
	}
	return u
}

// NewDatabase creates an empty database on the server and returns its URL.
// The database is dropped when the test ends.
func NewDatabase(t testing.TB) string {
	t.Helper()
	admin := URL(t)
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "ferun_test_" + hex.EncodeToString(b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect to %s: %v", EnvURL, err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Errorf("drop test database %s: %v", name, err)
			return
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})
	return withDatabase(t, admin, name)
}

// withDatabase returns the server URL u pointed at database name.
func withDatabase(t testing.TB, u, name string) string {
	if strings.HasPrefix(u, "postgres://") || strings.HasPrefix(u, "postgresql://") {
		p, err := url.Parse(u)
		if err != nil {
			t.Fatalf("parse %s: %v", EnvURL, err)
		}
		p.Path = "/" + name
		return p.String()
	}
	// A keyword/value string: a later keyword overrides an earlier one.
	return u + " dbname=" + name
}

// Open opens a fresh, empty database (Open, no migration, no listener) and
// closes it when the test ends.
func Open(t testing.TB) *db.DB {
	t.Helper()
	u := NewDatabase(t)
	d, err := db.Open(context.Background(), u, Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

// New returns a fresh database, migrated (without a bootstrap), with its
// listener running, so that Subscribe and WaitFor work. Everything is
// stopped, closed and dropped when the test ends.
func New(t testing.TB) *db.DB {
	t.Helper()
	d := Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := d.Migrate(ctx, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	StartListener(t, d)
	return d
}

// StartListener runs d's listener until the test ends and waits until it
// listens.
func StartListener(t testing.TB, d *db.DB) {
	t.Helper()
	lctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.RunListener(lctx)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.WaitListening(ctx); err != nil {
		t.Fatalf("listener: %v", err)
	}
}

// Logger returns a logger that writes to the test's log at level Info and
// above while the test runs, and drops what comes after it ended.
func Logger(t testing.TB) *slog.Logger {
	w := &testWriter{t: t}
	t.Cleanup(w.close)
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

type testWriter struct {
	mu     sync.Mutex
	t      testing.TB
	closed bool
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return len(p), nil
	}
	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

func (w *testWriter) close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

var _ io.Writer = (*testWriter)(nil)

// Environment is the ID of owner's environment name, inserted when it
// has none: the place of a test's sandboxes and tenants' jobs, which
// every one is in (ADR 0035). q is a pool or a transaction of a migrated
// database where owner exists.
func Environment(t testing.TB, q db.Querier, owner, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := q.QueryRow(context.Background(), `WITH e AS (INSERT INTO environments (id, owner, name)
			VALUES (gen_random_uuid(), $1, $2) ON CONFLICT (owner, name) DO NOTHING RETURNING id)
		SELECT id FROM e UNION ALL SELECT id FROM environments WHERE owner = $1 AND name = $2 LIMIT 1`,
		owner, name).Scan(&id); err != nil {
		t.Fatalf("environment %s of %s: %v", name, owner, err)
	}
	return id
}
