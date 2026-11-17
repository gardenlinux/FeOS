// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! gRPC control plane (tonic on a unix socket).
//!
//! Locking: the device table is a `Mutex` used
//! ONLY here in the control plane — the ublk request path (`diskd::dev`)
//! denies disallowed types outright and never touches this map. Every
//! blocking store/device call runs in `spawn_blocking`.
//!
//! Each call that changes a disk (its row, its top, its device) holds the
//! disk's lock until its work on the node ends, also when its caller goes
//! away: the guard moves into the blocking closure. So a seal and an open
//! of one disk never run together. A seal whose caller goes away before
//! its new layer is committed stops at its next chunk, and leaves the
//! disk as it was. `diskd.state` has one writer at a time
//! (`update_state`), the pins one refresh at a time, and the journals of
//! unheaded disks one writer at a time.
//!
//! Beyond devices: `Bake` (live seal of a frozen top), flatten-on-seal as
//! `Seal`'s default behaviour, `SealFile` (flat memory
//! layers), `OpenDisk{head_key}` (restore from a foreign node's head),
//! `PutHead`/`GetHead` (a checkpoint's head, last), per-chunk upload
//! markers + bounded-parallel S3 PUT/GET (`Push`, `Pull --warm`, miss
//! hydration, materialize).

#![allow(clippy::disallowed_types)] // control plane only — see module docs

use std::collections::{BTreeSet, HashMap, HashSet};
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use disk_store::chunk::Hash;
use feos_proto::disk_service as pb;
use feos_proto::disk_service::disk_service_server::DiskService;
use futures::StreamExt as _;
use tokio::sync::OwnedMutexGuard;
use tonic::{Request, Response, Status};
use tracing::{debug, info, warn};

use crate::dev::{self, DeviceCtx, StartedDevice};
use crate::state::{DeviceRow, State};
use crate::uffd::{self, MemStats, MemoryCtx, StartedMemory};

type DeviceTable = Arc<Mutex<HashMap<String, StartedDevice>>>;
/// Live UFFD memory handlers, one per restored pod. Control plane
/// only, exactly like `DeviceTable` — the fault path never looks here.
type MemoryTable = Arc<Mutex<HashMap<String, StartedMemory>>>;
/// A lock per disk that a call holds, made on first use and gone when no
/// call holds or waits for it. Control plane only.
type DiskLocks = Arc<Mutex<HashMap<String, Arc<tokio::sync::Mutex<()>>>>>;

#[derive(Clone)]
pub struct DiskSvc {
    pub state_dir: PathBuf,
    pub cache: Arc<disk_store::cache::Cache>,
    pub s3: Option<Arc<disk_store::s3::S3>>,
    pub devices: DeviceTable,
    pub memories: MemoryTable,
    /// Bounded parallelism for S3 PUT/GET (`--s3-concurrency`).
    pub s3_concurrency: usize,
    /// `--mem-readahead`: chunks the UFFD backend pulls ahead of a fault.
    pub mem_readahead: u32,
    disk_locks: DiskLocks,
    /// The one writer of `diskd.state` (`update_state`).
    state_mu: Arc<Mutex<()>>,
    /// One refresh of the pins at a time, so that an older set never
    /// replaces a newer one (`refresh_pins`).
    pins_mu: Arc<Mutex<()>>,
    /// The journals of unheaded disks and memory layers: an append and
    /// its markers, or an expiry, at a time.
    journal_mu: Arc<tokio::sync::Mutex<()>>,
    /// Runs in Seal's closure, under the disk's lock, at each `SealPoint`.
    #[cfg(test)]
    seal_hook: Option<Arc<dyn Fn(SealPoint) + Send + Sync>>,
}

/// Where Seal's closure runs a test's hook.
#[cfg(test)]
#[derive(Clone, Copy, Debug, PartialEq)]
enum SealPoint {
    /// Under the disk's lock, before the seal.
    Locked,
    /// The new layer's manifest is committed; the row does not name it yet.
    Committed,
}

/// How long a call waits for the lock of its disk: a local seal of a
/// large disk takes minutes.
const DISK_LOCK_WAIT: Duration = Duration::from_secs(10 * 60);

/// How long a push trusts the chunk markers in the journal of a disk or
/// memory layer that no head names yet (`unheaded/`). A sweep deletes
/// such chunks once they are older than its grace, which must be longer
/// than this and the longest hibernation together (ADR 0011).
const UNHEADED_TRUST_SECONDS: u64 = 12 * 60 * 60;

/// Chunks a push marks uploaded at a time, after their batch is in the
/// journal.
const MARK_BATCH: usize = 256;

/// The lock of a disk, held. Dropping it lets the next call of the disk
/// go, and forgets the lock when nobody else holds or waits for it.
pub(crate) struct DiskLock {
    id: String,
    locks: DiskLocks,
    guard: Option<OwnedMutexGuard<()>>,
}

impl Drop for DiskLock {
    fn drop(&mut self) {
        drop(self.guard.take());
        if let Ok(mut locks) = self.locks.lock()
            && locks
                .get(&self.id)
                .is_some_and(|l| Arc::strong_count(l) == 1)
        {
            locks.remove(&self.id);
        }
    }
}

/// Nothing younger than this is swept (ADR 0011): between the first
/// chunk PUT of a checkpoint and the head.json that names it, every one
/// of its objects looks unreferenced from the bucket's side.
const DEFAULT_SWEEP_GRACE_SECONDS: i64 = 24 * 60 * 60;
/// Soft wall-clock budget for one sweep. Listing can be slow; a
/// sweep that runs out of budget stops between passes and the next one
/// picks up where the bucket now is.
const DEFAULT_SWEEP_BUDGET_SECONDS: u64 = 15 * 60;

fn internal(e: impl std::fmt::Display) -> Status {
    Status::internal(e.to_string())
}

/// Upload/download paths run at SCHED_IDLE so S3 I/O never contends with
/// the pod's own I/O in the other direction. Best-effort: failure (e.g. in
/// a container) is silent. The raw syscall: musl's sched_setscheduler
/// only returns ENOSYS.
pub(crate) fn set_sched_idle() {
    // SAFETY: thread 0 = calling thread; sched_param is zeroed plain data
    // (priority 0, as SCHED_IDLE wants) and outlives the call.
    unsafe {
        let param: libc::sched_param = std::mem::zeroed();
        libc::syscall(
            libc::SYS_sched_setscheduler,
            0,
            libc::SCHED_IDLE,
            &param as *const libc::sched_param,
        );
    }
}

impl DiskSvc {
    pub fn new(
        state_dir: PathBuf,
        cache: Arc<disk_store::cache::Cache>,
        s3: Option<Arc<disk_store::s3::S3>>,
        s3_concurrency: usize,
        mem_readahead: u32,
    ) -> Self {
        DiskSvc {
            state_dir,
            cache,
            s3,
            devices: Default::default(),
            memories: Default::default(),
            s3_concurrency,
            mem_readahead,
            disk_locks: Default::default(),
            state_mu: Default::default(),
            pins_mu: Default::default(),
            journal_mu: Default::default(),
            #[cfg(test)]
            seal_hook: None,
        }
    }

    /// Takes the lock of disk `id`, waiting at most `DISK_LOCK_WAIT`.
    async fn lock_disk(&self, id: &str) -> Result<DiskLock, Status> {
        self.lock_disk_within(id, DISK_LOCK_WAIT).await
    }

    async fn lock_disk_within(&self, id: &str, wait: Duration) -> Result<DiskLock, Status> {
        let lock = self
            .disk_locks
            .lock()
            .map_err(|_| internal("poisoned"))?
            .entry(id.to_string())
            .or_default()
            .clone();
        let mut locked = DiskLock {
            id: id.to_string(),
            locks: self.disk_locks.clone(),
            guard: None,
        };
        let acquired = tokio::time::timeout(wait, lock.lock_owned()).await;
        match acquired {
            Ok(guard) => {
                locked.guard = Some(guard);
                Ok(locked)
            }
            Err(_) => Err(Status::unavailable(format!("disk {id} is busy"))),
        }
    }

    /// Loads `diskd.state`, lets `f` change it and stores it, as its one
    /// writer: two calls that change two disks' rows keep both changes.
    fn update_state<R>(
        &self,
        f: impl FnOnce(&mut State) -> Result<R, String>,
    ) -> Result<R, String> {
        // The mutex guards no data: a panic under it leaves nothing torn.
        let _writer = self.state_mu.lock().unwrap_or_else(|e| e.into_inner());
        let mut st = State::load(&self.state_dir).map_err(|e| e.to_string())?;
        let r = f(&mut st)?;
        State::store(&self.state_dir, &st).map_err(|e| e.to_string())?;
        Ok(r)
    }

    pub fn link_path(&self, pod_id: &str) -> PathBuf {
        self.state_dir.join("live").join(pod_id).join("disk")
    }

    pub fn top_path(&self, pod_id: &str) -> PathBuf {
        self.state_dir.join("live").join(pod_id).join("top.img")
    }

    fn load_layer(&self, id: &str) -> Result<disk_store::Manifest, String> {
        let layer_id = disk_store::manifest::LayerId::from_hex(id)
            .map_err(|e| format!("bad layer id {id}: {e}"))?;
        let p = self
            .state_dir
            .join("layers")
            .join(format!("{layer_id}.manifest"));
        let buf = std::fs::read(&p).map_err(|e| format!("{}: {e}", p.display()))?;
        disk_store::Manifest::decode(&buf).map_err(|e| e.to_string())
    }

    fn load_chain(&self, ids: &[String]) -> Result<Vec<disk_store::Manifest>, String> {
        let mut out = Vec::with_capacity(ids.len());
        for (i, id) in ids.iter().enumerate() {
            let m = self.load_layer(id)?;
            if i > 0 {
                let want = disk_store::manifest::LayerId::from_hex(&ids[i - 1])
                    .map_err(|e| e.to_string())?;
                if m.parent != Some(want) {
                    return Err(format!("chain not contiguous at {id}"));
                }
            }
            out.push(m);
        }
        Ok(out)
    }

    fn layer_path(&self, id_hex: &str) -> PathBuf {
        self.state_dir
            .join("layers")
            .join(format!("{id_hex}.manifest"))
    }

    /// Startup/drill recovery: reattach a previously-seen device.
    pub(crate) fn spawn_dev_recovery(
        &self,
        row: DeviceRow,
        pod_id: String,
    ) -> Result<pb::Device, String> {
        self.spawn_dev(row, pod_id, true, false)
    }

    /// Everything device spawn needs (runs on a blocking thread). A
    /// `new_row` (CreateDisk, OpenDisk from a head) is stored as it is;
    /// otherwise the row in the state gets the device.
    fn spawn_dev(
        &self,
        row: DeviceRow,
        pod_id: String,
        recover: bool,
        new_row: bool,
    ) -> Result<pb::Device, String> {
        let chain = self.load_chain(&row.chain)?;
        let logical = row.logical_size;
        let live = self.state_dir.join("live").join(&pod_id);
        std::fs::create_dir_all(&live).map_err(|e| e.to_string())?;
        let top = self.top_path(&pod_id);
        if !top.is_file() {
            let f = std::fs::File::create(&top).map_err(|e| e.to_string())?;
            f.set_len(logical).map_err(|e| e.to_string())?;
        }

        let started = dev::spawn(
            DeviceCtx {
                pod_id: pod_id.clone(),
                state_dir: self.state_dir.clone(),
                chain: Arc::new(chain),
                top: Some(top),
                logical_size: logical,
                cache: self.cache.clone(),
                s3: self.s3.clone(),
                shared: Arc::new(dev::DevShared::default()),
                fetch_concurrency: self.s3_concurrency,
            },
            recover,
        )?;
        let dev_id = started.dev_id;
        let bdev = started.bdev.clone();

        // The symlink Firecracker is configured with (ADR 0011).
        let link = self.link_path(&pod_id);
        let _ = std::fs::remove_file(&link);
        std::os::unix::fs::symlink(&bdev, &link).map_err(|e| e.to_string())?;
        self.devices
            .lock()
            .map_err(|_| "poisoned")?
            .insert(pod_id.clone(), started);

        self.update_state(|st| {
            put_device_row(st, &pod_id, row, dev_id, new_row);
            Ok(())
        })?;

        Ok(pb::Device {
            pod_id,
            dev_path: bdev.display().to_string(),
            link_path: link.display().to_string(),
            size_bytes: logical,
        })
    }
}

// ---- upload tracking -----------------------------------------------------
//
// Per-chunk upload markers under `uploaded/chunks/<hh>/<hash>`: `Push` skips marked chunks — this is what makes memory
// "increments" and flatten cheap on the wire (an unchanged page hashes to
// a chunk S3 already has and is not uploaded again). Markers are HINTS,
// never durability-proofed: they are written unsynced, a lost marker
// costs one redundant PUT, and `Fsck --check-s3` rebuilds them from S3
// truth (marker present ⟺ object present).

impl DiskSvc {
    fn chunk_marker_dir(&self) -> PathBuf {
        self.state_dir.join("uploaded").join("chunks")
    }

    fn chunk_marker(&self, h: &disk_store::chunk::Hash) -> PathBuf {
        let hex = h.to_hex().to_string();
        self.chunk_marker_dir().join(&hex[0..2]).join(hex)
    }

    fn chunk_uploaded(&self, h: &disk_store::chunk::Hash) -> bool {
        self.chunk_marker(h).is_file()
    }

    /// Record "this chunk is in S3". Hint only — no fsync (see module
    /// comment above); errors are logged, not fatal.
    fn mark_chunk_uploaded(&self, h: &disk_store::chunk::Hash) {
        let p = self.chunk_marker(h);
        if p.is_file() {
            return;
        }
        if let Some(dir) = p.parent()
            && !dir.is_dir()
        {
            let _ = std::fs::create_dir_all(dir);
        }
        if let Err(e) = std::fs::write(&p, b"") {
            tracing::debug!(hash = %h.to_hex(), error = %e, "chunk marker write failed");
        }
    }

    fn unmark_chunk(&self, h: &disk_store::chunk::Hash) {
        let _ = std::fs::remove_file(self.chunk_marker(h));
    }

    fn mark_uploaded(&self, layer_id: &str) -> Result<(), String> {
        let dir = self.state_dir.join("uploaded");
        std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
        std::fs::write(
            dir.join(layer_id),
            "
",
        )
        .map_err(|e| e.to_string())
    }

    /// Chunks that must never be evicted by policy: chunks of live device
    /// chains plus chunks of layers not yet pushed (tenure).
    fn tenure_protected(&self) -> std::collections::HashSet<disk_store::chunk::Hash> {
        let mut keep = std::collections::HashSet::new();
        let add_layer =
            |id: &str, keep: &mut std::collections::HashSet<disk_store::chunk::Hash>| {
                if let Ok(m) = self.load_layer(id) {
                    for e in &m.entries {
                        if e.kind == disk_store::manifest::EntryKind::Data {
                            keep.insert(disk_store::chunk::Hash::from(e.hash));
                        }
                    }
                }
            };
        if let Ok(st) = State::load(&self.state_dir) {
            for row in st.devices.values() {
                for id in &row.chain {
                    add_layer(id, &mut keep);
                }
            }
        }
        if let Ok(rd) = std::fs::read_dir(self.state_dir.join("layers")) {
            for e in rd.flatten() {
                let id = e
                    .file_name()
                    .to_string_lossy()
                    .trim_end_matches(".manifest")
                    .to_string();
                if !self.state_dir.join("uploaded").join(&id).is_file() {
                    add_layer(&id, &mut keep);
                }
            }
        }
        keep
    }

    /// Push a layer and all missing ancestors, oldest-first (a chain is
    /// only usable whole on another node). With a `journal`, the chunks and
    /// layers it uploads are listed there before they are marked uploaded.
    async fn push_chain(&self, id_hex: &str, journal: Option<Journal<'_>>) -> Result<(), String> {
        self.expire_journals_now().await;
        // collect the not-yet-uploaded chain segment, head -> old
        let mut pending: Vec<String> = Vec::new();
        let mut cur = id_hex.to_string();
        let s3 = self.s3.clone();
        loop {
            let m = self.load_layer(&cur)?;
            let marker = self.state_dir.join("uploaded").join(&cur).is_file();
            // marker + real probe: stale markers (e.g. lab cleanup
            // deleting manifests) must not masquerade as tiered
            let tiered = if marker {
                if let Some(s3) = &s3 {
                    let lid =
                        disk_store::manifest::LayerId::from_hex(&cur).map_err(|e| e.to_string())?;
                    s3.has_manifest(&lid).await
                } else {
                    marker
                }
            } else {
                false
            };
            // Scan the WHOLE chain — an ancestor's upload may be stale
            // even when the head is fine (S3 deletions, partial pushes).
            if !tiered {
                pending.push(cur.clone());
            }
            match m.parent {
                Some(p) => cur = p.hex(),
                None => break,
            }
        }
        let pending: Vec<String> = pending.into_iter().rev().collect();
        for id in &pending {
            self.push_layer(id, journal).await?;
        }
        Ok(())
    }

    /// Push one layer: chunks (marked-uploaded ones skipped) at bounded
    /// concurrency, manifest last (crash-safe ordering, ADR 0011). The
    /// caller expires the journals first (`expire_journals_now`).
    async fn push_layer(&self, id_hex: &str, journal: Option<Journal<'_>>) -> Result<(), String> {
        set_sched_idle();
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| "S3 not configured".to_string())?;
        let lid = disk_store::manifest::LayerId::from_hex(id_hex).map_err(|e| e.to_string())?;
        let m = self.load_layer(id_hex)?;
        let hashes: Vec<disk_store::chunk::Hash> = m
            .entries
            .iter()
            .filter(|e| e.kind == disk_store::manifest::EntryKind::Data)
            .map(|e| disk_store::chunk::Hash::from(e.hash))
            .collect();
        let n_bytes = AtomicU64::new(0);
        let conc = self.s3_concurrency.max(1);
        // Each chunk: Ok(Some) when it is in S3 now and needs its marker.
        let mut results = futures::stream::iter(hashes)
            .map(|h| {
                let s3 = s3.clone();
                let n_bytes = &n_bytes;
                async move {
                    if self.chunk_uploaded(&h) {
                        return Ok(None);
                    }
                    let p = self.cache.chunk_path(&h);
                    if p.is_file() {
                        let raw = tokio::task::spawn_blocking({
                            let p = p.clone();
                            move || std::fs::read(&p)
                        })
                        .await
                        .map_err(|e| e.to_string())?
                        .map_err(|e| e.to_string())?;
                        s3.put_chunk(&h, &raw).await.map_err(|e| e.to_string())?;
                        n_bytes.fetch_add(raw.len() as u64, Ordering::Relaxed);
                    } else if s3.has_chunk(&h).await {
                        // Evicted locally, present remotely (the memory-increment
                        // case): learn the marker without a re-upload.
                    } else {
                        return Err(format!("chunk missing locally and in S3: {}", h.to_hex()));
                    }
                    Ok(Some(h))
                }
            })
            .buffer_unordered(conc);
        // Every chunk is tried before an error counts; the markers go in
        // batches, each after it is in the journal.
        let mut first_err: Option<String> = None;
        let mut batch: Vec<Hash> = Vec::with_capacity(MARK_BATCH);
        while let Some(r) = results.next().await {
            match r {
                Ok(Some(h)) => {
                    batch.push(h);
                    if batch.len() >= MARK_BATCH {
                        self.mark_pushed(std::mem::take(&mut batch), journal).await;
                    }
                }
                Ok(None) => {}
                Err(e) => {
                    first_err.get_or_insert(e);
                }
            }
        }
        self.mark_pushed(batch, journal).await;
        if let Some(e) = first_err {
            return Err(e);
        }
        s3.put_manifest(&lid, &m).await.map_err(|e| e.to_string())?;
        let n_bytes = n_bytes.load(Ordering::Relaxed);
        self.mark_layer_pushed(id_hex, n_bytes, journal).await?;
        info!(layer = %id_hex, bytes = n_bytes, "push: layer uploaded");
        Ok(())
    }

    /// Writes the marker of a layer whose manifest is in S3 now: what
    /// `push_chain` trusts. With a disk's journal, the layer is listed in
    /// it first, durably; once that journal is gone, no marker is written.
    async fn mark_layer_pushed(
        &self,
        id_hex: &str,
        n_bytes: u64,
        journal: Option<Journal<'_>>,
    ) -> Result<(), String> {
        let marker = self.state_dir.join("uploaded").join(id_hex);
        let write_marker = move || -> Result<(), String> {
            if let Some(dir) = marker.parent() {
                std::fs::create_dir_all(dir).map_err(|e| e.to_string())?;
            }
            std::fs::write(&marker, format!("{n_bytes}\n")).map_err(|e| e.to_string())
        };
        let Some(Journal::Disk(journal)) = journal else {
            return write_marker();
        };
        // As in mark_pushed: both, or neither, before an expiry looks.
        let journals = self.journal_mu.clone().lock_owned().await;
        let this = self.clone();
        let journal = journal.to_path_buf();
        let line = format!("{LAYER_LINE}{id_hex}\n");
        tokio::task::spawn_blocking(move || {
            let _journals = journals;
            let listed = append_journal(&journal, &line, None, &this.journal_tmp())
                .map_err(|e| format!("{}: {e}", journal.display()))?;
            if listed {
                write_marker()?;
            }
            Ok(())
        })
        .await
        .map_err(|e| e.to_string())?
    }

    /// Ensure the manifest of `id_hex` and its ancestors are local (one
    /// S3 fetch walk), marking every fetched layer uploaded. The S3-side
    /// existence of a manifest proves every chunk it references is in S3
    /// (chunks-before-manifest upload rule) — so chunk markers are
    /// learned for free and later pushes dedup against them.
    /// A manifest for the sweep's reachability walk: the local copy when
    /// this node happens to have one, otherwise straight from S3. Never
    /// caches — the sweeper must not pull the whole bucket onto a node.
    async fn manifest_for_sweep(
        &self,
        id_hex: &str,
        s3: &disk_store::s3::S3,
    ) -> Result<disk_store::Manifest, String> {
        if self.layer_path(id_hex).is_file() {
            return self.load_layer(id_hex);
        }
        let lid = disk_store::manifest::LayerId::from_hex(id_hex).map_err(|e| e.to_string())?;
        s3.get_manifest(&lid)
            .await
            .map_err(|e| format!("sweep: manifest {id_hex}: {e}"))
    }

    /// Transitive parents of `ids`. A head names its whole chain today,
    /// but the catalog names only the baked layer — its parent L0 is
    /// reachable and must survive.
    async fn expand_parents(
        &self,
        ids: &std::collections::HashSet<String>,
        s3: &disk_store::s3::S3,
    ) -> Vec<String> {
        let mut out = Vec::new();
        let mut seen: std::collections::HashSet<String> = ids.clone();
        let mut queue: Vec<String> = ids.iter().cloned().collect();
        while let Some(id) = queue.pop() {
            let m = match self.manifest_for_sweep(&id, s3).await {
                Ok(m) => m,
                // A live id with no manifest anywhere is the caller's
                // problem, not the sweeper's; skipping it only ever
                // keeps MORE objects (its chunks stay unreferenced-
                // looking, and their own grace still protects them).
                Err(_) => continue,
            };
            if let Some(parent) = m.parent {
                let pid = parent.hex();
                if seen.insert(pid.clone()) {
                    out.push(pid.clone());
                    queue.push(pid);
                }
            }
        }
        out
    }

    async fn ensure_manifests(&self, id_hex: &str) -> Result<(), String> {
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| "S3 not configured".to_string())?;
        let mut id = id_hex.to_string();
        loop {
            let path = self.layer_path(&id);
            let m = if path.is_file() {
                self.load_layer(&id)?
            } else {
                let lid =
                    disk_store::manifest::LayerId::from_hex(&id).map_err(|e| e.to_string())?;
                let m = s3.get_manifest(&lid).await.map_err(|e| e.to_string())?;
                if m.layer_id() != lid {
                    return Err(format!("manifest id mismatch for {id}"));
                }
                std::fs::create_dir_all(self.state_dir.join("layers"))
                    .map_err(|e| e.to_string())?;
                // Two disks over this layer may fetch it at once.
                let tmp = disk_store::cache::staging_tmp(
                    &self.cache.staging_dir(),
                    &format!("{id}.manifest"),
                );
                std::fs::write(&tmp, m.encode()).map_err(|e| e.to_string())?;
                std::fs::rename(&tmp, &path).map_err(|e| e.to_string())?;
                for e in &m.entries {
                    if e.kind == disk_store::manifest::EntryKind::Data {
                        self.mark_chunk_uploaded(&disk_store::chunk::Hash::from(e.hash));
                    }
                }
                self.mark_uploaded(&id)?;
                m
            };
            match m.parent {
                Some(p) => id = p.hex(),
                None => return Ok(()),
            }
        }
    }
}

// ---- unheaded disks, and pins ---------------------------------------------
//
// `unheaded/<disk>` exists while no head names the disk's own layer: from
// the seal of a checkpoint that was not finished (abandoned, failed) until
// PutHead of the disk, RemoveDevice, or an OpenDisk from a head of the
// pod. Meanwhile that layer may be the only copy of what the pod wrote:
// its chunks, and those of every layer of a device row that is not
// uploaded, are pinned in the cache (`Cache::pin`).
//
// The file is a journal as well: its first line the unix time it started
// at, then what a push uploaded for the disk, one per line, each listed
// before it is marked uploaded: a chunk, or a layer (`L <layer>`). Nothing
// in the bucket names those yet, and a sweep deletes them once they are
// older than its grace; so their markers are trusted for
// UNHEADED_TRUST_SECONDS only, then they are uploaded again. Only Seal
// starts a disk's journal: a push that still runs once PutHead or
// RemoveDevice has removed it marks nothing. A memory layer that no head
// names yet has a journal of its chunks in `unheaded-memory/<layer>`,
// which its first chunk starts.

/// The journal a push lists what it uploads in.
#[derive(Clone, Copy)]
enum Journal<'a> {
    /// `unheaded/<disk>`: chunks and layers.
    Disk(&'a Path),
    /// `unheaded-memory/<layer>`: chunks. SealFile puts the layer's
    /// manifest every time, whatever its marker says.
    Memory(&'a Path),
}

impl Journal<'_> {
    fn path(&self) -> &Path {
        match self {
            Journal::Disk(path) | Journal::Memory(path) => path,
        }
    }

    /// When a journal that is not there starts, if a push starts it.
    fn start(&self, now: u64) -> Option<u64> {
        match self {
            Journal::Disk(_) => None,
            Journal::Memory(_) => Some(now),
        }
    }
}

/// How a journal lists a layer.
const LAYER_LINE: &str = "L ";

impl DiskSvc {
    fn unheaded_path(&self, disk: &str) -> PathBuf {
        self.state_dir.join("unheaded").join(disk)
    }

    fn memory_journal_path(&self, layer: &str) -> PathBuf {
        self.state_dir.join("unheaded-memory").join(layer)
    }

    fn journal_tmp(&self) -> PathBuf {
        self.state_dir.join("staging").join("journal.tmp")
    }

    /// Marks the disk unheaded, unless it is: a journal that starts now.
    /// Blocking context.
    fn mark_unheaded(&self, disk: &str) -> Result<(), String> {
        let _journals = self.journal_mu.blocking_lock();
        let path = self.unheaded_path(disk);
        if !path.is_file() {
            write_journal(&path, unix_now(), &self.journal_tmp())
                .map_err(|e| format!("{}: {e}", path.display()))?;
        }
        Ok(())
    }

    /// Removes a journal: a head names its layer now, or its disk is gone.
    /// Blocking context.
    fn remove_journal(&self, path: &Path) -> Result<(), String> {
        let _journals = self.journal_mu.blocking_lock();
        match std::fs::remove_file(path) {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(format!("{}: {e}", path.display())),
        }
    }

    /// Marks chunks that are in S3 now uploaded. With a journal, they are
    /// listed in it first, durably, and the expiry waits for both. Once a
    /// disk's journal is gone, they are not marked: the disk is headed or
    /// removed, and a later push uploads them again.
    async fn mark_pushed(&self, hashes: Vec<Hash>, journal: Option<Journal<'_>>) {
        if hashes.is_empty() {
            return;
        }
        let Some(journal) = journal else {
            for h in &hashes {
                self.mark_chunk_uploaded(h);
            }
            return;
        };
        // Moved into the blocking task: a push dropped here still writes
        // both, or neither, before an expiry can look.
        let journals = self.journal_mu.clone().lock_owned().await;
        let this = self.clone();
        let start = journal.start(unix_now());
        let journal = journal.path().to_path_buf();
        let marked = tokio::task::spawn_blocking(move || {
            let _journals = journals;
            let mut lines = String::with_capacity(hashes.len() * 65);
            for h in &hashes {
                lines.push_str(h.to_hex().as_str());
                lines.push('\n');
            }
            let listed = append_journal(&journal, &lines, start, &this.journal_tmp())
                .map_err(|e| format!("{}: {e}", journal.display()))?;
            if listed {
                for h in &hashes {
                    this.mark_chunk_uploaded(h);
                }
            } else {
                debug!(journal = %journal.display(), "push: the disk is headed or gone; chunks left unmarked");
            }
            Ok::<(), String>(())
        })
        .await;
        match marked {
            Ok(Ok(())) => {}
            // Unmarked chunks are uploaded again by the next push.
            Ok(Err(e)) => warn!(error = %e, "push: chunks left unmarked"),
            Err(e) => warn!(error = %e, "push: chunks left unmarked"),
        }
    }

    async fn expire_journals_now(&self) {
        let this = self.clone();
        let _ = tokio::task::spawn_blocking(move || {
            if this.expire_journals(unix_now()) {
                // A layer that is not uploaded is pinned.
                this.refresh_pins();
            }
        })
        .await;
    }

    /// Ends the trust in the journals that started more than
    /// UNHEADED_TRUST_SECONDS before `now`: the chunks and layers they list
    /// lose their markers, and a disk's journal starts again at `now`, empty
    /// (the disk stays unheaded); a memory layer's, or that of a disk this
    /// node no longer has, goes. Returns whether a layer lost its marker.
    /// Blocking context.
    fn expire_journals(&self, now: u64) -> bool {
        let _journals = self.journal_mu.blocking_lock();
        let rows = State::load(&self.state_dir).ok();
        let mut layers_unmarked = false;
        for (dir, of_disks) in [
            (self.state_dir.join("unheaded"), true),
            (self.state_dir.join("unheaded-memory"), false),
        ] {
            let Ok(rd) = std::fs::read_dir(&dir) else {
                continue;
            };
            for e in rd.flatten() {
                let path = e.path();
                let Ok(f) = std::fs::File::open(&path) else {
                    continue;
                };
                let mut lines = std::io::BufRead::lines(std::io::BufReader::new(f));
                // A torn first line counts as old: expiring costs uploads.
                let since = lines
                    .next()
                    .and_then(|l| l.ok())
                    .and_then(|l| l.trim().parse::<u64>().ok())
                    .unwrap_or(0);
                if now.saturating_sub(since) <= UNHEADED_TRUST_SECONDS {
                    continue;
                }
                let mut chunks = 0u64;
                let mut layers = 0u64;
                for l in lines.map_while(Result::ok) {
                    let l = l.trim();
                    if let Some(layer) = l.strip_prefix(LAYER_LINE) {
                        // The next push of a chain that names it puts it
                        // again. A torn line names no layer.
                        if disk_store::manifest::LayerId::from_hex(layer).is_ok() {
                            let _ =
                                std::fs::remove_file(self.state_dir.join("uploaded").join(layer));
                            layers += 1;
                        }
                    } else if let Ok(h) = Hash::from_hex(l) {
                        self.unmark_chunk(&h);
                        chunks += 1;
                    }
                }
                layers_unmarked |= layers > 0;
                let disk = e.file_name().to_string_lossy().to_string();
                let keep = of_disks
                    && rows
                        .as_ref()
                        .is_none_or(|st| st.devices.contains_key(&disk));
                let done = if keep {
                    write_journal(&path, now, &self.journal_tmp())
                } else {
                    std::fs::remove_file(&path)
                };
                if let Err(e) = done {
                    warn!(journal = %path.display(), error = %e, "unheaded: journal not reset");
                }
                info!(journal = %path.display(), chunks, layers, "unheaded: markers expired");
            }
        }
        layers_unmarked
    }

    /// The chunks to pin: those of every device row's layers that are not
    /// uploaded, and of an unheaded disk's own layer. Rows only: a memory
    /// or baked layer is in no row, and stays evictable.
    fn pinned_row_chunks(&self) -> Result<HashSet<Hash>, String> {
        let st = State::load(&self.state_dir).map_err(|e| e.to_string())?;
        let mut layers: BTreeSet<&str> = BTreeSet::new();
        for (disk, row) in &st.devices {
            let unheaded = self.unheaded_path(disk).is_file();
            for (i, id) in row.chain.iter().enumerate() {
                let own = i >= row.shared_depth;
                if (unheaded && own) || !self.state_dir.join("uploaded").join(id).is_file() {
                    layers.insert(id);
                }
            }
        }
        let mut keep = HashSet::new();
        for id in layers {
            match self.load_layer(id) {
                Ok(m) => keep.extend(
                    m.entries
                        .iter()
                        .filter(|e| e.kind == disk_store::manifest::EntryKind::Data)
                        .map(|e| Hash::from(e.hash)),
                ),
                // A row whose layer is gone cannot be read anyway.
                Err(e) => warn!(layer = %id, error = %e, "pins: layer skipped"),
            }
        }
        Ok(keep)
    }

    /// Pins what `pinned_row_chunks` names, in place of what was pinned.
    /// After every change of the rows, the uploads or the unheaded disks,
    /// and at startup. Blocking context.
    pub(crate) fn refresh_pins(&self) {
        let _one = self.pins_mu.lock().unwrap_or_else(|e| e.into_inner());
        match self.pinned_row_chunks() {
            Ok(keep) => {
                debug!(chunks = keep.len(), "pins refreshed");
                self.cache.pin(keep);
            }
            Err(e) => warn!(error = %e, "pins not refreshed"),
        }
    }

    async fn refresh_pins_now(&self) {
        let this = self.clone();
        let _ = tokio::task::spawn_blocking(move || this.refresh_pins()).await;
    }
}

#[tonic::async_trait]
impl DiskService for DiskSvc {
    async fn import_base(
        &self,
        request: Request<pb::ImportBaseRequest>,
    ) -> Result<Response<pb::Layer>, Status> {
        let req = request.into_inner();
        let this = self.clone();
        let path = req.path.clone();
        let res = tokio::task::spawn_blocking(move || {
            // A compressed image is unpacked first, sparse, next to the
            // cache: the seal walks the image's data extents only.
            let unpacked = path.ends_with(".zst").then(|| {
                static IMPORTS: AtomicU64 = AtomicU64::new(0);
                this.state_dir.join("staging").join(format!(
                    "import-{}-{}.img",
                    std::process::id(),
                    IMPORTS.fetch_add(1, Ordering::Relaxed)
                ))
            });
            let image = match &unpacked {
                Some(raw) => {
                    std::fs::create_dir_all(this.state_dir.join("staging"))
                        .map_err(|e| e.to_string())?;
                    disk_store::materialize::unpack_zstd(std::path::Path::new(&path), raw)
                        .map_err(|e| format!("unpack {path}: {e}"))?;
                    raw.clone()
                }
                None => PathBuf::from(&path),
            };
            let sealed = (|| {
                let md = std::fs::metadata(&image).map_err(|e| e.to_string())?;
                let logical = md.len();
                let chunk_size = if req.chunk_size == 0 {
                    disk_store::chunk::DEFAULT_DISK_CHUNK_SIZE
                } else {
                    req.chunk_size
                };
                disk_store::seal::seal_top(
                    &image,
                    &[],
                    None,
                    &this.cache,
                    &this.state_dir,
                    logical,
                    chunk_size,
                    None,
                )
                .map_err(|e| e.to_string())
            })();
            if let Some(raw) = &unpacked {
                let _ = std::fs::remove_file(raw);
            }
            sealed
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        info!(layer = %res.layer_id, path = %req.path, "import_base");
        Ok(Response::new(pb::Layer {
            layer_id: res.layer_id.hex(),
            entries: res.manifest.entries.len() as u64,
            chunks_new: res.stats.chunks_new,
            chunks_inherited: res.stats.chunks_inherited,
            chunks_zeroed: res.stats.chunks_zeroed,
            chunks_pool_hit: res.stats.chunks_pool_hit,
            bytes_new: res.stats.bytes_new,
            seal_seconds: res.stats.seal_seconds,
            logical_bytes: res.manifest.data_bytes(),
        }))
    }

    async fn create_disk(
        &self,
        request: Request<pb::CreateDiskRequest>,
    ) -> Result<Response<pb::Device>, Status> {
        let req = request.into_inner();
        if req.pod_id.is_empty() || req.chain.is_empty() {
            return Err(Status::invalid_argument("pod_id and chain required"));
        }
        let locked = self.lock_disk(&req.pod_id).await?;
        if self
            .devices
            .lock()
            .map_err(|_| internal("poisoned"))?
            .contains_key(&req.pod_id)
        {
            return Err(Status::already_exists(req.pod_id));
        }
        let chain = self.load_chain(&req.chain).map_err(internal)?;
        let head = chain.last().ok_or_else(|| internal("empty chain"))?;
        // Never smaller than the chain: a disk that cut its layers would
        // cut the filesystem in them.
        let logical = req.logical_size.max(head.logical_size);
        let row = DeviceRow {
            dev_id: None,
            chain: req.chain.clone(),
            logical_size: logical,
            chunk_size: head.chunk_size,
            has_top: true,
            // Every layer handed in at create is shared/immutable; the
            // pod's private layer only appears at the first seal.
            shared_depth: req.chain.len(),
        };
        let this = self.clone();
        let pod_id = req.pod_id;
        let dev = tokio::task::spawn_blocking(move || {
            let _locked = locked;
            let dev = this.spawn_dev(row, pod_id, false, true);
            // A chain whose layers are not uploaded is pinned.
            this.refresh_pins();
            dev
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        Ok(Response::new(dev))
    }

    async fn open_disk(
        &self,
        request: Request<pb::OpenDiskRequest>,
    ) -> Result<Response<pb::Device>, Status> {
        let req = request.into_inner();
        let (pod_id, row, locked) = if req.head_key.is_empty() {
            // After a seal of the disk that still runs, also one whose
            // caller went away: the device starts over its new layer, or
            // over its top as it was if the seal stopped before it
            // committed the layer.
            let locked = self.lock_disk(&req.pod_id).await?;
            // Started already: its caller lost the answer, or never knew.
            let live = self
                .devices
                .lock()
                .map_err(|_| internal("poisoned"))?
                .get(&req.pod_id)
                .map(|d| d.bdev.clone());
            if let Some(bdev) = live {
                let st = State::load(&self.state_dir).map_err(internal)?;
                return Ok(Response::new(pb::Device {
                    dev_path: bdev.display().to_string(),
                    link_path: self.link_path(&req.pod_id).display().to_string(),
                    size_bytes: st
                        .devices
                        .get(&req.pod_id)
                        .map_or(0, |row| row.logical_size),
                    pod_id: req.pod_id,
                }));
            }
            let st = State::load(&self.state_dir).map_err(internal)?;
            let row = st
                .devices
                .get(&req.pod_id)
                .cloned()
                .ok_or_else(|| Status::not_found(req.pod_id.clone()))?;
            (req.pod_id.clone(), row, locked)
        } else {
            // Restore on a foreign node: pull the head and its manifests
            // from S3, create the row here, start over the chain with a
            // fresh top.
            let s3 = self
                .s3
                .clone()
                .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
            let buf = s3
                .get_object(&req.head_key)
                .await
                .map_err(|e| internal(format!("head {}: {e}", req.head_key)))?;
            let head = disk_store::head::PodHead::decode(&buf)
                .map_err(|e| internal(format!("head {}: {e}", req.head_key)))?;
            if !req.pod_id.is_empty() && req.pod_id != head.pod_id {
                return Err(Status::invalid_argument(format!(
                    "pod {} != head's {}",
                    req.pod_id, head.pod_id
                )));
            }
            let Some(chain_head) = head.chain.last() else {
                return Err(Status::invalid_argument("head has an empty chain"));
            };
            let locked = self.lock_disk(&head.pod_id).await?;
            if self
                .devices
                .lock()
                .map_err(|_| internal("poisoned"))?
                .contains_key(&head.pod_id)
            {
                return Err(Status::already_exists(head.pod_id));
            }
            // A disk the node has of the pod may hold writes the head does
            // not: its top would end up over another chain.
            if State::load(&self.state_dir)
                .map_err(internal)?
                .devices
                .contains_key(&head.pod_id)
            {
                return Err(Status::failed_precondition(format!(
                    "this node has a disk of pod {} already: remove it first",
                    head.pod_id
                )));
            }
            self.ensure_manifests(chain_head).await.map_err(internal)?;
            // The pod's own layer stays its own, so that the next seal
            // replaces it and the chain keeps its shape; a head that does
            // not say which it is holds the whole chain as shared.
            let row = DeviceRow {
                dev_id: None,
                chain: head.chain.clone(),
                logical_size: head.logical_size,
                chunk_size: head.chunk_size,
                has_top: true,
                shared_depth: head.shared_depth(),
            };
            (head.pod_id.clone(), row, locked)
        };
        let this = self.clone();
        let new_row = !req.head_key.is_empty();
        let dev = tokio::task::spawn_blocking(move || {
            let _locked = locked;
            if new_row && let Err(e) = this.remove_journal(&this.unheaded_path(&pod_id)) {
                // A head names the disk's layer: a journal of an earlier
                // disk of the pod costs pins until its next head.
                warn!(pod = %pod_id, error = %e, "open_disk: journal left");
            }
            let dev = this.spawn_dev(row, pod_id, false, new_row);
            if new_row {
                this.refresh_pins();
            }
            dev
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        Ok(Response::new(dev))
    }

    async fn stop_disk(
        &self,
        request: Request<pb::StopDiskRequest>,
    ) -> Result<Response<pb::Empty>, Status> {
        let req = request.into_inner();
        let locked = self.lock_disk(&req.pod_id).await?;
        let started = {
            let mut t = self.devices.lock().map_err(|_| internal("poisoned"))?;
            t.remove(&req.pod_id)
                .ok_or_else(|| Status::not_found(req.pod_id.clone()))?
        };
        let this = self.clone();
        let pod = req.pod_id.clone();
        // The stop and the row go together: a caller that goes away
        // meanwhile leaves no row that names a device that is gone.
        tokio::task::spawn_blocking(move || -> Result<(), String> {
            let _locked = locked;
            dev::stop(started)?;
            let _ = std::fs::remove_file(this.link_path(&pod));
            // Row stays (top kept); clear the dev id.
            this.update_state(|st| {
                if let Some(row) = st.devices.get_mut(&pod) {
                    row.dev_id = None;
                }
                Ok(())
            })
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        info!(pod = %req.pod_id, "stop_disk");
        Ok(Response::new(pb::Empty {}))
    }

    async fn remove_device(
        &self,
        request: Request<pb::RemoveDeviceRequest>,
    ) -> Result<Response<pb::RemoveDeviceResponse>, Status> {
        let req = request.into_inner();
        if req.pod_id.is_empty() {
            return Err(Status::invalid_argument("pod_id required"));
        }
        let locked = self.lock_disk(&req.pod_id).await?;
        if self
            .devices
            .lock()
            .map_err(|_| internal("poisoned"))?
            .contains_key(&req.pod_id)
        {
            return Err(Status::failed_precondition(
                "device still live; stop it first",
            ));
        }
        let this = self.clone();
        let pod = req.pod_id;
        tokio::task::spawn_blocking(move || -> Result<(), String> {
            let _locked = locked;
            this.update_state(|st| {
                st.devices.remove(&pod);
                Ok(())
            })?;
            // The disk is gone: nothing of it needs pins or uploads any
            // more. Its journal goes only now, so that a row the call
            // could not remove keeps its mark.
            let journal = this.remove_journal(&this.unheaded_path(&pod));
            let dir = this.state_dir.join("live").join(&pod);
            let live = if dir.exists() {
                std::fs::remove_dir_all(&dir).map_err(|e| e.to_string())
            } else {
                Ok(())
            };
            this.refresh_pins();
            journal.and(live)
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        Ok(Response::new(pb::RemoveDeviceResponse {}))
    }

    async fn seal(&self, request: Request<pb::SealRequest>) -> Result<Response<pb::Layer>, Status> {
        let req = request.into_inner();
        if req.pod_id.is_empty() {
            return Err(Status::invalid_argument("pod_id required"));
        }
        let locked = self.lock_disk(&req.pod_id).await?;
        if self
            .devices
            .lock()
            .map_err(|_| internal("poisoned"))?
            .contains_key(&req.pod_id)
        {
            return Err(Status::failed_precondition(
                "device still live; stop_disk first",
            ));
        }
        if req.upload && self.s3.is_none() {
            return Err(Status::failed_precondition("S3 not configured"));
        }
        let this = self.clone();
        let pod = req.pod_id.clone();
        let pod_log = pod.clone();
        // Set when the call is dropped: a caller that goes away before the
        // new layer is committed cancels the local seal.
        let gone = Arc::new(AtomicBool::new(false));
        let _gone_with_the_call = SetOnDrop(gone.clone());
        let res = tokio::task::spawn_blocking(move || {
            // The disk's lock goes with the local seal: until it is
            // committed, or stopped because its caller went away.
            let _locked = locked;
            #[cfg(test)]
            if let Some(hook) = &this.seal_hook {
                hook(SealPoint::Locked);
            }
            // The new layer's chunks, from their admission until the pins
            // below name them.
            let hold = this.cache.hold();
            let sealed = (|| -> Result<disk_store::seal::SealOutcome, String> {
                let st = State::load(&this.state_dir).map_err(|e| e.to_string())?;
                let row = st
                    .devices
                    .get(&pod)
                    .ok_or_else(|| format!("no device row for {pod}"))?
                    .clone();
                // No head names the layer this seal makes until PutHead:
                // the node may hold its only copy (pinned, see
                // pinned_row_chunks). A seal that fails leaves the mark;
                // the next PutHead or RemoveDevice of the disk takes it.
                this.mark_unheaded(&pod)?;
                let chain = this.load_chain(&row.chain)?;
                // Flatten-on-seal (ADR 0011): the chain has
                // the fixed shape <shared…, S, top>. Exactly one private
                // layer S may sit above the shared prefix; the seal
                // replaces it with a merged S' (parent = last shared).
                let depth = row.shared_depth.min(chain.len());
                if chain.len() > depth + 1 {
                    return Err(format!(
                        "row {} has {} private layers (chain shape invariant broken)",
                        pod,
                        chain.len() - depth
                    ));
                }
                let merged: Option<&disk_store::Manifest> = if chain.len() > depth {
                    chain.last()
                } else {
                    None
                };
                let parents: Vec<&disk_store::Manifest> = chain.iter().collect();
                let top = this.top_path(&pod);
                if !top.is_file() {
                    // Sealed already, by a seal whose caller lost the
                    // answer: the row names the newest layer, and the top
                    // goes only after it does (below).
                    let newest = row.chain.last().ok_or("empty chain")?;
                    let manifest = this.load_layer(newest)?;
                    return Ok(disk_store::seal::SealOutcome {
                        layer_id: manifest.layer_id(),
                        manifest_path: this.layer_path(newest),
                        manifest,
                        stats: Default::default(),
                    });
                }
                // A caller that goes away stops the seal up to the commit
                // of its manifest. The row, the top and the chain are as
                // they were then; the disk keeps its unheaded mark, as
                // after a seal that failed.
                let outcome = disk_store::seal::seal_top_held(
                    &top,
                    &parents,
                    merged,
                    &this.cache,
                    &hold,
                    &this.state_dir,
                    row.logical_size,
                    row.chunk_size,
                    &|| gone.load(Ordering::Relaxed),
                )
                .map_err(|e| {
                    if matches!(e, disk_store::Error::SealCancelled) {
                        info!(pod = %pod, "seal cancelled, its caller gone: the disk is as it was");
                    }
                    e.to_string()
                })?;
                #[cfg(test)]
                if let Some(hook) = &this.seal_hook {
                    hook(SealPoint::Committed);
                }
                // Commit order: seal_top already committed chunks+manifest;
                // now point the row at shared… + S', and then delete the
                // top, which S' holds. A crash in between leaves a top
                // over S' that reads as S' does. From here on the seal
                // ends whether or not its caller is there.
                let new_id = outcome.layer_id.hex();
                let old_private: Option<String> = if row.chain.len() > depth {
                    row.chain.last().cloned()
                } else {
                    None
                };
                let new_chain = this.update_state(|st2| {
                    let row2 = st2
                        .devices
                        .get_mut(&pod)
                        .ok_or_else(|| format!("no device row for {pod}"))?;
                    row2.chain.truncate(depth);
                    row2.chain.push(new_id.clone());
                    row2.dev_id = None;
                    row2.has_top = false;
                    Ok(row2.chain.clone())
                })?;
                std::fs::remove_file(&top).map_err(|e| e.to_string())?;
                // head.json is committed after the row (proto contract);
                // existing memory refs survive a disk-only seal.
                let mut head = disk_store::head::PodHead::load(&this.state_dir, &pod)
                    .unwrap_or_else(|_| {
                        disk_store::head::PodHead::new(
                            &pod,
                            row.logical_size,
                            row.chunk_size,
                            new_chain.clone(),
                        )
                    });
                head.chain = new_chain;
                head.shared_depth = Some(depth.max(1));
                head.has_top = false;
                head.store(&this.state_dir).map_err(|e| e.to_string())?;
                // The replaced private layer is local garbage the moment
                // the row points at S' (GCP: S's exclusively-owned chunks
                // are fsck garbage; an S3 copy, if any, is the sweeper's
                // business). Same id (idempotent reseal) → keep.
                if let Some(old) = old_private
                    && old != new_id
                {
                    let _ = std::fs::remove_file(this.layer_path(&old));
                }
                Ok(outcome)
            })();
            // The row names the new layer now, or the seal failed: its
            // chunks are pinned, or go back to the budget, with the hold.
            this.refresh_pins();
            drop(hold);
            sealed
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        if req.upload {
            // Dropped with the call: what is uploaded stays, listed in the
            // disk's journal.
            let journal = self.unheaded_path(&pod_log);
            self.push_chain(&res.layer_id.hex(), Some(Journal::Disk(&journal)))
                .await
                .map_err(|e| Status::internal(format!("seal uploaded but push failed: {e}")))?;
            self.refresh_pins_now().await;
        }
        info!(pod = %pod_log, layer = %res.layer_id, upload = req.upload, "seal");
        Ok(Response::new(pb::Layer {
            layer_id: res.layer_id.hex(),
            entries: res.manifest.entries.len() as u64,
            chunks_new: res.stats.chunks_new,
            chunks_inherited: res.stats.chunks_inherited,
            chunks_zeroed: res.stats.chunks_zeroed,
            chunks_pool_hit: res.stats.chunks_pool_hit,
            bytes_new: res.stats.bytes_new,
            seal_seconds: res.stats.seal_seconds,
            logical_bytes: res.manifest.data_bytes(),
        }))
    }

    async fn bake(&self, request: Request<pb::BakeRequest>) -> Result<Response<pb::Layer>, Status> {
        let req = request.into_inner();
        if req.pod_id.is_empty() {
            return Err(Status::invalid_argument("pod_id required"));
        }
        // Live seal (ADR 0011): the device keeps serving.
        let shared = {
            self.devices
                .lock()
                .map_err(|_| internal("poisoned"))?
                .get(&req.pod_id)
                .map(|d| d.shared.clone())
        }
        .ok_or_else(|| Status::failed_precondition("device not live"))?;
        // Write guard (before): the guest MUST be frozen; a write during
        // the seal makes the layer torn.
        let writes_before = shared.stats.writes.load(Ordering::Relaxed);
        let this = self.clone();
        let pod = req.pod_id.clone();
        let pod_log = pod.clone();
        let res = tokio::task::spawn_blocking(
            move || -> Result<disk_store::seal::SealOutcome, String> {
                let st = State::load(&this.state_dir).map_err(|e| e.to_string())?;
                let row = st
                    .devices
                    .get(&pod)
                    .ok_or_else(|| format!("no device row for {pod}"))?
                    .clone();
                let chain = this.load_chain(&row.chain)?;
                let parents: Vec<&disk_store::Manifest> = chain.iter().collect();
                let top = this.top_path(&pod);
                if !top.is_file() {
                    return Err("no mutable top; nothing to bake".into());
                }
                disk_store::seal::seal_top(
                    &top,
                    &parents,
                    None, // never merge: the row chain is not mutated
                    &this.cache,
                    &this.state_dir,
                    row.logical_size,
                    row.chunk_size,
                    None,
                )
                .map_err(|e| e.to_string())
            },
        )
        .await
        .map_err(internal)?
        .map_err(internal)?;
        // Write guard (after): any write in between → torn layer; discard
        // the manifest (orphaned chunks are GC garbage) and fail.
        let writes_after = shared.stats.writes.load(Ordering::Relaxed);
        if writes_after != writes_before {
            let _ = std::fs::remove_file(&res.manifest_path);
            info!(pod = %pod_log, writes_before, writes_after, "bake rejected: guest not frozen");
            return Err(Status::failed_precondition(
                "writes during bake: guest not frozen",
            ));
        }
        if req.upload {
            self.push_chain(&res.layer_id.hex(), None)
                .await
                .map_err(|e| Status::internal(format!("bake sealed but push failed: {e}")))?;
            self.refresh_pins_now().await;
        }
        info!(pod = %pod_log, layer = %res.layer_id, upload = req.upload, "bake");
        Ok(Response::new(pb::Layer {
            layer_id: res.layer_id.hex(),
            entries: res.manifest.entries.len() as u64,
            chunks_new: res.stats.chunks_new,
            chunks_inherited: res.stats.chunks_inherited,
            chunks_zeroed: res.stats.chunks_zeroed,
            chunks_pool_hit: res.stats.chunks_pool_hit,
            bytes_new: res.stats.bytes_new,
            seal_seconds: res.stats.seal_seconds,
            logical_bytes: res.manifest.data_bytes(),
        }))
    }

    async fn seal_file(
        &self,
        request: Request<pb::SealFileRequest>,
    ) -> Result<Response<pb::Layer>, Status> {
        let req = request.into_inner();
        if req.path.is_empty() {
            return Err(Status::invalid_argument("path required"));
        }
        if req.upload && self.s3.is_none() {
            return Err(Status::failed_precondition("S3 not configured"));
        }
        let chunk_size = if req.chunk_size == 0 {
            disk_store::chunk::DEFAULT_MEM_CHUNK_SIZE
        } else {
            req.chunk_size
        };
        let this = self.clone();
        let path = req.path.clone();
        let res = tokio::task::spawn_blocking(move || {
            disk_store::seal::seal_file(
                std::path::Path::new(&path),
                &this.cache,
                &this.state_dir,
                chunk_size,
                None,
            )
            .map_err(|e| e.to_string())
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        if req.upload {
            let layer = res.layer_id.hex();
            // A call that goes before the layer is up takes its manifest
            // with it (a seal of the file makes it again), so that the
            // layer does not stay behind unuploaded, its chunks kept from
            // every Evict.
            let mut unuploaded = UnuploadedLayer {
                manifest: res.manifest_path.clone(),
                uploaded: self.state_dir.join("uploaded").join(&layer),
                armed: true,
            };
            self.expire_journals_now().await;
            // Flat layer — no ancestors.
            let journal = self.memory_journal_path(&layer);
            self.push_layer(&layer, Some(Journal::Memory(&journal)))
                .await
                .map_err(|e| {
                    Status::internal(format!("seal_file uploaded but push failed: {e}"))
                })?;
            unuploaded.armed = false;
        }
        info!(path = %req.path, layer = %res.layer_id, upload = req.upload, "seal_file");
        Ok(Response::new(pb::Layer {
            layer_id: res.layer_id.hex(),
            entries: res.manifest.entries.len() as u64,
            chunks_new: res.stats.chunks_new,
            chunks_inherited: res.stats.chunks_inherited,
            chunks_zeroed: res.stats.chunks_zeroed,
            chunks_pool_hit: res.stats.chunks_pool_hit,
            bytes_new: res.stats.bytes_new,
            seal_seconds: res.stats.seal_seconds,
            logical_bytes: res.manifest.data_bytes(),
        }))
    }

    async fn materialize(
        &self,
        request: Request<pb::MaterializeRequest>,
    ) -> Result<Response<pb::Empty>, Status> {
        let req = request.into_inner();
        let this = self.clone();
        // A restore on a node that has never seen this pod knows the
        // layer ids and nothing else — not even the manifests. Pull those
        // first (with their parents), or load_chain below fails with a
        // bare ENOENT on a layers/<id>.manifest path (the restoring
        // node starts cold by definition).
        if this.s3.is_some() {
            for id in &req.chain {
                if this.layer_path(id).is_file() {
                    continue;
                }
                this.ensure_manifests(id)
                    .await
                    .map_err(|e| internal(format!("manifest {id}: {e}")))?;
            }
        }
        // Hydrate next (concurrent GET): memory-restore and cross-node
        // materialize operate on chains whose chunks are S3-only.
        {
            let chain = this.load_chain(&req.chain).map_err(internal)?;
            if let Some(s3) = &this.s3 {
                let refs: Vec<&disk_store::Manifest> = chain.iter().collect();
                disk_store::s3::hydrate_chain(&refs, &this.cache, s3, this.s3_concurrency)
                    .await
                    .map_err(|e| internal(format!("hydrate: {e}")))?;
            }
        }
        tokio::task::spawn_blocking(move || {
            let chain = this.load_chain(&req.chain).map_err(|e| e.to_string())?;
            let refs: Vec<&disk_store::Manifest> = chain.iter().collect();
            disk_store::materialize::materialize(
                &refs,
                &this.cache,
                std::path::Path::new(&req.out_path),
            )
            .map_err(|e| e.to_string())
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        Ok(Response::new(pb::Empty {}))
    }

    async fn push(&self, r: Request<pb::LayerRef>) -> Result<Response<pb::Empty>, Status> {
        let req = r.into_inner();
        if req.layer_id.is_empty() {
            return Err(Status::invalid_argument("layer_id required"));
        }
        if self.s3.is_none() {
            return Err(Status::failed_precondition("S3 not configured"));
        }
        self.expire_journals_now().await;
        self.push_layer(&req.layer_id, None)
            .await
            .map_err(Status::internal)?;
        self.refresh_pins_now().await;
        Ok(Response::new(pb::Empty {}))
    }

    async fn pull(&self, r: Request<pb::LayerRef>) -> Result<Response<pb::Empty>, Status> {
        let req = r.into_inner();
        if req.layer_id.is_empty() {
            return Err(Status::invalid_argument("layer_id required"));
        }
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        let head_id = req.layer_id.clone();
        self.ensure_manifests(&req.layer_id)
            .await
            .map_err(Status::internal)?;
        if req.warm {
            set_sched_idle();
            // fetch every missing data chunk of the whole chain (ids
            // come out head-first; load_chain wants L0 first)
            let mut ids = Vec::new();
            let mut cur = Some(req.layer_id.clone());
            while let Some(id) = cur {
                let m = self.load_layer(&id).map_err(internal)?;
                cur = m.parent.map(|p| p.hex());
                ids.push(id);
            }
            ids.reverse();
            let chain = self.load_chain(&ids).map_err(internal)?;
            let refs: Vec<&disk_store::Manifest> = chain.iter().collect();
            let fetched =
                disk_store::s3::hydrate_chain(&refs, &self.cache, &s3, self.s3_concurrency)
                    .await
                    .map_err(|e| internal(format!("warm pull: {e}")))?;
            info!(layer = %head_id, bytes = fetched, "pull --warm");
        }
        info!(layer = %head_id, warm = req.warm, "pull");
        Ok(Response::new(pb::Empty {}))
    }

    async fn evict(
        &self,
        request: Request<pb::EvictRequest>,
    ) -> Result<Response<pb::EvictStats>, Status> {
        let req = request.into_inner();
        let target = match pb::evict_request::Target::try_from(req.target) {
            Ok(pb::evict_request::Target::All) => disk_store::cache::EvictTarget::All,
            _ => disk_store::cache::EvictTarget::Budget,
        };
        let keep = if req.force {
            std::collections::HashSet::new()
        } else {
            self.tenure_protected()
        };
        let st = self.cache.evict_skip(target, &keep).map_err(internal)?;
        Ok(Response::new(pb::EvictStats {
            removed_entries: st.removed_entries,
            freed_bytes: st.freed_bytes,
        }))
    }

    async fn stats(
        &self,
        request: Request<pb::StatsRequest>,
    ) -> Result<Response<pb::StatsResponse>, Status> {
        let req = request.into_inner();
        let mut out = pb::StatsResponse {
            devices: Vec::new(),
            cache_bytes: 0,
            cache_entries: 0,
            s3_puts: 0,
            s3_put_bytes: 0,
            s3_gets: 0,
            s3_get_bytes: 0,
            memories: Vec::new(),
            object_store: self.s3.is_some(),
            object_store_id: self
                .s3
                .as_ref()
                .map(|s3| s3.id().to_string())
                .unwrap_or_default(),
            version: env!("CARGO_PKG_VERSION").to_string(),
        };
        if let Ok(st) = self.cache.stats() {
            out.cache_bytes = st.bytes;
            out.cache_entries = st.entries;
        }
        if let Some(s3) = &self.s3 {
            out.s3_puts = s3.puts.load(Ordering::Relaxed);
            out.s3_put_bytes = s3.bytes_up.load(Ordering::Relaxed);
            out.s3_gets = s3.gets.load(Ordering::Relaxed);
            out.s3_get_bytes = s3.bytes_down.load(Ordering::Relaxed);
        }
        let devs = self.devices.lock().map_err(|_| internal("poisoned"))?;
        for (id, d) in devs.iter() {
            if !req.pod_id.is_empty() && req.pod_id != *id {
                continue;
            }
            let s = &d.shared.stats;
            let ld = |a: &std::sync::atomic::AtomicU64| a.load(Ordering::Relaxed);
            out.devices.push(pb::DeviceStats {
                pod_id: id.clone(),
                dev_path: d.bdev.display().to_string(),
                reads: ld(&s.reads),
                writes: ld(&s.writes),
                flushes: ld(&s.flushes),
                discards: ld(&s.discards),
                read_bytes: ld(&s.read_bytes),
                write_bytes: ld(&s.write_bytes),
                cache_hits: ld(&s.cache_hits),
                cache_misses: ld(&s.cache_misses),
                s3_fetches: ld(&s.s3_fetches),
                s3_fetch_bytes: ld(&s.s3_fetch_bytes),
                errors: ld(&s.errors),
                histograms: vec![
                    pb::Histogram {
                        name: "read".into(),
                        buckets: s.read_hist.snapshot(),
                        p50_us: s.read_hist.percentile(0.5),
                        p99_us: s.read_hist.percentile(0.99),
                    },
                    pb::Histogram {
                        name: "flush".into(),
                        buckets: s.flush_hist.snapshot(),
                        p50_us: s.flush_hist.percentile(0.5),
                        p99_us: s.flush_hist.percentile(0.99),
                    },
                ],
            });
        }
        drop(devs);
        let mems = self.memories.lock().map_err(|_| internal("poisoned"))?;
        for (id, m) in mems.iter() {
            if !req.pod_id.is_empty() && req.pod_id != *id {
                continue;
            }
            let s = &m.stats;
            let ld = |a: &std::sync::atomic::AtomicU64| a.load(Ordering::Relaxed);
            out.memories.push(pb::MemoryStats {
                pod_id: id.clone(),
                attached: s.attached.load(Ordering::Relaxed),
                faults: ld(&s.faults),
                chunks_served: ld(&s.chunks_served),
                chunks_zeroed: ld(&s.chunks_zeroed),
                bytes_served: ld(&s.bytes_served),
                cache_hits: ld(&s.cache_hits),
                cache_misses: ld(&s.cache_misses),
                s3_fetches: ld(&s.s3_fetches),
                s3_fetch_bytes: ld(&s.s3_fetch_bytes),
                removes: ld(&s.removes),
                pages_reclaimed: ld(&s.pages_reclaimed),
                errors: ld(&s.errors),
                chunks_total: ld(&s.chunks_total),
            });
        }
        Ok(Response::new(out))
    }

    async fn fsck(
        &self,
        request: Request<pb::FsckRequest>,
    ) -> Result<Response<pb::FsckReport>, Status> {
        let check_s3 = request.into_inner().check_s3;
        let this = self.clone();

        // --check-s3: rebuild the upload markers from S3 truth. Probe
        // every chunk referenced by any local manifest at bounded
        // concurrency; afterwards a marker exists ⟺ the object exists.
        let mut remote: std::collections::HashSet<disk_store::chunk::Hash> =
            std::collections::HashSet::new();
        if check_s3 {
            let s3 = this
                .s3
                .clone()
                .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
            let layers = load_local_layers(&this.state_dir).map_err(internal)?;
            let mut probes: Vec<disk_store::chunk::Hash> = Vec::new();
            for (_, m) in &layers {
                for e in &m.entries {
                    if e.kind == disk_store::manifest::EntryKind::Data {
                        probes.push(disk_store::chunk::Hash::from(e.hash));
                    }
                }
            }
            let probes: Vec<disk_store::chunk::Hash> = probes
                .into_iter()
                .collect::<std::collections::HashSet<_>>()
                .into_iter()
                .collect();
            let found: Vec<Option<disk_store::chunk::Hash>> =
                futures::stream::iter(probes.iter().copied())
                    .map(|h| {
                        let s3 = s3.clone();
                        async move { s3.has_chunk(&h).await.then_some(h) }
                    })
                    .buffer_unordered(this.s3_concurrency.max(1))
                    .collect()
                    .await;
            for h in found.into_iter().flatten() {
                remote.insert(h);
            }
            for h in &probes {
                if remote.contains(h) {
                    this.mark_chunk_uploaded(h);
                } else {
                    this.unmark_chunk(h);
                }
            }
        }

        let report = tokio::task::spawn_blocking(move || -> Result<_, String> {
            let layers = load_local_layers(&this.state_dir)?;
            let s3_has = |h: &disk_store::chunk::Hash| remote.contains(h);
            disk_store::gc::fsck(
                &this.state_dir,
                &layers,
                if check_s3 { Some(&s3_has) } else { None },
            )
            .map_err(|e| e.to_string())
        })
        .await
        .map_err(internal)?
        .map_err(internal)?;
        Ok(Response::new(pb::FsckReport {
            complete_layers: report.complete_layers,
            dangling_layers: report
                .dangling_layers
                .iter()
                .map(|d| d.layer_id.clone())
                .collect(),
            garbage_chunks: report.garbage_chunks,
            garbage_bytes: report.garbage_bytes,
        }))
    }

    // Object-store GC (ADR 0011). The caller owns the refcounts, so
    // the live set arrives as an argument; diskd owns the bucket, so
    // the walking and the deleting happen here. Three passes, each over
    // one prefix, each only ever deleting objects that are unreachable
    // AND past the grace — a sweep that stops halfway (budget, error)
    // leaves the store consistent.
    async fn sweep(
        &self,
        request: Request<pb::SweepRequest>,
    ) -> Result<Response<pb::SweepReport>, Status> {
        let req = request.into_inner();
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        let grace = match (req.no_grace, req.grace_seconds) {
            (true, _) => 0, // labs only; see SweepRequest.no_grace
            (false, 0) => DEFAULT_SWEEP_GRACE_SECONDS,
            (false, n) => n as i64,
        };
        let budget = std::time::Duration::from_secs(if req.budget_seconds == 0 {
            DEFAULT_SWEEP_BUDGET_SECONDS
        } else {
            req.budget_seconds
        });
        let started = std::time::Instant::now();
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs() as i64)
            .unwrap_or_default();

        let mut rep = pb::SweepReport::default();
        let delete_all = |keys: Vec<String>| {
            let s3 = s3.clone();
            let conc = self.s3_concurrency.max(1);
            async move {
                if req.dry_run {
                    return Ok::<(), String>(());
                }
                let errs: Vec<String> = futures::stream::iter(keys)
                    .map(|k| {
                        let s3 = s3.clone();
                        async move {
                            s3.delete_object_key(&k)
                                .await
                                .err()
                                .map(|e| format!("{k}: {e}"))
                        }
                    })
                    .buffer_unordered(conc)
                    .collect::<Vec<_>>()
                    .await
                    .into_iter()
                    .flatten()
                    .collect();
                if let Some(first) = errs.first() {
                    return Err(format!("{} delete(s) failed, first: {first}", errs.len()));
                }
                Ok(())
            }
        };

        // --- pass 1: snapshots/ ------------------------------------
        let snaps = s3.list("snapshots/").await.map_err(internal)?;
        rep.snapshot_objects_seen = snaps.len() as u64;
        let mut live_heads: std::collections::HashSet<String> =
            req.live_heads.iter().cloned().collect();
        // A live head keeps the heads of its pod's volumes (ADR 0016).
        for key in &req.live_heads {
            let buf = match s3.find_object(key).await {
                Ok(Some(buf)) => buf,
                // A head that is gone names nothing; pass 1 sees it.
                Ok(None) => continue,
                Err(e) => return Err(internal(format!("sweep: live head {key}: {e}"))),
            };
            let head = disk_store::head::PodHead::decode(&buf)
                .map_err(|e| internal(format!("sweep: live head {key}: {e}")))?;
            live_heads.extend(head.volumes);
        }
        let plan = disk_store::sweep::plan_snapshots(&snaps, &live_heads, now, grace);
        rep.snapshot_objects_deleted = plan.dead_snapshot_keys.len() as u64;
        rep.bytes_deleted += plan.dead_bytes;
        rep.deleted_keys
            .extend(plan.dead_snapshot_keys.iter().cloned());
        delete_all(plan.dead_snapshot_keys.clone())
            .await
            .map_err(internal)?;

        // Every surviving head keeps its disk chain and its memory
        // layer alive; walk the manifests' parent pointers too, because
        // a chain entry's parent is reachable data even when the head
        // does not spell it out.
        let mut live_layers: std::collections::HashSet<String> =
            req.live_layers.iter().cloned().collect();
        for key in &plan.live_head_keys {
            let buf = match s3.get_object(key).await {
                Ok(b) => b,
                Err(e) => {
                    // A head we cannot read is a head we must not
                    // garbage-collect around: abort rather than delete
                    // layers it might reference.
                    return Err(internal(format!("sweep: live head {key}: {e}")));
                }
            };
            let head = disk_store::head::PodHead::decode(&buf)
                .map_err(|e| internal(format!("sweep: live head {key}: {e}")))?;
            live_layers.extend(head.chain.iter().cloned());
            if let Some(m) = &head.memory {
                live_layers.insert(m.layer.clone());
            }
        }
        for id in self.expand_parents(&live_layers, &s3).await {
            live_layers.insert(id);
        }
        if started.elapsed() >= budget {
            rep.budget_exhausted = true;
            return Ok(Response::new(rep));
        }

        // --- pass 2: layers/ ---------------------------------------
        let layer_objs = s3.list("layers/").await.map_err(internal)?;
        rep.layers_seen = layer_objs.len() as u64;
        let (dead_layers, bytes) =
            disk_store::sweep::plan_layers(&layer_objs, &live_layers, now, grace);
        rep.layers_deleted = dead_layers.len() as u64;
        rep.bytes_deleted += bytes;
        rep.deleted_keys.extend(dead_layers.iter().cloned());
        delete_all(dead_layers.clone()).await.map_err(internal)?;
        if !req.dry_run && !dead_layers.is_empty() {
            // A layer's marker is a claim about S3 too: a row that still
            // names a deleted layer gets its chunks pinned.
            for id in dead_layers
                .iter()
                .filter_map(|k| disk_store::sweep::layer_id_of_key(k))
            {
                let _ = std::fs::remove_file(self.state_dir.join("uploaded").join(id));
            }
            self.refresh_pins_now().await;
        }
        if started.elapsed() >= budget {
            rep.budget_exhausted = true;
            return Ok(Response::new(rep));
        }

        // --- pass 3: chunks/ ---------------------------------------
        // Referenced = the entry tables of every manifest that survived
        // pass 2 (not just the live set: a manifest we chose to keep
        // because it is inside the grace owns its chunks too).
        let dead_layer_ids: std::collections::HashSet<&str> = dead_layers
            .iter()
            .filter_map(|k| disk_store::sweep::layer_id_of_key(k))
            .collect();
        let surviving: Vec<String> = layer_objs
            .iter()
            .filter_map(|o| disk_store::sweep::layer_id_of_key(&o.key))
            .filter(|id| !dead_layer_ids.contains(id))
            .map(|id| id.to_string())
            .collect();
        let mut referenced: std::collections::HashSet<disk_store::chunk::Hash> =
            std::collections::HashSet::new();
        for id in &surviving {
            let m = self.manifest_for_sweep(id, &s3).await.map_err(internal)?;
            for e in &m.entries {
                if e.kind == disk_store::manifest::EntryKind::Data {
                    referenced.insert(disk_store::chunk::Hash::from(e.hash));
                }
            }
        }
        let chunk_objs = s3.list("chunks/").await.map_err(internal)?;
        rep.chunks_seen = chunk_objs.len() as u64;
        let (dead_chunks, bytes) =
            disk_store::sweep::plan_chunks(&chunk_objs, &referenced, now, grace);
        rep.chunks_deleted = dead_chunks.len() as u64;
        rep.bytes_deleted += bytes;
        rep.deleted_keys.extend(dead_chunks.iter().cloned());
        delete_all(dead_chunks.clone()).await.map_err(internal)?;
        if !req.dry_run {
            // The upload markers are a claim about S3; a chunk we just
            // removed from S3 must lose its marker or the next Push
            // would skip a PUT the bucket needs (the stale-marker trap).
            for k in &dead_chunks {
                if let Some(hex) = disk_store::sweep::chunk_hex_of_key(k)
                    && let Ok(h) = disk_store::chunk::Hash::from_hex(hex)
                {
                    self.unmark_chunk(&h);
                }
            }
        }
        info!(
            snapshots = rep.snapshot_objects_deleted,
            layers = rep.layers_deleted,
            chunks = rep.chunks_deleted,
            bytes = rep.bytes_deleted,
            dry_run = req.dry_run,
            seconds = started.elapsed().as_secs_f64(),
            "sweep"
        );
        Ok(Response::new(rep))
    }

    // Generic object gateway (the caller writes and reads vmstate and
    // head.json through here: all S3 traffic lives in diskd).
    /// Bind a Firecracker UFFD backend socket for this pod's guest
    /// RAM. Returns once the socket exists: the caller's
    /// `PUT /snapshot/load` is what makes Firecracker connect to it, and
    /// the VM is running long before the first chunk is on the wire.
    ///
    /// The memory layer is flat (`SealFile`: no parents), so "resolve the
    /// chain" is one manifest — but it is fetched, cached and verified by
    /// exactly the code the ublk device reads through.
    async fn open_memory(
        &self,
        request: Request<pb::OpenMemoryRequest>,
    ) -> Result<Response<pb::Memory>, Status> {
        let req = request.into_inner();
        if req.pod_id.is_empty() || req.layer.is_empty() || req.socket_path.is_empty() {
            return Err(Status::invalid_argument(
                "pod_id, layer and socket_path are required",
            ));
        }
        if self.s3.is_none() {
            return Err(Status::failed_precondition("S3 not configured"));
        }
        // Cold node by definition: pull the manifest before anything
        // else can ask it a question (same walk as Materialize's).
        if !self.layer_path(&req.layer).is_file() {
            self.ensure_manifests(&req.layer)
                .await
                .map_err(|e| internal(format!("memory manifest {}: {e}", req.layer)))?;
        }
        let manifest = self.load_layer(&req.layer).map_err(internal)?;
        let logical_size = manifest.logical_size;
        let chunk_size = manifest.chunk_size;
        let stats = Arc::new(MemStats::default());
        let started = uffd::serve(MemoryCtx {
            pod_id: req.pod_id.clone(),
            socket: PathBuf::from(&req.socket_path),
            manifest,
            cache: self.cache.clone(),
            readahead: self.mem_readahead,
            fetch_concurrency: self.s3_concurrency,
            stats: stats.clone(),
        })
        .map_err(internal)?;
        let chunks_total = stats.chunks_total.load(Ordering::Relaxed);
        let previous = {
            let mut t = self.memories.lock().map_err(|_| internal("poisoned"))?;
            t.insert(req.pod_id.clone(), started)
        };
        if let Some(old) = previous {
            tokio::task::spawn_blocking(move || uffd::stop(old))
                .await
                .map_err(internal)?;
        }
        info!(pod = %req.pod_id, layer = %req.layer, socket = %req.socket_path,
              chunks = chunks_total, "open_memory: uffd backend armed");
        Ok(Response::new(pb::Memory {
            pod_id: req.pod_id,
            socket_path: req.socket_path,
            logical_size,
            chunk_size,
            chunks_total,
        }))
    }

    /// Idempotent: the handler also lets go by itself when Firecracker
    /// closes the backend socket, so a pause/destroy that races the VMM's
    /// exit finds nothing here and that is not an error.
    async fn close_memory(
        &self,
        request: Request<pb::CloseMemoryRequest>,
    ) -> Result<Response<pb::Empty>, Status> {
        let req = request.into_inner();
        let started = {
            let mut t = self.memories.lock().map_err(|_| internal("poisoned"))?;
            t.remove(&req.pod_id)
        };
        if let Some(m) = started {
            tokio::task::spawn_blocking(move || uffd::stop(m))
                .await
                .map_err(internal)?;
            info!(pod = %req.pod_id, "close_memory: uffd backend detached");
        }
        Ok(Response::new(pb::Empty {}))
    }

    async fn put_object(
        &self,
        request: Request<pb::PutObjectRequest>,
    ) -> Result<Response<pb::Empty>, Status> {
        let req = request.into_inner();
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        let data = std::fs::read(&req.path)
            .map_err(|e| Status::invalid_argument(format!("read {}: {e}", req.path)))?;
        s3.put_object(&req.key, data).await.map_err(internal)?;
        Ok(Response::new(pb::Empty {}))
    }

    async fn get_object(
        &self,
        request: Request<pb::GetObjectRequest>,
    ) -> Result<Response<pb::Empty>, Status> {
        let req = request.into_inner();
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        let data = s3.get_object(&req.key).await.map_err(internal)?;
        let tmp = format!("{}.tmp-{}", req.path, std::process::id());
        std::fs::write(&tmp, &data)
            .and_then(|_| std::fs::rename(&tmp, &req.path))
            .map_err(|e| Status::internal(format!("write {}: {e}", req.path)))?;
        Ok(Response::new(pb::Empty {}))
    }

    // The head is a checkpoint's last object (ADR 0011's commit order):
    // it names the disk's chain as this node's row has it after a seal,
    // and the memory the caller sealed and put beside it. Every layer it
    // names must be in the bucket before it is, so that a head in the
    // bucket is always whole.
    async fn put_head(
        &self,
        request: Request<pb::PutHeadRequest>,
    ) -> Result<Response<pb::Head>, Status> {
        let req = request.into_inner();
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        if req.pod_id.is_empty() || req.epoch == 0 {
            return Err(Status::invalid_argument("pod_id and an epoch are required"));
        }
        let locked = self.lock_disk(&req.pod_id).await?;
        if self
            .devices
            .lock()
            .map_err(|_| internal("poisoned"))?
            .contains_key(&req.pod_id)
        {
            return Err(Status::failed_precondition(
                "device still live; stop and seal it first",
            ));
        }
        let row = State::load(&self.state_dir)
            .map_err(internal)?
            .devices
            .get(&req.pod_id)
            .cloned()
            .ok_or_else(|| Status::not_found(format!("no disk of pod {}", req.pod_id)))?;
        if self.top_path(&req.pod_id).is_file() {
            return Err(Status::failed_precondition(
                "the disk has writes that are not sealed; seal it first",
            ));
        }
        let mut head = disk_store::head::PodHead::new(
            &req.pod_id,
            row.logical_size,
            row.chunk_size,
            row.chain.clone(),
        );
        head.shared_depth = Some(row.shared_depth.clamp(1, row.chain.len().max(1)));
        head.epoch = req.epoch;
        head.memory = req.memory.map(|m| disk_store::head::MemoryRef {
            layer: m.layer,
            vmstate_key: m.vmstate_key,
            epoch: req.epoch,
            fc_version: m.firecracker_version,
            cpu_model: m.cpu_model,
            kernel_release: m.kernel_release,
        });
        head.volumes = req.volume_heads;
        head.validate()
            .map_err(|e| Status::invalid_argument(e.to_string()))?;
        let own = format!("snapshots/{}/", req.pod_id);
        if let Some(key) = head.volumes.iter().find(|key| key.starts_with(&own)) {
            return Err(Status::invalid_argument(format!(
                "{key} is a head of pod {}'s own disk, not of a volume",
                req.pod_id
            )));
        }

        let mut layers = head.chain.clone();
        layers.extend(head.memory.as_ref().map(|m| m.layer.clone()));
        for id in &layers {
            let lid = disk_store::manifest::LayerId::from_hex(id)
                .map_err(|e| Status::invalid_argument(format!("layer {id}: {e}")))?;
            if !s3.has_manifest(&lid).await {
                return Err(Status::failed_precondition(format!(
                    "layer {id} is not in the object store; push it first"
                )));
            }
        }
        if let Some(m) = &head.memory
            && !s3.has_object(&m.vmstate_key).await
        {
            return Err(Status::failed_precondition(format!(
                "{} is not in the object store",
                m.vmstate_key
            )));
        }
        for key in &head.volumes {
            if !s3.has_object(key).await {
                return Err(Status::failed_precondition(format!(
                    "the volume's head {key} is not in the object store; put it first"
                )));
            }
        }
        let key = disk_store::head::head_key(&req.pod_id, req.epoch);
        if s3.has_object(&key).await {
            return Err(Status::already_exists(format!(
                "{key} exists: a head is never overwritten"
            )));
        }
        head.stamp();
        let buf = head.encode().map_err(internal)?;
        // The head, and then the journals and pins of what it names, in a
        // task of their own with the disk's lock: a caller that goes away
        // does not part them.
        let this = self.clone();
        let pod = req.pod_id.clone();
        let memory = head.memory.as_ref().map(|m| m.layer.clone());
        let put = tokio::spawn({
            let key = key.clone();
            async move {
                s3.put_object(&key, buf).await.map_err(internal)?;
                // The head is stored, and the call succeeds: a journal
                // left behind costs pins and uploads only.
                let cleaned = tokio::task::spawn_blocking(move || {
                    let _locked = locked;
                    let mut journals = vec![this.unheaded_path(&pod)];
                    journals.extend(memory.map(|layer| this.memory_journal_path(&layer)));
                    for journal in &journals {
                        if let Err(e) = this.remove_journal(journal) {
                            warn!(pod = %pod, error = %e, "put_head: journal left");
                        }
                    }
                    this.refresh_pins();
                })
                .await;
                if let Err(e) = cleaned {
                    warn!(error = %e, "put_head: journals and pins left");
                }
                Ok::<(), Status>(())
            }
        });
        put.await.map_err(internal)??;
        info!(
            pod = %req.pod_id,
            key = %key,
            memory = head.memory.is_some(),
            volumes = head.volumes.len(),
            "put_head"
        );
        Ok(Response::new(head_message(key, head)))
    }

    async fn get_head(
        &self,
        request: Request<pb::GetHeadRequest>,
    ) -> Result<Response<pb::Head>, Status> {
        let key = request.into_inner().key;
        let s3 = self
            .s3
            .clone()
            .ok_or_else(|| Status::failed_precondition("S3 not configured"))?;
        let buf = s3
            .find_object(&key)
            .await
            .map_err(|e| internal(format!("head {key}: {e}")))?
            .ok_or_else(|| Status::not_found(format!("no head {key}")))?;
        let head = disk_store::head::PodHead::decode(&buf)
            .map_err(|e| Status::invalid_argument(format!("head {key}: {e}")))?;
        Ok(Response::new(head_message(key, head)))
    }
}

/// A head as the API shows it.
fn head_message(key: String, head: disk_store::head::PodHead) -> pb::Head {
    pb::Head {
        key,
        epoch: head
            .memory
            .as_ref()
            .map_or(head.epoch, |m| head.epoch.max(m.epoch)),
        pod_id: head.pod_id,
        chain: head.chain,
        logical_size: head.logical_size,
        memory: head.memory.map(|m| pb::MemoryRef {
            layer: m.layer,
            vmstate_key: m.vmstate_key,
            firecracker_version: m.fc_version,
            cpu_model: m.cpu_model,
            kernel_release: m.kernel_release,
        }),
        volume_heads: head.volumes,
    }
}

fn load_local_layers(
    state_dir: &std::path::Path,
) -> Result<Vec<(String, disk_store::Manifest)>, String> {
    let dir = state_dir.join("layers");
    let mut layers = Vec::new();
    if dir.is_dir() {
        for e in std::fs::read_dir(&dir).map_err(|e| e.to_string())? {
            let e = e.map_err(|e| e.to_string())?;
            let name = e.file_name().to_string_lossy().to_string();
            if !name.ends_with(".manifest") {
                continue;
            }
            let id = name.trim_end_matches(".manifest").to_string();
            let m =
                disk_store::Manifest::decode(&std::fs::read(e.path()).map_err(|x| x.to_string())?)
                    .map_err(|x| x.to_string())?;
            layers.push((id, m));
        }
    }
    Ok(layers)
}

/// Removes the local manifest of a layer whose SealFile ended before the
/// layer was uploaded, unless it is uploaded after all.
struct UnuploadedLayer {
    manifest: PathBuf,
    uploaded: PathBuf,
    armed: bool,
}

impl Drop for UnuploadedLayer {
    fn drop(&mut self) {
        if self.armed && !self.uploaded.is_file() {
            let _ = std::fs::remove_file(&self.manifest);
        }
    }
}

/// Sets its flag when it is dropped: with the future of a call, when the
/// call ends or its caller goes away.
struct SetOnDrop(Arc<AtomicBool>);

impl Drop for SetOnDrop {
    fn drop(&mut self) {
        self.0.store(true, Ordering::Relaxed);
    }
}

/// The row `spawn_dev` leaves for a device it started: a new row as it was
/// handed in, else the row as the state has it now (another call may have
/// changed it since it was read), with the device.
fn put_device_row(st: &mut State, pod_id: &str, row: DeviceRow, dev_id: i32, new_row: bool) {
    match st.devices.get_mut(pod_id) {
        Some(current) if !new_row => current.dev_id = Some(dev_id),
        _ => {
            st.devices.insert(
                pod_id.to_string(),
                DeviceRow {
                    dev_id: Some(dev_id),
                    ..row
                },
            );
        }
    }
}

fn unix_now() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or_default()
}

/// Writes a journal that starts at `since` and lists no chunk, in place of
/// what `path` held, through `tmp`.
fn write_journal(path: &Path, since: u64, tmp: &Path) -> std::io::Result<()> {
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir)?;
    }
    if let Some(dir) = tmp.parent() {
        std::fs::create_dir_all(dir)?;
    }
    std::fs::write(tmp, format!("{since}\n"))?;
    std::fs::File::open(tmp)?.sync_all()?;
    std::fs::rename(tmp, path)
}

/// Appends `lines` to the journal at `path`, durably, and returns whether
/// it did. A journal that is not there starts at `start`, or, without one,
/// gets nothing.
fn append_journal(
    path: &Path,
    lines: &str,
    start: Option<u64>,
    tmp: &Path,
) -> std::io::Result<bool> {
    if !path.is_file() {
        let Some(since) = start else {
            return Ok(false);
        };
        write_journal(path, since, tmp)?;
    }
    let mut f = std::fs::OpenOptions::new().append(true).open(path)?;
    f.write_all(lines.as_bytes())?;
    f.sync_data()?;
    Ok(true)
}

#[cfg(test)]
mod tests {
    use super::*;
    use disk_store::manifest::{Entry, Manifest};
    use std::os::unix::fs::FileExt;

    const CHUNK: u32 = 64 * 1024;
    const SIZE: u64 = 1 << 20;

    fn svc(dir: &Path) -> DiskSvc {
        let cache = disk_store::cache::Cache::open(dir, 64 << 20).unwrap();
        DiskSvc::new(dir.to_path_buf(), Arc::new(cache), None, 4, 0)
    }

    fn row(chain: &[&str]) -> DeviceRow {
        DeviceRow {
            dev_id: None,
            chain: chain.iter().map(|s| s.to_string()).collect(),
            logical_size: SIZE,
            chunk_size: CHUNK,
            has_top: true,
            shared_depth: 1,
        }
    }

    fn h(i: u8) -> Hash {
        disk_store::chunk::hash(&[i; 16])
    }

    /// A layer with data at chunk k for each hash, stored in `layers/`.
    fn layer(svc: &DiskSvc, parent: Option<&Manifest>, hashes: &[Hash]) -> Manifest {
        let mut m = Manifest::new(CHUNK, SIZE, parent.map(|p| p.layer_id()));
        for (k, h) in hashes.iter().enumerate() {
            m.entries.push(Entry::data(
                k as u64 * CHUNK as u64,
                *h.as_bytes(),
                CHUNK,
                100,
            ));
        }
        std::fs::create_dir_all(svc.state_dir.join("layers")).unwrap();
        std::fs::write(svc.layer_path(&m.layer_id().hex()), m.encode()).unwrap();
        m
    }

    fn uploaded(svc: &DiskSvc, m: &Manifest) {
        svc.mark_uploaded(&m.layer_id().hex()).unwrap();
    }

    /// A disk "p" over an L0 with data, and a top with writes.
    fn disk_with_writes(svc: &DiskSvc) -> String {
        let base = svc.state_dir.join("base.img");
        let f = std::fs::File::create(&base).unwrap();
        f.set_len(SIZE).unwrap();
        f.write_all_at(&[7u8; 4096], 0).unwrap();
        let l0 = disk_store::seal::seal_top(
            &base,
            &[],
            None,
            &svc.cache,
            &svc.state_dir,
            SIZE,
            CHUNK,
            None,
        )
        .unwrap()
        .layer_id
        .hex();
        let mut st = State::default();
        st.devices.insert("p".into(), row(&[&l0]));
        State::store(&svc.state_dir, &st).unwrap();
        std::fs::create_dir_all(svc.state_dir.join("live/p")).unwrap();
        let top = std::fs::File::create(svc.top_path("p")).unwrap();
        top.set_len(SIZE).unwrap();
        top.write_all_at(&[9u8; 8192], 3 * CHUNK as u64).unwrap();
        l0
    }

    /// The layer a seal of `disk_with_writes` makes.
    async fn layer_with_writes() -> String {
        let dir = tempfile::tempdir().unwrap();
        let svc = svc(dir.path());
        disk_with_writes(&svc);
        seal(&svc).await.unwrap().layer_id
    }

    async fn seal(svc: &DiskSvc) -> Result<pb::Layer, Status> {
        svc.seal(Request::new(pb::SealRequest {
            pod_id: "p".into(),
            upload: false,
        }))
        .await
        .map(Response::into_inner)
    }

    fn layer_files(svc: &DiskSvc) -> Vec<String> {
        let mut names: Vec<String> = std::fs::read_dir(svc.state_dir.join("layers"))
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        names.sort();
        names
    }

    /// Starts a local seal of disk "p" whose closure stops at `point` until
    /// the test sends on the returned channel; returns once it is there.
    async fn seal_held_at(
        svc: &mut DiskSvc,
        point: SealPoint,
    ) -> (
        tokio::task::JoinHandle<Result<pb::Layer, Status>>,
        std::sync::mpsc::Sender<()>,
    ) {
        let (entered_tx, entered_rx) = std::sync::mpsc::channel::<()>();
        let (release_tx, release_rx) = std::sync::mpsc::channel::<()>();
        let release_rx = Mutex::new(release_rx);
        svc.seal_hook = Some(Arc::new(move |at| {
            if at == point {
                let _ = entered_tx.send(());
                let _ = release_rx.lock().unwrap().recv();
            }
        }));
        let sealing = tokio::spawn({
            let svc = svc.clone();
            async move { seal(&svc).await }
        });
        tokio::task::spawn_blocking(move || {
            entered_rx
                .recv_timeout(Duration::from_secs(10))
                .expect("the seal's closure gets there")
        })
        .await
        .unwrap();
        (sealing, release_tx)
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_seal_whose_caller_went_leaves_the_disk_as_it_was() {
        let dir = tempfile::tempdir().unwrap();
        let mut svc = svc(dir.path());
        let l0 = disk_with_writes(&svc);
        let top = std::fs::read(svc.top_path("p")).unwrap();
        let layers = layer_files(&svc);
        let (sealing, release) = seal_held_at(&mut svc, SealPoint::Locked).await;

        // The caller goes away; the closure keeps the disk's lock until it
        // stops.
        sealing.abort();
        assert!(sealing.await.unwrap_err().is_cancelled());
        let busy = svc
            .lock_disk_within("p", Duration::from_millis(50))
            .await
            .err()
            .expect("the disk is locked");
        assert_eq!(busy.code(), tonic::Code::Unavailable);
        assert_eq!(busy.message(), "disk p is busy");
        let waiter = tokio::spawn({
            let svc = svc.clone();
            async move { svc.lock_disk("p").await }
        });
        tokio::time::sleep(Duration::from_millis(200)).await;
        assert!(
            !waiter.is_finished(),
            "a call of the disk waits for the seal"
        );

        release.send(()).unwrap();
        let locked = tokio::time::timeout(Duration::from_secs(10), waiter)
            .await
            .expect("the lock is free once the seal stopped")
            .unwrap()
            .unwrap();
        // What an OpenDisk finds then: the disk as it was.
        let st = State::load(&svc.state_dir).unwrap();
        let row = &st.devices["p"];
        assert_eq!(row.chain, [l0]);
        assert!(row.has_top);
        assert_eq!(std::fs::read(svc.top_path("p")).unwrap(), top);
        assert_eq!(layer_files(&svc), layers, "a layer was committed");
        assert!(svc.unheaded_path("p").is_file(), "as after a failed seal");
        // L0 is not uploaded: still pinned.
        let stats = svc.cache.stats().unwrap();
        assert_eq!(stats.pinned_entries, stats.entries);
        drop(locked);

        // The next seal seals, as the first would have.
        svc.seal_hook = None;
        let sealed = seal(&svc).await.unwrap();
        assert_eq!(sealed.layer_id, layer_with_writes().await);
        let st = State::load(&svc.state_dir).unwrap();
        assert_eq!(st.devices["p"].chain[1], sealed.layer_id);
        assert!(!svc.top_path("p").exists());
        assert!(svc.disk_locks.lock().unwrap().is_empty(), "unused locks go");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_seal_whose_caller_went_once_it_committed_ends() {
        let dir = tempfile::tempdir().unwrap();
        let mut svc = svc(dir.path());
        let l0 = disk_with_writes(&svc);
        let (sealing, release) = seal_held_at(&mut svc, SealPoint::Committed).await;
        sealing.abort();
        assert!(sealing.await.unwrap_err().is_cancelled());
        let waiter = tokio::spawn({
            let svc = svc.clone();
            async move { svc.lock_disk("p").await }
        });
        release.send(()).unwrap();
        let locked = tokio::time::timeout(Duration::from_secs(10), waiter)
            .await
            .expect("the lock is free once the seal ended")
            .unwrap()
            .unwrap();
        // What an OpenDisk finds then: the sealed layer, no top.
        let want = layer_with_writes().await;
        let st = State::load(&svc.state_dir).unwrap();
        let sealed = &st.devices["p"];
        assert_eq!(sealed.chain, [l0, want.clone()]);
        assert!(!sealed.has_top);
        assert!(!svc.top_path("p").exists());
        assert!(svc.layer_path(&want).is_file());
        assert!(svc.unheaded_path("p").is_file(), "no head names S' yet");
        let stats = svc.cache.stats().unwrap();
        assert_eq!(stats.pinned_entries, stats.entries, "S' is pinned");
        drop(locked);

        // A seal that comes later finds it sealed already.
        svc.seal_hook = None;
        assert_eq!(seal(&svc).await.unwrap().layer_id, want);
        assert!(svc.disk_locks.lock().unwrap().is_empty(), "unused locks go");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_new_layer_is_pinned() {
        let dir = tempfile::tempdir().unwrap();
        let svc = svc(dir.path());
        disk_with_writes(&svc);
        let layer = svc
            .seal(Request::new(pb::SealRequest {
                pod_id: "p".into(),
                upload: false,
            }))
            .await
            .unwrap()
            .into_inner();
        let st = svc.cache.stats().unwrap();
        // L0 is not uploaded either: pinned as well.
        assert_eq!(st.pinned_entries, st.entries);
        let s = svc.load_layer(&layer.layer_id).unwrap();
        let data = s
            .entries
            .iter()
            .filter(|e| e.kind == disk_store::manifest::EntryKind::Data)
            .count();
        assert!(data > 0);
        svc.cache
            .evict_skip(disk_store::cache::EvictTarget::All, &HashSet::new())
            .unwrap();
        for e in &s.entries {
            assert!(svc.cache.contains(&Hash::from(e.hash)), "S' survives Evict");
        }
    }

    #[test]
    fn update_state_keeps_every_writer() {
        let dir = tempfile::tempdir().unwrap();
        let svc = svc(dir.path());
        const WRITERS: usize = 8;
        const ROUNDS: u64 = 25;
        std::thread::scope(|s| {
            for w in 0..WRITERS {
                let svc = &svc;
                s.spawn(move || {
                    for _ in 0..ROUNDS {
                        svc.update_state(|st| {
                            st.devices
                                .entry(format!("d{w}"))
                                .or_insert_with(|| DeviceRow {
                                    logical_size: 0,
                                    ..row(&["l0"])
                                })
                                .logical_size += 1;
                            Ok(())
                        })
                        .unwrap();
                    }
                });
            }
        });
        let st = State::load(&svc.state_dir).unwrap();
        assert_eq!(st.devices.len(), WRITERS);
        for r in st.devices.values() {
            assert_eq!(r.logical_size, ROUNDS, "a change was lost");
        }
    }

    #[test]
    fn spawn_dev_keeps_a_chain_stored_meanwhile() {
        let mut st = State::default();
        // spawn_dev was handed [l0]; a seal stored [l0, s] since.
        st.devices.insert("p".into(), row(&["l0", "s"]));
        put_device_row(&mut st, "p", row(&["l0"]), 7, false);
        assert_eq!(st.devices["p"].chain, ["l0", "s"]);
        assert_eq!(st.devices["p"].dev_id, Some(7));
        // A new row (CreateDisk, OpenDisk from a head) is stored as handed.
        put_device_row(&mut st, "p", row(&["l1"]), 8, true);
        assert_eq!(st.devices["p"].chain, ["l1"]);
        assert_eq!(st.devices["p"].dev_id, Some(8));
        put_device_row(&mut st, "q", row(&["l0"]), 9, false);
        assert_eq!(st.devices["q"].dev_id, Some(9));
    }

    #[test]
    fn pinned_row_chunks_are_what_only_the_node_has() {
        let dir = tempfile::tempdir().unwrap();
        let svc = svc(dir.path());
        let l0 = layer(&svc, None, &[h(1), h(2)]);
        uploaded(&svc, &l0);
        let l0_local = layer(&svc, None, &[h(3)]);
        // x: unheaded, its own layer uploaded (an abandon after the push).
        let sx = layer(&svc, Some(&l0), &[h(10), h(11)]);
        uploaded(&svc, &sx);
        // y: headed and uploaded.
        let sy = layer(&svc, Some(&l0), &[h(20)]);
        uploaded(&svc, &sy);
        // z: headed, its layers not uploaded.
        let sz = layer(&svc, Some(&l0_local), &[h(30)]);
        let id = |m: &Manifest| m.layer_id().hex();
        let mut st = State::default();
        st.devices.insert("x".into(), row(&[&id(&l0), &id(&sx)]));
        st.devices.insert("y".into(), row(&[&id(&l0), &id(&sy)]));
        st.devices
            .insert("z".into(), row(&[&id(&l0_local), &id(&sz)]));
        // w: unheaded, but no layer of its own yet.
        st.devices.insert("w".into(), row(&[&id(&l0)]));
        State::store(&svc.state_dir, &st).unwrap();
        for disk in ["x", "w"] {
            write_journal(&svc.unheaded_path(disk), unix_now(), &svc.journal_tmp()).unwrap();
        }
        let pinned = svc.pinned_row_chunks().unwrap();
        let want: HashSet<Hash> = [h(3), h(10), h(11), h(30)].into_iter().collect();
        assert_eq!(pinned, want);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn unheaded_journals_expire_after_12_hours() {
        let dir = tempfile::tempdir().unwrap();
        let svc = svc(dir.path());
        let now = unix_now();
        let hours = |n: u64| now - n * 60 * 60;
        let mut st = State::default();
        st.devices.insert("old".into(), row(&["l0"]));
        st.devices.insert("young".into(), row(&["l0"]));
        State::store(&svc.state_dir, &st).unwrap();
        let old = svc.unheaded_path("old");
        let young = svc.unheaded_path("young");
        let gone = svc.unheaded_path("gone");
        let memory = svc.memory_journal_path("m");
        for (path, since) in [(&old, 13), (&young, 11), (&gone, 13), (&memory, 13)] {
            write_journal(path, hours(since), &svc.journal_tmp()).unwrap();
        }
        // What pushes listed and marked: chunks, and the layers of disks.
        let layer = |i: u8| h(i).to_hex().to_string();
        let layer_marked = |i: u8| svc.state_dir.join("uploaded").join(layer(i)).is_file();
        let disk = |path| Some(Journal::Disk(path));
        svc.mark_pushed(vec![h(1), h(2)], disk(&old)).await;
        svc.mark_layer_pushed(&layer(100), 0, disk(&old))
            .await
            .unwrap();
        svc.mark_pushed(vec![h(3), h(4)], disk(&young)).await;
        svc.mark_layer_pushed(&layer(101), 0, disk(&young))
            .await
            .unwrap();
        svc.mark_pushed(vec![h(5)], disk(&gone)).await;
        svc.mark_pushed(vec![h(6)], Some(Journal::Memory(&memory)))
            .await;
        for i in 1..=6 {
            assert!(svc.chunk_uploaded(&h(i)), "marker {i}");
        }
        assert!(layer_marked(100) && layer_marked(101));
        let young_before = std::fs::read_to_string(&young).unwrap();

        let expired = tokio::task::spawn_blocking({
            let svc = svc.clone();
            move || svc.expire_journals(now)
        })
        .await
        .unwrap();

        assert!(expired, "a layer lost its marker");
        for i in [1, 2, 5, 6] {
            assert!(!svc.chunk_uploaded(&h(i)), "marker {i} expired");
        }
        for i in [3, 4] {
            assert!(svc.chunk_uploaded(&h(i)), "marker {i} kept");
        }
        assert!(!layer_marked(100), "the old journal's layer is put again");
        assert!(layer_marked(101), "the young journal's layer is trusted");
        assert_eq!(std::fs::read_to_string(&old).unwrap(), format!("{now}\n"));
        assert_eq!(std::fs::read_to_string(&young).unwrap(), young_before);
        assert!(!gone.exists(), "the journal of a disk the node lost goes");
        assert!(!memory.exists(), "an expired memory journal goes");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn only_a_seal_starts_a_disks_journal() {
        let dir = tempfile::tempdir().unwrap();
        let svc = svc(dir.path());
        let layer = h(100).to_hex().to_string();
        let layer_marked = || svc.state_dir.join("uploaded").join(&layer).is_file();
        let disk = svc.unheaded_path("p");

        // Headed or removed meanwhile: a push that still runs marks
        // nothing, and leaves the disk headed.
        svc.mark_pushed(vec![h(1)], Some(Journal::Disk(&disk)))
            .await;
        svc.mark_layer_pushed(&layer, 0, Some(Journal::Disk(&disk)))
            .await
            .unwrap();
        assert!(!disk.exists());
        assert!(!svc.chunk_uploaded(&h(1)));
        assert!(!layer_marked());

        // Started by a seal: listed, then marked.
        tokio::task::spawn_blocking({
            let svc = svc.clone();
            move || svc.mark_unheaded("p")
        })
        .await
        .unwrap()
        .unwrap();
        let since = std::fs::read_to_string(&disk).unwrap();
        svc.mark_pushed(vec![h(1), h(2)], Some(Journal::Disk(&disk)))
            .await;
        svc.mark_layer_pushed(&layer, 0, Some(Journal::Disk(&disk)))
            .await
            .unwrap();
        assert_eq!(
            std::fs::read_to_string(&disk).unwrap(),
            format!("{since}{}\n{}\nL {layer}\n", h(1).to_hex(), h(2).to_hex())
        );
        assert!(svc.chunk_uploaded(&h(1)) && svc.chunk_uploaded(&h(2)));
        assert!(layer_marked());

        // A memory layer's first chunk starts its journal.
        let memory = svc.memory_journal_path("m");
        svc.mark_pushed(vec![h(3)], Some(Journal::Memory(&memory)))
            .await;
        let text = std::fs::read_to_string(&memory).unwrap();
        let lines: Vec<&str> = text.lines().collect();
        assert!(lines[0].parse::<u64>().is_ok(), "{text}");
        assert_eq!(lines[1..], [h(3).to_hex().to_string()]);
        assert!(svc.chunk_uploaded(&h(3)));
    }

    #[tokio::test]
    async fn unused_disk_locks_go() {
        let dir = tempfile::tempdir().unwrap();
        let svc = svc(dir.path());
        let locks = || svc.disk_locks.lock().unwrap().len();
        let waiter = || {
            let svc = svc.clone();
            tokio::spawn(async move { svc.lock_disk("p").await.map(|_| ()) })
        };

        // A call that gave up waiting.
        let held = svc.lock_disk("p").await.unwrap();
        let busy = svc.lock_disk_within("p", Duration::from_millis(20)).await;
        assert_eq!(busy.err().unwrap().code(), tonic::Code::Unavailable);
        drop(held);
        assert_eq!(locks(), 0, "after a call that gave up");

        // A call whose caller went away while it waited.
        let held = svc.lock_disk("p").await.unwrap();
        let w = waiter();
        tokio::time::sleep(Duration::from_millis(10)).await;
        w.abort();
        assert!(w.await.unwrap_err().is_cancelled());
        drop(held);
        assert_eq!(locks(), 0, "after a call that went away waiting");

        // One that went away once the lock was its, before it ran again.
        let held = svc.lock_disk("p").await.unwrap();
        let w = waiter();
        tokio::time::sleep(Duration::from_millis(10)).await;
        drop(held);
        w.abort();
        assert!(w.await.unwrap_err().is_cancelled());
        assert_eq!(locks(), 0, "after a call that went away with the lock");

        // One that waited and got it.
        let held = svc.lock_disk("p").await.unwrap();
        let w = waiter();
        tokio::time::sleep(Duration::from_millis(10)).await;
        drop(held);
        w.await.unwrap().unwrap();
        assert_eq!(locks(), 0, "after a call that waited");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn remove_device_keeps_the_mark_of_a_row_it_kept() {
        let dir = tempfile::tempdir().unwrap();
        let svc = svc(dir.path());
        let mut st = State::default();
        st.devices.insert("p".into(), row(&["l0"]));
        State::store(&svc.state_dir, &st).unwrap();
        let journal = svc.unheaded_path("p");
        write_journal(&journal, unix_now(), &svc.journal_tmp()).unwrap();
        let remove =
            || svc.remove_device(Request::new(pb::RemoveDeviceRequest { pod_id: "p".into() }));

        // diskd.state cannot be written.
        let tmp = svc.state_dir.join("diskd.state.tmp");
        std::fs::create_dir(&tmp).unwrap();
        assert!(remove().await.is_err());
        assert!(State::load(&svc.state_dir)
            .unwrap()
            .devices
            .contains_key("p"));
        assert!(journal.is_file(), "the row it kept lost its mark");

        std::fs::remove_dir(&tmp).unwrap();
        remove().await.unwrap();
        assert!(State::load(&svc.state_dir).unwrap().devices.is_empty());
        assert!(!journal.exists());
    }
}
