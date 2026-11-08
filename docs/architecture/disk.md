The Disk Daemon
===============

`diskd` serves pods' disks as block devices over a chunk store that it
tiers to an object store, and restores pods' memory from the same chunks.
Every pod has a disk from it, and hibernates into its object store, and
so does every volume. The decisions behind it:
[ADR 0011](../adr/0011-disk-daemon.md),
[ADR 0008](../adr/0008-suspend-and-restore.md),
[ADR 0016](../adr/0016-volumes.md).

## Processes

- **`diskd`** (`crates/diskd`): one per node, as root, beside `feosd`:
  under systemd (`diskd.service`, of the package `diskd`) or
  `feos-host-init`. It needs the kernel's ublk driver (`/dev/ublk-control`,
  `CONFIG_BLK_DEV_UBLK`) and, for memory, userfaultfd
  (`CONFIG_USERFAULTFD`). Each device has a thread of its own, and a
  thread that fetches its missing chunks. Once it serves, its devices
  recovered, it sends `READY=1` to `$NOTIFY_SOCKET` if systemd set one.
- **`feosd`** is its client, on `--diskd-socket`
  (`/var/lib/feos/disk/diskd.sock`); see [below](#the-pods-disks).
- **`diskctl`** (`crates/diskctl`): its client for development, the lab
  scripts, tests and operators. It prints JSON.

`diskd [--state-dir DIR] [--socket PATH] [--budget-bytes N]
[--s3-concurrency N] [--mem-readahead N] [--s3-prefix P]
[--stop-after-lock FILE]`. The state
directory defaults to `/var/lib/feos/disk`, the socket to
`<state-dir>/diskd.sock` (mode 0660, as `feosd`'s), the cache budget to
8 GiB. Logs are JSON when stdout is not a terminal; `RUST_LOG` filters
them.

One diskd per state directory: it takes an exclusive `flock(2)` on
`<state-dir>/diskd.lock` before anything else and holds it until it ends,
waiting while another process holds it. SIGTERM ends diskd at once; with
`--stop-after-lock FILE`, diskd serves on after it until no process holds
`FILE`'s lock, then exits: for a diskd stopped together with a `feosd`
that suspends its pods first, through diskd's disks
(`/var/lib/feos/feosd.lock`, [node.md](node.md#on-kubernetes)).

The object store is S3 or compatible, configured from the environment:
`S3_BUCKET`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
`AWS_ENDPOINT_URL` (path-style) and `AWS_REGION` (default `us-east-1`).
Without them diskd runs from its local store only: what is not cached
there fails to read.

## A pod's disk

A disk is a chain of layers, L0 first, and a mutable top:

```
L0       a base image (ImportBase), or a blank data volume
L1       a baked image layer on L0 (Bake), optional
S        the pod's own layer, from its last seal, optional
top      a sparse file: what the pod wrote since
```

- `CreateDisk(pod, chain)` starts a device over the chain and a fresh
  top, and links `<state>/live/<pod>/disk` to `/dev/ublkb<N>`. A device
  has 4 KiB physical and 512 B logical blocks, one queue of depth 128,
  1 MiB requests, a volatile cache (flushes reach diskd) and discard.
- Reads go top first, then down the chain: a chunk is read from the
  cache, fetched from the object store into it when missing, or zero.
  Writes go to the top. Discards and write-zeroes write zeroes into the
  top.
- Layers are cut into 64 KiB chunks (memory layers into 256 KiB ones),
  content-addressed with blake3 and stored once. A layer's manifest is a
  binary table with, per chunk, data, zero or absent (read the layer
  below); the layer's ID is the blake3 of the manifest. A seal finds the
  top's changes with `SEEK_DATA` and `SEEK_HOLE` and compares hashes,
  which is why a discard writes zeroes and never punches a hole.
- `ImportBase(path)` seals a raw image into an L0; an image whose name
  ends in `.zst` is unpacked first, sparse, into `staging/`.
- `StopDisk` stops the device and keeps the top. `OpenDisk(pod)` starts it
  again over the same chain and top, or returns it if it runs;
  `OpenDisk(pod, head_key)` starts it on a node that has no disk of the
  pod, from a head in the object store, with a fresh top.
- `Seal(pod, upload)`, on a stopped device, merges the top into the pod's
  layer S, points the device's row at it and then removes the top; with
  `upload`, it pushes the chain. A disk without a top is sealed already,
  and the seal returns its newest layer: `Seal` without upload and then
  with it seals, and then uploads. A seal whose caller goes away before
  it committed the new layer stops at its next chunk and leaves the disk
  as it was, its row and top unchanged; once the layer is committed, the
  seal ends on the node, and its upload stops where it is: what it
  uploaded stays, listed in the disk's journal (below). `Bake(pod)` seals
  the frozen top of a running device into a layer on L0 and changes
  nothing about the pod.
- `PutHead(pod, epoch, memory, volume_heads)` uploads the head of a
  checkpoint of a sealed, stopped disk: its chain, how much of it is
  shared, its size, the memory the caller sealed and put beside it, and
  the heads of the pod's volumes. It refuses a head whose layers,
  vmstate or volumes' heads are not in the bucket, and one that exists:
  heads are never overwritten. `GetHead(key)` reads one.
- `RemoveDevice` forgets a stopped pod.
- `CreateDisk`, `OpenDisk`, `StopDisk`, `RemoveDevice`, `Seal` and
  `PutHead` hold the disk's lock until their work on the node ends, also
  when their caller goes away; a call waits for it up to 10 minutes and
  fails `UNAVAILABLE` "disk `<pod>` is busy" then. So an `OpenDisk` that
  comes during a seal starts the device after it: over the sealed layer,
  or over the top as it was when the seal's caller went away before the
  layer was committed. A push holds no lock.

When diskd ends, its devices stay. The next diskd recovers every device in
`diskd.state` before it serves: I/O that waited goes on.

## The cache

The cache holds raw chunks within its budget (`--budget-bytes`), and drops
the least recently used ones beyond it: chunks it can fetch again. Those
it cannot are pinned, outside the budget, and never evicted, not by
`Evict` either: the chunks of every layer of a device's chain that is not
uploaded, and of a disk's own layer while no head names it. A seal marks
the disk unheaded (`unheaded/<disk>`) until `PutHead` or `RemoveDevice`
of the disk, or an `OpenDisk` from a head, and holds its new layer's
chunks from their admission on. A hibernation that was abandoned or
failed leaves the disk so until its next head: the node has the only copy
of what the pod wrote.

A marker (`uploaded/chunks/<hh>/<chunk>`, `uploaded/<layer>`) says a chunk
or a layer is in the bucket, and a push skips what has one.
`unheaded/<disk>` is the journal of what was pushed for the disk while it
is unheaded, and `unheaded-memory/<layer>` that of a memory layer no head
names yet: the time it started at, then a chunk per line, or for a disk a
layer (`L <layer>`), each listed before its marker is written. Only a seal
starts a disk's journal; a push that still runs once it is gone marks
nothing. Nothing in the bucket names what a journal lists, so a sweep
deletes it once it is older than its grace; at the start of every push,
the markers of a journal older than 12 hours go, and what they marked is
uploaded again.

## Memory

`SealFile(path)` seals a memory dump into a flat layer of 256 KiB chunks.
`OpenMemory(pod, layer, socket)` binds a socket for Firecracker's UFFD
memory backend (`PUT /snapshot/load` with `backend_type` `Uffd`) and serves
the guest's page faults from the layer; pages the guest reported free come
back as zero pages. `CloseMemory` detaches it.

## The pods' disks

Every pod has one disk (`crates/services/pod-service/src/disk.rs`):

- **L0** is the blank data volume of the newest guest installed,
  `/usr/lib/feos/guest/<version>/data.img.zst`: an 8 GiB ext4 filesystem,
  label `feos-data`, made by `make guest-data` (`hack/guest/data/mk-data`)
  with a fixed UUID, hash seed and time, so that every build is the same
  file, and its inode tables and journal zeroed. `feosd` has diskd import
  it the first time a pod needs it after `feosd` started.
- **A boot** of a pod (create, start) opens its disk: `OpenDisk(pod)` if
  diskd has it, else `OpenDisk(pod, head)` from the pod's checkpoint, else
  `CreateDisk(pod, [L0])`. The microVM's `data_disk` is the link
  `<state>/live/<pod>/disk`; the microVM layer makes a device node of it,
  `data` in the jail, at every start and restore, and Firecracker attaches
  it as `/data` with `cache_type` `Writeback`. The guest mounts it at
  `/var/lib/feos`. On a boot after which the disk holds the pod's
  containers, `feosd` creates them with `adopt`: their writable layers and
  logs stay.
- **When the microVM ends**, by a stop, a suspend, a crash or the guest,
  `feosd` stops the disk (`StopDisk`) and lets go of memory diskd served
  it (`CloseMemory`). A resume opens the disk before it restores the
  microVM.
- **A delete** stops and removes the disk (`RemoveDevice`), its top with
  it, and fails while diskd cannot.

A pod from before pods had disks gets one at its next boot, in a new
microVM.

## The volumes' disks

A volume (`feos.volume.v1`, [node.md](node.md#volumes)) is a disk of its
own, `volume-<id>` in diskd and in the bucket
(`crates/services/pod-service/src/disk.rs`):

- **Its L0** is empty: one chunk of zeroes, from
  `/var/lib/feos/volumes/empty-layer.img`, which `feosd` writes and has
  diskd import the first time a volume needs it; the same layer on every
  node. The disk is as large as the volume (`CreateDisk(volume-<id>,
  [L0], size)`), and what lies beyond the L0 reads as zeroes.
- **A boot** of a pod opens each of its volumes after its disk, in the
  order of its spec: `OpenDisk(volume-<id>)` if diskd has it, else
  `OpenDisk(volume-<id>, head)` for a checkpointed volume, else a blank
  disk for a volume without its filesystem. A volume that has its
  filesystem and whose disk diskd lost fails the boot. The microVM gets
  the links as `volume0`, `volume1`, ... after `data`.
- The disks stop with the microVM, as the pod's does, and stay when the
  pod is deleted. `DeleteVolume` stops and removes the disk.

## Hibernation

`HibernatePod` (`feos.pod.v1`) takes a pod off the node, in a task of
`feosd`'s that holds the pod's lock. The call waits for it and gets its
answer, and so does a `HibernatePod` that comes while it runs; a call that
is cancelled, or whose connection goes, leaves it running. Until it ends,
`PodInfo.hibernation` shows its phase, whether a resume abandons it, its
epoch and the request ids of its calls.

1. a running pod is suspended (`purge_guest`), its snapshot in its jail;
2. `StopDisk`, `Seal` without upload, then `Seal(upload)`: the disk's
   chain is in the bucket;
3. for each volume: `StopDisk(volume-<id>)`, `Seal` without upload, then
   `Seal(upload)`; a volume that is checkpointed already keeps its head;
4. `SealFile(memory, upload)`, and `PutObject` of the vmstate to
   `snapshots/<pod>/<epoch>/vmstate` and of the pod's spec, a
   `feos.pod.v1.PodSpec`, to `snapshots/<pod>/<epoch>/pod.pb`;
5. past the point of no return, for each volume sealed in step 3:
   `PutObject` of its record, a `feos.volume.v1.Volume` with its size,
   labels and whether it has its filesystem, to
   `snapshots/volume-<id>/<epoch>/volume.pb`, and `PutHead(volume-<id>,
   epoch)`; then `PutHead(pod, epoch, memory, volume_heads)`: the head,
   with what the snapshot pins (from its `host.json`) and the volumes'
   heads;
6. the pod is `HIBERNATED`, with `PodInfo.checkpoint`, and its volumes
   are `CHECKPOINTED`, with theirs; its microVM and snapshot, its disk
   and its volumes' (`RemoveDevice`), its addresses and its TAP device
   go. A `feosd` that starts with a hibernated pod removes what is left.

The epoch is the seconds since 1970, or one more than the last
checkpoint's of the pod and of each of its volumes. A stopped or failed
pod skips step 1 and the memory.

A `ResumePod` that comes before step 5 abandons the hibernation, unless a
`HibernatePod` of it said `no_abandon`, as the control plane's drains do,
or the pod has no memory. The hibernation stops before its next step, or
drops the call to diskd it is in, a seal or an upload: diskd stops a
seal on the node before it commits the new layer, and a push where it
is; the suspend runs to its end. Every `HibernatePod` of it fails with
`ABORTED`, and so does a later one with one of its request ids. The
resume then takes the pod's lock and resumes the pod from its snapshot,
on its disk and volumes as the hibernation left them: `OpenDisk` waits
for a seal that diskd still stops or ends, and starts a sealed disk over
its sealed layer, with a fresh top, and one whose seal stopped over its
top. Nothing names what was uploaded: the next hibernation's push
skips the chunks that have their markers ([the cache](#the-cache)), and
a sweep deletes the rest. A resume that comes in step 5 or later, or to a
hibernation it may not abandon, waits for it, and restores the pod from
its new checkpoint.

`RestorePod(pod, head, cold)` brings it back; `ResumePod` of a hibernated
pod is `RestorePod` with its own checkpoint:

1. a node that does not have the pod reads the head (`GetHead`) and the
   spec (`GetObject`) and records the pod, hibernated; the request's
   `security_groups` and `name_tables`, each when not empty, replace the
   spec's, here as on a node that has the pod (ADR 0035: a sandbox comes
   back in its groups of the moment, which the node must have);
2. each volume's head and record are read; the restore fails before
   anything changes if one is missing, or if the node has the volume
   attached to another pod, or available, with data of its own;
3. the disk the node has of the pod goes (`RemoveDevice`), and so do its
   volumes', which are recorded as the checkpoint has them; the pod gets
   addresses and is `PENDING`, `restoring from <head>`;
4. with memory in the head, an installed Firecracker of its version and
   the same CPU model and kernel, and unless `cold`: `OpenDisk(pod, head)`,
   `OpenDisk(volume-<id>, head)` for each volume, `CreateVm`, `GetObject` of the vmstate, `OpenMemory` on
   `memory.sock` in the microVM's directory, and `ResumeVm` with
   `RemoteSnapshot`: Firecracker loads the vmstate with the `Uffd`
   backend, `/memory.sock` in its jail, and the guest runs; it is pushed
   the time and its network. The pod is `RUNNING` when the call returns;
5. otherwise, or if step 4 fails before the guest ran, the pod boots anew
   on the checkpoint's disk and volumes, in the background, and adopts
   its containers.

## Directories

| Path (in the state directory) | Holds |
|---|---|
| `diskd.sock` | the API |
| `diskd.lock` | the lock of the one diskd on the state directory, with its pid |
| `diskd.state` | the device table: pod, ublk device id, chain, size |
| `cache/<hh>/<hash>` | raw chunks, within the budget |
| `staging/` | chunks and manifests before their rename, a compressed image while it is imported |
| `layers/<layer>.manifest` | layer manifests |
| `live/<pod>/top.img`, `live/<pod>/disk` | a pod's top, and the link to its device |
| `snapshots/<pod>/head.json` | a pod's head after its last seal: chain, size |
| `uploaded/` | which chunks and layers are in the object store |
| `unheaded/<disk>` | a disk whose own layer no head names yet, and the journal of the chunks pushed for it |
| `unheaded-memory/<layer>` | the journal of the chunks pushed for a memory layer no head names yet |
| `zero.bin` | zeroes to serve reads from |

The state directory belongs on a filesystem of its own, preferably XFS.

| Key (in the bucket) | Holds |
|---|---|
| `chunks/<hh>/<hash>.zst` | chunks, zstd |
| `layers/<layer>.manifest` | layer manifests, uploaded after their chunks |
| `snapshots/<pod>/<epoch>/head.json`, `…/vmstate`, `…/pod.pb` | a checkpoint: its head (`PutHead`), last; the caller's own objects (`PutObject`) |
| `snapshots/volume-<id>/<epoch>/head.json`, `…/volume.pb` | a volume's part of its pod's checkpoint, before the pod's head |

A head (`head.json`) names the disk's chain, L0 first, how many of its
layers are shared (`shared_depth`; the rest, at most one, is the pod's
own and the next seal replaces it), the disk's size and chunk size, the
epoch, for a checkpoint with memory the memory layer, the vmstate's key
and what the snapshot pins: the Firecracker version, the CPU model and
the host's kernel, and the keys of the heads of the pod's volumes
(`volumes`, left out when there are none).

`Sweep` deletes what the bucket holds that the live heads and layers it is
given do not reach, and nothing younger than a day; a live head keeps the
heads of its volumes. The live heads are every pod's
`checkpoint.head_key`, on every node: a pod reads from the checkpoint it
came from until its next one; and every volume's, whose pod may be gone. `feosd` sweeps nothing; the
control plane, which knows all pods, will. A grace must be longer than
12 hours and the longest hibernation together, or a sweep may delete a
chunk whose marker a push still trusts ([the cache](#the-cache)):
`no_grace` is for labs.

## Development

    make diskd               # static diskd and diskctl
    make diskd-lab-up        # a lab diskd with its state in target/diskd/state
    make diskd-lab           # the lab tests against it, as root
    make diskd-lab-drill     # SIGKILL diskd under fio; it must recover
    make diskd-lab-down      # LAB_FLAGS=--clean also stops its devices
    make diskd-mkscratch     # an XFS volume for the benchmarks
    make diskd-bench-store   # the chunk store's benchmark

The lab tests are `#[ignore]`d in `cargo test`. Those that need S3 skip
without `S3_BUCKET` and `AWS_*` in the environment, and use a key prefix
of their own.

`make test-pods` runs a diskd beside `feosd` in its container, with the
node's `/dev`, and the pods' integration tests against it: disks across a
stop, a start and a crash, and hibernation with and without memory, from
an evicted cache; volumes across stops and pods, formatted once, mounted
read-only, attached to one pod, and hibernated with their pod. `feosd`
reaches diskd there through a gate of the tests'
(`crates/feos/tests/integration/diskd_gate.rs`), which holds the calls a
test names: a resume abandons a hibernation while the disk, a volume or
the memory uploads, and waits for a drain's and for one past its point
of no return; a hibernation outlives its call, and two calls join one.
With S3 in the environment, or in
`~/.config/feos/diskd/s3.env` (`FEOS_S3_ENV`), it hibernates under a key
prefix of its own and sweeps it at the end; without, the hibernation tests
skip.

`crates/diskd/clippy.toml` and `crates/disk-store/clippy.toml` deny
`Mutex` and `RwLock`: diskd's request path is shared-nothing, and threads
talk to the budget, seal and control actors through channels (ADR 0011).
A use outside the request path needs an `#[allow]` and a comment saying
why.
