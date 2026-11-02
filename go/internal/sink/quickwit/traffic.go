// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package quickwit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// The attributes of the ingress's server spans that tell a failed request
// (docs/architecture/traces.md, The ingress).
const (
	attrStatus  = "http.response.status_code"
	attrOutcome = "ferun.outcome"
)

// statusTerms are the status codes lo to hi as the terms of a set of the
// query language (field:IN [500 501 …]).
func statusTerms(lo, hi int) string {
	var b strings.Builder
	for c := lo; c <= hi; c++ {
		if c > lo {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.Itoa(c))
	}
	return b.String()
}

// failedClause is SpanQuery.Failed in the query language. A range needs
// a fast field, which an attribute is not, so the codes are a set of
// terms (net/http writes 100 to 999). A status that is left out (none
// written) is told by the outcome: a request that was neither aborted
// nor upgraded and has no status is of the class 2xx at the ingress
// (proxy's recorder.class), so 2xx without a code of 200 to 299 is one.
var failedClause = "(span_attributes." + attrStatus + ":IN [" + statusTerms(500, 999) + "] OR (span_attributes." +
	attrOutcome + `:"2xx" AND NOT span_attributes.` + attrStatus + ":IN [" + statusTerms(200, 299) + "]))"

// Failed reports whether a span of the ingress's is the span of a failed
// request, as FeRun counts the errors of its traffic: a status of 500 or
// more, or none written unless the client went away (outcome aborted).
// It matches what failedClause finds.
func Failed(d *SpanDoc) bool {
	code, ok := intValue(d.SpanAttributes[attrStatus])
	if ok && code >= 500 && code <= 999 {
		return true
	}
	return valueIs(d.SpanAttributes[attrOutcome], "2xx") && !(ok && code >= 200 && code <= 299)
}

// intValue is a JSON value that is an integer.
func intValue(raw json.RawMessage) (int64, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var n json.Number
	if len(raw) == 0 || d.Decode(&n) != nil {
		return 0, false
	}
	v, err := n.Int64()
	return v, err == nil
}

// Bucket is a step of a Histogram: its start in Unix ns, how many spans
// started in it, and the 95th percentile of their durations in ns (nil:
// not asked).
type Bucket struct {
	StartNs int64
	Count   int64
	P95Ns   *float64
}

// Histogram is the spans of a search per step of time: how many, the
// 50th and 95th percentiles of their durations in ns (nil: no span, or
// not asked), and the steps that hold spans, the oldest first.
type Histogram struct {
	Count        int64
	P50Ns, P95Ns *float64
	Buckets      []Bucket
}

// percentiles is a percentiles aggregation of the spans' durations.
func percentiles(pcts ...float64) map[string]any {
	return map[string]any{"percentiles": map[string]any{"field": "duration_ns", "percents": pcts, "keyed": false}}
}

// percentileValues is the answer of a percentiles aggregation (keyed:
// false); a value is null without documents.
type percentileValues struct {
	Values []struct {
		Key   float64  `json:"key"`
		Value *float64 `json:"value"`
	} `json:"values"`
}

func (p percentileValues) at(pct float64) *float64 {
	for _, v := range p.Values {
		if v.Key == pct && v.Value != nil && !math.IsNaN(*v.Value) {
			return v.Value
		}
	}
	return nil
}

// SpanHistogram answers q's spans per step of their ts, the steps aligned
// to the epoch (a date_histogram's fixed interval), and with durations
// the percentiles of their durations, Quickwit's estimates (a sketch of
// about 1 % relative error), overall and per step. step is whole
// milliseconds, at least one.
func (c *Client) SpanHistogram(ctx context.Context, pattern string, q SpanQuery, step time.Duration, durations bool) (Histogram, error) {
	if step < time.Millisecond || step%time.Millisecond != 0 {
		return Histogram{}, fmt.Errorf("quickwit: a histogram's step must be whole milliseconds, got %s", step)
	}
	steps := map[string]any{"date_histogram": map[string]any{"field": "ts",
		"fixed_interval": strconv.FormatInt(step.Milliseconds(), 10) + "ms", "min_doc_count": 1}}
	aggs := map[string]any{"steps": steps}
	if durations {
		steps["aggs"] = map[string]any{"p95": percentiles(95)}
		aggs["durations"] = percentiles(50, 95)
	}
	req := spanSearch(q, 0)
	req["aggs"] = aggs
	var out struct {
		Aggregations struct {
			Steps struct {
				Buckets []struct {
					Key      float64          `json:"key"` // ms since the epoch
					DocCount int64            `json:"doc_count"`
					P95      percentileValues `json:"p95"`
				} `json:"buckets"`
			} `json:"steps"`
			Durations percentileValues `json:"durations"`
		} `json:"aggregations"`
		Errors []string `json:"errors"`
	}
	if err := c.searchJSON(ctx, pattern, req, &out); err != nil {
		return Histogram{}, err
	}
	h := Histogram{P50Ns: out.Aggregations.Durations.at(50), P95Ns: out.Aggregations.Durations.at(95),
		Buckets: make([]Bucket, 0, len(out.Aggregations.Steps.Buckets))}
	for _, b := range out.Aggregations.Steps.Buckets {
		if b.DocCount == 0 {
			continue
		}
		h.Count += b.DocCount
		h.Buckets = append(h.Buckets, Bucket{StartNs: int64(math.Round(b.Key)) * int64(time.Millisecond), Count: b.DocCount,
			P95Ns: b.P95.at(95)})
	}
	return h, searchErrors(out.Errors)
}
