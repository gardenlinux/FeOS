// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visits

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/egress"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// providerName is a provider's name: a DNS label of at most 63.
var providerName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// The bounds of a provider's fields.
const (
	maxURL       = 2048
	maxClientID  = 255
	maxSecret    = 4096
	maxRuleItems = 100
	maxGroup     = 255
	maxEmail     = 254
	maxDomain    = 253
)

// pass returns err as it is when it is an answer of the API (an
// *apierr.Error), else the failure it is (failed).
func (s *Service) pass(what string, err error) error {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return err
	}
	return s.failed(what, err)
}

// rfc3339 is t as the answers write times: RFC 3339 in seconds, UTC; ""
// for none.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// builtin is the provider ferun as answered.
func builtin(envs []EnvRef) Provider {
	return Provider{Name: model.ProviderFerun, Kind: model.ProviderKindFerun, BuiltIn: true, Environments: envs}
}

// view is a provider row as answered: its secret only as set.
func (s *Service) view(p store.AccessProvider, envs []EnvRef) Provider {
	v := Provider{Name: p.Name, Kind: p.Kind, URL: p.URL, Environments: envs, CreatedAt: rfc3339(p.CreatedAt),
		UpdatedAt: rfc3339(p.UpdatedAt)}
	if p.Kind == model.ProviderKindOIDC {
		v.Issuer, v.ClientID = p.Issuer, p.ClientID
		v.ClientSecret = &Secret{Set: len(p.ClientSecretSealed) > 0}
		r := ruleOf(p.Rule)
		v.Rule = &r
		if s.cfg.AuthURL != "" {
			v.BackchannelURI = s.cfg.AuthURL + "/backchannel/" + p.Tenant + "/" + p.Name
		}
	}
	return v
}

// ruleOf is a stored rule.
func ruleOf(raw json.RawMessage) Rule {
	var r Rule
	_ = json.Unmarshal(raw, &r)
	return r
}

// envsByProvider are the environments of tenant whose access is visitors,
// by the provider each names (model.ProviderFerun for the builtin).
func (s *Service) envsByProvider(ctx context.Context, tenant string) (map[string][]EnvRef, error) {
	rows, err := s.st.ProviderEnvironments(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := map[string][]EnvRef{}
	for _, r := range rows {
		name := r.Provider
		if name == "" {
			name = model.ProviderFerun
		}
		out[name] = append(out[name], EnvRef{ID: r.ID.String(), Name: r.Name})
	}
	return out, nil
}

// refs is never nil, so that the answers carry [].
func refs(r []EnvRef) []EnvRef {
	if r == nil {
		return []EnvRef{}
	}
	return r
}

// ListProviders lists a tenant's providers, ferun first, then by name,
// with the kinds this installation serves and the redirect URI an issuer
// of a provider of kind oidc registers ("" without ferun-auth).
func (s *Service) ListProviders(ctx context.Context, tenant string) (ProviderList, error) {
	rows, err := s.st.ListAccessProviders(ctx, tenant)
	if err != nil {
		return ProviderList{}, s.failed("list providers", err)
	}
	envs, err := s.envsByProvider(ctx, tenant)
	if err != nil {
		return ProviderList{}, s.failed("list providers", err)
	}
	out := ProviderList{Providers: []Provider{builtin(refs(envs[model.ProviderFerun]))}, Kinds: s.Kinds()}
	for _, p := range rows {
		out.Providers = append(out.Providers, s.view(p, refs(envs[p.Name])))
	}
	if s.cfg.AuthURL != "" {
		out.RedirectURI = s.cfg.AuthURL + "/callback"
	}
	return out, nil
}

// GetProvider answers one of a tenant's providers; ferun always exists.
// 404 no such provider else.
func (s *Service) GetProvider(ctx context.Context, tenant, name string) (Provider, error) {
	envs, err := s.envsByProvider(ctx, tenant)
	if err != nil {
		return Provider{}, s.failed("read a provider", err)
	}
	if name == model.ProviderFerun {
		return builtin(refs(envs[name])), nil
	}
	if !providerName.MatchString(name) {
		return Provider{}, errNoProvider()
	}
	p, err := s.st.GetAccessProvider(ctx, tenant, name)
	if errors.Is(err, store.ErrNoRow) {
		return Provider{}, errNoProvider()
	}
	if err != nil {
		return Provider{}, s.failed("read a provider", err)
	}
	return s.view(p, refs(envs[name])), nil
}

// secretIn is a PUT's clientSecret: a new secret, or keep the stored one.
type secretIn struct {
	value string
	keep  bool
}

// PutProvider makes or changes a tenant's provider (§1.8.2): 201 when it
// makes one (created), 200 when it changes one or finds it as asked, which
// writes nothing. A change of the kind, the issuer, the client or the rule
// ends the provider's open visits; a change of the url or the secret alone
// does not. The address of a provider of kind tenant is judged by the
// control plane's destination rule before anything is written.
func (s *Service) PutProvider(ctx context.Context, tenant, name string, in ProviderIn, madeBy string) (p Provider, created bool, err error) {
	if name == model.ProviderFerun {
		return Provider{}, false, apierr.WithCode(http.StatusConflict, CodeReservedName, textReservedName)
	}
	if !providerName.MatchString(name) {
		return Provider{}, false, invalidProvider("provider name %q: a DNS label of at most 63 lowercase letters, digits and hyphens", name)
	}
	want, secret, err := s.checkProviderIn(ctx, tenant, name, in)
	if err != nil {
		return Provider{}, false, err
	}
	want.MadeBy = madeBy
	// A new secret is sealed outside the transaction (which may run again).
	var sealed []byte
	if want.Kind == model.ProviderKindOIDC && !secret.keep {
		if sealed, err = s.d.Sealer.Seal([]byte(secret.value), store.AccessProviderAAD(tenant, name)); err != nil {
			return Provider{}, false, s.failed("seal a client secret", err)
		}
	}
	var row store.AccessProvider
	ended, wrote := 0, false
	err = s.st.InTx(ctx, func(tx *store.Store) error {
		created, ended, wrote = false, 0, false
		t, err := tx.LockTenant(ctx, tenant, store.ForNoKeyUpdate)
		if errors.Is(err, store.ErrNoRow) {
			return apierr.New(http.StatusUnauthorized, "unknown tenant token")
		}
		if err != nil {
			return err
		}
		if t.Deleting() {
			return apierr.New(http.StatusConflict, "tenant %s is being deleted", tenant)
		}
		old, err := tx.LockAccessProvider(ctx, tenant, name, store.ForUpdate)
		if errors.Is(err, store.ErrNoRow) {
			n, err := tx.CountAccessProviders(ctx, tenant)
			if err != nil {
				return err
			}
			if n >= maxProviders {
				return apierr.WithCode(http.StatusConflict, CodeTooManyProviders, textTooManyProviders, maxProviders)
			}
			if secret.keep && want.Kind == model.ProviderKindOIDC {
				return invalidProvider("clientSecret {\"set\": true} keeps the stored secret, and provider %s has none", name)
			}
			want.ClientSecretSealed = sealed
			row, err = tx.InsertAccessProvider(ctx, want)
			created, wrote = err == nil, err == nil
			return err
		}
		if err != nil {
			return err
		}
		next := want
		if want.Kind == model.ProviderKindOIDC {
			switch {
			case secret.keep && len(old.ClientSecretSealed) == 0:
				return invalidProvider("clientSecret {\"set\": true} keeps the stored secret, and provider %s has none", name)
			case secret.keep || s.sameSecret(old, secret.value):
				next.ClientSecretSealed = old.ClientSecretSealed
			default:
				next.ClientSecretSealed = sealed
			}
		}
		identity := old.Kind != next.Kind || old.Issuer != next.Issuer || old.ClientID != next.ClientID ||
			!sameRule(old.Rule, next.Rule)
		if !identity && old.URL == next.URL && bytes.Equal(old.ClientSecretSealed, next.ClientSecretSealed) {
			row = old
			return nil
		}
		if row, err = tx.UpdateAccessProvider(ctx, next); err != nil {
			return err
		}
		wrote = true
		if identity {
			ended, err = tx.EndProviderVisits(ctx, tenant, name, model.VisitProviderChanged)
		}
		return err
	})
	if err != nil {
		return Provider{}, false, s.pass("put a provider", err)
	}
	if wrote {
		s.log.Info("access provider put", "tenant", tenant, "provider", name, "kind", row.Kind, "created", created,
			"visits_ended", ended)
	}
	envs, err := s.envsByProvider(ctx, tenant)
	if err != nil {
		return Provider{}, false, s.failed("read a provider", err)
	}
	return s.view(row, refs(envs[name])), created, nil
}

// sameSecret reports whether old's stored secret is value.
func (s *Service) sameSecret(old store.AccessProvider, value string) bool {
	if len(old.ClientSecretSealed) == 0 {
		return false
	}
	plain, err := s.d.Sealer.Open(old.ClientSecretSealed, store.AccessProviderAAD(old.Tenant, old.Name))
	return err == nil && string(plain) == value
}

// sameRule compares two stored rules by what they say.
func sameRule(a, b json.RawMessage) bool {
	x, _ := json.Marshal(ruleOf(a))
	y, _ := json.Marshal(ruleOf(b))
	return bytes.Equal(x, y)
}

// checkProviderIn checks a PUT's body and returns the row it asks for
// (without a sealed secret) and its secret.
func (s *Service) checkProviderIn(ctx context.Context, tenant, name string, in ProviderIn) (store.AccessProvider, secretIn, error) {
	row := store.AccessProvider{Tenant: tenant, Name: name, Kind: in.Kind}
	other := func(field string) error {
		return invalidProvider("%s is not a field of kind %s", field, in.Kind)
	}
	switch in.Kind {
	case model.ProviderKindTenant:
		switch {
		case in.Issuer != "":
			return row, secretIn{}, other("issuer")
		case in.ClientID != "":
			return row, secretIn{}, other("clientId")
		case len(in.ClientSecret) > 0:
			return row, secretIn{}, other("clientSecret")
		case in.Rule != nil:
			return row, secretIn{}, other("rule")
		}
		u, err := httpsURL("url", in.URL)
		if err != nil {
			return row, secretIn{}, err
		}
		if u.Fragment != "" || strings.Contains(in.URL, "#") {
			return row, secretIn{}, invalidProvider("url: an https URL without a fragment")
		}
		// One answer for a refused address and for a name that does not
		// resolve, so that the check tells nothing of the names inside.
		if err := checkHost(ctx, s.judge, u.Hostname()); err != nil {
			s.log.Debug("a provider's address refused", "tenant", tenant, "provider", name, "err", err)
			return row, secretIn{}, apierr.WithCode(http.StatusUnprocessableEntity, CodeDestinationDenied,
				textDestinationDenied, u.Hostname())
		}
		row.URL = in.URL
		return row, secretIn{}, nil
	case model.ProviderKindOIDC:
		if in.URL != "" {
			return row, secretIn{}, other("url")
		}
		if !s.serves(model.ProviderKindOIDC) {
			return row, secretIn{}, apierr.WithCode(http.StatusConflict, CodeFerunAuthRequired, textOIDCNeedsFerunAuth)
		}
		if _, err := httpsURL("issuer", in.Issuer); err != nil {
			return row, secretIn{}, err
		}
		if l := len(in.ClientID); l == 0 || l > maxClientID || !printable(in.ClientID) {
			return row, secretIn{}, invalidProvider("clientId: 1 to %d printable characters", maxClientID)
		}
		secret, err := clientSecretIn(in.ClientSecret)
		if err != nil {
			return row, secretIn{}, err
		}
		rule, err := checkRule(in.Rule)
		if err != nil {
			return row, secretIn{}, err
		}
		raw, _ := json.Marshal(rule)
		row.Issuer, row.ClientID, row.Rule = in.Issuer, in.ClientID, raw
		return row, secret, nil
	}
	return row, secretIn{}, invalidProvider("kind %q: a provider is of kind tenant or oidc", in.Kind)
}

// checkHost is egress.CheckHost (tests replace it).
var checkHost = egress.CheckHost

// httpsURL parses an absolute https URL with a host, without userinfo, at
// most maxURL bytes.
func httpsURL(field, raw string) (*url.URL, error) {
	bad := invalidProvider("%s: an absolute https URL with a host, without userinfo, of at most %d bytes", field, maxURL)
	if raw == "" || len(raw) > maxURL || !printable(raw) || strings.ContainsAny(raw, " ") {
		return nil, bad
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Hostname() == "" {
		return nil, bad
	}
	return u, nil
}

// printable reports whether s is valid UTF-8 without control characters.
func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// clientSecretIn reads a PUT's clientSecret: a string of 1 to maxSecret
// bytes, or {"set": true}, which keeps the stored one.
func clientSecretIn(raw json.RawMessage) (secretIn, error) {
	bad := invalidProvider("clientSecret: a string of 1 to %d bytes, or {\"set\": true} to keep the stored one", maxSecret)
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return secretIn{}, bad
	}
	if raw[0] == '"' {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil || v == "" || len(v) > maxSecret {
			return secretIn{}, bad
		}
		return secretIn{value: v}, nil
	}
	var keep map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keep); err != nil || len(keep) != 1 || string(bytes.TrimSpace(keep["set"])) != "true" {
		return secretIn{}, bad
	}
	return secretIn{keep: true}, nil
}

// checkRule checks an oidc provider's rule (exactly one of its fields)
// and returns it with emails and domains in ASCII lower case.
func checkRule(r *Rule) (Rule, error) {
	bad := invalidProvider(`rule: exactly one of {"anyone": true}, {"emails": [...]}, {"domains": [...]}, {"groups": [...]}, each list of 1 to %d`, maxRuleItems)
	if r == nil {
		return Rule{}, bad
	}
	set := 0
	for _, on := range []bool{r.Anyone, r.Emails != nil, r.Domains != nil, r.Groups != nil} {
		if on {
			set++
		}
	}
	if set != 1 {
		return Rule{}, bad
	}
	list := func(items []string, check func(string) (string, bool), what string) ([]string, error) {
		if len(items) == 0 || len(items) > maxRuleItems {
			return nil, bad
		}
		out := make([]string, 0, len(items))
		for _, it := range items {
			v, ok := check(it)
			if !ok {
				return nil, invalidProvider("rule: %q is no %s", it, what)
			}
			out = append(out, v)
		}
		return out, nil
	}
	var out Rule
	var err error
	switch {
	case r.Anyone:
		out.Anyone = true
	case r.Emails != nil:
		out.Emails, err = list(r.Emails, checkEmail, "e-mail address")
	case r.Domains != nil:
		out.Domains, err = list(r.Domains, checkDomain, "DNS name")
	default:
		out.Groups, err = list(r.Groups, func(g string) (string, bool) {
			return g, g != "" && len(g) <= maxGroup && printable(g)
		}, "group of 1 to 255 printable characters")
	}
	return out, err
}

// asciiLower lowers ASCII letters only (a rule compares exactly after
// that, never with Unicode folding).
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// checkEmail is a bare ASCII e-mail address, lowered.
func checkEmail(e string) (string, bool) {
	if e == "" || len(e) > maxEmail || !isASCII(e) {
		return "", false
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || a.Name != "" {
		return "", false
	}
	_, domain, ok := strings.Cut(e, "@")
	if !ok {
		return "", false
	}
	if _, ok := checkDomain(domain); !ok {
		return "", false
	}
	return asciiLower(e), true
}

// checkDomain is a DNS name, lowered: labels of letters, digits and
// hyphens, at most 253 bytes, no "@".
func checkDomain(d string) (string, bool) {
	d = asciiLower(d)
	if d == "" || len(d) > maxDomain {
		return "", false
	}
	for _, l := range strings.Split(d, ".") {
		if !providerName.MatchString(l) {
			return "", false
		}
	}
	return d, true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 || s[i] < 0x20 {
			return false
		}
	}
	return true
}

// DeleteProvider deletes a tenant's provider no environment names: 404 no
// such provider, 409 reserved_name for ferun, 409 provider_in_use for one
// an environment names.
func (s *Service) DeleteProvider(ctx context.Context, tenant, name string) error {
	if name == model.ProviderFerun {
		return apierr.WithCode(http.StatusConflict, CodeReservedName, textReservedName)
	}
	if !providerName.MatchString(name) {
		return errNoProvider()
	}
	ok, err := s.st.DeleteAccessProvider(ctx, tenant, name)
	if errors.Is(err, store.ErrInUse) {
		ids, err := s.st.EnvironmentsNamingProvider(ctx, tenant, name)
		if err != nil {
			return s.failed("delete a provider", err)
		}
		return apierr.WithCode(http.StatusConflict, CodeProviderInUse, textProviderInUse, name, len(ids))
	}
	if err != nil {
		return s.failed("delete a provider", err)
	}
	if !ok {
		return errNoProvider()
	}
	s.log.Info("access provider deleted", "tenant", tenant, "provider", name)
	return nil
}
