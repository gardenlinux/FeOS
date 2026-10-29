// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::{Command, OrchestratorCommand};
use log::info;
use tokio::sync::mpsc;

pub struct ImageServiceDispatcher {
    command_rx: mpsc::Receiver<Command>,
    command_tx: mpsc::Sender<Command>,
    orchestrator_tx: mpsc::Sender<OrchestratorCommand>,
}

impl ImageServiceDispatcher {
    pub fn new(orchestrator_tx: mpsc::Sender<OrchestratorCommand>) -> Self {
        let (command_tx, command_rx) = mpsc::channel(32);
        Self {
            command_rx,
            command_tx,
            orchestrator_tx,
        }
    }

    pub fn get_command_sender(&self) -> mpsc::Sender<Command> {
        self.command_tx.clone()
    }

    pub async fn run(mut self) {
        info!("GrpcDispatcher: Running and waiting for API commands.");
        while let Some(cmd) = self.command_rx.recv().await {
            self.handle_command(cmd).await;
        }
        info!("GrpcDispatcher: Channel closed, shutting down.");
    }

    async fn handle_command(&mut self, cmd: Command) {
        let orchestrator_cmd = match cmd {
            Command::PullImage(req, pull_token, responder) => OrchestratorCommand::PullImage {
                image_ref: req.image_ref,
                exclusive: req.exclusive,
                pull_token,
                responder,
            },
            Command::ListImages(_req, responder) => OrchestratorCommand::ListImages { responder },
            Command::DeleteImage(req, responder) => OrchestratorCommand::DeleteImage {
                image_uuid: req.image_uuid,
                responder,
            },
            Command::WatchImageStatus(req, stream_sender) => {
                OrchestratorCommand::WatchImageStatus {
                    image_uuid: req.image_uuid,
                    stream_sender,
                }
            }
        };

        if self.orchestrator_tx.send(orchestrator_cmd).await.is_err() {
            log::error!("GrpcDispatcher: Failed to send command to Orchestrator. The actor may have shut down.");
        }
    }
}
