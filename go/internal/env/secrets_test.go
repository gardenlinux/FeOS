// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// putHTTPSecret writes a version of HTTP secret name of tenantA's
// environment env through the secrets' PUT: a header APIKey with key on
// an upstream that is an address on the internet, in mode.
func (h *harness) putHTTPSecret(env, name, key, mode string) SecretVersion {
	h.t.Helper()
	v, err := h.svc.PutSecret(h.ctx(), tenantA, env, name, PutSecretRequest{Type: SecretHTTP,
		Upstream: "https://203.0.113.40", Mode: mode, Author: "tester",
		Credential: json.RawMessage(`{"kind":"headers","headers":{"APIKey":` + mustQuote(key) + `}}`)})
	if err != nil {
		h.t.Fatalf("put HTTP secret %s of %s: %v", name, env, err)
	}
	return v
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// codeOf is the code of an API error, "" for none.
func codeOf(err error) (int, string) {
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		return 0, ""
	}
	return ae.Status, ae.Code
}

// A secret is a value or an HTTP API, in one name space (ADR 0055): the
// list has both, a read either, and a write of one kind to a name the
// other holds is 409 secret_kind, also below the API (migration 0040's
// trigger).
func TestSecretsOfBothKinds(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.withCredentials()
	h.create("prod", nil)
	h.putSecret("prod", "token", "t0ken")
	one := h.putHTTPSecret("prod", "s4", "key-1", "")
	if !one.Created || one.Version != 1 || one.Type != SecretHTTP || one.HTTPSecretDef == nil ||
		one.Upstream != "https://203.0.113.40" || one.Mode != "name" {
		t.Fatalf("the first version: %+v", one)
	}
	if again := h.putHTTPSecret("prod", "s4", "key-1", "name"); again.Created || again.Version != 1 {
		t.Fatalf("the same definition again: %+v", again)
	}
	// A kept key under other prefixes is version 2 (the key goes to the
	// same API).
	two, err := h.svc.PutSecret(h.ctx(), tenantA, "prod", "s4", PutSecretRequest{Type: SecretHTTP,
		Upstream: "https://203.0.113.40", PathPrefixes: []string{"/odata/"},
		Credential: json.RawMessage(`{"kind":"headers","headers":{"APIKey":{"set":true}}}`)})
	if err != nil || !two.Created || two.Version != 2 || len(two.PathPrefixes) != 1 {
		t.Fatalf("a kept key under other prefixes: %+v %v", two, err)
	}

	list, err := h.svc.Secrets(h.ctx(), tenantA, "prod")
	if err != nil || len(list.Secrets) != 2 {
		t.Fatalf("the list: %+v %v", list, err)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "key-1") || strings.Contains(string(raw), "t0ken") {
		t.Fatalf("the list shows a secret: %s", raw)
	}
	s4, token := list.Secrets[0], list.Secrets[1]
	if s4.Name != "s4" || s4.Type != SecretHTTP || s4.HTTPSecretDef == nil || s4.Version != 2 ||
		s4.Credential.Headers["Apikey"].Set != true || s4.UsedBy.Objects == nil {
		t.Fatalf("the HTTP secret: %s", raw)
	}
	if token.Name != "token" || token.Type != SecretValue || token.HTTPSecretDef != nil {
		t.Fatalf("the value: %s", raw)
	}
	var item map[string]any
	b, _ := json.Marshal(token)
	if err := json.Unmarshal(b, &item); err != nil || len(item) != 5 {
		t.Fatalf("a value's item has its five fields: %s", b)
	}

	if got, err := h.svc.Secret(h.ctx(), tenantA, "prod", "s4"); err != nil || got.Type != SecretHTTP || got.Version != 2 {
		t.Fatalf("GET s4: %+v %v", got, err)
	}
	if got, err := h.svc.Secret(h.ctx(), tenantA, "prod", "token"); err != nil || got.Type != SecretValue {
		t.Fatalf("GET token: %+v %v", got, err)
	}
	if _, err := h.svc.Secret(h.ctx(), tenantA, "prod", "nope"); !isCode(err, http.StatusNotFound, "no_secret") {
		t.Fatalf("GET nope: %v", err)
	}

	_, err = h.svc.PutSecret(h.ctx(), tenantA, "prod", "s4", PutSecretRequest{Value: "v"})
	if !isCode(err, http.StatusConflict, "secret_kind") {
		t.Fatalf("a value over an HTTP secret: %v", err)
	}
	_, err = h.svc.PutSecret(h.ctx(), tenantA, "prod", "token", PutSecretRequest{Type: SecretHTTP,
		Upstream: "https://203.0.113.40", Credential: json.RawMessage(`{"kind":"headers","headers":{"APIKey":"k"}}`)})
	if !isCode(err, http.StatusConflict, "secret_kind") {
		t.Fatalf("an HTTP secret over a value: %v", err)
	}
	_, err = h.svc.PutSecret(h.ctx(), tenantA, "prod", "x", PutSecretRequest{Value: "v", Upstream: "https://a.example"})
	if st, _ := codeOf(err); st != http.StatusBadRequest {
		t.Fatalf("a value with an upstream: %v", err)
	}

	// Below the API: the trigger refuses either insert.
	prod := h.env("prod")
	_, err = h.st.InsertSecret(h.ctx(), store.EnvSecret{EnvID: prod.ID, Name: "s4", Version: 1, Sealed: []byte{1}})
	if !store.SecretKindConflict(err) {
		t.Fatalf("a value row of an HTTP secret's name: %v", err)
	}
	err = h.st.InTx(h.ctx(), func(tx *store.Store) error {
		return tx.InsertEnvCredential(h.ctx(), store.EnvCredential{EnvID: prod.ID, Name: "token", Version: 1,
			Upstream: "https://203.0.113.40", Mode: "name", Kind: "headers", Sealed: []byte{1}})
	})
	if !store.SecretKindConflict(err) {
		t.Fatalf("an HTTP secret row of a value's name: %v", err)
	}

	// Deleted, a value's name is free for an HTTP secret.
	if err := h.svc.DeleteSecret(h.ctx(), tenantA, "prod", "token"); err != nil {
		t.Fatal(err)
	}
	if v := h.putHTTPSecret("prod", "token", "k", ""); v.Version != 1 {
		t.Fatalf("an HTTP secret of a deleted value's name: %+v", v)
	}
	if err := h.svc.DeleteSecret(h.ctx(), tenantA, "prod", "s4"); err != nil {
		t.Fatalf("delete an unused HTTP secret: %v", err)
	}
	if err := h.svc.DeleteSecret(h.ctx(), tenantA, "prod", "s4"); !isCode(err, http.StatusNotFound, "no_secret") {
		t.Fatalf("delete it again: %v", err)
	}
}

func isCode(err error, status int, code string) bool {
	st, c := codeOf(err)
	return st == status && c == code
}

// s4Spec has the service shop call HTTP secret s4 by its URL.
const s4Spec = `{"services": {"shop": {"image": "ghcr.io/acme/shop:1",
  "env": {"S4_URL": {"secret": "s4", "key": "url"}}}}}`

// A variable that references an HTTP secret gets its pods the binding of
// the secret's name at the version the deployment pins: a new version
// reaches them with the next deployment, sealed again in place, and the
// secret's uses say who pins it.
func TestHTTPSecretByVariable(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.withCredentials()
	h.create("prod", nil)
	h.putHTTPSecret("prod", "s4", "key-1", "")
	h.putSecret("prod", "token", "t0ken")
	prod := h.env("prod")

	a := h.apply("prod", s4Spec, 0)
	sp, err := h.svc.deploymentSpec(h.ctx(), prod, a.Number)
	if err != nil {
		t.Fatal(err)
	}
	want := spec.SecretRef{Version: 1, Type: spec.SecretTypeHTTP, Upstream: "https://203.0.113.40", Mode: "name"}
	if got := sp.Secrets["s4"]; got != want {
		t.Fatalf("the pin: %+v", got)
	}
	tb := model.TemplateHTTPBinding{Name: "s4", Upstream: "https://203.0.113.40", Mode: "name"}
	pod := h.podOf(prod.ID, "shop", a.Number, tb)
	got, v1, err := h.svc.RowHTTPBindings(h.ctx(), pod, true)
	if err != nil || len(got) != 1 || got[0].GetName() != "s4" || got[0].GetHeaders().GetHeaders()["Apikey"] != "key-1" {
		t.Fatalf("the pod's bindings: %v %v", got, err)
	}
	if _, closed, _ := h.svc.RowHTTPBindings(h.ctx(), pod, false); closed != v1 {
		t.Fatalf("the version opened %s and not %s", v1, closed)
	}

	// Version 2 reaches the pod with the next deployment, in place.
	h.putHTTPSecret("prod", "s4", "key-2", "")
	if got, _, _ := h.svc.RowHTTPBindings(h.ctx(), pod, true); got[0].GetHeaders().GetHeaders()["Apikey"] != "key-1" {
		t.Fatalf("a new version moved no pin, but the pod got %v", got)
	}
	b := h.apply("prod", s4Spec, a.Number)
	var secretEntry, serviceEntry *spec.DiffEntry
	for i, d := range b.Diff {
		switch d.Kind {
		case spec.KindSecret:
			secretEntry = &b.Diff[i]
		case spec.KindService:
			serviceEntry = &b.Diff[i]
		}
	}
	if secretEntry == nil || secretEntry.Change != spec.ChangeUpdate || serviceEntry == nil ||
		serviceEntry.Effect != spec.EffectInPlace {
		t.Fatalf("the diff of a new version: %+v", b.Diff)
	}
	h.exec("UPDATE env_instances SET deployment = $1 WHERE sandbox_id = $2", b.Number, pod.ID)
	got, v2, err := h.svc.RowHTTPBindings(h.ctx(), pod, true)
	if err != nil || got[0].GetHeaders().GetHeaders()["Apikey"] != "key-2" || v2 == v1 {
		t.Fatalf("the moved pod's bindings: %v %s %v", got, v2, err)
	}

	uses, err := h.svc.CredentialUses(h.ctx(), h.st, prod.ID)
	if u := uses["s4"]; err != nil || len(u.Objects) != 1 || u.Objects[0] != "service/shop" || u.Latest != 2 {
		t.Fatalf("uses: %+v %v", uses, err)
	}
	if err := h.svc.DeleteSecret(h.ctx(), tenantA, "prod", "s4"); !isCode(err, http.StatusConflict, "secret_in_use") ||
		!strings.Contains(err.Error(), "service shop") {
		t.Fatalf("delete while pinned: %v", err)
	}

	// The references the kinds refuse, each at its variable.
	for _, c := range []struct{ env, problem string }{
		{`{"secret": "s4"}`, "its key never reaches a container"},
		{`{"secret": "token", "key": "url"}`, "secret token is a value"},
		{`{"secret": "s4", "key": "token"}`, "key url or host"},
		{`{"secret": "nope", "key": "url"}`, "no HTTP secret nope"},
	} {
		raw := `{"services": {"shop": {"image": "ghcr.io/acme/shop:1", "env": {"X": ` + c.env + `}}}}`
		_, err := h.svc.Apply(h.ctx(), tenantA, "prod", ApplyRequest{Spec: json.RawMessage(raw), Base: b.Number})
		if f, ok := findingAt(err, "services.shop.env.X"); !ok || !strings.Contains(f.Problem, c.problem) {
			t.Errorf("%s: %s", c.env, describe(err))
		}
	}
	// A service of the name the pods would resolve to the API.
	raw := `{"services": {"shop": {"image": "ghcr.io/acme/shop:1", "env": {"X": {"secret": "s4", "key": "host"}}},
	  "s4": {"image": "ghcr.io/acme/shop:1"}}}`
	_, err = h.svc.Apply(h.ctx(), tenantA, "prod", ApplyRequest{Spec: json.RawMessage(raw), Base: b.Number})
	if f, ok := findingAt(err, "services.shop.env.X"); !ok || !strings.Contains(f.Problem, "a service's name") {
		t.Errorf("a service named as the binding: %s", describe(err))
	}
	// A spec without an environment cannot look it up: a warning.
	ans, err := h.svc.PlanSpec(h.ctx(), tenantA, PlanSpecRequest{Spec: json.RawMessage(s4Spec)})
	if err != nil || !ans.Valid {
		t.Fatalf("a spec without an environment: %+v %v", ans, err)
	}
	warned := false
	for _, f := range ans.Findings {
		warned = warned || (f.Severity == spec.SeverityWarning && f.Path == "services.shop.env.S4_URL")
	}
	if !warned {
		t.Fatalf("no warning of the lookup: %+v", ans.Findings)
	}
}

// A clone copies an HTTP secret it is asked to copy, as version 1 of its
// own, which its spec pins; one its variables reference and it does not
// copy is the clone's finding.
func TestCloneCopiesHTTPSecrets(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	bs := h.withCredentials()
	h.create("prod", nil)
	h.putHTTPSecret("prod", "s4", "key-1", "")
	h.apply("prod", s4Spec, 0)
	h.putHTTPSecret("prod", "s4", "key-2", "")

	_, err := h.svc.Clone(h.ctx(), tenantA, "prod", CloneRequest{Name: "copy"})
	if f, ok := findingAt(err, "services.shop.env.S4_URL"); !ok || !strings.Contains(f.Remedy, `secrets: {"s4": {"copy": true}}`) {
		t.Fatalf("a clone without the HTTP secret: %s", describe(err))
	}
	st, err := h.svc.Clone(h.ctx(), tenantA, "prod", CloneRequest{Name: "copy",
		Secrets: map[string]CloneSecret{"s4": {Copy: true}}})
	if err != nil {
		t.Fatalf("clone: %s", describe(err))
	}
	cp := h.env(st.Name)
	opened, err := bs.OpenCredential(h.ctx(), cp.ID, "s4", 1, "s4")
	if err != nil || opened.GetHeaders().GetHeaders()["Apikey"] != "key-2" {
		t.Fatalf("the copy holds %v %v", opened, err)
	}
	sp, err := h.svc.deploymentSpec(h.ctx(), cp, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ref := sp.Secrets["s4"]; !ref.IsHTTP() || ref.Version != 1 {
		t.Fatalf("the copy's pin: %+v", ref)
	}
}

// A new plan refuses bindings stated in the spec (ADR 0055); a stored
// deployment with them keeps running, takes a deployment that leaves them
// as they are, a revert to it and a clone of it.
func TestStatedBindingsRefused(t *testing.T) {
	h := newHarness(t, harnessOptions{bindings: &fakeHTTPBindings{}})
	h.create("shop", nil)
	h.putSecret("shop", "erp-key", "k")
	h.putSecret("shop", "pay-pass", "p")
	_, err := h.svc.Apply(h.ctx(), tenantA, "shop", ApplyRequest{Spec: json.RawMessage(shopSpec), Author: "tester"})
	for _, path := range []string{"bindings.erp", "bindings.pay"} {
		if f, ok := findingAt(err, path); !ok || f.Problem != spec.GivesWay ||
			!strings.Contains(f.Remedy, "make an HTTP secret") {
			t.Fatalf("a new plan's %s: %s", path, describe(err))
		}
	}
	ans, err := h.svc.PlanSpec(h.ctx(), tenantA, PlanSpecRequest{Spec: json.RawMessage(shopSpec)})
	if err != nil || ans.Valid {
		t.Fatalf("a spec without an environment: %+v %v", ans, err)
	}

	one := h.applyStored("shop", shopSpec, 0)
	more := strings.Replace(shopSpec, `"bindings": ["erp"]}`, `"bindings": ["erp"], "replicas": 2}`, 1)
	two := h.apply("shop", more, one.Number)
	changed := strings.Replace(more, "https://api.erp.example", "https://api2.erp.example", 1)
	_, err = h.svc.Apply(h.ctx(), tenantA, "shop", ApplyRequest{Spec: json.RawMessage(changed), Base: two.Number})
	if _, ok := findingAt(err, "bindings.erp"); !ok {
		t.Fatalf("a changed binding: %s", describe(err))
	}
	if _, ok := findingAt(err, "bindings.pay"); ok {
		t.Fatalf("an unchanged binding refused: %s", describe(err))
	}
	three := h.apply("shop", `{"services": {"web": {"image": "ghcr.io/acme/web:1"}}}`, two.Number)
	if _, err := h.svc.Revert(h.ctx(), tenantA, "shop", one.Number, RevertRequest{Base: three.Number,
		Author: "tester"}); err != nil {
		t.Fatalf("a revert to a deployment with bindings: %s", describe(err))
	}
	if _, err := h.svc.Clone(h.ctx(), tenantA, "shop", CloneRequest{Name: "copy", Deployment: &one.Number,
		Secrets: map[string]CloneSecret{"erp-key": {Copy: true}, "pay-pass": {Copy: true}}}); err != nil {
		t.Fatalf("a clone of it: %s", describe(err))
	}
}
