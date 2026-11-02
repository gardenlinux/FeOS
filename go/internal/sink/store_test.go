// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// quickwitQuery selects a tenant's environment, as many as the tests hold.
func quickwitQuery(tenant, env string) quickwit.Query {
	return quickwit.Query{Tenant: tenant, Environment: env, Max: 100000}
}

func testConfig() Config {
	c := Default()
	c.Name = "sink-0"
	return c
}

// logTopic is the topic (and index) of the default retention of
// testConfig.
var logTopic = quickwit.IndexID("ferun-logs", 30)

// newTestDB is a migrated, empty database of the sink's own.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	d := dbtest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := Migrate(ctx, d); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return d
}

// newTestStore is a store over a new database, the index in memory and
// the topics in memory; idx.sync(lg) is a commit of the index's sources.
func newTestStore(t *testing.T, cfg Config) (*Store, *memIndex, *memLog) {
	t.Helper()
	d := newTestDB(t)
	idx, lg := newMemIndex(), newMemLog()
	s := NewStore(d, idx, lg, cfg, dbtest.Logger(t))
	// The program makes the topics and indexes at its start.
	if err := s.EnsureIndexes(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, idx, lg
}

// base is the tests' time of the first line.
var base = time.Now().Add(-5 * time.Minute).UnixNano()

func envLabels(tenant, env string, replica int) Labels {
	return Labels{Tenant: tenant, Node: "node-1", Instance: "inst" + fmt.Sprint(replica), Environment: env, Kind: "service",
		Object: "api", Replica: replica, Deployment: 3, Attrs: map[string]string{attrTenant: tenant}}
}

// lines makes n lines from offset off on, 10 bytes apart, a millisecond
// apart from ts on.
func lines(off int64, ts int64, n int, text func(i int) string) []Line {
	out := make([]Line, n)
	for i := range out {
		t := text(i)
		lvl, tr := Extract(t)
		out[i] = Line{Off: off + int64(i)*10, TS: ts + int64(i)*int64(time.Millisecond), Source: "stdout", Level: lvl, Trace: tr, Text: t}
	}
	return out
}

func batch(key StreamKey, l Labels, ls []Line) []StreamBatch {
	return []StreamBatch{{Key: key, Labels: l, Lines: ls, Next: -1}}
}

func mustRead(t *testing.T, s *Store, tenant string, v url.Values) scanResult {
	t.Helper()
	q, err := parseQuery(v, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Read(context.Background(), tenant, q)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func texts(r scanResult, forward bool) []string {
	p := r.page(forward)
	out := make([]string, len(p.Lines))
	for i, l := range p.Lines {
		out[i] = l.Text
	}
	return out
}

// pageAll reads every page of v in its direction, oldest first.
func pageAll(t *testing.T, s *Store, v url.Values) []string {
	t.Helper()
	forward := v.Get("direction") == "forward"
	var got []string
	for pages := 0; ; pages++ {
		if pages > 200 {
			t.Fatal("too many pages")
		}
		r := mustRead(t, s, "acme", v)
		if forward {
			got = append(got, texts(r, true)...)
		} else {
			got = append(texts(r, false), got...)
		}
		if r.cursor == nil {
			return got
		}
		v.Set("cursor", encodeCursor(forward, *r.cursor))
	}
}

// docsIn are the documents of topic's records, in the topic's order.
func docsIn(t *testing.T, lg *memLog, topic string) []quickwit.Doc {
	t.Helper()
	var out []quickwit.Doc
	for _, r := range lg.records(topic) {
		var d quickwit.Doc
		if err := json.Unmarshal(r.Value, &d); err != nil {
			t.Fatalf("record %d/%d: %v", r.Partition, r.Offset, err)
		}
		out = append(out, d)
	}
	return out
}

// produceAgain appends the records of topic's stream sid to the topic once
// more, as after an answer to the node that did not arrive.
func produceAgain(t *testing.T, lg *memLog, topic, sid string) int {
	t.Helper()
	var again []kafka.Record
	for _, r := range lg.records(topic) {
		var d quickwit.Doc
		if json.Unmarshal(r.Value, &d) == nil && d.Stream == sid {
			again = append(again, kafka.Record{Partition: r.Partition, Key: r.Key, Value: r.Value})
		}
	}
	if err := lg.Produce(context.Background(), topic, again); err != nil {
		t.Fatal(err)
	}
	return len(again)
}

// streamSID is the stream ID of the stream of key.
func streamSID(t *testing.T, s *Store, key StreamKey) string {
	t.Helper()
	var sid string
	if err := s.db.Pool.QueryRow(context.Background(), `SELECT sid FROM log_streams WHERE pod = $1 AND container = $2 AND log_id = $3`,
		key.Pod, key.Container, key.LogID).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	return sid
}

func TestIngestIdempotent(t *testing.T) {
	s, idx, lg := newTestStore(t, testConfig())
	ctx := context.Background()
	key := StreamKey{"pod1", "main", "L1"}
	l := envLabels("acme", "env1", 0)
	ls := lines(0, base, 5, func(i int) string { return fmt.Sprintf("line %d", i) })
	for range 3 {
		if err := s.Ingest(ctx, batch(key, l, ls)); err != nil {
			t.Fatal(err)
		}
	}
	// A batch sent again produces nothing.
	if lg.produces != 1 {
		t.Errorf("%d produces of a batch sent three times", lg.produces)
	}
	// Overlapping and new, and labels that lose to the first write's.
	l2 := envLabels("acme", "other", 1)
	more := lines(30, base+int64(3*time.Millisecond), 4, func(i int) string { return fmt.Sprintf("line %d", i+3) })
	if err := s.Ingest(ctx, batch(key, l2, more)); err != nil {
		t.Fatal(err)
	}
	var lineCount, bytes, next, lastOff int64
	var env string
	if err := s.db.Pool.QueryRow(ctx, `SELECT lines, bytes, next_offset, last_off, environment FROM log_streams`).
		Scan(&lineCount, &bytes, &next, &lastOff, &env); err != nil {
		t.Fatal(err)
	}
	if lineCount != 7 || bytes != 7*6 || next != 60 || lastOff != 60 || env != "env1" {
		t.Errorf("lines %d, bytes %d, next %d, last %d, env %q", lineCount, bytes, next, lastOff, env)
	}
	// A record per (stream, offset), in the stream's partition, keyed by
	// its tenant, environment and object, its document of the first
	// write's labels.
	sid := streamSID(t, s, key)
	recs := lg.records(logTopic)
	part := kafka.Partition("acme/env1/service/api", int32(testConfig().KafkaPartitions))
	var offs []int64
	for _, r := range recs {
		var d quickwit.Doc
		if err := json.Unmarshal(r.Value, &d); err != nil {
			t.Fatal(err)
		}
		if string(r.Key) != "acme/env1/service/api" || r.Partition != part || d.Stream != sid || d.Tenant != "acme" ||
			d.Environment != "env1" || d.Replica != 0 || d.Deployment != 3 || d.Source != SourceContainer || d.IOStream != "stdout" {
			t.Errorf("record %q in %d: %+v", r.Key, r.Partition, d)
		}
		offs = append(offs, d.Off)
	}
	if fmt.Sprint(offs) != "[0 10 20 30 40 50 60]" {
		t.Errorf("offsets in the topic %v", offs)
	}
	// A run of an environment's job lies in its object's partition, a
	// tenant's job (no environment) in its job's.
	job := Labels{Tenant: "acme", Environment: "env1", Kind: "job", Object: "migrate", Attempt: 1, Attrs: map[string]string{}}
	if err := s.Ingest(ctx, batch(StreamKey{"pod2", "main", "L1"}, job, lines(0, base, 1, func(int) string { return "j" }))); err != nil {
		t.Fatal(err)
	}
	tjob := Labels{Tenant: "acme", Job: "build-2", Attrs: map[string]string{}}
	if err := s.Ingest(ctx, batch(StreamKey{"pod3", "main", "L1"}, tjob, lines(0, base, 1, func(int) string { return "t" }))); err != nil {
		t.Fatal(err)
	}
	recs = lg.records(logTopic)
	inEnv, jobs := 0, 0
	for _, r := range recs {
		switch string(r.Key) {
		case "acme/env1/service/api", "acme/env1/job/migrate":
			inEnv++
		case "acme//build-2":
			jobs++
			if r.Partition != kafka.Partition("acme//build-2", int32(testConfig().KafkaPartitions)) {
				t.Errorf("the tenant's job's record in %d", r.Partition)
			}
		}
	}
	if len(recs) != 9 || inEnv != 8 || jobs != 1 {
		t.Errorf("%d records, %d of the environment, %d of the tenant's job", len(recs), inEnv, jobs)
	}

	pos, err := s.Positions(ctx, []StreamKey{{"nope", "x", "y"}, key})
	if err != nil || pos[0] != 0 || pos[1] != 60 {
		t.Errorf("positions %v, %v", pos, err)
	}
	// A node that sends log.next_offset gets it back.
	b := batch(key, l, lines(70, base+int64(10*time.Millisecond), 1, func(int) string { return "x" }))
	b[0].Next = 82
	if err := s.Ingest(ctx, b); err != nil {
		t.Fatal(err)
	}
	if pos, _ := s.Positions(ctx, []StreamKey{key}); pos[0] != 82 {
		t.Errorf("positions after next_offset %v", pos)
	}
	if err := s.EndStreams(ctx, []StreamKey{key, {"nope", "x", "y"}}, 0); err != nil {
		t.Fatal(err)
	}
	// Read from the topic's tail, then from the index once it has them.
	for _, when := range []string{"tail", "index"} {
		if when == "index" {
			idx.sync(lg)
			if n := idx.count("acme"); n != 10 {
				t.Fatalf("%d documents", n)
			}
		}
		r := mustRead(t, s, "acme", url.Values{"environment": {"env1"}})
		if got := strings.Join(texts(r, false), "|"); got != "line 0|j|line 1|line 2|line 3|line 4|line 5|line 6|x" {
			t.Errorf("%s: read %s", when, got)
		}
		for _, st := range r.streams {
			if st.SID == sid && st.EndedAt == nil {
				t.Errorf("%s: stream not ended", when)
			}
		}
		if r := mustRead(t, s, "other-tenant", url.Values{"environment": {"env1"}}); len(r.lines) != 0 {
			t.Errorf("%s: another tenant reads the stream", when)
		}
	}
}

func TestQuota(t *testing.T) {
	cfg := testConfig()
	cfg.Quota = 100
	s, _, lg := newTestStore(t, cfg)
	ctx := context.Background()
	text := func(int) string { return strings.Repeat("a", 60) }
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "1"}, envLabels("small", "e", 0), lines(0, base, 2, text))); err != nil {
		t.Fatal(err)
	}
	// Over now: small's next batch is refused, big's in the same batch stored.
	b := append(batch(StreamKey{"p", "c", "1"}, envLabels("small", "e", 0), lines(100, base, 1, text)),
		batch(StreamKey{"q", "c", "1"}, envLabels("big", "e", 0), lines(0, base, 1, text))...)
	err := s.Ingest(ctx, b)
	if !errors.Is(err, ErrQuota) || !strings.Contains(err.Error(), "small") {
		t.Fatalf("over quota: %v", err)
	}
	per := map[string]int{}
	for _, d := range docsIn(t, lg, logTopic) {
		per[d.Tenant]++
	}
	var smallLast, smallUsed, bigUsed int64
	if err := s.db.Pool.QueryRow(ctx, `SELECT (SELECT last_off FROM log_streams WHERE tenant = 'small'),
		(SELECT bytes FROM log_usage WHERE tenant = 'small'), (SELECT bytes FROM log_usage WHERE tenant = 'big')`).
		Scan(&smallLast, &smallUsed, &bigUsed); err != nil {
		t.Fatal(err)
	}
	if per["small"] != 2 || per["big"] != 1 || smallLast != 10 || smallUsed != 120 || bigUsed != 60 {
		t.Errorf("records %v, small's last offset %d, usage %d and %d", per, smallLast, smallUsed, bigUsed)
	}
}

// A produce that fails stores nothing, neither a stream's row nor its
// counters nor the usage; the node sends again, and everything is stored.
func TestProduceFailure(t *testing.T) {
	s, _, lg := newTestStore(t, testConfig())
	ctx := context.Background()
	known, fresh := StreamKey{"p", "c", "1"}, StreamKey{"q", "c", "1"}
	text := func(i int) string { return fmt.Sprintf("line %d", i) }
	if err := s.Ingest(ctx, batch(known, envLabels("acme", "e", 0), lines(0, base, 2, text))); err != nil {
		t.Fatal(err)
	}
	b := append(batch(known, envLabels("acme", "e", 0), lines(0, base, 5, text)),
		batch(fresh, envLabels("acme", "e", 1), lines(0, base, 3, text))...)
	lg.failProduce = errors.New("kafka down")
	if err := s.Ingest(ctx, b); err == nil {
		t.Fatal("no error")
	}
	state := func() string {
		t.Helper()
		var streams, lines, last, used int64
		if err := s.db.Pool.QueryRow(ctx, `SELECT count(*), sum(lines), max(last_off),
			(SELECT sum(bytes) FROM log_usage) FROM log_streams`).Scan(&streams, &lines, &last, &used); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%d streams, %d lines, last %d, %d bytes used, %d records", streams, lines, last, used,
			lg.count(logTopic))
	}
	if got := state(); got != "1 streams, 2 lines, last 10, 12 bytes used, 2 records" {
		t.Errorf("after the failed produce: %s", got)
	}
	if pos, _ := s.Positions(ctx, []StreamKey{known, fresh}); fmt.Sprint(pos) != "[10 0]" {
		t.Errorf("positions %v", pos)
	}
	lg.failProduce = nil
	if err := s.Ingest(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "2 streams, 8 lines, last 40, 48 bytes used, 8 records" {
		t.Errorf("sent again: %s", got)
	}
	r := mustRead(t, s, "acme", url.Values{"environment": {"e"}})
	if len(r.lines) != 8 {
		t.Errorf("read %v", texts(r, false))
	}
}

// Instances of the sink take a stream's lines once between them: the
// stream's row is the lock.
func TestIngestAcrossInstances(t *testing.T) {
	d := newTestDB(t)
	idx, lg := newMemIndex(), newMemLog()
	ctx := context.Background()
	stores := make([]*Store, 4)
	for i := range stores {
		c := testConfig()
		c.Name = fmt.Sprintf("sink-%d", i)
		stores[i] = NewStore(d, idx, lg, c, dbtest.Logger(t))
		if err := stores[i].EnsureIndexes(ctx); err != nil {
			t.Fatal(err)
		}
	}
	const streams, rounds = 10, 5
	var wg sync.WaitGroup
	errc := make(chan error, len(stores))
	for _, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Every instance sends every batch, as nodes do that retry
			// at another instance.
			for r := range rounds {
				var b []StreamBatch
				for i := range streams {
					b = append(b, batch(StreamKey{fmt.Sprintf("pod%d", i), "c", "1"}, envLabels("acme", "e", i),
						lines(int64(r)*100, base+int64(r)*int64(time.Second), 5, func(j int) string { return fmt.Sprintf("r%d l%d", r, j) }))...)
				}
				if err := s.Ingest(ctx, b); err != nil {
					errc <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, d := range docsIn(t, lg, logTopic) {
		k := fmt.Sprintf("%s/%d", d.Stream, d.Off)
		if seen[k] {
			t.Errorf("%s twice in the topic", k)
		}
		seen[k] = true
	}
	if len(seen) != streams*rounds*5 {
		t.Errorf("%d records", len(seen))
	}
	idx.sync(lg)
	r := mustRead(t, stores[1], "acme", url.Values{"environment": {"e"}, "limit": {"5000"}, "since": {fmt.Sprint(base - 1)}})
	if len(r.lines) != streams*rounds*5 {
		t.Errorf("read %d lines", len(r.lines))
	}
}

func TestReadTailAndIndex(t *testing.T) {
	cfg := testConfig()
	s, idx, lg := newTestStore(t, cfg)
	ctx := context.Background()
	key1, key2 := StreamKey{"pod1", "main", "L1"}, StreamKey{"pod2", "main", "L1"}
	// Stream 1: 30 lines, in the index; stream 2: interleaved in time, its
	// first batch in the index and in the topic's tail again, as after an
	// answer lost, its last two only in the tail.
	ln := func(prefix string) func(int) string {
		return func(i int) string {
			lvl := "INFO"
			if i%10 == 7 {
				lvl = "ERROR"
			}
			return fmt.Sprintf("%s %s line %02d", lvl, prefix, i)
		}
	}
	for i := 0; i < 30; i += 10 {
		if err := s.Ingest(ctx, batch(key1, envLabels("acme", "env1", 0),
			lines(int64(i)*10, base+int64(2*i)*int64(time.Millisecond), 10, func(j int) string { return ln("a")(i + j) }))); err != nil {
			t.Fatal(err)
		}
		idx.sync(lg)
	}
	if err := s.Ingest(ctx, batch(key2, envLabels("acme", "env1", 1),
		lines(0, base+int64(time.Millisecond), 10, func(j int) string { return ln("b")(j) }))); err != nil {
		t.Fatal(err)
	}
	idx.sync(lg)
	if n := produceAgain(t, lg, logTopic, streamSID(t, s, key2)); n != 10 {
		t.Fatalf("%d records again", n)
	}
	for k := range 2 {
		if err := s.Ingest(ctx, batch(key2, envLabels("acme", "env1", 1),
			lines(100+int64(k)*100, base+int64(11+10*k)*int64(time.Millisecond), 10, func(j int) string { return ln("b")(j + 10 + 10*k) }))); err != nil {
			t.Fatal(err)
		}
	}
	if n, recs := idx.count("acme"), lg.count(logTopic); n != 40 || recs != 70 {
		t.Fatalf("indexed %d, %d records", n, recs)
	}
	if ids, _ := idx.Indexes(ctx, ""); fmt.Sprint(ids) != "[ferun-logs-v5-30d ferun-logs-v5-400d]" {
		t.Errorf("indexes %v", ids)
	}
	// A read finds the lines of an index of before (version 1,
	// <prefix>-<days>d) with the current version's: stream 1's first
	// batch moves there.
	idx.mu.Lock()
	v3 := idx.docs[logTopic]
	idx.docs["ferun-logs-30d"], idx.docs[logTopic] = v3[:10], v3[10:]
	idx.mu.Unlock()
	// A line sent again after it was indexed is not stored again.
	produces := lg.produces
	if err := s.Ingest(ctx, batch(key1, envLabels("acme", "env1", 0), lines(0, base, 1, ln("a")))); err != nil {
		t.Fatal(err)
	}
	if lg.produces != produces {
		t.Error("a line indexed already was produced again")
	}

	reads := func(when string) {
		t.Helper()
		all := mustRead(t, s, "acme", url.Values{"environment": {"env1"}, "limit": {"100"}})
		if len(all.lines) != 60 || all.cursor != nil || all.partial {
			t.Fatalf("%s: %d lines, cursor %v, partial %v", when, len(all.lines), all.cursor, all.partial)
		}
		page := all.page(false)
		for i := 1; i < len(page.Lines); i++ {
			if page.Lines[i].TS < page.Lines[i-1].TS {
				t.Fatalf("%s: lines not oldest first at %d", when, i)
			}
		}
		if len(page.Streams) != 2 {
			t.Errorf("%s: %d streams", when, len(page.Streams))
		}

		// Pages backward and forward meet every line once.
		for _, dir := range []string{"backward", "forward"} {
			v := url.Values{"environment": {"env1"}, "limit": {"7"}, "direction": {dir}}
			if dir == "forward" {
				v.Set("since", fmt.Sprint(base-int64(time.Hour)))
			}
			got := pageAll(t, s, v)
			seen := map[string]bool{}
			for _, x := range got {
				if seen[x] {
					t.Errorf("%s, %s: %q twice", when, dir, x)
				}
				seen[x] = true
			}
			if len(seen) != 60 || !slices.Equal(got, texts(all, false)) {
				t.Errorf("%s, %s: %d lines", when, dir, len(seen))
			}
		}

		// Filters, across both.
		errs := mustRead(t, s, "acme", url.Values{"environment": {"env1"}, "level": {"error"}})
		if got := strings.Join(texts(errs, false), "|"); got != "ERROR a line 07|ERROR b line 07|ERROR b line 17|ERROR a line 17|ERROR b line 27|ERROR a line 27" {
			t.Errorf("%s: errors: %s", when, got)
		}
		words := mustRead(t, s, "acme", url.Values{"environment": {"env1"}, "q": {"B -error"}, "replica": {"1"}})
		if len(words.lines) != 27 {
			t.Errorf("%s: words: %d lines", when, len(words.lines))
		}
		re := mustRead(t, s, "acme", url.Values{"instance": {"inst1"}, "re": {`line 2[0-4]$`}})
		if len(re.lines) != 5 {
			t.Errorf("%s: re: %d lines", when, len(re.lines))
		}

		// Streams list.
		sts, err := s.Streams(ctx, "acme", Selector{Environment: "env1"}, 0)
		if err != nil || len(sts) != 2 || sts[0].Lines != 30 {
			t.Fatalf("%s: streams %v, %v", when, sts, err)
		}
	}
	reads("tail")
	// Quickwit commits: the index holds stream 2's first batch twice now.
	idx.sync(lg)
	if n := idx.count("acme"); n != 70 {
		t.Fatalf("indexed %d", n)
	}
	reads("index")
}

func TestTailAndIndexInterleaved(t *testing.T) {
	s, idx, lg := newTestStore(t, testConfig())
	ctx := context.Background()
	ms := int64(time.Millisecond)
	type want struct {
		ts     int64
		pod    string
		off    int64
		text   string
		errors bool
	}
	var all []want
	// Two streams, six batches each that overlap in time, B's third at
	// the very times of A's; the first two of each in the index, the rest
	// in the tail.
	for k := range 6 {
		for _, pod := range []string{"a", "b"} {
			var ls []Line
			for i := range 4 {
				ts := base + int64(8*k+2*i)*ms
				if pod == "b" && k != 2 {
					ts += ms
				}
				text := fmt.Sprintf("%s k%d i%d", pod, k, i)
				if i == 3 {
					text = "ERROR " + text
				}
				lvl, _ := Extract(text)
				off := int64(k*40 + i*10)
				ls = append(ls, Line{Off: off, TS: ts, Source: "stdout", Level: lvl, Text: text})
				all = append(all, want{ts, pod, off, text, i == 3})
			}
			if err := s.Ingest(ctx, batch(StreamKey{pod, "main", "L"}, envLabels("acme", "e", 0), ls)); err != nil {
				t.Fatal(err)
			}
		}
		if k == 1 {
			idx.sync(lg)
		}
	}
	if n, recs := idx.count("acme"), lg.count(logTopic); n != 16 || recs != 48 {
		t.Fatalf("indexed %d of %d", n, recs)
	}
	// The scan's order: time, stream (a's row first), offset.
	slices.SortFunc(all, func(x, y want) int {
		if c := cmpInt(x.ts, y.ts); c != 0 {
			return c
		}
		if c := strings.Compare(x.pod, y.pod); c != 0 {
			return c
		}
		return cmpInt(x.off, y.off)
	})
	var every, errs []string
	for _, w := range all {
		every = append(every, w.text)
		if w.errors {
			errs = append(errs, w.text)
		}
	}
	since := fmt.Sprint(base - 1)
	for _, dir := range []string{"backward", "forward"} {
		for _, c := range []struct {
			name  string
			limit string
			level string
			want  []string
		}{
			{"all", "1000", "", every}, {"pages", "5", "", every}, {"errors", "2", "error", errs},
		} {
			v := url.Values{"environment": {"e"}, "direction": {dir}, "since": {since}, "limit": {c.limit}}
			if c.level != "" {
				v.Set("level", c.level)
			}
			if got := pageAll(t, s, v); !slices.Equal(got, c.want) {
				t.Errorf("%s %s:\n got %v\nwant %v", dir, c.name, got, c.want)
			}
		}
	}
}

// The scan limit bounds what a read takes from the index: past it the page
// is partial, with a cursor that goes on from where it stopped.
func TestScanLimit(t *testing.T) {
	cfg := testConfig()
	cfg.ScanLimit = 100
	s, idx, lg := newTestStore(t, cfg)
	ctx := context.Background()
	key, l := StreamKey{"p", "c", "1"}, envLabels("acme", "e", 0)
	if err := s.Ingest(ctx, batch(key, l, lines(0, base, 600, func(i int) string { return fmt.Sprintf("old %03d", i) }))); err != nil {
		t.Fatal(err)
	}
	idx.sync(lg)
	if err := s.Ingest(ctx, batch(key, l, lines(6000, base+int64(time.Second), 5, func(i int) string { return fmt.Sprintf("new %d", i) }))); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"backward", "forward"} {
		// The index answers every line, the sink's filter none of the
		// old: the first page stops at the limit.
		v := url.Values{"environment": {"e"}, "direction": {dir}, "since": {fmt.Sprint(base - 1)}, "limit": {"10"},
			"re": {"^new|old 59[89]"}}
		first := mustRead(t, s, "acme", v)
		if !first.partial || first.cursor == nil {
			t.Errorf("%s: first page: partial %v, cursor %v", dir, first.partial, first.cursor)
		}
		want := "old 598|old 599|new 0|new 1|new 2|new 3|new 4"
		if got := strings.Join(pageAll(t, s, v), "|"); got != want {
			t.Errorf("%s: %s", dir, got)
		}
	}
}

func TestTraces(t *testing.T) {
	cfg := testConfig()
	s, idx, lg := newTestStore(t, cfg)
	ctx := context.Background()
	other := "0af7651916cd43dd8448eb211c80319c"
	text := func(i int) string {
		tr := other
		if i == 3 {
			tr = traceA
		}
		return fmt.Sprintf(`{"level":"info","i":%d,"trace_id":"%s"}`, i, tr)
	}
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "1"}, envLabels("acme", "e", 0), lines(0, base-int64(48*time.Hour), 5, text))); err != nil {
		t.Fatal(err)
	}
	idx.sync(lg)
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "1"}, envLabels("acme", "e", 0), lines(100, base, 5, text))); err != nil {
		t.Fatal(err)
	}
	// Two days back, without since: the index finds the old one, the tail
	// the new.
	r := mustRead(t, s, "acme", url.Values{"environment": {"e"}, "trace": {traceA}})
	if len(r.lines) != 2 {
		t.Errorf("trace: %d lines: %v", len(r.lines), texts(r, false))
	}
}

func TestRetentionAndDelete(t *testing.T) {
	cfg := testConfig()
	cfg.Retention = 24 * time.Hour
	cfg.RetentionMax = 72 * time.Hour
	s, idx, lg := newTestStore(t, cfg)
	ctx := context.Background()
	old := base - int64(30*time.Hour)
	text := func(i int) string { return fmt.Sprintf("line %d", i) }
	// Default retention: expires. retention_days 2: kept. 400: clamped to
	// 3 days, expires.
	short, long := envLabels("acme", "e", 0), envLabels("acme", "e", 1)
	long.RetentionDays = 2
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "short"}, short, lines(0, old, 3, text))); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "long"}, long, lines(0, old, 3, text))); err != nil {
		t.Fatal(err)
	}
	ancient := envLabels("acme", "e", 2)
	ancient.RetentionDays = 400
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "ancient"}, ancient, lines(0, base-int64(100*time.Hour), 1, text))); err != nil {
		t.Fatal(err)
	}
	idx.sync(lg)
	// An index (and topic) per retention: the default's, and the longest's
	// for the streams that ask for more.
	if ids, _ := idx.Indexes(ctx, "ferun-logs-"); fmt.Sprint(ids) != "[ferun-logs-v5-1d ferun-logs-v5-3d]" || idx.count("acme") != 7 ||
		lg.count("ferun-logs-v5-1d") != 3 || lg.count("ferun-logs-v5-3d") != 4 {
		t.Fatalf("indexes %v, %d documents", ids, idx.count("acme"))
	}
	// A line in the tail of a stream past retention is not read either.
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "short"}, short, lines(100, old, 1, text))); err != nil {
		t.Fatal(err)
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var streams []string
	rows, _ := s.db.Pool.Query(ctx, `SELECT log_id FROM log_streams ORDER BY log_id`)
	for rows.Next() {
		var x string
		_ = rows.Scan(&x)
		streams = append(streams, x)
	}
	rows.Close()
	if fmt.Sprint(streams) != "[long]" {
		t.Errorf("after the sweep: streams %v", streams)
	}
	// The index keeps what its own retention keeps; a read finds only the
	// streams left.
	r := mustRead(t, s, "acme", url.Values{"environment": {"e"}, "since": {fmt.Sprint(base - int64(200*time.Hour))}})
	if len(r.lines) != 3 {
		t.Errorf("after the sweep: %d lines read: %v", len(r.lines), texts(r, false))
	}

	// DELETE of an environment's streams: the rows and a delete task of
	// every index; a line in the tail is read no more, its stream gone.
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "new"}, short, lines(0, base, 2, text))); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "job"}, Labels{Tenant: "acme", Job: "j1", Attrs: map[string]string{}},
		lines(0, base, 2, text))); err != nil {
		t.Fatal(err)
	}
	// An index of before (version 1) gets its delete task too.
	idx.add("ferun-logs-1d", quickwit.Doc{Tenant: "acme", Environment: "e", Stream: "s_v1", Text: "v1"})
	if n, err := s.DeleteStreams(ctx, "other", "e", ""); err != nil || n != 0 {
		t.Fatalf("another tenant deleted %d: %v", n, err)
	}
	n, err := s.DeleteStreams(ctx, "acme", "e", "")
	if err != nil || n != 2 {
		t.Fatalf("deleted %d: %v", n, err)
	}
	var left int
	_ = s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM log_streams`).Scan(&left)
	envDocs, _ := idx.Search(ctx, "ferun-logs-*", quickwitQuery("acme", "e"))
	if left != 1 || len(envDocs) != 0 {
		t.Errorf("after delete: %d streams, %d documents", left, len(envDocs))
	}
	all := url.Values{"environment": {"e"}, "since": {fmt.Sprint(base - int64(200*time.Hour))}}
	if r := mustRead(t, s, "acme", all); len(r.lines) != 0 {
		t.Errorf("after delete: %v", texts(r, false))
	}
	idx.sync(lg)
	if r := mustRead(t, s, "acme", all); len(r.lines) != 0 {
		t.Errorf("after delete and a commit: %v", texts(r, false))
	}
	if r := mustRead(t, s, "acme", url.Values{"job": {"j1"}}); len(r.lines) != 2 {
		t.Errorf("the job's: %v", texts(r, false))
	}
}

// followRecorder collects a follow's output.
type followRecorder struct {
	mu      sync.Mutex
	lines   []string
	streams int
	beats   int
	skipped int
}

func (f *followRecorder) Line(l Line, st *streamRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, l.Text)
	return nil
}

func (f *followRecorder) Skipped(n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.skipped += n
	return nil
}

func (f *followRecorder) Heartbeat() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats++
	return nil
}

func (f *followRecorder) snapshot() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...), f.beats
}

// follow runs a follow of v in the background; stop ends it and returns
// its error.
func follow(t *testing.T, s *Store, v url.Values) (rec *followRecorder, stop func() error) {
	t.Helper()
	q, err := parseQuery(v, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rec = &followRecorder{}
	done := make(chan error, 1)
	go func() { done <- s.Follow(ctx, "acme", q, rec) }()
	t.Cleanup(cancel)
	return rec, func() error {
		cancel()
		return <-done
	}
}

func TestFollow(t *testing.T) {
	cfg := testConfig()
	cfg.Heartbeat = 50 * time.Millisecond
	s, idx, lg := newTestStore(t, cfg)
	ctx := context.Background()
	key := StreamKey{"p", "c", "1"}
	text := func(prefix string) func(int) string {
		return func(i int) string { return fmt.Sprintf("%s%d", prefix, i) }
	}
	if err := s.Ingest(ctx, batch(key, envLabels("acme", "e", 0), lines(0, base, 3, text("old")))); err != nil {
		t.Fatal(err)
	}
	idx.sync(lg)
	rec, stop := follow(t, s, url.Values{"environment": {"e"}, "kind": {"service"}, "object": {"api"}, "follow": {"1"}, "q": {"-skip"}})
	waitFor(t, func() bool { l, _ := rec.snapshot(); return len(l) == 3 })
	// New lines, a commit of the index meanwhile, the new lines in the
	// topic again (an answer lost), a stream of the object that starts now
	// (another replica, a job's stream of it), a line the filter drops,
	// another object's line and another environment's.
	if err := s.Ingest(ctx, batch(key, envLabels("acme", "e", 0), lines(100, base+int64(time.Second), 3, text("new")))); err != nil {
		t.Fatal(err)
	}
	idx.sync(lg)
	produceAgain(t, lg, logTopic, streamSID(t, s, key))
	if err := s.Ingest(ctx, batch(key, envLabels("acme", "e", 0), lines(200, base+int64(2*time.Second), 2, text("hot")))); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, batch(StreamKey{"p2", "c", "1"}, envLabels("acme", "e", 1), append(
		lines(0, base+int64(3*time.Second), 1, text("second")), lines(10, base+int64(4*time.Second), 1, text("skip"))...))); err != nil {
		t.Fatal(err)
	}
	jl := envLabels("acme", "e", 0)
	jl.Job = "build-1"
	if err := s.Ingest(ctx, batch(StreamKey{"j", "c", "1"}, jl, lines(0, base+int64(4500*time.Millisecond), 1, text("job")))); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, batch(StreamKey{"x", "c", "1"}, envLabels("acme", "x", 0), lines(0, base+int64(4*time.Second), 1, text("x")))); err != nil {
		t.Fatal(err)
	}
	web := envLabels("acme", "e", 0)
	web.Object = "web"
	if err := s.Ingest(ctx, batch(StreamKey{"w", "c", "1"}, web, lines(0, base+int64(4*time.Second), 1, text("web")))); err != nil {
		t.Fatal(err)
	}
	// A line in two parts, in two batches: given once, joined.
	if err := s.Ingest(ctx, batch(key, envLabels("acme", "e", 0), []Line{
		{Off: 300, TS: base + int64(5*time.Second), Source: "stdout", Level: LevelUnknown, Partial: true, Text: "part1 "}}),
	); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := s.Ingest(ctx, batch(key, envLabels("acme", "e", 0), []Line{
		{Off: 310, TS: base + int64(6*time.Second), Source: "stdout", Level: LevelUnknown, Text: "part2"}}),
	); err != nil {
		t.Fatal(err)
	}
	want := "old0|old1|old2|new0|new1|new2|hot0|hot1|second0|job0|part1 part2"
	waitFor(t, func() bool { l, _ := rec.snapshot(); return strings.Join(l, "|") == want })
	waitFor(t, func() bool { _, b := rec.snapshot(); return b > 0 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if l, _ := rec.snapshot(); strings.Join(l, "|") != want {
		t.Errorf("follow gave %v", l)
	}
}

// A follow of a tenant's job or of an object before its stream starts
// gives the stream's lines; one of a stream gives only that stream's; an
// environment as a whole is not followed.
func TestFollowBeforeTheStream(t *testing.T) {
	cfg := testConfig()
	cfg.Heartbeat = 20 * time.Millisecond
	s, _, _ := newTestStore(t, cfg)
	ctx := context.Background()
	jl := Labels{Tenant: "acme", Job: "build-1", Attrs: map[string]string{}}
	other := Labels{Tenant: "acme", Job: "build-2", Attrs: map[string]string{}}
	byJob, stopJob := follow(t, s, url.Values{"job": {"build-1"}, "follow": {"1"}})
	byObject, stopObject := follow(t, s, url.Values{"environment": {"f"}, "kind": {"service"}, "object": {"api"}, "follow": {"1"}})
	// A follow beats once it reads its partitions: wait for that.
	for _, rec := range []*followRecorder{byJob, byObject} {
		waitFor(t, func() bool { _, b := rec.snapshot(); return b > 0 })
	}
	for i, l := range []Labels{other, jl, envLabels("acme", "f", 0)} {
		name := l.Job
		if name == "" {
			name = "api"
		}
		if err := s.Ingest(ctx, batch(StreamKey{name, "c", "1"}, l, lines(0, base+int64(i), 1, func(int) string { return name }))); err != nil {
			t.Fatal(err)
		}
	}
	sid := streamSID(t, s, StreamKey{"build-1", "c", "1"})
	byStream, stopStream := follow(t, s, url.Values{"stream": {sid}, "follow": {"1"}})
	waitFor(t, func() bool { l, _ := byStream.snapshot(); return len(l) == 1 })
	if err := s.Ingest(ctx, batch(StreamKey{"build-2", "c", "1"}, other, lines(10, base+10, 1, func(int) string { return "2 again" }))); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, batch(StreamKey{"build-1", "c", "1"}, jl, lines(10, base+10, 1, func(int) string { return "1 again" }))); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []*followRecorder{byJob, byStream} {
		waitFor(t, func() bool { l, _ := rec.snapshot(); return strings.Join(l, "|") == "build-1|1 again" || len(l) > 2 })
	}
	for name, rec := range map[string]*followRecorder{"job": byJob, "stream": byStream} {
		if l, _ := rec.snapshot(); strings.Join(l, "|") != "build-1|1 again" {
			t.Errorf("a follow of the %s gave %v", name, l)
		}
	}
	waitFor(t, func() bool { l, _ := byObject.snapshot(); return len(l) == 1 })
	if l, _ := byObject.snapshot(); l[0] != "api" {
		t.Errorf("a follow of the object gave %v", l)
	}
	for _, stop := range []func() error{stopJob, stopObject, stopStream} {
		if err := stop(); err != nil {
			t.Fatal(err)
		}
	}
	q, _ := parseQuery(url.Values{"environment": {"f"}, "follow": {"1"}}, time.Now())
	if err := s.Follow(ctx, "acme", q, &followRecorder{}); !errors.Is(err, ErrFollowScope) {
		t.Errorf("a follow of an environment: %v", err)
	}
}

// A follow over its rate skips lines, error lines last, and says how many.
func TestFollowRate(t *testing.T) {
	cfg := testConfig()
	cfg.Heartbeat = 20 * time.Millisecond
	s, _, _ := newTestStore(t, cfg)
	ctx := context.Background()
	rec, stop := follow(t, s, url.Values{"environment": {"e"}, "kind": {"service"}, "object": {"api"}, "follow": {"1"}})
	waitFor(t, func() bool { _, b := rec.snapshot(); return b > 0 })
	n := followBurst + 300
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "1"}, envLabels("acme", "e", 0), lines(0, base, n, func(i int) string {
		if i%2 == 0 {
			return fmt.Sprintf("ERROR %d", i)
		}
		return fmt.Sprintf("INFO %d", i)
	}))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { rec.mu.Lock(); defer rec.mu.Unlock(); return rec.skipped > 0 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	given, errs := len(rec.lines), 0
	for _, l := range rec.lines {
		if strings.HasPrefix(l, "ERROR") {
			errs++
		}
	}
	if given+rec.skipped != n || given > followBurst+followErrBurst+followRate+followErrRate {
		t.Errorf("given %d, skipped %d, of %d", given, rec.skipped, n)
	}
	if errs <= given/2 {
		t.Errorf("%d error lines among %d given: errors not first", errs, given)
	}
}

func TestFollowKey(t *testing.T) {
	s, _, _ := newTestStore(t, testConfig())
	ctx := context.Background()
	if err := s.Ingest(ctx, batch(StreamKey{"p", "c", "1"}, envLabels("acme", "e", 0), lines(0, base, 1, func(int) string { return "x" }))); err != nil {
		t.Fatal(err)
	}
	inst := envLabels("acme", "e", 0).Instance
	for _, c := range []struct {
		sel  Selector
		want string
	}{
		{Selector{Environment: "e", Kind: "service", Object: "api"}, "acme/e/service/api"},
		{Selector{Environment: "e", Kind: "service", Object: "api", Replica: new(int)}, "acme/e/service/api"},
		{Selector{Job: "j"}, "acme//j"},
		{Selector{Instance: inst}, "acme/e/service/api"},
		{Selector{Environment: "e"}, ""},
		{Selector{Environment: "e", Object: "api"}, ""},
		{Selector{Instance: "nope"}, ""},
	} {
		got, err := s.FollowKey(ctx, "acme", c.sel)
		if c.want == "" {
			if !errors.Is(err, ErrFollowScope) {
				t.Errorf("%+v: %q, %v; want ErrFollowScope", c.sel, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%+v: %q, %v; want %q", c.sel, got, err, c.want)
		}
	}
}

func TestFollowLimit(t *testing.T) {
	now := time.Now()
	f := newFollowLimit(now)
	info, errl := Line{Level: LevelInfo}, Line{Level: LevelError}
	n := 0
	for range followBurst + 100 {
		if f.admit(&info, now) {
			n++
		}
	}
	if n != followBurst {
		t.Errorf("a burst of %d info lines took %d", followBurst+100, n)
	}
	// Errors have their own bucket once the lines' is empty.
	e := 0
	for range followErrBurst + 10 {
		if f.admit(&errl, now) {
			e++
		}
	}
	if e != followErrBurst {
		t.Errorf("%d error lines over the burst taken, want %d", e, followErrBurst)
	}
	if got := f.skippedDue(now); got != 0 {
		t.Errorf("said %d before a second", got)
	}
	if got := f.skippedDue(now.Add(time.Second)); got != 110 {
		t.Errorf("skipped %d, want 110", got)
	}
	// A second later, the rate's lines go again.
	later := now.Add(2 * time.Second)
	if !f.admit(&info, later) {
		t.Error("no line after the bucket refilled")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
