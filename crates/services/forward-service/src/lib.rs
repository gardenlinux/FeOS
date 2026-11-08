// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Connections between a guest's containers and its host, over vsock:
//!
//! - PortForwardService carries a connection from the host to a TCP port on
//!   the guest's loopback, so that a served port never has to be bound on
//!   the VM's network interface.
//! - KubeService and SshService run the other way: loopback listeners and
//!   an agent socket in the guest whose connections are piped to the host
//!   (vsock ports 10002 and 10003), where jailed proxies add the
//!   credentials. The guest never holds them.
//! - FeID's socket ([`FeidSocket`]) is piped the same way, to the pod's
//!   feos-authn on vsock port 10002, which holds the pod's key and signs
//!   its TLS handshakes (ADR 0014).

mod feid;
mod kube;
mod portforward;
mod ssh;

pub use feid::{FeidSocket, FEID_PREAMBLE};
pub use kube::{KubeApiHandler, KUBE_VSOCK_PORT};
pub use portforward::PortForwardApiHandler;
pub use ssh::{SshApiHandler, SSH_VSOCK_PORT};
