// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The page sizes of /api/v1/logs and /api/v1/log-streams.
const (
	defaultLimit   = 500
	maxLimit       = 5000
	maxStreamsList = 500
	// maxStreamsRead bounds the streams one read selects.
	maxStreamsRead = 10000
	maxQueryText   = 1024
)

// Selector selects streams by their labels; one of Environment, Job,
// Instance and Stream is set. Source is SourceIngress, SourceEgress,
// SourceContainer or "" for all.
type Selector struct {
	Environment, Kind, Object, Instance, Container, Job, Stream, Source string
	Replica, Attempt                                                    *int
}

// Query is a read of /api/v1/logs: the selector, the time range in Unix
// ns (both ends included; Since unset is the default of the direction),
// the line filters, the direction, the page's size and where it goes on.
type Query struct {
	Sel          Selector
	Since, Until int64
	SinceSet     bool
	Words        []string // lower case, all must appear
	NotWords     []string // lower case, none may appear
	Re           *regexp.Regexp
	Levels       int // a mask of level bits; LevelAll for all
	Trace        string
	Forward      bool
	Limit        int
	Cursor       *pos
	Follow       bool
}

// pos is a line's place in the order of reads: time, stream row, offset.
type pos struct {
	TS, Stream, Off int64
}

func cmpPos(a, b pos) int {
	switch {
	case a.TS != b.TS:
		return cmpInt(a.TS, b.TS)
	case a.Stream != b.Stream:
		return cmpInt(a.Stream, b.Stream)
	}
	return cmpInt(a.Off, b.Off)
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (l Line) pos() pos { return pos{TS: l.TS, Stream: l.Stream, Off: l.Off} }

// before reports whether a comes before b in the scan's order: ascending
// forward, descending backward.
func (q *Query) before(a, b pos) bool {
	if q.Forward {
		return cmpPos(a, b) < 0
	}
	return cmpPos(a, b) > 0
}

// encodeCursor is a cursor's opaque text: the direction and the place.
func encodeCursor(forward bool, p pos) string {
	d := "b"
	if forward {
		d = "f"
	}
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%s:%d:%d:%d", d, p.TS, p.Stream, p.Off))
}

func decodeCursor(s string, forward bool) (pos, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return pos{}, fmt.Errorf("cursor is not one of an answer")
	}
	parts := strings.Split(string(b), ":")
	if len(parts) != 4 || (parts[0] != "b" && parts[0] != "f") {
		return pos{}, fmt.Errorf("cursor is not one of an answer")
	}
	if (parts[0] == "f") != forward {
		return pos{}, fmt.Errorf("cursor is of the other direction")
	}
	var p pos
	for i, dst := range []*int64{&p.TS, &p.Stream, &p.Off} {
		n, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil {
			return pos{}, fmt.Errorf("cursor is not one of an answer")
		}
		*dst = n
	}
	return p, nil
}

// parseSelector reads the selector's parameters; one of environment,
// job, instance and stream is required.
func parseSelector(v url.Values) (Selector, error) {
	s := Selector{
		Environment: v.Get("environment"), Kind: v.Get("kind"), Object: v.Get("object"),
		Instance: v.Get("instance"), Container: v.Get("container"), Job: v.Get("job"), Stream: v.Get("stream"),
		Source: v.Get("source"),
	}
	if !validSource(s.Source) {
		return Selector{}, fmt.Errorf("source must be ingress, egress or container")
	}
	for _, n := range []struct {
		name string
		dst  **int
	}{{"replica", &s.Replica}, {"attempt", &s.Attempt}} {
		if t := v.Get(n.name); t != "" {
			x, err := strconv.Atoi(t)
			if err != nil || x < 0 {
				return Selector{}, fmt.Errorf("%s must be a number", n.name)
			}
			*n.dst = &x
		}
	}
	if s.Environment == "" && s.Job == "" && s.Instance == "" && s.Stream == "" {
		return Selector{}, fmt.Errorf("one of environment, job, instance and stream is required")
	}
	return s, nil
}

// parseTime reads RFC 3339 or Unix nanoseconds.
func parseTime(name, s string) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, fmt.Errorf("%s must be RFC 3339 or Unix nanoseconds", name)
	}
	return t.UnixNano(), nil
}

// parseQuery reads the parameters of /api/v1/logs (sink.md, Reading);
// now is the default of until.
func parseQuery(v url.Values, now time.Time) (Query, error) {
	sel, err := parseSelector(v)
	if err != nil {
		return Query{}, err
	}
	q := Query{Sel: sel, Until: now.UnixNano(), Levels: LevelAll, Limit: defaultLimit}
	switch v.Get("direction") {
	case "", "backward":
	case "forward":
		q.Forward = true
	default:
		return Query{}, fmt.Errorf("direction must be backward or forward")
	}
	if t := v.Get("until"); t != "" {
		if q.Until, err = parseTime("until", t); err != nil {
			return Query{}, err
		}
	}
	if t := v.Get("since"); t != "" {
		if q.Since, err = parseTime("since", t); err != nil {
			return Query{}, err
		}
		q.SinceSet = true
	} else if !q.Forward {
		q.Since = q.Until - int64(time.Hour)
	}
	if q.SinceSet && q.Since > q.Until {
		return Query{}, fmt.Errorf("since must not be after until")
	}
	if t := v.Get("q"); t != "" {
		if len(t) > maxQueryText {
			return Query{}, fmt.Errorf("q is longer than %d bytes", maxQueryText)
		}
		for _, w := range strings.Fields(strings.ToLower(t)) {
			if not, ok := strings.CutPrefix(w, "-"); ok && not != "" {
				q.NotWords = append(q.NotWords, not)
			} else {
				q.Words = append(q.Words, w)
			}
		}
	}
	if t := v.Get("re"); t != "" {
		if len(t) > maxQueryText {
			return Query{}, fmt.Errorf("re is longer than %d bytes", maxQueryText)
		}
		if q.Re, err = regexp.Compile(t); err != nil {
			return Query{}, fmt.Errorf("re: %v", err)
		}
	}
	if t := v.Get("level"); t != "" {
		q.Levels = 0
		for _, n := range strings.Split(t, ",") {
			b := levelBit(strings.TrimSpace(n))
			if b == 0 {
				return Query{}, fmt.Errorf("level must be a list of error, warn, info, debug and unknown")
			}
			q.Levels |= b
		}
	}
	if t := v.Get("trace"); t != "" {
		if !isTraceID(t) {
			return Query{}, fmt.Errorf("trace must be 32 lowercase hex digits, not all zero")
		}
		q.Trace = t
		if !q.SinceSet {
			// A trace's lines are found by its index, wherever they are.
			q.Since, q.SinceSet = 0, true
		}
	}
	if t := v.Get("limit"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 1 {
			return Query{}, fmt.Errorf("limit must be a positive number")
		}
		q.Limit = min(n, maxLimit)
	}
	switch v.Get("follow") {
	case "", "0":
	case "1":
		q.Follow = true
	default:
		return Query{}, fmt.Errorf("follow must be 1 or 0")
	}
	if t := v.Get("cursor"); t != "" {
		if q.Follow {
			return Query{}, fmt.Errorf("follow takes no cursor")
		}
		p, err := decodeCursor(t, q.Forward)
		if err != nil {
			return Query{}, err
		}
		q.Cursor = &p
	}
	return q, nil
}

// match reports whether a line passes the line filters.
func (q *Query) match(l *Line) bool {
	if l.Level&q.Levels == 0 {
		return false
	}
	if len(q.Words) > 0 || len(q.NotWords) > 0 {
		lower := strings.ToLower(l.Text)
		for _, w := range q.Words {
			if !strings.Contains(lower, w) {
				return false
			}
		}
		for _, w := range q.NotWords {
			if strings.Contains(lower, w) {
				return false
			}
		}
	}
	if q.Re != nil && !q.Re.MatchString(l.Text) {
		return false
	}
	if q.Trace != "" {
		tr := l.Trace
		if tr == "" {
			_, tr = Extract(l.Text)
		}
		if tr != q.Trace {
			return false
		}
	}
	return true
}

// inRange reports whether a line lies in the time range and after the
// cursor.
func (q *Query) inRange(l *Line) bool {
	if l.TS < q.Since || l.TS > q.Until {
		return false
	}
	return q.Cursor == nil || q.before(*q.Cursor, l.pos())
}
