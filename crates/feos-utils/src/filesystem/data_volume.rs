// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The data volume of a guest: the first virtio disk, in the order of the
//! microVM's drives, that does not hold the root filesystem. In a microVM
//! booted from an initramfs it is the first drive, the pod's disk; the
//! pod's volumes follow it (ADR 0016). Images and container state live on
//! it, so they survive pause and resume.
//!
//! A pod's disk starts from the guest's blank data volume, an ext4
//! filesystem of 8 GiB, and may be larger (`Resources.disk_gib`): what
//! lies beyond the filesystem reads as zeroes. The guest grows the
//! filesystem to the disk at the boot that finds it smaller, online, with
//! `EXT4_IOC_RESIZE_FS`, as the kernel does it; a disk it filled already
//! is mounted as it is.

use super::disks::{virtio_disks, Disk};
use super::volume::read_head;
use log::{info, warn};
use nix::mount::{mount, MsFlags};
use std::os::fd::AsRawFd;
use std::time::{Duration, Instant};

/// virtio-blk probes asynchronously: the disk can appear after PID 1 starts.
const PROBE_ATTEMPTS: u32 = 80;
const PROBE_INTERVAL: Duration = Duration::from_millis(25);
/// The data volume's mount options. Its disk reads zeroes where nothing
/// was written: the inode tables of the groups a grown filesystem added
/// are zeroes already, and the kernel need not write them, as on a
/// volume. The blank data volume's own are zeroed when it is made.
const MOUNT_OPTIONS: &str = "noinit_itable";

// EXT4_IOC_RESIZE_FS: grows a mounted ext4 filesystem to the given number
// of blocks.
nix::ioctl_write_ptr!(ext4_resize_fs, b'f', 16, u64);

/// Waits up to two seconds for the data volume and mounts it (ext4) at
/// `target`, grown to its disk first where the disk is larger. Returns the
/// device it mounted, or None if there is no data volume or the mount
/// failed; both are logged. A filesystem that does not grow is mounted as
/// it is, and logged.
pub fn mount_data_volume(target: &str) -> Option<String> {
    let disk = (0..PROBE_ATTEMPTS).find_map(|attempt| {
        if attempt > 0 {
            std::thread::sleep(PROBE_INTERVAL);
        }
        find_data_disk()
    });
    let Some(disk) = disk else {
        warn!("No data volume found; {target} stays in RAM");
        return None;
    };
    let device = disk.path();

    if let Err(e) = std::fs::create_dir_all(target) {
        warn!("Failed to create {target}: {e}");
    }
    let geometry = read_head(&device).ok().and_then(|head| Geometry::of(&head));
    let grow = geometry.and_then(|g| g.grown_to(disk.size_bytes));
    // A filesystem that grows is mounted with the kernel's default: with
    // noinit_itable, the resize would write every new inode table.
    let options = if grow.is_some() {
        None
    } else {
        Some(MOUNT_OPTIONS)
    };
    if let Err(e) = mount(
        Some(device.as_str()),
        target,
        Some("ext4"),
        MsFlags::empty(),
        options,
    ) {
        warn!("Failed to mount data volume {device} at {target}: {e}");
        return None;
    }
    info!("Mounted data volume {device} at {target}");
    if let (Some(geometry), Some(blocks)) = (geometry, grow) {
        grow_filesystem(target, geometry, blocks);
    }
    Some(device)
}

/// Grows the filesystem mounted at `target` to `blocks`, then mounts it
/// again with [`MOUNT_OPTIONS`]: that ends the kernel's background zeroing
/// of the new inode tables before it begins (it waits up to 5 s), which
/// the disk's zeroes make needless.
fn grow_filesystem(target: &str, geometry: Geometry, blocks: u64) {
    let started = Instant::now();
    let grown = std::fs::File::open(target)
        .map_err(|e| e.to_string())
        // SAFETY: the ioctl reads one u64 from a live reference.
        .and_then(|dir| {
            unsafe { ext4_resize_fs(dir.as_raw_fd(), &blocks) }.map_err(|e| e.to_string())
        });
    match grown {
        Ok(_) => info!(
            "Grew the data volume at {target} from {} to {} MiB in {} ms",
            (geometry.blocks * geometry.block_size) >> 20,
            (blocks * geometry.block_size) >> 20,
            started.elapsed().as_millis()
        ),
        Err(e) => warn!(
            "Failed to grow the data volume at {target} to {} MiB: {e}; it stays at {} MiB",
            (blocks * geometry.block_size) >> 20,
            (geometry.blocks * geometry.block_size) >> 20
        ),
    }
    if let Err(e) = mount(
        None::<&str>,
        target,
        None::<&str>,
        MsFlags::MS_REMOUNT,
        Some(MOUNT_OPTIONS),
    ) {
        warn!("Failed to mount the data volume at {target} again with {MOUNT_OPTIONS}: {e}");
    }
}

/// An ext4 filesystem's size, from its superblock.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Geometry {
    block_size: u64,
    blocks: u64,
}

impl Geometry {
    /// The geometry of the ext4 filesystem whose superblock is at 1024 in
    /// `head`, if there is one.
    fn of(head: &[u8]) -> Option<Self> {
        let sb = head.get(1024..2048)?;
        let u32_at = |offset: usize| {
            u32::from_le_bytes([sb[offset], sb[offset + 1], sb[offset + 2], sb[offset + 3]])
        };
        if u16::from_le_bytes([sb[0x38], sb[0x39]]) != 0xEF53 || u32_at(0x18) > 6 {
            return None;
        }
        const INCOMPAT_64BIT: u32 = 0x80;
        let high = if u32_at(0x60) & INCOMPAT_64BIT != 0 {
            u64::from(u32_at(0x150))
        } else {
            0
        };
        let blocks = high << 32 | u64::from(u32_at(0x04));
        (blocks > 0).then_some(Self {
            block_size: 1024 << u32_at(0x18),
            blocks,
        })
    }

    /// The blocks the filesystem grows to on a disk of `disk_bytes`, if
    /// the disk holds more whole blocks than the filesystem has.
    fn grown_to(self, disk_bytes: u64) -> Option<u64> {
        let blocks = disk_bytes / self.block_size;
        (blocks > self.blocks).then_some(blocks)
    }
}

/// The first virtio disk, in the order of the microVM's drives, that does
/// not back the root mount.
fn find_data_disk() -> Option<Disk> {
    let root_disk = root_major_minor().and_then(|majmin| {
        std::fs::read_link(format!("/sys/dev/block/{majmin}"))
            .ok()
            .and_then(|target| target.file_name().map(|n| n.to_string_lossy().into_owned()))
    });
    virtio_disks()
        .into_iter()
        .find(|disk| root_disk.as_deref() != Some(disk.name.as_str()))
}

/// major:minor of the device mounted at /, from /proc/self/mountinfo.
fn root_major_minor() -> Option<String> {
    let mountinfo = std::fs::read_to_string("/proc/self/mountinfo").ok()?;
    root_major_minor_from(&mountinfo)
}

fn root_major_minor_from(mountinfo: &str) -> Option<String> {
    mountinfo.lines().find_map(|line| {
        let fields: Vec<&str> = line.split(' ').collect();
        (fields.len() > 4 && fields[4] == "/").then(|| fields[2].to_string())
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn finds_the_root_mount() {
        let mountinfo = "\
22 1 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw
23 22 0:5 / /dev rw,nosuid shared:2 - devtmpfs devtmpfs rw";
        assert_eq!(root_major_minor_from(mountinfo).as_deref(), Some("252:1"));
    }

    /// A disk's start with an ext4 superblock of `blocks` blocks of
    /// 1024 << `log` bytes, with the 64bit feature or without.
    fn superblock(blocks: u64, log: u32, wide: bool) -> Vec<u8> {
        let mut head = vec![0u8; 4096];
        let sb = &mut head[1024..2048];
        sb[0x04..0x08].copy_from_slice(&(blocks as u32).to_le_bytes());
        sb[0x18..0x1C].copy_from_slice(&log.to_le_bytes());
        sb[0x38..0x3A].copy_from_slice(&0xEF53u16.to_le_bytes());
        if wide {
            sb[0x60..0x64].copy_from_slice(&0x80u32.to_le_bytes());
            sb[0x150..0x154].copy_from_slice(&((blocks >> 32) as u32).to_le_bytes());
        }
        head
    }

    #[test]
    fn the_blank_data_volume_fills_a_disk_of_its_size() {
        let blank = Geometry::of(&superblock(8 << 18, 2, true)).unwrap();
        assert_eq!(
            blank,
            Geometry {
                block_size: 4096,
                blocks: 2 << 20
            }
        );
        assert_eq!(blank.grown_to(8 << 30), None);
        // Less than a block more is nothing to grow into.
        assert_eq!(blank.grown_to((8 << 30) + 4095), None);
        assert_eq!(blank.grown_to(4 << 30), None);
    }

    #[test]
    fn a_larger_disk_grows_the_filesystem_to_its_whole_blocks() {
        let blank = Geometry::of(&superblock(8 << 18, 2, true)).unwrap();
        assert_eq!(blank.grown_to(30 << 30), Some((30 << 30) / 4096));
        assert_eq!(blank.grown_to((30 << 30) + 100), Some((30 << 30) / 4096));
        // Past 2^32 blocks of 1 KiB, the high half counts.
        let wide = Geometry::of(&superblock(5 << 30, 0, true)).unwrap();
        assert_eq!(wide.blocks, 5 << 30);
        let narrow = Geometry::of(&superblock(5 << 30, 0, false)).unwrap();
        assert_eq!(narrow.blocks, (5u64 << 30) & 0xFFFF_FFFF);
    }

    #[test]
    fn no_geometry_without_an_ext4_superblock() {
        assert_eq!(Geometry::of(&[0u8; 4096]), None);
        assert_eq!(Geometry::of(&[0u8; 1500]), None);
        let mut bad = superblock(1 << 20, 2, true);
        bad[1024 + 0x18] = 9;
        assert_eq!(Geometry::of(&bad), None);
        assert_eq!(Geometry::of(&superblock(0, 2, false)), None);
    }

    #[test]
    fn no_root_mount_in_an_unrelated_table() {
        let mountinfo = "23 22 0:5 / /dev rw,nosuid shared:2 - devtmpfs devtmpfs rw";
        assert_eq!(root_major_minor_from(mountinfo), None);
    }
}
