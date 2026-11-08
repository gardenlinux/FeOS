// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package sinklog writes the ingress's access log into the sink (ADR
// 0026 §8, docs/architecture/sink.md, The ingress's access log): a
// stream per (instance, start of its agent, environment, service), each
// request a JSON line of it, exported over OTLP with the ingress's
// certificate; and each request's server span, in batches of their own
// (docs/architecture/traces.md, The ingress).
package sinklog

import (
	"encoding/json"
	"time"

	otlplogsv1 "github.com/gardenlinux/feos/go/gen/otlp/collector/logs/v1"
	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	logsv1 "github.com/gardenlinux/feos/go/gen/otlp/logs/v1"
	resourcev1 "github.com/gardenlinux/feos/go/gen/otlp/resource/v1"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// A batch's bounds: records, and bytes of their lines.
const (
	MaxRecords = 1000
	MaxBytes   = 1 << 20
)

// The attributes of the contract.
const (
	attrPod         = "ferun.pod"
	attrContainer   = "ferun.container"
	attrLogID       = "ferun.log_id"
	attrTenant      = "ferun.tenant"
	attrEnvironment = "ferun.environment"
	attrKind        = "ferun.kind"
	attrObject      = "ferun.object"
	attrSource      = "ferun.source"
	attrOffset      = "log.offset"
	attrNextOffset  = "log.next_offset"
	// SourceIngress is ferun.source of the ingress's streams.
	SourceIngress = "ingress"
)

// Line is a request's line in the sink: the fields and names of
// ferund's AccessLogRecord (internal/env), and its level. Reason is the
// record's (wire.AccessLogEntry.Reason).
type Line struct {
	TsNs         int64     `json:"tsNs,string"`
	Time         time.Time `json:"time"`
	DurationNs   int64     `json:"durationNs"`
	Ingress      string    `json:"ingress"`
	Service      string    `json:"service"`
	Instance     string    `json:"instance"`
	Host         string    `json:"host"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Proto        string    `json:"proto"`
	Status       int       `json:"status"`
	Outcome      string    `json:"outcome"`
	Reason       string    `json:"reason,omitempty"`
	Attempts     int       `json:"attempts"`
	HeldNs       int64     `json:"heldNs"`
	BytesIn      int64     `json:"bytesIn"`
	BytesOut     int64     `json:"bytesOut"`
	ClientIP     string    `json:"clientIp"`
	UserAgent    string    `json:"userAgent"`
	TraceID      string    `json:"traceId"`
	SpanID       string    `json:"spanId"`
	ParentSpanID string    `json:"parentSpanId"`
	Level        string    `json:"level"`
}

// Level is a request's level: error for a 5xx or an aborted request,
// warn for a 4xx, else info.
func Level(status int, outcome string) string {
	switch {
	case status >= 500 || outcome == "aborted" || outcome == "5xx":
		return "error"
	case status >= 400 || outcome == "4xx":
		return "warn"
	}
	return "info"
}

// NewLine is the line of a record of the ingress instance name.
func NewLine(name string, e wire.AccessLogEntry) Line {
	t := e.Time.UTC()
	return Line{
		TsNs: t.UnixNano(), Time: t, DurationNs: e.DurationNs, Ingress: name, Service: e.Service,
		Instance: e.Instance, Host: e.Host, Method: e.Method, Path: e.Path, Proto: e.Proto, Status: e.Status,
		Outcome: e.Outcome, Reason: e.Reason, Attempts: e.Attempts, HeldNs: e.HeldNs, BytesIn: e.BytesIn, BytesOut: e.BytesOut,
		ClientIP: e.ClientIP, UserAgent: e.UserAgent, TraceID: e.TraceID, SpanID: e.SpanID,
		ParentSpanID: e.ParentSpanID, Level: Level(e.Status, e.Outcome),
	}
}

type streamKey struct{ env, service string }

// Streams numbers the records of one run of an ingress instance in their
// streams: name is the instance's, start the run's ID (a UUID).
type Streams struct {
	name, start string
	next        map[streamKey]int64
}

// NewStreams returns the streams of a run, all empty.
func NewStreams(name, start string) *Streams {
	return &Streams{name: name, start: start, next: map[streamKey]int64{}}
}

// Batch makes the request of the first records of recs, at most
// MaxRecords of them and MaxBytes of their lines (one at least): each
// record its stream's next offset, which it uses up. It returns the
// request, nil when it holds no record, how many records it took from
// recs, and how many of them it skipped: those without an environment or
// tenant. A request is sent until the sink takes or refuses it; Batch
// never numbers a record twice.
func (s *Streams) Batch(recs []wire.AccessLogEntry) (req *otlplogsv1.ExportLogsServiceRequest, taken, skipped int) {
	byKey := map[streamKey]*logsv1.ResourceLogs{}
	var out []*logsv1.ResourceLogs
	size := 0
	for _, e := range recs {
		if taken == MaxRecords {
			break
		}
		if e.Env == "" || e.Tenant == "" {
			taken++
			skipped++
			continue
		}
		text, err := json.Marshal(NewLine(s.name, e))
		if err != nil {
			taken++
			skipped++
			continue
		}
		if size > 0 && size+len(text) > MaxBytes {
			break
		}
		size += len(text)
		taken++
		k := streamKey{e.Env, e.Service}
		rl := byKey[k]
		if rl == nil {
			rl = &logsv1.ResourceLogs{
				Resource:  &resourcev1.Resource{Attributes: s.resource(k, e.Tenant)},
				ScopeLogs: []*logsv1.ScopeLogs{{}},
			}
			byKey[k] = rl
			out = append(out, rl)
		}
		off := s.next[k]
		s.next[k] = off + 1
		sl := rl.ScopeLogs[0]
		sl.LogRecords = append(sl.LogRecords, &logsv1.LogRecord{
			TimeUnixNano: uint64(max(e.Time.UnixNano(), 0)),
			Body:         str(string(text)),
			Attributes:   []*commonv1.KeyValue{kv(attrOffset, num(off)), kv(attrNextOffset, num(off+1))},
		})
	}
	if len(out) == 0 {
		return nil, taken, skipped
	}
	return &otlplogsv1.ExportLogsServiceRequest{ResourceLogs: out}, taken, skipped
}

// resource is a stream's resource attributes: its key and the service's
// labels.
func (s *Streams) resource(k streamKey, tenant string) []*commonv1.KeyValue {
	return []*commonv1.KeyValue{
		kv(attrPod, str("ingress/"+s.name)),
		kv(attrContainer, str(k.env+"/"+k.service)),
		kv(attrLogID, str(s.start)),
		kv(attrTenant, str(tenant)),
		kv(attrEnvironment, str(k.env)),
		kv(attrKind, str("service")),
		kv(attrObject, str(k.service)),
		kv(attrSource, str(SourceIngress)),
	}
}

func kv(k string, v *commonv1.AnyValue) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: v}
}

func str(s string) *commonv1.AnyValue {
	return &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: s}}
}

func num(n int64) *commonv1.AnyValue {
	return &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: n}}
}
