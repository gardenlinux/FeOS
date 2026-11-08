// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// ErrSequence is the error of Apply on a delta that does not follow the
// state's last message: a message was lost, and only a new stream, which
// starts whole, repairs the state.
var ErrSequence = errors.New("wire: table out of sequence")

// State is an ingress's table as the Sync stream built it: the
// environments by ID, the mesh, the ingress itself and the pending ACME
// challenges, as of message Seq of the current stream. The ingress caches
// it as JSON.
type State struct {
	Seq        uint64         `json:"seq"`
	Envs       map[string]Env `json:"envs"`
	Mesh       Mesh           `json:"mesh"`
	Self       Self           `json:"self"`
	Challenges []Challenge    `json:"challenges,omitempty"`

	// parts are the parts of a split message received so far, joined
	// into one with the last one's Seq; nil: none. Not cached, not
	// cloned.
	parts *Table
}

// NewState returns an empty state: no message applied yet.
func NewState() *State {
	return &State{Envs: map[string]Env{}}
}

// Apply applies a message of the Sync stream. A whole table replaces the
// environments, and its Seq starts the sequence again. A delta must carry
// Seq s.Seq+1, else Apply returns an error wrapping ErrSequence and
// leaves s as it was; its Envs are put by ID, then its Removed are
// deleted. In either, a non-nil Mesh or Self replaces the state's and a
// nil one keeps it. Challenges, when not nil, replace the state's; a
// whole table without them leaves none.
//
// A message in parts (Table.More) is applied once its last part has
// arrived, all parts as one message, and not at all before: Apply keeps
// each earlier part and returns nil for it. A part must carry the Seq
// after the part before it, else Apply returns an error wrapping
// ErrSequence and drops the parts it kept; a whole part drops them as
// well, as a new stream does.
//
// Apply returns the message it applied: t, or the parts joined into one
// message with the last part's Seq. It keeps t's slices and maps: t must
// not change afterwards.
func (s *State) Apply(t *Table) (*Table, error) {
	if t == nil {
		return nil, errors.New("wire: no table")
	}
	m := t
	switch {
	case t.Whole:
		s.parts = nil
	case s.parts != nil:
		if t.Seq != s.parts.Seq+1 {
			last := s.parts.Seq
			s.parts = nil
			return nil, fmt.Errorf("%w: part %d after %d", ErrSequence, t.Seq, last)
		}
		m = s.parts.join(t)
	case t.Seq != s.Seq+1:
		return nil, fmt.Errorf("%w: message %d after %d", ErrSequence, t.Seq, s.Seq)
	}
	if m.More {
		if m == t {
			m = t.firstPart()
		}
		s.parts = m
		return nil, nil
	}
	s.parts = nil
	if m.Whole || s.Envs == nil {
		s.Envs = make(map[string]Env, len(m.Envs))
	}
	for _, e := range m.Envs {
		s.Envs[e.ID] = e
	}
	for _, id := range m.Removed {
		delete(s.Envs, id)
	}
	if m.Mesh != nil {
		s.Mesh = *m.Mesh
	}
	if m.Self != nil {
		s.Self = *m.Self
	}
	switch {
	case m.Challenges != nil:
		s.Challenges = m.Challenges.Items
	case m.Whole:
		s.Challenges = nil
	}
	s.Seq = m.Seq
	return m, nil
}

// firstPart returns a copy of t, the first part of a message in parts,
// with lists of its own for the next parts to join.
func (t *Table) firstPart() *Table {
	c := *t
	c.Envs, c.Removed = slices.Clone(t.Envs), slices.Clone(t.Removed)
	return &c
}

// join adds the next part n to the parts t (from firstPart) and returns
// t.
func (t *Table) join(n *Table) *Table {
	t.Seq, t.More = n.Seq, n.More
	t.Envs = append(t.Envs, n.Envs...)
	t.Removed = append(t.Removed, n.Removed...)
	if n.Mesh != nil {
		t.Mesh = n.Mesh
	}
	if n.Self != nil {
		t.Self = n.Self
	}
	if n.Challenges != nil {
		t.Challenges = n.Challenges
	}
	return t
}

// Clone returns a deep copy of s's table, which shares nothing with it,
// without the parts it keeps.
func (s *State) Clone() *State {
	if s == nil {
		return nil
	}
	c := &State{
		Seq:        s.Seq,
		Envs:       make(map[string]Env, len(s.Envs)),
		Mesh:       s.Mesh.clone(),
		Self:       s.Self,
		Challenges: slices.Clone(s.Challenges),
	}
	for id, e := range s.Envs {
		c.Envs[id] = e.clone()
	}
	return c
}

func (m Mesh) clone() Mesh {
	c := Mesh{Version: m.Version}
	if m.Peers != nil {
		c.Peers = make([]Peer, len(m.Peers))
		for i, p := range m.Peers {
			p.PublicKey = slices.Clone(p.PublicKey)
			c.Peers[i] = p
		}
	}
	return c
}

func (e Env) clone() Env {
	e.Hosts = slices.Clone(e.Hosts)
	e.Routes = slices.Clone(e.Routes)
	for i := range e.Routes {
		e.Routes[i].From = slices.Clone(e.Routes[i].From)
	}
	if e.Domains != nil {
		ds := make([]Domain, len(e.Domains))
		for i, d := range e.Domains {
			d.Chain, d.Key = copyBytesList(d.Chain), slices.Clone(d.Key)
			ds[i] = d
		}
		e.Domains = ds
	}
	if e.Services != nil {
		svcs := make([]Service, len(e.Services))
		for i, svc := range e.Services {
			if svc.Endpoints != nil {
				eps := make([]Endpoint, len(svc.Endpoints))
				for j, ep := range svc.Endpoints {
					ep.Ports = maps.Clone(ep.Ports)
					eps[j] = ep
				}
				svc.Endpoints = eps
			}
			svcs[i] = svc
		}
		e.Services = svcs
	}
	return e
}
