// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// HTTPBinding is a row of http_bindings: an HTTP API a sandbox calls by a
// name (ADR 0028), its credential sealed at rest.
type HTTPBinding struct {
	Name          string
	Upstream      string // https://host[:port], canonical
	Mode          string // name, intercept
	CAPEM         string
	PathPrefixes  []string
	ForwardedHost bool
	RatePerSecond int
	RateBurst     int
	Kind          string // headers, basic, oauth2, clientCertificate
	Sealed        []byte // 0x01 || nonce(12) || AES-256-GCM(credential JSON)
	CreatedAt     time.Time
}

// HTTPBindings returns a sandbox's HTTP set: its version and its bindings
// by name, read in one statement; model.ErrNotFound when the sandbox does
// not exist.
func (s *Store) HTTPBindings(ctx context.Context, sandboxID podid.ID) (string, []HTTPBinding, error) {
	rows, err := s.q.Query(ctx, `SELECT s.http_version, b.name, b.upstream, b.mode, b.ca_pem, b.path_prefixes,
			b.forwarded_host, b.rate_per_second, b.rate_burst, b.credential_kind, b.sealed, b.created_at
		FROM sandboxes s LEFT JOIN http_bindings b ON b.sandbox_id = s.id WHERE s.id = $1 ORDER BY b.name`, sandboxID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var version string
	found := false
	bs := []HTTPBinding{}
	for rows.Next() {
		found = true
		var name, upstream, mode, ca, kind *string
		var prefixes []string
		var fwd *bool
		var rate, burst *int
		var sealed []byte
		var created *time.Time
		if err := rows.Scan(&version, &name, &upstream, &mode, &ca, &prefixes, &fwd, &rate, &burst, &kind, &sealed,
			&created); err != nil {
			return "", nil, err
		}
		if name == nil {
			continue
		}
		bs = append(bs, HTTPBinding{Name: *name, Upstream: *upstream, Mode: *mode, CAPEM: *ca,
			PathPrefixes: nonNil(prefixes), ForwardedHost: *fwd, RatePerSecond: *rate, RateBurst: *burst, Kind: *kind,
			Sealed: sealed, CreatedAt: timeOf(created)})
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if !found {
		return "", nil, model.ErrNotFound
	}
	return version, bs, nil
}

// SetHTTPBindings replaces a sandbox's HTTP set with bs and version ("" and
// none to delete it); model.ErrNotFound when the sandbox does not exist.
// The caller holds the sandbox's row FOR UPDATE.
func (s *Store) SetHTTPBindings(ctx context.Context, sandboxID podid.ID, version string, bs []HTTPBinding) error {
	tag, err := s.q.Exec(ctx, "UPDATE sandboxes SET http_version = $2 WHERE id = $1", sandboxID, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return model.ErrNotFound
	}
	if _, err := s.q.Exec(ctx, "DELETE FROM http_bindings WHERE sandbox_id = $1", sandboxID); err != nil {
		return err
	}
	for _, b := range bs {
		if _, err := s.q.Exec(ctx, `INSERT INTO http_bindings (sandbox_id, name, upstream, mode, ca_pem, path_prefixes,
				forwarded_host, rate_per_second, rate_burst, credential_kind, sealed)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			sandboxID, b.Name, b.Upstream, b.Mode, b.CAPEM, nonNil(b.PathPrefixes), b.ForwardedHost, b.RatePerSecond,
			b.RateBurst, b.Kind, b.Sealed); err != nil {
			return err
		}
	}
	return nil
}

// InterceptCA is a row of pod_intercept_cas: one of a pod's interception
// CAs (ADR 0028 §5), its key sealed at rest.
type InterceptCA struct {
	Seq       int
	Hosts     []string // sorted, lowercase
	CertPEM   string
	SealedKey []byte
	CreatedAt time.Time
}

// InterceptCAs returns a pod's interception CAs, oldest first.
func (s *Store) InterceptCAs(ctx context.Context, sandboxID podid.ID) ([]InterceptCA, error) {
	rows, err := s.q.Query(ctx, `SELECT seq, hosts, cert_pem, sealed_key, created_at FROM pod_intercept_cas
		WHERE sandbox_id = $1 ORDER BY seq`, sandboxID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (InterceptCA, error) {
		var c InterceptCA
		err := r.Scan(&c.Seq, &c.Hosts, &c.CertPEM, &c.SealedKey, &c.CreatedAt)
		c.Hosts = nonNil(c.Hosts)
		return c, err
	})
}

// InsertInterceptCA adds a pod's CA at c.Seq unless one is there (a
// concurrent writer's): inserted false then. model.ErrNotFound when the
// sandbox does not exist.
func (s *Store) InsertInterceptCA(ctx context.Context, sandboxID podid.ID, c InterceptCA) (bool, error) {
	tag, err := s.q.Exec(ctx, `INSERT INTO pod_intercept_cas (sandbox_id, seq, hosts, cert_pem, sealed_key)
		SELECT id, $2, $3, $4, $5 FROM sandboxes WHERE id = $1
		ON CONFLICT (sandbox_id, seq) DO NOTHING`, sandboxID, c.Seq, nonNil(c.Hosts), c.CertPEM, c.SealedKey)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM sandboxes WHERE id = $1)", sandboxID).Scan(&exists); err != nil {
			return false, err
		}
		if !exists {
			return false, model.ErrNotFound
		}
		return false, nil
	}
	return true, nil
}

// DeleteInterceptCAsBefore deletes a pod's CAs older than seq.
func (s *Store) DeleteInterceptCAsBefore(ctx context.Context, sandboxID podid.ID, seq int) error {
	_, err := s.q.Exec(ctx, "DELETE FROM pod_intercept_cas WHERE sandbox_id = $1 AND seq < $2", sandboxID, seq)
	return err
}

// SetHTTPPushed records that the pod of the row's incarnation inc took
// the clear part of version v of its HTTP bindings. A row of another
// incarnation, or gone, is left: false.
func (s *Store) SetHTTPPushed(ctx context.Context, sandboxID podid.ID, inc uuid.UUID, v string) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE sandboxes SET http_pushed = $3
		WHERE id = $1 AND incarnation = $2 AND http_pushed IS DISTINCT FROM $3`, sandboxID, inc, v)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ResetHTTPPushed records that the pod of the row's incarnation inc took
// no clear part yet that a node confirmed: after its create, which a feosd
// from before ADR 0028 takes without the spec's part. A row of another
// incarnation, or gone, is left.
func (s *Store) ResetHTTPPushed(ctx context.Context, sandboxID podid.ID, inc uuid.UUID) error {
	_, err := s.q.Exec(ctx, `UPDATE sandboxes SET http_pushed = ''
		WHERE id = $1 AND incarnation = $2 AND http_pushed <> ''`, sandboxID, inc)
	return err
}

// HTTPPushCandidates returns the rows on nodes whose pods may hold another
// clear part of their HTTP bindings than the row's: a pod on one of nodes
// (running, paused or stopped, materialised, no op), with an HTTP set of
// its own, a template that names bindings, or a part taken before; at
// most limit, in a random order, so that a pass of many reaches each in
// turn. The caller computes each one's part and compares its version with
// HTTPPushed.
func (s *Store) HTTPPushCandidates(ctx context.Context, nodes []string, limit int) ([]Sandbox, error) {
	return collectSandboxes(s.q.Query(ctx, "SELECT "+sandboxColumns("s")+` FROM sandboxes s
		WHERE s.node = ANY($1) AND s.materialised AND s.op = ''
			AND s.client_state IN ('running', 'paused', 'stopped')
			AND (s.http_version <> '' OR s.http_pushed <> '' OR s.pod_template ? 'httpBindings')
		ORDER BY random() LIMIT $2`, nonNil(nodes), limit))
}

// EnvInstanceDeployment returns the deployment an environment's pod
// belongs to: its instance's (env_instances), else its job run's
// (env_job_runs); ErrNoRow when it is neither.
func (s *Store) EnvInstanceDeployment(ctx context.Context, sandboxID podid.ID) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, `SELECT deployment FROM env_instances WHERE sandbox_id = $1
		UNION ALL SELECT deployment FROM env_job_runs WHERE sandbox_id = $1 LIMIT 1`, sandboxID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoRow
	}
	return n, err
}
