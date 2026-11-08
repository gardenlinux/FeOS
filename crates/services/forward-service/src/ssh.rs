// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! SshService — the agent socket for SSH bindings.
//!
//! A sandbox logs in to an SSH server without ever holding the key. The
//! container's `SSH_AUTH_SOCK` names `/run/feos/ssh-agent.sock`, a Unix
//! socket of FeOS's. Every connection accepted there becomes one vsock
//! stream to the host (CID 2, port 10003), where the sandbox's jailed ssh
//! agent decides what it signs and signs it.
//!
//! This is `KubeService`'s pipe without the preamble: one port, one agent,
//! nothing to select. FeOS writes nothing of its own and parses nothing;
//! it splices bytes, and a half-close travels in both directions. Identity
//! comes from the host-side socket the stream arrives on, which is the
//! sandbox's own — a process that dialled the vsock port itself would gain
//! nothing the socket does not already give it.
//!
//! **Where the socket lives.** FeOS binds it in its share directory
//! (`container::SHARE_DIR`, on the /run tmpfs), which every container sees
//! read-only at `/run/feos` — bind-mounted by the runtime spec at create
//! and adopt, like the toolbox at /feos-tools. The mount is of the
//! directory, so a socket made or removed while a container runs appears
//! or vanishes in it, too. The path and the variable are constant; the
//! socket exists only while the host says the sandbox has bindings, and
//! a missing socket is how ssh learns there is no agent. Mode 0666: any
//! uid in the container may connect, as with any local ssh-agent the
//! session owns. A Unix socket is not a port, so no port forward can
//! publish it.
//!
//! **SetAgent is idempotent.** The manager pushes it after every boot and
//! resume and whenever the bindings go from none to some or back.
//! `enabled` with the socket in place keeps it as it is; `enabled = false`
//! removes the file, then stops accepting, and leaves connections already
//! accepted to finish. The socket lives in guest RAM, so a restored
//! snapshot has it back and the push after a resume simply finds it.
//!
//! **The connections do not survive a pause** — for the reason
//! `svc/kube.rs` documents: the host side dies with the run's agent, and
//! an idle vsock stream held across a snapshot never wakes its reader. An
//! agent connection lives for the milliseconds of a login, so this is
//! rare, but the push after a boot or resume carries `reset_connections`
//! all the same, and FeOS then closes every agent connection it holds,
//! both sockets of each, before it applies `enabled`.
//!
//! When no agent listens on the host (one crashed and is being respawned,
//! say), the guest connection is closed at once. ssh sees the agent go
//! away, and the login fails with an agent error instead of hanging.

use std::io;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use feos_proto::ssh_service::ssh_service_server::SshService;
use feos_proto::ssh_service::{SetAgentRequest, SetAgentResponse};
use tokio::net::{UnixListener, UnixStream};
use tokio::task::{JoinHandle, JoinSet};
use tonic::{Request, Response, Status};

use crate::kube::{HostDialer, VsockDialer};
use feos_utils::share as container;

/// Port the sandbox's ssh agent is reached on (`ops.SSHVsockPort`).
/// Firecracker maps a guest connection to (CID 2, 10003) to the host path
/// `<uds_path>_10003`, which the manager creates and hands to the agent.
pub const SSH_VSOCK_PORT: u32 = 10003;

/// The socket's name in the share directory.
const SOCK_NAME: &str = "ssh-agent.sock";

/// The name the socket is bound under before it is moved into place, so
/// that it appears at [`SOCK_NAME`] only once it has its final mode.
const SOCK_TMP_NAME: &str = ".ssh-agent.sock.new";

/// Any uid in the container may connect.
const SOCK_MODE: u32 = 0o666;

/// Upper bound for reaching the agent; see kube.rs's `DIAL_TIMEOUT`.
const DIAL_TIMEOUT: Duration = Duration::from_secs(5);

/// Pause after a failed `accept` (out of fds, say), so the loop does not
/// spin while the condition lasts.
const ACCEPT_BACKOFF: Duration = Duration::from_millis(100);

/// The accepted connections, so a reset can end them all. Each one is a
/// task that owns both of its sockets; ending the task closes them.
struct Conns {
    dialer: Arc<dyn HostDialer>,
    open: Mutex<JoinSet<()>>,
}

impl Conns {
    fn open(&self) -> MutexGuard<'_, JoinSet<()>> {
        // Spawns and swaps only; a panic elsewhere cannot leave it torn.
        self.open.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Pipe one accepted connection.
    fn spawn(&self, guest: UnixStream) {
        let mut open = self.open();
        // Collect what has finished, so the set holds open connections
        // and not the history of every one.
        while open.try_join_next().is_some() {}
        open.spawn(pipe(guest, self.dialer.clone()));
    }

    /// End every open connection and wait until each is gone. Returns how
    /// many there were.
    async fn reset(&self) -> usize {
        let mut old = std::mem::take(&mut *self.open());
        while old.try_join_next().is_some() {}
        let n = old.len();
        old.shutdown().await;
        n
    }
}

pub struct SshApiHandler(Arc<Socket>);

/// What a socket of the share directory is called: its file, the file it
/// is bound as before it is moved into place, and what logs call it. The
/// SSH agent's, and FeID's (`crate::feid`), which pipes the same way.
#[derive(Clone, Copy)]
pub(crate) struct Names {
    pub file: &'static str,
    pub tmp: &'static str,
    pub what: &'static str,
}

const SSH_NAMES: Names = Names {
    file: SOCK_NAME,
    tmp: SOCK_TMP_NAME,
    what: "ssh agent socket",
};

pub(crate) struct Socket {
    /// The share directory the socket is made in.
    dir: PathBuf,
    names: Names,
    /// The accept loop while the socket exists; the loop owns the
    /// listener. The async mutex also serializes calls, and a call holds
    /// it while it waits for a stopped loop to be gone.
    live: tokio::sync::Mutex<Option<JoinHandle<()>>>,
    conns: Arc<Conns>,
}

impl Default for SshApiHandler {
    fn default() -> Self {
        Self::with_parts(
            PathBuf::from(container::SHARE_DIR),
            Arc::new(VsockDialer(SSH_VSOCK_PORT)),
        )
    }
}

impl SshApiHandler {
    /// Tests put the socket in a directory of their own and dial a Unix
    /// socket instead of vsock.
    pub fn with_parts(dir: PathBuf, dialer: Arc<dyn HostDialer>) -> Self {
        SshApiHandler(Arc::new(Socket::new(dir, SSH_NAMES, dialer)))
    }
}

impl Socket {
    pub(crate) fn new(dir: PathBuf, names: Names, dialer: Arc<dyn HostDialer>) -> Socket {
        Socket {
            dir,
            names,
            live: tokio::sync::Mutex::new(None),
            conns: Arc::new(Conns {
                dialer,
                open: Mutex::new(JoinSet::new()),
            }),
        }
    }

    fn path(&self) -> PathBuf {
        self.dir.join(self.names.file)
    }

    /// Converge on `enabled`, after ending every open connection if
    /// `reset` says the host side of them is gone.
    pub(crate) async fn apply(&self, enabled: bool, reset: bool) -> SetAgentResponse {
        let what = self.names.what;
        let mut live = self.live.lock().await;

        // Before the socket: a connection accepted from here on is one to
        // the new agent, and must stay.
        if reset {
            let n = self.conns.reset().await;
            if n > 0 {
                log::info!("{what}: reset {n} connection(s)");
            }
        }

        // An accept loop never ends on its own; one that did (a panic) is
        // dropped here and made afresh below if the socket is wanted.
        if live.as_ref().is_some_and(|task| task.is_finished()) {
            *live = None;
        }

        if !enabled {
            // The file first: from here on no connect finds the socket,
            // and none lands in the backlog of a listener about to close.
            let removed = remove_if_present(&self.path());
            let had = match live.take() {
                Some(task) => {
                    close(task).await;
                    true
                }
                None => false,
            };
            match removed {
                Err(e) => log::warn!("{what}: remove {}: {e}", self.path().display()),
                Ok(()) if had => log::info!("{what}: removed"),
                Ok(()) => {}
            }
            return SetAgentResponse {
                listening: false,
                error: String::new(),
            };
        }

        if live.is_some() {
            return SetAgentResponse {
                listening: true,
                error: String::new(),
            };
        }
        match listen(&self.dir, self.names) {
            Ok(listener) => {
                *live = Some(tokio::spawn(accept_loop(
                    listener,
                    self.conns.clone(),
                    what,
                )));
                let seen = Path::new(container::SHARE_DEST).join(self.names.file);
                log::info!("{what}: listening ({})", seen.display());
                SetAgentResponse {
                    listening: true,
                    error: String::new(),
                }
            }
            Err(e) => {
                let error = format!("{what} {}: {e}", self.path().display());
                log::warn!("{error}");
                SetAgentResponse {
                    listening: false,
                    error,
                }
            }
        }
    }
}

#[tonic::async_trait]
impl SshService for SshApiHandler {
    async fn set_agent(
        &self,
        req: Request<SetAgentRequest>,
    ) -> Result<Response<SetAgentResponse>, Status> {
        let socket = self.0.clone();
        let SetAgentRequest {
            enabled,
            reset_connections,
        } = req.into_inner();
        // A call runs to completion even if the caller gives up halfway.
        // Cancelled between removing the file and closing the listener, it
        // would leave a listener nobody can reach and the next enable
        // believing the socket is in place.
        tokio::spawn(async move { socket.apply(enabled, reset_connections).await })
            .await
            .map(Response::new)
            .map_err(|e| Status::internal(format!("ssh agent socket: {e}")))
    }
}

/// Make the socket: bind it under a temporary name, give it its mode,
/// then rename it into place. A container process sees either no socket
/// or one it may connect to, never one still carrying the umask's mode.
/// The rename also replaces whatever an earlier life left at the path.
fn listen(dir: &Path, names: Names) -> io::Result<UnixListener> {
    container::make_share_dir(dir)?;
    let tmp = dir.join(names.tmp);
    remove_if_present(&tmp)?;
    let listener = UnixListener::bind(&tmp)?;
    let placed = std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(SOCK_MODE))
        .and_then(|()| std::fs::rename(&tmp, dir.join(names.file)));
    if let Err(e) = placed {
        let _ = std::fs::remove_file(&tmp);
        return Err(e);
    }
    Ok(listener)
}

fn remove_if_present(path: &Path) -> io::Result<()> {
    match std::fs::remove_file(path) {
        Err(e) if e.kind() != io::ErrorKind::NotFound => Err(e),
        _ => Ok(()),
    }
}

/// Stop the accept loop and wait until it is gone, so the listener is
/// closed by the time the RPC answers. Connections already accepted run
/// in tasks of their own and are not touched.
async fn close(task: JoinHandle<()>) {
    task.abort();
    let _ = task.await;
}

async fn accept_loop(listener: UnixListener, conns: Arc<Conns>, what: &'static str) {
    loop {
        match listener.accept().await {
            Ok((conn, _peer)) => conns.spawn(conn),
            Err(e) => {
                // Out of fds, or a connection aborted before it was
                // accepted. Neither ends the listener.
                log::info!("{what}: accept: {e}");
                tokio::time::sleep(ACCEPT_BACKOFF).await;
            }
        }
    }
}

/// One accepted connection: reach the agent, then splice until both
/// directions are done. If the agent cannot be reached, the guest
/// connection is dropped, which closes it.
async fn pipe(mut guest: UnixStream, dialer: Arc<dyn HostDialer>) {
    let mut host = match tokio::time::timeout(DIAL_TIMEOUT, dialer.dial()).await {
        Ok(Ok(host)) => host,
        Ok(Err(_)) | Err(_) => return,
    };
    // EOF on either side becomes a shutdown(Write) on the other, and the
    // opposite direction keeps flowing until it ends too. An error on
    // either side drops both.
    let _ = tokio::io::copy_bidirectional(&mut guest, &mut host).await;
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::kube::{DialFuture, HostIo};
    use std::future::Future;
    use std::os::unix::fs::FileTypeExt;
    use std::sync::atomic::{AtomicU32, Ordering};
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    static SEQ: AtomicU32 = AtomicU32::new(0);

    /// Stands in for vsock: the agent's end of a guest stream is a Unix
    /// connection on the host anyway.
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

    /// A fresh directory with paths short enough for sun_path whatever
    /// TMPDIR is. Holds the share directory and the fake agent's socket;
    /// gone when the test ends.
    struct Scratch(PathBuf);

    impl Scratch {
        fn new() -> Self {
            let p = PathBuf::from(format!(
                "/tmp/feos-ssh-{}-{}",
                std::process::id(),
                SEQ.fetch_add(1, Ordering::Relaxed)
            ));
            let _ = std::fs::remove_dir_all(&p);
            std::fs::create_dir_all(&p).unwrap();
            Scratch(p)
        }

        fn share(&self) -> PathBuf {
            self.0.join("share")
        }

        /// The service, dialling `v.sock_10003` in here.
        fn svc(&self) -> Arc<SshApiHandler> {
            Arc::new(SshApiHandler::with_parts(
                self.share(),
                Arc::new(UnixDialer(self.0.join("v.sock_10003"))),
            ))
        }
    }

    impl Drop for Scratch {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    /// The service with its share directory, and a fake ssh agent: the
    /// listening end of `v.sock_10003`.
    fn fake_agent() -> (Arc<SshApiHandler>, PathBuf, UnixListener, Scratch) {
        let base = Scratch::new();
        let agent = UnixListener::bind(base.0.join("v.sock_10003")).expect("bind fake agent");
        (base.svc(), base.share(), agent, base)
    }

    /// No agent at all: the dial fails like a vsock connect with nobody
    /// listening on the host.
    fn no_agent() -> (Arc<SshApiHandler>, PathBuf, Scratch) {
        let base = Scratch::new();
        (base.svc(), base.share(), base)
    }

    async fn set(svc: &SshApiHandler, enabled: bool) -> SetAgentResponse {
        push(svc, enabled, false).await
    }

    /// The push after a boot or resume.
    async fn set_and_reset(svc: &SshApiHandler, enabled: bool) -> SetAgentResponse {
        push(svc, enabled, true).await
    }

    async fn push(svc: &SshApiHandler, enabled: bool, reset: bool) -> SetAgentResponse {
        svc.set_agent(Request::new(SetAgentRequest {
            enabled,
            reset_connections: reset,
        }))
        .await
        .expect("SetAgent never fails as a whole")
        .into_inner()
    }

    async fn within<F: Future>(f: F) -> F::Output {
        tokio::time::timeout(Duration::from_secs(5), f)
            .await
            .expect("timed out")
    }

    async fn connect(share: &Path) -> io::Result<UnixStream> {
        UnixStream::connect(share.join(SOCK_NAME)).await
    }

    async fn accept_stream(agent: &UnixListener) -> UnixStream {
        within(agent.accept()).await.unwrap().0
    }

    async fn task_id(svc: &SshApiHandler) -> Option<tokio::task::Id> {
        svc.0.live.lock().await.as_ref().map(|t| t.id())
    }

    fn assert_listening(resp: &SetAgentResponse) {
        assert!(resp.listening, "not listening: {}", resp.error);
        assert!(resp.error.is_empty(), "{}", resp.error);
    }

    #[test]
    fn the_socket_path_is_the_contracts() {
        assert_eq!(
            Path::new(container::SHARE_DEST).join(SOCK_NAME),
            Path::new(container::SSH_AGENT_SOCK)
        );
        assert_eq!(container::SSH_AGENT_SOCK, "/run/feos/ssh-agent.sock");
        assert_eq!(SSH_VSOCK_PORT, 10003);
    }

    #[test]
    fn the_agent_env_is_added_once_and_never_over_a_set_one() {
        let mut env = vec!["PATH=/bin".to_string()];
        container::set_ssh_agent_env(&mut env);
        container::set_ssh_agent_env(&mut env);
        assert_eq!(
            env,
            vec![
                "PATH=/bin".to_string(),
                "SSH_AUTH_SOCK=/run/feos/ssh-agent.sock".to_string()
            ]
        );

        let mut own = vec!["SSH_AUTH_SOCK=/tmp/other".to_string()];
        container::set_ssh_agent_env(&mut own);
        assert_eq!(own, vec!["SSH_AUTH_SOCK=/tmp/other".to_string()]);
    }

    #[tokio::test]
    async fn enabling_makes_a_socket_any_uid_may_connect_to() {
        let (svc, share, _base) = no_agent();
        assert!(!share.exists(), "the call makes the directory");

        let resp = set(&svc, true).await;
        assert_listening(&resp);
        let meta = std::fs::symlink_metadata(share.join(SOCK_NAME)).unwrap();
        assert!(meta.file_type().is_socket());
        assert_eq!(meta.permissions().mode() & 0o777, 0o666);
        let dir_mode = std::fs::metadata(&share).unwrap().permissions().mode() & 0o777;
        assert_eq!(dir_mode & 0o005, 0o005, "others may look up the socket");
        // Nothing but the socket is left in the directory.
        let names: Vec<_> = std::fs::read_dir(&share)
            .unwrap()
            .map(|e| e.unwrap().file_name())
            .collect();
        assert_eq!(names, vec![std::ffi::OsString::from(SOCK_NAME)]);
        connect(&share).await.expect("the socket accepts");

        // The same call again keeps the socket as it is — same accept loop.
        let id = task_id(&svc).await;
        assert_listening(&set(&svc, true).await);
        assert_listening(&set_and_reset(&svc, true).await);
        assert_eq!(task_id(&svc).await, id);
        set(&svc, false).await;
    }

    #[tokio::test]
    async fn disabling_removes_the_socket_and_is_idempotent() {
        let (svc, share, _base) = no_agent();
        // Disabled from the start: nothing to remove, nothing fails.
        let resp = set(&svc, false).await;
        assert!(!resp.listening && resp.error.is_empty());

        assert_listening(&set(&svc, true).await);
        let resp = set(&svc, false).await;
        assert!(!resp.listening && resp.error.is_empty());
        assert!(!share.join(SOCK_NAME).exists());
        assert_eq!(task_id(&svc).await, None);
        let gone = connect(&share).await.expect_err("socket still there");
        assert_eq!(gone.kind(), io::ErrorKind::NotFound);

        let resp = set(&svc, false).await;
        assert!(!resp.listening && resp.error.is_empty());

        // And back.
        assert_listening(&set(&svc, true).await);
        connect(&share).await.expect("listening again");
        set(&svc, false).await;
    }

    #[tokio::test]
    async fn a_stale_file_at_the_path_is_replaced() {
        let (svc, share, _base) = no_agent();
        std::fs::create_dir_all(&share).unwrap();
        std::fs::write(share.join(SOCK_NAME), b"left over").unwrap();
        assert_listening(&set(&svc, true).await);
        let meta = std::fs::symlink_metadata(share.join(SOCK_NAME)).unwrap();
        assert!(meta.file_type().is_socket());
        set(&svc, false).await;
    }

    #[tokio::test]
    async fn a_socket_that_cannot_be_made_is_reported() {
        let base = Scratch::new();
        let svc = base.svc();
        // The share directory's place is taken by a file.
        std::fs::write(base.share(), b"").unwrap();
        let resp = set(&svc, true).await;
        assert!(!resp.listening);
        assert!(
            resp.error.contains(&base.share().display().to_string()),
            "{}",
            resp.error
        );
        assert_eq!(task_id(&svc).await, None);

        // The next push retries; once the way is clear it listens.
        std::fs::remove_file(base.share()).unwrap();
        assert_listening(&set(&svc, true).await);
        set(&svc, false).await;
    }

    /// No preamble: the agent's first bytes are the client's. A client
    /// that ends its request with a shutdown still reads the answer.
    #[tokio::test]
    async fn the_guest_half_close_reaches_the_agent_and_the_answer_comes_back() {
        let (svc, share, agent, _base) = fake_agent();
        set(&svc, true).await;

        let mut client = connect(&share).await.unwrap();
        let mut host = accept_stream(&agent).await;

        // REQUEST_IDENTITIES, as ssh sends it.
        let request = [0u8, 0, 0, 1, 11];
        client.write_all(&request).await.unwrap();
        client.shutdown().await.unwrap();

        let mut got = Vec::new();
        within(host.read_to_end(&mut got)).await.unwrap();
        assert_eq!(got, request);

        // IDENTITIES_ANSWER with no keys.
        let answer = [0u8, 0, 0, 5, 12, 0, 0, 0, 0];
        host.write_all(&answer).await.unwrap();
        host.shutdown().await.unwrap();
        let mut back = Vec::new();
        within(client.read_to_end(&mut back)).await.unwrap();
        assert_eq!(back, answer);
        set(&svc, false).await;
    }

    #[tokio::test]
    async fn the_agent_half_close_leaves_the_guest_direction_open() {
        let (svc, share, agent, _base) = fake_agent();
        set(&svc, true).await;

        let mut client = connect(&share).await.unwrap();
        let mut host = accept_stream(&agent).await;

        host.write_all(b"first").await.unwrap();
        host.shutdown().await.unwrap();
        let mut got = Vec::new();
        within(client.read_to_end(&mut got)).await.unwrap();
        assert_eq!(got, b"first");

        // The client has seen EOF and can still talk.
        client.write_all(b"after").await.unwrap();
        client.shutdown().await.unwrap();
        let mut late = Vec::new();
        within(host.read_to_end(&mut late)).await.unwrap();
        assert_eq!(late, b"after");
        set(&svc, false).await;
    }

    #[tokio::test]
    async fn without_an_agent_the_guest_connection_is_closed() {
        let (svc, share, _base) = no_agent();
        set(&svc, true).await;

        let mut client = connect(&share).await.unwrap();
        let mut buf = [0u8; 16];
        assert_eq!(within(client.read(&mut buf)).await.unwrap(), 0);

        // The socket itself stays: the next login may find the agent back.
        assert!(share.join(SOCK_NAME).exists());
        set(&svc, false).await;
    }

    #[tokio::test]
    async fn disabling_leaves_open_connections_alone() {
        let (svc, share, agent, _base) = fake_agent();
        set(&svc, true).await;

        let mut client = connect(&share).await.unwrap();
        let mut host = accept_stream(&agent).await;

        let resp = set(&svc, false).await;
        assert!(!resp.listening);
        assert!(connect(&share).await.is_err());

        client.write_all(b"ping").await.unwrap();
        let mut buf = [0u8; 4];
        within(host.read_exact(&mut buf)).await.unwrap();
        assert_eq!(&buf, b"ping");
        host.write_all(b"pong").await.unwrap();
        within(client.read_exact(&mut buf)).await.unwrap();
        assert_eq!(&buf, b"pong");
    }

    /// The push after a resume: every connection of the previous run ends,
    /// both sides, and the socket stays for the next login.
    #[tokio::test]
    async fn a_reset_ends_every_open_connection_and_keeps_the_socket() {
        let (svc, share, agent, _base) = fake_agent();
        set(&svc, true).await;
        let listener = task_id(&svc).await;

        let mut idle = connect(&share).await.unwrap();
        let mut old_host = accept_stream(&agent).await;
        // A second one: a reset ends all of them, not just the first.
        let mut idle2 = connect(&share).await.unwrap();
        let mut old_host2 = accept_stream(&agent).await;

        assert_listening(&set_and_reset(&svc, true).await);

        // Both sides of both connections see EOF at once — the call has
        // already waited for them to be closed.
        let mut buf = [0u8; 16];
        for client in [&mut idle, &mut idle2] {
            assert_eq!(within(client.read(&mut buf)).await.unwrap(), 0);
        }
        for host in [&mut old_host, &mut old_host2] {
            assert_eq!(within(host.read(&mut buf)).await.unwrap(), 0);
        }

        // The socket itself was kept, and the next login works.
        assert_eq!(task_id(&svc).await, listener);
        let mut fresh = connect(&share).await.unwrap();
        let mut host = accept_stream(&agent).await;
        fresh.write_all(b"ping").await.unwrap();
        let mut got = [0u8; 4];
        within(host.read_exact(&mut got)).await.unwrap();
        assert_eq!(&got, b"ping");
        set(&svc, false).await;
    }

    /// A resume of a sandbox whose bindings were removed while it was
    /// paused: connections end and the socket goes.
    #[tokio::test]
    async fn a_reset_with_disable_ends_connections_and_removes_the_socket() {
        let (svc, share, agent, _base) = fake_agent();
        set(&svc, true).await;
        let mut client = connect(&share).await.unwrap();
        let mut host = accept_stream(&agent).await;

        let resp = set_and_reset(&svc, false).await;
        assert!(!resp.listening && resp.error.is_empty());
        let mut buf = [0u8; 16];
        assert_eq!(within(client.read(&mut buf)).await.unwrap(), 0);
        assert_eq!(within(host.read(&mut buf)).await.unwrap(), 0);
        assert!(!share.join(SOCK_NAME).exists());
    }

    #[tokio::test]
    async fn without_a_reset_open_connections_are_left_alone() {
        let (svc, share, agent, _base) = fake_agent();
        set(&svc, true).await;
        let mut client = connect(&share).await.unwrap();
        let mut host = accept_stream(&agent).await;

        // A binding change while running.
        assert_listening(&set(&svc, true).await);

        client.write_all(b"ping").await.unwrap();
        let mut buf = [0u8; 4];
        within(host.read_exact(&mut buf)).await.unwrap();
        assert_eq!(&buf, b"ping");
        host.write_all(b"pong").await.unwrap();
        within(client.read_exact(&mut buf)).await.unwrap();
        assert_eq!(&buf, b"pong");
        set(&svc, false).await;
    }
}
