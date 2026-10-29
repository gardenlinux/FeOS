// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Pods on a node (ADR 0004). A pod's containers, images, tools and
//! watches are reached with `--pod <id>` on those commands.

use anyhow::{bail, Context, Result};
use clap::{Args, Subcommand};
use crossterm::terminal::{disable_raw_mode, enable_raw_mode};
use crossterm::tty::IsTty;
use feos_proto::pod_service::{
    pod_service_client::PodServiceClient, ContainerSpec, ContainerState, CreatePodRequest,
    DeletePodRequest, GetPodRequest, HibernatePodRequest, ListPodsRequest, PodConsoleInput,
    PodInfo, PodNetwork, PodSpec, PodState, PodVolume, Resources, RestorePodRequest,
    ResumePodRequest, StartPodRequest, StopPodRequest, StreamPodEventsRequest, SuspendPodRequest,
    VolumeMount,
};
use std::collections::HashMap;
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::mpsc;
use tokio_stream::StreamExt;
use tonic::transport::Channel;

/// How long --wait waits for a pod to come up.
const WAIT_TIMEOUT: Duration = Duration::from_secs(300);

#[derive(Args, Debug)]
pub struct PodArgs {
    #[arg(
        short,
        long,
        global = true,
        env = "FEOS_ADDRESS",
        default_value = "unix:/run/feos/api.sock"
    )]
    pub address: String,

    #[command(subcommand)]
    command: PodCommand,
}

#[derive(Subcommand, Debug)]
pub enum PodCommand {
    /// Create a pod with one container, and bring it up
    Create(Box<CreatePodOptions>),
    /// List the pods
    List,
    /// Show a pod and its containers
    Get { pod_id: String },
    /// Bring a stopped or failed pod up again
    Start {
        pod_id: String,
        #[arg(long, help = "Wait until the pod runs")]
        wait: bool,
    },
    /// Shut a pod's microVM down; its disk stays
    Stop { pod_id: String },
    /// Remove a pod, its microVM and its disk
    Delete { pod_id: String },
    /// Write a running pod to a snapshot on the node
    Suspend { pod_id: String },
    /// Restore a suspended or hibernated pod
    Resume { pod_id: String },
    /// Take a pod off the node into object storage: its disk and memory
    Hibernate { pod_id: String },
    /// Bring a hibernated pod back, here or on a node that does not have it
    Restore {
        pod_id: String,
        #[arg(
            long,
            help = "The S3 key of the checkpoint's head; the pod's own when not given"
        )]
        head: Option<String>,
        #[arg(long, help = "Boot the pod anew on its disk, without its memory")]
        cold: bool,
        #[arg(long, help = "Wait until the pod runs")]
        wait: bool,
    },
    /// Attach to the serial console of a pod's microVM; Ctrl+] leaves
    Console { pod_id: String },
    /// Watch the pods' state changes
    Events {
        #[arg(help = "Only this pod's")]
        pod_id: Option<String>,
    },
}

#[derive(Args, Debug)]
pub struct CreatePodOptions {
    #[arg(
        long,
        help = "The pod's ID, 11 characters of lowercase Crockford base32 (or a UUID); a new one when not given"
    )]
    pod_id: Option<String>,

    #[arg(long, default_value_t = 1)]
    vcpus: u32,

    #[arg(long, default_value_t = 512, help = "Memory in MiB")]
    memory: u64,

    #[arg(long, default_value = "main", help = "The container's name")]
    name: String,

    #[arg(
        long,
        help = "The container's image, e.g. docker.io/library/busybox:1.36; without, the pod has no container"
    )]
    image: Option<String>,

    #[arg(long, requires = "image", value_parser = parse_key_val, help = "KEY=value for the container (repeatable)")]
    env: Vec<(String, String)>,

    #[arg(long, value_parser = parse_key_val, help = "KEY=value for the pod (repeatable)")]
    label: Vec<(String, String)>,

    #[arg(
        long,
        value_name = "KEY=value",
        value_parser = parse_key_val,
        conflicts_with = "no_network",
        help = "KEY=value the pod reads from its metadata service at 169.254.169.254 (repeatable); no credentials"
    )]
    metadata: Vec<(String, String)>,

    #[arg(long, help = "No network: the pod can pull no image then")]
    no_network: bool,

    #[arg(
        long = "security-group",
        value_name = "ID",
        conflicts_with = "no_network",
        help = "A security group of the pod (repeatable); without, the node's default group: out to the internet, nothing in"
    )]
    security_groups: Vec<String>,

    #[arg(
        long = "name-table",
        value_name = "ID",
        conflicts_with = "no_network",
        help = "A name table whose names the node's resolver answers the pod (repeatable)"
    )]
    name_tables: Vec<String>,

    #[arg(
        long = "dns-upstream",
        value_name = "ADDRESS",
        conflicts_with = "no_network",
        help = "A resolver the node's resolver asks for the pod (repeatable); the node's when not given"
    )]
    dns_upstreams: Vec<String>,

    #[arg(
        long = "volume",
        value_name = "NAME=VOLUME_ID",
        value_parser = parse_volume,
        help = "A volume of the pod, under a name of the pod's (repeatable); it must be attached to no other pod"
    )]
    volumes: Vec<PodVolume>,

    #[arg(
        long = "mount",
        value_name = "NAME:PATH[:ro]",
        requires = "image",
        value_parser = parse_mount,
        help = "Mounts the pod's volume NAME in the container at PATH, read-only with :ro (repeatable)"
    )]
    mounts: Vec<VolumeMount>,

    #[arg(long, help = "Wait until the pod runs")]
    wait: bool,

    #[arg(
        last = true,
        requires = "image",
        help = "The container's command; the image's when not given"
    )]
    command: Vec<String>,
}

fn parse_key_val(s: &str) -> Result<(String, String), String> {
    s.split_once('=')
        .map(|(key, value)| (key.to_string(), value.to_string()))
        .ok_or_else(|| format!("invalid KEY=value format: {s}"))
}

fn parse_volume(s: &str) -> Result<PodVolume, String> {
    let (name, volume_id) = parse_key_val(s).map_err(|_| format!("{s:?} is not NAME=VOLUME_ID"))?;
    Ok(PodVolume { name, volume_id })
}

fn parse_mount(s: &str) -> Result<VolumeMount, String> {
    let (volume, rest) = s
        .split_once(':')
        .ok_or_else(|| format!("{s:?} is not NAME:PATH[:ro]"))?;
    let (path, read_only) = match rest.strip_suffix(":ro") {
        Some(path) => (path, true),
        None => (rest, false),
    };
    Ok(VolumeMount {
        volume: volume.to_string(),
        path: path.to_string(),
        read_only,
    })
}

pub async fn handle_pod_command(args: PodArgs) -> Result<()> {
    let mut client = PodServiceClient::new(crate::transport::channel(&args.address).await?);

    match args.command {
        PodCommand::Create(opts) => create_pod(&mut client, *opts).await?,
        PodCommand::List => list_pods(&mut client).await?,
        PodCommand::Get { pod_id } => get_pod(&mut client, pod_id).await?,
        PodCommand::Start { pod_id, wait } => {
            client
                .start_pod(StartPodRequest {
                    pod_id: pod_id.clone(),
                    pull_token: String::new(),
                })
                .await?;
            if wait {
                wait_until_running(&mut client, &pod_id).await?;
                println!("Pod {pod_id} runs");
            } else {
                println!("Pod {pod_id} is coming up");
            }
        }
        PodCommand::Stop { pod_id } => {
            client
                .stop_pod(StopPodRequest {
                    pod_id: pod_id.clone(),
                })
                .await?;
            println!("Pod {pod_id} is stopped");
        }
        PodCommand::Delete { pod_id } => {
            client
                .delete_pod(DeletePodRequest {
                    pod_id: pod_id.clone(),
                })
                .await?;
            println!("Pod {pod_id} is deleted");
        }
        PodCommand::Suspend { pod_id } => {
            let response = client
                .suspend_pod(SuspendPodRequest {
                    pod_id: pod_id.clone(),
                })
                .await?
                .into_inner();
            println!(
                "Pod {pod_id} is suspended; its memory takes {} MiB on disk",
                response.memory_bytes >> 20
            );
        }
        PodCommand::Resume { pod_id } => {
            client
                .resume_pod(ResumePodRequest {
                    pod_id: pod_id.clone(),
                })
                .await?;
            println!("Pod {pod_id} runs again");
        }
        PodCommand::Hibernate { pod_id } => {
            let response = client
                .hibernate_pod(HibernatePodRequest {
                    pod_id: pod_id.clone(),
                    ..Default::default()
                })
                .await?
                .into_inner();
            let checkpoint = response.checkpoint.unwrap_or_default();
            println!(
                "Pod {pod_id} is hibernated in {}: {} MiB of memory, {} MiB on its disk",
                checkpoint.head_key,
                response.memory_bytes >> 20,
                response.disk_bytes >> 20
            );
        }
        PodCommand::Restore {
            pod_id,
            head,
            cold,
            wait,
        } => {
            let response = client
                .restore_pod(RestorePodRequest {
                    pod_id: pod_id.clone(),
                    head_key: head.unwrap_or_default(),
                    cold,
                })
                .await?
                .into_inner();
            if response.memory_restored {
                println!("Pod {pod_id} runs again, with its memory");
            } else {
                println!("Pod {pod_id} boots anew on its disk: {}", response.reason);
                if wait {
                    wait_until_running(&mut client, &pod_id).await?;
                    println!("Pod {pod_id} runs");
                }
            }
        }
        PodCommand::Console { pod_id } => console(&mut client, pod_id).await?,
        PodCommand::Events { pod_id } => watch_events(&mut client, pod_id).await?,
    }
    Ok(())
}

fn create_pod_request(opts: CreatePodOptions) -> CreatePodRequest {
    let network = if opts.no_network {
        Some(PodNetwork {
            none: true,
            ..Default::default()
        })
    } else if !opts.security_groups.is_empty()
        || !opts.dns_upstreams.is_empty()
        || !opts.name_tables.is_empty()
    {
        Some(PodNetwork {
            security_groups: opts.security_groups,
            dns_upstreams: opts.dns_upstreams,
            name_tables: opts.name_tables,
            ..Default::default()
        })
    } else {
        None
    };
    CreatePodRequest {
        pod_id: opts.pod_id.unwrap_or_else(feos_utils::pod_id::mint),
        spec: Some(PodSpec {
            resources: Some(Resources {
                vcpus: opts.vcpus,
                memory_mib: opts.memory,
            }),
            containers: opts
                .image
                .map(|image_ref| ContainerSpec {
                    name: opts.name,
                    image_ref,
                    command: opts.command,
                    env: opts.env.into_iter().collect(),
                    volume_mounts: opts.mounts,
                })
                .into_iter()
                .collect(),
            network,
            labels: opts.label.into_iter().collect::<HashMap<_, _>>(),
            volumes: opts.volumes,
            metadata: opts.metadata.into_iter().collect(),
        }),
        image_layers: None,
        pull_token: String::new(),
    }
}

async fn create_pod(client: &mut PodServiceClient<Channel>, opts: CreatePodOptions) -> Result<()> {
    let wait = opts.wait;
    let request = create_pod_request(opts);
    let pod_id = request.pod_id.clone();
    client.create_pod(request).await?;
    if wait {
        wait_until_running(client, &pod_id).await?;
        println!("Pod {pod_id} runs");
    } else {
        println!("Pod {pod_id} is coming up; 'feosctl pod get {pod_id}' shows how far it got");
    }
    Ok(())
}

/// Waits until the pod runs, and fails if it fails.
async fn wait_until_running(client: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<()> {
    let deadline = tokio::time::Instant::now() + WAIT_TIMEOUT;
    loop {
        let pod = client
            .get_pod(GetPodRequest {
                pod_id: pod_id.to_string(),
            })
            .await?
            .into_inner();
        match pod.state() {
            PodState::Running => return Ok(()),
            PodState::Pending => {}
            state => bail!("pod {pod_id} is {}: {}", state_name(state), pod.reason),
        }
        if tokio::time::Instant::now() >= deadline {
            bail!(
                "pod {pod_id} did not come up within {}s",
                WAIT_TIMEOUT.as_secs()
            );
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }
}

async fn list_pods(client: &mut PodServiceClient<Channel>) -> Result<()> {
    let pods = client
        .list_pods(ListPodsRequest {})
        .await?
        .into_inner()
        .pods;
    if pods.is_empty() {
        println!("No pods");
        return Ok(());
    }
    println!(
        "{:<38} {:<10} {:<10} REASON",
        "POD_ID", "STATE", "CONTAINERS"
    );
    println!("{:-<38} {:-<10} {:-<10} {:-<20}", "", "", "", "");
    for pod in pods {
        let containers = pod.spec.as_ref().map_or(0, |spec| spec.containers.len());
        println!(
            "{:<38} {:<10} {:<10} {}",
            pod.pod_id,
            state_name(pod.state()),
            containers,
            pod.reason
        );
    }
    Ok(())
}

async fn get_pod(client: &mut PodServiceClient<Channel>, pod_id: String) -> Result<()> {
    let pod = client.get_pod(GetPodRequest { pod_id }).await?.into_inner();
    print_pod(&pod);
    Ok(())
}

fn print_pod(pod: &PodInfo) {
    println!("Pod {}", pod.pod_id);
    println!("  State: {}", state_name(pod.state()));
    if !pod.reason.is_empty() {
        println!("  Reason: {}", pod.reason);
    }
    let Some(spec) = &pod.spec else {
        return;
    };
    if let Some(resources) = &spec.resources {
        println!(
            "  Resources: {} vCPUs, {} MiB",
            resources.vcpus, resources.memory_mib
        );
    }
    if let Some(microvm) = &pod.microvm {
        println!(
            "  MicroVM: pid {}, guest {}{}",
            microvm.pid,
            microvm.guest_version,
            if microvm.guest_attached {
                " (attached)"
            } else {
                ""
            }
        );
    }
    let network = spec.network.clone().unwrap_or_default();
    match (pod.ipv6_address.as_str(), pod.ipv4_address.as_str()) {
        _ if network.none => println!("  Network: none"),
        ("", "") => println!("  Addresses: none yet"),
        (ipv6, ipv4) => {
            for (family, address) in [("IPv6", ipv6), ("IPv4", ipv4)] {
                if !address.is_empty() {
                    println!("  {family} address: {address}");
                }
            }
        }
    }
    if let Some(checkpoint) = &pod.checkpoint {
        println!(
            "  Checkpoint: {}{}",
            checkpoint.head_key,
            if checkpoint.memory {
                ""
            } else {
                " (disk only)"
            }
        );
    }
    if !network.security_groups.is_empty() {
        println!("  Security groups: {}", network.security_groups.join(", "));
    }
    if !network.dns_upstreams.is_empty() {
        println!("  DNS upstreams: {}", network.dns_upstreams.join(", "));
    }
    if !network.name_tables.is_empty() {
        println!("  Name tables: {}", network.name_tables.join(", "));
    }
    for (key, value) in &spec.labels {
        println!("  Label: {key}={value}");
    }
    let mut metadata: Vec<_> = spec.metadata.iter().collect();
    metadata.sort();
    for (key, value) in metadata {
        println!("  Metadata: {key}={value}");
    }
    for volume in &spec.volumes {
        println!("  Volume: {} ({})", volume.name, volume.volume_id);
    }
    println!("  Containers:");
    for container in &spec.containers {
        let status = pod
            .containers
            .iter()
            .find(|status| status.name == container.name);
        let state = match status {
            Some(status) => match (status.state(), status.exit_code) {
                (ContainerState::Exited, Some(code)) => format!("exited ({code})"),
                (state, _) => container_state_name(state).to_string(),
            },
            None => "-".to_string(),
        };
        println!(
            "    {} ({}): {}{}",
            container.name,
            container.image_ref,
            state,
            match container.command.is_empty() {
                true => String::new(),
                false => format!(", {}", container.command.join(" ")),
            }
        );
    }
}

/// The console in the terminal, raw, until Ctrl+] or its end.
async fn console(client: &mut PodServiceClient<Channel>, pod_id: String) -> Result<()> {
    if !std::io::stdin().is_tty() {
        bail!("Cannot enter interactive console mode without a TTY.");
    }
    let (input_tx, input_rx) = mpsc::channel(10);
    input_tx
        .send(PodConsoleInput {
            pod_id: pod_id.clone(),
            input: Vec::new(),
        })
        .await?;
    let mut output = client
        .stream_pod_console(tokio_stream::wrappers::ReceiverStream::new(input_rx))
        .await?
        .into_inner();
    println!("Connected to the console of pod {pod_id}. Press Ctrl+] to leave.");

    struct RawModeGuard;
    impl Drop for RawModeGuard {
        fn drop(&mut self) {
            if let Err(e) = disable_raw_mode() {
                eprintln!("\r\nFailed to disable raw mode: {e}. Please reset your terminal.\r\n");
            }
        }
    }
    enable_raw_mode().context("Failed to enable terminal raw mode")?;
    let _guard = RawModeGuard;

    let output_task = tokio::spawn(async move {
        let mut stdout = tokio::io::stdout();
        while let Some(message) = output.next().await {
            match message {
                Ok(message) => {
                    if stdout.write_all(&message.output).await.is_err()
                        || stdout.flush().await.is_err()
                    {
                        break;
                    }
                }
                Err(e) => {
                    eprintln!("\r\nThe console ended: {}\r\n", e.message());
                    break;
                }
            }
        }
    });
    let input_task = tokio::spawn(async move {
        let mut stdin = tokio::io::stdin();
        let mut buffer = [0u8; 1];
        // Ctrl+] leaves.
        while let Ok(1) = stdin.read(&mut buffer).await {
            if buffer[0] == 29 {
                break;
            }
            let input = PodConsoleInput {
                pod_id: String::new(),
                input: buffer.to_vec(),
            };
            if input_tx.send(input).await.is_err() {
                break;
            }
        }
    });
    tokio::select! {
        _ = output_task => {},
        _ = input_task => {},
    }
    Ok(())
}

async fn watch_events(
    client: &mut PodServiceClient<Channel>,
    pod_id: Option<String>,
) -> Result<()> {
    let mut events = client
        .stream_pod_events(StreamPodEventsRequest {
            pod_id: pod_id.unwrap_or_default(),
        })
        .await?
        .into_inner();
    while let Some(event) = events.next().await {
        let event = event?;
        let what = if event.deleted {
            "DELETED".to_string()
        } else {
            state_name(event.state()).to_string()
        };
        if event.reason.is_empty() {
            println!("{} {what}", event.pod_id);
        } else {
            println!("{} {what}: {}", event.pod_id, event.reason);
        }
    }
    Ok(())
}

fn state_name(state: PodState) -> &'static str {
    match state {
        PodState::Unspecified => "UNKNOWN",
        PodState::Pending => "PENDING",
        PodState::Running => "RUNNING",
        PodState::Stopped => "STOPPED",
        PodState::Suspended => "SUSPENDED",
        PodState::Failed => "FAILED",
        PodState::Hibernated => "HIBERNATED",
    }
}

fn container_state_name(state: ContainerState) -> &'static str {
    match state {
        ContainerState::Unspecified => "unknown",
        ContainerState::Creating => "creating",
        ContainerState::Created => "created",
        ContainerState::Running => "running",
        ContainerState::Exited => "exited",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use clap::Parser;

    #[derive(Parser)]
    struct Cli {
        #[command(subcommand)]
        command: PodCommand,
    }

    #[test]
    fn a_create_takes_its_command_after_the_options() {
        let cli = Cli::try_parse_from([
            "pod", "create", "--image", "busybox", "--env", "A=b", "--", "sleep", "infinity",
        ])
        .unwrap();
        let PodCommand::Create(opts) = cli.command else {
            panic!("not a create");
        };
        let request = create_pod_request(*opts);
        let spec = request.spec.unwrap();
        assert_eq!(spec.containers[0].command, ["sleep", "infinity"]);
        assert_eq!(spec.containers[0].env["A"], "b");
        assert_eq!(spec.network, None, "a pod has a network by default");
        assert!(feos_utils::pod_id::is_short(&request.pod_id));
    }

    #[test]
    fn a_create_gives_the_pod_metadata_with_a_network() {
        let cli = Cli::try_parse_from([
            "pod",
            "create",
            "--metadata",
            "role=web",
            "--metadata",
            "url=http://a/?b=c",
        ])
        .unwrap();
        let PodCommand::Create(opts) = cli.command else {
            panic!("not a create");
        };
        let spec = create_pod_request(*opts).spec.unwrap();
        assert_eq!(spec.metadata["role"], "web");
        assert_eq!(spec.metadata["url"], "http://a/?b=c");
        assert!(
            Cli::try_parse_from(["pod", "create", "--no-network", "--metadata", "a=b"]).is_err()
        );
    }

    #[test]
    fn a_create_names_volumes_and_mounts_them() {
        let id = "aaaaaaaa-0000-0000-0000-000000000001";
        let cli = Cli::try_parse_from([
            "pod",
            "create",
            "--image",
            "busybox",
            "--volume",
            &format!("db={id}"),
            "--mount",
            "db:/var/lib/db",
            "--mount",
            "db:/ro:ro",
        ])
        .unwrap();
        let PodCommand::Create(opts) = cli.command else {
            panic!("not a create");
        };
        let spec = create_pod_request(*opts).spec.unwrap();
        assert_eq!(
            spec.volumes,
            [PodVolume {
                name: "db".to_string(),
                volume_id: id.to_string()
            }]
        );
        let mounts: Vec<_> = spec.containers[0]
            .volume_mounts
            .iter()
            .map(|m| (m.volume.as_str(), m.path.as_str(), m.read_only))
            .collect();
        assert_eq!(mounts, [("db", "/var/lib/db", false), ("db", "/ro", true)]);
        assert!(parse_mount("db").is_err());
        assert!(parse_volume("db").is_err());
    }

    #[test]
    fn a_restore_names_a_head_or_takes_the_pods_own() {
        let head = "snapshots/11111111-2222-3333-4444-555555555555/7/head.json";
        let cli = Cli::try_parse_from(["pod", "restore", "p", "--head", head, "--cold"]).unwrap();
        let PodCommand::Restore {
            pod_id,
            head: Some(given),
            cold: true,
            wait: false,
        } = cli.command
        else {
            panic!("not a cold restore from a head");
        };
        assert_eq!((pod_id.as_str(), given.as_str()), ("p", head));
        let cli = Cli::try_parse_from(["pod", "restore", "p"]).unwrap();
        assert!(matches!(
            cli.command,
            PodCommand::Restore {
                head: None,
                cold: false,
                ..
            }
        ));
        assert!(Cli::try_parse_from(["pod", "hibernate", "p"]).is_ok());
    }

    #[test]
    fn a_pod_without_an_image_has_no_container() {
        let cli = Cli::try_parse_from(["pod", "create", "--memory", "256"]).unwrap();
        let PodCommand::Create(opts) = cli.command else {
            panic!("not a create");
        };
        assert!(create_pod_request(*opts)
            .spec
            .unwrap()
            .containers
            .is_empty());
        assert!(Cli::try_parse_from(["pod", "create", "--", "sleep"]).is_err());
    }

    #[test]
    fn a_pod_names_its_groups_and_resolvers() {
        let cli = Cli::try_parse_from([
            "pod",
            "create",
            "--security-group",
            "web",
            "--security-group",
            "db",
            "--dns-upstream",
            "2620:fe::fe",
            "--name-table",
            "env-1",
        ])
        .unwrap();
        let PodCommand::Create(opts) = cli.command else {
            panic!("not a create");
        };
        let network = create_pod_request(*opts).spec.unwrap().network.unwrap();
        assert_eq!(network.security_groups, ["web", "db"]);
        assert_eq!(network.dns_upstreams, ["2620:fe::fe"]);
        assert_eq!(network.name_tables, ["env-1"]);
        assert!(!network.none);
        assert!(
            Cli::try_parse_from(["pod", "create", "--no-network", "--security-group", "web"])
                .is_err()
        );
    }

    #[test]
    fn a_pod_may_have_no_network() {
        let cli = Cli::try_parse_from(["pod", "create", "--no-network"]).unwrap();
        let PodCommand::Create(opts) = cli.command else {
            panic!("not a create");
        };
        let spec = create_pod_request(*opts).spec.unwrap();
        assert_eq!(
            spec.network,
            Some(PodNetwork {
                none: true,
                ..Default::default()
            })
        );
    }
}
