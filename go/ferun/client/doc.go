// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package client is an HTTP client of FeRun's project class (ADR 0057)
// over the types of packages spec and api: one method per route, the
// header that names a project (FeRun-Project) and the person an
// integration acts for (FeRun-Person), and the refusals of ADR 0031 as
// *Error with FeRun's body.
//
// The client sends JSON; a caller reads a YAML spec with
// spec.YAMLToJSON or spec.ParseYAML first. It neither retries nor
// caches: the context ends each call. Answers are decoded leniently, so a
// field FeRun adds to an answer reaches an older caller unread; the
// strict parser is for specs. The sink's reads (logs, traces) take the
// same base URL and headers and answer raw JSON.
package client
