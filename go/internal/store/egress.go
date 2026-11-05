// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// A pod's requests through its HTTP bindings (ADR 0028 §6): feos-authn's
// "http" record of each request (docs/architecture/sink.md), which feosd
// publishes as the pod's audit event of kind http_request, or of kind http,
// its type's word, and its "http_end" of kind http_end. The records are the
// sink's (ADR 0026): ferund keeps none of them as activity. For the
// bindings' incidents it counts each pod's requests per binding and minute
// of the database's clock (http_egress) and keeps the newest EgressKeep
// records of each (http_egress_records), both swept after a few minutes.
const (
	KindHTTPRequest = "http_request"
	KindHTTP        = "http"
	KindHTTPEnd     = "http_end"
)

// EgressKeep is how many records of a pod's binding http_egress_records
// keeps, the newest.
const EgressKeep = 10

// The refusal codes of feos-authn (error.code of a record) that say the
// upstream could not be reached or its TLS failed, and that no OAuth
// token could be had. Every other refusal is the pod's or the binding's
// (denied_path, unknown_binding, rate_limited, misdirected, …) and counts
// for no condition of the upstream's.
var (
	egressUnreachable = []string{"unreachable", "tls"}
	egressNoToken     = []string{"credential_unavailable"}
)

// EgressClass is how a record of a request counts for its binding: whether
// it counts at all (the upstream answered it, or feos-authn refused it
// because the upstream could not be reached or no token could be had), and
// as which of the counts of EgressStat.
type EgressClass struct {
	Counted, Refused, Failing, TokenFailed bool
}

// ClassifyEgress classes a record by the status it was answered with (0
// when none) and, for a refusal of feos-authn's own, its code; refusal
// false is the upstream's answer.
func ClassifyEgress(status int, refusal bool, code string) EgressClass {
	if !refusal {
		return EgressClass{Counted: true, Refused: status == 401 || status == 403,
			Failing: status == 429 || status >= 500}
	}
	switch {
	case slices.Contains(egressUnreachable, code):
		return EgressClass{Counted: true, Failing: true}
	case slices.Contains(egressNoToken, code):
		return EgressClass{Counted: true, TokenFailed: true}
	}
	return EgressClass{}
}

// EgressCount is what one batch of a node's events adds of one pod's
// requests through one binding: the counts of EgressStat, and its newest
// records (at most EgressKeep, oldest first), each the record's JSON.
type EgressCount struct {
	SandboxID                               podid.ID
	Binding                                 string
	Requests, Refused, Failing, TokenFailed int64
	Records                                 []json.RawMessage
}

// AddEgress adds counts to the minute of the database's clock and keeps
// the newest EgressKeep records of each pod's binding. It writes in the
// order of (sandbox, binding), so that two batches do not deadlock.
func (s *Store) AddEgress(ctx context.Context, counts []EgressCount) error {
	sorted := slices.Clone(counts)
	slices.SortFunc(sorted, func(a, b EgressCount) int {
		return cmp.Or(cmp.Compare(a.SandboxID, b.SandboxID), cmp.Compare(a.Binding, b.Binding))
	})
	for _, c := range sorted {
		if _, err := s.q.Exec(ctx, `INSERT INTO http_egress (sandbox_id, binding, minute, requests, refused, failing,
				token_failed)
			VALUES ($1, $2, date_trunc('minute', now()), $3, $4, $5, $6)
			ON CONFLICT (sandbox_id, binding, minute) DO UPDATE SET requests = http_egress.requests + EXCLUDED.requests,
				refused = http_egress.refused + EXCLUDED.refused, failing = http_egress.failing + EXCLUDED.failing,
				token_failed = http_egress.token_failed + EXCLUDED.token_failed`,
			c.SandboxID, c.Binding, c.Requests, c.Refused, c.Failing, c.TokenFailed); err != nil {
			return fmt.Errorf("count the requests of %s through %s: %w", c.SandboxID, c.Binding, err)
		}
		if len(c.Records) == 0 {
			continue
		}
		records := make([]string, 0, len(c.Records))
		for _, r := range c.Records {
			records = append(records, string(r))
		}
		if _, err := s.q.Exec(ctx, `INSERT INTO http_egress_records (sandbox_id, binding, detail)
			SELECT $1, $2, r::jsonb FROM unnest($3::text[]) WITH ORDINALITY AS u(r, n) ORDER BY n`,
			c.SandboxID, c.Binding, records); err != nil {
			return fmt.Errorf("keep the records of %s through %s: %w", c.SandboxID, c.Binding, err)
		}
		if _, err := s.q.Exec(ctx, `DELETE FROM http_egress_records WHERE sandbox_id = $1 AND binding = $2
			AND id < (SELECT id FROM http_egress_records WHERE sandbox_id = $1 AND binding = $2
				ORDER BY id DESC OFFSET $3 LIMIT 1)`, c.SandboxID, c.Binding, EgressKeep-1); err != nil {
			return fmt.Errorf("trim the records of %s through %s: %w", c.SandboxID, c.Binding, err)
		}
	}
	return nil
}

// SweepEgress deletes the counts and records older than keep by the
// database's clock.
func (s *Store) SweepEgress(ctx context.Context, keep time.Duration) error {
	if _, err := s.q.Exec(ctx, "DELETE FROM http_egress WHERE minute < now() - $1::interval", interval(keep)); err != nil {
		return fmt.Errorf("sweep http_egress: %w", err)
	}
	if _, err := s.q.Exec(ctx, "DELETE FROM http_egress_records WHERE arrived_at < now() - $1::interval",
		interval(keep)); err != nil {
		return fmt.Errorf("sweep http_egress_records: %w", err)
	}
	return nil
}

// BindingEnv is an environment whose bindings the controller watches: its
// latest deployment and the bindings it has.
type BindingEnv struct {
	ID         uuid.UUID
	Deployment int
	Bindings   []string
}

// BindingEnvs returns the environments that are not being deleted and
// have bindings in their latest deployment or an open incident of one.
func (s *Store) BindingEnvs(ctx context.Context) ([]BindingEnv, error) {
	rows, err := s.q.Query(ctx, `SELECT e.id, e.deployment,
			coalesce((SELECT array_agg(k ORDER BY k) FROM jsonb_object_keys(coalesce(d.spec->'bindings', '{}'::jsonb)) k),
				'{}')
		FROM environments e LEFT JOIN env_deployments d ON d.env_id = e.id AND d.number = e.deployment
		WHERE NOT e.deleting AND e.deployment > 0 AND (d.spec ? 'bindings' OR EXISTS (SELECT 1 FROM env_incidents i
			WHERE i.env_id = e.id AND i.closed_at IS NULL AND i.object LIKE 'binding/%'))
		ORDER BY e.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (BindingEnv, error) {
		var b BindingEnv
		err := r.Scan(&b.ID, &b.Deployment, &b.Bindings)
		b.Bindings = nonNil(b.Bindings)
		return b, err
	})
}

// EgressStat is what an environment's pods' requests through one binding
// were over a window: those that reached the upstream or were meant to
// (Requests: the upstream's answers, feos-authn's unreachable, tls and
// credential_unavailable; not its refusals for the pod's own mistakes),
// those the upstream refused with 401 or 403 (Refused), those it answered
// with 429 or 5xx or that could not reach it (Failing), and those refused
// because no token could be had (TokenFailed).
type EgressStat struct {
	Binding                                 string
	Requests, Refused, Failing, TokenFailed int64
}

// EgressStats returns, by binding, the requests of the environment's pods
// that arrived over the last window by the database's clock: the counts of
// the current minute and of the minutes that began within the window
// before it. Pods whose rows are gone do not count.
func (s *Store) EgressStats(ctx context.Context, envID uuid.UUID, window time.Duration) ([]EgressStat, error) {
	rows, err := s.q.Query(ctx, `SELECT e.binding, sum(e.requests)::bigint, sum(e.refused)::bigint,
			sum(e.failing)::bigint, sum(e.token_failed)::bigint
		FROM http_egress e JOIN sandboxes s ON s.id = e.sandbox_id
		WHERE s.environment_id = $1 AND e.minute >= date_trunc('minute', now()) - $2::interval
		GROUP BY e.binding ORDER BY e.binding`, envID, interval(window))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EgressStat, error) {
		var st EgressStat
		err := r.Scan(&st.Binding, &st.Requests, &st.Refused, &st.Failing, &st.TokenFailed)
		return st, err
	})
}

// EgressRecords returns the newest limit records of the environment's
// pods' requests through binding that arrived over the last window by the
// database's clock, newest first, each with the pod's ID as "sandbox".
func (s *Store) EgressRecords(ctx context.Context, envID uuid.UUID, binding string, window time.Duration, limit int) (
	[]json.RawMessage, error) {
	rows, err := s.q.Query(ctx, `SELECT r.detail || jsonb_build_object('sandbox', r.sandbox_id)
		FROM http_egress_records r JOIN sandboxes s ON s.id = r.sandbox_id
		WHERE s.environment_id = $1 AND r.binding = $2 AND r.arrived_at >= now() - $3::interval
		ORDER BY r.id DESC LIMIT $4`, envID, binding, interval(window), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (json.RawMessage, error) {
		var b []byte
		err := r.Scan(&b)
		return json.RawMessage(b), err
	})
}
