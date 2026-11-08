// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! rustls's side: the configurations, the resolvers that fetch the
//! workload's certificate per handshake, the verifiers that check peers
//! against the platform's trust bundle as it is now, and the peer's ID.
//!
//! The bundle is fetched again after a minute, as the chain is, so that a
//! configuration made at start sees a new root without being made again.
//! A verifier that cannot fetch it keeps the bundle it had.

use std::fmt;
use std::sync::{Arc, Mutex};
use std::time::Instant;

use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::client::{ResolvesClientCert, WebPkiServerVerifier};
use rustls::crypto::CryptoProvider;
use rustls::pki_types::pem::PemObject;
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::server::danger::{ClientCertVerified, ClientCertVerifier};
use rustls::server::{ClientHello, ResolvesServerCert, WebPkiClientVerifier};
use rustls::sign::CertifiedKey;
use rustls::{
    CommonState, DigitallySignedStruct, DistinguishedName, RootCertStore, SignatureScheme,
};

use crate::{lock, Error, Inner, CACHE_TTL};

/// The certificates of a PEM text.
pub(crate) fn certificates(pem: &str) -> Result<Vec<CertificateDer<'static>>, Error> {
    CertificateDer::pem_slice_iter(pem.as_bytes())
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| Error::Certificate(format!("a certificate that does not parse: {e}")))
}

/// The platform's trust bundle.
#[derive(Clone)]
pub(crate) struct Bundle {
    roots: Arc<RootCertStore>,
}

impl Bundle {
    pub(crate) fn from_pem(pem: &str) -> Result<Bundle, Error> {
        let mut roots = RootCertStore::empty();
        let (added, _) = roots.add_parsable_certificates(certificates(pem)?);
        if added == 0 {
            return Err(Error::Certificate(
                "the trust bundle holds no usable root".into(),
            ));
        }
        Ok(Bundle {
            roots: Arc::new(roots),
        })
    }
}

fn provider() -> Arc<CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

pub(crate) fn client_config(inner: Arc<Inner>) -> Result<Arc<rustls::ClientConfig>, Error> {
    let verifier = Arc::new(ServerVerifier::new(inner.clone())?);
    let config = rustls::ClientConfig::builder_with_provider(provider())
        .with_protocol_versions(&[&rustls::version::TLS13])?
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_client_cert_resolver(Arc::new(Resolver(inner)));
    Ok(Arc::new(config))
}

/// A client that checks the server by its SPIFFE ID, against the
/// platform's bundle alone: an ID means something only under the
/// platform's CA. The name the handshake carries is not checked.
pub(crate) fn pinned_client_config(
    inner: Arc<Inner>,
    peer: SpiffeId,
) -> Result<Arc<rustls::ClientConfig>, Error> {
    let verifier = Arc::new(PinnedVerifier::new(inner.clone(), peer)?);
    let config = rustls::ClientConfig::builder_with_provider(provider())
        .with_protocol_versions(&[&rustls::version::TLS13])?
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_client_cert_resolver(Arc::new(Resolver(inner)));
    Ok(Arc::new(config))
}

pub(crate) fn server_config(inner: Arc<Inner>) -> Result<Arc<rustls::ServerConfig>, Error> {
    let verifier = Arc::new(ClientVerifier::new(inner.clone())?);
    let config = rustls::ServerConfig::builder_with_provider(provider())
        .with_protocol_versions(&[&rustls::version::TLS13])?
        .with_client_cert_verifier(verifier)
        .with_cert_resolver(Arc::new(Resolver(inner)));
    Ok(Arc::new(config))
}

/// Resolves the workload's certificate in every handshake, for a client
/// and a server: from the cache, or from the signer.
struct Resolver(Arc<Inner>);

impl fmt::Debug for Resolver {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("feid::Resolver")
    }
}

impl Resolver {
    fn key(&self, schemes: &[SignatureScheme]) -> Option<Arc<CertifiedKey>> {
        if !schemes.contains(&SignatureScheme::ED25519) {
            log::warn!("feid: the peer does not take Ed25519; no certificate is presented");
            return None;
        }
        match self.0.certified_key() {
            Ok(key) => Some(key),
            Err(e) => {
                log::error!("feid: the workload's certificate: {e}");
                None
            }
        }
    }
}

impl ResolvesClientCert for Resolver {
    fn resolve(&self, _hints: &[&[u8]], schemes: &[SignatureScheme]) -> Option<Arc<CertifiedKey>> {
        self.key(schemes)
    }

    fn has_certs(&self) -> bool {
        true
    }
}

impl ResolvesServerCert for Resolver {
    fn resolve(&self, hello: ClientHello<'_>) -> Option<Arc<CertifiedKey>> {
        self.key(hello.signature_schemes())
    }
}

/// A verifier made from the bundle, again once the bundle is old.
struct Refreshing<V: ?Sized> {
    inner: Arc<Inner>,
    make: fn(&Bundle) -> Result<Arc<V>, Error>,
    current: Mutex<(Arc<V>, Instant)>,
}

impl<V: ?Sized> Refreshing<V> {
    fn new(inner: Arc<Inner>, make: fn(&Bundle) -> Result<Arc<V>, Error>) -> Result<Self, Error> {
        let first = make(&inner.bundle()?)?;
        Ok(Refreshing {
            inner,
            make,
            current: Mutex::new((first, Instant::now())),
        })
    }

    fn get(&self) -> Arc<V> {
        let mut current = lock(&self.current);
        if current.1.elapsed() >= CACHE_TTL {
            match self.inner.bundle().and_then(|b| (self.make)(&b)) {
                Ok(v) => *current = (v, Instant::now()),
                Err(e) => log::warn!("feid: the trust bundle: {e}; the one in place is kept"),
            }
        }
        current.0.clone()
    }
}

struct ServerVerifier {
    webpki: Refreshing<WebPkiServerVerifier>,
    provider: Arc<CryptoProvider>,
}

impl ServerVerifier {
    fn new(inner: Arc<Inner>) -> Result<Self, Error> {
        fn make(bundle: &Bundle) -> Result<Arc<WebPkiServerVerifier>, Error> {
            let mut roots = (*bundle.roots).clone();
            roots.extend(webpki_roots::TLS_SERVER_ROOTS.iter().cloned());
            WebPkiServerVerifier::builder_with_provider(Arc::new(roots), provider())
                .build()
                .map_err(|e| Error::Certificate(format!("a verifier of servers: {e}")))
        }
        Ok(ServerVerifier {
            webpki: Refreshing::new(inner, make)?,
            provider: provider(),
        })
    }
}

impl fmt::Debug for ServerVerifier {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("feid::ServerVerifier")
    }
}

impl ServerCertVerifier for ServerVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        server_name: &ServerName<'_>,
        ocsp: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        self.webpki
            .get()
            .verify_server_cert(end_entity, intermediates, server_name, ocsp, now)
    }

    fn verify_tls12_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        Err(rustls::Error::General("feid speaks TLS 1.3 only".into()))
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(
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

/// Checks a server's chain against the platform's bundle and its one
/// URI name against the ID it must have.
struct PinnedVerifier {
    roots: Refreshing<RootCertStore>,
    peer: SpiffeId,
    provider: Arc<CryptoProvider>,
}

impl PinnedVerifier {
    fn new(inner: Arc<Inner>, peer: SpiffeId) -> Result<Self, Error> {
        fn make(bundle: &Bundle) -> Result<Arc<RootCertStore>, Error> {
            Ok(bundle.roots.clone())
        }
        Ok(PinnedVerifier {
            roots: Refreshing::new(inner, make)?,
            peer,
            provider: provider(),
        })
    }
}

impl fmt::Debug for PinnedVerifier {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "feid::PinnedVerifier({})", self.peer)
    }
}

impl ServerCertVerifier for PinnedVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        let cert = rustls::server::ParsedCertificate::try_from(end_entity)?;
        rustls::client::verify_server_cert_signed_by_trust_anchor(
            &cert,
            &self.roots.get(),
            intermediates,
            now,
            self.provider.signature_verification_algorithms.all,
        )?;
        match SpiffeId::of_certificate(end_entity) {
            Some(id) if id == self.peer => Ok(ServerCertVerified::assertion()),
            Some(id) => Err(rustls::Error::General(format!(
                "the server is {id}, not {}",
                self.peer
            ))),
            None => Err(rustls::Error::General(format!(
                "the server has no SPIFFE ID; {} was expected",
                self.peer
            ))),
        }
    }

    fn verify_tls12_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        Err(rustls::Error::General("feid speaks TLS 1.3 only".into()))
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(
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

struct ClientVerifier {
    webpki: Refreshing<dyn ClientCertVerifier>,
    provider: Arc<CryptoProvider>,
}

impl ClientVerifier {
    fn new(inner: Arc<Inner>) -> Result<Self, Error> {
        fn make(bundle: &Bundle) -> Result<Arc<dyn ClientCertVerifier>, Error> {
            WebPkiClientVerifier::builder_with_provider(bundle.roots.clone(), provider())
                .build()
                .map_err(|e| Error::Certificate(format!("a verifier of clients: {e}")))
        }
        Ok(ClientVerifier {
            webpki: Refreshing::new(inner, make)?,
            provider: provider(),
        })
    }
}

impl fmt::Debug for ClientVerifier {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("feid::ClientVerifier")
    }
}

impl ClientCertVerifier for ClientVerifier {
    fn root_hint_subjects(&self) -> &[DistinguishedName] {
        // The roots change under the verifier; a client of the platform
        // has one certificate anyway.
        &[]
    }

    fn verify_client_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        now: UnixTime,
    ) -> Result<ClientCertVerified, rustls::Error> {
        self.webpki
            .get()
            .verify_client_cert(end_entity, intermediates, now)
    }

    fn verify_tls12_signature(
        &self,
        _message: &[u8],
        _cert: &CertificateDer<'_>,
        _dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        Err(rustls::Error::General("feid speaks TLS 1.3 only".into()))
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(
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

/// A SPIFFE ID: `spiffe://<trust domain>/<path>`.
#[derive(Clone, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct SpiffeId(String);

impl SpiffeId {
    /// `s` when it is a SPIFFE ID: the scheme, a trust domain of lowercase
    /// letters, digits, `.`, `-` and `_`, and a path, if any, of segments
    /// that are neither empty nor `.` or `..`.
    pub fn parse(s: &str) -> Option<SpiffeId> {
        let rest = s.strip_prefix("spiffe://")?;
        let (domain, path) = match rest.find('/') {
            Some(i) => (&rest[..i], &rest[i..]),
            None => (rest, ""),
        };
        let domain_ok = !domain.is_empty()
            && domain
                .bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b"-._".contains(&b));
        let path_ok = path.is_empty()
            || path[1..].split('/').all(|seg| {
                !seg.is_empty()
                    && seg != "."
                    && seg != ".."
                    && seg
                        .bytes()
                        .all(|b| b.is_ascii_alphanumeric() || b"-._".contains(&b))
            });
        (domain_ok && path_ok).then(|| SpiffeId(s.to_string()))
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }

    pub fn trust_domain(&self) -> &str {
        let rest = &self.0["spiffe://".len()..];
        rest.split('/').next().unwrap_or(rest)
    }

    /// The path, with its leading `/`; empty for a trust domain's own ID.
    pub fn path(&self) -> &str {
        let rest = &self.0["spiffe://".len()..];
        rest.find('/').map_or("", |i| &rest[i..])
    }

    /// The ID of a certificate: its one URI name, when that is a SPIFFE
    /// ID. A certificate with more than one URI name has none.
    pub(crate) fn of_certificate(der: &CertificateDer<'_>) -> Option<SpiffeId> {
        use x509_parser::prelude::{FromDer, GeneralName, X509Certificate};
        let (_, cert) = X509Certificate::from_der(der.as_ref()).ok()?;
        let san = cert.subject_alternative_name().ok()??;
        let mut uris = san.value.general_names.iter().filter_map(|n| match n {
            GeneralName::URI(u) => Some(*u),
            _ => None,
        });
        let (first, second) = (uris.next()?, uris.next());
        if second.is_some() {
            return None;
        }
        SpiffeId::parse(first)
    }
}

impl fmt::Display for SpiffeId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

/// The SPIFFE ID of a connection's peer, from the certificate it
/// presented; None without one. A server's handshake has checked the
/// certificate against the platform's bundle before.
pub fn peer_id(conn: &CommonState) -> Option<SpiffeId> {
    SpiffeId::of_certificate(conn.peer_certificates()?.first()?)
}

/// What a handshake's report says of the peer's certificate.
#[cfg(feature = "tokio")]
pub(crate) fn describe_peer(conn: &CommonState, report: &mut feid_proto::HandshakeReport) {
    use x509_parser::prelude::{FromDer, X509Certificate};
    let Some(leaf) = conn.peer_certificates().and_then(|c| c.first()) else {
        return;
    };
    let digest = ring::digest::digest(&ring::digest::SHA256, leaf.as_ref());
    report.peer_fingerprint = digest.as_ref().iter().map(|b| format!("{b:02x}")).collect();
    if let Some(id) = SpiffeId::of_certificate(leaf) {
        report.peer_spiffe = id.0;
    }
    if let Ok((_, cert)) = X509Certificate::from_der(leaf.as_ref()) {
        report.peer_subject = cert.subject().to_string();
        report.peer_issuer = cert.issuer().to_string();
        report.peer_serial = cert.raw_serial_as_string().replace(':', "");
        report.peer_not_after = rfc3339(cert.validity().not_after.timestamp());
    }
}

/// Unix seconds as `YYYY-MM-DDTHH:MM:SSZ`.
#[cfg(feature = "tokio")]
pub(crate) fn rfc3339(unix: i64) -> String {
    let (days, secs) = (unix.div_euclid(86_400), unix.rem_euclid(86_400));
    // Howard Hinnant's days_from_civil, backwards.
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + i64::from(month <= 2);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}Z",
        secs / 3600,
        secs % 3600 / 60,
        secs % 60
    )
}
