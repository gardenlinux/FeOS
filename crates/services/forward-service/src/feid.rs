// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! FeID's socket (ADR 0014): `/run/feos/feid.sock` in every container,
//! where FeID's libraries find the pod's signer. Every connection accepted
//! there becomes one vsock stream to the host (CID 2, port 10002), where
//! the pod's feos-authn holds the pod's key; FeOS writes the preamble
//! `feid\n` first and splices bytes from then on. It parses nothing of
//! feid/1: the signer checks what it signs.
//!
//! It is the SSH agent socket's pipe with a preamble: the same socket in
//! the share directory, mode 0666, bound under a temporary name and moved
//! into place, the same idempotent set, the same reset after a boot or
//! resume (see `ssh.rs`). The preamble carries no authority: the host
//! knows the pod by the socket the stream arrives on. Port 10002 is
//! feos-authn's one port, whose preamble selects what a stream is for.

use std::sync::Arc;

use tokio::io::AsyncWriteExt;

use crate::kube::{DialFuture, HostDialer, VsockDialer};
use crate::ssh::{Names, Socket};
use feos_utils::share as container;

/// What a stream to feos-authn's port says first, to be FeID's.
pub const FEID_PREAMBLE: &[u8] = b"feid\n";

const NAMES: Names = Names {
    file: "feid.sock",
    tmp: ".feid.sock.new",
    what: "feid socket",
};

/// The socket, while the host says the pod has an identity.
pub struct FeidSocket(Arc<Socket>);

impl Default for FeidSocket {
    fn default() -> Self {
        Self::with_parts(
            container::SHARE_DIR.into(),
            Arc::new(VsockDialer(feos_utils::vsock::AUTHN_PORT)),
        )
    }
}

impl FeidSocket {
    /// Tests put the socket in a directory of their own and dial a Unix
    /// socket instead of vsock.
    pub fn with_parts(dir: std::path::PathBuf, dialer: Arc<dyn HostDialer>) -> Self {
        FeidSocket(Arc::new(Socket::new(
            dir,
            NAMES,
            Arc::new(Preamble(dialer)),
        )))
    }

    /// Makes the socket, or removes it; first ends every open connection
    /// when `reset`. Whether it is there afterwards, or why not.
    pub async fn set(&self, enabled: bool, reset: bool) -> Result<bool, String> {
        let socket = self.0.clone();
        // A call runs to completion even if its caller gives up, as the
        // agent socket's does.
        let answer = tokio::spawn(async move { socket.apply(enabled, reset).await })
            .await
            .map_err(|e| format!("feid socket: {e}"))?;
        match answer.error.is_empty() {
            true => Ok(answer.listening),
            false => Err(answer.error),
        }
    }
}

/// Dials the host and says what the stream is for.
struct Preamble(Arc<dyn HostDialer>);

impl HostDialer for Preamble {
    fn dial(&self) -> DialFuture {
        let dial = self.0.dial();
        Box::pin(async move {
            let mut host = dial.await?;
            host.write_all(FEID_PREAMBLE).await?;
            host.flush().await?;
            Ok(host)
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::kube::HostIo;
    use std::os::unix::fs::{FileTypeExt, PermissionsExt};
    use std::path::PathBuf;
    use tokio::io::AsyncReadExt;
    use tokio::net::{UnixListener, UnixStream};

    /// Stands in for vsock: the host's end of a guest stream is a Unix
    /// connection anyway.
    struct UnixDialer(PathBuf);

    impl HostDialer for UnixDialer {
        fn dial(&self) -> DialFuture {
            let path = self.0.clone();
            Box::pin(async move {
                let s = UnixStream::connect(path).await?;
                Ok(Box::new(s) as Box<dyn HostIo>)
            })
        }
    }

    #[tokio::test]
    async fn a_container_reaches_feos_authn_with_the_preamble_first() {
        // Short enough for sun_path whatever TMPDIR is.
        let dir = PathBuf::from(format!("/tmp/feos-feid-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let share = dir.join("share");
        let host_path = dir.join("v.sock_10002");
        let host = UnixListener::bind(&host_path).unwrap();
        let socket = FeidSocket::with_parts(share.clone(), Arc::new(UnixDialer(host_path)));

        assert_eq!(socket.set(true, false).await, Ok(true));
        let path = share.join("feid.sock");
        let meta = std::fs::symlink_metadata(&path).unwrap();
        assert!(meta.file_type().is_socket());
        assert_eq!(meta.permissions().mode() & 0o777, 0o666);
        // Again: the socket in place is kept.
        assert_eq!(socket.set(true, false).await, Ok(true));

        let mut guest = UnixStream::connect(&path).await.unwrap();
        guest
            .write_all(b"{\"v\":1,\"op\":\"hello\"}\n")
            .await
            .unwrap();
        let (mut stream, _) = host.accept().await.unwrap();
        let mut got = vec![0u8; FEID_PREAMBLE.len() + 21];
        stream.read_exact(&mut got).await.unwrap();
        assert_eq!(&got[..FEID_PREAMBLE.len()], FEID_PREAMBLE);
        assert_eq!(&got[FEID_PREAMBLE.len()..], b"{\"v\":1,\"op\":\"hello\"}\n");
        stream.write_all(b"answer\n").await.unwrap();
        let mut back = [0u8; 7];
        guest.read_exact(&mut back).await.unwrap();
        assert_eq!(&back, b"answer\n");

        assert_eq!(socket.set(false, false).await, Ok(false));
        assert!(!path.exists());
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_container_sees_it_where_the_libraries_look() {
        assert_eq!(
            std::path::Path::new(container::SHARE_DEST).join(NAMES.file),
            std::path::Path::new(container::FEID_SOCK)
        );
        assert_eq!(container::FEID_SOCK, "/run/feos/feid.sock");
    }
}
