// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gardenlinux/feos/go/internal/apierr"
	envs "github.com/gardenlinux/feos/go/internal/env"
)

// The environments' routes (ADR 0018, the contract's §5): tenant class,
// each a thin adapter of a request to one method of Deps.Environments and
// of its answer (or error, envs.WriteError: 422 with findings, 409 with
// the current deployment) back. The route door, a class of its own, is the
// service's handler as it is.

// envRoutes registers the environments' routes on add; none without the
// service.
//
// Each states its role and scope (ADR 0031): what reaches beyond one
// environment (the list, a create, a clone, a spec without an
// environment) the tenant's grant, the rest the environment's, where the
// higher of the tenant's and the environment's grant counts. A deletion is
// the environment's admin's or the tenant's writer's.
func (s *server) envRoutes(tenant func(n need, pattern string, h apiHandler),
	add func(c class, pattern string, h http.Handler)) {
	e := s.deps.Environments
	if e == nil {
		return
	}
	env := func(n need, pattern string, h apiHandler) { tenant(n, pattern, s.envHandler(h)) }
	env(onTenant(write), "POST /api/v1/environments", s.createEnv)
	env(member, "GET /api/v1/environments", s.listEnvs)
	env(onTenant(read), "POST /api/v1/environments/plan", s.planSpec)
	env(onEnv(read), "GET /api/v1/environments/{env}", s.getEnv)
	env(need{role: admin, scope: scopeEnv, orTenant: write}, "DELETE /api/v1/environments/{env}", s.deleteEnv)
	env(onEnv(write), "POST /api/v1/environments/{env}/plan", s.planEnv)
	env(onEnv(write), "POST /api/v1/environments/{env}/deployments", s.applyDeployment)
	env(onEnv(read), "GET /api/v1/environments/{env}/deployments", s.listDeployments)
	env(onEnv(read), "GET /api/v1/environments/{env}/deployments/{n}", s.getDeployment)
	env(onEnv(write), "POST /api/v1/environments/{env}/deployments/{n}/revert", s.revertDeployment)
	env(onEnv(write), "PUT /api/v1/environments/{env}/objects/{kind}/{name}", s.putObject)
	env(onEnv(write), "DELETE /api/v1/environments/{env}/objects/{kind}/{name}", s.deleteObject)
	env(onEnv(read), "GET /api/v1/environments/{env}/objects/{kind}/{name}/logs", s.objectLogs)
	env(onEnv(read), "GET /api/v1/environments/{env}/tags", s.listTags)
	env(onEnv(read), "GET /api/v1/environments/{env}/tags/{tag}", s.getTag)
	env(onEnv(write), "PUT /api/v1/environments/{env}/tags/{tag}", s.putTag)
	env(onEnv(write), "DELETE /api/v1/environments/{env}/tags/{tag}", s.deleteTag)
	env(onTenant(write), "POST /api/v1/environments/{env}/clone", s.cloneEnv)
	env(onEnv(read), "GET /api/v1/environments/{env}/secrets", s.listSecrets)
	env(onEnv(write), "PUT /api/v1/environments/{env}/secrets/{name}", s.putSecret)
	env(onEnv(write), "DELETE /api/v1/environments/{env}/secrets/{name}", s.deleteSecret)
	env(onEnv(read), "GET /api/v1/environments/{env}/incidents", s.listIncidents)
	env(onEnv(read), "GET /api/v1/environments/{env}/incidents/{id}", s.getIncident)
	env(onEnv(read), "GET /api/v1/environments/{env}/access-logs", s.accessLogs)
	env(onEnv(read), "GET /api/v1/environments/{env}/traffic", s.traffic)
	env(onEnv(write), "POST /api/v1/environments/{env}/services/{name}/restart", s.restartService)
	env(onEnv(write), "POST /api/v1/environments/{env}/jobs/{name}/run", s.runJob)
	env(onEnv(write), "POST /api/v1/environments/{env}/bindings/{name}/check", s.checkBinding)
	env(onEnv(admin), "POST /api/v1/environments/{env}/route-tokens", s.createRouteToken)
	env(onEnv(admin), "DELETE /api/v1/environments/{env}/route-tokens", s.revokeRouteTokens)
	env(onEnv(admin), "PUT /api/v1/environments/{env}/public", s.setEnvPublic)
	add(classRoute, "/api/v1/environments/{env}/http/{path...}", http.HandlerFunc(e.ServeRoute))
}

// envHandler answers an environment route's error as the environments
// do (envs.WriteError), logging what is not the client's fault.
func (s *server) envHandler(h apiHandler) apiHandler {
	return func(w http.ResponseWriter, r *http.Request) error {
		err := h(w, r)
		if err == nil {
			return nil
		}
		var ae *apierr.Error
		var spec *envs.InvalidSpecError
		var base *envs.BaseMismatchError
		if !errors.As(err, &spec) && !errors.As(err, &base) && (!errors.As(err, &ae) || ae.Status >= 500) {
			s.log.Warn("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		}
		envs.WriteError(w, err)
		return nil
	}
}

// noContent answers status without a body.
func noContent(w http.ResponseWriter, status int) error {
	w.WriteHeader(status)
	return nil
}

// pathNumber is a path value that is a deployment number or an incident ID.
func pathNumber(r *http.Request, name, what string) (int64, error) {
	s := r.PathValue(name)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, apierr.New(http.StatusBadRequest, "%s %q is not a number", what, s)
	}
	return n, nil
}

// deploymentNumber is the path's {n}.
func deploymentNumber(r *http.Request) (int, error) {
	n, err := pathNumber(r, "n", "deployment")
	if err != nil {
		return 0, err
	}
	if n > 1<<31-1 {
		return 0, apierr.New(http.StatusNotFound, "no such deployment")
	}
	return int(n), nil
}

func (s *server) createEnv(w http.ResponseWriter, r *http.Request) error {
	var req envs.CreateEnvRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	st, created, err := s.deps.Environments.CreateEnv(r.Context(), tenantOf(r), req)
	if err != nil {
		return err
	}
	if created {
		return writeJSON(w, http.StatusCreated, st)
	}
	return writeJSON(w, http.StatusOK, st)
}

// listEnvs is GET /api/v1/environments?label=k=v…: every label must
// match.
func (s *server) listEnvs(w http.ResponseWriter, r *http.Request) error {
	labels := map[string]string{}
	for _, l := range r.URL.Query()["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return apierr.New(http.StatusBadRequest, "label must be key=value, got %q", l)
		}
		labels[k] = v
	}
	list, err := s.deps.Environments.ListEnvs(r.Context(), tenantOf(r), labels)
	if err != nil {
		return err
	}
	// The list holds what the principal may read (ADR 0031).
	readable := list.Environments[:0]
	for _, e := range list.Environments {
		if mayReadEnv(r, e.ID) {
			readable = append(readable, e)
		}
	}
	list.Environments = readable
	return writeJSON(w, http.StatusOK, list)
}

func (s *server) planSpec(w http.ResponseWriter, r *http.Request) error {
	var req envs.PlanSpecRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	a, err := s.deps.Environments.PlanSpec(r.Context(), tenantOf(r), req)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, a)
}

func (s *server) getEnv(w http.ResponseWriter, r *http.Request) error {
	st, err := s.deps.Environments.GetEnv(r.Context(), tenantOf(r), r.PathValue("env"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *server) deleteEnv(w http.ResponseWriter, r *http.Request) error {
	if err := s.deps.Environments.DeleteEnv(r.Context(), tenantOf(r), r.PathValue("env")); err != nil {
		return err
	}
	return noContent(w, http.StatusAccepted)
}

func (s *server) planEnv(w http.ResponseWriter, r *http.Request) error {
	var req envs.PlanRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	a, err := s.deps.Environments.Plan(r.Context(), tenantOf(r), r.PathValue("env"), req)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, a)
}

func (s *server) applyDeployment(w http.ResponseWriter, r *http.Request) error {
	var req envs.ApplyRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	a, err := s.deps.Environments.Apply(r.Context(), tenantOf(r), r.PathValue("env"), req)
	if err != nil {
		return err
	}
	return writeJSON(w, a.StatusCode(), a)
}

func (s *server) listDeployments(w http.ResponseWriter, r *http.Request) error {
	limit, err := queryInt(r, "limit")
	if err != nil {
		return err
	}
	list, err := s.deps.Environments.Deployments(r.Context(), tenantOf(r), r.PathValue("env"), limit)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *server) getDeployment(w http.ResponseWriter, r *http.Request) error {
	n, err := deploymentNumber(r)
	if err != nil {
		return err
	}
	d, err := s.deps.Environments.Deployment(r.Context(), tenantOf(r), r.PathValue("env"), n)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, d)
}

func (s *server) revertDeployment(w http.ResponseWriter, r *http.Request) error {
	n, err := deploymentNumber(r)
	if err != nil {
		return err
	}
	var req envs.RevertRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	a, err := s.deps.Environments.Revert(r.Context(), tenantOf(r), r.PathValue("env"), n, req)
	if err != nil {
		return err
	}
	return writeJSON(w, a.StatusCode(), a)
}

func (s *server) putObject(w http.ResponseWriter, r *http.Request) error {
	var req envs.PutObjectRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	a, err := s.deps.Environments.PutObject(r.Context(), tenantOf(r), r.PathValue("env"), r.PathValue("kind"),
		r.PathValue("name"), req)
	if err != nil {
		return err
	}
	return writeJSON(w, a.StatusCode(), a)
}

// deleteObject is DELETE …/objects/{kind}/{name}?base=&dryRun=1[&author=
// &message=].
func (s *server) deleteObject(w http.ResponseWriter, r *http.Request) error {
	base, err := queryInt(r, "base")
	if err != nil {
		return err
	}
	dryRun, err := queryBool(r, "dryRun")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	req := envs.DeleteObjectRequest{Base: base, DryRun: dryRun, Author: q.Get("author"), Message: q.Get("message")}
	a, err := s.deps.Environments.DeleteObject(r.Context(), tenantOf(r), r.PathValue("env"), r.PathValue("kind"),
		r.PathValue("name"), req)
	if err != nil {
		return err
	}
	return writeJSON(w, a.StatusCode(), a)
}

// objectLogs is GET …/objects/{kind}/{name}/logs?lines=200&replica=0.
func (s *server) objectLogs(w http.ResponseWriter, r *http.Request) error {
	lines, err := queryInt(r, "lines")
	if err != nil {
		return err
	}
	replica, err := queryInt(r, "replica")
	if err != nil {
		return err
	}
	a, err := s.deps.Environments.Logs(r.Context(), tenantOf(r), r.PathValue("env"), r.PathValue("kind"),
		r.PathValue("name"), lines, replica)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, a)
}

func (s *server) listTags(w http.ResponseWriter, r *http.Request) error {
	list, err := s.deps.Environments.Tags(r.Context(), tenantOf(r), r.PathValue("env"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *server) getTag(w http.ResponseWriter, r *http.Request) error {
	t, err := s.deps.Environments.Tag(r.Context(), tenantOf(r), r.PathValue("env"), r.PathValue("tag"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, t)
}

func (s *server) putTag(w http.ResponseWriter, r *http.Request) error {
	var req envs.PutTagRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	t, err := s.deps.Environments.PutTag(r.Context(), tenantOf(r), r.PathValue("env"), r.PathValue("tag"), req)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, t)
}

func (s *server) deleteTag(w http.ResponseWriter, r *http.Request) error {
	if err := s.deps.Environments.DeleteTag(r.Context(), tenantOf(r), r.PathValue("env"), r.PathValue("tag")); err != nil {
		return err
	}
	return noContent(w, http.StatusNoContent)
}

func (s *server) cloneEnv(w http.ResponseWriter, r *http.Request) error {
	var req envs.CloneRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	st, err := s.deps.Environments.Clone(r.Context(), tenantOf(r), r.PathValue("env"), req)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, st)
}

func (s *server) listSecrets(w http.ResponseWriter, r *http.Request) error {
	list, err := s.deps.Environments.Secrets(r.Context(), tenantOf(r), r.PathValue("env"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *server) putSecret(w http.ResponseWriter, r *http.Request) error {
	var req envs.PutSecretRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	v, err := s.deps.Environments.PutSecret(r.Context(), tenantOf(r), r.PathValue("env"), r.PathValue("name"), req)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, v)
}

func (s *server) deleteSecret(w http.ResponseWriter, r *http.Request) error {
	if err := s.deps.Environments.DeleteSecret(r.Context(), tenantOf(r), r.PathValue("env"), r.PathValue("name")); err != nil {
		return err
	}
	return noContent(w, http.StatusNoContent)
}

// listIncidents is GET …/incidents[?open=1].
func (s *server) listIncidents(w http.ResponseWriter, r *http.Request) error {
	open, err := queryBool(r, "open")
	if err != nil {
		return err
	}
	list, err := s.deps.Environments.Incidents(r.Context(), tenantOf(r), r.PathValue("env"), open)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *server) getIncident(w http.ResponseWriter, r *http.Request) error {
	id, err := pathNumber(r, "id", "incident")
	if err != nil {
		return err
	}
	inc, err := s.deps.Environments.Incident(r.Context(), tenantOf(r), r.PathValue("env"), id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, inc)
}

// accessLogs is GET …/access-logs[?limit=&beforeTsNs=&traceId=]: a page
// of the ingress's access log of the environment (ADR 0021).
func (s *server) accessLogs(w http.ResponseWriter, r *http.Request) error {
	limit, err := queryInt(r, "limit")
	if err != nil {
		return err
	}
	before, err := queryInt64(r, "beforeTsNs")
	if err != nil {
		return err
	}
	trace := r.URL.Query().Get("traceId")
	if trace != "" && !isTraceID(trace) {
		return apierr.New(http.StatusBadRequest, "traceId must be 32 lower case hex digits, got %q", trace)
	}
	list, err := s.deps.Environments.AccessLogs(r.Context(), tenantOf(r), r.PathValue("env"),
		envs.AccessLogQuery{Limit: limit, BeforeTsNs: before, TraceID: trace})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

// traffic is GET …/traffic[?window=1h|24h]: the requests, errors and
// latencies of the environment's routed services that the ingress
// answered in the window, per bucket (FeCode ADR 0009 §4).
func (s *server) traffic(w http.ResponseWriter, r *http.Request) error {
	window, err := envs.ParseTrafficWindow(r.URL.Query().Get("window"))
	if err != nil {
		return err
	}
	t, err := s.deps.Environments.Traffic(r.Context(), tenantOf(r), r.PathValue("env"), window)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, t)
}

// isTraceID reports whether s is a trace ID of W3C Trace Context: 32
// lower case hex digits.
func isTraceID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *server) restartService(w http.ResponseWriter, r *http.Request) error {
	if err := s.deps.Environments.RestartService(r.Context(), tenantOf(r), r.PathValue("env"), r.PathValue("name")); err != nil {
		return err
	}
	return noContent(w, http.StatusAccepted)
}

func (s *server) runJob(w http.ResponseWriter, r *http.Request) error {
	if err := s.deps.Environments.RunJob(r.Context(), tenantOf(r), r.PathValue("env"), r.PathValue("name")); err != nil {
		return err
	}
	return noContent(w, http.StatusAccepted)
}

// checkBinding is POST …/bindings/{name}/check {"probe"?}: the preflight of
// the environment's binding with its secrets' latest versions, 200 with
// the result, ok or not.
func (s *server) checkBinding(w http.ResponseWriter, r *http.Request) error {
	var req envs.BindingCheckRequest
	if _, err := decodeJSON(r, &req); err != nil {
		return err
	}
	res, err := s.deps.Environments.CheckBinding(r.Context(), tenantOf(r), r.PathValue("env"), r.PathValue("name"), req)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, res)
}

func (s *server) createRouteToken(w http.ResponseWriter, r *http.Request) error {
	t, err := s.deps.Environments.CreateRouteToken(r.Context(), tenantOf(r), r.PathValue("env"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, t)
}

func (s *server) revokeRouteTokens(w http.ResponseWriter, r *http.Request) error {
	if err := s.deps.Environments.RevokeRouteTokens(r.Context(), tenantOf(r), r.PathValue("env")); err != nil {
		return err
	}
	return noContent(w, http.StatusNoContent)
}

// setEnvPublic is PUT …/public {"public": bool, "hostLabel"?: string}:
// public is required (absent or null is a 400), so that an empty body
// changes nothing by accident.
func (s *server) setEnvPublic(w http.ResponseWriter, r *http.Request) error {
	var req envs.PublicRequest
	sent, err := decodeJSON(r, &req)
	if err != nil {
		return err
	}
	if raw, ok := sent["public"]; !ok || string(raw) == "null" {
		return apierr.New(http.StatusBadRequest, "public is required: {\"public\": true} or {\"public\": false}")
	}
	a, err := s.deps.Environments.SetPublic(r.Context(), tenantOf(r), r.PathValue("env"), req.Public, req.HostLabel)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, a)
}
