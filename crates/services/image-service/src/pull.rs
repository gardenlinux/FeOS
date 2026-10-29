// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Pulling an OCI image into a directory.
//!
//! Layers are streamed: a gzip tar layer is decompressed and unpacked into
//! `rootfs/` as it arrives, so the transfer and the unpack overlap. Layers
//! must be applied in order, but nothing says they must be fetched in order:
//! the first layer streams straight into the unpacker while the others are
//! fetched concurrently into temporary blobs (at most [`PREFETCH`] at a
//! time), the shape containerd uses. VM artifacts (kernel, initramfs, disk
//! image) are written to files of their own.
//!
//! Every request is retried, and a transfer that stops making progress
//! counts as failed: the registry client exposes no read timeout, so a
//! half-dead connection would otherwise sit there forever.

use crate::error::ImageServiceError;
use crate::unpack::unpack_layer;
use crate::PullToken;
use log::{info, warn};
use oci_client::client::{ClientConfig, ClientProtocol};
use oci_client::manifest::{self, OciDescriptor};
use oci_client::{Client, Reference};
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::task::Poll;
use std::time::Duration;
use tokio::sync::{oneshot, OwnedSemaphorePermit, Semaphore};

const SQUASHFS_MEDIA_TYPE: &str = "application/vnd.ironcore.image.squashfs.v1alpha1.squashfs";
const INITRAMFS_MEDIA_TYPE: &str = "application/vnd.ironcore.image.initramfs.v1alpha1.initramfs";
const VMLINUZ_MEDIA_TYPE: &str = "application/vnd.ironcore.image.vmlinuz.v1alpha1.vmlinuz";
const ROOTFS_MEDIA_TYPE: &str = "application/vnd.ironcore.image.rootfs.v1alpha1.rootfs";

/// How many layer blobs may be in flight ahead of the unpacker. Bounds the
/// concurrent connections and the temporary space a large image takes.
const PREFETCH: usize = 3;
/// A registry request may fail to reach the other end: a reset connection,
/// a DNS hiccup, a 5xx from a CDN edge.
const PULL_ATTEMPTS: u32 = 4;
/// A transfer that has moved no byte for this long is a failed transfer.
const STALL_TIMEOUT: Duration = Duration::from_secs(20);
/// A manifest is a few KiB.
const MANIFEST_TIMEOUT: Duration = Duration::from_secs(30);

/// What a pull produced besides the files in its directory.
#[derive(Debug)]
pub struct Pulled {
    /// Digest of the image config: the image's content id.
    pub digest: String,
    /// The OCI image config.
    pub config: Vec<u8>,
    /// Compressed bytes of config and layers.
    pub size_bytes: u64,
}

/// What to do with a layer.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Layer {
    /// A gzip tar, unpacked into rootfs/.
    Tar,
    /// A VM artifact, stored under this file name.
    File(&'static str),
    /// A layer FeOS accepts but does not use.
    Skip,
}

fn classify(media_type: &str) -> Result<Layer, ImageServiceError> {
    Ok(match media_type {
        manifest::IMAGE_LAYER_GZIP_MEDIA_TYPE | manifest::IMAGE_DOCKER_LAYER_GZIP_MEDIA_TYPE => {
            Layer::Tar
        }
        ROOTFS_MEDIA_TYPE => Layer::File("disk.image"),
        INITRAMFS_MEDIA_TYPE => Layer::File("initramfs"),
        VMLINUZ_MEDIA_TYPE => Layer::File("vmlinuz"),
        SQUASHFS_MEDIA_TYPE => Layer::Skip,
        other => {
            return Err(ImageServiceError::UnsupportedLayer(other.to_string()));
        }
    })
}

/// HTTPS, except for a registry on the loopback interface, which may speak
/// plain HTTP, as docker allows: a local registry for development and tests.
fn client_config(reference: &Reference) -> ClientConfig {
    let registry = reference.resolve_registry();
    let protocol = if is_loopback(registry) {
        ClientProtocol::HttpsExcept(vec![registry.to_string()])
    } else {
        ClientProtocol::Https
    };
    ClientConfig {
        protocol,
        ..Default::default()
    }
}

/// Whether a registry's host ("host", "host:port", "[v6]:port") is this
/// machine.
fn is_loopback(registry: &str) -> bool {
    let host = match registry.strip_prefix('[') {
        Some(rest) => rest.split(']').next().unwrap_or_default(),
        None => registry.split(':').next().unwrap_or_default(),
    };
    host == "localhost"
        || host
            .parse::<std::net::IpAddr>()
            .is_ok_and(|ip| ip.is_loopback())
}

/// Normalizes a reference the way docker does: "debian:13" becomes
/// "docker.io/library/debian:13", a missing tag becomes ":latest".
pub fn normalize_ref(image_ref: &str) -> Result<String, ImageServiceError> {
    let reference = Reference::try_from(image_ref.trim())?;
    Ok(reference.whole())
}

/// Pulls `image_ref` into `dest`, which must exist and be empty: gzip tar
/// layers end up in `dest/rootfs/`, VM artifacts in `dest/disk.image`,
/// `dest/initramfs` and `dest/vmlinuz`.
/// The pull is anonymous, or with `token`'s Bearer token (ADR 0025), which
/// the registry client keeps for the layers.
pub async fn pull_image(
    image_ref: &str,
    dest: &Path,
    token: &PullToken,
) -> Result<Pulled, ImageServiceError> {
    let auth = token.auth();
    let reference = Reference::try_from(image_ref)?;
    let client = Arc::new(Client::new(client_config(&reference)));

    info!("ImagePuller: pulling manifest and config for {image_ref}");
    let (manifest, _digest, config) = retry("manifest", || {
        let client = client.clone();
        let reference = reference.clone();
        let auth = auth.clone();
        async move {
            tokio::time::timeout(
                MANIFEST_TIMEOUT,
                client.pull_manifest_and_config(&reference, &auth),
            )
            .await
            .map_err(|_| format!("no manifest within {}s", MANIFEST_TIMEOUT.as_secs()))?
            .map_err(|e| e.to_string())
        }
    })
    .await?;

    let kinds = manifest
        .layers
        .iter()
        .map(|layer| classify(&layer.media_type))
        .collect::<Result<Vec<_>, _>>()?;
    if kinds.iter().all(|kind| *kind == Layer::Skip) {
        return Err(ImageServiceError::MissingLayer(
            "no layer FeOS can use".to_string(),
        ));
    }
    let rootfs = dest.join("rootfs");
    if kinds.contains(&Layer::Tar) {
        std::fs::create_dir_all(&rootfs)?;
    }

    // Start every fetch except a leading tar layer, which streams.
    let mut slots = Slots::new(PREFETCH);
    let mut fetches = Vec::with_capacity(kinds.len());
    for (index, (layer, kind)) in manifest.layers.iter().zip(&kinds).enumerate() {
        let path = match kind {
            Layer::Tar if index == 0 => None,
            Layer::Tar => Some(dest.join(format!("layer.{index}.blob"))),
            Layer::File(name) => Some(dest.join(name)),
            Layer::Skip => {
                warn!(
                    "ImagePuller: skipping layer {} ({})",
                    layer.digest, layer.media_type
                );
                None
            }
        };
        fetches.push(path.map(|path| {
            let client = client.clone();
            let reference = reference.clone();
            let layer = layer.clone();
            let slot = slots.next();
            tokio::spawn(async move {
                let permit = slot.await?;
                fetch_to_file(&client, &reference, &layer, &path).await?;
                let size = std::fs::metadata(&path).map(|m| m.len()).unwrap_or(0);
                Ok::<(u64, OwnedSemaphorePermit), ImageServiceError>((size, permit))
            })
        }));
    }

    let mut size_bytes = config.len() as u64;
    for (index, (layer, kind)) in manifest.layers.iter().zip(&kinds).enumerate() {
        match (kind, fetches[index].take()) {
            (Layer::Tar, None) => {
                info!("ImagePuller: streaming layer {}", layer.digest);
                size_bytes += stream_first_layer(&client, &reference, layer, &rootfs).await?;
            }
            (Layer::Tar, Some(fetch)) => {
                let (size, permit) = join(fetch).await?;
                let blob = dest.join(format!("layer.{index}.blob"));
                let rootfs = rootfs.clone();
                let digest = layer.digest.clone();
                tokio::task::spawn_blocking(move || {
                    let result = std::fs::File::open(&blob).and_then(|f| unpack_layer(f, &rootfs));
                    // Free the prefetch slot as soon as the layer is applied.
                    let _ = std::fs::remove_file(&blob);
                    drop(permit);
                    result
                })
                .await
                .map_err(|e| ImageServiceError::Internal(e.to_string()))?
                .map_err(|e| ImageServiceError::Pull(format!("unpack layer {digest}: {e}")))?;
                size_bytes += size;
            }
            (Layer::File(_), Some(fetch)) => size_bytes += join(fetch).await?.0,
            _ => {}
        }
    }

    Ok(Pulled {
        digest: manifest.config.digest,
        config: config.into_bytes(),
        size_bytes,
    })
}

/// Hands out the [`PREFETCH`] slots in layer order. [`pull_image`] frees a
/// slot once it applied the slot's layer, so in layer order too; fetches
/// that raced for the slots could take them all for later layers, leaving
/// none for the layer it waits for, and the pull would hang without a
/// transfer for the stall guard to catch.
struct Slots {
    semaphore: Arc<Semaphore>,
    turn: Option<oneshot::Receiver<()>>,
}

impl Slots {
    fn new(slots: usize) -> Self {
        Self {
            semaphore: Arc::new(Semaphore::new(slots)),
            turn: None,
        }
    }

    /// The slot of the next layer, taken only after that of the layer
    /// before (or once its fetch ended without one).
    fn next(
        &mut self,
    ) -> impl std::future::Future<Output = Result<OwnedSemaphorePermit, ImageServiceError>> + Send + use<>
    {
        let semaphore = self.semaphore.clone();
        let (taken, next) = oneshot::channel();
        let before = self.turn.replace(next);
        async move {
            if let Some(before) = before {
                let _ = before.await;
            }
            let permit = semaphore
                .acquire_owned()
                .await
                .map_err(|e| ImageServiceError::Internal(e.to_string()))?;
            let _ = taken.send(());
            Ok(permit)
        }
    }
}

async fn join(
    fetch: tokio::task::JoinHandle<Result<(u64, OwnedSemaphorePermit), ImageServiceError>>,
) -> Result<(u64, OwnedSemaphorePermit), ImageServiceError> {
    fetch
        .await
        .map_err(|e| ImageServiceError::Internal(format!("fetch task: {e}")))?
}

/// Fetches a blob into a file of its own; a retry truncates what the
/// previous attempt left.
async fn fetch_to_file(
    client: &Arc<Client>,
    reference: &Reference,
    layer: &OciDescriptor,
    path: &Path,
) -> Result<(), ImageServiceError> {
    retry(&format!("layer {}", layer.digest), || {
        let client = client.clone();
        let reference = reference.clone();
        let layer = layer.clone();
        let path = path.to_path_buf();
        async move {
            // pull_blob hands its sink many small chunks, and every write on
            // a tokio::fs::File is a round trip to the blocking pool: buffer.
            let file = tokio::fs::File::create(&path)
                .await
                .map_err(|e| format!("{}: {e}", path.display()))?;
            let seen = Arc::new(AtomicU64::new(0));
            let mut sink = ProgressWriter {
                inner: tokio::io::BufWriter::with_capacity(1 << 20, file),
                seen: seen.clone(),
            };
            with_stall_guard(
                &format!("layer {}", layer.digest),
                seen,
                client.pull_blob(&reference, &layer, &mut sink),
            )
            .await?;
            tokio::io::AsyncWriteExt::flush(&mut sink)
                .await
                .map_err(|e| format!("{}: {e}", path.display()))
        }
    })
    .await
}

/// Streams the first layer through a pipe into the unpacker, so that its
/// compressed bytes never touch the disk; the pipe's buffer throttles the
/// download when the disk is slower. It can be retried because it is the
/// first layer: emptying rootfs/ undoes a failed attempt.
async fn stream_first_layer(
    client: &Arc<Client>,
    reference: &Reference,
    layer: &OciDescriptor,
    rootfs: &Path,
) -> Result<u64, ImageServiceError> {
    retry(&format!("layer {} (streamed)", layer.digest), || {
        let client = client.clone();
        let reference = reference.clone();
        let layer = layer.clone();
        let rootfs = rootfs.to_path_buf();
        async move { stream_layer_once(&client, &reference, &layer, rootfs).await }
    })
    .await
}

async fn stream_layer_once(
    client: &Client,
    reference: &Reference,
    layer: &OciDescriptor,
    rootfs: PathBuf,
) -> Result<u64, String> {
    // What a try before unpacked: many files, removed on the blocking pool.
    let cleared = rootfs.clone();
    tokio::task::spawn_blocking(move || {
        let _ = std::fs::remove_dir_all(&cleared);
        std::fs::create_dir_all(&cleared)
    })
    .await
    .map_err(|e| e.to_string())?
    .map_err(|e| e.to_string())?;
    let (reader, writer) = nix::unistd::pipe().map_err(|e| format!("layer pipe: {e}"))?;
    // A 1 MiB pipe lets the transfer run ahead of the unpacker instead of
    // blocking on almost every chunk, and tokio's pipe sender needs the fd
    // non-blocking.
    let _ = nix::fcntl::fcntl(&writer, nix::fcntl::FcntlArg::F_SETPIPE_SZ(1 << 20));
    let flags = nix::fcntl::fcntl(&writer, nix::fcntl::FcntlArg::F_GETFL)
        .map_err(|e| format!("layer pipe: {e}"))?;
    nix::fcntl::fcntl(
        &writer,
        nix::fcntl::FcntlArg::F_SETFL(
            nix::fcntl::OFlag::from_bits_retain(flags) | nix::fcntl::OFlag::O_NONBLOCK,
        ),
    )
    .map_err(|e| format!("layer pipe: {e}"))?;

    let unpack = tokio::task::spawn_blocking(move || -> std::io::Result<u64> {
        let mut counted = CountingRead {
            inner: std::fs::File::from(reader),
            n: 0,
        };
        unpack_layer(&mut counted, &rootfs)?;
        // tar stops at its end-of-archive marker; drain the gzip trailer
        // and padding, or the writer blocks on a full pipe forever.
        std::io::copy(&mut counted, &mut std::io::sink())?;
        Ok(counted.n)
    });

    // Not tokio::fs::File: on a pipe that costs a blocking-pool round trip
    // per chunk. The pipe sender is epoll-driven.
    let sender = tokio::net::unix::pipe::Sender::from_owned_fd(writer)
        .map_err(|e| format!("layer pipe: {e}"))?;
    let seen = Arc::new(AtomicU64::new(0));
    let mut sink = ProgressWriter {
        inner: sender,
        seen: seen.clone(),
    };
    let pulled = with_stall_guard(
        &format!("layer {}", layer.digest),
        seen,
        client.pull_blob(reference, layer, &mut sink),
    )
    .await;
    // The unpacker gets EOF only once every byte is out and this end closed.
    let flushed = tokio::io::AsyncWriteExt::flush(&mut sink).await;
    drop(sink);

    let unpacked = unpack.await.map_err(|e| format!("unpack task: {e}"))?;
    // A failing unpacker breaks the pipe, so the pull would only report
    // EPIPE: report the unpacker first.
    let size = unpacked.map_err(|e| format!("unpack layer {}: {e}", layer.digest))?;
    pulled?;
    flushed.map_err(|e| format!("layer pipe: {e}"))?;
    Ok(size)
}

/// Counts the bytes read, so a streamed layer still reports its size.
struct CountingRead {
    inner: std::fs::File,
    n: u64,
}

impl std::io::Read for CountingRead {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        let k = self.inner.read(buf)?;
        self.n += k as u64;
        Ok(k)
    }
}

/// Counts the bytes written, so that the stall guard can tell a slow
/// transfer from a dead one.
struct ProgressWriter<W> {
    inner: W,
    seen: Arc<AtomicU64>,
}

impl<W: tokio::io::AsyncWrite + Unpin> tokio::io::AsyncWrite for ProgressWriter<W> {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &[u8],
    ) -> Poll<std::io::Result<usize>> {
        let result = Pin::new(&mut self.inner).poll_write(cx, buf);
        if let Poll::Ready(Ok(n)) = &result {
            self.seen.fetch_add(*n as u64, Ordering::Relaxed);
        }
        result
    }

    fn poll_flush(
        mut self: Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.inner).poll_flush(cx)
    }

    fn poll_shutdown(
        mut self: Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.inner).poll_shutdown(cx)
    }
}

/// Runs `transfer`, failing it when `seen` has not moved for
/// [`STALL_TIMEOUT`]. Dropping the future cancels the transfer.
async fn with_stall_guard<T, E: std::fmt::Display>(
    what: &str,
    seen: Arc<AtomicU64>,
    transfer: impl std::future::Future<Output = Result<T, E>>,
) -> Result<T, String> {
    let watchdog = async {
        let mut last = seen.load(Ordering::Relaxed);
        loop {
            tokio::time::sleep(STALL_TIMEOUT).await;
            let now = seen.load(Ordering::Relaxed);
            if now == last {
                return now;
            }
            last = now;
        }
    };
    tokio::pin!(transfer);
    tokio::select! {
        result = &mut transfer => result.map_err(|e| e.to_string()),
        bytes = watchdog => Err(format!(
            "{what}: no progress for {}s after {bytes} bytes",
            STALL_TIMEOUT.as_secs()
        )),
    }
}

/// Retries `attempt` with linear backoff. The closure builds a fresh
/// request every time.
async fn retry<T, F, Fut>(what: &str, mut attempt: F) -> Result<T, ImageServiceError>
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<Output = Result<T, String>>,
{
    let mut last = String::new();
    for n in 1..=PULL_ATTEMPTS {
        match attempt().await {
            Ok(value) => {
                if n > 1 {
                    info!("ImagePuller: {what} succeeded on attempt {n}");
                }
                return Ok(value);
            }
            Err(e) => {
                last = e;
                if n < PULL_ATTEMPTS {
                    warn!("ImagePuller: {what}: attempt {n} failed ({last}), retrying");
                    tokio::time::sleep(Duration::from_secs(n.into())).await;
                }
            }
        }
    }
    Err(ImageServiceError::Pull(format!(
        "{what}: {last} (after {PULL_ATTEMPTS} attempts)"
    )))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn normalizes_references_like_docker() {
        for (short, full) in [
            ("debian:13", "docker.io/library/debian:13"),
            ("debian", "docker.io/library/debian:latest"),
            ("ghcr.io/maltej/debian-ai:1", "ghcr.io/maltej/debian-ai:1"),
            ("localhost:5000/x", "localhost:5000/x:latest"),
        ] {
            assert_eq!(normalize_ref(short).unwrap(), full);
        }
    }

    /// Pulls a real image from Docker Hub. Needs the network, and root:
    /// unpacking hands files to the image's owners.
    #[tokio::test]
    #[ignore]
    async fn pulls_and_unpacks_a_real_image() {
        let dest = tempfile::tempdir().unwrap();
        let pulled = pull_image(
            "docker.io/library/busybox:1.36",
            dest.path(),
            &PullToken::default(),
        )
        .await
        .unwrap();
        assert!(pulled.digest.starts_with("sha256:"));
        assert!(pulled.size_bytes > 0);
        assert!(dest.path().join("rootfs/bin/busybox").is_file());
        assert!(!dest.path().join("layer.1.blob").exists());
    }

    /// Slots go to the layers in order, whichever fetch asks first: a later
    /// layer that asked first must not take the slot an earlier one needs.
    #[tokio::test]
    async fn slots_go_to_the_layers_in_order() {
        let mut slots = Slots::new(2);
        let (first, second, third) = (slots.next(), slots.next(), slots.next());
        let third = tokio::spawn(third);
        let second = tokio::spawn(second);
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(!second.is_finished() && !third.is_finished());

        let first = first.await.unwrap();
        let second = second.await.unwrap().unwrap();
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(!third.is_finished());

        drop(first);
        drop(third.await.unwrap().unwrap());
        drop(second);
    }

    #[test]
    fn only_loopback_registries_may_speak_http() {
        for registry in [
            "localhost",
            "localhost:5000",
            "127.0.0.1:5000",
            "[::1]:5000",
        ] {
            assert!(is_loopback(registry), "{registry}");
        }
        for registry in [
            "ghcr.io",
            "docker.io",
            "10.0.0.1:5000",
            "localhost.example.com",
        ] {
            assert!(!is_loopback(registry), "{registry}");
        }
        let reference = Reference::try_from("127.0.0.1:5000/feos/guest:test").unwrap();
        assert!(matches!(
            client_config(&reference).protocol,
            ClientProtocol::HttpsExcept(ref hosts) if hosts == &["127.0.0.1:5000".to_string()]
        ));
    }
}
