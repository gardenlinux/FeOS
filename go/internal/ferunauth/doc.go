// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package ferunauth is ferun-auth (ADR 0054): the access provider of the
// kinds ferun and oidc, an OpenID Connect relying party beside ferund on a
// host of its own.
//
// A navigation to a guarded environment without a visit comes to GET
// /visit with the challenge the ingress sealed. ferun-auth shows it to
// ferund (POST /api/v1/challenges/open, with its service token of kind
// auth), which says the environment, the host and the provider: kind
// ferun, the installation's issuer (--issuer, exactly ferund's
// --oidc-issuer), or kind oidc, a tenant's issuer, client and rule. The
// browser goes to the issuer with state, nonce and a PKCE challenge; the
// sign-in in flight lives in a sealed cookie of its own on ferun-auth's
// host (__Host-ferun_auth_<k>, k from the state), so instances share
// nothing but --key-file and several tabs sign in at once. At GET
// /callback the code is exchanged, the ID token verified, and the
// challenge accepted (kind ferun: ferund decides by the person's grant;
// kind oidc: when the provider's rule admits the visitor) or rejected; the
// browser goes on to ferund's redirect, the host's /.ferun/auth. The
// issuers' back-channel logouts at POST /backchannel/ferun and
// /backchannel/<tenant>/<provider> end the visits of a session.
//
// Every issuer URL is fetched through the ruled client of package egress:
// https only, IPv4, never into --deny-prefix's blocks unless
// --allow-prefix allows them, redirects of GETs only. ferund is called at
// --ferun-url, inside the cluster. ferun-auth links no database driver and
// no gRPC; it logs no challenge, state, code, token, secret or session.
//
// Every flag has an environment variable FERUN_AUTH_<NAME> (the flag's
// name in upper snake case); a flag given wins over its variable.
package ferunauth
