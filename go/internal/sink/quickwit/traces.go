// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package quickwit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// SpanDoc is a span as the trace index holds it (traces.md, The index).
// Attributes are JSON values as they came from OTLP's AnyValue.
type SpanDoc struct {
	TS          int64  `json:"ts"`
	Tenant      string `json:"tenant"`
	Environment string `json:"environment,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Object      string `json:"object,omitempty"`
	Instance    string `json:"instance,omitempty"`
	Job         string `json:"job,omitempty"`
	Source      string `json:"source"`
	Pod         string `json:"pod,omitempty"`
	Node        string `json:"node,omitempty"`
	Replica     uint64 `json:"replica"`
	// Deployment keeps the field name release, as Doc's does.
	Deployment         uint64                     `json:"release"`
	Attempt            uint64                     `json:"attempt"`
	TraceID            string                     `json:"trace_id"`
	SpanID             string                     `json:"span_id"`
	ParentSpanID       string                     `json:"parent_span_id,omitempty"`
	TraceState         string                     `json:"trace_state,omitempty"`
	Flags              uint64                     `json:"flags"`
	ServiceName        string                     `json:"service_name"`
	SpanName           string                     `json:"span_name"`
	SpanKind           uint64                     `json:"span_kind"`
	StartNs            int64                      `json:"start_ns"`
	EndNs              int64                      `json:"end_ns"`
	DurationNs         int64                      `json:"duration_ns"`
	StatusCode         uint64                     `json:"status_code"`
	StatusMessage      string                     `json:"status_message,omitempty"`
	ScopeName          string                     `json:"scope_name,omitempty"`
	ScopeVersion       string                     `json:"scope_version,omitempty"`
	ResourceAttributes map[string]json.RawMessage `json:"resource_attributes"`
	SpanAttributes     map[string]json.RawMessage `json:"span_attributes"`
	Events             []SpanEvent                `json:"events,omitempty"`
	Links              []SpanLink                 `json:"links,omitempty"`
}

// SpanEvent is an event of a span.
type SpanEvent struct {
	TimeNs     int64                      `json:"time_ns"`
	Name       string                     `json:"name"`
	Attributes map[string]json.RawMessage `json:"attributes,omitempty"`
}

// SpanLink is a link of a span to another.
type SpanLink struct {
	TraceID    string                     `json:"trace_id"`
	SpanID     string                     `json:"span_id"`
	TraceState string                     `json:"trace_state,omitempty"`
	Attributes map[string]json.RawMessage `json:"attributes,omitempty"`
}

// SpanQuery selects a tenant's spans: the labels and fields set (empty:
// any), a time range of ts in Unix ns (both ends included; 0: open), a
// range of durations (0: open), and attributes each with its value in
// the span's or the resource's attributes; with Failed only the
// ingress's spans of failed requests (Failed); with Kind only spans of
// that kind (OTLP's number; 0: any); with ServerFailed only the spans of
// failed requests as a server's span says it (ServerFailed); without the
// spans of a request to one of NotPaths (a service's readiness check, by
// url.path or the older http.target); at most Max hits.
// Environments, when not nil, bounds the spans to those of the
// environments it names (a reader's grants on environments, ADR 0031):
// a span of no environment is none of them, and an empty list selects
// nothing.
type SpanQuery struct {
	Tenant                                     string
	Environment, Object, Job, Instance, Source string
	Environments                               []string
	TraceID, Service, Operation                string
	Tags                                       map[string]string
	SinceNs, UntilNs                           int64
	MinDurationNs, MaxDurationNs               int64
	Failed                                     bool
	Kind                                       uint64
	ServerFailed                               bool
	NotPaths                                   []string
	Max                                        int
}

// String is the query in Quickwit's query language.
func (q SpanQuery) String() string {
	var c []string
	term := func(field, v string) {
		if v != "" {
			c = append(c, field+":"+quote(v))
		}
	}
	term("tenant", q.Tenant)
	term("environment", q.Environment)
	if q.Environments != nil {
		var or []string
		for _, e := range q.Environments {
			or = append(or, "environment:"+quote(e))
		}
		if len(or) == 0 {
			or = []string{"NOT *"}
		}
		c = append(c, "("+strings.Join(or, " OR ")+")")
	}
	term("object", q.Object)
	term("job", q.Job)
	term("instance", q.Instance)
	term("source", q.Source)
	term("trace_id", q.TraceID)
	term("service_name", q.Service)
	term("span_name", q.Operation)
	if q.SinceNs > 0 || q.UntilNs > 0 {
		lo, hi := "*", "*"
		if q.SinceNs > 0 {
			lo = rfc3339(q.SinceNs)
		}
		if q.UntilNs > 0 {
			hi = rfc3339(q.UntilNs)
		}
		c = append(c, "ts:["+lo+" TO "+hi+"]")
	}
	if q.MinDurationNs > 0 || q.MaxDurationNs > 0 {
		lo, hi := "*", "*"
		if q.MinDurationNs > 0 {
			lo = strconv.FormatInt(q.MinDurationNs, 10)
		}
		if q.MaxDurationNs > 0 {
			hi = strconv.FormatInt(q.MaxDurationNs, 10)
		}
		c = append(c, "duration_ns:["+lo+" TO "+hi+"]")
	}
	keys := make([]string, 0, len(q.Tags))
	for k := range q.Tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		f, v := fieldPath(k), quote(q.Tags[k])
		c = append(c, "(span_attributes."+f+":"+v+" OR resource_attributes."+f+":"+v+")")
	}
	if q.Failed {
		c = append(c, failedClause)
	}
	if q.Kind > 0 {
		c = append(c, "span_kind:"+strconv.FormatUint(q.Kind, 10))
	}
	if q.ServerFailed {
		c = append(c, serverFailedClause)
	}
	for _, p := range q.NotPaths {
		v := quote(p)
		c = append(c, "NOT span_attributes."+fieldPath(attrPath)+":"+v, "NOT span_attributes."+fieldPath(attrTarget)+":"+v)
	}
	if len(c) == 0 {
		return "*"
	}
	return strings.Join(c, " AND ")
}

// fieldPath is an attribute's key as a path of the query language: ASCII
// punctuation but . _ - @ $ / escaped with a backslash. A dot separates
// the path's parts, as Quickwit indexes a key with dots. A key with a
// backslash cannot be written (ValidTagKey).
func fieldPath(k string) string {
	var b strings.Builder
	for _, r := range k {
		if r < 0x80 && !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-@$/", r)) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ValidTagKey reports whether an attribute's key can be searched: not
// empty, without a backslash or a control character.
func ValidTagKey(k string) bool {
	return k != "" && !strings.ContainsFunc(k, func(r rune) bool { return r == '\\' || r < 0x20 || r == 0x7f })
}

// Match reports whether a span passes q, as the index would answer it,
// for spans not in the index yet (the time range, the labels, the
// fields, the durations and the attributes).
func (q *SpanQuery) Match(d *SpanDoc) bool {
	eq := func(want, got string) bool { return want == "" || want == got }
	if !eq(q.Tenant, d.Tenant) || !eq(q.Environment, d.Environment) || !eq(q.Object, d.Object) || !eq(q.Job, d.Job) ||
		!eq(q.Instance, d.Instance) || !eq(q.Source, d.Source) || !eq(q.TraceID, d.TraceID) ||
		!eq(q.Service, d.ServiceName) || !eq(q.Operation, d.SpanName) {
		return false
	}
	if q.Environments != nil && (d.Environment == "" || !slices.Contains(q.Environments, d.Environment)) {
		return false
	}
	if (q.SinceNs > 0 && d.TS < q.SinceNs) || (q.UntilNs > 0 && d.TS > q.UntilNs) {
		return false
	}
	if (q.MinDurationNs > 0 && d.DurationNs < q.MinDurationNs) || (q.MaxDurationNs > 0 && d.DurationNs > q.MaxDurationNs) {
		return false
	}
	for k, v := range q.Tags {
		if !valueIs(d.SpanAttributes[k], v) && !valueIs(d.ResourceAttributes[k], v) {
			return false
		}
	}
	if q.Kind > 0 && d.SpanKind != q.Kind {
		return false
	}
	if q.ServerFailed && !ServerFailed(d) {
		return false
	}
	for _, p := range q.NotPaths {
		if valueIs(d.SpanAttributes[attrPath], p) || valueIs(d.SpanAttributes[attrTarget], p) {
			return false
		}
	}
	return !q.Failed || Failed(d)
}

// valueIs reports whether a JSON value is v: a string equal to it, a
// bool or a number written as v, or a number of v's value.
func valueIs(raw json.RawMessage, v string) bool {
	if len(raw) == 0 {
		return false
	}
	var x any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&x); err != nil {
		return false
	}
	switch x := x.(type) {
	case string:
		return x == v
	case bool:
		return strconv.FormatBool(x) == v
	case json.Number:
		if x.String() == v {
			return true
		}
		a, err1 := x.Float64()
		b, err2 := strconv.ParseFloat(v, 64)
		return err1 == nil && err2 == nil && a == b
	}
	return false
}

// TraceMappingVersion is the version of TraceIndexConfig, in the trace
// index's name (TraceIndexID), as MappingVersion is the log indexes': a
// changed mapping is a new index, and reads search every version until
// the old one ages out. Version 2 is filled by a Kafka source (ADR 0026
// §11), version 3 by one on Apache Kafka, version 4 on a topic of 64
// partitions.
const TraceMappingVersion = 4

// TraceIndexID is the trace index of a retention in days, in prefix, of
// TraceMappingVersion: <prefix>-v<version>-<days>d.
func TraceIndexID(prefix string, days int) string {
	return prefix + "-v" + strconv.Itoa(TraceMappingVersion) + "-" + strconv.Itoa(days) + "d"
}

// TraceIndexConfig is the configuration of the trace index id: its spans
// partitioned as the log indexes' lines are, by tenant, environment and
// job (IndexSettings).
func TraceIndexConfig(id string, s IndexSettings) []byte {
	raw := func(name string, fast bool) map[string]any {
		return map[string]any{"name": name, "type": "text", "tokenizer": "raw", "fast": fast}
	}
	typed := func(name, typ string, fast bool) map[string]any {
		return map[string]any{"name": name, "type": typ, "fast": fast}
	}
	cfg := map[string]any{
		"version":  "0.9",
		"index_id": id,
		"doc_mapping": map[string]any{
			"mode": "strict",
			"field_mappings": []map[string]any{
				{"name": "ts", "type": "datetime", "input_formats": []string{"unix_timestamp"},
					"output_format": "unix_timestamp_nanos", "fast": true, "fast_precision": "nanoseconds"},
				raw("tenant", false), raw("environment", false), raw("kind", false), raw("object", false),
				raw("instance", false), raw("job", false), raw("source", false), raw("pod", false), raw("node", false),
				typed("replica", "u64", false), typed("release", "u64", false), typed("attempt", "u64", false),
				raw("trace_id", true), raw("span_id", false), raw("parent_span_id", false),
				{"name": "trace_state", "type": "text", "indexed": false},
				typed("flags", "u64", false),
				raw("service_name", true), raw("span_name", true),
				typed("span_kind", "u64", false),
				typed("start_ns", "i64", true), typed("end_ns", "i64", true), typed("duration_ns", "i64", true),
				typed("status_code", "u64", false),
				{"name": "status_message", "type": "text", "tokenizer": "default"},
				raw("scope_name", false), raw("scope_version", false),
				{"name": "resource_attributes", "type": "json"},
				{"name": "span_attributes", "type": "json"},
				{"name": "events", "type": "array<json>"},
				{"name": "links", "type": "array<json>"},
			},
			"timestamp_field": "ts",
			"tag_fields":      []string{"tenant", "environment", "job"},
			"partition_key":   partitionKey(s.Partitions),
		},
		"indexing_settings": map[string]any{"commit_timeout_secs": s.CommitSecs},
		"retention":         map[string]any{"period": fmt.Sprintf("%d days", s.RetentionDays), "schedule": "hourly"},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// spanSearch is the body of a search of q's spans.
func spanSearch(q SpanQuery, maxHits int) map[string]any {
	req := map[string]any{"query": q.String(), "max_hits": maxHits, "count_all": false, "format": "json"}
	if q.SinceNs > 0 {
		req["start_timestamp"] = q.SinceNs / 1e9
	}
	if q.UntilNs > 0 {
		req["end_timestamp"] = q.UntilNs/1e9 + 1
	}
	return req
}

func (c *Client) searchJSON(ctx context.Context, pattern string, req map[string]any, out any) error {
	body, _ := json.Marshal(req)
	return c.do(ctx, http.MethodPost, c.search+"/api/v1/"+pattern+"/search", "application/json", body, out)
}

func searchErrors(errs []string) error {
	if len(errs) > 0 {
		return fmt.Errorf("quickwit: search: %s", strings.Join(errs, "; "))
	}
	return nil
}

// SearchSpans answers the spans of q in the indexes pattern names, the
// oldest first, at most q.Max (bounded by MaxHits).
func (c *Client) SearchSpans(ctx context.Context, pattern string, q SpanQuery) ([]SpanDoc, error) {
	req := spanSearch(q, min(max(q.Max, 1), MaxHits))
	req["sort_by"] = "-ts" // ascending (Search)
	var out struct {
		Hits   []SpanDoc `json:"hits"`
		Errors []string  `json:"errors"`
	}
	if err := c.searchJSON(ctx, pattern, req, &out); err != nil {
		return nil, err
	}
	return out.Hits, searchErrors(out.Errors)
}

// SpanTerms answers the values of a fast field (service_name,
// span_name) of q's spans, at most size, in no order.
func (c *Client) SpanTerms(ctx context.Context, pattern string, q SpanQuery, field string, size int) ([]string, error) {
	req := spanSearch(q, 0)
	req["aggs"] = map[string]any{"names": map[string]any{"terms": map[string]any{"field": field, "size": size}}}
	var out struct {
		Aggregations struct {
			Names struct {
				Buckets []struct {
					Key string `json:"key"`
				} `json:"buckets"`
			} `json:"names"`
		} `json:"aggregations"`
		Errors []string `json:"errors"`
	}
	if err := c.searchJSON(ctx, pattern, req, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Aggregations.Names.Buckets))
	for _, b := range out.Aggregations.Names.Buckets {
		names = append(names, b.Key)
	}
	return names, searchErrors(out.Errors)
}

// TraceHit is a trace of a search: its ID and the start of its newest
// span that matched (Unix ns, to about a microsecond).
type TraceHit struct {
	TraceID  string
	NewestNs int64
}

// NewestTraces answers the traces of q's spans, at most n, those of the
// newest spans first.
func (c *Client) NewestTraces(ctx context.Context, pattern string, q SpanQuery, n int) ([]TraceHit, error) {
	req := spanSearch(q, 0)
	req["aggs"] = map[string]any{"traces": map[string]any{
		"terms": map[string]any{"field": "trace_id", "size": n, "order": map[string]any{"newest": "desc"}},
		"aggs":  map[string]any{"newest": map[string]any{"max": map[string]any{"field": "start_ns"}}},
	}}
	var out struct {
		Aggregations struct {
			Traces struct {
				Buckets []struct {
					Key    string `json:"key"`
					Newest struct {
						Value float64 `json:"value"`
					} `json:"newest"`
				} `json:"buckets"`
			} `json:"traces"`
		} `json:"aggregations"`
		Errors []string `json:"errors"`
	}
	if err := c.searchJSON(ctx, pattern, req, &out); err != nil {
		return nil, err
	}
	hits := make([]TraceHit, 0, len(out.Aggregations.Traces.Buckets))
	for _, b := range out.Aggregations.Traces.Buckets {
		hits = append(hits, TraceHit{TraceID: b.Key, NewestNs: int64(b.Newest.Value)})
	}
	return hits, searchErrors(out.Errors)
}
