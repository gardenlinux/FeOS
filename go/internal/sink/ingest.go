// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	sinkv1 "github.com/gardenlinux/feos/go/gen/feos/sink/v1"
	otlplogsv1 "github.com/gardenlinux/feos/go/gen/otlp/collector/logs/v1"
	otlptracev1 "github.com/gardenlinux/feos/go/gen/otlp/collector/trace/v1"
	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	logsv1 "github.com/gardenlinux/feos/go/gen/otlp/logs/v1"
)

// The ingest's limits: a request's size, an Export's records, the streams
// of a Positions or EndStreams.
const (
	maxIngestMessage = 8 << 20
	maxExportRecords = 10000
	maxKeys          = 1000
)

// A stream's sources: an ingress's access log (ferun.source), and a
// container's output (any other).
const (
	SourceIngress   = "ingress"
	SourceContainer = "container"
	// ingressPod starts the ferun.pod of an ingress's streams, the
	// instance's name follows.
	ingressPod = "ingress/"
)

// The resource and record attributes the sink reads (sink.md, The OTLP
// shape).
const (
	attrPod         = "ferun.pod"
	attrContainer   = "ferun.container"
	attrLogID       = "ferun.log_id"
	attrTenant      = "ferun.tenant"
	attrInstance    = "ferun.instance"
	attrEnvironment = "ferun.environment"
	attrKind        = "ferun.kind"
	attrObject      = "ferun.object"
	attrReplica     = "ferun.replica"
	attrDeployment  = "ferun.deployment"
	// attrRelease is ferun.deployment's earlier word (ADR 0018, "The
	// deployments' word"), which ferund no longer writes: a pod keeps the
	// labels of its insert for its life, so the sink reads it where
	// ferun.deployment is not set.
	attrRelease       = "ferun.release"
	attrJob           = "ferun.job"
	attrAttempt       = "ferun.attempt"
	attrRetentionDays = "ferun.retention_days"
	attrSource        = "ferun.source"
	attrOffset        = "log.offset"
	attrNextOffset    = "log.next_offset"
	attrIOStream      = "log.iostream"
	attrPartial       = "log.partial"
)

// ingest is the ingest's gRPC services over the store.
type ingest struct {
	otlplogsv1.UnimplementedLogsServiceServer
	sinkv1.UnimplementedSinkServiceServer
	store *Store
	ident *Identity
	log   *slog.Logger
}

// newIngestServer is the ingest's gRPC server: TLS of the identity, nodes
// and ingresses only, LogsService, SinkService and TraceService.
func newIngestServer(store *Store, traces *Traces, ident *Identity, log *slog.Logger) *grpc.Server {
	in := &ingest{store: store, ident: ident, log: log}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(ident.TLSConfig())),
		grpc.MaxRecvMsgSize(maxIngestMessage+64<<10),
		grpc.MaxConcurrentStreams(64),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.ChainUnaryInterceptor(in.authorize),
	)
	otlplogsv1.RegisterLogsServiceServer(srv, in)
	sinkv1.RegisterSinkServiceServer(srv, in)
	otlptracev1.RegisterTraceServiceServer(srv, &traceIngest{traces: traces, log: log})
	return srv
}

type peerKey struct{}

// authorize puts the peer, from its verified certificate, into the
// context; a call without one is refused, and SinkService is the nodes'.
func (in *ingest) authorize(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer")
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "not TLS")
	}
	pr, err := in.ident.verifyPeer(ti.State.PeerCertificates)
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "not a node or an ingress: %v", err)
	}
	if pr.node == "" && strings.HasPrefix(info.FullMethod, "/"+sinkv1.SinkService_ServiceDesc.ServiceName+"/") {
		return nil, status.Error(codes.PermissionDenied, "the sink service is the nodes'")
	}
	return h(context.WithValue(ctx, peerKey{}, pr), req)
}

func peerOf(ctx context.Context) ingestPeer {
	p, _ := ctx.Value(peerKey{}).(ingestPeer)
	return p
}

// nodeOf is the peer's name, for the log.
func nodeOf(ctx context.Context) string {
	if p := peerOf(ctx); p.node != "" {
		return p.node
	}
	return ingressPod + peerOf(ctx).ingress
}

// Export stores a batch and answers once it is committed.
func (in *ingest) Export(ctx context.Context, req *otlplogsv1.ExportLogsServiceRequest) (*otlplogsv1.ExportLogsServiceResponse, error) {
	batch, err := parseExport(req, peerOf(ctx), time.Now())
	if errors.Is(err, errNotYours) {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := in.store.Ingest(ctx, batch); err != nil {
		if errors.Is(err, ErrQuota) {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		in.log.Error("store a batch", "node", nodeOf(ctx), "err", err)
		return nil, status.Error(codes.Unavailable, "the sink cannot store the batch now")
	}
	return &otlplogsv1.ExportLogsServiceResponse{}, nil
}

// Positions answers where the sink is with each stream.
func (in *ingest) Positions(ctx context.Context, req *sinkv1.PositionsRequest) (*sinkv1.PositionsResponse, error) {
	keys, err := streamKeys(req.GetStreams())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	next, err := in.store.Positions(ctx, keys)
	if err != nil {
		in.log.Error("positions", "node", nodeOf(ctx), "err", err)
		return nil, status.Error(codes.Unavailable, "the sink cannot read positions now")
	}
	return &sinkv1.PositionsResponse{NextOffsets: next}, nil
}

// EndStreams closes the streams; the cold stage makes their last chunks.
func (in *ingest) EndStreams(ctx context.Context, req *sinkv1.EndStreamsRequest) (*sinkv1.EndStreamsResponse, error) {
	keys, err := streamKeys(req.GetStreams())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.GetTimeUnixNano() < 0 {
		return nil, status.Error(codes.InvalidArgument, "time_unix_nano must not be negative")
	}
	if err := in.store.EndStreams(ctx, keys, req.GetTimeUnixNano()); err != nil {
		in.log.Error("end streams", "node", nodeOf(ctx), "err", err)
		return nil, status.Error(codes.Unavailable, "the sink cannot end streams now")
	}
	return &sinkv1.EndStreamsResponse{}, nil
}

func streamKeys(ks []*sinkv1.StreamKey) ([]StreamKey, error) {
	if len(ks) > maxKeys {
		return nil, fmt.Errorf("%d streams; at most %d", len(ks), maxKeys)
	}
	out := make([]StreamKey, len(ks))
	for i, k := range ks {
		out[i] = StreamKey{Pod: k.GetPod(), Container: k.GetContainer(), LogID: k.GetLogId()}
		if out[i].Pod == "" || out[i].Container == "" || out[i].LogID == "" {
			return nil, fmt.Errorf("stream %d: pod, container and log_id are required", i)
		}
	}
	return out, nil
}

// errNotYours refuses a stream the peer may not write.
var errNotYours = errors.New("not the peer's stream")

// parseExport reads an Export into the streams' batches: a ResourceLogs a
// stream (two of the same key are joined), its labels from the resource's
// attributes and the peer, the node of the certificate. A record without
// log.offset, a resource without its key or tenant, and more than
// maxExportRecords records are refused. A stream with ferun.source
// ingress is an ingress's whose name its ferun.pod has (ingress/<name>),
// every other a node's, never of an ingress/ pod: one of another peer
// refuses the request with errNotYours. A record's time is its own, else
// when the node read it, else now; its text is made valid UTF-8 without
// NULs, which PostgreSQL's text cannot hold.
func parseExport(req *otlplogsv1.ExportLogsServiceRequest, p ingestPeer, now time.Time) ([]StreamBatch, error) {
	count := 0
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			count += len(sl.GetLogRecords())
		}
	}
	if count > maxExportRecords {
		return nil, fmt.Errorf("%d log records; at most %d a request", count, maxExportRecords)
	}
	byKey := map[StreamKey]int{}
	var out []StreamBatch
	for i, rl := range req.GetResourceLogs() {
		attrs := map[string]string{}
		for _, kv := range rl.GetResource().GetAttributes() {
			attrs[kv.GetKey()] = anyText(kv.GetValue())
		}
		key := StreamKey{Pod: attrs[attrPod], Container: attrs[attrContainer], LogID: attrs[attrLogID]}
		if key.Pod == "" || key.Container == "" || key.LogID == "" {
			return nil, fmt.Errorf("resource %d: %s, %s and %s are required", i, attrPod, attrContainer, attrLogID)
		}
		if attrs[attrTenant] == "" {
			return nil, fmt.Errorf("resource %d: %s is required", i, attrTenant)
		}
		source := SourceContainer
		if attrs[attrSource] == SourceIngress {
			source = SourceIngress
			if p.ingress == "" || key.Pod != ingressPod+p.ingress {
				return nil, fmt.Errorf("resource %d: %w: a stream of %s %s is the ingress's of its %s alone", i, errNotYours,
					attrSource, SourceIngress, attrPod)
			}
		} else if p.node == "" || strings.HasPrefix(key.Pod, ingressPod) {
			return nil, fmt.Errorf("resource %d: %w: a stream without %s %s is a node's, of a pod not %s…", i, errNotYours,
				attrSource, SourceIngress, ingressPod)
		}
		j, ok := byKey[key]
		if !ok {
			j = len(out)
			byKey[key] = j
			out = append(out, StreamBatch{Key: key, Labels: labelsOf(attrs, p.node, source), Next: -1})
		}
		b := &out[j]
		for _, sl := range rl.GetScopeLogs() {
			for _, r := range sl.GetLogRecords() {
				ln, next, err := parseRecord(r, now)
				if err != nil {
					return nil, fmt.Errorf("resource %d: %w", i, err)
				}
				b.Lines = append(b.Lines, ln)
				b.Next = max(b.Next, next)
			}
		}
	}
	return out, nil
}

// deploymentAttr is the attribute that holds a stream's deployment:
// ferun.deployment, else its earlier word.
func deploymentAttr(attrs map[string]string) string {
	if _, ok := attrs[attrDeployment]; ok {
		return attrDeployment
	}
	return attrRelease
}

func labelsOf(attrs map[string]string, node, source string) Labels {
	num := func(k string) int {
		n, _ := strconv.Atoi(attrs[k])
		return max(n, 0)
	}
	return Labels{
		Tenant: attrs[attrTenant], Node: node, Instance: attrs[attrInstance],
		Environment: attrs[attrEnvironment], Kind: attrs[attrKind], Object: attrs[attrObject], Job: attrs[attrJob],
		Replica: num(attrReplica), Deployment: num(deploymentAttr(attrs)), Attempt: num(attrAttempt),
		RetentionDays: num(attrRetentionDays), Source: source, Attrs: attrs,
	}
}

// parseRecord reads one record; next is its log.next_offset, -1 without.
func parseRecord(r *logsv1.LogRecord, now time.Time) (Line, int64, error) {
	ln := Line{Off: -1}
	next := int64(-1)
	for _, kv := range r.GetAttributes() {
		v := kv.GetValue()
		switch kv.GetKey() {
		case attrOffset:
			ln.Off = anyInt(v)
		case attrNextOffset:
			next = anyInt(v)
		case attrIOStream:
			if s := v.GetStringValue(); s == "stdout" || s == "stderr" {
				ln.Source = s
			}
		case attrPartial:
			ln.Partial = v.GetBoolValue()
		}
	}
	if ln.Off < 0 {
		return Line{}, 0, fmt.Errorf("a log record without %s", attrOffset)
	}
	ln.TS = int64(r.GetTimeUnixNano())
	if ln.TS <= 0 {
		ln.TS = int64(r.GetObservedTimeUnixNano())
	}
	if ln.TS <= 0 {
		ln.TS = now.UnixNano()
	}
	ln.Text = cleanText(anyText(r.GetBody()))
	ln.Level, ln.Trace = Extract(ln.Text)
	// The record's own trace ID and severity win (ADR 0026 §10).
	if id := r.GetTraceId(); len(id) == 16 && !allZero(id) {
		ln.Trace = hex.EncodeToString(id)
	}
	if ln.Level == LevelUnknown {
		if lvl := severityLevel(int32(r.GetSeverityNumber())); lvl != 0 {
			ln.Level = lvl
		}
	}
	return ln, next, nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// severityLevel is the level of an OTLP severity number, 0 for none:
// 1 to 8 (TRACE, DEBUG) debug, 9 to 12 info, 13 to 16 warn, 17 to 24
// (ERROR, FATAL) error.
func severityLevel(n int32) int {
	switch {
	case n >= 17 && n <= 24:
		return LevelError
	case n >= 13 && n <= 16:
		return LevelWarn
	case n >= 9 && n <= 12:
		return LevelInfo
	case n >= 1 && n <= 8:
		return LevelDebug
	}
	return 0
}

// cleanText makes s valid UTF-8 without NULs.
func cleanText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return strings.ReplaceAll(s, "\x00", "�")
}

// anyText is a value as text: a string as it is, a number or a bool
// written out, bytes as text, anything else empty.
func anyText(v *commonv1.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return x.StringValue
	case *commonv1.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonv1.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonv1.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonv1.AnyValue_BytesValue:
		return string(x.BytesValue)
	}
	return ""
}

// anyInt is an int value, or a string of one; -1 for anything else.
func anyInt(v *commonv1.AnyValue) int64 {
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_IntValue:
		return x.IntValue
	case *commonv1.AnyValue_StringValue:
		if n, err := strconv.ParseInt(x.StringValue, 10, 64); err == nil {
			return n
		}
	}
	return -1
}
