// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The signer's side of one connection: [`serve`] reads the hello,
//! negotiates, and then answers one request at a time through a
//! [`Handler`]. The caller has attributed the stream before: a handler
//! serves one workload, and no frame can name another.
//!
//! What the server does itself, so that every signer does it the same:
//!
//! - the framing, the cap, and closing after a frame too large;
//! - the version: no overlap, or a later frame of another version, is
//!   `unsupported_version` and the end of the connection;
//! - `sign`'s input: `hash` must be `none`, and the message must pass
//!   [`cv::check`]. The handler sees only inputs that did, and hears of
//!   the refusals through [`Handler::sign_refused`], for its audit;
//! - masking: a [`Refusal::internal`] reaches the client as `internal
//!   error`, its detail only the log;
//! - one-way ops are never answered, so that a client's next answer is
//!   the one it waits for.

use std::future::Future;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tokio::io::{AsyncRead, AsyncWrite, AsyncWriteExt, BufReader};

use crate::cv::{self, CertificateVerify};
use crate::error::{code, Refusal};
use crate::frame::{self, ReadError};
use crate::{negotiate, op, Envelope, HandshakeReport, Out, HASH_NONE, MAX_FRAME, VERSION};

/// One workload's signer, behind one connection.
pub trait Handler: Send + Sync {
    /// The workload's certificate chain, PEM, its own certificate first.
    fn certificate(&self) -> impl Future<Output = Result<String, Refusal>> + Send;
    /// The trust bundle, PEM.
    fn trust_bundle(&self) -> impl Future<Output = Result<String, Refusal>> + Send;
    /// The signature of an input that passed [`cv::check`]. A handler
    /// that audits records it before it answers, and refuses with
    /// [`code::UNAVAILABLE_AUDIT_LOG`] when it cannot.
    fn sign(
        &self,
        cv: CertificateVerify<'_>,
    ) -> impl Future<Output = Result<Vec<u8>, Refusal>> + Send;
    /// A `sign` the server refused before the handler saw it.
    fn sign_refused(&self, refusal: &Refusal) {
        let _ = refusal;
    }
    /// A library's report of a handshake. Never answered.
    fn handshake_report(&self, report: HandshakeReport);
}

/// How [`serve`] runs.
#[derive(Clone, Debug)]
pub struct ServeOptions {
    /// The server's name in its hello_ack, for the client's log.
    pub name: String,
    /// How long the hello may take to arrive.
    pub hello_timeout: Duration,
    /// The largest frame read; [`MAX_FRAME`] by default.
    pub max_frame: usize,
}

impl ServeOptions {
    pub fn named(name: impl Into<String>) -> ServeOptions {
        ServeOptions {
            name: name.into(),
            hello_timeout: Duration::from_secs(10),
            max_frame: MAX_FRAME,
        }
    }
}

#[derive(Deserialize)]
struct Hello {
    #[serde(default)]
    min: u32,
    #[serde(default)]
    max: u32,
    #[serde(default)]
    client: String,
}

#[derive(Serialize)]
struct HelloAck<'a> {
    proto: u32,
    #[serde(skip_serializing_if = "str::is_empty")]
    server: &'a str,
}

#[derive(Deserialize)]
struct Sign {
    hash: String,
    #[serde(with = "crate::b64")]
    message: Vec<u8>,
}

#[derive(Deserialize)]
struct Report {
    report: HandshakeReport,
}

#[derive(Serialize)]
struct ErrorPayload<'a> {
    in_reply_to: &'a str,
    code: &'a str,
    message: &'a str,
    retriable: bool,
}

#[derive(Serialize)]
struct Chain {
    chain_pem: String,
}

#[derive(Serialize)]
struct Bundle {
    ca_pem: String,
}

#[derive(Serialize)]
struct Signature {
    #[serde(with = "crate::b64")]
    signature: Vec<u8>,
}

/// Writes frames; false once the stream cannot be written.
struct Writer<W> {
    w: W,
}

impl<W: AsyncWrite + Unpin> Writer<W> {
    async fn frame<P: Serialize>(&mut self, v: u32, op: &str, id: Option<u32>, payload: P) -> bool {
        let Some(bytes) = frame::encode(&Out { v, op, id, payload }) else {
            log::error!("feid: an answer to be sent as {op} is larger than a frame");
            return false;
        };
        self.w.write_all(&bytes).await.is_ok() && self.w.flush().await.is_ok()
    }

    async fn error(&mut self, in_reply_to: &str, id: Option<u32>, refusal: &Refusal) -> bool {
        let payload = ErrorPayload {
            in_reply_to,
            code: &refusal.code,
            message: &refusal.message,
            retriable: refusal.retriable,
        };
        self.frame(VERSION, op::ERROR, id, payload).await
    }

    async fn fail(
        &mut self,
        in_reply_to: &str,
        id: Option<u32>,
        code: &str,
        message: &str,
    ) -> bool {
        self.error(in_reply_to, id, &Refusal::new(code, message, false))
            .await
    }
}

/// Serves one connection until the client closes it or breaks the
/// protocol.
pub async fn serve<S, H>(stream: S, handler: &H, options: &ServeOptions)
where
    S: AsyncRead + AsyncWrite + Unpin + Send,
    H: Handler,
{
    let (r, w) = tokio::io::split(stream);
    let mut r = BufReader::new(r);
    let mut w = Writer { w };
    let max = options.max_frame;

    let raw = match tokio::time::timeout(options.hello_timeout, frame::read(&mut r, max)).await {
        Err(_) => {
            log::debug!("feid: no hello within {:?}", options.hello_timeout);
            return;
        }
        Ok(Err(ReadError::TooLarge)) => {
            w.fail("", None, code::BAD_FRAME, "frame exceeds max size")
                .await;
            return;
        }
        Ok(Err(_)) => return,
        Ok(Ok(raw)) => raw,
    };
    let envelope = match Envelope::parse(&raw) {
        Ok(e) => e,
        Err(m) => {
            w.fail("", None, code::BAD_FRAME, &m).await;
            return;
        }
    };
    if envelope.op != op::HELLO {
        w.fail(
            "",
            envelope.id,
            code::BAD_REQUEST,
            "first frame must be hello",
        )
        .await;
        return;
    }
    let hello: Hello = match serde_json::from_slice(&raw) {
        Ok(h) => h,
        Err(e) => {
            let m = format!("hello: {e}");
            w.fail("", envelope.id, code::BAD_FRAME, &m).await;
            return;
        }
    };
    let Some(version) = negotiate(hello.min, hello.max, VERSION, VERSION) else {
        let m = format!(
            "client supports [{},{}], server only v{VERSION}",
            hello.min, hello.max
        );
        w.fail(op::HELLO, envelope.id, code::UNSUPPORTED_VERSION, &m)
            .await;
        return;
    };
    log::debug!("feid: hello from {:?}, v{version}", hello.client);
    let ack = HelloAck {
        proto: version,
        server: &options.name,
    };
    if !w.frame(version, op::HELLO_ACK, envelope.id, ack).await {
        return;
    }

    loop {
        let raw = match frame::read(&mut r, max).await {
            Ok(raw) => raw,
            Err(ReadError::TooLarge) => {
                w.fail("", None, code::BAD_FRAME, "frame exceeds max size")
                    .await;
                return;
            }
            Err(ReadError::Closed) => return,
            Err(ReadError::Truncated) => {
                log::debug!("feid: the client closed the connection inside a frame");
                return;
            }
            Err(ReadError::Io(e)) => {
                log::debug!("feid: reading a frame: {e}");
                return;
            }
        };
        if !dispatch(&mut w, handler, version, &raw).await {
            return;
        }
    }
}

/// One frame after the hello; false ends the connection.
async fn dispatch<W, H>(w: &mut Writer<W>, handler: &H, version: u32, raw: &[u8]) -> bool
where
    W: AsyncWrite + Unpin,
    H: Handler,
{
    let envelope = match Envelope::parse(raw) {
        Ok(e) => e,
        // The frame was read whole: the stream is still in step.
        Err(m) => return w.fail("", None, code::BAD_FRAME, &m).await,
    };
    let (name, id) = (envelope.op.as_str(), envelope.id);
    if envelope.v != u64::from(version) {
        let m = format!("frame version {}, negotiated {version}", envelope.v);
        w.fail(name, id, code::UNSUPPORTED_VERSION, &m).await;
        return false;
    }
    let refuse = |refusal: &Refusal| {
        if let Some(detail) = &refusal.detail {
            log::warn!("feid: {name}: {detail}");
        }
    };
    match name {
        op::GET_CERTIFICATE => match handler.certificate().await {
            Ok(chain_pem) => {
                w.frame(version, op::CERTIFICATE, id, Chain { chain_pem })
                    .await
            }
            Err(r) => {
                refuse(&r);
                w.error(name, id, &r).await
            }
        },
        op::GET_CA => match handler.trust_bundle().await {
            Ok(ca_pem) => w.frame(version, op::CA_BUNDLE, id, Bundle { ca_pem }).await,
            Err(r) => {
                refuse(&r);
                w.error(name, id, &r).await
            }
        },
        op::SIGN => {
            let request: Sign = match serde_json::from_slice(raw) {
                Ok(s) => s,
                Err(e) => {
                    let r = Refusal::new(code::BAD_REQUEST, format!("sign: {e}"), false);
                    handler.sign_refused(&r);
                    return w.error(name, id, &r).await;
                }
            };
            if request.hash != HASH_NONE {
                let r = Refusal::new(
                    code::REFUSED_UNSUPPORTED_HASH,
                    format!(
                        "hash {:?} is not accepted: v1 signs unhashed TLS 1.3 inputs only",
                        request.hash
                    ),
                    false,
                );
                handler.sign_refused(&r);
                return w.error(name, id, &r).await;
            }
            let cv = match cv::check(&request.message) {
                Ok(cv) => cv,
                Err(r) => {
                    handler.sign_refused(&r);
                    return w.error(name, id, &r).await;
                }
            };
            match handler.sign(cv).await {
                Ok(signature) => {
                    w.frame(version, op::SIGNATURE, id, Signature { signature })
                        .await
                }
                Err(r) => {
                    refuse(&r);
                    w.error(name, id, &r).await
                }
            }
        }
        op::HANDSHAKE_REPORT => {
            match serde_json::from_slice::<Report>(raw) {
                Ok(r) => handler.handshake_report(r.report),
                Err(e) => log::debug!("feid: a handshake_report that does not parse: {e}"),
            }
            true
        }
        op::WIRE_EVENT => {
            log::debug!("feid: a wire_event, which this signer does not take");
            true
        }
        other => {
            let m = format!("unknown op {other:?}");
            w.fail(other, id, code::UNSUPPORTED_OP, &m).await
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::cv::{input, Role};
    use crate::{Client, Error};
    use std::collections::VecDeque;
    use std::io::{BufRead, Write};
    use std::os::unix::net::UnixStream as StdStream;
    use std::sync::{Arc, Mutex};

    #[derive(Default)]
    struct Fake {
        /// Answers for `sign`, one per call; a signature of the input's
        /// role when none is queued.
        refusals: Mutex<VecDeque<Refusal>>,
        signed: Mutex<Vec<(Role, usize)>>,
        refused: Mutex<Vec<Refusal>>,
        reports: Mutex<Vec<HandshakeReport>>,
    }

    impl Handler for Fake {
        async fn certificate(&self) -> Result<String, Refusal> {
            Ok("-----BEGIN CERTIFICATE-----\nleaf\n-----END CERTIFICATE-----\n".into())
        }

        async fn trust_bundle(&self) -> Result<String, Refusal> {
            Ok("bundle".into())
        }

        async fn sign(&self, cv: CertificateVerify<'_>) -> Result<Vec<u8>, Refusal> {
            if let Some(r) = self.refusals.lock().unwrap().pop_front() {
                return Err(r);
            }
            self.signed
                .lock()
                .unwrap()
                .push((cv.role, cv.transcript_hash.len()));
            Ok(format!("signed by the {}", cv.role).into_bytes())
        }

        fn sign_refused(&self, refusal: &Refusal) {
            self.refused.lock().unwrap().push(refusal.clone());
        }

        fn handshake_report(&self, report: HandshakeReport) {
            self.reports.lock().unwrap().push(report);
        }
    }

    /// A server on one end of a socket pair; the other end, blocking.
    fn pair(fake: Arc<Fake>) -> StdStream {
        let (ours, theirs) = StdStream::pair().unwrap();
        theirs.set_nonblocking(true).unwrap();
        let theirs = tokio::net::UnixStream::from_std(theirs).unwrap();
        tokio::spawn(async move {
            serve(theirs, fake.as_ref(), &ServeOptions::named("test-signer/1")).await;
        });
        ours.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        ours
    }

    /// Writes raw lines and reads one line back per expected answer.
    struct Raw {
        w: StdStream,
        r: std::io::BufReader<StdStream>,
    }

    impl Raw {
        fn new(stream: StdStream) -> Raw {
            Raw {
                r: std::io::BufReader::new(stream.try_clone().unwrap()),
                w: stream,
            }
        }

        fn send(&mut self, line: &[u8]) {
            self.w.write_all(line).unwrap();
        }

        fn line(&mut self) -> String {
            let mut s = String::new();
            self.r.read_line(&mut s).unwrap();
            s
        }

        fn closed(&mut self) -> bool {
            let mut s = String::new();
            matches!(self.r.read_line(&mut s), Ok(0))
        }
    }

    async fn blocking<T: Send + 'static>(f: impl FnOnce() -> T + Send + 'static) -> T {
        tokio::task::spawn_blocking(f).await.unwrap()
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn every_op_round_trips() {
        let fake = Arc::new(Fake::default());
        let stream = pair(fake.clone());
        blocking(move || {
            let mut c = Client::connect(stream, "feid-test/1").unwrap();
            assert_eq!((c.version(), c.server()), (1, "test-signer/1"));
            assert!(c.certificate().unwrap().contains("leaf"));
            assert_eq!(c.trust_bundle().unwrap(), "bundle");
            let report = HandshakeReport {
                direction: "outbound".into(),
                peer_spiffe: "spiffe://example.org/db".into(),
                ..Default::default()
            };
            c.report(&report).unwrap();
            // The report has no answer: the next answer is the signature.
            let sig = c.sign(&input(Role::Client, &[7; 32])).unwrap();
            assert_eq!(sig, b"signed by the client");
            let sig = c.sign(&input(Role::Server, &[7; 48])).unwrap();
            assert_eq!(sig, b"signed by the server");
        })
        .await;
        assert_eq!(
            *fake.signed.lock().unwrap(),
            vec![(Role::Client, 32), (Role::Server, 48)]
        );
        assert_eq!(
            fake.reports.lock().unwrap()[0].peer_spiffe,
            "spiffe://example.org/db"
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn the_server_refuses_what_is_no_certificate_verify_and_tells_the_handler() {
        let fake = Arc::new(Fake::default());
        let stream = pair(fake.clone());
        blocking(move || {
            let mut c = Client::connect(stream, "").unwrap();
            let e = c.sign(b"please sign this contract").unwrap_err();
            assert_eq!(e.code(), Some(code::REFUSED_NOT_CERTIFICATE_VERIFY));
            assert!(e.is_refusal());
            // The connection stays usable.
            assert_eq!(c.trust_bundle().unwrap(), "bundle");
        })
        .await;
        assert!(fake.signed.lock().unwrap().is_empty());
        let refused = fake.refused.lock().unwrap();
        assert_eq!(refused.len(), 1);
        assert_eq!(refused[0].code, code::REFUSED_NOT_CERTIFICATE_VERIFY);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_hashed_input_is_refused() {
        let fake = Arc::new(Fake::default());
        let mut raw = Raw::new(pair(fake.clone()));
        let message = base64::Engine::encode(
            &base64::engine::general_purpose::STANDARD,
            input(Role::Client, &[1; 32]),
        );
        blocking(move || {
            raw.send(b"{\"v\":1,\"op\":\"hello\",\"min\":1,\"max\":1}\n");
            raw.line();
            raw.send(
                format!(
                    "{{\"v\":1,\"op\":\"sign\",\"hash\":\"sha256\",\"message\":\"{message}\"}}\n"
                )
                .as_bytes(),
            );
            let answer = raw.line();
            assert!(
                answer.contains(r#""code":"refused.unsupported_hash""#),
                "{answer}"
            );
        })
        .await;
        assert_eq!(
            fake.refused.lock().unwrap()[0].code,
            code::REFUSED_UNSUPPORTED_HASH
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_retriable_refusal_stays_retriable_and_internal_detail_stays_home() {
        let fake = Arc::new(Fake::default());
        fake.refusals.lock().unwrap().extend([
            Refusal::new(
                code::UNAVAILABLE_AUDIT_LOG,
                "the audit file cannot be written",
                true,
            ),
            Refusal::internal("open /secret/path: permission denied"),
        ]);
        let stream = pair(fake);
        blocking(move || {
            let mut c = Client::connect(stream, "").unwrap();
            let e = c.sign(&input(Role::Client, &[1; 32])).unwrap_err();
            assert!(e.is_retriable_refusal());
            assert_eq!(e.code(), Some(code::UNAVAILABLE_AUDIT_LOG));
            let e = c.sign(&input(Role::Client, &[1; 32])).unwrap_err();
            assert_eq!(e.to_string(), "feid: internal: internal error");
            assert!(!e.to_string().contains("secret"));
        })
        .await;
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn golden_answer_frames() {
        let fake = Arc::new(Fake::default());
        let mut raw = Raw::new(pair(fake));
        blocking(move || {
            raw.send(b"{\"v\":1,\"op\":\"hello\",\"min\":1,\"max\":1,\"client\":\"x\",\"id\":3}\n");
            assert_eq!(
                raw.line(),
                "{\"v\":1,\"op\":\"hello_ack\",\"id\":3,\"proto\":1,\"server\":\"test-signer/1\"}\n"
            );
            raw.send(b"{\"v\":1,\"op\":\"get_ca\"}\n");
            assert_eq!(raw.line(), "{\"v\":1,\"op\":\"ca_bundle\",\"ca_pem\":\"bundle\"}\n");
            raw.send(b"{\"v\":1,\"op\":\"get_certificate\",\"id\":9}\n");
            assert_eq!(
                raw.line(),
                "{\"v\":1,\"op\":\"certificate\",\"id\":9,\"chain_pem\":\"-----BEGIN CERTIFICATE-----\\nleaf\\n-----END CERTIFICATE-----\\n\"}\n"
            );
            raw.send(b"{\"v\":1,\"op\":\"delete_everything\",\"id\":4}\n");
            assert_eq!(
                raw.line(),
                "{\"v\":1,\"op\":\"error\",\"id\":4,\"in_reply_to\":\"delete_everything\",\"code\":\"unsupported_op\",\"message\":\"unknown op \\\"delete_everything\\\"\",\"retriable\":false}\n"
            );
            // A frame that is no JSON is answered, and the connection
            // stays in step.
            raw.send(b"{not json\n");
            assert!(raw.line().contains(r#""code":"bad_frame""#));
            // A wire_event is dropped without an answer.
            raw.send(b"{\"v\":1,\"op\":\"wire_event\",\"event\":{}}\n");
            raw.send(b"{\"v\":1,\"op\":\"get_ca\"}\n");
            assert!(raw.line().contains("ca_bundle"));
        })
        .await;
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn the_hello_comes_first_and_must_overlap() {
        let mut raw = Raw::new(pair(Arc::new(Fake::default())));
        blocking(move || {
            raw.send(b"{\"v\":1,\"op\":\"get_ca\"}\n");
            let answer = raw.line();
            assert!(answer.contains(r#""code":"bad_request""#), "{answer}");
            assert!(raw.closed());
        })
        .await;
        let mut raw = Raw::new(pair(Arc::new(Fake::default())));
        blocking(move || {
            raw.send(b"{\"v\":2,\"op\":\"hello\",\"min\":2,\"max\":3}\n");
            let answer = raw.line();
            assert!(
                answer.contains(r#""in_reply_to":"hello","code":"unsupported_version""#),
                "{answer}"
            );
            assert!(raw.closed());
        })
        .await;
        let stream = pair(Arc::new(Fake::default()));
        blocking(move || {
            // The client's own check: version 1 only.
            assert!(Client::connect(stream, "").is_ok());
        })
        .await;
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_frame_of_another_version_ends_the_connection() {
        let mut raw = Raw::new(pair(Arc::new(Fake::default())));
        blocking(move || {
            raw.send(b"{\"v\":1,\"op\":\"hello\",\"min\":1,\"max\":1}\n");
            raw.line();
            raw.send(b"{\"v\":2,\"op\":\"get_ca\"}\n");
            assert!(raw.line().contains(r#""code":"unsupported_version""#));
            assert!(raw.closed());
        })
        .await;
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_frame_too_large_ends_the_connection() {
        let mut raw = Raw::new(pair(Arc::new(Fake::default())));
        blocking(move || {
            raw.send(b"{\"v\":1,\"op\":\"hello\",\"min\":1,\"max\":1}\n");
            raw.line();
            let flood = vec![b'x'; MAX_FRAME + 1];
            // The server may close before it has read all of it.
            let _ = raw.w.write_all(&flood);
            assert!(raw.line().contains(r#""code":"bad_frame""#));
            assert!(raw.closed());
        })
        .await;
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_silent_client_is_dropped_after_the_hello_timeout() {
        let (ours, theirs) = StdStream::pair().unwrap();
        theirs.set_nonblocking(true).unwrap();
        let theirs = tokio::net::UnixStream::from_std(theirs).unwrap();
        let fake = Fake::default();
        let mut options = ServeOptions::named("t");
        options.hello_timeout = Duration::from_millis(50);
        tokio::time::timeout(Duration::from_secs(5), serve(theirs, &fake, &options))
            .await
            .expect("serve returns");
        let mut raw = Raw::new(ours);
        assert!(blocking(move || raw.closed()).await);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_client_sees_a_closed_signer_as_its_transport() {
        let (ours, theirs) = StdStream::pair().unwrap();
        drop(theirs);
        let e = blocking(move || Client::connect(ours, "").err().unwrap()).await;
        assert!(matches!(e, Error::Transport(_)), "{e}");
    }
}
