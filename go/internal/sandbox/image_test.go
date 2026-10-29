// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/bindings"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/model"
)

func TestParseImageRef(t *testing.T) {
	cases := []struct {
		in, name, host, repo, ref string
	}{
		{"alpine", "docker.io", "registry-1.docker.io", "library/alpine", "latest"},
		{"alpine:3.20", "docker.io", "registry-1.docker.io", "library/alpine", "3.20"},
		{"maltej/debian-ai", "docker.io", "registry-1.docker.io", "maltej/debian-ai", "latest"},
		{"docker.io/library/alpine:3", "docker.io", "registry-1.docker.io", "library/alpine", "3"},
		{"index.docker.io/x/y:1", "docker.io", "registry-1.docker.io", "x/y", "1"},
		{"ghcr.io/maltej/debian-ai:latest", "ghcr.io", "ghcr.io", "maltej/debian-ai", "latest"},
		{"localhost:5000/a/b", "localhost:5000", "localhost:5000", "a/b", "latest"},
		{"ghcr.io/a/b:tag@sha256:abc", "ghcr.io", "ghcr.io", "a/b", "sha256:abc"},
		{"alpine@sha256:abc", "docker.io", "registry-1.docker.io", "library/alpine", "sha256:abc"},
	}
	for _, c := range cases {
		r, err := parseImageRef(c.in)
		if err != nil || r.name != c.name || r.host != c.host || r.repo != c.repo || r.ref != c.ref {
			t.Errorf("%s: %+v %v", c.in, r, err)
		}
	}
	for _, bad := range []string{"", " ", "a b", "x@md5:abc"} {
		if _, err := parseImageRef(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// registry is an anonymous OCI registry with a Bearer token flow, over
// https.
type registry struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	token string
	// manifests by repo and reference: content type and body.
	manifests map[string][2]string
	digests   map[string]string // Docker-Content-Digest per reference ("" = none)
	// realm, when set, is the challenge's realm instead of the registry's
	// own /token; redirects answer a reference with a 307 to their URL.
	realm     string
	redirects map[string]string
	// badToken: /token answers something that is not JSON.
	badToken bool
	// tokens counts the token requests; tokenStatus, when set, is /token's
	// answer instead of a token.
	tokens      int
	tokenStatus int
	// status answers a reference with that status, once authorised;
	// delays hold its answer; noHead answers HEAD with 405.
	status map[string]int
	delays map[string]time.Duration
	noHead bool
	// private repositories take only the token of user's login, given at
	// the realm as Basic authentication; basic answers every reference
	// with a Basic challenge instead; realmAuth is the Authorization the
	// realm saw last.
	private   map[string]bool
	user      [2]string
	basic     bool
	realmAuth string
}

func newRegistry(t *testing.T) *registry {
	r := &registry{hits: map[string]int{}, token: "anon-token", manifests: map[string][2]string{}, digests: map[string]string{},
		redirects: map[string]string{}, status: map[string]int{}, delays: map[string]time.Duration{},
		private: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		bad, code, user := r.badToken, r.tokenStatus, r.user
		r.tokens++
		r.realmAuth = req.Header.Get("Authorization")
		r.mu.Unlock()
		if u, pw, ok := req.BasicAuth(); ok {
			if u != user[0] || pw != user[1] {
				http.Error(w, "bad login", http.StatusUnauthorized)
				return
			}
			fmt.Fprintf(w, `{"token": "user-token", "expires_in": 300}`)
			return
		}
		if bad {
			fmt.Fprint(w, "<html>not a token</html>")
			return
		}
		if code != 0 {
			http.Error(w, http.StatusText(code), code)
			return
		}
		if req.URL.Query().Get("scope") == "" || req.URL.Query().Get("service") != "test-registry" {
			http.Error(w, "bad token request", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"token": %q}`, r.token)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, req *http.Request) {
		key := strings.TrimPrefix(req.URL.Path, "/v2/")
		r.mu.Lock()
		r.hits[key]++
		m, ok := r.manifests[key]
		d := r.digests[key]
		realm, redirect := r.realm, r.redirects[key]
		r.mu.Unlock()
		if redirect != "" {
			http.Redirect(w, req, redirect, http.StatusTemporaryRedirect)
			return
		}
		repo := strings.Split(key, "/manifests/")[0]
		r.mu.Lock()
		private, basic := r.private[repo], r.basic
		r.mu.Unlock()
		if basic {
			w.Header().Set("WWW-Authenticate", `Basic realm="test-registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		want := "Bearer " + r.token
		if private {
			want = "Bearer user-token"
		}
		if got := req.Header.Get("Authorization"); got != want && !(got == "Bearer user-token" && !private) {
			if realm == "" {
				realm = r.srv.URL + "/token"
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s",service="test-registry",scope="repository:%s:pull"`,
				realm, repo))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !strings.Contains(req.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			http.Error(w, "no accept", http.StatusBadRequest)
			return
		}
		if key == "limited/x/manifests/latest" {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		r.mu.Lock()
		code, delay, noHead := r.status[key], r.delays[key], r.noHead
		r.mu.Unlock()
		time.Sleep(delay)
		if code != 0 {
			http.Error(w, http.StatusText(code), code)
			return
		}
		if noHead && req.Method == http.MethodHead {
			http.Error(w, "no HEAD here", http.StatusMethodNotAllowed)
			return
		}
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", m[0])
		if d != "" {
			w.Header().Set("Docker-Content-Digest", d)
		}
		fmt.Fprint(w, m[1])
	})
	r.srv = httptest.NewTLSServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *registry) host() string { return strings.TrimPrefix(r.srv.URL, "https://") }

// resolver returns a resolver that trusts the registry's certificate,
// under the destination rule deny (none: every IPv4 address).
func (r *registry) resolver(deny ...netip.Prefix) *resolver {
	res := newResolver(deny)
	res.transport.TLSClientConfig = r.srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return res
}

func (r *registry) set(f func(r *registry)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r)
}

func (r *registry) put(key, mediaType, body, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifests[key] = [2]string{mediaType, body}
	r.digests[key] = digest
}

func (r *registry) hitsOf(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[key]
}

func (r *registry) tokenRequests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokens
}

func TestResolver(t *testing.T) {
	reg := newRegistry(t)
	const amd = "sha256:" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	reg.put("team/app/manifests/v1", "application/vnd.oci.image.index.v1+json", `{"mediaType":"application/vnd.oci.image.index.v1+json",
		"manifests":[{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},
		{"digest":"`+amd+`","platform":{"os":"linux","architecture":"amd64"}}]}`, "sha256:index")
	manifest := `{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:cfg"}}`
	reg.put("team/plain/manifests/v1", "application/vnd.oci.image.manifest.v1+json", manifest, "")
	reg.put("team/arm/manifests/v1", "application/vnd.oci.image.index.v1+json",
		`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}}]}`, "")

	now := time.Unix(1_700_000_000, 0)
	r := reg.resolver()
	r.now = func() time.Time { return now }
	host := reg.host()

	// An index: the linux/amd64 member's digest, after the anonymous
	// token flow.
	got, err := r.resolve(bg, host+"/team/app:v1")
	if err != nil || got != host+"/team/app@"+amd {
		t.Fatalf("index: %q %v", got, err)
	}
	if reg.hitsOf("team/app/manifests/v1") != 2 {
		t.Fatalf("hits %d (challenge and answer)", reg.hitsOf("team/app/manifests/v1"))
	}
	// Cached for 60 s.
	if again, err := r.resolve(bg, host+"/team/app:v1"); err != nil || again != got || reg.hitsOf("team/app/manifests/v1") != 2 {
		t.Fatalf("cached: %q %v, hits %d", again, err, reg.hitsOf("team/app/manifests/v1"))
	}
	now = now.Add(61 * time.Second)
	if _, err := r.resolve(bg, host+"/team/app:v1"); err != nil || reg.hitsOf("team/app/manifests/v1") != 4 {
		t.Fatalf("after the TTL: %v, hits %d", err, reg.hitsOf("team/app/manifests/v1"))
	}

	// A manifest without Docker-Content-Digest: the digest of its body.
	got, err = r.resolve(bg, host+"/team/plain:v1")
	if want := fmt.Sprintf("%s/team/plain@sha256:%x", host, sha256.Sum256([]byte(manifest))); err != nil || got != want {
		t.Fatalf("manifest: %q %v, want %q", got, err, want)
	}

	// A digest reference is passed through without asking the registry.
	d := "sha256:" + strings.Repeat("b", 64)
	if got, err := r.resolve(bg, host+"/team/other@"+d); err != nil || got != host+"/team/other@"+d || reg.hitsOf("team/other/manifests/"+d) != 0 {
		t.Fatalf("digest: %q %v", got, err)
	}
	if got, _ := r.resolve(bg, "alpine@"+d); got != "docker.io/library/alpine@"+d {
		t.Fatalf("docker hub digest: %q", got)
	}

	// Failures.
	if _, err := r.resolve(bg, host+"/team/arm:v1"); err == nil || !strings.Contains(err.Error(), "no linux/amd64 variant") {
		t.Fatalf("arm only: %v", err)
	}
	if _, err := r.resolve(bg, host+"/team/missing:v1"); err == nil || !strings.Contains(err.Error(), "not found, or not for these credentials") {
		t.Fatalf("missing: %v", err)
	}
	if _, err := r.resolve(bg, host+"/limited/x"); err == nil || !strings.Contains(err.Error(), "rate-limited") {
		t.Fatalf("rate limited: %v", err)
	}
	if _, err := r.resolve(bg, "127.0.0.1:1/nothing/here:v1"); err == nil || err.Error() != "registry 127.0.0.1:1 unreachable" {
		t.Fatalf("unreachable: %v", err)
	}
	// (A repository whose token is not kept yet: the token is asked for.)
	reg.set(func(r *registry) { r.badToken = true })
	if _, err := r.resolve(bg, host+"/team/fresh:v1"); err == nil || err.Error() != "token realm returned no token" {
		t.Fatalf("a token realm that answers no JSON: %v", err)
	}
}

// The resolver reaches what a tenant's image names under the destination
// rule of bindings, on the address it dials: a registry, the target of a
// redirect and a token realm alike, each over https only; it tells which
// address it refused, never what a dial found there.
func TestResolverDestinationRule(t *testing.T) {
	reg := newRegistry(t)
	reg.put("team/app/manifests/v1", "application/vnd.oci.image.manifest.v1+json", `{"config":{}}`, "sha256:"+strings.Repeat("d", 64))
	host := reg.host()
	// A plain-http server that nothing may reach.
	var plainHits atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
		fmt.Fprint(w, `{"token": "leaked"}`)
	}))
	defer plain.Close()

	// The default rule refuses the loopback, an address as a name, and a
	// cluster address, before anything is sent.
	def := reg.resolver(bindings.DefaultDenyPrefixes()...)
	port := strings.Split(host, ":")[1]
	for image, want := range map[string]string{
		host + "/team/app:v1": "registry " + host + " is not reachable from the control plane: " +
			"127.0.0.1 is an internal address (127.0.0.0/8)",
		"localhost:" + port + "/team/app:v1": "registry localhost:" + port + " is not reachable from the control plane: " +
			"127.0.0.1 is an internal address (127.0.0.0/8)",
		"10.43.0.1:5432/x/y:z": "registry 10.43.0.1:5432 is not reachable from the control plane: " +
			"10.43.0.1 is an internal address (10.0.0.0/8)",
	} {
		if _, err := def.resolve(bg, image); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", image, err, want)
		}
	}
	if reg.hitsOf("team/app/manifests/v1") != 0 {
		t.Fatal("the registry was reached")
	}

	// A rule that allows the registry's address: the realm and the
	// redirects are judged on their own.
	r := reg.resolver(netip.MustParsePrefix("10.0.0.0/8"))
	for _, c := range []struct {
		name, realm, redirect, want string
	}{
		{"realm over http", plain.URL + "/token", "",
			"unsupported auth challenge: the token realm is not an https URL"},
		{"realm on a denied address", "https://10.255.0.1/token", "",
			"token realm 10.255.0.1 is not reachable from the control plane: 10.255.0.1 is an internal address (10.0.0.0/8)"},
		{"redirect to http", "", plain.URL + "/v2/team/app/manifests/v1",
			"registry " + host + " redirected to a URL that is not https"},
		{"redirect to a denied address", "", "https://10.1.2.3/v2/team/app/manifests/v1",
			"registry " + host + " is not reachable from the control plane: 10.1.2.3 is an internal address (10.0.0.0/8)"},
	} {
		reg.set(func(r *registry) {
			r.realm = c.realm
			r.redirects["team/app/manifests/v1"] = c.redirect
		})
		r.cache = map[string]resolved{}
		if _, err := r.resolve(bg, host+"/team/app:v1"); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("the plain-http server was reached %d times", n)
	}
	reg.set(func(r *registry) {
		r.realm = ""
		r.redirects = map[string]string{}
	})
	if got, err := r.resolve(bg, host+"/team/app:v1"); err != nil || got != host+"/team/app@sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("allowed: %q %v", got, err)
	}
}

// The engine's resolver applies the default rule when --deny-prefix names
// none.
func TestEngineResolverDefaultRule(t *testing.T) {
	h := newHarness(t)
	e, err := New(Config{InstanceID: "resolver", Log: dbtest.Logger(t)}, Deps{DB: h.d, Conns: h.ins[0].srv})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.resolveImage(bg, "127.0.0.1:1/x/y:z", model.RegistryCredentials{})
	if want := "registry 127.0.0.1:1 is not reachable from the control plane: 127.0.0.1 is an internal address (127.0.0.0/8)"; err == nil ||
		err.Error() != want {
		t.Fatalf("%v, want %q", err, want)
	}
}

func TestEnsureResolvesAndPinsImage(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	reg := newRegistry(t)
	const amd = "sha256:" + "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	reg.put("team/app/manifests/v1", "application/vnd.oci.image.index.v1+json",
		`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"`+amd+`","platform":{"os":"linux","architecture":"amd64"}}]}`, "")
	res := reg.resolver()
	e.resolveImage, e.checkImage = res.resolveAs, res.access
	reg.put("team/app/manifests/"+amd, "application/vnd.oci.image.manifest.v1+json", `{}`, amd)
	image := reg.host() + "/team/app:v1"
	sb := h.sandbox(512)
	h.exec("UPDATE sandboxes SET image = $2 WHERE id = $1", sb.ID, image)
	h.ensure(sb.ID)
	row := h.row(sb.ID)
	if row.ImageRef != reg.host()+"/team/app@"+amd || row.Image != image {
		t.Fatalf("image %q pinned to %q", row.Image, row.ImageRef)
	}
	info, err := h.pods("n1").GetPod(bg, podReq(sb.ID))
	if err != nil || info.GetSpec().GetContainers()[0].GetImageRef() != row.ImageRef {
		t.Fatalf("pod image %v %v", info.GetSpec().GetContainers(), err)
	}
	// A tag that moves later does not change the sandbox's image.
	reg.put("team/app/manifests/v1", "application/vnd.oci.image.index.v1+json",
		`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:moved","platform":{"os":"linux","architecture":"amd64"}}]}`, "")
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.ensure(sb.ID)
	if h.row(sb.ID).ImageRef != row.ImageRef {
		t.Fatal("the pinned image changed")
	}
	// An image that cannot be resolved fails the Ensure.
	bad := h.sandbox(512)
	h.exec("UPDATE sandboxes SET image = $2 WHERE id = $1", bad.ID, reg.host()+"/team/missing:v1")
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	_, err = e.Ensure(ctx, bad.ID)
	wantErr(t, err, nil, "resolve image "+reg.host()+"/team/missing:v1: not found")
	wantState(t, h.row(bad.ID), model.StatePending)
}

// anon is an asker without credentials.
var anon = model.RegistryCredentials{}

// A create with credentials (ADR 0025): the realm gets them as Basic
// authentication and gives their token, which is kept apart from the
// anonymous one, as are the tags resolved with them; a registry that
// takes only Basic authentication is refused, and the guest's pull token
// is asked for anew.
func TestResolverWithCredentials(t *testing.T) {
	reg := newRegistry(t)
	const mt = "application/vnd.oci.image.manifest.v1+json"
	body := `{"mediaType":"` + mt + `","config":{"digest":"sha256:c1"}}`
	d := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body)))
	reg.put("team/private/manifests/"+d, mt, body, d)
	reg.put("team/private/manifests/v1", mt, body, d)
	reg.set(func(r *registry) {
		r.private["team/private"] = true
		r.user = [2]string{"alice", "s3cret"}
	})
	res := reg.resolver()
	alice := model.RegistryCredentials{Username: "alice", Password: "s3cret"}
	pinned := reg.host() + "/team/private@" + d

	// Anonymous: refused, and the tag does not resolve.
	if err := res.access(bg, pinned, anon); !errors.Is(err, errImageRefused) {
		t.Fatalf("anonymous: %v", err)
	}
	if _, err := res.resolve(bg, reg.host()+"/team/private:v1"); err == nil {
		t.Fatal("anonymous resolve of a private tag")
	}
	// Her login: a yes, the token of her login at the realm, Basic.
	if err := res.access(bg, pinned, alice); err != nil {
		t.Fatalf("alice: %v", err)
	}
	reg.mu.Lock()
	realmAuth := reg.realmAuth
	reg.mu.Unlock()
	if u, pw, ok := (&http.Request{Header: http.Header{"Authorization": {realmAuth}}}).BasicAuth(); !ok || u != "alice" || pw != "s3cret" {
		t.Fatalf("the realm saw %q", realmAuth)
	}
	if got, err := res.resolveAs(bg, reg.host()+"/team/private:v1", alice); err != nil || got != pinned {
		t.Fatalf("alice's resolve: %q %v", got, err)
	}
	// What she resolved and her token are hers: anonymous gets neither.
	if _, err := res.resolve(bg, reg.host()+"/team/private:v1"); err == nil {
		t.Fatal("an anonymous resolve took alice's tag or token")
	}
	if err := res.access(bg, pinned, anon); !errors.Is(err, errImageRefused) {
		t.Fatalf("anonymous after alice: %v", err)
	}
	// Another password is refused at the realm.
	if err := res.access(bg, pinned, model.RegistryCredentials{Username: "alice", Password: "guess"}); !errors.Is(err, errImageRefused) {
		t.Fatalf("a wrong password: %v", err)
	}
	// The guest's pull token: asked for anew, her login's; none for an
	// anonymous create.
	before := reg.tokenRequests()
	if tok, err := res.pullToken(bg, pinned, alice); err != nil || tok != "user-token" || reg.tokenRequests() != before+1 {
		t.Fatalf("pull token %q %v, %d token requests", tok, err, reg.tokenRequests()-before)
	}
	if tok, err := res.pullToken(bg, pinned, anon); err != nil || tok != "" {
		t.Fatalf("anonymous pull token %q %v", tok, err)
	}
	// A registry that takes only Basic authentication: refused with
	// credentials, as anonymously.
	reg.set(func(r *registry) { r.basic = true })
	if err := res.access(bg, reg.host()+"/team/basic@"+d, alice); !errors.Is(err, errBasicOnly) {
		t.Fatalf("basic only: %v", err)
	}
	if _, err := res.pullToken(bg, reg.host()+"/team/basic@"+d, alice); !errors.Is(err, errBasicOnly) {
		t.Fatalf("basic only pull token: %v", err)
	}
	if err := res.access(bg, reg.host()+"/team/basic@"+d, anon); !errors.Is(err, errImageRefused) {
		t.Fatalf("basic only, anonymous: %v", err)
	}
}

// The question before a create (ADR 0025): HEAD of the manifest by its
// digest with the realm's token, which is kept; the registry's no, and
// no answer, which is no as well.
func TestResolverAccess(t *testing.T) {
	reg := newRegistry(t)
	const mt = "application/vnd.oci.image.manifest.v1+json"
	body := `{"mediaType":"` + mt + `","config":{"digest":"sha256:c0"}}`
	d := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body)))
	other := "sha256:" + strings.Repeat("1", 64)
	for _, repo := range []string{"team/app", "team/slow", "team/head"} {
		reg.put(repo+"/manifests/"+d, mt, body, d)
	}
	res := reg.resolver()
	pinned := func(repo, digest string) string { return reg.host() + "/" + repo + "@" + digest }

	// A yes, with the realm's token; the next question takes the token
	// kept and asks once.
	for range 2 {
		if err := res.access(bg, pinned("team/app", d), anon); err != nil {
			t.Fatal(err)
		}
	}
	if n, h := reg.tokenRequests(), reg.hitsOf("team/app/manifests/"+d); n != 1 || h != 3 {
		t.Fatalf("%d token requests and %d questions, want 1 and 3", n, h)
	}
	noAnswerWith := func(err error, text string) {
		t.Helper()
		var na *noAnswerError
		if !errors.As(err, &na) || !strings.Contains(err.Error(), text) {
			t.Fatalf("%v, want no answer with %q", err, text)
		}
	}
	// The registry does not have it, or refuses it.
	if err := res.access(bg, pinned("team/app", other), anon); !errors.Is(err, errImageNotThere) {
		t.Fatalf("missing: %v", err)
	}
	reg.set(func(r *registry) { r.status["team/secret/manifests/"+d] = http.StatusForbidden })
	if err := res.access(bg, pinned("team/secret", d), anon); !errors.Is(err, errImageRefused) {
		t.Fatalf("forbidden: %v", err)
	}
	// An answer for another digest, a rate limit or a failure is none.
	reg.put("team/liar/manifests/"+d, mt, body, other)
	noAnswerWith(res.access(bg, pinned("team/liar", d), anon), "answered for "+other)
	reg.set(func(r *registry) {
		r.status["team/busy/manifests/"+d] = http.StatusTooManyRequests
		r.status["team/down/manifests/"+d] = http.StatusServiceUnavailable
	})
	noAnswerWith(res.access(bg, pinned("team/busy", d), anon), "rate-limited")
	noAnswerWith(res.access(bg, pinned("team/down", d), anon), "HTTP 503")
	noAnswerWith(newResolver(nil).access(bg, "127.0.0.1:1/x/y@"+d, anon), "registry 127.0.0.1:1 unreachable")
	// A registry that does not answer HEAD is asked with GET, and the
	// body must be the digest's.
	reg.set(func(r *registry) { r.noHead = true })
	if err := res.access(bg, pinned("team/head", d), anon); err != nil {
		t.Fatalf("GET: %v", err)
	}
	reg.put("team/forged/manifests/"+d, mt, `{"forged":true}`, "")
	noAnswerWith(res.access(bg, pinned("team/forged", d), anon), "served a manifest of")
	reg.set(func(r *registry) { r.noHead = false })
	// A realm that refuses a token refuses the image.
	reg.set(func(r *registry) { r.tokenStatus = http.StatusUnauthorized })
	if err := res.access(bg, pinned("team/other", d), anon); !errors.Is(err, errImageRefused) {
		t.Fatalf("realm refused: %v", err)
	}
	reg.set(func(r *registry) { r.tokenStatus = 0 })
	// Only a pinned image is asked for.
	if err := res.access(bg, reg.host()+"/team/app:v1", anon); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("a tag: %v", err)
	}
	// Questions at once share one request: the challenge and the answer.
	reg.set(func(r *registry) { r.delays["team/slow/manifests/"+d] = 300 * time.Millisecond })
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = res.access(bg, pinned("team/slow", d), anon) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if h := reg.hitsOf("team/slow/manifests/" + d); h != 2 {
		t.Fatalf("%d requests for 8 questions at once, want 2", h)
	}
}
