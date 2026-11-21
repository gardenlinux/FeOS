// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// retentionNs is a stream's retention in SQL, in ns: its
// ferun.retention_days or $1, at most $2.
const retentionNs = `least(coalesce(s.retention_days::bigint * 86400000000000, $1::bigint), $2::bigint)`

// nowNs is the database's clock in Unix ns.
const nowNs = `(extract(epoch FROM now()) * 1000000000)::bigint`

// RunSweep runs the sweep every SweepInterval, the first at once, until
// ctx ends.
func (s *Store) RunSweep(ctx context.Context) error {
	for {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("retention sweep", "err", err)
		}
		t := time.NewTimer(s.cfg.SweepInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// Sweep deletes, by the database's clock, the streams whose last line is
// past their retention (the index drops their lines by its own retention,
// at least as long: ADR 0026 §10; a read finds no line of a stream that
// is gone) and the usage of days before yesterday. Several instances may
// sweep at once.
func (s *Store) Sweep(ctx context.Context) error {
	def, maxRet := s.cfg.Retention.Nanoseconds(), s.cfg.RetentionMax.Nanoseconds()
	var errs []error
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM log_streams s
		WHERE coalesce(s.last_ns, (extract(epoch FROM s.created_at) * 1000000000)::bigint) < `+nowNs+` - `+retentionNs,
		def, maxRet)
	if err != nil {
		errs = append(errs, fmt.Errorf("expired streams: %w", err))
	} else if tag.RowsAffected() > 0 {
		s.log.Info("deleted expired streams", "streams", tag.RowsAffected())
	}
	if _, err := s.db.Pool.Exec(ctx, `DELETE FROM log_usage WHERE day < (now() AT TIME ZONE 'utc')::date - 1`); err != nil {
		errs = append(errs, fmt.Errorf("usage: %w", err))
	}
	return errors.Join(errs...)
}

// DeleteStreams deletes a project's streams of an environment or a job:
// it leaves a delete task of them in every log index (Quickwit removes
// their lines when it rewrites their splits; until then no read finds
// them, their streams being gone), then deletes the streams with their
// hot lines, each under its lock, which keeps the cold stage from moving
// its mark meanwhile. It returns how many streams went.
func (s *Store) DeleteStreams(ctx context.Context, project, environment, job string) (int, error) {
	sel := Selector{Environment: environment, Job: job}
	streams, err := selectStreams(ctx, s.db.Pool, project, streamFilter{sel: sel, limit: maxStreamsRead})
	if err != nil {
		return 0, err
	}
	if len(streams) > 0 {
		ids, err := s.index.Indexes(ctx, s.cfg.IndexPrefix+"-")
		if err != nil {
			return 0, fmt.Errorf("index: %w", err)
		}
		for _, id := range ids {
			if err := s.index.Delete(ctx, id, quickwit.Query{Project: project, Environment: environment, Job: job}); err != nil {
				return 0, fmt.Errorf("index %s: delete: %w", id, err)
			}
		}
	}
	gone := 0
	for _, st := range streams {
		tag, err := s.db.Pool.Exec(ctx, `DELETE FROM log_streams WHERE id = $1 AND project = $2`, st.ID, project)
		if err != nil {
			return gone, err
		}
		gone += int(tag.RowsAffected())
	}
	return gone, nil
}
