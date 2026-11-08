// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The error codes, which are stable within a version and namespaced, and
//! the two error types: [`Error`], what a client gets, and [`Refusal`],
//! what a server's handler answers with.
//!
//! A refusal is retried only when its frame says `"retriable":true`. A
//! missing flag is false: a client never retries a request it was refused
//! unless the server opted in. A failure of the transport is different:
//! the connection is gone, and the library above may dial again.

use std::fmt;
use std::io;

use crate::MAX_FRAME;

/// The codes of feid/1. A client handles a code it does not know as
/// [`INTERNAL`].
pub mod code {
    /// The frame could not be read: not JSON, or too large. After a frame
    /// that is too large the connection is closed: its end is unknown.
    pub const BAD_FRAME: &str = "bad_frame";
    /// JSON, but no valid request: hello not first, a payload of the
    /// wrong shape.
    pub const BAD_REQUEST: &str = "bad_request";
    /// The versions do not overlap, or a frame after the hello carries
    /// another than the one negotiated. The connection is closed.
    pub const UNSUPPORTED_VERSION: &str = "unsupported_version";
    /// An op this server does not know: how a client learns that it
    /// talks to an older one.
    pub const UNSUPPORTED_OP: &str = "unsupported_op";
    /// The transport names no workload. The connection is closed.
    pub const NOT_ATTRIBUTED: &str = "not_attributed";
    /// The input to `sign` is not a TLS 1.3 CertificateVerify. Never
    /// retriable: the caller misbehaves.
    pub const REFUSED_NOT_CERTIFICATE_VERIFY: &str = "refused.not_certificate_verify";
    /// A `hash` other than `none`.
    pub const REFUSED_UNSUPPORTED_HASH: &str = "refused.unsupported_hash";
    /// The signature's audit record could not be written, so it was not
    /// made. Retriable.
    pub const UNAVAILABLE_AUDIT_LOG: &str = "unavailable.audit_log";
    /// The workload has no valid certificate yet, or no longer: a pod
    /// that was just resumed, an issuer that could not be reached before
    /// the old one expired. Retriable.
    pub const UNAVAILABLE_IDENTITY: &str = "unavailable.identity";
    /// The workload has no identity at all. Not retriable.
    pub const IDENTITY_UNPROVISIONED: &str = "identity.unprovisioned";
    /// Anything else. Its message never carries the server's detail.
    pub const INTERNAL: &str = "internal";
}

/// What a client call failed with.
#[derive(Debug)]
pub enum Error {
    /// The server's error frame.
    Remote {
        code: String,
        message: String,
        retriable: bool,
    },
    /// The stream failed or ended. The connection is not usable any more.
    Transport(io::Error),
    /// A frame that is no valid answer.
    Malformed(String),
    /// A frame larger than [`MAX_FRAME`]: the stream cannot be read on.
    FrameTooLarge,
}

impl Error {
    /// The server's code, for a remote error.
    pub fn code(&self) -> Option<&str> {
        match self {
            Error::Remote { code, .. } => Some(code),
            _ => None,
        }
    }

    /// A refusal the server marked retriable.
    pub fn is_retriable_refusal(&self) -> bool {
        matches!(
            self,
            Error::Remote {
                retriable: true,
                ..
            }
        )
    }

    /// A refusal that must not be asked again.
    pub fn is_refusal(&self) -> bool {
        matches!(
            self,
            Error::Remote {
                retriable: false,
                ..
            }
        )
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Remote { code, message, .. } => write!(f, "feid: {code}: {message}"),
            Error::Transport(e) => write!(f, "feid: transport: {e}"),
            Error::Malformed(m) => write!(f, "feid: malformed frame: {m}"),
            Error::FrameTooLarge => write!(f, "feid: a frame larger than {MAX_FRAME} bytes"),
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Error::Transport(e) => Some(e),
            _ => None,
        }
    }
}

/// A handler's answer when it does not give what was asked: a code of
/// [`code`] and a message for the caller, or an internal failure whose
/// detail the server logs and never sends.
#[derive(Clone, Debug, PartialEq)]
pub struct Refusal {
    pub code: String,
    pub message: String,
    pub retriable: bool,
    /// For [`Refusal::internal`]: what only the log gets.
    pub detail: Option<String>,
}

impl Refusal {
    pub fn new(code: &str, message: impl Into<String>, retriable: bool) -> Refusal {
        Refusal {
            code: code.to_string(),
            message: message.into(),
            retriable,
            detail: None,
        }
    }

    /// A failure of the server's own: the client reads `internal error`.
    pub fn internal(detail: impl Into<String>) -> Refusal {
        Refusal {
            code: code::INTERNAL.to_string(),
            message: "internal error".to_string(),
            retriable: false,
            detail: Some(detail.into()),
        }
    }
}

impl fmt::Display for Refusal {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match &self.detail {
            Some(detail) => write!(f, "{}: {detail}", self.code),
            None => write!(f, "{}: {}", self.code, self.message),
        }
    }
}
