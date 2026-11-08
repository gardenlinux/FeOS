// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The check that keeps a signer from being a signing oracle: it signs
//! only the exact input of a TLS 1.3 CertificateVerify (RFC 8446 §4.4.3),
//!
//! ```text
//! 64 × 0x20 || "TLS 1.3, client CertificateVerify" or
//!              "TLS 1.3, server CertificateVerify" || 0x00 || transcript hash
//! ```
//!
//! with a transcript hash of 32 or 48 bytes, as SHA-256 and SHA-384, the
//! hashes of TLS 1.3's cipher suites, make it. Ed25519 signs the input
//! unhashed, so the signer sees all of it. Any other input is refused.
//! The check exists once, here: the server runs it before its handler
//! sees a `sign`.

use std::fmt;

use crate::error::{code, Refusal};

const PAD: usize = 64;
const CLIENT: &[u8] = b"TLS 1.3, client CertificateVerify";
const SERVER: &[u8] = b"TLS 1.3, server CertificateVerify";

/// Whose CertificateVerify an input is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Role {
    Client,
    Server,
}

impl Role {
    pub fn as_str(self) -> &'static str {
        match self {
            Role::Client => "client",
            Role::Server => "server",
        }
    }
}

impl fmt::Display for Role {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// An input that passed the check.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct CertificateVerify<'a> {
    pub role: Role,
    /// The transcript hash at its end: what the signature commits to.
    pub transcript_hash: &'a [u8],
    /// The whole input, to be signed as it is.
    pub message: &'a [u8],
}

/// Checks `message` against the shape above.
pub fn check(message: &[u8]) -> Result<CertificateVerify<'_>, Refusal> {
    let refuse = |why: &str| {
        Refusal::new(
            code::REFUSED_NOT_CERTIFICATE_VERIFY,
            format!("refusing to sign: not a TLS 1.3 CertificateVerify input ({why})"),
            false,
        )
    };
    if message.len() < PAD || message[..PAD].iter().any(|&b| b != 0x20) {
        return Err(refuse("no padding of 64 spaces"));
    }
    let rest = &message[PAD..];
    let (role, after) = if let Some(after) = rest.strip_prefix(CLIENT) {
        (Role::Client, after)
    } else if let Some(after) = rest.strip_prefix(SERVER) {
        (Role::Server, after)
    } else {
        return Err(refuse("no CertificateVerify context"));
    };
    let Some(hash) = after.strip_prefix(&[0u8]) else {
        return Err(refuse("no separator after the context"));
    };
    if hash.len() != 32 && hash.len() != 48 {
        return Err(refuse(&format!(
            "a transcript hash of {} bytes",
            hash.len()
        )));
    }
    Ok(CertificateVerify {
        role,
        transcript_hash: hash,
        message,
    })
}

#[cfg(test)]
pub(crate) fn input(role: Role, hash: &[u8]) -> Vec<u8> {
    let mut m = vec![0x20u8; PAD];
    m.extend_from_slice(match role {
        Role::Client => CLIENT,
        Role::Server => SERVER,
    });
    m.push(0);
    m.extend_from_slice(hash);
    m
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn both_roles_and_both_hash_lengths_pass() {
        for role in [Role::Client, Role::Server] {
            for len in [32, 48] {
                let hash = vec![0xab; len];
                let m = input(role, &hash);
                let cv = check(&m).unwrap();
                assert_eq!(cv.role, role);
                assert_eq!(cv.transcript_hash, &hash[..]);
                assert_eq!(cv.message, &m[..]);
            }
        }
    }

    #[test]
    fn everything_else_is_refused() {
        let good = input(Role::Client, &[1; 32]);
        let mut cases: Vec<(&str, Vec<u8>)> = vec![
            ("empty", vec![]),
            ("arbitrary bytes", b"sign this for me, please".to_vec()),
            ("a hash too short", input(Role::Client, &[1; 31])),
            ("a hash too long", input(Role::Client, &[1; 33])),
            ("a SHA-512 hash", input(Role::Server, &[1; 64])),
            ("no hash", input(Role::Server, &[])),
        ];
        let mut padding = good.clone();
        padding[3] = b'x';
        cases.push(("a padding byte changed", padding));
        let mut short = good.clone();
        short.remove(0);
        cases.push(("63 spaces", short));
        let mut context = good.clone();
        context[PAD + 9] = b'X';
        cases.push(("another context", context));
        let mut separator = good.clone();
        separator[PAD + CLIENT.len()] = 1;
        cases.push(("no zero byte", separator));
        let mut tls12 = vec![0x20u8; PAD];
        tls12.extend_from_slice(b"TLS 1.2, client CertificateVerify\0");
        tls12.extend_from_slice(&[1; 32]);
        cases.push(("TLS 1.2's wording", tls12));
        for (what, m) in cases {
            let refusal = check(&m).expect_err(what);
            assert_eq!(refusal.code, code::REFUSED_NOT_CERTIFICATE_VERIFY, "{what}");
            assert!(!refusal.retriable, "{what}");
        }
    }
}
