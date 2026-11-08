# ADR 0006: The control plane's calls travel through Valkey

**Status:** Superseded by [ADR 0015](0015-control-plane.md) · **Date:** 2026-10-14

It decided that `feosd` connects out to Valkey and reads its node's
command stream, Valkey carrying the gRPC calls of the `feos.*` APIs as
frames with at-least-once delivery, so that a node opens no port for
control. ADR 0015 keeps that requirement and replaces the transport:
nodes connect out to every instance of the control plane over mTLS and
serve the `feos.*` APIs on the connection they opened, with no broker
between, and the alternatives weighed here stand there.
