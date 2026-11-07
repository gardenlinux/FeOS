# The sink

`ferun-sink` keeps the platform's logs (ADR 0026): every container's
output of the pods FeRun makes, shipped by the nodes, and the requests
`ferun-ingress` answered, written by its instances, appended to
Kafka's topics and indexed from there by Quickwit (ADR 0026 §10,
§11), and read by tenants over HTTP. This page is the contract between its parts;
the ADR says why.

```
guest ──CRI lines──▶ logs/container.log
                         │ StreamContainerLogs(follow, from_offset)
feosd (node) ──OTLP LogsService/Export, SinkService (mTLS, node cert)──▶ ferun-sink ──▶ PostgreSQL `sink` (streams, positions, quota)
     ▲ TelemetryResponse.sink_endpoint                                       │   └──▶ Kafka topic ferun-logs-v5-<days>d ──Kafka source──▶ Quickwit index of that name
ferund ──CreatePodRequest.log_labels──▶ feosd                                │
     ◀── GET /api/v1/tenant (token or session) ──────────────────────────────┤
readers ── GET /api/v1/logs, /api/v1/log-streams (Bearer, or cookie) ────────┘
```

## The guest

A container's stdout and stderr go through pipes the guest reads; it
writes each line to `logs/container.log` in the CRI log format:

```
2026-10-30T12:00:00.123456789Z stdout F the line's text
2026-10-30T12:00:00.123500000Z stderr P the first 64 KiB of a longer line…
```

The time is when the guest read the line, UTC, nanoseconds; `F` a full
line, `P` a part that the next entry of its stream goes on with: the
first 64 KiB of a longer line, or what came of a line that stayed
unfinished for a second. A part and the rest of its line are two entries,
the rest possibly empty. A log file gets an ID when the guest makes it
(`log_id`, 32 lowercase hex digits, kept beside it in
`container.log.id`), so that offsets of a log made anew start again under
a new ID; the ID stays with the file across the guest's restarts,
upgrades, reboots (an adopted container keeps its log) and suspends.
`StreamContainerLogs` answers the text without the prefix, with
`offset`, `next_offset`, `time_unix_nano`, `source`, `partial` and
`log_id`; with `follow` and `from_offset` it starts there. Lines of a log
written before (no prefix) are answered as they are, without a time.
docs/architecture/guest.md says how the guest reads the containers'
output.

## The node

`ferund` sends a pod's labels with its create
(`CreatePodRequest.log_labels`); `feosd` keeps them in the pod's record.
A restore brings them again (`RestorePodRequest.log_labels`,
`log_redact`): a node that takes the pod from its checkpoint records
them, one that has the pod replaces its own when the labels are not
empty. The guest's log and its ID travel on the pod's disk, so the
restored pod's streams are the same ones, and the node goes on from
where the sink has them (`Positions`), wherever they were shipped from.
`ferund` names the sink's ingest in every `TelemetryResponse`
(`sink_endpoint`, `--sink-endpoint`); empty: nothing is shipped.

For every running pod with labels and every container of it, `feosd`
learns the log's ID from its last entry, asks the sink where the stream
is (`SinkService/Positions`), follows the container's log from there and
exports batches over OTLP (`LogsService/Export`): at most 1 MiB or 1,000
lines a batch, sent once full or after 500 ms. An empty log is followed
from its start, and its first entry names its ID; if the sink has that
stream further on, the node starts again from there. A batch is kept
until the sink answers; on an error it is sent again after a backoff (1 s
to 30 s), to whichever address the endpoint resolves to, and the stream
reads nothing meanwhile: the guest's log is the buffer, and the node
holds a batch and a few entries a stream. `RESOURCE_EXHAUSTED` drops the
batch, counted as the limit's drops are. Before a running pod is
stopped or deleted, `feosd` lets its containers end: SIGTERM to each,
and up to 10 s for their processes to exit (the guest writes what they
wrote before it reports an exit), while the pod still runs and its
streams follow. Then, as before a pod is suspended or hibernates, it
sends what its containers wrote: each stream that follows its log reads
until the log was quiet for 400 ms and the sink took what it read, for
at most 5 s in all; a stream that does not follow now (no sink, a
backoff) is not waited for. Only then does the microVM end. A stream
whose pod no longer runs is let go of, never cut off: it sends what it
read and what the guest still sends it, for at most 10 s, and is ended
at the sink (`EndStreams`) once its pod is deleted, also after a stop. A
pod restarted after a crash goes on from the sink's positions: its log
is on its disk. A stop of `feosd`
that suspends its pods does not drain them: the next `feosd` goes on
from the sink's positions. After a delete it calls
`SinkService/EndStreams` for the streams whose log it knew, asked again
until the sink answers. A guest of before ADR 0026 (entries without
`log_id`) is not shipped.

**Redaction**: `CreatePodRequest.log_redact` names values (the pod's
secrets) that `feosd` replaces in every line's text with `[redacted]`,
the longest first, before the line counts against the limit and is
sent. They are kept in the pod's record (`PodInfo.log_redact`, empty in
every answer of the API), and never sent or logged. A value cut across
two parts of a line is not found.

**Limit**: 1 MiB/s per stream with a burst of 8 MiB (a token bucket on
bytes). Lines beyond it are dropped; once the bucket allows, the node
sends a record of its own, `[ferun] dropped N lines (B bytes)`, source
`stderr`, at the offset of the first line it dropped.

**TLS**: TLS 1.3, ALPN `h2`. The node presents its node certificate
(`spiffe://<td>/node/<name>`) and accepts a server certificate of the
platform's CA (its trust bundle) for server authentication, without an
IP address, whose one URI is `spiffe://<td>/sink/<name>` (a DNS label);
the name the node dials is not checked, so the endpoint may be an
address.

### The OTLP shape

One `ResourceLogs` per stream. Its resource attributes are the pod's
`log_labels`, as `ferund` set them, and the node's own:

| attribute | value | set by |
|---|---|---|
| `ferun.pod` | the pod's ID on its node | node |
| `ferun.container` | the container's name | node |
| `ferun.log_id` | the guest's ID of the log | node |
| `ferun.node` | the node's name | node |
| `ferun.tenant` | the tenant | `ferund` |
| `ferun.instance` | the sandbox's ID (`InstanceStatus.id`) | `ferund` |
| `ferun.environment` | the environment's ID | `ferund`, an environment's pods |
| `ferun.kind`, `ferun.object` | `service`, `addon` or `job`, and its name | `ferund`, an environment's pods |
| `ferun.replica`, `ferun.deployment` | numbers, as text; `ferun.release` is `ferun.deployment`'s earlier word, read where it is not set | `ferund`, an environment's pods |
| `ferun.job`, `ferun.attempt` | a tenant's job's ID, the attempt | `ferund`, a tenant's job |
| `ferun.retention_days` | days, as text | `ferund`, when a job asked |

One `LogRecord` per entry: `time_unix_nano` the line's time (0 for an
old line: the sink uses `observed_time_unix_nano`, when the node read
it), `body` the text as a string (invalid UTF-8 replaced), attributes
`log.offset` (int), `log.next_offset` (int, the entry's `next_offset`:
the offset just past the line), `log.iostream` (`stdout`, `stderr`, or
none), and `log.partial` (bool, only when true). The node's record of
dropped lines has `log.offset` of the first line it dropped and
`log.next_offset` past the last. `Positions` answers the greatest
`log.next_offset` stored; from a node that sends none, the offset of the
last line stored, which the node then sends again and the sink stores
once.

### A pod's egress records (ADR 0028)

Every request a pod sends through one of its HTTP bindings is recorded
by its `feos-authn` (its audit file, `docs/architecture/authn.md`): an
`http` record when the answer's head leaves, and an `http_end` record,
joined by `spanId`, once its body is done. `feosd` ships the `http`
records to the sink as the pod's **egress** stream, and each request as
a client span on the traces' path once its `http_end` came
([traces.md](traces.md#a-pods-egress)). This is the contract `feosd`
follows; the sink requires nothing of `http_end`.

- **Stream**: one per run of the pod's `feos-authn`. Its resource has
  the pod's `log_labels`, as its containers' streams have them, and the
  node's attributes, with `ferun.container` = `feos-authn`,
  `ferun.log_id` = an ID `feosd` gives the run (a UUID) and
  `ferun.source` = `egress`. It comes from the node's certificate as
  every other stream of a pod; an ingress cannot write one.
- **Records**: one `LogRecord` per request, its `http` record, in the
  order `feos-authn` wrote them: `time_unix_nano` the request's start,
  `log.offset` its number in the stream from 0, `log.next_offset` the
  number after, `log.iostream` none, `severity_number` 17 (error) for a
  status of 500 and above or a refusal, 13 (warn) for 400 to 499, else 9
  (info). `Positions` answers as for any stream, so a `feosd` that
  restarts goes on after the last record the sink has.
- **Body**: the `http` record as `feos-authn` wrote it, one JSON object:

  | field | value |
  |---|---|
  | `ts` | the request's start, RFC 3339 with nanoseconds, UTC |
  | `type` | `http` |
  | `binding`, `mode` | the binding's name and `name` or `intercept`; null when no binding matched |
  | `method` | the request's method |
  | `host` | the upstream's host; what the pod asked for when no binding matched |
  | `path` | the request's path, without the query |
  | `status` | the status the pod was answered: the upstream's, or `feos-authn`'s own for a refusal |
  | `durationMs` | until the answer's head; until its end where `feosd` folded the `http_end` in |
  | `upstreamAddr` | `address:port` dialed; null for a refusal before any |
  | `credential` | the credential's kind: `headers`, `basic`, `oauth2`, `clientCertificate`, never its value |
  | `traceId`, `spanId`, `parentSpanId` | the request's span, lowercase hex (`spanId` the proxy's, `parentSpanId` the pod's, null when the request started the trace) |
  | `error` | null for the upstream's answers, whatever their status; `{"code", "message"}` for a refusal of `feos-authn`'s own (`FeOS-Binding-Error`) |
  | `bytesIn`, `bytesOut` | optional: the request's and the answer's body bytes, where `feosd` folded the `http_end` in |
  | `level` | optional: `error`, `warn` or `info` as `severity_number`; the sink takes either |

  The codes of `error` are `feos-authn`'s: `denied_destination` (403),
  `denied_path` (403), `unknown_binding` (404), `method_not_allowed`
  (405), `misdirected` (421), `rate_limited` (429), `unreachable` (502),
  `tls` (502), `credential_unavailable` (503, no OAuth token could be
  had), `not_ready` (503), `binding_invalid` (503), `audit_unavailable`
  (503), and `bad_request` (400). A message never holds a credential, a
  token or a body. Query strings and bodies are never recorded.
- **Audit**: `feosd` also publishes each `http` record as the pod's audit
  record (`AuditRecord`, kind `http_request`, or `http`, its type;
  `detail_json` the same object). The sink is where a pod's requests
  are kept: `ferund` keeps none of them in its activity log. It counts
  them per pod, binding and minute for the bindings' incidents and keeps
  the newest ten of each pod's binding as their evidence, both for ten
  minutes ([control-plane.md](control-plane.md#http-bindings)). It may
  publish the other records (`http_end`, `token`, `bindings`) as audit
  records of their types; `ferund` drops `http_end` and keeps the others
  in its activity log (source `authn`), counting none.
- **Batches, limits**: as a container's stream. A record the limit
  drops is counted in the node's dropped-lines record.
- **Reading**: `GET /api/v1/logs?environment=<id>&source=egress` (and
  `kind`, `object` for a service's pods), `GET /api/v1/log-streams` with
  `source` `egress`; the line's text is the JSON above, its level the
  record's `level`, else its severity.

## The ingress's access log

`ferun-ingress` writes every request it answered for an environment
into the sink itself (ADR 0026 §8), when `--sink-endpoint` names the
ingest (its manifest: `ferun-sink-ingest.ferun.svc.cluster.local:4317`;
it runs in the cluster's network and resolves the name itself). The
sink is its only destination: without `--sink-endpoint` the access log
is not kept, and the agent counts its records as dropped.

- **Stream**: one per (ingress instance, start of its agent,
  environment, service). Key: `ferun.pod` = `ingress/<instance name>`,
  `ferun.container` = `<environment id>/<service>`, `ferun.log_id` = the
  agent's start (a UUID). Labels: `ferun.tenant` (the environment's
  owner, as the proxy's routing table has it), `ferun.environment`,
  `ferun.kind` = `service`, `ferun.object` = the service, `ferun.source`
  = `ingress`. A record's `log.offset` is its number in the stream from
  0, `log.next_offset` the number after; `log.iostream` none. The
  stream's `node` is empty.
- **Line**: the record as JSON, the fields and names of `ferund`'s
  `AccessLogRecord` (`tsNs` as a string, `time`, `durationNs`,
  `ingress`, `service`, `instance`, `host`, `method`, `path`, `proto`,
  `status`, `outcome`, `attempts`, `heldNs`, `bytesIn`, `bytesOut`,
  `clientIp`, `userAgent`, `traceId`, `spanId`, `parentSpanId`), and
  `level`: `error` for a 5xx or `aborted`, `warn` for a 4xx, else
  `info`. The LogRecord's time is the request's start.
- **TLS**: the ingress presents its certificate
  (`spiffe://<td>/ingress/<name>`) and checks the sink's as a node does.
  The sink takes streams with `ferun.source` = `ingress` from an
  ingress's certificate only, whose name is the key's (`ferun.pod` =
  `ingress/<name>`), and every other stream from a node's certificate
  only, never one whose `ferun.pod` begins with `ingress/`; a request
  with a stream of another is refused whole with `PERMISSION_DENIED`.
  `SinkService` is the nodes' alone: an ingress needs no positions (each
  start is a new stream) and ends no stream.
- **Batches**: the agent's buffer (10,000 records) as now, a batch at
  most 1,000 records or 1 MiB of lines, every second or once 1,000 wait;
  a failed batch is kept, with its offsets, and sent again with backoff
  (1 to 30 s) while new records wait in the buffer, which drops and
  counts what overflows it. A batch refused with `INVALID_ARGUMENT` or
  `RESOURCE_EXHAUSTED` is dropped and counted, as is a record without a
  tenant.
- **Reading**: the selector `source` (`ingress`, `egress` or
  `container`; none: all) picks them; a stream's JSON has `source`
  (`log_streams.source`, migration `0003`, `container` for every stream
  before it). `ferund`'s
  `GET /api/v1/environments/{env}/access-logs` reads them from the sink
  (`environment`, `source=ingress`, `trace`, backward from `before`,
  `since=0`) and answers its `AccessLogList`; the sink is the only
  place it reads ([`ferund`](#ferund)). `ferund`'s own table of before,
  `access_logs`, was dropped on 2026-10-31 (migration `0020`) without
  moving its records.

## The sink

A program of `go/` (`go/cmd/ferun-sink`, `go/internal/sink`), its own
database (`sink`, migrations of its own), two ports:

- **ingest** (gRPC over TLS 1.3, `--ingest-listen`, default `:4317`):
  `LogsService/Export` and `SinkService`, nodes only, and
  `LogsService/Export` for the ingresses' access logs ([The ingress's
  access log](#the-ingresss-access-log)), from the first certificate
  on. A batch is stored in one transaction and then answered: each
  stream's row locked, its new lines produced to its topic (Kafka
  answers once two of its three replicas have them), the row and the usage
  updated, the commit. A line is stored once per (stream, offset), and
  one at or below the last offset stored counts as stored: a node sends
  a stream's lines in the order of their offsets. A tenant over
  its quota (`--log-quota`, bytes of text a day, UTC, 0: none; reached by
  what is stored, so the batch that crosses it is stored) is refused with
  `RESOURCE_EXHAUSTED`, which names it; the other tenants' streams of the
  same batch are stored. A failure of the database or of Kafka is
  `UNAVAILABLE`, and nothing of the batch is stored; a request
  over 8 MiB, of more than 10,000 records, or with a resource without
  `ferun.pod`, `ferun.container`, `ferun.log_id` and `ferun.tenant` or a
  record without `log.offset` is refused. A stream's `node` is the name
  in the node's certificate, not `ferun.node`; the sink does not check
  that the node runs the pod. A stream's `source` is `ingress` for
  `ferun.source` `ingress`, `egress` for `egress` (a pod's requests
  through its HTTP bindings, from a node's certificate), else
  `container`.
- **API** (HTTP, `--listen`, default `:8080`): the routes below, behind
  the cluster's Gateway at `run.<domain>/api/v1/logs` and
  `/api/v1/log-streams`; `/healthz`, 200 once the ingest serves and while
  the database answers.

**Identity.** The sink holds a service token of kind `sink` (`ferund
service-token --kind sink --name <name>`, kept in a Secret). With it, it
asks `ferund` for a certificate (`POST /api/v1/service-certificates`,
Bearer the token, `{"csr": "<PEM>"}` → `{"certificate": "<PEM chain>",
"bundle": "<PEM roots>", "notAfter": "<RFC 3339>"}`, the CSR of an
Ed25519 key), valid 24 h, the URI `spiffe://<td>/sink/<name>` and
ServerAuth, and asks again at half its life. `<name>` is the token's,
and the sink's `--name` must be it: the instances of a deployment share
one token and name (`ferun-sink`). The sink checks the certificate's one
URI and key and that it verifies to the bundle. The bundle verifies the
nodes' certificates (ClientAuth, one URI `spiffe://<td>/node/<name>`)
and the ingresses' (`spiffe://<td>/ingress/<name>`).

**Readers** (ADR 0031). A read brings a FeRun token (`Authorization:
Bearer …`), or, where the sink is served under `ferund`'s host (the
Gateway's `run.<domain>`), a person's session: the cookie
`__Host-ferun_session` (or, for one release, its old name
`ferun_session`) and the tenant in `FeRun-Tenant` (400 without it). A
session's change (`DELETE`) needs `X-Requested-With: ferun` and a
request of the same origin (`Sec-Fetch-Site`, or `Origin` against
`Host`; 403 otherwise), as `ferund`'s own. The sink asks `ferund` `GET
/api/v1/tenant?filter=environments` with the same `Authorization`, or
the cookie and `FeRun-Tenant` (`200 {"tenant", "role", "environments"}`;
a 4xx but 429 passed on as it came), and keeps the answer by the
SHA-256 of the credential and the tenant, a token's a minute, a
session's 10 s. A `role` on the tenant reads every stream and span; a
principal without one (a person's or a token's grants on environments)
reads those whose `environment` is one of `environments`, and none of no
environment (a sandbox's, a tenant's job's, a build's); one that may
read nothing is 404 "no such tenant". An answer without `role` is a
`ferund` of before people: the whole tenant's, as its every token was.

**Stages.** `log_streams` (one row per stream key: labels, first and
last time, next offset, the last offset stored, ended; its ID `s_` and 16
random hex digits) and `log_usage` (a tenant's bytes of a day) in the
database; the lines in Kafka, then in Quickwit (§11). A topic per
index, of its name: `<--index-prefix>-v<version>-<days>d`
(`ferun-logs-v5-30d`), the days of `--log-retention`, or of
`--log-retention-max` for a stream whose `ferun.retention_days` is
longer than the default. At its start, and only then, the sink makes the
topic of each class that is not there (`--kafka-partitions` 64,
`--kafka-replication` 3, `min.insync.replicas` 2, `retention.ms`
`--kafka-retention` 24 h, `segment.ms` an hour), its index (Quickwit's
index API) with its retention, `--index-commit-timeout` (10 s) as its
commit interval and `--index-partitions` (16, at most 200) partitions,
and the index's one Kafka source `kafka` on the topic
(`auto.offset.reset` earliest; a session of 10 s). A record is a line as
the index's document, JSON: `ts` (ns, the time field), `tenant`,
`stream` (the ID), `environment`, `kind`, `object`, `replica`,
`release` (the stream's deployment: the field keeps the label's earlier
word, since a field of its own is a new mapping, so a new index and
topic of every retention, for a field no search filters by; it takes
the new name with the next mapping version), `instance`, `container`, `job`, `attempt`, `source`,
`iostream`, `level` (the bit), `trace_id`, `off`, `partial`, `cont`, and
`text` (Quickwit's `default` tokenizer, with positions). Its key is
its stream's object, `<tenant>/<environment>/<kind>/<object>` (an
environment's job run: kind `job`, its job's name), or `<tenant>//<job>`
for a stream of no environment (a tenant's job), its partition the key's
FNV-1a hash modulo the topic's partitions, chosen by the sink: a
stream's lines are in order in one partition, and an object's streams
(its replicas, deployments and runs) share theirs. An environment's objects
spread over the partitions, so one busy service fills its own partition
and a follow of another reads none of its records. The source indexes each record once, the topic's
offsets kept in Quickwit's metastore with each split it publishes; a
batch the sink produced twice (an answer to the node that did not
arrive, then the node's next send) is indexed twice, and the reads take
one (stream, offset) once. `--kafka-brokers` names Kafka's brokers
(also the sources'), `--index-search-url` Quickwit's REST API. The hot stage of
before (`log_batches`, `log_lines`) went on 2026-10-31 (migration `0008`).

**Partitions.** One tenant may hold many projects' environments (FeCode's
does), so an index's lines are hashed into partitions by their tenant,
environment and job (`partition_key: hash_mod((tenant,environment,job),
<--index-partitions>)`), and a split holds one partition's lines: a
commit writes a split per partition that got lines, and merges keep
them apart. `tenant`, `environment` and `job` are tags (`tag_fields`):
Quickwit records a split's values of each while they are fewer than
1,000, and a search for an environment or a job reads only the splits
that have it. More partitions mean fewer environments a split and more,
smaller splits; Quickwit's own bound is 200 (`max_num_partitions`).

**Versions.** Quickwit never changes an index's partition key or time
field, and a changed mapping only applies to new splits, so the index
configuration has a version in the index's name
(`quickwit.MappingVersion`): version 1, `<prefix>-<days>d`, had
`tenant` as its only tag and no partitions; version 2 is the above. A
new version's indexes take every new line; a read searches
`<prefix>-*`, the old and the new together (the fields it reads are the
same in both, `ts` a datetime in each, so they sort as one), and a
delete leaves its task in every index of the prefix. The old indexes
are emptied by their own retention and can then be deleted by hand.
`--index-partitions` (and `--index-commit-timeout`) apply only to
indexes made after a change: an index keeps what it was made with, so a
change takes a new version or a new retention.

**Chunks of before.** Until 2026-10-31 the cold stage wrote gzip NDJSON
chunks to the object store (`log_chunks`); they all moved into the index
that day, and the table, the code that moved them and the sink's bucket
settings went (migration `0007`).

**Level** (§6): an OTLP record's severity number when the text gives
none (1 to 8 debug, 9 to 12 info, 13 to 16 warn, 17 to 24 error); a JSON line's `level`, `lvl` or `severity` (a word, or a
number on pino's scale: 50 and above error, 40 warn, 30 info, 10 and 20
debug); else a leading `ERROR`, `WARN`, `WARNING`, `INFO`, `DEBUG`,
`TRACE`, `FATAL`, `PANIC` (bare, `[…]`, or `level=…`, also after a
leading `time=` or `ts=` field, as Go's slog writes it), case
insensitive; mapped to `error` (also fatal, panic, critical), `warn`,
`info`, `debug` (also trace), or `` (unknown). **Trace ID**: an OTLP
record's `trace_id`, else a JSON line's `trace_id`,
`traceId` or `traceparent` (its second field): 32 lowercase hex, not all
zero.

**Retention**: `--log-retention` (720 h) or a stream's
`ferun.retention_days`, at most `--log-retention-max` (9600 h); a sweep
every 10 min deletes the streams whose last line is past their
retention. A topic keeps its records `--kafka-retention`, a
replay buffer: an index that falls further behind loses what the topic
dropped. Quickwit drops a
split of an index once its newest line is past the index's retention,
which is at least the stream's; a read never answers a line of a stream
that is gone.

## Reading

All routes need a reader (**Readers** above) and answer only the
streams it may read: a selector that names others finds nothing, a
follow gives none of their lines.

A read takes its streams from the database, then reads, of each log
topic, the partitions of their keys from the index source's checkpoint
(`GET /api/v1/indexes/<id>`: per partition the last offset published) to
their end, the lines Quickwit has not published, then searches the
indexes (`<prefix>-*`, the versions of before too). A line in both is
taken once (stream, offset).

### `GET /api/v1/logs`

| parameter | |
|---|---|
| `environment`, `kind`, `object`, `replica`, `instance`, `container`, `job`, `attempt`, `stream`, `source` | the selector; one of `environment`, `job`, `instance`, `stream` is required; `source` is `ingress` or `container` (none: both) |
| `since`, `until` | RFC 3339 or Unix nanoseconds, both included; `until` defaults to now, `since` to `until` − 1 h (backward) or to the stream's start (forward), and, with `trace`, to the start of what is kept |
| `q` | words that all appear, case insensitive; `-word` must not. The index finds whole words (`erro` finds no `error`; a word with punctuation, `user_id=42`, as a phrase); a line it found must then hold each word as a substring |
| `re` | an RE2 expression |
| `level` | a comma list of `error`, `warn`, `info`, `debug`, `unknown` |
| `trace` | a trace ID |
| `direction` | `backward` (default: the newest first) or `forward` |
| `limit` | lines, default 500, at most 5,000 |
| `cursor` | from an answer before: the next page in the same direction |
| `follow` | `1`: new lines as they come, as NDJSON |

```json
{
  "lines": [
    {"ts": "1793361600123456789", "time": "2026-10-30T12:00:00.123456789Z", "stream": "s_…",
     "offset": "4096", "source": "stdout", "level": "error", "partial": false, "text": "…"}
  ],
  "streams": {"s_…": {"id": "s_…", "tenant": "…", "instance": "…", "container": "main",
     "environment": "…", "kind": "service", "object": "api", "replica": 0, "deployment": 12,
     "job": "", "attempt": 0, "node": "…", "source": "container", "firstTime": "…", "lastTime": "…", "endedAt": null,
     "lines": 1234, "bytes": 56789}},
  "cursor": "…",
  "partial": false
}
```

A line the node sent in parts (`log.partial`: a 64 KiB part of a longer
line, or one left unfinished for a second) is answered once, joined with
the following lines of its stream and source up to and including the
first that is not partial, with the first part's `ts`, `offset` and
`level`; `partial` is `true` when its rest is not there yet or when it
was cut at 1 MiB. The filters see the joined text. A page holds whole
lines only and its cursor goes on after the last line it considered, so
a page may hold fewer lines than `limit` and still have a cursor. A
follow holds a partial line for its rest for at most 5 s, then gives it
`partial`; a part whose line the follow's first page gave is given on its
own.

A page's lines are oldest first in either direction; a backward page's
`cursor` leads to older lines, a forward page's to newer. `cursor` is
empty at the end. `partial` says the page stopped at the scan limit (256
MiB of lines read) before `limit`: there may be more behind `cursor`.
With `follow=1` the answer is `application/x-ndjson`, one object a line:
`{"line": {…}, "stream": {…}}` (the stream once, with its first line), and
`{}` after 15 s without a line. It gives the page a backward read gives
(`since`, `until`, `limit` and the filters as there), oldest first, then
every line of the selector's streams stored after it: the topics'
partition of the selector's object, read from its end of before the page
as records come (a fetch waits up to 500 ms); a line the page gave is not
given again, and a stream of the object that starts meanwhile is followed
too. A follow is of one object: `environment`, `kind` and `object`; a
tenant's `job`; or an `instance` or `stream` (of one object); any other
selector is `400` (an environment as a whole is read, not followed). A
follow gives at most 100 lines a second (a burst of 500), and once those
are spent 50 more a second of `error` and `warn` lines (a burst of 200);
the lines it skips it counts and says at most once a second,
`{"skipped": <n>}`. It takes no `cursor`. A
`level` of `unknown` selects the lines whose `level` is `""`.

### `GET /api/v1/log-streams`

The selector of `/logs` and `since` (default 7 days back) →
`{"streams": [ … ]}`, as in `/logs`, the latest `lastTime` first, at
most 500.

### `DELETE /api/v1/log-streams?environment=<id>` or `?job=<id>`

Needs write on the tenant (else 403 `role_required`). Deletes the streams' rows, and leaves a delete task of the environment's or job's lines in every log index (Quickwit removes them when its janitor rewrites their splits; no read finds them meanwhile). 204.

## `ferund`

- **Labels**: an environment's instance and job run, and a tenant's job,
  get `log_labels` on their pod's create (the table above), kept on their
  sandbox row (`sandboxes.log_labels`, migration `0019`).
  `ferun.deployment` is the deployment the instance was made for; a pod
  keeps the labels of its create, so the sink reads its earlier word
  `ferun.release` where it is not set, which `ferund` wrote, beside
  `ferun.deployment`, up to the version of its migration `0024` (ADR
  0018, "The deployments' word"). The column `log_streams.release` and
  the index field `release` hold it. A sandbox a tenant made
  itself (an attached service's workspace too) has none: its logs are
  not shipped. `POST /api/v1/jobs` takes `logRetentionDays` (1 to 400),
  which becomes `ferun.retention_days`.
- `--sink-endpoint` (host:port of the ingest) to the nodes, a name
  resolved every 10 s into one ready instance's address per node;
  `--sink-url` (the API, inside the cluster) for its own reads.
- `GET /api/v1/tenant`: the principal's tenant, its role there and the
  environments it holds a grant on (control-plane.md, The sink).
- `ferund service-token --kind sink --name <name>` prints
  `feos-sink-<secret>` once; `GET /api/v1/admin/service-tokens` lists the
  tokens, `DELETE /api/v1/admin/service-tokens/{id}` revokes one (the
  certificates issued under it live on, 24 h at most). `POST
  /api/v1/service-certificates` issues a server certificate
  (ServerAuth, no DNS name) for an Ed25519 or ECDSA P-256 key; at most 10
  requests of a token an hour (429).
- `InstanceStatus.image`: the image the instance runs,
  `registry/repository@sha256:…` as the deployment resolved it.
- `GET /api/v1/environments/{env}/access-logs` reads the ingress's
  lines from the sink alone, with the caller's token or session
  (`environment=<ID>&source=ingress&direction=backward&since=0&until=<beforeTsNs − 1>&trace=<traceId>&limit=<the rest>`,
  paged up to 8 times), each line's JSON an `AccessLogRecord`. Without
  `--sink-url` it answers 503 `no_log_store`, and 503
  `log_store_unavailable` (`Retry-After: 5`) when the sink fails.
- `GET /api/v1/environments/{env}/traffic` reads the sink's
  `GET /api/v1/traffic`, the services' own server spans counted per
  service and step, and for a routed service without them the
  ingress's ([traces.md](traces.md#reading-the-traffic)), with the
  caller's token or session, and answers 503 as `…/access-logs` does.
- The objects' logs call and a job run's log read from the sink when
  `--sink-url` is set, with the caller's token or session
  (`instance=<sandbox ID>&container=main&direction=backward&since=<its
  start>`, paged), and from the node as before when the sink has no lines
  or fails; a paused instance's lines are answered from the sink. A
  tenant's job's log reads the sink too, also once the job has ended;
  its kept tail when the sink has no line.
- **Redaction**: a create with labels names `log_redact`, asked anew at
  each create and stored nowhere: the values `ferund` redacts its stored
  tails of, each also in its URL-userinfo escaping, of 4 bytes or more,
  longest first. An environment's pod: its addons' credentials, its
  tenant secrets (latest versions and those its deployments pin) and those
  its template names; a tenant's job's: its secrets and image
  credentials. A failure to compute them fails the create, so that no
  pod ships unredacted lines. Every restore sends the row's labels and
  the redactions, asked anew, as well (`RestorePodRequest`), with the
  same failure, so that a hibernated pod restored on another node goes on
  shipping.
- **Parts**: where `ferund` answers lines of a tail, from the node or the
  sink, a partial entry is joined with the next entries of its source up
  to one that is not partial, the line cut to its budget (8 KiB in an
  answer); a line still open at the end goes in as far as it came. From
  the sink it pages back until `lines` + 1 lines have ended. `ferund` redacts a running tenant job's
  sink lines again; an ended job's (its secrets are gone) are the node's
  redaction alone.
