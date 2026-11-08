# ADR 0036: A workload's identity: one ID per service, job, addon and sandbox, in a certificate that says who and never what; an addon maps it to the role a reference grants, so that no password reaches the pod; within a cluster the address is the workload

**Status:** Merged into [ADR 0014](0014-identity-and-credentials.md) · **Date:** 2026-11-07

It decided the layout of the SPIFFE IDs (one per service, job and
addon, shared by their replicas and rolls; a sandbox and a tenant's job
by their IDs; the environment by its ID), the subject that names the pod
in its CN and the workload's set in an `OU`, that a certificate says who
and never what, that within a cluster the address is the workload and no
server checks its callers' certificates there, that a reference to an
addon's role is its grant through the addon's map, and that outbound TLS
bindings are terminated in the guest while bindings that add a secret
stay on the host. All of it lives in ADR 0014, the addons' maps in
[ADR 0038](0038-addons-admit-a-workload-by-its-certificate.md), and
what it parked (servers that read their callers' IDs, a certificate of
the network's own, flows named by IDs) is in the backlog.
