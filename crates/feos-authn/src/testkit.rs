// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! What the end-to-end tests play: a fake `feosd` on the control socket
//! (hello, the sealed bindings, the pod's connections as descriptors), a
//! test PKI, an HTTPS upstream that tells what it received, and an OAuth
//! token endpoint. Shared by the in-process tests (`src/tests.rs`) and
//! the test of the jailed binary (`tests/e2e.rs`), which includes this
//! file.

#![allow(dead_code)]

use std::collections::HashMap;
use std::convert::Infallible;
use std::io::{IoSlice, IoSliceMut};
use std::net::SocketAddr;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::path::Path;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use base64::Engine;
use bytes::Bytes;
use feos_proto::fleet_service::PodBindings;
use http::{HeaderMap, Request, Response, StatusCode};
use http_body::Frame;
use http_body_util::combinators::BoxBody;
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::service::service_fn;
use hyper_util::rt::{TokioExecutor, TokioIo};
use nix::sys::socket::{
    bind, connect, listen, recvmsg, sendmsg, setsockopt, socket, sockopt, AddressFamily, Backlog,
    ControlMessage, ControlMessageOwned, MsgFlags, SockFlag, SockType, UnixAddr,
};
use prost::Message;
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer, ServerName};
use serde_json::{json, Value};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use x25519_dalek::StaticSecret;

pub const POD_ID: &str = "k2v8d0h3q9x";

/// A CA for the upstreams' server certificates.
pub struct Pki {
    issuer: rcgen::Issuer<'static, rcgen::KeyPair>,
    pub ca_pem: String,
}

impl Pki {
    pub fn new() -> Pki {
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
        let mut p = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        p.distinguished_name
            .push(rcgen::DnType::CommonName, "test upstream CA");
        p.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        p.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let cert = p.self_signed(&key).unwrap();
        let ca_pem = cert.pem();
        Pki {
            issuer: rcgen::Issuer::new(p, key),
            ca_pem,
        }
    }

    /// A server certificate for `names` (DNS names or IP addresses).
    pub fn leaf(&self, names: &[&str]) -> (Vec<CertificateDer<'static>>, PrivateKeyDer<'static>) {
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
        let mut p =
            rcgen::CertificateParams::new(names.iter().map(|n| n.to_string()).collect::<Vec<_>>())
                .unwrap();
        p.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
        let cert = p.signed_by(&key, &self.issuer).unwrap();
        (
            vec![cert.der().clone()],
            PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der())),
        )
    }

    pub fn server_config(&self, names: &[&str]) -> Arc<rustls::ServerConfig> {
        self.server_config_alpn(names, &[b"h2", b"http/1.1"])
    }

    /// A server that speaks `alpn` only.
    pub fn server_config_alpn(&self, names: &[&str], alpn: &[&[u8]]) -> Arc<rustls::ServerConfig> {
        let (chain, key) = self.leaf(names);
        let mut c = rustls::ServerConfig::builder_with_provider(Arc::new(
            rustls::crypto::ring::default_provider(),
        ))
        .with_safe_default_protocol_versions()
        .unwrap()
        .with_no_client_auth()
        .with_single_cert(chain, key)
        .unwrap();
        c.alpn_protocols = alpn.iter().map(|p| p.to_vec()).collect();
        Arc::new(c)
    }
}

/// An interception CA, ECDSA P-256, name-constrained to `names`: cert and
/// key PEM.
pub fn intercept_ca(names: &[&str]) -> (String, String) {
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
    let mut p = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    p.distinguished_name
        .push(rcgen::DnType::CommonName, "FeOS interception CA");
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

/// What an upstream received.
#[derive(Clone, Debug)]
pub struct Seen {
    pub method: String,
    pub uri: String,
    pub version: http::Version,
    pub headers: HeaderMap,
    pub body: Bytes,
}

/// An HTTPS upstream on 127.0.0.1 with h2 and http/1.1. Paths:
/// `/redirect` answers 302 to `https://<its own authority>/next`; `/ws` an
/// upgrade to WebSocket, when it is asked for one, that echoes each text
/// frame, and to "key" answers with the credential it received; `/big` 1
/// MiB; `/protected` 401 unless the bearer token is `tok-2` or later;
/// `/fault` and `/echo-header` repeat the credential they received
/// ([`echoed`]), `/fault` in a 500's JSON body cut in two chunks in the
/// middle of the credential, a header and a trailer, `/echo-header` in a
/// 401's `WWW-Authenticate`, another header and (HTTP/1.1) its reason;
/// `/gzip` answers whatever was asked a gzip body that repeats the
/// credential ([`gzip_body`]), cut in small frames, with a length and an
/// `ETag`; `/br` answers br, `/partial` 206; everything else 200 with
/// "ok".
pub struct Upstream {
    pub addr: SocketAddr,
    pub seen: Arc<Mutex<Vec<Seen>>>,
}

pub const BIG: usize = 1 << 20;

impl Upstream {
    pub async fn start(config: Arc<rustls::ServerConfig>) -> Upstream {
        let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = l.local_addr().unwrap();
        let seen = Arc::new(Mutex::new(Vec::new()));
        let acceptor = tokio_rustls::TlsAcceptor::from(config);
        let s = seen.clone();
        tokio::spawn(async move {
            loop {
                let Ok((tcp, _)) = l.accept().await else {
                    return;
                };
                let (acceptor, s) = (acceptor.clone(), s.clone());
                tokio::spawn(async move {
                    let Ok(tls) = acceptor.accept(tcp).await else {
                        return;
                    };
                    let h2 = tls.get_ref().1.alpn_protocol() == Some(b"h2");
                    let svc = service_fn(move |req| upstream_handler(req, s.clone()));
                    let io = TokioIo::new(tls);
                    if h2 {
                        let _ = hyper::server::conn::http2::Builder::new(TokioExecutor::new())
                            .serve_connection(io, svc)
                            .await;
                    } else {
                        let _ = hyper::server::conn::http1::Builder::new()
                            .serve_connection(io, svc)
                            .with_upgrades()
                            .await;
                    }
                });
            }
        });
        Upstream { addr, seen }
    }

    pub fn last(&self) -> Seen {
        self.seen
            .lock()
            .unwrap()
            .last()
            .cloned()
            .expect("a request")
    }

    pub fn count(&self) -> usize {
        self.seen.lock().unwrap().len()
    }
}

/// The authority a request named: HTTP/2's, or HTTP/1.1's Host.
pub fn authority(s: &Seen) -> String {
    match s.headers.get(http::header::HOST) {
        Some(h) => h.to_str().unwrap().to_string(),
        None => s
            .uri
            .parse::<http::Uri>()
            .unwrap()
            .authority()
            .unwrap()
            .to_string(),
    }
}

/// An upstream's answer body.
pub type TestBody = BoxBody<Bytes, Infallible>;

fn full(b: impl Into<Bytes>) -> TestBody {
    Full::new(b.into()).boxed()
}

/// A body of frames that come with a pause between them.
struct Paced(tokio::sync::mpsc::Receiver<Frame<Bytes>>);

impl http_body::Body for Paced {
    type Data = Bytes;
    type Error = Infallible;

    fn poll_frame(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Result<Frame<Bytes>, Infallible>>> {
        self.get_mut().0.poll_recv(cx).map(|f| f.map(Ok))
    }
}

fn paced(frames: Vec<Frame<Bytes>>) -> TestBody {
    paced_by(frames, Duration::from_millis(30))
}

/// Frames that come one at a time, each in a write of its own.
fn paced_fast(frames: Vec<Frame<Bytes>>) -> TestBody {
    paced_by(frames, Duration::from_millis(1))
}

fn paced_by(frames: Vec<Frame<Bytes>>, pause: Duration) -> TestBody {
    let (tx, rx) = tokio::sync::mpsc::channel(1);
    tokio::spawn(async move {
        for f in frames {
            if tx.send(f).await.is_err() {
                return;
            }
            tokio::time::sleep(pause).await;
        }
    });
    Paced(rx).boxed()
}

/// What `/gzip` packs: a JSON body that repeats `credential` a few times.
pub fn gzip_body(credential: &str) -> String {
    let rows: Vec<String> = (0..20)
        .map(|i| format!(r#"{{"PurchaseOrder":"45000001{i:02}","Note":"key {credential}"}}"#))
        .collect();
    format!(r#"{{"value":[{}]}}"#, rows.join(","))
}

fn gzip(data: &[u8]) -> Vec<u8> {
    use std::io::Write;
    let mut e = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    e.write_all(data).unwrap();
    e.finish().unwrap()
}

/// The credential a request carried, in every form an upstream may repeat
/// it in: `X-API-Key`'s value, or `Authorization`'s, and for Basic the
/// user and password decoded and the password alone, for Bearer the token
/// alone.
pub fn echoed(h: &HeaderMap) -> Vec<String> {
    let get = |n: &str| h.get(n).and_then(|v| v.to_str().ok()).map(String::from);
    let Some(v) = get("x-api-key").or_else(|| get("authorization")) else {
        return Vec::new();
    };
    let mut out = vec![v.clone()];
    if let Some(b) = v.strip_prefix("Basic ") {
        let pair = base64::engine::general_purpose::STANDARD.decode(b).unwrap();
        let pair = String::from_utf8(pair).unwrap();
        let password = pair.split_once(':').unwrap().1.to_string();
        out.push(pair);
        out.push(password);
    }
    if let Some(t) = v.strip_prefix("Bearer ") {
        out.push(t.to_string());
    }
    out
}

/// A WebSocket frame as a client sends it: final, text, masked.
pub fn ws_client_frame(payload: &[u8]) -> Vec<u8> {
    let key = [0x37, 0xfa, 0x21, 0x3d];
    let mut f = vec![0x81, 0x80 | payload.len() as u8];
    assert!(payload.len() < 126);
    f.extend_from_slice(&key);
    f.extend(payload.iter().enumerate().map(|(i, b)| b ^ key[i % 4]));
    f
}

/// A WebSocket frame as a server sends it: final, text, unmasked.
pub fn ws_server_frame(payload: &[u8]) -> Vec<u8> {
    let mut f = vec![0x81];
    if payload.len() < 126 {
        f.push(payload.len() as u8);
    } else {
        f.push(126);
        f.extend_from_slice(&(payload.len() as u16).to_be_bytes());
    }
    f.extend_from_slice(payload);
    f
}

/// One frame's payload, unmasked.
pub async fn ws_read<R: AsyncRead + Unpin>(r: &mut R) -> std::io::Result<Vec<u8>> {
    let mut head = [0u8; 2];
    r.read_exact(&mut head).await?;
    let mut len = u64::from(head[1] & 0x7f);
    if len == 126 {
        let mut l = [0u8; 2];
        r.read_exact(&mut l).await?;
        len = u64::from(u16::from_be_bytes(l));
    } else if len == 127 {
        let mut l = [0u8; 8];
        r.read_exact(&mut l).await?;
        len = u64::from_be_bytes(l);
    }
    let mut key = [0u8; 4];
    if head[1] & 0x80 != 0 {
        r.read_exact(&mut key).await?;
    }
    let mut payload = vec![0u8; len as usize];
    r.read_exact(&mut payload).await?;
    if head[1] & 0x80 != 0 {
        for (i, b) in payload.iter_mut().enumerate() {
            *b ^= key[i % 4];
        }
    }
    Ok(payload)
}

/// The upstream's end of a WebSocket: echoes each frame, and answers "key"
/// with the credential it received.
async fn ws_echo<T: AsyncRead + AsyncWrite + Unpin>(io: T, credential: String) {
    let (mut r, mut w) = tokio::io::split(io);
    while let Ok(p) = ws_read(&mut r).await {
        let reply = match &p[..] {
            b"key" => format!("your key is {credential}").into_bytes(),
            _ => p,
        };
        if w.write_all(&ws_server_frame(&reply)).await.is_err() {
            return;
        }
    }
}

async fn upstream_handler(
    req: Request<Incoming>,
    seen: Arc<Mutex<Vec<Seen>>>,
) -> Result<Response<TestBody>, Infallible> {
    let (mut parts, body) = req.into_parts();
    let path = parts.uri.path().to_string();
    let on_upgrade = parts.extensions.remove::<hyper::upgrade::OnUpgrade>();
    let body = body
        .collect()
        .await
        .map(|b| b.to_bytes())
        .unwrap_or_default();
    let s = Seen {
        method: parts.method.to_string(),
        uri: parts.uri.to_string(),
        version: parts.version,
        headers: parts.headers.clone(),
        body: body.clone(),
    };
    let location_host = authority(&s);
    seen.lock().unwrap().push(s);
    let credential = echoed(&parts.headers);
    let mut resp = Response::new(full(Bytes::from_static(b"ok")));
    match path.as_str() {
        "/redirect" => {
            *resp.status_mut() = StatusCode::FOUND;
            resp.headers_mut().insert(
                http::header::LOCATION,
                format!("https://{location_host}/next?page=2")
                    .parse()
                    .unwrap(),
            );
            resp.headers_mut().insert(
                http::header::CONTENT_LOCATION,
                "https://elsewhere.example/x".parse().unwrap(),
            );
        }
        "/ws" if parts.headers.contains_key(http::header::UPGRADE) => {
            if let Some(up) = on_upgrade {
                let credential = credential.first().cloned().unwrap_or_default();
                tokio::spawn(async move {
                    if let Ok(io) = up.await {
                        ws_echo(TokioIo::new(io), credential).await;
                    }
                });
            }
            *resp.status_mut() = StatusCode::SWITCHING_PROTOCOLS;
            *resp.body_mut() = full(Bytes::new());
            let h = resp.headers_mut();
            h.insert(http::header::CONNECTION, "Upgrade".parse().unwrap());
            h.insert(http::header::UPGRADE, "websocket".parse().unwrap());
        }
        "/big" => *resp.body_mut() = full(vec![b'x'; BIG]),
        "/fault" => {
            let first = credential.first().cloned().unwrap_or_default();
            let body = format!(
                r#"{{"fault":{{"faultstring":"Rate limit quota violation. Quota limit exceeded. Identifier : {}","detail":{{"errorcode":"policies.ratelimit.QuotaViolation"}}}}}}"#,
                credential.join(" / ")
            );
            // Cut in the middle of the credential's first form.
            let at = body.find(&first).unwrap_or(0) + first.len() / 2;
            let body = Bytes::from(body);
            let mut trailers = HeaderMap::new();
            trailers.insert("x-debug", format!("key={first}").parse().unwrap());
            *resp.status_mut() = StatusCode::INTERNAL_SERVER_ERROR;
            resp.headers_mut()
                .insert("content-type", "application/json".parse().unwrap());
            resp.headers_mut()
                .insert("x-quota-key", first.parse().unwrap());
            *resp.body_mut() = paced(vec![
                Frame::data(body.slice(..at)),
                Frame::data(body.slice(at..)),
                Frame::trailers(trailers),
            ]);
        }
        "/echo-header" => {
            let first = credential.first().cloned().unwrap_or_default();
            *resp.status_mut() = StatusCode::UNAUTHORIZED;
            let h = resp.headers_mut();
            h.insert(
                http::header::WWW_AUTHENTICATE,
                format!(r#"Bearer error="invalid_token", error_description="{first} is refused""#)
                    .parse()
                    .unwrap(),
            );
            h.insert("x-echo", first.parse().unwrap());
            resp.extensions_mut()
                .insert(hyper::ext::ReasonPhrase::try_from(format!("Refused {first}")).unwrap());
        }
        "/gzip" => {
            let first = credential.first().cloned().unwrap_or_default();
            let z = Bytes::from(gzip(gzip_body(&first).as_bytes()));
            let h = resp.headers_mut();
            h.insert(http::header::CONTENT_ENCODING, "gzip".parse().unwrap());
            h.insert(http::header::CONTENT_LENGTH, z.len().into());
            h.insert(http::header::ETAG, "\"v1\"".parse().unwrap());
            let frames = (0..z.len())
                .step_by(16)
                .map(|i| Frame::data(z.slice(i..(i + 16).min(z.len()))))
                .collect();
            *resp.body_mut() = paced_fast(frames);
        }
        "/br" => {
            resp.headers_mut()
                .insert(http::header::CONTENT_ENCODING, "br".parse().unwrap());
            *resp.body_mut() = full(vec![0x0b, 0x01, 0x80, 0x6f, 0x6b, 0x03]);
        }
        "/partial" => {
            *resp.status_mut() = StatusCode::PARTIAL_CONTENT;
            resp.headers_mut()
                .insert(http::header::CONTENT_RANGE, "bytes 0-1/10".parse().unwrap());
        }
        "/protected" => {
            let ok = parts
                .headers
                .get(http::header::AUTHORIZATION)
                .and_then(|v| v.to_str().ok())
                .and_then(|v| v.strip_prefix("Bearer tok-"))
                .and_then(|n| n.parse::<u64>().ok())
                .is_some_and(|n| n >= 2);
            if !ok {
                *resp.status_mut() = StatusCode::UNAUTHORIZED;
            }
        }
        _ => {
            resp.headers_mut()
                .insert("alt-svc", "h3=\":443\"".parse().unwrap());
        }
    }
    Ok(resp)
}

/// A token request: its form and its Authorization.
pub type TokenRequest = (String, Option<String>);

/// An OAuth 2.0 token endpoint over HTTPS at `/token`: tokens `tok-1`,
/// `tok-2`, … (or of another prefix) with the given lifetime; it records
/// each request's form and Authorization, and answers after `delay_ms`;
/// with `error` set, 400 with that OAuth error.
pub struct TokenServer {
    pub addr: SocketAddr,
    pub fetches: Arc<AtomicU64>,
    pub requests: Arc<Mutex<Vec<TokenRequest>>>,
    pub delay_ms: Arc<AtomicU64>,
    pub error: Arc<Mutex<Option<String>>>,
}

impl TokenServer {
    pub async fn start(config: Arc<rustls::ServerConfig>, expires_in: u64) -> TokenServer {
        TokenServer::start_with(config, expires_in, "tok-").await
    }

    pub async fn start_with(
        config: Arc<rustls::ServerConfig>,
        expires_in: u64,
        prefix: &str,
    ) -> TokenServer {
        let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = l.local_addr().unwrap();
        let fetches = Arc::new(AtomicU64::new(0));
        let requests = Arc::new(Mutex::new(Vec::new()));
        let delay_ms = Arc::new(AtomicU64::new(0));
        let error = Arc::new(Mutex::new(None::<String>));
        let prefix = prefix.to_string();
        let acceptor = tokio_rustls::TlsAcceptor::from(config);
        let (f, r, d, e) = (
            fetches.clone(),
            requests.clone(),
            delay_ms.clone(),
            error.clone(),
        );
        tokio::spawn(async move {
            loop {
                let Ok((tcp, _)) = l.accept().await else {
                    return;
                };
                let (acceptor, f, r, d, e, prefix) = (
                    acceptor.clone(),
                    f.clone(),
                    r.clone(),
                    d.clone(),
                    e.clone(),
                    prefix.clone(),
                );
                tokio::spawn(async move {
                    let Ok(tls) = acceptor.accept(tcp).await else {
                        return;
                    };
                    let h2 = tls.get_ref().1.alpn_protocol() == Some(b"h2");
                    let svc = service_fn(move |req: Request<Incoming>| {
                        let (f, r, d, e) = (f.clone(), r.clone(), d.clone(), e.clone());
                        let prefix = prefix.clone();
                        async move {
                            let delay = d.load(Ordering::SeqCst);
                            tokio::time::sleep(Duration::from_millis(delay)).await;
                            let auth = req
                                .headers()
                                .get(http::header::AUTHORIZATION)
                                .map(|v| v.to_str().unwrap().to_string());
                            let body = req.into_body().collect().await.unwrap().to_bytes();
                            r.lock()
                                .unwrap()
                                .push((String::from_utf8_lossy(&body).to_string(), auth));
                            if let Some(error) = e.lock().unwrap().clone() {
                                let mut resp = Response::new(Full::new(Bytes::from(
                                    json!({"error": error}).to_string(),
                                )));
                                *resp.status_mut() = StatusCode::BAD_REQUEST;
                                return Ok::<_, Infallible>(resp);
                            }
                            let n = f.fetch_add(1, Ordering::SeqCst) + 1;
                            let token = json!({
                                "access_token": format!("{prefix}{n}"),
                                "token_type": "bearer",
                                "expires_in": expires_in,
                            });
                            Ok::<_, Infallible>(Response::new(Full::new(Bytes::from(
                                token.to_string(),
                            ))))
                        }
                    });
                    let io = TokioIo::new(tls);
                    if h2 {
                        let _ = hyper::server::conn::http2::Builder::new(TokioExecutor::new())
                            .serve_connection(io, svc)
                            .await;
                    } else {
                        let _ = hyper::server::conn::http1::Builder::new()
                            .serve_connection(io, svc)
                            .await;
                    }
                });
            }
        });
        TokenServer {
            addr,
            fetches,
            requests,
            delay_ms,
            error,
        }
    }
}

/// feosd's side of the control socket.
pub fn seqpacket_listener(path: &Path) -> OwnedFd {
    let fd = socket(
        AddressFamily::Unix,
        SockType::SeqPacket,
        SockFlag::SOCK_CLOEXEC,
        None,
    )
    .unwrap();
    bind(fd.as_raw_fd(), &UnixAddr::new(path).unwrap()).unwrap();
    listen(&fd, Backlog::new(8).unwrap()).unwrap();
    fd
}

pub struct Feosd {
    pub fd: OwnedFd,
}

impl Feosd {
    /// Connects to the control socket at `path`.
    pub fn connect(path: &Path) -> Feosd {
        let fd = socket(
            AddressFamily::Unix,
            SockType::SeqPacket,
            SockFlag::SOCK_CLOEXEC,
            None,
        )
        .unwrap();
        setsockopt(
            &fd,
            sockopt::ReceiveTimeout,
            &nix::sys::time::TimeVal::new(15, 0),
        )
        .unwrap();
        let addr = UnixAddr::new(path).unwrap();
        for _ in 0..200 {
            if connect(fd.as_raw_fd(), &addr).is_ok() {
                return Feosd { fd };
            }
            std::thread::sleep(Duration::from_millis(25));
        }
        panic!("connect to {}", path.display());
    }

    pub fn send(&self, msg: &Value, fds: &[RawFd]) {
        let raw = msg.to_string();
        let iov = [IoSlice::new(raw.as_bytes())];
        let cmsg = [ControlMessage::ScmRights(fds)];
        let cmsgs: &[ControlMessage] = if fds.is_empty() { &[] } else { &cmsg };
        sendmsg::<()>(self.fd.as_raw_fd(), &iov, cmsgs, MsgFlags::empty(), None).unwrap();
    }

    /// The next message, within 15 seconds.
    pub fn recv(&self) -> Value {
        let mut buf = vec![0u8; 1 << 20];
        let mut iov = [IoSliceMut::new(&mut buf)];
        let mut cmsg = nix::cmsg_space!([RawFd; 4]);
        let msg = recvmsg::<()>(
            self.fd.as_raw_fd(),
            &mut iov,
            Some(&mut cmsg),
            MsgFlags::empty(),
        )
        .expect("a message from feos-authn");
        for c in msg.cmsgs().unwrap() {
            if let ControlMessageOwned::ScmRights(_) = c {
                panic!("feos-authn sent descriptors");
            }
        }
        let n = msg.bytes;
        assert!(n > 0, "feos-authn closed the control connection");
        serde_json::from_slice(&buf[..n]).unwrap()
    }

    /// Whether feos-authn closed the connection: the next read is its end.
    pub fn closed(&self) -> bool {
        let mut buf = [0u8; 64];
        let mut iov = [IoSliceMut::new(&mut buf)];
        matches!(
            recvmsg::<()>(self.fd.as_raw_fd(), &mut iov, None, MsgFlags::empty()),
            Ok(m) if m.bytes == 0
        )
    }

    pub fn hello(&self, deny: &[&str], source_v4: Option<&str>) {
        self.send(
            &json!({"v": 1, "op": "hello", "podId": POD_ID, "denyPrefixes": deny,
                "sourceV4": source_v4, "sourceV6": null}),
            &[],
        );
    }

    /// Answers a credentials request with `set` sealed to its key, as the
    /// control plane does; returns the request.
    pub fn answer(&self, set: &PodBindings, version: &str) -> Value {
        self.answer_for(set, version, POD_ID)
    }

    /// As [`Feosd::answer`], sealed for `pod`.
    pub fn answer_for(&self, set: &PodBindings, version: &str, pod: &str) -> Value {
        let req = self.recv();
        assert_eq!(req["op"], "credentials", "{req}");
        assert_eq!(req["v"], 1);
        let b64 = base64::engine::general_purpose::STANDARD;
        let pk: [u8; 32] = b64
            .decode(req["hpkePublicKey"].as_str().unwrap())
            .unwrap()
            .try_into()
            .unwrap();
        assert!(!b64.decode(req["csr"].as_str().unwrap()).unwrap().is_empty());
        let mut eph = [0u8; 32];
        ring::rand::SecureRandom::fill(&ring::rand::SystemRandom::new(), &mut eph).unwrap();
        let (enc, ct) = feos_authn::hpke::seal(
            pk,
            &StaticSecret::from(eph),
            feos_authn::hpke::BINDINGS_INFO,
            &feos_authn::hpke::bindings_aad(pod, version),
            &set.encode_to_vec(),
        )
        .unwrap();
        self.send(
            &json!({"v": 1, "op": "credentials", "certificate": null,
                "bindings": {"enc": b64.encode(enc), "ciphertext": b64.encode(ct),
                    "version": version}}),
            &[],
        );
        req
    }

    /// Expects the `ready` of `version`.
    pub fn ready(&self, version: &str) {
        let m = self.recv();
        assert_eq!(m, json!({"v": 1, "op": "ready", "version": version}));
    }
}

/// The node's address for the tests: connections of "the pod" to it are
/// accepted here and passed to feos-authn, as feosd does.
pub struct Node {
    pub listener: TcpListener,
}

impl Node {
    pub async fn new() -> Node {
        Node {
            listener: TcpListener::bind("127.0.0.1:0").await.unwrap(),
        }
    }

    /// A connection of the pod's to `port`, passed to feos-authn.
    pub async fn connect(&self, feosd: &Feosd, port: u16) -> TcpStream {
        let client = TcpStream::connect(self.listener.local_addr().unwrap())
            .await
            .unwrap();
        let (server, peer) = self.listener.accept().await.unwrap();
        let fd: OwnedFd = server.into_std().unwrap().into();
        feosd.send(
            &json!({"v": 1, "op": "conn", "port": port, "peer": peer.to_string()}),
            &[fd.as_raw_fd()],
        );
        drop(fd);
        client
    }
}

/// An HTTP/1.1 client on a pod's connection.
pub async fn h1(stream: TcpStream) -> hyper::client::conn::http1::SendRequest<Full<Bytes>> {
    let (send, conn) = hyper::client::conn::http1::handshake(TokioIo::new(stream))
        .await
        .unwrap();
    tokio::spawn(async move {
        let _ = conn.with_upgrades().await;
    });
    send
}

/// Sends on an HTTP/1.1 client once its connection is ready again.
pub async fn send1(
    s: &mut hyper::client::conn::http1::SendRequest<Full<Bytes>>,
    req: Request<Full<Bytes>>,
) -> Response<Incoming> {
    s.ready().await.unwrap();
    s.send_request(req).await.unwrap()
}

/// A request's answer: status, headers and the whole body.
pub async fn read(resp: Response<Incoming>) -> (StatusCode, HeaderMap, Bytes) {
    let (parts, body) = resp.into_parts();
    let body = body.collect().await.unwrap().to_bytes();
    (parts.status, parts.headers, body)
}

pub fn get(host: &str, path: &str) -> Request<Full<Bytes>> {
    Request::get(path)
        .header(http::header::HOST, host)
        .body(Full::new(Bytes::new()))
        .unwrap()
}

/// A TLS client config that trusts the PEM CA, with ALPN.
pub fn tls_client(ca_pem: &str, alpn: &[&[u8]]) -> Arc<rustls::ClientConfig> {
    let mut roots = rustls::RootCertStore::empty();
    for c in rustls::pki_types::pem::PemObject::pem_slice_iter(ca_pem.as_bytes()) {
        roots.add(c.unwrap()).unwrap();
    }
    let mut c = rustls::ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .unwrap()
    .with_root_certificates(roots)
    .with_no_client_auth();
    c.alpn_protocols = alpn.iter().map(|p| p.to_vec()).collect();
    Arc::new(c)
}

pub async fn tls_connect(
    stream: TcpStream,
    config: Arc<rustls::ClientConfig>,
    sni: &str,
) -> std::io::Result<tokio_rustls::client::TlsStream<TcpStream>> {
    tokio_rustls::TlsConnector::from(config)
        .connect(ServerName::try_from(sni.to_string()).unwrap(), stream)
        .await
}

/// The audit file's records.
pub fn records(path: &Path) -> Vec<Value> {
    std::fs::read_to_string(path)
        .unwrap_or_default()
        .lines()
        .map(|l| serde_json::from_str(l).unwrap())
        .collect()
}

/// Waits until a record matches.
pub async fn await_record(path: &Path, f: impl Fn(&Value) -> bool) -> Value {
    for _ in 0..200 {
        if let Some(r) = records(path).into_iter().find(&f) {
            return r;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    panic!("no such record in {:#?}", records(path));
}

/// A resolver of fixed answers, (host, port) to addresses.
#[derive(Default)]
pub struct MapResolver(pub HashMap<(String, u16), Vec<SocketAddr>>);

impl feos_authn::dest::Resolve for MapResolver {
    fn resolve(&self, host: &str, port: u16) -> feos_authn::dest::ResolveFuture {
        let r = self
            .0
            .get(&(host.to_string(), port))
            .cloned()
            .ok_or_else(|| std::io::Error::other(format!("no test address for {host}:{port}")));
        Box::pin(async move { r })
    }
}

/// Takes the descriptor a test made, for a process that owns it.
pub fn owned(fd: RawFd) -> OwnedFd {
    // SAFETY: the caller hands over a descriptor nothing else owns.
    unsafe { OwnedFd::from_raw_fd(fd) }
}
