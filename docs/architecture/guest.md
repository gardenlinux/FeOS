The Guest
=========

What runs inside a pod's microVM. The decisions behind it:
[ADR 0007](../adr/0007-guest.md), [ADR 0016](../adr/0016-volumes.md)
(volumes). The guest is `feos-guest`, `crates/feos` built with the
`guest` feature.

## Boot

The guest is a kernel and an initramfs. The kernel starts from
Firecracker's minimal microVM config: virtio-mmio devices, balloon and
vsock, IPv6 and IPv4, no modules. The initramfs holds the guest as `/init`,
`/sbin/mke2fs`, which formats the pod's new volumes, static from
e2fsprogs built with musl, and a static uutils coreutils, the toolbox for
containers. Kernel, Firecracker, e2fsprogs and coreutils downloads are
pinned by sha256 (`hack/guest/`).

As PID 1 the guest:

1. moves its root off the initramfs and mounts `/proc`, `/sys`, `/dev`,
   `/dev/pts`, `/run`, a tmpfs at `/var/lib/feos` and the cgroup
   filesystem;
2. mounts the data volume, the first virtio disk in the order of the
   microVM's drives that is not the root, at `/var/lib/feos`, if the VM
   has one;
3. sets `vm.overcommit_memory` to 1, so that software that reserves more
   address space than it touches starts in a small VM;
4. takes Ctrl-Alt-Del over: a SIGINT makes it sync and reboot;
5. starts the task service (`--task-service`) and binds its listeners;
6. dials the host on vsock port 10001 and holds
   `LifecycleService.Attach` open.

It runs no DHCP and no NTP: the host pushes the time and the network when
the guest attaches and after every resume, and the network again when
`feosd` finds a running pod's guest configured for an earlier network
([node.md](node.md)). While the guest is attached, `feosd` measures its
clock once a minute and sends the offset to `AdjustTime`
([ADR 0027](../adr/0027-the-guests-clock.md)): over 100 ms the guest
steps its clock (`adjtimex` `ADJ_SETOFFSET`), otherwise it slews it
(`ADJ_OFFSET_SINGLESHOT`, 0.5 ms/s) and, at least 16 s after its last
adjustment, corrects its frequency (`ADJ_FREQUENCY`) by half of the rate
it drifted at. It remembers only when it last adjusted; `SetTime` starts
that again.

## Network

A pod has both families ([ADR 0010](../adr/0010-pod-network.md)):
`feosd` pushes `eth0` an IPv6 address of the node's pod prefix as a /128
with `fe80::1` as its gateway, an IPv4 address of `169.254.16.0/20` as a
/32 with `169.254.0.1` as its gateway, and `feosd`'s resolver,
`169.254.0.53`. The IPv6 address is the pod's on the network; the IPv4
one is for leaving, and the node masquerades it.

The guest turns router advertisements and address autoconfiguration off
before the interface goes up, and adds the IPv6 address without
duplicate address detection, since the node is the link's only other
end. The IPv4 gateway lies outside the /32, and the kernel takes no
route via a gateway it has no route to: the guest routes `169.254.0.1`
on the link first (`169.254.0.1 dev eth0 scope link`), then everything
via it. IPv6 routes everything via `fe80::1`, which the link-local
prefix holds. Any gateway outside its address's prefix, and for IPv6
outside the link-local one, gets such a route of its own.

A push may carry routes (`routes`: a prefix, a gateway on the link and an
MTU, 0 for the interface's), which replace their family's default route:
a pod whose IPv6 address is a ULA is pushed `fc00::/7` via `fe80::1`, with
the mesh's MTU of 1420 on a mesh, and no default route over IPv6
([ADR 0017](../adr/0017-pod-mesh.md)). A route of a family the push has no
address of, a prefix with bits beyond its length, a gateway of another
family and an MTU below the family's minimum (1280 for IPv6, 68 for IPv4)
are refused.

A push leaves `eth0` with exactly its addresses and routes: the routes
the kernel makes for the addresses, the default routes or the pushed
routes, and the routes to the gateways. Addresses and routes of an earlier push go, as after a
resume on another node, and so do other routes through `eth0`; a family
whose address the push leaves empty goes with its routes. What is there
already stays, so the same push again changes nothing. Pushes take effect
one after the other.

Names resolve as the upstream resolvers answer them. Where a name has
addresses of both families, programs take IPv6, since the pod's IPv4
address is link-local, unless the pod has no route to the name's IPv6
addresses, as a pod with a ULA has none to global ones; IPv4 literals and
programs that speak only IPv4 leave over IPv4.

The pod's metadata is at `169.254.169.254`, port 80, a session token
first ([node.md](node.md#metadata)). Firecracker answers it in the
network device and takes its packets by their destination, so the
guest's default route through `169.254.0.1` reaches it: the guest needs
no route of its own for it, and runs nothing for it.

## Volumes

The pod's volumes are virtio disks after the data volume, in the order
of the pod's spec. `feosd` calls `GuestService.MountVolumes`
(`crates/feos/src/volumes.rs`, `feos_utils::filesystem::volume`) once the
guest attached and before it creates the containers, with each volume's
name, ID, drive, size and whether it is new. For each, the guest:

1. orders its virtio disks by the start of their MMIO windows, from
   `/proc/iomem` for the platform device under
   `/sys/block/vd*/device`, or else by their virtio devices' indexes, and
   takes the drive's disk; Firecracker answers no drive's ID;
2. checks that the disk has the volume's size;
3. reads its first 68 KiB: an ext4 superblock, valid, its checksum
   included, whose mount time or count say whether it was ever mounted,
   or another filesystem's signature, a partition table, zeroes or other
   data;
4. formats it with `mke2fs -t ext4 -b 4096 -m 0 -U <volume ID> -E
   lazy_itable_init=1,lazy_journal_init=1,nodiscard` only if the host
   says it is new; a filesystem of the volume's own, by its UUID, is
   formatted over then, since its confirmation was lost, and one
   mounted earlier in the same boot is unmounted first; another UUID's
   filesystem that was mounted, or another filesystem, fails the volume;
5. without the host saying it is new, mounts only the volume's own
   filesystem: a blank disk or another UUID fails it;
6. mounts it at `/run/feos/volumes/<name>`, ext4 with `noinit_itable`,
   `nosuid` and `nodev`.

A volume that fails is an answer, with its reason; the others are
mounted all the same. A call for a volume that is mounted already checks
it and leaves it. `mke2fs` runs as a child of PID 1, which the reaper
reaps.

## Listeners and vsock ports

| Port or path | What |
|---|---|
| vsock 10000, `/run/feos/api.sock` | the guest's API ([api.md](api.md)) |
| vsock 10001 (dialled) | the uplink to the host |
| vsock 10002 (dialled) | Kubernetes connections, piped to the host's proxy |
| vsock 10003 (dialled) | SSH agent connections, piped to the host's agent |
| `/run/feos/image.sock`, `/run/feos/task.sock` | internal |

## Directories

| Path | Holds |
|---|---|
| `/var/lib/feos/images/` | pulled images, a `metadata.json` each |
| `/var/lib/feos/containers/` | containers, a `container.json` and the writable layer each |
| `/var/lib/feos/upgrade/` | a downloaded binary before an upgrade |
| `/run/feos/volumes/<name>` | the pod's volumes, mounted |
| `/run/feos/containers/` | the runtime's state of running containers |
| `/run/feos/share/` | what every container sees read-only at `/run/feos` |
| `/usr/lib/feos-tools/` | the toolbox |

## Containers

- A container's root is an overlay over its image's rootfs, with a
  writable layer in its directory.
- Every container gets the toolbox read-only at `/feos-tools` and the
  share directory read-only at `/run/feos`: the pushed resolvers
  (`/run/feos/resolv.conf`) and, while SSH bindings exist, the SSH agent
  socket (`/run/feos/ssh-agent.sock`).
- A container's volume mounts (`ContainerConfig.volume_mounts`) are bind
  mounts of `/run/feos/volumes/<name>`, read-write or read-only,
  `nosuid` and `nodev`, after FeOS's own. A create, and an adoption,
  fails while a volume it mounts is not mounted.
- **The pod's interception CAs** (ADR 0028 §5), for a pod with intercept
  bindings: feosd pushes them with `GuestService.SetInterceptCas` before
  it creates a bring-up's containers, after a resume or a restore with
  memory, and when `SetPodHttpBindings` changes them. The guest writes
  them to `intercept-ca.pem` in the share directory, which every
  container sees read-only at `/run/feos/intercept-ca.pem`. A container
  created or adopted while the guest holds CAs, init, sidecar or main,
  gets in its directory `intercept/system.pem`, a copy of its image's
  system bundle, the first of `/etc/ssl/certs/ca-certificates.crt`,
  `/etc/pki/tls/certs/ca-bundle.crt` and `/etc/ssl/cert.pem` that its
  image has (read with its links resolved in the image's root), and
  `intercept/ca-bundle.pem`, that bundle and the CAs, bind-mounted
  read-only, `nosuid`, `nodev`, `noexec`, over every one of those paths
  the image has, or over `/etc/ssl/certs/ca-certificates.crt` when it has
  none (libcontainer makes the file); and its environment, where its
  `ContainerConfig.env` does not set them, `SSL_CERT_FILE` and
  `REQUESTS_CA_BUNDLE` naming the first of those paths and
  `NODE_EXTRA_CA_CERTS` naming `/run/feos/intercept-ca.pem`. Every exec
  takes the container's environment, so a sandbox's tool calls and an
  attached service's processes have them too. An exec in a container
  created before the guest held CAs, as a sandbox's are, gets
  `NODE_EXTRA_CA_CERTS` and, where its environment names none,
  `SSL_CERT_FILE` and `REQUESTS_CA_BUNDLE` naming
  `/run/feos/intercept/<id>.pem`, `intercept/<id>.pem` in the share
  directory: the container's system bundle as the exec finds it in its
  root filesystem (opened as a path and read only if it is a regular
  file, as a container may have put anything there) and the CAs, made
  anew whenever that bundle differs from `intercept/exec-system.pem` in
  the container's directory, where the exec keeps it. Nothing is
  mounted over the container's own bundle. A change rewrites the CA
  file and every mounted bundle in place, truncated and written, never
  renamed, so that the mounts see it, and replaces every bundle in the
  share directory, of the system bundle kept, by a rename; a process
  that read its store sees it after its restart. A container's delete
  removes its bundle in the share directory. Without CAs the file is empty and
  containers created then get nothing; a container adopted then keeps the
  variables it was created with. Containers of a pod without intercept
  bindings are untouched. Java's keystore is not covered.
- A container's hostname is its ID.
- The task service runs containers with libcontainer and is the subreaper
  of their processes. Exec joins a container's namespaces from the task
  service and streams through the container service; it has no terminal
  and no stdin yet.
- **Records are files.** Startup reconciles them: containers the runtime
  does not know are stopped, and interrupted creations are removed. A
  container survives a reboot of the guest with its writable layer, and a
  create with `adopt` takes it over.
- A container's exit code, 128 + the signal when it was killed, is in its
  record from when its process exits until it starts again or is
  adopted. `GetContainer` and `ListContainers` report it, and so feosd's
  `GetPod`; a container that did not survive a reboot has none.
- A container's stdout and stderr are FIFOs, `logs/stdout` and
  `logs/stderr` in its directory (ADR 0026). PID 1 reads them and writes
  each line to `logs/container.log` in the CRI log format (`<RFC 3339
  nano, UTC> <stdout|stderr> <F|P> <text>`), the time it read the line:
  a line over 64 KiB goes in parts (`P`), and one left unfinished for a
  second goes as a part too, which the next entry of its stream goes on
  with. The log's ID (`log_id`, 32 hex digits) is written beside it,
  `container.log.id`, before the log is made, and stays with the log;
  a container created anew under its name gets a new log and ID. An
  adoption keeps both; a log a crash left unfinished gets its newline
  first. stdout and stderr are read side by side: lines of the two
  streams written within the same moment may land in either order.
- The FIFOs are open for reading and writing in PID 1, and the task
  service holds them open too while the container is there: a container
  never writes into one without a reader, and gets no SIGPIPE whatever
  restarts. A task service that restarts lets go of its hold; PID 1
  still reads. A guest that upgrades itself execs, and reads the FIFOs
  of every container the runtime knows again when its container service
  starts: meanwhile the pipe holds what the container writes, and the
  container waits once it is full. Only the unfinished line PID 1 had
  read is lost then. A suspended or hibernated pod's readers are in its
  snapshot as they were; lines read right after a restore carry the
  guest's clock, which may lag until the node pushes the time. When a
  container's process exits, what its FIFOs still hold and its
  unfinished lines are written before its exit is recorded. A write to
  the log that fails is logged and the line dropped: the log never holds
  the container up.
- `StreamContainerLogs` serves the log: the last `tail_lines` lines (at
  most 10 000, and from no further back than 4 MiB before the end, found
  reading back from the end), or all of it with `tail`, then with
  `follow` what is appended, until the container is deleted or the
  caller ends the call; with `follow` and `from_offset` (and `log_id`)
  from an offset an entry named, or from 0 of the log there is when the
  offset is past its end or of another log. An entry is the line's text
  without the prefix, with its source, time, whether it is a part, its
  offset, the next offset and the log's ID. A line written before ADR
  0026, without the prefix, goes out as it is, without a time or a
  source, one over 64 KiB in parts. A tail counts a line of the format
  as one, and each part of a line of before as one; it cuts a line of
  before that reaches further back than the parts asked for, or than the
  4 MiB, so that its last parts are 64 KiB each (a line of which not one
  such part fits in the 4 MiB is left out), and leaves a line of the
  format out that starts beyond the 4 MiB. A follow sends a last line of
  before that stays unfinished for a second as it is. `feosctl container
  logs` prints a part and the rest of its line as one line.

## Power

`HostService.Shutdown` reboots, which ends the VM: Firecracker cannot
power a guest off, and a halted guest would leave Firecracker running.
