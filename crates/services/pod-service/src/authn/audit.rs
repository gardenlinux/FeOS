// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's `feos-authn` audit file, `<pods>/<id>/authn-audit.ndjson`, read
//! by `feosd` two ways (ADR 0014, "Audit"; ADR 0028 §6):
//!
//! - as a log, as a guest's container logs are read
//!   (`StreamContainerLogs`, [`follow`]): the lines a filter keeps, each
//!   with its offset in the file and the offset past it, under the file's
//!   ID ([`log_id`]), from an offset the sink has, so that the log shipper
//!   follows it from where the sink is;
//! - from an offset `feosd` keeps beside it (`authn-audit.offset`, the
//!   file's inode and the offset), in batches, to a handler that publishes
//!   them ([`tail`]): the offset moves once the handler took a batch.
//!
//! Lines are read whole: a line that is not ended yet waits, and one longer
//! than [`MAX_LINE`] is skipped.
//!
//! The file is the process's to append to as long as it runs, so it is not
//! renamed: once it holds more than [`MAX_FILE`] on disk, `tail` frees
//! what it has read, but the last [`KEEP`] bytes, by punching a hole over
//! the file's head ([`compact`]). Every offset stays what it was; a hole
//! reads as zeros, which the readers skip. A tail more than [`MAX_FILE`]
//! behind skips to the last [`KEEP`] bytes, and says so.

use super::AUDIT;
use feos_proto::container_service::{log_entry::Source, LogEntry, StreamContainerLogsRequest};
use futures::future::BoxFuture;
use log::{debug, warn};
use nix::errno::Errno;
use nix::fcntl::{fallocate, FallocateFlags};
use nix::unistd::{lseek, Whence};
use std::io::{self, SeekFrom};
use std::os::fd::AsFd;
use std::os::unix::fs::{FileExt, MetadataExt};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, SystemTime};
use tokio::io::{AsyncReadExt, AsyncSeekExt};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::Status;

/// The file's ID, and the offset `tail` keeps.
const ID: &str = "authn-audit.id";
const OFFSET: &str = "authn-audit.offset";
/// A line's most bytes.
pub const MAX_LINE: usize = 1 << 20;
/// How often the end of a file is looked past.
const POLL: Duration = Duration::from_millis(200);
/// The most a tail without follow reads of a file's end.
const TAIL_WINDOW: u64 = 4 << 20;
/// A batch's most lines and bytes.
const BATCH_LINES: usize = 512;
const BATCH_BYTES: usize = 1 << 20;
/// How often `tail` looks at the files.
const TAIL_PERIOD: Duration = Duration::from_secs(1);
/// A pod's batches in a pass of `tail`; then the next pod's turn.
const BATCHES_PER_PASS: usize = 8;
/// The most of a pod's audit file kept on disk; past it, what was read is
/// freed.
pub const MAX_FILE: u64 = 64 << 20;
/// What a compaction keeps of what was read: the egress log follows the
/// file at its own pace.
pub const KEEP: u64 = 16 << 20;

/// Which lines are read: those of `"type":"http"`.
pub type Keep = Arc<dyn Fn(&[u8]) -> bool + Send + Sync>;

/// The ID of the audit file in the pod's directory `dir`: made with the
/// file, so that a file made again starts its offsets under another.
pub fn log_id(dir: &Path) -> io::Result<String> {
    let path = dir.join(ID);
    match std::fs::read_to_string(&path) {
        Ok(id) if !id.trim().is_empty() => return Ok(id.trim().to_string()),
        Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e),
        _ => {}
    }
    std::fs::create_dir_all(dir)?;
    let id = uuid::Uuid::new_v4().to_string();
    let temporary = dir.join(format!("{ID}.new"));
    std::fs::write(&temporary, &id)?;
    std::fs::rename(temporary, path)?;
    Ok(id)
}

/// The audit file of the pod's directory `dir` as a log, as
/// `StreamContainerLogs` answers `request`: the last `tail_lines` lines
/// kept without `follow`; with it, from `from_offset` of the log
/// `log_id` names (0 for another log, or past its end), from the start
/// with `tail`, else from its end, until the stream is dropped.
pub fn follow(
    dir: PathBuf,
    request: StreamContainerLogsRequest,
    keep: Keep,
) -> ReceiverStream<Result<LogEntry, Status>> {
    let (sender, receiver) = mpsc::channel(64);
    tokio::spawn(async move {
        let result = match request.follow {
            true => follow_from(&dir, &request, &keep, &sender).await,
            false => send_tail(&dir, &request, &keep, &sender).await,
        };
        if let Err(e) = result {
            let _ = sender
                .send(Err(Status::internal(format!("the audit file: {e}"))))
                .await;
        }
    });
    ReceiverStream::new(receiver)
}

fn entry(line: &[u8], offset: u64, id: &str) -> LogEntry {
    LogEntry {
        line: line.to_vec(),
        source: Source::Stdout as i32,
        offset,
        next_offset: offset + line.len() as u64 + 1,
        time_unix_nano: SystemTime::now()
            .duration_since(SystemTime::UNIX_EPOCH)
            .map_or(0, |since| {
                i64::try_from(since.as_nanos()).unwrap_or(i64::MAX)
            }),
        partial: false,
        log_id: id.to_string(),
    }
}

async fn send_tail(
    dir: &Path,
    request: &StreamContainerLogsRequest,
    keep: &Keep,
    sender: &mpsc::Sender<Result<LogEntry, Status>>,
) -> io::Result<()> {
    let count = match (request.tail_lines, request.tail) {
        (0, true) => usize::MAX,
        (0, false) => return Ok(()),
        (lines, _) => lines as usize,
    };
    let mut file = match tokio::fs::File::open(dir.join(AUDIT)).await {
        Ok(file) => file,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e),
    };
    let id = log_id(dir)?;
    let len = file.metadata().await?.len();
    let start = len.saturating_sub(TAIL_WINDOW);
    file.seek(SeekFrom::Start(start)).await?;
    let mut bytes = Vec::new();
    file.read_to_end(&mut bytes).await?;
    let mut lines = Vec::new();
    let mut offset = start;
    let mut rest = bytes.as_slice();
    // A window that starts inside a line skips it.
    if start > 0
        && let Some(end) = rest.iter().position(|b| *b == b'\n')
    {
        offset += end as u64 + 1;
        rest = &rest[end + 1..];
    }
    while let Some(end) = rest.iter().position(|b| *b == b'\n') {
        let line = &rest[..end];
        if line.len() <= MAX_LINE && keep(line) {
            lines.push(entry(line, offset, &id));
        }
        offset += end as u64 + 1;
        rest = &rest[end + 1..];
    }
    let skip = lines.len().saturating_sub(count);
    for entry in lines.into_iter().skip(skip) {
        if sender.send(Ok(entry)).await.is_err() {
            break;
        }
    }
    Ok(())
}

async fn follow_from(
    dir: &Path,
    request: &StreamContainerLogsRequest,
    keep: &Keep,
    sender: &mpsc::Sender<Result<LogEntry, Status>>,
) -> io::Result<()> {
    let path = dir.join(AUDIT);
    // A log that is not there yet is waited for.
    let mut file = loop {
        match tokio::fs::File::open(&path).await {
            Ok(file) => break file,
            Err(e) if e.kind() == io::ErrorKind::NotFound => {
                if sender.is_closed() {
                    return Ok(());
                }
                tokio::time::sleep(POLL).await;
            }
            Err(e) => return Err(e),
        }
    };
    let id = log_id(dir)?;
    let len = file.metadata().await?.len();
    let mut offset = match (request.from_offset, request.tail) {
        (0, true) => 0,
        (0, false) => len,
        // Of this log, and within it.
        (from, _) if (request.log_id.is_empty() || request.log_id == id) && from <= len => from,
        _ => 0,
    };
    offset = past_hole(&file, offset, len);
    file.seek(SeekFrom::Start(offset)).await?;
    let mut pending: Vec<u8> = Vec::new();
    let mut skipping = false;
    let mut chunk = vec![0u8; 64 << 10];
    loop {
        let read = file.read(&mut chunk).await?;
        if read == 0 {
            if sender.is_closed() {
                return Ok(());
            }
            tokio::time::sleep(POLL).await;
            continue;
        }
        let mut data = &chunk[..read];
        if pending.is_empty() && !skipping {
            // What a compaction freed under the reader.
            let zeros = data.iter().take_while(|b| **b == 0).count();
            offset += zeros as u64;
            data = &data[zeros..];
        }
        pending.extend_from_slice(data);
        let mut consumed = 0;
        while let Some(end) = pending[consumed..].iter().position(|b| *b == b'\n') {
            let line = &pending[consumed..consumed + end];
            if !skipping && keep(line) {
                let entry = entry(line, offset, &id);
                if sender.send(Ok(entry)).await.is_err() {
                    return Ok(());
                }
            }
            skipping = false;
            offset += end as u64 + 1;
            consumed += end + 1;
        }
        pending.drain(..consumed);
        if pending.len() > MAX_LINE {
            // The rest of that line goes too.
            debug!("Audit: Skipping a line longer than {MAX_LINE} bytes");
            offset += pending.len() as u64;
            pending.clear();
            skipping = true;
        }
    }
}

/// Takes a batch of a pod's lines; true once they are published.
pub type Handler = Arc<dyn Fn(String, Vec<Vec<u8>>) -> BoxFuture<'static, bool> + Send + Sync>;

/// Follows the audit files of the pods `pods` names below `dir` from the
/// offsets kept beside them, for as long as the process runs: each new
/// batch of the lines `keep` keeps goes to `handler`, and the offset
/// moves past it once the handler took it, and past the lines not kept.
pub async fn tail(
    dir: PathBuf,
    pods: Arc<dyn Fn() -> Vec<String> + Send + Sync>,
    keep: Keep,
    handler: Handler,
) {
    // Which pod goes first, moved on every pass.
    let mut first = 0usize;
    loop {
        let listed = pods.clone();
        let ids = tokio::task::spawn_blocking(move || listed())
            .await
            .unwrap_or_default();
        let mut more = false;
        for turn in 0..ids.len() {
            let pod_id = &ids[(first + turn) % ids.len()];
            let pod_dir = dir.join(pod_id);
            let mut batches = 0;
            loop {
                if batches == BATCHES_PER_PASS {
                    more = true;
                    break;
                }
                batches += 1;
                match read_batch(&pod_dir, &keep).await {
                    Ok(None) => break,
                    Ok(Some((lines, end, inode))) => {
                        if !lines.is_empty() && !handler(pod_id.clone(), lines).await {
                            break;
                        }
                        if let Err(e) = store_offset(&pod_dir, inode, end).await {
                            warn!("Audit: Pod {pod_id}'s offset is not kept: {e}");
                            break;
                        }
                    }
                    Err(e) => {
                        debug!("Audit: Pod {pod_id}'s audit file: {e}");
                        break;
                    }
                }
            }
            if let Err(e) = compact(&pod_dir, MAX_FILE, KEEP).await {
                warn!("Audit: Pod {pod_id}'s audit file is not compacted: {e}");
            }
        }
        first = first.wrapping_add(1);
        match more {
            // A pod with more to read goes on at once, after the others.
            true => tokio::task::yield_now().await,
            false => tokio::time::sleep(TAIL_PERIOD).await,
        }
    }
}

/// Frees what was read of a pod's audit file in `dir` once it holds more
/// than `max` bytes on disk: a hole over its head, up to the kept offset
/// but the last `keep` bytes' lines; the file keeps its length and its
/// offsets. A kept offset more than `max` behind its end moves to those
/// lines first: what lay between is dropped, and said.
async fn compact(dir: &Path, max: u64, keep: u64) -> io::Result<()> {
    let path = dir.join(AUDIT);
    let meta = match tokio::fs::metadata(&path).await {
        Ok(meta) => meta,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e),
    };
    if meta.blocks() * 512 <= max {
        return Ok(());
    }
    let (inode, len) = (meta.ino(), meta.len());
    let offset = match load_offset(dir).await {
        Some((kept, offset)) if kept == inode && offset <= len => offset,
        _ => 0,
    };
    let from = len.saturating_sub(keep);
    let (cut, dropped) = tokio::task::spawn_blocking(move || {
        let file = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(&path)?;
        if file.metadata()?.ino() != inode {
            return Ok((0, false));
        }
        let (cut, dropped) = match offset {
            // Read: freed up to the kept lines.
            offset if offset > from => (line_start(&file, from)?.min(offset), false),
            // Unread past the cap: dropped up to the kept lines.
            offset if len - offset > max => (line_start(&file, from)?, true),
            offset => (offset, false),
        };
        if cut > 0 {
            let flags = FallocateFlags::FALLOC_FL_PUNCH_HOLE | FallocateFlags::FALLOC_FL_KEEP_SIZE;
            let cut_at = i64::try_from(cut).map_err(io::Error::other)?;
            fallocate(&file, flags, 0, cut_at)?;
        }
        Ok::<_, io::Error>((cut, dropped))
    })
    .await
    .map_err(io::Error::other)??;
    if dropped {
        warn!(
            "Audit: {} bytes of records in {} were not read and are dropped: the file keeps \
             {max} bytes at most",
            cut - offset,
            dir.join(AUDIT).display()
        );
        store_offset(dir, inode, cut).await?;
    }
    Ok(())
}

/// The offset of the first line that starts at or past `from`; past the
/// longest a line may be, `from` and that length.
fn line_start(file: &std::fs::File, from: u64) -> io::Result<u64> {
    if from == 0 {
        return Ok(0);
    }
    let mut bytes = vec![0u8; MAX_LINE + 1];
    let read = file.read_at(&mut bytes, from - 1)?;
    Ok(match bytes[..read].iter().position(|b| *b == b'\n') {
        Some(at) => from + at as u64,
        None => from - 1 + read as u64,
    })
}

/// The first offset at or past `offset` that holds data, below `len`: a
/// compaction's hole is passed over, not read.
fn past_hole(file: &impl AsFd, offset: u64, len: u64) -> u64 {
    let Ok(at) = i64::try_from(offset) else {
        return offset;
    };
    match lseek(file.as_fd(), at, Whence::SeekData) {
        Ok(data) => u64::try_from(data)
            .unwrap_or(offset)
            .clamp(offset, len.max(offset)),
        Err(Errno::ENXIO) => len.max(offset),
        Err(_) => offset,
    }
}

/// The next batch of a pod's lines past its kept offset, the offset past
/// them and the file's inode; None at the end or without a file.
async fn read_batch(dir: &Path, keep: &Keep) -> io::Result<Option<(Vec<Vec<u8>>, u64, u64)>> {
    let mut file = match tokio::fs::File::open(dir.join(AUDIT)).await {
        Ok(file) => file,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e),
    };
    let meta = file.metadata().await?;
    let (inode, len) = (meta.ino(), meta.len());
    let offset = match load_offset(dir).await {
        Some((kept, offset)) if kept == inode && offset <= len => offset,
        _ => 0,
    };
    let offset = past_hole(&file, offset, len);
    if offset >= len {
        return Ok(None);
    }
    file.seek(SeekFrom::Start(offset)).await?;
    let want = usize::try_from((len - offset).min(BATCH_BYTES as u64 + MAX_LINE as u64))
        .unwrap_or(BATCH_BYTES);
    let mut bytes = vec![0u8; want];
    let read = file.read(&mut bytes).await?;
    bytes.truncate(read);
    // The rest of a hole's last block.
    let zeros = bytes.iter().take_while(|b| **b == 0).count();
    let offset = offset + zeros as u64;
    let (mut lines, mut end, mut size) = (Vec::new(), offset, 0);
    let mut rest = &bytes[zeros..];
    while let Some(at) = rest.iter().position(|b| *b == b'\n') {
        let line = &rest[..at];
        if line.len() <= MAX_LINE && keep(line) {
            size += line.len();
            lines.push(line.to_vec());
        }
        end += at as u64 + 1;
        rest = &rest[at + 1..];
        if lines.len() >= BATCH_LINES || size >= BATCH_BYTES {
            break;
        }
    }
    if end == offset {
        // An unfinished line; one beyond the longest a line may be goes.
        if rest.len() > MAX_LINE {
            return Ok(Some((Vec::new(), offset + rest.len() as u64, inode)));
        }
        if zeros > 0 {
            return Ok(Some((Vec::new(), offset, inode)));
        }
        return Ok(None);
    }
    Ok(Some((lines, end, inode)))
}

async fn load_offset(dir: &Path) -> Option<(u64, u64)> {
    let text = tokio::fs::read_to_string(dir.join(OFFSET)).await.ok()?;
    let (inode, offset) = text.trim().split_once(' ')?;
    Some((inode.parse().ok()?, offset.parse().ok()?))
}

async fn store_offset(dir: &Path, inode: u64, offset: u64) -> io::Result<()> {
    let temporary = dir.join(format!("{OFFSET}.new"));
    tokio::fs::write(&temporary, format!("{inode} {offset}\n")).await?;
    tokio::fs::rename(temporary, dir.join(OFFSET)).await
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use std::sync::Mutex;
    use tokio_stream::StreamExt;

    fn http(line: &[u8]) -> bool {
        line.windows(13).any(|w| w == br#""type":"http""#)
    }

    fn keep() -> Keep {
        Arc::new(http)
    }

    fn append(dir: &Path, text: &str) {
        let mut file = std::fs::OpenOptions::new()
            .append(true)
            .create(true)
            .open(dir.join(AUDIT))
            .unwrap();
        file.write_all(text.as_bytes()).unwrap();
    }

    const A: &str = "{\"type\":\"http\",\"n\":1}\n";
    const TOKEN: &str = "{\"type\":\"token\"}\n";
    const B: &str = "{\"type\":\"http\",\"n\":2}\n";

    async fn next(stream: &mut ReceiverStream<Result<LogEntry, Status>>) -> LogEntry {
        tokio::time::timeout(Duration::from_secs(5), stream.next())
            .await
            .unwrap()
            .unwrap()
            .unwrap()
    }

    #[tokio::test]
    async fn the_file_is_a_log_of_its_http_records_with_offsets_under_its_id() {
        let dir = tempfile::tempdir().unwrap();
        // Followed before the file is there, from its start.
        let mut stream = follow(
            dir.path().to_path_buf(),
            StreamContainerLogsRequest {
                follow: true,
                tail: true,
                ..Default::default()
            },
            keep(),
        );
        append(dir.path(), &format!("{A}{TOKEN}"));
        let first = next(&mut stream).await;
        assert_eq!(first.line, A.trim_end().as_bytes());
        assert_eq!((first.offset, first.next_offset), (0, A.len() as u64));
        let id = log_id(dir.path()).unwrap();
        assert_eq!(first.log_id, id);
        // An unfinished line waits for its end.
        append(dir.path(), "{\"type\":\"http\",\"n\":2}");
        append(dir.path(), "\n");
        let second = next(&mut stream).await;
        let at = (A.len() + TOKEN.len()) as u64;
        assert_eq!(
            (second.line.as_slice(), second.offset),
            (B.trim_end().as_bytes(), at)
        );
        drop(stream);

        // The shipper's probe: the last line, and its ID.
        let mut probe = follow(
            dir.path().to_path_buf(),
            StreamContainerLogsRequest {
                tail_lines: 1,
                ..Default::default()
            },
            keep(),
        );
        assert_eq!(next(&mut probe).await.offset, at);
        assert!(probe.next().await.is_none());

        // From where the sink is; another log's offset starts at 0.
        let from = |offset: u64, log_id: &str| {
            follow(
                dir.path().to_path_buf(),
                StreamContainerLogsRequest {
                    follow: true,
                    from_offset: offset,
                    log_id: log_id.to_string(),
                    ..Default::default()
                },
                keep(),
            )
        };
        let mut stream = from(A.len() as u64, &id);
        assert_eq!(next(&mut stream).await.offset, at);
        let mut stream = from(A.len() as u64, "another");
        assert_eq!(next(&mut stream).await.offset, 0);
        let mut stream = from(1 << 30, &id);
        assert_eq!(next(&mut stream).await.offset, 0);
    }

    #[tokio::test]
    async fn the_tail_publishes_batches_from_its_kept_offset() {
        let dir = tempfile::tempdir().unwrap();
        let pod_dir = dir.path().join("p");
        std::fs::create_dir(&pod_dir).unwrap();
        append(&pod_dir, &format!("{A}{TOKEN}"));
        type Taken = Vec<(String, Vec<Vec<u8>>)>;
        let taken: Arc<Mutex<Taken>> = Arc::default();
        let accept = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let handler: Handler = {
            let (taken, accept) = (taken.clone(), accept.clone());
            Arc::new(move |pod, lines| {
                let (taken, accept) = (taken.clone(), accept.clone());
                Box::pin(async move {
                    let ok = accept.load(std::sync::atomic::Ordering::SeqCst);
                    if ok {
                        taken.lock().unwrap().push((pod, lines));
                    }
                    ok
                })
            })
        };
        let pods: Arc<dyn Fn() -> Vec<String> + Send + Sync> =
            Arc::new(|| vec!["p".to_string(), "gone".to_string()]);
        let task = tokio::spawn(tail(
            dir.path().to_path_buf(),
            pods.clone(),
            keep(),
            handler.clone(),
        ));
        // Refused: the offset stays.
        tokio::time::sleep(Duration::from_millis(1500)).await;
        assert!(taken.lock().unwrap().is_empty());
        assert!(!pod_dir.join(OFFSET).exists());
        accept.store(true, std::sync::atomic::Ordering::SeqCst);
        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        while taken.lock().unwrap().is_empty() {
            assert!(tokio::time::Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        assert_eq!(
            taken.lock().unwrap()[0],
            ("p".to_string(), vec![A.trim_end().as_bytes().to_vec()])
        );
        append(&pod_dir, B);
        while taken.lock().unwrap().len() < 2 {
            assert!(tokio::time::Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        assert_eq!(
            taken.lock().unwrap()[1].1,
            [B.trim_end().as_bytes().to_vec()]
        );
        task.abort();

        // A tail that starts again goes on from the kept offset.
        let again = tokio::spawn(tail(dir.path().to_path_buf(), pods, keep(), handler));
        tokio::time::sleep(Duration::from_millis(1500)).await;
        assert_eq!(taken.lock().unwrap().len(), 2, "nothing twice");
        again.abort();
        let (inode, offset) = load_offset(&pod_dir).await.unwrap();
        assert_eq!(offset, (A.len() + TOKEN.len() + B.len()) as u64);
        assert_eq!(inode, std::fs::metadata(pod_dir.join(AUDIT)).unwrap().ino());
    }

    /// A busy pod's records take their turn: a quiet pod's batch goes
    /// before the busy one's ninth.
    #[tokio::test]
    async fn the_tail_takes_the_pods_in_turn() {
        let dir = tempfile::tempdir().unwrap();
        for pod in ["busy", "quiet"] {
            std::fs::create_dir(dir.path().join(pod)).unwrap();
        }
        let busy = A.repeat(BATCH_LINES * (BATCHES_PER_PASS + 2));
        append(&dir.path().join("busy"), &busy);
        append(&dir.path().join("quiet"), B);
        let taken: Arc<Mutex<Vec<String>>> = Arc::default();
        let handler: Handler = {
            let taken = taken.clone();
            Arc::new(move |pod, _lines| {
                taken.lock().unwrap().push(pod);
                Box::pin(async { true })
            })
        };
        let pods: Arc<dyn Fn() -> Vec<String> + Send + Sync> =
            Arc::new(|| vec!["busy".to_string(), "quiet".to_string()]);
        let task = tokio::spawn(tail(dir.path().to_path_buf(), pods, keep(), handler));
        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        while taken.lock().unwrap().len() < BATCHES_PER_PASS + 3 {
            assert!(tokio::time::Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        task.abort();
        let taken = taken.lock().unwrap().clone();
        let quiet = taken.iter().position(|pod| pod == "quiet").unwrap();
        assert_eq!(quiet, BATCHES_PER_PASS, "{taken:?}");
        assert_eq!(
            taken.iter().filter(|pod| *pod == "busy").count(),
            BATCHES_PER_PASS + 2
        );
    }

    fn allocated(path: &Path) -> u64 {
        std::fs::metadata(path).unwrap().blocks() * 512
    }

    /// Past its cap the file's head is freed, its length and offsets kept;
    /// its readers pass over the hole. A tail too far behind skips ahead.
    #[tokio::test]
    async fn a_file_past_its_cap_frees_what_was_read() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(AUDIT);
        let (max, keep_bytes) = (1u64 << 20, 256u64 << 10);
        // Lines of 1000 bytes, numbered.
        let line = |n: usize| {
            let head = format!("{{\"type\":\"http\",\"n\":{n},\"pad\":\"");
            format!("{head}{}\"}}\n", "x".repeat(1000 - head.len() - 3))
        };
        let lines: String = (0..3000).map(line).collect();
        append(dir.path(), &lines);
        let len = lines.len() as u64;
        let inode = std::fs::metadata(&path).unwrap().ino();
        // Under its cap, or nothing read: nothing is freed.
        compact(dir.path(), 64 << 20, keep_bytes).await.unwrap();
        compact(dir.path(), 4 << 20, keep_bytes).await.unwrap();
        assert!(allocated(&path) >= len);

        store_offset(dir.path(), inode, len).await.unwrap();
        compact(dir.path(), max, keep_bytes).await.unwrap();
        let meta = std::fs::metadata(&path).unwrap();
        assert_eq!((meta.ino(), meta.len()), (inode, len), "the same file");
        assert!(
            allocated(&path) <= keep_bytes + (64 << 10),
            "{}",
            allocated(&path)
        );
        assert_eq!(load_offset(dir.path()).await, Some((inode, len)));
        let bytes = std::fs::read(&path).unwrap();
        let kept = bytes.iter().position(|b| *b != 0).unwrap();
        assert_eq!(kept % 1000, 0, "a line's start");
        assert!(len - kept as u64 >= keep_bytes - 1000);

        // The egress log from the start: the kept lines, whole, at their
        // offsets.
        let mut stream = follow(
            dir.path().to_path_buf(),
            StreamContainerLogsRequest {
                follow: true,
                tail: true,
                ..Default::default()
            },
            keep(),
        );
        let first = next(&mut stream).await;
        assert_eq!(first.offset, kept as u64);
        assert_eq!(first.line, line(kept / 1000).trim_end().as_bytes());
        drop(stream);
        // A tail whose offset is gone starts at the kept lines too.
        tokio::fs::remove_file(dir.path().join(OFFSET))
            .await
            .unwrap();
        let mut batch = read_batch(dir.path(), &keep()).await.unwrap().unwrap();
        while batch.0.is_empty() {
            store_offset(dir.path(), inode, batch.1).await.unwrap();
            batch = read_batch(dir.path(), &keep()).await.unwrap().unwrap();
        }
        assert_eq!(batch.0[0], line(kept / 1000).trim_end().as_bytes());

        // Unread past the cap: the oldest are dropped, the offset moves.
        store_offset(dir.path(), inode, len).await.unwrap();
        let more: String = (3000..5000).map(line).collect();
        append(dir.path(), &more);
        let len = len + more.len() as u64;
        compact(dir.path(), max, keep_bytes).await.unwrap();
        let (_, offset) = load_offset(dir.path()).await.unwrap();
        assert!(offset > len - keep_bytes - 1000 && offset <= len - keep_bytes + 1000);
        assert_eq!(offset % 1000, 0, "a line's start");
        assert!(allocated(&path) <= keep_bytes + (64 << 10));
        let batch = read_batch(dir.path(), &keep()).await.unwrap().unwrap();
        assert_eq!(
            batch.0[0],
            line(offset as usize / 1000).trim_end().as_bytes()
        );
    }
}
