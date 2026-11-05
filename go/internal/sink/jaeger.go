// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// The bounds of the reads of traces (traces.md, Reading): the spans of a
// trace, the traces of a search, the names of services and operations,
// how far back the names go, and the traces loaded at once.
const (
	maxTraceSpans      = 10000
	defaultTraceLimit  = 20
	maxTraceLimit      = 100
	maxSpanNames       = 10000
	maxTags            = 32
	spanNamesLookback  = 7 * 24 * time.Hour
	defaultTraceWindow = time.Hour
	traceLoads         = 4
)

// Services answers the service names of a tenant's spans of sel since
// sinceNs, sorted.
func (t *Traces) Services(ctx context.Context, sel quickwit.SpanQuery, sinceNs int64) ([]string, error) {
	return t.names(ctx, sel, sinceNs, "service_name", func(d *quickwit.SpanDoc) string { return d.ServiceName })
}

// Operations answers the span names of a tenant's spans of sel (its
// Service set) since sinceNs, sorted.
func (t *Traces) Operations(ctx context.Context, sel quickwit.SpanQuery, sinceNs int64) ([]string, error) {
	return t.names(ctx, sel, sinceNs, "span_name", func(d *quickwit.SpanDoc) string { return d.SpanName })
}

func (t *Traces) names(ctx context.Context, q quickwit.SpanQuery, sinceNs int64, field string,
	of func(*quickwit.SpanDoc) string) ([]string, error) {
	q.SinceNs = sinceNs
	names, err := t.index.SpanTerms(ctx, t.pattern(), q, field, maxSpanNames)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	hot, err := t.tailSpans(ctx, q)
	if err != nil {
		return nil, err
	}
	for i := range hot {
		names = append(names, of(&hot[i]))
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// Trace answers a trace's spans of sel (its Tenant and TraceID set), from
// the index and its topic's tail (ADR 0026 §11), one per span ID, the earliest first, at most
// maxTraceSpans; none: nil.
func (t *Traces) Trace(ctx context.Context, sel quickwit.SpanQuery) ([]quickwit.SpanDoc, error) {
	q := quickwit.SpanQuery{Tenant: sel.Tenant, TraceID: sel.TraceID, Environment: sel.Environment, Object: sel.Object,
		Job: sel.Job, Instance: sel.Instance, Source: sel.Source, Max: maxTraceSpans}
	spans, err := t.index.SearchSpans(ctx, t.pattern(), q)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	hot, err := t.tailSpans(ctx, q)
	if err != nil {
		return nil, err
	}
	return dedupeSpans(append(spans, hot...)), nil
}

// dedupeSpans keeps one span per span ID, sorts them by start and keeps
// at most maxTraceSpans.
func dedupeSpans(spans []quickwit.SpanDoc) []quickwit.SpanDoc {
	seen := make(map[string]bool, len(spans))
	out := spans[:0]
	for _, s := range spans {
		if seen[s.SpanID] {
			continue
		}
		seen[s.SpanID] = true
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b quickwit.SpanDoc) int {
		return cmp.Or(cmp.Compare(a.StartNs, b.StartNs), cmp.Compare(a.SpanID, b.SpanID))
	})
	if len(out) > maxTraceSpans {
		out = out[:maxTraceSpans]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Find answers the traces of the newest spans that pass q, at most
// limit, newest first, each with its spans of q's selectors (Trace).
func (t *Traces) Find(ctx context.Context, q quickwit.SpanQuery, limit int) ([][]quickwit.SpanDoc, error) {
	hits, err := t.index.NewestTraces(ctx, t.pattern(), q, limit)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	newest := map[string]int64{}
	for _, h := range hits {
		newest[h.TraceID] = h.NewestNs
	}
	hot, err := t.tailSpans(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, d := range hot {
		if n, ok := newest[d.TraceID]; !ok || d.StartNs > n {
			newest[d.TraceID] = d.StartNs
		}
	}
	ids := make([]string, 0, len(newest))
	for id := range newest {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { return cmp.Or(cmp.Compare(newest[b], newest[a]), cmp.Compare(a, b)) })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	traces := make([][]quickwit.SpanDoc, len(ids))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(traceLoads)
	for i, id := range ids {
		g.Go(func() error {
			sel := q
			sel.TraceID = id
			spans, err := t.Trace(gctx, sel)
			traces[i] = spans
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(traces, func(s []quickwit.SpanDoc) bool { return len(s) == 0 }), nil
}

// parseSpanSelector reads FeRun's selectors of the trace reads:
// environment, object, job, instance and source (ingress, egress or
// container).
func parseSpanSelector(v url.Values, tenant string) (quickwit.SpanQuery, error) {
	q := quickwit.SpanQuery{Tenant: tenant, Environment: v.Get("environment"), Object: v.Get("object"), Job: v.Get("job"),
		Instance: v.Get("instance"), Source: v.Get("source")}
	if !validSource(q.Source) {
		return quickwit.SpanQuery{}, fmt.Errorf("source must be ingress, egress or container")
	}
	return q, nil
}

// parseTraceID reads a trace ID as Jaeger takes it: up to 32 hex digits,
// padded with zeros on the left; not all zero.
func parseTraceID(s string) (string, error) {
	s = strings.ToLower(s)
	if len(s) == 0 || len(s) > 32 || strings.Trim(s, "0123456789abcdef") != "" {
		return "", fmt.Errorf("a trace ID is up to 32 hex digits")
	}
	s = strings.Repeat("0", 32-len(s)) + s
	if !isTraceID(s) {
		return "", fmt.Errorf("a trace ID must not be all zero")
	}
	return s, nil
}

// parseTraceSearch reads the parameters of a search of traces (Jaeger's
// GET /api/traces and FeRun's selectors); now is the default of end.
func parseTraceSearch(v url.Values, tenant string, now time.Time) (quickwit.SpanQuery, int, error) {
	q, err := parseSpanSelector(v, tenant)
	if err != nil {
		return q, 0, err
	}
	q.Service, q.Operation = v.Get("service"), v.Get("operation")
	q.UntilNs = now.UnixNano()
	us := func(name string) (int64, bool, error) {
		s := v.Get(name)
		if s == "" {
			return 0, false, nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 || n > (1<<63-1)/1000-1 {
			return 0, false, fmt.Errorf("%s must be microseconds since the epoch", name)
		}
		return n, true, nil
	}
	if n, ok, err := us("end"); err != nil {
		return q, 0, err
	} else if ok {
		q.UntilNs = n*1000 + 999
	}
	q.SinceNs = q.UntilNs - int64(defaultTraceWindow)
	if n, ok, err := us("start"); err != nil {
		return q, 0, err
	} else if ok {
		q.SinceNs = n * 1000
	}
	if q.SinceNs > q.UntilNs {
		return q, 0, fmt.Errorf("start must not be after end")
	}
	for _, d := range []struct {
		name string
		dst  *int64
	}{{"minDuration", &q.MinDurationNs}, {"maxDuration", &q.MaxDurationNs}} {
		if s := v.Get(d.name); s != "" {
			x, err := time.ParseDuration(s)
			if err != nil || x < 0 {
				return q, 0, fmt.Errorf("%s must be a duration such as 100ms or 1.5s", d.name)
			}
			*d.dst = int64(x)
		}
	}
	if s := v.Get("tags"); s != "" {
		var tags map[string]any
		if err := json.Unmarshal([]byte(s), &tags); err != nil {
			return q, 0, fmt.Errorf("tags must be a JSON object of attributes and values")
		}
		if len(tags) > maxTags {
			return q, 0, fmt.Errorf("at most %d tags", maxTags)
		}
		q.Tags = make(map[string]string, len(tags))
		for k, x := range tags {
			if !quickwit.ValidTagKey(k) {
				return q, 0, fmt.Errorf("tag %q: a key must not be empty or hold a backslash or a control character", k)
			}
			switch x := x.(type) {
			case string:
				q.Tags[k] = x
			case bool, float64:
				q.Tags[k] = fmt.Sprint(x)
			default:
				return q, 0, fmt.Errorf("tag %q: a value must be a string, a number or a bool", k)
			}
		}
	}
	limit := defaultTraceLimit
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return q, 0, fmt.Errorf("limit must be a number")
		}
		if n > 0 {
			limit = min(n, maxTraceLimit)
		}
	}
	return q, limit, nil
}

// The answers of Jaeger's query API (jaeger-query's structuredResponse
// and its JSON model).
type (
	jaegerAnswer struct {
		Data   any           `json:"data"`
		Total  int           `json:"total"`
		Limit  int           `json:"limit"`
		Offset int           `json:"offset"`
		Errors []jaegerError `json:"errors"`
	}
	jaegerError struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	jaegerTrace struct {
		TraceID   string                   `json:"traceID"`
		Spans     []jaegerSpan             `json:"spans"`
		Processes map[string]jaegerProcess `json:"processes"`
		Warnings  []string                 `json:"warnings"`
	}
	jaegerSpan struct {
		TraceID       string      `json:"traceID"`
		SpanID        string      `json:"spanID"`
		OperationName string      `json:"operationName"`
		References    []jaegerRef `json:"references"`
		StartTime     int64       `json:"startTime"`
		Duration      int64       `json:"duration"`
		Tags          []jaegerTag `json:"tags"`
		Logs          []jaegerLog `json:"logs"`
		ProcessID     string      `json:"processID"`
		Warnings      []string    `json:"warnings"`
		Flags         uint64      `json:"flags"`
	}
	jaegerRef struct {
		RefType string `json:"refType"`
		TraceID string `json:"traceID"`
		SpanID  string `json:"spanID"`
	}
	jaegerTag struct {
		Key   string `json:"key"`
		Type  string `json:"type"`
		Value any    `json:"value"`
	}
	jaegerLog struct {
		Timestamp int64       `json:"timestamp"`
		Fields    []jaegerTag `json:"fields"`
	}
	jaegerProcess struct {
		ServiceName string      `json:"serviceName"`
		Tags        []jaegerTag `json:"tags"`
	}
)

// spanKinds are the span.kind tags of OTLP's kinds.
var spanKinds = map[uint64]string{1: "internal", 2: "server", 3: "client", 4: "producer", 5: "consumer"}

// tagOf is an attribute as a Jaeger tag: a string, a bool, an integer as
// int64, another number as float64, an array or an object as its JSON
// text.
func tagOf(k string, raw json.RawMessage) jaegerTag {
	var x any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&x); err != nil {
		return jaegerTag{Key: k, Type: "string", Value: string(raw)}
	}
	switch x := x.(type) {
	case string:
		return jaegerTag{Key: k, Type: "string", Value: x}
	case bool:
		return jaegerTag{Key: k, Type: "bool", Value: x}
	case json.Number:
		if !strings.ContainsAny(x.String(), ".eE") {
			if n, err := x.Int64(); err == nil {
				return jaegerTag{Key: k, Type: "int64", Value: n}
			}
		}
		if f, err := x.Float64(); err == nil {
			return jaegerTag{Key: k, Type: "float64", Value: f}
		}
		return jaegerTag{Key: k, Type: "string", Value: x.String()}
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return jaegerTag{Key: k, Type: "string", Value: string(raw)}
	}
	return jaegerTag{Key: k, Type: "string", Value: b.String()}
}

// tagsOf are attributes as tags, by key, without the keys in skip.
func tagsOf(attrs map[string]json.RawMessage, skip string) []jaegerTag {
	out := make([]jaegerTag, 0, len(attrs))
	for k, v := range attrs {
		if k != skip {
			out = append(out, tagOf(k, v))
		}
	}
	slices.SortFunc(out, func(a, b jaegerTag) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

// jaegerTraceOf is a trace's spans (by start) in Jaeger's JSON: a
// process per distinct resource, p1 the first span's.
func jaegerTraceOf(spans []quickwit.SpanDoc) jaegerTrace {
	tr := jaegerTrace{Spans: make([]jaegerSpan, 0, len(spans)), Processes: map[string]jaegerProcess{}}
	if len(spans) > 0 {
		tr.TraceID = spans[0].TraceID
	}
	procs := map[string]string{}
	for i := range spans {
		d := &spans[i]
		res, _ := json.Marshal(d.ResourceAttributes) // a map's keys sorted
		pk := d.ServiceName + "\x00" + string(res)
		pid, ok := procs[pk]
		if !ok {
			pid = "p" + strconv.Itoa(len(procs)+1)
			procs[pk] = pid
			tr.Processes[pid] = jaegerProcess{ServiceName: d.ServiceName, Tags: tagsOf(d.ResourceAttributes, attrServiceName)}
		}
		s := jaegerSpan{TraceID: d.TraceID, SpanID: d.SpanID, OperationName: d.SpanName, References: []jaegerRef{},
			StartTime: d.StartNs / 1000, Duration: d.DurationNs / 1000, Tags: tagsOf(d.SpanAttributes, ""),
			Logs: make([]jaegerLog, 0, len(d.Events)), ProcessID: pid, Flags: d.Flags}
		if d.ParentSpanID != "" {
			s.References = append(s.References, jaegerRef{RefType: "CHILD_OF", TraceID: d.TraceID, SpanID: d.ParentSpanID})
		}
		for _, l := range d.Links {
			if isTraceID(l.TraceID) && isSpanID(l.SpanID) {
				s.References = append(s.References, jaegerRef{RefType: "FOLLOWS_FROM", TraceID: l.TraceID, SpanID: l.SpanID})
			}
		}
		if k, ok := spanKinds[d.SpanKind]; ok {
			s.Tags = append(s.Tags, jaegerTag{Key: "span.kind", Type: "string", Value: k})
		}
		switch d.StatusCode {
		case 1:
			s.Tags = append(s.Tags, jaegerTag{Key: "otel.status_code", Type: "string", Value: "OK"})
		case 2:
			s.Tags = append(s.Tags, jaegerTag{Key: "otel.status_code", Type: "string", Value: "ERROR"})
		}
		if d.StatusMessage != "" {
			s.Tags = append(s.Tags, jaegerTag{Key: "otel.status_description", Type: "string", Value: d.StatusMessage})
		}
		if d.StatusCode == 2 {
			s.Tags = append(s.Tags, jaegerTag{Key: "error", Type: "bool", Value: true})
		}
		for _, e := range d.Events {
			fields := append(tagsOf(e.Attributes, ""), jaegerTag{Key: "event", Type: "string", Value: e.Name})
			s.Logs = append(s.Logs, jaegerLog{Timestamp: e.TimeNs / 1000, Fields: fields})
		}
		tr.Spans = append(tr.Spans, s)
	}
	return tr
}

// The handlers of /api/v1/jaeger/api/… (traces.md, Reading), each with
// the tenant of the request's token.

func (a *api) jaegerFail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	writeJSON(w, status, jaegerAnswer{Errors: []jaegerError{{Code: status, Msg: msg}}})
}

func (a *api) jaegerRead(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	a.log.Error("read traces", "path", r.URL.Path, "err", err)
	a.jaegerFail(w, r, http.StatusServiceUnavailable, "the sink cannot answer now")
}

func (a *api) jaegerNames(w http.ResponseWriter, r *http.Request, tenant string) {
	sel, err := parseSpanSelector(r.URL.Query(), tenant)
	if err != nil {
		a.jaegerFail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	since := time.Now().Add(-spanNamesLookback).UnixNano()
	var names []string
	if svc := r.PathValue("service"); svc != "" {
		sel.Service = svc
		names, err = a.traces.Operations(r.Context(), sel, since)
	} else {
		names, err = a.traces.Services(r.Context(), sel, since)
	}
	if err != nil {
		a.jaegerRead(w, r, err)
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, jaegerAnswer{Data: names, Total: len(names)})
}

func (a *api) jaegerTrace(w http.ResponseWriter, r *http.Request, tenant string) {
	sel, err := parseSpanSelector(r.URL.Query(), tenant)
	if err != nil {
		a.jaegerFail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if sel.TraceID, err = parseTraceID(r.PathValue("traceID")); err != nil {
		a.jaegerFail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	spans, err := a.traces.Trace(r.Context(), sel)
	if err != nil {
		a.jaegerRead(w, r, err)
		return
	}
	if len(spans) == 0 {
		a.jaegerFail(w, r, http.StatusNotFound, "trace not found")
		return
	}
	writeJSON(w, http.StatusOK, jaegerAnswer{Data: []jaegerTrace{jaegerTraceOf(spans)}, Total: 1})
}

func (a *api) jaegerFind(w http.ResponseWriter, r *http.Request, tenant string) {
	q, limit, err := parseTraceSearch(r.URL.Query(), tenant, time.Now())
	if err != nil {
		a.jaegerFail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	found, err := a.traces.Find(r.Context(), q, limit)
	if err != nil {
		a.jaegerRead(w, r, err)
		return
	}
	out := make([]jaegerTrace, 0, len(found))
	for _, spans := range found {
		out = append(out, jaegerTraceOf(spans))
	}
	writeJSON(w, http.StatusOK, jaegerAnswer{Data: out, Total: len(out)})
}
