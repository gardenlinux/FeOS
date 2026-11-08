// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Handshakes between two workloads, each with a signer of its own on a
//! Unix socket that holds its key, as feos-authn does.

use std::collections::VecDeque;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use feid_proto::error::code;
use feid_proto::{CertificateVerify, HandshakeReport, Refusal, Role};
use rcgen::SigningKey;
use rustls::pki_types::ServerName;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

use crate::{peer_id, tls, Error, Feid, SpiffeId};

const WEB: &str = "spiffe://run.example/tenant/acme/env/prod/service/web";
const API: &str = "spiffe://run.example/tenant/acme/env/prod/service/api";

struct Ca {
    issuer: rcgen::Issuer<'static, rcgen::KeyPair>,
    pem: String,
}

impl Ca {
    fn new(name: &str) -> Ca {
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let mut p = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        p.distinguished_name.push(rcgen::DnType::CommonName, name);
        p.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        p.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let pem = p.self_signed(&key).unwrap().pem();
        Ca {
            issuer: rcgen::Issuer::new(p, key),
            pem,
        }
    }

    /// A workload's key and certificate, as the platform issues them.
    fn workload(&self, spiffe: &str, dns: &str) -> (rcgen::KeyPair, String) {
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let mut p = rcgen::CertificateParams::new(vec![dns.to_string()]).unwrap();
        p.subject_alt_names
            .push(rcgen::SanType::URI(spiffe.try_into().unwrap()));
        p.distinguished_name.push(
            rcgen::DnType::CommonName,
            spiffe.trim_start_matches("spiffe://"),
        );
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

/// A signer on a Unix socket, holding one workload's key.
struct Signer {
    key: rcgen::KeyPair,
    chain: String,
    bundle: String,
    signed: Mutex<Vec<Role>>,
    attempts: AtomicU32,
    refusals: Mutex<VecDeque<Refusal>>,
    reports: Mutex<Vec<HandshakeReport>>,
}

impl feid_proto::Handler for Signer {
    async fn certificate(&self) -> Result<String, Refusal> {
        Ok(self.chain.clone())
    }

    async fn trust_bundle(&self) -> Result<String, Refusal> {
        Ok(self.bundle.clone())
    }

    async fn sign(&self, cv: CertificateVerify<'_>) -> Result<Vec<u8>, Refusal> {
        self.attempts.fetch_add(1, Ordering::Relaxed);
        if let Some(r) = self.refusals.lock().unwrap().pop_front() {
            return Err(r);
        }
        self.signed.lock().unwrap().push(cv.role);
        self.key
            .sign(cv.message)
            .map_err(|e| Refusal::internal(e.to_string()))
    }

    fn handshake_report(&self, report: HandshakeReport) {
        self.reports.lock().unwrap().push(report);
    }
}

struct Workload {
    _dir: tempfile::TempDir,
    socket: PathBuf,
    signer: Arc<Signer>,
}

fn workload(ca: &Ca, spiffe: &str, dns: &str, bundle: &str) -> Workload {
    let (key, chain) = ca.workload(spiffe, dns);
    let signer = Arc::new(Signer {
        key,
        chain,
        bundle: bundle.to_string(),
        signed: Mutex::default(),
        attempts: AtomicU32::new(0),
        refusals: Mutex::default(),
        reports: Mutex::default(),
    });
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("feid.sock");
    let listener = tokio::net::UnixListener::bind(&socket).unwrap();
    let serving = signer.clone();
    tokio::spawn(async move {
        loop {
            let Ok((stream, _)) = listener.accept().await else {
                return;
            };
            let signer = serving.clone();
            tokio::spawn(async move {
                let options = feid_proto::ServeOptions::named("test-signer");
                feid_proto::serve(stream, signer.as_ref(), &options).await;
            });
        }
    });
    Workload {
        _dir: dir,
        socket,
        signer,
    }
}

async fn blocking<T: Send + 'static>(f: impl FnOnce() -> T + Send + 'static) -> T {
    tokio::task::spawn_blocking(f).await.unwrap()
}

async fn feid(w: &Workload) -> Feid {
    let socket = w.socket.clone();
    blocking(move || Feid::with_socket(socket).unwrap()).await
}

/// A handshake from `client` to `server` by `name`, and a line echoed.
async fn handshake(
    client: &Feid,
    server: &Feid,
    name: &str,
) -> Result<(Option<SpiffeId>, Option<SpiffeId>), std::io::Error> {
    let (c_io, s_io) = tokio::io::duplex(1 << 16);
    let (cc, sc) = {
        let (client, server) = (client.clone(), server.clone());
        blocking(move || {
            (
                client.client_config().unwrap(),
                server.server_config().unwrap(),
            )
        })
        .await
    };
    let server = server.clone();
    let accepted = tokio::spawn(async move {
        let mut tls = server.accept(sc, s_io).await?;
        let caller = peer_id(tls.get_ref().1);
        let mut buf = [0u8; 5];
        tls.read_exact(&mut buf).await?;
        tls.write_all(&buf).await?;
        tls.flush().await?;
        Ok::<_, std::io::Error>(caller)
    });
    let name = ServerName::try_from(name.to_string()).unwrap();
    let connected = async {
        let mut tls = client.connect(cc, name, c_io).await?;
        let callee = peer_id(tls.get_ref().1);
        tls.write_all(b"hello").await?;
        tls.flush().await?;
        let mut buf = [0u8; 5];
        tls.read_exact(&mut buf).await?;
        assert_eq!(&buf, b"hello");
        Ok::<_, std::io::Error>(callee)
    };
    let (connected, accepted) = tokio::join!(connected, accepted);
    let callee = connected?;
    let caller = accepted.unwrap()?;
    Ok((caller, callee))
}

async fn eventually(what: &str, f: impl Fn() -> bool) {
    for _ in 0..200 {
        if f() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    panic!("{what}");
}

#[tokio::test(flavor = "multi_thread")]
async fn two_workloads_handshake_with_keys_they_never_hold() {
    let ca = Ca::new("platform");
    let web = workload(&ca, WEB, "web.acme.internal", &ca.pem);
    let api = workload(&ca, API, "api.acme.internal", &ca.pem);
    let (client, server) = (feid(&web).await, feid(&api).await);
    let (caller, callee) = handshake(&client, &server, "api.acme.internal")
        .await
        .unwrap();
    assert_eq!(caller.unwrap().as_str(), WEB);
    assert_eq!(callee.unwrap().as_str(), API);
    // Each side's CertificateVerify was signed by its own signer.
    assert_eq!(*web.signer.signed.lock().unwrap(), vec![Role::Client]);
    assert_eq!(*api.signer.signed.lock().unwrap(), vec![Role::Server]);
    let own = blocking(move || client.id().unwrap()).await;
    assert_eq!(own.as_str(), WEB);
    // Both handshakes are reported, each to its own signer.
    eventually("the client's report", || {
        !web.signer.reports.lock().unwrap().is_empty()
    })
    .await;
    eventually("the server's report", || {
        !api.signer.reports.lock().unwrap().is_empty()
    })
    .await;
    let out = web.signer.reports.lock().unwrap()[0].clone();
    assert_eq!(out.direction, "outbound");
    assert_eq!(out.sni, "api.acme.internal");
    assert_eq!(out.peer_spiffe, API);
    assert_eq!(out.version, "TLS 1.3");
    assert_eq!(out.peer_fingerprint.len(), 64);
    assert!(out.peer_not_after.ends_with('Z'));
    let inbound = api.signer.reports.lock().unwrap()[0].clone();
    assert_eq!(inbound.direction, "inbound");
    assert_eq!(inbound.peer_spiffe, WEB);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_server_is_checked_by_its_name() {
    let ca = Ca::new("platform");
    let web = workload(&ca, WEB, "web.acme.internal", &ca.pem);
    let api = workload(&ca, API, "api.acme.internal", &ca.pem);
    let (client, server) = (feid(&web).await, feid(&api).await);
    let e = handshake(&client, &server, "db.acme.internal")
        .await
        .unwrap_err();
    assert!(e.to_string().contains("certificate"), "{e}");
}

/// A handshake where the client checks the server by its ID.
async fn pinned(
    client: &Feid,
    server: &Feid,
    peer: &str,
) -> Result<Option<SpiffeId>, std::io::Error> {
    let (c_io, s_io) = tokio::io::duplex(1 << 16);
    let peer = SpiffeId::parse(peer).unwrap();
    let (cc, sc) = {
        let (client, server) = (client.clone(), server.clone());
        blocking(move || {
            (
                client.client_config_for(&peer).unwrap(),
                server.server_config().unwrap(),
            )
        })
        .await
    };
    let server = server.clone();
    let accepted = tokio::spawn(async move { server.accept(sc, s_io).await.map(|_| ()) });
    let name = ServerName::try_from("anything.invalid").unwrap();
    let connected = client.connect(cc, name, c_io).await;
    let _ = accepted.await;
    connected.map(|tls| peer_id(tls.get_ref().1))
}

#[tokio::test(flavor = "multi_thread")]
async fn a_server_without_a_name_is_checked_by_its_id() {
    let ca = Ca::new("platform");
    let web = workload(&ca, WEB, "web.acme.internal", &ca.pem);
    let api = workload(&ca, API, "api.acme.internal", &ca.pem);
    let (client, server) = (feid(&web).await, feid(&api).await);
    let callee = pinned(&client, &server, API).await.unwrap();
    assert_eq!(callee.unwrap().as_str(), API);
    // Another ID than the server's is refused, whatever its name.
    let e = pinned(&client, &server, WEB).await.unwrap_err();
    assert!(e.to_string().contains("not spiffe"), "{e}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_client_of_another_ca_is_refused() {
    let ca = Ca::new("platform");
    let other = Ca::new("somebody else");
    let web = workload(&other, WEB, "web.acme.internal", &ca.pem);
    let api = workload(&ca, API, "api.acme.internal", &ca.pem);
    let (client, server) = (feid(&web).await, feid(&api).await);
    assert!(handshake(&client, &server, "api.acme.internal")
        .await
        .is_err());
}

#[tokio::test(flavor = "multi_thread")]
async fn a_retriable_refusal_is_asked_again_and_a_final_one_is_not() {
    let ca = Ca::new("platform");
    let web = workload(&ca, WEB, "web.acme.internal", &ca.pem);
    let api = workload(&ca, API, "api.acme.internal", &ca.pem);
    let (client, server) = (feid(&web).await, feid(&api).await);
    web.signer.refusals.lock().unwrap().push_back(Refusal::new(
        code::UNAVAILABLE_AUDIT_LOG,
        "the audit file is full",
        true,
    ));
    handshake(&client, &server, "api.acme.internal")
        .await
        .unwrap();
    assert_eq!(web.signer.attempts.load(Ordering::Relaxed), 2);

    web.signer.refusals.lock().unwrap().push_back(Refusal::new(
        code::REFUSED_NOT_CERTIFICATE_VERIFY,
        "no",
        false,
    ));
    assert!(handshake(&client, &server, "api.acme.internal")
        .await
        .is_err());
    assert_eq!(web.signer.attempts.load(Ordering::Relaxed), 3);
}

/// What a resumed pod's signer does: a new key, and a chain for it.
#[tokio::test(flavor = "multi_thread")]
async fn a_new_key_and_chain_are_used_in_the_next_handshake() {
    let ca = Ca::new("platform");
    let api = workload(&ca, API, "api.acme.internal", &ca.pem);
    let (key, chain) = ca.workload(WEB, "web.acme.internal");
    let web = Arc::new(Signer {
        key,
        chain,
        bundle: ca.pem.clone(),
        signed: Mutex::default(),
        attempts: AtomicU32::new(0),
        refusals: Mutex::default(),
        reports: Mutex::default(),
    });
    // A signer whose key and chain can be swapped.
    let current = Arc::new(Mutex::new(web.clone()));
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("feid.sock");
    let listener = tokio::net::UnixListener::bind(&socket).unwrap();
    let serving = current.clone();
    tokio::spawn(async move {
        while let Ok((stream, _)) = listener.accept().await {
            let signer = serving.lock().unwrap().clone();
            tokio::spawn(async move {
                let options = feid_proto::ServeOptions::named("test-signer");
                feid_proto::serve(stream, signer.as_ref(), &options).await;
            });
        }
    });
    let client = blocking(move || Feid::with_socket(socket).unwrap()).await;
    let server = feid(&api).await;
    handshake(&client, &server, "api.acme.internal")
        .await
        .unwrap();

    let (key, chain) = ca.workload(WEB, "web.acme.internal");
    *current.lock().unwrap() = Arc::new(Signer {
        key,
        chain,
        bundle: ca.pem.clone(),
        signed: Mutex::default(),
        attempts: AtomicU32::new(0),
        refusals: Mutex::default(),
        reports: Mutex::default(),
    });
    // The old signer's connections end, as a resumed pod's do.
    client.inner.idle.lock().unwrap().clear();
    handshake(&client, &server, "api.acme.internal")
        .await
        .unwrap();
    assert_eq!(current.lock().unwrap().signed.lock().unwrap().len(), 1);
}

#[tokio::test(flavor = "multi_thread")]
async fn without_a_signer_there_is_no_identity() {
    let dir = tempfile::tempdir().unwrap();
    let missing = dir.path().join("feid.sock");
    let e = blocking(move || Feid::with_socket(missing).err().unwrap()).await;
    assert!(matches!(e, Error::NoIdentity { .. }), "{e}");
    assert!(e.to_string().contains("no identity"), "{e}");
}

#[test]
fn spiffe_ids() {
    let id = SpiffeId::parse(API).unwrap();
    assert_eq!(id.trust_domain(), "run.example");
    assert_eq!(id.path(), "/tenant/acme/env/prod/service/api");
    assert_eq!(SpiffeId::parse("spiffe://run.example").unwrap().path(), "");
    for bad in [
        "https://run.example/x",
        "spiffe://",
        "spiffe://Run.Example/x",
        "spiffe://run.example/",
        "spiffe://run.example//x",
        "spiffe://run.example/a/../b",
        "spiffe://run.example/a?b",
    ] {
        assert!(SpiffeId::parse(bad).is_none(), "{bad}");
    }
}

#[test]
fn times_are_rfc3339() {
    assert_eq!(tls::rfc3339(0), "1970-01-01T00:00:00Z");
    assert_eq!(tls::rfc3339(951_782_400), "2000-02-29T00:00:00Z");
    assert_eq!(tls::rfc3339(1_786_377_600), "2026-08-10T16:00:00Z");
}
