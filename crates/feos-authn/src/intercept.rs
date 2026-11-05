// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The intercept mode (ADR 0028 §5): a pod's TLS connection to
//! 169.254.169.1:443 under the name of one of its intercept bindings'
//! upstreams. The ClientHello's SNI must name exactly such an upstream;
//! anything else is closed before a certificate is sent. The certificate
//! is a leaf for exactly that name, ECDSA P-256, valid 24 hours, issued in
//! memory from the newest of the pod's interception CAs and kept per name
//! until an hour before it ends. ALPN offers h2 and http/1.1.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
use rustls::server::{Acceptor, ServerConfig};
use tokio::net::TcpStream;
use tokio_rustls::LazyConfigAcceptor;

use crate::proxy::{self, Ctx, Entry};
use crate::upstream::{provider, ALPN_H2, ALPN_HTTP11};

pub const LEAF_LIFETIME: Duration = Duration::from_secs(24 * 3600);
/// A leaf is issued again when less than this is left of it.
const LEAF_RENEW_BEFORE: Duration = Duration::from_secs(3600);
/// Backdating against a guest clock a little behind the node's.
const LEAF_BACKDATE: Duration = Duration::from_secs(5 * 60);
const HELLO_TIMEOUT: Duration = Duration::from_secs(10);

pub struct LeafIssuer {
    issuer: rcgen::Issuer<'static, rcgen::KeyPair>,
    cache: Mutex<HashMap<String, (Arc<ServerConfig>, Instant)>>,
}

impl LeafIssuer {
    /// The issuer of a CA's certificate and its PKCS#8 key, both PEM.
    pub fn from_ca(cert_pem: &str, key_pem: &str) -> Result<LeafIssuer, String> {
        // The key's errors say where, never what.
        let key = rcgen::KeyPair::from_pem(key_pem)
            .map_err(|_| "the interception CA's key is no PKCS#8 PEM key".to_string())?;
        let issuer = rcgen::Issuer::from_ca_cert_pem(cert_pem, key)
            .map_err(|e| format!("the interception CA's certificate: {e}"))?;
        Ok(LeafIssuer {
            issuer,
            cache: Mutex::new(HashMap::new()),
        })
    }

    /// A leaf for `name` and its key, DER.
    pub fn issue(
        &self,
        name: &str,
    ) -> Result<(CertificateDer<'static>, PrivateKeyDer<'static>), String> {
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256)
            .map_err(|e| format!("a leaf key: {e}"))?;
        let mut p = rcgen::CertificateParams::new(vec![name.to_string()])
            .map_err(|e| format!("a leaf for {name}: {e}"))?;
        p.distinguished_name = rcgen::DistinguishedName::new();
        p.distinguished_name
            .push(rcgen::DnType::CommonName, name.to_string());
        p.is_ca = rcgen::IsCa::ExplicitNoCa;
        p.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
        p.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
        p.use_authority_key_identifier_extension = true;
        let mut serial = [0u8; 16];
        ring::rand::SecureRandom::fill(&ring::rand::SystemRandom::new(), &mut serial)
            .map_err(|_| "the system's random source failed".to_string())?;
        serial[0] &= 0x7f;
        p.serial_number = Some(rcgen::SerialNumber::from_slice(&serial));
        let now = time::OffsetDateTime::now_utc();
        p.not_before = now - LEAF_BACKDATE;
        p.not_after = p.not_before + LEAF_LIFETIME;
        let cert = p
            .signed_by(&key, &self.issuer)
            .map_err(|e| format!("sign a leaf for {name}: {e}"))?;
        Ok((
            cert.der().clone(),
            PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der())),
        ))
    }

    /// The TLS server's configuration for `name`, its leaf from the cache
    /// or issued now.
    pub fn server_config(&self, name: &str) -> Result<Arc<ServerConfig>, String> {
        let now = Instant::now();
        {
            let cache = self.cache.lock().unwrap_or_else(|e| e.into_inner());
            if let Some((c, until)) = cache.get(name)
                && now < *until
            {
                return Ok(c.clone());
            }
        }
        let (cert, key) = self.issue(name)?;
        let mut config = ServerConfig::builder_with_provider(provider())
            .with_protocol_versions(&[&rustls::version::TLS13, &rustls::version::TLS12])
            .map_err(|e| format!("TLS: {e}"))?
            .with_no_client_auth()
            .with_single_cert(vec![cert], key)
            .map_err(|e| format!("TLS with the leaf for {name}: {e}"))?;
        config.alpn_protocols = vec![ALPN_H2.to_vec(), ALPN_HTTP11.to_vec()];
        let config = Arc::new(config);
        // Issued now, backdated: what is left is the lifetime less the
        // backdating.
        let until = now + LEAF_LIFETIME - LEAF_BACKDATE - LEAF_RENEW_BEFORE;
        self.cache
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .insert(name.to_string(), (config.clone(), until));
        Ok(config)
    }
}

/// An SNI as the bindings key it: lowercase, without a final dot.
pub fn sni_key(sni: &str) -> String {
    sni.strip_suffix('.').unwrap_or(sni).to_ascii_lowercase()
}

/// Serves one connection to port 443.
pub async fn serve(ctx: Arc<Ctx>, stream: TcpStream) {
    // Without bindings there is no CA to answer with: wait a little, then
    // close.
    let Some(snap) = ctx.wait_ready(Instant::now() + proxy::READY_WAIT).await else {
        log::debug!("intercept: no bindings yet; closing a connection");
        return;
    };
    let start = match tokio::time::timeout(
        HELLO_TIMEOUT,
        LazyConfigAcceptor::new(Acceptor::default(), stream),
    )
    .await
    {
        Ok(Ok(s)) => s,
        Ok(Err(e)) => {
            log::debug!("intercept: no ClientHello: {e}");
            return;
        }
        Err(_) => return,
    };
    let Some(sni) = start.client_hello().server_name().map(sni_key) else {
        log::debug!("intercept: a ClientHello without SNI; refused");
        return;
    };
    // Only an intercept binding's upstream, and only with a CA to issue
    // from: anything else is refused before a certificate is sent.
    let (Some(_), Some(leaf)) = (snap.by_host.get(&sni), snap.leaf.clone()) else {
        log::debug!("intercept: SNI {sni:?} names no intercept binding; refused");
        return;
    };
    let config = match leaf.server_config(&sni) {
        Ok(c) => c,
        Err(e) => {
            log::error!("intercept: {e}");
            return;
        }
    };
    let tls = match start.into_stream(config).await {
        Ok(t) => t,
        Err(e) => {
            log::debug!("intercept: TLS with the pod for {sni}: {e}");
            return;
        }
    };
    let h2 = tls.get_ref().1.alpn_protocol() == Some(ALPN_H2);
    proxy::serve_http(ctx, tls, Arc::new(Entry::Intercept { host: sni }), h2).await;
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    /// A test interception CA: ECDSA P-256, name-constrained to `names`.
    pub fn test_ca(names: &[&str]) -> (String, String) {
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
        let mut p = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        p.distinguished_name.push(
            rcgen::DnType::CommonName,
            "FeOS interception CA k2v8d0h3q9x",
        );
        p.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Constrained(0));
        p.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        p.name_constraints = Some(rcgen::NameConstraints {
            permitted_subtrees: names
                .iter()
                .map(|n| rcgen::GeneralSubtree::DnsName(n.to_string()))
                .collect(),
            excluded_subtrees: vec![],
        });
        let cert = p.self_signed(&key).unwrap();
        (cert.pem(), key.serialize_pem())
    }

    fn verify(ca_pem: &str, leaf: &CertificateDer<'_>, name: &str) -> Result<(), rustls::Error> {
        use rustls::client::danger::ServerCertVerifier;
        let mut roots = rustls::RootCertStore::empty();
        for c in rustls::pki_types::pem::PemObject::pem_slice_iter(ca_pem.as_bytes()) {
            roots.add(c.unwrap()).unwrap();
        }
        let v = rustls::client::WebPkiServerVerifier::builder_with_provider(
            Arc::new(roots),
            provider(),
        )
        .build()
        .unwrap();
        let sn = rustls::pki_types::ServerName::try_from(name.to_string()).unwrap();
        v.verify_server_cert(leaf, &[], &sn, &[], rustls::pki_types::UnixTime::now())
            .map(|_| ())
    }

    #[test]
    fn issues_a_leaf_for_exactly_the_name() {
        let (ca, key) = test_ca(&["api.erp.example"]);
        let l = LeafIssuer::from_ca(&ca, &key).unwrap();
        let (leaf, _) = l.issue("api.erp.example").unwrap();
        verify(&ca, &leaf, "api.erp.example").unwrap();
        assert!(verify(&ca, &leaf, "other.example").is_err());
        let (_, x) = x509_parser::parse_x509_certificate(&leaf).unwrap();
        let life = x.validity().not_after.timestamp() - x.validity().not_before.timestamp();
        assert_eq!(life, 24 * 3600);
        assert_eq!(
            x.public_key().algorithm.algorithm.to_id_string(),
            "1.2.840.10045.2.1"
        );
        // The CA's name constraints hold: a leaf for a name outside them
        // does not verify.
        let (outside, _) = l.issue("evil.example").unwrap();
        assert!(verify(&ca, &outside, "evil.example").is_err());
    }

    #[test]
    fn caches_per_name() {
        let (ca, key) = test_ca(&["a.example", "b.example"]);
        let l = LeafIssuer::from_ca(&ca, &key).unwrap();
        let a1 = l.server_config("a.example").unwrap();
        let a2 = l.server_config("a.example").unwrap();
        let b = l.server_config("b.example").unwrap();
        assert!(Arc::ptr_eq(&a1, &a2));
        assert!(!Arc::ptr_eq(&a1, &b));
        assert_eq!(sni_key("A.Example."), "a.example");
    }

    #[test]
    fn refuses_a_broken_ca() {
        let (ca, _) = test_ca(&["a.example"]);
        assert!(LeafIssuer::from_ca(&ca, "no key").is_err());
        let (_, key) = test_ca(&["a.example"]);
        assert!(LeafIssuer::from_ca("no cert", &key).is_err());
    }
}
