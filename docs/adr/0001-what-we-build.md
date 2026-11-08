# ADR 0001: What we build: two products on one platform of isolated pods

**Status:** Accepted · **Date:** 2026-10-14

Agent sandboxes and a container as a service (CaaS) are two products on
one platform. Both run untrusted code of many tenants, so both run on the
same unit, the pod: a Firecracker microVM with `feos-guest` and one or
more containers, which can be suspended to a snapshot and resumed, in
place or on another node. FeOS is everything that runs on a node and
speaks of pods, containers and images; what a sandbox or a service is
lives in the control plane, which runs centrally and replicated and
reaches nodes only over the connections the nodes open to it. CaaS has
an API of its own, shaped after what Kubernetes does for a pod, but it is
not a Kubernetes. Node and control plane live in one repository.

## Context

We offer two things:

- **Agent sandboxes.** An LLM agent runs commands and tools in a workspace
  for a user, often for days. The work is bursty: seconds of commands,
  tens of seconds of thinking, then minutes to hours of waiting for the
  user. The dominant resource of an idle sandbox is memory. Nodes get
  drained, so a sandbox has to resume elsewhere.
- **A container as a service.** Customers run their own container images
  as services without operating a cluster. Many services are idle most
  of the time, too.

Both run untrusted code of many tenants in containers, and both need a
stronger boundary between tenants than a shared kernel gives. Both are
idle most of the time, so what an idle workload costs shapes the platform
as much as isolation does.

## Decision

### Two products on one platform

Agent sandboxes and CaaS are two products on one platform. What the
products are, sandboxes with tools and tokens, services with replicas and
routes, lives in the control plane. The nodes do the same for both.

### CaaS has an API of its own

CaaS is a product of its own, not a Kubernetes. Its API follows what
Kubernetes does for a pod, its containers, restarts and probes, so that
images and users find what they know. There is no CRI adapter that lets
kubelet run pods on FeOS nodes, and no virtual kubelet that makes FeOS a
node of someone's cluster.

### The pod is the unit

A node runs **pods**: a Firecracker microVM that runs `feos-guest` and one
or more containers (ADR 0004). The VM is the boundary between tenants, the
container the boundary around the workload. An isolated container is a
pod with one container. A pod can be suspended to a snapshot and resumed
(ADR 0008).

### FeOS is the node layer

FeOS is everything that runs on a node: the node agent, the guest, PID 1
where FeOS is the node's operating system, the disk daemon, the process
beside each pod that holds its credentials, and the client (ADR 0003).
FeOS's APIs speak of pods, containers and images, not of sandboxes or
services.

### The control plane is central

Tenants, tokens, the registry of sandboxes and services, placement, the
policies that pause, hibernate and scale to zero, and the front doors (a
REST API, MCP, HTTP to the ports of pods) run centrally and replicated,
in programs of their own (ADR 0015; the ingress, ADR 0021; the sink,
ADR 0026). The control plane reaches nodes only over the connections the
nodes open to it.

### One repository

Node and control plane are coupled through the `feos.*` APIs: a feature
usually changes both sides at once. They live in one repository
(ADR 0002).

### Requirements

1. **Isolation.** The VM is the boundary between tenants. No control
   surface is reachable without authentication; a pod reaches only what
   is declared for it (ADR 0012); no credential enters a pod (ADR 0014).
2. **Cheap suspend.** Suspending is frequent: it writes what changed, not
   the whole memory.
3. **Fast resume.** Under a second to the first instruction. Memory and
   disk may follow lazily behind the running guest.
4. **Memory back on suspend.** A suspended pod holds no guest memory.
5. **Resume on another node**, through object storage, without fetching
   the whole memory or disk first.
6. **Pods outlive their node agent.** Restarting or upgrading `feosd` does
   not stop pods.
7. **Nodes open no port for control.** They connect out.

### Non-goals

- Arbitrary VMs: no other guest operating systems, no firmware boot.
- PCI passthrough: no GPUs or NVMe in pods, for now.
- Confidential computing (TDX, SEV-SNP).
- Point-in-time recovery: a snapshot is the latest state, not a history.

## Alternatives considered

- **FeOS as an operating system only, the platform in a repository of its
  own** that takes released FeOS artifacts. Every feature that crosses the
  API would become a change, a release and a version bump on two sides.
- **A runtime per product**: microVMs for sandboxes, containers on a shared
  kernel for CaaS. Two runtimes to build, and CaaS tenants are no more
  trusted than agents.
- **Kubernetes with a VM runtime** as the platform, a pod per sandbox. The
  sandbox system measured a constant 1 to 1.5 s added to every resume, and
  suspend and restore are not part of Kubernetes's model.
- **Kubernetes's API for CaaS**, through a CRI adapter or a virtual
  kubelet. Users would keep their tooling, but CaaS would take on
  Kubernetes's model and its releases, and suspend and scale to zero would
  sit beside them.
- **One program for the whole control plane**, front doors included. The
  ingress carries every application byte and the sink every log line, at
  a volume and with a lifetime of their own, so each is a program beside
  `ferund` (ADR 0021, ADR 0026).

## Consequences

- Decisions are written in these terms: node, pod, guest, control plane,
  sandbox and service.
- FeOS's APIs stay product-neutral. A capability one product needs, such
  as the agent's tools, is a capability of pods that the other product may
  leave unused.
- The products share placement, suspend and restore: an idle service
  costs what an idle sandbox costs, and both move between nodes the same
  way.
- CaaS brings what sandboxes alone would not need: inbound traffic
  (ADR 0021), private images (ADR 0022), persistent volumes (ADR 0016),
  restarts, probes and jobs (ADR 0004, ADR 0041, ADR 0048).

## History

Decided on 2026-10-14 (a1d6169), when the ADRs were started over from
what is built: two lines of work had met in this repository, FeOS, a Rust
PID 1 for bare-metal hypervisors, and an agent sandbox system of
Firecracker microVMs with a Go manager per node and a control plane of
its own. The control plane was "one central service" at first; the
ingress (ADR 0021, 2026-10-27) and the sink (ADR 0026, 2026-10-30)
became programs of their own beside `ferund`. The open questions on
inbound traffic, on what a service's pod needs and on suspend tiers were
answered by ADR 0021, ADR 0025, ADR 0016, ADR 0004, ADR 0018 and
ADR 0008, and closed on 2026-11-08.
