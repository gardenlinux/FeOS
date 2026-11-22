// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! S3 tiering (ADR 0011). Any S3-compatible object store, path-style, with
//! a custom endpoint. Env-driven:
//!
//!   AWS_ENDPOINT_URL        e.g. https://s3.example.com
//!   AWS_REGION              default "us-east-1"
//!   AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, or a role the platform
//!   hands the pod: AWS_CONTAINER_CREDENTIALS_FULL_URI (EKS Pod Identity)
//!   or AWS_WEB_IDENTITY_TOKEN_FILE (IRSA)
//!   S3_BUCKET
//!
//! Ordering rule (ADR 0011): chunks are uploaded *before* any manifest that
//! references them. Tests run under a per-run `diskd-test/<run-id>/`
//! prefix and clean it up.
//!
//! Chunk objects are whole-object GETs — comp_len in the manifest is
//! informational (see manifest.rs).

use std::time::{Duration, Instant};

use futures::StreamExt as _;
use object_store::path::Path as ObjPath;
use object_store::ObjectStore;
use tracing::instrument;

use crate::chunk::{self, Hash};
use crate::error::{Error, Result};

/// One remote object as the GC sweeper sees it: key
/// relative to the client prefix, size, and mtime in unix seconds (the
/// grace window is computed against it — an object younger than the
/// grace is never swept, because a concurrent upload may still be
/// working its way up to the head that will reference it).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RemoteObject {
    pub key: String,
    pub size: u64,
    pub modified_unix: i64,
}

/// The lowercase hex SHA-256 of "<endpoint>\n<bucket>", which names an
/// object store across nodes.
pub fn store_id(endpoint: &str, bucket: &str) -> String {
    use sha2::Digest;
    sha2::Sha256::digest(format!("{endpoint}\n{bucket}"))
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

pub struct S3 {
    store: object_store::aws::AmazonS3,
    /// Key prefix, "" in production, "diskd-test/<run-id>/" in tests.
    prefix: String,
    /// Which store this is, `store_id`; empty when it is not known.
    id: String,
    pub bytes_up: std::sync::atomic::AtomicU64,
    pub bytes_down: std::sync::atomic::AtomicU64,
    /// Whole-object PUTs issued (chunks + manifests + objects); tests
    /// count pushes ("no chunk PUT twice").
    pub puts: std::sync::atomic::AtomicU64,
    /// Whole-object GETs issued. Tests use it as the honest answer to
    /// "did this restore really come from S3, or from the local cache?" —
    /// evict the cache, restore, and the counter has to move by at least
    /// the checkpoint's chunk count.
    pub gets: std::sync::atomic::AtomicU64,
}

impl S3 {
    pub fn raw_store(&self) -> &object_store::aws::AmazonS3 {
        &self.store
    }

    pub fn new(store: object_store::aws::AmazonS3, prefix: impl Into<String>) -> Self {
        S3 {
            store,
            prefix: prefix.into(),
            id: String::new(),
            bytes_up: 0.into(),
            bytes_down: 0.into(),
            puts: 0.into(),
            gets: 0.into(),
        }
    }

    /// Build from process env. Returns Ok(None) when S3_BUCKET or the
    /// credentials are absent — callers treat that as "tiering disabled";
    /// tests skip.
    pub fn from_env(prefix: impl Into<String>) -> Result<Option<S3>> {
        let bucket = match std::env::var("S3_BUCKET") {
            Ok(b) if !b.is_empty() => b,
            _ => return Ok(None),
        };
        let endpoint = std::env::var("AWS_ENDPOINT_URL").ok();
        let region = std::env::var("AWS_REGION").unwrap_or_else(|_| "us-east-1".into());
        let set = |name: &str| std::env::var(name).is_ok_and(|v| !v.is_empty());
        let ak = std::env::var("AWS_ACCESS_KEY_ID").ok();
        let sk = std::env::var("AWS_SECRET_ACCESS_KEY").ok();
        let b = if let (Some(ak), Some(sk)) = (ak, sk) {
            object_store::aws::AmazonS3Builder::new()
                .with_access_key_id(&ak)
                .with_secret_access_key(&sk)
        } else if set("AWS_CONTAINER_CREDENTIALS_FULL_URI") || set("AWS_WEB_IDENTITY_TOKEN_FILE") {
            // A role the platform hands the pod (EKS Pod Identity, IRSA):
            // object_store fetches its credentials and renews them.
            object_store::aws::AmazonS3Builder::from_env()
        } else {
            return Ok(None);
        };
        let mut b = b
            .with_bucket_name(&bucket)
            .with_region(&region)
            // Ceph/radosgw: path-style.
            .with_virtual_hosted_style_request(false);
        // object_store's endpoint for the region when none is configured.
        let id = store_id(
            endpoint
                .as_deref()
                .unwrap_or(&format!("https://s3.{region}.amazonaws.com")),
            &bucket,
        );
        if let Some(ep) = endpoint {
            b = b.with_endpoint(ep);
        }
        let store = b.build().map_err(Error::from)?;
        Ok(Some(S3 {
            id,
            ..S3::new(store, prefix)
        }))
    }

    /// Which object store this is (feos.disk.v1 `object_store_id`): a pod
    /// hibernated through one node restores through another with the same.
    pub fn id(&self) -> &str {
        &self.id
    }

    fn key(&self, kind: &str, name: &str) -> ObjPath {
        ObjPath::from(format!("{}{kind}/{name}", self.prefix))
    }

    pub fn chunk_key(&self, h: &Hash) -> ObjPath {
        // chunks/<hh>/<hex>.zst — zstd wire format.
        let hex = h.to_hex().to_string();
        self.key("chunks", &format!("{}/{}.zst", &hex[0..2], hex))
    }

    pub fn manifest_key(&self, layer_id: &crate::manifest::LayerId) -> ObjPath {
        self.key("layers", &format!("{layer_id}.manifest"))
    }

    async fn put_retry(&self, key: &ObjPath, data: Vec<u8>) -> Result<()> {
        const ATTEMPTS: u32 = 5;
        let mut delay = Duration::from_millis(200);
        let mut last: Option<Error> = None;
        for _ in 0..ATTEMPTS {
            self.puts.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            match self.store.put(key, data.clone().into()).await {
                Ok(_) => return Ok(()),
                Err(e) => {
                    last = Some(e.into());
                    tokio::time::sleep(delay).await;
                    delay *= 2;
                }
            }
        }
        Err(last.unwrap_or_else(|| Error::S3("put failed".into())))
    }

    /// Raw object write at `key` (relative to the client prefix) —
    /// snapshot artifacts (head.json, vmstate) are not chunks.
    pub async fn put_object(&self, key: &str, data: Vec<u8>) -> Result<()> {
        let n = data.len() as u64;
        self.put_retry(&ObjPath::from(format!("{}{}", self.prefix, key)), data)
            .await?;
        self.bytes_up
            .fetch_add(n, std::sync::atomic::Ordering::Relaxed);
        Ok(())
    }

    /// Whether an object exists at `key` (relative to the client prefix).
    pub async fn has_object(&self, key: &str) -> bool {
        self.store
            .head(&ObjPath::from(format!("{}{}", self.prefix, key)))
            .await
            .is_ok()
    }

    /// `get_object`, with None where there is no object.
    pub async fn find_object(&self, key: &str) -> Result<Option<Vec<u8>>> {
        match self.get_object(key).await {
            Ok(buf) => Ok(Some(buf)),
            Err(_) if !self.has_object(key).await => Ok(None),
            Err(e) => Err(e),
        }
    }

    /// Raw object read at `key` (relative to the client prefix).
    pub async fn get_object(&self, key: &str) -> Result<Vec<u8>> {
        let r = self
            .store
            .get(&ObjPath::from(format!("{}{}", self.prefix, key)))
            .await?;
        let buf = r.bytes().await?;
        self.gets.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        self.bytes_down
            .fetch_add(buf.len() as u64, std::sync::atomic::Ordering::Relaxed);
        Ok(buf.to_vec())
    }

    /// Upload a raw chunk as zstd. Caller guarantees ordering (chunks
    /// before manifest).
    #[instrument(skip(self, raw), fields(len = raw.len()))]
    pub async fn put_chunk(&self, h: &Hash, raw: &[u8]) -> Result<()> {
        let wire = chunk::compress(raw)?;
        let n = wire.len() as u64;
        self.put_retry(&self.chunk_key(h), wire).await?;
        self.bytes_up
            .fetch_add(n, std::sync::atomic::Ordering::Relaxed);
        Ok(())
    }

    /// Download + decompress + verify a chunk. `expected_raw_len` sanity-
    /// checks the manifest.
    pub async fn get_chunk(&self, h: &Hash, expected_raw_len: u32) -> Result<Vec<u8>> {
        let r = self.store.get(&self.chunk_key(h)).await?;
        let wire = r.bytes().await?;
        self.gets.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        self.bytes_down
            .fetch_add(wire.len() as u64, std::sync::atomic::Ordering::Relaxed);
        let raw = chunk::decompress(&wire)?;
        if chunk::hash(&raw) != *h {
            return Err(Error::ChunkCorrupt(h.to_hex().to_string()));
        }
        if raw.len() != expected_raw_len as usize {
            return Err(Error::ChunkCorrupt(format!(
                "{}: raw len {} != manifest {}",
                h.to_hex(),
                raw.len(),
                expected_raw_len
            )));
        }
        Ok(raw)
    }

    pub async fn has_manifest(&self, id: &crate::manifest::LayerId) -> bool {
        self.store.head(&self.manifest_key(id)).await.is_ok()
    }

    pub async fn has_chunk(&self, h: &Hash) -> bool {
        self.store.head(&self.chunk_key(h)).await.is_ok()
    }

    /// Upload a layer manifest. MUST be called only after every chunk it
    /// references is uploaded (the commit rule, ADR 0011).
    pub async fn put_manifest(
        &self,
        id: &crate::manifest::LayerId,
        m: &crate::manifest::Manifest,
    ) -> Result<()> {
        self.put_retry(&self.manifest_key(id), m.encode()).await
    }

    pub async fn get_manifest(
        &self,
        id: &crate::manifest::LayerId,
    ) -> Result<crate::manifest::Manifest> {
        let r = self.store.get(&self.manifest_key(id)).await?;
        let buf = r.bytes().await?;
        self.gets.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        self.bytes_down
            .fetch_add(buf.len() as u64, std::sync::atomic::Ordering::Relaxed);
        crate::manifest::Manifest::decode(&buf)
    }

    /// Delete one chunk object (test cleanup; prod never deletes).
    pub async fn delete_chunk(&self, h: &Hash) -> Result<()> {
        self.store.delete(&self.chunk_key(h)).await?;
        Ok(())
    }

    /// Delete an object by its plain key relative to the prefix (test
    /// cleanup of snapshot artifacts).
    pub async fn delete_object_key(&self, key: &str) -> Result<()> {
        self.store
            .delete(&ObjPath::from(format!("{}{}", self.prefix, key)))
            .await?;
        Ok(())
    }

    /// Delete a raw object key (test cleanup).
    pub async fn delete_object(&self, key: &ObjPath) -> Result<()> {
        self.store.delete(key).await?;
        Ok(())
    }

    /// List every object under `sub` (a key prefix relative to this
    /// client's own prefix, e.g. "snapshots/", "layers/", "chunks/").
    /// Keys come back relative to the client prefix so they compose with
    /// `get_object`/`delete_object_key`. Listing can be slow, so one sweep
    /// walks each prefix exactly once.
    pub async fn list(&self, sub: &str) -> Result<Vec<RemoteObject>> {
        let full = format!("{}{}", self.prefix, sub);
        // ObjPath normalises away a trailing slash; listing with the
        // directory path is the prefix semantics object_store gives us.
        let prefix = ObjPath::from(full.trim_end_matches('/').to_string());
        let mut stream = self.store.list(Some(&prefix));
        let mut out = Vec::new();
        while let Some(meta) = stream.next().await {
            let meta = meta?;
            let key = meta.location.as_ref().to_string();
            let Some(rel) = key.strip_prefix(self.prefix.as_str()) else {
                continue; // belongs to a sibling prefix; never ours to touch
            };
            out.push(RemoteObject {
                key: rel.to_string(),
                size: meta.size,
                modified_unix: meta.last_modified.timestamp(),
            });
        }
        Ok(out)
    }

    /// Delete everything under this client's prefix (test cleanup).
    pub async fn delete_prefix(&self) -> Result<()> {
        let prefix = ObjPath::from(self.prefix.trim_end_matches('/').to_string());
        let mut stream = self.store.list(Some(&prefix));
        let mut keys = Vec::new();
        while let Some(meta) = stream.next().await {
            keys.push(meta?.location);
        }
        for k in keys {
            self.store.delete(&k).await?;
        }
        Ok(())
    }
}

/// Fetch every chunk of `chain` that's missing locally (chunks-before-
/// manifest is an upload rule; download order is free). Bounded
/// parallelism (`concurrency`, ≥ 1): a single stream is far too slow to
/// hibernate a pod. Returns bytes fetched.
pub async fn hydrate_chain(
    chain: &[&crate::manifest::Manifest],
    cache: &crate::cache::Cache,
    s3: &S3,
    concurrency: usize,
) -> Result<u64> {
    let t = Instant::now();
    let mut want: Vec<(Hash, u32)> = Vec::new();
    let mut seen = std::collections::HashSet::new();
    for m in chain {
        for e in &m.entries {
            if e.kind != crate::manifest::EntryKind::Data {
                continue;
            }
            let h = Hash::from_bytes(e.hash);
            if seen.insert(h) && !cache.contains(&h) {
                want.push((h, e.raw_len));
            }
        }
    }
    let fetched = std::sync::atomic::AtomicU64::new(0);
    let mut stream = futures::stream::iter(want)
        .map(|(h, raw_len)| async move {
            let raw = s3.get_chunk(&h, raw_len).await?;
            cache.admit(&h, &raw)?;
            Result::<u64>::Ok(raw.len() as u64)
        })
        .buffer_unordered(concurrency.max(1));
    while let Some(r) = stream.next().await {
        fetched.fetch_add(r?, std::sync::atomic::Ordering::Relaxed);
    }
    let fetched = fetched.load(std::sync::atomic::Ordering::Relaxed);
    tracing::debug!(
        fetched,
        ms = t.elapsed().as_millis() as u64,
        "hydrate_chain"
    );
    Ok(fetched)
}

#[cfg(test)]
mod tests {
    #[test]
    fn a_store_is_named_by_its_endpoint_and_bucket() {
        assert_eq!(
            super::store_id("https://s3.us-east-1.amazonaws.com", "fecode-dev"),
            "797355f275ba0e52973553f7031adfec238e65a21d3478a401622fefd3ec01c9"
        );
    }
}
