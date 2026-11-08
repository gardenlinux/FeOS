# ADR 0005: Firecracker is the VMM, in its jailer

**Status:** Accepted · **Date:** 2026-10-13

Firecracker is FeOS's only VMM, behind a `Hypervisor` trait that stays as
the seam where another would plug in. Every microVM runs in Firecracker's
jailer, as a user of its own, in a cgroup of its own that outlives a
restart of `feosd`. `feosd` speaks Firecracker's API itself, without an
HTTP client library. What Firecracker cannot do, FeOS does not offer:
hotplug, firmware boot, PCI passthrough, live migration.

## Context

The pods of ADR 0001 need what Firecracker ships: snapshots of guest
memory, restore through an external page-fault handler, the jailer and
rate limiters. Keeping a second VMM beside it means two backends of which
one is used and the other goes stale. What cloud-hypervisor offers beyond
Firecracker (PCI passthrough, firmware boot of arbitrary disk images,
hotplug, live migration) has no user here (ADR 0001).

`feosd` runs as root. A VMM that runs as root without a jail hands a VMM
escape the node.

## Decision

### Firecracker only

- Firecracker is FeOS's only VMM. The `Hypervisor` trait stays as the seam
  where another VMM would plug in.
- **A VM is a Firecracker process per boot.** Firecracker can neither
  reboot a guest nor power it off; its process ends when the guest
  reboots, and starting the VM again starts a new process.
- **Devices are fixed at creation.** Firecracker cannot attach disks or
  network devices to a running VM, so the API offers no hotplug.
- **Shutting a VM down is Ctrl-Alt-Del.** The guest needs the i8042
  keyboard driver and has to reboot on Ctrl-Alt-Del; `feos-guest` does
  (ADR 0007).
- **FeOS speaks Firecracker's API itself**: a handful of JSON requests over
  HTTP/1.1 on a Unix socket, without an HTTP client library.

### Every VM in its jailer

- Firecracker runs in the jailer: in a chroot, the VM's directory, in a
  mount namespace of its own, as a user of the VM's own. What it opens by
  path is linked into the chroot; the VM's user owns its disks and TAP
  devices.
- Every VM has a cgroup of its own, below a cgroup `feosd` is given, never
  below `feosd`'s own process: under systemd a unit of its own that runs
  no process holds them, since a service whose cgroup has controllers on
  for its children cannot start again. VMs outlive a restart of `feosd`
  either way.
- VMs share the CPU in proportion to their vCPUs, as their vCPU threads
  would without a cgroup each. The cgroup accounts the VM's memory and
  caps neither CPU nor memory.

## Alternatives considered

- **Keeping cloud-hypervisor next to Firecracker.** It would keep PCI
  passthrough and firmware boot, for a use case without a user, at the
  price of a second backend to test and maintain. FeOS ran VMs with
  cloud-hypervisor before this decision; that line stays on the branch
  `release-0.5`.
- **Keeping the hotplug RPCs** and emulating them with placeholder drives
  that are swapped with `PATCH /drives`. Nothing needs it; it can come
  without changing the API.
- **Firecracker as root without the jailer.** A VMM escape would own the
  node.
- **A memory cap per VM** (`memory.max` of the guest's memory and an
  allowance). A suspend is charged to the VM: with its snapshot in a tmpfs,
  which the kernel cannot reclaim without swap, the cgroup of a VM with
  256 MiB reached 519 MiB. A cap would kill the VM in the middle of a
  suspend, or when its guest writes to a sparse disk.
- **The VMs' cgroups below `feosd`'s service.** From systemd 258 on, a
  service whose cgroup has controllers on for its children does not start
  again until they are gone; `feosd` could not restart under its VMs.

## Consequences

- No PCI passthrough (GPUs, NVMe and SR-IOV VFs into VMs), no firmware
  boot, so no operating systems other than Linux, no hotplug, no live
  migration, no huge pages.
- The microVM layer hands TAP devices to the microVM's user by changing
  their owner.
- A snapshot is loaded only by the Firecracker version that wrote it, so
  nodes keep the versions their snapshots need, side by side (ADR 0008,
  ADR 0009).
- Caps per VM, for VMs that overcommit the node, need a way to ask for
  them, storage below the snapshots' directory for a memory cap, and a
  kernel with CPU bandwidth control for a CPU cap, which FeOS's kernel
  configuration leaves out.

## History

Decided on 2026-10-13, when the cloud-hypervisor adapter went and 0.6.0
started the new line; FeOS as it was stays on `release-0.5`. The microVM
layer was trimmed to what pods use on 2026-10-14 (c5853fb, ADR 0004). The
VMs' cgroups moved out of `feosd`'s service into a unit of their own on
2026-10-26 (7ea13ec).
