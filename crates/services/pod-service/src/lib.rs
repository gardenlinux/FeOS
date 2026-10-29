// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Pods (ADR 0004): `feos.pod.v1.PodService`, and the forwarding of a
//! pod's services to its guest; their volumes (ADR 0016),
//! `feos.volume.v1.VolumeService`.

mod bake;
pub mod disk;
pub mod forward;
#[cfg(test)]
mod hung;
pub mod network;
mod pods;
mod spec;
mod store;
pub mod volumes;

use feos_proto::network_service::mesh_service_server::MeshService;
use feos_proto::network_service::{
    GetMeshRequest, Mesh, PutMeshPeersRequest, PutMeshPeersResponse, UpdateMeshPeersRequest,
    UpdateMeshPeersResponse,
};
use feos_proto::pod_service::pod_service_server::PodService;
use feos_proto::pod_service::{
    CreatePodRequest, CreatePodResponse, DeletePodRequest, DeletePodResponse, GetPodRequest,
    HibernatePodRequest, HibernatePodResponse, ListPodsRequest, ListPodsResponse, PodConsoleInput,
    PodConsoleOutput, PodEvent, PodInfo, RestorePodRequest, RestorePodResponse, ResumePodRequest,
    ResumePodResponse, StartPodRequest, StartPodResponse, StopPodRequest, StopPodResponse,
    StreamPodEventsRequest, SuspendPodRequest, SuspendPodResponse,
};
use feos_proto::volume_service::volume_service_server::VolumeService;
use feos_proto::volume_service::{
    CreateVolumeRequest, DeleteVolumeRequest, DeleteVolumeResponse, GetVolumeRequest,
    ListVolumesRequest, ListVolumesResponse, Volume,
};
pub use pods::{read_record, Pods, StopRecord, Stopped, STOP_RECORD};
use std::pin::Pin;
use tokio::sync::{broadcast, mpsc};
use tokio_stream::wrappers::ReceiverStream;
use tokio_stream::Stream;
use tonic::{Request, Response, Status, Streaming};
pub use volumes::Volumes;

/// Where the pods are kept: a file each.
pub const POD_DIR: &str = "/var/lib/feos/pods";

/// The node's mesh (ADR 0017), part of the pods' network.
#[tonic::async_trait]
impl MeshService for Pods {
    async fn put_mesh_peers(
        &self,
        request: Request<PutMeshPeersRequest>,
    ) -> Result<Response<PutMeshPeersResponse>, Status> {
        self.network().put_mesh_peers(request.into_inner()).await?;
        Ok(Response::new(PutMeshPeersResponse {}))
    }

    async fn update_mesh_peers(
        &self,
        request: Request<UpdateMeshPeersRequest>,
    ) -> Result<Response<UpdateMeshPeersResponse>, Status> {
        self.network()
            .update_mesh_peers(request.into_inner())
            .await?;
        Ok(Response::new(UpdateMeshPeersResponse {}))
    }

    async fn get_mesh(&self, _: Request<GetMeshRequest>) -> Result<Response<Mesh>, Status> {
        Ok(Response::new(self.network().mesh()))
    }
}

#[tonic::async_trait]
impl PodService for Pods {
    type StreamPodEventsStream = Pin<Box<dyn Stream<Item = Result<PodEvent, Status>> + Send>>;
    type StreamPodConsoleStream =
        Pin<Box<dyn Stream<Item = Result<PodConsoleOutput, Status>> + Send>>;

    async fn create_pod(
        &self,
        request: Request<CreatePodRequest>,
    ) -> Result<Response<CreatePodResponse>, Status> {
        self.check_serving()?;
        let request = request.into_inner();
        let pod = self
            .create(
                request.pod_id,
                request.spec.unwrap_or_default(),
                request.image_layers,
            )
            .await?;
        Ok(Response::new(CreatePodResponse { pod: Some(pod) }))
    }

    async fn start_pod(
        &self,
        request: Request<StartPodRequest>,
    ) -> Result<Response<StartPodResponse>, Status> {
        self.check_serving()?;
        self.start(&request.into_inner().pod_id).await?;
        Ok(Response::new(StartPodResponse {}))
    }

    async fn stop_pod(
        &self,
        request: Request<StopPodRequest>,
    ) -> Result<Response<StopPodResponse>, Status> {
        self.check_serving()?;
        self.stop(&request.into_inner().pod_id).await?;
        Ok(Response::new(StopPodResponse {}))
    }

    async fn delete_pod(
        &self,
        request: Request<DeletePodRequest>,
    ) -> Result<Response<DeletePodResponse>, Status> {
        self.check_serving()?;
        self.delete(&request.into_inner().pod_id).await?;
        Ok(Response::new(DeletePodResponse {}))
    }

    async fn get_pod(&self, request: Request<GetPodRequest>) -> Result<Response<PodInfo>, Status> {
        Ok(Response::new(self.get(&request.into_inner().pod_id).await?))
    }

    async fn list_pods(
        &self,
        _request: Request<ListPodsRequest>,
    ) -> Result<Response<ListPodsResponse>, Status> {
        Ok(Response::new(ListPodsResponse { pods: self.list() }))
    }

    async fn stream_pod_events(
        &self,
        request: Request<StreamPodEventsRequest>,
    ) -> Result<Response<Self::StreamPodEventsStream>, Status> {
        let only = request.into_inner().pod_id;
        let mut events = self.subscribe();
        let (tx, rx) = mpsc::channel(64);
        tokio::spawn(async move {
            loop {
                let event = match events.recv().await {
                    Ok(event) => Ok(event),
                    Err(broadcast::error::RecvError::Lagged(missed)) => {
                        Err(Status::data_loss(format!("{missed} events were missed")))
                    }
                    Err(broadcast::error::RecvError::Closed) => return,
                };
                if let Ok(event) = &event
                    && !only.is_empty()
                    && event.pod_id != only
                {
                    continue;
                }
                // A stream that missed events ends with the error.
                let end = event.is_err();
                if tx.send(event).await.is_err() || end {
                    return;
                }
            }
        });
        Ok(Response::new(Box::pin(ReceiverStream::new(rx))))
    }

    async fn suspend_pod(
        &self,
        request: Request<SuspendPodRequest>,
    ) -> Result<Response<SuspendPodResponse>, Status> {
        self.check_serving()?;
        let memory_bytes = self.suspend(&request.into_inner().pod_id).await?;
        Ok(Response::new(SuspendPodResponse { memory_bytes }))
    }

    async fn stream_pod_console(
        &self,
        request: Request<Streaming<PodConsoleInput>>,
    ) -> Result<Response<Self::StreamPodConsoleStream>, Status> {
        let output = self.console(request.into_inner()).await?;
        Ok(Response::new(Box::pin(output)))
    }

    async fn resume_pod(
        &self,
        request: Request<ResumePodRequest>,
    ) -> Result<Response<ResumePodResponse>, Status> {
        self.check_serving()?;
        self.resume(&request.into_inner().pod_id).await?;
        Ok(Response::new(ResumePodResponse {}))
    }

    async fn hibernate_pod(
        &self,
        request: Request<HibernatePodRequest>,
    ) -> Result<Response<HibernatePodResponse>, Status> {
        self.check_serving()?;
        let request = request.into_inner();
        Ok(Response::new(
            self.hibernate(&request.pod_id, &request.request_id, request.no_abandon)
                .await?,
        ))
    }

    async fn restore_pod(
        &self,
        request: Request<RestorePodRequest>,
    ) -> Result<Response<RestorePodResponse>, Status> {
        self.check_serving()?;
        let request = request.into_inner();
        Ok(Response::new(
            self.restore(&request.pod_id, &request.head_key, request.cold)
                .await?,
        ))
    }
}

#[tonic::async_trait]
impl VolumeService for Volumes {
    async fn create_volume(
        &self,
        request: Request<CreateVolumeRequest>,
    ) -> Result<Response<Volume>, Status> {
        let request = request.into_inner();
        Ok(Response::new(
            self.create(&request.volume_id, request.size_bytes, request.labels)
                .await?,
        ))
    }

    async fn delete_volume(
        &self,
        request: Request<DeleteVolumeRequest>,
    ) -> Result<Response<DeleteVolumeResponse>, Status> {
        self.delete(&request.into_inner().volume_id).await?;
        Ok(Response::new(DeleteVolumeResponse {}))
    }

    async fn get_volume(
        &self,
        request: Request<GetVolumeRequest>,
    ) -> Result<Response<Volume>, Status> {
        Ok(Response::new(self.get(&request.into_inner().volume_id)?))
    }

    async fn list_volumes(
        &self,
        _request: Request<ListVolumesRequest>,
    ) -> Result<Response<ListVolumesResponse>, Status> {
        Ok(Response::new(ListVolumesResponse {
            volumes: self.list(),
        }))
    }
}
