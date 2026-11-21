// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package activity_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

var bg = context.Background()

func count(t *testing.T, d *db.DB, where string, args ...any) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(bg, "SELECT count(*) FROM activity WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitCount(t *testing.T, d *db.DB, want int, where string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := count(t, d, where, args...)
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d rows where %s, want %d", n, where, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func run(t *testing.T, r *activity.Recorder) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return stop
}

// The recorder writes rows in batches from many goroutines; every row is
// ferund's, of the instance, with its name normalised, its output capped
// and its category filled in.
func TestRecorderBatches(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	r := activity.NewRecorder(store.New(d), "i1", dbtest.Logger(t))
	run(t, r)
	sb := podid.Mint()
	var wg sync.WaitGroup
	for g := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				r.Record(activity.Event{SandboxID: sb, ProjectID: "acme", Kind: activity.KindToolStart,
					SpanID: activity.NewSpanID("call"), Name: "bash:   ls\n -la", Tool: "bash",
					Params: json.RawMessage(`{"command":"ls"}`), Detail: map[string]any{"g": g, "i": i}})
			}
		}()
	}
	wg.Wait()
	waitCount(t, d, 1000, "sandbox_id = $1", sb)
	if r.Dropped() != 0 {
		t.Fatalf("dropped %d", r.Dropped())
	}
	rows, err := store.New(d).ListSandboxActivity(bg, sb, store.ActivityQuery{Limit: 1})
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	row := rows[0]
	if row.Source != store.SourceFerund || row.InstanceID != "i1" || row.Name != "bash: ls -la" || row.Category != activity.CategoryTool ||
		row.ProjectID != "acme" || row.TsNs == 0 || !strings.HasPrefix(row.SpanID, "call-") || string(row.Params) != `{"command": "ls"}` {
		t.Fatalf("row %+v", row)
	}
}

// Recording never blocks: with nobody flushing, the queue fills and the
// rest is dropped and counted; a flusher started later writes the queue.
func TestRecordNeverBlocks(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	r := activity.NewRecorder(store.New(d), "i1", dbtest.Logger(t))
	start := time.Now()
	const queued, extra = 4096, 100
	for range queued + extra {
		r.Record(activity.Event{Kind: activity.KindSandboxState, Name: "x"})
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("recording took %s", took)
	}
	if r.Dropped() != extra {
		t.Fatalf("dropped %d, want %d", r.Dropped(), extra)
	}
	run(t, r)
	waitCount(t, d, queued, "kind = $1", activity.KindSandboxState)
}

// A stopping flusher writes what is queued before it returns.
func TestRunFlushesOnStop(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	r := activity.NewRecorder(store.New(d), "i1", dbtest.Logger(t))
	for range 50 {
		r.Record(activity.Event{Kind: activity.KindRunEnd})
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, "kind = $1", activity.KindRunEnd); n != 50 {
		t.Fatalf("%d rows after stop, want 50", n)
	}
}

// Rows the database refuses are tried again, then dropped and counted.
func TestRecorderDropsWhatCannotBeStored(t *testing.T) {
	t.Parallel()
	other := dbtest.Open(t)
	other.Close()
	r := activity.NewRecorder(store.New(other), "i1", dbtest.Logger(t))
	run(t, r)
	for range 10 {
		r.Record(activity.Event{Kind: activity.KindTombstone})
	}
	deadline := time.Now().Add(10 * time.Second)
	for r.Dropped() != 10 {
		if time.Now().After(deadline) {
			t.Fatalf("dropped %d, want 10", r.Dropped())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// RecordTx commits and rolls back with the caller's transaction.
func TestRecordTx(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	r := activity.NewRecorder(store.New(d), "i1", dbtest.Logger(t))
	sb := podid.Mint()
	tx, err := d.Pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordTx(bg, tx, activity.Event{SandboxID: sb, Kind: activity.KindRunStart}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(bg); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, "sandbox_id = $1", sb); n != 0 {
		t.Fatal("a rolled back row is stored")
	}
	exit, dur := int64(2), int64(1500)
	if err := d.Tx(bg, func(tx pgx.Tx) error {
		return r.RecordTx(bg, tx, activity.Event{SandboxID: sb, Kind: activity.KindToolEnd, ExitCode: &exit, DurationNs: &dur,
			Output: "a\x00b", Params: json.RawMessage(`not json`), Detail: map[string]any{"error": "x\x00y"}})
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.New(d).ListSandboxActivity(bg, sb, store.ActivityQuery{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	row := rows[0]
	if *row.ExitCode != 2 || *row.DurationNs != 1500 || row.Output != "ab" || string(row.Params) != `"not json"` ||
		string(row.Detail) != `{"error": "xy"}` || row.Category != activity.CategoryTool {
		t.Fatalf("row %+v", row)
	}
}

func TestName(t *testing.T) {
	if got := activity.Name("  bash:\tls\n-la  "); got != "bash: ls -la" {
		t.Fatalf("%q", got)
	}
	long := strings.Repeat("ä", 100) // 200 bytes
	got := activity.Name(long)
	if len(got) > activity.NameCap || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("%q (%d bytes)", got, len(got))
	}
	if got := activity.Name("a\x00b\xff"); got != "ab�" {
		t.Fatalf("%q", got)
	}
}

func TestCapOutput(t *testing.T) {
	s := strings.Repeat("x", activity.OutputCap-1) + "ü" // one byte over
	got, cut := activity.CapOutput(s)
	if !cut || len(got) != activity.OutputCap-1 || !utf8.ValidString(got) {
		t.Fatalf("cut %v, %d bytes", cut, len(got))
	}
	if got, cut := activity.CapOutput("short"); cut || got != "short" {
		t.Fatal("short output cut")
	}
}

func TestCleanJSON(t *testing.T) {
	long := strings.Repeat("1", 1001)
	for in, want := range map[string]string{
		``:                                ``,
		`{"a":1}`:                         `{"a":1}`,
		`{"a":"x\u0000y"}`:                `{"a":"xy"}`,
		`["\u0000",{"k\u0000":1}]`:        `["",{"k":1}]`,
		`{bad`:                            `"{bad"`,
		`"a\\u0000b"`:                     `"a\\u0000b"`,
		`{"a":"\ud800"}`:                  "{\"a\":\"\uFFFD\"}",
		`["\udc00x","\ud83d\ude00"]`:      "[\"\uFFFDx\",\"\U0001F600\"]",
		"\"\xff\"":                        "\"\uFFFD\"",
		"{\"k\xff\":1}":                   "{\"k\uFFFD\":1}",
		`[1e999999,1e-20000,1.5,-2,1E+2]`: `["1e999999","1e-20000",1.5,-2,1E+2]`,
		`{"n":1e9999,"m":-0.5e-9999}`:     `{"n":1e9999,"m":-0.5e-9999}`,
		long:                              `"` + long + `"`,
		`{"s":"1e999999"}`:                `{"s":"1e999999"}`,
		`"x\\ud800"`:                      `"x\\ud800"`,
	} {
		if got := string(activity.CleanJSON([]byte(in))); got != want {
			t.Errorf("CleanJSON(%s) = %s, want %s", in, got, want)
		}
	}
}

// What CleanJSON returns, jsonb takes: every document Go's decoder accepts
// but PostgreSQL refuses is made storable.
func TestCleanJSONIsStorable(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	for _, in := range []string{
		`{"a":"\ud800"}`,
		`{"a":"\udc00"}`,
		`"\ud800a"`,
		`{"\ud800":1}`,
		`1e999999`,
		`1e-999999`,
		`[1e-16384]`,
		`{"x":1e131072}`,
		"0." + strings.Repeat("1", 16384),
		strings.Repeat("1", 131073),
		"\"\xff\"",
		"{\"\xfe\":\"\xff\"}",
		`{"k\u0000":"\u0000"}`,
		strings.Repeat("[", 10000) + strings.Repeat("]", 10000),
		strings.Repeat("[", 10001) + strings.Repeat("]", 10001),
		"{bad\x00\xff",
		`{"a":[{"b":"\ud800","c":1e-99999,"d":"\u0000"}]}`,
	} {
		out := activity.CleanJSON([]byte(in))
		if _, err := d.Pool.Exec(bg, "SELECT $1::text::jsonb", string(out)); err != nil {
			t.Errorf("CleanJSON(%.40q) = %.60q, which jsonb refuses: %v", in, out, err)
		}
	}
}

// A tool call's arguments that Go accepts but jsonb would refuse neither
// cost the batch they are recorded in nor abort the caller's transaction.
func TestRecordRefusedJSON(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	r := activity.NewRecorder(store.New(d), "i1", dbtest.Logger(t))
	run(t, r)
	sb := podid.Mint()
	for i := range 20 {
		params := json.RawMessage(`{"command":"ls"}`)
		if i == 7 {
			params = json.RawMessage(`{"a":"\ud800"}`)
		}
		r.Record(activity.Event{SandboxID: sb, Kind: activity.KindToolStart, Params: params})
	}
	waitCount(t, d, 20, "sandbox_id = $1", sb)
	if r.Dropped() != 0 {
		t.Fatalf("dropped %d", r.Dropped())
	}

	other := podid.Mint()
	if err := d.Tx(bg, func(tx pgx.Tx) error {
		if err := r.RecordTx(bg, tx, activity.Event{SandboxID: other, Kind: activity.KindToolEnd,
			Params: json.RawMessage(`{"a":"\ud800"}`)}); err != nil {
			return err
		}
		_, err := tx.Exec(bg, "SELECT 1")
		return err
	}); err != nil {
		t.Fatalf("RecordTx: %v", err)
	}
	rows, err := store.New(d).ListSandboxActivity(bg, other, store.ActivityQuery{Limit: 1})
	if err != nil || len(rows) != 1 || string(rows[0].Params) != "{\"a\": \"\ufffd\"}" {
		t.Fatalf("rows %+v %v", rows, err)
	}
}

func TestCategoryAndSpans(t *testing.T) {
	if activity.CategoryOf(activity.KindToolEnd) != activity.CategoryTool ||
		activity.CategoryOf(activity.KindRunStart) != activity.CategoryLifecycle ||
		activity.CategoryOf(activity.KindPodCredentials) != activity.CategoryCredential {
		t.Fatal("categories")
	}
	a, b := activity.NewSpanID("run"), activity.NewSpanID("run")
	if a == b || !strings.HasPrefix(a, "run-") || len(a) != len("run-")+36 {
		t.Fatalf("span ids %q %q", a, b)
	}
	if _, err := uuid.Parse(activity.NewSpanID("")); err != nil {
		t.Fatal(err)
	}
}
