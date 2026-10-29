// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package api is ferund's front door over HTTP: the REST API, human sign-in
// (OIDC), the mounts of the other doors (MCP, the ports, the watches, the
// bindings) and the management UI. It owns the route table and the auth
// class of every route, stated at the route and nowhere else:
//
//	admin     /api/v1/admin/…            an admin's UI session, or Basic (--admin-users)
//	tenant    /api/v1/sandboxes[/{id}]   a tenant's token: create, list, delete,
//	          /api/v1/tools              pause, resume, tokens, access; the
//	          /api/v1/environments[/…]   environments (ADR 0018) but their door;
//	          /api/v1/domains[/…]        the domains (ADR 0021)
//	sandbox   /api/v1/sandboxes/{id}/…   the sandbox's own token: everything that
//	                                     acts inside the sandbox
//	mcp       /mcp                       the MCP door's own class (internal/mcpsrv)
//	forward   /fwd/…, …/ports/…          the port door's own class (internal/ports)
//	route     /api/v1/environments/{env}/http/…
//	                                     the route door's own class: a route token
//	                                     of the environment (internal/env)
//	public    /healthz, /readyz, /auth/…, the UI
//
// The two token classes stay apart: a tenant's token is not accepted on a
// sandbox route, not even for a sandbox the tenant owns (the forward class
// answers it 401 "forwarding needs a sandbox token"), and a sandbox's
// token is unknown on a tenant route. Whoever names something they may not
// see gets the answer of an absent thing (404), never a 403. Every
// unknown route under /api/ is a 404 {"error": "no such route: METHOD
// PATH"}, never a 405: clients match that text.
//
// The bodies, statuses and texts are those of the sandbox system's API,
// which FeCode's backend and the UI were built against: camelCase fields,
// int64 values as JSON strings, errors as {"error", "code"?}, any error
// that is not an *apierr.Error a 500 with its text.
package api

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/db"
	envs "github.com/gardenlinux/feos/go/internal/env"
	ingresssvc "github.com/gardenlinux/feos/go/internal/ingress/service"
	"github.com/gardenlinux/feos/go/internal/leader"
	"github.com/gardenlinux/feos/go/internal/model"
	"github.com/gardenlinux/feos/go/internal/nodelink"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/tools"
)

// Engine is what the API needs of the sandbox engine (*sandbox.Engine).
type Engine interface {
	Ensure(ctx context.Context, id podid.ID) (model.Lease, error)
	Pause(ctx context.Context, id podid.ID) error
	Resume(ctx context.Context, id podid.ID) error
	Delete(ctx context.Context, owner string, id podid.ID) (store.Sandbox, bool, error)
	DeleteTenant(ctx context.Context, tenant string) (bool, error)
	Touch(id podid.ID)
}

// Watches are the watch handlers (*watch.Service), sandbox class.
type Watches interface {
	Create(http.ResponseWriter, *http.Request)
	Update(http.ResponseWriter, *http.Request)
	Events(http.ResponseWriter, *http.Request)
	Delete(http.ResponseWriter, *http.Request)
}

// Bindings are the Kubernetes and SSH binding handlers
// (*bindings.Service). Off (Enabled false), their routes are not
// registered, and the access route answers the empty object.
type Bindings interface {
	Enabled() bool
	PutKube(http.ResponseWriter, *http.Request)
	GetKube(http.ResponseWriter, *http.Request)
	DeleteKube(http.ResponseWriter, *http.Request)
	PutSSH(http.ResponseWriter, *http.Request)
	GetSSH(http.ResponseWriter, *http.Request)
	DeleteSSH(http.ResponseWriter, *http.Request)
	CheckSSH(http.ResponseWriter, *http.Request)
	KubePreflight(http.ResponseWriter, *http.Request)
	SSHPreflight(http.ResponseWriter, *http.Request)
	Access(http.ResponseWriter, *http.Request)
}

// Environments is the environments service (*env.Service, ADR 0018):
// one method per environment route, tenant class, and the route door, a
// class of its own. nil: no environment route is registered.
type Environments interface {
	CreateEnv(ctx context.Context, tenant string, req envs.CreateEnvRequest) (envs.EnvStatus, bool, error)
	ListEnvs(ctx context.Context, tenant string, labels map[string]string) (envs.EnvList, error)
	GetEnv(ctx context.Context, tenant, envRef string) (envs.EnvStatus, error)
	DeleteEnv(ctx context.Context, tenant, envRef string) error
	PlanSpec(ctx context.Context, tenant string, req envs.PlanSpecRequest) (envs.PlanSpecAnswer, error)
	Plan(ctx context.Context, tenant, envRef string, req envs.PlanRequest) (envs.PlanAnswer, error)
	Apply(ctx context.Context, tenant, envRef string, req envs.ApplyRequest) (envs.ApplyAnswer, error)
	Releases(ctx context.Context, tenant, envRef string, limit int) (envs.ReleaseList, error)
	Release(ctx context.Context, tenant, envRef string, number int) (envs.ReleaseDetail, error)
	Revert(ctx context.Context, tenant, envRef string, number int, req envs.RevertRequest) (envs.ApplyAnswer, error)
	PutObject(ctx context.Context, tenant, envRef, kind, name string, req envs.PutObjectRequest) (envs.ObjectAnswer, error)
	DeleteObject(ctx context.Context, tenant, envRef, kind, name string, req envs.DeleteObjectRequest) (envs.ObjectAnswer, error)
	Clone(ctx context.Context, tenant, envRef string, req envs.CloneRequest) (envs.EnvStatus, error)
	Tags(ctx context.Context, tenant, envRef string) (envs.TagList, error)
	Tag(ctx context.Context, tenant, envRef, tag string) (envs.Tag, error)
	PutTag(ctx context.Context, tenant, envRef, tag string, req envs.PutTagRequest) (envs.Tag, error)
	DeleteTag(ctx context.Context, tenant, envRef, tag string) error
	Secrets(ctx context.Context, tenant, envRef string) (envs.SecretList, error)
	PutSecret(ctx context.Context, tenant, envRef, name string, req envs.PutSecretRequest) (envs.SecretVersion, error)
	DeleteSecret(ctx context.Context, tenant, envRef, name string) error
	Incidents(ctx context.Context, tenant, envRef string, openOnly bool) (envs.IncidentList, error)
	Incident(ctx context.Context, tenant, envRef string, id int64) (envs.Incident, error)
	Logs(ctx context.Context, tenant, envRef, kind, name string, lines, replica int) (envs.LogsAnswer, error)
	RestartService(ctx context.Context, tenant, envRef, name string) error
	RunJob(ctx context.Context, tenant, envRef, name string) error
	CreateRouteToken(ctx context.Context, tenant, envRef string) (envs.RouteToken, error)
	RevokeRouteTokens(ctx context.Context, tenant, envRef string) error
	SetPublic(ctx context.Context, tenant, envRef string, public bool) (envs.PublicAnswer, error)
	ServeRoute(w http.ResponseWriter, r *http.Request)
}

// Ingress is the ingress's identity service (*ingress/service.Service,
// ADR 0021) as the admin API uses it: join tokens of kind ingress, the
// ingress instances, their removal. nil: those answer 503.
type Ingress interface {
	IssueJoinToken(ctx context.Context, createdBy, name string, ttl time.Duration) (uuid.UUID, string, time.Time, error)
	ListIngresses(ctx context.Context) ([]ingresssvc.IngressInfo, error)
	RemoveIngress(ctx context.Context, name string) (bool, error)
}

// Config is the API's part of the instance's configuration.
type Config struct {
	// AdminUsers are the Basic credentials of the admin class (user →
	// password); none: Basic is off.
	AdminUsers map[string]string
	InstanceID string
	// PreparationWaitMs is what GET /api/v1/tools advertises: the longest
	// a tool call waits for its sandbox before the tool starts.
	PreparationWaitMs int64
	// PublicURL is the front door's external URL (no trailing slash): the
	// sign-in's redirect URI is PublicURL/auth/callback, its cookies are
	// Secure when it is https, and the Origin of a request authenticated
	// by a session must be its origin.
	PublicURL string
	// OIDC configures human sign-in; nil: none.
	OIDC *OIDCConfig
	// SessionTTL is the absolute lifetime of a UI session.
	SessionTTL time.Duration

	DefaultImage                   string
	DefaultVCPUs, DefaultMemoryMiB int
	// JoinTokenTTL is a join token's lifetime when the request names none.
	JoinTokenTTL time.Duration

	// The fleet's timings, which the nodes view shows (its "defaults"):
	// the interval nodes report telemetry at (zero:
	// config.TelemetryInterval), how long a node may stay silent before it
	// is down (--node-ttl), how long it is down before its sandboxes count
	// as lost (--lost-grace), and how long a drain waits for running execs
	// (--drain-timeout).
	TelemetryInterval, NodeTTL, LostGrace, DrainTimeout time.Duration

	Log *slog.Logger
}

// OIDCConfig is the OIDC client of human sign-in.
type OIDCConfig struct {
	// Issuer is compared exactly with the discovery document's issuer and
	// every ID token's iss.
	Issuer, ClientID, ClientSecret string
	Scopes                         []string
	// AdminGroup makes a person an admin when the groups claim holds it.
	AdminGroup string
	// HTTPClient talks to the provider; nil: a client with a 10 s timeout.
	HTTPClient *http.Client
}

// Deps are the parts of the instance the API serves.
type Deps struct {
	DB      *db.DB
	Store   *store.Store
	Engine  Engine
	Elector *leader.Elector
	Conns   nodelink.Conns
	Fleet   interface {
		IssueJoinToken(ctx context.Context, createdBy, node string, ttl time.Duration) (uuid.UUID, string, time.Time, error)
	}
	Tools *tools.REST
	Specs func() []tools.Spec
	// MCP, Ports and Watches answer 503 "not available on this instance"
	// when nil; Bindings nil is off.
	MCP      http.Handler
	Ports    http.Handler
	Watches  Watches
	Bindings Bindings
	// Environments serves the environment routes; nil: none are
	// registered.
	Environments Environments
	// Ingress issues join tokens of kind ingress and serves the ingress
	// instances' admin routes; nil: they answer 503.
	Ingress Ingress
	// Domains serves the domain routes; nil: none are registered.
	Domains Domains
	// UI is the built management UI; nil: none.
	UI fs.FS
	// Ready is /readyz (§11): server's check of the database, stopping,
	// and the nodes the instance should be connected to (nodes,
	// node_telemetry, Conns, start time). nil: always ready.
	Ready func(ctx context.Context) error
	// Sealer seals a sandbox's image credentials (ADR 0025); nil: a
	// create or a PUT with credentials answers 503.
	Sealer Sealer
}

// Sealer seals what the engine opens (internal/seal).
type Sealer interface {
	Seal(plaintext, aad []byte) ([]byte, error)
}

// The fixed limits of the API.
const (
	// maxBody bounds a request body the API reads itself.
	maxBody = 32 << 20
	// defaultJoinTokenTTL is a join token's lifetime when neither the
	// request nor the Config names one.
	defaultJoinTokenTTL = time.Hour
	// authCleanupInterval is how often the leader deletes stale sign-ins
	// and expired sessions.
	authCleanupInterval = 10 * time.Minute
)

// server is the API of one instance.
type server struct {
	cfg  Config
	deps Deps
	st   *store.Store
	log  *slog.Logger
	// oidc is the sign-in provider; nil without OIDC.
	oidc *provider
	// secure marks cookies Secure: the public URL is https.
	secure bool
	// origin is the public URL's origin, which a session's requests must
	// come from.
	origin  string
	touches *throttle
	mux     *http.ServeMux
}

// New returns the API's handler: every route of the instance.
func New(cfg Config, deps Deps) http.Handler {
	return newServer(cfg, deps).mux
}

// newServer builds the API and registers its routes.
func newServer(cfg Config, deps Deps) *server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.JoinTokenTTL <= 0 {
		cfg.JoinTokenTTL = defaultJoinTokenTTL
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.TelemetryInterval <= 0 {
		cfg.TelemetryInterval = config.TelemetryInterval
	}
	cfg.PublicURL = strings.TrimSuffix(cfg.PublicURL, "/")
	st := deps.Store
	if st == nil && deps.DB != nil {
		st = store.New(deps.DB)
	}
	s := &server{
		cfg: cfg, deps: deps, st: st,
		log:     cfg.Log.With("component", "api"),
		touches: newThrottle(time.Minute),
		mux:     http.NewServeMux(),
	}
	if u, err := url.Parse(cfg.PublicURL); err == nil && u.Host != "" {
		s.secure = u.Scheme == "https"
		s.origin = strings.ToLower(u.Scheme + "://" + u.Host)
	}
	if cfg.OIDC != nil {
		s.oidc = newProvider(*cfg.OIDC)
	}
	for _, rt := range s.routes() {
		s.mux.Handle(rt.pattern, rt.handler)
	}
	return s
}

// Loops returns the API's leader loop: every 10 minutes, the sign-ins
// begun more than 10 minutes ago and the expired UI sessions are deleted.
func Loops(deps Deps) []leader.Loop {
	st := deps.Store
	if st == nil {
		st = store.New(deps.DB)
	}
	return []leader.Loop{{
		Name:     "api-auth-cleanup",
		Interval: authCleanupInterval,
		Run: func(ctx context.Context) error {
			if _, err := st.DeleteStaleAuthLogins(ctx); err != nil {
				return err
			}
			_, err := st.DeleteExpiredUISessions(ctx)
			return err
		},
	}}
}

// class is the auth class of a route.
type class string

const (
	classPublic  class = "public"
	classAdmin   class = "admin"
	classTenant  class = "tenant"
	classSandbox class = "sandbox"
	classMCP     class = "mcp"
	classForward class = "forward"
	// classRoute is the route door's (ANY /api/v1/environments/{env}/http/
	// {path...}): a route token of the environment, plain-text answers, a
	// tenant's token refused; the environments service checks it.
	classRoute class = "route"
)

// route is one line of the route table.
type route struct {
	pattern string
	class   class
	handler http.Handler
}

// routes is the route table. Every line names its auth class, so that
// the credential a route takes is read from the line that registers it.
func (s *server) routes() []route {
	var rs []route
	add := func(c class, pattern string, h http.Handler) {
		rs = append(rs, route{pattern: pattern, class: c, handler: h})
	}
	admin := func(pattern string, h apiHandler) { add(classAdmin, pattern, s.admin(h)) }
	tenant := func(pattern string, h apiHandler) { add(classTenant, pattern, s.tenant(h)) }
	sandbox := func(pattern string, h apiHandler) { add(classSandbox, pattern, s.sandbox(h)) }
	sandboxHTTP := func(pattern string, h func(http.ResponseWriter, *http.Request)) {
		add(classSandbox, pattern, s.sandbox(serve(h)))
	}
	public := func(pattern string, h http.Handler) { add(classPublic, pattern, h) }

	// Operators: tenants, the node fleet, join tokens, the ingress's
	// instances, the instances and the activity log. Nobody's tenant: the
	// activity log carries full tool params and outputs, and a drain takes
	// capacity from everyone.
	admin("GET /api/v1/admin/tenants", s.listTenants)
	admin("POST /api/v1/admin/tenants", s.createTenant)
	admin("DELETE /api/v1/admin/tenants/{tenant}", s.deleteTenant)
	admin("GET /api/v1/admin/tenants/{tenant}/tokens", s.listTenantTokens)
	admin("POST /api/v1/admin/tenants/{tenant}/tokens", s.createTenantToken)
	admin("DELETE /api/v1/admin/tenants/{tenant}/tokens/{hash}", s.revokeTenantToken)
	admin("GET /api/v1/admin/nodes", s.listNodes)
	admin("PATCH /api/v1/admin/nodes/{node}", s.patchNode)
	admin("DELETE /api/v1/admin/nodes/{node}", s.removeNode)
	admin("POST /api/v1/admin/nodes/{node}/drain", s.drainNode)
	admin("POST /api/v1/admin/nodes/{node}/undrain", s.undrainNode)
	admin("GET /api/v1/admin/nodes/{node}/certs", s.listNodeCerts)
	admin("GET /api/v1/admin/nodes/{node}/activity", s.nodeActivity)
	admin("GET /api/v1/admin/sandboxes/{id}/activity", s.sandboxActivity)
	admin("POST /api/v1/admin/join-tokens", s.createJoinToken)
	admin("GET /api/v1/admin/join-tokens", s.listJoinTokens)
	admin("DELETE /api/v1/admin/join-tokens/{id}", s.revokeJoinToken)
	admin("GET /api/v1/admin/ingresses", s.listIngresses)
	admin("DELETE /api/v1/admin/ingresses/{name}", s.removeIngress)
	admin("GET /api/v1/admin/instances", s.listInstances)

	// Tenants: the sandbox as a thing one owns, and the tokens that reach
	// into it. A foreign sandbox is indistinguishable from an absent one.
	tenant("GET /api/v1/tools", s.listTools)
	tenant("POST /api/v1/sandboxes", s.createSandbox)
	tenant("GET /api/v1/sandboxes", s.listSandboxes)
	tenant("GET /api/v1/sandboxes/{id}", s.getSandbox)
	tenant("DELETE /api/v1/sandboxes/{id}", s.deleteSandbox)
	tenant("POST /api/v1/sandboxes/{id}/pause", s.pauseSandbox)
	tenant("POST /api/v1/sandboxes/{id}/resume", s.resumeSandbox)
	tenant("PUT /api/v1/sandboxes/{id}/image-credentials", s.putImageCredentials)
	tenant("DELETE /api/v1/sandboxes/{id}/image-credentials", s.deleteImageCredentials)
	tenant("GET /api/v1/sandboxes/{id}/tokens", s.listSandboxTokens)
	tenant("POST /api/v1/sandboxes/{id}/tokens", s.createSandboxToken)
	tenant("DELETE /api/v1/sandboxes/{id}/tokens/{hash}", s.revokeSandboxToken)
	tenant("GET /api/v1/sandboxes/{id}/access", s.access)

	// The sandbox's own token: the tool baseline over REST. Each route
	// makes the same tools.Runner call that MCP makes, with the call's id
	// from the Idempotency-Key header.
	t := s.deps.Tools
	sandboxHTTP("POST /api/v1/sandboxes/{id}/exec", toolRoute(t, (*tools.REST).Exec))
	sandboxHTTP("GET /api/v1/sandboxes/{id}/files/{path...}", toolRoute(t, (*tools.REST).ReadFile))
	sandboxHTTP("PUT /api/v1/sandboxes/{id}/files/{path...}", toolRoute(t, (*tools.REST).WriteFile))
	sandboxHTTP("PATCH /api/v1/sandboxes/{id}/files/{path...}", toolRoute(t, (*tools.REST).EditFile))
	sandboxHTTP("GET /api/v1/sandboxes/{id}/dirs/{path...}", toolRoute(t, (*tools.REST).Dirs))
	sandboxHTTP("GET /api/v1/sandboxes/{id}/search", toolRoute(t, (*tools.REST).Search))
	sandboxHTTP("POST /api/v1/sandboxes/{id}/fetch", toolRoute(t, (*tools.REST).Fetch))
	sandboxHTTP("GET /api/v1/sandboxes/{id}/processes", toolRoute(t, (*tools.REST).ListProcesses))
	sandboxHTTP("POST /api/v1/sandboxes/{id}/processes", toolRoute(t, (*tools.REST).StartProcess))
	sandboxHTTP("GET /api/v1/sandboxes/{id}/processes/{pid}/logs", toolRoute(t, (*tools.REST).ProcessLogs))
	sandboxHTTP("DELETE /api/v1/sandboxes/{id}/processes/{pid}", toolRoute(t, (*tools.REST).StopProcess))
	// The record of a call with an id, and its cancel (ADR 0023): neither
	// runs anything in the sandbox.
	sandboxHTTP("GET /api/v1/sandboxes/{id}/calls/{key}", toolRoute(t, (*tools.REST).Call))
	sandboxHTTP("DELETE /api/v1/sandboxes/{id}/calls/{key}", toolRoute(t, (*tools.REST).CancelCall))

	// Watches, same class; none of it wakes or holds the sandbox.
	w := s.deps.Watches
	sandbox("POST /api/v1/sandboxes/{id}/watches", watchRoute(w, Watches.Create))
	sandbox("PUT /api/v1/sandboxes/{id}/watches/{wid}", watchRoute(w, Watches.Update))
	sandbox("GET /api/v1/sandboxes/{id}/watches/{wid}/events", watchRoute(w, Watches.Events))
	sandbox("DELETE /api/v1/sandboxes/{id}/watches/{wid}", watchRoute(w, Watches.Delete))

	// The Kubernetes and SSH bindings, only while they are on: off, their
	// routes fall to the /api/ catch-all, which a client reads as "this
	// platform has no such surface". The sets are the sandbox's, the
	// preflights the tenant's (they need no sandbox).
	if b := s.deps.Bindings; b != nil && b.Enabled() {
		sandboxHTTP("PUT /api/v1/sandboxes/{id}/kube", b.PutKube)
		sandboxHTTP("GET /api/v1/sandboxes/{id}/kube", b.GetKube)
		sandboxHTTP("DELETE /api/v1/sandboxes/{id}/kube", b.DeleteKube)
		sandboxHTTP("PUT /api/v1/sandboxes/{id}/ssh", b.PutSSH)
		sandboxHTTP("GET /api/v1/sandboxes/{id}/ssh", b.GetSSH)
		sandboxHTTP("DELETE /api/v1/sandboxes/{id}/ssh", b.DeleteSSH)
		sandboxHTTP("POST /api/v1/sandboxes/{id}/ssh/{name}/check", b.CheckSSH)
		tenant("POST /api/v1/kube/preflight", serve(b.KubePreflight))
		tenant("POST /api/v1/ssh/preflight", serve(b.SSHPreflight))
	}

	// The environments (ADR 0018): the tenant's, and their route door.
	s.envRoutes(add)

	// The domains (ADR 0021): the tenant's claims on names, and their
	// bindings to its environments.
	s.domainRoutes(tenant)

	// The doors with classes of their own: their answers are plain text
	// (forward) or the MCP door's, not this API's JSON.
	add(classMCP, "/mcp", orUnavailable(s.deps.MCP))
	ports := orUnavailable(s.deps.Ports)
	add(classForward, "/fwd/{id}/{port}/{rest...}", ports)
	add(classForward, "/fwd/{id}/{port}", ports)
	add(classForward, "/api/v1/sandboxes/{id}/ports/{port}/{rest...}", ports)
	add(classForward, "/api/v1/sandboxes/{id}/ports/{port}", ports)
	add(classForward, "/fwd/", http.HandlerFunc(badForwardPath))

	// Anything else under /api/ is a 404 in JSON, not the UI's index.html:
	// a client that mistypes a route must see it.
	public("/api/", s.wrap(noRoute))

	// Health, sign-in and the UI.
	public("GET /healthz", http.HandlerFunc(healthz))
	public("GET /readyz", http.HandlerFunc(s.readyz))
	s.signInRoutes(public)
	public("/auth/", s.wrap(noRoute))
	public("/", s.ui())
	return rs
}

// toolRoute adapts a tools.REST handler; without the tools the route
// answers 503.
func toolRoute(t *tools.REST, h func(*tools.REST, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	if t == nil {
		return func(w http.ResponseWriter, _ *http.Request) { writeError(w, errUnavailable()) }
	}
	return func(w http.ResponseWriter, r *http.Request) { h(t, w, r) }
}

// watchRoute adapts a watch handler; without the watch service (no
// streams on this instance) the route answers 503.
func watchRoute(ws Watches, h func(Watches, http.ResponseWriter, *http.Request)) apiHandler {
	return func(w http.ResponseWriter, r *http.Request) error {
		if ws == nil {
			return errUnavailable()
		}
		h(ws, w, r)
		return nil
	}
}

// orUnavailable is h, or a plain-text 503 "not available on this
// instance" when nil.
func orUnavailable(h http.Handler) http.Handler {
	if h != nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, unavailableText, http.StatusServiceUnavailable)
	})
}

// badForwardPath answers a path under /fwd/ that names no sandbox and
// port.
func badForwardPath(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "expected /fwd/<sandbox>/<port>/<path>", http.StatusNotFound)
}

// noRoute is the catch-all of /api/ and /auth/.
func noRoute(_ http.ResponseWriter, r *http.Request) error {
	return errNoRoute(r)
}
