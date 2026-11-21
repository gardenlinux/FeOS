// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package api holds the answer and request types of FeRun's project
// class (ADR 0057): an environment's status, a plan with its findings
// and its diff, deployments, the catalog, the quota, secrets, the body
// and codes of a refusal (ADR 0031) and the header names. ferund answers
// with these types; a caller decodes them.
package api
