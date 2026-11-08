# ADR 0011: Disks are chains of sealed layers, served by one disk daemon per node

**Status:** Accepted · **Date:** 2026-10-15

Every pod's disk, and every volume (ADR 0016), is a block device served
by `diskd`, one daemon per node that owns the node's chunk cache and its
object store client. A disk is a chain of sealed, content-addressed
layers under a sparse writable top: what pods share, the guest's blank
data volume and the layers baked of images (ADR 0022), is stored once
per node and once in the object store, and a pod's own writes are sealed
into one layer of its own when the pod hibernates (ADR 0008). A pod's
memory goes the same way, and a restore serves it lazily while the
microVM runs. A disk lives as long as its pod, across stops, crashes and
restarts of `feosd` and of diskd itself, since a ublk device survives its
daemon. Without diskd, no pod comes up.

## Context

A pod's containers write into the pod's disk, and that disk has to
survive a stop, a crash and a restart of the node agent. ADR 0001 asks
for suspends that cost nothing on the node and for resumes on other
nodes, so a pod's disk and memory have to be able to live in an object
store, and come back from it faster than they could be downloaded whole.
Pulling an image in every new guest is the slow part of a pod's start:
seconds with a warm store, minutes with a cold one, once per pod and node
even when the same image ran next door a minute ago.

Firecracker attaches a raw file or a block device as a drive, nothing
else, and restores memory from a file or through a userfaultfd handler.
The node shells out to nothing and writes no filesystem of its own
(ADR 0001), so a pod's disk has to be a block device made of bytes the
node already has. FeOS's proof of concept built a disk daemon for this
next to its own guest; it comes into FeOS here.

## Decision

### One daemon per node owns the disks

- `diskd` is a daemon of its own on every node, run as root beside
  `feosd`. It owns the block devices behind pods' disks, the node's
  chunk cache and the object store client, and serves a gRPC API on a
  unix socket. `feosd` is its client; `diskctl` is the one for
  development. The object store is diskd's alone: `feosd` needs no head
  format and no store client of its own.
- A pod's disk outlives the Firecracker process that uses it, and diskd
  outlives `feosd`'s restarts and its own.
- diskd is a package of its own beside `feosd` (ADR 0009), with the
  object store's credentials in its configuration, root's alone. Its
  unit is ready once it serves, its devices recovered; `feosd` starts
  after it. A restart of diskd stops no pod. Under `feos-host-init`,
  diskd runs beside `feosd` and is started again when it ends.

### Disks are ublk devices

- A pod's disk is a device of the kernel's userspace block driver
  (ublk), served through `libublk`, the library of ublk's maintainer.
  With user recovery, a diskd that crashes or is upgraded leaves its
  devices in place and their I/O waiting; the next diskd takes them over
  from its state file. Pods see latency, never errors.
- Each device has one queue and one thread that owns everything its I/O
  touches. Nothing on the request path takes a lock: threads talk to the
  budget, seal and control actors through channels.
- Firecracker attaches the device with a writeback cache, so that a
  guest's flush reaches diskd and not only the host's page cache.

### A disk is a chain of layers on a pool of chunks

- A disk is a chain of sealed layers and a mutable top. The chain has a
  fixed shape: shared layers (a blank or base L0, and at most one baked
  image layer on it, ADR 0022) and at most one layer of the pod's own. A
  seal merges the top into the pod's layer, so a chain never grows.
- Layers are cut into content-addressed chunks, stored once per node and
  once in the object store, whatever pods share them. A layer's manifest
  says per chunk: data, explicitly zero, or absent, which means "read
  the layer below".
- The top is one sparse file per pod. Writes go there and nowhere else.
  A discard writes zeroes and never punches a hole, since a hole means
  "read the layer below"; the write path stays a plain write, and a seal
  finds what changed from the file's allocation.
- The node's cache holds raw chunks, so that reads need no
  decompression; the object store holds them compressed. The cache's
  budget, enforced by diskd, holds chunks that can be fetched again. The
  top is never evicted. A layer that is not uploaded, and a disk's own
  layer that no head names yet (sealed for a hibernation that was
  abandoned or failed), are pinned outside the budget: the node has their
  only durable copy.
- Commit order: chunks, then the layer's manifest, then the pod's head;
  upward to the object store in the same order. A crash leaves garbage,
  never a manifest without its chunks. A sweep deletes what the object
  store holds that nothing reaches; the caller names what is live, heads
  and layers, and nothing younger than a grace is touched.
- A marker says a chunk or a layer is in the object store, and a push
  skips what has one. The markers a push wrote for a disk or a memory
  layer that no head names yet are trusted for 12 hours, then what they
  mark is uploaded again: nothing in the store reaches it, so a sweep
  deletes it once it is older than its grace. A sweep's grace must
  therefore exceed 12 hours plus the longest hibernation.

### Memory goes the same way

A memory dump is sealed as a flat layer of larger chunks, so that a
checkpoint uploads only the chunks that changed. A restore does not
download it first: diskd serves Firecracker's userfaultfd backend from
the same cache and object store, and the microVM runs while its memory
arrives.

### A running disk can be baked

A running disk is sealed live, with the guest's filesystem frozen, into
a shared layer on L0: a bake. The seal fails if the device was written
to meanwhile. ADR 0022 decides what is baked and when; diskd only seals.

### Every pod has a disk

- Every pod has one disk from diskd, and there is no other kind: no pod
  without a disk, no disk of another kind. A disk is a capability of
  every pod, like its memory, so the pod's spec has no field for it.
  While diskd cannot be reached, pods do not come up. A pod's volumes
  (ADR 0016) are disks of diskd's too, beside its disk, with lives of
  their own.
- A disk starts from a blank data volume: an ext4 filesystem built with
  the guest, the same bytes from every build, shipped compressed, which
  the guest mounts for its images, its containers and what they write.
  `feosd` has diskd import the newest guest's volume once, as the L0 of
  every pod's disk.
- The blank volume's size is every pod's disk's size. A disk is thin: it
  costs what the pod writes, on the node and in the object store.
- A head records which layer is the pod's own, so that a restore
  elsewhere keeps the chain's shape: the next seal replaces the pod's
  layer instead of stacking a new one on it.
- Firecracker gets the disk as a device node of its own in its jail, made
  at every boot and restore from a link diskd keeps per pod: a device's
  number changes when it is started anew, and the snapshot holds the
  jailed path, which is the same on every node.

### A disk lives as long as its pod

- A boot starts the pod's disk: the one the node has, else the one of the
  pod's checkpoint (ADR 0008), else a new one on L0. Containers the disk
  holds from the boot before are adopted: their writable layers stay,
  their processes start anew.
- When the microVM ends, by a stop, a suspend, a crash or the guest, the
  disk stops and keeps its top. A started device locks its buffers in
  memory, which a pod that runs nothing should not hold.
- A delete removes the disk and its top. While diskd cannot remove it,
  the delete fails and can be repeated, so that no top is left behind.
- A hibernation stops, seals and uploads the disk, and removes it from
  the node once the head is up (ADR 0008).

### Every call of feosd's can be repeated

- `feosd` restarts, and its calls get lost. Opening a disk that runs
  returns it; sealing a disk without a top returns the layer it sealed.
  A seal points the device at the new layer before it removes the top,
  so that a crash in between loses nothing.
- Each call that changes a disk holds the disk's lock until its work on
  the node ends, whether or not its caller is still there. A seal whose
  caller went away before it committed the new layer stops at its next
  chunk, and the disk opens again as it was; one that committed it ends
  first, and the disk opens again over the sealed layer. Its upload
  stops where it is; what was uploaded stays, and the next push skips
  it.
- The head of a checkpoint is diskd's to write, last: from the device's
  own record, only once every layer it names and the vmstate are in the
  bucket, and never over another head.

## Alternatives considered

- **FUSE** serving a file: a daemon that dies takes every pod's disk with
  it, its operations do not fit a block device, and each request crosses
  the VFS.
- **NBD**: a socket protocol whose reconnect is no match for ublk's
  recovery.
- **Our own binding of ublk's `uring_cmd` interface**: a second
  implementation of an interface that is still moving, in a daemon every
  pod depends on.
- **A compressed local cache**: decompression on every read.
- **dm-thin, qcow2, ZFS or btrfs snapshots**: bound to one node's storage
  where the disk has to move between nodes, with dedup by block, not by
  content. dm-thin stays the fallback for a node without ublk.
- **A reflink copy of a local image per pod**: cheap, but a second disk
  path that cannot be sealed the same way.
- **One daemon per pod**: the cache and the object store client belong
  to the node.
- **`feosctl disk`** instead of `diskctl`: diskd's API is internal to the
  node, like the microVM layer, and two of `diskctl`'s commands talk to
  the kernel through libublk, which `feosctl` should not link.
- **A disk in the pod's spec, optional**: two kinds of pods, and every
  capability, a stop, a crash, a hibernation, decided twice. The proof
  of concept kept pods with plain disk files beside diskd's and dropped
  them for that.
- **mkfs on the node**, by `feosd` or the guest: a tool to shell out to,
  or an ext4 writer of our own. A blank volume built with the guest is
  the same file everywhere. The guest runs `mke2fs` for volumes
  (ADR 0016); the pods' disks keep the blank volume, whose chunks every
  pod shares, and no pod waits for a mkfs.
- **The blank volume uncompressed in the package**: dpkg writes holes as
  zeroes, the whole disk's size per guest version.
- **A disk size per pod**, by a blank volume per size or by growing the
  filesystem online in the guest: the kernel zeroes the inode tables of
  what it grows, 1.6 % of it, written into the top. With `mke2fs` in the
  guest (ADR 0016), a disk of the pod's size could also start empty and
  be formatted there. Open, in the backlog.
- **Devices that stay started** while their pod is stopped or suspended,
  as the proof of concept's did: buffers locked per pod that runs
  nothing.
- **feosd linking the chunk store** instead of a daemon of its own: a
  restart or an upgrade of `feosd` would take every pod's disk with it.
- **Baking in `feosd`'s own pods only**, so that no tenant's state lands
  in a shared layer: the bake point lies before any container of the
  pod's has run, which keeps the layer clean (ADR 0022); a bake pod of
  the node's own is ADR 0050.

## Consequences

- diskd is node-critical: while it is down, every pod's disk I/O waits.
  Its unit restarts it, and `feos-host-init` starts it again.
- A node's cache can outgrow its budget by the pinned layers: the own
  layers of pods whose hibernation was abandoned or failed, until their
  next head. They are bytes the node held before the seal, as tops.
- The host kernel needs ublk with user recovery and userfaultfd, and
  builds both in. Requests are copied once until ublk's zero-copy.
- One thread per device. Hundreds of pods mean hundreds of mostly idle
  threads until libublk serves several devices from one.
- libublk comes from a pinned git revision; its async API is not
  released.
- The state directory belongs on a filesystem of its own, preferably
  XFS, which the host kernel builds in, with room for the cache budget
  in raw chunks. On a node whose operating system is FeOS, the state
  lives in the tmpfs: the pods' disks are in memory, and nothing tiers.
- A node that tiers to an object store needs its credentials. Without
  them diskd serves from its local store alone: nothing hibernates and
  nothing is baked.
- `feosd` needs diskd: without it, no pod comes up. Tests that run
  `feosd` in a container run diskd beside it, and give the container the
  node's `/dev`, where ublk's devices appear.
- What a pod writes survives a stop, a crash and a restart of `feosd` or
  diskd; a pod's containers come back with their writable layers.
- Every guest version ships its blank volume; a guest whose volume
  changes starts new disks on another L0, and a layer baked on one L0
  fits no other (ADR 0022).
- The sweep's caller is the control plane, which knows every pod, every
  head and every baked layer; `feosd` sweeps nothing.

## History

Decided on 2026-10-14 and 2026-10-15 (58c61fe, 13a9a94), with diskd and
its chunk store taken over from the proof of concept (87d3eae,
2026-10-14); packaged beside `feosd` and run under `feos-host-init` the
same day (5c99f1a). Volumes were split out as ADR 0016 on 2026-10-16.
Pinned unheaded layers, the journal of markers and the lock per disk
came on 2026-10-17 (b1d1c18, 8189e5c), and the state directory's lock
with diskd outliving `feosd`'s stop on 2026-10-26 (88469ca, ADR 0020).
The bake, which `feosd` did not call at first, has been in use since
2026-10-29 (11537e4, ADR 0022). Accepted on 2026-11-08 as built.
