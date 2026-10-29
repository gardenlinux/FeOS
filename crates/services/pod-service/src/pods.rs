// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods, on top of the VM service (ADR 0004).
//!
//! A pod comes up in steps: its disk is started, its microVM is created
//! and booted, its guest attaches, and its containers are created and
//! started in the guest, one after the other. Each step looks at what is
//! there first, so that a bring-up that was cut off, by a restart of feosd
//! or a stop, is taken up again where it ended. A boot on a disk that has
//! the pod's containers adopts them. What changes a pod runs under its
//! lock; a stop or a delete ends a bring-up first.
//!
//! A pod hibernates (ADR 0008) in a task of its own, which holds the pod's
//! lock, in this order: it is suspended, its disk is stopped, sealed on the
//! node and uploaded, its volumes likewise, its memory sealed and uploaded,
//! its vmstate and its spec put beside them; then, past the point of no
//! return, its volumes' heads and its head, last. Then it is a record with
//! its checkpoint, and what it held on the node goes. A HibernatePod that
//! comes while it runs joins it, and one whose caller goes away leaves it
//! running. Until the head is up the pod stays suspended on the node; until
//! the heads are being put, a resume abandons the hibernation and resumes
//! the pod from its snapshot. A restore opens the checkpoint's disk and
//! restores the memory where the node can load the snapshot; elsewhere, it
//! boots anew on the disk.
//!
//! A pod's volumes (ADR 0016) follow its disk: started for a boot, a resume
//! and a restore, after the disk, stopped with it, sealed and uploaded
//! with it, each with a head of its own, put right before the pod's, which
//! names them. A pod claims them at its creation and lets go of them at its
//! deletion. After a boot, the guest mounts them before the containers
//! are created, and formats those the node knows to be new.

mod stop;

pub use stop::{read_record, StopRecord, Stopped, STOP_RECORD};

use crate::bake;
use crate::disk::{volume_disk, Disks};
use crate::network::{Attachment, Network};
use crate::spec;
use crate::store::Store;
use crate::volumes::{self, Volumes};
use feos_proto::container_service::{
    container_service_client::ContainerServiceClient, ContainerConfig,
    ContainerState as GuestContainerState, CreateContainerRequest, GetContainerRequest,
    ListContainersRequest, StartContainerRequest, VolumeMount as GuestVolumeMount,
};
use feos_proto::disk_service::{Head, MemoryRef};
use feos_proto::guest_service::{
    guest_service_client::GuestServiceClient, MountVolumesRequest, Volume as GuestVolume,
};
use feos_proto::host_service::{
    host_service_client::HostServiceClient, FreezeFilesystemRequest, ThawFilesystemRequest,
};
use feos_proto::image_service::{
    image_service_client::ImageServiceClient, ImageState, PullImageRequest, WatchImageStatusRequest,
};
use feos_proto::microvm::{
    micro_vm_service_server::MicroVmService, stream_vm_console_request as console_request,
    AttachConsoleMessage, ConsoleData, CreateVmRequest, DeleteVmRequest, GetVmRequest,
    PingVmRequest, RemoteSnapshot, ResumeVmRequest, ShutdownVmRequest, StartVmRequest,
    StreamVmConsoleRequest, StreamVmEventsRequest, SuspendVmRequest, VmConfig, VmEvent, VmInfo,
    VmState, VmStateChangedEvent,
};
use feos_proto::pod_service::{
    BakedLayer, Checkpoint, ContainerSpec, ContainerState, ContainerStatus, HibernatePodResponse,
    Hibernation as HibernationInfo, HibernationPhase, ImageLayers, MicroVm, PodConsoleInput,
    PodConsoleOutput, PodEvent, PodInfo, PodSpec, PodState, RestorePodResponse,
};
use feos_proto::volume_service::{Volume, VolumeCheckpoint, VolumeState};
use feos_utils::vsock::{self, API_PORT};
use futures::future::{BoxFuture, Shared};
use futures::FutureExt;
use log::{info, warn};
use microvm_service::api::VmApiHandler;
use microvm_service::vmm::{budget, snapshot};
use microvm_service::{memory_socket_path, vsock_path};
use prost::Message;
use std::collections::{HashMap, VecDeque};
use std::future::Future;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::atomic::AtomicBool;
use std::sync::{Arc, Mutex, MutexGuard};
use std::task::Poll;
use std::time::{Duration, SystemTime};
use tokio::sync::{broadcast, watch, OwnedMutexGuard};
use tokio::task::{AbortHandle, JoinError};
use tokio::time::{sleep, timeout, Instant};
use tokio_stream::{Stream, StreamExt};
use tonic::transport::Channel;
use tonic::{Code, Request, Status};

/// How long a booting guest may take to attach.
const GUEST_TIMEOUT: Duration = Duration::from_secs(60);
/// How often a step looks again while it waits.
const POLL: Duration = Duration::from_millis(250);
/// How long the VM service may take to record a change it made: a part of
/// the suspend's budget (`budget`).
const SETTLE_TIMEOUT: Duration = budget::RECORD_SETTLE;
/// How long GetPod waits for the guest's view of the containers.
const STATUS_TIMEOUT: Duration = Duration::from_secs(3);
/// How long the guest may take to mount the pod's volumes, formatting new
/// ones: mke2fs writes some MiB for every TiB.
const MOUNT_TIMEOUT: Duration = Duration::from_secs(300);
/// How often the rules are written anew without a change of the pods or
/// their groups: the node's own addresses, which no pod reaches, may have
/// changed.
const REFRESH: Duration = Duration::from_secs(60);
/// How many request ids a hibernation keeps of the calls that started or
/// joined it, and a pod of its abandoned hibernations: the last ones.
const REQUEST_IDS: usize = 16;

#[derive(Clone)]
pub struct Pods {
    inner: Arc<Inner>,
}

struct Inner {
    vms: VmApiHandler,
    store: Store,
    /// Where the pods are kept; a hibernation's and a restore's files pass
    /// through it.
    dir: PathBuf,
    network: Network,
    disks: Disks,
    volumes: Volumes,
    pods: Mutex<HashMap<String, Pod>>,
    events: broadcast::Sender<PodEvent>,
    /// feosd stops: calls that change pods are refused.
    stopping: AtomicBool,
    /// Held while the record of a stop is written or removed.
    stop_record: tokio::sync::Mutex<()>,
    /// Held while a pod is added or given addresses, which are handed out
    /// and kept on the blocking pool, off the pods' lock, one pod at a
    /// time.
    records: tokio::sync::Mutex<()>,
}

struct Pod {
    info: PodInfo,
    /// Held by what changes the pod: an operation, or its bring-up.
    lock: Arc<tokio::sync::Mutex<()>>,
    bring_up: Option<AbortHandle>,
    /// The hibernation in flight, from its first HibernatePod until its
    /// task ends.
    hibernation: Option<Hibernation>,
    /// The request ids of the pod's abandoned hibernations, the last 16, in
    /// memory only: a late HibernatePod with one of them is answered
    /// ABORTED instead of hibernating the resumed pod.
    abandoned: VecDeque<String>,
    /// What keeps its microVM from answering now, as a suspend does, for a
    /// call to its guest that gets no answer.
    busy: Option<&'static str>,
    /// The Bearer token for the guest's pull of the pod's image that its
    /// create or start brought (ADR 0025): in memory only, for the next
    /// bring-up, which takes it.
    pull_token: Option<String>,
}

impl Pod {
    fn new(info: PodInfo) -> Self {
        Self {
            info,
            lock: Arc::default(),
            bring_up: None,
            hibernation: None,
            abandoned: VecDeque::new(),
            busy: None,
            pull_token: None,
        }
    }

    /// Why its microVM may not answer now, as far as the pod knows.
    fn busy(&self) -> Option<String> {
        if let Some(busy) = self.busy {
            return Some(busy.to_string());
        }
        self.hibernation.as_ref().map(|hibernation| {
            format!("the pod hibernates, in {}", hibernation.phase.as_str_name())
        })
    }

    /// Its record, with its hibernation in flight, which is never kept.
    fn shown(&self) -> PodInfo {
        let mut info = self.info.clone();
        info.hibernation = self.hibernation.as_ref().map(Hibernation::shown);
        info
    }
}

/// Marks the pod busy until it drops.
struct BusyMark {
    pods: Pods,
    pod_id: String,
}

impl Drop for BusyMark {
    fn drop(&mut self) {
        if let Some(pod) = self.pods.pods().get_mut(&self.pod_id) {
            pod.busy = None;
        }
    }
}

/// What every HibernatePod of a hibernation is answered.
type Answer = Result<HibernatePodResponse, Status>;

/// A hibernation in flight (ADR 0008): its task holds the pod's lock.
struct Hibernation {
    /// Which hibernation this is: its task ends the slot only while the
    /// slot holds this token.
    token: Arc<()>,
    /// The task's answer, for every HibernatePod that joins it.
    answer: Shared<BoxFuture<'static, Answer>>,
    /// Set by a resume that abandons the hibernation; its task looks at it.
    abandon: watch::Sender<bool>,
    /// A HibernatePod said that it may not be abandoned, as a drain does:
    /// one call that started or joined it says so for all.
    no_abandon: bool,
    /// The request ids of the HibernatePods that started or joined it, the
    /// last 16 that carried one: the control plane abandons only a
    /// hibernation that its own call reached.
    request_ids: Vec<String>,
    phase: HibernationPhase,
    /// Whether the pod's memory goes with it; unknown until its task
    /// decided.
    memory: Option<bool>,
    /// The epoch of its checkpoint; 0 until it is chosen.
    epoch: u64,
}

impl Hibernation {
    fn new(
        token: Arc<()>,
        answer: Shared<BoxFuture<'static, Answer>>,
        abandon: watch::Sender<bool>,
        no_abandon: bool,
        request_id: &str,
    ) -> Self {
        let mut hibernation = Self {
            token,
            answer,
            abandon,
            no_abandon,
            request_ids: Vec::new(),
            phase: HibernationPhase::Waiting,
            memory: None,
            epoch: 0,
        };
        hibernation.add_request(request_id);
        hibernation
    }

    /// Whether a resume abandons it now: unless it may not be abandoned,
    /// the pod has no memory to resume, or its heads are being put.
    fn abandonable(&self) -> bool {
        !self.no_abandon && self.memory != Some(false) && self.phase < HibernationPhase::Head
    }

    /// Adds the request id of a HibernatePod that started or joined it.
    fn add_request(&mut self, request_id: &str) {
        if request_id.is_empty() || self.request_ids.iter().any(|id| id == request_id) {
            return;
        }
        if self.request_ids.len() == REQUEST_IDS {
            self.request_ids.remove(0);
        }
        self.request_ids.push(request_id.to_string());
    }

    /// As GetPod shows it.
    fn shown(&self) -> HibernationInfo {
        HibernationInfo {
            phase: self.phase as i32,
            abandonable: self.abandonable(),
            epoch: self.epoch,
            request_ids: self.request_ids.clone(),
        }
    }
}

impl Pods {
    /// The pods kept in `dir`, on the pods' network, with their disks from
    /// `disks` and the node's `volumes`. Those that were coming up come up
    /// now, and what hibernated pods left on the node goes; the others
    /// follow their microVMs from here on.
    pub async fn new(
        vms: VmApiHandler,
        dir: &Path,
        network: Network,
        disks: Disks,
        volumes: Volumes,
    ) -> io::Result<Self> {
        let store = Store::open(dir)?;
        // A hibernated pod has no addresses on the node.
        let (mut kept, hibernated): (Vec<_>, Vec<_>) = store
            .load()?
            .into_iter()
            .partition(|info| info.state() != PodState::Hibernated);
        for pod_id in network.adopt(&mut kept) {
            if let Some(info) = kept.iter().find(|info| info.pod_id == pod_id)
                && let Err(e) = store.save(info).await
            {
                warn!("Pods: Failed to keep {pod_id}: {e}");
            }
        }
        kept.extend(hibernated);
        network.sync(&kept);
        network.serve_dns();
        volumes.reconcile(&kept).await;
        let (events, _) = broadcast::channel(256);
        let pods = Self {
            inner: Arc::new(Inner {
                vms,
                store,
                dir: dir.to_path_buf(),
                network,
                disks,
                volumes,
                pods: Mutex::new(HashMap::new()),
                events,
                stopping: AtomicBool::new(false),
                stop_record: tokio::sync::Mutex::new(()),
                records: tokio::sync::Mutex::new(()),
            }),
        };
        let mut pending = Vec::new();
        let mut running = Vec::new();
        let mut purge = Vec::new();
        for info in kept {
            match info.state() {
                PodState::Pending => pending.push(info.pod_id.clone()),
                PodState::Running => running.push(info.pod_id.clone()),
                PodState::Hibernated => purge.push(info.pod_id.clone()),
                _ => {}
            }
            pods.pods().insert(info.pod_id.clone(), Pod::new(info));
        }
        info!("Pods: {} kept", pods.pods().len());
        match pods.apply_network().await {
            Ok(()) => pods.inner.network.start_records(),
            Err(e) => pods
                .inner
                .network
                .fail(format!("the pods' rules are not written: {e}")),
        }
        let weak = Arc::downgrade(&pods.inner);
        pods.inner.network.groups().set_in_use(move |id| {
            weak.upgrade()
                .is_some_and(|inner| Pods { inner }.names_group(id))
        });
        let weak = Arc::downgrade(&pods.inner);
        pods.inner.network.names().set_in_use(move |id| {
            weak.upgrade()
                .is_some_and(|inner| Pods { inner }.names_table(id))
        });
        for pod_id in running {
            // Suspended by a suspend that a feosd before this one did not
            // see to its end.
            if pods.follow_vm(&pod_id).await {
                continue;
            }
            if let Err(e) = pods.reattach(&pod_id).await {
                warn!("Pods: {pod_id} runs without its network: {}", e.message());
            }
        }
        for pod_id in pending {
            pods.spawn_bring_up(&pod_id);
        }
        // A hibernation that was cut off after its head was up.
        for pod_id in purge {
            let pods = pods.clone();
            tokio::spawn(async move {
                if let Ok(_lock) = pods.lock(&pod_id).await
                    && pods
                        .info(&pod_id)
                        .is_ok_and(|i| i.state() == PodState::Hibernated)
                {
                    pods.purge(&pod_id).await;
                }
            });
        }
        pods.follow_vms();
        pods.follow_security_groups();
        pods.follow_network();
        Ok(pods)
    }

    /// Creates the pod and brings it up, in a task of its own (`detached`).
    /// Creates a pod, with what its caller knows of baked layers of its
    /// image (ADR 0025): kept for its bring-up, and not compared when the
    /// pod is there already.
    pub async fn create(
        &self,
        pod_id: String,
        spec: PodSpec,
        image_layers: Option<ImageLayers>,
        pull_token: String,
    ) -> Result<PodInfo, Status> {
        self.detached(move |pods| async move {
            pods.create_detached(pod_id, spec, image_layers, pull_token)
                .await
        })
        .await
    }

    async fn create_detached(
        &self,
        pod_id: String,
        spec: PodSpec,
        image_layers: Option<ImageLayers>,
        pull_token: String,
    ) -> Result<PodInfo, Status> {
        spec::check_id(&pod_id)?;
        let spec = spec::normalize(spec)?;
        self.inner.network.check(&spec)?;
        if let Some(existing) = self.pods().get(&pod_id).map(|pod| pod.info.clone()) {
            return same_spec(existing, &spec);
        }
        // The pod claims its volumes before it is recorded, all or none.
        self.inner
            .volumes
            .claim(&pod_id, &spec::volume_ids(&spec))
            .await?;
        let info = PodInfo {
            pod_id: pod_id.clone(),
            spec: Some(spec.clone()),
            state: PodState::Pending as i32,
            image_layers,
            ..Default::default()
        };
        let recorded = match self.add(info, spec::has_network(&spec)).await {
            Ok(Ok(info)) => Ok(info),
            Ok(Err(existing)) => Err(Ok(existing)),
            Err(e) => Err(Err(e)),
        };
        let info = match recorded {
            Ok(info) => info,
            // Another create was faster: what its pod does not name is let
            // go of again.
            Err(Ok(existing)) => {
                let named = existing
                    .spec
                    .as_ref()
                    .map(spec::volume_ids)
                    .unwrap_or_default();
                self.inner.volumes.release(&pod_id, &named).await;
                return same_spec(existing, &spec);
            }
            Err(Err(e)) => {
                self.inner.volumes.release(&pod_id, &[]).await;
                return Err(e);
            }
        };
        info!("Pods: {pod_id} created");
        self.set_pull_token(&pod_id, pull_token);
        // The pod's rules stand before its TAP device carries a frame.
        if let Err(e) = self.apply_network().await {
            warn!("Pods: Failed to set the pods' rules: {e}");
        }
        self.announce(&info, false);
        self.spawn_bring_up(&pod_id);
        Ok(info)
    }

    /// Adds the pod `info`, unless one with its ID is there, which comes
    /// back as `Err`: gives it its addresses if `addresses`, keeps it, and
    /// adds it then, off the pods' lock. It runs in a task of its own under
    /// `records`: a caller that goes away leaves no pod kept that the pods
    /// do not have.
    async fn add(
        &self,
        info: PodInfo,
        addresses: bool,
    ) -> Result<Result<PodInfo, PodInfo>, Status> {
        let pods = self.clone();
        tokio::spawn(async move {
            let _records = pods.inner.records.lock().await;
            let pod_id = info.pod_id.clone();
            if let Some(existing) = pods.pods().get(&pod_id).map(|pod| pod.info.clone()) {
                return Ok(Err(existing));
            }
            let info = match addresses {
                true => pods.allocate(info).await?,
                false => info,
            };
            if let Err(e) = pods.inner.store.save(&info).await {
                pods.release_addresses(&info).await;
                return Err(Status::internal(format!("keep pod {pod_id}: {e}")));
            }
            pods.pods().insert(pod_id, Pod::new(info.clone()));
            Ok(Ok(info))
        })
        .await
        .unwrap_or_else(|e| Err(Status::internal(format!("the pod's adding ended: {e}"))))
    }

    /// Hands out the pod's addresses, on the blocking pool: they are kept
    /// before they are handed out. The caller holds `records`.
    async fn allocate(&self, mut info: PodInfo) -> Result<PodInfo, Status> {
        let others: Vec<PodInfo> = self
            .pods()
            .values()
            .filter(|pod| pod.info.pod_id != info.pod_id)
            .map(|pod| pod.info.clone())
            .collect();
        let pods = self.clone();
        tokio::task::spawn_blocking(move || {
            pods.inner
                .network
                .allocate(others.iter(), &mut info)
                .map(|()| info)
        })
        .await
        .unwrap_or_else(|e| Err(Status::internal(format!("hand out addresses: {e}"))))
    }

    /// Frees the pod's IPv4 address, on the blocking pool.
    async fn release_addresses(&self, info: &PodInfo) {
        let (pods, info) = (self.clone(), info.clone());
        let _ = tokio::task::spawn_blocking(move || pods.inner.network.release(&info)).await;
    }

    /// Runs a change of a pod in a task of its own, and waits for it: a
    /// caller that goes away, as one whose deadline passed, whose stream
    /// ferund reset, or that the node's stop cut, leaves the change running
    /// to its end. Cut off between two of its steps, which await records,
    /// the microVM service and diskd, a change would leave the pod half
    /// changed: pending with no bring-up, or deleted with its record kept.
    async fn detached<T, F>(&self, change: impl FnOnce(Pods) -> F) -> Result<T, Status>
    where
        T: Send + 'static,
        F: Future<Output = Result<T, Status>> + Send + 'static,
    {
        tokio::spawn(change(self.clone()))
            .await
            .unwrap_or_else(|e| Err(Status::internal(format!("the change ended: {e}"))))
    }

    /// Brings a stopped or failed pod up again, on its disk, in a task of
    /// its own (`detached`).
    pub async fn start(&self, pod_id: &str, pull_token: String) -> Result<(), Status> {
        let pod_id = pod_id.to_string();
        self.detached(move |pods| async move { pods.start_detached(&pod_id, pull_token).await })
            .await
    }

    async fn start_detached(&self, pod_id: &str, pull_token: String) -> Result<(), Status> {
        let _lock = self.lock(pod_id).await?;
        match self.info(pod_id)?.state() {
            PodState::Pending | PodState::Running => return Ok(()),
            PodState::Suspended => {
                return Err(Status::failed_precondition(format!(
                    "pod {pod_id} is suspended: resume it"
                )));
            }
            PodState::Hibernated => {
                return Err(Status::failed_precondition(format!(
                    "pod {pod_id} is hibernated: resume or restore it, or stop it to boot it anew"
                )));
            }
            _ => {}
        }
        self.set_state(pod_id, PodState::Pending, "").await;
        self.set_pull_token(pod_id, pull_token);
        drop(_lock);
        self.spawn_bring_up(pod_id);
        Ok(())
    }

    /// Keeps the token for the pod's next bring-up; empty: none.
    fn set_pull_token(&self, pod_id: &str, pull_token: String) {
        if let Some(pod) = self.pods().get_mut(pod_id) {
            pod.pull_token = (!pull_token.is_empty()).then_some(pull_token);
        }
    }

    /// Takes the token the pod's bring-up pulls with, if it has one: a
    /// bring-up uses it once.
    fn take_pull_token(&self, pod_id: &str) -> Option<String> {
        self.pods().get_mut(pod_id)?.pull_token.take()
    }

    /// Ends the pod's microVM and stops its disk. Whatever state it is in,
    /// the next start boots it anew on its disk; a hibernated pod's is the
    /// one of its checkpoint. It runs in a task of its own (`detached`).
    pub async fn stop(&self, pod_id: &str) -> Result<(), Status> {
        let pod_id = pod_id.to_string();
        self.detached(move |pods| async move { pods.stop_detached(&pod_id).await })
            .await
    }

    async fn stop_detached(&self, pod_id: &str) -> Result<(), Status> {
        let _lock = self.end_bring_up_and_lock(pod_id).await?;
        match self.info(pod_id)?.state() {
            PodState::Stopped => return Ok(()),
            PodState::Hibernated => {
                self.set_state(
                    pod_id,
                    PodState::Stopped,
                    "stopped: its memory is dropped, its disk is its checkpoint's",
                )
                .await;
                return Ok(());
            }
            _ => {}
        }
        match self.vm(pod_id).await?.map(|vm| vm.state()) {
            None | Some(VmState::Stopped) => {}
            Some(VmState::Running) => {
                let request = Request::new(ShutdownVmRequest {
                    vm_id: pod_id.to_string(),
                });
                match self.inner.vms.shutdown_vm(request).await {
                    Ok(_) => self.settle(pod_id, VmState::Stopped).await?,
                    // A microVM whose late snapshot Firecracker still
                    // writes is not ended under it: the caller stops the
                    // pod later.
                    Err(e) if !ends_after_failed_shutdown(&e) => return Err(e),
                    Err(e) => {
                        // A guest that does not go is ended.
                        warn!("Pods: {pod_id} did not shut down: {}", e.message());
                        self.delete_vm(pod_id).await?;
                    }
                }
            }
            Some(_) => self.delete_vm(pod_id).await?,
        }
        self.release_disk(pod_id).await;
        self.set_state(pod_id, PodState::Stopped, "stopped").await;
        Ok(())
    }

    /// Deletes the pod: its microVM, its disk, its place on the network,
    /// its record and its rules, and then frees its IPv4 address. Its
    /// volumes stay, stopped, and are attached to no pod then. It runs in a
    /// task of its own (`detached`): a delete that waits for the pod's
    /// lock behind a suspend is not cut off once it has it.
    pub async fn delete(&self, pod_id: &str) -> Result<(), Status> {
        let pod_id = pod_id.to_string();
        self.detached(move |pods| async move { pods.delete_detached(&pod_id).await })
            .await
    }

    async fn delete_detached(&self, pod_id: &str) -> Result<(), Status> {
        let _lock = self.end_bring_up_and_lock(pod_id).await?;
        self.delete_vm(pod_id).await?;
        // A pod whose disk cannot go stays, to be deleted again: diskd
        // would keep what it wrote for ever.
        self.inner.disks.close_memory(pod_id).await;
        if let Err(e) = self.inner.disks.remove(pod_id).await {
            self.set_state(
                pod_id,
                PodState::Failed,
                &format!("its disk is not deleted: {}", e.message()),
            )
            .await;
            return Err(e);
        }
        let info = self.info(pod_id)?;
        self.stop_volumes(&info).await;
        self.inner
            .network
            .detach(&info)
            .await
            .map_err(|e| Status::internal(e.to_string()))?;
        if let Some(pod) = self.forget(pod_id).await? {
            self.announce(&pod.info, true);
        }
        self.inner.volumes.release(pod_id, &[]).await;
        if let Err(e) = self.apply_network().await {
            warn!("Pods: Failed to set the pods' rules: {e}");
        }
        self.release_addresses(&info).await;
        info!("Pods: {pod_id} deleted");
        Ok(())
    }

    /// Takes the pod out of the pods and removes its record, under the
    /// pods' lock in one go: no save of the pod can be queued behind the
    /// removal, which would write the record back while the removal waits
    /// for the writer. A removal that fails puts the pod back, to be
    /// deleted again, unless one with its ID was created meanwhile.
    async fn forget(&self, pod_id: &str) -> Result<Option<Pod>, Status> {
        let (pod, removed) = {
            let mut pods = self.pods();
            (pods.remove(pod_id), self.inner.store.remove(pod_id))
        };
        if let Err(e) = removed.await {
            if let Some(pod) = pod {
                self.pods().entry(pod_id.to_string()).or_insert(pod);
            }
            return Err(Status::internal(format!("forget pod {pod_id}: {e}")));
        }
        Ok(pod)
    }

    /// Writes the running pod to a snapshot; returns the size of its memory.
    /// It runs in a task of its own that holds the pod's lock: a caller that
    /// goes away, as one whose deadline passed, leaves it running to its
    /// end. Cut off between the microVM's suspend and the pod's record, it
    /// would leave a suspended microVM under a pod that runs.
    pub async fn suspend(&self, pod_id: &str) -> Result<u64, Status> {
        let pods = self.clone();
        let pod_id = pod_id.to_string();
        tokio::spawn(async move {
            let _lock = pods.lock(&pod_id).await?;
            pods.expect_state(&pod_id, PodState::Running)?;
            pods.suspend_locked(&pod_id).await
        })
        .await
        .unwrap_or_else(|e| Err(Status::internal(format!("the suspend ended: {e}"))))
    }

    /// `suspend`, under the pod's lock. The snapshot holds what the guest
    /// wrote to its disk: Firecracker flushes it first. The disk stops
    /// with the microVM.
    async fn suspend_locked(&self, pod_id: &str) -> Result<u64, Status> {
        // Its guest does not answer while the snapshot is written.
        let _busy = self.mark_busy(pod_id, "the pod is being suspended");
        let request = Request::new(SuspendVmRequest {
            vm_id: pod_id.to_string(),
            purge_guest: true,
        });
        let response = self.inner.vms.suspend_vm(request).await?.into_inner();
        self.settle(pod_id, VmState::Suspended).await?;
        self.release_disk(pod_id).await;
        self.set_state(pod_id, PodState::Suspended, "suspended")
            .await;
        Ok(response.memory_bytes)
    }

    /// Restores a suspended pod from its snapshot, or a hibernated one from
    /// its checkpoint. A resume that comes while the pod hibernates
    /// abandons the hibernation if it may, and resumes the pod from its
    /// snapshot once the hibernation stopped; otherwise it waits for the
    /// hibernation, and finds the pod hibernated. It runs in a task of its
    /// own (`detached`).
    pub async fn resume(&self, pod_id: &str) -> Result<(), Status> {
        let pod_id = pod_id.to_string();
        self.detached(move |pods| async move { pods.resume_detached(&pod_id).await })
            .await
    }

    async fn resume_detached(&self, pod_id: &str) -> Result<(), Status> {
        let lock = self.lock_for_resume(pod_id).await?;
        if self.info(pod_id)?.state() == PodState::Hibernated {
            drop(lock);
            self.restore_detached(pod_id, "", false).await?;
            return Ok(());
        }
        self.expect_state(pod_id, PodState::Suspended)?;
        let attachment = self.attach(pod_id).await?;
        self.open_disk(pod_id).await?;
        self.open_volumes(pod_id).await?;
        let request = Request::new(ResumeVmRequest {
            vm_id: pod_id.to_string(),
            remote: None,
        });
        self.inner.vms.resume_vm(request).await?;
        self.settle(pod_id, VmState::Running).await?;
        self.set_state(pod_id, PodState::Running, "").await;
        self.check_microvm_network(pod_id, attachment.as_ref())
            .await;
        Ok(())
    }

    /// Takes the pod off the node into object storage (ADR 0008), in a task
    /// of its own that holds the pod's lock: a HibernatePod that comes
    /// while it runs joins it and gets its answer, and one whose caller
    /// goes away leaves it running. Until its heads are being put, a resume
    /// abandons it, unless a HibernatePod said `no_abandon`: it is answered
    /// ABORTED then, and so is a later HibernatePod with one of its
    /// `request_id`s. Until its head is up, the pod stays on the node,
    /// suspended if it ran.
    pub async fn hibernate(&self, pod_id: &str, request_id: &str, no_abandon: bool) -> Answer {
        let answer = {
            let mut pods = self.pods();
            let pod = pods
                .get_mut(pod_id)
                .ok_or_else(|| Status::not_found(format!("no pod {pod_id}")))?;
            match join(
                &mut pod.hibernation,
                &pod.abandoned,
                pod_id,
                request_id,
                no_abandon,
            )? {
                Some(answer) => answer,
                None => {
                    let (abandon, asked) = watch::channel(false);
                    let token = Arc::new(());
                    let task = tokio::spawn({
                        let pods = self.clone();
                        let pod_id = pod_id.to_string();
                        let token = token.clone();
                        async move { pods.run_hibernation(&pod_id, token, asked).await }
                    });
                    let id = pod_id.to_string();
                    let answer = async move { joined(&id, task.await) }.boxed().shared();
                    pod.hibernation = Some(Hibernation::new(
                        token,
                        answer.clone(),
                        abandon,
                        no_abandon,
                        request_id,
                    ));
                    answer
                }
            }
        };
        // A caller that goes away drops this, never the task.
        answer.await
    }

    /// A hibernation's task: it takes the pod's lock and hibernates the
    /// pod, and however it ends, a panic too, ends its slot before it lets
    /// go of the lock. A hibernation that a resume asked to abandon, and
    /// that did not hibernate the pod, was abandoned.
    async fn run_hibernation(
        &self,
        pod_id: &str,
        token: Arc<()>,
        abandon: watch::Receiver<bool>,
    ) -> Answer {
        let mut slot = SlotGuard {
            end: Some(Box::new({
                let pods = self.clone();
                let pod_id = pod_id.to_string();
                let token = token.clone();
                move |abandoned| {
                    if let Some(pod) = pods.pods().get_mut(&pod_id) {
                        end_slot(&mut pod.hibernation, &mut pod.abandoned, &token, abandoned);
                    }
                }
            })),
            abandoned: false,
            lock: None,
        };
        slot.lock = Some(self.lock(pod_id).await?);
        let mut run = Run {
            pod_id,
            token,
            abandon,
            obeys: false,
            phase: HibernationPhase::Waiting,
        };
        match self.hibernate_locked(&mut run).await {
            Err(e) if run.asked() => {
                if e.code() != Code::Aborted {
                    warn!(
                        "Pods: {pod_id}'s hibernation failed while a resume abandoned it: {}",
                        e.message()
                    );
                }
                info!(
                    "Pods: {pod_id}'s hibernation is abandoned for a resume, in {}",
                    run.phase.as_str_name()
                );
                slot.abandoned = true;
                Err(abandoned(pod_id))
            }
            Err(e) => {
                warn!("Pods: {pod_id}'s hibernation failed: {}", e.message());
                Err(e)
            }
            Ok(response) => Ok(response),
        }
    }

    /// The hibernation, under the pod's lock, step by step. Until its heads
    /// are being put, a resume may abandon it: it stops then between two
    /// steps, or drops the call to diskd that seals or uploads, and leaves
    /// the pod as the steps before it left it, suspended on the node with
    /// its disk sealed or not. diskd stops a dropped seal on the node
    /// before it commits the new layer, which leaves the disk as it was,
    /// or finishes it once it has; the resume's OpenDisk waits for either.
    /// The suspend, which a dropped call would cut, runs to its end.
    async fn hibernate_locked(&self, run: &mut Run<'_>) -> Answer {
        let pod_id = run.pod_id;
        let info = self.info(pod_id)?;
        if info.state() == PodState::Hibernated {
            return Ok(HibernatePodResponse {
                checkpoint: info.checkpoint,
                ..Default::default()
            });
        }
        // Before anything changes: without one, the pod stays as it is.
        self.inner.disks.require_object_store(pod_id).await?;
        let memory = match info.state() {
            PodState::Running => true,
            // One whose snapshot is gone has its disk.
            PodState::Suspended => snapshot::exists(pod_id),
            PodState::Stopped | PodState::Failed => false,
            state => {
                return Err(Status::failed_precondition(format!(
                    "pod {pod_id} is {}",
                    state.as_str_name()
                )));
            }
        };
        // Only a pod whose memory goes with it can be resumed instead.
        self.set_memory(run, memory);
        run.check()?;
        if info.state() == PodState::Running {
            self.set_phase(run, HibernationPhase::Suspending);
            self.set_state(pod_id, PodState::Running, "hibernating")
                .await;
            if let Err(e) = self.suspend_locked(pod_id).await {
                if self.info(pod_id)?.state() == PodState::Running {
                    self.set_state(pod_id, PodState::Running, "").await;
                }
                return Err(e);
            }
            run.check()?;
        }
        if !memory {
            // A failed pod's microVM may have left its disk started.
            self.delete_vm(pod_id).await?;
        }
        self.inner.disks.stop(pod_id).await?;
        let spec = info.spec.clone().unwrap_or_default();
        let epoch = next_epoch(self.checkpoint_epochs(&info));
        self.set_epoch(run, epoch);
        info!("Pods: {pod_id} hibernates, epoch {epoch}");
        run.check()?;
        self.set_phase(run, HibernationPhase::Sealing);
        run.abandonable(self.inner.disks.seal_local(pod_id)).await?;
        run.check()?;
        self.set_phase(run, HibernationPhase::UploadingDisk);
        let disk = run.abandonable(self.inner.disks.seal(pod_id)).await?;
        self.set_phase(run, HibernationPhase::Volumes);
        let volumes = self.seal_volumes(run, &spec).await?;
        self.set_phase(run, HibernationPhase::UploadingMemory);
        let mut memory_bytes = 0;
        let memory = if memory {
            let host = snapshot::Host::load(pod_id)
                .map_err(|e| Status::internal(format!("the snapshot's host record: {e}")))?;
            let layer = run
                .abandonable(self.inner.disks.seal_memory(&snapshot::memory(pod_id)))
                .await?;
            memory_bytes = layer.logical_bytes;
            let vmstate_key = checkpoint_key(pod_id, epoch, "vmstate");
            run.abandonable(
                self.inner
                    .disks
                    .put_object(&vmstate_key, &snapshot::vmstate(pod_id)),
            )
            .await?;
            Some(MemoryRef {
                layer: layer.layer_id,
                vmstate_key,
                firecracker_version: host.firecracker_version,
                cpu_model: host.cpu_model,
                kernel_release: host.kernel_release,
            })
        } else {
            None
        };
        // The spec, for a node that does not have the pod.
        run.abandonable(self.put_record(
            &format!("{pod_id}.spec"),
            &checkpoint_key(pod_id, epoch, SPEC_OBJECT),
            &spec.encode_to_vec(),
        ))
        .await?;

        // The point of no return: a head that was asked for may be up, so
        // from here on a resume waits for the hibernation.
        self.enter_head(run)?;
        let (volume_heads, volume_checkpoints) = self.head_volumes(pod_id, epoch, volumes).await?;
        let head = self
            .inner
            .disks
            .put_head(pod_id, epoch, memory, volume_heads)
            .await?;
        self.set_phase(run, HibernationPhase::Leaving);
        let checkpoint = Checkpoint {
            head_key: head.key,
            epoch,
            memory: head.memory.is_some(),
        };
        info!("Pods: {pod_id} is in {}", checkpoint.head_key);
        // Its volumes are in it, too.
        for (volume_id, volume_checkpoint) in volume_checkpoints {
            let checkpointed = self
                .inner
                .volumes
                .update(&volume_id, |volume| {
                    volume.state = VolumeState::Checkpointed as i32;
                    volume.checkpoint = Some(volume_checkpoint);
                })
                .await;
            if let Err(e) = checkpointed {
                warn!(
                    "Pods: Failed to record that volume {volume_id} is checkpointed: {}",
                    e.message()
                );
            }
        }

        // From here on, the checkpoint is the pod. It leaves the network
        // as a deleted pod does, its addresses last.
        let placed = self.info(pod_id)?;
        if let Err(e) = self.inner.network.detach(&placed).await {
            warn!("Pods: Failed to take {pod_id} off the network: {e}");
        }
        self.update(pod_id, |info| {
            info.checkpoint = Some(checkpoint.clone());
            info.ipv6_address.clear();
            info.ipv4_address.clear();
        })
        .await;
        self.set_state(pod_id, PodState::Hibernated, "hibernated")
            .await;
        self.purge(pod_id).await;
        if let Err(e) = self.apply_network().await {
            warn!("Pods: Failed to set the pods' rules: {e}");
        }
        self.release_addresses(&placed).await;
        Ok(HibernatePodResponse {
            checkpoint: Some(checkpoint),
            memory_bytes,
            disk_bytes: disk.logical_bytes,
        })
    }

    /// Removes what a hibernated pod held on the node: its microVM with
    /// its snapshot, its disk and its TAP device. What fails is logged and
    /// left; the next start of feosd tries again.
    async fn purge(&self, pod_id: &str) {
        if let Err(e) = self.delete_vm(pod_id).await {
            warn!("Pods: Failed to remove {pod_id}'s microVM: {}", e.message());
        }
        self.inner.disks.close_memory(pod_id).await;
        if let Err(e) = self.inner.disks.remove(pod_id).await {
            warn!("Pods: Failed to remove {pod_id}'s disk: {}", e.message());
        }
        let Ok(info) = self.info(pod_id) else {
            return;
        };
        // The volumes in its checkpoint.
        for wanted in &info.spec.clone().unwrap_or_default().volumes {
            let checkpointed = self
                .inner
                .volumes
                .get(&wanted.volume_id)
                .is_ok_and(|volume| volume.state() == VolumeState::Checkpointed);
            if checkpointed
                && let Err(e) = self
                    .inner
                    .disks
                    .remove(&volume_disk(&wanted.volume_id))
                    .await
            {
                warn!(
                    "Pods: Failed to remove the disk of {pod_id}'s volume {}: {}",
                    wanted.name,
                    e.message()
                );
            }
        }
        if let Err(e) = self.inner.network.detach(&info).await {
            warn!("Pods: Failed to take {pod_id} off the network: {e}");
        }
    }

    /// Brings a hibernated pod back from the checkpoint `head_key`, or its
    /// own: on this node, or on one that does not have the pod, which
    /// takes its spec from the checkpoint. The memory is restored where
    /// the node can load the snapshot, unless `cold`; otherwise, and if the
    /// restore of the memory fails before the guest ran, the pod boots
    /// anew on the checkpoint's disk. It runs in a task of its own
    /// (`detached`).
    pub async fn restore(
        &self,
        pod_id: &str,
        head_key: &str,
        cold: bool,
    ) -> Result<RestorePodResponse, Status> {
        let (pod_id, head_key) = (pod_id.to_string(), head_key.to_string());
        self.detached(
            move |pods| async move { pods.restore_detached(&pod_id, &head_key, cold).await },
        )
        .await
    }

    async fn restore_detached(
        &self,
        pod_id: &str,
        head_key: &str,
        cold: bool,
    ) -> Result<RestorePodResponse, Status> {
        spec::check_id(pod_id)?;
        if self.info(pod_id).is_err() {
            self.adopt_checkpoint(pod_id, head_key).await?;
        }
        let lock = self.end_bring_up_and_lock(pod_id).await?;
        let info = self.info(pod_id)?;
        let own = info.checkpoint.as_ref().map(|c| c.head_key.clone());
        let key = match head_key {
            "" => own.clone().unwrap_or_default(),
            key => key.to_string(),
        };
        match info.state() {
            PodState::Hibernated => {}
            // Restored already, by a call whose caller lost the answer.
            PodState::Pending | PodState::Running if own.as_deref() == Some(key.as_str()) => {
                return Ok(RestorePodResponse {
                    pod: Some(info),
                    ..Default::default()
                });
            }
            state => {
                return Err(Status::failed_precondition(format!(
                    "pod {pod_id} is {}, not hibernated",
                    state.as_str_name()
                )));
            }
        }
        if key.is_empty() {
            return Err(Status::failed_precondition(format!(
                "pod {pod_id} has no checkpoint"
            )));
        }
        let head = self.inner.disks.get_head(&key).await?;
        if head.pod_id != pod_id {
            return Err(Status::invalid_argument(format!(
                "{key} is pod {}'s, not {pod_id}'s",
                head.pod_id
            )));
        }
        // Its volumes come from the checkpoint, too, or the restore is
        // refused before anything changes.
        let spec = info.spec.clone().unwrap_or_default();
        let volumes = self.checkpoint_volumes(pod_id, &spec, &head).await?;

        // A disk the node has of the pod is the checkpoint's at best, and
        // so are its volumes'.
        self.inner.disks.remove(pod_id).await?;
        for volume in &volumes {
            self.inner
                .disks
                .remove(&volume_disk(&volume.volume_id))
                .await?;
        }
        self.inner.volumes.take_over(pod_id, volumes).await?;
        self.place(pod_id).await?;
        self.update(pod_id, |info| {
            info.checkpoint = Some(Checkpoint {
                head_key: key.clone(),
                epoch: head.epoch,
                memory: head.memory.is_some(),
            });
        })
        .await;
        self.set_state(pod_id, PodState::Pending, &format!("restoring from {key}"))
            .await;
        if let Err(e) = self.apply_network().await {
            warn!("Pods: Failed to set the pods' rules: {e}");
        }

        let reason = match why_cold(cold, head.memory.as_ref()) {
            Some(reason) => reason,
            None => match self.restore_memory(pod_id, &head).await {
                Ok(()) => {
                    self.set_state(pod_id, PodState::Running, "").await;
                    info!("Pods: {pod_id} is restored from {key}, with its memory");
                    return Ok(RestorePodResponse {
                        pod: Some(self.info(pod_id)?),
                        memory_restored: true,
                        reason: String::new(),
                    });
                }
                Err(Restore::Booted(e)) => {
                    // The guest runs: what is left to do is a bring-up's.
                    warn!("Pods: {pod_id} runs from {key}, but: {}", e.message());
                    drop(lock);
                    self.spawn_bring_up(pod_id);
                    return Err(e);
                }
                Err(Restore::NotBooted(e)) => {
                    warn!("Pods: {pod_id}'s memory is not restored: {}", e.message());
                    format!("its memory did not restore: {}", e.message())
                }
            },
        };
        info!("Pods: {pod_id} boots anew on the disk of {key}: {reason}");
        self.set_state(
            pod_id,
            PodState::Pending,
            &format!("booting anew on its disk: {reason}"),
        )
        .await;
        drop(lock);
        self.spawn_bring_up(pod_id);
        Ok(RestorePodResponse {
            pod: Some(self.info(pod_id)?),
            memory_restored: false,
            reason,
        })
    }

    /// Records a pod the node does not have, hibernated, from its
    /// checkpoint: its spec is beside the head.
    async fn adopt_checkpoint(&self, pod_id: &str, head_key: &str) -> Result<(), Status> {
        if head_key.is_empty() {
            return Err(Status::not_found(format!(
                "no pod {pod_id}: name its checkpoint's head to restore it here"
            )));
        }
        let head = self.inner.disks.get_head(head_key).await?;
        if head.pod_id != pod_id {
            return Err(Status::invalid_argument(format!(
                "{head_key} is pod {}'s, not {pod_id}'s",
                head.pod_id
            )));
        }
        let path = self.inner.dir.join(format!("{pod_id}.spec"));
        let key = checkpoint_key(pod_id, head.epoch, SPEC_OBJECT);
        let fetched = self.inner.disks.get_object(&key, &path).await;
        let read = fetched.and_then(|()| {
            std::fs::read(&path).map_err(|e| Status::internal(format!("read {key}: {e}")))
        });
        let _ = std::fs::remove_file(&path);
        let spec = PodSpec::decode(read?.as_slice())
            .map_err(|e| Status::invalid_argument(format!("{key}: {e}")))?;
        let spec = spec::normalize(spec)?;
        self.inner.network.check(&spec)?;
        // Before the pod is recorded: a restore that cannot take its
        // volumes leaves nothing on the node.
        self.checkpoint_volumes(pod_id, &spec, &head).await?;
        let info = {
            let info = PodInfo {
                pod_id: pod_id.to_string(),
                spec: Some(spec),
                state: PodState::Hibernated as i32,
                reason: "hibernated".to_string(),
                checkpoint: Some(Checkpoint {
                    head_key: head_key.to_string(),
                    epoch: head.epoch,
                    memory: head.memory.is_some(),
                }),
                ..Default::default()
            };
            match self.add(info, false).await? {
                Ok(info) => info,
                // Another restore was faster.
                Err(_) => return Ok(()),
            }
        };
        info!("Pods: {pod_id} comes from {head_key}");
        self.announce(&info, false);
        Ok(())
    }

    /// Restores the pod's memory from its checkpoint into a microVM that
    /// never booted, on the checkpoint's disk: diskd serves the memory as
    /// the guest asks for it.
    async fn restore_memory(&self, pod_id: &str, head: &Head) -> Result<(), Restore> {
        let memory = head.memory.clone().unwrap_or_default();
        let vmstate = self.inner.dir.join(format!("{pod_id}.vmstate"));
        let prepared = async {
            let info = self.info(pod_id)?;
            let attachment = self.attach(pod_id).await?;
            let disk = self.inner.disks.open_checkpoint(pod_id, &head.key).await?;
            // The snapshot's drives: its volumes, from their heads.
            let volumes = self.open_volumes(pod_id).await?;
            let config = spec::vm_config(
                pod_id,
                &info.spec.unwrap_or_default(),
                attachment.as_ref(),
                Some(&disk),
                &volumes,
            );
            self.delete_vm(pod_id).await?;
            let request = Request::new(CreateVmRequest {
                config: Some(config),
                vm_id: Some(pod_id.to_string()),
            });
            self.inner.vms.create_vm(request).await?;
            self.settle(pod_id, VmState::Created).await?;
            self.inner
                .disks
                .get_object(&memory.vmstate_key, &vmstate)
                .await?;
            self.inner
                .disks
                .open_memory(pod_id, &memory.layer, &memory_socket_path(pod_id))
                .await?;
            let request = Request::new(ResumeVmRequest {
                vm_id: pod_id.to_string(),
                remote: Some(RemoteSnapshot {
                    vmstate_path: vmstate.to_string_lossy().into_owned(),
                    firecracker_version: memory.firecracker_version.clone(),
                    cpu_model: memory.cpu_model.clone(),
                    kernel_release: memory.kernel_release.clone(),
                }),
            });
            self.inner.vms.resume_vm(request).await.map(drop)
        }
        .await;
        let _ = std::fs::remove_file(&vmstate);
        if let Err(e) = prepared {
            // The guest did not run: the disk is as the checkpoint has it,
            // for a boot.
            self.inner.disks.close_memory(pod_id).await;
            if let Err(e) = self.delete_vm(pod_id).await {
                warn!("Pods: Failed to remove {pod_id}'s microVM: {}", e.message());
            }
            return Err(Restore::NotBooted(e));
        }
        self.settle(pod_id, VmState::Running)
            .await
            .map_err(Restore::Booted)
    }

    /// Gives a pod that is to come back onto the node what it gave up when
    /// it hibernated: its addresses.
    /// The caller holds the pod's lock: nothing else changes its record.
    /// It runs in a task of its own under `records`, as `add` does.
    async fn place(&self, pod_id: &str) -> Result<(), Status> {
        let (pods, pod_id) = (self.clone(), pod_id.to_string());
        tokio::spawn(async move {
            let _records = pods.inner.records.lock().await;
            let info = pods.info(&pod_id)?;
            if !info.spec.as_ref().is_some_and(spec::has_network) || !info.ipv6_address.is_empty() {
                return Ok(());
            }
            let info = pods.allocate(info).await?;
            if let Err(e) = pods.inner.store.save(&info).await {
                pods.release_addresses(&info).await;
                return Err(Status::internal(format!("keep pod {pod_id}: {e}")));
            }
            if let Some(pod) = pods.pods().get_mut(&pod_id) {
                pod.info.ipv6_address = info.ipv6_address;
                pod.info.ipv4_address = info.ipv4_address;
            }
            Ok(())
        })
        .await
        .unwrap_or_else(|e| Err(Status::internal(format!("placing the pod ended: {e}"))))
    }

    /// Stops the pod's disk and its volumes' when its microVM has ended,
    /// and lets go of the memory diskd served it. What fails is logged: the
    /// disk's next start takes what is there.
    async fn release_disk(&self, pod_id: &str) {
        self.inner.disks.close_memory(pod_id).await;
        if let Err(e) = self.inner.disks.stop(pod_id).await {
            warn!("Pods: Failed to stop {pod_id}'s disk: {}", e.message());
        }
        if let Ok(info) = self.info(pod_id) {
            self.stop_volumes(&info).await;
        }
    }

    /// Stops the disks of the pod's volumes, which keep what it wrote.
    async fn stop_volumes(&self, info: &PodInfo) {
        for wanted in &info.spec.clone().unwrap_or_default().volumes {
            let disk = volume_disk(&wanted.volume_id);
            if let Err(e) = self.inner.disks.stop(&disk).await {
                warn!(
                    "Pods: Failed to stop the disk of {}'s volume {}: {}",
                    info.pod_id,
                    wanted.name,
                    e.message()
                );
            }
        }
    }

    /// Starts the pod's disk: the one the node has, or its checkpoint's,
    /// or a new one. Returns the path Firecracker takes.
    async fn open_disk(&self, pod_id: &str) -> Result<String, Status> {
        let info = self.info(pod_id)?;
        let head_key = info
            .checkpoint
            .as_ref()
            .map(|checkpoint| checkpoint.head_key.as_str())
            .filter(|key| !key.is_empty());
        let image = bake::image_of(&info);
        let (device, made) = self.inner.disks.open(pod_id, head_key, image).await?;
        // A new disk: what it was made on is the pod's, for its bake
        // point and for the control plane.
        if let Some(disk) = made {
            self.update(pod_id, |info| info.disk = Some(disk)).await;
        }
        Ok(device)
    }

    /// Starts the disks of the pod's volumes, in the order of its spec: the
    /// ones the node has, or their checkpoints', or blank ones for volumes
    /// without their filesystem. Returns the paths Firecracker takes.
    async fn open_volumes(&self, pod_id: &str) -> Result<Vec<String>, Status> {
        let spec = self.info(pod_id)?.spec.unwrap_or_default();
        let mut paths = Vec::with_capacity(spec.volumes.len());
        for wanted in &spec.volumes {
            let failed = |e: Status| {
                Status::new(e.code(), format!("volume {}: {}", wanted.name, e.message()))
            };
            let volume = self.inner.volumes.get(&wanted.volume_id).map_err(|_| {
                Status::failed_precondition(format!(
                    "volume {}: no volume {} on this node",
                    wanted.name, wanted.volume_id
                ))
            })?;
            if volume.pod_id != pod_id {
                return Err(Status::failed_precondition(format!(
                    "volume {}: {} is attached to pod {:?}",
                    wanted.name, volume.volume_id, volume.pod_id
                )));
            }
            let path = self
                .inner
                .disks
                .open_volume(&volume, self.inner.volumes.dir())
                .await
                .map_err(failed)?;
            if volume.state() != VolumeState::Attached {
                self.inner
                    .volumes
                    .update(&volume.volume_id, |volume| {
                        volume.state = VolumeState::Attached as i32;
                    })
                    .await
                    .map_err(failed)?;
            }
            paths.push(path);
        }
        Ok(paths)
    }

    /// Has the guest mount the pod's volumes, and format the ones the node
    /// knows to be new, and records which have their filesystem now. Fails
    /// unless every volume is mounted.
    async fn mount_volumes(&self, pod_id: &str, spec: &PodSpec) -> Result<(), Status> {
        if spec.volumes.is_empty() {
            return Ok(());
        }
        let mut request = MountVolumesRequest::default();
        for (index, wanted) in spec.volumes.iter().enumerate() {
            let volume = self.inner.volumes.get(&wanted.volume_id)?;
            request.volumes.push(GuestVolume {
                name: wanted.name.clone(),
                volume_id: volume.volume_id,
                // The pod's disk is the first drive.
                drive: index as u32 + 1,
                size_bytes: volume.size_bytes,
                format: !volume.formatted,
            });
        }
        let mut guest = GuestServiceClient::new(self.guest_channel(pod_id).await?);
        let response = timeout(MOUNT_TIMEOUT, guest.mount_volumes(request))
            .await
            .map_err(|_| {
                Status::deadline_exceeded(format!(
                    "the guest did not mount the volumes within {}s",
                    MOUNT_TIMEOUT.as_secs()
                ))
            })?
            .map_err(|e| match e.code() {
                Code::Unimplemented => Status::failed_precondition(
                    "the guest mounts no volumes: it is older than volumes are",
                ),
                code => Status::new(code, format!("the guest's volumes: {}", e.message())),
            })?
            .into_inner();
        let mut failed = Vec::new();
        if response.volumes.len() != spec.volumes.len() {
            failed.push(format!(
                "the guest answered for {} of {} volumes",
                response.volumes.len(),
                spec.volumes.len()
            ));
        }
        for (wanted, mounted) in spec.volumes.iter().zip(&response.volumes) {
            if !mounted.error.is_empty() {
                failed.push(format!("volume {}: {}", wanted.name, mounted.error));
                continue;
            }
            if mounted.formatted {
                info!(
                    "Pods: {pod_id}'s guest made the filesystem of volume {} on {}",
                    wanted.name, mounted.device
                );
            }
            if !self.inner.volumes.get(&wanted.volume_id)?.formatted {
                self.inner
                    .volumes
                    .update(&wanted.volume_id, |volume| volume.formatted = true)
                    .await?;
            }
        }
        if !failed.is_empty() {
            return Err(Status::failed_precondition(failed.join("; ")));
        }
        Ok(())
    }

    /// Seals and uploads each of the pod's volumes for a hibernation's
    /// checkpoint, in the order of the pod's spec: its chain, whose head
    /// `head_volumes` puts. A volume that is checkpointed already, of a pod
    /// that has not booted since, keeps its head. A resume may abandon the
    /// hibernation between two volumes, and during a seal or an upload.
    async fn seal_volumes(
        &self,
        run: &mut Run<'_>,
        spec: &PodSpec,
    ) -> Result<Vec<VolumePart>, Status> {
        let pod_id = run.pod_id;
        let mut parts = Vec::with_capacity(spec.volumes.len());
        for wanted in &spec.volumes {
            run.check()?;
            let volume = self.inner.volumes.get(&wanted.volume_id)?;
            if volume.pod_id != pod_id {
                return Err(Status::failed_precondition(format!(
                    "volume {} is attached to pod {:?}",
                    volume.volume_id, volume.pod_id
                )));
            }
            if volume.state() == VolumeState::Checkpointed
                && let Some(checkpoint) = &volume.checkpoint
            {
                parts.push(VolumePart::Kept(checkpoint.head_key.clone()));
                continue;
            }
            let disk = volume_disk(&volume.volume_id);
            // One that never booted gets its blank disk now.
            self.inner
                .disks
                .open_volume(&volume, self.inner.volumes.dir())
                .await?;
            self.inner.disks.stop(&disk).await?;
            run.abandonable(self.inner.disks.seal_local(&disk)).await?;
            run.check()?;
            run.abandonable(self.inner.disks.seal(&disk)).await?;
            parts.push(VolumePart::Sealed {
                name: wanted.name.clone(),
                volume,
            });
        }
        Ok(parts)
    }

    /// Puts the record and the head of each volume that `seal_volumes`
    /// sealed into the checkpoint `epoch`, past the hibernation's point of
    /// no return: the pod's head, put next, names them. Returns the heads
    /// of all the pod's volumes, and the new checkpoints of the sealed ones.
    async fn head_volumes(
        &self,
        pod_id: &str,
        epoch: u64,
        parts: Vec<VolumePart>,
    ) -> Result<(Vec<String>, Vec<(String, VolumeCheckpoint)>), Status> {
        let mut heads = Vec::new();
        let mut checkpoints = Vec::new();
        for part in parts {
            let (name, volume) = match part {
                VolumePart::Kept(head) => {
                    heads.push(head);
                    continue;
                }
                VolumePart::Sealed { name, volume } => (name, volume),
            };
            let disk = volume_disk(&volume.volume_id);
            // Its record, for a node that does not have it.
            let record = Volume {
                volume_id: volume.volume_id.clone(),
                size_bytes: volume.size_bytes,
                labels: volume.labels.clone(),
                formatted: volume.formatted,
                ..Default::default()
            };
            self.put_record(
                &format!("{disk}.volume"),
                &checkpoint_key(&disk, epoch, VOLUME_OBJECT),
                &record.encode_to_vec(),
            )
            .await?;
            let head = self
                .inner
                .disks
                .put_head(&disk, epoch, None, Vec::new())
                .await?;
            info!("Pods: {pod_id}'s volume {name} is in {}", head.key);
            heads.push(head.key.clone());
            checkpoints.push((
                volume.volume_id,
                VolumeCheckpoint {
                    head_key: head.key,
                    epoch,
                },
            ));
        }
        Ok((heads, checkpoints))
    }

    /// The pod's volumes as its checkpoint, the head `head`, has them, for a
    /// restore to take over. Fails before anything changes when the
    /// checkpoint lacks a volume, or when the node has one that a restore
    /// may not take: attached to another pod, or with data of its own.
    async fn checkpoint_volumes(
        &self,
        pod_id: &str,
        spec: &PodSpec,
        head: &Head,
    ) -> Result<Vec<Volume>, Status> {
        let mut volumes = Vec::with_capacity(spec.volumes.len());
        for wanted in &spec.volumes {
            if let Ok(had) = self.inner.volumes.get(&wanted.volume_id) {
                volumes::check_restorable(pod_id, &had)?;
            }
            let disk = volume_disk(&wanted.volume_id);
            let prefix = format!("snapshots/{disk}/");
            let key = head
                .volume_heads
                .iter()
                .find(|key| key.starts_with(&prefix))
                .ok_or_else(|| {
                    Status::failed_precondition(format!(
                        "{} has no head of volume {}",
                        head.key, wanted.volume_id
                    ))
                })?;
            let volume_head = self.inner.disks.get_head(key).await?;
            let record = self
                .get_record(
                    &format!("{disk}.volume"),
                    &checkpoint_key(&disk, volume_head.epoch, VOLUME_OBJECT),
                )
                .await?;
            let record = Volume::decode(record.as_slice())
                .map_err(|e| Status::invalid_argument(format!("{key}'s record: {e}")))?;
            volumes.push(Volume {
                volume_id: wanted.volume_id.clone(),
                size_bytes: volume_head.logical_size,
                labels: record.labels,
                state: VolumeState::Checkpointed as i32,
                pod_id: pod_id.to_string(),
                formatted: record.formatted,
                checkpoint: Some(VolumeCheckpoint {
                    head_key: key.clone(),
                    epoch: volume_head.epoch,
                }),
            });
        }
        Ok(volumes)
    }

    /// Puts `bytes` into the object store at `key`, through a file `name`
    /// in the pods' directory.
    async fn put_record(&self, name: &str, key: &str, bytes: &[u8]) -> Result<(), Status> {
        let path = self.inner.dir.join(name);
        put_through_file(&path, bytes, |path| self.inner.disks.put_object(key, path)).await
    }

    /// Gets the object at `key` from the object store, through a file
    /// `name` in the pods' directory.
    async fn get_record(&self, name: &str, key: &str) -> Result<Vec<u8>, Status> {
        let path = self.inner.dir.join(name);
        let fetched = self.inner.disks.get_object(key, &path).await;
        let read = fetched.and_then(|()| {
            std::fs::read(&path).map_err(|e| Status::internal(format!("read {key}: {e}")))
        });
        let _ = std::fs::remove_file(&path);
        read
    }

    /// The epochs of the pod's last checkpoint and of its volumes': its
    /// next is past all of them.
    fn checkpoint_epochs(&self, info: &PodInfo) -> Vec<u64> {
        let mut epochs: Vec<u64> = info.checkpoint.iter().map(|c| c.epoch).collect();
        for wanted in &info.spec.clone().unwrap_or_default().volumes {
            if let Ok(volume) = self.inner.volumes.get(&wanted.volume_id) {
                epochs.extend(volume.checkpoint.map(|checkpoint| checkpoint.epoch));
            }
        }
        epochs
    }

    /// Whether a pod on the node names the security group.
    fn names_group(&self, id: &str) -> bool {
        self.pods().values().any(|pod| {
            let network = pod
                .info
                .spec
                .as_ref()
                .and_then(|spec| spec.network.as_ref());
            network.is_some_and(|network| network.security_groups.iter().any(|g| g == id))
        })
    }

    /// Whether a pod on the node names the name table.
    fn names_table(&self, id: &str) -> bool {
        self.pods().values().any(|pod| {
            let network = pod
                .info
                .spec
                .as_ref()
                .and_then(|spec| spec.network.as_ref());
            network.is_some_and(|network| network.name_tables.iter().any(|t| t == id))
        })
    }

    /// Puts a pod that runs from before feosd started on the network again.
    async fn reattach(&self, pod_id: &str) -> Result<(), Status> {
        let attachment = self.attach(pod_id).await?;
        self.check_microvm_network(pod_id, attachment.as_ref())
            .await;
        Ok(())
    }

    /// Brings a running microVM that was made for another network in line.
    /// Where only its guest's configuration differs, such as one of IPv6
    /// alone with the resolver of that version, the guest is pushed this
    /// one and takes it at once. A microVM whose devices differ, such as
    /// the first version's with IPv4 on a bridge, keeps its network until
    /// the pod boots anew, and so does one from before pods had a metadata
    /// service; feosd warns.
    async fn check_microvm_network(&self, pod_id: &str, attachment: Option<&Attachment>) {
        let (Ok(info), Ok(Some(vm))) = (self.info(pod_id), self.vm(pod_id).await) else {
            return;
        };
        let config = vm.config.clone().unwrap_or_default();
        let disk = config.data_disk.as_ref().map(|disk| disk.path.as_str());
        let volumes: Vec<String> = config.volumes.iter().map(|v| v.path.clone()).collect();
        let wanted = spec::vm_config(
            pod_id,
            &info.spec.unwrap_or_default(),
            attachment,
            disk,
            &volumes,
        );
        if !stale(&vm, &wanted) {
            return;
        }
        if config.net != wanted.net {
            warn!(
                "Pods: {pod_id}'s microVM was made for another network; a stop and a start give it this one"
            );
            return;
        }
        if config.guest_network != wanted.guest_network {
            match wanted.guest_network {
                Some(network) => match self.inner.vms.set_guest_network(pod_id, network).await {
                    Ok(true) => info!("Pods: {pod_id}'s guest took this network"),
                    Ok(false) => {
                        info!("Pods: {pod_id}'s guest takes this network when it attaches")
                    }
                    Err(e) => warn!(
                        "Pods: {pod_id}'s guest keeps another network for now: {}",
                        e.message()
                    ),
                },
                None => warn!(
                    "Pods: {pod_id}'s microVM was made for another network; a stop and a start give it this one"
                ),
            }
        }
        if config.metadata_json != wanted.metadata_json {
            warn!(
                "Pods: {pod_id}'s microVM was made for other metadata; a stop and a start give it this one"
            );
        }
    }

    /// The pod, with its hibernation in flight, and its microVM and its
    /// containers as the guest reports them if it runs.
    pub async fn get(&self, pod_id: &str) -> Result<PodInfo, Status> {
        let mut info = self
            .pods()
            .get(pod_id)
            .map(Pod::shown)
            .ok_or_else(|| Status::not_found(format!("no pod {pod_id}")))?;
        if info.state() == PodState::Running {
            info.microvm = self.microvm(pod_id).await;
            info.containers = self.container_status(&info).await;
        }
        Ok(info)
    }

    /// The serial console of the pod's microVM. The first message names the
    /// pod.
    pub async fn console(
        &self,
        mut input: impl Stream<Item = Result<PodConsoleInput, Status>> + Send + Unpin + 'static,
    ) -> Result<impl Stream<Item = Result<PodConsoleOutput, Status>> + Send + 'static, Status> {
        let first = input.next().await.ok_or_else(|| {
            Status::invalid_argument("the console's first message names no pod")
        })??;
        self.check_reachable(&first.pod_id)?;
        let data = |input: Vec<u8>| StreamVmConsoleRequest {
            payload: Some(console_request::Payload::Data(ConsoleData { input })),
        };
        let mut start = vec![Ok(StreamVmConsoleRequest {
            payload: Some(console_request::Payload::Attach(AttachConsoleMessage {
                vm_id: first.pod_id,
            })),
        })];
        if !first.input.is_empty() {
            start.push(Ok(data(first.input)));
        }
        let rest = input.map(move |message| message.map(|message| data(message.input)));
        let output = self
            .inner
            .vms
            .console(Box::pin(tokio_stream::iter(start).chain(rest)))
            .await?;
        Ok(output.map(|output| {
            output.map(|output| PodConsoleOutput {
                output: output.output,
            })
        }))
    }

    /// The pods' network, which the mesh's API and the control plane's
    /// client reach through the pods.
    pub fn network(&self) -> &Network {
        &self.inner.network
    }

    pub fn list(&self) -> Vec<PodInfo> {
        let mut pods: Vec<PodInfo> = self.pods().values().map(Pod::shown).collect();
        pods.sort_by(|a, b| a.pod_id.cmp(&b.pod_id));
        pods
    }

    pub fn subscribe(&self) -> broadcast::Receiver<PodEvent> {
        self.inner.events.subscribe()
    }

    /// diskd, as the pods use it.
    pub fn disks(&self) -> &Disks {
        &self.inner.disks
    }

    /// Whether calls may go to the pod's guest: while its microVM runs.
    pub fn check_reachable(&self, pod_id: &str) -> Result<(), Status> {
        spec::check_id(pod_id)?;
        match self.info(pod_id)?.state() {
            PodState::Pending | PodState::Running => Ok(()),
            state => Err(Status::failed_precondition(format!(
                "pod {pod_id} is {}",
                state.as_str_name()
            ))),
        }
    }

    fn pods(&self) -> MutexGuard<'_, HashMap<String, Pod>> {
        self.inner.pods.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn info(&self, pod_id: &str) -> Result<PodInfo, Status> {
        self.pods()
            .get(pod_id)
            .map(|pod| pod.info.clone())
            .ok_or_else(|| Status::not_found(format!("no pod {pod_id}")))
    }

    fn expect_state(&self, pod_id: &str, state: PodState) -> Result<(), Status> {
        let actual = self.info(pod_id)?.state();
        if actual == state {
            Ok(())
        } else {
            Err(Status::failed_precondition(format!(
                "pod {pod_id} is {}, not {}",
                actual.as_str_name(),
                state.as_str_name()
            )))
        }
    }

    fn mark_busy(&self, pod_id: &str, why: &'static str) -> BusyMark {
        if let Some(pod) = self.pods().get_mut(pod_id) {
            pod.busy = Some(why);
        }
        BusyMark {
            pods: self.clone(),
            pod_id: pod_id.to_string(),
        }
    }

    /// Why the pod's microVM may not answer now: what the pod does, or
    /// else what the microVM service says, such as a snapshot that
    /// Firecracker still writes.
    async fn busy(&self, pod_id: &str) -> Option<String> {
        let busy = self.pods().get(pod_id).and_then(Pod::busy);
        match busy {
            Some(busy) => Some(busy),
            None => self.inner.vms.busy(pod_id).await,
        }
    }

    /// A channel to the API of the pod's guest. A guest that does not
    /// answer the connect within `vsock::CONNECT_TIMEOUT` fails it with
    /// `UNAVAILABLE`, and why its microVM is busy if that is known; once
    /// connected, a call has no bound but its caller's.
    pub(crate) async fn guest_channel(&self, pod_id: &str) -> Result<Channel, Status> {
        dial_guest(
            vsock_path(pod_id),
            pod_id,
            vsock::CONNECT_TIMEOUT,
            self.busy(pod_id),
        )
        .await
    }

    /// Takes the pod's lock, and makes sure the pod is still there.
    async fn lock(&self, pod_id: &str) -> Result<OwnedMutexGuard<()>, Status> {
        let lock = self
            .pods()
            .get(pod_id)
            .map(|pod| pod.lock.clone())
            .ok_or_else(|| Status::not_found(format!("no pod {pod_id}")))?;
        let guard = lock.clone().lock_owned().await;
        self.still_its_lock(pod_id, &lock)?;
        Ok(guard)
    }

    /// Fails unless the pod is there with the lock `lock`: a call that
    /// waited behind a delete of the pod does not act on a pod that was
    /// created again with its ID.
    fn still_its_lock(
        &self,
        pod_id: &str,
        lock: &Arc<tokio::sync::Mutex<()>>,
    ) -> Result<(), Status> {
        match self.pods().get(pod_id) {
            Some(pod) if Arc::ptr_eq(&pod.lock, lock) => Ok(()),
            _ => Err(Status::not_found(format!("no pod {pod_id}"))),
        }
    }

    /// Takes the pod's lock for a resume, and abandons a hibernation in
    /// flight that may be abandoned. The resume queues for the lock first,
    /// so that it takes it right after the hibernation's task, before
    /// anything that comes later.
    async fn lock_for_resume(&self, pod_id: &str) -> Result<OwnedMutexGuard<()>, Status> {
        let lock = self
            .pods()
            .get(pod_id)
            .map(|pod| pod.lock.clone())
            .ok_or_else(|| Status::not_found(format!("no pod {pod_id}")))?;
        // A first poll queues the resume, tokio's mutex being fair, unless
        // the task's budget is spent: it is not counted.
        let mut acquire = std::pin::pin!(tokio::task::unconstrained(lock.clone().lock_owned()));
        let early = futures::poll!(acquire.as_mut());
        match self.request_abandon(pod_id) {
            None => {}
            Some(Meets::Abandons(phase)) => info!(
                "Pods: A resume of {pod_id} abandons its hibernation, in {}",
                phase.as_str_name()
            ),
            Some(Meets::Waits(phase, why)) => info!(
                "Pods: A resume of {pod_id} waits for its hibernation, in {}: {why}",
                phase.as_str_name()
            ),
        }
        let guard = match early {
            Poll::Ready(guard) => guard,
            Poll::Pending => acquire.await,
        };
        self.still_its_lock(pod_id, &lock)?;
        Ok(guard)
    }

    /// What a resume does about the pod's hibernation in flight, if it has
    /// one: under the pods' mutex, as the hibernation's `enter_head`.
    fn request_abandon(&self, pod_id: &str) -> Option<Meets> {
        self.pods()
            .get(pod_id)?
            .hibernation
            .as_ref()
            .map(request_abandon)
    }

    /// Changes the pod's hibernation `token` while it is in flight.
    fn with_hibernation<R>(
        &self,
        pod_id: &str,
        token: &Arc<()>,
        change: impl FnOnce(&mut Hibernation) -> R,
    ) -> Option<R> {
        let mut pods = self.pods();
        let hibernation = pods
            .get_mut(pod_id)?
            .hibernation
            .as_mut()
            .filter(|hibernation| Arc::ptr_eq(&hibernation.token, token))?;
        Some(change(hibernation))
    }

    fn set_phase(&self, run: &mut Run<'_>, phase: HibernationPhase) {
        run.phase = phase;
        self.with_hibernation(run.pod_id, &run.token, |h| h.phase = phase);
    }

    /// Records whether the pod's memory goes with the hibernation: only
    /// then does a resume abandon it.
    fn set_memory(&self, run: &mut Run<'_>, memory: bool) {
        run.obeys = memory;
        self.with_hibernation(run.pod_id, &run.token, |h| h.memory = Some(memory));
    }

    fn set_epoch(&self, run: &mut Run<'_>, epoch: u64) {
        self.with_hibernation(run.pod_id, &run.token, |h| h.epoch = epoch);
    }

    /// Passes the hibernation's point of no return, unless a resume
    /// abandoned it first.
    fn enter_head(&self, run: &mut Run<'_>) -> Result<(), Status> {
        let pod_id = run.pod_id;
        self.with_hibernation(pod_id, &run.token, |h| enter_head(h, pod_id))
            .unwrap_or_else(|| run.check())?;
        run.phase = HibernationPhase::Head;
        Ok(())
    }

    async fn end_bring_up_and_lock(&self, pod_id: &str) -> Result<OwnedMutexGuard<()>, Status> {
        if let Some(task) = self
            .pods()
            .get_mut(pod_id)
            .and_then(|pod| pod.bring_up.take())
        {
            task.abort();
        }
        self.lock(pod_id).await
    }

    /// Changes the pod's record and keeps it, without an event. The change
    /// is asked to be kept under the pods' lock, in its order, and waited
    /// for without it (`Store`).
    async fn update(&self, pod_id: &str, change: impl FnOnce(&mut PodInfo)) {
        let saved = {
            let mut pods = self.pods();
            let Some(pod) = pods.get_mut(pod_id) else {
                return;
            };
            change(&mut pod.info);
            self.inner.store.save(&pod.info)
        };
        if let Err(e) = saved.await {
            warn!("Pods: Failed to keep {pod_id}: {e}");
        }
    }

    /// Changes the pod's state, and announces it once it is kept.
    async fn set_state(&self, pod_id: &str, state: PodState, reason: &str) {
        let (info, saved) = {
            let mut pods = self.pods();
            let Some(pod) = pods.get_mut(pod_id) else {
                return;
            };
            if pod.info.state() == state && pod.info.reason == reason {
                return;
            }
            pod.info.state = state as i32;
            pod.info.reason = reason.to_string();
            (pod.info.clone(), self.inner.store.save(&pod.info))
        };
        if let Err(e) = saved.await {
            warn!("Pods: Failed to keep {pod_id}: {e}");
        }
        if reason.is_empty() {
            info!("Pods: {pod_id} is {}", state.as_str_name());
        } else {
            info!("Pods: {pod_id} is {}: {reason}", state.as_str_name());
        }
        self.announce(&info, false);
    }

    fn announce(&self, info: &PodInfo, deleted: bool) {
        let _ = self.inner.events.send(PodEvent {
            pod_id: info.pod_id.clone(),
            state: info.state,
            reason: info.reason.clone(),
            deleted,
        });
    }

    fn spawn_bring_up(&self, pod_id: &str) {
        let pods = self.clone();
        let id = pod_id.to_string();
        let task = tokio::spawn(async move {
            let Ok(_lock) = pods.lock(&id).await else {
                return;
            };
            let Ok(info) = pods.info(&id) else {
                return;
            };
            if info.state() != PodState::Pending {
                return;
            }
            let pull_token = pods.take_pull_token(&id);
            let spec = info.spec.unwrap_or_default();
            match pods.bring_up(&id, &spec, pull_token.as_deref()).await {
                Ok(()) => pods.set_state(&id, PodState::Running, "").await,
                Err(e) => pods.set_state(&id, PodState::Failed, e.message()).await,
            }
        });
        if let Some(pod) = self.pods().get_mut(pod_id)
            && let Some(previous) = pod.bring_up.replace(task.abort_handle())
        {
            previous.abort();
        }
    }

    /// `pull_token` is what the guest's pull of the pod's image takes (ADR
    /// 0025); none: an anonymous pull.
    async fn bring_up(
        &self,
        pod_id: &str,
        spec: &PodSpec,
        pull_token: Option<&str>,
    ) -> Result<(), Status> {
        let booted = self.boot(pod_id, spec).await?;
        // A disk on a baked layer has the image: its guest pulls nothing,
        // and gets no token.
        let pull_token =
            pull_token.filter(|_| !bake::on_layer(&self.info(pod_id).unwrap_or_default()));
        self.wait_for_guest(pod_id).await?;
        self.mount_volumes(pod_id, spec).await?;
        let channel = self.guest_channel(pod_id).await?;
        self.bake_point(pod_id, spec, channel.clone(), pull_token)
            .await?;
        let mut guest = ContainerServiceClient::new(channel);
        for container in &spec.containers {
            run_container(&mut guest, container, booted, pull_token)
                .await
                .map_err(|e| {
                    Status::new(
                        e.code(),
                        format!("container {}: {}", container.name, e.message()),
                    )
                })?;
        }
        Ok(())
    }

    /// The bake point (ADR 0025, `bake`), before the pod's first container:
    /// the guest pulls the image, its filesystem is frozen, diskd seals the
    /// disk's top as a layer, the filesystem is thawed, and the layer goes
    /// to the object store in the background, after which the pod's record
    /// names it. A bake that does not happen leaves the bring-up as it would
    /// have been: its container pulls; only a filesystem that stays frozen
    /// fails it.
    async fn bake_point(
        &self,
        pod_id: &str,
        spec: &PodSpec,
        channel: Channel,
        pull_token: Option<&str>,
    ) -> Result<(), Status> {
        let Some(mut layer) = bake::due(&self.info(pod_id)?) else {
            return Ok(());
        };
        let Some(container) = spec.containers.first() else {
            return Ok(());
        };
        // A container there already: the disk holds what the pod did.
        let request = GetContainerRequest {
            container_id: container.name.clone(),
        };
        match ContainerServiceClient::new(channel.clone())
            .get_container(request)
            .await
        {
            Err(e) if e.code() == Code::NotFound => {}
            Ok(_) => return Ok(()),
            Err(e) => {
                warn!("Pods: {pod_id} does not bake its image: {}", e.message());
                return Ok(());
            }
        }
        if let Err(e) = self.inner.disks.require_object_store(pod_id).await {
            info!("Pods: {pod_id} does not bake its image: {}", e.message());
            return Ok(());
        }
        let image = layer.image_ref.clone();
        match timeout(
            BAKE_PULL_TIMEOUT,
            pull_in_guest(channel.clone(), &image, pull_token),
        )
        .await
        {
            Ok(Ok(())) => {}
            Ok(Err(e)) => {
                warn!("Pods: {pod_id} does not bake {image}: {}", e.message());
                return Ok(());
            }
            Err(_) => {
                warn!("Pods: {pod_id} does not bake {image}: the pull took longer than {BAKE_PULL_TIMEOUT:?}");
                return Ok(());
            }
        }
        let mut host = HostServiceClient::new(channel);
        // A freeze that a restart of feosd cut off is thawed first; a guest
        // that cannot freeze does not bake.
        let thawed = timeout(
            FREEZE_TIMEOUT,
            host.thaw_filesystem(ThawFilesystemRequest::default()),
        )
        .await;
        if let Ok(Err(e)) = thawed
            && e.code() == Code::Unimplemented
        {
            info!("Pods: {pod_id} does not bake {image}: its guest cannot freeze its filesystem");
            return Ok(());
        }
        // Only a freeze that took, or did not answer, is thawed again.
        match timeout(
            FREEZE_TIMEOUT,
            host.freeze_filesystem(FreezeFilesystemRequest::default()),
        )
        .await
        {
            Ok(Ok(_)) => {}
            Ok(Err(e)) => {
                warn!(
                    "Pods: {pod_id} does not bake {image}: the freeze failed: {}",
                    e.message()
                );
                return Ok(());
            }
            Err(_) => {
                thaw(&mut host, pod_id).await?;
                warn!("Pods: {pod_id} does not bake {image}: the freeze did not answer");
                return Ok(());
            }
        }
        let sealed = match timeout(BAKE_SEAL_TIMEOUT, self.inner.disks.bake(pod_id)).await {
            Ok(sealed) => sealed,
            Err(_) => Err(Status::deadline_exceeded(format!(
                "the seal took longer than {BAKE_SEAL_TIMEOUT:?}"
            ))),
        };
        thaw(&mut host, pod_id).await?;
        let sealed = match sealed {
            Ok(sealed) => sealed,
            Err(e) => {
                warn!("Pods: {pod_id} does not bake {image}: {}", e.message());
                return Ok(());
            }
        };
        info!(
            "Pods: {pod_id} baked {image} into layer {} ({} bytes new, {:.1}s); uploading it",
            sealed.layer_id, sealed.bytes_new, sealed.seal_seconds
        );
        layer.layer_id = sealed.layer_id;
        let pods = self.clone();
        let id = pod_id.to_string();
        tokio::spawn(async move { pods.push_bake(&id, layer).await });
        Ok(())
    }

    /// Uploads the layer the pod baked, and names it in the pod's record
    /// once the object store has it, which the pod's event tells the
    /// control plane.
    async fn push_bake(&self, pod_id: &str, layer: BakedLayer) {
        for attempt in 1..=BAKE_PUSHES {
            match self.inner.disks.push(&layer.layer_id).await {
                Ok(()) => {
                    info!(
                        "Pods: {pod_id}'s layer {} of {} is in the object store",
                        layer.layer_id, layer.image_ref
                    );
                    self.update(pod_id, |info| info.baked = Some(layer)).await;
                    if let Ok(info) = self.info(pod_id) {
                        self.announce(&info, false);
                    }
                    return;
                }
                Err(e) if attempt < BAKE_PUSHES => {
                    warn!(
                        "Pods: {pod_id}'s layer {} does not upload: {}; again",
                        layer.layer_id,
                        e.message()
                    );
                    sleep(Duration::from_secs(30 * u64::from(attempt))).await;
                }
                Err(e) => warn!(
                    "Pods: {pod_id}'s layer {} does not upload: {}; not baked",
                    layer.layer_id,
                    e.message()
                ),
            }
        }
    }

    /// Gets the pod's microVM running on its disk, from whatever state it
    /// is in. Returns whether it booted it anew.
    async fn boot(&self, pod_id: &str, spec: &PodSpec) -> Result<bool, Status> {
        let vm_id = pod_id.to_string();
        self.place(pod_id).await?;
        let attachment = self.attach(pod_id).await?;
        let disk = self.open_disk(pod_id).await?;
        let volumes = self.open_volumes(pod_id).await?;
        let config = spec::vm_config(pod_id, spec, attachment.as_ref(), Some(&disk), &volumes);
        loop {
            let vm = self.vm(pod_id).await?;
            match vm.as_ref().map(|vm| vm.state()) {
                None => {
                    let request = Request::new(CreateVmRequest {
                        config: Some(config.clone()),
                        vm_id: Some(vm_id.clone()),
                    });
                    self.inner.vms.create_vm(request).await?;
                }
                Some(VmState::Creating) => sleep(POLL).await,
                // One made for another network or disk is made anew, such
                // as one of the first version's with IPv4, or one from
                // before pods had disks or a metadata service.
                Some(VmState::Created | VmState::Stopped)
                    if vm.as_ref().is_some_and(|vm| stale(vm, &config)) =>
                {
                    info!("Pods: {pod_id}'s microVM was made for another network, metadata or disk: making it anew");
                    self.delete_vm(pod_id).await?;
                }
                Some(VmState::Created | VmState::Stopped) => {
                    let request = Request::new(StartVmRequest { vm_id });
                    self.inner.vms.start_vm(request).await?;
                    return Ok(true);
                }
                Some(VmState::Running) => return Ok(false),
                Some(VmState::Paused) => {
                    let request = Request::new(ResumeVmRequest {
                        vm_id,
                        remote: None,
                    });
                    self.inner.vms.resume_vm(request).await?;
                    return Ok(false);
                }
                // A crashed microVM does not start again: it is made anew.
                Some(VmState::Crashed) => self.delete_vm(pod_id).await?,
                Some(state @ (VmState::Suspended | VmState::Unspecified)) => {
                    return Err(Status::failed_precondition(format!(
                        "the microVM is {}",
                        state.as_str_name()
                    )));
                }
            }
        }
    }

    async fn wait_for_guest(&self, pod_id: &str) -> Result<(), Status> {
        let deadline = Instant::now() + GUEST_TIMEOUT;
        // The VM service records the start of a microVM after StartVm
        // returns: until it runs, a stopped one has not started yet.
        let mut started = false;
        loop {
            let vm = self
                .vm(pod_id)
                .await?
                .ok_or_else(|| Status::aborted("the microVM is gone"))?;
            if let Some(guest) = vm.guest.as_ref().filter(|guest| guest.attached) {
                if !guest.push_error.is_empty() {
                    warn!(
                        "Pods: {pod_id}'s guest took no configuration: {}",
                        guest.push_error
                    );
                }
                return Ok(());
            }
            started |= vm.state() == VmState::Running;
            if started && matches!(vm.state(), VmState::Stopped | VmState::Crashed) {
                return Err(Status::aborted(format!(
                    "the microVM ended while it booted: {}",
                    vm.state().as_str_name()
                )));
            }
            if Instant::now() >= deadline {
                return Err(Status::deadline_exceeded(format!(
                    "the guest did not attach within {}s; the microVM is {}",
                    GUEST_TIMEOUT.as_secs(),
                    vm.state().as_str_name()
                )));
            }
            sleep(POLL).await;
        }
    }

    /// Puts the pod on the network, if it has one: its TAP device, its
    /// routes, proxy ARP for its gateway and the resolver, before every
    /// boot and resume. Fails while the node does not have the pod's
    /// security groups or name tables.
    async fn attach(&self, pod_id: &str) -> Result<Option<Attachment>, Status> {
        self.inner.network.attach(&self.info(pod_id)?).await
    }

    /// Writes the pods' rules for the pods there are.
    async fn apply_network(&self) -> io::Result<()> {
        let pods = self.list();
        self.inner.network.apply(&pods).await
    }

    /// Writes the pods' rules again whenever the security groups or the
    /// mesh's peers change, and every so often.
    fn follow_security_groups(&self) {
        let pods = self.clone();
        let mut changes = self.inner.network.groups().subscribe();
        let mut peers = self.inner.network.subscribe_mesh();
        tokio::spawn(async move {
            loop {
                tokio::select! {
                    changed = changes.changed() => {
                        if changed.is_err() {
                            return;
                        }
                    }
                    changed = peers.changed() => {
                        if changed.is_err() {
                            return;
                        }
                    }
                    _ = sleep(REFRESH) => {}
                }
                if let Err(e) = pods.apply_network().await {
                    warn!("Pods: Failed to set the pods' rules: {e}");
                }
            }
        });
    }

    /// Puts the pods on the network once it comes up after feosd's start,
    /// as on a mesh whose /64 the control plane gives the node later
    /// (ADR 0017): as the start does for a network that is there, pods
    /// get the addresses they lack, the resolver knows them and answers,
    /// the rules are written, the records start, and running pods get
    /// their TAP devices.
    fn follow_network(&self) {
        let mut ready = self.inner.network.ready();
        if *ready.borrow() {
            return;
        }
        let pods = self.clone();
        tokio::spawn(async move {
            if ready.wait_for(|ready| *ready).await.is_err() {
                return;
            }
            pods.network_came_up().await;
        });
    }

    /// Gives the pod the addresses of `info` and keeps it, unless it is
    /// gone; the save is asked for under the pods' lock.
    fn take_addresses(&self, info: &PodInfo) -> Option<feos_utils::writer::Done<()>> {
        let mut pods = self.pods();
        let pod = pods.get_mut(&info.pod_id)?;
        pod.info.ipv6_address = info.ipv6_address.clone();
        pod.info.ipv4_address = info.ipv4_address.clone();
        Some(self.inner.store.save(&pod.info))
    }

    async fn network_came_up(&self) {
        let network = &self.inner.network;
        {
            // The addresses are handed out and kept on the blocking pool,
            // off the pods' lock; the pods take them then.
            let _records = self.inner.records.lock().await;
            // A hibernated pod has no addresses on the node.
            let kept: Vec<PodInfo> = self
                .pods()
                .values()
                .map(|pod| pod.info.clone())
                .filter(|info| info.state() != PodState::Hibernated)
                .collect();
            let pods = self.clone();
            let adopted = tokio::task::spawn_blocking(move || {
                let mut kept = kept;
                let changed = pods.inner.network.adopt(&mut kept);
                kept.retain(|info| changed.contains(&info.pod_id));
                kept
            })
            .await
            .unwrap_or_default();
            for info in adopted {
                if let Some(saved) = self.take_addresses(&info)
                    && let Err(e) = saved.await
                {
                    warn!("Pods: Failed to keep {}: {e}", info.pod_id);
                }
            }
        }
        network.sync(&self.list());
        network.serve_dns();
        match self.apply_network().await {
            Ok(()) => network.start_records(),
            Err(e) => network.fail(format!("the pods' rules are not written: {e}")),
        }
        let running: Vec<String> = self
            .list()
            .into_iter()
            .filter(|info| info.state() == PodState::Running)
            .map(|info| info.pod_id)
            .collect();
        for pod_id in running {
            if let Err(e) = self.reattach(&pod_id).await {
                warn!("Pods: {pod_id} runs without its network: {}", e.message());
            }
        }
        info!("Pods: The pods' network is up");
    }

    /// Waits until the VM service records the microVM in `state`: it
    /// records a change after the call that made it returned.
    async fn settle(&self, pod_id: &str, state: VmState) -> Result<(), Status> {
        let deadline = Instant::now() + SETTLE_TIMEOUT;
        loop {
            let actual = self.vm(pod_id).await?.map(|vm| vm.state());
            if actual == Some(state) {
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err(Status::deadline_exceeded(format!(
                    "the microVM is {}, not {}",
                    actual.map_or("gone", |actual| actual.as_str_name()),
                    state.as_str_name()
                )));
            }
            sleep(POLL).await;
        }
    }

    async fn vm(&self, pod_id: &str) -> Result<Option<VmInfo>, Status> {
        let request = Request::new(GetVmRequest {
            vm_id: pod_id.to_string(),
        });
        match self.inner.vms.get_vm(request).await {
            Ok(vm) => Ok(Some(vm.into_inner())),
            Err(e) if e.code() == Code::NotFound => Ok(None),
            Err(e) => Err(e),
        }
    }

    /// The running microVM, as far as it answers.
    async fn microvm(&self, pod_id: &str) -> Option<MicroVm> {
        let vm = self.vm(pod_id).await.ok()??;
        let request = Request::new(PingVmRequest {
            vm_id: pod_id.to_string(),
        });
        let pid = match self.inner.vms.ping_vm(request).await {
            Ok(ping) => ping.into_inner().pid,
            Err(_) => 0,
        };
        let guest = vm.guest.unwrap_or_default();
        Some(MicroVm {
            pid,
            guest_version: guest.feos_version,
            guest_attached: guest.attached,
        })
    }

    async fn delete_vm(&self, pod_id: &str) -> Result<(), Status> {
        let request = Request::new(DeleteVmRequest {
            vm_id: pod_id.to_string(),
        });
        match self.inner.vms.delete_vm(request).await {
            Err(e) if e.code() != Code::NotFound => Err(e),
            _ => Ok(()),
        }
    }

    async fn container_status(&self, info: &PodInfo) -> Vec<ContainerStatus> {
        let list = timeout(STATUS_TIMEOUT, async {
            let mut guest =
                ContainerServiceClient::new(self.guest_channel(&info.pod_id).await.ok()?);
            guest
                .list_containers(ListContainersRequest {})
                .await
                .ok()
                .map(|list| list.into_inner().containers)
        })
        .await;
        let Ok(Some(containers)) = list else {
            return Vec::new();
        };
        let spec = info.spec.clone().unwrap_or_default();
        spec.containers
            .iter()
            .filter_map(|wanted| {
                let container = containers.iter().find(|c| c.container_id == wanted.name)?;
                Some(ContainerStatus {
                    name: wanted.name.clone(),
                    state: match container.state() {
                        GuestContainerState::PullingImage => ContainerState::Creating,
                        GuestContainerState::Created => ContainerState::Created,
                        GuestContainerState::Running => ContainerState::Running,
                        GuestContainerState::Stopped => ContainerState::Exited,
                        GuestContainerState::Unspecified => ContainerState::Unspecified,
                    } as i32,
                    exit_code: container.exit_code,
                })
            })
            .collect()
    }

    /// Follows the microVMs' states: a running pod whose microVM crashed
    /// failed, and one whose microVM stopped is stopped. While an operation
    /// or a bring-up holds the pod, it sees to that itself. A running pod
    /// whose microVM was suspended is suspended once what holds it is done
    /// (`follow_vm`): the suspend that suspended the microVM failed after
    /// it, or it would have seen to it.
    fn follow_vms(&self) {
        let pods = self.clone();
        tokio::spawn(async move {
            loop {
                let request = Request::new(StreamVmEventsRequest::default());
                match pods.inner.vms.stream_vm_events(request).await {
                    Ok(stream) => {
                        let mut stream = stream.into_inner();
                        while let Some(Ok(event)) = stream.next().await {
                            pods.on_vm_event(event).await;
                        }
                    }
                    Err(e) => warn!("Pods: No events of the microVMs: {}", e.message()),
                }
                sleep(Duration::from_secs(1)).await;
            }
        });
    }

    async fn on_vm_event(&self, event: VmEvent) {
        let Some(data) = event.data else {
            return;
        };
        if !data
            .type_url
            .ends_with("feos.microvm.v1.VmStateChangedEvent")
        {
            return;
        }
        let Ok(changed) = VmStateChangedEvent::decode(data.value.as_slice()) else {
            return;
        };
        let next = {
            let pods = self.pods();
            let Some(pod) = pods.get(&event.vm_id) else {
                return;
            };
            if changed.new_state() == VmState::Suspended && pod.info.state() == PodState::Running {
                let pods = self.clone();
                let pod_id = event.vm_id.clone();
                tokio::spawn(async move { pods.follow_vm(&pod_id).await });
                return;
            }
            if pod.lock.try_lock().is_err() || pod.info.state() != PodState::Running {
                return;
            }
            match changed.new_state() {
                VmState::Crashed => Some((
                    PodState::Failed,
                    format!("the microVM crashed: {}", changed.reason),
                )),
                VmState::Stopped => Some((PodState::Stopped, "the microVM stopped".to_string())),
                _ => None,
            }
        };
        if let Some((state, reason)) = next {
            self.set_state(&event.vm_id, state, &reason).await;
            // The microVM is gone; its disk keeps what the pod wrote, for
            // the next start.
            let pods = self.clone();
            let pod_id = event.vm_id;
            tokio::spawn(async move {
                if let Ok(_lock) = pods.lock(&pod_id).await
                    && pods.info(&pod_id).is_ok_and(|info| {
                        matches!(info.state(), PodState::Failed | PodState::Stopped)
                    })
                {
                    pods.release_disk(&pod_id).await;
                }
            });
        }
    }
}

impl Pods {
    /// Brings the pod in line with its microVM's record, under the pod's
    /// lock, once what holds it is done: a running pod whose microVM was
    /// suspended is suspended, and its disk stopped, as a suspend that ends
    /// leaves it. That is a SuspendPod whose microVM's suspend went through
    /// and that failed after it, as when the microVM service recorded the
    /// suspend later than `SETTLE_TIMEOUT`, or that a feosd before this one
    /// did not see to its end. Returns whether the pod changed.
    async fn follow_vm(&self, pod_id: &str) -> bool {
        let Ok(_lock) = self.lock(pod_id).await else {
            return false;
        };
        let Ok(info) = self.info(pod_id) else {
            return false;
        };
        let vm = match self.vm(pod_id).await {
            Ok(vm) => vm.map(|vm| vm.state()),
            Err(e) => {
                warn!("Pods: No record of {pod_id}'s microVM: {}", e.message());
                return false;
            }
        };
        let Some((state, reason)) = follows(info.state(), vm) else {
            return false;
        };
        self.release_disk(pod_id).await;
        self.set_state(pod_id, state, reason).await;
        true
    }
}

/// Whether a stop whose shutdown of the microVM failed with `error` ends
/// the microVM: not when the microVM is busy (`vmm::is_busy`), which the
/// stop answers instead.
fn ends_after_failed_shutdown(error: &Status) -> bool {
    !microvm_service::vmm::is_busy(error)
}

/// What a pod in `pod` becomes whose microVM's record is `vm`, when no call
/// of the pod's saw to it (`Pods::follow_vm`): a running pod whose microVM
/// is suspended is suspended. A microVM that runs again after a suspend
/// that failed, its late snapshot discarded, finds its pod running, as the
/// suspend left it; nothing else changes here.
fn follows(pod: PodState, vm: Option<VmState>) -> Option<(PodState, &'static str)> {
    match (pod, vm) {
        (PodState::Running, Some(VmState::Suspended)) => Some((
            PodState::Suspended,
            "suspended: the microVM was suspended after its suspend failed",
        )),
        _ => None,
    }
}

/// A hibernation's task, as its steps see it.
struct Run<'a> {
    pod_id: &'a str,
    /// Its slot's token.
    token: Arc<()>,
    /// Whether a resume asked for its abandon.
    abandon: watch::Receiver<bool>,
    /// Whether the abandon stops it: once the pod's memory goes with it,
    /// for good. Without, a resume waits for the hibernation and restores
    /// the pod from its checkpoint.
    obeys: bool,
    phase: HibernationPhase,
}

impl Run<'_> {
    /// Whether a resume asked for an abandon that stops the hibernation.
    fn asked(&self) -> bool {
        self.obeys && *self.abandon.borrow()
    }

    /// An abandon point between two steps.
    fn check(&self) -> Result<(), Status> {
        if self.asked() {
            return Err(abandoned(self.pod_id));
        }
        Ok(())
    }

    /// A step that an abandon drops: a call to diskd that seals or uploads.
    async fn abandonable<T>(
        &mut self,
        step: impl Future<Output = Result<T, Status>>,
    ) -> Result<T, Status> {
        if !self.obeys {
            return step.await;
        }
        abandonable(&mut self.abandon, self.pod_id, step).await
    }
}

/// What a resume does about a hibernation in flight.
#[derive(Debug, PartialEq)]
enum Meets {
    /// It asked the hibernation to stop, in this phase.
    Abandons(HibernationPhase),
    /// It waits for the hibernation, in this phase, for this reason.
    Waits(HibernationPhase, &'static str),
}

/// A resume asks the hibernation to stop, if it may.
fn request_abandon(hibernation: &Hibernation) -> Meets {
    if !hibernation.abandonable() {
        let why = if hibernation.no_abandon {
            "it may not be abandoned"
        } else if hibernation.memory == Some(false) {
            "its pod has no memory to resume"
        } else {
            "its head is being put"
        };
        return Meets::Waits(hibernation.phase, why);
    }
    hibernation.abandon.send_replace(true);
    Meets::Abandons(hibernation.phase)
}

/// The hibernation passes its point of no return, unless a resume asked
/// for its abandon first. It runs under the pods' mutex, as
/// `request_abandon` does, so that one of the two comes first: a resume
/// that comes later waits for the hibernation.
fn enter_head(hibernation: &mut Hibernation, pod_id: &str) -> Result<(), Status> {
    if hibernation.memory == Some(true) && *hibernation.abandon.borrow() {
        return Err(abandoned(pod_id));
    }
    hibernation.phase = HibernationPhase::Head;
    Ok(())
}

/// What a HibernatePod does about the pod's hibernation in flight `slot`:
/// it joins it, and gets its answer, or starts one (None). One with the
/// request id of an abandoned hibernation fails with ABORTED, and starts
/// nothing.
fn join(
    slot: &mut Option<Hibernation>,
    abandoned_ids: &VecDeque<String>,
    pod_id: &str,
    request_id: &str,
    no_abandon: bool,
) -> Result<Option<Shared<BoxFuture<'static, Answer>>>, Status> {
    if !request_id.is_empty() && abandoned_ids.iter().any(|id| id == request_id) {
        return Err(abandoned(pod_id));
    }
    Ok(slot.as_mut().map(|hibernation| {
        hibernation.no_abandon |= no_abandon;
        hibernation.add_request(request_id);
        hibernation.answer.clone()
    }))
}

/// Ends the slot of the hibernation `token`, if the slot is still its; the
/// request ids of an abandoned one join the pod's abandoned ones.
fn end_slot(
    slot: &mut Option<Hibernation>,
    abandoned_ids: &mut VecDeque<String>,
    token: &Arc<()>,
    abandoned: bool,
) {
    if !slot
        .as_ref()
        .is_some_and(|hibernation| Arc::ptr_eq(&hibernation.token, token))
    {
        return;
    }
    let Some(hibernation) = slot.take() else {
        return;
    };
    if abandoned {
        abandoned_ids.extend(hibernation.request_ids);
        while abandoned_ids.len() > REQUEST_IDS {
            abandoned_ids.pop_front();
        }
    }
}

/// Ends a hibernation's slot, and then lets go of the pod's lock, on
/// every way out of its task, a panic too: a HibernatePod that comes later
/// starts a hibernation of its own.
struct SlotGuard {
    /// Ends the slot; for an abandoned hibernation with true.
    end: Option<Box<dyn FnOnce(bool) + Send>>,
    abandoned: bool,
    /// The pod's lock, which drops after the slot ended.
    lock: Option<OwnedMutexGuard<()>>,
}

impl Drop for SlotGuard {
    fn drop(&mut self) {
        if let Some(end) = self.end.take() {
            end(self.abandoned);
        }
    }
}

/// A hibernation's answer, from its task: one that panicked ended with an
/// internal error.
fn joined(pod_id: &str, joined: Result<Answer, JoinError>) -> Answer {
    joined.unwrap_or_else(|e| {
        Err(Status::internal(format!(
            "the hibernation of pod {pod_id} ended: {e}"
        )))
    })
}

/// The answer of a hibernation that a resume abandoned.
fn abandoned(pod_id: &str) -> Status {
    Status::aborted(format!(
        "pod {pod_id} is resumed: its hibernation was abandoned"
    ))
}

/// Runs a step of a hibernation until a resume asks for the abandon: the
/// step is dropped then, and with it its call to diskd, whose handler
/// stops: an upload where it is, a seal on the node before it commits its
/// layer.
async fn abandonable<T>(
    abandon: &mut watch::Receiver<bool>,
    pod_id: &str,
    step: impl Future<Output = Result<T, Status>>,
) -> Result<T, Status> {
    tokio::select! {
        biased;
        () = asked(abandon) => Err(abandoned(pod_id)),
        result = step => result,
    }
}

/// Returns once a resume asked for the abandon; never, once none can.
async fn asked(abandon: &mut watch::Receiver<bool>) {
    if abandon.wait_for(|asked| *asked).await.is_err() {
        std::future::pending::<()>().await;
    }
}

/// A volume of a hibernating pod's: checkpointed already, with its head,
/// or sealed and uploaded, its head to come.
enum VolumePart {
    Kept(String),
    Sealed { name: String, volume: Volume },
}

/// Puts `bytes` into the object store through the file `path`, which `put`
/// uploads. The file goes when `put` ends, and when the future is dropped,
/// as an abandoned hibernation drops it.
async fn put_through_file<'a, F>(
    path: &'a Path,
    bytes: &[u8],
    put: impl FnOnce(&'a Path) -> F,
) -> Result<(), Status>
where
    F: Future<Output = Result<(), Status>>,
{
    let _file = Removed(path);
    std::fs::write(path, bytes)
        .map_err(|e| Status::internal(format!("write {}: {e}", path.display())))?;
    put(path).await
}

/// Removes its file when it drops.
struct Removed<'a>(&'a Path);

impl Drop for Removed<'_> {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(self.0);
    }
}

/// What a restore of a pod's memory left.
enum Restore {
    /// The guest did not run: the disk is as the checkpoint has it.
    NotBooted(Status),
    /// The guest ran, and something after it failed.
    Booted(Status),
}

impl From<Status> for Restore {
    fn from(e: Status) -> Self {
        Restore::NotBooted(e)
    }
}

/// Why a restore boots the pod anew on its checkpoint's disk instead of
/// restoring its memory, if it does: it was asked to, the checkpoint
/// holds no memory, or this node cannot load the snapshot. That is
/// decided before anything starts, from what the snapshot pins; a load
/// that fails all the same falls back, too, as long as the guest did not
/// run.
fn why_cold(cold: bool, memory: Option<&MemoryRef>) -> Option<String> {
    match memory {
        _ if cold => Some("asked to boot anew".to_string()),
        None => Some("its checkpoint holds no memory".to_string()),
        Some(memory) => snapshot::Host {
            firecracker_version: memory.firecracker_version.clone(),
            cpu_model: memory.cpu_model.clone(),
            kernel_release: memory.kernel_release.clone(),
        }
        .unrestorable_here(),
    }
}

/// The object of a checkpoint that holds the pod's spec, for a node that
/// does not have the pod.
const SPEC_OBJECT: &str = "pod.pb";
/// The object of a volume's checkpoint that holds its record, a
/// `feos.volume.v1.Volume`: its size, labels and whether it has its
/// filesystem.
const VOLUME_OBJECT: &str = "volume.pb";

/// The key of an object of the pod's checkpoint `epoch`, beside its head.
fn checkpoint_key(pod_id: &str, epoch: u64, name: &str) -> String {
    format!("snapshots/{pod_id}/{epoch}/{name}")
}

/// The epoch of the pod's next checkpoint: the seconds since 1970, or one
/// more than the last one's of the pod and of its volumes, so that it grows
/// and no head of the pod or of a volume is written twice.
fn next_epoch(last: impl IntoIterator<Item = u64>) -> u64 {
    let now = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .map_or(0, |since| since.as_secs());
    now.max(last.into_iter().map(|epoch| epoch + 1).max().unwrap_or(0))
}

/// Whether the microVM's network, its metadata or its disks are not the
/// ones `wanted` has.
fn stale(vm: &VmInfo, wanted: &VmConfig) -> bool {
    let config = vm.config.as_ref();
    config.map(|config| {
        (
            &config.net,
            &config.guest_network,
            &config.metadata_json,
            &config.data_disk,
            &config.volumes,
        )
    }) != Some((
        &wanted.net,
        &wanted.guest_network,
        &wanted.metadata_json,
        &wanted.data_disk,
        &wanted.volumes,
    ))
}

/// A create of a pod that exists finds it if the spec is the same.
fn same_spec(existing: PodInfo, spec: &PodSpec) -> Result<PodInfo, Status> {
    if existing.spec.as_ref() == Some(spec) {
        Ok(existing)
    } else {
        Err(Status::already_exists(format!(
            "pod {} exists with another spec",
            existing.pod_id
        )))
    }
}

/// A channel to the API of the guest of `pod_id` behind `uds`, whose
/// connect takes `deadline` at most: past it, `UNAVAILABLE`, with what
/// `busy` says.
async fn dial_guest(
    uds: PathBuf,
    pod_id: &str,
    deadline: Duration,
    busy: impl Future<Output = Option<String>>,
) -> Result<Channel, Status> {
    match vsock::channel_within(uds, API_PORT, deadline).await {
        Ok(channel) => Ok(channel),
        Err(e) if vsock::timed_out(&e) => {
            let mut message = format!(
                "the guest of pod {pod_id} did not answer within {}",
                vsock::seconds(deadline)
            );
            match busy.await {
                // Marked busy: the caller tries again later, and takes
                // the guest for neither gone nor broken.
                Some(busy) => {
                    message.push_str(&format!("; the VM is busy: {busy}"));
                    Err(microvm_service::vmm::marked_busy(message))
                }
                None => Err(Status::unavailable(message)),
            }
        }
        Err(e) => Err(Status::unavailable(format!(
            "the guest of pod {pod_id}: {e}"
        ))),
    }
}

/// Gets a container running in the guest, from whatever is there. After a
/// new boot, a container that the pod's disk has from the boot before is
/// stopped, and is adopted with what it wrote; one that is not there is
/// created.
/// How long a bake waits for the guest's pull, for its freeze and thaw,
/// and for diskd's seal; how often it tries to upload the layer.
const BAKE_PULL_TIMEOUT: Duration = Duration::from_secs(15 * 60);
const FREEZE_TIMEOUT: Duration = Duration::from_secs(30);
const BAKE_SEAL_TIMEOUT: Duration = Duration::from_secs(5 * 60);
const BAKE_PUSHES: u32 = 3;

/// Has the guest pull `image_ref` into its image store, or join the pull
/// or the image there, and waits until it is ready.
async fn pull_in_guest(
    channel: Channel,
    image_ref: &str,
    pull_token: Option<&str>,
) -> Result<(), Status> {
    let mut images = ImageServiceClient::new(channel);
    let request = PullImageRequest {
        image_ref: image_ref.to_string(),
        exclusive: false,
        pull_token: pull_token.unwrap_or_default().to_string(),
    };
    let image_uuid = images.pull_image(request).await?.into_inner().image_uuid;
    let request = WatchImageStatusRequest { image_uuid };
    let mut statuses = images.watch_image_status(request).await?.into_inner();
    while let Some(status) = statuses.message().await? {
        match ImageState::try_from(status.state) {
            Ok(ImageState::Ready) => return Ok(()),
            Ok(ImageState::PullFailed | ImageState::NotFound) => {
                return Err(Status::unavailable(format!(
                    "the pull failed: {}",
                    status.message
                )));
            }
            _ => {}
        }
    }
    Err(Status::unavailable(
        "the image's status ended before it was ready",
    ))
}

/// Thaws the guest's data volume: a filesystem that was not frozen is
/// fine; one that stays frozen fails, since nothing could write there.
async fn thaw(host: &mut HostServiceClient<Channel>, pod_id: &str) -> Result<(), Status> {
    let mut last = String::new();
    for attempt in 0..3 {
        if attempt > 0 {
            sleep(Duration::from_secs(1)).await;
        }
        match timeout(
            FREEZE_TIMEOUT,
            host.thaw_filesystem(ThawFilesystemRequest::default()),
        )
        .await
        {
            Ok(Ok(_)) => return Ok(()),
            Ok(Err(e)) if bake::not_frozen(e.message()) => return Ok(()),
            Ok(Err(e)) => last = e.message().to_string(),
            Err(_) => last = "the thaw did not answer".to_string(),
        }
    }
    Err(Status::internal(format!(
        "the guest of pod {pod_id} keeps its filesystem frozen after a bake: {last}"
    )))
}

async fn run_container(
    guest: &mut ContainerServiceClient<Channel>,
    container: &ContainerSpec,
    booted: bool,
    pull_token: Option<&str>,
) -> Result<(), Status> {
    let container_id = container.name.clone();
    let create = CreateContainerRequest {
        config: Some(ContainerConfig {
            image_ref: container.image_ref.clone(),
            command: container.command.clone(),
            env: container.env.clone(),
            volume_mounts: container
                .volume_mounts
                .iter()
                .map(|mount| GuestVolumeMount {
                    volume: mount.volume.clone(),
                    path: mount.path.clone(),
                    read_only: mount.read_only,
                })
                .collect(),
        }),
        container_id: Some(container_id.clone()),
        // Without a container to adopt, a plain create.
        adopt: true,
        pull_token: pull_token.unwrap_or_default().to_string(),
    };
    // Adopted once at most: one that stops again exited in this boot.
    let mut adopt = booted;
    loop {
        let request = GetContainerRequest {
            container_id: container_id.clone(),
        };
        match guest.get_container(request).await {
            Err(e) if e.code() == Code::NotFound => {
                guest.create_container(create.clone()).await?;
            }
            Err(e) => return Err(e),
            Ok(info) => match info.into_inner().state() {
                // A create that a cut-off bring-up made is still pulling.
                GuestContainerState::PullingImage => sleep(POLL).await,
                GuestContainerState::Created => {
                    let request = StartContainerRequest { container_id };
                    guest.start_container(request).await?;
                    return Ok(());
                }
                GuestContainerState::Stopped if adopt => {
                    adopt = false;
                    guest.create_container(create.clone()).await?;
                }
                GuestContainerState::Running | GuestContainerState::Stopped => return Ok(()),
                GuestContainerState::Unspecified => {
                    return Err(Status::internal("the guest reports no state"));
                }
            },
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn memory(firecracker_version: &str) -> MemoryRef {
        let here = snapshot::Host::this(firecracker_version);
        MemoryRef {
            layer: "aa".to_string(),
            vmstate_key: "snapshots/p/1/vmstate".to_string(),
            firecracker_version: here.firecracker_version,
            cpu_model: here.cpu_model,
            kernel_release: here.kernel_release,
        }
    }

    #[test]
    fn a_checkpoint_boots_anew_where_its_memory_cannot_be_restored() {
        assert_eq!(
            why_cold(true, Some(&memory("1.17.0"))).unwrap(),
            "asked to boot anew"
        );
        assert!(why_cold(false, None).unwrap().contains("no memory"));
        // A Firecracker no node has.
        let reason = why_cold(false, Some(&memory("0.0.0-none"))).unwrap();
        assert!(reason.contains("not installed"), "{reason}");
        // What the snapshot does not record counts as different.
        let mut unknown = memory("0.0.0-none");
        unknown.cpu_model.clear();
        assert!(why_cold(false, Some(&unknown)).is_some());
    }

    /// A running pod follows its microVM into suspended, as a suspend that
    /// failed after the microVM's suspend went through leaves them; the
    /// control plane then finds it suspended. Nothing else follows.
    #[test]
    fn a_running_pod_follows_its_microvm_into_suspended() {
        let (state, reason) =
            follows(PodState::Running, Some(VmState::Suspended)).expect("it stays running");
        assert_eq!(state, PodState::Suspended);
        assert!(reason.starts_with("suspended"), "{reason}");
        for (pod, vm) in [
            (PodState::Running, Some(VmState::Running)),
            (PodState::Running, None),
            (PodState::Suspended, Some(VmState::Suspended)),
            (PodState::Suspended, Some(VmState::Running)),
            (PodState::Pending, Some(VmState::Suspended)),
            (PodState::Stopped, Some(VmState::Suspended)),
            (PodState::Hibernated, Some(VmState::Suspended)),
        ] {
            assert_eq!(follows(pod, vm), None, "{pod:?} and a microVM {vm:?}");
        }
    }

    /// A stop ends a microVM that does not shut down, but not a busy one:
    /// it answers busy, and the control plane stops the pod later.
    #[test]
    fn a_stop_leaves_a_busy_microvm_alone() {
        let busy = Status::from(microvm_service::vmm::VmmError::Busy(
            "Firecracker still writes the snapshot".into(),
        ));
        assert!(!ends_after_failed_shutdown(&busy), "{busy:?}");
        for other in [
            Status::unavailable("Could not connect to the API socket"),
            Status::internal("the guest did not go"),
            Status::deadline_exceeded("no answer"),
        ] {
            assert!(ends_after_failed_shutdown(&other), "{other:?}");
        }
    }

    #[test]
    fn epochs_grow_and_are_never_written_twice() {
        let now = next_epoch(None);
        assert!(now > 1_700_000_000);
        assert_eq!(next_epoch(Some(now + 100)), now + 101);
        // A volume's last checkpoint counts, too: it may come from a pod
        // that hibernated later.
        assert_eq!(next_epoch([now - 5, now + 200, now + 100]), now + 201);
        assert_eq!(checkpoint_key("p", 7, SPEC_OBJECT), "snapshots/p/7/pod.pb");
        assert_eq!(
            checkpoint_key(&volume_disk("v"), 7, VOLUME_OBJECT),
            "snapshots/volume-v/7/volume.pb"
        );
    }

    #[test]
    fn a_microvm_with_other_volumes_is_stale() {
        let disk = |path: &str| feos_proto::microvm::DataDiskConfig {
            path: path.to_string(),
        };
        let wanted = VmConfig {
            data_disk: Some(disk("/d")),
            volumes: vec![disk("/a")],
            ..Default::default()
        };
        let mut vm = VmInfo {
            config: Some(wanted.clone()),
            ..Default::default()
        };
        assert!(!stale(&vm, &wanted));
        // One made before the pod had volumes, or with others.
        vm.config.as_mut().unwrap().volumes.clear();
        assert!(stale(&vm, &wanted));
    }

    #[test]
    fn a_microvm_without_its_metadata_is_stale() {
        let wanted = VmConfig {
            metadata_json: r#"{"feos":{}}"#.to_string(),
            ..Default::default()
        };
        let mut vm = VmInfo {
            config: Some(wanted.clone()),
            ..Default::default()
        };
        assert!(!stale(&vm, &wanted));
        // One made before pods had a metadata service.
        vm.config.as_mut().unwrap().metadata_json.clear();
        assert!(stale(&vm, &wanted));
    }

    /// A hibernation in flight, started by a HibernatePod with
    /// `request_id`, and the receiver its task looks at.
    fn hibernation(no_abandon: bool, request_id: &str) -> (Hibernation, watch::Receiver<bool>) {
        let (abandon, asked) = watch::channel(false);
        let answer = futures::future::ready(Ok(HibernatePodResponse::default()))
            .boxed()
            .shared();
        let hibernation = Hibernation::new(Arc::new(()), answer, abandon, no_abandon, request_id);
        (hibernation, asked)
    }

    const PHASES: [HibernationPhase; 8] = [
        HibernationPhase::Waiting,
        HibernationPhase::Suspending,
        HibernationPhase::Sealing,
        HibernationPhase::UploadingDisk,
        HibernationPhase::Volumes,
        HibernationPhase::UploadingMemory,
        HibernationPhase::Head,
        HibernationPhase::Leaving,
    ];

    fn assert_abandoned(status: &Status) {
        assert_eq!(status.code(), Code::Aborted, "{status:?}");
        assert_eq!(
            status.message(),
            "pod p is resumed: its hibernation was abandoned"
        );
    }

    #[test]
    fn a_hibernation_is_abandonable_until_its_head() {
        let (mut h, _asked) = hibernation(false, "");
        for phase in PHASES {
            h.phase = phase;
            let before_head = phase < HibernationPhase::Head;
            for memory in [None, Some(true)] {
                h.memory = memory;
                assert_eq!(h.abandonable(), before_head, "{phase:?}, {memory:?}");
                assert_eq!(h.shown().abandonable, before_head);
            }
            // Without the memory, there is nothing to resume.
            h.memory = Some(false);
            assert!(!h.abandonable(), "{phase:?} without memory");
        }
        // A drain's never is.
        let (mut h, _asked) = hibernation(true, "");
        for phase in PHASES {
            h.phase = phase;
            for memory in [None, Some(true), Some(false)] {
                h.memory = memory;
                assert!(!h.abandonable(), "{phase:?}, {memory:?}");
            }
        }
    }

    #[test]
    fn a_resume_and_the_head_come_one_after_the_other() {
        // The resume first: the head is never put.
        let (mut h, asked) = hibernation(false, "r");
        h.phase = HibernationPhase::UploadingDisk;
        h.memory = Some(true);
        assert_eq!(
            request_abandon(&h),
            Meets::Abandons(HibernationPhase::UploadingDisk)
        );
        assert!(*asked.borrow());
        assert_abandoned(&enter_head(&mut h, "p").unwrap_err());
        assert_eq!(h.phase, HibernationPhase::UploadingDisk);

        // The head first: the resume waits.
        let (mut h, asked) = hibernation(false, "r");
        h.phase = HibernationPhase::UploadingMemory;
        h.memory = Some(true);
        enter_head(&mut h, "p").unwrap();
        assert_eq!(h.phase, HibernationPhase::Head);
        assert_eq!(
            request_abandon(&h),
            Meets::Waits(HibernationPhase::Head, "its head is being put")
        );
        assert!(!*asked.borrow());

        // A resume that asked before the pod turned out to have no memory
        // does not stop the hibernation.
        let (mut h, _asked) = hibernation(false, "r");
        assert_eq!(
            request_abandon(&h),
            Meets::Abandons(HibernationPhase::Waiting)
        );
        h.memory = Some(false);
        enter_head(&mut h, "p").unwrap();
        assert_eq!(
            request_abandon(&h),
            Meets::Waits(HibernationPhase::Head, "its pod has no memory to resume")
        );

        // A join with no_abandon makes it a drain's.
        let (mut h, asked) = hibernation(false, "r");
        h.memory = Some(true);
        let mut slot = Some(h);
        let joined = join(&mut slot, &VecDeque::new(), "p", "d", true).unwrap();
        assert!(joined.is_some(), "it joins");
        let h = slot.as_mut().unwrap();
        assert_eq!(
            request_abandon(h),
            Meets::Waits(HibernationPhase::Waiting, "it may not be abandoned")
        );
        assert!(!*asked.borrow());
        enter_head(h, "p").unwrap();
    }

    #[test]
    fn a_hibernatepod_joins_or_starts() {
        let mut abandoned_ids = VecDeque::from(["gone".to_string()]);
        let mut slot = None;
        // One of an abandoned hibernation starts nothing.
        assert_abandoned(&join(&mut slot, &abandoned_ids, "p", "gone", false).unwrap_err());
        assert!(slot.is_none());
        // Without a hibernation in flight, it starts one.
        assert!(join(&mut slot, &abandoned_ids, "p", "a", false)
            .unwrap()
            .is_none());

        let (h, _asked) = hibernation(false, "a");
        slot = Some(h);
        let answer = join(&mut slot, &abandoned_ids, "p", "b", false).unwrap();
        assert!(answer.is_some(), "it joins");
        // A call without an id, and one again, add nothing.
        join(&mut slot, &abandoned_ids, "p", "", false).unwrap();
        join(&mut slot, &abandoned_ids, "p", "b", false).unwrap();
        let h = slot.as_ref().unwrap();
        assert_eq!(h.request_ids, ["a", "b"]);
        assert!(!h.no_abandon);
        assert_eq!(h.shown().request_ids, ["a", "b"]);
        // One that may not be abandoned is sticky.
        join(&mut slot, &abandoned_ids, "p", "c", true).unwrap();
        join(&mut slot, &abandoned_ids, "p", "d", false).unwrap();
        assert!(slot.as_ref().unwrap().no_abandon);
        // The last 16 are kept.
        for i in 0..20 {
            join(&mut slot, &abandoned_ids, "p", &format!("n{i}"), false).unwrap();
        }
        let ids = &slot.as_ref().unwrap().request_ids;
        assert_eq!(ids.len(), REQUEST_IDS);
        assert_eq!(ids.first().map(String::as_str), Some("n4"));
        assert_eq!(ids.last().map(String::as_str), Some("n19"));

        // The slot ends only for its own token.
        let token = slot.as_ref().unwrap().token.clone();
        end_slot(&mut slot, &mut abandoned_ids, &Arc::new(()), true);
        assert!(slot.is_some(), "another hibernation's guard ended the slot");
        assert_eq!(abandoned_ids, ["gone"]);
        end_slot(&mut slot, &mut abandoned_ids, &token, true);
        assert!(slot.is_none());
        // An abandoned one's ids are remembered, the last 16.
        assert_eq!(abandoned_ids.len(), REQUEST_IDS);
        assert_eq!(abandoned_ids.front().map(String::as_str), Some("n4"));
        assert_abandoned(&join(&mut slot, &abandoned_ids, "p", "n19", false).unwrap_err());

        // One that was not abandoned leaves no ids.
        let (h, _asked) = hibernation(false, "x");
        let token = h.token.clone();
        slot = Some(h);
        end_slot(&mut slot, &mut abandoned_ids, &token, false);
        assert!(slot.is_none());
        assert!(!abandoned_ids.contains(&"x".to_string()));
    }

    /// Sets its flag when it drops.
    struct Dropped(Arc<std::sync::atomic::AtomicBool>);

    impl Drop for Dropped {
        fn drop(&mut self) {
            self.0.store(true, std::sync::atomic::Ordering::SeqCst);
        }
    }

    /// A guest whose microVM does not answer, as while Firecracker writes
    /// a snapshot: the call fails at its deadline, with why if known.
    #[tokio::test]
    async fn a_guest_that_does_not_answer_is_unavailable_with_why() {
        let dir = tempfile::tempdir().unwrap();
        let uds = dir.path().join("v.sock");
        let _vmm = tokio::net::UnixListener::bind(&uds).unwrap();
        let deadline = Duration::from_millis(200);
        let dial = |busy: Option<&'static str>| {
            let uds = uds.clone();
            async move {
                timeout(
                    Duration::from_secs(5),
                    dial_guest(uds, "p", deadline, async move { busy.map(str::to_string) }),
                )
                .await
                .expect("the call waited past its deadline")
                .unwrap_err()
            }
        };
        let busy = dial(Some("the pod is being suspended")).await;
        assert_eq!(busy.code(), Code::Unavailable);
        assert_eq!(
            busy.message(),
            "the guest of pod p did not answer within 0.2s; the VM is busy: the pod is being \
             suspended"
        );
        assert!(microvm_service::vmm::is_busy(&busy), "{busy:?}");
        let unknown = dial(None).await;
        assert_eq!(unknown.code(), Code::Unavailable);
        assert_eq!(
            unknown.message(),
            "the guest of pod p did not answer within 0.2s"
        );
        assert!(!microvm_service::vmm::is_busy(&unknown));
        // No VMM at all: refused at once, as before.
        let gone = dial_guest(dir.path().join("none"), "p", deadline, async { None })
            .await
            .unwrap_err();
        assert!(
            gone.message().starts_with("the guest of pod p: "),
            "{}",
            gone.message()
        );
    }

    #[tokio::test]
    async fn an_abandon_drops_the_step() {
        let flag = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let (abandon, mut asked) = watch::channel(false);
        let step = {
            let dropped = Dropped(flag.clone());
            async move {
                let _dropped = dropped;
                std::future::pending::<Result<u8, Status>>().await
            }
        };
        let task = tokio::spawn(async move { abandonable(&mut asked, "p", step).await });
        tokio::time::sleep(Duration::from_millis(20)).await;
        assert!(!task.is_finished(), "the step ended by itself");
        abandon.send_replace(true);
        let result = timeout(Duration::from_secs(5), task)
            .await
            .expect("the abandon did not stop the step")
            .unwrap();
        assert_abandoned(&result.unwrap_err());
        assert!(flag.load(std::sync::atomic::Ordering::SeqCst));

        // Without an abandon, the step's answer.
        let (_abandon, mut asked) = watch::channel(false);
        let answer = abandonable(&mut asked, "p", async { Ok::<_, Status>(7) }).await;
        assert_eq!(answer.unwrap(), 7);
        // With no one left to ask, too.
        let (abandon, mut asked) = watch::channel(false);
        drop(abandon);
        let answer = abandonable(&mut asked, "p", async {
            tokio::time::sleep(Duration::from_millis(10)).await;
            Ok::<_, Status>(8)
        })
        .await;
        assert_eq!(answer.unwrap(), 8);
    }

    #[tokio::test]
    async fn a_panicking_hibernation_ends_its_slot() {
        let (h, _asked) = hibernation(false, "a");
        let token = h.token.clone();
        let slots = Arc::new(Mutex::new((Some(h), VecDeque::<String>::new())));
        let lock = Arc::new(tokio::sync::Mutex::new(()));
        let task = tokio::spawn({
            let slots = slots.clone();
            let lock = lock.clone();
            async move {
                let mut slot = SlotGuard {
                    end: Some(Box::new(move |abandoned| {
                        let mut slots = slots.lock().unwrap();
                        let (slot, ids) = &mut *slots;
                        end_slot(slot, ids, &token, abandoned);
                    })),
                    abandoned: false,
                    lock: None,
                };
                slot.lock = Some(lock.lock_owned().await);
                if slot.lock.is_some() {
                    panic!("a hibernation's bug");
                }
                Ok::<_, Status>(HibernatePodResponse::default())
            }
        });
        let answer = joined("p", task.await);
        let status = answer.unwrap_err();
        assert_eq!(status.code(), Code::Internal);
        assert!(
            status
                .message()
                .starts_with("the hibernation of pod p ended"),
            "{status:?}"
        );
        assert!(slots.lock().unwrap().0.is_none(), "the slot stayed");
        assert!(lock.try_lock().is_ok(), "the pod's lock stayed held");
    }

    #[tokio::test]
    async fn a_record_put_through_a_file_leaves_no_file() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("p.spec");
        // Dropped while it puts, as an abandon drops it.
        let put = put_through_file(&path, b"spec", |file| {
            assert_eq!(std::fs::read(file).unwrap(), b"spec");
            std::future::pending()
        });
        assert!(timeout(Duration::from_millis(20), put).await.is_err());
        assert!(!path.exists(), "a dropped put left its file");
        // A put that fails.
        let failed = put_through_file(&path, b"spec", |_| async {
            Err(Status::unavailable("diskd"))
        })
        .await;
        assert_eq!(failed.unwrap_err().code(), Code::Unavailable);
        assert!(!path.exists());
        put_through_file(&path, b"spec", |_| async { Ok(()) })
            .await
            .unwrap();
        assert!(!path.exists());
    }

    /// Pods kept in `dir/pods`, without a network, diskd or microVM
    /// service: what calls them fails.
    fn bare_pods(dir: &Path) -> Pods {
        let (vms, _) = tokio::sync::mpsc::channel(1);
        let disks = Disks::new(dir.join("no-diskd.sock"));
        Pods {
            inner: Arc::new(Inner {
                vms: VmApiHandler::new(vms),
                store: Store::open(dir.join("pods")).unwrap(),
                dir: dir.join("pods"),
                network: Network::absent(dir),
                disks: disks.clone(),
                volumes: Volumes::open(dir.join("volumes"), disks).unwrap(),
                pods: Mutex::new(HashMap::new()),
                events: broadcast::channel(16).0,
                stopping: AtomicBool::new(false),
                stop_record: tokio::sync::Mutex::new(()),
                records: tokio::sync::Mutex::new(()),
            }),
        }
    }

    /// Holds the pods' writer on the record of another pod, which hangs
    /// as an fsync behind a snapshot's memory does: what is kept behind it
    /// waits, and lands once the FIFO is opened.
    fn hold_the_writer(pods: &Pods, dir: &Path) -> crate::hung::Hung {
        let hung = crate::hung::Hung::at(dir.join("pods/other.pb.new"));
        drop(pods.inner.store.save(&PodInfo {
            pod_id: "other".to_string(),
            ..Default::default()
        }));
        hung
    }

    const POD: &str = "11111111-2222-3333-4444-555555555555";

    /// Waits up to 5 s for `done`.
    async fn within_5s(mut done: impl FnMut() -> bool) -> bool {
        for _ in 0..50 {
            if done() {
                return true;
            }
            sleep(Duration::from_millis(100)).await;
        }
        done()
    }

    /// Whether the pod's bring-up was started.
    fn brought_up(pods: &Pods) -> bool {
        pods.pods()
            .get(POD)
            .is_some_and(|pod| pod.bring_up.is_some())
    }

    /// A StartPod whose caller goes away while the pod's record waits for
    /// its fsync, as when ferund resets the stream at its deadline, still
    /// brings the pod up: it is not left pending with nothing to bring it
    /// up.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_start_whose_caller_goes_away_brings_the_pod_up() {
        let dir = tempfile::tempdir().unwrap();
        let pods = bare_pods(dir.path());
        pods.pods().insert(
            POD.to_string(),
            Pod::new(PodInfo {
                pod_id: POD.to_string(),
                state: PodState::Stopped as i32,
                spec: Some(PodSpec::default()),
                ..Default::default()
            }),
        );
        let hung = hold_the_writer(&pods, dir.path());
        let start = tokio::spawn({
            let pods = pods.clone();
            async move { pods.start(POD, String::new()).await }
        });
        sleep(Duration::from_millis(200)).await;
        assert_eq!(pods.info(POD).unwrap().state(), PodState::Pending);
        start.abort();
        assert!(start.await.unwrap_err().is_cancelled());
        hung.open();
        assert!(
            within_5s(|| brought_up(&pods)).await,
            "the pod is {:?} and has no bring-up",
            pods.info(POD).unwrap().state()
        );
    }

    /// A CreatePod whose caller goes away while the pod's record waits for
    /// its fsync still brings the pod up once it is kept.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_create_whose_caller_goes_away_brings_the_pod_up() {
        let dir = tempfile::tempdir().unwrap();
        let pods = bare_pods(dir.path());
        let hung = hold_the_writer(&pods, dir.path());
        let spec = PodSpec {
            containers: vec![ContainerSpec {
                name: "main".to_string(),
                image_ref: "test".to_string(),
                ..Default::default()
            }],
            network: Some(feos_proto::pod_service::PodNetwork {
                none: true,
                ..Default::default()
            }),
            ..Default::default()
        };
        let create = tokio::spawn({
            let pods = pods.clone();
            async move {
                pods.create(POD.to_string(), spec, None, String::new())
                    .await
            }
        });
        sleep(Duration::from_millis(200)).await;
        assert!(pods.info(POD).is_err(), "added before it is kept");
        create.abort();
        assert!(create.await.unwrap_err().is_cancelled());
        hung.open();
        assert!(
            within_5s(|| brought_up(&pods)).await,
            "the pod is {:?} and has no bring-up",
            pods.info(POD).map(|info| info.state())
        );
    }

    /// A delete takes the pod out and removes its record in one go: a save
    /// of the pod that comes while the removal waits for the writer, as
    /// one of the addresses the network gives it when it comes up, finds
    /// no pod, and does not write the record back.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_deleted_pod_is_not_kept_again() {
        let dir = tempfile::tempdir().unwrap();
        let pods = bare_pods(dir.path());
        let info = PodInfo {
            pod_id: POD.to_string(),
            state: PodState::Stopped as i32,
            ..Default::default()
        };
        pods.inner.store.save(&info).await.unwrap();
        pods.pods().insert(POD.to_string(), Pod::new(info.clone()));
        let hung = hold_the_writer(&pods, dir.path());
        let forget = tokio::spawn({
            let pods = pods.clone();
            async move { pods.forget(POD).await.map(|pod| pod.is_some()) }
        });
        sleep(Duration::from_millis(200)).await;
        let late = pods.take_addresses(&PodInfo {
            ipv6_address: "fd00::2".to_string(),
            ..info
        });
        hung.open();
        assert!(forget.await.unwrap().unwrap(), "the pod was there");
        if let Some(late) = late {
            late.await.unwrap();
        }
        let record = dir.path().join(format!("pods/{POD}.pb"));
        assert!(!record.exists(), "the deleted pod's record is back");
        assert!(pods.info(POD).is_err());
    }
}
