// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Host side of the vsock transport to a FeOS guest.
//!
//! Firecracker exposes a guest's vsock device to the host as a Unix socket
//! ("hybrid vsock"). A host process connects to that socket, writes
//! `CONNECT <port>\n`, and the VMM answers `OK <host port>\n` once the guest
//! has accepted; from then on the stream carries the guest connection.
//!
//! The connect and its handshake are bounded (`CONNECT_TIMEOUT`): while
//! Firecracker writes a snapshot, or the microVM is paused, nothing answers
//! the handshake, and a call would hang until its caller gave up. A guest
//! that answers in time has no bound from then on: a stream, as an exec's
//! or a port forward's, lasts as long as it lasts.
//!
//! A caller that waits for a guest that may answer later takes its own
//! bound for the connect (`channel_within`): the microVM service's ping of
//! a resumed guest 30 s, as a VM restored from a snapshot written elsewhere
//! has its memory served page by page from object storage, and its pushes
//! of the time and the network and its free-page request before a
//! snapshot 10 s. The calls to the guest that a pod's callers make (exec,
//! port forward, GetPod, the bring-up) keep `CONNECT_TIMEOUT`.

use hyper_util::rt::TokioIo;
use std::io;
use std::path::{Path, PathBuf};
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;
use tonic::transport::{Channel, Endpoint, Uri};
use tower::service_fn;

/// Guest port of the FeOS API.
pub const API_PORT: u32 = 10000;
/// Host port the guest's lifecycle uplink dials (LifecycleService).
pub const UPLINK_PORT: u32 = 10001;
/// Host port of the pod's `feos-authn` (ADR 0014): one port for what it
/// serves the guest, a one-line preamble first that says what a stream is
/// for (`feid` for FeID's signer).
pub const AUTHN_PORT: u32 = 10002;

/// The longest reply the handshake accepts: "OK " and a 32-bit port.
const MAX_REPLY: usize = 32;

/// How long the connect to a guest and its handshake may take. A guest
/// that runs answers in milliseconds; one whose microVM is paused, or
/// whose Firecracker writes a snapshot, does not answer at all.
pub const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);

/// Connects to `port` in the guest behind the Firecracker vsock socket
/// `uds`, within `CONNECT_TIMEOUT`.
pub async fn connect(uds: &Path, port: u32) -> io::Result<UnixStream> {
    connect_within(uds, port, CONNECT_TIMEOUT).await
}

/// `connect`, within `deadline`: past it, an error of kind `TimedOut`.
pub async fn connect_within(uds: &Path, port: u32, deadline: Duration) -> io::Result<UnixStream> {
    tokio::time::timeout(deadline, handshake(uds, port))
        .await
        .unwrap_or_else(|_| {
            Err(io::Error::new(
                io::ErrorKind::TimedOut,
                format!("the guest did not answer within {}", seconds(deadline)),
            ))
        })
}

/// A duration as the errors say it: "5s", "0.2s".
pub fn seconds(duration: Duration) -> String {
    if duration.subsec_millis() == 0 {
        format!("{}s", duration.as_secs())
    } else {
        format!("{:.1}s", duration.as_secs_f64())
    }
}

async fn handshake(uds: &Path, port: u32) -> io::Result<UnixStream> {
    let mut stream = UnixStream::connect(uds).await?;
    stream
        .write_all(format!("CONNECT {port}\n").as_bytes())
        .await?;

    // Read the reply one byte at a time: the guest may start talking right
    // after it, and a buffered reader would swallow those bytes.
    let mut reply = Vec::new();
    loop {
        let byte = stream.read_u8().await?;
        if byte == b'\n' {
            break;
        }
        if reply.len() == MAX_REPLY {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "vsock handshake reply too long",
            ));
        }
        reply.push(byte);
    }
    if !reply.starts_with(b"OK ") {
        return Err(io::Error::new(
            io::ErrorKind::ConnectionRefused,
            format!(
                "guest refused CONNECT {port}: {:?}",
                String::from_utf8_lossy(&reply)
            ),
        ));
    }
    Ok(stream)
}

/// A gRPC channel to `port` in the guest behind the Firecracker vsock socket
/// `uds`. Every new HTTP/2 connection does its own handshake, within
/// `CONNECT_TIMEOUT`.
pub async fn channel(
    uds: impl Into<PathBuf>,
    port: u32,
) -> Result<Channel, tonic::transport::Error> {
    channel_within(uds, port, CONNECT_TIMEOUT).await
}

/// `channel`, whose connects and handshakes take `deadline` at most:
/// past it, the error is one that [`timed_out`] tells.
pub async fn channel_within(
    uds: impl Into<PathBuf>,
    port: u32,
    deadline: Duration,
) -> Result<Channel, tonic::transport::Error> {
    let uds = uds.into();
    Endpoint::from_static("http://feos.vsock")
        .connect_with_connector(service_fn(move |_: Uri| {
            let uds = uds.clone();
            async move { connect_within(&uds, port, deadline).await.map(TokioIo::new) }
        }))
        .await
}

/// Whether a channel failed because the guest did not answer its connect
/// in time.
pub fn timed_out(error: &tonic::transport::Error) -> bool {
    let mut source: Option<&(dyn std::error::Error + 'static)> = Some(error);
    while let Some(error) = source {
        if error
            .downcast_ref::<io::Error>()
            .is_some_and(|e| e.kind() == io::ErrorKind::TimedOut)
        {
            return true;
        }
        source = error.source();
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::AsyncBufReadExt;
    use tokio::net::UnixListener;

    /// A fake VMM that answers the handshake with `reply` and then sends
    /// `payload` right behind it, in the same write.
    async fn fake_vmm(reply: &'static str, payload: &'static [u8]) -> (tempfile::TempDir, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("v.sock");
        let listener = UnixListener::bind(&path).unwrap();
        tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut stream = tokio::io::BufReader::new(stream);
            let mut line = String::new();
            stream.read_line(&mut line).await.unwrap();
            assert_eq!(line, "CONNECT 10000\n");
            let mut out = reply.as_bytes().to_vec();
            out.extend_from_slice(payload);
            stream.get_mut().write_all(&out).await.unwrap();
        });
        (dir, path)
    }

    #[tokio::test]
    async fn keeps_bytes_after_the_reply() {
        let (_dir, path) = fake_vmm("OK 1073741824\n", b"guest").await;
        let mut stream = connect(&path, API_PORT).await.unwrap();
        let mut buf = [0u8; 5];
        stream.read_exact(&mut buf).await.unwrap();
        assert_eq!(&buf, b"guest");
    }

    /// A VMM that takes the connection and never answers, as one whose
    /// Firecracker writes a snapshot.
    fn silent_vmm() -> (tempfile::TempDir, PathBuf, UnixListener) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("v.sock");
        let listener = UnixListener::bind(&path).unwrap();
        (dir, path, listener)
    }

    #[tokio::test]
    async fn a_guest_that_does_not_answer_times_out() {
        let (_dir, path, _listener) = silent_vmm();
        let deadline = Duration::from_millis(200);
        let started = tokio::time::Instant::now();
        let err = tokio::time::timeout(
            Duration::from_secs(5),
            connect_within(&path, API_PORT, deadline),
        )
        .await
        .expect("the connect waited past its deadline")
        .unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::TimedOut);
        assert_eq!(err.to_string(), "the guest did not answer within 0.2s");
        assert!(started.elapsed() < Duration::from_secs(2));

        let err = tokio::time::timeout(
            Duration::from_secs(5),
            channel_within(&path, API_PORT, deadline),
        )
        .await
        .expect("the channel waited past its deadline")
        .unwrap_err();
        assert!(timed_out(&err), "{err:?}");
    }

    #[tokio::test]
    async fn a_stream_has_no_bound_once_connected() {
        let (_dir, path, listener) = silent_vmm();
        let guest = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut stream = tokio::io::BufReader::new(stream);
            let mut line = String::new();
            stream.read_line(&mut line).await.unwrap();
            stream.get_mut().write_all(b"OK 1\n").await.unwrap();
            let mut later = [0u8; 5];
            stream.read_exact(&mut later).await.unwrap();
            later
        });
        let mut stream = connect_within(&path, API_PORT, Duration::from_millis(100))
            .await
            .unwrap();
        tokio::time::sleep(Duration::from_millis(300)).await;
        // Still the guest's: the bound was the connect's alone.
        stream.write_all(b"after").await.unwrap();
        assert_eq!(&guest.await.unwrap(), b"after");
    }

    #[test]
    fn durations_read_as_seconds() {
        assert_eq!(seconds(CONNECT_TIMEOUT), "5s");
        assert_eq!(seconds(Duration::from_millis(1500)), "1.5s");
    }

    #[tokio::test]
    async fn refused_connect_is_an_error() {
        let (_dir, path) = fake_vmm("FAILURE\n", b"").await;
        let err = connect(&path, API_PORT).await.unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::ConnectionRefused);
    }
}
