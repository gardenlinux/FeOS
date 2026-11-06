# Traces

`ferun-sink` keeps the spans of the pods FeRun makes and of the requests
`ferun-ingress` answers (ADR 0026 §10, step 10), appended to a Kafka
topic and indexed from there by Quickwit's trace index (§11), and
answers them to a tenant in the shape of Jaeger's query API, and the
ingress's spans also as its traffic per service. This page is
the contract between the parts; the ADR says why. Logs are
docs/architecture/sink.md; what that page says of identities, the topics
and Quickwit holds here unless this one says otherwise.

```
pod (OTel SDK) ──OTLP/gRPC :4317, OTLP/HTTP protobuf :4318──▶ feosd at 169.254.169.1
                                   │ the pod by its source address, its ferun.* labels stamped
feosd (node) ──OTLP TraceService/Export (mTLS, node cert)──▶ ferun-sink ──▶ PostgreSQL `trace_usage` (quota)
ferun-ingress ──TraceService/Export (mTLS, ingress cert)──▶     │   └──▶ Kafka ferun-traces-v4-<days>d ──Kafka source──▶ Quickwit index of that name
tenants (FeCode) ── GET /api/v1/jaeger/api/… (Bearer) ──────────┤
ferund ── GET /api/v1/traffic (the caller's Bearer) ────────────┘
```

Sampling: every span that arrives is kept (ADR 0026, open question 2).
The ingress records a span of every request it routes to an environment,
as it writes a line of its access log, and starts a new trace sampled.
Rate limits on the node and the quota in the sink bound the volume.

## The pod

The node serves OTLP at an address of its own, `169.254.169.1`
(`feosd --otlp-address`), which it holds as it holds the resolver's: on
`feos-dns` and on every pod's TAP device, so that ARP for it is answered
there (`arp_ignore` 1). Not at the pods' gateway, `169.254.0.1`: the node
never holds that (node.md, Devices and routes), since where it is the
gateway of the node's own network, as on IronCore, the node would lose
its own; and held on another device than the pod's TAP, the pod's ARP
for its gateway would no longer be answered. `--otlp-address` is checked
as `--dns-address` is: an address of a host, neither the pods' gateway
nor in `--pod-ipv4-range`, and neither the resolver's, `169.254.169.254`
nor in a `--deny-prefix`.

- `:4317`, gRPC: `opentelemetry.proto.collector.trace.v1.TraceService/Export`.
  `LogsService` and `MetricsService` answer `UNIMPLEMENTED`.
- `:4318`, HTTP/1.1: `POST /v1/traces` with `Content-Type:
  application/x-protobuf`, the body an `ExportTraceServiceRequest`,
  optionally `Content-Encoding: gzip`. The answer is an
  `ExportTraceServiceResponse` as `application/x-protobuf`. JSON
  (`application/json`) answers 415; `/v1/logs`, `/v1/metrics` and any
  other path 404; another method 405.
- A request is at most 4 MiB on the wire and 6 MiB decompressed, so
  that it fits the sink's 8 MiB with the labels stamped (413,
  `RESOURCE_EXHAUSTED`; a gRPC message beyond 4 MiB gets gRPC's own
  `OUT_OF_RANGE`, before the node sees it).

The firewall lets a pod reach these two ports at that address, and
nothing else of the node there (`iifname "pod-*" ip daddr 169.254.169.1
tcp dport 4317-4318 accept`); the ports are fixed. The node serves once
the pods' network is there, with a control plane.

**Whose spans.** The node knows the pod by the request's source address,
which the firewall's anti-spoofing rule (`fib saddr . iif oif missing
drop`) makes the pod's own. Every pod on the node with labels counts, in
whatever state: one that starts sends at once, and an SDK sends what it
has as its process ends. The node keeps the pods' addresses as their
events come, and looks at its pods again before it refuses an address it
does not know, so the first request of a pod that just started is not
refused. An address of no pod with labels answers 403
(`PERMISSION_DENIED`). A pod sends as itself only.

**Labels.** In every `ResourceSpans` the node removes each resource
attribute whose key starts with `ferun.`, then adds the pod's labels, the
same as its log streams carry (sink.md, The OTLP shape) without
`ferun.container`, `ferun.log_id` and `ferun.retention_days`:
`ferun.pod`, `ferun.node`, `ferun.tenant`, `ferun.instance`, and those of
`ferun.environment`, `ferun.kind`, `ferun.object`, `ferun.replica`,
`ferun.deployment` (else its earlier word `ferun.release`), `ferun.job`,
`ferun.attempt` the pod has. A resource
without `service.name` gets the pod's `ferun.object`, else its
`ferun.job`, else `unknown_service`. Spans, scopes and their attributes
pass as they came.

**Rate.** A pod may send 1 MiB/s of requests (decompressed), with a
burst of 8 MiB; beyond, 429 with `Retry-After: 1` (gRPC
`RESOURCE_EXHAUSTED`). A rate-limited request is not sent on.

**On to the sink.** The node sends the request on to the sink's
`TraceService/Export` over its sink connection (the node certificate,
the sink's endpoint from `TelemetryResponse.sink_endpoint`) and answers
the pod with the sink's answer, within 8 s. It keeps nothing: the SDK's
batch and its retries are the buffer. The sink's codes map to the pod's:

| sink | gRPC to the pod | HTTP to the pod |
|---|---|---|
| OK (with `partial_success`) | OK, the same | 200, the same |
| `RESOURCE_EXHAUSTED` | `RESOURCE_EXHAUSTED` | 429, `Retry-After: 30` |
| `UNAVAILABLE`, `DEADLINE_EXCEEDED`, no connection, no sink endpoint | `UNAVAILABLE` | 503, `Retry-After: 2` |
| `INVALID_ARGUMENT` | `INVALID_ARGUMENT` | 400 |
| `PERMISSION_DENIED`, other | `INTERNAL` | 500 |

**The environment.** The node gives every container of a pod with a
network these variables, each unless the container's own environment
(its spec's) has it; they win over the image's:

| variable | value |
|---|---|
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | `http://169.254.169.1:4318/v1/traces`, at `--otlp-address` |
| `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | `http/protobuf` |
| `OTEL_SERVICE_NAME` | the pod's `ferun.object`, else its `ferun.job`; unset without |

Only the traces' endpoint is set, so an SDK sends neither logs nor
metrics there. A pod's logs stay its output (sink.md).

## A pod's egress

A pod's request through one of its HTTP bindings (ADR 0028) is a client
span of the pod's `feos-authn`, which `feosd` sends to the sink's
`TraceService/Export` with its node certificate, as it sends the pods'
own spans on. The request's record in the egress stream
([sink.md](sink.md#a-pods-egress-records-adr-0028)) carries the same
`traceId` and `spanId`.

| resource attribute | value |
|---|---|
| `service.name` | the pod's `ferun.object`, else its `ferun.job`, else `unknown_service`: the span is the pod's call |
| `ferun.source` | `egress` |
| `ferun.*` | the pod's labels, as its own spans carry them |

The span, made of the request's `http` and `http_end` records (joined by
`spanId`), once its `http_end` came: `trace_id` the record's `traceId`
(the request's `traceparent`'s, else a new one); `parent_span_id` its
`parentSpanId`; `span_id` its `spanId`, the one `feos-authn` passes on to
the upstream in `traceparent`; `name` `<METHOD> <binding>` (`GET erp`);
`kind` `SPAN_KIND_CLIENT`; `start_time_unix_nano` the request's start,
`end_time_unix_nano` its end (`http_end`'s `durationMs`, else the
`http` record's); `status` `STATUS_CODE_ERROR` for a status of 500 and
above or a refusal of `feos-authn`'s own (the message its code), else
unset. A request without its `http_end` (a killed process) has no span.
Attributes: `http.request.method`, `url.path` (without the query),
`server.address` (the upstream's host), `server.port` (when not 443),
`http.response.status_code` (left out when none was written),
`ferun.binding`, `ferun.binding.mode` (`name`, `intercept`),
`ferun.credential` (its kind), `network.peer.address` (the address
dialed), `http.request.body.size` and `http.response.body.size` (the
`http_end`'s bytes), `ferun.redacted` (the `http_end`'s `redacted`, the
places of the binding's credential replaced in the answer, when there
were any), and `ferun.error.code` for a refusal. The sink keeps it with
`source` `egress`; a read of an environment's traces finds it with the
pod's own spans, and `source=egress` alone.

## The ingress

`ferun-ingress` sends one span per request it writes to its access log,
from the same buffer, in batches of its own, to the sink's
`TraceService/Export` with its certificate. One `ResourceSpans` per
(environment, service) in a batch:

| resource attribute | value |
|---|---|
| `service.name` | `ferun-ingress` |
| `ferun.pod` | `ingress/<instance's name>` |
| `ferun.source` | `ingress` |
| `ferun.tenant`, `ferun.environment` | the request's |
| `ferun.kind`, `ferun.object` | `service`, the service's name |

The span is the request's record (proxy/trace.go): `trace_id`, `span_id`
the ingress's, `parent_span_id` the client's when its `traceparent` was
continued, `trace_state` as passed on, `flags` the trace's; `name` the
method (`GET`); `kind` `SPAN_KIND_SERVER`; `start_time_unix_nano` when
the request came, `end_time_unix_nano` start plus its duration; `status`
`STATUS_CODE_ERROR` with the outcome as message for a 5xx or an aborted
request, else unset. Attributes: `http.request.method`, `url.path`
(without the query), `server.address` (the host), `network.protocol.version`
(`1.1`, `2`), `http.response.status_code`, `client.address`,
`user_agent.original`, `http.request.body.size`,
`http.response.body.size`, `ferun.outcome`, `ferun.attempts`,
`ferun.held_ns` (the time the request waited for an instance, int), and
`ferun.upstream_instance` (the instance that answered); a string
attribute that is empty, and a status code of 0 (none written), are left
out. A batch of spans goes once, after the sink took or refused the
lines of the same records: a batch the sink refused or failed to take,
and the spans it rejected (`partial_success`), are dropped and counted
(`ferun_ingress_spans_dropped_total`, beside the access log's
`ferun_ingress_access_log_dropped_total`); the access log's lines are
written as before.

## The sink

### Ingest

`TraceService/Export` on the ingest's gRPC server, behind the same
`authorize`. Each `ResourceSpans` must have `ferun.tenant` and
`ferun.pod`. From a node, `ferun.source` must not be `ingress` and
`ferun.pod` must not start `ingress/`; from an ingress, `ferun.source`
must be `ingress` and `ferun.pod` `ingress/<its name>`. Else the request
answers `PERMISSION_DENIED`. At most 10,000 spans a request
(`INVALID_ARGUMENT`).

A span without a valid `trace_id` (16 bytes, not all zero) or `span_id`
(8 bytes, not all zero) is dropped and counted in the answer's
`partial_success.rejected_spans`, with an `error_message`. The others
become documents (below) and are produced to the trace topic in one
transaction with the tenant's usage, each keyed by its trace ID (a
trace's spans in one partition); the answer comes after Kafka has
them on two of its three replicas and the commit, well within the 8 s the
node gives a pod's export.
A span without a start gets the time it came as its `ts`; a resource
without `service.name` the service `unknown_service`.

**Quota.** Bytes of documents per tenant per day, `--trace-quota`
(default 1 GiB), counted in `trace_usage` (tenant, day) at ingest.
Beyond it the request answers `RESOURCE_EXHAUSTED` (the request that
crosses it is stored; of a request of several tenants, the others'
spans are stored). A loop of the sink deletes the usage of the days
before yesterday.

### The topic

`trace_spans` and its indexer held a span until its index had published
it until 2026-10-31 (§10); now the trace index's Kafka source takes the
spans from their topic, of the index's name, made with it at the sink's
start (sink.md, Stages). Spans sent twice (a node's retry after an
answer that did not arrive) are two records and two documents; a read
keeps one per span ID.

### The index

`<--trace-index-prefix>-v<N>-<days>d`, default `ferun-traces-v4-30d`
(`--trace-retention`, 30 days), created at first use with its topic and source as the logs'
indexes are, with a commit interval of 5 s. `N` is the
mapping's version (`quickwit.TraceMappingVersion`): a changed mapping is
a new index, and reads search `<prefix>-*`, every version, until the old
one ages out. Spans are partitioned as the log indexes' lines are, by
`hash_mod((tenant, environment, job), --index-partitions)` (16), and
`tenant`, `environment` and `job` are tag fields, so a read of one
environment's spans opens only its partition's splits. A document is a
span:

| field | type | from |
|---|---|---|
| `ts` | datetime (ns), the timestamp field, fast | `start_time_unix_nano` |
| `tenant` | raw text, the tag field | `ferun.tenant` |
| `environment`, `kind`, `object`, `instance`, `job`, `source`, `pod`, `node` | raw text | the `ferun.*` attributes; `source` `ingress` or `egress` as `ferun.source` says, else `container` |
| `replica`, `release`, `attempt` | u64 | the `ferun.*` attributes; `release` is the deployment, under the label's earlier word, as the log index has it (sink.md) |
| `trace_id`, `span_id`, `parent_span_id` | raw text, lower-case hex, `trace_id` fast | the span |
| `trace_state` | stored, not indexed | the span |
| `flags` | u64 | the span's `flags`, its low 8 bits (W3C trace flags) |
| `service_name` | raw text, fast | `service.name` |
| `span_name` | raw text, fast | `name` |
| `span_kind` | u64 (OTLP's number) | `kind` |
| `start_ns`, `end_ns`, `duration_ns` | i64, fast | the times; duration end minus start, at least 0 |
| `status_code` | u64 (0 unset, 1 ok, 2 error) | `status.code` |
| `status_message` | text | `status.message` |
| `scope_name`, `scope_version` | raw text | the scope |
| `resource_attributes`, `span_attributes` | json | the attributes |
| `events` | array of json `{time_ns, name, attributes}` | `events` |
| `links` | array of json `{trace_id, span_id, trace_state, attributes}` | `links` |

Attributes become JSON values by their `AnyValue`: string, bool, int (a
number; beyond ±2^53 a string), double, array, key-value list as an
object, bytes as base64 text.

### Reading: Jaeger's query API

Under `/api/v1/jaeger`, with a reader as `GET /api/v1/logs` takes it
(sink.md, Readers); every query is the tenant's own, whatever it asks,
and a reader of environments only (ADR 0031) reads the spans of those:
a trace's other spans are left out. The paths and answers are those of Jaeger's HTTP query API
(`jaeger-query`'s `/api/…`), so a client of that API, Grafana's Jaeger
data source among them, reads them with the base URL
`<sink>/api/v1/jaeger` and the token as a header.

- `GET /api/v1/jaeger/api/services` → `{"data": [names], "total": n}`,
  the `service_name`s of the tenant's spans of the last 7 days.
- `GET /api/v1/jaeger/api/services/{service}/operations` → `{"data":
  [names], "total": n}`, its `span_name`s of the last 7 days.
- `GET /api/v1/jaeger/api/traces/{traceID}` → `{"data": [trace]}`; none
  of the tenant's spans: 404 `{"data": null, "errors": [{"code": 404,
  "msg": "trace not found"}]}`. The spans come from the index and from
  the topic's tail: the trace's partition from the source's checkpoint
  to its end, the spans not yet published, so a fresh request's trace is
  found at once; one per span ID, at most 10,000. FeRun's selectors
  (below) narrow it to the spans that have them.
- `GET /api/v1/jaeger/api/traces?service=&operation=&tags=&start=&end=&minDuration=&maxDuration=&limit=`
  → `{"data": [traces]}`: the traces of the newest spans that match,
  each whole. A search reads the index alone: a span is found by it at
  most a commit (5 s) after it came; `/services` and `/operations` too. `start`, `end` in µs since the epoch (default the last
  hour); `minDuration`, `maxDuration` as Go durations (`100ms`, `1.5s`);
  `tags` a JSON object of attribute and value (a string, a number or a
  bool; a key without a backslash), each matched in the span's or the
  resource's attributes; `limit` traces, default 20, at most 100.
  FeRun's own selectors may be added: `environment`, `object`, `job`,
  `instance`, `source`; they narrow each trace answered as they narrow
  `/traces/{traceID}`. `service` is optional here, unlike in Jaeger.

FeRun's selectors narrow `/services` and `/services/{service}/operations`
as well. Every answer has Jaeger's `total`, `limit`, `offset` and
`errors` beside `data`; a refused query answers 400 with `errors`.

A trace is Jaeger's JSON: `{"traceID", "spans": [...], "processes":
{"p1": {"serviceName", "tags"}}, "warnings": null}`; a process per
distinct resource (service name and resource attributes, the `ferun.*`
among them). A span is `{"traceID", "spanID", "operationName",
"references": [{"refType": "CHILD_OF", "traceID", "spanID"}],
"startTime" (µs), "duration" (µs), "tags", "logs", "processID",
"warnings": null, "flags"}`; a link is a `FOLLOWS_FROM` reference;
`tags` are the span's attributes as `{"key", "type", "value"}` (`type`
`string`, `bool`, `int64`, `float64`; arrays and objects as JSON text,
`string`) plus `span.kind` (`server`, `client`, `producer`, `consumer`,
`internal`), `otel.status_code` (`OK`, `ERROR`) and
`otel.status_description` when set, and `error` `true` for an error;
`logs` are the events, `{"timestamp" (µs), "fields": [tags…, {"key":
"event", "type": "string", "value": name}]}`.

### Reading: the ingress's traffic

`GET /api/v1/traffic?environment=<ID>&service=<name>[&service=…]&since=&until=&step=`,
with a reader as above (404 "no such environment" for an environment
it may not read): the requests the ingress answered for
each service of the tenant's environment, counted from its server spans
(source `ingress`, the service as `object`). `ferund`'s
`…/environments/{env}/traffic` reads it (control-plane.md). `since` and
`until` are RFC 3339 or Unix ns, the range `[since, until)`, both
multiples of `step`, a Go duration of whole seconds (`60s`, `15m`); 1 to
128 services, at most 1,440 steps; else 400.

```json
{"services": [
  {"service": "web", "requests": 1234, "errors": 3, "p50Ns": 12300000.0, "p95Ns": 87000000.0,
   "steps": [{"tsNs": "1793581200000000000", "requests": 20, "errors": 0, "p95Ns": 90100000.0}]}
]}
```

A service per one asked, in that order; `steps` holds only the steps
with requests, oldest first; the percentiles are of `duration_ns`,
`null` without requests. A request failed (`errors`) when its status
was 500 or more, or none was written and the client did not go away.
The span's status does not say this (an aborted request is
`STATUS_CODE_ERROR` too), so the sink reads it from the attributes:
`http.response.status_code` in 500 to 999, or `ferun.outcome` `2xx`
without a code of 200 to 299 (the ingress's class of a request that
wrote no status and was neither aborted nor upgraded). An attribute is
not a fast field and takes no range, so the codes are a set of terms
(`IN [500 501 …]`).

`object` is not a fast field either, so the spans cannot be grouped
by service in one search: each service takes two searches of the index,
both with a `date_histogram` of `ts` in the step (aligned to the epoch,
`min_doc_count` 1). The failed requests are counted first, then all
requests with `percentiles` of `duration_ns` (50, 95) overall and 95
per step. The index only grows, so the errors counted first never
outnumber the requests counted after them. Four services are searched
at a time. The percentiles are Quickwit's estimates, within about 1 %.
The index alone is read, so the last commit's spans (5 s) are not in
it yet. The count is best effort: the ingress sends its spans once and
drops a batch the sink did not take (The ingress), and a span stored
twice is counted twice.

## FeCode

FeCode reads with its tenant token (tenant `fecode`) and lets only a
project's members read its environments' traces (as its logs, FeCode
ADR 0009 §12): its backend passes a member's request on to the sink with
the environment's FeRun ID as `environment` set by FeCode, never the
caller's. A trace ID in the Logs tab (a request's, a line's) opens the
trace: its spans as a waterfall, top to bottom, each with its service,
name, duration and attributes, and a link to the lines of the trace.
