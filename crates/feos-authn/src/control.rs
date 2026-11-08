// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The control socket between `feosd` and the pod's feos-authn: a
//! listening `SOCK_SEQPACKET` Unix socket that `feosd` binds at `<pod
//! dir>/authn.sock` and hands over as fd 4. `feosd` connects to it; one
//! connection serves at a time, and a new one replaces the old, so that a
//! `feosd` that starts again adopts the running process.
//!
//! One JSON object per message, `"v": 1` in each; file descriptors only
//! with `conn`, by `SCM_RIGHTS`. A message of another version, or of an
//! unknown `op`, is ignored.
//!
//! `feosd` → feos-authn:
//!
//! - `{"v":1,"op":"hello","podId":…,"denyPrefixes":[…],"sourceV4":…|null,
//!   "sourceV6":…|null}`, first on every connection.
//! - `{"v":1,"op":"credentials","certificate":{…}|null,"bindings":{"enc":
//!   b64,"ciphertext":b64,"version":…}|null}`: the answer to a credentials
//!   request, or `{"v":1,"op":"error","message":…}`. The certificate is
//!   FeID's identity ([`crate::feid`]); its `renewAfter` makes the next
//!   request due.
//! - `{"v":1,"op":"refetch"}`: the pod's bindings changed.
//! - `{"v":1,"op":"conn","port":80|443,"peer":"ip:port"}` with one fd: a
//!   connection of the pod's to 169.254.169.1.
//! - `{"v":1,"op":"vsock","inode":…}` with one fd: the listening socket of
//!   the pod's microVM's vsock port 10002, on which FeID's signer serves,
//!   and the inode of its path, which only `feosd` sees; it replaces the
//!   one before.
//!
//! feos-authn → `feosd`:
//!
//! - `{"v":1,"op":"credentials","hpkePublicKey":b64,"csr":b64}`: ask the
//!   control plane for the pod's certificate and sealed bindings.
//! - `{"v":1,"op":"ready","version":…}`: a set is opened and serves; sent
//!   again after every `hello`, so that a new `feosd` knows.
//! - `{"v":1,"op":"error","message":…}`: an answer that could not be
//!   used (a box that does not open); feos-authn asks again later.
//! - `{"v":1,"op":"vsock","inode":…}`: the inode `feosd` gave the vsock
//!   listener it serves on; after it took one, and after every `hello`
//!   once it has one.

use std::io::{self, IoSlice, IoSliceMut};
use std::net::{Ipv4Addr, Ipv6Addr};
use std::os::fd::{AsFd, AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::sync::Arc;
use std::time::Duration;

use base64::Engine;
use nix::sys::socket::{recvmsg, sendmsg, ControlMessageOwned, MsgFlags};
use serde::Deserialize;
use serde_json::{json, Value};
use tokio::io::unix::AsyncFd;
use tokio::sync::{watch, Semaphore};
use tokio::time::Instant;

use crate::audit::{self, Audit, BindingsRecord, InvalidBinding};
use crate::bindings::{self, Snapshot};
use crate::dest::{NetConfig, Prefix};
use crate::feid::{Identity, Signer};
use crate::keys::Keys;
use crate::proxy::{self, Ctx};
use crate::{intercept, Net};

/// The largest message: a credentials answer with every binding's PEMs.
/// `feosd` sends it in one message; past its socket's send buffer it
/// needs a larger one.
pub const MAX_MESSAGE: usize = 1 << 20;
/// The pod's connections served at once; past it, a new one is closed.
pub const MAX_CONNECTIONS: usize = 1024;
const BACKOFF_FIRST: Duration = Duration::from_secs(2);
const BACKOFF_MAX: Duration = Duration::from_secs(60);

#[derive(Deserialize)]
struct Envelope {
    v: u64,
    #[serde(default)]
    op: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Hello {
    pod_id: String,
    #[serde(default)]
    deny_prefixes: Vec<String>,
    #[serde(default)]
    source_v4: Option<String>,
    #[serde(default)]
    source_v6: Option<String>,
}

#[derive(Deserialize)]
struct Sealed {
    enc: String,
    ciphertext: String,
    #[serde(default)]
    version: String,
}

#[derive(Deserialize)]
struct Credentials {
    #[serde(default)]
    certificate: Option<Value>,
    #[serde(default)]
    bindings: Option<Sealed>,
}

#[derive(Deserialize)]
struct ErrorMsg {
    #[serde(default)]
    message: String,
}

#[derive(Deserialize)]
struct Vsock {
    #[serde(default)]
    inode: u64,
}

#[derive(Deserialize)]
struct Conn {
    port: u16,
    #[serde(default)]
    peer: Option<String>,
}

/// A seqpacket socket, non-blocking, on tokio's reactor.
struct Seq(AsyncFd<OwnedFd>);

fn nonblocking(fd: &OwnedFd) -> io::Result<()> {
    use nix::fcntl::{fcntl, FcntlArg, FdFlag, OFlag};
    let fl = OFlag::from_bits_truncate(fcntl(fd, FcntlArg::F_GETFL)?);
    fcntl(fd, FcntlArg::F_SETFL(fl | OFlag::O_NONBLOCK))?;
    fcntl(fd, FcntlArg::F_SETFD(FdFlag::FD_CLOEXEC))?;
    Ok(())
}

impl Seq {
    fn new(fd: OwnedFd) -> io::Result<Seq> {
        nonblocking(&fd)?;
        Ok(Seq(AsyncFd::new(fd)?))
    }

    async fn accept(&self) -> io::Result<OwnedFd> {
        use nix::sys::socket::{accept4, SockFlag};
        loop {
            let mut g = self.0.readable().await?;
            let r = g.try_io(|fd| {
                accept4(
                    fd.get_ref().as_raw_fd(),
                    SockFlag::SOCK_NONBLOCK | SockFlag::SOCK_CLOEXEC,
                )
                .map_err(io::Error::from)
            });
            if let Ok(r) = r {
                // SAFETY: accept4 returned a new descriptor that nothing
                // else owns.
                return r.map(|raw| unsafe { OwnedFd::from_raw_fd(raw) });
            }
        }
    }

    /// One message into `buf`: its length and its descriptors; None at
    /// the end of the connection.
    async fn recv(&self, buf: &mut [u8]) -> io::Result<Option<(usize, Vec<OwnedFd>)>> {
        loop {
            let mut g = self.0.readable().await?;
            let r = g.try_io(|fd| recv_once(fd.get_ref().as_fd().as_raw_fd(), buf));
            if let Ok(r) = r {
                return r;
            }
        }
    }

    async fn send(&self, msg: &[u8]) -> io::Result<()> {
        loop {
            let mut g = self.0.writable().await?;
            let r = g.try_io(|fd| {
                sendmsg::<()>(
                    fd.get_ref().as_raw_fd(),
                    &[IoSlice::new(msg)],
                    &[],
                    MsgFlags::MSG_NOSIGNAL,
                    None,
                )
                .map_err(io::Error::from)
            });
            if let Ok(r) = r {
                return r.map(|_| ());
            }
        }
    }
}

fn recv_once(fd: RawFd, buf: &mut [u8]) -> io::Result<Option<(usize, Vec<OwnedFd>)>> {
    let mut cmsg = nix::cmsg_space!([RawFd; 8]);
    let mut iov = [IoSliceMut::new(buf)];
    let msg = recvmsg::<()>(fd, &mut iov, Some(&mut cmsg), MsgFlags::MSG_CMSG_CLOEXEC)
        .map_err(io::Error::from)?;
    let mut fds = Vec::new();
    for c in msg.cmsgs().map_err(io::Error::from)? {
        if let ControlMessageOwned::ScmRights(raw) = c {
            for r in raw {
                // SAFETY: SCM_RIGHTS installed these descriptors in this
                // process for this message; nothing else owns them.
                fds.push(unsafe { OwnedFd::from_raw_fd(r) });
            }
        }
    }
    let (n, flags) = (msg.bytes, msg.flags);
    if flags.contains(MsgFlags::MSG_TRUNC) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!("a message larger than {} bytes", buf.len()),
        ));
    }
    if flags.contains(MsgFlags::MSG_CTRUNC) {
        log::warn!("control: a message's descriptors were cut; closing those that came");
        return Ok(Some((n, Vec::new())));
    }
    if n == 0 && fds.is_empty() {
        return Ok(None);
    }
    Ok(Some((n, fds)))
}

/// Whether and when to ask for credentials.
struct Fetch {
    /// A request is due: at start, after a `refetch`, after an error.
    want: bool,
    /// A request was sent on the current connection, its answer pending.
    inflight: bool,
    /// A `refetch` came while a request was in flight: ask again after.
    again: bool,
    retry_at: Option<Instant>,
    backoff: Duration,
    /// When the certificate in place is to be renewed.
    renew_at: Option<Instant>,
}

impl Fetch {
    fn fail(&mut self) {
        self.inflight = false;
        self.want = true;
        self.retry_at = Some(Instant::now() + self.backoff);
        self.backoff = (self.backoff * 2).min(BACKOFF_MAX);
    }
}

/// One pod's feos-authn.
pub struct Authn {
    pub pod_id: String,
    pub keys: Arc<Keys>,
    pub audit: Arc<Audit>,
    pub net: Arc<Net>,
    ready_tx: watch::Sender<Option<Arc<Snapshot>>>,
    identity_tx: watch::Sender<Option<Arc<Identity>>>,
    ctx: Arc<Ctx>,
    conns: Arc<Semaphore>,
    /// FeID's signer, and the vsock listener it serves on: its inode and
    /// its accept loop.
    signer: Arc<Signer>,
    vsock: std::sync::Mutex<Option<(u64, tokio::task::AbortHandle)>>,
}

impl Authn {
    pub fn new(pod_id: String, keys: Keys, audit: Arc<Audit>, net: Arc<Net>) -> Authn {
        let (ready_tx, ready) = watch::channel(None);
        let ctx = Arc::new(Ctx {
            audit: audit.clone(),
            net: net.clone(),
            ready,
        });
        let keys = Arc::new(keys);
        let identity_tx = watch::channel(None).0;
        let signer = Arc::new(Signer::new(
            keys.clone(),
            audit.clone(),
            identity_tx.subscribe(),
        ));
        Authn {
            pod_id: pod_id.to_ascii_lowercase(),
            keys,
            audit,
            net,
            ready_tx,
            identity_tx,
            ctx,
            conns: Arc::new(Semaphore::new(MAX_CONNECTIONS)),
            signer,
            vsock: std::sync::Mutex::new(None),
        }
    }

    fn snapshot(&self) -> Option<Arc<Snapshot>> {
        self.ready_tx.borrow().clone()
    }

    /// FeID's signer for the pod, which serves the identity in place.
    pub fn signer(&self) -> Signer {
        Signer::new(
            self.keys.clone(),
            self.audit.clone(),
            self.identity_tx.subscribe(),
        )
    }

    /// Serves FeID's signer on `listener` from now on, in place of the
    /// listener before; streams it accepted go on. `inode` is its path's,
    /// as `feosd` bound it, which is said back; 0 when none was given.
    pub fn serve_vsock(&self, listener: OwnedFd, inode: u64) {
        let signer = self.signer.clone();
        let task = tokio::spawn(async move {
            if let Err(e) = signer.serve(listener).await {
                log::error!("vsock: the listener: {e}");
            }
        });
        let old = self
            .vsock
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .replace((inode, task.abort_handle()));
        if let Some((_, old)) = old {
            old.abort();
        }
        log::info!("vsock: FeID's signer serves on a new listener");
    }

    fn vsock_inode(&self) -> Option<u64> {
        self.vsock
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .as_ref()
            .map(|(inode, _)| *inode)
            .filter(|&inode| inode != 0)
    }

    /// The pod's identity in place, if any.
    pub fn identity(&self) -> Option<Arc<Identity>> {
        self.identity_tx.borrow().clone()
    }

    /// Serves the control socket, the listening fd `feosd` handed over,
    /// for as long as the process runs.
    pub async fn run(self: Arc<Self>, listener: OwnedFd) -> io::Result<()> {
        let listener = Seq::new(listener)?;
        let mut session: Option<(Seq, bool)> = None;
        let mut fetch = Fetch {
            want: true,
            inflight: false,
            again: false,
            retry_at: None,
            backoff: BACKOFF_FIRST,
            renew_at: None,
        };
        let mut buf = vec![0u8; MAX_MESSAGE];
        enum Event {
            Accepted(io::Result<OwnedFd>),
            Message(io::Result<Option<(usize, Vec<OwnedFd>)>>),
            Retry,
            Renew,
        }
        loop {
            let ev = {
                let recv = async {
                    match &session {
                        Some((s, _)) => s.recv(&mut buf).await,
                        None => std::future::pending().await,
                    }
                };
                let retry = async {
                    match fetch.retry_at {
                        Some(at) => tokio::time::sleep_until(at).await,
                        None => std::future::pending().await,
                    }
                };
                let renew = async {
                    match fetch.renew_at {
                        Some(at) => tokio::time::sleep_until(at).await,
                        None => std::future::pending().await,
                    }
                };
                tokio::select! {
                    r = listener.accept() => Event::Accepted(r),
                    r = recv => Event::Message(r),
                    _ = retry => Event::Retry,
                    _ = renew => Event::Renew,
                }
            };
            match ev {
                Event::Accepted(Ok(fd)) => match Seq::new(fd) {
                    Ok(s) => {
                        if session.is_some() {
                            log::info!("control: a new connection from feosd replaces the old");
                        }
                        session = Some((s, false));
                        if fetch.inflight {
                            fetch.inflight = false;
                            fetch.want = true;
                        }
                    }
                    Err(e) => log::error!("control: a new connection: {e}"),
                },
                Event::Accepted(Err(e)) => {
                    log::error!("control: accept: {e}");
                    tokio::time::sleep(Duration::from_millis(100)).await;
                }
                Event::Message(Ok(Some((n, fds)))) => {
                    let keep = match &session {
                        Some((s, hello)) => {
                            let mut hello = *hello;
                            let keep = self
                                .message(s, &mut hello, &buf[..n], fds, &mut fetch)
                                .await;
                            if let Some((_, h)) = session.as_mut() {
                                *h = hello;
                            }
                            keep
                        }
                        None => true,
                    };
                    if !keep {
                        session = None;
                        if fetch.inflight {
                            fetch.inflight = false;
                            fetch.want = true;
                        }
                    }
                }
                Event::Message(r) => {
                    if let Err(e) = r {
                        log::warn!("control: the connection from feosd: {e}");
                    } else {
                        log::info!("control: feosd closed its connection");
                    }
                    session = None;
                    if fetch.inflight {
                        fetch.inflight = false;
                        fetch.want = true;
                    }
                }
                Event::Retry => fetch.retry_at = None,
                Event::Renew => {
                    log::info!("control: the pod's certificate is due for renewal");
                    fetch.renew_at = None;
                    if fetch.inflight {
                        fetch.again = true;
                    } else {
                        fetch.want = true;
                    }
                }
            }
            if let Some((s, true)) = &session
                && fetch.want
                && !fetch.inflight
                && fetch.retry_at.is_none()
            {
                match self.ask(s).await {
                    Ok(()) => {
                        fetch.inflight = true;
                        fetch.again = false;
                    }
                    Err(e) => {
                        log::warn!("control: ask for credentials: {e}");
                        session = None;
                    }
                }
            }
        }
    }

    async fn ask(&self, s: &Seq) -> io::Result<()> {
        let b64 = base64::engine::general_purpose::STANDARD;
        let msg = json!({
            "v": 1,
            "op": "credentials",
            "hpkePublicKey": b64.encode(self.keys.hpke_public()),
            "csr": b64.encode(self.keys.csr_der()),
        });
        s.send(msg.to_string().as_bytes()).await
    }

    async fn send(s: &Seq, msg: Value) {
        if let Err(e) = s.send(msg.to_string().as_bytes()).await {
            log::warn!("control: send to feosd: {e}");
        }
    }

    /// Handles one message; false closes the connection.
    async fn message(
        &self,
        s: &Seq,
        hello: &mut bool,
        raw: &[u8],
        fds: Vec<OwnedFd>,
        fetch: &mut Fetch,
    ) -> bool {
        let env: Envelope = match serde_json::from_slice(raw) {
            Ok(e) => e,
            Err(e) => {
                log::warn!("control: a message that is no JSON object: {e}");
                return true;
            }
        };
        if env.v != 1 {
            log::warn!("control: a message of version {}; ignored", env.v);
            return true;
        }
        if env.op != "conn" && env.op != "vsock" && !fds.is_empty() {
            log::warn!("control: descriptors with {:?}; closed", env.op);
        }
        if !*hello && env.op != "hello" {
            log::warn!("control: {:?} before hello; ignored", env.op);
            return true;
        }
        match env.op.as_str() {
            "hello" => {
                let h: Hello = match serde_json::from_slice(raw) {
                    Ok(h) => h,
                    Err(e) => {
                        log::error!("control: hello: {e}");
                        return false;
                    }
                };
                if !h.pod_id.eq_ignore_ascii_case(&self.pod_id) {
                    log::error!(
                        "control: a hello for pod {}, but this process serves {}; closed",
                        h.pod_id,
                        self.pod_id
                    );
                    return false;
                }
                match net_config(&h) {
                    Ok(c) => self.net.set(Some(c)),
                    Err(e) => {
                        // Fail closed: nothing is dialed without the rule.
                        log::error!("control: hello: {e}; nothing is dialed until a valid one");
                        self.net.set(None);
                    }
                }
                *hello = true;
                if let Some(snap) = self.snapshot() {
                    Self::send(s, json!({"v": 1, "op": "ready", "version": snap.version})).await;
                }
                if let Some(inode) = self.vsock_inode() {
                    Self::send(s, json!({"v": 1, "op": "vsock", "inode": inode})).await;
                }
                true
            }
            "credentials" => {
                fetch.inflight = false;
                let c: Credentials = match serde_json::from_slice(raw) {
                    Ok(c) => c,
                    Err(e) => {
                        log::error!("control: credentials: {e}");
                        fetch.fail();
                        return true;
                    }
                };
                match self.load(c) {
                    Ok(version) => {
                        fetch.backoff = BACKOFF_FIRST;
                        fetch.want = fetch.again;
                        fetch.again = false;
                        fetch.renew_at = self.identity().map(|id| renew_at(&id));
                        Self::send(s, json!({"v": 1, "op": "ready", "version": version})).await;
                    }
                    Err(e) => {
                        log::error!("control: {e}");
                        Self::send(s, json!({"v": 1, "op": "error", "message": e})).await;
                        fetch.fail();
                    }
                }
                true
            }
            "error" => {
                let m = serde_json::from_slice::<ErrorMsg>(raw)
                    .map(|m| m.message)
                    .unwrap_or_default();
                log::warn!(
                    "control: no credentials: {m}; asking again in {:?}",
                    fetch.backoff
                );
                fetch.fail();
                true
            }
            "refetch" => {
                if fetch.inflight {
                    fetch.again = true;
                } else {
                    fetch.want = true;
                }
                true
            }
            "conn" => {
                let c: Conn = match serde_json::from_slice(raw) {
                    Ok(c) => c,
                    Err(e) => {
                        log::warn!("control: conn: {e}");
                        return true;
                    }
                };
                let mut fds = fds.into_iter();
                match (fds.next(), fds.next()) {
                    (Some(fd), None) => self.serve(c, fd),
                    _ => log::warn!("control: a conn without exactly one descriptor; closed"),
                }
                true
            }
            "vsock" => {
                let inode = serde_json::from_slice::<Vsock>(raw)
                    .map(|v| v.inode)
                    .unwrap_or_default();
                let mut fds = fds.into_iter();
                match (fds.next(), fds.next()) {
                    (Some(fd), None) => {
                        self.serve_vsock(fd, inode);
                        if inode != 0 {
                            Self::send(s, json!({"v": 1, "op": "vsock", "inode": inode})).await;
                        }
                    }
                    _ => log::warn!("control: a vsock without exactly one descriptor; closed"),
                }
                true
            }
            other => {
                log::debug!("control: unknown op {other:?}; ignored");
                true
            }
        }
    }

    /// Puts an answer's certificate in place, when it is one for this
    /// run's key; one that is not leaves the one in place, which serves
    /// until it expires.
    fn load_identity(&self, certificate: Option<&Value>) {
        let Some(certificate) = certificate else {
            return;
        };
        match Identity::from_relayed(certificate, &self.keys) {
            Ok(id) => {
                log::info!(
                    "identity: {} until {}, renewed after {}",
                    id.spiffe_id,
                    id.not_after,
                    id.renew_after
                );
                self.identity_tx.send_replace(Some(Arc::new(id)));
            }
            Err(e) => log::error!("identity: the certificate is not taken: {e}"),
        }
    }

    /// Opens an answer's bindings and puts the set in place.
    fn load(&self, c: Credentials) -> Result<String, String> {
        let (set, version) = match &c.bindings {
            Some(sb) => (
                bindings::open(
                    &self.keys,
                    &self.pod_id,
                    &sb.enc,
                    &sb.ciphertext,
                    &sb.version,
                )?,
                sb.version.clone(),
            ),
            None => (Default::default(), String::new()),
        };
        self.load_identity(c.certificate.as_ref());
        let snap = bindings::snapshot(set, version.clone(), c.certificate, &self.net, &self.audit);
        for (name, why) in &snap.invalid {
            log::error!("bindings {version}: {name}: {why}");
        }
        let mut names: Vec<&str> = snap
            .by_name
            .values()
            .chain(snap.by_host.values())
            .map(|b| b.name.as_str())
            .collect();
        names.sort();
        let rec = BindingsRecord {
            ts: audit::now(),
            kind: "bindings",
            version: &version,
            http: names,
            invalid: snap
                .invalid
                .iter()
                .map(|(b, m)| InvalidBinding {
                    binding: b,
                    message: m,
                })
                .collect(),
        };
        if let Err(e) = self.audit.write(&rec) {
            log::error!("audit: the record of a binding set: {e}");
        }
        log::info!(
            "bindings {version}: {} HTTP, {} Kubernetes, {} SSH",
            snap.by_name.len() + snap.by_host.len(),
            snap.kube.len(),
            snap.ssh.len()
        );
        self.ready_tx.send_replace(Some(Arc::new(snap)));
        Ok(version)
    }

    /// Serves a connection of the pod's that `feosd` passed on.
    fn serve(&self, c: Conn, fd: OwnedFd) {
        let Ok(permit) = self.conns.clone().try_acquire_owned() else {
            log::warn!("too many of the pod's connections; closed one");
            return;
        };
        let stream = std::net::TcpStream::from(fd);
        let stream = match stream
            .set_nonblocking(true)
            .and_then(|_| tokio::net::TcpStream::from_std(stream))
        {
            Ok(s) => s,
            Err(e) => {
                log::warn!("a connection from {:?}: {e}", c.peer);
                return;
            }
        };
        let _ = stream.set_nodelay(true);
        let ctx = self.ctx.clone();
        tokio::spawn(async move {
            let _permit = permit;
            match c.port {
                80 => proxy::serve_name(ctx, stream).await,
                443 => intercept::serve(ctx, stream).await,
                p => log::warn!("a connection to port {p}; closed"),
            }
        });
    }
}

/// When to renew `id`: at its `renewAfter`, or at three quarters of its
/// lifetime when that is missing; at once when the time has passed.
fn renew_at(id: &Identity) -> Instant {
    let after = match id.renew_after {
        0 => id.not_before + (id.not_after - id.not_before) * 3 / 4,
        at => at,
    };
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs() as i64);
    Instant::now() + Duration::from_secs(after.saturating_sub(now).max(0) as u64)
}

fn net_config(h: &Hello) -> Result<NetConfig, String> {
    let deny = h
        .deny_prefixes
        .iter()
        .map(|p| p.parse::<Prefix>())
        .collect::<Result<Vec<_>, _>>()?;
    let source_v4 = match h.source_v4.as_deref() {
        None | Some("") => None,
        Some(s) => Some(
            s.parse::<Ipv4Addr>()
                .map_err(|_| format!("sourceV4 {s:?} is no IPv4 address"))?,
        ),
    };
    let source_v6 = match h.source_v6.as_deref() {
        None | Some("") => None,
        Some(s) => Some(
            s.parse::<Ipv6Addr>()
                .map_err(|_| format!("sourceV6 {s:?} is no IPv6 address"))?,
        ),
    };
    Ok(NetConfig {
        deny,
        source_v4,
        source_v6,
    })
}
