FeOS Developer Documentation
============================

See [overview.md](overview.md) for what FeOS is, [architecture/](architecture/)
for how it works, [/hack/README.md](/hack/README.md) for building and booting
FeOS images, and [/docs/adr](/docs/adr) for the decisions behind it.

## Repository layout

FeOS is a monorepo. Rust crates live under `crates/` in one Cargo workspace;
the control plane is Go, in `go/`, and its management UI JavaScript, in
`web/`.

```
crates/
  disk-store/           diskd's chunk store: chunks, layer manifests, seal, the
                        local cache, S3 tiering, the sweeper
  diskctl/              diskd's command-line client, for development
  diskd/                the disk daemon: pods' disks as ublk devices, memory
                        through userfaultfd
  feos/                 feosd and feos-guest: their startup, the wiring of
                        the services, the task supervisor
  feos-authn/           a pod's credentials on its node, one process per
                        pod: its HTTP bindings (ADR 0014, ADR 0028)
  feos-host-init/       PID 1 of a node whose operating system is FeOS:
                        machine setup (network, NTP, SR-IOV)
  feos-proto/           generated gRPC code for proto/
  feos-utils/           shared building blocks: logger, mounts, reaper, vsock,
                        resolvers, confined container file access, dispatch
  feosctl/              command-line client
  fleet-client/         feosd's side of the control plane: the join, the
                        node's certificate, the uplink, the node connections,
                        telemetry and events
  services/
    container-service/  containers: overlay roots, exec, adoption, persistence
    dns-service/        the pods' resolver, which feosd serves; it answers
                        the names of the pods' name tables itself
    host-service/       host information, logs, power, upgrade, time, network
                        push, filesystem freezing, memory
    image-service/      OCI image pulls and the image store
    network-service/    security groups: their store, SecurityGroupService,
                        and the compiler into sets of addresses and rules;
                        name tables: their store and NameService
    pod-service/        pods on top of the microVM layer, their network
                        (addresses, TAP devices, the nftables table, DNS,
                        records), and the forwarding of a pod's services to
                        its guest
    task-service/       the container runtime (libcontainer), its own process
    traffic-service/    traffic records: flows from conntrack, drops and
                        server names from nflog; TrafficService
    microvm-service/    the pods' microVMs, internal: Firecracker, jailed;
                        snapshots, balloon, the guest's uplink and pushes
    tool-service/       guest: agent file and web tools on a container's files
    watch-service/      guest: inotify subscriptions on a container's files
    forward-service/    guest: port forwarding, kube and SSH credential offloading
proto/feos/<api>/v1/    the gRPC APIs, package feos.<api>.v1
go/                     the Go module github.com/gardenlinux/feos/go: ferund,
                        ferun-ingress, ferun-testnode and feos-k8s (cmd/),
                        their packages (internal/), the generated code of
                        proto/ (gen/)
web/                    ferund's management UI (React, Vite), built into
                        go/internal/ui/dist
fechat/                 FeChat, FeRun's reference client, at chat.fecode.dev
deploy/                 example deployments of FeOS, FeRun and FeChat on
                        k3s and on Gardener
hack/                   kernel, initramfs, UKI, guest image and VM tooling
docs/                   documentation and ADRs
```

## Binaries

Each binary is named after where it runs ([ADR 0003](adr/0003-programs.md)):

- **`feosd`** (`crates/feos`, feature `host`, the default): the node agent,
  under systemd, `feos-host-init` or in a Kubernetes pod (ADR 0020), never
  PID 1. It runs pods and serves
  the public API on `/run/feos/api.sock`, and on TCP with `--tcp`. A pod's
  microVM runs in Firecracker, in its jailer
  ([ADR 0005](adr/0005-firecracker.md),
  [ADR 0008](adr/0008-suspend-and-restore.md)). With `--control-plane`, it
  joins the control plane and serves the same API on the node connections
  it opens to every instance (`crates/fleet-client`,
  [ADR 0015](adr/0015-control-plane.md)).
- **`feos-guest`** (`crates/feos`, `--no-default-features --features
  guest`): PID 1 in a microVM whose host configures it. Its API listens on
  vsock port 10000, not on TCP, and once it is ready it dials the host
  (vsock port 10001) and holds a `LifecycleService.Attach` call open. A data
  volume, if the microVM has one, holds images and containers; the pod's
  volumes are mounted for its containers, and a new one is formatted with
  the guest's static `mke2fs` ([ADR 0016](adr/0016-volumes.md)). It also serves
  the tool, watch, port forward, kube and SSH services for the agent in its
  containers. None of the node's code or dependencies are in this binary.
- **`feos-host-init`** (`crates/feos-host-init`): PID 1 when FeOS is the
  node's operating system. It sets the machine up and runs `feosd`.
- **`feos-authn`** (`crates/feos-authn`): a pod's credentials, one process
  per running pod that `feosd` starts with its microVM, jailed: its HTTP
  bindings at `169.254.169.1`, the credential added on the way out
  ([ADR 0014](adr/0014-identity-and-credentials.md),
  [ADR 0028](adr/0028-http-bindings.md),
  [architecture/authn.md](architecture/authn.md)). It ships in the
  `feosd` package, beside `feosd`.
- **`feosctl`** (`crates/feosctl`): the client. For development and the
  guest's tests it also has `container create --volume NAME:PATH[:ro]`
  with the command verbatim after `--`, `container logs [--tail N]
  [--follow]`, `guest mount-volumes`, the guest's internal call with
  which feosd mounts a pod's volumes before it creates the containers, and
  `guest set-intercept-cas`, with which it gives the guest the pod's
  interception CAs (ADR 0028).
- **`diskd`** (`crates/diskd`): the disk daemon, one per node next to
  `feosd`, as root. It serves pods' disks as ublk block devices over a
  content-addressed chunk store tiered to S3, and their memory through
  userfaultfd, on `<state-dir>/diskd.sock`
  ([ADR 0011](adr/0011-disk-daemon.md), [architecture/disk.md](architecture/disk.md)).
  Every pod's disk is diskd's: `feosd` is its client, and pods do not come
  up without it.
- **`diskctl`** (`crates/diskctl`): diskd's client, for development and
  the lab tests. diskd's API is internal to the node, so it is not part
  of `feosctl`.
- **`ferund`** (`go/cmd/ferund`): FeRun, the control plane, in replicas
  over one PostgreSQL; nodes connect to it
  ([ADR 0015](adr/0015-control-plane.md),
  [architecture/control-plane.md](architecture/control-plane.md)). It
  embeds the management UI of `web/`. Static, `CGO_ENABLED=0`.
- **`ferun-ingress`** (`go/cmd/ferun-ingress`): FeRun's ingress, which
  serves public environments' hosts straight to their pods as a peer on
  the pods' mesh, from the table `ferund` streams to it
  ([ADR 0021](adr/0021-ingress.md),
  [architecture/ingress.md](architecture/ingress.md)). It ships in
  `ferund`'s image as `/ferun-ingress`. Static, `CGO_ENABLED=0`.
- **`ferun-testnode`** (`go/cmd/ferun-testnode`): a node in memory that
  speaks feosd's side of the wire, for developing ferund and its UI
  without microVMs.
- **`feos-k8s`** (`go/cmd/feos-k8s`): the controller of FeOS nodes on
  Kubernetes: join tokens for new nodes, guard pods that hold a node's
  drain until FeRun has drained it, the removal of nodes that are gone
  ([ADR 0020](adr/0020-feos-on-kubernetes.md),
  [architecture/kubernetes.md](architecture/kubernetes.md)). Static,
  `CGO_ENABLED=0`.

`feosd` and `feos-guest` run the container runtime in a process of their
own, `--task-service`, and start it again when it exits. See
[ADR 0007](adr/0007-guest.md) for the guest's API.

Both serve the public API on `/run/feos/api.sock`. Features are additive: code
that only `feosd` needs sits behind `#[cfg(feature = "host")]`; code that only
the guest needs behind `#[cfg(feature = "guest")]`. With both features,
`crates/feos` builds both binaries and its library does both, which is only
useful for development.

## Building, linting, testing

You need the toolchain from `rust-toolchain.toml` (rustup installs it on first
use) and `protoc` with its well-known types (Debian: `protobuf-compiler`,
`libprotobuf-dev`); for the guest also `musl-gcc` and the kernel's headers
(Debian: `musl-tools`, `linux-libc-dev`), which build its `mke2fs`.

    make clippy          # lint feosd's and feos-guest's features, -D warnings
    make fmt-check
    make test-unit       # unit tests, no root needed
    make test            # all tests; the integration tests need root
    make release         # target/x86_64-unknown-linux-musl/release/{feosd,feos-authn,feos-host-init}
    make release-guest   # target/x86_64-unknown-linux-musl/release/feos-guest
    make cli             # feosctl

    make guest                 # guest kernel, initramfs and blank data volume:
                               # target/guest/
    make test-guest            # boot the guest under Firecracker and talk to it
    make test-guest-container  # run containers in the guest: exit codes,
                               # logs, postgres and valkey on volumes, a
                               # reboot that adopts them; needs sudo for a
                               # network namespace and NAT, and internet
                               # access for the images
    make test-pods             # pods against Firecracker, with the guest
                               # installed and a diskd; needs docker, ublk,
                               # S3 from the env or ~/.config/feos/diskd/s3.env
                               # for hibernation

    make diskd                 # static diskd and diskctl
    make diskd-lab             # diskd's lab tests: ublk, recovery, writes,
                               # flatten, S3; needs sudo and fio, S3 from the env

    make kernel                # the host kernel: target/kernel/;
                               # REBUILD_KERNEL=1 builds it again
    make initramfs             # FeOS as the node's operating system:
                               # target/initramfs.zst
    make test-host             # boot the host kernel in QEMU: the kernel's
                               # tests, then the node on QEMU's user network,
                               # PODS=1 a pod on it; needs /dev/kvm,
                               # qemu-system-x86_64, docker

Build release artifacts per package (`cargo build -p <crate>`), never with
`--workspace`: a workspace build unifies the features of all crates into every
binary.

Tests against the kernel need root and are ignored by default: those of
`feos_utils::nft` and of the pods' table and a pod's link in
`pod-service` (in a network namespace of their own, `unshare -n`, with
`nft` in `$NFT`), of
`traffic-service`, which make their namespaces, and of the guest's network
push. Their modules say how to run them, with `--ignored`. `make test-host`
runs those of the node in the host kernel.

## FeRun

You need the Go of `go/go.mod`'s `toolchain` line (`go` downloads it),
Node.js 22 and npm for the UI, and docker or a PostgreSQL 17 for the
tests.

    make go-lint         # gofmt and go vet
    make go-test         # every Go test, against a throwaway PostgreSQL 17 in
                         # docker, or the one FERUN_TEST_DATABASE_URL names
    make go-test-unit    # the Go tests; those that need a database skip
    make go-build        # static ferund, ferun-ingress, ferun-testnode and
                         # feos-k8s: target/go/
    make go-test-netns   # ferun-ingress's WireGuard test in network
                         # namespaces of its own; needs sudo -n
    make go-gen          # go/gen from proto/, after a change of a .proto
    make web             # the UI into go/internal/ui/dist (npm ci, build)
    make ferund-package  # ferund with the UI and its .deb: target/packages/ferund/
    make test-ferund-package  # installs it on Debian 13, in docker, and checks
                              # its user and its files' group

    cd web && npm test   # the UI's tests (vitest); they read ferund's API
                         # fixtures in go/internal/api/testdata/ferun, so a
                         # change there runs them too
    cd web && npm run dev  # the UI on Vite's server, /api, /auth and /mcp
                           # proxied to a ferund on localhost:8100

A single package's tests, with the race detector:

    export FERUN_TEST_DATABASE_URL='postgres://ferun:ferun@127.0.0.1:5432/postgres?sslmode=disable'
    cd go && go test -race ./internal/sandbox/...

The tests make a database of their own per test in that server and drop
it afterwards. The addon blueprints' tests against their real images run
only with docker and `FERUN_ADDON_DOCKER=1`:

    cd go && FERUN_ADDON_DOCKER=1 go test -run Docker ./internal/env/addons/

A ferund on one machine, with a test node:

    ferund init-ca --root-dir /tmp/ferun/root --dir /tmp/ferun/ca \
        --dns-domain feos.test
    head -c 32 /dev/urandom > /tmp/ferun/secret.key
    ferund serve --database-url "$DB" --ca-dir /tmp/ferun/ca \
        --admin-users admin:admin --bootstrap-token "$TOKEN" \
        --secret-key-file /tmp/ferun/secret.key
    ferund join-token --database-url "$DB" --ca-dir /tmp/ferun/ca
    ferun-testnode --join-token feos-join-... --addr 127.0.0.1:7443 \
        --server-name ferun.feos.test

Without `--secret-key-file` the sandboxes run, but an environment with
an addon or a secret stays deploying ("secrets need --secret-key-file").
The test node fails a pod whose security groups or name tables it does
not have, as feosd does (`--check-security-groups`,
`--check-name-tables`, both on by default; `--no-name-service` plays a
feosd from before the name tables), and with
`--fake-exec` answers every exec into a pod with exit 0 without running
anything (an exec into an environment's pod does so by default, so that
addon probes and provisioning pass).

An environment runs end to end on test nodes with these flags beside
the join's:

    ferun-testnode ... --fake-exec --fake-processes \
        --exit 'job/*=0@3s' --start-log \
        --port-addr 8080=127.0.0.1:18080

- `--fake-processes` answers the background-process scripts in memory,
  without running anything (`process_start`, and an attached service's
  start, listing, logs and stop); without it attached services never
  start.
- `--exit '<object glob>=<code>[@<delay>]'`, repeatable, makes the main
  container of an environment's pod whose `ferun.object` label matches
  exit with that code so long after each boot (2 s by default; a
  negative code: no exit status), the first match counting: with
  `job/*=0@3s` deploy jobs finish and a deployment can be deployed,
  `service/crasher=1` crash-loops a service, `process/<name>` ends a
  fake process.
- `--start-log` appends a line to a container's log at each boot and
  each such exit, so that logs and stored tails have lines.
- `--port-addr <port>=<host:port>`, repeatable, makes a port forward to
  a pod's port dial that address: readiness checks and the route door
  reach a server on the machine, here one on 18080 for the pods' 8080.
- `kill -USR1` logs the node's pods, volumes, security groups and name
  tables as one JSON line, `kill -HUP` restarts it gracefully.

Go code follows the rules of the Rust code where they apply, and these:

- **The dependency set is fixed:** the standard library, pgx, gRPC,
  protobuf, the MCP Go SDK, google/uuid, `golang.org/x/crypto` and
  `golang.org/x/sync`. A new dependency is a decision, made in `go.mod`
  and `go/internal/deps` together; `go mod tidy` and `go get` are not run
  in passing. The UI's dependencies are React and, for building and
  testing, Vite and Vitest.
- **No panics** outside `main` and tests: errors are wrapped with `%w`
  and returned. Logging is `log/slog`, structured and passed in; a log
  line that a remote party can trigger is rate-limited.
- **Every goroutine has an owner** and ends with its context;
  `context.Context` is the first argument of everything that blocks.
- **Times from the database:** every lease, TTL, grace and deadline is
  decided on PostgreSQL's clock, `now()`.
- **Nothing slow in a transaction**, no call to a node. A node's
  mutating pod calls never run on the caller's context.
- **Tests** use the standard `testing` package only; those that need
  PostgreSQL call `dbtest.New(t)`. Packages with concurrency are tested
  with `-race`.
- **gofmt and `go vet`** are clean, and every Go file starts with the
  SPDX header.

## Rules

- **Static musl.** FeOS binaries are built for `x86_64-unknown-linux-musl` and
  linked statically. Check new binaries with `file`; they must say
  "statically linked" or "static-pie linked".
- **Syscalls first.** Talk to the kernel through syscalls, using
  [`nix`](https://docs.rs/nix) (or `libc` where nix has no wrapper). Do not
  shell out to userspace tools such as `ip` or `mount`, and do not depend on
  glibc-specific behaviour. The one exception: the guest runs its static
  `mke2fs` to format a new volume ([ADR 0016](adr/0016-volumes.md));
  nothing else runs a tool, and nothing else runs `mke2fs`.
- **PID 1 must not die.** When PID 1 exits, the kernel panics. Code on the
  PID 1 path returns errors instead of panicking, and `main` idles rather
  than exit. It must keep working when filesystems are read-only.
- **Keep authorship.** FeOS has history worth keeping in `git blame`. Move
  files with `git mv`, and do not reformat code you are not changing:
  `rustfmt.toml` keeps the 2021 style edition for that reason.

## Protocol buffers

Each API has its own package `feos.<api>.v1` in
`proto/feos/<api>/v1/<api>.proto`. `crates/feos-proto` compiles all of them;
a new file needs an entry in its `build.rs` and a module in its `lib.rs`.
