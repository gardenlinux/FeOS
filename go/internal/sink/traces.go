// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"

	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
)

// TraceIndex is the trace index (docs/architecture/traces.md): Quickwit
// (quickwit.Client) or a test's; its Kafka source fills it from its
// topic (ADR 0026 §11).
type TraceIndex interface {
	SourceIndex
	SearchSpans(ctx context.Context, pattern string, q quickwit.SpanQuery) ([]quickwit.SpanDoc, error)
	SpanTerms(ctx context.Context, pattern string, q quickwit.SpanQuery, field string, size int) ([]string, error)
	NewestTraces(ctx context.Context, pattern string, q quickwit.SpanQuery, n int) ([]quickwit.TraceHit, error)
	SpanHistogram(ctx context.Context, pattern string, q quickwit.SpanQuery, step time.Duration, durations bool) (quickwit.Histogram, error)
}

var _ TraceIndex = (*quickwit.Client)(nil)

// Traces is the sink's spans: the gate of their ingest into the trace
// topic, and the reads of the index and the topic's tail.
type Traces struct {
	db    *db.DB
	index TraceIndex
	pipes *pipes
	cfg   Config
	log   *slog.Logger
}

// NewTraces returns the spans over d, the trace index and the topics.
func NewTraces(d *db.DB, index TraceIndex, log Log, cfg Config, logger *slog.Logger) *Traces {
	return &Traces{db: d, index: index, pipes: newPipes(log, index, cfg), cfg: cfg, log: logger}
}

// indexID is the trace index: <prefix>-v<mapping version>-<days>d.
func (t *Traces) indexID() (string, int) {
	days := durationDays(t.cfg.TraceRetention)
	return quickwit.TraceIndexID(t.cfg.TraceIndexPrefix, days), days
}

// pattern names every trace index of the sink.
func (t *Traces) pattern() string { return t.cfg.TraceIndexPrefix + "-*" }

// attrNode and attrServiceName are resource attributes of spans.
const (
	attrNode        = "ferun.node"
	attrServiceName = "service.name"
	// unknownService is the service of a resource without one.
	unknownService = "unknown_service"
)

// maxSafeInt is the largest integer a JSON number holds exactly in every
// reader (2^53).
const maxSafeInt = 1 << 53

// anyJSON is an OTLP value as JSON (traces.md, The index): a string, a
// bool, an int (beyond ±2^53 a string), a double (not finite: a string),
// an array, a key-value list as an object, bytes as base64 text; text
// is made valid UTF-8 without NULs, which PostgreSQL's jsonb cannot hold.
func anyJSON(v *commonv1.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return cleanText(x.StringValue)
	case *commonv1.AnyValue_BoolValue:
		return x.BoolValue
	case *commonv1.AnyValue_IntValue:
		if x.IntValue > maxSafeInt || x.IntValue < -maxSafeInt {
			return strconv.FormatInt(x.IntValue, 10)
		}
		return x.IntValue
	case *commonv1.AnyValue_DoubleValue:
		f := x.DoubleValue
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return strconv.FormatFloat(f, 'g', -1, 64)
		}
		// A double stays one when read back: 2 is written 2.0.
		s := strconv.FormatFloat(f, 'g', -1, 64)
		if f == math.Trunc(f) && math.Abs(f) < 1e21 {
			s = strconv.FormatFloat(f, 'f', 1, 64)
		}
		return json.RawMessage(s)
	case *commonv1.AnyValue_ArrayValue:
		out := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			out = append(out, anyJSON(e))
		}
		return out
	case *commonv1.AnyValue_KvlistValue:
		out := make(map[string]any, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			out[cleanText(kv.GetKey())] = anyJSON(kv.GetValue())
		}
		return out
	case *commonv1.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	}
	return nil
}

// attrsJSON is a list of attributes as an object of JSON values; of two
// with the same key the last wins.
func attrsJSON(kvs []*commonv1.KeyValue) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(kvs))
	for _, kv := range kvs {
		b, err := json.Marshal(anyJSON(kv.GetValue()))
		if err != nil {
			// Every value anyJSON makes marshals; keep the key regardless.
			b = []byte("null")
		}
		out[cleanText(kv.GetKey())] = b
	}
	return out
}

// isTraceIDBytes and isSpanIDBytes report whether an ID is valid: 16 or
// 8 bytes, not all zero.
func isTraceIDBytes(b []byte) bool { return len(b) == 16 && !allZero(b) }
func isSpanIDBytes(b []byte) bool  { return len(b) == 8 && !allZero(b) }

// isSpanID reports whether s is a span ID: 16 lowercase hex digits, not
// all zero.
func isSpanID(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(s) == 16 && hex.EncodeToString(b) == s && !allZero(b)
}

// clampNs is a uint64 of ns as an int64.
func clampNs(n uint64) int64 { return int64(min(n, math.MaxInt64)) }
