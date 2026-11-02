# FeOS

[![REUSE status](https://api.reuse.software/badge/github.com/gardenlinux/FeOS)](https://api.reuse.software/info/github.com/gardenlinux/FeOS)
[![GitHub License](https://img.shields.io/static/v1?label=License&message=Apache-2.0&color=blue)](LICENSE)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](https://makeapullrequest.com)

FeOS runs untrusted code of many tenants in **pods**: Firecracker microVMs
with containers in them, which cost no memory while they are idle and are
back in under a second, on the same node or another one. On top of it,
**FeRun**, a replicated control plane, offers two products:

- **Agent sandboxes**: workspaces in which an LLM agent runs commands and
  tools for a user, often for days, with exec, file tools, file watches,
  ports and credentials it can use but never sees.
- **Container as a service** (CaaS): customers run their own images as
  services, jobs and addons in environments, with deployments, routes and
  scale to zero, without operating a cluster.

FeOS is written in Rust, statically linked against musl; FeRun in Go.

> **Status:** FeOS 0.6.1 is in development and changes quickly. The node
> layer and the agent sandboxes run; CaaS is partly built. What is built and
> what is not is in [docs/overview.md](docs/overview.md)
> and in the [ADR index](docs/adr/README.md#index).

## How it fits together

```
     clients: agents (MCP), apps (REST), users (HTTP to their ports)
                                  │
  ┌───────────────────────────────┴───────────────────────────────┐
  │ control plane: FeRun, ferund in replicas over PostgreSQL      │
  │ tenants, sandboxes, environments, placement, idle policies    │
  └───────────────────────────────┬───────────────────────────────┘
                                  │ feos.* gRPC APIs over mTLS
                                  │ (the node connects out)
  ┌───────────────────────────────┴───────────────────────────────┐
  │ node: FeOS                                                    │
  │  feosd ── jailer + Firecracker ── pod (microVM)               │
  │   │                                └─ feos-guest (PID 1)      │
  │  diskd: the pods' disks, tiered to S3  └─ containers          │
  └───────────────────────────────────────────────────────────────┘
```

- A pod can be suspended to a snapshot and resumed in place, or hibernated
  into object storage and restored on another node.
- Every pod gets its own network: an IPv6 address, a link-local IPv4
  address for leaving through NAT, DNS from `feosd`, security groups in
  nftables, and records of its traffic.
- Nodes open no port for control: they connect out to the control plane.

## What is in this repository

| Part | Where | What |
|---|---|---|
| `feosd` | [`crates/feos`](crates/feos) | the node agent: runs pods in Firecracker's jailer, their snapshots, network and volumes, and serves the `feos.*` APIs |
| `feos-guest` | [`crates/feos`](crates/feos) (guest profile) | PID 1 in every pod: runs its containers with libcontainer, serves the guest's API over vsock |
| `diskd` | [`crates/diskd`](crates/diskd) | the disk daemon: a pod's disk as a ublk device over sealed layers, cached locally and tiered to S3 |
| `feos-host-init` | [`crates/feos-host-init`](crates/feos-host-init) | PID 1 of a node whose operating system is FeOS |
| `feosctl` | [`crates/feosctl`](crates/feosctl) | the command-line client of a node |
| `ferund` | [`go/`](go/) | FeRun, the control plane: REST, MCP, the nodes' connections, placement, environments and deployments |
| `feos-k8s` | [`go/`](go/) | joins and drains FeOS nodes that run on Kubernetes |
| management UI | [`web/`](web/) | FeRun's UI (React), served by `ferund` |
| FeChat | [`fechat/`](fechat/) | a reference chat client of FeRun: an LLM with tools in a sandbox per conversation |
| APIs | [`proto/`](proto/) | the gRPC APIs, `feos.<api>.v1`: the contract between node and control plane |
| deployment | [`deploy/`](deploy/) | manifests for running FeOS and FeRun on k3s or a Gardener shoot |

## Getting started

FeOS needs Linux with KVM on x86_64: bare metal, or a VM with nested
virtualization.

- **Build and test** the node layer: the pinned Rust toolchain
  (`rust-toolchain.toml`) and `protoc`, then
  `make fmt-check clippy test-unit`. FeRun needs Go and, for its tests,
  Docker: `make go-lint go-test`. [docs/development.md](docs/development.md)
  has the layout, all commands and the rules for code.
- **Install a node** on Debian 13 or Garden Linux from the apt repository
  at https://apt.fecode.dev: see
  [docs/architecture/node.md](docs/architecture/node.md#packages).
- **Run FeOS as the node's operating system** in a VM:
  [docs/guides/test-environment.md](docs/guides/test-environment.md) and
  [docs/guides/boot-uki.md](docs/guides/boot-uki.md).
- **Run the whole platform** on Kubernetes: [deploy/](deploy/).

## Documentation

| You want to know | Read |
|---|---|
| what FeOS is for and where it stands | [docs/overview.md](docs/overview.md) |
| how the node, the guest, the disks and the control plane work | [docs/architecture/](docs/architecture/) |
| why it is built this way | [docs/adr/](docs/adr/) |
| how to build, test and contribute | [docs/development.md](docs/development.md) |
| how to build images and packages | [hack/README.md](hack/README.md) |

## Contributing

Issues and pull requests are welcome. Please read
[docs/development.md](docs/development.md) first: it has the rules the code
follows (static musl, syscalls first, a PID 1 that does not die) and the
checks to run before a commit. Larger changes start with an ADR in
[docs/adr/](docs/adr/). [CLAUDE.md](CLAUDE.md) gives the same rules in
short to coding agents.

## Licensing

Copyright 2025 SAP SE or an SAP affiliate company and IronCore contributors. Please see our [LICENSE](LICENSE) for
copyright and license information. Detailed information including third-party components and their licensing/copyright
information is available [via the REUSE tool](https://api.reuse.software/info/github.com/gardenlinux/FeOS).

<p align="center"><img alt="Bundesministerium für Wirtschaft und Energie (BMWE)-EU funding logo" src="https://apeirora.eu/assets/img/BMWK-EU.png" width="400"/></p>
