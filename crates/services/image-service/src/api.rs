// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::{Command, PullToken};
use feos_proto::image_service::{
    image_service_server::ImageService, DeleteImageRequest, DeleteImageResponse,
    ImageStatusResponse, ListImagesRequest, ListImagesResponse, PullImageRequest,
    PullImageResponse, WatchImageStatusRequest,
};
use feos_utils::dispatch::dispatch_and_wait;
use log::info;
use std::pin::Pin;
use tokio::sync::mpsc;
use tokio_stream::{wrappers::ReceiverStream, Stream};
use tonic::{Request, Response, Status};

pub struct ImageApiHandler {
    dispatcher_tx: mpsc::Sender<Command>,
}

impl ImageApiHandler {
    pub fn new(dispatcher_tx: mpsc::Sender<Command>) -> Self {
        Self { dispatcher_tx }
    }
}

#[tonic::async_trait]
impl ImageService for ImageApiHandler {
    type WatchImageStatusStream =
        Pin<Box<dyn Stream<Item = Result<ImageStatusResponse, Status>> + Send>>;

    async fn pull_image(
        &self,
        request: Request<PullImageRequest>,
    ) -> Result<Response<PullImageResponse>, Status> {
        info!("ImageApi: Received PullImage request.");
        let mut request = request.into_inner();
        let token = PullToken::new(std::mem::take(&mut request.pull_token));
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::PullImage(request, token, resp_tx)
        })
        .await
    }

    async fn watch_image_status(
        &self,
        request: Request<WatchImageStatusRequest>,
    ) -> Result<Response<Self::WatchImageStatusStream>, Status> {
        info!("ImageApi: Received WatchImageStatus stream request.");
        let (stream_tx, stream_rx) = mpsc::channel(16);
        let cmd = Command::WatchImageStatus(request.into_inner(), stream_tx);
        self.dispatcher_tx
            .send(cmd)
            .await
            .map_err(|e| Status::internal(format!("Failed to send command to dispatcher: {e}")))?;
        let output_stream = ReceiverStream::new(stream_rx);
        Ok(Response::new(Box::pin(output_stream)))
    }

    async fn list_images(
        &self,
        request: Request<ListImagesRequest>,
    ) -> Result<Response<ListImagesResponse>, Status> {
        info!("ImageApi: Received ListImages request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::ListImages(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn delete_image(
        &self,
        request: Request<DeleteImageRequest>,
    ) -> Result<Response<DeleteImageResponse>, Status> {
        info!("ImageApi: Received DeleteImage request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::DeleteImage(request.into_inner(), resp_tx)
        })
        .await
    }
}
