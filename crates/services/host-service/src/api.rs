// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::Command;
use feos_proto::host_service::{
    host_service_server::HostService, AdjustTimeRequest, AdjustTimeResponse, FeosLogEntry,
    FreezeFilesystemRequest, FreezeFilesystemResponse, GetCpuInfoRequest, GetCpuInfoResponse,
    GetKernelStatsRequest, GetKernelStatsResponse, GetMemoryBreakdownRequest,
    GetNetworkConfigRequest, GetNetworkConfigResponse, GetNetworkInfoRequest,
    GetNetworkInfoResponse, GetTimeRequest, GetTimeResponse, GetVersionInfoRequest,
    GetVersionInfoResponse, HostnameRequest, HostnameResponse, KernelLogEntry, MemoryBreakdown,
    MemoryRequest, MemoryResponse, PingRequest, PingResponse, PreparePauseRequest,
    PreparePauseResponse, RebootRequest, RebootResponse, SetNetworkConfigRequest,
    SetNetworkConfigResponse, SetTimeRequest, SetTimeResponse, ShutdownRequest, ShutdownResponse,
    StreamFeosLogsRequest, StreamKernelLogsRequest, ThawFilesystemRequest, ThawFilesystemResponse,
    UpgradeFeosBinaryRequest, UpgradeFeosBinaryResponse,
};
use feos_utils::dispatch::dispatch_and_wait;
use log::{debug, info};
use std::pin::Pin;
use tokio::sync::mpsc;
use tokio_stream::{wrappers::ReceiverStream, Stream};
use tonic::{Request, Response, Status};

pub struct HostApiHandler {
    dispatcher_tx: mpsc::Sender<Command>,
}

impl HostApiHandler {
    pub fn new(dispatcher_tx: mpsc::Sender<Command>) -> Self {
        Self { dispatcher_tx }
    }
}

#[tonic::async_trait]
impl HostService for HostApiHandler {
    type StreamKernelLogsStream =
        Pin<Box<dyn Stream<Item = Result<KernelLogEntry, Status>> + Send>>;
    type StreamFeOSLogsStream = Pin<Box<dyn Stream<Item = Result<FeosLogEntry, Status>> + Send>>;

    async fn hostname(
        &self,
        _request: Request<HostnameRequest>,
    ) -> Result<Response<HostnameResponse>, Status> {
        info!("HostApi: Received Hostname request.");
        dispatch_and_wait(&self.dispatcher_tx, Command::GetHostname).await
    }

    async fn get_memory(
        &self,
        _request: Request<MemoryRequest>,
    ) -> Result<Response<MemoryResponse>, Status> {
        info!("HostApi: Received GetMemory request.");
        dispatch_and_wait(&self.dispatcher_tx, Command::GetMemory).await
    }

    async fn get_cpu_info(
        &self,
        _request: Request<GetCpuInfoRequest>,
    ) -> Result<Response<GetCpuInfoResponse>, Status> {
        info!("HostApi: Received GetCPUInfo request.");
        dispatch_and_wait(&self.dispatcher_tx, Command::GetCPUInfo).await
    }

    async fn get_kernel_stats(
        &self,
        _request: Request<GetKernelStatsRequest>,
    ) -> Result<Response<GetKernelStatsResponse>, Status> {
        info!("HostApi: Received GetKernelStats request.");
        dispatch_and_wait(&self.dispatcher_tx, Command::GetKernelStats).await
    }

    async fn get_network_info(
        &self,
        _request: Request<GetNetworkInfoRequest>,
    ) -> Result<Response<GetNetworkInfoResponse>, Status> {
        info!("HostApi: Received GetNetworkInfo request.");
        dispatch_and_wait(&self.dispatcher_tx, Command::GetNetworkInfo).await
    }

    async fn shutdown(
        &self,
        request: Request<ShutdownRequest>,
    ) -> Result<Response<ShutdownResponse>, Status> {
        info!("HostApi: Received Shutdown request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::Shutdown(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn reboot(
        &self,
        request: Request<RebootRequest>,
    ) -> Result<Response<RebootResponse>, Status> {
        info!("HostApi: Received Reboot request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::Reboot(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn upgrade_feos_binary(
        &self,
        request: Request<UpgradeFeosBinaryRequest>,
    ) -> Result<Response<UpgradeFeosBinaryResponse>, Status> {
        info!("HostApi: Received UpgradeFeosBinary request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::UpgradeFeosBinary(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn stream_kernel_logs(
        &self,
        _request: Request<StreamKernelLogsRequest>,
    ) -> Result<Response<Self::StreamKernelLogsStream>, Status> {
        info!("HostApi: Received StreamKernelLogs request.");
        let (stream_tx, stream_rx) = mpsc::channel(128);
        let cmd = Command::StreamKernelLogs(stream_tx);
        self.dispatcher_tx
            .send(cmd)
            .await
            .map_err(|e| Status::internal(format!("Failed to send command to dispatcher: {e}")))?;
        let output_stream = ReceiverStream::new(stream_rx);
        Ok(Response::new(Box::pin(output_stream)))
    }

    async fn stream_fe_os_logs(
        &self,
        _request: Request<StreamFeosLogsRequest>,
    ) -> Result<Response<Self::StreamFeOSLogsStream>, Status> {
        info!("HostApi: Received StreamFeOSLogs request.");
        let (stream_tx, stream_rx) = mpsc::channel(128);
        let cmd = Command::StreamFeOSLogs(stream_tx);
        self.dispatcher_tx
            .send(cmd)
            .await
            .map_err(|e| Status::internal(format!("Failed to send command to dispatcher: {e}")))?;
        let output_stream = ReceiverStream::new(stream_rx);
        Ok(Response::new(Box::pin(output_stream)))
    }

    async fn get_version_info(
        &self,
        _request: Request<GetVersionInfoRequest>,
    ) -> Result<Response<GetVersionInfoResponse>, Status> {
        info!("HostApi: Received GetVersionInfo request.");
        dispatch_and_wait(&self.dispatcher_tx, Command::GetVersionInfo).await
    }

    async fn ping(&self, _request: Request<PingRequest>) -> Result<Response<PingResponse>, Status> {
        // Hosts ping often; keep it out of the info log.
        debug!("HostApi: Received Ping request.");
        dispatch_and_wait(&self.dispatcher_tx, Command::Ping).await
    }

    async fn set_time(
        &self,
        request: Request<SetTimeRequest>,
    ) -> Result<Response<SetTimeResponse>, Status> {
        info!("HostApi: Received SetTime request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::SetTime(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn get_time(
        &self,
        _request: Request<GetTimeRequest>,
    ) -> Result<Response<GetTimeResponse>, Status> {
        debug!("HostApi: Received GetTime request.");
        dispatch_and_wait(&self.dispatcher_tx, Command::GetTime).await
    }

    async fn adjust_time(
        &self,
        request: Request<AdjustTimeRequest>,
    ) -> Result<Response<AdjustTimeResponse>, Status> {
        // Once a minute: keep it out of the info log.
        debug!("HostApi: Received AdjustTime request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::AdjustTime(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn set_network_config(
        &self,
        request: Request<SetNetworkConfigRequest>,
    ) -> Result<Response<SetNetworkConfigResponse>, Status> {
        info!("HostApi: Received SetNetworkConfig request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::SetNetworkConfig(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn get_network_config(
        &self,
        _request: Request<GetNetworkConfigRequest>,
    ) -> Result<Response<GetNetworkConfigResponse>, Status> {
        info!("HostApi: Received GetNetworkConfig request.");
        dispatch_and_wait(&self.dispatcher_tx, Command::GetNetworkConfig).await
    }

    async fn freeze_filesystem(
        &self,
        request: Request<FreezeFilesystemRequest>,
    ) -> Result<Response<FreezeFilesystemResponse>, Status> {
        info!("HostApi: Received FreezeFilesystem request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::FreezeFilesystem(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn thaw_filesystem(
        &self,
        request: Request<ThawFilesystemRequest>,
    ) -> Result<Response<ThawFilesystemResponse>, Status> {
        info!("HostApi: Received ThawFilesystem request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::ThawFilesystem(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn get_memory_breakdown(
        &self,
        request: Request<GetMemoryBreakdownRequest>,
    ) -> Result<Response<MemoryBreakdown>, Status> {
        info!("HostApi: Received GetMemoryBreakdown request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::GetMemoryBreakdown(request.into_inner(), resp_tx)
        })
        .await
    }

    async fn prepare_pause(
        &self,
        request: Request<PreparePauseRequest>,
    ) -> Result<Response<PreparePauseResponse>, Status> {
        info!("HostApi: Received PreparePause request.");
        dispatch_and_wait(&self.dispatcher_tx, |resp_tx| {
            Command::PreparePause(request.into_inner(), resp_tx)
        })
        .await
    }
}
