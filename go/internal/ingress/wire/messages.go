// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"slices"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
)

// The messages of IngressService. A []byte is coded as base64, a
// time.Time as RFC 3339 with nanoseconds; every JSON name is camelCase
// and fixed (see the package's compatibility rule).

// Certificate is an issued certificate as fleet.proto's Certificate
// carries it: the chain in DER without the root, the issued certificate
// first; the trust bundle's roots in DER; and when the holder renews it.
type Certificate struct {
	SpiffeID    string    `json:"spiffeId"`
	Chain       [][]byte  `json:"chain"`
	TrustBundle [][]byte  `json:"trustBundle"`
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	RenewAfter  time.Time `json:"renewAfter"`
}

// CertificateFromProto converts what pki's Issue functions return. A nil
// certificate, or a missing time, gives zero values.
func CertificateFromProto(c *fleetv1.Certificate) Certificate {
	if c == nil {
		return Certificate{}
	}
	return Certificate{
		SpiffeID:    c.GetSpiffeId(),
		Chain:       copyBytesList(c.GetChain()),
		TrustBundle: copyBytesList(c.GetTrustBundle()),
		NotBefore:   protoTime(c.GetNotBefore()),
		NotAfter:    protoTime(c.GetNotAfter()),
		RenewAfter:  protoTime(c.GetRenewAfter()),
	}
}

func protoTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// JoinRequest is an ingress's first call, without a client certificate:
// the one-time join token, the name it proposes (a DNS label; a token
// pinned to a name decides it) and a PKCS#10 request for its key in DER.
type JoinRequest struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	CSR   []byte `json:"csr"`
}

// JoinResponse names the ingress and carries its first certificate.
type JoinResponse struct {
	Name        string      `json:"name"`
	Certificate Certificate `json:"certificate"`
}

// RenewRequest asks for a new certificate for the key of CSR, under the
// certificate the connection presents.
type RenewRequest struct {
	CSR []byte `json:"csr"`
}

// RenewResponse carries the renewed certificate.
type RenewResponse struct {
	Certificate Certificate `json:"certificate"`
}

// HelloRequest is the first call on a connection with a certificate: the
// ingress's WireGuard public key (32 bytes), its WireGuard endpoint
// (host:port) and its version.
type HelloRequest struct {
	PublicKey []byte `json:"publicKey"`
	Endpoint  string `json:"endpoint"`
	Version   string `json:"version"`
}

// HelloResponse is the ingress's place on the pods' mesh: the cluster's
// ULA (PodNetwork), its /64 (Prefix) and its address in it, the zones of
// environments' default hosts (the apps zone, and the tenant zone of the
// tenants people make, ADR 0031; "": none) and the ID of the ferund
// instance that answered.
type HelloResponse struct {
	Name       string `json:"name"`
	PodNetwork string `json:"podNetwork"`
	Prefix     string `json:"prefix"`
	Address    string `json:"address"`
	AppsZone   string `json:"appsZone"`
	TenantZone string `json:"tenantZone,omitempty"`
	Instance   string `json:"instance"`
}

// SyncRequest opens the Sync stream. It has no fields yet.
type SyncRequest struct{}

// Table is one message of the Sync stream. The first message of a stream
// is whole: Envs is then every environment the ingress serves. Each
// later one is a delta: Envs are put by ID and Removed are deleted. Seq
// counts the messages of one stream from 1, so a gap shows a lost
// message. A message with nothing but Seq and SentAt is a heartbeat.
//
// Challenges, when not nil, are every pending ACME HTTP-01 answer, all
// of them: they replace the ingress's. In a delta nil keeps them; in a
// whole table nil means none (a control plane that sends none).
//
// A table larger than PartBytes goes in parts (Split): consecutive
// messages of the stream, each with its own Seq, every one but the last
// with More. The first part carries Whole, Mesh, Self and Challenges;
// Envs, then Removed, are spread over the parts. Nothing else comes
// between the parts, and a receiver applies them together once the last
// one has arrived, never a part alone (State.Apply).
type Table struct {
	Seq   uint64 `json:"seq"`
	Whole bool   `json:"whole,omitempty"`
	// More: the message goes on in the next part.
	More       bool        `json:"more,omitempty"`
	Envs       []Env       `json:"envs,omitempty"`
	Removed    []string    `json:"removed,omitempty"`
	Mesh       *Mesh       `json:"mesh,omitempty"`       // nil: unchanged
	Self       *Self       `json:"self,omitempty"`       // nil: unchanged
	Challenges *Challenges `json:"challenges,omitempty"` // nil: unchanged (in a delta)
	SentAt     time.Time   `json:"sentAt"`
}

// Challenges are the ACME HTTP-01 answers every ingress instance gives
// on port 80 while ferund's orders are pending, sorted by Token.
type Challenges struct {
	Items []Challenge `json:"items"`
}

// Challenge is one HTTP-01 answer: GET
// /.well-known/acme-challenge/<Token> on port 80 with the Host Host is
// answered 200 with KeyAuth as text/plain.
type Challenge struct {
	Token   string `json:"token"`
	Host    string `json:"host"`
	KeyAuth string `json:"keyAuth"`
}

// Mesh is the ingress's WireGuard peers: the nodes of the pods' mesh, at
// the mesh's peers version.
type Mesh struct {
	Version uint64 `json:"version"`
	Peers   []Peer `json:"peers"`
}

// Peer is one node on the pods' mesh: its WireGuard public key, its
// endpoint (host:port) and its /64.
type Peer struct {
	Name      string `json:"name"`
	PublicKey []byte `json:"publicKey"`
	Endpoint  string `json:"endpoint"`
	Prefix    string `json:"prefix"`
}

// Self is what the control plane holds of the ingress itself, with the
// zones of default hosts, under which the ingress makes cookies host-only.
// AdmittedVersion is the version of the security group ferun-ingress at
// which the ingress's address entered it: an endpoint whose node holds an
// older version does not admit the ingress yet.
type Self struct {
	Name            string `json:"name"`
	Address         string `json:"address"`
	AppsZone        string `json:"appsZone"`
	TenantZone      string `json:"tenantZone,omitempty"`
	AdmittedVersion uint64 `json:"admittedVersion"`
}

// Env is one public environment as the ingress routes it: its hosts, its
// routing deployment and the services its routes reach. Hash identifies the
// content, so that the control plane sends an environment again only
// when it changed.
//
// Domains are the hosts of the environment that have a certificate of
// their own (a tenant's domains, ADR 0021); each one's name is in Hosts
// too. The default host under the apps zone has none: it takes the
// ingress's wildcard.
type Env struct {
	ID     string   `json:"id"`
	Tenant string   `json:"tenant"`
	Name   string   `json:"name"`
	Hosts  []string `json:"hosts"`
	// Deployment is its routing deployment's number. The ingress does
	// not read it; it is in the hash, which so changed once with its key
	// (ADR 0018, "The deployments' word").
	Deployment int       `json:"deployment"`
	Hash       string    `json:"hash"`
	Routes     []Route   `json:"routes"`
	Services   []Service `json:"services"`
	Domains    []Domain  `json:"domains,omitempty"`
}

// Domain is a host served with a certificate of its own: its name, the
// certificate's chain in DER, the issued certificate first, and its
// private key, PKCS #8 DER.
type Domain struct {
	Name  string   `json:"name"`
	Chain [][]byte `json:"chain"`
	Key   []byte   `json:"key"`
}

// Route sends requests under Path to Service, at the port its endpoints
// list under Port (Endpoint.Ports). Strip removes Path from the request's
// path. Door marks a route that the route
// door of ferund serves alone (an attached service, which runs in a
// sandbox): the ingress does not serve it. From are the route's clients'
// address blocks, canonical prefixes of either family (ADR 0034,
// revised): the ingress answers a client whose address is in none of
// them 403. None: every client. An ingress older than the field ignores
// it.
type Route struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Service string   `json:"service"`
	Port    string   `json:"port"`
	Strip   bool     `json:"strip"`
	Door    bool     `json:"door"`
	From    []string `json:"from,omitempty"`
}

// Service is one routed service: the instances that answer, and the
// others counted by why they do not.
type Service struct {
	Name string `json:"name"`
	// Idle: the service has an idle policy, so the ingress holds the
	// instances it sends requests to.
	Idle bool `json:"idle"`
	// Endpoints: instances ready, running, without an operation, with an
	// address and a node, and with at least one admitted port.
	Endpoints []Endpoint `json:"endpoints"`
	// Asleep: instances paused, hibernated, pausing or hibernating, or
	// ready and running with an operation; Wake helps.
	Asleep int `json:"asleep"`
	// Waking: instances resuming, or with an operation resume or restore.
	Waking int `json:"waking"`
	// Pending: instances pending or creating, running but not ready, or
	// ready without an admitted port.
	Pending int `json:"pending"`
}

// Endpoint is one instance that answers: its pod address on the mesh,
// the node it runs on, its admitted ports under the keys Route.Port
// names, and the version of the security group ferun-ingress that its
// node holds (see Self.AdmittedVersion).
type Endpoint struct {
	Instance     string         `json:"instance"`
	Address      string         `json:"address"`
	Node         string         `json:"node"`
	Ports        map[string]int `json:"ports"`
	GroupVersion uint64         `json:"groupVersion"`
}

// WakeRequest asks the control plane to wake the asleep instances of a
// service of an environment (its ID).
type WakeRequest struct {
	Env     string `json:"env"`
	Service string `json:"service"`
}

// WakeResponse says how many instances are being woken and when to look
// again.
type WakeResponse struct {
	Waking            int `json:"waking"`
	RetryAfterSeconds int `json:"retryAfterSeconds"`
}

// MaxHoldIDs is the most instance IDs one list of a HoldRequest may hold;
// the control plane refuses more with INVALID_ARGUMENT.
const MaxHoldIDs = 10000

// HoldRequest holds instances (their IDs) the ingress sends requests to,
// so that the idle loop does not put them to sleep, and releases others.
// Leaving releases every hold of the ingress after Release: the ingress
// is shutting down.
type HoldRequest struct {
	Hold    []string `json:"hold"`
	Release []string `json:"release"`
	Leaving bool     `json:"leaving"`
}

// HoldRefusal is an instance the control plane did not hold, and why.
type HoldRefusal struct {
	Instance string `json:"instance"`
	Reason   string `json:"reason"`
}

// HoldResponse lists the instances held, those refused, and how long a
// hold lasts unless it is held again.
type HoldResponse struct {
	Held         []string      `json:"held"`
	Refused      []HoldRefusal `json:"refused"`
	LeaseSeconds int           `json:"leaseSeconds"`
}

func copyBytesList(in [][]byte) [][]byte {
	if in == nil {
		return nil
	}
	out := make([][]byte, len(in))
	for i, b := range in {
		out[i] = slices.Clone(b)
	}
	return out
}
