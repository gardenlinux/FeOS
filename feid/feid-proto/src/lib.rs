// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feid/1: the protocol between an application's TLS stack and the signer
//! that holds its workload identity's key (FeOS ADR 0014). The
//! application fetches its certificate chain and the trust bundle and has
//! its TLS 1.3 CertificateVerify signed; the key never enters it.
//!
//! Versioned NDJSON over a stream that is the caller's by construction (a
//! Unix socket, a vsock stream): one compact JSON object and `\n` per
//! frame, at most [`MAX_FRAME`] bytes, a hello that negotiates the
//! version first. The specification is `feid/docs/protocol.md`; on the
//! wire feid/1 is fetls/1, the protocol of the FeTLS prototype.
//!
//! - [`frame`]: reading and writing frames, with the size cap enforced
//!   while reading;
//! - [`client`]: the application's side, blocking, as a TLS stack asks
//!   for a signature in a call that cannot wait on a future;
//! - [`server`]: the signer's side, on tokio; it negotiates, frames, and
//!   checks every input to `sign` ([`cv`]) before its handler sees it;
//! - [`error`]: the codes and the two error types.
//!
//! Attribution is the transport's, never a frame's: no request names a
//! workload, and the signer knows whose stream it serves before it reads.

pub mod client;
pub mod cv;
pub mod error;
pub mod frame;
pub mod server;

use serde::{Deserialize, Serialize};

pub use client::Client;
pub use cv::{CertificateVerify, Role};
pub use error::{Error, Refusal};
pub use server::{serve, Handler, ServeOptions};

/// The only version this implementation speaks.
pub const VERSION: u32 = 1;

/// The largest frame, either way, without its `\n`. Far above any
/// legitimate one (the largest is a PEM chain); it bounds what a hostile
/// peer can make the other side hold.
pub const MAX_FRAME: usize = 1 << 20;

/// The operations of feid/1.
pub mod op {
    pub const HELLO: &str = "hello";
    pub const HELLO_ACK: &str = "hello_ack";
    pub const GET_CERTIFICATE: &str = "get_certificate";
    pub const CERTIFICATE: &str = "certificate";
    pub const GET_CA: &str = "get_ca";
    pub const CA_BUNDLE: &str = "ca_bundle";
    pub const SIGN: &str = "sign";
    pub const SIGNATURE: &str = "signature";
    /// One-way: never answered.
    pub const HANDSHAKE_REPORT: &str = "handshake_report";
    /// One-way: a passive wire observer's, which fetls/1 had. Read and
    /// dropped, never answered.
    pub const WIRE_EVENT: &str = "wire_event";
    pub const ERROR: &str = "error";
}

/// `sign`'s `hash`: the message is the whole TLS 1.3 CertificateVerify
/// input, unhashed, as TLS hands it to an Ed25519 key. The only value v1
/// accepts.
pub const HASH_NONE: &str = "none";

/// The highest version in both ranges, or None when they do not overlap.
pub fn negotiate(
    client_min: u32,
    client_max: u32,
    server_min: u32,
    server_max: u32,
) -> Option<u32> {
    let version = client_max.min(server_max);
    (version >= client_min.max(server_min)).then_some(version)
}

/// What a library saw of one completed handshake: metadata only, never
/// bytes of the connection or of a key. One-way.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct HandshakeReport {
    /// `inbound` or `outbound`.
    #[serde(default)]
    pub direction: String,
    /// `local>remote`, each `host:port`.
    #[serde(default)]
    pub tuple: String,
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub cipher_suite: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub sni: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub alpn: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub peer_subject: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub peer_spiffe: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub peer_issuer: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub peer_serial: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub peer_not_after: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub peer_fingerprint: String,
    /// What the client offered, as hex codes.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub offered_ciphers: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub offered_kex: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub offered_alpn: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub offered_version: String,
}

/// `[u8]` fields are base64 strings on the wire, the standard alphabet,
/// padded: what Go's encoding/json makes of a `[]byte`.
pub(crate) mod b64 {
    use base64::engine::general_purpose::STANDARD;
    use base64::Engine;
    use serde::{Deserialize, Deserializer, Serializer};

    pub fn serialize<S: Serializer>(bytes: &[u8], s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(&STANDARD.encode(bytes))
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Vec<u8>, D::Error> {
        let text = String::deserialize(d)?;
        STANDARD.decode(text).map_err(serde::de::Error::custom)
    }
}

/// The fields every frame has.
#[derive(Deserialize)]
pub(crate) struct Envelope {
    /// Missing, it is 0, which no version is.
    #[serde(default)]
    pub v: u64,
    #[serde(default)]
    pub op: String,
    #[serde(default)]
    pub id: Option<u32>,
}

impl Envelope {
    /// The envelope of a raw frame; an error when it is no JSON object
    /// or has no op.
    pub(crate) fn parse(raw: &[u8]) -> Result<Envelope, String> {
        let envelope: Envelope =
            serde_json::from_slice(raw).map_err(|e| format!("frame is not valid JSON: {e}"))?;
        if envelope.op.is_empty() {
            return Err("frame has no op".to_string());
        }
        Ok(envelope)
    }
}

/// A frame as it is written: the envelope, then the payload's fields, in
/// that order, as the Go reference writes them.
#[derive(Serialize)]
pub(crate) struct Out<'a, P: Serialize> {
    pub v: u32,
    pub op: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub id: Option<u32>,
    #[serde(flatten)]
    pub payload: P,
}

#[derive(Serialize)]
pub(crate) struct Empty {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn negotiation_takes_the_highest_common_version() {
        let cases: &[(u32, u32, u32, u32, Option<u32>)] = &[
            (1, 1, 1, 1, Some(1)),
            (1, 3, 1, 2, Some(2)),
            (2, 2, 1, 1, None),
            (1, 1, 2, 2, None),
            (1, 5, 3, 9, Some(5)),
            (0, 0, 1, 1, None),
        ];
        for &(cmin, cmax, smin, smax, want) in cases {
            assert_eq!(
                negotiate(cmin, cmax, smin, smax),
                want,
                "{cmin}..{cmax} with {smin}..{smax}"
            );
        }
    }

    #[test]
    fn a_report_leaves_out_what_it_does_not_know() {
        let report = HandshakeReport {
            direction: "outbound".into(),
            tuple: "[fd00::2]:5000>[fd00::3]:5432".into(),
            version: "TLS 1.3".into(),
            cipher_suite: "TLS_AES_128_GCM_SHA256".into(),
            peer_spiffe: "spiffe://example.org/db".into(),
            ..Default::default()
        };
        let json = serde_json::to_string(&report).unwrap();
        assert_eq!(
            json,
            r#"{"direction":"outbound","tuple":"[fd00::2]:5000>[fd00::3]:5432","version":"TLS 1.3","cipher_suite":"TLS_AES_128_GCM_SHA256","peer_spiffe":"spiffe://example.org/db"}"#
        );
        assert_eq!(
            serde_json::from_str::<HandshakeReport>(&json).unwrap(),
            report
        );
        // Fields a newer library adds are ignored.
        let newer: HandshakeReport =
            serde_json::from_str(r#"{"direction":"inbound","resumed":true}"#).unwrap();
        assert_eq!(newer.direction, "inbound");
    }

    #[test]
    fn an_envelope_needs_an_op() {
        assert!(Envelope::parse(br#"{"v":1}"#).is_err());
        assert!(Envelope::parse(b"not json").is_err());
        let e = Envelope::parse(br#"{"op":"get_ca","id":7}"#).unwrap();
        assert_eq!((e.v, e.op.as_str(), e.id), (0, "get_ca", Some(7)));
    }
}
