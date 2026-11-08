// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! FeID for Rust: mutual TLS with the workload's identity, whose key the
//! application never holds (FeOS ADR 0014). The application runs rustls
//! from its own sockets; its certificate comes from the signer, fetched
//! per handshake, and its CertificateVerify is signed there, over feid/1.
//!
//! ```no_run
//! # fn main() -> Result<(), feid::Error> {
//! let feid = feid::Feid::new()?;
//! // A client that checks servers by name, against the platform's CA
//! // and the WebPKI, and presents the workload's certificate.
//! let client = feid.client_config()?;
//! // A server that requires a client certificate of the platform's and
//! // presents the workload's.
//! let server = feid.server_config()?;
//! # let _ = (client, server);
//! # Ok(()) }
//! ```
//!
//! **Nothing to configure.** The signer is at [`DEFAULT_SOCKET`], or where
//! [`SOCKET_ENV`] points, for development. A pod without an identity has
//! no socket, and [`Feid::new`] says so.
//!
//! **Peers by name.** A client checks a server by its DNS name, as any
//! TLS client does. A server reads the client's SPIFFE ID with
//! [`peer_id`] and decides what it may do.
//!
//! **Any runtime.** rustls asks for its certificate and its signature in
//! calls that return a value; FeID answers them over a blocking local
//! socket in well under a millisecond, in any runtime or none. With the
//! `tokio` feature, [`Feid::connect`] and [`Feid::accept`] run the
//! handshake over tokio-rustls and send its report to the signer.
//!
//! TLS 1.3 only, Ed25519 only: the signer signs nothing it cannot read
//! as a TLS 1.3 transcript.

mod signer;
#[cfg(feature = "tokio")]
mod stream;
mod telemetry;
mod tls;

use std::fmt;
use std::io;
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant};

pub use feid_proto::HandshakeReport;
#[cfg(feature = "tokio")]
pub use stream::Endpoints;
pub use tls::{peer_id, SpiffeId};

/// Where the signer is in a pod on FeOS.
pub const DEFAULT_SOCKET: &str = "/run/feos/feid.sock";
/// The variable that names another socket, for development.
pub const SOCKET_ENV: &str = "FEID_SOCKET";

/// How long the trust bundle is used before it is fetched again. The
/// chain is fetched in every handshake: a pod that was resumed has a new
/// key, and a chain of the old one would not match its signatures.
const CACHE_TTL: Duration = Duration::from_secs(60);
/// A request's longest wait for its answer: the signer waits up to five
/// seconds for a certificate that is on its way.
const CALL_TIMEOUT: Duration = Duration::from_secs(10);
/// Attempts of a request that failed for its transport or was refused
/// retriably, and the backoff between them.
const ATTEMPTS: u32 = 4;
const BACKOFF_FIRST: Duration = Duration::from_millis(50);
const BACKOFF_MAX: Duration = Duration::from_secs(1);
/// Connections to the signer kept for the next request.
const IDLE_MAX: usize = 8;

/// What FeID failed with.
#[derive(Debug)]
pub enum Error {
    /// There is no signer: this workload has no identity.
    NoIdentity {
        socket: PathBuf,
        source: io::Error,
    },
    /// The signer refused, or could not be reached in time.
    Signer(feid_proto::Error),
    /// A certificate or the bundle from the signer that cannot be used.
    Certificate(String),
    Tls(rustls::Error),
}

impl Error {
    /// A refusal that asking again would not change.
    pub fn is_refusal(&self) -> bool {
        matches!(self, Error::Signer(e) if e.is_refusal())
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::NoIdentity { socket, source } => write!(
                f,
                "feid: no signer at {}: this workload has no identity ({source})",
                socket.display()
            ),
            Error::Signer(e) => write!(f, "{e}"),
            Error::Certificate(m) => write!(f, "feid: {m}"),
            Error::Tls(e) => write!(f, "feid: tls: {e}"),
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Error::NoIdentity { source, .. } => Some(source),
            Error::Signer(e) => Some(e),
            Error::Tls(e) => Some(e),
            Error::Certificate(_) => None,
        }
    }
}

impl From<rustls::Error> for Error {
    fn from(e: rustls::Error) -> Error {
        Error::Tls(e)
    }
}

type Conn = feid_proto::Client<UnixStream>;

/// A handle on the workload's identity. Cheap to clone; one per process
/// serves every configuration.
#[derive(Clone)]
pub struct Feid {
    inner: Arc<Inner>,
}

pub(crate) struct Inner {
    socket: PathBuf,
    name: String,
    idle: Mutex<Vec<Conn>>,
    /// The last chain fetched, as PEM and as rustls holds it.
    pub(crate) chain: Mutex<Option<(String, Arc<rustls::sign::CertifiedKey>)>>,
    pub(crate) bundle: Mutex<Option<(tls::Bundle, Instant)>>,
    reports: telemetry::Queue,
}

fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

impl Feid {
    /// The signer at [`SOCKET_ENV`]'s path, or at [`DEFAULT_SOCKET`].
    /// Dials it once: a workload without an identity learns it here, not
    /// at its first handshake.
    pub fn new() -> Result<Feid, Error> {
        let socket = std::env::var_os(SOCKET_ENV)
            .filter(|s| !s.is_empty())
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from(DEFAULT_SOCKET));
        Feid::with_socket(socket)
    }

    /// The signer at `socket`.
    pub fn with_socket(socket: impl AsRef<Path>) -> Result<Feid, Error> {
        let socket = socket.as_ref().to_path_buf();
        let name = format!("feid-rs/{}", env!("CARGO_PKG_VERSION"));
        let inner = Arc::new(Inner {
            reports: telemetry::Queue::new(socket.clone(), name.clone()),
            socket,
            name,
            idle: Mutex::new(Vec::new()),
            chain: Mutex::new(None),
            bundle: Mutex::new(None),
        });
        let first = inner.dial().map_err(|e| match e {
            Error::Signer(feid_proto::Error::Transport(source))
                if matches!(
                    source.kind(),
                    io::ErrorKind::NotFound | io::ErrorKind::ConnectionRefused
                ) =>
            {
                Error::NoIdentity {
                    socket: inner.socket.clone(),
                    source,
                }
            }
            e => e,
        })?;
        lock(&inner.idle).push(first);
        Ok(Feid { inner })
    }

    /// The workload's own SPIFFE ID, from its certificate.
    pub fn id(&self) -> Result<SpiffeId, Error> {
        let key = self.inner.certified_key()?;
        key.cert
            .first()
            .and_then(|c| SpiffeId::of_certificate(c))
            .ok_or_else(|| Error::Certificate("the workload's certificate has no SPIFFE ID".into()))
    }

    /// A rustls client: TLS 1.3, servers checked by name against the
    /// platform's trust bundle and the WebPKI's roots, the workload's
    /// certificate presented when a server asks for one.
    pub fn client_config(&self) -> Result<Arc<rustls::ClientConfig>, Error> {
        tls::client_config(self.inner.clone())
    }

    /// A rustls client that checks the server by its SPIFFE ID instead of
    /// a name, against the platform's trust bundle alone: for a peer
    /// whose certificate names no DNS name, such as a sandbox. The name a
    /// handshake is given is sent, and not checked.
    pub fn client_config_for(&self, peer: &SpiffeId) -> Result<Arc<rustls::ClientConfig>, Error> {
        tls::pinned_client_config(self.inner.clone(), peer.clone())
    }

    /// A rustls server: TLS 1.3, the workload's certificate, and a client
    /// certificate required and checked against the platform's trust
    /// bundle. Who the client is, [`peer_id`] reads.
    pub fn server_config(&self) -> Result<Arc<rustls::ServerConfig>, Error> {
        tls::server_config(self.inner.clone())
    }

    /// Sends a handshake's report to the signer, in the background. When
    /// the signer does not keep up, reports are dropped and counted.
    pub fn report(&self, report: HandshakeReport) {
        self.inner.reports.push(report);
    }

    /// How many reports were dropped.
    pub fn dropped_reports(&self) -> u64 {
        self.inner.reports.dropped()
    }
}

impl Inner {
    fn dial(&self) -> Result<Conn, Error> {
        let stream = UnixStream::connect(&self.socket)
            .map_err(|e| Error::Signer(feid_proto::Error::Transport(e)))?;
        let timeouts = stream
            .set_read_timeout(Some(CALL_TIMEOUT))
            .and_then(|_| stream.set_write_timeout(Some(CALL_TIMEOUT)));
        timeouts.map_err(|e| Error::Signer(feid_proto::Error::Transport(e)))?;
        feid_proto::Client::connect(stream, &self.name).map_err(Error::Signer)
    }

    /// One request, on an idle connection or a new one: again on a new
    /// connection when the transport failed, and after a backoff when the
    /// signer refused retriably. Any other refusal is final.
    pub(crate) fn call<T>(
        &self,
        what: &str,
        f: impl Fn(&mut Conn) -> Result<T, feid_proto::Error>,
    ) -> Result<T, Error> {
        let mut backoff = BACKOFF_FIRST;
        let mut last = None;
        for attempt in 0..ATTEMPTS {
            if attempt > 0 {
                std::thread::sleep(signer::jitter(backoff));
                backoff = (backoff * 2).min(BACKOFF_MAX);
            }
            let idle = lock(&self.idle).pop();
            let mut conn = match idle {
                Some(c) => c,
                None => match self.dial() {
                    Ok(c) => c,
                    Err(e) => {
                        log::debug!("feid: {what}: dial the signer: {e}");
                        last = Some(e);
                        continue;
                    }
                },
            };
            match f(&mut conn) {
                Ok(v) => {
                    self.keep(conn);
                    return Ok(v);
                }
                Err(
                    e @ feid_proto::Error::Remote {
                        retriable: true, ..
                    },
                ) => {
                    log::debug!("feid: {what}: {e}; asking again");
                    self.keep(conn);
                    last = Some(Error::Signer(e));
                }
                Err(e @ feid_proto::Error::Remote { .. }) => {
                    self.keep(conn);
                    return Err(Error::Signer(e));
                }
                // The connection is spent.
                Err(e) => {
                    log::debug!("feid: {what}: {e}; dialing again");
                    last = Some(Error::Signer(e));
                }
            }
        }
        Err(last.unwrap_or_else(|| {
            Error::Signer(feid_proto::Error::Transport(io::Error::other(
                "no attempt was made",
            )))
        }))
    }

    fn keep(&self, conn: Conn) {
        let mut idle = lock(&self.idle);
        if idle.len() < IDLE_MAX {
            idle.push(conn);
        }
    }

    /// The certificate chain with the delegated key, fetched from the
    /// signer, parsed again only when it changed.
    pub(crate) fn certified_key(
        self: &Arc<Self>,
    ) -> Result<Arc<rustls::sign::CertifiedKey>, Error> {
        let pem = self.call("get_certificate", |c| c.certificate())?;
        if let Some((last, key)) = lock(&self.chain).as_ref()
            && *last == pem
        {
            return Ok(key.clone());
        }
        let chain = tls::certificates(&pem)?;
        if chain.is_empty() {
            return Err(Error::Certificate("the signer sent an empty chain".into()));
        }
        let key = Arc::new(rustls::sign::CertifiedKey::new(
            chain,
            Arc::new(signer::DelegatedKey::new(self.clone())),
        ));
        *lock(&self.chain) = Some((pem, key.clone()));
        Ok(key)
    }

    /// The platform's trust bundle, fetched again after [`CACHE_TTL`].
    pub(crate) fn bundle(&self) -> Result<tls::Bundle, Error> {
        if let Some((bundle, at)) = lock(&self.bundle).as_ref()
            && at.elapsed() < CACHE_TTL
        {
            return Ok(bundle.clone());
        }
        let pem = self.call("get_ca", |c| c.trust_bundle())?;
        let bundle = tls::Bundle::from_pem(&pem)?;
        *lock(&self.bundle) = Some((bundle.clone(), Instant::now()));
        Ok(bundle)
    }
}

// The tests run handshakes over tokio.
#[cfg(all(test, feature = "tokio"))]
mod tests;
