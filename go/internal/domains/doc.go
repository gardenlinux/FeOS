// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package domains is the core of a project's domains in ferund (ADR 0021):
// claims on names, proven by a TXT record of the claim's token at
// <--domain-challenge-label>.<name>, bound to an environment of the
// project, routed to the edge by a CNAME to --edge-name or by A and AAAA
// records of the edge's addresses, and checked for as long as they live.
// A claim may be a wildcard, *.<name> (ADR 0045): its TXT record stands
// beside <name>, its routing is a fresh name below it resolving to the
// edge and its delegation, the CNAME of _acme-challenge.<name> to
// <token>.<--acme-zone>, where internal/acme answers its DNS-01.
//
//   - name.go: a name's normalization (lowercase LDH, IDNs as A-labels)
//     and the claims' tokens;
//   - state.go: Derive, the one place a domain's state and reason come
//     from;
//   - resolver.go: the lookups, on Go's own resolver over --dns-resolver
//     (or the system's resolvers): a recursive resolver, which may answer
//     from its cache for a record's TTL;
//   - check.go: one check, and the transitions it and every other writer
//     go through (settle): verification and supersession, the graces of
//     the TXT and the routing record, deactivation, the incidents of its
//     environment;
//   - service.go: the REST routes' operations, the environments' hook,
//     the leader's loops (domains-check, domains-settle).
//
// Certificates are internal/acme's: a verified, bound and routed domain
// waits in issuing until ACME kept its certificate, is served with it,
// and failed while orders fail without one. ACME calls Refresh after
// every order; the settle loop catches a certificate that expired.
//
// Every deadline is decided on the database's clock: a transaction's
// now(). The lookups run outside any transaction; their result is written
// in a short one that locks every claim of the name. Only an answer
// counts as wrong: a lookup that fails starts and ends no grace (it sets
// check_error), though a grace that ran out deactivates the domain at its
// end whether the lookup then answers or not, since its state is derived
// from the time.
package domains
