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
//
// The files: visits.go the service, its keys and its sweep (the leader's
// loop visits-sweep); providers.go a tenant's providers; challenges.go
// the providers' routes (open, accept and reject a challenge, read a
// visit, end the visits of an issuer's session); ingress.go the ingress's
// calls (redeem a code, renew and end a visit); ask.go the call to a
// provider of kind tenant. What a provider may do is decided here, after
// the challenge is opened or the visit read: anything it may not is a
// 404, never a 403. A failure inside the service is answered with a fixed
// text and logged; no challenge, code, token, binding or session is ever
// logged.
package visits
