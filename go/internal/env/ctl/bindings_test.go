// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// deploymentPart is the clear part of a pod's HTTP bindings as its
// instance's deployment has it: its version is the deployment's number,
// as a binding whose secret moves with each deployment would have it.
type deploymentPart struct{ st *store.Store }

func (d deploymentPart) PodHTTP(ctx context.Context, row store.Sandbox) (*podv1.PodHttpBindings, error) {
	if row.PodTemplate == nil || len(row.PodTemplate.HTTPBindings) == 0 {
		return nil, nil
	}
	n, err := d.st.EnvInstanceDeployment(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	return &podv1.PodHttpBindings{Names: []string{row.PodTemplate.HTTPBindings[0].Name}, Version: "d" + strconv.Itoa(n)}, nil
}

// bindingSpec is a stored spec whose web service names binding erp, its
// secret pinned at version.
func bindingSpec(version int, upstream string) string {
	return fmt.Sprintf(`{"services":{"web":{"image":%q,"command":["/app"],"replicas":1,"bindings":["erp"],
		"ports":[{"name":"http","port":8080}]}},
		"bindings":{"erp":{"upstream":%q,"mode":"name","credential":{"kind":"headers",
			"headers":{"X-API-Key":{"secret":"erp-key"}}}}},
		"secrets":{"erp-key":{"version":%d}}}`, testImage, upstream, version)
}

// A service's pods name its bindings in their template; a deployment that
// only pins another secret version keeps them and sends them their new
// bindings, while another upstream replaces them.
func TestServiceBindings(t *testing.T) {
	h := newHarness(t, options{})
	h.addNode("n1")
	h.eng.SetHTTPBindings(deploymentPart{h.st})
	env := h.newEnv("stage")
	h.putSecret(env.ID, "erp-key", "one")
	h.putSecret(env.ID, "erp-key", "two")

	r1 := h.apply(env.ID, bindingSpec(1, "https://api.erp.example"))
	h.waitDeployment(env.ID, r1, model.DeploymentDeployed, 30*time.Second)
	rows := h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "ready", ready)
	want := []model.TemplateHTTPBinding{{Name: "erp", Upstream: "https://api.erp.example", Mode: "name"}}
	if got := rows[0].Sandbox.PodTemplate.HTTPBindings; !reflect.DeepEqual(got, want) {
		t.Fatalf("the template's bindings: %+v", got)
	}
	first := rows[0].Sandbox.ID
	if got := h.pod("n1", first).GetSpec().GetHttpBindings().GetVersion(); got != "d1" {
		t.Fatalf("the create's part: %q", got)
	}

	// Another secret version: the same pod, its part sent again.
	r2 := h.apply(env.ID, bindingSpec(2, "https://api.erp.example"))
	h.waitDeployment(env.ID, r2, model.DeploymentDeployed, 30*time.Second)
	rows = h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "kept", ready)
	if rows[0].Sandbox.ID != first {
		t.Fatal("a new secret version replaced the pod")
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.pod("n1", first).GetSpec().GetHttpBindings().GetVersion() != "d2" {
		if time.Now().After(deadline) {
			t.Fatalf("the kept pod's part: %v", h.pod("n1", first).GetSpec().GetHttpBindings())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Another upstream: another template, another pod.
	r3 := h.apply(env.ID, bindingSpec(2, "https://api2.erp.example"))
	h.waitDeployment(env.ID, r3, model.DeploymentDeployed, 30*time.Second)
	rows = h.waitRows(env.ID, spec.KindService, "web", 1, 10*time.Second, "replaced", func(r store.EnvInstanceRow) bool {
		return ready(r) && r.Sandbox.ID != first
	})
	if got := rows[0].Sandbox.PodTemplate.HTTPBindings[0].Upstream; got != "https://api2.erp.example" {
		t.Fatalf("the new pod's upstream: %s", got)
	}
}
