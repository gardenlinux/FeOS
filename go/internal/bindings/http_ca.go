// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strconv"
	"time"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// A pod's interception CA (ADR 0028 §5): ECDSA P-256, so that clients
// without Ed25519 certificates accept it; self-signed; a CA for leaves
// only (path length 0); its permitted DNS subtrees exactly the pod's
// intercepted host names at its issue, every IP address excluded, the
// constraints critical. ferund makes it when a pod first needs one (a
// sandbox's PUT, an environment's pod at its first create or credentials)
// and keeps its key sealed at rest; it reaches the pod's feos-authn with
// the bindings, and its certificate the pod's containers in the clear part.
// A pod whose intercepted names change gets a new CA beside the old ones;
// the newest issues.
const (
	// caLifetime is a CA's validity: it outlives any pod, which keeps its
	// CA across suspends and hibernations.
	caLifetime = 10 * 365 * 24 * time.Hour
	// caBackdate covers the skew of clocks that check it.
	caBackdate = time.Hour
	// maxCAs bounds the CAs a pod keeps: the oldest go first.
	maxCAs = 8
)

// newInterceptCA makes a CA for hosts and returns its certificate and its
// key, both PEM, the key PKCS#8.
func newInterceptCA(podID podid.ID, seq int, hosts []string, now time.Time) (string, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("the CA's key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", "", fmt.Errorf("the CA's serial: %w", err)
	}
	_, all4, _ := net.ParseCIDR("0.0.0.0/0")
	_, all6, _ := net.ParseCIDR("::/0")
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"FeOS"},
			CommonName:   "FeOS interception CA " + podID.String() + " #" + strconv.Itoa(seq),
		},
		NotBefore:                   now.Add(-caBackdate),
		NotAfter:                    now.Add(caLifetime),
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         slices.Clone(hosts),
		ExcludedIPRanges:            []*net.IPNet{all4, all6},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", fmt.Errorf("the CA's certificate: %w", err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("the CA's key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})), nil
}

// caAAD binds a CA's sealed key to its pod and number.
func caAAD(id podid.ID, seq int) []byte {
	return sealAAD(tableCA, id, strconv.Itoa(seq), "ca")
}

// ensureCAs returns the pod's CAs, oldest first, after making a new one
// for hosts (sorted) when the newest's hosts differ, and dropping the
// oldest beyond maxCAs. A concurrent writer's CA for the same number wins;
// one for other hosts makes this one try the next number. tx may be a
// transaction or the pool.
func (s *Service) ensureCAs(ctx context.Context, tx *store.Store, id podid.ID, hosts []string) ([]store.InterceptCA, error) {
	for range 4 {
		cas, err := tx.InterceptCAs(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("interception CAs of %s: %w", id, err)
		}
		seq := 1
		if n := len(cas); n > 0 {
			if slices.Equal(cas[n-1].Hosts, hosts) {
				return cas, nil
			}
			seq = cas[n-1].Seq + 1
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return nil, err
		}
		certPEM, keyPEM, err := newInterceptCA(id, seq, hosts, now)
		if err != nil {
			return nil, err
		}
		sealed, err := s.sealer.Seal([]byte(keyPEM), caAAD(id, seq))
		if err != nil {
			return nil, fmt.Errorf("seal the CA's key: %w", err)
		}
		if _, err := tx.InsertInterceptCA(ctx, id, store.InterceptCA{Seq: seq, Hosts: hosts, CertPEM: certPEM,
			SealedKey: sealed}); err != nil {
			return nil, fmt.Errorf("an interception CA of %s: %w", id, err)
		}
		if seq > maxCAs {
			if err := tx.DeleteInterceptCAsBefore(ctx, id, seq-maxCAs+1); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("interception CAs of %s keep changing", id)
}

// openCAs opens the CAs' keys for the pod's bindings.
func (s *Service) openCAs(id podid.ID, cas []store.InterceptCA) ([]*fleetv1.InterceptCa, error) {
	out := make([]*fleetv1.InterceptCa, 0, len(cas))
	for _, c := range cas {
		key, err := s.sealer.Open(c.SealedKey, caAAD(id, c.Seq))
		if err != nil {
			return nil, fmt.Errorf("interception CA %d of %s: %w", c.Seq, id, err)
		}
		out = append(out, &fleetv1.InterceptCa{CertPem: c.CertPEM, KeyPem: string(key)})
	}
	return out, nil
}

// caDigest is the part of a pod's version its CAs make: the SHA-256 of
// their certificates.
func caDigest(cas []store.InterceptCA) []string {
	out := make([]string, len(cas))
	for i, c := range cas {
		sum := sha256.Sum256([]byte(c.CertPEM))
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}
