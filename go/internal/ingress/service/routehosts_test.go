// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/store"
)

// hostSpec is webSpec with a second route that answers on git.example.com
// alone (ADR 0045).
const hostSpec = `{"services": {"web": {"runtime": "image", "image": "web@sha256:1",
	"ports": [{"name": "http", "port": 8080}]}}, "routes": {"root": {"path": "/", "service": "web", "port": "http"},
	"git": {"host": "Git.Example.com", "path": "/", "service": "web", "port": "http"}}}`

// An ingress whose Hello does not name routeHosts never gets an
// environment with a route that has a host, as it would serve every route
// on every host; one that names it gets the route's host, canonical. An
// environment whose deployment gains a host route leaves the older one's
// table.
func TestSyncRouteHosts(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.world()
	h.meshNode("n1", 9)
	plain := h.mkEnv("plain", webSpec, true)
	hosted := h.mkEnv("hosted", hostSpec, true)
	later := h.mkEnv("later", webSpec, true)

	_, old, _ := h.connect("in-old", 1, "10.0.0.1:51820", wire.CapabilityVisits)
	_, cur, _ := h.connect("in-new", 2, "10.0.0.2:51820", wire.CapabilityVisits, wire.CapabilityRouteHosts)
	so, sn := h.sync(old), h.sync(cur)
	if first := so.next(10 * time.Second); !first.Whole || len(first.Envs) != 2 || so.state.Envs[hosted.ID.String()].ID != "" {
		t.Fatalf("old ingress, first table: %+v", so.state.Envs)
	}
	if first := sn.next(10 * time.Second); !first.Whole || len(first.Envs) != 3 {
		t.Fatalf("new ingress, first table: %d envs", len(first.Envs))
	}
	e := sn.state.Envs[hosted.ID.String()]
	if len(e.Routes) != 2 || e.Routes[0].Name != "git" || e.Routes[0].Host != "git.example.com" || e.Routes[1].Host != "" {
		t.Fatalf("new ingress, the hosted environment's routes %+v", e.Routes)
	}
	if r := sn.state.Envs[plain.ID.String()].Routes; len(r) != 1 || r[0].Host != "" {
		t.Fatalf("new ingress, the plain environment's routes %+v", r)
	}

	// later's deployment 2 has a host route: removed for the old ingress,
	// put with it for the new one.
	if _, err := h.st.InsertDeployment(bg, store.EnvDeployment{EnvID: later.ID, Number: 2, Spec: json.RawMessage(hostSpec),
		Origin: model.OriginApply}); err != nil {
		t.Fatal(err)
	}
	after := later
	after.Deployment = 2
	if _, err := h.st.UpdateEnvironment(bg, later, after); err != nil {
		t.Fatal(err)
	}
	gone := so.until(2*time.Second, "later removed", func(t *wire.Table) bool { return len(t.Removed) == 1 })
	if gone.Removed[0] != later.ID.String() || len(so.state.Envs) != 1 {
		t.Fatalf("old ingress: %+v, state %d envs", gone, len(so.state.Envs))
	}
	sn.until(2*time.Second, "later with hosts", func(*wire.Table) bool {
		r := sn.state.Envs[later.ID.String()].Routes
		return len(r) == 2 && r[0].Host == "git.example.com"
	})
}
