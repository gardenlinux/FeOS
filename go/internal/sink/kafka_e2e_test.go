// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// TestKafkaEndToEnd runs the store against a real Kafka API
// (FERUN_TEST_KAFKA_BROKERS) and Quickwit
// (FERUN_TEST_QUICKWIT_URL), which must reach the brokers at the same
// address: lines read from the topic's tail at once, from the index after
// its source's commit, each once, and followed as they come.
func TestKafkaEndToEnd(t *testing.T) {
	qw, brokers := os.Getenv("FERUN_TEST_QUICKWIT_URL"), os.Getenv("FERUN_TEST_KAFKA_BROKERS")
	if qw == "" || brokers == "" {
		t.Skip("FERUN_TEST_QUICKWIT_URL and FERUN_TEST_KAFKA_BROKERS are not set; skipping a test that needs Quickwit and Kafka")
	}
	cfg := testConfig()
	cfg.IndexPrefix = fmt.Sprintf("e2e-logs-%d", time.Now().UnixNano())
	cfg.KafkaBrokers = strings.Split(brokers, ",")
	cfg.KafkaReplication, cfg.KafkaPartitions = 1, 3
	cfg.CommitTimeout = 2 * time.Second
	cfg.RetentionMax = cfg.Retention
	cfg.Heartbeat = 200 * time.Millisecond
	qc, err := quickwit.New(qw, nil)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := kafka.New(cfg.KafkaBrokers, kafka.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	defer kc.Close()
	s := NewStore(newTestDB(t), qc, kafkaLog{kc}, cfg, dbtest.Logger(t))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	key, labels := StreamKey{"pod", "main", "L1"}, envLabels("acme", "env1", 0)
	text := func(i int) string { return fmt.Sprintf("INFO line %02d", i) }
	if err := s.Ingest(ctx, batch(key, labels, lines(0, base, 20, text))); err != nil {
		t.Fatal(err)
	}
	// Sent again: nothing new.
	if err := s.Ingest(ctx, batch(key, labels, lines(0, base, 20, text))); err != nil {
		t.Fatal(err)
	}
	all := url.Values{"environment": {"env1"}, "limit": {"100"}, "since": {fmt.Sprint(base - 1)}}
	if r := mustRead(t, s, "acme", all); len(r.lines) != 20 {
		t.Fatalf("from the tail: %d lines", len(r.lines))
	}
	id := s.indexID(durationDays(cfg.Retention))
	deadline := time.Now().Add(time.Minute)
	for {
		cp, err := qc.Checkpoint(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		n := int64(0)
		for _, o := range cp {
			n += o
		}
		if n >= 20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the source published %d of 20", n)
		}
		time.Sleep(500 * time.Millisecond)
	}
	r := mustRead(t, s, "acme", all)
	if len(r.lines) != 20 {
		t.Fatalf("from the index: %d lines", len(r.lines))
	}
	if r := mustRead(t, s, "acme", url.Values{"environment": {"env1"}, "q": {"line 07"}, "since": {fmt.Sprint(base - 1)}}); len(r.lines) != 1 {
		t.Errorf("a phrase: %d lines", len(r.lines))
	}
	// A follow gives the page, then what comes.
	q, err := parseQuery(url.Values{"environment": {"env1"}, "kind": {"service"}, "object": {"api"}, "follow": {"1"}, "since": {fmt.Sprint(base - 1)}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rec := &followRecorder{}
	fctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Follow(fctx, "acme", q, rec) }()
	waitFor(t, func() bool { l, _ := rec.snapshot(); return len(l) == 20 })
	if err := s.Ingest(ctx, batch(key, labels, lines(1000, base+int64(time.Second), 3, func(i int) string { return fmt.Sprintf("new %d", i) }))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { l, _ := rec.snapshot(); return len(l) == 23 })
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if l, _ := rec.snapshot(); l[20] != "new 0" || l[22] != "new 2" {
		t.Errorf("follow gave %v", l[19:])
	}
}
