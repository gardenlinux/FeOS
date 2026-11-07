// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// Index is the log indexes (ADR 0026 §10, §11): Quickwit
// (quickwit.Client) or a test's. Their Kafka sources fill them from the
// topics; the sink only searches them and leaves delete tasks.
type Index interface {
	SourceIndex
	Indexes(ctx context.Context, prefix string) ([]string, error)
	Search(ctx context.Context, pattern string, q quickwit.Query) ([]quickwit.Doc, error)
	Delete(ctx context.Context, id string, q quickwit.Query) error
}

var _ Index = (*quickwit.Client)(nil)

// indexID is the index (and topic) of a retention in days, of the current
// mapping: <prefix>-v<version>-<days>d.
func (s *Store) indexID(days int) string { return quickwit.IndexID(s.cfg.IndexPrefix, days) }

// indexPattern names every log index of the sink, of every mapping
// version (<prefix>-<days>d is version 1).
func (s *Store) indexPattern() string { return s.cfg.IndexPrefix + "-*" }

func durationDays(d time.Duration) int { return int((d + 24*time.Hour - 1) / (24 * time.Hour)) }

// classes are the retentions of the log indexes in days: the default's
// and the longest's (ADR 0026 §10, an index per retention).
func (s *Store) classes() []int {
	def, longest := durationDays(s.cfg.Retention), durationDays(s.cfg.RetentionMax)
	if longest == def {
		return []int{def}
	}
	return []int{def, longest}
}

// EnsureIndexes makes the log indexes and their topics of every class.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	for _, days := range s.classes() {
		if _, err := s.ensure(ctx, s.indexID(days), days); err != nil {
			return err
		}
	}
	return nil
}

// indexOf is the index of a stream: the default retention's, or the
// longest's for a stream that asks for more than the default.
func (s *Store) indexOf(retentionDays *int) (string, int) {
	c := s.classes()
	if retentionDays != nil && *retentionDays > c[0] {
		return s.indexID(c[len(c)-1]), c[len(c)-1]
	}
	return s.indexID(c[0]), c[0]
}

// ensure makes the log index (and topic) of days and returns its topic's
// partitions.
func (s *Store) ensure(ctx context.Context, id string, days int) (int32, error) {
	return s.pipes.ensure(ctx, id, func() []byte {
		return quickwit.IndexConfig(id, quickwit.IndexSettings{RetentionDays: days,
			CommitSecs: int(s.cfg.CommitTimeout / time.Second), Partitions: s.cfg.IndexPartitions})
	})
}

// streamKey is a stream's key in the topics: its tenant, environment
// and object (an environment's service, addon or job, or a sandbox, by
// kind and name), or, for a stream of no object (a tenant's job, which
// names its environment since ADR 0035, or none when shipped before),
// its tenant and job. A read selects by these labels: an object's
// streams, its replicas' and its ingress's, lie in one partition, an
// environment's in those of its objects.
func streamKey(tenant, environment, kind, object, job string) string {
	if environment != "" && kind != "" {
		return tenant + "/" + environment + "/" + kind + "/" + object
	}
	return tenant + "//" + job
}

// keyOf is a stream's key.
func keyOf(st *streamRow) string {
	return streamKey(st.Tenant, st.Environment, st.Kind, st.Object, st.Job)
}

// docOf is a line as its stream's index holds it.
func docOf(l Line, st *streamRow) quickwit.Doc {
	return quickwit.Doc{TS: l.TS, Tenant: st.Tenant, Stream: st.SID, Environment: st.Environment, Kind: st.Kind,
		Object: st.Object, Replica: st.Replica, Deployment: st.Deployment, Instance: st.Instance, Container: st.Container,
		Job: st.Job, Attempt: st.Attempt, Source: st.Source, IOStream: l.Source, Level: l.Level, TraceID: l.Trace,
		Off: l.Off, Partial: l.Partial, Cont: l.Cont, Text: l.Text}
}

// recordOf is a line as a record of its stream's topic, in its stream's
// partition of n.
func recordOf(l Line, st *streamRow, n int32) (kafka.Record, error) {
	d := docOf(l, st)
	v, err := json.Marshal(&d)
	if err != nil {
		return kafka.Record{}, fmt.Errorf("line %d: %w", l.Off, err)
	}
	key := keyOf(st)
	return kafka.Record{Partition: kafka.Partition(key, n), Key: []byte(key), Value: v}, nil
}

// docsOf reads records of a log topic as documents; one that is not a
// document is skipped (Quickwit skips it too).
func docsOf(recs []kafka.Record) []quickwit.Doc {
	out := make([]quickwit.Doc, 0, len(recs))
	for _, r := range recs {
		var d quickwit.Doc
		if json.Unmarshal(r.Value, &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// lineOf is a document as a line of the stream row id.
func lineOf(d quickwit.Doc, id int64) Line {
	lvl := d.Level
	if lvl == 0 {
		lvl = LevelUnknown
	}
	return Line{Stream: id, Off: d.Off, TS: d.TS, Source: d.IOStream, Level: lvl, Partial: d.Partial, Cont: d.Cont,
		Text: d.Text, Trace: d.TraceID}
}

// tailDocs reads the documents of the log topics that their indexes have
// not published, of the partitions of the streams' keys: the newest
// lines of a read, a join or a follow's start (ADR 0026 §11). A class
// whose topic no ingest has made yet is made here, as an ingest would.
func (s *Store) tailDocs(ctx context.Context, streams []*streamRow) ([]quickwit.Doc, error) {
	var out []quickwit.Doc
	for _, days := range s.classes() {
		id := s.indexID(days)
		n, err := s.pipes.open(ctx, id)
		if err != nil {
			return nil, err
		}
		recs, err := s.pipes.tail(ctx, id, partitionsOf(streams, n))
		if err != nil {
			return nil, err
		}
		out = append(out, docsOf(recs)...)
	}
	return out, nil
}

// partitionsOf are the partitions of n that the streams' keys lie in.
func partitionsOf(streams []*streamRow, n int32) []int32 {
	seen := map[int32]bool{}
	var out []int32
	for _, st := range streams {
		p := kafka.Partition(keyOf(st), n)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
