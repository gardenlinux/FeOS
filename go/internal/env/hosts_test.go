// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"slices"
	"testing"

	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/store"
)

// An environment's hosts are its served names, else its default host,
// and after them its wildcard domains, which retire no default host and
// are no route's URL (ADR 0045).
func TestHostsOfWildcards(t *testing.T) {
	e := store.Environment{Public: true, HostLabel: "web-acme"}
	for _, c := range []struct {
		zone    string
		domains []string
		want    []string
	}{
		{"apps.example", nil, []string{"web-acme.apps.example"}},
		{"apps.example", []string{"*.boxed.example.com"}, []string{"web-acme.apps.example", "*.boxed.example.com"}},
		{"apps.example", []string{"*.boxed.example.com", "example.com"}, []string{"example.com", "*.boxed.example.com"}},
		{"", []string{"*.boxed.example.com"}, []string{"*.boxed.example.com"}},
	} {
		if got := hostsOf(e, c.zone, c.domains); !slices.Equal(got, c.want) {
			t.Errorf("zone %q, domains %v: %v, want %v", c.zone, c.domains, got, c.want)
		}
	}
	sp := &spec.Spec{Services: map[string]spec.Service{"web": {Runtime: spec.RuntimeImage,
		Ports: []spec.Port{{Name: "http", Port: 8080}}}},
		Routes: map[string]spec.Route{"root": {Path: "/", Service: "web", Port: spec.PortRef{Name: "http"}}}}
	if rs := routesOf(sp, []string{"*.boxed.example.com"}); rs[0].URL != "" || rs[0].Door != DoorRoute {
		t.Fatalf("a wildcard alone %+v", rs[0])
	}
	if rs := routesOf(sp, []string{"*.boxed.example.com", "web-acme.apps.example"}); rs[0].URL != "https://web-acme.apps.example/" {
		t.Fatalf("a wildcard first %+v", rs[0])
	}
}
