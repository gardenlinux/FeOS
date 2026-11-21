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

	"github.com/gardenlinux/feos/go/internal/podid"
)

// TrafficRow is a row of traffic_records: a pod's traffic record from a
// node (ADR 0013).
type TrafficRow struct {
	Day        time.Time // set by the database
	ID         int64     // set by the database
	TsNs       int64     // the record's time
	Node       string
	StartID    uuid.UUID
	Seq        int64
	PodID      podid.ID // as sent
	SandboxID  podid.ID // the attributed sandbox; "" for a foreign record
	ForeignPod bool
	ProjectID  string
	Kind       string // flow, drop, dns, server_name
	Missed     int64
	Detail     json.RawMessage // protojson (UseProtoNames) of the record
}

const trafficInsertCols = "ts_ns, node, start_id, seq, pod_id, sandbox_id, foreign_pod, project_id, kind, missed, detail"

const trafficCols = "day, id, " + trafficInsertCols

// insertTrafficSQL inserts records given column by column, one array per
// column. Its text is the same for every batch size, so each connection
// prepares and caches it once.
const insertTrafficSQL = `INSERT INTO traffic_records (` + trafficInsertCols + `)
	SELECT * FROM unnest($1::bigint[], $2::text[], $3::uuid[], $4::bigint[], $5::text[], $6::text[],
		$7::boolean[], $8::text[], $9::text[], $10::bigint[], $11::jsonb[])
	ON CONFLICT DO NOTHING`

// InsertTraffic inserts the records, in statements of up to 500 rows. A
// record that collides with a stored one of the same (node, start, seq,
// day) is skipped.
func (s *Store) InsertTraffic(ctx context.Context, rows ...TrafficRow) error {
	for chunk := range slices.Chunk(rows, activityBatch) {
		n := len(chunk)
		var (
			tsNs, seq, missed = make([]int64, n), make([]int64, n), make([]int64, n)
			node, projectID   = make([]string, n), make([]string, n)
			kind, detail      = make([]string, n), make([]string, n)
			startID           = make([]uuid.UUID, n)
			podID, sandboxID  = make([]*string, n), make([]*string, n)
			foreignPod        = make([]bool, n)
		)
		for i, r := range chunk {
			tsNs[i], seq[i], missed[i] = r.TsNs, r.Seq, r.Missed
			node[i], projectID[i], kind[i], detail[i] = r.Node, r.ProjectID, r.Kind, jsonObject(r.Detail)
			startID[i], podID[i], sandboxID[i] = r.StartID, idOrNull(r.PodID), idOrNull(r.SandboxID)
			foreignPod[i] = r.ForeignPod
		}
		if _, err := s.q.Exec(ctx, insertTrafficSQL, tsNs, node, startID, seq, podID, sandboxID, foreignPod,
			projectID, kind, missed, detail); err != nil {
			return fmt.Errorf("insert traffic records: %w", err)
		}
	}
	return nil
}

func scanTraffic(row pgx.Row) (TrafficRow, error) {
	var r TrafficRow
	var podID, sandboxID *podid.ID
	var detail []byte
	if err := row.Scan(&r.Day, &r.ID, &r.TsNs, &r.Node, &r.StartID, &r.Seq, &podID, &sandboxID, &r.ForeignPod,
		&r.ProjectID, &r.Kind, &r.Missed, &detail); err != nil {
		return TrafficRow{}, err
	}
	r.PodID, r.SandboxID = idOf(podID), idOf(sandboxID)
	r.Detail = json.RawMessage(detail)
	return r, nil
}

// trafficPage is the statement and the arguments of a page of the
// records matching where (with its first argument), newest first.
func trafficPage(where string, arg any, q ActivityQuery) (string, []any) {
	args := []any{arg}
	if q.BeforeTsNs != 0 {
		args = append(args, q.BeforeTsNs)
		where += fmt.Sprintf(" AND ts_ns < $%d", len(args))
	}
	args = append(args, q.Limit)
	return "SELECT " + trafficCols + " FROM traffic_records WHERE " + where +
		fmt.Sprintf(" ORDER BY ts_ns DESC, id DESC LIMIT $%d", len(args)), args
}

// listTraffic returns the page of records matching where (with its first
// argument), ascending by ts_ns.
func (s *Store) listTraffic(ctx context.Context, where string, arg any, q ActivityQuery) ([]TrafficRow, error) {
	sql, args := trafficPage(where, arg, q)
	rows, err := s.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TrafficRow, error) { return scanTraffic(r) })
	if err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return out, nil
}

// The filters of a sandbox's and a node's traffic records. A node's is
// served, with the order, by the partial index traffic_node_foreign: a
// page reads only its own rows, not every record of the node.
const (
	sandboxTrafficWhere = "sandbox_id = $1"
	nodeTrafficWhere    = "node = $1 AND sandbox_id IS NULL"
)

// ListSandboxTraffic returns a page of a sandbox's attributed traffic
// records (Source of q is ignored), ascending by ts_ns.
func (s *Store) ListSandboxTraffic(ctx context.Context, sandboxID podid.ID, q ActivityQuery) ([]TrafficRow, error) {
	return s.listTraffic(ctx, sandboxTrafficWhere, sandboxID, q)
}

// ListNodeTraffic returns a page of the foreign traffic records a node
// sent: those attributed to no sandbox (Source of q is ignored), ascending
// by ts_ns. With ListNodeActivity it makes up the activity of a node.
func (s *Store) ListNodeTraffic(ctx context.Context, node string, q ActivityQuery) ([]TrafficRow, error) {
	return s.listTraffic(ctx, nodeTrafficWhere, node, q)
}
