// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package visits

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

func TestProviderList(t *testing.T) {
	f := newFx(t)
	stage, _ := f.env("stage", "fecode")
	prod, _ := f.env("prod", "")
	f.oidcProvider("sso", "s3cret")
	l, err := f.s.ListProviders(f.ctx, testTenant)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(l.Kinds, ",") != "tenant,ferun,oidc" || l.RedirectURI != testAuth+"/callback" || len(l.Providers) != 3 {
		t.Fatalf("list %+v", l)
	}
	b, _ := json.Marshal(l.Providers[0])
	if string(b) != `{"name":"ferun","kind":"ferun","builtIn":true,"environments":[{"id":"`+prod.ID.String()+
		`","name":"prod"}]}` {
		t.Fatalf("ferun %s", b)
	}
	fe := l.Providers[1]
	if fe.Name != "fecode" || fe.Kind != "tenant" || fe.URL != f.prov.url() || fe.BuiltIn || fe.ClientSecret != nil ||
		fe.Rule != nil || len(fe.Environments) != 1 || fe.Environments[0].ID != stage.ID.String() || fe.CreatedAt == "" {
		t.Fatalf("fecode %+v", fe)
	}
	b, _ = json.Marshal(l.Providers[2])
	for _, want := range []string{`"clientSecret":{"set":true}`, `"rule":{"domains":["example.com"]}`,
		`"backchannelUri":"https://auth.test/backchannel/acme/sso"`, `"environments":[]`, `"issuer":"https://login.example/"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("sso lacks %s: %s", want, b)
		}
	}
	if strings.Contains(string(b), "s3cret") {
		t.Fatalf("the secret answered: %s", b)
	}
	if p, err := f.s.GetProvider(f.ctx, testTenant, "ferun"); err != nil || !p.BuiltIn || len(p.Environments) != 1 {
		t.Fatalf("get ferun %+v %v", p, err)
	}
	if p, err := f.s.GetProvider(f.ctx, testTenant, "fecode"); err != nil || p.URL != f.prov.url() {
		t.Fatalf("get fecode %+v %v", p, err)
	}
	for _, name := range []string{"nope", "Bad_Name"} {
		_, err := f.s.GetProvider(f.ctx, testTenant, name)
		wantAPI(t, err, http.StatusNotFound, "", "no such provider")
	}
	// Another tenant has ferun alone.
	if l, err := f.s.ListProviders(f.ctx, "other"); err != nil || len(l.Providers) != 1 ||
		l.Providers[0].Environments == nil {
		t.Fatalf("other %+v %v", l, err)
	}
	// Without ferun-auth: tenant alone, no redirect URI, no back-channel.
	g := newFxOn(t, f.d, Config{})
	if l, err := g.s.ListProviders(g.ctx, testTenant); err != nil || strings.Join(l.Kinds, ",") != "tenant" ||
		l.RedirectURI != "" || l.Providers[2].BackchannelURI != "" {
		t.Fatalf("without ferun-auth %+v %v", l, err)
	}
}

func TestPutTenantProvider(t *testing.T) {
	f := newFx(t)
	url := f.prov.url()
	p, created, err := f.s.PutProvider(f.ctx, testTenant, "shop", ProviderIn{Kind: "tenant", URL: url}, "token:x")
	if err != nil || !created || p.Name != "shop" || p.URL != url || p.Environments == nil {
		t.Fatalf("make %+v %v %v", p, created, err)
	}
	row, err := f.st.GetAccessProvider(f.ctx, testTenant, "shop")
	if err != nil || row.MadeBy != "token:x" {
		t.Fatalf("row %+v %v", row, err)
	}
	// Unchanged: 200, nothing written.
	p2, created, err := f.s.PutProvider(f.ctx, testTenant, "shop", ProviderIn{Kind: "tenant", URL: url}, "token:y")
	if err != nil || created || p2.UpdatedAt != p.UpdatedAt {
		t.Fatalf("again %+v %v %v", p2, created, err)
	}
	if again, _ := f.st.GetAccessProvider(f.ctx, testTenant, "shop"); again.MadeBy != "token:x" ||
		!again.UpdatedAt.Equal(row.UpdatedAt) {
		t.Fatalf("an unchanged put wrote %+v", again)
	}
	// A new url keeps the visits.
	e, host := f.env("stage", "fecode")
	_, _, vid := f.visitIn(e, host, tenantCaller, "u-1")
	if _, created, err := f.s.PutProvider(f.ctx, testTenant, "fecode", ProviderIn{Kind: "tenant", URL: url + "/v2"},
		"token:x"); err != nil || created {
		t.Fatalf("new url %v %v", created, err)
	}
	if !f.row(vid).Open() {
		t.Fatal("a new url ended a visit")
	}

	for _, c := range []struct {
		name string
		in   ProviderIn
		text string
	}{
		{"shop", ProviderIn{Kind: "ferun"}, "a provider is of kind tenant or oidc"},
		{"shop", ProviderIn{}, "a provider is of kind tenant or oidc"},
		{"shop", ProviderIn{Kind: "tenant", URL: url, Issuer: "https://x/"}, "issuer is not a field of kind tenant"},
		{"shop", ProviderIn{Kind: "tenant", URL: url, ClientID: "c"}, "clientId is not a field of kind tenant"},
		{"shop", ProviderIn{Kind: "tenant", URL: url, ClientSecret: json.RawMessage(`"s"`)}, "clientSecret is not a field"},
		{"shop", ProviderIn{Kind: "tenant", URL: url, Rule: &Rule{Anyone: true}}, "rule is not a field"},
		{"shop", ProviderIn{Kind: "oidc", URL: url}, "url is not a field of kind oidc"},
		{"shop", ProviderIn{Kind: "tenant"}, "url: an absolute https URL"},
		{"shop", ProviderIn{Kind: "tenant", URL: "http://fecode.dev/visit"}, "url: an absolute https URL"},
		{"shop", ProviderIn{Kind: "tenant", URL: "https://user:pw@fecode.dev/visit"}, "url: an absolute https URL"},
		{"shop", ProviderIn{Kind: "tenant", URL: "/visit"}, "url: an absolute https URL"},
		{"shop", ProviderIn{Kind: "tenant", URL: url + "#x"}, "without a fragment"},
		{"shop", ProviderIn{Kind: "tenant", URL: "https://fecode.dev/" + strings.Repeat("x", 2048)}, "url: an absolute"},
		{"Shop", ProviderIn{Kind: "tenant", URL: url}, "a DNS label"},
		{strings.Repeat("a", 64), ProviderIn{Kind: "tenant", URL: url}, "a DNS label"},
	} {
		_, _, err := f.s.PutProvider(f.ctx, testTenant, c.name, c.in, "t")
		wantAPI(t, err, http.StatusBadRequest, CodeInvalidProvider, c.text)
	}
	_, _, err = f.s.PutProvider(f.ctx, testTenant, "ferun", ProviderIn{Kind: "tenant", URL: url}, "t")
	wantAPI(t, err, http.StatusConflict, CodeReservedName, "ferun is every tenant's provider and cannot be made")

	// An address the control plane may not reach, and a name that does
	// not resolve: the same 422.
	for u, host := range map[string]string{"https://10.0.0.1/visit": "10.0.0.1", "https://[fd00::1]/visit": "fd00::1",
		"https://nope.invalid/visit": "nope.invalid"} {
		_, _, err := f.s.PutProvider(f.ctx, testTenant, "shop", ProviderIn{Kind: "tenant", URL: u}, "t")
		wantAPI(t, err, http.StatusUnprocessableEntity, CodeDestinationDenied,
			"the address of "+host+" is not reachable from the control plane")
	}
	// With the built-in deny set, loopback is refused too.
	h, err := newService(Config{}, Deps{Store: f.st, Sealer: f.sealer}, nil)
	must(t, err)
	_, _, err = h.PutProvider(f.ctx, testTenant, "shop", ProviderIn{Kind: "tenant", URL: url}, "t")
	wantAPI(t, err, http.StatusUnprocessableEntity, CodeDestinationDenied, "the address of 127.0.0.1")
}

func TestPutOIDCProvider(t *testing.T) {
	f := newFx(t)
	in := ProviderIn{Kind: "oidc", Issuer: "https://login.example/", ClientID: "abc",
		ClientSecret: json.RawMessage(`"s3cret"`), Rule: &Rule{Domains: []string{"Example.COM"}}}
	p, created, err := f.s.PutProvider(f.ctx, testTenant, "sso", in, "token:x")
	if err != nil || !created || p.ClientSecret == nil || !p.ClientSecret.Set || p.Rule.Domains[0] != "example.com" ||
		p.BackchannelURI != testAuth+"/backchannel/acme/sso" {
		t.Fatalf("make %+v %v %v", p, created, err)
	}
	o, err := f.s.OIDCProvider(f.ctx, testTenant, "sso")
	if err != nil || o.ClientSecret != "s3cret" || o.Issuer != "https://login.example/" || o.ClientID != "abc" ||
		o.Tenant != testTenant || o.Name != "sso" || o.Rule.Domains[0] != "example.com" {
		t.Fatalf("oidc provider %+v %v", o, err)
	}
	row, _ := f.st.GetAccessProvider(f.ctx, testTenant, "sso")
	if strings.Contains(string(row.ClientSecretSealed), "s3cret") {
		t.Fatal("the secret is stored in plaintext")
	}
	// The same secret again, or {"set": true}: unchanged, nothing written.
	for _, secret := range []string{`"s3cret"`, `{"set": true}`} {
		in.ClientSecret = json.RawMessage(secret)
		if _, created, err := f.s.PutProvider(f.ctx, testTenant, "sso", in, "token:y"); err != nil || created {
			t.Fatalf("again with %s: %v %v", secret, created, err)
		}
		if again, _ := f.st.GetAccessProvider(f.ctx, testTenant, "sso"); !again.UpdatedAt.Equal(row.UpdatedAt) {
			t.Fatalf("an unchanged put with %s wrote", secret)
		}
	}
	// A visit: a new secret keeps it, a new rule ends it.
	e, host := f.env("stage", "sso")
	_, _, vid := f.visitIn(e, host, authCaller, "u")
	in.ClientSecret = json.RawMessage(`"other"`)
	if _, _, err := f.s.PutProvider(f.ctx, testTenant, "sso", in, "t"); err != nil || !f.row(vid).Open() {
		t.Fatalf("a new secret: %v, open %v", err, f.row(vid).Open())
	}
	if o, _ := f.s.OIDCProvider(f.ctx, testTenant, "sso"); o.ClientSecret != "other" {
		t.Fatalf("secret %q", o.ClientSecret)
	}
	in.Rule = &Rule{Emails: []string{"Ada@Example.com"}}
	p, _, err = f.s.PutProvider(f.ctx, testTenant, "sso", in, "t")
	if err != nil || p.Rule.Emails[0] != "ada@example.com" || f.row(vid).EndReason != model.VisitProviderChanged {
		t.Fatalf("a new rule %+v %v", p, err)
	}

	for _, c := range []struct {
		in   ProviderIn
		text string
	}{
		{ProviderIn{Kind: "oidc", Issuer: "http://login.example/", ClientID: "c", ClientSecret: json.RawMessage(`"s"`),
			Rule: &Rule{Anyone: true}}, "issuer: an absolute https URL"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientSecret: json.RawMessage(`"s"`), Rule: &Rule{Anyone: true}},
			"clientId"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c\x01", ClientSecret: json.RawMessage(`"s"`),
			Rule: &Rule{Anyone: true}}, "clientId"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", Rule: &Rule{Anyone: true}}, "clientSecret"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`""`),
			Rule: &Rule{Anyone: true}}, "clientSecret"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`{"set": false}`),
			Rule: &Rule{Anyone: true}}, "clientSecret"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`"` +
			strings.Repeat("s", 4097) + `"`), Rule: &Rule{Anyone: true}}, "clientSecret"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`"s"`)}, "rule"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`"s"`),
			Rule: &Rule{}}, "rule"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`"s"`),
			Rule: &Rule{Anyone: true, Groups: []string{"g"}}}, "rule"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`"s"`),
			Rule: &Rule{Emails: []string{}}}, "rule"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`"s"`),
			Rule: &Rule{Emails: []string{"Ada <a@example.com>"}}}, "e-mail address"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`"s"`),
			Rule: &Rule{Emails: []string{"a@examKple.com"}}}, "e-mail address"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`"s"`),
			Rule: &Rule{Domains: []string{"a@example.com"}}}, "DNS name"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`"s"`),
			Rule: &Rule{Groups: []string{""}}}, "group"},
		{ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c", ClientSecret: json.RawMessage(`"s"`),
			Rule: &Rule{Groups: make([]string, 101)}}, "rule"},
	} {
		_, _, err := f.s.PutProvider(f.ctx, testTenant, "sso2", c.in, "t")
		wantAPI(t, err, http.StatusBadRequest, CodeInvalidProvider, c.text)
	}
	// {"set": true} for a provider without a secret.
	_, _, err = f.s.PutProvider(f.ctx, testTenant, "sso3", ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c",
		ClientSecret: json.RawMessage(`{"set": true}`), Rule: &Rule{Anyone: true}}, "t")
	wantAPI(t, err, http.StatusBadRequest, CodeInvalidProvider, "has none")
	_, _, err = f.s.PutProvider(f.ctx, testTenant, "fecode", ProviderIn{Kind: "oidc", Issuer: "https://l/", ClientID: "c",
		ClientSecret: json.RawMessage(`{"set": true}`), Rule: &Rule{Anyone: true}}, "t")
	wantAPI(t, err, http.StatusBadRequest, CodeInvalidProvider, "has none")

	// Without ferun-auth: 409.
	g := newFxOn(t, f.d, Config{})
	in.ClientSecret = json.RawMessage(`"s"`)
	_, _, err = g.s.PutProvider(g.ctx, testTenant, "sso", in, "t")
	wantAPI(t, err, http.StatusConflict, CodeFerunAuthRequired, "ferun-auth")

	// OIDCProvider answers kind oidc alone.
	for _, name := range []string{"fecode", "nope", "ferun"} {
		_, err := f.s.OIDCProvider(f.ctx, testTenant, name)
		wantAPI(t, err, http.StatusNotFound, "", "no such provider")
	}
}

func TestProviderKindChange(t *testing.T) {
	f := newFx(t)
	e, host := f.env("stage", "fecode")
	_, _, vid := f.visitIn(e, host, tenantCaller, "u")
	// The provider becomes an oidc one: its visits end, its environment
	// stays guarded by it.
	_, created, err := f.s.PutProvider(f.ctx, testTenant, "fecode", ProviderIn{Kind: "oidc", Issuer: "https://l/",
		ClientID: "c", ClientSecret: json.RawMessage(`"s"`), Rule: &Rule{Anyone: true}}, "t")
	if err != nil || created || f.row(vid).EndReason != model.VisitProviderChanged {
		t.Fatalf("kind change %v %v %q", created, err, f.row(vid).EndReason)
	}
	sealed, _ := f.challenge(e, host, "/")
	if _, err := f.s.OpenChallenge(f.ctx, authCaller, sealed); err != nil {
		t.Fatal(err)
	}
}

func TestProviderLimitAndDelete(t *testing.T) {
	f := newFx(t)
	for i := range maxProviders - 1 {
		f.exec("INSERT INTO access_providers (tenant, name, kind, url) VALUES ($1, $2, 'tenant', 'https://x.example/')",
			testTenant, "p"+uuid.NewString()[:8]+string(rune('a'+i%26)))
	}
	_, _, err := f.s.PutProvider(f.ctx, testTenant, "one-more", ProviderIn{Kind: "tenant", URL: f.prov.url()}, "t")
	wantAPI(t, err, http.StatusConflict, CodeTooManyProviders, "at most 50")
	// A change of one that exists is no new one.
	if _, _, err := f.s.PutProvider(f.ctx, testTenant, "fecode", ProviderIn{Kind: "tenant", URL: f.prov.url() + "/x"},
		"t"); err != nil {
		t.Fatal(err)
	}

	e, _ := f.env("stage", "fecode")
	f.env("prod", "fecode")
	err = f.s.DeleteProvider(f.ctx, testTenant, "fecode")
	wantAPI(t, err, http.StatusConflict, CodeProviderInUse, "provider fecode is named by 2 environments")
	err = f.s.DeleteProvider(f.ctx, testTenant, "ferun")
	wantAPI(t, err, http.StatusConflict, CodeReservedName, "")
	err = f.s.DeleteProvider(f.ctx, testTenant, "nope")
	wantAPI(t, err, http.StatusNotFound, "", "no such provider")
	for _, name := range []string{"stage", "prod"} {
		x, err := f.st.LockEnvironmentByName(f.ctx, testTenant, name, store.NoLock)
		must(t, err)
		_, _, err = f.st.SetEnvironmentAccess(f.ctx, x.ID, model.AccessAnyone, "")
		must(t, err)
	}
	_ = e
	if err := f.s.DeleteProvider(f.ctx, testTenant, "fecode"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetProvider(f.ctx, testTenant, "fecode"); err == nil {
		t.Fatal("deleted provider read")
	}
}
