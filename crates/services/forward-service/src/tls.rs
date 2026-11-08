// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Outbound TLS bindings (ADR 0036): for a client without FeID, as `psql`
//! or an application's PostgreSQL driver, the guest listens on a loopback
//! address of the pod and, for every connection, dials the binding's
//! upstream from the pod's own address with TLS 1.3 from the first byte:
//! the pod's certificate, its CertificateVerify signed by feos-authn
//! through FeID's socket, the upstream pinned to its SPIFFE ID under the
//! platform's root alone, the upstream's ALPN. Then it pipes both
//! directions. The client speaks plaintext on loopback, which never leaves
//! the pod; the key is never in it.
//!
//! The bindings run in a process of their own, `feos-guest --tls-bindings
//! <file>`, so that a fault in TLS never takes PID 1 down (ADR 0007's rule
//! for the container runtime). PID 1 binds the bindings' listeners itself,
//! before it answers the set, writes the file, which names each binding's
//! listener by its descriptor, and starts the process, which inherits
//! them; it starts the process again when it ends. A set that changes the
//! bindings, or resets them after a resume, ends the process before and
//! starts another, and keeps the listener of every address that stays.
//! So a container, which the host starts after the set, never finds a
//! binding's port closed: a connection that comes while the process
//! starts, restarts or waits for the pod's signer waits in its listener's
//! backlog. The process reads the file and takes its listeners as root,
//! then puts itself in its [`jail`] before it serves: a user of its own,
//! no capabilities, a root that holds the share directory alone, a seccomp
//! allowlist. It accepts once the signer has given it the pod's
//! certificate. A process that cannot serves nothing.

use std::io;
use std::net::{SocketAddr, SocketAddrV4};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use log::{error, info, warn};
use serde::{Deserialize, Serialize};

pub mod jail;

/// The flag that makes `feos-guest` the bindings' process.
pub const FLAG: &str = "--tls-bindings";

/// Where PID 1 keeps the bindings' file and their process's pid: FeOS's own
/// directory, which containers never see.
const DIR: &str = "/run/feos";
const CONFIG: &str = "tls-bindings.json";
const PID_FILE: &str = "tls-bindings.pid";
const RESTART_DELAY: Duration = Duration::from_secs(1);
/// How long an ended process has to let its listeners go.
const STOP_TIMEOUT: Duration = Duration::from_secs(2);
/// How long a dial of the upstream may take, its name's lookup included.
const DIAL_TIMEOUT: Duration = Duration::from_secs(10);
/// How long the upstream's handshake may take.
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);
/// How often the process asks FeID's socket again while the pod's signer
/// does not answer yet: from the first delay, doubling, to the last.
const SIGNER_RETRY_FIRST: Duration = Duration::from_millis(50);
const SIGNER_RETRY_LAST: Duration = Duration::from_secs(1);

/// One binding, as the host pushes it (`SetFeid`'s `tls_bindings`).
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Binding {
    /// The loopback address and port the guest listens on, `127.x.y.z:p`.
    pub listen: String,
    /// The upstream: a name the pod's resolver answers, or an address.
    pub host: String,
    pub port: u16,
    /// The ALPN protocol the guest offers, `postgresql`; empty: none, for
    /// an upstream that has no protocol ID (Valkey, ADR 0039).
    pub alpn: String,
    /// The SPIFFE ID the upstream's certificate must carry.
    pub server_id: String,
}

impl Binding {
    /// Why the binding cannot be served, if it cannot.
    pub fn check(&self) -> Result<(), String> {
        let listen: SocketAddr = self
            .listen
            .parse()
            .map_err(|_| format!("listen {:?} is no address and port", self.listen))?;
        if !listen.ip().is_loopback() || !listen.is_ipv4() || listen.port() == 0 {
            return Err(format!(
                "listen {} is not a port of an IPv4 loopback address",
                self.listen
            ));
        }
        if self.host.is_empty() || self.host.len() > 253 || self.port == 0 {
            return Err(format!("upstream {}:{} is not one", self.host, self.port));
        }
        if self.alpn.len() > 255 {
            return Err(format!("ALPN {:?} is longer than 255 bytes", self.alpn));
        }
        if feid::SpiffeId::parse(&self.server_id).is_none() {
            return Err(format!("server ID {:?} is no SPIFFE ID", self.server_id));
        }
        Ok(())
    }
}

/// A binding in the process's file: the descriptor of its listener, which
/// the process inherits from PID 1.
#[derive(Debug, Serialize, Deserialize)]
pub struct Inherited {
    pub binding: Binding,
    pub fd: i32,
}

/// PID 1's side: the bindings the host pushed, their listeners, and their
/// process.
pub struct TlsBindings {
    dir: PathBuf,
    /// The binary to run; `None`: this one.
    binary: Option<PathBuf>,
    state: Arc<Mutex<State>>,
    /// One set at a time: a set waits for the process before to end.
    sets: tokio::sync::Mutex<()>,
}

#[derive(Default)]
struct State {
    bindings: Vec<Binding>,
    /// Each binding's listener, by its address, in the bindings' order.
    listeners: Vec<(String, Arc<std::net::TcpListener>)>,
    /// Counts the sets that changed the process: a supervisor of an older
    /// one stops.
    generation: u64,
    pid: Option<i32>,
}

impl Default for TlsBindings {
    fn default() -> Self {
        TlsBindings::with_parts(DIR.into(), None)
    }
}

impl TlsBindings {
    /// Tests keep the files in a directory of their own and run a binary
    /// of their own.
    pub fn with_parts(dir: PathBuf, binary: Option<PathBuf>) -> Self {
        TlsBindings {
            dir,
            binary,
            state: Arc::new(Mutex::new(State::default())),
            sets: tokio::sync::Mutex::new(()),
        }
    }

    /// Serves `bindings` from now on, none for an empty list: when it
    /// returns, each binding's address listens. A set of the bindings that
    /// are served already changes nothing, unless `reset`, after a boot or
    /// a resume, which ends their connections too. Returns how many are
    /// served.
    pub async fn set(&self, bindings: Vec<Binding>, reset: bool) -> Result<usize, String> {
        for b in &bindings {
            b.check()?;
        }
        let _set = self.sets.lock().await;
        let previous = {
            let mut st = self.state.lock().unwrap_or_else(|e| e.into_inner());
            let running = st.pid.is_some_and(alive);
            if st.bindings == bindings && !reset && (running || bindings.is_empty()) {
                return Ok(bindings.len());
            }
            st.generation += 1;
            st.bindings = bindings.clone();
            st.pid.take()
        };
        // The process before, this one's or one that a guest before a live
        // upgrade started, lets its listeners go first.
        let stale = previous.or_else(|| read_pid(&self.dir.join(PID_FILE)));
        if let Some(pid) = stale {
            stop(pid).await;
        }
        if bindings.is_empty() {
            self.lock().listeners.clear();
            let _ = std::fs::remove_file(self.dir.join(CONFIG));
            let _ = std::fs::remove_file(self.dir.join(PID_FILE));
            info!("TlsBindings: The pod has no TLS bindings");
            return Ok(0);
        }
        let listeners = self.listen(&bindings)?;
        let inherited: Vec<Inherited> = bindings
            .iter()
            .zip(&listeners)
            .map(|(b, l)| Inherited {
                binding: b.clone(),
                fd: l.as_raw_fd(),
            })
            .collect();
        write_config(&self.dir, &inherited).map_err(|e| format!("TLS bindings' file: {e}"))?;
        let generation = self.lock().generation;
        let supervisor = Supervisor {
            dir: self.dir.clone(),
            binary: self.binary.clone(),
            state: self.state.clone(),
            generation,
        };
        tokio::spawn(supervisor.run());
        info!(
            "TlsBindings: The pod has {} TLS binding(s): {}",
            bindings.len(),
            bindings
                .iter()
                .map(|b| format!("{} → {}:{} ({})", b.listen, b.host, b.port, b.server_id))
                .collect::<Vec<_>>()
                .join(", ")
        );
        Ok(bindings.len())
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// The listeners of `bindings`, in their order: an address keeps the
    /// listener it had, so that its waiting connections stay, and a new
    /// one is bound; the listeners of addresses no binding names any more
    /// close. When one cannot be bound, none listens.
    fn listen(&self, bindings: &[Binding]) -> Result<Vec<Arc<std::net::TcpListener>>, String> {
        let mut st = self.lock();
        let mut before = std::mem::take(&mut st.listeners);
        let mut now = Vec::with_capacity(bindings.len());
        for b in bindings {
            let listener = match before.iter().position(|(a, _)| *a == b.listen) {
                Some(i) => before.swap_remove(i).1,
                None => match bind(&b.listen) {
                    Ok(l) => Arc::new(l),
                    Err(e) => {
                        st.bindings.clear();
                        return Err(format!("listen on {}: {e}", b.listen));
                    }
                },
            };
            now.push((b.listen.clone(), listener));
        }
        st.listeners = now.clone();
        Ok(now.into_iter().map(|(_, l)| l).collect())
    }
}

/// Runs the bindings' process of one generation, again whenever it ends,
/// until a set makes another.
struct Supervisor {
    dir: PathBuf,
    binary: Option<PathBuf>,
    state: Arc<Mutex<State>>,
    generation: u64,
}

impl Supervisor {
    async fn run(self) {
        loop {
            if !self.current() {
                return;
            }
            if let Err(e) = self.run_once().await {
                error!("TlsBindings: Failed to run the TLS bindings: {e}");
            }
            tokio::time::sleep(RESTART_DELAY).await;
        }
    }

    fn current(&self) -> bool {
        self.state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .generation
            == self.generation
    }

    async fn run_once(&self) -> io::Result<()> {
        let binary = match &self.binary {
            Some(b) => b.clone(),
            None => std::env::current_exe()?,
        };
        let config = self.dir.join(CONFIG);
        let mut command = std::process::Command::new(&binary);
        command.arg(FLAG).arg(&config).stdin(Stdio::null());
        let reaped = feos_utils::reaper::is_running();
        // The process starts under the state's lock, with the listeners
        // its file names, and is recorded before the lock goes: a set
        // that replaces them either comes first, and this generation
        // starts nothing, or finds the process and ends it.
        let (pid, child) = {
            let mut st = self.state.lock().unwrap_or_else(|e| e.into_inner());
            if st.generation != self.generation {
                return Ok(());
            }
            // The process inherits the listeners, which are close-on-exec
            // in PID 1: the flag goes in the child alone, between fork and
            // exec.
            let fds: Vec<i32> = st.listeners.iter().map(|(_, l)| l.as_raw_fd()).collect();
            // SAFETY: fcntl(2) is async-signal-safe, and the closure
            // allocates nothing; the descriptors are open: the state holds
            // them, under the lock.
            unsafe {
                command.pre_exec(move || {
                    for &fd in &fds {
                        if libc::fcntl(fd, libc::F_SETFD, 0) == -1 {
                            return Err(io::Error::last_os_error());
                        }
                    }
                    Ok(())
                });
            }
            let (pid, child) = if reaped {
                // The reaper owns waitpid in PID 1: it gives the exit code.
                (command.spawn()?.id() as i32, None)
            } else {
                let child = tokio::process::Command::from(command).spawn()?;
                (child.id().unwrap_or_default() as i32, Some(child))
            };
            st.pid = Some(pid);
            (pid, child)
        };
        if let Err(e) = std::fs::write(self.dir.join(PID_FILE), pid.to_string()) {
            warn!("TlsBindings: Failed to write the pid file: {e}");
        }
        info!("TlsBindings: Started the TLS bindings' process (pid {pid})");
        match child {
            None => {
                let code = feos_utils::reaper::wait_for_exit(pid).await;
                warn!("TlsBindings: The TLS bindings' process (pid {pid}) exited with code {code}");
            }
            Some(mut child) => {
                let status = child.wait().await?;
                warn!("TlsBindings: The TLS bindings' process (pid {pid}) exited: {status}");
            }
        }
        Ok(())
    }
}

fn write_config(dir: &Path, bindings: &[Inherited]) -> io::Result<()> {
    use std::os::unix::fs::OpenOptionsExt;
    std::fs::create_dir_all(dir)?;
    let tmp = dir.join(format!(".{CONFIG}.new"));
    let json = serde_json::to_vec(bindings).map_err(io::Error::other)?;
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(&tmp)?;
    io::Write::write_all(&mut f, &json)?;
    f.sync_all()?;
    std::fs::rename(&tmp, dir.join(CONFIG))
}

fn read_pid(path: &Path) -> Option<i32> {
    let pid: i32 = std::fs::read_to_string(path).ok()?.trim().parse().ok()?;
    // Only a process that is the bindings' one: a pid is reused.
    let cmdline = std::fs::read(format!("/proc/{pid}/cmdline")).ok()?;
    cmdline
        .split(|b| *b == 0)
        .any(|arg| arg == FLAG.as_bytes())
        .then_some(pid)
}

fn alive(pid: i32) -> bool {
    Path::new(&format!("/proc/{pid}")).exists()
}

fn signal(pid: i32, sig: nix::sys::signal::Signal) {
    let _ = nix::sys::signal::kill(nix::unistd::Pid::from_raw(pid), sig);
}

/// Ends a process: SIGTERM, then SIGKILL after [`STOP_TIMEOUT`], and waits
/// until it is gone, so that its listeners are free.
async fn stop(pid: i32) {
    signal(pid, nix::sys::signal::Signal::SIGTERM);
    let deadline = tokio::time::Instant::now() + STOP_TIMEOUT;
    while alive(pid) && !zombie(pid) {
        if tokio::time::Instant::now() >= deadline {
            signal(pid, nix::sys::signal::Signal::SIGKILL);
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    // A killed process lets its sockets go when it exits, before its
    // parent reaps it.
    let deadline = tokio::time::Instant::now() + STOP_TIMEOUT;
    while alive(pid) && !zombie(pid) && tokio::time::Instant::now() < deadline {
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

/// Whether a process has exited and waits only to be reaped.
fn zombie(pid: i32) -> bool {
    std::fs::read_to_string(format!("/proc/{pid}/stat"))
        .ok()
        .and_then(|s| {
            s.rsplit_once(')')
                .map(|(_, rest)| rest.trim_start().starts_with('Z'))
        })
        .unwrap_or(true)
}

/// The bindings of a file, each with its listener.
pub struct Listeners(Vec<(Binding, std::net::TcpListener)>);

/// How many connections wait to be accepted, as tokio's own listeners.
const BACKLOG: i32 = 1024;

/// The bindings' process, first: reads `config`, which only root reads:
/// the bindings, each with the descriptor of the listener PID 1 bound.
pub fn read_config(config: &Path) -> Result<Vec<Inherited>, String> {
    serde_json::from_slice(
        &std::fs::read(config).map_err(|e| format!("{}: {e}", config.display()))?,
    )
    .map_err(|e| format!("{}: {e}", config.display()))
}

/// The bindings' process, once it closed every other descriptor: takes
/// each binding's listener, which must be an open, listening TCP socket on
/// the binding's address.
pub fn adopt(inherited: Vec<Inherited>) -> Result<Listeners, String> {
    let mut listeners = Vec::with_capacity(inherited.len());
    for Inherited { binding, fd } in inherited {
        binding.check()?;
        // Only an open descriptor becomes one this process owns.
        // SAFETY: fcntl(2) on a number, which changes nothing.
        if fd < 3 || unsafe { libc::fcntl(fd, libc::F_GETFD) } == -1 {
            return Err(format!("{}: descriptor {fd} is not open", binding.listen));
        }
        // SAFETY: an open descriptor that PID 1 passed for this binding
        // alone: from here this process owns it, and nothing else does.
        let fd = unsafe { OwnedFd::from_raw_fd(fd) };
        let listener = adopted(fd, &binding.listen)
            .map_err(|e| format!("{}: the inherited listener: {e}", binding.listen))?;
        listeners.push((binding, listener));
    }
    Ok(Listeners(listeners))
}

/// `fd` as the listener of `listen`, if it is one.
fn adopted(fd: OwnedFd, listen: &str) -> io::Result<std::net::TcpListener> {
    use nix::sys::socket::{getsockname, getsockopt, sockopt, SockaddrIn};
    if !getsockopt(&fd, sockopt::AcceptConn)? {
        return Err(io::Error::other("it does not listen"));
    }
    let local = SocketAddrV4::from(getsockname::<SockaddrIn>(fd.as_raw_fd())?);
    let want: SocketAddrV4 = listen.parse().map_err(io::Error::other)?;
    if local != want {
        return Err(io::Error::other(format!("it listens on {local}")));
    }
    let listener = std::net::TcpListener::from(fd);
    // Nothing this process might start gets it.
    // SAFETY: fcntl(2) on a descriptor the listener owns.
    if unsafe { libc::fcntl(listener.as_raw_fd(), libc::F_SETFD, libc::FD_CLOEXEC) } == -1 {
        return Err(io::Error::last_os_error());
    }
    listener.set_nonblocking(true)?;
    Ok(listener)
}

/// A non-blocking, close-on-exec listener on an IPv4 address and port,
/// which [`Binding::check`] passed: PID 1's.
fn bind(address: &str) -> io::Result<std::net::TcpListener> {
    use nix::sys::socket::{
        bind, setsockopt, socket, sockopt, AddressFamily, SockFlag, SockType, SockaddrIn,
    };
    let address: SocketAddrV4 = address.parse().map_err(io::Error::other)?;
    let fd = socket(
        AddressFamily::Inet,
        SockType::Stream,
        SockFlag::SOCK_CLOEXEC | SockFlag::SOCK_NONBLOCK,
        None,
    )?;
    setsockopt(&fd, sockopt::ReuseAddr, &true)?;
    bind(fd.as_raw_fd(), &SockaddrIn::from(address))?;
    // Not nix's listen: its Backlog stops at the libc's SOMAXCONN, 128 in
    // musl; the kernel's net.core.somaxconn caps it.
    // SAFETY: listen(2) on a socket this function owns.
    if unsafe { libc::listen(fd.as_raw_fd(), BACKLOG) } != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(std::net::TcpListener::from(fd))
}

/// The bindings' process, in its jail: serves `listeners` until one of
/// them cannot serve any more, which ends the process (PID 1 starts it
/// again). It accepts once the pod's signer answers through FeID's socket
/// with the pod's certificate; until then, while the pod's feos-authn
/// starts, connections wait in the listeners' backlogs.
pub async fn serve(listeners: Listeners, feid_socket: &Path) -> Result<(), String> {
    let bindings: Vec<&Binding> = listeners.0.iter().map(|(b, _)| b).collect();
    let (feid, configs) = signed(&bindings, feid_socket).await;
    let mut tasks = tokio::task::JoinSet::new();
    for ((b, listener), config) in listeners.0.into_iter().zip(configs) {
        let listener = tokio::net::TcpListener::from_std(listener)
            .map_err(|e| format!("listen on {}: {e}", b.listen))?;
        let alpn = if b.alpn.is_empty() {
            "no ALPN"
        } else {
            &b.alpn
        };
        info!(
            "TlsBindings: {} → {}:{} as {} ({alpn})",
            b.listen, b.host, b.port, b.server_id
        );
        tasks.spawn(accept(
            feid.clone(),
            Arc::new(config),
            Arc::new(b),
            listener,
        ));
    }
    match tasks.join_next().await {
        Some(Ok(e)) => Err(e),
        Some(Err(e)) => Err(format!("a binding's task: {e}")),
        None => Ok(()),
    }
}

/// FeID's client and each binding's configuration, once the pod's signer
/// gives the pod's certificate and the platform's bundle: asked again,
/// with a growing delay, until it does.
async fn signed(
    bindings: &[&Binding],
    socket: &Path,
) -> (Arc<feid::Feid>, Vec<rustls::ClientConfig>) {
    let since = tokio::time::Instant::now();
    let mut delay = SIGNER_RETRY_FIRST;
    let mut last = String::new();
    loop {
        match configs(bindings, socket).await {
            Ok((feid, id, configs)) => {
                info!(
                    "TlsBindings: The pod is {id}, after {} ms",
                    since.elapsed().as_millis()
                );
                return (feid, configs);
            }
            // Said once, and again when it says something else.
            Err(e) if e != last => {
                warn!("TlsBindings: Waiting for the pod's signer: {e}");
                last = e;
            }
            Err(_) => {}
        }
        tokio::time::sleep(delay).await;
        delay = (delay * 2).min(SIGNER_RETRY_LAST);
    }
}

/// One attempt of [`signed`]: the client, the pod's ID and the
/// configurations. FeID's calls block.
async fn configs(
    bindings: &[&Binding],
    socket: &Path,
) -> Result<(Arc<feid::Feid>, feid::SpiffeId, Vec<rustls::ClientConfig>), String> {
    let path = socket.to_path_buf();
    let feid = tokio::task::spawn_blocking(move || feid::Feid::with_socket(path))
        .await
        .map_err(|e| format!("FeID: {e}"))?
        .map_err(|e| format!("FeID's socket {}: {e}", socket.display()))?;
    let feid = Arc::new(feid);
    let f = feid.clone();
    let id = tokio::task::spawn_blocking(move || f.id())
        .await
        .map_err(|e| format!("FeID: {e}"))?
        .map_err(|e| format!("the pod's certificate: {e}"))?;
    let mut configs = Vec::with_capacity(bindings.len());
    for b in bindings {
        let server = feid::SpiffeId::parse(&b.server_id)
            .ok_or_else(|| format!("server ID {:?} is no SPIFFE ID", b.server_id))?;
        let f = feid.clone();
        let mut config = (*tokio::task::spawn_blocking(move || f.client_config_for(&server))
            .await
            .map_err(|e| format!("FeID: {e}"))?
            .map_err(|e| format!("FeID's client configuration: {e}"))?)
        .clone();
        // An empty ALPN offers none: rustls then sends no extension.
        config.alpn_protocols = if b.alpn.is_empty() {
            Vec::new()
        } else {
            vec![b.alpn.clone().into_bytes()]
        };
        configs.push(config);
    }
    Ok((feid, id, configs))
}

async fn accept(
    feid: Arc<feid::Feid>,
    config: Arc<rustls::ClientConfig>,
    binding: Arc<Binding>,
    listener: tokio::net::TcpListener,
) -> String {
    loop {
        let (client, _) = match listener.accept().await {
            Ok(c) => c,
            Err(e) => {
                // Out of descriptors or memory: wait, as a server does.
                warn!("TlsBindings: {}: accept: {e}", binding.listen);
                tokio::time::sleep(Duration::from_millis(100)).await;
                continue;
            }
        };
        let (feid, config, binding) = (feid.clone(), config.clone(), binding.clone());
        tokio::spawn(async move {
            if let Err(e) = pipe(&feid, config, &binding, client).await {
                warn!(
                    "TlsBindings: {} → {}:{}: {e}",
                    binding.listen, binding.host, binding.port
                );
            }
        });
    }
}

/// One connection: the upstream's TLS, then the bytes both ways.
async fn pipe(
    feid: &feid::Feid,
    config: Arc<rustls::ClientConfig>,
    b: &Binding,
    mut client: tokio::net::TcpStream,
) -> Result<(), String> {
    let upstream = tokio::time::timeout(DIAL_TIMEOUT, dial(&b.host, b.port))
        .await
        .map_err(|_| format!("no connection within {}s", DIAL_TIMEOUT.as_secs()))??;
    let name = rustls::pki_types::ServerName::try_from(b.host.clone())
        .map_err(|e| format!("server name {}: {e}", b.host))?;
    let mut tls = tokio::time::timeout(HANDSHAKE_TIMEOUT, feid.connect(config, name, upstream))
        .await
        .map_err(|_| format!("no handshake within {}s", HANDSHAKE_TIMEOUT.as_secs()))?
        .map_err(|e| format!("handshake: {e}"))?;
    tokio::io::copy_bidirectional(&mut client, &mut tls)
        .await
        .map(|_| ())
        .map_err(|e| format!("pipe: {e}"))
}

/// Dials the upstream at its every address in turn, as the resolver gives
/// them.
async fn dial(host: &str, port: u16) -> Result<tokio::net::TcpStream, String> {
    let addrs: Vec<SocketAddr> = tokio::net::lookup_host((host, port))
        .await
        .map_err(|e| format!("resolve {host}: {e}"))?
        .collect();
    let mut last = format!("{host} has no address");
    for addr in addrs {
        match tokio::net::TcpStream::connect(addr).await {
            Ok(s) => {
                let _ = s.set_nodelay(true);
                return Ok(s);
            }
            Err(e) => last = format!("connect {addr}: {e}"),
        }
    }
    Err(last)
}

#[cfg(test)]
mod tests {
    use super::*;
    use rcgen::SigningKey;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    const POD: &str = "spiffe://run.example/tenant/acme/env/e1/service/api";
    const ADDON: &str = "spiffe://run.example/tenant/acme/env/e1/addon/db";

    fn binding(listen: &str) -> Binding {
        Binding {
            listen: listen.into(),
            host: "db".into(),
            port: 5432,
            alpn: "postgresql".into(),
            server_id: ADDON.into(),
        }
    }

    #[test]
    fn a_binding_listens_on_loopback_and_pins_a_spiffe_id() {
        binding("127.18.3.4:5432").check().unwrap();
        // An upstream without an ALPN protocol (Valkey): none offered.
        Binding {
            alpn: String::new(),
            ..binding("127.18.3.4:6379")
        }
        .check()
        .unwrap();
        for bad in [
            Binding {
                listen: "10.0.0.1:5432".into(),
                ..binding("")
            },
            Binding {
                listen: "[::1]:5432".into(),
                ..binding("")
            },
            Binding {
                listen: "127.0.0.1:0".into(),
                ..binding("")
            },
            Binding {
                listen: "localhost:5432".into(),
                ..binding("")
            },
            Binding {
                host: String::new(),
                ..binding("127.0.0.1:1")
            },
            Binding {
                port: 0,
                ..binding("127.0.0.1:1")
            },
            Binding {
                alpn: "x".repeat(256),
                ..binding("127.0.0.1:1")
            },
            Binding {
                server_id: "https://db".into(),
                ..binding("127.0.0.1:1")
            },
        ] {
            assert!(bad.check().is_err(), "{bad:?}");
        }
    }

    struct Ca {
        issuer: rcgen::Issuer<'static, rcgen::KeyPair>,
        pem: String,
    }

    impl Ca {
        fn new() -> Ca {
            let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
            let mut p = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            p.distinguished_name.push(rcgen::DnType::CommonName, "root");
            p.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
            p.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
            let pem = p.self_signed(&key).unwrap().pem();
            Ca {
                issuer: rcgen::Issuer::new(p, key),
                pem,
            }
        }

        /// A workload's key and certificate: its SPIFFE ID, no DNS name.
        fn workload(&self, spiffe: &str) -> (rcgen::KeyPair, String) {
            let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
            let mut p = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            p.subject_alt_names
                .push(rcgen::SanType::URI(spiffe.try_into().unwrap()));
            p.distinguished_name
                .push(rcgen::DnType::CommonName, "run.example/pod");
            p.extended_key_usages = vec![
                rcgen::ExtendedKeyUsagePurpose::ClientAuth,
                rcgen::ExtendedKeyUsagePurpose::ServerAuth,
            ];
            let now = time::OffsetDateTime::now_utc();
            p.not_before = now - time::Duration::minutes(1);
            p.not_after = now + time::Duration::hours(1);
            let pem = p.signed_by(&key, &self.issuer).unwrap().pem();
            (key, pem)
        }
    }

    /// The pod's signer on a Unix socket, as feos-authn behind FeID's
    /// socket.
    struct Signer {
        key: rcgen::KeyPair,
        chain: String,
        bundle: String,
    }

    impl feid_proto::Handler for Signer {
        async fn certificate(&self) -> Result<String, feid_proto::Refusal> {
            Ok(self.chain.clone())
        }
        async fn trust_bundle(&self) -> Result<String, feid_proto::Refusal> {
            Ok(self.bundle.clone())
        }
        async fn sign(
            &self,
            cv: feid_proto::CertificateVerify<'_>,
        ) -> Result<Vec<u8>, feid_proto::Refusal> {
            self.key
                .sign(cv.message)
                .map_err(|e| feid_proto::Refusal::internal(e.to_string()))
        }
        fn handshake_report(&self, _: feid_proto::HandshakeReport) {}
    }

    fn signer(dir: &Path, ca: &Ca) -> PathBuf {
        let (key, chain) = ca.workload(POD);
        let signer = Arc::new(Signer {
            key,
            chain,
            bundle: ca.pem.clone(),
        });
        let socket = dir.join("feid.sock");
        let listener = std::os::unix::net::UnixListener::bind(&socket).unwrap();
        listener.set_nonblocking(true).unwrap();
        // A runtime of its own, as feos-authn is a process of its own: FeID
        // asks it for the pod's certificate in a handshake, blocking the
        // worker that runs the handshake.
        std::thread::spawn(move || {
            let runtime = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap();
            runtime.block_on(async move {
                let listener = tokio::net::UnixListener::from_std(listener).unwrap();
                while let Ok((stream, _)) = listener.accept().await {
                    let signer = signer.clone();
                    tokio::spawn(async move {
                        let options = feid_proto::ServeOptions::named("test-signer");
                        feid_proto::serve(stream, signer.as_ref(), &options).await;
                    });
                }
            });
        });
        socket
    }

    /// The upstream, as PostgreSQL 17 with direct TLS: requires a client
    /// certificate under the root, takes ALPN postgresql, and answers
    /// each line with the client's URI and the line.
    async fn upstream(ca: &Ca, spiffe: &str) -> SocketAddr {
        upstream_with(ca, spiffe, Some(b"postgresql")).await
    }

    /// The upstream with alpn its one ALPN protocol, or none (as Valkey:
    /// it then asserts that the client offered none).
    async fn upstream_with(ca: &Ca, spiffe: &str, alpn: Option<&'static [u8]>) -> SocketAddr {
        use rustls::pki_types::pem::PemObject;
        use rustls::pki_types::{CertificateDer, PrivateKeyDer};
        let (key, pem) = ca.workload(spiffe);
        let chain = vec![CertificateDer::from_pem_slice(pem.as_bytes()).unwrap()];
        let key = PrivateKeyDer::from_pem_slice(key.serialize_pem().as_bytes()).unwrap();
        let mut roots = rustls::RootCertStore::empty();
        roots
            .add(CertificateDer::from_pem_slice(ca.pem.as_bytes()).unwrap())
            .unwrap();
        let verifier = rustls::server::WebPkiClientVerifier::builder(Arc::new(roots))
            .build()
            .unwrap();
        let mut config =
            rustls::ServerConfig::builder_with_protocol_versions(&[&rustls::version::TLS13])
                .with_client_cert_verifier(verifier)
                .with_single_cert(chain, key)
                .unwrap();
        config.alpn_protocols = alpn.iter().map(|a| a.to_vec()).collect();
        let acceptor = tokio_rustls::TlsAcceptor::from(Arc::new(config));
        let listener = tokio::net::TcpListener::bind("[::1]:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            while let Ok((tcp, _)) = listener.accept().await {
                let acceptor = acceptor.clone();
                tokio::spawn(async move {
                    let Ok(mut tls) = acceptor.accept(tcp).await else {
                        return;
                    };
                    let conn = tls.get_ref().1;
                    assert_eq!(conn.alpn_protocol(), alpn);
                    let peer = conn.peer_certificates().unwrap()[0].clone();
                    let uri = if peer.windows(POD.len()).any(|w| w == POD.as_bytes()) {
                        "pod"
                    } else {
                        "?"
                    };
                    let mut buf = [0u8; 4];
                    while tls.read_exact(&mut buf).await.is_ok() {
                        let _ = tls.write_all(format!("{uri}:").as_bytes()).await;
                        let _ = tls.write_all(&buf).await;
                    }
                });
            }
        });
        addr
    }

    fn free_loopback() -> String {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        l.local_addr().unwrap().to_string()
    }

    /// The bindings served as the process serves them, with listeners
    /// bound as PID 1 binds them: they listen when this returns.
    fn serving(socket: &Path, bindings: &[Binding]) {
        let listeners = bindings
            .iter()
            .map(|b| (b.clone(), bind(&b.listen).unwrap()))
            .collect();
        let socket = socket.to_path_buf();
        tokio::spawn(async move { serve(Listeners(listeners), &socket).await });
    }

    fn connect_refused(listen: &str) -> bool {
        std::net::TcpStream::connect(listen)
            .is_err_and(|e| e.kind() == io::ErrorKind::ConnectionRefused)
    }

    // PID 1 listens on a binding's address when its set returns, before
    // the process runs; a set that changes the upstream keeps the
    // address's listener; a set of none closes it. The file names each
    // listener's descriptor, and the process takes only a listener on its
    // binding's address.
    #[tokio::test(flavor = "multi_thread")]
    async fn pid_1_listens_before_its_set_returns() {
        let dir = tempfile::tempdir().unwrap();
        // A process that serves nothing: only PID 1 listens.
        let tls = TlsBindings::with_parts(dir.path().into(), Some("true".into()));
        let b = binding(&free_loopback());
        assert_eq!(tls.set(vec![b.clone()], false).await.unwrap(), 1);
        let waiting = std::net::TcpStream::connect(&b.listen).unwrap();
        let fd = |dir: &Path| read_config(&dir.join(CONFIG)).unwrap()[0].fd;
        let first = fd(dir.path());

        let moved = Binding {
            port: 6432,
            ..b.clone()
        };
        assert_eq!(tls.set(vec![moved.clone()], false).await.unwrap(), 1);
        assert_eq!(fd(dir.path()), first, "the address got another listener");
        std::net::TcpStream::connect(&b.listen).unwrap();

        // The process's side: a duplicate, as it inherits one.
        let inherited = read_config(&dir.path().join(CONFIG)).unwrap();
        // SAFETY: dup(2) of a descriptor the set holds.
        let dup = |fd: i32| unsafe { libc::dup(fd) };
        let mut taken = adopt(
            inherited
                .into_iter()
                .map(|i| Inherited { fd: dup(i.fd), ..i })
                .collect(),
        )
        .unwrap();
        assert_eq!(
            taken.0.remove(0).1.local_addr().unwrap().to_string(),
            b.listen
        );
        let elsewhere = Inherited {
            binding: binding(&free_loopback()),
            fd: dup(first),
        };
        let e = adopt(vec![elsewhere]).err().unwrap();
        assert!(e.contains("it listens on"), "{e}");
        let unbound = std::net::TcpStream::connect(&b.listen).unwrap();
        let e = adopt(vec![Inherited {
            binding: b.clone(),
            fd: dup(unbound.as_raw_fd()),
        }])
        .err()
        .unwrap();
        assert!(e.contains("does not listen"), "{e}");

        assert_eq!(tls.set(Vec::new(), false).await.unwrap(), 0);
        assert!(connect_refused(&b.listen), "a binding outlived its set");
        drop(waiting);
    }

    // A client that connects while the pod's signer does not answer yet,
    // as a container that starts while feos-authn does, waits and is
    // served once it answers: not refused.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_client_before_the_signer_waits_for_it() {
        let dir = tempfile::tempdir().unwrap();
        let ca = Ca::new();
        let up = upstream(&ca, ADDON).await;
        let b = Binding {
            host: "::1".into(),
            port: up.port(),
            ..binding(&free_loopback())
        };
        serving(&dir.path().join("feid.sock"), std::slice::from_ref(&b));
        let mut c = tokio::net::TcpStream::connect(&b.listen).await.unwrap();
        c.write_all(b"ping").await.unwrap();
        tokio::time::sleep(Duration::from_millis(500)).await;
        signer(dir.path(), &ca);
        let mut answer = [0u8; 8];
        tokio::time::timeout(Duration::from_secs(10), c.read_exact(&mut answer))
            .await
            .unwrap()
            .unwrap();
        assert_eq!(&answer, b"pod:ping");
    }

    // A plaintext client on loopback reaches the upstream over TLS with
    // the pod's certificate, ALPN postgresql, the upstream pinned to its
    // ID; an upstream with another ID gets no bytes.
    #[tokio::test(flavor = "multi_thread")]
    async fn a_client_on_loopback_reaches_its_pinned_upstream_with_the_pods_identity() {
        let dir = tempfile::tempdir().unwrap();
        let ca = Ca::new();
        let socket = signer(dir.path(), &ca);
        let good = upstream(&ca, ADDON).await;
        let other = upstream(&ca, "spiffe://run.example/tenant/evil/env/e9/addon/db").await;
        let ok = Binding {
            host: "::1".into(),
            port: good.port(),
            ..binding(&free_loopback())
        };
        let pinned = Binding {
            host: "::1".into(),
            port: other.port(),
            ..binding(&free_loopback())
        };
        serving(&socket, &[ok.clone(), pinned.clone()]);

        let mut c = tokio::net::TcpStream::connect(&ok.listen).await.unwrap();
        c.write_all(b"ping").await.unwrap();
        let mut answer = [0u8; 8];
        c.read_exact(&mut answer).await.unwrap();
        assert_eq!(&answer, b"pod:ping");

        let mut c = tokio::net::TcpStream::connect(&pinned.listen)
            .await
            .unwrap();
        let _ = c.write_all(b"ping").await;
        let mut buf = Vec::new();
        let n = tokio::time::timeout(Duration::from_secs(10), c.read_to_end(&mut buf))
            .await
            .unwrap()
            .unwrap_or(0);
        assert_eq!(n, 0, "the upstream with another ID got the client: {buf:?}");
    }

    // A binding without an ALPN protocol offers none and reaches an
    // upstream that has none, as Valkey (ADR 0039).
    #[tokio::test(flavor = "multi_thread")]
    async fn a_binding_without_alpn_offers_none() {
        let dir = tempfile::tempdir().unwrap();
        let ca = Ca::new();
        let socket = signer(dir.path(), &ca);
        let up = upstream_with(&ca, ADDON, None).await;
        let b = Binding {
            host: "::1".into(),
            port: up.port(),
            alpn: String::new(),
            ..binding(&free_loopback())
        };
        serving(&socket, std::slice::from_ref(&b));
        let mut c = tokio::net::TcpStream::connect(&b.listen).await.unwrap();
        c.write_all(b"PING").await.unwrap();
        let mut answer = [0u8; 8];
        c.read_exact(&mut answer).await.unwrap();
        assert_eq!(&answer, b"pod:PING");
    }
}
