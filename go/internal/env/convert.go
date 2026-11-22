// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/bindings"
	"github.com/gardenlinux/feos/go/internal/env/envdeps"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The one-off conversion of ADR 0055's second step (`ferund
// convert-bindings`): every binding stated in an environment's latest
// deployment, inline (not one that names a credential, which is an HTTP
// secret already), becomes an HTTP secret of that environment of the
// binding's name. Its upstream, mode, CA, prefixes, forwarded host and
// rate limit are the binding's, its credential the binding's with every
// field that names a secret resolved at the version that deployment pins
// (a literal stays a literal), so that nobody enters a key again. A name a
// secret of either kind holds already is left as it is, and said. It
// writes no deployment: the next deployment of each environment
// references the HTTP secrets ({secret, key: url}), and until then its
// pods keep their bindings as they are.

// HTTPSecretWriter writes an HTTP secret (*bindings.Service), with every
// rule of a write: the definition's checks, the destination rule, the
// versions and one name space of both kinds.
type HTTPSecretWriter interface {
	PutHTTPSecret(ctx context.Context, env uuid.UUID, name string, raw []byte, by string) (bindings.HTTPSecret, bool,
		error)
}

// SecretOpener opens a sealed version of a value secret (*seal.Sealer).
type SecretOpener interface {
	Open(sealed, aad []byte) ([]byte, error)
}

// The outcomes of a binding's conversion.
const (
	ConvertWould   = "would make"
	ConvertMade    = "made"
	ConvertSame    = "unchanged"
	ConvertSkipped = "skipped"
	ConvertFailed  = "failed"
)

// Conversion is one binding's conversion: where, what, and its outcome
// (Convert…), with Version the HTTP secret's version made or found and
// Why the reason of a skip or a failure. It carries no value.
type Conversion struct {
	EnvID        uuid.UUID
	Project, Env string
	Deployment   int
	Binding      string
	Upstream     string
	Mode         string
	Kind         string
	Outcome      string
	Version      int
	Why          string
}

// String is the conversion as a line of the command's output, never a
// value.
func (c Conversion) String() string {
	s := fmt.Sprintf("environment %s/%s (%s) deployment %d: binding %s, upstream %s, mode %s, credential %s: %s",
		c.Project, c.Env, c.EnvID, c.Deployment, c.Binding, c.Upstream, c.Mode, c.Kind, c.Outcome)
	if c.Version > 0 {
		s += fmt.Sprintf(" HTTP secret %s version %d", c.Binding, c.Version)
	} else if c.Outcome == ConvertWould {
		s += " HTTP secret " + c.Binding
	}
	if c.Why != "" {
		s += ": " + c.Why
	}
	return s
}

// ConvertBindings converts the bindings of every environment's latest
// deployment that are stated in its spec (see above), by: with apply it
// writes them through w, else it says what it would make. A binding that
// cannot be converted is a failed conversion beside the others; the
// error is the database's.
func ConvertBindings(ctx context.Context, st *store.Store, open SecretOpener, w HTTPSecretWriter, apply bool,
	by string) ([]Conversion, error) {
	envs, err := st.BindingEnvs(ctx)
	if err != nil {
		return nil, fmt.Errorf("the environments with bindings: %w", err)
	}
	var out []Conversion
	for _, be := range envs {
		e, err := st.GetEnvironment(ctx, be.ID)
		if errors.Is(err, store.ErrNoRow) {
			continue
		}
		if err != nil {
			return out, fmt.Errorf("environment %s: %w", be.ID, err)
		}
		if e.Deleting || e.Deployment == 0 {
			continue
		}
		r, err := st.GetDeployment(ctx, e.ID, e.Deployment)
		if err != nil {
			return out, fmt.Errorf("environment %s deployment %d: %w", e.ID, e.Deployment, err)
		}
		sp, err := parseStored(r.Spec)
		if err != nil {
			return out, fmt.Errorf("environment %s deployment %d: %w", e.ID, e.Deployment, err)
		}
		for _, name := range slices.Sorted(maps.Keys(sp.Bindings)) {
			b := sp.Bindings[name]
			up, mode := spec.BindingTemplate(b)
			c := Conversion{EnvID: e.ID, Project: e.Owner, Env: e.Name, Deployment: e.Deployment, Binding: name,
				Upstream: up, Mode: mode}
			if b.Credential != nil {
				c.Kind = b.Credential.Kind
			}
			c.Outcome, c.Version, c.Why, err = convertBinding(ctx, st, open, w, e.ID, name, b, sp.Secrets, apply, by)
			if err != nil {
				return out, err
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// convertBinding converts binding name of environment env, its secrets
// pinned by pins: the outcome, the version, why (a skip's or a failure's
// reason) and the database's error.
func convertBinding(ctx context.Context, st *store.Store, open SecretOpener, w HTTPSecretWriter, env uuid.UUID,
	name string, b spec.Binding, pins map[string]spec.SecretRef, apply bool, by string) (string, int, string, error) {
	if ref := b.CredentialRef; ref != nil {
		return ConvertSkipped, 0, fmt.Sprintf("it names credential %s, an HTTP secret already: reference that by a "+
			"variable", ref.Name), nil
	}
	isValue, err := st.ValueSecretExists(ctx, env, name)
	if err != nil {
		return "", 0, "", err
	}
	if isValue {
		return ConvertSkipped, 0, "a value secret of the name exists", nil
	}
	if isHTTP, err := st.EnvCredentialExists(ctx, env, name); err != nil {
		return "", 0, "", err
	} else if isHTTP {
		return ConvertSkipped, 0, "an HTTP secret of the name exists", nil
	}
	raw, why, err := definitionOf(ctx, st, open, env, b, pins)
	if err != nil || why != "" {
		return ConvertFailed, 0, why, err
	}
	if !apply {
		return ConvertWould, 0, "", nil
	}
	h, created, err := w.PutHTTPSecret(ctx, env, name, raw, by)
	if err != nil {
		return ConvertFailed, 0, err.Error(), nil
	}
	if !created {
		return ConvertSame, h.Version, "", nil
	}
	return ConvertMade, h.Version, "", nil
}

// definitionOf is binding b as an HTTP secret's definition, as a PUT
// sends it, every field that names a secret resolved at the version pins
// has: the definition, or why it cannot be had, or the database's error.
func definitionOf(ctx context.Context, st *store.Store, open SecretOpener, env uuid.UUID, b spec.Binding,
	pins map[string]spec.SecretRef) ([]byte, string, error) {
	c := b.Credential
	if c == nil {
		return nil, "the binding has no credential", nil
	}
	why := ""
	var dbErr error
	val := func(field string, v *spec.EnvValue) string {
		if v == nil || why != "" || dbErr != nil {
			return ""
		}
		if v.Kind != spec.EnvSecret {
			return v.Value
		}
		version := pins[v.Secret].Version
		if version <= 0 {
			why = fmt.Sprintf("the deployment pins no version of secret %s, which its %s names", v.Secret, field)
			return ""
		}
		sec, err := st.GetSecret(ctx, env, v.Secret, version)
		if errors.Is(err, store.ErrNoRow) {
			why = fmt.Sprintf("secret %s has no version %d, which its %s names", v.Secret, version, field)
			return ""
		}
		if err != nil {
			dbErr = fmt.Errorf("secret %s version %d: %w", v.Secret, version, err)
			return ""
		}
		plain, err := open.Open(sec.Sealed, envdeps.SecretAAD(env, v.Secret, version))
		if err != nil {
			why = fmt.Sprintf("secret %s version %d does not open with this key", v.Secret, version)
			return ""
		}
		return string(plain)
	}
	cred := map[string]any{"kind": c.Kind}
	if len(c.Headers) > 0 {
		hs := map[string]string{}
		for _, k := range slices.Sorted(maps.Keys(c.Headers)) {
			h := c.Headers[k]
			hs[k] = val("header "+k, &h)
		}
		cred["headers"] = hs
	}
	for _, f := range []struct {
		name string
		v    *spec.EnvValue
	}{{"user", c.User}, {"password", c.Password}, {"tokenUrl", c.TokenURL}, {"clientId", c.ClientID},
		{"clientSecret", c.ClientSecret}, {"scope", c.Scope}, {"audience", c.Audience}, {"certPem", c.CertPem},
		{"keyPem", c.KeyPem}} {
		if f.v != nil {
			cred[f.name] = val(f.name, f.v)
		}
	}
	if c.ClientAuthInBody {
		cred["clientAuthInBody"] = true
	}
	if dbErr != nil || why != "" {
		return nil, why, dbErr
	}
	def := map[string]any{"upstream": b.Upstream, "mode": b.Mode, "caPem": b.CaPem, "pathPrefixes": b.PathPrefixes,
		"forwardedHost": b.ForwardedHost, "credential": cred}
	if rl := b.RateLimit; rl != nil {
		def["rateLimit"] = map[string]int{"perSecond": rl.PerSecond, "burst": rl.Burst}
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return nil, "", err
	}
	return raw, "", nil
}
