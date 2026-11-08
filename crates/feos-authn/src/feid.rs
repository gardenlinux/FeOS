// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! FeID's signer (ADR 0014): the pod's identity for its own TLS, served
//! over feid/1 (`feid/docs/protocol.md`). The pod's TLS stack fetches its
//! certificate chain and the trust bundle and has its CertificateVerify
//! signed; the key stays in this process.
//!
//! The pod reaches it over vsock. `feosd` binds the listening socket that
//! Firecracker connects the guest's streams to vsock port 10002 on, in the
//! microVM's jail, and hands it over (`--vsock-fd`); this process accepts
//! on it, so that a `feosd` that restarts stalls no handshake. The socket
//! is the pod's: attribution is done before a byte is read. Each stream
//! begins with a one-line preamble that says what it is for, which carries
//! no authority: `feid` is feid/1, nothing else is served yet.
//!
//! What it serves comes with the credentials (`control`): the chain, the
//! bundle, and when to renew. Until a certificate is valid, and after it
//! expired, a request waits [`IDENTITY_WAIT`] for one and is then refused
//! with `unavailable.identity`, which FeID's libraries retry. A certificate
//! for another key than the run's, or without the ID it is said to carry,
//! is not taken.
//!
//! Every signature is recorded before it is given (`tls_signature`), and
//! so is every refusal of one, with its reason; a signature whose record
//! cannot be written is not made. A library's handshake reports are
//! recorded as `handshake_report`. Both carry the number of the connection
//! they came on in this run; a library reports over a connection of its
//! own, so a signature and its handshake's report share the identity and
//! the moment, not the number.

use std::io;
use std::os::fd::OwnedFd;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use base64::Engine;
use feid_proto::error::code;
use feid_proto::{CertificateVerify, HandshakeReport, Refusal, ServeOptions};
use serde::Serialize;
use tokio::io::{AsyncBufReadExt, AsyncReadExt, BufReader};
use tokio::sync::{watch, Semaphore};

use crate::audit::{self, Audit, ErrorRec};
use crate::keys::Keys;

/// How long a request waits for a valid certificate.
pub const IDENTITY_WAIT: Duration = Duration::from_secs(5);
/// The pod's streams served at once; past it, a new one is closed.
pub const MAX_STREAMS: usize = 256;
/// A preamble's most bytes, its newline included, and how long it may
/// take to arrive.
const PREAMBLE_MAX: u64 = 64;
const PREAMBLE_WAIT: Duration = Duration::from_secs(5);
/// The preamble of a feid/1 stream.
pub const PREAMBLE_FEID: &str = "feid";
/// A field of a handshake report is cut to this many characters in the
/// audit, so that a library cannot fill the file with one report.
const REPORT_FIELD_MAX: usize = 512;
const REPORT_LIST_MAX: usize = 64;

/// The pod's certificate, as the control plane issued it for the run's key.
#[derive(Clone, Debug, PartialEq)]
pub struct Identity {
    pub spiffe_id: String,
    /// The chain, PEM, the pod's certificate first.
    pub chain_pem: String,
    /// The trust bundle, PEM.
    pub bundle_pem: String,
    /// Unix seconds.
    pub not_before: i64,
    pub not_after: i64,
    pub renew_after: i64,
}

#[derive(serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct Relayed {
    spiffe_id: String,
    chain: Vec<String>,
    #[serde(default)]
    trust_bundle: Vec<String>,
    not_before: i64,
    not_after: i64,
    #[serde(default)]
    renew_after: i64,
}

fn pem(der: &[u8]) -> String {
    let b64 = base64::engine::general_purpose::STANDARD.encode(der);
    let mut out = String::from("-----BEGIN CERTIFICATE-----\n");
    for line in b64.as_bytes().chunks(64) {
        out.push_str(std::str::from_utf8(line).unwrap_or_default());
        out.push('\n');
    }
    out.push_str("-----END CERTIFICATE-----\n");
    out
}

impl Identity {
    /// The certificate `feosd` relayed with the credentials, checked
    /// against the run's key: its first certificate must be for the
    /// identity key and carry the SPIFFE ID as its URI name.
    pub fn from_relayed(certificate: &serde_json::Value, keys: &Keys) -> Result<Identity, String> {
        use x509_parser::prelude::{FromDer, GeneralName, X509Certificate};
        let r: Relayed = serde_json::from_value(certificate.clone())
            .map_err(|e| format!("the certificate: {e}"))?;
        let b64 = base64::engine::general_purpose::STANDARD;
        let decode = |list: &[String], what: &str| -> Result<Vec<Vec<u8>>, String> {
            list.iter()
                .map(|c| b64.decode(c).map_err(|e| format!("the {what}: {e}")))
                .collect()
        };
        let chain = decode(&r.chain, "chain")?;
        let bundle = decode(&r.trust_bundle, "trust bundle")?;
        let leaf = chain.first().ok_or("the chain is empty")?;
        let (_, cert) = X509Certificate::from_der(leaf)
            .map_err(|e| format!("the pod's certificate does not parse: {e}"))?;
        if cert.public_key().subject_public_key.data.as_ref() != keys.identity_public() {
            return Err("the pod's certificate is for another key than this run's".into());
        }
        let names = cert
            .subject_alternative_name()
            .ok()
            .flatten()
            .map(|san| san.value.general_names.clone())
            .unwrap_or_default();
        if !names
            .iter()
            .any(|n| matches!(n, GeneralName::URI(u) if *u == r.spiffe_id))
        {
            return Err(format!(
                "the pod's certificate does not carry {}",
                r.spiffe_id
            ));
        }
        if bundle.is_empty() {
            return Err("the certificate comes without a trust bundle".into());
        }
        Ok(Identity {
            spiffe_id: r.spiffe_id,
            chain_pem: chain.iter().map(|c| pem(c)).collect(),
            bundle_pem: bundle.iter().map(|c| pem(c)).collect(),
            not_before: r.not_before,
            not_after: r.not_after,
            renew_after: r.renew_after,
        })
    }

    fn valid_at(&self, unix: i64) -> bool {
        self.not_before <= unix && unix < self.not_after
    }
}

fn unix_now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs() as i64)
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct SignatureRecord<'a> {
    ts: String,
    #[serde(rename = "type")]
    kind: &'static str,
    /// The pod's SPIFFE ID; null when it had none.
    identity: Option<&'a str>,
    /// "client" or "server": whose CertificateVerify; null for a refusal
    /// before the input was read as one.
    role: Option<&'static str>,
    conn: u64,
    error: Option<ErrorRec>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ReportRecord<'a> {
    ts: String,
    #[serde(rename = "type")]
    kind: &'static str,
    identity: Option<&'a str>,
    conn: u64,
    report: HandshakeReport,
}

fn cut(s: &mut String) {
    if s.chars().count() > REPORT_FIELD_MAX {
        *s = s.chars().take(REPORT_FIELD_MAX).collect();
    }
}

fn bounded(mut r: HandshakeReport) -> HandshakeReport {
    for s in [
        &mut r.direction,
        &mut r.tuple,
        &mut r.version,
        &mut r.cipher_suite,
        &mut r.sni,
        &mut r.alpn,
        &mut r.peer_subject,
        &mut r.peer_spiffe,
        &mut r.peer_issuer,
        &mut r.peer_serial,
        &mut r.peer_not_after,
        &mut r.peer_fingerprint,
        &mut r.offered_version,
    ] {
        cut(s);
    }
    for list in [
        &mut r.offered_ciphers,
        &mut r.offered_kex,
        &mut r.offered_alpn,
    ] {
        list.truncate(REPORT_LIST_MAX);
        list.iter_mut().for_each(cut);
    }
    r
}

/// The pod's signer: the run's key, the identity in place, the audit.
pub struct Signer {
    keys: Arc<Keys>,
    audit: Arc<Audit>,
    identity: watch::Receiver<Option<Arc<Identity>>>,
    wait: Duration,
    conns: AtomicU64,
    /// An expiry is said once, not per request.
    expired_said: AtomicBool,
    streams: Arc<Semaphore>,
}

impl Signer {
    pub fn new(
        keys: Arc<Keys>,
        audit: Arc<Audit>,
        identity: watch::Receiver<Option<Arc<Identity>>>,
    ) -> Signer {
        Signer {
            keys,
            audit,
            identity,
            wait: IDENTITY_WAIT,
            conns: AtomicU64::new(0),
            expired_said: AtomicBool::new(false),
            streams: Arc::new(Semaphore::new(MAX_STREAMS)),
        }
    }

    /// With another wait for a valid certificate, for tests.
    pub fn with_wait(mut self, wait: Duration) -> Signer {
        self.wait = wait;
        self
    }

    /// The identity valid now, waiting up to the wait for one.
    async fn valid(&self) -> Result<Arc<Identity>, Refusal> {
        let mut rx = self.identity.clone();
        let found = tokio::time::timeout(self.wait, async {
            loop {
                if let Some(id) = rx.borrow_and_update().clone()
                    && id.valid_at(unix_now())
                {
                    return Some(id);
                }
                if rx.changed().await.is_err() {
                    return None;
                }
            }
        })
        .await
        .ok()
        .flatten();
        match found {
            Some(id) => {
                self.expired_said.store(false, Ordering::Relaxed);
                Ok(id)
            }
            None => {
                let current = self.identity.borrow().clone();
                let why = match &current {
                    Some(id) if id.not_after <= unix_now() => {
                        if !self.expired_said.swap(true, Ordering::Relaxed) {
                            log::error!(
                                "feid: the pod's certificate for {} expired at {}: its \
                                 signatures are refused until a new one comes",
                                id.spiffe_id,
                                id.not_after
                            );
                        }
                        "the pod's certificate expired; a new one has not come"
                    }
                    Some(_) => "the pod's certificate is not valid yet",
                    None => "the pod has no certificate yet",
                };
                Err(Refusal::new(code::UNAVAILABLE_IDENTITY, why, true))
            }
        }
    }

    /// Serves the pod's streams on the vsock listener, for as long as the
    /// process runs.
    pub async fn serve(self: Arc<Self>, listener: OwnedFd) -> io::Result<()> {
        let listener = std::os::unix::net::UnixListener::from(listener);
        listener.set_nonblocking(true)?;
        let listener = tokio::net::UnixListener::from_std(listener)?;
        loop {
            let stream = match listener.accept().await {
                Ok((s, _)) => s,
                Err(e) => {
                    log::error!("vsock: accept: {e}");
                    tokio::time::sleep(Duration::from_millis(100)).await;
                    continue;
                }
            };
            let Ok(permit) = self.streams.clone().try_acquire_owned() else {
                log::warn!("vsock: {MAX_STREAMS} of the pod's streams are open; closed one");
                continue;
            };
            let signer = self.clone();
            tokio::spawn(async move {
                let _permit = permit;
                signer.stream(stream).await;
            });
        }
    }

    /// One stream: its preamble, then what it names.
    async fn stream(&self, stream: tokio::net::UnixStream) {
        let mut stream = BufReader::new(stream);
        let mut line = Vec::new();
        let mut limited = (&mut stream).take(PREAMBLE_MAX);
        let read = limited.read_until(b'\n', &mut line);
        match tokio::time::timeout(PREAMBLE_WAIT, read).await {
            Ok(Ok(_)) if line.last() == Some(&b'\n') => {}
            Ok(Ok(_)) => {
                log::warn!("vsock: a stream without a preamble; closed");
                return;
            }
            Ok(Err(e)) => {
                log::debug!("vsock: reading a preamble: {e}");
                return;
            }
            Err(_) => {
                log::debug!("vsock: no preamble within {PREAMBLE_WAIT:?}; closed");
                return;
            }
        }
        let preamble = String::from_utf8_lossy(&line[..line.len() - 1]);
        match preamble.trim_end_matches('\r') {
            PREAMBLE_FEID => {
                let conn = self.conns.fetch_add(1, Ordering::Relaxed) + 1;
                let handler = Conn { signer: self, conn };
                let options =
                    ServeOptions::named(concat!("feos-authn/", env!("CARGO_PKG_VERSION")));
                feid_proto::serve(stream, &handler, &options).await;
            }
            other => {
                let shown: String = other.chars().take(32).collect();
                log::warn!("vsock: a stream for {shown:?}, which is not served; closed");
            }
        }
    }

    fn record_signature(
        &self,
        identity: Option<&str>,
        role: Option<&'static str>,
        conn: u64,
        error: Option<&Refusal>,
    ) -> io::Result<()> {
        self.audit.write(&SignatureRecord {
            ts: audit::now(),
            kind: "tls_signature",
            identity,
            role,
            conn,
            error: error.map(|r| ErrorRec {
                code: r.code.clone(),
                message: r.message.clone(),
            }),
        })
    }
}

/// One feid/1 connection's handler: the signer and the connection's number.
struct Conn<'a> {
    signer: &'a Signer,
    conn: u64,
}

impl feid_proto::Handler for Conn<'_> {
    async fn certificate(&self) -> Result<String, Refusal> {
        Ok(self.signer.valid().await?.chain_pem.clone())
    }

    async fn trust_bundle(&self) -> Result<String, Refusal> {
        Ok(self.signer.valid().await?.bundle_pem.clone())
    }

    async fn sign(&self, cv: CertificateVerify<'_>) -> Result<Vec<u8>, Refusal> {
        let role = cv.role.as_str();
        let identity = match self.signer.valid().await {
            Ok(id) => id,
            Err(r) => {
                if let Err(e) = self
                    .signer
                    .record_signature(None, Some(role), self.conn, Some(&r))
                {
                    log::error!("audit: a refused signature: {e}");
                }
                return Err(r);
            }
        };
        if let Err(e) =
            self.signer
                .record_signature(Some(&identity.spiffe_id), Some(role), self.conn, None)
        {
            log::error!("audit: a signature, which is therefore not given: {e}");
            return Err(Refusal::new(
                code::UNAVAILABLE_AUDIT_LOG,
                "the signature's audit record cannot be written",
                true,
            ));
        }
        self.signer
            .keys
            .sign_identity(cv.message)
            .map_err(Refusal::internal)
    }

    fn sign_refused(&self, refusal: &Refusal) {
        let identity = self.signer.identity.borrow().clone();
        let id = identity.as_ref().map(|i| i.spiffe_id.as_str());
        if let Err(e) = self
            .signer
            .record_signature(id, None, self.conn, Some(refusal))
        {
            log::error!("audit: a refused signature: {e}");
        }
        log::warn!("feid: a signature refused: {}", refusal.message);
    }

    fn handshake_report(&self, report: HandshakeReport) {
        let identity = self.signer.identity.borrow().clone();
        let rec = ReportRecord {
            ts: audit::now(),
            kind: "handshake_report",
            identity: identity.as_ref().map(|i| i.spiffe_id.as_str()),
            conn: self.conn,
            report: bounded(report),
        };
        if let Err(e) = self.signer.audit.write(&rec) {
            log::warn!("audit: a handshake report: {e}");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::dest::Net;
    use crate::testkit::{
        await_record, records, seqpacket_listener, Feosd, IdentityPki, MapResolver, POD_ID,
    };
    use crate::Authn;
    use feid_proto::Client;
    use std::io::Write;
    use std::os::unix::net::{UnixListener, UnixStream};
    use std::path::{Path, PathBuf};

    const SPIFFE: &str = "spiffe://run.example/tenant/acme/env/prod/service/api";

    /// A CertificateVerify input of the client.
    fn cv_input(hash: &[u8]) -> Vec<u8> {
        let mut m = vec![0x20u8; 64];
        m.extend_from_slice(b"TLS 1.3, client CertificateVerify\0");
        m.extend_from_slice(hash);
        m
    }

    struct Pod {
        _dir: tempfile::TempDir,
        vsock: PathBuf,
        audit: PathBuf,
        feosd: Feosd,
        authn: Arc<Authn>,
    }

    /// A feos-authn with its control socket, its audit file and its vsock
    /// listener, as feosd starts it, and feosd's hello said.
    async fn pod(wait: Duration) -> Pod {
        let dir = tempfile::tempdir().unwrap();
        let control = dir.path().join("authn.sock");
        let listener = seqpacket_listener(&control);
        let audit = dir.path().join("authn-audit.ndjson");
        let file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&audit)
            .unwrap();
        let vsock = dir.path().join("v.sock_10002");
        let vsock_listener = UnixListener::bind(&vsock).unwrap();
        let authn = Arc::new(Authn::new(
            POD_ID.into(),
            Keys::generate().unwrap(),
            Arc::new(Audit::new(file.into())),
            Arc::new(Net::allowing_loopback(Arc::new(MapResolver::default()))),
        ));
        let signer = Arc::new(authn.signer().with_wait(wait));
        tokio::spawn(async move {
            let _ = signer.serve(OwnedFd::from(vsock_listener)).await;
        });
        let runner = authn.clone();
        tokio::spawn(async move {
            let _ = runner.run(listener).await;
        });
        let feosd = Feosd::connect(&control);
        feosd.hello(&[], None);
        Pod {
            _dir: dir,
            vsock,
            audit,
            feosd,
            authn,
        }
    }

    /// A stream to the signer as the guest pipes one: its preamble first.
    fn dial(path: &Path, preamble: &[u8]) -> UnixStream {
        let mut s = UnixStream::connect(path).unwrap();
        s.set_read_timeout(Some(Duration::from_secs(10))).unwrap();
        s.write_all(preamble).unwrap();
        s
    }

    async fn blocking<T: Send + 'static>(f: impl FnOnce() -> T + Send + 'static) -> T {
        tokio::task::spawn_blocking(f).await.unwrap()
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_pod_gets_its_chain_and_bundle_and_signatures_by_its_key() {
        let pod = pod(IDENTITY_WAIT).await;
        let pki = IdentityPki::new();
        pod.feosd.answer_identity(&pki, SPIFFE, 2700);
        pod.feosd.ready("");
        let public = pod.authn.keys.identity_public().to_vec();
        let vsock = pod.vsock.clone();
        let (chain, bundle) = blocking(move || {
            let mut c = Client::connect(dial(&vsock, b"feid\n"), "test").unwrap();
            assert!(c.server().starts_with("feos-authn/"));
            let chain = c.certificate().unwrap();
            let bundle = c.trust_bundle().unwrap();
            let message = cv_input(&[9; 32]);
            let sig = c.sign(&message).unwrap();
            ring::signature::UnparsedPublicKey::new(&ring::signature::ED25519, &public)
                .verify(&message, &sig)
                .expect("a signature by the run's identity key");
            // Something that is no CertificateVerify is refused, and the
            // connection goes on.
            let e = c.sign(b"a contract").unwrap_err();
            assert_eq!(e.code(), Some(code::REFUSED_NOT_CERTIFICATE_VERIFY));
            c.report(&HandshakeReport {
                direction: "outbound".into(),
                peer_spiffe: "x".repeat(10_000),
                ..Default::default()
            })
            .unwrap();
            assert!(c.trust_bundle().is_ok());
            (chain, bundle)
        })
        .await;
        assert_eq!(chain.matches("BEGIN CERTIFICATE").count(), 1);
        let b64 = base64::engine::general_purpose::STANDARD.encode(&pki.ca_der);
        assert!(bundle.replace('\n', "").contains(&b64[..60]));

        let signed = await_record(&pod.audit, |r| {
            r["type"] == "tls_signature" && r["error"].is_null()
        })
        .await;
        assert_eq!(signed["identity"], SPIFFE);
        assert_eq!(signed["role"], "client");
        assert_eq!(signed["conn"], 1);
        let refused = await_record(&pod.audit, |r| {
            r["type"] == "tls_signature" && !r["error"].is_null()
        })
        .await;
        assert_eq!(
            refused["error"]["code"],
            code::REFUSED_NOT_CERTIFICATE_VERIFY
        );
        assert!(refused["role"].is_null());
        let report = await_record(&pod.audit, |r| r["type"] == "handshake_report").await;
        assert_eq!(report["conn"], 1);
        assert_eq!(
            report["report"]["peer_spiffe"].as_str().unwrap().len(),
            REPORT_FIELD_MAX
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn without_a_certificate_a_request_waits_and_is_refused_retriably() {
        let pod = pod(Duration::from_millis(200)).await;
        let vsock = pod.vsock.clone();
        let e = blocking(move || {
            let mut c = Client::connect(dial(&vsock, b"feid\n"), "test").unwrap();
            c.sign(&cv_input(&[1; 48])).unwrap_err()
        })
        .await;
        assert_eq!(e.code(), Some(code::UNAVAILABLE_IDENTITY));
        assert!(e.is_retriable_refusal());
        let r = await_record(&pod.audit, |r| r["type"] == "tls_signature").await;
        assert_eq!(r["error"]["code"], code::UNAVAILABLE_IDENTITY);
        assert!(r["identity"].is_null());
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_request_waiting_for_the_certificate_is_answered_when_it_comes() {
        let pod = pod(Duration::from_secs(10)).await;
        let vsock = pod.vsock.clone();
        let asked = tokio::spawn(blocking(move || {
            let mut c = Client::connect(dial(&vsock, b"feid\n"), "test").unwrap();
            c.certificate()
        }));
        tokio::time::sleep(Duration::from_millis(200)).await;
        pod.feosd.answer_identity(&IdentityPki::new(), SPIFFE, 2700);
        let chain = asked.await.unwrap().unwrap();
        assert!(chain.contains("BEGIN CERTIFICATE"));
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn the_certificate_is_renewed_after_its_renew_after() {
        let pod = pod(IDENTITY_WAIT).await;
        let pki = IdentityPki::new();
        // Due in a second: feos-authn asks again with the same keys.
        let first = pod.feosd.answer_identity(&pki, SPIFFE, 1);
        pod.feosd.ready("");
        let second = pod.feosd.answer_identity(&pki, SPIFFE, 2700);
        assert_eq!(first["csr"], second["csr"]);
        assert_eq!(first["hpkePublicKey"], second["hpkePublicKey"]);
        pod.feosd.ready("");
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_certificate_for_another_key_or_id_is_not_taken() {
        let keys = Keys::generate().unwrap();
        let pki = IdentityPki::new();
        let good = pki.issue(keys.csr_der(), SPIFFE, 2700);
        let id = Identity::from_relayed(&good, &keys).unwrap();
        assert_eq!(id.spiffe_id, SPIFFE);
        assert!(id.valid_at(unix_now()));
        let other = Keys::generate().unwrap();
        let e = Identity::from_relayed(&good, &other).unwrap_err();
        assert!(e.contains("another key"), "{e}");
        let mut wrong_id = good.clone();
        wrong_id["spiffeId"] = "spiffe://run.example/tenant/evil".into();
        let e = Identity::from_relayed(&wrong_id, &keys).unwrap_err();
        assert!(e.contains("does not carry"), "{e}");
        let mut no_bundle = good.clone();
        no_bundle["trustBundle"] = serde_json::json!([]);
        assert!(Identity::from_relayed(&no_bundle, &keys).is_err());
        let mut empty = good;
        empty["chain"] = serde_json::json!([]);
        assert!(Identity::from_relayed(&empty, &keys).is_err());
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_signature_whose_record_cannot_be_written_is_not_made() {
        let keys = Arc::new(Keys::generate().unwrap());
        let pki = IdentityPki::new();
        let id = Identity::from_relayed(&pki.issue(keys.csr_der(), SPIFFE, 2700), &keys).unwrap();
        let (tx, rx) = watch::channel(Some(Arc::new(id)));
        // A file open for reading only: every write fails.
        let tmp = tempfile::NamedTempFile::new().unwrap();
        let ro = std::fs::File::open(tmp.path()).unwrap();
        let signer = Signer::new(keys, Arc::new(Audit::new(ro.into())), rx);
        let conn = Conn {
            signer: &signer,
            conn: 1,
        };
        let input = cv_input(&[3; 32]);
        let cv = feid_proto::cv::check(&input).unwrap();
        let r = feid_proto::Handler::sign(&conn, cv).await.unwrap_err();
        assert_eq!(r.code, code::UNAVAILABLE_AUDIT_LOG);
        assert!(r.retriable);
        drop(tx);
    }

    /// What the guest does for a container: a socket whose streams it
    /// pipes to the vsock port after the preamble.
    fn relay(dir: &Path, vsock: PathBuf) -> PathBuf {
        let path = dir.join("feid.sock");
        let listener = tokio::net::UnixListener::bind(&path).unwrap();
        tokio::spawn(async move {
            while let Ok((mut guest, _)) = listener.accept().await {
                let vsock = vsock.clone();
                tokio::spawn(async move {
                    use tokio::io::AsyncWriteExt;
                    let mut host = tokio::net::UnixStream::connect(&vsock).await.unwrap();
                    host.write_all(b"feid\n").await.unwrap();
                    let _ = tokio::io::copy_bidirectional(&mut guest, &mut host).await;
                });
            }
        });
        path
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn two_pods_handshake_through_feid_with_their_keys_in_feos_authn() {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        const WEB: &str = "spiffe://run.example/tenant/acme/env/prod/service/web";
        let pki = IdentityPki::new();
        let (web, api) = (pod(IDENTITY_WAIT).await, pod(IDENTITY_WAIT).await);
        web.feosd
            .answer_named(&pki, WEB, &["web.acme.internal"], 2700);
        api.feosd
            .answer_named(&pki, SPIFFE, &["api.acme.internal"], 2700);
        web.feosd.ready("");
        api.feosd.ready("");
        let web_socket = relay(web._dir.path(), web.vsock.clone());
        let api_socket = relay(api._dir.path(), api.vsock.clone());
        let (client, server, cc, sc) = blocking(move || {
            let client = feid::Feid::with_socket(web_socket).unwrap();
            let server = feid::Feid::with_socket(api_socket).unwrap();
            let cc = client.client_config().unwrap();
            let sc = server.server_config().unwrap();
            (client, server, cc, sc)
        })
        .await;
        let (c_io, s_io) = tokio::io::duplex(1 << 16);
        let accepted = tokio::spawn(async move {
            let mut tls = server.accept(sc, s_io).await.unwrap();
            let caller = feid::peer_id(tls.get_ref().1).unwrap();
            let mut buf = [0u8; 4];
            tls.read_exact(&mut buf).await.unwrap();
            tls.write_all(b"pong").await.unwrap();
            tls.flush().await.unwrap();
            caller
        });
        let name = rustls::pki_types::ServerName::try_from("api.acme.internal").unwrap();
        let mut tls = client.connect(cc, name, c_io).await.unwrap();
        let callee = feid::peer_id(tls.get_ref().1).unwrap();
        tls.write_all(b"ping").await.unwrap();
        tls.flush().await.unwrap();
        let mut buf = [0u8; 4];
        tls.read_exact(&mut buf).await.unwrap();
        assert_eq!(&buf, b"pong");
        let caller = accepted.await.unwrap();
        assert_eq!(caller.as_str(), WEB);
        assert_eq!(callee.as_str(), SPIFFE);

        // Each pod's feos-authn signed its side and recorded it first,
        // and each library's report came to its own pod.
        let signed = |r: &serde_json::Value, role: &str| {
            r["type"] == "tls_signature" && r["role"] == role && r["error"].is_null()
        };
        let w = await_record(&web.audit, |r| signed(r, "client")).await;
        assert_eq!(w["identity"], WEB);
        let a = await_record(&api.audit, |r| signed(r, "server")).await;
        assert_eq!(a["identity"], SPIFFE);
        let out = await_record(&web.audit, |r| r["type"] == "handshake_report").await;
        assert_eq!(out["report"]["direction"], "outbound");
        assert_eq!(out["report"]["peer_spiffe"], SPIFFE);
        let inbound = await_record(&api.audit, |r| r["type"] == "handshake_report").await;
        assert_eq!(inbound["report"]["peer_spiffe"], WEB);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn feosd_hands_the_vsock_listener_over_and_a_new_one_replaces_it() {
        use std::os::fd::AsRawFd;
        use std::os::unix::fs::MetadataExt;
        let pod = pod(IDENTITY_WAIT).await;
        pod.feosd.answer_identity(&IdentityPki::new(), SPIFFE, 2700);
        pod.feosd.ready("");
        let dir = pod._dir.path();
        let (a, b) = (dir.join("a.sock"), dir.join("b.sock"));
        for path in [&a, &b] {
            let listener = UnixListener::bind(path).unwrap();
            let inode = std::fs::metadata(path).unwrap().ino();
            pod.feosd.send(
                &serde_json::json!({"v": 1, "op": "vsock", "inode": inode}),
                &[listener.as_raw_fd()],
            );
            let said = pod.feosd.recv();
            assert_eq!(
                said,
                serde_json::json!({"v": 1, "op": "vsock", "inode": inode})
            );
            let path = path.clone();
            let chain = blocking(move || {
                Client::connect(dial(&path, b"feid\n"), "test")
                    .unwrap()
                    .certificate()
                    .unwrap()
            })
            .await;
            assert!(chain.contains("BEGIN CERTIFICATE"));
        }
        // The one before is closed.
        assert!(UnixStream::connect(&a).is_err());
        // A new feosd hears which one the process holds.
        pod.feosd.hello(&[], None);
        assert_eq!(pod.feosd.recv()["op"], "ready");
        let inode = std::fs::metadata(&b).unwrap().ino();
        assert_eq!(
            pod.feosd.recv(),
            serde_json::json!({"v": 1, "op": "vsock", "inode": inode})
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_stream_for_anything_else_is_closed() {
        let pod = pod(IDENTITY_WAIT).await;
        let vsock = pod.vsock.clone();
        let closed = blocking(move || {
            let mut s = dial(&vsock, b"kube 6443\n");
            let mut buf = [0u8; 8];
            matches!(std::io::Read::read(&mut s, &mut buf), Ok(0))
        })
        .await;
        assert!(closed);
        assert!(records(&pod.audit)
            .iter()
            .all(|r| r["type"] != "tls_signature"));
    }
}
