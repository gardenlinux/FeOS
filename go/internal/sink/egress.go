// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// The bounds of a read of the bindings' requests (traces.md, Reading: the
// bindings' requests): its range, the span names (method and binding)
// and the users of each a search counts, and the searches at once.
const (
	maxEgressRange = 7 * 24 * time.Hour
	maxEgressNames = 1000
	maxEgressUsers = 1000
	egressLoads    = 4
)

// egressKinds are the kinds of the pods whose requests a read counts per
// user: an environment's services and jobs and its sandboxes (ferun.kind).
var egressKinds = []string{"service", "job", "sandbox"}

// EgressQuery is a read of the requests of a project's environment's pods
// through their HTTP bindings in [SinceNs, UntilNs), Unix ns.
type EgressQuery struct {
	Project, Environment string
	SinceNs, UntilNs     int64
}

// Egress is the answer of a read: per binding that had a request in the
// range, by name, what its pods' egress spans count; Truncated says a
// search left span names or users out (more than it counts), so that
// the counts are short of them.
type Egress struct {
	Bindings  []BindingEgress `json:"bindings"`
	Truncated bool            `json:"truncated"`
}

// BindingEgress is a binding's requests in the range: all of them, those
// of each class (quickwit.EgressClasses), the start of the newest (Unix
// ns), the newest that was a problem (null for none), and the pods that
// sent them by their kind and object (a sandbox by its ID), the most
// requests first.
type BindingEgress struct {
	Binding     string         `json:"binding"`
	Requests    int64          `json:"requests"`
	Refused     int64          `json:"refused"`
	Failing     int64          `json:"failing"`
	TokenFailed int64          `json:"tokenFailed"`
	Blocked     int64          `json:"blocked"`
	LastNs      int64          `json:"lastNs,string"`
	LastProblem *EgressProblem `json:"lastProblem"`
	Users       []EgressUser   `json:"users"`
}

// EgressUser is a workload's requests through a binding.
type EgressUser struct {
	Kind     string `json:"kind"`
	Object   string `json:"object"`
	Requests int64  `json:"requests"`
}

// EgressProblem is a request that was a problem: its start (Unix ns), its
// class, the status it was answered with (0: none), feos-authn's code
// and message of a refusal of its own ("" for the upstream's answer),
// and its trace and span.
type EgressProblem struct {
	TsNs    int64  `json:"tsNs,string"`
	Class   string `json:"class"`
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`
}

// egressBinding is the binding of an egress span's name, "<METHOD>
// <binding>"; ok false for a request no binding matched (its name the
// method alone).
func egressBinding(spanName string) (string, bool) {
	_, b, ok := strings.Cut(spanName, " ")
	return b, ok && b != ""
}

// Egress counts the requests of q's environment's pods through their
// bindings from their egress spans in the trace index (traces.md, A
// pod's egress): one search for all of them, one per kind of pod for
// their users, one per class, each grouped by the span's name, then per
// binding with a problem a search for the newest. The index alone is
// read: a span is there at most a commit (5 s) after it came, and a pod
// sends it once the request's answer ended.
func (t *Traces) Egress(ctx context.Context, q EgressQuery) (Egress, error) {
	base := quickwit.SpanQuery{Project: q.Project, Environment: q.Environment, Source: SourceEgress, SinceNs: q.SinceNs,
		UntilNs: q.UntilNs - 1}
	type read struct {
		q      quickwit.SpanQuery
		fields []string
		groups []quickwit.SpanGroup
		more   bool
	}
	reads := []*read{{q: base, fields: []string{"span_name"}}}
	for _, k := range egressKinds {
		sel := base
		sel.ObjectKind = k
		reads = append(reads, &read{q: sel, fields: []string{"span_name", "service_name"}})
	}
	for _, c := range quickwit.EgressClasses {
		sel := base
		sel.Egress = c
		reads = append(reads, &read{q: sel, fields: []string{"span_name"}})
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(egressLoads)
	for _, r := range reads {
		g.Go(func() error {
			size := maxEgressNames
			if len(r.fields) > 1 {
				size = maxEgressUsers
			}
			var err error
			if r.groups, r.more, err = t.index.SpanGroups(gctx, t.pattern(), r.q, r.fields, size); err != nil {
				return fmt.Errorf("index: the bindings' requests (%s): %w", r.q, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return Egress{}, err
	}

	out := Egress{Bindings: []BindingEgress{}}
	byName := map[string]*BindingEgress{}
	binding := func(spanName string) *BindingEgress {
		name, ok := egressBinding(spanName)
		if !ok {
			return nil
		}
		if byName[name] == nil {
			byName[name] = &BindingEgress{Binding: name, Users: []EgressUser{}}
		}
		return byName[name]
	}
	for _, r := range reads {
		out.Truncated = out.Truncated || r.more
	}
	for _, gr := range reads[0].groups {
		if b := binding(gr.Keys[0]); b != nil {
			b.Requests += gr.Count
			b.LastNs = max(b.LastNs, gr.LastNs)
		}
	}
	for i, k := range egressKinds {
		for _, gr := range reads[1+i].groups {
			b := binding(gr.Keys[0])
			if b == nil {
				continue
			}
			if j := slices.IndexFunc(b.Users, func(u EgressUser) bool { return u.Kind == k && u.Object == gr.Keys[1] }); j >= 0 {
				b.Users[j].Requests += gr.Count
			} else {
				b.Users = append(b.Users, EgressUser{Kind: k, Object: gr.Keys[1], Requests: gr.Count})
			}
		}
	}
	for i, c := range quickwit.EgressClasses {
		for _, gr := range reads[1+len(egressKinds)+i].groups {
			b := binding(gr.Keys[0])
			if b == nil {
				continue
			}
			switch c {
			case quickwit.EgressRefused:
				b.Refused += gr.Count
			case quickwit.EgressFailing:
				b.Failing += gr.Count
			case quickwit.EgressTokenFailed:
				b.TokenFailed += gr.Count
			case quickwit.EgressBlocked:
				b.Blocked += gr.Count
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		b := byName[name]
		slices.SortFunc(b.Users, func(x, y EgressUser) int {
			return cmp.Or(cmp.Compare(y.Requests, x.Requests), cmp.Compare(x.Kind, y.Kind), cmp.Compare(x.Object, y.Object))
		})
		out.Bindings = append(out.Bindings, *b)
	}

	// The newest problem of each binding that had one.
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(egressLoads)
	for i := range out.Bindings {
		b := &out.Bindings[i]
		if b.Refused+b.Failing+b.TokenFailed+b.Blocked == 0 {
			continue
		}
		g.Go(func() error {
			sel := base
			sel.Egress, sel.Tags, sel.Max = quickwit.EgressProblem, map[string]string{quickwit.AttrBinding: b.Binding}, 1
			docs, err := t.index.NewestSpans(gctx, t.pattern(), sel)
			if err != nil {
				return fmt.Errorf("index: the newest problem of binding %s: %w", b.Binding, err)
			}
			if len(docs) > 0 {
				d := &docs[0]
				b.LastProblem = &EgressProblem{TsNs: d.TS, Class: quickwit.EgressClass(d), Status: quickwit.EgressStatus(d),
					Code: quickwit.EgressCode(d), TraceID: d.TraceID, SpanID: d.SpanID}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return Egress{}, err
	}
	return out, nil
}

// egressMessageWindow is how far from a problem's span its record is
// looked for in the log index: the span starts at the request's arrival,
// the record at its start.
const egressMessageWindow = 10 * time.Minute

// egressMessage is feos-authn's message of a refusal: the text's
// error.message of the pod's egress record of the request (sink.md, A
// pod's egress records), found in the log index by the request's trace
// and span; "" when it is not there.
func (s *Store) egressMessage(ctx context.Context, project, env string, p *EgressProblem) (string, error) {
	if p.TraceID == "" || p.SpanID == "" {
		return "", nil
	}
	docs, err := s.index.Search(ctx, s.indexPattern(), quickwit.Query{Project: project, Environment: env,
		Source: SourceEgress, Trace: p.TraceID, SinceNs: p.TsNs - int64(egressMessageWindow),
		UntilNs: p.TsNs + int64(egressMessageWindow), Order: quickwit.NewestFirst, Max: 100})
	if err != nil {
		return "", err
	}
	for _, d := range docs {
		var rec struct {
			SpanID string `json:"spanId"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(d.Text), &rec) == nil && rec.SpanID == p.SpanID && rec.Error != nil {
			return rec.Error.Message, nil
		}
	}
	return "", nil
}

// parseEgressQuery reads the parameters of /api/v1/egress: environment,
// since and until (RFC 3339 or Unix ns), the range [since, until) at
// most 7 days.
func parseEgressQuery(v url.Values, project string) (EgressQuery, error) {
	q := EgressQuery{Project: project, Environment: v.Get("environment")}
	if q.Environment == "" {
		return q, fmt.Errorf("environment is required")
	}
	for _, p := range []struct {
		name string
		dst  *int64
	}{{"since", &q.SinceNs}, {"until", &q.UntilNs}} {
		s := v.Get(p.name)
		if s == "" {
			return q, fmt.Errorf("%s is required", p.name)
		}
		var err error
		if *p.dst, err = parseTime(p.name, s); err != nil {
			return q, err
		}
		if *p.dst <= 0 {
			return q, fmt.Errorf("%s must be after the epoch", p.name)
		}
	}
	if q.SinceNs >= q.UntilNs {
		return q, fmt.Errorf("since must be before until")
	}
	if q.UntilNs-q.SinceNs > int64(maxEgressRange) {
		return q, fmt.Errorf("the range is at most %s", maxEgressRange)
	}
	return q, nil
}

// egress is GET /api/v1/egress: the read, and the messages of the
// problems that are feos-authn's refusals; a message that cannot be read
// is left empty.
func (a *api) egress(w http.ResponseWriter, r *http.Request, acc Access) {
	q, err := parseEgressQuery(r.URL.Query(), acc.Project)
	if err != nil {
		apierr.Write(w, apierr.New(http.StatusBadRequest, "%s", err.Error()))
		return
	}
	if !acc.mayRead(q.Environment) {
		apierr.Write(w, apierr.New(http.StatusNotFound, "no such environment"))
		return
	}
	out, err := a.traces.Egress(r.Context(), q)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for i := range out.Bindings {
		p := out.Bindings[i].LastProblem
		if p == nil || p.Code == "" {
			continue
		}
		if p.Message, err = a.store.egressMessage(r.Context(), q.Project, q.Environment, p); err != nil {
			if r.Context().Err() != nil {
				return
			}
			a.log.Warn("the message of a binding's refusal", "environment", q.Environment, "trace", p.TraceID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
