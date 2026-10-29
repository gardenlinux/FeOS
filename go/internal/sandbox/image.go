// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/gardenlinux/feos/go/internal/bindings"
)

// The image resolver pins an image to the digest of its linux/amd64
// manifest, so that a sandbox never silently changes image and a tag that
// moves never breaks CreatePod's idempotency (§9 "Image resolution"), and
// asks an image's registry before every create whether the image may be
// pulled (access, ADR 0025). It speaks the anonymous registry protocol
// only, over https: private registries are not supported yet.
const (
	resolveTTL          = 60 * time.Second
	resolveTimeout      = 30 * time.Second
	resolveDialTimeout  = 10 * time.Second
	resolveManifestMax  = 8 << 20
	dockerHub           = "docker.io"
	dockerHubRegistry   = "registry-1.docker.io"
	resolveManifestType = "application/vnd.oci.image.index.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, " +
		"application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.v2+json"
)

const (
	// tokenTTL is a registry token's life when its realm names none, as
	// the distribution spec says; tokenSlack is how long before its end
	// a token is no longer used.
	tokenTTL   = 60 * time.Second
	tokenSlack = 10 * time.Second
	// anonymous is the asker without credentials, in the keys of the
	// caches and of the shared questions: a create's credentials will key
	// them by an HMAC of their own (ADR 0025).
	anonymous = ""
)

// errImageNotFound is the answer of a registry that does not serve the
// image anonymously: it does not exist, or is private.
var errImageNotFound = errors.New("not found (private registries are not supported and look like this)")

// The registry's no to the question before a create (access): it refused
// the asker, or does not have the image for it (a private repository
// looks like that too).
var (
	errImageRefused  = errors.New("the registry refused it")
	errImageNotThere = errors.New("the registry does not have it")
	// errRealmRefused is a token realm that refused a token (401, 403,
	// 404).
	errRealmRefused = errors.New("the token realm refused a token")
)

// noAnswerError is a question before a create that the registry did not
// answer: unreachable, too slow, rate-limited, failing, or answering for
// another digest. It is no, and the create asks again later.
type noAnswerError struct{ err error }

func (e *noAnswerError) Error() string { return e.err.Error() }
func (e *noAnswerError) Unwrap() error { return e.err }

func noAnswer(format string, args ...any) error { return &noAnswerError{fmt.Errorf(format, args...)} }

var authParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// imageRef is an image reference split as Docker does.
type imageRef struct {
	// name is the registry as it is named in a pinned reference
	// ("docker.io" for Docker Hub); host is the one that serves it.
	name, host string
	repo       string
	// ref is the tag, or the digest ("sha256:…").
	ref string
}

func (r imageRef) digest() bool { return strings.HasPrefix(r.ref, "sha256:") }

// pinned is the reference with the manifest digest d.
func (r imageRef) pinned(d string) string { return r.name + "/" + r.repo + "@" + d }

// parseImageRef splits "registry/repo[:tag][@digest]": the registry is the
// first component when it has a dot or a colon or is localhost, else
// Docker Hub (with "library/" for a one-component repository); the tag is
// "latest" when none is given.
func parseImageRef(s string) (imageRef, error) {
	if strings.TrimSpace(s) == "" {
		return imageRef{}, errors.New("empty image reference")
	}
	var r imageRef
	rest := s
	if i := strings.Index(rest, "@"); i >= 0 {
		r.ref, rest = rest[i+1:], rest[:i]
		// A tag before the digest is ignored: the digest names the image.
		if j := strings.LastIndex(rest, ":"); j > strings.LastIndex(rest, "/") {
			rest = rest[:j]
		}
	} else if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		r.ref, rest = rest[i+1:], rest[:i]
	} else {
		r.ref = "latest"
	}
	if parts := strings.SplitN(rest, "/", 2); len(parts) == 2 &&
		(strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		r.name, r.repo = parts[0], parts[1]
	} else {
		r.name, r.repo = dockerHub, rest
	}
	if r.name == "index.docker.io" || r.name == dockerHubRegistry {
		r.name = dockerHub
	}
	if r.name == dockerHub && !strings.Contains(r.repo, "/") {
		r.repo = "library/" + r.repo
	}
	r.host = r.name
	if r.name == dockerHub {
		r.host = dockerHubRegistry
	}
	if r.repo == "" || r.ref == "" || strings.ContainsAny(r.repo+r.ref, " \t\r\n") {
		return imageRef{}, fmt.Errorf("invalid image reference %q", s)
	}
	if strings.Contains(s, "@") && !r.digest() {
		return imageRef{}, fmt.Errorf("invalid image reference %q: only sha256 digests", s)
	}
	return r, nil
}

// resolver resolves image references with a cache of tags.
type resolver struct {
	client *http.Client
	// transport is the client's; the tests give it their TLS roots.
	transport *http.Transport
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]resolved
	// tokens are the registries' tokens, by asker, host and scope, kept
	// until shortly before their end: the registry's own yes, for as long
	// as it gives it, and the only answer kept.
	tokens map[tokenKey]heldToken
	// asks shares questions of the same asker and image at once.
	asks singleflight.Group
}

type resolved struct {
	pinned  string
	expires time.Time
}

// tokenKey names a registry token: who asked (anonymous, or a create's
// credentials), the registry and the token's scope.
type tokenKey struct{ who, host, scope string }

type heldToken struct {
	token string
	until time.Time
}

// newResolver returns a resolver that reaches registries over https only,
// without a proxy, and dials only IPv4 addresses outside deny: the
// destination rule of bindings and preflights (--deny-prefix), since an
// image names its registry and the tenant names the image. Every
// connection is judged on the address actually dialled, to a registry,
// to the target of a redirect and to a token realm alike, so that a name
// that resolves to an internal address, at once or a moment later, is
// refused too.
func newResolver(deny []netip.Prefix) *resolver {
	allow := bindings.DenyPolicy(deny)
	d := &net.Dialer{Timeout: resolveDialTimeout, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return &deniedError{fmt.Errorf("unparseable address %q", address)}
		}
		if err := allow(ap.Addr()); err != nil {
			return &deniedError{err}
		}
		return nil
	}}
	t := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp4", addr)
		},
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &resolver{client: &http.Client{Transport: t, CheckRedirect: httpsRedirect}, transport: t, now: time.Now,
		cache: map[string]resolved{}, tokens: map[tokenKey]heldToken{}}
}

// deniedError is a dial the destination rule refused.
type deniedError struct{ err error }

func (e *deniedError) Error() string { return "destination denied: " + e.err.Error() }

// errNotHTTPS is a redirect to a URL that is not https.
var errNotHTTPS = errors.New("redirect to a URL that is not https")

// httpsRedirect follows at most 10 redirects, each to an https URL.
func httpsRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Scheme != "https" {
		return errNotHTTPS
	}
	return nil
}

// unreachable is the error of a request to what (a registry, a token
// realm) that got no answer. It says that the destination rule refused
// the address, or that a redirect was not https, or that the host does
// not exist; otherwise only that what is unreachable: the dial's own
// error would tell an open port from a closed or filtered one. The end of
// ctx stays in the chain, for the caller's budget.
func unreachable(ctx context.Context, what string, err error) error {
	var denied *deniedError
	var dns *net.DNSError
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("%s unreachable: %w", what, ctx.Err())
	case errors.As(err, &denied):
		return fmt.Errorf("%s is not reachable from the control plane: %v", what, denied.err)
	case errors.Is(err, errNotHTTPS):
		return fmt.Errorf("%s redirected to a URL that is not https", what)
	case errors.As(err, &dns) && dns.IsNotFound:
		return fmt.Errorf("%s unreachable: no such host", what)
	}
	return fmt.Errorf("%s unreachable", what)
}

// resolve returns "<registry>/<repository>@sha256:<manifest digest>" for
// image: for an index, the digest of its linux/amd64 manifest. A digest
// reference is passed through without asking the registry; a tag is
// cached for 60 s.
func (r *resolver) resolve(ctx context.Context, image string) (string, error) {
	ref, err := parseImageRef(image)
	if err != nil {
		return "", err
	}
	if ref.digest() {
		return ref.pinned(ref.ref), nil
	}
	// The tag cache is the asker's: what one asker resolved says nothing
	// about another's access.
	key := anonymous + "\x00" + image
	now := r.now()
	r.mu.Lock()
	if c, ok := r.cache[key]; ok && now.Before(c.expires) {
		r.mu.Unlock()
		return c.pinned, nil
	}
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	body, digest, err := r.manifest(ctx, ref, ref.ref)
	if err != nil {
		return "", err
	}
	var probe struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", fmt.Errorf("%s/%s: unparseable manifest", ref.name, ref.repo)
	}
	if strings.HasSuffix(probe.MediaType, "image.index.v1+json") || strings.HasSuffix(probe.MediaType, "manifest.list.v2+json") ||
		(probe.MediaType == "" && len(probe.Manifests) > 0) {
		digest = ""
		for _, m := range probe.Manifests {
			if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
				digest = m.Digest
				break
			}
		}
		if digest == "" {
			return "", fmt.Errorf("%s/%s: no linux/amd64 variant in this image index", ref.name, ref.repo)
		}
	} else if digest == "" {
		digest = fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	}
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("%s/%s: unsupported digest %q", ref.name, ref.repo, digest)
	}
	pinned := ref.pinned(digest)
	r.mu.Lock()
	r.cache[key] = resolved{pinned: pinned, expires: now.Add(resolveTTL)}
	for k, c := range r.cache {
		if !now.Before(c.expires) {
			delete(r.cache, k)
		}
	}
	r.mu.Unlock()
	return pinned, nil
}

// access asks the registry of pinned, a reference with a digest, whether
// the asker may pull that manifest, before every create of a pod that
// runs it (ADR 0025): HEAD /v2/<repository>/manifests/<digest> (a GET
// where the registry does not answer HEAD), the registry's Bearer
// challenge answered with a token of its realm. nil is a yes;
// errImageRefused and errImageNotThere are the registry's no; a
// *noAnswerError is no answer, and no as well. Nothing but the
// registry's token is kept, and questions of the same asker and image at
// once share one request, which runs on a context of its own, bounded by
// resolveTimeout, so that a caller who goes away does not end the others'.
func (r *resolver) access(ctx context.Context, pinned string) error {
	ref, err := parseImageRef(pinned)
	if err != nil {
		return err
	}
	if !ref.digest() {
		return fmt.Errorf("image %s is not pinned to a digest", pinned)
	}
	key := anonymous + "\x00" + ref.pinned(ref.ref)
	ch := r.asks.DoChan(key, func() (any, error) {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resolveTimeout)
		defer cancel()
		return nil, r.ask(actx, ref)
	})
	select {
	case res := <-ch:
		return res.Err
	case <-ctx.Done():
		return &noAnswerError{fmt.Errorf("registry %s: %w", ref.host, ctx.Err())}
	}
}

// ask is access's request: with the token kept, or none; a 401 drops it
// and answers the challenge once; a 405 to HEAD asks with GET.
func (r *resolver) ask(ctx context.Context, ref imageRef) error {
	u := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.host, ref.repo, ref.ref)
	tk := tokenKey{who: anonymous, host: ref.host, scope: "repository:" + ref.repo + ":pull"}
	method := http.MethodHead
	token := r.heldToken(tk)
	challenged := false
	for {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", resolveManifestType)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return &noAnswerError{unreachable(ctx, "registry "+ref.host, err)}
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && !challenged:
			challenge := resp.Header.Get("WWW-Authenticate")
			discard(resp)
			r.dropToken(tk, token)
			challenged = true
			token, err = r.bearer(ctx, tk, challenge)
			switch {
			case errors.Is(err, errRealmRefused):
				return errImageRefused
			case err != nil && strings.HasPrefix(err.Error(), "unsupported auth challenge"):
				// A login this asker does not have, as Basic
				// authentication asks for.
				return fmt.Errorf("%w: %v", errImageRefused, err)
			case err != nil:
				return &noAnswerError{err}
			}
			continue
		case resp.StatusCode == http.StatusMethodNotAllowed && method == http.MethodHead:
			discard(resp)
			method = http.MethodGet
			continue
		}
		return answer(resp, method, ref)
	}
}

// answer reads the registry's answer to access's request.
func answer(resp *http.Response, method string, ref imageRef) error {
	defer discard(resp)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return errImageRefused
	case http.StatusNotFound:
		return errImageNotThere
	case http.StatusTooManyRequests:
		return noAnswer("registry %s rate-limited the question", ref.host)
	default:
		return noAnswer("registry %s answered HTTP %d", ref.host, resp.StatusCode)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" && d != ref.ref {
		return noAnswer("registry %s answered for %s, not %s", ref.host, d, ref.ref)
	}
	if method == http.MethodGet {
		body, err := io.ReadAll(io.LimitReader(resp.Body, resolveManifestMax))
		if err != nil {
			return noAnswer("registry %s: manifest read: %v", ref.host, err)
		}
		if d := fmt.Sprintf("sha256:%x", sha256.Sum256(body)); d != ref.ref {
			return noAnswer("registry %s served a manifest of %s, not %s", ref.host, d, ref.ref)
		}
	}
	return nil
}

// manifest fetches a manifest anonymously, answering a Bearer challenge
// with an anonymous token; it returns the body and the digest the
// registry names (Docker-Content-Digest), if any.
func (r *resolver) manifest(ctx context.Context, ref imageRef, tagOrDigest string) ([]byte, string, error) {
	u := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.host, ref.repo, tagOrDigest)
	get := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", resolveManifestType)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return r.client.Do(req)
	}
	tk := tokenKey{who: anonymous, host: ref.host, scope: "repository:" + ref.repo + ":pull"}
	held := r.heldToken(tk)
	resp, err := get(held)
	if err != nil {
		return nil, "", unreachable(ctx, "registry "+ref.host, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		discard(resp)
		r.dropToken(tk, held)
		token, err := r.bearer(ctx, tk, challenge)
		if errors.Is(err, errRealmRefused) {
			return nil, "", errImageNotFound
		}
		if err != nil {
			return nil, "", err
		}
		if resp, err = get(token); err != nil {
			return nil, "", unreachable(ctx, "registry "+ref.host, err)
		}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		return nil, "", errImageNotFound
	case http.StatusTooManyRequests:
		return nil, "", errors.New("registry rate-limited the resolve; retry")
	default:
		return nil, "", fmt.Errorf("registry answered HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, resolveManifestMax))
	if err != nil {
		return nil, "", fmt.Errorf("manifest read: %w", err)
	}
	return body, resp.Header.Get("Docker-Content-Digest"), nil
}

// bearer answers a WWW-Authenticate Bearer challenge for the token tk
// names, and keeps the token until shortly before its end.
func (r *resolver) bearer(ctx context.Context, tk tokenKey, challenge string) (string, error) {
	token, ttl, err := r.token(ctx, challenge)
	if err != nil {
		return "", err
	}
	if ttl > tokenSlack {
		now := r.now()
		r.mu.Lock()
		for k, t := range r.tokens {
			if !now.Before(t.until) {
				delete(r.tokens, k)
			}
		}
		r.tokens[tk] = heldToken{token: token, until: now.Add(ttl - tokenSlack)}
		r.mu.Unlock()
	}
	return token, nil
}

// heldToken returns the token kept for tk, or "".
func (r *resolver) heldToken(tk tokenKey) string {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tokens[tk]; ok && now.Before(t.until) {
		return t.token
	}
	return ""
}

// dropToken forgets the token kept for tk if it is token: the registry no
// longer took it.
func (r *resolver) dropToken(tk tokenKey, token string) {
	if token == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tokens[tk]; ok && t.token == token {
		delete(r.tokens, tk)
	}
}

// discard reads a little of a body that is not used, and closes it.
func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
}

// token answers a WWW-Authenticate Bearer challenge anonymously, and
// returns the token's life (expires_in, 60 s when the realm names none).
// A realm that refuses is errRealmRefused.
func (r *resolver) token(ctx context.Context, challenge string) (string, time.Duration, error) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(challenge)), "bearer") {
		return "", 0, errors.New("unsupported auth challenge (anonymous pulls only)")
	}
	params := map[string]string{}
	for _, m := range authParam.FindAllStringSubmatch(challenge, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", 0, errors.New("unsupported auth challenge (anonymous pulls only)")
	}
	// The realm is fetched as the registry is: over https, under the
	// destination rule.
	u, err := url.Parse(realm)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", 0, errors.New("unsupported auth challenge: the token realm is not an https URL")
	}
	q := u.Query()
	if v := params["service"]; v != "" {
		q.Set("service", v)
	}
	if v := params["scope"]; v != "" {
		q.Set("scope", v)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", 0, unreachable(ctx, "token realm "+u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
			return "", 0, errRealmRefused
		}
		return "", 0, fmt.Errorf("token realm answered HTTP %d", resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", 0, errors.New("token realm returned no token")
	}
	ttl := tokenTTL
	if tok.ExpiresIn > 0 {
		ttl = time.Duration(min(tok.ExpiresIn, 24*3600)) * time.Second
	}
	if tok.Token != "" {
		return tok.Token, ttl, nil
	}
	if tok.AccessToken != "" {
		return tok.AccessToken, ttl, nil
	}
	return "", 0, errors.New("token realm returned no token")
}
