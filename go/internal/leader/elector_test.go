// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package leader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/store"
)

var bg = context.Background()

// testElector returns an Elector with shortened timings: TTL 3 s, renew
// every 300 ms, read every 100 ms, margin 1 s (step-down 2 s after a
// renew's send).
func testElector(t *testing.T, d *db.DB, id string) *Elector {
	t.Helper()
	e := NewElector(d, id, dbtest.Logger(t))
	e.ttl, e.renewEvery, e.readEvery, e.margin = 3*time.Second, 300*time.Millisecond, 100*time.Millisecond, time.Second
	return e
}

// start runs e until the returned stop is called (or the test ends).
func start(t *testing.T, e *Elector) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := e.Run(ctx); err != nil {
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

func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readLease(t *testing.T, d *db.DB) store.LeaseRow {
	t.Helper()
	l, found, err := store.New(d).ReadLease(bg, store.LeaderLease)
	if err != nil || !found {
		t.Fatalf("read lease: found %v, %v", found, err)
	}
	return l
}

func TestAcquireRenewExpiry(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	a := testElector(t, d, "a")
	b := testElector(t, d, "b")
	if !a.LeadingSince().IsZero() || a.IsLeader() {
		t.Fatal("leading before Run")
	}
	stopA := start(t, a)
	waitFor(t, "a to lead", 5*time.Second, a.IsLeader)
	if a.LeadingSince().IsZero() {
		t.Fatal("LeadingSince is zero while leading")
	}
	start(t, b)
	first := readLease(t, d)
	if first.Holder != "a" || first.Epoch != 1 {
		t.Fatalf("lease %+v", first)
	}
	holder, epoch, since, err := a.Leader(bg)
	if err != nil || holder != "a" || epoch != 1 || !since.Equal(first.AcquiredAt) {
		t.Fatalf("Leader() = %q %d %v %v", holder, epoch, since, err)
	}
	// a renews: the lease's expiry moves on, b never leads.
	waitFor(t, "a renewal", 5*time.Second, func() bool { return readLease(t, d).RenewedAt.After(first.RenewedAt) })
	time.Sleep(time.Second)
	if b.IsLeader() {
		t.Fatal("b leads while a's lease is valid")
	}
	if l := readLease(t, d); l.Holder != "a" || l.Epoch != 1 || !l.ExpiresAt.After(first.ExpiresAt) {
		t.Fatalf("lease after renewals %+v (first %+v)", l, first)
	}

	// a dies without releasing: b takes over once the lease expired, with
	// the next epoch.
	stopA()
	died := time.Now()
	waitFor(t, "b to lead", 10*time.Second, b.IsLeader)
	if took := time.Since(died); took < time.Second {
		t.Fatalf("b took over after %v, before a's lease expired", took)
	}
	if l := readLease(t, d); l.Holder != "b" || l.Epoch != 2 {
		t.Fatalf("lease after the takeover %+v", l)
	}
	if a.IsLeader() || !a.LeadingSince().IsZero() {
		t.Fatal("a still leads after its Run ended")
	}
}

func TestReacquireBumpsEpoch(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	a := testElector(t, d, "a")
	start(t, a)
	waitFor(t, "a to lead", 5*time.Second, a.IsLeader)
	since1 := a.LeadingSince()

	// The lease expires under a (a database failover, a paused process):
	// the next renew finds no row and a steps down at once; then a
	// acquires again, with the next epoch, so that work of the first term
	// fails the fence.
	var fenced pgx.Tx
	fenced, err := d.Pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer fenced.Rollback(bg)
	if err := a.Fence(bg, fenced); err != nil {
		t.Fatalf("fence in the first term: %v", err)
	}
	if err := fenced.Rollback(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(bg, "UPDATE leases SET expires_at = now() - interval '1 second' WHERE name = 'leader'"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a to step down", 5*time.Second, func() bool { return !a.IsLeader() })
	waitFor(t, "a to lead again", 5*time.Second, a.IsLeader)
	if l := readLease(t, d); l.Holder != "a" || l.Epoch != 2 {
		t.Fatalf("lease after re-acquisition %+v", l)
	}
	if !a.LeadingSince().After(since1) {
		t.Fatalf("LeadingSince %v not after the first term's %v", a.LeadingSince(), since1)
	}
	// A transaction of the first term's epoch fails the fence.
	ok, err := store.New(d).FenceLease(bg, store.LeaderLease, "a", 1)
	if err != nil || ok {
		t.Fatalf("fence with the first epoch: %v %v", ok, err)
	}
	if err := d.Tx(bg, func(tx pgx.Tx) error { return a.Fence(bg, tx) }); err != nil {
		t.Fatalf("fence in the second term: %v", err)
	}
}

func TestStepDownDeadline(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	a := testElector(t, d, "a")
	loopEnded := make(chan time.Time, 1)
	a.Register(Loop{Name: "blocker", Interval: time.Hour, Run: func(ctx context.Context) error {
		<-ctx.Done()
		loopEnded <- time.Now()
		return ctx.Err()
	}})
	start(t, a)
	waitFor(t, "a to lead", 5*time.Second, a.IsLeader)
	// Let a renew at least once, then hold the lease row: the renews
	// block on the row lock (their lock_timeout is 3 s), and a must step
	// down at its deadline, 2 s after the last successful renew's send,
	// without waiting for the renew to fail.
	first := readLease(t, d)
	waitFor(t, "a renewal", 5*time.Second, func() bool { return readLease(t, d).RenewedAt.After(first.RenewedAt) })
	tx, err := d.Pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(bg)
	if _, err := tx.Exec(bg, "SELECT 1 FROM leases WHERE name = 'leader' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	locked := time.Now()
	waitFor(t, "a to step down", 5*time.Second, func() bool { return !a.IsLeader() })
	took := time.Since(locked)
	if took < 1300*time.Millisecond || took > 2600*time.Millisecond {
		t.Fatalf("stepped down %v after the lease row was locked; want the deadline, 1.5–2 s", took)
	}
	select {
	case at := <-loopEnded:
		if at.Sub(locked) > 2700*time.Millisecond {
			t.Fatalf("the loop's context ended %v after the lock", at.Sub(locked))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop's context did not end at the step-down")
	}
	if err := a.Fence(bg, d.Pool); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("Fence after the step-down: %v", err)
	}
	if err := tx.Rollback(bg); err != nil {
		t.Fatal(err)
	}
	// Once its own lease expired, a leads again with the next epoch.
	waitFor(t, "a to lead again", 10*time.Second, a.IsLeader)
	if l := readLease(t, d); l.Epoch != 2 {
		t.Fatalf("epoch after the step-down %d", l.Epoch)
	}
}

func TestFence(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	a := testElector(t, d, "a")
	if err := a.Fence(bg, d.Pool); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("Fence while not leading: %v", err)
	}
	start(t, a)
	waitFor(t, "a to lead", 5*time.Second, a.IsLeader)
	if err := d.Tx(bg, func(tx pgx.Tx) error { return a.Fence(bg, tx) }); err != nil {
		t.Fatalf("Fence while leading: %v", err)
	}
	// Another instance holds the lease now (a's term has not noticed yet):
	// the fence fails and the transaction aborts.
	if _, err := d.Pool.Exec(bg, "UPDATE leases SET holder = 'b', epoch = epoch + 1 WHERE name = 'leader'"); err != nil {
		t.Fatal(err)
	}
	wrote := false
	err := d.Tx(bg, func(tx pgx.Tx) error {
		if err := a.Fence(bg, tx); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	if !errors.Is(err, ErrNotLeader) || wrote {
		t.Fatalf("Fence after losing the lease: %v (wrote %v)", err, wrote)
	}
	waitFor(t, "a to step down", 5*time.Second, func() bool { return !a.IsLeader() })
}

// counter is a loop that counts its runs and their concurrency.
type counter struct {
	runs, active, maxActive atomic.Int32
	delay                   time.Duration
	err                     error
	panics                  bool
}

func (c *counter) run(ctx context.Context) error {
	n := c.active.Add(1)
	defer c.active.Add(-1)
	for {
		m := c.maxActive.Load()
		if n <= m || c.maxActive.CompareAndSwap(m, n) {
			break
		}
	}
	c.runs.Add(1)
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
		}
	}
	if c.panics {
		panic("loop bug")
	}
	return c.err
}

func notify(t *testing.T, d *db.DB, payload string) {
	t.Helper()
	if err := db.Notify(bg, d.Pool, db.ChanLeader, payload); err != nil {
		t.Fatal(err)
	}
}

func TestLoopsAndWakeups(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	a := testElector(t, d, "a")
	alpha, beta, slow := &counter{}, &counter{}, &counter{delay: 300 * time.Millisecond}
	failing := &counter{err: errors.New("database unhappy")}
	panicking := &counter{panics: true}
	a.Register(
		Loop{Name: "alpha-sweep", Interval: time.Hour, Run: alpha.run},
		Loop{Name: "beta", Interval: time.Hour, Run: beta.run},
		Loop{Name: "slow", Interval: time.Hour, Run: slow.run},
		Loop{Name: "failing", Interval: 50 * time.Millisecond, Run: failing.run},
		Loop{Name: "panicking", Interval: 50 * time.Millisecond, Run: panicking.run},
	)
	start(t, a)
	waitFor(t, "a to lead", 5*time.Second, a.IsLeader)
	// Every loop runs once at the start of the term.
	waitFor(t, "the first runs", 5*time.Second, func() bool {
		return alpha.runs.Load() == 1 && beta.runs.Load() == 1 && slow.runs.Load() == 1
	})

	// A hint wakes the loops whose name holds it.
	notify(t, d, "alpha")
	waitFor(t, "alpha woken", 5*time.Second, func() bool { return alpha.runs.Load() == 2 })
	time.Sleep(200 * time.Millisecond)
	if beta.runs.Load() != 1 {
		t.Fatalf("beta ran %d times; the hint was alpha's", beta.runs.Load())
	}
	// A hint no loop's name holds, and "", wake every loop.
	notify(t, d, "project")
	waitFor(t, "every loop woken", 5*time.Second, func() bool { return alpha.runs.Load() == 3 && beta.runs.Load() == 2 })
	notify(t, d, "")
	waitFor(t, "every loop woken by \"\"", 5*time.Second, func() bool { return alpha.runs.Load() == 4 && beta.runs.Load() == 3 })

	// Wakes while a loop runs are coalesced: one run at a time, and one
	// more run after it covers all of them.
	waitFor(t, "slow idle", 5*time.Second, func() bool { return slow.active.Load() == 0 })
	before := slow.runs.Load()
	for range 10 {
		notify(t, d, "slow")
	}
	time.Sleep(1500 * time.Millisecond)
	if got := slow.runs.Load() - before; got < 1 || got > 3 {
		t.Fatalf("slow ran %d times for 10 wakes; want them coalesced", got)
	}
	if slow.maxActive.Load() != 1 {
		t.Fatalf("slow ran %d times at once", slow.maxActive.Load())
	}

	// Errors and panics are logged, never fatal: the loops keep their
	// schedule.
	waitFor(t, "failing and panicking loops to keep running", 5*time.Second, func() bool {
		return failing.runs.Load() >= 5 && panicking.runs.Load() >= 5
	})
	if !a.IsLeader() {
		t.Fatal("a stopped leading over failing loops")
	}

	// After Resign no loop runs any more.
	if err := a.Resign(bg); err != nil {
		t.Fatal(err)
	}
	stopped := alpha.runs.Load() + failing.runs.Load()
	notify(t, d, "alpha")
	time.Sleep(300 * time.Millisecond)
	if alpha.runs.Load()+failing.runs.Load() != stopped {
		t.Fatal("loops ran after Resign")
	}
}

func TestResign(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	a := testElector(t, d, "a")
	if err := a.Resign(bg); err != nil {
		t.Fatalf("Resign while not leading: %v", err)
	}
	a2 := testElector(t, d, "a2")
	start(t, a2)
	waitFor(t, "a2 to lead", 5*time.Second, a2.IsLeader)
	running := make(chan struct{})
	a2.Register(Loop{Name: "late", Interval: time.Hour, Run: func(context.Context) error { close(running); return nil }})
	if err := a2.Resign(bg); err != nil {
		t.Fatal(err)
	}
	if a2.IsLeader() || !a2.LeadingSince().IsZero() {
		t.Fatal("a2 leads after Resign")
	}
	if holder, _, _, err := a2.Leader(bg); err != nil || holder != "" {
		t.Fatalf("lease after Resign: holder %q, %v", holder, err)
	}
	// The lease expired at once: another instance takes over at its next
	// read, with the next epoch; the resigned one never competes again.
	b := testElector(t, d, "b")
	start(t, b)
	waitFor(t, "b to lead", 2*time.Second, b.IsLeader)
	if l := readLease(t, d); l.Holder != "b" || l.Epoch != 2 {
		t.Fatalf("lease %+v", l)
	}
	if err := b.Resign(bg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if a2.IsLeader() || b.IsLeader() {
		t.Fatal("a resigned elector leads again")
	}
	if holder, _, _, _ := a2.Leader(bg); holder != "" {
		t.Fatalf("lease taken by %q after both resigned", holder)
	}
	select {
	case <-running:
		t.Fatal("a loop registered during the term ran")
	default:
	}
}

// TestResignAfterRunEnded covers an elector whose Run context ended before
// Resign (a context that ends at SIGTERM): the term is over, but its lease
// is still valid, and Resign releases it all the same.
func TestResignAfterRunEnded(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	a := testElector(t, d, "a")
	a.ttl, a.margin = time.Minute, 2*time.Second
	stop := start(t, a)
	waitFor(t, "a to lead", 5*time.Second, a.IsLeader)
	stop()
	if a.IsLeader() {
		t.Fatal("a leads after Run ended")
	}
	if holder, _, _, err := a.Leader(bg); err != nil || holder != "a" {
		t.Fatalf("lease after Run ended: holder %q, %v; want a's, valid for its TTL", holder, err)
	}
	if err := a.Resign(bg); err != nil {
		t.Fatal(err)
	}
	if holder, _, _, err := a.Leader(bg); err != nil || holder != "" {
		t.Fatalf("lease after Resign: holder %q, %v", holder, err)
	}
	b := testElector(t, d, "b")
	start(t, b)
	waitFor(t, "b to lead", 2*time.Second, b.IsLeader)

	// Resigning again, or after another instance took the lease, leaves
	// that instance's lease alone: the release is guarded by the epoch.
	if err := a.Resign(bg); err != nil {
		t.Fatal(err)
	}
	if l := readLease(t, d); l.Holder != "b" || l.Epoch != 2 || !l.Valid {
		t.Fatalf("lease %+v", l)
	}
}

func TestStaleInstancesLoop(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	a := testElector(t, d, "a")
	st := store.New(d)
	for _, id := range []string{"fresh", "stale"} {
		if err := st.UpsertInstance(bg, store.InstanceInfo{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Pool.Exec(bg, "UPDATE instances SET last_seen = now() - interval '61 minutes' WHERE id = 'stale'"); err != nil {
		t.Fatal(err)
	}
	if err := a.deleteStaleInstances(bg); err != nil {
		t.Fatal(err)
	}
	ins, err := st.ListInstances(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].ID != "fresh" {
		t.Fatalf("instances after the loop %+v", ins)
	}
	found := false
	for _, l := range a.loops {
		found = found || (l.Name == LoopStaleInstances && l.Interval == time.Minute)
	}
	if !found {
		t.Fatal("the stale-instances loop is not registered")
	}
}

func TestWoken(t *testing.T) {
	loops := []Loop{{Name: "down-sweep"}, {Name: "tombstones"}, {Name: "drain"}}
	for _, tc := range []struct {
		hint string
		want []int
	}{
		{"tombstone", []int{1}},
		{"drain", []int{2}},
		{"down", []int{0}},
		{"project", []int{0, 1, 2}},
		{"", []int{0, 1, 2}},
		{"*", []int{0, 1, 2}},
	} {
		got := woken(loops, tc.hint)
		if len(got) != len(tc.want) {
			t.Fatalf("woken(%q) = %v, want %v", tc.hint, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("woken(%q) = %v, want %v", tc.hint, got, tc.want)
			}
		}
	}
}

// lockedBuffer is a log sink the elector's goroutines write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The elector's lines name the instance once: the logger it is given is
// the instance's, which carries it already.
func TestLogNamesInstanceOnce(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	var out lockedBuffer
	log := slog.New(slog.NewJSONHandler(&out, nil)).With("instance", "a")
	e := NewElector(d, "a", log)
	e.ttl, e.renewEvery, e.readEvery, e.margin = 3*time.Second, 300*time.Millisecond, 100*time.Millisecond, time.Second
	stop := start(t, e)
	waitFor(t, "a to lead", 5*time.Second, e.IsLeader)
	if err := e.Resign(bg); err != nil {
		t.Fatal(err)
	}
	stop()

	seen := map[string]bool{}
	for line := range strings.Lines(out.String()) {
		if n := strings.Count(line, `"instance":`); n != 1 {
			t.Errorf("%d instance attributes in %s", n, line)
		}
		var rec struct{ Msg string }
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		seen[rec.Msg] = true
	}
	for _, msg := range []string{"leading", "stepped down", "released the leader lease"} {
		if !seen[msg] {
			t.Errorf("no line %q in\n%s", msg, out.String())
		}
	}
}
