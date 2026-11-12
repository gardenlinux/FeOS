// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visits

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/visit"
)

// The bounds of an accept's fields.
const (
	maxSubject = 255
	maxName    = 200
	maxSession = 255
)

// guarded is an environment a challenge names, as it is now: guarded by
// a provider the caller may answer for, at the challenge's host.
type guarded struct {
	env store.Environment
	// provider is the provider's name (model.ProviderFerun for the
	// builtin), kind its kind, row its row (none for ferun).
	provider, kind string
	row            store.AccessProvider
}

// opened opens a sealed challenge: 404 no such challenge for one that is
// not ours.
func (s *Service) opened(ctx context.Context, sealed string) (visit.Challenge, error) {
	if sealed == "" || len(sealed) > visit.MaxSealed {
		return visit.Challenge{}, errNoChallenge()
	}
	keys, err := s.Keys(ctx)
	if err != nil {
		return visit.Challenge{}, s.failed("the keys", err)
	}
	ch, err := keys.OpenChallenge(sealed)
	if err != nil {
		return visit.Challenge{}, errNoChallenge()
	}
	return ch, nil
}

// admit reads on q, with lock, the environment ch names and checks that
// the caller may answer it: the environment is there, public, not being
// deleted, guarded (visitors) by a provider of a kind this installation
// serves, which the caller answers for (a tenant principal: of kind
// tenant, its own tenant's, and admin on the environment; ferun-auth: of
// kinds ferun and oidc), and ch's host is one of its hosts. Anything else
// is notFound, never a 403.
func (s *Service) admit(ctx context.Context, q *store.Store, c Caller, envID, host string, lock store.RowLock,
	notFound func() error) (guarded, error) {
	id, err := uuid.Parse(envID)
	if err != nil {
		return guarded{}, notFound()
	}
	e, err := q.LockEnvironment(ctx, id, lock)
	if errors.Is(err, store.ErrNoRow) {
		return guarded{}, notFound()
	}
	if err != nil {
		return guarded{}, s.failed("read an environment", err)
	}
	if !e.Public || e.Deleting || e.Access != model.AccessVisitors {
		return guarded{}, notFound()
	}
	g := guarded{env: e, provider: model.ProviderFerun, kind: model.ProviderKindFerun}
	if e.AccessProvider != "" {
		g.row, err = q.GetAccessProvider(ctx, e.Owner, e.AccessProvider)
		if errors.Is(err, store.ErrNoRow) {
			return guarded{}, notFound()
		}
		if err != nil {
			return guarded{}, s.failed("read a provider", err)
		}
		g.provider, g.kind = g.row.Name, g.row.Kind
	}
	if !s.serves(g.kind) || !c.answers(e, g.kind) {
		return guarded{}, notFound()
	}
	hosts, err := env.Hosts(ctx, q, s.cfg.Zones, e)
	if err != nil {
		return guarded{}, s.failed("the hosts of an environment", err)
	}
	if !slices.Contains(hosts, host) {
		return guarded{}, notFound()
	}
	return g, nil
}

// answers reports whether c may answer for environment e's provider of
// kind: ferun-auth for the kinds ferun and oidc, a tenant principal for
// its tenant's environments of kind tenant on which it is admin.
func (c Caller) answers(e store.Environment, kind string) bool {
	if c.Auth {
		return kind == model.ProviderKindFerun || kind == model.ProviderKindOIDC
	}
	return c.Tenant != "" && c.Tenant == e.Owner && kind == model.ProviderKindTenant && c.Admin != nil && c.Admin(e.ID)
}

// envView is an environment as a provider sees it.
func envView(e store.Environment) EnvView {
	labels := e.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return EnvView{ID: e.ID.String(), Name: e.Name, Tenant: e.Owner, Labels: labels}
}

// restart is where the browser of a challenge begins again: its host and
// path.
func restart(ch visit.Challenge) string {
	return "https://" + ch.Host + visit.SafePath(ch.Path)
}

// expired reports whether a challenge has ended at now (the database's
// clock), past its end and the leeway.
func expired(ch visit.Challenge, now time.Time) bool {
	return now.After(time.Unix(ch.Expires, 0).Add(visit.ChallengeLeeway))
}

// OpenChallenge opens a sealed challenge for a provider: the environment
// and host it asks for, whether it expired or was answered, where the
// browser begins again, the provider, and for ferun-auth an oidc
// provider's client with its secret. 404 no such challenge for a challenge
// that is not ours or that the caller may not answer (admit).
func (s *Service) OpenChallenge(ctx context.Context, c Caller, sealed string) (Challenge, error) {
	ch, err := s.opened(ctx, sealed)
	if err != nil {
		return Challenge{}, err
	}
	g, err := s.admit(ctx, s.st, c, ch.Env, ch.Host, store.NoLock, errNoChallenge)
	if err != nil {
		return Challenge{}, err
	}
	now, err := s.st.Now(ctx)
	if err != nil {
		return Challenge{}, s.failed("the time", err)
	}
	answered := true
	if _, err := s.st.GetVisitAnswer(ctx, ch.ID); errors.Is(err, store.ErrNoRow) {
		answered = false
	} else if err != nil {
		return Challenge{}, s.failed("read an answer", err)
	}
	out := Challenge{Environment: envView(g.env), Host: ch.Host, Path: ch.Path,
		ExpiresAt: rfc3339(time.Unix(ch.Expires, 0)), Expired: expired(ch, now), Answered: answered,
		Restart: restart(ch), Provider: ProviderRef{Name: g.provider, Kind: g.kind}}
	if c.Auth && g.kind == model.ProviderKindOIDC {
		o, err := s.oidcOf(g.row)
		if err != nil {
			return Challenge{}, err
		}
		out.OIDC = &OIDC{Issuer: o.Issuer, ClientID: o.ClientID, ClientSecret: o.ClientSecret, Rule: o.Rule}
	}
	return out, nil
}

// oidcOf is an oidc provider with its secret opened.
func (s *Service) oidcOf(p store.AccessProvider) (OIDCProvider, error) {
	secret, err := s.d.Sealer.Open(p.ClientSecretSealed, store.AccessProviderAAD(p.Tenant, p.Name))
	if err != nil {
		return OIDCProvider{}, s.failed("open a client secret", fmt.Errorf("provider %s of %s: %w", p.Name, p.Tenant, err))
	}
	return OIDCProvider{Tenant: p.Tenant, Name: p.Name, Issuer: p.Issuer, ClientID: p.ClientID,
		ClientSecret: string(secret), Rule: ruleOf(p.Rule)}, nil
}

// checkAccept checks an accept's fields and returns its end (zero: none).
func checkAccept(in Accept) (time.Time, error) {
	switch {
	case in.Subject == "" || len(in.Subject) > maxSubject || !printable(in.Subject):
		return time.Time{}, invalidAccept("subject: 1 to %d bytes without control characters", maxSubject)
	case len(in.Email) > maxEmail || !printable(in.Email):
		return time.Time{}, invalidAccept("email: at most %d bytes without control characters", maxEmail)
	case len(in.Name) > maxName || !printable(in.Name):
		return time.Time{}, invalidAccept("name: at most %d bytes without control characters", maxName)
	case len(in.Session) > maxSession || !printable(in.Session):
		return time.Time{}, invalidAccept("session: at most %d bytes without control characters", maxSession)
	}
	if in.EndsAt == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, in.EndsAt)
	if err != nil {
		return time.Time{}, invalidAccept("endsAt: a time in RFC 3339")
	}
	return t, nil
}

// AcceptChallenge records a provider's yes and makes the visit, which
// ends at the provider's end, at most visit.MaxVisit after now. For a
// provider of kind ferun ferund decides: the subject is a person of the
// installation's issuer with a grant on the environment or its tenant,
// else the challenge is recorded rejected and the answer is not
// admitted. The answer sends the browser to the challenge's host with
// the code. 404 as OpenChallenge, 409 challenge_expired and
// challenge_answered, 400 invalid_accept.
func (s *Service) AcceptChallenge(ctx context.Context, c Caller, in Accept) (Answer, error) {
	endsAt, err := checkAccept(in)
	if err != nil {
		return Answer{}, err
	}
	return s.answer(ctx, c, in.Challenge, &in, endsAt)
}

// RejectChallenge records a provider's no; the answer sends the browser
// to the challenge's host with a rejected code. The refusals of
// AcceptChallenge.
func (s *Service) RejectChallenge(ctx context.Context, c Caller, sealed string) (Answer, error) {
	return s.answer(ctx, c, sealed, nil, time.Time{})
}

// answer records an answer to a challenge, an accept with in (nil: a
// rejection), in one transaction that holds the environment FOR SHARE and
// checks it again, and signs the code from what it committed.
func (s *Service) answer(ctx context.Context, c Caller, sealed string, in *Accept, endsAt time.Time) (Answer, error) {
	ch, err := s.opened(ctx, sealed)
	if err != nil {
		return Answer{}, err
	}
	if _, err := s.admit(ctx, s.st, c, ch.Env, ch.Host, store.NoLock, errNoChallenge); err != nil {
		return Answer{}, err
	}
	visitID := uuid.New()
	var a store.VisitAnswer
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		g, err := s.admit(ctx, tx, c, ch.Env, ch.Host, store.ForShare, errNoChallenge)
		if err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		if expired(ch, now) {
			return apierr.WithCode(http.StatusConflict, CodeChallengeExpired, textChallengeExpired)
		}
		n := store.NewVisitAnswer{ChallengeID: ch.ID, EnvID: g.env.ID, Host: ch.Host, Outcome: visit.OutcomeRejected,
			Binding: ch.Binding, AnsweredBy: c.MadeBy, CodeLifetime: visit.CodeLifetime,
			ExpiresAt: time.Unix(ch.Expires, 0)}
		if in != nil {
			if !endsAt.IsZero() && !endsAt.After(now) {
				return invalidAccept("endsAt: a time after now")
			}
			nv := store.NewVisit{ID: visitID, EnvID: g.env.ID, Tenant: g.env.Owner, Provider: g.provider,
				ProviderKind: g.kind, Host: ch.Host, Subject: in.Subject, Email: in.Email, Name: in.Name,
				Session: in.Session, EndsAt: endsAt, MaxVisit: visit.MaxVisit}
			admitted := true
			if g.kind == model.ProviderKindFerun {
				nv.PersonID, admitted, err = tx.PersonMayVisit(ctx, s.cfg.Issuer, in.Subject, g.env.Owner, g.env.ID)
				if err != nil {
					return err
				}
			}
			if admitted {
				if _, err := tx.InsertVisit(ctx, nv); err != nil {
					return err
				}
				n.Outcome, n.VisitID = visit.OutcomeAccepted, visitID
			}
		}
		a, err = tx.InsertVisitAnswer(ctx, n)
		if errors.Is(err, store.ErrExists) {
			return apierr.WithCode(http.StatusConflict, CodeChallengeAnswered, textChallengeAnswered)
		}
		return err
	})
	if err != nil {
		return Answer{}, s.pass("answer a challenge", err)
	}
	code := visit.Code{Challenge: ch.ID, Env: ch.Env, Host: ch.Host, Path: ch.Path, Binding: ch.Binding,
		Outcome: a.Outcome, IssuedAt: a.CreatedAt.Unix(), Expires: a.CodeExpiresAt.Unix()}
	out := Answer{Admitted: a.Outcome == visit.OutcomeAccepted}
	if out.Admitted {
		code.Visit, out.Visit = a.VisitID.String(), a.VisitID.String()
	}
	keys, err := s.Keys(ctx)
	if err != nil {
		return Answer{}, s.failed("the keys", err)
	}
	signed, err := keys.SignCode(code)
	if err != nil {
		return Answer{}, s.failed("sign a code", err)
	}
	out.Redirect = "https://" + ch.Host + "/.ferun/auth?code=" + url.QueryEscape(signed)
	s.log.Info("challenge answered", "env", ch.Env, "outcome", a.Outcome, "visit", out.Visit, "by", c.MadeBy)
	return out, nil
}

// GetVisit answers a visit to its provider, ended or not, until it is
// swept: 404 no such visit unless the caller may answer for the visit's
// provider (a tenant principal: of kind tenant, its own tenant's, admin on
// the environment; ferun-auth: kinds ferun and oidc). Its binding is
// never answered.
func (s *Service) GetVisit(ctx context.Context, c Caller, id string) (Visit, error) {
	vid, err := uuid.Parse(id)
	if err != nil {
		return Visit{}, errNoVisit()
	}
	v, err := s.st.GetVisit(ctx, vid)
	if errors.Is(err, store.ErrNoRow) {
		return Visit{}, errNoVisit()
	}
	if err != nil {
		return Visit{}, s.failed("read a visit", err)
	}
	e, err := s.st.GetEnvironment(ctx, v.EnvID)
	if errors.Is(err, store.ErrNoRow) {
		return Visit{}, errNoVisit()
	}
	if err != nil {
		return Visit{}, s.failed("read an environment", err)
	}
	if e.Owner != v.Tenant || !c.answers(e, v.ProviderKind) {
		return Visit{}, errNoVisit()
	}
	return Visit{ID: v.ID.String(), Environment: envView(e), Host: v.Host,
		Provider: ProviderRef{Name: v.Provider, Kind: v.ProviderKind}, Subject: v.Subject, Email: v.Email, Name: v.Name,
		Session: v.Session, CreatedAt: rfc3339(v.CreatedAt), EndsAt: rfc3339(v.EndsAt), EndedAt: rfc3339(v.EndedAt),
		EndReason: v.EndReason}, nil
}

// EndBySession ends with backchannel_logout the open visits of an
// issuer's session (ferun-auth's back-channel logout): of the provider
// ferun, every tenant's ({"provider": "ferun", "session"}), or of a
// tenant's oidc provider ({"tenant", "provider", "session"}). 400
// invalid_request without a session or with another shape; 404 no such
// provider for one that does not exist or is not of kind oidc.
func (s *Service) EndBySession(ctx context.Context, in EndSession) (int, error) {
	bad := func(msg string) error { return apierr.WithCode(http.StatusBadRequest, CodeInvalidRequest, "%s", msg) }
	switch {
	case in.Session == "" || len(in.Session) > maxSession:
		return 0, bad("a session of 1 to 255 bytes is required")
	case in.Provider == "":
		return 0, bad("a provider is required")
	case in.Provider == model.ProviderFerun && in.Tenant != "":
		return 0, bad("the provider ferun is every tenant's: no tenant goes with it")
	case in.Provider != model.ProviderFerun && in.Tenant == "":
		return 0, bad("a tenant goes with a provider of kind oidc")
	}
	kind := model.ProviderKindFerun
	if in.Provider != model.ProviderFerun {
		p, err := s.st.GetAccessProvider(ctx, in.Tenant, in.Provider)
		if errors.Is(err, store.ErrNoRow) || (err == nil && p.Kind != model.ProviderKindOIDC) {
			return 0, errNoProvider()
		}
		if err != nil {
			return 0, s.failed("read a provider", err)
		}
		kind = model.ProviderKindOIDC
	}
	n, err := s.st.EndSessionVisits(ctx, kind, in.Tenant, in.Provider, in.Session, model.VisitBackchannelLogout)
	if err != nil {
		return 0, s.failed("end the visits of a session", err)
	}
	if n > 0 {
		s.log.Info("visits ended by a back-channel logout", "tenant", in.Tenant, "provider", in.Provider, "visits", n)
	}
	return n, nil
}

// OIDCProvider answers a tenant's oidc provider, with its secret, to
// ferun-auth (for an issuer's back-channel logout): 404 no such provider
// unless it exists and is of kind oidc.
func (s *Service) OIDCProvider(ctx context.Context, tenant, name string) (OIDCProvider, error) {
	if !providerName.MatchString(name) {
		return OIDCProvider{}, errNoProvider()
	}
	p, err := s.st.GetAccessProvider(ctx, tenant, name)
	if errors.Is(err, store.ErrNoRow) || (err == nil && p.Kind != model.ProviderKindOIDC) {
		return OIDCProvider{}, errNoProvider()
	}
	if err != nil {
		return OIDCProvider{}, s.failed("read a provider", err)
	}
	return s.oidcOf(p)
}
