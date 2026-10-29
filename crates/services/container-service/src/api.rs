// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::Command;
use feos_proto::container_service::{
    container_service_server::ContainerService, exec_stream_frame, ContainerEvent, ContainerInfo,
    CreateContainerRequest, CreateContainerResponse, DeleteContainerRequest,
    DeleteContainerResponse, ExecRequest, ExecStreamFrame, GetContainerRequest,
    ListContainersRequest, ListContainersResponse, LogEntry, StartContainerRequest,
    StartContainerResponse, StopContainerRequest, StopContainerResponse,
    StreamContainerEventsRequest, StreamContainerLogsRequest,
};
use feos_proto::task_service::exec_frame;
use feos_utils::dispatch::dispatch_and_wait;
use image_service::PullToken;
use log::info;
use std::pin::Pin;
use tokio::sync::mpsc;
use tokio_stream::{wrappers::ReceiverStream, Stream, StreamExt};
use tonic::{Request, Response, Status};

pub struct ContainerApiHandler {
    dispatcher_tx: mpsc::Sender<Command>,
}

impl ContainerApiHandler {
    pub fn new(dispatcher_tx: mpsc::Sender<Command>) -> Self {
        Self { dispatcher_tx }
    }
}

#[tonic::async_trait]
impl ContainerService for ContainerApiHandler {
    type StreamContainerLogsStream = Pin<Box<dyn Stream<Item = Result<LogEntry, Status>> + Send>>;
    type StreamContainerEventsStream =
        Pin<Box<dyn Stream<Item = Result<ContainerEvent, Status>> + Send>>;
    type ExecStream = Pin<Box<dyn Stream<Item = Result<ExecStreamFrame, Status>> + Send>>;

    async fn create_container(
        &self,
        request: Request<CreateContainerRequest>,
    ) -> Result<Response<CreateContainerResponse>, Status> {
        info!("ContainerApi: Received CreateContainer request.");
        let mut request = request.into_inner();
        let token = PullToken::new(std::mem::take(&mut request.pull_token));
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::CreateContainer(request, token, resp_tx)
        })
        .await
    }

    async fn start_container(
        &self,
        request: Request<StartContainerRequest>,
    ) -> Result<Response<StartContainerResponse>, Status> {
        info!("ContainerApi: Received StartContainer request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::StartContainer(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn stop_container(
        &self,
        request: Request<StopContainerRequest>,
    ) -> Result<Response<StopContainerResponse>, Status> {
        info!("ContainerApi: Received StopContainer request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::StopContainer(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn get_container(
        &self,
        request: Request<GetContainerRequest>,
    ) -> Result<Response<ContainerInfo>, Status> {
        info!("ContainerApi: Received GetContainer request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::GetContainer(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn list_containers(
        &self,
        request: Request<ListContainersRequest>,
    ) -> Result<Response<ListContainersResponse>, Status> {
        info!("ContainerApi: Received ListContainers request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::ListContainers(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn delete_container(
        &self,
        request: Request<DeleteContainerRequest>,
    ) -> Result<Response<DeleteContainerResponse>, Status> {
        info!("ContainerApi: Received DeleteContainer request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::DeleteContainer(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn stream_container_logs(
        &self,
        request: Request<StreamContainerLogsRequest>,
    ) -> Result<Response<Self::StreamContainerLogsStream>, Status> {
        info!("ContainerApi: Received StreamContainerLogs request.");
        let entries = dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::StreamContainerLogs(request.into_inner(), resp_tx)
        })
        .await?
        .into_inner();
        // Dropping this stream ends the reader (crate::logs).
        Ok(Response::new(Box::pin(ReceiverStream::new(entries))))
    }

    async fn stream_container_events(
        &self,
        _request: Request<StreamContainerEventsRequest>,
    ) -> Result<Response<Self::StreamContainerEventsStream>, Status> {
        Err(Status::unimplemented("Not yet implemented"))
    }

    async fn exec(
        &self,
        request: Request<ExecRequest>,
    ) -> Result<Response<Self::ExecStream>, Status> {
        info!("ContainerApi: Received Exec request.");
        let frames = dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::Exec(request.into_inner(), resp_tx)
        })
        .await?
        .into_inner();
        // Dropping this stream drops the task service's, which ends the
        // command.
        let frames = frames.map(|frame| {
            frame.map(|frame| ExecStreamFrame {
                frame: frame.frame.map(|frame| match frame {
                    exec_frame::Frame::Stdout(bytes) => exec_stream_frame::Frame::Stdout(bytes),
                    exec_frame::Frame::Stderr(bytes) => exec_stream_frame::Frame::Stderr(bytes),
                    exec_frame::Frame::ExitCode(code) => exec_stream_frame::Frame::ExitCode(code),
                }),
            })
        });
        Ok(Response::new(Box::pin(frames)))
    }
}
