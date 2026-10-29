# ADR 0022: Images come through the node: pulled with the create's credentials, baked once into a shared layer, built on the platform, and a service's pod supervised by its guest

**Status:** Proposed · **Date:** 2026-10-27

## Context

A service's pod is not yet what ADR 0004 decided it is, and its image
does not come the way ADR 0001 and ADR 0011 want it to.

**Images.** The guest pulls its own image: `feos-guest`'s image service
fetches the manifest and the layers over the pod's own network,
anonymously, gzip layers only, and unpacks them into the pod's disk.
Every new pod pulls from scratch, "once per pod and node even when the
same image ran next door a minute ago" (ADR 0011), into a disk of 8 GiB
that also holds its writable layer and its logs. Every replica of a
rollout pulls again. `ferund` resolves a tag to a digest before it
places a pod, anonymously too: "private registries are not supported",
a 401 looks like a 404, and a reference that carries a digest already
skips the registry and fails in the guest instead. The addons' images
come from Docker Hub, anonymously, under its rate limits. `diskd` has
`Bake` and `ImportBase` and `feosd` never calls them: "no baked images".
ADR 0001 lists private images as the first thing CaaS needs; ADR 0004
leaves open how: "images from private registries pulled through the
host, which adds the registry's credential"; the principle stands that
no credential enters a pod. The platform runs a registry already,
`oci.<domain>`, distribution with its blobs in the object store, whose
users are an operator's htpasswd.

**Builds.** Nothing on the platform builds an image. FeCode's stage
answers "builds are not available yet" for a service without an image,
and until something builds one, a project's stage and production run
images someone pushed by hand, from a sandbox that has no Docker
(FeCode's ADR 0008, FeOS ADR 0018).

**The pod.** ADR 0004 decided that the guest supervises its containers:
roles, a restart policy with backoff, startup, readiness and liveness
probes, `SIGTERM` and a grace period at stop, the image's user. None of
it is built. A container that exits stays exited; `ferund` notices by
asking `GetPod` every second for every running instance, over the node
connection and vsock, runs readiness checks of its own through the port
door, and restarts an exited container by rebooting its pod. Stopping a
pod is a reboot: no signal reaches a process, "a stop is a SIGKILL".
Every container runs as uid 0, whatever its image says. Its log is one
file, appended to without bound, without timestamps or the stream it
came from, gone with the pod. ADR 0018 called this the interim, and so
does `docs/architecture/control-plane.md` (follow-up 11).

A production application needs the opposite of each: a private image
pulled once, a rollout that starts a replica in the time a disk layer
takes to attach, a stop that lets a request finish, a crash that is
restarted inside the pod while the control plane is away, and a log
that is there when the pod is not.

## Decision

### Images are the node's artifacts, baked once and shared

- The node, not the guest, fetches a container image for a pod of a
  service, a job or an addon, and turns it into a **baked layer**: one
  sealed, content-addressed disk layer of `diskd`'s (ADR 0011), keyed by
  the image's source and manifest digest (ADR 0025), holding the
  image's root filesystem unpacked. The pod's container mounts that
  layer read-only and puts its writable layer on the pod's own disk, as
  it does with the unpacked image today; its process, exits and
  restarts do not change.
- A baked layer is made once per cluster and cached per node: `diskd`
  tiers it to the object store as it tiers hibernated disks, and a node
  that needs an image another node baked fetches the sealed layer,
  lazily by chunk, instead of baking again. A second replica, a replica
  on another node, a restart after a crash and a revert to last week's
  release all attach a layer that is there, once the image's registry
  has said yes to the create's credentials (ADR 0025).
- Baking is a pod's work, since the node shells out to nothing
  (ADR 0001) and writes no filesystem of its own: `feosd` pulls the
  layers over HTTPS with the image service's client on the host, keeps
  the blobs by digest in a cache of the node's, and runs a **bake pod**,
  a microVM of the node's own with `feos-guest` and no container, which
  takes the layers over vsock, unpacks them in order into a fresh volume
  (formatted as ADR 0016 formats one) and stops; `feosd` seals the
  volume as the image's layer. No tenant's code runs in a bake pod, and
  no credential enters it either. Until the node pulls, ADR 0025 bakes
  in an image's first pod instead, as gaios did; the bake pod replaces
  that with the host's pull.
- The guest's own image service stays for what a sandbox's agent pulls
  itself, anonymously, as today. A sandbox's default image is baked
  like any other, so a sandbox starts from a layer too.

### Private registries: the create's credentials, used by ferund and the node, never by a pod

- A create brings its **registry credentials** (ADR 0025): a sandbox's,
  sealed in its row, or an environment's service's or job's, from its
  secrets; never returned, and never the tenant's, since a tenant serves
  many. `ferund` resolves a tag to a digest with them and asks the
  registry with them before every new pod, and the plan's finding for an
  image that does not resolve says whether the registry refused or does
  not know it.
- The credentials travel to the node with the bake, over the node
  connection, as a pod's sealed bindings travel (ADR 0014, ADR 0015):
  `feosd` holds them in memory for the pull and writes them nowhere.
  The pod gets a layer; the guest never sees the registry.
- A reference with a digest is resolved like a tag: the registry is
  asked for the manifest, with the credential, so that a private image
  named by digest fails at plan, not in a pod. Layers may be gzip, zstd
  or uncompressed.

### The registry is not FeRun's

- Withdrawn by FeCode's ADR 0012, which drafted `ferund` as the token
  issuer of the platform's registry, with a namespace per tenant:
  `oci.<domain>` is FeCode's, and so are its tokens, since who may read
  and write a project's image is FeCode's knowledge. FeRun is a client of
  every registry alike, `oci.<domain>`, GHCR or Docker Hub: it pulls with
  the credentials a create brings (ADR 0025), issues no registry tokens
  and keeps no namespaces.
- The platform's own images, `ferund`, `feos-node`, the builder below,
  and the addons' images live under `platform/` in FeCode's registry,
  pushed by its super admins (FeCode's ADR 0012); a create pulls them as
  any other image.

### Builds run on the platform

- `ferund` builds images: `POST /api/v1/builds` with a source, a build
  context and a Dockerfile within it, build arguments, and the image to
  push, `<registry>/<repository>:<tag>`, in any registry. A build is
  a run of the platform's **builder** image, BuildKit, in a pod of the
  tenant's like a job's run (ADR 0018): placed, logged, bounded in time,
  retried never. Its result is a digest, kept with the build's record;
  its failure a record with the log's tail.
- The source is a snapshot `ferund` fetches for the build and puts into
  the pod as files: a git repository at a commit, fetched over HTTPS
  with the credential the caller passes for that one fetch (FeCode's
  git gateway with a token of a landing's; any forge with a token), or
  an archive the caller uploads. The credential is used by `ferund` and
  discarded; the pod gets the tree and the commit's ID as a build
  argument. No credential of the caller's enters the build pod but a
  push token it passes for that one build, scoped to the image's
  repository and short-lived (FeCode mints one per build for its
  registry, FeCode's ADR 0012).
- The microVM is the boundary between tenants (ADR 0001), so the
  builder's container may hold what a build needs, mount and user
  namespaces among them, which no container of a service holds. A
  build's cache is BuildKit's registry cache in the image's own
  repository, so the second build of a project is the first's work
  reused.

### A service's pod is supervised by its guest

ADR 0004 is built, in this order, and `ferund` drops its interim as
each part lands; the three points ADR 0004 left open are decided here:

1. **Stop.** `StopPod` sends the main containers `SIGTERM`, waits the
   pod's grace period, 30 s when unset, sends `SIGKILL`, then the
   sidecars the same way, then shuts the microVM down. A stop of the
   guest's container defaults to `SIGTERM` as its API says.
2. **Restarts in the guest** with Kubernetes's backoff, the restart
   count and last exit in the container's status, the writable layer
   kept. `ferund` stops rebooting pods for an exited container; its
   crash loop reads the guest's count.
3. **Exits and readiness as events**, not polls: the guest reports a
   container's start, exit and change of readiness on its uplink, and
   `feosd` forwards them to the control plane as pod events (ADR 0015).
   `ferund`'s one-second `GetPod` poll goes.
4. **Probes in the guest**: startup, readiness and liveness, exec, HTTP
   and TCP, run inside the pod's network; a failed liveness probe ends
   the process; readiness goes into the status and the events, and from
   there into the routes (ADR 0018, ADR 0021). `ferund`'s readiness
   checks through the port door go.
5. **Roles and the reaper**: init, sidecar and main containers in their
   order, an idle container's PID 1 the reaper, `reaper` on request.
6. **The image's user** runs the process, as Docker and Kubernetes do;
   `uid 0` only when the image says so or the spec asks for it. The
   image's `StopSignal` is honoured.
7. **Logs** are bounded: a container's log is a ring of a fixed size on
   the pod's disk, rotated, each line stamped with its time and stream;
   `StreamContainerLogs` serves it as before. When a pod ends, the last
   64 KiB of each container's log go into the pod's record and stay
   until the pod is deleted: a job's output is there when the job is
   not (ADR 0004's open question).

Per-container resources, ephemeral volumes for sidecars and volumes for
services (ADR 0016) are not part of this ADR.

## Alternatives considered

- **The credential in the guest**, pushed into the pod for its pull: a
  credential in a pod, against ADR 0001, and still a pull per pod.
- **A pull-through proxy on the host over vsock**, as ADR 0014 proxies
  the Kubernetes credential, with a blob cache on the node: the
  credential stays on the host and blobs are fetched once per node, but
  every pod still unpacks its image into its disk, and every replica's
  start is an unpack. Baking makes the proxy unnecessary; the host's
  client of the registry is what the bake pulls with.
- **A layer per image layer**, mounted as overlay lowers in the guest,
  as Kata and firecracker-containerd do: exact sharing of a base layer
  between images, but a drive per layer against the pod's drive limit
  and an overlay of many lowers in the guest. One layer per image, with
  `diskd`'s chunk deduplication across layers, gives most of the
  sharing for none of that.
- **Unpacking on the host** into a filesystem image: the node runs no
  `mkfs` (ADR 0001, ADR 0016's one exception is the guest's), and
  writing ext4 or erofs from Rust is a project of its own; a pod that
  boots in under a second is the node's own tool.
- **Lazy image formats** (eStargz, Nydus) and a converter beside the
  registry: a second image format to keep; a baked layer in the object
  store is fetched lazily by chunk already (ADR 0011).
- **Builds in the agent's sandbox**: the sandbox has no privileges and
  the build would depend on a workspace being there; a build at landing
  needs no sandbox and gives stage the image of main itself.
- **Builds by a CI outside** (GitHub Actions pushing to the registry):
  the GitOps path a caller may take (ADR 0018); FeCode's projects have
  no CI, FeCode is their workbench, and "stage follows main" is true
  only when the platform builds what main is.
- **kaniko or buildah** as the builder: BuildKit is what `docker build`
  is, with the cache and the frontends people's Dockerfiles expect, and
  in a microVM it may run with the privileges it wants.
- **`ferund` as the registry's token issuer**, with a namespace per
  tenant (this ADR's first draft): FeRun would own an authorization
  model of FeCode's projects and members that it has nowhere else;
  withdrawn by FeCode's ADR 0012.
- **Restarts and probes kept in `ferund`**: ADR 0004 rejected it, and
  ADR 0018 called it the interim: every exit and probe crosses vsock and
  the node connection, nothing restarts while `feosd` restarts, and a
  pod's stop cannot be graceful from outside.

## Consequences

- `feosd` gains the host's registry client with credentials, a blob
  cache by digest, the bake pod and the sealing of its volume as a
  layer; `diskd` serves baked layers and tiers them; the pod spec names
  an image's layer instead of, or beside, an image reference the guest
  pulls. A pod of a service starts without a pull, in the time its
  layer takes to attach.
- `ferund` gains the create's registry credentials (ADR 0025),
  resolution with them, builds with the builder image and their
  records, and loses its probes, its `GetPod` poll and its restarts by
  reboot as the guest takes them over. Releases pin digests as before.
- `feos-guest` gains what ADR 0004 lists and the ring log; its
  `ContainerSpec` and status grow additively (ADR 0007). Every image
  rolls once when its pods move from an unpacked image to a baked
  layer.
- The registry's authentication is FeCode's (FeCode's ADR 0012), and
  nothing of it is FeRun's. Addon blueprints name `platform/` images
  once they are there.
- A pod's disk no longer holds its image: the 8 GiB are the writable
  layer's and the log's; a size per pod stays ADR 0011's question.
- FeCode's stage and production get images the platform built from
  main, promotion carries their digests, and `values.images` stays for
  an image a project names (FeCode's ADR 0009).
- Built in two tracks. Images in ADR 0025's order, since FeCode needs
  private images first: the question before every new pod, credentials
  with the create and the registry's token for the guest's pull, the
  bake in an image's first pod; then builds, whose push tokens their
  caller brings; the bake pod and the host's pull last. Until then the
  registry's token is what the guest gets, scoped to one repository's
  pull and expiring in minutes, the one credential that enters a pod
  ([security.md](../security.md)), and a registry that takes only Basic
  authentication waits for the host's pull. The pod, apart from
  them: stop, restarts, exits and readiness as events, probes, so that
  `ferund` sheds its interim.

## Open questions

- **Garbage collection** of baked layers and blobs on nodes and in the
  object store: which release's images a cluster keeps, and for how
  long.
- **Multi-platform images** and a second architecture of nodes.
- **Per-container resources** and limits in the guest, and the memory a
  pod's sidecars may take.
- **Build limits**: how many builds a tenant runs at once, how long one
  may run, and the builder pod's size.
- **Scanning** images for known vulnerabilities before a release runs
  them.
- **Signing**: whether a build's result is signed and a release checks
  the signature.
