// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The application's side of one connection: the hello at
//! [`Client::connect`], then one request at a time, each answered before
//! the next.
//!
//! It blocks. A TLS stack asks for its key's signature and its
//! certificate in calls that return a value (rustls's `Signer::sign`,
//! Go's `crypto.Signer`), and a signature over a local stream takes well
//! under a millisecond. A blocking client works in any runtime, or none;
//! a timeout is the stream's (`set_read_timeout`).
//!
//! It holds no retry: after [`Error::Transport`] the connection is
//! spent, and the library above dials a new one.

use std::io::{BufReader, Read, Write};

use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};

use crate::error::Error;
use crate::frame::{self, ReadError};
use crate::{op, Empty, Envelope, HandshakeReport, Out, HASH_NONE, MAX_FRAME, VERSION};

/// One feid/1 connection, after its hello.
pub struct Client<S: Read + Write> {
    stream: BufReader<S>,
    version: u32,
    server: String,
}

#[derive(Serialize)]
struct HelloPayload<'a> {
    min: u32,
    max: u32,
    #[serde(skip_serializing_if = "str::is_empty")]
    client: &'a str,
}

#[derive(Deserialize)]
struct HelloAck {
    proto: u32,
    #[serde(default)]
    server: String,
}

#[derive(Serialize)]
struct SignPayload<'a> {
    hash: &'a str,
    #[serde(with = "crate::b64")]
    message: &'a [u8],
}

#[derive(Serialize)]
struct ReportPayload<'a> {
    report: &'a HandshakeReport,
}

#[derive(Deserialize)]
struct ErrorFrame {
    code: String,
    #[serde(default)]
    message: String,
    #[serde(default)]
    retriable: bool,
}

impl<S: Read + Write> Client<S> {
    /// Says hello on `stream`, a dialed connection to the signer, and
    /// takes the version the server chose. `name` is for the server's
    /// log only, such as `feid-rs/0.1.0`.
    pub fn connect(stream: S, name: &str) -> Result<Client<S>, Error> {
        let mut client = Client {
            stream: BufReader::new(stream),
            version: VERSION,
            server: String::new(),
        };
        client.write(
            op::HELLO,
            HelloPayload {
                min: VERSION,
                max: VERSION,
                client: name,
            },
        )?;
        let raw = client.read()?;
        let envelope = Envelope::parse(&raw).map_err(Error::Malformed)?;
        if envelope.op == op::ERROR {
            return Err(remote(&raw));
        }
        if envelope.op != op::HELLO_ACK {
            return Err(Error::Malformed(format!(
                "expected hello_ack, got {:?}",
                envelope.op
            )));
        }
        let ack: HelloAck = serde_json::from_slice(&raw)
            .map_err(|e| Error::Malformed(format!("hello_ack: {e}")))?;
        if ack.proto != VERSION {
            return Err(Error::Malformed(format!(
                "the server chose version {}, outside {VERSION}..{VERSION}",
                ack.proto
            )));
        }
        client.version = ack.proto;
        client.server = ack.server;
        Ok(client)
    }

    /// The version negotiated.
    pub fn version(&self) -> u32 {
        self.version
    }

    /// The server's name from its hello_ack, for logs.
    pub fn server(&self) -> &str {
        &self.server
    }

    /// The stream, to set its timeouts.
    pub fn stream(&self) -> &S {
        self.stream.get_ref()
    }

    /// The workload's certificate chain, PEM, its own certificate first.
    pub fn certificate(&mut self) -> Result<String, Error> {
        #[derive(Deserialize)]
        struct Answer {
            chain_pem: String,
        }
        let a: Answer = self.call(op::GET_CERTIFICATE, Empty {}, op::CERTIFICATE)?;
        Ok(a.chain_pem)
    }

    /// The platform's trust bundle, PEM.
    pub fn trust_bundle(&mut self) -> Result<String, Error> {
        #[derive(Deserialize)]
        struct Answer {
            ca_pem: String,
        }
        let a: Answer = self.call(op::GET_CA, Empty {}, op::CA_BUNDLE)?;
        Ok(a.ca_pem)
    }

    /// The signature of `message`, a TLS 1.3 CertificateVerify input,
    /// unhashed, by the workload's key.
    pub fn sign(&mut self, message: &[u8]) -> Result<Vec<u8>, Error> {
        #[derive(Deserialize)]
        struct Answer {
            #[serde(with = "crate::b64")]
            signature: Vec<u8>,
        }
        let a: Answer = self.call(
            op::SIGN,
            SignPayload {
                hash: HASH_NONE,
                message,
            },
            op::SIGNATURE,
        )?;
        Ok(a.signature)
    }

    /// Sends a handshake's report. Never answered: `Ok` means written.
    pub fn report(&mut self, report: &HandshakeReport) -> Result<(), Error> {
        self.write(op::HANDSHAKE_REPORT, ReportPayload { report })
    }

    fn call<P: Serialize, A: DeserializeOwned>(
        &mut self,
        op: &str,
        payload: P,
        answer: &str,
    ) -> Result<A, Error> {
        self.write(op, payload)?;
        let raw = self.read()?;
        let envelope = Envelope::parse(&raw).map_err(Error::Malformed)?;
        if envelope.op == op::ERROR {
            return Err(remote(&raw));
        }
        if envelope.op != answer {
            return Err(Error::Malformed(format!(
                "{op} answered with {:?}, not {answer:?}",
                envelope.op
            )));
        }
        serde_json::from_slice(&raw).map_err(|e| Error::Malformed(format!("{answer}: {e}")))
    }

    fn write<P: Serialize>(&mut self, op: &str, payload: P) -> Result<(), Error> {
        let frame = Out {
            v: self.version,
            op,
            id: None,
            payload,
        };
        let bytes = frame::encode(&frame).ok_or(Error::FrameTooLarge)?;
        let stream = self.stream.get_mut();
        stream.write_all(&bytes).map_err(Error::Transport)?;
        stream.flush().map_err(Error::Transport)
    }

    fn read(&mut self) -> Result<Vec<u8>, Error> {
        frame::read_blocking(&mut self.stream, MAX_FRAME).map_err(|e| match e {
            ReadError::TooLarge => Error::FrameTooLarge,
            ReadError::Io(e) => Error::Transport(e),
            ReadError::Closed | ReadError::Truncated => Error::Transport(std::io::Error::new(
                std::io::ErrorKind::UnexpectedEof,
                "the signer closed the connection",
            )),
        })
    }
}

fn remote(raw: &[u8]) -> Error {
    match serde_json::from_slice::<ErrorFrame>(raw) {
        Ok(f) => Error::Remote {
            code: f.code,
            message: f.message,
            retriable: f.retriable,
        },
        Err(e) => Error::Malformed(format!("error frame: {e}")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::error::code;

    #[test]
    fn golden_request_frames() {
        let hello = Out {
            v: 1,
            op: op::HELLO,
            id: None,
            payload: HelloPayload {
                min: 1,
                max: 1,
                client: "fetls-go/1.0",
            },
        };
        assert_eq!(
            serde_json::to_string(&hello).unwrap(),
            r#"{"v":1,"op":"hello","min":1,"max":1,"client":"fetls-go/1.0"}"#
        );
        let get = Out {
            v: 1,
            op: op::GET_CERTIFICATE,
            id: None,
            payload: Empty {},
        };
        assert_eq!(
            serde_json::to_string(&get).unwrap(),
            r#"{"v":1,"op":"get_certificate"}"#
        );
        let sign = Out {
            v: 1,
            op: op::SIGN,
            id: None,
            payload: SignPayload {
                hash: HASH_NONE,
                message: b" ",
            },
        };
        assert_eq!(
            serde_json::to_string(&sign).unwrap(),
            r#"{"v":1,"op":"sign","hash":"none","message":"IA=="}"#
        );
    }

    #[test]
    fn a_missing_retriable_is_never_retried() {
        let e = remote(br#"{"v":1,"op":"error","in_reply_to":"get_ca","code":"internal","message":"internal error"}"#);
        assert!(e.is_refusal() && !e.is_retriable_refusal());
        assert_eq!(e.code(), Some(code::INTERNAL));
        let e = remote(br#"{"v":1,"op":"error","in_reply_to":"sign","code":"unavailable.audit_log","message":"","retriable":true}"#);
        assert!(e.is_retriable_refusal());
    }
}
