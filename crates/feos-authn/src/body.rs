// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Bodies as they pass: streamed frame by frame, never buffered whole, and
//! counted. An answer's body carries its request's [`EndGuard`], which
//! writes the `http_end` record when the body is done or dropped, and holds
//! the binding's concurrency permit until then.

use std::pin::Pin;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::Instant;

use bytes::Bytes;
use http_body::{Body, Frame, SizeHint};
use hyper::body::Incoming;
use tokio::sync::OwnedSemaphorePermit;

use crate::audit::{self, Audit, ErrorRec, HttpEndRecord};

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
        }
    }

    /// A body passed on, its data bytes added to `counter`.
    pub fn counted(inner: Incoming, counter: Arc<AtomicU64>) -> ProxyBody {
        ProxyBody {
            kind: Kind::Incoming(inner),
            counter: Some(counter),
            guard: None,
        }
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
            Kind::Incoming(inner) => match Pin::new(inner).poll_frame(cx) {
                Poll::Ready(Some(Err(e))) => Poll::Ready(Some(Err(BoxError::from(e)))),
                Poll::Ready(Some(Ok(f))) => Poll::Ready(Some(Ok(f))),
                Poll::Ready(None) => Poll::Ready(None),
                Poll::Pending => Poll::Pending,
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
        match &self.kind {
            Kind::Full(b) => b.is_none(),
            Kind::Incoming(i) => i.is_end_stream(),
        }
    }

    fn size_hint(&self) -> SizeHint {
        match &self.kind {
            Kind::Full(b) => SizeHint::with_exact(b.as_ref().map_or(0, |b| b.len() as u64)),
            Kind::Incoming(i) => i.size_hint(),
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
