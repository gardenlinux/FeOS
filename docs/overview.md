FeOS Overview
=============

This repository builds two products on one platform: **agent sandboxes**
and a **container as a service** (CaaS). **FeOS** is everything that runs on
the nodes: it runs serverless pods, microVMs with containers in them that
cost no memory while idle and are back in under a second. A replicated
central service, the **control plane**, orchestrates the nodes for both
products. Node and control plane are coupled through their APIs, so they
live in one repository.

Why it is built this way: [ADR 0001](adr/0001-what-we-build.md).

## The products

**Agent sandboxes.** A sandbox is a workspace in which an LLM agent runs
commands and tools for a user, often for days. Its work is bursty: a
command runs for seconds, the model thinks for tens of seconds, then the
sandbox waits for the user, for minutes or hours. The agent is untrusted.
A sandbox needs exec and file tools, file watches, ports it can show to its
user, and credentials (Kubernetes, SSH) that it can use but never sees. An
idle sandbox should cost no memory, and it should come back in under a
second, on whichever node has room.

**Container as a service.** Customers run their own container images as
services, without operating a cluster. Their code is untrusted, too. A
service needs traffic from outside, restarts, health checks, persistent
storage and images from private registries, and a service nobody calls
should scale to zero.

## The pod

Both products run on one unit: the **pod**, a Firecracker microVM that runs
`feos-guest` and one or more containers in it.

- The microVM is the boundary between tenants; the container is the boundary
  around the workload inside it.
- A pod can be suspended to a snapshot and resumed: in place, and later on
  another node.
- FeOS knows pods. Whether a pod is an agent's sandbox or a customer's
  service only the control plane knows.
- What one product needs is a capability of every pod: exec and the tools,
  watches, port forwards, credential offloading, restarts and health
  checks. A sandbox's container is an idle one that the agent works in
  through exec and the tools; a service's runs its image's command
  ([ADR 0004](adr/0004-pods.md)).

## Two layers

```
     clients: agents (MCP), apps (REST), users (HTTP to their ports)
                                  │
  ┌───────────────────────────────┴───────────────────────────────┐
  │ control plane: FeRun, ferund in replicas                      │
  │ tenants, registry, placement, idle policies, front doors      │
  └───────────────────────────────┬───────────────────────────────┘
                                  │ feos.* APIs over mTLS
                                  │ (the node connects out)
  ┌───────────────────────────────┴───────────────────────────────┐
  │ node, FeOS                                                    │
  │                                                               │
  │  feos-host-init or systemd                                    │
  │   └─ feosd ──────── jailer + Firecracker ── pod (microVM)     │
  │       │                                      └─ feos-guest    │
  │       └─ /run/feos/api.sock (feosctl)            └─ containers│
  └───────────────────────────────────────────────────────────────┘
```

### FeOS, the node layer

| Program | Runs | Does |
|---|---|---|
| `feosd` | on every node, under systemd or `feos-host-init` | runs pods: Firecracker in its jailer, snapshots, the pods' network, each pod's `feos-authn`; serves the local API and the control plane's calls on the connections it opens to it |
| `feos-guest` | as PID 1 in every pod | runs the pod's containers with libcontainer, serves the guest's API to `feosd` over vsock; its time and network are pushed |
| `feos-host-init` | as PID 1 when FeOS is the node's operating system | sets the machine up and keeps `feosd` running; otherwise the node runs Debian or Garden Linux |
| `feosctl` | anywhere | the command-line client |

`diskd`, the disk daemon, runs beside `feosd` on every node: the pods'
disks, and their way into object storage (ADR 0011). `feos-authn`, the
process beside each pod that holds its identity and credentials
(ADR 0014), joins them when it is built.

### The control plane

One program, `ferund`, FeRun's daemon, in replicas over one database,
holds what is more than one node's business: tenants and their tokens,
the registry of sandboxes and services, placement on nodes, the policies
that pause, hibernate and scale to zero, and the front doors: a REST API,
MCP for agents, and HTTP to the ports of pods. It reaches nodes only over
the connections the nodes open to it, mTLS both ways: it calls the node's
API on one, the node reports telemetry and events on the other
(ADR 0015). It is written in Go, in `go/`, with its management UI in
`web/`; [architecture/control-plane.md](architecture/control-plane.md)
describes it. A second program, `ferun-ingress`, carries the traffic of
public environments' hosts: a peer on the pods' mesh that serves from
what `ferund` streams to it, so that no application byte crosses
`ferund` (ADR 0021, [architecture/ingress.md](architecture/ingress.md)).

### The contract

The `feos.*` gRPC APIs (`proto/feos/<api>/v1/`) are the contract between
the layers. `feosctl` and tests call them on the node's Unix socket; the
control plane calls the same APIs on the connection the node opens to it.
The guest's API is internal between `feosd` and `feos-guest`.

## Principles

- **Nodes open no port for control.** They connect out to the control plane.
- **No credential enters a pod.** What a workload may use, `feos-authn` on
  the host adds or signs with: its keys and tokens, and its own identity.
- **Pods outlive their node agent.** A restart or an upgrade of `feosd`
  leaves the pods running.
- **A snapshot pins what it was written on**: the Firecracker version, the
  CPU model and the host's kernel. Nodes keep the Firecracker versions
  their snapshots need.
- **PID 1 stays small.** `feos-host-init` and `feos-guest` must not die;
  work that can fail runs in processes that can be restarted.
- **Static musl, syscalls first** on the node: no shared libraries, no
  shelling out to userspace tools.

## Non-goals

- Arbitrary VMs: no other guest operating systems, no firmware boot.
- PCI passthrough: no GPUs or NVMe in pods, for now.
- Confidential computing (TDX, SEV-SNP).
- Point-in-time recovery: a snapshot is the latest state, not a history.

## Where it stands (2026-10-28)

| Part | State |
|---|---|
| `feos-guest` | runs containers, tools, watches, port forwarding and credential offloading in a Firecracker guest |
| `feosd` | runs pods (`feos.pod.v1`): a microVM in Firecracker's jailer and its containers, with snapshots, the balloon and the guest's uplink, and their persistent volumes (`feos.volume.v1`, ADR 0016), under systemd or `feos-host-init`; forwards a pod's container, image, tool, watch and port-forward calls to its guest; gives every pod its network (ADR 0010): an IPv6 address of the node's prefix and a link-local IPv4 address on a routed TAP device, with proxy ARP for the IPv4 gateway and masquerade for leaving over IPv4, DNS from its own resolver, which answers the names of the pods' name tables itself; enforces security groups with nftables (ADR 0012) and records flows, drops, DNS queries and server names (ADR 0013), served as `feos.network.v1`. Pods have 11-character IDs that ferund or `feosctl` mints, which name their TAP devices (ADR 0019); pods with UUIDs stay until they are deleted. On Kubernetes, `feosd` and `diskd` run in a pod per node, whose microVMs live in it and are suspended on a planned stop (ADR 0020). Credentials and proxies are not built; groups and records do not reach the control plane yet |
| `feos-host-init` | sets the machine up as PID 1 and runs `feosd` |
| `diskd` | the disk daemon (ADR 0011): ublk devices over sealed layers, a local cache and S3. Every pod has a disk from it, and hibernates into its object store: `HibernatePod` and `RestorePod`, here or on another node, with the pod's memory served lazily where the snapshot fits the node, and a boot on its disk where it does not (ADR 0008). Packaged beside `feosd`; no baked images, no sweep yet |
| packages | `.deb` packages of `feosd`, `feosctl`, `diskd`, Firecracker and the guest, in a signed apt repository at apt.fecode.dev; `ferund`'s is published beside them |
| control plane connections | `feosd --control-plane` (ADR 0015): joins with a one-time token, keeps its key and certificate and renews it, reports telemetry and its events (pods, start, stop) on the uplink, and serves its API on a node connection to every instance. The event buffer is in memory, not yet on disk; pods' credentials are not built |
| control plane | `ferund` (ADR 0015), in Go: tenants and their tokens, sandboxes on the nodes with placement, ops that survive their callers, pause, hibernation and restore elsewhere, drains, the agent's tools over REST and MCP, ports and file watches, Kubernetes and SSH bindings (off until feosd delivers them), the fleet of nodes with joins, certificates, telemetry and events, human sign-in with OIDC and the management UI; replicas over PostgreSQL with a leader. It runs on Kubernetes, on k3s or on a Gardener shoot, where `feos-k8s` joins new nodes and drains replaced ones in FeRun (ADR 0020, [deploy/](../deploy/)). Beside it `ferun-ingress` (ADR 0021): instances that join with a token, take a /96 of the pods' mesh, keep the table `ferund` streams to them through an outage, and proxy public environments' hosts over HTTPS to the pods, gated by the nodes' security groups and the mesh's handshakes, holding idle services awake and waking sleeping ones; two on the Gardener shoot behind a balancer of their own |
| CaaS | partly built in `ferund` (ADR 0018): environments with services, jobs, addons (`postgres`, `valkey`, `nats` on volumes), routes and sealed secrets; numbered deployments with a plan, compare-and-swap, revert and tags; image services rolled with a surge of one and scaled to zero, attached services in sandboxes of their environment (ADR 0035); names from the nodes' name tables, security groups, incidents, HTTP through `ferund`'s route door with route tokens, and for environments their tenant made public (`PUT …/public`) through `ferun-ingress` at a default host under the apps zone, with an operator's wildcard certificate; the REST API and a read-only view in the UI. Not built: volumes for services, MCP tools and a CLI for environments, data on a clone, addon backups, domains and their ACME certificates, the ingress's traffic records, L4 routes |

## Glossary

- **Node:** a machine that runs pods: bare metal, or a VM with nested
  virtualization.
- **Pod:** a microVM with `feos-guest` and its containers; the unit FeOS
  runs.
- **Sandbox:** the control plane's object for an agent's workspace. It has
  an identity across pauses and nodes; on a node, a pod is its instance.
- **Service:** the control plane's object for a customer's workload in
  CaaS; it runs as one or more pods.
- **Host / guest:** the node's side of a pod and the inside of its microVM.
- **Tenant:** whoever owns sandboxes or services; the unit of isolation and
  of authorization.

## Further reading

- [docs/adr/](adr/): the decisions and why they were made.
- [docs/architecture/](architecture/): how the node, the guest, the API
  and the control plane work.
- [docs/development.md](development.md): building, testing, the rules for
  code.
- [hack/README.md](../hack/README.md): kernel, images and test
  environments.
