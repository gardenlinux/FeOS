// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// The bounds of a read of the traffic (traces.md, Reading: the traffic):
// the services of a read (at most 128), its steps, the paths left out per
// service, and the services searched at once.
const (
	maxTrafficServices = 128
	maxTrafficSteps    = 1440
	maxTrafficSkips    = 8
	trafficLoads       = 4
)

// The sources of a read of the traffic: the ingress's server spans, or
// the services' own (TrafficQuery.Source).
const (
	TrafficIngress = "ingress"
	TrafficApp     = "app"
)

// TrafficQuery is a read of the traffic: a tenant's environment and its
// services by name, the range [SinceNs, UntilNs) in Unix ns, the step,
// whole seconds, of which both ends are multiples, and the source:
// TrafficIngress (also when empty), what the ingress answered for each
// service, or TrafficApp, the requests each service's own server spans
// count, without those to its Skip paths (its readiness check).
type TrafficQuery struct {
	Tenant, Environment string
	Services            []string
	SinceNs, UntilNs    int64
	Step                time.Duration
	Source              string
	Skip                map[string][]string
}

// ServiceTraffic is a service's requests in a read's range: how many, how
// many failed (quickwit.Failed of the ingress's spans, quickwit.
// ServerFailed of a service's own), the 50th and 95th
// percentiles of their durations in ns (null without requests), and the
// steps that had requests, the oldest first.
type ServiceTraffic struct {
	Service  string        `json:"service"`
	Requests int64         `json:"requests"`
	Errors   int64         `json:"errors"`
	P50Ns    *float64      `json:"p50Ns"`
	P95Ns    *float64      `json:"p95Ns"`
	Steps    []TrafficStep `json:"steps"`
}

// TrafficStep is a step of a ServiceTraffic: its start in Unix ns, its
// requests, those that failed, and the 95th percentile of their
// durations in ns.
type TrafficStep struct {
	TsNs     int64    `json:"tsNs,string"`
	Requests int64    `json:"requests"`
	Errors   int64    `json:"errors"`
	P95Ns    *float64 `json:"p95Ns"`
}

// Traffic answers the traffic of q's services, in q's order, from server
// spans in the index: the ingress's (source ingress, the service as
// object), or with TrafficApp the services' own (source container, kind
// server, the service as object, which the node stamps). Per service two
// searches, those of failed requests first, so that as the index grows
// they never outnumber the requests counted after them. The index alone
// is read; a span is in it at most a commit (5 s) after it came.
func (t *Traces) Traffic(ctx context.Context, q TrafficQuery) ([]ServiceTraffic, error) {
	out := make([]ServiceTraffic, len(q.Services))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(trafficLoads)
	for i, svc := range q.Services {
		g.Go(func() error {
			st, err := t.serviceTraffic(gctx, q, svc)
			out[i] = st
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Traces) serviceTraffic(ctx context.Context, q TrafficQuery, service string) (ServiceTraffic, error) {
	sel := quickwit.SpanQuery{Tenant: q.Tenant, Environment: q.Environment, Object: service, Source: SourceIngress,
		SinceNs: q.SinceNs, UntilNs: q.UntilNs - 1, Failed: true}
	if q.Source == TrafficApp {
		sel = quickwit.SpanQuery{Tenant: q.Tenant, Environment: q.Environment, Object: service, Source: SourceContainer,
			Kind: quickwit.SpanKindServer, NotPaths: q.Skip[service], SinceNs: q.SinceNs, UntilNs: q.UntilNs - 1,
			ServerFailed: true}
	}
	failed, err := t.index.SpanHistogram(ctx, t.pattern(), sel, q.Step, false)
	if err != nil {
		return ServiceTraffic{}, fmt.Errorf("index: the failed requests of %s: %w", service, err)
	}
	sel.Failed, sel.ServerFailed = false, false
	all, err := t.index.SpanHistogram(ctx, t.pattern(), sel, q.Step, true)
	if err != nil {
		return ServiceTraffic{}, fmt.Errorf("index: the requests of %s: %w", service, err)
	}
	errs := make(map[int64]int64, len(failed.Buckets))
	for _, b := range failed.Buckets {
		errs[b.StartNs] = b.Count
	}
	st := ServiceTraffic{Service: service, Requests: all.Count, Errors: min(failed.Count, all.Count),
		P50Ns: all.P50Ns, P95Ns: all.P95Ns, Steps: make([]TrafficStep, 0, len(all.Buckets))}
	for _, b := range all.Buckets {
		st.Steps = append(st.Steps, TrafficStep{TsNs: b.StartNs, Requests: b.Count, Errors: min(errs[b.StartNs], b.Count),
			P95Ns: b.P95Ns})
	}
	return st, nil
}

// parseTrafficQuery reads the parameters of /api/v1/traffic: environment,
// service (repeated), since and until (RFC 3339 or Unix ns, multiples of
// step), step (a Go duration of whole seconds), source (ingress, the
// default, or app) and skip (repeated, "<service>:<path>", app only).
func parseTrafficQuery(v url.Values, tenant string) (TrafficQuery, error) {
	q := TrafficQuery{Tenant: tenant, Environment: v.Get("environment"), Source: v.Get("source")}
	if q.Environment == "" {
		return q, fmt.Errorf("environment is required")
	}
	switch q.Source {
	case "":
		q.Source = TrafficIngress
	case TrafficIngress, TrafficApp:
	default:
		return q, fmt.Errorf("source must be ingress or app")
	}
	for _, s := range v["skip"] {
		svc, path, ok := strings.Cut(s, ":")
		if !ok || svc == "" || !strings.HasPrefix(path, "/") {
			return q, fmt.Errorf("a skip must be <service>:<path>, the path starting with /")
		}
		if q.Source != TrafficApp {
			return q, fmt.Errorf("skip is of the source app only")
		}
		if q.Skip == nil {
			q.Skip = map[string][]string{}
		}
		if len(q.Skip[svc]) >= maxTrafficSkips {
			return q, fmt.Errorf("at most %d skips per service", maxTrafficSkips)
		}
		if !slices.Contains(q.Skip[svc], path) {
			q.Skip[svc] = append(q.Skip[svc], path)
		}
	}
	for _, s := range v["service"] {
		if s == "" {
			return q, fmt.Errorf("a service must not be empty")
		}
		if !slices.Contains(q.Services, s) {
			q.Services = append(q.Services, s)
		}
	}
	if len(q.Services) == 0 || len(q.Services) > maxTrafficServices {
		return q, fmt.Errorf("give 1 to %d services", maxTrafficServices)
	}
	step, err := time.ParseDuration(v.Get("step"))
	if err != nil || step < time.Second || step%time.Second != 0 {
		return q, fmt.Errorf("step must be a duration of whole seconds, such as 60s or 15m")
	}
	q.Step = step
	for _, p := range []struct {
		name string
		dst  *int64
	}{{"since", &q.SinceNs}, {"until", &q.UntilNs}} {
		s := v.Get(p.name)
		if s == "" {
			return q, fmt.Errorf("%s is required", p.name)
		}
		if *p.dst, err = parseTime(p.name, s); err != nil {
			return q, err
		}
		if *p.dst <= 0 || *p.dst%int64(step) != 0 {
			return q, fmt.Errorf("%s must be after the epoch and a multiple of step", p.name)
		}
	}
	if q.SinceNs >= q.UntilNs {
		return q, fmt.Errorf("since must be before until")
	}
	if (q.UntilNs-q.SinceNs)/int64(step) > maxTrafficSteps {
		return q, fmt.Errorf("at most %d steps", maxTrafficSteps)
	}
	return q, nil
}

func (a *api) traffic(w http.ResponseWriter, r *http.Request, acc Access) {
	q, err := parseTrafficQuery(r.URL.Query(), acc.Tenant)
	if err != nil {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "%s", err.Error()))
		return
	}
	if !acc.mayRead(q.Environment) {
		apierr.Write(w, apierr.New(http.StatusNotFound, "no such environment"))
		return
	}
	services, err := a.traces.Traffic(r.Context(), q)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Services []ServiceTraffic `json:"services"`
	}{services})
}
