// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/store"
)

// bindingUsageWindows are …/bindings/usage's windows: how far back from
// now.
var bindingUsageWindows = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour}

// ParseBindingUsageWindow reads …/bindings/usage's window: 24h (also when
// empty) or 1h; another is a 400.
func ParseBindingUsageWindow(s string) (string, error) {
	if s == "" {
		return "24h", nil
	}
	if _, ok := bindingUsageWindows[s]; !ok {
		return "", apierr.New(http.StatusBadRequest, "window must be 1h or 24h, got %q", s)
	}
	return s, nil
}

// BindingUsage is GET /api/v1/environments/{env}/bindings/usage: what the
// environment's pods (its services' and jobs' and its sandboxes') sent
// through each HTTP binding in the window that ends now. Unavailable
// says, when the log store could not be asked, why the numbers are
// zeros; Truncated that the store left some requests uncounted.
type BindingUsage struct {
	Window      string       `json:"window"`
	Unavailable string       `json:"unavailable,omitempty"`
	Truncated   bool         `json:"truncated"`
	Bindings    []BindingUse `json:"bindings"`
}

// BindingUse is a binding's requests in the window: every one of them,
// those the upstream refused the credential of (401, 403), those that
// failed (429 or 5xx, or the upstream not reached: unreachable, tls),
// those that had no OAuth token (credential_unavailable) and those
// feos-authn refused itself (blocked: denied_path, rate_limited,
// unknown_binding, misdirected, …); the newest's start ("" for none),
// the newest problem (null for none), and the workloads that sent them,
// the most requests first.
type BindingUse struct {
	Name        string          `json:"name"`
	Requests    int64           `json:"requests"`
	Refused     int64           `json:"refused"`
	Failing     int64           `json:"failing"`
	TokenFailed int64           `json:"tokenFailed"`
	Blocked     int64           `json:"blocked"`
	LastAt      string          `json:"lastAt"`
	LastProblem *BindingProblem `json:"lastProblem"`
	Users       []BindingUser   `json:"users"`
}

// BindingProblem is a binding's request that was a problem: its start,
// the status the pod was answered (0: none), feos-authn's code and
// message of a refusal of its own ("" for the upstream's answer), and
// its trace.
type BindingProblem struct {
	At      string `json:"at"`
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"traceId"`
}

// BindingUser is a workload's requests through a binding: a service or a
// job by its name, a sandbox by its ID.
type BindingUser struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Requests int64  `json:"requests"`
}

// sinkEgress is the sink's GET /api/v1/egress answer.
type sinkEgress struct {
	Bindings []struct {
		Binding     string `json:"binding"`
		Requests    int64  `json:"requests"`
		Refused     int64  `json:"refused"`
		Failing     int64  `json:"failing"`
		TokenFailed int64  `json:"tokenFailed"`
		Blocked     int64  `json:"blocked"`
		LastNs      int64  `json:"lastNs,string"`
		LastProblem *struct {
			TsNs    int64  `json:"tsNs,string"`
			Status  int    `json:"status"`
			Code    string `json:"code"`
			Message string `json:"message"`
			TraceID string `json:"traceId"`
		} `json:"lastProblem"`
		Users []struct {
			Kind     string `json:"kind"`
			Object   string `json:"object"`
			Requests int64  `json:"requests"`
		} `json:"users"`
	} `json:"bindings"`
	Truncated bool `json:"truncated"`
}

// BindingUsage is GET /api/v1/environments/{env}/bindings/usage?window=:
// every binding of the environment's latest deployment, and every other
// one its pods sent requests through (a sandbox's own), by name, counted
// by the sink from the pods' egress spans (traces.md, Reading: the
// bindings' requests) with the caller's token or session, as …/traffic.
// Without --sink-url or a credential to pass on, or when the sink fails,
// the bindings are answered with zeros and Unavailable says why: the
// numbers are an addition to the bindings, not their state.
func (s *Service) BindingUsage(ctx context.Context, tenant, env, window string) (BindingUsage, error) {
	length, ok := bindingUsageWindows[window]
	if !ok {
		return BindingUsage{}, apierr.New(http.StatusBadRequest, "window must be 1h or 24h, got %q", window)
	}
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return BindingUsage{}, err
	}
	byName := map[string]*BindingUse{}
	use := func(name string) *BindingUse {
		if byName[name] == nil {
			byName[name] = &BindingUse{Name: name, Users: []BindingUser{}}
		}
		return byName[name]
	}
	if e.Deployment > 0 {
		sp, err := s.deploymentSpec(ctx, e, e.Deployment)
		if err != nil {
			return BindingUsage{}, err
		}
		for name := range sp.Bindings {
			use(name)
		}
	}
	out := BindingUsage{Window: window, Bindings: []BindingUse{}}
	t, _ := auth.TenantFrom(ctx)
	switch {
	case s.d.SinkURL == "":
		out.Unavailable = "the requests are counted in the log store, and this ferund has none (--sink-url)"
	case !t.Passable():
		out.Unavailable = "the requests are read from the log store with the caller's token or session, and the request has neither"
	default:
		until := time.Now()
		got, err := s.readSinkEgress(ctx, t, e.ID.String(), until.Add(-length), until)
		if errors.Is(err, errSinkTooOld) {
			s.log.Debug("the sink has no egress route", "env", e.ID)
			out.Unavailable = "the sink is older than this ferund"
			break
		}
		if err != nil {
			s.log.Warn("cannot read the bindings' requests from the sink", "env", e.ID, "err", err)
			out.Unavailable = "the log store does not answer; try again shortly"
			break
		}
		out.Truncated = got.Truncated
		for _, g := range got.Bindings {
			b := use(g.Binding)
			b.Requests, b.Refused, b.Failing, b.TokenFailed, b.Blocked = g.Requests, g.Refused, g.Failing, g.TokenFailed, g.Blocked
			if g.LastNs > 0 {
				b.LastAt = rfc3339(time.Unix(0, g.LastNs))
			}
			if p := g.LastProblem; p != nil {
				b.LastProblem = &BindingProblem{At: rfc3339(time.Unix(0, p.TsNs)), Status: p.Status, Code: p.Code,
					Message: p.Message, TraceID: p.TraceID}
			}
			for _, u := range g.Users {
				b.Users = append(b.Users, BindingUser{Kind: u.Kind, Name: u.Object, Requests: u.Requests})
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		out.Bindings = append(out.Bindings, *byName[name])
	}
	return out, nil
}

// errSinkTooOld is readSinkEgress's error for a sink without
// /api/v1/egress: a release before it, beside this ferund in a rolling
// update or one not rolled yet.
var errSinkTooOld = errors.New("the sink has no /api/v1/egress")

// readSinkEgress asks the sink for the requests of the environment envID's
// pods through their bindings in [since, until), as t. A 404 that is not
// one of the sink's own answers (JSON: no such environment, no such
// tenant) is its mux's for a route it lacks: errSinkTooOld.
func (s *Service) readSinkEgress(ctx context.Context, t auth.Tenant, envID string, since, until time.Time) (sinkEgress,
	error) {
	v := url.Values{"environment": {envID}, "since": {strconv.FormatInt(since.UnixNano(), 10)},
		"until": {strconv.FormatInt(until.UnixNano(), 10)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(s.d.SinkURL, "/")+"/api/v1/egress?"+v.Encode(), nil)
	if err != nil {
		return sinkEgress{}, err
	}
	t.PassOn(req.Header)
	resp, err := s.d.SinkClient.Do(req)
	if err != nil {
		return sinkEgress{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return sinkEgress{}, errSinkTooOld
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return sinkEgress{}, fmt.Errorf("the sink answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out sinkEgress
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return sinkEgress{}, fmt.Errorf("the sink's answer: %w", err)
	}
	return out, nil
}
