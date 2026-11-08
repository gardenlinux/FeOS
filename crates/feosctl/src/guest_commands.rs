// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::forward_commands::GuestAddress;
use anyhow::{bail, Result};
use clap::{Args, Subcommand};
use feos_proto::guest_service::{
    guest_service_client::GuestServiceClient, MountVolumesRequest, SetFeidRequest,
    SetInterceptCasRequest, TlsBinding, Volume,
};

/// What feosd calls in a pod's guest, for development and the guest's
/// tests
#[derive(Args, Debug)]
pub struct GuestArgs {
    #[command(flatten)]
    pub guest: GuestAddress,

    #[command(subcommand)]
    command: GuestCommand,
}

#[derive(Subcommand, Debug)]
pub enum GuestCommand {
    /// Mount the pod's volumes in the guest, as feosd does before it
    /// creates the containers
    MountVolumes {
        #[arg(
            long = "volume",
            required = true,
            help = "NAME:VOLUME_ID:DRIVE:SIZE_BYTES, with :format for a new volume",
            value_parser = parse_volume
        )]
        volumes: Vec<Volume>,
    },
    /// Set the pod's interception CAs in the guest (ADR 0028), as feosd
    /// does: none empties them
    SetInterceptCas {
        #[arg(
            long = "ca-file",
            value_name = "PATH",
            help = "A PEM file of a CA, oldest first (repeatable)"
        )]
        ca_files: Vec<std::path::PathBuf>,
    },
    /// Set FeID's socket and the pod's outbound TLS bindings in the guest
    /// (ADR 0036), as feosd does for a pod with an identity: without
    /// --disable, the socket, and the bindings given, none without
    SetFeid {
        #[arg(long, help = "Remove the socket, and so the bindings")]
        disable: bool,
        #[arg(long, help = "End every connection first, as after a resume")]
        reset: bool,
        #[arg(
            long = "tls-binding",
            value_name = "LISTEN,HOST,PORT,ALPN,SERVER_ID",
            help = "A binding, e.g. 127.64.0.1:5432,postgres,5432,postgresql,spiffe://… (repeatable)",
            value_parser = parse_tls_binding
        )]
        tls_bindings: Vec<TlsBinding>,
    },
}

fn parse_tls_binding(s: &str) -> Result<TlsBinding, String> {
    let &[listen, host, port, alpn, server_id] = &s.split(',').collect::<Vec<_>>()[..] else {
        return Err(format!(
            "invalid TLS binding {s}: LISTEN,HOST,PORT,ALPN,SERVER_ID"
        ));
    };
    Ok(TlsBinding {
        listen: listen.to_string(),
        host: host.to_string(),
        port: port
            .parse()
            .map_err(|e| format!("invalid port {port}: {e}"))?,
        alpn: alpn.to_string(),
        server_id: server_id.to_string(),
    })
}

fn parse_volume(s: &str) -> Result<Volume, String> {
    let fields: Vec<&str> = s.split(':').collect();
    let format = match fields.get(4) {
        None => false,
        Some(&"format") if fields.len() == 5 => true,
        Some(_) => return Err(format!("invalid volume {s}: unexpected {:?}", &fields[4..])),
    };
    let &[name, volume_id, drive, size] = &fields[..fields.len().min(4)] else {
        return Err(format!(
            "invalid volume {s}: NAME:VOLUME_ID:DRIVE:SIZE_BYTES[:format]"
        ));
    };
    Ok(Volume {
        name: name.to_string(),
        volume_id: volume_id.to_string(),
        drive: drive
            .parse()
            .map_err(|e| format!("invalid drive {drive}: {e}"))?,
        size_bytes: size
            .parse()
            .map_err(|e| format!("invalid size {size}: {e}"))?,
        format,
    })
}

pub async fn handle_guest_command(args: GuestArgs) -> Result<()> {
    let mut client = GuestServiceClient::new(crate::transport::channel(&args.guest.address).await?);
    match args.command {
        GuestCommand::MountVolumes { volumes } => {
            let mounted = client
                .mount_volumes(MountVolumesRequest { volumes })
                .await?
                .into_inner()
                .volumes;
            let mut failed = false;
            for volume in &mounted {
                if volume.error.is_empty() {
                    let formatted = if volume.formatted { ", formatted" } else { "" };
                    println!("{}: mounted from {}{formatted}", volume.name, volume.device);
                } else {
                    eprintln!("{}: {}", volume.name, volume.error);
                    failed = true;
                }
            }
            if failed {
                bail!("some volumes are not mounted");
            }
        }
        GuestCommand::SetInterceptCas { ca_files } => {
            let ca_pems = ca_files
                .iter()
                .map(std::fs::read_to_string)
                .collect::<Result<Vec<_>, _>>()?;
            let count = ca_pems.len();
            let failed = client
                .set_intercept_cas(SetInterceptCasRequest { ca_pems })
                .await?
                .into_inner()
                .failed;
            for failure in &failed {
                eprintln!("{}: {}", failure.container_id, failure.reason);
            }
            if !failed.is_empty() {
                bail!("some containers keep the CAs they had");
            }
            println!("The guest has {count} interception CAs");
        }
        GuestCommand::SetFeid {
            disable,
            reset,
            tls_bindings,
        } => {
            let answer = client
                .set_feid(SetFeidRequest {
                    enabled: !disable,
                    reset_connections: reset,
                    tls_bindings,
                })
                .await?
                .into_inner();
            let socket = if answer.listening {
                "listening"
            } else {
                "none"
            };
            println!(
                "FeID's socket: {socket}; TLS bindings: {}",
                answer.tls_bindings
            );
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn volumes_parse() {
        let v = parse_volume("pgdata:3b0c6c4e-8f1e-4a55-9a43-2f0e0d9b6a11:1:1073741824:format")
            .unwrap();
        assert_eq!(v.name, "pgdata");
        assert_eq!(v.drive, 1);
        assert_eq!(v.size_bytes, 1 << 30);
        assert!(v.format);
        assert!(!parse_volume("a:b:2:3").unwrap().format);
        for bad in ["a:b:c", "a:b:x:3", "a:b:1:3:other", "a:b:1:3:format:x"] {
            assert!(parse_volume(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn tls_bindings_parse() {
        let b = parse_tls_binding("127.64.0.1:5432,fd00::1,15432,postgresql,spiffe://t/x").unwrap();
        assert_eq!(b.listen, "127.64.0.1:5432");
        assert_eq!(b.host, "fd00::1");
        assert_eq!(b.port, 15432);
        assert_eq!(b.alpn, "postgresql");
        assert_eq!(b.server_id, "spiffe://t/x");
        for bad in ["a,b,1,c", "a,b,x,c,d", "a,b,1,c,d,e"] {
            assert!(parse_tls_binding(bad).is_err(), "{bad}");
        }
    }
}
