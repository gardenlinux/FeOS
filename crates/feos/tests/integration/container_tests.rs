// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

use super::{
    ensure_server, get_image_service_client, get_public_clients, TEST_CONTAINER_IMAGE_REF,
};
use anyhow::Result;
use feos_proto::container_service::{
    exec_stream_frame, ContainerConfig, ContainerState, CreateContainerRequest,
    DeleteContainerRequest, ExecRequest, GetContainerRequest, StartContainerRequest,
    StopContainerRequest,
};
use feos_proto::image_service::{DeleteImageRequest, PullImageRequest};
use log::info;
use std::time::Duration;
use tokio::time::timeout;

async fn wait_for_container_state(
    client: &mut feos_proto::container_service::container_service_client::ContainerServiceClient<
        tonic::transport::Channel,
    >,
    container_id: &str,
    target_state: ContainerState,
) -> Result<()> {
    info!("Waiting for container {container_id} to reach state {target_state:?}");
    for _ in 0..180 {
        let response = client
            .get_container(GetContainerRequest {
                container_id: container_id.to_string(),
            })
            .await?
            .into_inner();

        let current_state = ContainerState::try_from(response.state).unwrap_or_default();
        info!("Container {container_id} is in state {current_state:?}");

        if current_state == target_state {
            return Ok(());
        }

        tokio::time::sleep(Duration::from_secs(1)).await;
    }
    anyhow::bail!("Timeout waiting for container {container_id} to reach state {target_state:?}");
}

#[tokio::test]
async fn test_create_and_start_container() -> Result<()> {
    ensure_server().await;
    let (_, _, mut container_client) = get_public_clients().await?;

    let image_ref = TEST_CONTAINER_IMAGE_REF.clone();
    // Something that keeps running, to exec in.
    let container_config = ContainerConfig {
        image_ref: image_ref.clone(),
        command: vec!["sleep".to_string(), "300".to_string()],
        ..Default::default()
    };

    let create_req = CreateContainerRequest {
        config: Some(container_config),
        container_id: None,
        adopt: false,
        pull_token: String::new(),
    };

    info!("Sending CreateContainer request");
    let create_res = timeout(
        Duration::from_secs(180),
        container_client.create_container(create_req),
    )
    .await
    .expect("Timed out creating the container")?
    .into_inner();
    let container_id = create_res.container_id;
    info!("Container created with ID: {container_id}");
    wait_for_container_state(
        &mut container_client,
        &container_id,
        ContainerState::Created,
    )
    .await?;

    let start_req = StartContainerRequest {
        container_id: container_id.clone(),
    };
    info!("Sending StartContainer request for ID: {container_id}");
    container_client.start_container(start_req).await?;

    timeout(
        Duration::from_secs(30),
        wait_for_container_state(
            &mut container_client,
            &container_id,
            ContainerState::Running,
        ),
    )
    .await
    .expect("Timed out waiting for container to become running")?;
    info!("Container is in RUNNING state");

    let mut frames = container_client
        .exec(ExecRequest {
            container_id: container_id.clone(),
            command: vec!["echo".to_string(), "hello from exec".to_string()],
            ..Default::default()
        })
        .await?
        .into_inner();
    let (mut stdout, mut exit_code) = (Vec::new(), None);
    while let Some(frame) = frames.message().await? {
        match frame.frame {
            Some(exec_stream_frame::Frame::Stdout(bytes)) => stdout.extend(bytes),
            Some(exec_stream_frame::Frame::ExitCode(code)) => exit_code = Some(code),
            _ => {}
        }
    }
    assert_eq!(String::from_utf8_lossy(&stdout), "hello from exec\n");
    assert_eq!(exit_code, Some(0));
    info!("Exec in the container worked");

    // The container runs on the shared image of its reference, which a
    // shared pull hands out again. Its root is an overlay on the image, so
    // the image stays while the container is there.
    let mut image_client = get_image_service_client().await?;
    let image_uuid = image_client
        .pull_image(PullImageRequest {
            image_ref,
            exclusive: false,
            pull_token: String::new(),
        })
        .await?
        .into_inner()
        .image_uuid;
    let delete_image = DeleteImageRequest {
        image_uuid: image_uuid.clone(),
    };
    let status = image_client
        .delete_image(delete_image.clone())
        .await
        .expect_err("An image a container runs on cannot be deleted");
    assert_eq!(status.code(), tonic::Code::FailedPrecondition);
    info!("The image is in use: {}", status.message());

    let stop_req = StopContainerRequest {
        container_id: container_id.clone(),
        signal: None,
        timeout_seconds: None,
    };
    info!("Sending StopContainer request for ID: {container_id}");
    container_client.stop_container(stop_req).await?;

    timeout(
        Duration::from_secs(30),
        wait_for_container_state(
            &mut container_client,
            &container_id,
            ContainerState::Stopped,
        ),
    )
    .await
    .expect("Timed out waiting for container to become stopped")?;
    info!("Container is in STOPPED state");

    let delete_req = DeleteContainerRequest {
        container_id: container_id.clone(),
    };
    info!("Sending DeleteContainer request for ID: {container_id}");
    container_client.delete_container(delete_req).await?;
    info!("DeleteContainer call successful");

    let get_req = GetContainerRequest {
        container_id: container_id.clone(),
    };
    let result = container_client.get_container(get_req).await;
    assert!(result.is_err(), "GetContainer should fail after deletion");
    info!("Verified that container {container_id} is no longer found.");

    image_client.delete_image(delete_image).await?;
    info!("Deleted the image {image_uuid} once the container was gone");

    Ok(())
}
