// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Persistent volumes (ADR 0016): a volume outlives its pods, is attached
//! to one at a time, is formatted once, in the guest, and goes into object
//! storage with its pod and comes back with it.
//!
//! The containers' tests pull their image from the internet, as the pods
//! have a network; the format test looks at the volume's superblock from
//! the node and needs no container.

use super::diskd_gate::{diskd_gate, GateCall};
use super::fixtures::{verify_microvm_cleanup, PodGuard};
use super::pod_disk_tests::{
    assert_abandoned, can_hibernate, disk_dir, diskd, diskd_state, has_head, head_key, hibernation,
    pid, resume, spawn_hibernation, start, stop, wait_until_up,
};
use super::pod_tests::{get_pod, new_pod_id};
use super::{
    ensure_server, get_public_clients, installed_guest, OBJECT_STORE, PUBLIC_SERVER_ADDRESS,
    TEST_CONTAINER_IMAGE_REF,
};
use anyhow::{bail, Context, Result};
use feos_proto::container_service::{
    container_service_client::ContainerServiceClient, exec_stream_frame, ExecRequest,
};
use feos_proto::disk_service::{
    evict_request, EvictRequest, GetHeadRequest, OpenDiskRequest, StopDiskRequest, SweepRequest,
};
use feos_proto::pod_service::{
    pod_service_client::PodServiceClient, ContainerSpec, CreatePodRequest, DeletePodRequest,
    HibernatePodRequest, HibernationPhase, PodNetwork, PodSpec, PodState, PodVolume, Resources,
    RestorePodRequest, VolumeMount,
};
use feos_proto::volume_service::{
    volume_service_client::VolumeServiceClient, CreateVolumeRequest, DeleteVolumeRequest,
    GetVolumeRequest, Volume, VolumeState,
};
use feos_utils::filesystem::volume::{probe, read_head, Found};
use log::info;
use microvm_service::vm_dir;
use std::os::unix::fs::FileExt;
use std::time::Duration;
use tokio::time::timeout;
use tonic::transport::Channel;

/// The volumes' size in these tests.
const SIZE: u64 = 1 << 30;

async fn volumes() -> Result<VolumeServiceClient<Channel>> {
    Ok(VolumeServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?)
}

async fn create_volume(volumes: &mut VolumeServiceClient<Channel>) -> Result<Volume> {
    let volume = volumes
        .create_volume(CreateVolumeRequest {
            volume_id: uuid::Uuid::new_v4().to_string(),
            size_bytes: SIZE,
            labels: [("test".to_string(), "volumes".to_string())].into(),
        })
        .await?
        .into_inner();
    assert_eq!(volume.state(), VolumeState::Available);
    assert!(!volume.formatted);
    Ok(volume)
}

async fn get_volume(volumes: &mut VolumeServiceClient<Channel>, volume_id: &str) -> Result<Volume> {
    Ok(volumes
        .get_volume(GetVolumeRequest {
            volume_id: volume_id.to_string(),
        })
        .await?
        .into_inner())
}

/// A pod with the volume as `data`; with a container that mounts it at
/// /data, and read-only at /ro, if `container`, and a network for its
/// pull.
fn spec(volume_id: &str, container: bool) -> PodSpec {
    let containers = if container {
        vec![ContainerSpec {
            name: "main".to_string(),
            image_ref: TEST_CONTAINER_IMAGE_REF.clone(),
            command: vec!["sleep".to_string(), "3600".to_string()],
            volume_mounts: vec![
                VolumeMount {
                    volume: "data".to_string(),
                    path: "/data".to_string(),
                    read_only: false,
                },
                VolumeMount {
                    volume: "data".to_string(),
                    path: "/ro".to_string(),
                    read_only: true,
                },
            ],
            ..Default::default()
        }]
    } else {
        vec![]
    };
    PodSpec {
        resources: Some(Resources {
            vcpus: 1,
            memory_mib: 256,
        }),
        containers,
        network: (!container).then(|| PodNetwork {
            none: true,
            ..Default::default()
        }),
        volumes: vec![PodVolume {
            name: "data".to_string(),
            volume_id: volume_id.to_string(),
        }],
        ..Default::default()
    }
}

async fn create_pod(
    pods: &mut PodServiceClient<Channel>,
    volume_id: &str,
    container: bool,
) -> Result<(String, PodGuard)> {
    let pod_id = new_pod_id();
    pods.create_pod(CreatePodRequest {
        pod_id: pod_id.clone(),
        spec: Some(spec(volume_id, container)),
        image_layers: None,
    })
    .await?;
    let mut guard = PodGuard::new(pod_id.clone());
    let pod = wait_until_up(pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    Ok((pod_id, guard))
}

/// Runs `script` with sh in the pod's container: its exit code, stdout and
/// stderr.
async fn sh(pod_id: &str, script: &str) -> Result<(i32, String, String)> {
    let mut containers = ContainerServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    let mut request = tonic::Request::new(ExecRequest {
        container_id: "main".to_string(),
        command: vec!["sh".to_string(), "-c".to_string(), script.to_string()],
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("feos-pod-id", pod_id.parse().expect("a pod ID is ASCII"));
    let mut frames = containers.exec(request).await?.into_inner();
    let (mut stdout, mut stderr, mut code) = (Vec::new(), Vec::new(), None);
    while let Some(frame) = frames.message().await? {
        match frame.frame {
            Some(exec_stream_frame::Frame::Stdout(bytes)) => stdout.extend(bytes),
            Some(exec_stream_frame::Frame::Stderr(bytes)) => stderr.extend(bytes),
            Some(exec_stream_frame::Frame::ExitCode(exit)) => code = Some(exit),
            None => {}
        }
    }
    Ok((
        code.context("the exec ended without an exit code")?,
        String::from_utf8_lossy(&stdout).into_owned(),
        String::from_utf8_lossy(&stderr).into_owned(),
    ))
}

async fn delete_pod(pods: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<()> {
    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.to_string(),
    })
    .await?;
    verify_microvm_cleanup(pod_id);
    Ok(())
}

async fn delete_volume(volumes: &mut VolumeServiceClient<Channel>, volume_id: &str) -> Result<()> {
    volumes
        .delete_volume(DeleteVolumeRequest {
            volume_id: volume_id.to_string(),
        })
        .await?;
    assert!(
        !disk_dir(&format!("volume-{volume_id}")).exists(),
        "the volume's disk is gone with it"
    );
    Ok(())
}

#[tokio::test]
async fn test_volume_outlives_its_pods() -> Result<()> {
    if installed_guest()?.is_none() {
        return Ok(());
    }
    ensure_server().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let mut volumes = volumes().await?;
    let volume = create_volume(&mut volumes).await?;
    let volume_id = volume.volume_id.clone();

    let (first, mut guard) = create_pod(&mut pods, &volume_id, true).await?;
    let attached = get_volume(&mut volumes, &volume_id).await?;
    assert_eq!(attached.state(), VolumeState::Attached);
    assert_eq!(attached.pod_id, first);
    assert!(attached.formatted, "the guest made its filesystem");

    let (code, stdout, stderr) = sh(
        &first,
        "echo kept > /data/file && cat /ro/file && df -k /data | tail -1",
    )
    .await?;
    assert_eq!(code, 0, "{stderr}");
    assert!(stdout.starts_with("kept\n"), "{stdout}");
    info!(
        "The volume in the container: {}",
        stdout.lines().last().unwrap_or("")
    );

    // Read-only is read-only.
    let (code, _, stderr) = sh(&first, "touch /ro/other").await?;
    assert_ne!(code, 0, "a read-only mount took a write");
    assert!(stderr.contains("Read-only"), "{stderr}");

    // ReadWriteOnce: no second pod, and no delete while a pod has it.
    let status = pods
        .create_pod(CreatePodRequest {
            pod_id: new_pod_id(),
            spec: Some(spec(&volume_id, true)),
            image_layers: None,
        })
        .await
        .expect_err("a second pod with an attached volume");
    assert_eq!(status.code(), tonic::Code::FailedPrecondition);
    assert!(status.message().contains(&first), "{}", status.message());
    let status = volumes
        .delete_volume(DeleteVolumeRequest {
            volume_id: volume_id.clone(),
        })
        .await
        .expect_err("a delete of an attached volume");
    assert_eq!(status.code(), tonic::Code::FailedPrecondition);

    info!("Stopping and starting pod {first}");
    stop(&mut pods, &first).await?;
    let stats = diskd()
        .await?
        .stats(feos_proto::disk_service::StatsRequest {
            pod_id: format!("volume-{volume_id}"),
        })
        .await?
        .into_inner();
    assert!(
        stats.devices.is_empty(),
        "the volume's disk stops with the pod"
    );
    let pod = start(&mut pods, &first).await?;
    guard.set_pid(pid(&pod)?);
    let (code, stdout, stderr) = sh(&first, "cat /data/file").await?;
    assert_eq!((code, stdout.as_str()), (0, "kept\n"), "{stderr}");

    info!("Deleting pod {first}; the volume stays");
    delete_pod(&mut pods, &first).await?;
    guard.disable_cleanup();
    let free = get_volume(&mut volumes, &volume_id).await?;
    assert_eq!(free.state(), VolumeState::Available);
    assert!(free.pod_id.is_empty() && free.formatted);

    let (second, mut guard) = create_pod(&mut pods, &volume_id, true).await?;
    let (code, stdout, stderr) = sh(&second, "cat /data/file").await?;
    assert_eq!(
        (code, stdout.as_str()),
        (0, "kept\n"),
        "another pod finds no data: {stderr}"
    );
    assert_eq!(get_volume(&mut volumes, &volume_id).await?.pod_id, second);
    info!("Pod {second} has what pod {first} wrote");

    delete_pod(&mut pods, &second).await?;
    guard.disable_cleanup();
    delete_volume(&mut volumes, &volume_id).await?;
    let gone = volumes
        .get_volume(GetVolumeRequest {
            volume_id: volume_id.clone(),
        })
        .await
        .expect_err("a deleted volume");
    assert_eq!(gone.code(), tonic::Code::NotFound);
    Ok(())
}

/// The superblock of the volume's filesystem, read from the node: what the
/// probe finds and the mount count.
async fn superblock(volume_id: &str) -> Result<(Found, u16)> {
    let disk = format!("volume-{volume_id}");
    let mut disks = diskd().await?;
    let device = disks
        .open_disk(OpenDiskRequest {
            pod_id: disk.clone(),
            head_key: String::new(),
        })
        .await?
        .into_inner();
    let head = read_head(&device.dev_path);
    disks.stop_disk(StopDiskRequest { pod_id: disk }).await?;
    let head = head?;
    let mounts = u16::from_le_bytes([head[1024 + 0x34], head[1024 + 0x35]]);
    Ok((probe(&head), mounts))
}

/// Zeroes the start of the volume's disk, its superblock with it.
async fn wipe(volume_id: &str) -> Result<()> {
    let disk = format!("volume-{volume_id}");
    let mut disks = diskd().await?;
    let device = disks
        .open_disk(OpenDiskRequest {
            pod_id: disk.clone(),
            head_key: String::new(),
        })
        .await?
        .into_inner();
    let written = std::fs::OpenOptions::new()
        .write(true)
        .open(&device.dev_path)
        .and_then(|file| {
            file.write_all_at(&[0u8; 1 << 17], 0)?;
            file.sync_all()
        });
    disks.stop_disk(StopDiskRequest { pod_id: disk }).await?;
    Ok(written?)
}

#[tokio::test]
async fn test_volume_is_formatted_once() -> Result<()> {
    if installed_guest()?.is_none() {
        return Ok(());
    }
    ensure_server().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let mut volumes = volumes().await?;
    let volume = create_volume(&mut volumes).await?;
    let volume_id = volume.volume_id.clone();

    let (pod_id, mut guard) = create_pod(&mut pods, &volume_id, false).await?;
    assert!(get_volume(&mut volumes, &volume_id).await?.formatted);
    // The guest ordered its drives by their MMIO windows.
    let serial = std::fs::read_to_string(vm_dir(&pod_id).join("serial.log"))?;
    let drives = serial
        .lines()
        .find(|line| line.contains("The drives, in order"))
        .context("the guest logged no drives")?;
    info!("{drives}");
    assert!(
        drives.contains("vda at 0x") && drives.contains("vdb at 0x"),
        "{drives}"
    );
    stop(&mut pods, &pod_id).await?;
    // The guest made ext4 with the volume's ID, and mounted it once.
    let (found, mounts) = superblock(&volume_id).await?;
    assert_eq!(
        found,
        Found::Ext4 {
            uuid: volume_id.clone(),
            mounted: true
        }
    );
    assert_eq!(mounts, 1);
    // mke2fs wrote a few MiB of the GiB.
    let top = std::fs::metadata(disk_dir(&format!("volume-{volume_id}")).join("top.img"))?;
    let written = std::os::unix::fs::MetadataExt::blocks(&top) * 512;
    info!(
        "The new filesystem took {} KiB of the volume's top",
        written >> 10
    );
    assert!(written < 64 << 20, "{written} bytes for a new filesystem");

    // The next boot mounts it, and makes nothing.
    let pod = start(&mut pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    stop(&mut pods, &pod_id).await?;
    let (found, mounts) = superblock(&volume_id).await?;
    assert!(matches!(found, Found::Ext4 { mounted: true, .. }));
    assert_eq!(mounts, 2, "the filesystem was made anew");

    // A volume that has its filesystem is never formatted again: one
    // whose disk lost it fails its pod, and stays as it is.
    wipe(&volume_id).await?;
    pods.start_pod(feos_proto::pod_service::StartPodRequest {
        pod_id: pod_id.clone(),
    })
    .await?;
    let failed = timeout(Duration::from_secs(120), async {
        loop {
            let pod = get_pod(&mut pods, &pod_id).await?;
            match pod.state() {
                PodState::Failed => return anyhow::Ok(pod),
                PodState::Running => bail!("a pod came up on a volume without its filesystem"),
                _ => tokio::time::sleep(Duration::from_millis(200)).await,
            }
        }
    })
    .await
    .context("the pod did not fail")??;
    info!("Pod {pod_id} failed: {}", failed.reason);
    assert!(failed.reason.contains("blank"), "{}", failed.reason);
    stop(&mut pods, &pod_id).await?;
    let (found, _) = superblock(&volume_id).await?;
    assert_eq!(found, Found::Blank, "the guest formatted a volume");

    delete_pod(&mut pods, &pod_id).await?;
    guard.disable_cleanup();
    delete_volume(&mut volumes, &volume_id).await?;
    Ok(())
}

#[tokio::test]
async fn test_volume_hibernates_with_its_pod() -> Result<()> {
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
    let mut volumes = volumes().await?;
    let mut disks = diskd().await?;
    let volume = create_volume(&mut volumes).await?;
    let volume_id = volume.volume_id.clone();
    let volume_disk = format!("volume-{volume_id}");

    let (pod_id, mut guard) = create_pod(&mut pods, &volume_id, true).await?;
    let (code, _, stderr) = sh(&pod_id, "echo before > /data/file && sync").await?;
    assert_eq!(code, 0, "{stderr}");

    info!("Hibernating pod {pod_id} with its volume");
    let checkpoint = pods
        .hibernate_pod(HibernatePodRequest {
            pod_id: pod_id.clone(),
            ..Default::default()
        })
        .await?
        .into_inner()
        .checkpoint
        .context("a hibernation has a checkpoint")?;
    let head = disks
        .get_head(GetHeadRequest {
            key: checkpoint.head_key.clone(),
        })
        .await?
        .into_inner();
    let volume_head = head
        .volume_heads
        .first()
        .context("the pod's head names its volume's")?
        .clone();
    assert!(
        volume_head.starts_with(&format!("snapshots/{volume_disk}/")),
        "{volume_head}"
    );
    let checkpointed = get_volume(&mut volumes, &volume_id).await?;
    assert_eq!(checkpointed.state(), VolumeState::Checkpointed);
    assert_eq!(checkpointed.pod_id, pod_id);
    assert_eq!(
        checkpointed
            .checkpoint
            .as_ref()
            .map(|c| c.head_key.as_str()),
        Some(volume_head.as_str())
    );
    assert!(
        !disk_dir(&volume_disk).exists(),
        "the volume's disk left the node"
    );
    // A sweep that keeps the pod's head keeps its volume's.
    let report = disks
        .sweep(SweepRequest {
            live_heads: vec![checkpoint.head_key.clone()],
            dry_run: true,
            no_grace: true,
            ..Default::default()
        })
        .await?
        .into_inner();
    for key in [&checkpoint.head_key, &volume_head] {
        assert!(
            !report.deleted_keys.contains(key),
            "a sweep would delete {key}"
        );
    }
    // The hibernated pod still has its volume.
    let status = pods
        .create_pod(CreatePodRequest {
            pod_id: new_pod_id(),
            spec: Some(spec(&volume_id, true)),
            image_layers: None,
        })
        .await
        .expect_err("another pod with a hibernated pod's volume");
    assert_eq!(status.code(), tonic::Code::FailedPrecondition);

    // Another node: one that has neither the pod nor the volume, nor
    // their own layers in its cache.
    info!("Forgetting pod {pod_id}, its volume, and what diskd has of them");
    let volume_layer = disks
        .get_head(GetHeadRequest {
            key: volume_head.clone(),
        })
        .await?
        .into_inner()
        .chain
        .last()
        .cloned()
        .context("the volume has a chain")?;
    delete_pod(&mut pods, &pod_id).await?;
    let free = get_volume(&mut volumes, &volume_id).await?;
    assert_eq!(free.state(), VolumeState::Checkpointed);
    assert!(free.pod_id.is_empty());
    delete_volume(&mut volumes, &volume_id).await?;
    disks
        .evict(EvictRequest {
            target: evict_request::Target::All as i32,
            force: false,
        })
        .await?;
    let layers = diskd_state().join("layers");
    for layer in [&head.chain[1], &volume_layer] {
        std::fs::remove_file(layers.join(format!("{layer}.manifest")))?;
    }

    info!("Restoring pod {pod_id} from {}", checkpoint.head_key);
    let restored = pods
        .restore_pod(RestorePodRequest {
            pod_id: pod_id.clone(),
            head_key: checkpoint.head_key.clone(),
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
    guard.set_pid(pid(&pod)?);
    let back = get_volume(&mut volumes, &volume_id).await?;
    assert_eq!(back.state(), VolumeState::Attached);
    assert_eq!(back.pod_id, pod_id);
    assert_eq!(back.size_bytes, SIZE);
    assert!(back.formatted);
    assert_eq!(back.labels, volume.labels);
    let (code, stdout, stderr) = sh(
        &pod_id,
        "cat /data/file && echo after >> /data/file && sync",
    )
    .await?;
    assert_eq!((code, stdout.as_str()), (0, "before\n"), "{stderr}");
    assert!(layers.join(format!("{volume_layer}.manifest")).is_file());

    // Without the memory: a boot mounts the volume from its checkpoint.
    info!("Hibernating pod {pod_id} again, and restoring it without its memory");
    let again = pods
        .hibernate_pod(HibernatePodRequest {
            pod_id: pod_id.clone(),
            ..Default::default()
        })
        .await?
        .into_inner()
        .checkpoint
        .context("a hibernation has a checkpoint")?;
    assert!(again.epoch > checkpoint.epoch);
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
    let (code, stdout, stderr) = sh(&pod_id, "cat /data/file").await?;
    assert_eq!((code, stdout.as_str()), (0, "before\nafter\n"), "{stderr}");

    delete_pod(&mut pods, &pod_id).await?;
    guard.disable_cleanup();
    delete_volume(&mut volumes, &volume_id).await?;
    Ok(())
}

#[tokio::test]
async fn test_resume_abandons_a_volume_upload() -> Result<()> {
    if !can_hibernate()? {
        return Ok(());
    }
    ensure_server().await;
    let _store = OBJECT_STORE.read().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let mut volumes = volumes().await?;
    let mut disks = diskd().await?;
    let volume = create_volume(&mut volumes).await?;
    let volume_id = volume.volume_id.clone();
    let volume_disk = format!("volume-{volume_id}");

    let (pod_id, mut guard) = create_pod(&mut pods, &volume_id, true).await?;
    let (code, _, stderr) = sh(&pod_id, "echo before > /data/file && sync").await?;
    assert_eq!(code, 0, "{stderr}");

    info!("Hibernating pod {pod_id}, and resuming it while its volume uploads");
    let disk = volume_disk.clone();
    let upload = diskd_gate().hold(
        move |call| matches!(call, GateCall::Seal(seal) if seal.pod_id == disk && seal.upload),
    );
    let hibernating = spawn_hibernation(&pods, &pod_id, "v1", false);
    timeout(Duration::from_secs(120), upload.arrived())
        .await
        .context("the volume's upload did not begin")?;
    let shown = hibernation(&mut pods, &pod_id).await?;
    assert_eq!(shown.phase(), HibernationPhase::Volumes);
    assert!(shown.abandonable);
    resume(&mut pods, &pod_id, Duration::from_secs(30)).await?;
    assert_abandoned(&hibernating.await?.unwrap_err(), &pod_id);
    assert!(
        timeout(Duration::from_secs(30), upload.dropped()).await?,
        "the held call was not dropped"
    );
    // The volume stays the pod's, with no head of its own.
    let attached = get_volume(&mut volumes, &volume_id).await?;
    assert_eq!(attached.state(), VolumeState::Attached);
    assert_eq!(attached.pod_id, pod_id);
    assert_eq!(attached.checkpoint, None);
    assert!(!has_head(&mut disks, head_key(&volume_disk, shown.epoch)).await?);
    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    guard.set_pid(pid(&pod)?);
    // Its disk opened again over its sealed layer.
    let (code, stdout, stderr) = sh(
        &pod_id,
        "cat /data/file && echo after >> /data/file && sync",
    )
    .await?;
    assert_eq!((code, stdout.as_str()), (0, "before\n"), "{stderr}");

    // The volumes' heads are past the point of no return.
    info!("Hibernating pod {pod_id} again, and holding its volume's head");
    let disk = volume_disk.clone();
    let head = diskd_gate()
        .hold(move |call| matches!(call, GateCall::PutHead(head) if head.pod_id == disk));
    let hibernating = spawn_hibernation(&pods, &pod_id, "v2", false);
    timeout(Duration::from_secs(120), head.arrived())
        .await
        .context("the volume's head was not put")?;
    let shown = hibernation(&mut pods, &pod_id).await?;
    assert_eq!(shown.phase(), HibernationPhase::Head);
    assert!(!shown.abandonable);
    head.release();
    let checkpoint = hibernating
        .await??
        .checkpoint
        .context("a hibernation has a checkpoint")?;
    let checkpointed = get_volume(&mut volumes, &volume_id).await?;
    assert_eq!(checkpointed.state(), VolumeState::Checkpointed);
    assert_eq!(
        checkpointed.checkpoint.map(|c| c.head_key),
        Some(head_key(&volume_disk, checkpoint.epoch))
    );

    resume(&mut pods, &pod_id, Duration::from_secs(120)).await?;
    let pod = get_pod(&mut pods, &pod_id).await?;
    assert_eq!(pod.state(), PodState::Running);
    guard.set_pid(pid(&pod)?);
    let (code, stdout, stderr) = sh(&pod_id, "cat /data/file").await?;
    assert_eq!((code, stdout.as_str()), (0, "before\nafter\n"), "{stderr}");

    delete_pod(&mut pods, &pod_id).await?;
    guard.disable_cleanup();
    delete_volume(&mut volumes, &volume_id).await?;
    Ok(())
}
