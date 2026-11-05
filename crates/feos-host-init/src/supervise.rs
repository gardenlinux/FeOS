// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Runs feosd and diskd, and starts each again when it ends. feosd
//! replaces its own binary in an upgrade and runs it with the same pid, so
//! the wait goes on across an upgrade. The pods' disks outlive diskd: the
//! next one takes them over. The pods' resolver's sockets and their HTTP
//! bindings' listeners outlive feosd: feos-host-init holds them and hands
//! them to every feosd it starts.

use feos_utils::listen_fds;
use log::{error, info, warn};
use std::ffi::{CStr, CString};
use std::net::{Ipv4Addr, Ipv6Addr, SocketAddrV4};
use std::os::fd::{AsFd, OwnedFd};
use std::process::{Command, Stdio};
use std::time::Duration;

/// Where the initramfs has feosd, where the package installs it.
const FEOSD: &CStr = c"/usr/sbin/feosd";
/// The pods' resolver at feosd's default `--dns-address`
/// (`dns_service::ADDRESS`), which feos-host-init does not change.
const RESOLVER: SocketAddrV4 = SocketAddrV4::new(Ipv4Addr::new(169, 254, 0, 53), 53);
/// The pods' HTTP bindings at feosd's default `--otlp-address`, the name
/// mode's port and the intercept mode's (ADR 0028).
const HTTP_BINDINGS: [SocketAddrV4; 2] = [
    SocketAddrV4::new(Ipv4Addr::new(169, 254, 169, 1), 80),
    SocketAddrV4::new(Ipv4Addr::new(169, 254, 169, 1), 443),
];
/// feosd replaces its own binary here, and serves on TCP: a node whose
/// operating system is FeOS has no package manager, and no other way in.
const FEOSD_ARGS: &[&str] = &["--self-upgrade", "--tcp", "[::]:1337"];
const RESTART_DELAY: Duration = Duration::from_secs(1);
/// Where the initramfs has diskd, where the package installs it.
const DISKD: &str = "/usr/sbin/diskd";
/// Its state in /var/lib/feos, a tmpfs here: the pods' disks are in
/// memory, as their snapshots are. It tiers to no object store.
const DISKD_ARGS: &[&str] = &["--state-dir", "/var/lib/feos/disk"];

/// feosd's arguments: the prefix delegated to the node, if any, is its
/// pods' (ADR 0010).
fn feosd_args(pod_prefix: Option<(Ipv6Addr, u8)>) -> Vec<String> {
    let mut args: Vec<String> = FEOSD_ARGS.iter().map(|arg| arg.to_string()).collect();
    // feosd refuses a prefix without room for pods, and would never run.
    if let Some((prefix, len)) = pod_prefix.filter(|(_, len)| (1..=120).contains(len)) {
        let network = u128::from(prefix) & (u128::MAX << (128 - u32::from(len)));
        args.push("--pod-prefix".to_string());
        args.push(format!("{}/{len}", Ipv6Addr::from(network)));
    }
    args
}

/// The pods' resolver's sockets, bound to `feosd`'s default `--dns-address`
/// before the node holds it, which `feosd` takes from every start to the
/// next (ADR 0010): a pod's query waits in them while `feosd` restarts.
/// Without them, `feosd` binds its own.
pub fn resolver_sockets() -> Option<(OwnedFd, OwnedFd)> {
    match listen_fds::bind_freely(RESOLVER) {
        Ok(sockets) => Some(sockets),
        Err(e) => {
            error!("Failed to bind the pods' resolver's sockets to {RESOLVER}: {e}");
            None
        }
    }
}

/// The pods' HTTP bindings' listeners, bound before the node holds their
/// address, which `feosd` takes from every start to the next (ADR 0028):
/// a connection waits in their backlog while `feosd` restarts. Without,
/// `feosd` binds its own.
pub fn http_listeners() -> Vec<OwnedFd> {
    HTTP_BINDINGS
        .iter()
        .filter_map(|address| match listen_fds::listen_freely(*address) {
            Ok(fd) => Some(fd),
            Err(e) => {
                error!("Failed to bind the pods' HTTP bindings' listener at {address}: {e}");
                None
            }
        })
        .collect()
}

pub async fn run_feosd(
    pod_prefix: Option<(Ipv6Addr, u8)>,
    resolver: Option<(OwnedFd, OwnedFd)>,
    http: Vec<OwnedFd>,
) {
    let mut argv = vec![CString::from(FEOSD)];
    argv.extend(
        feosd_args(pod_prefix)
            .into_iter()
            .filter_map(|arg| CString::new(arg).ok()),
    );
    let sockets: Vec<_> = resolver
        .iter()
        .flat_map(|(udp, tcp)| {
            [
                (udp.as_fd(), listen_fds::DNS),
                (tcp.as_fd(), listen_fds::DNS),
            ]
        })
        .chain(http.iter().map(|fd| (fd.as_fd(), listen_fds::HTTP)))
        .collect();
    loop {
        // Not a std child: the reaper owns waitpid in PID 1, and the child
        // needs the sockets and the pid it has.
        match listen_fds::spawn(FEOSD, &argv, &sockets) {
            Ok(pid) => {
                let pid = pid.as_raw();
                info!("Started feosd (pid {pid})");
                let code = feos_utils::reaper::wait_for_exit(pid).await;
                warn!("feosd (pid {pid}) exited with code {code}; starting it again");
            }
            Err(e) => error!("Failed to start {FEOSD:?}: {e}"),
        }
        tokio::time::sleep(RESTART_DELAY).await;
    }
}

/// Runs diskd, the pods' disks (ADR 0011), and starts it again when it
/// ends.
pub async fn run_diskd() {
    loop {
        // A std child: the reaper owns waitpid in PID 1.
        match Command::new(DISKD)
            .args(DISKD_ARGS)
            .stdin(Stdio::null())
            .spawn()
        {
            Ok(child) => {
                let pid = child.id() as i32;
                info!("Started diskd (pid {pid})");
                let code = feos_utils::reaper::wait_for_exit(pid).await;
                warn!("diskd (pid {pid}) exited with code {code}; starting it again");
            }
            Err(e) => error!("Failed to start {DISKD}: {e}"),
        }
        tokio::time::sleep(RESTART_DELAY).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_delegated_prefix_is_the_pods() {
        assert_eq!(feosd_args(None), FEOSD_ARGS);
        let prefix = "2001:db8:7:100::".parse().unwrap();
        let args = feosd_args(Some((prefix, 56)));
        assert_eq!(&args[3..], ["--pod-prefix", "2001:db8:7:100::/56"]);
        // Bits past the length go; a prefix without room is left out.
        let args = feosd_args(Some(("2001:db8::1".parse().unwrap(), 64)));
        assert_eq!(args.last().unwrap(), "2001:db8::/64");
        assert_eq!(feosd_args(Some((prefix, 128))), FEOSD_ARGS);
    }
}
