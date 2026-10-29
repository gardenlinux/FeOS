FeOS Documentation
==================

| You want to know | Read |
|---|---|
| what FeOS is, what it is for, where it stands | [overview.md](overview.md) |
| how the node, the guest, the API and the control plane work | [architecture/](architecture/) |
| why it is built this way | [adr/](adr/) |
| where it protects less than intended, and what closes it | [security.md](security.md) |
| how to build, test and contribute | [development.md](development.md) |
| how to do a particular thing | [guides/](guides/) |
| what changed in a release | [changelog/](changelog/) |
| odds and ends | [misc/](misc/) |

## Guides

- [Test environment](guides/test-environment.md): FeOS as the node's
  operating system in a KVM VM with nested virtualization, with router
  advertisements, DHCPv6, DNS and an OCI mirror on the host.
- [Booting the UKI](guides/boot-uki.md): what the unified kernel image is,
  and booting it from a USB stick.
- [Cutting a release](guides/release.md).

Building images and the tooling under `hack/`:
[hack/README.md](../hack/README.md).

## Where things go

- **overview.md** changes when what we build changes.
- **architecture/** describes the system as it is and changes with the
  code, in the same commit. What is decided but not built is marked as
  such.
- **adr/** records decisions; see its [rules](adr/README.md#rules).
- **security.md** lists where the system protects less than its ADRs
  intend; an entry goes with the commit that closes it.
- **guides/** are step-by-step instructions for one task each.
- **changelog/** holds one file per release, `FeOS-<version>.md`, which
  the release workflow publishes as the release's notes.
- **misc/** takes whatever fits nowhere else: findings, notes, write-ups.
  Nothing there is kept up to date; a file starts with its date.
