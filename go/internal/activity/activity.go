// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package activity records ferund's own rows of the activity log: the
// event and span model the management UI's activity and timeline views
// read (tool calls, lifecycle ops, runs, state changes, placements,
// credentials). Spans are trees: runs ("run-<uuid>") are roots, an op that
// opens a run is its child, and a row joins a span by span_id and its
// parent by parent_span_id; a span is closed by a row with a duration.
//
// The Recorder writes rows in batches from a bounded queue: recording
// never blocks a request, and a full queue drops rows and counts them. A
// row that must commit with a lifecycle write goes through RecordTx in the
// writer's transaction instead. The package also owns the daily partitions
// of the activity and traffic tables and their retention (Loops).
package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The kinds of the rows ferund writes, and of the node records fleet
// stores.
const (
	KindToolStart      = "tool_start"
	KindToolEnd        = "tool_end"
	KindLifecycleStart = "lifecycle_start"
	KindLifecycleEnd   = "lifecycle_end"
	KindRunStart       = "run_start"
	KindRunEnd         = "run_end"
	KindSandboxState   = "sandbox_state"
	KindPlacement      = "placement"
	KindOverbooked     = "overbooked"
	KindTombstone      = "tombstone"
	KindPodCredentials = "pod_credentials"
	// KindPodHTTPBindings is a push of a pod's HTTP bindings' clear part
	// (ADR 0028): its version and the node's answer.
	KindPodHTTPBindings = "pod_http_bindings"
	KindNodeCert        = "node_cert"
	KindNodeStart       = "node_start"
	KindPodState        = "pod_state"
	KindNodeStarted     = "node_started"
	KindNodeStopping    = "node_stopping"
	KindNodeEvent       = "node_event"
)

// The categories: the lanes of the timeline.
const (
	CategoryLifecycle  = "lifecycle"
	CategoryTool       = "tool"
	CategoryNetwork    = "network"
	CategoryCredential = "credential"
)

// CategoryOf is the category of a kind when the producer names none: tool
// rows are tools, everything else is lifecycle.
func CategoryOf(kind string) string {
	switch kind {
	case KindToolStart, KindToolEnd:
		return CategoryTool
	case KindPodCredentials, KindNodeCert, KindPodHTTPBindings:
		return CategoryCredential
	}
	return CategoryLifecycle
}

// NameCap bounds a row's name in bytes: a name is a label, not a payload.
const NameCap = 120

// OutputCap bounds a row's output in bytes (the old audit cap).
const OutputCap = 64 << 10

const ellipsis = "…"

// Name normalises a human label: one line, whitespace collapsed, at most
// NameCap bytes, cut on a rune boundary with an ellipsis.
func Name(s string) string {
	s = strings.Join(strings.Fields(Clean(s)), " ")
	if len(s) <= NameCap {
		return s
	}
	cut := NameCap - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " ") + ellipsis
}

// NewSpanID mints a span id: "<prefix>-<uuid>" ("run-<uuid>" for a run),
// or a bare UUID for an empty prefix.
func NewSpanID(prefix string) string {
	if prefix == "" {
		return uuid.NewString()
	}
	return prefix + "-" + uuid.NewString()
}

// Clean makes a string storable as PostgreSQL text: NUL bytes removed,
// invalid UTF-8 replaced with U+FFFD.
func Clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return s
}

// CapOutput cuts s to at most OutputCap bytes on a rune boundary; the bool
// reports a cut.
func CapOutput(s string) (string, bool) {
	if len(s) <= OutputCap {
		return s, false
	}
	cut := OutputCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// CleanJSON makes a JSON value storable as jsonb, which refuses more than
// Go's decoder does: invalid JSON becomes a JSON string of its cleaned
// text. A valid document jsonb would refuse (see storable) is decoded and
// encoded again: invalid UTF-8 and unpaired surrogate escapes become
// U+FFFD, \u0000 is removed from strings and keys, and a number numeric
// cannot hold becomes a JSON string of its text. Empty input stays empty.
func CleanJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if !json.Valid(b) {
		s, _ := json.Marshal(Clean(string(b)))
		return s
	}
	if storable(b) {
		return json.RawMessage(b)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		s, _ := json.Marshal(Clean(string(b)))
		return s
	}
	out, err := json.Marshal(cleanValue(v))
	if err != nil {
		s, _ := json.Marshal(Clean(string(b)))
		return s
	}
	return out
}

// storable reports whether jsonb takes b, a valid JSON document, as it is:
// b is valid UTF-8 (PostgreSQL refuses invalid text before it parses
// anything), holds no \u escape (jsonb refuses \u0000 and an unpaired
// surrogate, both of which Go accepts; an escape that is fine costs only
// the slower path), and each of its numbers is a jsonbNumber.
func storable(b []byte) bool {
	if !utf8.Valid(b) || bytes.Contains(b, []byte(`\u`)) {
		return false
	}
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case c == '"':
			for i++; i < len(b) && b[i] != '"'; i++ {
				if b[i] == '\\' {
					i++
				}
			}
		case c == '-' || '0' <= c && c <= '9':
			j := i + 1
			for j < len(b) && strings.IndexByte("0123456789.eE+-", b[j]) >= 0 {
				j++
			}
			if !jsonbNumber(b[i:j]) {
				return false
			}
			i = j - 1
		}
	}
	return true
}

// jsonbNumberLen bounds the length of a JSON number jsonb is given as a
// number. Together with an exponent of at most four digits it keeps the
// number well inside what PostgreSQL's numeric holds (131072 digits before
// the decimal point, 16383 after it); outside, numeric refuses the
// document. Go's float64 range is no guide: 1e-20000 parses as 0 there.
const jsonbNumberLen = 1000

// jsonbNumber reports whether n, a JSON number, is within jsonbNumberLen
// and has an exponent of at most four digits.
func jsonbNumber(n []byte) bool {
	if len(n) > jsonbNumberLen {
		return false
	}
	if i := bytes.IndexAny(n, "eE"); i >= 0 {
		return len(bytes.TrimLeft(n[i+1:], "+-")) <= 4
	}
	return true
}

func cleanValue(v any) any {
	switch t := v.(type) {
	case string:
		return Clean(t)
	case json.Number:
		if !jsonbNumber([]byte(t)) {
			return string(t)
		}
		return t
	case []any:
		for i := range t {
			t[i] = cleanValue(t[i])
		}
		return t
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[Clean(k)] = cleanValue(x)
		}
		return m
	}
	return v
}

// detailJSON encodes a row's detail as a JSON object.
func detailJSON(d map[string]any) json.RawMessage {
	if len(d) == 0 {
		return json.RawMessage("{}")
	}
	b, err := json.Marshal(d)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"error": "detail not encodable: " + err.Error()})
	}
	return CleanJSON(b)
}

// Event is one row ferund records.
type Event struct {
	TsNs      int64    // producer time; 0: now
	SandboxID podid.ID // "": none (a node-scoped row)
	TenantID  string
	Node      string

	Kind         string
	Category     string // "": CategoryOf(Kind)
	RequestID    string
	SpanID       string
	ParentSpanID string
	Name         string // normalised with Name
	Tool         string

	Params     json.RawMessage
	ExitCode   *int64
	DurationNs *int64 // set on the row that closes a span

	Output          string // capped at OutputCap
	OutputTruncated bool
	Detail          map[string]any
}

// queueSize bounds the rows waiting for the flusher.
const queueSize = 4096

// maxBatch bounds the rows of one insert.
const maxBatch = 256

// Recorder writes ferund's activity rows: Record queues a row without
// blocking, Run inserts the queue in batches.
type Recorder struct {
	s          *store.Store
	instanceID string
	log        *slog.Logger
	queue      chan store.ActivityRow
	dropped    atomic.Int64

	logMu      sync.Mutex
	lastLog    time.Time
	suppressed int64
}

// NewRecorder returns a Recorder that writes through s as instanceID.
func NewRecorder(s *store.Store, instanceID string, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{s: s, instanceID: instanceID, log: log, queue: make(chan store.ActivityRow, queueSize)}
}

// Row is the activity row of e as the Recorder writes it: source ferund,
// its instance, strings cleaned, the name normalised, the output capped,
// the time and category filled in.
func (r *Recorder) Row(e Event) store.ActivityRow {
	ts := e.TsNs
	if ts == 0 {
		ts = time.Now().UnixNano()
	}
	category := e.Category
	if category == "" {
		category = CategoryOf(e.Kind)
	}
	out, cut := CapOutput(Clean(e.Output))
	return store.ActivityRow{
		TsNs:            ts,
		SandboxID:       e.SandboxID,
		TenantID:        Clean(e.TenantID),
		Node:            Clean(e.Node),
		InstanceID:      r.instanceID,
		Source:          store.SourceFerund,
		Kind:            Clean(e.Kind),
		Category:        Clean(category),
		RequestID:       Clean(e.RequestID),
		SpanID:          Clean(e.SpanID),
		ParentSpanID:    Clean(e.ParentSpanID),
		Name:            Name(e.Name),
		Tool:            Clean(e.Tool),
		Params:          CleanJSON(e.Params),
		ExitCode:        e.ExitCode,
		DurationNs:      e.DurationNs,
		Output:          out,
		OutputTruncated: e.OutputTruncated || cut,
		Detail:          detailJSON(e.Detail),
	}
}

// Record queues e for the flusher. It never blocks: when the queue is
// full, the row is dropped and counted (Dropped).
func (r *Recorder) Record(e Event) {
	select {
	case r.queue <- r.Row(e):
	default:
		r.drop(1, "the activity queue is full")
	}
}

// RecordTx writes e at once through q, in the caller's transaction, so
// that it commits or rolls back with the caller's write.
func (r *Recorder) RecordTx(ctx context.Context, q db.Querier, e Event) error {
	return store.On(q).InsertActivity(ctx, r.Row(e))
}

// Dropped returns how many rows were dropped: queued while the queue was
// full, or not stored after retries.
func (r *Recorder) Dropped() int64 { return r.dropped.Load() }

// drop counts n dropped rows and logs at most once a second, with the
// number of drops not logged since.
func (r *Recorder) drop(n int64, why string) {
	r.dropped.Add(n)
	r.logMu.Lock()
	defer r.logMu.Unlock()
	now := time.Now()
	if now.Sub(r.lastLog) < time.Second {
		r.suppressed += n
		return
	}
	r.log.Warn("activity rows dropped", "reason", why, "rows", n, "suppressed", r.suppressed, "total", r.dropped.Load())
	r.lastLog, r.suppressed = now, 0
}

// flushAttempts is how often a batch is tried before it is dropped.
const flushAttempts = 3

// flushTimeout bounds one insert of a batch.
const flushTimeout = 10 * time.Second

// Run inserts the queued rows in batches of up to 256 until ctx ends, then
// writes what is still queued (for at most 5 s) and returns nil. A batch
// the database cannot take is tried three times and then dropped; one it
// refuses for the content of some of its rows loses only those (flush).
// An insert under way when ctx ends is finished, not cancelled.
func (r *Recorder) Run(ctx context.Context) error {
	batch := make([]store.ActivityRow, 0, maxBatch)
	for {
		select {
		case <-ctx.Done():
			// Write what is queued now; rows recorded from here on wait
			// for nobody.
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			for left := len(r.queue); left > 0; left -= len(batch) {
				batch = r.gather(batch[:0], min(left, maxBatch))
				if len(batch) == 0 {
					break
				}
				r.flush(fctx, fctx.Done(), batch)
			}
			return nil
		case row := <-r.queue:
			batch = r.gather(append(batch[:0], row), maxBatch)
			r.flush(context.WithoutCancel(ctx), ctx.Done(), batch)
		}
	}
}

// gather appends queued rows to batch without waiting, up to n rows.
func (r *Recorder) gather(batch []store.ActivityRow, n int) []store.ActivityRow {
	for len(batch) < n {
		select {
		case row := <-r.queue:
			batch = append(batch, row)
		default:
			return batch
		}
	}
	return batch
}

// flush inserts batch, each attempt on ctx within flushTimeout. An insert
// that fails is tried again, flushAttempts times in all, and then the
// batch is dropped; once stop is closed it is not tried again. An insert
// the database refuses for the content of its rows (dataError) is not
// tried again: the batch is split in halves and each flushed on its own,
// so that only the rows the database refuses are dropped. flush returns
// false when it dropped rows for another reason; the rest of a split batch
// is then dropped too instead of waiting on the database again.
func (r *Recorder) flush(ctx context.Context, stop <-chan struct{}, batch []store.ActivityRow) bool {
	backoff := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, flushTimeout)
		err := r.s.InsertActivity(actx, batch...)
		cancel()
		switch {
		case err == nil:
			return true
		case dataError(err) && len(batch) == 1:
			r.drop(1, fmt.Sprintf("the database refused the row: %v", err))
			return true
		case dataError(err):
			half := len(batch) / 2
			if !r.flush(ctx, stop, batch[:half]) {
				r.drop(int64(len(batch)-half), "insert of the other half of a split batch failed")
				return false
			}
			return r.flush(ctx, stop, batch[half:])
		case attempt == flushAttempts:
			r.drop(int64(len(batch)), fmt.Sprintf("insert failed: %v", err))
			return false
		}
		t := time.NewTimer(backoff)
		select {
		case <-stop:
			t.Stop()
			r.drop(int64(len(batch)), fmt.Sprintf("insert failed while stopping: %v", err))
			return false
		case <-t.C:
		}
		backoff *= 4
	}
}

// dataError reports whether err is the database refusing a statement for
// the values it was given: a data exception (SQLSTATE class 22), an
// integrity constraint violation (23) or a program limit (54, such as a
// value too large). Trying such a statement again fails again.
func dataError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code[:min(2, len(pgErr.Code))] {
	case "22", "23", "54":
		return true
	}
	return false
}
