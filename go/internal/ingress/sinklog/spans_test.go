// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sinklog

import (
	"encoding/hex"
	"maps"
	"testing"
	"time"

	tracev1 "github.com/gardenlinux/feos/go/gen/otlp/trace/v1"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// A record is a server span of its (environment, service) resource,
// with the contract's resource attributes, fields and attributes.
func TestSpans(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 123456789, time.UTC)
	rec := func(env, svc string, status int, outcome string) wire.AccessLogEntry {
		return wire.AccessLogEntry{Time: t0, DurationNs: 1500, Env: env, Project: "acme", Service: svc, Instance: "i1",
			Host: "app.apps.example", Method: "POST", Path: "/a/b", Proto: "HTTP/2.0", Status: status, Outcome: outcome,
			BytesIn: 3, BytesOut: 12, ClientIP: "203.0.113.9", UserAgent: "ua", Attempts: 2, HeldNs: 7,
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", ParentSpanID: "b7ad6b7169203331",
			TraceState: "congo=t61rcWkgMzE", TraceFlags: 0x01}
	}
	started := rec("e1", "web", 0, "aborted")
	started.ParentSpanID, started.TraceState, started.Instance, started.UserAgent = "", "", "", ""
	started.Proto = "HTTP/1.1"
	badID := rec("e1", "web", 200, "2xx")
	badID.SpanID = "zz"
	req, skipped := Spans("in-0", []wire.AccessLogEntry{
		rec("e1", "web", 200, "2xx"), rec("e1", "api", 503, "5xx"), started,
		{Env: "e1", Service: "web", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7"}, badID,
	})
	if skipped != 2 || len(req.ResourceSpans) != 2 {
		t.Fatalf("skipped %d, %d resources", skipped, len(req.ResourceSpans))
	}
	web := req.ResourceSpans[0]
	want := map[string]any{
		"service.name": "ferun-ingress", "ferun.pod": "ingress/in-0", "ferun.source": "ingress", "ferun.project": "acme",
		"ferun.tenant":      "acme", // KEEP-TENANT: for the sink before ADR 0061
		"ferun.environment": "e1", "ferun.kind": "service", "ferun.object": "web",
	}
	if got := attrs(web.GetResource().GetAttributes()); !maps.Equal(got, want) {
		t.Errorf("resource %v", got)
	}
	spans := web.GetScopeSpans()[0].GetSpans()
	if len(spans) != 2 {
		t.Fatalf("%d spans of web", len(spans))
	}
	s := spans[0]
	if hex.EncodeToString(s.GetTraceId()) != "4bf92f3577b34da6a3ce929d0e0e4736" ||
		hex.EncodeToString(s.GetSpanId()) != "00f067aa0ba902b7" ||
		hex.EncodeToString(s.GetParentSpanId()) != "b7ad6b7169203331" || s.GetTraceState() != "congo=t61rcWkgMzE" ||
		s.GetFlags() != 1 || s.GetName() != "POST" || s.GetKind() != tracev1.Span_SPAN_KIND_SERVER ||
		s.GetStartTimeUnixNano() != uint64(t0.UnixNano()) || s.GetEndTimeUnixNano() != uint64(t0.UnixNano()+1500) ||
		s.GetStatus() != nil {
		t.Errorf("span %v", s)
	}
	wantAttrs := map[string]any{
		"http.request.method": "POST", "url.path": "/a/b", "server.address": "app.apps.example",
		"network.protocol.version": "2", "http.response.status_code": int64(200), "client.address": "203.0.113.9",
		"user_agent.original": "ua", "http.request.body.size": int64(3), "http.response.body.size": int64(12),
		"ferun.outcome": "2xx", "ferun.attempts": int64(2), "ferun.held_ns": int64(7), "ferun.upstream_instance": "i1",
	}
	if got := attrs(s.GetAttributes()); !maps.Equal(got, wantAttrs) {
		t.Errorf("attributes %v", got)
	}

	// A started trace has no parent; an aborted request is an error, and
	// what is empty is left out.
	s = spans[1]
	if s.GetParentSpanId() != nil || s.GetTraceState() != "" ||
		s.GetStatus().GetCode() != tracev1.Status_STATUS_CODE_ERROR || s.GetStatus().GetMessage() != "aborted" {
		t.Errorf("started span %v", s)
	}
	a := attrs(s.GetAttributes())
	for _, k := range []string{"http.response.status_code", "user_agent.original", "ferun.upstream_instance"} {
		if _, ok := a[k]; ok {
			t.Errorf("empty %s set", k)
		}
	}
	if a["network.protocol.version"] != "1.1" {
		t.Errorf("version %v", a["network.protocol.version"])
	}
	api := req.ResourceSpans[1].GetScopeSpans()[0].GetSpans()
	if len(api) != 1 || api[0].GetStatus().GetMessage() != "5xx" {
		t.Errorf("api's spans %v", api)
	}

	if req, skipped := Spans("in-0", []wire.AccessLogEntry{{Env: "e1"}}); req != nil || skipped != 1 {
		t.Errorf("no span: %v %d", req, skipped)
	}
}

func TestProtocolVersion(t *testing.T) {
	for in, want := range map[string]string{"HTTP/1.0": "1.0", "HTTP/1.1": "1.1", "HTTP/2.0": "2", "HTTP/3.0": "3"} {
		if got := protocolVersion(in); got != want {
			t.Errorf("protocolVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
