# ADR 0026: The sink keeps what the platform and its pods say

**Status:** Accepted · **Date:** 2026-10-30

What is said rather than decided, the logs and traces of pods and the
ingress's requests, goes to `ferun-sink`, a program of its own beside
`ferund` with a database of its own, which `ferund` neither carries nor
waits for. A stream is one container's output with labels `ferund` sets
at the pod's create and the pod cannot change; the guest writes CRI lines
and keeps them, the node follows them and ships them over OTLP with its
node certificate, at least once on the wire and once in the store. The
sink is the gate in front of Kafka, the log of record, and Quickwit, the
index and store of every word, label, level and trace ID; the database
keeps streams, positions and quota, never a line. A tenant reads its own
streams and traces with its token or session, within a retention and a
quota per tenant; the ingress's access log is a stream and the server
spans of the service it served.

## Context

A container's output lives in one file in its guest, stdout and stderr
together and without a time per line. `ferund` reads a tail of it
through the node, and a job's run keeps its last lines on its row. The
file goes with its pod: the log of an instance a deployment replaced, of
one that crashed and was recreated, of a paused pod or of a job's run
beyond its kept end cannot be read. Nothing searches a service's
replicas together, or a week back. Tenants work around it: a builder
keeps its own output and uploads it to a bucket as it exits; the
ingress's access log would have to live in the control plane's database
for want of another place, and nowhere could the rest of a trace a pod
makes go.

The platform is rolled out to a large organisation's teams, so the
volume is a company's, and traces are wanted for certain. The labels a
stream can have are few and FeRun knows them all; the object store
exists; recent lines want a durable shared place that several instances
can write and read. A log must not burden the control plane: it is a
write path of its own with a load of its own, and the control plane
should neither carry it nor fail with it.

The standard log stores split the same way. Elasticsearch and OpenSearch
index every word at a high cost of ingest and storage. Loki indexes a
stream's labels and time only and scans compressed chunks for a word,
which answers a selector and a time well and a search over weeks poorly;
a trace store of its own would be a second program of that kind, with
columns, filters per block and merges, which is what Tempo is. Quickwit
is both, for logs and traces: an index of every word in immutable splits
in the object store, its metadata in PostgreSQL, merges and retention of
its own, and a Kafka source that keeps a topic's offsets in its metastore
in the same transaction that publishes a split, so each record is indexed
exactly once.

## Decision

### A program of its own: `ferun-sink`

- `ferun-sink` is a program of this repository (ADR 0003), beside
  `ferund` and `ferun-ingress`, with its own database. `ferund` neither
  carries it nor waits for it: an environment runs, deploys and reads its
  status while the sink is down, and a node keeps what it could not send.
  It is where the platform puts what is said rather than decided: logs
  and traces, each a kind of record sharing ingest, the gate and the
  tenants' reads. The reference deployment runs two or more instances,
  which share nothing but the database, Kafka and Quickwit.
- The sink holds an identity of the platform CA, obtained from `ferund`
  with a service token, and asks `ferund` whose token or session a reader
  brings.

### A stream is one container's output, labelled by FeRun

- A **stream** is the output of one container of one pod: an image
  service's or an addon's instance, a job's run, a tenant's job, a
  sandbox's container. Its labels are fixed and set by `ferund` with the
  pod's create, never by the pod: the tenant always; the environment,
  kind, object, replica and deployment of an environment's pods; the job
  and attempt of a tenant's job; the pod's sandbox ID; and the source,
  stdout or stderr, per line. The node stamps the stream with them, and
  a restore brings them again.
- The labels are bounded by the pods FeRun makes, so streams stay few:
  one per instance and run, not per request or user. A pod adds no labels
  of its own; what it wants to find is in its lines. A stream has an end
  once its pod is gone, and its labels and time span stay: the record of
  an instance that is no longer there.

### The guest keeps, the node ships

- The guest writes each line with its time and stream, in the CRI log
  format Kubernetes uses, and the log file carries an ID, so that offsets
  of a log made anew start again under a new ID. Tails and the present
  logs calls show the text as before.
- `feosd` follows the log of every running container on its node from
  the offset the sink last stored for the stream, and exports batches to
  the sink over OTLP/gRPC, a resource per stream with its labels and a
  record per line with its offset. OTLP is the standard protocol, so a
  collector or a pod can speak it too; its `.proto` files are generated
  with the protobuf this repository has, no OpenTelemetry SDK. The node
  connects with its node certificate (ADR 0014), which lets it write the
  streams of the pods it runs and no others. The sink stores a batch and
  then answers; a batch it has seen is stored once, keyed by (stream,
  offset). The node keeps what is not acknowledged and sends it again, to
  any instance: at least once on the wire, once in the store.
- Before a pod pauses, hibernates or is deleted, the node sends what its
  containers wrote, for a few seconds at most. A pod that dies with its
  node loses what the node had not sent, a second's lines as a rule.
- The node replaces the values of the pod's secrets in every line before
  it counts or sends it, so that a secret printed by mistake never
  reaches the store.
- A stream may send a bounded rate with a burst. Beyond it the node drops
  lines and, once the rate allows, sends one of its own that says how
  many. A tenant's lines count against its quota in the sink.
- A pod sends its spans over OTLP to its node, at an address of the
  node's own, `169.254.169.1`, held like the resolver's and not the pods'
  gateway, which the node never holds. The node knows the pod by its
  source address, stamps the pod's labels as resource attributes and
  sends the spans on to the sink with its certificate, as it does logs.
  The ingress ships its own lines and spans with its ingress certificate
  (ADR 0021); the sink takes streams of source `ingress` from an ingress
  only and every other stream from a node only.

### Kafka is the log of record; the database keeps no line

- Every line and span is produced to a Kafka topic before the sender is
  answered, and Kafka answers once two replicas have it. A topic per
  index, of the index's name, made by the sink at its start together
  with the index and its source: a fixed set the platform owns, which
  nothing a tenant does ever makes. The topics are a replay buffer of a
  day; the index is the store. A tenant never reaches Kafka.
- The sink stays the gate: it checks the peers, takes the labels, counts
  the quota, and for a log stream keeps its row, labels, time span, the
  next offset for the node, and the rule that a line at or below the last
  offset stored is stored. It locks the stream, produces the new lines
  and answers the node after the produce and the commit of the stream's
  row. A batch sent again after an answer that did not arrive may lie in
  the topic twice; the reads take each (stream, offset) and each span ID
  once.
- The partition is the reader's address: a log record's key is its
  stream's object (tenant, environment, kind, object; a tenant's job:
  tenant and job), so a stream's lines stay in order, an object's
  replicas and deployments share a partition, and a follow of a service
  reads one partition per topic. A span's key is its trace ID: a trace
  is read from one partition.

### Quickwit is the index and the store

- Quickwit indexes every word of a line with its position, every label,
  the level and the trace ID; a span's document carries the labels
  likewise. Nothing but the sink reaches it: it has no authentication of
  its own, so the network lets only `ferun-sink` in, and its own OTLP and
  Jaeger endpoints stay off.
- An index per retention: Quickwit drops whole splits by age, per index,
  so a stream's retention picks its index, the default or the longer one
  a job may ask for. A stream is never kept shorter than asked.
- An index's lines are hashed into partitions by tenant, environment and
  job, a split holds one partition's, and the three are tags, so that a
  search for one environment of a tenant that holds many reads only the
  splits that have it. An index's configuration has a version in its
  name, since Quickwit changes neither a partition key nor a split's
  mapping: a new version's indexes take the new lines, and the old ones
  are read beside them until their retention has emptied them.
- Deleting a tenant's environment or job deletes its streams' rows at
  once, so no read finds their lines any more, and leaves a delete task
  in Quickwit, which removes the documents when it rewrites their splits.

### Reading

- A tenant reads its own streams with its FeRun token or a person's
  session, filtered to the environments it may read (ADR 0031). `GET
  /api/v1/logs` answers lines with their labels, source, level and time:
  a stream selector, a time range, filters (words that must all appear,
  a word to exclude, a regular expression, a level, a trace ID), a
  direction, a limit and a cursor; LogQL's shape without its syntax. A
  word matches whole words, as in every search engine; the sink then
  applies the filters exactly, so what Quickwit found is narrowed, never
  widened.
- A read is the index and the tail: it asks Quickwit for the source's
  checkpoint, searches the index, then reads the topic's partitions of
  its selector from the checkpoint to their end, the lines Quickwit has
  not committed, seconds of them. Every line is in one of the two, some
  in both; the read takes each once. A line is readable at once and
  searchable within the commit interval.
- A follow is of one object (a service, a job, a tenant's job, or one of
  its instances or streams), read from its partition as records come; an
  environment as a whole is searched, not followed. A follow gives a
  bounded number of lines a second, then some more of error and warn
  lines, and says once a second how many it skipped: a screen can show
  no more, and a noisy service must not starve the reader.
- `ferund`'s own logs calls answer from the sink when it has the stream,
  so a paused pod's and a replaced instance's logs are read without a
  node. A run's log tail is no longer kept on its row.
- Traces are read through Jaeger's query API, served by the sink for a
  tenant with the tenant's filter forced; a trace by ID reads its
  partition's tail too, so a fresh request's trace is found at once. An
  environment's traffic per service is counted from spans in Quickwit
  (ADR 0021).
- A line that is a JSON object gives its level and its trace ID under
  the names OpenTelemetry's log bridges write; a line of text gives a
  level when it starts with one; an OTLP record's own severity and trace
  ID count first. Nothing else is parsed, and the text is kept as it
  came.

### Retention and quota

- Lines are kept for thirty days. A tenant's job may ask for longer at
  its create, up to four hundred days: a version's build log matters
  while the version does. A tenant may delete an environment's or a job's
  streams. Spans are kept for thirty days; every span is kept, none
  sampled.
- A tenant has a quota of bytes per day for its lines and one for its
  spans, set by the operator and counted at ingest. Beyond it the sink
  refuses that tenant's
  batches with `RESOURCE_EXHAUSTED`, the other tenants' in the same
  batch are stored, and the node drops and counts what was refused.

### The ingress's access log is a stream of the service's

- `ferun-ingress` writes each request it answered for an environment as
  one JSON line of a stream whose labels are the service's and whose
  source is `ingress`, which a container's stream never has, with a
  level from the outcome: a 5xx or an aborted request `error`, a 4xx
  `warn`, else `info`; and as the request's server span, with the same
  labels. A search of a service finds its requests beside its
  containers' lines, "errors only" finds its 5xx too, and the trace ID
  ties a request to the lines its handling wrote and to the spans the
  application sent.
- The ingress ships its records itself, from a bounded buffer, with its
  certificate; a stream per instance, start, environment and service, so
  a batch sent again is stored once. `ferund` keeps the API that reads an
  environment's access log and reads the sink alone; without a sink the
  call answers 503, and an ingress without a sink keeps no access log.
- A pod's requests through its HTTP bindings are a third source,
  `egress` (ADR 0028), shipped by the node.

## Alternatives considered

- **Into `ferund`.** It has the database, the tenants and the nodes'
  connections. Not chosen: logs are a write path of their own with their
  own load, and the control plane should not carry it, nor fail with it.
  Traces belong in the same place, and that place is not the control
  plane. **The ingress's access log in `ferund`'s database** was the
  decision for two days and moved into the sink on 2026-10-31 for the
  same reason.
- **Grafana Loki.** Ingesters with a write-ahead log, chunks in S3, a
  label index, LogQL. Not chosen: a stateful cluster of its own (a ring,
  ingesters, a compactor) beside the database that is already run
  replicated, its tenancy mapped from FeRun's anyway, AGPL-3.0, and its
  generality buys little when the labels are fixed and few. The sink's
  OTLP ingest lets an exporter point at Loki instead if ever wanted.
- **VictoriaLogs** (Apache-2.0): its own disks, no object store.
  **Elasticsearch, OpenSearch**: a full-text index at their cost of
  ingest and storage, for a platform that wanted none before Quickwit
  made it cheap. **An OpenTelemetry Collector** in front: it ships and
  transforms but stores nothing; it may stand between nodes and the sink
  later, since both ends speak OTLP.
- **FeCode collects** by following each container through `ferund`: a
  tenant's logs would depend on its client being up, over long streams
  per container through the control plane, and only FeCode would have
  them.
- **The guest ships** directly: a guest has no credentials for the
  platform's services, and the node already reads its log.
- **Labels from the pod** (Loki's free labels): a pod could make streams
  without bound and claim another object's name. Its own fields stay in
  its lines.
- **PostgreSQL partitions as the hot stage and gzip NDJSON chunks in
  the object store as the cold stage**, with a few chosen indexes
  (streams by label, chunks by stream and time, a level bit per chunk,
  trace IDs) and the rest scanned in bounded pieces, Loki's shape on the
  database and bucket already there: the decision of 2026-10-30, built
  and live for a day. Replaced on 2026-10-31 because a search for a word
  over weeks was a scan, traces would have needed a trace store of the
  same kind built beside it, and a row a line, then a delete, a dead
  tuple and vacuum made the database the bottleneck at a company's
  volume. Quickwit indexes every word and takes traces as well.
- **The database as the hot stage in front of Quickwit**, a line held
  in a row until the sink's own indexer had it published: one
  transaction for positions, quota and lines, but a queue, a lock service
  and a live tail in the control plane's database cluster, every line
  written twice, and an indexer of our own beside Quickwit's Kafka
  source, which does the same exactly once. Replaced by Kafka on
  2026-10-31. **The hot stage in memory with a write-ahead log** per
  instance: the instances would have to share it.
- **Redpanda** in place of Apache Kafka: one binary without a JVM and
  the same protocol, but its Community Edition is under the Business
  Source License, which forbids a service that lets third parties cause
  the creation of a topic; a log service sold to customers comes close
  enough for doubt, and the architecture should not bend around a
  licence. Kafka is Apache-2.0, run by Strimzi.
- **`feosd` producing to the topics itself**: Kafka's ACLs know a client,
  not a tenant or a stream, so neither the rule that a node writes only
  its pods' streams nor a tenant's quota could be kept there; the
  position a moved or restored pod's stream goes on from must be kept
  beside the node anyway; and `feosd` would carry a Kafka client of its
  own. The gate stays one program with one certificate check.
- **Records keyed by tenant and environment** rather than by object: a
  follow is of one service, and a busy neighbour in the same environment
  would fill its partition.
- **Grafana Tempo** for traces (and Loki for logs): AGPL-3.0, and a
  cluster each with a ring, ingesters and compactors. **ClickHouse**
  with an OpenTelemetry schema: strong where traces are aggregated, but
  it keeps its data on disks of its own with replicas and Keeper, and the
  object store as its main storage is awkward in the open-source build;
  the choice if analytics over spans come to matter more than search.
  **Jaeger** stores nothing in an object store itself.
- **Sampling traces** at the node or the sink: every span is kept, and
  the quota per tenant bounds the volume instead.

## Consequences

- A log outlives its pod: a crashed or replaced instance's last lines, a
  paused service's, a build's whole log, for the retention. FeCode drops
  its own build-log upload.
- A line is readable from the tail at once and searchable within
  seconds; a word is looked up, not scanned; only a regular expression
  and a substring are still scanned, within what the other filters left.
  A count over Quickwit's documents may count a line twice; a reader
  never sees it twice.
- Kafka and Quickwit are programs the platform runs and watches, each
  with its own pods, volumes and a network policy as its only lock, and
  a database for Quickwit's metastore; nothing but the sink talks to
  them.
- `ferund` changes little: labels and redactions on a pod's create and
  restore, the call that answers whose a token or session is, and its
  logs and access-log calls read from the sink. A guest of before the
  format change keeps the old format, and its pods' logs are not shipped.
- A tenant's secrets never reach the store; a line that printed one
  shows a marker in its place.

## History

Decided on 2026-10-30 (4a4415c) with PostgreSQL partitions as the hot
stage and gzip chunks in the object store as the cold stage, and built
the same day (a557b48). On 2026-10-31, when traces became certain and the
volume a company's, Quickwit replaced the chunks (9b14c29, 95ad5a1) and
took traces from pods through the node (578b4f9, 3b61cc5); the same day
the database's hot stage gave way to a log of record, first Redpanda
(91906c4), then Apache Kafka for its licence (f876600), keyed by the
object (489547e). The ingress's access log moved from `ferund`'s database
into the sink on 2026-10-31 (b820813, 371279d). A pod's egress records
became a third source on 2026-11-05 (a3e9933, ADR 0028); readers by
session, filtered by their environments, on 2026-11-06 (e382591,
ADR 0031). Audit records in the sink were planned and not built.
