// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The audit records of a pod's requests through its HTTP bindings (ADR
// 0028 §6), kept in activity with source authn: feos-authn's "http"
// record of each request (docs/architecture/sink.md), as feosd publishes
// it, of kind http_request, or of kind http, its type's word.
const (
	KindHTTPRequest = "http_request"
	KindHTTP        = "http"
)

// The refusal codes of feos-authn (error.code of a record) that say the
// upstream could not be reached or its TLS failed, and that no OAuth
// token could be had. Every other refusal is the pod's or the binding's
// (denied_path, unknown_binding, rate_limited, misdirected, …) and counts
// for no condition of the upstream's.
var (
	egressUnreachable = []string{"unreachable", "tls"}
	egressNoToken     = []string{"credential_unavailable"}
)

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

// EgressStat is what the audit records of an environment's pods say of
// one binding over a window: its requests that reached the upstream or
// were meant to (Requests: the upstream's answers, feos-authn's
// unreachable, tls and credential_unavailable; not its refusals for the
// pod's own mistakes), those the upstream refused with 401 or 403
// (Refused), those it answered with 429 or 5xx or that could not reach
// it (Failing), and those refused because no token could be had
// (TokenFailed).
type EgressStat struct {
	Binding                                 string
	Requests, Refused, Failing, TokenFailed int64
}

// egressClass is the SQL of the classes of a record a: its status (0 when
// none), whether feos-authn refused it itself, its refusal's code.
const egressClass = `CASE WHEN a.detail->>'status' ~ '^[0-9]{1,3}$' THEN (a.detail->>'status')::int ELSE 0 END`

// EgressStats returns, by binding, the requests of the environment's pods
// over the last window by the database's clock (their records' ts_ns).
// Records of pods whose rows are gone do not count.
func (s *Store) EgressStats(ctx context.Context, envID uuid.UUID, window time.Duration) ([]EgressStat, error) {
	rows, err := s.q.Query(ctx, `SELECT binding, count(*),
			count(*) FILTER (WHERE upstream AND status IN (401, 403)),
			count(*) FILTER (WHERE (upstream AND (status = 429 OR status >= 500)) OR code = ANY($4)),
			count(*) FILTER (WHERE code = ANY($5))
		FROM (SELECT coalesce(a.detail->>'binding', '') AS binding, `+egressClass+` AS status,
				coalesce(jsonb_typeof(a.detail->'error'), 'null') = 'null' AS upstream,
				coalesce(a.detail->'error'->>'code', '') AS code
			FROM activity a JOIN sandboxes s ON s.id = a.sandbox_id
			WHERE s.environment_id = $1 AND a.source = 'authn' AND a.kind = ANY($2)
				AND coalesce(a.detail->>'type', 'http') = 'http'
				AND a.day >= (now() AT TIME ZONE 'utc')::date - 1
				AND a.ts_ns >= (extract(epoch FROM now()) * 1e9)::bigint - $3) r
		WHERE upstream OR code = ANY($4) OR code = ANY($5)
		GROUP BY binding ORDER BY binding`, envID, []string{KindHTTPRequest, KindHTTP}, window.Nanoseconds(),
		egressUnreachable, egressNoToken)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EgressStat, error) {
		var st EgressStat
		err := r.Scan(&st.Binding, &st.Requests, &st.Refused, &st.Failing, &st.TokenFailed)
		return st, err
	})
}

// EgressRecords returns the newest limit audit records of the
// environment's pods' requests through binding over the last window, newest
// first, as their egress JSON with the pod's ID as "sandbox".
func (s *Store) EgressRecords(ctx context.Context, envID uuid.UUID, binding string, window time.Duration, limit int) (
	[]json.RawMessage, error) {
	rows, err := s.q.Query(ctx, `SELECT a.detail || jsonb_build_object('sandbox', a.sandbox_id)
		FROM activity a JOIN sandboxes s ON s.id = a.sandbox_id
		WHERE s.environment_id = $1 AND a.source = 'authn' AND a.kind = ANY($2) AND a.detail->>'binding' = $3
			AND coalesce(a.detail->>'type', 'http') = 'http'
			AND a.day >= (now() AT TIME ZONE 'utc')::date - 1
			AND a.ts_ns >= (extract(epoch FROM now()) * 1e9)::bigint - $4
		ORDER BY a.ts_ns DESC, a.id DESC LIMIT $5`, envID, []string{KindHTTPRequest, KindHTTP}, binding,
		window.Nanoseconds(), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (json.RawMessage, error) {
		var b []byte
		err := r.Scan(&b)
		return json.RawMessage(b), err
	})
}
