// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"

	"google.golang.org/grpc/codes"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
)

// maxVisitCalls is how many renewals, and separately how many redeems
// and ends, the agent has in flight at once (ADR 0054): so that sign-ins
// and sign-outs cannot crowd out the renewals that put visits to their
// providers again, nor renewals the sign-ins.
const maxVisitCalls = 8

// ErrBusy is the error of a visit call beyond maxVisitCalls: it fails at
// once, unsent. To status.Code it reads as codes.ResourceExhausted, so
// the data path does not take it for the control plane being away.
var ErrBusy error = &callError{code: codes.ResourceExhausted, text: "ingress agent: too many visit calls in flight"}

// RedeemVisit redeems an accepted code on the current connection (ADR
// 0054). It returns ErrNotConnected without a connection and ErrBusy
// when maxVisitCalls redeems and ends are in flight; the caller bounds it
// with ctx.
func (a *Agent) RedeemVisit(ctx context.Context, req wire.RedeemVisitRequest) (wire.RedeemVisitResponse, error) {
	resp, err := visitCall(a, a.redeemSlots, func(s *session) (*wire.RedeemVisitResponse, error) {
		return s.client.RedeemVisit(ctx, &req)
	})
	if err != nil {
		return wire.RedeemVisitResponse{}, err
	}
	return *resp, nil
}

// RenewVisit asks for a visit's next token on the current connection.
// It returns ErrNotConnected without a connection and ErrBusy when
// maxVisitCalls renewals are in flight.
func (a *Agent) RenewVisit(ctx context.Context, req wire.RenewVisitRequest) (wire.RenewVisitResponse, error) {
	resp, err := visitCall(a, a.renewSlots, func(s *session) (*wire.RenewVisitResponse, error) {
		return s.client.RenewVisit(ctx, &req)
	})
	if err != nil {
		return wire.RenewVisitResponse{}, err
	}
	return *resp, nil
}

// EndVisit ends a visit whose visitor signed out, on the current
// connection; it shares the bound of RedeemVisit.
func (a *Agent) EndVisit(ctx context.Context, req wire.EndVisitRequest) (wire.EndVisitResponse, error) {
	resp, err := visitCall(a, a.redeemSlots, func(s *session) (*wire.EndVisitResponse, error) {
		return s.client.EndVisit(ctx, &req)
	})
	if err != nil {
		return wire.EndVisitResponse{}, err
	}
	return *resp, nil
}

// visitCall makes one call on the current session within a slot of
// slots.
func visitCall[R any](a *Agent, slots chan struct{}, call func(*session) (*R, error)) (*R, error) {
	s := a.current()
	if s == nil {
		return nil, ErrNotConnected
	}
	select {
	case slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	defer func() { <-slots }()
	return call(s)
}
