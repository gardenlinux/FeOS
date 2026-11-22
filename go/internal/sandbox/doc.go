// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package sandbox is the sandbox engine of ferund (§9 of the design, ADR
// 0015): it decides where a sandbox's pod runs and moves it through its
// lifecycle on the nodes, across any number of instances that share one
// PostgreSQL database.
//
// The row of a sandbox in sandboxes is the truth every instance acts on.
// Every lifecycle change is an op, begun in one transaction under the row's
// lock (the op columns name the op, its owner, its target node and its
// deadline), then run against the node outside any transaction, and
// finished in another transaction that applies what the node did. An op's
// node calls never run on the caller's context: they keep running when the
// caller gives up, until the op's deadline, because feosd cuts a mutating
// pod call whose request is cancelled at an arbitrary point. HibernatePod
// is the exception on a feosd that reports its hibernations
// (PodInfo.hibernation): it runs the hibernation in a task of its own,
// which outlives its call, so a hibernate op's deadline
// (--hibernate-timeout) is how long ferund waits for it, and its end
// arrives as the pod's event; an older feosd runs it in the call, which
// the deadline cuts. An
// op whose owner died or stops is taken over by another instance; a finish
// that finds the op no longer its own writes nothing but a tombstone for a
// pod it may have made, or a newer checkpoint.
//
// Around the ops:
//   - Ensure brings a sandbox to running before a tool call, a forward or a
//     resume: it places it (the old placement algorithm with the gates of
//     §9), resolves and pins its image, and starts or joins the ops that
//     its state needs. A hibernate in flight that a resume abandons now, by
//     its node's word, it preempts: a resume op takes the hibernate's
//     place, and the sandbox resumes from its snapshot on the node. A
//     drain's hibernate is never abandoned. Another hibernate in flight,
//     which takes minutes, it waits for only briefly and then tells its
//     caller to come back (ErrBusy). Acquire holds it awake while a call
//     runs.
//   - Sync and ReconcileNode apply what a node reports to the rows: Sync
//     one sandbox after a pod event, ReconcileNode a node's whole inventory
//     against a snapshot of the rows taken before it was listed.
//   - Tombstones delete pods that belong to no row any more: a deleted
//     sandbox's, a stale record of an earlier incarnation, an orphan. Their
//     processing checks the pod's incarnation and always runs detached.
//   - Bookings follow resource use: a pod that uses a node is booked there,
//     past the gates if reality says so.
//   - Loops are the leader's policies: the down sweep, lost grace, idle
//     pause, hibernate-after, drain, repair, tombstone processing, the
//     release and takeover of stale ops, and the clean-up of holds and
//     projects.
package sandbox
