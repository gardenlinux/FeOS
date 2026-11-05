// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's bindings as feos-authn serves them: opened from the box the
//! control plane sealed to the run, checked, and made into a [`Snapshot`]
//! with what each HTTP binding needs at run time (its pools, its token,
//! its limits). A new set replaces the old one whole; requests under way
//! finish with the set they started with.
//!
//! Kubernetes and SSH bindings are opened and kept with the set; nothing
//! serves them yet.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Instant;

use base64::Engine;
use feos_proto::fleet_service::{
    http_binding, HttpBinding, HttpBindingMode, KubeBinding, PodBindings, SshBinding,
};
use http::{HeaderName, HeaderValue, Uri};
use prost::Message;
use tokio::sync::Semaphore;

use crate::audit::Audit;
use crate::dest::Net;
use crate::intercept::LeafIssuer;
use crate::keys::Keys;
use crate::oauth::{self, OAuth};
use crate::ratelimit::Bucket;
use crate::upstream::{self, Connector, HttpClient, ALPN_H2, ALPN_HTTP11};

/// Requests in flight per binding, streamed bodies and upgrades included
/// (ADR 0028 §7); over it, 429.
pub const MAX_REQUESTS_PER_BINDING: usize = 256;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Mode {
    Name,
    Intercept,
}

impl Mode {
    pub fn as_str(self) -> &'static str {
        match self {
            Mode::Name => "name",
            Mode::Intercept => "intercept",
        }
    }
}

/// An upstream origin, `https://host[:port]`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Upstream {
    /// Lowercase, without an IPv6 literal's brackets.
    pub host: String,
    pub port: u16,
    /// As a URI's authority: brackets kept, the port only when not 443.
    pub authority: String,
}

impl Upstream {
    pub fn parse(s: &str) -> Result<Upstream, String> {
        let rest = s
            .strip_prefix("https://")
            .ok_or_else(|| format!("the upstream {s:?} is not https://host[:port]"))?;
        let rest = rest.strip_suffix('/').unwrap_or(rest);
        if rest.is_empty() || rest.contains(['/', '?', '#', '@']) {
            return Err(format!("the upstream {s:?} is not https://host[:port]"));
        }
        let uri: Uri = format!("https://{rest}/")
            .parse()
            .map_err(|_| format!("the upstream {s:?} is not https://host[:port]"))?;
        let host = upstream::bare_host(uri.host().unwrap_or_default()).to_ascii_lowercase();
        if host.is_empty() {
            return Err(format!("the upstream {s:?} has no host"));
        }
        let port = uri.port_u16().unwrap_or(443);
        let shown = if host.contains(':') {
            format!("[{host}]")
        } else {
            host.clone()
        };
        let authority = if port == 443 {
            shown
        } else {
            format!("{shown}:{port}")
        };
        Ok(Upstream {
            host,
            port,
            authority,
        })
    }

    pub fn origin(&self) -> String {
        format!("https://{}", self.authority)
    }
}

pub enum Credential {
    Headers(Vec<(HeaderName, HeaderValue)>),
    Basic(HeaderValue),
    OAuth2(Arc<OAuth>),
    /// mTLS to the upstream; the client's TLS config holds it.
    ClientCertificate,
}

impl Credential {
    pub fn kind(&self) -> &'static str {
        match self {
            Credential::Headers(_) => "headers",
            Credential::Basic(_) => "basic",
            Credential::OAuth2(_) => "oauth2",
            Credential::ClientCertificate => "clientCertificate",
        }
    }

    /// The headers the credential sets, which a pod's request loses.
    pub fn header_names(&self) -> Vec<HeaderName> {
        match self {
            Credential::Headers(h) => h.iter().map(|(n, _)| n.clone()).collect(),
            Credential::Basic(_) | Credential::OAuth2(_) => vec![http::header::AUTHORIZATION],
            Credential::ClientCertificate => Vec::new(),
        }
    }
}

/// What a binding needs to serve.
pub struct Live {
    pub upstream: Upstream,
    pub prefixes: Vec<String>,
    pub forwarded_host: bool,
    pub credential: Credential,
    pub limit: Option<Mutex<Bucket>>,
    pub requests: Arc<Semaphore>,
    /// ALPN h2 and http/1.1.
    pub client: HttpClient,
    /// ALPN http/1.1 only, for upgrades.
    pub client_h1: HttpClient,
}

pub struct Binding {
    pub name: String,
    pub mode: Mode,
    /// The upstream's host as the binding names it, for records.
    pub host: String,
    /// Why it cannot be served, or what it serves with.
    pub live: Result<Live, String>,
}

impl Binding {
    pub fn credential_kind(&self) -> Option<&'static str> {
        self.live.as_ref().ok().map(|l| l.credential.kind())
    }
}

#[derive(Default)]
pub struct Snapshot {
    pub version: String,
    /// Name-mode bindings by their label.
    pub by_name: HashMap<String, Arc<Binding>>,
    /// Intercept-mode bindings by their upstream's host.
    pub by_host: HashMap<String, Arc<Binding>>,
    /// The leaf issuer of the newest interception CA.
    pub leaf: Option<Arc<LeafIssuer>>,
    pub kube: Vec<KubeBinding>,
    pub ssh: Vec<SshBinding>,
    /// The pod's certificate as `feosd` relayed it; unused until FeID.
    pub certificate: Option<serde_json::Value>,
    /// Bindings that cannot be served, and why: for the records and logs.
    pub invalid: Vec<(String, String)>,
}

/// Opens a SealedBindings of the run.
pub fn open(
    keys: &Keys,
    pod_id: &str,
    enc_b64: &str,
    ciphertext_b64: &str,
    version: &str,
) -> Result<PodBindings, String> {
    let b64 = base64::engine::general_purpose::STANDARD;
    let enc = b64
        .decode(enc_b64)
        .map_err(|_| "the sealed bindings' enc is no base64".to_string())?;
    let ct = b64
        .decode(ciphertext_b64)
        .map_err(|_| "the sealed bindings' ciphertext is no base64".to_string())?;
    let pt = keys
        .open_bindings(pod_id, &enc, &ct, version)
        .map_err(|e| format!("open the sealed bindings: {e}"))?;
    PodBindings::decode(pt.as_slice()).map_err(|e| format!("the opened bindings: {e}"))
}

/// A DNS label: 1 to 63 lowercase letters, digits and "-", neither first
/// nor last a "-".
pub fn is_label(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 63
        && !s.starts_with('-')
        && !s.ends_with('-')
        && s.bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}

fn header_credential(h: &HashMap<String, String>) -> Result<Credential, String> {
    if h.is_empty() {
        return Err("the header credential names no header".into());
    }
    let mut out = Vec::new();
    let mut names: Vec<_> = h.keys().collect();
    names.sort();
    for name in names {
        let n = HeaderName::from_bytes(name.as_bytes())
            .map_err(|_| format!("the credential's header {name:?} is no header name"))?;
        if is_hop_by_hop(&n) || n == http::header::HOST {
            return Err(format!("the credential may not set {n}"));
        }
        // The value's error never shows the value.
        let mut v = HeaderValue::from_str(&h[name])
            .map_err(|_| format!("the credential's header {n} has no valid value"))?;
        v.set_sensitive(true);
        out.push((n, v));
    }
    Ok(Credential::Headers(out))
}

/// The hop-by-hop headers of RFC 9110 §7.6.1, and those of old proxies.
pub fn is_hop_by_hop(n: &HeaderName) -> bool {
    matches!(
        n.as_str(),
        "connection"
            | "keep-alive"
            | "proxy-connection"
            | "transfer-encoding"
            | "te"
            | "trailer"
            | "upgrade"
    ) || n.as_str().starts_with("proxy-")
}

fn live(
    b: &HttpBinding,
    upstream: Upstream,
    net: &Arc<Net>,
    audit: &Arc<Audit>,
) -> Result<Live, String> {
    for p in &b.path_prefixes {
        if !p.starts_with('/') {
            return Err(format!("the path prefix {p:?} does not begin with /"));
        }
    }
    let mut client_cert = None;
    let credential = match &b.credential {
        None => return Err("the binding has no credential".into()),
        Some(http_binding::Credential::Headers(h)) => header_credential(&h.headers)?,
        Some(http_binding::Credential::Basic(basic)) => {
            if basic.user.contains(':') {
                return Err("the basic credential's user holds a colon".into());
            }
            let pair = format!("{}:{}", basic.user, basic.password);
            let mut v = HeaderValue::from_str(&format!(
                "Basic {}",
                base64::engine::general_purpose::STANDARD.encode(pair)
            ))
            .map_err(|_| "the basic credential makes no header".to_string())?;
            v.set_sensitive(true);
            Credential::Basic(v)
        }
        Some(http_binding::Credential::Oauth2(o)) => {
            let token_url: Uri = o
                .token_url
                .parse()
                .map_err(|_| "the token URL is no URL".to_string())?;
            if token_url.scheme_str() != Some("https") || token_url.host().is_none() {
                return Err("the token URL is not https://".into());
            }
            if o.client_id.is_empty() {
                return Err("the OAuth credential has no client ID".into());
            }
            // The token endpoint: the WebPKI, and the binding's CA where
            // it names one, for an endpoint of the same private PKI.
            let tls = upstream::tls_config(
                upstream::roots(&b.ca_pem, true)?,
                None,
                &[ALPN_H2, ALPN_HTTP11],
            )?;
            let client = upstream::client(Connector::new(net.clone(), tls));
            Credential::OAuth2(Arc::new(OAuth::new(
                b.name.clone(),
                oauth::Config {
                    token_url,
                    client_id: o.client_id.clone(),
                    client_secret: o.client_secret.clone(),
                    scope: o.scope.clone(),
                    audience: o.audience.clone(),
                    auth_in_body: o.client_auth_in_body,
                },
                client,
                audit.clone(),
            )))
        }
        Some(http_binding::Credential::ClientCertificate(c)) => {
            client_cert = Some(upstream::client_auth(&c.cert_pem, &c.key_pem)?);
            Credential::ClientCertificate
        }
    };
    let roots = upstream::roots(&b.ca_pem, false)?;
    let cert_again = client_cert
        .as_ref()
        .map(|(chain, key)| (chain.clone(), key.clone_key()));
    let tls = upstream::tls_config(roots.clone(), cert_again, &[ALPN_H2, ALPN_HTTP11])?;
    let tls_h1 = upstream::tls_config(roots, client_cert, &[ALPN_HTTP11])?;
    let limit = b
        .rate_limit
        .as_ref()
        .filter(|r| r.per_second > 0)
        .map(|r| Mutex::new(Bucket::new(r.per_second, r.burst, Instant::now())));
    Ok(Live {
        upstream,
        prefixes: b.path_prefixes.clone(),
        forwarded_host: b.forwarded_host,
        credential,
        limit,
        requests: Arc::new(Semaphore::new(MAX_REQUESTS_PER_BINDING)),
        client: upstream::client(Connector::new(net.clone(), tls)),
        client_h1: upstream::client(Connector::new(net.clone(), tls_h1)),
    })
}

/// Makes the set to serve. A binding that cannot be served keeps its name,
/// so that its requests get why (503 binding_invalid) instead of 404.
pub fn snapshot(
    set: PodBindings,
    version: String,
    certificate: Option<serde_json::Value>,
    net: &Arc<Net>,
    audit: &Arc<Audit>,
) -> Snapshot {
    let mut s = Snapshot {
        version,
        certificate,
        kube: set.kube,
        ssh: set.ssh,
        ..Default::default()
    };
    for b in &set.http {
        let mode = match HttpBindingMode::try_from(b.mode) {
            Ok(HttpBindingMode::Name) => Mode::Name,
            Ok(HttpBindingMode::Intercept) => Mode::Intercept,
            Err(_) => {
                s.invalid
                    .push((b.name.clone(), format!("unknown mode {}", b.mode)));
                continue;
            }
        };
        let upstream = Upstream::parse(&b.upstream);
        let host = upstream
            .as_ref()
            .map(|u| u.host.clone())
            .unwrap_or_default();
        let key = match mode {
            Mode::Name => {
                if !is_label(&b.name) {
                    s.invalid
                        .push((b.name.clone(), "the name is no DNS label".into()));
                    continue;
                }
                b.name.clone()
            }
            Mode::Intercept => match &upstream {
                Ok(u) => u.host.clone(),
                Err(e) => {
                    s.invalid.push((b.name.clone(), e.clone()));
                    continue;
                }
            },
        };
        let map = match mode {
            Mode::Name => &s.by_name,
            Mode::Intercept => &s.by_host,
        };
        if map.contains_key(&key) {
            s.invalid.push((
                b.name.clone(),
                format!("{key} is bound twice; the first one serves"),
            ));
            continue;
        }
        let live = upstream.and_then(|u| {
            if mode == Mode::Intercept && u.port != 443 {
                return Err("an intercepted upstream is on port 443 only".into());
            }
            live(b, u, net, audit)
        });
        if let Err(e) = &live {
            s.invalid.push((b.name.clone(), e.clone()));
        }
        let binding = Arc::new(Binding {
            name: b.name.clone(),
            mode,
            host,
            live,
        });
        match mode {
            Mode::Name => s.by_name.insert(key, binding),
            Mode::Intercept => s.by_host.insert(key, binding),
        };
    }
    if !s.by_host.is_empty() {
        match set.intercept_cas.last() {
            Some(ca) => match LeafIssuer::from_ca(&ca.cert_pem, &ca.key_pem) {
                Ok(l) => s.leaf = Some(Arc::new(l)),
                Err(e) => s.invalid.push(("intercept CA".into(), e)),
            },
            None => s.invalid.push((
                "intercept CA".into(),
                "the set has no interception CA".into(),
            )),
        }
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::dest::SystemResolver;
    use std::os::fd::OwnedFd;

    #[test]
    fn upstreams() {
        let u = Upstream::parse("https://API.erp.example").unwrap();
        assert_eq!((u.host.as_str(), u.port), ("api.erp.example", 443));
        assert_eq!(u.origin(), "https://api.erp.example");
        let u = Upstream::parse("https://api.pay.example:8443/").unwrap();
        assert_eq!(u.authority, "api.pay.example:8443");
        let u = Upstream::parse("https://[2001:db8::1]:8443").unwrap();
        assert_eq!(u.host, "2001:db8::1");
        assert_eq!(u.authority, "[2001:db8::1]:8443");
        let u = Upstream::parse("https://api.example:443").unwrap();
        assert_eq!(u.authority, "api.example");
        for bad in [
            "http://api.example",
            "https://",
            "https://api.example/v1",
            "https://api.example?x",
            "https://user@api.example",
            "api.example",
        ] {
            assert!(Upstream::parse(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn labels() {
        assert!(is_label("erp"));
        assert!(is_label("erp-2"));
        for bad in ["", "-erp", "erp-", "ERP", "erp.example", &"a".repeat(64)] {
            assert!(!is_label(bad), "{bad}");
        }
    }

    /// The vector that go/internal/pki.Seal sealed (TestBindingsVector):
    /// what the control plane sends opens here, byte for byte.
    #[test]
    fn opens_what_go_sealed() {
        let v: serde_json::Value =
            serde_json::from_str(include_str!("../testdata/sealed-bindings.json")).unwrap();
        let hexkey = v["hpkePrivateKey"].as_str().unwrap();
        let secret: [u8; 32] = (0..32)
            .map(|i| u8::from_str_radix(&hexkey[2 * i..2 * i + 2], 16).unwrap())
            .collect::<Vec<_>>()
            .try_into()
            .unwrap();
        let keys = Keys::with_hpke_secret(secret).unwrap();
        let public: String = keys
            .hpke_public()
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect();
        assert_eq!(public, v["hpkePublicKey"].as_str().unwrap());
        let pod = v["podId"].as_str().unwrap();
        let (enc, ct, version) = (
            v["enc"].as_str().unwrap(),
            v["ciphertext"].as_str().unwrap(),
            v["version"].as_str().unwrap(),
        );
        let b = open(&keys, pod, enc, ct, version).unwrap();
        assert_eq!(b.kube.len(), 1);
        assert_eq!(b.kube[0].name, "prod");
        assert_eq!(b.ssh[0].user, "git");
        assert_eq!(b.http.len(), 4);
        let erp = &b.http[0];
        assert_eq!(erp.name, "erp");
        assert_eq!(erp.upstream, "https://api.erp.example");
        assert_eq!(erp.path_prefixes, vec!["/odata/", "/api"]);
        assert!(erp.forwarded_host);
        let rl = erp.rate_limit.as_ref().unwrap();
        assert_eq!((rl.per_second, rl.burst), (10, 20));
        match &erp.credential {
            Some(http_binding::Credential::Headers(h)) => {
                assert_eq!(h.headers["X-API-Key"], "erp-key")
            }
            _ => panic!("erp's credential"),
        }
        match &b.http[1].credential {
            Some(http_binding::Credential::Basic(c)) => {
                assert_eq!((c.user.as_str(), c.password.as_str()), ("u", "p"))
            }
            _ => panic!("pay's credential"),
        }
        assert_eq!(b.http[1].ca_pem, "ca-pem");
        match &b.http[2].credential {
            Some(http_binding::Credential::Oauth2(o)) => {
                assert_eq!(o.token_url, "https://login.example/token");
                assert_eq!((o.scope.as_str(), o.audience.as_str()), ("read", "graph"));
                assert!(o.client_auth_in_body);
            }
            _ => panic!("graph's credential"),
        }
        assert_eq!(b.http[3].mode, HttpBindingMode::Intercept as i32);
        match &b.http[3].credential {
            Some(http_binding::Credential::ClientCertificate(c)) => {
                assert_eq!(c.cert_pem, "cert-pem")
            }
            _ => panic!("partner's credential"),
        }
        assert_eq!(b.intercept_cas.len(), 2);
        assert_eq!(b.intercept_cas[1].cert_pem, "new-ca");

        // The version and the pod are authenticated.
        assert!(open(&keys, pod, enc, ct, "v4").is_err());
        assert!(open(&keys, "k2v8d0h3q9y", enc, ct, version).is_err());
        let other = Keys::generate().unwrap();
        assert!(open(&other, pod, enc, ct, version).is_err());
    }

    fn net_audit() -> (Arc<Net>, Arc<Audit>) {
        let f = tempfile::tempfile().unwrap();
        (
            Arc::new(Net::new(Arc::new(SystemResolver))),
            Arc::new(Audit::new(OwnedFd::from(f))),
        )
    }

    fn binding(name: &str, upstream: &str, mode: HttpBindingMode) -> HttpBinding {
        HttpBinding {
            name: name.into(),
            upstream: upstream.into(),
            mode: mode as i32,
            credential: Some(http_binding::Credential::Headers(
                feos_proto::fleet_service::HeaderCredential {
                    headers: [("X-API-Key".to_string(), "k".to_string())].into(),
                },
            )),
            ..Default::default()
        }
    }

    #[tokio::test]
    async fn snapshot_keys_and_refusals() {
        let (net, audit) = net_audit();
        let mut no_cred = binding("nocred", "https://x.example", HttpBindingMode::Name);
        no_cred.credential = None;
        let mut bad_header = binding("badheader", "https://x.example", HttpBindingMode::Name);
        bad_header.credential = Some(http_binding::Credential::Headers(
            feos_proto::fleet_service::HeaderCredential {
                headers: [("X-Key".to_string(), "line\nbreak-secret".to_string())].into(),
            },
        ));
        let set = PodBindings {
            http: vec![
                binding("erp", "https://api.erp.example", HttpBindingMode::Name),
                binding("erp", "https://other.example", HttpBindingMode::Name),
                binding("Bad.Name", "https://x.example", HttpBindingMode::Name),
                binding("plain", "http://x.example", HttpBindingMode::Name),
                binding(
                    "partner",
                    "https://Partner.example",
                    HttpBindingMode::Intercept,
                ),
                binding(
                    "port",
                    "https://port.example:8443",
                    HttpBindingMode::Intercept,
                ),
                no_cred,
                bad_header,
            ],
            ..Default::default()
        };
        let s = snapshot(set, "v1".into(), None, &net, &audit);
        assert!(s.by_name["erp"].live.is_ok());
        assert_eq!(s.by_name["erp"].host, "api.erp.example");
        assert!(!s.by_name.contains_key("Bad.Name"));
        assert!(s.by_name["plain"].live.is_err());
        assert!(s.by_name["nocred"].live.is_err());
        let bad = s.by_name["badheader"].live.as_ref().err().unwrap();
        assert!(!bad.contains("secret"), "{bad}");
        assert!(s.by_host["partner.example"].live.is_ok());
        assert!(s.by_host["port.example"].live.is_err());
        // Intercept bindings without a CA: said so.
        assert!(s.leaf.is_none());
        assert!(s.invalid.iter().any(|(n, _)| n == "intercept CA"));
        assert!(s
            .invalid
            .iter()
            .any(|(n, m)| n == "erp" && m.contains("twice")));
    }
}
