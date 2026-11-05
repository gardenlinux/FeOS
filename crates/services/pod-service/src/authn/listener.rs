// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods' HTTP bindings at the node's address (ADR 0028 §2): `feosd`
//! listens at `169.254.169.1` (`--otlp-address`), port 80 for the name
//! mode and 443 for the intercept mode, accepts a connection, knows the pod
//! by its source address, which the pod's TAP pins, and hands the accepted
//! socket to that pod's `feos-authn` (`conn`, `SCM_RIGHTS`) without
//! reading a byte. A connection from a pod without HTTP bindings, or whose
//! process does not run, is reset.
//!
//! The listeners outlive `feosd` as the resolver's sockets do:
//! `feosd-http.socket` or `feos-host-init` holds them and hands them over
//! (`LISTEN_FDNAMES` `http`), and a self-upgrade hands them on. Without,
//! `feosd` binds them itself, before the node holds the address.

use super::Authn;
use feos_utils::listen_fds;
use log::{debug, info, warn};
use nix::sys::socket::{setsockopt, sockopt};
use std::io;
use std::net::{IpAddr, Ipv4Addr, SocketAddr, SocketAddrV4};
use std::os::fd::{AsFd, OwnedFd};
use std::sync::Arc;
use std::time::Duration;
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::Semaphore;

/// Connections being handed over at a time.
const MAX_HANDING: usize = 1024;
/// How long a listener that failed to bind waits before it tries again.
const REBIND: Duration = Duration::from_secs(10);

/// Which pod sends from an address.
pub type Lookup = Arc<dyn Fn(IpAddr) -> Option<String> + Send + Sync>;

/// The listeners at `address`'s `ports`: those of `handed` bound there,
/// handed on to a self-upgrade, and new ones for the ports none was handed
/// for. A port that does not bind now is left out, and said.
pub fn listeners(
    handed: Vec<(OwnedFd, String)>,
    address: Ipv4Addr,
    ports: &[u16],
) -> Vec<(u16, std::net::TcpListener)> {
    let (mut taken, refused) = listen_fds::listeners_at(handed, address.into(), ports);
    for reason in refused {
        warn!("Bindings: The HTTP bindings' listeners do not take the {reason}");
    }
    for (port, _) in &taken {
        info!("Bindings: Taking the listener at {address}:{port} handed to feosd");
    }
    for port in ports {
        if taken.iter().any(|(taken, _)| taken == port) {
            continue;
        }
        match listen_fds::listen_freely(SocketAddrV4::new(address, *port)) {
            Ok(fd) => taken.push((*port, std::net::TcpListener::from(fd))),
            Err(e) => warn!("Bindings: Binding {address}:{port} failed: {e}"),
        }
    }
    for (_, listener) in &taken {
        if let Err(e) = listen_fds::hand_on(listener.as_fd(), listen_fds::HTTP) {
            warn!("Bindings: A self-upgrade will not hand on a listener: {e}");
        }
    }
    taken
}

/// Serves the HTTP bindings at `address`'s `ports` for as long as the
/// process runs, on the listeners of [`listeners`]; a port that did not
/// bind is tried again.
pub async fn serve_all(
    handed: Vec<(OwnedFd, String)>,
    address: Ipv4Addr,
    ports: &'static [u16],
    authn: Authn,
    lookup: Lookup,
) {
    let taken = listeners(handed, address, ports);
    let handing = Arc::new(Semaphore::new(MAX_HANDING));
    let mut tasks = Vec::new();
    for port in ports {
        let listener = taken
            .iter()
            .find(|(taken, _)| taken == port)
            .and_then(|(_, listener)| listener.try_clone().ok());
        let (authn, lookup, handing, port) =
            (authn.clone(), lookup.clone(), handing.clone(), *port);
        tasks.push(tokio::spawn(async move {
            let mut listener = listener;
            loop {
                let ready = match listener.take() {
                    Some(listener) => listener,
                    None => {
                        tokio::time::sleep(REBIND).await;
                        match listen_fds::listen_freely(SocketAddrV4::new(address, port)) {
                            Ok(fd) => std::net::TcpListener::from(fd),
                            Err(e) => {
                                debug!("Bindings: Binding {address}:{port} failed: {e}");
                                continue;
                            }
                        }
                    }
                };
                match serve(ready, port, authn.clone(), lookup.clone(), handing.clone()).await {
                    Ok(()) => return,
                    Err(e) => warn!("Bindings: The listener at {address}:{port} failed: {e}"),
                }
            }
        }));
    }
    drop(taken);
    info!("Bindings: The pods reach their HTTP bindings at {address}, ports {ports:?}");
    for task in tasks {
        let _ = task.await;
    }
}

/// Accepts the pods' connections on `listener`, port `port`, and hands
/// each to its pod's process. Returns only on an error the listener does
/// not recover from.
pub async fn serve(
    listener: std::net::TcpListener,
    port: u16,
    authn: Authn,
    lookup: Lookup,
    handing: Arc<Semaphore>,
) -> io::Result<()> {
    listener.set_nonblocking(true)?;
    let listener = TcpListener::from_std(listener)?;
    loop {
        let (stream, peer) = match listener.accept().await {
            Ok(accepted) => accepted,
            Err(e) => {
                pause(e).await?;
                continue;
            }
        };
        let Ok(permit) = handing.clone().try_acquire_owned() else {
            reset(stream);
            continue;
        };
        let (authn, lookup) = (authn.clone(), lookup.clone());
        tokio::spawn(async move {
            let _permit = permit;
            hand_over(stream, peer, port, &authn, &lookup).await;
        });
    }
}

/// Hands the connection from `peer` to its pod's process, or resets it.
async fn hand_over(stream: TcpStream, peer: SocketAddr, port: u16, authn: &Authn, lookup: &Lookup) {
    let Some(pod_id) = lookup(peer.ip().to_canonical()) else {
        debug!("Bindings: A connection from {peer}, which is no pod's, is reset");
        reset(stream);
        return;
    };
    let stream = match stream.into_std() {
        Ok(stream) => stream,
        Err(e) => {
            debug!("Bindings: A connection of pod {pod_id}: {e}");
            return;
        }
    };
    match authn.pass(&pod_id, port, peer, stream.as_fd()).await {
        // The process has its own copy; feosd's goes.
        Ok(()) => {}
        Err(why) => {
            debug!(
                "Bindings: Pod {pod_id}'s connection from {peer} to port {port} is reset: {why}"
            );
            if let Ok(stream) = TcpStream::from_std(stream) {
                reset(stream);
            }
        }
    }
}

/// Closes the connection with a reset: nothing was read of it, and the
/// pod learns at once that nothing serves it.
fn reset(stream: TcpStream) {
    let linger = libc::linger {
        l_onoff: 1,
        l_linger: 0,
    };
    let _ = setsockopt(&stream, sockopt::Linger, &linger);
    drop(stream);
}

/// Waits a moment after an accept that failed for a reason that may pass,
/// such as running out of descriptors; returns the errors of a listener
/// that is broken.
async fn pause(error: io::Error) -> io::Result<()> {
    match error.raw_os_error() {
        Some(libc::EBADF | libc::ENOTSOCK | libc::EINVAL | libc::EOPNOTSUPP) => Err(error),
        Some(libc::ECONNABORTED | libc::ECONNRESET | libc::EPROTO | libc::EPERM) => Ok(()),
        _ => {
            warn!("Bindings: accept: {error}");
            tokio::time::sleep(Duration::from_millis(100)).await;
            Ok(())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::super::fake;
    use super::super::*;
    use super::*;
    use tokio::io::AsyncReadExt;

    const POD: &str = "a0000000001";

    #[tokio::test]
    async fn a_pods_connection_is_handed_to_its_process_and_anothers_is_reset() {
        let dir = tempfile::tempdir().unwrap();
        let seen = fake::Shared::default();
        let config = Config {
            binary: PathBuf::from("/nonexistent/feos-authn"),
            jail: false,
        };
        let authn = Authn::with_launcher(config, dir.path(), fake::launcher(seen.clone()));
        authn.set_issuer(Arc::new(|_| {
            Box::pin(async { Ok(IssuePodCredentialsResponse::default()) })
        }));
        authn.want(POD, true, "");
        authn
            .wait_ready(POD, Duration::from_secs(10))
            .await
            .unwrap();

        // 127.0.0.1 is the pod's; 127.0.0.2 nobody's.
        let lookup: Lookup =
            Arc::new(|address| (address == IpAddr::from([127, 0, 0, 1])).then(|| POD.to_string()));
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let handing = Arc::new(Semaphore::new(4));
        tokio::spawn(serve(listener, 80, authn.clone(), lookup, handing));

        let mut pod = TcpStream::connect(address).await.unwrap();
        let mut said = String::new();
        pod.read_to_string(&mut said).await.unwrap();
        assert_eq!(
            said,
            format!("authn has \"{}\"\n", pod.local_addr().unwrap())
        );
        let conn = lock(&seen)
            .messages
            .iter()
            .find(|m| m["op"] == "conn")
            .cloned()
            .unwrap();
        assert_eq!(conn["port"], 80);

        // From an address no pod has: reset, not read.
        let socket = tokio::net::TcpSocket::new_v4().unwrap();
        socket.bind("127.0.0.2:0".parse().unwrap()).unwrap();
        let mut other = socket.connect(address).await.unwrap();
        let mut buffer = [0u8; 16];
        let read = other.read(&mut buffer).await;
        assert!(
            matches!(&read, Err(e) if e.kind() == io::ErrorKind::ConnectionReset),
            "{read:?}"
        );

        // A pod whose process does not run: reset.
        authn.want(POD, false, "");
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        while authn.pid(POD).is_some() {
            assert!(tokio::time::Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        let mut pod = TcpStream::connect(address).await.unwrap();
        let read = pod.read(&mut buffer).await;
        assert!(
            matches!(&read, Err(e) if e.kind() == io::ErrorKind::ConnectionReset),
            "{read:?}"
        );
        authn.remove(POD).await;
    }
}
