// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A workload's handshake with itself over loopback, for a pod with an
//! identity: its server and its client present the workload's
//! certificate, and each CertificateVerify is signed by the pod's signer.
//! The client checks the server by the workload's own ID, since a
//! sandbox's certificate names no DNS name. Prints what each end saw.
//!
//!     cargo build --release --target x86_64-unknown-linux-musl -p feid --example loopback

use std::error::Error;

use rustls::pki_types::ServerName;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

// One thread: a signature blocks only its own handshake.
#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<(), Box<dyn Error>> {
    let feid = feid::Feid::new()?;
    let me = feid.id()?;
    println!("this workload: {me}");

    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let address = listener.local_addr()?;
    let config = feid.server_config()?;
    let serving = feid.clone();
    let server = tokio::spawn(async move {
        let (stream, _) = listener.accept().await?;
        let mut tls = serving.accept(config, stream).await?;
        let caller = feid::peer_id(tls.get_ref().1);
        let mut buf = [0u8; 4];
        tls.read_exact(&mut buf).await?;
        tls.write_all(b"pong").await?;
        tls.shutdown().await?;
        Ok::<_, std::io::Error>(caller)
    });

    let config = feid.client_config_for(&me)?;
    let stream = TcpStream::connect(address).await?;
    let name = ServerName::try_from("loopback.feid.invalid")?;
    let mut tls = feid.connect(config, name, stream).await?;
    let callee = feid::peer_id(tls.get_ref().1);
    let cipher = tls.get_ref().1.negotiated_cipher_suite();
    tls.write_all(b"ping").await?;
    let mut buf = [0u8; 4];
    tls.read_exact(&mut buf).await?;
    let caller = server.await??;

    println!("cipher suite: {:?}", cipher.map(|s| s.suite()));
    println!("the server saw the client as: {}", show(caller));
    println!("the client saw the server as: {}", show(callee));
    println!("answer: {}", String::from_utf8_lossy(&buf));
    // The handshake reports go out in the background.
    tokio::time::sleep(std::time::Duration::from_millis(500)).await;
    println!("reports dropped: {}", feid.dropped_reports());
    Ok(())
}

fn show(id: Option<feid::SpiffeId>) -> String {
    id.map_or_else(|| "no SPIFFE ID".to_string(), |id| id.to_string())
}
