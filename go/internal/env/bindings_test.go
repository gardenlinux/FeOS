// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/bindings"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// fakeHTTPBindings are the bindings as the plans and the check see them:
// on, a destination rule that refuses *.internal.example, a preflight that
// records what it was given.
type fakeHTTPBindings struct {
	mu   sync.Mutex
	last *fleetv1.HttpBinding
}

func (f *fakeHTTPBindings) OpenCredential(context.Context, string, string, int, string) (*fleetv1.HttpBinding, error) {
	return nil, errors.New("no credentials here")
}

func (f *fakeHTTPBindings) Enabled() bool { return true }
func (f *fakeHTTPBindings) CheckDestination(_ context.Context, host string) error {
	if strings.HasSuffix(host, ".internal.example") {
		return errors.New(host + " resolves to an internal address")
	}
	return nil
}
func (f *fakeHTTPBindings) PreflightHTTP(_ context.Context, _ string, b *fleetv1.HttpBinding, _ *bindings.HTTPProbe) (
	*bindings.HTTPPreflightResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = b
	return &bindings.HTTPPreflightResult{OK: true, Addresses: []string{"203.0.113.40"}}, nil
}

// shopSpec has an ERP system's API for web and a payment provider's,
// intercepted, for the worker alone.
const shopSpec = `{
  "services": {
    "web": {"image": "ghcr.io/acme/web:1", "bindings": ["erp"]},
    "worker": {"image": "ghcr.io/acme/worker:1", "bindings": ["pay"]}
  },
  "bindings": {
    "erp": {"upstream": "https://api.erp.example", "credential": {"kind": "headers",
      "headers": {"X-API-Key": {"secret": "erp-key"}}}},
    "pay": {"upstream": "https://api.pay.example", "mode": "intercept", "credential": {"kind": "basic",
      "user": "shop", "password": {"secret": "pay-pass"}}}
  }
}`

// podOf inserts an instance row of the environment's object at a
// deployment, its template naming bindings as the controller renders them.
func (h *harness) podOf(envID uuid.UUID, object string, deployment int, tb ...model.TemplateHTTPBinding) store.Sandbox {
	h.t.Helper()
	id := podid.Mint()
	row, _, err := h.st.CreateSandbox(h.ctx(), store.NewSandbox{ID: id, Owner: tenantA, Image: "img", VCPUs: 1,
		MemoryMiB: 512, EnvironmentID: envID, Instance: true, PodTemplate: &model.PodTemplate{HTTPBindings: tb}})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.d.Pool.Exec(h.ctx(), `INSERT INTO env_instances (sandbox_id, env_id, kind, object, template_hash,
		deployment) VALUES ($1, $2, 'service', $3, 'h', $4)`, id, envID, object, deployment); err != nil {
		h.t.Fatal(err)
	}
	return row
}

func (h *harness) envID(name string) uuid.UUID {
	h.t.Helper()
	var id uuid.UUID
	if err := h.d.Pool.QueryRow(h.ctx(), "SELECT id FROM environments WHERE name = $1", name).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

var (
	erpTemplate = model.TemplateHTTPBinding{Name: "erp", Upstream: "https://api.erp.example", Mode: "name"}
	payTemplate = model.TemplateHTTPBinding{Name: "pay", Upstream: "https://api.pay.example", Mode: "intercept"}
)

// A pod gets exactly the bindings its object names, sealed with their
// secrets at the versions its deployment pins; a new version reaches it
// with the deployment it moves to; the check takes the latest.
func TestEnvBindingsScoped(t *testing.T) {
	fb := &fakeHTTPBindings{}
	h := newHarness(t, harnessOptions{bindings: fb})
	h.create("shop", nil)
	h.putSecret("shop", "erp-key", "erp-key-v1")
	h.putSecret("shop", "pay-pass", "pay-pass-v1")
	h.apply("shop", shopSpec, 0)
	envID := h.envID("shop")
	web := h.podOf(envID, "web", 1, erpTemplate)
	worker := h.podOf(envID, "worker", 1, payTemplate)

	bs, v1, err := h.svc.RowHTTPBindings(h.ctx(), web, true)
	if err != nil || len(bs) != 1 || bs[0].GetName() != "erp" ||
		bs[0].GetHeaders().GetHeaders()["X-Api-Key"] != "erp-key-v1" || v1 == "" {
		t.Fatalf("web's bindings: %v %q %v", bs, v1, err)
	}
	if closed, v, err := h.svc.RowHTTPBindings(h.ctx(), web, false); err != nil || v != v1 ||
		closed[0].GetHeaders().GetHeaders()["X-Api-Key"] != "" {
		t.Fatalf("web's bindings unopened: %v %q %v", closed, v, err)
	}
	ws, wv1, err := h.svc.RowHTTPBindings(h.ctx(), worker, true)
	if err != nil || len(ws) != 1 || ws[0].GetName() != "pay" || ws[0].GetBasic().GetPassword() != "pay-pass-v1" ||
		ws[0].GetMode() != fleetv1.HttpBindingMode_HTTP_BINDING_MODE_INTERCEPT {
		t.Fatalf("worker's bindings: %v %v", ws, err)
	}

	// Through the bindings, as IssuePodCredentials seals them: web has
	// erp alone and no CA, the worker pay alone and its CA.
	bsvc, err := bindings.New(bindings.Config{Enabled: true, Key: make([]byte, 32), Log: dbtest.Logger(t)}, h.st, h.d)
	if err != nil {
		t.Fatal(err)
	}
	bsvc.SetEnvSource(h.svc)
	pb, pv, err := bsvc.PodBindings(h.ctx(), web.ID)
	if err != nil || len(pb.GetHttp()) != 1 || pb.GetHttp()[0].GetName() != "erp" || len(pb.GetInterceptCas()) != 0 ||
		len(pb.GetKube()) != 0 {
		t.Fatalf("web's pod bindings: %v %v", pb, err)
	}
	part, err := bsvc.PodHTTP(h.ctx(), web)
	if err != nil || part.GetVersion() != pv || !slices.Equal(part.GetNames(), []string{"erp"}) {
		t.Fatalf("web's clear part: %v (sealed %s) %v", part, pv, err)
	}
	wb, wpv, err := bsvc.PodBindings(h.ctx(), worker.ID)
	if err != nil || len(wb.GetHttp()) != 1 || wb.GetHttp()[0].GetName() != "pay" || len(wb.GetInterceptCas()) != 1 {
		t.Fatalf("worker's pod bindings: %v %v", wb, err)
	}
	wpart, err := bsvc.PodHTTP(h.ctx(), worker)
	if err != nil || wpart.GetVersion() != wpv || len(wpart.GetNames()) != 0 ||
		!slices.Equal(wpart.GetInterceptHosts(), []string{"api.pay.example"}) || len(wpart.GetInterceptCaPems()) != 1 {
		t.Fatalf("worker's clear part: %v %v", wpart, err)
	}

	// A new version of erp-key and a deployment that pins it: web's pod,
	// moved to it in place, gets the new value under a new version; the
	// worker's is as it was.
	h.putSecret("shop", "erp-key", "erp-key-v2")
	if a := h.apply("shop", shopSpec, 1); a.Number != 2 {
		t.Fatalf("the second deployment: %+v", a)
	}
	if _, err := h.d.Pool.Exec(h.ctx(), "UPDATE env_instances SET deployment = 2"); err != nil {
		t.Fatal(err)
	}
	bs, v2, err := h.svc.RowHTTPBindings(h.ctx(), web, true)
	if err != nil || bs[0].GetHeaders().GetHeaders()["X-Api-Key"] != "erp-key-v2" || v2 == v1 {
		t.Fatalf("web's bindings at deployment 2: %v %q %v", bs, v2, err)
	}
	if _, wv2, _ := h.svc.RowHTTPBindings(h.ctx(), worker, true); wv2 != wv1 {
		t.Fatal("the worker's bindings moved with a secret it does not name")
	}
	if _, pv2, _ := bsvc.PodBindings(h.ctx(), web.ID); pv2 == pv {
		t.Fatal("web's pod version did not move")
	}

	// A pod whose template has another upstream than its deployment (it is
	// being replaced) gets no binding of that name.
	old := h.podOf(envID, "web", 2, model.TemplateHTTPBinding{Name: "erp", Upstream: "https://old.erp.example",
		Mode: "name"})
	if bs, _, err := h.svc.RowHTTPBindings(h.ctx(), old, true); err != nil || len(bs) != 0 {
		t.Fatalf("a stale template: %v %v", bs, err)
	}

	// The check takes the latest secret version; a binding the latest
	// deployment lacks is 404.
	h.putSecret("shop", "erp-key", "erp-key-v3")
	res, err := h.svc.CheckBinding(h.ctx(), tenantA, "shop", "erp", BindingCheckRequest{})
	if err != nil || !res.OK || fb.last.GetHeaders().GetHeaders()["X-Api-Key"] != "erp-key-v3" {
		t.Fatalf("check: %+v %v", res, err)
	}
	if _, err := h.svc.CheckBinding(h.ctx(), tenantA, "shop", "crm", BindingCheckRequest{}); !hasCode(err, CodeNoBinding) {
		t.Fatalf("check of no binding: %v", err)
	}
	if _, err := h.svc.CheckBinding(h.ctx(), tenantB, "shop", "erp", BindingCheckRequest{}); err == nil {
		t.Fatal("another tenant's check")
	}
}

func hasCode(err error, code string) bool {
	var ae *apierr.Error
	return errors.As(err, &ae) && ae.Code == code
}

// The plans judge the bindings' hosts and refuse bindings where they are
// off.
func TestEnvBindingsPlan(t *testing.T) {
	h := newHarness(t, harnessOptions{bindings: &fakeHTTPBindings{}})
	h.create("shop", nil)
	h.putSecret("shop", "erp-key", "k")
	h.putSecret("shop", "pay-pass", "p")
	internal := strings.Replace(shopSpec, "https://api.erp.example", "https://erp.internal.example", 1)
	_, err := h.svc.Apply(h.ctx(), tenantA, "shop", ApplyRequest{Spec: []byte(internal), Author: "tester"})
	if !strings.Contains(describe(err), "bindings.erp.upstream: erp.internal.example resolves to an internal address") {
		t.Fatalf("an internal upstream: %s", describe(err))
	}

	off := newHarness(t, harnessOptions{})
	off.create("shop", nil)
	off.putSecret("shop", "erp-key", "k")
	off.putSecret("shop", "pay-pass", "p")
	_, err = off.svc.Apply(off.ctx(), tenantA, "shop", ApplyRequest{Spec: []byte(shopSpec), Author: "tester"})
	if !strings.Contains(describe(err), "HTTP bindings are not enabled") {
		t.Fatalf("bindings off: %s", describe(err))
	}
	if _, err := off.svc.CheckBinding(off.ctx(), tenantA, "shop", "erp", BindingCheckRequest{}); !hasCode(err, CodeBindingsOff) {
		t.Fatalf("check while off: %v", err)
	}
}

// invalidSpec has the worker call an ERP system's API by its name and a
// payment provider's, intercepted, with a header from pay-token.
const invalidSpec = `{
  "services": {"worker": {"image": "ghcr.io/acme/worker:1", "bindings": ["erp", "pay"]}},
  "bindings": {
    "erp": {"upstream": "https://api.erp.example", "credential": {"kind": "headers",
      "headers": {"X-API-Key": {"secret": "erp-key"}}}},
    "pay": {"upstream": "https://api.pay.example", "mode": "intercept", "credential": {"kind": "headers",
      "headers": {"Authorization": {"secret": "pay-token"}}}}
  }
}`

// A binding whose secret is no valid header value: the plan warns without
// the value, and the pod gets the binding without its credential whether
// its bindings are opened or not, so that its clear part and its sealed set
// name the same bindings under one version, and its CA stays.
func TestEnvBindingsInvalidValue(t *testing.T) {
	h := newHarness(t, harnessOptions{bindings: &fakeHTTPBindings{}})
	h.create("shop", nil)
	h.putSecret("shop", "erp-key", "erp-key-v1")
	h.putSecret("shop", "pay-token", "Bearer t0ken\r\nX-Injected: 1")

	plan, err := h.svc.Plan(h.ctx(), tenantA, "shop", PlanRequest{Spec: []byte(invalidSpec)})
	if err != nil || !plan.Valid {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	warned := false
	for _, f := range plan.Findings {
		if f.Path == "bindings.pay.credential" && f.Severity == "warning" {
			warned = true
			if strings.Contains(f.Problem, "t0ken") || !strings.Contains(f.Problem, "control character") {
				t.Fatalf("the warning: %s", f.Problem)
			}
		}
	}
	if !warned {
		t.Fatalf("no warning: %+v", plan.Findings)
	}
	h.apply("shop", invalidSpec, 0)
	worker := h.podOf(h.envID("shop"), "worker", 1, erpTemplate, payTemplate)

	open, v, err := h.svc.RowHTTPBindings(h.ctx(), worker, true)
	if err != nil || len(open) != 2 || open[0].GetName() != "erp" || open[1].GetName() != "pay" ||
		open[0].GetHeaders().GetHeaders()["X-Api-Key"] != "erp-key-v1" || open[1].GetCredential() != nil {
		t.Fatalf("opened: %v %v", open, err)
	}
	closed, cv, err := h.svc.RowHTTPBindings(h.ctx(), worker, false)
	if err != nil || len(closed) != 2 || cv != v {
		t.Fatalf("unopened: %v %q, opened %q: %v", closed, cv, v, err)
	}

	bsvc, err := bindings.New(bindings.Config{Enabled: true, Key: make([]byte, 32), Log: dbtest.Logger(t)}, h.st, h.d)
	if err != nil {
		t.Fatal(err)
	}
	bsvc.SetEnvSource(h.svc)
	for range 3 {
		part, err := bsvc.PodHTTP(h.ctx(), worker)
		if err != nil {
			t.Fatal(err)
		}
		pb, pv, err := bsvc.PodBindings(h.ctx(), worker.ID)
		if err != nil {
			t.Fatal(err)
		}
		if part.GetVersion() != pv || !slices.Equal(part.GetNames(), []string{"erp"}) ||
			!slices.Equal(part.GetInterceptHosts(), []string{"api.pay.example"}) || len(pb.GetHttp()) != 2 ||
			len(pb.GetInterceptCas()) != 1 || len(part.GetInterceptCaPems()) != 1 {
			t.Fatalf("clear part %v, sealed %v (%s)", part, pb, pv)
		}
	}
	var cas int
	if err := h.d.Pool.QueryRow(h.ctx(), "SELECT count(*) FROM pod_intercept_cas WHERE sandbox_id = $1",
		worker.ID).Scan(&cas); err != nil || cas != 1 {
		t.Fatalf("CAs: %d %v", cas, err)
	}
}
