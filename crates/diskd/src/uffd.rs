// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Guest RAM, read-through: a Firecracker UFFD memory backend served out
//! of the same chunk store the ublk disk reads through (ADR 0011
//! applied to the memory image).
//!
//! Before this, restoring a hibernated pod meant materialising the
//! whole memory layer — ~460 objects, 26–40 MiB on the wire, 6–7 s —
//! *before* Firecracker was allowed to load the snapshot. Nothing about
//! that download was on the guest's critical path except the fact that
//! we waited for it. Now the caller hands Firecracker a
//! `mem_backend { backend_type: "Uffd", backend_path: … }`, the VM
//! resumes immediately, and the pages arrive as the guest asks for them.
//!
//! Wire protocol (Firecracker v1.17, `vmm::persist::create_guest_memory`):
//! we bind the unix socket, Firecracker connects, `mmap`s the guest
//! memory anonymously, creates a userfaultfd, `UFFDIO_REGISTER`s every
//! region itself, and then sends us — in one `sendmsg` — a JSON
//! `Vec<GuestRegionUffdMapping>` body plus the uffd as `SCM_RIGHTS`
//! ancillary data. From there on the fd is ours to serve. Firecracker
//! keeps its own copy of the uffd, so a handler that dies leaves the
//! guest frozen rather than broken: see `fatal`, which takes the VMM
//! down instead (the upstream example's panic hook, minus the panic).
//!
//! Reuse, not a second stack (the whole point): a fault resolves to one
//! 256 KiB chunk of the flat memory layer, which is looked up in
//! `disk_store::manifest`, satisfied by `disk_store::cache::Cache::ensure_local`
//! (cache hit, else `disk_store::s3::get_chunk` → zstd → blake3 → admit), and
//! installed with `UFFDIO_COPY`. Absent and `zero` entries never touch
//! the wire at all — they are `UFFDIO_ZEROPAGE`.
//!
//! Shared-nothing like the rest of the daemon: no lock anywhere here.
//! Faults are sharded onto worker threads by chunk index, so the same
//! chunk is always handled by the same thread and two faults into one
//! chunk can never become two S3 GETs.

#![deny(clippy::disallowed_types)]
#![cfg_attr(not(test), deny(clippy::unwrap_used, clippy::expect_used))]

use std::io;
use std::mem::{size_of, size_of_val};
use std::os::unix::io::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{mpsc, Arc};

use disk_store::chunk::Hash;
use disk_store::manifest::{EntryKind, Manifest};
use serde::Deserialize;
use tracing::{error, info, warn};

// ---------------------------------------------------------------- uapi

// linux/userfaultfd.h. Hand-rolled rather than pulled in as a crate: it
// is three structs and three ioctl numbers, and the numbers are derived
// from the struct sizes below instead of being copied as magic.
const IOC_NRSHIFT: u32 = 0;
const IOC_TYPESHIFT: u32 = 8;
const IOC_SIZESHIFT: u32 = 16;
const IOC_DIRSHIFT: u32 = 30;
const IOC_WRITE: u32 = 1;
const IOC_READ: u32 = 2;
const UFFDIO: u32 = 0xAA;

const fn ioc(dir: u32, nr: u32, size: usize) -> libc::c_ulong {
    ((dir << IOC_DIRSHIFT)
        | (UFFDIO << IOC_TYPESHIFT)
        | (nr << IOC_NRSHIFT)
        | ((size as u32) << IOC_SIZESHIFT)) as libc::c_ulong
}

#[repr(C)]
#[derive(Clone, Copy, Default)]
struct UffdioRange {
    start: u64,
    len: u64,
}

#[repr(C)]
#[derive(Clone, Copy, Default)]
struct UffdioCopy {
    dst: u64,
    src: u64,
    len: u64,
    mode: u64,
    /// out: bytes copied, or -errno.
    copy: i64,
}

#[repr(C)]
#[derive(Clone, Copy, Default)]
struct UffdioZeropage {
    range: UffdioRange,
    mode: u64,
    /// out: bytes zeroed, or -errno.
    zeropage: i64,
}

const UFFDIO_WAKE: libc::Ioctl = ioc(IOC_READ, 0x02, size_of::<UffdioRange>()) as libc::Ioctl;
const UFFDIO_COPY: libc::Ioctl =
    ioc(IOC_READ | IOC_WRITE, 0x03, size_of::<UffdioCopy>()) as libc::Ioctl;
const UFFDIO_ZEROPAGE: libc::Ioctl =
    ioc(IOC_READ | IOC_WRITE, 0x04, size_of::<UffdioZeropage>()) as libc::Ioctl;

const UFFD_EVENT_PAGEFAULT: u8 = 0x12;
const UFFD_EVENT_REMOVE: u8 = 0x15;
const UFFD_EVENT_UNMAP: u8 = 0x16;

/// `struct uffd_msg`: 8-byte header, then a 24-byte union. Read straight
/// off the fd, so the layout has to be exact — asserted below.
#[repr(C)]
#[derive(Clone, Copy, Default)]
struct UffdMsg {
    event: u8,
    _reserved1: u8,
    _reserved2: u16,
    _reserved3: u32,
    /// pagefault: [flags, address, ptid]; remove/unmap: [start, end, _].
    arg: [u64; 3],
}

const _: () = assert!(size_of::<UffdMsg>() == 32);
const _: () = assert!(UFFDIO_COPY as u32 == 0xc028_aa03);
const _: () = assert!(UFFDIO_ZEROPAGE as u32 == 0xc020_aa04);

// ------------------------------------------------------------ handshake

/// One guest memory region as Firecracker describes it. `offset` is the
/// byte offset into the memory image — i.e. into the flat memory layer's
/// logical address space, which is what the manifest indexes.
#[derive(Debug, Clone, Deserialize)]
struct RegionMapping {
    base_host_virt_addr: u64,
    size: u64,
    offset: u64,
    /// Host page size for this region (4 KiB unless hugepages are on).
    page_size: u64,
}

impl RegionMapping {
    fn contains(&self, addr: u64) -> bool {
        addr >= self.base_host_virt_addr && addr < self.base_host_virt_addr + self.size
    }
}

/// Pages the guest has handed back since this handler attached.
///
/// The balloon's free page reporting turns into `MADV_DONTNEED` on the
/// host, the page goes missing again, and the guest refaults it the next
/// time anything walks that memory — `snapshot_create` above all, which
/// reads every page there is. Serving it from the snapshot would put dead
/// bytes back into a page nobody wants, and write them into the next
/// checkpoint.
///
/// That is not theoretical. Measured: the same guest, the
/// same 64 MiB written and then deleted, produced a 506.0 MiB memory
/// layer when it had been restored over UFFD against 144.0 MiB when it
/// had resumed from a local file — and the UFFD generation carried
/// *exactly* as much as the one before it, so deleting the data shrank
/// the checkpoint by nothing at all. It is a ratchet: every generation
/// keeps what any earlier one ever held.
///
/// A free page is what the kernel's zero page is for. Bookkeeping is one
/// bit per page of the image (16 KiB for a 512 MiB guest), indexed by
/// file offset so it is independent of where the regions sit.
struct RemovedPages {
    bits: Vec<AtomicU64>,
    page_size: u64,
    pages: u64,
}

impl RemovedPages {
    fn new(logical_size: u64, page_size: u64) -> Self {
        let pages = logical_size.div_ceil(page_size.max(1));
        let words = (pages as usize).div_ceil(64);
        RemovedPages {
            bits: (0..words).map(|_| AtomicU64::new(0)).collect(),
            page_size: page_size.max(1),
            pages,
        }
    }

    fn slot(&self, off: u64) -> Option<(usize, u64)> {
        let idx = off / self.page_size;
        if idx >= self.pages {
            return None;
        }
        Some(((idx / 64) as usize, 1u64 << (idx % 64)))
    }

    fn is_set(&self, off: u64) -> bool {
        match self.slot(off) {
            Some((w, b)) => self.bits[w].load(Ordering::Relaxed) & b != 0,
            None => false,
        }
    }

    /// Mark every page of a host-address range the guest just released.
    /// Returns how many pages were newly marked, for the counter.
    fn mark(&self, regions: &[RegionMapping], start: u64, end: u64) -> u64 {
        let mut n = 0;
        for r in regions {
            let lo = start.max(r.base_host_virt_addr);
            let hi = end.min(r.base_host_virt_addr + r.size);
            if lo >= hi {
                continue;
            }
            let off_lo = r.offset + (lo - r.base_host_virt_addr);
            let off_hi = r.offset + (hi - r.base_host_virt_addr);
            let mut off = off_lo - (off_lo % self.page_size);
            while off < off_hi {
                if let Some((w, b)) = self.slot(off)
                    && self.bits[w].fetch_or(b, Ordering::Relaxed) & b == 0
                {
                    n += 1;
                }
                off += self.page_size;
            }
        }
        n
    }

    /// The pages are present again after a successful serve; a later
    /// release fires another REMOVE and marks them anew.
    fn clear(&self, from: u64, to: u64) {
        let mut off = from;
        while off < to {
            if let Some((w, b)) = self.slot(off) {
                self.bits[w].fetch_and(!b, Ordering::Relaxed);
            }
            off += self.page_size;
        }
    }
}

// ---------------------------------------------------------------- stats

/// Everything the handler of one pod has done, for `Stats`. Written
/// by the worker threads, read by the control plane: atomics only, same
/// posture as `crate::stats::DevStats`.
#[derive(Default)]
pub struct MemStats {
    pub attached: AtomicBool,
    pub faults: AtomicU64,
    pub chunks_served: AtomicU64,
    pub chunks_zeroed: AtomicU64,
    pub bytes_served: AtomicU64,
    pub cache_hits: AtomicU64,
    pub cache_misses: AtomicU64,
    pub s3_fetches: AtomicU64,
    pub s3_fetch_bytes: AtomicU64,
    pub removes: AtomicU64,
    /// Pages served as the kernel's zero page because the guest had
    /// reported them free — not from the snapshot.
    pub pages_reclaimed: AtomicU64,
    pub errors: AtomicU64,
    pub chunks_total: AtomicU64,
}

// ---------------------------------------------------------------- setup

pub struct MemoryCtx {
    pub pod_id: String,
    pub socket: PathBuf,
    /// The flat memory layer (SealFile output: no parent, 256 KiB chunks).
    pub manifest: Manifest,
    pub cache: Arc<disk_store::cache::Cache>,
    /// Chunks the guest touches beyond the fault itself, fetched into the
    /// cache while it works on the one it asked for. 0 = strictly
    /// demand-driven.
    pub readahead: u32,
    pub fetch_concurrency: usize,
    pub stats: Arc<MemStats>,
}

pub struct StartedMemory {
    pub socket: PathBuf,
    pub stats: Arc<MemStats>,
    stop: Arc<AtomicBool>,
    wake: OwnedFd,
    join: std::thread::JoinHandle<()>,
}

/// Immutable per-pod state every worker reads.
struct Shared {
    pod_id: String,
    manifest: Manifest,
    cache: Arc<disk_store::cache::Cache>,
    stats: Arc<MemStats>,
    readahead: u32,
    uffd: OwnedFd,
    regions: Vec<RegionMapping>,
    /// Host page size, the granularity every fill has to be able to fall
    /// back to (all regions share it — asserted at handshake).
    page_size: u64,
    /// What the guest has given back and must not get handed again.
    removed: RemovedPages,
    /// Firecracker's pid (SO_PEERCRED), for logs only.
    peer_pid: libc::pid_t,
    /// A pidfd for that same process, opened at handshake time. This —
    /// not the pid — is what `fatal` signals: the number can be recycled
    /// while this handler lives, the fd cannot. `None` means the peer was
    /// already gone when we looked, and then there is nothing to kill.
    peer_pidfd: Option<libc::c_int>,
}

impl Drop for Shared {
    fn drop(&mut self) {
        if let Some(fd) = self.peer_pidfd {
            // SAFETY: an fd this struct has owned since the handshake.
            unsafe { libc::close(fd) };
        }
    }
}

/// Bind the backend socket and start serving. Returns as soon as the
/// socket exists — the caller's `PUT /snapshot/load` is what makes
/// Firecracker connect to it.
pub fn serve(ctx: MemoryCtx) -> Result<StartedMemory, String> {
    if ctx.manifest.parent.is_some() {
        return Err("memory layer has a parent: not a flat SealFile layer".into());
    }
    if let Some(dir) = ctx.socket.parent() {
        std::fs::create_dir_all(dir).map_err(|e| format!("{}: {e}", dir.display()))?;
    }
    let _ = std::fs::remove_file(&ctx.socket);
    let listener = UnixListener::bind(&ctx.socket)
        .map_err(|e| format!("bind {}: {e}", ctx.socket.display()))?;
    // Firecracker may run unprivileged next to a root diskd.
    std::fs::set_permissions(
        &ctx.socket,
        std::os::unix::fs::PermissionsExt::from_mode(0o666),
    )
    .map_err(|e| format!("chmod {}: {e}", ctx.socket.display()))?;

    ctx.stats.chunks_total.store(
        ctx.manifest
            .entries
            .iter()
            .filter(|e| e.kind == EntryKind::Data)
            .count() as u64,
        Ordering::Relaxed,
    );

    let stop = Arc::new(AtomicBool::new(false));
    let wake = eventfd_new()?;
    let wake_thread = wake.try_clone().map_err(|e| e.to_string())?;
    let stop_thread = stop.clone();
    let socket = ctx.socket.clone();
    let stats = ctx.stats.clone();
    let name = format!("diskd-uffd-{}", ctx.pod_id);
    let join = std::thread::Builder::new()
        .name(name)
        .spawn(move || handler_thread(ctx, listener, wake_thread, stop_thread))
        .map_err(|e| e.to_string())?;
    Ok(StartedMemory {
        socket,
        stats,
        stop,
        wake,
        join,
    })
}

/// Detach: the VMM is gone (pause, purge, destroy) or is about to be.
pub fn stop(m: StartedMemory) {
    m.stop.store(true, Ordering::SeqCst);
    notify(&m.wake);
    let _ = m.join.join();
    let _ = std::fs::remove_file(&m.socket);
}

// --------------------------------------------------------------- thread

fn handler_thread(ctx: MemoryCtx, listener: UnixListener, wake: OwnedFd, stop: Arc<AtomicBool>) {
    let socket = ctx.socket.clone();
    let pod_id = ctx.pod_id.clone();
    match run(ctx, listener, &wake, &stop) {
        Ok(()) => info!(pod = %pod_id, "uffd: handler detached"),
        Err(e) => error!(pod = %pod_id, error = %e, "uffd: handler stopped"),
    }
    let _ = std::fs::remove_file(&socket);
}

fn run(
    ctx: MemoryCtx,
    listener: UnixListener,
    wake: &OwnedFd,
    stop: &Arc<AtomicBool>,
) -> Result<(), String> {
    let Some(stream) = accept(&listener, wake, stop)? else {
        return Ok(()); // stopped before Firecracker ever connected
    };
    let (regions, uffd) = handshake(&stream)?;

    let total: u64 = regions.iter().map(|r| r.size).sum();
    if total != ctx.manifest.logical_size {
        return Err(format!(
            "guest memory is {total} bytes but the memory layer covers {} — \
             this checkpoint does not belong to this VM shape",
            ctx.manifest.logical_size
        ));
    }
    let page = regions.first().map(|r| r.page_size).unwrap_or(0);
    if page == 0 || !page.is_power_of_two() {
        return Err(format!("firecracker reported page size {page}"));
    }
    info!(
        pod = %ctx.pod_id,
        regions = regions.len(),
        bytes = total,
        chunk_size = ctx.manifest.chunk_size,
        chunks = ctx.stats.chunks_total.load(Ordering::Relaxed),
        "uffd: attached, serving guest RAM read-through"
    );

    let pid = peer_pid(&stream);
    let removed = RemovedPages::new(ctx.manifest.logical_size, page);
    let shared = Arc::new(Shared {
        pod_id: ctx.pod_id.clone(),
        manifest: ctx.manifest,
        cache: ctx.cache,
        stats: ctx.stats.clone(),
        readahead: ctx.readahead,
        removed,
        uffd,
        regions,
        page_size: page,
        peer_pid: pid,
        peer_pidfd: pidfd_open(pid),
    });
    shared.stats.attached.store(true, Ordering::Relaxed);

    // One channel per worker, sharded by chunk index: the same chunk is
    // always served by the same thread, so concurrent faults into one
    // chunk coalesce into one fetch without any shared state.
    let workers = ctx.fetch_concurrency.max(1);
    let mut txs = Vec::with_capacity(workers);
    let mut joins = Vec::with_capacity(workers);
    for w in 0..workers {
        let (tx, rx) = mpsc::channel::<Job>();
        txs.push(tx);
        let sh = shared.clone();
        match std::thread::Builder::new()
            .name(format!("diskd-uffd-{}-{w}", shared.pod_id))
            .spawn(move || worker(sh, rx))
        {
            Ok(j) => joins.push(j),
            Err(e) => return Err(format!("uffd worker spawn: {e}")),
        }
    }

    let r = poll_loop(&shared, &stream, wake, stop, &txs);
    drop(txs);
    for j in joins {
        let _ = j.join();
    }
    shared.stats.attached.store(false, Ordering::Relaxed);
    r
}

/// Block until Firecracker connects, or until `stop`.
fn accept(
    listener: &UnixListener,
    wake: &OwnedFd,
    stop: &Arc<AtomicBool>,
) -> Result<Option<UnixStream>, String> {
    loop {
        if stop.load(Ordering::SeqCst) {
            return Ok(None);
        }
        let mut fds = [
            pollfd(listener.as_raw_fd(), libc::POLLIN),
            pollfd(wake.as_raw_fd(), libc::POLLIN),
        ];
        match poll(&mut fds, -1) {
            Ok(_) => {}
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(format!("poll(listener): {e}")),
        }
        if fds[1].revents != 0 {
            return Ok(None);
        }
        if fds[0].revents & libc::POLLIN != 0 {
            let (s, _) = listener.accept().map_err(|e| format!("accept: {e}"))?;
            return Ok(Some(s));
        }
    }
}

/// Receive the mappings + the userfaultfd. Firecracker sends both in one
/// `sendmsg` and then deliberately leaks its end of the socket, so the
/// connection stays open for the VMM's whole life — which is also how we
/// learn that it died.
fn handshake(stream: &UnixStream) -> Result<(Vec<RegionMapping>, OwnedFd), String> {
    let mut body: Vec<u8> = Vec::new();
    let mut uffd: Option<OwnedFd> = None;
    for _ in 0..16 {
        let mut buf = [0u8; 8192];
        let (n, fd) = recv_with_fd(stream.as_raw_fd(), &mut buf)
            .map_err(|e| format!("uffd handshake recvmsg: {e}"))?;
        if let Some(fd) = fd {
            uffd = Some(fd);
        }
        if n == 0 {
            return Err("firecracker closed the backend socket during the handshake".into());
        }
        body.extend_from_slice(&buf[..n]);
        if uffd.is_some()
            && let Ok(regions) = serde_json::from_slice::<Vec<RegionMapping>>(&body)
        {
            if regions.is_empty() {
                return Err("firecracker sent an empty memory-region list".into());
            }
            return Ok((regions, uffd.ok_or("uffd lost")?));
        }
    }
    Err(format!(
        "no usable uffd handshake after 16 reads (uffd={}, {} body bytes)",
        uffd.is_some(),
        body.len()
    ))
}

/// The event pump: userfaultfd on one side, the Firecracker socket on
/// the other. The socket is only ever read for its EOF — that is the
/// VMM exiting, and our cue to let go.
fn poll_loop(
    shared: &Arc<Shared>,
    stream: &UnixStream,
    wake: &OwnedFd,
    stop: &Arc<AtomicBool>,
    txs: &[mpsc::Sender<Job>],
) -> Result<(), String> {
    let cs = shared.manifest.chunk_size as u64;
    loop {
        if stop.load(Ordering::SeqCst) {
            return Ok(());
        }
        let mut fds = [
            pollfd(shared.uffd.as_raw_fd(), libc::POLLIN),
            pollfd(stream.as_raw_fd(), libc::POLLIN),
            pollfd(wake.as_raw_fd(), libc::POLLIN),
        ];
        match poll(&mut fds, -1) {
            Ok(_) => {}
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(format!("poll: {e}")),
        }
        if fds[2].revents != 0 || stop.load(Ordering::SeqCst) {
            return Ok(());
        }
        if fds[0].revents & libc::POLLIN != 0 {
            for msg in read_events(shared.uffd.as_raw_fd())? {
                match msg.event {
                    UFFD_EVENT_PAGEFAULT => {
                        shared.stats.faults.fetch_add(1, Ordering::Relaxed);
                        let addr = msg.arg[1];
                        let Some(off) = file_offset(&shared.regions, addr) else {
                            fatal(
                                shared,
                                &format!("fault at {addr:#x} is outside every guest region"),
                            );
                            return Err(format!("fault at {addr:#x} outside the guest regions"));
                        };
                        let chunk = off / cs;
                        dispatch(txs, chunk, Job::Fault { addr });
                        for i in 1..=u64::from(shared.readahead) {
                            let next = chunk + i;
                            if next * cs >= shared.manifest.logical_size {
                                break;
                            }
                            dispatch(txs, next, Job::Prefetch { chunk: next });
                        }
                    }
                    // The balloon's free page reporting MADV_DONTNEEDs
                    // guest pages; they go missing again and refault the
                    // next time anything walks that memory. Remember
                    // which ones, so the refault gets a zero page instead
                    // of the snapshot's dead bytes (see RemovedPages).
                    UFFD_EVENT_REMOVE | UFFD_EVENT_UNMAP => {
                        shared.stats.removes.fetch_add(1, Ordering::Relaxed);
                        shared.removed.mark(&shared.regions, msg.arg[0], msg.arg[1]);
                    }
                    other => warn!(pod = %shared.pod_id, event = other,
                                   "uffd: ignoring unexpected event"),
                }
            }
        }
        if fds[1].revents & (libc::POLLIN | libc::POLLHUP | libc::POLLERR) != 0 {
            let mut buf = [0u8; 256];
            // SAFETY: valid fd, valid buffer.
            let n = unsafe {
                libc::read(
                    stream.as_raw_fd(),
                    buf.as_mut_ptr() as *mut libc::c_void,
                    buf.len(),
                )
            };
            if n <= 0 {
                return Ok(()); // the VMM is gone
            }
            return Err(format!(
                "firecracker sent {n} unexpected bytes on the backend socket \
                 (a second uffd handshake? memory hotplug is not configured)"
            ));
        }
    }
}

fn dispatch(txs: &[mpsc::Sender<Job>], chunk: u64, job: Job) {
    let i = (chunk % txs.len() as u64) as usize;
    let _ = txs[i].send(job);
}

/// Why a fault could not be served. The distinction matters: one of
/// these is a VM to take down, the other is a VM that is already gone.
enum FaultErr {
    /// The VMM's address space no longer exists (ESRCH): its owner
    /// killed it, or it exited. Nothing to serve, nobody to tell.
    Gone,
    /// Anything else. A guest blocked on a page we cannot deliver would
    /// hang forever, so it does not get to stay alive.
    Fatal(String),
}

enum Job {
    /// A guest thread is blocked on this address; serve and wake it.
    Fault { addr: u64 },
    /// Warm the cache only — nothing is waiting.
    Prefetch { chunk: u64 },
}

fn worker(shared: Arc<Shared>, rx: mpsc::Receiver<Job>) {
    while let Ok(job) = rx.recv() {
        match job {
            Job::Fault { addr } => match serve_fault(&shared, addr) {
                Ok(()) => {}
                Err(FaultErr::Gone) => {
                    info!(pod = %shared.pod_id,
                          "uffd: the VMM went away mid-fault; letting go");
                    return;
                }
                Err(FaultErr::Fatal(e)) => {
                    shared.stats.errors.fetch_add(1, Ordering::Relaxed);
                    fatal(&shared, &e);
                    return;
                }
            },
            Job::Prefetch { chunk } => {
                let off = chunk * shared.manifest.chunk_size as u64;
                // Best-effort by definition: nothing is waiting on it,
                // and the fault that needs it will fetch it for real.
                let _ = ensure_chunk(&shared, off);
            }
        }
    }
}

/// Resolve one fault: which chunk covers it, where does that chunk live,
/// install it. The range served is the whole chunk clipped to the
/// region — 256 KiB per fault instead of 4 KiB, which is what makes a
/// lazy restore finite.
fn serve_fault(shared: &Arc<Shared>, addr: u64) -> Result<(), FaultErr> {
    let fault_page = addr & !(shared.page_size - 1);
    let region = shared
        .regions
        .iter()
        .find(|r| r.contains(fault_page))
        .ok_or_else(|| FaultErr::Fatal(format!("fault {fault_page:#x} outside every region")))?;

    let cs = shared.manifest.chunk_size as u64;
    let off = region.offset + (fault_page - region.base_host_virt_addr);
    let chunk_off = off - (off % cs);
    // Clip the chunk to the region it faulted in (regions are contiguous
    // in the image, but a chunk must never be installed across a gap in
    // the host's address space) and to the image's own end.
    let lo = chunk_off.max(region.offset);
    let hi = (chunk_off + cs).min(region.offset + region.size);
    let dst = region.base_host_virt_addr + (lo - region.offset);

    let entry = shared.manifest.entry_at(chunk_off).copied();
    let Some(entry) = entry.filter(|e| e.kind == EntryKind::Data) else {
        // Absent (flat layer, no parent → zero) or an explicit zero
        // entry. Costs nothing and never touches the wire.
        shared
            .stats
            .bytes_served
            .fetch_add(hi - lo, Ordering::Relaxed);
        return fill(
            shared,
            dst,
            None,
            (hi - lo) as usize,
            &shared.stats.chunks_zeroed,
        );
    };

    let raw = ensure_chunk(shared, chunk_off).map_err(FaultErr::Fatal)?;
    let from = (lo - chunk_off) as usize;
    let want = (hi - lo) as usize;
    if from + want > raw.len() {
        return Err(FaultErr::Fatal(format!(
            "chunk at {chunk_off} holds {} bytes, the region needs {}..{} (raw_len {})",
            raw.len(),
            from,
            from + want,
            entry.raw_len
        )));
    }
    // Serve the chunk in runs of like pages: whatever the guest has
    // handed back gets the kernel's zero page, the rest gets the
    // snapshot's content. With nothing released — the common case — the
    // inner loop runs to `hi` and this is the single ioctl it always was.
    let ps = shared.page_size;
    let mut pos = lo;
    while pos < hi {
        let freed = shared.removed.is_set(pos);
        let mut end = pos + ps;
        while end < hi && shared.removed.is_set(end) == freed {
            end += ps;
        }
        let d = region.base_host_virt_addr + (pos - region.offset);
        let n = (end - pos) as usize;
        if freed {
            fill(shared, d, None, n, &shared.stats.chunks_zeroed)?;
            shared.removed.clear(pos, end);
            shared
                .stats
                .pages_reclaimed
                .fetch_add((end - pos) / ps, Ordering::Relaxed);
        } else {
            let at = (pos - chunk_off) as usize;
            fill(
                shared,
                d,
                Some(&raw[at..at + n]),
                n,
                &shared.stats.chunks_served,
            )?;
        }
        pos = end;
    }
    shared
        .stats
        .bytes_served
        .fetch_add(want as u64, Ordering::Relaxed);
    Ok(())
}

/// The read-through itself — the disk path's `read_chunk`, minus ublk.
/// Attempts before a chunk is declared undeliverable.
///
/// A fetch that failed is not the same thing as a fault that cannot be
/// served, and the difference matters because the only answer this
/// handler has to the latter is killing the VM. Two transient conditions
/// reach here routinely. The bucket answers a burst of small GETs with
/// HTTP 503 SlowDown — measured: 32 of 463 objects, p99
/// 5.2 s — and the first pause after a restore is exactly such a burst,
/// because `snapshot_create` reads every page the guest never touched.
/// And the cache can unlink a chunk between `contains` saying yes and
/// the read, since admission-driven eviction runs concurrently.
///
/// The disk path answers both with an `EIO` the guest's block layer can
/// survive (dev.rs). The memory path has no such luxury mid-fault, so it
/// retries first and only escalates once the condition has proven itself
/// not to be transient.
const CHUNK_ATTEMPTS: u32 = 4;

fn ensure_chunk(shared: &Arc<Shared>, chunk_off: u64) -> Result<Vec<u8>, String> {
    let Some(entry) = shared
        .manifest
        .entry_at(chunk_off)
        .filter(|e| e.kind == EntryKind::Data)
    else {
        return Ok(Vec::new());
    };
    let hash = Hash::from_bytes(entry.hash);
    let mut last = String::new();
    for attempt in 1..=CHUNK_ATTEMPTS {
        match fetch_chunk_once(shared, &hash, entry.raw_len) {
            Ok(raw) => {
                if attempt > 1 {
                    info!(pod = %shared.pod_id, chunk = %hash.to_hex(), attempt,
                          "uffd: chunk arrived on retry");
                }
                return Ok(raw);
            }
            Err(e) => {
                last = e;
                if attempt < CHUNK_ATTEMPTS {
                    warn!(pod = %shared.pod_id, chunk = %hash.to_hex(), attempt,
                          error = %last, "uffd: chunk fetch failed, retrying");
                    std::thread::sleep(std::time::Duration::from_millis(100 * u64::from(attempt)));
                }
            }
        }
    }
    Err(format!("{last} (after {CHUNK_ATTEMPTS} attempts)"))
}

/// One attempt at getting a chunk's raw bytes into memory. A miss goes
/// through the same `Cache::ensure_local` the disk path uses, so the S3
/// GET, the zstd decode and the blake3 check are shared code, not a
/// second implementation.
fn fetch_chunk_once(shared: &Arc<Shared>, hash: &Hash, raw_len: u32) -> Result<Vec<u8>, String> {
    let st = &shared.stats;
    if shared.cache.contains(hash) {
        st.cache_hits.fetch_add(1, Ordering::Relaxed);
        shared.cache.touch(hash);
    } else {
        st.cache_misses.fetch_add(1, Ordering::Relaxed);
        shared
            .cache
            .ensure_local(hash, raw_len)
            .map_err(|e| format!("chunk {}: {e}", hash.to_hex()))?;
        st.s3_fetches.fetch_add(1, Ordering::Relaxed);
        st.s3_fetch_bytes
            .fetch_add(u64::from(raw_len), Ordering::Relaxed);
    }
    let p = shared.cache.chunk_path(hash);
    // Not an error worth killing a VM over: an evictor may have unlinked
    // this between `contains` and here. The caller retries, which
    // re-fetches.
    let raw = std::fs::read(&p).map_err(|e| format!("{}: {e}", p.display()))?;
    if raw.len() != raw_len as usize {
        return Err(format!(
            "chunk {} is {} bytes, the manifest says {}",
            hash.to_hex(),
            raw.len(),
            raw_len
        ));
    }
    Ok(raw)
}

// ----------------------------------------------------------- uffd ioctls

/// `EAGAIN` from a fill ioctl is not an error, it is a handshake.
///
/// The kernel raises `mmap_changing` for every non-cooperative event
/// (here: the balloon's `MADV_DONTNEED`, which becomes `UFFD_EVENT_REMOVE`)
/// and refuses every `UFFDIO_COPY`/`UFFDIO_ZEROPAGE` until the handler
/// has *read* that event — and the thread that called `madvise` is itself
/// blocked until then. So the window is exactly "the event pump has not
/// got to it yet", which is microseconds: spin briefly, then sleep in
/// small steps. Giving up in eight tight iterations, as this did first,
/// meant giving up before the pump could possibly have run — and a
/// restore on a busy guest died of it.
const EAGAIN_SPINS: u32 = 64;
const EAGAIN_STEP: std::time::Duration = std::time::Duration::from_micros(200);
const EAGAIN_BUDGET: std::time::Duration = std::time::Duration::from_secs(5);

/// Wait out one EAGAIN. Returns false once the budget is spent.
fn backoff(attempt: u32, since: std::time::Instant) -> bool {
    if since.elapsed() >= EAGAIN_BUDGET {
        return false;
    }
    if attempt < EAGAIN_SPINS {
        std::thread::yield_now();
    } else {
        std::thread::sleep(EAGAIN_STEP);
    }
    true
}

/// Fill `[dst, dst+len)` with a chunk's worth of data (or zeroes), one
/// ioctl where the range is wholly missing and page by page where it is
/// not.
///
/// Serving a whole 256 KiB chunk per fault is what makes a lazy restore
/// finite — 64 pages for one round trip. But a chunk does not stay
/// wholly missing: the balloon reports a few of its pages free,
/// Firecracker `MADV_DONTNEED`s exactly those, and the chunk is now a
/// mix. `UFFDIO_COPY` over a mixed range stops at the first page that is
/// already there and reports `EEXIST` — and if that is the *first* page,
/// it installs nothing at all. Returning "served" there, as this did
/// first, woke a guest whose page was still missing: it faulted again
/// immediately, forever. Measured: 20 000 faults/s on the same 244
/// chunks, a guest that answered nothing, and not one S3 GET to show
/// for it.
///
/// So `EEXIST` means "step over this page and carry on", never "done".
fn fill(
    shared: &Arc<Shared>,
    dst: u64,
    src: Option<&[u8]>,
    len: usize,
    counter: &AtomicU64,
) -> Result<(), FaultErr> {
    let t = std::time::Instant::now();
    let page = shared.page_size as usize;
    let mut done = 0usize;
    let mut attempt = 0u32;
    let mut stepped = false;
    while done < len {
        let (rc, moved) = match src {
            Some(src) => {
                let mut arg = UffdioCopy {
                    dst: dst + done as u64,
                    src: src.as_ptr() as u64 + done as u64,
                    len: (len - done) as u64,
                    mode: 0,
                    copy: 0,
                };
                // SAFETY: live userfaultfd; arg outlives the ioctl.
                let rc = unsafe { libc::ioctl(shared.uffd.as_raw_fd(), UFFDIO_COPY, &mut arg) };
                (rc, arg.copy)
            }
            None => {
                let mut arg = UffdioZeropage {
                    range: UffdioRange {
                        start: dst + done as u64,
                        len: (len - done) as u64,
                    },
                    mode: 0,
                    zeropage: 0,
                };
                // SAFETY: live userfaultfd; arg outlives the ioctl.
                let rc = unsafe { libc::ioctl(shared.uffd.as_raw_fd(), UFFDIO_ZEROPAGE, &mut arg) };
                (rc, arg.zeropage)
            }
        };
        if rc == 0 {
            counter.fetch_add(1, Ordering::Relaxed);
            // Anything we stepped over was woken by nobody: the kernel
            // only wakes what it filled.
            return if stepped {
                wake(shared, dst, len as u64).map_err(FaultErr::Fatal)
            } else {
                Ok(())
            };
        }
        let err = io::Error::last_os_error();
        // Partial progress is reported in the out field (negative when
        // nothing moved: the kernel puts -errno there).
        if moved > 0 {
            done += moved as usize;
            continue;
        }
        match err.raw_os_error() {
            // This page is already populated — by a racing serve of an
            // overlapping chunk, by the guest writing it, or because
            // only part of the chunk was madvised away. Step over it.
            Some(libc::EEXIST) => {
                stepped = true;
                done += page;
            }
            Some(libc::EAGAIN) => {
                attempt += 1;
                if !backoff(attempt, t) {
                    return Err(FaultErr::Fatal(format!(
                        "uffd fill {dst:#x}+{len}: EAGAIN for {:?} ({done} bytes in) — \
the event pump is not draining",
                        t.elapsed()
                    )));
                }
            }
            // The address space is gone: the VMM exited (usually because
            // its owner killed it). Nothing left to serve.
            Some(libc::ESRCH) => return Err(FaultErr::Gone),
            _ => {
                return Err(FaultErr::Fatal(format!(
                    "uffd fill {dst:#x}+{len} (+{done}): {err}"
                )))
            }
        }
    }
    // Every page of the range was already there. The faulting thread is
    // still blocked on one of them, so the wake is the whole answer.
    counter.fetch_add(1, Ordering::Relaxed);
    wake(shared, dst, len as u64).map_err(FaultErr::Fatal)
}

fn wake(shared: &Arc<Shared>, start: u64, len: u64) -> Result<(), String> {
    let mut arg = UffdioRange { start, len };
    // SAFETY: uffd is a live userfaultfd; arg outlives the ioctl.
    let rc = unsafe { libc::ioctl(shared.uffd.as_raw_fd(), UFFDIO_WAKE, &mut arg) };
    if rc == 0 {
        return Ok(());
    }
    Err(format!(
        "UFFDIO_WAKE {start:#x}+{len}: {}",
        io::Error::last_os_error()
    ))
}

fn read_events(fd: RawFd) -> Result<Vec<UffdMsg>, String> {
    const BATCH: usize = 32;
    let mut msgs = [UffdMsg::default(); BATCH];
    // SAFETY: valid fd; the buffer is exactly BATCH * sizeof(uffd_msg).
    let n = unsafe {
        libc::read(
            fd,
            msgs.as_mut_ptr() as *mut libc::c_void,
            size_of::<UffdMsg>() * BATCH,
        )
    };
    if n < 0 {
        let e = io::Error::last_os_error();
        return match e.raw_os_error() {
            Some(libc::EAGAIN) | Some(libc::EINTR) => Ok(Vec::new()),
            _ => Err(format!("read(uffd): {e}")),
        };
    }
    let got = n as usize / size_of::<UffdMsg>();
    Ok(msgs[..got].to_vec())
}

fn file_offset(regions: &[RegionMapping], addr: u64) -> Option<u64> {
    regions
        .iter()
        .find(|r| r.contains(addr))
        .map(|r| r.offset + (addr - r.base_host_virt_addr))
}

/// A fault we cannot serve is a guest that blocks forever. Firecracker
/// holds its own copy of the uffd, so simply exiting would leave the VM
/// hanging with no diagnosis; take it down instead and let the owner's
/// VMM watch report it (upstream's own recommendation for this backend).
/// Kill the VMM this handler was serving, because a guest blocked on a
/// page that will never arrive would hang forever and a visible death is
/// the better failure.
///
/// Through a pidfd, never `kill(2)` on the bare pid: diskd runs with
/// `hostPID: true`, so `peer_pid` is a host pid in its own namespace,
/// and it was captured at handshake time — possibly minutes and one
/// destroyed pod ago. A recycled pid would make this signal land on
/// whatever process now holds the number, which on a hypervisor node is
/// another project's VMM. The pidfd is pinned to the process that opened
/// the socket; if that process is gone the send fails and nothing else
/// is harmed.
fn fatal(shared: &Arc<Shared>, why: &str) {
    error!(pod = %shared.pod_id, pid = shared.peer_pid, reason = %why,
           "uffd: cannot serve a page fault — killing the VMM");
    let Some(fd) = shared.peer_pidfd else {
        error!(pod = %shared.pod_id,
               "uffd: no pidfd for the VMM — refusing to signal a pid that may have been reused");
        return;
    };
    // SAFETY: pidfd_send_signal(2) on an fd this handler owns for its
    // whole lifetime; siginfo NULL means "as if from kill(2)".
    let rc = unsafe { libc::syscall(libc::SYS_pidfd_send_signal, fd, libc::SIGKILL, 0, 0) };
    if rc != 0 {
        let e = std::io::Error::last_os_error();
        error!(pod = %shared.pod_id, error = %e, "uffd: could not signal the VMM");
    }
}

// ------------------------------------------------------------ fd plumbing

fn pollfd(fd: RawFd, events: libc::c_short) -> libc::pollfd {
    libc::pollfd {
        fd,
        events,
        revents: 0,
    }
}

fn poll(fds: &mut [libc::pollfd], timeout_ms: libc::c_int) -> io::Result<usize> {
    // SAFETY: fds is a valid slice of pollfd for its own length.
    let rc = unsafe { libc::poll(fds.as_mut_ptr(), fds.len() as libc::nfds_t, timeout_ms) };
    if rc < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(rc as usize)
}

fn eventfd_new() -> Result<OwnedFd, String> {
    // SAFETY: eventfd() returns a valid fd or -1.
    let fd = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC) };
    if fd < 0 {
        return Err(format!("eventfd: {}", io::Error::last_os_error()));
    }
    // SAFETY: fd is fresh and owned.
    Ok(unsafe { OwnedFd::from_raw_fd(fd) })
}

fn notify(efd: &OwnedFd) {
    let buf = 1u64.to_ne_bytes();
    // SAFETY: valid fd, 8-byte buffer.
    unsafe {
        libc::write(efd.as_raw_fd(), buf.as_ptr() as *const libc::c_void, 8);
    }
}

/// Pin the peer process so a later signal cannot land on a recycled pid.
/// Returns None when the process is already gone (ESRCH) or the kernel
/// refuses — callers then simply do not signal.
fn pidfd_open(pid: libc::pid_t) -> Option<libc::c_int> {
    if pid <= 1 {
        return None;
    }
    // SAFETY: pidfd_open(2) with no flags; the fd is owned by Shared.
    let fd = unsafe { libc::syscall(libc::SYS_pidfd_open, pid, 0) };
    if fd < 0 {
        let e = std::io::Error::last_os_error();
        warn!(pid, error = %e, "uffd: could not pin the VMM with a pidfd");
        return None;
    }
    Some(fd as libc::c_int)
}

fn peer_pid(stream: &UnixStream) -> libc::pid_t {
    let mut cred = libc::ucred {
        pid: 0,
        uid: 0,
        gid: 0,
    };
    let mut len = size_of::<libc::ucred>() as libc::socklen_t;
    // SAFETY: cred/len outlive the call and match SO_PEERCRED's shape.
    let rc = unsafe {
        libc::getsockopt(
            stream.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            (&mut cred as *mut libc::ucred).cast(),
            &mut len,
        )
    };
    if rc != 0 {
        return 0;
    }
    cred.pid
}

/// `recvmsg` with a one-fd `SCM_RIGHTS` slot.
fn recv_with_fd(fd: RawFd, buf: &mut [u8]) -> io::Result<(usize, Option<OwnedFd>)> {
    // u64 array: the control buffer must be aligned for struct cmsghdr.
    let mut cmsg = [0u64; 8];
    let mut iov = libc::iovec {
        iov_base: buf.as_mut_ptr().cast(),
        iov_len: buf.len(),
    };
    // SAFETY: msghdr is plain data; every pointer below is valid for the
    // duration of the call.
    let mut msg: libc::msghdr = unsafe { std::mem::zeroed() };
    msg.msg_iov = &mut iov;
    msg.msg_iovlen = 1;
    msg.msg_control = cmsg.as_mut_ptr().cast();
    msg.msg_controllen = size_of_val(&cmsg) as _;

    // SAFETY: valid fd and msghdr.
    let n = unsafe { libc::recvmsg(fd, &mut msg, 0) };
    if n < 0 {
        return Err(io::Error::last_os_error());
    }
    let mut out = None;
    // SAFETY: walking the control buffer we just handed to the kernel.
    unsafe {
        let mut c = libc::CMSG_FIRSTHDR(&msg);
        while !c.is_null() {
            if (*c).cmsg_level == libc::SOL_SOCKET && (*c).cmsg_type == libc::SCM_RIGHTS {
                let raw = std::ptr::read_unaligned(libc::CMSG_DATA(c).cast::<RawFd>());
                if raw >= 0 {
                    out = Some(OwnedFd::from_raw_fd(raw));
                }
            }
            c = libc::CMSG_NXTHDR(&msg, c);
        }
    }
    Ok((n as usize, out))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The handler counts after the ioctl that wakes the faulting thread,
    /// so a counter can lag the read the test just finished: wait for it.
    fn settled(counter: &std::sync::atomic::AtomicU64, ok: impl Fn(u64) -> bool) -> u64 {
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        loop {
            let v = counter.load(Ordering::Relaxed);
            if ok(v) || std::time::Instant::now() > deadline {
                return v;
            }
            std::thread::sleep(std::time::Duration::from_millis(5));
        }
    }

    #[test]
    fn ioctl_numbers_match_the_uapi() {
        // linux/userfaultfd.h, x86_64. Wrong numbers here are an
        // EINVAL per page fault, i.e. a wedged guest.
        assert_eq!(UFFDIO_COPY as u32, 0xc028_aa03);
        assert_eq!(UFFDIO_ZEROPAGE as u32, 0xc020_aa04);
        assert_eq!(UFFDIO_WAKE as u32, 0x8010_aa02);
    }

    // ---- a full round trip, with this test playing Firecracker ------
    //
    // Everything below the socket is the real thing: the real handshake,
    // the real fault path, the real manifest/cache lookup, the real
    // UFFDIO_COPY / UFFDIO_ZEROPAGE. What is faked is the VMM — it maps
    // anonymous memory, registers it with a userfaultfd and hands both
    // over exactly the way `vmm::persist::create_guest_memory` does.

    const UFFDIO_API: libc::Ioctl = ioc(IOC_READ | IOC_WRITE, 0x3F, 24) as libc::Ioctl;
    const UFFDIO_REGISTER: libc::Ioctl = ioc(IOC_READ | IOC_WRITE, 0x00, 32) as libc::Ioctl;
    const UFFD_API: u64 = 0xAA;
    const UFFD_FEATURE_EVENT_REMOVE: u64 = 1 << 3;
    const UFFDIO_REGISTER_MODE_MISSING: u64 = 1;
    const UFFD_USER_MODE_ONLY: libc::c_int = 1;

    #[repr(C)]
    #[derive(Default)]
    struct UffdioApi {
        api: u64,
        features: u64,
        ioctls: u64,
    }

    #[repr(C)]
    #[derive(Default)]
    struct UffdioRegister {
        range: UffdioRange,
        mode: u64,
        ioctls: u64,
    }

    /// The VMM half: anonymous guest memory + a registered userfaultfd.
    fn fake_vmm_memory(len: usize) -> Option<(*mut u8, OwnedFd)> {
        // SAFETY: standard anonymous mapping.
        let p = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANONYMOUS | libc::MAP_NORESERVE,
                -1,
                0,
            )
        };
        assert_ne!(p, libc::MAP_FAILED, "mmap");
        // SAFETY: userfaultfd(2). USER_MODE_ONLY keeps this unprivileged;
        // only this process's own reads fault.
        let fd = unsafe {
            libc::syscall(
                libc::SYS_userfaultfd,
                libc::O_CLOEXEC | libc::O_NONBLOCK | UFFD_USER_MODE_ONLY,
            )
        };
        if fd < 0 {
            // No userfaultfd on this kernel/pod: nothing to prove.
            // SAFETY: p came from mmap above.
            unsafe { libc::munmap(p, len) };
            return None;
        }
        // SAFETY: fresh owned fd.
        let uffd = unsafe { OwnedFd::from_raw_fd(fd as RawFd) };
        let mut api = UffdioApi {
            api: UFFD_API,
            // What Firecracker requires, so the test sees the same
            // mmap_changing / EAGAIN behaviour production does.
            features: UFFD_FEATURE_EVENT_REMOVE,
            ..Default::default()
        };
        // SAFETY: live uffd, arg outlives the call.
        assert_eq!(
            unsafe { libc::ioctl(uffd.as_raw_fd(), UFFDIO_API, &mut api) },
            0,
            "UFFDIO_API: {}",
            io::Error::last_os_error()
        );
        let mut reg = UffdioRegister {
            range: UffdioRange {
                start: p as u64,
                len: len as u64,
            },
            mode: UFFDIO_REGISTER_MODE_MISSING,
            ioctls: 0,
        };
        // SAFETY: live uffd, arg outlives the call.
        assert_eq!(
            unsafe { libc::ioctl(uffd.as_raw_fd(), UFFDIO_REGISTER, &mut reg) },
            0,
            "UFFDIO_REGISTER: {}",
            io::Error::last_os_error()
        );
        Some((p.cast(), uffd))
    }

    fn send_with_fd(stream: &UnixStream, body: &[u8], fd: RawFd) {
        let mut cmsg = [0u64; 8];
        let mut iov = libc::iovec {
            iov_base: body.as_ptr() as *mut libc::c_void,
            iov_len: body.len(),
        };
        // SAFETY: plain data; pointers live for the call.
        unsafe {
            let mut msg: libc::msghdr = std::mem::zeroed();
            msg.msg_iov = &mut iov;
            msg.msg_iovlen = 1;
            msg.msg_control = cmsg.as_mut_ptr().cast();
            msg.msg_controllen = libc::CMSG_SPACE(size_of::<RawFd>() as u32) as _;
            let c = libc::CMSG_FIRSTHDR(&msg);
            (*c).cmsg_level = libc::SOL_SOCKET;
            (*c).cmsg_type = libc::SCM_RIGHTS;
            (*c).cmsg_len = libc::CMSG_LEN(size_of::<RawFd>() as u32) as _;
            std::ptr::write_unaligned(libc::CMSG_DATA(c).cast::<RawFd>(), fd);
            assert!(libc::sendmsg(stream.as_raw_fd(), &msg, 0) > 0, "sendmsg");
        }
    }

    #[test]
    fn faults_are_served_out_of_the_chunk_store() {
        const CS: u32 = 64 * 1024;
        const CHUNKS: usize = 4;
        let len = CS as usize * CHUNKS;

        let tmp = tempfile::tempdir().unwrap();
        let cache = Arc::new(disk_store::cache::Cache::open(tmp.path(), 64 << 20).unwrap());

        // Chunk 1 is absent (flat layer: absent == zero), chunk 2 is an
        // explicit zero entry. The other two carry data — and the data
        // is position-dependent so a copy landing at the wrong offset
        // cannot pass.
        let mut want = vec![0u8; len];
        let mut manifest = Manifest::new(CS, len as u64, None);
        for ci in [0usize, 3] {
            let off = ci * CS as usize;
            let data: Vec<u8> = (0..CS as usize)
                .map(|i| (i.wrapping_mul(31).wrapping_add(ci * 7)) as u8)
                .collect();
            want[off..off + CS as usize].copy_from_slice(&data);
            let h = disk_store::chunk::hash(&data);
            cache.admit(&h, &data).unwrap();
            manifest.entries.push(disk_store::manifest::Entry::data(
                off as u64,
                *h.as_bytes(),
                CS,
                0,
            ));
        }
        manifest
            .entries
            .push(disk_store::manifest::Entry::zero(2 * CS as u64, CS));
        manifest.entries.sort_by_key(|e| e.offset);

        let Some((mem, uffd)) = fake_vmm_memory(len) else {
            println!("skipping: userfaultfd(2) unavailable here");
            return;
        };

        let socket = tmp.path().join("uffd.sock");
        let stats = Arc::new(MemStats::default());
        let started = serve(MemoryCtx {
            pod_id: "t".into(),
            socket: socket.clone(),
            manifest,
            cache,
            readahead: 2,
            fetch_concurrency: 2,
            stats: stats.clone(),
        })
        .expect("serve");

        // …and now be Firecracker.
        let stream = UnixStream::connect(&socket).expect("connect");
        let body = serde_json::to_vec(&serde_json::json!([{
            "base_host_virt_addr": mem as u64,
            "size": len,
            "offset": 0,
            "page_size": 4096,
            "page_size_kib": 4096,
        }]))
        .unwrap();
        send_with_fd(&stream, &body, uffd.as_raw_fd());

        // Touch every page from a thread so a handler that never answers
        // fails this test instead of hanging it.
        let (tx, rx) = mpsc::channel::<Vec<u8>>();
        let addr = mem as usize;
        std::thread::spawn(move || {
            // SAFETY: the mapping is alive for the test's duration; the
            // reads are exactly what a guest vCPU would do.
            let got = unsafe { std::slice::from_raw_parts(addr as *const u8, len) }.to_vec();
            let _ = tx.send(got);
        });
        let got = rx
            .recv_timeout(std::time::Duration::from_secs(30))
            .expect("the handler never served the faults");

        assert_eq!(got.len(), want.len());
        assert!(got == want, "guest RAM differs from the layer's content");
        assert_eq!(settled(&stats.chunks_served, |v| v >= 2), 2, "data chunks");
        assert!(
            settled(&stats.chunks_zeroed, |v| v >= 2) >= 2,
            "absent + explicit-zero chunks are served without touching the store"
        );
        assert_eq!(stats.errors.load(Ordering::Relaxed), 0);
        assert!(stats.attached.load(Ordering::Relaxed));

        stop(started);
        assert!(!socket.exists(), "the backend socket is cleaned up");
        // SAFETY: mem came from mmap in fake_vmm_memory.
        unsafe { libc::munmap(mem.cast(), len) };
    }

    /// The regression that cost a whole end-to-end run: a chunk that is only
    /// PARTLY missing.
    ///
    /// Serving 256 KiB per fault is the point of this handler, but the
    /// balloon punches single pages out of a chunk it already served.
    /// `UFFDIO_COPY` over the chunk then fails `EEXIST` on its first
    /// (still present) page and installs nothing — and a handler that
    /// treats that as "served" wakes a guest whose page is still gone.
    /// It refaults instantly, forever. This test makes the hole by hand
    /// and then reads through it; before the fix it hung.
    ///
    /// What lands IN the hole changed with V0: the hole was punched by
    /// `MADV_DONTNEED`, which is the guest releasing the page, so it now
    /// comes back as the kernel's zero page rather than the chunk's old
    /// bytes (see `RemovedPages`). What this test pins is unchanged —
    /// the read completes, the still-present pages keep their content,
    /// and the fill does not loop.
    #[test]
    fn a_partly_madvised_chunk_is_filled_page_by_page() {
        const CS: u32 = 64 * 1024;
        const PAGE: usize = 4096;
        let len = CS as usize;

        let tmp = tempfile::tempdir().unwrap();
        let cache = Arc::new(disk_store::cache::Cache::open(tmp.path(), 16 << 20).unwrap());
        let data: Vec<u8> = (0..len).map(|i| (i / PAGE + 1) as u8).collect();
        let h = disk_store::chunk::hash(&data);
        cache.admit(&h, &data).unwrap();
        let mut manifest = Manifest::new(CS, len as u64, None);
        manifest
            .entries
            .push(disk_store::manifest::Entry::data(0, *h.as_bytes(), CS, 0));

        let Some((mem, uffd)) = fake_vmm_memory(len) else {
            println!("skipping: userfaultfd(2) unavailable here");
            return;
        };
        let socket = tmp.path().join("uffd.sock");
        let stats = Arc::new(MemStats::default());
        let started = serve(MemoryCtx {
            pod_id: "t".into(),
            socket: socket.clone(),
            manifest,
            cache,
            readahead: 0,
            fetch_concurrency: 1,
            stats: stats.clone(),
        })
        .expect("serve");
        let stream = UnixStream::connect(&socket).expect("connect");
        let body = serde_json::to_vec(&serde_json::json!([{
            "base_host_virt_addr": mem as u64, "size": len, "offset": 0, "page_size": PAGE,
        }]))
        .unwrap();
        send_with_fd(&stream, &body, uffd.as_raw_fd());

        let addr = mem as usize;
        let (tx, rx) = mpsc::channel::<(u8, u8)>();
        std::thread::spawn(move || {
            // SAFETY: the mapping outlives this thread's use of it.
            let m = unsafe { std::slice::from_raw_parts(addr as *const u8, len) };
            // First touch: the whole chunk gets installed.
            assert_eq!(m[0], 1);
            // Punch one page out of the middle of it, leaving page 0 in
            // place. madvise blocks until the handler reads the REMOVE.
            // SAFETY: our own mapping, page-aligned range.
            let rc = unsafe {
                libc::madvise(
                    (addr + 3 * PAGE) as *mut libc::c_void,
                    PAGE,
                    libc::MADV_DONTNEED,
                )
            };
            assert_eq!(rc, 0, "madvise: {}", io::Error::last_os_error());
            // …and read through the hole. This is the fault that used to
            // loop: the chunk's first page is present, the faulting page
            // is not.
            // Page 0 is still present and must keep the chunk's bytes;
            // the released page must not.
            let _ = tx.send((m[3 * PAGE], m[0]));
        });
        let (hole, kept) = rx
            .recv_timeout(std::time::Duration::from_secs(30))
            .expect("the handler never filled a partly-populated chunk");
        assert_eq!(
            hole, 0,
            "a page the guest released came back holding the snapshot"
        );
        assert_eq!(kept, 1, "the pages around it still carry the chunk");
        assert!(
            settled(&stats.removes, |v| v >= 1) >= 1,
            "the REMOVE event reached the pump"
        );
        assert_eq!(stats.errors.load(Ordering::Relaxed), 0);
        // One S3-less fetch, two fills: the storm is 2, not 20 000.
        assert!(
            stats.chunks_served.load(Ordering::Relaxed) <= 8,
            "served {} times — the refault loop is back",
            stats.chunks_served.load(Ordering::Relaxed)
        );

        stop(started);
        // SAFETY: mem came from mmap in fake_vmm_memory.
        unsafe { libc::munmap(mem.cast(), len) };
    }

    #[test]
    fn removed_pages_index_by_file_offset_across_regions() {
        // Two regions, the second at a non-zero image offset — the case
        // where mixing up host address and file offset would go unnoticed
        // in a single-region test.
        let regions = vec![
            RegionMapping {
                base_host_virt_addr: 0x10_0000,
                size: 0x2000,
                offset: 0,
                page_size: 0x1000,
            },
            RegionMapping {
                base_host_virt_addr: 0x90_0000,
                size: 0x2000,
                offset: 0x2000,
                page_size: 0x1000,
            },
        ];
        let r = RemovedPages::new(0x4000, 0x1000);

        // One page in the middle of the second region.
        assert_eq!(r.mark(&regions, 0x90_1000, 0x90_2000), 1);
        assert!(
            r.is_set(0x3000),
            "second region, second page → offset 0x3000"
        );
        assert!(!r.is_set(0x2000));
        assert!(!r.is_set(0x0000));

        // Marking again is idempotent and reports nothing new.
        assert_eq!(r.mark(&regions, 0x90_1000, 0x90_2000), 0);

        // A range spanning both regions marks the rest.
        assert_eq!(r.mark(&regions, 0x10_0000, 0x90_2000), 3);
        for off in [0x0, 0x1000, 0x2000, 0x3000] {
            assert!(r.is_set(off), "offset {off:#x}");
        }

        r.clear(0x1000, 0x3000);
        assert!(r.is_set(0x0000));
        assert!(!r.is_set(0x1000));
        assert!(!r.is_set(0x2000));
        assert!(r.is_set(0x3000));

        // Outside the image is not a panic, just not set.
        assert!(!r.is_set(0x9999_0000));
    }

    /// A page the guest
    /// has handed back must NOT come back holding the snapshot's bytes.
    ///
    /// Without this, the freed page is refilled from the layer, the next
    /// `snapshot_create` writes those dead bytes into the following
    /// checkpoint, and every generation keeps everything any earlier one
    /// held — measured as 506.0 MiB against 144.0 MiB for
    /// the same guest, unchanged from one generation to the next.
    #[test]
    fn a_page_the_guest_released_comes_back_zero_not_stale() {
        const CS: u32 = 64 * 1024;
        let len = CS as usize;
        let tmp = tempfile::tempdir().unwrap();
        let cache = Arc::new(disk_store::cache::Cache::open(tmp.path(), 64 << 20).unwrap());

        // One data chunk, every byte non-zero, so "zero" can only mean
        // the kernel's zero page and never the layer's content.
        let data: Vec<u8> = (0..len).map(|i| ((i % 251) + 1) as u8).collect();
        let h = disk_store::chunk::hash(&data);
        cache.admit(&h, &data).unwrap();
        let mut manifest = Manifest::new(CS, len as u64, None);
        manifest
            .entries
            .push(disk_store::manifest::Entry::data(0, *h.as_bytes(), CS, 0));

        let Some((mem, uffd)) = fake_vmm_memory(len) else {
            println!("skipping: userfaultfd(2) unavailable here");
            return;
        };
        let socket = tmp.path().join("uffd.sock");
        let stats = Arc::new(MemStats::default());
        let started = serve(MemoryCtx {
            pod_id: "s5".into(),
            socket: socket.clone(),
            manifest,
            cache,
            readahead: 0,
            fetch_concurrency: 2,
            stats: stats.clone(),
        })
        .expect("serve");

        let stream = UnixStream::connect(&socket).expect("connect");
        let body = serde_json::to_vec(&serde_json::json!([{
            "base_host_virt_addr": mem as u64,
            "size": len,
            "offset": 0,
            "page_size": 4096,
            "page_size_kib": 4096,
        }]))
        .unwrap();
        send_with_fd(&stream, &body, uffd.as_raw_fd());

        let ps = 4096usize;
        let read_at = |i: usize| -> Vec<u8> {
            let (tx, rx) = mpsc::channel::<Vec<u8>>();
            let at = mem as usize + i * ps;
            std::thread::spawn(move || {
                // SAFETY: inside the live mapping for the test's duration.
                let v = unsafe { std::slice::from_raw_parts(at as *const u8, ps) }.to_vec();
                let _ = tx.send(v);
            });
            rx.recv_timeout(std::time::Duration::from_secs(30))
                .expect("the handler never served the fault")
        };

        // First touch: the snapshot's content, as always.
        assert_eq!(read_at(0), data[..ps], "the layer's bytes on first touch");
        assert_eq!(stats.pages_reclaimed.load(Ordering::Relaxed), 0);

        // The guest gives page 1 back. EVENT_REMOVE makes madvise wait
        // for the handler to read it, so this is ordered by the kernel.
        // SAFETY: the page is inside the live mapping.
        let rc = unsafe { libc::madvise(mem.cast::<u8>().add(ps).cast(), ps, libc::MADV_DONTNEED) };
        assert_eq!(rc, 0, "madvise: {}", io::Error::last_os_error());

        // …and touches it again. Before the fix this returned the layer.
        let got = read_at(1);
        assert!(
            got.iter().all(|&b| b == 0),
            "a released page came back holding the snapshot: {:?}…",
            &got[..16]
        );
        assert!(
            settled(&stats.pages_reclaimed, |v| v >= 1) >= 1,
            "the reclaimed page is counted"
        );
        assert!(settled(&stats.removes, |v| v >= 1) >= 1);

        // Control: a page that was never released still reads the layer,
        // so the fix did not simply zero the whole chunk.
        assert_eq!(
            read_at(2),
            data[2 * ps..3 * ps],
            "an untouched page still carries the snapshot's content"
        );
        assert_eq!(stats.errors.load(Ordering::Relaxed), 0);

        stop(started);
        // SAFETY: mem came from mmap in fake_vmm_memory.
        unsafe { libc::munmap(mem.cast(), len) };
    }

    #[test]
    fn mappings_decode_firecrackers_json() {
        // Exactly what v1.17 serialises, deprecated field included.
        let body = br#"[{"base_host_virt_addr":140737488355328,"size":536870912,
            "offset":0,"page_size":4096,"page_size_kib":4096}]"#;
        let r: Vec<RegionMapping> = serde_json::from_slice(body).unwrap();
        assert_eq!(r.len(), 1);
        assert_eq!(r[0].page_size, 4096);
        assert!(r[0].contains(140737488355328));
        assert!(!r[0].contains(140737488355328 + 536870912));
        assert_eq!(
            file_offset(&r, 140737488355328 + 4096),
            Some(4096),
            "offset into the memory image"
        );
    }
}
