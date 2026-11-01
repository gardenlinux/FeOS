// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::error::HostError;
use feos_proto::host_service::{
    AdjustTimeRequest, AdjustTimeResponse, FeosLogEntry, FreezeFilesystemRequest,
    FreezeFilesystemResponse, GetCpuInfoResponse, GetKernelStatsResponse,
    GetMemoryBreakdownRequest, GetNetworkConfigResponse, GetNetworkInfoResponse, GetTimeResponse,
    GetVersionInfoResponse, HostnameResponse, KernelLogEntry, MemoryBreakdown, MemoryResponse,
    PingResponse, PreparePauseRequest, PreparePauseResponse, RebootRequest, RebootResponse,
    SetNetworkConfigRequest, SetNetworkConfigResponse, SetTimeRequest, SetTimeResponse,
    ShutdownRequest, ShutdownResponse, ThawFilesystemRequest, ThawFilesystemResponse,
    UpgradeFeosBinaryRequest, UpgradeFeosBinaryResponse,
};
use std::path::PathBuf;
use tokio::sync::{mpsc, oneshot};
use tonic::Status;

pub mod api;
pub mod dispatcher;
pub mod error;
pub mod worker;

#[derive(Debug)]
pub enum Command {
    GetHostname(oneshot::Sender<Result<HostnameResponse, HostError>>),
    GetMemory(oneshot::Sender<Result<MemoryResponse, HostError>>),
    GetCPUInfo(oneshot::Sender<Result<GetCpuInfoResponse, HostError>>),
    GetKernelStats(oneshot::Sender<Result<GetKernelStatsResponse, HostError>>),
    GetNetworkInfo(oneshot::Sender<Result<GetNetworkInfoResponse, HostError>>),
    GetVersionInfo(oneshot::Sender<Result<GetVersionInfoResponse, HostError>>),
    UpgradeFeosBinary(
        UpgradeFeosBinaryRequest,
        oneshot::Sender<Result<UpgradeFeosBinaryResponse, Status>>,
    ),
    StreamKernelLogs(mpsc::Sender<Result<KernelLogEntry, Status>>),
    StreamFeOSLogs(mpsc::Sender<Result<FeosLogEntry, Status>>),
    Shutdown(
        ShutdownRequest,
        oneshot::Sender<Result<ShutdownResponse, HostError>>,
    ),
    Reboot(
        RebootRequest,
        oneshot::Sender<Result<RebootResponse, HostError>>,
    ),
    Ping(oneshot::Sender<Result<PingResponse, HostError>>),
    SetTime(
        SetTimeRequest,
        oneshot::Sender<Result<SetTimeResponse, HostError>>,
    ),
    GetTime(oneshot::Sender<Result<GetTimeResponse, HostError>>),
    AdjustTime(
        AdjustTimeRequest,
        oneshot::Sender<Result<AdjustTimeResponse, HostError>>,
    ),
    SetNetworkConfig(
        SetNetworkConfigRequest,
        oneshot::Sender<Result<SetNetworkConfigResponse, HostError>>,
    ),
    GetNetworkConfig(oneshot::Sender<Result<GetNetworkConfigResponse, HostError>>),
    FreezeFilesystem(
        FreezeFilesystemRequest,
        oneshot::Sender<Result<FreezeFilesystemResponse, HostError>>,
    ),
    ThawFilesystem(
        ThawFilesystemRequest,
        oneshot::Sender<Result<ThawFilesystemResponse, HostError>>,
    ),
    GetMemoryBreakdown(
        GetMemoryBreakdownRequest,
        oneshot::Sender<Result<MemoryBreakdown, HostError>>,
    ),
    PreparePause(
        PreparePauseRequest,
        oneshot::Sender<Result<PreparePauseResponse, HostError>>,
    ),
}

#[derive(Debug)]
pub struct RestartSignal(pub PathBuf);
