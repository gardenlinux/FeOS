// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ports

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// Sandboxes is what the door needs of the sandbox engine
// (*sandbox.Engine).
type Sandboxes interface {
	Ensure(ctx context.Context, id podid.ID) (model.Lease, error)
	Acquire(ctx context.Context, id podid.ID, kind model.HoldKind) (model.Lease, func(), error)
	Sync(ctx context.Context, id podid.ID) error
	Touch(id podid.ID)
}

// KubePorts names the loopback ports of a sandbox's Kubernetes bindings,
// which the door never forwards (*bindings.Service).
type KubePorts interface {
	KubePorts(ctx context.Context, id podid.ID) ([]int, error)
}

// timings are the door's limits and periods; tests shorten them.
type timings struct {
	// perSandbox bounds the Forward streams of one sandbox on this
	// instance, pooled ones included.
	perSandbox int
	// capWait is how long a request waits for its turn at a connection of
	// its pool, in the pool's queue and for its dial's slot of the
	// sandbox's cap together, and a dial for its slot, before the request
	// is answered 503. The rest of a dial is not bounded by it.
	capWait time.Duration
	// maxIdlePerHost bounds the idle connections of one (pod, port)
	// transport; idleTimeout closes a pooled one.
	maxIdlePerHost int
	idleTimeout    time.Duration
	// maxTransports bounds the transports kept; unusedAfter evicts one
	// that served nothing for that long, checked every sweepEvery.
	maxTransports int
	unusedAfter   time.Duration
	sweepEvery    time.Duration
	// wakeTimeout bounds a detached Sync and Ensure.
	wakeTimeout time.Duration
	// upgradedWait bounds CloseUpgraded's wait for the upgraded
	// connections' requests to end.
	upgradedWait time.Duration
	// rowTimeout bounds Run's read of a sandbox's row.
	rowTimeout time.Duration
	// probeTimeout bounds a Probe and a DialPod.
	probeTimeout time.Duration
	// probeSlots bounds the checks (Probe, DialPod) of one sandbox open at
	// once on this instance (ProbeSlots).
	probeSlots int
}

func defaultTimings() timings {
	return timings{
		perSandbox:     32,
		capWait:        30 * time.Second,
		maxIdlePerHost: 8,
		idleTimeout:    90 * time.Second,
		maxTransports:  4096,
		unusedAfter:    90 * time.Second,
		sweepEvery:     30 * time.Second,
		// The engine bounds an Ensure by the preparation budget (7 m 5 s);
		// this is the outer bound of a wake nobody waits for.
		wakeTimeout:  8 * time.Minute,
		upgradedWait: 10 * time.Second,
		rowTimeout:   10 * time.Second,
		probeTimeout: 2 * time.Second,
		probeSlots:   ProbeSlots,
	}
}

// Handler is the port door: an http.Handler for the routes
// /fwd/{id}/{port}[/{rest...}] and
// /api/v1/sandboxes/{id}/ports/{port}[/{rest...}]. It reads the sandbox,
// the port and the rest of the path from the request's path, so the rest
// keeps its escaping.
type Handler struct {
	sb      Sandboxes
	conns   nodelink.Conns
	kp      KubePorts
	resolve func(ctx context.Context, bearer string) (auth.Sandbox, error)
	db      *db.DB
	st      *store.Store
	// projectOf names the project of a project token's hash; it is the
	// store's lookup, nil without a database.
	projectOf func(ctx context.Context, hash string) (string, error)
	log       *slog.Logger
	rl        *rateLog
	t         timings

	// base is the parent of every Forward stream and of the wakes nobody
	// waits for; Run's end cancels it.
	base       context.Context
	cancelBase context.CancelFunc

	mu sync.Mutex
	// transports are the pools per (pod, port), each bound to the node and
	// incarnation it dials; retired ones serve their requests in flight
	// and close their connections as these go idle.
	transports map[tkey]*entry
	retired    []*entry
	// caps are the per-sandbox slots of Forward streams; probes those of
	// the checks' streams, apart from caps.
	caps   map[podid.ID]*capSem
	probes map[podid.ID]*capSem
	// waking are the sandboxes a wake nobody waits for runs for.
	waking map[podid.ID]bool
	// upgraded are the upgraded connections; upActive counts their
	// requests until each has released its hold, and upIdle is closed
	// when that count returns to 0. upClosing is set by CloseUpgraded.
	upgraded  map[*upgradedConn]struct{}
	upActive  int
	upIdle    chan struct{}
	upClosing bool
}

// New returns the port door. kp may be nil (no binding ever refuses a
// port); d, when set, tells the door of sandboxes whose pooled connections
// lost their pod (Run) and of projects' tokens, which it refuses with 401
// rather than as unknown.
func New(s Sandboxes, conns nodelink.Conns, kp KubePorts,
	resolve func(ctx context.Context, bearer string) (auth.Sandbox, error), d *db.DB, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "ports")
	h := &Handler{
		sb: s, conns: conns, kp: kp, resolve: resolve, db: d, log: log, rl: newRateLog(log), t: defaultTimings(),
		transports: map[tkey]*entry{},
		caps:       map[podid.ID]*capSem{},
		probes:     map[podid.ID]*capSem{},
		waking:     map[podid.ID]bool{},
		upgraded:   map[*upgradedConn]struct{}{},
	}
	if d != nil {
		h.st = store.New(d)
		h.projectOf = h.st.ResolveProjectToken
	}
	h.base, h.cancelBase = context.WithCancel(context.Background())
	return h
}

// The answers of the forward class that do not depend on the node.
const (
	textBadPath      = "expected /fwd/<sandbox>/<port>/<path>"
	textNoToken      = "forwarding needs a sandbox token"
	textNoSandbox    = "no such sandbox"
	textLookupFailed = "token lookup failed"
	textWakingUp     = "the sandbox is waking up, try again"
)

// reply is a plain-text answer of the forward class.
type reply struct {
	status     int
	msg        string
	retryAfter int // seconds; 0: no Retry-After
}

func (a reply) write(w http.ResponseWriter) {
	if a.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(a.retryAfter))
	}
	http.Error(w, a.msg, a.status)
}

// target is what a request's path names.
type target struct {
	id   string // the sandbox as the path names it
	port int
	// path and rawPath are the rest of the path after the port, with
	// its leading "/" ("/" when there is none): unescaped and as sent.
	path, rawPath string
	// credentials are the request headers that carried the caller's
	// credential, which stop at the door with Proxy-Authorization; nil
	// is Authorization (a sandbox token's).
	credentials []string
}

// splitPath reads the sandbox, the port and the rest from the escaped
// path of either mount. The port is a decimal 1..65535; a sandbox segment
// that is empty or holds ".." names nothing.
func splitPath(r *http.Request) (target, bool) {
	p := r.URL.EscapedPath()
	var parts []string
	var idAt, portAt int
	switch {
	case strings.HasPrefix(p, "/fwd/"):
		parts = strings.SplitN(p[1:], "/", 4)
		idAt, portAt = 1, 2
	case strings.HasPrefix(p, "/api/v1/sandboxes/"):
		parts = strings.SplitN(p[1:], "/", 7)
		if len(parts) > 4 && parts[4] != "ports" {
			return target{}, false
		}
		idAt, portAt = 3, 5
	default:
		return target{}, false
	}
	if len(parts) <= portAt {
		return target{}, false
	}
	id, err := url.PathUnescape(parts[idAt])
	if err != nil || id == "" || strings.Contains(id, "..") {
		return target{}, false
	}
	port, err := strconv.Atoi(parts[portAt])
	if err != nil || port < 1 || port > 65535 {
		return target{}, false
	}
	raw := "/"
	if len(parts) > portAt+1 {
		raw = "/" + parts[portAt+1]
	}
	rest, err := url.PathUnescape(raw)
	if err != nil {
		return target{}, false
	}
	return target{id: id, port: port, path: rest, rawPath: raw}, true
}

// bearer returns the token of "Authorization: Bearer <token>" (the prefix
// exact, the token trimmed), or "". The door takes the token from there
// only.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[len("Bearer "):])
}

// ServeHTTP is the forward class and the forward.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tg, ok := splitPath(r)
	if !ok {
		reply{http.StatusNotFound, textBadPath, 0}.write(w)
		return
	}
	tok := bearer(r)
	if tok == "" {
		reply{http.StatusUnauthorized, textNoToken, 0}.write(w)
		return
	}
	sb, err := h.resolve(r.Context(), tok)
	if errors.Is(err, model.ErrUnknownToken) {
		h.notSandboxToken(r.Context(), tok).write(w)
		return
	}
	if err != nil {
		h.rl.warn("lookup", "forward: token lookup failed", "err", err)
		reply{http.StatusInternalServerError, textLookupFailed, 0}.write(w)
		return
	}
	// The path's sandbox is a claim the token backs; a token of another
	// sandbox learns nothing about this one.
	if id, err := podid.Parse(tg.id); err != nil || id != sb.ID {
		reply{http.StatusNotFound, textNoSandbox, 0}.write(w)
		return
	}
	if rp := h.kubeRefusal(r.Context(), sb.ID, tg.port); rp != nil {
		rp.write(w)
		return
	}
	h.forward(w, r, sb.ID, tg)
}

// kubeRefusal is the answer to a forward to a port of the sandbox's
// Kubernetes bindings, or to a failed lookup of them; nil when the port
// may be forwarded. A Kubernetes listener injects the binding's credential
// for whoever reaches it; the guest refuses it too.
func (h *Handler) kubeRefusal(ctx context.Context, id podid.ID, port int) *reply {
	if h.kp == nil {
		return nil
	}
	kube, err := h.kp.KubePorts(ctx, id)
	if err != nil {
		h.rl.warn("lookup", "forward: binding lookup failed", "sandbox", id, "err", err)
		return &reply{http.StatusInternalServerError, "binding lookup failed", 0}
	}
	if slices.Contains(kube, port) {
		return &reply{http.StatusForbidden, kubeText(port), 0}
	}
	return nil
}

// notSandboxToken answers a bearer that is no sandbox's token. A project's
// token is told what the door takes, 401 as without a token: its holder
// sent the wrong one of its tokens, and the answer names no sandbox, so it
// tells nothing about the one in the path. Any other token gets the answer
// of an absent sandbox.
func (h *Handler) notSandboxToken(ctx context.Context, tok string) reply {
	if h.projectOf == nil {
		return reply{http.StatusNotFound, textNoSandbox, 0}
	}
	_, err := h.projectOf(ctx, store.HashToken(tok))
	switch {
	case err == nil:
		return reply{http.StatusUnauthorized, textNoToken, 0}
	case errors.Is(err, model.ErrUnknownToken):
		return reply{http.StatusNotFound, textNoSandbox, 0}
	}
	h.rl.warn("lookup", "forward: token lookup failed", "err", err)
	return reply{http.StatusInternalServerError, textLookupFailed, 0}
}

func kubeText(port int) string {
	return fmt.Sprintf("port %d is a Kubernetes listener and is never forwarded", port)
}

// forward holds the sandbox awake and proxies the request. When the node
// says that the pod is not running as the row has it, the row is synced
// and the sandbox woken; a GET or HEAD without a body then goes once more,
// anything else is told to try again.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, id podid.ID, tg target) {
	ctx := r.Context()
	repeatable := (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.ContentLength == 0 && len(r.TransferEncoding) == 0
	for attempt := 0; ; attempt++ {
		lease, release, err := h.acquire(ctx, id)
		if err != nil {
			h.acquireFailed(ctx, w, id, err)
			return
		}
		if !h.proxyHeld(w, r, lease, tg, release, attempt == 0 && repeatable) {
			return
		}
		if err := h.detached(ctx, func(dctx context.Context) error { return h.sb.Sync(dctx, id) }); err != nil {
			if ctx.Err() != nil {
				return
			}
			h.rl.warn("sync", "forward: cannot sync a sandbox whose node does not run it", "sandbox", id, "err", err)
		}
	}
}

// acquire brings the sandbox up and takes a forward hold. The engine's
// work runs detached from the request: the request only waits for it,
// and a hold that arrives after the request gave up is released at once.
func (h *Handler) acquire(ctx context.Context, id podid.ID) (model.Lease, func(), error) {
	type result struct {
		lease   model.Lease
		release func()
		err     error
	}
	done := make(chan result, 1)
	go func() {
		actx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		stop := context.AfterFunc(h.base, cancel)
		defer func() {
			stop()
			cancel()
		}()
		lease, release, err := h.sb.Acquire(actx, id, model.HoldForward)
		done <- result{lease, release, err}
	}()
	select {
	case res := <-done:
		return res.lease, res.release, res.err
	case <-ctx.Done():
		go func() {
			if res := <-done; res.release != nil {
				res.release()
			}
		}()
		return model.Lease{}, nil, ctx.Err()
	}
}

// detached runs f on a context that ignores the request's end and waits
// for it while the request lasts.
func (h *Handler) detached(ctx context.Context, f func(ctx context.Context) error) error {
	done := make(chan error, 1)
	go func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.t.wakeTimeout)
		stop := context.AfterFunc(h.base, cancel)
		defer func() {
			stop()
			cancel()
		}()
		done <- f(dctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// acquireFailed answers a sandbox that could not be brought up and held:
// a retry after the time the error asks for (60 s for a hibernation in
// flight), else after 5 s.
func (h *Handler) acquireFailed(ctx context.Context, w http.ResponseWriter, id podid.ID, err error) {
	switch {
	case ctx.Err() != nil:
		// The client is gone; the wake goes on without it.
	case errors.Is(err, model.ErrNotFound):
		reply{http.StatusNotFound, textNoSandbox, 0}.write(w)
	case errors.Is(err, model.ErrNodeUnavailable):
		reply{http.StatusServiceUnavailable, "the sandbox's node is unavailable", 5}.write(w)
	case errors.Is(err, model.ErrExpired):
		reply{http.StatusGone, "the sandbox expired", 0}.write(w)
	default:
		h.rl.warn("acquire", "forward: the sandbox could not be brought up", "sandbox", id, "err", err)
		reply{http.StatusServiceUnavailable, "the sandbox could not be brought up: " + err.Error(), apierr.RetryAfterOf(err, 5)}.write(w)
	}
}

// wake syncs and ensures a sandbox in the background, once at a time per
// sandbox: the answer to a request that is told to try again.
func (h *Handler) wake(id podid.ID) {
	h.mu.Lock()
	if h.waking[id] {
		h.mu.Unlock()
		return
	}
	h.waking[id] = true
	h.mu.Unlock()
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.waking, id)
			h.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(h.base, h.t.wakeTimeout)
		defer cancel()
		if err := h.sb.Sync(ctx, id); err != nil && ctx.Err() == nil {
			h.rl.warn("sync", "forward: cannot sync a sandbox whose node does not run it", "sandbox", id, "err", err)
		}
		if _, err := h.sb.Ensure(ctx, id); err != nil && ctx.Err() == nil {
			h.rl.warn("acquire", "forward: the sandbox could not be brought up", "sandbox", id, "err", err)
		}
	}()
}

// proxyHeld proxies one attempt under the hold release ends, which it
// releases afterwards, recording the activity first. It reports whether
// the node did not run the pod and nothing was answered (the caller
// retries). An upgraded connection is closed here as well: the proxy
// closes it after the piping, but not when it refuses the 101 (a protocol
// the client did not ask for, a client connection it cannot hijack), and
// such a connection would hold its Forward stream and its slot of the
// sandbox's cap until the instance stops.
func (h *Handler) proxyHeld(w http.ResponseWriter, r *http.Request, lease model.Lease, tg target, release func(),
	mayRetry bool) (stale bool) {
	var up *upgradedConn
	defer func() {
		if up != nil {
			_ = up.Close()
		}
		h.sb.Touch(lease.SandboxID)
		release()
		if up != nil {
			h.upgradeEnded()
		}
	}()
	return h.proxy(w, r, lease, tg, mayRetry, &up)
}

// HostHeader names the Host the server in the guest sees, for a caller
// that cannot send it as the Host: FeCode's backend reaches the doors
// through the front door's ingress at the platform's own name, which
// routes by Host, and names the preview host here. It is no credential:
// whoever may call the door may send any Host.
const HostHeader = "X-FeRun-Host"

// maxHost bounds a Host the guest is given: a DNS name and a port.
const maxHost = 261

// GuestHost is the Host the server in the guest sees for r: X-FeRun-Host
// when it holds one host[:port] of letters, digits, '.', '-', '_', ':'
// and brackets, else the Host r came with.
func GuestHost(r *http.Request) string {
	h := r.Header.Get(HostHeader)
	if h == "" || len(h) > maxHost {
		return r.Host
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(".-_:[]", c) >= 0) {
			return r.Host
		}
	}
	return h
}

// pseudoHost names a pod's port in the pools' keys: it is never resolved.
func pseudoHost(id podid.ID, port int) string {
	return id.String() + "--" + strconv.Itoa(port) + ".ferun.invalid"
}

// proxy forwards the request to the pod's port through the pool of the
// lease's node. An upgraded connection is tracked in *up until it ends.
func (h *Handler) proxy(w http.ResponseWriter, r *http.Request, lease model.Lease, tg target, mayRetry bool,
	up **upgradedConn) (stale bool) {
	tr, done := h.transport(lease, tg.port)
	defer done()
	ctx, stop := h.waitForConn(r.Context())
	defer stop()
	r = r.WithContext(ctx)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = pseudoHost(lease.SandboxID, tg.port)
			pr.Out.URL.Path = tg.path
			pr.Out.URL.RawPath = tg.rawPath
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			// The server in the guest sees the host the client named
			// (FeCode's preview host): allowed-hosts checks and hot-reload
			// URLs are built from it. A caller that reaches the door
			// through an ingress that routes by Host names it in
			// X-FeRun-Host instead (GuestHost).
			pr.Out.Host = GuestHost(pr.In)
			pr.Out.Header.Del(HostHeader)
			// The caller's credential stops here: the sandbox's token,
			// or what the route door names.
			creds := tg.credentials
			if creds == nil {
				creds = []string{"Authorization"}
			}
			for _, name := range creds {
				pr.Out.Header.Del(name)
			}
			pr.Out.Header.Del("Proxy-Authorization")
		},
		Transport:     tr,
		FlushInterval: -1, // hot reload, event streams and terminals stream
		ModifyResponse: func(res *http.Response) error {
			if res.StatusCode != http.StatusSwitchingProtocols {
				return nil
			}
			if rwc, ok := res.Body.(io.ReadWriteCloser); ok {
				*up = h.trackUpgrade(rwc, lease)
				res.Body = *up
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			v := classify(err, lease.Node, tg.port)
			if v.stale {
				if mayRetry {
					stale = true
					return
				}
				h.wake(lease.SandboxID)
			}
			if v.log {
				h.rl.warn("forward", "forward failed", "sandbox", lease.SandboxID, "port", tg.port, "node", lease.Node,
					"err", err)
			}
			v.write(w)
		},
		ErrorLog: slog.NewLogLogger(h.log.Handler(), slog.LevelDebug),
	}
	rp.ServeHTTP(w, r)
	return stale
}
