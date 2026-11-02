// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// memTraceIndex is the trace index in memory: what Quickwit answers for
// the queries the sink makes (SpanQuery.Match).
type memTraceIndex struct {
	memSources
	mu   sync.Mutex
	docs map[string][]quickwit.SpanDoc
}

func newMemTraceIndex() *memTraceIndex { return &memTraceIndex{docs: map[string][]quickwit.SpanDoc{}} }

func (m *memTraceIndex) Ensure(_ context.Context, id string, _ []byte, src quickwit.Source) error {
	m.mu.Lock()
	if _, ok := m.docs[id]; !ok {
		m.docs[id] = nil
	}
	m.mu.Unlock()
	m.ensure(id, src)
	return nil
}

// sync is a commit of the trace topic's source.
func (m *memTraceIndex) sync(lg *memLog) {
	m.pull(lg, func(id string, r kafka.Record) {
		var d quickwit.SpanDoc
		if json.Unmarshal(r.Value, &d) == nil {
			m.mu.Lock()
			m.docs[id] = append(m.docs[id], d)
			m.mu.Unlock()
		}
	})
}

func (m *memTraceIndex) match(pattern string, q *quickwit.SpanQuery) []quickwit.SpanDoc {
	prefix := strings.TrimSuffix(pattern, "*")
	var out []quickwit.SpanDoc
	for id, docs := range m.docs {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		for i := range docs {
			if q.Match(&docs[i]) {
				out = append(out, docs[i])
			}
		}
	}
	return out
}

func (m *memTraceIndex) SearchSpans(_ context.Context, pattern string, q quickwit.SpanQuery) ([]quickwit.SpanDoc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.match(pattern, &q)
	slices.SortStableFunc(out, func(a, b quickwit.SpanDoc) int { return cmp.Compare(a.TS, b.TS) })
	if len(out) > q.Max {
		out = out[:q.Max]
	}
	return out, nil
}

func (m *memTraceIndex) SpanTerms(_ context.Context, pattern string, q quickwit.SpanQuery, field string, size int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, d := range m.match(pattern, &q) {
		switch field {
		case "service_name":
			out = append(out, d.ServiceName)
		case "span_name":
			out = append(out, d.SpanName)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	return out[:min(len(out), size)], nil
}

func (m *memTraceIndex) NewestTraces(_ context.Context, pattern string, q quickwit.SpanQuery, n int) ([]quickwit.TraceHit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	newest := map[string]int64{}
	for _, d := range m.match(pattern, &q) {
		if x, ok := newest[d.TraceID]; !ok || d.StartNs > x {
			newest[d.TraceID] = d.StartNs
		}
	}
	var out []quickwit.TraceHit
	for id, x := range newest {
		out = append(out, quickwit.TraceHit{TraceID: id, NewestNs: x})
	}
	slices.SortFunc(out, func(a, b quickwit.TraceHit) int { return cmp.Compare(b.NewestNs, a.NewestNs) })
	return out[:min(len(out), n)], nil
}

func (m *memTraceIndex) SpanHistogram(_ context.Context, pattern string, q quickwit.SpanQuery, step time.Duration,
	durations bool) (quickwit.Histogram, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byStep := map[int64][]int64{}
	var all []int64
	for _, d := range m.match(pattern, &q) {
		start := d.TS - d.TS%int64(step)
		byStep[start] = append(byStep[start], d.DurationNs)
		all = append(all, d.DurationNs)
	}
	h := quickwit.Histogram{Count: int64(len(all))}
	if durations {
		h.P50Ns, h.P95Ns = percentileCont(all, 0.50), percentileCont(all, 0.95)
	}
	for _, start := range slices.Sorted(maps.Keys(byStep)) {
		b := quickwit.Bucket{StartNs: start, Count: int64(len(byStep[start]))}
		if durations {
			b.P95Ns = percentileCont(byStep[start], 0.95)
		}
		h.Buckets = append(h.Buckets, b)
	}
	return h, nil
}

// percentileCont is the p-th percentile of xs, interpolated between the
// closest ranks (PostgreSQL's percentile_cont); nil for none. Quickwit
// estimates it within about 1 %.
func percentileCont(xs []int64, p float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	s := slices.Sorted(slices.Values(xs))
	r := p * float64(len(s)-1)
	lo := int(r)
	v := float64(s[lo])
	if lo+1 < len(s) {
		v += (r - float64(lo)) * float64(s[lo+1]-s[lo])
	}
	return &v
}

// count is how many spans the indexes hold.
func (m *memTraceIndex) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, docs := range m.docs {
		n += len(docs)
	}
	return n
}
