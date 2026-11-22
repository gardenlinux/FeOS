// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	otlplogsv1 "github.com/gardenlinux/feos/go/gen/otlp/collector/logs/v1"
	commonv1 "github.com/gardenlinux/feos/go/gen/otlp/common/v1"
	logsv1 "github.com/gardenlinux/feos/go/gen/otlp/logs/v1"
	resourcev1 "github.com/gardenlinux/feos/go/gen/otlp/resource/v1"

	"github.com/gardenlinux/feos/go/internal/auth"
)

const traceA = "4bf92f3577b34da6a3ce929d0e0e4736"

func TestExtract(t *testing.T) {
	for _, tc := range []struct {
		text  string
		level int
		trace string
	}{
		{"ERROR something broke", LevelError, ""},
		{"error: lower case", LevelError, ""},
		{"[WARN] careful", LevelWarn, ""},
		{"Warning: careful", LevelWarn, ""},
		{"level=info msg=hello", LevelInfo, ""},
		{`level="debug" msg=x`, LevelDebug, ""},
		{"time=2026-10-30T12:00:00Z level=WARN msg=slog", LevelWarn, ""},
		{"TRACE deep", LevelDebug, ""},
		{"FATAL gone", LevelError, ""},
		{"panic: runtime error", LevelError, ""},
		{"information is not a level", LevelUnknown, ""},
		{"errors happen", LevelUnknown, ""},
		{"GET /index 200", LevelUnknown, ""},
		{"", LevelUnknown, ""},
		{`{"level":"error","msg":"x","trace_id":"` + traceA + `"}`, LevelError, traceA},
		{`{"lvl":"warning","traceId":"` + traceA + `"}`, LevelWarn, traceA},
		{`{"severity":"CRITICAL","traceparent":"00-` + traceA + `-00f067aa0ba902b7-01"}`, LevelError, traceA},
		{`{"level":30,"msg":"pino"}`, LevelInfo, ""},
		{`{"level":50}`, LevelError, ""},
		{`{"msg":"no level","trace_id":"00000000000000000000000000000000"}`, LevelUnknown, ""},
		{`{"level":"info","trace_id":"4BF92F3577B34DA6A3CE929D0E0E4736"}`, LevelInfo, ""},
		{`{"level":"info"`, LevelUnknown, ""},
	} {
		level, trace := Extract(tc.text)
		if level != tc.level || trace != tc.trace {
			t.Errorf("Extract(%q) = %d, %q; want %d, %q", tc.text, level, trace, tc.level, tc.trace)
		}
	}
}

func TestParseSelectorSource(t *testing.T) {
	for src, ok := range map[string]bool{"": true, "ingress": true, "egress": true, "container": true, "node": false} {
		s, err := parseSelector(url.Values{"environment": {"e1"}, "source": {src}})
		if (err == nil) != ok || (ok && s.Source != src) {
			t.Errorf("source %q: %+v, %v", src, s, err)
		}
	}
}

func TestParseQuery(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	q, err := parseQuery(url.Values{"environment": {"e1"}, "q": {"Foo -bar  baz"}, "level": {"error,unknown"},
		"limit": {"9999"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if q.Until != now.UnixNano() || q.Since != now.Add(-time.Hour).UnixNano() || q.SinceSet || q.Forward {
		t.Errorf("times %d..%d set %v", q.Since, q.Until, q.SinceSet)
	}
	if fmt.Sprint(q.Words) != "[foo baz]" || fmt.Sprint(q.NotWords) != "[bar]" || q.Levels != LevelError|LevelUnknown || q.Limit != maxLimit {
		t.Errorf("filters %v %v %d %d", q.Words, q.NotWords, q.Levels, q.Limit)
	}
	if !q.match(&Line{Text: "FOO and baz", Level: LevelError}) || q.match(&Line{Text: "foo baz bar", Level: LevelError}) ||
		q.match(&Line{Text: "foo baz", Level: LevelInfo}) {
		t.Error("word filters")
	}
	q, err = parseQuery(url.Values{"instance": {"i"}, "re": {`^a+b\d`}}, now)
	if err != nil || !q.match(&Line{Text: "aab1", Level: LevelInfo}) || q.match(&Line{Text: "xab1", Level: LevelInfo}) {
		t.Errorf("re filter: %v", err)
	}
	q, err = parseQuery(url.Values{"job": {"j"}, "direction": {"forward"}, "since": {"2026-10-30T11:00:00Z"}, "until": {"1793361600000000000"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Forward || q.Since != now.Add(-time.Hour).UnixNano() || q.Until != 1793361600000000000 {
		t.Errorf("forward %v %d %d", q.Forward, q.Since, q.Until)
	}
	q, err = parseQuery(url.Values{"stream": {"s_1"}, "trace": {traceA}}, now)
	if err != nil || q.Since != 0 || !q.SinceSet {
		t.Errorf("trace widens since: %d %v %v", q.Since, q.SinceSet, err)
	}
	for _, bad := range []url.Values{
		{},
		{"kind": {"service"}},
		{"environment": {"e"}, "direction": {"up"}},
		{"environment": {"e"}, "level": {"loud"}},
		{"environment": {"e"}, "re": {"("}},
		{"environment": {"e"}, "trace": {"xyz"}},
		{"environment": {"e"}, "limit": {"0"}},
		{"environment": {"e"}, "since": {"yesterday"}},
		{"environment": {"e"}, "replica": {"one"}},
		{"environment": {"e"}, "cursor": {"!!"}},
		{"environment": {"e"}, "cursor": {encodeCursor(true, pos{1, 2, 3})}},
		{"environment": {"e"}, "follow": {"1"}, "cursor": {encodeCursor(false, pos{1, 2, 3})}},
		{"environment": {"e"}, "since": {"2"}, "until": {"1"}},
	} {
		if _, err := parseQuery(bad, now); err == nil {
			t.Errorf("parseQuery(%v) succeeded", bad)
		}
	}
}

func TestCursor(t *testing.T) {
	p := pos{TS: 1761825600123456789, Stream: 42, Off: 4096}
	for _, fwd := range []bool{false, true} {
		got, err := decodeCursor(encodeCursor(fwd, p), fwd)
		if err != nil || got != p {
			t.Errorf("round trip %v: %+v, %v", fwd, got, err)
		}
		if _, err := decodeCursor(encodeCursor(fwd, p), !fwd); err == nil {
			t.Errorf("cursor of the other direction accepted")
		}
	}
	q := Query{}
	if !q.before(pos{2, 0, 0}, pos{1, 0, 0}) || q.before(pos{1, 1, 1}, pos{1, 1, 2}) {
		t.Error("backward order")
	}
	q.Forward = true
	if !q.before(pos{1, 1, 1}, pos{1, 1, 2}) || !q.before(pos{1, 1, 9}, pos{1, 2, 0}) {
		t.Error("forward order")
	}
}

func TestProjects(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != pathProject || r.URL.Query().Get("filter") != "environments" {
			http.NotFound(w, r)
			return
		}
		session := ""
		// A ferund of the release before reads the cookie's old name,
		// which the sink passes on beside the new one.
		if c, err := r.Cookie(auth.OldSessionCookie); err == nil {
			session = c.Value + " " + r.Header.Get("FeRun-Project")
		}
		switch r.Header.Get("Authorization") + session {
		case "Bearer good":
			// A ferund of before people answers the project alone.
			_, _ = w.Write([]byte(`{"project":"acme"}`))
		case "Bearer reader":
			_, _ = w.Write([]byte(`{"project":"acme","role":"read","environments":[]}`))
		case "Bearer stage":
			_, _ = w.Write([]byte(`{"project":"acme","role":"","environments":["e1",""]}`))
		case "Bearer made":
			_, _ = w.Write([]byte(`{"project":"acme","createdAt":"2026-11-22T09:30:00.123456Z","role":"read","environments":[]}`))
		case "Bearer made-e1":
			_, _ = w.Write([]byte(`{"project":"acme","createdAt":"2026-11-22T09:30:00.123456Z","role":"","environments":["e1"]}`))
		case "s1 acme":
			_, _ = w.Write([]byte(`{"project":"acme","role":"write","environments":["e1"]}`))
		case "s1 other":
			http.Error(w, `{"error":"no such project"}`, http.StatusNotFound)
		case "Bearer a.b.c":
			// An integration: the person it names narrows the answer.
			switch person, sent := r.Header["Ferun-Person"]; {
			case !sent && r.Header.Get("FeRun-Project") == "acme":
				_, _ = w.Write([]byte(`{"project":"acme","role":"write","environments":[]}`))
			case sent && person[0] == "ada":
				_, _ = w.Write([]byte(`{"project":"acme","role":"","environments":["e2"]}`))
			default:
				http.Error(w, `{"error":"no such project"}`, http.StatusNotFound)
			}
		case "Bearer broken":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unknown token"}`))
		}
	}))
	defer srv.Close()
	ts := NewProjects(srv.URL, nil, time.Minute)
	made := time.Date(2026, 11, 22, 9, 30, 0, 123456000, time.UTC)
	now := time.Now()
	ts.now = func() time.Time { return now }
	ctx := context.Background()
	good := Credential{Authorization: "Bearer good"}
	for range 3 {
		if got, err := ts.Resolve(ctx, good); err != nil || !reflect.DeepEqual(got, Access{Project: "acme", All: true, Write: true}) {
			t.Fatalf("Resolve = %+v, %v", got, err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("%d calls; the answer was not kept", calls.Load())
	}
	now = now.Add(61 * time.Second)
	if _, err := ts.Resolve(ctx, good); err != nil || calls.Load() != 2 {
		t.Errorf("after the TTL: %v, %d calls", err, calls.Load())
	}
	for _, c := range []struct {
		c    Credential
		want Access
	}{
		{Credential{Authorization: "Bearer reader"}, Access{Project: "acme", All: true}},
		{Credential{Authorization: "Bearer stage"}, Access{Project: "acme", Envs: []string{"e1"}}},
		{Credential{Session: "s1", Project: "acme"}, Access{Project: "acme", All: true, Write: true}},
		// The project's making bounds what is read (ADR 0056); a ferund
		// of before answers none, which bounds nothing.
		{Credential{Authorization: "Bearer made"}, Access{Project: "acme", All: true, Since: made}},
		{Credential{Authorization: "Bearer made-e1"}, Access{Project: "acme", Envs: []string{"e1"}, Since: made}},
	} {
		if got, err := ts.Resolve(ctx, c.c); err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%+v: %+v, %v", c.c, got, err)
		}
	}
	if got := (Access{Since: made}).from(); got != made.UnixNano() {
		t.Errorf("from %d", got)
	}
	if got := (Access{}).from(); got != 0 {
		t.Errorf("no bound: from %d", got)
	}
	// Fresh asks ferund even with an answer kept, and keeps its own.
	n0 := calls.Load()
	if _, err := ts.Fresh(ctx, Credential{Authorization: "Bearer made"}); err != nil || calls.Load() != n0+1 {
		t.Errorf("fresh: %v, %d calls", err, calls.Load()-n0)
	}
	if _, err := ts.Resolve(ctx, Credential{Authorization: "Bearer made"}); err != nil || calls.Load() != n0+1 {
		t.Errorf("after fresh: %v, %d calls", err, calls.Load()-n0)
	}
	// A session's answer is kept for sessionTTL, per project.
	n := calls.Load()
	var ref *refusal
	if _, err := ts.Resolve(ctx, Credential{Session: "s1", Project: "other"}); !errors.As(err, &ref) || ref.status != 404 {
		t.Errorf("another project: %v", err)
	}
	if _, err := ts.Resolve(ctx, Credential{Session: "s1", Project: "acme"}); err != nil || calls.Load() != n+1 {
		t.Errorf("kept: %v, %d calls", err, calls.Load()-n)
	}
	now = now.Add(sessionTTL)
	if _, err := ts.Resolve(ctx, Credential{Session: "s1", Project: "acme"}); err != nil || calls.Load() != n+2 {
		t.Errorf("after sessionTTL: %v, %d calls", err, calls.Load()-n)
	}
	_, err := ts.Resolve(ctx, Credential{Authorization: "Bearer bad"})
	if !errors.As(err, &ref) || ref.status != 401 || string(ref.body) != `{"error":"unknown token"}` {
		t.Errorf("refusal: %v", err)
	}
	n = calls.Load()
	if _, err := ts.Resolve(ctx, Credential{Authorization: "Bearer bad"}); err == nil || calls.Load() != n+1 {
		t.Errorf("a refusal was kept: %v, %d calls", err, calls.Load())
	}
	if _, err := ts.Resolve(ctx, Credential{Authorization: "Bearer broken"}); err == nil || errors.As(err, &ref) {
		t.Errorf("a failure: %v", err)
	}
	// An integration's bearer with the project and the person it names
	// (ADR 0056): the person goes to ferund and is part of the key.
	in := Credential{Authorization: "Bearer a.b.c", Project: "acme"}
	ada := Credential{Authorization: "Bearer a.b.c", Project: "acme", Person: "ada", PersonSent: true}
	for range 2 {
		if got, err := ts.Resolve(ctx, in); err != nil || !reflect.DeepEqual(got, Access{Project: "acme", All: true, Write: true}) {
			t.Fatalf("integration: %+v %v", got, err)
		}
		if got, err := ts.Resolve(ctx, ada); err != nil || !reflect.DeepEqual(got, Access{Project: "acme", Envs: []string{"e2"}}) {
			t.Fatalf("naming ada: %+v %v", got, err)
		}
	}
	if _, err := ts.Resolve(ctx, Credential{Authorization: "Bearer a.b.c", Project: "acme", PersonSent: true}); !errors.As(err, &ref) ||
		ref.status != 404 {
		t.Fatalf("naming nobody: %v", err)
	}
	// An integration's answers, with a person or without, are kept for
	// sessionTTL, as a session's: a person loses a grant more often than a
	// token is revoked.
	n = calls.Load()
	now = now.Add(sessionTTL - time.Second)
	for _, c := range []Credential{in, ada} {
		if _, err := ts.Resolve(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != n {
		t.Errorf("an integration's answer was not kept: %d calls", calls.Load()-n)
	}
	now = now.Add(time.Second)
	for _, c := range []Credential{in, ada} {
		if _, err := ts.Resolve(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != n+2 {
		t.Errorf("after sessionTTL: %d calls, want 2", calls.Load()-n)
	}
	r := httptest.NewRequest("GET", "/api/v1/logs", nil)
	r.Header.Set("Authorization", "Bearer a.b.c")
	r.Header.Set(auth.ProjectHeader, "acme")
	r.Header.Set(auth.PersonHeader, "ada")
	if c, err := credential(r); err != nil || c != ada {
		t.Fatalf("credential %+v %v", c, err)
	}
	r.Header.Add(auth.PersonHeader, "bob")
	if _, err := credential(r); err == nil {
		t.Fatal("two persons named")
	}
}

func TestParseExport(t *testing.T) {
	now := time.Unix(1000, 0)
	req := exportRequest(resourceAttrs("p1", "main", "L1", "acme", map[string]string{"ferun.replica": "2", "ferun.retention_days": "90"}),
		record(0, 5, "stdout", "INFO hello"), record(12, 0, "stderr", "x\x00y\xff"))
	req.ResourceLogs = append(req.ResourceLogs, exportRequest(resourceAttrs("p1", "main", "L1", "acme", nil),
		record(30, 7, "", "third")).ResourceLogs...)
	got, err := parseExport(req, ingestPeer{node: "node-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Lines) != 3 {
		t.Fatalf("%d streams", len(got))
	}
	b := got[0]
	if b.Labels.Node != "node-1" || b.Labels.Replica != 2 || b.Labels.RetentionDays != 90 || b.Labels.Project != "acme" ||
		b.Labels.Source != SourceContainer {
		t.Errorf("labels %+v", b.Labels)
	}
	if b.Lines[0].Level != LevelInfo || b.Lines[0].TS != 5 || b.Lines[1].TS != now.UnixNano() || b.Lines[1].Text != "x�y�" {
		t.Errorf("lines %+v", b.Lines)
	}
	if b.Next != -1 {
		t.Errorf("next %d", b.Next)
	}
	bad := exportRequest(resourceAttrs("p1", "main", "L1", "", nil), record(0, 1, "", "x"))
	if _, err := parseExport(bad, ingestPeer{node: "n"}, now); err == nil {
		t.Error("a resource without a project passed")
	}
	noOff := exportRequest(resourceAttrs("p1", "main", "L1", "acme", nil), &logsv1.LogRecord{Body: str("x")})
	if _, err := parseExport(noOff, ingestPeer{node: "n"}, now); err == nil {
		t.Error("a record without an offset passed")
	}

	// An ingress's stream is the ingress's of its name alone; every
	// other stream a node's, never of an ingress/ pod.
	ing := exportRequest(resourceAttrs("ingress/in-0", "e1/web", "L1", "acme", map[string]string{attrSource: SourceIngress}),
		record(0, 1, "", `{"level":"warn"}`))
	got, err = parseExport(ing, ingestPeer{ingress: "in-0"}, now)
	if err != nil || len(got) != 1 || got[0].Labels.Source != SourceIngress || got[0].Labels.Node != "" ||
		got[0].Lines[0].Level != LevelWarn {
		t.Fatalf("an ingress's stream: %+v, %v", got, err)
	}
	// A pod's egress records (ADR 0028) are its node's, in a stream of
	// their own.
	egress := exportRequest(resourceAttrs("p1", "feos-authn", "E1", "acme", map[string]string{attrSource: SourceEgress,
		"ferun.environment": "e1", "ferun.kind": "service", "ferun.object": "web"}),
		record(0, 1, "", `{"ts":"2026-11-05T01:00:00Z","type":"http","binding":"erp","status":503,"level":"error"}`))
	got, err = parseExport(egress, ingestPeer{node: "node-1"}, now)
	if err != nil || len(got) != 1 || got[0].Labels.Source != SourceEgress || got[0].Labels.Node != "node-1" ||
		got[0].Labels.Object != "web" || got[0].Lines[0].Level != LevelError {
		t.Fatalf("a pod's egress stream: %+v, %v", got, err)
	}
	if _, err := parseExport(egress, ingestPeer{ingress: "in-0"}, now); !errors.Is(err, errNotYours) {
		t.Errorf("an ingress wrote a pod's egress: %v", err)
	}
	asNode := exportRequest(resourceAttrs("ingress/in-0", "e1/web", "L1", "acme", nil), record(0, 1, "", "x"))
	for _, c := range []struct {
		req *otlplogsv1.ExportLogsServiceRequest
		p   ingestPeer
	}{
		{ing, ingestPeer{node: "n"}}, {ing, ingestPeer{ingress: "in-1"}}, {req, ingestPeer{ingress: "in-0"}},
		{asNode, ingestPeer{node: "n"}}, {asNode, ingestPeer{ingress: "in-0"}},
	} {
		if _, err := parseExport(c.req, c.p, now); !errors.Is(err, errNotYours) {
			t.Errorf("%+v wrote %v: %v", c.p, c.req.ResourceLogs[0].Resource.Attributes, err)
		}
	}
}

func TestConfig(t *testing.T) {
	env := map[string]string{
		"FERUN_SINK_NAME": "sink-0", "FERUN_SINK_DATABASE_URL": "postgres://x/sink", "FERUN_SINK_TRUST_DOMAIN": "run.example.com",
		"FERUN_SINK_FERUN_URL": "http://ferund:8100",
		"FERUN_SINK_LOG_QUOTA": "1GiB", "FERUN_SINK_KAFKA_BROKERS": "kafka-0:9092, kafka-1:9092,",
		"FERUN_SINK_INDEX_SEARCH_URL": "http://quickwit-searcher:7280",
		EnvServiceToken:               "tok",
	}
	c, err := Parse([]string{"--log-retention", "24h"}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "sink-0" || c.Retention != 24*time.Hour || c.Quota != 1<<30 || c.ServiceToken != "tok" {
		t.Errorf("config %+v", c)
	}
	if c.IndexPrefix != "ferun-logs" || c.CommitTimeout != 10*time.Second || c.IndexPartitions != 16 {
		t.Errorf("index %q, commit %v, %d partitions", c.IndexPrefix, c.CommitTimeout, c.IndexPartitions)
	}
	if fmt.Sprint(c.KafkaBrokers) != "[kafka-0:9092 kafka-1:9092]" || c.KafkaPartitions != 64 || c.KafkaReplication != 3 ||
		c.KafkaRetention != 24*time.Hour {
		t.Errorf("kafka %v, %d partitions, %d replicas, retention %v", c.KafkaBrokers, c.KafkaPartitions, c.KafkaReplication,
			c.KafkaRetention)
	}
	if c, err := Parse([]string{"--kafka-brokers", "a:1", "--kafka-partitions", "3", "--kafka-replication", "1", "--kafka-retention", "2h"},
		func(k string) string { return env[k] }); err != nil || fmt.Sprint(c.KafkaBrokers) != "[a:1]" || c.KafkaPartitions != 3 ||
		c.KafkaReplication != 1 || c.KafkaRetention != 2*time.Hour {
		t.Errorf("kafka flags: %+v, %v", c, err)
	}
	for _, args := range [][]string{{"--kafka-brokers", " , "}, {"--kafka-partitions", "0"}, {"--kafka-replication", "0"},
		{"--kafka-retention", "30s"}} {
		if _, err := Parse(args, func(k string) string { return env[k] }); err == nil {
			t.Errorf("%v passed", args)
		}
	}
	if c.KafkaSASL != KafkaSASLNone {
		t.Errorf("kafka sasl %q, want none", c.KafkaSASL)
	}
	env["FERUN_SINK_KAFKA_SASL"] = "aws-msk-iam"
	if c, err := Parse(nil, func(k string) string { return env[k] }); err != nil || c.KafkaSASL != KafkaSASLAWSMSKIAM {
		t.Errorf("FERUN_SINK_KAFKA_SASL=aws-msk-iam: %q, %v", c.KafkaSASL, err)
	}
	delete(env, "FERUN_SINK_KAFKA_SASL")
	if _, err := Parse([]string{"--kafka-sasl", "scram"}, func(k string) string { return env[k] }); err == nil {
		t.Error("--kafka-sasl scram passed")
	}
	if c, err := Parse(nil, func(k string) string { return env[k] }); err != nil || fmt.Sprint(c.SourceBrokers()) != fmt.Sprint(c.KafkaBrokers) ||
		c.QuickwitKafkaUsername != "" || c.QuickwitKafkaPassword != "" {
		t.Errorf("Quickwit's sources by default: %v, %q, %v", c.SourceBrokers(), c.QuickwitKafkaUsername, err)
	}
	pwFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(pwFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env["FERUN_SINK_QUICKWIT_KAFKA_BROKERS"] = "b-1:9096,b-2:9096"
	env["FERUN_SINK_QUICKWIT_KAFKA_USERNAME"] = "quickwit"
	env["FERUN_SINK_QUICKWIT_KAFKA_PASSWORD_FILE"] = pwFile
	if c, err := Parse(nil, func(k string) string { return env[k] }); err != nil || fmt.Sprint(c.SourceBrokers()) != "[b-1:9096 b-2:9096]" ||
		c.QuickwitKafkaUsername != "quickwit" || c.QuickwitKafkaPassword != "s3cret" {
		t.Errorf("Quickwit's sources by SCRAM: %v, %q, %v", c.SourceBrokers(), c.QuickwitKafkaUsername, err)
	}
	if err := os.WriteFile(pwFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(nil, func(k string) string { return env[k] }); err == nil {
		t.Error("an empty password passed")
	}
	if err := os.WriteFile(pwFile, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--quickwit-kafka-password-file", filepath.Join(t.TempDir(), "none")},
		{"--quickwit-kafka-username", ""}} {
		if _, err := Parse(args, func(k string) string { return env[k] }); err == nil {
			t.Errorf("%v passed", args)
		}
	}
	delete(env, "FERUN_SINK_QUICKWIT_KAFKA_BROKERS")
	delete(env, "FERUN_SINK_QUICKWIT_KAFKA_USERNAME")
	if _, err := Parse(nil, func(k string) string { return env[k] }); err == nil {
		t.Error("a password file without a user passed")
	}
	delete(env, "FERUN_SINK_QUICKWIT_KAFKA_PASSWORD_FILE")
	if a := kafkaAuth(KafkaSASLAWSMSKIAM); a.AWS == nil {
		t.Error("aws-msk-iam signs in with no credentials")
	}
	if a := kafkaAuth(KafkaSASLNone); a.AWS != nil {
		t.Error("none signs in with AWS credentials")
	}
	for _, p := range []string{"0", "201"} {
		if _, err := Parse([]string{"--index-partitions", p}, func(k string) string { return env[k] }); err == nil {
			t.Errorf("--index-partitions %s passed", p)
		}
	}
	if c, err := Parse([]string{"--index-partitions", "200"}, func(k string) string { return env[k] }); err != nil || c.IndexPartitions != 200 {
		t.Errorf("--index-partitions 200: %d, %v", c.IndexPartitions, err)
	}
	delete(env, "FERUN_SINK_INDEX_SEARCH_URL")
	if _, err := Parse(nil, func(k string) string { return env[k] }); err == nil {
		t.Error("no index passed")
	}
	env["FERUN_SINK_INDEX_SEARCH_URL"] = "http://quickwit-searcher:7280"
	delete(env, EnvServiceToken)
	if _, err := Parse(nil, func(k string) string { return env[k] }); err == nil {
		t.Error("no service token passed")
	}
}

func gunzip(t *testing.T, data []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func str(s string) *commonv1.AnyValue {
	return &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: s}}
}

func kv(k string, v *commonv1.AnyValue) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: v}
}

func resourceAttrs(pod, container, logID, project string, more map[string]string) []*commonv1.KeyValue {
	attrs := []*commonv1.KeyValue{kv(attrPod, str(pod)), kv(attrContainer, str(container)), kv(attrLogID, str(logID))}
	if project != "" {
		attrs = append(attrs, kv(attrProject, str(project)))
	}
	for k, v := range more {
		attrs = append(attrs, kv(k, str(v)))
	}
	return attrs
}

func record(off int64, ts uint64, source, text string) *logsv1.LogRecord {
	attrs := []*commonv1.KeyValue{kv(attrOffset, &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: off}})}
	if source != "" {
		attrs = append(attrs, kv(attrIOStream, str(source)))
	}
	return &logsv1.LogRecord{TimeUnixNano: ts, Body: str(text), Attributes: attrs}
}

func exportRequest(attrs []*commonv1.KeyValue, records ...*logsv1.LogRecord) *otlplogsv1.ExportLogsServiceRequest {
	return &otlplogsv1.ExportLogsServiceRequest{ResourceLogs: []*logsv1.ResourceLogs{{
		Resource:  &resourcev1.Resource{Attributes: attrs},
		ScopeLogs: []*logsv1.ScopeLogs{{LogRecords: records}},
	}}}
}

// A stream's and a span's project is ferun.project, else its earlier
// word, which a pod placed before ADR 0061 keeps for its life; without
// either a batch is refused.
func TestProjectLabel(t *testing.T) {
	for _, c := range []struct {
		attrs map[string]string
		want  string
	}{
		{map[string]string{attrProject: "acme"}, "acme"},
		{map[string]string{attrProjectLegacy: "old"}, "old"},
		{map[string]string{attrProject: "acme", attrProjectLegacy: "acme"}, "acme"},
		{map[string]string{attrProject: "new", attrProjectLegacy: "old"}, "new"},
		{map[string]string{}, ""},
	} {
		if got := labelsOf(c.attrs, "n1", SourceContainer).Project; got != c.want {
			t.Errorf("%v: project %q, want %q", c.attrs, got, c.want)
		}
	}
	legacy := []*commonv1.KeyValue{kv(attrPod, str("p1")), kv(attrContainer, str("main")), kv(attrLogID, str("L1")),
		kv(attrProjectLegacy, str("acme"))}
	got, err := parseExport(exportRequest(legacy, record(0, 5, "stdout", "hi")), ingestPeer{node: "node-1"}, time.Unix(1000, 0))
	if err != nil || len(got) != 1 || got[0].Labels.Project != "acme" {
		t.Fatalf("a stream of before: %+v %v", got, err)
	}
	if _, err := parseExport(exportRequest(legacy[:3], record(0, 5, "stdout", "hi")), ingestPeer{node: "node-1"}, time.Unix(1000, 0)); err == nil {
		t.Fatal("a stream without a project was taken")
	}
	attrs := podAttrs("", "env1", "web")
	delete(attrs, attrProject)
	attrs[attrProjectLegacy] = "acme"
	spans, _, err := parseTraceExport(traceRequest(attrs, testSpan("0000000000000000000000000000000a", "000000000000000b", "", "GET /",
		time.Now().UnixNano(), time.Millisecond)), ingestPeer{node: "node-1"}, time.Now())
	if err != nil || len(spans) != 1 || spans[0].doc.Project != "acme" {
		t.Fatalf("a span of before: %+v %v", spans, err)
	}
}

// A stream's deployment is ferun.deployment, else its earlier word
// ferun.release, which a pod made before keeps for its life (ADR 0018,
// "The deployments' word").
func TestDeploymentLabel(t *testing.T) {
	for _, c := range []struct {
		attrs map[string]string
		want  int
	}{
		{map[string]string{attrDeployment: "7"}, 7},
		{map[string]string{attrRelease: "6"}, 6},
		{map[string]string{attrDeployment: "7", attrRelease: "7"}, 7},
		{map[string]string{attrDeployment: "8", attrRelease: "7"}, 8},
		{map[string]string{}, 0},
	} {
		if got := labelsOf(c.attrs, "n1", SourceContainer).Deployment; got != c.want {
			t.Errorf("%v: deployment %d, want %d", c.attrs, got, c.want)
		}
	}
}
