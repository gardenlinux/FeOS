// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/store"
)

func ptr[T any](v T) *T { return &v }

// A spec asks for its image services' replicas by their resources, with
// the defaults, and its addons by their plans; attached services ask for
// nothing (their sandbox counts).
func TestSpecUse(t *testing.T) {
	var sp spec.Spec
	if err := json.Unmarshal([]byte(`{"services": {
		"web": {"image": "nginx", "replicas": 3, "resources": {"vcpus": 2, "memoryMib": 1024}},
		"one": {"image": "nginx"},
		"off": {"image": "nginx", "replicas": 0},
		"dev": {"runtime": "attached", "sandbox": "x", "command": ["vite"]}},
		"addons": {"db": {"type": "postgres", "plan": "nope"}}}`), &sp); err != nil {
		t.Fatal(err)
	}
	v, m := SpecUse(&sp)
	if v != 3*2+1 || m != 3*1024+spec.DefaultMemoryMib {
		t.Fatalf("use %d vCPUs, %d MiB", v, m)
	}
	if v, m := SpecUse(nil); v != 0 || m != 0 {
		t.Fatalf("nil %d %d", v, m)
	}
}

// Check refuses the first resource that use and ask pass, with the limit
// and the use; what ask does not ask for passes, however far over.
func TestCheck(t *testing.T) {
	l := store.TenantLimits{Sandboxes: ptr(2), VCPUs: ptr(4), MemoryMiB: ptr(int64(1024))}
	if err := Check(l, Use{Sandboxes: 1, VCPUs: 2}, Use{Sandboxes: 1, VCPUs: 2, MemoryMiB: 512}); err != nil {
		t.Fatal(err)
	}
	err := Check(l, Use{Sandboxes: 1, VCPUs: 3}, Use{Sandboxes: 1, VCPUs: 2})
	var e *apierr.Error
	if !errors.As(err, &e) || e.Status != http.StatusUnprocessableEntity || e.Code != apierr.CodeQuotaExceeded ||
		e.Quota.Resource != VCPUs || e.Quota.Limit != 4 || e.Quota.Use != 3 {
		t.Fatalf("err %+v", err)
	}
	if err := Check(l, Use{Sandboxes: 9, MemoryMiB: 9999}, Use{Environments: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Check(store.TenantLimits{Environments: ptr(0)}, Use{}, Use{Environments: 1}); err == nil {
		t.Fatal("a limit of 0 let one through")
	}
}

// A deployment is an error finding when it brings a sum past the limit
// and asks for more than the current one.
func TestSpecFindings(t *testing.T) {
	l := store.TenantLimits{VCPUs: ptr(4)}
	small := &spec.Spec{Services: map[string]spec.Service{"a": {Image: "x", Replicas: ptr(2)}}}
	big := &spec.Spec{Services: map[string]spec.Service{"a": {Image: "x", Replicas: ptr(5)}}}
	if fs := SpecFindings(l, Use{VCPUs: 2}, nil, small); len(fs) != 0 {
		t.Fatalf("small %+v", fs)
	}
	fs := SpecFindings(l, Use{VCPUs: 2}, small, big)
	if len(fs) != 1 || fs[0].Severity != spec.SeverityError || !strings.Contains(fs[0].Problem, "brings the tenant to 7 of its limit 4") {
		t.Fatalf("big %+v", fs)
	}
	// Over already (a lowered limit): no more is fine, less too.
	if fs := SpecFindings(l, Use{VCPUs: 9}, big, big); len(fs) != 0 {
		t.Fatalf("unchanged %+v", fs)
	}
	if fs := SpecFindings(l, Use{VCPUs: 9}, big, small); len(fs) != 0 {
		t.Fatalf("smaller %+v", fs)
	}
}
