// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd and the control plane (ADR 0015): what the node reports of its
//! pods, diskd and installed versions, and its pods' events; and its pods'
//! logs and spans, which go to the sink the control plane names (ADR 0026).

use feos_proto::container_service::StreamContainerLogsRequest;
use feos_proto::pod_service::PodInfo;
use fleet_client::egress::{self, Joiner};
use fleet_client::{
    Diskd, Events, Fleet, Incoming, Installed, Inventory, LogStream, Logs, Senders, ShippedPod,
    Shipper, TracedPod, Traces, EGRESS_CONTAINER,
};
use log::{debug, warn};
use microvm_service::vmm::installed;
use pod_service::authn::audit;
use pod_service::Pods;
use std::collections::HashMap;
use std::sync::Arc;
use std::sync::Mutex;
use std::time::{Duration, SystemTime};
use tokio::runtime::Handle;
use tokio::sync::broadcast::error::RecvError;

/// The node, as the control plane's threads ask it.
struct Node {
    pods: Pods,
    /// feosd's runtime, which what the control plane's answers set up
    /// runs on: the pods' network serves the pods from there.
    runtime: Handle,
}

impl Inventory for Node {
    fn pods(&self) -> Vec<PodInfo> {
        self.pods.list()
    }

    fn installed(&self) -> Installed {
        Installed {
            firecracker: installed::firecracker_versions(),
            guest: installed::guest_versions(),
        }
    }

    async fn diskd(&self) -> Option<Diskd> {
        let stats = self.pods.disks().node_stats().await.ok()?;
        Some(Diskd {
            object_store: stats.object_store,
            object_store_id: stats.object_store_id,
            version: stats.version,
        })
    }

    fn pod_prefix(&self) -> Option<String> {
        let (prefix, len) = self.pods.network().pod_prefix()?;
        Some(format!("{prefix}/{len}"))
    }

    fn mesh_key(&self) -> Option<([u8; 32], u16)> {
        self.pods.network().mesh_key()
    }

    /// Taken on feosd's runtime, to the end, even when the control plane's
    /// side stops waiting for it.
    async fn joined_mesh(&self, pod_network: String, pod_prefix: String) -> Result<(), String> {
        let pods = self.pods.clone();
        self.runtime
            .spawn(async move { pods.network().join_mesh(&pod_network, &pod_prefix).await })
            .await
            .map_err(|e| format!("taking the mesh's answer: {e}"))?
    }
}

/// The pods' logs, as the shipper reads them.
struct PodLogs(Pods);

impl Logs for PodLogs {
    fn pods(&self) -> Vec<ShippedPod> {
        self.0
            .shipping_logs()
            .into_iter()
            .map(|pod| ShippedPod {
                containers: pod
                    .spec
                    .map(|spec| {
                        // The pod's requests through its HTTP bindings, a
                        // log of their own (ADR 0028 §6).
                        let egress = pod_service::has_http_bindings(&spec)
                            .then(|| EGRESS_CONTAINER.to_string());
                        spec.containers
                            .into_iter()
                            .map(|c| c.name)
                            .chain(egress)
                            .collect()
                    })
                    .unwrap_or_default(),
                id: pod.pod_id,
                labels: pod.log_labels,
                redact: pod.log_redact,
            })
            .collect()
    }

    async fn logs(
        &self,
        pod_id: &str,
        request: StreamContainerLogsRequest,
    ) -> Result<LogStream, tonic::Status> {
        if request.container_id == EGRESS_CONTAINER {
            let dir = self.0.authn().pod_dir(pod_id);
            return Ok(Box::pin(audit::follow(
                dir,
                request,
                Arc::new(egress::is_http),
            )));
        }
        Ok(Box::pin(self.0.container_logs(pod_id, request).await?))
    }
}

/// The pods that send spans, as the trace listener knows them.
struct PodSenders(Pods);

impl Senders for PodSenders {
    fn pods(&self) -> Vec<TracedPod> {
        self.0
            .labelled()
            .into_iter()
            .map(|pod| TracedPod {
                addresses: [&pod.ipv4_address, &pod.ipv6_address]
                    .into_iter()
                    .filter_map(|address| address.parse().ok())
                    .collect(),
                id: pod.pod_id,
                labels: pod.log_labels,
            })
            .collect()
    }
}

/// Takes the pods' spans at OTLP's address, once the pods'
/// network is there, and sends them on to the sink over the logs' channel
/// (docs/architecture/traces.md).
fn take_spans(pods: &Pods, shipper: Shipper) -> Traces {
    let traces = Traces::start(PodSenders(pods.clone()), shipper);
    let serving = traces.clone();
    let pods = pods.clone();
    tokio::spawn(async move {
        let mut ready = pods.network().ready();
        if ready.wait_for(|ready| *ready).await.is_err() {
            return;
        }
        serving.serve(pods.network().otlp_address()).await;
    });
    traces
}

/// How often the requests that wait for their ends are looked at.
const EGRESS_EXPIRY: Duration = Duration::from_secs(60);
/// How often a batch of egress spans is sent before it is dropped.
const EGRESS_TRIES: u32 = 3;
/// The batches of egress spans that wait to be sent; past them, a batch is
/// dropped.
const EGRESS_QUEUE: usize = 64;

type EgressSpans = (
    String,
    Vec<feos_proto::opentelemetry::proto::trace::v1::Span>,
);

/// Publishes the records of the pods' feos-authn (ADR 0014, ADR 0028 §6)
/// from their audit files: each as the pod's audit event, and each request
/// as a client span once its two records came; one whose end did not come
/// within [`egress::END_WAIT`] has none. Spans go as the pods' own do,
/// best effort, from a task of their own: the tail, which reads every
/// pod's records in turn, never waits for them.
fn publish_audit(pods: &Pods, events: Events, traces: Traces) {
    let joiners: Arc<Mutex<HashMap<String, Joiner>>> = Arc::default();
    let (exports, mut exporting) = tokio::sync::mpsc::channel::<EgressSpans>(EGRESS_QUEUE);
    tokio::spawn(async move {
        while let Some((pod_id, spans)) = exporting.recv().await {
            export_spans(&traces, &pod_id, spans).await;
        }
    });
    let handler: audit::Handler = {
        let joiners = joiners.clone();
        Arc::new(move |pod_id: String, lines: Vec<Vec<u8>>| {
            let observed = SystemTime::now();
            let mut spans = Vec::new();
            {
                let mut joiners = joiners.lock().unwrap_or_else(|e| e.into_inner());
                let joiner = joiners.entry(pod_id.clone()).or_default();
                for line in &lines {
                    let Some(record) = egress::record(line) else {
                        continue;
                    };
                    if let Some(kind) = record.kind.as_deref().and_then(egress::audit_kind) {
                        events.audit(&pod_id, kind, &String::from_utf8_lossy(line));
                    }
                    spans.extend(joiner.take(record, observed));
                }
            }
            if !spans.is_empty()
                && let Err(e) = exports.try_send((pod_id, spans))
            {
                let (pod_id, spans) = e.into_inner();
                debug!(
                    "Fleet: Pod {pod_id}'s {} egress spans are dropped: {EGRESS_QUEUE} batches \
                     wait to be sent",
                    spans.len()
                );
            }
            Box::pin(async { true })
        })
    };
    pods.follow_audit(Arc::new(egress::is_followed), handler);
    let live = pods.clone();
    // Forgets the requests whose ends do not come.
    tokio::spawn(async move {
        loop {
            tokio::time::sleep(EGRESS_EXPIRY).await;
            let known: std::collections::HashSet<String> =
                live.list().into_iter().map(|pod| pod.pod_id).collect();
            let mut joiners = joiners.lock().unwrap_or_else(|e| e.into_inner());
            for (pod_id, joiner) in joiners.iter_mut() {
                let forgotten = joiner.expire(SystemTime::now(), egress::END_WAIT);
                if forgotten > 0 {
                    debug!(
                        "Fleet: {forgotten} of pod {pod_id}'s requests have no record of their \
                         end: no span"
                    );
                }
            }
            joiners.retain(|pod_id, joiner| known.contains(pod_id) || !joiner.is_empty());
        }
    });
}

async fn export_spans(
    traces: &Traces,
    pod_id: &str,
    spans: Vec<feos_proto::opentelemetry::proto::trace::v1::Span>,
) {
    if spans.is_empty() {
        return;
    }
    for attempt in 1..=EGRESS_TRIES {
        match traces.export_egress(pod_id, spans.clone()).await {
            Ok(()) => return,
            Err(e) if attempt < EGRESS_TRIES => {
                debug!("Fleet: Pod {pod_id}'s egress spans: {}; again", e.message());
                tokio::time::sleep(Duration::from_secs(u64::from(attempt))).await;
            }
            Err(e) => debug!(
                "Fleet: Pod {pod_id}'s {} egress spans are dropped: {}",
                spans.len(),
                e.message()
            ),
        }
    }
}

/// Ships the pods' logs to the sink the control plane names, and drains
/// a pod's before it pauses, hibernates or is deleted; and their spans.
fn ship_logs(fleet: &Fleet, pods: &Pods) {
    let shipper = Shipper::start(PodLogs(pods.clone()), fleet.sink());
    let traces = take_spans(pods, shipper.clone());
    publish_audit(pods, fleet.events().clone(), traces.clone());
    let drain = shipper.clone();
    pods.set_log_drain(Arc::new(move |pod_id| {
        let shipper = drain.clone();
        Box::pin(async move { shipper.drain(&pod_id).await })
    }));
    let mut events = pods.subscribe();
    tokio::spawn(async move {
        loop {
            match events.recv().await {
                Ok(event) => {
                    if event.deleted {
                        shipper.deleted(&event.pod_id);
                    }
                    shipper.changed();
                    traces.changed();
                }
                Err(RecvError::Lagged(_)) => {
                    shipper.changed();
                    traces.changed();
                }
                Err(RecvError::Closed) => return,
            }
        }
    });
}

/// Starts the node's side of the control plane, with the pods' events. It
/// runs on threads of its own; what it asks of the node runs on this
/// runtime. `calls` are those of the node connections' server.
pub(crate) fn start(
    config: fleet_client::Config,
    pods: Pods,
    calls: fleet_client::NodeCalls,
) -> Result<(Fleet, Incoming), String> {
    let mut events = pods.subscribe();
    let node = Node {
        pods: pods.clone(),
        runtime: Handle::current(),
    };
    let (fleet, incoming) = fleet_client::start(config, node, calls)?;
    ship_logs(&fleet, &pods);
    // The pods' feos-authn ask for their credentials over the uplink.
    let issuing = fleet.clone();
    pods.authn().set_issuer(Arc::new(move |request| {
        let fleet = issuing.clone();
        Box::pin(async move { fleet.issue_pod_credentials(request).await })
    }));
    let sink = fleet.events().clone();
    tokio::spawn(async move {
        loop {
            match events.recv().await {
                Ok(event) => sink.pod(event),
                Err(RecvError::Lagged(missed)) => {
                    warn!("Fleet: {missed} pod events were missed");
                    sink.lost(missed);
                }
                Err(RecvError::Closed) => return,
            }
        }
    });
    Ok((fleet, incoming))
}
