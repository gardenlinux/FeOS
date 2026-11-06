// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// certsEvery is how often the certificate files are read again.
const certsEvery = 30 * time.Second

// Certs is a certificate of the HTTPS listener from files: the
// operator's wildcard for the apps zone, or the one for the tenant zone
// (ADR 0031), which cert-manager renews into a Secret that the kubelet
// updates in the mounted files. The apps zone's is the default: every SNI
// that no other certificate covers, or none, gets it; an unknown host is
// answered 404 by the proxy.
type Certs struct {
	certFile, keyFile string
	log               *slog.Logger
	cur               atomic.Pointer[tls.Certificate]

	mu sync.Mutex
	// sum identifies the files' content that cur was loaded from; bad
	// the last content or error that failed, logged once.
	sum [32]byte
	bad string
}

// LoadPair loads the certificate and key of the PEM files, or fails.
func LoadPair(certFile, keyFile string, log *slog.Logger) (*Certs, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Certs{certFile: certFile, keyFile: keyFile, log: log}
	if _, err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// reload reads the files and swaps the certificate when their content
// changed and makes a valid pair. It reports whether it swapped; on an
// error the certificate stays as it was.
func (c *Certs) reload() (bool, error) {
	certPEM, err := os.ReadFile(c.certFile)
	if err != nil {
		return false, fmt.Errorf("read the TLS certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(c.keyFile)
	if err != nil {
		return false, fmt.Errorf("read the TLS key: %w", err)
	}
	h := sha256.New()
	_ = binary.Write(h, binary.BigEndian, uint64(len(certPEM)))
	h.Write(certPEM)
	h.Write(keyPEM)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur.Load() != nil && sum == c.sum {
		return false, nil
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false, fmt.Errorf("the TLS certificate %s and key %s: %w", c.certFile, c.keyFile, err)
	}
	c.sum = sum
	c.bad = ""
	c.cur.Store(&pair)
	return true, nil
}

// Run reads the files again every 30 s until ctx ends. A pair that does
// not load keeps the certificate in use (the kubelet swaps the files
// one by one, so the next turn loads both), logged once per content.
func (c *Certs) Run(ctx context.Context) {
	t := time.NewTicker(certsEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.turn()
		}
	}
}

func (c *Certs) turn() {
	changed, err := c.reload()
	switch {
	case err != nil:
		c.mu.Lock()
		repeat := c.bad == err.Error()
		c.bad = err.Error()
		c.mu.Unlock()
		if !repeat {
			c.log.Warn("the TLS certificate files do not load; the certificate in use stays", "err", err)
		}
	case changed:
		attrs := []any{"file", c.certFile}
		if leaf := c.Leaf(); leaf != nil {
			attrs = append(attrs, "subject", leaf.Subject.String(), "notAfter", leaf.NotAfter)
		}
		c.log.Info("loaded a new TLS certificate", attrs...)
	}
}

// GetCertificate is tls.Config's: the certificate in use, whatever the
// client's SNI.
func (c *Certs) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.cur.Load(), nil
}

// covers reports whether the certificate in use is valid for the
// canonical host name (a wildcard covering one label).
func (c *Certs) covers(name string) bool {
	leaf := c.Leaf()
	return name != "" && leaf != nil && leaf.VerifyHostname(name) == nil
}

// Leaf is the certificate in use.
func (c *Certs) Leaf() *x509.Certificate {
	if pair := c.cur.Load(); pair != nil {
		return pair.Leaf
	}
	return nil
}

// TLSConfig is the HTTPS listener's TLS: 1.2 and later, HTTP/2 and
// HTTP/1.1, the certificate of c.
func (c *Certs) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: c.GetCertificate,
	}
}
