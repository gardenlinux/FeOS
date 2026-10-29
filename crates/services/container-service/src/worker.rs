// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::{
    dispatcher::{runtime_knows, validate_container_id},
    error::ContainerServiceError,
    logs,
    persistence::{repository::ContainerRepository, ContainerRecord, ContainerStatus},
    runtime::{
        adapter::ContainerAdapter,
        bundle::{self, Process},
    },
};
use feos_proto::{
    container_service::{
        ContainerConfig, ContainerState, CreateContainerRequest, CreateContainerResponse,
        DeleteContainerRequest, DeleteContainerResponse, ExecRequest, LogEntry,
        StartContainerRequest, StartContainerResponse, StopContainerRequest, StopContainerResponse,
        StreamContainerLogsRequest,
    },
    image_service::{
        image_service_client::ImageServiceClient, ImageState as OciImageState, PullImageRequest,
        WatchImageStatusRequest,
    },
    task_service::ExecFrame,
};
use hyper_util::rt::TokioIo;
use image_service::{PullToken, IMAGE_SERVICE_SOCKET};
use log::{error, info, warn};
use std::{
    path::{Path, PathBuf},
    sync::Arc,
};
use tokio::sync::{mpsc, oneshot};
use tokio_stream::StreamExt;
use tonic::{
    transport::{Channel, Endpoint, Uri},
    Status, Streaming,
};
use tower::service_fn;
use uuid::Uuid;

/// The log entries a stream holds while its caller reads slower.
const LOG_STREAM_BUFFER: usize = 16;

async fn get_image_service_client() -> Result<ImageServiceClient<Channel>, ContainerServiceError> {
    let socket_path = PathBuf::from(IMAGE_SERVICE_SOCKET);
    Endpoint::try_from("http://[::1]:50051")
        .unwrap()
        .connect_with_connector(service_fn(move |_: Uri| {
            let socket_path = socket_path.clone();
            async move {
                tokio::net::UnixStream::connect(socket_path)
                    .await
                    .map(TokioIo::new)
            }
        }))
        .await
        .map(ImageServiceClient::new)
        .map_err(|e| ContainerServiceError::ImageService(e.to_string()))
}

/// Pulls the image, or joins the pull or the image that is there, and waits
/// until it is ready. Returns its UUID.
/// `token` is the create's pull token (ADR 0025), for a pull it starts.
async fn pull_image(image_ref: &str, token: &PullToken) -> Result<Uuid, ContainerServiceError> {
    info!("ContainerWorker: Requesting image {image_ref}");
    let mut client = get_image_service_client().await?;
    let image_uuid = client
        .pull_image(PullImageRequest {
            image_ref: image_ref.to_string(),
            exclusive: false,
            pull_token: token.secret().to_string(),
        })
        .await
        .map_err(|status| {
            ContainerServiceError::ImageService(format!(
                "PullImage RPC failed for {image_ref}: {status}"
            ))
        })?
        .into_inner()
        .image_uuid;
    let parsed = Uuid::parse_str(&image_uuid)
        .map_err(|e| ContainerServiceError::ImageService(format!("Invalid image UUID: {e}")))?;
    wait_for_image_ready(&mut client, &image_uuid, image_ref).await?;
    Ok(parsed)
}

async fn wait_for_image_ready(
    client: &mut ImageServiceClient<Channel>,
    image_uuid: &str,
    image_ref: &str,
) -> Result<(), ContainerServiceError> {
    let mut stream = client
        .watch_image_status(WatchImageStatusRequest {
            image_uuid: image_uuid.to_string(),
        })
        .await
        .map_err(|e| {
            ContainerServiceError::ImageService(format!(
                "WatchImageStatus RPC failed for {image_uuid}: {e}"
            ))
        })?
        .into_inner();

    while let Some(status_res) = stream.next().await {
        let status = status_res.map_err(|e| {
            ContainerServiceError::ImageService(format!("Image stream error for {image_uuid}: {e}"))
        })?;
        let state = OciImageState::try_from(status.state).unwrap_or(OciImageState::Unspecified);
        match state {
            OciImageState::Ready => return Ok(()),
            OciImageState::PullFailed | OciImageState::NotFound => {
                return Err(ContainerServiceError::ImageService(format!(
                    "Image pull failed for {image_ref} (uuid: {image_uuid}): {}",
                    status.message
                )))
            }
            _ => continue,
        }
    }
    Err(ContainerServiceError::ImageService(format!(
        "Image watch stream for {image_uuid} ended before reaching a terminal state."
    )))
}

fn image_dir(image_uuid: Uuid) -> PathBuf {
    Path::new(image_service::IMAGE_DIR).join(image_uuid.to_string())
}

/// Runs `f` on the blocking pool: mounts and file writes block.
async fn blocking<T: Send + 'static>(
    f: impl FnOnce() -> Result<T, String> + Send + 'static,
) -> Result<T, ContainerServiceError> {
    tokio::task::spawn_blocking(f)
        .await
        .map_err(|e| ContainerServiceError::Adapter(format!("task: {e}")))?
        .map_err(ContainerServiceError::Adapter)
}

/// Creates or adopts a container and returns when it is created.
pub async fn create_container(
    req: CreateContainerRequest,
    token: &PullToken,
    repository: &ContainerRepository,
    adapter: &ContainerAdapter,
) -> Result<CreateContainerResponse, ContainerServiceError> {
    let container_id = match req.container_id.as_deref().filter(|s| !s.is_empty()) {
        Some(id) => id.to_string(),
        None => Uuid::new_v4().to_string(),
    };
    validate_container_id(&container_id)?;
    let existing = repository.get_container(&container_id).await?;
    let live = existing.as_ref().is_some_and(|rec| {
        matches!(
            rec.status.state,
            ContainerState::PullingImage | ContainerState::Created | ContainerState::Running
        ) && (rec.status.state == ContainerState::PullingImage || runtime_knows(&container_id))
    });

    if req.adopt && bundle::adoptable(&container_id) && !live {
        return adopt_container(
            container_id,
            req.config,
            existing,
            token,
            repository,
            adapter,
        )
        .await;
    }
    if existing.is_some() || bundle::container_dir(&container_id).exists() {
        return Err(ContainerServiceError::AlreadyExists(container_id));
    }

    let config = req.config.ok_or_else(|| {
        ContainerServiceError::InvalidArgument("ContainerConfig is required".to_string())
    })?;
    if config.image_ref.is_empty() {
        return Err(ContainerServiceError::InvalidArgument(
            "image_ref is required".to_string(),
        ));
    }
    // The volumes it mounts are mounted, before its image is pulled.
    let mounts = config.volume_mounts.clone();
    tokio::task::spawn_blocking(move || bundle::volume_mounts(&mounts))
        .await
        .map_err(|e| ContainerServiceError::Adapter(format!("task: {e}")))?
        .map_err(ContainerServiceError::InvalidState)?;

    // The record claims the ID while the image is pulled.
    let mut record = ContainerRecord {
        container_id: container_id.clone(),
        image_uuid: Uuid::nil(),
        status: ContainerStatus {
            state: ContainerState::PullingImage,
            process_id: None,
            exit_code: None,
        },
        config: config.clone(),
    };
    repository.save_container(&record).await?;

    let result = async {
        record.image_uuid = pull_image(&config.image_ref, token).await?;
        let image_dir = image_dir(record.image_uuid);
        let id = container_id.clone();
        let (command, env) = (config.command.clone(), config.env.clone());
        let mounts = config.volume_mounts.clone();
        blocking(move || {
            let process = bundle::process(&image_dir, &command, &env)?;
            let volumes = bundle::volume_mounts(&mounts)?;
            let spec = bundle::runtime_spec(&id, &process, &volumes);
            bundle::provision(&id, &image_dir, &spec, false)
        })
        .await?;
        create_in_runtime(&mut record, repository, adapter).await
    }
    .await;

    if let Err(e) = &result {
        error!("ContainerWorker ({container_id}): Creation failed: {e}");
        let removed = container_id.clone();
        let _ = blocking(move || bundle::remove(&removed)).await;
        if let Err(e) = repository.delete_container(&container_id).await {
            warn!("Failed to clean up the record of {container_id}: {e}");
        }
    }
    result.map(|()| CreateContainerResponse {
        container_id,
        resolver_error: String::new(),
    })
}

/// Adopts the container directory a reboot left: its writable layer and
/// logs stay, its mounts, spec and runtime state are built anew.
async fn adopt_container(
    container_id: String,
    config: Option<ContainerConfig>,
    existing: Option<ContainerRecord>,
    token: &PullToken,
    repository: &ContainerRepository,
    adapter: &ContainerAdapter,
) -> Result<CreateContainerResponse, ContainerServiceError> {
    info!("ContainerWorker ({container_id}): Adopting the container of a previous boot");
    let config = config.unwrap_or_default();
    let mut merged = existing
        .as_ref()
        .map(|rec| rec.config.clone())
        .unwrap_or_default();
    if !config.image_ref.is_empty() {
        merged.image_ref = config.image_ref;
    }
    if !config.command.is_empty() {
        merged.command = config.command.clone();
    }
    merged.env.extend(config.env.clone());
    if !config.volume_mounts.is_empty() {
        merged.volume_mounts = config.volume_mounts.clone();
    }
    if merged.image_ref.is_empty() {
        return Err(ContainerServiceError::InvalidArgument(format!(
            "no image known for container {container_id}; set config.image_ref"
        )));
    }

    let image_uuid = pull_image(&merged.image_ref, token).await?;
    let image_dir = image_dir(image_uuid);
    let id = container_id.clone();
    let mounts = merged.volume_mounts.clone();
    let resolver_error = blocking(move || {
        // The spec it ran with: its command and environment, with the
        // request's on top.
        let process = match bundle::previous_process(&id) {
            Some((args, env)) => {
                let image = bundle::process(&image_dir, &args, &Default::default())?;
                let mut previous = Process {
                    args: if config.command.is_empty() {
                        args
                    } else {
                        config.command.clone()
                    },
                    env,
                    cwd: image.cwd,
                };
                let mut overrides: Vec<_> = config.env.iter().collect();
                overrides.sort();
                for (key, value) in overrides {
                    previous.env.retain(|v| !v.starts_with(&format!("{key}=")));
                    previous.env.push(format!("{key}={value}"));
                }
                previous
            }
            None => bundle::process(&image_dir, &config.command, &config.env)?,
        };
        let volumes = bundle::volume_mounts(&mounts)?;
        let spec = bundle::runtime_spec(&id, &process, &volumes);
        bundle::provision(&id, &image_dir, &spec, true)
    })
    .await?;

    let mut record = ContainerRecord {
        container_id: container_id.clone(),
        image_uuid,
        status: ContainerStatus {
            state: ContainerState::Created,
            process_id: None,
            exit_code: None,
        },
        config: merged,
    };
    if let Err(e) = create_in_runtime(&mut record, repository, adapter).await {
        bundle::unmount(&container_id);
        return Err(e);
    }
    Ok(CreateContainerResponse {
        container_id,
        resolver_error,
    })
}

/// Creates the container in the runtime and records it as CREATED.
async fn create_in_runtime(
    record: &mut ContainerRecord,
    repository: &ContainerRepository,
    adapter: &ContainerAdapter,
) -> Result<(), ContainerServiceError> {
    let id = record.container_id.clone();
    let pid = adapter
        .create_container(&id, &bundle::bundle_dir(&id), &bundle::log_file(&id))
        .await
        .map_err(|e| ContainerServiceError::Adapter(format!("create {id}: {e}")))?;
    info!("ContainerWorker ({id}): Created, init pid {pid}");
    record.status = ContainerStatus {
        state: ContainerState::Created,
        process_id: Some(pid),
        exit_code: None,
    };
    repository.save_container(record).await?;
    Ok(())
}

pub async fn handle_start_container(
    req: StartContainerRequest,
    responder: oneshot::Sender<Result<StartContainerResponse, ContainerServiceError>>,
    repository: ContainerRepository,
    adapter: Arc<ContainerAdapter>,
) {
    let id = req.container_id.clone();
    let result = adapter.start_container(&id).await;

    match result {
        Ok(_) => {
            info!("Worker: Start command sent for container {id}");
            let pid = match repository.record_start(&id).await {
                Ok(pid) => pid,
                Err(e) => {
                    let err = ContainerServiceError::Persistence(e);
                    error!("Worker: {err}");
                    let _ = responder.send(Err(err));
                    return;
                }
            };
            let _ = responder.send(Ok(StartContainerResponse {}));
            tokio::spawn(track_exit(id, pid, repository, adapter));
        }
        Err(e) => {
            let err = ContainerServiceError::Adapter(e.to_string());
            error!("Worker: {err}");
            let _ = responder.send(Err(err));
        }
    }
}

/// Records the container as STOPPED, with its exit code, once its process
/// `pid` exits. The task service maps a signal to 128 + the signal.
async fn track_exit(
    id: String,
    pid: Option<i64>,
    repository: ContainerRepository,
    adapter: Arc<ContainerAdapter>,
) {
    match adapter.wait_container(&id).await {
        Ok(exit_code) => {
            info!("Worker: Container {id} exited with code {exit_code}");
            if let Err(e) = repository.record_exit(&id, pid, exit_code).await {
                warn!("Worker: Failed to record that container {id} stopped: {e}");
            }
        }
        Err(e) => warn!("Worker: Lost track of container {id}: {e}"),
    }
}

pub async fn handle_stop_container(
    req: StopContainerRequest,
    responder: oneshot::Sender<Result<StopContainerResponse, ContainerServiceError>>,
    repository: ContainerRepository,
    adapter: Arc<ContainerAdapter>,
) {
    let id = req.container_id.clone();
    let signal = req.signal.unwrap_or(9);
    let result = adapter.stop_container(&id, signal).await;

    match result {
        Ok(_) => {
            info!("Worker: Stop command sent for container {id}");
            // Keeps the exit code, which track_exit may have recorded.
            if let Err(e) = repository
                .update_container_state(&id, ContainerState::Stopped)
                .await
            {
                let err = ContainerServiceError::Persistence(e);
                error!("Worker: {err}");
                let _ = responder.send(Err(err));
                return;
            }
            let _ = responder.send(Ok(StopContainerResponse {}));
        }
        Err(e) => {
            let err = ContainerServiceError::Adapter(e.to_string());
            error!("Worker: {err}");
            let _ = responder.send(Err(err));
        }
    }
}

pub async fn handle_delete_container(
    req: DeleteContainerRequest,
    responder: oneshot::Sender<Result<DeleteContainerResponse, ContainerServiceError>>,
    repository: ContainerRepository,
    adapter: Arc<ContainerAdapter>,
) {
    let id = req.container_id.clone();
    let result = async {
        adapter
            .delete_container(&id)
            .await
            .map_err(|e| ContainerServiceError::Adapter(e.to_string()))?;
        let remove_id = id.clone();
        blocking(move || bundle::remove(&remove_id)).await?;
        repository.delete_container(&id).await?;
        Ok(DeleteContainerResponse {})
    }
    .await;
    match &result {
        Ok(_) => info!("Worker: Deleted container {id}"),
        Err(e) => error!("Worker: Failed to delete container {id}: {e}"),
    }
    let _ = responder.send(result);
}

/// Opens the container's log where the request starts it and streams it
/// from there (crate::logs).
pub async fn handle_stream_logs(
    req: StreamContainerLogsRequest,
    responder: oneshot::Sender<
        Result<mpsc::Receiver<Result<LogEntry, Status>>, ContainerServiceError>,
    >,
    repository: ContainerRepository,
) {
    let id = req.container_id.clone();
    let path = bundle::log_file(&id);
    let open_path = path.clone();
    let (tail_lines, tail) = (req.tail_lines, req.tail);
    let opened = blocking(move || {
        logs::open(&open_path, tail_lines, tail)
            .map_err(|e| format!("{}: {e}", open_path.display()))
    })
    .await;
    match opened {
        Ok(opened) => {
            let (tx, rx) = mpsc::channel(LOG_STREAM_BUFFER);
            let _ = responder.send(Ok(rx));
            logs::stream(opened, req.follow, id, path, repository, tx).await;
        }
        Err(e) => {
            let _ = responder.send(Err(e));
        }
    }
}

pub async fn handle_exec(
    req: ExecRequest,
    responder: oneshot::Sender<Result<Streaming<ExecFrame>, ContainerServiceError>>,
    adapter: Arc<ContainerAdapter>,
) {
    let result = async {
        if req.command.is_empty() {
            return Err(ContainerServiceError::InvalidArgument(
                "empty command".to_string(),
            ));
        }
        let id = req.container_id.clone();
        let env = req.env.clone();
        let env = blocking(move || Ok(bundle::exec_env(&id, &env))).await?;
        adapter
            .exec(&req.container_id, req.command, env, req.working_dir)
            .await
            .map_err(|e| ContainerServiceError::Adapter(e.to_string()))
    }
    .await;
    let _ = responder.send(result);
}
