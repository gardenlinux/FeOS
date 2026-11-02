// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package acme is ferund's ACME client (ADR 0021, "Certificates: ferund
// is the ACME client"): it obtains and renews the certificates of
// tenants' domains over HTTP-01, through the ingress's table, with
// golang.org/x/crypto/acme.
//
// # Account
//
// The platform has one account per directory (--acme-directory): an
// ECDSA P-256 key, made by the first instance that needs it and sealed
// in acme_accounts under --secret-key-file (additional data
// 'acme-account:' || directory). The directory's primary key is the
// lock: an instance that lost the insert to another uses that one's key,
// so concurrent instances never register two accounts. The terms of
// service are agreed to; the contact is mailto:<--acme-email>, kept up
// to date.
//
// # Orders
//
// One leader loop, LoopName, runs every 30 s and when ferun_leader names
// Hint. It deletes expired challenges, opens the incidents of failing
// renewals, and with a directory orders a certificate for every due
// domain, one at a time (store.ClaimACMEOrder: verified, bound to a
// public environment, routed, its TXT record not missing, its
// next_issue_at passed; issuing or failed without a valid certificate,
// or a certificate past its renew_after). The claim moves next_issue_at
// past the order's bound, so that a crash or a new leader does not run
// it twice at once. No network call runs inside a transaction.
//
// An order is for the one name, with HTTP-01, in the profile
// --acme-profile names (default shortlived, Let's Encrypt's six-day
// certificates; empty: the CA's default):
//
//  1. a new order, its authorization, its http-01 challenge;
//  2. the answer kept in acme_challenges (expires_at ChallengeTTL ahead),
//     which notifies the ingress's table: every instance answers it on
//     port 80;
//  3. the challenge is accepted only once every ingress instance
//     connected now (seen within 45 s, not left) holds its token
//     (ingresses.challenge_tokens, which each Sync stream records when it
//     has sent the challenges), at most DeliveryWait; an order with no
//     instance connected, or one that does not get the answer in time,
//     fails without asking the CA to validate;
//  4. the authorization and the order are awaited, the order finalized
//     with a CSR of a new ECDSA P-256 key, the chain fetched (DER, leaf
//     first) and checked: it is for the name and for the key;
//  5. the certificate is kept in domain_certs, its key sealed with
//     'domain-cert:' || domain_id, renew_after at half its lifetime
//     (a six-day certificate is renewed after three days, with three
//     left for failures); the domain's failure is cleared, its challenges deleted,
//     its incident certificate_failing closed, and the table notified.
//
// A failure records issue_error (readable: a CAA record that forbids the
// CA, the CA's rate limit, the challenge's failure with the CA's
// detail), a backoff of 1 h doubling to 6 h (or the CA's Retry-After,
// when longer) in next_issue_at, and issue_failing_since; the domain's
// challenges are deleted. After every order, successful or not, the
// domains core's Refresher re-derives the domain's state.
//
// # Renewals
//
// The loop is the renewals' reconciler: a certificate past its
// renew_after is due like a new domain, and ordered again while its
// domain is still verified, bound, public and routed; one whose name
// resolves elsewhere (dns-moved) is not renewed, as HTTP-01 could not
// succeed. The ingress serves a renewed certificate from the next
// handshake on, as the table carries it. The CA's renewal information
// (ARI, RFC 9773) is not asked: x/crypto/acme has no client for it, and
// one order per domain every three days stays far below Let's Encrypt's
// limits.
//
// # Incidents
//
// A domain served with a valid certificate whose orders have failed for
// a day gets the incident certificate_failing on its environment
// (object domain/<name>, deployment the environment's latest); a renewal
// that succeeds closes it.
package acme
