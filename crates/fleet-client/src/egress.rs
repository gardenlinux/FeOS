// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's requests through its HTTP bindings (ADR 0028 §6), as its
//! `feos-authn` records them in its audit file, one JSON object a line
//! (docs/architecture/authn.md, "The audit file"). A request has two
//! records, joined by `spanId`: `http` when its answer's head is known,
//!
//! ```json
//! {"ts":"2026-11-05T12:00:00.123456789Z","type":"http","binding":"erp","mode":"name",
//!  "method":"GET","host":"api.erp.example","path":"/odata/x","status":200,"durationMs":12,
//!  "upstreamAddr":"192.0.2.10:443","credential":"headers","traceId":"<32 hex>",
//!  "spanId":"<16 hex>","parentSpanId":"<16 hex>"|null,"error":{"code","message"}|null}
//! ```
//!
//! and `http_end` once its body or tunnel ended (`status`, `bytesIn`,
//! `bytesOut`, `durationMs` from the request's arrival, `error`). Beside
//! them, `token` (a fetch of an OAuth token) and `bindings` (a set opened).
//!
//! `ts` is when a record was written: an `http`'s request began
//! `durationMs` before it, and so did an `http_end`'s.
//!
//! The node ships the `http` records as the log records of a stream of the
//! pod's, container [`EGRESS_CONTAINER`], whose resource has `ferun.source`
//! [`SOURCE`] (`sink::Shipper`, from the audit file as a log), each at its
//! request's start and with its [`severity`]; each request as a client
//! span ([`Joiner`]) once its `http_end` came, over the path of the pods'
//! own spans (`Traces::export_egress`): one without its `http_end` (a
//! process that was killed) has none; and every record of a type it knows
//! as the pod's audit event (`Events::audit`, of the kind [`audit_kind`]
//! names). Other types are skipped. This is the sink's contract
//! (docs/architecture/sink.md, traces.md).

use feos_proto::opentelemetry::proto::common::v1::{any_value::Value, AnyValue, KeyValue};
use feos_proto::opentelemetry::proto::trace::v1::{
    span::SpanKind, status::StatusCode, Span, Status,
};
use serde::Deserialize;
use std::collections::HashMap;
use std::time::{Duration, SystemTime};

/// The container a pod's egress stream is of, and the resource's
/// `ferun.source`.
pub const EGRESS_CONTAINER: &str = "feos-authn";
pub const SOURCE: &str = "egress";
/// The records' types: a request's head and its end.
pub const HTTP_TYPE: &str = "http";
pub const HTTP_END_TYPE: &str = "http_end";
/// How long a request's span waits for its `http_end`.
pub const END_WAIT: Duration = Duration::from_secs(600);
/// The requests a pod's [`Joiner`] holds at most.
const PENDING: usize = 10_000;

/// The kind of the audit event (feos.fleet.v1.AuditRecord) of a record of
/// `type`; None for a type the node does not publish. A request's end is
/// not published: it completes the request's span ([`Joiner`]), and the
/// control plane counts requests by their heads.
pub fn audit_kind(kind: &str) -> Option<&'static str> {
    match kind {
        HTTP_TYPE => Some("http_request"),
        "token" => Some("token"),
        "bindings" => Some("bindings"),
        _ => None,
    }
}

/// A record of `feos-authn`'s audit file; what the node does not read of
/// it, it ignores, and a field that is null is unset.
#[derive(Clone, Debug, Default, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase", default)]
pub struct Record {
    /// RFC 3339, or Unix milliseconds: when the record was written.
    pub ts: Option<serde_json::Value>,
    #[serde(rename = "type")]
    pub kind: Option<String>,
    pub binding: Option<String>,
    pub mode: Option<String>,
    pub method: Option<String>,
    pub host: Option<String>,
    pub path: Option<String>,
    pub status: Option<u16>,
    pub bytes_in: Option<u64>,
    pub bytes_out: Option<u64>,
    pub duration_ms: Option<f64>,
    pub upstream_addr: Option<String>,
    pub credential: Option<String>,
    pub trace_id: Option<String>,
    pub span_id: Option<String>,
    pub parent_span_id: Option<String>,
    pub error: Option<RecordError>,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq)]
#[serde(default)]
pub struct RecordError {
    pub code: String,
    pub message: String,
}

/// The record of a line.
pub fn record(line: &[u8]) -> Option<Record> {
    serde_json::from_slice(line).ok()
}

/// The record of a line, if it is one of `"type":"http"`.
pub fn http_record(line: &[u8]) -> Option<Record> {
    record(line).filter(|record| record.kind.as_deref() == Some(HTTP_TYPE))
}

/// Whether a line is a record of `"type":"http"`: the log's.
pub fn is_http(line: &[u8]) -> bool {
    http_record(line).is_some()
}

/// Whether a line is a record the node follows: one of a type it
/// publishes as an event, or a request's end, which completes its span.
pub fn is_followed(line: &[u8]) -> bool {
    record(line).is_some_and(|record| {
        record
            .kind
            .as_deref()
            .is_some_and(|kind| kind == HTTP_END_TYPE || audit_kind(kind).is_some())
    })
}

/// A pod's requests whose `http` came and whose `http_end` did not yet,
/// by their span IDs.
#[derive(Default)]
pub struct Joiner {
    pending: HashMap<String, (Record, SystemTime)>,
}

impl Joiner {
    /// Takes a record, read at `observed`: a request's span once both its
    /// records came.
    pub fn take(&mut self, record: Record, observed: SystemTime) -> Option<Span> {
        let span_id = record.span_id.clone()?;
        match record.kind.as_deref() {
            Some(HTTP_TYPE) => {
                if self.pending.len() < PENDING {
                    self.pending.insert(span_id, (record, observed));
                }
                None
            }
            Some(HTTP_END_TYPE) => match self.pending.remove(&span_id) {
                Some((head, _)) => joined_span(&head, &record, observed),
                // Its head was published before this feosd started.
                None => None,
            },
            _ => None,
        }
    }

    /// Forgets the requests that waited for their `http_end` longer than
    /// `wait` at `now`: they have no span. Returns how many.
    pub fn expire(&mut self, now: SystemTime, wait: Duration) -> usize {
        let before = self.pending.len();
        self.pending
            .retain(|_, (_, at)| now.duration_since(*at).map_or(true, |age| age < wait));
        before - self.pending.len()
    }

    pub fn is_empty(&self) -> bool {
        self.pending.is_empty()
    }
}

/// When the record says its answer came; `observed` when it does not.
pub fn time(record: &Record, observed: SystemTime) -> SystemTime {
    match &record.ts {
        Some(serde_json::Value::String(text)) => chrono::DateTime::parse_from_rfc3339(text)
            .ok()
            .and_then(|time| {
                let nanos = u64::try_from(time.timestamp_nanos_opt()?).ok()?;
                SystemTime::UNIX_EPOCH.checked_add(Duration::from_nanos(nanos))
            })
            .unwrap_or(observed),
        Some(serde_json::Value::Number(millis)) => millis
            .as_u64()
            .and_then(|millis| SystemTime::UNIX_EPOCH.checked_add(Duration::from_millis(millis)))
            .unwrap_or(observed),
        _ => observed,
    }
}

/// A record's `durationMs`: none for one that is not there, negative, not
/// finite, or longer than a duration holds. A record is what feos-authn
/// wrote, and nothing it holds may end the node's tail.
fn took(record: &Record) -> Duration {
    record
        .duration_ms
        .and_then(|ms| Duration::try_from_secs_f64(ms / 1000.0).ok())
        .unwrap_or(Duration::ZERO)
}

fn unix_nanos(time: SystemTime) -> u64 {
    time.duration_since(SystemTime::UNIX_EPOCH)
        .map_or(0, |since| {
            u64::try_from(since.as_nanos()).unwrap_or(u64::MAX)
        })
}

fn hex(text: Option<&str>, len: usize) -> Option<Vec<u8>> {
    let text = text?;
    if text.len() != len * 2 {
        return None;
    }
    let bytes: Option<Vec<u8>> = (0..len)
        .map(|i| u8::from_str_radix(text.get(i * 2..i * 2 + 2)?, 16).ok())
        .collect();
    bytes.filter(|bytes| bytes.iter().any(|b| *b != 0))
}

fn string(key: &str, value: &str) -> KeyValue {
    KeyValue {
        key: key.to_string(),
        value: Some(AnyValue {
            value: Some(Value::StringValue(value.to_string())),
        }),
    }
}

fn int(key: &str, value: i64) -> KeyValue {
    KeyValue {
        key: key.to_string(),
        value: Some(AnyValue {
            value: Some(Value::IntValue(value)),
        }),
    }
}

/// A request's span from its `http` alone: until its answer's head.
pub fn head_span(head: &Record, observed: SystemTime) -> Option<Span> {
    span_of(head, None, observed)
}

/// When a record's request began, in Unix nanoseconds: `durationMs`
/// before the record was written.
pub fn start_unix_nanos(record: &Record, observed: SystemTime) -> i64 {
    let written = time(record, observed);
    let start = written.checked_sub(took(record)).unwrap_or(written);
    i64::try_from(unix_nanos(start)).unwrap_or(i64::MAX)
}

/// An `http` record's OTLP severity: error (17) for 5xx or a refusal of
/// `feos-authn`'s own, warn (13) for 4xx, else info (9).
pub fn severity(record: &Record) -> i32 {
    match record.status {
        _ if record.error.is_some() => 17,
        Some(status) if status >= 500 => 17,
        Some(status) if status >= 400 => 13,
        _ => 9,
    }
}

/// A request's span from its `http` and its `http_end`: from its arrival
/// to its end, with its bytes.
pub fn joined_span(head: &Record, end: &Record, observed: SystemTime) -> Option<Span> {
    span_of(head, Some(end), observed)
}

/// The request as a client span: `<METHOD> <binding>`, with its trace
/// context; an error for 5xx or a failure of its own. None for a record
/// without a trace and a span ID.
fn span_of(head: &Record, end: Option<&Record>, observed: SystemTime) -> Option<Span> {
    let trace_id = hex(head.trace_id.as_deref(), 16)?;
    let span_id = hex(head.span_id.as_deref(), 8)?;
    let last = end.unwrap_or(head);
    let finished = time(last, observed);
    let started = finished.checked_sub(took(last)).unwrap_or(finished);
    let text = |value: &Option<String>| value.clone().unwrap_or_default();
    let (method, binding) = (text(&head.method), text(&head.binding));
    let mut attributes = vec![
        string("http.request.method", &method),
        string("url.path", &text(&head.path)),
        string("server.address", &text(&head.host)),
        string("ferun.binding", &binding),
    ];
    let status = last.status.or(head.status);
    if let Some(status) = status {
        attributes.push(int("http.response.status_code", i64::from(status)));
    }
    if let Some(mode) = head.mode.as_deref().filter(|m| !m.is_empty()) {
        attributes.push(string("ferun.binding.mode", mode));
    }
    if let Some(credential) = head.credential.as_deref().filter(|c| !c.is_empty()) {
        attributes.push(string("ferun.credential", credential));
    }
    if let Some(peer) = head.upstream_addr.as_deref().filter(|a| !a.is_empty()) {
        attributes.push(string("network.peer.address", peer));
        let port = peer.parse::<std::net::SocketAddr>().ok().map(|a| a.port());
        if let Some(port) = port.filter(|port| *port != 443) {
            attributes.push(int("server.port", i64::from(port)));
        }
    }
    if let Some(end) = end {
        if let Some(bytes) = end.bytes_in {
            attributes.push(int(
                "http.request.body.size",
                i64::try_from(bytes).unwrap_or(i64::MAX),
            ));
        }
        if let Some(bytes) = end.bytes_out {
            attributes.push(int(
                "http.response.body.size",
                i64::try_from(bytes).unwrap_or(i64::MAX),
            ));
        }
    }
    // A refusal of feos-authn's own; an end's error (`aborted` and the
    // like) is the end's, not the request's status.
    let refusal = head.error.as_ref();
    if let Some(refusal) = refusal {
        attributes.push(string("ferun.error.code", &refusal.code));
    }
    if let Some(ended) = end.and_then(|end| end.error.as_ref()) {
        attributes.push(string("ferun.end.error", &ended.code));
    }
    let failed = refusal.is_some() || status.is_some_and(|status| status >= 500);
    let status = failed.then(|| Status {
        code: StatusCode::Error as i32,
        message: refusal.map(|r| r.code.clone()).unwrap_or_default(),
    });
    let name = match binding.is_empty() {
        true => method,
        false => format!("{method} {binding}"),
    };
    Some(Span {
        trace_id,
        span_id,
        parent_span_id: hex(head.parent_span_id.as_deref(), 8).unwrap_or_default(),
        name,
        kind: SpanKind::Client as i32,
        start_time_unix_nano: unix_nanos(started),
        end_time_unix_nano: unix_nanos(finished),
        attributes,
        status,
        ..Default::default()
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    const HEAD: &str = r#"{"ts":"2026-11-05T10:00:01Z","type":"http","binding":"erp","mode":"name",
        "method":"GET","host":"api.erp.example","path":"/odata/v4/orders","status":200,
        "durationMs":12,"upstreamAddr":"203.0.113.7:443",
        "credential":"oauth2","traceId":"0af7651916cd43dd8448eb211c80319c",
        "spanId":"b7ad6b7169203331","parentSpanId":"00f067aa0ba902b7","error":null,
        "later":"ignored"}"#;
    const END: &str = r#"{"ts":"2026-11-05T10:00:02Z","type":"http_end","binding":"erp",
        "traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331",
        "status":200,"bytesIn":0,"bytesOut":5120,"durationMs":1250,"error":null}"#;

    fn line(text: &str) -> Vec<u8> {
        text.replace('\n', "").into_bytes()
    }

    fn attribute(span: &Span, key: &str) -> Option<Value> {
        span.attributes
            .iter()
            .find(|kv| kv.key == key)
            .and_then(|kv| kv.value.clone())
            .and_then(|v| v.value)
    }

    #[test]
    fn a_requests_two_records_become_one_client_span_of_its_trace() {
        let mut joiner = Joiner::default();
        let head = record(&line(HEAD)).unwrap();
        assert!(joiner.take(head, SystemTime::UNIX_EPOCH).is_none());
        assert!(!joiner.is_empty());
        let span = joiner
            .take(record(&line(END)).unwrap(), SystemTime::UNIX_EPOCH)
            .unwrap();
        assert!(joiner.is_empty());
        assert_eq!(span.name, "GET erp");
        assert_eq!(span.kind, SpanKind::Client as i32);
        assert_eq!(span.trace_id[0], 0x0a);
        assert_eq!(
            span.span_id,
            [0xb7, 0xad, 0x6b, 0x71, 0x69, 0x20, 0x33, 0x31]
        );
        assert_eq!(span.parent_span_id.len(), 8);
        // 2026-11-05T10:00:02Z, its end; its arrival 1250 ms before.
        assert_eq!(span.end_time_unix_nano, 1_793_872_802_000_000_000);
        assert_eq!(
            span.end_time_unix_nano - span.start_time_unix_nano,
            1_250_000_000
        );
        assert_eq!(
            attribute(&span, "http.request.method"),
            Some(Value::StringValue("GET".into()))
        );
        assert_eq!(
            attribute(&span, "url.path"),
            Some(Value::StringValue("/odata/v4/orders".into()))
        );
        assert_eq!(
            attribute(&span, "server.address"),
            Some(Value::StringValue("api.erp.example".into()))
        );
        assert_eq!(
            attribute(&span, "http.response.status_code"),
            Some(Value::IntValue(200))
        );
        assert_eq!(
            attribute(&span, "ferun.binding"),
            Some(Value::StringValue("erp".into()))
        );
        assert_eq!(
            attribute(&span, "http.response.body.size"),
            Some(Value::IntValue(5120))
        );
        assert_eq!(span.status, None);
    }

    /// What a record says is feos-authn's: no duration or time in it
    /// ends the node's tail.
    #[test]
    fn a_record_of_any_duration_or_time_is_taken() {
        let observed = SystemTime::UNIX_EPOCH + Duration::from_secs(1_793_872_802);
        for ms in [
            "1e300",
            "1.7976931348623157e308",
            "-5",
            "18446744073709551615000",
            "0",
        ] {
            for ts in [
                r#""2026-11-05T10:00:01Z""#,
                "18446744073709551615",
                r#""+262142-12-31T23:59:59Z""#,
                "null",
            ] {
                let head = format!(
                    r#"{{"ts":{ts},"type":"http","traceId":"0af7651916cd43dd8448eb211c80319c",
                    "spanId":"b7ad6b7169203331","durationMs":{ms}}}"#
                );
                let end = head.replace(r#""type":"http""#, r#""type":"http_end""#);
                let (head, end) = (record(&line(&head)).unwrap(), record(&line(&end)).unwrap());
                let start = start_unix_nanos(&head, observed);
                assert!(start >= 0, "{ms} {ts}");
                let mut joiner = Joiner::default();
                joiner.take(head, observed);
                let span = joiner.take(end, observed).unwrap();
                assert!(
                    span.start_time_unix_nano <= span.end_time_unix_nano,
                    "{ms} {ts}"
                );
            }
        }
        // A duration too long for one counts as none.
        let head = record(&line(
            &HEAD.replace(r#""durationMs":12"#, r#""durationMs":1e300"#),
        ));
        let head = head.unwrap();
        let written = time(&head, observed);
        assert_eq!(
            start_unix_nanos(&head, observed),
            i64::try_from(unix_nanos(written)).unwrap()
        );
    }

    #[test]
    fn a_request_without_its_end_has_no_span() {
        let mut joiner = Joiner::default();
        let at = SystemTime::UNIX_EPOCH + Duration::from_secs(1_000);
        joiner.take(record(&line(HEAD)).unwrap(), at);
        assert_eq!(joiner.expire(at + Duration::from_secs(1), END_WAIT), 0);
        assert_eq!(joiner.expire(at + END_WAIT, END_WAIT), 1);
        assert!(joiner.is_empty());
        // An end whose head this feosd never saw has none either.
        assert!(joiner.take(record(&line(END)).unwrap(), at).is_none());
    }

    #[test]
    fn a_log_record_is_at_its_requests_start_with_its_severity() {
        let head = record(&line(HEAD)).unwrap();
        // Written at 10:00:01, 12 ms after the request came.
        assert_eq!(
            start_unix_nanos(&head, SystemTime::now()),
            1_793_872_801_000_000_000 - 12_000_000
        );
        assert_eq!(severity(&head), 9);
        let with = |status: u16, error: bool| Record {
            status: Some(status),
            error: error.then(RecordError::default),
            ..Default::default()
        };
        assert_eq!(severity(&with(404, false)), 13);
        assert_eq!(severity(&with(502, false)), 17);
        assert_eq!(severity(&with(403, true)), 17);
    }

    #[test]
    fn failures_are_errors_and_other_types_are_skipped() {
        let failed = br#"{"type":"http","binding":"pay","method":"POST","status":503,
            "traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331"}"#;
        let failed = head_span(&http_record(failed).unwrap(), SystemTime::now()).unwrap();
        assert_eq!(failed.status.unwrap().code, StatusCode::Error as i32);
        assert!(failed.parent_span_id.is_empty());

        let refused = br#"{"type":"http","binding":null,"mode":null,"method":"GET","status":404,
            "host":"nope","upstreamAddr":null,
            "error":{"code":"unknown_binding","message":"no binding nope"},
            "traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331"}"#;
        let refused = head_span(&http_record(refused).unwrap(), SystemTime::now()).unwrap();
        assert_eq!(refused.name, "GET");
        assert_eq!(
            attribute(&refused, "ferun.error.code"),
            Some(Value::StringValue("unknown_binding".into()))
        );
        let status = refused.status.unwrap();
        assert_eq!(status.code, StatusCode::Error as i32);
        assert_eq!(status.message, "unknown_binding");

        // A body that broke off.
        let mut joiner = Joiner::default();
        joiner.take(record(&line(HEAD)).unwrap(), SystemTime::now());
        let aborted = line(&END.replace(
            "\"error\":null",
            "\"error\":{\"code\":\"aborted\",\"message\":\"the pod went away\"}",
        ));
        let span = joiner
            .take(record(&aborted).unwrap(), SystemTime::now())
            .unwrap();
        assert_eq!(
            attribute(&span, "ferun.end.error"),
            Some(Value::StringValue("aborted".into()))
        );
        assert_eq!(span.status, None, "the upstream answered");

        assert!(is_http(&line(HEAD)) && !is_http(&line(END)));
        // A request's end is followed for its span, not published.
        assert_eq!(audit_kind(HTTP_TYPE), Some("http_request"));
        assert_eq!(audit_kind(HTTP_END_TYPE), None);
        for published in [
            line(HEAD),
            line(END),
            br#"{"type":"token","binding":"pay","reason":"first_use"}"#.to_vec(),
            br#"{"type":"bindings","version":"3","http":["erp"],"invalid":[]}"#.to_vec(),
        ] {
            assert!(is_followed(&published));
        }
        for skipped in [
            &br#"{"type":"later"}"#[..],
            br#"not json"#,
            br#"{"binding":"pay"}"#,
        ] {
            assert!(
                !is_followed(skipped),
                "{}",
                String::from_utf8_lossy(skipped)
            );
        }
        // Without its trace context, a request has no span.
        let bare = http_record(
            br#"{"type":"http","traceId":"00000000000000000000000000000000","spanId":"b7ad6b7169203331"}"#,
        )
        .unwrap();
        assert!(head_span(&bare, SystemTime::now()).is_none());
    }

    #[test]
    fn a_time_is_rfc_3339_or_milliseconds() {
        let observed = SystemTime::UNIX_EPOCH + Duration::from_secs(9);
        let at = |ts: serde_json::Value| {
            time(
                &Record {
                    ts: Some(ts),
                    ..Default::default()
                },
                observed,
            )
        };
        assert_eq!(
            at(serde_json::json!(1500)),
            SystemTime::UNIX_EPOCH + Duration::from_millis(1500)
        );
        assert_eq!(
            at(serde_json::json!("1970-01-01T00:00:02.5Z")),
            SystemTime::UNIX_EPOCH + Duration::from_millis(2500)
        );
        assert_eq!(at(serde_json::json!("yesterday")), observed);
        assert_eq!(time(&Record::default(), observed), observed);
    }
}
