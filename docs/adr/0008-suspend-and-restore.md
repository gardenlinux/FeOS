# ADR 0008: Suspend to the node, hibernate to object storage, restore anywhere

**Status:** Accepted · **Date:** 2026-10-15

A suspend writes a full snapshot of a running pod into its microVM's
directory on the node, ends its Firecracker process and gives the guest's
free memory back; a resume restores it in two steps and discards the
snapshot once the guest runs. A hibernation takes a pod off the node into
object storage as a checkpoint, its disk's layers, its memory as a layer,
its vmstate, its volumes and its spec, with a head written last, in a task
of `feosd`'s that holds the pod's lock; until the head is being put, a
resume abandons it. A restore brings a checkpoint back on any node that
names the same object store, with its memory served lazily where the
snapshot's pins fit the node, and as a fresh boot on the checkpoint's disk
where they do not. When a pod suspends, hibernates and where it comes back
is the control plane's policy; `feosd` only does it.

## Context

ADR 0001 asks for cheap suspends, fast resumes, memory back on suspend and
resumes on other nodes: the sandboxes are idle most of the time, and a
service of CaaS that nobody calls scales to zero. Firecracker writes
snapshots of a VM's state and memory and restores them, but only on the
Firecracker version and CPU model they were written on, and it does not
know a snapshot written on another host from a corrupted one. A restored
guest keeps idle vsock connections that are not woken until its keepalive
notices, seconds later.

Every pod has a disk from the disk daemon (ADR 0011), whose layers are
sealed, content-addressed and tiered to the object store in chunks; a pod's
memory can be a layer like them, and a restore can read it lazily through
a userfaultfd handler. A pod's volumes (ADR 0016) are disks of the same
daemon.

## Decision

### Free memory goes back to the host

A pod's microVM has a virtio balloon with free page reporting and
statistics. Free page reporting is what keeps snapshots small: the guest
hands freed memory back, and Firecracker writes it as zeroes.

### Suspend

A suspend writes a full snapshot into the VM's directory, ends its
Firecracker process and punches holes over the pages of zeroes. Asked to
purge, `feos-guest` first drops its caches and compacts its memory, and
`feosd` waits until the VMM stops shrinking.

### Resume

- A resume takes two steps. It loads the snapshot with the vCPUs stopped;
  a failure up to there keeps the snapshot and the VM suspended. Then it
  resumes the vCPUs and removes the snapshot: once the guest ran, its disks
  have moved on, and the snapshot would roll it back against them.
- After a resume, `feosd` pings the guest once and pushes the time and the
  network again (ADR 0007). It does not wait for the guest's uplink.
- A boot of a suspended VM discards its snapshot. That is the way out when
  a snapshot cannot be restored.

### A snapshot pins what it was written with

A snapshot records the Firecracker version, the CPU model and the host's
kernel it was written on, and is not loaded elsewhere. Nodes keep the
Firecracker versions their snapshots need (ADR 0009). The guest's memory
holds the `feos-guest` it was suspended with, which a restore brings back
as it was (ADR 0007).

### Hibernation: a pod in object storage

- A hibernated pod is a record on the node with its checkpoint, and
  nothing else of it there. When a pod hibernates is the control plane's
  policy, not `feosd`'s, as is where it comes back. Without a control
  plane, the API and `feosctl` drive it.
- A checkpoint is the pod: its disk's layers, its memory as a flat layer,
  Firecracker's vmstate, its spec, and the head that names them, last. A
  checkpoint has an epoch, which grows with every hibernation of the pod,
  so that no head is written twice. All of a node's traffic with the
  object store runs through the disk daemon.
- A hibernation runs on its own, in a task of `feosd`'s that holds the
  pod's lock, on a node whose disk daemon has an object store; elsewhere
  it is refused before the pod changes. It goes in this order: the pod is
  suspended if it runs, its disk stops, is sealed on the node and
  uploaded, its volumes are sealed and uploaded, its memory is sealed and
  uploaded, its vmstate and its spec are put beside them, and the head
  goes last. Then the pod is hibernated, and what it held on the node
  goes: its microVM and snapshot, its disk, its volumes' disks, its
  addresses and its TAP device. A call that is cancelled, or whose
  connection goes, leaves the hibernation running; one that comes while it
  runs waits for it and gets its answer. Until the head is up the pod
  stays suspended on the node, and a hibernation that fails leaves it so;
  what it uploaded is garbage for the sweep. A stopped or failed pod
  hibernates its disk alone.
- The pod's volumes (ADR 0016) go with it: each is sealed and uploaded
  beside the disk, with a head of its own before the pod's, which names
  them. They leave the node with the pod's disk.
- The guest's filesystem is not frozen. A restore with memory brings the
  memory and the disk back together; a boot on the disk alone sees it as
  after a power cut at the snapshot, synced a moment before, which ext4's
  journal keeps whole.
- A resume that comes while a pod hibernates abandons the hibernation,
  unless the caller said it may not (a drain), the pod has no memory to
  resume, or its head is being put: the hibernation stops at its next
  step, between two of the disk daemon's calls or by dropping the one that
  seals or uploads, which stops a seal on the node before it commits its
  layer, and the pod resumes on the node from its snapshot, on its disk
  sealed or not. What was uploaded stays, and its chunks serve the next
  hibernation. The volumes' heads are put with the pod's, after the point
  of no return, so an abandon leaves no head. A resume that comes later
  waits for the pod's lock and finds it hibernated: it restores it. A
  resume of a hibernated pod is a restore from the pod's own checkpoint.

### Restore from a checkpoint

- A restore takes a checkpoint's head. A node that does not have the pod
  records it from the checkpoint's spec; one that has a disk of the pod
  drops it for the checkpoint's. The pod gets the node's addresses, which
  the guest is pushed after the restore, and comes back in the security
  groups and name tables the control plane sends with the restore
  (ADR 0035). Its volumes come from the checkpoint too; a restore whose
  volumes another pod on the node holds, or that the node has with data
  of their own, is refused before anything changes (ADR 0016).
- Whether the memory is restored is decided before anything starts, from
  what the snapshot pins: the Firecracker that wrote it must be installed,
  on the same CPU model and kernel. Then the disk daemon serves the memory
  to Firecracker's userfaultfd backend from the memory layer, and the
  guest runs before its memory is downloaded. Otherwise, or when asked to
  restore cold, the pod boots anew on the checkpoint's disk and adopts its
  containers: what they wrote stays, what ran in them is gone. A restore
  of the memory that fails before the guest ran falls back to that boot;
  the disk is as the checkpoint has it.
- A pod keeps its checkpoint in its record while it runs from it: its disk
  reads layers of it lazily, so a sweep of the object store keeps the
  heads of all pods' checkpoints.
- Stopping a hibernated pod drops its memory; a start boots it on the
  checkpoint's disk. Deleting it forgets it; its objects go with a sweep.
- A crashed pod boots anew on its disk at its next start, like a
  checkpoint without memory.

## Alternatives considered

- **Waiting for the uplink after a resume.** The resume would hang until
  the guest's keepalive fires.
- **Diff snapshots on top of a base.** Nothing takes repeated snapshots of
  one boot, and dirty page tracking costs on every write. Hibernation does
  not need them: a memory layer is uploaded in chunks the object store
  does not have, which is what an increment is.
- **Hibernation as a flag of the suspend**, or tiers `feosd` decides: when
  a pod leaves its node is a policy, and policies are the control plane's
  (ADR 0001, ADR 0015).
- **Exporting a pod through the pod API** as a stream of bytes, for the
  caller to keep: every byte would cross `feosd` and the control plane,
  and a restore could not read the memory lazily.
- **Uploading outside the pod's lock**, so that a pod stays resumable
  while its checkpoint uploads: a resume during the upload resumes on the
  node, and the checkpoint stays but does not count. The lock stays: a
  resume stops the upload instead of racing it, and the pod's disk is the
  sealed layer plus a new top, so nothing on the node is shared between
  the two.
- **Trying the snapshot and booting anew if the load fails**, whatever the
  pins say: a snapshot loaded on the wrong host can corrupt the guest
  instead of failing. The pins decide; a load that fails all the same
  falls back.
- **Freezing the guest's filesystem** before the snapshot: every restore
  with memory would have to thaw it, and a guest that is not thawed stops
  writing.
- **Downloading the memory before the restore**, a file Firecracker loads:
  the guest waits for all of it. With the memory served lazily, it waits
  for what it touches.
- **A hibernation that a resume cannot interrupt.** A caller that wants
  its sandbox back would wait minutes per GiB for an upload it does not
  need; abandoning costs nothing but the chunks already up, which the next
  hibernation reuses.

## Consequences

- Pods have no huge pages: Firecracker restores hugetlbfs memory only
  through a userfaultfd handler, and backs neither an initramfs nor a
  balloon with it.
- A suspended pod's disks must stay as they are until it is restored.
- Where the snapshots' directory is a tmpfs, as on a node whose operating
  system is FeOS, a snapshot stays in memory: suspending frees only what
  was free in the guest, and a restored VM keeps the snapshot's pages
  mapped besides its own until it stops. Suspend saves memory where that
  directory has storage below it (ADR 0009).
- A pod restored with its memory depends on the disk daemon until its
  next suspend or stop: the daemon serves pages the guest has not touched
  yet, and a userfaultfd does not survive the daemon. A restart of the
  daemon freezes such guests until they are stopped; a disk survives it.
  The first suspend after such a restore reads every page through the
  daemon.
- Checkpoints pile up in the object store until a sweep with the heads
  of all pods removes what none of them reaches; the control plane knows
  them all.
- Two restores of one checkpoint are two pods with one past. The control
  plane places a pod on one node at a time.
- The control plane's policies are the suspend tiers (ADR 0015): it
  pauses an idle pod on its node, hibernates one paused for long into the
  object store, and empties a node by hibernating its pods with the
  abandon ruled out.

## History

Suspend and resume were decided on 2026-10-15 with the pods (ADR 0004);
hibernation into object storage and the restore on any node the same day
(2aaced9). The hibernation moved into a task of its own that a resume
abandons on 2026-10-17 (78fffc3, 8189e5c); volumes joined the checkpoint
on 2026-10-16 (ADR 0016); a restore carries the pod's security groups and
name tables since 2026-11-07 (3335b36, ADR 0035). Hibernation and restore
have run on the nodes since 2026-10-17.
