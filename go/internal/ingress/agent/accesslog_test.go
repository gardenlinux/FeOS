// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	otlplogsv1 "github.com/gardenlinux/feos/go/gen/otlp/collector/logs/v1"
	otlptracev1 "github.com/gardenlinux/feos/go/gen/otlp/collector/trace/v1"
	"github.com/gardenlinux/feos/go/internal/ingress/sinklog"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/pki"
)

func entry(path string) wire.AccessLogEntry {
	return wire.AccessLogEntry{Time: time.Now(), Env: "e1", Path: path, Status: 200, Outcome: "2xx"}
}

// Without a sink the access log is not kept: a record is dropped and
// counted, nothing waits, and the agent says so once at its start.
func TestAccessLogWithoutSink(t *testing.T) {
	h := newHarness(t)
	ta := h.joined(nil)
	ta.Record(entry("/a"))
	ta.Record(entry("/b"))
	if err := ta.FlushAccessLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sent, dropped, pending := ta.AccessLogCounts(); sent != 0 || dropped != 2 || pending != 0 {
		t.Errorf("counts %d %d %d", sent, dropped, pending)
	}
	ta.logs.mu.Lock()
	n := 0
	for _, l := range ta.logs.lines {
		if strings.Contains(l, "level=INFO") && strings.Contains(l, "no sink endpoint") {
			n++
		}
	}
	ta.logs.mu.Unlock()
	if n != 1 {
		t.Errorf("said %d times that there is no sink", n)
	}
}

// Beyond the buffer a record is dropped; Record never blocks.
func TestAccessLogBufferFull(t *testing.T) {
	h := newHarness(t)
	h.cfg.SinkEndpoint = newFakeSink(t, h.fake.Issuer()).addr
	a := h.start(h.cfg, nil)
	for range accessLogBuffer + 5 {
		a.Record(entry("/"))
	}
	if sent, dropped, pending := a.AccessLogCounts(); sent != 0 || dropped != 5 || pending != accessLogBuffer {
		t.Errorf("counts %d %d %d", sent, dropped, pending)
	}
	if err := a.FlushAccessLog(context.Background()); err != ErrNotConnected {
		t.Errorf("flush without a connection: %v", err)
	}
}

// fakeSink is the sink's ingest: a LogsService and a TraceService over
// TLS with a sink's certificate of the fake control plane's CA, which
// record what they take and answer err while it is set.
type fakeSink struct {
	otlplogsv1.UnimplementedLogsServiceServer
	addr   string
	traces *fakeTraces
	mu     sync.Mutex
	reqs   []*otlplogsv1.ExportLogsServiceRequest
	peer   string
	err    error
}

// fakeTraces is the sink's TraceService: it answers err while it is set,
// else rejects the first reject spans of a request.
type fakeTraces struct {
	otlptracev1.UnimplementedTraceServiceServer
	mu     sync.Mutex
	reqs   []*otlptracev1.ExportTraceServiceRequest
	err    error
	reject int64
}

func (f *fakeTraces) Export(_ context.Context, req *otlptracev1.ExportTraceServiceRequest) (*otlptracev1.ExportTraceServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.reqs = append(f.reqs, req)
	resp := &otlptracev1.ExportTraceServiceResponse{}
	if f.reject > 0 {
		resp.PartialSuccess = &otlptracev1.ExportTracePartialSuccess{RejectedSpans: f.reject, ErrorMessage: "invalid"}
	}
	return resp, nil
}

func (f *fakeTraces) set(err error, reject int64) {
	f.mu.Lock()
	f.err, f.reject = err, reject
	f.mu.Unlock()
}

// spans returns the span IDs the sink took, in order.
func (f *fakeTraces) spans() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, req := range f.reqs {
		for _, rs := range req.GetResourceSpans() {
			for _, sp := range rs.GetScopeSpans()[0].GetSpans() {
				ids = append(ids, hex.EncodeToString(sp.GetSpanId()))
			}
		}
	}
	return ids
}

func newFakeSink(t *testing.T, iss *pki.Issuer) *fakeSink {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, err := pki.CreateCSR(key, "sink-0")
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := iss.IssueSink(csr, "sink-0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: c.Chain, PrivateKey: key}},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    iss.Bundle(),
	})))
	f := &fakeSink{traces: &fakeTraces{}}
	otlplogsv1.RegisterLogsServiceServer(srv, f)
	otlptracev1.RegisterTraceServiceServer(srv, f.traces)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = ln.Addr().String()
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return f
}

func (f *fakeSink) Export(ctx context.Context, req *otlplogsv1.ExportLogsServiceRequest) (*otlplogsv1.ExportLogsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if p, ok := grpcpeer.FromContext(ctx); ok {
		if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 {
			f.peer = ti.State.PeerCertificates[0].URIs[0].String()
		}
	}
	f.reqs = append(f.reqs, req)
	return &otlplogsv1.ExportLogsServiceResponse{}, nil
}

func (f *fakeSink) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// taken returns the records' paths and offsets the sink took, in order,
// and the URI of the last client.
func (f *fakeSink) taken() (paths []string, offsets []int64, peer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.reqs {
		for _, rl := range req.GetResourceLogs() {
			for _, r := range rl.GetScopeLogs()[0].GetLogRecords() {
				var line sinklog.Line
				_ = json.Unmarshal([]byte(r.GetBody().GetStringValue()), &line)
				paths = append(paths, line.Path)
				offsets = append(offsets, r.GetAttributes()[0].GetValue().GetIntValue())
			}
		}
	}
	return paths, offsets, f.peer
}

// With a sink the access log goes there, with the instance's
// certificate: a batch the sink failed to take
// is sent again under the same offsets; one it refuses as invalid is
// dropped and counted.
func TestAccessLogToSink(t *testing.T) {
	h := newHarness(t)
	sink := newFakeSink(t, h.fake.Issuer())
	h.cfg.SinkEndpoint = sink.addr
	ta := h.joined(nil)
	ctx := context.Background()
	rec := func(path string) wire.AccessLogEntry {
		e := entry(path)
		e.Project, e.Service = "acme", "web"
		return e
	}
	ta.Record(rec("/a"))
	ta.Record(rec("/b"))
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	paths, offsets, peer := sink.taken()
	if !slices.Equal(paths, []string{"/a", "/b"}) || !slices.Equal(offsets, []int64{0, 1}) ||
		peer != pki.IngressID(h.fake.TrustDomain(), testName) {
		t.Fatalf("taken %v %v by %s", paths, offsets, peer)
	}

	sink.fail(status.Error(codes.Unavailable, "down"))
	ta.Record(rec("/c"))
	if err := ta.FlushAccessLog(ctx); err == nil {
		t.Fatal("flush succeeded")
	}
	ta.Record(rec("/d"))
	if sent, dropped, pending := ta.AccessLogCounts(); sent != 2 || dropped != 0 || pending != 2 {
		t.Fatalf("while the sink is down: %d %d %d", sent, dropped, pending)
	}
	sink.fail(nil)
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	if paths, offsets, _ := sink.taken(); !slices.Equal(paths, []string{"/a", "/b", "/c", "/d"}) ||
		!slices.Equal(offsets, []int64{0, 1, 2, 3}) {
		t.Fatalf("after the outage %v %v", paths, offsets)
	}

	sink.fail(status.Error(codes.InvalidArgument, "no"))
	ta.Record(rec("/e"))
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	if sent, dropped, pending := ta.AccessLogCounts(); sent != 4 || dropped != 1 || pending != 0 {
		t.Errorf("after a refusal: %d %d %d", sent, dropped, pending)
	}
}

// Each record's server span goes to the sink's TraceService in a batch
// of its own, once: a batch the sink failed to take or refused is
// dropped and counted, spans it rejected are counted, and the access
// log goes on as before.
func TestSpansToSink(t *testing.T) {
	h := newHarness(t)
	sink := newFakeSink(t, h.fake.Issuer())
	h.cfg.SinkEndpoint = sink.addr
	ta := h.joined(nil)
	ctx := context.Background()
	n := 0
	rec := func() wire.AccessLogEntry {
		n++
		e := entry(fmt.Sprintf("/%d", n))
		e.Project, e.Service, e.Method = "acme", "web", "GET"
		e.TraceID, e.SpanID, e.TraceFlags = "4bf92f3577b34da6a3ce929d0e0e4736", fmt.Sprintf("%016x", n), 1
		return e
	}
	ta.Record(rec())
	ta.Record(rec())
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	if ids := sink.traces.spans(); !slices.Equal(ids, []string{"0000000000000001", "0000000000000002"}) {
		t.Fatalf("spans %v", ids)
	}
	if sent, dropped := ta.SpanCounts(); sent != 2 || dropped != 0 {
		t.Fatalf("span counts %d %d", sent, dropped)
	}

	// The sink fails the spans: dropped, the access log goes on.
	sink.traces.set(status.Error(codes.Unavailable, "down"), 0)
	ta.Record(rec())
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	// It refuses them as invalid: dropped.
	sink.traces.set(status.Error(codes.InvalidArgument, "no"), 0)
	ta.Record(rec())
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	// It rejects one of two: that one is counted as dropped.
	sink.traces.set(nil, 1)
	ta.Record(rec())
	ta.Record(rec())
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	if sent, dropped := ta.SpanCounts(); sent != 3 || dropped != 3 {
		t.Errorf("span counts %d %d", sent, dropped)
	}
	if sent, dropped, pending := ta.AccessLogCounts(); sent != 6 || dropped != 0 || pending != 0 {
		t.Errorf("access log counts %d %d %d", sent, dropped, pending)
	}

	// While the access log's batch waits for the sink, so do its spans;
	// they go once after it.
	sink.traces.set(nil, 0)
	sink.fail(status.Error(codes.Unavailable, "down"))
	ta.Record(rec())
	if err := ta.FlushAccessLog(ctx); err == nil {
		t.Fatal("flush succeeded")
	}
	if ids := sink.traces.spans(); len(ids) != 4 {
		t.Fatalf("spans sent before their lines: %v", ids)
	}
	sink.fail(nil)
	if err := ta.FlushAccessLog(ctx); err != nil {
		t.Fatal(err)
	}
	if ids := sink.traces.spans(); len(ids) != 5 || ids[4] != "0000000000000007" {
		t.Errorf("spans %v", ids)
	}
	paths, _, _ := sink.taken()
	if len(paths) != 7 {
		t.Errorf("lines %v", paths)
	}
}
