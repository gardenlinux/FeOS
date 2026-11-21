// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// maxJoined bounds a joined line's text; one cut there stays partial.
const maxJoined = 1 << 20

// joiner joins a partial line with the lines that go on with it (Cont), in
// the order of their offsets: up to and including the first that is not
// partial, at most maxJoined bytes.
type joiner struct {
	head *Line
	text strings.Builder
	done bool
}

func newJoiner(head *Line) *joiner {
	j := &joiner{head: head}
	j.text.WriteString(head.Text)
	return j
}

// add takes the next line of the head's stream and source; it reports
// whether the joined line is complete.
func (j *joiner) add(l *Line) bool {
	if j.done {
		return true
	}
	if !l.Cont {
		// The line ended without its rest: it stays partial.
		return j.finish(true)
	}
	j.text.WriteString(l.Text)
	if j.text.Len() >= maxJoined {
		return j.finish(true)
	}
	if !l.Partial {
		return j.finish(false)
	}
	return false
}

// finish ends the joined line; partial says it is not whole.
func (j *joiner) finish(partial bool) bool {
	t := j.text.String()
	if len(t) > maxJoined {
		cut := maxJoined
		for cut > 0 && !utf8.RuneStart(t[cut]) {
			cut--
		}
		t, partial = t[:cut], true
	}
	j.head.Text, j.head.Partial, j.done = t, partial, true
	return true
}

// joinParts completes each partial head of a read with the lines that go
// on with it, those at or below bound[stream] (the streams' last offsets
// the read saw): from the index and the topic's tail of its stream, each
// offset once. A head whose rest is not there stays partial.
func (s *Store) joinParts(ctx context.Context, project string, heads []Line, streams map[int64]*streamRow,
	bound map[int64]int64) error {
	tails := map[int64][]quickwit.Doc{}
	for i := range heads {
		h := &heads[i]
		if !h.Partial || h.Cont {
			continue
		}
		upTo, ok := bound[h.Stream]
		st := streams[h.Stream]
		if !ok || st == nil || upTo <= h.Off {
			continue
		}
		from := h.Off
		rest, err := s.index.Search(ctx, s.indexPattern(), quickwit.Query{Project: project, Environment: st.Environment,
			Job: st.Job, Stream: st.SID, IOStream: h.Source, OffAfter: &from, OffTo: &upTo, Order: quickwit.ByOffset,
			Max: 2000})
		if err != nil {
			return fmt.Errorf("the rest of a partial line: index: %w", err)
		}
		tail, ok := tails[st.ID]
		if !ok {
			if tail, err = s.tailDocs(ctx, []*streamRow{st}); err != nil {
				return fmt.Errorf("the rest of a partial line: %w", err)
			}
			tails[st.ID] = tail
		}
		for _, d := range tail {
			if d.Stream == st.SID && d.IOStream == h.Source && d.Off > h.Off && d.Off <= upTo {
				rest = append(rest, d)
			}
		}
		slices.SortFunc(rest, func(a, b quickwit.Doc) int { return cmpInt(a.Off, b.Off) })
		j := newJoiner(h)
		last := h.Off
		for k := range rest {
			if rest[k].Off <= last {
				continue // in both, or twice in the topic
			}
			last = rest[k].Off
			l := lineOf(rest[k], h.Stream)
			if j.add(&l) {
				break
			}
		}
		if !j.done {
			j.finish(true)
		}
	}
	return nil
}
