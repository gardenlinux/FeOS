// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/bindings/httpcheck"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The HTTP surface (ADR 0028):
//
//	PUT    /api/v1/sandboxes/{id}/http   sandbox  the complete binding set
//	GET    /api/v1/sandboxes/{id}/http   sandbox  the set, every secret field {"set": true}
//	DELETE /api/v1/sandboxes/{id}/http   sandbox  no bindings
//	POST   /api/v1/http/preflight        tenant   one binding, no sandbox
//
// A PUT states the whole set. It is stored with its credentials sealed,
// a new interception CA is made when the set's intercepted names change,
// and the pod, when it is on a node, is sent the set's clear part
// (SetPodHttpBindings) after the commit, so that a running pod's resolver
// and trust files follow and its feos-authn fetches the set again.

// The codes of the HTTP surface beyond the shared ones.
const (
	codeInsecureUpstream = httpcheck.CodeInsecureUpstream
	codeBadHeader        = httpcheck.CodeBadHeader
	codeNameTaken        = "name_taken"
	codeTokenFailed      = "token_failed"
)

// ------------------------------------------------------------ wire types --

type httpRateLimitIn struct {
	PerSecond int `json:"perSecond"`
	Burst     int `json:"burst"`
}

// httpCredentialIn is a credential as a PUT and a preflight send it: kind
// and the fields of its kind.
type httpCredentialIn struct {
	Kind             string            `json:"kind"`
	Headers          map[string]string `json:"headers"`
	User             string            `json:"user"`
	Password         string            `json:"password"`
	TokenURL         string            `json:"tokenUrl"`
	ClientID         string            `json:"clientId"`
	ClientSecret     string            `json:"clientSecret"`
	Scope            string            `json:"scope"`
	Audience         string            `json:"audience"`
	ClientAuthInBody bool              `json:"clientAuthInBody"`
	CertPem          string            `json:"certPem"`
	KeyPem           string            `json:"keyPem"`
}

type httpBindingIn struct {
	Name          string            `json:"name"`
	Upstream      string            `json:"upstream"`
	Mode          string            `json:"mode"`
	CaPem         string            `json:"caPem"`
	PathPrefixes  []string          `json:"pathPrefixes"`
	ForwardedHost bool              `json:"forwardedHost"`
	RateLimit     *httpRateLimitIn  `json:"rateLimit"`
	Credential    *httpCredentialIn `json:"credential"`
}

// secretSet stands for a secret field's value in an answer.
type secretSet struct {
	Set bool `json:"set"`
}

type httpCredentialOut struct {
	Kind             string               `json:"kind"`
	Headers          map[string]secretSet `json:"headers,omitempty"`
	User             string               `json:"user,omitempty"`
	Password         *secretSet           `json:"password,omitempty"`
	TokenURL         string               `json:"tokenUrl,omitempty"`
	ClientID         string               `json:"clientId,omitempty"`
	ClientSecret     *secretSet           `json:"clientSecret,omitempty"`
	Scope            string               `json:"scope,omitempty"`
	Audience         string               `json:"audience,omitempty"`
	ClientAuthInBody bool                 `json:"clientAuthInBody,omitempty"`
	CertPem          string               `json:"certPem,omitempty"`
	KeyPem           *secretSet           `json:"keyPem,omitempty"`
}

type httpRateLimitOut struct {
	PerSecond int `json:"perSecond"`
	Burst     int `json:"burst"`
}

type httpBindingOut struct {
	Name          string            `json:"name"`
	Upstream      string            `json:"upstream"`
	Mode          string            `json:"mode"`
	CaPem         string            `json:"caPem"`
	PathPrefixes  []string          `json:"pathPrefixes"`
	ForwardedHost bool              `json:"forwardedHost"`
	RateLimit     *httpRateLimitOut `json:"rateLimit"`
	Credential    httpCredentialOut `json:"credential"`
}

type httpSetOut struct {
	Version  string           `json:"version"`
	Bindings []httpBindingOut `json:"bindings"`
}

// ------------------------------------------------------- internal shape --

// httpCredential is a binding's credential; exactly the fields of its
// Kind are set. Secret material: never logged, never rendered. Its JSON is
// what the sealed column holds.
type httpCredential struct {
	Kind             string            `json:"kind"`
	Headers          map[string]string `json:"headers,omitempty"`
	User             string            `json:"user,omitempty"`
	Password         string            `json:"password,omitempty"`
	TokenURL         string            `json:"tokenUrl,omitempty"`
	ClientID         string            `json:"clientId,omitempty"`
	ClientSecret     string            `json:"clientSecret,omitempty"`
	Scope            string            `json:"scope,omitempty"`
	Audience         string            `json:"audience,omitempty"`
	ClientAuthInBody bool              `json:"clientAuthInBody,omitempty"`
	CertPEM          string            `json:"certPem,omitempty"`
	KeyPEM           string            `json:"keyPem,omitempty"`
}

// httpBinding is one HTTP API a pod calls by a name, validated: its
// upstream canonical, its mode explicit, its rate limit normalised.
type httpBinding struct {
	Name          string
	Upstream      string
	Host          string
	Mode          string
	CAPEM         string
	PathPrefixes  []string
	ForwardedHost bool
	RatePerSecond int
	RateBurst     int
	Credential    httpCredential
}

// resolvedName is the name the pod's resolver answers for the binding:
// its label in the name mode, the upstream's host in the intercept mode.
func (b httpBinding) resolvedName() string {
	if b.Mode == httpcheck.ModeIntercept {
		return b.Host
	}
	return b.Name
}

func (b httpBinding) proto() *fleetv1.HttpBinding {
	p := &fleetv1.HttpBinding{Name: b.Name, Upstream: b.Upstream, CaPem: b.CAPEM,
		PathPrefixes: slices.Clone(b.PathPrefixes), ForwardedHost: b.ForwardedHost}
	if b.Mode == httpcheck.ModeIntercept {
		p.Mode = fleetv1.HttpBindingMode_HTTP_BINDING_MODE_INTERCEPT
	}
	if b.RatePerSecond > 0 {
		p.RateLimit = &fleetv1.RateLimit{PerSecond: uint32(b.RatePerSecond), Burst: uint32(b.RateBurst)}
	}
	c := b.Credential
	switch c.Kind {
	case httpcheck.KindHeaders:
		p.Credential = &fleetv1.HttpBinding_Headers{Headers: &fleetv1.HeaderCredential{Headers: maps.Clone(c.Headers)}}
	case httpcheck.KindBasic:
		p.Credential = &fleetv1.HttpBinding_Basic{Basic: &fleetv1.BasicCredential{User: c.User, Password: c.Password}}
	case httpcheck.KindOAuth2:
		p.Credential = &fleetv1.HttpBinding_Oauth2{Oauth2: &fleetv1.OAuthClientCredentials{TokenUrl: c.TokenURL,
			ClientId: c.ClientID, ClientSecret: c.ClientSecret, Scope: c.Scope, Audience: c.Audience,
			ClientAuthInBody: c.ClientAuthInBody}}
	case httpcheck.KindClientCertificate:
		p.Credential = &fleetv1.HttpBinding_ClientCertificate{ClientCertificate: &fleetv1.ClientCertificate{
			CertPem: c.CertPEM, KeyPem: c.KeyPEM}}
	}
	return p
}

// httpBindingOf is the internal shape of a binding as a pod receives it
// (an environment's, resolved); its credential as the proto has it.
func httpBindingOf(p *fleetv1.HttpBinding) httpBinding {
	b := httpBinding{Name: p.GetName(), Upstream: p.GetUpstream(), CAPEM: p.GetCaPem(),
		PathPrefixes: slices.Clone(p.GetPathPrefixes()), ForwardedHost: p.GetForwardedHost(), Mode: httpcheck.ModeName,
		RatePerSecond: int(p.GetRateLimit().GetPerSecond()), RateBurst: int(p.GetRateLimit().GetBurst())}
	if p.GetMode() == fleetv1.HttpBindingMode_HTTP_BINDING_MODE_INTERCEPT {
		b.Mode = httpcheck.ModeIntercept
	}
	if up, err := httpcheck.ParseUpstream("upstream", b.Upstream); err == nil {
		b.Host = up.Host
	}
	switch c := p.GetCredential().(type) {
	case *fleetv1.HttpBinding_Headers:
		b.Credential = httpCredential{Kind: httpcheck.KindHeaders, Headers: maps.Clone(c.Headers.GetHeaders())}
	case *fleetv1.HttpBinding_Basic:
		b.Credential = httpCredential{Kind: httpcheck.KindBasic, User: c.Basic.GetUser(), Password: c.Basic.GetPassword()}
	case *fleetv1.HttpBinding_Oauth2:
		o := c.Oauth2
		b.Credential = httpCredential{Kind: httpcheck.KindOAuth2, TokenURL: o.GetTokenUrl(), ClientID: o.GetClientId(),
			ClientSecret: o.GetClientSecret(), Scope: o.GetScope(), Audience: o.GetAudience(),
			ClientAuthInBody: o.GetClientAuthInBody()}
	case *fleetv1.HttpBinding_ClientCertificate:
		b.Credential = httpCredential{Kind: httpcheck.KindClientCertificate, CertPEM: c.ClientCertificate.GetCertPem(),
			KeyPEM: c.ClientCertificate.GetKeyPem()}
	}
	return b
}

// ------------------------------------------------------------ validation --

// ownerOf is the tenant of the sandbox a sandbox-class request was made
// as.
func ownerOf(r *http.Request) string {
	sb, _ := auth.SandboxFrom(r.Context())
	return sb.Owner
}

// asBindingErr turns a check's failure into the surface's.
func asBindingErr(e *httpcheck.Error) *bindingErr {
	if e == nil {
		return nil
	}
	return &bindingErr{code: e.Code, msg: e.Msg}
}

// validateHTTPBinding checks one binding without touching the network and
// returns it in its internal shape with the hosts the destination rule
// judges (the upstream's, and an OAuth token endpoint's). named: a PUT,
// where the name is part of the binding; a preflight takes any name.
func validateHTTPBinding(in httpBindingIn, named bool) (httpBinding, []string, *bindingErr) {
	var b httpBinding
	if named || in.Name != "" {
		if err := httpcheck.Name(in.Name); err != nil {
			return b, nil, asBindingErr(err)
		}
	}
	b.Name = in.Name
	up, cerr := httpcheck.ParseUpstream("upstream", in.Upstream)
	if cerr != nil {
		return b, nil, asBindingErr(cerr)
	}
	b.Upstream, b.Host = up.Origin(), up.Host
	if b.Mode, cerr = httpcheck.Mode("mode", in.Mode, up); cerr != nil {
		return b, nil, asBindingErr(cerr)
	}
	if in.CaPem != "" {
		if cerr := httpcheck.CAPEM("caPem", in.CaPem); cerr != nil {
			return b, nil, asBindingErr(cerr)
		}
	}
	b.CAPEM = in.CaPem
	if cerr := httpcheck.PathPrefixes("pathPrefixes", in.PathPrefixes); cerr != nil {
		return b, nil, asBindingErr(cerr)
	}
	b.PathPrefixes = slices.Clone(in.PathPrefixes)
	if b.PathPrefixes == nil {
		b.PathPrefixes = []string{}
	}
	b.ForwardedHost = in.ForwardedHost
	if in.RateLimit != nil {
		if b.RatePerSecond, b.RateBurst, cerr = httpcheck.RateLimit("rateLimit", in.RateLimit.PerSecond,
			in.RateLimit.Burst); cerr != nil {
			return b, nil, asBindingErr(cerr)
		}
	}
	hosts := []string{up.Host}
	if in.Credential == nil {
		return b, nil, bindingErrf(codeInvalidBinding, "credential is required")
	}
	c, tokenHost, berr := validateHTTPCredential(*in.Credential)
	if berr != nil {
		return b, nil, berr
	}
	b.Credential = c
	if tokenHost != "" && tokenHost != up.Host {
		hosts = append(hosts, tokenHost)
	}
	return b, hosts, nil
}

// validateHTTPCredential checks a credential: the fields of its kind and
// no other. It returns the token endpoint's host of an OAuth credential.
func validateHTTPCredential(in httpCredentialIn) (httpCredential, string, *bindingErr) {
	c := httpCredential{Kind: in.Kind}
	others := func(allowed ...string) *bindingErr {
		set := map[string]bool{
			"headers": len(in.Headers) > 0, "user": in.User != "", "password": in.Password != "",
			"tokenUrl": in.TokenURL != "", "clientId": in.ClientID != "", "clientSecret": in.ClientSecret != "",
			"scope": in.Scope != "", "audience": in.Audience != "", "clientAuthInBody": in.ClientAuthInBody,
			"certPem": in.CertPem != "", "keyPem": in.KeyPem != "",
		}
		for _, k := range allowed {
			delete(set, k)
		}
		for _, k := range slices.Sorted(maps.Keys(set)) {
			if set[k] {
				return bindingErrf(codeInvalidBinding, "credential kind %s takes no %s", in.Kind, k)
			}
		}
		return nil
	}
	switch in.Kind {
	case httpcheck.KindHeaders:
		if berr := others("headers"); berr != nil {
			return c, "", berr
		}
		if len(in.Headers) == 0 {
			return c, "", bindingErrf(codeInvalidBinding, "credential.headers needs at least one header")
		}
		if len(in.Headers) > httpcheck.MaxHeaders {
			return c, "", bindingErrf(codeBadHeader, "credential.headers: at most %d headers", httpcheck.MaxHeaders)
		}
		c.Headers = map[string]string{}
		seen := map[string]string{}
		for _, name := range slices.Sorted(maps.Keys(in.Headers)) {
			f := "credential.headers." + name
			if err := httpcheck.HeaderName(f, name); err != nil {
				return c, "", asBindingErr(err)
			}
			if err := httpcheck.HeaderValue(f, in.Headers[name]); err != nil {
				return c, "", asBindingErr(err)
			}
			canon := http.CanonicalHeaderKey(name)
			if prev, ok := seen[canon]; ok {
				return c, "", bindingErrf(codeBadHeader, "credential.headers: %s and %s are one header", prev, name)
			}
			seen[canon] = name
			c.Headers[canon] = in.Headers[name]
		}
	case httpcheck.KindBasic:
		if berr := others("user", "password"); berr != nil {
			return c, "", berr
		}
		if err := httpcheck.BasicUser("credential.user", in.User); err != nil {
			return c, "", asBindingErr(err)
		}
		if err := httpcheck.Text("credential.password", in.Password, false); err != nil {
			return c, "", asBindingErr(err)
		}
		c.User, c.Password = in.User, in.Password
	case httpcheck.KindOAuth2:
		if berr := others("tokenUrl", "clientId", "clientSecret", "scope", "audience", "clientAuthInBody"); berr != nil {
			return c, "", berr
		}
		host, err := httpcheck.TokenURL("credential.tokenUrl", in.TokenURL)
		if err != nil {
			return c, "", asBindingErr(err)
		}
		for _, f := range []struct {
			name, v string
			empty   bool
		}{{"clientId", in.ClientID, false}, {"clientSecret", in.ClientSecret, false},
			{"scope", in.Scope, true}, {"audience", in.Audience, true}} {
			if err := httpcheck.Text("credential."+f.name, f.v, f.empty); err != nil {
				return c, "", asBindingErr(err)
			}
		}
		c.TokenURL, c.ClientID, c.ClientSecret = in.TokenURL, in.ClientID, in.ClientSecret
		c.Scope, c.Audience, c.ClientAuthInBody = in.Scope, in.Audience, in.ClientAuthInBody
		return c, host, nil
	case httpcheck.KindClientCertificate:
		if berr := others("certPem", "keyPem"); berr != nil {
			return c, "", berr
		}
		if err := httpcheck.ClientCertificate("credential", in.CertPem, in.KeyPem); err != nil {
			return c, "", asBindingErr(err)
		}
		c.CertPEM, c.KeyPEM = in.CertPem, in.KeyPem
	case "":
		return c, "", bindingErrf(codeInvalidBinding,
			"credential.kind is required (headers, basic, oauth2 or clientCertificate)")
	default:
		return c, "", bindingErrf(codeInvalidBinding,
			"credential.kind %q is not supported (headers, basic, oauth2 or clientCertificate)", in.Kind)
	}
	return c, "", nil
}

// validateHTTPSet checks a PUT's whole set: every binding, the uniqueness
// of names and of the names the pod resolves, the names the sandbox's
// tables hold, the size, and only then, when everything local is fine, the
// destination rule, which resolves names. The bindings come back sorted
// by name.
func (s *Service) validateHTTPSet(ctx context.Context, id podid.ID, owner string, ins []httpBindingIn) (
	[]httpBinding, *bindingErr) {
	if len(ins) > httpcheck.MaxBindings {
		return nil, bindingErrf(codeInvalidBinding, "at most %d bindings per sandbox", httpcheck.MaxBindings)
	}
	out := make([]httpBinding, 0, len(ins))
	var hosts [][]string
	names, resolved := map[string]bool{}, map[string]string{}
	for i, in := range ins {
		b, hs, berr := validateHTTPBinding(in, true)
		if berr != nil {
			berr.msg = fmt.Sprintf("bindings[%d]: %s", i, berr.msg)
			return nil, berr
		}
		if names[b.Name] {
			return nil, bindingErrf(codeInvalidBinding, "bindings[%d]: duplicate name %q", i, b.Name)
		}
		names[b.Name] = true
		if other, ok := resolved[b.resolvedName()]; ok {
			return nil, bindingErrf(codeInvalidBinding, "bindings[%d] (%s): the pod would resolve %s for %s as well",
				i, b.Name, b.resolvedName(), other)
		}
		resolved[b.resolvedName()] = b.Name
		out = append(out, b)
		hosts = append(hosts, hs)
	}
	if raw, err := json.Marshal(out); err != nil || len(raw) > maxSetBytes {
		return nil, bindingErrf(codeInvalidBinding,
			"the set is too large (its CA bundles and certificates must fit in %d KiB)", maxSetBytes>>10)
	}
	if len(out) > 0 {
		taken, err := s.sandboxNames(ctx, id, owner)
		if err != nil {
			return nil, bindingErrf(codeUnreachable, "the names of the sandbox's environments: %v", err)
		}
		for i, b := range out {
			if env, ok := taken[b.resolvedName()]; ok {
				return nil, bindingErrf(codeNameTaken, "bindings[%d] (%s): environment %s gives the sandbox the name %s; "+
					"name the binding otherwise", i, b.Name, env, b.resolvedName())
			}
		}
	}
	checked := map[string]bool{}
	for i, hs := range hosts {
		for _, host := range hs {
			if checked[host] {
				continue
			}
			if berr := s.dest.checkDestination(ctx, host); berr != nil {
				berr.msg = fmt.Sprintf("bindings[%d] (%s): %s", i, out[i].Name, berr.msg)
				return nil, berr
			}
			checked[host] = true
		}
	}
	slices.SortFunc(out, func(a, b httpBinding) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// sandboxNames are the names the environments with attached services in
// the sandbox give it (its table includes theirs), each with the first
// environment that gives it.
func (s *Service) sandboxNames(ctx context.Context, id podid.ID, owner string) (map[string]string, error) {
	out := map[string]string{}
	if owner == "" {
		return out, nil
	}
	rows, err := s.st.AttachedSpecs(ctx, owner, uuid.Nil, []string{strings.ToLower(id.String())})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		var sp spec.Spec
		if err := json.Unmarshal(r.Spec, &sp); err != nil {
			continue
		}
		for _, n := range spec.SandboxNames(sp, strings.ToLower(id.String())) {
			if _, ok := out[n]; !ok {
				out[n] = r.EnvName
			}
		}
	}
	return out, nil
}

// --------------------------------------------------------------- at rest --

// newVersion is a set's version: random, a new one at every change.
func newVersion() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("a version: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (s *Service) sealHTTP(id podid.ID, b httpBinding) (store.HTTPBinding, error) {
	pt, err := json.Marshal(b.Credential)
	if err != nil {
		return store.HTTPBinding{}, err
	}
	sealed, err := s.sealer.Seal(pt, sealAAD(tableHTTP, id, b.Name, b.Credential.Kind))
	if err != nil {
		return store.HTTPBinding{}, fmt.Errorf("seal binding %s: %w", b.Name, err)
	}
	return store.HTTPBinding{Name: b.Name, Upstream: b.Upstream, Mode: b.Mode, CAPEM: b.CAPEM,
		PathPrefixes: b.PathPrefixes, ForwardedHost: b.ForwardedHost, RatePerSecond: b.RatePerSecond,
		RateBurst: b.RateBurst, Kind: b.Credential.Kind, Sealed: sealed}, nil
}

func (s *Service) openHTTPRow(id podid.ID, r store.HTTPBinding) (httpBinding, error) {
	pt, err := s.sealer.Open(r.Sealed, sealAAD(tableHTTP, id, r.Name, r.Kind))
	if err != nil {
		return httpBinding{}, err
	}
	var c httpCredential
	if err := json.Unmarshal(pt, &c); err != nil {
		return httpBinding{}, fmt.Errorf("sealed credential: %v", err)
	}
	if c.Kind != r.Kind {
		return httpBinding{}, fmt.Errorf("sealed credential of kind %s in a row of kind %s", c.Kind, r.Kind)
	}
	b := httpBinding{Name: r.Name, Upstream: r.Upstream, Mode: r.Mode, CAPEM: r.CAPEM,
		PathPrefixes: slices.Clone(r.PathPrefixes), ForwardedHost: r.ForwardedHost, RatePerSecond: r.RatePerSecond,
		RateBurst: r.RateBurst, Credential: c}
	if b.PathPrefixes == nil {
		b.PathPrefixes = []string{}
	}
	if up, cerr := httpcheck.ParseUpstream("upstream", r.Upstream); cerr == nil {
		b.Host = up.Host
	}
	return b, nil
}

// openHTTP opens a stored set, by name.
func (s *Service) openHTTP(id podid.ID, rows []store.HTTPBinding) ([]httpBinding, error) {
	bs := make([]httpBinding, 0, len(rows))
	for _, r := range rows {
		b, err := s.openHTTPRow(id, r)
		if err != nil {
			return nil, fmt.Errorf("http binding %s of %s: %w", r.Name, id, err)
		}
		bs = append(bs, b)
	}
	return bs, nil
}

// interceptHosts are the hosts a set intercepts, sorted, without
// duplicates.
func interceptHosts(bs []httpBinding) []string {
	var out []string
	for _, b := range bs {
		if b.Mode == httpcheck.ModeIntercept {
			out = append(out, b.Host)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// applyHTTP stores a validated set (nil: none) under the sandbox's row
// lock, makes a new interception CA when the set's intercepted names
// differ from the newest CA's, and returns the set as stored, read in the
// same transaction. The same set again is a no-op, unless what is stored
// no longer opens (a changed key): then the PUT is the repair. changed
// reports whether anything was written.
func (s *Service) applyHTTP(ctx context.Context, id podid.ID, bs []httpBinding) (httpSetOut, bool, error) {
	var res httpSetOut
	changed := false
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		changed = false
		if _, err := tx.LockSandbox(ctx, id, store.ForUpdate); err != nil {
			if errors.Is(err, model.ErrNotFound) {
				return noSuchSandbox()
			}
			return err
		}
		stored, rows, err := tx.HTTPBindings(ctx, id)
		if err != nil {
			return err
		}
		if old, err := s.openHTTP(id, rows); err == nil && sameHTTP(old, bs) {
			res = s.httpSetOf(id, stored, old)
			return nil
		}
		version := ""
		if len(bs) > 0 {
			if version, err = newVersion(); err != nil {
				return err
			}
		}
		out := make([]store.HTTPBinding, 0, len(bs))
		for _, b := range bs {
			row, err := s.sealHTTP(id, b)
			if err != nil {
				return err
			}
			out = append(out, row)
		}
		if err := tx.SetHTTPBindings(ctx, id, version, out); err != nil {
			return err
		}
		if hosts := interceptHosts(bs); len(hosts) > 0 {
			if _, err := s.ensureCAs(ctx, tx, id, hosts); err != nil {
				return err
			}
		}
		changed = true
		res = s.httpSetOf(id, version, bs)
		return nil
	})
	return res, changed, err
}

// sameHTTP reports whether two sets, both by name, are the same.
func sameHTTP(a, b []httpBinding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if len(x.Credential.Headers) == 0 {
			x.Credential.Headers = nil
		}
		if len(y.Credential.Headers) == 0 {
			y.Credential.Headers = nil
		}
		if !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

// -------------------------------------------------------------- handlers --

// httpSetOf renders a set: everything but the secret fields, each of
// which answers {"set": true}.
func (s *Service) httpSetOf(_ podid.ID, version string, bs []httpBinding) httpSetOut {
	out := httpSetOut{Version: version, Bindings: []httpBindingOut{}}
	for _, b := range bs {
		o := httpBindingOut{Name: b.Name, Upstream: b.Upstream, Mode: b.Mode, CaPem: b.CAPEM,
			PathPrefixes: slices.Clone(b.PathPrefixes), ForwardedHost: b.ForwardedHost}
		if o.PathPrefixes == nil {
			o.PathPrefixes = []string{}
		}
		if b.RatePerSecond > 0 {
			o.RateLimit = &httpRateLimitOut{PerSecond: b.RatePerSecond, Burst: b.RateBurst}
		}
		c := b.Credential
		o.Credential = httpCredentialOut{Kind: c.Kind, User: c.User, TokenURL: c.TokenURL, ClientID: c.ClientID,
			Scope: c.Scope, Audience: c.Audience, ClientAuthInBody: c.ClientAuthInBody, CertPem: c.CertPEM}
		set := &secretSet{Set: true}
		if len(c.Headers) > 0 {
			o.Credential.Headers = map[string]secretSet{}
			for k := range c.Headers {
				o.Credential.Headers[k] = secretSet{Set: true}
			}
		}
		if c.Password != "" {
			o.Credential.Password = set
		}
		if c.ClientSecret != "" {
			o.Credential.ClientSecret = set
		}
		if c.KeyPEM != "" {
			o.Credential.KeyPem = set
		}
		out.Bindings = append(out.Bindings, o)
	}
	return out
}

// httpSet reads and renders the stored set.
func (s *Service) httpSet(ctx context.Context, id podid.ID) (httpSetOut, error) {
	version, rows, err := s.st.HTTPBindings(ctx, id)
	if errors.Is(err, model.ErrNotFound) {
		return httpSetOut{}, noSuchSandbox()
	}
	if err != nil {
		return httpSetOut{}, fmt.Errorf("http bindings %s: %w", id, err)
	}
	bs, err := s.openHTTP(id, rows)
	if err != nil {
		return httpSetOut{}, fmt.Errorf("%w; PUT the set again", err)
	}
	return s.httpSetOf(id, version, bs), nil
}

// GetHTTP answers the sandbox's HTTP set, every secret field {"set":
// true}.
func (s *Service) GetHTTP(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	id, aerr := sandboxOf(r)
	if aerr != nil {
		apierr.Write(w, aerr)
		return
	}
	out, err := s.httpSet(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}

// PutHTTP replaces the sandbox's HTTP set, has its pod sent the set's
// clear part, and answers the set as GET does.
func (s *Service) PutHTTP(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	id, aerr := sandboxOf(r)
	if aerr != nil {
		apierr.Write(w, aerr)
		return
	}
	var body struct {
		Bindings []httpBindingIn `json:"bindings"`
	}
	sent, err := decodeJSON(r, &body)
	if err != nil {
		apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
		return
	}
	if _, ok := sent["bindings"]; !ok {
		apierr.Write(w, bindingErrf(codeInvalidBinding,
			"bindings is required: the complete set, [] for none").as(http.StatusBadRequest))
		return
	}
	bs, berr := s.validateHTTPSet(r.Context(), id, ownerOf(r), body.Bindings)
	if berr != nil {
		apierr.Write(w, berr.as(http.StatusBadRequest))
		return
	}
	out, changed, err := s.applyHTTP(r.Context(), id, bs)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if changed {
		s.push(id)
	}
	if s.afterApply != nil {
		s.afterApply()
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}

// DeleteHTTP removes the sandbox's HTTP set: 204.
func (s *Service) DeleteHTTP(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	id, aerr := sandboxOf(r)
	if aerr != nil {
		apierr.Write(w, aerr)
		return
	}
	_, changed, err := s.applyHTTP(r.Context(), id, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if changed {
		s.push(id)
	}
	w.WriteHeader(http.StatusNoContent)
}
