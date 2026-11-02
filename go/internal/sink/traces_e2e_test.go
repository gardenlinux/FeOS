// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	otlptracev1 "github.com/gardenlinux/feos/go/gen/otlp/collector/trace/v1"
	tracev1 "github.com/gardenlinux/feos/go/gen/otlp/trace/v1"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/ingress/sinklog"
)

// A node and an ingress export spans over the ingest with their
// certificates; a tenant reads them in the shape of Jaeger's API: a trace
// by its ID from the topic's tail at once, and every read from the index
// once it committed.
func TestTracesEndToEnd(t *testing.T) {
	dbURL := dbtest.NewDatabase(t)
	iss := newCA(t)
	fsrv := httptest.NewServer(&fakeFerund{iss: iss})
	defer fsrv.Close()
	cfg := Default()
	cfg.Name, cfg.Listen, cfg.IngestListen = "sink-0", "127.0.0.1:0", "127.0.0.1:0"
	cfg.DatabaseURL, cfg.TrustDomain, cfg.FerunURL, cfg.ServiceToken = dbURL, testTD, fsrv.URL, "svc-token"
	tidx, lg := newMemTraceIndex(), newMemLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prog, err := New(ctx, cfg, Deps{Index: newMemIndex(), TraceIndex: tidx, Log: lg}, dbtest.Logger(t))
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
	export := func(creds *tls.Config, req *otlptracev1.ExportTraceServiceRequest) (*otlptracev1.ExportTraceServiceResponse, error) {
		conn, err := grpc.NewClient(addr.String(), grpc.WithTransportCredentials(credentials.NewTLS(creds)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		return otlptracev1.NewTraceServiceClient(conn).Export(actx, req)
	}
	node := nodeTLS(t, iss, "node-1", false)
	ingress := sinklog.ClientTLS(testTD, ingressIdentity(t, iss, "in-0"))

	start := time.Now().Add(-time.Minute).UnixNano()
	inSpan := testSpan(traceA, "00f067aa0ba902b7", "", "GET", start, 30*time.Millisecond)
	inAttrs := map[string]string{attrTenant: "acme", attrPod: "ingress/in-0", attrSource: SourceIngress,
		attrServiceName: "ferun-ingress", attrEnvironment: "env1", attrKind: "service", attrObject: "web"}
	if resp, err := export(ingress, traceRequest(inAttrs, inSpan)); err != nil || resp.GetPartialSuccess() != nil {
		t.Fatalf("the ingress's export: %v %v", resp, err)
	}
	appSpan := testSpan(traceA, "1111111111111111", "00f067aa0ba902b7", "handler", start+int64(time.Millisecond), 20*time.Millisecond)
	bad := &tracev1.Span{TraceId: make([]byte, 16), SpanId: []byte{1, 2, 3, 4, 5, 6, 7, 8}}
	resp, err := export(node, traceRequest(podAttrs("acme", "env1", "web"), appSpan, bad))
	if err != nil || resp.GetPartialSuccess().GetRejectedSpans() != 1 || resp.GetPartialSuccess().GetErrorMessage() == "" {
		t.Fatalf("the node's export: %v %v", resp, err)
	}
	for _, c := range []struct {
		what  string
		creds *tls.Config
		attrs map[string]string
	}{
		{"a node writing the ingress's", node, inAttrs},
		{"an ingress writing a pod's", ingress, podAttrs("acme", "env1", "web")},
		{"another ingress writing in-0's", sinklog.ClientTLS(testTD, ingressIdentity(t, iss, "in-1")), inAttrs},
	} {
		if _, err := export(c.creds, traceRequest(c.attrs, appSpan)); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: %v", c.what, err)
		}
	}
	tooMany := make([]*tracev1.Span, maxExportSpans+1)
	for i := range tooMany {
		tooMany[i] = appSpan
	}
	if _, err := export(node, traceRequest(podAttrs("acme", "env1", "web"), tooMany...)); status.Code(err) != codes.InvalidArgument {
		t.Errorf("too many spans: %v", err)
	}

	api := "http://" + prog.APIAddr().String() + "/api/v1/jaeger/api"
	get := func(path string, token string) (int, []byte) {
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
		return resp.StatusCode, b
	}
	type answer struct {
		Data []struct {
			TraceID string `json:"traceID"`
			Spans   []struct {
				SpanID    string `json:"spanID"`
				ProcessID string `json:"processID"`
			} `json:"spans"`
			Processes map[string]struct {
				ServiceName string `json:"serviceName"`
			} `json:"processes"`
		} `json:"data"`
		Errors []jaegerError `json:"errors"`
	}
	read := func(path string, want int) answer {
		t.Helper()
		code, b := get(path, "tenant-token")
		var a answer
		if err := json.Unmarshal(b, &a); code != want || err != nil {
			t.Fatalf("%s: %d %s", path, code, b)
		}
		return a
	}
	names := func(path string) string {
		t.Helper()
		code, b := get(path, "tenant-token")
		var a struct {
			Data  []string `json:"data"`
			Total int      `json:"total"`
		}
		if err := json.Unmarshal(b, &a); code != http.StatusOK || err != nil || a.Total != len(a.Data) {
			t.Fatalf("%s: %d %s", path, code, b)
		}
		return url.Values{"x": a.Data}.Encode()
	}
	byID := func(when string) {
		t.Helper()
		a := read("/traces/"+traceA, http.StatusOK)
		if len(a.Data) != 1 || a.Data[0].TraceID != traceA || len(a.Data[0].Spans) != 2 || len(a.Data[0].Processes) != 2 ||
			a.Data[0].Spans[0].SpanID != "00f067aa0ba902b7" || a.Data[0].Processes[a.Data[0].Spans[0].ProcessID].ServiceName != "ferun-ingress" {
			t.Errorf("%s: the trace: %+v", when, a)
		}
		if a := read("/traces/"+traceA+"?source=ingress", http.StatusOK); len(a.Data[0].Spans) != 1 {
			t.Errorf("%s: the ingress's spans: %+v", when, a)
		}
		if a := read("/traces/"+traceA+"?environment=env2", http.StatusNotFound); a.Data != nil || len(a.Errors) != 1 ||
			a.Errors[0].Code != 404 || a.Errors[0].Msg != "trace not found" {
			t.Errorf("%s: another environment's: %+v", when, a)
		}
	}
	check := func(when string) {
		t.Helper()
		byID(when)
		if a := read("/traces?service=web&environment=env1", http.StatusOK); len(a.Data) != 1 || len(a.Data[0].Spans) != 2 {
			t.Errorf("%s: search: %+v", when, a)
		}
		if a := read("/traces?service=web&minDuration=25ms", http.StatusOK); len(a.Data) != 0 {
			t.Errorf("%s: search by duration: %+v", when, a)
		}
		if a := read(`/traces?tags=`+url.QueryEscape(`{"ferun.object":"web"}`), http.StatusOK); len(a.Data) != 1 {
			t.Errorf("%s: search by tag: %+v", when, a)
		}
		if got := names("/services"); got != "x=ferun-ingress&x=web" {
			t.Errorf("%s: services %s", when, got)
		}
		if got := names("/services/web/operations"); got != "x=handler" {
			t.Errorf("%s: operations %s", when, got)
		}
	}
	byID("tail")
	if a := read("/traces?service=web&environment=env1", http.StatusOK); len(a.Data) != 0 {
		t.Errorf("tail: a search found %+v", a)
	}
	tidx.sync(lg)
	if tidx.count() != 2 {
		t.Fatalf("%d spans indexed", tidx.count())
	}
	check("index")

	// The ingress's traffic of the environment's services: the ingress's
	// span of web, none of api; the pod's own span is not traffic.
	step := int64(time.Minute)
	until := (time.Now().UnixNano()/step + 1) * step
	traffic := "http://" + prog.APIAddr().String() + "/api/v1/traffic?" + url.Values{"environment": {"env1"},
		"service": {"web", "api"}, "since": {fmt.Sprint(until - int64(time.Hour))}, "until": {fmt.Sprint(until)},
		"step": {"60s"}}.Encode()
	r, _ := http.NewRequestWithContext(actx, http.MethodGet, traffic, nil)
	r.Header.Set("Authorization", "Bearer tenant-token")
	tresp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var tr struct {
		Services []ServiceTraffic `json:"services"`
	}
	err = json.NewDecoder(tresp.Body).Decode(&tr)
	_ = tresp.Body.Close()
	if err != nil || tresp.StatusCode != http.StatusOK || len(tr.Services) != 2 || tr.Services[0].Service != "web" ||
		tr.Services[0].Requests != 1 || tr.Services[0].Errors != 0 || len(tr.Services[0].Steps) != 1 ||
		tr.Services[1].Service != "api" || tr.Services[1].Requests != 0 {
		t.Errorf("traffic: %d %+v %v", tresp.StatusCode, tr, err)
	}
	r, _ = http.NewRequestWithContext(actx, http.MethodGet, strings.Replace(traffic, "step=60s", "step=1ms", 1), nil)
	r.Header.Set("Authorization", "Bearer tenant-token")
	if tresp, err := http.DefaultClient.Do(r); err != nil || tresp.StatusCode != http.StatusBadRequest {
		t.Errorf("traffic of a bad step: %v %v", tresp, err)
	} else {
		_ = tresp.Body.Close()
	}

	if a := read("/traces/zz", http.StatusBadRequest); len(a.Errors) != 1 || a.Errors[0].Code != 400 {
		t.Errorf("a bad ID: %+v", a)
	}
	if code, _ := get("/services", ""); code != http.StatusUnauthorized {
		t.Errorf("without a token: %d", code)
	}
	if code, _ := get("/services", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("a wrong token: %d", code)
	}
}
