// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Baked images (ADR 0025). A pod whose create names its one container's
//! image starts its new disk on the newest layer baked of that image for
//! the guest it boots and that guest's blank data volume, if the object
//! store has one (`Disks::open`). Otherwise, when its create lets it, the
//! pod bakes the image at its bake point: after the guest pulled it and
//! before the first container, with the guest's filesystem frozen, diskd
//! seals the disk's top as a layer, and once the object store has it the
//! pod's record names it (`PodInfo.baked`) for the control plane's
//! catalog. Nothing of the pod's own is on the disk then: no container has
//! run, and its command, environment and volumes are not there yet.
//!
//! The control plane asks the image's registry before every create, so a
//! layer is only ever named to a create that may pull its image; the image
//! is the key, registry, repository and manifest digest, never a digest
//! alone.

use feos_proto::pod_service::{BakedLayer, PodInfo};

/// The image a pod's create named, and the layers baked of it, for a pod
/// whose one container runs that image.
pub(crate) fn image_of(info: &PodInfo) -> Option<(&str, &[BakedLayer])> {
    let layers = info.image_layers.as_ref()?;
    let containers = &info.spec.as_ref()?.containers;
    if layers.image_ref.is_empty()
        || containers.len() != 1
        || containers[0].image_ref != layers.image_ref
    {
        return None;
    }
    Some((&layers.image_ref, &layers.layers))
}

/// What the pod bakes its image as, if its bake is due: its create lets
/// it, it made its disk on the blank data volume, it never ran from a
/// checkpoint, and it baked nothing yet. Whether a container is there
/// already the guest says.
pub(crate) fn due(info: &PodInfo) -> Option<BakedLayer> {
    let (image_ref, _) = image_of(info)?;
    if !info.image_layers.as_ref()?.bake || info.baked.is_some() || info.checkpoint.is_some() {
        return None;
    }
    let disk = info.disk.as_ref()?;
    if !disk.layer_id.is_empty() || disk.image_ref != image_ref {
        return None;
    }
    Some(disk.clone())
}

/// Whether a thaw's error says that the filesystem was not frozen: FITHAW
/// fails with EINVAL then.
pub(crate) fn not_frozen(message: &str) -> bool {
    message.contains("EINVAL")
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::pod_service::{Checkpoint, ContainerSpec, ImageLayers, PodSpec};

    const IMAGE: &str = "ghcr.io/acme/app@sha256:aaaa";

    fn pod(images: &[&str], bake: bool, layers: Vec<BakedLayer>) -> PodInfo {
        PodInfo {
            pod_id: "p".into(),
            spec: Some(PodSpec {
                containers: images
                    .iter()
                    .map(|image| ContainerSpec {
                        name: "main".into(),
                        image_ref: image.to_string(),
                        ..Default::default()
                    })
                    .collect(),
                ..Default::default()
            }),
            image_layers: Some(ImageLayers {
                image_ref: IMAGE.into(),
                layers,
                bake,
            }),
            ..Default::default()
        }
    }

    fn disk(layer_id: &str) -> Option<BakedLayer> {
        Some(BakedLayer {
            image_ref: IMAGE.into(),
            guest_version: "g".into(),
            l0: "l0".into(),
            layer_id: layer_id.into(),
        })
    }

    #[test]
    fn the_image_is_the_one_container_s() {
        let layer = disk("l").unwrap();
        let info = pod(&[IMAGE], false, vec![layer.clone()]);
        assert_eq!(image_of(&info), Some((IMAGE, &[layer][..])));
        assert_eq!(image_of(&pod(&["other@sha256:bbbb"], true, vec![])), None);
        assert_eq!(image_of(&pod(&[IMAGE, IMAGE], true, vec![])), None);
        assert_eq!(image_of(&pod(&[], true, vec![])), None);
        let mut none = pod(&[IMAGE], true, vec![]);
        none.image_layers = None;
        assert_eq!(image_of(&none), None);
    }

    #[test]
    fn a_bake_is_due_once_on_a_blank_disk_the_pod_made() {
        let mut info = pod(&[IMAGE], true, vec![]);
        // No disk made yet, or one of before.
        assert_eq!(due(&info), None);
        info.disk = disk("");
        assert_eq!(due(&info), disk(""));
        // Not without the create's leave, nor on a baked layer, nor twice,
        // nor after a checkpoint.
        let mut no_leave = info.clone();
        no_leave.image_layers.as_mut().unwrap().bake = false;
        assert_eq!(due(&no_leave), None);
        let mut on_layer = info.clone();
        on_layer.disk = disk("l");
        assert_eq!(due(&on_layer), None);
        let mut baked = info.clone();
        baked.baked = disk("mine");
        assert_eq!(due(&baked), None);
        let mut restored = info.clone();
        restored.checkpoint = Some(Checkpoint::default());
        assert_eq!(due(&restored), None);
    }

    #[test]
    fn a_thaw_of_a_filesystem_not_frozen_is_einval() {
        assert!(not_frozen("/var/lib/feos: EINVAL: Invalid argument"));
        assert!(!not_frozen("/var/lib/feos: EIO: Input/output error"));
    }
}
