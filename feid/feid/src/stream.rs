// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Handshakes over tokio-rustls that report themselves: [`Feid::connect`]
//! and [`Feid::accept`] run the handshake and, once it is done, queue its
//! report for the signer. A connection made with rustls directly works as
//! well; it is not reported unless the application calls
//! [`Feid::report`].

use std::io;
use std::net::SocketAddr;
use std::sync::Arc;

use rustls::pki_types::ServerName;
use rustls::CommonState;
use tokio::io::{AsyncRead, AsyncWrite};
use tokio_rustls::{client, server, TlsAcceptor, TlsConnector};

use crate::{tls, Feid, HandshakeReport};

/// A stream's two ends, for a report's `tuple`.
pub trait Endpoints {
    fn local(&self) -> Option<SocketAddr>;
    fn remote(&self) -> Option<SocketAddr>;
}

impl Endpoints for tokio::net::TcpStream {
    fn local(&self) -> Option<SocketAddr> {
        self.local_addr().ok()
    }

    fn remote(&self) -> Option<SocketAddr> {
        self.peer_addr().ok()
    }
}

impl Endpoints for tokio::net::UnixStream {
    fn local(&self) -> Option<SocketAddr> {
        None
    }

    fn remote(&self) -> Option<SocketAddr> {
        None
    }
}

impl Endpoints for tokio::io::DuplexStream {
    fn local(&self) -> Option<SocketAddr> {
        None
    }

    fn remote(&self) -> Option<SocketAddr> {
        None
    }
}

fn report(direction: &str, conn: &CommonState, ends: &dyn Endpoints, sni: &str) -> HandshakeReport {
    let tuple = match (ends.local(), ends.remote()) {
        (Some(l), Some(r)) => format!("{l}>{r}"),
        _ => String::new(),
    };
    let mut report = HandshakeReport {
        direction: direction.into(),
        tuple,
        version: "TLS 1.3".into(),
        cipher_suite: conn
            .negotiated_cipher_suite()
            .and_then(|s| s.suite().as_str())
            .unwrap_or_default()
            .into(),
        sni: sni.into(),
        alpn: conn
            .alpn_protocol()
            .map(|p| String::from_utf8_lossy(p).into_owned())
            .unwrap_or_default(),
        ..Default::default()
    };
    tls::describe_peer(conn, &mut report);
    report
}

impl Feid {
    /// A TLS 1.3 client handshake on `stream` with `config`, a
    /// [`Feid::client_config`] or one derived from it, to `server`.
    pub async fn connect<S>(
        &self,
        config: Arc<rustls::ClientConfig>,
        server: ServerName<'static>,
        stream: S,
    ) -> io::Result<client::TlsStream<S>>
    where
        S: AsyncRead + AsyncWrite + Unpin + Endpoints,
    {
        let sni = match &server {
            ServerName::DnsName(n) => n.as_ref().to_string(),
            _ => String::new(),
        };
        let tls = TlsConnector::from(config).connect(server, stream).await?;
        let (io, conn) = tls.get_ref();
        self.report(report("outbound", conn, io, &sni));
        Ok(tls)
    }

    /// A TLS 1.3 server handshake on `stream` with `config`, a
    /// [`Feid::server_config`] or one derived from it. The client's ID is
    /// [`crate::peer_id`] of the connection.
    pub async fn accept<S>(
        &self,
        config: Arc<rustls::ServerConfig>,
        stream: S,
    ) -> io::Result<server::TlsStream<S>>
    where
        S: AsyncRead + AsyncWrite + Unpin + Endpoints,
    {
        let tls = TlsAcceptor::from(config).accept(stream).await?;
        let (io, conn) = tls.get_ref();
        let sni = conn.server_name().unwrap_or_default().to_string();
        self.report(report("inbound", conn, io, &sni));
        Ok(tls)
    }
}
