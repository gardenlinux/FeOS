// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A binding's connections to its upstream: dialed under the destination
//! rule ([`crate::dest`]), TLS 1.2 or 1.3 with ALPN `h2` and `http/1.1`,
//! the server checked against the binding's CA or the WebPKI, and pooled
//! per binding by hyper's client. HTTP/1.1 upgrades take a pool of their
//! own that offers `http/1.1` only.

use std::future::Future;
use std::io;
use std::net::SocketAddr;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::Duration;

use http::Uri;
use hyper_util::client::legacy::connect::{Connected, Connection};
use hyper_util::client::legacy::Client;
use hyper_util::rt::{TokioExecutor, TokioIo, TokioTimer};
use rustls::pki_types::pem::PemObject;
use rustls::pki_types::{CertificateDer, PrivateKeyDer, ServerName};
use rustls::{ClientConfig, RootCertStore};
use tokio::net::TcpStream;
use tokio_rustls::client::TlsStream;
use tokio_rustls::TlsConnector;

use crate::body::ProxyBody;
use crate::dest::{ConnectError, ConnectErrorKind, Net};

const TLS_TIMEOUT: Duration = Duration::from_secs(10);
const POOL_IDLE: Duration = Duration::from_secs(90);
const POOL_MAX_IDLE: usize = 16;

pub const ALPN_H2: &[u8] = b"h2";
pub const ALPN_HTTP11: &[u8] = b"http/1.1";

/// The address a response's connection was dialed to, which hyper's client
/// puts into the response's extensions.
#[derive(Clone, Copy, Debug)]
pub struct UpstreamAddr(pub SocketAddr);

pub type HttpClient = Client<Connector, ProxyBody>;

/// rustls with ring, TLS 1.3 and 1.2.
pub fn provider() -> Arc<rustls::crypto::CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

/// The roots a binding's upstream is checked against: its CA's
/// certificates when it names one, the WebPKI's otherwise, or both with
/// `webpki_too`.
pub fn roots(ca_pem: &str, webpki_too: bool) -> Result<RootCertStore, String> {
    let mut roots = RootCertStore::empty();
    if !ca_pem.trim().is_empty() {
        let mut n = 0;
        for c in CertificateDer::pem_slice_iter(ca_pem.as_bytes()) {
            let c = c.map_err(|e| format!("the CA's PEM: {e}"))?;
            roots
                .add(c)
                .map_err(|e| format!("the CA's certificate: {e}"))?;
            n += 1;
        }
        if n == 0 {
            return Err("the CA's PEM holds no certificate".into());
        }
    }
    if ca_pem.trim().is_empty() || webpki_too {
        roots.extend(webpki_roots::TLS_SERVER_ROOTS.iter().cloned());
    }
    Ok(roots)
}

/// A client certificate's chain and key from PEM.
pub fn client_auth(
    cert_pem: &str,
    key_pem: &str,
) -> Result<(Vec<CertificateDer<'static>>, PrivateKeyDer<'static>), String> {
    let chain = CertificateDer::pem_slice_iter(cert_pem.as_bytes())
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| format!("the client certificate's PEM: {e}"))?;
    if chain.is_empty() {
        return Err("the client certificate's PEM holds no certificate".into());
    }
    // The key's PEM error says where, never what.
    let key = PrivateKeyDer::from_pem_slice(key_pem.as_bytes())
        .map_err(|_| "the client certificate's key is no PEM private key".to_string())?;
    Ok((chain, key))
}

pub fn tls_config(
    roots: RootCertStore,
    client: Option<(Vec<CertificateDer<'static>>, PrivateKeyDer<'static>)>,
    alpn: &[&[u8]],
) -> Result<Arc<ClientConfig>, String> {
    let builder = ClientConfig::builder_with_provider(provider())
        .with_protocol_versions(&[&rustls::version::TLS13, &rustls::version::TLS12])
        .map_err(|e| format!("TLS: {e}"))?
        .with_root_certificates(roots);
    let mut config = match client {
        Some((chain, key)) => builder
            .with_client_auth_cert(chain, key)
            .map_err(|e| format!("the client certificate: {e}"))?,
        None => builder.with_no_client_auth(),
    };
    config.alpn_protocols = alpn.iter().map(|p| p.to_vec()).collect();
    Ok(Arc::new(config))
}

/// Dials an upstream for hyper's client: the destination rule, then TLS.
#[derive(Clone)]
pub struct Connector {
    net: Arc<Net>,
    tls: Arc<ClientConfig>,
}

impl Connector {
    pub fn new(net: Arc<Net>, tls: Arc<ClientConfig>) -> Connector {
        Connector { net, tls }
    }
}

/// A host as a URI has it, without an IPv6 literal's brackets.
pub fn bare_host(host: &str) -> &str {
    host.strip_prefix('[')
        .and_then(|h| h.strip_suffix(']'))
        .unwrap_or(host)
}

impl tower_service::Service<Uri> for Connector {
    type Response = Conn;
    type Error = ConnectError;
    type Future = Pin<Box<dyn Future<Output = Result<Conn, ConnectError>> + Send>>;

    fn poll_ready(&mut self, _: &mut Context<'_>) -> Poll<Result<(), ConnectError>> {
        Poll::Ready(Ok(()))
    }

    fn call(&mut self, uri: Uri) -> Self::Future {
        let net = self.net.clone();
        let tls = TlsConnector::from(self.tls.clone());
        Box::pin(async move {
            use ConnectErrorKind::*;
            let host = bare_host(uri.host().unwrap_or_default()).to_string();
            let port = uri.port_u16().unwrap_or(443);
            let name = ServerName::try_from(host.clone())
                .map_err(|_| ConnectError::new(Tls, format!("{host} is no server name")))?;
            let (tcp, addr) = net.dial(&host, port).await?;
            let stream = tokio::time::timeout(TLS_TIMEOUT, tls.connect(name, tcp))
                .await
                .map_err(|_| {
                    ConnectError::new(Tls, format!("TLS with {host} ({addr}): timed out"))
                })?
                .map_err(|e| ConnectError::new(Tls, format!("TLS with {host} ({addr}): {e}")))?;
            let h2 = stream.get_ref().1.alpn_protocol() == Some(ALPN_H2);
            Ok(Conn {
                io: TokioIo::new(stream),
                h2,
                addr,
            })
        })
    }
}

pub struct Conn {
    io: TokioIo<TlsStream<TcpStream>>,
    h2: bool,
    addr: SocketAddr,
}

impl Connection for Conn {
    fn connected(&self) -> Connected {
        let c = Connected::new().extra(UpstreamAddr(self.addr));
        if self.h2 {
            c.negotiated_h2()
        } else {
            c
        }
    }
}

impl hyper::rt::Read for Conn {
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: hyper::rt::ReadBufCursor<'_>,
    ) -> Poll<io::Result<()>> {
        Pin::new(&mut self.get_mut().io).poll_read(cx, buf)
    }
}

impl hyper::rt::Write for Conn {
    fn poll_write(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.get_mut().io).poll_write(cx, buf)
    }

    fn poll_flush(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.get_mut().io).poll_flush(cx)
    }

    fn poll_shutdown(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.get_mut().io).poll_shutdown(cx)
    }

    fn is_write_vectored(&self) -> bool {
        self.io.is_write_vectored()
    }

    fn poll_write_vectored(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        bufs: &[io::IoSlice<'_>],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.get_mut().io).poll_write_vectored(cx, bufs)
    }
}

/// A pooled client over `connector`.
pub fn client(connector: Connector) -> HttpClient {
    Client::builder(TokioExecutor::new())
        .pool_timer(TokioTimer::new())
        .timer(TokioTimer::new())
        .pool_idle_timeout(POOL_IDLE)
        .pool_max_idle_per_host(POOL_MAX_IDLE)
        .build(connector)
}

/// What a failed request through hyper's client was: the connector's
/// error, when it was one, else unreachable.
pub fn connect_error(e: &hyper_util::client::legacy::Error) -> ConnectError {
    let mut source: Option<&(dyn std::error::Error + 'static)> = std::error::Error::source(e);
    while let Some(s) = source {
        if let Some(c) = s.downcast_ref::<ConnectError>() {
            return c.clone();
        }
        source = s.source();
    }
    let what = match std::error::Error::source(e) {
        Some(s) => format!("{e}: {s}"),
        None => e.to_string(),
    };
    ConnectError::new(ConnectErrorKind::Unreachable, what)
}
