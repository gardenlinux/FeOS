// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::{
    error::ImageServiceError,
    pull::{normalize_ref, pull_image},
    pull_dir, FileCommand, ImageStateEvent, OrchestratorCommand, PullToken,
};
use feos_proto::image_service::{
    DeleteImageResponse, ImageInfo, ImageState, ImageStatusResponse, ListImagesResponse,
    PullImageResponse,
};
use log::{error, info, warn};
use std::collections::HashMap;
use tokio::sync::{broadcast, mpsc, oneshot};
use tonic::Status;
use uuid::Uuid;

pub struct Orchestrator {
    command_rx: mpsc::Receiver<OrchestratorCommand>,
    command_tx: mpsc::Sender<OrchestratorCommand>,
    broadcast_tx: broadcast::Sender<ImageStateEvent>,
    filestore_tx: mpsc::Sender<FileCommand>,
    store: HashMap<String, ImageInfo>,
}

impl Orchestrator {
    pub fn new(filestore_tx: mpsc::Sender<FileCommand>) -> Self {
        let (command_tx, command_rx) = mpsc::channel(32);
        let (broadcast_tx, _) = broadcast::channel(32);
        Self {
            command_rx,
            command_tx,
            broadcast_tx,
            filestore_tx,
            store: HashMap::new(),
        }
    }

    pub fn get_command_sender(&self) -> mpsc::Sender<OrchestratorCommand> {
        self.command_tx.clone()
    }

    pub async fn run(mut self) {
        let (responder, resp_rx) = oneshot::channel();
        if self
            .filestore_tx
            .send(FileCommand::ScanExistingImages { responder })
            .await
            .is_ok()
            && let Ok(initial_store) = resp_rx.await
        {
            self.store = initial_store;
        }

        info!("Orchestrator: Running and waiting for commands.");
        while let Some(cmd) = self.command_rx.recv().await {
            self.handle_command(cmd).await;
        }
        info!("Orchestrator: Channel closed, shutting down.");
    }

    async fn handle_command(&mut self, cmd: OrchestratorCommand) {
        match cmd {
            OrchestratorCommand::PullImage {
                image_ref,
                exclusive,
                pull_token,
                responder,
            } => {
                let image_ref = match normalize_ref(&image_ref) {
                    Ok(image_ref) => image_ref,
                    Err(e) => {
                        let _ = responder.send(Err(e));
                        return;
                    }
                };
                // Images are immutable: a pull of a reference that is there
                // or on its way joins it, unless either pull is exclusive.
                if !exclusive
                    && let Some(existing) = self.store.values().find(|info| {
                        !info.exclusive
                            && info.image_ref == image_ref
                            && matches!(
                                ImageState::try_from(info.state),
                                Ok(ImageState::Ready | ImageState::Downloading)
                            )
                    })
                {
                    info!(
                        "Orchestrator: '{image_ref}' is already image {}",
                        existing.image_uuid
                    );
                    let _ = responder.send(Ok(PullImageResponse {
                        image_uuid: existing.image_uuid.clone(),
                    }));
                    return;
                }

                let image_uuid = Uuid::new_v4().to_string();
                info!("Orchestrator: Start pull for '{image_ref}', assigned UUID {image_uuid}");

                self.store.insert(
                    image_uuid.clone(),
                    ImageInfo {
                        image_uuid: image_uuid.clone(),
                        image_ref: image_ref.clone(),
                        state: ImageState::Downloading as i32,
                        exclusive,
                        ..Default::default()
                    },
                );
                self.broadcast_state_change(
                    image_uuid.clone(),
                    ImageState::Downloading,
                    "Pull initiated".to_string(),
                );

                let _ = responder.send(Ok(PullImageResponse {
                    image_uuid: image_uuid.clone(),
                }));

                tokio::spawn(pull_oci_image(
                    self.command_tx.clone(),
                    image_uuid,
                    image_ref,
                    pull_token,
                ));
            }
            OrchestratorCommand::FinalizePull {
                image_uuid,
                image_ref,
                pulled,
            } => {
                info!("Orchestrator: Finalizing pull for {image_uuid}");
                let mut exclusive = false;
                if let Some(info) = self.store.get_mut(&image_uuid) {
                    info.digest = pulled.digest.clone();
                    info.size_bytes = pulled.size_bytes;
                    exclusive = info.exclusive;
                }
                let (responder, resp_rx) = oneshot::channel();
                let file_cmd = FileCommand::StoreImage {
                    image_uuid: image_uuid.clone(),
                    image_ref,
                    exclusive,
                    pulled,
                    responder,
                };

                if self.filestore_tx.send(file_cmd).await.is_err() {
                    let err_msg = "Failed to send StoreImage command to FileStore.";
                    error!("Orchestrator: {err_msg}");
                    self.update_and_broadcast_state(
                        image_uuid,
                        ImageState::PullFailed,
                        err_msg.to_string(),
                    );
                    return;
                }

                match resp_rx.await {
                    Ok(Ok(())) => {
                        info!("Orchestrator: FileStore successfully stored image {image_uuid}");
                        self.update_and_broadcast_state(
                            image_uuid,
                            ImageState::Ready,
                            "Image is ready".to_string(),
                        );
                    }
                    Ok(Err(e)) => {
                        let err_msg = format!("FileStore failed to store image: {e}");
                        error!("Orchestrator: {err_msg} ({image_uuid})");
                        self.update_and_broadcast_state(
                            image_uuid,
                            ImageState::PullFailed,
                            err_msg,
                        );
                    }
                    Err(_) => {
                        let err_msg = "FileStore actor dropped response channel.";
                        error!("Orchestrator: {err_msg} ({image_uuid})");
                        self.update_and_broadcast_state(
                            image_uuid,
                            ImageState::PullFailed,
                            err_msg.to_string(),
                        );
                    }
                }
            }
            OrchestratorCommand::FailPull { image_uuid, error } => {
                let err_msg = format!("Pull failed: {error}");
                error!("Orchestrator: {err_msg} ({image_uuid})");
                self.update_and_broadcast_state(image_uuid, ImageState::PullFailed, err_msg);
            }
            OrchestratorCommand::ListImages { responder } => {
                let images = self.store.values().cloned().collect();
                let _ = responder.send(Ok(ListImagesResponse { images }));
            }
            OrchestratorCommand::DeleteImage {
                image_uuid,
                responder,
            } => {
                info!("Orchestrator: Deleting image {image_uuid}");
                let (file_resp_tx, file_resp_rx) = oneshot::channel();
                let file_cmd = FileCommand::DeleteImage {
                    image_uuid: image_uuid.clone(),
                    responder: file_resp_tx,
                };

                if self.filestore_tx.send(file_cmd).await.is_err() {
                    error!("Orchestrator: Failed to send DeleteImage command to FileStore.");
                } else if let Ok(Err(e)) = file_resp_rx.await {
                    if e.kind() == std::io::ErrorKind::ResourceBusy {
                        let _ = responder
                            .send(Err(ImageServiceError::InUse(image_uuid, e.to_string())));
                        return;
                    }
                    if e.kind() != std::io::ErrorKind::NotFound {
                        warn!("Orchestrator: FileStore failed to delete {image_uuid}: {e}");
                    }
                }

                self.store.remove(&image_uuid);
                self.broadcast_state_change(
                    image_uuid,
                    ImageState::NotFound,
                    "Image deleted".to_string(),
                );
                let _ = responder.send(Ok(DeleteImageResponse {}));
            }
            OrchestratorCommand::WatchImageStatus {
                image_uuid,
                stream_sender,
            } => {
                let initial_state = self
                    .store
                    .get(&image_uuid)
                    .map(|info| ImageState::try_from(info.state).unwrap_or(ImageState::Unspecified))
                    .unwrap_or(ImageState::NotFound);

                tokio::spawn(watch_image_status_stream(
                    image_uuid,
                    initial_state,
                    stream_sender,
                    self.broadcast_tx.subscribe(),
                ));
            }
        }
    }

    fn update_and_broadcast_state(
        &mut self,
        image_uuid: String,
        new_state: ImageState,
        message: String,
    ) {
        if let Some(info) = self.store.get_mut(&image_uuid) {
            info.state = new_state as i32;
        }
        self.broadcast_state_change(image_uuid, new_state, message);
    }

    fn broadcast_state_change(&self, image_uuid: String, state: ImageState, message: String) {
        let event = ImageStateEvent {
            image_uuid,
            state,
            message,
        };
        if self.broadcast_tx.send(event).is_err() {
            info!("Orchestrator: Broadcast failed, no active listeners.");
        }
    }
}

/// Pulls an image into its pull directory and hands the result back to the
/// orchestrator.
pub async fn pull_oci_image(
    command_tx: mpsc::Sender<OrchestratorCommand>,
    image_uuid: String,
    image_ref: String,
    pull_token: PullToken,
) {
    let dir = pull_dir(&image_uuid);
    let result = async {
        let _ = tokio::fs::remove_dir_all(&dir).await;
        tokio::fs::create_dir_all(&dir).await?;
        pull_image(&image_ref, &dir, &pull_token).await
    }
    .await;
    let cmd = match result {
        Ok(pulled) => OrchestratorCommand::FinalizePull {
            image_uuid,
            image_ref,
            pulled,
        },
        Err(error) => {
            let _ = tokio::fs::remove_dir_all(&dir).await;
            OrchestratorCommand::FailPull { image_uuid, error }
        }
    };
    if command_tx.send(cmd).await.is_err() {
        error!("ImagePuller: Failed to report the pull result. Actor may be down.");
    }
}

pub async fn watch_image_status_stream(
    image_uuid_to_watch: String,
    initial_state: ImageState,
    stream_sender: mpsc::Sender<Result<ImageStatusResponse, Status>>,
    mut broadcast_rx: broadcast::Receiver<ImageStateEvent>,
) {
    info!("ImageWatcher: Starting watch stream for {image_uuid_to_watch}");

    let initial_response = ImageStatusResponse {
        state: initial_state as i32,
        progress_percent: if initial_state == ImageState::Ready {
            100
        } else {
            0
        },
        message: format!("Initial state: {initial_state:?}"),
    };
    if stream_sender.send(Ok(initial_response)).await.is_err() {
        info!(
            "ImageWatcher: Client disconnected before initial state send for {image_uuid_to_watch}"
        );
        return;
    }

    if matches!(
        initial_state,
        ImageState::Ready | ImageState::PullFailed | ImageState::NotFound
    ) {
        info!(
            "ImageWatcher: Closing stream for {image_uuid_to_watch} due to terminal initial state."
        );
        return;
    }

    loop {
        match broadcast_rx.recv().await {
            Ok(event) => {
                if event.image_uuid == image_uuid_to_watch {
                    info!(
                        "ImageWatcher: Got relevant event for {image_uuid_to_watch}: {:?}",
                        event.state
                    );
                    let response = ImageStatusResponse {
                        state: event.state as i32,
                        progress_percent: if event.state == ImageState::Ready {
                            100
                        } else {
                            0
                        },
                        message: event.message,
                    };

                    if stream_sender.send(Ok(response)).await.is_err() {
                        info!("ImageWatcher: Client disconnected. Closing watch for {image_uuid_to_watch}");
                        break;
                    }

                    if matches!(
                        event.state,
                        ImageState::Ready | ImageState::PullFailed | ImageState::NotFound
                    ) {
                        info!("ImageWatcher: Reached terminal state. Closing watch for {image_uuid_to_watch}");
                        break;
                    }
                }
            }
            Err(broadcast::error::RecvError::Lagged(n)) => {
                warn!("ImageWatcher: Stream for {image_uuid_to_watch} lagged by {n} messages. Continuing.");
            }
            Err(broadcast::error::RecvError::Closed) => {
                info!("ImageWatcher: Broadcast channel closed. Shutting down watch for {image_uuid_to_watch}");
                break;
            }
        }
    }
}
