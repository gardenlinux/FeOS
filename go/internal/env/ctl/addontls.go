// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/env/addons"
	"github.com/gardenlinux/feos/go/internal/env/spec"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// An addon that admits workloads by their certificates (ADR 0038): its
// server certificate, which the control plane's issuer makes for the
// addon's ID and the key of its credential addons.TLSKey, and the grants
// of the deployment's references, which its tls step writes into the
// addon's map.

// AddonIssuer issues addons' server certificates: the control plane's
// issuer (*pki.Issuer).
type AddonIssuer interface {
	TrustDomain() string
	IssueAddonServer(pub crypto.PublicKey, spiffeID, pod, workload string, ttl time.Duration,
		self bool) ([]byte, *x509.Certificate, error)
	TrustPEM() []byte
}

// addonCert is an instance's server certificate as the controller issued
// it. The controller keeps it in memory: it is state it can always make
// again, and a new leader that issues anew costs the addon one reload.
type addonCert struct {
	// key is the credential's PEM it certifies, chain its PEM.
	key, chain string
	// renewAt is when the controller issues the next (the database's
	// clock): three quarters into its lifetime.
	renewAt time.Time
}

// newKeyCredential is a fresh addons.TLSKey: an Ed25519 key, PKCS#8 PEM.
func newKeyCredential() (string, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// publicOf is the public key of a key credential's PEM.
func publicOf(keyPEM string) (crypto.PublicKey, error) {
	b, _ := pem.Decode([]byte(keyPEM))
	if b == nil || b.Type != "PRIVATE KEY" {
		return nil, errors.New("the key credential is no PKCS#8 PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the key credential: %w", err)
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("the key credential cannot sign")
	}
	return signer.Public(), nil
}

// addonTLS is what instance cur of addon name is provisioned with: its
// server certificate, issued when it has none, when its key differs or
// when it is due (for ClientAuth too when its steps reach it as itself,
// ADR 0039), its ID, and the grants of the target's references.
func (c *Controller) addonTLS(ctx context.Context, v *view, name string, cs addons.CertificateServer,
	cur store.EnvInstanceRow) (*addons.TLS, error) {
	key, err := c.ResolveSecret(ctx, v.env.ID, addons.CredentialSecret(name, addons.TLSKey), 1)
	if err != nil {
		return nil, err
	}
	pod := cur.Sandbox.ID
	iss := c.d.Issuer
	id := pki.WorkloadID(iss.TrustDomain(), v.env.Owner, v.env.ID, spec.KindAddon, name)
	c.mu.Lock()
	got, ok := c.addonCerts[pod]
	c.mu.Unlock()
	if !ok || got.key != key || !v.now.Before(got.renewAt) {
		pub, err := publicOf(key)
		if err != nil {
			return nil, err
		}
		chain, _, err := iss.IssueAddonServer(pub, id, pod.String(), model.WorkloadGroupID(v.env.ID, spec.KindAddon, name),
			c.t.addonCertTTL, cs.ProvisionsAsItself())
		if err != nil {
			return nil, err
		}
		got = addonCert{key: key, chain: string(chain), renewAt: v.now.Add(c.t.addonCertTTL / 4 * 3)}
		c.mu.Lock()
		c.addonCerts[pod] = got
		// An instance's pod never comes back: what is long past its
		// renewal belongs to one that is gone.
		maps.DeleteFunc(c.addonCerts, func(_ podid.ID, a addonCert) bool {
			return a.renewAt.Before(v.now.Add(-c.t.addonCertTTL))
		})
		c.mu.Unlock()
		c.log.Info("addon certificate issued", "environment", v.env.ID, "addon", name, "sandbox", pod)
	}
	return &addons.TLS{CertPEM: got.chain, CAPEM: string(iss.TrustPEM()), ID: id,
		Grants: grantsOf(iss.TrustDomain(), v, name, cs)}, nil
}

// grantsOf are the grants of the target's references to addon name: per
// service and job whose env names one of its roles, the workload's ID as
// its pods' certificates carry it in their OU, the one its sets on the
// nodes carry (workloadOf: an attached service's is its sandbox's), with
// its SPIFFE ID; sorted, one per workload and role.
func grantsOf(td string, v *view, name string, cs addons.CertificateServer) []addons.Grant {
	seen := map[string]bool{}
	var out []addons.Grant
	add := func(kind, object string, env map[string]spec.EnvValue) {
		for _, key := range slices.Sorted(maps.Keys(env)) {
			val := env[key]
			if val.Kind != spec.EnvAddon || val.Addon != name {
				continue
			}
			role := cs.GrantedRole(addons.Ref{Key: val.Key, Fields: val.Fields})
			workload := workloadOf(v.env.ID, v.target, kind, object)
			if role == "" || workload == "" || seen[workload+"\x00"+role] {
				continue
			}
			seen[workload+"\x00"+role] = true
			id := pki.WorkloadID(td, v.env.Owner, v.env.ID, kind, object)
			if sbx, ok := strings.CutPrefix(workload, "wl-s-"); ok {
				id = pki.WorkloadID(td, v.env.Owner, v.env.ID, pki.WorkloadSandbox, sbx)
			}
			out = append(out, addons.Grant{Workload: workload, ID: id, Role: role})
		}
	}
	for _, svc := range slices.Sorted(maps.Keys(v.target.Services)) {
		add(spec.KindService, svc, v.target.Services[svc].Env)
	}
	for _, job := range slices.Sorted(maps.Keys(v.target.Jobs)) {
		add(spec.KindJob, job, v.target.Jobs[job].Env)
	}
	slices.SortFunc(out, func(a, b addons.Grant) int {
		if c := strings.Compare(a.Workload, b.Workload); c != 0 {
			return c
		}
		return strings.Compare(a.Role, b.Role)
	})
	return out
}
