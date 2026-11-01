// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The VM service's end of a FeOS guest.
//!
//! FeOS in the guest profile dials its host on vsock port 10001 once its API
//! is up and holds a LifecycleService call open. Firecracker forwards the
//! connection to the Unix socket `<vsock_path>_10001`, on which the VM
//! service listens for each VM; which socket a call came in on tells which
//! VM it is, never the guest.
//!
//! The guest has no DHCP, no NTP and no RTC. The VM service pushes it the
//! wall time and its network configuration through its API (vsock port
//! 10000) whenever it attaches, which it does after a boot and whenever its
//! call was lost, and after every resume. A resume does not wait for the
//! uplink: a call that sat idle before a snapshot is not woken in the
//! restored guest until its keepalive notices, seconds later. The guest's
//! listeners come back with its memory, so the resume pings it instead.
//!
//! While a guest is attached, the VM service measures its clock once a
//! minute and has it correct the offset (`AdjustTime`, ADR 0027): kvm-clock
//! runs at the node's uncorrected rate and drifts by about a second a day.

use crate::{vsock_path, VmEventWrapper};
use feos_proto::host_service::{
    host_service_client::HostServiceClient, AdjustTimeRequest, GetTimeRequest, NetworkRoute,
    PingRequest, PreparePauseRequest, SetNetworkConfigRequest, SetTimeRequest,
};
use feos_proto::lifecycle_service::lifecycle_service_server::{
    LifecycleService, LifecycleServiceServer,
};
use feos_proto::lifecycle_service::{Hello, HostMessage};
use feos_proto::microvm::{
    GuestAttachedEvent, GuestDetachedEvent, GuestInfo, GuestNetworkConfig, VmEvent,
};
use feos_utils::vsock::{API_PORT, UPLINK_PORT};
use log::{debug, info, warn};
use prost::Message;
use prost_types::Any;
use std::collections::HashMap;
use std::io;
use std::os::unix::fs::PermissionsExt;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, MutexGuard, Weak};
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::net::UnixListener;
use tokio::sync::{mpsc, watch};
use tokio_stream::wrappers::{ReceiverStream, UnixListenerStream};
use tonic::transport::{Channel, Server};
use tonic::{Code, Request, Response, Status};
use uuid::Uuid;

/// How long one push may take.
pub(crate) const PUSH_TIMEOUT: Duration = Duration::from_secs(10);
/// How long a resumed guest may take to answer, its connect included. It
/// answers within about 100 ms unless it is broken, or its memory comes
/// from object storage: a VM restored from a snapshot written elsewhere
/// has its memory served by the UFFD handler page by page, and its first
/// answer can take longer than a connect's 5 s (`vsock::CONNECT_TIMEOUT`).
const RESUME_TIMEOUT: Duration = Duration::from_secs(30);
/// How often the clock of an attached guest is measured and corrected.
const CLOCK_PERIOD: Duration = Duration::from_secs(60);
/// How long one measurement and correction may take, its connect included.
const CLOCK_TIMEOUT: Duration = Duration::from_secs(5);
/// The `GetTime` calls of one measurement; the one with the shortest round
/// trip counts.
const CLOCK_SAMPLES: usize = 4;
/// A measurement whose shortest round trip took longer than this is dropped.
const CLOCK_MAX_ROUND_TRIP: Duration = Duration::from_millis(10);
/// A guest without `AdjustTime` is set when it is off by more than this.
const CLOCK_SET_THRESHOLD: Duration = Duration::from_millis(20);

/// Where the VM service listens for the guest's uplink.
pub fn uplink_path(vm_id: &str) -> PathBuf {
    let mut path = vsock_path(vm_id).into_os_string();
    path.push(format!("_{UPLINK_PORT}"));
    path.into()
}

#[derive(Default)]
struct Guest {
    network: Option<GuestNetworkConfig>,
    /// Unset until a guest attaches in the VM's current boot.
    info: Option<GuestInfo>,
    /// Counts attaches, so that the end of a call that another one replaced
    /// does not mark the guest detached.
    attach_id: u64,
    /// Ends the listener and every call on it.
    stop: Option<watch::Sender<bool>>,
}

/// The FeOS guests of all VMs.
pub struct Guests {
    guests: Mutex<HashMap<String, Guest>>,
    events: mpsc::Sender<VmEventWrapper>,
}

impl Guests {
    pub fn new(events: mpsc::Sender<VmEventWrapper>) -> Arc<Self> {
        Arc::new(Self {
            guests: Mutex::new(HashMap::new()),
            events,
        })
    }

    fn lock(&self) -> MutexGuard<'_, HashMap<String, Guest>> {
        // Every operation on the map is short and does not panic.
        self.guests.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Listens for the VM's guest and remembers what to push into it. A
    /// listener the VM had is replaced.
    pub fn listen(
        self: &Arc<Self>,
        vm_id: &str,
        network: Option<GuestNetworkConfig>,
    ) -> io::Result<()> {
        let path = uplink_path(vm_id);
        match std::fs::remove_file(&path) {
            Ok(()) => {}
            Err(e) if e.kind() == io::ErrorKind::NotFound => {}
            Err(e) => return Err(e),
        }
        let listener = UnixListener::bind(&path)?;
        // Firecracker connects as the VM's user. The jail's directory is
        // that user's alone.
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o666))?;

        let (stop, mut stopped) = watch::channel(false);
        let uplink = Uplink {
            vm_id: vm_id.to_string(),
            guests: Arc::downgrade(self),
            stop: stop.subscribe(),
        };
        let task_vm_id = vm_id.to_string();
        tokio::spawn(async move {
            let served = Server::builder()
                .add_service(LifecycleServiceServer::new(uplink))
                .serve_with_incoming_shutdown(UnixListenerStream::new(listener), async move {
                    let _ = stopped.wait_for(|stop| *stop).await;
                })
                .await;
            if let Err(e) = served {
                warn!("Guests ({task_vm_id}): The uplink listener failed: {e}");
            }
        });

        let mut guests = self.lock();
        let guest = guests.entry(vm_id.to_string()).or_default();
        if let Some(old) = guest.stop.replace(stop) {
            let _ = old.send(true);
        }
        guest.network = network;
        Ok(())
    }

    /// Stops listening for the VM's guest and forgets it.
    pub fn forget(&self, vm_id: &str) {
        if let Some(stop) = self.lock().remove(vm_id).and_then(|guest| guest.stop) {
            let _ = stop.send(true);
        }
        let _ = std::fs::remove_file(uplink_path(vm_id));
    }

    /// The VM boots anew: what its last guest reported is gone.
    pub fn reset(&self, vm_id: &str) {
        if let Some(guest) = self.lock().get_mut(vm_id) {
            guest.info = None;
        }
    }

    pub fn info(&self, vm_id: &str) -> Option<GuestInfo> {
        self.lock().get(vm_id).and_then(|guest| guest.info.clone())
    }

    /// After the VM resumed: once a FeOS guest answers, pushes the time and
    /// its network configuration. Does nothing for a VM whose guest never
    /// attached in this boot, which may be no FeOS, unless it was restored
    /// from a snapshot written elsewhere, whose guest attached there.
    pub async fn resumed(&self, vm_id: &str, from_elsewhere: bool) {
        if self.info(vm_id).is_none() && !from_elsewhere {
            return;
        }
        let result = match ping_within(vsock_path(vm_id), RESUME_TIMEOUT).await {
            Ok(_) => self.push(vm_id).await,
            Err(e) => Err(format!("the resumed guest does not answer: {e}")),
        };
        self.pushed(vm_id, result);
    }

    /// Remembers another network configuration for the VM's guest, and
    /// pushes it with the time if the guest is attached: it takes it at
    /// once. Returns whether it pushed; a guest that is not attached gets
    /// it when it attaches or resumes.
    pub async fn reconfigure(
        &self,
        vm_id: &str,
        network: GuestNetworkConfig,
    ) -> Result<bool, String> {
        let attached = match self.lock().get_mut(vm_id) {
            Some(guest) => {
                guest.network = Some(network);
                guest.info.as_ref().is_some_and(|info| info.attached)
            }
            // Its listener takes the network from the VM's record.
            None => false,
        };
        if !attached {
            return Ok(false);
        }
        let result = self.push(vm_id).await;
        self.pushed(vm_id, result.clone());
        result.map(|()| true)
    }

    /// Before a snapshot: has a FeOS guest empty its memory as far as it
    /// can, so that the snapshot gets smaller. Best effort.
    pub async fn prepare_pause(&self, vm_id: &str, free_page_reporting: bool) {
        if self.info(vm_id).is_none() {
            return;
        }
        let prepared = tokio::time::timeout(PUSH_TIMEOUT, async {
            let channel =
                feos_utils::vsock::channel_within(vsock_path(vm_id), API_PORT, PUSH_TIMEOUT)
                    .await
                    .map_err(|e| e.to_string())?;
            HostServiceClient::new(channel)
                .prepare_pause(PreparePauseRequest {
                    compact: true,
                    // Free page reporting reports small blocks too.
                    page_reporting_order: free_page_reporting.then_some(0),
                    ..Default::default()
                })
                .await
                .map_err(|status| status.message().to_string())
        })
        .await
        .unwrap_or_else(|_| Err(format!("no answer within {}s", PUSH_TIMEOUT.as_secs())));
        match prepared {
            Ok(response) => {
                let response = response.into_inner();
                info!(
                    "Guests ({vm_id}): The guest emptied its memory in {} ms: {}",
                    response.duration_ms,
                    response.steps.join(", ")
                );
            }
            Err(e) => warn!("Guests ({vm_id}): The guest could not empty its memory: {e}"),
        }
    }

    /// Pushes the wall time and the network configuration into the guest.
    async fn push(&self, vm_id: &str) -> Result<(), String> {
        let network = self
            .lock()
            .get(vm_id)
            .and_then(|guest| guest.network.clone());
        tokio::time::timeout(PUSH_TIMEOUT, async {
            let channel =
                feos_utils::vsock::channel_within(vsock_path(vm_id), API_PORT, PUSH_TIMEOUT)
                    .await
                    .map_err(|e| format!("connect to the guest: {e}"))?;
            let mut client = HostServiceClient::new(channel);
            client
                .set_time(SetTimeRequest {
                    time: Some(SystemTime::now().into()),
                })
                .await
                .map_err(|status| format!("SetTime: {}", status.message()))?;
            if let Some(network) = network {
                let response = client
                    .set_network_config(SetNetworkConfigRequest {
                        interface: network.interface,
                        ipv4_cidr: network.ipv4_cidr,
                        gateway: network.gateway,
                        dns: network.dns,
                        hostname: network.hostname,
                        ipv6_cidr: network.ipv6_cidr,
                        ipv6_gateway: network.ipv6_gateway,
                        routes: network
                            .routes
                            .into_iter()
                            .map(|route| NetworkRoute {
                                destination: route.destination,
                                gateway: route.gateway,
                                mtu: route.mtu,
                            })
                            .collect(),
                    })
                    .await
                    .map_err(|status| format!("SetNetworkConfig: {}", status.message()))?
                    .into_inner();
                // The push took effect; a container keeps stale resolvers.
                for failure in response.resolver_failed {
                    warn!(
                        "Guests ({vm_id}): Container {} keeps its resolvers: {}",
                        failure.container_id, failure.reason
                    );
                }
            }
            Ok(())
        })
        .await
        .unwrap_or_else(|_| Err(format!("no answer within {}s", PUSH_TIMEOUT.as_secs())))
    }

    /// Measures the guest's clock and has it correct the offset. A guest
    /// that has no `AdjustTime` yet is set instead, if it is far off.
    async fn keep_clock(&self, vm_id: &str) {
        let kept = tokio::time::timeout(CLOCK_TIMEOUT, async {
            let channel =
                feos_utils::vsock::channel_within(vsock_path(vm_id), API_PORT, CLOCK_TIMEOUT)
                    .await
                    .map_err(|e| format!("connect to the guest: {e}"))?;
            let mut client = HostServiceClient::new(channel);
            let (offset_ns, round_trip) = clock_offset(&mut client).await?;
            if round_trip > CLOCK_MAX_ROUND_TRIP {
                return Err(format!(
                    "the shortest round trip took {} µs",
                    round_trip.as_micros()
                ));
            }
            match client.adjust_time(AdjustTimeRequest { offset_ns }).await {
                Ok(response) => {
                    let response = response.into_inner();
                    if response.stepped {
                        info!(
                            "Guests ({vm_id}): Stepped the guest's clock, it was {} ms off",
                            offset_ns / 1_000_000
                        );
                    } else {
                        debug!(
                            "Guests ({vm_id}): The guest's clock was {} µs off; frequency {} ppb",
                            offset_ns / 1_000,
                            response.frequency_ppb
                        );
                    }
                    Ok(())
                }
                Err(status) if status.code() == Code::Unimplemented => {
                    if offset_ns.unsigned_abs() <= CLOCK_SET_THRESHOLD.as_nanos() as u64 {
                        return Ok(());
                    }
                    client
                        .set_time(SetTimeRequest {
                            time: Some(SystemTime::now().into()),
                        })
                        .await
                        .map_err(|status| format!("SetTime: {}", status.message()))?;
                    info!(
                        "Guests ({vm_id}): Set the clock of a guest without AdjustTime, it was {} ms off",
                        offset_ns / 1_000_000
                    );
                    Ok(())
                }
                Err(status) => Err(format!("AdjustTime: {}", status.message())),
            }
        })
        .await
        .unwrap_or_else(|_| Err(format!("no answer within {}s", CLOCK_TIMEOUT.as_secs())));
        // A pausing microVM does not answer; the next minute tries again.
        if let Err(e) = kept {
            debug!("Guests ({vm_id}): The guest's clock was not kept: {e}");
        }
    }

    fn pushed(&self, vm_id: &str, result: Result<(), String>) {
        let push_error = match result {
            Ok(()) => {
                info!("Guests ({vm_id}): Pushed the time and the configuration");
                String::new()
            }
            Err(e) => {
                warn!("Guests ({vm_id}): Push failed: {e}");
                e
            }
        };
        if let Some(info) = self.lock().get_mut(vm_id).and_then(|g| g.info.as_mut()) {
            info.push_error = push_error;
        }
    }

    /// Records an attach; returns its ID.
    fn attached(&self, vm_id: &str, hello: &Hello) -> u64 {
        let mut guests = self.lock();
        let guest = guests.entry(vm_id.to_string()).or_default();
        guest.attach_id += 1;
        guest.info = Some(GuestInfo {
            attached: true,
            boot_id: hello.boot_id.clone(),
            attach_seq: hello.attach_seq,
            feos_version: hello.feos_version.clone(),
            push_error: String::new(),
        });
        guest.attach_id
    }

    /// Records the end of a call; false if a newer one replaced it.
    fn detached(&self, vm_id: &str, attach_id: u64) -> bool {
        let mut guests = self.lock();
        match guests.get_mut(vm_id) {
            Some(guest) if guest.attach_id == attach_id => {
                if let Some(info) = guest.info.as_mut() {
                    info.attached = false;
                }
                true
            }
            _ => false,
        }
    }

    async fn emit(&self, vm_id: &str, type_name: &str, data: Vec<u8>) {
        let event = VmEvent {
            vm_id: vm_id.to_string(),
            id: Uuid::new_v4().to_string(),
            component_id: "guest-uplink".to_string(),
            data: Some(Any {
                type_url: format!("type.googleapis.com/feos.microvm.v1.{type_name}"),
                value: data,
            }),
        };
        let wrapper = VmEventWrapper {
            event,
            process_id: None,
        };
        if self.events.send(wrapper).await.is_err() {
            warn!("Guests ({vm_id}): Event channel closed");
        }
    }
}

/// The LifecycleService of one VM's uplink listener.
struct Uplink {
    vm_id: String,
    guests: Weak<Guests>,
    stop: watch::Receiver<bool>,
}

#[tonic::async_trait]
impl LifecycleService for Uplink {
    type AttachStream = ReceiverStream<Result<HostMessage, Status>>;

    async fn attach(
        &self,
        request: Request<Hello>,
    ) -> Result<Response<Self::AttachStream>, Status> {
        let guests = self
            .guests
            .upgrade()
            .ok_or_else(|| Status::unavailable("the VM service is shutting down"))?;
        let vm_id = self.vm_id.clone();
        let hello = request.into_inner();
        info!(
            "Guests ({vm_id}): Guest attached ({}, seq {}, boot {}, FeOS {})",
            hello.reason().as_str_name(),
            hello.attach_seq,
            hello.boot_id,
            hello.feos_version
        );
        let attach_id = guests.attached(&vm_id, &hello);
        let result = guests.push(&vm_id).await;
        guests.pushed(&vm_id, result);
        if let Some(info) = guests.info(&vm_id) {
            let event = GuestAttachedEvent { guest: Some(info) };
            guests
                .emit(&vm_id, "GuestAttachedEvent", event.encode_to_vec())
                .await;
        }

        // The host sends nothing; the stream ends when the guest goes or
        // the listener stops. Until then, the guest's clock is kept.
        let (tx, rx) = mpsc::channel(1);
        let mut stop = self.stop.clone();
        tokio::spawn(async move {
            let start = tokio::time::Instant::now() + CLOCK_PERIOD;
            let mut clock = tokio::time::interval_at(start, CLOCK_PERIOD);
            clock.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                let tick = tokio::select! {
                    _ = tx.closed() => false,
                    _ = stop.wait_for(|stop| *stop) => false,
                    _ = clock.tick() => true,
                };
                if !tick {
                    break;
                }
                guests.keep_clock(&vm_id).await;
            }
            drop(tx);
            if guests.detached(&vm_id, attach_id) {
                info!("Guests ({vm_id}): Guest detached");
                guests
                    .emit(
                        &vm_id,
                        "GuestDetachedEvent",
                        GuestDetachedEvent {}.encode_to_vec(),
                    )
                    .await;
            }
        });
        Ok(Response::new(ReceiverStream::new(rx)))
    }
}

/// How far the guest's clock is ahead of the node's, in nanoseconds, and the
/// round trip of the measurement: of `CLOCK_SAMPLES` calls, the one with the
/// shortest round trip, against the middle of it on the node's clock.
async fn clock_offset(client: &mut HostServiceClient<Channel>) -> Result<(i64, Duration), String> {
    let mut best: Option<(i64, Duration)> = None;
    for _ in 0..CLOCK_SAMPLES {
        let sent = SystemTime::now();
        let time = client
            .get_time(GetTimeRequest {})
            .await
            .map_err(|status| format!("GetTime: {}", status.message()))?
            .into_inner()
            .time
            .ok_or("GetTime returned no time")?;
        let received = SystemTime::now();
        let round_trip = received.duration_since(sent).unwrap_or_default();
        if best.is_some_and(|(_, best)| best <= round_trip) {
            continue;
        }
        let middle = sent + round_trip / 2;
        let guest = Duration::new(time.seconds.max(0) as u64, time.nanos.max(0) as u32);
        let node = middle.duration_since(UNIX_EPOCH).unwrap_or_default();
        let offset_ns = guest.as_nanos() as i64 - node.as_nanos() as i64;
        best = Some((offset_ns, round_trip));
    }
    best.ok_or_else(|| "no measurement".to_string())
}

/// Pings the FeOS guest behind the vsock socket `uds`, within `deadline`,
/// its connect included: the connect has the whole deadline, not a
/// connect's 5 s.
async fn ping_within(uds: PathBuf, deadline: Duration) -> Result<(), String> {
    tokio::time::timeout(deadline, async {
        let channel = feos_utils::vsock::channel_within(uds, API_PORT, deadline)
            .await
            .map_err(|e| e.to_string())?;
        HostServiceClient::new(channel)
            .ping(PingRequest {})
            .await
            .map(|_| ())
            .map_err(|status| status.message().to_string())
    })
    .await
    .unwrap_or_else(|_| {
        Err(format!(
            "no answer within {}",
            feos_utils::vsock::seconds(deadline)
        ))
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A resumed guest that answers its connect after a connect's 5 s, as
    /// one whose memory the UFFD handler serves from object storage, is
    /// still waited for: the resume's bound covers the connect.
    #[tokio::test]
    async fn a_resumed_guest_may_take_longer_than_a_connect() {
        use tokio::io::{AsyncBufReadExt, AsyncWriteExt};
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("v.sock");
        let listener = UnixListener::bind(&path).unwrap();
        let late = feos_utils::vsock::CONNECT_TIMEOUT + Duration::from_secs(1);
        let vmm = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut stream = tokio::io::BufReader::new(stream);
            let mut line = String::new();
            stream.read_line(&mut line).await.unwrap();
            tokio::time::sleep(late).await;
            // Connected; the guest then goes, which the ping says.
            stream.get_mut().write_all(b"OK 1\n").await.unwrap();
        });
        let err = ping_within(path, RESUME_TIMEOUT).await.unwrap_err();
        vmm.await.unwrap();
        assert!(
            !err.contains("did not answer") && !err.contains("no answer"),
            "the connect gave up first: {err}"
        );
    }

    #[test]
    fn the_uplink_is_beside_the_vsock_socket() {
        assert_eq!(
            uplink_path("vm"),
            PathBuf::from("/var/lib/feos/vms/firecracker/vm/root/v.sock_10001")
        );
    }

    #[tokio::test]
    async fn another_network_goes_to_the_attached_guest() {
        let (events, _) = mpsc::channel(1);
        let guests = Guests::new(events);
        let network = |address: &str| GuestNetworkConfig {
            interface: "eth0".to_string(),
            ipv6_cidr: format!("{address}/128"),
            ipv6_gateway: "fe80::1".to_string(),
            ..Default::default()
        };
        // A VM nobody listens for: its listener takes the record's.
        assert_eq!(
            guests.reconfigure("vm", network("2001:db8::2")).await,
            Ok(false)
        );
        assert!(guests.lock().get("vm").is_none());

        // A guest that is not attached gets it when it attaches.
        guests.lock().insert(
            "vm".to_string(),
            Guest {
                network: Some(network("2001:db8::2")),
                ..Default::default()
            },
        );
        assert_eq!(
            guests.reconfigure("vm", network("2001:db8::3")).await,
            Ok(false)
        );
        assert_eq!(guests.lock()["vm"].network, Some(network("2001:db8::3")));

        // An attached one is pushed it at once; here none answers.
        guests.lock().get_mut("vm").unwrap().info = Some(GuestInfo {
            attached: true,
            ..Default::default()
        });
        let pushed = guests.reconfigure("vm", network("2001:db8::4")).await;
        let error = pushed.expect_err("no guest answers");
        assert_eq!(guests.lock()["vm"].network, Some(network("2001:db8::4")));
        assert_eq!(guests.info("vm").unwrap().push_error, error);
    }
}
