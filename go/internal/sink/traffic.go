// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// The bounds of a read of the ingress's traffic (traces.md, Reading: the
// ingress's traffic): the services of a read (an environment's routes,
// at most 128), its steps, and the services searched at once.
const (
	maxTrafficServices = 128
	maxTrafficSteps    = 1440
	trafficLoads       = 4
)

// TrafficQuery is a read of the ingress's traffic: a tenant's environment
// and its services by name, the range [SinceNs, UntilNs) in Unix ns, and
// the step, whole seconds, of which both ends are multiples.
type TrafficQuery struct {
	Tenant, Environment string
	Services            []string
	SinceNs, UntilNs    int64
	Step                time.Duration
}

// ServiceTraffic is a service's requests through the ingress in a read's
// range: how many, how many failed (quickwit.Failed), the 50th and 95th
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

// Traffic answers the ingress's traffic of q's services, in q's order,
// from its server spans in the index (source ingress, the service as
// object): per service two searches, those of failed requests first, so
// that as the index grows they never outnumber the requests counted
// after them. The index alone is read; a span is in it at most a commit
// (5 s) after it came.
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
	failed, err := t.index.SpanHistogram(ctx, t.pattern(), sel, q.Step, false)
	if err != nil {
		return ServiceTraffic{}, fmt.Errorf("index: the failed requests of %s: %w", service, err)
	}
	sel.Failed = false
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
// step), step (a Go duration of whole seconds).
func parseTrafficQuery(v url.Values, tenant string) (TrafficQuery, error) {
	q := TrafficQuery{Tenant: tenant, Environment: v.Get("environment")}
	if q.Environment == "" {
		return q, fmt.Errorf("environment is required")
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

func (a *api) traffic(w http.ResponseWriter, r *http.Request, tenant string) {
	q, err := parseTrafficQuery(r.URL.Query(), tenant)
	if err != nil {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "%s", err.Error()))
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
