// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gardenlinux/feos/go/internal/bindings/httpcheck"
)

// An environment's bindings (ADR 0028): checked by the rules of a
// sandbox's HTTP set (internal/bindings/httpcheck), their credentials'
// secret fields (a header's value, password, clientSecret, keyPem)
// secrets of the environment, never literals, which would stand in the
// clear in the spec, its plans and its deployments, their other fields
// literals or secrets, their names in the
// space of the services' and addons' (the pods' name tables answer
// those), each named by the services and jobs that get it.

const bindingsRemedy = "see the HTTP bindings in ADR 0028"

// bindings checks the spec's bindings and that each is named.
func (v *validator) bindings() {
	if len(v.s.Bindings) == 0 {
		return
	}
	if on, ok := v.vc.(interface{ HTTPBindings() bool }); ok && !on.HTTPBindings() {
		v.errorf("bindings", "remove the bindings, or ask the platform's operators for --bindings",
			"HTTP bindings are not enabled on this platform")
	}
	resolved := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(v.s.Bindings)) {
		b := v.s.Bindings[name]
		up, mode := v.binding(name, b)
		if up.Host == "" {
			continue
		}
		r := name
		if mode == httpcheck.ModeIntercept {
			r = up.Host
		}
		if other, ok := resolved[r]; ok {
			v.errorf("bindings."+name, "intercept each host once", "bindings %s and %s both make the pods resolve %s",
				other, name, r)
			continue
		}
		resolved[r] = name
		if _, ok := v.s.Services[r]; ok && r != name {
			v.errorf("bindings."+name+".upstream", "rename the service", "the pods would resolve %s, a service's "+
				"name, to the binding", r)
		}
		if _, ok := v.s.Addons[r]; ok && r != name {
			v.errorf("bindings."+name+".upstream", "rename the addon", "the pods would resolve %s, an addon's "+
				"name, to the binding", r)
		}
	}
	for name := range v.s.Bindings {
		if _, ok := v.s.Services[name]; ok {
			v.errorf("bindings."+name, "rename one", "a binding and a service are both named %s: they share the "+
				"names the pods resolve", name)
		}
		if _, ok := v.s.Addons[name]; ok {
			v.errorf("bindings."+name, "rename one", "a binding and an addon are both named %s: they share the "+
				"names the pods resolve", name)
		}
	}
	named := map[string]bool{}
	for _, svc := range v.s.Services {
		for _, n := range svc.Bindings {
			named[n] = true
		}
	}
	for _, j := range v.s.Jobs {
		for _, n := range j.Bindings {
			named[n] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(v.s.Bindings)) {
		if !named[name] {
			v.warnf("bindings."+name, "name it in the bindings of the services and jobs that call it, or remove it",
				"no service or job names binding %s: no pod gets it", name)
		}
	}
}

// fail adds a check's failure at path.
func (v *validator) fail(path string, e *httpcheck.Error) {
	if e != nil {
		v.errorf(path, bindingsRemedy, "%s", e.Msg)
	}
}

// destination asks the destination rule of host, when the context can.
func (v *validator) destination(path, host string) {
	d, ok := v.vc.(interface{ Destination(host string) error })
	if !ok {
		return
	}
	if err := d.Destination(host); err != nil {
		v.errorf(path, "bind an API on the internet: the platform's own networks are no destination", "%v", err)
	}
}

// binding checks one binding and returns its upstream and mode (the zero
// upstream when it is none).
func (v *validator) binding(name string, b Binding) (httpcheck.Upstream, string) {
	p := "bindings." + name
	if b.TenantCredential != nil {
		return v.tenantCredential(p, b)
	}
	up, err := httpcheck.ParseUpstream("upstream", b.Upstream)
	if err != nil {
		v.fail(p+".upstream", err)
		return httpcheck.Upstream{}, ""
	}
	mode, err := httpcheck.Mode("mode", b.Mode, up)
	v.fail(p+".mode", err)
	v.destination(p+".upstream", up.Host)
	if b.CaPem != "" {
		v.fail(p+".caPem", httpcheck.CAPEM("caPem", b.CaPem))
	}
	v.fail(p+".pathPrefixes", httpcheck.PathPrefixes("pathPrefixes", b.PathPrefixes))
	if b.RateLimit != nil {
		_, _, err := httpcheck.RateLimit("rateLimit", b.RateLimit.PerSecond, b.RateLimit.Burst)
		v.fail(p+".rateLimit", err)
	}
	if b.ForwardedHost && mode == httpcheck.ModeIntercept {
		v.warnf(p+".forwardedHost", "remove forwardedHost", "forwardedHost has no effect in the intercept mode: the "+
			"upstream sees its own name")
	}
	v.credential(p+".credential", b.Credential)
	return up, mode
}

// tenantCredential checks a binding that names a credential of the
// tenant (ADR 0033): nothing of its own beyond the upstream and mode a
// normalised spec carries, which must be the version's; a credential the
// tenant has, at a version it has. It returns the version's upstream and
// mode.
func (v *validator) tenantCredential(p string, b Binding) (httpcheck.Upstream, string) {
	ref := b.TenantCredential
	cp := p + ".tenantCredential"
	own := map[string]bool{"credential": b.Credential != nil, "caPem": b.CaPem != "",
		"pathPrefixes": len(b.PathPrefixes) > 0, "forwardedHost": b.ForwardedHost, "rateLimit": b.RateLimit != nil}
	for _, f := range slices.Sorted(maps.Keys(own)) {
		if own[f] {
			v.errorf(p+"."+f, "remove it, or state the binding without tenantCredential",
				"a binding that names a tenant credential takes its %s from the credential", f)
		}
	}
	if !IsName(ref.Name) {
		v.errorf(cp+".name", "name a credential of the tenant", "%q is no credential's name", ref.Name)
		return httpcheck.Upstream{}, ""
	}
	cc, ok := v.vc.(CredentialContext)
	if !ok {
		v.errorf(cp, "state the binding's upstream and credential", "the tenant's credentials are not available here")
		return httpcheck.Upstream{}, ""
	}
	info, ok := cc.TenantCredential(ref.Name)
	if !ok {
		v.errorf(cp+".name", "make the credential under the tenant's credentials first",
			"the tenant has no credential %s", ref.Name)
		return httpcheck.Upstream{}, ""
	}
	version := ref.Version
	if version == 0 {
		version = info.Latest
	}
	t, ok := info.Versions[version]
	if !ok {
		v.errorf(cp+".version", fmt.Sprintf("use a version it has, the latest is %d", info.Latest),
			"credential %s has no version %d", ref.Name, version)
		return httpcheck.Upstream{}, ""
	}
	up, err := httpcheck.ParseUpstream("upstream", t.Upstream)
	if err != nil {
		v.fail(cp, err)
		return httpcheck.Upstream{}, ""
	}
	if b.Upstream != "" || b.Mode != "" {
		if wu, wm := BindingTemplate(b); wu != up.Origin() || wm != t.Mode {
			v.errorf(p+".upstream", "remove upstream and mode: they are the credential's",
				"credential %s version %d calls %s in the %s mode", ref.Name, version, up.Origin(), t.Mode)
		}
	}
	v.destination(cp, up.Host)
	return up, t.Mode
}

// credential checks a binding's credential: its kind's fields, and no
// other, each a literal or a secret, the secret fields a secret.
func (v *validator) credential(p string, c *BindingCredential) {
	if c == nil {
		v.errorf(p, `set the credential: {"kind": "headers" | "basic" | "oauth2" | "clientCertificate", …}`,
			"a binding needs a credential")
		return
	}
	var want []string
	switch c.Kind {
	case httpcheck.KindHeaders:
		want = []string{"headers"}
	case httpcheck.KindBasic:
		want = []string{"user", "password"}
	case httpcheck.KindOAuth2:
		want = []string{"tokenUrl", "clientId", "clientSecret", "scope", "audience", "clientAuthInBody"}
	case httpcheck.KindClientCertificate:
		want = []string{"certPem", "keyPem"}
	case "":
		v.errorf(p+".kind", "use headers, basic, oauth2 or clientCertificate", "a credential needs a kind")
		return
	default:
		v.errorf(p+".kind", "use headers, basic, oauth2 or clientCertificate", "kind %q is unknown", c.Kind)
		return
	}
	set := map[string]bool{"headers": len(c.Headers) > 0, "user": c.User != nil, "password": c.Password != nil,
		"tokenUrl": c.TokenURL != nil, "clientId": c.ClientID != nil, "clientSecret": c.ClientSecret != nil,
		"scope": c.Scope != nil, "audience": c.Audience != nil, "clientAuthInBody": c.ClientAuthInBody,
		"certPem": c.CertPem != nil, "keyPem": c.KeyPem != nil}
	for _, f := range slices.Sorted(maps.Keys(set)) {
		if set[f] && !slices.Contains(want, f) {
			v.errorf(p+"."+f, "remove it", "a credential of kind %s has no %s", c.Kind, f)
		}
	}
	// value checks one value field: required or not; literal, what the
	// literal must pass (nil: anything but control characters).
	value := func(field string, val *EnvValue, required bool, literal func(field, s string) *httpcheck.Error) {
		fp := p + "." + field
		if val == nil {
			if required {
				v.errorf(fp, `set it to a string, {"value": "…"} or {"secret": "<name>"}`, "a credential of kind %s "+
					"needs %s", c.Kind, field)
			}
			return
		}
		switch val.Kind {
		case EnvSecret:
			v.secretRef(fp, val.Secret)
		case EnvString, EnvLiteral:
			if literal == nil {
				literal = func(field, s string) *httpcheck.Error { return httpcheck.Text(field, s, !required) }
			}
			v.fail(fp, literal(field, val.Value))
		default:
			v.errorf(fp, `use a string, {"value": "…"} or {"secret": "<name>"}`, "a credential's %s is a literal or "+
				"a secret", field)
		}
	}
	// secret checks a secret field: required, a secret of the environment.
	secret := func(field string, val *EnvValue) {
		fp := p + "." + field
		switch {
		case val == nil:
			v.errorf(fp, `set it to {"secret": "<name>"}`, "a credential of kind %s needs %s", c.Kind, field)
		case val.Kind == EnvSecret:
			v.secretRef(fp, val.Secret)
		default:
			v.errorf(fp, `store the value as a secret of the environment and write {"secret": "<name>"}`,
				"a credential's %s is a secret's: a literal would stand in the clear in the spec, its plans and its "+
					"deployments", field)
		}
	}
	switch c.Kind {
	case httpcheck.KindHeaders:
		if len(c.Headers) == 0 {
			v.errorf(p+".headers", "name at least one header", "a credential of kind headers needs headers")
		}
		if len(c.Headers) > httpcheck.MaxHeaders {
			v.errorf(p+".headers", "", "at most %d headers", httpcheck.MaxHeaders)
		}
		canon := map[string]string{}
		for _, h := range slices.Sorted(maps.Keys(c.Headers)) {
			v.fail(p+".headers."+h, httpcheck.HeaderName("header", h))
			if prev, ok := canon[strings.ToLower(h)]; ok {
				v.errorf(p+".headers."+h, "name each header once", "%s and %s are one header", prev, h)
			}
			canon[strings.ToLower(h)] = h
			val := c.Headers[h]
			secret("headers."+h, &val)
		}
	case httpcheck.KindBasic:
		value("user", c.User, true, httpcheck.BasicUser)
		secret("password", c.Password)
	case httpcheck.KindOAuth2:
		fp := p + ".tokenUrl"
		switch {
		case c.TokenURL == nil:
			v.errorf(fp, "set the token endpoint's https URL", "a credential of kind oauth2 needs tokenUrl")
		case c.TokenURL.Kind != EnvString && c.TokenURL.Kind != EnvLiteral:
			v.errorf(fp, "write the token endpoint's URL as it is", "tokenUrl is a literal: the destination rule "+
				"judges its host")
		default:
			host, err := httpcheck.TokenURL("tokenUrl", c.TokenURL.Value)
			v.fail(fp, err)
			if err == nil {
				v.destination(fp, host)
			}
		}
		value("clientId", c.ClientID, true, nil)
		secret("clientSecret", c.ClientSecret)
		value("scope", c.Scope, false, nil)
		value("audience", c.Audience, false, nil)
	case httpcheck.KindClientCertificate:
		// The pair is checked once the key's secret resolves: a plan's
		// warning, a pod's binding_invalid.
		value("certPem", c.CertPem, true, func(string, string) *httpcheck.Error { return nil })
		secret("keyPem", c.KeyPem)
	}
}

// bindingRefs checks a service's or job's bindings: each names a binding
// of the environment, once.
func (v *validator) bindingRefs(path string, names []string) {
	seen := map[string]bool{}
	for i, n := range names {
		ip := index(path, i)
		switch {
		case seen[n]:
			v.errorf(ip, "name each binding once", "binding %s is named twice", n)
		case v.s.Bindings == nil || !hasBinding(v.s.Bindings, n):
			v.errorf(ip, "declare the binding under bindings, or name one that is declared", "no binding %s", n)
		}
		seen[n] = true
	}
}

func hasBinding(bs map[string]Binding, n string) bool {
	_, ok := bs[n]
	return ok
}

// BindingTemplate is what of a binding makes a pod another: its name, its
// upstream and its mode, canonical ("" when the upstream is none).
func BindingTemplate(b Binding) (upstream, mode string) {
	up, err := httpcheck.ParseUpstream("upstream", b.Upstream)
	if err != nil {
		return b.Upstream, b.Mode
	}
	m, err := httpcheck.Mode("mode", b.Mode, up)
	if err != nil {
		m = b.Mode
	}
	return up.Origin(), m
}
