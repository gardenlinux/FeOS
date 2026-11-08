// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sinklog

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

func TestLevel(t *testing.T) {
	for _, c := range []struct {
		status  int
		outcome string
		want    string
	}{
		{200, "2xx", "info"}, {101, "upgraded", "info"}, {301, "3xx", "info"},
		{404, "4xx", "warn"}, {429, "4xx", "warn"},
		{500, "5xx", "error"}, {503, "5xx", "error"},
		{200, "aborted", "error"}, {0, "aborted", "error"},
	} {
		if got := Level(c.status, c.outcome); got != c.want {
			t.Errorf("Level(%d, %q) = %q, want %q", c.status, c.outcome, got, c.want)
		}
	}
}

func attrs(kvs []*commonv1.KeyValue) map[string]any {
	m := map[string]any{}
	for _, kv := range kvs {
		switch v := kv.GetValue().GetValue().(type) {
		case *commonv1.AnyValue_StringValue:
			m[kv.GetKey()] = v.StringValue
		case *commonv1.AnyValue_IntValue:
			m[kv.GetKey()] = v.IntValue
		}
	}
	return m
}

// A record is a JSON line of its (environment, service) stream, numbered
// from 0 in it, across batches; the stream's key and labels are the
// contract's.
func TestBatch(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.FixedZone("x", 3600))
	rec := func(env, svc, path string, status int, outcome string) wire.AccessLogEntry {
		return wire.AccessLogEntry{Time: t0, DurationNs: 1500, Env: env, Tenant: "acme", Service: svc, Instance: "i1",
			Host: "app.apps.example", Method: "GET", Path: path, Proto: "HTTP/2.0", Status: status, Outcome: outcome,
			BytesIn: 3, BytesOut: 12, ClientIP: "203.0.113.9", UserAgent: "ua", Attempts: 2, HeldNs: 7,
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", ParentSpanID: "b7ad6b7169203331"}
	}
	s := NewStreams("in-0", "run-1")
	req, taken, skipped := s.Batch([]wire.AccessLogEntry{
		rec("e1", "web", "/a", 200, "2xx"), rec("e1", "api", "/b", 503, "5xx"), rec("e1", "web", "/c", 404, "4xx"),
		{Env: "e1", Service: "web"},
	})
	if taken != 4 || skipped != 1 || len(req.ResourceLogs) != 2 {
		t.Fatalf("taken %d, skipped %d, %d streams", taken, skipped, len(req.ResourceLogs))
	}
	web := req.ResourceLogs[0]
	want := map[string]any{
		"ferun.pod": "ingress/in-0", "ferun.container": "e1/web", "ferun.log_id": "run-1", "ferun.tenant": "acme",
		"ferun.environment": "e1", "ferun.kind": "service", "ferun.object": "web", "ferun.source": "ingress",
	}
	if got := attrs(web.GetResource().GetAttributes()); !maps.Equal(got, want) {
		t.Errorf("resource %v", got)
	}
	recs := web.GetScopeLogs()[0].GetLogRecords()
	if len(recs) != 2 {
		t.Fatalf("%d records of web", len(recs))
	}
	for i, r := range recs {
		if a := attrs(r.GetAttributes()); !maps.Equal(a, map[string]any{"log.offset": int64(i), "log.next_offset": int64(i + 1)}) {
			t.Errorf("record %d: %v", i, a)
		}
		if r.GetTimeUnixNano() != uint64(t0.UnixNano()) {
			t.Errorf("record %d: time %d", i, r.GetTimeUnixNano())
		}
	}
	// The line: every field of ferund's AccessLogRecord, and the level.
	var line map[string]any
	if err := json.Unmarshal([]byte(recs[1].GetBody().GetStringValue()), &line); err != nil {
		t.Fatal(err)
	}
	keys := slices.Sorted(maps.Keys(line))
	wantKeys := []string{"attempts", "bytesIn", "bytesOut", "clientIp", "durationNs", "heldNs", "host", "ingress",
		"instance", "level", "method", "outcome", "parentSpanId", "path", "proto", "service", "spanId", "status",
		"time", "traceId", "tsNs", "userAgent"}
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("keys %v", keys)
	}
	if line["tsNs"] != "1793444400000000000" || line["time"] != "2026-10-31T11:00:00Z" || line["level"] != "warn" ||
		line["ingress"] != "in-0" || line["path"] != "/c" || line["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("line %v", line)
	}
	api := req.ResourceLogs[1].GetScopeLogs()[0].GetLogRecords()
	if len(api) != 1 || !strings.Contains(api[0].GetBody().GetStringValue(), `"level":"error"`) {
		t.Errorf("api's record %v", api)
	}

	// The next batch goes on with the streams' numbers.
	req, _, _ = s.Batch([]wire.AccessLogEntry{rec("e1", "web", "/d", 200, "2xx")})
	if a := attrs(req.ResourceLogs[0].GetScopeLogs()[0].GetLogRecords()[0].GetAttributes()); a["log.offset"] != int64(2) {
		t.Errorf("the next batch's offset %v", a)
	}
	// Only records without a tenant: no request.
	if req, taken, skipped := s.Batch([]wire.AccessLogEntry{{Env: "e1"}}); req != nil || taken != 1 || skipped != 1 {
		t.Errorf("no record: %v %d %d", req, taken, skipped)
	}
}

// A batch holds at most MaxRecords records and MaxBytes of lines, one
// record at least.
func TestBatchBounds(t *testing.T) {
	s := NewStreams("in-0", "run-1")
	e := wire.AccessLogEntry{Time: time.Now(), Env: "e1", Tenant: "acme", Service: "web", Status: 200, Outcome: "2xx"}
	many := slices.Repeat([]wire.AccessLogEntry{e}, MaxRecords+5)
	if _, taken, _ := s.Batch(many); taken != MaxRecords {
		t.Errorf("taken %d", taken)
	}
	e.Path = "/" + strings.Repeat("p", 2000)
	big := slices.Repeat([]wire.AccessLogEntry{e}, MaxRecords)
	req, taken, _ := s.Batch(big)
	size := 0
	for _, r := range req.ResourceLogs[0].GetScopeLogs()[0].GetLogRecords() {
		size += len(r.GetBody().GetStringValue())
	}
	if taken >= MaxRecords || size > MaxBytes || size+2500 < MaxBytes {
		t.Errorf("taken %d, %d bytes", taken, size)
	}
	e.Path = "/" + strings.Repeat("p", MaxBytes)
	if req, taken, _ := s.Batch([]wire.AccessLogEntry{e, e}); taken != 1 || len(req.ResourceLogs[0].GetScopeLogs()[0].GetLogRecords()) != 1 {
		t.Errorf("an oversized record: taken %d", taken)
	}
}

// A record's reason goes into its line, and a record without one keeps
// the line as before.
func TestLineReason(t *testing.T) {
	e := wire.AccessLogEntry{Env: "e1", Service: "web", Status: 403, Outcome: "4xx",
		Reason: wire.ReasonRouteRefusedClient}
	b, err := json.Marshal(NewLine("in-0", e))
	if err != nil || !strings.Contains(string(b), `"reason":"route-refused-client"`) {
		t.Fatalf("line %s, %v", b, err)
	}
	e.Reason = ""
	if b, err = json.Marshal(NewLine("in-0", e)); err != nil || strings.Contains(string(b), `"reason"`) {
		t.Fatalf("line without a reason %s, %v", b, err)
	}
}
