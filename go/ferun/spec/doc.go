// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package spec is an environment's deployment spec as FeRun's API takes
// and answers it (ADRs 0018, 0057), with what a caller needs to read and
// change one:
//
//   - the types of a spec, its services, jobs, addons, routes, bindings
//     and secret pins, with their JSON forms; a spec is JSON with object
//     maps keyed by DNS-label names, numbers are JSON numbers;
//   - Parse, the strict reader: an unknown field anywhere, a value of the
//     wrong type, a duplicate key and trailing data are error findings
//     with their paths, the same ones ferund's plan gives;
//   - MergePatch, an RFC 7396 merge patch of a spec document, with which
//     a caller lays its own values over a spec;
//   - Finding {severity, path, problem, remedy}, the shape of every
//     problem of a spec, and the diff's entries.
//
// Whether a spec holds is the plan's answer, not this package's: the
// bounds, the defaults, the catalog and the rules live behind it.
package spec
