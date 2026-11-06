// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package quickwit

import (
	"encoding/json"
	"testing"
)

func TestSpanQueryString(t *testing.T) {
	for _, c := range []struct {
		q    SpanQuery
		want string
	}{
		{SpanQuery{}, "*"},
		{SpanQuery{Tenant: "acme", Environment: "e", Source: "ingress", TraceID: "ab", Service: "api", Operation: `GET "/"`},
			`tenant:"acme" AND environment:"e" AND source:"ingress" AND trace_id:"ab" AND service_name:"api" AND span_name:"GET \"/\""`},
		{SpanQuery{SinceNs: 1, MinDurationNs: 5}, `ts:[1970-01-01T00:00:00.000000001Z TO *] AND duration_ns:[5 TO *]`},
		{SpanQuery{MaxDurationNs: 7}, `duration_ns:[* TO 7]`},
		{SpanQuery{Tenant: "acme", Environments: []string{"e1", "e2"}},
			`tenant:"acme" AND (environment:"e1" OR environment:"e2")`},
		{SpanQuery{Environments: []string{}}, `(NOT *)`},
		{SpanQuery{Tags: map[string]string{"b": "2", "http.method": "GET", "a b:c": `x"y`}},
			`(span_attributes.a\ b\:c:"x\"y" OR resource_attributes.a\ b\:c:"x\"y") AND ` +
				`(span_attributes.b:"2" OR resource_attributes.b:"2") AND ` +
				`(span_attributes.http.method:"GET" OR resource_attributes.http.method:"GET")`},
	} {
		if got := c.q.String(); got != c.want {
			t.Errorf("%+v:\n got %s\nwant %s", c.q, got, c.want)
		}
	}
}

func TestSpanQueryMatch(t *testing.T) {
	d := SpanDoc{TS: 100, Tenant: "acme", Environment: "e", Source: "container", TraceID: "t", ServiceName: "api", SpanName: "GET",
		DurationNs: 50, SpanAttributes: map[string]json.RawMessage{"code": json.RawMessage(`500`), "f": json.RawMessage(`2.0`),
			"ok": json.RawMessage(`true`)},
		ResourceAttributes: map[string]json.RawMessage{"ferun.object": json.RawMessage(`"web"`)}}
	for _, c := range []struct {
		q    SpanQuery
		want bool
	}{
		{SpanQuery{Tenant: "acme"}, true},
		{SpanQuery{Tenant: "other"}, false},
		{SpanQuery{Environment: "e", Source: "container", TraceID: "t", Service: "api", Operation: "GET"}, true},
		{SpanQuery{Source: "ingress"}, false},
		{SpanQuery{SinceNs: 100, UntilNs: 100}, true},
		{SpanQuery{SinceNs: 101}, false},
		{SpanQuery{UntilNs: 99}, false},
		{SpanQuery{MinDurationNs: 50, MaxDurationNs: 50}, true},
		{SpanQuery{MinDurationNs: 51}, false},
		{SpanQuery{MaxDurationNs: 49}, false},
		{SpanQuery{Tags: map[string]string{"code": "500", "f": "2", "ok": "true", "ferun.object": "web"}}, true},
		{SpanQuery{Tags: map[string]string{"code": "501"}}, false},
		{SpanQuery{Tags: map[string]string{"nope": "x"}}, false},
		{SpanQuery{Environments: []string{"x", "e"}}, true},
		{SpanQuery{Environments: []string{"x"}}, false},
		{SpanQuery{Environments: []string{}}, false},
	} {
		if got := c.q.Match(&d); got != c.want {
			t.Errorf("%+v: %v", c.q, got)
		}
	}
}
