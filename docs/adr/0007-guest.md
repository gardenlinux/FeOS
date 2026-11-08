# ADR 0007: feos-guest, the pod's PID 1, and its API

**Status:** Accepted · **Date:** 2026-10-13

`feos-guest` is PID 1 of a pod's microVM. It serves its API on vsock and a
Unix socket, never on TCP, dials the host once its listeners are up and
holds that call open as its readiness, and is configured by the host: the
wall time and the network are pushed to it after every boot and every
resume, and it runs no DHCP and no NTP. Its API is FeOS's own packages,
internal between `feosd` and `feos-guest`, and it changes only by
addition. Containers run with libcontainer in a process of its own, their
records are files, and the pod's credentials never enter the guest.

## Context

A guest in a Firecracker microVM has no console a person reads and no
network until something configures it. A PID 1 that discovers its
configuration from the network (DHCP, NTP) is wrong after a restore until
its next lease or sync, and needs a network to be configured at all. A
PID 1 that runs the container runtime in its own process dies with a
runtime failure.

A suspended pod keeps the `feos-guest` it was suspended with in its
memory, for as long as it is suspended: a newer `feosd` talks to older
guests.

## Decision

### Transport and configuration

- The API listens on a vsock port and on a Unix socket in the guest, never
  on TCP.
- Once its listeners are up, `feos-guest` dials the host over vsock and
  holds `feos.lifecycle.v1.LifecycleService.Attach` open: readiness and
  liveness in one call. The socket a call comes in on names the VM to
  `feosd`; the guest never names itself.
- **Configuration is pushed, not discovered.** When a guest attaches,
  after a boot or when its call was lost, and after every resume, `feosd`
  pushes the wall time and the pod's network. The guest runs no DHCP and
  no NTP. Its clock follows the node's from then on (ADR 0027).
- The data volume, the virtio disk that is not the root, is the pod's disk
  (ADR 0011), mounted at `/var/lib/feos`: images, containers and logs live
  there.
- The pod's identity and credentials stay on the host, with the pod's
  `feos-authn`. The guest pipes the connections that need them to the
  host over vsock, with a preamble that says what each is for (ADR 0014).

### API

- The guest's API is FeOS's own packages: containers, images, tools,
  watches, port forwards, the credential listeners, and `feos.host.v1`,
  which is the same service in `feosd` and `feos-guest`: what the machine
  is and its power. In `feos-guest`, shutting down reboots, which ends the
  VM; powering off would halt the guest and leave Firecracker running.
- What only a guest does for its host gets a package of its own,
  `feos.guest.v1`, which only `feos-guest` serves.
- The guest's API is a contract between `feosd` and `feos-guest`
  (ADR 0004), and it only grows: a suspended pod keeps the `feos-guest` it
  was suspended with, so a newer `feosd` talks to older guests. The
  guest's APIs, `feos.lifecycle.v1` included, change only by addition
  within v1. No field is renumbered or removed, and no RPC changes its
  meaning.

### Runtime

- **libcontainer in a process of its own.** Containers run with youki's
  libcontainer, linked in, not with the youki binary, in the task service,
  a process PID 1 supervises and starts again when it exits. A runtime
  failure does not take down PID 1. The task service is the subreaper of
  the containers' processes.
- **Container records are files**, one in each container's directory, not
  rows in a database. A record lives and dies with its container's
  directory and survives a reboot with the writable layer that adoption
  takes over. The guest has no SQLite.
- **One HTTP client.** Image pulls, the web tool and the binary upgrade
  share one client, on rustls. The registry client checks the digest of
  every blob it pulls.
- `feos-guest` takes Ctrl-Alt-Del over: the kernel sends it a signal
  instead of rebooting at once, and it syncs and reboots (ADR 0005).

### Containers and images

- **Pulls are asynchronous**: a pull returns an image ID, and a watch
  reports when it is ready. Images are immutable and shared; a request
  marked `exclusive` gets an image of its own.
- **Creating a container returns once the container is created**, or
  fails with the reason. It pulls the image if needed. A create may adopt
  a container of the same ID from before a reboot, with its writable layer
  and logs.
- **Container IDs** are names of 1 to 64 letters, digits, `-`, `_` and
  `.`; the guest generates a UUID when a client gives none. A container's
  hostname is its ID.
- Deleting a container kills it if it runs. Container state is an
  enumeration, not a string.
- Exec runs a command in a running container without a terminal and
  without stdin; its output streams, and its exit code is the last frame.

## Alternatives considered

- **Keeping the sandbox system's API as it was**, under the FeOS name.
  FeOS would carry two APIs for containers and images; the port mapped its
  eleven services onto FeOS's packages instead.
- **The guest-only RPCs in `HostService`.** `feosd` would serve them, too:
  freezing a filesystem on a node would freeze the host's root, setting
  the network configuration would rewrite its network.
- **DHCP and NTP in the guest**, as FeOS's PID 1 did before pods. A
  restored guest's clock and network are wrong until the next lease or
  sync; a push after every resume fixes them at once, and the guest needs
  no network to be configured.
- **libcontainer in PID 1.** Simpler, but a runtime failure, or a fork
  from a multithreaded process gone wrong, would hit PID 1.
- **sqlite for container records.** It would cost the guest 18 crates and
  SQLite's C code, and a database that has to stay consistent with the
  containers' directories across freezes, snapshots and reboots.
- **The API on TCP**, as FeOS's PID 1 served it before pods. vsock needs
  no network to be configured and is reachable from the host alone.

## Consequences

- `feosd` is the only client of the guest's API, and the guest has no
  network of its own to be configured over: a pod without a network boots
  and serves all the same.
- An older guest in a suspended pod answers a newer `feosd`'s calls it
  does not know with `UNIMPLEMENTED`, which `feosd` handles (ADR 0027).
- Exec has neither a terminal nor stdin: an interactive shell in a sandbox
  needs a stream the API does not have yet.

## History

Decided on 2026-10-13, when the sandbox system's guest was ported onto
FeOS's PID 1 and its eleven services onto FeOS's packages. The pod's
credentials moved from the guest's own listeners to `feos-authn` on
2026-10-15 (4c7f609, ADR 0014). `feos.guest.v1` began with the volumes'
mounting on 2026-10-16 (446fd90, ADR 0016); the time, network, freeze and
`PreparePause` calls that `HostService` carried before it are still there,
listed in the backlog.
