// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// bigEnv is an environment with routes whose paths are n characters of
// '&', which HTML escaping would make six bytes each.
func bigEnv(id string, routes, n int) Env {
	e := env(id, "h-"+id)
	e.Routes = nil
	for i := range routes {
		e.Routes = append(e.Routes, Route{Name: fmt.Sprintf("r%d", i), Path: "/" + strings.Repeat("&", n) + fmt.Sprint(i),
			Service: "web", Port: "http"})
	}
	return e
}

func mustSize(t *testing.T, v any) int {
	t.Helper()
	n, err := Size(v)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The codec does not escape HTML, and Size is what Marshal codes.
func TestCodecNoHTMLEscape(t *testing.T) {
	in := &Table{Seq: 1, Envs: []Env{{ID: "e1", Routes: []Route{{Path: "/a&b<c>d"}}}}, SentAt: t0}
	b, err := Codec{}.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"path":"/a&b<c>d"`) || strings.HasSuffix(string(b), "\n") {
		t.Errorf("coded %s", b)
	}
	if n := mustSize(t, in); n != len(b) {
		t.Errorf("Size %d, Marshal %d bytes", n, len(b))
	}
	out := new(Table)
	if err := (Codec{}).Unmarshal(b, out); err != nil || !reflect.DeepEqual(in, out) {
		t.Errorf("round trip %+v %v", out, err)
	}
	// An escaped message from elsewhere still decodes.
	if err := (Codec{}).Unmarshal([]byte(`{"envs":[{"routes":[{"path":"/a&b"}]}]}`), out); err != nil ||
		out.Envs[0].Routes[0].Path != "/a&b" {
		t.Errorf("escaped: %+v %v", out, err)
	}
	if _, err := Size(func() {}); err == nil {
		t.Error("a func sized")
	}
}

// joinParts is what the parts carry together.
func joinParts(parts []*Table) (envs []Env, removed []string) {
	for _, p := range parts {
		envs = append(envs, p.Envs...)
		removed = append(removed, p.Removed...)
	}
	return envs, removed
}

// A table that fits is itself; a larger one goes in parts of consecutive
// seqs, More on all but the last, Whole, Mesh and Self on the first, the
// environments then the removals in order, each part within the limit
// and with something in it.
func TestSplit(t *testing.T) {
	mesh := &Mesh{Version: 3, Peers: []Peer{{Name: "n1", PublicKey: []byte{1, 2, 3}}}}
	self := &Self{Name: "ingress-0", AppsZone: "apps.test"}
	small := &Table{Seq: 1, Whole: true, Envs: []Env{env("a", "1")}, Mesh: mesh, Self: self, SentAt: t0}
	parts, err := Split(small, PartBytes)
	if err != nil || len(parts) != 1 || parts[0] != small {
		t.Fatalf("small: %+v %v", parts, err)
	}

	var envs []Env
	for i := range 20 {
		envs = append(envs, bigEnv(fmt.Sprintf("e%02d", i), 10, 100))
	}
	// The challenges go with the first part, as the mesh and self do.
	challenges := &Challenges{}
	for i := range 20 {
		challenges.Items = append(challenges.Items, Challenge{Token: fmt.Sprintf("token-%02d", i), Host: "a.example",
			KeyAuth: strings.Repeat("k", 80)})
	}
	// The visit keys go with the first part too.
	keys := testVisitKeys()
	var removed []string
	for i := range 1000 {
		removed = append(removed, fmt.Sprintf("gone-%04d", i))
	}
	one := mustSize(t, &envs[0])
	limit := 4*one + partFrame
	for _, tc := range []struct {
		name string
		t    *Table
	}{
		{"whole", &Table{Seq: 1, Whole: true, Envs: envs, Mesh: mesh, Self: self, Challenges: challenges, VisitKeys: keys,
			SentAt: t0}},
		{"delta", &Table{Seq: 7, Envs: envs, Removed: removed, Self: self, Challenges: challenges, SentAt: t0}},
		{"keys", &Table{Seq: 8, Envs: envs, VisitKeys: keys, SentAt: t0}},
		{"removals", &Table{Seq: 9, Removed: removed, SentAt: t0}},
	} {
		parts, err := Split(tc.t, limit)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(parts) < 2 {
			t.Fatalf("%s: %d parts", tc.name, len(parts))
		}
		for i, p := range parts {
			if p.Seq != tc.t.Seq+uint64(i) || p.More != (i < len(parts)-1) || !p.SentAt.Equal(t0) {
				t.Errorf("%s part %d: seq %d more %v sentAt %v", tc.name, i, p.Seq, p.More, p.SentAt)
			}
			first := i == 0
			if p.Whole != (first && tc.t.Whole) || (p.Mesh == tc.t.Mesh) != (first || tc.t.Mesh == nil) ||
				(p.Self == tc.t.Self) != (first || tc.t.Self == nil) || (!first && (p.Mesh != nil || p.Self != nil)) {
				t.Errorf("%s part %d: whole %v mesh %v self %v", tc.name, i, p.Whole, p.Mesh, p.Self)
			}
			if (p.Challenges == tc.t.Challenges) != (first || tc.t.Challenges == nil) || (!first && p.Challenges != nil) {
				t.Errorf("%s part %d: challenges %v", tc.name, i, p.Challenges)
			}
			if (p.VisitKeys == tc.t.VisitKeys) != (first || tc.t.VisitKeys == nil) || (!first && p.VisitKeys != nil) {
				t.Errorf("%s part %d: visit keys %v", tc.name, i, p.VisitKeys)
			}
			if len(p.Envs)+len(p.Removed) == 0 {
				t.Errorf("%s part %d is empty", tc.name, i)
			}
			b, err := Codec{}.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if len(b) > limit {
				t.Errorf("%s part %d: %d bytes, limit %d", tc.name, i, len(b), limit)
			}
		}
		gotEnvs, gotRemoved := joinParts(parts)
		if !reflect.DeepEqual(gotEnvs, tc.t.Envs) || !reflect.DeepEqual(gotRemoved, tc.t.Removed) {
			t.Errorf("%s: the parts carry other environments or removals", tc.name)
		}
		// A part's lists end where they end: appending to one does not
		// reach into the next part.
		before := lists(parts)
		parts[0].Envs = append(parts[0].Envs, env("x", "1"))
		parts[0].Removed = append(parts[0].Removed, "x")
		if after := lists(parts[1:]); !reflect.DeepEqual(after, before[1:]) {
			t.Errorf("%s: appending to a part changed the next one", tc.name)
		}
	}

	// An environment larger than the limit goes alone, in a part of its
	// own; the parts around it are as usual.
	huge := bigEnv("huge", 100, 100)
	mixed := &Table{Seq: 1, Whole: true, Envs: []Env{envs[0], huge, envs[1]}, SentAt: t0}
	parts, err = Split(mixed, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 || len(parts[1].Envs) != 1 || parts[1].Envs[0].ID != "huge" {
		t.Fatalf("huge: %d parts %+v", len(parts), parts)
	}
}

// lists copies the lists of parts, to compare them later.
func lists(parts []*Table) [][2]any {
	var out [][2]any
	for _, p := range parts {
		out = append(out, [2]any{append([]Env(nil), p.Envs...), append([]string(nil), p.Removed...)})
	}
	return out
}

// applyAll applies messages in order and returns what each applied.
func applyAll(t *testing.T, s *State, msgs []*Table) []*Table {
	t.Helper()
	var out []*Table
	for _, m := range msgs {
		got, err := s.Apply(m)
		if err != nil {
			t.Fatalf("Apply seq %d: %v", m.Seq, err)
		}
		out = append(out, got)
	}
	return out
}

// A message in parts is applied once its last part arrives, all parts as
// one, and not at all before; the parts carry the sequence.
func TestStateParts(t *testing.T) {
	s := NewState()
	mustApply(t, s, &Table{Seq: 1, Whole: true, Envs: []Env{env("old", "1")}, Self: &Self{Name: "before"}})
	mustApply(t, s, &Table{Seq: 2})

	// A new stream's whole table in three parts.
	parts := []*Table{
		{Seq: 1, Whole: true, More: true, Envs: []Env{env("a", "1"), env("b", "1")}, Mesh: &Mesh{Version: 5},
			Self: &Self{Name: "ingress-0"}},
		{Seq: 2, More: true, Envs: []Env{env("c", "1")}},
		{Seq: 3, Envs: []Env{env("d", "1")}, Removed: []string{"a"}},
	}
	first := append([]Env(nil), parts[0].Envs...)
	before := s.Clone()
	for i, p := range parts[:2] {
		got, err := s.Apply(p)
		if err != nil || got != nil {
			t.Fatalf("part %d: %+v %v", i, got, err)
		}
		if !reflect.DeepEqual(s.Clone(), before) {
			t.Fatalf("part %d changed the state: %+v", i, s)
		}
	}
	got, err := s.Apply(parts[2])
	if err != nil || got == nil {
		t.Fatalf("last part: %+v %v", got, err)
	}
	if got.Seq != 3 || !got.Whole || got.More || len(got.Envs) != 4 || !reflect.DeepEqual(got.Removed, []string{"a"}) ||
		got.Mesh.Version != 5 || got.Self.Name != "ingress-0" {
		t.Fatalf("joined %+v", got)
	}
	// Puts before removes, over all parts; the whole table replaced the
	// old environments.
	if ids := envIDs(s); !reflect.DeepEqual(ids, map[string]string{"b": "1", "c": "1", "d": "1"}) || s.Seq != 3 ||
		s.Mesh.Version != 5 || s.Self.Name != "ingress-0" {
		t.Fatalf("state %+v", s)
	}
	if !reflect.DeepEqual(parts[0].Envs, first) {
		t.Error("joining changed the first part")
	}

	// A delta in two parts, then a single message: the sequence goes on
	// from the last part.
	out := applyAll(t, s, []*Table{
		{Seq: 4, More: true, Envs: []Env{env("e", "1")}},
		{Seq: 5, Removed: []string{"b"}, Self: &Self{Name: "ingress-0", AdmittedVersion: 7}},
		{Seq: 6, Envs: []Env{env("c", "2")}},
	})
	if out[0] != nil || out[1] == nil || out[1].Seq != 5 || len(out[1].Envs) != 1 || out[2] == nil {
		t.Fatalf("applied %+v", out)
	}
	if ids := envIDs(s); !reflect.DeepEqual(ids, map[string]string{"c": "2", "d": "1", "e": "1"}) || s.Seq != 6 ||
		s.Self.AdmittedVersion != 7 {
		t.Fatalf("state %+v", s)
	}

	// A part out of sequence: ErrSequence, nothing applied, the parts
	// dropped, so their continuation is out of sequence too.
	before = s.Clone()
	if got, err := s.Apply(&Table{Seq: 7, More: true, Envs: []Env{env("x", "1")}}); err != nil || got != nil {
		t.Fatalf("first part: %+v %v", got, err)
	}
	if _, err := s.Apply(&Table{Seq: 9, Envs: []Env{env("y", "1")}}); !errors.Is(err, ErrSequence) {
		t.Fatalf("a gap between parts: %v", err)
	}
	if _, err := s.Apply(&Table{Seq: 8, Envs: []Env{env("y", "1")}}); !errors.Is(err, ErrSequence) {
		t.Fatalf("the dropped parts' continuation: %v", err)
	}
	if !reflect.DeepEqual(s, before) {
		t.Fatalf("a refused part changed the state: %+v", s)
	}
	// A heartbeat that follows is applied as ever.
	mustApply(t, s, &Table{Seq: 7})

	// A whole part drops the parts kept, as a new stream does.
	if got, err := s.Apply(&Table{Seq: 8, More: true, Envs: []Env{env("z", "1")}}); err != nil || got != nil {
		t.Fatalf("first part: %+v %v", got, err)
	}
	mustApply(t, s, &Table{Seq: 1, Whole: true, Envs: []Env{env("w", "1")}})
	if ids := envIDs(s); !reflect.DeepEqual(ids, map[string]string{"w": "1"}) || s.Seq != 1 {
		t.Fatalf("state %+v", s)
	}
	mustApply(t, s, &Table{Seq: 2})
}

// Splitting and applying the parts gives the state that applying the
// table whole gives.
func TestSplitApply(t *testing.T) {
	var envs []Env
	for i := range 30 {
		envs = append(envs, bigEnv(fmt.Sprintf("e%02d", i), 3, 50+i))
	}
	limit := 3 * mustSize(t, &envs[0])
	msgs := []*Table{
		{Seq: 1, Whole: true, Envs: envs, Mesh: &Mesh{Version: 2}, Self: &Self{Name: "ingress-0"},
			Challenges: &Challenges{Items: []Challenge{{Token: "t1", Host: "a.example", KeyAuth: "t1.k"}}}, VisitKeys: testVisitKeys()},
		{Seq: 2, Envs: envs[:10], Removed: []string{"e20", "e21", "e22", "e23"},
			Challenges: &Challenges{Items: []Challenge{{Token: "t2", Host: "b.example", KeyAuth: "t2.k"}}},
			VisitKeys:  &VisitKeys{TokenKID: "other", TokenKey: []byte{9}, ChallengeKID: "other", ChallengeKey: []byte{8}}},
		{Seq: 3},
		{Seq: 4, Removed: []string{"e00", "e01", "e02", "e03", "e04", "e05", "e06", "e07", "e08", "e09"}},
	}
	whole, split := NewState(), NewState()
	seq := uint64(0)
	for _, m := range msgs {
		mustApply(t, whole, m)
		c := *m
		c.Seq = seq + 1
		parts, err := Split(&c, limit)
		if err != nil {
			t.Fatal(err)
		}
		applied := applyAll(t, split, parts)
		for i, a := range applied {
			if (a == nil) != (i < len(parts)-1) {
				t.Fatalf("message %d part %d applied %+v", m.Seq, i, a)
			}
		}
		seq = parts[len(parts)-1].Seq
	}
	if seq <= uint64(len(msgs)) {
		t.Fatalf("nothing was split: last seq %d", seq)
	}
	split.Seq = whole.Seq
	if !reflect.DeepEqual(split, whole) {
		t.Errorf("split\n got %+v\nwant %+v", envIDs(split), envIDs(whole))
	}
}

// F13: a table larger than an ingress takes in one message fails every
// stream, whole, with RESOURCE_EXHAUSTED; split, it arrives within the
// receive limit and is applied whole. The limits are scaled down from
// MaxMessageBytes and PartBytes by 32, for the test's speed.
func TestSyncTableLargerThanAMessage(t *testing.T) {
	const recvLimit, partLimit = MaxMessageBytes / 32, PartBytes / 32
	var envs []Env
	for i := range 17 {
		envs = append(envs, bigEnv(fmt.Sprintf("e%02d", i), 100, 600))
	}
	whole := &Table{Seq: 1, Whole: true, Envs: envs, Mesh: &Mesh{Version: 1}, Self: &Self{Name: "ingress-0"}, SentAt: t0}
	if n := mustSize(t, whole); n <= recvLimit {
		t.Fatalf("the table takes %d bytes, not more than %d", n, recvLimit)
	}
	recv := func(tables []*Table) (*State, error) {
		c := NewClient(serve(t, &fakeServer{tables: tables}))
		stream, err := c.Sync(testContext(t), grpc.MaxCallRecvMsgSize(recvLimit))
		if err != nil {
			return nil, err
		}
		st := NewState()
		for range tables {
			got, err := stream.Recv()
			if err != nil {
				return st, err
			}
			if _, err := st.Apply(got); err != nil {
				return st, err
			}
		}
		return st, nil
	}

	_, err := recv([]*Table{whole})
	if st, _ := status.FromError(err); st.Code() != codes.ResourceExhausted || !strings.Contains(st.Message(), "larger than max") {
		t.Fatalf("one message: %v, want RESOURCE_EXHAUSTED", err)
	}

	parts, err := Split(whole, partLimit)
	if err != nil {
		t.Fatal(err)
	}
	st, err := recv(parts)
	if err != nil {
		t.Fatalf("in %d parts: %v", len(parts), err)
	}
	if len(st.Envs) != len(envs) || st.Seq != uint64(len(parts)) || st.Self.Name != "ingress-0" {
		t.Fatalf("state: %d envs, seq %d of %d parts", len(st.Envs), st.Seq, len(parts))
	}
}

// The sizes keep their order: an environment of the largest size the
// control plane sends (table.MaxEnvBytes, 2 MiB) fits a part, and a part
// fits a message many times.
func TestSizes(t *testing.T) {
	if PartBytes < 2<<20 || PartBytes*4 > MaxMessageBytes {
		t.Errorf("PartBytes %d, MaxMessageBytes %d", PartBytes, MaxMessageBytes)
	}
}
