// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package ports is ferund's port front door (§13 of the design): HTTP to a
// port inside a sandbox, at /fwd/{id}/{port}/{rest...} and at its alias
// /api/v1/sandboxes/{id}/ports/{port}/{rest...}.
//
// The door has an auth class of its own, the forward class: the sandbox's
// token in "Authorization: Bearer" and nowhere else (no cookie, no query
// parameter), the {id} of the path a claim the token must back, and
// answers in plain text. A token of another sandbox and an unknown one get
// the answer of an absent sandbox; a tenant's token gets 401 "forwarding
// needs a sandbox token", as no token does. A port of the sandbox's
// Kubernetes bindings is never forwarded.
//
// What the door does is a lookup and a pipe. A request to a sandbox that
// sleeps waits while the engine brings it up; the bringing up runs
// detached from the request, so that a browser that gives up on a cold
// start does not cancel it for the next request. Then the request goes
// through httputil.ReverseProxy to a pseudo-host that names the pod and
// the port; the transport behind it dials by opening
// feos.portforward.v1.PortForwardService/Forward on this instance's node
// connection to the sandbox's node, and uses the stream as a net.Conn.
// Nothing parses the payload, so HTTP, its upgrades (WebSockets, hot
// reload) and whatever an upgrade turns into ride through unchanged. The
// sandbox's token stops at the door; the server in the guest sees the
// Host the client sent, which for FeCode's previews is the preview host,
// or the one it names in X-FeRun-Host when the Host had to be the
// platform's own name to reach the door through an ingress that routes
// by Host (GuestHost).
//
// Connections are pooled per pod, port and node: a page that loads a
// hundred modules rides a few Forward streams, at most 32 per sandbox on
// this instance, pooled ones included, and so at most 32 per port. A
// request waits up to 30 s for its turn at a connection, a pooled one or a
// slot for a new one, and then gets 503; a dial that has its slot waits
// for the node and the guest as long as they take. The environments'
// readiness checks (Probe, DialPod) have four slots of their own per
// sandbox beside the 32, so that traffic never fails a check. A forwarded request,
// and an upgraded connection for as long as it is open, holds the sandbox
// awake (a forward hold), and its end counts as activity. At an instance's
// stop the upgraded connections are closed once no request is left
// (CloseUpgraded); their clients reconnect to another instance. So are
// those to a pod that stops running on its node, as when a drain moves its
// sandbox: a forward does not keep a sandbox on a draining node, and its
// client reconnects to where it runs next.
package ports
