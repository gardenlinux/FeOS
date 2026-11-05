// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods' network (ADR 0010, 0012, 0013). Each pod has an IPv6 address
//! of the node's prefix, its address on the network, and an IPv4 address
//! of the node's link-local range for leaving through masquerade. Both are
//! routed to its TAP device, a link of its own between the node and the
//! pod, on which the node is `fe80::1` and answers ARP for `169.254.0.1`
//! by proxy. Names go through feosd's resolver, which answers those of the
//! pods' name tables itself; the pods' security groups become nftables
//! rules; and their traffic becomes records. Where the
//! network routes the node no prefix, the node's prefix is a /96 of the
//! cluster's mesh (ADR 0017), which the control plane gives it, maybe after
//! feosd started: the network comes up then.

mod addresses;
mod devices;
mod mesh;
mod rules;

pub use mesh::PORT as MESH_PORT;
pub use mesh::{Assignment, Config as MeshConfig, DEVICE as MESH_DEVICE, MTU as MESH_MTU};
pub use rules::{HTTP_PORTS, OTLP_PORTS, TABLE};

use addresses::{Ipv4Addresses, Ipv6Addresses};
use devices::PodLink;
use dns_service::{HttpNames, Resolver};
use feos_proto::network_service::{
    traffic_record, Mesh as MeshInfo, PutMeshPeersRequest, TrafficRecord, UpdateMeshPeersRequest,
};
use feos_proto::pod_service::{PodInfo, PodNetwork, PodSpec};
use feos_utils::listen_fds;
use feos_utils::nft::{self, Family, Table};
use log::{error, info, warn};
use mesh::Mesh;
use network_service::{compile, delta, Delta, NameTables, Policy, Prefix, SecurityGroups};
use rtnetlink::Handle;
use std::collections::{HashMap, HashSet};
use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::os::fd::{AsFd, OwnedFd};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard, OnceLock, PoisonError, RwLock};
use std::time::{Duration, Instant, SystemTime};
use tokio::net::{TcpListener, UdpSocket};
use tokio::sync::{broadcast, watch};
use tonic::Status;
use traffic_service::Collector;

/// The node's address on every pod's TAP device: the pods' IPv6 gateway.
pub const GATEWAY: Ipv6Addr = Ipv6Addr::new(0xfe80, 0, 0, 0, 0, 0, 0, 1);
/// The pods' IPv4 gateway, on their link although outside their /32. The
/// node answers ARP for it on every TAP device by proxy and never holds
/// it: where it is the gateway of the node's own network, as in some
/// clouds, the node would lose its own.
pub const GATEWAY4: Ipv4Addr = Ipv4Addr::new(169, 254, 0, 1);
/// The pods' IPv4 addresses by default (`--pod-ipv4-range`): 4096, of
/// which the first and the last are left out.
pub const POD_IPV4_RANGE: (Ipv4Addr, u8) = (Ipv4Addr::new(169, 254, 16, 0), 20);
/// The node's address for its pods' spans by default (`--otlp-address`,
/// docs/architecture/traces.md).
pub const OTLP_ADDRESS: Ipv4Addr = Ipv4Addr::new(169, 254, 169, 1);
/// The metadata service's address in many clouds, which the node does not
/// take for its own.
const METADATA: Ipv4Addr = Ipv4Addr::new(169, 254, 169, 254);
/// The device that holds the resolver's address, and OTLP's.
pub const DNS_DEVICE: &str = "feos-dns";
/// The node's default upstream resolvers: Google's and Cloudflare's.
pub const DNS_UPSTREAMS: [&str; 4] = [
    "2001:4860:4860::8888",
    "2606:4700:4700::1111",
    "8.8.8.8",
    "1.1.1.1",
];
/// Records kept for the calls of the traffic API that fall behind.
pub const RECORDS: usize = 4096;
/// The bridge of the first version of the network, with IPv4 on it.
const OLD_BRIDGE: &str = "feos-pods";
/// The NAT64 version's translator: its device, with its routes and its
/// program, the maps it pinned, and the counter of its IPv4 pool in the
/// pods' directory.
const OLD_NAT64_DEVICE: &str = "feos-nat64";
const OLD_NAT64_PINS: &str = "/sys/fs/bpf/feos/nat64";
const OLD_NAT64_COUNTER: &str = "next-address";
/// How often the network's setup is tried, and how long apart.
const SET_UP_ATTEMPTS: u32 = 3;
const SET_UP_PAUSE: Duration = Duration::from_secs(1);
const DNS_PORT: u16 = 53;

/// How the node builds the pods' network: feosd's flags.
#[derive(Clone, Debug, PartialEq)]
pub struct Config {
    /// The prefix the network routes to the node (`--pod-prefix`).
    /// Without it or a mesh, pods with a network fail.
    pub pod_prefix: Option<(Ipv6Addr, u8)>,
    /// The cluster's mesh, whose /96 is the node's prefix when it has no
    /// `--pod-prefix`: with a control plane.
    pub mesh: Option<MeshConfig>,
    /// The pods' IPv4 addresses, a /32 of it each (`--pod-ipv4-range`).
    pub pod_ipv4_range: (Ipv4Addr, u8),
    /// The resolver's address on every node (`--dns-address`).
    pub dns_address: Ipv4Addr,
    /// Where pods send their spans, on every node (`--otlp-address`).
    pub otlp_address: Ipv4Addr,
    /// The upstreams of pods that name none (`--dns-upstream`).
    pub dns_upstreams: Vec<IpAddr>,
    /// What no pod reaches (`--deny-prefix`), of either family.
    pub deny_prefixes: Vec<Prefix>,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            pod_prefix: None,
            mesh: None,
            pod_ipv4_range: POD_IPV4_RANGE,
            dns_address: dns_service::ADDRESS,
            otlp_address: OTLP_ADDRESS,
            dns_upstreams: DNS_UPSTREAMS
                .iter()
                .filter_map(|address| address.parse().ok())
                .collect(),
            deny_prefixes: Vec::new(),
        }
    }
}

impl Config {
    /// Checks what the flags say together: the node holds the resolver's
    /// address and OTLP's, so each must be one of a host and neither the
    /// pods' gateway nor in their range; OTLP's is neither the resolver's
    /// nor a metadata service's nor denied; and the range must not hold
    /// the gateway.
    pub fn check(&self) -> Result<(), String> {
        let (dns, otlp) = (self.dns_address, self.otlp_address);
        for (what, address) in [("DNS", dns), ("OTLP", otlp)] {
            if address.is_unspecified()
                || address.is_loopback()
                || address.is_multicast()
                || address.is_broadcast()
            {
                return Err(format!(
                    "the {what} address {address} is not one a host has"
                ));
            }
            if address == GATEWAY4 {
                return Err(format!(
                    "the {what} address {address} is the pods' gateway, which the node must not hold"
                ));
            }
        }
        if otlp == dns {
            return Err(format!("the OTLP address {otlp} is the DNS address"));
        }
        if otlp == METADATA {
            return Err(format!(
                "the OTLP address {otlp} is a metadata service's in many clouds"
            ));
        }
        let host = Prefix::V4(otlp, 32);
        if let Some(denied) = self
            .deny_prefixes
            .iter()
            .find(|denied| denied.contains(&host))
        {
            return Err(format!(
                "the OTLP address {otlp} is in the denied prefix {denied}"
            ));
        }
        let (range, len) = self.pod_ipv4_range;
        let range = Prefix::V4(range, len);
        for (what, address) in [
            ("the DNS address", dns),
            ("the OTLP address", otlp),
            ("the pods' gateway", GATEWAY4),
        ] {
            if range.contains(&Prefix::V4(address, 32)) {
                return Err(format!(
                    "the pods' IPv4 range {range} holds {what} {address}"
                ));
            }
        }
        Ok(())
    }
}

/// `--pod-prefix`: an IPv6 prefix with room for pods.
pub fn parse_pod_prefix(s: &str) -> Result<(Ipv6Addr, u8), String> {
    match s.parse::<Prefix>()? {
        Prefix::V6(address, len) if (1..=120).contains(&len) => Ok((address, len)),
        Prefix::V6(..) => Err(format!("pod prefix {s}: from /1 to /120")),
        Prefix::V4(..) => Err(format!(
            "pod prefix {s}: an IPv6 prefix; the pods' IPv4 range is --pod-ipv4-range"
        )),
    }
}

/// `--pod-ipv4-range`: an IPv4 prefix with room for pods besides its first
/// and last address.
pub fn parse_pod_ipv4_range(s: &str) -> Result<(Ipv4Addr, u8), String> {
    match s.parse::<Prefix>()? {
        Prefix::V4(address, len) if (1..=30).contains(&len) => Ok((address, len)),
        Prefix::V4(..) => Err(format!("pods' IPv4 range {s}: from /1 to /30")),
        Prefix::V6(..) => Err(format!("pods' IPv4 range {s}: an IPv4 prefix")),
    }
}

/// The unique local addresses (RFC 4193), which no network routes to the
/// internet.
pub const ULA: (Ipv6Addr, u8) = (Ipv6Addr::new(0xfc00, 0, 0, 0, 0, 0, 0, 0), 7);

/// A pod's place on the network, for its microVM.
#[derive(Clone, Debug, PartialEq)]
pub struct Attachment {
    pub tap: String,
    pub mac: [u8; 6],
    pub ipv6: Ipv6Addr,
    pub ipv4: Ipv4Addr,
    /// The resolver.
    pub dns: Ipv4Addr,
    /// The routes that replace the pod's default route over IPv6: none
    /// for a global address, one for the ULAs over `fe80::1` for a ULA
    /// (ADR 0017).
    pub routes: Vec<PodRoute>,
}

/// A route of a pod's, via a gateway on its link.
#[derive(Clone, Debug, PartialEq)]
pub struct PodRoute {
    pub destination: Prefix,
    pub gateway: IpAddr,
    /// 0 is the link's.
    pub mtu: u32,
}

/// The routes of a pod with `address` instead of its default route over
/// IPv6: a ULA has no route to the internet, so that a pod with one, given
/// none, takes IPv4 there at once (RFC 6724, rule 1). `mtu` is that of the
/// node's way to the other ULAs, 0 for the link's.
pub fn routes(address: Ipv6Addr, mtu: u32) -> Vec<PodRoute> {
    let (ula, len) = ULA;
    if !Prefix::V6(ula, len).contains(&Prefix::V6(address, 128)) {
        return Vec::new();
    }
    vec![PodRoute {
        destination: Prefix::V6(ula, len),
        gateway: IpAddr::V6(GATEWAY),
        mtu,
    }]
}

/// The pod's TAP device and its guest's MAC address, by its ID: `pod-<ID>`
/// for a short ID, `pod-` and the first 8 hex digits for a UUID
/// (ADR 0019).
pub use feos_utils::pod_id::{mac, tap_name};

pub fn mac_string(mac: [u8; 6]) -> String {
    mac.iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<Vec<_>>()
        .join(":")
}

/// Whether the pod has a network: unless its spec says none.
fn has_network(info: &PodInfo) -> bool {
    info.spec.as_ref().is_some_and(crate::spec::has_network)
}

/// The names of the pod's HTTP bindings, as its resolver answers them
/// (ADR 0028).
fn http_names(info: &PodInfo) -> HttpNames {
    info.spec
        .as_ref()
        .and_then(|spec| spec.http_bindings.as_ref())
        .map(|bindings| HttpNames {
            names: bindings.names.clone(),
            hosts: bindings.intercept_hosts.clone(),
        })
        .unwrap_or_default()
}

fn pod_network(info: &PodInfo) -> PodNetwork {
    info.spec
        .as_ref()
        .and_then(|spec| spec.network.clone())
        .unwrap_or_default()
}

/// The pods' network on this node.
pub struct Network {
    config: Config,
    groups: SecurityGroups,
    /// The name tables, from which the resolver answers the pods that name
    /// them.
    names: NameTables,
    records: broadcast::Sender<TrafficRecord>,
    /// The pods' directory, which keeps their addresses.
    dir: PathBuf,
    /// What the network is made of, once it is there: pods with a network
    /// fail until then, for the reason in `unready`.
    parts: OnceLock<Parts>,
    unready: Mutex<String>,
    /// Whether the network is there, for what waits for it.
    ready: watch::Sender<bool>,
    /// The resolver's sockets that were handed to feosd, until the network
    /// comes up.
    handed: Mutex<Handed>,
    /// Sets the network up after the start, once.
    setting_up: tokio::sync::Mutex<()>,
    /// The cluster's mesh, if the node is on one.
    mesh: Option<Mesh>,
    /// Moves with every change of the mesh's peers.
    mesh_changes: watch::Sender<u64>,
    /// Set when the rules could not be written at the start.
    broken: Mutex<Option<String>>,
    directory: Arc<Directory>,
    /// The policy and the table in the kernel.
    applied: tokio::sync::Mutex<Option<(Policy, Table)>>,
    /// The node's addresses, as last read.
    node_addresses: Mutex<Vec<IpAddr>>,
    collector: Mutex<Option<Collector>>,
}

struct Parts {
    /// The node's prefix.
    prefix: (Ipv6Addr, u8),
    ipv6: Ipv6Addresses,
    ipv4: Ipv4Addresses,
    resolver: Arc<Resolver>,
    /// The resolver's sockets until it serves on them.
    sockets: Mutex<Option<(UdpSocket, TcpListener)>>,
}

/// The resolver's sockets that were handed to `feosd`, if any.
type Handed = (Option<std::net::UdpSocket>, Option<std::net::TcpListener>);

impl Network {
    /// Sets the node up for pods: removes the first version's bridge and
    /// tables and the NAT64 version's translator, turns forwarding on,
    /// routes the pod prefix as unreachable, and gives the resolver its
    /// address and sockets: those of `sockets` that are bound to its
    /// address, which systemd or `feos-host-init` hold across restarts of
    /// `feosd` (ADR 0010), or new ones. On a mesh, the device comes up
    /// first, and the prefix is the /96 the control plane gave the node: a
    /// node that has none yet sets the rest up when it gets one
    /// ([`Network::join_mesh`]). A failure is logged, and fails the pods
    /// with a network from then on: feosd goes on serving the others.
    pub async fn start(
        config: Config,
        sockets: Vec<(OwnedFd, String)>,
        dir: &Path,
        groups: SecurityGroups,
        names: NameTables,
        records: broadcast::Sender<TrafficRecord>,
    ) -> Self {
        let resolver = SocketAddr::new(config.dns_address.into(), DNS_PORT);
        let (udp, tcp, refused) = listen_fds::bound_to(sockets, resolver);
        for reason in refused {
            warn!("Network: The resolver does not take the {reason}");
        }
        let mut handed = (udp, tcp);
        let mut prefix = config.pod_prefix;
        let mut unready = "feosd has no --pod-prefix: pods with a network cannot run".to_string();
        let mut mesh = None;
        if let (Some(mesh_config), None) = (&config.mesh, config.pod_prefix) {
            let opened = match devices::connect().await {
                Ok(handle) => Mesh::open(mesh_config.clone(), &handle).await,
                Err(e) => Err(e),
            };
            match opened {
                Ok(opened) => {
                    prefix = opened.assignment().map(|a| a.pod_prefix);
                    unready = "the node waits for the control plane to give it its /96 of the \
                               mesh: pods with a network cannot run yet"
                        .to_string();
                    mesh = Some(opened);
                }
                Err(e) => {
                    error!("Network: The mesh is not set up, pods with a network fail: {e}");
                    unready = format!("the mesh is not set up: {e}");
                }
            }
        }
        let parts = OnceLock::new();
        match prefix {
            Some(prefix) => {
                match set_up_again(&config, prefix, dir, &names, &records, &mut handed).await {
                    Ok(set_up) => {
                        log_set_up(&config, prefix);
                        let _ = parts.set(set_up);
                    }
                    Err(e) => {
                        error!(
                        "Network: The pods' network is not set up, pods with a network fail: {e}"
                    );
                        unready = format!("the pods' network is not set up: {e}");
                    }
                }
            }
            None => {
                if let Err(e) = remove_old_network(true, dir).await {
                    warn!("Network: Failed to remove the IPv4 network: {e}");
                }
                match &mesh {
                    Some(_) => info!(
                        "Network: Waiting for the control plane to give the node its /96 of the mesh"
                    ),
                    None => warn!("Network: No --pod-prefix: pods with a network fail"),
                }
            }
        }
        let ready = watch::Sender::new(parts.get().is_some());
        Self {
            config,
            groups,
            names,
            records,
            dir: dir.to_path_buf(),
            parts,
            unready: Mutex::new(unready),
            ready,
            handed: Mutex::new(handed),
            setting_up: tokio::sync::Mutex::new(()),
            mesh,
            mesh_changes: watch::Sender::new(0),
            broken: Mutex::new(None),
            directory: Arc::default(),
            applied: tokio::sync::Mutex::new(None),
            node_addresses: Mutex::new(Vec::new()),
            collector: Mutex::new(None),
        }
    }

    /// For tests: a network that is not there, set up without touching
    /// the node. Pods without a network work; the rules are not written.
    #[cfg(test)]
    pub(crate) fn absent(dir: &Path) -> Self {
        let (records, _) = broadcast::channel(1);
        Self {
            config: Config::default(),
            groups: SecurityGroups::open(dir.join("groups")).unwrap(),
            names: NameTables::open(dir.join("names")).unwrap(),
            records,
            dir: dir.to_path_buf(),
            parts: OnceLock::new(),
            unready: Mutex::new("no network in this test".to_string()),
            ready: watch::Sender::new(false),
            handed: Mutex::new((None, None)),
            setting_up: tokio::sync::Mutex::new(()),
            mesh: None,
            mesh_changes: watch::Sender::new(0),
            broken: Mutex::new(None),
            directory: Arc::default(),
            applied: tokio::sync::Mutex::new(None),
            node_addresses: Mutex::new(Vec::new()),
            collector: Mutex::new(None),
        }
    }

    pub fn groups(&self) -> &SecurityGroups {
        &self.groups
    }

    pub fn names(&self) -> &NameTables {
        &self.names
    }

    /// Where the node takes its pods' spans (docs/architecture/traces.md),
    /// at [`OTLP_PORTS`]: an address it holds as it does the resolver's.
    pub fn otlp_address(&self) -> Ipv4Addr {
        self.config.otlp_address
    }

    /// Where the pods reach their HTTP bindings (ADR 0028), at
    /// [`HTTP_PORTS`]: the node's address of OTLP's.
    pub fn http_address(&self) -> Ipv4Addr {
        self.config.otlp_address
    }

    /// Has the resolver answer the names of the pod's HTTP bindings as its
    /// record has them now: after SetPodHttpBindings.
    pub fn set_http_names(&self, info: &PodInfo) {
        let (Ok(parts), Ok(address)) = (self.parts(), info.ipv4_address.parse::<Ipv4Addr>()) else {
            return;
        };
        if has_network(info) {
            parts
                .resolver
                .set_http(address.into(), &info.pod_id, &http_names(info));
        }
    }

    /// The node's upstream resolvers (`--dns-upstream`), which a pod's
    /// `feos-authn` resolves its upstreams with.
    pub fn dns_upstreams(&self) -> Vec<IpAddr> {
        self.config.dns_upstreams.clone()
    }

    /// What a pod's `feos-authn` dials none of (ADR 0014, "The jail"):
    /// `--deny-prefix`, the node's own addresses and the mesh's peers', the
    /// pods' IPv4 range and the node's prefix for pods.
    pub fn authn_deny_prefixes(&self) -> Vec<String> {
        let node = lock(&self.node_addresses).clone();
        let mut denied = self.denied(&node);
        if let Some((prefix, len)) = self.pod_prefix().or(self.config.pod_prefix) {
            denied.push(Prefix::V6(prefix, len));
        }
        let mut seen = HashSet::new();
        denied
            .iter()
            .map(ToString::to_string)
            .filter(|prefix| seen.insert(prefix.clone()))
            .collect()
    }

    /// The pod that sends from `address`, as the pods' TAP devices pin it:
    /// for the HTTP bindings' listeners.
    pub fn pod_at(&self, address: IpAddr) -> Option<String> {
        self.directory.pod_at(address.to_canonical())
    }

    /// The parts, if the network works.
    fn parts(&self) -> Result<&Parts, Status> {
        if let Some(broken) = lock(&self.broken).as_ref() {
            return Err(Status::failed_precondition(broken.clone()));
        }
        self.parts
            .get()
            .ok_or_else(|| Status::failed_precondition(lock(&self.unready).clone()))
    }

    /// Whether the network is there, and when it comes.
    pub fn ready(&self) -> watch::Receiver<bool> {
        self.ready.subscribe()
    }

    /// The node's prefix for pods, once it has one.
    pub fn pod_prefix(&self) -> Option<(Ipv6Addr, u8)> {
        self.parts.get().map(|parts| parts.prefix)
    }

    /// The node's WireGuard key and port, if it is on a mesh.
    pub fn mesh_key(&self) -> Option<(feos_utils::wireguard::Key, u16)> {
        self.mesh
            .as_ref()
            .map(|mesh| (mesh.public_key(), mesh.port()))
    }

    /// Takes what the control plane answered to the node's JoinMesh: the
    /// cluster's ULA and the node's /96 of it, which become the node's
    /// prefix and the route to the other nodes. The first /96 sets the
    /// network up, if it is not yet; another one than the network has
    /// takes effect at feosd's next start. A node with `--pod-prefix`
    /// asks only whether the cluster is meshed, and refuses the prefix if
    /// it is (ADR 0017).
    pub async fn join_mesh(&self, pod_network: &str, pod_prefix: &str) -> Result<(), String> {
        let Some(mesh) = &self.mesh else {
            if !pod_network.is_empty() && self.config.pod_prefix.is_some() {
                self.fail(format!(
                    "the cluster's pods are on a mesh of {pod_network}, which refuses \
                     feosd's --pod-prefix"
                ));
            }
            return Ok(());
        };
        if pod_network.is_empty() {
            let reason = "the control plane routes the cluster's pods, and feosd has no \
                          --pod-prefix: pods with a network cannot run";
            if *lock(&self.unready) != reason {
                error!("Network: {reason}");
                *lock(&self.unready) = reason.to_string();
            }
            return Ok(());
        }
        let assignment = Assignment::parse(pod_network, pod_prefix)?;
        let handle = devices::connect().await.map_err(|e| e.to_string())?;
        if mesh
            .assign(assignment, &handle)
            .await
            .map_err(|e| e.to_string())?
        {
            info!("Network: The node has {pod_prefix} of the mesh's {pod_network}");
        }
        let _setting_up = self.setting_up.lock().await;
        match self.parts.get() {
            Some(parts) if parts.prefix == assignment.pod_prefix => {}
            Some(parts) => error!(
                "Network: The control plane gave the node {pod_prefix} in place of {}/{}: feosd \
                 takes it at its next start",
                parts.prefix.0, parts.prefix.1
            ),
            None => {
                let mut handed = std::mem::take(&mut *lock(&self.handed));
                let prefix = assignment.pod_prefix;
                match set_up_again(
                    &self.config,
                    prefix,
                    &self.dir,
                    &self.names,
                    &self.records,
                    &mut handed,
                )
                .await
                {
                    Ok(parts) => {
                        log_set_up(&self.config, prefix);
                        let _ = self.parts.set(parts);
                        self.ready.send_replace(true);
                    }
                    Err(e) => {
                        *lock(&self.handed) = handed;
                        let reason = format!("the pods' network is not set up: {e}");
                        *lock(&self.unready) = reason.clone();
                        return Err(reason);
                    }
                }
            }
        }
        Ok(())
    }

    /// The node's mesh, as `MeshService.GetMesh` shows it.
    pub fn mesh(&self) -> MeshInfo {
        self.mesh.as_ref().map(Mesh::info).unwrap_or_default()
    }

    pub async fn put_mesh_peers(&self, request: PutMeshPeersRequest) -> Result<(), Status> {
        self.on_mesh()?.put(request).await?;
        self.mesh_changes.send_modify(|n| *n = n.wrapping_add(1));
        Ok(())
    }

    pub async fn update_mesh_peers(&self, request: UpdateMeshPeersRequest) -> Result<(), Status> {
        self.on_mesh()?.update(request).await?;
        self.mesh_changes.send_modify(|n| *n = n.wrapping_add(1));
        Ok(())
    }

    /// A counter that moves with every change of the mesh's peers, whose
    /// endpoints the rules deny pods.
    pub fn subscribe_mesh(&self) -> watch::Receiver<u64> {
        self.mesh_changes.subscribe()
    }

    fn on_mesh(&self) -> Result<&Mesh, Status> {
        self.mesh
            .as_ref()
            .ok_or_else(|| Status::failed_precondition("the node is not on a mesh"))
    }

    /// Marks the network as not working.
    pub fn fail(&self, reason: String) {
        error!("Network: {reason}; pods with a network fail");
        *lock(&self.broken) = Some(reason);
    }

    /// Checks the network of a pod's spec: its security groups' and name
    /// tables' IDs, each table named once, and its upstream resolvers.
    pub fn check(&self, spec: &PodSpec) -> Result<(), Status> {
        let Some(network) = &spec.network else {
            return Ok(());
        };
        for id in &network.security_groups {
            network_service::check_id(id)?;
        }
        let mut tables = HashSet::new();
        for id in &network.name_tables {
            network_service::check_table_id(id)?;
            if !tables.insert(id) {
                return Err(Status::invalid_argument(format!(
                    "name table {id} is named twice"
                )));
            }
        }
        self.upstreams(network)?;
        Ok(())
    }

    /// The pod's upstream resolvers, port 53.
    fn upstreams(&self, network: &PodNetwork) -> Result<Vec<SocketAddr>, Status> {
        network
            .dns_upstreams
            .iter()
            .map(|upstream| {
                let address = upstream.parse::<IpAddr>().map_err(|_| {
                    Status::invalid_argument(format!(
                        "DNS upstream {upstream:?} is not an IP address"
                    ))
                })?;
                if let Some(why) = self.refused_upstream(address) {
                    return Err(Status::invalid_argument(format!(
                        "DNS upstream {upstream}: {why}"
                    )));
                }
                Ok(SocketAddr::new(address, DNS_PORT))
            })
            .collect()
    }

    /// Why a pod may not have its queries sent to `address`: one it may not
    /// reach itself, or the resolver.
    fn refused_upstream(&self, address: IpAddr) -> Option<&'static str> {
        if address == IpAddr::V4(self.config.dns_address) {
            return Some("it is the resolver itself");
        }
        let special = match address {
            IpAddr::V4(v4) => {
                v4.is_loopback()
                    || v4.is_unspecified()
                    || v4.is_link_local()
                    || v4.is_multicast()
                    || v4.is_broadcast()
            }
            IpAddr::V6(v6) => {
                v6.is_loopback()
                    || v6.is_unspecified()
                    || v6.is_multicast()
                    || v6.is_unicast_link_local()
            }
        };
        if special {
            return Some("not an address a resolver has");
        }
        let host = match address {
            IpAddr::V4(v4) => Prefix::V4(v4, 32),
            IpAddr::V6(v6) => Prefix::V6(v6, 128),
        };
        if self
            .config
            .deny_prefixes
            .iter()
            .any(|denied| denied.contains(&host))
        {
            return Some("it is in a denied prefix");
        }
        let (range, len) = self.config.pod_ipv4_range;
        let pods = [
            self.pod_prefix()
                .or(self.config.pod_prefix)
                .map(|(prefix, len)| Prefix::V6(prefix, len)),
            Some(Prefix::V4(range, len)),
        ];
        if pods.iter().flatten().any(|pods| pods.contains(&host)) {
            return Some("it is a pod's");
        }
        if lock(&self.node_addresses).contains(&address) {
            return Some("it is the node's");
        }
        None
    }

    /// Gives a new pod its addresses: an IPv6 address of the prefix and an
    /// IPv4 address of the range. `pods` are the others.
    pub fn allocate<'a>(
        &self,
        pods: impl Iterator<Item = &'a PodInfo>,
        info: &mut PodInfo,
    ) -> Result<(), Status> {
        let parts = self.parts()?;
        let tap = tap_name(&info.pod_id);
        let mut ipv6 = HashSet::new();
        let mut ipv4 = HashSet::new();
        for pod in pods.filter(|pod| has_network(pod)) {
            if tap_name(&pod.pod_id) == tap {
                return Err(Status::already_exists(format!(
                    "pod {} has the TAP device {tap} already: the first 8 digits of a pod's ID tell it",
                    pod.pod_id
                )));
            }
            ipv6.extend(pod.ipv6_address.parse::<Ipv6Addr>());
            ipv4.extend(pod.ipv4_address.parse::<Ipv4Addr>());
        }
        let exhausted = |e: io::Error| Status::resource_exhausted(format!("no address: {e}"));
        info.ipv6_address = parts.ipv6.allocate(&ipv6).map_err(exhausted)?.to_string();
        info.ipv4_address = parts.ipv4.allocate(&ipv4).map_err(exhausted)?.to_string();
        Ok(())
    }

    /// Gives pods kept from before what this network needs: an IPv6
    /// address of the prefix to those without one, such as those of the
    /// first version's IPv4 network, and an IPv4 address of the range to
    /// those without one, such as those of the IPv6 only network or with
    /// an address of the first version's pool. Returns the IDs of those
    /// changed.
    pub fn adopt(&self, pods: &mut [PodInfo]) -> Vec<String> {
        let Ok(parts) = self.parts() else {
            return Vec::new();
        };
        // What pods keep comes first; the others get what is left.
        let mut ipv6 = HashSet::new();
        let mut ipv4 = HashSet::new();
        let mut kept = Vec::with_capacity(pods.len());
        for pod in pods.iter() {
            let network = has_network(pod);
            let v6 = pod.ipv6_address.parse::<Ipv6Addr>().ok();
            let v6 = v6.filter(|a| network && parts.ipv6.contains(*a) && ipv6.insert(*a));
            let v4 = pod.ipv4_address.parse::<Ipv4Addr>().ok();
            let v4 = v4.filter(|a| network && parts.ipv4.contains(*a) && ipv4.insert(*a));
            kept.push((v6, v4));
        }
        // An address handed out that no pod kept is free.
        if let Err(e) = parts.ipv4.reconcile(&ipv4, SystemTime::now()) {
            warn!("Network: The pods' IPv4 addresses are not kept: {e}");
        }
        let mut changed = Vec::new();
        for (pod, (v6, v4)) in pods.iter_mut().zip(kept) {
            if !has_network(pod) {
                continue;
            }
            let before = (pod.ipv6_address.clone(), pod.ipv4_address.clone());
            if v6.is_none() {
                match parts.ipv6.allocate(&ipv6) {
                    Ok(address) => {
                        ipv6.insert(address);
                        pod.ipv6_address = address.to_string();
                    }
                    Err(e) => warn!("Network: No address for pod {}: {e}", pod.pod_id),
                }
            }
            if v4.is_none() {
                pod.ipv4_address = match parts.ipv4.allocate(&ipv4) {
                    Ok(address) => {
                        ipv4.insert(address);
                        address.to_string()
                    }
                    Err(e) => {
                        warn!("Network: No IPv4 address for pod {}: {e}", pod.pod_id);
                        String::new()
                    }
                };
            }
            if (&pod.ipv6_address, &pod.ipv4_address) != (&before.0, &before.1) {
                info!(
                    "Network: Pod {} has the addresses {} and {}",
                    pod.pod_id, pod.ipv6_address, pod.ipv4_address
                );
                changed.push(pod.pod_id.clone());
            }
        }
        changed
    }

    /// Lets the resolver answer on its sockets, once it knows the pods
    /// ([`Network::sync`]): a query that waited in them while `feosd`
    /// restarted is answered then, not refused.
    pub fn serve_dns(&self) {
        let Ok(parts) = self.parts() else {
            return;
        };
        let Some((udp, tcp)) = lock(&parts.sockets).take() else {
            return;
        };
        let serving = parts.resolver.clone();
        tokio::spawn(async move {
            if let Err(e) = serving.serve(udp, tcp).await {
                error!("Network: The resolver stopped: {e}");
            }
        });
    }

    /// Brings the resolver's pods in line with the pods kept: after a
    /// restart of feosd.
    pub fn sync(&self, pods: &[PodInfo]) {
        let Ok(parts) = self.parts() else {
            return;
        };
        for pod in pods.iter().filter(|pod| has_network(pod)) {
            let Ok(address) = pod.ipv4_address.parse::<Ipv4Addr>() else {
                continue;
            };
            let network = pod_network(pod);
            match self.upstreams(&network) {
                Ok(upstreams) => {
                    parts.resolver.set_client(
                        address.into(),
                        &pod.pod_id,
                        upstreams,
                        network.name_tables,
                    );
                    parts
                        .resolver
                        .set_http(address.into(), &pod.pod_id, &http_names(pod));
                }
                Err(e) => warn!("Network: Pod {} gets no DNS: {}", pod.pod_id, e.message()),
            }
        }
    }

    /// Puts the pod on the network, if it has one: before every boot and
    /// resume. Fails if the node does not have the pod's security groups or
    /// name tables.
    pub async fn attach(&self, info: &PodInfo) -> Result<Option<Attachment>, Status> {
        if !has_network(info) {
            return Ok(None);
        }
        let parts = self.parts()?;
        let pod_id = &info.pod_id;
        let ipv6 = info
            .ipv6_address
            .parse::<Ipv6Addr>()
            .ok()
            .filter(|address| parts.ipv6.contains(*address))
            .ok_or_else(|| {
                Status::failed_precondition(format!(
                    "pod {pod_id} has no address of the pod prefix"
                ))
            })?;
        let ipv4 = info
            .ipv4_address
            .parse::<Ipv4Addr>()
            .ok()
            .filter(|address| parts.ipv4.contains(*address))
            .ok_or_else(|| {
                Status::failed_precondition(format!(
                    "pod {pod_id} has no IPv4 address of the pods' range"
                ))
            })?;
        let network = pod_network(info);
        let missing = self.groups.missing(&network.security_groups);
        if !missing.is_empty() {
            return Err(Status::failed_precondition(format!(
                "the node does not have the security groups {}",
                missing.join(", ")
            )));
        }
        let missing = self.names.missing(&network.name_tables);
        if !missing.is_empty() {
            return Err(Status::failed_precondition(format!(
                "the node does not have the name tables {}",
                missing.join(", ")
            )));
        }
        let upstreams = self
            .upstreams(&network)
            .map_err(|e| Status::failed_precondition(e.message()))?;
        let tap = tap_name(pod_id);
        let handle = devices::connect()
            .await
            .map_err(|e| Status::internal(format!("netlink: {e}")))?;
        let link = PodLink {
            gateway: GATEWAY,
            gateway4: GATEWAY4,
            node4: self.config.dns_address,
            otlp4: self.config.otlp_address,
            ipv6,
            ipv4,
        };
        devices::tap(&handle, &tap, &link)
            .await
            .map_err(|e| Status::internal(format!("attach {tap}: {e}")))?;
        parts
            .resolver
            .set_client(ipv4.into(), pod_id, upstreams, network.name_tables);
        parts
            .resolver
            .set_http(ipv4.into(), pod_id, &http_names(info));
        Ok(Some(Attachment {
            tap,
            mac: mac(pod_id),
            ipv6,
            ipv4,
            dns: self.config.dns_address,
            routes: routes(ipv6, self.mesh.as_ref().map_or(0, |_| MESH_MTU)),
        }))
    }

    /// Takes a deleted pod off the network: its resolver, its TAP device
    /// with its routes and its proxy ARP, and the connections its addresses
    /// have in connection tracking, so that the next pod with its IPv4
    /// address finds none of them. Before the pod leaves the directory, so
    /// that their ends are recorded as the pod's.
    pub async fn detach(&self, info: &PodInfo) -> io::Result<()> {
        if !has_network(info) {
            return Ok(());
        }
        let ipv4 = match (self.parts.get(), info.ipv4_address.parse::<Ipv4Addr>()) {
            (Some(parts), Ok(address)) if parts.ipv4.contains(address) => {
                parts.resolver.remove_client(address.into());
                Some(address)
            }
            _ => None,
        };
        let ipv6 = match (self.parts.get(), info.ipv6_address.parse::<Ipv6Addr>()) {
            (Some(parts), Ok(address)) if parts.ipv6.contains(address) => Some(address),
            _ => None,
        };
        let tap = tap_name(&info.pod_id);
        microvm_service::vmm::jail::tap::delete(&tap)
            .map_err(|e| io::Error::new(e.kind(), format!("delete {tap}: {e}")))?;
        let addresses = ipv4.map(IpAddr::V4).into_iter().chain(ipv6.map(IpAddr::V6));
        for address in addresses {
            let deleted =
                tokio::task::spawn_blocking(move || traffic_service::delete_connections(address))
                    .await
                    .map_err(io::Error::other)
                    .and_then(|deleted| deleted);
            match deleted {
                Ok(0) => {}
                Ok(n) => info!("Network: Deleted {n} connections of {address}"),
                Err(e) => warn!("Network: Failed to delete the connections of {address}: {e}"),
            }
        }
        Ok(())
    }

    /// Frees a deleted pod's IPv4 address, once the pod is off the network
    /// and out of the rules: it comes round again after the others free.
    pub fn release(&self, info: &PodInfo) {
        if !has_network(info) {
            return;
        }
        if let (Some(parts), Ok(address)) =
            (self.parts.get(), info.ipv4_address.parse::<Ipv4Addr>())
            && let Err(e) = parts.ipv4.free(address, SystemTime::now())
        {
            warn!("Network: Failed to free {address}: {e}");
        }
    }

    /// Writes the rules for these pods and the node's security groups:
    /// the whole table the first time and whenever pods or rules change,
    /// only the elements of sets that changed otherwise.
    pub async fn apply(&self, pods: &[PodInfo]) -> io::Result<()> {
        if self.parts().is_err() {
            return Ok(());
        }
        let mut applied = self.applied.lock().await;
        let node = match devices::connect().await {
            Ok(handle) => devices::node_addresses(&handle).await,
            Err(e) => Err(e),
        };
        let node = match node {
            Ok(node) => {
                *lock(&self.node_addresses) = node.clone();
                node
            }
            Err(e) => {
                warn!("Network: The node's addresses do not read: {e}");
                lock(&self.node_addresses).clone()
            }
        };
        let pods: Vec<&PodInfo> = pods.iter().filter(|pod| has_network(pod)).collect();
        self.directory.set(&pods);
        let members: Vec<network_service::Pod> = pods
            .iter()
            .filter_map(|pod| {
                Some(network_service::Pod {
                    pod_id: pod.pod_id.clone(),
                    tap: tap_name(&pod.pod_id),
                    address: pod.ipv6_address.parse().ok()?,
                    groups: pod_network(pod).security_groups,
                })
            })
            .collect();
        let policy = compile(&self.groups.snapshot(), &members, &self.denied(&node));
        let table = rules::table(&policy, &self.rules());
        match applied
            .as_ref()
            .map(|(old, table)| (delta(old, &policy), table))
        {
            Some((Delta::Elements(changes), _)) if changes.is_empty() => {}
            Some((Delta::Elements(changes), old)) => {
                if let Err(e) = nft::update(old, &rules::element_ops(&changes)) {
                    warn!("Network: Failed to change the sets, replacing the table: {e}");
                    nft::replace(&table)?;
                }
            }
            _ => nft::replace(&table)?,
        }
        *applied = Some((policy, table));
        Ok(())
    }

    /// What no pod reaches: the denied prefixes, the node's own addresses
    /// and the mesh's peers', and the pods' IPv4 range, which link-local
    /// addresses cover unless `--pod-ipv4-range` moves it: pods reach each
    /// other over IPv6 alone, and other nodes' pods through the mesh.
    fn denied(&self, node: &[IpAddr]) -> Vec<Prefix> {
        let mut denied = self.config.deny_prefixes.clone();
        let peers = self.mesh.as_ref().map(Mesh::endpoints).unwrap_or_default();
        denied.extend(node.iter().chain(&peers).map(|address| match *address {
            IpAddr::V4(v4) => Prefix::V4(v4, 32),
            IpAddr::V6(v6) => Prefix::V6(v6, 128),
        }));
        let (range, len) = self.config.pod_ipv4_range;
        denied.push(Prefix::V4(range, len));
        denied
    }

    fn rules(&self) -> rules::Options {
        rules::Options {
            dns: IpAddr::V4(self.config.dns_address),
            otlp: IpAddr::V4(self.config.otlp_address),
            ipv4_range: self.config.pod_ipv4_range,
            mesh: self
                .mesh
                .as_ref()
                .and_then(Mesh::assignment)
                .map(|assignment| assignment.pod_network),
        }
    }

    /// Starts the records of flows, drops and server names: once the
    /// table is written, whose `ct` expressions load connection tracking.
    pub fn start_records(&self) {
        if self.parts().is_err() {
            return;
        }
        if let Err(e) = traffic_service::enable_accounting() {
            warn!("Network: Flows have no counters: {e}");
        }
        let (sender, receiver) = broadcast::channel(RECORDS);
        let config = traffic_service::Config::default();
        match Collector::start(self.directory.clone(), config, sender) {
            Ok(collector) => {
                *lock(&self.collector) = Some(collector);
                // The resolver records the pods' queries to it.
                let resolver = IpAddr::V4(self.config.dns_address);
                tokio::spawn(forward_records(
                    receiver,
                    self.records.clone(),
                    move |record| !is_dns_flow(record, resolver),
                ));
                info!("Network: Recording the pods' flows, drops and server names");
            }
            Err(e) => error!("Network: No records of flows, drops or server names: {e}"),
        }
    }
}

/// Sets the node up for pods, see [`Network::start`]. Every step can be
/// taken again; the resolver's sockets are taken from `handed` or bound
/// last.
async fn set_up(
    config: &Config,
    (prefix, len): (Ipv6Addr, u8),
    dir: &Path,
    names: &NameTables,
    records: &broadcast::Sender<TrafficRecord>,
    handed: &mut Handed,
) -> io::Result<Parts> {
    config.check().map_err(io::Error::other)?;
    remove_old_network(false, dir).await?;
    let handle = devices::connect().await?;
    devices::enable_forwarding()?;
    devices::route_unreachable(&handle, prefix, len).await?;
    devices::dummy_with_addresses(
        &handle,
        DNS_DEVICE,
        &[config.dns_address.into(), config.otlp_address.into()],
    )
    .await?;

    let resolver = Resolver::new(dns_service::Config {
        default_upstreams: config
            .dns_upstreams
            .iter()
            .map(|address| SocketAddr::new(*address, DNS_PORT))
            .collect(),
        http_address: Some(config.otlp_address),
        ..Default::default()
    });
    let tables = names.clone();
    resolver.set_names(move |ids, name| tables.lookup(ids, name));
    let address = SocketAddr::new(config.dns_address.into(), DNS_PORT);
    let udp = match handed.0.take() {
        Some(socket) => {
            info!("Network: The resolver takes the UDP socket handed to feosd");
            socket.set_nonblocking(true)?;
            UdpSocket::from_std(socket)?
        }
        None => UdpSocket::bind(address)
            .await
            .map_err(|e| io::Error::new(e.kind(), format!("DNS: bind udp {address}: {e}")))?,
    };
    let tcp = match handed.1.take() {
        Some(listener) => {
            info!("Network: The resolver takes the TCP socket handed to feosd");
            listener.set_nonblocking(true)?;
            TcpListener::from_std(listener)?
        }
        None => TcpListener::bind(address)
            .await
            .map_err(|e| io::Error::new(e.kind(), format!("DNS: bind tcp {address}: {e}")))?,
    };
    // A self-upgrade hands them to the next feosd.
    for fd in [udp.as_fd(), tcp.as_fd()] {
        if let Err(e) = listen_fds::hand_on(fd, listen_fds::DNS) {
            warn!("Network: A self-upgrade will not hand on the resolver's sockets: {e}");
        }
    }
    tokio::spawn(forward_records(
        resolver.subscribe(),
        records.clone(),
        |_| true,
    ));
    let (range, range_len) = config.pod_ipv4_range;
    Ok(Parts {
        prefix: (prefix, len),
        ipv6: Ipv6Addresses::new(dir, prefix, len),
        ipv4: Ipv4Addresses::new(dir, range, range_len),
        resolver,
        sockets: Mutex::new(Some((udp, tcp))),
    })
}

fn log_set_up(config: &Config, (prefix, len): (Ipv6Addr, u8)) {
    let (range, range_len) = config.pod_ipv4_range;
    info!(
        "Network: Pods get addresses of {prefix}/{len} and {range}/{range_len}, DNS at {}, \
         OTLP at {}",
        config.dns_address, config.otlp_address
    );
}

/// [`set_up`], taken again a few times after a failure: netlink may answer
/// a moment later.
async fn set_up_again(
    config: &Config,
    prefix: (Ipv6Addr, u8),
    dir: &Path,
    names: &NameTables,
    records: &broadcast::Sender<TrafficRecord>,
    handed: &mut Handed,
) -> io::Result<Parts> {
    let mut attempt = 1;
    loop {
        match set_up(config, prefix, dir, names, records, handed).await {
            Err(e) if attempt < SET_UP_ATTEMPTS => {
                warn!("Network: Setting the pods' network up failed, trying again: {e}");
                attempt += 1;
                tokio::time::sleep(SET_UP_PAUSE).await;
            }
            result => return result,
        }
    }
}

/// Removes the first version of the network: its bridge and the table
/// `bridge feos`, on a node whose kernel has the bridge family; with
/// `inet`, the table `inet feos` too, which a working network replaces
/// anyway. Removes what the NAT64 version left, too.
async fn remove_old_network(inet: bool, dir: &Path) -> io::Result<()> {
    match nft::delete(Family::Bridge, TABLE) {
        Ok(()) => {}
        // No bridge family, as in feos-host-init's kernel.
        Err(e) if e.kind() == io::ErrorKind::NotFound => {}
        // EOPNOTSUPP, whose text musl gives as "Not supported".
        Err(e) if e.to_string().to_lowercase().contains("not supported") => {}
        Err(e) => warn!("Network: Failed to remove the table bridge {TABLE}: {e}"),
    }
    if inet && let Err(e) = nft::delete(Family::Inet, TABLE) {
        warn!("Network: Failed to remove the table inet {TABLE}: {e}");
    }
    let handle = devices::connect().await?;
    if devices::delete_link(&handle, OLD_BRIDGE).await? {
        info!("Network: Removed the bridge {OLD_BRIDGE} of the IPv4 network");
    }
    remove_old_nat64(&handle, Path::new(OLD_NAT64_PINS), dir).await;
    Ok(())
}

/// Removes what the NAT64 version left on the node: its device, which
/// takes its routes and its program with it, the maps pinned in `pins`,
/// and the counter of its IPv4 pool in `dir`. A failure is logged.
async fn remove_old_nat64(handle: &Handle, pins: &Path, dir: &Path) {
    match devices::delete_link(handle, OLD_NAT64_DEVICE).await {
        Ok(true) => info!("Network: Removed the device {OLD_NAT64_DEVICE} of NAT64"),
        Ok(false) => {}
        Err(e) => warn!("Network: Failed to remove the device {OLD_NAT64_DEVICE}: {e}"),
    }
    remove_old_pins(pins);
    match std::fs::remove_file(dir.join(OLD_NAT64_COUNTER)) {
        Ok(()) => info!("Network: Removed the counter of NAT64's IPv4 pool"),
        Err(e) if e.kind() == io::ErrorKind::NotFound => {}
        Err(e) => warn!("Network: Failed to remove the counter of NAT64's IPv4 pool: {e}"),
    }
}

/// Removes the maps that NAT64 pinned in `dir` on the bpf filesystem, the
/// directory, and its parent if that is empty then. Unlinking a pin needs
/// no bpf(2): the kernel frees a map that nothing holds.
fn remove_old_pins(dir: &Path) {
    let entries = match std::fs::read_dir(dir) {
        Ok(entries) => entries,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return,
        Err(e) => {
            warn!("Network: Failed to read {}: {e}", dir.display());
            return;
        }
    };
    for entry in entries.flatten() {
        if let Err(e) = std::fs::remove_file(entry.path()) {
            warn!("Network: Failed to remove {}: {e}", entry.path().display());
        }
    }
    match std::fs::remove_dir(dir) {
        Ok(()) => info!("Network: Removed NAT64's maps in {}", dir.display()),
        Err(e) => warn!("Network: Failed to remove {}: {e}", dir.display()),
    }
    if let Some(parent) = dir.parent() {
        // Another program's pins may be there.
        let _ = std::fs::remove_dir(parent);
    }
}

/// Whether the record is the flow of a pod's query to the resolver, which
/// the resolver's own record covers.
fn is_dns_flow(record: &TrafficRecord, resolver: IpAddr) -> bool {
    let Some(traffic_record::Record::Flow(flow)) = &record.record else {
        return false;
    };
    flow.tuple.as_ref().is_some_and(|tuple| {
        tuple.destination_port == u32::from(DNS_PORT)
            && tuple.destination_address.parse::<IpAddr>().ok() == Some(resolver)
    })
}

/// Forwards the records that `keep` keeps, until `from` closes. What was
/// lost on the way is counted in the next record's missed, and so is what
/// a record left out counted.
async fn forward_records(
    mut from: broadcast::Receiver<TrafficRecord>,
    to: broadcast::Sender<TrafficRecord>,
    keep: impl Fn(&TrafficRecord) -> bool + Send + 'static,
) {
    let mut missed = 0;
    loop {
        match from.recv().await {
            Ok(mut record) => {
                if !keep(&record) {
                    missed += record.missed;
                    continue;
                }
                record.missed += std::mem::take(&mut missed);
                // Nobody may be listening.
                let _ = to.send(record);
            }
            Err(broadcast::error::RecvError::Lagged(lost)) => missed += lost,
            Err(broadcast::error::RecvError::Closed) => return,
        }
    }
}

/// Which pod an address, of either family, or a TAP device is, for the
/// records.
#[derive(Default)]
struct Directory {
    addresses: RwLock<HashMap<IpAddr, String>>,
    taps: RwLock<HashMap<String, String>>,
    /// The addresses of pods that left, with when, for [`DEPARTED`]: the
    /// ends of their connections come after they left, those that a
    /// delete ends among them. An address that a pod holds is its.
    departed: RwLock<HashMap<IpAddr, (String, Instant)>>,
}

/// How long the records know a pod by its addresses after it left.
const DEPARTED: Duration = Duration::from_secs(300);

impl Directory {
    /// Knows these pods, those with a network, and those that left in the
    /// last [`DEPARTED`].
    fn set(&self, pods: &[&PodInfo]) {
        self.set_at(pods, Instant::now());
    }

    fn set_at(&self, pods: &[&PodInfo], now: Instant) {
        let addresses: HashMap<IpAddr, String> = pods
            .iter()
            .flat_map(|pod| {
                [&pod.ipv6_address, &pod.ipv4_address]
                    .into_iter()
                    .filter_map(|address| address.parse::<IpAddr>().ok())
                    .map(|address| (address, pod.pod_id.clone()))
            })
            .collect();
        let taps = pods
            .iter()
            .map(|pod| (tap_name(&pod.pod_id), pod.pod_id.clone()))
            .collect();
        let left = std::mem::replace(
            &mut *self
                .addresses
                .write()
                .unwrap_or_else(PoisonError::into_inner),
            addresses.clone(),
        );
        *self.taps.write().unwrap_or_else(PoisonError::into_inner) = taps;
        let mut departed = self
            .departed
            .write()
            .unwrap_or_else(PoisonError::into_inner);
        departed.retain(|address, (_, since)| {
            !addresses.contains_key(address) && now.saturating_duration_since(*since) < DEPARTED
        });
        for (address, pod_id) in left {
            if !addresses.contains_key(&address) {
                departed.insert(address, (pod_id, now));
            }
        }
    }

    /// The pod that holds `address` now; none for one that left.
    fn pod_at(&self, address: IpAddr) -> Option<String> {
        self.addresses
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .get(&address)
            .cloned()
    }
}

impl traffic_service::Pods for Directory {
    fn by_address(&self, address: IpAddr) -> Option<String> {
        let address = address.to_canonical();
        let holder = self
            .addresses
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .get(&address)
            .cloned();
        holder.or_else(|| {
            self.departed
                .read()
                .unwrap_or_else(PoisonError::into_inner)
                .get(&address)
                .filter(|(_, since)| since.elapsed() < DEPARTED)
                .map(|(pod_id, _)| pod_id.clone())
        })
    }

    fn by_interface(&self, name: &str) -> Option<String> {
        self.taps
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .get(name)
            .cloned()
    }
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::network_service::{Flow, Tuple};
    use std::fs;
    use traffic_service::Pods as _;

    fn network(config: Config) -> Network {
        let dir = tempfile::tempdir().unwrap();
        let groups = SecurityGroups::open(dir.path().join("groups")).unwrap();
        let names = NameTables::open(dir.path().join("names")).unwrap();
        Network {
            config,
            groups,
            names,
            records: broadcast::channel(4).0,
            dir: dir.path().to_path_buf(),
            parts: OnceLock::new(),
            unready: Mutex::new("not set up".to_string()),
            ready: watch::Sender::new(false),
            handed: Mutex::new((None, None)),
            setting_up: tokio::sync::Mutex::new(()),
            mesh: None,
            mesh_changes: watch::Sender::new(0),
            broken: Mutex::new(None),
            directory: Arc::default(),
            applied: tokio::sync::Mutex::new(None),
            node_addresses: Mutex::new(vec!["192.0.2.7".parse().unwrap()]),
            collector: Mutex::new(None),
        }
    }

    /// A network whose addresses are kept in `dir`, as if it were set up.
    fn set_up_in(dir: &Path, config: Config) -> Network {
        let (prefix, len) = config.pod_prefix.expect("a pod prefix");
        let (range, range_len) = config.pod_ipv4_range;
        let network = network(config);
        let _ = network.parts.set(Parts {
            prefix: (prefix, len),
            ipv6: Ipv6Addresses::new(dir, prefix, len),
            ipv4: Ipv4Addresses::new(dir, range, range_len),
            resolver: Resolver::new(dns_service::Config::default()),
            sockets: Mutex::new(None),
        });
        network
    }

    fn lab() -> Config {
        Config {
            pod_prefix: Some(("fd00:fe05:1::".parse().unwrap(), 64)),
            ..Config::default()
        }
    }

    /// A pod with a network and these addresses.
    fn pod(id: &str, ipv6: &str, ipv4: &str) -> PodInfo {
        PodInfo {
            pod_id: id.to_string(),
            spec: Some(PodSpec::default()),
            ipv6_address: ipv6.to_string(),
            ipv4_address: ipv4.to_string(),
            ..Default::default()
        }
    }

    fn with_upstreams(upstreams: &[&str]) -> PodNetwork {
        PodNetwork {
            dns_upstreams: upstreams.iter().map(|u| u.to_string()).collect(),
            ..Default::default()
        }
    }

    #[test]
    fn a_ula_has_no_default_route() {
        let ula = network_service::Prefix::V6("fc00::".parse().unwrap(), 7);
        assert_eq!(
            routes("fd5e:1b2c:93a0:7::2".parse().unwrap(), 1420),
            [PodRoute {
                destination: ula,
                gateway: "fe80::1".parse().unwrap(),
                mtu: 1420,
            }]
        );
        assert!(routes("2001:db8:1:2::2".parse().unwrap(), 0).is_empty());
        assert!(routes("fe80::2".parse().unwrap(), 0).is_empty());
    }

    #[test]
    fn names_and_macs_come_from_the_pod() {
        let id = "8d99e180-1694-4d94-a42e-097769b89510";
        assert_eq!(tap_name(id), "pod-8d99e180");
        assert!(tap_name(id).len() < libc::IFNAMSIZ);
        assert_eq!(mac_string(mac(id)), "06:00:8d:99:e1:80");
        // A short ID is its TAP device's name (ADR 0019).
        let id = "k7m2q9x4c1b";
        assert_eq!(tap_name(id), "pod-k7m2q9x4c1b");
        assert_eq!(tap_name(id).len(), libc::IFNAMSIZ - 1);
        assert_eq!(mac(id)[..2], [0x06, 0x00]);
    }

    #[test]
    fn flags_are_parsed() {
        assert_eq!(
            parse_pod_prefix("fd00:fe05:1::/64"),
            Ok(("fd00:fe05:1::".parse().unwrap(), 64))
        );
        assert!(parse_pod_prefix("fd00:fe05:1::1/64").is_err(), "host bits");
        assert!(parse_pod_prefix("fd00::/127").is_err(), "no room");
        assert!(parse_pod_prefix("10.0.0.0/8").is_err());
        assert_eq!(
            parse_pod_ipv4_range("169.254.16.0/20"),
            Ok((Ipv4Addr::new(169, 254, 16, 0), 20))
        );
        assert_eq!(
            parse_pod_ipv4_range("10.77.0.0/30"),
            Ok((Ipv4Addr::new(10, 77, 0, 0), 30))
        );
        assert!(parse_pod_ipv4_range("10.77.0.0/31").is_err(), "no room");
        assert!(parse_pod_ipv4_range("10.77.0.1/24").is_err(), "host bits");
        assert!(parse_pod_ipv4_range("fd00::/64").is_err());
        let config = Config::default();
        assert_eq!(config.dns_upstreams.len(), 4);
        assert_eq!(config.dns_address, Ipv4Addr::new(169, 254, 0, 53));
        assert_eq!(config.otlp_address, Ipv4Addr::new(169, 254, 169, 1));
        assert_eq!(config.pod_ipv4_range, (Ipv4Addr::new(169, 254, 16, 0), 20));
        assert_eq!(config.check(), Ok(()));
    }

    #[test]
    fn the_node_holds_neither_the_gateway_nor_a_pods_address() {
        let config = |dns: &str, range: &str| Config {
            dns_address: dns.parse().unwrap(),
            pod_ipv4_range: parse_pod_ipv4_range(range).unwrap(),
            ..Config::default()
        };
        assert_eq!(config("10.77.0.53", "10.78.0.0/20").check(), Ok(()));
        for (dns, range) in [
            // This very gateway may be the node's own network's.
            ("169.254.0.1", "169.254.16.0/20"),
            ("0.0.0.0", "169.254.16.0/20"),
            ("127.0.0.53", "169.254.16.0/20"),
            ("224.0.0.53", "169.254.16.0/20"),
            ("255.255.255.255", "169.254.16.0/20"),
            ("169.254.0.53", "169.254.0.0/16"),
            ("169.254.0.53", "169.254.0.0/24"),
            ("10.77.0.53", "169.254.0.0/30"),
        ] {
            assert!(config(dns, range).check().is_err(), "{dns} and {range}");
        }
        // OTLP's address, as the resolver's, and neither it nor a metadata
        // service's nor denied.
        let otlp = |otlp: &str, range: &str, denied: &[&str]| Config {
            otlp_address: otlp.parse().unwrap(),
            pod_ipv4_range: parse_pod_ipv4_range(range).unwrap(),
            deny_prefixes: denied.iter().map(|p| p.parse().unwrap()).collect(),
            ..Config::default()
        };
        assert_eq!(
            otlp("169.254.169.1", "169.254.16.0/20", &[]).check(),
            Ok(())
        );
        assert_eq!(otlp("10.77.0.17", "169.254.16.0/20", &[]).check(), Ok(()));
        for (address, range, denied) in [
            ("169.254.0.1", "169.254.16.0/20", &[][..]),
            ("169.254.0.53", "169.254.16.0/20", &[]),
            ("169.254.169.254", "169.254.16.0/20", &[]),
            ("0.0.0.0", "169.254.16.0/20", &[]),
            ("127.0.0.1", "169.254.16.0/20", &[]),
            ("169.254.16.9", "169.254.16.0/20", &[]),
            ("169.254.169.1", "169.254.0.0/16", &[]),
            ("169.254.169.1", "169.254.16.0/20", &["169.254.169.0/24"]),
            ("169.254.169.1", "169.254.16.0/20", &["169.254.169.1/32"]),
        ] {
            assert!(
                otlp(address, range, denied).check().is_err(),
                "{address} and {range}, denied {denied:?}"
            );
        }
    }

    #[test]
    fn upstreams_a_pod_may_not_reach_are_refused() {
        let network = network(Config {
            pod_prefix: Some(("fd00:fe05:1::".parse().unwrap(), 64)),
            pod_ipv4_range: (Ipv4Addr::new(10, 77, 0, 0), 20),
            deny_prefixes: vec![
                "169.254.169.254/32".parse().unwrap(),
                "2001:db8::/32".parse().unwrap(),
                "100.64.0.0/10".parse().unwrap(),
            ],
            ..Config::default()
        });
        let check = |upstreams: &[&str]| network.upstreams(&with_upstreams(upstreams));
        assert_eq!(
            check(&["2606:4700:4700::1111", "9.9.9.9"]).unwrap(),
            [
                "[2606:4700:4700::1111]:53".parse().unwrap(),
                "9.9.9.9:53".parse().unwrap()
            ]
        );
        for refused in [
            "169.254.0.53",
            "169.254.0.1",
            "100.64.1.1",
            "2001:db8::53",
            "127.0.0.1",
            "::1",
            "fe80::1",
            "fd00:fe05:1::2",
            "10.77.0.9",
            "192.0.2.7",
            "dns.google",
        ] {
            let error = check(&[refused]).unwrap_err();
            assert_eq!(error.code(), tonic::Code::InvalidArgument, "{refused}");
        }
        let spec = PodSpec {
            network: Some(PodNetwork {
                security_groups: vec!["a/b".to_string()],
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(network.check(&spec).is_err(), "a bad group ID");
        let tables = |ids: &[&str]| PodSpec {
            network: Some(PodNetwork {
                name_tables: ids.iter().map(|id| id.to_string()).collect(),
                ..Default::default()
            }),
            ..Default::default()
        };
        network.check(&tables(&["env-1", "sbx-1"])).unwrap();
        for bad in [&["a/b"][..], &["env-1", "env-1"], &[""]] {
            let error = network.check(&tables(bad)).unwrap_err();
            assert_eq!(error.code(), tonic::Code::InvalidArgument, "{bad:?}");
        }
    }

    #[test]
    fn denied_has_the_node_and_the_pods_ipv4() {
        let network = network(Config {
            deny_prefixes: vec!["169.254.169.254/32".parse().unwrap()],
            pod_ipv4_range: (Ipv4Addr::new(10, 77, 0, 0), 20),
            ..Config::default()
        });
        let node = ["192.0.2.7".parse().unwrap(), "2001:db8::7".parse().unwrap()];
        assert_eq!(
            network.denied(&node),
            [
                "169.254.169.254/32".parse().unwrap(),
                "192.0.2.7/32".parse().unwrap(),
                "2001:db8::7/128".parse().unwrap(),
                "10.77.0.0/20".parse().unwrap(),
            ]
        );
        assert_eq!(
            self::tests::network(Config::default()).denied(&[]),
            ["169.254.16.0/20".parse().unwrap()]
        );
        let rules = network.rules();
        assert_eq!(rules.dns, "169.254.0.53".parse::<IpAddr>().unwrap());
        assert_eq!(rules.ipv4_range, (Ipv4Addr::new(10, 77, 0, 0), 20));
    }

    #[test]
    fn without_a_prefix_pods_with_a_network_fail() {
        let network = network(Config::default());
        let mut info = PodInfo::default();
        let error = network.allocate(std::iter::empty(), &mut info).unwrap_err();
        assert_eq!(error.code(), tonic::Code::FailedPrecondition);
        network.fail("nftables: no".to_string());
        assert_eq!(network.parts().err().unwrap().message(), "nftables: no");
    }

    #[test]
    fn a_pod_gets_an_address_of_each_family() {
        let dir = tempfile::tempdir().unwrap();
        let network = set_up_in(dir.path(), lab());
        let others = [
            pod(
                "11111111-0000-0000-0000-000000000001",
                "fd00:fe05:1::2",
                "169.254.16.1",
            ),
            // Without a network, its addresses are no pod's.
            PodInfo {
                spec: Some(PodSpec {
                    network: Some(PodNetwork {
                        none: true,
                        ..Default::default()
                    }),
                    ..Default::default()
                }),
                ..pod("22222222-0000-0000-0000-000000000000", "", "169.254.16.2")
            },
        ];
        let mut info = pod("33333333-0000-0000-0000-000000000000", "", "");
        network.allocate(others.iter(), &mut info).unwrap();
        assert_eq!(
            (info.ipv6_address.as_str(), info.ipv4_address.as_str()),
            ("fd00:fe05:1::3", "169.254.16.2")
        );
        // A pod whose ID starts as another's would share its TAP device.
        let mut twin = pod("11111111-0000-0000-0000-000000000009", "", "");
        let error = network.allocate(others.iter(), &mut twin).unwrap_err();
        assert_eq!(error.code(), tonic::Code::AlreadyExists);
        // Short IDs do not: their TAP devices are named by all of them.
        let others = [
            pod("a1111111111", "fd00:fe05:1::2", "169.254.16.1"),
            pod(
                "a1111111-0000-0000-0000-000000000000",
                "fd00:fe05:1::3",
                "169.254.16.2",
            ),
        ];
        let mut info = pod("a1111111112", "", "");
        network.allocate(others.iter(), &mut info).unwrap();
        assert_eq!(info.ipv6_address, "fd00:fe05:1::4");
    }

    #[test]
    fn a_deleted_pods_ipv4_address_comes_round_last() {
        let dir = tempfile::tempdir().unwrap();
        let network = set_up_in(
            dir.path(),
            Config {
                pod_ipv4_range: (Ipv4Addr::new(10, 77, 0, 0), 30),
                ..lab()
            },
        );
        let ids = [
            "11111111-0000-0000-0000-000000000000",
            "22222222-0000-0000-0000-000000000000",
            "33333333-0000-0000-0000-000000000000",
        ];
        let mut pods: Vec<PodInfo> = Vec::new();
        for id in &ids[..2] {
            let mut info = pod(id, "", "");
            network.allocate(pods.iter(), &mut info).unwrap();
            pods.push(info);
        }
        assert_eq!(pods[1].ipv4_address, "10.77.0.2");
        let mut third = pod(ids[2], "", "");
        let error = network.allocate(pods.iter(), &mut third).unwrap_err();
        assert_eq!(error.code(), tonic::Code::ResourceExhausted);
        let deleted = pods.remove(0);
        network.release(&deleted);
        network.allocate(pods.iter(), &mut third).unwrap();
        assert_eq!(third.ipv4_address, "10.77.0.1");
        // Its IPv6 address never comes round.
        assert_eq!(third.ipv6_address, "fd00:fe05:1::5");
    }

    #[test]
    fn pods_kept_from_before_get_what_they_lack() {
        let dir = tempfile::tempdir().unwrap();
        let network = set_up_in(dir.path(), lab());
        let mut kept = vec![
            pod("a", "fd00:fe05:1::2", "169.254.16.1"),
            // An address of the first version's pool.
            pod("b", "fd00:fe05:1::3", "10.200.0.5"),
            // IPv6 only.
            pod("c", "fd00:fe05:1::4", ""),
            // The first version's: neither.
            pod("d", "", ""),
            // Another's IPv4 address.
            pod("e", "fd00:fe05:1::5", "169.254.16.1"),
            PodInfo {
                spec: Some(PodSpec {
                    network: Some(PodNetwork {
                        none: true,
                        ..Default::default()
                    }),
                    ..Default::default()
                }),
                ..pod("f", "", "10.200.0.6")
            },
        ];
        assert_eq!(network.adopt(&mut kept), ["b", "c", "d", "e"]);
        let addresses: Vec<(&str, &str)> = kept
            .iter()
            .map(|pod| (pod.ipv6_address.as_str(), pod.ipv4_address.as_str()))
            .collect();
        assert_eq!(
            addresses,
            [
                ("fd00:fe05:1::2", "169.254.16.1"),
                ("fd00:fe05:1::3", "169.254.16.2"),
                ("fd00:fe05:1::4", "169.254.16.3"),
                ("fd00:fe05:1::6", "169.254.16.4"),
                ("fd00:fe05:1::5", "169.254.16.5"),
                ("", "10.200.0.6"),
            ]
        );
        // A second start changes nothing.
        assert!(network.adopt(&mut kept).is_empty());
        assert_eq!(
            fs::read_to_string(dir.path().join("ipv4-addresses")).unwrap(),
            "next 169.254.16.6\n"
        );
    }

    #[tokio::test]
    async fn a_pod_without_its_addresses_is_not_attached() {
        let dir = tempfile::tempdir().unwrap();
        let network = set_up_in(dir.path(), lab());
        for (ipv6, ipv4) in [
            ("fd00:fe05:1::2", ""),
            ("fd00:fe05:1::2", "10.200.0.5"),
            ("", "169.254.16.1"),
        ] {
            let info = pod("11111111-0000-0000-0000-000000000000", ipv6, ipv4);
            let error = network.attach(&info).await.unwrap_err();
            assert_eq!(
                error.code(),
                tonic::Code::FailedPrecondition,
                "{ipv6} {ipv4}"
            );
        }
        let mut none = pod("11111111-0000-0000-0000-000000000000", "", "");
        none.spec = Some(PodSpec {
            network: Some(PodNetwork {
                none: true,
                ..Default::default()
            }),
            ..Default::default()
        });
        assert_eq!(network.attach(&none).await.unwrap(), None);
    }

    #[test]
    fn the_directory_knows_pods_by_both_addresses_and_tap() {
        let directory = Directory::default();
        let pods = [
            pod(
                "aaaaaaaa-0000-0000-0000-000000000000",
                "fd00::2",
                "169.254.16.1",
            ),
            pod("bbbbbbbb-0000-0000-0000-000000000000", "fd00::3", ""),
            pod("k7m2q9x4c1b", "fd00::5", "169.254.16.5"),
        ];
        directory.set(&pods.iter().collect::<Vec<_>>());
        let id = |address: &str| directory.by_address(address.parse().unwrap());
        let a = Some(pods[0].pod_id.clone());
        assert_eq!(id("fd00::2"), a);
        assert_eq!(id("169.254.16.1"), a);
        assert_eq!(id("::ffff:169.254.16.1"), a);
        assert_eq!(id("fd00::3"), Some(pods[1].pod_id.clone()));
        assert_eq!(id("169.254.16.2"), None);
        assert_eq!(directory.by_interface("pod-aaaaaaaa"), a);
        assert_eq!(
            directory.by_interface("pod-k7m2q9x4c1b"),
            Some(pods[2].pod_id.clone())
        );
        assert_eq!(directory.by_interface("eth0"), None);

        // A pod that left is known by its addresses for a while: the ends
        // of its connections come after it left.
        let now = Instant::now();
        directory.set_at(&[&pods[1]], now);
        assert_eq!(id("fd00::2"), a);
        assert_eq!(id("::ffff:169.254.16.1"), a);
        assert_eq!(directory.by_interface("pod-aaaaaaaa"), None);
        // A pod that holds the address is its.
        let c = pod(
            "cccccccc-0000-0000-0000-000000000000",
            "fd00::4",
            "169.254.16.1",
        );
        directory.set_at(&[&pods[1], &c], now);
        assert_eq!(id("169.254.16.1"), Some(c.pod_id.clone()));
        directory.set_at(&[&pods[1]], now);
        assert_eq!(id("169.254.16.1"), Some(c.pod_id.clone()));
        assert_eq!(id("fd00::2"), a);
        // Until it has been gone too long.
        directory.set_at(&[&pods[1]], now + DEPARTED);
        assert_eq!(id("fd00::2"), None);
        assert_eq!(id("169.254.16.1"), None);
        assert_eq!(id("fd00::4"), None);
        let long_ago = Instant::now().checked_sub(DEPARTED).unwrap();
        directory.set_at(&[], long_ago);
        assert_eq!(id("fd00::3"), None);
    }

    #[tokio::test]
    async fn dns_flows_are_left_to_the_resolver() {
        let flow = |destination: &str, port: u32, missed: u64| TrafficRecord {
            pod_id: "a".to_string(),
            missed,
            record: Some(traffic_record::Record::Flow(Flow {
                tuple: Some(Tuple {
                    protocol: 17,
                    destination_address: destination.to_string(),
                    destination_port: port,
                    ..Default::default()
                }),
                ..Default::default()
            })),
            ..Default::default()
        };
        let resolver = IpAddr::V4(dns_service::ADDRESS);
        assert!(is_dns_flow(&flow("169.254.0.53", 53, 0), resolver));
        assert!(!is_dns_flow(&flow("169.254.0.53", 853, 0), resolver));
        assert!(!is_dns_flow(&flow("8.8.8.8", 53, 0), resolver));

        let (from, receiver) = broadcast::channel(8);
        let (to, mut records) = broadcast::channel(8);
        let forwarding = tokio::spawn(forward_records(receiver, to, move |record| {
            !is_dns_flow(record, resolver)
        }));
        from.send(flow("169.254.0.53", 53, 2)).unwrap();
        from.send(flow("8.8.8.8", 53, 1)).unwrap();
        drop(from);
        forwarding.await.unwrap();
        let record = records.recv().await.unwrap();
        // What was left out counts as missed.
        assert_eq!(record.missed, 3);
        assert!(records.try_recv().is_err());
    }

    #[test]
    fn nat64s_pins_go() {
        let bpffs = tempfile::tempdir().unwrap();
        let feos = bpffs.path().join("feos");
        let pins = feos.join("nat64");
        fs::create_dir_all(&pins).unwrap();
        for map in ["nat64_pod_ipv4", "nat64_ipv4_pod", "nat64_stats"] {
            fs::write(pins.join(map), "").unwrap();
        }
        // Another program's pins keep the parent.
        fs::write(feos.join("other"), "").unwrap();
        remove_old_pins(&pins);
        assert!(!pins.exists());
        assert!(feos.join("other").exists());

        fs::remove_file(feos.join("other")).unwrap();
        fs::create_dir(&pins).unwrap();
        remove_old_pins(&pins);
        assert!(!feos.exists());
        // Nothing to remove.
        remove_old_pins(&pins);
    }

    /// Removes a NAT64 device, its pins and its counter. Needs
    /// CAP_NET_ADMIN, in a network namespace of its own:
    /// `sudo unshare -n <the test binary> --ignored nat64s_device_goes`.
    #[tokio::test]
    #[ignore]
    async fn nat64s_device_goes() {
        let alone = fs::read_to_string("/proc/self/net/dev").is_ok_and(|dev| {
            dev.lines()
                .skip(2)
                .all(|line| line.trim_start().starts_with("lo:"))
        });
        assert!(alone, "run this in a network namespace of its own");
        let handle = devices::connect().await.unwrap();
        handle
            .link()
            .add(rtnetlink::LinkDummy::new(OLD_NAT64_DEVICE).build())
            .execute()
            .await
            .unwrap();
        let bpffs = tempfile::tempdir().unwrap();
        let pins = bpffs.path().join("feos/nat64");
        fs::create_dir_all(&pins).unwrap();
        fs::write(pins.join("nat64_stats"), "").unwrap();
        let dir = tempfile::tempdir().unwrap();
        fs::write(dir.path().join(OLD_NAT64_COUNTER), "10.200.0.9").unwrap();
        fs::write(dir.path().join("next-ipv6-address"), "7").unwrap();

        remove_old_nat64(&handle, &pins, dir.path()).await;
        assert!(!devices::delete_link(&handle, OLD_NAT64_DEVICE)
            .await
            .unwrap());
        assert!(!bpffs.path().join("feos").exists());
        assert!(!dir.path().join(OLD_NAT64_COUNTER).exists());
        assert!(dir.path().join("next-ipv6-address").exists());
        // Again, with nothing left.
        remove_old_nat64(&handle, &pins, dir.path()).await;
    }
}
