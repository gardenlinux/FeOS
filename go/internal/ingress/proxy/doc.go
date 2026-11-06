// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package proxy is the data path of ferun-ingress (ADR 0021): TLS by SNI
// with the operator's wildcard certificate, then HTTP/1.1 or HTTP/2 from
// the client, routed by host, environment and route to an instance of a
// service, and HTTP/1.1 to the instance's pod address on the pods' mesh,
// over pooled keep-alive connections.
//
// The routing is the table the agent keeps (wire.State), rebuilt as an
// immutable value on every change (SetState). An instance is usable only
// when the control plane admitted the route's port on its node, its node
// holds a version of the security group ferun-ingress that admits this
// ingress, and the ingress has a fresh WireGuard handshake with that
// node (SetMeshHealth); an instance on a node without one is the last
// resort.
//
// A request that finds no usable instance is held until the table shows
// one: while instances of its service are asleep or waking for up to the
// wake bound (the proxy asks the control plane to wake them), while they
// are only pending for up to the pending bound. A request to a service
// with an idle policy first holds its instance at the control plane, so
// that the idle loop cannot pause the pod under it (holds.go).
//
// A request whose dial failed is sent once more to another instance,
// whatever its method, as nothing of it was sent; a GET, HEAD or OPTIONS
// without a body also when it was sent but no response came. Responses
// stream unbuffered, and upgrades pass through.
//
// One tenant cannot take the instance from the others, nor one
// environment its tenant's (limits.go): forwarded and held requests each
// have a share per tenant and within it one per environment beside the
// instance's cap, each wait for a connection to an instance is bounded,
// and an HTTP/2 connection carries at most 63 streams. What a request
// keeps while it waits is bounded as well: 32 KiB of headers in 100
// fields, no trailers, and 64 KiB of its body (HTTP/2's window). When
// the table changes, the open connections to what it no longer routes
// to are closed, upgraded ones included (Proxy.cutConns).
//
// On a host under the apps zone or the tenant zone (ADR 0031), the proxy
// makes every cookie of a response host-only (cookies.go): an app can then
// set no cookie for other apps under the zone, nor for the platform's
// hosts beside it.
package proxy
