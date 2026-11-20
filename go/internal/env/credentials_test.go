// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/bindings"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

// credBindings is the bindings service as the environments use it, on
// the harness's database, every destination allowed.
type credBindings struct{ *bindings.Service }

func (credBindings) CheckDestination(context.Context, string) error { return nil }

// withCredentials gives the harness the bindings service and returns it.
func (h *harness) withCredentials() *bindings.Service {
	h.t.Helper()
	key := make([]byte, 32)
	key[0] = 7
	bs, err := bindings.New(bindings.Config{Enabled: true, Key: key, Log: dbtest.Logger(h.t)}, h.st, h.d)
	if err != nil {
		h.t.Fatal(err)
	}
	bs.SetCredentialUsers(h.svc)
	h.svc.d.Bindings = credBindings{bs}
	return bs
}

// putCredential writes a version of a credential of tenantA's environment
// env through its route's handler: headers Authorization token on an
// upstream that is an address on the internet.
func (h *harness) putCredential(bs *bindings.Service, env, name, token string) int {
	h.t.Helper()
	e := h.env(env)
	body := `{"upstream":"https://203.0.113.40","credential":{"kind":"headers","headers":{"Authorization":"` + token + `"}}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/environments/"+env+"/credentials/"+name, strings.NewReader(body))
	req = req.WithContext(auth.WithTenant(h.ctx(), auth.Tenant{Name: tenantA}))
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	bs.PutCredential(w, req, bindings.Env{ID: e.ID, Name: e.Name, Tenant: tenantA})
	if w.Code != http.StatusCreated {
		h.t.Fatalf("put credential %s of %s: %d %s", name, env, w.Code, w.Body.String())
	}
	var out struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		h.t.Fatal(err)
	}
	return out.Version
}

// paySpec has the service shop call the payment API by the environment's
// credential pay.
const paySpec = `{
  "services": {"shop": {"image": "ghcr.io/acme/shop:1", "bindings": ["pay"]}},
  "bindings": {"pay": {"credentialRef": {"name": "pay"}}}
}`

// findingAt is the error finding at path of err, an invalid spec's.
func findingAt(err error, path string) (spec.Finding, bool) {
	var ise *InvalidSpecError
	if !errors.As(err, &ise) {
		return spec.Finding{}, false
	}
	for _, f := range ise.Findings {
		if f.Severity == spec.SeverityError && f.Path == path {
			return f, true
		}
	}
	return spec.Finding{}, false
}

// A binding names a credential of its own environment: another
// environment's of the same name is none of it; its pods get the pinned
// version, and the credential's uses say so.
func TestCredentialRefIsTheEnvironments(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	bs := h.withCredentials()
	h.create("prod", nil)
	h.create("dev", nil)
	h.putCredential(bs, "prod", "pay", "Bearer prod-key-1")

	_, err := h.svc.Apply(h.ctx(), tenantA, "dev", ApplyRequest{Spec: json.RawMessage(paySpec), Author: "tester"})
	if f, ok := findingAt(err, "bindings.pay.credentialRef.name"); !ok ||
		!strings.Contains(f.Problem, "the environment has no credential pay") {
		t.Fatalf("dev named prod's credential: %s", describe(err))
	}
	// A spec without an environment has no credentials either.
	ans, err := h.svc.PlanSpec(h.ctx(), tenantA, PlanSpecRequest{Spec: json.RawMessage(paySpec)})
	if err != nil || ans.Valid {
		t.Fatalf("a spec without an environment: %+v %v", ans, err)
	}

	a := h.applyStored("prod", paySpec, 0)
	prod := h.env("prod")
	sp, err := h.svc.deploymentSpec(h.ctx(), prod, a.Number)
	if err != nil {
		t.Fatal(err)
	}
	if ref := sp.Bindings["pay"].CredentialRef; ref == nil || ref.Version != 1 || sp.Bindings["pay"].Upstream != "https://203.0.113.40" {
		t.Fatalf("the pin: %+v", sp.Bindings["pay"])
	}
	// A new version moves no pin: the pod gets version 1 until the next
	// deployment.
	h.putCredential(bs, "prod", "pay", "Bearer prod-key-2")
	pod := h.podOf(prod.ID, "shop", a.Number,
		model.TemplateHTTPBinding{Name: "pay", Upstream: "https://203.0.113.40", Mode: "name"})
	got, _, err := h.svc.RowHTTPBindings(h.ctx(), pod, true)
	if err != nil || len(got) != 1 || got[0].GetHeaders().GetHeaders()["Authorization"] != "Bearer prod-key-1" {
		t.Fatalf("the pod's bindings: %v %v", got, err)
	}
	uses, err := h.svc.CredentialUses(h.ctx(), h.st, prod.ID)
	if u := uses["pay"]; err != nil || len(u.Bindings) != 1 || u.Bindings[0] != "pay" || u.Latest != 1 {
		t.Fatalf("uses: %+v %v", uses, err)
	}
	if uses, err := h.svc.CredentialUses(h.ctx(), h.st, h.env("dev").ID); err != nil || len(uses) != 0 {
		t.Fatalf("dev's uses: %+v %v", uses, err)
	}
}

// A clone copies the credentials it is asked to, as version 1 of its
// own, and its spec pins those; one its spec names and it does not copy
// is the clone's finding, once.
func TestCloneCopiesCredentials(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	bs := h.withCredentials()
	h.create("prod", nil)
	h.putCredential(bs, "prod", "pay", "Bearer prod-key-1")
	h.applyStored("prod", paySpec, 0)
	h.putCredential(bs, "prod", "pay", "Bearer prod-key-2")

	_, err := h.svc.Clone(h.ctx(), tenantA, "prod", CloneRequest{Name: "copy"})
	f, ok := findingAt(err, "bindings.pay.credentialRef.name")
	if !ok || !strings.Contains(f.Remedy, `credentials: {"pay": {"copy": true}}`) {
		t.Fatalf("a clone without the credential: %s", describe(err))
	}
	var ise *InvalidSpecError
	if errors.As(err, &ise) {
		n := 0
		for _, f := range ise.Findings {
			if f.Path == "bindings.pay.credentialRef.name" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("the finding %d times: %s", n, describe(err))
		}
	}
	_, err = h.svc.Clone(h.ctx(), tenantA, "prod", CloneRequest{Name: "copy",
		Credentials: map[string]CloneCredential{"pay": {Copy: true}, "nope": {Copy: true}}})
	if _, ok := findingAt(err, "credentials.nope"); !ok {
		t.Fatalf("a credential the source lacks: %s", describe(err))
	}

	st, err := h.svc.Clone(h.ctx(), tenantA, "prod", CloneRequest{Name: "copy",
		Credentials: map[string]CloneCredential{"pay": {Copy: true}}})
	if err != nil {
		t.Fatalf("clone: %s", describe(err))
	}
	cp := h.env(st.Name)
	c, err := h.st.EnvCredential(h.ctx(), cp.ID, "pay", 0)
	if err != nil || c.Version != 1 {
		t.Fatalf("the copy's credential: %+v %v", c, err)
	}
	opened, err := bs.OpenCredential(h.ctx(), cp.ID, "pay", 1, "pay")
	if err != nil || opened.GetHeaders().GetHeaders()["Authorization"] != "Bearer prod-key-2" {
		t.Fatalf("the copy holds %v %v", opened, err)
	}
	sp, err := h.svc.deploymentSpec(h.ctx(), cp, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ref := sp.Bindings["pay"].CredentialRef; ref == nil || ref.Version != 1 {
		t.Fatalf("the copy's pin: %+v", sp.Bindings["pay"])
	}
}

// An apply pins no HTTP secret that is gone: one deleted between its
// plan and its transaction, which takes the secret's lock as a delete
// does, makes it plan again and find the variable's HTTP secret missing.
func TestApplyPinsNoDeletedCredential(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.withCredentials()
	h.create("prod", nil)
	h.putHTTPSecret("prod", "s4", "key-1", "")
	prod := h.env("prod")
	h.svc.afterPlan = func() {
		h.svc.afterPlan = nil
		if ok, err := h.st.DeleteEnvCredential(h.ctx(), prod.ID, "s4"); err != nil || !ok {
			t.Errorf("delete: %v %v", ok, err)
		}
	}
	_, err := h.svc.Apply(h.ctx(), tenantA, "prod", ApplyRequest{Spec: json.RawMessage(s4Spec), Author: "tester"})
	if f, ok := findingAt(err, "services.shop.env.S4_URL"); !ok || !strings.Contains(f.Problem, "no HTTP secret s4") {
		t.Fatalf("an apply of an HTTP secret deleted after its plan: %s", describe(err))
	}
	if e := h.env("prod"); e.Deployment != 0 {
		t.Fatalf("deployment %d was made", e.Deployment)
	}
}

// A revert to a deployment that pinned version 1 of a credential deleted
// since finds no version 1 once the credential is made again (it is
// version 2): it is refused, rather than deployed with another secret
// under the old number.
func TestRevertFindsNoReusedCredentialVersion(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	bs := h.withCredentials()
	h.create("prod", nil)
	h.putCredential(bs, "prod", "pay", "Bearer prod-key-1")
	one := h.applyStored("prod", paySpec, 0)
	two := h.apply("prod", `{"services": {"shop": {"image": "ghcr.io/acme/shop:1"}}}`, one.Number)
	prod := h.env("prod")
	if ok, err := h.st.DeleteEnvCredential(h.ctx(), prod.ID, "pay"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if v := h.putCredential(bs, "prod", "pay", "Bearer someone-elses-key"); v != 2 {
		t.Fatalf("made again as version %d, want 2", v)
	}
	_, err := h.svc.Revert(h.ctx(), tenantA, "prod", one.Number, RevertRequest{Base: two.Number, Author: "tester"})
	var ise *InvalidSpecError
	if !errors.As(err, &ise) || !strings.Contains(describe(err), "bindings.pay.credentialRef") {
		t.Fatalf("a revert onto a credential made again: %s", describe(err))
	}
	if e := h.env("prod"); e.Deployment != two.Number {
		t.Fatalf("deployment %d is the latest, want %d", e.Deployment, two.Number)
	}
}
