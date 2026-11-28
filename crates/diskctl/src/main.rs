// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! diskctl, the command-line client of diskd's API (ADR 0011). It is for
//! development, the lab scripts and tests: diskd's API is internal to the
//! node, and feosd will be its client. Output is JSON.

use clap::{Parser, Subcommand, ValueEnum};
use feos_proto::disk_service as pb;
use feos_proto::disk_service::disk_service_client::DiskServiceClient;
use hyper_util::rt::TokioIo;
use tonic::transport::{Endpoint, Uri};
use tower::service_fn;

#[derive(Parser, Debug)]
#[command(name = "diskctl", about = "Command-line client of diskd", version)]
struct Cli {
    /// diskd's control socket.
    #[arg(long, global = true, default_value = "/var/lib/feos/disk/diskd.sock")]
    socket: String,

    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Clone, Copy, Debug, ValueEnum)]
enum EvictTarget {
    /// Drop every reconstructible byte.
    All,
    /// Drop until inside the configured budget.
    Budget,
}

#[derive(Subcommand, Debug)]
enum Cmd {
    /// Raw image file → L0 (chunked, dedup'd, sealed as a base layer).
    ImportBase {
        path: String,
        /// Chunk size in bytes; omitted = the store default (64 KiB).
        chunk_size: Option<u32>,
    },
    /// Chain + a fresh top → /dev/ublkb<N> and a stable symlink.
    Create {
        pod_id: String,
        /// Layer ids, L0 first.
        #[arg(required = true)]
        chain: Vec<String>,
    },
    /// Drop a device row (stop it first).
    Delete { pod_id: String },
    /// Existing pod → device; with --head, restore a foreign node's
    /// pod from its S3 head.
    Open {
        pod_id: String,
        /// S3 key of a head.json (snapshots/<id>/<epoch>/head.json).
        #[arg(long)]
        head: Option<String>,
    },
    /// Stop a device, keep its top (short-idle pause).
    Stop { pod_id: String },
    /// Top → new layer, flatten-on-seal (ADR 0011).
    Seal {
        pod_id: String,
        /// Also push the sealed layer to S3.
        #[arg(long)]
        upload: bool,
    },
    /// Frozen top of a LIVE device → catalog layer (ADR 0011).
    Bake {
        pod_id: String,
        #[arg(long)]
        upload: bool,
    },
    /// Plain file (memory dump) → flat layer (ADR 0011).
    SealFile {
        path: String,
        #[arg(long)]
        upload: bool,
        /// 0 = the memory-layer default (256 KiB).
        #[arg(long, default_value_t = 0)]
        chunk_size: u32,
    },
    /// Chain → raw file (tests: byte equality; ops: image inspection).
    Materialize {
        out_path: String,
        /// Layer ids, L0 first.
        #[arg(required = true)]
        chain: Vec<String>,
    },
    /// Upload a layer (chunks before manifest).
    Push {
        layer_id: String,
        /// Accepted for symmetry with `pull`; the daemon's Push always
        /// uploads every chunk, so it changes nothing.
        #[arg(long)]
        warm: bool,
    },
    /// Download a layer's manifest, and with --warm its chunks too.
    Pull {
        layer_id: String,
        /// Also fetch every chunk now instead of lazily on read.
        #[arg(long)]
        warm: bool,
    },
    /// Drop reconstructible cache bytes.
    Evict {
        #[arg(value_enum, default_value_t = EvictTarget::All)]
        target: EvictTarget,
        /// Also evict what is kept otherwise: the chunks of the devices'
        /// chains, and of layers that have not been pushed — those exist
        /// nowhere else. Pinned chunks stay: those of a device's layer
        /// that has not been pushed, and of a disk's own layer while no
        /// head names it.
        #[arg(long)]
        force: bool,
    },
    /// Per-device and per-memory-handler counters.
    Stats {
        /// Empty = every device and handler.
        pod_id: Option<String>,
    },
    /// Manifests ↔ chunks ↔ S3 consistency, GC candidates.
    Fsck {
        /// Also probe S3 for chunks missing locally.
        #[arg(long = "s3")]
        check_s3: bool,
    },
    /// Object-store GC (ADR 0011). The caller knows what is live,
    /// so it is spelled out on the command line; an empty one means
    /// "nothing is referenced" and only the grace protects the bucket —
    /// hence --dry-run being the sane first move.
    Sweep {
        /// Plan only: report what WOULD go, delete nothing.
        #[arg(long)]
        dry_run: bool,
        /// Labs only: no grace window at all.
        #[arg(long, conflicts_with = "grace")]
        no_grace: bool,
        /// Never delete anything younger than this [default: the daemon's 24 h].
        #[arg(long)]
        grace: Option<u64>,
        /// Soft wall-clock budget for one sweep, in seconds.
        #[arg(long)]
        budget: Option<u64>,
        /// S3 key of a head.json a pod row still points at (repeatable).
        #[arg(long = "head")]
        heads: Vec<String>,
        /// Layer id the image catalog pins (repeatable).
        #[arg(long = "layer")]
        layers: Vec<String>,
    },
    /// Upload a checkpoint's head for a sealed, stopped disk, last.
    PutHead {
        pod_id: String,
        #[arg(long)]
        epoch: u64,
        /// The memory layer (seal-file --upload); without, the disk alone.
        #[arg(long)]
        memory_layer: Option<String>,
        /// The vmstate's key (put-object), with --memory-layer.
        #[arg(long, requires = "memory_layer")]
        vmstate_key: Option<String>,
        #[arg(long, default_value = "")]
        firecracker_version: String,
        #[arg(long, default_value = "")]
        cpu_model: String,
        #[arg(long, default_value = "")]
        kernel_release: String,
        /// The head of one of the pod's volumes (repeatable).
        #[arg(long = "volume-head")]
        volume_heads: Vec<String>,
    },
    /// Read a head from the object store.
    GetHead { key: String },
    /// Debug: dump a ublk device's kernel state.
    DevDump { dev_id: i32 },
    /// Debug: delete an orphaned (quiesced) ublk device.
    DevDel { dev_id: i32 },
}

fn head_json(h: &pb::Head) -> String {
    serde_json::to_string_pretty(&serde_json::json!({
        "key": h.key, "pod_id": h.pod_id, "epoch": h.epoch,
        "chain": h.chain, "logical_size": h.logical_size,
        "memory": h.memory.as_ref().map(|m| serde_json::json!({
            "layer": m.layer, "vmstate_key": m.vmstate_key,
            "firecracker_version": m.firecracker_version,
            "cpu_model": m.cpu_model, "kernel_release": m.kernel_release,
        })),
        "volume_heads": h.volume_heads,
    }))
    .unwrap_or_default()
}

async fn connect(sock: &str) -> Result<DiskServiceClient<tonic::transport::Channel>, String> {
    let sock = sock.to_string();
    let channel = Endpoint::try_from("http://[::]:50051")
        .map_err(|e| e.to_string())?
        .connect_with_connector(service_fn(move |_: Uri| {
            let sock = sock.clone();
            async move {
                tokio::net::UnixStream::connect(sock)
                    .await
                    .map(TokioIo::new)
            }
        }))
        .await
        .map_err(|e| format!("connect: {e}"))?;
    Ok(DiskServiceClient::new(channel))
}

#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<(), String> {
    let cli = Cli::parse();
    // The two debug commands talk to the kernel, not to the daemon.
    let mut c = match cli.cmd {
        Cmd::DevDump { .. } | Cmd::DevDel { .. } => None,
        _ => Some(connect(&cli.socket).await?),
    };
    macro_rules! c {
        () => {
            c.as_mut().ok_or("no connection")?
        };
    }
    let out = match cli.cmd {
        Cmd::ImportBase { path, chunk_size } => {
            let l = c!()
                .import_base(pb::ImportBaseRequest {
                    path,
                    chunk_size: chunk_size.unwrap_or(0),
                })
                .await
                .map_err(|e| e.message().to_string())?
                .into_inner();
            serde_json::to_string_pretty(&serde_json::json!({
                "layer_id": l.layer_id, "entries": l.entries,
                "chunks_new": l.chunks_new, "bytes_new": l.bytes_new,
                "seal_seconds": l.seal_seconds,
            }))
            .map_err(|e| e.to_string())?
        }
        Cmd::Create { pod_id, chain } => {
            let d = c!()
                .create_disk(pb::CreateDiskRequest {
                    pod_id,
                    chain,
                    logical_size: 0,
                })
                .await
                .map_err(|e| e.message().to_string())?
                .into_inner();
            serde_json::to_string_pretty(&serde_json::json!({
                "pod_id": d.pod_id, "dev_path": d.dev_path,
                "link_path": d.link_path, "size_bytes": d.size_bytes,
            }))
            .map_err(|e| e.to_string())?
        }
        Cmd::Delete { pod_id } => {
            c!().remove_device(pb::RemoveDeviceRequest { pod_id })
                .await
                .map_err(|e| e.message().to_string())?;
            "ok".into()
        }
        Cmd::Open { pod_id, head } => {
            let d = c!()
                .open_disk(pb::OpenDiskRequest {
                    pod_id,
                    head_key: head.unwrap_or_default(),
                })
                .await
                .map_err(|e| e.message().to_string())?
                .into_inner();
            serde_json::to_string_pretty(&serde_json::json!({
                "dev_path": d.dev_path, "link_path": d.link_path,
            }))
            .map_err(|e| e.to_string())?
        }
        Cmd::Bake { pod_id, upload } => {
            let l = c!()
                .bake(pb::BakeRequest { pod_id, upload })
                .await
                .map_err(|e| format!("{}: {}", e.code(), e.message()))?
                .into_inner();
            serde_json::to_string_pretty(&serde_json::json!({
                "layer_id": l.layer_id, "entries": l.entries,
                "chunks_new": l.chunks_new, "chunks_pool_hit": l.chunks_pool_hit,
                "bytes_new": l.bytes_new, "seal_seconds": l.seal_seconds,
            }))
            .unwrap_or_default()
        }
        Cmd::SealFile {
            path,
            upload,
            chunk_size,
        } => {
            let l = c!()
                .seal_file(pb::SealFileRequest {
                    path,
                    upload,
                    chunk_size,
                })
                .await
                .map_err(|e| e.message().to_string())?
                .into_inner();
            serde_json::to_string_pretty(&serde_json::json!({
                "layer_id": l.layer_id, "entries": l.entries,
                "chunks_new": l.chunks_new, "chunks_pool_hit": l.chunks_pool_hit,
                "chunks_zeroed": l.chunks_zeroed,
                "bytes_new": l.bytes_new, "seal_seconds": l.seal_seconds,
            }))
            .unwrap_or_default()
        }
        Cmd::Stop { pod_id } => {
            c!().stop_disk(pb::StopDiskRequest { pod_id })
                .await
                .map_err(|e| e.message().to_string())?;
            "ok".into()
        }
        Cmd::Seal { pod_id, upload } => {
            let l = c!()
                .seal(pb::SealRequest { pod_id, upload })
                .await
                .map_err(|e| e.message().to_string())?
                .into_inner();
            serde_json::to_string_pretty(&serde_json::json!({
                "layer_id": l.layer_id,
                "entries": l.entries,
                "chunks_new": l.chunks_new,
                "seal_seconds": l.seal_seconds,
            }))
            .unwrap_or_default()
        }
        Cmd::Materialize { out_path, chain } => {
            c!().materialize(pb::MaterializeRequest { chain, out_path })
                .await
                .map_err(|e| e.message().to_string())?;
            "ok".into()
        }
        Cmd::Push { layer_id, warm } => {
            c!().push(pb::LayerRef { layer_id, warm })
                .await
                .map_err(|e| e.message().to_string())?;
            "ok".into()
        }
        Cmd::Pull { layer_id, warm } => {
            c!().pull(pb::LayerRef { layer_id, warm })
                .await
                .map_err(|e| e.message().to_string())?;
            "ok".into()
        }
        Cmd::Evict { target, force } => {
            let target = match target {
                EvictTarget::Budget => pb::evict_request::Target::Budget as i32,
                EvictTarget::All => pb::evict_request::Target::All as i32,
            };
            let s = c!()
                .evict(pb::EvictRequest { target, force })
                .await
                .map_err(|e| e.message().to_string())?
                .into_inner();
            serde_json::to_string_pretty(&serde_json::json!({
                "removed_entries": s.removed_entries, "freed_bytes": s.freed_bytes,
            }))
            .map_err(|e| e.to_string())?
        }
        Cmd::Stats { pod_id } => {
            let s = c!()
                .stats(pb::StatsRequest {
                    pod_id: pod_id.unwrap_or_default(),
                })
                .await
                .map_err(|e| e.message().to_string())?
                .into_inner();
            serde_json::to_string_pretty(&serde_json::json!({
                "object_store": s.object_store,
                "cache_bytes": s.cache_bytes, "cache_entries": s.cache_entries,
                "s3_puts": s.s3_puts, "s3_put_bytes": s.s3_put_bytes,
                "s3_gets": s.s3_gets, "s3_get_bytes": s.s3_get_bytes,
                "devices": s.devices.iter().map(|d| serde_json::json!({
                    "pod_id": d.pod_id, "dev_path": d.dev_path,
                    "reads": d.reads, "read_bytes": d.read_bytes,
                    "writes": d.writes, "write_bytes": d.write_bytes,
                    "flushes": d.flushes, "errors": d.errors,
                    "cache_hits": d.cache_hits, "cache_misses": d.cache_misses,
                    "s3_fetches": d.s3_fetches, "s3_fetch_bytes": d.s3_fetch_bytes,
                    "read_p99_us": d.histograms.first().map(|h| h.p99_us),
                })).collect::<Vec<_>>(),
                "memories": s.memories.iter().map(|m| serde_json::json!({
                    "pod_id": m.pod_id, "attached": m.attached,
                    "faults": m.faults, "chunks_served": m.chunks_served,
                    "chunks_zeroed": m.chunks_zeroed, "chunks_total": m.chunks_total,
                    "bytes_served": m.bytes_served,
                    "cache_hits": m.cache_hits, "cache_misses": m.cache_misses,
                    "s3_fetches": m.s3_fetches, "s3_fetch_bytes": m.s3_fetch_bytes,
                    "removes": m.removes, "pages_reclaimed": m.pages_reclaimed,
                    "errors": m.errors,
                })).collect::<Vec<_>>(),
            }))
            .map_err(|e| e.to_string())?
        }
        Cmd::Fsck { check_s3 } => {
            let r = c!()
                .fsck(pb::FsckRequest { check_s3 })
                .await
                .map_err(|e| e.message().to_string())?
                .into_inner();
            serde_json::to_string_pretty(&serde_json::json!({
                "complete_layers": r.complete_layers.len(),
                "dangling_layers": r.dangling_layers,
                "garbage_chunks": r.garbage_chunks.len(),
                "garbage_chunk_ids": r.garbage_chunks,
                "garbage_bytes": r.garbage_bytes,
            }))
            .map_err(|e| e.to_string())?
        }
        Cmd::Sweep {
            dry_run,
            no_grace,
            grace,
            budget,
            heads,
            layers,
        } => {
            let r = c!()
                .sweep(pb::SweepRequest {
                    live_heads: heads,
                    live_layers: layers,
                    grace_seconds: grace.unwrap_or(0),
                    dry_run,
                    budget_seconds: budget.unwrap_or(0),
                    no_grace,
                    max_keys: 0,
                })
                .await
                .map_err(|e| e.message().to_string())?
                .into_inner();
            serde_json::to_string_pretty(&serde_json::json!({
                "dry_run": dry_run,
                "snapshot_objects_seen": r.snapshot_objects_seen,
                "snapshot_objects_deleted": r.snapshot_objects_deleted,
                "layers_seen": r.layers_seen,
                "layers_deleted": r.layers_deleted,
                "chunks_seen": r.chunks_seen,
                "chunks_deleted": r.chunks_deleted,
                "bytes_deleted": r.bytes_deleted,
                "budget_exhausted": r.budget_exhausted,
                "deleted_keys": r.deleted_keys,
            }))
            .map_err(|e| e.to_string())?
        }
        Cmd::PutHead {
            pod_id,
            epoch,
            memory_layer,
            vmstate_key,
            firecracker_version,
            cpu_model,
            kernel_release,
            volume_heads,
        } => {
            let memory = memory_layer.map(|layer| pb::MemoryRef {
                layer,
                vmstate_key: vmstate_key.unwrap_or_default(),
                firecracker_version,
                cpu_model,
                kernel_release,
            });
            let h = c!()
                .put_head(pb::PutHeadRequest {
                    pod_id,
                    epoch,
                    memory,
                    volume_heads,
                })
                .await
                .map_err(|e| format!("{}: {}", e.code(), e.message()))?
                .into_inner();
            head_json(&h)
        }
        Cmd::GetHead { key } => {
            let h = c!()
                .get_head(pb::GetHeadRequest { key })
                .await
                .map_err(|e| format!("{}: {}", e.code(), e.message()))?
                .into_inner();
            head_json(&h)
        }
        Cmd::DevDump { dev_id } => {
            let ctrl = libublk::ctrl::UblkCtrl::new_simple(dev_id).map_err(|e| e.to_string())?;
            ctrl.dump();
            "ok".into()
        }
        Cmd::DevDel { dev_id } => {
            let ctrl = libublk::ctrl::UblkCtrl::new_simple(dev_id).map_err(|e| e.to_string())?;
            let _ = ctrl.kill_dev();
            ctrl.del_dev()
                .map(|_| "deleted".to_string())
                .map_err(|e| e.to_string())?
        }
    };
    println!("{out}");
    Ok(())
}
