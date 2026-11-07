// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/google/uuid"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/bindings"
	"github.com/gardenlinux/feos/go/internal/bindings/httpcheck"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// An environment's bindings (ADR 0028). A pod gets exactly the bindings
// its service or job names, no other: its template lists their names,
// upstreams and modes (what makes it another pod), and the rest, their
// credentials with the secrets' versions among them, comes from the
// deployment its instance (or job run) belongs to whenever the pod asks
// for its bindings, so that a new secret version is sealed to the running
// pod again and replaces none. A secret a binding names enters no
// container's environment unless a service's env names it itself.

// HTTPBindings is what the environments need of the bindings
// (*bindings.Service): whether they are on, the destination rule, and the
// preflight of a binding resolved, for a tenant (an error while it has too
// many in flight).
type HTTPBindings interface {
	Enabled() bool
	CheckDestination(ctx context.Context, host string) error
	PreflightHTTP(ctx context.Context, tenant string, b *fleetv1.HttpBinding, probe *bindings.HTTPProbe) (
		*bindings.HTTPPreflightResult, error)
	// OpenCredential is a version of a tenant's credential as a pod
	// receives it, under the binding's name (ADR 0033).
	OpenCredential(ctx context.Context, tenant, name string, version int, as string) (*fleetv1.HttpBinding, error)
}

// BindingCheckRequest is POST /api/v1/environments/{env}/bindings/{name}/
// check: an optional probe, one GET or HEAD with the credential.
type BindingCheckRequest struct {
	Probe *bindings.HTTPProbe `json:"probe"`
}

// The codes of an environment binding's check that are not the
// preflight's.
const (
	CodeNoBinding         = "no_binding"
	CodeBindingsOff       = "bindings_off"
	CodeBindingNotChecked = "binding_unresolved"
)

// RowHTTPBindings is bindings.EnvSource: the HTTP bindings of an
// environment's pod, those its template names, as the deployment its
// instance or job run belongs to has them, by name, and the version of
// their definitions (a hash of the bindings with their secrets by name and
// pinned version, never a value). open: with the secrets' values. A
// binding the deployment no longer has as the template says (another
// upstream or mode: the pod is being replaced) is left out and logged. A
// binding whose secret's value is not a valid one (a header value with a
// line break) is kept without its credential and logged, opened or not:
// the names and the version are the same in the pod's clear part and in
// what its feos-authn gets, which answers the binding's requests 503
// binding_invalid.
func (s *Service) RowHTTPBindings(ctx context.Context, row store.Sandbox, open bool) ([]*fleetv1.HttpBinding, string,
	error) {
	t := row.PodTemplate
	if !row.Instance || t == nil || len(t.HTTPBindings) == 0 {
		return nil, "", nil
	}
	num, err := s.st.EnvInstanceDeployment(ctx, row.ID)
	if errors.Is(err, store.ErrNoRow) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("the deployment of %s: %w", row.ID, err)
	}
	e, err := s.st.GetEnvironment(ctx, row.EnvironmentID)
	if err != nil {
		return nil, "", fmt.Errorf("environment %s: %w", row.EnvironmentID, err)
	}
	sp, err := s.deploymentSpec(ctx, e, num)
	if err != nil {
		return nil, "", err
	}
	var out []*fleetv1.HttpBinding
	var defs []bindingDef
	for _, tb := range t.HTTPBindings {
		b, ok := sp.Bindings[tb.Name]
		if up, mode := spec.BindingTemplate(b); !ok || up != tb.Upstream || mode != tb.Mode {
			s.log.Warn("a pod's binding is not its deployment's any more; it is left out", "sandbox", row.ID,
				"binding", tb.Name, "deployment", num)
			continue
		}
		p, def, err := s.resolveBinding(ctx, row.EnvironmentID, tb.Name, b, sp.Secrets, open)
		if ce := httpcheck.AsError(err); ce != nil {
			s.log.Warn("a binding's secret is not a valid value; the pod gets the binding without its credential",
				"sandbox", row.ID, "binding", tb.Name, "err", ce.Msg)
			p.Credential = nil
		} else if err != nil {
			return nil, "", err
		}
		out = append(out, p)
		defs = append(defs, def)
	}
	if len(out) == 0 {
		return nil, "", nil
	}
	raw, err := json.Marshal(defs)
	if err != nil {
		return nil, "", fmt.Errorf("the bindings' version: %w", err)
	}
	sum := sha256.Sum256(raw)
	return out, hex.EncodeToString(sum[:]), nil
}

var _ bindings.EnvSource = (*Service)(nil)

// bindingDef is what a binding's version hashes: the binding as the spec
// has it and the versions of the secrets it names.
type bindingDef struct {
	Name     string         `json:"name"`
	Binding  spec.Binding   `json:"binding"`
	Versions map[string]int `json:"versions"`
}

// resolveBinding is a binding as a pod receives it: its secrets at the
// versions pins name (0: the latest), opened when open (else their fields
// are empty), every value checked as the sandbox surface checks it. A
// value that fails its check, or a credential of no known kind, is an
// *httpcheck.Error beside the binding and its definition, both whole: the
// definition names every secret whether open or not.
func (s *Service) resolveBinding(ctx context.Context, envID uuid.UUID, name string, b spec.Binding,
	pins map[string]spec.SecretRef, open bool) (*fleetv1.HttpBinding, bindingDef, error) {
	def := bindingDef{Name: name, Binding: b, Versions: map[string]int{}}
	if ref := b.TenantCredential; ref != nil {
		return s.resolveCredential(ctx, envID, name, b, def, open)
	}
	up, mode := spec.BindingTemplate(b)
	p := &fleetv1.HttpBinding{Name: name, Upstream: up, CaPem: b.CaPem, PathPrefixes: slices.Clone(b.PathPrefixes),
		ForwardedHost: b.ForwardedHost}
	if mode == httpcheck.ModeIntercept {
		p.Mode = fleetv1.HttpBindingMode_HTTP_BINDING_MODE_INTERCEPT
	}
	if rl := b.RateLimit; rl != nil && rl.PerSecond > 0 {
		burst := rl.Burst
		if burst == 0 {
			burst = rl.PerSecond
		}
		p.RateLimit = &fleetv1.RateLimit{PerSecond: uint32(rl.PerSecond), Burst: uint32(burst)}
	}
	c := b.Credential
	if c == nil {
		return p, def, &httpcheck.Error{Code: httpcheck.CodeInvalidBinding, Msg: "the binding has no credential"}
	}
	val := func(field string, v *spec.EnvValue) (string, error) {
		if v == nil {
			return "", nil
		}
		if v.Kind != spec.EnvSecret {
			return v.Value, nil
		}
		version := pins[v.Secret].Version
		def.Versions[v.Secret] = version
		if !open {
			return "", nil
		}
		value, err := s.ctl.ResolveSecret(ctx, envID, v.Secret, version)
		if err != nil {
			return "", fmt.Errorf("binding %s's %s: %w", name, field, err)
		}
		return value, nil
	}
	var err error
	str := func(field string, v *spec.EnvValue) string {
		if err != nil {
			return ""
		}
		var out string
		out, err = val(field, v)
		return out
	}
	// bad is the first opened value that fails its check; the fields after
	// it are still read, so that def names their secrets too.
	var bad *httpcheck.Error
	check := func(cerr *httpcheck.Error) {
		if bad == nil && err == nil && open {
			bad = cerr
		}
	}
	switch c.Kind {
	case httpcheck.KindHeaders:
		h := map[string]string{}
		for _, k := range slices.Sorted(maps.Keys(c.Headers)) {
			v := c.Headers[k]
			h[http.CanonicalHeaderKey(k)] = str("header "+k, &v)
			check(httpcheck.HeaderValue("header "+k, h[http.CanonicalHeaderKey(k)]))
		}
		p.Credential = &fleetv1.HttpBinding_Headers{Headers: &fleetv1.HeaderCredential{Headers: h}}
	case httpcheck.KindBasic:
		bc := &fleetv1.BasicCredential{User: str("user", c.User), Password: str("password", c.Password)}
		check(httpcheck.BasicUser("user", bc.User))
		p.Credential = &fleetv1.HttpBinding_Basic{Basic: bc}
	case httpcheck.KindOAuth2:
		p.Credential = &fleetv1.HttpBinding_Oauth2{Oauth2: &fleetv1.OAuthClientCredentials{
			TokenUrl: str("tokenUrl", c.TokenURL), ClientId: str("clientId", c.ClientID),
			ClientSecret: str("clientSecret", c.ClientSecret), Scope: str("scope", c.Scope),
			Audience: str("audience", c.Audience), ClientAuthInBody: c.ClientAuthInBody}}
	case httpcheck.KindClientCertificate:
		cc := &fleetv1.ClientCertificate{CertPem: str("certPem", c.CertPem), KeyPem: str("keyPem", c.KeyPem)}
		check(httpcheck.ClientCertificate("credential", cc.CertPem, cc.KeyPem))
		p.Credential = &fleetv1.HttpBinding_ClientCertificate{ClientCertificate: cc}
	default:
		return p, def, &httpcheck.Error{Code: httpcheck.CodeInvalidBinding, Msg: "unknown credential kind " + c.Kind}
	}
	if err != nil {
		return nil, def, err
	}
	if bad != nil {
		return p, def, bad
	}
	return p, def, nil
}

// bindingValueFindings are the warnings of a plan's bindings whose secrets,
// at the versions it pins, are no valid values (a header value with a line
// break): their pods would get them without their credentials. A finding
// has the check's words, never the value; a secret that does not resolve
// is the references' finding.
func (s *Service) bindingValueFindings(ctx context.Context, envID uuid.UUID, sp spec.Spec) []spec.Finding {
	var out []spec.Finding
	for _, name := range slices.Sorted(maps.Keys(sp.Bindings)) {
		_, _, err := s.resolveBinding(ctx, envID, name, sp.Bindings[name], sp.Secrets, true)
		if ce := httpcheck.AsError(err); ce != nil {
			out = append(out, spec.Finding{Severity: spec.SeverityWarning, Path: "bindings." + name + ".credential",
				Problem: fmt.Sprintf("a secret it names is no valid value (%s): its pods get the binding without its "+
					"credential, and feos-authn answers its requests 503 binding_invalid", ce.Msg),
				Remedy: "set a valid value as a new version of the secret, then plan again"})
		}
	}
	return out
}

// bindingHosts are the hosts the destination rule judges of a spec's
// bindings: their upstreams' and their token endpoints', sorted.
func bindingHosts(sp spec.Spec) []string {
	var out []string
	for _, b := range sp.Bindings {
		if up, err := httpcheck.ParseUpstream("upstream", b.Upstream); err == nil {
			out = append(out, up.Host)
		}
		if c := b.Credential; c != nil && c.TokenURL != nil && c.TokenURL.Kind != spec.EnvSecret {
			if host, err := httpcheck.TokenURL("tokenUrl", c.TokenURL.Value); err == nil {
				out = append(out, host)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// sandboxHTTPNames are the names a sandbox's own HTTP bindings make its
// resolver answer.
func (s *Service) sandboxHTTPNames(ctx context.Context, id podid.ID) ([]string, error) {
	_, rows, err := s.st.HTTPBindings(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("the HTTP bindings of sandbox %s: %w", id, err)
	}
	var out []string
	for _, r := range rows {
		name := r.Name
		if r.Mode == httpcheck.ModeIntercept {
			if up, err := httpcheck.ParseUpstream("upstream", r.Upstream); err == nil {
				name = up.Host
			}
		}
		out = append(out, name)
	}
	return out, nil
}

// CheckBinding is POST /api/v1/environments/{env}/bindings/{name}/check:
// the preflight of the binding as the environment's latest deployment has
// it, its secrets at their latest versions (what the next deployment would
// pin), from this instance. 404 no_binding for a binding the latest
// deployment does not have, 404 bindings_off without HTTP bindings, 429
// too_many_preflights while the tenant has too many in flight.
func (s *Service) CheckBinding(ctx context.Context, tenant, envRef, name string, req BindingCheckRequest) (
	*bindings.HTTPPreflightResult, error) {
	e, err := resolveEnv(ctx, s.st, tenant, envRef, store.NoLock)
	if err != nil {
		return nil, err
	}
	if s.d.Bindings == nil || !s.d.Bindings.Enabled() {
		return nil, apierr.WithCode(http.StatusNotFound, CodeBindingsOff, "HTTP bindings are not enabled on this platform")
	}
	var b spec.Binding
	ok := false
	if e.Deployment > 0 {
		sp, err := s.deploymentSpec(ctx, e, e.Deployment)
		if err != nil {
			return nil, err
		}
		b, ok = sp.Bindings[name]
	}
	if !ok {
		return nil, apierr.WithCode(http.StatusNotFound, CodeNoBinding,
			"environment %s has no binding %s in its latest deployment", e.Name, name)
	}
	p, _, err := s.resolveBinding(ctx, e.ID, name, b, nil, true)
	if err != nil {
		var ce *httpcheck.Error
		if !errors.As(err, &ce) {
			ce = &httpcheck.Error{Code: CodeBindingNotChecked, Msg: err.Error()}
		}
		return &bindings.HTTPPreflightResult{Addresses: []string{},
			Error: &bindings.HTTPPreflightError{Code: ce.Code, Message: ce.Msg}}, nil
	}
	return s.d.Bindings.PreflightHTTP(ctx, tenant, p, req.Probe)
}

// resolveCredential is a binding that names a credential of the tenant
// (ADR 0033) as a pod receives it: the pinned version, opened when open,
// else its name, upstream and mode only. The definition is the binding,
// whose pin names the version.
func (s *Service) resolveCredential(ctx context.Context, envID uuid.UUID, name string, b spec.Binding, def bindingDef,
	open bool) (*fleetv1.HttpBinding, bindingDef, error) {
	ref := b.TenantCredential
	up, mode := spec.BindingTemplate(b)
	p := &fleetv1.HttpBinding{Name: name, Upstream: up}
	if mode == httpcheck.ModeIntercept {
		p.Mode = fleetv1.HttpBindingMode_HTTP_BINDING_MODE_INTERCEPT
	}
	if !open {
		return p, def, nil
	}
	if s.d.Bindings == nil {
		return p, def, &httpcheck.Error{Code: httpcheck.CodeInvalidBinding, Msg: "HTTP bindings are off"}
	}
	e, err := s.st.GetEnvironment(ctx, envID)
	if err != nil {
		return nil, def, fmt.Errorf("environment %s: %w", envID, err)
	}
	opened, err := s.d.Bindings.OpenCredential(ctx, e.Owner, ref.Name, ref.Version, name)
	if errors.Is(err, model.ErrNotFound) {
		return p, def, &httpcheck.Error{Code: httpcheck.CodeInvalidBinding,
			Msg: fmt.Sprintf("credential %s has no version %d any more", ref.Name, ref.Version)}
	}
	if err != nil {
		return nil, def, fmt.Errorf("binding %s's credential %s: %w", name, ref.Name, err)
	}
	return opened, def, nil
}

// CredentialUses is bindings.CredentialUsers: the environments of a
// tenant whose latest or deployed deployment names a credential, by the
// credential's name, with the bindings that name it and the versions
// each of the two pins (ADR 0033).
func (s *Service) CredentialUses(ctx context.Context, tenant string) (map[string][]bindings.EnvCredentialUse, error) {
	envs, err := s.st.ListEnvironments(ctx, tenant, nil)
	if err != nil {
		return nil, fmt.Errorf("environments: %w", err)
	}
	out := map[string][]bindings.EnvCredentialUse{}
	for _, e := range envs {
		uses := map[string]*bindings.EnvCredentialUse{}
		read := func(number int, latest bool) error {
			if number == 0 {
				return nil
			}
			sp, err := s.deploymentSpec(ctx, e, number)
			if err != nil {
				return err
			}
			for _, bname := range slices.Sorted(maps.Keys(sp.Bindings)) {
				ref := sp.Bindings[bname].TenantCredential
				if ref == nil {
					continue
				}
				u := uses[ref.Name]
				if u == nil {
					u = &bindings.EnvCredentialUse{ID: e.ID.String(), Name: e.Name, Bindings: []string{}}
					uses[ref.Name] = u
				}
				if !slices.Contains(u.Bindings, bname) {
					u.Bindings = append(u.Bindings, bname)
				}
				if latest {
					u.Latest = max(u.Latest, ref.Version)
				} else {
					u.Deployed = max(u.Deployed, ref.Version)
				}
			}
			return nil
		}
		if err := read(e.Deployment, true); err != nil {
			return nil, err
		}
		if e.Deployed != e.Deployment {
			if err := read(e.Deployed, false); err != nil {
				return nil, err
			}
		} else {
			for _, u := range uses {
				u.Deployed = u.Latest
			}
		}
		for _, name := range slices.Sorted(maps.Keys(uses)) {
			out[name] = append(out[name], *uses[name])
		}
	}
	return out, nil
}

var _ bindings.CredentialUsers = (*Service)(nil)
