// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/ingress/table"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// Wake's answers: in how many seconds the ingress looks again; later
// when a hibernated instance is restored from the object store, which
// takes longer than a resume.
const (
	wakeRetryAfter      = 5
	wakeRetryHibernated = 10
)

// Wake wakes the asleep instances of a routed service of a public
// environment (§6.6 of the ingress's design): the instances that are not
// stopping and that the table counts as Asleep (table.Asleep: paused,
// hibernated, pausing or hibernating, or ready and running with an
// operation, a pause on its way). An environment that is not public,
// deleting or unknown is NOT_FOUND, and so is a service that is not an
// image service a route of the routing deployment sends to. Each instance
// is woken once at a time on this instance, in the background on Run's
// context, never the caller's: Engine.Sync, then Engine.Ensure, within
// config.PreparationBudget; failures are logged, rate-limited. The answer
// is how many are being woken and when to look again: in 5 s, in 10 s
// when one is hibernated or hibernating (a restore takes longer). Without
// an Engine nothing is woken. A database error, rpcTimeout's expiry
// included, is UNAVAILABLE. While the instance stops, and before Run, it
// answers UNAVAILABLE.
func (s *Service) Wake(ctx context.Context, req *wire.WakeRequest) (*wire.WakeResponse, error) {
	if _, err := ingressPeer(ctx); err != nil {
		return nil, err
	}
	if s.stopping.Load() {
		return nil, errStopping()
	}
	if !s.running() {
		return nil, status.Error(codes.Unavailable, "the ingress service does not run on this instance yet")
	}
	envID, err := uuid.Parse(req.Env)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "env is not an environment's ID")
	}
	if !spec.IsName(req.Service) {
		return nil, status.Error(codes.InvalidArgument, "service is not a service's name")
	}
	dctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	rows, err := s.st.IngressRouting(dctx, []uuid.UUID{envID})
	if err != nil {
		return nil, dbError(ctx, err, "wake")
	}
	envs := table.Group(rows)
	if len(envs) == 0 {
		return nil, status.Errorf(codes.NotFound, "no public environment %s", envID)
	}
	e := envs[0]
	sp, err := s.tbl.Specs().Get(dctx, e.ID, e.CreatedAt, table.RoutingDeployment(e.Deployment, e.Deployed))
	if err != nil && !errors.Is(err, table.ErrSpec) {
		return nil, dbError(ctx, err, "wake")
	}
	if err != nil || !routedImage(sp, req.Service) {
		return nil, status.Errorf(codes.NotFound, "service %s of %s is not routed", req.Service, envID)
	}
	resp := &wire.WakeResponse{RetryAfterSeconds: wakeRetryAfter}
	if s.engine == nil {
		return resp, nil
	}
	for _, in := range e.Instances {
		if in.Service != req.Service || !table.Asleep(in.Phase, in.ClientState, in.Op) {
			continue
		}
		s.wake(in.ID)
		resp.Waking++
		if in.ClientState == model.StateHibernated || in.ClientState == model.StateHibernating {
			resp.RetryAfterSeconds = wakeRetryHibernated
		}
	}
	return resp, nil
}

// routedImage reports whether service is an image service of sp that a
// route sends to: the services the ingress serves.
func routedImage(sp *spec.Spec, service string) bool {
	if target, ok := sp.Services[service]; !ok || spec.IsAttached(target) {
		return false
	}
	for _, r := range spec.IngressRoutes(sp) {
		if r.Service == service && !r.Door {
			return true
		}
	}
	return false
}

// wake brings the sandbox id up in the background, once at a time on this
// instance: a Sync first, which applies what its node holds of the pod
// (a pause that ended there), then an Ensure, which resumes or restores
// it.
func (s *Service) wake(id podid.ID) {
	s.wakingMu.Lock()
	if s.waking[id] {
		s.wakingMu.Unlock()
		return
	}
	s.waking[id] = true
	s.wakingMu.Unlock()
	done := func() {
		s.wakingMu.Lock()
		delete(s.waking, id)
		s.wakingMu.Unlock()
	}
	if !s.goRunning(func(base context.Context) {
		defer done()
		ctx, cancel := context.WithTimeout(base, config.PreparationBudget)
		defer cancel()
		if err := s.engine.Sync(ctx, id); err != nil && ctx.Err() == nil {
			s.rl.warn("wake-sync", "ingress Wake: cannot sync a sandbox", "sandbox", id, "err", err)
		}
		if _, err := s.engine.Ensure(ctx, id); err != nil && ctx.Err() == nil {
			s.rl.warn("wake-ensure", "ingress Wake: the sandbox could not be brought up", "sandbox", id, "err", err)
		}
	}) {
		done()
	}
}
