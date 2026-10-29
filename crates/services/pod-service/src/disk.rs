// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods' disks (ADR 0011): every pod has one, a ublk device of diskd,
//! the node's disk daemon, which feosd reaches on its socket.
//!
//! A pod's disk starts from the blank data volume of the newest guest
//! installed, which diskd imports once as an L0, and holds what the pod
//! writes in its top. It lives as long as the pod: it is started for a
//! boot or a restore, stopped when the microVM ends, sealed and uploaded
//! when the pod hibernates, and removed when the pod is deleted or has
//! hibernated. All the node's traffic with the object store runs through
//! diskd.
//!
//! A pod's volumes (ADR 0016) are disks of diskd's too, `volume-<id>`,
//! each on an empty L0 that all volumes share, one chunk of zeroes, under
//! a disk as large as the volume.

use feos_proto::disk_service::{
    disk_service_client::DiskServiceClient, BakeRequest, CloseMemoryRequest, CreateDiskRequest,
    GetHeadRequest, GetObjectRequest, Head, ImportBaseRequest, Layer, LayerRef, MemoryRef,
    OpenDiskRequest, OpenMemoryRequest, PutHeadRequest, PutObjectRequest, RemoveDeviceRequest,
    SealFileRequest, SealRequest, StatsRequest, StatsResponse, StopDiskRequest,
};
use feos_proto::pod_service::BakedLayer;
use feos_proto::volume_service::{Volume, VolumeState};
use hyper_util::rt::TokioIo;
use log::{info, warn};
use microvm_service::vmm::installed::{self, BLANK_DATA_VOLUME};
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;
use tokio::time::{sleep, Instant};
use tonic::transport::{Channel, Endpoint, Uri};
use tonic::{Code, Status};
use tower::service_fn;

/// diskd's socket, in its state directory (`diskd --state-dir`).
pub const DISKD_SOCKET: &str = "/var/lib/feos/disk/diskd.sock";
/// How long a boot waits for a diskd that does not answer: one that
/// restarts recovers its devices before it serves.
const DISKD_WAIT: Duration = Duration::from_secs(20);
/// The image of the volumes' empty L0, in the directory the caller names:
/// one chunk of zeroes, the same layer on every node.
const EMPTY_LAYER: &str = "empty-layer.img";
const EMPTY_LAYER_BYTES: u64 = 64 << 10;

/// diskd's name of a volume's disk, and its prefix in the object store.
pub fn volume_disk(volume_id: &str) -> String {
    format!("volume-{volume_id}")
}

/// diskd, as the pods use it.
#[derive(Clone)]
pub struct Disks {
    socket: PathBuf,
    channel: Channel,
    /// The blank data volume's layer, by its image, and the volumes' empty
    /// one: imported once.
    blanks: Arc<tokio::sync::Mutex<HashMap<PathBuf, String>>>,
}

/// A call's error, saying that diskd answered it.
fn diskd(e: Status) -> Status {
    Status::new(e.code(), format!("diskd: {}", e.message()))
}

impl Disks {
    /// diskd on `socket`. It is connected to on the first call, and again
    /// after it restarted.
    pub fn new(socket: impl Into<PathBuf>) -> Self {
        let socket = socket.into();
        let path = socket.clone();
        let channel = Endpoint::from_static("http://diskd").connect_with_connector_lazy(
            service_fn(move |_: Uri| {
                let path = path.clone();
                async move {
                    tokio::net::UnixStream::connect(path)
                        .await
                        .map(TokioIo::new)
                }
            }),
        );
        Self {
            socket,
            channel,
            blanks: Default::default(),
        }
    }

    pub fn socket(&self) -> &Path {
        &self.socket
    }

    fn client(&self) -> DiskServiceClient<Channel> {
        DiskServiceClient::new(self.channel.clone())
    }

    /// Starts the pod's disk for a boot, and returns the path Firecracker
    /// takes: the disk the node has of the pod, or the one of its
    /// checkpoint `head_key`, or a new one, on a layer baked of the pod's
    /// `image` or on the blank data volume (ADR 0025). For a new disk of a
    /// pod with an image, it also returns what the disk was made on.
    pub async fn open(
        &self,
        pod_id: &str,
        head_key: Option<&str>,
        image: Option<(&str, &[BakedLayer])>,
    ) -> Result<(String, Option<BakedLayer>), Status> {
        if let Some(device) = self.open_existing(pod_id).await? {
            return Ok((device, None));
        }
        match head_key {
            Some(head_key) => Ok((self.open_checkpoint(pod_id, head_key).await?, None)),
            None => self.create(pod_id, image).await,
        }
    }

    /// Starts the disk the node has of `disk`, if it has one.
    async fn open_existing(&self, disk: &str) -> Result<Option<String>, Status> {
        let request = OpenDiskRequest {
            pod_id: disk.to_string(),
            head_key: String::new(),
        };
        // diskd may be starting, and recovering its devices first.
        let deadline = Instant::now() + DISKD_WAIT;
        loop {
            match self.client().open_disk(request.clone()).await {
                Ok(device) => return Ok(Some(device.into_inner().link_path)),
                Err(e) if e.code() == Code::NotFound => return Ok(None),
                Err(e) if e.code() == Code::Unavailable && Instant::now() < deadline => {
                    sleep(Duration::from_millis(500)).await;
                }
                Err(e) => return Err(diskd(e)),
            }
        }
    }

    /// Starts a volume's disk for a boot or a restore, and returns the path
    /// Firecracker takes: the disk the node has of it; for a checkpointed
    /// volume the one of its checkpoint; for a volume without its
    /// filesystem a blank one of its size, on the empty L0 of the image in
    /// `scratch`. A volume that has its filesystem gets no blank disk: one
    /// whose disk the node lost fails.
    pub async fn open_volume(&self, volume: &Volume, scratch: &Path) -> Result<String, Status> {
        let disk = volume_disk(&volume.volume_id);
        if let Some(device) = self.open_existing(&disk).await? {
            return Ok(device);
        }
        let checkpoint = volume
            .checkpoint
            .as_ref()
            .map(|checkpoint| checkpoint.head_key.as_str())
            .filter(|key| !key.is_empty());
        match checkpoint {
            Some(head_key) if volume.state() == VolumeState::Checkpointed => {
                self.open_checkpoint(&disk, head_key).await
            }
            _ if volume.formatted => Err(Status::failed_precondition(format!(
                "diskd has no disk of volume {}, which has its filesystem",
                volume.volume_id
            ))),
            _ => self.create_volume(&disk, volume.size_bytes, scratch).await,
        }
    }

    /// A new, blank disk of `size` bytes for a volume.
    async fn create_volume(&self, disk: &str, size: u64, scratch: &Path) -> Result<String, Status> {
        let create = |chain: String| {
            let mut client = self.client();
            let request = CreateDiskRequest {
                pod_id: disk.to_string(),
                chain: vec![chain],
                logical_size: size,
            };
            async move { client.create_disk(request).await }
        };
        let device = match create(self.empty_layer(scratch).await?).await {
            Ok(device) => device,
            // diskd may have lost the layer since it was imported.
            Err(e) if e.code() == Code::Internal => {
                warn!(
                    "Disks: The volumes' empty layer did not serve {disk}: {}; importing it again",
                    e.message()
                );
                self.blanks.lock().await.remove(&scratch.join(EMPTY_LAYER));
                create(self.empty_layer(scratch).await?)
                    .await
                    .map_err(diskd)?
            }
            Err(e) => return Err(diskd(e)),
        };
        info!("Disks: {disk} is a new disk of {size} bytes");
        Ok(device.into_inner().link_path)
    }

    /// The volumes' empty L0, imported the first time a volume needs it,
    /// from an image of zeroes in `scratch`.
    async fn empty_layer(&self, scratch: &Path) -> Result<String, Status> {
        let image = scratch.join(EMPTY_LAYER);
        let mut blanks = self.blanks.lock().await;
        if let Some(layer) = blanks.get(&image) {
            return Ok(layer.clone());
        }
        std::fs::create_dir_all(scratch)
            .and_then(|()| std::fs::File::create(&image))
            .and_then(|file| file.set_len(EMPTY_LAYER_BYTES))
            .map_err(|e| Status::internal(format!("write {}: {e}", image.display())))?;
        let request = ImportBaseRequest {
            path: image.to_string_lossy().into_owned(),
            chunk_size: 0,
        };
        let layer = self
            .client()
            .import_base(request)
            .await
            .map_err(diskd)?
            .into_inner();
        info!("Disks: The volumes' empty layer is {}", layer.layer_id);
        blanks.insert(image, layer.layer_id.clone());
        Ok(layer.layer_id)
    }

    /// Starts the pod's disk from its checkpoint, the head `head_key`. The
    /// node must have no disk of the pod.
    pub async fn open_checkpoint(&self, pod_id: &str, head_key: &str) -> Result<String, Status> {
        let request = OpenDiskRequest {
            pod_id: pod_id.to_string(),
            head_key: head_key.to_string(),
        };
        let device = self.client().open_disk(request).await.map_err(diskd)?;
        info!("Disks: {pod_id}'s disk comes from {head_key}");
        Ok(device.into_inner().link_path)
    }

    /// A new disk: on the newest of `image`'s baked layers that is made for
    /// the newest guest installed and its blank data volume and that the
    /// object store has, else on the blank data volume. Returns what the
    /// disk was made on when the pod has an image.
    async fn create(
        &self,
        pod_id: &str,
        image: Option<(&str, &[BakedLayer])>,
    ) -> Result<(String, Option<BakedLayer>), Status> {
        let (blank, guest_version) = self.blank().await?;
        let made = |layer_id: &str| {
            image.map(|(image_ref, _)| BakedLayer {
                image_ref: image_ref.to_string(),
                guest_version: guest_version.clone(),
                l0: blank.clone(),
                layer_id: layer_id.to_string(),
            })
        };
        if let Some((image_ref, layers)) = image {
            for layer in layers.iter().filter(|layer| {
                layer.image_ref == image_ref
                    && layer.guest_version == guest_version
                    && layer.l0 == blank
                    && !layer.layer_id.is_empty()
            }) {
                match self.create_on_layer(pod_id, &blank, &layer.layer_id).await {
                    Ok(device) => {
                        info!(
                            "Disks: {pod_id} has a new disk on layer {} of {image_ref}",
                            layer.layer_id
                        );
                        return Ok((device, made(&layer.layer_id)));
                    }
                    Err(e) => warn!(
                        "Disks: {pod_id} does not start on layer {} of {image_ref}: {}; passed over",
                        layer.layer_id,
                        e.message()
                    ),
                }
            }
        }
        let device = self.create_blank(pod_id).await?;
        Ok((device, made("")))
    }

    /// A new disk on the baked layer `layer` over the blank data volume's
    /// `blank`: its manifest is fetched from the object store first, unless
    /// diskd has it.
    async fn create_on_layer(
        &self,
        pod_id: &str,
        blank: &str,
        layer: &str,
    ) -> Result<String, Status> {
        let pull = LayerRef {
            layer_id: layer.to_string(),
            warm: false,
        };
        self.client().pull(pull).await.map_err(diskd)?;
        let request = CreateDiskRequest {
            pod_id: pod_id.to_string(),
            chain: vec![blank.to_string(), layer.to_string()],
            logical_size: 0,
        };
        let device = self.client().create_disk(request).await.map_err(diskd)?;
        Ok(device.into_inner().link_path)
    }

    /// A new disk on the blank data volume.
    async fn create_blank(&self, pod_id: &str) -> Result<String, Status> {
        let create = |chain: String| {
            let mut client = self.client();
            let request = CreateDiskRequest {
                pod_id: pod_id.to_string(),
                chain: vec![chain],
                logical_size: 0,
            };
            async move { client.create_disk(request).await }
        };
        let device = match create(self.blank().await?.0).await {
            Ok(device) => device,
            // diskd may have lost the layer since it was imported: once
            // more, imported anew.
            Err(e) if e.code() == Code::Internal => {
                warn!(
                    "Disks: The blank data volume did not serve {pod_id}: {}; importing it again",
                    e.message()
                );
                self.blanks.lock().await.clear();
                create(self.blank().await?.0).await.map_err(diskd)?
            }
            Err(e) => return Err(diskd(e)),
        };
        info!("Disks: {pod_id} has a new disk");
        Ok(device.into_inner().link_path)
    }

    /// The layer of the blank data volume of the newest guest installed,
    /// imported into diskd the first time a pod needs it, and that guest's
    /// version: its directory's name.
    async fn blank(&self) -> Result<(String, String), Status> {
        let guest = installed::newest_guest().ok_or_else(|| {
            Status::failed_precondition(format!(
                "no guest is installed in {}",
                installed::GUEST_DIR
            ))
        })?;
        let version = guest
            .file_name()
            .map(|name| name.to_string_lossy().into_owned())
            .unwrap_or_default();
        let image = guest.join(BLANK_DATA_VOLUME);
        let mut blanks = self.blanks.lock().await;
        if let Some(layer) = blanks.get(&image) {
            return Ok((layer.clone(), version));
        }
        if !image.is_file() {
            return Err(Status::failed_precondition(format!(
                "the guest in {} has no blank data volume {BLANK_DATA_VOLUME}",
                guest.display()
            )));
        }
        let request = ImportBaseRequest {
            path: image.to_string_lossy().into_owned(),
            chunk_size: 0,
        };
        let layer = self
            .client()
            .import_base(request)
            .await
            .map_err(diskd)?
            .into_inner();
        info!(
            "Disks: The blank data volume {} is layer {}",
            image.display(),
            layer.layer_id
        );
        blanks.insert(image, layer.layer_id.clone());
        Ok((layer.layer_id, version))
    }

    /// Seals the frozen top of the pod's running disk as a layer on its L0
    /// (ADR 0025): the guest's filesystem must be frozen, or diskd refuses.
    /// The layer is not uploaded yet: `push` does that.
    pub async fn bake(&self, pod_id: &str) -> Result<Layer, Status> {
        let request = BakeRequest {
            pod_id: pod_id.to_string(),
            upload: false,
        };
        Ok(self
            .client()
            .bake(request)
            .await
            .map_err(diskd)?
            .into_inner())
    }

    /// Uploads a layer to the object store: its chunks, then its manifest.
    pub async fn push(&self, layer_id: &str) -> Result<(), Status> {
        let request = LayerRef {
            layer_id: layer_id.to_string(),
            warm: false,
        };
        self.client().push(request).await.map_err(diskd)?;
        Ok(())
    }

    /// Stops the pod's disk, which keeps what the pod wrote: its microVM
    /// has ended. Nothing to do for a disk that is not started.
    pub async fn stop(&self, pod_id: &str) -> Result<(), Status> {
        let request = StopDiskRequest {
            pod_id: pod_id.to_string(),
        };
        match self.client().stop_disk(request).await {
            Ok(_) => Ok(()),
            Err(e) if e.code() == Code::NotFound => Ok(()),
            Err(e) => Err(diskd(e)),
        }
    }

    /// Removes the pod's disk from the node, with what it holds that is
    /// not sealed.
    pub async fn remove(&self, pod_id: &str) -> Result<(), Status> {
        self.stop(pod_id).await?;
        let request = RemoveDeviceRequest {
            pod_id: pod_id.to_string(),
        };
        self.client().remove_device(request).await.map_err(diskd)?;
        Ok(())
    }

    /// diskd's figures of the node, without those of its devices: its
    /// object store, and its version.
    pub async fn node_stats(&self) -> Result<StatsResponse, Status> {
        // A name that no device has.
        let request = StatsRequest {
            pod_id: "-".to_string(),
        };
        let stats = self.client().stats(request).await.map_err(diskd)?;
        Ok(stats.into_inner())
    }

    /// Fails unless diskd has an object store, which a pod hibernates
    /// into.
    pub async fn require_object_store(&self, pod_id: &str) -> Result<(), Status> {
        let request = StatsRequest {
            pod_id: pod_id.to_string(),
        };
        let stats = self.client().stats(request).await.map_err(diskd)?;
        if stats.into_inner().object_store {
            return Ok(());
        }
        Err(Status::failed_precondition(
            "diskd has no object store: pods do not hibernate on this node",
        ))
    }

    /// Seals the stopped disk's writes into its own layer and uploads its
    /// chain: a pod's, or a volume's. After `seal_local` it finds the disk
    /// sealed, and uploads. A call that is dropped stops the upload where
    /// it is; what is uploaded stays, and the next one skips it.
    pub async fn seal(&self, pod_id: &str) -> Result<Layer, Status> {
        self.seal_with(pod_id, true).await
    }

    /// Seals the stopped disk's writes into its own layer, on the node: a
    /// pod's, or a volume's. The disk opens again over that layer. A call
    /// that is dropped before diskd committed the layer leaves the disk as
    /// it was, over its top.
    pub async fn seal_local(&self, pod_id: &str) -> Result<Layer, Status> {
        self.seal_with(pod_id, false).await
    }

    async fn seal_with(&self, pod_id: &str, upload: bool) -> Result<Layer, Status> {
        let request = SealRequest {
            pod_id: pod_id.to_string(),
            upload,
        };
        Ok(self
            .client()
            .seal(request)
            .await
            .map_err(diskd)?
            .into_inner())
    }

    /// Seals a memory dump into a flat layer and uploads it.
    pub async fn seal_memory(&self, path: &Path) -> Result<Layer, Status> {
        let request = SealFileRequest {
            path: path.to_string_lossy().into_owned(),
            upload: true,
            chunk_size: 0,
        };
        Ok(self
            .client()
            .seal_file(request)
            .await
            .map_err(diskd)?
            .into_inner())
    }

    /// Uploads a file to the object store at `key`.
    pub async fn put_object(&self, key: &str, path: &Path) -> Result<(), Status> {
        let request = PutObjectRequest {
            key: key.to_string(),
            path: path.to_string_lossy().into_owned(),
        };
        self.client().put_object(request).await.map_err(diskd)?;
        Ok(())
    }

    /// Downloads the object at `key` into a file.
    pub async fn get_object(&self, key: &str, path: &Path) -> Result<(), Status> {
        let request = GetObjectRequest {
            key: key.to_string(),
            path: path.to_string_lossy().into_owned(),
        };
        self.client().get_object(request).await.map_err(diskd)?;
        Ok(())
    }

    /// Uploads the head of the checkpoint `epoch` of a pod's disk, last,
    /// or of a volume's, before its pod's, which names it in
    /// `volume_heads`.
    pub async fn put_head(
        &self,
        pod_id: &str,
        epoch: u64,
        memory: Option<MemoryRef>,
        volume_heads: Vec<String>,
    ) -> Result<Head, Status> {
        let request = PutHeadRequest {
            pod_id: pod_id.to_string(),
            epoch,
            memory,
            volume_heads,
        };
        Ok(self
            .client()
            .put_head(request)
            .await
            .map_err(diskd)?
            .into_inner())
    }

    pub async fn get_head(&self, key: &str) -> Result<Head, Status> {
        let request = GetHeadRequest {
            key: key.to_string(),
        };
        Ok(self
            .client()
            .get_head(request)
            .await
            .map_err(diskd)?
            .into_inner())
    }

    /// Serves the pod's memory from a memory layer on `socket`, for
    /// Firecracker to restore it from.
    pub async fn open_memory(
        &self,
        pod_id: &str,
        layer: &str,
        socket: &Path,
    ) -> Result<(), Status> {
        let request = OpenMemoryRequest {
            pod_id: pod_id.to_string(),
            layer: layer.to_string(),
            socket_path: socket.to_string_lossy().into_owned(),
        };
        self.client().open_memory(request).await.map_err(diskd)?;
        Ok(())
    }

    /// Lets go of the pod's memory, if diskd serves it. Its handler also
    /// ends when Firecracker does.
    pub async fn close_memory(&self, pod_id: &str) {
        let request = CloseMemoryRequest {
            pod_id: pod_id.to_string(),
        };
        if let Err(e) = self.client().close_memory(request).await {
            warn!(
                "Disks: Failed to let go of {pod_id}'s memory: {}",
                e.message()
            );
        }
    }
}
