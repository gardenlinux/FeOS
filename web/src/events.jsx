// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React from 'react';
import { fmtDurNs, shortId } from './format.js';
import { isTraffic, trafficLabel, trafficSummary } from './traffic.js';

// The event model of the activity views, in two layers:
//
//   RAW    one object per event of GET …/activity, untouched, shown one to
//          one in the event table and its expansion. The only things
//          derived here are the lane (the event's category) and the level
//          (error when the event itself reports a failure).
//
//   SPANS  deriveRows() builds the span tree from the envelope alone
//          (spanId, parentSpanId, durationNs, name), only for the timeline;
//          it never hides or merges events of the table.
//
// The events are of two kinds. ferund's own rows (v 2) are spans: a run
// of the sandbox on a node, the ops inside it (lifecycle_start and
// lifecycle_end), the tool calls (tool_start, tool_end), and point markers
// (a state change, a placement, a tombstone). The records of nodes (v 1:
// feosd's pod states and node events, feos-authn's audit records, the
// traffic records, and the gaps of their streams) carry no span: each is a
// point. Every row but a traffic record has a name, which the views show
// verbatim; a traffic record is labelled from its body.

// The lanes are the events' categories, in this order; a category not
// listed gets a lane of its own after them.
export const LANES = [
  { key: 'lifecycle', label: 'lifecycle' },
  { key: 'tool', label: 'tool calls' },
  { key: 'network', label: 'network' },
  { key: 'credential', label: 'certificates' },
];

// laneOf is the event's lane: its category ('' reads as lifecycle).
export function laneOf(ev) {
  return ev.category || 'lifecycle';
}

// lanesOf returns the lanes the events use, the known ones first.
export function lanesOf(events) {
  const seen = new Set();
  for (const ev of events || []) seen.add(laneOf(ev));
  const known = LANES.filter((l) => seen.has(l.key));
  const extra = [...seen].filter((k) => !LANES.some((l) => l.key === k)).sort()
    .map((k) => ({ key: k, label: k }));
  return [...known, ...extra];
}

// The labels of the kinds, for events without a name.
export const KIND_LABEL = {
  // ferund
  run_start: 'run started',
  run_end: 'run ended',
  lifecycle_start: 'operation started',
  lifecycle_end: 'operation finished',
  tool_start: 'tool call started',
  tool_end: 'tool call finished',
  sandbox_state: 'sandbox state changed',
  placement: 'placed on a node',
  overbooked: 'node overbooked',
  tombstone: 'pod tombstoned',
  pod_credentials: 'pod certificate issued',
  node_cert: 'node certificate issued',
  node_start: 'feosd started',
  node_removed: 'node removed',
  project_created: 'project created',
  project_account_moved: 'project moved to another account',
  environment_protected: 'environment protected',
  environment_unprotected: 'environment protection lifted',
  integration_registered: 'integration registered',
  integration_changed: 'integration changed',
  integration_disabled: 'integration disabled',
  integration_enabled: 'integration enabled',
  integration_removed: 'integration removed',
  account_created: 'account created',
  account_changed: 'account changed',
  account_closed: 'account closed',
  account_reopened: 'account reopened',
  account_deleted: 'account deleted',
  account_member_set: 'account member set',
  account_member_removed: 'account member removed',
  sign_in: 'signed in',
  // feosd
  pod_state: 'pod state changed',
  node_started: 'feosd started',
  node_stopping: 'feosd stopping',
  node_event: 'node event',
  gap: 'gap in stream',
  // feos-authn
  kube_request: 'Kubernetes request',
  kube_deny: 'Kubernetes request denied',
  tls_connection: 'TLS connection',
  tls_signature: 'TLS signature',
  ssh_signature: 'SSH signature',
  ssh_deny: 'SSH signature denied',
  handshake_report: 'handshake report',
};

export function detailOf(ev) {
  try {
    const d = ev && ev.detailJson ? JSON.parse(ev.detailJson) : {};
    return d && typeof d === 'object' ? d : {};
  } catch {
    return {};
  }
}

// kindLabel names an event by its kind: a traffic record from its body,
// a known kind from KIND_LABEL, any other by its kind token.
export function kindLabel(ev) {
  if (isTraffic(ev)) return trafficLabel(ev);
  return KIND_LABEL[ev.kind] || ev.kind || 'event';
}

// eventTitle is the event column's text: the producer's name, verbatim,
// or the kind's label.
export function eventTitle(ev) {
  return (ev && ev.name) || kindLabel(ev);
}

export function msOf(ev) {
  return Number(ev.tsNs) / 1e6;
}

export function exitOf(ev) {
  if (ev.exitCode === undefined || ev.exitCode === null) return null;
  return Number(ev.exitCode);
}

// levelOf is an event's level: error when the event reports a failure (a
// tool's exit code other than 0, an error in its detail, a failed pod),
// warn when it reports a loss or a refusal (a gap, a drop, a denial, an
// overbooked node), info otherwise.
export function levelOf(ev) {
  const ex = exitOf(ev);
  if (ex !== null && ex !== 0) return 'error';
  const d = detailOf(ev);
  if (d.error) return 'error';
  if (ev.kind === 'pod_state' && d.state === 'POD_STATE_FAILED') return 'error';
  if (ev.kind === 'gap' || ev.kind === 'overbooked' || ev.kind === 'kube_deny' || ev.kind === 'ssh_deny') return 'warn';
  if (isTraffic(ev) && ev.kind === 'drop') return 'warn';
  return 'info';
}

// keyOf is the event's identity, unique and stable across reads (ferund's
// eventId).
export function keyOf(ev) {
  return ev.eventId;
}

const short = (id) => (id ? shortId(String(id)) : '');

// eventDescription is the description column of one raw event.
export function eventDescription(ev) {
  const d = detailOf(ev);
  if (isTraffic(ev)) return <span className="mono">{trafficSummary(ev)}</span>;
  switch (ev.kind) {
    case 'tool_start':
      return <span className="mono tool-name">{ev.tool}</span>;
    case 'tool_end': {
      const ex = exitOf(ev);
      return <>
        <span className="mono tool-name">{ev.tool}</span>
        {ex !== null && <span className={'exit' + (ex === 0 ? '' : ' bad')}> exit {ex}</span>}
        {ev.durationNs != null && <span className="muted"> {fmtDurNs(ev.durationNs)}</span>}
        {ev.outputTruncated && <span className="muted"> (output truncated)</span>}
        {d.error && <span className="errtext"> {d.error}</span>}
      </>;
    }
    case 'pod_state':
      return <>
        {ev.podId && <span className="muted mono">pod {short(ev.podId)}</span>}
        {d.reason && <span className="muted"> {d.reason}</span>}
      </>;
    case 'gap':
      return <span className="muted">records {d.from}–{d.to} of start {short(d.startId)} never arrived</span>;
    case 'node_start':
      return <span className="muted mono">start {short(d.start_id)}</span>;
    default:
      return <>
        {ev.durationNs != null && <span className="muted">{fmtDurNs(ev.durationNs)}</span>}
        {d.error && <span className="errtext"> {d.error}</span>}
        {!d.error && d.reason && <span className="muted"> {String(d.reason)}</span>}
        {!d.error && !d.reason && d.description && <span className="muted"> {String(d.description)}</span>}
      </>;
  }
}

// ------------------------------------------------------------- the spans --
//
// Row (and nested sub-row) shape:
//   { id, depth, lane, kind, title, bar, startMs, endMs, ownStartMs,
//     ownEndMs, state, open, err, tool, members, subs, start, end, ev,
//     pick, spanId, parentSpanId }
//   bar false: a point (endMs null), a marker or an open span that never
//     got an extent (a lone opener), drawn hollow.
//   open true: none of the span's own members closed it. Nothing is
//     extended to "now".
//   subs: every descendant of the row in the row's lane, flattened, with
//     its depth (1 child, 2 grandchild, …).
//
// The envelope decides everything: an event without spanId is a point;
// one with spanId and no durationNs opens its span; one with durationNs
// closes it (a single such event is a closed span of its own). Its
// extent ends at the closer's time and starts durationNs before it.
// requestId groups nothing: it is shown, not used.

const NS = 1e6;

const startMsOf = (ev) =>
  (Number(ev.tsNs) - (ev.durationNs != null ? Number(ev.durationNs) : 0)) / NS;
const endMsOf = (ev) => Number(ev.tsNs) / NS;
const anyErr = (members) => members.some((m) => levelOf(m) === 'error');
const isCloser = (ev) => ev.durationNs != null && ev.durationNs !== '';

// extentOf is the interval the members cover, and the latest of them.
function extentOf(members) {
  let startMs = Infinity;
  let maxEnd = -Infinity;
  let last = members[0];
  for (const m of members) {
    startMs = Math.min(startMs, startMsOf(m));
    const e = endMsOf(m);
    if (e >= maxEnd) { maxEnd = e; last = m; }
  }
  return { startMs, maxEnd, last };
}

const markerRow = (ev, depth) => ({
  id: keyOf(ev), depth, lane: laneOf(ev), kind: ev.kind, title: eventTitle(ev), bar: false,
  startMs: msOf(ev), endMs: null, state: 'closed', err: levelOf(ev) === 'error',
  members: [ev], subs: [], ev, pick: ev, spanId: '', parentSpanId: ev.parentSpanId || '',
});

const spanNode = (id) => ({ id, members: [], markers: [], kids: [], parentId: '', parent: null });

// reaches reports whether node is from or one of its ancestors; it guards
// against a malformed parent chain (a cycle would hang the render).
function reaches(from, node) {
  for (let n = from, i = 0; n && i < 64; n = n.parent, i += 1) if (n === node) return true;
  return false;
}

// buildForest groups the events into spans and links the spans into a
// forest; markers are attached to their parent span when it is present.
function buildForest(evs) {
  const spans = new Map();
  const markers = [];
  for (const ev of evs) {
    if (!ev.spanId) { markers.push(ev); continue; }
    let s = spans.get(ev.spanId);
    if (!s) spans.set(ev.spanId, (s = spanNode(ev.spanId)));
    s.members.push(ev);
    if (!s.parentId && ev.parentSpanId && ev.parentSpanId !== ev.spanId) s.parentId = ev.parentSpanId;
  }
  for (const s of spans.values()) s.members.sort((a, b) => Number(a.tsNs) - Number(b.tsNs));
  // A parent whose span has no rows here leaves the child a root; the
  // pointer stays on the row, for display.
  for (const s of spans.values()) {
    const p = spans.get(s.parentId);
    if (p && p !== s && !reaches(p, s)) { s.parent = p; p.kids.push(s); }
  }
  const loose = [];
  for (const ev of markers) {
    const p = ev.parentSpanId ? spans.get(ev.parentSpanId) : null;
    if (p) p.markers.push(ev); else loose.push(ev);
  }
  return { spans, loose };
}

// measure computes a subtree bottom-up: a span is closed when one of its
// own members closes it (children never close a parent); its extent is
// that of its own members and of all its child spans, so that a parent
// always contains its children; it is a bar when that extent is an
// interval. Markers inside a span are points in it and never stretch it.
function measure(s) {
  for (const k of s.kids) measure(k);
  const own = extentOf(s.members);
  s.lane = laneOf(s.members[0]);
  s.closed = s.members.some(isCloser);
  s.ownStartMs = own.startMs;
  s.ownEndMs = own.maxEnd;
  let startMs = own.startMs;
  let endMs = own.maxEnd;
  for (const k of s.kids) {
    startMs = Math.min(startMs, k.startMs);
    endMs = Math.max(endMs, k.endMs);
  }
  s.startMs = startMs;
  s.endMs = endMs;
  s.bar = s.closed || endMs > startMs;
}

function spanRow(s, depth, subs) {
  const first = s.members[0];
  const last = s.members[s.members.length - 1];
  return {
    id: `span:${s.id}`, depth, lane: s.lane, kind: first.kind, title: eventTitle(first),
    tool: (s.members.find((m) => m.tool) || {}).tool,
    bar: s.bar, startMs: s.startMs, endMs: s.bar ? s.endMs : null,
    ownStartMs: s.ownStartMs, ownEndMs: s.ownEndMs,
    state: s.closed ? 'closed' : 'open', open: !s.closed,
    err: anyErr(s.members), members: s.members, subs,
    start: first, end: last, ev: first, pick: s.bar ? last : first,
    spanId: s.id, parentSpanId: s.parentId,
  };
}

// collectSubs flattens a span's subtree within its lane. The lanes are the
// categories, so nesting happens within a lane: a child of another
// category (a tool call under a run) becomes a root row in its own lane,
// keeping its parentSpanId for display, while the parent's extent still
// covers it.
function collectSubs(s, lane, depth, subs, rows, queue) {
  for (const k of s.kids) {
    if (k.lane !== lane) { queue.push(k); continue; }
    subs.push(spanRow(k, depth, []));
    collectSubs(k, lane, depth + 1, subs, rows, queue);
  }
  for (const m of s.markers) {
    if (laneOf(m) !== lane) { rows.push(markerRow(m, 0)); continue; }
    subs.push(markerRow(m, depth));
  }
}

// deriveRows is the timeline's model of one page of events: one row per
// root span (and per span promoted to its own lane, and per loose point),
// each with its subtree, sorted by start. Nothing is kept between calls.
export function deriveRows(events) {
  const evs = events || [];
  if (!evs.length) return [];
  const { spans, loose } = buildForest(evs);
  const rows = loose.map((ev) => markerRow(ev, 0));
  const queue = [];
  for (const s of spans.values()) if (!s.parent) { measure(s); queue.push(s); }
  while (queue.length) {
    const s = queue.shift();
    const subs = [];
    collectSubs(s, s.lane, 1, subs, rows, queue);
    subs.sort((a, b) => a.startMs - b.startMs);
    rows.push(spanRow(s, 0, subs));
  }
  rows.sort((a, b) => a.startMs - b.startMs);
  return rows;
}
