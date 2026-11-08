# ADR 0003: The programs of node and control plane, and where they run

**Status:** Accepted · **Date:** 2026-10-14

Each program is a binary of its own, named after where it runs, and
binaries differ only where the code differs; where only the role differs,
one binary chooses at runtime. On a node run `feos-host-init` (PID 1 where
FeOS is the operating system), `feosd` (the node agent, never PID 1),
`diskd` (the disk daemon), `feos-guest` (PID 1 in a pod) and one
`feos-authn` per pod that has credentials; `feosctl` is the client. The
control plane is `ferund`, with `ferun-ingress`, `ferun-sink` and
`feos-k8s` beside it. No binary is called `feos`, and there is no manager
beside `feosd`: everything that concerns pods on a node is `feosd`'s,
everything that concerns sandboxes and services is the control plane's.

## Context

The platform's programs run in different places with different trust and
lifetimes. PID 1 must not die, whether of a node or of a pod. The guest
runs in every pod and must carry none of the node's code or dependencies.
The node agent is large and changes often; a fault in it or an upgrade of
it must not stop a pod. The control plane runs elsewhere, in replicas.
The node's programs run under FeOS as the node's operating system, under
systemd on a distribution (ADR 0009), or in a Kubernetes pod (ADR 0020),
and must do the same under each.

## Decision

### A binary of its own where the code differs

Each binary is named after where it runs. Where only the role differs,
the binary chooses at runtime.

- `feos-host-init`: PID 1 when FeOS is the node's operating system.
- `feosd`: the node agent, under `feos-host-init`, systemd or in a
  Kubernetes pod (ADR 0020), never PID 1.
- `feos-guest`: PID 1 in a pod's microVM (ADR 0007).
- `diskd`: one per node beside `feosd`, the pods' disks and their way
  into object storage (ADR 0011).
- `feos-authn`: one per running pod that has bindings, started and
  jailed by `feosd` (ADR 0014, ADR 0028).
- `feosctl`: the client, anywhere; `diskctl` is `diskd`'s client, for
  development, since `diskd`'s API is internal to the node.

- `feos-host-init` sets the machine up: mounts, the network, the clock,
  SR-IOV. It takes Ctrl-Alt-Del and the power signals over, reaps
  orphans, starts `diskd` and `feosd` and starts each again when it ends.
  It serves no API and logs to the kernel's log. It answers the same
  power signals as systemd, so `feosd` reboots and powers off the node
  the same way under both (ADR 0009).
- `feosd` and `feos-guest` are built from one crate, told apart by
  features that select the services and stay additive; the startup both
  share is the crate's library. The guest binary carries none of the
  node's code or dependencies.
- Both start their task service, the container runtime, by running
  themselves again, so the two processes are always of one version.
- `feosd` is never PID 1. A fault in it restarts `feosd`; under
  `feos-host-init` and systemd the pods keep running, and in a Kubernetes
  pod a planned stop suspends them for the next `feosd` (ADR 0020).
- No binary is called `feos`. FeOS is the project's name; it stays in the
  node's paths and in the proto packages.

### The control plane's programs

The control plane is Go, in `go/`. Each program is one because it has a
lifetime, a scale or a trust of its own; its ADR says why.

- `ferund`: the control plane, in replicas over one database; nodes
  connect to it (ADR 0015).
- `ferun-ingress`: public environments' hosts, as a peer of the pod
  mesh, so that no application byte crosses `ferund` (ADR 0021).
- `ferun-sink`: what the platform and its pods say, logs and traces
  (ADR 0026).
- `feos-k8s`: on Kubernetes, beside `ferund`; it joins, drains and
  removes FeOS nodes with the cluster (ADR 0020).
- `ferun-testnode`: a node in memory that speaks `feosd`'s side of the
  wire, for development.

### Pods are feosd's, sandboxes and services the control plane's

There is no manager beside `feosd`. `feosd` runs pods (ADR 0004), takes
the control plane's calls on the connections it opens (ADR 0015), gives
pods their network (ADR 0010), enforces what reaches them (ADR 0012),
runs their `feos-authn` processes (ADR 0014), and suspends, hibernates
and restores them (ADR 0008). The control plane holds the sandbox and
the service as objects, maps the agent's tools onto a pod's tool and exec
calls, and decides the policies that pause and hibernate. `feosd` reports
what they need and decides none of it.

## Alternatives considered

- **Two profiles of one binary named `feos`**, the host profile PID 1 of a
  bare-metal hypervisor, the guest profile PID 1 in a microVM, and the
  host profile also an ordinary process for development, told apart by
  `getpid() == 1` throughout its startup. Two files of one name did
  different things and needed a profile of their own (`release-guest`)
  not to overwrite each other; on a distribution the host profile carried
  the bare-metal setup (DHCPv6, raw sockets, NTP), and on bare metal
  PID 1 carried the whole node agent.
- **One binary for host and guest that selects its role at runtime.**
  Every pod would carry the node agent's code and dependencies (the VM
  service, SQLite): more to attack in every pod.
- **The task service as a binary of its own.** It would keep libcontainer
  out of the main process, but two files would have to be upgraded
  together and kept at one version; running itself again gives that for
  free.
- **A manager next to `feosd`**, the sandbox system's Go manager, speaking
  `feos.vm.v1` to `feosd` and the guest's API through the vsock path. Two
  programs on every node to version against each other, and the guest's
  API a public one.
- **The whole manager in `feosd`**, sandboxes included. `feosd` would know
  one of the two products, and CaaS would need its own semantics next to
  them in the node agent.

## Consequences

- The code that sets the machine up, and its dependencies for DHCP and
  NTP, live in `feos-host-init` alone; `feosd` sets nothing of the
  machine up.
- `feosd` is FeOS's largest program, and a fault in it costs no pod.
- A node's programs ship together and are of one version on a node
  (ADR 0009); the guest's version is pinned per pod with its snapshot
  (ADR 0008).
- A product's semantics never reach a node: a new kind of workload is a
  change in the control plane, not in `feosd`.

## History

Decided on 2026-10-14 (a1d6169), replacing the two profiles of one binary
`feos`. The sandbox system's Go manager, about 20,000 lines, was not
moved beside `feosd` but split along ADR 0001's line: its pod half was
ported into `feosd`'s crates, in Rust, its sandbox half into the control
plane. The port landed as pods on the microVM layer (f539c16,
2026-10-14), `diskd` and hibernation (87d3eae and 2aaced9, 2026-10-14 and
2026-10-15), the pods' network and resolver (38d5d18, 2026-10-15), the
control plane connections (634df80, 2026-10-16) and `feos-authn`
(5aa3bf0, 2026-11-05; shipped with `feosd` in bf1d67a). The control
plane's programs came with ADR 0015 (`ferund`), ADR 0020 (`feos-k8s`,
674d11d, 2026-10-26), ADR 0021 (`ferun-ingress`, 33ea94e, 2026-10-28) and
ADR 0026 (`ferun-sink`, a557b48, 2026-10-30).
