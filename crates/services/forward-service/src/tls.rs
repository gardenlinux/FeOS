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
//! for the container runtime). PID 1 writes the file, starts the process,
//! and starts it again when it ends; a set that changes the bindings, or
//! resets them after a resume, ends the process before and starts another.

use std::io;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use log::{error, info, warn};
use serde::{Deserialize, Serialize};

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

/// One binding, as the host pushes it (`SetFeid`'s `tls_bindings`).
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Binding {
    /// The loopback address and port the guest listens on, `127.x.y.z:p`.
    pub listen: String,
    /// The upstream: a name the pod's resolver answers, or an address.
    pub host: String,
    pub port: u16,
    /// The ALPN protocol the guest offers, `postgresql`.
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
        if self.alpn.is_empty() || self.alpn.len() > 255 {
            return Err(format!("ALPN {:?} is not one", self.alpn));
        }
        if feid::SpiffeId::parse(&self.server_id).is_none() {
            return Err(format!("server ID {:?} is no SPIFFE ID", self.server_id));
        }
        Ok(())
    }
}

/// PID 1's side: the bindings the host pushed, and their process.
pub struct TlsBindings {
    dir: PathBuf,
    /// The binary to run; `None`: this one.
    binary: Option<PathBuf>,
    state: Arc<Mutex<State>>,
}

#[derive(Default)]
struct State {
    bindings: Vec<Binding>,
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
        }
    }

    /// Serves `bindings` from now on, none for an empty list. A set of the
    /// bindings that are served already changes nothing, unless `reset`,
    /// after a boot or a resume, which ends their connections too. Returns
    /// how many are served.
    pub async fn set(&self, bindings: Vec<Binding>, reset: bool) -> Result<usize, String> {
        for b in &bindings {
            b.check()?;
        }
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
            let _ = std::fs::remove_file(self.dir.join(CONFIG));
            let _ = std::fs::remove_file(self.dir.join(PID_FILE));
            info!("TlsBindings: The pod has no TLS bindings");
            return Ok(0);
        }
        write_config(&self.dir, &bindings).map_err(|e| format!("TLS bindings' file: {e}"))?;
        let generation = self
            .state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .generation;
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
        if feos_utils::reaper::is_running() {
            // The reaper owns waitpid in PID 1: ask it for the exit code.
            let child = command.spawn()?;
            let pid = child.id() as i32;
            if !self.started(pid) {
                return Ok(());
            }
            let code = feos_utils::reaper::wait_for_exit(pid).await;
            warn!("TlsBindings: The TLS bindings' process (pid {pid}) exited with code {code}");
        } else {
            let mut child = tokio::process::Command::from(command).spawn()?;
            let pid = child.id().unwrap_or_default() as i32;
            if !self.started(pid) {
                return Ok(());
            }
            let status = child.wait().await?;
            warn!("TlsBindings: The TLS bindings' process (pid {pid}) exited: {status}");
        }
        Ok(())
    }

    /// Records a started process as the current one; a process that a set
    /// made stale while it started is ended at once.
    fn started(&self, pid: i32) -> bool {
        let mut st = self.state.lock().unwrap_or_else(|e| e.into_inner());
        if st.generation != self.generation {
            drop(st);
            signal(pid, nix::sys::signal::Signal::SIGKILL);
            return false;
        }
        st.pid = Some(pid);
        drop(st);
        if let Err(e) = std::fs::write(self.dir.join(PID_FILE), pid.to_string()) {
            warn!("TlsBindings: Failed to write the pid file: {e}");
        }
        info!("TlsBindings: Started the TLS bindings' process (pid {pid})");
        true
    }
}

fn write_config(dir: &Path, bindings: &[Binding]) -> io::Result<()> {
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

/// The bindings' process: serves the bindings of `config` until one of
/// them cannot serve any more, which ends the process (PID 1 starts it
/// again).
pub async fn serve(config: &Path, feid_socket: &Path) -> Result<(), String> {
    let bindings: Vec<Binding> = serde_json::from_slice(
        &std::fs::read(config).map_err(|e| format!("{}: {e}", config.display()))?,
    )
    .map_err(|e| format!("{}: {e}", config.display()))?;
    let socket = feid_socket.to_path_buf();
    let feid = tokio::task::spawn_blocking(move || feid::Feid::with_socket(socket))
        .await
        .map_err(|e| format!("FeID: {e}"))?
        .map_err(|e| format!("FeID's socket {}: {e}", feid_socket.display()))?;
    let feid = Arc::new(feid);
    let mut tasks = tokio::task::JoinSet::new();
    for b in bindings {
        b.check()?;
        let id = feid::SpiffeId::parse(&b.server_id)
            .ok_or_else(|| format!("server ID {:?} is no SPIFFE ID", b.server_id))?;
        // The configuration asks the signer for the bundle: blocking.
        let f = feid.clone();
        let mut config = (*tokio::task::spawn_blocking(move || f.client_config_for(&id))
            .await
            .map_err(|e| format!("FeID: {e}"))?
            .map_err(|e| format!("FeID's client configuration: {e}"))?)
        .clone();
        config.alpn_protocols = vec![b.alpn.clone().into_bytes()];
        let listener = tokio::net::TcpListener::bind(&b.listen)
            .await
            .map_err(|e| format!("listen on {}: {e}", b.listen))?;
        info!(
            "TlsBindings: {} → {}:{} as {} ({})",
            b.listen, b.host, b.port, b.server_id, b.alpn
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
                alpn: String::new(),
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
        let listener = tokio::net::UnixListener::bind(&socket).unwrap();
        tokio::spawn(async move {
            while let Ok((stream, _)) = listener.accept().await {
                let signer = signer.clone();
                tokio::spawn(async move {
                    let options = feid_proto::ServeOptions::named("test-signer");
                    feid_proto::serve(stream, signer.as_ref(), &options).await;
                });
            }
        });
        socket
    }

    /// The upstream, as PostgreSQL 17 with direct TLS: requires a client
    /// certificate under the root, takes ALPN postgresql, and answers
    /// each line with the client's URI and the line.
    async fn upstream(ca: &Ca, spiffe: &str) -> SocketAddr {
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
        config.alpn_protocols = vec![b"postgresql".to_vec()];
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
                    assert_eq!(conn.alpn_protocol(), Some(&b"postgresql"[..]));
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

    async fn serving(dir: &Path, socket: &Path, bindings: &[Binding]) {
        let config = dir.join(CONFIG);
        std::fs::write(&config, serde_json::to_vec(bindings).unwrap()).unwrap();
        let socket = socket.to_path_buf();
        tokio::spawn(async move { serve(&config, &socket).await });
        for b in bindings {
            for _ in 0..200 {
                if tokio::net::TcpStream::connect(&b.listen).await.is_ok() {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        }
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
        serving(dir.path(), &socket, &[ok.clone(), pinned.clone()]).await;

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
}
