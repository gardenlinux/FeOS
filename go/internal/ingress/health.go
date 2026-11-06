// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ingress

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/ingress/agent"
	"github.com/gardenlinux/feos/go/internal/ingress/proxy"
)

// maxListedNodes is how many nodes a readiness reason names.
const maxListedNodes = 5

// agentState is what readiness takes from the agent and the program
// besides the data path.
type agentState struct {
	// hasState: the agent has a table, cached or streamed, and the proxy
	// routes by it.
	hasState bool
	// meshConfigured: the mesh device was configured.
	meshConfigured bool
	// draining: the program is shutting down.
	draining bool
	// removed: the control plane said the ingress was removed, for the
	// reason removedText.
	removed     bool
	removedText string
}

// readiness is the rule of /readyz (§7.6): the instance is ready when it
// is not draining, not removed, has a table and a configured mesh device,
// and its data path has no endpoints, or at least one node with
// endpoints that is fresh and admits it, and either every node with
// endpoints is so or 60 s passed since the first table of the process
// (firstStateAt). The reason says why it is not ready.
func readiness(a agentState, dp proxy.DataPath, firstStateAt, now time.Time) (bool, string) {
	switch {
	case a.draining:
		return false, "the ingress is shutting down"
	case a.removed:
		return false, "the control plane removed the ingress (" + a.removedText + "); it serves again after a join with a new token"
	case !a.hasState:
		return false, "no routing table yet: the control plane was not reached, and no table is cached"
	case !a.meshConfigured:
		return false, "the mesh device is not configured"
	case dp.Endpoints == 0:
		return true, ""
	case len(dp.FreshAdmittedNodes) == 0:
		return false, fmt.Sprintf("none of the %d nodes with endpoints has a fresh handshake and admits the ingress: %s",
			len(dp.EndpointNodes), listNodes(dp.EndpointNodes))
	case len(dp.FreshAdmittedNodes) >= len(dp.EndpointNodes):
		return true, ""
	case !firstStateAt.IsZero() && now.Sub(firstStateAt) >= readyWindow:
		return true, ""
	}
	var waiting []string
	for _, n := range dp.EndpointNodes {
		if !slices.Contains(dp.FreshAdmittedNodes, n) {
			waiting = append(waiting, n)
		}
	}
	return false, fmt.Sprintf("waiting, for up to %s after the first table, for nodes with endpoints to have a fresh handshake and admit the ingress: %s",
		readyWindow, listNodes(waiting))
}

// listNodes names the first nodes of a list and counts the rest.
func listNodes(nodes []string) string {
	if len(nodes) <= maxListedNodes {
		return strings.Join(nodes, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(nodes[:maxListedNodes], ", "), len(nodes)-maxListedNodes)
}

// Ready reports whether the instance is ready to take traffic, and why
// not (the body of a failing /readyz). A change of the verdict is
// logged.
func (p *Program) Ready() (bool, string) {
	removed, removedText := p.agent.IdentityRemoved()
	a := agentState{
		hasState:       p.agent.HasState() && p.proxy.Ready() == nil,
		meshConfigured: p.agent.MeshConfigured(),
		draining:       p.draining.Load(),
		removed:        removed,
		removedText:    removedText,
	}
	p.mu.Lock()
	first := p.firstStateAt
	p.mu.Unlock()
	ok, reason := readiness(a, p.proxy.DataPath(), first, p.now())

	p.mu.Lock()
	changed := !p.readyKnown || p.lastReady != ok
	p.readyKnown, p.lastReady = true, ok
	p.mu.Unlock()
	switch {
	case !changed:
	case ok:
		p.log.Info("ferun-ingress: ready")
	case a.draining:
		p.log.Info("ferun-ingress: not ready", "reason", reason)
	default:
		p.log.Warn("ferun-ingress: not ready", "reason", reason)
	}
	return ok, reason
}

// Status is what /statusz reports: the agent's status, with each mesh
// peer's freshness, and the program's and the data path's.
type Status struct {
	agent.Status
	Version  string `json:"version"`
	Ready    bool   `json:"ready"`
	Reason   string `json:"reason,omitempty"`
	Draining bool   `json:"draining"`
	// FirstStateAt is when the process got its first table, cached or
	// streamed.
	FirstStateAt time.Time `json:"firstStateAt,omitzero"`
	// TableAgeSeconds is the age of the table: since its last message,
	// or since the cache was written.
	TableAgeSeconds float64 `json:"tableAgeSeconds,omitempty"`
	// MeshPeers replaces the agent's list: the same peers, each with
	// whether its handshake is fresh.
	MeshPeers []MeshPeer `json:"meshPeers"`
	// Held are the instances the ingress holds at the control plane.
	Held     []string       `json:"held"`
	DataPath proxy.DataPath `json:"dataPath"`
	// TLSNotAfter and TLSNames describe the HTTPS certificate in use.
	TLSNotAfter time.Time `json:"tlsNotAfter,omitzero"`
	TLSNames    []string  `json:"tlsNames,omitempty"`
	// TenantZoneTLSNotAfter and TenantZoneTLSNames describe the tenant
	// zone's certificate in use, when there is one.
	TenantZoneTLSNotAfter time.Time `json:"tenantZoneTlsNotAfter,omitzero"`
	TenantZoneTLSNames    []string  `json:"tenantZoneTlsNames,omitempty"`
}

// MeshPeer is a node on the mesh device, and whether its last handshake
// is fresh (younger than 180 s).
type MeshPeer struct {
	agent.MeshPeer
	Fresh bool `json:"fresh"`
}

// Status returns the instance's status.
func (p *Program) Status() Status {
	ok, reason := p.Ready()
	now := p.now()
	as := p.agent.Status()
	st := Status{
		Status:    as,
		Version:   p.cfg.Version,
		Ready:     ok,
		Reason:    reason,
		Draining:  p.draining.Load(),
		MeshPeers: make([]MeshPeer, 0, len(as.MeshPeers)),
		Held:      p.proxy.Held(),
		DataPath:  p.proxy.DataPath(),
	}
	st.Status.MeshPeers = nil
	p.mu.Lock()
	st.FirstStateAt = p.firstStateAt
	p.mu.Unlock()
	if !as.TableAt.IsZero() {
		st.TableAgeSeconds = now.Sub(as.TableAt).Seconds()
	}
	for _, mp := range as.MeshPeers {
		st.MeshPeers = append(st.MeshPeers, MeshPeer{MeshPeer: mp, Fresh: isFresh(mp.LastHandshake, now)})
	}
	if st.Held == nil {
		st.Held = []string{}
	}
	if leaf := p.certs.Leaf(); leaf != nil {
		st.TLSNotAfter = leaf.NotAfter
		st.TLSNames = leaf.DNSNames
	}
	if p.tenantCerts != nil {
		if leaf := p.tenantCerts.Leaf(); leaf != nil {
			st.TenantZoneTLSNotAfter, st.TenantZoneTLSNames = leaf.NotAfter, leaf.DNSNames
		}
	}
	return st
}

// Metrics writes the proxy's metrics and the instance's own in the
// Prometheus text format.
func (p *Program) Metrics(w io.Writer) {
	bw := bufio.NewWriter(w)
	defer func() { _ = bw.Flush() }()
	p.proxy.Metrics(bw)
	ok, _ := p.Ready()
	as := p.agent.Status()
	now := p.now()
	gauge := func(name, help string, v float64) {
		proxy.WriteMetric(bw, name, "gauge", help, proxy.Sample{Value: v})
	}
	proxy.WriteMetric(bw, "ferun_ingress_info", "gauge", "The instance's name and version.",
		proxy.Sample{Labels: []string{"name", p.cfg.Name, "version", p.cfg.Version}, Value: 1})
	gauge("ferun_ingress_ready", "1 while /readyz succeeds.", boolValue(ok))
	gauge("ferun_ingress_draining", "1 while the instance shuts down.", boolValue(p.draining.Load()))
	gauge("ferun_ingress_control_connected", "1 while the instance holds a connection to the control plane.", boolValue(as.Connected))
	gauge("ferun_ingress_identity_removed", "1 when the control plane removed the ingress.", boolValue(as.IdentityRemoved))
	sent, dropped, pending := p.agent.AccessLogCounts()
	proxy.WriteMetric(bw, "ferun_ingress_access_log_sent_total", "counter",
		"Access log records the sink took.", proxy.Sample{Value: float64(sent)})
	proxy.WriteMetric(bw, "ferun_ingress_access_log_dropped_total", "counter",
		"Access log records dropped: no sink endpoint, the buffer was full, or the sink refused them.",
		proxy.Sample{Value: float64(dropped)})
	gauge("ferun_ingress_access_log_pending", "Access log records waiting to be sent.", float64(pending))
	spansSent, spansDropped := p.agent.SpanCounts()
	proxy.WriteMetric(bw, "ferun_ingress_spans_sent_total", "counter",
		"Server spans the sink took.", proxy.Sample{Value: float64(spansSent)})
	proxy.WriteMetric(bw, "ferun_ingress_spans_dropped_total", "counter",
		"Server spans dropped: the sink failed, refused or rejected them, or their record had no tenant.",
		proxy.Sample{Value: float64(spansDropped)})
	proxy.WriteMetric(bw, "ferun_ingress_control_refusals_total", "counter",
		"Connection attempts the control plane refused (PERMISSION_DENIED).", proxy.Sample{Value: float64(as.RefusalsTotal)})
	if !as.CertNotAfter.IsZero() {
		gauge("ferun_ingress_identity_not_after_seconds", "When the instance's certificate expires, in Unix seconds.",
			float64(as.CertNotAfter.Unix()))
	}
	if leaf := p.certs.Leaf(); leaf != nil {
		gauge("ferun_ingress_tls_not_after_seconds", "When the HTTPS certificate in use expires, in Unix seconds.",
			float64(leaf.NotAfter.Unix()))
	}
	if p.tenantCerts != nil {
		if leaf := p.tenantCerts.Leaf(); leaf != nil {
			gauge("ferun_ingress_tenant_zone_tls_not_after_seconds",
				"When the tenant zone's HTTPS certificate in use expires, in Unix seconds.", float64(leaf.NotAfter.Unix()))
		}
	}
	gauge("ferun_ingress_mesh_configured", "1 when the mesh device is configured.", boolValue(as.MeshConfigured))
	gauge("ferun_ingress_mesh_port", "The mesh device's UDP port.", float64(as.MeshPort))
	proxy.WriteMetric(bw, "ferun_ingress_mesh_repairs_total", "counter",
		"Repairs of the paths to nodes with endpoints that had no handshake for 180 s: a new port and their peers set anew.",
		proxy.Sample{Value: float64(as.MeshRepairs)})
	gauge("ferun_ingress_mesh_peers", "Peers of the mesh device that are nodes of the table.", float64(len(as.MeshPeers)))
	var ages []proxy.Sample
	for _, mp := range as.MeshPeers {
		if !mp.LastHandshake.IsZero() {
			ages = append(ages, proxy.Sample{Labels: []string{"node", mp.Node}, Value: now.Sub(mp.LastHandshake).Seconds()})
		}
	}
	proxy.WriteMetric(bw, "ferun_ingress_mesh_handshake_age_seconds", "gauge",
		"Seconds since the last WireGuard handshake with a node; a node without one is not listed.", ages...)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// healthHandler serves the health listener: /healthz, /readyz, /metrics
// and /statusz.
func (p *Program) healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ok, reason := p.Ready(); !ok {
			writeText(w, http.StatusServiceUnavailable, "not ready: "+reason)
			return
		}
		writeText(w, http.StatusOK, "ready")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		p.Metrics(w)
	})
	mux.HandleFunc("GET /statusz", func(w http.ResponseWriter, _ *http.Request) {
		b, err := json.MarshalIndent(p.Status(), "", "  ")
		if err != nil {
			writeText(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append(b, '\n'))
	})
	return mux
}

func writeText(w http.ResponseWriter, code int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, text+"\n")
}
