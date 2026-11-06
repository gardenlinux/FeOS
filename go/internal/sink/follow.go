// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// followSink is where a follow writes: a line, with its stream the first
// time, the heartbeat, and the lines skipped.
type followSink interface {
	Line(l Line, st *streamRow) error
	Heartbeat() error
	// Skipped says n lines were skipped by the follow's rate.
	Skipped(n int) error
}

// ErrFollowScope refuses a follow of more than one object: what can be
// followed live is an environment's object (a service, an addon, a job,
// by environment, kind and name), finer its replica, instance or stream,
// or a tenant's job; an environment as a whole is searched, not followed.
var ErrFollowScope = errors.New("a follow is of one object: environment, kind and object, or a tenant's job, or an instance or stream of one")

// FollowKey is the topic key of what q's selector follows: one object's,
// or ErrFollowScope. An instance or a stream is the key of its streams,
// which must be there and of one object.
func (s *Store) FollowKey(ctx context.Context, tenant string, sel Selector) (string, error) {
	switch {
	case sel.Environment != "" && sel.Kind != "" && sel.Object != "":
		return streamKey(tenant, sel.Environment, sel.Kind, sel.Object, ""), nil
	case sel.Environment == "" && sel.Job != "":
		return streamKey(tenant, "", "", "", sel.Job), nil
	case sel.Instance != "" || sel.Stream != "":
		sts, err := selectStreams(ctx, s.db.Pool, tenant, streamFilter{sel: sel, limit: maxStreamsRead})
		if err != nil {
			return "", err
		}
		key := ""
		for _, st := range sts {
			if k := keyOf(st); key == "" {
				key = k
			} else if k != key {
				return "", ErrFollowScope
			}
		}
		if key == "" {
			return "", fmt.Errorf("%w: no stream of the instance or stream yet", ErrFollowScope)
		}
		return key, nil
	}
	return "", ErrFollowScope
}

// The bounds of a follow's lines a second (ADR 0026 §11): rate lines, a
// burst of burst; beyond them error and warn lines have errRate more, and
// the rest is skipped, which the follow says once a second ({"skipped"}).
const (
	followRate      = 100
	followBurst     = 500
	followErrRate   = 50
	followErrBurst  = 200
	skippedInterval = time.Second
)

// Follow answers /api/v1/logs?follow=1: the page a backward read of q
// gives, then every line stored after it, as the topics bring it, until
// ctx ends or a write fails (ADR 0026 §11). What it follows is one object
// (FollowKey): its partition of each log topic, read from where it ended
// before the page was read; a line the page gave is not given again, and
// a stream of the object that starts meanwhile is followed too (its key is
// the same). Lines beyond followRate a second are skipped, error and warn
// lines last.
func (s *Store) Follow(ctx context.Context, tenant string, q Query, out followSink) error {
	followed, err := s.FollowKey(ctx, tenant, q.Sel)
	if err != nil {
		return err
	}
	q.Forward, q.Cursor = false, nil
	if !q.SinceSet {
		q.Since = q.Until - int64(time.Hour)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	recs := make(chan []kafka.Record)
	errc := make(chan error, len(s.classes()))
	for _, days := range s.classes() {
		id := s.indexID(days)
		n, err := s.pipes.open(ctx, id)
		if err != nil {
			return err
		}
		from, err := s.pipes.ends(ctx, id, []int32{kafka.Partition(followed, n)})
		if err != nil {
			return err
		}
		f, err := s.pipes.log.Follow(id, from)
		if err != nil {
			return err
		}
		go func() {
			defer f.Close()
			for {
				rs, err := f.Next(ctx)
				if err != nil {
					errc <- err
					return
				}
				if ctx.Err() != nil {
					return
				}
				if len(rs) > 0 {
					select {
					case recs <- rs:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	res, err := s.Read(ctx, tenant, q)
	if err != nil {
		return err
	}
	type key struct {
		sid string
		off int64
	}
	given := map[key]bool{}
	known := map[string]*streamRow{} // by stream ID; nil: gone
	byID := map[int64]*streamRow{}
	for id, st := range res.streams {
		known[st.SID], byID[id] = st, st
	}
	slices.Reverse(res.lines)
	for _, l := range res.lines {
		st := res.streams[l.Stream]
		given[key{st.SID, l.Off}] = true
		if err := out.Line(l, st); err != nil {
			return err
		}
	}
	joins := followJoins{open: map[joinKey]*followJoin{}, cut: map[joinKey]bool{}}
	limit := newFollowLimit(time.Now())
	beat := time.NewTicker(s.cfg.Heartbeat)
	defer beat.Stop()
	lastWrite := time.Now()
	// A record can come before its stream's row: an ingest produces, then
	// commits. Its line waits for the row, looked for again every
	// streamRetry, at most streamWait.
	var waiting []waitingDoc
	for {
		var retry, say <-chan time.Time
		if len(waiting) > 0 {
			retry = time.After(streamRetry)
		}
		if limit.skipped > 0 {
			say = time.After(time.Until(limit.said.Add(skippedInterval)))
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return err
		case <-beat.C:
		case <-retry:
		case <-say:
		case rs := <-recs:
			now := time.Now()
			for _, d := range docsOf(rs) {
				if !selects(tenant, q.Sel, &d) || given[key{d.Stream, d.Off}] {
					continue
				}
				given[key{d.Stream, d.Off}] = true
				waiting = append(waiting, waitingDoc{d: d, since: now})
			}
		}
		var lines []Line
		now := time.Now()
		unknown := map[string]bool{} // not found in this round
		keep := waiting[:0]
		for _, w := range waiting {
			var st *streamRow
			if !unknown[w.d.Stream] {
				var err error
				if st, err = s.followed(ctx, tenant, w.d.Stream, known, now.Sub(w.since) >= streamWait); err != nil {
					return err
				}
			}
			switch _, gone := known[w.d.Stream]; {
			case st != nil:
				byID[st.ID] = st
				lines = append(lines, lineOf(w.d, st.ID))
			case !gone:
				unknown[w.d.Stream] = true
				keep = append(keep, w)
			}
		}
		clear(waiting[len(keep):])
		waiting = keep
		for _, l := range joins.take(lines, time.Now()) {
			if !q.match(&l) {
				continue
			}
			if !limit.admit(&l, time.Now()) {
				continue
			}
			if err := out.Line(l, byID[l.Stream]); err != nil {
				return err
			}
			lastWrite = time.Now()
		}
		if n := limit.skippedDue(time.Now()); n > 0 {
			if err := out.Skipped(n); err != nil {
				return err
			}
			lastWrite = time.Now()
		}
		if time.Since(lastWrite) >= s.cfg.Heartbeat {
			if err := out.Heartbeat(); err != nil {
				return err
			}
			lastWrite = time.Now()
		}
		// A follow's set of given lines is bounded by what it wrote; a
		// long follow forgets those older than the topics could send again.
		if len(given) > 200000 {
			clear(given)
		}
	}
}

// waitingDoc is a record a follow took whose stream it has not found yet.
type waitingDoc struct {
	d     quickwit.Doc
	since time.Time
}

// streamRetry is how often a follow looks for the stream of a line that
// waits for it, streamWait how long at most.
const (
	streamRetry = 100 * time.Millisecond
	streamWait  = 5 * time.Second
)

// followed is the stream of sid that a follow gives a line of: known, or
// read from the database (a stream that started during the follow); nil
// for one not found, which is gone once final (known[sid] nil).
func (s *Store) followed(ctx context.Context, tenant, sid string, known map[string]*streamRow, final bool) (*streamRow, error) {
	if st, ok := known[sid]; ok {
		return st, nil
	}
	sts, err := selectStreams(ctx, s.db.Pool, tenant, streamFilter{sel: Selector{Stream: sid}, limit: 1})
	if err != nil {
		return nil, fmt.Errorf("stream %s: %w", sid, err)
	}
	if len(sts) == 0 {
		if final {
			known[sid] = nil
		}
		return nil, nil
	}
	known[sid] = sts[0]
	return sts[0], nil
}

// selects reports whether a document is of tenant's streams of sel.
func selects(tenant string, sel Selector, d *quickwit.Doc) bool {
	eq := func(want, got string) bool { return want == "" || want == got }
	return d.Tenant == tenant && eq(sel.Environment, d.Environment) && eq(sel.Kind, d.Kind) && eq(sel.Object, d.Object) &&
		eq(sel.Instance, d.Instance) && eq(sel.Container, d.Container) && eq(sel.Job, d.Job) && eq(sel.Stream, d.Stream) &&
		eq(sel.Source, d.Source) && (sel.Replica == nil || *sel.Replica == d.Replica) &&
		(sel.Attempt == nil || *sel.Attempt == d.Attempt) &&
		(sel.Within == nil || (d.Environment != "" && slices.Contains(sel.Within, d.Environment)))
}

// joinKey is a stream's source, whose lines a partial one joins.
type joinKey struct {
	stream int64
	source string
}

type followJoin struct {
	j     *joiner
	since time.Time
}

// followJoins joins a follow's partial lines with their rest across polls
// (as joiner does for a read): an open one waits for its rest at most
// joinWait, then is given partial. A line that goes on with one the page
// gave is given on its own; the rest of one cut at maxJoined is dropped.
type followJoins struct {
	open map[joinKey]*followJoin
	cut  map[joinKey]bool
}

// joinWait is how long a follow holds a partial line for its rest.
const joinWait = 5 * time.Second

// take joins the lines of a poll, in each stream's offset order, and
// returns those complete, and those held past joinWait, in time order.
func (f *followJoins) take(lines []Line, now time.Time) []Line {
	var done []Line
	for i := range lines {
		l := lines[i]
		k := joinKey{l.Stream, l.Source}
		if l.Cont {
			if o := f.open[k]; o != nil {
				if o.j.add(&l) {
					done = append(done, *o.j.head)
					delete(f.open, k)
					f.cut[k] = l.Partial
				}
				continue
			}
			if f.cut[k] {
				f.cut[k] = l.Partial
				continue
			}
			done = append(done, l)
			continue
		}
		if o := f.open[k]; o != nil {
			o.j.add(&l)
			done = append(done, *o.j.head)
			delete(f.open, k)
		}
		delete(f.cut, k)
		if l.Partial {
			head := l
			f.open[k] = &followJoin{j: newJoiner(&head), since: now}
			continue
		}
		done = append(done, l)
	}
	for k, o := range f.open {
		if now.Sub(o.since) >= joinWait {
			o.j.finish(true)
			done = append(done, *o.j.head)
			delete(f.open, k)
		}
	}
	slices.SortStableFunc(done, func(a, b Line) int { return cmpPos(a.pos(), b.pos()) })
	return done
}

// followLimit is a follow's rate: a bucket of lines, and one more for
// error and warn lines once it is empty; what neither takes is counted.
type followLimit struct {
	tokens, errTokens float64
	last              time.Time
	skipped           int
	said              time.Time
}

func newFollowLimit(now time.Time) *followLimit {
	return &followLimit{tokens: followBurst, errTokens: followErrBurst, last: now, said: now}
}

// admit reports whether l may go now, counting it skipped if not.
func (f *followLimit) admit(l *Line, now time.Time) bool {
	dt := now.Sub(f.last).Seconds()
	f.last = now
	f.tokens = min(followBurst, f.tokens+dt*followRate)
	f.errTokens = min(followErrBurst, f.errTokens+dt*followErrRate)
	switch {
	case f.tokens >= 1:
		f.tokens--
		return true
	case l.Level&(LevelError|LevelWarn) != 0 && f.errTokens >= 1:
		f.errTokens--
		return true
	}
	f.skipped++
	return false
}

// skippedDue returns the lines skipped since it last said, at most once a
// skippedInterval.
func (f *followLimit) skippedDue(now time.Time) int {
	if f.skipped == 0 || now.Sub(f.said) < skippedInterval {
		return 0
	}
	n := f.skipped
	f.skipped, f.said = 0, now
	return n
}
