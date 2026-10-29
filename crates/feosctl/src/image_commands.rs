// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use crate::transport::Conn;
use anyhow::Result;
use clap::{Args, Subcommand};
use feos_proto::image_service::{
    image_service_client::ImageServiceClient, DeleteImageRequest, ImageState, ListImagesRequest,
    PullImageRequest, WatchImageStatusRequest,
};
use tokio_stream::StreamExt;

#[derive(Args, Debug)]
pub struct ImageArgs {
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
    command: ImageCommand,
}

#[derive(Subcommand, Debug)]
pub enum ImageCommand {
    /// Pull a container image from a registry
    Pull {
        #[arg(
            required = true,
            help = "Container image reference to pull (e.g., docker.io/library/ubuntu:latest)"
        )]
        image_ref: String,
    },
    /// List all local container images
    List,
    /// Watch the status of an image pull operation
    Watch {
        #[arg(required = true, help = "UUID of the image to watch")]
        image_uuid: String,
    },
    /// Delete a local container image
    Delete {
        #[arg(required = true, help = "UUID of the image to delete")]
        image_uuid: String,
    },
}

pub async fn handle_image_command(args: ImageArgs) -> Result<()> {
    let mut client = ImageServiceClient::new(
        crate::transport::connect(&args.address, args.pod.as_deref()).await?,
    );

    match args.command {
        ImageCommand::Pull { image_ref } => pull_image(&mut client, image_ref).await?,
        ImageCommand::List => list_images(&mut client).await?,
        ImageCommand::Watch { image_uuid } => watch_image(&mut client, image_uuid).await?,
        ImageCommand::Delete { image_uuid } => delete_image(&mut client, image_uuid).await?,
    }

    Ok(())
}

async fn pull_image(client: &mut ImageServiceClient<Conn>, image_ref: String) -> Result<()> {
    println!("Requesting image pull for: {image_ref}...");
    let request = PullImageRequest {
        image_ref,
        exclusive: false,
        pull_token: String::new(),
    };
    let response = client.pull_image(request).await?.into_inner();
    println!("Image pull initiated. UUID: {}", response.image_uuid);
    println!(
        "Use 'feosctl image watch {}' to see progress.",
        response.image_uuid
    );
    Ok(())
}

async fn list_images(client: &mut ImageServiceClient<Conn>) -> Result<()> {
    let request = ListImagesRequest {};
    let response = client.list_images(request).await?.into_inner();
    if response.images.is_empty() {
        println!("No local images found.");
        return Ok(());
    }

    println!("{:<38} {:<12} REFERENCE", "UUID", "STATE");
    println!("{:-<38} {:-<12} {:-<40}", "", "", "");
    for image in response.images {
        let state = ImageState::try_from(image.state).unwrap_or_default();
        println!(
            "{:<38} {:<12} {}",
            image.image_uuid,
            format!("{state:?}"),
            image.image_ref
        );
    }
    Ok(())
}

async fn watch_image(client: &mut ImageServiceClient<Conn>, image_uuid: String) -> Result<()> {
    println!("Watching status for image: {image_uuid}. Press Ctrl+C to stop.");
    let request = WatchImageStatusRequest {
        image_uuid: image_uuid.clone(),
    };
    let mut stream = client.watch_image_status(request).await?.into_inner();

    while let Some(status_res) = stream.next().await {
        match status_res {
            Ok(status) => {
                let state = ImageState::try_from(status.state).unwrap_or_default();
                println!(
                    "Status: {:<12} | Progress: {:>3}% | Message: {}",
                    format!("{state:?}"),
                    status.progress_percent,
                    status.message
                );
                if matches!(state, ImageState::Ready | ImageState::PullFailed) {
                    println!("Terminal state reached. Exiting watch.");
                    break;
                }
            }
            Err(e) => {
                eprintln!("\nError in watch stream: {e}");
                break;
            }
        }
    }
    Ok(())
}

async fn delete_image(client: &mut ImageServiceClient<Conn>, image_uuid: String) -> Result<()> {
    let request = DeleteImageRequest {
        image_uuid: image_uuid.clone(),
    };
    client.delete_image(request).await?;
    println!("Successfully deleted image: {image_uuid}");
    Ok(())
}
