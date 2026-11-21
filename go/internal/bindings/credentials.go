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

// The HTTP secrets of an environment (ADR 0055; ADR 0033 named them its
// credentials): an HTTP API's definition that an environment states once,
// versioned, its credential sealed per version. A secret of an
// environment is a value (internal/env) or an HTTP secret, one name space
// of both. The environments' secrets routes reach them through
// internal/env (HTTPSecrets, PutHTTPSecret, …); the credentials routes of
// ADR 0033 stay one release as their aliases, under /api/v1/environments/
// {env}, where api resolves the environment and checks the grant on it.
//
//	GET    …/credentials                environment read   the latest of each, with their uses
//	GET    …/credentials/{name}         environment read   one
//	PUT    …/credentials/{name}         environment write  a new version
//	DELETE …/credentials/{name}         environment write  every version, while unused
//	POST   …/credentials/{name}/check   environment write  the preflight of the stored latest version, or of a draft
//	POST   …/secrets/{name}/check       environment write  the same, of an HTTP secret (CheckSecret)
//
// A sandbox of the environment names the HTTP secrets its pod gets
// (SetSandboxSecrets), at their latest version, beside its own HTTP set;
// a new version is sent to its running pods at once. A service or job
// references one by a variable {secret, key}, pinned by its deployment
// (internal/env).

const tableCredentials = "env_credentials"

// Env is the environment a credentials route acts in, resolved by api
// from the path's {env} once the grant on it is checked.
type Env struct {
	ID      uuid.UUID
	Name    string
	Project string
}

// The codes of the credentials beyond the bindings' own.
const (
	codeCredentialInUse = "credential_in_use"
	codeNoCredential    = "no_credential"
	// CodeSecretKind: 409 (a write) or 422 (a sandbox's name) for a
	// secret of the other kind than the request needs (ADR 0055).
	CodeSecretKind = "secret_kind"
	// CodeNoSecret: 404 or 422 for a secret the environment does not
	// have.
	CodeNoSecret    = "no_secret"
	codeSecretInUse = "secret_in_use"
)

// words are how a route names an HTTP secret: the secrets routes say
// "HTTP secret", the credentials routes of ADR 0033, their aliases for
// one release, "credential", each with its codes.
type words struct {
	noun, noCode, inUseCode string
}

var (
	credentialWords = words{noun: "credential", noCode: codeNoCredential, inUseCode: codeCredentialInUse}
	secretWords     = words{noun: "HTTP secret", noCode: CodeNoSecret, inUseCode: codeSecretInUse}
)

func (w words) none(name string) error {
	return apierr.WithCode(http.StatusNotFound, w.noCode, "no such %s: %s", w.noun, name)
}

// errIsAValue is the 409 of an HTTP secret's write to a name a value
// secret of the environment holds.
func errIsAValue(name string) error {
	return apierr.WithCode(http.StatusConflict, CodeSecretKind, "secret %s is a value; a secret's kind does not "+
		"change: delete it and make a new one", name)
}

// credentialAAD binds a sealed credential to its environment, name and
// version.
func credentialAAD(env uuid.UUID, name string, version int, kind string) []byte {
	return []byte("ferun/" + tableCredentials + "/v1\x00" + env.String() + "\x00" + name + "\x00" +
		strconv.Itoa(version) + "\x00" + kind)
}

// CredentialUse is how an environment's deployments use one of its HTTP
// secrets: the services and jobs of its latest deployment that get it
// ("service/app", by a variable that references it or a binding that
// names it), the bindings of its latest or deployed deployment that name
// it (credentialRef, ADR 0033), and the version each of the two
// deployments pins (0: none).
type CredentialUse struct {
	Objects  []string
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

// SecretUses are where an HTTP secret is used in its environment: the
// sandboxes that name it, the services and jobs of the latest deployment
// that get it, the bindings of the latest or deployed deployment that
// name it (credentialRef), and the version each of the two deployments
// pins (0: none).
type SecretUses struct {
	Sandboxes []string `json:"sandboxes"`
	Objects   []string `json:"objects"`
	Bindings  []string `json:"bindings"`
	Latest    int      `json:"latest"`
	Deployed  int      `json:"deployed"`
}

// used reports whether anything uses the secret.
func (u SecretUses) used() bool {
	return len(u.Sandboxes) > 0 || len(u.Bindings) > 0 || len(u.Objects) > 0 || u.Latest > 0 || u.Deployed > 0
}

// HTTPSecretDef is an HTTP secret's definition as an answer carries it,
// every secret field {"set": true}, with its uses.
type HTTPSecretDef struct {
	Upstream      string            `json:"upstream"`
	Mode          string            `json:"mode"`
	CaPem         string            `json:"caPem"`
	PathPrefixes  []string          `json:"pathPrefixes"`
	ForwardedHost bool              `json:"forwardedHost"`
	RateLimit     *HTTPRateLimitOut `json:"rateLimit"`
	Credential    HTTPCredentialOut `json:"credential"`
	UsedBy        SecretUses        `json:"usedBy"`
}

// HTTPSecret is the latest version of an HTTP secret as the answers
// carry it.
type HTTPSecret struct {
	Name      string
	Version   int
	CreatedBy string
	CreatedAt time.Time
	Def       HTTPSecretDef
}

// credentialOut is an HTTP secret as the credentials routes answer it.
type credentialOut struct {
	Name string `json:"name"`
	HTTPSecretDef
	Version   int       `json:"version"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

func (h HTTPSecret) credentialOut() credentialOut {
	return credentialOut{Name: h.Name, HTTPSecretDef: h.Def, Version: h.Version, CreatedBy: h.CreatedBy,
		CreatedAt: h.CreatedAt}
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
// environment's binding that names it, or the HTTP secret a variable
// references, named by the secret (env.Service). model.ErrNotFound when
// there is no such version.
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

func (s *Service) secretOf(c store.EnvCredential, b httpBinding, uses SecretUses) HTTPSecret {
	var none podid.ID
	o := s.httpSetOf(none, "", []httpBinding{b}).Bindings[0]
	return HTTPSecret{Name: c.Name, Version: c.Version, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt,
		Def: HTTPSecretDef{Upstream: o.Upstream, Mode: o.Mode, CaPem: o.CaPem, PathPrefixes: o.PathPrefixes,
			ForwardedHost: o.ForwardedHost, RateLimit: o.RateLimit, Credential: o.Credential, UsedBy: uses}}
}

// uses reads where each of an environment's credentials is used,
// through st.
func (s *Service) uses(ctx context.Context, st *store.Store, env uuid.UUID) (map[string]SecretUses, error) {
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
	out := map[string]SecretUses{}
	for name, ids := range sbs {
		u := out[name]
		for _, id := range ids {
			u.Sandboxes = append(u.Sandboxes, id.String())
		}
		out[name] = u
	}
	for name, d := range deps {
		u := out[name]
		u.Objects, u.Bindings = slices.Clone(d.Objects), slices.Clone(d.Bindings)
		u.Latest, u.Deployed = d.Latest, d.Deployed
		out[name] = u
	}
	return out, nil
}

func usesOf(m map[string]SecretUses, name string) SecretUses {
	u := m[name]
	if u.Sandboxes == nil {
		u.Sandboxes = []string{}
	}
	if u.Objects == nil {
		u.Objects = []string{}
	}
	if u.Bindings == nil {
		u.Bindings = []string{}
	}
	return u
}

// HTTPSecrets are the latest versions of the environment's HTTP secrets,
// every secret field {"set": true}, with their uses, by name.
func (s *Service) HTTPSecrets(ctx context.Context, env uuid.UUID) ([]HTTPSecret, error) {
	cs, err := s.st.EnvCredentials(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	uses, err := s.uses(ctx, s.st, env)
	if err != nil {
		return nil, err
	}
	out := []HTTPSecret{}
	for _, c := range cs {
		b, err := s.openCredential(c)
		if err != nil {
			return nil, err
		}
		out = append(out, s.secretOf(c, b, usesOf(uses, c.Name)))
	}
	return out, nil
}

// HTTPSecret is the latest version of one of the environment's HTTP
// secrets with its uses; model.ErrNotFound when it has none of the name.
func (s *Service) HTTPSecret(ctx context.Context, env uuid.UUID, name string) (HTTPSecret, error) {
	c, err := s.st.EnvCredential(ctx, env, name, 0)
	if err != nil {
		return HTTPSecret{}, err
	}
	b, err := s.openCredential(c)
	if err != nil {
		return HTTPSecret{}, err
	}
	uses, err := s.uses(ctx, s.st, env)
	if err != nil {
		return HTTPSecret{}, err
	}
	return s.secretOf(c, b, usesOf(uses, name)), nil
}

// ListCredentials answers the latest version of each of the environment's
// credentials, every secret field {"set": true}, with their uses.
func (s *Service) ListCredentials(w http.ResponseWriter, r *http.Request, env Env) {
	if s.off(w, r) {
		return
	}
	hs, err := s.HTTPSecrets(r.Context(), env.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]credentialOut, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.credentialOut())
	}
	apierr.WriteJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

// GetCredential answers one credential's latest version.
func (s *Service) GetCredential(w http.ResponseWriter, r *http.Request, env Env) {
	if s.off(w, r) {
		return
	}
	name := r.PathValue("name")
	h, err := s.HTTPSecret(r.Context(), env.ID, name)
	if errors.Is(err, model.ErrNotFound) {
		s.fail(w, r, credentialWords.none(name))
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("credential %s: %w", name, err))
		return
	}
	apierr.WriteJSON(w, http.StatusOK, h.credentialOut())
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
// same kind that sends them to the same API: its origin, CA and token URL
// are prev's (movedFrom). A stored secret goes nowhere its version did not
// send it: a check sends its draft, a PUT gives it to the pods, and either
// needs only write on the environment, which never reads a secret. How the
// code calls the API (the mode), the forwarded host, the prefixes and the
// rate limit change while keeping it (ADR 0055): the key goes to the same
// API.
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
			"whose %s is another; re-enter the secret fields when the upstream, CA or token URL change",
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

// movedFrom is the first of in's upstream (its origin, canonical), CA
// (but for the white space around it) and token URL that is not prev's,
// "" when all are: where a secret goes and whom it trusts there. A draft
// whose upstream is no valid one moved.
func movedFrom(in httpBindingIn, prev httpBinding) string {
	up, cerr := httpcheck.ParseUpstream("upstream", in.Upstream)
	if cerr != nil || up.Origin() != prev.Upstream {
		return "upstream"
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

// StripType takes a draft's "type" out of raw, the body of a secret's
// PUT or a check's draft under the secrets routes: "http" or none is the
// definition without it, a value secret's body (another type, or a
// "value") is 409 secret_kind.
func StripType(name string, raw []byte) ([]byte, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest)
	}
	if t, ok := body["type"]; ok {
		var typ string
		if json.Unmarshal(t, &typ) != nil || typ != "http" {
			return nil, apierr.WithCode(http.StatusBadRequest, codeInvalidBinding,
				"type: an HTTP secret's draft is of type \"http\", not %s", t)
		}
		delete(body, "type")
	}
	if _, ok := body["value"]; ok {
		return nil, apierr.WithCode(http.StatusBadRequest, codeInvalidBinding,
			"value: an HTTP secret %s has no value, its definition has the credential", name)
	}
	return json.Marshal(body)
}

// PutHTTPSecret writes a new version of an HTTP secret of environment
// env: raw is the whole definition, a secret field {"set": true} for the
// latest version's value; by is who wrote it. It answers the latest
// version and whether this made it: the same definition again makes
// none. A new version is sent to every running pod of the environment's
// sandboxes that name it. 409 secret_kind while a value secret of the
// environment holds the name; 400 and 422 with the bindings' codes.
func (s *Service) PutHTTPSecret(ctx context.Context, env uuid.UUID, name string, raw []byte, by string) (
	HTTPSecret, bool, error) {
	if !s.enabled {
		return HTTPSecret{}, false, bindingErrf(codeInvalidBinding, "HTTP bindings are not enabled on this platform").
			as(http.StatusUnprocessableEntity)
	}
	in, err := s.draft(ctx, env, name, raw)
	if err != nil {
		return HTTPSecret{}, false, err
	}
	b, hosts, berr := validateHTTPBinding(in, true)
	if berr != nil {
		return HTTPSecret{}, false, berr.as(http.StatusUnprocessableEntity)
	}
	for _, host := range hosts {
		if berr := s.dest.checkDestination(ctx, host); berr != nil {
			return HTTPSecret{}, false, berr.as(http.StatusUnprocessableEntity)
		}
	}

	var stored store.EnvCredential
	created := false
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		created = false
		if err := tx.LockEnvCredential(ctx, env, name); err != nil {
			return err
		}
		// One name space of both kinds (ADR 0055), under the lock a
		// value's insert takes too (migration 0040).
		if isValue, err := tx.ValueSecretExists(ctx, env, name); err != nil {
			return err
		} else if isValue {
			return errIsAValue(name)
		}
		cur, err := tx.EnvCredential(ctx, env, name, 0)
		switch {
		case err == nil:
			if old, oerr := s.openCredential(cur); oerr == nil && sameHTTP([]httpBinding{old}, []httpBinding{b}) {
				stored = cur
				return nil
			}
		case !errors.Is(err, model.ErrNotFound):
			return err
		}
		// One more than any the name had: a credential deleted and made
		// again does not start at 1 again.
		version, err := tx.NextEnvCredentialVersion(ctx, env, name)
		if err != nil {
			return err
		}
		row, err := s.sealCredential(env, version, b)
		if err != nil {
			return err
		}
		row.CreatedBy = by
		if err := tx.InsertEnvCredential(ctx, row); err != nil {
			if store.SecretKindConflict(err) {
				return errIsAValue(name)
			}
			return err
		}
		stored, err = tx.EnvCredential(ctx, env, name, version)
		created = err == nil
		return err
	})
	if err != nil {
		var ae *apierr.Error
		if errors.As(err, &ae) {
			return HTTPSecret{}, false, err
		}
		return HTTPSecret{}, false, fmt.Errorf("credential %s: %w", name, err)
	}
	if created {
		s.rotated(ctx, env, name)
	}
	uses, err := s.uses(ctx, s.st, env)
	if err != nil {
		return HTTPSecret{}, false, err
	}
	return s.secretOf(stored, b, usesOf(uses, name)), created, nil
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
	h, created, err := s.PutHTTPSecret(r.Context(), env.ID, name, raw, auth.MadeBy(r.Context()))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	apierr.WriteJSON(w, status, h.credentialOut())
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

// DeleteHTTPSecret removes every version of an HTTP secret that no
// sandbox of the environment names and neither its latest nor its
// deployed deployment pins: false when the environment has none of the
// name; 409 secret_in_use, naming its users, while one does.
func (s *Service) DeleteHTTPSecret(ctx context.Context, env uuid.UUID, name string) (bool, error) {
	return s.deleteCredential(ctx, env, name, secretWords)
}

func (s *Service) deleteCredential(ctx context.Context, env uuid.UUID, name string, w words) (bool, error) {
	found := false
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		if err := tx.LockEnvCredential(ctx, env, name); err != nil {
			return err
		}
		// In the transaction, under the lock that SetSandboxCredentials
		// and an apply that pins the credential take too.
		uses, err := s.uses(ctx, tx, env)
		if err != nil {
			return err
		}
		if u := uses[name]; u.used() {
			var who []string
			for _, id := range u.Sandboxes {
				who = append(who, "sandbox "+id)
			}
			for _, o := range u.Objects {
				who = append(who, strings.Replace(o, "/", " ", 1))
			}
			for _, b := range u.Bindings {
				who = append(who, "binding "+b)
			}
			if u.Latest > 0 {
				who = append(who, fmt.Sprintf("the latest deployment (version %d)", u.Latest))
			}
			if u.Deployed > 0 && u.Deployed != u.Latest {
				who = append(who, fmt.Sprintf("the deployed deployment (version %d)", u.Deployed))
			}
			return apierr.WithCode(http.StatusConflict, w.inUseCode,
				"%s %s is in use by %s; take it from them first", w.noun, name, strings.Join(who, ", "))
		}
		ok, err := tx.DeleteEnvCredential(ctx, env, name)
		found = ok
		return err
	})
	return found, err
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
	ok, err := s.deleteCredential(r.Context(), env.ID, name, credentialWords)
	if err == nil && !ok {
		err = credentialWords.none(name)
	}
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
	s.check(w, r, env, credentialWords)
}

// CheckSecret is POST /api/v1/environments/{env}/secrets/{name}/check:
// CheckCredential of an HTTP secret, its draft's "type" "http" allowed;
// 409 secret_kind for a value secret, 404 no_secret for neither.
func (s *Service) CheckSecret(w http.ResponseWriter, r *http.Request, env Env) {
	s.check(w, r, env, secretWords)
}

func (s *Service) check(w http.ResponseWriter, r *http.Request, env Env, wd words) {
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
	if wd == secretWords {
		isValue, err := s.st.ValueSecretExists(r.Context(), env.ID, name)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if isValue {
			s.fail(w, r, apierr.WithCode(http.StatusConflict, CodeSecretKind,
				"secret %s is a value: only an HTTP secret is checked", name))
			return
		}
	}
	if raw := bytes.TrimSpace(body.Binding); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		if wd == secretWords {
			var err error
			if raw, err = StripType(name, raw); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		in, err := s.draft(r.Context(), env.ID, name, raw)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		release, err := s.takePreflight(env.Project)
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
		s.fail(w, r, wd.none(name))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.PreflightHTTP(r.Context(), env.Project, p, body.Probe)
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
// surface's code; a name the environment has no credential of is
// no_credential, as it was (the sandbox's credentials of ADR 0033, an
// alias of its secrets for one release).
func (s *Service) CheckSandboxCredentials(ctx context.Context, env uuid.UUID, names []string) error {
	_, err := s.sandboxCredentials(ctx, env, names, credentialWords)
	return err
}

// CheckSandboxSecrets refuses a list of HTTP secrets a sandbox of
// environment env cannot name (ADR 0055): as CheckSandboxCredentials,
// and a value secret is 422 secret_kind, a name the environment has no
// secret of 422 no_secret.
func (s *Service) CheckSandboxSecrets(ctx context.Context, env uuid.UUID, names []string) error {
	_, err := s.sandboxCredentials(ctx, env, names, secretWords)
	return err
}

func (s *Service) sandboxCredentials(ctx context.Context, env uuid.UUID, names []string, w words) ([]httpBinding,
	error) {
	if !s.enabled {
		if len(names) == 0 {
			return nil, nil
		}
		return nil, bindingErrf(codeInvalidBinding, "HTTP bindings are not enabled on this platform").
			as(http.StatusUnprocessableEntity)
	}
	if len(names) > httpcheck.MaxBindings {
		return nil, bindingErrf(codeInvalidBinding, "at most %d %ss per sandbox", httpcheck.MaxBindings, w.noun).
			as(http.StatusUnprocessableEntity)
	}
	seen, resolved := map[string]bool{}, map[string]string{}
	var out []httpBinding
	for _, name := range names {
		if seen[name] {
			return nil, bindingErrf(codeInvalidBinding, "%s %s is named twice", w.noun, name).
				as(http.StatusUnprocessableEntity)
		}
		seen[name] = true
		c, err := s.st.EnvCredential(ctx, env, name, 0)
		if errors.Is(err, model.ErrNotFound) {
			return nil, s.noSandboxSecret(ctx, env, name, w)
		}
		if err != nil {
			return nil, err
		}
		b, err := s.openCredential(c)
		if err != nil {
			return nil, err
		}
		if other, ok := resolved[b.resolvedName()]; ok {
			return nil, bindingErrf(codeInvalidBinding, "%ss %s and %s both make the pod resolve %s", w.noun, other,
				name, b.resolvedName()).as(http.StatusUnprocessableEntity)
		}
		resolved[b.resolvedName()] = name
		out = append(out, b)
	}
	return out, nil
}

// noSandboxSecret is the 422 of a name a sandbox cannot name, which is no
// HTTP secret of its environment: secret_kind for a value secret under
// the secrets' words, else no_secret (no_credential).
func (s *Service) noSandboxSecret(ctx context.Context, env uuid.UUID, name string, w words) error {
	if w == secretWords {
		isValue, err := s.st.ValueSecretExists(ctx, env, name)
		if err != nil {
			return err
		}
		if isValue {
			return apierr.WithCode(http.StatusUnprocessableEntity, CodeSecretKind, "secret %s is a value: a "+
				"sandbox names HTTP secrets of its environment, and a value secret reaches no sandbox", name)
		}
	}
	return apierr.WithCode(http.StatusUnprocessableEntity, w.noCode, "no such %s in the sandbox's environment: %s",
		w.noun, name)
}

// SetSandboxCredentials states the credentials of its environment env a
// sandbox of project names (none: an empty list), refused like
// CheckSandboxCredentials and, beyond, when one shares a name with the
// sandbox's own HTTP set or a name an environment gives the sandbox; then
// its pod is sent them.
func (s *Service) SetSandboxCredentials(ctx context.Context, project string, env uuid.UUID, id podid.ID,
	names []string) error {
	return s.setSandboxCredentials(ctx, project, env, id, names, credentialWords)
}

// SetSandboxSecrets states the HTTP secrets of its environment env a
// sandbox of project names (ADR 0055): SetSandboxCredentials with the
// secrets' words and codes (CheckSandboxSecrets).
func (s *Service) SetSandboxSecrets(ctx context.Context, project string, env uuid.UUID, id podid.ID,
	names []string) error {
	return s.setSandboxCredentials(ctx, project, env, id, names, secretWords)
}

func (s *Service) setSandboxCredentials(ctx context.Context, project string, env uuid.UUID, id podid.ID,
	names []string, w words) error {
	bs, err := s.sandboxCredentials(ctx, env, names, w)
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
		taken, err := s.sandboxNames(ctx, id, project)
		if err != nil {
			return fmt.Errorf("the names of the sandbox's environments: %w", err)
		}
		for _, b := range bs {
			if mine[b.Name] || mine[b.resolvedName()] {
				return bindingErrf(codeNameTaken, "%s %s: the sandbox's own HTTP set has a binding that "+
					"resolves %s", w.noun, b.Name, b.resolvedName()).as(http.StatusConflict)
			}
			if env, ok := taken[b.resolvedName()]; ok {
				return bindingErrf(codeNameTaken, "%s %s: environment %s gives the sandbox the name %s", w.noun,
					b.Name, env, b.resolvedName()).as(http.StatusConflict)
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
				return apierr.WithCode(http.StatusUnprocessableEntity, w.noCode,
					"no such %s in the sandbox's environment: %s", w.noun, name)
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
