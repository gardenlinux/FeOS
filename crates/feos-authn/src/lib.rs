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
pub mod dest;
pub mod hpke;
pub mod jail;
pub mod keys;
pub mod ratelimit;
pub mod trace;

pub use dest::Net;
