// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sinklog

import (
	"encoding/hex"
	"strings"

	otlptracev1 "github.com/gardenlinux/feos/go/gen/otlp/collector/trace/v1"
	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	resourcev1 "github.com/gardenlinux/feos/go/gen/otlp/resource/v1"
	tracev1 "github.com/gardenlinux/feos/go/gen/otlp/trace/v1"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// ServiceName is service.name of the ingress's spans.
const ServiceName = "ferun-ingress"

// Spans makes the request of the server spans of recs, those of the
// ingress instance name (docs/architecture/traces.md, The ingress): a
// ResourceSpans per (environment, service). It returns the request, nil
// when it holds no span, and how many records it skipped: those without
// an environment or tenant, or without valid IDs.
func Spans(name string, recs []wire.AccessLogEntry) (req *otlptracev1.ExportTraceServiceRequest, skipped int) {
	byKey := map[streamKey]*tracev1.ResourceSpans{}
	var out []*tracev1.ResourceSpans
	for _, e := range recs {
		sp := span(e)
		if e.Env == "" || e.Tenant == "" || sp == nil {
			skipped++
			continue
		}
		k := streamKey{e.Env, e.Service}
		rs := byKey[k]
		if rs == nil {
			rs = &tracev1.ResourceSpans{
				Resource: &resourcev1.Resource{Attributes: []*commonv1.KeyValue{
					kv("service.name", str(ServiceName)),
					kv(attrPod, str("ingress/"+name)),
					kv(attrSource, str(SourceIngress)),
					kv(attrTenant, str(e.Tenant)),
					kv(attrEnvironment, str(e.Env)),
					kv(attrKind, str("service")),
					kv(attrObject, str(e.Service)),
				}},
				ScopeSpans: []*tracev1.ScopeSpans{{}},
			}
			byKey[k] = rs
			out = append(out, rs)
		}
		ss := rs.ScopeSpans[0]
		ss.Spans = append(ss.Spans, sp)
	}
	if len(out) == 0 {
		return nil, skipped
	}
	return &otlptracev1.ExportTraceServiceRequest{ResourceSpans: out}, skipped
}

// span is a record's server span; nil when its IDs are not valid.
func span(e wire.AccessLogEntry) *tracev1.Span {
	id, err1 := hex.DecodeString(e.TraceID)
	sid, err2 := hex.DecodeString(e.SpanID)
	if err1 != nil || err2 != nil || len(id) != 16 || len(sid) != 8 {
		return nil
	}
	var parent []byte
	if e.ParentSpanID != "" {
		p, err := hex.DecodeString(e.ParentSpanID)
		if err != nil || len(p) != 8 {
			return nil
		}
		parent = p
	}
	start := max(e.Time.UnixNano(), 0)
	sp := &tracev1.Span{
		TraceId:           id,
		SpanId:            sid,
		TraceState:        e.TraceState,
		ParentSpanId:      parent,
		Flags:             uint32(e.TraceFlags),
		Name:              e.Method,
		Kind:              tracev1.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: uint64(start),
		EndTimeUnixNano:   uint64(start + max(e.DurationNs, 0)),
	}
	if Level(e.Status, e.Outcome) == "error" {
		sp.Status = &tracev1.Status{Code: tracev1.Status_STATUS_CODE_ERROR, Message: e.Outcome}
	}
	a := []*commonv1.KeyValue{
		kv("http.request.method", str(e.Method)),
		kv("url.path", str(e.Path)),
		kv("server.address", str(e.Host)),
		kv("network.protocol.version", str(protocolVersion(e.Proto))),
	}
	if e.Status != 0 {
		a = append(a, kv("http.response.status_code", num(int64(e.Status))))
	}
	a = appendStr(a, "client.address", e.ClientIP)
	a = appendStr(a, "user_agent.original", e.UserAgent)
	a = append(a,
		kv("http.request.body.size", num(e.BytesIn)),
		kv("http.response.body.size", num(e.BytesOut)),
		kv("ferun.outcome", str(e.Outcome)),
		kv("ferun.attempts", num(int64(e.Attempts))),
		kv("ferun.held_ns", num(e.HeldNs)),
	)
	a = appendStr(a, "ferun.reason", e.Reason)
	sp.Attributes = appendStr(a, "ferun.upstream_instance", e.Instance)
	return sp
}

// appendStr adds the attribute k when v is not empty.
func appendStr(a []*commonv1.KeyValue, k, v string) []*commonv1.KeyValue {
	if v == "" {
		return a
	}
	return append(a, kv(k, str(v)))
}

// protocolVersion is the version of an HTTP protocol as OpenTelemetry
// names it: "HTTP/1.1" is "1.1", "HTTP/2.0" "2".
func protocolVersion(proto string) string {
	v := strings.TrimPrefix(proto, "HTTP/")
	if major, ok := strings.CutSuffix(v, ".0"); ok && major != "1" {
		return major
	}
	return v
}
