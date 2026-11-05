// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's `feos-authn` (ADR 0014, ADR 0028): one process per running pod
//! with HTTP bindings, which holds the pod's credentials and bindings in
//! its memory and serves the connections the pod opens to them.
//!
//! A pod's run of it begins when the pod comes to want one (created,
//! started, resumed or restored, or given its first binding while it
//! runs) and ends when it stops wanting it (suspended, stopped, deleted,
//! failed, or left without bindings): every run is a new process with new
//! keys. One that exits while its pod wants it is started again, after a
//! backoff.
//!
//! `feosd` keeps each run's pid, start time and user in the pod's
//! directory (`<pods>/<id>/authn.json`), beside the control socket
//! `authn.sock` it binds and hands to the process listening, and the audit
//! file `authn-audit.ndjson` it opens for it. A `feosd` that starts again
//! adopts the process by its pid and start time, dials its control socket
//! and says hello again; a process that is gone, or whose pod does not
//! want it, is not adopted. The protocol is in [`control`], the jail in
//! [`jail`].

pub mod audit;
pub mod control;
pub mod jail;
pub mod listener;

use base64::engine::general_purpose::STANDARD as BASE64;
use base64::Engine;
use control::{Control, FromAuthn, ToAuthn};
use feos_proto::fleet_service::{IssuePodCredentialsRequest, IssuePodCredentialsResponse};
use futures::future::BoxFuture;
use log::{debug, info, warn};
use nix::sys::socket::{
    bind, listen, socket, AddressFamily, Backlog, SockFlag, SockType, UnixAddr,
};
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::io;
use std::os::fd::{AsRawFd, BorrowedFd, OwnedFd};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard, RwLock};
use std::time::Duration;
use tokio::io::unix::AsyncFd;
use tokio::sync::watch;
use tokio::task::JoinHandle;
use tokio::time::{sleep, timeout, Instant};
use tonic::Status;

/// The control socket, the audit file and the run's record, in the pod's
/// directory.
pub const SOCKET: &str = "authn.sock";
pub const AUDIT: &str = "authn-audit.ndjson";
const RECORD: &str = "authn.json";
/// The binary, next to `feosd`'s by default (`--authn-binary`).
pub const BINARY: &str = "feos-authn";
/// How long a process has to exit after SIGTERM, and after SIGKILL.
const STOP_GRACE: Duration = Duration::from_secs(2);
const KILL_WAIT: Duration = Duration::from_secs(1);
/// The wait before a process that exited is started again, doubled up to
/// the second; and before a pod's bindings are asked for again while the
/// process holds another version than the pod's record.
const RESTART_BACKOFF: (Duration, Duration) = (Duration::from_secs(1), Duration::from_secs(30));
const REFETCH_BACKOFF: (Duration, Duration) = (Duration::from_secs(2), Duration::from_secs(60));
/// How often a run that waits is looked at.
const POLL: Duration = Duration::from_millis(100);
/// How long the control plane has to answer a pod's credentials.
const ISSUE_TIMEOUT: Duration = Duration::from_secs(30);
/// A pod's connections being handed to its process at a time: past it,
/// the pod's next one is reset, so that one pod, or its process that does
/// not read, holds no more of the node's hand-overs.
pub const HANDING_PER_POD: usize = 64;
/// How long a hand-over waits for the process to take the connection.
const HAND_OVER_TIMEOUT: Duration = Duration::from_secs(1);

/// Asks the control plane for a pod's credentials (fleet.proto
/// `IssuePodCredentials`), over feosd's uplink.
pub type Issuer = Arc<
    dyn Fn(
            IssuePodCredentialsRequest,
        ) -> BoxFuture<'static, Result<IssuePodCredentialsResponse, Status>>
        + Send
        + Sync,
>;

/// Starts a process, as [`jail::spawn`] does: a test starts a fake.
pub type Launcher = Arc<dyn Fn(&jail::Start<'_>) -> io::Result<i32> + Send + Sync>;

/// How the processes are started.
#[derive(Clone, Debug, PartialEq)]
pub struct Config {
    /// `feos-authn`'s binary (`feosd --authn-binary`).
    pub binary: PathBuf,
    /// Each in its jail, as a user of its own: as root. Without, as this
    /// process's user, for tests.
    pub jail: bool,
}

impl Config {
    /// The binary next to this process's own, jailed.
    pub fn beside_feosd() -> Self {
        let binary = std::env::current_exe()
            .ok()
            .and_then(|exe| Some(exe.parent()?.join(BINARY)))
            .unwrap_or_else(|| PathBuf::from("/usr/sbin").join(BINARY));
        Self { binary, jail: true }
    }
}

/// What a pod wants of its `feos-authn`, as its record says.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Wanted {
    /// A process runs: the pod is coming up or runs, with HTTP bindings.
    pub run: bool,
    /// Counts the pod's runs: each time it comes to want one.
    generation: u64,
    /// The version of the bindings in its record (`PodHttpBindings`);
    /// empty: none stated.
    pub version: String,
    /// The pod is deleted.
    gone: bool,
}

/// The pods' processes.
#[derive(Clone)]
pub struct Authn {
    inner: Arc<Inner>,
}

struct Inner {
    config: Config,
    launch: Launcher,
    /// The pods' directories are below it.
    dir: PathBuf,
    issuer: RwLock<Option<Issuer>>,
    /// The prefixes the processes dial none of: feosd's `--deny-prefix`
    /// set, the node's own networks and those denied always; None until
    /// the node's addresses are known, and nothing says hello before.
    deny: watch::Sender<Option<Vec<String>>>,
    /// The resolvers its jail's `resolv.conf` names: the node's upstreams.
    resolvers: RwLock<Vec<std::net::IpAddr>>,
    pods: Mutex<HashMap<String, Slot>>,
    /// Processes a feosd before this one started, until their pods' slots
    /// take them.
    adopted: Mutex<HashMap<String, Arc<Run>>>,
    /// The users the processes run as.
    uids: Mutex<HashSet<u32>>,
}

/// A pod, as its processes follow it.
struct Slot {
    wanted: watch::Sender<Wanted>,
    /// The process that runs now, if any.
    current: Arc<Mutex<Option<Arc<Run>>>>,
    /// Why the last start or credentials failed.
    error: Arc<Mutex<String>>,
    /// The pod's hand-overs in flight ([`HANDING_PER_POD`]).
    handing: Arc<tokio::sync::Semaphore>,
    actor: JoinHandle<()>,
}

/// A run of a pod's process.
struct Run {
    pod_id: String,
    pid: i32,
    uid: u32,
    generation: Mutex<u64>,
    /// Readable once the process exited.
    exit: AsyncFd<OwnedFd>,
    /// Whether this feosd started it: it reaps it.
    child: bool,
    control: RwLock<Option<Arc<Control>>>,
    /// The version of the bindings it holds, once it is ready.
    ready: watch::Sender<Option<String>>,
}

/// A run's record in the pod's directory.
#[derive(Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Record {
    pid: i32,
    start_time: u64,
    uid: u32,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(|e| e.into_inner())
}

impl Authn {
    /// The processes of the pods below `dir`, started by `config`: those a
    /// feosd before this one started are adopted, if they still run, for
    /// their pods' slots to take them.
    pub fn new(config: Config, dir: &Path) -> Self {
        let launch: Launcher = Arc::new(jail::spawn);
        Self::with_launcher(config, dir, launch)
    }

    pub fn with_launcher(config: Config, dir: &Path, launch: Launcher) -> Self {
        let authn = Self {
            inner: Arc::new(Inner {
                config,
                launch,
                dir: dir.to_path_buf(),
                issuer: RwLock::new(None),
                deny: watch::Sender::new(None),
                resolvers: RwLock::new(Vec::new()),
                pods: Mutex::new(HashMap::new()),
                adopted: Mutex::new(HashMap::new()),
                uids: Mutex::new(HashSet::new()),
            }),
        };
        authn.adopt();
        authn
    }

    /// Asks `issuer` for the pods' credentials from now on; until then,
    /// a process that asks is told that the node has no control plane.
    pub fn set_issuer(&self, issuer: Issuer) {
        *self.inner.issuer.write().unwrap_or_else(|e| e.into_inner()) = Some(issuer);
    }

    /// The prefixes the processes dial none of, as they are now: the first
    /// list lets them say hello, and every process connected is told a
    /// list that changed, by a hello again.
    pub fn set_deny_prefixes(&self, deny: Vec<String>) {
        self.inner.deny.send_if_modified(|now| {
            let changed = now.as_ref() != Some(&deny);
            if changed {
                *now = Some(deny);
            }
            changed
        });
    }

    /// The resolvers the processes resolve upstreams with, which their
    /// jail's `resolv.conf` names, from their next start on.
    pub fn set_resolvers(&self, resolvers: Vec<std::net::IpAddr>) {
        *self
            .inner
            .resolvers
            .write()
            .unwrap_or_else(|e| e.into_inner()) = resolvers;
    }

    /// The pod's directory, where its process's files are.
    pub fn pod_dir(&self, pod_id: &str) -> PathBuf {
        self.inner.dir.join(pod_id)
    }

    /// Follows what the pod wants: a process while `run`, a new one each
    /// time it comes to want one, and its bindings of `version`.
    pub fn want(&self, pod_id: &str, run: bool, version: &str) {
        let mut pods = lock(&self.inner.pods);
        // A pod that never wanted one has no slot; a process a feosd before
        // started for it is ended by `end_unclaimed`.
        if !run && !pods.contains_key(pod_id) {
            return;
        }
        let slot = pods
            .entry(pod_id.to_string())
            .or_insert_with(|| self.slot(pod_id));
        slot.wanted.send_if_modified(|wanted| {
            let before = wanted.clone();
            if run && !wanted.run {
                wanted.generation += 1;
            }
            wanted.run = run;
            wanted.version = version.to_string();
            *wanted != before
        });
    }

    /// The pod is deleted: its process ends, and its directory goes.
    pub async fn remove(&self, pod_id: &str) {
        let slot = lock(&self.inner.pods).remove(pod_id);
        let adopted = lock(&self.inner.adopted).remove(pod_id);
        if let Some(run) = adopted {
            stop(&self.inner, &run).await;
        }
        if let Some(slot) = slot {
            slot.wanted.send_modify(|wanted| {
                wanted.run = false;
                wanted.gone = true;
            });
            drop(slot.wanted);
            let _ = timeout(STOP_GRACE + KILL_WAIT * 2, slot.actor).await;
        }
        let dir = self.pod_dir(pod_id);
        jail::unlock_root(&dir);
        match tokio::fs::remove_dir_all(&dir).await {
            Err(e) if e.kind() != io::ErrorKind::NotFound => {
                warn!("Authn: Failed to remove {}: {e}", dir.display())
            }
            _ => {}
        }
    }

    /// Ends the adopted processes whose pods no slot took: once every pod
    /// kept said what it wants.
    pub async fn end_unclaimed(&self) {
        let unclaimed: Vec<Arc<Run>> = lock(&self.inner.adopted).drain().map(|(_, r)| r).collect();
        for run in unclaimed {
            info!(
                "Authn: Pod {}'s feos-authn (pid {}) is not wanted; ending it",
                run.pod_id, run.pid
            );
            stop(&self.inner, &run).await;
        }
    }

    /// Waits until the pod's process holds its credentials, `wait` at
    /// most: a run waits for them before the pod's containers start (ADR
    /// 0014). A pod that wants none waits for nothing.
    pub async fn wait_ready(&self, pod_id: &str, wait: Duration) -> Result<(), Status> {
        let deadline = Instant::now() + wait;
        loop {
            let (wanted, current, error) = {
                let pods = lock(&self.inner.pods);
                let Some(slot) = pods.get(pod_id) else {
                    return Ok(());
                };
                (
                    slot.wanted.borrow().clone(),
                    lock(&slot.current).clone(),
                    lock(&slot.error).clone(),
                )
            };
            if !wanted.run {
                return Ok(());
            }
            if let Some(run) = current
                && *lock(&run.generation) == wanted.generation
                && run.ready.borrow().is_some()
            {
                return Ok(());
            }
            if Instant::now() >= deadline {
                let why = match error.is_empty() {
                    true => "it is not ready".to_string(),
                    false => error,
                };
                return Err(Status::unavailable(format!(
                    "pod {pod_id}'s feos-authn has not got its credentials within {}s: {why}",
                    wait.as_secs()
                )));
            }
            sleep(POLL).await;
        }
    }

    /// Hands a connection the pod opened to one of its HTTP bindings at
    /// `port` from `peer` to its process, which reads it; feosd reads
    /// nothing of it. Fails while the pod has no process to take it, while
    /// [`HANDING_PER_POD`] of its connections are being handed over, and
    /// when the process does not take it within [`HAND_OVER_TIMEOUT`].
    pub async fn pass(
        &self,
        pod_id: &str,
        port: u16,
        peer: std::net::SocketAddr,
        connection: BorrowedFd<'_>,
    ) -> Result<(), String> {
        let (current, handing) = {
            let pods = lock(&self.inner.pods);
            let slot = pods.get(pod_id).ok_or("the pod has no HTTP bindings")?;
            if !slot.wanted.borrow().run {
                return Err("the pod has no HTTP bindings, or does not run".into());
            }
            (lock(&slot.current).clone(), slot.handing.clone())
        };
        let _handing = handing
            .try_acquire_owned()
            .map_err(|_| format!("{HANDING_PER_POD} of its connections are being handed over"))?;
        let run = current.ok_or("its feos-authn does not run")?;
        let control = run.control().ok_or("its feos-authn is not connected")?;
        let message = ToAuthn::Conn {
            port,
            peer: peer.to_string(),
        };
        // A message is one sendmsg: one that did not go when the wait ends
        // went not at all.
        match timeout(HAND_OVER_TIMEOUT, control.send(&message, Some(connection))).await {
            Ok(sent) => sent.map_err(|e| format!("its feos-authn did not take it: {e}")),
            Err(_) => Err(format!(
                "its feos-authn did not take it within {}ms",
                HAND_OVER_TIMEOUT.as_millis()
            )),
        }
    }

    /// The pid of the pod's process that runs now.
    pub fn pid(&self, pod_id: &str) -> Option<i32> {
        let pods = lock(&self.inner.pods);
        let current = lock(&pods.get(pod_id)?.current).clone();
        current.map(|run| run.pid)
    }

    /// A pod's slot, and its actor, which takes the process a feosd before
    /// started for it.
    fn slot(&self, pod_id: &str) -> Slot {
        let (wanted, receiver) = watch::channel(Wanted::default());
        let current = Arc::new(Mutex::new(None));
        let error = Arc::new(Mutex::new(String::new()));
        let adopted = lock(&self.inner.adopted).remove(pod_id);
        let actor = tokio::spawn(actor(
            self.inner.clone(),
            pod_id.to_string(),
            receiver,
            current.clone(),
            error.clone(),
            adopted,
        ));
        Slot {
            wanted,
            current,
            error,
            handing: Arc::new(tokio::sync::Semaphore::new(HANDING_PER_POD)),
            actor,
        }
    }

    /// Adopts the processes of the records in the pods' directories that
    /// still run; the others' records go.
    fn adopt(&self) {
        let Ok(entries) = std::fs::read_dir(&self.inner.dir) else {
            return;
        };
        for entry in entries.flatten() {
            let path = entry.path();
            if !path.is_dir() {
                continue;
            }
            let pod_id = entry.file_name().to_string_lossy().into_owned();
            let Ok(text) = std::fs::read_to_string(path.join(RECORD)) else {
                continue;
            };
            let record: Option<Record> = serde_json::from_str(&text).ok();
            let found = record
                .as_ref()
                .and_then(|r| Some((r, jail::find(r.pid, r.start_time)?)));
            let Some((record, pidfd)) = found else {
                let _ = std::fs::remove_file(path.join(RECORD));
                continue;
            };
            match Run::new(&pod_id, record.pid, record.uid, pidfd, false) {
                Ok(run) => {
                    info!(
                        "Authn: Adopted pod {pod_id}'s feos-authn (pid {})",
                        record.pid
                    );
                    lock(&self.inner.uids).insert(record.uid);
                    lock(&self.inner.adopted).insert(pod_id, Arc::new(run));
                }
                Err(e) => warn!("Authn: Pod {pod_id}'s feos-authn is not adopted: {e}"),
            }
        }
    }
}

impl Run {
    fn new(pod_id: &str, pid: i32, uid: u32, pidfd: OwnedFd, child: bool) -> io::Result<Self> {
        Ok(Self {
            pod_id: pod_id.to_string(),
            pid,
            uid,
            generation: Mutex::new(0),
            exit: AsyncFd::with_interest(pidfd, tokio::io::Interest::READABLE)?,
            child,
            control: RwLock::new(None),
            ready: watch::Sender::new(None),
        })
    }

    fn control(&self) -> Option<Arc<Control>> {
        self.control
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    fn set_control(&self, control: Option<Arc<Control>>) {
        *self.control.write().unwrap_or_else(|e| e.into_inner()) = control;
    }

    /// Resolves once the process exited.
    async fn exited(&self) {
        let _ = self.exit.readable().await;
    }

    fn signal(&self, signal: libc::c_int) {
        // SAFETY: a plain system call on the run's pidfd, which names the
        // process even after its pid is reused.
        unsafe {
            libc::syscall(
                libc::SYS_pidfd_send_signal,
                self.exit.get_ref().as_raw_fd(),
                signal,
                std::ptr::null::<libc::siginfo_t>(),
                0,
            )
        };
    }
}

/// Follows a pod's wants: starts and ends its runs, starts one that exited
/// again, and has its process fetch its bindings again when their version
/// moves. Ends with its pod.
async fn actor(
    inner: Arc<Inner>,
    pod_id: String,
    mut wanted: watch::Receiver<Wanted>,
    current: Arc<Mutex<Option<Arc<Run>>>>,
    error: Arc<Mutex<String>>,
    mut adopted: Option<Arc<Run>>,
) {
    let mut backoff = RESTART_BACKOFF.0;
    // Not before this instant after an exit or a failed start.
    let mut not_before = Instant::now();
    let mut asked_version = String::new();
    loop {
        let want = wanted.borrow_and_update().clone();
        let run = lock(&current).clone();
        if let Some(adopted) = adopted.take() {
            match want.run {
                true => {
                    *lock(&adopted.generation) = want.generation;
                    connect(&inner, &adopted, wanted.clone(), error.clone()).await;
                    *lock(&current) = Some(adopted);
                    continue;
                }
                false => stop(&inner, &adopted).await,
            }
        }
        match run {
            Some(run) if want.run && *lock(&run.generation) == want.generation => {
                // The record's bindings moved: fetched again at once.
                if !want.version.is_empty()
                    && want.version != asked_version
                    && run
                        .ready
                        .borrow()
                        .as_deref()
                        .is_some_and(|v| v != want.version)
                {
                    asked_version = want.version.clone();
                    refetch(&run).await;
                }
            }
            Some(run) => {
                *lock(&current) = None;
                stop(&inner, &run).await;
                continue;
            }
            None if want.run && Instant::now() >= not_before => {
                match start(&inner, &pod_id, want.generation).await {
                    Ok(run) => {
                        let run = Arc::new(run);
                        connect(&inner, &run, wanted.clone(), error.clone()).await;
                        *lock(&current) = Some(run);
                        continue;
                    }
                    Err(e) => {
                        warn!("Authn: Pod {pod_id}'s feos-authn does not start: {e}");
                        *lock(&error) = format!("it does not start: {e}");
                        not_before = Instant::now() + backoff;
                        backoff = (backoff * 2).min(RESTART_BACKOFF.1);
                    }
                }
            }
            None => {}
        }
        if want.gone {
            return;
        }
        let run = lock(&current).clone();
        let waiting = run.is_none() && want.run;
        tokio::select! {
            changed = wanted.changed() => {
                if changed.is_err() {
                    let run = lock(&current).take();
                    if let Some(run) = run {
                        stop(&inner, &run).await;
                    }
                    return;
                }
            }
            () = async {
                match &run {
                    Some(run) => run.exited().await,
                    None => std::future::pending().await,
                }
            } => {
                if let Some(run) = run {
                    *lock(&current) = None;
                    let code = reap(&run).await;
                    let step = code.and_then(jail::failed_step).map(|s| format!(" in its jail's {s}")).unwrap_or_default();
                    warn!(
                        "Authn: Pod {pod_id}'s feos-authn (pid {}) exited{}{step}; again in {}s",
                        run.pid,
                        code.map(|c| format!(" with {c}")).unwrap_or_default(),
                        backoff.as_secs()
                    );
                    *lock(&error) = format!("it exited{}", code.map(|c| format!(" with {c}")).unwrap_or_default());
                    forget(&inner, &run).await;
                    not_before = Instant::now() + backoff;
                    backoff = (backoff * 2).min(RESTART_BACKOFF.1);
                }
            }
            () = sleep_until_or_pending(not_before, waiting) => {}
        }
        // A run that held on for a while starts again at once next time.
        if lock(&current)
            .as_ref()
            .is_some_and(|run| run.ready.borrow().is_some())
        {
            backoff = RESTART_BACKOFF.0;
        }
    }
}

async fn sleep_until_or_pending(instant: Instant, waiting: bool) {
    match waiting {
        true => tokio::time::sleep_until(instant).await,
        false => std::future::pending().await,
    }
}

/// Starts a run of the pod's process: its directory, its control socket's
/// listener, its audit file, a user, and the process in its jail.
async fn start(inner: &Arc<Inner>, pod_id: &str, generation: u64) -> io::Result<Run> {
    let uid = {
        let mut uids = lock(&inner.uids);
        let uid = (jail::UID_BASE..jail::UID_BASE + jail::UID_COUNT)
            .find(|uid| !uids.contains(uid))
            .ok_or_else(|| io::Error::other("no user ID is free"))?;
        uids.insert(uid);
        uid
    };
    let (inner2, id) = (inner.clone(), pod_id.to_string());
    let started = tokio::task::spawn_blocking(move || launch(&inner2, &id, uid))
        .await
        .map_err(io::Error::other)
        .and_then(|started| started);
    match started {
        Ok((pid, pidfd)) => {
            info!("Authn: Started pod {pod_id}'s feos-authn (pid {pid}, user {uid})");
            let run = Run::new(pod_id, pid, uid, pidfd, true)?;
            *lock(&run.generation) = generation;
            Ok(run)
        }
        Err(e) => {
            lock(&inner.uids).remove(&uid);
            Err(e)
        }
    }
}

/// [`start`]'s blocking part: returns the pid and its pidfd.
fn launch(inner: &Inner, pod_id: &str, uid: u32) -> io::Result<(i32, OwnedFd)> {
    let dir = inner.dir.join(pod_id);
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(&dir)?;
    let socket_path = dir.join(SOCKET);
    match std::fs::remove_file(&socket_path) {
        Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e),
        _ => {}
    }
    let listener = socket(
        AddressFamily::Unix,
        SockType::SeqPacket,
        SockFlag::SOCK_CLOEXEC,
        None,
    )?;
    bind(listener.as_raw_fd(), &UnixAddr::new(&socket_path)?)?;
    std::fs::set_permissions(&socket_path, std::fs::Permissions::from_mode(0o600))?;
    listen(&listener, Backlog::new(16).map_err(io::Error::from)?)?;
    let audit = std::fs::OpenOptions::new()
        .append(true)
        .create(true)
        .mode(0o600)
        .open(dir.join(AUDIT))?;
    let resolvers = inner
        .resolvers
        .read()
        .unwrap_or_else(|e| e.into_inner())
        .clone();
    let root = jail::root_dir(&dir, &resolvers)?;
    let audit = OwnedFd::from(audit);
    let pid = (inner.launch)(&jail::Start {
        binary: &inner.config.binary,
        pod_id,
        audit: &audit,
        control: &listener,
        root: &root,
        uid: inner.config.jail.then_some(uid),
    })?;
    let pidfd = jail::pidfd(pid)?;
    let start_time = jail::start_time(pid)
        .ok_or_else(|| io::Error::other(format!("process {pid} has no start time")))?;
    let record = Record {
        pid,
        start_time,
        uid,
    };
    write_record(&dir, &record)?;
    Ok((pid, pidfd))
}

fn write_record(dir: &Path, record: &Record) -> io::Result<()> {
    let temporary = dir.join(format!("{RECORD}.new"));
    std::fs::write(
        &temporary,
        serde_json::to_vec(record).map_err(io::Error::other)?,
    )?;
    std::fs::rename(temporary, dir.join(RECORD))
}

/// Dials the run's control socket, says hello once the node's deny
/// prefixes are known, and reads what the process says from then on. A
/// process that cannot be dialed is ended: one that cannot be told
/// anything serves nothing, and its exit starts another.
async fn connect(
    inner: &Arc<Inner>,
    run: &Arc<Run>,
    wanted: watch::Receiver<Wanted>,
    error: Arc<Mutex<String>>,
) {
    let path = inner.dir.join(&run.pod_id).join(SOCKET);
    let control = match Control::connect(&path) {
        Ok(control) => Arc::new(control),
        Err(e) => {
            warn!(
                "Authn: Pod {}'s feos-authn (pid {}) does not answer on {}: {e}; ending it",
                run.pod_id,
                run.pid,
                path.display()
            );
            *lock(&error) = format!("its control socket does not answer: {e}");
            run.signal(libc::SIGKILL);
            return;
        }
    };
    tokio::spawn(read(inner.clone(), run.clone(), control, wanted, error));
}

fn hello(run: &Run, deny: Vec<String>) -> ToAuthn {
    ToAuthn::Hello {
        pod_id: run.pod_id.clone(),
        deny_prefixes: deny,
        source_v4: None,
        source_v6: None,
    }
}

/// Says hello once the node's deny prefixes are known, then reads what the
/// run's process says until it closes its end, and says hello again when
/// the prefixes change.
async fn read(
    inner: Arc<Inner>,
    run: Arc<Run>,
    control: Arc<Control>,
    wanted: watch::Receiver<Wanted>,
    error: Arc<Mutex<String>>,
) {
    let mut deny = inner.deny.subscribe();
    // A hello with fewer prefixes would let the process dial what it may
    // not: none before the node's are known. The process says nothing
    // before its hello; its end closing ends the wait.
    let mut said = loop {
        if let Some(list) = deny.borrow_and_update().clone() {
            break list;
        }
        *lock(&error) = "it waits for the node's network".into();
        tokio::select! {
            changed = deny.changed() => {
                if changed.is_err() {
                    return;
                }
            }
            received = control.recv() => {
                if !matches!(received, Ok(Some(_))) {
                    return;
                }
            }
        }
    };
    if let Err(e) = control.send(&hello(&run, said.clone()), None).await {
        warn!(
            "Authn: Pod {}'s feos-authn (pid {}) did not take its hello: {e}; ending it",
            run.pod_id, run.pid
        );
        run.signal(libc::SIGKILL);
        return;
    }
    run.set_control(Some(control.clone()));
    let mut deny_open = true;
    let mut refetch_backoff = REFETCH_BACKOFF.0;
    loop {
        let bytes = tokio::select! {
            received = control.recv() => match received {
                Ok(Some(bytes)) => bytes,
                Ok(None) => break,
                Err(e) => {
                    warn!("Authn: Pod {}'s control socket: {e}", run.pod_id);
                    break;
                }
            },
            changed = deny.changed(), if deny_open => {
                deny_open = changed.is_ok();
                let now = deny.borrow_and_update().clone();
                if let Some(list) = now.filter(|list| *list != said) {
                    if let Err(e) = control.send(&hello(&run, list.clone()), None).await {
                        warn!(
                            "Authn: Pod {}'s feos-authn did not take its new hello: {e}",
                            run.pod_id
                        );
                    }
                    said = list;
                }
                continue;
            }
        };
        match control::decode(&bytes) {
            Ok(FromAuthn::Credentials {
                hpke_public_key,
                csr,
            }) => {
                let (inner, control, run, error) =
                    (inner.clone(), control.clone(), run.clone(), error.clone());
                tokio::spawn(async move {
                    let answer = credentials(&inner, &run.pod_id, &hpke_public_key, &csr).await;
                    if let ToAuthn::Error { message } = &answer {
                        warn!(
                            "Authn: Pod {}'s credentials are not issued: {message}",
                            run.pod_id
                        );
                        *lock(&error) = format!("its credentials are not issued: {message}");
                    }
                    if let Err(e) = control.send(&answer, None).await {
                        warn!(
                            "Authn: Pod {}'s feos-authn did not take its credentials: {e}",
                            run.pod_id
                        );
                    }
                });
            }
            Ok(FromAuthn::Ready { version }) => {
                info!(
                    "Authn: Pod {}'s feos-authn holds its bindings of version {version:?}",
                    run.pod_id
                );
                let own = wanted.borrow().version.clone();
                run.ready.send_replace(Some(version.clone()));
                if !own.is_empty() && own != version {
                    // The control plane may not have sealed that version
                    // yet: asked again after a while, then less often.
                    let (run, wait) = (run.clone(), refetch_backoff);
                    refetch_backoff = (refetch_backoff * 2).min(REFETCH_BACKOFF.1);
                    tokio::spawn(async move {
                        sleep(wait).await;
                        refetch(&run).await;
                    });
                } else {
                    refetch_backoff = REFETCH_BACKOFF.0;
                    lock(&error).clear();
                }
            }
            Ok(FromAuthn::Error { message }) => {
                warn!(
                    "Authn: Pod {}'s feos-authn cannot use its credentials: {message}",
                    run.pod_id
                );
                *lock(&error) = format!("it cannot use its credentials: {message}");
            }
            Ok(FromAuthn::Unknown) => debug!(
                "Authn: Pod {}'s feos-authn said what this feosd does not know: {}",
                run.pod_id,
                String::from_utf8_lossy(&bytes)
            ),
            Err(e) => warn!("Authn: Pod {}'s feos-authn: {e}", run.pod_id),
        }
    }
    run.set_control(None);
}

/// Has the run's process ask for its credentials again.
async fn refetch(run: &Run) {
    let Some(control) = run.control() else {
        return;
    };
    if let Err(e) = control.send(&ToAuthn::Refetch, None).await {
        warn!(
            "Authn: Pod {}'s feos-authn did not take a refetch: {e}",
            run.pod_id
        );
    }
}

/// The control plane's answer to a process's `credentials`, as the
/// process is told it.
async fn credentials(inner: &Inner, pod_id: &str, hpke_public_key: &str, csr: &str) -> ToAuthn {
    let error = |message: String| ToAuthn::Error { message };
    let (Ok(hpke_public_key), Ok(csr)) = (BASE64.decode(hpke_public_key), BASE64.decode(csr))
    else {
        return error("the key or the signing request is not base64".into());
    };
    let issuer = inner
        .issuer
        .read()
        .unwrap_or_else(|e| e.into_inner())
        .clone();
    let Some(issuer) = issuer else {
        return error("the node has no control plane, which issues credentials".into());
    };
    let request = IssuePodCredentialsRequest {
        pod_id: pod_id.to_string(),
        csr,
        hpke_public_key,
    };
    match timeout(ISSUE_TIMEOUT, issuer(request)).await {
        Err(_) => error(format!(
            "the control plane did not answer within {}s",
            ISSUE_TIMEOUT.as_secs()
        )),
        Ok(Err(status)) => error(format!("{:?}: {}", status.code(), status.message())),
        Ok(Ok(response)) => answer(response),
    }
}

/// An `IssuePodCredentialsResponse` as the process is told it.
fn answer(response: IssuePodCredentialsResponse) -> ToAuthn {
    let seconds = |time: Option<prost_types::Timestamp>| time.map_or(0, |t| t.seconds);
    let encode = |ders: Vec<Vec<u8>>| ders.iter().map(|der| BASE64.encode(der)).collect();
    ToAuthn::Credentials {
        certificate: response.certificate.map(|c| control::Certificate {
            spiffe_id: c.spiffe_id,
            chain: encode(c.chain),
            trust_bundle: encode(c.trust_bundle),
            not_before: seconds(c.not_before),
            not_after: seconds(c.not_after),
            renew_after: seconds(c.renew_after),
        }),
        bindings: response.bindings.map(|b| control::Sealed {
            enc: BASE64.encode(b.enc),
            ciphertext: BASE64.encode(b.ciphertext),
            version: b.version,
        }),
    }
}

/// Ends a run: SIGTERM, SIGKILL after [`STOP_GRACE`], and forgets it.
async fn stop(inner: &Inner, run: &Run) {
    run.set_control(None);
    run.signal(libc::SIGTERM);
    if timeout(STOP_GRACE, run.exited()).await.is_err() {
        warn!(
            "Authn: Pod {}'s feos-authn (pid {}) outlived SIGTERM; killing it",
            run.pod_id, run.pid
        );
        run.signal(libc::SIGKILL);
        let _ = timeout(KILL_WAIT, run.exited()).await;
    }
    reap(run).await;
    info!(
        "Authn: Ended pod {}'s feos-authn (pid {})",
        run.pod_id, run.pid
    );
    forget(inner, run).await;
}

/// The exit code of a run that exited, if this feosd started it: the
/// reaper's when it owns waitpid, else its own wait.
async fn reap(run: &Run) -> Option<i32> {
    if !run.child {
        return None;
    }
    if feos_utils::reaper::is_running() {
        return timeout(KILL_WAIT, feos_utils::reaper::wait_for_exit(run.pid))
            .await
            .ok();
    }
    let pid = nix::unistd::Pid::from_raw(run.pid);
    match nix::sys::wait::waitpid(pid, Some(nix::sys::wait::WaitPidFlag::WNOHANG)) {
        Ok(nix::sys::wait::WaitStatus::Exited(_, code)) => Some(code),
        Ok(nix::sys::wait::WaitStatus::Signaled(_, signal, _)) => Some(128 + signal as i32),
        _ => None,
    }
}

/// Removes a run's record and socket and frees its user.
async fn forget(inner: &Inner, run: &Run) {
    let dir = inner.dir.join(&run.pod_id);
    // A record of another run is not this one's to remove.
    let record = tokio::fs::read_to_string(dir.join(RECORD)).await.ok();
    let ours = record
        .and_then(|text| serde_json::from_str::<Record>(&text).ok())
        .is_some_and(|record| record.pid == run.pid);
    if ours {
        let _ = tokio::fs::remove_file(dir.join(RECORD)).await;
        let _ = tokio::fs::remove_file(dir.join(SOCKET)).await;
    }
    lock(&inner.uids).remove(&run.uid);
}

/// The audit file of the pod `pod_id` below `dir`.
pub fn audit_path(dir: &Path, pod_id: &str) -> PathBuf {
    dir.join(pod_id).join(AUDIT)
}

/// A fake `feos-authn` for tests: a real process to start, stop and adopt
/// (`sleep`), and a thread that speaks the protocol on the control
/// socket's listener it was handed.
#[cfg(test)]
pub(crate) mod fake {
    use super::*;
    use nix::sys::socket::{accept, send, MsgFlags};
    use serde_json::{json, Value};

    /// What the fake saw, and what it wrote into the connections it got.
    #[derive(Default)]
    pub struct Seen {
        pub messages: Vec<Value>,
        /// The pids it ran as, in order.
        pub pids: Vec<i32>,
    }

    pub type Shared = Arc<Mutex<Seen>>;

    /// A launcher that starts `sleep` and the fake's thread.
    pub fn launcher(seen: Shared) -> Launcher {
        Arc::new(move |start: &jail::Start<'_>| {
            let listener = start.control.try_clone()?;
            let child = std::process::Command::new("sleep")
                .arg("3600")
                .stdin(std::process::Stdio::null())
                .spawn()?;
            let pid = child.id() as i32;
            lock(&seen).pids.push(pid);
            let seen = seen.clone();
            std::thread::spawn(move || serve(listener, seen));
            Ok(pid)
        })
    }

    /// A launcher whose process takes its control connections and never
    /// reads them, as one that hangs.
    pub fn deaf_launcher() -> Launcher {
        Arc::new(move |start: &jail::Start<'_>| {
            let listener = start.control.try_clone()?;
            let child = std::process::Command::new("sleep")
                .arg("3600")
                .stdin(std::process::Stdio::null())
                .spawn()?;
            std::thread::spawn(move || {
                let mut held = Vec::new();
                while let Ok(raw) = accept(listener.as_raw_fd()) {
                    // SAFETY: accept returned it, ours alone.
                    held.push(unsafe { <OwnedFd as std::os::fd::FromRawFd>::from_raw_fd(raw) });
                }
            });
            Ok(child.id() as i32)
        })
    }

    fn say(fd: &OwnedFd, message: Value) {
        let _ = send(
            fd.as_raw_fd(),
            message.to_string().as_bytes(),
            MsgFlags::MSG_NOSIGNAL,
        );
    }

    /// Answers every connection on `listener` until it closes.
    fn serve(listener: OwnedFd, seen: Shared) {
        let mut version: Option<String> = None;
        let ask = |fd: &OwnedFd| {
            say(
                fd,
                json!({"v": 1, "op": "credentials", "hpkePublicKey": "a2V5", "csr": "Y3Ny"}),
            )
        };
        loop {
            let Ok(raw) = accept(listener.as_raw_fd()) else {
                return;
            };
            // SAFETY: accept returned it, ours alone.
            let conn = unsafe { <OwnedFd as std::os::fd::FromRawFd>::from_raw_fd(raw) };
            while let Ok(Some((bytes, fds))) = control::recv_with_fds(conn.as_fd()) {
                let Ok(message) = serde_json::from_slice::<Value>(&bytes) else {
                    continue;
                };
                lock(&seen).messages.push(message.clone());
                match message["op"].as_str() {
                    Some("hello") => match &version {
                        Some(version) => {
                            say(&conn, json!({"v": 1, "op": "ready", "version": version}))
                        }
                        None => ask(&conn),
                    },
                    Some("refetch") => ask(&conn),
                    Some("credentials") => {
                        let got = message["bindings"]["version"].as_str().unwrap_or_default();
                        version = Some(got.to_string());
                        say(&conn, json!({"v": 1, "op": "ready", "version": got}));
                    }
                    Some("conn") => {
                        for fd in fds {
                            let reply = format!("authn has {}\n", message["peer"]);
                            let _ = nix::unistd::write(&fd, reply.as_bytes());
                        }
                    }
                    _ => {}
                }
            }
        }
    }

    use std::os::fd::AsFd;
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::fleet_service::SealedBindings;
    use std::io::Read;
    use std::os::fd::AsFd;

    const POD: &str = "a0000000001";

    /// An issuer that seals the bindings of the version in `version`.
    fn issuer(version: Arc<Mutex<String>>) -> Issuer {
        Arc::new(move |request: IssuePodCredentialsRequest| {
            let version = lock(&version).clone();
            Box::pin(async move {
                assert_eq!(request.csr, b"csr");
                assert_eq!(request.hpke_public_key, b"key");
                Ok(IssuePodCredentialsResponse {
                    certificate: Some(feos_proto::fleet_service::Certificate {
                        spiffe_id: format!("spiffe://t/pod/{}", request.pod_id),
                        ..Default::default()
                    }),
                    bindings: Some(SealedBindings {
                        enc: b"e".to_vec(),
                        ciphertext: b"c".to_vec(),
                        version,
                    }),
                })
            })
        })
    }

    /// Processes of the fake, before the node's prefixes are known.
    fn authn_unready(dir: &Path, seen: &fake::Shared) -> Authn {
        let config = Config {
            binary: PathBuf::from("/nonexistent/feos-authn"),
            jail: false,
        };
        Authn::with_launcher(config, dir, fake::launcher(seen.clone()))
    }

    /// Processes of the fake, which say hello at once.
    fn authn(dir: &Path, seen: &fake::Shared) -> Authn {
        let authn = authn_unready(dir, seen);
        authn.set_deny_prefixes(Vec::new());
        authn
    }

    fn ops(seen: &fake::Shared) -> Vec<String> {
        lock(seen)
            .messages
            .iter()
            .map(|m| m["op"].as_str().unwrap_or_default().to_string())
            .collect()
    }

    async fn within(what: &str, mut done: impl FnMut() -> bool) {
        let deadline = Instant::now() + Duration::from_secs(10);
        while !done() {
            assert!(Instant::now() < deadline, "timed out waiting for {what}");
            sleep(Duration::from_millis(20)).await;
        }
    }

    fn alive(pid: i32) -> bool {
        // A zombie is gone too.
        std::fs::read_to_string(format!("/proc/{pid}/stat")).is_ok_and(|stat| {
            !stat
                .rsplit_once(')')
                .is_some_and(|(_, rest)| rest.trim_start().starts_with('Z'))
        })
    }

    #[tokio::test]
    async fn a_run_gets_its_credentials_takes_connections_and_ends_with_its_pod() {
        let dir = tempfile::tempdir().unwrap();
        let seen = fake::Shared::default();
        let authn = authn_unready(dir.path(), &seen);
        let version = Arc::new(Mutex::new("v1".to_string()));
        authn.set_issuer(issuer(version.clone()));

        // Nothing says hello before the node's prefixes are known.
        authn.want(POD, true, "v1");
        let error = authn
            .wait_ready(POD, Duration::from_millis(500))
            .await
            .unwrap_err();
        assert!(error.message().contains("the node's network"), "{error}");
        assert!(authn.pid(POD).is_some());
        assert!(ops(&seen).is_empty(), "{:?}", ops(&seen));
        authn.set_deny_prefixes(vec!["10.0.0.0/8".to_string()]);
        authn
            .wait_ready(POD, Duration::from_secs(10))
            .await
            .unwrap();
        let pid = authn.pid(POD).unwrap();
        assert_eq!(lock(&seen).pids, [pid]);
        let hello = lock(&seen).messages[0].clone();
        assert_eq!(hello["podId"], POD);
        assert_eq!(hello["denyPrefixes"], serde_json::json!(["10.0.0.0/8"]));
        assert_eq!(hello["sourceV4"], serde_json::Value::Null);
        // A list that changes is said again; the same one is not.
        authn.set_deny_prefixes(vec!["10.0.0.0/8".to_string()]);
        authn.set_deny_prefixes(vec!["10.0.0.0/8".to_string(), "fd00::/8".to_string()]);
        within("the hello again", || {
            lock(&seen).messages.iter().any(|m| {
                m["op"] == "hello"
                    && m["denyPrefixes"] == serde_json::json!(["10.0.0.0/8", "fd00::/8"])
            })
        })
        .await;
        let hellos = ops(&seen).iter().filter(|op| *op == "hello").count();
        assert_eq!(hellos, 2);
        let answer = lock(&seen).messages[1].clone();
        assert_eq!(
            answer["certificate"]["spiffeId"],
            format!("spiffe://t/pod/{POD}")
        );
        assert_eq!(answer["bindings"]["enc"], "ZQ==");
        let pod_dir = authn.pod_dir(POD);
        let record: Record =
            serde_json::from_str(&std::fs::read_to_string(pod_dir.join(RECORD)).unwrap()).unwrap();
        assert_eq!((record.pid, record.uid), (pid, jail::UID_BASE));
        assert_eq!(Some(record.start_time), jail::start_time(pid));
        assert!(pod_dir.join(SOCKET).exists() && pod_dir.join(AUDIT).exists());

        // A connection of the pod's is passed on, not read.
        let (mut ours, theirs) = std::os::unix::net::UnixStream::pair().unwrap();
        let peer: std::net::SocketAddr = "169.254.16.5:40000".parse().unwrap();
        authn.pass(POD, 80, peer, theirs.as_fd()).await.unwrap();
        drop(theirs);
        let mut said = String::new();
        tokio::task::spawn_blocking(move || ours.read_to_string(&mut said).map(|_| said))
            .await
            .unwrap()
            .map(|said| assert_eq!(said, "authn has \"169.254.16.5:40000\"\n"))
            .unwrap();
        assert!(ops(&seen).contains(&"conn".to_string()));

        // The bindings move on: fetched again.
        *lock(&version) = "v2".to_string();
        authn.want(POD, true, "v2");
        within("the refetch", || {
            ops(&seen).contains(&"refetch".to_string())
        })
        .await;
        within("the second answer", || {
            lock(&seen)
                .messages
                .iter()
                .any(|m| m["bindings"]["version"] == "v2")
        })
        .await;
        assert_eq!(authn.pid(POD), Some(pid), "the same run");

        // The run ends with the pod's; another begins with a new process.
        authn.want(POD, false, "v2");
        within("the end", || !alive(pid)).await;
        within("the record's removal", || !pod_dir.join(RECORD).exists()).await;
        assert!(authn
            .pass(POD, 80, peer, std::io::stdin().as_fd())
            .await
            .is_err());
        authn.want(POD, true, "v2");
        authn
            .wait_ready(POD, Duration::from_secs(10))
            .await
            .unwrap();
        let second = authn.pid(POD).unwrap();
        assert_ne!(second, pid);

        // Deleted: the process and the directory go.
        authn.remove(POD).await;
        assert!(!alive(second));
        assert!(!pod_dir.exists());
    }

    /// A process that does not read its control socket holds at most
    /// [`HANDING_PER_POD`] hand-overs, each for [`HAND_OVER_TIMEOUT`] at
    /// most: the node's other pods keep theirs.
    #[tokio::test]
    async fn a_process_that_does_not_read_holds_few_hand_overs_and_briefly() {
        let dir = tempfile::tempdir().unwrap();
        let config = Config {
            binary: PathBuf::from("/nonexistent/feos-authn"),
            jail: false,
        };
        let authn = Authn::with_launcher(config, dir.path(), fake::deaf_launcher());
        authn.set_deny_prefixes(Vec::new());
        authn.want(POD, true, "");
        let (_ours, theirs) = std::os::unix::net::UnixStream::pair().unwrap();
        let peer: std::net::SocketAddr = "169.254.16.5:40000".parse().unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        while let Err(e) = authn.pass(POD, 80, peer, theirs.as_fd()).await {
            assert!(Instant::now() < deadline, "never connected: {e}");
            sleep(Duration::from_millis(20)).await;
        }
        // Its socket fills; then a hand-over gives up after its time.
        let mut sent = 0;
        let failure = loop {
            let t0 = Instant::now();
            match authn.pass(POD, 80, peer, theirs.as_fd()).await {
                Ok(()) => sent += 1,
                Err(e) => {
                    assert!(t0.elapsed() < HAND_OVER_TIMEOUT * 2, "{:?}", t0.elapsed());
                    break e;
                }
            }
            assert!(sent < 100_000, "the socket never filled");
        };
        assert!(failure.contains("within"), "{failure}");
        // Past the pod's share, hand-overs are refused at once.
        let t0 = Instant::now();
        let passes = (0..HANDING_PER_POD + 8).map(|_| authn.pass(POD, 80, peer, theirs.as_fd()));
        let results = futures::future::join_all(passes).await;
        let refused = results
            .iter()
            .filter(|r| matches!(r, Err(e) if e.contains("being handed over")))
            .count();
        assert_eq!(refused, 8, "{results:?}");
        assert!(results.iter().all(Result::is_err));
        assert!(t0.elapsed() < HAND_OVER_TIMEOUT * 2, "{:?}", t0.elapsed());
        authn.remove(POD).await;
    }

    #[tokio::test]
    async fn a_process_that_exits_is_started_again() {
        let dir = tempfile::tempdir().unwrap();
        let seen = fake::Shared::default();
        let authn = authn(dir.path(), &seen);
        authn.set_issuer(issuer(Arc::new(Mutex::new(String::new()))));
        authn.want(POD, true, "");
        authn
            .wait_ready(POD, Duration::from_secs(10))
            .await
            .unwrap();
        let pid = authn.pid(POD).unwrap();
        nix::sys::signal::kill(nix::unistd::Pid::from_raw(pid), nix::sys::signal::SIGKILL).unwrap();
        within("another process", || {
            authn.pid(POD).is_some_and(|other| other != pid)
        })
        .await;
        authn.remove(POD).await;
    }

    #[tokio::test]
    async fn without_a_control_plane_a_run_is_not_ready_and_says_why() {
        let dir = tempfile::tempdir().unwrap();
        let seen = fake::Shared::default();
        let authn = authn(dir.path(), &seen);
        authn.want(POD, true, "");
        let error = authn
            .wait_ready(POD, Duration::from_millis(500))
            .await
            .unwrap_err();
        assert!(error.message().contains("no control plane"), "{error}");
        // A pod that wants none waits for nothing.
        authn
            .wait_ready("b0000000002", Duration::ZERO)
            .await
            .unwrap();
        authn.remove(POD).await;
    }

    #[test]
    fn a_feosd_that_starts_again_adopts_the_processes_it_wants() {
        let dir = tempfile::tempdir().unwrap();
        let seen = fake::Shared::default();
        let other = "c0000000003";
        // The first feosd: its runtime ends without ending the processes,
        // as feosd's does in a restart.
        let first = tokio::runtime::Runtime::new().unwrap();
        let pids = first.block_on(async {
            let authn = authn(dir.path(), &seen);
            authn.set_issuer(issuer(Arc::new(Mutex::new("v1".into()))));
            authn.want(POD, true, "v1");
            authn.want(other, true, "v1");
            authn
                .wait_ready(POD, Duration::from_secs(10))
                .await
                .unwrap();
            authn
                .wait_ready(other, Duration::from_secs(10))
                .await
                .unwrap();
            (authn.pid(POD).unwrap(), authn.pid(other).unwrap())
        });
        drop(first);
        assert!(alive(pids.0) && alive(pids.1));

        let second = tokio::runtime::Runtime::new().unwrap();
        second.block_on(async {
            let authn = authn(dir.path(), &seen);
            authn.set_issuer(issuer(Arc::new(Mutex::new("v1".into()))));
            let hellos = ops(&seen).iter().filter(|op| *op == "hello").count();
            authn.want(POD, true, "v1");
            authn.want(other, false, "v1");
            authn.end_unclaimed().await;
            authn
                .wait_ready(POD, Duration::from_secs(10))
                .await
                .unwrap();
            assert_eq!(authn.pid(POD), Some(pids.0), "adopted, not started anew");
            within("the hello again", || {
                ops(&seen).iter().filter(|op| *op == "hello").count() > hellos
            })
            .await;
            within("the unwanted one's end", || !alive(pids.1)).await;
            assert_eq!(lock(&seen).pids.len(), 2, "nothing started");
            // An adopted process is not this feosd's child: it is ended by
            // its pidfd, and its exit seen the same way.
            authn.remove(POD).await;
            within("the adopted one's end", || !alive(pids.0)).await;
            authn.remove(other).await;
        });
    }
}
