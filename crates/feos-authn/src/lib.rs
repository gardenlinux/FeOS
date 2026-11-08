// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feos-authn: one pod's credentials on its node (ADR 0014), in a process
//! of its own that `feosd` starts with the pod's microVM. Its first part is
//! the HTTP side (ADR 0028): the pod's HTTP bindings, reached at
//! 169.254.169.1 by a name (port 80) or under the upstream's own name with
//! TLS (port 443), to which it adds the credential. The pod's identity,
//! FeID and the TLS, Kubernetes and SSH bindings follow; their sets are
//! opened and kept already.
//!
//! See `docs/architecture/authn.md` for the control protocol and the audit
//! records.

pub mod audit;
pub mod bindings;
pub mod body;
pub mod control;
pub mod dest;
pub mod gunzip;
pub mod hpke;
pub mod intercept;
pub mod jail;
pub mod keys;
pub mod oauth;
pub mod proxy;
pub mod ratelimit;
pub mod redact;
pub mod trace;
pub mod upstream;

// The test kit names the crate as its integration tests do.
#[cfg(test)]
extern crate self as feos_authn;
#[cfg(test)]
mod testkit;
#[cfg(test)]
mod tests;

pub use control::Authn;
pub use dest::Net;
