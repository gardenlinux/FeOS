// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ferunauth

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/gardenlinux/feos/go/internal/oidc"
)

// This file takes the issuers' back-channel logouts (OpenID Connect
// Back-Channel Logout 1.0): the installation's issuer at /backchannel/ferun,
// a tenant's provider of kind oidc at /backchannel/<tenant>/<provider>.
// ferund ends the visits of the session the logout token names.

// backchannelPrefix starts a tenant provider's back-channel path.
const backchannelPrefix = "/backchannel/"

// maxLogoutBody bounds a back-channel call's form.
const maxLogoutBody = 64 << 10

// nameRE is a tenant's or a provider's name: a DNS label.
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// logoutToken reads the form's logout_token; "" when there is none.
func logoutToken(w http.ResponseWriter, r *http.Request) string {
	r.Body = http.MaxBytesReader(w, r.Body, maxLogoutBody)
	if err := r.ParseForm(); err != nil {
		return ""
	}
	return r.PostForm.Get("logout_token")
}

// backchannelFerun is POST /backchannel/ferun: a logout at the
// installation's issuer ends the visits of kind ferun of its session.
func (s *Server) backchannelFerun(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Issuer == "" {
		notFound(w)
		return
	}
	p := s.issuers.get(s.cfg.Issuer, s.cfg.ClientID, s.sec.clientSecret)
	s.backchannel(w, r, p, endBody{Provider: kindFerun})
}

// backchannelTenant is POST /backchannel/<tenant>/<provider>, rest being
// the escaped path after /backchannel/: a logout at a tenant's issuer ends
// the visits of that provider of its session.
func (s *Server) backchannelTenant(w http.ResponseWriter, r *http.Request, rest string) {
	tenant, name, ok := strings.Cut(rest, "/")
	if !ok || !nameRE.MatchString(tenant) || !nameRE.MatchString(name) {
		notFound(w)
		return
	}
	prov, err := s.ferund.oidcProvider(r.Context(), tenant, name)
	switch {
	case statusOf(err) == http.StatusNotFound:
		notFound(w)
		return
	case err != nil:
		s.backchannelFailed(w, "read the provider", err)
		return
	}
	if prov.Issuer == "" || prov.ClientID == "" || !isHTTPS(prov.Issuer) {
		s.log.Error("ferun-auth: ferund gave a provider of kind oidc without its issuer and client", "tenant", tenant, "provider", name)
		jsonAnswer(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	p := s.issuers.get(prov.Issuer, prov.ClientID, prov.ClientSecret)
	s.backchannel(w, r, p, endBody{Tenant: tenant, Provider: name})
}

// backchannel verifies the logout token with p and ends the visits of its
// session: 200; a token that does not verify 400; an issuer or ferund
// that fails 503, which the issuer may try again.
func (s *Server) backchannel(w http.ResponseWriter, r *http.Request, p *oidc.Provider, end endBody) {
	raw := logoutToken(w, r)
	if raw == "" {
		jsonAnswer(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	d, err := discover(r.Context(), p)
	if err != nil {
		s.log.Warn("ferun-auth: back-channel logout: the issuer cannot be used", "tenant", end.Tenant, "provider", end.Provider, "err", err)
		jsonAnswer(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	sid, _, err := p.VerifyLogout(r.Context(), d, raw)
	if err != nil {
		var re *oidc.ErrRefused
		if errors.As(err, &re) {
			s.log.Warn("ferun-auth: back-channel logout refused", "tenant", end.Tenant, "provider", end.Provider, "reason", re.Reason)
			jsonAnswer(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		s.log.Warn("ferun-auth: back-channel logout: the issuer cannot be used", "tenant", end.Tenant, "provider", end.Provider, "err", err)
		jsonAnswer(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	end.Session = sid
	n, err := s.ferund.endVisits(r.Context(), end)
	if err != nil {
		s.backchannelFailed(w, "end the visits", err)
		return
	}
	s.log.Info("ferun-auth: back-channel logout", "tenant", end.Tenant, "provider", end.Provider, "ended", n)
	noStore(w)
	w.WriteHeader(http.StatusOK)
}

// backchannelFailed answers a failed call to ferund on the back channel.
func (s *Server) backchannelFailed(w http.ResponseWriter, what string, err error) {
	if refusedToken(err) {
		s.log.Error("ferund refused ferun-auth's token", "call", what, "err", err)
	} else {
		s.log.Warn("ferun-auth: back-channel logout: ferund failed", "call", what, "err", err)
	}
	jsonAnswer(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
}
