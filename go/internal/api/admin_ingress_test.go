// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/fleet"
	ingresssvc "github.com/gardenlinux/feos/go/internal/ingress/service"
	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/pki"
)

// ingressWorld is an API with the fleet and the ingress service on one
// database and one CA, on a meshed cluster.
type ingressWorld struct {
	e   *env
	iss *pki.Issuer
	svc *ingresssvc.Service
}

func newIngressWorld(t *testing.T) *ingressWorld {
	t.Helper()
	t.Parallel()
	d := dbtest.New(t)
	base := t.TempDir()
	if err := pki.InitCA(pki.InitCAOptions{RootDir: filepath.Join(base, "root"), Dir: filepath.Join(base, "ca"),
		TrustDomain: "feos.test", DNSDomains: []string{"example.test"}}); err != nil {
		t.Fatal(err)
	}
	iss, err := pki.LoadIssuer(filepath.Join(base, "ca"), "feos.test")
	if err != nil {
		t.Fatal(err)
	}
	ula := netip.MustParsePrefix("fd00:1:2::/64")
	fl := fleet.New(fleet.Config{TrustDomain: "feos.test"}, fleet.Deps{DB: d, Issuer: iss})
	svc := ingresssvc.New(ingresssvc.Config{InstanceID: testInstance, TrustDomain: "feos.test", PodNetwork: ula,
		Log: dbtest.Logger(t)}, ingresssvc.Deps{DB: d, Issuer: iss})
	e := newEnv(t, envOptions{db: d, fleet: fl, ingress: svc})
	if err := e.st.InsertPodNetwork(context.Background(), ula); err != nil {
		t.Fatal(err)
	}
	return &ingressWorld{e: e, iss: iss, svc: svc}
}

// join joins an ingress with token (as sent, without the pin) and says
// Hello with WireGuard key wg, as an ingress does on the nodes' port. It
// returns the certificate.
func (w *ingressWorld) join(t *testing.T, token, name string, wg []byte) *x509.Certificate {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.CreateCSR(key, name)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resp, err := w.svc.Join(ctx, &wire.JoinRequest{Token: token, Name: name, CSR: csr})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(resp.Certificate.Chain[0])
	if err != nil {
		t.Fatal(err)
	}
	if wg != nil {
		hctx := nodelink.ContextWithIngress(ctx, nodelink.IngressPeer{Name: resp.Name, Serial: pki.Serial(leaf),
			Addr: "192.0.2.9:40000", NotAfter: leaf.NotAfter, Conn: 1})
		if _, err := w.svc.Hello(hctx, &wire.HelloRequest{PublicKey: wg, Endpoint: "10.0.0.9:51820", Version: "v1"}); err != nil {
			t.Fatal(err)
		}
	}
	return leaf
}

// Join tokens of kind ingress: the kind in the answer and in the list; a
// bad kind and a name that contradicts node are refused; without the
// ingress service, ingress tokens answer 503.
func TestIngressJoinTokens(t *testing.T) {
	w := newIngressWorld(t)
	e := w.e
	r := e.do("POST", "/api/v1/admin/join-tokens", adminAuth(), map[string]any{"kind": "ingress", "name": "ferun-ingress-0"})
	expect(t, r, http.StatusCreated, "")
	if got := keysInOrder(t, r.Body.Bytes()); strings.Join(got, ",") != "id,token,expiresAt,kind,name,node" {
		t.Fatalf("keys %v", got)
	}
	jt := decode[map[string]string](t, r)
	if jt["kind"] != "ingress" || jt["name"] != "ferun-ingress-0" || jt["node"] != "ferun-ingress-0" ||
		!strings.HasPrefix(jt["token"], "feos-ingress-join-") || !strings.HasSuffix(jt["token"], "."+w.iss.JoinPin()) {
		t.Fatalf("token %+v", jt)
	}
	// node is name's alias; the same value twice is fine.
	r = e.do("POST", "/api/v1/admin/join-tokens", adminAuth(), map[string]any{"kind": "ingress", "node": "ferun-ingress-1",
		"name": "ferun-ingress-1"})
	expect(t, r, http.StatusCreated, "")
	if jt := decode[map[string]string](t, r); jt["name"] != "ferun-ingress-1" || jt["kind"] != "ingress" {
		t.Fatalf("alias %+v", jt)
	}
	// A node token by default, with name.
	r = e.do("POST", "/api/v1/admin/join-tokens", adminAuth(), map[string]any{"name": "n1"})
	expect(t, r, http.StatusCreated, "")
	if jt := decode[map[string]string](t, r); jt["kind"] != "node" || jt["node"] != "n1" || jt["name"] != "n1" {
		t.Fatalf("node token %+v", jt)
	}

	expect(t, e.do("POST", "/api/v1/admin/join-tokens", adminAuth(), map[string]any{"kind": "pod"}), http.StatusBadRequest, "")
	expect(t, e.do("POST", "/api/v1/admin/join-tokens", adminAuth(), map[string]any{"node": "n1", "name": "n2"}),
		http.StatusBadRequest, "")
	expect(t, e.do("POST", "/api/v1/admin/join-tokens", adminAuth(), map[string]any{"kind": "ingress", "name": "Bad_Name"}),
		http.StatusBadRequest, "")

	r = e.do("GET", "/api/v1/admin/join-tokens", adminAuth(), nil)
	expect(t, r, http.StatusOK, "")
	list := decode[struct {
		JoinTokens []map[string]string `json:"joinTokens"`
	}](t, r)
	kinds := map[string]string{}
	for _, jt := range list.JoinTokens {
		kinds[jt["node"]] = jt["kind"]
	}
	if len(list.JoinTokens) != 3 || kinds["ferun-ingress-0"] != "ingress" || kinds["ferun-ingress-1"] != "ingress" ||
		kinds["n1"] != "node" {
		t.Fatalf("list %+v", list.JoinTokens)
	}

	// Without the service.
	bare := newEnv(t, envOptions{db: e.d})
	expect(t, bare.do("POST", "/api/v1/admin/join-tokens", adminAuth(), map[string]any{"kind": "ingress"}),
		http.StatusServiceUnavailable, "")
	expect(t, bare.do("GET", "/api/v1/admin/ingresses", adminAuth(), nil), http.StatusServiceUnavailable, "")
	expect(t, bare.do("DELETE", "/api/v1/admin/ingresses/x", adminAuth(), nil), http.StatusServiceUnavailable, "")
}

// The ingress instances: listed with their place on the mesh, removed
// with 204, then unknown.
func TestIngressesAdmin(t *testing.T) {
	w := newIngressWorld(t)
	e := w.e
	expect(t, e.do("GET", "/api/v1/admin/ingresses", adminAuth(), nil), http.StatusOK, `{"ingresses":[]}`+"\n")

	tokenOf := func(name string) string {
		r := e.do("POST", "/api/v1/admin/join-tokens", adminAuth(), map[string]any{"kind": "ingress", "name": name})
		expect(t, r, http.StatusCreated, "")
		sent, _, err := pki.SplitIngressJoinToken(decode[map[string]string](t, r)["token"])
		if err != nil {
			t.Fatal(err)
		}
		return sent
	}
	wg := bytes.Repeat([]byte{7}, 32)
	leaf := w.join(t, tokenOf("in-0"), "in-0", wg)
	w.join(t, tokenOf("in-1"), "in-1", nil)

	r := e.do("GET", "/api/v1/admin/ingresses", adminAuth(), nil)
	expect(t, r, http.StatusOK, "")
	raw := decode[struct {
		Ingresses []map[string]any `json:"ingresses"`
	}](t, r)
	if len(raw.Ingresses) != 2 {
		t.Fatalf("ingresses %+v", raw.Ingresses)
	}
	want := []string{"name", "status", "joinedAt", "removedAt", "meshPrefix", "meshAddress", "meshEndpoint", "publicKey",
		"version", "lastSeen", "leftAt", "connected", "admittedVersion", "certNotAfter"}
	for _, k := range want {
		if _, ok := raw.Ingresses[0][k]; !ok {
			t.Errorf("no %s in %+v", k, raw.Ingresses[0])
		}
	}
	if len(raw.Ingresses[0]) != len(want) {
		t.Errorf("fields %+v", raw.Ingresses[0])
	}
	list := decode[struct {
		Ingresses []struct {
			Name, Status, JoinedAt, RemovedAt, MeshPrefix, MeshAddress, MeshEndpoint, PublicKey, Version, LastSeen,
			LeftAt, CertNotAfter string
			Connected       bool
			AdmittedVersion int64
		} `json:"ingresses"`
	}](t, r)
	g0, g1 := list.Ingresses[0], list.Ingresses[1]
	if g0.Name != "in-0" || g0.Status != model.IngressJoined || g0.JoinedAt == "" || g0.RemovedAt != "" ||
		g0.MeshPrefix != "fd00:1:2::1:0:0/96" || g0.MeshAddress != "fd00:1:2::1:0:1" || g0.MeshEndpoint != "10.0.0.9:51820" ||
		g0.PublicKey != base64.StdEncoding.EncodeToString(wg) || g0.Version != "v1" || g0.LastSeen == "" || g0.LeftAt != "" ||
		!g0.Connected || g0.AdmittedVersion != 1 || g0.CertNotAfter != leaf.NotAfter.UTC().Format(time.RFC3339) {
		t.Errorf("in-0 %+v", g0)
	}
	if g1.Name != "in-1" || g1.Connected || g1.MeshPrefix != "" || g1.PublicKey != "" || g1.AdmittedVersion != 0 ||
		g1.CertNotAfter == "" {
		t.Errorf("in-1 %+v", g1)
	}

	expect(t, e.do("DELETE", "/api/v1/admin/ingresses/in-0", adminAuth(), nil), http.StatusNoContent, "")
	expect(t, e.do("DELETE", "/api/v1/admin/ingresses/nobody", adminAuth(), nil), http.StatusNotFound,
		errBody("unknown ingress nobody"))
	r = e.do("GET", "/api/v1/admin/ingresses", adminAuth(), nil)
	expect(t, r, http.StatusOK, "")
	after := decode[struct {
		Ingresses []map[string]any `json:"ingresses"`
	}](t, r)
	if after.Ingresses[0]["status"] != model.IngressRemoved || after.Ingresses[0]["removedAt"] == "" {
		t.Errorf("removed %+v", after.Ingresses[0])
	}
	// Its row stays: removing it again changes nothing and answers the
	// same, as a node's removal does.
	expect(t, e.do("DELETE", "/api/v1/admin/ingresses/in-0", adminAuth(), nil), http.StatusNoContent, "")
	// A project is no admin.
	expect(t, e.do("GET", "/api/v1/admin/ingresses", bearerOf(e.projectToken), nil), http.StatusUnauthorized, "")
}
