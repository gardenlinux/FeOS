// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The control socket between `feosd` and a pod's `feos-authn` (ADR 0014,
//! ADR 0028): a `SOCK_SEQPACKET` Unix socket that `feosd` binds at
//! `<pod dir>/authn.sock` and hands to the process listening (fd 4), and
//! dials, at the start and after every restart of its own.
//!
//! One JSON object per message, at most [`MAX_MESSAGE`] bytes, each with
//! `"v": 1` and its `"op"`; fields it does not know a reader ignores, and
//! an `op` it does not know it skips. File descriptors travel only with
//! `conn` and `vsock`, as `SCM_RIGHTS`.
//!
//! `feosd` to `feos-authn`:
//!
//! - `{"v":1,"op":"hello","podId":…,"denyPrefixes":[…],"sourceV4":…|null,
//!   "sourceV6":…|null}`, first on every connection. `feos-authn` answers
//!   it with `ready` if it has its credentials, else it asks for them.
//! - `{"v":1,"op":"credentials","certificate":{…}|null,"bindings":{"enc",
//!   "ciphertext","version"}|null}`, or `{"v":1,"op":"error","message":…}`:
//!   the control plane's answer to `credentials`. The certificate is
//!   `{"spiffeId","chain":[<b64 DER>…],"trustBundle":[<b64 DER>…],
//!   "notBefore","notAfter","renewAfter"}`, times in Unix seconds.
//! - `{"v":1,"op":"refetch"}`: the pod's bindings moved to another version;
//!   `feos-authn` asks for its credentials again.
//! - `{"v":1,"op":"conn","port":80|443,"peer":"<ip:port>"}` with one fd: a
//!   connection the pod opened to an HTTP binding, accepted, not read.
//! - `{"v":1,"op":"vsock","inode":…}` with one fd: the listening socket
//!   that the pod's microVM's vsock port 10002 connects to, bound in its
//!   jail, on which FeID's signer serves (ADR 0014), and its path's inode;
//!   it replaces the one before.
//!
//! `feos-authn` to `feosd`:
//!
//! - `{"v":1,"op":"credentials","hpkePublicKey":<b64>,"csr":<b64 DER>}`.
//! - `{"v":1,"op":"ready","version":…}`: it holds the bindings of that
//!   version; after every set, and after every `hello` once it has one.
//! - `{"v":1,"op":"error","message":…}`: an answer it cannot use, such as
//!   a box that does not open; it asks again later itself.
//! - `{"v":1,"op":"vsock","inode":…}`: the inode it was given with the
//!   vsock listener it serves on; after it took one, and after every
//!   `hello` once it has one, so that a new `feosd` does not bind it again.
//!
//! A `hello` whose `podId` is not the process's closes the connection
//! (docs/architecture/authn.md).

use nix::cmsg_space;
use nix::errno::Errno;
use nix::sys::socket::{
    connect, recvmsg, sendmsg, setsockopt, socket, sockopt, AddressFamily, ControlMessage,
    ControlMessageOwned, MsgFlags, SockFlag, SockType, UnixAddr,
};
use serde::{Deserialize, Serialize};
use std::io::{self, IoSlice, IoSliceMut};
use std::os::fd::{AsRawFd, BorrowedFd, FromRawFd, OwnedFd, RawFd};
use std::path::Path;
use tokio::io::unix::AsyncFd;
use tokio::io::Interest;

/// The protocol's version, in every message.
pub const VERSION: u32 = 1;
/// A message's most bytes.
pub const MAX_MESSAGE: usize = 1 << 20;
/// The send buffer of `feosd`'s end: the largest message fits.
const SEND_BUFFER: usize = 2 * MAX_MESSAGE;

/// What `feosd` sends.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(tag = "op", rename_all = "lowercase", rename_all_fields = "camelCase")]
pub enum ToAuthn {
    Hello {
        pod_id: String,
        deny_prefixes: Vec<String>,
        source_v4: Option<String>,
        source_v6: Option<String>,
    },
    Credentials {
        certificate: Option<Certificate>,
        bindings: Option<Sealed>,
    },
    Error {
        message: String,
    },
    Refetch,
    Conn {
        port: u16,
        peer: String,
    },
    /// With the listener as its descriptor; the inode of its path.
    Vsock {
        inode: u64,
    },
}

/// The pod's certificate (feos.fleet.v1.Certificate).
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Certificate {
    pub spiffe_id: String,
    /// Base64 of DER, the pod's first.
    pub chain: Vec<String>,
    pub trust_bundle: Vec<String>,
    /// Unix seconds.
    pub not_before: i64,
    pub not_after: i64,
    pub renew_after: i64,
}

/// The pod's bindings, sealed to the run's key (feos.fleet.v1.SealedBindings).
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Sealed {
    /// Base64.
    pub enc: String,
    pub ciphertext: String,
    pub version: String,
}

/// What `feos-authn` sends.
#[derive(Clone, Debug, PartialEq, Deserialize, Serialize)]
#[serde(tag = "op", rename_all = "lowercase", rename_all_fields = "camelCase")]
pub enum FromAuthn {
    Credentials {
        /// Base64 of the X25519 key.
        hpke_public_key: String,
        /// Base64 of the PKCS#10 DER.
        csr: String,
    },
    Ready {
        #[serde(default)]
        version: String,
    },
    Error {
        #[serde(default)]
        message: String,
    },
    Vsock {
        inode: u64,
    },
    /// One this version does not know: skipped.
    #[serde(other)]
    Unknown,
}

#[derive(Serialize)]
struct Outgoing<'a, T> {
    v: u32,
    #[serde(flatten)]
    body: &'a T,
}

#[derive(Deserialize)]
struct Incoming {
    v: u32,
}

/// Encodes a message with its version.
pub fn encode<T: Serialize>(body: &T) -> Vec<u8> {
    // A struct of strings, numbers and options always serializes.
    serde_json::to_vec(&Outgoing { v: VERSION, body }).unwrap_or_default()
}

/// Decodes a message of `feos-authn`'s. Another version is an error.
pub fn decode(bytes: &[u8]) -> Result<FromAuthn, String> {
    let Incoming { v } =
        serde_json::from_slice(bytes).map_err(|e| format!("not a message: {e}"))?;
    if v != VERSION {
        return Err(format!("a message of version {v}, not {VERSION}"));
    }
    serde_json::from_slice(bytes).map_err(|e| format!("a message that does not read: {e}"))
}

/// One end of a control socket, non-blocking, on tokio's reactor.
pub struct Control {
    fd: AsyncFd<OwnedFd>,
    /// One sender at a time: a message is one `sendmsg`, but its fd and
    /// its order are the caller's.
    send: tokio::sync::Mutex<()>,
}

impl Control {
    /// Dials the process listening at `path`.
    pub fn connect(path: &Path) -> io::Result<Self> {
        let fd = socket(
            AddressFamily::Unix,
            SockType::SeqPacket,
            SockFlag::SOCK_CLOEXEC | SockFlag::SOCK_NONBLOCK,
            None,
        )?;
        let address = UnixAddr::new(path)?;
        // A listener's backlog takes a connect at once, or refuses it.
        connect(fd.as_raw_fd(), &address)?;
        // Root may grow its buffer beyond the system's limit; others keep
        // theirs, which holds what is sent but the largest answers.
        let _ = setsockopt(&fd, sockopt::SndBufForce, &SEND_BUFFER);
        Self::from_fd(fd)
    }

    /// An end that is connected already, non-blocking.
    pub fn from_fd(fd: OwnedFd) -> io::Result<Self> {
        Ok(Self {
            fd: AsyncFd::with_interest(fd, Interest::READABLE | Interest::WRITABLE)?,
            send: tokio::sync::Mutex::new(()),
        })
    }

    /// Sends `message`, with `fd` if there is one.
    pub async fn send(&self, message: &ToAuthn, fd: Option<BorrowedFd<'_>>) -> io::Result<()> {
        let bytes = encode(message);
        if bytes.len() > MAX_MESSAGE {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!(
                    "a message of {} bytes, more than {MAX_MESSAGE}",
                    bytes.len()
                ),
            ));
        }
        let _one = self.send.lock().await;
        let fds: Vec<RawFd> = fd.iter().map(AsRawFd::as_raw_fd).collect();
        loop {
            let mut ready = self.fd.writable().await?;
            let sent = ready.try_io(|fd| {
                let iov = [IoSlice::new(&bytes)];
                let rights = [ControlMessage::ScmRights(&fds)];
                let cmsgs: &[ControlMessage] = match fds.is_empty() {
                    true => &[],
                    false => &rights,
                };
                sendmsg::<()>(fd.as_raw_fd(), &iov, cmsgs, MsgFlags::MSG_NOSIGNAL, None)
                    .map_err(io::Error::from)
            });
            match sent {
                Ok(Ok(n)) if n == bytes.len() => return Ok(()),
                Ok(Ok(n)) => {
                    return Err(io::Error::other(format!(
                        "sent {n} of {} bytes of a message",
                        bytes.len()
                    )))
                }
                Ok(Err(e)) => return Err(e),
                Err(_would_block) => continue,
            }
        }
    }

    /// The next message, as it came; None once the other end closed. Fds
    /// that came with it are closed: `feos-authn` sends none.
    pub async fn recv(&self) -> io::Result<Option<Vec<u8>>> {
        let mut buffer = vec![0u8; MAX_MESSAGE];
        loop {
            let mut ready = self.fd.readable().await?;
            let received = ready.try_io(|fd| {
                let mut iov = [IoSliceMut::new(&mut buffer)];
                let mut space = cmsg_space!([RawFd; 4]);
                let message = recvmsg::<()>(
                    fd.as_raw_fd(),
                    &mut iov,
                    Some(&mut space),
                    MsgFlags::MSG_CMSG_CLOEXEC,
                )
                .map_err(io::Error::from)?;
                if let Ok(cmsgs) = message.cmsgs() {
                    for cmsg in cmsgs {
                        if let ControlMessageOwned::ScmRights(fds) = cmsg {
                            for fd in fds {
                                // SAFETY: received with this message, ours.
                                drop(unsafe { OwnedFd::from_raw_fd(fd) });
                            }
                        }
                    }
                }
                if message.flags.contains(MsgFlags::MSG_TRUNC) {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        format!("a message of more than {MAX_MESSAGE} bytes"),
                    ));
                }
                Ok(message.bytes)
            });
            match received {
                Ok(Ok(0)) => return Ok(None),
                Ok(Ok(n)) => {
                    buffer.truncate(n);
                    return Ok(Some(buffer));
                }
                Ok(Err(e)) if e.raw_os_error() == Some(Errno::ECONNRESET as i32) => {
                    return Ok(None)
                }
                Ok(Err(e)) => return Err(e),
                Err(_would_block) => continue,
            }
        }
    }
}

/// A connected pair, for tests and the fake `feos-authn`.
pub fn pair() -> io::Result<(OwnedFd, OwnedFd)> {
    Ok(nix::sys::socket::socketpair(
        AddressFamily::Unix,
        SockType::SeqPacket,
        None,
        SockFlag::SOCK_CLOEXEC | SockFlag::SOCK_NONBLOCK,
    )?)
}

/// Receives one message and the fds that came with it, blocking: the
/// other side's, for tests and the fake `feos-authn`.
pub fn recv_with_fds(fd: BorrowedFd<'_>) -> io::Result<Option<(Vec<u8>, Vec<OwnedFd>)>> {
    let mut buffer = vec![0u8; MAX_MESSAGE];
    let mut iov = [IoSliceMut::new(&mut buffer)];
    let mut space = cmsg_space!([RawFd; 4]);
    let message = loop {
        match recvmsg::<()>(
            fd.as_raw_fd(),
            &mut iov,
            Some(&mut space),
            MsgFlags::MSG_CMSG_CLOEXEC,
        ) {
            Err(Errno::EINTR) => continue,
            other => break other?,
        }
    };
    let mut fds = Vec::new();
    for cmsg in message.cmsgs()? {
        if let ControlMessageOwned::ScmRights(received) = cmsg {
            // SAFETY: received with this message, ours.
            fds.extend(
                received
                    .into_iter()
                    .map(|fd| unsafe { OwnedFd::from_raw_fd(fd) }),
            );
        }
    }
    let n = message.bytes;
    if n == 0 {
        return Ok(None);
    }
    buffer.truncate(n);
    Ok(Some((buffer, fds)))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::io::{Read, Write};
    use std::os::fd::AsFd;

    #[test]
    fn messages_are_versioned_json_in_camel_case() {
        let hello = ToAuthn::Hello {
            pod_id: "p".into(),
            deny_prefixes: vec!["10.0.0.0/8".into()],
            source_v4: None,
            source_v6: Some("fd00::2".into()),
        };
        let value: serde_json::Value = serde_json::from_slice(&encode(&hello)).unwrap();
        assert_eq!(
            value,
            json!({"v": 1, "op": "hello", "podId": "p", "denyPrefixes": ["10.0.0.0/8"],
                   "sourceV4": null, "sourceV6": "fd00::2"})
        );
        let value: serde_json::Value = serde_json::from_slice(&encode(&ToAuthn::Refetch)).unwrap();
        assert_eq!(value, json!({"v": 1, "op": "refetch"}));
        let value: serde_json::Value =
            serde_json::from_slice(&encode(&ToAuthn::Vsock { inode: 7 })).unwrap();
        assert_eq!(value, json!({"v": 1, "op": "vsock", "inode": 7}));
        assert_eq!(
            decode(br#"{"v":1,"op":"vsock","inode":42}"#),
            Ok(FromAuthn::Vsock { inode: 42 })
        );
        let conn = ToAuthn::Conn {
            port: 443,
            peer: "169.254.16.5:40000".into(),
        };
        let value: serde_json::Value = serde_json::from_slice(&encode(&conn)).unwrap();
        assert_eq!(
            value,
            json!({"v": 1, "op": "conn", "port": 443, "peer": "169.254.16.5:40000"})
        );
        let answer = ToAuthn::Credentials {
            certificate: Some(Certificate {
                spiffe_id: "spiffe://t/pod/p".into(),
                not_after: 7,
                ..Default::default()
            }),
            bindings: Some(Sealed {
                enc: "ZQ==".into(),
                ciphertext: "Yw==".into(),
                version: "3".into(),
            }),
        };
        let value: serde_json::Value = serde_json::from_slice(&encode(&answer)).unwrap();
        assert_eq!(value["certificate"]["spiffeId"], "spiffe://t/pod/p");
        assert_eq!(value["certificate"]["notAfter"], 7);
        assert_eq!(value["bindings"]["version"], "3");
        let none = ToAuthn::Credentials {
            certificate: None,
            bindings: None,
        };
        let value: serde_json::Value = serde_json::from_slice(&encode(&none)).unwrap();
        assert_eq!(
            value,
            json!({"v": 1, "op": "credentials", "certificate": null, "bindings": null})
        );

        assert_eq!(
            decode(br#"{"v":1,"op":"credentials","hpkePublicKey":"aw==","csr":"Yw==","x":1}"#),
            Ok(FromAuthn::Credentials {
                hpke_public_key: "aw==".into(),
                csr: "Yw==".into()
            })
        );
        assert_eq!(
            decode(br#"{"v":1,"op":"ready","version":"3"}"#),
            Ok(FromAuthn::Ready {
                version: "3".into()
            })
        );
        assert_eq!(
            decode(br#"{"v":1,"op":"error","message":"the box does not open"}"#),
            Ok(FromAuthn::Error {
                message: "the box does not open".into()
            })
        );
        assert_eq!(
            decode(br#"{"v":1,"op":"later","what":1}"#),
            Ok(FromAuthn::Unknown)
        );
        assert!(decode(br#"{"v":2,"op":"ready"}"#).is_err());
        assert!(decode(br#"{"op":"ready"}"#).is_err());
        assert!(decode(b"garbage").is_err());
    }

    #[tokio::test]
    async fn a_message_carries_an_fd_and_eof_ends_the_stream() {
        let (ours, theirs) = pair().unwrap();
        let control = Control::from_fd(ours).unwrap();
        let (mut left, right) = std::os::unix::net::UnixStream::pair().unwrap();
        let conn = ToAuthn::Conn {
            port: 80,
            peer: "169.254.16.5:1".into(),
        };
        control.send(&conn, Some(right.as_fd())).await.unwrap();
        drop(right);
        let theirs = std::sync::Arc::new(theirs);
        let reader = theirs.clone();
        let (bytes, fds) = tokio::task::spawn_blocking(move || {
            nix::fcntl::fcntl(
                reader.as_fd(),
                nix::fcntl::FcntlArg::F_SETFL(nix::fcntl::OFlag::empty()),
            )
            .unwrap();
            recv_with_fds(reader.as_fd()).unwrap().unwrap()
        })
        .await
        .unwrap();
        let value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(value["op"], "conn");
        assert_eq!(fds.len(), 1);
        // The fd is the connection's other end.
        let mut passed = std::os::unix::net::UnixStream::from(fds.into_iter().next().unwrap());
        passed.write_all(b"hi").unwrap();
        let mut said = [0u8; 2];
        left.read_exact(&mut said).unwrap();
        assert_eq!(&said, b"hi");

        // Their answer, then their close.
        let ready = br#"{"v":1,"op":"ready","version":""}"#;
        nix::sys::socket::send(theirs.as_raw_fd(), ready, MsgFlags::empty()).unwrap();
        let got = control.recv().await.unwrap().unwrap();
        assert_eq!(decode(&got), Ok(FromAuthn::Ready { version: "".into() }));
        drop(theirs);
        assert_eq!(control.recv().await.unwrap(), None);
    }

    #[tokio::test]
    async fn a_message_too_large_is_not_sent() {
        let (ours, _theirs) = pair().unwrap();
        let control = Control::from_fd(ours).unwrap();
        let error = ToAuthn::Error {
            message: "x".repeat(MAX_MESSAGE),
        };
        assert!(control.send(&error, None).await.is_err());
    }
}
