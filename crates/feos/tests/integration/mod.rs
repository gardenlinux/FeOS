// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use anyhow::{Context, Result};
use feos_proto::{
    container_service::container_service_client::ContainerServiceClient,
    host_service::host_service_client::HostServiceClient,
    image_service::image_service_client::ImageServiceClient,
    pod_service::pod_service_client::PodServiceClient,
};
use hyper_util::rt::TokioIo;
use image_service::IMAGE_SERVICE_SOCKET;
use log::{error, info};
use microvm_service::vmm::installed::{FIRECRACKER_DIR, GUEST_DIR};
use nix::sys::prctl;
use std::env;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::{LazyLock as Lazy, OnceLock as SyncOnceCell};
use std::time::Duration;
use tokio::net::UnixStream;
use tokio::sync::OnceCell as TokioOnceCell;
use tonic::transport::{Channel, Endpoint, Uri};
use tower::service_fn;

pub mod container_tests;
pub mod diskd_gate;
pub mod fixtures;
pub mod host_tests;
pub mod image_tests;
pub mod pod_bake_tests;
pub mod pod_disk_tests;
pub mod pod_tests;
pub mod pod_volume_tests;

pub const PUBLIC_SERVER_ADDRESS: &str = "http://[::1]:1337";
/// The pods' prefix: a ULA, as in labs.
pub const POD_PREFIX: &str = "fd00:fe05:1::/64";
pub const DEFAULT_TEST_CONTAINER_IMAGE_REF: &str = "docker.io/library/busybox:1.36";

/// An image in a local registry, which the image test pulls: `make
/// test-pods` pushes the guest there.
pub static TEST_IMAGE_REF: Lazy<Option<String>> =
    Lazy::new(|| env::var("TEST_IMAGE_REF").ok().filter(|r| !r.is_empty()));
pub static TEST_CONTAINER_IMAGE_REF: Lazy<String> = Lazy::new(|| {
    env::var("TEST_CONTAINER_IMAGE_REF")
        .unwrap_or_else(|_| DEFAULT_TEST_CONTAINER_IMAGE_REF.to_string())
});

static SERVER_RUNTIME: TokioOnceCell<Arc<tokio::runtime::Runtime>> = TokioOnceCell::const_new();
static TEMP_DIR_GUARD: SyncOnceCell<tempfile::TempDir> = SyncOnceCell::new();

/// The tests that use the object store: one that counts diskd's uploads
/// takes it alone, the others share it.
pub static OBJECT_STORE: tokio::sync::RwLock<()> = tokio::sync::RwLock::const_new(());

pub async fn ensure_server() {
    SERVER_RUNTIME
        .get_or_init(|| async { setup_server().await })
        .await;
}

async fn setup_server() -> Arc<tokio::runtime::Runtime> {
    let temp_dir = TEMP_DIR_GUARD.get_or_init(|| {
        tempfile::Builder::new()
            .prefix("feos-test-")
            .tempdir()
            .expect("Failed to create temp dir")
    });

    let db_path = temp_dir.path().join("vms.db");
    let db_url = format!("sqlite:{}", db_path.to_str().unwrap());

    // SAFETY: not strictly sound: the test harness runs tests on several
    // threads, and another thread may read the environment concurrently.
    // TODO: pass the database URLs to run_server instead of the environment.
    unsafe {
        env::set_var("DATABASE_URL", &db_url);
        // feosd runs inside this test binary; the task service needs the real one.
        env::set_var("FEOS_TASK_SERVICE_BINARY", env!("CARGO_BIN_EXE_feosd"));
    }
    info!("Using temporary database for tests: {db_url}");

    if let Err(e) = prctl::set_child_subreaper(true) {
        log::error!("Failed to set as child subreaper, container tests will fail: {e}");
    }

    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("Failed to create a new Tokio runtime for the server");

    // feosd reaches diskd through the tests' gate.
    let gate = temp_dir.path().join("diskd-gate.sock");
    {
        let _runtime = runtime.enter();
        diskd_gate::serve(&gate).expect("Failed to serve the diskd gate");
    }

    runtime.spawn(async move {
        let options = main_server::ServerOptions {
            tcp: Some("[::]:1337".parse().expect("an address")),
            network: pod_service::network::Config {
                pod_prefix: pod_service::network::parse_pod_prefix(POD_PREFIX).ok(),
                ..Default::default()
            },
            diskd_socket: Some(gate),
            ..Default::default()
        };
        if let Err(e) = main_server::run_server(options).await {
            panic!("Test server failed to run: {e}");
        }
    });

    info!("Waiting for the server to start...");
    for _ in 0..60 {
        if Channel::from_static(PUBLIC_SERVER_ADDRESS)
            .connect()
            .await
            .is_ok()
        {
            info!("Server is up and running at {PUBLIC_SERVER_ADDRESS}");
            return Arc::new(runtime);
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    error!("Server did not start in time.");
    panic!("Server did not start in time.");
}

pub async fn get_public_clients() -> Result<(
    PodServiceClient<Channel>,
    HostServiceClient<Channel>,
    ContainerServiceClient<Channel>,
)> {
    let pod_client = PodServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    let host_client = HostServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    let container_client = ContainerServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    Ok((pod_client, host_client, container_client))
}

pub async fn get_image_service_client() -> Result<ImageServiceClient<Channel>> {
    let endpoint = Endpoint::from_static("http://[::1]:50051");
    let channel = endpoint
        .connect_with_connector(service_fn(|_: Uri| async {
            UnixStream::connect(IMAGE_SERVICE_SOCKET)
                .await
                .map(TokioIo::new)
        }))
        .await?;
    Ok(ImageServiceClient::new(channel))
}

/// The guest and Firecracker the pods boot, as their packages install them.
pub struct Installed {
    pub guest: PathBuf,
    pub firecracker: PathBuf,
}

/// Installs the guest (make guest) and the Firecracker in PATH (make
/// firecracker) as their packages would, or None if either is missing, or
/// no diskd serves the pods' disks (make test-pods runs one): the pod
/// tests are skipped then.
pub fn installed_guest() -> Result<Option<Installed>> {
    let repo = Path::new(env!("CARGO_MANIFEST_DIR")).join("../..");
    let kernel = repo.join("target/guest/vmlinuz");
    let initramfs = repo.join("target/guest/initramfs.cpio.zst");
    let data = repo.join("target/guest/data.img.zst");
    let firecracker = env::split_paths(&env::var_os("PATH").unwrap_or_default())
        .map(|dir| dir.join("firecracker"))
        .find(|path| path.is_file());
    let guest_built = kernel.exists() && initramfs.exists() && data.exists();
    let (Some(firecracker), true) = (firecracker, guest_built) else {
        log::warn!("Skipping test: no Firecracker in PATH, or no guest in target/guest.");
        return Ok(None);
    };
    if !Path::new(pod_service::disk::DISKD_SOCKET).exists() {
        log::warn!(
            "Skipping test: no diskd on {}.",
            pod_service::disk::DISKD_SOCKET
        );
        return Ok(None);
    }
    let guest = install(
        Path::new(GUEST_DIR),
        "0.0.1-test",
        &[
            (&kernel, "vmlinuz"),
            (&initramfs, "initramfs"),
            (&data, "data.img.zst"),
        ],
    )?;
    let firecracker = install(
        Path::new(FIRECRACKER_DIR),
        "1.17.0",
        &[
            (&firecracker, "firecracker"),
            (&firecracker.with_file_name("jailer"), "jailer"),
        ],
    )?;
    Ok(Some(Installed { guest, firecracker }))
}

/// Installs `files` as a package would, in `dir/<version>/`, by hard link
/// or copy.
fn install(dir: &Path, version: &str, files: &[(&Path, &str)]) -> Result<PathBuf> {
    let dir = dir.join(version);
    std::fs::create_dir_all(&dir).with_context(|| format!("create {}", dir.display()))?;
    for (source, name) in files {
        let target = dir.join(name);
        if !target.exists() && std::fs::hard_link(source, &target).is_err() {
            std::fs::copy(source, &target)?;
        }
    }
    Ok(dir)
}
