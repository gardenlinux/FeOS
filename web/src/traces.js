// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The Traces tab's arithmetic (ADR 0026 §10, docs/architecture/traces.md):
// the log store answers a tenant's traces in the shape of Jaeger's HTTP
// query API under /api/v1/jaeger; fromJaeger reads a trace of it into
// spans with their service, name, kind, times in µs, error, attributes,
// resource, events and links, and the rest lays them out as a waterfall.
// Pure, with no React or browser imports, so that the tests load it as it
// is.

import { clockOf, durationText } from './logs.js';

// TRACE_RANGES are the search's times, a span back from now.
export const TRACE_RANGES = [
  ['15m', '15 min'],
  ['1h', '1 h'],
  ['1d', '1 day'],
  ['7d', '7 days'],
];
const SPANS_US = { '15m': 15 * 60e6, '1h': 3600e6, '1d': 86400e6, '7d': 7 * 86400e6 };
export const DEFAULT_TRACE_RANGE = '1h';
export const TRACE_LIMIT = 50;

// traceRangeOf is the range a search's since names.
export const traceRangeOf = (focus) => (focus && SPANS_US[focus.since] ? focus.since : DEFAULT_TRACE_RANGE);

// traceFocusOf reads a search of the recent traces from the address's
// query: the service the traces went through, and the time.
export function traceFocusOf(search) {
  const p = new URLSearchParams(search || '');
  const f = {};
  const svc = (p.get('service') || '').trim();
  if (svc) f.service = svc;
  if (SPANS_US[p.get('since')]) f.since = p.get('since');
  return f;
}

// traceQueryOf is the address's query of such a search, '' or "?…".
export function traceQueryOf(focus) {
  const p = new URLSearchParams();
  if (focus && focus.service) p.set('service', focus.service);
  if (focus && focus.since && focus.since !== DEFAULT_TRACE_RANGE) p.set('since', focus.since);
  const s = p.toString();
  return s ? '?' + s : '';
}

// traceSearchParams is the store's query of an environment's (its ID)
// recent traces: Jaeger's start and end in µs.
export function traceSearchParams(focus, env, now = Date.now()) {
  const p = new URLSearchParams();
  p.set('environment', env);
  if (focus && focus.service) p.set('service', focus.service);
  const end = Math.floor(now * 1000);
  p.set('start', String(end - SPANS_US[traceRangeOf(focus)]));
  p.set('end', String(end));
  p.set('limit', String(TRACE_LIMIT));
  return p;
}

// LIFTED are the tags a span's own fields carry.
const LIFTED = new Set(['span.kind', 'otel.status_code', 'otel.status_description', 'error']);

const tagText = (kv) => (typeof kv.value === 'string' ? kv.value : JSON.stringify(kv.value));

// fromJaeger reads a trace of Jaeger's shape into {traceId, startUs,
// durationUs, spans, warnings}: each span once, oldest first, its parent
// (its first CHILD_OF of the same trace), its other references as links,
// span.kind, the status and error lifted out of its tags, its events with
// their names, and its process's service and tags as its resource.
export function fromJaeger(t) {
  const out = { traceId: t.traceID, startUs: 0, durationUs: 0, spans: [], warnings: t.warnings || [] };
  const seen = new Set();
  let end = 0;
  for (const s of t.spans || []) {
    if (seen.has(s.spanID)) continue;
    seen.add(s.spanID);
    const p = (t.processes || {})[s.processID] || {};
    const x = {
      spanId: s.spanID, parentId: '', service: p.serviceName || '', name: s.operationName || '', kind: '',
      startUs: Number(s.startTime) || 0, durationUs: Number(s.duration) || 0, error: false, status: '', statusMessage: '',
      attributes: [], resource: p.tags || [], events: [], links: [],
    };
    for (const r of s.references || []) {
      if (r.refType === 'CHILD_OF' && !x.parentId && r.traceID === s.traceID) x.parentId = r.spanID;
      else x.links.push({ traceId: r.traceID, spanId: r.spanID });
    }
    for (const kv of s.tags || []) {
      if (kv.key === 'span.kind') x.kind = tagText(kv);
      else if (kv.key === 'otel.status_code') x.status = tagText(kv);
      else if (kv.key === 'otel.status_description') x.statusMessage = tagText(kv);
      else if (kv.key === 'error') x.error = x.error || tagText(kv) === 'true';
      if (!LIFTED.has(kv.key)) x.attributes.push(kv);
    }
    x.error = x.error || x.status === 'ERROR';
    for (const l of s.logs || []) {
      const ev = { timeUs: Number(l.timestamp) || 0, name: '', attributes: [] };
      for (const kv of l.fields || []) {
        if (kv.key === 'event' && !ev.name) ev.name = tagText(kv);
        else ev.attributes.push(kv);
      }
      x.events.push(ev);
    }
    if (out.spans.length === 0 || x.startUs < out.startUs) out.startUs = x.startUs;
    end = Math.max(end, x.startUs + x.durationUs);
    out.spans.push(x);
  }
  out.spans.sort((a, b) => a.startUs - b.startUs || (a.spanId < b.spanId ? -1 : 1));
  if (out.spans.length) out.durationUs = end - out.startUs;
  return out;
}

// summaryOf is a trace of a search in a few facts: its root's service
// and name, the request's path and status, its services, spans and
// failures.
export function summaryOf(t) {
  const v = fromJaeger(t);
  const ids = new Set(v.spans.map((s) => s.spanId));
  const sm = { traceId: v.traceId, startUs: v.startUs, durationUs: v.durationUs, service: '', name: '', path: '', statusCode: 0,
    services: [], spans: v.spans.length, errors: 0 };
  let root = null;
  for (const s of v.spans) {
    if (s.error) sm.errors++;
    if (!sm.services.includes(s.service)) sm.services.push(s.service);
    if (!root && (!s.parentId || !ids.has(s.parentId))) root = s;
  }
  if (root) {
    sm.service = root.service;
    sm.name = root.name;
    for (const kv of root.attributes) {
      if (['url.path', 'http.route', 'http.target'].includes(kv.key) && !sm.path) sm.path = tagText(kv);
      if (['http.response.status_code', 'http.status_code'].includes(kv.key)) sm.statusCode = Number(tagText(kv)) || 0;
    }
  }
  return sm;
}

// summaryText is a trace of a search in a few words: its root's name, the
// request's path and status.
export function summaryText(t) {
  const ask = [t.name, t.path && !String(t.name).includes(t.path) ? t.path : ''].filter(Boolean).join(' ');
  return t.statusCode ? `${ask} → ${t.statusCode}` : ask;
}

// usText is a span of µs as people read it.
export const usText = (us) => durationText((Number(us) || 0) * 1000);

// offsetText is how far into the trace a time is: +12 ms.
export const offsetText = (us, start) => `+${usText(Math.max(0, us - start))}`;

// clockOfUs is a time in µs as a time of day with milliseconds, local or
// UTC.
export function clockOfUs(us, utc = false) {
  const d = new Date(Math.floor(Number(us) / 1000));
  return Number.isNaN(d.getTime()) ? '' : clockOf(d.toISOString(), utc);
}

// spanRows are the trace's spans as the waterfall draws them, top to
// bottom: each root by start, its children under it by start, depth
// counting the levels. A span whose parent the trace lacks (a caller FeRun
// never heard from, or a span not yet arrived) is a root marked orphan.
// Each row also knows what the tree's lines need: its parent in the
// drawing (up), its ancestors from the root (path), whether it is its
// parent's last child (last), how many spans hang below it (below), and
// for each level above it whether a line runs on past it (rails).
export function spanRows(spans) {
  const list = spans || [];
  const byId = new Map(list.map((s) => [s.spanId, s]));
  const kids = new Map();
  const roots = [];
  for (const s of list) {
    if (s.parentId && byId.has(s.parentId) && s.parentId !== s.spanId) {
      if (!kids.has(s.parentId)) kids.set(s.parentId, []);
      kids.get(s.parentId).push(s);
    } else roots.push(s);
  }
  const byStart = (a, b) => a.startUs - b.startUs || (a.spanId < b.spanId ? -1 : 1);
  const out = [];
  const seen = new Set();
  // A child of a cycle's span is no child here once its span was drawn.
  const walk = (s, depth, path, rails, last) => {
    seen.add(s.spanId);
    const children = (kids.get(s.spanId) || []).filter((c) => !seen.has(c.spanId)).sort(byStart);
    for (const c of children) seen.add(c.spanId);
    const row = { span: s, depth, children: children.length, orphan: depth === 0 && !!s.parentId, up: path[path.length - 1] || '',
      path, last, rails, below: 0 };
    out.push(row);
    const at = out.length;
    children.forEach((c, i) => walk(c, depth + 1, [...path, s.spanId], depth === 0 ? [] : [...rails, !last], i === children.length - 1));
    row.below = out.length - at;
  };
  const top = roots.sort(byStart);
  for (const r of top) seen.add(r.spanId);
  top.forEach((r) => walk(r, 0, [], [], true));
  // Spans of a cycle are reached by no root; they still show.
  for (const s of [...list].sort(byStart)) if (!seen.has(s.spanId)) walk(s, 0, [], [], true);
  return out;
}

// shownRows are the rows a reader sees with the spans of folded hiding
// what hangs below them.
export function shownRows(rows, folded) {
  const out = [];
  let hideBelow = Infinity;
  for (const r of rows) {
    if (r.depth > hideBelow) continue;
    hideBelow = folded.has(r.span.spanId) && r.children > 0 ? r.depth : Infinity;
    out.push(r);
  }
  return out;
}

// FOLD_BUDGET is about how many rows a trace opens with.
export const FOLD_BUDGET = 24;

// firstFolds are the spans a trace opens folded: every failed span shows,
// then the longest spans open first while the rows stay within budget.
// A trace smaller than the budget opens whole.
export function firstFolds(rows, budget = FOLD_BUDGET) {
  const folded = new Set(rows.filter((r) => r.children > 0).map((r) => r.span.spanId));
  const byId = new Map(rows.map((r) => [r.span.spanId, r]));
  let shown = rows.filter((r) => r.depth === 0).length;
  const open = (r) => {
    if (!folded.delete(r.span.spanId)) return;
    shown += r.children;
  };
  for (const r of rows) if (r.span.error) for (const id of r.path) open(byId.get(id));
  for (;;) {
    const next = shownRows(rows, folded)
      .filter((r) => folded.has(r.span.spanId) && shown + r.children <= budget)
      .sort((a, b) => b.span.durationUs - a.span.durationUs)[0];
    if (!next) return folded;
    open(next);
  }
}

// foldLevel folds or opens one level: more opens every folded span shown
// at the shallowest such depth, less folds every open span at the deepest.
export function foldLevel(rows, folded, more) {
  const shown = shownRows(rows, folded).filter((r) => r.children > 0);
  const next = new Set(folded);
  if (more) {
    const at = shown.filter((r) => folded.has(r.span.spanId));
    const depth = Math.min(...at.map((r) => r.depth));
    for (const r of at) if (r.depth === depth) next.delete(r.span.spanId);
  } else {
    const at = shown.filter((r) => !folded.has(r.span.spanId));
    const depth = Math.max(...at.map((r) => r.depth));
    for (const r of at) if (r.depth === depth) next.add(r.span.spanId);
  }
  return next;
}

// within is the part of a span inside its parent, [start, end) in µs, or
// null: a child that starts after its parent ends ran on its own (a job
// the request left behind), and a clock that runs ahead puts a child
// before its caller.
function within(s, p) {
  const start = Math.max(s.startUs, p.startUs);
  const end = Math.min(s.startUs + s.durationUs, p.startUs + p.durationUs);
  return end > start || (end === start && s.durationUs === 0) ? { start, end } : null;
}

// criticalPath is the time each span alone held the trace up, as Jaeger
// reckons it: from a root's end back, the child that finished last before
// that moment holds it, down its own children, and then the time before
// that child's start is its parent's again. A map of span ID to sections,
// [start, end] in µs; a span off the path has none.
export function criticalPath(rows) {
  const out = new Map();
  const kids = new Map();
  for (const r of rows) {
    if (!r.up) continue;
    if (!kids.has(r.up)) kids.set(r.up, []);
    kids.get(r.up).push(r.span);
  }
  const add = (id, start, end) => {
    if (end <= start) return;
    if (!out.has(id)) out.set(id, []);
    out.get(id).push([start, end]);
  };
  const walk = (s, start, end) => {
    const parts = (kids.get(s.spanId) || [])
      .map((c) => ({ c, in: within(c, { startUs: start, durationUs: end - start }) }))
      .filter((k) => k.in)
      .sort((a, b) => b.in.end - a.in.end);
    let t = end;
    for (const k of parts) {
      if (k.in.end > t) continue; // ran beside the one that held it
      add(s.spanId, k.in.end, t);
      walk(k.c, k.in.start, k.in.end);
      t = k.in.start;
    }
    add(s.spanId, start, t);
  };
  for (const r of rows) if (r.depth === 0) walk(r.span, r.span.startUs, r.span.startUs + r.span.durationUs);
  for (const v of out.values()) v.sort((a, b) => a[0] - b[0]);
  return out;
}

// criticalByService is how long each service held the trace up, longest
// first: [service, µs].
export function criticalByService(rows, path) {
  const sum = new Map();
  for (const r of rows) {
    const us = (path.get(r.span.spanId) || []).reduce((n, [a, b]) => n + b - a, 0);
    if (us > 0) sum.set(r.span.service, (sum.get(r.span.service) || 0) + us);
  }
  return [...sum].sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1));
}

// selfUs is the time a span spent in no child of its own.
export function selfUs(span, children) {
  const parts = (children || []).map((c) => within(c, span)).filter(Boolean).sort((a, b) => a.start - b.start);
  let busy = 0;
  let from = 0;
  let to = -Infinity;
  for (const p of parts) {
    if (p.start > to) {
      if (to > from) busy += to - from;
      from = p.start;
      to = p.end;
    } else to = Math.max(to, p.end);
  }
  if (to > from) busy += to - from;
  return Math.max(0, span.durationUs - busy);
}

// waitParts are the parts of a span spent waiting, [start, end) in µs,
// merged: the time its own spans cover, and the whole of a call (a
// request out, a message sent) without a span of its own below it, which
// waits on something the trace does not see.
export function waitParts(span, children) {
  const parts = (children || []).map((c) => within(c, span)).filter(Boolean).sort((a, b) => a.start - b.start);
  if (!parts.length) return isCall(span) && span.durationUs > 0 ? [[span.startUs, span.startUs + span.durationUs]] : [];
  const out = [];
  for (const p of parts) {
    const last = out[out.length - 1];
    if (last && p.start <= last[1]) last[1] = Math.max(last[1], p.end);
    else out.push([p.start, p.end]);
  }
  return out;
}

// SERVICE_HUES is how many services get a hue of their own; the rest are
// grey.
export const SERVICE_HUES = 7;

// serviceHues gives each service its hue, 1 to SERVICE_HUES, 0 for grey:
// the environment's services by name first, so that a service keeps its
// hue from trace to trace, then the trace's others as they come.
export function serviceHues(envServices, spans) {
  const hues = new Map();
  const names = [...new Set(envServices || [])].sort();
  for (const s of spans || []) if (!names.includes(s.service)) names.push(s.service);
  names.forEach((n, i) => hues.set(n, i < SERVICE_HUES ? i + 1 : 0));
  return hues;
}

// isCall tells a span that waits on another: a request out, or a message
// sent.
export const isCall = (span) => span.kind === 'client' || span.kind === 'producer';

// attrValue is an attribute's value as text.
export function attrValue(a) {
  const v = a && a.value;
  if (typeof v === 'string') return v;
  if (v === undefined) return '';
  return JSON.stringify(v);
}

// attrOf is the value of an attribute key in a list, undefined without.
export function attrOf(list, key) {
  const a = (list || []).find((x) => x.key === key);
  return a ? a.value : undefined;
}

// bindingOf is the HTTP binding a span went out through (ADR 0028 §6: the
// platform's client span of a request through a binding carries
// ferun.binding), '' for another span.
export function bindingOf(span) {
  if (!span || span.kind !== 'client') return '';
  const b = attrOf(span.attributes, 'ferun.binding');
  return typeof b === 'string' ? b : '';
}

// barOf places a span's bar in the trace: left and width in percent of
// the trace's duration, at least a sliver wide so that a short span shows.
export function barOf(span, trace) {
  const total = Math.max(1, trace.durationUs || 0);
  const left = Math.min(100, Math.max(0, ((span.startUs - trace.startUs) / total) * 100));
  const width = Math.min(100 - left, Math.max(0.4, (span.durationUs / total) * 100));
  return { left, width };
}

// whereOf names where a span ran: object#replica from its resource's
// FeRun labels, else its service.
export function whereOf(span) {
  const obj = attrOf(span.resource, 'ferun.object');
  const replica = attrOf(span.resource, 'ferun.replica');
  if (attrOf(span.resource, 'ferun.source') === 'ingress') return span.service;
  if (obj && replica != null) return `${obj}#${replica}`;
  return span.service || obj || 'unknown service';
}

// spanTitle is a span's name, with the request's path where the name is
// the method alone (the ingress's span: POST).
export function spanTitle(span) {
  const path = attrOf(span.attributes, 'url.path');
  return typeof path === 'string' && !String(span.name).includes('/') ? `${span.name} ${path}` : span.name;
}
