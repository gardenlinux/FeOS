// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/store"
)

// ------------------------------------------------------------------ keys --

func newEd25519(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newECDSA(t *testing.T, c elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// testRSA2048 is generated once: RSA key generation is the slow part of
// these tests.
var testRSA2048 = sync.OnceValues(func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) })

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := testRSA2048()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func opensshPEM(t *testing.T, raw crypto.PrivateKey) string {
	t.Helper()
	blk, err := ssh.MarshalPrivateKey(raw, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(blk))
}

func opensshPEMWithPassphrase(t *testing.T, raw crypto.PrivateKey, pass string) string {
	t.Helper()
	blk, err := ssh.MarshalPrivateKeyWithPassphrase(raw, "laptop", []byte(pass))
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(blk))
}

func pkcs8PEM(t *testing.T, raw crypto.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func sec1PEM(t *testing.T, k *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func pkcs1PEM(k *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

// encryptedPKCS1PEM is the legacy OpenSSL encryption (Proc-Type headers)
// older ssh-keygen versions wrote; people still upload it.
func encryptedPKCS1PEM(t *testing.T, k *rsa.PrivateKey, pass string) string {
	t.Helper()
	// x509.EncryptPEMBlock is deprecated as insecure, which is beside the
	// point: it is the format under test.
	blk, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(k),
		[]byte(pass), x509.PEMCipherAES256)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(blk))
}

func pubOf(t *testing.T, raw crypto.PrivateKey) ssh.PublicKey {
	t.Helper()
	s, err := ssh.NewSignerFromKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s.PublicKey()
}

// hostLine is a host key in authorized_keys form with a comment, as a
// person pastes it from ssh-keyscan or /etc/ssh/*.pub.
func hostLine(t *testing.T, raw crypto.PrivateKey) string {
	return sshKeyLine(pubOf(t, raw)) + " root@prod"
}

// testDSAPEM is a DSA key in OpenSSL's "DSA PRIVATE KEY" format. Its
// numbers only have the right sizes: x/crypto parses it into an ssh-dss
// signer, and the type has to be refused.
var testDSAPEM = func() string {
	p := new(big.Int).SetBit(big.NewInt(1), 1023, 1)
	q := new(big.Int).SetBit(big.NewInt(1), 159, 1)
	der, _ := asn1.Marshal(struct {
		Version       int
		P, Q, G, Y, X *big.Int
	}{0, p, q, big.NewInt(2), big.NewInt(3), big.NewInt(5)})
	return string(pem.EncodeToMemory(&pem.Block{Type: "DSA PRIVATE KEY", Bytes: der}))
}()

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// keyBinding is one valid uploaded-key binding in the PUT shape.
func keyBinding(name, server, user, hostKey, keyPEM string) map[string]any {
	return map[string]any{"name": name, "server": server, "user": user, "hostKeys": []string{hostKey},
		"auth": map[string]any{"type": "private_key", "keyPem": keyPEM}}
}

// genBinding is one valid generate binding in the PUT shape.
func genBinding(name, server, user, hostKey string) map[string]any {
	return map[string]any{"name": name, "server": server, "user": user, "hostKeys": []string{hostKey},
		"auth": map[string]any{"type": "generate"}}
}

// putSSH sets sb1's SSH bindings and returns the decoded answer.
func (r *rig) putSSH(body string) sshSetOut {
	r.t.Helper()
	code, raw := r.sandboxCall(http.MethodPut, "/ssh", body)
	if code != http.StatusOK {
		r.t.Fatalf("PUT ssh: HTTP %d %s", code, raw)
	}
	var out sshSetOut
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *rig) sshRows() (string, []store.SSHBinding) {
	r.t.Helper()
	v, rows, err := r.st.SSHBindings(bg, r.sb1)
	if err != nil {
		r.t.Fatal(err)
	}
	return v, rows
}

// podSSH is sb1's SSH set as a pod receives it.
func (r *rig) podSSH() []*fleetv1.SshBinding {
	r.t.Helper()
	b, _, err := r.svc.PodBindings(bg, r.sb1)
	if err != nil {
		r.t.Fatal(err)
	}
	return b.GetSsh()
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

// ----------------------------------------------------------------- tests --

// The wiring these tests assume, and api is expected to provide: the set
// and the check behind a sandbox token that names the path's sandbox, the
// preflight behind a project token. It checks the harness's mux, not this
// package; the route table and its auth classes are api's to test.
func TestSSHHarnessWiring(t *testing.T) {
	r := newRig(t)
	own := "/api/v1/sandboxes/" + r.sb1.String()
	for _, c := range []struct {
		name, method, path, auth string
		want                     int
	}{
		{"set without token", http.MethodGet, own + "/ssh", "", http.StatusUnauthorized},
		{"set with project bearer", http.MethodGet, own + "/ssh", "Bearer " + projectToken, http.StatusNotFound},
		{"another sandbox's set", http.MethodGet, "/api/v1/sandboxes/" + r.sb2.String() + "/ssh", "Bearer " + sb1Token,
			http.StatusNotFound},
		{"own set", http.MethodGet, own + "/ssh", "Bearer " + sb1Token, http.StatusOK},
		{"check with project bearer", http.MethodPost, own + "/ssh/prod/check", "Bearer " + projectToken, http.StatusNotFound},
		{"preflight with sandbox token", http.MethodPost, "/api/v1/ssh/preflight", "Bearer " + sb1Token,
			http.StatusUnauthorized},
	} {
		if code, body := r.call(c.method, c.path, c.auth, ""); code != c.want {
			t.Errorf("%s: HTTP %d %s, want %d", c.name, code, body, c.want)
		}
	}
}

// Validation, each failure with its stable code.
func TestSSHPutValidationCodes(t *testing.T) {
	r := newRig(t)
	ed := newEd25519(t)
	host := hostLine(t, newEd25519(t))
	key := opensshPEM(t, ed)
	ok := func() map[string]any { return keyBinding("prod", "ssh://git.example.test", "deploy", host, key) }
	with := func(k string, v any) map[string]any { b := ok(); b[k] = v; return b }
	authOf := func(a map[string]any) map[string]any { b := ok(); b["auth"] = a; return b }
	without := func(k string) map[string]any { b := ok(); delete(b, k); return b }

	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	p521 := newECDSA(t, elliptic.P521())
	skBlob := ssh.Marshal(struct{ Type, Pub, App string }{"sk-ssh-ed25519@openssh.com", string(make([]byte, 32)), "ssh:"})
	skLine := "sk-ssh-ed25519@openssh.com " + b64(skBlob)
	encrypted := opensshPEMWithPassphrase(t, ed, "correct horse")
	seventeen := make([]string, 17)
	for i := range seventeen {
		seventeen[i] = host
	}
	many := make([]map[string]any, 33)
	for i := range many {
		many[i] = genBinding("b"+string(rune('a'+i%26))+string(rune('a'+i/26)), "ssh://git.example.test", "git", host)
	}

	for _, c := range []struct {
		name string
		body string
		code string
	}{
		{"no bindings field", `{}`, codeInvalidBinding},
		{"not json", `{"bindings":`, codeInvalidBinding},
		{"name not a label", setBody(with("name", "Prod")), codeInvalidBinding},
		{"duplicate name", setBody(ok(), ok()), codeInvalidBinding},
		{"too many", setBody(many...), codeInvalidBinding},
		{"no server", setBody(with("server", "")), codeInvalidBinding},
		{"not ssh scheme", setBody(with("server", "https://git.example.test")), codeInvalidBinding},
		{"no scheme", setBody(with("server", "git.example.test:22")), codeInvalidBinding},
		{"user in server", setBody(with("server", "ssh://git@git.example.test")), codeInvalidBinding},
		{"path", setBody(with("server", "ssh://git.example.test/repo")), codeInvalidBinding},
		{"slash path", setBody(with("server", "ssh://git.example.test/")), codeInvalidBinding},
		{"query", setBody(with("server", "ssh://git.example.test?x=1")), codeInvalidBinding},
		{"port zero", setBody(with("server", "ssh://git.example.test:0")), codeInvalidBinding},
		{"no user", setBody(without("user")), codeInvalidBinding},
		{"user uppercase", setBody(with("user", "Deploy")), codeInvalidBinding},
		{"user too long", setBody(with("user", strings.Repeat("a", 33))), codeInvalidBinding},
		{"no hostKeys", setBody(without("hostKeys")), codeInvalidBinding},
		{"empty hostKeys", setBody(with("hostKeys", []string{})), codeInvalidBinding},
		{"too many hostKeys", setBody(with("hostKeys", seventeen)), codeInvalidBinding},
		{"no auth", setBody(without("auth")), codeInvalidBinding},
		{"no auth type", setBody(authOf(map[string]any{"keyPem": key})), codeInvalidBinding},
		{"unknown auth type", setBody(authOf(map[string]any{"type": "certificate"})), codeInvalidBinding},
		{"private_key without keyPem", setBody(authOf(map[string]any{"type": "private_key"})), codeInvalidBinding},
		{"generate with keyPem", setBody(authOf(map[string]any{"type": "generate", "keyPem": key})), codeInvalidBinding},
		{"not utf-8", setBody(with("user", "\xff")), codeInvalidBinding},
		// bad_host_key: a hostKeys entry that is not a supported public key.
		{"host key garbage", setBody(with("hostKeys", []string{"not a key"})), codeBadHostKey},
		{"host key bad base64", setBody(with("hostKeys", []string{"ssh-ed25519 !!!"})), codeBadHostKey},
		{"known_hosts line", setBody(with("hostKeys", []string{"git.example.test " + host})), codeBadHostKey},
		{"host key type lies", setBody(with("hostKeys", []string{"ssh-rsa " + strings.Fields(host)[1]})), codeBadHostKey},
		{"host key P-521", setBody(with("hostKeys", []string{hostLine(t, p521)})), codeBadHostKey},
		{"host key RSA 1024", setBody(with("hostKeys", []string{hostLine(t, small)})), codeBadHostKey},
		{"host key sk", setBody(with("hostKeys", []string{skLine})), codeBadHostKey},
		// bad_key: the key does not parse, the passphrase is wrong, missing
		// or needless, the type is not supported.
		{"key garbage", setBody(authOf(map[string]any{"type": "private_key", "keyPem": "nope"})), codeBadKey},
		{"key is a public key", setBody(authOf(map[string]any{"type": "private_key", "keyPem": host})), codeBadKey},
		{"encrypted, no passphrase", setBody(authOf(map[string]any{"type": "private_key", "keyPem": encrypted})), codeBadKey},
		{"encrypted, wrong passphrase", setBody(authOf(map[string]any{"type": "private_key", "keyPem": encrypted,
			"passphrase": "wrong"})), codeBadKey},
		{"plain, needless passphrase", setBody(authOf(map[string]any{"type": "private_key", "keyPem": key,
			"passphrase": "x"})), codeBadKey},
		{"RSA 1024", setBody(authOf(map[string]any{"type": "private_key", "keyPem": pkcs1PEM(small)})), codeBadKey},
		{"P-521", setBody(authOf(map[string]any{"type": "private_key", "keyPem": sec1PEM(t, p521)})), codeBadKey},
		{"DSA", setBody(authOf(map[string]any{"type": "private_key", "keyPem": testDSAPEM})), codeBadKey},
		// The destination rule, Kubernetes', without exceptions.
		{"platform address", setBody(with("server", "ssh://10.0.0.10")), codeDestinationDenied},
		{"loopback", setBody(with("server", "ssh://127.0.0.1:2222")), codeDestinationDenied},
		{"metadata", setBody(with("server", "ssh://169.254.169.254")), codeDestinationDenied},
		{"ipv6", setBody(with("server", "ssh://[2001:db8::1]")), codeDestinationDenied},
		{"name resolving internal", setBody(with("server", "ssh://internal.test")), codeDestinationDenied},
		{"name resolving mixed", setBody(with("server", "ssh://mixed.test")), codeDestinationDenied},
		{"name not resolving", setBody(with("server", "ssh://nxdomain.test")), codeDestinationDenied},
	} {
		code, raw := r.sandboxCall(http.MethodPut, "/ssh", c.body)
		msg, got := codeOf(raw)
		if code != http.StatusBadRequest || got != c.code || msg == "" {
			t.Errorf("%s: HTTP %d %s, want 400 with code %s", c.name, code, strings.TrimSpace(raw), c.code)
		}
	}
	if v, rows := r.sshRows(); v != "" || len(rows) != 0 {
		t.Fatalf("a rejected PUT stored something: %q %+v", v, rows)
	}
	// The texts clients see.
	for body, want := range map[string]string{
		setBody(without("hostKeys")): "bindings[0]: hostKeys is required: at least one public key of the server " +
			"(a preflight without hostKeys reports the one it presents), or anyServer for a key without a destination",
		setBody(authOf(map[string]any{"type": "private_key", "keyPem": encrypted})): "bindings[0]: the key is encrypted; auth.passphrase is required",
	} {
		_, raw := r.sandboxCall(http.MethodPut, "/ssh", body)
		if msg, _ := codeOf(raw); msg != want {
			t.Errorf("message %q, want %q", msg, want)
		}
	}
	// And the valid forms: default port, explicit port, IP literal, an
	// encrypted key with its passphrase, a generate binding.
	out := r.putSSH(setBody(ok(),
		keyBinding("ip", "ssh://203.0.113.40:2222", "git", host, key),
		map[string]any{"name": "enc", "server": "ssh://git.example.test", "user": "ci", "hostKeys": []string{host},
			"auth": map[string]any{"type": "private_key", "keyPem": encrypted, "passphrase": "correct horse"}},
		genBinding("gh", "ssh://git.example.test", "git", host)))
	if len(out.Bindings) != 4 || out.Version == "" {
		t.Fatalf("valid set: %+v", out)
	}
}

// The version is the old system's: the SHA-256 of the snake_case JSON of
// the bindings sorted by name.
func TestSSHVersionShape(t *testing.T) {
	bs := []sshBinding{
		{Name: "prod", User: "deploy", HostKeys: []string{"ssh-ed25519 AAAA"}, PrivateKey: "K1"},
		{Name: "any", AnyServer: true, PrivateKey: "K2"},
	}
	want := `[{"name":"any","user":"","any_server":true,"private_key":"K2"},` +
		`{"name":"prod","user":"deploy","host_keys":["ssh-ed25519 AAAA"],"private_key":"K1"}]`
	sum := sha256.Sum256([]byte(want))
	if got := sshVersion(bs); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("version %s, want the hash of %s", got, want)
	}
	if sshVersion(nil) != "" {
		t.Fatal("the empty set's version is not empty")
	}
}

// PUT, GET, DELETE: stored, normalised, never echoed, idempotent.
func TestSSHPutGetDelete(t *testing.T) {
	r := newRig(t)
	ed := newEd25519(t)
	edPEM := opensshPEM(t, ed)
	hostKey := newEd25519(t)
	host := hostLine(t, hostKey)
	rk := rsaKey(t)
	legacyBinding := keyBinding("legacy", "ssh://203.0.113.40", "root", host, encryptedPKCS1PEM(t, rk, "pw"))
	legacyBinding["auth"].(map[string]any)["passphrase"] = "pw"
	body := setBody(keyBinding("prod", "ssh://git.example.test:22", "deploy", host, edPEM), legacyBinding)

	out := r.putSSH(body)
	// What a pod gets: name order, host keys without comment, keys in
	// unencrypted OpenSSH that parse to the uploaded keys.
	set := r.podSSH()
	if len(set) != 2 {
		t.Fatalf("the pod gets %+v", set)
	}
	legacy, prod := set[0], set[1]
	if legacy.Name != "legacy" || prod.Name != "prod" || prod.User != "deploy" || prod.Server != "ssh://git.example.test:22" ||
		len(prod.HostKeys) != 1 || prod.HostKeys[0] != sshKeyLine(pubOf(t, hostKey)) {
		t.Fatalf("pod bindings: %+v", set)
	}
	for name, want := range map[string]crypto.PrivateKey{"prod": ed, "legacy": rk} {
		var pk string
		for _, b := range set {
			if b.Name == name {
				pk = b.PrivateKey
			}
		}
		if !strings.HasPrefix(pk, "-----BEGIN OPENSSH PRIVATE KEY-----") {
			t.Fatalf("%s: the pod gets a key that is not OpenSSH: %.40q", name, pk)
		}
		signer, err := ssh.ParsePrivateKey([]byte(pk))
		if err != nil {
			t.Fatalf("%s: key does not parse unencrypted: %v", name, err)
		}
		if sshKeyLine(signer.PublicKey()) != sshKeyLine(pubOf(t, want)) {
			t.Fatalf("%s: the key is not the uploaded one", name)
		}
	}
	if v, _ := r.sshRows(); v != out.Version {
		t.Fatalf("stored version %q, answered %q", v, out.Version)
	}

	// The answer: public facts only.
	fp := regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
	p := out.Bindings[1]
	if p.Name != "prod" || p.Server != "ssh://git.example.test:22" || p.User != "deploy" ||
		p.AuthType != "private_key" || p.KeyType != "ssh-ed25519" ||
		p.Fingerprint != ssh.FingerprintSHA256(pubOf(t, ed)) || !fp.MatchString(p.Fingerprint) ||
		p.PublicKey != sshKeyLine(pubOf(t, ed))+" prod@sandbox-"+r.sb1.String()[:8] ||
		len(p.HostKeys) != 1 || p.HostKeys[0].Type != "ssh-ed25519" ||
		p.HostKeys[0].Fingerprint != ssh.FingerprintSHA256(pubOf(t, hostKey)) || p.AnyServer {
		t.Fatalf("PUT answer: %+v", p)
	}
	if l := out.Bindings[0]; l.KeyType != "ssh-rsa" || !strings.HasPrefix(l.PublicKey, "ssh-rsa ") {
		t.Fatalf("PUT answer for the RSA key: %+v", l)
	}

	// GET: the same, and no private material, no passphrase.
	code, raw := r.sandboxCall(http.MethodGet, "/ssh", "")
	if code != http.StatusOK {
		t.Fatalf("GET: %d %s", code, raw)
	}
	secrets := []string{"PRIVATE KEY", `"pw"`, "passphrase", "keyPem"}
	for _, b := range set {
		secrets = append(secrets, strings.Split(b.PrivateKey, "\n")[2])
	}
	for _, secret := range secrets {
		if strings.Contains(raw, secret) {
			t.Fatalf("GET leaks %q: %s", secret, raw)
		}
	}
	var got sshSetOut
	_ = json.Unmarshal([]byte(raw), &got)
	if got.Version != out.Version || len(got.Bindings) != 2 || got.Bindings[1].PublicKey != p.PublicKey {
		t.Fatalf("GET: %+v", got)
	}

	// The same set again, even re-encrypted, is a no-op: the normalised
	// key is a function of the key, so the version holds.
	_, before := r.sshRows()
	if again := r.putSSH(body); again.Version != out.Version {
		t.Fatalf("repeat PUT version %q, want %q", again.Version, out.Version)
	}
	// The same key in another format is the same binding.
	pkcs8 := strings.Replace(body, jsonString(edPEM), jsonString(pkcs8PEM(t, ed)), 1)
	if pkcs8 == body {
		t.Fatal("test bug: the key was not swapped")
	}
	if again := r.putSSH(pkcs8); again.Version != out.Version {
		t.Fatal("the same key as PKCS#8 changed the version")
	}
	_, after := r.sshRows()
	for i := range before {
		if string(before[i].Sealed) != string(after[i].Sealed) {
			t.Fatal("an unchanged set was rewritten")
		}
	}

	// A new server only: stored, the version unchanged.
	moved := strings.Replace(body, "ssh://203.0.113.40", "ssh://203.0.113.41", 1)
	if again := r.putSSH(moved); again.Version != out.Version || again.Bindings[0].Server != "ssh://203.0.113.41" {
		t.Fatalf("server change: %+v", again)
	}
	if set := r.podSSH(); set[0].Server != "ssh://203.0.113.41" {
		t.Fatalf("the pod gets the old server: %+v", set[0])
	}

	// A smaller set replaces the whole one.
	r.putSSH(setBody(keyBinding("prod", "ssh://git.example.test:22", "deploy", host, opensshPEM(t, ed))))
	if set := r.podSSH(); len(set) != 1 {
		t.Fatalf("after shrinking, the pod gets %+v", set)
	}

	// DELETE: 204, nothing left.
	if code, raw := r.sandboxCall(http.MethodDelete, "/ssh", ""); code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", code, raw)
	}
	code, raw = r.sandboxCall(http.MethodGet, "/ssh", "")
	if code != http.StatusOK || strings.TrimSpace(raw) != `{"version":"","bindings":[]}` {
		t.Fatalf("GET after DELETE: %d %s", code, raw)
	}
	if v, rows := r.sshRows(); v != "" || len(rows) != 0 {
		t.Fatalf("left after DELETE: %q %+v", v, rows)
	}
}

// Every upload format becomes the same one format, deterministically.
func TestSSHKeyFormats(t *testing.T) {
	ed := newEd25519(t)
	p256, p384 := newECDSA(t, elliptic.P256()), newECDSA(t, elliptic.P384())
	rk := rsaKey(t)
	for _, c := range []struct {
		name, pem, pass string
		want            crypto.PrivateKey
	}{
		{"ed25519 openssh", opensshPEM(t, ed), "", ed},
		{"ed25519 openssh encrypted", opensshPEMWithPassphrase(t, ed, "s3cret"), "s3cret", ed},
		{"ed25519 pkcs8", pkcs8PEM(t, ed), "", ed},
		{"p256 sec1", sec1PEM(t, p256), "", p256},
		{"p256 pkcs8", pkcs8PEM(t, p256), "", p256},
		{"p384 openssh", opensshPEM(t, p384), "", p384},
		{"rsa pkcs1", pkcs1PEM(rk), "", rk},
		{"rsa pkcs1 encrypted", encryptedPKCS1PEM(t, rk, "s3cret"), "s3cret", rk},
		{"rsa pkcs8", pkcs8PEM(t, rk), "", rk},
		{"rsa openssh encrypted", opensshPEMWithPassphrase(t, rk, "s3cret"), "s3cret", rk},
	} {
		raw, berr := parseSSHPrivateKey(c.pem, c.pass)
		if berr != nil {
			t.Errorf("%s: %v", c.name, berr)
			continue
		}
		a, err := normalizeSSHPrivateKey(raw, "prod@sandbox-12345678")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if b, _ := normalizeSSHPrivateKey(raw, "prod@sandbox-12345678"); a != b {
			t.Errorf("%s: normalisation is not deterministic", c.name)
		}
		signer, err := ssh.ParsePrivateKey([]byte(a))
		if err != nil || !strings.HasPrefix(a, "-----BEGIN OPENSSH PRIVATE KEY-----") {
			t.Errorf("%s: normalised key does not parse as unencrypted OpenSSH: %v", c.name, err)
			continue
		}
		// OpenSSH's line width, the only one strict parsers take.
		lines := strings.Split(strings.TrimSuffix(a, "\n"), "\n")
		for i, l := range lines[1 : len(lines)-2] {
			if len(l) != 70 {
				t.Errorf("%s: base64 line %d is %d wide, want 70", c.name, i+1, len(l))
				break
			}
		}
		if last := lines[len(lines)-2]; len(last) == 0 || len(last) > 70 {
			t.Errorf("%s: last base64 line is %d wide", c.name, len(last))
		}
		if sshKeyLine(signer.PublicKey()) != sshKeyLine(pubOf(t, c.want)) {
			t.Errorf("%s: normalised key is another key", c.name)
		}
	}
}

// The normalised key is what OpenSSH itself reads, and the public line and
// fingerprint shown are the ones ssh-keygen prints. Skipped where
// ssh-keygen is not installed.
func TestSSHKeysAgreeWithSSHKeygen(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not installed")
	}
	dir := t.TempDir()
	for name, raw := range map[string]crypto.PrivateKey{"ed25519": newEd25519(t),
		"p384": newECDSA(t, elliptic.P384()), "rsa": rsaKey(t)} {
		key, err := normalizeSSHPrivateKey(raw, "prod@sandbox-12345678")
		if err != nil {
			t.Fatal(err)
		}
		facts, err := sshFactsOf(key, "prod@sandbox-12345678")
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte(key), 0o600); err != nil {
			t.Fatal(err)
		}
		pub, err := exec.Command(keygen, "-y", "-f", file).Output()
		if err != nil {
			t.Fatalf("%s: ssh-keygen -y does not read the normalised key: %v", name, err)
		}
		if got := strings.TrimSpace(string(pub)); got != facts.publicKey {
			t.Fatalf("%s: ssh-keygen -y prints %q, ferund shows %q", name, got, facts.publicKey)
		}
		fp, err := exec.Command(keygen, "-l", "-E", "sha256", "-f", file).Output()
		if err != nil {
			t.Fatalf("%s: ssh-keygen -l: %v", name, err)
		}
		if f := strings.Fields(string(fp)); len(f) < 2 || f[1] != facts.fingerprint {
			t.Fatalf("%s: ssh-keygen -l prints %q, ferund shows %q", name, fp, facts.fingerprint)
		}
	}
}

// A generated key belongs to its binding's name. A PUT that names the
// binding with generate again keeps it, also when server, user or host
// keys change; removing, renaming or switching it to private_key discards
// it for good.
func TestSSHGeneratedKeyRetention(t *testing.T) {
	r := newRig(t)
	host := hostLine(t, newEd25519(t))
	other := hostLine(t, newEd25519(t))
	pk := func(out sshSetOut, name string) string {
		t.Helper()
		for _, b := range out.Bindings {
			if b.Name == name {
				if b.AuthType != "generate" || b.KeyType != "ssh-ed25519" {
					t.Fatalf("binding %s: %+v", name, b)
				}
				return b.PublicKey
			}
		}
		t.Fatalf("no binding %s in %+v", name, out)
		return ""
	}
	blob := func(line string) string { return strings.Fields(line)[1] }

	first := r.putSSH(setBody(genBinding("gh", "ssh://git.example.test", "git", host)))
	key1 := pk(first, "gh")
	if !strings.HasPrefix(key1, "ssh-ed25519 ") || !strings.HasSuffix(key1, " gh@sandbox-"+r.sb1.String()[:8]) {
		t.Fatalf("public key line: %q", key1)
	}
	if set := r.podSSH(); len(set) != 1 || !strings.HasPrefix(set[0].PrivateKey, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("the pod gets %+v", set)
	}

	// The same PUT again: same key, same version.
	again := r.putSSH(setBody(genBinding("gh", "ssh://git.example.test", "git", host)))
	if pk(again, "gh") != key1 || again.Version != first.Version {
		t.Fatal("a repeated generate PUT minted a new key")
	}

	// Server, user and host keys change: the key stays, the set is new.
	changed := genBinding("gh", "ssh://203.0.113.50:2222", "deploy", other)
	out := r.putSSH(setBody(changed, keyBinding("prod", "ssh://git.example.test", "root", host, opensshPEM(t, newEd25519(t)))))
	if pk(out, "gh") != key1 || out.Version == first.Version {
		t.Fatalf("changing the destination of a generated binding: key %q version %q", pk(out, "gh"), out.Version)
	}

	// Renamed: a new key under the new name; the old name back does not
	// bring the old key back.
	renamed := r.putSSH(setBody(genBinding("github", "ssh://git.example.test", "git", host)))
	if blob(pk(renamed, "github")) == blob(key1) {
		t.Fatal("a renamed binding kept its key")
	}
	back := r.putSSH(setBody(genBinding("gh", "ssh://git.example.test", "git", host)))
	if blob(pk(back, "gh")) == blob(key1) {
		t.Fatal("a removed generated key came back")
	}
	key3 := pk(back, "gh")

	// Switched to private_key and back: a new key.
	r.putSSH(setBody(keyBinding("gh", "ssh://git.example.test", "git", host, opensshPEM(t, newEd25519(t)))))
	switched := r.putSSH(setBody(genBinding("gh", "ssh://git.example.test", "git", host)))
	if blob(pk(switched, "gh")) == blob(key3) {
		t.Fatal("a generated key survived a switch to private_key")
	}
	key4 := pk(switched, "gh")

	// DELETE and back: a new key.
	if code, raw := r.sandboxCall(http.MethodDelete, "/ssh", ""); code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", code, raw)
	}
	fresh := r.putSSH(setBody(genBinding("gh", "ssh://git.example.test", "git", host)))
	if blob(pk(fresh, "gh")) == blob(key4) {
		t.Fatal("a generated key survived DELETE")
	}
	// A preflight cannot use generate: the key exists only in a binding.
	b, _ := json.Marshal(genBinding("gh", "ssh://git.example.test", "git", host))
	if code, raw := r.projectCall(http.MethodPost, "/api/v1/ssh/preflight", string(b)); code != http.StatusBadRequest ||
		!strings.Contains(raw, codeInvalidBinding) {
		t.Fatalf("preflight with generate: %d %s", code, raw)
	}
}

// The key is sealed in the database (no plaintext key, no passphrase at
// all), another key cannot open it, a blob moved to another row or
// relabelled does not open, and a generated key that no longer opens is
// refused with 409 instead of being replaced behind the person's back.
func TestSSHSecretsSealedAtRest(t *testing.T) {
	r := newRig(t)
	ed := newEd25519(t)
	host := hostLine(t, newEd25519(t))
	b := keyBinding("prod", "ssh://git.example.test", "deploy", host, opensshPEMWithPassphrase(t, ed, "hunter2-passphrase"))
	b["auth"].(map[string]any)["passphrase"] = "hunter2-passphrase"
	r.putSSH(setBody(b, genBinding("gh", "ssh://git.example.test", "git", host)))

	set := r.podSSH()
	secrets := []string{"hunter2-passphrase"}
	for _, b := range set {
		secrets = append(secrets, strings.Split(b.PrivateKey, "\n")[2])
	}
	var dump string
	if err := r.d.Pool.QueryRow(bg, "SELECT string_agg(ssh_bindings::text, ' ') FROM ssh_bindings").Scan(&dump); err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(dump, secret) {
			t.Fatalf("ssh_bindings holds secret material in plaintext (%.20q…)", secret)
		}
	}
	version, rows := r.sshRows()
	other, err := New(Config{Enabled: true, Key: newKey(t), Log: quietLog()}, r.st, r.d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.openSSH(r.sb1, version, rows); err == nil || !strings.Contains(err.Error(), "--secret-key-file") {
		t.Fatalf("another key must not open the set: %v", err)
	}
	if _, err := r.svc.openSSHKey(r.sb2, rows[0]); err == nil {
		t.Fatal("a sealed key opened under another sandbox")
	}
	relabelled := rows[0]
	if relabelled.AuthType == sshAuthPrivateKey {
		relabelled.AuthType = sshAuthGenerate
	} else {
		relabelled.AuthType = sshAuthPrivateKey
	}
	if _, err := r.svc.openSSHKey(r.sb1, relabelled); err == nil {
		t.Fatal("a sealed key opened under another auth type")
	}

	// A generated key that no longer opens cannot be kept.
	err = r.st.InTx(bg, func(tx *store.Store) error {
		return tx.SetSSHBindings(bg, r.sb1, "x", []store.SSHBinding{{Name: "gh", Server: "ssh://git.example.test",
			User: "git", HostKeys: []string{strings.Join(strings.Fields(host)[:2], " ")}, AuthType: sshAuthGenerate,
			KeyType: "ssh-ed25519", Fingerprint: "SHA256:x", PublicKey: "ssh-ed25519 x", Sealed: []byte{1, 2, 3}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	code, raw := r.sandboxCall(http.MethodPut, "/ssh", setBody(genBinding("gh", "ssh://git.example.test", "git", host)))
	if msg, _ := codeOf(raw); code != http.StatusConflict || !strings.Contains(msg, "DELETE the set and PUT it again") {
		t.Fatalf("PUT over an unopenable generated key: %d %s", code, raw)
	}
	// DELETE is the way out.
	if code, raw := r.sandboxCall(http.MethodDelete, "/ssh", ""); code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", code, raw)
	}
	r.putSSH(setBody(genBinding("gh", "ssh://git.example.test", "git", host)))
}

// The amendment: a key for any server. Stored without server, user or
// host keys; the pod gets it without them.
func TestSSHAnyServerBinding(t *testing.T) {
	r := newRig(t)
	ed := newEd25519(t)
	anyBinding := func(auth map[string]any) map[string]any {
		return map[string]any{"name": "project", "anyServer": true, "auth": auth}
	}
	upload := map[string]any{"type": "private_key", "keyPem": opensshPEM(t, ed)}

	out := r.putSSH(setBody(anyBinding(upload)))
	p := out.Bindings[0]
	if !p.AnyServer || p.Server != "" || p.User != "" || len(p.HostKeys) != 0 ||
		p.Fingerprint != ssh.FingerprintSHA256(pubOf(t, ed)) {
		t.Fatalf("answer: %+v", p)
	}
	set := r.podSSH()
	if len(set) != 1 || set[0].Server != "" || set[0].User != "" || len(set[0].HostKeys) != 0 {
		t.Fatalf("the pod gets %+v", set)
	}
	stored, rows := r.sshRows()
	if wire, _ := json.Marshal(sshBinding{Name: "project", AnyServer: true, PrivateKey: "k"}); strings.Contains(string(wire), "host_keys") ||
		!strings.Contains(string(wire), `"any_server":true`) {
		t.Fatalf("version form: %s", wire)
	}
	// The same PUT again: nothing written.
	if again := r.putSSH(setBody(anyBinding(upload))); again.Version != out.Version {
		t.Fatalf("version moved: %s → %s", out.Version, again.Version)
	}
	if v, rows2 := r.sshRows(); v != stored || string(rows2[0].Sealed) != string(rows[0].Sealed) {
		t.Fatal("an unchanged any-server set was rewritten")
	}
	if code, raw := r.sandboxCall(http.MethodGet, "/ssh", ""); code != http.StatusOK || !strings.Contains(raw, `"anyServer":true`) {
		t.Fatalf("GET: %d %s", code, raw)
	}

	// A generated key for any server is kept across PUTs like any other.
	gen := r.putSSH(setBody(anyBinding(map[string]any{"type": "generate"}))).Bindings[0].PublicKey
	if again := r.putSSH(setBody(anyBinding(map[string]any{"type": "generate"}))).Bindings[0].PublicKey; again != gen {
		t.Fatal("the generated key was not kept")
	}

	// What an any-server binding refuses, and what a pinned one still needs.
	for name, b := range map[string]map[string]any{
		"server":                  {"name": "x", "anyServer": true, "server": "ssh://git.example.test", "auth": upload},
		"user":                    {"name": "x", "anyServer": true, "user": "git", "auth": upload},
		"hostKeys":                {"name": "x", "anyServer": true, "hostKeys": []string{hostLine(t, newEd25519(t))}, "auth": upload},
		"no auth":                 {"name": "x", "anyServer": true},
		"pinned without hostKeys": {"name": "x", "server": "ssh://git.example.test", "user": "git", "auth": upload},
	} {
		code, raw := r.sandboxCall(http.MethodPut, "/ssh", setBody(b))
		if _, c := codeOf(raw); code != http.StatusBadRequest || c != codeInvalidBinding {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
}

// An uploaded-key set whose blobs no longer open (the key of
// --secret-key-file changed) is repaired by PUTting the same set through
// a Service with the new key: the version holds, and the key opens under
// the new key.
func TestSSHRepairUnderNewKey(t *testing.T) {
	r := newRig(t)
	host := hostLine(t, newEd25519(t))
	body := setBody(keyBinding("prod", "ssh://git.example.test", "deploy", host, opensshPEM(t, newEd25519(t))))
	v := r.putSSH(body).Version
	other, err := New(Config{Enabled: true, Key: newKey(t), Log: quietLog()}, r.st, r.d)
	if err != nil {
		t.Fatal(err)
	}
	_, rows := r.sshRows()
	if _, err := other.openSSH(r.sb1, v, rows); err == nil {
		t.Fatal("another key opened the set")
	}
	r2 := r.on(other)
	if again := r2.putSSH(body); again.Version != v {
		t.Fatalf("repair PUT version %q, want %q", again.Version, v)
	}
	_, rows = r.sshRows()
	set, err := other.openSSH(r.sb1, v, rows)
	if err != nil || len(set) != 1 || !strings.HasPrefix(set[0].PrivateKey, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("the repair PUT did not reseal the set: %v", err)
	}
	if _, err := r.svc.openSSH(r.sb1, v, rows); err == nil {
		t.Fatal("the old key still opens the resealed set")
	}
}
