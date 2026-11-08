# ADR 0017: A WireGuard mesh carries pods' IPv6 between nodes that their network routes no prefix to

**Status:** Merged into [ADR 0010](0010-pod-network.md) · **Date:** 2026-10-17

This ADR decided that a cluster whose network routes no prefix to its
nodes gives every node a /96 of a ULA /64 of the cluster's own, carried
by a WireGuard mesh in which every node is a peer with its /96 as its one
allowed prefix, and that a pod with a ULA address gets a route for
`fc00::/7` in place of a default route over IPv6. The cluster's ULA, the
node's /96, the device, the peers and how the control plane sends them,
and the pod's routes are now the mesh and IPv6 sections of ADR 0010; the
ingress as a peer of the mesh is ADR 0021. The node's side is described
in docs/architecture/node.md, the control plane's in
docs/architecture/control-plane.md.
