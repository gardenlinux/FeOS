// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import "time"

// What an environment's workloads are and do: their identities (ADR
// 0036), their services' traffic and their HTTP bindings' use.

// Identities is GET /api/v1/environments/{env}/identities: who the
// environment's workloads are to the platform (ADR 0036), the SPIFFE ID
// their pods' certificates carry under the trust domain, and what the
// certificate each pod holds names. No key and no certificate is in it.
// The trust domain is "" on an instance without an issuer, and with it
// the issuer and every SPIFFE ID, OU and subject. Issuer is the issuing
// CA's subject (RFC 2253), which signs every pod's certificate.
type Identities struct {
	TrustDomain string             `json:"trustDomain"`
	Issuer      string             `json:"issuer"`
	Workloads   []WorkloadIdentity `json:"workloads"`
}

// WorkloadIdentity is a workload: an object of the latest deployment
// (kind service, job or addon, by its name), a sandbox of the environment
// (kind sandbox, by its ID), or an object only an earlier deployment has
// whose pods are still there; its SPIFFE ID (the certificates' one URI
// name), the OU of its pods' subjects (the ID its set carries on the
// nodes, on which a postgres addon maps a role), and its pods ([] for
// none). An attached service runs in its sandbox's pod: its ID, its OU
// and its pod are the sandbox's.
type WorkloadIdentity struct {
	Kind     string        `json:"kind"`
	Name     string        `json:"name"`
	SPIFFEID string        `json:"spiffeId"`
	OU       string        `json:"ou"`
	Pods     []PodIdentity `json:"pods"`
}

// PodIdentity is a pod of a workload: its ID (an instance's, as the
// status shows it, or the sandbox's), the subject its certificates carry
// (RFC 2253: CN=<trust domain>/<pod>,OU=<workload>), and
// of the newest certificate issued to it its serial, when it was issued
// and its end ("" before its first), and the node it was issued on (the
// pod's node before its first).
type PodIdentity struct {
	Pod      string `json:"pod"`
	Subject  string `json:"subject"`
	Serial   string `json:"serial"`
	IssuedAt string `json:"issuedAt"`
	NotAfter string `json:"notAfter"`
	Node     string `json:"node"`
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

// BindingUsage is GET /api/v1/environments/{env}/bindings/usage: what the
// environment's pods (its services' and jobs' and its sandboxes') sent
// through each HTTP binding in the window that ends now. Unavailable
// says, when the log store could not be asked, why the numbers are
// zeros; Truncated that the store left some requests uncounted.
type BindingUsage struct {
	Window      string       `json:"window"`
	Unavailable string       `json:"unavailable,omitempty"`
	Truncated   bool         `json:"truncated"`
	Bindings    []BindingUse `json:"bindings"`
}

// BindingUse is a binding's requests in the window: every one of them,
// those the upstream refused the credential of (401, 403), those that
// failed (429 or 5xx, or the upstream not reached: unreachable, tls),
// those that had no OAuth token (credential_unavailable) and those
// feos-authn refused itself (blocked: denied_path, rate_limited,
// unknown_binding, misdirected, …); the newest's start ("" for none),
// the newest problem (null for none), and the workloads that sent them,
// the most requests first.
type BindingUse struct {
	Name        string          `json:"name"`
	Requests    int64           `json:"requests"`
	Refused     int64           `json:"refused"`
	Failing     int64           `json:"failing"`
	TokenFailed int64           `json:"tokenFailed"`
	Blocked     int64           `json:"blocked"`
	LastAt      string          `json:"lastAt"`
	LastProblem *BindingProblem `json:"lastProblem"`
	Users       []BindingUser   `json:"users"`
}

// BindingProblem is a binding's request that was a problem: its start,
// the status the pod was answered (0: none), feos-authn's code and
// message of a refusal of its own ("" for the upstream's answer), and
// its trace.
type BindingProblem struct {
	At      string `json:"at"`
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"traceId"`
}

// BindingUser is a workload's requests through a binding: a service or a
// job by its name, a sandbox by its ID.
type BindingUser struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Requests int64  `json:"requests"`
}
