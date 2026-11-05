// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feos-authn: one pod's credentials on its node (ADR 0014, ADR 0028),
//! started by `feosd` with the pod's microVM:
//!
//!     feos-authn --pod-id <id> --audit-fd 3 --control-fd 4
//!
//! fd 3 is the pod's audit file, open for appending; fd 4 the listening
//! `SOCK_SEQPACKET` control socket. It logs to stderr.

use std::os::fd::{FromRawFd, OwnedFd};
use std::process::ExitCode;
use std::sync::Arc;

use clap::Parser;
use feos_authn::audit::Audit;
use feos_authn::dest::SystemResolver;
use feos_authn::keys::Keys;
use feos_authn::{jail, Authn, Net};

#[derive(Parser)]
#[command(
    name = "feos-authn",
    version,
    about = "A pod's credentials on its node"
)]
struct Args {
    /// The pod's ID as the control plane writes it: a short ID, or a UUID
    /// in lowercase hyphenated form.
    #[arg(long)]
    pod_id: String,
    /// The audit file, open for appending.
    #[arg(long, default_value_t = 3)]
    audit_fd: i32,
    /// The listening SOCK_SEQPACKET control socket.
    #[arg(long, default_value_t = 4)]
    control_fd: i32,
    /// Skip the seccomp filter, rlimits and the dropping of capabilities:
    /// for debugging only.
    #[arg(long, hide = true)]
    no_jail: bool,
}

fn main() -> ExitCode {
    let args = Args::parse();
    env_logger::Builder::from_env(env_logger::Env::default().default_filter_or("info"))
        .write_style(env_logger::WriteStyle::Never)
        .format_target(false)
        .init();
    if args.audit_fd < 3 || args.control_fd < 3 || args.audit_fd == args.control_fd {
        log::error!("--audit-fd and --control-fd must be two descriptors above stderr");
        return ExitCode::FAILURE;
    }
    // Before anything else may open a descriptor of the same number.
    let fds_ok = [args.audit_fd, args.control_fd]
        .iter()
        // SAFETY: F_GETFD only asks whether the number is open.
        .all(|&fd| unsafe { libc::fcntl(fd, libc::F_GETFD) } >= 0);
    if !fds_ok {
        log::error!("fd {} or {} is not open", args.audit_fd, args.control_fd);
        return ExitCode::FAILURE;
    }
    if let Err(e) = jail::close_other_fds(&[args.audit_fd, args.control_fd]) {
        log::error!("close inherited descriptors: {e}");
        return ExitCode::FAILURE;
    }
    // SAFETY: feosd handed these descriptors to this process, which owns
    // them from here on; they were checked to be open.
    let (audit, control) = unsafe {
        (
            OwnedFd::from_raw_fd(args.audit_fd),
            OwnedFd::from_raw_fd(args.control_fd),
        )
    };
    let keys = match Keys::generate() {
        Ok(k) => k,
        Err(e) => {
            log::error!("{e}");
            return ExitCode::FAILURE;
        }
    };
    // The runtime's threads and descriptors (its reactor, its signal
    // driver) exist before the filter, which then covers all threads.
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .max_blocking_threads(8)
        .enable_all()
        .build()
    {
        Ok(r) => r,
        Err(e) => {
            log::error!("start the runtime: {e}");
            return ExitCode::FAILURE;
        }
    };
    if !args.no_jail
        && let Err(e) = jail::enter()
    {
        log::error!("enter the jail: {e}");
        return ExitCode::FAILURE;
    }
    let pod_id = args.pod_id;
    log::info!("feos-authn {} for pod {pod_id}", env!("CARGO_PKG_VERSION"));
    runtime.block_on(async move {
        let net = Arc::new(Net::new(Arc::new(SystemResolver)));
        let authn = Arc::new(Authn::new(pod_id, keys, Arc::new(Audit::new(audit)), net));
        match authn.run(control).await {
            Ok(()) => ExitCode::SUCCESS,
            Err(e) => {
                log::error!("the control socket: {e}");
                ExitCode::FAILURE
            }
        }
    })
}
