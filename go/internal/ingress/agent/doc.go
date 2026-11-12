// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package agent is the control side of ferun-ingress (ADR 0021): the
// instance's identity and state directory, its one connection to the
// control plane, the table it serves, its place on the pods' mesh and the
// Wake, Hold and visit calls of the data path. It speaks the protocol of package
// wire and imports nothing of ferund's.
//
// # State directory
//
// The directory is 0700 and its files 0600; every write goes to a
// temporary file that is synced, renamed into place and followed by a
// sync of the directory, so a crash leaves the old content or the new.
//
//	tls.key                the certificate's key, ECDSA P-256, PKCS #8 PEM
//	cert.json              the certificate, its chain and the trust bundle
//	cert.json.lost-<unix>  the certificate of an identity declared lost
//	join-tried             the SHA-256 (hex) of every join token spent, one per line
//	removed                the refusal that said the ingress was removed, until the next join
//	wg.key                 the WireGuard private key, 32 raw bytes
//	mesh.json              the place on the mesh: {podNetwork, prefix, address}
//	table-v2.json          the table cache: {savedAt, state}, written 1 s after a change,
//	                       and at least once a minute while heartbeats confirm it; it holds
//	                       the domains' certificates and keys, the ACME challenges and the
//	                       visits' keys (ADR 0054)
//	table.json             the cache of the releases before visits: never read, and deleted
//	                       by the first write of table-v2.json, so an older binary started
//	                       on the volume serves nothing until it is connected
//
// The certificate is renewed for the same key. A new key is made only
// when the identity is declared lost, so tls.key and cert.json never
// disagree while cert.json exists, and a Join repeated after a lost
// answer carries the same key, which the control plane answers again.
// A new WireGuard key is made after every join, before the certificate
// is kept, and so before the first Hello after it; the control plane
// fixes it at that Hello. A join again retires the key before it, so
// that a copy of the directory does not stay on the mesh: the identity
// declared lost always ends in a join, and thus in a new wg.key.
//
// # Start
//
// Run loads the state and, before it contacts the control plane,
// configures the mesh device from mesh.json, wg.key and the cached
// table's peers, and hands the cached table to OnState: an instance that
// restarts while the control plane is away serves at once. Without a
// valid certificate it joins: it reads the token file (every 30 s while
// there is none it has not tried), checks the server against the token's
// pin before it sends the token, and records the token in join-tried once
// a Join with it was answered or refused, which spends it: it is never
// sent again. A Join that got no answer is sent again with the same token
// and key, which the control plane answers again; so is one whose
// certificate or new WireGuard key could not be kept.
//
// # Connection
//
// The agent holds one connection to one control-plane instance at a
// time: TLS 1.3 with ALPN ferun-ingress/1 to an address of the list,
// tried in random order, with backoff (200 ms to 5 s, ±20 %) when every
// address failed. On it, Hello takes the instance's place on the mesh
// (the device is configured from the answer), and Sync streams the table:
// a whole snapshot, then deltas. Each applied message goes to OnState as a
// copy and to the cache; a change of the mesh's version sets the device's
// peers. A message out of sequence, or a stream that ends or stays silent
// for 45 s, makes the agent connect again, and the new stream starts
// whole. The certificate is renewed at its renew time on the current
// connection; the agent then connects with the new certificate and drops
// the old connection only once the new one said Hello. Every Hello names
// the capability visits (ADR 0054), so the control plane also streams the
// environments that admit visitors, with the visits' keys.
//
// # Visits
//
// RedeemVisit, RenewVisit and EndVisit are the data path's calls for
// visits, on the current connection like Hold, ErrNotConnected without
// one. At most 8 renewals, and separately 8 redeems and ends, are in
// flight; a call beyond fails at once with ErrBusy, which status.Code
// reads as ResourceExhausted and ErrNotConnected as Unavailable, so the
// data path tells a busy instance from a control plane that is away.
//
// # Refusals
//
// A PERMISSION_DENIED never throws the identity away by itself (D19). The
// identity is declared lost when the refusal says the ingress was
// removed, or when refusals persist for 5 minutes over at least 3
// connection attempts without a successful Hello in between. cert.json is
// then renamed to cert.json.lost-<unix>, a new key is made, and the agent
// joins again, with a token it has not tried. A removal is reported by
// IdentityRemoved until the next join, also after a restart.
//
// A Hello refused with FAILED_PRECONDITION because the mesh key is fixed
// means wg.key changed without a re-join: the agent logs it, keeps
// serving from the cache and tries again every 60 s. One refused with
// FAILED_PRECONDITION "…; make a new one" (the key a join again retired,
// or another peer's) makes a new wg.key, and the agent connects again
// after its backoff.
//
// # Mesh
//
// MeshDevice is the WireGuard device; NewKernelMesh is the kernel's (Linux
// only). A device that cannot be configured, or no longer be read, is
// configured again every 2 s while the connection and the table go on;
// MeshConfigured reports it. Every 2 s, and after the peers changed, the
// agent reads the device's peers and reports each node's last handshake
// to OnMesh, the input of the data path's gating.
//
// A node that runs endpoints and has had no handshake for 180 s is lost,
// and the agent repairs its path, at most once a minute: the device moves
// to the next of 16 ports from MeshPort on, the lost nodes' peers are
// removed and set again, and a Hello on the current session tells the
// control plane the new endpoint, which reaches the nodes as any new
// endpoint does. A WireGuard peer's UDP flow keeps its addresses and ports
// for good, and the keepalive and the handshake's retries never let it
// rest, so a flow that an outage left broken in the network, or a learnt
// endpoint that leads nowhere, would otherwise last until the pod is
// replaced.
package agent
