// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! TLS to the control plane's port for nodes: TLS 1.3 only, with ring, and
//! without session resumption, so that every connection presents the
//! node's current certificate.
//!
//! The control plane is checked for the name feosd was configured with,
//! even when it dials an address, and for the one URI name
//! `spiffe://<trust domain>/ferun`: one issuer signs pods and the control
//! plane alike, and that URI is what tells them apart. A join checks the
//! chain against the root that the join token pins; later connections
//! check it against the trust bundle.
//!
//! A root is trusted for its key, without its name constraints
//! ([`anchors`], ADR 0024): rustls-webpki refuses every URI name under a
//! CA's URI constraint, and the platform's roots carry one. The issuer's
//! own name constraints still hold, and the control plane's certificate
//! is held to its exact names.

use crate::identity::Identity;
use crate::token;
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::client::{Resumption, WebPkiServerVerifier};
use rustls::crypto::{verify_tls12_signature, verify_tls13_signature, CryptoProvider};
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{
    CertificateError, ClientConfig, DigitallySignedStruct, Error, OtherError, RootCertStore,
    SignatureScheme,
};
use std::io;
use std::os::fd::AsFd;
use std::sync::Arc;
use std::time::Duration;
use tokio::net::TcpStream;
use tokio::time::timeout;
use tokio_rustls::client::TlsStream;
use tokio_rustls::TlsConnector;

/// The uplink's and the join's ALPN protocol.
pub const ALPN_UPLINK: &[u8] = b"h2";
/// The node connection's: on it, the node is the HTTP/2 server.
pub const ALPN_NODE: &[u8] = b"feos-node/1";

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);
/// Unacknowledged data fails the connection after this, rather than after
/// the kernel's retransmissions run out, a quarter of an hour.
const TCP_USER_TIMEOUT_MS: u32 = 20_000;
const TCP_KEEPALIVE_IDLE_S: u32 = 15;
const TCP_KEEPALIVE_INTERVAL_S: u32 = 5;
const TCP_KEEPALIVE_COUNT: u32 = 3;

/// ring, as the rest of feosd has it.
pub fn provider() -> Arc<CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

/// The join's configuration: the server is checked against the pinned
/// root, and the node shows no certificate.
pub fn join_config(
    provider: &Arc<CryptoProvider>,
    pin: &str,
    trust_domain: &str,
) -> Result<Arc<ClientConfig>, Error> {
    let verifier = Arc::new(JoinVerifier {
        pin: pin.to_string(),
        trust_domain: trust_domain.to_string(),
        provider: provider.clone(),
    });
    let config = builder(provider)?
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_no_client_auth();
    Ok(finish(config, ALPN_UPLINK))
}

/// The configuration of the uplink (`h2`) or of a node connection
/// (`feos-node/1`): the node's certificate, and the server checked against
/// the trust bundle.
pub fn node_config(
    provider: &Arc<CryptoProvider>,
    identity: &Identity,
    trust_domain: &str,
    alpn: &[u8],
) -> Result<Arc<ClientConfig>, Error> {
    let verifier = Arc::new(ControlPlaneVerifier::new(
        provider,
        &identity.bundle,
        trust_domain,
    )?);
    let config = builder(provider)?
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_client_auth_cert(identity.chain.clone(), identity.private_key())?;
    Ok(finish(config, alpn))
}

/// The configuration of a connection to the sink (ADR 0026): the node's
/// certificate, and the sink's checked against the trust bundle and for
/// its one URI, `spiffe://<trust domain>/sink/<name>`, whatever the name it
/// is dialed by.
pub fn sink_config(
    provider: &Arc<CryptoProvider>,
    identity: &Identity,
    trust_domain: &str,
) -> Result<Arc<ClientConfig>, Error> {
    let verifier = Arc::new(SinkVerifier::new(provider, &identity.bundle, trust_domain)?);
    let config = builder(provider)?
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_client_auth_cert(identity.chain.clone(), identity.private_key())?;
    Ok(finish(config, ALPN_UPLINK))
}

/// The trust anchors of `roots`: each root's subject and key, without its
/// name constraints. rustls-webpki checks no URI name constraint and
/// refuses every URI name beneath one (fail closed), and the roots that
/// `ferund init-ca` made before ADR 0024 constrain URIs to the trust
/// domain; a chain from such a root to the control plane's certificate,
/// which carries a URI name, would never verify. The constraints of the
/// certificates between the root and the leaf, the issuer's, still hold,
/// and [`check_control_plane_leaf`] holds the leaf to the exact URI and to
/// no IP address, more than the root's constraints asked.
fn anchors<'a, 'b: 'a>(
    roots: impl IntoIterator<Item = &'a CertificateDer<'b>>,
) -> Result<RootCertStore, Error> {
    let mut store = RootCertStore::empty();
    for root in roots {
        store.add(root.clone().into_owned())?;
    }
    for anchor in &mut store.roots {
        anchor.name_constraints = None;
    }
    Ok(store)
}

fn builder(
    provider: &Arc<CryptoProvider>,
) -> Result<rustls::ConfigBuilder<ClientConfig, rustls::WantsVerifier>, Error> {
    ClientConfig::builder_with_provider(provider.clone())
        .with_protocol_versions(&[&rustls::version::TLS13])
}

fn finish(mut config: ClientConfig, alpn: &[u8]) -> Arc<ClientConfig> {
    config.alpn_protocols = vec![alpn.to_vec()];
    config.resumption = Resumption::disabled();
    Arc::new(config)
}

/// Dials `address` and shakes hands for `name`, the control plane's name.
/// The server must choose the configuration's ALPN protocol.
pub async fn connect(
    address: &str,
    name: &ServerName<'static>,
    config: Arc<ClientConfig>,
) -> io::Result<TlsStream<TcpStream>> {
    let tcp = timeout(CONNECT_TIMEOUT, TcpStream::connect(address))
        .await
        .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "connecting timed out"))??;
    tune(&tcp)?;
    let alpn = config.alpn_protocols.first().cloned().unwrap_or_default();
    let tls = timeout(
        HANDSHAKE_TIMEOUT,
        TlsConnector::from(config).connect(name.clone(), tcp),
    )
    .await
    .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "the TLS handshake timed out"))??;
    let chosen = tls.get_ref().1.alpn_protocol();
    if chosen != Some(alpn.as_slice()) {
        return Err(io::Error::other(format!(
            "the control plane chose ALPN {:?}, not {:?}",
            chosen.map(String::from_utf8_lossy),
            String::from_utf8_lossy(&alpn)
        )));
    }
    Ok(tls)
}

/// No Nagle; keepalive, and a timeout for unacknowledged data, so that a
/// dead peer is noticed in seconds.
fn tune(tcp: &TcpStream) -> io::Result<()> {
    use nix::sys::socket::{setsockopt, sockopt};
    tcp.set_nodelay(true)?;
    let fd = tcp.as_fd();
    setsockopt(&fd, sockopt::KeepAlive, &true)?;
    setsockopt(&fd, sockopt::TcpKeepIdle, &TCP_KEEPALIVE_IDLE_S)?;
    setsockopt(&fd, sockopt::TcpKeepInterval, &TCP_KEEPALIVE_INTERVAL_S)?;
    setsockopt(&fd, sockopt::TcpKeepCount, &TCP_KEEPALIVE_COUNT)?;
    setsockopt(&fd, sockopt::TcpUserTimeout, &TCP_USER_TIMEOUT_MS)?;
    Ok(())
}

/// Why the control plane's certificate is refused, as rustls says it.
struct Refusal(String);

impl std::fmt::Debug for Refusal {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::fmt::Display for Refusal {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for Refusal {}

fn refused(message: impl Into<String>) -> Error {
    Error::InvalidCertificate(CertificateError::Other(OtherError(Arc::new(Refusal(
        message.into(),
    )))))
}

/// Checks the control plane's own certificate beyond its chain and name: it
/// carries no IP address, and exactly one URI name, the control plane's
/// SPIFFE ID.
fn check_control_plane_leaf(leaf: &[u8], trust_domain: &str) -> Result<(), Error> {
    use x509_parser::extensions::GeneralName;
    let (_, cert) = x509_parser::parse_x509_certificate(leaf)
        .map_err(|e| refused(format!("the control plane's certificate: {e}")))?;
    let names = cert
        .subject_alternative_name()
        .map_err(|e| refused(format!("the control plane's certificate's names: {e}")))?;
    let names = names.map(|names| names.value.general_names.as_slice());
    let mut uris = Vec::new();
    for name in names.unwrap_or_default() {
        match name {
            GeneralName::URI(uri) => uris.push(*uri),
            GeneralName::IPAddress(_) => {
                return Err(refused(
                    "the control plane's certificate carries an IP address",
                ))
            }
            _ => {}
        }
    }
    let want = token::control_plane_id(trust_domain);
    if uris != [want.as_str()] {
        return Err(refused(format!(
            "the server's certificate names {uris:?}, not the control plane {want}"
        )));
    }
    Ok(())
}

/// The join's check: the certificate of the chain after the server's own
/// whose public key the token pins, a CA, is the only trust anchor. A
/// server whose chain holds none is refused before the token is sent.
#[derive(Debug)]
struct JoinVerifier {
    pin: String,
    trust_domain: String,
    provider: Arc<CryptoProvider>,
}

impl ServerCertVerifier for JoinVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        server_name: &ServerName<'_>,
        ocsp_response: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, Error> {
        let mut pinned = Vec::new();
        for cert in intermediates {
            if token::pin(cert).map_err(refused)? != self.pin {
                continue;
            }
            if !is_ca(cert)? {
                return Err(refused(
                    "the certificate matching the join token's pin is not a CA",
                ));
            }
            pinned.push(cert);
        }
        let roots = anchors(pinned)?;
        if roots.is_empty() {
            return Err(refused(
                "the control plane's chain holds no root matching the join token's pin",
            ));
        }
        let verifier =
            WebPkiServerVerifier::builder_with_provider(Arc::new(roots), self.provider.clone())
                .build()
                .map_err(|e| Error::General(format!("the pinned root: {e}")))?;
        verifier.verify_server_cert(end_entity, intermediates, server_name, ocsp_response, now)?;
        check_control_plane_leaf(end_entity, &self.trust_domain)?;
        Ok(ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        verify_tls12_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        verify_tls13_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider
            .signature_verification_algorithms
            .supported_schemes()
    }
}

/// Whether a certificate is a CA's: basic constraints with CA:true.
fn is_ca(cert: &[u8]) -> Result<bool, Error> {
    let (_, cert) = x509_parser::parse_x509_certificate(cert)
        .map_err(|e| refused(format!("a certificate of the control plane's chain: {e}")))?;
    let constraints = cert
        .basic_constraints()
        .map_err(|e| refused(format!("a certificate's basic constraints: {e}")))?;
    Ok(constraints.is_some_and(|constraints| constraints.value.ca))
}

/// The check after the join: the chain to the trust bundle, and the
/// control plane's own certificate.
#[derive(Debug)]
struct ControlPlaneVerifier {
    inner: Arc<WebPkiServerVerifier>,
    trust_domain: String,
}

impl ControlPlaneVerifier {
    /// The check against the roots of `bundle`, as [`anchors`] trusts them.
    fn new(
        provider: &Arc<CryptoProvider>,
        bundle: &[CertificateDer<'_>],
        trust_domain: &str,
    ) -> Result<Self, Error> {
        Ok(Self {
            inner: WebPkiServerVerifier::builder_with_provider(
                Arc::new(anchors(bundle)?),
                provider.clone(),
            )
            .build()
            .map_err(|e| Error::General(format!("the trust bundle: {e}")))?,
            trust_domain: trust_domain.to_string(),
        })
    }
}

impl ServerCertVerifier for ControlPlaneVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        server_name: &ServerName<'_>,
        ocsp_response: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, Error> {
        self.inner.verify_server_cert(
            end_entity,
            intermediates,
            server_name,
            ocsp_response,
            now,
        )?;
        check_control_plane_leaf(end_entity, &self.trust_domain)?;
        Ok(ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        self.inner.verify_tls12_signature(message, cert, dss)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        self.inner.verify_tls13_signature(message, cert, dss)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.inner.supported_verify_schemes()
    }
}

/// Checks a sink's own certificate beyond its chain: no IP address, and
/// exactly one URI name, a sink's SPIFFE ID in the trust domain.
fn check_sink_leaf(leaf: &[u8], trust_domain: &str) -> Result<(), Error> {
    use x509_parser::extensions::GeneralName;
    let (_, cert) = x509_parser::parse_x509_certificate(leaf)
        .map_err(|e| refused(format!("the sink's certificate: {e}")))?;
    let names = cert
        .subject_alternative_name()
        .map_err(|e| refused(format!("the sink's certificate's names: {e}")))?;
    let names = names.map(|names| names.value.general_names.as_slice());
    let mut uris = Vec::new();
    for name in names.unwrap_or_default() {
        match name {
            GeneralName::URI(uri) => uris.push(*uri),
            GeneralName::IPAddress(_) => {
                return Err(refused("the sink's certificate carries an IP address"))
            }
            _ => {}
        }
    }
    let prefix = token::sink_id_prefix(trust_domain);
    let named = match uris.as_slice() {
        [uri] => uri.strip_prefix(&prefix).is_some_and(token::is_dns_label),
        _ => false,
    };
    if !named {
        return Err(refused(format!(
            "the server's certificate names {uris:?}, not a sink {prefix}<name>"
        )));
    }
    Ok(())
}

/// The check of a sink: the chain to the trust bundle, for a server, and
/// the sink's own certificate; not the name it was dialed by.
#[derive(Debug)]
struct SinkVerifier {
    roots: RootCertStore,
    trust_domain: String,
    provider: Arc<CryptoProvider>,
}

impl SinkVerifier {
    fn new(
        provider: &Arc<CryptoProvider>,
        bundle: &[CertificateDer<'_>],
        trust_domain: &str,
    ) -> Result<Self, Error> {
        let roots = anchors(bundle)?;
        if roots.is_empty() {
            return Err(Error::General("the trust bundle is empty".into()));
        }
        Ok(Self {
            roots,
            trust_domain: trust_domain.to_string(),
            provider: provider.clone(),
        })
    }
}

impl ServerCertVerifier for SinkVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, Error> {
        let cert = rustls::server::ParsedCertificate::try_from(end_entity)?;
        rustls::client::verify_server_cert_signed_by_trust_anchor(
            &cert,
            &self.roots,
            intermediates,
            now,
            self.provider.signature_verification_algorithms.all,
        )?;
        check_sink_leaf(end_entity, &self.trust_domain)?;
        Ok(ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        verify_tls12_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        verify_tls13_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider
            .signature_verification_algorithms
            .supported_schemes()
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use rcgen::{
        BasicConstraints, CertificateParams, DnType, ExtendedKeyUsagePurpose, IsCa, Issuer,
        KeyPair, SanType,
    };

    /// A CA of the test PKI.
    pub struct Ca {
        pub cert: rcgen::Certificate,
        pub issuer: Issuer<'static, KeyPair>,
    }

    impl Ca {
        pub fn root(name: &str) -> Self {
            let key = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
            let params = ca_params(name);
            let cert = params.self_signed(&key).unwrap();
            Self {
                cert,
                issuer: Issuer::new(params, key),
            }
        }

        pub fn intermediate(&self, name: &str) -> Self {
            let key = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
            let params = ca_params(name);
            let cert = params.signed_by(&key, &self.issuer).unwrap();
            Self {
                cert,
                issuer: Issuer::new(params, key),
            }
        }

        /// A leaf with DNS names `dns`, URI names `uris` and the extended
        /// key usage `usage`.
        pub fn leaf(
            &self,
            dns: &[&str],
            uris: &[&str],
            usage: ExtendedKeyUsagePurpose,
            key: &KeyPair,
        ) -> rcgen::Certificate {
            let mut params =
                CertificateParams::new(dns.iter().map(|s| s.to_string()).collect::<Vec<_>>())
                    .unwrap();
            for uri in uris {
                params
                    .subject_alt_names
                    .push(SanType::URI(uri.to_string().try_into().unwrap()));
            }
            params.extended_key_usages = vec![usage];
            params.signed_by(key, &self.issuer).unwrap()
        }
    }

    fn ca_params(name: &str) -> CertificateParams {
        let mut params = CertificateParams::new(Vec::<String>::new()).unwrap();
        params.distinguished_name.push(DnType::CommonName, name);
        params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        params
    }

    impl Ca {
        /// A root with the name constraints `ferund init-ca` gives: DNS
        /// names under `dns`, URIs in `uris`, no IP address.
        fn constrained_root(name: &str, dns: &[&str], uris: &[&str]) -> Self {
            let key = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
            let mut params = ca_params(name);
            params.custom_extensions.push(name_constraints(dns, uris));
            let cert = params.self_signed(&key).unwrap();
            Self {
                cert,
                issuer: Issuer::new(params, key),
            }
        }

        /// An intermediate with those name constraints.
        fn constrained_intermediate(&self, name: &str, dns: &[&str], uris: &[&str]) -> Self {
            let key = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
            let mut params = ca_params(name);
            params.custom_extensions.push(name_constraints(dns, uris));
            let cert = params.signed_by(&key, &self.issuer).unwrap();
            Self {
                cert,
                issuer: Issuer::new(params, key),
            }
        }
    }

    /// A critical name constraints extension (RFC 5280 4.2.1.10) that
    /// permits the DNS domains `dns` and the URI hosts `uris` and excludes
    /// every IPv4 and IPv6 address, written by hand: rcgen has no URI
    /// subtree.
    fn name_constraints(dns: &[&str], uris: &[&str]) -> rcgen::CustomExtension {
        fn tlv(tag: u8, content: &[u8]) -> Vec<u8> {
            let mut out = vec![tag];
            let len = content.len();
            if len < 0x80 {
                out.push(len as u8);
            } else {
                out.push(0x82);
                out.extend((len as u16).to_be_bytes());
            }
            out.extend_from_slice(content);
            out
        }
        // GeneralSubtree: SEQUENCE { base GeneralName }; dNSName is [2],
        // uniformResourceIdentifier [6], iPAddress [7], all implicit.
        let mut permitted = Vec::new();
        for d in dns {
            permitted.extend(tlv(0x30, &tlv(0x82, d.as_bytes())));
        }
        for u in uris {
            permitted.extend(tlv(0x30, &tlv(0x86, u.as_bytes())));
        }
        let mut excluded = tlv(0x30, &tlv(0x87, &[0; 8]));
        excluded.extend(tlv(0x30, &tlv(0x87, &[0; 32])));
        let mut subtrees = tlv(0xa0, &permitted);
        subtrees.extend(tlv(0xa1, &excluded));
        let mut ext =
            rcgen::CustomExtension::from_oid_content(&[2, 5, 29, 30], tlv(0x30, &subtrees));
        ext.set_criticality(true);
        ext
    }

    pub fn key() -> KeyPair {
        KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap()
    }

    fn verify(
        verifier: &dyn ServerCertVerifier,
        chain: &[&rcgen::Certificate],
        name: &str,
    ) -> Result<(), String> {
        let intermediates: Vec<CertificateDer<'_>> =
            chain[1..].iter().map(|cert| cert.der().clone()).collect();
        verifier
            .verify_server_cert(
                chain[0].der(),
                &intermediates,
                &ServerName::try_from(name.to_string()).unwrap(),
                &[],
                UnixTime::now(),
            )
            .map(|_| ())
            .map_err(|e| e.to_string())
    }

    /// Fails, and says `why`.
    fn refused(result: Result<(), String>, why: &str) {
        match result {
            Ok(()) => panic!("accepted; expected a refusal for {why:?}"),
            Err(e) => assert!(e.contains(why), "{e:?} does not say {why:?}"),
        }
    }

    fn join_verifier(pinned: &rcgen::Certificate) -> JoinVerifier {
        JoinVerifier {
            pin: token::pin(pinned.der()).unwrap(),
            trust_domain: "feos.local".into(),
            provider: provider(),
        }
    }

    #[test]
    fn a_join_accepts_a_chain_that_ends_in_the_pinned_root() {
        let root = Ca::root("root");
        let issuer = root.intermediate("issuer");
        let key = key();
        let ferun = "spiffe://feos.local/ferun";
        fn chain<'a>(
            leaf: &'a rcgen::Certificate,
            issuer: &'a Ca,
            root: &'a Ca,
        ) -> [&'a rcgen::Certificate; 3] {
            [leaf, &issuer.cert, &root.cert]
        }
        let leaf = issuer.leaf(
            &["ferun.feos.test"],
            &[ferun],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        let verifier = join_verifier(&root.cert);
        verify(&verifier, &chain(&leaf, &issuer, &root), "ferun.feos.test").unwrap();

        // Another root's chain.
        let other = Ca::root("other");
        let other_issuer = other.intermediate("issuer");
        let other_leaf = other_issuer.leaf(
            &["ferun.feos.test"],
            &[ferun],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        let no_root = "holds no root matching the join token's pin";
        refused(
            verify(
                &verifier,
                &[&other_leaf, &other_issuer.cert, &other.cert],
                "ferun.feos.test",
            ),
            no_root,
        );
        // The pinned root sent, but not as the anchor of this chain.
        refused(
            verify(
                &verifier,
                &[&other_leaf, &other_issuer.cert, &root.cert],
                "ferun.feos.test",
            ),
            "UnknownIssuer",
        );
        // Without the root, nothing is pinned.
        refused(
            verify(&verifier, &[&leaf, &issuer.cert], "ferun.feos.test"),
            no_root,
        );
        // A leaf for another name.
        refused(
            verify(&verifier, &chain(&leaf, &issuer, &root), "evil.feos.test"),
            "not valid for name",
        );
        // A pod's certificate of the same CA, without the control plane's
        // URI; two URIs; the control plane of another trust domain.
        for uris in [
            &["spiffe://feos.local/project/t/sandbox/s"][..],
            &[ferun, "spiffe://feos.local/node/n1"],
            &["spiffe://other.local/ferun"],
            &[],
        ] {
            let cert = issuer.leaf(
                &["ferun.feos.test"],
                uris,
                ExtendedKeyUsagePurpose::ServerAuth,
                &key,
            );
            refused(
                verify(&verifier, &chain(&cert, &issuer, &root), "ferun.feos.test"),
                "not the control plane spiffe://feos.local/ferun",
            );
        }
        // An IP address in the control plane's certificate.
        let mut params = CertificateParams::new(vec!["ferun.feos.test".to_string()]).unwrap();
        params
            .subject_alt_names
            .push(SanType::URI(ferun.try_into().unwrap()));
        params
            .subject_alt_names
            .push(SanType::IpAddress("10.0.0.1".parse().unwrap()));
        params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        let with_ip = params.signed_by(&key, &issuer.issuer).unwrap();
        refused(
            verify(
                &verifier,
                &chain(&with_ip, &issuer, &root),
                "ferun.feos.test",
            ),
            "carries an IP address",
        );
        // A client certificate is no server's.
        let client = issuer.leaf(
            &["ferun.feos.test"],
            &[ferun],
            ExtendedKeyUsagePurpose::ClientAuth,
            &key,
        );
        refused(
            verify(
                &verifier,
                &chain(&client, &issuer, &root),
                "ferun.feos.test",
            ),
            "server authentication",
        );
    }

    #[test]
    fn the_pinned_certificate_must_be_a_ca() {
        let root = Ca::root("root");
        let issuer = root.intermediate("issuer");
        let key = key();
        let leaf = issuer.leaf(
            &["ferun.feos.test"],
            &["spiffe://feos.local/ferun"],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        // The issuer may be pinned: it is a CA, and the chain leads to it.
        let verifier = join_verifier(&issuer.cert);
        verify(
            &verifier,
            &[&leaf, &issuer.cert, &root.cert],
            "ferun.feos.test",
        )
        .unwrap();
        // Not a CA, pinned and sent after the server's own.
        let other_key = self::key();
        let not_ca = issuer.leaf(
            &["x.feos.test"],
            &[],
            ExtendedKeyUsagePurpose::ServerAuth,
            &other_key,
        );
        let verifier = join_verifier(&not_ca);
        refused(
            verify(
                &verifier,
                &[&leaf, &not_ca, &issuer.cert],
                "ferun.feos.test",
            ),
            "is not a CA",
        );
    }

    #[test]
    fn later_connections_check_the_trust_bundle() {
        let root = Ca::root("root");
        let issuer = root.intermediate("issuer");
        let key = key();
        let verifier =
            ControlPlaneVerifier::new(&provider(), &[root.cert.der().clone()], "feos.local")
                .unwrap();
        let leaf = issuer.leaf(
            &["ferun.feos.test"],
            &["spiffe://feos.local/ferun"],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        verify(&verifier, &[&leaf, &issuer.cert], "ferun.feos.test").unwrap();
        refused(
            verify(&verifier, &[&leaf, &issuer.cert], "other.feos.test"),
            "not valid for name",
        );
        let pod = issuer.leaf(
            &["ferun.feos.test"],
            &["spiffe://feos.local/project/t/sandbox/s"],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        refused(
            verify(&verifier, &[&pod, &issuer.cert], "ferun.feos.test"),
            "not the control plane",
        );
        let other = Ca::root("other").intermediate("issuer");
        let foreign = other.leaf(
            &["ferun.feos.test"],
            &["spiffe://feos.local/ferun"],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        refused(
            verify(&verifier, &[&foreign, &other.cert], "ferun.feos.test"),
            "UnknownIssuer",
        );
    }

    #[test]
    fn a_sink_is_checked_for_its_uri_not_its_name() {
        let root = Ca::root("root");
        let issuer = root.intermediate("issuer");
        let key = key();
        let verifier =
            SinkVerifier::new(&provider(), &[root.cert.der().clone()], "feos.local").unwrap();
        let sink = |dns: &[&str], uris: &[&str], usage| issuer.leaf(dns, uris, usage, &key);
        let server = ExtendedKeyUsagePurpose::ServerAuth;
        // Whatever name it is dialed by: an address, or a name it lacks.
        let leaf = sink(
            &["sink.feos.test"],
            &["spiffe://feos.local/sink/a-1"],
            server.clone(),
        );
        verify(&verifier, &[&leaf, &issuer.cert], "sink.feos.test").unwrap();
        verify(&verifier, &[&leaf, &issuer.cert], "10.0.0.7").unwrap();
        verify(&verifier, &[&leaf, &issuer.cert], "other.feos.test").unwrap();
        let no_dns = sink(&[], &["spiffe://feos.local/sink/b"], server.clone());
        verify(&verifier, &[&no_dns, &issuer.cert], "10.0.0.7").unwrap();
        // Not a sink: the control plane, a pod, another trust domain, a
        // sink without a name, two URIs.
        for uris in [
            &["spiffe://feos.local/ferun"][..],
            &["spiffe://feos.local/project/t/sandbox/s"],
            &["spiffe://other.local/sink/a"],
            &["spiffe://feos.local/sink/"],
            &["spiffe://feos.local/sink/a/b"],
            &["spiffe://feos.local/sink/a", "spiffe://feos.local/sink/b"],
            &[],
        ] {
            let leaf = sink(&["sink.feos.test"], uris, server.clone());
            refused(
                verify(&verifier, &[&leaf, &issuer.cert], "sink.feos.test"),
                "not a sink",
            );
        }
        // A client's certificate, another CA's.
        let client = sink(
            &[],
            &["spiffe://feos.local/sink/a"],
            ExtendedKeyUsagePurpose::ClientAuth,
        );
        assert!(verify(&verifier, &[&client, &issuer.cert], "x").is_err());
        let other = Ca::root("other").intermediate("issuer");
        let foreign = other.leaf(&[], &["spiffe://feos.local/sink/a"], server, &key);
        refused(
            verify(&verifier, &[&foreign, &other.cert], "x"),
            "UnknownIssuer",
        );
    }

    /// The roots `ferund init-ca` made before ADR 0024 constrain URIs to
    /// the trust domain, which rustls-webpki cannot check: the root is
    /// trusted for its key, and an issuer without a URI constraint, as
    /// `ferund new-issuer` makes, anchors the control plane, on a join and
    /// after it.
    #[test]
    fn a_root_that_constrains_uris_still_anchors_the_control_plane() {
        let root = Ca::constrained_root("root", &["feos.test"], &["feos.local"]);
        let issuer = root.constrained_intermediate("issuer", &["feos.test"], &[]);
        let key = key();
        let leaf = issuer.leaf(
            &["ferun.feos.test"],
            &["spiffe://feos.local/ferun"],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        let bundle =
            ControlPlaneVerifier::new(&provider(), &[root.cert.der().clone()], "feos.local")
                .unwrap();
        verify(&bundle, &[&leaf, &issuer.cert], "ferun.feos.test").unwrap();
        let join = join_verifier(&root.cert);
        verify(&join, &[&leaf, &issuer.cert, &root.cert], "ferun.feos.test").unwrap();
        // A pod's certificate of the same issuer is still refused.
        let pod = issuer.leaf(
            &["ferun.feos.test"],
            &["spiffe://feos.local/project/t/sandbox/s"],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        refused(
            verify(&bundle, &[&pod, &issuer.cert], "ferun.feos.test"),
            "not the control plane",
        );
    }

    /// An issuer that constrains URIs, as `ferund init-ca` made before ADR
    /// 0024, fails every chain to the control plane's URI name: that is
    /// rustls-webpki's rule, and why such an issuer is replaced.
    #[test]
    fn an_issuer_that_constrains_uris_is_refused() {
        let root = Ca::constrained_root("root", &["feos.test"], &["feos.local"]);
        let issuer = root.constrained_intermediate("issuer", &["feos.test"], &["feos.local"]);
        let key = key();
        let leaf = issuer.leaf(
            &["ferun.feos.test"],
            &["spiffe://feos.local/ferun"],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        let bundle =
            ControlPlaneVerifier::new(&provider(), &[root.cert.der().clone()], "feos.local")
                .unwrap();
        refused(
            verify(&bundle, &[&leaf, &issuer.cert], "ferun.feos.test"),
            "NameConstraintViolation",
        );
        refused(
            verify(
                &join_verifier(&root.cert),
                &[&leaf, &issuer.cert, &root.cert],
                "ferun.feos.test",
            ),
            "NameConstraintViolation",
        );
    }

    /// Only the root's constraints are left out: the issuer's DNS and IP
    /// constraints still bind the control plane's certificate.
    #[test]
    fn the_issuers_name_constraints_still_hold() {
        let root = Ca::constrained_root("root", &["feos.test"], &["feos.local"]);
        let issuer = root.constrained_intermediate("issuer", &["feos.test"], &[]);
        let key = key();
        let outside = issuer.leaf(
            &["ferun.other.test"],
            &["spiffe://feos.local/ferun"],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        let bundle =
            ControlPlaneVerifier::new(&provider(), &[root.cert.der().clone()], "feos.local")
                .unwrap();
        refused(
            verify(&bundle, &[&outside, &issuer.cert], "ferun.other.test"),
            "NameConstraintViolation",
        );
    }

    /// A rotation puts a new root beside the old one, under the same name
    /// (`ferund init-ca` names every root after the trust domain): the
    /// bundle anchors chains from either, and a chain whose issuer claims
    /// the name without the key is still refused.
    #[test]
    fn a_bundle_of_two_roots_with_one_name_anchors_either() {
        let old = Ca::constrained_root("root", &["feos.test"], &["feos.local"]);
        let new = Ca::constrained_root("root", &["feos.test"], &[]);
        let old_issuer = old.constrained_intermediate("issuer", &["feos.test"], &[]);
        let new_issuer = new.constrained_intermediate("issuer", &["feos.test"], &[]);
        let key = key();
        let ferun = "spiffe://feos.local/ferun";
        let bundle = ControlPlaneVerifier::new(
            &provider(),
            &[old.cert.der().clone(), new.cert.der().clone()],
            "feos.local",
        )
        .unwrap();
        for issuer in [&old_issuer, &new_issuer] {
            let leaf = issuer.leaf(
                &["ferun.feos.test"],
                &[ferun],
                ExtendedKeyUsagePurpose::ServerAuth,
                &key,
            );
            verify(&bundle, &[&leaf, &issuer.cert], "ferun.feos.test").unwrap();
        }
        // The new root alone: the old issuer's chain has no anchor.
        let only_new =
            ControlPlaneVerifier::new(&provider(), &[new.cert.der().clone()], "feos.local")
                .unwrap();
        let old_leaf = old_issuer.leaf(
            &["ferun.feos.test"],
            &[ferun],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        refused(
            verify(&only_new, &[&old_leaf, &old_issuer.cert], "ferun.feos.test"),
            "BadSignature",
        );
        // A join pinned to the new root takes the new chain.
        let new_leaf = new_issuer.leaf(
            &["ferun.feos.test"],
            &[ferun],
            ExtendedKeyUsagePurpose::ServerAuth,
            &key,
        );
        verify(
            &join_verifier(&new.cert),
            &[&new_leaf, &new_issuer.cert, &new.cert],
            "ferun.feos.test",
        )
        .unwrap();
    }
}
