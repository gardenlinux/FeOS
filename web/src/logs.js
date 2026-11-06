// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The Logs tab's arithmetic: a search of the log store, ferun-sink (ADR
// 0026, docs/architecture/sink.md, Reading), and how its lines read. Pure,
// with no React or browser imports, so that the tests load it as it is.
//
// A search is its focus: what it reads (object "<kind>/<name>" and its
// replica, from the path; source), the words or /an expression/ (q), the
// levels, the time (since, until; a span back from now or from and to) and
// a trace. Everything but the object and the replica is in the address's
// query, so that a search is a link.

// LEVELS are the store's levels, the chips of the search.
export const LEVELS = ['error', 'warn', 'info', 'debug', 'unknown'];

// SOURCES are what the store keeps of an environment: the containers'
// output, the requests the ingress answered for a service (ADR 0026 §8)
// and the requests pods sent out through their HTTP bindings (ADR 0028
// §6). A search without one reads them all, so that a request, what it
// caused and what it called stand side by side.
export const SOURCES = [
  ['', 'all'],
  ['container', 'output'],
  ['ingress', 'requests'],
  ['egress', 'outbound'],
];

// RANGES are the search's times: a span back from now, or from and to.
export const RANGES = [
  ['15m', '15 min'],
  ['1h', '1 h'],
  ['1d', '1 day'],
  ['7d', '7 days'],
  ['custom', 'from … to'],
];
const SPANS = { '15m': 15 * 60e3, '1h': 3600e3, '1d': 86400e3, '7d': 7 * 86400e3 };
export const DEFAULT_RANGE = '1h';
// A trace's lines are read as far back as the store keeps them (its
// default with a trace), unless a range is picked.
export const TRACE_RANGES = [['all', 'all kept'], ...RANGES];

export const defaultRange = (focus) => (focus && focus.trace ? 'all' : DEFAULT_RANGE);
export const rangesOf = (focus) => (focus && focus.trace ? TRACE_RANGES : RANGES);

// rangeOf is the range a search's since names: a span, or custom for a
// time.
export function rangeOf(focus) {
  const s = (focus && focus.since) || '';
  if (!s) return defaultRange(focus);
  return SPANS[s] || s === 'all' ? s : 'custom';
}

// QUERY_KEYS are the parts of a search the address's query holds.
const QUERY_KEYS = ['source', 'q', 'level', 'since', 'until', 'trace'];

// focusOf reads a search from the route's object and replica and the
// address's query.
export function focusOf(item, replica, search) {
  const p = new URLSearchParams(search || '');
  const f = {};
  if (item) f.object = item;
  if (item && replica != null) f.replica = replica;
  for (const k of QUERY_KEYS) {
    const v = (p.get(k) || '').trim();
    if (v) f[k] = v;
  }
  if (f.source && !SOURCES.some(([s]) => s === f.source)) delete f.source;
  if (f.trace && !isTrace(f.trace)) delete f.trace;
  return f;
}

// queryOf is the address's query of a search: its parts but the object
// and the replica, '' or "?…".
export function queryOf(focus) {
  const p = new URLSearchParams();
  for (const k of QUERY_KEYS) if (focus && focus[k]) p.set(k, focus[k]);
  const s = p.toString();
  return s ? '?' + s : '';
}

// parseSearch reads the text box: /…/ is an RE2 expression, anything else
// words that all appear (-word: must not).
export function parseSearch(text) {
  const t = String(text || '').trim();
  const m = /^\/(.+)\/$/.exec(t);
  if (m) return { q: '', re: m[1] };
  return { q: t, re: '' };
}

// levelsOf reads a comma list of levels, the known ones only.
export const levelsOf = (s) =>
  String(s || '').split(',').map((x) => x.trim()).filter((x) => LEVELS.includes(x));

// toggleLevel turns a level on or off in a comma list, in LEVELS' order.
export function toggleLevel(list, level) {
  const on = new Set(levelsOf(list));
  if (on.has(level)) on.delete(level);
  else on.add(level);
  return LEVELS.filter((l) => on.has(l)).join(',');
}

// sinkParams is the store's query of a search of environment env (its
// ID): the selector, the filters and the time, a span read back from now.
// A replica is a container's, so a replica without a source reads the
// containers only (a service's requests have no replica).
export function sinkParams(focus, env, { now = Date.now(), limit } = {}) {
  const f = focus || {};
  const p = new URLSearchParams();
  p.set('environment', env);
  if (f.object) {
    const i = f.object.indexOf('/');
    p.set('kind', f.object.slice(0, i));
    p.set('object', f.object.slice(i + 1));
    if (f.replica != null) p.set('replica', String(f.replica));
  }
  const source = f.source || (f.object && f.replica != null ? 'container' : '');
  if (source) p.set('source', source);
  if (f.trace) p.set('trace', f.trace);
  const { q, re } = parseSearch(f.q);
  if (q) p.set('q', q);
  if (re) p.set('re', re);
  const levels = levelsOf(f.level);
  if (levels.length) p.set('level', levels.join(','));
  const range = rangeOf(f);
  if (range === 'custom') {
    p.set('since', f.since);
    if (f.until) p.set('until', f.until);
  } else if (range !== 'all') {
    p.set('since', new Date(now - SPANS[range]).toISOString());
  }
  if (limit) p.set('limit', String(limit));
  return p;
}

// reachesNow tells a search that reaches to now, so new lines belong to
// it.
export const reachesNow = (focus) => rangeOf(focus) !== 'custom' || !(focus && focus.until);

// follows tells a search the store follows live: of one object, to now. A
// whole environment is searched, not followed (sink.md, follow).
export const follows = (focus) => reachesNow(focus) && !!(focus && focus.object);

// lineKey names a line across pages: its stream and offset.
export const lineKey = (l, i) => (l.stream ? `${l.stream}:${l.offset}` : `n:${i}`);

// levelOf is the level a line's text says: a level field, else a word at
// its start.
export function levelOf(text) {
  const t = String(text || '');
  const m = /"?(?:level|lvl|severity)"?\s*[=:]\s*"?([a-zA-Z]+)/i.exec(t)
    || /^\W{0,3}(?:\[\s*)?(TRACE|DEBUG|INFO|WARN(?:ING)?|ERROR|ERR|FATAL|PANIC)\b/i.exec(t.slice(0, 80));
  const l = ((m && m[1]) || '').toLowerCase();
  if (/^(error|err|fatal|panic|crit)/.test(l)) return 'error';
  if (/^warn/.test(l)) return 'warn';
  if (/^debug|^trace/.test(l)) return 'debug';
  if (/^info/.test(l)) return 'info';
  return 'unknown';
}

// lineLevel is the store's level of a line, else what its text says.
export const lineLevel = (l) => l.level || levelOf(l.text);

// mergeLines puts a page's lines into those there, oldest first, each
// once: earlier ones in front, newer ones behind.
export function mergeLines(have, page, where) {
  const seen = new Set(have.map((l) => l.key));
  const add = page.filter((l) => !seen.has(l.key));
  return where === 'before' ? [...add, ...have] : [...have, ...add];
}

// keyed gives a page's lines their keys, a JSON line its object (rec), a
// line with a trace ID that ID, and a record (JSON or logfmt) its message
// and fields (record), read once.
export const keyed = (lines) =>
  (lines || []).map((l, i) => {
    const rec = jsonOf(l.text);
    const pairs = rec ? null : logfmtOf(l.text);
    const trace = traceOf(rec || pairsObject(pairs));
    const record = recordOf(l.text, rec, pairs, { time: l.time, level: lineLevel(l), trace });
    return { ...l, key: lineKey(l, i), rec, trace, record };
  });

// jsonOf is a line's text as a JSON object, null for any other text.
export function jsonOf(text) {
  const t = String(text || '');
  if (!/^\s*\{/.test(t)) return null;
  try {
    const o = JSON.parse(t);
    return o && typeof o === 'object' && !Array.isArray(o) ? o : null;
  } catch {
    return null;
  }
}

// isTrace tells a trace ID as the store keeps it: 32 lowercase hex, not
// all zero.
export const isTrace = (v) => typeof v === 'string' && /^[0-9a-f]{32}$/.test(v) && !/^0+$/.test(v);

// traceOf is the trace ID of a record as the store reads it: its trace_id,
// traceId, or traceparent's second field; '' for none.
export function traceOf(rec) {
  if (!rec) return '';
  for (const k of ['trace_id', 'traceId']) if (isTrace(rec[k])) return rec[k];
  const tp = typeof rec.traceparent === 'string' ? rec.traceparent.split('-')[1] : '';
  return isTrace(tp) ? tp : '';
}

// A record is a line of key=value: a JSON object, or logfmt as Go's slog
// and logrus write it. The Logs tab shows its message and then its fields
// as key=value, leaving out what the line's head says already (its time,
// level and trace), and opens a line to every field. The store keeps no
// fields (ADR 0026 §6): it searches the text, so a field's word is text
// the line holds without a space — the pair as the line writes it
// ("status":201, status=201), else a string value in its quotes.

// logfmtOf reads a line of logfmt into its pairs ({key, value, raw}, raw
// the pair as written): two or more key=value separated by spaces, a
// value bare or in double quotes; null for any other line, so text that
// merely holds an = stays text.
export function logfmtOf(text) {
  const s = String(text || '');
  const pairs = [];
  let i = 0;
  while (i < s.length) {
    if (s[i] === ' ') {
      i++;
      continue;
    }
    const eq = s.indexOf('=', i);
    if (eq < 0) return null;
    const key = s.slice(i, eq);
    if (!/^[A-Za-z_][\w.-]*$/.test(key)) return null;
    let j = eq + 1;
    let value;
    if (s[j] === '"') {
      let k = j + 1;
      while (k < s.length && s[k] !== '"') k += s[k] === '\\' ? 2 : 1;
      if (k >= s.length) return null;
      value = unquote(s.slice(j, k + 1));
      j = k + 1;
      if (j < s.length && s[j] !== ' ') return null;
    } else {
      while (j < s.length && s[j] !== ' ') j++;
      value = s.slice(eq + 1, j);
      if (value.includes('"')) return null;
    }
    pairs.push({ key, value, raw: s.slice(i, j) });
    i = j;
  }
  return pairs.length >= 2 ? pairs : null;
}

// unquote is a quoted logfmt value's text: Go's quoting is JSON's but for
// a few escapes, which stay as written.
function unquote(q) {
  try {
    return JSON.parse(q);
  } catch {
    return q.slice(1, -1);
  }
}

const pairsObject = (pairs) => (pairs ? Object.fromEntries(pairs.map((p) => [p.key, p.value])) : null);

const MSG_KEYS = ['msg', 'message'];
const TIME_KEYS = ['time', 'ts', 'timestamp', '@timestamp'];
const LEVEL_KEYS = ['level', 'lvl', 'severity'];
const TRACE_KEYS = ['trace_id', 'traceId', 'traceparent', 'span_id', 'spanId', 'trace_flags', 'traceFlags'];

// SHORT is how much of a value the line shows; the opened line shows all.
const SHORT = 160;

// recordOf reads a record into {msg, fields, shown}: the message (msg or
// message, '' for none), every field ({key, value, text — opened —, short
// — on the line —, word}; a nested object's fields under dotted keys), and
// the fields the line shows: not the message, nor the time, level and
// trace the head shows (head: the line's time, its level, its trace ID).
// rec is the line's JSON object, pairs its logfmt; null when it has
// neither.
export function recordOf(text, rec, pairs, head = {}) {
  const top = rec ? Object.entries(rec).map(([key, value]) => ({ key, value })) : pairs;
  if (!top) return null;
  const fields = [];
  for (const f of top) flatten(f.key, f.value, f.raw, fields, 0);
  for (const f of fields) {
    const t = valueText(f.value);
    f.short = t.length > SHORT ? `${t.slice(0, SHORT - 1)}…` : t;
    // Opened, a string stands alone: no quotes, its line breaks kept.
    f.text = typeof f.value === 'string' && f.value !== '' ? f.value : t;
    // A time finds its own line only.
    f.word = f.top && TIME_KEYS.includes(f.key) ? '' : fieldWord(String(text || ''), f, !!rec);
  }
  const msg = fields.find((f) => f.top && MSG_KEYS.includes(f.key) && typeof f.value === 'string');
  const said = (f) => f.top && (
    (head.time && TIME_KEYS.includes(f.key))
    || (head.level && head.level !== 'unknown' && LEVEL_KEYS.includes(f.key))
    || (head.trace && TRACE_KEYS.includes(f.key)));
  return { msg: msg ? msg.value : '', fields, shown: fields.filter((f) => f !== msg && !said(f)) };
}

// flatten puts a field into out, a nested object as its fields (key.sub),
// four levels deep; arrays stay whole.
function flatten(key, value, raw, out, depth) {
  if (value && typeof value === 'object' && !Array.isArray(value) && depth < 4 && Object.keys(value).length) {
    for (const [k, v] of Object.entries(value)) flatten(`${key}.${k}`, v, undefined, out, depth + 1);
    return;
  }
  out.push({ key, value, raw, top: depth === 0 });
}

// valueText is a value as the line shows it: a string bare, in quotes
// when empty or holding a space; anything else as JSON.
export function valueText(v) {
  if (typeof v === 'string') return v === '' || /\s/.test(v) ? JSON.stringify(v) : v;
  if (v === undefined) return 'null';
  return typeof v === 'object' ? JSON.stringify(v) : String(v);
}

// fieldWord is the search's word for a field's value (see above), ''
// when the line holds none without a space.
function fieldWord(text, f, json) {
  const held = (w) => (w && w.length <= 256 && !/\s/.test(w) && text.includes(w) ? w : '');
  if (!json) return held(f.raw);
  const v = f.value;
  if (!(v === null || ['string', 'number', 'boolean'].includes(typeof v))) return '';
  return (f.top && held(`${JSON.stringify(f.key)}:${JSON.stringify(v)}`))
    || (typeof v === 'string' && v !== '' ? held(JSON.stringify(v)) : '');
}

// wordIn says how a search's text holds a word: 'in' (lines with it),
// 'out' (-word: lines without it) or ''.
export function wordIn(q, word) {
  const words = String(q || '').trim().split(/\s+/);
  if (words.includes(word)) return 'in';
  return words.includes(`-${word}`) ? 'out' : '';
}

// toggleWord puts a word into a search's words — -word with not — or
// takes it out where it is there already; null when the search is an
// expression, which takes no words.
export function toggleWord(q, word, not) {
  const t = String(q || '').trim();
  if (parseSearch(t).re) return null;
  const want = not ? `-${word}` : word;
  const words = t ? t.split(/\s+/) : [];
  const rest = words.filter((w) => w !== word && w !== `-${word}`);
  return (words.includes(want) ? rest : [...rest, want]).join(' ');
}

// durationText is a duration in nanoseconds as people read it.
export function durationText(ns) {
  const n = Number(ns) || 0;
  if (n < 1e6) return `${Math.round(n / 1e3)} µs`;
  if (n < 1e9) return `${Math.round(n / 1e6)} ms`;
  return `${(n / 1e9).toFixed(1)} s`;
}

// isRequest tells a line of a service's requests: a line of an ingress
// stream that is a record (the JSON of ferund's AccessLogRecord).
export const isRequest = (l, st) => !!(st && st.source === 'ingress' && l.rec && l.rec.method);

// requestParts are a request's line in a few words: what was asked, how it
// ended (the status, or aborted, upgraded), and the rest: its duration,
// the attempts beyond the first, and the instance it went to.
export function requestParts(rec) {
  const status = Number(rec.status) || 0;
  const special = rec.outcome === 'aborted' || rec.outcome === 'upgraded';
  const end = special ? (status ? `${status} ${rec.outcome}` : rec.outcome) : String(status || rec.outcome || '?');
  const rest = [durationText(rec.durationNs)];
  if (rec.attempts > 1) rest.push(`${rec.attempts} attempts`);
  if (rec.instance) rest.push(String(rec.instance).slice(0, 8));
  return { ask: `${rec.method} ${rec.path || '/'}`, end, rest: rest.join(' · ') };
}

// requestTitle is what a request's line shows on hover: the rest of its
// record.
export function requestTitle(rec) {
  const parts = [`${rec.method} ${rec.host || ''}${rec.path || ''} ${rec.proto || ''}`.trim()];
  if (rec.clientIp) parts.push(`from ${rec.clientIp}`);
  if (rec.userAgent) parts.push(rec.userAgent);
  parts.push(`${Number(rec.bytesIn) || 0} B in, ${Number(rec.bytesOut) || 0} B out`);
  if (Number(rec.heldNs) > 0) parts.push(`held ${durationText(rec.heldNs)} for an instance`);
  if (rec.instance) parts.push(`instance ${rec.instance}`);
  if (rec.traceId) parts.push(`trace ${rec.traceId}`);
  return parts.join('\n');
}

// isEgress tells a line of the requests a pod sent out through an HTTP
// binding: a line of an egress stream that is a record.
export const isEgress = (l, st) => !!(st && st.source === 'egress' && l.rec && l.rec.method);

// egressParts are an outbound request's line in a few words: the binding,
// what was asked, how it ended — the API's status, or the platform's
// refusal (FeOS-Binding-Error) with its code —, and its duration.
export function egressParts(rec) {
  const status = Number(rec.status) || 0;
  const refused = rec.error && typeof rec.error === 'object' ? rec.error.code || 'refused' : '';
  const end = status ? String(status) : refused ? 'refused' : 'no answer';
  const ms = Number(rec.durationMs);
  return {
    binding: rec.binding || '',
    ask: `${rec.method} ${rec.path || '/'}`,
    end,
    refused,
    rest: durationText(Number.isFinite(ms) ? ms * 1e6 : 0),
  };
}

// egressTitle is what an outbound request's line shows on hover: the rest
// of its record (never a credential: the record names only its kind).
export function egressTitle(rec) {
  const parts = [`${rec.method} https://${rec.host || rec.binding || ''}${rec.path || ''}`];
  parts.push(`through the binding ${rec.binding}${rec.mode ? ` (${rec.mode} mode)` : ''}`);
  if (rec.upstreamAddr) parts.push(`to ${rec.upstreamAddr}`);
  if (rec.credential) parts.push(`credential: ${rec.credential}`);
  parts.push(`${Number(rec.bytesIn) || 0} B in, ${Number(rec.bytesOut) || 0} B out`);
  if (rec.error && typeof rec.error === 'object') {
    parts.push(`refused by the platform: ${rec.error.code || ''}${rec.error.message ? ` — ${rec.error.message}` : ''}`);
  }
  if (rec.traceId) parts.push(`trace ${rec.traceId}`);
  return parts.join('\n');
}

// streamTag names a line's stream in a word: object#replica, a request's
// service, a job's name.
export function streamTag(st) {
  if (!st) return '';
  if (st.kind === 'job' || !st.kind || st.source === 'ingress' || st.source === 'egress') return st.object || st.job || '';
  return `${st.object}#${st.replica}`;
}

// The Logs tab shows its times in the viewer's zone or in UTC, as they
// pick beside the range: the lines' times, the days between them, and
// from and to.
export const TIME_ZONES = [
  ['local', 'local'],
  ['utc', 'UTC'],
];

const dateOf = (t) => {
  if (!t) return null;
  const d = new Date(/^\d+$/.test(t) ? Number(t) / 1e6 : t);
  return Number.isNaN(d.getTime()) ? null : d;
};
const pad = (n, w = 2) => String(n).padStart(w, '0');

// clockOf is a line's time of day with milliseconds, in local time or
// UTC, which ends in Z, so that a time read alone says its zone.
export function clockOf(time, utc = false) {
  const d = dateOf(time);
  if (!d) return '';
  if (utc) return d.toISOString().slice(11, 24);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
}

// dayOf is a line's day, for a divider where the day changes; dayText
// names it.
export function dayOf(time, utc = false) {
  const d = dateOf(time);
  if (!d) return '';
  return utc ? d.toISOString().slice(0, 10) : `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}
export function dayText(time, utc = false) {
  const d = dateOf(time);
  if (!d) return '';
  return d.toLocaleDateString('en-GB', { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric', ...(utc ? { timeZone: 'UTC' } : {}) });
}

// inputOf is a time (RFC 3339 or Unix ns) as the value of a
// datetime-local field, in local time or UTC; timeOfInput reads such a
// value back as RFC 3339.
export function inputOf(t, utc = false) {
  const d = dateOf(t);
  if (!d) return '';
  if (utc) return d.toISOString().slice(0, 16);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}
export const timeOfInput = (v, utc = false) => (v ? new Date(utc ? `${v}Z` : v).toISOString() : '');

// whatOf names what a search reads, for its count.
export function whatOf(focus) {
  const f = focus || {};
  const obj = f.object ? f.object.replace('/', ' ') : '';
  const rep = obj && f.replica != null ? `${obj}, replica ${f.replica}` : obj;
  let what;
  if (f.source === 'ingress') what = obj ? `the requests of ${obj}` : 'every request';
  else if (f.source === 'egress') what = obj ? `the outbound requests of ${rep}` : 'every outbound request';
  else if (f.source === 'container') what = obj ? `the output of ${rep}` : 'every object’s output';
  else what = rep || 'every object';
  return f.trace ? `${what}, trace ${f.trace.slice(0, 8)}…` : what;
}

// storeDown tells a failed read of a store that is not there or does not
// answer (no route to it yet, a gateway's 502/503/504, no network) from
// one that refused the search: the node's last lines can stand in then.
export const storeDown = (e) => !e || !e.status || e.status === 404 || e.status >= 502;
