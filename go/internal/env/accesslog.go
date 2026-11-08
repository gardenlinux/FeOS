// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package env

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/auth"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The access log's page sizes.
const (
	accessLogDefaultLimit = 200
	accessLogMaxLimit     = 1000
)

// AccessLogQuery is a page of an environment's access log: at most Limit
// records (default 200, at most 1000) before BeforeTsNs (0: the newest),
// of the trace TraceID when it is set.
type AccessLogQuery struct {
	Limit      int
	BeforeTsNs int64
	TraceID    string
}

// AccessLogList is GET /api/v1/environments/{env}/access-logs: a page of
// the requests ferun-ingress answered for the environment, ascending by
// time. The next older page is the one before the first record's tsNs.
type AccessLogList struct {
	Records []AccessLogRecord `json:"records"`
}

// AccessLogRecord is one request and its server span at the ingress
// (ADR 0021). TsNs is its start in Unix nanoseconds, as a string; Path
// the escaped path without the query; Status 101 for an upgrade, 0 when
// none was written; Outcome the status class, "upgraded" or "aborted";
// Instance the instance of the last attempt, "" for none; HeldNs how long
// it waited for an instance. TraceID and SpanID name the span in W3C
// Trace Context, which the instance got in traceparent with SpanID as its
// parent; ParentSpanID is the client's span, "" when the request brought
// none.
type AccessLogRecord struct {
	TsNs         int64     `json:"tsNs,string"`
	Time         time.Time `json:"time"`
	DurationNs   int64     `json:"durationNs"`
	Ingress      string    `json:"ingress"`
	Service      string    `json:"service"`
	Instance     string    `json:"instance"`
	Host         string    `json:"host"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Proto        string    `json:"proto"`
	Status       int       `json:"status"`
	Outcome      string    `json:"outcome"`
	Attempts     int       `json:"attempts"`
	HeldNs       int64     `json:"heldNs"`
	BytesIn      int64     `json:"bytesIn"`
	BytesOut     int64     `json:"bytesOut"`
	ClientIP     string    `json:"clientIp"`
	UserAgent    string    `json:"userAgent"`
	TraceID      string    `json:"traceId"`
	SpanID       string    `json:"spanId"`
	ParentSpanID string    `json:"parentSpanId"`
	// Reason says why the ingress answered itself, "route-refused-client"
	// for a client outside a route's from (ADR 0034, revised); "": none.
	Reason string `json:"reason,omitempty"`
}

// The codes of the access log's answers when the sink cannot be read
// (503), which clients (FeCode) match.
const (
	// CodeNoLogStore: ferund has no --sink-url (or the request no token
	// or session to read the sink with), and the access log is kept
	// nowhere else.
	CodeNoLogStore = "no_log_store"
	// CodeLogStoreUnavailable: the sink failed; Retry-After.
	CodeLogStoreUnavailable = "log_store_unavailable"
)

// AccessLogs is GET /api/v1/environments/{env}/access-logs: the
// ingress's lines of the environment in the sink (ADR 0026 §8), read with
// the caller's token or session (auth.Tenant.PassOn). Without --sink-url
// or either it answers 503 no_log_store, and when the sink fails 503
// log_store_unavailable.
func (s *Service) AccessLogs(ctx context.Context, tenant, env string, q AccessLogQuery) (AccessLogList, error) {
	e, err := resolveEnv(ctx, s.st, tenant, env, store.NoLock)
	if err != nil {
		return AccessLogList{}, err
	}
	if q.Limit <= 0 {
		q.Limit = accessLogDefaultLimit
	}
	q.Limit = min(q.Limit, accessLogMaxLimit)
	t, _ := auth.TenantFrom(ctx)
	switch {
	case s.d.SinkURL == "":
		return AccessLogList{}, apierr.WithCode(http.StatusServiceUnavailable, CodeNoLogStore,
			"the access log is kept in the sink, and this ferund has no --sink-url")
	case !t.Passable():
		return AccessLogList{}, apierr.WithCode(http.StatusServiceUnavailable, CodeNoLogStore,
			"the access log is read from the sink with the caller's token or session, and the request has neither")
	}
	recs, err := s.sinkAccessLogs(ctx, t, e.ID.String(), q)
	if err != nil {
		s.log.Warn("cannot read the access log from the sink", "env", e.ID, "err", err)
		return AccessLogList{}, apierr.WithCode(http.StatusServiceUnavailable, CodeLogStoreUnavailable,
			"the log store cannot be read; try again shortly").RetryAfter(5)
	}
	return AccessLogList{Records: recs}, nil
}

// sinkAccessPages bounds the pages one read asks the sink for: a page
// may hold fewer lines than asked and still go on.
const sinkAccessPages = 8

// sinkAccessLogs reads the newest q.Limit records of the environment
// envID before q.BeforeTsNs (of q.TraceID) from the sink as t: its
// streams of source ingress, as many as it has, oldest first.
func (s *Service) sinkAccessLogs(ctx context.Context, t auth.Tenant, envID string, q AccessLogQuery) ([]AccessLogRecord, error) {
	v := url.Values{"environment": {envID}, "source": {"ingress"}, "direction": {"backward"}, "since": {"0"}}
	if q.BeforeTsNs > 0 {
		v.Set("until", strconv.FormatInt(q.BeforeTsNs-1, 10))
	}
	if q.TraceID != "" {
		v.Set("trace", q.TraceID)
	}
	var pages [][]AccessLogRecord
	n := 0
	for range sinkAccessPages {
		v.Set("limit", strconv.Itoa(q.Limit-n))
		page, cursor, err := s.sinkAccessPage(ctx, t, v)
		if err != nil {
			return nil, err
		}
		pages = append(pages, page)
		n += len(page)
		if cursor == "" || n >= q.Limit {
			break
		}
		v.Set("cursor", cursor)
	}
	out := make([]AccessLogRecord, 0, n)
	for i := len(pages) - 1; i >= 0; i-- {
		out = append(out, pages[i]...)
	}
	return out[max(len(out)-q.Limit, 0):], nil
}

// sinkAccessPage asks the sink for one page: its records, oldest first,
// and the cursor to the older page ("": none). A line that is not a
// record is left out.
func (s *Service) sinkAccessPage(ctx context.Context, t auth.Tenant, v url.Values) ([]AccessLogRecord, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(s.d.SinkURL, "/")+"/api/v1/logs?"+v.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	t.PassOn(req.Header)
	resp, err := s.d.SinkClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", fmt.Errorf("the sink answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var page struct {
		Lines []struct {
			TS   int64  `json:"ts,string"`
			Text string `json:"text"`
		} `json:"lines"`
		Cursor string `json:"cursor"`
	}
	// A record's line is some 3 KiB at the fields' bounds.
	if err := json.NewDecoder(io.LimitReader(resp.Body, int64(accessLogMaxLimit)<<13)).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("the sink's answer: %w", err)
	}
	out := make([]AccessLogRecord, 0, len(page.Lines))
	for _, l := range page.Lines {
		var r AccessLogRecord
		if err := json.Unmarshal([]byte(l.Text), &r); err != nil {
			continue
		}
		if r.TsNs == 0 {
			r.TsNs = l.TS
		}
		r.Time = time.Unix(0, r.TsNs).UTC()
		out = append(out, r)
	}
	return out, page.Cursor, nil
}
