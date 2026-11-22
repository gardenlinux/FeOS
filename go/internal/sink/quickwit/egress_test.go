// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package quickwit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The classes in the query language: a refusal by feos-authn's code,
// the upstream's answer by its status where the code is none of
// feos-authn's (the index cannot ask for a field's presence).
func TestEgressClauses(t *testing.T) {
	codes := "unreachable tls credential_unavailable denied_destination denied_path unknown_binding method_not_allowed " +
		"misdirected rate_limited not_ready binding_invalid audit_unavailable bad_request"
	answered := "NOT span_attributes.ferun.error.code:IN [" + codes + "]"
	for class, want := range map[string]string{
		EgressRefused:     "(span_attributes.http.response.status_code:IN [401 403] AND " + answered + ")",
		EgressTokenFailed: "span_attributes.ferun.error.code:IN [credential_unavailable]",
		EgressBlocked: "span_attributes.ferun.error.code:IN [denied_destination denied_path unknown_binding " +
			"method_not_allowed misdirected rate_limited not_ready binding_invalid audit_unavailable bad_request]",
	} {
		if got := (SpanQuery{Egress: class}).String(); got != want {
			t.Errorf("%s:\n%s\nwant\n%s", class, got, want)
		}
	}
	failing := SpanQuery{Egress: EgressFailing}.String()
	if !strings.HasPrefix(failing, "(span_attributes.ferun.error.code:IN [unreachable tls] OR "+
		"(span_attributes.http.response.status_code:IN [429 500 501 ") || !strings.HasSuffix(failing, " 999] AND "+answered+"))") {
		t.Errorf("failing: %s", failing)
	}
	problem := SpanQuery{Egress: EgressProblem}.String()
	for _, c := range EgressClasses {
		if !strings.Contains(problem, egressClauses[c]) {
			t.Errorf("problem lacks %s: %s", c, problem)
		}
	}
	if got := (SpanQuery{Environment: "e", ObjectKind: "sandbox", Source: "egress"}).String(); got !=
		`environment:"e" AND kind:"sandbox" AND source:"egress"` {
		t.Errorf("kind: %s", got)
	}
}

// SpanGroups asks for terms aggregations nested in the fields' order, the
// newest start innermost, and reads Quickwit 0.9's answer: a group per
// innermost bucket, more where a level left values out. NewestSpans
// sorts by ts descending.
func TestSpanGroups(t *testing.T) {
	var asked map[string]any
	answer := `{"num_hits": 6, "hits": [], "errors": [], "aggregations": {"g": {"buckets": [
	  {"key": "GET erp", "doc_count": 5, "g": {"buckets": [
	    {"key": "api", "doc_count": 4, "last": {"value": 1.795086861481846e+18}},
	    {"key": "sbx1", "doc_count": 1, "last": {"value": 1.795086855e+18}}], "sum_other_doc_count": 0}},
	  {"key": "PUT crm", "doc_count": 1, "g": {"buckets": [
	    {"key": "migrate", "doc_count": 1, "last": {"value": null}}], "sum_other_doc_count": 2}}],
	  "sum_other_doc_count": 0}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/ferun-traces-*/search" || json.Unmarshal(b, &asked) != nil {
			http.Error(w, `{"message":"no"}`, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	c, err := New(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := SpanQuery{Project: "acme", Environment: "e", Source: "egress", ObjectKind: "service"}
	groups, more, err := c.SpanGroups(context.Background(), "ferun-traces-*", q, []string{"span_name", "service_name"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if aggs, _ := json.Marshal(asked["aggs"]); string(aggs) != `{"g":{"aggs":{"g":{"aggs":{"last":{"max":{"field":"start_ns"}}},`+
		`"terms":{"field":"service_name","size":50}}},"terms":{"field":"span_name","size":50}}}` {
		t.Errorf("aggs %s", aggs)
	}
	if asked["max_hits"] != float64(0) || asked["query"] != `tenant:"acme" AND environment:"e" AND kind:"service" AND source:"egress"` { // KEEP-TENANT: Quickwit's stored name
		t.Errorf("asked %v", asked)
	}
	if got := fmt.Sprint(groups); !more || got != "[{[GET erp api] 4 1795086861481846016} {[GET erp sbx1] 1 1795086855000000000} "+
		"{[PUT crm migrate] 1 0}]" {
		t.Errorf("groups %s, more %v", got, more)
	}
	answer = `{"num_hits": 0, "hits": [], "errors": [], "aggregations": {"g": {"buckets": [], "sum_other_doc_count": 0}}}`
	if groups, more, err := c.SpanGroups(context.Background(), "ferun-traces-*", q, []string{"span_name"}, 5); err != nil ||
		len(groups) != 0 || more {
		t.Errorf("none: %v %v %v", groups, more, err)
	}
	if _, _, err := c.SpanGroups(context.Background(), "ferun-traces-*", q, nil, 5); err == nil {
		t.Error("no field")
	}

	answer = `{"num_hits": 1, "hits": [{"ts": 7, "span_id": "a"}], "errors": []}`
	q.Max = 1
	docs, err := c.NewestSpans(context.Background(), "ferun-traces-*", q)
	if err != nil || len(docs) != 1 || docs[0].SpanID != "a" || asked["sort_by"] != "ts" || asked["max_hits"] != float64(1) {
		t.Errorf("newest: %+v %v %v", docs, err, asked)
	}
}
