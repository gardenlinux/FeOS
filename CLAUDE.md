# FeOS — agent instructions

FeOS is the node layer for microVM pods, in one monorepo: `feosd` (the node
agent), `feos-guest` (PID 1 in a pod), `feos-host-init` (PID 1 of a node
whose operating system is FeOS) and `feosctl`. Read `docs/overview.md`,
`docs/development.md` (layout, binaries, commands, rules) and `docs/adr/`
before larger changes.

Hard rules:

- **Static musl.** Binaries build for `x86_64-unknown-linux-musl`, statically
  linked. Check new binaries with `file`.
- **Syscalls first**, through `nix` (or `libc` where nix lacks a wrapper). No
  shelling out to userspace tools, no glibc-specific behaviour. The one
  exception: `mke2fs` in the guest, to format a new volume (ADR 0016);
  nothing else.
- **PID 1 must not die.** No panics on the PID 1 path; return errors, log,
  keep serving.
- **Both feature sets must build.** `make clippy` lints `feosd`'s features
  (the default), `feos-guest`'s (`-p feos --no-default-features --features
  guest`) and both together. feosd-only code goes behind
  `#[cfg(feature = "host")]`, guest-only code behind
  `#[cfg(feature = "guest")]`; features stay additive.
- **Release builds per package** (`-p`), never `--workspace`.
- **Keep authorship.** Use `git mv` to move files, and do not reformat code you
  are not otherwise changing.

Before you commit: `make fmt-check clippy test-unit`. When you change what
runs in the guest, also `make guest test-guest test-guest-container` (needs
/dev/kvm, and sudo for the test network). When you change the pod or
microVM service, also `make test-pods` (needs docker).

FeRun, the control plane (ADR 0015), is Go in `go/` (`ferund`,
`ferun-testnode`) with its management UI in `web/`; read
`docs/architecture/control-plane.md` before larger changes there. Its rules:

- **Fixed dependencies.** The standard library, pgx/v5, grpc, protobuf, the
  MCP go-sdk, google/uuid, x/crypto, x/sync and franz-go (the sink's Kafka
  client, ADR 0026 §11), and go.yaml.in/yaml/v3, in the published module
  go/ferun alone (ADR 0057); for the UI React, Vite and
  Vitest. Never `go get`, `go mod tidy`, or edit `go.mod`, `go.sum` or
  `go/internal/deps`, and add no npm package, without asking.
- **Static.** `ferund` builds with `CGO_ENABLED=0`; check it with `file`.
- **No panics** outside `main` and tests: wrap errors with `%w` and return
  them. `log/slog`, passed in; every goroutine ends with its context.
- **The database's clock** decides every lease, TTL and deadline (`now()`).
  No node call inside a transaction; a mutating pod call never runs on its
  caller's context.
- **Tests** use the standard `testing` package only; database tests call
  `dbtest.New(t)`, which skips without `FERUN_TEST_DATABASE_URL`. Run
  packages with concurrency with `-race`.
- Every Go file starts with the SPDX header; gofmt and `go vet` are clean.

When you change `go/`: `make go-lint go-test` (a throwaway PostgreSQL 17 in
docker; it covers the published module `go/ferun` too), or, for one package,
`go test -race ./internal/<pkg>/...` in `go/` with `FERUN_TEST_DATABASE_URL`
set, and `go vet ./... && go test -race ./...` in `go/ferun` when you change
it. When you change `web/`: `cd web && npm test`, and `make web` to build
it into `go/internal/ui/dist`.
