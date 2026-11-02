// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package store is ferund's typed access to the schema of internal/db: one
// file per domain (sandboxes, tenants, tokens, nodes, telemetry, node
// certificates, join tokens, leases, instances, holds, bookings,
// placements, tombstones, node events, activity, traffic, issued
// certificates, watches, bindings, cluster secrets, the keyed tool calls
// in tool_calls.go, the ingress's instances in ingresses.go, its holds in
// ingress_holds.go, the apps zone's host labels in host_labels.go, the
// ingress's routing table in ingress_table.go, and the environments'
// tables in env_*.go). A Store runs either on the pool or inside one
// transaction (InTx); its methods never begin a transaction of their own
// unless they say so.
//
// The writers send the notifications other instances wait for, in the
// writing transaction: every write of a sandbox's client_state, op, node,
// incarnation or checkpoint, and every sandbox insert or delete, sends
// ferun_sandbox with the id (or "*" for more than 256 rows); a node's
// supersession, re-join, removal, fork, new start, stopping_until or
// draining sends ferun_node; a watch's new paths, owner, result or deletion
// sends ferun_watch; the tool_calls.go writers that end a call (done,
// interrupted, not_run) send ferun_call with the call's id; the writes the
// ingress's table follows send ferun_ingress: "env:<id>" (or "*" for more
// than 256 environments) for an environment's publicity, host label,
// deployments and deletion (env_environments.go), its service instances and
// their admitted ports (env_instances.go) and its tenant's deletion
// (tenants.go); "ingress:<name>" for an ingress instance's re-join,
// removal, mesh join and certificates (ingresses.go); "mesh" for every
// change of the mesh's peers, a node's or an ingress's (bumpMeshPeers in
// mesh.go); and "*" for a node's version of the ingress's peer group, and
// its re-join while it held one (nodes.go). Other notifications
// (ferun_leader, ferun_reconcile, ferun_cancel) are the callers'.
//
// Every time a decision depends on is taken from the database clock
// (now()). Within a transaction now() is the transaction's start time, so
// Now returns the time every statement of the transaction compares with.
//
// Nullable columns map to zero values: "" for a NULL text, uuid.Nil for a
// NULL uuid, "" for a NULL pod ID, the zero time.Time for a NULL
// timestamptz.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// ErrNoRow is returned by the lookups of rows other than sandboxes when the
// row does not exist (sandbox lookups return model.ErrNotFound).
var ErrNoRow = errors.New("no such row")

// ErrStale is returned by UpdateSandbox when the row is no longer the one
// the caller read: another writer changed it, which a caller holding the
// row lock never sees.
var ErrStale = errors.New("the sandbox row changed since it was read")

// The channels the store's writers notify.
const (
	chanSandbox = db.ChanSandbox
	chanNode    = db.ChanNode
	chanWatch   = db.ChanWatch
	chanCall    = db.ChanCall
)

// notifyBatch is the most rows one transaction names one by one; a writer
// of more sends "*" instead.
const notifyBatch = 256

// Store runs typed queries on a pool or on one transaction.
type Store struct {
	d  *db.DB
	q  db.Querier
	tx bool
}

// New returns a Store on d's pool.
func New(d *db.DB) *Store {
	return &Store{d: d, q: d.Pool}
}

// On returns a Store that runs on q. With a pgx.Tx it is in that
// transaction: InTx then runs its function on the same transaction.
func On(q db.Querier) *Store {
	_, tx := q.(pgx.Tx)
	return &Store{q: q, tx: tx}
}

// Q returns the Querier the Store runs on.
func (s *Store) Q() db.Querier { return s.q }

// DB returns the database the Store was made from (nil for a Store made by
// On).
func (s *Store) DB() *db.DB { return s.d }

// InTx runs fn with a Store on a new transaction (db.DB.Tx: retried on
// serialization failures, deadlocks and lock timeouts) and commits it. On a
// Store that is already in a transaction, fn runs on it.
func (s *Store) InTx(ctx context.Context, fn func(tx *Store) error) error {
	if s.tx {
		return fn(s)
	}
	if s.d != nil {
		return s.d.Tx(ctx, func(tx pgx.Tx) error {
			return fn(&Store{d: s.d, q: tx, tx: true})
		})
	}
	return s.local(ctx, fn)
}

// local runs fn in a transaction without retries: the Store's own when it
// is in one, else a new one on its querier. For statements that need SET
// LOCAL, and for InTx on a Store made by On from a pool.
func (s *Store) local(ctx context.Context, fn func(tx *Store) error) error {
	if s.tx {
		return fn(s)
	}
	b, ok := s.q.(interface {
		Begin(ctx context.Context) (pgx.Tx, error)
	})
	if !ok {
		return errors.New("store: a querier that cannot begin a transaction")
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(&Store{d: s.d, q: tx, tx: true}); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = tx.Rollback(rctx)
		cancel()
		return err
	}
	return tx.Commit(ctx)
}

// Now returns the database's now(): within a transaction, the time every
// now() of the transaction is.
func (s *Store) Now(ctx context.Context) (time.Time, error) {
	var t time.Time
	if err := s.q.QueryRow(ctx, "SELECT now()").Scan(&t); err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// SetLocalTimeouts sets lock_timeout and statement_timeout for the rest of
// the Store's transaction (the leader's fenced loops: 1 s and 3 s). Outside
// a transaction it has no effect.
func (s *Store) SetLocalTimeouts(ctx context.Context, lock, statement time.Duration) error {
	_, err := s.q.Exec(ctx, "SELECT set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)",
		fmt.Sprintf("%dms", lock.Milliseconds()), fmt.Sprintf("%dms", statement.Milliseconds()))
	return err
}

// Notify sends payload on channel in the Store's transaction.
func (s *Store) Notify(ctx context.Context, channel, payload string) error {
	return db.Notify(ctx, s.q, channel, payload)
}

// notifySandboxes sends ferun_sandbox for each id, or "*" for more than
// notifyBatch ids.
func (s *Store) notifySandboxes(ctx context.Context, ids []podid.ID) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > notifyBatch {
		return s.Notify(ctx, chanSandbox, "*")
	}
	for _, id := range ids {
		if err := s.Notify(ctx, chanSandbox, id.String()); err != nil {
			return err
		}
	}
	return nil
}

// notifyCalls sends ferun_call for each tool call id, or "*" for more than
// notifyBatch ids.
func (s *Store) notifyCalls(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > notifyBatch {
		return s.Notify(ctx, chanCall, "*")
	}
	for _, id := range ids {
		if err := s.Notify(ctx, chanCall, id.String()); err != nil {
			return err
		}
	}
	return nil
}

// HashToken is how tokens are stored: the lowercase hex SHA-256 of the
// token as the client sends it.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}

func nullID(id podid.ID) any {
	if id.IsZero() {
		return nil
	}
	return string(id)
}

func timeOf(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

func stringOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func uuidOf(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
}

func idOf(p *podid.ID) podid.ID {
	if p == nil {
		return ""
	}
	return *p
}

// nonNil returns ss, or an empty slice for nil: a nil slice is NULL to
// pgx, and the array columns are NOT NULL.
func nonNil[T any](ss []T) []T {
	if ss == nil {
		return []T{}
	}
	return ss
}

// interval renders d for a "$n::interval" parameter.
func interval(d time.Duration) string {
	return fmt.Sprintf("%d microseconds", d.Microseconds())
}

func noRow(err error, sentinel error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return sentinel
	}
	return err
}

// isForeignKeyViolation reports a write that names a row that does not
// exist (SQLSTATE 23503).
func isForeignKeyViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}
