# ADR 0009: One feosd under systemd or feos-host-init, from signed packages per version

**Status:** Accepted · **Date:** 2026-10-15

A node runs Debian 13 or Garden Linux, with systemd running `feosd` and
`diskd`; FeOS as the node's operating system, with `feos-host-init` as
PID 1, runs the same `feosd`, which depends on nothing of the system it
runs under. The node's programs come as Debian packages from a signed apt
repository of our own: `feosd` (with `feos-authn`), `feosctl`, `diskd`
(with `diskctl`), and Firecracker and the guest in a package per version,
installed side by side so that a suspended pod always finds the version
it was written with. Under systemd a restart or an upgrade of `feosd`
stops `feosd` alone: the microVMs live in a delegated unit of their own
and the next `feosd` takes them over. `feosd` sets nothing of the machine
up; on a distribution that is the operating system's.

## Context

`feosd` (ADR 0003) runs on a node under one of two operating systems:

- FeOS itself: `feos-host-init` as PID 1, booted from a unified kernel
  image (UKI);
- a general-purpose distribution with systemd: Debian 13, or Garden Linux,
  which builds on Debian.

Whatever the operating system, `feosd` needs:

- pods that outlive a restart and an upgrade of `feosd` (ADR 0001);
- the Firecracker version a snapshot was written with (ADR 0008): an
  upgrade that replaces Firecracker leaves suspended pods that cannot be
  restored;
- no port open to receive work (ADR 0015).

From systemd 258 on, systemd starts a service's main process in the
service's cgroup and moves it into its subgroup only then, which the
kernel refuses while the cgroup has controllers on for its children: a
service whose cgroup holds running microVMs cannot start again.

On a Kubernetes cluster that replaces its nodes, nothing installed on a
node outlives it; there the same binaries run in a pod per node
(ADR 0020).

## Decision

### Debian and Garden Linux first

Nodes run Debian 13 or Garden Linux. FeOS as the node's operating system
stays a goal, and runs the same `feosd`: nothing in `feosd` depends on
which of them it runs under.

### Debian packages

The binaries are static musl (ADR 0002) and depend on no library of the
distribution, so one `.deb` serves Debian 13 and Garden Linux. nfpm
builds the packages, because Firecracker and the guest are no cargo
crates. They are published in a signed apt repository of our own.

The packages are `feosd`, with `feos-authn` beside it and their units;
`feosctl`; `diskd`, with `diskctl` and its unit (ADR 0011); and
Firecracker with its jailer and the guest with its kernel, initramfs and
blank data volume, each in a package named with its version and
installed in a directory of that version, behind an unversioned package
that depends on the newest.

- Firecracker and the guest are installed side by side, as kernel
  packages are: an upgrade adds the new version and leaves the old ones.
  `feosd` boots new pods with the newest versions it finds, records in
  each pod what it booted with, and reports which versions its pods and
  snapshots still use. A version nothing uses may be removed, by an
  operator; no automatic cleanup removes one.
- The disk daemon is one package, like `feosd`: the pods' disks outlive
  its restarts, so an upgrade replaces it in place, and `feosd` depends
  on it.
- `feos-authn` comes with `feosd`: each run of a pod starts it anew
  (ADR 0014), so there is no version of it to keep.

### feosd under systemd

- `feosd` tells systemd that it serves once its listeners are bound,
  with one datagram and no libsystemd.
- A restart or an upgrade stops `feosd` alone; the pods keep running, and
  the next `feosd` takes them over, as containerd's shims outlive
  containerd. The microVMs' cgroups are not below `feosd`'s unit but
  below a delegated unit of their own that runs no process, so that
  systemd never has to start a process in a cgroup that holds microVMs
  (ADR 0005).
- Listeners that must outlive `feosd`, the pods' resolver's and the HTTP
  bindings', are systemd's sockets and are handed to every `feosd`
  (ADR 0010, ADR 0028).
- `feosd` sets nothing of the machine up. Network, time and SR-IOV VFs are
  the operating system's: systemd-networkd, timesyncd or chrony, the
  kernel command line.
- The unit's state directory is on disk. That is what makes suspend
  free memory (ADR 0008).
- A reboot or a shutdown asked of `feosd` is a signal to PID 1, the one
  systemd and `feos-host-init` both answer. No D-Bus, no `systemctl`.
- `feosd` replaces its own binary only where `feos-host-init` runs it
  and says so at its start. On a distribution the package manager owns
  the binary, and the call is refused.

### FeOS as the node's operating system

The UKI's initramfs carries `feos-host-init`, `feosd`, `feos-authn`,
`diskd`, and Firecracker and the guest, laid out as the packages lay
them out. `feos-host-init` sets the machine up (ADR 0003), mounts a tmpfs
as the state directory, starts `diskd` and `feosd`, allowed to replace
their own binaries, and starts each again when it ends. It answers the
same signals as systemd, so `feosd` reboots and powers off the node the
same way under both.

## Alternatives considered

- **A DaemonSet that owns `feosd` on a node that stays**, as the sandbox
  system ran its manager. Kubernetes would own `feosd`'s life: a VMM
  started in a pod lives in its cgroup and PID namespace and dies with
  it, unless the pod runs privileged in the host's PID namespace and moves
  its VMs into cgroups outside its own. A systemd unit gets there with two
  settings. Where the cluster replaces its nodes, a pod per node is the
  decision all the same (ADR 0020).
- **One package with Firecracker and the guest in it.** An upgrade would
  replace the Firecracker that suspended pods need.
- **cargo-deb.** It packages one crate; Firecracker and the guest are none.
- **The microVMs' cgroups below `feosd`'s own unit.** Simplest, and
  what the unit did at first; from systemd 258 on, the unit does not
  start again while microVMs run below it.
- **systemd-sysext images**, for nodes with a read-only `/usr`. They ran
  on the Gardener shoot for one day; a replaced node loses the extension
  with the machine, and there FeOS runs as a pod per node (ADR 0020).
- **A Garden Linux feature that bakes the packages into the image.**
  Garden Linux nodes on Gardener are replaced, not updated, and run the
  pod per node (ADR 0020); nobody needs the feature.
- **`feosd` setting the node up**, as FeOS did as PID 1. On a distribution
  it would contend with its network and time services.

## Consequences

- A release is the packages in the apt repository and the UKI; a build of
  any commit can go to a node as a version below the release it leads to.
- A snapshot is restored only on a node that has its Firecracker and
  guest packages. Placement learns which a node has from its telemetry
  (ADR 0015).
- The control plane's `ferund` takes a package of the same kind, built
  and published beside the node's (ADR 0015).
- An image for a Kubernetes cluster that replaces its nodes is built from
  these packages (ADR 0020): one build serves both ways of running a node.

## History

Decided on 2026-10-15; the packages and the apt repository landed on
2026-10-14 (de4f353, 574d1c7), `diskd`'s package on 2026-10-15 (5c99f1a),
`feos-authn` in `feosd`'s on 2026-11-05 (bf1d67a). The microVMs' cgroups
moved out of `feosd`'s unit into a delegated one of their own on
2026-10-26 (7ea13ec),
when Garden Linux's systemd 259 refused to restart `feosd`. On the Gardener
shoot the packages ran as a systemd-sysext for one day, 2026-10-26
(c820288), before ADR 0020 put FeOS in a pod per node (50edd00,
2026-10-27). The ADR stayed Proposed until 2026-11-08, although the
packages had run on every node from 2026-10-16 on.
