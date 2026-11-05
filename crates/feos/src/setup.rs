// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use anyhow::Result;
use container_service::{
    api::ContainerApiHandler, dispatcher::Dispatcher as ContainerDispatcher,
    Command as ContainerCommand,
};
use feos_proto::{
    container_service::container_service_server::ContainerServiceServer,
    host_service::host_service_server::HostServiceServer,
    image_service::image_service_server::ImageServiceServer,
};
#[cfg(feature = "guest")]
use feos_utils::filesystem::mount_virtual_filesystems;
use host_service::{
    api::HostApiHandler, dispatcher::HostServiceDispatcher, Command as HostCommand, RestartSignal,
};
use image_service::{
    api::ImageApiHandler, dispatcher::ImageServiceDispatcher, filestore::FileStore,
    worker::Orchestrator, IMAGE_DIR,
};
use log::{error, info};
#[cfg(feature = "host")]
use microvm_service::{
    api::VmApiHandler, dispatcher::VmServiceDispatcher, Command as VmCommand, DEFAULT_VM_DB_URL,
    VM_DIR,
};
use nix::libc;
#[cfg(feature = "host")]
use std::env;
use std::ffi::CString;
use std::os::unix::ffi::OsStringExt;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use tokio::fs;
#[cfg(feature = "host")]
use tokio::fs::File;
use tokio::sync::mpsc;

#[cfg(feature = "host")]
pub(crate) async fn initialize_vm_service(db_url: &str) -> Result<VmApiHandler> {
    info!("Main: Ensuring VM directory '{VM_DIR}' exists...");
    fs::create_dir_all(VM_DIR).await?;
    info!("Main: Directory check complete. Path '{VM_DIR}' is ready.");

    let (vm_tx, vm_rx) = mpsc::channel::<VmCommand>(32);
    let vm_dispatcher = VmServiceDispatcher::new(vm_rx, db_url).await?;
    tokio::spawn(async move {
        vm_dispatcher.run().await;
    });
    let vm_api_handler = VmApiHandler::new(vm_tx);
    info!("Main: VM Service is configured.");

    Ok(vm_api_handler)
}

/// The pods, on top of the VM service, their disks from diskd, their
/// volumes, and their network: its security groups and the records of its
/// traffic, and the resolver on the sockets handed to `feosd`, if any.
#[cfg(feature = "host")]
pub(crate) async fn initialize_pod_service(
    vms: VmApiHandler,
    network: pod_service::network::Config,
    dns_sockets: Vec<(std::os::fd::OwnedFd, String)>,
    diskd_socket: Option<std::path::PathBuf>,
    authn: pod_service::authn::Config,
) -> Result<(
    pod_service::Pods,
    pod_service::Volumes,
    network_service::SecurityGroups,
    network_service::NameTables,
    traffic_service::TrafficApi,
)> {
    let groups = network_service::SecurityGroups::open(network_service::SECURITY_GROUP_DIR)?;
    let names = network_service::NameTables::open(network_service::NAME_TABLE_DIR)?;
    // Flows, drops and server names, and the resolver's queries, for
    // TrafficService.
    let (records, _) = tokio::sync::broadcast::channel(pod_service::network::RECORDS);
    let dir = Path::new(pod_service::POD_DIR);
    let network = pod_service::network::Network::start(
        network,
        dns_sockets,
        dir,
        groups.clone(),
        names.clone(),
        records.clone(),
    )
    .await;
    let disks = pod_service::disk::Disks::new(
        diskd_socket.unwrap_or_else(|| pod_service::disk::DISKD_SOCKET.into()),
    );
    info!(
        "Main: The pods' disks come from diskd on {}",
        disks.socket().display()
    );
    let volumes = pod_service::Volumes::open(pod_service::volumes::VOLUME_DIR, disks.clone())?;
    let pods = pod_service::Pods::new(vms, dir, network, disks, volumes.clone(), authn).await?;
    info!("Main: Pod Service is configured.");
    Ok((
        pods,
        volumes,
        groups,
        names,
        traffic_service::TrafficApi::new(records),
    ))
}

pub(crate) async fn initialize_container_service(
) -> Result<ContainerServiceServer<ContainerApiHandler>> {
    info!("Main: Initializing Container Service...");

    let (container_tx, container_rx) = mpsc::channel::<ContainerCommand>(32);
    let container_dispatcher = ContainerDispatcher::new(container_rx).await?;
    tokio::spawn(async move {
        container_dispatcher.run().await;
    });
    let container_api_handler = ContainerApiHandler::new(container_tx);
    let container_service = ContainerServiceServer::new(container_api_handler);
    info!("Main: Container Service is configured.");

    Ok(container_service)
}

pub(crate) fn initialize_host_service(
    restart_tx: Option<mpsc::Sender<RestartSignal>>,
    log_handle: feos_utils::feos_logger::LogHandle,
) -> HostServiceServer<HostApiHandler> {
    let (host_tx, host_rx) = mpsc::channel::<HostCommand>(32);
    let host_dispatcher = HostServiceDispatcher::new(host_rx, restart_tx, log_handle);
    tokio::spawn(async move {
        host_dispatcher.run().await;
    });

    let host_api_handler = HostApiHandler::new(host_tx);
    let host_service = HostServiceServer::new(host_api_handler);
    info!("Main: Host Service is configured.");

    host_service
}

pub(crate) async fn initialize_image_service() -> Result<ImageServiceServer<ImageApiHandler>> {
    info!("Main: Ensuring image directory '{IMAGE_DIR}' exists...");
    fs::create_dir_all(IMAGE_DIR).await?;
    info!("Main: Directory check complete. Path '{IMAGE_DIR}' is ready.");

    let filestore_actor = FileStore::new();
    let filestore_tx = filestore_actor.get_command_sender();
    tokio::spawn(async move {
        filestore_actor.run().await;
    });
    info!("Main: FileStore actor for Image Service has been started.");

    let orchestrator_actor = Orchestrator::new(filestore_tx);
    let orchestrator_tx = orchestrator_actor.get_command_sender();
    tokio::spawn(async move {
        orchestrator_actor.run().await;
    });
    info!("Main: Orchestrator actor for Image Service has been started.");

    let grpc_dispatcher = ImageServiceDispatcher::new(orchestrator_tx);
    let grpc_dispatcher_tx = grpc_dispatcher.get_command_sender();
    tokio::spawn(async move {
        grpc_dispatcher.run().await;
    });
    info!("Main: gRPC Dispatcher for Image Service has been started.");

    let image_api_handler = ImageApiHandler::new(grpc_dispatcher_tx);
    let image_service = ImageServiceServer::new(image_api_handler);
    info!("Main: Image Service is configured.");

    Ok(image_service)
}

/// The guest's setup as PID 1 of its VM.
#[cfg(feature = "guest")]
pub(crate) fn perform_first_boot_initialization() {
    info!("Main: Performing first-boot initialization...");
    info!("Main: Mounting virtual filesystems...");
    mount_virtual_filesystems();

    #[cfg(feature = "guest")]
    crate::guest::initialize_guest();
}

#[cfg(feature = "host")]
pub(crate) async fn setup_database() -> Result<String> {
    let db_url = env::var("DATABASE_URL").unwrap_or_else(|_| {
        info!("Main: DATABASE_URL not set, using default '{DEFAULT_VM_DB_URL}'");
        DEFAULT_VM_DB_URL.to_string()
    });

    if let Some(db_path_str) = db_url.strip_prefix("sqlite:") {
        let db_path = Path::new(db_path_str);
        if let Some(db_dir) = db_path.parent() {
            info!(
                "Main: Ensuring database directory '{}' exists...",
                db_dir.display()
            );
            fs::create_dir_all(db_dir).await?;
        }
        if !db_path.exists() {
            info!(
                "Main: Database file does not exist, creating at '{}'...",
                db_path.display()
            );
            File::create(db_path).await?;
        }
    }

    Ok(db_url)
}

pub(crate) fn handle_upgrade(new_binary_path: &Path) -> Result<()> {
    info!("Main: Upgrade signal received. New binary at {new_binary_path:?}. Preparing to execv.");

    let current_exe = match std::env::current_exe() {
        Ok(path) => path,
        Err(e) => {
            // Using panic here as not knowing the current exe is a fatal state.
            panic!("FATAL: Could not get current executable path: {e}");
        }
    };
    info!("Main: Current binary is at {current_exe:?}");

    let rename_result = std::fs::rename(new_binary_path, &current_exe);

    match rename_result {
        Ok(_) => {
            info!("Main: Successfully replaced on-disk binary via atomic rename.");
        }
        Err(e) if e.raw_os_error() == Some(libc::EXDEV) => {
            info!("Main: Cross-device link detected. Falling back to copy-then-rename strategy.");
            let staging_path = current_exe.with_extension("staging");
            if let Err(copy_err) = std::fs::copy(new_binary_path, &staging_path) {
                error!(
                    "CRITICAL: Failed to copy new binary to staging path {staging_path:?}: {copy_err}. Aborting upgrade."
                );
                return Ok(());
            }
            if let Err(perm_err) =
                std::fs::set_permissions(&staging_path, std::fs::Permissions::from_mode(0o755))
            {
                error!(
                    "CRITICAL: Failed to set permissions on staged binary {staging_path:?}: {perm_err}. Aborting upgrade."
                );
                let _ = std::fs::remove_file(&staging_path);
                return Ok(());
            }
            if let Err(final_rename_err) = std::fs::rename(&staging_path, &current_exe) {
                error!(
                    "CRITICAL: Failed to perform final atomic rename from {staging_path:?}: {final_rename_err}. Aborting upgrade."
                );
                let _ = std::fs::remove_file(&staging_path);
                return Ok(());
            }
            let _ = std::fs::remove_file(new_binary_path);
            info!("Main: Successfully replaced on-disk binary via copy-then-rename.");
        }
        Err(e) => {
            error!(
                "CRITICAL: Failed to rename new binary into place with an unexpected error: {e}. Aborting upgrade."
            );
            return Ok(());
        }
    }

    let mut args: Vec<String> = std::env::args().collect();
    let restart_flag = "--restarted-after-upgrade";
    if !args.contains(&restart_flag.to_string()) {
        args.push(restart_flag.to_string());
    }

    let cstr_args: Vec<CString> = args
        .into_iter()
        .map(|arg| CString::new(arg).unwrap())
        .collect();
    let cstr_path = CString::new(current_exe.into_os_string().into_vec()).unwrap();

    info!("Main: Executing new binary with arguments: {cstr_args:?}");
    // The logger writes from a task of its own, which the exec ends.
    log::logger().flush();
    // The new binary gets the resolver's sockets, which pods' queries wait
    // in meanwhile (ADR 0010).
    let e = feos_utils::listen_fds::exec(&cstr_path, &cstr_args);
    // The exec replaces the current process, so it should not return.
    // If it returns, it's a fatal error.
    panic!("FATAL: execv failed after replacing binary: {e}");
}
