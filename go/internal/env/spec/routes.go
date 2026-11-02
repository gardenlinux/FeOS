// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"maps"
	"slices"
)

// IsAttached reports whether a service runs attached to a sandbox of the
// tenant rather than as replicas of an image, as the controller decides
// it: its runtime is attached, or it names neither a runtime nor an image.
// The ingress does not reach an attached service; the route door alone
// serves its routes.
func IsAttached(s Service) bool {
	switch {
	case s.Runtime != "":
		return s.Runtime == RuntimeAttached
	case s.Image != "":
		return false
	}
	return true
}

// RouteView is a route as the ingress's table carries it (ADR 0021).
type RouteView struct {
	// Name is the route's name in the spec.
	Name string
	// Path is the route's path as written, Prefix the path as the route
	// door matches it (RoutePrefix).
	Path, Prefix string
	Service      string
	Port         PortRef
	Strip        bool
	// Door: its target is an attached service (IsAttached), which the
	// route door alone serves.
	Door bool
}

// IngressRoutes returns the routes of sp by name, each with its prefix and
// whether only the route door serves it. A nil spec has none.
func IngressRoutes(sp *Spec) []RouteView {
	if sp == nil {
		return nil
	}
	var out []RouteView
	for _, name := range slices.Sorted(maps.Keys(sp.Routes)) {
		r := sp.Routes[name]
		target, ok := sp.Services[r.Service]
		out = append(out, RouteView{Name: name, Path: r.Path, Prefix: RoutePrefix(r.Path), Service: r.Service, Port: r.Port,
			Strip: r.Strip, Door: ok && IsAttached(target)})
	}
	return out
}

// RoutedPorts returns the port numbers that the routes of the routing
// specs (an environment's deployed and desired release) send the ingress
// to on a pod of the image service named service: for every route that
// targets service where it is an image service in that routing spec, the
// number of the route's port in own, the pod's own release, else in the
// routing spec. The numbers are ascending and unique, without 0 (a port
// neither resolves). Nil specs are allowed and give nothing.
func RoutedPorts(service string, own *Spec, routing ...*Spec) []int {
	var ownPorts []Port
	if own != nil {
		ownPorts = own.Services[service].Ports
	}
	seen := map[int]bool{}
	for _, sp := range routing {
		if sp == nil {
			continue
		}
		target, ok := sp.Services[service]
		if !ok || IsAttached(target) {
			continue
		}
		for _, r := range sp.Routes {
			if r.Service != service {
				continue
			}
			n, _ := PortNumber(ownPorts, r.Port)
			if n <= 0 {
				n, _ = PortNumber(target.Ports, r.Port)
			}
			if n > 0 {
				seen[n] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
