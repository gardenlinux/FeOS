// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feos-host-init: PID 1 when FeOS is the node's operating system. It sets
//! the machine up (mounts, the network, SR-IOV, the clock),
//! takes Ctrl-Alt-Del and the power signals over, reaps orphans, and starts
//! diskd and feosd and starts them again when they end, feosd with the pods'
//! resolver's sockets, which it holds. It serves no API and logs to the
//! kernel's log.

mod kmsg;
mod machine;
mod network;
mod ntp;
mod supervise;

use feos_utils::filesystem::{get_root_fstype, mount_virtual_filesystems, move_root};
use log::info;
use nix::unistd::execv;
use std::ffi::CString;

#[tokio::main]
async fn main() {
    if std::env::args().any(|arg| arg == "--version") && std::process::id() != 1 {
        println!(
            "feos-host-init {}",
            feos_utils::version::full_version_string()
        );
        return;
    }
    if std::process::id() != 1 {
        eprintln!("feos-host-init runs as PID 1; elsewhere, run feosd");
        std::process::exit(1);
    }

    leave_initramfs();
    kmsg::init();
    mount_virtual_filesystems();
    kmsg::lift_limit();
    info!(
        "feos-host-init {} is setting the machine up",
        feos_utils::version::full_version_string()
    );
    feos_utils::reaper::spawn();
    feos_utils::host::power::take_over_ctrl_alt_del();
    machine::take_over_power_signals();
    let pod_prefix = machine::set_up().await;
    tokio::spawn(supervise::run_diskd());
    let resolver = supervise::resolver_sockets();
    let http = supervise::http_listeners();
    tokio::spawn(supervise::run_feosd(pod_prefix, resolver, http));

    // PID 1 must never exit: the kernel panics when it does.
    std::future::pending::<()>().await;
}

/// Moves the root off the initramfs, which pivot_root(2) cannot leave, and
/// runs this binary again from the new root.
fn leave_initramfs() {
    let fstype = match get_root_fstype() {
        Ok(fstype) => fstype,
        Err(e) => {
            eprintln!("feos-host-init: Failed to get the root's filesystem: {e}");
            return;
        }
    };
    if fstype != "rootfs" {
        return;
    }
    if let Err(e) = move_root() {
        eprintln!("feos-host-init: Failed to move the root: {e}");
        return;
    }
    let argv: Vec<CString> = std::env::args()
        .filter_map(|arg| CString::new(arg).ok())
        .collect();
    let Some(path) = argv.first() else {
        return;
    };
    let Err(e) = execv(path, &argv);
    eprintln!("feos-host-init: execv failed: {e}");
}
