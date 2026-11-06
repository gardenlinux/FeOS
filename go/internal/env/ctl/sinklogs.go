// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

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

	"github.com/gardenlinux/feos/go/internal/auth"
)

// sinkPage is the most lines sinkLog asks a page of the sink's GET
// /api/v1/logs for (the sink's own default; at most 5,000,
// docs/architecture/sink.md): of lines of up to 64 KiB, 32 MiB.
const sinkPage = 500

// sinkLog reads the last lines lines of a stream's main container from
// the sink (ADR 0026), oldest first, within answerLog's budget: sel names
// the stream (instance=<sandbox ID>, or job=<ID>), since its start. It
// asks with the caller's own token or session (auth.Tenant.PassOn), so
// that the sink answers what the caller may read only. ok is false
// without --sink-url or either, when the sink fails, and when it has no
// line: the caller reads the node then.
func (c *Controller) sinkLog(ctx context.Context, key, value string, since time.Time, lines int) ([]string, bool) {
	t, _ := auth.TenantFrom(ctx)
	if c.d.SinkURL == "" || !t.Passable() || lines <= 0 {
		return nil, false
	}
	q := url.Values{key: {value}, "container": {"main"}, "direction": {"backward"}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339Nano))
	}
	// Pages back until lines+1 lines have ended there (a line's parts
	// may begin on an older page; the oldest line, which may lack its
	// start, is dropped by the tail), the first sized to that.
	var pages [][]sinkLine
	ended, bytes, limit := 0, 0, min(lines+1, sinkPage)
	for ended <= lines && bytes <= answerLog.total {
		q.Set("limit", strconv.Itoa(limit))
		limit = sinkPage
		page, cursor, err := c.sinkPage(ctx, t, q)
		if err != nil {
			c.rl.warn("sink-logs", "cannot read a log from the sink; reading the node", key, value, "err", err)
			return nil, false
		}
		for _, l := range page {
			bytes += min(len(l.Text), answerLog.line)
			if !l.Partial {
				ended++
			}
		}
		pages = append(pages, page)
		if cursor == "" || len(page) == 0 {
			break
		}
		q.Set("cursor", cursor)
	}
	if len(pages) == 0 || len(pages[0]) == 0 {
		return nil, false
	}
	// Oldest first, a line's partial parts joined.
	tail := logTail{b: answerLog, max: lines}
	for i := len(pages) - 1; i >= 0; i-- {
		for _, l := range pages[i] {
			tail.addPart(strings.TrimRight(l.Text, "\r\n"), l.Source, l.Partial)
		}
	}
	return tail.result(), true
}

// sinkLine is a line of the sink's answer, as far as sinkLog reads it.
type sinkLine struct {
	Text    string `json:"text"`
	Source  string `json:"source"`
	Partial bool   `json:"partial"`
}

// sinkPage asks the sink for one page: its lines, oldest first, and the
// cursor to the older page ("": none).
func (c *Controller) sinkPage(ctx context.Context, t auth.Tenant, q url.Values) ([]sinkLine, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.d.SinkURL, "/")+"/api/v1/logs?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	t.PassOn(req.Header)
	resp, err := c.d.SinkClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", fmt.Errorf("the sink answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var page struct {
		Lines  []sinkLine `json:"lines"`
		Cursor string     `json:"cursor"`
	}
	// sinkPage lines of the sink's longest part (64 KiB), twice for their
	// escapes and fields.
	if err := json.NewDecoder(io.LimitReader(resp.Body, (2*sinkPage)<<16)).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("the sink's answer: %w", err)
	}
	return page.Lines, page.Cursor, nil
}
