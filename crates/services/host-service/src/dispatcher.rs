// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::worker::network::PushedNetworkConfig;
use crate::{worker, Command, RestartSignal};
use feos_utils::feos_logger::LogHandle;
use log::info;
use tokio::sync::mpsc;
use tonic::Status;

pub struct HostServiceDispatcher {
    rx: mpsc::Receiver<Command>,
    /// Where an upgrade hands the new binary over; None where this FeOS
    /// must not replace its own binary.
    restart_tx: Option<mpsc::Sender<RestartSignal>>,
    log_handle: LogHandle,
    /// Pings served since boot.
    pings: u64,
    pushed_network_config: PushedNetworkConfig,
}

impl HostServiceDispatcher {
    pub fn new(
        rx: mpsc::Receiver<Command>,
        restart_tx: Option<mpsc::Sender<RestartSignal>>,
        log_handle: LogHandle,
    ) -> Self {
        Self {
            rx,
            restart_tx,
            log_handle,
            pings: 0,
            pushed_network_config: PushedNetworkConfig::default(),
        }
    }

    pub async fn run(mut self) {
        info!("HostDispatcher: Running and waiting for commands.");
        while let Some(cmd) = self.rx.recv().await {
            match cmd {
                Command::GetHostname(responder) => {
                    tokio::spawn(worker::handle_hostname(responder));
                }
                Command::GetMemory(responder) => {
                    tokio::spawn(worker::handle_get_memory(responder));
                }
                Command::GetCPUInfo(responder) => {
                    tokio::spawn(worker::handle_get_cpu_info(responder));
                }
                Command::GetKernelStats(responder) => {
                    tokio::spawn(worker::handle_get_kernel_stats(responder));
                }
                Command::GetNetworkInfo(responder) => {
                    tokio::spawn(worker::handle_get_network_info(responder));
                }
                Command::GetVersionInfo(responder) => {
                    tokio::spawn(worker::handle_get_version_info(responder));
                }
                Command::UpgradeFeosBinary(req, responder) => match self.restart_tx.clone() {
                    Some(restart_tx) => {
                        tokio::spawn(worker::handle_upgrade(restart_tx, req, responder));
                    }
                    None => {
                        let _ = responder.send(Err(Status::failed_precondition(
                            "this FeOS does not replace its own binary: a package \
                             manager owns it (feosd does with --self-upgrade)",
                        )));
                    }
                },
                Command::StreamKernelLogs(stream_tx) => {
                    tokio::spawn(worker::handle_stream_kernel_logs(stream_tx));
                }
                Command::StreamFeOSLogs(stream_tx) => {
                    let log_handle = self.log_handle.clone();
                    tokio::spawn(worker::handle_stream_feos_logs(log_handle, stream_tx));
                }
                Command::Shutdown(req, responder) => {
                    tokio::spawn(worker::handle_shutdown(req, responder));
                }
                Command::Reboot(req, responder) => {
                    tokio::spawn(worker::handle_reboot(req, responder));
                }
                Command::Ping(responder) => {
                    self.pings += 1;
                    tokio::spawn(worker::info::handle_ping(self.pings, responder));
                }
                Command::SetTime(req, responder) => {
                    tokio::spawn(worker::clock::handle_set_time(req, responder));
                }
                Command::GetTime(responder) => {
                    tokio::spawn(worker::clock::handle_get_time(responder));
                }
                Command::AdjustTime(req, responder) => {
                    tokio::spawn(worker::clock::handle_adjust_time(req, responder));
                }
                Command::SetNetworkConfig(req, responder) => {
                    let pushed = self.pushed_network_config.clone();
                    tokio::spawn(worker::network::handle_set_network_config(
                        req, pushed, responder,
                    ));
                }
                Command::GetNetworkConfig(responder) => {
                    let pushed = self.pushed_network_config.clone();
                    tokio::spawn(worker::network::handle_get_network_config(
                        pushed, responder,
                    ));
                }
                Command::FreezeFilesystem(req, responder) => {
                    tokio::spawn(worker::filesystem::handle_freeze(req, responder));
                }
                Command::ThawFilesystem(req, responder) => {
                    tokio::spawn(worker::filesystem::handle_thaw(req, responder));
                }
                Command::GetMemoryBreakdown(req, responder) => {
                    tokio::spawn(worker::memory::handle_get_memory_breakdown(req, responder));
                }
                Command::PreparePause(req, responder) => {
                    tokio::spawn(worker::memory::handle_prepare_pause(req, responder));
                }
            }
        }
        info!("HostDispatcher: Channel closed, shutting down.");
    }
}
