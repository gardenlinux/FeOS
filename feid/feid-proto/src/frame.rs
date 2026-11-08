// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Frames: one compact JSON object and `\n`. JSON as serde_json writes it
//! has no raw newline, so a line is a frame. The cap is enforced while
//! reading: a flood without a newline is cut off at [`MAX_FRAME`], never
//! held whole. A `\r` before the `\n` is dropped, for frames typed by
//! hand into a socket.

use std::io::{self, BufRead};

use serde::Serialize;
use tokio::io::{AsyncBufRead, AsyncBufReadExt};

use crate::MAX_FRAME;

/// Why no frame came.
#[derive(Debug)]
pub enum ReadError {
    /// The stream ended between frames.
    Closed,
    /// The stream ended inside a frame.
    Truncated,
    /// More than the cap without a newline; the stream cannot be read on.
    TooLarge,
    Io(io::Error),
}

/// Appends what `available` holds of the frame to `out`; true once the
/// frame is whole. `Err` past the cap.
fn take(out: &mut Vec<u8>, available: &[u8], max: usize) -> Result<(bool, usize), ReadError> {
    let (done, n) = match available.iter().position(|&b| b == b'\n') {
        Some(i) => (true, i + 1),
        None => (false, available.len()),
    };
    // The newline is not counted against the cap.
    if out.len() + n - usize::from(done) > max {
        return Err(ReadError::TooLarge);
    }
    out.extend_from_slice(&available[..n]);
    Ok((done, n))
}

fn strip(mut frame: Vec<u8>) -> Vec<u8> {
    if frame.last() == Some(&b'\n') {
        frame.pop();
        if frame.last() == Some(&b'\r') {
            frame.pop();
        }
    }
    frame
}

/// Reads one frame, without its newline, from a blocking stream.
pub fn read_blocking<R: BufRead>(r: &mut R, max: usize) -> Result<Vec<u8>, ReadError> {
    let mut out = Vec::new();
    loop {
        let available = match r.fill_buf() {
            Ok(a) => a,
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(ReadError::Io(e)),
        };
        if available.is_empty() {
            return Err(match out.is_empty() {
                true => ReadError::Closed,
                false => ReadError::Truncated,
            });
        }
        let (done, n) = take(&mut out, available, max)?;
        r.consume(n);
        if done {
            return Ok(strip(out));
        }
    }
}

/// Reads one frame, without its newline, from a tokio stream.
pub async fn read<R: AsyncBufRead + Unpin>(r: &mut R, max: usize) -> Result<Vec<u8>, ReadError> {
    let mut out = Vec::new();
    loop {
        let available = r.fill_buf().await.map_err(ReadError::Io)?;
        if available.is_empty() {
            return Err(match out.is_empty() {
                true => ReadError::Closed,
                false => ReadError::Truncated,
            });
        }
        let (done, n) = take(&mut out, available, max)?;
        r.consume(n);
        if done {
            return Ok(strip(out));
        }
    }
}

/// A frame as it is written: compact JSON and `\n`. None past the cap.
pub fn encode<T: Serialize>(frame: &T) -> Option<Vec<u8>> {
    let mut bytes = match serde_json::to_vec(frame) {
        Ok(b) => b,
        Err(e) => {
            log::error!("feid: a frame that does not serialize: {e}");
            return None;
        }
    };
    if bytes.len() > MAX_FRAME {
        return None;
    }
    bytes.push(b'\n');
    Some(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn frames_are_lines() {
        let mut input: &[u8] = b"{\"a\":1}\n{\"b\":2}\r\n{\"c\"";
        assert_eq!(read_blocking(&mut input, 64).unwrap(), b"{\"a\":1}");
        assert_eq!(read_blocking(&mut input, 64).unwrap(), b"{\"b\":2}");
        assert!(matches!(
            read_blocking(&mut input, 64),
            Err(ReadError::Truncated)
        ));
        let mut empty: &[u8] = b"";
        assert!(matches!(
            read_blocking(&mut empty, 64),
            Err(ReadError::Closed)
        ));
    }

    #[test]
    fn the_cap_counts_the_frame_not_its_newline() {
        let mut exact: &[u8] = b"12345678\n";
        assert_eq!(read_blocking(&mut exact, 8).unwrap(), b"12345678");
        let mut over: &[u8] = b"123456789\n";
        assert!(matches!(
            read_blocking(&mut over, 8),
            Err(ReadError::TooLarge)
        ));
        // A flood is cut off at the cap, before its end is in.
        let flood = vec![b'x'; 4 * 8192];
        let mut reader = io::BufReader::with_capacity(16, &flood[..]);
        assert!(matches!(
            read_blocking(&mut reader, 100),
            Err(ReadError::TooLarge)
        ));
        assert!(flood.len() - reader.get_ref().len() <= 100 + 16);
    }

    #[tokio::test]
    async fn the_async_reader_reads_the_same() {
        let mut input: &[u8] = b"{\"a\":1}\n12345678\n123456789\n";
        assert_eq!(read(&mut input, 8).await.unwrap(), b"{\"a\":1}");
        assert_eq!(read(&mut input, 8).await.unwrap(), b"12345678");
        assert!(matches!(
            read(&mut input, 8).await,
            Err(ReadError::TooLarge)
        ));
    }
}
