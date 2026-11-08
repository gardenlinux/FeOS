# ADR 0034: Reachability from declarations: a deployment's references open the ports they name, a service chooses its egress by domains and blocks, a route names its clients' blocks, a sandbox says what it reaches, and every change reaches every node before the pod's name

**Status:** Merged into [ADR 0012](0012-reachability-from-declarations.md) · **Date:** 2026-11-06

This ADR decided that a deployment's references, `uses`, routes and a
sandbox's `reaches` are the rules a pod gets, that a service and a job
choose their egress by domains and blocks, which the resolver enforces
into a timed set, and that a route's `from` names its clients; its first
version had given tenants security groups of their own. All of it now
lives in ADR 0012, which states reachability from declarations together
with the security groups that enforce it on the node. Its propagation
of member changes as pushed deltas, members before names, is
[ADR 0052](0052-pushed-membership-deltas.md); the sandbox's and the
tenant's job's place in an environment is ADR 0035.
