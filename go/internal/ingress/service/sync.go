// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/table"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The Sync stream's timings.
const (
	// defaultHeartbeat is how often a stream sends an empty table and
	// records the ingress as seen, which keeps its holds' fence away
	// (D11) and shows the ingress that the stream lives.
	defaultHeartbeat = 10 * time.Second
	// tableWait bounds the wait for this instance's table when a stream
	// opens.
	tableWait = 10 * time.Second
	// defaultRowTimeout bounds a stream's read of the ingress's row, and
	// the heartbeat's write of it (SeenIngress).
	defaultRowTimeout = 5 * time.Second
	// streamEndWait bounds how long RemoveIngress waits for the removed
	// ingress's streams to end before it closes its connections;
	// streamFlush is the time their status then has to reach the
	// ingress.
	streamEndWait = time.Second
	streamFlush   = 100 * time.Millisecond
)

// The texts the streams end with.
const (
	stoppingText   = "instance stopping"
	notReadyText   = "the table is not ready on this instance"
	unhealthyText  = "this instance cannot read the database"
	helloFirstText = "call Hello first"
)

// errStopping is what a stream, a Wake and a Hold answer on an instance
// that stops (D18): the ingress goes to another instance.
func errStopping() error { return status.Error(codes.Unavailable, stoppingText) }

// removedText is the refusal of a removed ingress, on which it declares
// its identity lost (D19).
func removedText(name string) string {
	return fmt.Sprintf("ingress %s was removed; join it again", name)
}

// syncStream is a Sync stream on this instance.
type syncStream struct {
	name   string
	cancel context.CancelCauseFunc
	// dirty: the ingress's row may have changed; the stream reads it
	// again.
	dirty chan struct{}
	// done is closed when the stream has ended.
	done chan struct{}
}

// markDirty has the stream read its ingress's row again.
func (st *syncStream) markDirty() {
	select {
	case st.dirty <- struct{}{}:
	default:
	}
}

// Sync streams the ingress's table (§6.4 of the ingress's design), after
// a Hello on the same connection (FAILED_PRECONDITION "call Hello first"
// before). It uses the instance's table (table.Builder) and waits at most
// tableWait for it (UNAVAILABLE "the table is not ready on this
// instance"), reads the ingress's row (removed: PERMISSION_DENIED; no
// address on the pods' mesh: FAILED_PRECONDITION) and sends the whole
// table, seq 1: every environment, the mesh's nodes, the ingress itself
// (Self: its address, the apps zone and the version of ferun-ingress at
// which its address entered the group) and the pending ACME challenges.
// Then it sends a delta whenever the table changed, with the mesh when
// its version changed, Self when it changed and the challenges, all of
// them, when they changed: the environments put and removed
// since the last message, so that changes during a blocked Send coalesce
// into one message. A table, whole or delta, that takes more than
// wire.PartBytes goes in parts (wire.Split), each of its own seq, sent
// one after the other. Every heartbeat it sends an empty table and
// records the ingress as seen (SeenIngress), and reads its row again, as
// it does when ferun_ingress names the ingress (onIngressChanged). It
// records the tokens of the challenges it sent whenever they change
// (recordChallenges).
//
// The stream ends with UNAVAILABLE "this instance cannot read the
// database" once the table is unhealthy (no read for 30 s, D18), with
// UNAVAILABLE "instance stopping" at BeginStopping, with
// PERMISSION_DENIED when the ingress is removed, and when a Send fails.
func (s *Service) Sync(_ *wire.SyncRequest, stream wire.SyncServer) error {
	p, err := ingressPeer(stream.Context())
	if err != nil {
		return err
	}
	if s.stopping.Load() {
		return errStopping()
	}
	if !s.saidHello(p) {
		return status.Error(codes.FailedPrecondition, helloFirstText)
	}
	ctx, cancel := context.WithCancelCause(stream.Context())
	defer cancel(nil)
	st := &syncStream{name: p.Name, cancel: cancel, dirty: make(chan struct{}, 1), done: make(chan struct{})}
	defer close(st.done)
	if !s.register(st) {
		return errStopping()
	}
	defer s.unregister(st)
	release := s.tbl.Use()
	defer release()
	return s.serveSync(ctx, p, st, stream)
}

// serveSync is Sync's stream after its checks: ctx ends with the stream,
// with the cause the stream is ended with.
func (s *Service) serveSync(ctx context.Context, p nodelink.IngressPeer, st *syncStream, stream wire.SyncServer) error {
	wctx, cancel := context.WithTimeout(ctx, tableWait)
	snap, err := s.tbl.Wait(wctx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return streamEnded(ctx)
		}
		s.rl.warn("sync-table", "ingress stream refused: the table is not ready", "ingress", p.Name)
		return status.Error(codes.Unavailable, notReadyText)
	}
	if !snap.Healthy {
		return status.Error(codes.Unavailable, unhealthyText)
	}
	self, err := s.readSelf(ctx, p)
	if err != nil {
		return err
	}
	envs, _ := table.Diff(nil, snap)
	mesh := snap.Mesh
	first := &wire.Table{Seq: 1, Whole: true, Envs: envs, Mesh: &mesh, Self: &self,
		Challenges: &wire.Challenges{Items: snap.Challenges}, SentAt: time.Now()}
	last, err := s.send(ctx, stream, first)
	if err != nil {
		return err
	}
	sent := &syncState{seq: last, sent: table.Hashes(snap.Envs), mesh: snap.Mesh.Version, self: self,
		challenges: snap.Challenges}
	s.recordChallenges(ctx, p.Name, sent)
	s.log.Debug("ingress stream opened", "ingress", p.Name, "peer", p.Addr, "envs", len(envs), "parts", last)
	want := self
	tick := time.NewTicker(s.heartbeat)
	defer tick.Stop()
	for {
		changed := s.tbl.Changed()
		cur := s.tbl.Current()
		if cur == nil || !cur.Healthy {
			s.rl.warn("sync-unhealthy", "ingress stream ended: this instance cannot read the database", "ingress", p.Name)
			return status.Error(codes.Unavailable, unhealthyText)
		}
		if t := sent.delta(cur, want); t != nil {
			t.SentAt = time.Now()
			last, err := s.send(ctx, stream, t)
			if err != nil {
				return err
			}
			sent.record(t, last)
		}
		s.recordChallenges(ctx, p.Name, sent)
		select {
		case <-ctx.Done():
			return streamEnded(ctx)
		case <-changed:
		case <-st.dirty:
			if want, err = s.rereadSelf(ctx, p, want); err != nil {
				return err
			}
		case <-tick.C:
			sent.seq++
			if err := stream.Send(&wire.Table{Seq: sent.seq, SentAt: time.Now()}); err != nil {
				return sendFailed(ctx, err)
			}
			s.seen(ctx, p.Name)
			s.recordChallenges(ctx, p.Name, sent)
			if want, err = s.rereadSelf(ctx, p, want); err != nil {
				return err
			}
		}
	}
}

// syncState is what a stream sent: its last seq, the environments' hashes
// by ID, the mesh's version, Self and the challenges; and the challenges'
// tokens last recorded as the ingress's (recordChallenges; nil: none
// yet).
type syncState struct {
	seq        uint64
	sent       map[string]string
	mesh       uint64
	self       wire.Self
	challenges []wire.Challenge
	recorded   []string
}

// delta returns the next message, which brings the ingress from what the
// stream sent to cur and self; nil when nothing changed.
func (st *syncState) delta(cur *table.Snapshot, self wire.Self) *wire.Table {
	puts, removed := table.Diff(st.sent, cur)
	t := &wire.Table{Envs: puts, Removed: removed}
	if cur.Mesh.Version != st.mesh {
		m := cur.Mesh
		t.Mesh = &m
	}
	if self != st.self {
		t.Self = &self
	}
	if !slices.Equal(cur.Challenges, st.challenges) {
		t.Challenges = &wire.Challenges{Items: cur.Challenges}
	}
	if len(puts) == 0 && len(removed) == 0 && t.Mesh == nil && t.Self == nil && t.Challenges == nil {
		return nil
	}
	t.Seq = st.seq + 1
	return t
}

// record notes that t was sent, its last part with seq last.
func (st *syncState) record(t *wire.Table, last uint64) {
	st.seq = last
	for _, e := range t.Envs {
		st.sent[e.ID] = e.Hash
	}
	for _, id := range t.Removed {
		delete(st.sent, id)
	}
	if t.Mesh != nil {
		st.mesh = t.Mesh.Version
	}
	if t.Self != nil {
		st.self = *t.Self
	}
	if t.Challenges != nil {
		st.challenges = t.Challenges.Items
	}
}

// recordChallenges records the challenges the stream's ingress holds:
// the tokens of those it sent, when they differ from the ones recorded
// (store.RecordIngressChallenges), at the stream's start whatever they
// are. An ACME order accepts its challenge only once every connected
// ingress holds its token. A failed write is tried again at the next
// change and at every heartbeat.
func (s *Service) recordChallenges(ctx context.Context, name string, st *syncState) {
	tokens := make([]string, 0, len(st.challenges))
	for _, c := range st.challenges {
		tokens = append(tokens, c.Token)
	}
	if st.recorded != nil && slices.Equal(tokens, st.recorded) {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, s.rowTimeout)
	defer cancel()
	if err := s.st.RecordIngressChallenges(rctx, name, tokens); err != nil {
		if ctx.Err() == nil {
			s.rl.warn("challenges", "cannot record the challenges an ingress holds", "ingress", name, "err", err)
		}
		return
	}
	st.recorded = tokens
}

// send sends t on the stream, in parts of at most s.partBytes when it is
// larger (wire.Split), and returns the seq of its last part.
func (s *Service) send(ctx context.Context, stream wire.SyncServer, t *wire.Table) (uint64, error) {
	parts, err := wire.Split(t, s.partBytes)
	if err != nil {
		return 0, status.Errorf(codes.Internal, "code the table: %v", err)
	}
	for _, part := range parts {
		if err := stream.Send(part); err != nil {
			return 0, sendFailed(ctx, err)
		}
	}
	return parts[len(parts)-1].Seq, nil
}

// readSelf reads the ingress's row for its stream and returns Self:
// PERMISSION_DENIED when the ingress was removed (or has no row),
// FAILED_PRECONDITION when it has no address on the pods' mesh,
// UNAVAILABLE when the row cannot be read, within s.rowTimeout too.
func (s *Service) readSelf(ctx context.Context, p nodelink.IngressPeer) (wire.Self, error) {
	rctx, cancel := context.WithTimeout(ctx, s.rowTimeout)
	defer cancel()
	g, err := s.st.GetIngress(rctx, p.Name)
	switch {
	case errors.Is(err, store.ErrNoRow):
		return wire.Self{}, status.Errorf(codes.PermissionDenied, "certificate %s is not ingress %s's", p.Serial, p.Name)
	case err != nil:
		if ctx.Err() != nil {
			return wire.Self{}, streamEnded(ctx)
		}
		return wire.Self{}, dbError(ctx, err, "read the ingress")
	case g.Status == model.IngressRemoved:
		return wire.Self{}, status.Error(codes.PermissionDenied, removedText(p.Name))
	case g.MeshAddress == "":
		return wire.Self{}, status.Errorf(codes.FailedPrecondition, "ingress %s has no address on the pods' mesh; %s",
			p.Name, helloFirstText)
	}
	return wire.Self{Name: g.Name, Address: g.MeshAddress, AppsZone: s.cfg.AppsZone,
		TenantZone: s.cfg.TenantZone, AdmittedVersion: uint64(max(g.GroupVersion, 0))}, nil
}

// rereadSelf reads the ingress's row again for its stream: Self as it is
// now, or the stream's end (a removed ingress); a failed read keeps last,
// and the next heartbeat reads again.
func (s *Service) rereadSelf(ctx context.Context, p nodelink.IngressPeer, last wire.Self) (wire.Self, error) {
	self, err := s.readSelf(ctx, p)
	if err == nil {
		return self, nil
	}
	if ctx.Err() != nil {
		return last, streamEnded(ctx)
	}
	if st, ok := refused(err); ok {
		if st.Code() == codes.PermissionDenied {
			s.log.Info("ingress stream ended: the ingress was refused", "ingress", p.Name, "err", st.Message())
		}
		return last, st.Err()
	}
	s.rl.warn("sync-row", "ingress stream: cannot read the ingress's row; trying again at the heartbeat",
		"ingress", p.Name, "err", err)
	return last, nil
}

// seen records that the ingress was seen (a heartbeat, a Hold), best
// effort.
func (s *Service) seen(ctx context.Context, name string) {
	rctx, cancel := context.WithTimeout(ctx, s.rowTimeout)
	defer cancel()
	if err := s.st.SeenIngress(rctx, name, ""); err != nil && ctx.Err() == nil {
		s.rl.warn("seen", "cannot record that an ingress was seen", "ingress", name, "err", err)
	}
}

// streamEnded is what a stream answers once its ctx is done: the status
// it was ended with (BeginStopping, RemoveIngress), else the ingress's
// own cancellation.
func streamEnded(ctx context.Context) error {
	cause := context.Cause(ctx)
	if _, ok := status.FromError(cause); ok && cause != nil {
		return cause
	}
	return rpcError(cause, "sync")
}

// sendFailed is what a stream answers after a failed Send: the status it
// was ended with when that came first, else the Send's error.
func sendFailed(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return streamEnded(ctx)
	}
	return err
}

// register adds a stream; false once the instance stops.
func (s *Service) register(st *syncStream) bool {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.stopping.Load() {
		return false
	}
	m := s.streams[st.name]
	if m == nil {
		m = map[*syncStream]struct{}{}
		s.streams[st.name] = m
	}
	m[st] = struct{}{}
	return true
}

// unregister takes an ended stream out.
func (s *Service) unregister(st *syncStream) {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if m := s.streams[st.name]; m != nil {
		delete(m, st)
		if len(m) == 0 {
			delete(s.streams, st.name)
		}
	}
}

// streamsOf returns the streams of ingress name, or every stream when
// all.
func (s *Service) streamsOf(name string, all bool) []*syncStream {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	var out []*syncStream
	for n, m := range s.streams {
		if !all && n != name {
			continue
		}
		for st := range m {
			out = append(out, st)
		}
	}
	return out
}

// endStreams ends the streams of ingress name (every stream when all)
// with err and returns the channels closed when each has ended.
func (s *Service) endStreams(name string, all bool, err error) []<-chan struct{} {
	sts := s.streamsOf(name, all)
	done := make([]<-chan struct{}, 0, len(sts))
	for _, st := range sts {
		st.cancel(err)
		done = append(done, st.done)
	}
	return done
}

// awaitStreams waits until every channel of done is closed, at most
// within.
func awaitStreams(done []<-chan struct{}, within time.Duration) {
	timer := time.NewTimer(within)
	defer timer.Stop()
	for _, ch := range done {
		select {
		case <-ch:
		case <-timer.C:
			return
		}
	}
}

// onIngressChanged is told of every change of ingress name that
// ferun_ingress names, before its re-check is asked for: each of its
// streams on this instance reads its row again, and ends with
// PERMISSION_DENIED when the ingress was removed, or sends its new Self.
func (s *Service) onIngressChanged(name string) {
	for _, st := range s.streamsOf(name, false) {
		st.markDirty()
	}
}

// onEveryIngressChanged is onIngressChanged for every ingress: on
// ferun_ingress "*", and when the listener reconnected and may have
// missed a change.
func (s *Service) onEveryIngressChanged() {
	for _, st := range s.streamsOf("", true) {
		st.markDirty()
	}
}

// BeginStopping is §7 step 1 for the ingress (D18): from now on the nodes'
// port closes every new ferun-ingress/1 connection at once
// (nodelink.Server.StopIngress), every Sync stream ends with UNAVAILABLE
// "instance stopping", so that the ingress follows another instance, and
// Wake and Hold answer UNAVAILABLE. The established connections stay
// until the port shuts down. Calling it again changes nothing.
func (s *Service) BeginStopping() {
	if s.links != nil {
		s.links.StopIngress()
	}
	s.streamsMu.Lock()
	first := !s.stopping.Swap(true)
	s.streamsMu.Unlock()
	n := len(s.endStreams("", true, errStopping()))
	if first {
		s.log.Info("ingress: stopping; streams ended, Wake and Hold refused", "streams", n)
	}
}
