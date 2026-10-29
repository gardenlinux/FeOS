// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
)

// An image service's login (ADR 0025): its template keeps the secret's
// name, its pods are asked for with the secret's latest value, and a
// rotated secret reaches the next replica without a new template.
func TestImageCredentials(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	env := h.newEnv("stage")
	h.putSecret(env.ID, "pull-token", "first")
	var mu sync.Mutex
	var asked []model.RegistryCredentials
	h.eng.SetImageAccess(func(_ context.Context, _ string, auth model.RegistryCredentials) error {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, auth)
		return nil
	})
	last := func() model.RegistryCredentials {
		mu.Lock()
		defer mu.Unlock()
		if len(asked) == 0 {
			return model.RegistryCredentials{}
		}
		return asked[len(asked)-1]
	}
	creds := `,"imageCredentials":{"username":"project","password":{"secret":"pull-token"}}`

	r1 := h.apply(env.ID, serviceSpec(1, "1", creds))
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 30*time.Second)
	rows := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)
	want := &model.TemplateCredentials{Username: model.TemplateValue{Value: "project"},
		Password: model.TemplateValue{Secret: "pull-token"}}
	if got := rows[0].Sandbox.PodTemplate.ImageCredentials; !reflect.DeepEqual(got, want) {
		t.Fatalf("template credentials %+v", got)
	}
	if got := last(); got != (model.RegistryCredentials{Username: "project", Password: "first"}) {
		t.Fatalf("asked with %+v", got)
	}

	h.putSecret(env.ID, "pull-token", "second")
	r2 := h.apply(env.ID, serviceSpec(2, "1", creds))
	h.waitRelease(env.ID, r2, model.ReleaseRealised, 30*time.Second)
	rows = h.waitRows(env.ID, spec.KindService, "web", 2, 10*time.Second, "ready", ready)
	if rows[0].Instance.TemplateHash != rows[1].Instance.TemplateHash {
		t.Fatal("a rotated secret made another template")
	}
	if got := last(); got != (model.RegistryCredentials{Username: "project", Password: "second"}) {
		t.Fatalf("the new replica asked with %+v", got)
	}
}
