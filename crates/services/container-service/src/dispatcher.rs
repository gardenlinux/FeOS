// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::{
    error::ContainerServiceError,
    persistence::{repository::ContainerRepository, ContainerRecord},
    runtime::{adapter::ContainerAdapter, bundle},
    worker, Command,
};
use feos_proto::container_service::{ContainerInfo, ContainerState, ListContainersResponse};
use feos_utils::filesystem::CONTAINERS_DIR;
use log::{info, warn};
use std::{path::Path, sync::Arc};
use task_service::RUNTIME_STATE_DIR;
use tokio::sync::mpsc;

pub struct Dispatcher {
    rx: mpsc::Receiver<Command>,
    repository: ContainerRepository,
    adapter: Arc<ContainerAdapter>,
}

/// Container IDs name directories: 1 to 64 letters, digits, "-", "_" and
/// ".", and not "." or "..".
pub(crate) fn validate_container_id(id: &str) -> Result<(), ContainerServiceError> {
    let valid = !id.is_empty()
        && id.len() <= 64
        && id != "."
        && id != ".."
        && id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'));
    if valid {
        Ok(())
    } else {
        Err(ContainerServiceError::InvalidArgument(format!(
            "invalid container ID {id:?}"
        )))
    }
}

/// Whether the runtime knows the container: its state lives in RAM, so after
/// a reboot it knows none.
pub(crate) fn runtime_knows(id: &str) -> bool {
    Path::new(RUNTIME_STATE_DIR).join(id).exists()
}

impl Dispatcher {
    pub async fn new(rx: mpsc::Receiver<Command>) -> Result<Self, ContainerServiceError> {
        let repository = ContainerRepository::connect(CONTAINERS_DIR).await?;
        Self::reconcile(&repository).await;
        let adapter = Arc::new(ContainerAdapter::new());
        Ok(Self {
            rx,
            repository,
            adapter,
        })
    }

    /// Records from before a reboot: on a data volume they survive it, the
    /// containers do not. A container the runtime does not know is stopped;
    /// one whose creation was cut short is removed.
    async fn reconcile(repository: &ContainerRepository) {
        let records = match repository.list_all_containers().await {
            Ok(records) => records,
            Err(e) => {
                warn!("Dispatcher: Failed to list containers to reconcile: {e}");
                return;
            }
        };
        for record in records {
            let id = &record.container_id;
            match record.status.state {
                ContainerState::PullingImage => {
                    info!("Dispatcher: Removing container {id}, whose creation was cut short");
                    let removed = id.clone();
                    let _ = tokio::task::spawn_blocking(move || bundle::remove(&removed)).await;
                    let _ = repository.delete_container(id).await;
                }
                ContainerState::Created | ContainerState::Running if !runtime_knows(id) => {
                    info!("Dispatcher: Container {id} did not survive the reboot; stopped");
                    let _ = repository
                        .update_container_state(id, ContainerState::Stopped)
                        .await;
                }
                _ => {}
            }
        }
    }

    pub async fn run(mut self) {
        info!("Dispatcher: Running and waiting for commands.");
        while let Some(cmd) = self.rx.recv().await {
            let repo = self.repository.clone();
            let adapter = self.adapter.clone();
            tokio::spawn(async move {
                if let Err(e) = Self::handle_command(cmd, repo, adapter).await {
                    warn!("Dispatcher: Error handling command: {e}");
                }
            });
        }
        info!("Dispatcher: Channel closed, shutting down.");
    }

    pub(crate) async fn get_container_record(
        repo: &ContainerRepository,
        id_str: &str,
    ) -> Result<ContainerRecord, ContainerServiceError> {
        validate_container_id(id_str)?;
        repo.get_container(id_str).await?.ok_or_else(|| {
            ContainerServiceError::NotFound(format!("Container '{id_str}' not found"))
        })
    }

    async fn handle_command(
        cmd: Command,
        repository: ContainerRepository,
        adapter: Arc<ContainerAdapter>,
    ) -> Result<(), ContainerServiceError> {
        match cmd {
            Command::CreateContainer(req, token, responder) => {
                let result = worker::create_container(req, &token, &repository, &adapter).await;
                let _ = responder.send(result);
            }
            Command::StartContainer(req, responder) => {
                let record = Self::get_container_record(&repository, &req.container_id).await;
                match record {
                    Ok(rec) if rec.status.state == ContainerState::Created => {
                        tokio::spawn(worker::handle_start_container(
                            req, responder, repository, adapter,
                        ));
                    }
                    Ok(rec) => {
                        let _ = responder.send(Err(ContainerServiceError::InvalidState(format!(
                            "Cannot start container in state {:?}",
                            rec.status.state
                        ))));
                    }
                    Err(e) => {
                        let _ = responder.send(Err(e));
                    }
                }
            }
            Command::StopContainer(req, responder) => {
                let record = Self::get_container_record(&repository, &req.container_id).await;
                match record {
                    Ok(rec) if rec.status.state == ContainerState::Running => {
                        tokio::spawn(worker::handle_stop_container(
                            req, responder, repository, adapter,
                        ));
                    }
                    Ok(rec) => {
                        let _ = responder.send(Err(ContainerServiceError::InvalidState(format!(
                            "Cannot stop container in state {:?}",
                            rec.status.state
                        ))));
                    }
                    Err(e) => {
                        let _ = responder.send(Err(e));
                    }
                }
            }
            Command::DeleteContainer(req, responder) => {
                match Self::get_container_record(&repository, &req.container_id).await {
                    // The runtime kills a running container first.
                    Ok(_) => {
                        tokio::spawn(worker::handle_delete_container(
                            req, responder, repository, adapter,
                        ));
                    }
                    Err(e) => {
                        let _ = responder.send(Err(e));
                    }
                }
            }
            Command::GetContainer(req, responder) => {
                let result = Self::get_container_record(&repository, &req.container_id)
                    .await
                    .map(|rec| ContainerInfo {
                        container_id: rec.container_id,
                        state: rec.status.state as i32,
                        config: Some(rec.config),
                        pid: rec.status.process_id,
                        exit_code: rec.status.exit_code,
                    });
                let _ = responder.send(result);
            }
            Command::ListContainers(_req, responder) => {
                let result = repository
                    .list_all_containers()
                    .await
                    .map(|records| {
                        let containers = records
                            .into_iter()
                            .map(|rec| ContainerInfo {
                                container_id: rec.container_id,
                                state: rec.status.state as i32,
                                config: Some(rec.config),
                                pid: rec.status.process_id,
                                exit_code: rec.status.exit_code,
                            })
                            .collect();
                        ListContainersResponse { containers }
                    })
                    .map_err(ContainerServiceError::Persistence);
                let _ = responder.send(result);
            }
            Command::StreamContainerLogs(req, responder) => {
                match Self::get_container_record(&repository, &req.container_id).await {
                    Ok(_) => {
                        tokio::spawn(worker::handle_stream_logs(req, responder, repository));
                    }
                    Err(e) => {
                        let _ = responder.send(Err(e));
                    }
                }
            }
            Command::Exec(req, responder) => {
                let record = Self::get_container_record(&repository, &req.container_id).await;
                match record {
                    Ok(rec) if rec.status.state == ContainerState::Running => {
                        tokio::spawn(worker::handle_exec(req, responder, adapter));
                    }
                    Ok(rec) => {
                        let _ = responder.send(Err(ContainerServiceError::InvalidState(format!(
                            "Cannot exec in container in state {:?}",
                            rec.status.state
                        ))));
                    }
                    Err(e) => {
                        let _ = responder.send(Err(e));
                    }
                }
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn container_ids_are_directory_names() {
        for id in ["main", "a-b_c.1", "6f2c1e8e-6c1f-4e6e-9a51-0d1c5b1a2f3e"] {
            assert!(validate_container_id(id).is_ok(), "{id}");
        }
        for id in ["", ".", "..", "a/b", "a b", &"x".repeat(65)] {
            assert!(validate_container_id(id).is_err(), "{id}");
        }
    }
}
