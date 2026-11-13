// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A container's /etc/hosts when its image has none.
//!
//! Images rarely carry /etc/hosts: Docker and Kubernetes write one when a
//! container starts. Without it a lookup of `localhost` goes to the pod's
//! resolver, which does not know the name, and a workload that dials
//! localhost fails. FeOS writes the usual file into the container's upper
//! layer when it creates or adopts a container whose image has none. A
//! file or link at /etc/hosts, the image's or the workload's, stays as it
//! is; the file FeOS wrote is the workload's to change.

use crate::fs::openat2_confined;
use nix::errno::Errno;
use nix::fcntl::{OFlag, ResolveFlag};
use nix::sys::stat::{mkdirat, Mode};
use std::fs::File;
use std::io::{self, Write};
use std::path::Path;

/// What FeOS writes: the loopback names, as Docker's file has them.
pub const CONTENT: &str = "\
# Written by FeOS: the image has no /etc/hosts.
127.0.0.1\tlocalhost
::1\tlocalhost ip6-localhost ip6-loopback
";

/// Writes `root`/etc/hosts with [`CONTENT`] unless an entry of that name
/// is there. /etc is resolved the way the container sees it (absolute
/// links and parent traversals stay in the root) and made when the image
/// has none; the file is created exclusively and never through a link.
pub fn provision(root: &Path) -> Result<(), String> {
    let root = File::open(root).map_err(|e| format!("{}: {e}", root.display()))?;
    let open_etc = || {
        openat2_confined(
            &root,
            Path::new("etc"),
            OFlag::O_RDONLY | OFlag::O_DIRECTORY,
            Mode::empty(),
            ResolveFlag::RESOLVE_IN_ROOT | ResolveFlag::RESOLVE_NO_MAGICLINKS,
        )
    };
    let etc = match open_etc() {
        Err(e) if e.kind() == io::ErrorKind::NotFound => {
            match mkdirat(&root, "etc", Mode::from_bits_truncate(0o755)) {
                Ok(()) | Err(Errno::EEXIST) => {}
                Err(e) => return Err(format!("mkdir /etc: {e}")),
            }
            open_etc()
        }
        result => result,
    }
    .map_err(|e| format!("/etc: {e}"))?;
    let file = match openat2_confined(
        &etc,
        Path::new("hosts"),
        OFlag::O_WRONLY | OFlag::O_CREAT | OFlag::O_EXCL | OFlag::O_NOFOLLOW,
        Mode::from_bits_truncate(0o644),
        ResolveFlag::RESOLVE_BENEATH | ResolveFlag::RESOLVE_NO_SYMLINKS,
    ) {
        Err(e) if e.kind() == io::ErrorKind::AlreadyExists => return Ok(()),
        result => result.map_err(|e| format!("/etc/hosts: {e}"))?,
    };
    File::from(file)
        .write_all(CONTENT.as_bytes())
        .map_err(|e| format!("/etc/hosts: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;
    use std::path::PathBuf;

    fn scratch(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("feos-hosts-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(dir.join("root")).unwrap();
        dir
    }

    #[test]
    fn an_image_without_hosts_gets_the_loopback_names() {
        let dir = scratch("none");
        let root = dir.join("root");
        provision(&root).unwrap();
        assert_eq!(
            std::fs::read_to_string(root.join("etc/hosts")).unwrap(),
            CONTENT
        );
        // Again (an adoption): the file is there and stays.
        std::fs::write(root.join("etc/hosts"), "10.0.0.1 db\n").unwrap();
        provision(&root).unwrap();
        assert_eq!(
            std::fs::read_to_string(root.join("etc/hosts")).unwrap(),
            "10.0.0.1 db\n"
        );
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn a_link_at_hosts_is_never_followed() {
        let dir = scratch("link");
        let root = dir.join("root");
        std::fs::create_dir_all(root.join("etc")).unwrap();
        let outside = dir.join("outside");
        symlink(&outside, root.join("etc/hosts")).unwrap();
        provision(&root).unwrap();
        assert!(!outside.exists());
        assert_eq!(std::fs::read_link(root.join("etc/hosts")).unwrap(), outside);
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn an_etc_that_links_out_stays_in_the_root() {
        let dir = scratch("etc");
        let root = dir.join("root");
        std::fs::create_dir_all(dir.join("host-etc")).unwrap();
        std::fs::create_dir_all(root.join("real-etc")).unwrap();
        // An absolute link resolves in the root, as the container sees it.
        symlink("/real-etc", root.join("etc")).unwrap();
        provision(&root).unwrap();
        assert_eq!(
            std::fs::read_to_string(root.join("real-etc/hosts")).unwrap(),
            CONTENT
        );
        assert!(!dir.join("host-etc/hosts").exists());
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
