// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The host's side of a pod's outbound TLS binding, for the guest's tests
//! (hack/guest/test-container), with a CA made for the run:
//!
//! - FeID's signer on the Unix socket that the microVM's vsock port 10002
//!   reaches, after the preamble `feid\n`, as feos-authn serves it: the
//!   pod's certificate for POD_ID, the CA as the trust bundle, and its
//!   CertificateVerify signed;
//! - an upstream at ADDRESS, as PostgreSQL 17 with direct TLS: TLS 1.3,
//!   ALPN `postgresql`, a certificate for SERVER_ID and a client's
//!   required under the CA. It answers a client's first four bytes with
//!   `pod:` and those bytes when the client presented POD_ID, with `?:`
//!   otherwise, and closes.
//!
//!     tls-binding-peer <feid socket> <address> <pod ID> <server ID>
//!
//! Prints `ready` once both listen, and a line per connection.

use std::error::Error;
use std::sync::Arc;

use rcgen::SigningKey;
use rustls::pki_types::pem::PemObject;
use rustls::pki_types::{CertificateDer, PrivateKeyDer};
use tokio::io::{AsyncReadExt, AsyncWriteExt};

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
        println!("signer: a CertificateVerify");
        self.key
            .sign(cv.message)
            .map_err(|e| feid_proto::Refusal::internal(e.to_string()))
    }
    fn handshake_report(&self, _: feid_proto::HandshakeReport) {}
}

/// A workload's key and certificate under `issuer`: its SPIFFE ID, no DNS
/// name.
fn workload(
    issuer: &rcgen::Issuer<'_, rcgen::KeyPair>,
    spiffe: &str,
) -> Result<(rcgen::KeyPair, String), Box<dyn Error>> {
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519)?;
    let mut p = rcgen::CertificateParams::new(Vec::<String>::new())?;
    p.subject_alt_names
        .push(rcgen::SanType::URI(spiffe.try_into()?));
    p.distinguished_name
        .push(rcgen::DnType::CommonName, "feos.test/pod");
    p.extended_key_usages = vec![
        rcgen::ExtendedKeyUsagePurpose::ClientAuth,
        rcgen::ExtendedKeyUsagePurpose::ServerAuth,
    ];
    let now = time::OffsetDateTime::now_utc();
    p.not_before = now - time::Duration::minutes(5);
    p.not_after = now + time::Duration::hours(2);
    let pem = p.signed_by(&key, issuer)?.pem();
    Ok((key, pem))
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let [socket, address, pod, server] = &args[..] else {
        return Err("usage: tls-binding-peer <feid socket> <address> <pod ID> <server ID>".into());
    };

    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519)?;
    let mut ca = rcgen::CertificateParams::new(Vec::<String>::new())?;
    ca.distinguished_name
        .push(rcgen::DnType::CommonName, "tls-binding-peer");
    ca.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    ca.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
    let ca_pem = ca.self_signed(&ca_key)?.pem();
    let issuer = rcgen::Issuer::new(ca, ca_key);

    let (key, chain) = workload(&issuer, pod)?;
    let signer = Arc::new(Signer {
        key,
        chain,
        bundle: ca_pem.clone(),
    });
    let _ = std::fs::remove_file(socket);
    let feid = tokio::net::UnixListener::bind(socket)?;

    let (key, pem) = workload(&issuer, server)?;
    let chain = vec![CertificateDer::from_pem_slice(pem.as_bytes())?];
    let key = PrivateKeyDer::from_pem_slice(key.serialize_pem().as_bytes())?;
    let mut roots = rustls::RootCertStore::empty();
    roots.add(CertificateDer::from_pem_slice(ca_pem.as_bytes())?)?;
    let verifier = rustls::server::WebPkiClientVerifier::builder(Arc::new(roots)).build()?;
    let mut config =
        rustls::ServerConfig::builder_with_protocol_versions(&[&rustls::version::TLS13])
            .with_client_cert_verifier(verifier)
            .with_single_cert(chain, key)?;
    config.alpn_protocols = vec![b"postgresql".to_vec()];
    let acceptor = tokio_rustls::TlsAcceptor::from(Arc::new(config));
    let upstream = tokio::net::TcpListener::bind(address.as_str()).await?;
    println!("ready");

    tokio::spawn(async move {
        while let Ok((mut stream, _)) = feid.accept().await {
            let signer = signer.clone();
            tokio::spawn(async move {
                let mut preamble = [0u8; 5];
                if stream.read_exact(&mut preamble).await.is_err() || &preamble != b"feid\n" {
                    println!("signer: no preamble");
                    return;
                }
                let options = feid_proto::ServeOptions::named("tls-binding-peer");
                feid_proto::serve(stream, signer.as_ref(), &options).await;
            });
        }
    });

    let pod = pod.clone().into_bytes();
    while let Ok((tcp, peer)) = upstream.accept().await {
        let (acceptor, pod) = (acceptor.clone(), pod.clone());
        tokio::spawn(async move {
            let mut tls = match acceptor.accept(tcp).await {
                Ok(tls) => tls,
                Err(e) => return println!("upstream: {peer}: handshake: {e}"),
            };
            let conn = tls.get_ref().1;
            let alpn = conn
                .alpn_protocol()
                .map(|a| String::from_utf8_lossy(a).into_owned());
            let is_pod = conn
                .peer_certificates()
                .and_then(|c| c.first())
                .is_some_and(|c| c.windows(pod.len()).any(|w| w == pod));
            println!("upstream: {peer}: ALPN {alpn:?}, the pod's certificate: {is_pod}");
            let mut buf = [0u8; 4];
            if tls.read_exact(&mut buf).await.is_ok() {
                let who: &[u8] = if is_pod { b"pod:" } else { b"?:" };
                let _ = tls.write_all(&[who, &buf].concat()).await;
            }
            let _ = tls.shutdown().await;
        });
    }
    Ok(())
}
