// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
)

// api is the HTTP API (sink.md, Reading).
type api struct {
	store   *Store
	traces  *Traces
	tenants *Tenants
	ingest  <-chan struct{}
	log     *slog.Logger
}

// newAPI returns the HTTP API's handler: /api/v1/logs,
// /api/v1/log-streams, /api/v1/jaeger/api/…, /api/v1/traffic and
// /healthz; ingest is closed once the ingest serves.
func newAPI(store *Store, traces *Traces, tenants *Tenants, ingest <-chan struct{}, log *slog.Logger) http.Handler {
	a := &api{store: store, traces: traces, tenants: tenants, ingest: ingest, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /api/v1/logs", a.tenant(a.logs))
	mux.HandleFunc("GET /api/v1/log-streams", a.tenant(a.streams))
	mux.HandleFunc("DELETE /api/v1/log-streams", a.tenant(a.deleteStreams))
	mux.HandleFunc("GET /api/v1/jaeger/api/services", a.tenant(a.jaegerNames))
	mux.HandleFunc("GET /api/v1/jaeger/api/services/{service}/operations", a.tenant(a.jaegerNames))
	mux.HandleFunc("GET /api/v1/jaeger/api/traces/{traceID}", a.tenant(a.jaegerTrace))
	mux.HandleFunc("GET /api/v1/jaeger/api/traces", a.tenant(a.jaegerFind))
	mux.HandleFunc("GET /api/v1/traffic", a.tenant(a.traffic))
	return mux
}

// healthz answers 200 once the ingest serves and while the database
// answers: the instance's readiness for both Services.
func (a *api) healthz(w http.ResponseWriter, r *http.Request) {
	select {
	case <-a.ingest:
	default:
		http.Error(w, "the ingest waits for its certificate", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.db.Pool.Ping(ctx); err != nil {
		http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// The header a change made with a session's cookie must carry, as
// ferund's own (a page of another origin cannot set it without CORS,
// which the sink never grants).
const (
	csrfHeader = "X-Requested-With"
	csrfValue  = "ferun"
)

// crossOrigin is net/http's cross-origin check: Sec-Fetch-Site, or
// without it Origin against Host. The sink is served under ferund's host,
// so that ferund's pages are of its own origin.
var crossOrigin http.CrossOriginProtection

// credential is the reader's credential: a token (Authorization: Bearer
// …), else a person's session (its cookie, where the sink is served under
// ferund's host), which names its tenant in FeRun-Tenant and whose
// changes need X-Requested-With: ferun and a request of the same origin,
// as ferund's do (ADR 0031).
func credential(r *http.Request) (Credential, error) {
	c := Credential{Tenant: r.Header.Get(auth.TenantHeader)}
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") && len(v) > len("Bearer ") {
		c.Authorization = v
		return c, nil
	}
	ck, err := r.Cookie(auth.SessionCookie)
	if err != nil || ck.Value == "" {
		return c, apierr.New(http.StatusUnauthorized, "a tenant's token (Authorization: Bearer …) or a session is required")
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		if r.Header.Get(csrfHeader) != csrfValue {
			return c, apierr.New(http.StatusForbidden, "missing %s header", csrfHeader)
		}
		if err := crossOrigin.Check(r); err != nil {
			return c, apierr.New(http.StatusForbidden, "cross-origin request refused")
		}
	}
	if c.Tenant == "" {
		return c, apierr.New(http.StatusBadRequest, "a session names its tenant in the %s header", auth.TenantHeader)
	}
	c.Session = ck.Value
	return c, nil
}

// tenant resolves the request's reader to what it may read through
// ferund; a refusal of ferund is passed on as it came, and a reader that
// may read nothing of the tenant is answered as an unknown tenant.
func (a *api) tenant(h func(w http.ResponseWriter, r *http.Request, acc Access)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := credential(r)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		acc, err := a.tenants.Resolve(r.Context(), c)
		var ref *refusal
		if errors.As(err, &ref) {
			if ref.contentType != "" {
				w.Header().Set("Content-Type", ref.contentType)
			}
			w.WriteHeader(ref.status)
			_, _ = w.Write(ref.body)
			return
		}
		if err != nil {
			a.log.Warn("resolve a tenant", "err", err)
			apierr.Write(w, apierr.New(http.StatusBadGateway, "cannot ask ferund for the reader's tenant"))
			return
		}
		if acc.none() {
			apierr.Write(w, apierr.New(http.StatusNotFound, "no such tenant"))
			return
		}
		h(w, r, acc)
	}
}

func (a *api) logs(w http.ResponseWriter, r *http.Request, acc Access) {
	q, err := parseQuery(r.URL.Query(), time.Now())
	if err != nil {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "%s", err.Error()))
		return
	}
	q.Sel.Within = acc.within()
	tenant := acc.Tenant
	if q.Follow {
		a.follow(w, r, tenant, q)
		return
	}
	res, err := a.store.Read(r.Context(), tenant, q)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res.page(q.Forward))
}

func (a *api) streams(w http.ResponseWriter, r *http.Request, acc Access) {
	v := r.URL.Query()
	sel, err := parseSelector(v)
	if err != nil {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "%s", err.Error()))
		return
	}
	sel.Within = acc.within()
	tenant := acc.Tenant
	since := time.Now().Add(-7 * 24 * time.Hour).UnixNano()
	if t := v.Get("since"); t != "" {
		if since, err = parseTime("since", t); err != nil {
			apierr.Write(w, apierr.New(http.StatusBadRequest, "%s", err.Error()))
			return
		}
	}
	rows, err := a.store.Streams(r.Context(), tenant, sel, since)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := struct {
		Streams []StreamJSON `json:"streams"`
	}{Streams: make([]StreamJSON, 0, len(rows))}
	for _, st := range rows {
		out.Streams = append(out.Streams, st.json())
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteStreams needs write on the tenant (FeCode's token, which deletes
// an environment's streams with the environment).
func (a *api) deleteStreams(w http.ResponseWriter, r *http.Request, acc Access) {
	if !acc.Write {
		apierr.Write(w, apierr.RoleRequired("write", "tenant"))
		return
	}
	tenant := acc.Tenant
	v := r.URL.Query()
	env, job := v.Get("environment"), v.Get("job")
	if (env == "") == (job == "") || len(v) != 1 {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "give environment or job, and nothing else"))
		return
	}
	n, err := a.store.DeleteStreams(r.Context(), tenant, env, job)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.log.Info("deleted streams", "tenant", tenant, "environment", env, "job", job, "streams", n)
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	a.log.Error("read", "path", r.URL.Path, "err", err)
	apierr.Write(w, apierr.New(http.StatusServiceUnavailable, "the sink cannot answer now"))
}

// follow answers follow=1 as NDJSON.
func (a *api) follow(w http.ResponseWriter, r *http.Request, tenant string, q Query) {
	// What is followed is checked before the answer starts: an
	// environment as a whole is searched, not followed.
	if _, err := a.store.FollowKey(r.Context(), tenant, q.Sel); err != nil {
		if errors.Is(err, ErrFollowScope) {
			apierr.Write(w, apierr.New(http.StatusBadRequest, "%s", err.Error()))
			return
		}
		a.fail(w, r, err)
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	out := &ndjsonFollow{w: w, rc: rc, enc: json.NewEncoder(w), sent: map[int64]bool{}}
	if err := a.store.Follow(r.Context(), tenant, q, out); err != nil && r.Context().Err() == nil {
		a.log.Warn("follow", "err", err)
	}
}

// ndjsonFollow writes a follow's objects: {"line": …, "stream": …} with
// the stream the first time only, {"skipped": n} when its rate skipped n
// lines, and {}.
type ndjsonFollow struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	enc  *json.Encoder
	sent map[int64]bool
}

type followLine struct {
	Line   LineJSON    `json:"line"`
	Stream *StreamJSON `json:"stream,omitempty"`
}

func (f *ndjsonFollow) Line(l Line, st *streamRow) error {
	out := followLine{Line: lineJSON(l, st.SID)}
	if !f.sent[st.ID] {
		f.sent[st.ID] = true
		sj := st.json()
		out.Stream = &sj
	}
	if err := f.enc.Encode(out); err != nil {
		return err
	}
	return f.rc.Flush()
}

func (f *ndjsonFollow) Skipped(n int) error {
	if err := f.enc.Encode(map[string]int{"skipped": n}); err != nil {
		return err
	}
	return f.rc.Flush()
}

func (f *ndjsonFollow) Heartbeat() error {
	if _, err := f.w.Write([]byte("{}\n")); err != nil {
		return err
	}
	return f.rc.Flush()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
