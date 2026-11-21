// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// Log is the log of record (ADR 0026 §11): the topics on Kafka
// (kafka.Client), or a test's.
type Log interface {
	EnsureTopic(ctx context.Context, topic string, tc kafka.TopicConfig) error
	Partitions(ctx context.Context, topic string) (int32, error)
	Produce(ctx context.Context, topic string, recs []kafka.Record) error
	Ends(ctx context.Context, topic string) (map[int32]int64, error)
	Read(ctx context.Context, topic string, from, to map[int32]int64, max int) ([]kafka.Record, error)
	Follow(topic string, from map[int32]int64) (Follower, error)
}

// Follower reads partitions as their records come (kafka.Follower);
// Add adds partitions, each from its offset.
type Follower interface {
	Next(ctx context.Context) ([]kafka.Record, error)
	Add(from map[int32]int64)
	Close()
}

// kafkaLog is kafka.Client as a Log.
type kafkaLog struct{ *kafka.Client }

func (k kafkaLog) Follow(topic string, from map[int32]int64) (Follower, error) {
	return k.Client.Follow(topic, from)
}

// SourceIndex is what the pipes need of the index: an index fed by a
// Kafka source, and what the source has published of each partition.
type SourceIndex interface {
	// Ensure makes the index of config unless it exists, and its Kafka
	// source on src's topic unless it has one.
	Ensure(ctx context.Context, id string, config []byte, src quickwit.Source) error
	// Checkpoint is, for each partition of the index's source, the
	// offset of the first record it has not published (0: none yet).
	Checkpoint(ctx context.Context, id string) (map[int32]int64, error)
}

// pipes are the sink's topics and the indexes their sources fill, one
// topic per index and of its name, for logs and traces alike: made on
// first use, appended to, and read back where the index has not caught
// up (ADR 0026 §11).
type pipes struct {
	log   Log
	index SourceIndex
	cfg   Config

	mu    sync.Mutex
	parts map[string]int32 // the ensured topics' partitions
}

func newPipes(log Log, index SourceIndex, cfg Config) *pipes {
	return &pipes{log: log, index: index, cfg: cfg, parts: map[string]int32{}}
}

// ensure makes id's topic, its index of config and the index's source, and
// returns the topic's partitions. Only the sink's start makes them
// (Program.ensurePipes): the topics are the platform's, fixed, never made
// by what a project does (ADR 0026 §11).
func (p *pipes) ensure(ctx context.Context, id string, config func() []byte) (int32, error) {
	p.mu.Lock()
	n, ok := p.parts[id]
	p.mu.Unlock()
	if ok {
		return n, nil
	}
	tc := kafka.TopicConfig{Partitions: int32(p.cfg.KafkaPartitions), Retention: p.cfg.KafkaRetention,
		Replication: int16(p.cfg.KafkaReplication)}
	if err := p.log.EnsureTopic(ctx, id, tc); err != nil {
		return 0, err
	}
	n, err := p.log.Partitions(ctx, id)
	if err != nil {
		return 0, err
	}
	src := quickwit.Source{Topic: id, Brokers: p.cfg.KafkaBrokers}
	if err := p.index.Ensure(ctx, id, config(), src); err != nil {
		return 0, fmt.Errorf("index %s: %w", id, err)
	}
	p.mu.Lock()
	p.parts[id] = n
	p.mu.Unlock()
	return n, nil
}

// produce appends recs to id's topic; it returns once Kafka has them
// on every in-sync replica.
func (p *pipes) produce(ctx context.Context, id string, recs []kafka.Record) error {
	if len(recs) == 0 {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return p.log.Produce(pctx, id, recs)
}

// tailBound is how many records one read of a tail takes at most.
const tailBound = 200000

// tail reads the records of id's topic in the partitions parts that its
// index has not published: from the source's checkpoint to the topic's
// end. The checkpoint is read before the end, so that a record is in the
// index or in the tail, or in both while a split is published meanwhile.
func (p *pipes) tail(ctx context.Context, id string, parts []int32) ([]kafka.Record, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	cp, err := p.index.Checkpoint(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("index %s: checkpoint: %w", id, err)
	}
	ends, err := p.log.Ends(ctx, id)
	if err != nil {
		return nil, err
	}
	from, to := map[int32]int64{}, map[int32]int64{}
	for _, part := range slices.Compact(slices.Sorted(slices.Values(parts))) {
		from[part], to[part] = cp[part], ends[part]
	}
	return p.log.Read(ctx, id, from, to, tailBound)
}

// ends are the offsets after the last record of each of the partitions
// parts of id's topic.
func (p *pipes) ends(ctx context.Context, id string, parts []int32) (map[int32]int64, error) {
	all, err := p.log.Ends(ctx, id)
	if err != nil {
		return nil, err
	}
	out := map[int32]int64{}
	for _, part := range parts {
		out[part] = all[part]
	}
	return out, nil
}

// open is how many partitions id's topic has: ensured, or asked of the
// log; a topic that is not there fails, it is never made here.
func (p *pipes) open(ctx context.Context, id string) (int32, error) {
	p.mu.Lock()
	n, ok := p.parts[id]
	p.mu.Unlock()
	if ok {
		return n, nil
	}
	n, err := p.log.Partitions(ctx, id)
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	p.parts[id] = n
	p.mu.Unlock()
	return n, nil
}
