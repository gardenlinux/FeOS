// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

#[cfg(feature = "host")]
pub mod cgroup;
#[cfg(feature = "host")]
mod fleet;
#[cfg(feature = "guest")]
mod guest;
mod setup;
mod task_supervisor;
#[cfg(feature = "guest")]
mod volumes;

use anyhow::{Context, Result};
use container_service::api::ContainerApiHandler;
#[cfg(feature = "host")]
use feos_proto::network_service::{
    mesh_service_server::MeshServiceServer, name_service_server::NameServiceServer,
    security_group_service_server::SecurityGroupServiceServer,
    traffic_service_server::TrafficServiceServer,
};
#[cfg(feature = "host")]
use feos_proto::pod_service::pod_service_server::PodServiceServer;
#[cfg(feature = "host")]
use feos_proto::volume_service::volume_service_server::VolumeServiceServer;
use feos_proto::{
    container_service::container_service_server::ContainerServiceServer,
    host_service::host_service_server::HostServiceServer,
    image_service::image_service_server::ImageServiceServer,
};
use host_service::{api::HostApiHandler, RestartSignal};
use image_service::{api::ImageApiHandler, IMAGE_SERVICE_SOCKET};
use log::{error, info, warn};
#[cfg(feature = "host")]
use network_service::{NameTables, SecurityGroups};
use nix::unistd::{chown, Group, Uid};
#[cfg(feature = "host")]
use pod_service::{forward::ForwardLayer, Pods, Volumes, STOP_RECORD};
use setup::*;
use std::os::linux::net::SocketAddrExt;
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{SocketAddr, UnixDatagram};
use std::path::Path;
#[cfg(feature = "host")]
use std::sync::{Arc, OnceLock};
#[cfg(feature = "host")]
use std::time::Duration;
use task_service::TASK_SERVICE_SOCKET;
use tokio::{fs, net::UnixListener, sync::mpsc};
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::server::Router;
use tonic::transport::Server;
use tower::layer::util::Identity;
#[cfg(feature = "host")]
use tower::layer::util::Stack;

/// The public gRPC API on a local Unix socket, served by feosd and
/// feos-guest. feos-guest serves the same API on vsock, feosd on TCP if
/// asked to.
pub const API_SOCKET: &str = "/run/feos/api.sock";
/// The group that may use the API socket, if it exists. The feosd package
/// creates it.
const API_GROUP: &str = "feos";
/// How long the pods of a stop may take to resume before feosd connects to
/// the control plane all the same; those still resuming go on.
#[cfg(feature = "host")]
const RESUME_WAIT: Duration = Duration::from_secs(120);
/// With a control plane, the pods of a stop longer ago than this stay
/// suspended: the control plane counts a node's sandboxes as lost after
/// its `--lost-grace`, 10 minutes by default, and may bring them back
/// elsewhere.
#[cfg(feature = "host")]
const RESUME_WITHIN: Duration = Duration::from_secs(300);
/// How long a stop may take before feosd is killed, by default
/// (`feosd --stop-grace`): feos-node's terminationGracePeriodSeconds.
#[cfg(feature = "host")]
pub const STOP_GRACE: Duration = Duration::from_secs(300);
/// How long the node connections' calls in flight may take at most when
/// feosd stops, before the pods are suspended; and no more than a tenth of
/// the stop's grace, so that the suspends fit in the rest.
#[cfg(feature = "host")]
const CALLS_WAIT: Duration = Duration::from_secs(30);
/// What a stop leaves of its grace when it gives up on a pod whose VM is
/// busy: to record the pods it suspended and exit.
#[cfg(feature = "host")]
const STOP_MARGIN: Duration = Duration::from_secs(10);

/// How long the calls in flight on the node connections get when feosd
/// stops, of the stop's `grace`.
#[cfg(feature = "host")]
fn calls_wait(grace: Duration) -> Duration {
    CALLS_WAIT.min(grace / 10)
}

/// How feosd or feos-guest serves.
#[derive(Debug)]
pub struct ServerOptions {
    /// This process is the one an upgrade exec'd: the one-time setup ran.
    pub restarted_after_upgrade: bool,
    /// UpgradeFeosBinary may replace this binary. A package manager owns
    /// feosd's binary unless feos-host-init runs it.
    pub self_upgrade: bool,
    /// Serves the API on TCP too, without authentication: for labs.
    #[cfg(feature = "host")]
    pub tcp: Option<std::net::SocketAddr>,
    /// The pods' network (ADR 0010).
    #[cfg(feature = "host")]
    pub network: pod_service::network::Config,
    /// The sockets handed to `feosd` (`feos_utils::listen_fds`): the
    /// resolver's, which outlive it.
    #[cfg(feature = "host")]
    pub dns_sockets: Vec<(std::os::fd::OwnedFd, String)>,
    /// diskd's socket, which the pods' disks come from (ADR 0011); its
    /// default when unset.
    #[cfg(feature = "host")]
    pub diskd_socket: Option<std::path::PathBuf>,
    /// The control plane (ADR 0015); none for a node of its own.
    #[cfg(feature = "host")]
    pub fleet: Option<fleet_client::Config>,
    /// SIGTERM suspends the running pods, for the next feosd to resume:
    /// for a feosd whose microVMs end with it (ADR 0020).
    #[cfg(feature = "host")]
    pub suspend_on_stop: bool,
    /// How long a stop may take before feosd is killed: `STOP_GRACE` by
    /// default.
    #[cfg(feature = "host")]
    pub stop_grace: Duration,
    /// How the pods' feos-authn processes start (ADR 0014, ADR 0028).
    #[cfg(feature = "host")]
    pub authn: pod_service::authn::Config,
}

impl Default for ServerOptions {
    fn default() -> Self {
        Self::new(false, false)
    }
}

impl ServerOptions {
    /// Options without the TCP listener, and pods without a network.
    pub fn new(restarted_after_upgrade: bool, self_upgrade: bool) -> Self {
        Self {
            restarted_after_upgrade,
            self_upgrade,
            #[cfg(feature = "host")]
            tcp: None,
            #[cfg(feature = "host")]
            network: Default::default(),
            #[cfg(feature = "host")]
            dns_sockets: Vec::new(),
            #[cfg(feature = "host")]
            diskd_socket: None,
            #[cfg(feature = "host")]
            fleet: None,
            #[cfg(feature = "host")]
            suspend_on_stop: false,
            #[cfg(feature = "host")]
            stop_grace: STOP_GRACE,
            #[cfg(feature = "host")]
            authn: pod_service::authn::Config::beside_feosd(),
        }
    }
}

pub async fn run_server(options: ServerOptions) -> Result<()> {
    println!(
        "
    ███████╗███████╗ ██████╗ ███████╗
    ██╔════╝██╔════╝██╔═══██╗██╔════╝
    █████╗  █████╗  ██║   ██║███████╗
    ██╔══╝  ██╔══╝  ██║   ██║╚════██║
    ██║     ███████╗╚██████╔╝███████║
    ╚═╝     ╚══════╝ ╚═════╝ ╚══════╝
                 v{}
    ",
        feos_utils::version::full_version_string()
    );

    let log_handle = feos_utils::feos_logger::Builder::new()
        .filter_level(log::LevelFilter::Info)
        .max_history(150)
        .init()
        .expect("Failed to initialize feos_logger");

    if !Uid::current().is_root() {
        warn!("Not running as root! (uid: {})", Uid::current());
    }

    // feos-guest is PID 1 of its VM and sets it up; feosd sets nothing up.
    #[cfg(feature = "guest")]
    if std::process::id() == 1 {
        feos_utils::host::power::take_over_ctrl_alt_del();
        if options.restarted_after_upgrade {
            info!("Main: Skipping one-time initialization on restart after upgrade.");
        } else {
            perform_first_boot_initialization();
        }
    }

    let (restart_tx, mut restart_rx) = mpsc::channel::<RestartSignal>(1);

    #[cfg(feature = "host")]
    let vms = initialize_vm_service(&setup_database().await?).await?;
    #[cfg(feature = "host")]
    let (pods, volumes, groups, names, traffic) = initialize_pod_service(
        vms,
        options.network,
        options.dns_sockets,
        options.diskd_socket,
        options.authn,
    )
    .await?;
    let services = Services {
        #[cfg(feature = "host")]
        pods,
        #[cfg(feature = "host")]
        volumes,
        #[cfg(feature = "host")]
        groups,
        #[cfg(feature = "host")]
        names,
        #[cfg(feature = "host")]
        traffic,
        container: initialize_container_service().await?,
        host: initialize_host_service(options.self_upgrade.then_some(restart_tx), log_handle),
        image: initialize_image_service().await?,
        #[cfg(feature = "guest")]
        guest: guest::GuestServices::new(),
    };
    task_supervisor::start().await;

    // Every listener serves the same services.
    let api_uds_stream = bind_unix_socket(API_SOCKET).await?;
    allow_group(API_SOCKET);
    let api_unix_socket_server = services.router().serve_with_incoming(api_uds_stream);
    info!("Main: Public gRPC Server listening on Unix socket {API_SOCKET}");

    #[cfg(feature = "guest")]
    let vsock_server = services
        .router()
        .serve_with_incoming(guest::vsock_incoming()?);
    #[cfg(feature = "guest")]
    info!(
        "Main: Public gRPC Server listening on vsock port {}",
        feos_utils::vsock::API_PORT
    );
    #[cfg(not(feature = "guest"))]
    let vsock_server = std::future::pending::<Result<(), tonic::transport::Error>>();

    #[cfg(feature = "host")]
    let tcp_server = {
        let incoming = match options.tcp {
            Some(addr) => {
                let listener = tokio::net::TcpListener::bind(addr)
                    .await
                    .with_context(|| format!("bind {addr}"))?;
                info!("Main: Public gRPC Server listening on {addr}, without authentication");
                Some(tokio_stream::wrappers::TcpListenerStream::new(listener))
            }
            None => None,
        };
        let router = services.router();
        async move {
            match incoming {
                Some(incoming) => router.serve_with_incoming(incoming).await,
                None => std::future::pending().await,
            }
        }
    };
    #[cfg(not(feature = "host"))]
    let tcp_server = std::future::pending::<Result<(), tonic::transport::Error>>();

    let image_uds_stream = bind_unix_socket(IMAGE_SERVICE_SOCKET).await?;
    let image_unix_socket_server = Server::builder()
        .add_service(services.image.clone())
        .serve_with_incoming(image_uds_stream);
    info!("Main: Internal ImageService listening on Unix socket {IMAGE_SERVICE_SOCKET}");

    // The listeners are bound: calls land in their backlog from here on.
    notify_ready();
    // Tell the host that the guest is ready.
    #[cfg(feature = "guest")]
    tokio::spawn(guest::run_uplink());

    // The pods a stop of feosd suspended are resumed first, for a while,
    // then the control plane's connections come up, once the listeners
    // serve: the node connections serve the same services. The control
    // plane thus finds the pods running as it left them (node.md, "On
    // Kubernetes").
    #[cfg(feature = "host")]
    let fleet_slot: Arc<OnceLock<fleet_client::Fleet>> = Arc::default();
    #[cfg(feature = "host")]
    let node_calls = fleet_client::NodeCalls::new();
    #[cfg(feature = "host")]
    let node_server = {
        let pods = services.pods.clone();
        // The calls run here, on feosd's runtime; the connections on the
        // control plane's threads.
        let router = services.router_with(fleet_client::server(
            tokio::runtime::Handle::current(),
            &node_calls,
        ));
        let calls = node_calls.clone();
        let slot = fleet_slot.clone();
        let config = options.fleet.clone();
        async move {
            // A record older than that is the control plane's to act on.
            let within = config.is_some().then_some(RESUME_WITHIN);
            let resuming = {
                let pods = pods.clone();
                tokio::spawn(
                    async move { pods.resume_after_stop(Path::new(STOP_RECORD), within).await },
                )
            };
            if tokio::time::timeout(RESUME_WAIT, resuming).await.is_err() {
                warn!(
                    "Main: The pods are still resuming after {}s; connecting to the control \
                     plane all the same",
                    RESUME_WAIT.as_secs()
                );
            }
            // A stop that came meanwhile: nothing more to connect.
            let started = match config {
                Some(config) if !pods.stopping() => {
                    fleet::start(config, pods.clone(), calls.clone())
                        .map_err(|e| error!("Fleet: Not connecting to the control plane: {e}"))
                        .ok()
                }
                _ => None,
            };
            match started {
                // Ends only with a stop, whatever becomes of the dialers.
                Some((fleet, incoming)) => {
                    // On the control plane's threads, as the rest of it: the
                    // connections answer whatever feosd's own threads do.
                    let serving = fleet.serve(router, incoming);
                    let _ = slot.set(fleet);
                    let served = serving
                        .await
                        .map_err(|e| e.to_string())?
                        .map_err(|e| e.to_string());
                    if calls.stopping() {
                        // feosd stops: the stop ends it.
                        return std::future::pending().await;
                    }
                    served
                }
                None => std::future::pending().await,
            }
        }
    };
    #[cfg(not(feature = "host"))]
    let node_server = std::future::pending::<Result<(), String>>();
    let mut node_serving = true;

    // SIGTERM: with --suspend-on-stop, feosd refuses calls that change
    // pods, tells the control plane that it stops, closes the node
    // connections once their calls are done or a bound passed, and
    // suspends the running pods for the next feosd to resume. Without it,
    // with a control plane, it tells the control plane that it stops and
    // closes the node connections so before it ends; without either, it
    // ends as the signal ends it.
    #[cfg(feature = "host")]
    let terminated = {
        let slot = fleet_slot.clone();
        let pods = services.pods.clone();
        let suspend = options.suspend_on_stop;
        let grace = options.stop_grace;
        let signal = (suspend || options.fleet.is_some()).then(|| {
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
                .map_err(|e| warn!("Main: Failed to handle SIGTERM: {e}"))
                .ok()
        });
        async move {
            let Some(mut signal) = signal.flatten() else {
                return std::future::pending().await;
            };
            signal.recv().await;
            let stopped_at = tokio::time::Instant::now();
            info!("Main: SIGTERM: feosd stops, within {}s", grace.as_secs());
            if suspend {
                pods.begin_stop();
            }
            if let Some(fleet) = slot.get() {
                fleet.stopping("feosd stops").await;
                fleet.stop(calls_wait(grace)).await;
            }
            if suspend {
                // A pod whose VM is busy is tried again until then.
                let until = stopped_at + grace.saturating_sub(STOP_MARGIN);
                pods.suspend_for_stop(Path::new(STOP_RECORD), until).await;
            }
        }
    };
    #[cfg(not(feature = "host"))]
    let terminated = std::future::pending::<()>();

    tokio::pin!(
        api_unix_socket_server,
        vsock_server,
        tcp_server,
        image_unix_socket_server,
        node_server,
        terminated
    );

    // Serve until one of the servers stops. A failed upgrade leaves the
    // running binary serving.
    loop {
        tokio::select! {
            res = &mut api_unix_socket_server => {
                if let Err(e) = res {
                    error!("API unix socket server failed: {e}");
                }
                break;
            },
            res = &mut vsock_server => {
                if let Err(e) = res {
                    error!("vsock server failed: {e}");
                }
                break;
            },
            res = &mut tcp_server => {
                if let Err(e) = res {
                    error!("TCP server failed: {e}");
                }
                break;
            },
            res = &mut image_unix_socket_server => {
                if let Err(e) = res {
                    error!("Image unix socket server failed: {e}");
                }
                break;
            },
            // The node connections never end feosd.
            res = &mut node_server, if node_serving => {
                node_serving = false;
                error!("Fleet: The node connections' server stopped: {res:?}");
            },
            () = &mut terminated => return Ok(()),
            Some(RestartSignal(new_binary_path)) = restart_rx.recv() => {
                #[cfg(feature = "host")]
                if let Some(fleet) = fleet_slot.get() {
                    fleet.stopping("feosd restarts for an upgrade").await;
                }
                if let Err(e) = handle_upgrade(&new_binary_path) {
                    error!("Upgrade failed: {e}");
                }
            }
        };
    }

    anyhow::bail!("a gRPC server stopped serving")
}

/// The public API's services, one instance each for every listener.
struct Services {
    #[cfg(feature = "host")]
    pods: Pods,
    #[cfg(feature = "host")]
    volumes: Volumes,
    #[cfg(feature = "host")]
    groups: SecurityGroups,
    #[cfg(feature = "host")]
    names: NameTables,
    #[cfg(feature = "host")]
    traffic: traffic_service::TrafficApi,
    container: ContainerServiceServer<ContainerApiHandler>,
    host: HostServiceServer<HostApiHandler>,
    image: ImageServiceServer<ImageApiHandler>,
    #[cfg(feature = "guest")]
    guest: guest::GuestServices,
}

/// feosd forwards the calls for a pod's guest (ADR 0004), below the layers
/// `L` of a listener's server.
#[cfg(feature = "host")]
type Layered<L> = Stack<ForwardLayer, L>;
#[cfg(not(feature = "host"))]
type Layered<L> = L;
type Layers = Layered<Identity>;

impl Services {
    fn router(&self) -> Router<Layers> {
        self.router_with(Server::builder())
    }

    /// The services on `server`, as a listener configures it.
    fn router_with<L: Clone>(&self, server: Server<L>) -> Router<Layered<L>> {
        #[cfg(feature = "host")]
        let mut server = server.layer(ForwardLayer::new(self.pods.clone()));
        #[cfg(not(feature = "host"))]
        let mut server = server;
        let router = server
            .add_service(self.container.clone())
            .add_service(self.host.clone())
            .add_service(self.image.clone());
        #[cfg(feature = "host")]
        let router = router
            .add_service(PodServiceServer::new(self.pods.clone()))
            .add_service(VolumeServiceServer::new(self.volumes.clone()))
            .add_service(SecurityGroupServiceServer::new(self.groups.clone()))
            .add_service(NameServiceServer::new(self.names.clone()))
            .add_service(MeshServiceServer::new(self.pods.clone()))
            .add_service(TrafficServiceServer::new(self.traffic.clone()));
        #[cfg(feature = "guest")]
        let router = self.guest.add_to(router);
        router
    }
}

/// Lets the group `feos` use a socket, if the group exists.
fn allow_group(path: &str) {
    let Ok(Some(group)) = Group::from_name(API_GROUP) else {
        return;
    };
    if let Err(e) = chown(path, None, Some(group.gid)) {
        warn!("Main: Failed to give {path} to group {API_GROUP}: {e}");
        return;
    }
    if let Err(e) = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o660)) {
        warn!("Main: Failed to open {path} to group {API_GROUP}: {e}");
    }
}

/// Tells systemd that feosd serves (sd_notify READY=1): one datagram to
/// $NOTIFY_SOCKET, without libsystemd. Nothing to do without it.
fn notify_ready() {
    let Some(socket) = std::env::var_os("NOTIFY_SOCKET") else {
        return;
    };
    let socket = socket.as_bytes();
    // "@" names a socket in the abstract namespace.
    let address = match socket.strip_prefix(b"@") {
        Some(name) => SocketAddr::from_abstract_name(name),
        None => SocketAddr::from_pathname(Path::new(std::ffi::OsStr::from_bytes(socket))),
    };
    let sent = address.and_then(|address| {
        let datagram = UnixDatagram::unbound()?;
        datagram.send_to_addr(b"READY=1", &address)
    });
    if let Err(e) = sent {
        warn!("Main: Failed to notify systemd: {e}");
    }
}

/// The task service process, `feosd --task-service` or `feos-guest
/// --task-service`, which run_server starts.
pub async fn run_task_service() -> Result<()> {
    feos_utils::feos_logger::Builder::new()
        .filter_level(log::LevelFilter::Info)
        .max_history(150)
        .init()
        .context("logger")?;
    task_service::server::serve(TASK_SERVICE_SOCKET)
        .await
        .map_err(|e| anyhow::anyhow!("task service: {e}"))
}

/// Binds a Unix socket, creating its directory and replacing a stale socket.
async fn bind_unix_socket(path: &str) -> Result<UnixListenerStream> {
    if let Some(dir) = Path::new(path).parent() {
        fs::create_dir_all(dir)
            .await
            .with_context(|| format!("create {}", dir.display()))?;
    }
    fs::remove_file(path).await.ok();
    let listener = UnixListener::bind(path).with_context(|| format!("bind {path}"))?;
    Ok(UnixListenerStream::new(listener))
}

#[cfg(all(test, feature = "host"))]
mod tests {
    use super::*;

    /// The calls in flight get a tenth of the stop's grace, 30 s at most:
    /// feos-node's 300 s leave the suspends 270 s.
    #[test]
    fn the_calls_in_flight_get_a_tenth_of_the_grace() {
        assert_eq!(calls_wait(STOP_GRACE), Duration::from_secs(30));
        assert_eq!(calls_wait(Duration::from_secs(90)), Duration::from_secs(9));
        assert_eq!(calls_wait(Duration::from_secs(3600)), CALLS_WAIT);
    }
}
