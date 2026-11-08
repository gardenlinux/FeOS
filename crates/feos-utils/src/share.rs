// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The share directory: what FeOS hands every container, read-only. It holds
//! the pushed resolvers and, while the host offers them, the SSH agent
//! socket and FeID's.

use std::path::Path;

/// The share directory in the guest. Its own directory on the /run tmpfs,
/// never /run/feos itself, which holds FeOS's sockets.
pub const SHARE_DIR: &str = "/run/feos/share";
/// Where a container sees the share directory.
pub const SHARE_DEST: &str = "/run/feos";
/// The SSH agent socket as a container sees it.
pub const SSH_AGENT_SOCK: &str = "/run/feos/ssh-agent.sock";
/// FeID's socket as a container sees it (ADR 0014): where FeID's
/// libraries find the pod's signer.
pub const FEID_SOCK: &str = "/run/feos/feid.sock";

/// Creates the share directory if it is missing. 0755: a container process
/// of any user has to read it, and only FeOS writes there.
pub fn make_share_dir(dir: &Path) -> std::io::Result<()> {
    use std::os::unix::fs::DirBuilderExt;
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o755)
        .create(dir)
}

/// Names the SSH agent socket in `env`, unless it names one already: in a
/// container's runtime spec and in every exec. The path is the same whether
/// or not the socket exists; ssh treats a missing socket as no agent.
pub fn set_ssh_agent_env(env: &mut Vec<String>) {
    if !env.iter().any(|e| e.starts_with("SSH_AUTH_SOCK=")) {
        env.push(format!("SSH_AUTH_SOCK={SSH_AGENT_SOCK}"));
    }
}
