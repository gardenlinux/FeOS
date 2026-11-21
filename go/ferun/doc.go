// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package ferun is FeRun's published module (ADR 0057): what a caller of
// FeRun's API compiles against instead of keeping a copy of the format.
// It holds three packages:
//
//   - spec: an environment's spec as the API takes and answers it, its
//     strict parser, the merge patch of a spec document (RFC 7396) and
//     the finding {severity, path, problem, remedy};
//   - api: the answer and request types of the project class, the body
//     and codes of a refusal (ADR 0031), the header names;
//   - client: an HTTP client of the project class over them.
//
// Judging a spec is not published: validation, normalisation, reach,
// the diff, the bounds and the defaults are ferund's, and a caller asks
// the plan. ferund uses this module itself; there is one definition.
//
// The module depends on the standard library and go.yaml.in/yaml/v3
// alone, and on nothing of ferund's.
//
// The add-only rule: the exported names of this module are part of
// FeRun's API. A field of the spec or of an answer keeps its name, its
// type and its meaning; new fields are added; a field leaves only
// through a release in which it is still read. A caller compiled
// against an older module refuses a field FeRun gained since as an
// unknown one until it is built again, so FeRun is released first.
package ferun
