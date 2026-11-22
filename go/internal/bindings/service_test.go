// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package bindings

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	fleetv1 "github.com/gardenlinux/feos/go/gen/feos/fleet/v1"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/pki"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Bindings are on only with --bindings and a key.
func TestEnabled(t *testing.T) {
	d := dbtest.New(t)
	st := store.New(d)
	key := newKey(t)
	for _, c := range []struct {
		name    string
		cfg     Config
		want    bool
		wantErr bool
	}{
		{"default", Config{}, false, false},
		{"flag without key", Config{Enabled: true}, false, false},
		{"key without flag", Config{Key: key}, false, false},
		{"flag and key", Config{Enabled: true, Key: key}, true, false},
		{"short key", Config{Enabled: true, Key: key[:16]}, false, true},
	} {
		c.cfg.Log = quietLog()
		s, err := New(c.cfg, st, d)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err %v", c.name, err)
			continue
		}
		if err == nil && s.Enabled() != c.want {
			t.Errorf("%s: Enabled %v, want %v", c.name, s.Enabled(), c.want)
		}
	}
}

// sshServersOf is what podVersion reads of a delivered SSH set: names and
// servers.
func sshServersOf(b *fleetv1.PodBindings) []sshStored {
	var out []sshStored
	for _, x := range b.GetSsh() {
		out = append(out, sshStored{sshBinding: sshBinding{Name: x.Name}, server: x.Server})
	}
	return out
}

// PodBindings is what fleet seals into IssuePodCredentials: both sets,
// opened, with the pod version of their two versions.
func TestPodBindings(t *testing.T) {
	r := newRig(t)

	// Nothing stored, or no such sandbox: nil.
	if b, v, err := r.svc.PodBindings(bg, r.sb1); err != nil || b != nil || v != "" {
		t.Fatalf("no bindings: %v %q %v", b, v, err)
	}
	if b, v, err := r.svc.PodBindings(bg, podid.Mint()); err != nil || b != nil || v != "" {
		t.Fatalf("no sandbox: %v %q %v", b, v, err)
	}

	ca := newTestCA(t, "ca")
	cert, key, _ := ca.issue(t, "fecode", nil)
	kube := r.putKube(setBody(tokenBinding("prod", 6443, "https://k8s.example.test:6443"),
		map[string]any{"name": "staging", "port": 7443, "server": "https://203.0.113.20", "serverName": "api.staging",
			"caPem": ca.caPEM, "auth": map[string]any{"type": "client_cert", "certPem": cert, "keyPem": key}}))

	// Only a Kubernetes set: the version covers it alone.
	b, v, err := r.svc.PodBindings(bg, r.sb1)
	if err != nil || b == nil || len(b.Kube) != 2 || len(b.Ssh) != 0 || v != podVersion(kube.Version, "", nil) {
		t.Fatalf("kube only: %v %q %v", b, v, err)
	}

	host := hostLine(t, newEd25519(t))
	ssh := r.putSSH(setBody(genBinding("gh", "ssh://git.example.test", "git", host),
		map[string]any{"name": "any", "anyServer": true, "auth": map[string]any{"type": "generate"}}))
	b, v, err = r.svc.PodBindings(bg, r.sb1)
	if err != nil {
		t.Fatal(err)
	}
	if v != podVersion(kube.Version, ssh.Version, sshServersOf(b)) || v == "" || v == podVersion(kube.Version, "", nil) {
		t.Fatalf("version %q", v)
	}
	want := &fleetv1.PodBindings{
		Kube: []*fleetv1.KubeBinding{
			{Name: "prod", Port: 6443, Server: "https://k8s.example.test:6443",
				Credential: &fleetv1.KubeBinding_Token{Token: kubeTestToken}},
			{Name: "staging", Port: 7443, Server: "https://203.0.113.20", ServerName: "api.staging", CaPem: ca.caPEM,
				Credential: &fleetv1.KubeBinding_ClientCertificate{ClientCertificate: &fleetv1.ClientCertificate{
					CertPem: cert, KeyPem: key}}},
		},
	}
	got := proto.Clone(b).(*fleetv1.PodBindings)
	got.Ssh = nil
	if !proto.Equal(got, want) {
		t.Fatalf("kube bindings:\n%v\nwant\n%v", got, want)
	}
	if len(b.Ssh) != 2 || b.Ssh[0].Name != "any" || b.Ssh[0].Server != "" || b.Ssh[0].User != "" ||
		b.Ssh[1].Name != "gh" || b.Ssh[1].Server != "ssh://git.example.test" || b.Ssh[1].User != "git" ||
		len(b.Ssh[1].HostKeys) != 1 || b.Ssh[1].HostKeys[0] != strings.Join(strings.Fields(host)[:2], " ") ||
		!strings.HasPrefix(b.Ssh[1].PrivateKey, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("ssh bindings: %v", b.Ssh)
	}

	// What fleet does with it: seal to the run's key, which the pod opens.
	priv, err := pki.HPKEKEM().GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := pki.Seal(priv.PublicKey().Bytes(), r.sb1, b, v)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := pki.Open(priv, r.sb1, sealed)
	if err != nil || !proto.Equal(opened, b) || sealed.GetVersion() != v {
		t.Fatalf("seal round trip: %v", err)
	}

	// A change of either set changes the version.
	r.putSSH(setBody(genBinding("gh", "ssh://git.example.test", "git", host)))
	if _, v2, _ := r.svc.PodBindings(bg, r.sb1); v2 == v {
		t.Fatal("the version did not follow the SSH set")
	}
	// Both sets gone: nil again.
	r.sandboxCall(http.MethodDelete, "/kube", "")
	r.sandboxCall(http.MethodDelete, "/ssh", "")
	if b, v, err := r.svc.PodBindings(bg, r.sb1); err != nil || b != nil || v != "" {
		t.Fatalf("after DELETE: %v %q %v", b, v, err)
	}

	// Off: nothing, whatever is stored.
	r.putKube(setBody(tokenBinding("prod", 6443, "https://k8s.example.test:6443")))
	off, err := New(Config{Key: r.key, Log: quietLog()}, r.st, r.d)
	if err != nil {
		t.Fatal(err)
	}
	if b, v, err := off.PodBindings(bg, r.sb1); err != nil || b != nil || v != "" {
		t.Fatalf("off: %v %q %v", b, v, err)
	}
}

// A stored set whose rows do not hash to its recorded version is not
// delivered: the pod would believe it holds a set it does not.
func TestPodBindingsVersionMismatch(t *testing.T) {
	r := newRig(t)
	r.putKube(setBody(tokenBinding("prod", 6443, "https://k8s.example.test:6443")))
	if _, err := r.d.Pool.Exec(bg, "UPDATE sandboxes SET kube_version = 'bogus' WHERE id = $1", r.sb1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.svc.PodBindings(bg, r.sb1); err == nil || !strings.Contains(err.Error(), "PUT the set again") {
		t.Fatalf("PodBindings: %v", err)
	}
	// The PUT of the same set is the repair.
	r.putKube(setBody(tokenBinding("prod", 6443, "https://k8s.example.test:6443")))
	if b, _, err := r.svc.PodBindings(bg, r.sb1); err != nil || len(b.GetKube()) != 1 {
		t.Fatalf("after the repair: %v %v", b, err)
	}
}

// The pod version tells the two sets apart, follows each SSH binding's
// server, and does not depend on the order of the bindings.
func TestPodVersion(t *testing.T) {
	if podVersion("", "", nil) != "" {
		t.Fatal("no sets, a version")
	}
	if podVersion("a", "", nil) == podVersion("", "a", nil) || podVersion("a", "", nil) == "" ||
		len(podVersion("a", "b", nil)) != 64 {
		t.Fatal("the pod version does not tell the sets apart")
	}
	b := func(name, server string) sshStored {
		return sshStored{sshBinding: sshBinding{Name: name}, server: server}
	}
	one := []sshStored{b("gh", "ssh://a"), b("any", "")}
	if podVersion("k", "s", one) == podVersion("k", "s", []sshStored{b("gh", "ssh://b"), b("any", "")}) {
		t.Fatal("the pod version does not cover the server")
	}
	if podVersion("k", "s", one) != podVersion("k", "s", []sshStored{b("any", ""), b("gh", "ssh://a")}) {
		t.Fatal("the pod version depends on the order of the bindings")
	}
	// The separators keep a server from passing for the next name.
	if podVersion("k", "s", []sshStored{b("a", "b"), b("c", "")}) == podVersion("k", "s", []sshStored{b("a", "b\x00c"), b("", "")}) {
		t.Fatal("the pod version is ambiguous")
	}
}

// A PUT that changes only the server of a pinned SSH binding leaves the
// SSH set's version as the REST surface has always had it, but changes
// what the pod receives, so the pod version follows it.
func TestPodVersionFollowsTheServer(t *testing.T) {
	r := newRig(t)
	host := hostLine(t, newEd25519(t))
	before := r.putSSH(setBody(genBinding("gh", "ssh://203.0.113.40", "git", host)))
	_, v1, err := r.svc.PodBindings(bg, r.sb1)
	if err != nil || v1 == "" {
		t.Fatalf("PodBindings: %q %v", v1, err)
	}
	after := r.putSSH(setBody(genBinding("gh", "ssh://203.0.113.41", "git", host)))
	if after.Version != before.Version {
		t.Fatalf("the SSH set's version changed with the server: %q, was %q", after.Version, before.Version)
	}
	b, v2, err := r.svc.PodBindings(bg, r.sb1)
	if err != nil || b.GetSsh()[0].Server != "ssh://203.0.113.41" {
		t.Fatalf("PodBindings after the move: %v %v", b, err)
	}
	if v2 == v1 {
		t.Fatal("the pod version did not follow a server-only change")
	}
}

// KubePorts: the ports of the Kubernetes set, never forwarded, on or off:
// a pod started while bindings were on keeps its Kubernetes listeners
// after an instance restarts with them off.
func TestKubePorts(t *testing.T) {
	r := newRig(t)
	if p, err := r.svc.KubePorts(bg, r.sb1); err != nil || len(p) != 0 {
		t.Fatalf("no bindings: %v %v", p, err)
	}
	r.putKube(setBody(tokenBinding("staging", 7443, "https://k8s.example.test"), tokenBinding("prod", 6443, "https://k8s.example.test")))
	if p, err := r.svc.KubePorts(bg, r.sb1); err != nil || len(p) != 2 || p[0] != 6443 || p[1] != 7443 {
		t.Fatalf("ports: %v %v", p, err)
	}
	for name, cfg := range map[string]Config{
		"off":     {Key: r.key},
		"no key":  {Enabled: true},
		"neither": {},
	} {
		cfg.Log = quietLog()
		off, err := New(cfg, r.st, r.d)
		if err != nil {
			t.Fatal(err)
		}
		if p, err := off.KubePorts(bg, r.sb1); err != nil || len(p) != 2 || p[0] != 6443 || p[1] != 7443 {
			t.Fatalf("%s: %v %v", name, p, err)
		}
	}
}

// --deny-prefix replaces the default set; every non-IPv4 address stays
// refused.
func TestDenyPrefixes(t *testing.T) {
	def := denyPolicy(DefaultDenyPrefixes())
	for addr, denied := range map[string]bool{
		"10.1.2.3": true, "172.16.0.1": true, "172.32.0.1": false, "192.168.1.1": true, "100.64.0.1": true,
		"169.254.169.254": true, "127.0.0.1": true, "0.1.2.3": true, "224.0.0.1": true, "255.255.255.255": true,
		"203.0.113.10": false, "8.8.8.8": false, "::1": true, "2001:db8::1": true, "::ffff:8.8.8.8": false,
		"::ffff:10.0.0.1": true,
	} {
		if err := def(netip.MustParseAddr(addr)); (err != nil) != denied {
			t.Errorf("%s: %v, want denied=%v", addr, err, denied)
		}
	}
	d := dbtest.New(t)
	s, err := New(Config{Enabled: true, Key: newKey(t), DenyPrefixes: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
		Log: quietLog()}, store.New(d), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.dest.allowDest(netip.MustParseAddr("203.0.113.10")); err == nil {
		t.Error("a configured prefix is not denied")
	}
	if err := s.dest.allowDial(netip.MustParseAddr("10.0.0.1")); err != nil {
		t.Errorf("the configured set replaces the default: %v", err)
	}
	if err := s.dest.allowDest(netip.MustParseAddr("2001:db8::1")); err == nil {
		t.Error("an IPv6 address is a destination")
	}
	if kerr := s.dest.checkDestination(bg, "203.0.113.7"); kerr == nil || kerr.code != codeDestinationDenied {
		t.Errorf("checkDestination: %v", kerr)
	}
}

// Every PUT and DELETE holds the sandbox's row FOR UPDATE: one waits while
// another transaction holds the row, and goes on when it commits.
func TestPutWaitsForTheRowLock(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct{ method, suffix, body string }{
		{http.MethodPut, "/kube", setBody(tokenBinding("prod", 6443, "https://k8s.example.test"))},
		{http.MethodDelete, "/kube", ""},
		{http.MethodPut, "/ssh", setBody(genBinding("gh", "ssh://git.example.test", "git", hostLine(t, newEd25519(t))))},
		{http.MethodDelete, "/ssh", ""},
	} {
		tx, err := r.d.Pool.Begin(bg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.On(tx).LockSandbox(bg, r.sb1, store.ForUpdate); err != nil {
			t.Fatal(err)
		}
		type result struct {
			code int
			body string
			err  error
		}
		done := make(chan result, 1)
		go func() {
			code, body, err := r.try(c.method, "/api/v1/sandboxes/"+r.sb1.String()+c.suffix, "Bearer "+sb1Token, c.body)
			done <- result{code, body, err}
		}()
		select {
		case res := <-done:
			t.Fatalf("%s %s finished (%d %v) while another transaction held the row", c.method, c.suffix, res.code, res.err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx.Commit(bg); err != nil {
			t.Fatal(err)
		}
		select {
		case res := <-done:
			if res.err != nil {
				t.Fatalf("%s %s: %v", c.method, c.suffix, res.err)
			}
			if res.code != http.StatusOK && res.code != http.StatusNoContent {
				t.Fatalf("%s %s after the lock: %d %s", c.method, c.suffix, res.code, res.body)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s %s did not go on after the lock was released", c.method, c.suffix)
		}
	}
}

// Concurrent PUTs of a new generate binding, on any instance, mint one key:
// the row lock serialises them, and every later one keeps the first's key.
func TestConcurrentGeneratePutsMintOneKey(t *testing.T) {
	r := newRig(t)
	// A second instance on the same database.
	other, err := New(Config{Enabled: true, Key: r.key, Log: quietLog()}, store.New(r.d), r.d)
	if err != nil {
		t.Fatal(err)
	}
	other.dest.lookup = fakeLookup
	host := hostLine(t, newEd25519(t))
	var wg sync.WaitGroup
	keys := make([]string, 8)
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc := r.svc
			if i%2 == 1 {
				svc = other
			}
			vs, berr := svc.validateSSHSet(bg, []sshBindingIn{{Name: "gh", Server: "ssh://git.example.test", User: "git",
				HostKeys: []string{host}, Auth: &sshAuthIn{Type: sshAuthGenerate}}})
			if berr != nil {
				t.Error(berr)
				return
			}
			out, err := svc.applySSH(bg, r.sb1, vs)
			if err != nil || len(out.Bindings) != 1 {
				t.Error(out, err)
				return
			}
			keys[i] = out.Bindings[0].PublicKey
		}()
	}
	wg.Wait()
	for _, k := range keys {
		if k != keys[0] || k == "" {
			t.Fatalf("concurrent PUTs minted several keys: %v", keys)
		}
	}
}

// The snapshot of PodBindings reads both sets as of one moment: a PUT of
// both sets that commits between its two reads is not half seen, and the
// version names exactly the bindings returned.
func TestPodBindingsSnapshot(t *testing.T) {
	r := newRig(t)
	old := r.putKube(setBody(tokenBinding("prod", 6443, "https://k8s.example.test")))
	host := hostLine(t, newEd25519(t))
	ran := false
	r.svc.betweenReads = func() {
		r.svc.betweenReads = nil
		ran = true
		// A concurrent PUT of both sets commits now.
		r.putKube(setBody(tokenBinding("staging", 7443, "https://k8s.example.test")))
		r.putSSH(setBody(genBinding("gh", "ssh://git.example.test", "git", host)))
	}
	b, v, err := r.svc.PodBindings(bg, r.sb1)
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("test bug: the hook did not run")
	}
	if len(b.GetKube()) != 1 || b.Kube[0].Name != "prod" || len(b.GetSsh()) != 0 ||
		v != podVersion(old.Version, "", nil) {
		t.Fatalf("a snapshot saw a later commit: %v %q", b, v)
	}
	// Without the hook, the next read sees both new sets.
	b, v, err = r.svc.PodBindings(bg, r.sb1)
	if err != nil || len(b.GetKube()) != 1 || b.Kube[0].Name != "staging" || len(b.GetSsh()) != 1 ||
		v == podVersion(old.Version, "", nil) {
		t.Fatalf("after the PUT: %v %q %v", b, v, err)
	}
	if _, err := r.svc.snapshot(bg, podid.Mint()); !errors.Is(err, errNoSandbox) {
		t.Fatalf("snapshot of no sandbox: %v", err)
	}
}

// A PUT answers the set it wrote, read under its own row lock, never the
// set of a PUT that committed right after it (another instance's, say).
func TestPutAnswersItsOwnSet(t *testing.T) {
	r := newRig(t)
	other, err := New(Config{Enabled: true, Key: r.key, Log: quietLog()}, store.New(r.d), r.d)
	if err != nil {
		t.Fatal(err)
	}
	r2 := r.on(other)
	host := hostLine(t, newEd25519(t))
	later := r2.putKube(setBody(tokenBinding("later", 7443, "https://k8s.example.test")))
	want := r2.putKube(setBody(tokenBinding("prod", 6443, "https://k8s.example.test")))

	// Between each PUT's commit and its answer, the other instance commits
	// another set.
	r.svc.afterApply = func() {
		r.svc.afterApply = nil
		r2.putKube(setBody(tokenBinding("later", 7443, "https://k8s.example.test")))
	}
	got := r.putKube(setBody(tokenBinding("prod", 6443, "https://k8s.example.test")))
	if got.Version != want.Version || len(got.Bindings) != 1 || got.Bindings[0].Name != "prod" {
		t.Fatalf("PUT kube answered %+v, want its own set %q (the later one is %q)", got, want.Version, later.Version)
	}
	r.svc.afterApply = func() {
		r.svc.afterApply = nil
		r2.putSSH(setBody(genBinding("later", "ssh://git.example.test", "git", host)))
	}
	if out := r.putSSH(setBody(genBinding("gh", "ssh://git.example.test", "git", host))); len(out.Bindings) != 1 ||
		out.Bindings[0].Name != "gh" {
		t.Fatalf("PUT ssh answered %+v, want its own set", out)
	}
}

// Off, every set, preflight and check handler answers the catch-all's 404
// and touches nothing, even when a caller mounts it anyway, and also when
// the Service has no key to seal with.
func TestHandlersAnswerNoSuchRouteWhenOff(t *testing.T) {
	r := newRig(t)
	for name, cfg := range map[string]Config{"off": {Key: r.key}, "no key": {Enabled: true}} {
		cfg.Log = quietLog()
		off, err := New(cfg, r.st, r.d)
		if err != nil {
			t.Fatal(err)
		}
		base := "/api/v1/sandboxes/" + r.sb1.String()
		for _, c := range []struct {
			method, path, body string
			h                  http.HandlerFunc
		}{
			{http.MethodGet, base + "/kube", "", off.GetKube},
			{http.MethodPut, base + "/kube", setBody(tokenBinding("prod", 6443, "https://k8s.example.test")), off.PutKube},
			{http.MethodDelete, base + "/kube", "", off.DeleteKube},
			{http.MethodPost, "/api/v1/kube/preflight", `{"server":"https://k8s.example.test","auth":{"type":"token","token":"t"}}`,
				off.KubePreflight},
			{http.MethodGet, base + "/ssh", "", off.GetSSH},
			{http.MethodPut, base + "/ssh", setBody(genBinding("gh", "ssh://git.example.test", "git",
				hostLine(t, newEd25519(t)))), off.PutSSH},
			{http.MethodDelete, base + "/ssh", "", off.DeleteSSH},
			{http.MethodPost, base + "/ssh/gh/check", "", off.CheckSSH},
			{http.MethodPost, "/api/v1/ssh/preflight", `{"server":"ssh://git.example.test","user":"git"}`, off.SSHPreflight},
		} {
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			req.SetPathValue("id", r.sb1.String())
			req.SetPathValue("name", "gh")
			ctx := auth.WithSandbox(req.Context(), auth.Sandbox{ID: r.sb1, Owner: "acme", TokenHash: store.HashToken(sb1Token)})
			ctx = auth.WithProject(ctx, auth.Project{Name: "acme"})
			w := httptest.NewRecorder()
			c.h(w, req.WithContext(ctx))
			msg, _ := codeOf(w.Body.String())
			if w.Code != http.StatusNotFound || msg != "no such route: "+c.method+" "+c.path {
				t.Errorf("%s: %s %s: HTTP %d %s, want 404 no such route", name, c.method, c.path, w.Code, w.Body)
			}
		}
	}
	if v, rows := r.kubeRows(); v != "" || len(rows) != 0 {
		t.Fatalf("an off PUT stored a Kubernetes set: %q %+v", v, rows)
	}
	if v, rows := r.sshRows(); v != "" || len(rows) != 0 {
		t.Fatalf("an off PUT stored an SSH set: %q %+v", v, rows)
	}
}

// A sandbox-class handler reached without a sandbox in the context (a
// wiring mistake) refuses rather than guessing.
func TestSandboxHandlersNeedASandbox(t *testing.T) {
	r := newRig(t)
	for _, h := range []http.HandlerFunc{r.svc.GetKube, r.svc.PutKube, r.svc.DeleteKube, r.svc.GetSSH,
		r.svc.PutSSH, r.svc.DeleteSSH, r.svc.CheckSSH} {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/sandboxes/"+r.sb1.String()+"/kube", strings.NewReader(`{"bindings":[]}`))
		req.SetPathValue("id", r.sb1.String())
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("HTTP %d %s, want 401", w.Code, w.Body)
		}
	}
}
