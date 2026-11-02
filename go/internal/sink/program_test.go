// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	sinkv1 "github.com/gardenlinux/feos/go/gen/feos/sink/v1"
	otlplogsv1 "github.com/gardenlinux/feos/go/gen/otlp/collector/logs/v1"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/ingress/sinklog"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
)

const (
	testTD     = "run.example.com"
	testDomain = "example.com"
)

// fakeFerund is ferund's part of the contract: the service certificate,
// issued with pki, and a token's tenant.
type fakeFerund struct {
	iss   *pki.Issuer
	certs atomic.Int32
}

func newCA(t *testing.T) *pki.Issuer {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "ca")
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: dir, TrustDomain: testTD,
		DNSDomains: []string{testDomain}}); err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(dir, testTD)
	if err != nil {
		t.Fatal(err)
	}
	return iss
}

func pemCerts(ders ...[]byte) string {
	var b strings.Builder
	for _, d := range ders {
		b.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d}))
	}
	return b.String()
}

func (f *fakeFerund) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == pathServiceCertificates:
		if r.Header.Get("Authorization") != "Bearer svc-token" {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		var req certRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b, _ := pem.Decode([]byte(req.CSR))
		if b == nil || b.Type != "CERTIFICATE REQUEST" {
			http.Error(w, "no CSR", http.StatusBadRequest)
			return
		}
		// ferund's own issuance is built apart; a pod's certificate has
		// the shape the contract asks: the one URI, ServerAuth.
		c, leaf, err := f.iss.IssueSink(b.Bytes, "sink-0", 24*time.Hour)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.certs.Add(1)
		writeJSON(w, http.StatusOK, certAnswer{Certificate: pemCerts(c.Chain...), Bundle: pemCerts(c.TrustBundle...), NotAfter: leaf.NotAfter})
	case r.Method == http.MethodGet && r.URL.Path == pathTenant:
		if r.Header.Get("Authorization") != "Bearer tenant-token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid token"}`))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"tenant": "acme"})
	default:
		http.NotFound(w, r)
	}
}

// nodeTLS is a node's client side: its node certificate, and the check of
// the sink's certificate by the bundle and its one URI.
func nodeTLS(t *testing.T, iss *pki.Issuer, name string, pod bool) *tls.Config {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, err := pki.CreateCSR(key, name)
	if err != nil {
		t.Fatal(err)
	}
	var chain [][]byte
	if pod {
		c, _, err := iss.IssuePod(csr, pki.SandboxID(testTD, "acme", podid.ID("abcdefghjkm")), nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		chain = c.Chain
	} else {
		c, _, err := iss.IssueNode(csr, name, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		chain = c.Chain
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{{Certificate: chain, PrivateKey: key}},
		InsecureSkipVerify: true, // verified below, by URI
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			certs := make([]*x509.Certificate, len(raw))
			for i, r := range raw {
				c, err := x509.ParseCertificate(r)
				if err != nil {
					return err
				}
				certs[i] = c
			}
			inter := x509.NewCertPool()
			for _, c := range certs[1:] {
				inter.AddCert(c)
			}
			if _, err := certs[0].Verify(x509.VerifyOptions{Roots: iss.Bundle(), Intermediates: inter}); err != nil {
				return err
			}
			if len(certs[0].URIs) != 1 || certs[0].URIs[0].String() != pki.SinkID(testTD, "sink-0") {
				return errors.New("not the sink")
			}
			return nil
		},
	}
}

func TestEndToEnd(t *testing.T) {
	dbURL := dbtest.NewDatabase(t)
	iss := newCA(t)
	ferund := &fakeFerund{iss: iss}
	fsrv := httptest.NewServer(ferund)
	defer fsrv.Close()
	cfg := Default()
	cfg.Name, cfg.Listen, cfg.IngestListen = "sink-0", "127.0.0.1:0", "127.0.0.1:0"
	cfg.DatabaseURL, cfg.TrustDomain, cfg.FerunURL, cfg.ServiceToken = dbURL, testTD, fsrv.URL, "svc-token"
	idx, lg := newMemIndex(), newMemLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prog, err := New(ctx, cfg, Deps{Index: idx, TraceIndex: newMemTraceIndex(), Log: lg}, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- prog.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	actx, acancel := context.WithTimeout(ctx, 30*time.Second)
	defer acancel()
	addr, err := prog.IngestAddr(actx)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := grpc.NewClient(addr.String(), grpc.WithTransportCredentials(credentials.NewTLS(nodeTLS(t, iss, "node-1", false))))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	logs := otlplogsv1.NewLogsServiceClient(conn)
	sinks := sinkv1.NewSinkServiceClient(conn)
	now := uint64(time.Now().UnixNano())
	req := exportRequest(resourceAttrs("abcdefghjkm", "main", "log1", "acme",
		map[string]string{attrEnvironment: "env1", attrKind: "service", attrObject: "api", attrReplica: "0", attrDeployment: "4", "ferun.node": "liar"}),
		record(0, now-3000, "stdout", "INFO starting"), record(20, now-2000, "stderr", `{"level":"error","trace_id":"`+traceA+`"}`),
		record(70, now-1000, "stdout", "plain"))
	for range 2 {
		if _, err := logs.Export(actx, req); err != nil {
			t.Fatalf("export: %v", err)
		}
	}
	pos, err := sinks.Positions(actx, &sinkv1.PositionsRequest{Streams: []*sinkv1.StreamKey{{Pod: "abcdefghjkm", Container: "main", LogId: "log1"}}})
	if err != nil || len(pos.NextOffsets) != 1 || pos.NextOffsets[0] != 70 {
		t.Fatalf("positions %v, %v", pos, err)
	}

	// A pod's certificate is no node's.
	podConn, err := grpc.NewClient(addr.String(), grpc.WithTransportCredentials(credentials.NewTLS(nodeTLS(t, iss, "x", true))))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = podConn.Close() }()
	if _, err := otlplogsv1.NewLogsServiceClient(podConn).Export(actx, req); err == nil {
		t.Error("a pod exported")
	} else if c := status.Code(err); c != codes.Unavailable && c != codes.PermissionDenied {
		t.Errorf("a pod's export: %v", err)
	}

	api := "http://" + prog.APIAddr().String()
	get := func(path, token string) (*http.Response, []byte) {
		t.Helper()
		r, _ := http.NewRequestWithContext(actx, http.MethodGet, api+path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	if resp, b := get("/api/v1/logs?environment=env1", "wrong"); resp.StatusCode != 401 || string(b) != `{"error":"invalid token"}` {
		t.Errorf("wrong token: %d %s", resp.StatusCode, b)
	}
	if resp, _ := get("/api/v1/logs?environment=env1", ""); resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	if resp, _ := get("/healthz", ""); resp.StatusCode != 200 {
		t.Errorf("healthz %d", resp.StatusCode)
	}
	// The lines are in the topic's tail, then in the index: a read finds
	// them in either.
	var page PageJSON
	var b []byte
	for _, when := range []string{"tail", "index"} {
		if when == "index" {
			idx.sync(lg)
			if idx.count("acme") != 3 {
				t.Fatalf("%d documents", idx.count("acme"))
			}
		}
		var resp *http.Response
		resp, b = get("/api/v1/logs?environment=env1", "tenant-token")
		if resp.StatusCode != 200 {
			t.Fatalf("%s: logs: %d %s", when, resp.StatusCode, b)
		}
		page = PageJSON{}
		if err := json.Unmarshal(b, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Lines) != 3 || page.Lines[1].Level != "error" || page.Lines[1].Source != "stderr" || page.Lines[2].Text != "plain" {
			t.Fatalf("%s: page %s", when, b)
		}
	}
	st := page.Streams[page.Lines[0].Stream]
	if st.Node != "node-1" || st.Environment != "env1" || st.Deployment != 4 || st.Lines != 3 || st.Tenant != "acme" {
		t.Errorf("stream %+v", st)
	}
	if !strings.Contains(string(b), `"ts":"`) || !strings.Contains(string(b), `"offset":"20"`) {
		t.Errorf("ts and offset are not strings: %s", b)
	}
	resp, b := get("/api/v1/logs?environment=env1&trace="+traceA, "tenant-token")
	if resp.StatusCode != 200 || strings.Count(string(b), `"offset"`) != 1 {
		t.Errorf("trace: %d %s", resp.StatusCode, b)
	}
	resp, b = get("/api/v1/log-streams?environment=env1", "tenant-token")
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"container":"main"`) {
		t.Errorf("streams: %d %s", resp.StatusCode, b)
	}
	if resp, _ := get("/api/v1/logs?kind=service", "tenant-token"); resp.StatusCode != 400 {
		t.Errorf("no selector: %d", resp.StatusCode)
	}

	// Follow: the page, then a new line, as NDJSON.
	fctx, fcancel := context.WithCancel(actx)
	defer fcancel()
	fr, _ := http.NewRequestWithContext(fctx, http.MethodGet, api+"/api/v1/logs?environment=env1&kind=service&object=api&follow=1", nil)
	fr.Header.Set("Authorization", "Bearer tenant-token")
	fresp, err := http.DefaultClient.Do(fr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresp.Body.Close() }()
	if ct := fresp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content type %q", ct)
	}
	sc := bufio.NewScanner(fresp.Body)
	var got []followLine
	for len(got) < 4 && sc.Scan() {
		var fl followLine
		if err := json.Unmarshal(sc.Bytes(), &fl); err != nil {
			t.Fatal(err)
		}
		got = append(got, fl)
		if len(got) == 3 {
			if _, err := logs.Export(actx, exportRequest(resourceAttrs("abcdefghjkm", "main", "log1", "acme",
				map[string]string{attrEnvironment: "env1"}), record(90, now, "stdout", "followed"))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(got) != 4 || got[0].Stream == nil || got[1].Stream != nil || got[3].Line.Text != "followed" {
		t.Errorf("follow: %+v", got)
	}
	fcancel()

	if _, err := sinks.EndStreams(actx, &sinkv1.EndStreamsRequest{Streams: []*sinkv1.StreamKey{{Pod: "abcdefghjkm", Container: "main", LogId: "log1"}}}); err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequestWithContext(actx, http.MethodDelete, api+"/api/v1/log-streams?environment=env1", nil)
	r.Header.Set("Authorization", "Bearer tenant-token")
	dresp, err := http.DefaultClient.Do(r)
	if err != nil || dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %v %v", dresp, err)
	}
	_ = dresp.Body.Close()
	resp, b = get("/api/v1/log-streams?environment=env1", "tenant-token")
	if resp.StatusCode != 200 || string(b) != "{\"streams\":[]}\n" {
		t.Errorf("after delete: %s", b)
	}
	if ferund.certs.Load() != 1 {
		t.Errorf("%d certificates", ferund.certs.Load())
	}
}

// ingressIdentity is an ingress's certificate, issued with pki, and the
// bundle, as the ingress agent holds them.
func ingressIdentity(t *testing.T, iss *pki.Issuer, name string) sinklog.Identity {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	csr, err := pki.CreateCSR(key, name)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := iss.IssueIngress(csr, name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert := &tls.Certificate{Certificate: c.Chain, PrivateKey: key}
	return func() (*tls.Certificate, *x509.CertPool, error) { return cert, iss.Bundle(), nil }
}

// The ingress writes its access log with its own certificate as streams
// of the services it served (sink.md, The ingress's access log): found
// by source, level and trace; a node cannot write an ingress's stream,
// nor an ingress a container's, nor another ingress's.
func TestIngressEndToEnd(t *testing.T) {
	dbURL := dbtest.NewDatabase(t)
	iss := newCA(t)
	fsrv := httptest.NewServer(&fakeFerund{iss: iss})
	defer fsrv.Close()
	cfg := Default()
	cfg.Name, cfg.Listen, cfg.IngestListen = "sink-0", "127.0.0.1:0", "127.0.0.1:0"
	cfg.DatabaseURL, cfg.TrustDomain, cfg.FerunURL, cfg.ServiceToken = dbURL, testTD, fsrv.URL, "svc-token"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prog, err := New(ctx, cfg, Deps{Index: newMemIndex(), TraceIndex: newMemTraceIndex(), Log: newMemLog()}, dbtest.Logger(t))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- prog.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()
	actx, acancel := context.WithTimeout(ctx, 30*time.Second)
	defer acancel()
	addr, err := prog.IngestAddr(actx)
	if err != nil {
		t.Fatal(err)
	}

	client, err := sinklog.NewClient(addr.String(), testTD, ingressIdentity(t, iss, "in-0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	t0 := time.Now().Add(-time.Minute).UTC()
	rec := func(at time.Duration, path string, status int, outcome, trace string) wire.AccessLogEntry {
		return wire.AccessLogEntry{Time: t0.Add(at), DurationNs: 1500, Env: "env1", Tenant: "acme", Service: "web",
			Instance: "i1", Host: "app.apps.example.com", Method: "GET", Path: path, Proto: "HTTP/2.0", Status: status,
			Outcome: outcome, Attempts: 1, TraceID: trace, SpanID: "00f067aa0ba902b7"}
	}
	streams := sinklog.NewStreams("in-0", "3f0c2a4e-8d1b-4c55-9a39-0d6f1b7a2c11")
	req, taken, skipped := streams.Batch([]wire.AccessLogEntry{
		rec(0, "/a", 200, "2xx", ""), rec(time.Second, "/b", 503, "5xx", traceA), rec(2*time.Second, "/c", 404, "4xx", ""),
		{Env: "env1", Service: "web"}, // no tenant: skipped
	})
	if taken != 4 || skipped != 1 {
		t.Fatalf("taken %d, skipped %d", taken, skipped)
	}
	// A batch sent again is stored once.
	for range 2 {
		if err := client.Export(actx, req); err != nil {
			t.Fatalf("export: %v", err)
		}
	}

	api := "http://" + prog.APIAddr().String()
	read := func(query string) PageJSON {
		t.Helper()
		r, _ := http.NewRequestWithContext(actx, http.MethodGet, api+"/api/v1/logs?"+query, nil)
		r.Header.Set("Authorization", "Bearer tenant-token")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", query, resp.StatusCode, b)
		}
		var page PageJSON
		if err := json.Unmarshal(b, &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	page := read("environment=env1&source=ingress&trace=" + traceA)
	if len(page.Lines) != 1 || page.Lines[0].Level != "error" || page.Lines[0].Offset != 1 ||
		page.Lines[0].TS != t0.Add(time.Second).UnixNano() {
		t.Fatalf("the trace's lines: %+v", page.Lines)
	}
	var line sinklog.Line
	if err := json.Unmarshal([]byte(page.Lines[0].Text), &line); err != nil || line.Path != "/b" || line.Ingress != "in-0" ||
		line.TraceID != traceA || line.Status != 503 {
		t.Errorf("line %q: %v", page.Lines[0].Text, err)
	}
	st := page.Streams[page.Lines[0].Stream]
	if st.Source != SourceIngress || st.Node != "" || st.Environment != "env1" || st.Kind != "service" ||
		st.Object != "web" || st.Container != "env1/web" || st.Tenant != "acme" || st.Lines != 3 {
		t.Errorf("stream %+v", st)
	}
	if page := read("environment=env1&source=ingress&level=warn,error&object=web"); len(page.Lines) != 2 ||
		page.Lines[0].Level != "error" || page.Lines[1].Level != "warn" {
		t.Errorf("warn and error: %+v", page.Lines)
	}
	if page := read("environment=env1&source=container"); len(page.Lines) != 0 {
		t.Errorf("containers' lines: %+v", page.Lines)
	}
	if page := read("environment=env1"); len(page.Lines) != 3 {
		t.Errorf("both sources: %+v", page.Lines)
	}

	// Writes of a stream of another peer are refused.
	denied := func(what string, creds *tls.Config, req *otlplogsv1.ExportLogsServiceRequest) {
		t.Helper()
		conn, err := grpc.NewClient(addr.String(), grpc.WithTransportCredentials(credentials.NewTLS(creds)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := otlplogsv1.NewLogsServiceClient(conn).Export(actx, req); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: %v", what, err)
		}
	}
	ingressTLS := func(name string) *tls.Config {
		return sinklog.ClientTLS(testTD, ingressIdentity(t, iss, name))
	}
	denied("a node writing an ingress's stream", nodeTLS(t, iss, "node-1", false), req)
	denied("an ingress writing another's stream", ingressTLS("in-1"), req)
	container := exportRequest(resourceAttrs("abcdefghjkm", "main", "log1", "acme",
		map[string]string{attrEnvironment: "env1"}), record(0, uint64(t0.UnixNano()), "stdout", "x"))
	denied("an ingress writing a container's stream", ingressTLS("in-0"), container)
	conn, err := grpc.NewClient(addr.String(), grpc.WithTransportCredentials(credentials.NewTLS(ingressTLS("in-0"))))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := sinkv1.NewSinkServiceClient(conn).Positions(actx, &sinkv1.PositionsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("an ingress's Positions: %v", err)
	}
	if page := read("environment=env1"); len(page.Lines) != 3 {
		t.Errorf("after the refusals: %+v", page.Lines)
	}
}
