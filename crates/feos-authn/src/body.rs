// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Bodies as they pass: streamed frame by frame, never buffered whole, and
//! counted. An answer's body carries its request's [`EndGuard`], which
//! writes the `http_end` record when the body is done or dropped, and holds
//! the binding's concurrency permit until then. An upstream's answer is
//! redacted of the binding's credential as it streams ([`crate::redact`]):
//! its data, which keeps its length, and its trailers. A gzip answer is
//! unpacked first ([`crate::gunzip`]).

use std::pin::Pin;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::task::{ready, Context, Poll};
use std::time::Instant;

use bytes::Bytes;
use http_body::{Body, Frame, SizeHint};
use hyper::body::Incoming;
use tokio::sync::OwnedSemaphorePermit;

use crate::audit::{self, Audit, ErrorRec, HttpEndRecord};
use crate::gunzip::Gunzip;
use crate::redact::Scanner;

pub type BoxError = Box<dyn std::error::Error + Send + Sync>;

enum Kind {
    Incoming(Incoming),
    Full(Option<Bytes>),
}

/// The body type on both legs: the pod's or the upstream's, passed on, or
/// one of the proxy's own.
pub struct ProxyBody {
    kind: Kind,
    counter: Option<Arc<AtomicU64>>,
    guard: Option<EndGuard>,
    redact: Option<Box<Redacting>>,
}

/// A body's redaction: its search, the unpacking of a gzip answer before
/// it, and trailers that wait for what the search held back.
struct Redacting {
    scanner: Scanner,
    gunzip: Option<Gunzip>,
    trailers: Option<http::HeaderMap>,
    ended: bool,
}

impl Redacting {
    fn poll(
        &mut self,
        inner: &mut Incoming,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Bytes>, BoxError>>> {
        loop {
            if let Some(t) = self.trailers.take() {
                return Poll::Ready(Some(Ok(Frame::trailers(t))));
            }
            if self.ended {
                return Poll::Ready(None);
            }
            // A chunk being unpacked goes on before the next is read.
            if let Some(g) = self.gunzip.as_mut().filter(|g| g.busy()) {
                let rest = match g.step() {
                    Ok(b) => self.scanner.push(b),
                    Err(e) => return Poll::Ready(Some(Err(gzip_error(e)))),
                };
                if !rest.is_empty() {
                    return Poll::Ready(Some(Ok(Frame::data(rest))));
                }
                continue;
            }
            let rest = match ready!(Pin::new(&mut *inner).poll_frame(cx)) {
                Some(Err(e)) => return Poll::Ready(Some(Err(BoxError::from(e)))),
                Some(Ok(frame)) => match frame.into_data() {
                    Ok(data) => match self.gunzip.as_mut() {
                        Some(g) => {
                            g.feed(data);
                            continue;
                        }
                        None => self.scanner.push(data),
                    },
                    Err(frame) => {
                        let Ok(mut t) = frame.into_trailers() else {
                            continue;
                        };
                        let n = self.scanner.redactor().redact_headers(&mut t);
                        self.scanner.count(n);
                        self.trailers = Some(t);
                        match self.end() {
                            Ok(b) => b,
                            Err(e) => return Poll::Ready(Some(Err(e))),
                        }
                    }
                },
                None => {
                    self.ended = true;
                    match self.end() {
                        Ok(b) => b,
                        Err(e) => return Poll::Ready(Some(Err(e))),
                    }
                }
            };
            if !rest.is_empty() {
                return Poll::Ready(Some(Ok(Frame::data(rest))));
            }
        }
    }

    /// The data's end: the rest of the unpacking, searched, and what the
    /// search held back.
    fn end(&mut self) -> Result<Bytes, BoxError> {
        let Some(g) = self.gunzip.as_mut() else {
            return Ok(self.scanner.finish());
        };
        let last = g.finish().map_err(gzip_error)?;
        let a = self.scanner.push(last);
        let b = self.scanner.finish();
        Ok(match (a.is_empty(), b.is_empty()) {
            (_, true) => a,
            (true, false) => b,
            (false, false) => [a, b].concat().into(),
        })
    }
}

fn gzip_error(e: std::io::Error) -> BoxError {
    format!("the upstream's gzip answer cannot be unpacked: {e}").into()
}

impl ProxyBody {
    pub fn empty() -> ProxyBody {
        ProxyBody::full(Bytes::new())
    }

    pub fn full(b: impl Into<Bytes>) -> ProxyBody {
        let b = b.into();
        ProxyBody {
            kind: Kind::Full((!b.is_empty()).then_some(b)),
            counter: None,
            guard: None,
            redact: None,
        }
    }

    /// A body passed on, its data bytes added to `counter`.
    pub fn counted(inner: Incoming, counter: Arc<AtomicU64>) -> ProxyBody {
        ProxyBody {
            kind: Kind::Incoming(inner),
            counter: Some(counter),
            guard: None,
            redact: None,
        }
    }

    /// A body passed on redacted by `scanner`, which counts what it
    /// replaced; unpacked first if it is `gzip`.
    pub fn redacted(mut self, scanner: Scanner, gzip: bool) -> ProxyBody {
        self.redact = Some(Box::new(Redacting {
            scanner,
            gunzip: gzip.then(Gunzip::default),
            trailers: None,
            ended: false,
        }));
        self
    }

    /// Ends the exchange of `guard` with this body: its bytes count as the
    /// answer's, and its end, or its drop, writes the record.
    pub fn with_guard(mut self, guard: EndGuard) -> ProxyBody {
        if self.counter.is_none() {
            self.counter = Some(guard.bytes_out.clone());
        }
        self.guard = Some(guard);
        self
    }

    fn finish(&mut self, error: Option<ErrorRec>) {
        if let Some(g) = self.guard.take() {
            g.finish(error);
        }
    }
}

impl Body for ProxyBody {
    type Data = Bytes;
    type Error = BoxError;

    fn poll_frame(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Bytes>, BoxError>>> {
        let this = self.get_mut();
        let polled = match &mut this.kind {
            Kind::Full(b) => Poll::Ready(b.take().map(|b| Ok(Frame::data(b)))),
            Kind::Incoming(inner) => match &mut this.redact {
                Some(r) => r.poll(inner, cx),
                None => match Pin::new(inner).poll_frame(cx) {
                    Poll::Ready(Some(Err(e))) => Poll::Ready(Some(Err(BoxError::from(e)))),
                    Poll::Ready(Some(Ok(f))) => Poll::Ready(Some(Ok(f))),
                    Poll::Ready(None) => Poll::Ready(None),
                    Poll::Pending => Poll::Pending,
                },
            },
        };
        match &polled {
            Poll::Ready(Some(Ok(f))) => {
                if let (Some(d), Some(c)) = (f.data_ref(), &this.counter) {
                    c.fetch_add(d.len() as u64, Ordering::Relaxed);
                }
                if let Kind::Full(None) = this.kind {
                    // A full body is one frame; its end is known now.
                    this.finish(None);
                }
            }
            Poll::Ready(Some(Err(e))) => {
                let msg = e.to_string();
                this.finish(Some(ErrorRec {
                    code: "upstream_body".into(),
                    message: msg,
                }));
            }
            Poll::Ready(None) => this.finish(None),
            Poll::Pending => {}
        }
        polled
    }

    fn is_end_stream(&self) -> bool {
        match (&self.kind, &self.redact) {
            (Kind::Full(b), _) => b.is_none(),
            (Kind::Incoming(i), None) => i.is_end_stream(),
            (Kind::Incoming(i), Some(r)) => {
                r.trailers.is_none()
                    && (r.ended
                        || (i.is_end_stream() && r.scanner.held() == 0 && r.gunzip.is_none()))
            }
        }
    }

    fn size_hint(&self) -> SizeHint {
        match &self.kind {
            Kind::Full(b) => SizeHint::with_exact(b.as_ref().map_or(0, |b| b.len() as u64)),
            // Unpacked, its length is not known before its end.
            Kind::Incoming(_) if self.redact.as_ref().is_some_and(|r| r.gunzip.is_some()) => {
                SizeHint::new()
            }
            Kind::Incoming(i) => {
                let inner = i.size_hint();
                // What the search holds back is still to come; the mask
                // keeps every length.
                let held = self.redact.as_ref().map_or(0, |r| r.scanner.held() as u64);
                let mut h = SizeHint::new();
                h.set_lower(inner.lower().saturating_add(held));
                if let Some(u) = inner.upper() {
                    h.set_upper(u.saturating_add(held).max(h.lower()));
                }
                h
            }
        }
    }
}

impl Drop for ProxyBody {
    fn drop(&mut self) {
        // A body at its end is often dropped without a last poll (an empty
        // one, a HEAD's): that is its end, not an abort. Any other drop is
        // the guard's own, an abort.
        if self.is_end_stream() {
            self.finish(None);
        }
    }
}

/// The end of one request's exchange: writes its `http_end` record once,
/// when finished or dropped, and releases what the request held.
pub struct EndGuard {
    pub audit: Arc<Audit>,
    pub binding: Option<String>,
    pub trace_id: String,
    pub span_id: String,
    pub status: u16,
    pub start: Instant,
    pub bytes_in: Arc<AtomicU64>,
    pub bytes_out: Arc<AtomicU64>,
    /// The places of the binding's credential replaced in the answer.
    pub redacted: Arc<AtomicU64>,
    pub permit: Option<OwnedSemaphorePermit>,
    done: bool,
}

impl EndGuard {
    pub fn new(
        audit: Arc<Audit>,
        binding: Option<String>,
        trace_id: String,
        span_id: String,
        start: Instant,
        bytes_in: Arc<AtomicU64>,
    ) -> EndGuard {
        EndGuard {
            audit,
            binding,
            trace_id,
            span_id,
            status: 0,
            start,
            bytes_in,
            bytes_out: Arc::new(AtomicU64::new(0)),
            redacted: Arc::new(AtomicU64::new(0)),
            permit: None,
            done: false,
        }
    }

    pub fn finish(mut self, error: Option<ErrorRec>) {
        self.write(error);
    }

    fn write(&mut self, error: Option<ErrorRec>) {
        if self.done {
            return;
        }
        self.done = true;
        let rec = HttpEndRecord {
            ts: audit::now(),
            kind: "http_end",
            binding: self.binding.as_deref(),
            trace_id: &self.trace_id,
            span_id: &self.span_id,
            status: self.status,
            bytes_in: self.bytes_in.load(Ordering::Relaxed),
            bytes_out: self.bytes_out.load(Ordering::Relaxed),
            redacted: self.redacted.load(Ordering::Relaxed),
            duration_ms: self.start.elapsed().as_millis() as u64,
            error,
        };
        if let Err(e) = self.audit.write(&rec) {
            // The answer is out already; what is left is to say so.
            log::error!("audit: write an http_end record: {e}");
        }
    }
}

impl Drop for EndGuard {
    fn drop(&mut self) {
        self.write(Some(ErrorRec {
            code: "aborted".into(),
            message: "the exchange ended before its answer's end".into(),
        }));
    }
}
