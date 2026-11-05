// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's events (fleet.proto PublishEvents): pod events and feosd's
//! own, numbered densely from 1 within a start of feosd, kept until the
//! control plane acknowledges them, and sent again after a reconnect, to
//! whichever instance the uplink is on.
//!
//! The buffer is in memory and bounded: what overflows it is dropped and
//! counted, and its numbers are skipped. A buffer on disk, which keeps a
//! start's events across a restart of feosd, is still to come.

use crate::uplink::describe;
use feos_proto::fleet_service::fleet_service_client::FleetServiceClient;
use feos_proto::fleet_service::{event::Body, AuditRecord, Event, EventAck, EventBatch, NodeEvent};
use feos_proto::pod_service::PodEvent;
use log::{debug, info, warn};
use prost::Message;
use std::collections::VecDeque;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;
use tokio::sync::{mpsc, watch, Notify};
use tokio::time::{sleep, timeout};
use tokio_stream::wrappers::ReceiverStream;
use tonic::transport::Channel;
use tonic::{Status, Streaming};

/// The events kept at most.
pub const CAPACITY: usize = 16384;
/// Of them, the pods' request records (audit events of kind
/// [`REQUEST_KIND`]) kept at most: while the control plane lags, a flood of
/// requests drops request records, never another event.
pub const REQUEST_CAPACITY: usize = CAPACITY / 4;
/// The kind of a request record's audit event (egress::audit_kind).
const REQUEST_KIND: &str = "http_request";
/// A batch's limits (fleet.proto).
const BATCH_EVENTS: usize = 512;
const BATCH_BYTES: usize = 1 << 20;
/// How long an acknowledgement may take before the stream counts as lost.
const ACK_TIMEOUT: Duration = Duration::from_secs(30);
const BACKOFF: (Duration, Duration) = (Duration::from_secs(1), Duration::from_secs(30));

/// The node's events since feosd started.
#[derive(Clone)]
pub struct Events {
    buffer: Arc<Mutex<Buffer>>,
    added: Arc<Notify>,
    /// The highest number the control plane acknowledged.
    acked: watch::Sender<u64>,
}

struct Buffer {
    start_id: String,
    /// The number of the next event.
    next: u64,
    /// Not acknowledged yet, in the order of their numbers.
    events: VecDeque<Event>,
    dropped: u64,
    capacity: usize,
    /// The request records among `events`, and how many may be.
    requests: usize,
    request_capacity: usize,
}

impl Buffer {
    fn pop_front(&mut self) -> Option<Event> {
        let event = self.events.pop_front()?;
        if is_request(&event) {
            self.requests -= 1;
        }
        Some(event)
    }
}

fn is_request(event: &Event) -> bool {
    matches!(&event.body, Some(Body::Audit(record)) if record.kind == REQUEST_KIND)
}

impl Events {
    pub(crate) fn new(start_id: &str) -> Self {
        Self::with_capacities(start_id, CAPACITY, REQUEST_CAPACITY)
    }

    #[cfg(test)]
    fn with_capacity(start_id: &str, capacity: usize) -> Self {
        Self::with_capacities(start_id, capacity, (capacity / 4).max(1))
    }

    fn with_capacities(start_id: &str, capacity: usize, request_capacity: usize) -> Self {
        Self {
            buffer: Arc::new(Mutex::new(Buffer {
                start_id: start_id.to_string(),
                next: 1,
                events: VecDeque::new(),
                dropped: 0,
                capacity,
                requests: 0,
                request_capacity,
            })),
            added: Arc::new(Notify::new()),
            acked: watch::Sender::new(0),
        }
    }

    fn buffer(&self) -> MutexGuard<'_, Buffer> {
        self.buffer.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// A pod's state changed.
    pub fn pod(&self, event: PodEvent) {
        self.push(Body::Pod(event));
    }

    /// A record of a pod's feos-authn (ADR 0014): its kind, and its fields
    /// as a JSON object.
    pub fn audit(&self, pod_id: &str, kind: &str, detail_json: &str) {
        self.push(Body::Audit(AuditRecord {
            pod_id: pod_id.to_string(),
            kind: kind.to_string(),
            detail_json: detail_json.to_string(),
        }));
    }

    /// Something of feosd itself: "started", or "stopping".
    pub fn node(&self, kind: &str, message: &str) {
        self.push(Body::Node(NodeEvent {
            kind: kind.to_string(),
            message: message.to_string(),
        }));
    }

    /// Counts `n` events that were lost before they reached the buffer,
    /// such as those a subscriber that fell behind missed.
    pub fn lost(&self, n: u64) {
        let mut buffer = self.buffer();
        buffer.next += n;
        buffer.dropped += n;
    }

    fn push(&self, body: Body) {
        let mut buffer = self.buffer();
        let seq = buffer.next;
        buffer.next += 1;
        let event = Event {
            seq,
            time: Some(std::time::SystemTime::now().into()),
            body: Some(body),
        };
        let request = is_request(&event);
        if request && buffer.requests >= buffer.request_capacity {
            // The newest request record goes, so that the others' order
            // and every other event stay.
            buffer.dropped += 1;
            return;
        }
        if buffer.events.len() >= buffer.capacity {
            buffer.pop_front();
            buffer.dropped += 1;
        }
        if request {
            buffer.requests += 1;
        }
        buffer.events.push_back(event);
        drop(buffer);
        self.added.notify_one();
    }

    /// Waits until the control plane has acknowledged every event so far,
    /// for at most `wait`.
    pub async fn flush(&self, wait: Duration) -> bool {
        let last = self.buffer().next - 1;
        let mut acked = self.acked.subscribe();
        matches!(
            timeout(wait, acked.wait_for(|seq| *seq >= last)).await,
            Ok(Ok(_))
        )
    }

    /// A batch without events, which asks where the control plane is.
    fn ask(&self) -> EventBatch {
        let buffer = self.buffer();
        EventBatch {
            start_id: buffer.start_id.clone(),
            events: Vec::new(),
            dropped: buffer.dropped,
        }
    }

    /// The oldest events not acknowledged, as many as a batch holds; None
    /// without any.
    fn batch(&self) -> Option<EventBatch> {
        let mut buffer = self.buffer();
        loop {
            let mut batch = EventBatch {
                start_id: buffer.start_id.clone(),
                events: Vec::new(),
                dropped: buffer.dropped,
            };
            let mut size = batch.encoded_len();
            for event in &buffer.events {
                let len = prost::encoding::message::encoded_len(2, event);
                if batch.events.len() == BATCH_EVENTS || size + len > BATCH_BYTES {
                    break;
                }
                size += len;
                batch.events.push(event.clone());
            }
            if !batch.events.is_empty() {
                return Some(batch);
            }
            // An event larger than a batch can never be sent.
            buffer.pop_front()?;
            buffer.dropped += 1;
        }
    }

    /// The control plane stored every event up to `seq`.
    fn ack(&self, seq: u64) {
        let mut buffer = self.buffer();
        let seq = seq.min(buffer.next - 1);
        while buffer.events.front().is_some_and(|event| event.seq <= seq) {
            buffer.pop_front();
        }
        drop(buffer);
        self.acked.send_if_modified(|acked| {
            let newer = seq > *acked;
            if newer {
                *acked = seq;
            }
            newer
        });
    }
}

/// Publishes the events on the uplink, wherever it is, until the uplink's
/// sender goes.
pub(crate) async fn publish(events: Events, mut uplink: watch::Receiver<Option<Channel>>) {
    let mut wait = BACKOFF.0;
    let mut failing = false;
    loop {
        if uplink.has_changed().is_err() {
            return;
        }
        let Some(channel) = uplink.borrow_and_update().clone() else {
            if uplink.changed().await.is_err() {
                return;
            }
            continue;
        };
        match stream(&events, channel, &mut uplink, &mut failing).await {
            // The uplink moved: the stream follows it at once.
            Ok(()) => {
                wait = BACKOFF.0;
                continue;
            }
            Err(status) if failing => debug!(
                "Fleet: The events' stream failed again: {}",
                describe(&status)
            ),
            Err(status) => {
                warn!("Fleet: The events' stream failed: {}", describe(&status));
                failing = true;
            }
        }
        tokio::select! {
            _ = sleep(wait) => {}
            _ = uplink.changed() => {}
        }
        wait = (wait * 2).min(BACKOFF.1);
    }
}

/// One stream of batches on `channel`: where the control plane is first,
/// then what it does not have, one batch at a time. Ends when the uplink
/// moves, once the batch in flight is acknowledged.
async fn stream(
    events: &Events,
    channel: Channel,
    uplink: &mut watch::Receiver<Option<Channel>>,
    failing: &mut bool,
) -> Result<(), Status> {
    let (tx, rx) = mpsc::channel(1);
    let ended = || Status::unavailable("the events' stream ended");
    tx.send(events.ask()).await.map_err(|_| ended())?;
    let mut client = FleetServiceClient::new(channel);
    let mut acks = timeout(ACK_TIMEOUT, client.publish_events(ReceiverStream::new(rx)))
        .await
        .map_err(|_| Status::deadline_exceeded("the events' stream did not open"))??
        .into_inner();
    let ack = next_ack(&mut acks).await?;
    events.ack(ack.seq);
    *failing = false;
    info!(
        "Fleet: The control plane has the node's events up to {}",
        ack.seq
    );
    loop {
        let Some(batch) = events.batch() else {
            tokio::select! {
                _ = events.added.notified() => continue,
                _ = uplink.changed() => return Ok(()),
            }
        };
        tx.send(batch).await.map_err(|_| ended())?;
        let ack = next_ack(&mut acks).await?;
        events.ack(ack.seq);
        if uplink.has_changed().unwrap_or(true) {
            return Ok(());
        }
    }
}

async fn next_ack(acks: &mut Streaming<EventAck>) -> Result<EventAck, Status> {
    match timeout(ACK_TIMEOUT, acks.message()).await {
        Ok(Ok(Some(ack))) => Ok(ack),
        Ok(Ok(None)) => Err(Status::unavailable(
            "the control plane ended the events' stream",
        )),
        Ok(Err(status)) => Err(status),
        Err(_) => Err(Status::deadline_exceeded(
            "no acknowledgement of the node's events",
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn seqs(batch: &EventBatch) -> Vec<u64> {
        batch.events.iter().map(|event| event.seq).collect()
    }

    #[test]
    fn events_are_numbered_densely_and_kept_until_acknowledged() {
        let events = Events::new("a6f0c1de-0000-4000-8000-000000000001");
        assert!(events.batch().is_none());
        events.node("started", "feosd 0.6.0");
        events.pod(PodEvent {
            pod_id: "p".into(),
            ..Default::default()
        });
        let batch = events.batch().unwrap();
        assert_eq!(batch.start_id, "a6f0c1de-0000-4000-8000-000000000001");
        assert_eq!(seqs(&batch), [1, 2]);
        assert_eq!(batch.dropped, 0);
        // Not acknowledged: sent again.
        assert_eq!(seqs(&events.batch().unwrap()), [1, 2]);
        events.ack(1);
        assert_eq!(seqs(&events.batch().unwrap()), [2]);
        // An acknowledgement covers every number up to its own, and one
        // beyond what the node has changes nothing.
        events.ack(7);
        assert!(events.batch().is_none());
        assert_eq!(*events.acked.borrow(), 2);
        assert_eq!(events.ask().events.len(), 0);
    }

    #[test]
    fn what_overflows_is_dropped_and_counted() {
        let events = Events::with_capacity("s", 3);
        for _ in 0..5 {
            events.node("started", "");
        }
        let batch = events.batch().unwrap();
        // The oldest went; their numbers are skipped.
        assert_eq!(seqs(&batch), [3, 4, 5]);
        assert_eq!(batch.dropped, 2);
        events.lost(10);
        events.node("stopping", "");
        let batch = events.batch().unwrap();
        assert_eq!(seqs(&batch), [4, 5, 16]);
        assert_eq!(batch.dropped, 13);
    }

    #[test]
    fn a_flood_of_requests_drops_request_records_only() {
        let events = Events::with_capacity("s", 8);
        events.node("started", "");
        for _ in 0..10 {
            events.audit("p", REQUEST_KIND, "{}");
        }
        events.audit("p", "token", "{}");
        events.pod(PodEvent {
            pod_id: "p".into(),
            ..Default::default()
        });
        let batch = events.batch().unwrap();
        // Two of eight may be request records; the rest of them went, and
        // nothing else did.
        assert_eq!(seqs(&batch), [1, 2, 3, 12, 13]);
        assert_eq!(batch.dropped, 8);
        // Acknowledged, they make room again.
        events.ack(13);
        events.audit("p", REQUEST_KIND, "{}");
        assert_eq!(seqs(&events.batch().unwrap()), [14]);
    }

    #[test]
    fn batches_keep_their_limits() {
        let events = Events::new("s");
        for _ in 0..1000 {
            events.node("started", "");
        }
        assert_eq!(events.batch().unwrap().events.len(), BATCH_EVENTS);
        let events = Events::new("s");
        let big = "x".repeat(300 << 10);
        for _ in 0..5 {
            events.node("started", &big);
        }
        let batch = events.batch().unwrap();
        assert_eq!(seqs(&batch), [1, 2, 3]);
        assert!(batch.encoded_len() <= BATCH_BYTES);
        // One event larger than a batch is dropped rather than block the
        // others.
        let events = Events::new("s");
        events.node("started", &"x".repeat(2 << 20));
        events.node("stopping", "");
        let batch = events.batch().unwrap();
        assert_eq!(seqs(&batch), [2]);
        assert_eq!(batch.dropped, 1);
    }

    #[tokio::test]
    async fn a_flush_waits_for_the_acknowledgement() {
        let events = Events::new("s");
        assert!(events.flush(Duration::from_millis(10)).await);
        events.node("stopping", "");
        assert!(!events.flush(Duration::from_millis(10)).await);
        let acker = events.clone();
        tokio::spawn(async move {
            sleep(Duration::from_millis(20)).await;
            acker.ack(1);
        });
        assert!(events.flush(Duration::from_secs(5)).await);
    }
}
