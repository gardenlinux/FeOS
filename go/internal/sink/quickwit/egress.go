// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package quickwit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
)

// The attributes of a pod's egress span (docs/architecture/traces.md, A
// pod's egress): the binding the request went through ("" when none
// matched), and the code of a refusal of feos-authn's own, which the
// upstream's answers do not have.
const (
	AttrBinding   = "ferun.binding"
	attrErrorCode = "ferun.error.code"
)

// The classes of a pod's request through a binding as its egress span
// tells them (SpanQuery.Egress, EgressClass), those of the bindings'
// incidents in ferund (store.ClassifyEgress) and the refusals that count
// for none of them:
//
//	EgressRefused      the upstream answered 401 or 403
//	EgressFailing      the upstream answered 429 or 5xx, or feos-authn
//	                   could not reach it (unreachable, tls)
//	EgressTokenFailed  no OAuth token could be had (credential_unavailable)
//	EgressBlocked      feos-authn refused it for any other code
//
// EgressProblem selects any of the four.
const (
	EgressRefused     = "refused"
	EgressFailing     = "failing"
	EgressTokenFailed = "tokenFailed"
	EgressBlocked     = "blocked"
	EgressProblem     = "problem"
)

// EgressClasses are the classes of a request that is a problem, in the
// order of EgressClass's precedence.
var EgressClasses = []string{EgressRefused, EgressFailing, EgressTokenFailed, EgressBlocked}

// The codes of feos-authn's refusals (docs/architecture/sink.md, A pod's
// egress records): those of an upstream it could not reach, of a token
// it could not get, and every other. The trace index cannot ask whether
// a span has an attribute (it keeps no field presence), so a refusal is
// told by its code: a code not listed here counts by its status, as the
// upstream's answer does, in the query and in EgressClass alike.
var (
	egressUnreachable = []string{"unreachable", "tls"}
	egressNoToken     = []string{"credential_unavailable"}
	egressBlocked     = []string{"denied_destination", "denied_path", "unknown_binding", "method_not_allowed",
		"misdirected", "rate_limited", "not_ready", "binding_invalid", "audit_unavailable", "bad_request"}
	egressCodes = slices.Concat(egressUnreachable, egressNoToken, egressBlocked)
)

// egressClauses are the classes in the query language (SpanQuery.Egress).
var egressClauses = func() map[string]string {
	code, status := "span_attributes."+fieldPath(attrErrorCode), "span_attributes."+attrStatus
	in := func(field string, terms []string) string { return field + ":IN [" + strings.Join(terms, " ") + "]" }
	answered := "NOT " + in(code, egressCodes)
	m := map[string]string{
		EgressRefused: "(" + status + ":IN [401 403] AND " + answered + ")",
		EgressFailing: "(" + in(code, egressUnreachable) + " OR (" + status + ":IN [429 " + statusTerms(500, 999) +
			"] AND " + answered + "))",
		EgressTokenFailed: in(code, egressNoToken),
		EgressBlocked:     in(code, egressBlocked),
	}
	var any []string
	for _, c := range EgressClasses {
		any = append(any, m[c])
	}
	m[EgressProblem] = "(" + strings.Join(any, " OR ") + ")"
	return m
}()

// EgressClass is the class of a pod's egress span, "" for a request the
// upstream answered with another status; it matches what the query of
// SpanQuery.Egress finds.
func EgressClass(d *SpanDoc) string {
	code := stringValue(d.SpanAttributes[attrErrorCode])
	status, _ := intValue(d.SpanAttributes[attrStatus])
	switch {
	case slices.Contains(egressUnreachable, code):
		return EgressFailing
	case slices.Contains(egressNoToken, code):
		return EgressTokenFailed
	case slices.Contains(egressBlocked, code):
		return EgressBlocked
	case status == 401 || status == 403:
		return EgressRefused
	case status == 429 || status >= 500 && status <= 999:
		return EgressFailing
	}
	return ""
}

// EgressCode is a span's refusal code, "" for the upstream's answer.
func EgressCode(d *SpanDoc) string { return stringValue(d.SpanAttributes[attrErrorCode]) }

// EgressStatus is the status a span's request was answered with, 0 when
// none was written.
func EgressStatus(d *SpanDoc) int {
	n, _ := intValue(d.SpanAttributes[attrStatus])
	return int(n)
}

// stringValue is a JSON value that is a string, else "".
func stringValue(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// SpanGroup is the spans of a search that share their values of the
// fields SpanGroups names: the values, in the fields' order, how many,
// and the start of the newest (Unix ns, to about a microsecond: Quickwit
// answers a maximum as a double).
type SpanGroup struct {
	Keys   []string
	Count  int64
	LastNs int64
}

// groupBuckets is a terms aggregation's answer, nested as SpanGroups
// asks.
type groupBuckets struct {
	Buckets []struct {
		Key      string `json:"key"`
		DocCount int64  `json:"doc_count"`
		Last     *struct {
			Value *float64 `json:"value"`
		} `json:"last"`
		Next *groupBuckets `json:"g"`
	} `json:"buckets"`
	SumOtherDocCount int64 `json:"sum_other_doc_count"`
}

// groupAggs is the aggregation of SpanGroups: a terms aggregation of each
// field nested in the one before it, the newest start at the innermost.
func groupAggs(fields []string, size int) map[string]any {
	terms := map[string]any{"terms": map[string]any{"field": fields[0], "size": size}}
	if len(fields) > 1 {
		terms["aggs"] = groupAggs(fields[1:], size)
	} else {
		terms["aggs"] = map[string]any{"last": map[string]any{"max": map[string]any{"field": "start_ns"}}}
	}
	return map[string]any{"g": terms}
}

// flatten appends b's groups under keys to out; more reports values left
// out at any level.
func (b *groupBuckets) flatten(keys []string, out []SpanGroup) ([]SpanGroup, bool) {
	more := b.SumOtherDocCount > 0
	for _, x := range b.Buckets {
		k := append(slices.Clone(keys), x.Key)
		if x.Next != nil {
			var m bool
			out, m = x.Next.flatten(k, out)
			more = more || m
			continue
		}
		g := SpanGroup{Keys: k, Count: x.DocCount}
		if x.Last != nil && x.Last.Value != nil && !math.IsNaN(*x.Last.Value) {
			g.LastNs = int64(*x.Last.Value)
		}
		out = append(out, g)
	}
	return out, more
}

// SpanGroups answers q's spans grouped by their values of fields (fast
// fields: span_name, service_name; one or more, nested in their order),
// at most size values of a field within a group of the one before it;
// more reports that some were left out (a terms aggregation's
// sum_other_doc_count), whose spans the groups do not count.
func (c *Client) SpanGroups(ctx context.Context, pattern string, q SpanQuery, fields []string, size int) (
	groups []SpanGroup, more bool, err error) {
	if len(fields) == 0 || size < 1 {
		return nil, false, fmt.Errorf("quickwit: a grouping needs a field and a size")
	}
	req := spanSearch(q, 0)
	req["aggs"] = groupAggs(fields, size)
	var out struct {
		Aggregations struct {
			G groupBuckets `json:"g"`
		} `json:"aggregations"`
		Errors []string `json:"errors"`
	}
	if err := c.searchJSON(ctx, pattern, req, &out); err != nil {
		return nil, false, err
	}
	groups, more = out.Aggregations.G.flatten(nil, nil)
	return groups, more, searchErrors(out.Errors)
}

// NewestSpans answers the spans of q in the indexes pattern names, the
// newest first, at most q.Max (bounded by MaxHits).
func (c *Client) NewestSpans(ctx context.Context, pattern string, q SpanQuery) ([]SpanDoc, error) {
	req := spanSearch(q, min(max(q.Max, 1), MaxHits))
	req["sort_by"] = "ts" // descending (Search)
	var out struct {
		Hits   []SpanDoc `json:"hits"`
		Errors []string  `json:"errors"`
	}
	if err := c.searchJSON(ctx, pattern, req, &out); err != nil {
		return nil, err
	}
	return out.Hits, searchErrors(out.Errors)
}
