// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

func argsHash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// newCall is a bash call of key by instance, its arguments hashed from the
// key.
func newCall(id podid.ID, key, instance string) store.NewToolCall {
	return store.NewToolCall{SandboxID: id, Key: key, Tool: "bash", ArgsHash: argsHash(key), InstanceID: instance,
		Lease: 30 * time.Second}
}

// beginCall records a new running call of key by instance.
func beginCall(t *testing.T, s *store.Store, id podid.ID, key, instance string) store.ToolCall {
	t.Helper()
	c, inserted, err := s.BeginToolCall(bg, newCall(id, key, instance))
	if err != nil || !inserted {
		t.Fatalf("begin %s: %+v inserted %v: %v", key, c, inserted, err)
	}
	return c
}

func getCall(t *testing.T, s *store.Store, id podid.ID, key string) store.ToolCall {
	t.Helper()
	c, err := s.GetToolCall(bg, id, key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return c
}

func markSent(t *testing.T, s *store.Store, c store.ToolCall) {
	t.Helper()
	if ok, err := s.MarkToolCallSent(bg, c.ID); err != nil || !ok {
		t.Fatalf("mark %s sent: %v %v", c.Key, ok, err)
	}
}

func finishCall(t *testing.T, s *store.Store, c store.ToolCall, state string) {
	t.Helper()
	if ok, err := s.FinishToolCall(bg, c.ID, store.ToolCallOutcome{State: state}); err != nil || !ok {
		t.Fatalf("finish %s as %s: %v %v", c.Key, state, ok, err)
	}
}

// callSandbox makes the project acme and a sandbox of it.
func callSandbox(t *testing.T, s *store.Store) podid.ID {
	t.Helper()
	mkProject(t, s, "acme")
	return mkSandbox(t, s, "acme").ID
}

// expectPayloads waits until every payload of want arrived on ch, skipping
// others.
func expectPayloads(t *testing.T, ch <-chan string, want ...string) {
	t.Helper()
	missing := map[string]bool{}
	for _, w := range want {
		missing[w] = true
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for len(missing) > 0 {
		select {
		case p := <-ch:
			delete(missing, p)
		case <-timer.C:
			t.Fatalf("no notifications %v", missing)
		}
	}
}

// expectEnded checks that the call ended in state with the error text.
func expectEnded(t *testing.T, s *store.Store, c store.ToolCall, state, text string) {
	t.Helper()
	r := getCall(t, s, c.SandboxID, c.Key)
	if r.ID != c.ID || r.State != state || r.FinishedAt.IsZero() || r.Error != text {
		t.Fatalf("%s: %+v, want %s with %q", c.Key, r, state, text)
	}
}

func expectRunning(t *testing.T, s *store.Store, c store.ToolCall) {
	t.Helper()
	if r := getCall(t, s, c.SandboxID, c.Key); r.ID != c.ID || r.State != store.ToolCallRunning || !r.FinishedAt.IsZero() {
		t.Fatalf("%s is not running: %+v", c.Key, r)
	}
}

func sameIDs(got []uuid.UUID, want ...uuid.UUID) bool {
	a := slices.Clone(got)
	b := slices.Clone(want)
	cmp := func(x, y uuid.UUID) int { return bytes.Compare(x[:], y[:]) }
	slices.SortFunc(a, cmp)
	slices.SortFunc(b, cmp)
	return slices.Equal(a, b)
}

// The texts of the writers that end another run's call.
func expiredSent(instance string) string {
	return "interrupted: instance " + instance + " that ran the call stopped renewing it before the call ended (it " +
		"crashed, or lost its database); what the call did in the sandbox is unknown: look before repeating it"
}

func expiredUnsent(instance string) string {
	return "not run: instance " + instance + " that took the call stopped renewing it before the call reached the " +
		"sandbox; repeat it with the same id"
}

func claimedSent(instance string) string {
	return "interrupted: instance " + instance + " started again; its earlier run had the call and ended without its " +
		"stop; what the call did in the sandbox is unknown: look before repeating it"
}

func claimedUnsent(instance string) string {
	return "not run: instance " + instance + " started again before the call reached the sandbox; repeat it with the " +
		"same id"
}

// A key's first Begin records a running call; another Begin of the key, by
// any instance, returns that row unchanged. Of two concurrent Begins one
// inserts. A sandbox that does not exist is model.ErrNotFound.
func TestBeginToolCall(t *testing.T) {
	s, _ := newStore(t)
	sb := callSandbox(t, s)

	c := beginCall(t, s, sb, "k1", "ferund-0")
	if c.ID == uuid.Nil || c.SandboxID != sb || c.Key != "k1" || c.Tool != "bash" || !bytes.Equal(c.ArgsHash, argsHash("k1")) ||
		c.InstanceID != "ferund-0" || c.State != store.ToolCallRunning || c.Sent || !c.FinishedAt.IsZero() ||
		c.ExitCode != 0 || len(c.Output) != 0 || c.Truncated || c.IsError || c.Error != "" {
		t.Fatalf("new call %+v", c)
	}
	var leaseOK bool
	if err := s.Q().QueryRow(bg, `SELECT lease_until > now() + interval '29 seconds' AND lease_until <= now() + interval '30 seconds'
		AND started_at <= now() FROM tool_calls WHERE id = $1`, c.ID).Scan(&leaseOK); err != nil || !leaseOK {
		t.Fatalf("lease of the new call %v: %v %v", c.LeaseUntil, leaseOK, err)
	}

	again, inserted, err := s.BeginToolCall(bg, newCall(sb, "k1", "ferund-1"))
	if err != nil || inserted || again.ID != c.ID || !bytes.Equal(again.ArgsHash, c.ArgsHash) || again.State != c.State ||
		again.InstanceID != "ferund-0" || !again.StartedAt.Equal(c.StartedAt) || !again.LeaseUntil.Equal(c.LeaseUntil) {
		t.Fatalf("second begin %+v inserted %v: %v", again, inserted, err)
	}
	if got := getCall(t, s, sb, "k1"); got.ID != c.ID {
		t.Fatalf("get %+v", got)
	}
	if _, err := s.GetToolCall(bg, sb, "k2"); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("get of an unknown key: %v", err)
	}

	for n := range 5 {
		key := fmt.Sprintf("race-%d", n)
		var wg sync.WaitGroup
		calls := make([]store.ToolCall, 2)
		insertedBy := make([]bool, 2)
		errs := make([]error, 2)
		for i := range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				calls[i], insertedBy[i], errs[i] = s.BeginToolCall(bg, newCall(sb, key, fmt.Sprintf("ferund-%d", i)))
			}()
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || insertedBy[0] == insertedBy[1] || calls[0].ID != calls[1].ID {
			t.Fatalf("concurrent begins %+v %v %v", calls, insertedBy, errs)
		}
	}

	if _, _, err := s.BeginToolCall(bg, newCall(podid.Mint(), "k1", "ferund-0")); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("begin on a missing sandbox: %v", err)
	}
}

// A key is 1 to 255 characters of [A-Za-z0-9._~:@-] (ADR 0023).
func TestToolCallKeyFormat(t *testing.T) {
	s, _ := newStore(t)
	sb := callSandbox(t, s)
	for _, key := range []string{"", "a b", "a/b", strings.Repeat("a", 256), "ä"} {
		_, _, err := s.BeginToolCall(bg, newCall(sb, key, "ferund-0"))
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23514" || pe.ConstraintName != "tool_calls_key_format" {
			t.Errorf("key %q: %v", key, err)
		}
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._~:@-"
	key := strings.Repeat(alphabet, 4)[:255]
	c := beginCall(t, s, sb, key, "ferund-0")
	if got := getCall(t, s, sb, key); got.ID != c.ID || got.Key != key {
		t.Fatalf("255-byte key: %+v", got)
	}
}

// A not_run row is replaced by a Begin of the same tool and arguments: a
// new run, every column of the earlier one reset. Another tool or other
// arguments get the not_run row unchanged.
func TestToolCallNotRunReplacement(t *testing.T) {
	s, _ := newStore(t)
	sb := callSandbox(t, s)
	c := beginCall(t, s, sb, "k1", "ferund-0")
	markSent(t, s, c)
	if ok, err := s.FinishToolCall(bg, c.ID, store.ToolCallOutcome{State: store.ToolCallNotRun, ExitCode: 3,
		Output: []byte("out"), Truncated: true, IsError: true, Error: "not run: refused"}); err != nil || !ok {
		t.Fatalf("finish %v %v", ok, err)
	}
	notRun := getCall(t, s, sb, "k1")

	other := newCall(sb, "k1", "ferund-1")
	other.ArgsHash = argsHash("other")
	otherTool := newCall(sb, "k1", "ferund-1")
	otherTool.Tool = "write"
	for _, n := range []store.NewToolCall{other, otherTool} {
		got, inserted, err := s.BeginToolCall(bg, n)
		if err != nil || inserted || got.ID != c.ID || got.State != store.ToolCallNotRun || got.InstanceID != "ferund-0" ||
			got.Error != "not run: refused" || !got.FinishedAt.Equal(notRun.FinishedAt) || !got.Sent ||
			string(got.Output) != "out" || got.ExitCode != 3 {
			t.Fatalf("begin of %s %x: %+v inserted %v: %v", n.Tool, n.ArgsHash, got, inserted, err)
		}
	}

	r, inserted, err := s.BeginToolCall(bg, newCall(sb, "k1", "ferund-1"))
	if err != nil || !inserted || r.ID == c.ID || r.State != store.ToolCallRunning || r.Sent || !r.FinishedAt.IsZero() ||
		r.Error != "" || r.InstanceID != "ferund-1" || r.ExitCode != 0 || len(r.Output) != 0 || r.Truncated || r.IsError ||
		!r.LeaseUntil.After(r.StartedAt) {
		t.Fatalf("replacement %+v inserted %v: %v", r, inserted, err)
	}
	if got := getCall(t, s, sb, "k1"); got.ID != r.ID || got.State != store.ToolCallRunning {
		t.Fatalf("row after the replacement %+v", got)
	}
}

func TestMarkToolCallSent(t *testing.T) {
	s, _ := newStore(t)
	sb := callSandbox(t, s)
	c := beginCall(t, s, sb, "k1", "ferund-0")
	markSent(t, s, c)
	if got := getCall(t, s, sb, "k1"); !got.Sent || got.State != store.ToolCallRunning {
		t.Fatalf("after the mark %+v", got)
	}
	finishCall(t, s, c, store.ToolCallDone)
	if ok, err := s.MarkToolCallSent(bg, c.ID); err != nil || ok {
		t.Fatalf("mark of a finished call: %v %v", ok, err)
	}
	if ok, err := s.MarkToolCallSent(bg, uuid.New()); err != nil || ok {
		t.Fatalf("mark of an unknown call: %v %v", ok, err)
	}
}

// noSQL is a Querier that counts what it is asked to run and runs nothing.
type noSQL struct{ calls int }

var errNoSQL = errors.New("no SQL expected")

type errRow struct{}

func (errRow) Scan(...any) error { return errNoSQL }

func (q *noSQL) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	q.calls++
	return pgconn.CommandTag{}, errNoSQL
}

func (q *noSQL) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.calls++
	return nil, errNoSQL
}

func (q *noSQL) QueryRow(context.Context, string, ...any) pgx.Row {
	q.calls++
	return errRow{}
}

func (q *noSQL) Begin(context.Context) (pgx.Tx, error) {
	q.calls++
	return nil, errNoSQL
}

// FinishToolCall ends a running call with its outcome, byte for byte, and
// notifies its id; a call ends once. A state that is not final is refused
// before any SQL.
func TestFinishToolCall(t *testing.T) {
	s, d := newStore(t)
	sb := callSandbox(t, s)
	c := beginCall(t, s, sb, "k1", "ferund-0")
	ch, unsubscribe := d.Subscribe(db.ChanCall)
	defer unsubscribe()

	out := []byte{'a', 0, 0xff, 'b', 0, '\n', 0xfe}
	if ok, err := s.FinishToolCall(bg, c.ID, store.ToolCallOutcome{State: store.ToolCallDone, ExitCode: 7, Output: out,
		Truncated: true, IsError: true, Error: "exit 7"}); err != nil || !ok {
		t.Fatalf("finish %v %v", ok, err)
	}
	expectPayload(t, ch, c.ID.String())
	r := getCall(t, s, sb, "k1")
	if r.State != store.ToolCallDone || r.FinishedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) || !bytes.Equal(r.Output, out) ||
		r.ExitCode != 7 || !r.Truncated || !r.IsError || r.Error != "exit 7" {
		t.Fatalf("finished call %+v", r)
	}
	if ok, err := s.FinishToolCall(bg, c.ID, store.ToolCallOutcome{State: store.ToolCallInterrupted}); err != nil || ok {
		t.Fatalf("second finish %v %v", ok, err)
	}
	if got := getCall(t, s, sb, "k1"); got.State != store.ToolCallDone || !bytes.Equal(got.Output, out) {
		t.Fatalf("the second finish changed the row: %+v", got)
	}

	// No output at all is an empty one.
	c2 := beginCall(t, s, sb, "k2", "ferund-0")
	finishCall(t, s, c2, store.ToolCallInterrupted)
	expectPayload(t, ch, c2.ID.String())
	if got := getCall(t, s, sb, "k2"); got.State != store.ToolCallInterrupted || len(got.Output) != 0 {
		t.Fatalf("finish without output %+v", got)
	}

	c3 := beginCall(t, s, sb, "k3", "ferund-0")
	for _, state := range []string{"", store.ToolCallRunning, "failed"} {
		q := &noSQL{}
		if ok, err := store.On(q).FinishToolCall(bg, c3.ID, store.ToolCallOutcome{State: state}); err == nil || ok ||
			errors.Is(err, errNoSQL) || q.calls != 0 {
			t.Fatalf("finish as %q: %v %v, %d statements", state, ok, err, q.calls)
		}
		if ok, err := s.FinishToolCall(bg, c3.ID, store.ToolCallOutcome{State: state}); err == nil || ok {
			t.Fatalf("finish as %q: %v %v", state, ok, err)
		}
	}
	expectRunning(t, s, c3)
}

// Abandon ends only a running call of that instance and key that was not
// sent.
func TestAbandonToolCall(t *testing.T) {
	s, d := newStore(t)
	sb := callSandbox(t, s)
	c := beginCall(t, s, sb, "k1", "ferund-0")
	sent := beginCall(t, s, sb, "sent", "ferund-0")
	markSent(t, s, sent)
	ch, unsubscribe := d.Subscribe(db.ChanCall)
	defer unsubscribe()

	for _, a := range [][3]string{{"k1", "ferund-1"}, {"k2", "ferund-0"}, {"sent", "ferund-0"}} {
		if ok, err := s.AbandonToolCall(bg, sb, a[0], a[1], "not run: stopping"); err != nil || ok {
			t.Fatalf("abandon of %s by %s: %v %v", a[0], a[1], ok, err)
		}
	}
	if ok, err := s.AbandonToolCall(bg, podid.Mint(), "k1", "ferund-0", "not run: stopping"); err != nil || ok {
		t.Fatalf("abandon on another sandbox: %v %v", ok, err)
	}
	if ok, err := s.AbandonToolCall(bg, sb, "k1", "ferund-0", "not run: stopping"); err != nil || !ok {
		t.Fatalf("abandon %v %v", ok, err)
	}
	expectPayload(t, ch, c.ID.String())
	expectEnded(t, s, c, store.ToolCallNotRun, "not run: stopping")
	if ok, err := s.AbandonToolCall(bg, sb, "k1", "ferund-0", "again"); err != nil || ok {
		t.Fatalf("second abandon %v %v", ok, err)
	}
	expectNoPayload(t, ch, sent.ID.String())
	if got := getCall(t, s, sb, "sent"); got.State != store.ToolCallRunning || !got.Sent || got.Error != "" {
		t.Fatalf("the sent call changed: %+v", got)
	}
}

// Renew extends only the instance's running calls and returns them.
func TestRenewToolCalls(t *testing.T) {
	s, _ := newStore(t)
	sb := callSandbox(t, s)
	a := beginCall(t, s, sb, "a", "ferund-0")
	b := beginCall(t, s, sb, "b", "ferund-0")
	finishCall(t, s, b, store.ToolCallDone)
	c := beginCall(t, s, sb, "c", "ferund-1")

	if ids, err := s.RenewToolCalls(bg, "ferund-0", nil, time.Hour); err != nil || ids != nil {
		t.Fatalf("renew of nothing: %v %v", ids, err)
	}
	ids, err := s.RenewToolCalls(bg, "ferund-0", []uuid.UUID{a.ID, b.ID, c.ID, uuid.New()}, time.Hour)
	if err != nil || !sameIDs(ids, a.ID) {
		t.Fatalf("renewed %v: %v", ids, err)
	}
	leaseFrom := func(c store.ToolCall) (renewed bool) {
		t.Helper()
		if err := s.Q().QueryRow(bg, "SELECT lease_until > now() + interval '59 minutes' FROM tool_calls WHERE id = $1",
			c.ID).Scan(&renewed); err != nil {
			t.Fatal(err)
		}
		return renewed
	}
	if !leaseFrom(a) || leaseFrom(b) || leaseFrom(c) {
		t.Fatalf("leases: a %v b %v c %v", leaseFrom(a), leaseFrom(b), leaseFrom(c))
	}
	if got := getCall(t, s, sb, "a"); !got.LeaseUntil.After(a.LeaseUntil) {
		t.Fatalf("lease %v, was %v", got.LeaseUntil, a.LeaseUntil)
	}
}

// expireCalls sets the leases of the calls back by ago.
func expireCalls(t *testing.T, s *store.Store, ago string, calls ...store.ToolCall) {
	t.Helper()
	for _, c := range calls {
		exec(t, s, "UPDATE tool_calls SET lease_until = now() - $2::interval WHERE id = $1", c.ID, ago)
	}
}

// The leader's expiry ends the calls whose lease ran out more than the
// margin ago, oldest first: interrupted when sent, not_run when not, each
// naming the instance, each notified.
func TestExpireToolCalls(t *testing.T) {
	s, d := newStore(t)
	sb := callSandbox(t, s)
	sent := beginCall(t, s, sb, "sent", "ferund-0")
	markSent(t, s, sent)
	unsent := beginCall(t, s, sb, "unsent", "ferund-1")
	fresh := beginCall(t, s, sb, "fresh", "ferund-0")
	expireCalls(t, s, "1 second", sent, unsent)
	ch, unsubscribe := d.Subscribe(db.ChanCall)
	defer unsubscribe()

	if ids, err := s.ExpireToolCalls(bg, time.Minute, 10); err != nil || len(ids) != 0 {
		t.Fatalf("expiry with a margin of a minute: %v %v", ids, err)
	}
	expectRunning(t, s, sent)
	expectRunning(t, s, unsent)

	ids, err := s.ExpireToolCalls(bg, 0, 10)
	if err != nil || !sameIDs(ids, sent.ID, unsent.ID) {
		t.Fatalf("expired %v: %v", ids, err)
	}
	expectPayloads(t, ch, sent.ID.String(), unsent.ID.String())
	expectEnded(t, s, sent, store.ToolCallInterrupted, expiredSent("ferund-0"))
	expectEnded(t, s, unsent, store.ToolCallNotRun, expiredUnsent("ferund-1"))
	expectRunning(t, s, fresh)

	// The limit, oldest lease first.
	var old []store.ToolCall
	for n := range 3 {
		c := beginCall(t, s, sb, fmt.Sprintf("old-%d", n), "ferund-0")
		expireCalls(t, s, fmt.Sprintf("%d seconds", 3-n), c)
		old = append(old, c)
	}
	if ids, err := s.ExpireToolCalls(bg, 0, 2); err != nil || !sameIDs(ids, old[0].ID, old[1].ID) {
		t.Fatalf("expired with a limit of 2: %v %v", ids, err)
	}
	expectRunning(t, s, old[2])
	if ids, err := s.ExpireToolCalls(bg, 0, 2); err != nil || !sameIDs(ids, old[2].ID) {
		t.Fatalf("expired the rest: %v %v", ids, err)
	}
	expectRunning(t, s, fresh)
}

// A waiter's expiry ends the one call it waits on, as the leader's does.
func TestExpireToolCall(t *testing.T) {
	s, d := newStore(t)
	sb := callSandbox(t, s)
	sent := beginCall(t, s, sb, "sent", "ferund-0")
	markSent(t, s, sent)
	unsent := beginCall(t, s, sb, "unsent", "ferund-0")
	fresh := beginCall(t, s, sb, "fresh", "ferund-0")
	expireCalls(t, s, "1 second", sent, unsent)
	ch, unsubscribe := d.Subscribe(db.ChanCall)
	defer unsubscribe()

	if state, err := s.ExpireToolCall(bg, sent.ID, time.Minute); err != nil || state != "" {
		t.Fatalf("expiry with a margin of a minute: %q %v", state, err)
	}
	if state, err := s.ExpireToolCall(bg, fresh.ID, 0); err != nil || state != "" {
		t.Fatalf("expiry of a live lease: %q %v", state, err)
	}
	expectRunning(t, s, sent)
	for _, tc := range []struct {
		c    store.ToolCall
		want string
	}{{sent, store.ToolCallInterrupted}, {unsent, store.ToolCallNotRun}} {
		if state, err := s.ExpireToolCall(bg, tc.c.ID, 0); err != nil || state != tc.want {
			t.Fatalf("expiry of %s: %q %v", tc.c.Key, state, err)
		}
		expectPayload(t, ch, tc.c.ID.String())
	}
	expectEnded(t, s, sent, store.ToolCallInterrupted, expiredSent("ferund-0"))
	expectEnded(t, s, unsent, store.ToolCallNotRun, expiredUnsent("ferund-0"))
	if state, err := s.ExpireToolCall(bg, sent.ID, 0); err != nil || state != "" {
		t.Fatalf("second expiry: %q %v", state, err)
	}
	expectRunning(t, s, fresh)
}

// A claim of the instance ID ends that ID's running calls, and no others.
func TestEndInstanceToolCalls(t *testing.T) {
	s, d := newStore(t)
	sb := callSandbox(t, s)
	sent := beginCall(t, s, sb, "sent", "ferund-0")
	markSent(t, s, sent)
	unsent := beginCall(t, s, sb, "unsent", "ferund-0")
	done := beginCall(t, s, sb, "done", "ferund-0")
	finishCall(t, s, done, store.ToolCallDone)
	other := beginCall(t, s, sb, "other", "ferund-1")
	ch, unsubscribe := d.Subscribe(db.ChanCall)
	defer unsubscribe()

	if n, err := s.EndInstanceToolCalls(bg, "ferund-0"); err != nil || n != 2 {
		t.Fatalf("ended %d: %v", n, err)
	}
	expectPayloads(t, ch, sent.ID.String(), unsent.ID.String())
	expectEnded(t, s, sent, store.ToolCallInterrupted, claimedSent("ferund-0"))
	expectEnded(t, s, unsent, store.ToolCallNotRun, claimedUnsent("ferund-0"))
	expectEnded(t, s, done, store.ToolCallDone, "")
	expectRunning(t, s, other)
	if n, err := s.EndInstanceToolCalls(bg, "ferund-0"); err != nil || n != 0 {
		t.Fatalf("ended again %d: %v", n, err)
	}
}

// The retention deletes ended calls of every final state that ended more
// than the retention ago, oldest first; running and recent ones stay.
func TestDeleteFinishedToolCalls(t *testing.T) {
	s, _ := newStore(t)
	sb := callSandbox(t, s)
	var old []store.ToolCall
	for n, state := range []string{store.ToolCallDone, store.ToolCallInterrupted, store.ToolCallNotRun} {
		c := beginCall(t, s, sb, state, "ferund-0")
		finishCall(t, s, c, state)
		exec(t, s, "UPDATE tool_calls SET finished_at = now() - $2::interval WHERE id = $1", c.ID,
			fmt.Sprintf("2 hours %d seconds", 3-n))
		old = append(old, c)
	}
	recent := beginCall(t, s, sb, "recent", "ferund-0")
	finishCall(t, s, recent, store.ToolCallDone)
	running := beginCall(t, s, sb, "running", "ferund-0")
	exec(t, s, "UPDATE tool_calls SET started_at = now() - interval '3 hours' WHERE id = $1", running.ID)

	if n, err := s.DeleteFinishedToolCalls(bg, time.Hour, 2); err != nil || n != 2 {
		t.Fatalf("deleted %d: %v", n, err)
	}
	if _, err := s.GetToolCall(bg, sb, old[2].Key); err != nil {
		t.Fatalf("the newest old call: %v", err)
	}
	if n, err := s.DeleteFinishedToolCalls(bg, time.Hour, 1000); err != nil || n != 1 {
		t.Fatalf("deleted %d: %v", n, err)
	}
	for _, c := range old {
		if _, err := s.GetToolCall(bg, sb, c.Key); !errors.Is(err, store.ErrNoRow) {
			t.Fatalf("%s was kept: %v", c.Key, err)
		}
	}
	getCall(t, s, sb, "recent")
	expectRunning(t, s, running)
	if n, err := s.DeleteFinishedToolCalls(bg, time.Hour, 1000); err != nil || n != 0 {
		t.Fatalf("deleted again %d: %v", n, err)
	}
}

// A sandbox's calls go with it.
func TestToolCallCascade(t *testing.T) {
	s, _ := newStore(t)
	sb := callSandbox(t, s)
	beginCall(t, s, sb, "k1", "ferund-0")
	c := beginCall(t, s, sb, "k2", "ferund-0")
	finishCall(t, s, c, store.ToolCallDone)
	keep := mkSandbox(t, s, "acme").ID
	beginCall(t, s, keep, "k1", "ferund-0")

	exec(t, s, "DELETE FROM sandboxes WHERE id = $1", sb)
	var n int
	if err := s.Q().QueryRow(bg, "SELECT count(*) FROM tool_calls WHERE sandbox_id = $1", sb).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d calls of the deleted sandbox: %v", n, err)
	}
	getCall(t, s, keep, "k1")
}
