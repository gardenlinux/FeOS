// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The activity routes' page sizes.
const (
	activityDefaultLimit = 200
	activityMaxLimit     = 1000
)

// sourceTraffic selects the traffic records (ADR 0013) as events.
const sourceTraffic = "traffic"

// The kinds of the events the activity routes make from other rows than
// activity's.
const (
	kindGap = "gap"
)

// activityEvent is one event of the activity log as the UI reads it: the
// old event and span model. tsNs, exitCode and durationNs are int64
// strings; exitCode and durationNs are absent when the event has none.
type activityEvent struct {
	Kind         string `json:"kind"`
	TsNs         i64    `json:"tsNs"`
	Node         string `json:"node"`
	Source       string `json:"source"`
	Category     string `json:"category"`
	RequestID    string `json:"requestId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Name         string `json:"name"`
	// V is the envelope generation: 2 for ferund's span rows, 1 for the
	// records of nodes.
	V int `json:"v"`
	// EventID is unique and stable across reads (the UI keys on it).
	EventID         string `json:"eventId"`
	Tool            string `json:"tool"`
	ParamsJSON      string `json:"paramsJson"`
	ExitCode        *i64   `json:"exitCode,omitempty"`
	DurationNs      *i64   `json:"durationNs,omitempty"`
	Output          string `json:"output"`
	OutputTruncated bool   `json:"outputTruncated"`
	DetailJSON      string `json:"detailJson"`
	// PodID is the pod a node record names, as sent.
	PodID string `json:"podId,omitempty"`

	// stream is the node stream (node, start) of a node's record, zero for
	// ferund's own rows; the page's gaps are those of its events' streams.
	stream store.NodeStartKey
}

// activityQuery reads the page parameters: limit (default 200, at most
// 1000), beforeTsNs and source ("", ferund, feosd, authn, traffic).
func activityQuery(r *http.Request) (store.ActivityQuery, error) {
	limit, err := queryInt(r, "limit")
	if err != nil {
		return store.ActivityQuery{}, err
	}
	if limit <= 0 {
		limit = activityDefaultLimit
	}
	limit = min(limit, activityMaxLimit)
	before, err := queryInt64(r, "beforeTsNs")
	if err != nil {
		return store.ActivityQuery{}, err
	}
	source := r.URL.Query().Get("source")
	switch source {
	case "", store.SourceFerund, store.SourceFeosd, store.SourceAuthn, sourceTraffic:
	default:
		return store.ActivityQuery{}, apierr.New(http.StatusBadRequest,
			"source must be %s, %s, %s or %s, got %q", store.SourceFerund, store.SourceFeosd, store.SourceAuthn, sourceTraffic, source)
	}
	return store.ActivityQuery{Limit: limit, BeforeTsNs: before, Source: source}, nil
}

// sandboxActivity is GET /api/v1/admin/sandboxes/{id}/activity: a
// sandbox's attributed activity, ascending by time, the newest page before
// beforeTsNs, whole. No existence check: a deleted sandbox's log stays
// readable.
func (s *server) sandboxActivity(w http.ResponseWriter, r *http.Request) error {
	id, err := pathSandboxID(r)
	if err != nil {
		return err
	}
	q, err := activityQuery(r)
	if err != nil {
		return err
	}
	return s.activityPage(w, r, q,
		func(ctx context.Context, q store.ActivityQuery) ([]store.ActivityRow, error) {
			return s.st.ListSandboxActivity(ctx, id, q)
		},
		func(ctx context.Context, q store.ActivityQuery) ([]store.TrafficRow, error) {
			return s.st.ListSandboxTraffic(ctx, id, q)
		})
}

// tenantSandboxActivity is GET /api/v1/sandboxes/{id}/activity: the
// activity of one of the tenant's sandboxes for the readers of its
// environment (ADR 0031), with the admin route's parameters and answer
// but without what is the fleet's: a placement names the node it chose
// and no other node's verdict, and a node's overbooking is left out. An
// operator reads it whole. A deleted sandbox's log is the admin route's.
func (s *server) tenantSandboxActivity(w http.ResponseWriter, r *http.Request) error {
	sb, err := s.owned(r)
	if err != nil {
		return err
	}
	q, err := activityQuery(r)
	if err != nil {
		return err
	}
	t, _ := auth.TenantFrom(r.Context())
	whole := t.Principal.Operator
	if !whole {
		q.SkipKinds = fleetKinds
	}
	return s.activityPage(w, r, q,
		func(ctx context.Context, q store.ActivityQuery) ([]store.ActivityRow, error) {
			rows, err := s.st.ListSandboxActivity(ctx, sb.ID, q)
			if err != nil || whole {
				return rows, err
			}
			for i := range rows {
				rows[i] = withoutFleet(rows[i])
			}
			return rows, nil
		},
		func(ctx context.Context, q store.ActivityQuery) ([]store.TrafficRow, error) {
			return s.st.ListSandboxTraffic(ctx, sb.ID, q)
		})
}

// fleetKinds are the kinds of a sandbox's rows that tell of the fleet,
// not of the sandbox: a node's overbooking, with its booked capacity.
var fleetKinds = []string{activity.KindOverbooked}

// withoutFleet is a row as a tenant reads it: a placement without its
// candidates, every node's verdict on the pod with that node's room. A
// detail that does not parse is left out whole.
func withoutFleet(a store.ActivityRow) store.ActivityRow {
	if a.Kind != activity.KindPlacement || len(a.Detail) == 0 {
		return a
	}
	var d map[string]json.RawMessage
	if err := json.Unmarshal(a.Detail, &d); err != nil {
		a.Detail = nil
		return a
	}
	delete(d, "candidates")
	b, err := json.Marshal(d)
	if err != nil {
		a.Detail = nil
		return a
	}
	a.Detail = b
	return a
}

// nodeActivity is GET /api/v1/admin/nodes/{node}/activity: a node's own
// rows and the records it sent about pods that are not on it.
func (s *server) nodeActivity(w http.ResponseWriter, r *http.Request) error {
	node := r.PathValue("node")
	q, err := activityQuery(r)
	if err != nil {
		return err
	}
	return s.activityPage(w, r, q,
		func(ctx context.Context, q store.ActivityQuery) ([]store.ActivityRow, error) {
			return s.st.ListNodeActivity(ctx, node, q)
		},
		func(ctx context.Context, q store.ActivityQuery) ([]store.TrafficRow, error) {
			return s.st.ListNodeTraffic(ctx, node, q)
		})
}

// activityPage answers one page: the activity rows of q's source, or the
// traffic records for "traffic", or for "" both, merged by time; then the
// gaps of the node streams the page shows, detected within its time
// range.
func (s *server) activityPage(w http.ResponseWriter, r *http.Request, q store.ActivityQuery,
	rows func(context.Context, store.ActivityQuery) ([]store.ActivityRow, error),
	traffic func(context.Context, store.ActivityQuery) ([]store.TrafficRow, error),
) error {
	ctx := r.Context()
	var events []activityEvent
	if q.Source != sourceTraffic {
		as, err := rows(ctx, q)
		if err != nil {
			return fmt.Errorf("list activity: %w", err)
		}
		for _, a := range as {
			events = append(events, eventOfActivity(a))
		}
	}
	if q.Source == "" || q.Source == sourceTraffic {
		ts, err := traffic(ctx, q)
		if err != nil {
			return fmt.Errorf("list traffic: %w", err)
		}
		for _, t := range ts {
			events = append(events, eventOfTraffic(t))
		}
	}
	// The newest page of the merged rows, ascending.
	slices.SortStableFunc(events, func(a, b activityEvent) int { return cmp.Compare(a.TsNs, b.TsNs) })
	if len(events) > q.Limit {
		events = events[len(events)-q.Limit:]
	}
	// The streams of the events returned, not of those cut off.
	starts := map[store.NodeStartKey]bool{}
	for _, ev := range events {
		if ev.stream != (store.NodeStartKey{}) {
			starts[ev.stream] = true
		}
	}
	if len(events) > 0 && len(starts) > 0 {
		gaps, err := s.gapEvents(ctx, events, starts, q.BeforeTsNs)
		if err != nil {
			return err
		}
		if len(gaps) > 0 {
			events = append(events, gaps...)
			slices.SortStableFunc(events, func(a, b activityEvent) int { return cmp.Compare(a.TsNs, b.TsNs) })
		}
	}
	if events == nil {
		events = []activityEvent{}
	}
	return writeJSON(w, http.StatusOK, struct {
		Events []activityEvent `json:"events"`
	}{events})
}

// gapEvents are the gaps of the node streams of a page: those detected
// from the page's first event on, up to its upper bound (beforeTsNs, or
// now for the newest page). A gap is found when the event after it is
// stored, which is just after that event's time.
func (s *server) gapEvents(ctx context.Context, page []activityEvent, starts map[store.NodeStartKey]bool, before int64) ([]activityEvent, error) {
	keys := make([]store.NodeStartKey, 0, len(starts))
	for k := range starts {
		keys = append(keys, k)
	}
	from := time.Unix(0, int64(page[0].TsNs))
	to := time.Unix(0, math.MaxInt64)
	if before > 0 {
		to = time.Unix(0, before-1)
	}
	gaps, err := s.st.EventGaps(ctx, keys, from, to)
	if err != nil {
		return nil, fmt.Errorf("list gaps: %w", err)
	}
	out := make([]activityEvent, 0, len(gaps))
	for _, g := range gaps {
		n := g.ToSeq - g.FromSeq + 1
		out = append(out, activityEvent{
			Kind: kindGap, TsNs: i64(g.DetectedAt.UnixNano()), Node: g.Node, Source: store.SourceFeosd,
			Category: activity.CategoryLifecycle, Name: fmt.Sprintf("gap in stream: %d events lost", n), V: 1,
			EventID:    fmt.Sprintf("%s:%s:gap:%d-%d", g.Node, g.StartID, g.FromSeq, g.ToSeq),
			ParamsJSON: "",
			DetailJSON: string(detail(map[string]any{"from": g.FromSeq, "to": g.ToSeq, "startId": g.StartID.String()})),
		})
	}
	return out, nil
}

// eventOfActivity renders an activity row. eventId: <node>:<start>:<seq>
// for a node's record, <node>:<start>:start for a node's new start,
// ferund:<id> for ferund's own rows.
func eventOfActivity(a store.ActivityRow) activityEvent {
	ev := activityEvent{
		Kind: a.Kind, TsNs: i64(a.TsNs), Node: a.Node, Source: a.Source, Category: a.Category,
		RequestID: a.RequestID, SpanID: a.SpanID, ParentSpanID: a.ParentSpanID, Name: a.Name, V: 1,
		Tool: a.Tool, Output: a.Output, OutputTruncated: a.OutputTruncated, DetailJSON: "{}",
	}
	if a.Source == store.SourceFerund {
		ev.V = 2
	}
	switch {
	case a.Seq != 0 && a.StartID != uuid.Nil:
		ev.EventID = fmt.Sprintf("%s:%s:%d", a.Node, a.StartID, a.Seq)
	case a.Kind == activity.KindNodeStart && a.StartID != uuid.Nil:
		ev.EventID = fmt.Sprintf("%s:%s:start", a.Node, a.StartID)
	default:
		ev.EventID = "ferund:" + strconv.FormatInt(a.ID, 10)
	}
	if len(a.Params) > 0 {
		ev.ParamsJSON = string(a.Params)
	}
	if len(a.Detail) > 0 {
		ev.DetailJSON = string(a.Detail)
	}
	if a.ExitCode != nil {
		v := i64(*a.ExitCode)
		ev.ExitCode = &v
	}
	if a.DurationNs != nil {
		v := i64(*a.DurationNs)
		ev.DurationNs = &v
	}
	if !a.PodID.IsZero() {
		ev.PodID = a.PodID.String()
	}
	if a.StartID != uuid.Nil && a.Node != "" {
		ev.stream = store.NodeStartKey{Node: a.Node, StartID: a.StartID}
	}
	return ev
}

// eventOfTraffic renders a traffic record as an event: its kind, source
// traffic, category network, the record's protojson as the detail.
func eventOfTraffic(t store.TrafficRow) activityEvent {
	ev := activityEvent{
		Kind: t.Kind, TsNs: i64(t.TsNs), Node: t.Node, Source: sourceTraffic, Category: activity.CategoryNetwork,
		V: 1, EventID: fmt.Sprintf("%s:%s:%d", t.Node, t.StartID, t.Seq), DetailJSON: "{}",
		stream: store.NodeStartKey{Node: t.Node, StartID: t.StartID},
	}
	if len(t.Detail) > 0 && json.Valid(t.Detail) {
		ev.DetailJSON = string(t.Detail)
	}
	if !t.PodID.IsZero() {
		ev.PodID = t.PodID.String()
	}
	return ev
}
