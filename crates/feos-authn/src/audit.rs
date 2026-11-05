// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The audit file (ADR 0014): one JSON object per line, appended to the
//! file `feosd` opened for the run (`<pod dir>/authn-audit.ndjson`, fd 3)
//! and that `feosd` reads and ships. A record is written before its answer
//! leaves; a request whose record cannot be written is refused.
//!
//! Records never hold a credential's value, a query string or a body.
//! The kinds, by `type`:
//!
//! - `http`: a request through an HTTP binding, written when its answer's
//!   head is known and before it is sent to the pod: the upstream's
//!   status, or the refusal's with its `error`.
//! - `http_end`: the same request's end, once its answer's body (or the
//!   tunnel of an upgrade) is done: the bytes of both directions and the
//!   whole duration, joined to its `http` record by `spanId`.
//! - `token`: a fetch of an OAuth 2.0 token, before the token is used.
//! - `bindings`: a set of bindings opened and in use.

use std::fs::File;
use std::io::{self, Write};
use std::os::fd::OwnedFd;
use std::sync::Mutex;

use chrono::{SecondsFormat, Utc};
use serde::Serialize;

/// A refusal or failure in a record: a code of `FeOS-Binding-Error`'s and a
/// message without secrets.
#[derive(Clone, Debug, Serialize, PartialEq, Eq)]
pub struct ErrorRec {
    pub code: String,
    pub message: String,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct HttpRecord<'a> {
    pub ts: String,
    #[serde(rename = "type")]
    pub kind: &'static str,
    /// The binding's name; null when none matched.
    pub binding: Option<&'a str>,
    /// "name" or "intercept"; null when no binding matched.
    pub mode: Option<&'static str>,
    pub method: &'a str,
    /// The upstream's host, or what the pod asked for when no binding
    /// matched.
    pub host: Option<&'a str>,
    /// The path without its query string.
    pub path: &'a str,
    pub status: u16,
    /// Until the answer's head.
    pub duration_ms: u64,
    /// The address that was dialed, "ip:port"; null for a refusal before
    /// any.
    pub upstream_addr: Option<String>,
    /// "headers", "basic", "oauth2" or "clientCertificate".
    pub credential: Option<&'static str>,
    pub trace_id: String,
    pub span_id: String,
    pub parent_span_id: Option<String>,
    pub error: Option<ErrorRec>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct HttpEndRecord<'a> {
    pub ts: String,
    #[serde(rename = "type")]
    pub kind: &'static str,
    pub binding: Option<&'a str>,
    pub trace_id: &'a str,
    pub span_id: &'a str,
    pub status: u16,
    /// The request body's bytes, from the pod; for an upgrade, the bytes
    /// the pod sent through the tunnel.
    pub bytes_in: u64,
    /// The answer body's bytes, to the pod; for an upgrade, the bytes it
    /// received through the tunnel.
    pub bytes_out: u64,
    /// From the request's arrival to its end.
    pub duration_ms: u64,
    /// Set when the exchange did not end as it should: the upstream's body
    /// failed, or the pod went away before the end.
    pub error: Option<ErrorRec>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TokenRecord<'a> {
    pub ts: String,
    #[serde(rename = "type")]
    pub kind: &'static str,
    pub binding: &'a str,
    /// The token endpoint's host.
    pub host: &'a str,
    /// "first_use", "renewal" or "unauthorized" (after a 401).
    pub reason: &'static str,
    pub status: Option<u16>,
    pub expires_in: Option<u64>,
    pub duration_ms: u64,
    pub upstream_addr: Option<String>,
    pub error: Option<ErrorRec>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct BindingsRecord<'a> {
    pub ts: String,
    #[serde(rename = "type")]
    pub kind: &'static str,
    pub version: &'a str,
    /// The HTTP bindings' names.
    pub http: Vec<&'a str>,
    /// Bindings that cannot be served, with why.
    pub invalid: Vec<InvalidBinding<'a>>,
}

#[derive(Serialize)]
pub struct InvalidBinding<'a> {
    pub binding: &'a str,
    pub message: &'a str,
}

pub fn now() -> String {
    Utc::now().to_rfc3339_opts(SecondsFormat::Nanos, true)
}

/// The audit file. Each record is one `write(2)` of a whole line to a file
/// open for appending, under a lock, so lines never interleave.
pub struct Audit {
    file: Mutex<File>,
}

impl Audit {
    pub fn new(fd: OwnedFd) -> Audit {
        Audit {
            file: Mutex::new(File::from(fd)),
        }
    }

    pub fn write<T: Serialize>(&self, rec: &T) -> io::Result<()> {
        let mut line = serde_json::to_vec(rec).map_err(io::Error::other)?;
        line.push(b'\n');
        // A writer that panicked left at most a torn line; the file is
        // still the file.
        let mut f = self.file.lock().unwrap_or_else(|e| e.into_inner());
        f.write_all(&line)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Seek};

    #[test]
    fn records_are_lines() {
        let tmp = tempfile::tempfile().unwrap();
        let mut reader = tmp.try_clone().unwrap();
        let audit = Audit::new(OwnedFd::from(tmp));
        audit
            .write(&HttpRecord {
                ts: now(),
                kind: "http",
                binding: Some("erp"),
                mode: Some("name"),
                method: "GET",
                host: Some("api.erp.example"),
                path: "/x",
                status: 200,
                duration_ms: 12,
                upstream_addr: Some("192.0.2.1:443".into()),
                credential: Some("headers"),
                trace_id: "0af7651916cd43dd8448eb211c80319c".into(),
                span_id: "b7ad6b7169203331".into(),
                parent_span_id: None,
                error: None,
            })
            .unwrap();
        audit
            .write(&TokenRecord {
                ts: now(),
                kind: "token",
                binding: "graph",
                host: "login.example",
                reason: "first_use",
                status: Some(200),
                expires_in: Some(3600),
                duration_ms: 3,
                upstream_addr: None,
                error: None,
            })
            .unwrap();
        let mut s = String::new();
        reader.rewind().unwrap();
        reader.read_to_string(&mut s).unwrap();
        let lines: Vec<serde_json::Value> = s
            .lines()
            .map(|l| serde_json::from_str(l).unwrap())
            .collect();
        assert_eq!(lines.len(), 2);
        assert_eq!(lines[0]["type"], "http");
        assert_eq!(lines[0]["upstreamAddr"], "192.0.2.1:443");
        assert_eq!(lines[0]["durationMs"], 12);
        assert_eq!(lines[0]["parentSpanId"], serde_json::Value::Null);
        assert_eq!(lines[0]["error"], serde_json::Value::Null);
        assert!(lines[0]["ts"].as_str().unwrap().ends_with('Z'));
        assert_eq!(lines[1]["type"], "token");
        assert_eq!(lines[1]["expiresIn"], 3600);
    }

    #[test]
    fn a_record_that_cannot_be_written_fails() {
        // A file open for reading only: write(2) fails with EBADF.
        let tmp = tempfile::NamedTempFile::new().unwrap();
        let ro = File::open(tmp.path()).unwrap();
        let audit = Audit::new(OwnedFd::from(ro));
        assert!(audit.write(&serde_json::json!({"type": "http"})).is_err());
    }
}
