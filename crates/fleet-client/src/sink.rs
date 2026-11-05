// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's log shipper (ADR 0026, docs/architecture/sink.md): the
//! containers' logs of the running pods that have log labels go to the
//! sink the control plane names (`TelemetryResponse.sink_endpoint`), over
//! OTLP's LogsService, with the node's certificate.
//!
//! A stream is one log of one container: (pod, container, the guest's ID
//! of the log). For each, the shipper asks the sink where it is
//! (`SinkService/Positions`), follows the log in the pod's guest from
//! there (`StreamContainerLogs` with `from_offset`), and exports batches
//! ([`batch`]) under the stream's limit. A batch is kept until the sink
//! takes it, and sent again after a backoff; meanwhile the stream reads
//! nothing more: what the container writes waits in its log, in the
//! guest, and the shipper's memory is a batch and a few entries a stream.
//! A tenant over its quota (`RESOURCE_EXHAUSTED`) has its batch dropped
//! and counted as the limit's drops are.
//!
//! Before a pod pauses, hibernates or is deleted, `drain` sends what its
//! streams read until their logs are quiet; the pod service bounds it.
//! A stream whose pod no longer runs is let go of, not cut off: it sends
//! what it read and what the guest still sends it, for [`FINAL_SEND`] at
//! most. After a delete, the pod's streams end (`SinkService/EndStreams`),
//! also those let go of before, asked again until the sink answers. Nothing here touches a pod: it
//! only reads its logs.

mod batch;

use crate::tls;
use batch::{Batch, Key, Limit, Line};
use feos_proto::container_service::{LogEntry, StreamContainerLogsRequest};
use feos_proto::opentelemetry::proto::collector::logs::v1::logs_service_client::LogsServiceClient;
use feos_proto::opentelemetry::proto::resource::v1::Resource;
use feos_proto::sink_service::sink_service_client::SinkServiceClient;
use feos_proto::sink_service::{EndStreamsRequest, PositionsRequest, StreamKey};
use hyper_util::rt::TokioIo;
use log::{debug, info, warn};
use rustls::pki_types::ServerName;
use rustls::ClientConfig;
use std::collections::{HashMap, HashSet, VecDeque};
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, SystemTime};
use tokio::sync::{mpsc, oneshot, watch, Notify};
use tokio::task::JoinHandle;
use tokio::time::{sleep, sleep_until, timeout, Instant};
use tokio_stream::{Stream, StreamExt};
use tonic::transport::{Channel, Endpoint, Uri};
use tonic::{Code, Status};
use tower::service_fn;

/// How often the pods are looked at without an event.
const RESCAN: Duration = Duration::from_secs(10);
/// The wait after a failure, doubled up to the second.
const BACKOFF: (Duration, Duration) = (Duration::from_secs(1), Duration::from_secs(30));
/// The deadline of a call to the sink.
const CALL_TIMEOUT: Duration = Duration::from_secs(30);
/// How long a drained stream's log has to be quiet: the guest looks for
/// more every 200 ms.
const DRAIN_QUIET: Duration = Duration::from_millis(400);
/// How long a stream whose guest predates ADR 0026 waits before it looks
/// again.
const OLD_GUEST_WAIT: Duration = Duration::from_secs(3600);
/// The most streams a call names (sink.proto), and that wait to be ended.
const KEYS_PER_CALL: usize = 1000;
const MAX_ENDS: usize = 10_000;
/// A drain's requests a stream holds.
const DRAINS: usize = 4;
/// How long a stream that is let go of may take to send what it read: the
/// rest of its log that the guest already sent, and its batch.
const FINAL_SEND: Duration = Duration::from_secs(10);

/// Where the sink is, and how the node reaches it.
#[derive(Clone)]
pub struct Target {
    /// `host:port` of its ingest.
    pub endpoint: String,
    /// The node's certificate, and the sink checked for its URI
    /// (`tls::sink_config`).
    pub config: Arc<ClientConfig>,
    /// The node's name, `ferun.node`.
    pub node: String,
}

/// A pod whose containers' logs are shipped.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ShippedPod {
    pub id: String,
    pub labels: HashMap<String, String>,
    /// The values its lines are redacted of before they are sent: secrets,
    /// never sent nor logged.
    pub redact: Vec<String>,
    /// Its containers' names, as its guest knows them.
    pub containers: Vec<String>,
}

/// What a pod's streams send with their lines, and leave out of them.
struct Labels {
    labels: HashMap<String, String>,
    /// The longest first.
    redact: Vec<String>,
}

/// A container's log, as its guest streams it.
pub type LogStream = Pin<Box<dyn Stream<Item = Result<LogEntry, Status>> + Send>>;

/// What the node has of its pods' logs.
pub trait Logs: Send + Sync + 'static {
    /// The running pods that have log labels. It may block briefly, on the
    /// lock of the pods' records; it runs on the blocking pool.
    fn pods(&self) -> Vec<ShippedPod>;
    /// A container's log in a pod's guest.
    fn logs(
        &self,
        pod_id: &str,
        request: StreamContainerLogsRequest,
    ) -> impl Future<Output = Result<LogStream, Status>> + Send;
}

/// The shipper; its tasks run on the runtime it was started on, for as
/// long as the process does.
#[derive(Clone)]
pub struct Shipper {
    inner: Arc<Inner>,
}

struct Inner {
    target: watch::Receiver<Option<Target>>,
    /// The channel to the sink, for the target it was made for.
    channel: Mutex<Option<(String, Arc<ClientConfig>, Channel)>>,
    streams: Mutex<HashMap<(String, String), Handle>>,
    /// The streams let go of while their pods do not run, until the pods
    /// are deleted, which ends them, or run again.
    idle: Mutex<HashMap<(String, String), Handle>>,
    /// The streams to end, and a nudge for whoever ends them.
    ends: Mutex<VecDeque<Key>>,
    ended: Notify,
    /// A nudge to look at the pods now.
    rescan: Notify,
}

/// A stream's task, as the shipper holds it. Dropped, it lets the task
/// go: the task sends what it read, for [`FINAL_SEND`] at most, and ends.
struct Handle {
    task: JoinHandle<()>,
    /// Taken and dropped: the stream stops.
    stop: Option<oneshot::Sender<()>>,
    state: Arc<State>,
    drains: mpsc::Sender<oneshot::Sender<()>>,
}

/// What a stream's task says of itself.
#[derive(Default)]
struct State {
    /// The ID of the log it ships, once known.
    log_id: Mutex<String>,
    /// It follows its log: a drain waits for it.
    following: AtomicBool,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(|e| e.into_inner())
}

impl Shipper {
    /// Ships the logs of `logs` to the sink `target` names, while it names
    /// one; none: nothing is shipped.
    pub fn start<L: Logs>(logs: L, target: watch::Receiver<Option<Target>>) -> Self {
        let shipper = Self {
            inner: Arc::new(Inner {
                target,
                channel: Mutex::new(None),
                streams: Mutex::new(HashMap::new()),
                idle: Mutex::new(HashMap::new()),
                ends: Mutex::new(VecDeque::new()),
                ended: Notify::new(),
                rescan: Notify::new(),
            }),
        };
        tokio::spawn(supervise(shipper.inner.clone(), Arc::new(logs)));
        tokio::spawn(end_streams(shipper.inner.clone()));
        shipper
    }

    /// The pods may have changed: they are looked at now.
    pub fn changed(&self) {
        self.inner.rescan.notify_one();
    }

    /// Sends what the containers of pod `pod_id` wrote, until their logs
    /// are quiet; for the streams that follow their logs now. Its caller
    /// bounds it.
    pub async fn drain(&self, pod_id: &str) {
        let waits: Vec<oneshot::Receiver<()>> = lock(&self.inner.streams)
            .iter()
            .filter(|((pod, _), handle)| {
                pod == pod_id && handle.state.following.load(Ordering::Relaxed)
            })
            .filter_map(|(_, handle)| {
                let (done, wait) = oneshot::channel();
                handle.drains.try_send(done).ok().map(|()| wait)
            })
            .collect();
        for wait in waits {
            let _ = wait.await;
        }
    }

    /// The channel to the sink the control plane names now, the logs' own,
    /// and the node's name; `UNAVAILABLE` while it names none.
    pub(crate) fn sink(&self) -> Result<(Channel, String), Status> {
        let target = self
            .inner
            .target()
            .ok_or_else(|| Status::unavailable("the control plane names no sink"))?;
        Ok((self.inner.channel(&target)?, target.node))
    }

    /// Pod `pod_id` is deleted: its streams send what they read, and end.
    pub fn deleted(&self, pod_id: &str) {
        let mut gone = Vec::new();
        for map in [&self.inner.streams, &self.inner.idle] {
            let mut streams = lock(map);
            let keys: Vec<_> = streams.keys().filter(|k| k.0 == pod_id).cloned().collect();
            for key in keys {
                if let Some(handle) = streams.remove(&key) {
                    gone.push((key, handle));
                }
            }
        }
        if gone.is_empty() {
            return;
        }
        let inner = self.inner.clone();
        tokio::spawn(async move {
            let mut ended = Vec::new();
            for ((pod, container), handle) in gone {
                let Handle {
                    mut task,
                    stop,
                    state,
                    ..
                } = handle;
                drop(stop);
                if timeout(FINAL_SEND + BACKOFF.0, &mut task).await.is_err() {
                    task.abort();
                }
                let log_id = lock(&state.log_id).clone();
                if !log_id.is_empty() {
                    ended.push(Key {
                        pod,
                        container,
                        log_id,
                    });
                }
            }
            inner.end(ended);
        });
    }
}

impl Inner {
    /// Ends the streams `ended` at the sink, behind those before.
    fn end(&self, ended: Vec<Key>) {
        if ended.is_empty() {
            return;
        }
        let mut ends = lock(&self.ends);
        ends.extend(ended);
        while ends.len() > MAX_ENDS {
            if let Some(key) = ends.pop_front() {
                warn!(
                    "Logs: Too many streams wait to be ended; {}/{} is not",
                    key.pod, key.container
                );
            }
        }
        drop(ends);
        self.ended.notify_one();
    }

    fn target(&self) -> Option<Target> {
        self.target.borrow().clone()
    }

    /// The channel to `target`'s sink: the one there is, unless the target
    /// changed.
    fn channel(&self, target: &Target) -> Result<Channel, Status> {
        let mut cached = lock(&self.channel);
        if let Some((endpoint, config, channel)) = cached.as_ref()
            && *endpoint == target.endpoint
            && Arc::ptr_eq(config, &target.config)
        {
            return Ok(channel.clone());
        }
        let channel = connect(target).map_err(Status::unavailable)?;
        *cached = Some((
            target.endpoint.clone(),
            target.config.clone(),
            channel.clone(),
        ));
        Ok(channel)
    }
}

/// A channel to the sink at `target`, which connects when it is first used,
/// and again after it lost its connection, to whichever address the
/// endpoint resolves to then.
fn connect(target: &Target) -> Result<Channel, String> {
    let endpoint = target.endpoint.clone();
    let (host, port) = endpoint
        .rsplit_once(':')
        .ok_or_else(|| format!("the sink's endpoint {endpoint:?} is not HOST:PORT"))?;
    if port.parse::<u16>().is_err() {
        return Err(format!("the sink's endpoint {endpoint:?} is not HOST:PORT"));
    }
    let host = host.trim_start_matches('[').trim_end_matches(']');
    // The sink is checked for its URI, not for this name.
    let name = ServerName::try_from(host.to_string())
        .map_err(|e| format!("the sink's endpoint {endpoint:?}: {e}"))?;
    let config = target.config.clone();
    Ok(Endpoint::from_shared(format!("https://{endpoint}"))
        .map_err(|e| format!("the sink's endpoint {endpoint:?}: {e}"))?
        .http2_keep_alive_interval(Duration::from_secs(10))
        .keep_alive_timeout(Duration::from_secs(5))
        .connect_with_connector_lazy(service_fn(move |_: Uri| {
            let (address, name, config) = (endpoint.clone(), name.clone(), config.clone());
            async move {
                tls::connect(&address, &name, config)
                    .await
                    .map(TokioIo::new)
            }
        })))
}

/// Keeps a stream for every container of every pod `logs` ships, while
/// there is a sink.
async fn supervise<L: Logs>(inner: Arc<Inner>, logs: Arc<L>) {
    let mut target = inner.target.clone();
    loop {
        let shipping = target.borrow_and_update().is_some();
        let pods = match shipping {
            true => {
                let logs = logs.clone();
                tokio::task::spawn_blocking(move || logs.pods())
                    .await
                    .unwrap_or_default()
            }
            false => Vec::new(),
        };
        let wanted: HashMap<(String, String), Arc<Labels>> = pods
            .into_iter()
            .filter(|pod| !pod.labels.is_empty())
            .flat_map(|pod| {
                let labels = Arc::new(Labels {
                    labels: pod.labels,
                    redact: batch::redactions(pod.redact),
                });
                pod.containers
                    .into_iter()
                    .map(move |container| ((pod.id.clone(), container), labels.clone()))
            })
            .collect();
        {
            let mut streams = lock(&inner.streams);
            // One let go of sends what it read, and ends; it is kept to be
            // ended at the sink with its pod.
            let gone: Vec<_> = streams
                .keys()
                .filter(|key| !wanted.contains_key(*key))
                .cloned()
                .collect();
            let mut idle = lock(&inner.idle);
            for key in gone {
                if let Some(mut handle) = streams.remove(&key) {
                    drop(handle.stop.take());
                    idle.insert(key, handle);
                }
            }
            streams.retain(|_, handle| !handle.task.is_finished());
            for (key, labels) in wanted {
                if streams.contains_key(&key) {
                    continue;
                }
                // Its pod runs again: a stream of its own takes over.
                idle.remove(&key);
                let state = Arc::new(State::default());
                let (drains, requests) = mpsc::channel(DRAINS);
                let (stop, stopped) = oneshot::channel();
                let task = tokio::spawn(follow(
                    inner.clone(),
                    logs.clone(),
                    key.clone(),
                    labels,
                    state.clone(),
                    requests,
                    stopped,
                ));
                let handle = Handle {
                    task,
                    stop: Some(stop),
                    state,
                    drains,
                };
                streams.insert(key, handle);
            }
        }
        tokio::select! {
            () = sleep(RESCAN) => {}
            () = inner.rescan.notified() => {}
            changed = target.changed() => {
                if changed.is_err() {
                    // The control plane's side is gone: nothing more to ship.
                    for map in [&inner.streams, &inner.idle] {
                        for (_, handle) in lock(map).drain() {
                            handle.task.abort();
                        }
                    }
                    return;
                }
            }
        }
    }
}

/// How a stream's follow ended.
enum Done {
    /// Its log ended: the container is gone, or another log took its
    /// place. It starts again.
    Ended,
    /// The sink has more of a log than where it started: it starts again
    /// from there.
    Again,
    /// The guest predates ADR 0026: its logs have no IDs or offsets.
    OldGuest,
    /// There is no sink.
    NoTarget,
    /// It was let go of, and sent what it read.
    Stopped,
}

/// Ships one container's log, starting again whenever it ends or fails.
async fn follow<L: Logs>(
    inner: Arc<Inner>,
    logs: Arc<L>,
    (pod, container): (String, String),
    labels: Arc<Labels>,
    state: Arc<State>,
    mut drains: mpsc::Receiver<oneshot::Sender<()>>,
    stop: oneshot::Receiver<()>,
) {
    let mut stream = Shipping {
        inner,
        pod,
        container,
        labels,
        state,
        warned: false,
        stop: Some(stop),
        ending: None,
    };
    let mut wait = BACKOFF.0;
    loop {
        let done = stream.ship(logs.as_ref(), &mut drains).await;
        stream.state.following.store(false, Ordering::Relaxed);
        // Nothing to wait for while it does not follow.
        while let Ok(done) = drains.try_recv() {
            let _ = done.send(());
        }
        if stream.ending.is_some() {
            return;
        }
        match done {
            Ok(Done::Again) => wait = BACKOFF.0,
            Ok(Done::Stopped) => return,
            Ok(Done::Ended) => {
                wait = BACKOFF.0;
                if !stream.pause(BACKOFF.0).await {
                    return;
                }
            }
            Ok(Done::OldGuest) => {
                info!(
                    "Logs: The guest of pod {} predates ADR 0026; {}'s log is not shipped",
                    stream.pod, stream.container
                );
                if !stream.pause(OLD_GUEST_WAIT).await {
                    return;
                }
            }
            Ok(Done::NoTarget) => {
                let mut target = stream.inner.target.clone();
                while target.borrow_and_update().is_none() {
                    tokio::select! {
                        changed = target.changed() => {
                            if changed.is_err() {
                                return;
                            }
                        }
                        () = told_to_stop(&mut stream.stop) => return,
                    }
                }
            }
            Err(status) => {
                // Said once at warn, until shipping works again: a sink
                // the node cannot reach must not stay silent.
                if stream.warned {
                    debug!(
                        "Logs: Shipping {}/{} failed: {}; again in {}s",
                        stream.pod,
                        stream.container,
                        status.message(),
                        wait.as_secs()
                    );
                } else {
                    warn!(
                        "Logs: Shipping {}/{} failed: {}; trying again",
                        stream.pod,
                        stream.container,
                        status.message()
                    );
                    stream.warned = true;
                }
                if !stream.pause(wait).await {
                    return;
                }
                wait = (wait * 2).min(BACKOFF.1);
            }
        }
    }
}

/// One container's log, as it is shipped.
struct Shipping {
    inner: Arc<Inner>,
    pod: String,
    container: String,
    labels: Arc<Labels>,
    state: Arc<State>,
    /// A failure was said at warn, until the sink answers again.
    warned: bool,
    /// Resolves when the stream is let go of; none once it was.
    stop: Option<oneshot::Receiver<()>>,
    /// Since it was let go of: until when it may send what it read.
    ending: Option<Instant>,
}

/// Waits until the stream is let go of; for ever once it was.
async fn told_to_stop(stop: &mut Option<oneshot::Receiver<()>>) {
    match stop.as_mut() {
        Some(stopped) => {
            let _ = stopped.await;
            *stop = None;
        }
        None => std::future::pending().await,
    }
}

impl Shipping {
    /// Waits `wait`; false when the stream is let go of meanwhile.
    async fn pause(&mut self, wait: Duration) -> bool {
        tokio::select! {
            () = sleep(wait) => true,
            () = told_to_stop(&mut self.stop) => false,
        }
    }

    /// The stream is let go of: what it read goes, until [`FINAL_SEND`].
    fn let_go(&mut self) {
        self.ending.get_or_insert(Instant::now() + FINAL_SEND);
    }

    fn key(&self, log_id: &str) -> Key {
        Key {
            pod: self.pod.clone(),
            container: self.container.clone(),
            log_id: log_id.to_string(),
        }
    }

    fn request(&self) -> StreamContainerLogsRequest {
        StreamContainerLogsRequest {
            container_id: self.container.clone(),
            ..Default::default()
        }
    }

    /// Where the sink is with the stream of log `log_id`.
    async fn position(&self, log_id: &str) -> Result<u64, Status> {
        let target = self
            .inner
            .target()
            .ok_or_else(|| Status::unavailable("no sink"))?;
        let mut sink = SinkServiceClient::new(self.inner.channel(&target)?);
        let key = self.key(log_id);
        let request = PositionsRequest {
            streams: vec![StreamKey {
                pod: key.pod,
                container: key.container,
                log_id: key.log_id,
            }],
        };
        let answer = timeout(CALL_TIMEOUT, sink.positions(request))
            .await
            .map_err(|_| Status::deadline_exceeded("Positions timed out"))??;
        Ok(answer
            .into_inner()
            .next_offsets
            .first()
            .copied()
            .unwrap_or(0))
    }

    /// Follows the log from where the sink is with it, until it ends.
    async fn ship<L: Logs>(
        &mut self,
        logs: &L,
        drains: &mut mpsc::Receiver<oneshot::Sender<()>>,
    ) -> Result<Done, Status> {
        if self.ending.is_some() {
            return Ok(Done::Stopped);
        }
        let Some(target) = self.inner.target() else {
            return Ok(Done::NoTarget);
        };
        // Which log there is: its last entry says.
        let mut probe = logs
            .logs(
                &self.pod,
                StreamContainerLogsRequest {
                    tail_lines: 1,
                    ..self.request()
                },
            )
            .await?;
        let last = probe.next().await.transpose()?;
        drop(probe);
        let mut log_id = match last {
            Some(entry) if entry.log_id.is_empty() => return Ok(Done::OldGuest),
            Some(entry) => entry.log_id,
            None => String::new(),
        };
        let request = if log_id.is_empty() {
            // An empty log, or none yet: from its start.
            StreamContainerLogsRequest {
                follow: true,
                tail: true,
                ..self.request()
            }
        } else {
            let from_offset = self.position(&log_id).await?;
            if self.warned {
                info!(
                    "Logs: The sink is reached again for {}/{}",
                    self.pod, self.container
                );
                self.warned = false;
            }
            StreamContainerLogsRequest {
                follow: true,
                from_offset,
                log_id: log_id.clone(),
                ..self.request()
            }
        };
        let mut entries = logs.logs(&self.pod, request).await?;
        *lock(&self.state.log_id) = log_id.clone();
        self.state.following.store(true, Ordering::Relaxed);
        let mut resource = batch::resource(&self.key(&log_id), &target.node, &self.labels.labels);
        let mut limit = Limit::new(batch::RATE, batch::BURST, Instant::now());
        let mut batch = Batch::new();
        let mut waiting: Vec<oneshot::Sender<()>> = Vec::new();
        // When the log last said something, or a drain began.
        let mut heard = Instant::now();
        loop {
            // Drained, or let go of: what it read goes once the log is
            // quiet.
            let quiet = !waiting.is_empty() || self.ending.is_some();
            tokio::select! {
                entry = entries.next() => {
                    let entry = match entry {
                        None => {
                            if let Some(line) = limit.dropped.take() {
                                batch.push(line, Instant::now());
                            }
                            self.send(&mut batch, &resource, &mut limit).await?;
                            return Ok(Done::Ended);
                        }
                        // The guest is gone: what was read still goes.
                        Some(Err(status)) => {
                            if let Some(line) = limit.dropped.take() {
                                batch.push(line, Instant::now());
                            }
                            self.send(&mut batch, &resource, &mut limit).await?;
                            return Err(status);
                        }
                        Some(Ok(entry)) => entry,
                    };
                    heard = Instant::now();
                    if entry.log_id.is_empty() {
                        return Ok(Done::OldGuest);
                    }
                    if entry.log_id != log_id {
                        // The log got its ID, or another took its place.
                        self.send(&mut batch, &resource, &mut limit).await?;
                        log_id = entry.log_id.clone();
                        *lock(&self.state.log_id) = log_id.clone();
                        resource = batch::resource(&self.key(&log_id), &target.node, &self.labels.labels);
                        if self.position(&log_id).await? > entry.offset {
                            return Ok(Done::Again);
                        }
                    }
                    let now = Instant::now();
                    let mut line = Line::of(entry);
                    if self.container == crate::egress::EGRESS_CONTAINER {
                        // A request: its start and its severity.
                        let observed = std::time::SystemTime::now();
                        if let Some(record) = crate::egress::record(line.text.as_bytes()) {
                            line.time_unix_nano = crate::egress::start_unix_nanos(&record, observed);
                            line.severity = crate::egress::severity(&record);
                        }
                    }
                    batch::redact(&mut line.text, &self.labels.redact);
                    for line in limit.admit(line, now) {
                        if batch.would_overflow(&line) {
                            self.send(&mut batch, &resource, &mut limit).await?;
                        }
                        batch.push(line, now);
                        if batch.is_full() {
                            self.send(&mut batch, &resource, &mut limit).await?;
                        }
                    }
                }
                () = due(batch.due) => self.send(&mut batch, &resource, &mut limit).await?,
                Some(done) = drains.recv() => {
                    if waiting.is_empty() {
                        heard = Instant::now();
                    }
                    waiting.push(done);
                }
                () = told_to_stop(&mut self.stop) => {
                    self.let_go();
                    heard = Instant::now();
                }
                () = sleep_until(heard + DRAIN_QUIET), if quiet => {
                    if let Some(line) = limit.dropped.take() {
                        batch.push(line, Instant::now());
                    }
                    self.send(&mut batch, &resource, &mut limit).await?;
                    for done in waiting.drain(..) {
                        let _ = done.send(());
                    }
                    if self.ending.is_some() {
                        return Ok(Done::Stopped);
                    }
                }
            }
        }
    }

    /// Exports the batch until the sink takes it, then empties it; one the
    /// sink refuses for its tenant's quota is dropped and counted. Fails
    /// when there is no sink any more.
    async fn send(
        &mut self,
        batch: &mut Batch,
        resource: &Resource,
        limit: &mut Limit,
    ) -> Result<(), Status> {
        if batch.is_empty() {
            return Ok(());
        }
        let mut wait = BACKOFF.0;
        loop {
            let target = self
                .inner
                .target()
                .ok_or_else(|| Status::unavailable("no sink"))?;
            let error = match self.inner.channel(&target) {
                Ok(channel) => {
                    let request = batch.request(resource, SystemTime::now());
                    let mut client = LogsServiceClient::new(channel);
                    let deadline = match self.ending {
                        Some(end) => {
                            CALL_TIMEOUT.min(end.saturating_duration_since(Instant::now()))
                        }
                        None => CALL_TIMEOUT,
                    };
                    match timeout(deadline, client.export(request)).await {
                        Ok(Ok(_)) => {
                            if self.warned {
                                info!(
                                    "Logs: The sink takes {}/{}'s log again",
                                    self.pod, self.container
                                );
                                self.warned = false;
                            }
                            batch.take();
                            return Ok(());
                        }
                        Ok(Err(status)) if status.code() == Code::ResourceExhausted => {
                            for line in batch.take() {
                                limit.dropped.add(&line);
                            }
                            return Ok(());
                        }
                        Ok(Err(status)) => status,
                        Err(_) => Status::deadline_exceeded("the export timed out"),
                    }
                }
                Err(status) => status,
            };
            if !self.warned {
                warn!(
                    "Logs: The sink did not take {}/{}'s log: {}; kept and sent again",
                    self.pod,
                    self.container,
                    error.message()
                );
                self.warned = true;
            }
            // Let go of, it tries until its time is up, and drops the rest.
            if let Some(end) = self.ending {
                let left = end.saturating_duration_since(Instant::now());
                if left.is_zero() {
                    let lines = batch.take().len();
                    warn!(
                        "Logs: {}/{}'s last {lines} lines were not sent: the sink did not take \
                         them before the stream ended",
                        self.pod, self.container
                    );
                    return Err(Status::unavailable("the stream ended"));
                }
                sleep(wait.min(left)).await;
            } else if !self.pause(wait).await {
                self.let_go();
            }
            wait = (wait * 2).min(BACKOFF.1);
        }
    }
}

/// Waits until `at`, if it is set.
async fn due(at: Option<Instant>) {
    match at {
        Some(at) => sleep_until(at).await,
        None => std::future::pending().await,
    }
}

/// Ends the streams of deleted pods at the sink, asking again until it
/// answers.
async fn end_streams(inner: Arc<Inner>) {
    let mut wait = BACKOFF.0;
    loop {
        let keys: Vec<Key> = {
            let mut ends = lock(&inner.ends);
            let n = ends.len().min(KEYS_PER_CALL);
            ends.drain(..n).collect()
        };
        if keys.is_empty() {
            inner.ended.notified().await;
            continue;
        }
        let ended = match inner.target() {
            Some(target) => match inner.channel(&target) {
                Ok(channel) => {
                    let mut sink = SinkServiceClient::new(channel);
                    let request = EndStreamsRequest {
                        streams: keys
                            .iter()
                            .map(|key| StreamKey {
                                pod: key.pod.clone(),
                                container: key.container.clone(),
                                log_id: key.log_id.clone(),
                            })
                            .collect(),
                        time_unix_nano: 0,
                    };
                    match timeout(CALL_TIMEOUT, sink.end_streams(request)).await {
                        Ok(Ok(_)) => Ok(()),
                        Ok(Err(status)) => Err(status.message().to_string()),
                        Err(_) => Err("EndStreams timed out".to_string()),
                    }
                }
                Err(status) => Err(status.message().to_string()),
            },
            None => Err("no sink".to_string()),
        };
        match ended {
            Ok(()) => {
                let pods: HashSet<&str> = keys.iter().map(|key| key.pod.as_str()).collect();
                debug!("Logs: Ended the streams of pods {pods:?}");
                wait = BACKOFF.0;
            }
            Err(e) => {
                debug!("Logs: Ending {} streams failed: {e}", keys.len());
                {
                    let mut ends = lock(&inner.ends);
                    for key in keys.into_iter().rev() {
                        ends.push_front(key);
                    }
                    ends.truncate(MAX_ENDS);
                }
                sleep(wait).await;
                wait = (wait * 2).min(BACKOFF.1);
            }
        }
    }
}
