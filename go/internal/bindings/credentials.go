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

	"github.com/google/uuid"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/bindings/httpcheck"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Credentials of an environment (ADR 0033): an HTTP binding's definition
// that an environment states once, versioned, its credential sealed per
// version. The routes are under /api/v1/environments/{env}; api resolves
// the environment and checks the grant on it.
//
//	GET    …/credentials                environment read   the latest of each, with their uses
//	GET    …/credentials/{name}         environment read   one
//	PUT    …/credentials/{name}         environment write  a new version
//	DELETE …/credentials/{name}         environment write  every version, while unused
//	POST   …/credentials/{name}/check   environment write  the preflight of the stored latest version, or of a draft
//
// A sandbox of the environment names the credentials its pod gets
// (SetSandboxCredentials), at their latest version, beside its own HTTP
// set; a new version is sent to its running pods at once. The
// environment's binding names one in its spec, pinned by its deployment
// (internal/env).

const tableCredentials = "env_credentials"

// Env is the environment a credentials route acts in, resolved by api
// from the path's {env} once the grant on it is checked.
type Env struct {
	ID     uuid.UUID
	Name   string
	Tenant string
}

// The codes of the credentials beyond the bindings' own.
const (
	codeCredentialInUse = "credential_in_use"
	codeNoCredential    = "no_credential"
)

// credentialAAD binds a sealed credential to its environment, name and
// version.
func credentialAAD(env uuid.UUID, name string, version int, kind string) []byte {
	return []byte("ferun/" + tableCredentials + "/v1\x00" + env.String() + "\x00" + name + "\x00" +
		strconv.Itoa(version) + "\x00" + kind)
}

// CredentialUse is how an environment's deployments use one of its
// credentials: the bindings of its latest or deployed deployment that
// name it, and the version each of the two pins (0: none).
type CredentialUse struct {
	Bindings []string
	Latest   int
	Deployed int
}

// CredentialUsers tells how an environment's deployments use its
// credentials (*env.Service), by the credential's name, read through st
// (a delete's transaction, or the store).
type CredentialUsers interface {
	CredentialUses(ctx context.Context, st *store.Store, env uuid.UUID) (map[string]CredentialUse, error)
}

// SetCredentialUsers sets what answers the deployments' uses; nil: none
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

// credentialUses are where a credential is used in its environment: the
// sandboxes that name it, and the bindings of the latest or deployed
// deployment that name it with the version each pins (0: none).
type credentialUses struct {
	Sandboxes []string `json:"sandboxes"`
	Bindings  []string `json:"bindings"`
	Latest    int      `json:"latest"`
	Deployed  int      `json:"deployed"`
}

// used reports whether anything uses the credential.
func (u credentialUses) used() bool { return len(u.Sandboxes) > 0 || len(u.Bindings) > 0 }

type credentialOut struct {
	httpBindingOut
	Version   int            `json:"version"`
	CreatedBy string         `json:"createdBy"`
	CreatedAt time.Time      `json:"createdAt"`
	UsedBy    credentialUses `json:"usedBy"`
}

// ------------------------------------------------------------- sealing --

func (s *Service) sealCredential(env uuid.UUID, version int, b httpBinding) (store.EnvCredential, error) {
	pt, err := json.Marshal(b.Credential)
	if err != nil {
		return store.EnvCredential{}, err
	}
	sealed, err := s.sealer.Seal(pt, credentialAAD(env, b.Name, version, b.Credential.Kind))
	if err != nil {
		return store.EnvCredential{}, fmt.Errorf("seal credential %s: %w", b.Name, err)
	}
	return store.EnvCredential{EnvID: env, Name: b.Name, Version: version, Upstream: b.Upstream, Mode: b.Mode,
		CAPEM: b.CAPEM, PathPrefixes: b.PathPrefixes, ForwardedHost: b.ForwardedHost, RatePerSecond: b.RatePerSecond,
		RateBurst: b.RateBurst, Kind: b.Credential.Kind, Sealed: sealed}, nil
}

// openCredential opens a stored version into a binding named as the
// credential.
func (s *Service) openCredential(c store.EnvCredential) (httpBinding, error) {
	pt, err := s.sealer.Open(c.Sealed, credentialAAD(c.EnvID, c.Name, c.Version, c.Kind))
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

// OpenCredential is a version of an environment's credential (0: the
// latest) as a pod receives it, under the binding's name as: the
// environment's binding that names it (env.Service). model.ErrNotFound
// when there is no such version.
func (s *Service) OpenCredential(ctx context.Context, env uuid.UUID, name string, version int, as string) (
	*fleetv1.HttpBinding, error) {
	if s.sealer == nil {
		return nil, errors.New("no secret key to open credentials with")
	}
	c, err := s.st.EnvCredential(ctx, env, name, version)
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

// CopyCredential is the latest version of a credential of environment
// from, sealed as version 1 of the same name of environment to: what a
// clone of from inserts in the transaction that makes to. model.ErrNotFound
// when from has no credential of that name.
func (s *Service) CopyCredential(ctx context.Context, from uuid.UUID, name string, to uuid.UUID) (
	store.EnvCredential, error) {
	if s.sealer == nil {
		return store.EnvCredential{}, errors.New("no secret key to open credentials with")
	}
	c, err := s.st.EnvCredential(ctx, from, name, 0)
	if err != nil {
		return store.EnvCredential{}, err
	}
	b, err := s.openCredential(c)
	if err != nil {
		return store.EnvCredential{}, err
	}
	return s.sealCredential(to, 1, b)
}

// ------------------------------------------------------------- answers --

func (s *Service) credentialOf(c store.EnvCredential, b httpBinding, uses credentialUses) credentialOut {
	var none podid.ID
	set := s.httpSetOf(none, "", []httpBinding{b})
	return credentialOut{httpBindingOut: set.Bindings[0], Version: c.Version, CreatedBy: c.CreatedBy,
		CreatedAt: c.CreatedAt, UsedBy: uses}
}

// uses reads where each of an environment's credentials is used,
// through st.
func (s *Service) uses(ctx context.Context, st *store.Store, env uuid.UUID) (map[string]credentialUses, error) {
	sbs, err := st.CredentialSandboxes(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("the credentials' sandboxes: %w", err)
	}
	var deps map[string]CredentialUse
	if u := s.credentialUsers(); u != nil {
		if deps, err = u.CredentialUses(ctx, st, env); err != nil {
			return nil, fmt.Errorf("the credentials' deployments: %w", err)
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
	for name, d := range deps {
		u := out[name]
		u.Bindings, u.Latest, u.Deployed = slices.Clone(d.Bindings), d.Latest, d.Deployed
		out[name] = u
	}
	return out, nil
}

func usesOf(m map[string]credentialUses, name string) credentialUses {
	u := m[name]
	if u.Sandboxes == nil {
		u.Sandboxes = []string{}
	}
	if u.Bindings == nil {
		u.Bindings = []string{}
	}
	return u
}

func noSuchCredential(name string) error {
	return apierr.WithCode(http.StatusNotFound, codeNoCredential, "no such credential: %s", name)
}

// ListCredentials answers the latest version of each of the environment's
// credentials, every secret field {"set": true}, with their uses.
func (s *Service) ListCredentials(w http.ResponseWriter, r *http.Request, env Env) {
	if s.off(w, r) {
		return
	}
	cs, err := s.st.EnvCredentials(r.Context(), env.ID)
	if err != nil {
		s.fail(w, r, fmt.Errorf("credentials: %w", err))
		return
	}
	uses, err := s.uses(r.Context(), s.st, env.ID)
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
func (s *Service) GetCredential(w http.ResponseWriter, r *http.Request, env Env) {
	if s.off(w, r) {
		return
	}
	name := r.PathValue("name")
	c, err := s.st.EnvCredential(r.Context(), env.ID, name, 0)
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
	uses, err := s.uses(r.Context(), s.st, env.ID)
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
// same kind that sends them to the same place: its upstream, mode, CA and
// token URL are prev's (movedFrom). A stored secret goes nowhere its
// version did not send it: a check sends its draft, a PUT gives it to the
// pods, and either needs only write on the environment, which never reads
// a secret.
func keep(in *httpBindingIn, kept []string, prev *httpBinding) *bindingErr {
	if len(kept) == 0 {
		return nil
	}
	if prev == nil || in.Credential == nil || prev.Credential.Kind != in.Credential.Kind {
		return bindingErrf(codeInvalidBinding, "%s: {\"set\": true} keeps a value of the latest version, which "+
			"has none of this kind; send the value", kept[0])
	}
	if field := movedFrom(*in, *prev); field != "" {
		return bindingErrf(codeInvalidBinding, "credential.%s: {\"set\": true} keeps the latest version's value, "+
			"whose %s is another; re-enter the secret fields when the upstream, mode, CA or token URL change",
			kept[0], field)
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
			v, ok := pc.Headers[http.CanonicalHeaderKey(h)] // stored by its canonical name
			if !ok {
				return bindingErrf(codeInvalidBinding, "credential.headers.%s: the latest version has no such "+
					"header to keep; send its value", h)
			}
			in.Credential.Headers[h] = v
		}
	}
	return nil
}

// movedFrom is the first of in's upstream (canonical), mode, CA (but for
// the white space around it) and token URL that is not prev's, "" when
// all are: where a secret goes and whom it trusts there. A draft whose
// upstream or mode is no valid one moved.
func movedFrom(in httpBindingIn, prev httpBinding) string {
	up, cerr := httpcheck.ParseUpstream("upstream", in.Upstream)
	if cerr != nil || up.Origin() != prev.Upstream {
		return "upstream"
	}
	if mode, cerr := httpcheck.Mode("mode", in.Mode, up); cerr != nil || mode != prev.Mode {
		return "mode"
	}
	if strings.TrimSpace(in.CaPem) != strings.TrimSpace(prev.CAPEM) {
		return "caPem"
	}
	if in.Credential.TokenURL != prev.Credential.TokenURL {
		return "tokenUrl"
	}
	return ""
}

// draft reads a credential's definition as a PUT or a check sends it
// (raw), named name: a secret field sent as {"set": true} gets the value
// of the environment's latest version of it. 400 for a body that is no
// definition or names another credential, 422 for a field to keep that
// the latest version lacks or that would go elsewhere (keep).
func (s *Service) draft(ctx context.Context, env uuid.UUID, name string, raw []byte) (httpBindingIn, error) {
	raw, kept, err := keptSecrets(raw)
	if err != nil {
		return httpBindingIn{}, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest)
	}
	var in httpBindingIn
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return httpBindingIn{}, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest)
	}
	if in.Name != "" && in.Name != name {
		return httpBindingIn{}, bindingErrf(codeInvalidBinding, "name: %q is not the path's %q", in.Name, name).
			as(http.StatusBadRequest)
	}
	in.Name = name
	if len(kept) == 0 {
		return in, nil
	}
	var prev *httpBinding
	latest, err := s.st.EnvCredential(ctx, env, name, 0)
	switch {
	case err == nil:
		b, oerr := s.openCredential(latest)
		if oerr != nil {
			return httpBindingIn{}, fmt.Errorf("%w; send every secret field", oerr)
		}
		prev = &b
	case !errors.Is(err, model.ErrNotFound):
		return httpBindingIn{}, fmt.Errorf("credential %s: %w", name, err)
	}
	if berr := keep(&in, kept, prev); berr != nil {
		return httpBindingIn{}, berr.as(http.StatusUnprocessableEntity)
	}
	return in, nil
}

// PutCredential writes a new version of a credential: the whole
// definition, a secret field {"set": true} for the latest version's
// value. The same definition again answers the latest, 200; a new
// version 201, and every running pod of the environment's sandboxes that
// name it is sent it.
func (s *Service) PutCredential(w http.ResponseWriter, r *http.Request, env Env) {
	if s.off(w, r) {
		return
	}
	name := r.PathValue("name")
	raw, err := readBody(r)
	if err != nil {
		apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
		return
	}
	in, err := s.draft(r.Context(), env.ID, name, raw)
	if err != nil {
		s.fail(w, r, err)
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

	var stored store.EnvCredential
	created := false
	err = s.st.InTx(r.Context(), func(tx *store.Store) error {
		created = false
		if err := tx.LockEnvCredential(r.Context(), env.ID, name); err != nil {
			return err
		}
		cur, err := tx.EnvCredential(r.Context(), env.ID, name, 0)
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
		row, err := s.sealCredential(env.ID, version, b)
		if err != nil {
			return err
		}
		row.CreatedBy = auth.MadeBy(r.Context())
		if err := tx.InsertEnvCredential(r.Context(), row); err != nil {
			return err
		}
		stored, err = tx.EnvCredential(r.Context(), env.ID, name, version)
		created = err == nil
		return err
	})
	if err != nil {
		s.fail(w, r, fmt.Errorf("credential %s: %w", name, err))
		return
	}
	if created {
		s.rotated(r.Context(), env.ID, name)
	}
	uses, err := s.uses(r.Context(), s.st, env.ID)
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
// environment's sandboxes that name it: each makes the CAs its
// interception needs and is pushed its bindings' clear part, whose
// version moved, so that its feos-authn fetches them again.
func (s *Service) rotated(ctx context.Context, env uuid.UUID, name string) {
	sbs, err := s.st.CredentialSandboxes(ctx, env)
	if err != nil {
		s.log.Error("credentials: the sandboxes of a new version", "environment", env, "credential", name, "err", err)
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
// of the environment names and neither its latest nor its deployed
// deployment pins: 204; 409 credential_in_use, naming its users, while
// one does.
func (s *Service) DeleteCredential(w http.ResponseWriter, r *http.Request, env Env) {
	if s.off(w, r) {
		return
	}
	name := r.PathValue("name")
	err := s.st.InTx(r.Context(), func(tx *store.Store) error {
		if err := tx.LockEnvCredential(r.Context(), env.ID, name); err != nil {
			return err
		}
		// In the transaction, under the lock that SetSandboxCredentials
		// and an apply that pins the credential take too.
		uses, err := s.uses(r.Context(), tx, env.ID)
		if err != nil {
			return err
		}
		if u := uses[name]; u.used() {
			var who []string
			for _, id := range u.Sandboxes {
				who = append(who, "sandbox "+id)
			}
			for _, b := range u.Bindings {
				who = append(who, "binding "+b)
			}
			return apierr.WithCode(http.StatusConflict, codeCredentialInUse,
				"credential %s is in use by %s; take it from them first", name, strings.Join(who, ", "))
		}
		ok, err := tx.DeleteEnvCredential(r.Context(), env.ID, name)
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

// CheckCredential runs the preflight of a credential: {binding, probe},
// both optional. Without binding, of the stored latest version; with it,
// of that draft of the credential (a definition as a PUT sends it, a
// secret field {"set": true} for the latest version's value), nothing
// stored, so that a form is checked before it saves. A draft's flaws are
// the preflight's answer, as on POST /api/v1/http/preflight.
func (s *Service) CheckCredential(w http.ResponseWriter, r *http.Request, env Env) {
	if s.off(w, r) {
		return
	}
	name := r.PathValue("name")
	var body struct {
		Binding json.RawMessage `json:"binding"`
		Probe   *HTTPProbe      `json:"probe"`
	}
	if r.ContentLength != 0 {
		if _, err := decodeJSON(r, &body); err != nil {
			apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
			return
		}
	}
	if raw := bytes.TrimSpace(body.Binding); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		in, err := s.draft(r.Context(), env.ID, name, raw)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		release, err := s.takePreflight(env.Tenant)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		defer release()
		apierr.WriteJSON(w, http.StatusOK, s.httpPreflight(r.Context(), in, body.Probe))
		return
	}
	p, err := s.OpenCredential(r.Context(), env.ID, name, 0, name)
	if errors.Is(err, model.ErrNotFound) {
		s.fail(w, r, noSuchCredential(name))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.PreflightHTTP(r.Context(), env.Tenant, p, body.Probe)
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
// environment env cannot name: one the environment lacks, a duplicate, or
// two that make its pod resolve the same name. 422 with the binding
// surface's code.
func (s *Service) CheckSandboxCredentials(ctx context.Context, env uuid.UUID, names []string) error {
	_, err := s.sandboxCredentials(ctx, env, names)
	return err
}

func (s *Service) sandboxCredentials(ctx context.Context, env uuid.UUID, names []string) ([]httpBinding, error) {
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
		c, err := s.st.EnvCredential(ctx, env, name, 0)
		if errors.Is(err, model.ErrNotFound) {
			return nil, apierr.WithCode(http.StatusUnprocessableEntity, codeNoCredential,
				"no such credential in the sandbox's environment: %s", name)
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

// SetSandboxCredentials states the credentials of its environment env a
// sandbox of tenant names (none: an empty list), refused like
// CheckSandboxCredentials and, beyond, when one shares a name with the
// sandbox's own HTTP set or a name an environment gives the sandbox; then
// its pod is sent them.
func (s *Service) SetSandboxCredentials(ctx context.Context, tenant string, env uuid.UUID, id podid.ID,
	names []string) error {
	bs, err := s.sandboxCredentials(ctx, env, names)
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
		// A DeleteCredential, which takes the same locks, may have deleted
		// one since it was read above; in name order, as an apply takes
		// them.
		for _, name := range sorted {
			if err := tx.LockEnvCredential(ctx, env, name); err != nil {
				return err
			}
			if _, err := tx.EnvCredential(ctx, env, name, 0); errors.Is(err, model.ErrNotFound) {
				return apierr.WithCode(http.StatusUnprocessableEntity, codeNoCredential,
					"no such credential in the sandbox's environment: %s", name)
			} else if err != nil {
				return fmt.Errorf("credential %s: %w", name, err)
			}
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
func credsPlain(cs []store.EnvCredential) []httpBinding {
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
func credsVersion(own string, cs []store.EnvCredential) string {
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
