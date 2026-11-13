// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visits

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/visit"
)

// The reasons of a refused redeem and of an ended renewal that are no
// end reason of a visit.
const (
	reasonUsed    = "used"
	reasonExpired = "expired"
	reasonEnded   = "ended"
	reasonUnknown = "unknown"
)

// sameHash compares two binding hashes in constant time.
func sameHash(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// endReason is why a visit can no longer be renewed, at the database's
// now in v, with its environment e (found false: gone): "" while it
// lives. Its end, the environment no longer public, or no longer guarded
// by the visit's provider.
func endReason(v store.Visit, e store.Environment, found bool) string {
	provider := e.AccessProvider
	if provider == "" {
		provider = model.ProviderFerun
	}
	switch {
	case !v.EndsAt.After(v.Now):
		return model.VisitExpired
	case !found || !e.Public || e.Deleting:
		return model.VisitUnpublished
	case e.Access != model.AccessVisitors || provider != v.Provider:
		return model.VisitProviderChanged
	}
	return ""
}

// environmentOf reads a visit's environment on q; found false when it is
// gone.
func environmentOf(ctx context.Context, q *store.Store, v store.Visit) (store.Environment, bool, error) {
	e, err := q.GetEnvironment(ctx, v.EnvID)
	if errors.Is(err, store.ErrNoRow) {
		return store.Environment{}, false, nil
	}
	return e, err == nil, err
}

// token signs the visit's next token from its row v, read at v.Now: for
// TokenLifetime, never past the visit's end, and for a provider of kind
// tenant never more than TokenLifetime past its last ask (S14).
func (s *Service) token(ctx context.Context, v store.Visit) (string, error) {
	keys, err := s.Keys(ctx)
	if err != nil {
		return "", err
	}
	exp := v.Now.Add(visit.TokenLifetime)
	if v.EndsAt.Before(exp) {
		exp = v.EndsAt
	}
	if asked := v.AskedAt.Add(visit.TokenLifetime); v.ProviderKind == model.ProviderKindTenant && asked.Before(exp) {
		exp = asked
	}
	return keys.SignToken(visit.Token{Subject: v.Subject, Env: v.EnvID.String(), Visit: v.ID.String(), Host: v.Host,
		Provider: v.Provider, Email: v.Email, Name: v.Name, Binding: v.Binding, IssuedAt: v.Now.Unix(),
		Expires: exp.Unix(), VisitEnds: v.EndsAt.Unix()})
}

// Redeem redeems an accepted code for the visit's first token (§1.10):
// with the binding of the challenge cookie the code names, at the host it
// names, once, or again within visit.RedeemWindow with the same binding,
// while the code lives. The visit takes the binding of the visit cookie
// the ingress sets. A malformed VisitBinding is ErrInvalid; every refusal
// is OutcomeRefused with its reason.
func (s *Service) Redeem(ctx context.Context, req wire.RedeemVisitRequest) (wire.RedeemVisitResponse, error) {
	refused := func(reason string) (wire.RedeemVisitResponse, error) {
		return wire.RedeemVisitResponse{Outcome: wire.OutcomeRefused, Reason: reason}, nil
	}
	// VisitBinding is the hash of the visit cookie's secret: 43 base64url
	// characters, as a secret is.
	if !visit.ValidBindingSecret(req.VisitBinding) {
		return wire.RedeemVisitResponse{}, fmt.Errorf("the visit's binding: %w", ErrInvalid)
	}
	keys, err := s.Keys(ctx)
	if err != nil {
		return wire.RedeemVisitResponse{}, err
	}
	code, err := keys.VerifyCode(req.Code)
	if err != nil || code.Outcome != visit.OutcomeAccepted || code.Host != req.Host ||
		!visit.ValidBindingSecret(req.Binding) || !sameHash(visit.BindingHash(req.Binding), code.Binding) {
		return refused(reasonUnknown)
	}
	vid, err := uuid.Parse(code.Visit)
	if err != nil {
		return refused(reasonUnknown)
	}
	var out wire.RedeemVisitResponse
	var v store.Visit
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		out, v = wire.RedeemVisitResponse{}, store.Visit{}
		got, ok, err := tx.RedeemVisitAnswer(ctx, code.Challenge, code.Binding, visit.RedeemWindow)
		if err != nil {
			return err
		}
		if !ok || got != vid {
			a, err := tx.GetVisitAnswer(ctx, code.Challenge)
			switch {
			case errors.Is(err, store.ErrNoRow) || (err == nil && (a.VisitID != vid || a.Binding != code.Binding)):
				out = wire.RedeemVisitResponse{Outcome: wire.OutcomeRefused, Reason: reasonUnknown}
			case err != nil:
				return err
			case !a.CodeExpiresAt.After(a.Now):
				out = wire.RedeemVisitResponse{Outcome: wire.OutcomeRefused, Reason: reasonExpired}
			default:
				out = wire.RedeemVisitResponse{Outcome: wire.OutcomeRefused, Reason: reasonUsed}
			}
			return nil
		}
		v, err = tx.BindVisit(ctx, vid, req.VisitBinding)
		if errors.Is(err, store.ErrNoRow) {
			out = wire.RedeemVisitResponse{Outcome: wire.OutcomeRefused, Reason: reasonEnded}
			return nil
		}
		if err != nil {
			return err
		}
		e, found, err := environmentOf(ctx, tx, v)
		if err != nil {
			return err
		}
		if reason := endReason(v, e, found); reason != "" {
			if _, err := tx.EndVisit(ctx, v.ID, reason); err != nil {
				return err
			}
			out = wire.RedeemVisitResponse{Outcome: wire.OutcomeRefused, Reason: reasonEnded}
		}
		return nil
	})
	if err != nil {
		return wire.RedeemVisitResponse{}, fmt.Errorf("redeem a code: %w", err)
	}
	if out.Outcome != "" {
		s.log.Debug("a code refused", "env", code.Env, "reason", out.Reason)
		return out, nil
	}
	tok, err := s.token(ctx, v)
	if err != nil {
		return wire.RedeemVisitResponse{}, fmt.Errorf("sign a token: %w", err)
	}
	s.log.Info("visit begun", "visit", v.ID, "env", v.EnvID, "provider", v.Provider)
	return wire.RedeemVisitResponse{Outcome: wire.OutcomeAdmitted, Token: tok}, nil
}

// visitOf is the visit a token and the visit cookie's secret beside it
// hold: ok false when there is none (no row, no binding yet, or another
// secret than the token's and the row's binding), which ends nothing.
func (s *Service) visitOf(ctx context.Context, tok visit.Token, secret string) (store.Visit, bool, error) {
	vid, err := uuid.Parse(tok.Visit)
	if err != nil {
		return store.Visit{}, false, nil
	}
	v, err := s.st.GetVisit(ctx, vid)
	if errors.Is(err, store.ErrNoRow) {
		return store.Visit{}, false, nil
	}
	if err != nil {
		return store.Visit{}, false, err
	}
	hash := visit.BindingHash(secret)
	if v.Binding == "" || !sameHash(hash, tok.Binding) || !sameHash(hash, v.Binding) {
		return store.Visit{}, false, nil
	}
	return v, true, nil
}

// end ends a visit with reason and returns the reason it ended with: its
// own, or another's that ended it first.
func (s *Service) end(ctx context.Context, v store.Visit, reason string) (string, error) {
	ok, err := s.st.EndVisit(ctx, v.ID, reason)
	if err != nil || ok {
		return reason, err
	}
	again, err := s.st.GetVisit(ctx, v.ID)
	if errors.Is(err, store.ErrNoRow) {
		return reason, nil
	}
	if err != nil {
		return "", err
	}
	if again.EndReason == "" {
		return reason, nil
	}
	return again.EndReason, nil
}

// Renew gives a visit's next token (D7, §1.9), with the visit cookie's
// secret: OutcomeEnded with reason unknown when the cookie holds no visit;
// with the visit's end reason when it ended, or ends now (its time, its
// environment unpublished or guarded otherwise, or its provider's no);
// else OutcomeRenewed. A provider of kind tenant is asked at most every
// visit.AskEvery per visit, never inside a transaction, and its failure to
// answer keeps the visit; kind ferun looks the grant up at every renewal;
// kind oidc is asked nothing. A malformed token or secret is ErrInvalid.
func (s *Service) Renew(ctx context.Context, req wire.RenewVisitRequest) (wire.RenewVisitResponse, error) {
	ended := func(reason string) (wire.RenewVisitResponse, error) {
		return wire.RenewVisitResponse{Outcome: wire.OutcomeEnded, Reason: reason}, nil
	}
	keys, err := s.Keys(ctx)
	if err != nil {
		return wire.RenewVisitResponse{}, err
	}
	tok, err := keys.VerifyToken(req.Token)
	if err != nil {
		return wire.RenewVisitResponse{}, fmt.Errorf("the token: %w", ErrInvalid)
	}
	if !visit.ValidBindingSecret(req.Secret) {
		return wire.RenewVisitResponse{}, fmt.Errorf("the visit's secret: %w", ErrInvalid)
	}
	v, ok, err := s.visitOf(ctx, tok, req.Secret)
	if err != nil {
		return wire.RenewVisitResponse{}, fmt.Errorf("read a visit: %w", err)
	}
	if !ok {
		return ended(reasonUnknown)
	}
	if !v.Open() {
		return ended(v.EndReason)
	}
	if tok.Env != v.EnvID.String() || tok.Host != v.Host || tok.Subject != v.Subject || tok.Provider != v.Provider {
		return ended(reasonUnknown)
	}
	e, found, err := environmentOf(ctx, s.st, v)
	if err != nil {
		return wire.RenewVisitResponse{}, fmt.Errorf("read an environment: %w", err)
	}
	if reason := endReason(v, e, found); reason != "" {
		reason, err := s.end(ctx, v, reason)
		if err != nil {
			return wire.RenewVisitResponse{}, fmt.Errorf("end a visit: %w", err)
		}
		return ended(reason)
	}
	no, endCtx := false, ctx
	switch v.ProviderKind {
	case model.ProviderKindTenant:
		// The claim uses up the ask for the next visit.AskEvery: it, the
		// provider's row, the call and the end its no brings run detached
		// from the ingress's call, so that an ask claimed is an ask made
		// and its verdict recorded even when the ingress gave up (a claim
		// cancelled in flight may have committed).
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), visit.AskTimeout+askGrace)
		defer cancel()
		claimed, err := s.st.ClaimVisitAsk(dctx, v.ID, visit.AskEvery)
		if err != nil {
			return wire.RenewVisitResponse{}, fmt.Errorf("claim an ask: %w", err)
		}
		if claimed {
			no, endCtx = s.askTenant(dctx, v) == verdictNo, dctx
		}
	case model.ProviderKindFerun:
		ok, err := s.st.PersonMayVisitByID(ctx, v.PersonID, v.Tenant, v.EnvID)
		if err != nil {
			return wire.RenewVisitResponse{}, fmt.Errorf("look up a grant: %w", err)
		}
		no = !ok
	}
	if no {
		reason, err := s.end(endCtx, v, model.VisitProviderNo)
		if err != nil {
			return wire.RenewVisitResponse{}, fmt.Errorf("end a visit: %w", err)
		}
		s.log.Info("visit ended by its provider", "visit", v.ID, "env", v.EnvID)
		return ended(reason)
	}
	// The next token from a fresh read: an ask or another renewal may
	// have changed the row.
	if v, err = s.st.GetVisit(ctx, v.ID); err != nil {
		return wire.RenewVisitResponse{}, fmt.Errorf("read a visit: %w", err)
	}
	if !v.Open() {
		return ended(v.EndReason)
	}
	if !v.EndsAt.After(v.Now) {
		reason, err := s.end(ctx, v, model.VisitExpired)
		if err != nil {
			return wire.RenewVisitResponse{}, fmt.Errorf("end a visit: %w", err)
		}
		return ended(reason)
	}
	next, err := s.token(ctx, v)
	if err != nil {
		return wire.RenewVisitResponse{}, fmt.Errorf("sign a token: %w", err)
	}
	return wire.RenewVisitResponse{Outcome: wire.OutcomeRenewed, Token: next}, nil
}

// End ends a visit whose visitor signed out on its host, with the visit
// cookie's secret; the token's expiry does not matter. A secret that is
// not the visit's changes nothing; the answer is the same either way. A
// malformed token is ErrInvalid.
func (s *Service) End(ctx context.Context, req wire.EndVisitRequest) (wire.EndVisitResponse, error) {
	keys, err := s.Keys(ctx)
	if err != nil {
		return wire.EndVisitResponse{}, err
	}
	tok, err := keys.VerifyToken(req.Token)
	if err != nil {
		return wire.EndVisitResponse{}, fmt.Errorf("the token: %w", ErrInvalid)
	}
	if !visit.ValidBindingSecret(req.Secret) {
		return wire.EndVisitResponse{}, nil
	}
	v, ok, err := s.visitOf(ctx, tok, req.Secret)
	if err != nil {
		return wire.EndVisitResponse{}, fmt.Errorf("read a visit: %w", err)
	}
	if !ok {
		return wire.EndVisitResponse{}, nil
	}
	if done, err := s.st.EndVisit(ctx, v.ID, model.VisitSignedOut); err != nil {
		return wire.EndVisitResponse{}, err
	} else if done {
		s.log.Info("visit signed out", "visit", v.ID, "env", v.EnvID)
	}
	return wire.EndVisitResponse{}, nil
}

// verdict is a provider's answer to an ask.
type verdict int

const (
	verdictNone verdict = iota // no answer: the visit lives on
	verdictYes
	verdictNo
)

// askGrace is what a claimed ask may take beyond the call's own
// visit.AskTimeout: the provider's row before it and the end after it.
const askGrace = 2 * time.Second

// askTenant asks a visit's provider of kind tenant whether it still
// vouches for the visit (§1.9), the call bounded by visit.AskTimeout. Its
// caller passes a context detached from the ingress's call.
func (s *Service) askTenant(ctx context.Context, v store.Visit) verdict {
	p, err := s.st.GetAccessProvider(ctx, v.Tenant, v.Provider)
	if err != nil || p.Kind != model.ProviderKindTenant {
		s.rl.warn("ask:"+v.Tenant+"/"+v.Provider, "cannot read a visit's provider", "tenant", v.Tenant,
			"provider", v.Provider, "err", err)
		return verdictNone
	}
	ctx, cancel := context.WithTimeout(ctx, visit.AskTimeout)
	defer cancel()
	start := time.Now()
	got, err := s.ask(ctx, p.URL, v.ID)
	s.log.Debug("a provider asked", "tenant", v.Tenant, "provider", v.Provider, "visit", v.ID, "verdict", int(got),
		"took", time.Since(start), "err", err)
	if err != nil {
		s.rl.warn("ask:"+v.Tenant+"/"+v.Provider, "a provider did not answer", "tenant", v.Tenant,
			"provider", v.Provider, "err", err)
	}
	return got
}
