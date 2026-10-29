// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"strings"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Baked images (ADR 0025): the first pod of an image on a node with an
// object store pulls it, and its node seals the guest's disk as a layer
// before any container ran and reports it in the pod's record once the
// store has it; the engine records it in image_bakes. A later create of
// the same image, once the image's registry said yes to it, names the
// layers, and its node starts the pod's disk on the newest one made for
// the guest it boots. The image is the key, registry, repository and
// manifest digest, never a digest alone.

// maxBakesSeen bounds the bakes this instance remembers having recorded.
const maxBakesSeen = 4096

// imageLayers is what a create of image tells its node (--image-bakes):
// the baked layers the catalog has, and that the node may bake the image
// when none fits. nil without --image-bakes. A catalog that cannot be read
// names no layer: the node bakes again.
func (e *Engine) imageLayers(ctx context.Context, image string) *podv1.ImageLayers {
	if !e.cfg.ImageBakes || !strings.Contains(image, "@sha256:") {
		return nil
	}
	layers := &podv1.ImageLayers{ImageRef: image, Bake: true}
	bakes, err := e.st.ImageBakes(ctx, image)
	if err != nil {
		e.rl.warn("image-bakes", "cannot read the bakes of an image; the create names none", "image", image, "err", err)
	}
	for _, b := range bakes {
		layers.Layers = append(layers.Layers, &podv1.BakedLayer{ImageRef: b.Image, GuestVersion: b.GuestVersion,
			L0: b.L0, LayerId: b.Layer})
	}
	return layers
}

// recordBake records in image_bakes the bake a pod on node reports, once
// per instance and layer: a layer of the image its one container runs.
// Every observation of a pod passes here (getPod), the sync after the
// pod's event among them.
func (e *Engine) recordBake(ctx context.Context, node string, info *podv1.PodInfo) {
	b := info.GetBaked()
	if !e.cfg.ImageBakes || b.GetLayerId() == "" {
		return
	}
	containers := info.GetSpec().GetContainers()
	if len(containers) != 1 || containers[0].GetImageRef() != b.GetImageRef() {
		return
	}
	key := b.GetImageRef() + "\x00" + b.GetGuestVersion() + "\x00" + b.GetL0() + "\x00" + b.GetLayerId()
	e.mu.Lock()
	seen := e.bakes[key]
	e.mu.Unlock()
	if seen {
		return
	}
	added, err := e.st.RecordImageBake(ctx, store.ImageBake{Image: b.GetImageRef(), GuestVersion: b.GetGuestVersion(),
		L0: b.GetL0(), Layer: b.GetLayerId(), Node: node})
	if err != nil {
		e.rl.warn("image-bakes", "cannot record a bake", "image", b.GetImageRef(), "node", node, "err", err)
		return
	}
	if added {
		e.log.Info("image baked", "image", b.GetImageRef(), "layer", b.GetLayerId(), "guest", b.GetGuestVersion(),
			"node", node, "pod", info.GetPodId())
	}
	e.mu.Lock()
	if len(e.bakes) >= maxBakesSeen {
		clear(e.bakes)
	}
	e.bakes[key] = true
	e.mu.Unlock()
}
