// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package spec is the deployment spec of an environment (ADR 0018): the
// wire format a client sends to plan or apply a deployment, and ferund's
// treatment of it, which is pure (no database, no node, no registry):
//
//   - Parse reads a spec strictly: unknown fields anywhere are error
//     findings, never ignored.
//   - Validate checks it against the rules of the contract's §3, looking
//     up what lies outside the spec (the tenant's sandboxes, the
//     environment's secrets, the addon types, the images' resolution)
//     through a ValidateContext the caller filled beforehand.
//   - Normalise fills the defaults, makes the runtime explicit, writes
//     durations as Go writes them, pins images to digests and secrets to
//     versions, and drops what nothing references: the form ferund
//     stores, hashes and diffs. EqualNormalised compares two of them.
//   - Diff lists what a deployment changes against another, per object, with
//     the effect on the running pods; secret values never appear in a
//     spec, versions do.
//   - MergePatch applies an RFC 7396 merge patch to a spec document.
//
// A spec is JSON with object maps keyed by DNS-label names. Numbers are
// JSON numbers. Every problem is a Finding {severity, path, problem,
// remedy}, the shape FeCode shows its users and agents too.
package spec
