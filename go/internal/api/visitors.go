// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"reflect"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/visits"
)

// ferund's routes of visitors (ADR 0054, docs/architecture/control-plane.md,
// "Visitors"): a tenant's access providers (tenant class), the challenges
// a provider opens, accepts and rejects and the visits it reads (provider
// class), and ferun-auth's own routes (auth class). Each handler decodes
// its body into the visits service's types, says who calls (a
// visits.Caller) and writes the service's answer as it comes: who may
// answer for which environment is the service's to decide. A sealed
// challenge travels in POST bodies only, never in a path, and is never
// logged.

// maxVisitorBody bounds the body of a visitors route.
const maxVisitorBody = 64 << 10

// visitorRoutes registers the routes of visitors, only with the visits
// service: without it (ferund without --secret-key-file) they fall to the
// /api/ catch-all, 404 no such route.
func (s *server) visitorRoutes(tenant func(n need, pattern string, h apiHandler),
	add func(c class, pattern string, h http.Handler), public func(pattern string, h http.Handler)) {
	v := s.deps.Visitors
	if vs, ok := v.(*visits.Service); v == nil || (ok && vs == nil) {
		return
	}
	provider := func(pattern string, h apiHandler) { add(classProvider, pattern, s.provider(h)) }
	authOnly := func(pattern string, h apiHandler) { add(classAuth, pattern, s.authOnly(h)) }

	// The tenant's providers: every principal of the tenant reads them
	// (an environment's admin chooses among them), its admins make,
	// change and delete them.
	tenant(member, "GET /api/v1/providers", s.listProviders)
	tenant(member, "GET /api/v1/providers/{provider}", s.getProvider)
	tenant(onTenant(admin), "PUT /api/v1/providers/{provider}", s.putProvider)
	tenant(onTenant(admin), "DELETE /api/v1/providers/{provider}", s.deleteProvider)

	// A provider's side of a sign-in, and its read of a visit.
	provider("POST /api/v1/challenges/open", s.openChallenge)
	provider("POST /api/v1/challenges/accept", s.acceptChallenge)
	provider("POST /api/v1/challenges/reject", s.rejectChallenge)
	provider("GET /api/v1/visits/{visit}", s.getVisit)

	// The key ferund signs its questions to the providers of kind tenant
	// with, for them to verify: public, as the ingress's /.ferun/keys.
	public("GET /api/v1/visit-keys", s.wrap(s.visitKeys))

	// ferun-auth's own: an issuer's back-channel logout.
	authOnly("POST /api/v1/visits/end", s.endVisits)
	authOnly("GET /api/v1/oidc-providers/{tenant}/{provider}", s.oidcProvider)
}

// decodeVisitorBody reads a visitors route's body (at most
// maxVisitorBody) into v, as decodeJSON does: unknown fields are ignored.
func decodeVisitorBody(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxVisitorBody+1))
	if err != nil {
		return apierr.New(http.StatusBadRequest, "read body: %v", err)
	}
	if len(raw) > maxVisitorBody {
		return apierr.New(http.StatusRequestEntityTooLarge, "request body larger than %d bytes", maxVisitorBody)
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	_, err = decodeJSON(r, v)
	return err
}

// callerOf is who calls a provider route: ferun-auth (its service
// token), or a tenant's principal, which answers only where it is admin
// on the environment.
func callerOf(r *http.Request) visits.Caller {
	if svc, ok := auth.ServiceFrom(r.Context()); ok {
		return visits.Caller{Auth: true, MadeBy: "service:" + svc.ID.String()}
	}
	t, _ := auth.TenantFrom(r.Context())
	return visits.Caller{
		Tenant: t.Name,
		Admin:  func(env uuid.UUID) bool { return t.EnvRole(env) >= auth.RoleAdmin },
		MadeBy: t.Principal.String(),
	}
}

// listProviders is GET /api/v1/providers: the tenant's providers, ferun
// first, the kinds this installation serves and the redirect URI of
// ferun-auth.
func (s *server) listProviders(w http.ResponseWriter, r *http.Request) error {
	out, err := s.deps.Visitors.ListProviders(r.Context(), tenantOf(r))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

// getProvider is GET /api/v1/providers/{provider}.
func (s *server) getProvider(w http.ResponseWriter, r *http.Request) error {
	p, err := s.deps.Visitors.GetProvider(r.Context(), tenantOf(r), r.PathValue("provider"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

// putProvider is PUT /api/v1/providers/{provider}: 201 with the provider
// made, 200 with one changed or found as asked. A make or a change is an
// activity row of the tenant; a PUT that changes nothing (FeCode's
// registration at every start) is none.
func (s *server) putProvider(w http.ResponseWriter, r *http.Request) error {
	var in visits.ProviderIn
	if err := decodeVisitorBody(r, &in); err != nil {
		return err
	}
	ctx, tenant, name := r.Context(), tenantOf(r), r.PathValue("provider")
	t, _ := auth.TenantFrom(ctx)
	by := t.Principal.String()
	before, beforeErr := s.deps.Visitors.GetProvider(ctx, tenant, name)
	p, created, err := s.deps.Visitors.PutProvider(ctx, tenant, name, in, by)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	// Made, or changed: what the provider says differs, or it was
	// written (updated_at, in seconds; it alone shows a new secret). A
	// pre-read that failed tells nothing, so it records nothing.
	if created || beforeErr == nil && (!sameProvider(before, p) || before.UpdatedAt != p.UpdatedAt) {
		verb := "changed"
		if created {
			verb = "made"
		}
		s.record(ctx, store.ActivityRow{TenantID: tenant, Kind: "access_provider_put", Category: "credential",
			Name:   fmt.Sprintf("access provider %s (%s) %s by %s", p.Name, p.Kind, verb, by),
			Detail: detail(map[string]any{"provider": p.Name, "kind": p.Kind, "created": created, "by": by})})
	}
	return writeJSON(w, status, p)
}

// sameProvider reports whether two answers say the same provider: its
// kind, address, issuer, client, whether a secret is stored, and rule.
func sameProvider(a, b visits.Provider) bool {
	return a.Kind == b.Kind && a.URL == b.URL && a.Issuer == b.Issuer && a.ClientID == b.ClientID &&
		(a.ClientSecret != nil && a.ClientSecret.Set) == (b.ClientSecret != nil && b.ClientSecret.Set) &&
		reflect.DeepEqual(a.Rule, b.Rule)
}

// deleteProvider is DELETE /api/v1/providers/{provider}: {"deleted":
// true}, an activity row of the tenant.
func (s *server) deleteProvider(w http.ResponseWriter, r *http.Request) error {
	ctx, tenant, name := r.Context(), tenantOf(r), r.PathValue("provider")
	if err := s.deps.Visitors.DeleteProvider(ctx, tenant, name); err != nil {
		return err
	}
	by := auth.MadeBy(ctx)
	s.record(ctx, store.ActivityRow{TenantID: tenant, Kind: "access_provider_deleted", Category: "credential",
		Name:   fmt.Sprintf("access provider %s deleted by %s", name, by),
		Detail: detail(map[string]any{"provider": name, "by": by})})
	return writeJSON(w, http.StatusOK, struct {
		Deleted bool `json:"deleted"`
	}{true})
}

// challengeBody is the body of open and reject: the sealed challenge.
type challengeBody struct {
	Challenge string `json:"challenge"`
}

// openChallenge is POST /api/v1/challenges/open {"challenge"}: what the
// challenge asks for, to a caller that answers for its environment.
func (s *server) openChallenge(w http.ResponseWriter, r *http.Request) error {
	var in challengeBody
	if err := decodeVisitorBody(r, &in); err != nil {
		return err
	}
	ch, err := s.deps.Visitors.OpenChallenge(r.Context(), callerOf(r), in.Challenge)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, ch)
}

// acceptChallenge is POST /api/v1/challenges/accept: the provider admits
// a subject; the answer is where the browser goes.
func (s *server) acceptChallenge(w http.ResponseWriter, r *http.Request) error {
	var in visits.Accept
	if err := decodeVisitorBody(r, &in); err != nil {
		return err
	}
	ans, err := s.deps.Visitors.AcceptChallenge(r.Context(), callerOf(r), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, ans)
}

// rejectChallenge is POST /api/v1/challenges/reject {"challenge"}.
func (s *server) rejectChallenge(w http.ResponseWriter, r *http.Request) error {
	var in challengeBody
	if err := decodeVisitorBody(r, &in); err != nil {
		return err
	}
	ans, err := s.deps.Visitors.RejectChallenge(r.Context(), callerOf(r), in.Challenge)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, ans)
}

// getVisit is GET /api/v1/visits/{visit}: a provider reads a visit it
// answers for (its renewal's ask names it).
func (s *server) getVisit(w http.ResponseWriter, r *http.Request) error {
	v, err := s.deps.Visitors.GetVisit(r.Context(), callerOf(r), r.PathValue("visit"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

// endVisits is POST /api/v1/visits/end: an issuer's back-channel logout
// ends the visits of its session; {"ended": n}.
func (s *server) endVisits(w http.ResponseWriter, r *http.Request) error {
	var in visits.EndSession
	if err := decodeVisitorBody(r, &in); err != nil {
		return err
	}
	n, err := s.deps.Visitors.EndBySession(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, struct {
		Ended int `json:"ended"`
	}{n})
}

// oidcProvider is GET /api/v1/oidc-providers/{tenant}/{provider}: a
// tenant's provider of kind oidc with its secret, to verify its issuer's
// back-channel logout.
func (s *server) oidcProvider(w http.ResponseWriter, r *http.Request) error {
	p, err := s.deps.Visitors.OIDCProvider(r.Context(), r.PathValue("tenant"), r.PathValue("provider"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

// visitKeys answers the token key as a JWKS (GET /api/v1/visit-keys), the
// document the ingress serves at /.ferun/keys: a provider of kind tenant
// verifies ferund's signed questions with it (ADR 0054).
func (s *server) visitKeys(w http.ResponseWriter, r *http.Request) error {
	jwks, err := s.deps.Visitors.JWKS(r.Context())
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(jwks)
	return nil
}
