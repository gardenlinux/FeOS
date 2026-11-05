// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pod's volumes in its guest (ADR 0016): `feos.guest.v1.GuestService`
//! `MountVolumes`, which feosd calls once the guest attached and before it
//! creates the containers. A volume that fails is an answer, never a
//! panic: this runs in PID 1. And the pod's interception CAs (ADR 0028),
//! `SetInterceptCas`, which feosd pushes before it creates the containers
//! too (`container_service::runtime::intercept`).

use container_service::runtime::intercept;
use feos_proto::guest_service::guest_service_server::GuestService;
use feos_proto::guest_service::{
    InterceptFailure, MountVolumesRequest, MountVolumesResponse, MountedVolume,
    SetInterceptCasRequest, SetInterceptCasResponse, Volume,
};
use feos_utils::filesystem::disks::{virtio_disks, Disk};
use feos_utils::filesystem::volume::{self, Plan};
use feos_utils::filesystem::CONTAINERS_DIR;
use feos_utils::share::SHARE_DIR;
use log::{info, warn};
use std::path::Path;
use std::time::Duration;
use tokio::sync::Mutex;
use tokio::time::{sleep, Instant};
use tonic::{Request, Response, Status};

/// How long the guest waits for a drive to show up: virtio-blk may probe
/// after PID 1 started.
const DISK_WAIT: Duration = Duration::from_secs(5);

#[derive(Default)]
pub(crate) struct GuestApi {
    /// One call at a time: two would format one disk twice.
    mounting: Mutex<()>,
}

#[tonic::async_trait]
impl GuestService for GuestApi {
    async fn mount_volumes(
        &self,
        request: Request<MountVolumesRequest>,
    ) -> Result<Response<MountVolumesResponse>, Status> {
        let volumes = request.into_inner().volumes;
        let _mounting = self.mounting.lock().await;
        let drives = volumes.iter().map(|v| v.drive as usize + 1).max();
        let disks = wait_for_disks(drives.unwrap_or(0)).await;
        let found: Vec<String> = disks
            .iter()
            .map(|disk| match disk.window {
                Some(window) => format!(
                    "{} at {window:#x}, {} MiB",
                    disk.name,
                    disk.size_bytes >> 20
                ),
                None => format!("{}, {} MiB", disk.name, disk.size_bytes >> 20),
            })
            .collect();
        info!("Volumes: The drives, in order: {}", found.join("; "));
        let mut mounted = Vec::with_capacity(volumes.len());
        let mut seen = std::collections::HashSet::new();
        for volume in &volumes {
            let result = if !seen.insert(volume.drive) || !seen_name(&volumes, volume) {
                Err("another volume of the call has its name or its drive".to_string())
            } else {
                mount(volume, &disks).await
            };
            let answer = match result {
                Ok((device, formatted)) => MountedVolume {
                    name: volume.name.clone(),
                    device,
                    formatted,
                    error: String::new(),
                },
                Err(error) => {
                    warn!("Volumes: Volume {} is not mounted: {error}", volume.name);
                    MountedVolume {
                        name: volume.name.clone(),
                        error,
                        ..Default::default()
                    }
                }
            };
            mounted.push(answer);
        }
        Ok(Response::new(MountVolumesResponse { volumes: mounted }))
    }

    async fn set_intercept_cas(
        &self,
        request: Request<SetInterceptCasRequest>,
    ) -> Result<Response<SetInterceptCasResponse>, Status> {
        let cas = request.into_inner().ca_pems;
        let count = cas.len();
        let failed = tokio::task::spawn_blocking(move || {
            intercept::publish(Path::new(SHARE_DIR), Path::new(CONTAINERS_DIR), &cas)
        })
        .await
        .map_err(|e| Status::internal(format!("the CAs' task: {e}")))?
        .map_err(|e| Status::internal(format!("writing the interception CAs: {e}")))?;
        for (container, reason) in &failed {
            warn!("Intercept: Container {container} keeps the CAs it had: {reason}");
        }
        info!("Intercept: The pod has {count} interception CAs");
        Ok(Response::new(SetInterceptCasResponse {
            failed: failed
                .into_iter()
                .map(|(container_id, reason)| InterceptFailure {
                    container_id,
                    reason,
                })
                .collect(),
        }))
    }
}

/// Whether `volume` is the only one of `volumes` with its name.
fn seen_name(volumes: &[Volume], volume: &Volume) -> bool {
    volumes.iter().filter(|v| v.name == volume.name).count() == 1
}

/// The microVM's virtio disks, once there are `count` of them, or what
/// there is after a while.
async fn wait_for_disks(count: usize) -> Vec<Disk> {
    let deadline = Instant::now() + DISK_WAIT;
    loop {
        let disks = tokio::task::spawn_blocking(virtio_disks)
            .await
            .unwrap_or_default();
        if disks.len() >= count || Instant::now() >= deadline {
            return disks;
        }
        sleep(Duration::from_millis(50)).await;
    }
}

/// Mounts one volume, formatting it if the host says it is new and the
/// disk allows it. Returns its device and whether it was formatted now.
async fn mount(volume: &Volume, disks: &[Disk]) -> Result<(String, bool), String> {
    if !volume::valid_name(&volume.name) {
        return Err(format!("invalid volume name {:?}", volume.name));
    }
    if !valid_uuid(&volume.volume_id) {
        return Err(format!("volume ID {:?} is not a UUID", volume.volume_id));
    }
    // Drive 0 is the pod's disk, mounted at /var/lib/feos.
    if volume.drive == 0 {
        return Err("drive 0 is the pod's disk".to_string());
    }
    let disk = disks.get(volume.drive as usize).ok_or_else(|| {
        format!(
            "the microVM has no drive {}: {} virtio disks",
            volume.drive,
            disks.len()
        )
    })?;
    let device = disk.path();
    if disk.size_bytes != volume.size_bytes {
        return Err(format!(
            "{device}, drive {}, has {} bytes, the volume {}",
            volume.drive, disk.size_bytes, volume.size_bytes
        ));
    }
    let target = volume::mount_point(&volume.name);
    let head = {
        let device = device.clone();
        tokio::task::spawn_blocking(move || {
            volume::read_head(&device).map_err(|e| format!("read {device}: {e}"))
        })
        .await
        .map_err(|e| e.to_string())??
    };
    let found = volume::probe(&head);
    if volume::mounted_from(&target, &device) {
        // Mounted by an earlier call: the same volume?
        volume::plan(&found, &volume.volume_id, false)?;
        if !volume.format {
            return Ok((device, false));
        }
        // Whose confirmation the host did not get: made again.
        volume::unmount_volume(&target)?;
    }
    if volume::is_mount_point(&target) {
        return Err(format!(
            "{} has another filesystem mounted",
            target.display()
        ));
    }
    let formatted = match volume::plan(&found, &volume.volume_id, volume.format)? {
        Plan::Format => {
            info!(
                "Volumes: Formatting volume {} ({}) on {device}",
                volume.name, volume.volume_id
            );
            volume::make_filesystem(&device, &volume.volume_id).await?;
            true
        }
        Plan::Mount => false,
    };
    volume::mount_volume(&device, &target)?;
    info!(
        "Volumes: Mounted volume {} ({}) from {device} at {}",
        volume.name,
        volume.volume_id,
        target.display()
    );
    Ok((device, formatted))
}

/// Whether `id` is a UUID in its text form, which mke2fs takes.
fn valid_uuid(id: &str) -> bool {
    let groups: Vec<&str> = id.split('-').collect();
    groups.iter().map(|g| g.len()).eq([8, 4, 4, 4, 12])
        && groups
            .iter()
            .all(|g| g.chars().all(|c| c.is_ascii_hexdigit()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn volume_ids_are_uuids() {
        assert!(valid_uuid("11111111-2222-3333-4444-55555555aaAA"));
        assert!(!valid_uuid("11111111-2222-3333-4444-5555555555"));
        assert!(!valid_uuid("11111111222233334444555555555555"));
        assert!(!valid_uuid("-o x"));
        assert!(!valid_uuid(""));
    }

    #[tokio::test]
    async fn a_volume_without_its_drive_is_an_answer() {
        let volume = Volume {
            name: "db".to_string(),
            volume_id: "11111111-2222-3333-4444-555555555555".to_string(),
            drive: 1,
            size_bytes: 1 << 30,
            format: true,
        };
        let disks = [Disk {
            name: "vda".to_string(),
            size_bytes: 8 << 30,
            window: None,
        }];
        let error = mount(&volume, &disks).await.unwrap_err();
        assert!(error.contains("no drive 1"), "{error}");
        let error = mount(
            &Volume {
                drive: 0,
                ..volume.clone()
            },
            &disks,
        )
        .await
        .unwrap_err();
        assert!(error.contains("pod's disk"), "{error}");
        let error = mount(
            &Volume {
                name: "../x".to_string(),
                ..volume
            },
            &disks,
        )
        .await
        .unwrap_err();
        assert!(error.contains("invalid volume name"), "{error}");
    }
}
