// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::error::ContainerServiceError;
use feos_proto::container_service::{
    ContainerInfo, CreateContainerRequest, CreateContainerResponse, DeleteContainerRequest,
    DeleteContainerResponse, ExecRequest, GetContainerRequest, ListContainersRequest,
    ListContainersResponse, LogEntry, StartContainerRequest, StartContainerResponse,
    StopContainerRequest, StopContainerResponse, StreamContainerLogsRequest,
};
use feos_proto::task_service::ExecFrame;
use image_service::PullToken;
use tokio::sync::{mpsc, oneshot};
use tonic::{Status, Streaming};

pub mod api;
pub mod dispatcher;
pub mod error;
pub mod logs;
pub mod persistence;
pub mod runtime;
pub mod worker;

pub enum Command {
    /// A create, its request's pull token taken out into the `PullToken`.
    CreateContainer(
        CreateContainerRequest,
        PullToken,
        oneshot::Sender<Result<CreateContainerResponse, ContainerServiceError>>,
    ),
    StartContainer(
        StartContainerRequest,
        oneshot::Sender<Result<StartContainerResponse, ContainerServiceError>>,
    ),
    StopContainer(
        StopContainerRequest,
        oneshot::Sender<Result<StopContainerResponse, ContainerServiceError>>,
    ),
    GetContainer(
        GetContainerRequest,
        oneshot::Sender<Result<ContainerInfo, ContainerServiceError>>,
    ),
    ListContainers(
        ListContainersRequest,
        oneshot::Sender<Result<ListContainersResponse, ContainerServiceError>>,
    ),
    DeleteContainer(
        DeleteContainerRequest,
        oneshot::Sender<Result<DeleteContainerResponse, ContainerServiceError>>,
    ),
    Exec(
        ExecRequest,
        oneshot::Sender<Result<Streaming<ExecFrame>, ContainerServiceError>>,
    ),
    StreamContainerLogs(
        StreamContainerLogsRequest,
        oneshot::Sender<Result<mpsc::Receiver<Result<LogEntry, Status>>, ContainerServiceError>>,
    ),
}

impl std::fmt::Debug for Command {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Command::CreateContainer(req, token, _) => f
                .debug_tuple("CreateContainer")
                .field(req)
                .field(token)
                .finish(),
            Command::StartContainer(req, _) => f.debug_tuple("StartContainer").field(req).finish(),
            Command::StopContainer(req, _) => f.debug_tuple("StopContainer").field(req).finish(),
            Command::GetContainer(req, _) => f.debug_tuple("GetContainer").field(req).finish(),
            Command::ListContainers(req, _) => f.debug_tuple("ListContainers").field(req).finish(),
            Command::DeleteContainer(req, _) => {
                f.debug_tuple("DeleteContainer").field(req).finish()
            }
            Command::Exec(req, _) => f.debug_tuple("Exec").field(req).finish(),
            Command::StreamContainerLogs(req, _) => {
                f.debug_tuple("StreamContainerLogs").field(req).finish()
            }
        }
    }
}
