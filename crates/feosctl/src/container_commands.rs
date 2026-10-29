// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::transport::Conn;
use anyhow::Result;
use clap::{Args, Subcommand};
use feos_proto::container_service::{
    container_service_client::ContainerServiceClient, exec_stream_frame, ContainerConfig,
    ContainerState, CreateContainerRequest, DeleteContainerRequest, ExecRequest,
    GetContainerRequest, ListContainersRequest, StartContainerRequest, StopContainerRequest,
    StreamContainerLogsRequest, VolumeMount,
};

#[derive(Args, Debug)]
pub struct ContainerArgs {
    #[arg(
        short,
        long,
        global = true,
        env = "FEOS_ADDRESS",
        default_value = "unix:/run/feos/api.sock"
    )]
    pub address: String,

    /// A pod on the node at the address: the calls go to its guest.
    #[arg(long, global = true, env = "FEOS_POD")]
    pub pod: Option<String>,

    #[command(subcommand)]
    command: ContainerCommand,
}

#[derive(Subcommand, Debug)]
pub enum ContainerCommand {
    /// Create a new container
    Create {
        #[arg(
            long,
            required_unless_present = "adopt",
            default_value = "",
            help = "Container image reference (e.g., docker.io/library/alpine:latest)"
        )]
        image_ref: String,

        #[arg(
            long,
            help = "Optional container identifier (letters, digits, -, _, .)"
        )]
        id: Option<String>,

        #[arg(long, help = "Adopt the container of this ID from before a reboot")]
        adopt: bool,

        #[arg(
            long,
            help = "Override the default command of the image",
            num_args = 1..,
            value_delimiter = ' '
        )]
        cmd: Vec<String>,

        #[arg(
            long,
            help = "Set environment variables (e.g., --env KEY1=VALUE1 --env KEY2=VALUE2)",
            value_parser = parse_key_val
        )]
        env: Vec<(String, String)>,

        #[arg(
            long = "volume",
            help = "Mount a volume the guest mounted: NAME:PATH, or NAME:PATH:ro",
            value_parser = parse_volume_mount
        )]
        volumes: Vec<VolumeMount>,

        #[arg(
            last = true,
            conflicts_with = "cmd",
            help = "The command and its arguments as they are, after -- (in place of --cmd)"
        )]
        args: Vec<String>,
    },
    /// Start a created container
    Start {
        #[arg(required = true, help = "Container identifier")]
        id: String,
    },
    /// Stop a running container
    Stop {
        #[arg(required = true, help = "Container identifier")]
        id: String,
    },
    /// Get detailed information about a container
    Info {
        #[arg(required = true, help = "Container identifier")]
        id: String,
    },
    /// List all containers
    List,
    /// Print a container's log: stdout and stderr, as the container wrote them
    Logs {
        #[arg(required = true, help = "Container identifier")]
        id: String,

        #[arg(
            long,
            short = 'n',
            help = "Only the last lines (at most 10000 and 4 MiB); 0: none of the log so far"
        )]
        tail: Option<u32>,

        #[arg(
            long,
            short = 'f',
            help = "Go on printing what it writes, until it is deleted"
        )]
        follow: bool,
    },
    /// Delete a container; a running one is killed first
    Delete {
        #[arg(required = true, help = "Container identifier")]
        id: String,
    },
    /// Run a command in a running container
    Exec {
        #[arg(required = true, help = "Container identifier")]
        id: String,

        #[arg(long, short = 'w', help = "Working directory in the container")]
        workdir: Option<String>,

        #[arg(
            long,
            help = "Set environment variables (e.g., --env KEY1=VALUE1)",
            value_parser = parse_key_val
        )]
        env: Vec<(String, String)>,

        #[arg(required = true, last = true, help = "Command and arguments, after --")]
        command: Vec<String>,
    },
}

fn parse_volume_mount(s: &str) -> Result<VolumeMount, String> {
    let (volume, rest) = s
        .split_once(':')
        .ok_or_else(|| format!("invalid volume mount {s}: NAME:PATH[:ro]"))?;
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

fn parse_key_val(s: &str) -> Result<(String, String), String> {
    s.split_once('=')
        .map(|(key, value)| (key.to_string(), value.to_string()))
        .ok_or_else(|| format!("invalid KEY=value format: {s}"))
}

pub async fn handle_container_command(args: ContainerArgs) -> Result<()> {
    let mut client = ContainerServiceClient::new(
        crate::transport::connect(&args.address, args.pod.as_deref()).await?,
    );

    match args.command {
        ContainerCommand::Create {
            image_ref,
            id,
            adopt,
            cmd,
            env,
            volumes,
            args,
        } => {
            let command = if args.is_empty() { cmd } else { args };
            create_container(&mut client, image_ref, id, adopt, command, env, volumes).await?
        }
        ContainerCommand::Start { id } => start_container(&mut client, id).await?,
        ContainerCommand::Stop { id } => stop_container(&mut client, id).await?,
        ContainerCommand::Info { id } => get_container_info(&mut client, id).await?,
        ContainerCommand::List => list_containers(&mut client).await?,
        ContainerCommand::Logs { id, tail, follow } => {
            let request = StreamContainerLogsRequest {
                container_id: id,
                follow,
                tail: tail.is_none(),
                tail_lines: tail.unwrap_or(0),
            };
            print_logs(&mut client, request).await?
        }
        ContainerCommand::Delete { id } => delete_container(&mut client, id).await?,
        ContainerCommand::Exec {
            id,
            workdir,
            env,
            command,
        } => {
            let request = ExecRequest {
                container_id: id,
                command,
                env: env.into_iter().collect(),
                working_dir: workdir.unwrap_or_default(),
            };
            let code = exec(&mut client, request).await?;
            std::process::exit(code);
        }
    }

    Ok(())
}

async fn create_container(
    client: &mut ContainerServiceClient<Conn>,
    image_ref: String,
    id: Option<String>,
    adopt: bool,
    cmd: Vec<String>,
    env: Vec<(String, String)>,
    volume_mounts: Vec<VolumeMount>,
) -> Result<()> {
    println!("Requesting container creation with image: {image_ref}...");

    let config = ContainerConfig {
        image_ref,
        command: cmd,
        env: env.into_iter().collect(),
        volume_mounts,
    };

    let request = CreateContainerRequest {
        config: Some(config),
        container_id: id,
        adopt,
        pull_token: String::new(),
    };

    let response = client.create_container(request).await?.into_inner();
    println!("Container created. Container ID: {}", response.container_id);
    if !response.resolver_error.is_empty() {
        println!("Resolvers not linked: {}", response.resolver_error);
    }
    println!(
        "Use 'feosctl container start {}' to run it.",
        response.container_id
    );

    Ok(())
}

async fn start_container(client: &mut ContainerServiceClient<Conn>, id: String) -> Result<()> {
    println!("Requesting to start container: {id}...");
    let request = StartContainerRequest {
        container_id: id.clone(),
    };
    client.start_container(request).await?;
    println!("Start request sent for container: {id}");
    Ok(())
}

async fn stop_container(client: &mut ContainerServiceClient<Conn>, id: String) -> Result<()> {
    println!("Requesting to stop container: {id}...");
    let request = StopContainerRequest {
        container_id: id.clone(),
        ..Default::default()
    };
    client.stop_container(request).await?;
    println!("Stop request sent for container: {id}");
    Ok(())
}

async fn get_container_info(client: &mut ContainerServiceClient<Conn>, id: String) -> Result<()> {
    let request = GetContainerRequest {
        container_id: id.clone(),
    };
    let response = client.get_container(request).await?.into_inner();

    println!("Container Info for: {id}");
    println!(
        "  State: {:?}",
        ContainerState::try_from(response.state).unwrap_or(ContainerState::Unspecified)
    );
    if let Some(pid) = response.pid {
        println!("  PID: {pid}");
    }
    if let Some(exit_code) = response.exit_code {
        println!("  Exit Code: {exit_code}");
    }
    if let Some(config) = response.config {
        println!("  Config:");
        println!("    Image Ref: {}", config.image_ref);
        if !config.command.is_empty() {
            println!("    Command: {:?}", config.command);
        }
        if !config.env.is_empty() {
            println!("    Env: {:?}", config.env);
        }
    }

    Ok(())
}

async fn list_containers(client: &mut ContainerServiceClient<Conn>) -> Result<()> {
    let request = ListContainersRequest {};
    let response = client.list_containers(request).await?.into_inner();

    if response.containers.is_empty() {
        println!("No containers found.");
        return Ok(());
    }

    println!("{:<38} {:<15} IMAGE_REF", "CONTAINER_ID", "STATE");
    println!("{:-<38} {:-<15} {:-<40}", "", "", "");
    for container in response.containers {
        let state =
            ContainerState::try_from(container.state).unwrap_or(ContainerState::Unspecified);
        let image_ref = container
            .config
            .map(|c| c.image_ref)
            .unwrap_or_else(|| "N/A".to_string());
        println!(
            "{:<38} {:<15} {}",
            container.container_id,
            format!("{:?}", state),
            image_ref
        );
    }
    Ok(())
}

async fn delete_container(client: &mut ContainerServiceClient<Conn>, id: String) -> Result<()> {
    println!("Requesting to delete container: {id}...");
    let request = DeleteContainerRequest {
        container_id: id.clone(),
    };
    client.delete_container(request).await?;
    println!("Successfully deleted container: {id}");
    Ok(())
}

/// Prints the log's lines as they come.
async fn print_logs(
    client: &mut ContainerServiceClient<Conn>,
    request: StreamContainerLogsRequest,
) -> Result<()> {
    use std::io::Write;
    let mut entries = client.stream_container_logs(request).await?.into_inner();
    let mut stdout = std::io::stdout();
    while let Some(entry) = entries.message().await? {
        stdout.write_all(&entry.line)?;
        stdout.write_all(b"\n")?;
        stdout.flush()?;
    }
    Ok(())
}

/// Streams the command's output to this process's stdout and stderr and
/// returns its exit code.
async fn exec(client: &mut ContainerServiceClient<Conn>, request: ExecRequest) -> Result<i32> {
    use std::io::Write;
    let mut frames = client.exec(request).await?.into_inner();
    while let Some(frame) = frames.message().await? {
        match frame.frame {
            Some(exec_stream_frame::Frame::Stdout(bytes)) => {
                std::io::stdout().write_all(&bytes)?;
                std::io::stdout().flush()?;
            }
            Some(exec_stream_frame::Frame::Stderr(bytes)) => {
                std::io::stderr().write_all(&bytes)?;
            }
            Some(exec_stream_frame::Frame::ExitCode(code)) => return Ok(code),
            None => {}
        }
    }
    anyhow::bail!("the exec stream ended without an exit code")
}
