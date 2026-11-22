// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

func TestServerVersionCheck(t *testing.T) {
	t.Parallel()
	for _, num := range []int{90600, 160004, 169999} {
		err := db.CheckServerVersion(num)
		if !errors.Is(err, db.ErrServerVersion) {
			t.Fatalf("version %d: %v, want ErrServerVersion", num, err)
		}
		if !strings.HasPrefix(err.Error(), "ferund needs PostgreSQL 17 or newer") {
			t.Fatalf("version %d: text %q", num, err)
		}
	}
	for _, num := range []int{170000, 170004, 180000} {
		if err := db.CheckServerVersion(num); err != nil {
			t.Fatalf("version %d refused: %v", num, err)
		}
	}
}

// Two instances migrating one empty database at once: one applies the
// schema, the other waits for the lock and finds it applied; both run their
// bootstrap after the schema exists.
func TestMigrateConcurrent(t *testing.T) {
	t.Parallel()
	url := dbtest.NewDatabase(t)
	ctx := context.Background()
	var handles [3]*db.DB
	for i := range handles {
		d, err := db.Open(ctx, url, dbtest.Logger(t))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		handles[i] = d
	}
	var bootstraps atomic.Int32
	bootstrap := func(ctx context.Context, tx pgx.Tx) error {
		bootstraps.Add(1)
		// The schema is there: the bootstrap of §11 runs inside the lock.
		_, err := tx.Exec(ctx, `INSERT INTO projects (id) SELECT 'operator'
			WHERE NOT EXISTS (SELECT 1 FROM projects) ON CONFLICT DO NOTHING`)
		return err
	}
	var wg sync.WaitGroup
	errs := make([]error, len(handles))
	for i, d := range handles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = d.Migrate(ctx, bootstrap)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("migrate %d: %v", i, err)
		}
	}
	if n := bootstraps.Load(); n < int32(len(handles)) {
		t.Fatalf("bootstrap ran %d times, want at least %d", n, len(handles))
	}
	d := handles[0]
	var applied, projects, secrets int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM projects").Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM cluster_secrets WHERE name = 'mcp-session' AND length(value) = 32").Scan(&secrets); err != nil {
		t.Fatal(err)
	}
	// Every file in migrations/ is applied once.
	ms, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	if applied != len(ms) || projects != 1 || secrets != 1 {
		t.Fatalf("migrations %d, projects %d, secrets %d; want %d, 1, 1", applied, projects, secrets, len(ms))
	}
	var secret []byte
	if err := d.Pool.QueryRow(ctx, "SELECT value FROM cluster_secrets WHERE name = 'mcp-session'").Scan(&secret); err != nil {
		t.Fatal(err)
	}
	// Migrating again changes nothing.
	if err := d.Migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var again []byte
	if err := d.Pool.QueryRow(ctx, "SELECT value FROM cluster_secrets WHERE name = 'mcp-session'").Scan(&again); err != nil {
		t.Fatal(err)
	}
	if string(again) != string(secret) {
		t.Fatal("a second migration replaced the cluster secret")
	}
	// Today's and tomorrow's partitions exist, so rows can be written.
	if _, err := d.Pool.Exec(ctx, "INSERT INTO activity (ts_ns, source, kind) VALUES (1, 'ferund', 'test')"); err != nil {
		t.Fatalf("insert activity: %v", err)
	}
	days, err := d.PartitionDays(ctx, db.TableTraffic)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("traffic partitions %v, want today and tomorrow", days)
	}
}

func TestSessionSettings(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx := context.Background()
	var lock, stmt, tz string
	if err := d.Pool.QueryRow(ctx, "SELECT current_setting('lock_timeout'), current_setting('statement_timeout'), current_setting('TimeZone')").Scan(&lock, &stmt, &tz); err != nil {
		t.Fatal(err)
	}
	if lock != "5s" || stmt != "30s" || tz != "UTC" {
		t.Fatalf("lock_timeout %s, statement_timeout %s, TimeZone %s", lock, stmt, tz)
	}

	// The server's side of the TCP settings, and the end of a session
	// idle in a transaction. The tcp_* settings read 0 on a Unix socket.
	var unix bool
	if err := d.Pool.QueryRow(ctx, "SELECT inet_server_addr() IS NULL").Scan(&unix); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"idle_in_transaction_session_timeout": "60000"}
	if !unix {
		want["tcp_keepalives_idle"] = "5"
		want["tcp_keepalives_interval"] = "5"
		want["tcp_keepalives_count"] = "3"
		want["tcp_user_timeout"] = "15000"
	}
	rows, err := d.Pool.Query(ctx, `SELECT name, setting FROM pg_settings WHERE name IN
		('tcp_keepalives_idle', 'tcp_keepalives_interval', 'tcp_keepalives_count', 'tcp_user_timeout', 'idle_in_transaction_session_timeout')`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	var name, setting string
	if _, err := pgx.ForEachRow(rows, []any{&name, &setting}, func() error {
		got[name] = setting
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// The server's WARNING notices are logged.
func TestWarningNoticeLogged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var buf syncBuffer
	d, err := db.Open(ctx, dbtest.NewDatabase(t), slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Pool.Exec(ctx, "DO $$ BEGIN RAISE WARNING 'ferun-notice-test'; END $$"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "ferun-notice-test") {
		t.Fatalf("log %q lacks the warning", buf.String())
	}
}

// syncBuffer is a bytes.Buffer safe for a logger and a reader at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// Tx runs the function again after each of the retryable failures, and
// not after others.
func TestTxRetries(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx := context.Background()
	for _, code := range []string{"40001", "40P01", "55P03"} {
		attempts := 0
		err := d.Tx(ctx, func(tx pgx.Tx) error {
			attempts++
			if attempts < 3 {
				_, err := tx.Exec(ctx, fmt.Sprintf("DO $$ BEGIN RAISE EXCEPTION 'injected' USING ERRCODE = '%s'; END $$", code))
				return err
			}
			_, err := tx.Exec(ctx, "SELECT 1")
			return err
		})
		if err != nil || attempts != 3 {
			t.Fatalf("%s: err %v after %d attempts, want success after 3", code, err, attempts)
		}
	}

	// Not retried: another SQLSTATE, or a plain error.
	attempts := 0
	err := d.Tx(ctx, func(tx pgx.Tx) error {
		attempts++
		_, err := tx.Exec(ctx, "DO $$ BEGIN RAISE EXCEPTION 'injected' USING ERRCODE = '23505'; END $$")
		return err
	})
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23505" || attempts != 1 {
		t.Fatalf("unique violation: err %v after %d attempts", err, attempts)
	}

	// Gives up after five retries.
	attempts = 0
	err = d.Tx(ctx, func(tx pgx.Tx) error {
		attempts++
		_, err := tx.Exec(ctx, "DO $$ BEGIN RAISE EXCEPTION 'injected' USING ERRCODE = '40001'; END $$")
		return err
	})
	if !db.Retryable(err) || attempts != 6 {
		t.Fatalf("persistent failure: err %v after %d attempts, want 6", err, attempts)
	}

	// The writes of a failed attempt are rolled back.
	if _, err := d.Pool.Exec(ctx, "CREATE TABLE t (n int)"); err != nil {
		t.Fatal(err)
	}
	attempts = 0
	err = d.Tx(ctx, func(tx pgx.Tx) error {
		attempts++
		if _, err := tx.Exec(ctx, "INSERT INTO t VALUES ($1)", attempts); err != nil {
			return err
		}
		if attempts == 1 {
			_, err := tx.Exec(ctx, "DO $$ BEGIN RAISE EXCEPTION 'injected' USING ERRCODE = '40P01'; END $$")
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var rows []int32
	r, err := d.Pool.Query(ctx, "SELECT n FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if rows, err = pgx.CollectRows(r, pgx.RowTo[int32]); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0] != 2 {
		t.Fatalf("rows %v, want only the second attempt's", rows)
	}
}

// A real lock timeout: the row is held by another transaction for the
// first attempt and released before the retry.
func TestTxRetriesLockTimeout(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, "CREATE TABLE t (n int PRIMARY KEY); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	holder, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, "SELECT n FROM t WHERE n = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	err = d.Tx(ctx, func(tx pgx.Tx) error {
		attempts++
		if attempts == 2 {
			if err := holder.Rollback(ctx); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '100ms'"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE t SET n = 2 WHERE n = 1")
		return err
	})
	if err != nil || attempts < 2 {
		t.Fatalf("err %v after %d attempts", err, attempts)
	}
}

func waitPayload(t *testing.T, ch <-chan string, want string, within time.Duration) {
	t.Helper()
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		select {
		case p := <-ch:
			if p == want {
				return
			}
		case <-timer.C:
			t.Fatalf("no payload %q within %s", want, within)
		}
	}
}

func TestHubDelivers(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx := context.Background()
	ch, unsubscribe := d.Subscribe(db.ChanSandbox)
	defer unsubscribe()
	other, unsubscribeOther := d.Subscribe(db.ChanNode)
	defer unsubscribeOther()

	if err := db.Notify(ctx, d.Pool, db.ChanSandbox, "a"); err != nil {
		t.Fatal(err)
	}
	waitPayload(t, ch, "a", 5*time.Second)

	// Inside a transaction: delivered at commit, never after a rollback.
	if err := d.Tx(ctx, func(tx pgx.Tx) error {
		return db.Notify(ctx, tx, db.ChanSandbox, "committed")
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Notify(ctx, tx, db.ChanSandbox, "never"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Notify(ctx, d.Pool, db.ChanSandbox, "b"); err != nil {
		t.Fatal(err)
	}
	var got []string
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for len(got) == 0 || got[len(got)-1] != "b" {
		select {
		case p := <-ch:
			got = append(got, p)
		case <-timer.C:
			t.Fatalf("got %v, no b", got)
		}
	}
	for _, p := range got {
		if p == "never" {
			t.Fatalf("a rolled back notification arrived: %v", got)
		}
	}
	if got[0] != "committed" {
		t.Fatalf("got %v, want the committed transaction's payload first", got)
	}
	if len(other) != 0 {
		t.Fatalf("another channel's subscriber got %q", <-other)
	}
	if !d.LastFailure().IsZero() {
		t.Fatalf("LastFailure %v without a failure", d.LastFailure())
	}
}

// After the listener's connection is cut, every subscriber gets "", and the
// listener listens again.
func TestHubReconnect(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx := context.Background()
	ch, unsubscribe := d.Subscribe(db.ChanWatch)
	defer unsubscribe()
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE datname = current_database() AND application_name = 'ferund-listener'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("terminated %d listeners, want 1", n)
	}
	waitPayload(t, ch, "", 20*time.Second)
	if d.LastFailure().IsZero() {
		t.Fatal("the lost listener connection is not recorded as a failure")
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.WaitListening(wctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Notify(ctx, d.Pool, db.ChanWatch, "w1"); err != nil {
		t.Fatal(err)
	}
	waitPayload(t, ch, "w1", 5*time.Second)
}

// WaitFor never misses a notification sent between its check and its wait:
// here the notification arrives while the first check runs, and the timer
// is far away, so only the subscription can end the wait.
func TestWaitForNeverMisses(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checks := 0
	err := d.WaitFor(ctx, db.ChanSandbox, "s1", time.Hour, func(ctx context.Context) (bool, error) {
		checks++
		if checks == 1 {
			if err := db.Notify(ctx, d.Pool, db.ChanSandbox, "s1"); err != nil {
				return false, err
			}
			// Let the notification reach the subscription before the
			// wait begins.
			time.Sleep(300 * time.Millisecond)
			return false, nil
		}
		return true, nil
	})
	if err != nil || checks != 2 {
		t.Fatalf("err %v after %d checks", err, checks)
	}
}

func TestWaitForKeys(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx := context.Background()

	// Payloads for other keys do not end the wait; "*" does.
	var checks atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- d.WaitFor(ctx, db.ChanSandbox, "mine", time.Hour, func(context.Context) (bool, error) {
			return checks.Add(1) >= 2, nil
		})
	}()
	for checks.Load() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if err := db.Notify(ctx, d.Pool, db.ChanSandbox, "other"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("returned on another key's payload: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := db.Notify(ctx, d.Pool, db.ChanSandbox, "*"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal(`"*" did not end the wait`)
	}

	// The timer ends a wait without notifications.
	checks.Store(0)
	start := time.Now()
	if err := d.WaitFor(ctx, db.ChanSandbox, "quiet", 50*time.Millisecond, func(context.Context) (bool, error) {
		return checks.Add(1) >= 3, nil
	}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the poll timer did not fire")
	}

	// A check's error and the context's end are returned.
	boom := errors.New("boom")
	if err := d.WaitFor(ctx, db.ChanSandbox, "x", 0, func(context.Context) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Fatalf("err %v, want boom", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := d.WaitFor(cctx, db.ChanSandbox, "x", time.Hour, func(context.Context) (bool, error) { return false, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want the deadline", err)
	}
}

func TestPartitions(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	ctx := context.Background()
	var now time.Time
	if err := d.Pool.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -5)
	to := today.AddDate(0, 0, 2)

	// Two instances ensuring the same days at once.
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = d.EnsurePartitions(ctx, from, to)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range db.PartitionedTables {
		days, err := d.PartitionDays(ctx, table)
		if err != nil {
			t.Fatal(err)
		}
		if len(days) != 8 || !days[0].Equal(from) || !days[7].Equal(to) {
			t.Fatalf("%s: days %v, want %s to %s", table, days, from, to)
		}
	}
	// A row of an old day lands in its partition.
	if _, err := d.Pool.Exec(ctx, "INSERT INTO activity (day, ts_ns, source, kind) VALUES ($1, 1, 'ferund', 'old')", from); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO traffic_records (day, ts_ns, node, start_id, seq, kind, detail)
		VALUES ($1, 1, 'n', gen_random_uuid(), 1, 'flow', '{}')`, from); err != nil {
		t.Fatal(err)
	}

	// Drop what lies wholly before three days ago: the partitions of five
	// and four days ago, not the one of three days ago.
	dropped, err := d.DropPartitions(ctx, db.TableActivity, today.AddDate(0, 0, -3))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{db.PartitionName(db.TableActivity, from), db.PartitionName(db.TableActivity, from.AddDate(0, 0, 1))}
	if strings.Join(dropped, ",") != strings.Join(want, ",") {
		t.Fatalf("dropped %v, want %v", dropped, want)
	}
	var n int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM activity WHERE kind = 'old'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows of a dropped partition remain", n)
	}
	// The other table is untouched, and dropping again drops nothing.
	if days, _ := d.PartitionDays(ctx, db.TableTraffic); len(days) != 8 {
		t.Fatalf("traffic partitions %v", days)
	}
	if dropped, err := d.DropPartitions(ctx, db.TableActivity, today.AddDate(0, 0, -3)); err != nil || len(dropped) != 0 {
		t.Fatalf("second drop: %v %v", dropped, err)
	}
	// Today's partition never goes while its day lasts.
	if dropped, err := d.DropPartitions(ctx, db.TableActivity, now); err != nil || len(dropped) != 3 {
		t.Fatalf("drop before now: %v %v", dropped, err)
	}
	if _, err := d.Pool.Exec(ctx, "INSERT INTO activity (ts_ns, source, kind) VALUES (1, 'ferund', 'today')"); err != nil {
		t.Fatalf("insert today: %v", err)
	}
	if _, err := d.PartitionDays(ctx, "sandboxes"); err == nil {
		t.Fatal("PartitionDays accepted an unpartitioned table")
	}
}

// A connection that cannot be made is recorded as a failure.
func TestLastFailureOnConnect(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := db.Open(ctx, "postgres://ferun@127.0.0.1:1/ferun?sslmode=disable&connect_timeout=2", dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !d.LastFailure().IsZero() {
		t.Fatal("a failure before any connection")
	}
	if err := d.Pool.Ping(ctx); err == nil {
		t.Fatal("ping of a closed port succeeded")
	}
	if d.LastFailure().IsZero() {
		t.Fatal("the failed connect is not recorded")
	}
}
