// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package quickwit is the sink's client of Quickwit's REST API (ADR 0026
// §10): the log indexes, ingest that answers once a batch is published,
// search and delete tasks. Only the standard library: JSON over HTTP.
package quickwit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Doc is a line as an index holds it: its time (Unix ns), its stream's
// labels, and the line.
type Doc struct {
	TS          int64  `json:"ts"`
	Tenant      string `json:"tenant"`
	Stream      string `json:"stream"`
	Environment string `json:"environment,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Object      string `json:"object,omitempty"`
	Replica     int    `json:"replica"`
	// Deployment is the stream's ferun.deployment. Its field keeps the
	// name release, the label's earlier word (ADR 0018, "The
	// deployments' word"): a field of its own is a new mapping, so a new
	// index and topic of every retention (MappingVersion), for a field no
	// search filters by; it takes the new name with the next version.
	Deployment int    `json:"release"`
	Instance   string `json:"instance,omitempty"`
	Container  string `json:"container,omitempty"`
	Job        string `json:"job,omitempty"`
	Attempt    int    `json:"attempt"`
	Source     string `json:"source"`
	IOStream   string `json:"iostream,omitempty"`
	Level      int    `json:"level"`
	TraceID    string `json:"trace_id,omitempty"`
	Off        int64  `json:"off"`
	Partial    bool   `json:"partial"`
	Cont       bool   `json:"cont"`
	Text       string `json:"text"`
}

// Query selects a tenant's lines: the labels set (empty or nil: any), a
// time range in Unix ns (both ends included; 0: open), offsets after
// OffAfter up to OffTo (nil: any), levels (a mask; 0: any), a trace ID,
// words each in the text as a phrase, words none of which may be there,
// at most Max hits in the order Order. With OrPartial a partial line
// passes the words: they may be in its rest.
type Query struct {
	Tenant                                                      string
	Environment, Kind, Object, Instance, Container, Job, Stream string
	Source, IOStream                                            string
	Replica, Attempt                                            *int
	SinceNs, UntilNs                                            int64
	OffAfter, OffTo                                             *int64
	Levels                                                      int
	Trace                                                       string
	Words, NotWords                                             []string
	OrPartial                                                   bool
	Order                                                       Order
	Max                                                         int
}

// Order is a search's order of hits.
type Order int

const (
	// NewestFirst is by time, then offset, descending.
	NewestFirst Order = iota
	// OldestFirst is by time, then offset, ascending.
	OldestFirst
	// ByOffset is by offset ascending, for one stream's lines.
	ByOffset
)

// MaxHits is the most hits one search answers (Quickwit's bound).
const MaxHits = 10000

// levelBits are the level bits a mask may hold (the sink's LevelBits).
var levelBits = []int{1, 2, 4, 8, 16}

// String is the query in Quickwit's query language.
func (q Query) String() string {
	var c []string
	term := func(field, v string) {
		if v != "" {
			c = append(c, field+":"+quote(v))
		}
	}
	term("tenant", q.Tenant)
	term("environment", q.Environment)
	term("kind", q.Kind)
	term("object", q.Object)
	term("instance", q.Instance)
	term("container", q.Container)
	term("job", q.Job)
	term("stream", q.Stream)
	term("source", q.Source)
	term("iostream", q.IOStream)
	term("trace_id", q.Trace)
	if q.Replica != nil {
		c = append(c, "replica:"+strconv.Itoa(*q.Replica))
	}
	if q.Attempt != nil {
		c = append(c, "attempt:"+strconv.Itoa(*q.Attempt))
	}
	if q.SinceNs > 0 || q.UntilNs > 0 {
		lo, hi := "*", "*"
		if q.SinceNs > 0 {
			lo = rfc3339(q.SinceNs)
		}
		if q.UntilNs > 0 {
			hi = rfc3339(q.UntilNs)
		}
		c = append(c, "ts:["+lo+" TO "+hi+"]")
	}
	if q.OffAfter != nil || q.OffTo != nil {
		lo, hi, open := "*", "*", "["
		if q.OffAfter != nil {
			lo, open = strconv.FormatInt(*q.OffAfter, 10), "{"
		}
		if q.OffTo != nil {
			hi = strconv.FormatInt(*q.OffTo, 10)
		}
		c = append(c, "off:"+open+lo+" TO "+hi+"]")
	}
	if q.Levels != 0 {
		var in []string
		for _, b := range levelBits {
			if q.Levels&b != 0 {
				in = append(in, strconv.Itoa(b))
			}
		}
		if len(in) < len(levelBits) {
			c = append(c, "level:IN ["+strings.Join(in, " ")+"]")
		}
	}
	var words []string
	for _, w := range q.Words {
		words = append(words, "text:"+quote(w))
	}
	for _, w := range q.NotWords {
		words = append(words, "NOT text:"+quote(w))
	}
	switch {
	case len(words) > 0 && q.OrPartial:
		c = append(c, "(("+strings.Join(words, " AND ")+") OR partial:true)")
	case len(words) > 0:
		c = append(c, words...)
	}
	if len(c) == 0 {
		return "*"
	}
	return strings.Join(c, " AND ")
}

// quote is a phrase of the query language: s in double quotes, a quote
// or backslash in it escaped.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func rfc3339(ns int64) string {
	return time.Unix(0, ns).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// Client calls Quickwit's REST API (any node answers it; the searchers'
// Service in the reference deployment).
type Client struct {
	search string
	http   *http.Client
}

// New returns a client of Quickwit at searchURL (http://host:7280).
func New(searchURL string, hc *http.Client) (*Client, error) {
	p, err := url.Parse(searchURL)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return nil, fmt.Errorf("quickwit: %q is not an http(s) URL", searchURL)
	}
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{search: strings.TrimRight(searchURL, "/"), http: hc}, nil
}

// Error is an answer of Quickwit that is not a success.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("quickwit: %d %s", e.Status, e.Message) }

// IsNotFound reports whether err is Quickwit's 404.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, url, ctype string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("quickwit: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return fmt.Errorf("quickwit: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &m)
		if m.Message == "" {
			m.Message = strings.TrimSpace(string(data[:min(len(data), 512)]))
		}
		return &Error{Status: resp.StatusCode, Message: m.Message}
	}
	if out == nil {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return d.Decode(out)
}

// MappingVersion is the version of IndexConfig, in every log index's
// name (IndexID). Quickwit never changes an index's partition key or time
// field, and a changed mapping only applies to new splits, so a change of
// IndexConfig is a new version: new lines go to the new version's
// indexes, a read searches <prefix>-* and so the old ones too, which
// their retention empties. Version 1 is the indexes named <prefix>-<days>d
// (tenant the only tag, no partitions), version 2 partitions by tenant,
// environment and job, version 3 is the same mapping filled by a Kafka
// source (ADR 0026 §11): an index has its source from its start; version
// 4 the same on Apache Kafka in place of Redpanda (a source's brokers do
// not change), version 5 on topics of 64 partitions keyed by object (a
// topic's partitions are its from its start).
const MappingVersion = 5

// MaxPartitions is the most partitions an index may have: Quickwit's
// max_num_partitions (200 by default), past which it puts every line into
// one partition more.
const MaxPartitions = 200

// IndexID is the log index of a retention in days, in prefix, of
// MappingVersion: <prefix>-v<version>-<days>d.
func IndexID(prefix string, days int) string {
	return prefix + "-v" + strconv.Itoa(MappingVersion) + "-" + strconv.Itoa(days) + "d"
}

// IndexSettings are what an index is made with, and never change for it:
// its lines are kept RetentionDays, committed every CommitSecs, and
// hashed by their tenant, environment and job into Partitions partitions
// (1 to MaxPartitions), a split holding one partition's lines.
type IndexSettings struct {
	RetentionDays, CommitSecs, Partitions int
}

// partitionKey is the partition key of an index of n partitions. A
// search for an environment or a job prunes the splits whose tags lack it
// (tag_fields), so a split should hold few of them.
func partitionKey(n int) string {
	return "hash_mod((tenant,environment,job), " + strconv.Itoa(n) + ")"
}

// IndexConfig is the configuration of the log index id.
func IndexConfig(id string, s IndexSettings) []byte {
	raw := func(name string) map[string]any {
		return map[string]any{"name": name, "type": "text", "tokenizer": "raw"}
	}
	num := func(name string, fast bool) map[string]any {
		return map[string]any{"name": name, "type": "u64", "fast": fast}
	}
	cfg := map[string]any{
		"version":  "0.9",
		"index_id": id,
		"doc_mapping": map[string]any{
			"mode": "strict",
			"field_mappings": []map[string]any{
				{"name": "ts", "type": "datetime", "input_formats": []string{"unix_timestamp"},
					"output_format": "unix_timestamp_nanos", "fast": true, "fast_precision": "nanoseconds"},
				raw("tenant"), raw("stream"), raw("environment"), raw("kind"), raw("object"),
				num("replica", false), num("release", false), raw("instance"), raw("container"), raw("job"),
				num("attempt", false), raw("source"), raw("iostream"), num("level", true), raw("trace_id"),
				num("off", true),
				{"name": "partial", "type": "bool"},
				{"name": "cont", "type": "bool"},
				{"name": "text", "type": "text", "tokenizer": "default", "record": "position"},
			},
			"timestamp_field": "ts",
			// Quickwit records a split's values of a tag field while
			// they are fewer than 1,000.
			"tag_fields":    []string{"tenant", "environment", "job"},
			"partition_key": partitionKey(s.Partitions),
		},
		"indexing_settings": map[string]any{"commit_timeout_secs": s.CommitSecs},
		"search_settings":   map[string]any{"default_search_fields": []string{"text"}},
		"retention":         map[string]any{"period": fmt.Sprintf("%d days", s.RetentionDays), "schedule": "hourly"},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// Source is the Kafka topic an index's source reads, at the brokers.
type Source struct {
	Topic   string
	Brokers []string
}

// sourceID is the one source of a sink's index.
const sourceID = "kafka"

// Ensure makes the index id of config unless it exists, and its Kafka
// source on src's topic unless it has one (ADR 0026 §11): Quickwit keeps
// the topic's offsets in its metastore with each split it publishes, so a
// record is indexed once.
func (c *Client) Ensure(ctx context.Context, id string, config []byte, src Source) error {
	err := c.do(ctx, http.MethodGet, c.search+"/api/v1/indexes/"+url.PathEscape(id), "", nil, nil)
	if IsNotFound(err) {
		err = c.do(ctx, http.MethodPost, c.search+"/api/v1/indexes", "application/json", config, nil)
		if alreadyExists(err) {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"version": "0.9", "source_id": sourceID, "source_type": "kafka", "num_pipelines": 1, "input_format": "json",
		"params": map[string]any{
			"topic": src.Topic, "client_log_level": "warn", "enable_backfill_mode": false,
			// earliest: a partition without a checkpoint is read from its
			// start, not its end; a short session, so that a restarted
			// indexer takes its partitions again within seconds.
			"client_params": map[string]string{"bootstrap.servers": strings.Join(src.Brokers, ","),
				"auto.offset.reset": "earliest", "session.timeout.ms": "10000", "heartbeat.interval.ms": "3000"},
		},
	})
	err = c.do(ctx, http.MethodPost, c.search+"/api/v1/indexes/"+url.PathEscape(id)+"/sources", "application/json", body, nil)
	if alreadyExists(err) {
		return nil
	}
	return err
}

func alreadyExists(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == http.StatusBadRequest || e.Status == http.StatusConflict) &&
		strings.Contains(e.Message, "already exist")
}

// Checkpoint is, for each partition of the index's Kafka source, the
// offset of the first record it has not published: Quickwit keeps the
// offset of the last one it published, partitions and positions as
// 20 digits; a partition it has published nothing of is not there.
func (c *Client) Checkpoint(ctx context.Context, id string) (map[int32]int64, error) {
	var md struct {
		Checkpoint map[string]map[string]string `json:"checkpoint"`
	}
	if err := c.do(ctx, http.MethodGet, c.search+"/api/v1/indexes/"+url.PathEscape(id), "", nil, &md); err != nil {
		return nil, err
	}
	out := map[int32]int64{}
	for part, position := range md.Checkpoint[sourceID] {
		p, err := strconv.ParseInt(part, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("quickwit: checkpoint of %s: partition %q", id, part)
		}
		if position == "" {
			continue // nothing published yet: from the start
		}
		last, err := strconv.ParseInt(position, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("quickwit: checkpoint of %s: position %q", id, position)
		}
		out[int32(p)] = last + 1
	}
	return out, nil
}

// Indexes lists the ids of the indexes that start with prefix.
func (c *Client) Indexes(ctx context.Context, prefix string) ([]string, error) {
	var out []struct {
		IndexConfig struct {
			IndexID string `json:"index_id"`
		} `json:"index_config"`
	}
	if err := c.do(ctx, http.MethodGet, c.search+"/api/v1/indexes", "", nil, &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range out {
		if strings.HasPrefix(m.IndexConfig.IndexID, prefix) {
			ids = append(ids, m.IndexConfig.IndexID)
		}
	}
	return ids, nil
}

// Search answers the hits of q in the indexes pattern names
// (ferun-logs-*), at most q.Max (bounded by MaxHits).
func (c *Client) Search(ctx context.Context, pattern string, q Query) ([]Doc, error) {
	req := map[string]any{"query": q.String(), "max_hits": min(max(q.Max, 1), MaxHits), "count_all": false,
		"format": "json"}
	switch q.Order {
	case NewestFirst:
		req["sort_by"] = "ts,off" // Quickwit: a field alone is descending
	case OldestFirst:
		req["sort_by"] = "-ts,-off" // and with a minus ascending
	case ByOffset:
		req["sort_by"] = "-off"
	}
	if q.SinceNs > 0 {
		req["start_timestamp"] = q.SinceNs / 1e9
	}
	if q.UntilNs > 0 {
		req["end_timestamp"] = q.UntilNs/1e9 + 1
	}
	body, _ := json.Marshal(req)
	var out struct {
		Hits   []Doc    `json:"hits"`
		Errors []string `json:"errors"`
	}
	err := c.do(ctx, http.MethodPost, c.search+"/api/v1/"+pattern+"/search", "application/json", body, &out)
	if err != nil {
		return nil, err
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("quickwit: search: %s", strings.Join(out.Errors, "; "))
	}
	return out.Hits, nil
}

// Delete leaves a delete task of q's labels in the index id; Quickwit
// removes the documents when it rewrites their splits.
func (c *Client) Delete(ctx context.Context, id string, q Query) error {
	body, _ := json.Marshal(map[string]any{"query": q.String()})
	return c.do(ctx, http.MethodPost, c.search+"/api/v1/"+url.PathEscape(id)+"/delete-tasks", "application/json", body, nil)
}
