// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"reflect"
	"slices"
	"testing"
)

func TestIsAttached(t *testing.T) {
	for _, c := range []struct {
		svc  Service
		want bool
	}{
		{Service{Runtime: RuntimeAttached, Sandbox: "x"}, true},
		{Service{Runtime: RuntimeImage, Image: "x"}, false},
		{Service{Image: "x"}, false},
		{Service{Sandbox: "x"}, true},
		{Service{}, true},
	} {
		if got := IsAttached(c.svc); got != c.want {
			t.Errorf("IsAttached(%+v) = %v", c.svc, got)
		}
	}
}

// The routes by name, with their prefixes; an attached target is the
// route door's alone, and a target the spec lacks is not.
func TestIngressRoutes(t *testing.T) {
	if got := IngressRoutes(nil); got != nil {
		t.Fatalf("nil spec: %v", got)
	}
	sp := &Spec{
		Services: map[string]Service{
			"api": {Runtime: RuntimeImage, Image: "x", Ports: []Port{{Name: "http", Port: 8080}}},
			"web": {Runtime: RuntimeAttached, Sandbox: "s", Ports: []Port{{Name: "http", Port: 5173}}},
		},
		Routes: map[string]Route{
			"root":    {Path: "/", Service: "web", Port: PortRef{Name: "http"}},
			"api":     {Path: "/api/", Service: "api", Port: PortRef{Number: 8080}, Strip: true},
			"missing": {Path: "/gone", Service: "nope", Port: PortRef{Name: "http"}},
		},
	}
	want := []RouteView{
		{Name: "api", Path: "/api/", Prefix: "/api", Service: "api", Port: PortRef{Number: 8080}, Strip: true},
		{Name: "missing", Path: "/gone", Prefix: "/gone", Service: "nope", Port: PortRef{Name: "http"}},
		{Name: "root", Path: "/", Prefix: "/", Service: "web", Port: PortRef{Name: "http"}, Door: true},
	}
	if got := IngressRoutes(sp); !reflect.DeepEqual(got, want) {
		t.Fatalf("routes\n got %+v\nwant %+v", got, want)
	}
}

func TestRoutedPorts(t *testing.T) {
	image := func(ports ...Port) Service { return Service{Runtime: RuntimeImage, Image: "x", Ports: ports} }
	http := func(n int) Port { return Port{Name: "http", Port: n} }
	admin := func(n int) Port { return Port{Name: "admin", Port: n} }

	// The pod's own release declares http 8080, the routing release 9090:
	// the own number wins. admin is only in the routing release: its
	// number there is the fallback.
	own := &Spec{Services: map[string]Service{"api": image(http(8080))}}
	routing := &Spec{
		Services: map[string]Service{"api": image(http(9090), admin(9100)), "web": {Runtime: RuntimeAttached, Sandbox: "s",
			Ports: []Port{http(5173)}}},
		Routes: map[string]Route{
			"api":   {Path: "/api", Service: "api", Port: PortRef{Name: "http"}},
			"admin": {Path: "/admin", Service: "api", Port: PortRef{Name: "admin"}},
			"again": {Path: "/v2", Service: "api", Port: PortRef{Number: 8080}},
			"root":  {Path: "/", Service: "web", Port: PortRef{Name: "http"}},
		},
	}
	if got := RoutedPorts("api", own, routing); !slices.Equal(got, []int{8080, 9100}) {
		t.Fatalf("own vs routing: %v", got)
	}
	// Without the own spec, the routing spec's numbers; a number the
	// routing spec does not declare resolves to nothing.
	if got := RoutedPorts("api", nil, routing); !slices.Equal(got, []int{9090, 9100}) {
		t.Fatalf("routing only: %v", got)
	}
	// An attached target is ignored, even when the own spec declares the
	// port.
	if got := RoutedPorts("web", &Spec{Services: map[string]Service{"web": image(http(80))}}, routing); len(got) != 0 {
		t.Fatalf("attached: %v", got)
	}
	// The union of the deployed and the desired release, sorted and
	// unique.
	desired := &Spec{
		Services: map[string]Service{"api": image(http(8080), Port{Name: "metrics", Port: 7000})},
		Routes: map[string]Route{
			"api":     {Path: "/api", Service: "api", Port: PortRef{Name: "http"}},
			"metrics": {Path: "/metrics", Service: "api", Port: PortRef{Name: "metrics"}},
		},
	}
	own = &Spec{Services: map[string]Service{"api": image(http(8080), Port{Name: "metrics", Port: 7000})}}
	if got := RoutedPorts("api", own, routing, desired); !slices.Equal(got, []int{7000, 8080, 9100}) {
		t.Fatalf("union: %v", got)
	}
	// An unknown service, and a routing spec that lacks the target.
	if got := RoutedPorts("nope", own, routing, desired); len(got) != 0 {
		t.Fatalf("unknown service: %v", got)
	}
	lacking := &Spec{Routes: map[string]Route{"api": {Path: "/api", Service: "api", Port: PortRef{Name: "http"}}}}
	if got := RoutedPorts("api", own, lacking); len(got) != 0 {
		t.Fatalf("target not in the routing spec: %v", got)
	}
	// Nil specs.
	if got := RoutedPorts("api", nil); len(got) != 0 {
		t.Fatalf("no routing spec: %v", got)
	}
	if got := RoutedPorts("api", nil, nil, nil); len(got) != 0 {
		t.Fatalf("nil routing specs: %v", got)
	}
	if got := RoutedPorts("api", own, nil, desired); !slices.Equal(got, []int{7000, 8080}) {
		t.Fatalf("a nil and a spec: %v", got)
	}
}
