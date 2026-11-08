// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import "time"

// The bounds of an access log record's fields, which the ingress cuts
// them to before it records a request: MaxAccessLogPath bounds Path,
// MaxAccessLogUserAgent UserAgent and MaxAccessLogText every other text
// field, in bytes.
const (
	MaxAccessLogPath      = 1024
	MaxAccessLogUserAgent = 256
	MaxAccessLogText      = 256
)

// ReasonRouteRefusedClient is the Reason of a request answered 403
// because its route has clients' blocks (Route.From) and the client's
// address is in none of them (ADR 0034, revised).
const ReasonRouteRefusedClient = "route-refused-client"

// AccessLogEntry is one request the ingress answered for an environment,
// which is also the request's server span (W3C Trace Context): TraceID
// and SpanID name it, ParentSpanID the client's span when the request
// brought a traceparent; TraceState is the tracestate passed on and
// TraceFlags the trace's flags. Path is the escaped path without the query.
// Outcome is how it ended: the status class ("2xx" … "5xx"),
// "upgraded", or "aborted". Reason is why the ingress refused the
// request itself where a record says so (ReasonRouteRefusedClient),
// else "". Instance is the instance the last attempt
// went to, "" when none; Attempts counts the attempts, HeldNs how long
// the request waited for an instance. Tenant is the environment's owner,
// which labels its stream in the sink (ADR 0026 §8), where the agent
// writes it as a line (internal/ingress/sinklog); it never goes to the
// control plane.
type AccessLogEntry struct {
	Time         time.Time `json:"time"`
	DurationNs   int64     `json:"durationNs"`
	Env          string    `json:"env"`
	Tenant       string    `json:"tenant,omitempty"`
	Service      string    `json:"service"`
	Instance     string    `json:"instance"`
	Host         string    `json:"host"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Proto        string    `json:"proto"`
	Status       int       `json:"status"`
	Outcome      string    `json:"outcome"`
	Reason       string    `json:"reason,omitempty"`
	BytesIn      int64     `json:"bytesIn"`
	BytesOut     int64     `json:"bytesOut"`
	ClientIP     string    `json:"clientIp"`
	UserAgent    string    `json:"userAgent"`
	Attempts     int       `json:"attempts"`
	HeldNs       int64     `json:"heldNs"`
	TraceID      string    `json:"traceId"`
	SpanID       string    `json:"spanId"`
	ParentSpanID string    `json:"parentSpanId"`
	TraceState   string    `json:"traceState,omitempty"`
	TraceFlags   byte      `json:"traceFlags"`
}
