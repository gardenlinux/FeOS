// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/sink/kafka"
)

// Store is the sink's log streams: the gate of their ingest into the
// topics, the reads from the index and the topics' tail, and retention
// (ADR 0026 §11). The database keeps the streams, not their lines.
type Store struct {
	db    *db.DB
	index Index
	pipes *pipes
	cfg   Config
	log   *slog.Logger
	// scans bounds the reads that scan at once.
	scans chan struct{}
}

// maxScans is how many reads scan at once; more wait.
const maxScans = 8

// NewStore returns the store over d, the log indexes and the topics.
func NewStore(d *db.DB, index Index, log Log, cfg Config, logger *slog.Logger) *Store {
	return &Store{db: d, index: index, pipes: newPipes(log, index, cfg), cfg: cfg, log: logger,
		scans: make(chan struct{}, maxScans)}
}

// StreamKey is a stream's key as the node names it.
type StreamKey struct {
	Pod, Container, LogID string
}

func (k StreamKey) less(o StreamKey) bool {
	if k.Pod != o.Pod {
		return k.Pod < o.Pod
	}
	if k.Container != o.Container {
		return k.Container < o.Container
	}
	return k.LogID < o.LogID
}

// Labels are a stream's labels, set by ferund through the node (sink.md,
// The OTLP shape) or by the ingress; Attrs are all its resource
// attributes. Node is "" for an ingress's stream; Source is
// SourceIngress or SourceContainer.
type Labels struct {
	Tenant, Node, Instance, Environment, Kind, Object, Job string
	// Deployment is ferun.deployment (or its earlier word ferun.release);
	// it is stored in the column release, as Doc keeps the field release.
	Replica, Deployment, Attempt int
	// RetentionDays is ferun.retention_days, 0 for none.
	RetentionDays int
	Source        string
	Attrs         map[string]string
}

// Line is one stored line. Stream is the stream's row ID; Level a bit;
// Partial says the next line of its source goes on with it, Cont that it
// goes on with the last one; Trace its trace ID, the record's own or the
// one its text names.
type Line struct {
	Stream        int64
	Off, TS       int64
	Source        string
	Level         int
	Partial, Cont bool
	Text          string
	Trace         string
}

// StreamBatch is one stream's part of an Export: its key, labels and
// lines, and Next, the offset after its last line when the node said it
// (log.next_offset), else -1.
type StreamBatch struct {
	Key    StreamKey
	Labels Labels
	Lines  []Line
	Next   int64
}

// ErrQuota is Ingest's refusal of a tenant over its quota; the other
// tenants' lines of the batch are stored.
var ErrQuota = errors.New("over the tenant's log quota")

// newSID returns a new stream ID, s_ and 16 hex digits.
func newSID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "s_" + hex.EncodeToString(b), nil
}

// Ingest stores a batch: in one transaction, each stream's row (its
// labels the first write's) locked, its new lines (above its last
// offset) produced to its index's topic, then the stream's counters and
// times and the tenants' usage of the day, and the commit. A stream of a
// tenant over its quota is left out and Ingest returns ErrQuota (wrapped)
// after the commit. A failed produce stores nothing: the node sends
// again.
func (s *Store) Ingest(ctx context.Context, batch []StreamBatch) error {
	sort.Slice(batch, func(i, j int) bool { return batch[i].Key.less(batch[j].Key) })
	var over []string
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		over = over[:0]
		refused := map[string]bool{}
		if s.cfg.Quota > 0 {
			for _, b := range batch {
				t := b.Labels.Tenant
				if _, seen := refused[t]; seen {
					continue
				}
				var used int64
				err := tx.QueryRow(ctx, `SELECT bytes FROM log_usage WHERE tenant = $1 AND day = (now() AT TIME ZONE 'utc')::date`,
					t).Scan(&used)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				refused[t] = used >= s.cfg.Quota
				if refused[t] {
					over = append(over, t)
				}
			}
		}
		usage := map[string]int64{}
		recs := map[string][]kafka.Record{}
		var counts []streamCount
		for i := range batch {
			b := &batch[i]
			if refused[b.Labels.Tenant] {
				continue
			}
			c, rs, err := s.ingestStream(ctx, tx, b)
			if err != nil {
				return err
			}
			counts = append(counts, c)
			recs[c.topic] = append(recs[c.topic], rs...)
			usage[b.Labels.Tenant] += c.bytes
		}
		for topic, rs := range recs {
			if err := s.pipes.produce(ctx, topic, rs); err != nil {
				return err
			}
		}
		for _, c := range counts {
			if _, err := tx.Exec(ctx, `UPDATE log_streams SET lines = lines + $2, bytes = bytes + $3,
					first_ns = least(first_ns, $4), last_ns = greatest(last_ns, $5),
					next_offset = greatest(next_offset, $6), last_off = greatest(last_off, $7), open_sources = $8
				WHERE id = $1`, c.id, c.lines, c.bytes, c.minTS, c.maxTS, c.next, c.last, c.open); err != nil {
				return fmt.Errorf("stream counters: %w", err)
			}
		}
		for t, n := range usage {
			if n == 0 {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO log_usage (tenant, day, bytes) VALUES ($1, (now() AT TIME ZONE 'utc')::date, $2)
				ON CONFLICT (tenant, day) DO UPDATE SET bytes = log_usage.bytes + EXCLUDED.bytes`, t, n); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(over) > 0 {
		return fmt.Errorf("%w: %v", ErrQuota, over)
	}
	return nil
}

// streamCount is what a batch adds to a stream's row.
type streamCount struct {
	id           int64
	topic        string
	lines, bytes int64
	minTS, maxTS *int64
	next, last   int64
	open         int16
}

// ingestStream locks one stream's row (made with its labels if new) and
// returns its new lines as records of its topic, and what they add to the
// row.
func (s *Store) ingestStream(ctx context.Context, tx pgx.Tx, b *StreamBatch) (streamCount, []kafka.Record, error) {
	sid, err := newSID()
	if err != nil {
		return streamCount{}, nil, err
	}
	l := b.Labels
	var retention *int
	if l.RetentionDays > 0 {
		retention = &l.RetentionDays
	}
	source := l.Source
	if source == "" {
		source = SourceContainer
	}
	if _, err := tx.Exec(ctx, `INSERT INTO log_streams (sid, pod, container, log_id, tenant, node, instance, environment,
			kind, object, replica, release, job, attempt, retention_days, labels, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (pod, container, log_id) DO NOTHING`,
		sid, b.Key.Pod, b.Key.Container, b.Key.LogID, l.Tenant, l.Node, l.Instance, l.Environment,
		l.Kind, l.Object, l.Replica, l.Deployment, l.Job, l.Attempt, retention, l.Attrs, source); err != nil {
		return streamCount{}, nil, fmt.Errorf("stream: %w", err)
	}
	var open int16
	var keptDays *int
	row := tx.QueryRow(ctx, `SELECT `+streamCols+`, open_sources, retention_days FROM log_streams
		WHERE pod = $1 AND container = $2 AND log_id = $3 FOR UPDATE`, b.Key.Pod, b.Key.Container, b.Key.LogID)
	st, err := scanStreamWith(row, &open, &keptDays)
	if err != nil {
		return streamCount{}, nil, fmt.Errorf("lock stream: %w", err)
	}
	topic, _ := s.indexOf(keptDays)
	n, err := s.pipes.open(ctx, topic)
	if err != nil {
		return streamCount{}, nil, err
	}
	// A node sends a stream's lines in the order of their offsets, so a
	// line at or below the last stored is stored; a line goes on with the
	// last of its source when that is partial.
	slices.SortStableFunc(b.Lines, func(x, y Line) int { return cmpInt(x.Off, y.Off) })
	c := streamCount{id: st.ID, topic: topic, next: b.Next, last: -1}
	from := st.LastOff
	var recs []kafka.Record
	for _, ln := range b.Lines {
		c.next = max(c.next, ln.Off)
		c.last = max(c.last, ln.Off)
		if ln.Off <= from {
			continue
		}
		from = ln.Off
		bit := sourceBit(ln.Source)
		ln.Cont = open&bit != 0
		if ln.Partial {
			open |= bit
		} else {
			open &^= bit
		}
		r, err := recordOf(ln, st, n)
		if err != nil {
			return streamCount{}, nil, err
		}
		recs = append(recs, r)
		c.lines++
		c.bytes += int64(len(ln.Text))
		if c.minTS == nil || ln.TS < *c.minTS {
			c.minTS = &ln.TS
		}
		if c.maxTS == nil || ln.TS > *c.maxTS {
			c.maxTS = &ln.TS
		}
	}
	c.open = open
	return c, recs, nil
}

// sourceBit is a source's bit in log_streams.open_sources.
func sourceBit(source string) int16 {
	switch source {
	case "stdout":
		return 1
	case "stderr":
		return 2
	}
	return 4
}

// Positions returns each stream's next offset, 0 for one not known, in
// the order of keys.
func (s *Store) Positions(ctx context.Context, keys []StreamKey) ([]uint64, error) {
	pods, conts, ids := splitKeys(keys)
	rows, err := s.db.Pool.Query(ctx, `SELECT s.pod, s.container, s.log_id, s.next_offset FROM log_streams s
		JOIN unnest($1::text[], $2::text[], $3::text[]) AS k(pod, container, log_id)
			ON s.pod = k.pod AND s.container = k.container AND s.log_id = k.log_id`, pods, conts, ids)
	if err != nil {
		return nil, err
	}
	got := map[StreamKey]int64{}
	for rows.Next() {
		var k StreamKey
		var next int64
		if err := rows.Scan(&k.Pod, &k.Container, &k.LogID, &next); err != nil {
			rows.Close()
			return nil, err
		}
		got[k] = next
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]uint64, len(keys))
	for i, k := range keys {
		out[i] = uint64(max(got[k], 0))
	}
	return out, nil
}

// EndStreams marks the streams ended at endNs (Unix ns; 0: now, DB
// clock); an end once set stays. Streams not known are ignored.
func (s *Store) EndStreams(ctx context.Context, keys []StreamKey, endNs int64) error {
	pods, conts, ids := splitKeys(keys)
	_, err := s.db.Pool.Exec(ctx, `UPDATE log_streams s SET ended_at = coalesce(s.ended_at,
			CASE WHEN $4::bigint = 0 THEN now() ELSE to_timestamp($4::bigint / 1e9) END)
		FROM unnest($1::text[], $2::text[], $3::text[]) AS k(pod, container, log_id)
		WHERE s.pod = k.pod AND s.container = k.container AND s.log_id = k.log_id`, pods, conts, ids, endNs)
	return err
}

func splitKeys(keys []StreamKey) (pods, conts, ids []string) {
	for _, k := range keys {
		pods, conts, ids = append(pods, k.Pod), append(conts, k.Container), append(ids, k.LogID)
	}
	return pods, conts, ids
}

// dbNow is the database's clock.
func (s *Store) dbNow(ctx context.Context) (time.Time, error) {
	var now time.Time
	err := s.db.Pool.QueryRow(ctx, "SELECT now()").Scan(&now)
	return now, err
}
