// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	otlptracev1 "github.com/gardenlinux/feos/go/gen/otlp/collector/trace/v1"
	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// maxExportSpans bounds the spans of a request.
const maxExportSpans = 10000

// traceIngest is TraceService on the ingest's gRPC server, behind the
// same authorize as the logs.
type traceIngest struct {
	otlptracev1.UnimplementedTraceServiceServer
	traces *Traces
	log    *slog.Logger
}

// Export stores a request's valid spans and answers once they are
// committed; the others are counted in partial_success.
func (in *traceIngest) Export(ctx context.Context, req *otlptracev1.ExportTraceServiceRequest) (*otlptracev1.ExportTraceServiceResponse, error) {
	spans, rejected, err := parseTraceExport(req, peerOf(ctx), time.Now())
	if errors.Is(err, errNotYours) {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := in.traces.Ingest(ctx, spans); err != nil {
		if errors.Is(err, ErrTraceQuota) {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		in.log.Error("store spans", "peer", nodeOf(ctx), "err", err)
		return nil, status.Error(codes.Unavailable, "the sink cannot store the spans now")
	}
	resp := &otlptracev1.ExportTraceServiceResponse{}
	if rejected > 0 {
		resp.PartialSuccess = &otlptracev1.ExportTracePartialSuccess{RejectedSpans: rejected,
			ErrorMessage: fmt.Sprintf("%d spans without a valid trace_id (16 bytes, not all zero) or span_id (8 bytes, not all zero)", rejected)}
	}
	return resp, nil
}

// traceSpan is a span to store: its document and the document's JSON.
type traceSpan struct {
	doc quickwit.SpanDoc
	raw []byte
}

// parseTraceExport reads a request into documents (traces.md, The sink).
// A ResourceSpans without ferun.tenant and ferun.pod, or of another peer
// (a node's: ferun.source not ingress, ferun.pod not ingress/…; an
// ingress's: ferun.source ingress, ferun.pod ingress/<its name>), refuses
// the request with errNotYours; more than maxExportSpans spans refuse it
// too. A span without a valid trace or span ID is left out and counted in
// rejected. now is the time of a span without a start.
func parseTraceExport(req *otlptracev1.ExportTraceServiceRequest, p ingestPeer, now time.Time) ([]traceSpan, int64, error) {
	count := 0
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			count += len(ss.GetSpans())
		}
	}
	if count > maxExportSpans {
		return nil, 0, fmt.Errorf("%d spans; at most %d a request", count, maxExportSpans)
	}
	var out []traceSpan
	var rejected int64
	for i, rs := range req.GetResourceSpans() {
		text := map[string]string{}
		for _, kv := range rs.GetResource().GetAttributes() {
			text[kv.GetKey()] = anyText(kv.GetValue())
		}
		tenant, pod := text[attrTenant], text[attrPod]
		if tenant == "" || pod == "" {
			return nil, 0, fmt.Errorf("resource %d: %w: %s and %s are required", i, errNotYours, attrTenant, attrPod)
		}
		source := SourceContainer
		if text[attrSource] == SourceIngress {
			source = SourceIngress
			if p.ingress == "" || pod != ingressPod+p.ingress {
				return nil, 0, fmt.Errorf("resource %d: %w: spans of %s %s are the ingress's of its %s alone", i, errNotYours,
					attrSource, SourceIngress, attrPod)
			}
		} else if p.node == "" || strings.HasPrefix(pod, ingressPod) {
			return nil, 0, fmt.Errorf("resource %d: %w: spans without %s %s are a node's, of a pod not %s…", i, errNotYours,
				attrSource, SourceIngress, ingressPod)
		}
		num := func(k string) uint64 {
			n, _ := strconv.ParseUint(text[k], 10, 64)
			return n
		}
		service := cleanText(text[attrServiceName])
		if service == "" {
			service = unknownService
		}
		base := quickwit.SpanDoc{
			Tenant: cleanText(tenant), Environment: cleanText(text[attrEnvironment]), Kind: cleanText(text[attrKind]),
			Object: cleanText(text[attrObject]), Instance: cleanText(text[attrInstance]), Job: cleanText(text[attrJob]),
			Source: source, Pod: cleanText(pod), Node: cleanText(text[attrNode]),
			Replica: num(attrReplica), Deployment: num(deploymentAttr(text)), Attempt: num(attrAttempt),
			ServiceName: service, ResourceAttributes: attrsJSON(rs.GetResource().GetAttributes()),
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				if !isTraceIDBytes(sp.GetTraceId()) || !isSpanIDBytes(sp.GetSpanId()) {
					rejected++
					continue
				}
				d := base
				d.ScopeName, d.ScopeVersion = cleanText(ss.GetScope().GetName()), cleanText(ss.GetScope().GetVersion())
				d.TraceID, d.SpanID = hex.EncodeToString(sp.GetTraceId()), hex.EncodeToString(sp.GetSpanId())
				if isSpanIDBytes(sp.GetParentSpanId()) {
					d.ParentSpanID = hex.EncodeToString(sp.GetParentSpanId())
				}
				d.TraceState = cleanText(sp.GetTraceState())
				d.Flags = uint64(sp.GetFlags() & 0xff)
				d.SpanName = cleanText(sp.GetName())
				d.SpanKind = uint64(max(sp.GetKind(), 0))
				d.StartNs, d.EndNs = clampNs(sp.GetStartTimeUnixNano()), clampNs(sp.GetEndTimeUnixNano())
				d.DurationNs = max(d.EndNs-d.StartNs, 0)
				d.TS = d.StartNs
				if d.TS == 0 {
					d.TS = now.UnixNano()
				}
				d.StatusCode = uint64(max(sp.GetStatus().GetCode(), 0))
				d.StatusMessage = cleanText(sp.GetStatus().GetMessage())
				d.SpanAttributes = attrsJSON(sp.GetAttributes())
				for _, e := range sp.GetEvents() {
					ev := quickwit.SpanEvent{TimeNs: clampNs(e.GetTimeUnixNano()), Name: cleanText(e.GetName())}
					if len(e.GetAttributes()) > 0 {
						ev.Attributes = attrsJSON(e.GetAttributes())
					}
					d.Events = append(d.Events, ev)
				}
				for _, l := range sp.GetLinks() {
					ln := quickwit.SpanLink{TraceID: hex.EncodeToString(l.GetTraceId()), SpanID: hex.EncodeToString(l.GetSpanId()),
						TraceState: cleanText(l.GetTraceState())}
					if len(l.GetAttributes()) > 0 {
						ln.Attributes = attrsJSON(l.GetAttributes())
					}
					d.Links = append(d.Links, ln)
				}
				raw, err := json.Marshal(&d)
				if err != nil {
					return nil, 0, fmt.Errorf("resource %d: span %s: %w", i, d.SpanID, err)
				}
				out = append(out, traceSpan{doc: d, raw: raw})
			}
		}
	}
	return out, rejected, nil
}

// ErrTraceQuota is Ingest's refusal of a tenant over its trace quota;
// the other tenants' spans are stored.
var ErrTraceQuota = errors.New("over the tenant's trace quota")

// Ingest stores spans: in one transaction, each tenant's quota checked,
// the spans produced to the trace topic (keyed by their trace IDs), the
// usage counted, and the commit. A tenant over its quota is left out and
// Ingest returns ErrTraceQuota (wrapped) after the commit; a failed
// produce stores nothing, and the sender sends again.
func (t *Traces) Ingest(ctx context.Context, spans []traceSpan) error {
	if len(spans) == 0 {
		return nil
	}
	id, _ := t.indexID()
	n, err := t.pipes.open(ctx, id)
	if err != nil {
		return err
	}
	byTenant := map[string][]*traceSpan{}
	for i := range spans {
		s := &spans[i]
		byTenant[s.doc.Tenant] = append(byTenant[s.doc.Tenant], s)
	}
	tenants := make([]string, 0, len(byTenant))
	for tn := range byTenant {
		tenants = append(tenants, tn)
	}
	slices.Sort(tenants)
	var over []string
	err = t.db.Tx(ctx, func(tx pgx.Tx) error {
		over = over[:0]
		var recs []kafka.Record
		usage := map[string]int64{}
		for _, tn := range tenants {
			if t.cfg.TraceQuota > 0 {
				var used int64
				err := tx.QueryRow(ctx, `SELECT bytes FROM trace_usage WHERE tenant = $1 AND day = (now() AT TIME ZONE 'utc')::date`,
					tn).Scan(&used)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if used >= t.cfg.TraceQuota {
					over = append(over, tn)
					continue
				}
			}
			for _, s := range byTenant[tn] {
				recs = append(recs, spanRecord(s.doc.TraceID, s.raw, n))
				usage[tn] += int64(len(s.raw))
			}
		}
		if err := t.pipes.produce(ctx, id, recs); err != nil {
			return err
		}
		for _, tn := range tenants {
			if usage[tn] == 0 {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO trace_usage (tenant, day, bytes) VALUES ($1, (now() AT TIME ZONE 'utc')::date, $2)
				ON CONFLICT (tenant, day) DO UPDATE SET bytes = trace_usage.bytes + EXCLUDED.bytes`, tn, usage[tn]); err != nil {
				return fmt.Errorf("usage: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(over) > 0 {
		return fmt.Errorf("%w: %v", ErrTraceQuota, over)
	}
	return nil
}
