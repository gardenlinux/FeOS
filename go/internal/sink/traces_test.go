// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"

	otlptracev1 "github.com/gardenlinux/feos/go/gen/otlp/collector/trace/v1"
	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	resourcev1 "github.com/gardenlinux/feos/go/gen/otlp/resource/v1"
	tracev1 "github.com/gardenlinux/feos/go/gen/otlp/trace/v1"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// traceB is a trace beside traceA.
const traceB = "0af7651916cd43dd8448eb211c80319c"

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// testSpan is a span of trace with ID id, parent (hex, "" for none),
// from start for dur.
func testSpan(trace, id, parent, name string, start int64, dur time.Duration) *tracev1.Span {
	return &tracev1.Span{TraceId: mustHex(trace), SpanId: mustHex(id), ParentSpanId: mustHex(parent), Name: name,
		Kind: tracev1.Span_SPAN_KIND_SERVER, StartTimeUnixNano: uint64(start), EndTimeUnixNano: uint64(start + int64(dur))}
}

// podAttrs are a pod's resource attributes as the node stamps them.
func podAttrs(tenant, env, object string) map[string]string {
	return map[string]string{attrTenant: tenant, attrPod: "abcdefghjkm", attrNode: "node-1", attrEnvironment: env,
		attrKind: "service", attrObject: object, attrInstance: "i-" + object, attrReplica: "1", attrDeployment: "7",
		attrServiceName: object}
}

func traceRequest(attrs map[string]string, spans ...*tracev1.Span) *otlptracev1.ExportTraceServiceRequest {
	var kvs []*commonv1.KeyValue
	for k, v := range attrs {
		kvs = append(kvs, kv(k, str(v)))
	}
	return &otlptracev1.ExportTraceServiceRequest{ResourceSpans: []*tracev1.ResourceSpans{{
		Resource:   &resourcev1.Resource{Attributes: kvs},
		ScopeSpans: []*tracev1.ScopeSpans{{Scope: &commonv1.InstrumentationScope{Name: "lib", Version: "1.2"}, Spans: spans}},
	}}}
}

func TestAnyJSON(t *testing.T) {
	arr := &commonv1.AnyValue{Value: &commonv1.AnyValue_ArrayValue{ArrayValue: &commonv1.ArrayValue{Values: []*commonv1.AnyValue{
		str("a"), {Value: &commonv1.AnyValue_IntValue{IntValue: 1}}}}}}
	kvl := &commonv1.AnyValue{Value: &commonv1.AnyValue_KvlistValue{KvlistValue: &commonv1.KeyValueList{Values: []*commonv1.KeyValue{
		kv("k", &commonv1.AnyValue{Value: &commonv1.AnyValue_BoolValue{BoolValue: true}})}}}}
	dbl := func(f float64) *commonv1.AnyValue {
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: f}}
	}
	num := func(n int64) *commonv1.AnyValue {
		return &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: n}}
	}
	for _, c := range []struct {
		v    *commonv1.AnyValue
		want string
	}{
		{str("x\x00y"), `"x�y"`},
		{str("\xff"), `"�"`},
		{num(42), `42`},
		{num(1 << 53), `9007199254740992`},
		{num(1<<53 + 1), `"9007199254740993"`},
		{num(-(1<<53 + 1)), `"-9007199254740993"`},
		{dbl(2), `2.0`},
		{dbl(1.5), `1.5`},
		{dbl(1e300), `1e+300`},
		{dbl(math.NaN()), `"NaN"`},
		{dbl(math.Inf(-1)), `"-Inf"`},
		{&commonv1.AnyValue{Value: &commonv1.AnyValue_BytesValue{BytesValue: []byte{1, 2, 255}}}, `"AQL/"`},
		{arr, `["a",1]`},
		{kvl, `{"k":true}`},
		{&commonv1.AnyValue{}, `null`},
		{nil, `null`},
	} {
		b, err := json.Marshal(anyJSON(c.v))
		if err != nil || string(b) != c.want {
			t.Errorf("%v: %s, %v; want %s", c.v, b, err, c.want)
		}
	}
}

func TestParseTraceExport(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	start := now.Add(-time.Second).UnixNano()
	root := testSpan(traceA, "00f067aa0ba902b7", "", "GET /", start, 1500*time.Microsecond)
	root.TraceState, root.Flags = "k=v", 0x101
	root.Status = &tracev1.Status{Code: tracev1.Status_STATUS_CODE_ERROR, Message: "boom"}
	root.Attributes = []*commonv1.KeyValue{kv("http.response.status_code", &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: 500}})}
	root.Events = []*tracev1.Span_Event{{TimeUnixNano: uint64(start + 10), Name: "exception",
		Attributes: []*commonv1.KeyValue{kv("exception.message", str("x"))}}}
	root.Links = []*tracev1.Span_Link{{TraceId: mustHex(traceB), SpanId: mustHex("1111111111111111")}}
	child := testSpan(traceA, "2222222222222222", "00f067aa0ba902b7", "query", start+100, -time.Microsecond)
	child.ParentSpanId = mustHex("0000000000000000") // all zero: no parent
	noStart := testSpan(traceA, "3333333333333333", "", "late", 0, 0)
	badTrace := testSpan("00000000000000000000000000000000", "4444444444444444", "", "x", start, 0)
	badSpan := &tracev1.Span{TraceId: mustHex(traceA), SpanId: []byte{1, 2, 3}}
	attrs := podAttrs("acme", "env1", "api")
	attrs["ferun.foo"] = "bar"
	req := traceRequest(attrs, root, child, noStart, badTrace, badSpan)
	spans, rejected, err := parseTraceExport(req, ingestPeer{node: "node-1"}, now)
	if err != nil || rejected != 2 || len(spans) != 3 {
		t.Fatalf("%d spans, %d rejected, %v", len(spans), rejected, err)
	}
	d := spans[0].doc
	if d.TS != start || d.StartNs != start || d.EndNs != start+1_500_000 || d.DurationNs != 1_500_000 || d.TraceID != traceA ||
		d.SpanID != "00f067aa0ba902b7" || d.ParentSpanID != "" || d.TraceState != "k=v" || d.Flags != 1 || d.SpanKind != 2 ||
		d.StatusCode != 2 || d.StatusMessage != "boom" || d.SpanName != "GET /" || d.ServiceName != "api" ||
		d.ScopeName != "lib" || d.ScopeVersion != "1.2" {
		t.Errorf("span fields: %+v", d)
	}
	if d.Tenant != "acme" || d.Environment != "env1" || d.Kind != "service" || d.Object != "api" || d.Instance != "i-api" ||
		d.Source != SourceContainer || d.Pod != "abcdefghjkm" || d.Node != "node-1" || d.Replica != 1 || d.Deployment != 7 {
		t.Errorf("labels: %+v", d)
	}
	if string(d.SpanAttributes["http.response.status_code"]) != "500" || string(d.ResourceAttributes["ferun.foo"]) != `"bar"` {
		t.Errorf("attributes: %s %s", d.SpanAttributes, d.ResourceAttributes)
	}
	if len(d.Events) != 1 || d.Events[0].TimeNs != start+10 || d.Events[0].Name != "exception" ||
		string(d.Events[0].Attributes["exception.message"]) != `"x"` {
		t.Errorf("events: %+v", d.Events)
	}
	if len(d.Links) != 1 || d.Links[0].TraceID != traceB || d.Links[0].SpanID != "1111111111111111" {
		t.Errorf("links: %+v", d.Links)
	}
	var back quickwit.SpanDoc
	if err := json.Unmarshal(spans[0].raw, &back); err != nil || back.SpanID != d.SpanID || back.DurationNs != d.DurationNs {
		t.Errorf("raw %s: %v", spans[0].raw, err)
	}
	if c := spans[1].doc; c.ParentSpanID != "" || c.DurationNs != 0 || c.EndNs != start+100-1000 {
		t.Errorf("child: %+v", c)
	}
	if c := spans[2].doc; c.TS != now.UnixNano() || c.StartNs != 0 {
		t.Errorf("a span without a start: %+v", c)
	}

	// A resource without a service.name gets unknown_service.
	delete(attrs, attrServiceName)
	if spans, _, err := parseTraceExport(traceRequest(attrs, root), ingestPeer{node: "node-1"}, now); err != nil ||
		spans[0].doc.ServiceName != unknownService {
		t.Errorf("no service: %+v, %v", spans, err)
	}

	// The peers' rules.
	ingress := map[string]string{attrTenant: "acme", attrPod: "ingress/in-0", attrSource: SourceIngress, attrServiceName: "ferun-ingress"}
	for _, c := range []struct {
		what  string
		attrs map[string]string
		peer  ingestPeer
		ok    bool
	}{
		{"a node's pod", podAttrs("acme", "e", "api"), ingestPeer{node: "node-1"}, true},
		{"an ingress's own", ingress, ingestPeer{ingress: "in-0"}, true},
		{"an ingress's of another", ingress, ingestPeer{ingress: "in-1"}, false},
		{"a node's as an ingress", ingress, ingestPeer{node: "node-1"}, false},
		{"an ingress's as a pod", map[string]string{attrTenant: "acme", attrPod: "abcdefghjkm"}, ingestPeer{ingress: "in-0"}, false},
		{"a node's of an ingress/ pod", map[string]string{attrTenant: "acme", attrPod: "ingress/in-0"}, ingestPeer{node: "n"}, false},
		{"no tenant", map[string]string{attrPod: "abcdefghjkm"}, ingestPeer{node: "n"}, false},
		{"no pod", map[string]string{attrTenant: "acme"}, ingestPeer{node: "n"}, false},
	} {
		spans, _, err := parseTraceExport(traceRequest(c.attrs, root), c.peer, now)
		if c.ok && (err != nil || len(spans) != 1) {
			t.Errorf("%s: %v", c.what, err)
		}
		if !c.ok && !errors.Is(err, errNotYours) {
			t.Errorf("%s: %v, want errNotYours", c.what, err)
		}
		if c.ok && c.attrs[attrSource] == SourceIngress && spans[0].doc.Source != SourceIngress {
			t.Errorf("%s: source %q", c.what, spans[0].doc.Source)
		}
	}

	many := make([]*tracev1.Span, maxExportSpans+1)
	for i := range many {
		many[i] = root
	}
	if _, _, err := parseTraceExport(traceRequest(podAttrs("acme", "e", "api"), many...), ingestPeer{node: "n"}, now); err == nil ||
		errors.Is(err, errNotYours) {
		t.Errorf("too many spans: %v", err)
	}
}

func TestParseTraceSearch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	q, limit, err := parseTraceSearch(url.Values{}, "acme", now)
	if err != nil || limit != 20 || q.Tenant != "acme" || q.UntilNs != now.UnixNano() || q.SinceNs != now.Add(-time.Hour).UnixNano() {
		t.Errorf("defaults: %+v %d %v", q, limit, err)
	}
	v := url.Values{"service": {"api"}, "operation": {"GET /"}, "start": {"1000"}, "end": {"2000"}, "minDuration": {"1.5ms"},
		"maxDuration": {"2s"}, "tags": {`{"http.method":"GET","code":500,"ok":true}`}, "limit": {"500"},
		"environment": {"e1"}, "object": {"o"}, "job": {"j"}, "instance": {"i"}, "source": {"ingress"}}
	q, limit, err = parseTraceSearch(v, "acme", now)
	if err != nil || limit != 100 || q.Service != "api" || q.Operation != "GET /" || q.SinceNs != 1_000_000 || q.UntilNs != 2_000_999 ||
		q.MinDurationNs != 1_500_000 || q.MaxDurationNs != 2e9 || q.Tags["http.method"] != "GET" || q.Tags["code"] != "500" ||
		q.Tags["ok"] != "true" || q.Environment != "e1" || q.Object != "o" || q.Job != "j" || q.Instance != "i" || q.Source != "ingress" {
		t.Errorf("all: %+v %d %v", q, limit, err)
	}
	for _, bad := range []url.Values{
		{"start": {"x"}}, {"start": {"3000"}, "end": {"2000"}}, {"minDuration": {"10"}}, {"maxDuration": {"-1s"}},
		{"tags": {"x"}}, {"tags": {`{"a":{"b":1}}`}}, {"tags": {`{"a\\b":"1"}`}}, {"tags": {`{"":"1"}`}},
		{"limit": {"x"}}, {"source": {"pod"}},
	} {
		if _, _, err := parseTraceSearch(bad, "acme", now); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
	for in, want := range map[string]string{traceA: traceA, strings.ToUpper(traceA): traceA, "abc": strings.Repeat("0", 29) + "abc"} {
		if got, err := parseTraceID(in); err != nil || got != want {
			t.Errorf("trace ID %s: %s, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0", "xyz", traceA + "0"} {
		if _, err := parseTraceID(in); err == nil {
			t.Errorf("trace ID %q: no error", in)
		}
	}
}

func TestJaegerTraceShape(t *testing.T) {
	start := int64(1_800_000_000_000_000_000)
	attrs := func(kv ...string) map[string]json.RawMessage {
		m := map[string]json.RawMessage{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = json.RawMessage(kv[i+1])
		}
		return m
	}
	res := attrs("service.name", `"api"`, "ferun.environment", `"e1"`)
	spans := []quickwit.SpanDoc{
		{TraceID: traceA, SpanID: "00f067aa0ba902b7", SpanName: "GET /", SpanKind: 2, StartNs: start, DurationNs: 1_500_000,
			ServiceName: "api", ResourceAttributes: res, Flags: 1, StatusCode: 2, StatusMessage: "boom",
			SpanAttributes: attrs("s", `"x"`, "i", `500`, "f", `2.0`, "b", `true`, "a", `[1,"a"]`, "o", `{"k": 1}`, "big", `1e400`),
			Events:         []quickwit.SpanEvent{{TimeNs: start + 5000, Name: "exception", Attributes: attrs("m", `"x"`)}},
			Links:          []quickwit.SpanLink{{TraceID: traceB, SpanID: "1111111111111111"}, {TraceID: "", SpanID: ""}}},
		{TraceID: traceA, SpanID: "2222222222222222", ParentSpanID: "00f067aa0ba902b7", SpanName: "query", SpanKind: 3,
			StartNs: start + 1000, DurationNs: 999, ServiceName: "api", ResourceAttributes: res, StatusCode: 1},
		{TraceID: traceA, SpanID: "3333333333333333", ParentSpanID: "2222222222222222", SpanName: "SELECT", StartNs: start + 2000,
			ServiceName: "db", ResourceAttributes: attrs("service.name", `"db"`)},
	}
	b, err := json.Marshal(jaegerAnswer{Data: []jaegerTrace{jaegerTraceOf(spans)}, Total: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Data []struct {
			TraceID string `json:"traceID"`
			Spans   []struct {
				TraceID, SpanID, OperationName, ProcessID string
				References                                []map[string]string
				StartTime, Duration                       int64
				Tags                                      []map[string]any
				Logs                                      []struct {
					Timestamp int64
					Fields    []map[string]any
				}
				Warnings *[]string
				Flags    int
			} `json:"spans"`
			Processes map[string]struct {
				ServiceName string           `json:"serviceName"`
				Tags        []map[string]any `json:"tags"`
			} `json:"processes"`
			Warnings *[]string `json:"warnings"`
		} `json:"data"`
		Total  int
		Errors *[]any
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 1 || got.Total != 1 || got.Errors != nil {
		t.Fatalf("answer %s", b)
	}
	tr := got.Data[0]
	if tr.TraceID != traceA || len(tr.Spans) != 3 || len(tr.Processes) != 2 || tr.Warnings != nil {
		t.Fatalf("trace %s", b)
	}
	if p := tr.Processes["p1"]; p.ServiceName != "api" || len(p.Tags) != 1 || p.Tags[0]["key"] != "ferun.environment" ||
		p.Tags[0]["value"] != "e1" || tr.Processes["p2"].ServiceName != "db" {
		t.Errorf("processes %+v", tr.Processes)
	}
	s := tr.Spans[0]
	if s.SpanID != "00f067aa0ba902b7" || s.OperationName != "GET /" || s.StartTime != start/1000 || s.Duration != 1500 ||
		s.ProcessID != "p1" || s.Flags != 1 || s.Warnings != nil {
		t.Errorf("span %+v", s)
	}
	if len(s.References) != 1 || s.References[0]["refType"] != "FOLLOWS_FROM" || s.References[0]["traceID"] != traceB {
		t.Errorf("references %+v", s.References)
	}
	tags := map[string]string{}
	for _, tg := range s.Tags {
		vb, _ := json.Marshal(tg["value"])
		tags[tg["key"].(string)] = tg["type"].(string) + " " + string(vb)
	}
	for k, want := range map[string]string{"s": `string "x"`, "i": "int64 500", "f": "float64 2", "b": "bool true",
		"a": `string "[1,\"a\"]"`, "o": `string "{\"k\":1}"`, "big": `string "1e400"`, "span.kind": `string "server"`,
		"otel.status_code": `string "ERROR"`, "otel.status_description": `string "boom"`, "error": "bool true"} {
		if tags[k] != want {
			t.Errorf("tag %s: %s, want %s", k, tags[k], want)
		}
	}
	if len(s.Logs) != 1 || s.Logs[0].Timestamp != (start+5000)/1000 || len(s.Logs[0].Fields) != 2 ||
		s.Logs[0].Fields[1]["key"] != "event" || s.Logs[0].Fields[1]["value"] != "exception" {
		t.Errorf("logs %+v", s.Logs)
	}
	c := tr.Spans[1]
	if len(c.References) != 1 || c.References[0]["refType"] != "CHILD_OF" || c.References[0]["spanID"] != "00f067aa0ba902b7" ||
		c.References[0]["traceID"] != traceA || c.ProcessID != "p1" || len(c.Logs) != 0 || len(c.Tags) != 2 {
		t.Errorf("child %+v", c)
	}
	if d := tr.Spans[2]; d.ProcessID != "p2" || len(d.Tags) != 0 || d.Tags == nil {
		t.Errorf("db span %+v", d)
	}
}

func TestDedupeSpans(t *testing.T) {
	in := []quickwit.SpanDoc{{SpanID: "b", StartNs: 2}, {SpanID: "a", StartNs: 3}, {SpanID: "b", StartNs: 2}, {SpanID: "c", StartNs: 1}}
	out := dedupeSpans(in)
	if len(out) != 3 || out[0].SpanID != "c" || out[1].SpanID != "b" || out[2].SpanID != "a" {
		t.Errorf("%+v", out)
	}
	if dedupeSpans(nil) != nil {
		t.Error("none: not nil")
	}
}

func TestTraceConfig(t *testing.T) {
	c, err := Parse([]string{"--trace-quota", "2GiB", "--trace-retention", "48h", "--trace-index-prefix", "tr"}, testEnv(nil))
	if err != nil || c.TraceQuota != 2<<30 || c.TraceRetention != 48*time.Hour || c.TraceIndexPrefix != "tr" {
		t.Errorf("%+v, %v", c, err)
	}
	if c, err := Parse(nil, testEnv(nil)); err != nil || c.TraceQuota != 1<<30 || c.TraceRetention != 720*time.Hour ||
		c.TraceIndexPrefix != "ferun-traces" {
		t.Errorf("defaults %+v, %v", c, err)
	}
	for _, args := range [][]string{{"--trace-retention", "0s"}, {"--trace-index-prefix", "ferun-logs"},
		{"--trace-index-prefix", "ferun-logs-x"}, {"--trace-index-prefix", "a*"}} {
		if _, err := Parse(args, testEnv(nil)); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

// testEnv is a getenv of the required settings, and more.
func testEnv(more map[string]string) func(string) string {
	env := map[string]string{EnvName("name"): "sink-0", EnvName("database-url"): "postgres://x/y", EnvName("trust-domain"): testTD,
		EnvName("ferun-url"): "http://ferund:8100", EnvName("kafka-brokers"): "rp:9092",
		EnvName("index-search-url"): "http://qw:7280", EnvServiceToken: "t"}
	for k, v := range more {
		env[k] = v
	}
	return func(k string) string { return env[k] }
}
