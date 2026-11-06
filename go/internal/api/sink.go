// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/store"
)

// ferund's routes of the sink (ADR 0026, docs/architecture/sink.md): the
// tenant of a token, which the sink asks for its readers; the
// certificates of its instances, under their service tokens; and the
// operators' list and revoke of those tokens (`ferund service-token`
// creates them).

// The service certificates' fixed limits: a certificate lives 24 h (the
// sink asks again at half of it), and a token gets at most
// serviceCertsPerHour of them in an hour.
const (
	serviceCertTTL      = 24 * time.Hour
	serviceCertsPerHour = 10
)

func (s *server) sinkRoutes(admin, service func(pattern string, h apiHandler),
	tenant func(n need, pattern string, h apiHandler)) {
	tenant(member, "GET /api/v1/tenant", s.getTenant)
	service("POST /api/v1/service-certificates", s.issueServiceCert)
	admin("GET /api/v1/admin/service-tokens", s.listServiceTokens)
	admin("DELETE /api/v1/admin/service-tokens/{id}", s.revokeServiceToken)
}

// service is the service class: the bearer is a service token
// (service_tokens), which the route's handler resolves.
func (s *server) service(h apiHandler) http.Handler {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		if bearer(r) == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
			return apierr.New(http.StatusUnauthorized, "service token required")
		}
		return h(w, r)
	})
}

// getTenant is GET /api/v1/tenant: the tenant of the request's token.
func (s *server) getTenant(w http.ResponseWriter, r *http.Request) error {
	return writeJSON(w, http.StatusOK, struct {
		Tenant string `json:"tenant"`
	}{tenantOf(r)})
}

// issueServiceCert is POST /api/v1/service-certificates {csr}: the
// certificate of the token's holder for the CSR's key, valid 24 h, its
// one URI spiffe://<td>/sink/<name>; the chain without the root and the
// bundle, in PEM. An unknown or revoked token is 401; the eleventh
// certificate of a token within an hour is 429.
func (s *server) issueServiceCert(w http.ResponseWriter, r *http.Request) error {
	tok := bearer(r)
	if !strings.HasPrefix(tok, pki.ServiceTokenPrefix(model.ServiceSink)) {
		return apierr.New(http.StatusUnauthorized, "unknown service token")
	}
	if s.deps.Issuer == nil {
		return errUnavailable()
	}
	var req struct {
		CSR string `json:"csr"`
	}
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	t, ok, err := s.st.UseServiceToken(r.Context(), store.HashToken(tok), serviceCertsPerHour, time.Hour)
	if errors.Is(err, store.ErrNoRow) {
		return apierr.New(http.StatusUnauthorized, "unknown service token")
	}
	if err != nil {
		return fmt.Errorf("service token lookup: %w", err)
	}
	if !ok {
		return apierr.New(http.StatusTooManyRequests, "more than %d certificates within an hour under this token", serviceCertsPerHour)
	}
	block, _ := pem.Decode([]byte(req.CSR))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return apierr.New(http.StatusBadRequest, "csr must be a PEM CERTIFICATE REQUEST")
	}
	if t.Kind != model.ServiceSink {
		return apierr.New(http.StatusUnauthorized, "unknown service token")
	}
	pc, leaf, err := s.deps.Issuer.IssueSink(block.Bytes, t.Name, serviceCertTTL)
	if err != nil {
		return apierr.New(http.StatusBadRequest, "%v", err)
	}
	serial := pki.Serial(leaf)
	s.log.Info("service certificate issued", "kind", t.Kind, "name", t.Name, "serial", serial)
	s.record(r.Context(), store.ActivityRow{Kind: "service_cert", Category: "credential",
		Name:   fmt.Sprintf("%s %s certificate %s", t.Kind, t.Name, serial),
		Detail: detail(map[string]any{"kind": t.Kind, "name": t.Name, "serial": serial, "token": t.ID.String()})})
	pemOf := func(ders [][]byte) string {
		var b strings.Builder
		for _, d := range ders {
			b.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d}))
		}
		return b.String()
	}
	return writeJSON(w, http.StatusOK, struct {
		Certificate string `json:"certificate"`
		Bundle      string `json:"bundle"`
		NotAfter    string `json:"notAfter"`
	}{pemOf(pc.Chain), pemOf(pc.TrustBundle), rfc3339(leaf.NotAfter)})
}

// listServiceTokens is GET /api/v1/admin/service-tokens: every service
// token, oldest first, never the token itself.
func (s *server) listServiceTokens(w http.ResponseWriter, r *http.Request) error {
	ts, err := s.st.ListServiceTokens(r.Context())
	if err != nil {
		return fmt.Errorf("list service tokens: %w", err)
	}
	type serviceTokenInfo struct {
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		CreatedBy  string `json:"createdBy"`
		CreatedAt  string `json:"createdAt"`
		LastUsedAt string `json:"lastUsedAt"`
	}
	out := struct {
		ServiceTokens []serviceTokenInfo `json:"serviceTokens"`
	}{ServiceTokens: make([]serviceTokenInfo, 0, len(ts))}
	for _, t := range ts {
		out.ServiceTokens = append(out.ServiceTokens, serviceTokenInfo{ID: t.ID.String(), Kind: t.Kind, Name: t.Name,
			CreatedBy: t.CreatedBy, CreatedAt: rfc3339(t.CreatedAt), LastUsedAt: rfc3339(t.LastUsedAt)})
	}
	return writeJSON(w, http.StatusOK, out)
}

// revokeServiceToken is DELETE /api/v1/admin/service-tokens/{id}:
// {revoked}, as a join token's revoke answers. The certificates issued
// under it live on until their end (24 h at most).
func (s *server) revokeServiceToken(w http.ResponseWriter, r *http.Request) error {
	revoked := false
	if id, err := uuid.Parse(r.PathValue("id")); err == nil {
		t, err := s.st.DeleteServiceToken(r.Context(), id)
		switch {
		case err == nil:
			revoked = true
			by := adminOfRequest(r)
			s.log.Info("service token revoked", "kind", t.Kind, "name", t.Name, "by", by)
			s.record(r.Context(), store.ActivityRow{Kind: "service_token_revoked", Category: "credential",
				Name:   fmt.Sprintf("%s token %s revoked by %s", t.Kind, t.Name, by),
				Detail: detail(map[string]any{"kind": t.Kind, "name": t.Name, "id": t.ID.String(), "by": by})})
		case !errors.Is(err, store.ErrNoRow):
			return fmt.Errorf("revoke service token: %w", err)
		}
	}
	return writeJSON(w, http.StatusOK, struct {
		Revoked bool `json:"revoked"`
	}{revoked})
}
