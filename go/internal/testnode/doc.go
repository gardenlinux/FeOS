// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package testnode is a Go implementation of what feosd does on the wire
// to the control plane (ADR 0015), for tests and local development: a
// node that ferund cannot tell from feosd by what it sends and answers.
//
// The node joins with a token "feos-join-<secret>.<pin>" over a connection
// that authenticates the control plane by the token's pin, or takes the
// identity it kept in its state directory. Afterwards it keeps a node
// connection (ALPN "feos-node/1") to every instance the control plane
// lists, on which it is the HTTP/2 server of its feos.* APIs, and an
// uplink (ALPN "h2") to every one, of which it uses one at a time: there
// it reports telemetry, publishes its events with dense sequence numbers
// per start and acknowledgements, renews its certificate and asks for
// pods' credentials.
//
// Its pods live in memory and follow feosd's rules: creates are idempotent
// by their normalised spec, the lifecycle calls have feosd's preconditions
// and answers, hibernations go to a fake object store that nodes share
// (in memory, or in a directory that several processes share),
// and calls that carry "feos-pod-id" reach a fake guest: commands run with
// a temporary directory per pod as their working directory, the tools work
// on that directory, watches answer with scripted events, and port
// forwards dial local addresses. A hibernation runs on its own: every
// HibernatePod that comes while it runs joins it, and a ResumePod abandons
// it until its head is being put (Config.ResumeWaitsForHibernation plays
// feosd from before that). A Script injects faults and scripted answers
// while the node runs, before a call or at a point inside its work
// (HibernateUpload, HibernateHead); Restart, Crash and Partition play what
// happens to a real node.
//
// For an environment's pods (ADR 0018) it serves VolumeService,
// SecurityGroupService and NameService over records in memory with
// feosd's rules (a pod claims its volumes at its create; with
// Config.CheckSecurityGroups and Config.CheckNameTables a pod whose groups
// or name tables the node lacks fails at its bring-up; a RestorePod that
// brings security groups or name tables puts them into the pod's spec,
// each unless empty, before it is placed (ADR 0035); Node.Resolve
// answers a pod's name as feosd's resolver would; Config.NoNameService
// plays a feosd from before the name tables), reports a
// container's exit with its code (Script.ExitContainer) and serves a
// container's log (Script.AppendLog, StreamContainerLogs with
// tail_lines). An Exec into an environment's pod exits 0 without running
// anything unless the script answers it (Script.SetPodExec), so that an
// addon's probe and provisioning pass.
package testnode
