// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"unicode/utf8"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The Kubernetes surface:
//
//	PUT    /api/v1/sandboxes/{id}/kube   sandbox  the complete binding set
//	GET    /api/v1/sandboxes/{id}/kube   sandbox  the set without secret material
//	DELETE /api/v1/sandboxes/{id}/kube   sandbox  no bindings
//	POST   /api/v1/kube/preflight        project   one binding, no sandbox
//
// A PUT states the whole set, so a client's reconcile is one verb. The set
// is stored with its credentials sealed; it reaches a pod sealed to the
// run's key when the pod asks for its credentials (PodBindings).

// The auth types of a Kubernetes binding.
const (
	kubeAuthToken      = "token"
	kubeAuthClientCert = "client_cert"
)

// maxKubeBindings bounds a set: each binding is a listener in the pod. A
// sanity cap, not a policy.
const maxKubeBindings = 32

// maxSetBytes bounds the JSON of a set with its version, as the old
// system's control line did, so that a set fits whatever carries it.
const maxSetBytes = 1<<20 - 1024

// ------------------------------------------------------------ wire types --

type kubeAuthIn struct {
	Type    string `json:"type"`
	Token   string `json:"token"`
	CertPem string `json:"certPem"`
	KeyPem  string `json:"keyPem"`
}

type kubeBindingIn struct {
	Name       string     `json:"name"`
	Port       int        `json:"port"`
	Server     string     `json:"server"`
	ServerName string     `json:"serverName"`
	CaPem      string     `json:"caPem"`
	Auth       kubeAuthIn `json:"auth"`
}

type kubeBindingOut struct {
	Name        string `json:"name"`
	Port        int    `json:"port"`
	Server      string `json:"server"`
	ServerName  string `json:"serverName"`
	HasCa       bool   `json:"hasCa"`
	AuthType    string `json:"authType"`
	Fingerprint string `json:"fingerprint"`
}

type kubeSetOut struct {
	Version  string           `json:"version"`
	Bindings []kubeBindingOut `json:"bindings"`
}

// ------------------------------------------------------- internal shape --

// kubeAuth is a binding's credential; exactly the fields of its Type are
// set. Secret material: never logged, never rendered.
type kubeAuth struct {
	Type    string `json:"type"`
	Token   string `json:"token,omitempty"`
	CertPEM string `json:"cert_pem,omitempty"`
	KeyPEM  string `json:"key_pem,omitempty"`
}

// kubeBinding is one cluster a sandbox reaches through 127.0.0.1:Port. Its
// JSON (snake_case) is what the set's version hashes.
type kubeBinding struct {
	Name       string   `json:"name"`
	Port       int      `json:"port"`
	Server     string   `json:"server"`
	ServerName string   `json:"server_name,omitempty"`
	CAPEM      string   `json:"ca_pem,omitempty"`
	Auth       kubeAuth `json:"auth"`
}

func (b kubeBinding) proto() *fleetv1.KubeBinding {
	p := &fleetv1.KubeBinding{Name: b.Name, Port: uint32(b.Port), Server: b.Server, ServerName: b.ServerName,
		CaPem: b.CAPEM}
	if b.Auth.Type == kubeAuthToken {
		p.Credential = &fleetv1.KubeBinding_Token{Token: b.Auth.Token}
	} else {
		p.Credential = &fleetv1.KubeBinding_ClientCertificate{ClientCertificate: &fleetv1.ClientCertificate{
			CertPem: b.Auth.CertPEM, KeyPem: b.Auth.KeyPEM}}
	}
	return p
}

// kubeVersion is the deterministic version of a set: "" for the empty
// set, else the lowercase hex SHA-256 of the JSON of the bindings sorted
// by port.
func kubeVersion(bs []kubeBinding) string {
	if len(bs) == 0 {
		return ""
	}
	sorted := append([]kubeBinding(nil), bs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Port < sorted[j].Port })
	// Plain strings and ints: Marshal cannot fail.
	raw, _ := json.Marshal(sorted)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// kubeFingerprint identifies a credential without revealing it: the first
// 16 hex of the SHA-256 of the token, or of the certificate PEM followed
// by the key PEM.
func kubeFingerprint(a kubeAuth) string {
	var sum [32]byte
	if a.Type == kubeAuthToken {
		sum = sha256.Sum256([]byte(a.Token))
	} else {
		sum = sha256.Sum256([]byte(a.CertPEM + a.KeyPEM))
	}
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// ------------------------------------------------------------ validation --

// nameRe is a binding's name: a DNS label.
var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// hostRe is a DNS name, loosely: what may stand in a URL host or a TLS
// server name.
var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

func validHost(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	return hostRe.MatchString(h)
}

// validateKubeBinding checks one binding without touching the network and
// returns it in its internal shape with the server's host, for the
// destination rule. named: a PUT, where name and port are part of the
// binding; a preflight ignores both.
func validateKubeBinding(in kubeBindingIn, named bool) (kubeBinding, string, *bindingErr) {
	b := kubeBinding{Server: in.Server, ServerName: in.ServerName, CAPEM: in.CaPem,
		Auth: kubeAuth{Type: in.Auth.Type, Token: in.Auth.Token, CertPEM: in.Auth.CertPem, KeyPEM: in.Auth.KeyPem}}
	// The version hashes the JSON of exactly these strings; text that JSON
	// would rewrite has no place in a binding.
	for _, f := range []string{in.Name, in.Server, in.ServerName, in.CaPem,
		in.Auth.Type, in.Auth.Token, in.Auth.CertPem, in.Auth.KeyPem} {
		if !utf8.ValidString(f) {
			return b, "", bindingErrf(codeInvalidBinding, "binding fields must be valid UTF-8")
		}
	}
	if named {
		if !nameRe.MatchString(in.Name) {
			return b, "", bindingErrf(codeInvalidBinding,
				"name %q must be a DNS label: lowercase letters, digits and '-', at most 63", in.Name)
		}
		if in.Port < 1024 || in.Port > 65535 {
			return b, "", bindingErrf(codeInvalidBinding, "port %d must be between 1024 and 65535", in.Port)
		}
		if in.Port >= 10000 && in.Port <= 10002 {
			return b, "", bindingErrf(codeInvalidBinding,
				"port %d is reserved (10000–10002 are the sandbox's own vsock ports)", in.Port)
		}
		b.Name, b.Port = in.Name, in.Port
	}

	// server: an https URL naming a host and nothing else.
	if in.Server == "" {
		return b, "", bindingErrf(codeInvalidBinding, "server is required")
	}
	u, err := url.Parse(in.Server)
	if err != nil {
		return b, "", bindingErrf(codeInvalidBinding, "server %q is not a URL: %v", in.Server, err)
	}
	if u.Scheme != "https" {
		return b, "", bindingErrf(codeInsecureServer,
			"server %q must be an https:// URL: the credential would cross the network in clear", in.Server)
	}
	host := u.Hostname()
	switch {
	case u.Opaque != "" || host == "":
		return b, "", bindingErrf(codeInvalidBinding, "server %q names no host", in.Server)
	case u.User != nil:
		return b, "", bindingErrf(codeInvalidBinding, "server must not carry credentials in the URL")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return b, "", bindingErrf(codeInvalidBinding, "server %q must not have a query or fragment", in.Server)
	case u.Path != "" && u.Path != "/":
		return b, "", bindingErrf(codeInvalidBinding,
			"server %q must not have a path (an API server behind a path prefix is not supported)", in.Server)
	case !validHost(host):
		return b, "", bindingErrf(codeInvalidBinding, "server host %q is not a host name or address", host)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return b, "", bindingErrf(codeInvalidBinding, "server port %q is not a port", p)
		}
	}
	if in.ServerName != "" && !validHost(in.ServerName) {
		return b, "", bindingErrf(codeInvalidBinding, "serverName %q is not a host name or address", in.ServerName)
	}

	if in.CaPem != "" {
		if err := checkCAPEM(in.CaPem); err != nil {
			return b, "", bindingErrf(codeBadCA, "caPem: %v", err)
		}
	}

	switch in.Auth.Type {
	case kubeAuthToken:
		if in.Auth.Token == "" {
			return b, "", bindingErrf(codeInvalidBinding, "auth.token is required for auth type token")
		}
		if in.Auth.CertPem != "" || in.Auth.KeyPem != "" {
			return b, "", bindingErrf(codeInvalidBinding, "auth type token takes no certPem/keyPem")
		}
		for i := 0; i < len(in.Auth.Token); i++ {
			if c := in.Auth.Token[i]; c < 0x21 || c > 0x7e {
				// It becomes an Authorization header.
				return b, "", bindingErrf(codeInvalidBinding, "auth.token must be printable ASCII without spaces")
			}
		}
	case kubeAuthClientCert:
		if in.Auth.CertPem == "" || in.Auth.KeyPem == "" {
			return b, "", bindingErrf(codeInvalidBinding,
				"auth.certPem and auth.keyPem are required for auth type client_cert")
		}
		if in.Auth.Token != "" {
			return b, "", bindingErrf(codeInvalidBinding, "auth type client_cert takes no token")
		}
		if _, err := tls.X509KeyPair([]byte(in.Auth.CertPem), []byte(in.Auth.KeyPem)); err != nil {
			return b, "", bindingErrf(codeBadClientCert, "client certificate: %v", err)
		}
	case "":
		return b, "", bindingErrf(codeInvalidBinding, "auth.type is required (token or client_cert)")
	default:
		return b, "", bindingErrf(codeInvalidBinding, "auth.type %q is not supported (token or client_cert)", in.Auth.Type)
	}
	return b, host, nil
}

// checkCAPEM requires at least one certificate and nothing that is not
// one: a CA bundle with a stray key in it is a pasting accident.
func checkCAPEM(s string) error {
	rest, n := []byte(s), 0
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			return fmt.Errorf("holds a %s block; only certificates belong here", blk.Type)
		}
		if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
			return fmt.Errorf("certificate %d: %v", n+1, err)
		}
		n++
	}
	if n == 0 {
		return errors.New("holds no PEM certificate")
	}
	return nil
}

// validateKubeSet checks a PUT's whole set: every binding, then the
// uniqueness of names and ports and the size, and only then, when
// everything local is fine, the destination rule, which resolves names.
func (s *Service) validateKubeSet(ctx context.Context, ins []kubeBindingIn) ([]kubeBinding, *bindingErr) {
	if len(ins) > maxKubeBindings {
		return nil, bindingErrf(codeInvalidBinding, "at most %d bindings per sandbox", maxKubeBindings)
	}
	out := make([]kubeBinding, 0, len(ins))
	hosts := make([]string, 0, len(ins))
	names, ports := map[string]bool{}, map[int]bool{}
	for i, in := range ins {
		b, host, kerr := validateKubeBinding(in, true)
		if kerr != nil {
			kerr.msg = fmt.Sprintf("bindings[%d]: %s", i, kerr.msg)
			return nil, kerr
		}
		if names[b.Name] {
			return nil, bindingErrf(codeInvalidBinding, "bindings[%d]: duplicate name %q", i, b.Name)
		}
		if ports[b.Port] {
			return nil, bindingErrf(codeInvalidBinding, "bindings[%d]: duplicate port %d", i, b.Port)
		}
		names[b.Name], ports[b.Port] = true, true
		out = append(out, b)
		hosts = append(hosts, host)
	}
	if raw, err := json.Marshal(struct {
		Version  string        `json:"version"`
		Bindings []kubeBinding `json:"bindings"`
	}{kubeVersion(out), out}); err != nil || len(raw) > maxSetBytes {
		return nil, bindingErrf(codeInvalidBinding,
			"the set is too large (its CA bundles and certificates must fit in %d KiB)", maxSetBytes>>10)
	}
	checked := map[string]bool{}
	for i, host := range hosts {
		if checked[host] {
			continue
		}
		if kerr := s.dest.checkDestination(ctx, host); kerr != nil {
			kerr.msg = fmt.Sprintf("bindings[%d] (%s): %s", i, out[i].Name, kerr.msg)
			return nil, kerr
		}
		checked[host] = true
	}
	return out, nil
}

// --------------------------------------------------------------- at rest --

// kubeSecret is the sealed part of a binding: its auth without the type,
// which stays a plain column because GET reports it.
type kubeSecret struct {
	Token   string `json:"token,omitempty"`
	CertPEM string `json:"cert_pem,omitempty"`
	KeyPEM  string `json:"key_pem,omitempty"`
}

func (s *Service) sealKube(id podid.ID, b kubeBinding) (store.KubeBinding, error) {
	pt, err := json.Marshal(kubeSecret{Token: b.Auth.Token, CertPEM: b.Auth.CertPEM, KeyPEM: b.Auth.KeyPEM})
	if err != nil {
		return store.KubeBinding{}, err
	}
	sealed, err := s.sealer.Seal(pt, sealAAD(tableKube, id, b.Name, b.Auth.Type))
	if err != nil {
		return store.KubeBinding{}, fmt.Errorf("seal binding %s: %w", b.Name, err)
	}
	return store.KubeBinding{Name: b.Name, Port: b.Port, Server: b.Server, ServerName: b.ServerName,
		CAPEM: b.CAPEM, AuthType: b.Auth.Type, Sealed: sealed, Fingerprint: kubeFingerprint(b.Auth)}, nil
}

func (s *Service) openKubeRow(id podid.ID, r store.KubeBinding) (kubeBinding, error) {
	pt, err := s.sealer.Open(r.Sealed, sealAAD(tableKube, id, r.Name, r.AuthType))
	if err != nil {
		return kubeBinding{}, err
	}
	var sec kubeSecret
	if err := json.Unmarshal(pt, &sec); err != nil {
		return kubeBinding{}, fmt.Errorf("sealed credential: %v", err)
	}
	return kubeBinding{Name: r.Name, Port: r.Port, Server: r.Server, ServerName: r.ServerName, CAPEM: r.CAPEM,
		Auth: kubeAuth{Type: r.AuthType, Token: sec.Token, CertPEM: sec.CertPEM, KeyPEM: sec.KeyPEM}}, nil
}

// openKube opens a stored set. The version is recomputed from the opened
// bindings and has to equal the stored one, which is the version a pod is
// told it holds: a set that hashed differently is an error here.
func (s *Service) openKube(id podid.ID, version string, rows []store.KubeBinding) ([]kubeBinding, error) {
	bs := make([]kubeBinding, 0, len(rows))
	for _, r := range rows {
		b, err := s.openKubeRow(id, r)
		if err != nil {
			return nil, fmt.Errorf("kube binding %s of %s: %w", r.Name, id, err)
		}
		bs = append(bs, b)
	}
	if kubeVersion(bs) != version {
		return nil, fmt.Errorf("kube bindings of %s do not match their recorded version; PUT the set again", id)
	}
	return bs, nil
}

// applyKube stores a validated set (nil: none) under the sandbox's row
// lock and returns the set as stored, read in the same transaction, so a
// PUT answers its own write and never a later one from another instance.
// The same set again is a no-op, unless what is stored no longer opens (a
// changed key): then the PUT is the repair.
func (s *Service) applyKube(ctx context.Context, id podid.ID, bs []kubeBinding) (kubeSetOut, error) {
	v := kubeVersion(bs)
	var res kubeSetOut
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		if _, err := tx.LockSandbox(ctx, id, store.ForUpdate); err != nil {
			if errors.Is(err, model.ErrNotFound) {
				return noSuchSandbox()
			}
			return err
		}
		stored, rows, err := tx.KubeBindings(ctx, id)
		if err != nil {
			return err
		}
		if stored == v {
			if _, err := s.openKube(id, stored, rows); err == nil {
				res = kubeSetOf(stored, rows)
				return nil
			}
		}
		out := make([]store.KubeBinding, 0, len(bs))
		for _, b := range bs {
			row, err := s.sealKube(id, b)
			if err != nil {
				return err
			}
			out = append(out, row)
		}
		if err := tx.SetKubeBindings(ctx, id, v, out); err != nil {
			return err
		}
		if stored, rows, err = tx.KubeBindings(ctx, id); err != nil {
			return err
		}
		res = kubeSetOf(stored, rows)
		return nil
	})
	return res, err
}

// -------------------------------------------------------------- handlers --

// kubeSet renders the stored set: names, servers and fingerprints, never
// the material.
func (s *Service) kubeSet(ctx context.Context, id podid.ID) (kubeSetOut, error) {
	version, rows, err := s.st.KubeBindings(ctx, id)
	if errors.Is(err, model.ErrNotFound) {
		return kubeSetOut{}, noSuchSandbox()
	}
	if err != nil {
		return kubeSetOut{}, fmt.Errorf("kube bindings %s: %w", id, err)
	}
	return kubeSetOf(version, rows), nil
}

// kubeSetOf renders stored rows: names, servers and fingerprints, never
// the material.
func kubeSetOf(version string, rows []store.KubeBinding) kubeSetOut {
	out := kubeSetOut{Version: version, Bindings: []kubeBindingOut{}}
	for _, r := range rows {
		out.Bindings = append(out.Bindings, kubeBindingOut{Name: r.Name, Port: r.Port, Server: r.Server,
			ServerName: r.ServerName, HasCa: r.CAPEM != "", AuthType: r.AuthType, Fingerprint: r.Fingerprint})
	}
	return out
}

func (s *Service) writeKubeSet(w http.ResponseWriter, r *http.Request, id podid.ID) {
	out, err := s.kubeSet(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}

// GetKube answers the sandbox's Kubernetes set without secret material.
func (s *Service) GetKube(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	id, aerr := sandboxOf(r)
	if aerr != nil {
		apierr.Write(w, aerr)
		return
	}
	s.writeKubeSet(w, r, id)
}

// PutKube replaces the sandbox's Kubernetes set and answers it as GET
// does.
func (s *Service) PutKube(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	id, aerr := sandboxOf(r)
	if aerr != nil {
		apierr.Write(w, aerr)
		return
	}
	var body struct {
		Bindings []kubeBindingIn `json:"bindings"`
	}
	sent, err := decodeJSON(r, &body)
	if err != nil {
		apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
		return
	}
	if _, ok := sent["bindings"]; !ok {
		// A body without the field is far more likely a client bug than a
		// wish to drop every cluster; DELETE says that explicitly.
		apierr.Write(w, bindingErrf(codeInvalidBinding,
			"bindings is required: the complete set, [] for none").as(http.StatusBadRequest))
		return
	}
	bs, kerr := s.validateKubeSet(r.Context(), body.Bindings)
	if kerr != nil {
		apierr.Write(w, kerr.as(http.StatusBadRequest))
		return
	}
	out, err := s.applyKube(r.Context(), id, bs)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.afterApply != nil {
		s.afterApply()
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}

// DeleteKube removes the sandbox's Kubernetes set: 204.
func (s *Service) DeleteKube(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	id, aerr := sandboxOf(r)
	if aerr != nil {
		apierr.Write(w, aerr)
		return
	}
	if _, err := s.applyKube(r.Context(), id, nil); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// KubePreflight checks one binding against its cluster before any sandbox
// has it: does the credential work, and as whom. Nothing is stored.
func (s *Service) KubePreflight(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	var in kubeBindingIn
	if _, err := decodeJSON(r, &in); err != nil {
		apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
		return
	}
	b, host, kerr := validateKubeBinding(in, false)
	if kerr == nil {
		kerr = s.dest.checkDestination(r.Context(), host)
	}
	if kerr == nil {
		var res *kubePreflightResult
		if res, kerr = s.kubePreflight(r.Context(), b); kerr == nil {
			apierr.WriteJSON(w, http.StatusOK, res)
			return
		}
	}
	if kerr.code == codeInvalidBinding {
		apierr.Write(w, kerr.as(http.StatusBadRequest)) // the request, not the cluster
		return
	}
	apierr.Write(w, kerr.as(http.StatusUnprocessableEntity))
}
