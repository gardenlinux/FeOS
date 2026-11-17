// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package db is ferund's access to PostgreSQL: the connection pool with the
// session settings every statement relies on, transactions that retry on
// serialization failures, deadlocks and lock timeouts, the embedded schema
// and its migration, the daily partitions of the activity tables, and the
// notification hub, through which every instance learns of another's writes
// (LISTEN/NOTIFY).
//
// Transactions run at READ COMMITTED with explicit row locks: every
// statement sees its own snapshot, so a recheck that must see what another
// transaction committed while this one waited for a lock is a separate,
// later statement. Nothing slow (a node call) ever runs inside a
// transaction.
package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gardenlinux/feos/go/internal/awsauth"
)

// Querier runs statements: a pgx.Tx, a *pgxpool.Pool or a *pgx.Conn.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// The notification channels. Every instance listens to all of them.
const (
	// ChanSandbox carries a sandbox id whose state, op, node, checkpoint or
	// existence changed, or "*": re-read every sandbox you wait on.
	ChanSandbox = "ferun_sandbox"
	// ChanNode carries a node name whose certificates, removal, identity,
	// stopping_until or draining changed.
	ChanNode = "ferun_node"
	// ChanLeader carries a hint that wakes the leader's loops.
	ChanLeader = "ferun_leader"
	// ChanWatch carries a watch id whose path set, owner or result changed.
	ChanWatch = "ferun_watch"
	// ChanCancel carries a JSON cancellation of an exec or an MCP call.
	ChanCancel = "ferun_cancel"
	// ChanReconcile carries a node name an instance asks another to
	// reconcile.
	ChanReconcile = "ferun_reconcile"
	// ChanCall carries the id of a tool call (tool_calls.id) that left the
	// state running (done, interrupted, not_run), or "*": re-read every
	// tool call you wait on.
	ChanCall = "ferun_call"
	// ChanIngress carries what changed in the ingress's table (ADR 0021):
	// "env:<uuid>", an environment whose publicity, releases, deletion or
	// service instances changed; "ingress:<name>", an ingress whose row or
	// certificates changed; "mesh", the mesh's peers; or "*": rebuild
	// everything.
	ChanIngress = "ferun_ingress"
)

// Channels lists every channel the hub listens to.
var Channels = []string{ChanSandbox, ChanNode, ChanLeader, ChanWatch, ChanCancel, ChanReconcile, ChanCall, ChanIngress}

// Session settings of every connection: a lock waited on for longer than
// lock_timeout fails the statement with 55P03 (Tx retries it), a statement
// running longer than statement_timeout is cancelled, and times are UTC.
const (
	sessionLockTimeout      = "5s"
	sessionStatementTimeout = "30s"
)

// defaultMaxConns is the pool size unless the URL names one
// (pool_max_conns).
const defaultMaxConns = 32

// TCP settings of every connection, the pool's and the listener's, on
// both ends: a server that vanished without a reset fails the connection
// within about 20 s, and the pool and the listener connect again, to
// whichever server the name leads to now; the server likewise ends the
// session of an instance that vanished, and frees its locks.
const (
	tcpKeepAlive   = 5 * time.Second
	tcpUserTimeout = 15 * time.Second
	dialTimeout    = 10 * time.Second
)

// sessionIdleInTx ends a session that idles inside a transaction for
// longer: ferund never does (a transaction runs nothing slow), so only an
// instance that vanished mid-transaction meets it.
const sessionIdleInTx = "60s"

// noticeInterval is the least time between two logged WARNING notices.
const noticeInterval = 10 * time.Second

// dialer is the dialer of every connection; timeout bounds a dial, 0 for
// dialTimeout. connect_timeout in the URL bounds the whole connect anyway
// (pgconn applies it as the context's deadline).
func dialer(timeout time.Duration) *net.Dialer {
	if timeout <= 0 {
		timeout = dialTimeout
	}
	return &net.Dialer{
		Timeout:         timeout,
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: tcpKeepAlive, Interval: tcpKeepAlive, Count: 3},
		Control: func(network, _ string, raw syscall.RawConn) error {
			if strings.HasPrefix(network, "tcp") {
				setUserTimeout(raw, tcpUserTimeout)
			}
			return nil
		},
	}
}

// DB is an open database: the pool, the notification hub and the record of
// the last connection failure.
type DB struct {
	Pool *pgxpool.Pool

	log *slog.Logger
	// listenConfig is the configuration of the hub's own connection.
	listenConfig *pgx.ConnConfig
	// password gives a connection its password as it dials, nil for the
	// URL's (iamPassword).
	password func(context.Context, *pgx.ConnConfig) error
	hub      hub
	// lastFailure is the UnixNano of the last connection failure, 0 if
	// none.
	lastFailure atomic.Int64

	listenMu  sync.Mutex
	listening chan struct{} // closed while the listener listens
	listenOK  bool

	// noticeMu guards the rate limit of the logged WARNING notices:
	// when the last was logged, and how many were left out since.
	noticeMu         sync.Mutex
	noticeAt         time.Time
	noticeSuppressed int
}

// Open connects the pool. Every connection, the pool's and the
// listener's, gets the session settings lock_timeout = 5s,
// statement_timeout = 30s, TimeZone = UTC, the TCP keepalive and user
// timeout on both ends (tcp_keepalives_*, tcp_user_timeout) and
// idle_in_transaction_session_timeout = 60s; it dials with dialer, and the
// server's WARNING notices are logged (onNotice). Open does not migrate;
// see Migrate.
func Open(ctx context.Context, url string, log *slog.Logger) (*DB, error) {
	if log == nil {
		log = slog.Default()
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if !strings.Contains(url, "pool_max_conns") {
		cfg.MaxConns = defaultMaxConns
	}
	rp := cfg.ConnConfig.RuntimeParams
	rp["lock_timeout"] = sessionLockTimeout
	rp["statement_timeout"] = sessionStatementTimeout
	rp["timezone"] = "UTC"
	// The server's side of the TCP settings (ignored on a Unix socket).
	rp["tcp_keepalives_idle"] = "5"
	rp["tcp_keepalives_interval"] = "5"
	rp["tcp_keepalives_count"] = "3"
	rp["tcp_user_timeout"] = "15000"
	rp["idle_in_transaction_session_timeout"] = sessionIdleInTx
	if rp["application_name"] == "" {
		rp["application_name"] = "ferund"
	}
	d := &DB{log: log, listening: make(chan struct{})}
	d.hub.subs = map[string]map[*subscriber]struct{}{}
	if d.password, err = iamPassword(); err != nil {
		return nil, err
	}
	cfg.BeforeConnect = d.password

	// Before the copy, so that the listener's connection has both.
	cfg.ConnConfig.DialFunc = dialer(cfg.ConnConfig.ConnectTimeout).DialContext
	cfg.ConnConfig.OnNotice = d.onNotice

	lc := cfg.ConnConfig.Copy()
	lc.RuntimeParams["application_name"] = "ferund-listener"
	lc.Tracer = nil
	d.listenConfig = lc

	cfg.ConnConfig.Tracer = failureTracer{d}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	d.Pool = pool
	return d, nil
}

// iamPassword is the hook that signs a connection in with an IAM token
// of Amazon RDS (FERUN_DATABASE_AUTH=aws-iam, in AWS_REGION): the URL
// names the user and no password, and every new connection gets a fresh
// token, valid for 15 minutes, from the credentials the platform hands
// the process (awsauth). nil for the URL's own password.
func iamPassword() (func(context.Context, *pgx.ConnConfig) error, error) {
	switch mode := os.Getenv("FERUN_DATABASE_AUTH"); mode {
	case "", "password":
		return nil, nil
	case "aws-iam":
		region := os.Getenv("AWS_REGION")
		if region == "" {
			return nil, errors.New("FERUN_DATABASE_AUTH=aws-iam needs AWS_REGION")
		}
		src := awsauth.NewSource()
		return func(ctx context.Context, cc *pgx.ConnConfig) error {
			c, err := src.Get(ctx)
			if err != nil {
				return fmt.Errorf("the database's IAM token: %w", err)
			}
			cc.Password = awsauth.RDSToken(c, region, cc.Host, cc.Port, cc.User, time.Now())
			return nil
		}, nil
	default:
		return nil, fmt.Errorf("FERUN_DATABASE_AUTH must be password or aws-iam, got %q", mode)
	}
}

// onNotice logs the server's WARNING notices, at most one per
// noticeInterval (the next one says how many it left out): among them a
// commit whose wait for its synchronous standby was cancelled, done on the
// primary but perhaps not replicated.
func (d *DB) onNotice(_ *pgconn.PgConn, n *pgconn.Notice) {
	if n == nil || n.SeverityUnlocalized != "WARNING" {
		return
	}
	d.noticeMu.Lock()
	defer d.noticeMu.Unlock()
	if !d.noticeAt.IsZero() && time.Since(d.noticeAt) < noticeInterval {
		d.noticeSuppressed++
		return
	}
	d.log.Warn("database warning", "message", n.Message, "detail", n.Detail, "code", n.Code, "left_out", d.noticeSuppressed)
	d.noticeAt, d.noticeSuppressed = time.Now(), 0
}

// Close closes the pool. The listener ends with its own context.
func (d *DB) Close() {
	d.Pool.Close()
}

// LastFailure returns when the database last failed for a connection
// reason: a statement that failed on its connection, a failed acquire or
// connect, a lost listener connection. Zero if it never did.
func (d *DB) LastFailure() time.Time {
	n := d.lastFailure.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (d *DB) noteFailure() {
	d.lastFailure.Store(time.Now().UnixNano())
}

// maxTxRetries is how often Tx runs a function again after a retryable
// failure.
const maxTxRetries = 5

// Tx runs fn in a transaction and commits it. When the transaction fails
// with a serialization failure (40001), a deadlock (40P01) or a lock
// timeout (55P03), the whole function runs again, after a jittered backoff,
// up to 5 times. fn must not call a node or wait for anything slow, and
// must be safe to run more than once.
func (d *DB) Tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	backoff := 10 * time.Millisecond
	for attempt := 0; ; attempt++ {
		err := d.tx(ctx, fn)
		if err == nil || !Retryable(err) || attempt >= maxTxRetries {
			return err
		}
		// Full jitter: a random wait up to the backoff, so that the
		// transactions that collided do not collide again.
		wait := time.Duration(rand.Int64N(int64(backoff))) + time.Millisecond
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		backoff = min(backoff*2, 500*time.Millisecond)
	}
}

func (d *DB) tx(ctx context.Context, fn func(tx pgx.Tx) error) (err error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// Roll back on a context of its own, so that a cancelled
			// caller does not cost the connection.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = tx.Rollback(rctx)
			cancel()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Retryable reports whether err is a serialization failure, a deadlock or
// a lock timeout: a transaction failing with one of them may succeed when
// run again.
func Retryable(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Code {
	case "40001", "40P01", "55P03":
		return true
	}
	return false
}

// Notify sends payload on channel through q. Inside a transaction it is
// delivered when the transaction commits, and not at all if it rolls back.
func Notify(ctx context.Context, q Querier, channel, payload string) error {
	if _, err := q.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload); err != nil {
		return fmt.Errorf("notify %s: %w", channel, err)
	}
	return nil
}

// connectionFailure reports whether err, from a statement, an acquire or a
// connect made with ctx, failed for a reason of the connection or the
// server rather than of the statement or the caller.
func connectionFailure(ctx context.Context, err error) bool {
	if err == nil || (ctx != nil && ctx.Err() != nil) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		// connection_exception, admin_shutdown, crash_shutdown,
		// cannot_connect_now, too_many_connections.
		return strings.HasPrefix(pe.Code, "08") || pe.Code == "57P01" || pe.Code == "57P02" ||
			pe.Code == "57P03" || pe.Code == "53300"
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return strings.Contains(err.Error(), "conn closed")
}

// failureTracer records connection failures of the pool's statements,
// acquires and connects in DB.lastFailure.
type failureTracer struct{ d *DB }

func (t failureTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (t failureTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if connectionFailure(ctx, data.Err) {
		t.d.noteFailure()
	}
}

func (t failureTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return ctx
}

func (t failureTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if connectionFailure(ctx, data.Err) {
		t.d.noteFailure()
	}
}

func (t failureTracer) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	return ctx
}

func (t failureTracer) TraceConnectEnd(ctx context.Context, data pgx.TraceConnectEndData) {
	if connectionFailure(ctx, data.Err) {
		t.d.noteFailure()
	}
}
