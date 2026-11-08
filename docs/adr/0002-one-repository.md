# ADR 0002: One repository, laid out by language

**Status:** Accepted · **Date:** 2026-10-13

Node, control plane and clients live in one repository, laid out by
language: Rust crates flat under `crates/` in one Cargo workspace, Go
under `go/`, the management UI under `web/`, the APIs under `proto/`.
Release artifacts are built per package, never for the whole workspace,
and node binaries are static musl. What is versioned on its own or is a
client of the public API alone has a directory of its own (`feid/`,
`fechat/`). Authorship is kept: files move with `git mv`, and code nobody
changes is not reformatted.

## Context

The platform (ADR 0001) consists of programs that run in different places:
`feos-guest` in every pod, `feosd` and what joins it on every node,
`feos-host-init` where FeOS is the node's operating system, the control
plane's programs, and clients (ADR 0003). Most features change more than
one of them at once: a capability of pods is an API in the guest, a call
in `feosd` and a route in the control plane.

Many building blocks cut across the programs: the proto definitions, the
vsock transport between host and guest, sandboxing helpers (namespaces,
capabilities, seccomp), netlink configuration, OCI image handling.

The code carries the work of several contributors. That work should stay
in the codebase and in `git blame`, not only in the history.

## Decision

1. **One repository** for the node, the control plane and the clients.
2. **Laid out by language, not by component.** Rust crates live flat under
   `crates/`, the service crates under `crates/services/`. The control
   plane's programs are one Go module under `go/`, its management UI is
   under `web/`. FeOS's APIs live under `proto/feos/<api>/v1/` with the
   package `feos.<api>.v1`. Two things have a directory of their own:
   `feid/`, FeID (ADR 0014), a member of the workspace that depends on no
   crate of FeOS and is versioned on its own, so that it can move into a
   repository of its own; and `fechat/`, the reference client, which is a
   client of the public API alone, with a Go module and a front end of
   its own.
3. **One Cargo workspace**, with four rules:
   - Release artifacts are built per package (`cargo build -p <crate>`),
     never with `--workspace`, which would unify the features of all crates
     into every binary.
   - `[workspace.dependencies]` declare minimal features; each crate adds
     what it needs, since a crate cannot turn off a feature the workspace
     enables.
   - One `release` profile. A binary that needs something else gets a
     custom profile that inherits from it.
   - Crates that need another toolchain, such as eBPF programs on nightly,
     stay outside the workspace.
4. **Static binaries.** Node binaries are static musl
   (`x86_64-unknown-linux-musl`) and depend on no library of the system
   they run on. The Go programs build static too (`CGO_ENABLED=0`).
5. **Authorship is kept.** Files move with `git mv`. `rustfmt` keeps the
   2021 style edition, and code that is not otherwise changed is not
   reformatted. Where other code replaces a crate's internals, the crate,
   its API and its structure stay.

## Alternatives considered

- **A repository for FeOS and one for the platform**, the platform taking
  released FeOS artifacts. A feature across the API would become a change,
  a release and a version bump on two sides.
- **A directory per component**, as the sandbox system's repository had.
  The shared building blocks would need a home outside the components
  anyway, and each component directory would end up wrapping a single
  binary crate.
- **A workspace per component.** Shared crates would be built once per
  workspace, the lockfiles would drift apart, and there would be no single
  lint and test run. What separate workspaces give (isolated features and
  profiles, other toolchains) the rules in point 3 provide too.
- **Replacing FeOS's crates with the sandbox system's code.** Less work,
  since that code was newer in most overlapping areas, but it would have
  removed the FeOS contributors' code from the project. The sandbox
  system's functionality was ported into the FeOS crates instead; where
  its implementation was clearly better, such as image unpacking, it
  replaced the internals and the crate stayed.
- **Top-level directories for Python and TypeScript**, for FeID's client
  libraries. FeID has libraries in Rust and Go only; the directories would
  have been empty.

## Consequences

- CI lints and tests every binary's feature set, and the two feature sets
  of `crates/feos` build apart and together.
- The shared building blocks live in crates of their own (`feos-utils`,
  `feos-proto`, `feos-jail`), used by every program that needs them.
- A feature that crosses the API is one commit: proto, node, control plane
  and UI change together, and a version of the repository names a version
  of everything.
- `git blame` names the author of a line, not the person who moved or
  reformatted it.

## History

Decided on 2026-10-13 and numbered 0002 on 2026-10-14 (a1d6169). The
sandbox system's functionality was ported into the FeOS crates between
2026-10-14 and 2026-11-05 rather than replacing them. Directories for
Python and TypeScript were added for FeID's libraries on 2026-10-15
(7e35131) and taken out the same day (9ed6bfa). `go/` became the control
plane's module and `web/` its UI on 2026-10-16 (a30171a, 5d251e8), and
FeChat came in as `fechat/` the same day (ba473e4); `feid/` was added on
2026-11-08 (649707e).
