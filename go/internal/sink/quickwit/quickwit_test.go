// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package quickwit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/sink/kafka"
)

func TestQueryString(t *testing.T) {
	two, off, to := 2, int64(10), int64(30)
	for _, c := range []struct {
		q    Query
		want string
	}{
		{Query{}, "*"},
		{Query{Tenant: "acme", Environment: `e"1`, Replica: &two}, `tenant:"acme" AND environment:"e\"1" AND replica:2`},
		{Query{Tenant: "t", SinceNs: 1, UntilNs: 2e9}, `tenant:"t" AND ts:[1970-01-01T00:00:00.000000001Z TO 1970-01-01T00:00:02.000000000Z]`},
		{Query{Stream: "s_1", OffAfter: &off, OffTo: &to}, `stream:"s_1" AND off:{10 TO 30]`},
		{Query{Levels: 1 | 2}, `level:IN [1 2]`},
		{Query{Levels: 31}, `*`},
		{Query{Words: []string{"user_id=42", `a\b`}, NotWords: []string{"x"}},
			`text:"user_id=42" AND text:"a\\b" AND NOT text:"x"`},
		{Query{Words: []string{"w"}, OrPartial: true}, `((text:"w") OR partial:true)`},
	} {
		if got := c.q.String(); got != c.want {
			t.Errorf("%+v:\n got %s\nwant %s", c.q, got, c.want)
		}
	}
}

func TestIndexID(t *testing.T) {
	for _, c := range []struct {
		prefix string
		days   int
		want   string
	}{
		{"ferun-logs", 30, "ferun-logs-v5-30d"},
		{"ferun-logs", 400, "ferun-logs-v5-400d"},
		{"x", 1, "x-v5-1d"},
	} {
		if got := IndexID(c.prefix, c.days); got != c.want {
			t.Errorf("IndexID(%q, %d) = %q, want %q", c.prefix, c.days, got, c.want)
		}
	}
	// A read's pattern, <prefix>-*, names the indexes of every version,
	// <prefix>-<days>d being version 1's.
	for _, id := range []string{"ferun-logs-30d", IndexID("ferun-logs", 30)} {
		if !strings.HasPrefix(id, "ferun-logs-") {
			t.Errorf("%s is not of the pattern", id)
		}
	}
}

func TestIndexConfig(t *testing.T) {
	var cfg struct {
		IndexID    string `json:"index_id"`
		DocMapping struct {
			TagFields     []string `json:"tag_fields"`
			PartitionKey  string   `json:"partition_key"`
			Timestamp     string   `json:"timestamp_field"`
			FieldMappings []struct {
				Name, Type string
			} `json:"field_mappings"`
		} `json:"doc_mapping"`
		Indexing struct {
			CommitSecs int `json:"commit_timeout_secs"`
		} `json:"indexing_settings"`
		Retention struct {
			Period string `json:"period"`
		} `json:"retention"`
	}
	if err := json.Unmarshal(IndexConfig("ferun-logs-v5-30d", IndexSettings{RetentionDays: 30, CommitSecs: 10, Partitions: 16}), &cfg); err != nil {
		t.Fatal(err)
	}
	m := cfg.DocMapping
	if cfg.IndexID != "ferun-logs-v5-30d" || cfg.Indexing.CommitSecs != 10 || cfg.Retention.Period != "30 days" ||
		m.PartitionKey != "hash_mod((tenant,environment,job), 16)" || fmt.Sprint(m.TagFields) != "[tenant environment job]" ||
		m.Timestamp != "ts" || len(m.FieldMappings) == 0 || m.FieldMappings[0].Name != "ts" || m.FieldMappings[0].Type != "datetime" {
		t.Errorf("config %+v", cfg)
	}
}

// fakeQuickwit answers the index API as Quickwit does for Ensure and
// Checkpoint: an index's metadata, the creation of an index and of its
// source.
type fakeQuickwit struct {
	mu      sync.Mutex
	indexes map[string]string // id: metadata
	sources map[string][]byte // id: the source's body
	calls   []string
	down    bool
}

func (f *fakeQuickwit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if f.down {
		http.Error(w, `{"message":"metastore down"}`, http.StatusInternalServerError)
		return
	}
	rest, _ := strings.CutPrefix(r.URL.Path, "/api/v1/indexes")
	id, sub, _ := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
	id, _ = url.PathUnescape(id)
	switch {
	case r.Method == http.MethodGet && id != "" && sub == "":
		md, ok := f.indexes[id]
		if !ok {
			http.Error(w, `{"message":"index not found"}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, md)
	case r.Method == http.MethodPost && id == "":
		var cfg struct {
			IndexID string `json:"index_id"`
		}
		_ = json.Unmarshal(body, &cfg)
		if _, ok := f.indexes[cfg.IndexID]; ok {
			http.Error(w, `{"message":"index `+cfg.IndexID+` already exists"}`, http.StatusBadRequest)
			return
		}
		f.indexes[cfg.IndexID] = `{"checkpoint":{}}`
	case r.Method == http.MethodPost && sub == "sources":
		if _, ok := f.sources[id]; ok {
			http.Error(w, `{"message":"source kafka already exists"}`, http.StatusBadRequest)
			return
		}
		f.sources[id] = body
	default:
		http.Error(w, `{"message":"no"}`, http.StatusMethodNotAllowed)
	}
}

func TestEnsure(t *testing.T) {
	f := &fakeQuickwit{indexes: map[string]string{}, sources: map[string][]byte{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src := Source{Topic: "ferun-logs-v5-30d", Brokers: []string{"rp-0:9092", "rp-1:9092"}}
	cfg := IndexConfig("ferun-logs-v5-30d", IndexSettings{RetentionDays: 30, CommitSecs: 10, Partitions: 16})
	for range 2 {
		if err := c.Ensure(ctx, "ferun-logs-v5-30d", cfg, src); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(f.calls, ", "); got != "GET /api/v1/indexes/ferun-logs-v5-30d, POST /api/v1/indexes, "+
		"POST /api/v1/indexes/ferun-logs-v5-30d/sources, GET /api/v1/indexes/ferun-logs-v5-30d, "+
		"POST /api/v1/indexes/ferun-logs-v5-30d/sources" {
		t.Errorf("calls %s", got)
	}
	var body struct {
		SourceID   string `json:"source_id"`
		SourceType string `json:"source_type"`
		Format     string `json:"input_format"`
		Params     struct {
			Topic  string            `json:"topic"`
			Client map[string]string `json:"client_params"`
		} `json:"params"`
	}
	if err := json.Unmarshal(f.sources["ferun-logs-v5-30d"], &body); err != nil {
		t.Fatal(err)
	}
	if body.SourceID != "kafka" || body.SourceType != "kafka" || body.Format != "json" || body.Params.Topic != src.Topic ||
		body.Params.Client["bootstrap.servers"] != "rp-0:9092,rp-1:9092" || body.Params.Client["auto.offset.reset"] != "earliest" ||
		body.Params.Client["session.timeout.ms"] != "10000" || body.Params.Client["heartbeat.interval.ms"] != "3000" {
		t.Errorf("source %s", f.sources["ferun-logs-v5-30d"])
	}
	// Another error than "already exists" is one.
	f.mu.Lock()
	f.down = true
	f.mu.Unlock()
	if err := c.Ensure(ctx, "ferun-logs-v5-30d", cfg, src); err == nil {
		t.Error("no error")
	}
}

func TestCheckpoint(t *testing.T) {
	f := &fakeQuickwit{indexes: map[string]string{
		// Positions are those of the last record published, 20 digits; ""
		// none yet; another source's are not the Kafka source's.
		"i": `{"index_config":{},"checkpoint":{"kafka":{"00000000000000000000":"00000000000000000000",
			"00000000000000000003":"00000000000000000041","00000000000000000007":""},
			"_ingest-api-source":{"ingest_partition":"00000000000000000099"}}}`,
		"new": `{"checkpoint":{}}`,
		"bad": `{"checkpoint":{"kafka":{"0":"x"}}}`,
	}, sources: map[string][]byte{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if cp, err := c.Checkpoint(ctx, "i"); err != nil || fmt.Sprint(cp) != "map[0:1 3:42]" {
		t.Errorf("checkpoint %v, %v", cp, err)
	}
	if cp, err := c.Checkpoint(ctx, "new"); err != nil || len(cp) != 0 {
		t.Errorf("a new index's %v, %v", cp, err)
	}
	if _, err := c.Checkpoint(ctx, "bad"); err == nil {
		t.Error("a bad position passed")
	}
	if _, err := c.Checkpoint(ctx, "none"); !IsNotFound(err) {
		t.Errorf("no index: %v", err)
	}
}

// ingestForTest indexes docs through Quickwit's ingest API and returns
// once their split is published: an index filled without its topic.
func ingestForTest(ctx context.Context, c *Client, id string, docs []Doc) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for i := range docs {
		if err := enc.Encode(&docs[i]); err != nil {
			return err
		}
	}
	return c.do(ctx, http.MethodPost, c.search+"/api/v1/"+url.PathEscape(id)+"/ingest?commit=wait_for", "application/x-ndjson",
		body.Bytes(), nil)
}

// TestQuickwit runs against the Quickwit at FERUN_TEST_QUICKWIT_URL (one
// node of every service, e.g. `quickwit run` in its container) and the
// Kafka API at FERUN_TEST_KAFKA_BROKERS (reached at the same addresses by
// the test and by Quickwit): an index made with its source, filled through
// Quickwit's ingest API, answers the sink's searches.
func TestQuickwit(t *testing.T) {
	u, brokers := os.Getenv("FERUN_TEST_QUICKWIT_URL"), os.Getenv("FERUN_TEST_KAFKA_BROKERS")
	if u == "" || brokers == "" {
		t.Skip("FERUN_TEST_QUICKWIT_URL or FERUN_TEST_KAFKA_BROKERS is not set; skipping a test that needs Quickwit and Kafka")
	}
	c, err := New(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	prefix := fmt.Sprintf("test-logs-%d", time.Now().UnixNano())
	id := IndexID(prefix, 1)
	settings := IndexSettings{RetentionDays: 1, CommitSecs: 1, Partitions: 16}
	kc, err := kafka.New(strings.Split(brokers, ","), kafka.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	defer kc.Close()
	if err := kc.EnsureTopic(ctx, id, kafka.TopicConfig{Partitions: 2, Retention: time.Hour, Replication: 1}); err != nil {
		t.Fatal(err)
	}
	src := Source{Topic: id, Brokers: strings.Split(brokers, ",")}
	for range 2 {
		if err := c.Ensure(ctx, id, IndexConfig(id, settings), src); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { _ = c.do(context.Background(), "DELETE", c.search+"/api/v1/indexes/"+id, "", nil, nil) }()
	if cp, err := c.Checkpoint(ctx, id); err != nil || len(cp) != 0 {
		t.Errorf("a new index's checkpoint %v, %v", cp, err)
	}
	now := time.Now().UnixNano()
	docs := []Doc{
		{TS: now, Tenant: "acme", Stream: "s_1", Environment: "e", Source: "container", IOStream: "stdout", Level: 1, Off: 10,
			Text: "GET /x user_id=42 ERROR connection reset", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736"},
		{TS: now + 1, Tenant: "acme", Stream: "s_1", Environment: "e", Source: "container", IOStream: "stdout", Level: 4, Off: 20,
			Text: "second line", Partial: true},
		{TS: now + 1, Tenant: "acme", Stream: "s_2", Environment: "e", Source: "container", IOStream: "stderr", Level: 4, Off: 20,
			Text: "other stream"},
		{TS: now, Tenant: "other", Stream: "s_3", Environment: "e", Source: "container", Level: 1, Off: 0, Text: "ERROR not acme's"},
	}
	// A tenant of many environments and a job: their lines go to splits
	// of their partitions.
	for i, env := range []string{"prod", "stage", "dev", "preview-1", "preview-2", "preview-3"} {
		docs = append(docs, Doc{TS: now + int64(i), Tenant: "part", Stream: "s_" + env, Environment: env, Source: "container",
			Level: 1, Text: "a line of " + env})
	}
	docs = append(docs, Doc{TS: now, Tenant: "part", Stream: "s_job", Job: "build-1", Source: "container", Level: 1, Text: "a job's line"})
	if err := ingestForTest(ctx, c, id, docs); err != nil {
		t.Fatal(err)
	}
	pattern := prefix + "-*"
	search := func(q Query) string {
		t.Helper()
		if q.Max == 0 {
			q.Max = 100
		}
		hits, err := c.Search(ctx, pattern, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, fmt.Sprintf("%s/%d", h.Stream, h.Off))
		}
		return strings.Join(out, " ")
	}
	for _, x := range []struct {
		q    Query
		want string
	}{
		{Query{Tenant: "acme"}, "s_2/20 s_1/20 s_1/10"},
		{Query{Tenant: "acme", Order: OldestFirst}, "s_1/10 s_1/20 s_2/20"},
		{Query{Tenant: "acme", Words: []string{"user_id=42"}}, "s_1/10"},
		{Query{Tenant: "acme", Words: []string{"erro"}}, ""},
		{Query{Tenant: "acme", Words: []string{"reset"}, OrPartial: true}, "s_1/20 s_1/10"},
		{Query{Tenant: "acme", NotWords: []string{"second"}}, "s_2/20 s_1/10"},
		{Query{Tenant: "acme", Levels: 1}, "s_1/10"},
		{Query{Tenant: "acme", Trace: "4bf92f3577b34da6a3ce929d0e0e4736"}, "s_1/10"},
		{Query{Tenant: "acme", SinceNs: now + 1, UntilNs: now + 1}, "s_2/20 s_1/20"},
		{Query{Tenant: "acme", Stream: "s_1", OffAfter: new(int64), OffTo: ptr(int64(15)), Order: ByOffset}, "s_1/10"},
		{Query{Tenant: "acme", IOStream: "stderr"}, "s_2/20"},
		{Query{Tenant: "acme", Max: 1}, "s_2/20"},
	} {
		if got := search(x.q); got != x.want {
			t.Errorf("%s: got %q, want %q", x.q, got, x.want)
		}
	}
	hits, err := c.Search(ctx, pattern, Query{Tenant: "acme", Stream: "s_1", Max: 1})
	if err != nil || len(hits) != 1 || hits[0].TS != now+1 || !hits[0].Partial || hits[0].Text != "second line" {
		t.Errorf("a hit's fields: %+v, %v", hits, err)
	}

	// Partitions: a split holds few environments and records them as
	// tags, so a search for one environment reads fewer splits.
	var splits struct {
		Splits []struct {
			SplitID string   `json:"split_id"`
			Tags    []string `json:"tags"`
		} `json:"splits"`
	}
	if err := c.do(ctx, "GET", c.search+"/api/v1/indexes/"+id+"/splits", "", nil, &splits); err != nil {
		t.Fatal(err)
	}
	withProd := 0
	for _, sp := range splits.Splits {
		tags := strings.Join(sp.Tags, " ")
		if !strings.Contains(tags, "tenant!") || !strings.Contains(tags, "environment!") || !strings.Contains(tags, "job!") {
			t.Errorf("split %s without its tag fields: %v", sp.SplitID, sp.Tags)
		}
		if strings.Contains(tags, "environment:prod") {
			withProd++
		}
	}
	if len(splits.Splits) < 2 || withProd != 1 {
		t.Errorf("%d splits, %d with prod: %+v", len(splits.Splits), withProd, splits.Splits)
	}
	plan := func(q Query) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"query": q.String(), "max_hits": 10})
		var out struct {
			Searched []string `json:"searched_splits"`
		}
		if err := c.do(ctx, "POST", c.search+"/api/v1/"+id+"/search-plan", "application/json", body, &out); err != nil {
			t.Fatal(err)
		}
		return len(out.Searched)
	}
	if all, prod, job := plan(Query{Tenant: "part"}), plan(Query{Tenant: "part", Environment: "prod"}),
		plan(Query{Tenant: "part", Job: "build-1"}); prod != 1 || job != 1 || all < 2 {
		t.Errorf("splits searched: %d for the tenant, %d for an environment, %d for a job", all, prod, job)
	}

	// An index of version 1 (<prefix>-<days>d: tenant its only tag, no
	// partitions) is searched with the current one's, in one order.
	old := prefix + "-1d"
	var v1 map[string]any
	if err := json.Unmarshal(IndexConfig(old, settings), &v1); err != nil {
		t.Fatal(err)
	}
	dm, ok := v1["doc_mapping"].(map[string]any)
	if !ok {
		t.Fatalf("config %v", v1)
	}
	delete(dm, "partition_key")
	dm["tag_fields"] = []string{"tenant"}
	body, _ := json.Marshal(v1)
	if err := c.do(ctx, "POST", c.search+"/api/v1/indexes", "application/json", body, nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.do(context.Background(), "DELETE", c.search+"/api/v1/indexes/"+old, "", nil, nil) }()
	if err := ingestForTest(ctx, c, old, []Doc{{TS: now + 10, Tenant: "part", Stream: "s_prod", Environment: "prod",
		Source: "container", Level: 1, Off: 5, Text: "a line of before"}}); err != nil {
		t.Fatal(err)
	}
	hits, err = c.Search(ctx, pattern, Query{Tenant: "part", Environment: "prod", Max: 10})
	if err != nil || len(hits) != 2 || hits[0].TS != now+10 || hits[0].Text != "a line of before" || hits[1].Text != "a line of prod" {
		t.Errorf("across versions: %+v, %v", hits, err)
	}

	ids, err := c.Indexes(ctx, prefix+"-")
	slices.Sort(ids)
	if err != nil || fmt.Sprint(ids) != fmt.Sprint([]string{old, id}) {
		t.Errorf("indexes %v, %v", ids, err)
	}
	if err := c.Delete(ctx, id, Query{Tenant: "acme", Environment: "e"}); err != nil {
		t.Errorf("delete task: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
