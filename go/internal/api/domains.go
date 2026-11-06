// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/domains"
)

// Domains is the domains service (*domains.Service, ADR 0021): one method
// per domain route, tenant class. nil: no domain route is registered.
type Domains interface {
	CreateDomain(ctx context.Context, tenant string, req domains.CreateRequest) (domains.Domain, error)
	ListDomains(ctx context.Context, tenant, env string, labels map[string]string) (domains.List, error)
	GetDomain(ctx context.Context, tenant, id string) (domains.Domain, error)
	BindDomain(ctx context.Context, tenant, id, env string) (domains.Domain, error)
	UnbindDomain(ctx context.Context, tenant, id string) (domains.Domain, error)
	CheckDomain(ctx context.Context, tenant, id string) (domains.Domain, error)
	DeleteDomain(ctx context.Context, tenant, id string) error
}

// domainRoutes registers the domains' routes, tenant class, each a thin
// adapter of a request to one method of Deps.Domains; none without the
// service.
//
// A claim is the tenant's (ADR 0021): claiming, checking and deleting
// need admin on the tenant; a binding admin on the environments it leaves
// and joins, which the handler checks; the list and a domain answer what
// the principal may read: every domain with read on the tenant, else
// those bound to an environment it reads.
func (s *server) domainRoutes(tenant func(n need, pattern string, h apiHandler)) {
	if s.deps.Domains == nil {
		return
	}
	bindings := need{role: admin, scope: scopeHandler}
	tenant(onTenant(admin), "POST /api/v1/domains", s.createDomain)
	tenant(member, "GET /api/v1/domains", s.listDomains)
	tenant(member, "GET /api/v1/domains/{id}", s.getDomain)
	tenant(onTenant(admin), "DELETE /api/v1/domains/{id}", s.deleteDomain)
	tenant(bindings, "PUT /api/v1/domains/{id}/environment", s.bindDomain)
	tenant(bindings, "DELETE /api/v1/domains/{id}/environment", s.unbindDomain)
	tenant(onTenant(admin), "POST /api/v1/domains/{id}/check", s.checkDomain)
}

// mayReadDomain reports whether the request's principal may read d.
func mayReadDomain(r *http.Request, d domains.Domain) bool {
	t, _ := auth.TenantFrom(r.Context())
	if t.MayOnTenant(read) {
		return true
	}
	return d.Environment != nil && mayReadEnv(r, *d.Environment)
}

// errNoDomain is the domains' answer for one the principal may not see.
func errNoDomain() error { return apierr.New(http.StatusNotFound, "no such domain") }

// visibleDomain reads the domain of the path for a principal that may
// read it; 404 else.
func (s *server) visibleDomain(r *http.Request) (domains.Domain, error) {
	d, err := s.deps.Domains.GetDomain(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		return domains.Domain{}, err
	}
	if !mayReadDomain(r, d) {
		return domains.Domain{}, errNoDomain()
	}
	return d, nil
}

// allowBinding checks admin on the environment a domain is bound to, if
// any, and on the one it is to be bound to (ref, "" for none).
func (s *server) allowBinding(r *http.Request, ref string) error {
	t, _ := auth.TenantFrom(r.Context())
	if t.MayOnTenant(admin) {
		return nil
	}
	d, err := s.visibleDomain(r)
	if err != nil {
		return err
	}
	if d.Environment != nil {
		id, err := uuid.Parse(*d.Environment)
		if err != nil {
			return fmt.Errorf("domain %s: environment %q: %w", d.ID, *d.Environment, err)
		}
		if err := allowEnvID(t, id, admin); err != nil {
			return err
		}
	}
	if ref == "" {
		return nil
	}
	return s.allowEnv(r, t, ref, admin)
}

func (s *server) createDomain(w http.ResponseWriter, r *http.Request) error {
	var req domains.CreateRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	d, err := s.deps.Domains.CreateDomain(r.Context(), tenantOf(r), req)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, d)
}

// listDomains is GET /api/v1/domains?environment=<env>&label=k=v…: every
// label must match.
func (s *server) listDomains(w http.ResponseWriter, r *http.Request) error {
	labels := map[string]string{}
	for _, l := range r.URL.Query()["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return apierr.New(http.StatusBadRequest, "label must be key=value, got %q", l)
		}
		labels[k] = v
	}
	list, err := s.deps.Domains.ListDomains(r.Context(), tenantOf(r), r.URL.Query().Get("environment"), labels)
	if err != nil {
		return err
	}
	readable := list.Domains[:0]
	for _, d := range list.Domains {
		if mayReadDomain(r, d) {
			readable = append(readable, d)
		}
	}
	list.Domains = readable
	return writeJSON(w, http.StatusOK, list)
}

func (s *server) getDomain(w http.ResponseWriter, r *http.Request) error {
	d, err := s.visibleDomain(r)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}

func (s *server) deleteDomain(w http.ResponseWriter, r *http.Request) error {
	if err := s.deps.Domains.DeleteDomain(r.Context(), tenantOf(r), r.PathValue("id")); err != nil {
		return err
	}
	return noContent(w, http.StatusNoContent)
}

// bindDomain is PUT …/environment {"environment": "<id or name>"}.
func (s *server) bindDomain(w http.ResponseWriter, r *http.Request) error {
	var req domains.BindRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	if err := s.allowBinding(r, req.Environment); err != nil {
		return err
	}
	d, err := s.deps.Domains.BindDomain(r.Context(), tenantOf(r), r.PathValue("id"), req.Environment)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}

func (s *server) unbindDomain(w http.ResponseWriter, r *http.Request) error {
	if err := s.allowBinding(r, ""); err != nil {
		return err
	}
	d, err := s.deps.Domains.UnbindDomain(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}

func (s *server) checkDomain(w http.ResponseWriter, r *http.Request) error {
	d, err := s.deps.Domains.CheckDomain(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}
