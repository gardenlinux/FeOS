// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

// The sizes of the Sync stream's messages. The control plane bounds one
// environment far below PartBytes (table.MaxEnvBytes), so that no
// tenant's environments can make a message too large for every ingress.
const (
	// MaxMessageBytes is the largest message an ingress receives (its
	// calls' MaxCallRecvMsgSize). A larger one fails the stream with
	// RESOURCE_EXHAUSTED, and every new stream with it.
	MaxMessageBytes = 32 << 20
	// PartBytes is the most a message of the Sync stream takes coded
	// when ferund sends it: a larger Table goes in parts (Split).
	PartBytes = 4 << 20
)

// partFrame is what a part takes beside its environments, removals,
// mesh, self, challenges and visit keys, generously: its braces, seq, whole, more,
// sentAt and the keys of its lists.
const partFrame = 160

// Split returns the messages that carry t, each taking at most limit
// bytes coded (Size): t itself when it fits, else parts with the Seqs
// t.Seq, t.Seq+1, …, every one but the last with More (see Table). The
// first part carries t's Whole, Mesh, Self, Challenges, VisitKeys and
// SentAt, every later one SentAt; t's environments, then its removals,
// follow in order, as many in each part as fit, and at least one. A part
// takes more than limit only when one environment alone (in the first
// part: with the mesh, self, the challenges and the visit keys) does. The parts share t's
// slices and values: t must not change while they are used.
func Split(t *Table, limit int) ([]*Table, error) {
	base := partFrame
	if t.Mesh != nil {
		n, err := Size(t.Mesh)
		if err != nil {
			return nil, err
		}
		base += n + len(`,"mesh":`)
	}
	if t.Self != nil {
		n, err := Size(t.Self)
		if err != nil {
			return nil, err
		}
		base += n + len(`,"self":`)
	}
	if t.Challenges != nil {
		n, err := Size(t.Challenges)
		if err != nil {
			return nil, err
		}
		base += n + len(`,"challenges":`)
	}
	if t.VisitKeys != nil {
		n, err := Size(t.VisitKeys)
		if err != nil {
			return nil, err
		}
		base += n + len(`,"visitKeys":`)
	}
	// Each item with the comma before it.
	envSizes := make([]int, len(t.Envs))
	total := base
	for i := range t.Envs {
		n, err := Size(&t.Envs[i])
		if err != nil {
			return nil, err
		}
		envSizes[i] = n + 1
		total += n + 1
	}
	idSizes := make([]int, len(t.Removed))
	for i, id := range t.Removed {
		n, err := Size(id)
		if err != nil {
			return nil, err
		}
		idSizes[i] = n + 1
		total += n + 1
	}
	if total <= limit {
		return []*Table{t}, nil
	}

	parts := []*Table{{Seq: t.Seq, Whole: t.Whole, Mesh: t.Mesh, Self: t.Self, Challenges: t.Challenges, VisitKeys: t.VisitKeys,
		SentAt: t.SentAt}}
	used, items := base, 0
	envFrom, idFrom := 0, 0
	// cut gives the current part the environments before envTo and the
	// removals before idTo that no earlier part has.
	cut := func(envTo, idTo int) {
		p := parts[len(parts)-1]
		p.Envs, p.Removed = window(t.Envs, envFrom, envTo), window(t.Removed, idFrom, idTo)
		envFrom, idFrom = envTo, idTo
	}
	// next marks the current part More and begins the next one.
	next := func() {
		p := parts[len(parts)-1]
		p.More = true
		parts = append(parts, &Table{Seq: p.Seq + 1, SentAt: t.SentAt})
		used, items = partFrame, 0
	}
	for i, n := range envSizes {
		if items > 0 && used+n > limit {
			cut(i, 0)
			next()
		}
		used += n
		items++
	}
	for i, n := range idSizes {
		if items > 0 && used+n > limit {
			cut(len(t.Envs), i)
			next()
		}
		used += n
		items++
	}
	cut(len(t.Envs), len(t.Removed))
	return parts, nil
}

// window returns s[from:to], capped so that an append cannot reach the
// rest of s; nil when it is empty.
func window[T any](s []T, from, to int) []T {
	if from == to {
		return nil
	}
	return s[from:to:to]
}
