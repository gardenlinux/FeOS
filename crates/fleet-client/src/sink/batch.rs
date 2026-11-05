// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! What a stream sends (docs/architecture/sink.md, "The OTLP shape"): its
//! entries as OTLP log records, under the limit of a stream, in batches.

use feos_proto::container_service::{log_entry::Source, LogEntry};
use feos_proto::opentelemetry::proto::collector::logs::v1::ExportLogsServiceRequest;
use feos_proto::opentelemetry::proto::common::v1::{any_value::Value, AnyValue, KeyValue};
use feos_proto::opentelemetry::proto::logs::v1::{LogRecord, ResourceLogs, ScopeLogs};
use feos_proto::opentelemetry::proto::resource::v1::Resource;
use std::collections::{BTreeMap, HashMap};
use std::time::{Duration, SystemTime};
use tokio::time::Instant;

/// A batch's most bytes of text, and records.
pub(crate) const BATCH_BYTES: usize = 1 << 20;
pub(crate) const BATCH_LINES: usize = 1000;
/// How long a batch waits for more once it has a record.
pub(crate) const BATCH_WAIT: Duration = Duration::from_millis(500);
/// A stream's rate and burst, in bytes of text (a token bucket).
pub(crate) const RATE: u64 = 1 << 20;
pub(crate) const BURST: u64 = 8 << 20;
/// What a record costs beside its text, for a batch's bytes.
const RECORD_OVERHEAD: usize = 64;

/// What a stream is, as the sink keys it.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub(crate) struct Key {
    pub(crate) pod: String,
    pub(crate) container: String,
    pub(crate) log_id: String,
}

/// The resource attributes of a stream: the pod's labels as they are, and
/// the node's own, which win over a label of the same name.
pub(crate) fn resource(key: &Key, node: &str, labels: &HashMap<String, String>) -> Resource {
    let mut all: BTreeMap<&str, &str> = labels
        .iter()
        .map(|(k, v)| (k.as_str(), v.as_str()))
        .collect();
    all.insert("ferun.pod", &key.pod);
    all.insert("ferun.container", &key.container);
    all.insert("ferun.log_id", &key.log_id);
    all.insert("ferun.node", node);
    // A pod's requests through its HTTP bindings (ADR 0028), not a
    // container's output.
    if key.container == crate::egress::EGRESS_CONTAINER {
        all.insert("ferun.source", crate::egress::SOURCE);
    }
    Resource {
        attributes: all
            .into_iter()
            .map(|(k, v)| attribute(k, Value::StringValue(v.to_string())))
            .collect(),
        ..Default::default()
    }
}

fn attribute(key: &str, value: Value) -> KeyValue {
    KeyValue {
        key: key.to_string(),
        value: Some(AnyValue { value: Some(value) }),
    }
}

fn unix_nanos(time: SystemTime) -> u64 {
    time.duration_since(SystemTime::UNIX_EPOCH)
        .map_or(0, |since| {
            u64::try_from(since.as_nanos()).unwrap_or(u64::MAX)
        })
}

/// What a line is redacted of: `values` without the empty ones and
/// repeats, the longest first, so that one inside another goes whole.
pub(crate) fn redactions(mut values: Vec<String>) -> Vec<String> {
    values.retain(|value| !value.is_empty());
    values.sort_by(|a, b| b.len().cmp(&a.len()).then_with(|| a.cmp(b)));
    values.dedup();
    values
}

/// Replaces every one of `values` in `text` with `[redacted]`.
pub(crate) fn redact(text: &mut String, values: &[String]) {
    for value in values {
        if text.contains(value.as_str()) {
            *text = text.replace(value.as_str(), "[redacted]");
        }
    }
}

/// A record to send: an entry of the log, or the node's own of lines it
/// dropped.
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct Line {
    pub(crate) offset: u64,
    pub(crate) next_offset: u64,
    pub(crate) time_unix_nano: i64,
    pub(crate) source: Source,
    pub(crate) partial: bool,
    pub(crate) text: String,
    /// Lines and bytes it stands for, when the node dropped them.
    pub(crate) dropped: Option<(u64, u64)>,
    /// Its OTLP severity number; 0 unset (an egress record's, ADR 0028).
    pub(crate) severity: i32,
}

impl Line {
    pub(crate) fn of(entry: LogEntry) -> Self {
        Self {
            offset: entry.offset,
            next_offset: entry.next_offset,
            time_unix_nano: entry.time_unix_nano,
            source: Source::try_from(entry.source).unwrap_or(Source::Unspecified),
            partial: entry.partial,
            text: String::from_utf8(entry.line)
                .unwrap_or_else(|e| String::from_utf8_lossy(e.as_bytes()).into_owned()),
            dropped: None,
            severity: 0,
        }
    }

    /// The lines and bytes of text it stands for.
    fn counts(&self) -> (u64, u64) {
        self.dropped.unwrap_or((1, self.text.len() as u64))
    }

    /// As OTLP has it, read by the node at `observed`.
    pub(crate) fn record(&self, observed: SystemTime) -> LogRecord {
        let mut attributes = vec![
            attribute("log.offset", Value::IntValue(as_i64(self.offset))),
            attribute("log.next_offset", Value::IntValue(as_i64(self.next_offset))),
        ];
        match self.source {
            Source::Stdout => attributes.push(attribute(
                "log.iostream",
                Value::StringValue("stdout".into()),
            )),
            Source::Stderr => attributes.push(attribute(
                "log.iostream",
                Value::StringValue("stderr".into()),
            )),
            Source::Unspecified => {}
        }
        if self.partial {
            attributes.push(attribute("log.partial", Value::BoolValue(true)));
        }
        LogRecord {
            time_unix_nano: u64::try_from(self.time_unix_nano).unwrap_or(0),
            observed_time_unix_nano: unix_nanos(observed),
            severity_number: self.severity,
            body: Some(AnyValue {
                value: Some(Value::StringValue(self.text.clone())),
            }),
            attributes,
            ..Default::default()
        }
    }
}

fn as_i64(offset: u64) -> i64 {
    i64::try_from(offset).unwrap_or(i64::MAX)
}

/// Lines dropped since the last one sent.
#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) struct Dropped {
    lines: u64,
    bytes: u64,
    /// Where the first starts, and its time; where the last one ends.
    offset: u64,
    time_unix_nano: i64,
    next_offset: u64,
}

impl Dropped {
    pub(crate) fn is_empty(&self) -> bool {
        self.lines == 0
    }

    pub(crate) fn add(&mut self, line: &Line) {
        let (lines, bytes) = line.counts();
        if self.lines == 0 {
            self.offset = line.offset;
            self.time_unix_nano = line.time_unix_nano;
        }
        self.lines += lines;
        self.bytes += bytes;
        self.next_offset = self.next_offset.max(line.next_offset);
    }

    /// The node's record of them, at the first one's offset, and empties
    /// it.
    pub(crate) fn take(&mut self) -> Option<Line> {
        if self.is_empty() {
            return None;
        }
        let dropped = std::mem::take(self);
        Some(Line {
            offset: dropped.offset,
            next_offset: dropped.next_offset,
            time_unix_nano: dropped.time_unix_nano,
            source: Source::Stderr,
            partial: false,
            text: format!(
                "[ferun] dropped {} lines ({} bytes)",
                dropped.lines, dropped.bytes
            ),
            dropped: Some((dropped.lines, dropped.bytes)),
            severity: 0,
        })
    }
}

/// A stream's limit: a bucket of bytes, [`RATE`] a second up to [`BURST`].
/// A line it has no room for is dropped; once it has, the record of the
/// lines dropped goes first.
pub(crate) struct Limit {
    rate: u64,
    burst: u64,
    tokens: u64,
    at: Instant,
    pub(crate) dropped: Dropped,
}

impl Limit {
    pub(crate) fn new(rate: u64, burst: u64, now: Instant) -> Self {
        Self {
            rate,
            burst,
            tokens: burst,
            at: now,
            dropped: Dropped::default(),
        }
    }

    fn refill(&mut self, now: Instant) {
        let elapsed = now.saturating_duration_since(self.at);
        let added = (elapsed.as_nanos() * u128::from(self.rate) / 1_000_000_000) as u64;
        if added > 0 {
            self.tokens = self.tokens.saturating_add(added).min(self.burst);
            // What did not make a whole token is kept for the next one.
            let used = Duration::from_nanos(
                (u128::from(added) * 1_000_000_000 / u128::from(self.rate.max(1))) as u64,
            );
            self.at += used;
        }
        if self.tokens == self.burst {
            self.at = now;
        }
    }

    /// What goes out for `line` at `now`: the record of the lines dropped
    /// before it, if any, and it; nothing when it is dropped.
    pub(crate) fn admit(&mut self, line: Line, now: Instant) -> Vec<Line> {
        self.refill(now);
        let cost = line.text.len() as u64;
        if cost > self.tokens {
            self.dropped.add(&line);
            return Vec::new();
        }
        self.tokens -= cost;
        let mut out: Vec<Line> = self.dropped.take().into_iter().collect();
        out.push(line);
        out
    }
}

/// The records a stream sends next, of one resource.
pub(crate) struct Batch {
    pub(crate) lines: Vec<Line>,
    bytes: usize,
    /// When it goes, once it has a record.
    pub(crate) due: Option<Instant>,
}

impl Batch {
    pub(crate) fn new() -> Self {
        Self {
            lines: Vec::new(),
            bytes: 0,
            due: None,
        }
    }

    pub(crate) fn is_empty(&self) -> bool {
        self.lines.is_empty()
    }

    /// Whether it goes now: it holds as much as a batch may.
    pub(crate) fn is_full(&self) -> bool {
        self.lines.len() >= BATCH_LINES || self.bytes >= BATCH_BYTES
    }

    /// Whether `line` would make it hold more than a batch may.
    pub(crate) fn would_overflow(&self, line: &Line) -> bool {
        !self.is_empty() && self.bytes + line.text.len() + RECORD_OVERHEAD > BATCH_BYTES
    }

    pub(crate) fn push(&mut self, line: Line, now: Instant) {
        self.bytes += line.text.len() + RECORD_OVERHEAD;
        self.due.get_or_insert(now + BATCH_WAIT);
        self.lines.push(line);
    }

    /// Its records as an export, of `resource`, read at `observed`.
    pub(crate) fn request(
        &self,
        resource: &Resource,
        observed: SystemTime,
    ) -> ExportLogsServiceRequest {
        ExportLogsServiceRequest {
            resource_logs: vec![ResourceLogs {
                resource: Some(resource.clone()),
                scope_logs: vec![ScopeLogs {
                    log_records: self.lines.iter().map(|l| l.record(observed)).collect(),
                    ..Default::default()
                }],
                ..Default::default()
            }],
        }
    }

    /// Empties it, giving back its lines.
    pub(crate) fn take(&mut self) -> Vec<Line> {
        self.bytes = 0;
        self.due = None;
        std::mem::take(&mut self.lines)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn line(offset: u64, len: usize) -> Line {
        Line {
            offset,
            next_offset: offset + len as u64 + 41,
            time_unix_nano: offset as i64 * 1000,
            source: Source::Stdout,
            partial: false,
            text: "x".repeat(len),
            dropped: None,
            severity: 0,
        }
    }

    fn string(value: &Option<AnyValue>) -> Option<&str> {
        match value.as_ref()?.value.as_ref()? {
            Value::StringValue(s) => Some(s),
            _ => None,
        }
    }

    fn attributes(attributes: &[KeyValue]) -> Vec<(String, String)> {
        attributes
            .iter()
            .map(|kv| {
                let value = match kv.value.as_ref().and_then(|v| v.value.as_ref()) {
                    Some(Value::StringValue(s)) => s.clone(),
                    Some(Value::IntValue(i)) => i.to_string(),
                    Some(Value::BoolValue(b)) => b.to_string(),
                    other => format!("{other:?}"),
                };
                (kv.key.clone(), value)
            })
            .collect()
    }

    fn pairs(pairs: &[(&str, &str)]) -> Vec<(String, String)> {
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    #[test]
    fn a_stream_is_its_labels_and_the_nodes_own() {
        let key = Key {
            pod: "p1".into(),
            container: "main".into(),
            log_id: "abc".into(),
        };
        let labels: HashMap<String, String> = [
            ("ferun.tenant", "acme"),
            ("ferun.replica", "0"),
            ("ferun.pod", "forged"),
        ]
        .into_iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect();
        let resource = resource(&key, "node-1", &labels);
        assert_eq!(
            attributes(&resource.attributes),
            pairs(&[
                ("ferun.container", "main"),
                ("ferun.log_id", "abc"),
                ("ferun.node", "node-1"),
                ("ferun.pod", "p1"),
                ("ferun.replica", "0"),
                ("ferun.tenant", "acme"),
            ])
        );
        // A pod's egress records (ADR 0028): their source, too.
        let egress = Key {
            container: crate::egress::EGRESS_CONTAINER.into(),
            ..key
        };
        let stamped = super::resource(&egress, "node-1", &labels);
        let attributes = attributes(&stamped.attributes);
        assert!(attributes.contains(&("ferun.source".to_string(), "egress".to_string())));
        assert!(attributes.contains(&("ferun.container".to_string(), "feos-authn".to_string())));
    }

    #[test]
    fn an_entry_is_a_log_record() {
        let observed = SystemTime::UNIX_EPOCH + Duration::from_secs(2);
        let entry = LogEntry {
            line: b"bad \xff utf-8".to_vec(),
            source: Source::Stderr as i32,
            offset: 4096,
            next_offset: 4200,
            time_unix_nano: 1_000_000_007,
            partial: true,
            log_id: "abc".into(),
        };
        let record = Line::of(entry).record(observed);
        assert_eq!(record.time_unix_nano, 1_000_000_007);
        assert_eq!(record.observed_time_unix_nano, 2_000_000_000);
        assert_eq!(string(&record.body), Some("bad \u{fffd} utf-8"));
        assert_eq!(
            attributes(&record.attributes),
            pairs(&[
                ("log.offset", "4096"),
                ("log.next_offset", "4200"),
                ("log.iostream", "stderr"),
                ("log.partial", "true"),
            ])
        );
        // A line of before: no time, no stream, not partial.
        let record = Line::of(LogEntry {
            line: b"old".to_vec(),
            offset: 0,
            next_offset: 4,
            ..Default::default()
        })
        .record(observed);
        assert_eq!(record.time_unix_nano, 0);
        assert_eq!(
            attributes(&record.attributes),
            pairs(&[("log.offset", "0"), ("log.next_offset", "4")])
        );
    }

    #[test]
    fn secrets_are_redacted() {
        let values = redactions(vec![
            "hunter2".into(),
            String::new(),
            "user:hunter2".into(),
            "hunter2".into(),
        ]);
        assert_eq!(values, ["user:hunter2", "hunter2"]);
        let mut text = "postgres://user:hunter2@db and hunter2 again".to_string();
        redact(&mut text, &values);
        assert_eq!(text, "postgres://[redacted]@db and [redacted] again");
        let mut clean = "nothing here".to_string();
        redact(&mut clean, &values);
        assert_eq!(clean, "nothing here");
        redact(&mut clean, &[]);
        assert_eq!(clean, "nothing here");
    }

    #[test]
    fn the_limit_drops_and_says_so() {
        let start = Instant::now();
        let mut limit = Limit::new(100, 300, start);
        // The burst goes at once.
        assert_eq!(limit.admit(line(0, 150), start).len(), 1);
        assert_eq!(limit.admit(line(200, 150), start).len(), 1);
        // Then nothing until the bucket has room.
        assert!(limit.admit(line(400, 50), start).is_empty());
        assert!(limit.admit(line(500, 60), start).is_empty());
        let later = start + Duration::from_millis(500);
        assert!(limit.admit(line(600, 70), later).is_empty());
        // 0.6 s later: 60 more bytes, room for a line of 40, after the
        // record of those dropped, at the first one's offset.
        let out = limit.admit(line(700, 40), start + Duration::from_millis(1100));
        assert_eq!(out.len(), 2);
        assert_eq!(out[0].text, "[ferun] dropped 3 lines (180 bytes)");
        assert_eq!(out[0].offset, 400);
        assert_eq!(out[0].next_offset, 600 + 70 + 41);
        assert_eq!(out[0].time_unix_nano, 400_000);
        assert_eq!(out[0].source, Source::Stderr);
        assert_eq!(out[1].offset, 700);
        // Its record is said once.
        assert!(limit.dropped.is_empty());
        // Refilled no further than the burst.
        let much_later = start + Duration::from_secs(3600);
        assert_eq!(limit.admit(line(800, 300), much_later).len(), 1);
        assert!(limit.admit(line(900, 1), much_later).is_empty());
    }

    #[test]
    fn a_refused_batch_counts_as_dropped() {
        let mut dropped = Dropped::default();
        let mut first = Dropped::default();
        first.add(&line(10, 5));
        first.add(&line(60, 7));
        dropped.add(&first.take().unwrap());
        dropped.add(&line(120, 3));
        let record = dropped.take().unwrap();
        assert_eq!(record.text, "[ferun] dropped 3 lines (15 bytes)");
        assert_eq!((record.offset, record.next_offset), (10, 120 + 3 + 41));
        assert!(dropped.take().is_none());
    }

    #[test]
    fn a_batch_is_full_at_its_lines_or_bytes() {
        let now = Instant::now();
        let mut batch = Batch::new();
        assert!(batch.is_empty() && batch.due.is_none());
        batch.push(line(0, 10), now);
        assert_eq!(batch.due, Some(now + BATCH_WAIT));
        for i in 1..BATCH_LINES as u64 {
            assert!(!batch.is_full());
            batch.push(line(i * 100, 10), now + Duration::from_millis(100));
        }
        assert!(batch.is_full());
        assert_eq!(batch.due, Some(now + BATCH_WAIT));
        assert_eq!(batch.take().len(), BATCH_LINES);
        assert!(batch.is_empty() && batch.due.is_none());

        // In bytes: a line that would overflow it goes in the next one, but
        // a single long line always fits an empty batch.
        let big = line(0, 600 << 10);
        assert!(!batch.would_overflow(&big));
        batch.push(big.clone(), now);
        assert!(!batch.is_full());
        assert!(batch.would_overflow(&big));
        batch.push(line(1, 500 << 10), now);
        assert!(batch.is_full());
    }

    #[test]
    fn a_batch_is_one_resource_of_records() {
        let mut batch = Batch::new();
        batch.push(line(0, 3), Instant::now());
        batch.push(line(44, 2), Instant::now());
        let key = Key {
            pod: "p".into(),
            container: "c".into(),
            log_id: "l".into(),
        };
        let request = batch.request(&resource(&key, "n", &HashMap::new()), SystemTime::now());
        assert_eq!(request.resource_logs.len(), 1);
        let logs = &request.resource_logs[0];
        assert_eq!(logs.resource.as_ref().unwrap().attributes.len(), 4);
        assert_eq!(logs.scope_logs.len(), 1);
        let records = &logs.scope_logs[0].log_records;
        assert_eq!(records.len(), 2);
        assert_eq!(string(&records[1].body), Some("xx"));
    }
}
