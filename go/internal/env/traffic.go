// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/store"
)

// trafficWindow is a window of …/traffic: how far back from its end, and
// its step as the answer names it.
type trafficWindow struct {
	length, step time.Duration
	stepName     string
}

// trafficWindows are …/traffic's windows: the last hour in minutes (60
// steps) and the last day in quarter hours (96).
var trafficWindows = map[string]trafficWindow{
	"1h":  {time.Hour, time.Minute, "1m"},
	"24h": {24 * time.Hour, 15 * time.Minute, "15m"},
}

// ParseTrafficWindow reads …/traffic's window: 1h (also when empty) or
// 24h; another is a 400.
func ParseTrafficWindow(s string) (string, error) {
	if s == "" {
		return "1h", nil
	}
	if _, ok := trafficWindows[s]; !ok {
		return "", apierr.New(http.StatusBadRequest, "window must be 1h or 24h, got %q", s)
	}
	return s, nil
}

// Traffic is GET /api/v1/environments/{env}/traffic: the requests each
// of the environment's services answered in the window (FeCode ADR 0009
// §4), from the [Since, Until) that ends at now truncated to the step, in
// buckets of the step.
type Traffic struct {
	Window   string           `json:"window"`
	Since    time.Time        `json:"since"`
	Until    time.Time        `json:"until"`
	Step     string           `json:"step"`
	Services []ServiceTraffic `json:"services"`
}

// ServiceTraffic is a service's traffic in the window: where it is
// counted (Source), its requests, those that failed, the 50th and 95th
// percentiles of their durations in milliseconds to a tenth (null without
// requests), and every bucket of the window, the oldest first.
type ServiceTraffic struct {
	Service string `json:"service"`
	// Source is TrafficApp, the service's own server spans (a failed
	// request's status an error, or 500 to 599), or, for a routed service
	// that sends none, TrafficIngress, what the ingress answered for it (a
	// status of 500 or more, or none written unless the client went away).
	Source   string         `json:"source"`
	Requests int64          `json:"requests"`
	Errors   int64          `json:"errors"`
	P50Ms    *float64       `json:"p50Ms"`
	P95Ms    *float64       `json:"p95Ms"`
	Series   []TrafficPoint `json:"series"`
}

// TrafficPoint is a bucket [T, T+step) of a service's traffic; P95Ms is
// null in a bucket without requests.
type TrafficPoint struct {
	T        time.Time `json:"t"`
	Requests int64     `json:"requests"`
	Errors   int64     `json:"errors"`
	P95Ms    *float64  `json:"p95Ms"`
}

// The sources of a service's traffic (ServiceTraffic.Source), as the
// sink's traffic reads them (GET /api/v1/traffic?source=).
const (
	TrafficApp     = "app"
	TrafficIngress = "ingress"
)

// sinkTraffic is a service's traffic as the sink answers it
// (GET /api/v1/traffic): the steps with requests only, times in ns; the
// source is the read's.
type sinkTraffic struct {
	Source   string   `json:"-"`
	Service  string   `json:"service"`
	Requests int64    `json:"requests"`
	Errors   int64    `json:"errors"`
	P50Ns    *float64 `json:"p50Ns"`
	P95Ns    *float64 `json:"p95Ns"`
	Steps    []struct {
		TsNs     int64    `json:"tsNs,string"`
		Requests int64    `json:"requests"`
		Errors   int64    `json:"errors"`
		P95Ns    *float64 `json:"p95Ns"`
	} `json:"steps"`
}

// Traffic is GET /api/v1/environments/{env}/traffic?window=: the traffic
// of the image services of the environment's desired and deployed
// deployments, read from the sink with the caller's token or session
// (traces.md, Reading: the traffic): what each service's own server spans
// count, without those of its readiness check, which a request reaches
// whether it came through the ingress or from another service; for a
// routed service that sends no spans, what the ingress answered for it
// (an attached service's routes only the route door serves). As
// …/access-logs: without --sink-url or either 503 no_log_store, when the
// sink fails 503 log_store_unavailable.
func (s *Service) Traffic(ctx context.Context, tenant, env, window string) (Traffic, error) {
	w, ok := trafficWindows[window]
	if !ok {
		return Traffic{}, apierr.New(http.StatusBadRequest, "window must be 1h or 24h, got %q", window)
	}
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return Traffic{}, err
	}
	t, _ := auth.TenantFrom(ctx)
	switch {
	case s.d.SinkURL == "":
		return Traffic{}, apierr.WithCode(http.StatusServiceUnavailable, CodeNoLogStore,
			"the traffic is read from the sink, and this ferund has no --sink-url")
	case !t.Passable():
		return Traffic{}, apierr.WithCode(http.StatusServiceUnavailable, CodeNoLogStore,
			"the traffic is read from the sink with the caller's token or session, and the request has neither")
	}
	ts, err := s.trafficServices(ctx, e)
	if err != nil {
		return Traffic{}, err
	}
	since, until := trafficRange(time.Now(), w.length, w.step)
	out := Traffic{Window: window, Since: since, Until: until, Step: w.stepName, Services: []ServiceTraffic{}}
	if len(ts.services) == 0 {
		return out, nil
	}
	got, err := s.readSinkTraffic(ctx, t, e.ID.String(), TrafficApp, ts.services, ts.skip, since, until, w.step)
	if err == nil && len(ts.routed) > 0 {
		var ingress []sinkTraffic
		ingress, err = s.readSinkTraffic(ctx, t, e.ID.String(), TrafficIngress, ts.routed, nil, since, until, w.step)
		got = preferApp(got, ingress)
	}
	if err != nil {
		s.log.Warn("cannot read the traffic from the sink", "env", e.ID, "err", err)
		return Traffic{}, apierr.WithCode(http.StatusServiceUnavailable, CodeLogStoreUnavailable,
			"the log store cannot be read; try again shortly").RetryAfter(5)
	}
	out.Services = trafficOf(got, since, until, w.step)
	return out, nil
}

// trafficSources are what a read of an environment's traffic asks the
// sink: its image services (sorted), the paths of their readiness checks
// over HTTP, which are no traffic, and the services its routes send the
// ingress to (sorted).
type trafficSources struct {
	services, routed []string
	skip             map[string]string
}

// trafficServices are the traffic sources of e's desired and deployed
// deployments.
func (s *Service) trafficServices(ctx context.Context, e store.Environment) (trafficSources, error) {
	out := trafficSources{skip: map[string]string{}}
	for _, n := range []int{e.Deployment, e.Deployed} {
		if n <= 0 {
			continue
		}
		sp, err := s.deploymentSpec(ctx, e, n)
		if err != nil {
			return trafficSources{}, err
		}
		for name, svc := range sp.Services {
			if spec.IsAttached(svc) {
				continue
			}
			out.services = append(out.services, name)
			if r := svc.Readiness; r != nil && r.HTTP != nil && strings.HasPrefix(r.HTTP.Path, "/") {
				out.skip[name] = r.HTTP.Path
			}
		}
		for _, r := range spec.IngressRoutes(sp) {
			if !r.Door {
				out.routed = append(out.routed, r.Service)
			}
		}
	}
	slices.Sort(out.services)
	slices.Sort(out.routed)
	out.services, out.routed = slices.Compact(out.services), slices.Compact(out.routed)
	return out, nil
}

// preferApp is a service's own traffic where it has requests, else what
// the ingress answered for it.
func preferApp(app, ingress []sinkTraffic) []sinkTraffic {
	byName := map[string]sinkTraffic{}
	for _, g := range ingress {
		byName[g.Service] = g
	}
	out := make([]sinkTraffic, 0, len(app))
	for _, g := range app {
		if g.Requests == 0 {
			if in, ok := byName[g.Service]; ok && in.Requests > 0 {
				g = in
			}
		}
		out = append(out, g)
	}
	return out
}

// trafficRange is the window of length that ends at now truncated to step
// (a multiple of it since the epoch): [since, until).
func trafficRange(now time.Time, length, step time.Duration) (since, until time.Time) {
	ns := now.UnixNano()
	until = time.Unix(0, ns-ns%int64(step)).UTC()
	return until.Add(-length), until
}

// trafficOf is the sink's traffic of services as the answer has it: the
// services with requests, by name, each with every bucket of [since,
// until) step apart, zeros where the sink has none; durations in ms.
func trafficOf(got []sinkTraffic, since, until time.Time, step time.Duration) []ServiceTraffic {
	n := int(until.Sub(since) / step)
	out := []ServiceTraffic{}
	for _, g := range got {
		if g.Requests == 0 {
			continue
		}
		st := ServiceTraffic{Service: g.Service, Source: g.Source, Requests: g.Requests, Errors: g.Errors, P50Ms: nsToMs(g.P50Ns),
			P95Ms: nsToMs(g.P95Ns), Series: make([]TrafficPoint, n)}
		for i := range st.Series {
			st.Series[i].T = since.Add(time.Duration(i) * step)
		}
		for _, b := range g.Steps {
			i := (b.TsNs - since.UnixNano()) / int64(step)
			if i < 0 || i >= int64(n) {
				continue
			}
			p := &st.Series[i]
			p.Requests, p.Errors, p.P95Ms = b.Requests, b.Errors, nsToMs(b.P95Ns)
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b ServiceTraffic) int { return strings.Compare(a.Service, b.Service) })
	return out
}

// nsToMs is a duration in ns as ms to a tenth; nil stays nil.
func nsToMs(ns *float64) *float64 {
	if ns == nil || math.IsNaN(*ns) {
		return nil
	}
	ms := math.Round(*ns/1e5) / 10
	return &ms
}

// readSinkTraffic asks the sink for the traffic of services of the
// environment envID from source in [since, until) by step, as t, without
// the requests to a service's path in skip.
func (s *Service) readSinkTraffic(ctx context.Context, t auth.Tenant, envID, source string, services []string,
	skip map[string]string, since, until time.Time, step time.Duration) ([]sinkTraffic, error) {
	v := url.Values{"environment": {envID}, "service": services, "since": {strconv.FormatInt(since.UnixNano(), 10)},
		"until": {strconv.FormatInt(until.UnixNano(), 10)}, "step": {strconv.FormatInt(int64(step/time.Second), 10) + "s"},
		"source": {source}}
	for _, svc := range services {
		if p, ok := skip[svc]; ok {
			v.Add("skip", svc+":"+p)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(s.d.SinkURL, "/")+"/api/v1/traffic?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	t.PassOn(req.Header)
	resp, err := s.d.SinkClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("the sink answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		Services []sinkTraffic `json:"services"`
	}
	// A service's answer is some 10 KiB at 96 steps.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("the sink's answer: %w", err)
	}
	for i := range out.Services {
		out.Services[i].Source = source
	}
	return out.Services, nil
}
