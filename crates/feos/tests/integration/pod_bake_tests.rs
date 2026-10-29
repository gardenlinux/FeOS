// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Baked images (ADR 0025): the first pod of an image bakes it after its
//! guest pulled it and before its container, and a pod whose create names
//! that layer starts its disk on it, so that its guest finds the image in
//! its store and pulls nothing. Baking needs diskd with an object store,
//! as `make test-pods` gives it when it has one; without, the test skips.
//! The pods pull their image from the internet, as they have a network.

use super::fixtures::{verify_microvm_cleanup, PodGuard};
use super::pod_disk_tests::{pid, wait_until_up};
use super::pod_tests::{get_pod, new_pod_id};
use super::{
    ensure_server, get_public_clients, installed_guest, OBJECT_STORE, PUBLIC_SERVER_ADDRESS,
    TEST_CONTAINER_IMAGE_REF,
};
use anyhow::{bail, Context, Result};
use feos_proto::container_service::{
    container_service_client::ContainerServiceClient, exec_stream_frame, ExecRequest,
};
use feos_proto::image_service::{image_service_client::ImageServiceClient, ListImagesRequest};
use feos_proto::pod_service::{
    pod_service_client::PodServiceClient, BakedLayer, ContainerSpec, CreatePodRequest,
    DeletePodRequest, ImageLayers, PodInfo, PodSpec, Resources,
};
use log::info;
use std::time::Duration;
use tokio::time::{sleep, Instant};
use tonic::transport::Channel;

/// How long a bake's upload may take after its pod came up.
const BAKED_TIMEOUT: Duration = Duration::from_secs(300);

fn spec() -> PodSpec {
    PodSpec {
        resources: Some(Resources {
            vcpus: 1,
            memory_mib: 256,
        }),
        containers: vec![ContainerSpec {
            name: "main".to_string(),
            image_ref: TEST_CONTAINER_IMAGE_REF.clone(),
            command: vec!["sleep".to_string(), "3600".to_string()],
            ..Default::default()
        }],
        ..Default::default()
    }
}

/// Creates a pod of the test image whose create names `layers` and lets
/// it bake, and waits until it runs.
async fn create(
    pods: &mut PodServiceClient<Channel>,
    layers: Vec<BakedLayer>,
) -> Result<(String, PodGuard)> {
    let pod_id = new_pod_id();
    pods.create_pod(CreatePodRequest {
        pod_id: pod_id.clone(),
        spec: Some(spec()),
        image_layers: Some(ImageLayers {
            image_ref: TEST_CONTAINER_IMAGE_REF.clone(),
            layers,
            bake: true,
        }),
    })
    .await?;
    let mut guard = PodGuard::new(pod_id.clone());
    let pod = wait_until_up(pods, &pod_id).await?;
    guard.set_pid(pid(&pod)?);
    Ok((pod_id, guard))
}

/// The pod's record once it names its bake.
async fn wait_until_baked(pods: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<PodInfo> {
    let deadline = Instant::now() + BAKED_TIMEOUT;
    loop {
        let pod = get_pod(pods, pod_id).await?;
        if pod.baked.is_some() {
            return Ok(pod);
        }
        if Instant::now() > deadline {
            bail!("pod {pod_id} names no bake after {BAKED_TIMEOUT:?}: {pod:?}");
        }
        sleep(Duration::from_millis(500)).await;
    }
}

/// The images in the pod's guest, by ID and reference.
async fn images(pod_id: &str) -> Result<Vec<(String, String)>> {
    let mut images = ImageServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    let mut request = tonic::Request::new(ListImagesRequest {});
    request
        .metadata_mut()
        .insert("feos-pod-id", pod_id.parse().expect("a pod ID is ASCII"));
    let mut listed: Vec<(String, String)> = images
        .list_images(request)
        .await?
        .into_inner()
        .images
        .into_iter()
        .map(|image| (image.image_uuid, image.image_ref))
        .collect();
    listed.sort();
    Ok(listed)
}

/// Runs `script` with sh in the pod's container: its exit code and output.
async fn sh(pod_id: &str, script: &str) -> Result<(i32, String)> {
    let mut containers = ContainerServiceClient::connect(PUBLIC_SERVER_ADDRESS).await?;
    let mut request = tonic::Request::new(ExecRequest {
        container_id: "main".to_string(),
        command: vec!["sh".to_string(), "-c".to_string(), script.to_string()],
        ..Default::default()
    });
    request
        .metadata_mut()
        .insert("feos-pod-id", pod_id.parse().expect("a pod ID is ASCII"));
    let mut frames = containers.exec(request).await?.into_inner();
    let (mut output, mut code) = (Vec::new(), None);
    while let Some(frame) = frames.message().await? {
        match frame.frame {
            Some(exec_stream_frame::Frame::Stdout(bytes))
            | Some(exec_stream_frame::Frame::Stderr(bytes)) => output.extend(bytes),
            Some(exec_stream_frame::Frame::ExitCode(exit)) => code = Some(exit),
            None => {}
        }
    }
    Ok((
        code.context("the exec ended without an exit code")?,
        String::from_utf8_lossy(&output).into_owned(),
    ))
}

async fn delete(pods: &mut PodServiceClient<Channel>, pod_id: &str) -> Result<()> {
    pods.delete_pod(DeletePodRequest {
        pod_id: pod_id.to_string(),
    })
    .await?;
    verify_microvm_cleanup(pod_id);
    Ok(())
}

#[tokio::test]
async fn test_a_pod_bakes_its_image_and_the_next_starts_on_it() -> Result<()> {
    if installed_guest()?.is_none() {
        return Ok(());
    }
    if std::env::var("S3_BUCKET").map_or(true, |bucket| bucket.is_empty()) {
        log::warn!("Skipping test: no object store (S3_BUCKET) to bake into.");
        return Ok(());
    }
    let _store = OBJECT_STORE.read().await;
    ensure_server().await;
    let (mut pods, _, _) = get_public_clients().await?;
    let image = TEST_CONTAINER_IMAGE_REF.clone();

    // The first pod pulls and bakes; its container runs on a thawed
    // filesystem, and its record names the layer once it is uploaded.
    let (first, mut first_guard) = create(&mut pods, vec![]).await?;
    let (code, output) = sh(&first, "echo written > /root/file && cat /root/file").await?;
    assert_eq!(
        (code, output.as_str()),
        (0, "written\n"),
        "the first pod's container"
    );
    let info = wait_until_baked(&mut pods, &first).await?;
    let disk = info.disk.context("the first pod's disk")?;
    let baked = info.baked.context("the first pod's bake")?;
    info!("The first pod {first} baked {image} into {baked:?}");
    assert_eq!(disk.image_ref, image);
    assert!(
        disk.layer_id.is_empty(),
        "the first disk is on the blank data volume"
    );
    assert!(
        !disk.guest_version.is_empty() && !disk.l0.is_empty(),
        "{disk:?}"
    );
    assert_eq!(
        (&baked.image_ref, &baked.guest_version, &baked.l0),
        (&disk.image_ref, &disk.guest_version, &disk.l0)
    );
    assert!(!baked.layer_id.is_empty());
    let first_images = images(&first).await?;
    assert_eq!(first_images.len(), 1, "{first_images:?}");

    // The next pod starts on the layer: its guest has the first pod's
    // image, by the first pod's ID, and bakes nothing. The first pod's
    // container's file is not there: it was written after the bake.
    let (second, mut second_guard) = create(&mut pods, vec![baked.clone()]).await?;
    let info = get_pod(&mut pods, &second).await?;
    assert_eq!(
        info.disk.as_ref().map(|disk| disk.layer_id.as_str()),
        Some(baked.layer_id.as_str())
    );
    assert_eq!(info.baked, None);
    assert_eq!(
        images(&second).await?,
        first_images,
        "the image from the layer"
    );
    let (code, output) = sh(&second, "test ! -e /root/file && echo clean").await?;
    assert_eq!(
        (code, output.as_str()),
        (0, "clean\n"),
        "the second pod's container"
    );

    // A layer the object store does not have is passed over for the next
    // one that is there.
    let missing = BakedLayer {
        layer_id: "0".repeat(64),
        ..baked.clone()
    };
    let (third, mut third_guard) = create(&mut pods, vec![missing, baked.clone()]).await?;
    let info = get_pod(&mut pods, &third).await?;
    assert_eq!(
        info.disk.as_ref().map(|disk| disk.layer_id.as_str()),
        Some(baked.layer_id.as_str()),
        "the next layer that is there"
    );

    for (pod_id, guard) in [
        (&first, &mut first_guard),
        (&second, &mut second_guard),
        (&third, &mut third_guard),
    ] {
        delete(&mut pods, pod_id).await?;
        guard.disable_cleanup();
    }
    Ok(())
}
