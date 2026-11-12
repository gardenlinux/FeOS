// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package table builds the ingress's table in ferund (ADR 0021): what
// ferun-ingress routes, as the Sync stream of the ingress service sends
// it. Every ferund instance that serves an ingress stream builds the
// whole table from the database itself, so that any instance can serve
// any ingress and a failover is a reconnect plus a whole snapshot.
//
// BuildEnv turns one public environment's rows (store.IngressRouting)
// into a wire.Env, purely: the routes of its routing deployment (the
// deployed one, else the latest), and for each routed image service the
// instances that answer (Endpoints, with only the ports their node
// admits to the ingress) and the others counted by why they do not
// (Asleep: Wake helps; Waking; Pending). An environment without a usable
// spec (ErrSpec), or whose entry would take more than MaxEnvBytes
// (ErrTooLarge), cannot be routed and is left out. Its served domains
// (store.IngressDomains, the keys opened with the Builder's Sealer) are
// more hosts with certificates of their own, as many as fit the bound.
// Who it admits (ADR 0054) is in its entry: open to anyone it carries
// nothing of it, so its entry is as before; otherwise Access, its
// provider's name and SignIn, where a navigation without a visit is sent
// (SignIn: a provider of kind tenant's URL, ferun-auth's /visit for ferun
// and kind oidc, "" when there is none), and it never loses Access on any
// path, so the ingress fails closed. Diff leaves such environments out for
// an ingress that does not check visits.
// BuildMesh turns the mesh's peers into the nodes the ingress peers
// with.
//
// The Builder keeps an immutable Snapshot of the whole table up to date
// while someone uses it (Use): it rebuilds an environment when
// ferun_ingress names it, or ferun_sandbox names one of its instances'
// sandboxes, after a short debounce; it reads the mesh's peers again on
// "mesh" and the pending ACME challenges on "challenges"; and it reads
// everything every few seconds as a backstop, and the installation's keys
// of visits at a full read until it has them
// (writers of an older ferund send no ferun_ingress). An environment
// whose rows are gone (no longer public, deleting, of a deleting tenant)
// leaves the snapshot. Diff gives a stream the environments to put and
// to remove since what it sent.
package table
