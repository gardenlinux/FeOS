// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package service is ferund's side of the ingress's API,
// ferun.ingress.v1.IngressService (ADR 0021, internal/ingress/wire): what
// an instance serves to ferun-ingress on the nodes' port, under ALPN
// ferun-ingress/1, and what it decides about the ingress's instances.
//
//   - Join, on the ingress join server, turns a one-time join token of
//     kind ingress, "feos-ingress-join-<secret>", and a signing request
//     into the instance's name and its first certificate,
//     spiffe://<td>/ingress/<name>. A token pinned to a name creates that
//     name or joins it again (every older certificate is revoked, its
//     WireGuard key cleared and retired); an unpinned one creates the
//     proposed name only. A repeated Join with the same key within 10
//     minutes answers again.
//   - RenewCertificate, Hello and the later methods, on the ingress
//     server, take the instance's name from its verified certificate
//     (nodelink.IngressFromContext), never from a message.
//     RenewCertificate renews the instance's newest certificate alone,
//     for its key, near its renew time; Hello makes the presenting
//     certificate current (an older one is superseded: revoked one grace
//     ahead) and puts the instance on the pods' mesh with its fixed
//     WireGuard key, which no other peer holds and which, after a join
//     again, is not the one the join retired.
//   - Sync, after a Hello on its connection, streams the instance's table
//     (internal/ingress/table): whole first, then deltas, each in parts
//     when it takes more than wire.PartBytes, and an empty table every
//     heartbeat, which records the ingress as seen. What the connection's
//     first Hello named decides what the streams on it get (ADR 0054): an
//     ingress that names wire.CapabilityVisits gets every environment and
//     the keys of visits; any other never gets an environment that is not
//     open to anyone (one it got open is removed once it turns guarded),
//     so that an older ingress, which would serve it open, never has it.
//   - RedeemVisit, RenewVisit and EndVisit are the visits of ADR 0054 on
//     the ingress's connection, answered by ferund's visits service
//     (Deps.Visits, internal/visits): an accepted code redeemed for a
//     visit's first token, a visit's next token, a visit ended at its
//     sign-out. Their outcomes are answers, never PERMISSION_DENIED; a
//     malformed request is INVALID_ARGUMENT and every failure UNAVAILABLE,
//     which the ingress reads as ferund away (it serves on with the token
//     it has). Without visits on the instance they answer UNAVAILABLE.
//   - Wake wakes the asleep instances of a routed service of a public
//     environment in the background (Engine.Sync, Engine.Ensure), once at
//     a time each.
//   - Hold holds the instances the ingress sends requests to, so that the
//     idle loop does not pause them, releases them, and releases all when
//     the ingress leaves.
//   - CheckIngress decides, for nodelink, whether a certificate of an
//     ingress may connect, on the database clock and never from a cache.
//   - Run runs the table's builder and follows the other instances'
//     writes on ferun_ingress: a change of an ingress re-checks its
//     connections on this instance and has its streams read its row.
//   - BeginStopping is the instance's stop for the ingress (D18): new
//     ingress connections close, the streams end with UNAVAILABLE, and
//     Wake and Hold answer UNAVAILABLE.
//   - IssueJoinToken, ListIngresses and RemoveIngress are the operator's:
//     the admin API's and `ferund join-token --kind ingress`.
package service
