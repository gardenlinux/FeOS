// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/bindings/httpcheck"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Credentials of a tenant (ADR 0033): an HTTP binding's definition that
// a tenant states once, versioned, its credential sealed per version.
//
//	GET    /api/v1/credentials                tenant read   the latest of each, with their uses
//	GET    /api/v1/credentials/{name}         tenant read   one
//	PUT    /api/v1/credentials/{name}         tenant write  a new version
//	DELETE /api/v1/credentials/{name}         tenant write  every version, while unused
//	POST   /api/v1/credentials/{name}/check   tenant write  the preflight with the stored credential
//
// A sandbox names the credentials its pod gets (SetSandboxCredentials),
// at their latest version, beside its own HTTP set; a new version is sent
// to its running pods at once. An environment's binding names one in its
// spec, pinned by its deployment (internal/env).

const tableCredentials = "tenant_credentials"

// The codes of the credentials beyond the bindings' own.
const (
	codeCredentialInUse = "credential_in_use"
	codeNoCredential    = "no_credential"
)

// credentialAAD binds a sealed credential to its tenant, name and
// version.
func credentialAAD(tenant, name string, version int, kind string) []byte {
	return []byte("ferun/" + tableCredentials + "/v1\x00" + tenant + "\x00" + name + "\x00" + strconv.Itoa(version) +
		"\x00" + kind)
}

// EnvCredentialUse is an environment whose latest or deployed deployment
// pins a credential: the bindings that name it and the versions pinned.
type EnvCredentialUse struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Bindings []string `json:"bindings"`
	// Latest is the version the latest deployment pins (0: none), Deployed
	// the deployed one's.
	Latest   int `json:"latest"`
	Deployed int `json:"deployed"`
}

// CredentialUsers tells which environments use a tenant's credentials
// (*env.Service), by the credential's name.
type CredentialUsers interface {
	CredentialUses(ctx context.Context, tenant string) (map[string][]EnvCredentialUse, error)
}

// SetCredentialUsers sets what answers the environments' uses; nil: none
// are known, and a delete checks the sandboxes only.
func (s *Service) SetCredentialUsers(u CredentialUsers) {
	s.hooks.Lock()
	defer s.hooks.Unlock()
	s.credUsers = u
}

func (s *Service) credentialUsers() CredentialUsers {
	s.hooks.Lock()
	defer s.hooks.Unlock()
	return s.credUsers
}

// credentialUses are where a credential is used.
type credentialUses struct {
	Sandboxes    []string           `json:"sandboxes"`
	Environments []EnvCredentialUse `json:"environments"`
}

type credentialOut struct {
	httpBindingOut
	Version   int            `json:"version"`
	CreatedBy string         `json:"createdBy"`
	CreatedAt time.Time      `json:"createdAt"`
	UsedBy    credentialUses `json:"usedBy"`
}

// ------------------------------------------------------------- sealing --

func (s *Service) sealCredential(tenant string, version int, b httpBinding) (store.TenantCredential, error) {
	pt, err := json.Marshal(b.Credential)
	if err != nil {
		return store.TenantCredential{}, err
	}
	sealed, err := s.sealer.Seal(pt, credentialAAD(tenant, b.Name, version, b.Credential.Kind))
	if err != nil {
		return store.TenantCredential{}, fmt.Errorf("seal credential %s: %w", b.Name, err)
	}
	return store.TenantCredential{Tenant: tenant, Name: b.Name, Version: version, Upstream: b.Upstream, Mode: b.Mode,
		CAPEM: b.CAPEM, PathPrefixes: b.PathPrefixes, ForwardedHost: b.ForwardedHost, RatePerSecond: b.RatePerSecond,
		RateBurst: b.RateBurst, Kind: b.Credential.Kind, Sealed: sealed}, nil
}

// openCredential opens a stored version into a binding named as the
// credential.
func (s *Service) openCredential(c store.TenantCredential) (httpBinding, error) {
	pt, err := s.sealer.Open(c.Sealed, credentialAAD(c.Tenant, c.Name, c.Version, c.Kind))
	if err != nil {
		return httpBinding{}, fmt.Errorf("credential %s version %d: %w", c.Name, c.Version, err)
	}
	var cred httpCredential
	if err := json.Unmarshal(pt, &cred); err != nil {
		return httpBinding{}, fmt.Errorf("credential %s version %d: %v", c.Name, c.Version, err)
	}
	b := httpBinding{Name: c.Name, Upstream: c.Upstream, Mode: c.Mode, CAPEM: c.CAPEM,
		PathPrefixes: slices.Clone(c.PathPrefixes), ForwardedHost: c.ForwardedHost, RatePerSecond: c.RatePerSecond,
		RateBurst: c.RateBurst, Credential: cred}
	if b.PathPrefixes == nil {
		b.PathPrefixes = []string{}
	}
	if up, cerr := httpcheck.ParseUpstream("upstream", c.Upstream); cerr == nil {
		b.Host = up.Host
	}
	return b, nil
}

// OpenCredential is a version of a tenant's credential (0: the latest)
// as a pod receives it, under the binding's name as: an environment's
// binding that names it (env.Service). model.ErrNotFound when there is no
// such version.
func (s *Service) OpenCredential(ctx context.Context, tenant, name string, version int, as string) (
	*fleetv1.HttpBinding, error) {
	if s.sealer == nil {
		return nil, errors.New("no secret key to open credentials with")
	}
	c, err := s.st.TenantCredential(ctx, tenant, name, version)
	if err != nil {
		return nil, err
	}
	b, err := s.openCredential(c)
	if err != nil {
		return nil, err
	}
	b.Name = as
	return b.proto(), nil
}

// ------------------------------------------------------------- answers --

func (s *Service) credentialOf(c store.TenantCredential, b httpBinding, uses credentialUses) credentialOut {
	var none podid.ID
	set := s.httpSetOf(none, "", []httpBinding{b})
	return credentialOut{httpBindingOut: set.Bindings[0], Version: c.Version, CreatedBy: c.CreatedBy,
		CreatedAt: c.CreatedAt, UsedBy: uses}
}

// uses reads where each of a tenant's credentials is used.
func (s *Service) uses(ctx context.Context, tenant string) (map[string]credentialUses, error) {
	sbs, err := s.st.CredentialSandboxes(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("the credentials' sandboxes: %w", err)
	}
	var envs map[string][]EnvCredentialUse
	if u := s.credentialUsers(); u != nil {
		if envs, err = u.CredentialUses(ctx, tenant); err != nil {
			return nil, fmt.Errorf("the credentials' environments: %w", err)
		}
	}
	out := map[string]credentialUses{}
	for name, ids := range sbs {
		u := out[name]
		for _, id := range ids {
			u.Sandboxes = append(u.Sandboxes, id.String())
		}
		out[name] = u
	}
	for name, es := range envs {
		u := out[name]
		u.Environments = es
		out[name] = u
	}
	return out, nil
}

func usesOf(m map[string]credentialUses, name string) credentialUses {
	u := m[name]
	if u.Sandboxes == nil {
		u.Sandboxes = []string{}
	}
	if u.Environments == nil {
		u.Environments = []EnvCredentialUse{}
	}
	return u
}

func tenantOf(r *http.Request) string {
	t, _ := auth.TenantFrom(r.Context())
	return t.Name
}

func noSuchCredential(name string) error {
	return apierr.WithCode(http.StatusNotFound, codeNoCredential, "no such credential: %s", name)
}

// ListCredentials answers the latest version of each of the tenant's
// credentials, every secret field {"set": true}, with their uses.
func (s *Service) ListCredentials(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	tenant := tenantOf(r)
	cs, err := s.st.TenantCredentials(r.Context(), tenant)
	if err != nil {
		s.fail(w, r, fmt.Errorf("credentials: %w", err))
		return
	}
	uses, err := s.uses(r.Context(), tenant)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []credentialOut{}
	for _, c := range cs {
		b, err := s.openCredential(c)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, s.credentialOf(c, b, usesOf(uses, c.Name)))
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

// GetCredential answers one credential's latest version.
func (s *Service) GetCredential(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	tenant, name := tenantOf(r), r.PathValue("name")
	c, err := s.st.TenantCredential(r.Context(), tenant, name, 0)
	if errors.Is(err, model.ErrNotFound) {
		s.fail(w, r, noSuchCredential(name))
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("credential %s: %w", name, err))
		return
	}
	b, err := s.openCredential(c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	uses, err := s.uses(r.Context(), tenant)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, s.credentialOf(c, b, usesOf(uses, name)))
}

// keptSecrets takes a credential's secret fields sent as {"set": true}
// out of a PUT's body: the body with each such field empty, and their
// paths ("password", "headers.X-Api-Key").
func keptSecrets(raw []byte) ([]byte, []string, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, nil, err
	}
	cr, ok := body["credential"]
	if !ok || bytes.Equal(bytes.TrimSpace(cr), []byte("null")) {
		return raw, nil, nil
	}
	var cred map[string]json.RawMessage
	if err := json.Unmarshal(cr, &cred); err != nil {
		return nil, nil, fmt.Errorf("credential: %v", err)
	}
	isSet := func(v json.RawMessage) bool {
		var m struct {
			Set bool `json:"set"`
		}
		return json.Unmarshal(v, &m) == nil && m.Set && bytes.HasPrefix(bytes.TrimSpace(v), []byte("{"))
	}
	var kept []string
	for _, f := range []string{"password", "clientSecret", "keyPem"} {
		if v, ok := cred[f]; ok && isSet(v) {
			cred[f] = json.RawMessage(`""`)
			kept = append(kept, f)
		}
	}
	if hv, ok := cred["headers"]; ok {
		var hs map[string]json.RawMessage
		if err := json.Unmarshal(hv, &hs); err == nil {
			for k, v := range hs {
				if isSet(v) {
					hs[k] = json.RawMessage(`""`)
					kept = append(kept, "headers."+k)
				}
			}
			if cred["headers"], err = json.Marshal(hs); err != nil {
				return nil, nil, err
			}
		}
	}
	var err error
	if body["credential"], err = json.Marshal(cred); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(body)
	return out, kept, err
}

// keep fills the kept secret fields of in from prev, a credential of the
// same kind.
func keep(in *httpBindingIn, kept []string, prev *httpBinding) *bindingErr {
	if len(kept) == 0 {
		return nil
	}
	if prev == nil || in.Credential == nil || prev.Credential.Kind != in.Credential.Kind {
		return bindingErrf(codeInvalidBinding, "%s: {\"set\": true} keeps a value of the latest version, which "+
			"has none of this kind; send the value", kept[0])
	}
	pc := prev.Credential
	for _, k := range kept {
		switch {
		case k == "password":
			in.Credential.Password = pc.Password
		case k == "clientSecret":
			in.Credential.ClientSecret = pc.ClientSecret
		case k == "keyPem":
			in.Credential.KeyPem = pc.KeyPEM
		case strings.HasPrefix(k, "headers."):
			h := strings.TrimPrefix(k, "headers.")
			v, ok := pc.Headers[h]
			if !ok {
				return bindingErrf(codeInvalidBinding, "credential.headers.%s: the latest version has no such "+
					"header to keep; send its value", h)
			}
			in.Credential.Headers[h] = v
		}
	}
	return nil
}

// PutCredential writes a new version of a credential: the whole
// definition, a secret field {"set": true} for the latest version's
// value. The same definition again answers the latest, 200; a new
// version 201, and every running pod that names it is sent it.
func (s *Service) PutCredential(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	tenant, name := tenantOf(r), r.PathValue("name")
	raw, err := readBody(r)
	if err != nil {
		apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
		return
	}
	raw, kept, err := keptSecrets(raw)
	if err != nil {
		apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
		return
	}
	var in httpBindingIn
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
		return
	}
	if in.Name != "" && in.Name != name {
		apierr.Write(w, bindingErrf(codeInvalidBinding, "name: %q is not the path's %q", in.Name, name).
			as(http.StatusBadRequest))
		return
	}
	in.Name = name

	var prev *httpBinding
	latest, err := s.st.TenantCredential(r.Context(), tenant, name, 0)
	if err == nil {
		b, oerr := s.openCredential(latest)
		if oerr != nil && len(kept) > 0 {
			s.fail(w, r, fmt.Errorf("%w; send every secret field", oerr))
			return
		}
		if oerr == nil {
			prev = &b
		}
	} else if !errors.Is(err, model.ErrNotFound) {
		s.fail(w, r, fmt.Errorf("credential %s: %w", name, err))
		return
	}
	if berr := keep(&in, kept, prev); berr != nil {
		apierr.Write(w, berr.as(http.StatusUnprocessableEntity))
		return
	}
	b, hosts, berr := validateHTTPBinding(in, true)
	if berr != nil {
		apierr.Write(w, berr.as(http.StatusUnprocessableEntity))
		return
	}
	for _, host := range hosts {
		if berr := s.dest.checkDestination(r.Context(), host); berr != nil {
			apierr.Write(w, berr.as(http.StatusUnprocessableEntity))
			return
		}
	}

	var stored store.TenantCredential
	created := false
	err = s.st.InTx(r.Context(), func(tx *store.Store) error {
		created = false
		if err := tx.LockTenantCredential(r.Context(), tenant, name); err != nil {
			return err
		}
		cur, err := tx.TenantCredential(r.Context(), tenant, name, 0)
		version := 1
		switch {
		case err == nil:
			if old, oerr := s.openCredential(cur); oerr == nil && sameHTTP([]httpBinding{old}, []httpBinding{b}) {
				stored = cur
				return nil
			}
			version = cur.Version + 1
		case !errors.Is(err, model.ErrNotFound):
			return err
		}
		row, err := s.sealCredential(tenant, version, b)
		if err != nil {
			return err
		}
		row.CreatedBy = auth.MadeBy(r.Context())
		if err := tx.InsertTenantCredential(r.Context(), row); err != nil {
			return err
		}
		stored, err = tx.TenantCredential(r.Context(), tenant, name, version)
		created = err == nil
		return err
	})
	if err != nil {
		s.fail(w, r, fmt.Errorf("credential %s: %w", name, err))
		return
	}
	if created {
		s.rotated(r.Context(), tenant, name)
	}
	uses, err := s.uses(r.Context(), tenant)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	apierr.WriteJSON(w, status, s.credentialOf(stored, b, usesOf(uses, name)))
}

// rotated sends a credential's new version to the running pods of the
// sandboxes that name it: each makes the CAs its interception needs and
// is pushed its bindings' clear part, whose version moved, so that its
// feos-authn fetches them again.
func (s *Service) rotated(ctx context.Context, tenant, name string) {
	sbs, err := s.st.CredentialSandboxes(ctx, tenant)
	if err != nil {
		s.log.Error("credentials: the sandboxes of a new version", "tenant", tenant, "credential", name, "err", err)
		return
	}
	for _, id := range sbs[name] {
		if err := s.ensureSandboxCAs(ctx, id); err != nil {
			s.log.Error("credentials: the interception CAs of a sandbox", "sandbox", id, "err", err)
		}
		s.push(id)
	}
}

// DeleteCredential removes every version of a credential that no sandbox
// names and no environment's latest or deployed deployment pins: 204;
// 409 credential_in_use, naming its users, while one does.
func (s *Service) DeleteCredential(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	tenant, name := tenantOf(r), r.PathValue("name")
	err := s.st.InTx(r.Context(), func(tx *store.Store) error {
		if err := tx.LockTenantCredential(r.Context(), tenant, name); err != nil {
			return err
		}
		uses, err := s.uses(r.Context(), tenant)
		if err != nil {
			return err
		}
		if u := uses[name]; len(u.Sandboxes) > 0 || len(u.Environments) > 0 {
			var who []string
			for _, id := range u.Sandboxes {
				who = append(who, "sandbox "+id)
			}
			for _, e := range u.Environments {
				who = append(who, "environment "+e.Name)
			}
			return apierr.WithCode(http.StatusConflict, codeCredentialInUse,
				"credential %s is in use by %s; take it from them first", name, strings.Join(who, ", "))
		}
		ok, err := tx.DeleteTenantCredential(r.Context(), tenant, name)
		if err != nil {
			return err
		}
		if !ok {
			return noSuchCredential(name)
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CheckCredential runs the preflight of a credential's latest version,
// with an optional probe: {probe}.
func (s *Service) CheckCredential(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	tenant, name := tenantOf(r), r.PathValue("name")
	var body struct {
		Probe *HTTPProbe `json:"probe"`
	}
	if r.ContentLength != 0 {
		if _, err := decodeJSON(r, &body); err != nil {
			apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
			return
		}
	}
	p, err := s.OpenCredential(r.Context(), tenant, name, 0, name)
	if errors.Is(err, model.ErrNotFound) {
		s.fail(w, r, noSuchCredential(name))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.PreflightHTTP(r.Context(), tenant, p, body.Probe)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, res)
}

// ------------------------------------------------------------ sandboxes --

// SandboxCredentialNames are the credentials a sandbox names.
func (s *Service) SandboxCredentialNames(ctx context.Context, id podid.ID) ([]string, error) {
	return s.st.SandboxCredentialNames(ctx, id)
}

// CheckSandboxCredentials refuses a list of credentials a sandbox of
// tenant cannot name: an unknown one, a duplicate, or two that make its
// pod resolve the same name. 422 with the binding surface's code.
func (s *Service) CheckSandboxCredentials(ctx context.Context, tenant string, names []string) error {
	_, err := s.sandboxCredentials(ctx, tenant, names)
	return err
}

func (s *Service) sandboxCredentials(ctx context.Context, tenant string, names []string) ([]httpBinding, error) {
	if !s.enabled {
		if len(names) == 0 {
			return nil, nil
		}
		return nil, bindingErrf(codeInvalidBinding, "HTTP bindings are not enabled on this platform").
			as(http.StatusUnprocessableEntity)
	}
	if len(names) > httpcheck.MaxBindings {
		return nil, bindingErrf(codeInvalidBinding, "at most %d credentials per sandbox", httpcheck.MaxBindings).
			as(http.StatusUnprocessableEntity)
	}
	seen, resolved := map[string]bool{}, map[string]string{}
	var out []httpBinding
	for _, name := range names {
		if seen[name] {
			return nil, bindingErrf(codeInvalidBinding, "credential %s is named twice", name).
				as(http.StatusUnprocessableEntity)
		}
		seen[name] = true
		c, err := s.st.TenantCredential(ctx, tenant, name, 0)
		if errors.Is(err, model.ErrNotFound) {
			return nil, apierr.WithCode(http.StatusUnprocessableEntity, codeNoCredential, "no such credential: %s", name)
		}
		if err != nil {
			return nil, err
		}
		b, err := s.openCredential(c)
		if err != nil {
			return nil, err
		}
		if other, ok := resolved[b.resolvedName()]; ok {
			return nil, bindingErrf(codeInvalidBinding, "credentials %s and %s both make the pod resolve %s", other,
				name, b.resolvedName()).as(http.StatusUnprocessableEntity)
		}
		resolved[b.resolvedName()] = name
		out = append(out, b)
	}
	return out, nil
}

// SetSandboxCredentials states the credentials a sandbox of tenant names
// (none: an empty list), refused like CheckSandboxCredentials and, beyond,
// when one shares a name with the sandbox's own HTTP set or a name an
// environment gives the sandbox; then its pod is sent them.
func (s *Service) SetSandboxCredentials(ctx context.Context, tenant string, id podid.ID, names []string) error {
	bs, err := s.sandboxCredentials(ctx, tenant, names)
	if err != nil {
		return err
	}
	if len(bs) > 0 {
		_, own, err := s.st.HTTPBindings(ctx, id)
		if err != nil {
			return fmt.Errorf("the HTTP set of %s: %w", id, err)
		}
		mine := map[string]bool{}
		for _, r := range own {
			mine[r.Name] = true
			if up, cerr := httpcheck.ParseUpstream("upstream", r.Upstream); cerr == nil && r.Mode == httpcheck.ModeIntercept {
				mine[up.Host] = true
			}
		}
		taken, err := s.sandboxNames(ctx, id, tenant)
		if err != nil {
			return fmt.Errorf("the names of the sandbox's environments: %w", err)
		}
		for _, b := range bs {
			if mine[b.Name] || mine[b.resolvedName()] {
				return bindingErrf(codeNameTaken, "credential %s: the sandbox's own HTTP set has a binding that "+
					"resolves %s", b.Name, b.resolvedName()).as(http.StatusConflict)
			}
			if env, ok := taken[b.resolvedName()]; ok {
				return bindingErrf(codeNameTaken, "credential %s: environment %s gives the sandbox the name %s", b.Name,
					env, b.resolvedName()).as(http.StatusConflict)
			}
		}
	}
	sorted := slices.Sorted(slices.Values(names))
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		if _, err := tx.LockSandbox(ctx, id, store.ForUpdate); err != nil {
			if errors.Is(err, model.ErrNotFound) {
				return noSuchSandbox()
			}
			return err
		}
		return tx.SetSandboxCredentialNames(ctx, id, sorted)
	})
	if err != nil {
		return err
	}
	if err := s.ensureSandboxCAs(ctx, id); err != nil {
		return err
	}
	s.push(id)
	return nil
}

// ensureSandboxCAs makes the interception CA a sandbox's pod needs for
// the hosts its own set and its credentials intercept.
func (s *Service) ensureSandboxCAs(ctx context.Context, id podid.ID) error {
	snap, err := s.snapshot(ctx, id)
	if errors.Is(err, errNoSandbox) {
		return nil
	}
	if err != nil {
		return err
	}
	bs := append(httpPlain(snap.http), credsPlain(snap.creds)...)
	if hosts := interceptHosts(bs); len(hosts) > 0 {
		if _, err := s.ensureCAs(ctx, s.st, id, hosts); err != nil {
			return err
		}
	}
	return nil
}

// credsPlain are a sandbox's credentials as bindings without their
// credentials: what the clear part needs.
func credsPlain(cs []store.TenantCredential) []httpBinding {
	out := make([]httpBinding, 0, len(cs))
	for _, c := range cs {
		b := httpBinding{Name: c.Name, Upstream: c.Upstream, Mode: c.Mode}
		if up, err := httpcheck.ParseUpstream("upstream", c.Upstream); err == nil {
			b.Host = up.Host
		}
		out = append(out, b)
	}
	return out
}

// credsVersion is a sandbox's HTTP version with its credentials: its own
// set's version alone when it names none, else that and each
// credential's name and version.
func credsVersion(own string, cs []store.TenantCredential) string {
	if len(cs) == 0 {
		return own
	}
	var b strings.Builder
	b.WriteString(own)
	b.WriteString("\x00credentials")
	for _, c := range cs {
		fmt.Fprintf(&b, "\x00%s/%d", c.Name, c.Version)
	}
	return b.String()
}

// mergeCreds adds a sandbox's credentials to its own set; a credential
// whose name or resolved name its own set has already is left out and
// logged (a write refuses it, but a new version may change its mode).
func (s *Service) mergeCreds(id podid.ID, own []httpBinding, creds []httpBinding) []httpBinding {
	if len(creds) == 0 {
		return own
	}
	out := slices.Clone(own)
	used := map[string]bool{}
	for _, b := range own {
		used[b.Name], used[b.resolvedName()] = true, true
	}
	for _, c := range creds {
		if used[c.Name] || used[c.resolvedName()] {
			s.log.Warn("credentials: a credential resolves a name the sandbox's own set has; it is left out",
				"sandbox", id, "credential", c.Name)
			continue
		}
		used[c.Name], used[c.resolvedName()] = true, true
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b httpBinding) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// readBody reads a request's body, bounded like decodeJSON's.
func readBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		return nil, fmt.Errorf("read body: %v", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		raw = []byte("{}")
	}
	return raw, nil
}
