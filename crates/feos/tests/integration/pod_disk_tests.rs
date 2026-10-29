// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Pods' disks from diskd (ADR 0011) and their hibernation (ADR 0008): what
//! is on a pod's disk survives a stop, a crash and a trip through object
//! storage, and its memory comes back where the node can restore it.
//!
//! The pods have no network, so that the tests need no registry: what is
//! on a disk is told by container records, which the guest keeps on its
//! data volume and lists. A test puts a record on a stopped pod's disk
//! from the node; the guest's own changes are a record it deletes.
//! Hibernation needs diskd with an object store, as `make test-pods` gives
//! it when it has one; without, those tests skip. feosd reaches diskd
//! through the tests' gate (`diskd_gate`), which holds a hibernation at the
//! call a test names, so that a resume comes while it uploads, or after its
//! point of no return.

use super::diskd_gate::{diskd_gate, GateCall};
use super::fixtures::{verify_microvm_cleanup, PodGuard};
use super::pod_tests::{for_pod, get_pod, guest_ping, legacy_pod_id, microvm_metadata, new_pod_id};
use super::{
    ensure_server, get_public_clients, installed_guest, OBJECT_STORE, PUBLIC_SERVER_ADDRESS,
};
use anyhow::{bail, ensure, Context, Result};
use feos_proto::container_service::{
    container_service_client::ContainerServiceClient, DeleteContainerRequest, ListContainersRequest,
};
use feos_proto::disk_service::{
    disk_service_client::DiskServiceClient, evict_request, EvictRequest, GetHeadRequest,
    OpenDiskRequest, StatsRequest, StopDiskRequest,
};
use feos_proto::pod_service::{
    pod_service_client::PodServiceClient, CreatePodRequest, DeletePodRequest, HibernatePodRequest,
    HibernatePodResponse, Hibernation, HibernationPhase, PodInfo, PodNetwork, PodSpec, PodState,
    Resources, RestorePodRequest, ResumePodRequest, StartPodRequest, StopPodRequest,
};
use hyper_util::rt::TokioIo;
use log::info;
use microvm_service::vm_dir;
use nix::mount::{mount, umount, MsFlags};
use nix::sys::signal::{kill, Signal};
use nix::unistd::Pid;
use pod_service::disk::DISKD_SOCKET;
use pod_service::network;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};
use tokio::task::JoinHandle;
use tokio::time::timeout;
use tonic::transport::{Channel, Endpoint, Uri};
use tonic::{Code, Status};
use tower::service_fn;

/// How long a pod may take to come up.
const UP_TIMEOUT: Duration = Duration::from_secs(120);

/// diskd's state directory, beside its socket.
pub(super) fn diskd_state() -> PathBuf {
    Path::new(DISKD_SOCKET)
        .parent()
        .expect("the socket is in the state directory")
        .to_path_buf()
}

pub(super) async fn diskd() -> Result<DiskServiceClient<Channel>> {
    let channel = Endpoint::from_static("http://diskd")
        .connect_with_connector(service_fn(|_: Uri| async {
            tokio::net::UnixStream::connect(DISKD_SOCKET)
                .await
                .map(TokioIo::new)
        }))
        .await?;
    Ok(DiskServiceClient::new(channel))
}

/// A pod without containers and without a network.
fn spec() -> PodSpec {
    PodSpec {
        resources: Some(Resources {
            vcpus: 1,
            memory_mib: 256,
        }),
        network: Some(PodNetwork {
            none: true,
            ..Default::default()
        }),
        ..Default::default()
    }
}

/// Waits until the pod runs.
pub(super) async fn wait_until_up(
    pods: &mut PodServiceClient<Channel>,
    pod_id: &str,
) -> Result<PodInfo> {
    timeout(UP_TIMEOUT, async {
        loop {
            let pod = get_pod(pods, pod_id).await?;
            match pod.state() {
                PodState::Running => return Ok(pod),
                PodState::Failed => bail!("pod {pod_id} failed: {}", pod.reason),
                _ => tokio::time::sleep(Duration::from_millis(200)).await,
            }
        }
    })
    .await
    .with_context(|| format!("pod {pod_id} did not come up"))?
}

/// The pod's disk on the node: diskd's directory of it.
pub(super) fn disk_dir(pod_id: &str) -> PathBuf {
    diskd_state().join("live").join(pod_id)
}

/// Puts container records named `names` on the stopped pod's disk, as the
/// guest keeps them, with `marker` for their image: the disk is started,
/// mounted on the node, written and stopped again.
async fn plant(pod_id: &str, names: &[&str], marker: &str) -> Result<()> {
    let mut disks = diskd().await?;
    let device = disks
        .open_disk(OpenDiskRequest {
            pod_id: pod_id.to_string(),
            head_key: String::new(),
        })
        .await?
        .into_inner();
    let dir = tempfile::tempdir()?;
    mount(
        Some(device.dev_path.as_str()),
        dir.path(),
        Some("ext4"),
        MsFlags::empty(),
        None::<&str>,
    )
    .context("mount the pod's disk")?;
    let written = (|| -> Result<()> {
        for name in names {
            let container = dir.path().join("containers").join(name);
            std::fs::create_dir_all(&container)?;
            let record = serde_json::json!({
                "container_id": name,
                "image_uuid": uuid::Uuid::nil().to_string(),
                "state": "STOPPED",
                "pid": null,
                "image_ref": marker,
                "command": [],
                "env": {},
            });
            std::fs::write(container.join("container.json"), record.to_string())?;
        }
        Ok(())
    })();
    nix::unistd::sync();
    umount(dir.path()).context("unmount the pod's disk")?;
    disks
        .stop_disk(StopDiskRequest {
            pod_id: pod_id.to_string(),
        })
        .await?;
    written
}

/// The container records the pod's guest lists: name and image.
async fn records(pod_id: &str) -> Result<Vec<(String, String)>> {
    let mut containers = ContainerServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    let mut listed: Vec<(String, String)> = containers
        .list_containers(for_pod(pod_id, ListContainersRequest {}))
        .await?
        .into_inner()
        .containers
        .into_iter()
        .map(|c| (c.container_id, c.config.unwrap_or_default().image_ref))
        .collect();
    listed.sort();
    Ok(listed)
}

fn marked(names: &[&str], marker: &str) -> Vec<(String, String)> {
    names
        .iter()
        .map(|name| (name.to_string(), marker.to_string()))
        .collect()
}

pub(super) async fn start(pods: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<PodInfo> {
    pods.start_pod(StartPodRequest {
        pod_id: pod_id.to_string(),
    })
    .await?;
    wait_until_up(pods, pod_id).await
}

pub(super) async fn stop(pods: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<()> {
    pods.stop_pod(StopPodRequest {
        pod_id: pod_id.to_string(),
    })
    .await?;
    Ok(())
}

pub(super) fn pid(pod: &PodInfo) -> Result<i32> {
    Ok(pod
        .microvm
        .as_ref()
        .context("a running pod has a microVM")?
        .pid as i32)
}

#[tokio::test]
async fn test_pod_disk_survives_stop_and_crash() -> Result<()> {
    if installed_guest()?.is_none() {
        return Ok(());
    }
    ensure_server().await;
    let (mut pods, _, _) = get_public_clients().await?;

    let pod_id = new_pod_id();
    pods.create_pod(CreatePodRequest {
        pod_id: pod_id.clone(),
        spec: Some(spec()),
        image_layers: None,
    })
    .await?;
    let mut guard = PodGuard::new(pod_id.clone());
    let pod = wait_until_up(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);

    // The pod's disk is a ublk device of diskd's, linked in its directory,
    // and the guest's data volume: a new disk holds no containers.
    let link = std::fs::read_link(disk_dir(&pod_id).join("disk"))?;
    assert!(
        link.to_string_lossy().starts_with("/dev/ublkb"),
        "the pod's disk is {}",
        link.display()
    );
    assert!(records(&pod_id).await?.is_empty());
    let first_boot = guest_ping(&pod_id).await?.boot_id;

    info!("Stopping pod {pod_id}: its disk stops, and keeps what it holds");
    stop(&mut pods, &pod_id).await?;
    let stats = diskd()
        .await?
        .stats(StatsRequest {
            pod_id: pod_id.clone(),
        })
        .await?
        .into_inner();
    assert!(stats.devices.is_empty(), "a stopped pod's disk is stopped");
    assert!(disk_dir(&pod_id).join("top.img").is_file());
    let marker = format!("marker/{}", uuid::Uuid::new_v4());
    plant(&pod_id, &["keep", "drop"], &marker).await?;

    let pod = start(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    assert_ne!(guest_ping(&pod_id).await?.boot_id, first_boot, "a new boot");
    assert_eq!(
        records(&pod_id).await?,
        marked(&["drop", "keep"], &marker),
        "the guest booted on another disk"
    );
    // The guest's own writes stay, too.
    let mut containers = ContainerServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    containers
        .delete_container(for_pod(
            &pod_id,
            DeleteContainerRequest {
                container_id: "drop".to_string(),
            },
        ))
        .await?;
    stop(&mut pods, &pod_id).await?;
    let pod = start(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));
    info!("Pod {pod_id} booted anew on its disk, with what the node and the guest wrote");

    let crashed = pid(&pod)?;
    info!("Killing Firecracker, pid {crashed}");
    kill(Pid::from_raw(crashed), Signal::SIGKILL)?;
    timeout(Duration::from_secs(30), async {
        while get_pod(&mut pods, &pod_id).await?.state() != PodState::Failed {
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
        anyhow::Ok(())
    })
    .await
    .context("the pod did not fail")??;
    let pod = start(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    assert_eq!(
        records(&pod_id).await?,
        marked(&["keep"], &marker),
        "a crash lost the disk"
    );
    info!("Pod {pod_id} came back from a crash on its disk");

    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    verify_microvm_cleanup(&pod_id);
    assert!(
        !disk_dir(&pod_id).exists(),
        "the pod's disk is gone with it"
    );
    guard.disable_cleanup();
    Ok(())
}

/// Hibernates the pod; returns its checkpoint's head.
async fn hibernate(pods: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<String> {
    let response = pods
        .hibernate_pod(HibernatePodRequest {
            pod_id: pod_id.to_string(),
            ..Default::default()
        })
        .await?
        .into_inner();
    let checkpoint = response
        .checkpoint
        .context("a hibernation has a checkpoint")?;
    info!(
        "Pod {pod_id} is in {}: {} MiB of memory, {} KiB on its disk",
        checkpoint.head_key,
        response.memory_bytes >> 20,
        response.disk_bytes >> 10
    );
    assert!(
        checkpoint.memory,
        "a running pod hibernates with its memory"
    );
    assert!(response.memory_bytes > 0 && response.disk_bytes > 0);
    let pod = get_pod(pods, pod_id).await?;
    assert_eq!(pod.state(), PodState::Hibernated);
    assert_eq!(pod.checkpoint.as_ref(), Some(&checkpoint));
    // Nothing of it is left on the node.
    assert!(!vm_dir(pod_id).exists(), "a hibernated pod has no microVM");
    assert!(!disk_dir(pod_id).exists(), "a hibernated pod has no disk");
    assert!(
        !Path::new("/sys/class/net")
            .join(network::tap_name(pod_id))
            .exists(),
        "a hibernated pod has no TAP device"
    );
    Ok(checkpoint.head_key)
}

#[tokio::test]
async fn test_pod_hibernate_and_restore() -> Result<()> {
    if installed_guest()?.is_none() {
        return Ok(());
    }
    if std::env::var("S3_BUCKET").map_or(true, |bucket| bucket.is_empty()) {
        log::warn!("Skipping test: no object store (S3_BUCKET) to hibernate to.");
        return Ok(());
    }
    ensure_server().await;
    let _store = OBJECT_STORE.read().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let mut disks = diskd().await?;

    // On the network, with metadata: a restore gives the pod addresses
    // anew, and its metadata has them.
    let networked = PodSpec {
        network: None,
        metadata: [("role".to_string(), "hibernated".to_string())].into(),
        ..spec()
    };
    // A pod with a UUID, as hibernated pods from before short IDs have:
    // its snapshot names its TAP device by the UUID's first 8 hex digits.
    let pod_id = legacy_pod_id();
    pods.create_pod(CreatePodRequest {
        pod_id: pod_id.clone(),
        spec: Some(networked.clone()),
        image_layers: None,
    })
    .await?;
    let mut guard = PodGuard::new(pod_id.clone());
    wait_until_up(&mut pods, &pod_id).await?;
    stop(&mut pods, &pod_id).await?;
    let marker = format!("marker/{}", uuid::Uuid::new_v4());
    plant(&pod_id, &["keep"], &marker).await?;
    let pod = start(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));
    let before = guest_ping(&pod_id).await?;
    let placed = pod.ipv6_address.clone();

    let head_key = hibernate(&mut pods, &pod_id).await?;

    // Another node: one that has neither the pod nor anything of it in
    // its cache.
    info!("Forgetting pod {pod_id} and what diskd has of it");
    let head = disks
        .get_head(GetHeadRequest {
            key: head_key.clone(),
        })
        .await?
        .into_inner();
    assert_eq!(head.chain.len(), 2, "the blank volume and the pod's layer");
    let memory = head.memory.clone().context("the head has the memory")?;
    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    disks
        .evict(EvictRequest {
            target: evict_request::Target::All as i32,
            force: false,
        })
        .await?;
    // The pod's own layers come from the object store again; the blank
    // volume is every pod's.
    let layers = diskd_state().join("layers");
    for layer in [&head.chain[1], &memory.layer] {
        std::fs::remove_file(layers.join(format!("{layer}.manifest")))?;
    }

    info!("Restoring pod {pod_id} from {head_key}");
    let restored = pods
        .restore_pod(RestorePodRequest {
            pod_id: pod_id.clone(),
            head_key: head_key.clone(),
            cold: false,
        })
        .await?
        .into_inner();
    assert!(
        restored.memory_restored,
        "the memory was not restored: {}",
        restored.reason
    );
    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    assert_eq!(
        pod.spec,
        Some(networked),
        "the spec comes with the checkpoint"
    );
    guard.set_pid(pid(&pod)?);
    // No address is handed out twice: the restored pod has a new one, and
    // the metadata that went in after the snapshot's load says so.
    assert_ne!(pod.ipv6_address, placed);
    let metadata = microvm_metadata(&pod_id).await?;
    assert_eq!(metadata["feos"]["ipv6"], pod.ipv6_address.as_str());
    assert_eq!(metadata["feos"]["ipv4"], pod.ipv4_address.as_str());
    assert_eq!(metadata["metadata"]["role"], "hibernated");
    // The same boot, with its memory: the ping count goes on.
    let after = guest_ping(&pod_id).await?;
    assert_eq!(after.boot_id, before.boot_id, "the same boot");
    assert!(after.tick > before.tick, "the guest forgot its pings");
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));
    for layer in [&head.chain[1], &memory.layer] {
        assert!(layers.join(format!("{layer}.manifest")).is_file());
    }
    let stats = disks
        .stats(StatsRequest {
            pod_id: pod_id.clone(),
        })
        .await?
        .into_inner();
    let served = stats.memories.first().context("diskd serves the memory")?;
    info!(
        "diskd served {} faults, {} chunks from the object store",
        served.faults, served.s3_fetches
    );
    assert!(
        served.s3_fetches > 0,
        "the memory came from the object store"
    );

    // Without its memory: a new boot on the checkpoint's disk.
    info!("Hibernating pod {pod_id}, and restoring it without its memory");
    let second = hibernate(&mut pods, &pod_id).await?;
    let restored = pods
        .restore_pod(RestorePodRequest {
            pod_id: pod_id.clone(),
            head_key: String::new(),
            cold: true,
        })
        .await?
        .into_inner();
    assert!(!restored.memory_restored);
    let pod = wait_until_up(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    assert_ne!(
        guest_ping(&pod_id).await?.boot_id,
        before.boot_id,
        "a new boot"
    );
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));
    // Hibernation after hibernation, the chain keeps its shape.
    let head = disks
        .get_head(GetHeadRequest { key: second })
        .await?
        .into_inner();
    assert_eq!(head.chain.len(), 2, "the chain grew: {:?}", head.chain);

    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    verify_microvm_cleanup(&pod_id);
    assert!(!disk_dir(&pod_id).exists());
    guard.disable_cleanup();
    Ok(())
}

/// Whether pods hibernate here: with the guest, Firecracker, diskd and an
/// object store.
pub(super) fn can_hibernate() -> Result<bool> {
    if installed_guest()?.is_none() {
        return Ok(false);
    }
    if std::env::var("S3_BUCKET").map_or(true, |bucket| bucket.is_empty()) {
        log::warn!("Skipping test: no object store (S3_BUCKET) to hibernate to.");
        return Ok(false);
    }
    Ok(true)
}

/// How long a hibernation may take to reach a call of diskd's: a suspend
/// and a seal on the node come first.
const STEP_TIMEOUT: Duration = Duration::from_secs(120);
/// How long a resume may take that abandons a hibernation.
const ABANDON_TIMEOUT: Duration = Duration::from_secs(30);

/// Starts a hibernation of the pod, as the control plane asks for it with
/// its op's id; its answer.
pub(super) fn spawn_hibernation(
    pods: &PodServiceClient<Channel>,
    pod_id: &str,
    request_id: &str,
    no_abandon: bool,
) -> JoinHandle<Result<HibernatePodResponse, Status>> {
    let mut pods = pods.clone();
    let request = HibernatePodRequest {
        pod_id: pod_id.to_string(),
        no_abandon,
        request_id: request_id.to_string(),
    };
    tokio::spawn(async move {
        pods.hibernate_pod(request)
            .await
            .map(|response| response.into_inner())
    })
}

/// The pod's hibernation in flight, as GetPod shows it.
pub(super) async fn hibernation(
    pods: &mut PodServiceClient<Channel>,
    pod_id: &str,
) -> Result<Hibernation> {
    get_pod(pods, pod_id)
        .await?
        .hibernation
        .context("GetPod shows no hibernation in flight")
}

/// Resumes the pod, and fails unless it does within `within`.
pub(super) async fn resume(
    pods: &mut PodServiceClient<Channel>,
    pod_id: &str,
    within: Duration,
) -> Result<()> {
    let request = ResumePodRequest {
        pod_id: pod_id.to_string(),
    };
    timeout(within, pods.resume_pod(request))
        .await
        .with_context(|| format!("the resume did not return within {}s", within.as_secs()))??;
    Ok(())
}

/// Whether the object store has the head `key`.
pub(super) async fn has_head(disks: &mut DiskServiceClient<Channel>, key: String) -> Result<bool> {
    match disks.get_head(GetHeadRequest { key }).await {
        Ok(_) => Ok(true),
        Err(e) if e.code() == Code::NotFound => Ok(false),
        Err(e) => Err(e.into()),
    }
}

/// The key of the pod's head of the checkpoint `epoch`.
pub(super) fn head_key(pod_id: &str, epoch: u64) -> String {
    format!("snapshots/{pod_id}/{epoch}/head.json")
}

/// Fails unless `status` is a hibernation's that a resume abandoned.
pub(super) fn assert_abandoned(status: &Status, pod_id: &str) {
    assert_eq!(status.code(), Code::Aborted, "{status:?}");
    assert_eq!(
        status.message(),
        format!("pod {pod_id} is resumed: its hibernation was abandoned")
    );
}

/// diskd's uploads to the object store so far, whole-object PUTs.
async fn s3_puts(disks: &mut DiskServiceClient<Channel>) -> Result<u64> {
    let request = StatsRequest {
        pod_id: "-".to_string(),
    };
    Ok(disks.stats(request).await?.into_inner().s3_puts)
}

/// Waits until diskd uploaded `count` more objects than `since`.
async fn s3_puts_rise(
    disks: &mut DiskServiceClient<Channel>,
    since: u64,
    count: u64,
) -> Result<u64> {
    timeout(Duration::from_secs(120), async {
        loop {
            let puts = s3_puts(disks).await?;
            if puts >= since + count {
                return anyhow::Ok(puts);
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    })
    .await
    .with_context(|| format!("diskd did not upload {count} objects"))?
}

/// The size of the data `fill` writes, and how many chunks of 64 KiB it is
/// in diskd.
const FILL_BYTES: u64 = 256 << 20;
const FILL_CHUNKS: u64 = FILL_BYTES >> 16;

/// Writes `FILL_BYTES` of data that no disk has into a file on the stopped
/// pod's disk, from the node, so that the disk's next upload takes a while.
async fn fill(pod_id: &str) -> Result<()> {
    let mut disks = diskd().await?;
    let device = disks
        .open_disk(OpenDiskRequest {
            pod_id: pod_id.to_string(),
            head_key: String::new(),
        })
        .await?
        .into_inner();
    let dir = tempfile::tempdir()?;
    mount(
        Some(device.dev_path.as_str()),
        dir.path(),
        Some("ext4"),
        MsFlags::empty(),
        None::<&str>,
    )
    .context("mount the pod's disk")?;
    let written = (|| -> Result<()> {
        // xorshift64, from a seed of this run's.
        let mut state = uuid::Uuid::new_v4().as_u64_pair().0 | 1;
        let mut file = std::fs::File::create(dir.path().join("fill"))?;
        let mut block = vec![0u8; 1 << 20];
        for _ in 0..FILL_BYTES / block.len() as u64 {
            for word in block.as_chunks_mut::<8>().0 {
                state ^= state << 13;
                state ^= state >> 7;
                state ^= state << 17;
                *word = state.to_le_bytes();
            }
            file.write_all(&block)?;
        }
        file.sync_all()?;
        Ok(())
    })();
    nix::unistd::sync();
    umount(dir.path()).context("unmount the pod's disk")?;
    disks
        .stop_disk(StopDiskRequest {
            pod_id: pod_id.to_string(),
        })
        .await?;
    written
}

/// How many chunks of 64 KiB diskd admits to its cache at a time when it
/// seals new data: 64 MiB of them.
const SEAL_WAVE: u64 = (64 << 20) >> 16;

/// The entries in diskd's chunk cache.
async fn cache_entries(disks: &mut DiskServiceClient<Channel>) -> Result<u64> {
    let request = StatsRequest {
        pod_id: "-".to_string(),
    };
    Ok(disks.stats(request).await?.into_inner().cache_entries)
}

/// The layers of the disk's row in diskd's state, its shared ones first.
fn disk_chain(disk: &str) -> Result<Vec<String>> {
    let state: serde_json::Value =
        serde_json::from_slice(&std::fs::read(diskd_state().join("diskd.state"))?)?;
    let chain = state["devices"][disk]["chain"]
        .as_array()
        .with_context(|| format!("diskd has no row of disk {disk}"))?;
    Ok(chain
        .iter()
        .filter_map(|layer| layer.as_str().map(str::to_string))
        .collect())
}

/// A pod of `spec()`, up.
async fn up_pod(pods: &mut PodServiceClient<Channel>) -> Result<(String, PodGuard)> {
    let pod_id = new_pod_id();
    pods.create_pod(CreatePodRequest {
        pod_id: pod_id.clone(),
        spec: Some(spec()),
        image_layers: None,
    })
    .await?;
    let mut guard = PodGuard::new(pod_id.clone());
    let pod = wait_until_up(pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    Ok((pod_id, guard))
}

async fn delete(
    pods: &mut PodServiceClient<Channel>,
    pod_id: &str,
    mut guard: PodGuard,
) -> Result<()> {
    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.to_string(),
    })
    .await?;
    verify_microvm_cleanup(pod_id);
    assert!(!disk_dir(pod_id).exists());
    guard.disable_cleanup();
    Ok(())
}

#[tokio::test]
async fn test_resume_abandons_the_disk_upload() -> Result<()> {
    if !can_hibernate()? {
        return Ok(());
    }
    ensure_server().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let mut disks = diskd().await?;
    let (pod_id, mut guard) = up_pod(&mut pods).await?;

    // A checkpoint first: one that the abandoned hibernation does not
    // replace.
    let earlier = {
        let _store = OBJECT_STORE.read().await;
        let head_key = hibernate(&mut pods, &pod_id).await?;
        resume(&mut pods, &pod_id, STEP_TIMEOUT).await?;
        let pod = get_pod(&mut pods, &pod_id).await?;
        assert_eq!(pod.state(), PodState::Running);
        guard.set_pid(pid(&pod)?);
        let earlier = pod
            .checkpoint
            .context("a restored pod has its checkpoint")?;
        assert_eq!(earlier.head_key, head_key);
        earlier
    };
    // Then data to upload, which takes a while.
    stop(&mut pods, &pod_id).await?;
    let marker = format!("marker/{}", uuid::Uuid::new_v4());
    plant(&pod_id, &["keep", "drop"], &marker).await?;
    fill(&pod_id).await?;
    let pod = start(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    let before = guest_ping(&pod_id).await?;

    info!("Hibernating pod {pod_id}, and resuming it while its disk uploads");
    let (epoch, abandoned_puts) = {
        // The only uploads now, so that they can be counted.
        let _store = OBJECT_STORE.write().await;
        let puts = s3_puts(&mut disks).await?;
        let id = pod_id.clone();
        let upload = diskd_gate().pass(
            move |call| matches!(call, GateCall::Seal(seal) if seal.pod_id == id && seal.upload),
        );
        let hibernation = spawn_hibernation(&pods, &pod_id, "r1", false);
        timeout(STEP_TIMEOUT, upload.arrived())
            .await
            .context("the disk's upload did not begin")?;
        // The upload is under way in diskd.
        s3_puts_rise(&mut disks, puts, 1000).await?;
        let shown = self::hibernation(&mut pods, &pod_id).await?;
        assert_eq!(shown.phase(), HibernationPhase::UploadingDisk);
        assert!(shown.abandonable);
        assert!(shown.epoch > earlier.epoch, "{shown:?}");
        assert_eq!(shown.request_ids, ["r1"]);

        resume(&mut pods, &pod_id, ABANDON_TIMEOUT).await?;
        assert_abandoned(&hibernation.await?.unwrap_err(), &pod_id);
        assert!(
            timeout(ABANDON_TIMEOUT, upload.dropped()).await?,
            "diskd's upload was not dropped"
        );
        // diskd stopped uploading.
        tokio::time::sleep(Duration::from_secs(1)).await;
        let stopped = s3_puts(&mut disks).await?;
        tokio::time::sleep(Duration::from_secs(3)).await;
        assert_eq!(s3_puts(&mut disks).await?, stopped, "diskd uploads on");
        (shown.epoch, stopped - puts)
    };
    info!("The abandoned upload uploaded {abandoned_puts} of the {FILL_CHUNKS} chunks filled");

    // The pod runs on, from its snapshot on the node.
    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    assert_eq!(pod.hibernation, None);
    assert_eq!(pod.checkpoint.as_ref(), Some(&earlier));
    guard.set_pid(pid(&pod)?);
    let after = guest_ping(&pod_id).await?;
    assert_eq!(after.boot_id, before.boot_id, "the same boot");
    assert!(after.tick > before.tick, "the guest forgot its pings");
    assert!(!has_head(&mut disks, head_key(&pod_id, epoch)).await?);
    // A late call of the abandoned hibernation starts no other.
    let late = spawn_hibernation(&pods, &pod_id, "r1", false).await?;
    assert_abandoned(&late.unwrap_err(), &pod_id);
    assert_eq!(
        get_pod(&mut pods, &pod_id).await?.state(),
        PodState::Running
    );

    // The disk opened again over its sealed layer: what the guest writes
    // now goes into the next checkpoint, with what was there.
    let mut containers = ContainerServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    containers
        .delete_container(for_pod(
            &pod_id,
            DeleteContainerRequest {
                container_id: "drop".to_string(),
            },
        ))
        .await?;
    info!("Hibernating pod {pod_id} again: the chunks uploaded before count");
    let (checkpoint, upload_puts) = {
        let _store = OBJECT_STORE.write().await;
        let puts = s3_puts(&mut disks).await?;
        let id = pod_id.clone();
        let upload = diskd_gate().pass(
            move |call| matches!(call, GateCall::Seal(seal) if seal.pod_id == id && seal.upload),
        );
        let id = pod_id.clone();
        let memory = diskd_gate()
            .hold(move |call| matches!(call, GateCall::SealFile(seal) if seal.path.contains(&id)));
        let hibernation = spawn_hibernation(&pods, &pod_id, "r2", false);
        timeout(Duration::from_secs(600), memory.arrived())
            .await
            .context("the memory's upload did not begin")?;
        assert!(!upload.dropped().await, "the disk's upload was dropped");
        let upload_puts = s3_puts(&mut disks).await? - puts;
        memory.release();
        let response = hibernation.await??;
        (
            response
                .checkpoint
                .context("a hibernation has a checkpoint")?,
            upload_puts,
        )
    };
    info!(
        "The disk's upload took {upload_puts} objects after the abandoned one's {abandoned_puts}"
    );
    assert!(
        upload_puts < FILL_CHUNKS,
        "the upload took {upload_puts} objects: the {abandoned_puts} uploaded before did not count"
    );
    assert!(has_head(&mut disks, checkpoint.head_key.clone()).await?);
    resume(&mut pods, &pod_id, STEP_TIMEOUT).await?;
    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    assert_eq!(pod.checkpoint.as_ref(), Some(&checkpoint));
    guard.set_pid(pid(&pod)?);
    assert_eq!(guest_ping(&pod_id).await?.boot_id, before.boot_id);
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));

    delete(&mut pods, &pod_id, guard).await
}

#[tokio::test]
async fn test_resume_abandons_the_memory_upload() -> Result<()> {
    if !can_hibernate()? {
        return Ok(());
    }
    ensure_server().await;
    let _store = OBJECT_STORE.read().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let mut disks = diskd().await?;
    let (pod_id, mut guard) = up_pod(&mut pods).await?;
    let before = guest_ping(&pod_id).await?;

    info!("Hibernating pod {pod_id}, and resuming it while its memory uploads");
    let id = pod_id.clone();
    let memory = diskd_gate()
        .hold(move |call| matches!(call, GateCall::SealFile(seal) if seal.path.contains(&id)));
    let hibernation = spawn_hibernation(&pods, &pod_id, "m1", false);
    timeout(STEP_TIMEOUT, memory.arrived())
        .await
        .context("the memory's upload did not begin")?;
    let shown = self::hibernation(&mut pods, &pod_id).await?;
    assert_eq!(shown.phase(), HibernationPhase::UploadingMemory);
    assert!(shown.abandonable);
    assert_eq!(shown.request_ids, ["m1"]);
    resume(&mut pods, &pod_id, ABANDON_TIMEOUT).await?;
    assert_abandoned(&hibernation.await?.unwrap_err(), &pod_id);
    assert!(
        timeout(ABANDON_TIMEOUT, memory.dropped()).await?,
        "the held call was not dropped"
    );

    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    assert_eq!(pod.hibernation, None);
    assert_eq!(pod.checkpoint, None);
    guard.set_pid(pid(&pod)?);
    let after = guest_ping(&pod_id).await?;
    assert_eq!(after.boot_id, before.boot_id, "the same boot");
    assert!(after.tick > before.tick, "the guest forgot its pings");
    assert!(!has_head(&mut disks, head_key(&pod_id, shown.epoch)).await?);

    // Its disk, sealed and uploaded, opened again: the next hibernation
    // takes it from there.
    hibernate(&mut pods, &pod_id).await?;
    resume(&mut pods, &pod_id, STEP_TIMEOUT).await?;
    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    guard.set_pid(pid(&pod)?);
    assert_eq!(guest_ping(&pod_id).await?.boot_id, before.boot_id);

    delete(&mut pods, &pod_id, guard).await
}

#[tokio::test]
async fn test_resume_abandons_the_local_seal() -> Result<()> {
    if !can_hibernate()? {
        return Ok(());
    }
    ensure_server().await;
    let _store = OBJECT_STORE.read().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let mut disks = diskd().await?;
    let (pod_id, mut guard) = up_pod(&mut pods).await?;

    // Data that no layer has, which the local seal takes a while for.
    stop(&mut pods, &pod_id).await?;
    let marker = format!("marker/{}", uuid::Uuid::new_v4());
    plant(&pod_id, &["keep"], &marker).await?;
    fill(&pod_id).await?;
    let pod = start(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    let before = guest_ping(&pod_id).await?;
    let unsealed = disk_chain(&pod_id)?;

    info!("Hibernating pod {pod_id}, and resuming it while its disk is sealed on the node");
    // diskd seals at once; feosd has its answer only once the test
    // releases it, so that feosd is in the call when the resume comes,
    // whether diskd ended the seal by then or not.
    let id = pod_id.clone();
    let local = diskd_gate().pass_holding_answer(
        move |call| matches!(call, GateCall::Seal(seal) if seal.pod_id == id && !seal.upload),
    );
    let entries = cache_entries(&mut disks).await?;
    let hibernation = spawn_hibernation(&pods, &pod_id, "s1", false);
    timeout(STEP_TIMEOUT, local.arrived())
        .await
        .context("the local seal did not begin")?;
    // The resume comes once diskd admitted a wave of the seal's chunks,
    // with the seal under way, or once diskd ended the seal.
    let admitted = timeout(STEP_TIMEOUT, async {
        loop {
            let admitted = cache_entries(&mut disks).await?.saturating_sub(entries);
            if admitted >= SEAL_WAVE || local.answered() {
                return anyhow::Ok(admitted);
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    })
    .await
    .context("the local seal neither admitted chunks nor ended")??;
    let shown = self::hibernation(&mut pods, &pod_id).await?;
    assert_eq!(shown.phase(), HibernationPhase::Sealing);
    assert!(shown.abandonable);
    let resuming = Instant::now();
    resume(&mut pods, &pod_id, ABANDON_TIMEOUT).await?;
    let took = resuming.elapsed();
    assert_abandoned(&hibernation.await?.unwrap_err(), &pod_id);
    assert!(
        timeout(ABANDON_TIMEOUT, local.dropped()).await?,
        "feosd waited for the local seal"
    );
    let answered = local.answered();
    drop(local);
    // diskd stopped the seal before it committed the layer, or had
    // committed it: the disk opened again over its top as it was, or over
    // the sealed layer with a fresh top.
    let chain = disk_chain(&pod_id)?;
    if chain == unsealed {
        ensure!(
            !answered,
            "diskd answered the seal, and the disk is not sealed"
        );
        info!(
            "The resume took {took:?}: diskd stopped the local seal before its commit, \
             {admitted} chunks into it"
        );
    } else {
        ensure!(
            chain.len() == unsealed.len() + 1 && chain.starts_with(&unsealed),
            "the disk's chain {chain:?} after the abandon, {unsealed:?} before"
        );
        let how = if answered {
            "had sealed the disk before the abandon"
        } else {
            "had committed the layer when the seal was dropped"
        };
        info!("The resume took {took:?}: diskd {how}");
    }

    // The pod runs on, with its memory and its disk.
    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    assert_eq!(pod.hibernation, None);
    assert_eq!(pod.checkpoint, None);
    guard.set_pid(pid(&pod)?);
    let after = guest_ping(&pod_id).await?;
    assert_eq!(after.boot_id, before.boot_id, "the same boot");
    assert!(after.tick > before.tick, "the guest forgot its pings");
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));

    // The next hibernation seals what the disk has, and restores.
    info!("Hibernating pod {pod_id} again, and restoring it");
    let head_key = hibernate(&mut pods, &pod_id).await?;
    let head = disks
        .get_head(GetHeadRequest {
            key: head_key.clone(),
        })
        .await?
        .into_inner();
    assert_eq!(head.chain.len(), 2, "the chain grew: {:?}", head.chain);
    resume(&mut pods, &pod_id, STEP_TIMEOUT).await?;
    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    assert_eq!(
        pod.checkpoint
            .as_ref()
            .map(|checkpoint| &checkpoint.head_key),
        Some(&head_key)
    );
    guard.set_pid(pid(&pod)?);
    assert_eq!(guest_ping(&pod_id).await?.boot_id, before.boot_id);
    assert_eq!(records(&pod_id).await?, marked(&["keep"], &marker));

    delete(&mut pods, &pod_id, guard).await
}

/// Holds a hibernation of the pod at the first call that `matches`, has a
/// resume come meanwhile, and checks that the resume waits for the
/// hibernation and then restores the pod from its checkpoint, with its
/// memory. Returns the hibernation as GetPod showed it while it was held.
async fn resume_waits(
    pods: &mut PodServiceClient<Channel>,
    pod_id: &str,
    guard: &mut PodGuard,
    no_abandon: bool,
    matches: impl Fn(&GateCall) -> bool + Send + Sync + 'static,
) -> Result<Hibernation> {
    let before = guest_ping(pod_id).await?;
    let held = diskd_gate().hold(matches);
    let hibernation = spawn_hibernation(pods, pod_id, "w1", no_abandon);
    timeout(STEP_TIMEOUT, held.arrived())
        .await
        .context("the hibernation did not reach the call")?;
    let shown = self::hibernation(pods, pod_id).await?;
    assert!(!shown.abandonable, "{shown:?}");

    let resumed = {
        let mut pods = pods.clone();
        let pod_id = pod_id.to_string();
        tokio::spawn(async move { resume(&mut pods, &pod_id, STEP_TIMEOUT).await })
    };
    tokio::time::sleep(Duration::from_secs(2)).await;
    ensure!(!resumed.is_finished(), "the resume did not wait");
    held.release();
    let checkpoint = hibernation
        .await??
        .checkpoint
        .context("a hibernation has a checkpoint")?;
    resumed.await??;
    let pod = get_pod(pods, pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    assert_eq!(pod.hibernation, None);
    assert_eq!(pod.checkpoint.as_ref(), Some(&checkpoint), "not restored");
    guard.set_pid(pid(&pod)?);
    assert_eq!(guest_ping(pod_id).await?.boot_id, before.boot_id);
    Ok(shown)
}

#[tokio::test]
async fn test_resume_waits_for_a_drain() -> Result<()> {
    if !can_hibernate()? {
        return Ok(());
    }
    ensure_server().await;
    let _store = OBJECT_STORE.read().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let (pod_id, mut guard) = up_pod(&mut pods).await?;

    info!("Hibernating pod {pod_id} for a drain, and resuming it while its disk uploads");
    let id = pod_id.clone();
    let shown = resume_waits(
        &mut pods,
        &pod_id,
        &mut guard,
        true,
        move |call| matches!(call, GateCall::Seal(seal) if seal.pod_id == id && seal.upload),
    )
    .await?;
    assert_eq!(shown.phase(), HibernationPhase::UploadingDisk);

    delete(&mut pods, &pod_id, guard).await
}

#[tokio::test]
async fn test_resume_after_the_point_of_no_return() -> Result<()> {
    if !can_hibernate()? {
        return Ok(());
    }
    ensure_server().await;
    let _store = OBJECT_STORE.read().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let (pod_id, mut guard) = up_pod(&mut pods).await?;

    info!("Hibernating pod {pod_id}, and resuming it while its head is put");
    let id = pod_id.clone();
    let shown = resume_waits(
        &mut pods,
        &pod_id,
        &mut guard,
        false,
        move |call| matches!(call, GateCall::PutHead(head) if head.pod_id == id),
    )
    .await?;
    assert_eq!(shown.phase(), HibernationPhase::Head);

    delete(&mut pods, &pod_id, guard).await
}

#[tokio::test]
async fn test_hibernation_outlives_its_call() -> Result<()> {
    if !can_hibernate()? {
        return Ok(());
    }
    ensure_server().await;
    let _store = OBJECT_STORE.read().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let mut disks = diskd().await?;
    let (pod_id, guard) = up_pod(&mut pods).await?;

    info!("Hibernating pod {pod_id} with a call that gives up after a second");
    let id = pod_id.clone();
    let upload = diskd_gate()
        .hold(move |call| matches!(call, GateCall::Seal(seal) if seal.pod_id == id && seal.upload));
    let mut request = tonic::Request::new(HibernatePodRequest {
        pod_id: pod_id.clone(),
        ..Default::default()
    });
    request.set_timeout(Duration::from_secs(1));
    let status = pods
        .hibernate_pod(request)
        .await
        .expect_err("a hibernation that held answered within a second");
    assert!(
        matches!(status.code(), Code::DeadlineExceeded | Code::Cancelled),
        "{status:?}"
    );
    timeout(STEP_TIMEOUT, upload.arrived())
        .await
        .context("the hibernation did not go on")?;
    upload.release();
    // The pod is HIBERNATED before its hibernation ends: in LEAVING, it
    // still leaves the node.
    let pod = timeout(STEP_TIMEOUT, async {
        loop {
            let pod = get_pod(&mut pods, &pod_id).await?;
            if pod.state() == PodState::Hibernated {
                match &pod.hibernation {
                    None => return anyhow::Ok(pod),
                    Some(shown) => assert_eq!(shown.phase(), HibernationPhase::Leaving),
                }
            }
            tokio::time::sleep(Duration::from_millis(200)).await;
        }
    })
    .await
    .context("the pod did not hibernate")??;
    let checkpoint = pod
        .checkpoint
        .context("a hibernated pod has a checkpoint")?;
    assert!(has_head(&mut disks, checkpoint.head_key).await?);

    delete(&mut pods, &pod_id, guard).await
}

#[tokio::test]
async fn test_hibernate_pod_joins() -> Result<()> {
    if !can_hibernate()? {
        return Ok(());
    }
    ensure_server().await;
    let _store = OBJECT_STORE.read().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let (pod_id, guard) = up_pod(&mut pods).await?;

    info!("Hibernating pod {pod_id} with two calls");
    let id = pod_id.clone();
    let upload = diskd_gate()
        .hold(move |call| matches!(call, GateCall::Seal(seal) if seal.pod_id == id && seal.upload));
    let first = spawn_hibernation(&pods, &pod_id, "j1", false);
    timeout(STEP_TIMEOUT, upload.arrived())
        .await
        .context("the hibernation did not reach the upload")?;
    let second = spawn_hibernation(&pods, &pod_id, "j2", false);
    timeout(Duration::from_secs(10), async {
        while self::hibernation(&mut pods, &pod_id).await?.request_ids != ["j1", "j2"] {
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        anyhow::Ok(())
    })
    .await
    .context("the second call did not join")??;
    upload.release();
    let first = first.await??;
    let second = second.await??;
    assert!(first.checkpoint.is_some() && first.disk_bytes > 0);
    assert_eq!(first, second, "one hibernation, one answer");
    assert_eq!(
        get_pod(&mut pods, &pod_id).await?.state(),
        PodState::Hibernated
    );

    delete(&mut pods, &pod_id, guard).await
}
