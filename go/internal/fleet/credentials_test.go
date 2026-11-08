// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hpke"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

type credsFixture struct {
	*harness
	serial string
	pod    podid.ID
	env    uuid.UUID
	priv   hpke.PrivateKey
	pub    []byte
}

func newCredsFixture(t *testing.T) *credsFixture {
	h := newHarness(t)
	f := &credsFixture{harness: h, serial: h.joined("n1")}
	h.joined("n2")
	if _, _, err := h.st.CreateTenant(bg, "acme"); err != nil {
		t.Fatal(err)
	}
	f.pod = podid.Mint()
	f.env = dbtest.Environment(t, h.st.Q(), "acme", "box")
	if _, _, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: f.pod, Owner: "acme", Image: "alpine", VCPUs: 1, MemoryMiB: 512,
		EnvironmentID: f.env}); err != nil {
		t.Fatal(err)
	}
	h.exec("UPDATE sandboxes SET node = 'n1', client_state = 'running' WHERE id = $1", f.pod)
	priv, err := pki.HPKEKEM().GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	f.priv, f.pub = priv, priv.PublicKey().Bytes()
	return f
}

func (f *credsFixture) issue(pod string) (*fleetv1.IssuePodCredentialsResponse, error) {
	return f.s.UplinkServer().IssuePodCredentials(uplinkCtx("n1", f.serial), &fleetv1.IssuePodCredentialsRequest{
		PodId: pod, Csr: newCSR(f.t), HpkePublicKey: f.pub,
	})
}

func TestIssuePodCredentials(t *testing.T) {
	f := newCredsFixture(t)
	bindings := &fleetv1.PodBindings{Kube: []*fleetv1.KubeBinding{{Name: "prod", Port: 6443, Server: "https://k8s.example.com",
		Credential: &fleetv1.KubeBinding_Token{Token: "secret"}}}}
	f.bindings.set(bindings, "v1")

	resp, err := f.issue(f.pod.String())
	if err != nil {
		t.Fatal(err)
	}
	// The sandbox's workload ID, its environment by its ID (ADR 0036, 0037).
	want := "spiffe://feos.test/tenant/acme/env/" + f.env.String() + "/sandbox/" + f.pod.String()
	if resp.GetCertificate().GetSpiffeId() != want {
		t.Fatalf("SPIFFE ID %q; want %q", resp.GetCertificate().GetSpiffeId(), want)
	}
	leaf, err := x509.ParseCertificate(resp.GetCertificate().GetChain()[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != want || len(leaf.DNSNames) != 0 {
		t.Fatalf("leaf names %v %v", leaf.URIs, leaf.DNSNames)
	}
	if got, want := leaf.Subject.String(), "CN=feos.test/"+f.pod.String()+",OU=wl-s-"+f.pod.String(); got != want {
		t.Fatalf("subject %q, want %q", got, want)
	}
	if got := leaf.NotAfter.Sub(leaf.NotBefore); got < 69*time.Minute || got > 71*time.Minute {
		t.Fatalf("pod certificate lifetime %v (backdated 10 minutes, then the default hour)", got)
	}
	sealed := resp.GetBindings()
	if sealed.GetVersion() != "v1" {
		t.Fatalf("sealed version %q", sealed.GetVersion())
	}
	opened, err := pki.Open(f.priv, f.pod, sealed)
	if err != nil || !proto.Equal(opened, bindings) {
		t.Fatalf("opened %v, %v", opened, err)
	}
	certs, err := f.st.ListIssuedCerts(bg, f.pod)
	if err != nil || len(certs) != 1 {
		t.Fatalf("issued certs %+v, %v", certs, err)
	}
	sb, _ := f.st.GetSandbox(bg, f.pod)
	if c := certs[0]; c.Serial != pki.Serial(leaf) || c.Incarnation != sb.Incarnation || c.Node != "n1" || c.SPIFFEID != want ||
		string(c.HPKEPublicKey) != string(f.pub) || c.BindingsVersion != "v1" {
		t.Fatalf("issued cert %+v", c)
	}
	if n := f.count(`SELECT count(*) FROM activity WHERE sandbox_id = $1 AND tenant_id = 'acme' AND kind = 'pod_credentials'
		AND node = 'n1' AND detail->>'bindings_version' = 'v1' AND detail->>'serial' = $2
		AND length(detail->>'hpke_key_sha256') = 64`, f.pod, pki.Serial(leaf)); n != 1 {
		t.Fatalf("%d pod_credentials rows", n)
	}

	// A request repeated with the same key (an answer lost) is answered
	// normally: a new certificate, the same bindings.
	again, err := f.issue(f.pod.String())
	if err != nil {
		t.Fatal(err)
	}
	if serialOf(t, again.GetCertificate()) == pki.Serial(leaf) {
		t.Fatal("the same certificate again")
	}
	if opened, err := pki.Open(f.priv, f.pod, again.GetBindings()); err != nil || !proto.Equal(opened, bindings) {
		t.Fatalf("repeat bindings %v, %v", opened, err)
	}

	// At most 5 per pod per minute, whatever the key.
	for range 3 {
		if _, err := f.issue(f.pod.String()); err != nil {
			t.Fatal(err)
		}
	}
	_, err = f.issue(f.pod.String())
	wantCode(t, err, codes.ResourceExhausted, "too many credential requests for pod "+f.pod.String())
	f.exec("UPDATE issued_certs SET issued_at = now() - interval '61 seconds' WHERE pod_id = $1", f.pod)
	if _, err := f.issue(f.pod.String()); err != nil {
		t.Fatalf("after a minute: %v", err)
	}
}

func TestIssuePodCredentialsStates(t *testing.T) {
	f := newCredsFixture(t)
	for _, state := range []string{"creating", "running", "resuming"} {
		f.exec("UPDATE sandboxes SET client_state = $2 WHERE id = $1", f.pod, state)
		resp, err := f.issue(f.pod.String())
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if resp.GetBindings() != nil {
			t.Fatal("sealed bindings without any")
		}
		f.exec("DELETE FROM issued_certs")
	}
	// A pod without a run here, or not placed here, is refused before its
	// bindings are read.
	calls := f.bindings.callCount()
	for _, state := range []string{"pending", "pausing", "paused", "hibernating", "hibernated", "stopped", "failed", "unknown", "lost"} {
		f.exec("UPDATE sandboxes SET client_state = $2 WHERE id = $1", f.pod, state)
		_, err := f.issue(f.pod.String())
		wantCode(t, err, codes.PermissionDenied, "pod "+f.pod.String()+" has no run on node n1")
	}
	f.exec("UPDATE sandboxes SET client_state = 'running', node = 'n2' WHERE id = $1", f.pod)
	_, err := f.issue(f.pod.String())
	wantCode(t, err, codes.PermissionDenied, "pod "+f.pod.String()+" is not placed on node n1")
	f.exec("UPDATE sandboxes SET node = NULL WHERE id = $1", f.pod)
	_, err = f.issue(f.pod.String())
	wantCode(t, err, codes.PermissionDenied, "is not placed on node n1")
	f.exec("UPDATE sandboxes SET node = 'n1' WHERE id = $1", f.pod)

	unknown := uuid.NewString()
	_, err = f.issue(unknown)
	wantCode(t, err, codes.NotFound, "no pod "+unknown)
	_, err = f.issue("pod-1")
	wantCode(t, err, codes.NotFound, "no pod pod-1")
	if n := f.bindings.callCount(); n != calls {
		t.Fatalf("the bindings were read %d times for pods refused", n-calls)
	}

	// The signing request is for an Ed25519 key; the HPKE key is X25519.
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecCSR, _ := pki.CreateCSR(p256, "x")
	_, err = f.s.UplinkServer().IssuePodCredentials(uplinkCtx("n1", f.serial), &fleetv1.IssuePodCredentialsRequest{
		PodId: f.pod.String(), Csr: ecCSR, HpkePublicKey: f.pub})
	wantCode(t, err, codes.InvalidArgument, "Ed25519")
	_, err = f.s.UplinkServer().IssuePodCredentials(uplinkCtx("n1", f.serial), &fleetv1.IssuePodCredentialsRequest{
		PodId: f.pod.String(), Csr: newCSR(t), HpkePublicKey: f.pub[:31]})
	wantCode(t, err, codes.InvalidArgument, "hpke_public_key")

	// The bindings source fails: nothing is issued.
	f.bindings.mu.Lock()
	f.bindings.err = errors.New("bindings unavailable")
	f.bindings.mu.Unlock()
	_, err = f.issue(f.pod.String())
	wantCode(t, err, codes.Unavailable, "bindings unavailable")
	f.bindings.mu.Lock()
	f.bindings.err = nil
	f.bindings.mu.Unlock()
	// An empty set seals nothing.
	f.bindings.set(&fleetv1.PodBindings{}, "v0")
	if resp, err := f.issue(f.pod.String()); err != nil || resp.GetBindings() != nil {
		t.Fatalf("an empty set: %v %v", resp.GetBindings(), err)
	}
	if certs, _ := f.st.ListIssuedCerts(bg, f.pod); len(certs) != 1 || certs[0].BindingsVersion != "" {
		t.Fatalf("issued certs %+v", certs)
	}

	// A superseded certificate within its grace still passes the checks
	// (§8); past its grace the node is told to use its newest one.
	f.exec("DELETE FROM issued_certs")
	f.exec(`UPDATE node_certs SET revoked_at = now() + interval '15 minutes', revoked_reason = 'superseded'
		WHERE serial = $1`, f.serial)
	if _, err := f.issue(f.pod.String()); err != nil {
		t.Fatalf("a superseded certificate within its grace: %v", err)
	}
	f.exec("UPDATE node_certs SET revoked_at = now() - interval '1 second' WHERE serial = $1", f.serial)
	_, err = f.issue(f.pod.String())
	wantCode(t, err, codes.Unavailable, "certificate superseded; reconnect with the node's newest certificate")
	f.exec("UPDATE node_certs SET revoked_at = NULL, revoked_reason = '' WHERE serial = $1", f.serial)

	// The node's certificate must pass the checks: a forked or removed
	// node, or a revoked certificate, gets nothing.
	f.exec("UPDATE nodes SET identity_forked_at = now() WHERE name = 'n1'")
	_, err = f.issue(f.pod.String())
	wantCode(t, err, codes.PermissionDenied, "forked")
	f.exec("UPDATE nodes SET identity_forked_at = NULL, status = 'removed' WHERE name = 'n1'")
	_, err = f.issue(f.pod.String())
	wantCode(t, err, codes.PermissionDenied, "node n1 was removed; join it again")
	f.exec("UPDATE nodes SET status = 'ready' WHERE name = 'n1'")
	f.exec("UPDATE node_certs SET revoked_at = now(), revoked_reason = 'rejoined' WHERE serial = $1", f.serial)
	_, err = f.issue(f.pod.String())
	wantCode(t, err, codes.PermissionDenied, "was revoked (rejoined)")
	_, err = f.s.UplinkServer().IssuePodCredentials(bg, &fleetv1.IssuePodCredentialsRequest{PodId: f.pod.String()})
	wantCode(t, err, codes.Unauthenticated, "")
}

// TestIssuePodCredentialsConcurrentLimit sends more concurrent requests for
// one pod than the limit allows, as feos-authn retrying over several
// uplinks may: exactly the limit is issued, the rest is refused.
func TestIssuePodCredentialsConcurrentLimit(t *testing.T) {
	f := newCredsFixture(t)
	f.exec(`INSERT INTO issued_certs (serial, pod_id, incarnation, node, spiffe_id, hpke_public_key, not_after)
		SELECT 'earlier-' || g, id, incarnation, 'n1', 'x', '\x00', now() + interval '1 hour'
		FROM sandboxes, generate_series(1, 2) g WHERE id = $1`, f.pod)
	const requests = 12
	csrs := make([][]byte, requests)
	for i := range csrs {
		csrs[i] = newCSR(t)
	}
	errs := make(chan error, requests)
	start := make(chan struct{})
	for i := range requests {
		go func() {
			<-start
			_, err := f.s.UplinkServer().IssuePodCredentials(uplinkCtx("n1", f.serial), &fleetv1.IssuePodCredentialsRequest{
				PodId: f.pod.String(), Csr: csrs[i], HpkePublicKey: f.pub,
			})
			errs <- err
		}()
	}
	close(start)
	issued, refused := 0, 0
	for range requests {
		switch err := <-errs; {
		case err == nil:
			issued++
		default:
			wantCode(t, err, codes.ResourceExhausted, "too many credential requests for pod "+f.pod.String())
			refused++
		}
	}
	if issued != credentialsPerWindow-2 || refused != requests-issued {
		t.Fatalf("%d issued, %d refused; want %d issued after 2 earlier ones", issued, refused, credentialsPerWindow-2)
	}
	if n := f.count("SELECT count(*) FROM issued_certs WHERE pod_id = $1", f.pod); n != credentialsPerWindow {
		t.Fatalf("%d certificates of the pod within a minute; the limit is %d", n, credentialsPerWindow)
	}
}

func TestIssuePodCredentialsWithoutBindings(t *testing.T) {
	f := newCredsFixture(t)
	s := New(Config{InstanceID: "i2", TrustDomain: testTD, Log: f.s.cfg.Log}, Deps{DB: f.d, Issuer: f.iss, Links: f.links, Hooks: f.hooks})
	defer s.stop()
	resp, err := s.UplinkServer().IssuePodCredentials(uplinkCtx("n1", f.serial), &fleetv1.IssuePodCredentialsRequest{
		PodId: f.pod.String(), Csr: newCSR(t), HpkePublicKey: f.pub})
	if err != nil || resp.GetBindings() != nil || resp.GetCertificate() == nil {
		t.Fatalf("without a bindings source: %v, %v", resp, err)
	}
}
