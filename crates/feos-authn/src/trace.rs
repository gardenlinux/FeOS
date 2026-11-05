// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! W3C trace context on a binding's requests. The proxy is a client span
//! of its own: a request's `traceparent` names its parent, the upstream
//! gets the proxy's span as its parent, and `tracestate` passes on as it
//! came. A request without a valid `traceparent` starts a trace, sampled,
//! since its record is kept anyway.

use http::{HeaderMap, HeaderValue};
use ring::rand::{SecureRandom, SystemRandom};

pub const TRACEPARENT: &str = "traceparent";
pub const TRACESTATE: &str = "tracestate";

pub struct TraceContext {
    pub trace_id: [u8; 16],
    pub parent_span_id: Option<[u8; 8]>,
    pub span_id: [u8; 8],
    pub flags: u8,
    /// `tracestate`, passed on only with a valid `traceparent`.
    pub state: Vec<HeaderValue>,
}

fn random(buf: &mut [u8]) {
    // getrandom(2) does not fail once the kernel's pool is ready, which it
    // is long before a pod runs; IDs of zeros would be invalid, so a
    // failure falls back to the clock, as unique as IDs need to be here.
    if SystemRandom::new().fill(buf).is_err() {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(1);
        for (i, b) in buf.iter_mut().enumerate() {
            *b = (nanos >> ((i % 16) * 8)) as u8 | 1;
        }
    }
}

fn nonzero_random<const N: usize>() -> [u8; N] {
    let mut b = [0u8; N];
    while b.iter().all(|&x| x == 0) {
        random(&mut b);
    }
    b
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn unhex<const N: usize>(s: &str) -> Option<[u8; N]> {
    let s = s.as_bytes();
    if s.len() != 2 * N {
        return None;
    }
    let digit = |c: u8| match c {
        b'0'..=b'9' => Some(c - b'0'),
        b'a'..=b'f' => Some(c - b'a' + 10),
        _ => None,
    };
    let mut out = [0u8; N];
    for (i, o) in out.iter_mut().enumerate() {
        *o = digit(s[2 * i])? << 4 | digit(s[2 * i + 1])?;
    }
    Some(out)
}

/// Parses a `traceparent` value: the trace ID, the parent's span ID and the
/// flags. Version 00 is exactly that; a later version may append fields
/// after a dash, which are ignored; version ff is invalid.
pub fn parse_traceparent(v: &str) -> Option<([u8; 16], [u8; 8], u8)> {
    if v.len() < 55 || !v.is_ascii() {
        return None;
    }
    let (head, rest) = v.split_at(55);
    let version = unhex::<1>(&head[0..2])?[0];
    if version == 0xff || (version == 0 && !rest.is_empty()) {
        return None;
    }
    if !rest.is_empty() && !rest.starts_with('-') {
        return None;
    }
    let b = head.as_bytes();
    if b[2] != b'-' || b[35] != b'-' || b[52] != b'-' {
        return None;
    }
    let trace: [u8; 16] = unhex(&head[3..35])?;
    let parent: [u8; 8] = unhex(&head[36..52])?;
    let flags = unhex::<1>(&head[53..55])?[0];
    if trace.iter().all(|&x| x == 0) || parent.iter().all(|&x| x == 0) {
        return None;
    }
    Some((trace, parent, flags))
}

impl TraceContext {
    /// The proxy's span for a request with these headers.
    pub fn from_headers(h: &HeaderMap) -> TraceContext {
        let mut values = h.get_all(TRACEPARENT).iter();
        let parsed = match (values.next(), values.next()) {
            (Some(v), None) => v.to_str().ok().and_then(parse_traceparent),
            _ => None,
        };
        match parsed {
            Some((trace_id, parent, flags)) => TraceContext {
                trace_id,
                parent_span_id: Some(parent),
                span_id: nonzero_random(),
                flags,
                state: h.get_all(TRACESTATE).iter().cloned().collect(),
            },
            None => TraceContext {
                trace_id: nonzero_random(),
                parent_span_id: None,
                span_id: nonzero_random(),
                flags: 0x01,
                state: Vec::new(),
            },
        }
    }

    /// The `traceparent` the upstream gets: the proxy's span as its parent.
    pub fn traceparent(&self) -> HeaderValue {
        let v = format!(
            "00-{}-{}-{:02x}",
            hex(&self.trace_id),
            hex(&self.span_id),
            self.flags
        );
        // Hex digits and dashes only.
        HeaderValue::from_str(&v).unwrap_or(HeaderValue::from_static(""))
    }

    pub fn trace_id(&self) -> String {
        hex(&self.trace_id)
    }

    pub fn span_id(&self) -> String {
        hex(&self.span_id)
    }

    pub fn parent_span_id(&self) -> Option<String> {
        self.parent_span_id.map(|p| hex(&p))
    }

    /// Sets the trace context on the upstream's request headers.
    pub fn apply(&self, h: &mut HeaderMap) {
        h.remove(TRACEPARENT);
        h.remove(TRACESTATE);
        h.insert(TRACEPARENT, self.traceparent());
        for s in &self.state {
            h.append(TRACESTATE, s.clone());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const TP: &str = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01";

    #[test]
    fn parses_version_00() {
        let (t, p, f) = parse_traceparent(TP).unwrap();
        assert_eq!(hex(&t), "0af7651916cd43dd8448eb211c80319c");
        assert_eq!(hex(&p), "b7ad6b7169203331");
        assert_eq!(f, 1);
    }

    #[test]
    fn refuses_invalid() {
        for bad in [
            "",
            "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01-x",
            "ff-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
            "00-00000000000000000000000000000000-b7ad6b7169203331-01",
            "00-0af7651916cd43dd8448eb211c80319c-0000000000000000-01",
            "00-0AF7651916CD43DD8448EB211C80319C-b7ad6b7169203331-01",
            "00_0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
        ] {
            assert!(parse_traceparent(bad).is_none(), "{bad}");
        }
        // A later version may carry more.
        assert!(
            parse_traceparent("01-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01-more")
                .is_some()
        );
    }

    #[test]
    fn continues_a_trace_as_a_child_span() {
        let mut h = HeaderMap::new();
        h.insert(TRACEPARENT, HeaderValue::from_static(TP));
        h.insert(TRACESTATE, HeaderValue::from_static("vendor=x"));
        let t = TraceContext::from_headers(&h);
        assert_eq!(t.trace_id(), "0af7651916cd43dd8448eb211c80319c");
        assert_eq!(t.parent_span_id().as_deref(), Some("b7ad6b7169203331"));
        assert_ne!(t.span_id(), "b7ad6b7169203331");
        let mut out = HeaderMap::new();
        t.apply(&mut out);
        let tp = out.get(TRACEPARENT).unwrap().to_str().unwrap();
        assert_eq!(
            tp,
            format!("00-0af7651916cd43dd8448eb211c80319c-{}-01", t.span_id())
        );
        assert_eq!(out.get(TRACESTATE).unwrap(), "vendor=x");
    }

    #[test]
    fn starts_a_trace_without_one() {
        let mut h = HeaderMap::new();
        h.insert(TRACESTATE, HeaderValue::from_static("vendor=x"));
        let t = TraceContext::from_headers(&h);
        assert!(t.parent_span_id.is_none());
        let mut out = HeaderMap::new();
        t.apply(&mut out);
        let tp = out.get(TRACEPARENT).unwrap().to_str().unwrap();
        assert!(parse_traceparent(tp).is_some());
        assert!(tp.ends_with("-01"));
        // A tracestate without its traceparent is dropped.
        assert!(out.get(TRACESTATE).is_none());
        // Two traceparents are no trace context.
        let mut two = HeaderMap::new();
        two.append(TRACEPARENT, HeaderValue::from_static(TP));
        two.append(TRACEPARENT, HeaderValue::from_static(TP));
        assert!(TraceContext::from_headers(&two).parent_span_id.is_none());
    }
}
