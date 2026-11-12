// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/visit"
	"github.com/gardenlinux/feos/go/internal/visits"
)

// Visits is what the ingress's calls of visits (ADR 0054) need of ferund's
// visits service (*visits.Service): the redeem of an accepted code, the
// renewal of a visit's token and the end of a visit. An error wrapping
// visits.ErrInvalid is a malformed request; any other is a failure.
type Visits interface {
	Redeem(ctx context.Context, req wire.RedeemVisitRequest) (wire.RedeemVisitResponse, error)
	Renew(ctx context.Context, req wire.RenewVisitRequest) (wire.RenewVisitResponse, error)
	End(ctx context.Context, req wire.EndVisitRequest) (wire.EndVisitResponse, error)
}

// visitsOffText is what the calls of visits answer (UNAVAILABLE) on an
// instance without Deps.Visits.
const visitsOffText = "visits do not run on this instance"

// The sizes of the calls' fields: a binding secret or its hash is 43
// base64url characters (visit.NewBindingSecret, visit.BindingHash), a host
// a DNS name.
const (
	bindingLen = 43
	maxHostLen = 253
)

// RedeemVisit redeems an accepted code that a browser brought to the
// ingress's /.ferun/auth (ADR 0054) for the visit's first token, with the
// secret of that browser's challenge cookie and the hash of the visit
// secret the ingress made (Visits.Redeem). The outcome, admitted or
// refused, is the answer; never PERMISSION_DENIED.
func (s *Service) RedeemVisit(ctx context.Context, req *wire.RedeemVisitRequest) (*wire.RedeemVisitResponse, error) {
	p, err := s.visitCall(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case !validJWT(req.Code):
		return nil, s.visitInvalid(p, "redeem", "code is not a code")
	case len(req.Binding) != bindingLen:
		return nil, s.visitInvalid(p, "redeem", "binding is not a binding secret")
	case len(req.VisitBinding) != bindingLen:
		return nil, s.visitInvalid(p, "redeem", "visitBinding is not a binding")
	case req.Host == "" || len(req.Host) > maxHostLen:
		return nil, s.visitInvalid(p, "redeem", "host is not a host")
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := s.visits.Redeem(ctx, *req)
	if err != nil {
		return nil, s.visitError(ctx, p, "redeem", err)
	}
	if resp.Outcome == wire.OutcomeRefused {
		s.rl.warn("visit-redeem", "ingress RedeemVisit: a code was refused", "ingress", p.Name, "reason", resp.Reason)
	}
	return &resp, nil
}

// RenewVisit gives a visit's next token for its token and the visit
// cookie's secret (Visits.Renew): renewed with the token, or ended with
// the reason. Every error is no answer: the ingress serves on with the
// token it has (ADR 0054, the outage rule).
func (s *Service) RenewVisit(ctx context.Context, req *wire.RenewVisitRequest) (*wire.RenewVisitResponse, error) {
	p, err := s.visitCall(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case !validJWT(req.Token):
		return nil, s.visitInvalid(p, "renew", "token is not a token")
	case len(req.Secret) != bindingLen:
		return nil, s.visitInvalid(p, "renew", "secret is not a visit secret")
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := s.visits.Renew(ctx, *req)
	if err != nil {
		return nil, s.visitError(ctx, p, "renew", err)
	}
	if resp.Outcome == wire.OutcomeEnded && resp.Reason == "unknown" {
		// A cookie that holds no visit: one a browser made up or kept.
		s.rl.warn("visit-renew", "ingress RenewVisit: a cookie holds no visit", "ingress", p.Name)
	}
	return &resp, nil
}

// EndVisit ends a visit whose visitor signed out on a host, for its token
// and the visit cookie's secret (Visits.End).
func (s *Service) EndVisit(ctx context.Context, req *wire.EndVisitRequest) (*wire.EndVisitResponse, error) {
	p, err := s.visitCall(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case !validJWT(req.Token):
		return nil, s.visitInvalid(p, "end", "token is not a token")
	case len(req.Secret) != bindingLen:
		return nil, s.visitInvalid(p, "end", "secret is not a visit secret")
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := s.visits.End(ctx, *req)
	if err != nil {
		return nil, s.visitError(ctx, p, "end", err)
	}
	return &resp, nil
}

// visitCall is the calls' first checks: an ingress's certificate on the
// connection, an instance that does not stop (UNAVAILABLE "instance
// stopping") and visits running on it (UNAVAILABLE).
func (s *Service) visitCall(ctx context.Context) (nodelink.IngressPeer, error) {
	p, err := ingressPeer(ctx)
	if err != nil {
		return nodelink.IngressPeer{}, err
	}
	if s.stopping.Load() {
		return nodelink.IngressPeer{}, errStopping()
	}
	if s.visits == nil {
		return nodelink.IngressPeer{}, status.Error(codes.Unavailable, visitsOffText)
	}
	return p, nil
}

// validJWT reports whether raw can be a token or a code by its size.
func validJWT(raw string) bool { return raw != "" && len(raw) <= visit.MaxJWT }

// visitInvalid is INVALID_ARGUMENT for a call of visits: what a browser's
// cookie or link can cause, so it is logged rate-limited, the field named
// and its value never.
func (s *Service) visitInvalid(p nodelink.IngressPeer, what, why string) error {
	s.rl.warn("visit-invalid", "ingress call of visits refused", "ingress", p.Name, "call", what, "err", why)
	return status.Errorf(codes.InvalidArgument, "%s the visit: %s", what, why)
}

// visitError is what a call of visits answers for the visits service's
// error: INVALID_ARGUMENT for visits.ErrInvalid, else what dbError
// answers (UNAVAILABLE while the call goes on), which the ingress reads as
// ferund away.
func (s *Service) visitError(ctx context.Context, p nodelink.IngressPeer, what string, err error) error {
	if errors.Is(err, visits.ErrInvalid) {
		return s.visitInvalid(p, what, "the request is not valid")
	}
	if ctx.Err() == nil {
		s.rl.warn("visit-"+what, "ingress call of visits failed", "ingress", p.Name, "call", what, "err", err)
	}
	return dbError(ctx, err, what+" the visit")
}
