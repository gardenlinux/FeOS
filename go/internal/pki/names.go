// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/podid"
)

// The SPIFFE layout (provisional; ADR 0014 leaves it open): a node is
// spiffe://<td>/node/<name>, an instance of ferun-ingress is
// spiffe://<td>/ingress/<name>, an instance of ferun-sink is
// spiffe://<td>/sink/<name>, the control plane's instances share
// spiffe://<td>/ferun; a pod has its workload's ID (WorkloadID, ADR
// 0036), a pod outside an environment spiffe://<td>/tenant/<tenant>/sandbox/<id>.
const (
	spiffeScheme    = "spiffe://"
	nodePath        = "/node/"
	ingressPath     = "/ingress/"
	sinkPath        = "/sink/"
	controlPlanePth = "/ferun"
	maxCNBytes      = 64
)

// NodeID is the SPIFFE ID of node name.
func NodeID(td, name string) string {
	return spiffeScheme + td + nodePath + name
}

// IngressID is the SPIFFE ID of the ferun-ingress instance name.
func IngressID(td, name string) string {
	return spiffeScheme + td + ingressPath + name
}

// SinkID is the SPIFFE ID of the ferun-sink instance name (ADR 0026).
func SinkID(td, name string) string {
	return spiffeScheme + td + sinkPath + name
}

// SandboxID is the SPIFFE ID of a sandbox's pod outside an environment,
// which only rows from before ADR 0035 are.
func SandboxID(td, tenant string, id podid.ID) string {
	return spiffeScheme + td + "/tenant/" + tenant + "/sandbox/" + id.String()
}

// validatePodSubject checks what IssuePod puts in a subject: a CN of at
// most 64 bytes (RFC 5280's bound), and a CN and an OU of DNS-name and
// pod-ID characters and "/", none of which RFC 2253 escapes, so that a
// map anchored on the printed subject holds.
func validatePodSubject(n pkix.Name) error {
	if len(n.CommonName) == 0 || len(n.CommonName) > maxCNBytes {
		return fmt.Errorf("subject CN %q is empty or longer than %d bytes", n.CommonName, maxCNBytes)
	}
	vals := append([]string{n.CommonName}, n.OrganizationalUnit...)
	for _, v := range vals {
		for i := 0; i < len(v); i++ {
			c := v[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '/') {
				return fmt.Errorf("subject value %q has a character outside [A-Za-z0-9./-]", v)
			}
		}
	}
	if len(n.OrganizationalUnit) > 1 {
		return fmt.Errorf("subject has %d OUs; a pod's has at most one", len(n.OrganizationalUnit))
	}
	return nil
}

// PodIdentity is what a pod's certificate names (ADR 0036): the SPIFFE
// ID of its workload, kind and name as store.Sandbox.Workload gives them,
// and the subject with the pod in the CN and the workload's set ID in the
// OU. A pod outside an environment (kind "") keeps SandboxID and a
// subject without an OU.
func PodIdentity(td, tenant string, envID uuid.UUID, pod podid.ID, kind, name, set string) (string, pkix.Name) {
	if kind == "" {
		return SandboxID(td, tenant, pod), PodSubject(td, pod.String(), "")
	}
	return WorkloadID(td, tenant, envID, kind, name), PodSubject(td, pod.String(), set)
}

// ControlPlaneID is the SPIFFE ID that every instance's certificate on the
// nodes' port carries, and no other certificate.
func ControlPlaneID(td string) string {
	return spiffeScheme + td + controlPlanePth
}

// Serial is a certificate's serial number as the database keeps it:
// lowercase hex without leading zeros.
func Serial(c *x509.Certificate) string {
	return c.SerialNumber.Text(16)
}

// ParseNodeID returns the node name of a node certificate: the certificate
// must carry exactly one URI name, spiffe://<td>/node/<name>, with name a
// DNS label.
func ParseNodeID(td string, cert *x509.Certificate) (string, error) {
	return parseNamedID(td, cert, nodePath, "a node", NodeID)
}

// ParseIngressID returns the instance name of a ferun-ingress certificate:
// the certificate must carry exactly one URI name,
// spiffe://<td>/ingress/<name>, with name a DNS label. A node's
// certificate never passes, nor an ingress's as a node's.
func ParseIngressID(td string, cert *x509.Certificate) (string, error) {
	return parseNamedID(td, cert, ingressPath, "an ingress", IngressID)
}

// ParseSinkID returns the instance name of a ferun-sink certificate: the
// certificate must carry exactly one URI name, spiffe://<td>/sink/<name>,
// with name a DNS label. Neither a node's nor an ingress's passes.
func ParseSinkID(td string, cert *x509.Certificate) (string, error) {
	return parseNamedID(td, cert, sinkPath, "a sink", SinkID)
}

// parseNamedID is ParseNodeID, ParseIngressID and ParseSinkID: the one URI name of cert
// must be spiffe://<td><path><name> in canonical form, with name a DNS
// label; holder names the holder in errors ("a node") and id rebuilds the
// URI.
func parseNamedID(td string, cert *x509.Certificate, path, holder string, id func(td, name string) string) (string, error) {
	if cert == nil {
		return "", errors.New("no certificate")
	}
	if len(cert.URIs) != 1 {
		return "", fmt.Errorf("certificate has %d URI names; %s's has exactly one", len(cert.URIs), holder)
	}
	u := cert.URIs[0]
	s := u.String()
	prefix := spiffeScheme + td + path
	if u.Scheme != "spiffe" || u.Host != td || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.HasPrefix(s, prefix) {
		return "", fmt.Errorf("URI name %q is not %s of trust domain %s", s, holder, td)
	}
	name := s[len(prefix):]
	if !isDNSLabel(name) || s != id(td, name) {
		return "", fmt.Errorf("URI name %q does not name %s", s, holder)
	}
	return name, nil
}

// ValidateTrustDomain checks a SPIFFE trust domain: a lowercase DNS name of
// labels [a-z0-9]([a-z0-9-]{0,61}[a-z0-9])? joined by dots, at most 255
// bytes, without a scheme or a port.
func ValidateTrustDomain(td string) error {
	if err := validateDomain(td); err != nil {
		return fmt.Errorf("trust domain: %w", err)
	}
	return nil
}

// validateDomain checks a lowercase DNS name as ValidateTrustDomain
// describes it; an IP address is refused although its labels would pass.
func validateDomain(s string) error {
	if s == "" {
		return errors.New("empty name")
	}
	if len(s) > 255 {
		return fmt.Errorf("%q is longer than 255 bytes", s)
	}
	if strings.Contains(s, "://") {
		return fmt.Errorf("%q has a scheme; give the name alone", s)
	}
	if strings.Contains(s, ":") {
		return fmt.Errorf("%q has a port; give the name alone", s)
	}
	if net.ParseIP(s) != nil {
		return fmt.Errorf("%q is an IP address, not a DNS name", s)
	}
	for _, l := range strings.Split(s, ".") {
		if !isDNSLabel(l) {
			return fmt.Errorf("%q is not a lowercase DNS name (label %q)", s, l)
		}
	}
	return nil
}

// isDNSLabel reports whether s is [a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?.
func isDNSLabel(s string) bool {
	if len(s) < 1 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}

// IsNodeName reports whether name is a valid node name: a DNS label, as
// fleet.proto defines it for JoinRequest.name.
func IsNodeName(name string) bool { return isDNSLabel(name) }

// within reports whether name equals domain or lies under it. A domain
// with a leading dot (a name constraint's form for "subdomains only")
// matches only names under it.
func within(name, domain string) bool {
	if strings.HasPrefix(domain, ".") {
		return strings.HasSuffix(name, domain)
	}
	return name == domain || strings.HasSuffix(name, "."+domain)
}

// validatePodID checks a pod's SPIFFE ID: spiffe://<td>/<path> in canonical
// form, path segments of letters, digits, ".", "-" and "_" (the SPIFFE ID
// rules), and outside the namespaces of the control plane (/ferun), of
// nodes (/node/...), of ferun-ingress (/ingress/...) and of ferun-sink
// (/sink/...), so that a pod's certificate can never pose as any of them.
func validatePodID(td, id string) error {
	if strings.ContainsAny(id, "%?#@") {
		return fmt.Errorf("SPIFFE ID %q is not canonical", id)
	}
	u, err := url.Parse(id)
	if err != nil {
		return fmt.Errorf("SPIFFE ID %q: %w", id, err)
	}
	if u.Scheme != "spiffe" || u.Host != td || u.Opaque != "" || u.User != nil {
		return fmt.Errorf("SPIFFE ID %q is not in trust domain %s", id, td)
	}
	if u.String() != id {
		return fmt.Errorf("SPIFFE ID %q is not canonical", id)
	}
	path := u.Path
	if path == "" || path == "/" || strings.HasSuffix(path, "/") {
		return fmt.Errorf("SPIFFE ID %q has no path", id)
	}
	for _, seg := range strings.Split(path[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("SPIFFE ID %q has an invalid path segment %q", id, seg)
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
				return fmt.Errorf("SPIFFE ID %q has an invalid character in %q", id, seg)
			}
		}
	}
	if path == controlPlanePth || strings.HasPrefix(path, controlPlanePth+"/") {
		return fmt.Errorf("SPIFFE ID %q is the control plane's; a pod never gets it", id)
	}
	if path == strings.TrimSuffix(nodePath, "/") || strings.HasPrefix(path, nodePath) {
		return fmt.Errorf("SPIFFE ID %q names a node; a pod never gets it", id)
	}
	if path == strings.TrimSuffix(ingressPath, "/") || strings.HasPrefix(path, ingressPath) {
		return fmt.Errorf("SPIFFE ID %q names an ingress; a pod never gets it", id)
	}
	if path == strings.TrimSuffix(sinkPath, "/") || strings.HasPrefix(path, sinkPath) {
		return fmt.Errorf("SPIFFE ID %q names a sink; a pod never gets it", id)
	}
	return nil
}
