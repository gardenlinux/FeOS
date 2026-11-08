# ADR 0050: The node pulls images, and a bake pod unpacks them

**Status:** Proposed · **Date:** 2026-10-27

The node, not the guest, fetches a container image: `feosd` pulls its
layers with the create's registry credentials, held in memory and
written nowhere, keeps the blobs by digest in a cache of the node's, and
runs a bake pod, a microVM of the node's own with `feos-guest` and no
container, which unpacks the layers into a fresh volume that `feosd`
seals as the image's layer (ADR 0022). No tenant's code runs in a bake
pod and no credential enters it, so no credential enters any pod: the
guest's pull on the registry's token ends, and a registry that takes only
Basic authentication works like any other. The key, the catalog and the
question before every pod stay as ADR 0022 has them.

## Context

Under ADR 0022 an image's first pod pulls and bakes. The guest pulls with
the registry's Bearer token for the repository's pull, the one credential
that enters a pod, and code in a sandbox may pull every tag of the
repository while the token lives, the exposure recorded in
[security.md](../security.md). A registry that answers with a Basic
challenge issues no token, so a create with credentials for it is
refused: its password would have to enter the pod. And the bake happens
in a tenant's pod, before any container of its own has run, which keeps
the layer clean but makes the first pod of every image slower than the
rest and ties the bake to that pod's guest.

The node shells out to nothing and writes no filesystem of its own
(ADR 0001; ADR 0016's one exception is the guest's), so the node cannot
unpack an image into a filesystem itself. A pod that boots in under a
second is the node's own tool for that.

## Decision

- **The node fetches the image.** `feosd` pulls the manifest and the
  layers over https with the registry client of the image service, on
  the host, with the create's credentials. The credentials travel to the
  node with the create over the node connection, as a pod's sealed
  bindings do (ADR 0014, ADR 0015); `feosd` holds them in memory for the
  pull and writes them nowhere. Blobs are kept by digest in a cache of
  the node's. Layers may be gzip, zstd or uncompressed.
- **A bake pod unpacks.** A microVM of the node's own, with `feos-guest`
  and no container, takes the layers over vsock, unpacks them in order
  into a fresh volume formatted as ADR 0016 formats one, and stops;
  `feosd` seals the volume as the image's layer, keyed and catalogued as
  ADR 0022 says. No tenant's code runs in a bake pod, and no credential
  enters it.
- **The bake pod replaces the bake in an image's first pod.** A pod gets
  a layer; the guest never sees the registry, and no pull token is sent
  to it. A registry that takes only Basic authentication is pulled from
  like any other.
- **The guest's own image service stays** for what a sandbox's agent
  pulls itself, anonymously.

## Alternatives considered

- **The credential in the guest**, pushed into the pod for its pull: a
  credential in a pod, against ADR 0001, and still a pull per pod.
- **A pull-through proxy on the host over vsock**, as ADR 0014 proxies
  the Kubernetes credential, with a blob cache on the node: the
  credential stays on the host and blobs are fetched once per node, but
  every pod still unpacks its image into its disk, and every replica's
  start is an unpack. Baking makes the proxy unnecessary; the host's
  client of the registry is what the bake pulls with.
- **Unpacking on the host** into a filesystem image: the node runs no
  mkfs (ADR 0001, ADR 0016), and writing ext4 or erofs from Rust is a
  project of its own.
- **The bake in an image's first pod** (ADR 0022's rule meanwhile): the
  registry's token in the guest, a tenant's pod doing the node's work,
  and no Basic-only registries; kept until the node pulls, since FeCode
  needed private images from a token-issuing registry first.

## Consequences

- The node holds registry credentials in memory and a blob cache on
  disk; a pod of a service starts without a pull, in the time its layer takes to
  attach, and its first pod no slower than the rest.
- No credential enters any pod: the follow-up in
  [security.md](../security.md) closes, and a registry with Basic
  authentication alone works.
- A bake pod is a microVM per image per cluster, on the node's own
  account; its size and the blob cache's budget are the node's to bound.

## History

Decided on 2026-10-27 as a part of ADR 0022 (33ea94e, 2026-10-28). On
2026-10-29 ADR 0025 put the bake back into an image's first pod, with the
registry's token in the guest, so that FeCode's private images could run
first, and named the node's pull as the step that closes the exposure.
Split out of ADR 0022 as an ADR of its own on 2026-11-08; not built.
