// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gardenlinux/feos/go/internal/store"
)

// A tenant a person makes has the default limits; an operator sets any
// tenant's; a member reads the use against them; a sandbox's create past
// a limit is 422 quota_exceeded, also when creates come at once.
func TestQuotas(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	e := w.e
	five, eight, mem := 5, 8, int64(16384)
	e.srv.cfg.TenantDefaults = store.TenantLimits{Sandboxes: &five, VCPUs: &eight, MemoryMiB: &mem}
	p := w.person(t, "p", false)
	resp := e.serve(as(p, "", "POST", "/api/v1/tenants", map[string]string{"name": "shop"}))
	expect(t, resp, http.StatusCreated, "")
	if got := decode[struct{ Limits limitsView }](t, resp).Limits; got.Sandboxes == nil || *got.Sandboxes != 5 ||
		got.VCPUs == nil || *got.VCPUs != 8 || got.Environments != nil {
		t.Fatalf("limits %+v", got)
	}
	expect(t, e.serve(as(p, "shop", "GET", "/api/v1/tenant/quota", nil)), http.StatusOK,
		`{"limits":{"sandboxes":5,"vcpus":8,"memoryMib":16384},"use":{"sandboxes":0,"environments":0,`+
			`"runningJobs":0,"vcpus":0,"memoryMib":0}}`+"\n")
	// Tenants operators make have none.
	expect(t, e.serve(byToken(e.tenantToken, "GET", "/api/v1/tenant/quota", nil)), http.StatusOK,
		`{"limits":{},"use":{"sandboxes":0,"environments":2,"runningJobs":0,"vcpus":0,"memoryMib":0}}`+"\n")

	// An operator sets a tenant's limits; null and absent are none.
	expect(t, e.do("PUT", "/api/v1/admin/tenants/shop/limits", adminAuth(), map[string]any{"sandboxes": 3,
		"vcpus": 4, "memoryMib": nil}), http.StatusOK, `{"name":"shop","limits":{"sandboxes":3,"vcpus":4}}`+"\n")
	expect(t, e.do("PUT", "/api/v1/admin/tenants/shop/limits", adminAuth(), map[string]any{"sandboxes": -1}),
		http.StatusBadRequest, errBody("sandboxes must be at least 0, or null for no limit"))
	expect(t, e.do("PUT", "/api/v1/admin/tenants/nope/limits", adminAuth(), map[string]any{}), http.StatusNotFound,
		errBody("unknown tenant nope"))
	expect(t, e.serve(as(p, "", "PUT", "/api/v1/admin/tenants/shop/limits", map[string]any{})), http.StatusForbidden,
		errBody("operators only"))

	create := func(body map[string]any) *httptest.ResponseRecorder {
		return e.serve(as(p, "shop", "POST", "/api/v1/sandboxes", body))
	}
	expect(t, create(map[string]any{"vcpus": 2}), http.StatusCreated, "")
	expect(t, create(map[string]any{"vcpus": 4}), http.StatusUnprocessableEntity,
		`{"error":"the tenant's limit of vcpus is 4, 2 are in use and this asks for 4 more",`+
			`"code":"quota_exceeded","resource":"vcpus","limit":4,"use":2}`+"\n")
	// At once: the limit of 3 sandboxes lets two more through.
	expect(t, e.do("PUT", "/api/v1/admin/tenants/shop/limits", adminAuth(), map[string]any{"sandboxes": 3}),
		http.StatusOK, "")
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = create(map[string]any{"vcpus": 1}).Code
		}()
	}
	wg.Wait()
	made := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			made++
		} else if c != http.StatusUnprocessableEntity {
			t.Fatalf("codes %v", codes)
		}
	}
	if made != 2 {
		t.Fatalf("codes %v", codes)
	}
	resp = e.serve(as(p, "shop", "GET", "/api/v1/tenant/quota", nil))
	if got := decode[struct {
		Use struct{ Sandboxes, VCPUs int }
	}](t, resp).Use; got.Sandboxes != 3 || got.VCPUs != 4 {
		t.Fatalf("use %+v", got)
	}
	// The admin list shows them.
	resp = e.do("GET", "/api/v1/admin/tenants", adminAuth(), nil)
	expect(t, resp, http.StatusOK, "")
	for _, tn := range decode[struct{ Tenants []tenantInfo }](t, resp).Tenants {
		if tn.Name == "shop" && (tn.MadeBy != w.persons["p"].String() || tn.Limits.Sandboxes == nil) {
			t.Fatalf("shop %+v", tn)
		}
	}
}
