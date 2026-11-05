// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd: the node agent, under systemd, feos-host-init or in a Kubernetes
//! pod, never PID 1.

use anyhow::{anyhow, bail, Context, Result};
use feos_utils::filesystem::DATA_ROOT;
use main_server::{run_server, run_task_service, ServerOptions};
use microvm_service::vmm::jail;
use nix::sys::prctl;
use pod_service::network;
use std::net::SocketAddr;
use std::path::Path;

/// The lock that one feosd per node holds on its state.
const STATE_LOCK: &str = "/var/lib/feos/feosd.lock";

const USAGE: &str = "usage: feosd [--tcp ADDRESS] [--self-upgrade] [--pod-prefix PREFIX]
             [--pod-ipv4-range PREFIX] [--dns-address ADDRESS]
             [--otlp-address ADDRESS] [--dns-upstream ADDRESS]... [--deny-prefix PREFIX]...
             [--diskd-socket PATH] [--authn-binary PATH] [--vms-cgroup PATH]
             [--suspend-on-stop]
             [--stop-grace SECONDS]
             [--control-plane HOST:PORT [--control-plane-name NAME]
              [--trust-domain NAME] [--join-token-file PATH] [--node-dir PATH]
              [--mesh-port PORT] [--mesh-address ADDRESS]]
       feosd --hand-over-vms --vms-cgroup PATH
       feosd --version

  --tcp ADDRESS            also serve the API on TCP, without authentication
  --self-upgrade           let UpgradeFeosBinary replace this binary
  --pod-prefix PREFIX      the IPv6 prefix the network routes to this node, such
                           as a /64; pods get their addresses from it. Without,
                           a node with a control plane takes a /64 of its
                           cluster's mesh (ADR 0017), and one without fails
                           pods with a network
  --pod-ipv4-range PREFIX  the IPv4 prefix pods get an address of each, for
                           leaving through masquerade; 169.254.16.0/20 by
                           default
  --dns-address ADDRESS    the pods' resolver, an IPv4 address the node holds;
                           169.254.0.53 by default
  --otlp-address ADDRESS   where pods send their spans over OTLP, ports 4317
                           and 4318, an IPv4 address the node holds as the
                           resolver's; 169.254.169.1 by default
  --dns-upstream ADDRESS   an upstream resolver of pods that name none
                           (repeatable); Google's and Cloudflare's by default
  --deny-prefix PREFIX     a prefix of either family no pod reaches
                           (repeatable)
  --diskd-socket PATH      diskd's socket, which the pods' disks come from;
                           /var/lib/feos/disk/diskd.sock by default
  --authn-binary PATH      feos-authn, which feosd starts for every running pod
                           with HTTP bindings (ADR 0028); next to feosd's own
                           binary by default
  --vms-cgroup PATH        the cgroup the microVMs' cgroups go below, from the
                           root of the hierarchy, such as
                           system.slice/feos-vms.service; the cgroup feosd is
                           given by default. feosd moves the microVMs below
                           its own cgroup there when it starts
  --suspend-on-stop        for a feosd whose microVMs end with it, as in a
                           container (ADR 0020): SIGTERM suspends every running
                           pod into its directory and records them in
                           /var/lib/feos/suspended-for-stop.json, after telling
                           the control plane that feosd stops and letting go
                           of it; then feosd exits. The next feosd resumes them
                           before it connects to the control plane
  --stop-grace SECONDS     how long a stop may take before feosd is killed, as
                           the pod's terminationGracePeriodSeconds or the
                           unit's TimeoutStopSec say; 300 by default. The node
                           connections' calls in flight get a tenth of it, 30 s
                           at most, and with --suspend-on-stop a pod whose VM
                           is busy is tried again until 10 s before its end
  --hand-over-vms          move the microVMs below feosd's own cgroup to
                           --vms-cgroup's and exit: feosd.service's
                           ExecStopPost=, so that a feosd from before
                           --vms-cgroup can be restarted
  --control-plane HOST:PORT
                           the control plane's port for nodes, which the node
                           joins and connects to (ADR 0015); without, the node
                           is on its own
  --control-plane-name NAME
                           the DNS name the control plane's certificate is
                           checked for, wherever feosd dials; HOST by default
  --trust-domain NAME      the control plane's SPIFFE trust domain; feos.local
                           by default
  --join-token-file PATH   the join token, read while the node has not joined;
                           /etc/feos/join-token by default
  --node-dir PATH          the node's key, certificate and trust bundle, and the
                           mesh's key and peers in mesh/; /var/lib/feos/node by
                           default
  --mesh-port PORT         the UDP port of the mesh's WireGuard device; 51820 by
                           default
  --mesh-address ADDRESS   where the other nodes reach this node's mesh; the
                           address from which it reaches the control plane by
                           default";

#[derive(Debug, Default, PartialEq)]
struct Args {
    tcp: Option<SocketAddr>,
    self_upgrade: bool,
    network: network::Config,
    diskd_socket: Option<std::path::PathBuf>,
    /// feos-authn's binary; next to feosd's by default.
    authn_binary: Option<std::path::PathBuf>,
    /// The cgroup the VMs' cgroups go below; the one feosd is given by
    /// default.
    vms_cgroup: Option<String>,
    hand_over_vms: bool,
    suspend_on_stop: bool,
    /// How long a stop may take; `main_server::STOP_GRACE` by default.
    stop_grace: Option<std::time::Duration>,
    /// The control plane, and how the node joins it.
    fleet: Option<fleet_client::Config>,
    /// Passed by feosd itself: the task service, and the process an upgrade
    /// exec'd.
    task_service: bool,
    restarted_after_upgrade: bool,
    version: bool,
}

impl Args {
    fn parse(args: impl IntoIterator<Item = String>) -> Result<Self> {
        let mut parsed = Self::default();
        let mut args = args.into_iter();
        // The first --dns-upstream replaces the defaults.
        let mut upstreams = Vec::new();
        let mut control_plane = None;
        let mut control_plane_name = None;
        let mut trust_domain = None;
        let mut join_token_file = None;
        let mut node_dir = None;
        let mut mesh_port = None;
        let mut mesh_address = None;
        while let Some(arg) = args.next() {
            let mut value = |what: &str| {
                args.next()
                    .with_context(|| format!("{arg} needs {what}\n{USAGE}"))
            };
            let flag = |e: String| anyhow!("{arg}: {e}");
            let network = &mut parsed.network;
            match arg.as_str() {
                "--tcp" => {
                    let address = value("an address")?;
                    let address = address
                        .parse()
                        .with_context(|| format!("--tcp {address}"))?;
                    parsed.tcp = Some(address);
                }
                "--pod-prefix" => {
                    network.pod_prefix =
                        Some(network::parse_pod_prefix(&value("a prefix")?).map_err(flag)?);
                }
                "--pod-ipv4-range" => {
                    network.pod_ipv4_range =
                        network::parse_pod_ipv4_range(&value("a prefix")?).map_err(flag)?;
                }
                "--dns-address" => {
                    let address = value("an address")?;
                    network.dns_address = address
                        .parse()
                        .with_context(|| format!("--dns-address {address}: an IPv4 address"))?;
                }
                "--otlp-address" => {
                    let address = value("an address")?;
                    network.otlp_address = address
                        .parse()
                        .with_context(|| format!("--otlp-address {address}: an IPv4 address"))?;
                }
                "--dns-upstream" => {
                    let address = value("an address")?;
                    upstreams.push(
                        address
                            .parse()
                            .with_context(|| format!("--dns-upstream {address}"))?,
                    );
                }
                "--deny-prefix" => {
                    let prefix = value("a prefix")?.parse().map_err(flag)?;
                    network.deny_prefixes.push(prefix);
                }
                "--diskd-socket" => parsed.diskd_socket = Some(value("a path")?.into()),
                "--authn-binary" => parsed.authn_binary = Some(value("a path")?.into()),
                "--vms-cgroup" => {
                    let path = value("a cgroup")?;
                    let cgroup = path.trim_matches('/');
                    if cgroup.is_empty() || cgroup.split('/').any(|c| c == "." || c == "..") {
                        bail!("--vms-cgroup {path}: a cgroup below the root\n{USAGE}");
                    }
                    parsed.vms_cgroup = Some(cgroup.to_string());
                }
                "--hand-over-vms" => parsed.hand_over_vms = true,
                "--suspend-on-stop" => parsed.suspend_on_stop = true,
                "--stop-grace" => {
                    let seconds = value("seconds")?;
                    parsed.stop_grace = Some(
                        seconds
                            .parse::<u64>()
                            .ok()
                            .filter(|seconds| *seconds > 0)
                            .map(std::time::Duration::from_secs)
                            .with_context(|| {
                                format!("--stop-grace {seconds}: a number of seconds above 0")
                            })?,
                    );
                }
                "--control-plane" => control_plane = Some(value("HOST:PORT")?),
                "--control-plane-name" => control_plane_name = Some(value("a name")?),
                "--trust-domain" => trust_domain = Some(value("a name")?),
                "--join-token-file" => join_token_file = Some(value("a path")?.into()),
                "--node-dir" => node_dir = Some(value("a path")?.into()),
                "--mesh-port" => {
                    let port = value("a port")?;
                    mesh_port = Some(
                        port.parse::<u16>()
                            .ok()
                            .filter(|port| *port != 0)
                            .with_context(|| format!("--mesh-port {port}: a port"))?,
                    );
                }
                "--mesh-address" => {
                    let address = value("an address")?;
                    mesh_address = Some(
                        address
                            .parse::<std::net::IpAddr>()
                            .with_context(|| format!("--mesh-address {address}"))?,
                    );
                }
                "--self-upgrade" => parsed.self_upgrade = true,
                "--task-service" => parsed.task_service = true,
                "--restarted-after-upgrade" => parsed.restarted_after_upgrade = true,
                "--version" => parsed.version = true,
                other => bail!("unknown argument {other}\n{USAGE}"),
            }
        }
        if parsed.hand_over_vms && parsed.vms_cgroup.is_none() {
            bail!("--hand-over-vms needs --vms-cgroup\n{USAGE}");
        }
        if !upstreams.is_empty() {
            parsed.network.dns_upstreams = upstreams;
        }
        if let Some(address) = control_plane {
            let trust_domain = trust_domain
                .as_deref()
                .unwrap_or(fleet_client::TRUST_DOMAIN);
            let mut config =
                fleet_client::Config::new(&address, control_plane_name.as_deref(), trust_domain)
                    .map_err(|e| anyhow!("--control-plane: {e}\n{USAGE}"))?;
            if let Some(file) = join_token_file {
                config.join_token_file = file;
            }
            if let Some(dir) = node_dir {
                config.node_dir = dir;
            }
            // Without a prefix of its network's, the node is on its
            // cluster's mesh (ADR 0017).
            if parsed.network.pod_prefix.is_none() {
                parsed.network.mesh = Some(network::MeshConfig {
                    dir: config.node_dir.join("mesh"),
                    port: mesh_port.unwrap_or(network::MESH_PORT),
                });
                config.mesh_address = mesh_address;
            } else if mesh_port.is_some() || mesh_address.is_some() {
                bail!(
                    "--mesh-port and --mesh-address are for a node without --pod-prefix\n{USAGE}"
                );
            }
            parsed.fleet = Some(config);
        } else if let Some(trust_domain) = trust_domain {
            fleet_client::check_trust_domain(&trust_domain)
                .map_err(|e| anyhow!("--trust-domain: {e}\n{USAGE}"))?;
        } else if mesh_port.is_some() || mesh_address.is_some() {
            bail!("--mesh-port and --mesh-address need --control-plane\n{USAGE}");
        }
        parsed
            .network
            .check()
            .map_err(|e| anyhow!("{e}\n{USAGE}"))?;
        Ok(parsed)
    }
}

fn main() -> Result<()> {
    // The resolver's sockets, if systemd, feos-host-init or the feosd
    // before a self-upgrade handed them over (ADR 0010).
    // SAFETY: the process has no other threads yet.
    let dns_sockets = unsafe { feos_utils::listen_fds::take() };
    serve(dns_sockets)
}

#[tokio::main]
async fn serve(dns_sockets: Vec<(std::os::fd::OwnedFd, String)>) -> Result<()> {
    let args = Args::parse(std::env::args().skip(1))?;
    if args.version {
        println!("feosd {}", feos_utils::version::full_version_string());
        return Ok(());
    }
    if args.task_service {
        return run_task_service().await;
    }
    if args.hand_over_vms {
        hand_over_vms(args.vms_cgroup.as_deref().unwrap_or_default());
        return Ok(());
    }
    if std::process::id() == 1 {
        bail!(
            "feosd does not run as PID 1: feos-host-init runs it, or in a Kubernetes pod the \
             pod's shared process namespace (shareProcessNamespace)"
        );
    }

    // One feosd per node: another one, such as the old pod of a rollout
    // that still suspends its pods, has to end first. Held until feosd
    // ends; a self-upgrade's exec takes it again.
    let _state = feos_utils::state_lock::take(Path::new(STATE_LOCK), |holder| match holder {
        Some(pid) => eprintln!(
            "feosd: Another feosd runs on {DATA_ROOT} (pid {pid} in its namespace): waiting \
             for it to end"
        ),
        None => eprintln!("feosd: Another feosd runs on {DATA_ROOT}: waiting for it to end"),
    })
    .await
    .with_context(|| format!("lock {STATE_LOCK}"))?;

    // feosd inherits the orphans of its children, such as the processes
    // of a task service that died, and reaps them.
    prctl::set_child_subreaper(true).context("become a subreaper")?;
    feos_utils::reaper::spawn();
    match main_server::cgroup::enter_leaf() {
        Ok(given) => {
            jail::set_cgroup_base(args.vms_cgroup.as_deref().unwrap_or(&given));
            report(jail::adopt(&given));
        }
        Err(e) => {
            eprintln!("feosd: Failed to move into a leaf of its cgroup: {e}");
            if let Some(vms) = &args.vms_cgroup {
                jail::set_cgroup_base(vms);
            }
        }
    }

    let fleet = args.fleet.map(|config| fleet_client::Config {
        pod_prefix: args
            .network
            .pod_prefix
            .map(|(prefix, len)| format!("{prefix}/{len}"))
            .unwrap_or_default(),
        version: feos_utils::version::full_version_string(),
        ..config
    });
    run_server(ServerOptions {
        tcp: args.tcp,
        network: args.network,
        dns_sockets,
        diskd_socket: args.diskd_socket,
        fleet,
        suspend_on_stop: args.suspend_on_stop,
        stop_grace: args.stop_grace.unwrap_or(main_server::STOP_GRACE),
        authn: match args.authn_binary {
            Some(binary) => pod_service::authn::Config { binary, jail: true },
            None => pod_service::authn::Config::beside_feosd(),
        },
        ..ServerOptions::new(args.restarted_after_upgrade, args.self_upgrade)
    })
    .await?;
    // SIGTERM, and the control plane knows: nothing is left to wait for.
    std::process::exit(0)
}

/// Moves the VMs below the cgroup feosd is given to `vms`, for a feosd
/// that starts next: feosd.service's ExecStopPost=. Errors are logged; the
/// VMs run on either way.
fn hand_over_vms(vms: &str) {
    match main_server::cgroup::given() {
        Ok(given) => {
            jail::set_cgroup_base(vms);
            report(jail::adopt(&given));
        }
        Err(e) => eprintln!("feosd: Failed to find its cgroup: {e}"),
    }
}

fn report(adopted: jail::Adopted) {
    if !adopted.moved.is_empty() {
        eprintln!(
            "feosd: Moved the cgroups of {} VMs to {}: {}",
            adopted.moved.len(),
            jail::cgroup("").display(),
            adopted.moved.join(", ")
        );
    }
    for error in adopted.errors {
        eprintln!("feosd: Failed to move a VM's cgroup: {error}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(args: &[&str]) -> Result<Args> {
        Args::parse(args.iter().map(|arg| arg.to_string()))
    }

    #[test]
    fn arguments_are_parsed() {
        let args = parse(&["--tcp", "[::]:1337", "--self-upgrade"]).unwrap();
        assert_eq!(args.tcp, Some("[::]:1337".parse().unwrap()));
        assert!(args.self_upgrade && !args.task_service);
        let args = parse(&["--diskd-socket", "/run/diskd.sock"]).unwrap();
        assert_eq!(args.diskd_socket, Some("/run/diskd.sock".into()));
        let authn = parse(&["--authn-binary", "/opt/feos-authn"]).unwrap();
        assert_eq!(authn.authn_binary, Some("/opt/feos-authn".into()));
        assert!(!args.suspend_on_stop);
        assert!(parse(&["--suspend-on-stop"]).unwrap().suspend_on_stop);
        assert_eq!(args.stop_grace, None);
        assert_eq!(
            parse(&["--stop-grace", "120"]).unwrap().stop_grace,
            Some(std::time::Duration::from_secs(120))
        );
        for bad in ["0", "-1", "2m", ""] {
            assert!(parse(&["--stop-grace", bad]).is_err(), "{bad}");
        }
        assert_eq!(parse(&[]).unwrap(), Args::default());
    }

    #[test]
    fn the_vms_cgroup_is_below_the_root() {
        let args = parse(&["--vms-cgroup", "/system.slice/feos-vms.service/"]).unwrap();
        assert_eq!(
            args.vms_cgroup.as_deref(),
            Some("system.slice/feos-vms.service")
        );
        assert!(!args.hand_over_vms);
        let args = parse(&["--hand-over-vms", "--vms-cgroup", "feos-vms"]).unwrap();
        assert!(args.hand_over_vms);
        for bad in [
            &["--vms-cgroup", "/"][..],
            &["--vms-cgroup", "system.slice/../x"],
            &["--hand-over-vms"],
        ] {
            assert!(parse(bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn the_control_plane_is_configured() {
        assert_eq!(parse(&[]).unwrap().fleet, None);
        let fleet = parse(&["--control-plane", "ferun.example.com:7443"])
            .unwrap()
            .fleet
            .unwrap();
        assert_eq!(fleet.address, "ferun.example.com:7443");
        assert_eq!(fleet.name, "ferun.example.com");
        assert_eq!(fleet.trust_domain, "feos.local");
        assert_eq!(
            fleet.join_token_file,
            std::path::Path::new("/etc/feos/join-token")
        );
        assert_eq!(fleet.node_dir, std::path::Path::new("/var/lib/feos/node"));
        let fleet = parse(&[
            "--control-plane",
            "10.43.0.17:7443",
            "--control-plane-name",
            "ferun-nodes.ferun.svc.cluster.local",
            "--trust-domain",
            "run.fecode.dev",
            "--join-token-file",
            "/run/credentials/feosd/join-token",
            "--node-dir",
            "/srv/feos/node",
        ])
        .unwrap()
        .fleet
        .unwrap();
        assert_eq!(fleet.address, "10.43.0.17:7443");
        assert_eq!(fleet.name, "ferun-nodes.ferun.svc.cluster.local");
        assert_eq!(fleet.trust_domain, "run.fecode.dev");
        assert_eq!(
            fleet.join_token_file,
            std::path::Path::new("/run/credentials/feosd/join-token")
        );
        assert_eq!(fleet.node_dir, std::path::Path::new("/srv/feos/node"));
        for bad in [
            &["--mesh-port", "51821"][..],
            &[
                "--control-plane",
                "ferun.example.com:7443",
                "--mesh-port",
                "0",
            ],
            &[
                "--control-plane",
                "ferun.example.com:7443",
                "--mesh-address",
                "node-b",
            ],
            &[
                "--control-plane",
                "ferun.example.com:7443",
                "--pod-prefix",
                "2001:db8:1:2::/64",
                "--mesh-port",
                "51821",
            ],
        ] {
            assert!(parse(bad).is_err(), "{bad:?}");
        }
        for bad in [
            &["--control-plane"][..],
            &["--control-plane", "ferun.example.com"],
            &["--control-plane", "10.43.0.17:7443"],
            &[
                "--control-plane",
                "ferun.example.com:7443",
                "--trust-domain",
                "Feos",
            ],
            &[
                "--control-plane",
                "10.43.0.17:7443",
                "--control-plane-name",
                "10.43.0.17",
            ],
            &["--trust-domain", "feos.local:443"],
            &["--node-dir"],
        ] {
            assert!(parse(bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn a_node_without_a_prefix_is_on_the_mesh() {
        let args = parse(&["--control-plane", "ferun.example.com:7443"]).unwrap();
        let mesh = args.network.mesh.unwrap();
        assert_eq!(mesh.dir, std::path::Path::new("/var/lib/feos/node/mesh"));
        assert_eq!(mesh.port, 51820);
        assert_eq!(args.fleet.unwrap().mesh_address, None);
        let args = parse(&[
            "--control-plane",
            "ferun.example.com:7443",
            "--node-dir",
            "/srv/feos/node",
            "--mesh-port",
            "51821",
            "--mesh-address",
            "10.0.0.11",
        ])
        .unwrap();
        let mesh = args.network.mesh.unwrap();
        assert_eq!(mesh.dir, std::path::Path::new("/srv/feos/node/mesh"));
        assert_eq!(mesh.port, 51821);
        assert_eq!(
            args.fleet.unwrap().mesh_address,
            Some("10.0.0.11".parse().unwrap())
        );
        // A prefix of its network's, or no control plane: no mesh.
        let args = parse(&[
            "--control-plane",
            "ferun.example.com:7443",
            "--pod-prefix",
            "2001:db8:1:2::/64",
        ])
        .unwrap();
        assert_eq!(args.network.mesh, None);
        assert_eq!(parse(&[]).unwrap().network.mesh, None);
    }

    #[test]
    fn bad_arguments_are_refused() {
        assert!(parse(&["--tcp"]).is_err());
        assert!(parse(&["--tcp", "nowhere"]).is_err());
        assert!(parse(&["--frobnicate"]).is_err());
        for bad in [
            &["--pod-prefix", "10.0.0.0/8"][..],
            &["--pod-prefix", "fd00::1/64"],
            &["--pod-prefix"],
            &["--pod-ipv4-range", "fd00::/64"],
            &["--pod-ipv4-range", "169.254.16.0/31"],
            &["--pod-ipv4-range", "169.254.0.0/16"],
            &["--dns-address", "fd00::53"],
            &["--dns-address", "169.254.0.1"],
            &["--dns-address", "169.254.16.53"],
            &["--otlp-address", "fd00::1"],
            &["--otlp-address", "169.254.0.1"],
            &["--otlp-address", "169.254.0.53"],
            &["--otlp-address", "169.254.169.254"],
            &["--otlp-address", "169.254.16.9"],
            &["--otlp-address", "10.0.0.1", "--deny-prefix", "10.0.0.0/8"],
            &["--dns-upstream", "dns.google"],
            &["--deny-prefix", "169.254.169.254"],
        ] {
            assert!(parse(bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn the_network_is_configured() {
        let args = parse(&[]).unwrap();
        assert_eq!(args.network, network::Config::default());
        assert_eq!(args.network.pod_prefix, None);
        let args = parse(&[
            "--pod-prefix",
            "fd00:fe05:1::/64",
            "--pod-ipv4-range",
            "10.77.16.0/24",
            "--dns-address",
            "10.77.0.53",
            "--otlp-address",
            "10.77.0.17",
            "--dns-upstream",
            "9.9.9.9",
            "--dns-upstream",
            "2620:fe::fe",
            "--deny-prefix",
            "169.254.169.254/32",
            "--deny-prefix",
            "fd00:ec2::/64",
        ])
        .unwrap();
        let network = args.network;
        assert_eq!(
            network.pod_prefix,
            Some(("fd00:fe05:1::".parse().unwrap(), 64))
        );
        assert_eq!(network.pod_ipv4_range, ("10.77.16.0".parse().unwrap(), 24));
        assert_eq!(
            network.dns_address,
            "10.77.0.53".parse::<std::net::Ipv4Addr>().unwrap()
        );
        assert_eq!(
            network.otlp_address,
            "10.77.0.17".parse::<std::net::Ipv4Addr>().unwrap()
        );
        assert_eq!(
            network.dns_upstreams,
            [
                "9.9.9.9".parse::<std::net::IpAddr>().unwrap(),
                "2620:fe::fe".parse().unwrap()
            ]
        );
        assert_eq!(
            network.deny_prefixes,
            [
                "169.254.169.254/32".parse().unwrap(),
                "fd00:ec2::/64".parse().unwrap()
            ]
        );
    }
}
