# ADR 0020: FeOS on Kubernetes: a pod per node whose microVMs end with it

**Status:** Accepted · **Date:** 2026-10-26

Where a Kubernetes cluster replaces its nodes, FeOS is Kubernetes
workload, not host services: a DaemonSet runs one privileged pod per
worker with `diskd` and `feosd`, the same binaries as the packages, and
nothing is installed on the host. The microVMs live in `feosd`'s
container and end with it; a planned stop suspends every running pod into
its directory and the next `feosd` resumes them, and a crash loses them
as a lost node would. A controller, `feos-k8s`, joins new workers to
FeRun, holds a cordoned worker's drain until FeRun has moved its
sandboxes away, and removes workers that are gone. A cluster whose nodes
stay runs the packages under systemd (ADR 0009); a DaemonSet may install
them there, but does not run them.

## Context

A Gardener shoot's nodes do not stay: an update of the operating system
or of Kubernetes, and every maintenance window, drains a worker, deletes
its machine and creates a new one under a new name. FeOS installed on the
host (ADR 0009) goes with the machine, and three gaps show:

- **A replaced node takes its sandboxes with it.** The machine controller
  drains a node by evicting its pods and skips DaemonSet pods; kubelet
  has no graceful node shutdown. Nobody tells FeRun, so the sandboxes on
  the node are not hibernated or moved: their microVMs end when the
  machine is deleted.
- **A new node does not join.** A node joins FeRun with a join token for
  its name, which an operator makes. A replacement worker has a new name
  and no token.
- **Kubernetes does not know the microVMs.** Their memory is outside every
  pod, so the scheduler places other workloads on memory the microVMs
  use, and kubelet's eviction does not see them.

A host that the cluster does not know also needs an installer for every
change, and Garden Linux's read-only `/usr` takes packages only as a
system extension.

## Decision

On a Kubernetes cluster that replaces its nodes, FeOS is Kubernetes
workload, not host services. The packages for Debian (ADR 0009) and
`feos-host-init` stay as they are, and a cluster whose nodes stay runs
the packages under systemd; a DaemonSet may install them on such a node,
but `feosd` and `diskd` are the host's services there, and their pods
outlive the DaemonSet.

### A pod per node

- A DaemonSet, `feos-node`, runs one pod per FeOS worker, with `diskd` and
  `feosd` from one image built from the packages: the same binaries.
  Nothing is installed on the host: no extension, no unit, no file in
  `/etc`. Configuration comes from a ConfigMap and Secrets.
- The pod is privileged and on the host's network (TAP devices, nftables,
  the mesh's WireGuard device, the resolver's address), with the host's
  `/dev` (KVM, TUN, ublk) and `/var/lib/feos` on the host for what
  outlives the pod on the node: the microVMs' directories, snapshots,
  disks, the node's identity, and every Firecracker and guest version an
  image has brought to the node, kept side by side as the packages keep
  them (ADR 0009), so that a suspended pod finds the version it was
  written with.
- The pod requests the memory and CPU that the node gives microVMs
  (FeRun's capacity of the node), plus `feosd`'s and `diskd`'s own, and
  has no limits. The scheduler keeps that memory free of other pods, and
  kubelet's eviction counts the microVMs against the request. Its
  priority class is `system-node-critical`.
- `diskd` starts before `feosd` and stops after it: a suspend writes
  through the pods' disks.

### MicroVMs live in the pod

- `feosd` puts each microVM into a cgroup `vms/<id>` below its container's
  cgroup, as below the root under `feos-host-init` (ADR 0005). The
  microVMs belong to the container: when it ends, they end.
- **A stop is planned** (a rollout, a deletion of the pod, `SIGTERM`):
  `feosd` tells FeRun that it stops, lets the calls in flight end,
  suspends every running pod into its directory (ADR 0008), and exits.
  The next `feosd` on the node resumes the pods it suspended for the stop
  before it connects to FeRun, which so never sees them suspended. The
  pod's termination grace covers the suspend of the node's memory at the
  disk's speed.
- **A crash is not** (`SIGKILL`, the OOM killer, a panic): its microVMs
  end. FeRun treats the sandboxes as on a lost node: back from their last
  hibernation, or lost. That is the price of microVMs that Kubernetes
  knows and ends with their pod; a restart that keeps them would need them
  outside the pod, where neither Kubernetes nor systemd lets them be.

### Nodes come and go with Kubernetes

A small controller, `feos-k8s`, runs as a Deployment of one replica beside
`ferund` and speaks to the Kubernetes API and to FeRun's admin API:

- **Join.** For a node that runs `feos-node` and has not joined, it makes a
  join token for the node's name, once, and puts it into the Secret the pod
  reads. A replaced worker joins without an operator.
- **Drain.** For every FeOS node it keeps a guard pod, bound to the node,
  under a PodDisruptionBudget that allows no disruption. When the machine
  controller (or `kubectl drain`, or the cluster autoscaler) cordons the
  node, the eviction of the guard is refused and retried; the controller
  drains the node in FeRun, which hibernates its sandboxes into the object
  store and restores them on other nodes. Once the node is drained, the
  controller deletes the guard, the drain goes on, and the machine goes.
  An uncordon undrains it.
- **Removal.** When a Kubernetes node is gone, the controller removes the
  FeOS node from FeRun.

The controller is Go in `go/`, with the standard library only: the
Kubernetes API is JSON over HTTPS with the service account's token.

## Alternatives considered

- **systemd on the host**, the packages laid over Garden Linux's `/usr` as
  a system extension by a DaemonSet's init container, with the microVMs'
  cgroups in `feos-vms.service` (ADR 0009). It ran on the shoot for one
  day: the microVMs outlive a crash of `feosd`, but the node stays a host
  the cluster does not know, replaced nodes lose their sandboxes, and
  every host change needs an installer. The cgroup fix stays in the
  Debian packages.
- **MicroVMs outside the pod** (moved out of the container's cgroup): they
  would outlive a restart of the container, but in a cgroup that neither
  kubelet nor systemd owns, and kubelet would not count their memory.
- **A Kubernetes pod per microVM** (as Kata Containers or KubeVirt do):
  the scheduler would place sandboxes, which is FeRun's job (ADR 0015).
- **Graceful node shutdown** in kubelet: the machine controller drains
  first and deletes the machine after, and DaemonSet pods are not part of
  the drain; nor would a shutdown wait for hibernations of minutes.
- **A pod per node on every cluster**, the k3s nodes too. Nodes that stay
  lose nothing by running the packages, and a `feosd` under systemd
  restarts without a pause for its pods; the pod per node pays that pause
  for nodes that would lose the pods anyway.

## Consequences

- A rollout of `feos-node` suspends and resumes every pod on the node,
  one node at a time: a pause of seconds to minutes per sandbox instead of
  none. A crash of `feosd` ends the node's microVMs.
- A node update or a maintenance window moves sandboxes to other nodes
  before the worker goes; it needs an object store, and room on the other
  workers, or the drain waits until the machine controller's drain
  timeout.
- A new worker joins on its own, and a worker that is gone leaves FeRun
  on its own.
- Moving a running node from the packages to the pod is a drain in FeRun,
  the switch, and an undrain, one node at a time.
- FeRun waits for a node that restarts long enough for a suspend and a
  resume, so that calls wait rather than fail (ADR 0015).

## History

Decided on 2026-10-26 (9e7acb8) after one day of FeOS as a systemd-sysext
on the Gardener shoot (c820288), whose nodes Gardener replaces.
`feos-k8s` landed on 2026-10-26 (674d11d) and the pod per worker on
2026-10-27 (50edd00), which removed the sysext installer. The calls in
flight were let end before the suspend on 2026-10-29 (6f09970), and the
node's store of every Firecracker and guest version an image brought
came on 2026-11-02 (261acd5). The k3s deployment kept the packages under
systemd, installed by a DaemonSet (b2e2cb8, 2026-10-16; f4e4191,
2026-10-27).
