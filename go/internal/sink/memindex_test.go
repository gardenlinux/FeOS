// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// memIndex is the index in memory: what Quickwit answers for the
// queries the sink makes, a word matched as a substring (a superset of
// Quickwit's whole words, which the sink's own filters narrow anyway).
type memIndex struct {
	memSources
	mu   sync.Mutex
	docs map[string][]quickwit.Doc
}

func newMemIndex() *memIndex { return &memIndex{docs: map[string][]quickwit.Doc{}} }

func (m *memIndex) Ensure(_ context.Context, id string, _ []byte, src quickwit.Source) error {
	m.mu.Lock()
	if _, ok := m.docs[id]; !ok {
		m.docs[id] = nil
	}
	m.mu.Unlock()
	m.ensure(id, src)
	return nil
}

// sync is a commit of every source: what the topics hold that the index
// has not taken is indexed now.
func (m *memIndex) sync(lg *memLog) {
	m.pull(lg, func(id string, r kafka.Record) {
		var d quickwit.Doc
		if json.Unmarshal(r.Value, &d) == nil {
			m.mu.Lock()
			m.docs[id] = append(m.docs[id], d)
			m.mu.Unlock()
		}
	})
}

func (m *memIndex) Indexes(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id := range m.docs {
		if strings.HasPrefix(id, prefix) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func matches(d *quickwit.Doc, q *quickwit.Query) bool {
	eq := func(want, got string) bool { return want == "" || want == got }
	if !eq(q.Project, d.Project) || !eq(q.Environment, d.Environment) || !eq(q.Kind, d.Kind) || !eq(q.Object, d.Object) ||
		!eq(q.Instance, d.Instance) || !eq(q.Container, d.Container) || !eq(q.Job, d.Job) || !eq(q.Stream, d.Stream) ||
		!eq(q.Source, d.Source) || !eq(q.IOStream, d.IOStream) || !eq(q.Trace, d.TraceID) {
		return false
	}
	if (q.Replica != nil && *q.Replica != d.Replica) || (q.Attempt != nil && *q.Attempt != d.Attempt) {
		return false
	}
	if (q.SinceNs > 0 && d.TS < q.SinceNs) || (q.UntilNs > 0 && d.TS > q.UntilNs) {
		return false
	}
	if (q.OffAfter != nil && d.Off <= *q.OffAfter) || (q.OffTo != nil && d.Off > *q.OffTo) {
		return false
	}
	if q.Levels != 0 && d.Level&q.Levels == 0 {
		return false
	}
	words := true
	text := strings.ToLower(d.Text)
	for _, w := range q.Words {
		words = words && strings.Contains(text, strings.ToLower(w))
	}
	for _, w := range q.NotWords {
		words = words && !strings.Contains(text, strings.ToLower(w))
	}
	return words || (q.OrPartial && d.Partial)
}

func (m *memIndex) Search(_ context.Context, pattern string, q quickwit.Query) ([]quickwit.Doc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := strings.TrimSuffix(pattern, "*")
	var out []quickwit.Doc
	for id, docs := range m.docs {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		for i := range docs {
			if matches(&docs[i], &q) {
				out = append(out, docs[i])
			}
		}
	}
	slices.SortStableFunc(out, func(a, b quickwit.Doc) int {
		switch q.Order {
		case quickwit.OldestFirst:
			return cmp.Or(cmp.Compare(a.TS, b.TS), cmp.Compare(a.Off, b.Off))
		case quickwit.ByOffset:
			return cmp.Compare(a.Off, b.Off)
		}
		return cmp.Or(cmp.Compare(b.TS, a.TS), cmp.Compare(b.Off, a.Off))
	})
	if len(out) > q.Max {
		out = out[:q.Max]
	}
	return out, nil
}

func (m *memIndex) Delete(_ context.Context, id string, q quickwit.Query) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := m.docs[id][:0]
	for _, d := range m.docs[id] {
		if !matches(&d, &q) {
			keep = append(keep, d)
		}
	}
	m.docs[id] = keep
	return nil
}

// add puts docs into the index id as its source would have, making the
// index unless it is there: an index of before, filled by its indexer.
func (m *memIndex) add(id string, docs ...quickwit.Doc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs[id] = append(m.docs[id], docs...)
}

// count is how many documents of project the indexes hold.
func (m *memIndex) count(project string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, docs := range m.docs {
		for _, d := range docs {
			if d.Project == project {
				n++
			}
		}
	}
	return n
}
