// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/apierr"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

type eventsAnswer struct {
	Events []map[string]any `json:"events"`
}

// kindsOf lists the events' kinds, or eventIds.
func fieldOf(evs []map[string]any, key string) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = fmt.Sprint(ev[key])
	}
	return out
}

// The activity of a sandbox: its rows and its traffic records merged by
// time, ascending, the newest page; the gaps of the node streams it shows;
// stable event ids; the source filter.
func TestSandboxActivity(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	sb := podid.Mint()
	start := uuid.New()
	base := time.Now().Add(-time.Minute).UnixNano()
	exit, dur := int64(3), int64(1500)
	rows := []store.ActivityRow{
		{TsNs: base + 1, SandboxID: sb, PodID: sb, TenantID: testTenant, Node: "n1", StartID: start, Seq: 1,
			Source: store.SourceFeosd, Kind: "pod_state", Category: "lifecycle", Name: "pod RUNNING ()",
			Detail: json.RawMessage(`{"state":"RUNNING"}`)},
		{TsNs: base + 2, SandboxID: sb, TenantID: testTenant, InstanceID: "i1", Source: store.SourceFerund,
			Kind: "tool_start", Category: "tool", SpanID: "op-1", ParentSpanID: "run-1", Tool: "bash",
			Params: json.RawMessage(`{"command":"ls"}`)},
		{TsNs: base + 5, SandboxID: sb, TenantID: testTenant, InstanceID: "i1", Source: store.SourceFerund,
			Kind: "tool_end", Category: "tool", SpanID: "op-1", ParentSpanID: "run-1", Tool: "bash", ExitCode: &exit,
			DurationNs: &dur, Output: "out"},
		{TsNs: base + 6, SandboxID: sb, PodID: sb, TenantID: testTenant, Node: "n1", StartID: start, Seq: 6,
			Source: store.SourceAuthn, Kind: "kube_request", Category: "credential", Name: "GET /api 200"},
	}
	if err := e.st.InsertActivity(ctx, rows...); err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertTraffic(ctx,
		store.TrafficRow{TsNs: base + 3, Node: "n1", StartID: start, Seq: 3, PodID: sb, SandboxID: sb, TenantID: testTenant,
			Kind: "flow", Detail: json.RawMessage(`{"flow":{"bytes_sent":"10"}}`)},
		store.TrafficRow{TsNs: base + 4, Node: "n1", StartID: start, Seq: 4, PodID: sb, SandboxID: sb, TenantID: testTenant,
			Kind: "dns", Detail: json.RawMessage(`{"dns":{"name":"x."}}`)},
	); err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertEventGap(ctx, "n1", start, 7, 9); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/admin/sandboxes/" + sb.String() + "/activity"

	w := e.do("GET", path, adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	all := decode[eventsAnswer](t, w)
	if got := strings.Join(fieldOf(all.Events, "kind"), ","); got != "pod_state,tool_start,flow,dns,tool_end,kube_request,gap" {
		t.Fatalf("kinds %s", got)
	}
	ids := fieldOf(all.Events, "eventId")
	want := []string{
		fmt.Sprintf("n1:%s:1", start), "ferund:", fmt.Sprintf("n1:%s:3", start), fmt.Sprintf("n1:%s:4", start), "ferund:",
		fmt.Sprintf("n1:%s:6", start), fmt.Sprintf("n1:%s:gap:7-9", start),
	}
	for i := range want {
		if !strings.HasPrefix(ids[i], want[i]) {
			t.Fatalf("eventIds %v", ids)
		}
	}
	if ids[1] == ids[4] {
		t.Fatalf("ferund's event ids collide: %v", ids)
	}
	if got := strings.Join(fieldOf(all.Events, "source"), ","); got != "feosd,ferund,traffic,traffic,ferund,authn,feosd" {
		t.Fatalf("sources %s", got)
	}
	end := all.Events[4]
	if end["exitCode"] != "3" || end["durationNs"] != "1500" || end["v"] != 2.0 || end["output"] != "out" ||
		end["tsNs"] != fmt.Sprint(base+5) || end["detailJson"] != "{}" {
		t.Fatalf("tool_end %+v", end)
	}
	if _, ok := all.Events[0]["exitCode"]; ok {
		t.Fatal("exitCode of an event without one")
	}
	if all.Events[1]["paramsJson"] != `{"command": "ls"}` && all.Events[1]["paramsJson"] != `{"command":"ls"}` {
		t.Fatalf("paramsJson %q", all.Events[1]["paramsJson"])
	}
	flow := all.Events[2]
	if flow["category"] != "network" || flow["v"] != 1.0 || !strings.Contains(fmt.Sprint(flow["detailJson"]), "bytes_sent") {
		t.Fatalf("flow %+v", flow)
	}
	gap := all.Events[6]
	if gap["name"] != "gap in stream: 3 events lost" || gap["node"] != "n1" {
		t.Fatalf("gap %+v", gap)
	}

	for _, c := range []struct {
		query, kinds string
	}{
		{"?source=traffic", "flow,dns,gap"},
		{"?source=ferund", "tool_start,tool_end"},
		{"?source=feosd", "pod_state,gap"},
		{"?source=authn", "kube_request,gap"},
		{"?limit=2", "tool_end,kube_request,gap"},
		{fmt.Sprintf("?beforeTsNs=%d&limit=2", base+4), "tool_start,flow"},
	} {
		w := e.do("GET", path+c.query, adminAuth(), nil)
		expect(t, w, http.StatusOK, "")
		if got := strings.Join(fieldOf(decode[eventsAnswer](t, w).Events, "kind"), ","); got != c.kinds {
			t.Fatalf("%s: kinds %s, want %s", c.query, got, c.kinds)
		}
	}
	for _, c := range []struct{ query, msg string }{
		{"?limit=x", `limit must be an integer, got "x"`},
		{"?beforeTsNs=1.5", `beforeTsNs must be an integer, got "1.5"`},
		{"?source=manager", `source must be ferund, feosd, authn or traffic, got "manager"`},
	} {
		expect(t, e.do("GET", path+c.query, adminAuth(), nil), http.StatusBadRequest, errBody(c.msg))
	}
	expect(t, e.do("GET", "/api/v1/admin/sandboxes/x/activity", adminAuth(), nil), http.StatusBadRequest,
		errBody(apierr.MsgSandboxID))
	// A sandbox with nothing: an empty list, never null.
	expect(t, e.do("GET", "/api/v1/admin/sandboxes/"+podid.Mint().String()+"/activity", adminAuth(), nil), http.StatusOK,
		`{"events":[]}`+"\n")
}

// A page shows the gaps of the node streams of the events it returns, not
// of those its limit cut off: with limit 1 only n1's event is returned,
// so n2's gap is not.
func TestActivityGapsOfThePage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	sb := podid.Mint()
	start1, start2 := uuid.New(), uuid.New()
	base := time.Now().Add(-time.Minute).UnixNano()
	if err := e.st.InsertTraffic(ctx, store.TrafficRow{TsNs: base + 1, Node: "n2", StartID: start2, Seq: 1, PodID: sb,
		SandboxID: sb, TenantID: testTenant, Kind: "flow", Detail: json.RawMessage(`{"flow":{}}`)}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertActivity(ctx, store.ActivityRow{TsNs: base + 2, SandboxID: sb, PodID: sb, TenantID: testTenant,
		Node: "n1", StartID: start1, Seq: 1, Source: store.SourceFeosd, Kind: "pod_state", Category: "lifecycle"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertEventGap(ctx, "n1", start1, 2, 3); err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertEventGap(ctx, "n2", start2, 2, 5); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/admin/sandboxes/" + sb.String() + "/activity"
	for _, c := range []struct {
		query string
		ids   []string
	}{
		{"", []string{fmt.Sprintf("n2:%s:1", start2), fmt.Sprintf("n1:%s:1", start1), "gap", "gap"}},
		{"?limit=1", []string{fmt.Sprintf("n1:%s:1", start1), fmt.Sprintf("n1:%s:gap:2-3", start1)}},
		{"?source=traffic&limit=1", []string{fmt.Sprintf("n2:%s:1", start2), fmt.Sprintf("n2:%s:gap:2-5", start2)}},
	} {
		w := e.do("GET", path+c.query, adminAuth(), nil)
		expect(t, w, http.StatusOK, "")
		ids := fieldOf(decode[eventsAnswer](t, w).Events, "eventId")
		ok := len(ids) == len(c.ids)
		for i := 0; ok && i < len(ids); i++ {
			ok = ids[i] == c.ids[i] || (c.ids[i] == "gap" && strings.Contains(ids[i], ":gap:"))
		}
		if !ok {
			t.Fatalf("%q: eventIds %v, want %v", c.query, ids, c.ids)
		}
	}
}

// The activity of a node: its own rows (a new start among them) and the
// records it sent about pods that are not on it, never a sandbox's.
func TestNodeActivity(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	start := uuid.New()
	pod := podid.Mint()
	base := time.Now().Add(-time.Minute).UnixNano()
	if err := e.st.InsertActivity(ctx,
		store.ActivityRow{TsNs: base + 1, Node: "n1", StartID: start, InstanceID: "i1", Source: store.SourceFerund,
			Kind: "node_start", Name: "feosd started"},
		store.ActivityRow{TsNs: base + 2, Node: "n1", StartID: start, Seq: 2, PodID: pod, ForeignPod: true,
			Source: store.SourceFeosd, Kind: "pod_state"},
		store.ActivityRow{TsNs: base + 3, Node: "n1", StartID: start, Seq: 3, SandboxID: pod, PodID: pod,
			Source: store.SourceFeosd, Kind: "pod_state"},
	); err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertTraffic(ctx, store.TrafficRow{TsNs: base + 4, Node: "n1", StartID: start, Seq: 4, PodID: pod,
		ForeignPod: true, Kind: "drop", Detail: json.RawMessage(`{"drop":{}}`)}); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/api/v1/admin/nodes/n1/activity", adminAuth(), nil)
	expect(t, w, http.StatusOK, "")
	evs := decode[eventsAnswer](t, w).Events
	if got := strings.Join(fieldOf(evs, "kind"), ","); got != "node_start,pod_state,drop" {
		t.Fatalf("kinds %s", got)
	}
	if evs[0]["eventId"] != fmt.Sprintf("n1:%s:start", start) || evs[1]["podId"] != pod.String() {
		t.Fatalf("events %+v", evs)
	}
}

// A tenant reads its sandbox's activity on its own route (ADR 0031): a
// reader of the sandbox's environment, and FeCode's admin token, get its
// rows and traffic without the fleet's part (a placement without the
// other nodes' verdicts, no overbooking); an operator gets them whole;
// another tenant's sandbox is absent to a tenant-wide grant, a sandbox of
// another environment to an environment's reader.
func TestTenantSandboxActivity(t *testing.T) {
	t.Parallel()
	w := newPeopleWorld(t, envOptions{})
	ctx := context.Background()
	id := w.podIn(t, "GET /api/v1/sandboxes/{id}", w.stage)
	sb, err := podid.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := w.e.st.CreateMintedSandbox(ctx, func(id podid.ID) store.NewSandbox {
		return store.NewSandbox{ID: id, Owner: "other", Image: "img", VCPUs: 1, MemoryMiB: 512,
			EnvironmentID: mkEnvRow(t, w.e, "other", "dev")}
	})
	if err != nil {
		t.Fatal(err)
	}
	inProd := w.podIn(t, "GET /api/v1/sandboxes/{id}", w.prod)
	base := time.Now().Add(-time.Minute).UnixNano()
	ferundRow := func(ts int64, of podid.ID, kind, name, detail string) store.ActivityRow {
		return store.ActivityRow{TsNs: base + ts, SandboxID: of, TenantID: testTenant, InstanceID: "i1", Node: "n1",
			Source: store.SourceFerund, Kind: kind, Category: "lifecycle", Name: name, Detail: json.RawMessage(detail)}
	}
	tool := ferundRow(3, sb, "tool_start", "", `{}`)
	tool.Category, tool.Tool, tool.Params = "tool", "bash", json.RawMessage(`{"command":"ls"}`)
	if err := w.e.st.InsertActivity(ctx,
		ferundRow(1, sb, activity.KindPlacement, "placed on n1 (create)", `{"node":"n1","op":"create","trigger":"api",`+
			`"candidates":[{"node":"n1","feasible":true},{"node":"n2","feasible":false,"reason":"memory: 3900 of 4096 MiB booked"}]}`),
		ferundRow(2, sb, activity.KindOverbooked, "node n1 overbooked by sandbox "+id+": memory", `{"node":"n1","gate":"memory"}`),
		tool,
		ferundRow(4, foreign.ID, "tool_start", "the other tenant's", `{}`),
	); err != nil {
		t.Fatal(err)
	}
	if err := w.e.st.InsertTraffic(ctx, store.TrafficRow{TsNs: base + 5, Node: "n1", StartID: uuid.New(), Seq: 1, PodID: sb,
		SandboxID: sb, TenantID: testTenant, Kind: "flow", Detail: json.RawMessage(`{"flow":{"bytes_sent":"10"}}`)}); err != nil {
		t.Fatal(err)
	}

	path := "/api/v1/sandboxes/" + id + "/activity"
	read := func(p principal, path string) []map[string]any {
		t.Helper()
		resp := w.e.serve(p.request("GET", path, nil))
		expect(t, resp, http.StatusOK, "")
		return decode[eventsAnswer](t, resp).Events
	}
	placement := func(evs []map[string]any) map[string]any {
		t.Helper()
		var d map[string]any
		if err := json.Unmarshal([]byte(fmt.Sprint(evs[0]["detailJson"])), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	ps := w.principals(t)
	admin, operator, stageRead := ps[0], ps[4], ps[8]
	for _, p := range []principal{stageRead, admin} {
		evs := read(p, path)
		if got := strings.Join(fieldOf(evs, "kind"), ","); got != "placement,tool_start,flow" {
			t.Fatalf("%s: kinds %s", p.name, got)
		}
		if d := placement(evs); d["node"] != "n1" || d["op"] != "create" || d["candidates"] != nil {
			t.Fatalf("%s: placement %v", p.name, d)
		}
		if evs[1]["paramsJson"] == "" || evs[1]["tool"] != "bash" {
			t.Fatalf("%s: tool %+v", p.name, evs[1])
		}
		if got := strings.Join(fieldOf(read(p, path+"?source=traffic"), "kind"), ","); got != "flow" {
			t.Fatalf("%s: traffic %s", p.name, got)
		}
	}
	evs := read(operator, path)
	if got := strings.Join(fieldOf(evs, "kind"), ","); got != "placement,overbooked,tool_start,flow" {
		t.Fatalf("operator: kinds %s", got)
	}
	if c, _ := placement(evs)["candidates"].([]any); len(c) != 2 {
		t.Fatalf("operator: placement %v", placement(evs))
	}

	expect(t, w.e.serve(admin.request("GET", "/api/v1/sandboxes/"+foreign.ID.String()+"/activity", nil)),
		http.StatusNotFound, errBody("unknown sandbox "+foreign.ID.String()))
	expect(t, w.e.serve(stageRead.request("GET", "/api/v1/sandboxes/"+inProd+"/activity", nil)),
		http.StatusNotFound, errBody("unknown sandbox "+inProd))
	expect(t, w.e.serve(stageRead.request("GET", path+"?source=manager", nil)), http.StatusBadRequest,
		errBody(`source must be ferund, feosd, authn or traffic, got "manager"`))
}
