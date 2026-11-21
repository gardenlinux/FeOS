// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"crypto"
	"crypto/elliptic"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The SSH preflight and check against an in-test SSH server built on
// x/crypto/ssh, which records what the client did on its side of the
// wire: connections, authentication attempts, channels and global
// requests. The assertions that matter most are on that side: a preflight
// that "worked" while opening a session would be a preflight that ran
// something on a person's server.

// fakeSSHServer is an SSH server with one host key (or several), a set of
// authorized keys for one user, and a record of everything a client did.
type fakeSSHServer struct {
	t        *testing.T
	ln       net.Listener
	user     string
	hostKeys []ssh.Signer

	mu         sync.Mutex
	authorized []ssh.PublicKey
	conns      int      // TCP connections accepted
	auths      []string // every authentication attempt: "<method> <user> <ok>"
	logins     int      // successful authentications
	channels   []string // channel types a client asked for: must stay empty
	requests   []string // global requests a client sent: must stay empty
	wg         sync.WaitGroup
}

func newFakeSSHServer(t *testing.T, user string, hostKeys ...crypto.PrivateKey) *fakeSSHServer {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSSHServer{t: t, ln: ln, user: user}
	for _, k := range hostKeys {
		s, err := ssh.NewSignerFromKey(k)
		if err != nil {
			t.Fatal(err)
		}
		f.hostKeys = append(f.hostKeys, s)
	}
	served := make(chan struct{})
	go func() { defer close(served); f.serve() }()
	t.Cleanup(func() { ln.Close(); <-served; f.wg.Wait() })
	return f
}

func (f *fakeSSHServer) server() string { return "ssh://" + f.ln.Addr().String() }

func (f *fakeSSHServer) authorize(k ssh.PublicKey) {
	f.mu.Lock()
	f.authorized = append(f.authorized, k)
	f.mu.Unlock()
}

var errUnauthorizedKey = errors.New("key not authorized for this user")

func (f *fakeSSHServer) config() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-OpenSSH_9.6",
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if c.User() == f.user {
				for _, a := range f.authorized {
					if string(a.Marshal()) == string(key.Marshal()) {
						return &ssh.Permissions{}, nil
					}
				}
			}
			return nil, errUnauthorizedKey
		},
		AuthLogCallback: func(c ssh.ConnMetadata, method string, err error) {
			f.mu.Lock()
			f.auths = append(f.auths, method+" "+c.User()+" "+map[bool]string{true: "ok", false: "no"}[err == nil])
			f.mu.Unlock()
		},
	}
	for _, s := range f.hostKeys {
		cfg.AddHostKey(s)
	}
	return cfg
}

// serve accepts until the listener closes; wg counts the connections.
func (f *fakeSSHServer) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns++
		f.mu.Unlock()
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			sc, chans, reqs, err := ssh.NewServerConn(c, f.config())
			if err != nil {
				return
			}
			f.mu.Lock()
			f.logins++
			f.mu.Unlock()
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				for r := range reqs {
					f.mu.Lock()
					f.requests = append(f.requests, r.Type)
					f.mu.Unlock()
					if r.WantReply {
						_ = r.Reply(false, nil)
					}
				}
			}()
			for ch := range chans {
				f.mu.Lock()
				f.channels = append(f.channels, ch.ChannelType())
				f.mu.Unlock()
				_ = ch.Reject(ssh.Prohibited, "no")
			}
			_ = sc.Wait()
		}()
	}
}

type sshServerRecord struct {
	conns, logins      int
	auths, chans, reqs []string
}

// settle waits until every connection the server accepted has ended (the
// client closed it), so its record is complete.
func (f *fakeSSHServer) settle() {
	f.t.Helper()
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		f.t.Fatal("a preflight connection was left open")
	}
}

func (f *fakeSSHServer) record() sshServerRecord {
	f.settle()
	f.mu.Lock()
	defer f.mu.Unlock()
	return sshServerRecord{conns: f.conns, logins: f.logins, auths: append([]string(nil), f.auths...),
		chans: append([]string(nil), f.channels...), reqs: append([]string(nil), f.requests...)}
}

func (f *fakeSSHServer) reset() {
	f.settle()
	f.mu.Lock()
	f.conns, f.logins, f.auths, f.channels, f.requests = 0, 0, nil, nil, nil
	f.mu.Unlock()
}

// noChannels is the assertion every successful preflight ends with.
func (rec sshServerRecord) noChannels(t *testing.T) {
	t.Helper()
	if len(rec.chans) != 0 || len(rec.reqs) != 0 {
		t.Fatalf("the preflight opened channels %v / sent requests %v; it must stop at authentication", rec.chans, rec.reqs)
	}
}

func (r *rig) sshPreflight(body map[string]any) (int, string, string) {
	r.t.Helper()
	b, _ := json.Marshal(body)
	code, raw := r.projectCall(http.MethodPost, "/api/v1/ssh/preflight", string(b))
	_, c := codeOf(raw)
	return code, raw, c
}

func (r *rig) checkSSH(name, body string) (int, string, string) {
	r.t.Helper()
	code, raw := r.sandboxCall(http.MethodPost, "/ssh/"+name+"/check", body)
	_, c := codeOf(raw)
	return code, raw, c
}

func preflightBody(server, user string, hostKeys []string, keyPEM string) map[string]any {
	b := map[string]any{"server": server, "user": user}
	if hostKeys != nil {
		b["hostKeys"] = hostKeys
	}
	if keyPEM != "" {
		b["auth"] = map[string]any{"type": "private_key", "keyPem": keyPEM}
	}
	return b
}

func decodePreflight(t *testing.T, raw string) sshPreflightResult {
	t.Helper()
	var res sshPreflightResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// ----------------------------------------------------------------- tests --

// A key the server accepts: authenticated, the presented host key and
// version reported, and on the server's side one login and nothing else.
func TestSSHPreflightAuthenticates(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	hostKey := newEd25519(t)
	srv := newFakeSSHServer(t, "deploy", hostKey)
	ed, rk := newEd25519(t), rsaKey(t)
	srv.authorize(pubOf(t, ed))
	srv.authorize(pubOf(t, rk))

	for name, keyPEM := range map[string]string{"ed25519": opensshPEM(t, ed), "rsa": pkcs1PEM(rk)} {
		srv.reset()
		code, raw, _ := r.sshPreflight(preflightBody(srv.server(), "deploy", []string{hostLine(t, hostKey)}, keyPEM))
		if code != http.StatusOK {
			t.Fatalf("%s: preflight: %d %s", name, code, raw)
		}
		res := decodePreflight(t, raw)
		if !res.Authenticated || res.ServerVersion != "SSH-2.0-OpenSSH_9.6" ||
			res.HostKey.Type != "ssh-ed25519" || res.HostKey.Line != sshKeyLine(pubOf(t, hostKey)) ||
			res.HostKey.Fingerprint != ssh.FingerprintSHA256(pubOf(t, hostKey)) {
			t.Fatalf("%s: answer %s", name, raw)
		}
		rec := srv.record()
		if rec.conns != 1 || rec.logins != 1 {
			t.Fatalf("%s: server saw %d connections, %d logins", name, rec.conns, rec.logins)
		}
		rec.noChannels(t)
	}
	if v, rows := r.sshRows(); v != "" || len(rows) != 0 {
		t.Fatalf("preflight stored: %q %+v", v, rows)
	}
}

// Without auth the preflight stops inside the key exchange: it reports the
// host key, and the server sees no authentication request at all.
func TestSSHPreflightWithoutAuth(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	hostKey := newEd25519(t)
	srv := newFakeSSHServer(t, "git", hostKey)

	code, raw, _ := r.sshPreflight(preflightBody(srv.server(), "git", nil, ""))
	if code != http.StatusOK {
		t.Fatalf("preflight without auth: %d %s", code, raw)
	}
	res := decodePreflight(t, raw)
	if res.Authenticated || res.HostKey.Line != sshKeyLine(pubOf(t, hostKey)) ||
		res.ServerVersion != "SSH-2.0-OpenSSH_9.6" || !strings.Contains(raw, `"authenticated":false`) {
		t.Fatalf("answer: %s", raw)
	}
	rec := srv.record()
	if rec.conns != 1 || len(rec.auths) != 0 || rec.logins != 0 {
		t.Fatalf("server saw %d connections, auth attempts %v, %d logins", rec.conns, rec.auths, rec.logins)
	}

	// The reported line goes into hostKeys unchanged, and it holds.
	srv.reset()
	if code, raw, _ = r.sshPreflight(preflightBody(srv.server(), "git", []string{res.HostKey.Line}, "")); code != http.StatusOK {
		t.Fatalf("preflight with the confirmed host key: %d %s", code, raw)
	}
	if rec := srv.record(); len(rec.auths) != 0 {
		t.Fatalf("auth attempts without auth: %v", rec.auths)
	}
}

// A server that presents a key the binding does not pin is refused before
// anything is authenticated.
func TestSSHPreflightHostKeyMismatch(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	hostKey := newEd25519(t)
	srv := newFakeSSHServer(t, "deploy", hostKey)
	ed := newEd25519(t)
	srv.authorize(pubOf(t, ed))

	// Pinned: another ed25519 key.
	code, raw, c := r.sshPreflight(preflightBody(srv.server(), "deploy", []string{hostLine(t, newEd25519(t))}, opensshPEM(t, ed)))
	if code != http.StatusUnprocessableEntity || c != codeHostKeyMismatch ||
		!strings.Contains(raw, ssh.FingerprintSHA256(pubOf(t, hostKey))) {
		t.Fatalf("other host key: %d %s", code, raw)
	}
	if rec := srv.record(); len(rec.auths) != 0 || rec.logins != 0 {
		t.Fatalf("the key was offered to an unverified server: %v", rec.auths)
	}

	// Pinned: a type the server has no key of.
	srv.reset()
	code, raw, c = r.sshPreflight(preflightBody(srv.server(), "deploy", []string{hostLine(t, rsaKey(t))}, opensshPEM(t, ed)))
	if code != http.StatusUnprocessableEntity || c != codeHostKeyMismatch {
		t.Fatalf("pinned type absent: %d %s", code, raw)
	}

	// A server with two host keys is asked for the pinned one.
	ecHost := newECDSA(t, elliptic.P256())
	two := newFakeSSHServer(t, "deploy", hostKey, ecHost)
	two.authorize(pubOf(t, ed))
	for _, pinned := range []crypto.PrivateKey{hostKey, ecHost} {
		code, raw, _ := r.sshPreflight(preflightBody(two.server(), "deploy", []string{hostLine(t, pinned)}, opensshPEM(t, ed)))
		if code != http.StatusOK || decodePreflight(t, raw).HostKey.Line != sshKeyLine(pubOf(t, pinned)) {
			t.Fatalf("server with two host keys, pinned %s: %d %s", pubOf(t, pinned).Type(), code, raw)
		}
	}
	two.record().noChannels(t)
}

// The server refuses the key, or the key for this user: unauthorized.
func TestSSHPreflightUnauthorized(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	hostKey := newEd25519(t)
	srv := newFakeSSHServer(t, "deploy", hostKey)
	ed := newEd25519(t)
	srv.authorize(pubOf(t, ed))
	host := []string{hostLine(t, hostKey)}

	for name, body := range map[string]map[string]any{
		"unknown key":  preflightBody(srv.server(), "deploy", host, opensshPEM(t, newEd25519(t))),
		"another user": preflightBody(srv.server(), "root", host, opensshPEM(t, ed)),
	} {
		srv.reset()
		code, raw, c := r.sshPreflight(body)
		if code != http.StatusUnprocessableEntity || c != codeUnauthorized {
			t.Fatalf("%s: %d %s", name, code, raw)
		}
		rec := srv.record()
		if rec.logins != 0 || len(rec.auths) == 0 {
			t.Fatalf("%s: server record %+v", name, rec)
		}
		rec.noChannels(t)
	}
}

// Requests that are wrong in themselves (400) and bindings that cannot
// work (422), before anything is dialled.
func TestSSHPreflightValidation(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	hostKey := newEd25519(t)
	srv := newFakeSSHServer(t, "deploy", hostKey)
	key := opensshPEM(t, newEd25519(t))
	host := []string{hostLine(t, hostKey)}

	for _, c := range []struct {
		name   string
		body   map[string]any
		status int
		code   string
	}{
		{"no user", preflightBody(srv.server(), "", host, key), http.StatusBadRequest, codeInvalidBinding},
		{"bad server", preflightBody("https://"+srv.ln.Addr().String(), "deploy", host, key), http.StatusBadRequest,
			codeInvalidBinding},
		{"generate", map[string]any{"server": srv.server(), "user": "deploy", "auth": map[string]any{"type": "generate"}},
			http.StatusBadRequest, codeInvalidBinding},
		{"anyServer", map[string]any{"anyServer": true, "auth": map[string]any{"type": "private_key", "keyPem": key}},
			http.StatusBadRequest, codeInvalidBinding},
		{"bad key", preflightBody(srv.server(), "deploy", host, "garbage"), http.StatusUnprocessableEntity, codeBadKey},
		{"bad host key", preflightBody(srv.server(), "deploy", []string{"ssh-ed25519 AAAA"}, key),
			http.StatusUnprocessableEntity, codeBadHostKey},
		{"internal destination", preflightBody("ssh://10.0.0.10", "deploy", host, key), http.StatusUnprocessableEntity,
			codeDestinationDenied},
	} {
		code, raw, got := r.sshPreflight(c.body)
		if code != c.status || got != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, code, strings.TrimSpace(raw), c.status, c.code)
		}
	}
	// name is optional and ignored.
	b := preflightBody(srv.server(), "deploy", host, "")
	b["name"] = "Not A Label"
	if code, raw, _ := r.sshPreflight(b); code != http.StatusOK {
		t.Fatalf("preflight with an ignored name: %d %s", code, raw)
	}
	if rec := srv.record(); rec.conns != 1 {
		t.Fatalf("validation failures reached the server: %d connections", rec.conns)
	}
}

// unreachable: nothing listening, not an SSH server, and the budget
// (shortened here) over a server that never speaks.
func TestSSHPreflightUnreachable(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	r.svc.budget = 300 * time.Millisecond

	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "ssh://" + l.Addr().String()
	l.Close()
	notSSH := listenAndServe(t, func(c net.Conn) { _, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n")) })
	silent := listenAndServe(t, func(c net.Conn) { time.Sleep(2 * time.Second) })

	for name, server := range map[string]string{"nothing listening": closed, "not ssh": notSSH, "silent": silent} {
		start := time.Now()
		code, raw, c := r.sshPreflight(preflightBody(server, "git", nil, ""))
		if code != http.StatusUnprocessableEntity || c != codeUnreachable {
			t.Errorf("%s: %d %s", name, code, raw)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: the budget did not hold: %s", name, d)
		}
	}
}

func listenAndServe(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { l.Close(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); defer c.Close(); handle(c) }()
		}
	}()
	return "ssh://" + l.Addr().String()
}

// The destination rule twice: the production policy refuses the loopback
// server at validation; with validation fooled, the dialer's own check
// refuses the address it would connect to: the server never sees a
// connection.
func TestSSHPreflightDestinationRuleAtDial(t *testing.T) {
	r := newRig(t)
	srv := newFakeSSHServer(t, "git", newEd25519(t))

	if code, raw, c := r.sshPreflight(preflightBody(srv.server(), "git", nil, "")); code != http.StatusUnprocessableEntity ||
		c != codeDestinationDenied {
		t.Fatalf("production policy at validation: %d %s", code, raw)
	}
	r.svc.dest.allowDest = allowLoopback // validation passes; allowDial stays the deny policy
	if code, raw, c := r.sshPreflight(preflightBody(srv.server(), "git", nil, "")); code != http.StatusUnprocessableEntity ||
		c != codeDestinationDenied {
		t.Fatalf("production policy at dial: %d %s", code, raw)
	}
	if rec := srv.record(); rec.conns != 0 {
		t.Fatalf("the dial check let %d connections through", rec.conns)
	}
}

// The check is the preflight for a stored binding, and the deploy-key flow
// end to end: preflight without auth, PUT generate, the line into the
// server's authorized keys, check.
func TestSSHCheckDeployKeyFlow(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	hostKey := newEd25519(t)
	srv := newFakeSSHServer(t, "git", hostKey)

	// 1. Which host key does the server present?
	code, raw, _ := r.sshPreflight(preflightBody(srv.server(), "git", nil, ""))
	if code != http.StatusOK {
		t.Fatalf("preflight: %d %s", code, raw)
	}
	confirmed := decodePreflight(t, raw).HostKey.Line

	// 2. A binding with a generated key; its public line is the answer.
	out := r.putSSH(setBody(genBinding("gh", srv.server(), "git", confirmed)))
	line := out.Bindings[0].PublicKey

	// Before the key is registered: unauthorized.
	srv.reset()
	if code, raw, c := r.checkSSH("gh", ""); code != http.StatusUnprocessableEntity || c != codeUnauthorized {
		t.Fatalf("check before registering the key: %d %s", code, raw)
	}

	// 3. "Paste this line into GitHub."
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatalf("the public key line does not parse as authorized_keys: %v", err)
	}
	srv.authorize(pub)

	// 4. Check: authenticated, with the key only ferund holds.
	srv.reset()
	code, raw, _ = r.checkSSH("gh", "")
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	if res := decodePreflight(t, raw); !res.Authenticated || res.HostKey.Line != confirmed {
		t.Fatalf("check answer: %s", raw)
	}
	rec := srv.record()
	if rec.logins != 1 {
		t.Fatalf("server logins: %d", rec.logins)
	}
	rec.noChannels(t)

	// No such binding: 404 no_binding.
	if code, raw, c := r.checkSSH("nope", ""); code != http.StatusNotFound || c != codeNoBinding {
		t.Fatalf("check of a missing binding: %d %s", code, raw)
	}

	// The binding's hostKeys are always enforced.
	r.putSSH(setBody(genBinding("gh", srv.server(), "git", hostLine(t, newEd25519(t)))))
	srv.reset()
	if code, raw, c := r.checkSSH("gh", ""); code != http.StatusUnprocessableEntity || c != codeHostKeyMismatch {
		t.Fatalf("check against a changed host key: %d %s", code, raw)
	}
	if rec := srv.record(); len(rec.auths) != 0 {
		t.Fatalf("the check authenticated against an unpinned server: %v", rec.auths)
	}

	// And the destination rule applies to a stored binding too.
	r.putSSH(setBody(genBinding("gh", srv.server(), "git", confirmed)))
	r.svc.dest.allowDial = denyPolicy(DefaultDenyPrefixes())
	if code, raw, c := r.checkSSH("gh", ""); code != http.StatusUnprocessableEntity || c != codeDestinationDenied {
		t.Fatalf("check through the production dial policy: %d %s", code, raw)
	}
}

// The check of a key for any server names the server in its body.
func TestSSHCheckAnyServer(t *testing.T) {
	r := newRig(t)
	r.allowLoopback()
	ed := newEd25519(t)
	r.putSSH(setBody(map[string]any{"name": "project", "anyServer": true,
		"auth": map[string]any{"type": "private_key", "keyPem": opensshPEM(t, ed)}}))
	hostKey := newEd25519(t)
	srv := newFakeSSHServer(t, "git", hostKey)

	if code, raw, c := r.checkSSH("project", ""); code != http.StatusBadRequest || c != codeInvalidBinding ||
		!strings.Contains(raw, "binding project is for any server, so the check names one") {
		t.Fatalf("check without a server: %d %s", code, raw)
	}
	body := `{"server":"` + srv.server() + `","user":"git"}`
	srv.reset()
	if code, raw, c := r.checkSSH("project", body); code != http.StatusUnprocessableEntity || c != codeUnauthorized {
		t.Fatalf("check before registering: %d %s", code, raw)
	}
	srv.authorize(pubOf(t, ed))
	srv.reset()
	code, raw, _ := r.checkSSH("project", body)
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	if res := decodePreflight(t, raw); !res.Authenticated || res.HostKey.Line != sshKeyLine(pubOf(t, hostKey)) {
		t.Fatalf("check answer: %s", raw)
	}
	srv.record().noChannels(t)
	// With hostKeys given they are enforced.
	srv.reset()
	pinned := `{"server":"` + srv.server() + `","user":"git","hostKeys":["` + hostLine(t, newEd25519(t)) + `"]}`
	if code, raw, c := r.checkSSH("project", pinned); code != http.StatusUnprocessableEntity || c != codeHostKeyMismatch {
		t.Fatalf("check against another host key: %d %s", code, raw)
	}
	// An internal destination named in the body is refused.
	if code, raw, c := r.checkSSH("project", `{"server":"ssh://10.0.0.10","user":"git"}`); code != http.StatusUnprocessableEntity ||
		c != codeDestinationDenied {
		t.Fatalf("check of an internal destination: %d %s", code, raw)
	}
}
