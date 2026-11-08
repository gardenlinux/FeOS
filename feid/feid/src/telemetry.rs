// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Handshake reports, sent to the signer by a thread of their own over a
//! connection of their own, so that a report never waits in a handshake.
//! The queue is bounded: when the signer does not keep up, new reports
//! are dropped and counted. A report is one-way; one lost costs the
//! control plane's join a side, never a connection.

use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender, TrySendError};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use feid_proto::HandshakeReport;

const QUEUE: usize = 1024;
const WRITE_TIMEOUT: Duration = Duration::from_secs(2);

pub(crate) struct Queue {
    socket: PathBuf,
    name: String,
    sender: OnceLock<SyncSender<HandshakeReport>>,
    /// Starting the thread once.
    start: Mutex<()>,
    dropped: Arc<AtomicU64>,
}

impl Queue {
    pub(crate) fn new(socket: PathBuf, name: String) -> Queue {
        Queue {
            socket,
            name,
            sender: OnceLock::new(),
            start: Mutex::new(()),
            dropped: Arc::new(AtomicU64::new(0)),
        }
    }

    pub(crate) fn push(&self, report: HandshakeReport) {
        let Some(sender) = self.sender() else {
            self.dropped.fetch_add(1, Ordering::Relaxed);
            return;
        };
        match sender.try_send(report) {
            Ok(()) => {}
            Err(TrySendError::Full(_)) | Err(TrySendError::Disconnected(_)) => {
                self.dropped.fetch_add(1, Ordering::Relaxed);
            }
        }
    }

    pub(crate) fn dropped(&self) -> u64 {
        self.dropped.load(Ordering::Relaxed)
    }

    fn sender(&self) -> Option<&SyncSender<HandshakeReport>> {
        if let Some(s) = self.sender.get() {
            return Some(s);
        }
        let _once = self.start.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(s) = self.sender.get() {
            return Some(s);
        }
        let (tx, rx) = sync_channel(QUEUE);
        let (socket, name, dropped) =
            (self.socket.clone(), self.name.clone(), self.dropped.clone());
        let started = std::thread::Builder::new()
            .name("feid-reports".into())
            .spawn(move || send_all(rx, socket, name, dropped));
        if let Err(e) = started {
            log::warn!("feid: no thread for handshake reports: {e}");
            return None;
        }
        let _ = self.sender.set(tx);
        self.sender.get()
    }
}

/// Sends what comes, dialing again when the connection broke; a report
/// that cannot be sent is dropped.
fn send_all(rx: Receiver<HandshakeReport>, socket: PathBuf, name: String, dropped: Arc<AtomicU64>) {
    let mut conn: Option<feid_proto::Client<UnixStream>> = None;
    for report in rx {
        if conn.is_none() {
            conn = UnixStream::connect(&socket)
                .ok()
                .filter(|s| {
                    s.set_write_timeout(Some(WRITE_TIMEOUT)).is_ok()
                        && s.set_read_timeout(Some(WRITE_TIMEOUT)).is_ok()
                })
                .and_then(|s| feid_proto::Client::connect(s, &name).ok());
        }
        let sent = conn.as_mut().is_some_and(|c| c.report(&report).is_ok());
        if !sent {
            conn = None;
            dropped.fetch_add(1, Ordering::Relaxed);
        }
    }
}
