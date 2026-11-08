// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
)

// backdate is how far before its issuance a certificate becomes valid:
// nodes may boot before their clock is set.
const backdate = 10 * time.Minute

// Issuer is the platform CA's intermediate, the one issuing key of ADR
// 0014 that every instance holds. It issues node, ingress, pod and server
// certificates.
type Issuer struct {
	td        string
	cert      *x509.Certificate
	key       crypto.Signer
	root      *x509.Certificate // the root of the bundle that anchors cert
	bundle    *x509.CertPool
	bundleDER [][]byte
	cpDomain  string
	pin       string
}

// LoadIssuer reads the issuer from dir (issuer.crt, issuer.key, bundle.crt,
// issuer.json) for trust domain td. It refuses a dir that holds root.key,
// an issuer that does not verify to a root of the bundle, an issuer whose
// URI name constraints do not permit td, an issuer.json that names another
// trust domain, and a control-plane domain that does not lie within the
// issuer's permitted DNS domains.
func LoadIssuer(dir, trustDomain string) (*Issuer, error) {
	if err := ValidateTrustDomain(trustDomain); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(dir, RootKeyFile)); err == nil {
		return nil, fmt.Errorf("%s holds %s: the root key stays offline, never in the CA directory of an instance", dir, RootKeyFile)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	certs, err := readCerts(filepath.Join(dir, IssuerCertFile))
	if err != nil {
		return nil, err
	}
	cert := certs[0]
	if !cert.IsCA || !cert.BasicConstraintsValid {
		return nil, fmt.Errorf("%s is not a CA certificate", IssuerCertFile)
	}
	key, err := readKey(filepath.Join(dir, IssuerKeyFile))
	if err != nil {
		return nil, err
	}
	if !publicKeysEqual(key.Public(), cert.PublicKey) {
		return nil, fmt.Errorf("%s does not belong to %s", IssuerKeyFile, IssuerCertFile)
	}
	roots, err := readCerts(filepath.Join(dir, BundleFile))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	bundleDER := make([][]byte, 0, len(roots))
	for _, r := range roots {
		pool.AddCert(r)
		bundleDER = append(bundleDER, r.Raw)
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	chains, err := cert.Verify(x509.VerifyOptions{
		Roots:         pool,
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	if err != nil {
		return nil, fmt.Errorf("%s does not verify to a root of %s: %w", IssuerCertFile, BundleFile, err)
	}
	chain := chains[0]
	root := chain[len(chain)-1]
	for _, c := range chain[1:] {
		if !uriDomainPermitted(c, trustDomain) {
			return nil, fmt.Errorf("the name constraints of %q do not permit trust domain %s", c.Subject.CommonName, trustDomain)
		}
	}
	if !uriDomainPermitted(cert, trustDomain) {
		return nil, fmt.Errorf("the issuer's name constraints do not permit trust domain %s", trustDomain)
	}

	raw, err := os.ReadFile(filepath.Join(dir, IssuerJSONFile))
	if err != nil {
		return nil, err
	}
	var meta issuerJSON
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("%s: %w", IssuerJSONFile, err)
	}
	if meta.TrustDomain != "" && meta.TrustDomain != trustDomain {
		return nil, fmt.Errorf("%s names trust domain %s, not %s", IssuerJSONFile, meta.TrustDomain, trustDomain)
	}
	cp := meta.ControlPlaneDomain
	if err := validateDomain(cp); err != nil {
		return nil, fmt.Errorf("%s: control-plane domain: %w", IssuerJSONFile, err)
	}
	if !dnsPermitted(cert, cp) {
		return nil, fmt.Errorf("control-plane domain %s lies outside the issuer's permitted DNS domains %v", cp, cert.PermittedDNSDomains)
	}

	sum := sha256.Sum256(root.RawSubjectPublicKeyInfo)
	return &Issuer{
		td:        trustDomain,
		cert:      cert,
		key:       key,
		root:      root,
		bundle:    pool,
		bundleDER: bundleDER,
		cpDomain:  cp,
		pin:       hex.EncodeToString(sum[:]),
	}, nil
}

// TrustDomain is the SPIFFE trust domain the issuer issues for.
func (i *Issuer) TrustDomain() string { return i.td }

// ConstrainsURIs reports whether the issuer has URI name constraints, as
// `ferund init-ca` gave them before ADR 0024. feosd's TLS (rustls-webpki)
// refuses every URI name beneath one, so a node of this version refuses
// the nodes' port's certificate; `ferund new-issuer` replaces the issuer.
func (i *Issuer) ConstrainsURIs() bool {
	return len(i.cert.PermittedURIDomains) > 0 || len(i.cert.ExcludedURIDomains) > 0
}

// Bundle is the trust bundle: the roots of bundle.crt.
func (i *Issuer) Bundle() *x509.CertPool { return i.bundle }

// BundleDER is the trust bundle in DER, as Certificate.trust_bundle carries
// it.
func (i *Issuer) BundleDER() [][]byte { return append([][]byte(nil), i.bundleDER...) }

// ControlPlaneDomain is the domain of the instances' names on the nodes'
// port, from issuer.json.
func (i *Issuer) ControlPlaneDomain() string { return i.cpDomain }

// JoinPin is the lowercase hex SHA-256 of the DER SubjectPublicKeyInfo of
// the root that anchors the issuer: the root at the end of the chain on
// the nodes' port, which a joining node checks against its token.
func (i *Issuer) JoinPin() string { return i.pin }

// Certificate returns the issuer's own certificate.
func (i *Issuer) Certificate() *x509.Certificate { return i.cert }

// Root returns the root of the bundle that anchors the issuer.
func (i *Issuer) Root() *x509.Certificate { return i.root }

// IssueNode issues a node's client certificate for the key of csrDER:
// ClientAuth, the URI spiffe://<td>/node/<name> and no DNS name, subject CN
// name, valid for ttl. The CSR's own subject and names are ignored. The
// key is Ed25519 or ECDSA P-256. The node renews it at half its lifetime,
// so that a node switched off at any time keeps half of it (3.5 days of
// the default 7: a weekend) before it has to join again.
func (i *Issuer) IssueNode(csrDER []byte, name string, ttl time.Duration) (*fleetv1.Certificate, *x509.Certificate, error) {
	if !isDNSLabel(name) {
		return nil, nil, fmt.Errorf("node name %q is not a DNS label", name)
	}
	csr, _, err := CheckCSR(csrDER, true)
	if err != nil {
		return nil, nil, err
	}
	id := NodeID(i.td, name)
	u, err := url.Parse(id)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := i.sign(&x509.Certificate{
		Subject:     pkix.Name{CommonName: name},
		URIs:        []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, csr.PublicKey, ttl)
	if err != nil {
		return nil, nil, err
	}
	return i.certificateProto(id, leaf, leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore)/2)), leaf, nil
}

// IssueIngress issues the client certificate of the ferun-ingress instance
// name for the key of csrDER: ClientAuth, the URI
// spiffe://<td>/ingress/<name> and no DNS name, subject CN name, valid for
// ttl. The CSR's own subject and names are ignored. The key is Ed25519 or
// ECDSA P-256. Like a node, the instance renews it at half its lifetime.
func (i *Issuer) IssueIngress(csrDER []byte, name string, ttl time.Duration) (*fleetv1.Certificate, *x509.Certificate, error) {
	if !isDNSLabel(name) {
		return nil, nil, fmt.Errorf("ingress name %q is not a DNS label", name)
	}
	csr, _, err := CheckCSR(csrDER, true)
	if err != nil {
		return nil, nil, err
	}
	id := IngressID(i.td, name)
	u, err := url.Parse(id)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := i.sign(&x509.Certificate{
		Subject:     pkix.Name{CommonName: name},
		URIs:        []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, csr.PublicKey, ttl)
	if err != nil {
		return nil, nil, err
	}
	return i.certificateProto(id, leaf, leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore)/2)), leaf, nil
}

// IssuePod issues a pod's certificate (ADR 0014) for the Ed25519 key of
// csrDER: ClientAuth and ServerAuth, the URI spiffeID (its workload's,
// WorkloadID) and the DNS names given, the subject given (PodSubject: the
// pod in the CN, its workload in the OU, ADR 0036), valid for ttl. It refuses the
// control plane's SPIFFE ID and every ID in the node and ingress
// namespaces, any wildcard name, any name equal to or under the
// control-plane domain and any name outside the issuer's permitted DNS
// domains. The CSR's own
// subject and names are ignored. It is renewed at three quarters of its
// lifetime.
func (i *Issuer) IssuePod(csrDER []byte, spiffeID string, subject pkix.Name, dnsNames []string, ttl time.Duration) (*fleetv1.Certificate, *x509.Certificate, error) {
	if err := validatePodID(i.td, spiffeID); err != nil {
		return nil, nil, err
	}
	if err := validatePodSubject(subject); err != nil {
		return nil, nil, err
	}
	for _, n := range dnsNames {
		if strings.Contains(n, "*") {
			return nil, nil, fmt.Errorf("DNS name %q is a wildcard; a pod never gets one", n)
		}
		if err := validateDomain(n); err != nil {
			return nil, nil, fmt.Errorf("DNS name: %w", err)
		}
		if within(n, i.cpDomain) {
			return nil, nil, fmt.Errorf("DNS name %q lies in the control-plane domain %s; a pod never gets it", n, i.cpDomain)
		}
		if !dnsPermitted(i.cert, n) {
			return nil, nil, fmt.Errorf("DNS name %q lies outside the platform's DNS domains %v", n, i.cert.PermittedDNSDomains)
		}
	}
	csr, _, err := CheckCSR(csrDER, false)
	if err != nil {
		return nil, nil, err
	}
	u, err := url.Parse(spiffeID)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := i.sign(&x509.Certificate{
		Subject:     subject,
		URIs:        []*url.URL{u},
		DNSNames:    append([]string(nil), dnsNames...),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}, csr.PublicKey, ttl)
	if err != nil {
		return nil, nil, err
	}
	return i.certificateProto(spiffeID, leaf, leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore)/4*3)), leaf, nil
}

// IssueAddonServer issues an addon's server certificate (ADR 0038) for
// the public key pub, which the addon's own credential holds: the one URI
// spiffeID (validated as a pod's), no DNS or IP name, the subject of a pod
// certificate (PodSubject: pod and workload), ServerAuth, valid for ttl;
// with self also ClientAuth, for an addon whose provisioning reaches it
// as its own ID over TLS (valkey, ADR 0039). The chain is leaf and
// issuer, PEM; the leaf is returned too, for its lifetime.
func (i *Issuer) IssueAddonServer(pub crypto.PublicKey, spiffeID, pod, workload string, ttl time.Duration,
	self bool) ([]byte, *x509.Certificate, error) {
	if err := validatePodID(i.td, spiffeID); err != nil {
		return nil, nil, err
	}
	if pod == "" || workload == "" || strings.Contains(pod, "/") {
		return nil, nil, fmt.Errorf("addon certificate: pod %q and workload %q must be plain IDs", pod, workload)
	}
	subject := PodSubject(i.td, pod, workload)
	if err := validatePodSubject(subject); err != nil {
		return nil, nil, fmt.Errorf("addon certificate: %w", err)
	}
	u, err := url.Parse(spiffeID)
	if err != nil {
		return nil, nil, err
	}
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	if self {
		usage = append(usage, x509.ExtKeyUsageClientAuth)
	}
	leaf, err := i.sign(&x509.Certificate{
		Subject:     subject,
		URIs:        []*url.URL{u},
		ExtKeyUsage: usage,
	}, pub, ttl)
	if err != nil {
		return nil, nil, err
	}
	var chain []byte
	for _, der := range [][]byte{leaf.Raw, i.cert.Raw} {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return chain, leaf, nil
}

// TrustPEM is what a server that verifies the platform's workloads trusts
// (an addon's ssl_ca_file, ADR 0038): the bundle's roots and the issuer's
// certificate, PEM.
func (i *Issuer) TrustPEM() []byte {
	var out []byte
	for _, der := range append(i.BundleDER(), i.cert.Raw) {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return out
}

// IssueServer issues an instance's certificate for the nodes' port with a
// new ECDSA P-256 key: ServerAuth, the DNS names given (each equal to or
// under the control-plane domain, never an IP address or a wildcard) and
// the URI spiffe://<td>/ferun, which no other kind of certificate carries.
// The chain is leaf, issuer, root, so that a joining node finds the root
// its token pins.
func (i *Issuer) IssueServer(names []string, ttl time.Duration) (*tls.Certificate, error) {
	if len(names) == 0 {
		return nil, errors.New("a server certificate needs at least one DNS name")
	}
	for _, n := range names {
		if strings.Contains(n, "*") {
			return nil, fmt.Errorf("server name %q is a wildcard", n)
		}
		if err := validateDomain(n); err != nil {
			return nil, fmt.Errorf("server name: %w", err)
		}
		if !within(n, i.cpDomain) {
			return nil, fmt.Errorf("server name %q does not lie under the control-plane domain %s", n, i.cpDomain)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(ControlPlaneID(i.td))
	if err != nil {
		return nil, err
	}
	leaf, err := i.sign(&x509.Certificate{
		Subject:     pkix.Name{CommonName: names[0]},
		DNSNames:    append([]string(nil), names...),
		URIs:        []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, &key.PublicKey, ttl)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{leaf.Raw, i.cert.Raw, i.root.Raw},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// sign completes tmpl with what every issued certificate has (a random
// positive 128-bit serial, NotBefore backdated 10 minutes, NotAfter ttl
// ahead but not beyond the issuer's, KeyUsage DigitalSignature, not a CA)
// and signs it with the issuer.
func (i *Issuer) sign(tmpl *x509.Certificate, pub crypto.PublicKey, ttl time.Duration) (*x509.Certificate, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("certificate lifetime %v is not positive", ttl)
	}
	for _, u := range tmpl.URIs {
		if u.Host != i.td {
			return nil, fmt.Errorf("URI %s lies outside trust domain %s", u, i.td)
		}
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl.SerialNumber = serial
	tmpl.NotBefore = now.Add(-backdate)
	tmpl.NotAfter = now.Add(ttl)
	if tmpl.NotAfter.After(i.cert.NotAfter) {
		tmpl.NotAfter = i.cert.NotAfter
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.BasicConstraintsValid = true
	tmpl.IsCA = false
	tmpl.IPAddresses = nil
	der, err := x509.CreateCertificate(rand.Reader, tmpl, i.cert, pub, i.key)
	if err != nil {
		return nil, fmt.Errorf("issue certificate: %w", err)
	}
	return x509.ParseCertificate(der)
}

// certificateProto fills fleet.proto's Certificate: the chain without the
// root, the bundle, and the time of its renewal.
func (i *Issuer) certificateProto(id string, leaf *x509.Certificate, renewAfter time.Time) *fleetv1.Certificate {
	return &fleetv1.Certificate{
		SpiffeId:    id,
		Chain:       [][]byte{leaf.Raw, i.cert.Raw},
		TrustBundle: i.BundleDER(),
		NotBefore:   timestamppb.New(leaf.NotBefore),
		NotAfter:    timestamppb.New(leaf.NotAfter),
		RenewAfter:  timestamppb.New(renewAfter),
	}
}

// newSerial returns a random positive serial of 128 bits.
func newSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, err
		}
		if n.Sign() > 0 {
			return n, nil
		}
	}
}

// uriDomainPermitted reports whether c's URI name constraints admit a URI
// whose host is td.
func uriDomainPermitted(c *x509.Certificate, td string) bool {
	for _, d := range c.ExcludedURIDomains {
		if within(td, d) {
			return false
		}
	}
	if len(c.PermittedURIDomains) == 0 {
		return true
	}
	for _, d := range c.PermittedURIDomains {
		if within(td, d) {
			return true
		}
	}
	return false
}

// dnsPermitted reports whether c's DNS name constraints admit name. A
// certificate without permitted DNS domains admits none here: the issuer
// must be constrained to the platform's domains.
func dnsPermitted(c *x509.Certificate, name string) bool {
	for _, d := range c.ExcludedDNSDomains {
		if within(name, d) {
			return false
		}
	}
	for _, d := range c.PermittedDNSDomains {
		if within(name, d) {
			return true
		}
	}
	return false
}

// readCerts reads every CERTIFICATE block of a PEM file; at least one.
func readCerts(path string) ([]*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []*x509.Certificate
	for {
		var b *pem.Block
		b, raw = pem.Decode(raw)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s holds no certificate", path)
	}
	return out, nil
}

// readKey reads a private key in PEM: PKCS#8 or SEC 1.
func readKey(path string) (crypto.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for {
		var b *pem.Block
		b, raw = pem.Decode(raw)
		if b == nil {
			return nil, fmt.Errorf("%s holds no private key", path)
		}
		switch b.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			s, ok := k.(crypto.Signer)
			if !ok {
				return nil, fmt.Errorf("%s: unsupported key type %T", path, k)
			}
			return s, nil
		case "EC PRIVATE KEY":
			k, err := x509.ParseECPrivateKey(b.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			return k, nil
		}
	}
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	k, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && k.Equal(b)
}

// spkiHash is the lowercase hex SHA-256 of a DER SubjectPublicKeyInfo.
func spkiHash(spki []byte) string {
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:])
}

// equalHex compares two hex strings in constant time, ignoring case.
func equalHex(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(a)), []byte(strings.ToLower(b))) == 1
}

// noIPs is the guard of every verification helper: a certificate of this
// CA never carries an IP address.
func noIPs(c *x509.Certificate) error {
	if len(c.IPAddresses) > 0 {
		return fmt.Errorf("certificate carries IP addresses %v", ipStrings(c.IPAddresses))
	}
	return nil
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}
