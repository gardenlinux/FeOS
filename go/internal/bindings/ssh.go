// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"context"
	"crypto"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The SSH surface, the Kubernetes surface's twin:
//
//	PUT    /api/v1/sandboxes/{id}/ssh              sandbox  the complete binding set
//	GET    /api/v1/sandboxes/{id}/ssh              sandbox  the set without secret material
//	DELETE /api/v1/sandboxes/{id}/ssh              sandbox  no bindings
//	POST   /api/v1/sandboxes/{id}/ssh/{name}/check sandbox  a stored binding: does it work now
//	POST   /api/v1/ssh/preflight                   project   one binding, no sandbox
//
// What differs from Kubernetes is that an SSH key has a public half meant
// to be shown: GET answers every binding's public key line, ready for
// authorized_keys or a deploy-key form, and with "generate" that line is
// the whole setup. A generated key belongs to its binding's name in its
// sandbox and is kept across PUTs while that binding stays "generate".

// The auth types of an SSH binding.
const (
	sshAuthPrivateKey = "private_key"
	sshAuthGenerate   = "generate"
)

// maxSSHBindings bounds a set, like Kubernetes': a sanity cap.
const maxSSHBindings = 32

// maxSSHHostKeys bounds one binding's host keys: room for rotation.
const maxSSHHostKeys = 16

// ------------------------------------------------------------ wire types --

type sshAuthIn struct {
	Type       string `json:"type"`
	KeyPem     string `json:"keyPem"`
	Passphrase string `json:"passphrase"`
}

type sshBindingIn struct {
	Name      string     `json:"name"`
	Server    string     `json:"server"`
	User      string     `json:"user"`
	HostKeys  []string   `json:"hostKeys"`
	AnyServer bool       `json:"anyServer"`
	Auth      *sshAuthIn `json:"auth"`
}

type sshHostKeyOut struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
}

type sshBindingOut struct {
	Name        string          `json:"name"`
	Server      string          `json:"server"`
	User        string          `json:"user"`
	HostKeys    []sshHostKeyOut `json:"hostKeys"`
	AnyServer   bool            `json:"anyServer"`
	AuthType    string          `json:"authType"`
	KeyType     string          `json:"keyType"`
	Fingerprint string          `json:"fingerprint"`
	PublicKey   string          `json:"publicKey"`
}

type sshSetOut struct {
	Version  string          `json:"version"`
	Bindings []sshBindingOut `json:"bindings"`
}

// ------------------------------------------------------- internal shape --

// sshBinding is one destination the pod's agent signs for: a login as User
// with PrivateKey on a server that proves it holds one of HostKeys, or,
// with AnyServer, a key for any server (no user, no host keys). The
// server address is ferund's business only. Its JSON is what the set's
// version hashes.
type sshBinding struct {
	Name string `json:"name"`
	User string `json:"user"`
	// "<type> <base64>"; never empty for a pinned binding, so omitempty
	// changes no version.
	HostKeys  []string `json:"host_keys,omitempty"`
	AnyServer bool     `json:"any_server,omitempty"`
	// Unencrypted OpenSSH. Secret material.
	PrivateKey string `json:"private_key"`
}

// sshStored is one binding as stored: the agent's binding plus what only
// ferund knows (the server, how the key came to be) and the key's public
// facts.
type sshStored struct {
	sshBinding
	server   string
	authType string
	facts    sshKeyFacts
}

func (b sshStored) proto() *fleetv1.SshBinding {
	return &fleetv1.SshBinding{Name: b.Name, Server: b.server, User: b.User,
		HostKeys: append([]string(nil), b.HostKeys...), PrivateKey: b.PrivateKey}
}

// sshVersion is the deterministic version of a set: "" for the empty set,
// else the lowercase hex SHA-256 of the JSON of the bindings sorted by
// name.
func sshVersion(bs []sshBinding) string {
	if len(bs) == 0 {
		return ""
	}
	sorted := append([]sshBinding(nil), bs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	// Plain strings and bools: Marshal cannot fail.
	raw, _ := json.Marshal(sorted)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func sshBindingsOf(bs []sshStored) []sshBinding {
	out := make([]sshBinding, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.sshBinding)
	}
	return out
}

// ------------------------------------------------------------ validation --

// sshUserRe is a login name.
var sshUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)

// sshValidated is one binding after local validation: everything a PUT
// stores and a preflight dials, the key parsed (nil for generate or for a
// preflight without auth).
type sshValidated struct {
	name         string
	server       string
	host         string
	port         int
	user         string
	hostKeys     []ssh.PublicKey
	hostKeyLines []string
	anyServer    bool
	authType     string // "": a preflight without auth
	privateKey   crypto.PrivateKey
}

// parseSSHServer reads a server: ssh://host[:port], port 22 if absent,
// nothing else in the URL.
func parseSSHServer(server string) (string, int, *bindingErr) {
	if server == "" {
		return "", 0, bindingErrf(codeInvalidBinding, "server is required")
	}
	u, err := url.Parse(server)
	if err != nil {
		return "", 0, bindingErrf(codeInvalidBinding, "server %q is not a URL: %v", server, err)
	}
	host := u.Hostname()
	switch {
	case u.Scheme != "ssh":
		return "", 0, bindingErrf(codeInvalidBinding, "server %q must be an ssh:// URL", server)
	case u.Opaque != "" || host == "":
		return "", 0, bindingErrf(codeInvalidBinding, "server %q names no host", server)
	case u.User != nil:
		return "", 0, bindingErrf(codeInvalidBinding, "server must not carry a user; the login name is the binding's user")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", 0, bindingErrf(codeInvalidBinding, "server %q must not have a query or fragment", server)
	case u.Path != "":
		return "", 0, bindingErrf(codeInvalidBinding, "server %q must not have a path", server)
	case !validHost(host):
		return "", 0, bindingErrf(codeInvalidBinding, "server host %q is not a host name or address", host)
	}
	port := 22
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, bindingErrf(codeInvalidBinding, "server port %q is not a port", p)
		}
		port = n
	}
	return host, port, nil
}

// validateSSHBinding checks one binding without touching the network.
// named: a PUT, where name, hostKeys and auth are required and auth may be
// generate; a preflight ignores the name and takes hostKeys and an
// uploaded key optionally.
func validateSSHBinding(in sshBindingIn, named bool) (sshValidated, *bindingErr) {
	v := sshValidated{server: in.Server, user: in.User}
	fields := append([]string{in.Name, in.Server, in.User}, in.HostKeys...)
	if in.Auth != nil {
		fields = append(fields, in.Auth.Type, in.Auth.KeyPem, in.Auth.Passphrase)
	}
	for _, f := range fields {
		if !utf8.ValidString(f) {
			return v, bindingErrf(codeInvalidBinding, "binding fields must be valid UTF-8")
		}
	}
	if named {
		if !nameRe.MatchString(in.Name) {
			return v, bindingErrf(codeInvalidBinding,
				"name %q must be a DNS label: lowercase letters, digits and '-', at most 63", in.Name)
		}
		v.name = in.Name
	}
	if in.AnyServer {
		switch {
		case !named:
			return v, bindingErrf(codeInvalidBinding,
				"anyServer belongs to a sandbox's binding; a preflight dials one server — give its server and user")
		case in.Server != "" || in.User != "" || len(in.HostKeys) > 0:
			return v, bindingErrf(codeInvalidBinding, "an anyServer binding takes no server, user or hostKeys")
		}
		v.anyServer = true
	} else if berr := validateSSHDestination(in, named, &v); berr != nil {
		return v, berr
	}

	if in.Auth == nil {
		if named {
			return v, bindingErrf(codeInvalidBinding,
				`auth is required: {"type":"private_key","keyPem":"…"} or {"type":"generate"}`)
		}
		return v, nil
	}
	switch in.Auth.Type {
	case sshAuthPrivateKey:
		if in.Auth.KeyPem == "" {
			return v, bindingErrf(codeInvalidBinding, "auth.keyPem is required for auth type private_key")
		}
		key, berr := parseSSHPrivateKey(in.Auth.KeyPem, in.Auth.Passphrase)
		if berr != nil {
			return v, berr
		}
		v.authType, v.privateKey = sshAuthPrivateKey, key
	case sshAuthGenerate:
		if !named {
			return v, bindingErrf(codeInvalidBinding,
				"a preflight takes an uploaded key or none; a generated key exists only in a "+
					"sandbox's binding — check it with POST …/ssh/{name}/check")
		}
		if in.Auth.KeyPem != "" || in.Auth.Passphrase != "" {
			return v, bindingErrf(codeInvalidBinding, "auth type generate takes no keyPem or passphrase")
		}
		v.authType = sshAuthGenerate
	case "":
		return v, bindingErrf(codeInvalidBinding, "auth.type is required (private_key or generate)")
	default:
		return v, bindingErrf(codeInvalidBinding, "auth.type %q is not supported (private_key or generate)", in.Auth.Type)
	}
	return v, nil
}

// validateSSHDestination checks where a pinned binding (or a preflight)
// logs in: server, user and host keys, required in a PUT, the host keys
// optional in a preflight.
func validateSSHDestination(in sshBindingIn, named bool, v *sshValidated) *bindingErr {
	host, port, berr := parseSSHServer(in.Server)
	if berr != nil {
		return berr
	}
	v.host, v.port = host, port
	if in.User == "" {
		return bindingErrf(codeInvalidBinding, "user is required")
	}
	if !sshUserRe.MatchString(in.User) {
		return bindingErrf(codeInvalidBinding,
			"user %q is not a login name (lowercase letters, digits, '_', '.', '-'; at most 32)", in.User)
	}
	if named && len(in.HostKeys) == 0 {
		return bindingErrf(codeInvalidBinding,
			"hostKeys is required: at least one public key of the server (a preflight without "+
				"hostKeys reports the one it presents), or anyServer for a key without a destination")
	}
	if len(in.HostKeys) > maxSSHHostKeys {
		return bindingErrf(codeInvalidBinding, "at most %d hostKeys per binding", maxSSHHostKeys)
	}
	for i, line := range in.HostKeys {
		pub, norm, err := parseSSHPublicKeyLine(line)
		if err != nil {
			return bindingErrf(codeBadHostKey, "hostKeys[%d]: %v", i, err)
		}
		v.hostKeys = append(v.hostKeys, pub)
		v.hostKeyLines = append(v.hostKeyLines, norm)
	}
	return nil
}

// validateSSHSet checks a PUT's whole set: every binding, the uniqueness
// of names, and only then the destination rule, which resolves names.
func (s *Service) validateSSHSet(ctx context.Context, ins []sshBindingIn) ([]sshValidated, *bindingErr) {
	if len(ins) > maxSSHBindings {
		return nil, bindingErrf(codeInvalidBinding, "at most %d bindings per sandbox", maxSSHBindings)
	}
	out := make([]sshValidated, 0, len(ins))
	names := map[string]bool{}
	for i, in := range ins {
		v, berr := validateSSHBinding(in, true)
		if berr != nil {
			berr.msg = fmt.Sprintf("bindings[%d]: %s", i, berr.msg)
			return nil, berr
		}
		if names[v.name] {
			return nil, bindingErrf(codeInvalidBinding, "bindings[%d]: duplicate name %q", i, v.name)
		}
		names[v.name] = true
		out = append(out, v)
	}
	checked := map[string]bool{}
	for i, v := range out {
		if v.anyServer || checked[v.host] {
			continue // a key for any server names no host
		}
		if berr := s.dest.checkDestination(ctx, v.host); berr != nil {
			berr.msg = fmt.Sprintf("bindings[%d] (%s): %s", i, v.name, berr.msg)
			return nil, berr
		}
		checked[v.host] = true
	}
	return out, nil
}

// --------------------------------------------------------------- at rest --

// sshSecret is the sealed part of a binding: the private key.
type sshSecret struct {
	PrivateKey string `json:"private_key"`
}

func (s *Service) sealSSH(id podid.ID, b sshStored) (store.SSHBinding, error) {
	pt, err := json.Marshal(sshSecret{PrivateKey: b.PrivateKey})
	if err != nil {
		return store.SSHBinding{}, err
	}
	sealed, err := s.sealer.Seal(pt, sealAAD(tableSSH, id, b.Name, b.authType))
	if err != nil {
		return store.SSHBinding{}, fmt.Errorf("seal binding %s: %w", b.Name, err)
	}
	return store.SSHBinding{Name: b.Name, Server: b.server, User: b.User, HostKeys: b.HostKeys,
		AuthType: b.authType, KeyType: b.facts.keyType, Fingerprint: b.facts.fingerprint,
		PublicKey: b.facts.publicKey, Sealed: sealed}, nil
}

func (s *Service) openSSHKey(id podid.ID, r store.SSHBinding) (string, error) {
	pt, err := s.sealer.Open(r.Sealed, sealAAD(tableSSH, id, r.Name, r.AuthType))
	if err != nil {
		return "", err
	}
	var sec sshSecret
	if err := json.Unmarshal(pt, &sec); err != nil {
		return "", fmt.Errorf("sealed credential: %v", err)
	}
	if sec.PrivateKey == "" {
		return "", errors.New("sealed credential holds no key")
	}
	return sec.PrivateKey, nil
}

// openSSH opens a stored set, rows sorted by name. The version is
// recomputed from the opened bindings and has to equal the stored one.
func (s *Service) openSSH(id podid.ID, version string, rows []store.SSHBinding) ([]sshStored, error) {
	bs := make([]sshStored, 0, len(rows))
	for _, r := range rows {
		key, err := s.openSSHKey(id, r)
		if err != nil {
			return nil, fmt.Errorf("ssh binding %s of %s: %w", r.Name, id, err)
		}
		// A binding for any server is stored without a server.
		bs = append(bs, sshStored{
			sshBinding: sshBinding{Name: r.Name, User: r.User, HostKeys: append([]string{}, r.HostKeys...),
				AnyServer: r.Server == "", PrivateKey: key},
			server: r.Server, authType: r.AuthType,
			facts: sshKeyFacts{keyType: r.KeyType, fingerprint: r.Fingerprint, publicKey: r.PublicKey},
		})
	}
	if sshVersion(sshBindingsOf(bs)) != version {
		return nil, fmt.Errorf("ssh bindings of %s do not match their recorded version; PUT the set again", id)
	}
	return bs, nil
}

// errSSHStoredKey marks a generated key that is stored but does not open
// any more: it cannot be kept, and minting a new one in its place would
// silently invalidate the line the person registered.
var errSSHStoredKey = errors.New("stored generated key does not open")

// resolveSSH turns a validated PUT into the set to store: an uploaded key
// normalised, a generated one kept from the stored binding of the same
// name when that one was generate too, or made fresh. existing are the
// rows stored now. The result is sorted by name, the version's order.
func (s *Service) resolveSSH(id podid.ID, vs []sshValidated, existing []store.SSHBinding) ([]sshStored, error) {
	kept := map[string]store.SSHBinding{}
	for _, r := range existing {
		if r.AuthType == sshAuthGenerate {
			kept[r.Name] = r
		}
	}
	out := make([]sshStored, 0, len(vs))
	for _, v := range vs {
		comment := sshKeyComment(v.name, id.String())
		var key string
		var err error
		switch v.authType {
		case sshAuthPrivateKey:
			key, err = normalizeSSHPrivateKey(v.privateKey, comment)
		case sshAuthGenerate:
			if r, ok := kept[v.name]; ok {
				if key, err = s.openSSHKey(id, r); err != nil {
					return nil, fmt.Errorf("binding %s: %w (%v); DELETE the set and PUT it again for a new key",
						v.name, errSSHStoredKey, err)
				}
			} else {
				key, err = generateSSHKey(comment)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("binding %s: key: %w", v.name, err)
		}
		facts, err := sshFactsOf(key, comment)
		if err != nil {
			return nil, fmt.Errorf("binding %s: normalized key does not parse: %w", v.name, err)
		}
		out = append(out, sshStored{
			sshBinding: sshBinding{Name: v.name, User: v.user, HostKeys: v.hostKeyLines, AnyServer: v.anyServer,
				PrivateKey: key},
			server: v.server, authType: v.authType, facts: facts,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// sameStored reports whether rows (as stored, sorted by name) carry the
// same non-secret columns as bs: with an equal version, a change of the
// server alone, which the version does not cover, is still a change to
// store.
func sameStored(rows []store.SSHBinding, bs []sshStored) bool {
	if len(rows) != len(bs) {
		return false
	}
	for i, r := range rows {
		b := bs[i]
		if r.Name != b.Name || r.Server != b.server || r.AuthType != b.authType || r.PublicKey != b.facts.publicKey {
			return false
		}
	}
	return true
}

// applySSH stores a validated set (nil: none) under the sandbox's row
// lock, which serialises PUTs and DELETEs so that two of them never mint a
// key for the same binding. Keys are resolved against what is stored, so
// "the same set" is decided on the resolved bindings: the same version and
// the same non-secret columns is a no-op. A stored set that no longer
// opens is rewritten: the PUT is the repair. It returns the set as stored,
// read in the same transaction, so a PUT answers its own write and never a
// later one from another instance.
func (s *Service) applySSH(ctx context.Context, id podid.ID, vs []sshValidated) (sshSetOut, error) {
	var res sshSetOut
	err := s.st.InTx(ctx, func(tx *store.Store) error {
		if _, err := tx.LockSandbox(ctx, id, store.ForUpdate); err != nil {
			if errors.Is(err, model.ErrNotFound) {
				return noSuchSandbox()
			}
			return err
		}
		stored, rows, err := tx.SSHBindings(ctx, id)
		if err != nil {
			return err
		}
		bs, err := s.resolveSSH(id, vs, rows)
		if err != nil {
			if errors.Is(err, errSSHStoredKey) {
				return apierr.New(http.StatusConflict, "%v", err)
			}
			return err
		}
		set := sshBindingsOf(bs)
		version := sshVersion(set)
		if raw, err := json.Marshal(struct {
			Version  string       `json:"version"`
			Bindings []sshBinding `json:"bindings"`
		}{version, set}); err != nil || len(raw) > maxSetBytes {
			return bindingErrf(codeInvalidBinding,
				"the set is too large (its keys must fit in %d KiB)", maxSetBytes>>10).as(http.StatusBadRequest)
		}
		if version == stored && sameStored(rows, bs) {
			if _, err := s.openSSH(id, stored, rows); err == nil {
				res = sshSetOf(stored, rows)
				return nil
			}
		}
		out := make([]store.SSHBinding, 0, len(bs))
		for _, b := range bs {
			row, err := s.sealSSH(id, b)
			if err != nil {
				return err
			}
			out = append(out, row)
		}
		if err := tx.SetSSHBindings(ctx, id, version, out); err != nil {
			return err
		}
		if stored, rows, err = tx.SSHBindings(ctx, id); err != nil {
			return err
		}
		res = sshSetOf(stored, rows)
		return nil
	})
	return res, err
}

// -------------------------------------------------------------- handlers --

// sshSetOf renders stored rows: names, servers, fingerprints and public
// key lines, never a private key.
func sshSetOf(version string, rows []store.SSHBinding) sshSetOut {
	out := sshSetOut{Version: version, Bindings: []sshBindingOut{}}
	for _, r := range rows {
		hks := []sshHostKeyOut{}
		for _, line := range r.HostKeys {
			hk := sshHostKeyOut{}
			if pub, _, err := parseSSHPublicKeyLine(line); err == nil {
				hk = sshHostKeyOut{Type: pub.Type(), Fingerprint: ssh.FingerprintSHA256(pub)}
			}
			hks = append(hks, hk)
		}
		out.Bindings = append(out.Bindings, sshBindingOut{
			Name: r.Name, Server: r.Server, User: r.User, HostKeys: hks, AnyServer: r.Server == "",
			AuthType: r.AuthType, KeyType: r.KeyType, Fingerprint: r.Fingerprint, PublicKey: r.PublicKey,
		})
	}
	return out
}

func (s *Service) sshSet(ctx context.Context, id podid.ID) (sshSetOut, error) {
	version, rows, err := s.st.SSHBindings(ctx, id)
	if errors.Is(err, model.ErrNotFound) {
		return sshSetOut{}, noSuchSandbox()
	}
	if err != nil {
		return sshSetOut{}, fmt.Errorf("ssh bindings %s: %w", id, err)
	}
	return sshSetOf(version, rows), nil
}

func (s *Service) writeSSHSet(w http.ResponseWriter, r *http.Request, id podid.ID) {
	out, err := s.sshSet(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}

// GetSSH answers the sandbox's SSH set without secret material.
func (s *Service) GetSSH(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	id, aerr := sandboxOf(r)
	if aerr != nil {
		apierr.Write(w, aerr)
		return
	}
	s.writeSSHSet(w, r, id)
}

// PutSSH replaces the sandbox's SSH set and answers it as GET does.
func (s *Service) PutSSH(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	id, aerr := sandboxOf(r)
	if aerr != nil {
		apierr.Write(w, aerr)
		return
	}
	var body struct {
		Bindings []sshBindingIn `json:"bindings"`
	}
	sent, err := decodeJSON(r, &body)
	if err != nil {
		apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
		return
	}
	if _, ok := sent["bindings"]; !ok {
		// A body without the field is far more likely a client bug than a
		// wish to drop every server; DELETE says that explicitly.
		apierr.Write(w, bindingErrf(codeInvalidBinding,
			"bindings is required: the complete set, [] for none").as(http.StatusBadRequest))
		return
	}
	vs, berr := s.validateSSHSet(r.Context(), body.Bindings)
	if berr != nil {
		apierr.Write(w, berr.as(http.StatusBadRequest))
		return
	}
	out, err := s.applySSH(r.Context(), id, vs)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.afterApply != nil {
		s.afterApply()
	}
	apierr.WriteJSON(w, http.StatusOK, out)
}

// DeleteSSH removes the sandbox's SSH set, generated keys with it: 204.
func (s *Service) DeleteSSH(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	id, aerr := sandboxOf(r)
	if aerr != nil {
		apierr.Write(w, aerr)
		return
	}
	if _, err := s.applySSH(r.Context(), id, nil); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SSHPreflight checks one binding against its server before any sandbox
// has it: does the key log in, or, without a key, which host key does the
// server present. Nothing is stored.
func (s *Service) SSHPreflight(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	var in sshBindingIn
	if _, err := decodeJSON(r, &in); err != nil {
		apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
		return
	}
	v, berr := validateSSHBinding(in, false)
	if berr == nil {
		berr = s.dest.checkDestination(r.Context(), v.host)
	}
	if berr == nil {
		p := sshProbe{server: v.server, host: v.host, port: v.port, user: v.user, hostKeys: v.hostKeys}
		if v.privateKey != nil {
			signer, err := ssh.NewSignerFromKey(v.privateKey)
			if err != nil {
				berr = bindingErrf(codeBadKey, "the key is not usable for SSH: %v", err)
			}
			p.signer = signer
		}
		if berr == nil {
			var res *sshPreflightResult
			if res, berr = s.probe(r.Context(), p); berr == nil {
				apierr.WriteJSON(w, http.StatusOK, res)
				return
			}
		}
	}
	if berr.code == codeInvalidBinding {
		apierr.Write(w, berr.as(http.StatusBadRequest)) // the request, not the server
		return
	}
	apierr.Write(w, berr.as(http.StatusUnprocessableEntity))
}

// CheckSSH is the preflight for a stored binding ({name}): its server, user
// and key (the only way to try a generated key), its host keys always
// enforced. A binding for any server has none of those, so its check
// names them in the body, as a preflight does.
func (s *Service) CheckSSH(w http.ResponseWriter, r *http.Request) {
	if s.off(w, r) {
		return
	}
	id, aerr := sandboxOf(r)
	if aerr != nil {
		apierr.Write(w, aerr)
		return
	}
	name := r.PathValue("name")
	version, rows, err := s.st.SSHBindings(r.Context(), id)
	if errors.Is(err, model.ErrNotFound) {
		apierr.Write(w, noSuchSandbox())
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("ssh bindings %s: %w", id, err))
		return
	}
	set, err := s.openSSH(id, version, rows)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var b *sshStored
	for i := range set {
		if set[i].Name == name {
			b = &set[i]
		}
	}
	if b == nil {
		apierr.Write(w, bindingErrf(codeNoBinding, "the sandbox has no ssh binding %q", name).as(http.StatusNotFound))
		return
	}
	var p sshProbe
	var berr *bindingErr
	if b.AnyServer {
		var in sshBindingIn
		if _, err := decodeJSON(r, &in); err != nil {
			apierr.Write(w, bindingErrf(codeInvalidBinding, "%v", err).as(http.StatusBadRequest))
			return
		}
		var v sshValidated
		dest := sshBindingIn{Server: in.Server, User: in.User, HostKeys: in.HostKeys}
		if derr := validateSSHDestination(dest, false, &v); derr != nil {
			derr.msg = fmt.Sprintf("binding %s is for any server, so the check names one: %s", name, derr.msg)
			apierr.Write(w, derr.as(http.StatusBadRequest))
			return
		}
		if berr = s.dest.checkDestination(r.Context(), v.host); berr == nil {
			p = sshProbe{server: in.Server, host: v.host, port: v.port, user: in.User, hostKeys: v.hostKeys}
		}
	} else {
		host, port, perr := parseSSHServer(b.server)
		if berr = perr; berr == nil {
			berr = s.dest.checkDestination(r.Context(), host)
		}
		if berr == nil {
			p = sshProbe{server: b.server, host: host, port: port, user: b.User}
			for i, line := range b.HostKeys {
				pub, _, err := parseSSHPublicKeyLine(line)
				if err != nil {
					berr = bindingErrf(codeBadHostKey, "hostKeys[%d]: %v", i, err)
					break
				}
				p.hostKeys = append(p.hostKeys, pub)
			}
		}
	}
	if berr == nil {
		signer, err := ssh.ParsePrivateKey([]byte(b.PrivateKey))
		if err != nil {
			s.fail(w, r, fmt.Errorf("ssh binding %s of %s: stored key: %w", name, id, err))
			return
		}
		p.signer = signer
		var res *sshPreflightResult
		if res, berr = s.probe(r.Context(), p); berr == nil {
			apierr.WriteJSON(w, http.StatusOK, res)
			return
		}
	}
	apierr.Write(w, berr.as(http.StatusUnprocessableEntity))
}
