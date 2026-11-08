// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! An upstream's gzip answer, unpacked as it streams, so that it can be
//! searched for the binding's credential ([`crate::redact`]). A redacted
//! request asks for `identity`, but some servers send gzip anyway; the pod
//! gets the answer unpacked, as a server may send it.
//!
//! A chunk is unpacked a step at a time: each step takes as much of it as
//! gives at most a buffer of output ([`STEP`]), so that what the proxy
//! holds stays small however much a chunk unpacks to (a bomb only costs
//! the pod's own bandwidth, as the answer streams at the pod's pace).

use std::io::{self, Write};

use bytes::{Buf, Bytes};
use flate2::write::MultiGzDecoder;

/// About the most output one step gives: flate2's own buffer.
pub const STEP: usize = 32 * 1024;

pub struct Gunzip {
    dec: MultiGzDecoder<Vec<u8>>,
    /// The chunk's part not unpacked yet.
    pending: Bytes,
}

impl Default for Gunzip {
    fn default() -> Gunzip {
        Gunzip {
            dec: MultiGzDecoder::new(Vec::with_capacity(STEP)),
            pending: Bytes::new(),
        }
    }
}

impl Gunzip {
    /// Whether a chunk waits to be unpacked: [`Gunzip::step`] before the
    /// next chunk.
    pub fn busy(&self) -> bool {
        !self.pending.is_empty()
    }

    /// Takes the next chunk, to be unpacked by [`Gunzip::step`].
    pub fn feed(&mut self, chunk: Bytes) {
        debug_assert!(!self.busy());
        self.pending = chunk;
    }

    /// Unpacks the next part of the chunk; returns what it gave, which may
    /// be nothing. When the chunk is done, what it gave is all out.
    pub fn step(&mut self) -> io::Result<Bytes> {
        let n = self.dec.write(&self.pending)?;
        if n == 0 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "data after the gzip stream's end",
            ));
        }
        self.pending.advance(n);
        if self.pending.is_empty() {
            self.dec.flush()?;
        }
        Ok(self.take())
    }

    /// The stream's end: the rest, or an error if the gzip stream is cut.
    pub fn finish(&mut self) -> io::Result<Bytes> {
        debug_assert!(!self.busy());
        self.dec.try_finish()?;
        Ok(self.take())
    }

    fn take(&mut self) -> Bytes {
        Bytes::from(std::mem::replace(
            self.dec.get_mut(),
            Vec::with_capacity(STEP),
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use flate2::write::GzEncoder;
    use flate2::Compression;

    pub fn gzip(data: &[u8]) -> Vec<u8> {
        let mut e = GzEncoder::new(Vec::new(), Compression::default());
        e.write_all(data).unwrap();
        e.finish().unwrap()
    }

    /// Unpacks `chunks`; returns the output and the largest piece.
    fn unpack(chunks: &[&[u8]]) -> io::Result<(Vec<u8>, usize)> {
        let mut g = Gunzip::default();
        let (mut out, mut largest) = (Vec::new(), 0);
        for c in chunks {
            g.feed(Bytes::copy_from_slice(c));
            while g.busy() {
                let b = g.step()?;
                largest = largest.max(b.len());
                out.extend_from_slice(&b);
            }
        }
        let b = g.finish()?;
        largest = largest.max(b.len());
        out.extend_from_slice(&b);
        Ok((out, largest))
    }

    #[test]
    fn unpacks_across_chunks() {
        let text = b"{\"value\":[{\"PurchaseOrder\":\"4500000101\"}]}".repeat(500);
        let z = gzip(&text);
        for size in [1, 7, 100, z.len()] {
            let chunks: Vec<&[u8]> = z.chunks(size).collect();
            assert_eq!(unpack(&chunks).unwrap().0, text, "chunks of {size}");
        }
        // Two members, as a server may send: one after the other.
        let two = [gzip(b"one "), gzip(b"two")].concat();
        assert_eq!(unpack(&[&two]).unwrap().0, b"one two");
    }

    #[test]
    fn a_bomb_comes_out_a_step_at_a_time() {
        let zeros = vec![0u8; 64 << 20];
        let z = gzip(&zeros);
        assert!(z.len() < 128 << 10, "{}", z.len());
        let (out, largest) = unpack(&[&z]).unwrap();
        assert_eq!(out.len(), zeros.len());
        assert!(largest <= 2 * STEP, "{largest}");
    }

    #[test]
    fn a_cut_or_broken_stream_is_an_error() {
        let z = gzip(&b"abc".repeat(1000));
        assert!(unpack(&[&z[..z.len() - 4]]).is_err());
        assert!(unpack(&[b"not gzip at all"]).is_err());
        let mut tail = z.clone();
        tail.extend_from_slice(b"junk");
        assert!(unpack(&[&tail]).is_err());
    }
}
