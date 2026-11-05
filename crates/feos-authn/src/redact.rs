// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! What feos-authn hands the pod from an upstream never holds the binding's
//! credential (ADR 0028 §3): every value of it at least [`MIN_LEN`] bytes
//! long is replaced wherever the upstream repeats it — the status line's
//! reason, a header's value, a body, a trailer, a WebSocket frame — by a
//! mask of the same length ([`mask`]). The length stays, so that
//! `Content-Length`, a frame's length and every offset stay true.
//!
//! A body is searched as it streams: a chunk goes on at once but for its
//! end when that end could begin a value, which waits for the next chunk
//! (at most the longest value's length less one byte). A value split
//! across chunks is found; a body without one passes as it came, without
//! a copy. Values are matched byte for byte, as the binding holds them and
//! in the forms it sends them ([`crate::bindings`]); one the upstream
//! changes (escapes, encodes otherwise, splits itself) is not recognised.
//!
//! What is replaced is counted, never recorded: the records' `redacted`.

use std::fmt;
use std::io;
use std::pin::Pin;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::task::{ready, Context, Poll};

use bytes::{Bytes, BytesMut};
use http::{HeaderMap, HeaderValue};
use memchr::memmem::Finder;
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};

/// Shorter values are not redacted: a header of "1" or "true" would mask
/// every one in a body.
pub const MIN_LEN: usize = 8;

/// The mask's text, where it fits; `*` fills the rest.
const MARKER: &[u8] = b"redacted-by-FeOS-binding";
const SHORT_MARKER: &[u8] = b"redacted";

/// Masks `out` whole: the marker when it fits, `*` for the rest. ASCII
/// without spaces, so that a masked header value, URL or token stays one.
pub fn mask(out: &mut [u8]) {
    out.fill(b'*');
    let text = if out.len() >= MARKER.len() {
        MARKER
    } else if out.len() >= SHORT_MARKER.len() {
        SHORT_MARKER
    } else {
        b""
    };
    out[..text.len()].copy_from_slice(text);
}

/// The values of one credential, or of one token, that are redacted.
#[derive(Default)]
pub struct Secrets {
    finders: Vec<Finder<'static>>,
}

impl Secrets {
    /// The values of at least [`MIN_LEN`] bytes, each once.
    pub fn new<I>(values: I) -> Secrets
    where
        I: IntoIterator,
        I::Item: AsRef<[u8]>,
    {
        let mut finders: Vec<Finder<'static>> = Vec::new();
        for v in values {
            let v = v.as_ref();
            if v.len() >= MIN_LEN && !finders.iter().any(|f| f.needle() == v) {
                finders.push(Finder::new(v).into_owned());
            }
        }
        Secrets { finders }
    }

    pub fn is_empty(&self) -> bool {
        self.finders.is_empty()
    }

    pub fn len(&self) -> usize {
        self.finders.len()
    }
}

/// What one answer is redacted of: its binding's values and its tokens'.
#[derive(Clone, Default)]
pub struct Redactor {
    sets: Vec<Arc<Secrets>>,
}

impl Redactor {
    pub fn add(&mut self, s: &Arc<Secrets>) {
        if !s.is_empty() && !self.sets.iter().any(|x| Arc::ptr_eq(x, s)) {
            self.sets.push(s.clone());
        }
    }

    /// Nothing to redact: the answer passes as it came.
    pub fn is_empty(&self) -> bool {
        self.sets.is_empty()
    }

    fn finders(&self) -> impl Iterator<Item = &Finder<'static>> {
        self.sets.iter().flat_map(|s| s.finders.iter())
    }

    /// The places in `buf` that hold a value, sorted, overlapping ones
    /// merged; `(0, inherited)` is one already, found before.
    fn spans(&self, buf: &[u8], inherited: usize) -> Vec<(usize, usize)> {
        let mut found = Vec::new();
        if inherited > 0 {
            found.push((0, inherited.min(buf.len())));
        }
        for f in self.finders() {
            let len = f.needle().len();
            // Overlapping occurrences too ("aaaa" twice in "aaaaa"),
            // merged as they come.
            let mut run: Option<(usize, usize)> = None;
            let mut from = 0;
            while let Some(at) = f.find(&buf[from..]) {
                let s = from + at;
                match &mut run {
                    Some(r) if s < r.1 => r.1 = s + len,
                    _ => {
                        found.extend(run.take());
                        run = Some((s, s + len));
                    }
                }
                from = s + 1;
            }
            found.extend(run);
        }
        if found.len() > 1 {
            found.sort_unstable();
            let mut merged: Vec<(usize, usize)> = Vec::with_capacity(found.len());
            for (s, e) in found {
                match merged.last_mut() {
                    Some(last) if s < last.1 => last.1 = last.1.max(e),
                    _ => merged.push((s, e)),
                }
            }
            found = merged;
        }
        found
    }

    /// The length of the longest end of `buf` that begins a value without
    /// being one: what must wait for the next chunk.
    fn partial(&self, buf: &[u8]) -> usize {
        let mut best = 0;
        for f in self.finders() {
            let p = f.needle();
            let w = (p.len() - 1).min(buf.len());
            if w <= best {
                continue;
            }
            let tail = &buf[buf.len() - w..];
            for i in memchr::memchr_iter(p[0], tail) {
                let l = w - i;
                if l <= best {
                    break;
                }
                if tail[i..] == p[..l] {
                    best = l;
                    break;
                }
            }
        }
        best
    }

    /// Masks every place in `v` that holds a value; how many.
    pub fn redact(&self, v: &mut [u8]) -> u64 {
        if self.is_empty() {
            return 0;
        }
        let spans = self.spans(v, 0);
        for &(s, e) in &spans {
            mask(&mut v[s..e]);
        }
        spans.len() as u64
    }

    /// `s` with every value masked, and how many.
    pub fn redact_str(&self, s: &str) -> (String, u64) {
        let mut b = s.as_bytes().to_vec();
        let n = self.redact(&mut b);
        // A value is found whole, so its bytes are whole characters; the
        // mask is ASCII.
        let s = String::from_utf8(b)
            .unwrap_or_else(|e| String::from_utf8_lossy(e.as_bytes()).into_owned());
        (s, n)
    }

    /// The header values that hold a value, masked; how many places.
    pub fn redact_headers(&self, h: &mut HeaderMap) -> u64 {
        if self.is_empty() {
            return 0;
        }
        let mut n = 0;
        for (_, v) in h.iter_mut() {
            let spans = self.spans(v.as_bytes(), 0);
            if spans.is_empty() {
                continue;
            }
            let mut b = v.as_bytes().to_vec();
            for &(s, e) in &spans {
                mask(&mut b[s..e]);
            }
            // The mask is visible ASCII, the rest was a value already.
            let mut new = HeaderValue::from_bytes(&b)
                .unwrap_or_else(|_| HeaderValue::from_static("redacted-by-FeOS-binding"));
            new.set_sensitive(v.is_sensitive());
            *v = new;
            n += spans.len() as u64;
        }
        n
    }
}

/// A stream's search: chunks in, what may go on out, the places counted
/// into `count`. What it holds back is less than the longest value.
pub struct Scanner {
    redactor: Redactor,
    /// The end of what came, as it came, not yet passed on.
    carry: Bytes,
    /// The carry's first bytes that belong to a place found and counted
    /// before.
    carry_masked: usize,
    count: Arc<AtomicU64>,
}

impl Scanner {
    pub fn new(redactor: Redactor, count: Arc<AtomicU64>) -> Scanner {
        Scanner {
            redactor,
            carry: Bytes::new(),
            carry_masked: 0,
            count,
        }
    }

    pub fn redactor(&self) -> &Redactor {
        &self.redactor
    }

    /// The bytes held back.
    pub fn held(&self) -> usize {
        self.carry.len()
    }

    /// Counts `n` places replaced elsewhere in the same answer.
    pub fn count(&self, n: u64) {
        if n > 0 {
            self.count.fetch_add(n, Ordering::Relaxed);
        }
    }

    /// Takes the next chunk; returns what may go on now, which may be
    /// nothing.
    pub fn push(&mut self, chunk: Bytes) -> Bytes {
        let buf = if self.carry.is_empty() {
            chunk
        } else {
            let mut b = BytesMut::with_capacity(self.carry.len() + chunk.len());
            b.extend_from_slice(&self.carry);
            b.extend_from_slice(&chunk);
            b.freeze()
        };
        self.round(buf, false)
    }

    /// The stream's end: what was held back.
    pub fn finish(&mut self) -> Bytes {
        let buf = std::mem::take(&mut self.carry);
        if buf.is_empty() {
            return buf;
        }
        self.round(buf, true)
    }

    fn round(&mut self, buf: Bytes, end: bool) -> Bytes {
        let inherited = std::mem::take(&mut self.carry_masked);
        let spans = self.redactor.spans(&buf, inherited);
        let cut = match end {
            true => buf.len(),
            false => buf.len() - self.redactor.partial(&buf),
        };
        let out = if spans.is_empty() {
            buf.slice(..cut)
        } else {
            let mut out = BytesMut::from(&buf[..cut]);
            let mut new = 0;
            for &(s, e) in &spans {
                // The place the carry's masked bytes belong to, counted
                // when its start went on.
                let continued = inherited > 0 && s == 0;
                if s < cut {
                    mask(&mut out[s..e.min(cut)]);
                    if !continued {
                        new += 1;
                    }
                    if e > cut {
                        self.carry_masked = e - cut;
                    }
                } else if continued {
                    self.carry_masked = e - cut;
                }
                // A place wholly in the carry is found again with the
                // next chunk.
            }
            if new > 0 {
                self.count.fetch_add(new, Ordering::Relaxed);
            }
            out.freeze()
        };
        self.carry = buf.slice(cut..);
        out
    }
}

/// An upstream's WebSocket frames (RFC 6455 §5.2) that cannot be checked:
/// compressed or of an extension (RSV bits), or masked, which a server's
/// never are. The tunnel ends.
#[derive(Debug)]
pub struct Unredactable(pub &'static str);

impl fmt::Display for Unredactable {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.0)
    }
}

impl std::error::Error for Unredactable {}

/// The upstream's side of a WebSocket tunnel, frame by frame: headers pass,
/// each payload is searched as it streams and ends with its frame. A value
/// split across two frames of a message is not found.
pub struct Frames {
    scanner: Scanner,
    head: [u8; 10],
    have: usize,
    /// Of the current frame's payload, still to come.
    left: u64,
}

impl Frames {
    pub fn new(scanner: Scanner) -> Frames {
        Frames {
            scanner,
            head: [0; 10],
            have: 0,
            left: 0,
        }
    }

    fn head_len(&self) -> usize {
        if self.have < 2 {
            return 2;
        }
        match self.head[1] & 0x7f {
            126 => 4,
            127 => 10,
            _ => 2,
        }
    }

    /// Takes the upstream's bytes; appends what may go to the pod to `out`.
    pub fn push(&mut self, mut input: &[u8], out: &mut BytesMut) -> Result<(), Unredactable> {
        while !input.is_empty() {
            if self.left > 0 {
                let take = self.left.min(input.len() as u64) as usize;
                out.extend_from_slice(&self.scanner.push(Bytes::copy_from_slice(&input[..take])));
                input = &input[take..];
                self.left -= take as u64;
                if self.left == 0 {
                    out.extend_from_slice(&self.scanner.finish());
                }
                continue;
            }
            let need = self.head_len();
            let take = (need - self.have).min(input.len());
            self.head[self.have..self.have + take].copy_from_slice(&input[..take]);
            self.have += take;
            input = &input[take..];
            if self.have < 2 {
                continue;
            }
            if self.head[0] & 0x70 != 0 {
                return Err(Unredactable(
                    "the upstream sent a compressed or extended WebSocket frame, \
                     which cannot be checked for the binding's credential",
                ));
            }
            if self.head[1] & 0x80 != 0 {
                return Err(Unredactable("the upstream sent a masked WebSocket frame"));
            }
            let need = self.head_len();
            if self.have < need {
                continue;
            }
            let h = &self.head[..need];
            self.left = match need {
                4 => u64::from(u16::from_be_bytes([h[2], h[3]])),
                10 => u64::from_be_bytes([h[2], h[3], h[4], h[5], h[6], h[7], h[8], h[9]]),
                _ => u64::from(h[1] & 0x7f),
            };
            out.extend_from_slice(h);
            self.have = 0;
        }
        Ok(())
    }

    /// The stream's end: what was held back, a frame cut short included.
    pub fn finish(&mut self, out: &mut BytesMut) {
        out.extend_from_slice(&self.scanner.finish());
        out.extend_from_slice(&self.head[..self.have]);
        self.have = 0;
    }
}

/// A tunnel's upstream end whose reads are redacted frame by frame
/// ([`Frames`]); without frames, reads pass as they come. Writes pass.
pub struct RedactedRead<T> {
    inner: T,
    frames: Option<Frames>,
    out: BytesMut,
    buf: Box<[u8]>,
    eof: bool,
}

impl<T> RedactedRead<T> {
    pub fn new(inner: T, frames: Option<Frames>) -> RedactedRead<T> {
        let buf = match frames {
            Some(_) => vec![0; 16 * 1024].into_boxed_slice(),
            None => Box::default(),
        };
        RedactedRead {
            inner,
            frames,
            out: BytesMut::new(),
            buf,
            eof: false,
        }
    }
}

impl<T: AsyncRead + Unpin> AsyncRead for RedactedRead<T> {
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        dst: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        let RedactedRead {
            inner,
            frames,
            out,
            buf,
            eof,
        } = self.get_mut();
        let Some(frames) = frames.as_mut() else {
            return Pin::new(inner).poll_read(cx, dst);
        };
        loop {
            if !out.is_empty() {
                let n = out.len().min(dst.remaining());
                dst.put_slice(&out.split_to(n));
                return Poll::Ready(Ok(()));
            }
            if *eof {
                return Poll::Ready(Ok(()));
            }
            let mut rb = ReadBuf::new(buf);
            ready!(Pin::new(&mut *inner).poll_read(cx, &mut rb))?;
            if rb.filled().is_empty() {
                *eof = true;
                frames.finish(out);
                continue;
            }
            frames.push(rb.filled(), out).map_err(io::Error::other)?;
        }
    }
}

impl<T: AsyncWrite + Unpin> AsyncWrite for RedactedRead<T> {
    fn poll_write(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.get_mut().inner).poll_write(cx, buf)
    }

    fn poll_flush(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.get_mut().inner).poll_flush(cx)
    }

    fn poll_shutdown(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.get_mut().inner).poll_shutdown(cx)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn redactor(values: &[&str]) -> Redactor {
        let mut r = Redactor::default();
        r.add(&Arc::new(Secrets::new(values.iter().map(|v| v.as_bytes()))));
        r
    }

    /// The scanner over `chunks`: what it passed on, and how many places.
    fn stream(r: &Redactor, chunks: &[&[u8]]) -> (Vec<u8>, u64) {
        let count = Arc::new(AtomicU64::new(0));
        let mut s = Scanner::new(r.clone(), count.clone());
        let mut out = Vec::new();
        for c in chunks {
            out.extend_from_slice(&s.push(Bytes::copy_from_slice(c)));
            assert!(s.held() < 64, "held {}", s.held());
        }
        out.extend_from_slice(&s.finish());
        (out, count.load(Ordering::Relaxed))
    }

    /// Every byte in a place is masked, every other one as it came.
    fn assert_masked(input: &[u8], out: &[u8], spans: &[(usize, usize)], values: &[&str]) {
        assert_eq!(out.len(), input.len());
        for (i, (a, b)) in input.iter().zip(out).enumerate() {
            if spans.iter().any(|&(s, e)| s <= i && i < e) {
                assert!(
                    *b == b'*' || MARKER.contains(b),
                    "byte {i} of {:?}",
                    String::from_utf8_lossy(out)
                );
            } else {
                assert_eq!(a, b, "byte {i} of {:?}", String::from_utf8_lossy(out));
            }
        }
        for v in values {
            let found = Finder::new(v.as_bytes()).find(out);
            assert!(found.is_none(), "{v} in {:?}", String::from_utf8_lossy(out));
        }
    }

    #[test]
    fn masks() {
        let m = |n: usize| {
            let mut b = vec![0; n];
            mask(&mut b);
            String::from_utf8(b).unwrap()
        };
        assert_eq!(m(8), "redacted");
        assert_eq!(m(12), "redacted****");
        assert_eq!(m(24), "redacted-by-FeOS-binding");
        assert_eq!(m(32), "redacted-by-FeOS-binding********");
        assert_eq!(m(3), "***");
        assert_eq!(m(0), "");
        // A masked value is still a header value.
        assert!(HeaderValue::from_str(&m(40)).is_ok());
    }

    #[test]
    fn short_and_repeated_values_are_left_out() {
        let s = Secrets::new(["1", "true", "seven77", "eight888", "eight888"]);
        assert_eq!(s.len(), 1);
        let r = redactor(&["1", "true"]);
        assert!(r.is_empty());
        let mut b = b"1 is true".to_vec();
        assert_eq!(r.redact(&mut b), 0);
        assert_eq!(&b, b"1 is true");
    }

    #[test]
    fn places_overlapping_or_inside_others_are_masked_whole() {
        // The password inside user:password, and two values that share
        // their ends: one place each.
        let r = redactor(&["service:pa55word!", "pa55word!", "abcdefgh", "efghijkl"]);
        let input = b"auth service:pa55word! then xxabcdefghijklyy and pa55word!";
        let mut out = input.to_vec();
        assert_eq!(r.redact(&mut out), 3);
        let spans = r.spans(input, 0);
        assert_eq!(spans, vec![(5, 22), (30, 42), (49, 58)]);
        assert_masked(input, &out, &spans, &["pa55word!", "abcdefgh", "efghijkl"]);
        // Repeats of one value that overlap themselves: one place.
        let r = redactor(&["aaaaaaaa"]);
        let input = [b'a'; 100];
        let mut out = input.to_vec();
        assert_eq!(r.redact(&mut out), 1);
        assert!(out.iter().all(|b| *b != b'a' || MARKER.contains(b)));
        assert!(Finder::new("aaaaaaaa").find(&out).is_none());
        // Two values side by side: two places.
        let r = redactor(&["k3y-0123456789"]);
        let mut out = b"k3y-0123456789k3y-0123456789".to_vec();
        assert_eq!(r.redact(&mut out), 2);
        assert_eq!(&out, b"redacted******redacted******");
    }

    /// The incident: a value echoed in a body, split across two chunks at
    /// every point, and across chunks of one byte.
    #[test]
    fn a_value_split_across_chunks_is_found() {
        let key = "Zq8f-91kd03jfQ0-a8sd8f";
        let r = redactor(&[key]);
        let input = format!(
            r#"{{"fault":{{"faultstring":"Rate limit quota violation. Quota limit exceeded. Identifier : {key}","detail":{{"errorcode":"policies.ratelimit.QuotaViolation"}}}}}}"#
        );
        let input = input.as_bytes();
        let spans = r.spans(input, 0);
        assert_eq!(spans.len(), 1);
        for cut in 0..=input.len() {
            let (out, n) = stream(&r, &[&input[..cut], &input[cut..]]);
            assert_masked(input, &out, &spans, &[key]);
            assert_eq!(n, 1, "cut at {cut}");
        }
        let bytes: Vec<&[u8]> = input.chunks(1).collect();
        let (out, n) = stream(&r, &bytes);
        assert_masked(input, &out, &spans, &[key]);
        assert_eq!(n, 1);
        // Whole, the mask is the marker.
        let (out, _) = stream(&r, &[input]);
        assert!(String::from_utf8(out)
            .unwrap()
            .contains("Identifier : redacted**************\""));
    }

    /// Values that begin as they end, or as another ends: wherever the
    /// chunks are cut, every place is masked and counted once.
    #[test]
    fn values_that_overlap_themselves_or_others_are_found_across_chunks() {
        let values = ["abcabcab", "cabxyz12", "12121212"];
        let r = redactor(&values);
        let input = b"--abcabcabcabxyz12--1212121212121212-ab-abcabca";
        let spans = r.spans(input, 0);
        let mut whole = input.to_vec();
        let places = r.redact(&mut whole);
        for a in 0..=input.len() {
            for b in a..=input.len() {
                let (out, n) = stream(&r, &[&input[..a], &input[a..b], &input[b..]]);
                assert_masked(input, &out, &spans, &values);
                assert_eq!(n, places, "cuts at {a} and {b}");
            }
        }
    }

    #[test]
    fn a_body_without_a_value_passes_as_it_came() {
        let r = redactor(&["sk-live-0123456789abcdef"]);
        let count = Arc::new(AtomicU64::new(0));
        let mut s = Scanner::new(r, count.clone());
        let chunk = Bytes::from(vec![b'x'; 64 * 1024]);
        let out = s.push(chunk.clone());
        // The same bytes, not a copy; nothing held back.
        assert_eq!(out.as_ptr(), chunk.as_ptr());
        assert_eq!(out.len(), chunk.len());
        assert_eq!(s.held(), 0);
        // An end that could begin the value waits, and only that end.
        let out = s.push(Bytes::from_static(b"yyy sk-li"));
        assert_eq!(&out[..], b"yyy ");
        assert_eq!(s.held(), 5);
        let out = s.push(Bytes::from_static(b"ne and more"));
        assert_eq!(&out[..], b"sk-line and more");
        assert!(s.finish().is_empty());
        assert_eq!(count.load(Ordering::Relaxed), 0);
    }

    #[test]
    fn header_values_are_masked_and_stay_sensitive() {
        let r = redactor(&["tok-0123456789"]);
        let mut h = HeaderMap::new();
        h.insert(
            "www-authenticate",
            HeaderValue::from_static(
                r#"Bearer error="invalid_token", error_description="tok-0123456789 expired""#,
            ),
        );
        let mut v = HeaderValue::from_static("https://api.example/x?token=tok-0123456789");
        v.set_sensitive(true);
        h.insert("location", v);
        h.insert("content-type", HeaderValue::from_static("application/json"));
        assert_eq!(r.redact_headers(&mut h), 2);
        assert_eq!(
            h["www-authenticate"],
            r#"Bearer error="invalid_token", error_description="redacted****** expired""#
        );
        assert_eq!(h["location"], "https://api.example/x?token=redacted******");
        assert!(h["location"].is_sensitive());
        assert_eq!(h["content-type"], "application/json");
        let (s, n) = r.redact_str("the token tok-0123456789 — refused");
        assert_eq!((s.as_str(), n), ("the token redacted****** — refused", 1));
    }

    /// A server's WebSocket frame: unmasked, FIN, opcode text.
    fn frame(payload: &[u8]) -> Vec<u8> {
        let mut f = vec![0x81];
        match payload.len() {
            n if n < 126 => f.push(n as u8),
            n if n <= 0xffff => {
                f.push(126);
                f.extend_from_slice(&(n as u16).to_be_bytes());
            }
            n => {
                f.push(127);
                f.extend_from_slice(&(n as u64).to_be_bytes());
            }
        }
        f.extend_from_slice(payload);
        f
    }

    fn frames(r: &Redactor, chunks: &[&[u8]]) -> (Result<Vec<u8>, Unredactable>, u64) {
        let count = Arc::new(AtomicU64::new(0));
        let mut f = Frames::new(Scanner::new(r.clone(), count.clone()));
        let mut out = BytesMut::new();
        for c in chunks {
            if let Err(e) = f.push(c, &mut out) {
                return (Err(e), count.load(Ordering::Relaxed));
            }
        }
        f.finish(&mut out);
        (Ok(out.to_vec()), count.load(Ordering::Relaxed))
    }

    #[test]
    fn websocket_frames_are_redacted_payload_by_payload() {
        let key = "ws-key-0123456789";
        let r = redactor(&[key]);
        let long = format!("{}{key}{}", "a".repeat(200), "b".repeat(70_000));
        let mut input = frame(format!(r#"{{"echo":"{key}"}}"#).as_bytes());
        input.extend(frame(b""));
        input.extend(frame(long.as_bytes()));
        input.extend(frame(b"plain"));
        let (whole, n) = frames(&r, &[&input]);
        let whole = whole.unwrap();
        assert_eq!(n, 2);
        assert_eq!(whole.len(), input.len());
        assert!(Finder::new(key).find(&whole).is_none());
        let mut want = frame(br#"{"echo":"redacted*********"}"#);
        want.extend(frame(b""));
        assert_eq!(&whole[..want.len()], &want[..]);
        // The same wherever the stream is cut.
        let short = &input[..want.len() + 40];
        let (part, _) = frames(&r, &[short]);
        let part = part.unwrap();
        for cut in 0..=short.len() {
            let (out, _) = frames(&r, &[&short[..cut], &short[cut..]]);
            assert_eq!(out.unwrap(), part, "cut at {cut}");
        }
        let bytes: Vec<&[u8]> = input.chunks(7).collect();
        assert_eq!(frames(&r, &bytes).0.unwrap(), whole);

        // A compressed frame (RSV1) or a masked one: the tunnel ends.
        let mut deflated = frame(b"x");
        deflated[0] |= 0x40;
        assert!(frames(&r, &[&deflated]).0.is_err());
        let mut masked = frame(b"x");
        masked[1] |= 0x80;
        assert!(frames(&r, &[&masked]).0.is_err());
    }
}
