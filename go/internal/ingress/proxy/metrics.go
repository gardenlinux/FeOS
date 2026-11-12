// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// requestClasses are the classes of answered requests (recorder.class).
var requestClasses = []string{"1xx", "2xx", "3xx", "4xx", "5xx", "upgraded", "aborted"}

// metrics are the proxy's counters.
type metrics struct {
	requestsMu sync.Mutex
	requests   map[string]uint64

	dialErrors     atomic.Uint64
	responseErrors atomic.Uint64
	refused        atomic.Uint64
	retries        atomic.Uint64
	lastResort     atomic.Uint64
	tooLarge       atomic.Uint64
	refusedClient  atomic.Uint64
	tooManyFields  atomic.Uint64
	trailers       atomic.Uint64

	heldTotal    atomic.Uint64
	holdTimeouts atomic.Uint64
	wakes        atomic.Uint64

	holdCalls         atomic.Uint64
	holdAdmitFailures atomic.Uint64
	holdRefusals      atomic.Uint64
	holdRenewFailures atomic.Uint64

	cookieDomains atomic.Uint64

	acmeAnswered atomic.Uint64
	acmeUnknown  atomic.Uint64

	waitErrors             atomic.Uint64
	rejectedInflightEnv    atomic.Uint64
	rejectedInflightTenant atomic.Uint64
	rejectedInflight       atomic.Uint64
	rejectedHeldEnv        atomic.Uint64
	rejectedHeldTenant     atomic.Uint64
	rejectedHeld           atomic.Uint64
	connsCut               atomic.Uint64

	visitSignIns     atomic.Uint64
	visitRequired    atomic.Uint64
	visitRejected    atomic.Uint64
	visitRedeemed    atomic.Uint64
	visitRenewed     atomic.Uint64
	visitEnded       atomic.Uint64
	visitFailed      atomic.Uint64
	visitUnavailable atomic.Uint64
}

func (m *metrics) request(class string) {
	m.requestsMu.Lock()
	if m.requests == nil {
		m.requests = map[string]uint64{}
	}
	m.requests[class]++
	m.requestsMu.Unlock()
}

// Sample is one value of a metric, with its labels as name, value pairs.
type Sample struct {
	Labels []string
	Value  float64
}

// WriteMetric writes one metric in the Prometheus text format: its HELP
// and TYPE lines (typ "counter" or "gauge"), then its samples.
func WriteMetric(w io.Writer, name, typ, help string, samples ...Sample) {
	var b strings.Builder
	b.WriteString("# HELP " + name + " " + strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(help) + "\n")
	b.WriteString("# TYPE " + name + " " + typ + "\n")
	for _, s := range samples {
		b.WriteString(name)
		if len(s.Labels) > 0 {
			b.WriteByte('{')
			for i := 0; i+1 < len(s.Labels); i += 2 {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(s.Labels[i] + `="` + labelValue(s.Labels[i+1]) + `"`)
			}
			b.WriteByte('}')
		}
		b.WriteString(" " + strconv.FormatFloat(s.Value, 'g', -1, 64) + "\n")
	}
	_, _ = io.WriteString(w, b.String())
}

func labelValue(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func counter(name, help string, v uint64) func(io.Writer) {
	return func(w io.Writer) { WriteMetric(w, name, "counter", help, Sample{Value: float64(v)}) }
}

// Metrics writes the proxy's metrics in the Prometheus text format. The
// program adds the agent's (control connection, handshakes) with
// WriteMetric.
func (p *Proxy) Metrics(w io.Writer) {
	bw := bufio.NewWriter(w)
	defer func() { _ = bw.Flush() }()
	m := &p.m

	m.requestsMu.Lock()
	reqs := maps.Clone(m.requests)
	m.requestsMu.Unlock()
	var samples []Sample
	for _, c := range requestClasses {
		samples = append(samples, Sample{Labels: []string{"class", c}, Value: float64(reqs[c])})
	}
	WriteMetric(bw, "ferun_ingress_requests_total", "counter",
		"Requests answered, by status class; upgraded: switched protocols; "+
			"aborted: the client went away or the response was cut off.",
		samples...)
	WriteMetric(bw, "ferun_ingress_upstream_errors_total", "counter",
		"Failed attempts to forward a request, by kind: the dial failed, no response (or a failed upgrade) came, "+
			"or no connection to the instance came free in time (wait).",
		Sample{Labels: []string{"kind", "dial"}, Value: float64(m.dialErrors.Load())},
		Sample{Labels: []string{"kind", "response"}, Value: float64(m.responseErrors.Load())},
		Sample{Labels: []string{"kind", "wait"}, Value: float64(m.waitErrors.Load())})
	WriteMetric(bw, "ferun_ingress_rejected_total", "counter",
		"Requests answered 503 because a limit was taken: the environment's or the tenant's share of "+
			"forwarded requests (inflight_env, inflight_tenant) or the instance's cap (inflight), the "+
			"environment's or the tenant's share of held requests (held_env, held_tenant) or the instance's "+
			"cap (held).",
		Sample{Labels: []string{"limit", "inflight_env"}, Value: float64(m.rejectedInflightEnv.Load())},
		Sample{Labels: []string{"limit", "inflight_tenant"}, Value: float64(m.rejectedInflightTenant.Load())},
		Sample{Labels: []string{"limit", "inflight"}, Value: float64(m.rejectedInflight.Load())},
		Sample{Labels: []string{"limit", "held_env"}, Value: float64(m.rejectedHeldEnv.Load())},
		Sample{Labels: []string{"limit", "held_tenant"}, Value: float64(m.rejectedHeldTenant.Load())},
		Sample{Labels: []string{"limit", "held"}, Value: float64(m.rejectedHeld.Load())})
	WriteMetric(bw, "ferun_ingress_headers_rejected_total", "counter",
		"Requests rejected for their headers, by reason: more than 100 fields (431), announced trailers (400).",
		Sample{Labels: []string{"reason", "fields"}, Value: float64(m.tooManyFields.Load())},
		Sample{Labels: []string{"reason", "trailers"}, Value: float64(m.trailers.Load())})
	for _, f := range []func(io.Writer){
		counter("ferun_ingress_upstream_refused_total",
			"Requests answered 502 because the instance refused the connection.", m.refused.Load()),
		counter("ferun_ingress_retries_total",
			"Requests sent once more to another endpoint after a failed attempt.", m.retries.Load()),
		counter("ferun_ingress_last_resort_total",
			"Picks of an endpoint on a node without a fresh handshake.", m.lastResort.Load()),
		counter("ferun_ingress_too_large_total",
			"Requests answered 413 because their body was too large.", m.tooLarge.Load()),
		counter("ferun_ingress_client_refused_total",
			"Requests answered 403 because their route does not admit the client's address.", m.refusedClient.Load()),
		counter("ferun_ingress_held_total",
			"Requests held while no endpoint of their service was usable.", m.heldTotal.Load()),
		counter("ferun_ingress_hold_timeouts_total",
			"Held requests answered 503 at their bound.", m.holdTimeouts.Load()),
		counter("ferun_ingress_wakes_total",
			"Wake requests handed to the agent.", m.wakes.Load()),
		counter("ferun_ingress_idle_hold_calls_total",
			"Hold calls to the control plane: admits and renewals.", m.holdCalls.Load()),
		counter("ferun_ingress_idle_hold_admit_failures_total",
			"Admits of an idle hold that failed; the request went on unheld.", m.holdAdmitFailures.Load()),
		counter("ferun_ingress_idle_hold_refusals_total",
			"Admits of an idle hold that the control plane refused.", m.holdRefusals.Load()),
		counter("ferun_ingress_idle_hold_renew_failures_total",
			"Renewals of the idle holds that failed.", m.holdRenewFailures.Load()),
		counter("ferun_ingress_cookie_domains_removed_total",
			"Domain attributes removed from Set-Cookie headers on hosts under the apps zone or the tenant zone.",
			m.cookieDomains.Load()),
		counter("ferun_ingress_upstream_connections_cut_total",
			"Open connections to instances closed because the table no longer routes to them.",
			m.connsCut.Load()),
		counter("ferun_ingress_visit_sign_ins_total",
			"Navigations of guarded environments without a visit sent to their provider to sign in.",
			m.visitSignIns.Load()),
		counter("ferun_ingress_visit_required_total",
			"Requests of guarded environments without a visit answered 401 (not navigations).",
			m.visitRequired.Load()),
		counter("ferun_ingress_visit_rejected_total",
			"Sign-ins whose provider did not admit the visitor (403 at /.ferun/auth).", m.visitRejected.Load()),
		counter("ferun_ingress_visit_redeemed_total",
			"Accepted codes redeemed for a visit at the control plane.", m.visitRedeemed.Load()),
		counter("ferun_ingress_visit_renewed_total",
			"Visits' tokens renewed at the control plane.", m.visitRenewed.Load()),
		counter("ferun_ingress_visit_ended_total",
			"Visits seen ending: at a renewal, or signed out on this instance.", m.visitEnded.Load()),
		counter("ferun_ingress_visit_failed_total",
			"Redeems and renewals that got no answer of the control plane.", m.visitFailed.Load()),
		counter("ferun_ingress_visit_unavailable_total",
			"Requests of guarded environments that cannot sign anyone in (no keys, no sign-in address, "+
				"an unknown access or no visit calls): never forwarded.", m.visitUnavailable.Load()),
	} {
		f(bw)
	}
	WriteMetric(bw, "ferun_ingress_held", "gauge", "Requests held now.",
		Sample{Value: float64(p.held.count())})
	WriteMetric(bw, "ferun_ingress_inflight", "gauge",
		"Requests forwarded now: waiting for a connection to an instance or for its answer, upgraded ones included.",
		Sample{Value: float64(p.inflight.count())})
	WriteMetric(bw, "ferun_ingress_idle_holds", "gauge", "Instances held validly at the control plane.",
		Sample{Value: float64(len(p.Held()))})
	WriteMetric(bw, "ferun_ingress_upstream_connections", "gauge", "Connections to instances, idle or not.",
		Sample{Value: float64(p.up.open())})

	v := p.cur.Load()
	p.mu.Lock()
	stateAt := p.stateAt
	p.mu.Unlock()
	if !stateAt.IsZero() {
		WriteMetric(bw, "ferun_ingress_table_age_seconds", "gauge",
			"Seconds since the agent last handed the proxy its table.",
			Sample{Value: time.Since(stateAt).Seconds()})
	}
	fresh := 0
	for _, n := range slices.Sorted(maps.Keys(v.fresh)) {
		if v.fresh[n] {
			fresh++
		}
	}
	WriteMetric(bw, "ferun_ingress_mesh_nodes", "gauge",
		"Nodes of the mesh by handshake: fresh (younger than 180 s) or stale.",
		Sample{Labels: []string{"handshake", "fresh"}, Value: float64(fresh)},
		Sample{Labels: []string{"handshake", "stale"}, Value: float64(len(v.fresh) - fresh)})
	dp := p.DataPath()
	WriteMetric(bw, "ferun_ingress_endpoints", "gauge", "Endpoints in the table.",
		Sample{Value: float64(dp.Endpoints)})
	WriteMetric(bw, "ferun_ingress_endpoint_nodes", "gauge",
		"Nodes with endpoints, by whether their handshake is fresh and their group version admits the ingress.",
		Sample{Labels: []string{"admitted", "true"}, Value: float64(len(dp.FreshAdmittedNodes))},
		Sample{Labels: []string{"admitted", "false"}, Value: float64(len(dp.EndpointNodes) - len(dp.FreshAdmittedNodes))})
	hosts, domains, challenges := 0, 0, 0
	if v.t != nil {
		hosts, domains, challenges = len(v.t.hosts), len(v.t.certs), len(v.t.challenges)
	}
	WriteMetric(bw, "ferun_ingress_hosts", "gauge", "Hosts in the table.", Sample{Value: float64(hosts)})
	WriteMetric(bw, "ferun_ingress_domain_certificates", "gauge",
		"Hosts in the table with a certificate of their own (domains); the others get the wildcard.",
		Sample{Value: float64(domains)})
	WriteMetric(bw, "ferun_ingress_acme_challenges", "gauge", "Pending ACME HTTP-01 challenges in the table.",
		Sample{Value: float64(challenges)})
	WriteMetric(bw, "ferun_ingress_acme_answers_total", "counter",
		"Requests under /.well-known/acme-challenge/ on port 80, by whether a challenge was answered.",
		Sample{Labels: []string{"result", "answered"}, Value: float64(m.acmeAnswered.Load())},
		Sample{Labels: []string{"result", "unknown"}, Value: float64(m.acmeUnknown.Load())})
}
