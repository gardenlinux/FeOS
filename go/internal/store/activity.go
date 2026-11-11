// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// The sources of activity rows.
const (
	SourceFerund = "ferund" // ferund's own rows
	SourceFeosd  = "feosd"  // a node's pod and node events
	SourceAuthn  = "authn"  // a pod's feos-authn audit records
)

// ActivityRow is a row of activity: the old system's event and span
// model.
type ActivityRow struct {
	Day             time.Time // the partition (UTC, DB clock); set by the database
	ID              int64     // set by the database
	TsNs            int64     // producer time, ordering
	SandboxID       podid.ID  // the attributed sandbox; "": none
	PodID           podid.ID  // node records: the pod the record names, as sent
	ForeignPod      bool      // a node record about a pod not placed on that node
	TenantID        string
	Node            string
	StartID         uuid.UUID // node records: the start of feosd
	Seq             int64     // node events: the sequence number; 0 = none
	InstanceID      string    // ferund's own rows: the instance
	Source          string    // Source*
	Kind            string
	Category        string
	RequestID       string
	SpanID          string
	ParentSpanID    string
	Name            string
	Tool            string
	Params          json.RawMessage // a JSON value, or nil
	ExitCode        *int64
	DurationNs      *int64
	Output          string
	OutputTruncated bool
	Detail          json.RawMessage // a JSON object; nil stores {}
}

const activityInsertCols = `ts_ns, sandbox_id, pod_id, foreign_pod, tenant_id, node, start_id, seq, instance_id, source, kind,
	category, request_id, span_id, parent_span_id, name, tool, params, exit_code, duration_ns, output, output_truncated, detail`

const activityCols = "day, id, " + activityInsertCols

// activityBatch bounds the rows of one INSERT statement.
const activityBatch = 500

// insertActivitySQL inserts rows given column by column, one array per
// column. Its text is the same for every batch size, so each connection
// prepares and caches it once.
const insertActivitySQL = `INSERT INTO activity (` + activityInsertCols + `)
	SELECT * FROM unnest($1::bigint[], $2::text[], $3::text[], $4::boolean[], $5::text[], $6::text[],
		$7::uuid[], $8::bigint[], $9::text[], $10::text[], $11::text[], $12::text[], $13::text[], $14::text[],
		$15::text[], $16::text[], $17::text[], $18::jsonb[], $19::bigint[], $20::bigint[], $21::text[],
		$22::boolean[], $23::jsonb[])
	ON CONFLICT DO NOTHING`

// uuidOrNull is u, or NULL for uuid.Nil, as an array element. (A nil
// *uuid.UUID cannot be one: pgx calls its Value method, which panics.)
func uuidOrNull(u uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil}
}

// idOrNull is id, or nil (NULL) for none, as an array element.
func idOrNull(id podid.ID) *string {
	if id.IsZero() {
		return nil
	}
	s := string(id)
	return &s
}

// seqOrNull is n, or nil (NULL) for 0.
func seqOrNull(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}

// jsonOrNull is b as JSON text, or nil (NULL) when empty.
func jsonOrNull(b json.RawMessage) *string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	return &s
}

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func jsonObject(b json.RawMessage) string {
	if len(b) == 0 {
		return "{}"
	}
	return string(b)
}

// InsertActivity inserts the rows, in statements of up to 500 rows. A row
// that collides with a stored node event of the same (node, start, seq,
// day) is skipped. Strings must hold no NUL byte and the JSON must be
// valid jsonb (internal/activity sanitises what it records).
func (s *Store) InsertActivity(ctx context.Context, rows ...ActivityRow) error {
	for chunk := range slices.Chunk(rows, activityBatch) {
		n := len(chunk)
		var (
			tsNs, seq                 = make([]int64, n), make([]*int64, n)
			sandboxID, podID          = make([]*string, n), make([]*string, n)
			startID                   = make([]pgtype.UUID, n)
			foreignPod, truncated     = make([]bool, n), make([]bool, n)
			tenantID, node, instance  = make([]string, n), make([]string, n), make([]string, n)
			source, kind, category    = make([]string, n), make([]string, n), make([]string, n)
			requestID, spanID, parent = make([]string, n), make([]string, n), make([]string, n)
			name, tool, output        = make([]string, n), make([]string, n), make([]string, n)
			params                    = make([]*string, n)
			exitCode, durationNs      = make([]*int64, n), make([]*int64, n)
			detail                    = make([]string, n)
		)
		for i, r := range chunk {
			tsNs[i], seq[i] = r.TsNs, seqOrNull(r.Seq)
			sandboxID[i], podID[i], startID[i] = idOrNull(r.SandboxID), idOrNull(r.PodID), uuidOrNull(r.StartID)
			foreignPod[i], truncated[i] = r.ForeignPod, r.OutputTruncated
			tenantID[i], node[i], instance[i] = r.TenantID, r.Node, r.InstanceID
			source[i], kind[i], category[i] = r.Source, r.Kind, r.Category
			requestID[i], spanID[i], parent[i] = r.RequestID, r.SpanID, r.ParentSpanID
			name[i], tool[i], output[i] = r.Name, r.Tool, r.Output
			params[i] = jsonOrNull(r.Params)
			exitCode[i], durationNs[i] = r.ExitCode, r.DurationNs
			detail[i] = jsonObject(r.Detail)
		}
		if _, err := s.q.Exec(ctx, insertActivitySQL, tsNs, sandboxID, podID, foreignPod, tenantID, node, startID,
			seq, instance, source, kind, category, requestID, spanID, parent, name, tool, params, exitCode,
			durationNs, output, truncated, detail); err != nil {
			return fmt.Errorf("insert activity: %w", err)
		}
	}
	return nil
}

func scanActivity(row pgx.Row) (ActivityRow, error) {
	var r ActivityRow
	var sandboxID, podID *podid.ID
	var startID *uuid.UUID
	var seq *int64
	var params, detail []byte
	if err := row.Scan(&r.Day, &r.ID, &r.TsNs, &sandboxID, &podID, &r.ForeignPod, &r.TenantID, &r.Node, &startID, &seq,
		&r.InstanceID, &r.Source, &r.Kind, &r.Category, &r.RequestID, &r.SpanID, &r.ParentSpanID, &r.Name, &r.Tool,
		&params, &r.ExitCode, &r.DurationNs, &r.Output, &r.OutputTruncated, &detail); err != nil {
		return ActivityRow{}, err
	}
	r.SandboxID, r.PodID, r.StartID = idOf(sandboxID), idOf(podID), uuidOf(startID)
	if seq != nil {
		r.Seq = *seq
	}
	if params != nil {
		r.Params = json.RawMessage(params)
	}
	r.Detail = json.RawMessage(detail)
	return r, nil
}

// ActivityQuery selects a page of activity: the newest Limit rows older
// than BeforeTsNs (0: no bound) of Source ("": any), none of SkipKinds.
// Traffic records take Limit and BeforeTsNs alone.
type ActivityQuery struct {
	Limit      int
	BeforeTsNs int64
	Source     string
	SkipKinds  []string
}

// listActivity returns the page of rows matching where (with its first
// argument), ascending by ts_ns.
func (s *Store) listActivity(ctx context.Context, where string, arg any, q ActivityQuery) ([]ActivityRow, error) {
	args := []any{arg}
	if q.BeforeTsNs != 0 {
		args = append(args, q.BeforeTsNs)
		where += fmt.Sprintf(" AND ts_ns < $%d", len(args))
	}
	if q.Source != "" {
		args = append(args, q.Source)
		where += fmt.Sprintf(" AND source = $%d", len(args))
	}
	if len(q.SkipKinds) > 0 {
		args = append(args, q.SkipKinds)
		where += fmt.Sprintf(" AND kind <> ALL($%d)", len(args))
	}
	args = append(args, q.Limit)
	rows, err := s.q.Query(ctx, "SELECT "+activityCols+" FROM activity WHERE "+where+
		fmt.Sprintf(" ORDER BY ts_ns DESC, id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ActivityRow, error) { return scanActivity(r) })
	if err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return out, nil
}

// ListSandboxActivity returns a page of a sandbox's attributed activity,
// ascending by ts_ns (foreign records are never attributed).
func (s *Store) ListSandboxActivity(ctx context.Context, sandboxID podid.ID, q ActivityQuery) ([]ActivityRow, error) {
	return s.listActivity(ctx, "sandbox_id = $1", sandboxID, q)
}

// ListNodeActivity returns a page of a node's own activity: its
// node-scoped rows and the foreign records it sent, ascending by ts_ns.
func (s *Store) ListNodeActivity(ctx context.Context, node string, q ActivityQuery) ([]ActivityRow, error) {
	return s.listActivity(ctx, "node = $1 AND sandbox_id IS NULL", node, q)
}

// ListSpanActivity returns the rows of a span and of its children,
// ascending by ts_ns.
func (s *Store) ListSpanActivity(ctx context.Context, spanID string) ([]ActivityRow, error) {
	rows, err := s.q.Query(ctx, "SELECT "+activityCols+` FROM activity WHERE span_id = $1 OR parent_span_id = $1
		ORDER BY ts_ns, id`, spanID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ActivityRow, error) { return scanActivity(r) })
}
