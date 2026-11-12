// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package visits is ferund's side of ADR 0054: a tenant's access
// providers, the challenges providers answer, the visits ferund keeps, and
// their codes and tokens on the ingress's connection.
//
// The keys of tokens and challenges are cluster secrets (visit-token, the
// Ed25519 seed sealed with --secret-key-file; visit-challenge, the AES-256
// key), made on first use, so visits run only with a sealer. A provider of
// kind tenant is the tenant's own service, which ferund asks at a visit's
// renewal, at most every visit.AskEvery and never inside a transaction;
// kinds ferun and oidc are served by ferun-auth (--visitor-auth-url).
package visits
