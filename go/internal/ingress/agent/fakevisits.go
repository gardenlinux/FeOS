// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/visit"
)

// fakeVisits is the FakeControlPlane's side of visits (ADR 0054), FOR
// TESTS ONLY: the keys it streams and signs with, the visitor it admits,
// what it was asked and what it is told to answer. Guarded by the fake's
// mu.
type fakeVisits struct {
	keys     *visit.Keys
	subject  string
	email    string
	lifetime time.Duration
	err      error
	delay    time.Duration
	ended    map[string]string // visit ID → end reason
	redeemed map[string]bool   // challenge IDs
	redeems  []wire.RedeemVisitRequest
	renewals []wire.RenewVisitRequest
	ends     []wire.EndVisitRequest
}

// tableKeys are the keys the table carries; nil before SetVisits.
func (v *fakeVisits) tableKeys() *wire.VisitKeys {
	if v.keys == nil {
		return nil
	}
	return &wire.VisitKeys{TokenKID: v.keys.TokenKID(), TokenKey: v.keys.TokenKey(),
		ChallengeKID: v.keys.ChallengeKID(), ChallengeKey: v.keys.ChallengeKey()}
}

// SetVisits gives the fake the installation's keys of visits, which must
// be able to sign: the table carries them from now on, in every whole
// table and in a delta to every stream, and the visit calls are answered
// with tokens they sign.
func (f *FakeControlPlane) SetVisits(k *visit.Keys) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visits.keys = k
	keys := f.visits.tableKeys()
	f.broadcastLocked(func(*fakeStream) *wire.Table { return &wire.Table{VisitKeys: keys} })
}

// SetVisitor sets the subject and email of the visits redeemed from now
// on; the default is "visitor-1" without an email.
func (f *FakeControlPlane) SetVisitor(subject, email string) {
	f.mu.Lock()
	f.visits.subject, f.visits.email = subject, email
	f.mu.Unlock()
}

// SetVisitTokenLifetime sets the lifetime of the tokens signed from now
// on; 0 is visit.TokenLifetime.
func (f *FakeControlPlane) SetVisitTokenLifetime(d time.Duration) {
	f.mu.Lock()
	f.visits.lifetime = d
	f.mu.Unlock()
}

// SetVisitError makes every visit call fail with err (a gRPC status
// error); nil answers them again.
func (f *FakeControlPlane) SetVisitError(err error) {
	f.mu.Lock()
	f.visits.err = err
	f.mu.Unlock()
}

// SetVisitDelay makes every visit call wait d before it answers.
func (f *FakeControlPlane) SetVisitDelay(d time.Duration) {
	f.mu.Lock()
	f.visits.delay = d
	f.mu.Unlock()
}

// EndVisit ends the visit id with reason: its renewals answer ended from
// now on.
func (f *FakeControlPlane) EndVisit(id, reason string) {
	f.mu.Lock()
	if f.visits.ended == nil {
		f.visits.ended = map[string]string{}
	}
	f.visits.ended[id] = reason
	f.mu.Unlock()
}

// Redeems returns the RedeemVisit calls received, in order.
func (f *FakeControlPlane) Redeems() []wire.RedeemVisitRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.visits.redeems)
}

// VisitRenewals returns the RenewVisit calls received, in order.
func (f *FakeControlPlane) VisitRenewals() []wire.RenewVisitRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.visits.renewals)
}

// EndVisits returns the EndVisit calls received, in order.
func (f *FakeControlPlane) EndVisits() []wire.EndVisitRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.visits.ends)
}

// visitWait waits the fake's delay and returns its error, if any.
func (f *FakeControlPlane) visitWait(ctx context.Context) error {
	f.mu.Lock()
	d, err := f.visits.delay, f.visits.err
	f.mu.Unlock()
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
	return err
}

// signLocked signs the token of a visit from its claims, issued now.
func (f *FakeControlPlane) signLocked(t visit.Token) (string, error) {
	life := f.visits.lifetime
	if life <= 0 {
		life = visit.TokenLifetime
	}
	now := time.Now()
	t.IssuedAt = now.Unix()
	t.Expires = min(now.Add(life).Unix(), t.VisitEnds)
	return f.visits.keys.SignToken(t)
}

func (s *fakeService) RedeemVisit(ctx context.Context, req *wire.RedeemVisitRequest) (*wire.RedeemVisitResponse, error) {
	f := s.f
	f.mu.Lock()
	f.visits.redeems = append(f.visits.redeems, *req)
	f.mu.Unlock()
	if err := f.visitWait(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.visits.keys
	if k == nil {
		return nil, status.Error(codes.Unavailable, "visits do not run on this instance")
	}
	refused := &wire.RedeemVisitResponse{Outcome: wire.OutcomeRefused, Reason: "unknown"}
	c, err := k.VerifyCode(req.Code)
	if err != nil || c.Host != req.Host || visit.BindingHash(req.Binding) != c.Binding ||
		c.Outcome != visit.OutcomeAccepted {
		return refused, nil
	}
	if f.visits.redeemed[c.Challenge] {
		return &wire.RedeemVisitResponse{Outcome: wire.OutcomeRefused, Reason: "used"}, nil
	}
	if f.visits.redeemed == nil {
		f.visits.redeemed = map[string]bool{}
	}
	f.visits.redeemed[c.Challenge] = true
	sub := f.visits.subject
	if sub == "" {
		sub = "visitor-1"
	}
	tok, err := f.signLocked(visit.Token{Subject: sub, Env: c.Env, Visit: c.Visit, Host: c.Host,
		Provider: f.envs[c.Env].Provider, Email: f.visits.email, Binding: req.VisitBinding,
		VisitEnds: time.Now().Add(visit.MaxVisit).Unix()})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &wire.RedeemVisitResponse{Outcome: wire.OutcomeAdmitted, Token: tok}, nil
}

func (s *fakeService) RenewVisit(ctx context.Context, req *wire.RenewVisitRequest) (*wire.RenewVisitResponse, error) {
	f := s.f
	f.mu.Lock()
	f.visits.renewals = append(f.visits.renewals, *req)
	f.mu.Unlock()
	if err := f.visitWait(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.visits.keys
	if k == nil {
		return nil, status.Error(codes.Unavailable, "visits do not run on this instance")
	}
	t, err := k.VerifyToken(req.Token)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed token")
	}
	if t.Binding != visit.BindingHash(req.Secret) {
		return &wire.RenewVisitResponse{Outcome: wire.OutcomeEnded, Reason: "unknown"}, nil
	}
	if reason, ok := f.visits.ended[t.Visit]; ok {
		return &wire.RenewVisitResponse{Outcome: wire.OutcomeEnded, Reason: reason}, nil
	}
	if time.Now().Unix() >= t.VisitEnds {
		return &wire.RenewVisitResponse{Outcome: wire.OutcomeEnded, Reason: "expired"}, nil
	}
	tok, err := f.signLocked(t)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &wire.RenewVisitResponse{Outcome: wire.OutcomeRenewed, Token: tok}, nil
}

func (s *fakeService) EndVisit(ctx context.Context, req *wire.EndVisitRequest) (*wire.EndVisitResponse, error) {
	f := s.f
	f.mu.Lock()
	f.visits.ends = append(f.visits.ends, *req)
	f.mu.Unlock()
	if err := f.visitWait(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if k := f.visits.keys; k != nil {
		if t, err := k.VerifyToken(req.Token); err == nil && t.Binding == visit.BindingHash(req.Secret) {
			if f.visits.ended == nil {
				f.visits.ended = map[string]string{}
			}
			if _, ok := f.visits.ended[t.Visit]; !ok {
				f.visits.ended[t.Visit] = "signed_out"
			}
		}
	}
	return &wire.EndVisitResponse{}, nil
}
