// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The builder's forms and list (ADR 0032, "The builder edits the draft in
// forms"): what the list of objects shows (groups, one-line summaries,
// the draft's change against its base), and the forms' typed fields to
// the spec's values and back: a variable's value (Text, Secret, Addon
// output, Service address), a health check (a readiness), an idle, a
// binding of a tenant credential. Pure.

import { getObject, imageBaseName, isObject, objectsOf, setObject, uniqueName } from './draft.js';
import { envValueKind, secretNames } from './refs.js';
import { changesSince } from './rebase.js';
import { unitKey } from './findings.js';
import { imageView } from './pins.js';

// ---------------------------------------------------------------- list --

// unitOfItem is the object a builder's address names ("service/web", as
// …/builder/service/web has it) as the draft's unit ("services/web");
// '' for none or a kind the spec lacks. itemOfUnit is the way back.
export function unitOfItem(item) {
  const s = String(item || '');
  // new/<kind>: the form for a new one (secrets and bindings have one).
  if (s.startsWith('new/')) {
    const g = GROUPS.find((x) => x.kind === s.slice(4));
    return g ? `${g.key}/` : '';
  }
  const i = s.indexOf('/');
  if (i <= 0 || i === s.length - 1) return '';
  const g = GROUPS.find((x) => x.kind === s.slice(0, i));
  return g ? unitKey(g.key, s.slice(i + 1)) : '';
}

export function itemOfUnit(unit) {
  const s = String(unit || '');
  const i = s.indexOf('/');
  if (i > 0 && i === s.length - 1) {
    const g = GROUPS.find((x) => x.key === s.slice(0, i));
    return g ? `new/${g.kind}` : '';
  }
  if (i <= 0 || i === s.length - 1) return '';
  const g = GROUPS.find((x) => x.key === s.slice(0, i));
  return g ? `${g.kind}/${s.slice(i + 1)}` : '';
}

// GROUPS are the list's groups, in the order a person reads an
// environment: what runs, what it uses, how it is reached, what it holds.
export const GROUPS = [
  { key: 'services', title: 'Services', one: 'service', kind: 'service' },
  { key: 'jobs', title: 'Jobs', one: 'job', kind: 'job' },
  { key: 'addons', title: 'Addons', one: 'addon', kind: 'addon' },
  { key: 'routes', title: 'Routes', one: 'route', kind: 'route' },
  { key: 'bindings', title: 'Bindings', one: 'binding', kind: 'binding' },
  { key: 'secrets', title: 'Secrets', one: 'secret', kind: 'secret' },
];

// MARK is a change's mark in the list and the review.
export const MARK = { add: ['+', 'added'], update: ['~', 'changed'], remove: ['−', 'removed'] };

// changeMarks are the draft's changes against its base by unit
// ("services/web" → add, update or remove).
export function changeMarks(base, spec) {
  return new Map(changesSince(base || {}, spec || {}).map((c) => [c.unit, c.change]));
}

// listGroups are the list's groups with their rows: {key, title, one,
// rows: [{unit, key, name, mark, removed}]}; a row for every object of
// the draft (a secret it pins or references) and for every object of the
// base the draft removes, by name.
export function listGroups(spec, base, marks = changeMarks(base, spec)) {
  return GROUPS.map((g) => {
    const names = new Set(g.key === 'secrets' ? secretNames(spec) : objectsOf(spec, g.key).map(([n]) => n));
    const gone = new Set();
    for (const [n] of objectsOf(base, g.key)) {
      if (!names.has(n) && marks.get(unitKey(g.key, n)) === 'remove') { names.add(n); gone.add(n); }
    }
    const rows = [...names].sort((a, b) => a.localeCompare(b)).map((name) => {
      const unit = unitKey(g.key, name);
      return { unit, key: g.key, name, mark: marks.get(unit) || '', removed: gone.has(name) };
    });
    return { ...g, rows };
  });
}

// shortImage is an image as the list shows it: its last segment and tag
// ("web:2.3"), the digest's start for an image written by digest.
export function shortImage(o) {
  const w = imageView(o).written;
  if (!w) return '';
  const tag = /:([^/:@]+)$/.exec(w.split('@')[0]);
  if (tag) return `${imageBaseName(w)}:${tag[1]}`;
  const d = /@sha256:([0-9a-f]{7})/.exec(w);
  return imageBaseName(w) + (d ? `@${d[1]}` : '');
}

// summaryOf is a row's line under its name: a service's image tag, a
// job's and when it runs, an addon's type, version and plan, a route's
// path → service, a binding's credential or upstream, a secret's pin.
export function summaryOf(key, o, pin) {
  if (key === 'secrets') {
    if (!pin) return '';
    if (pin.missing) return 'no value yet';
    const p = pin.pinned ? `v${pin.pinned}` : 'latest';
    return pin.newer ? `${p} · v${pin.latest} newer` : pin.latest && pin.pinned ? `${p} of ${pin.latest}` : p;
  }
  if (!isObject(o)) return '';
  switch (key) {
    case 'services':
      if (o.runtime === 'attached') return `attached · ${o.sandbox || 'no sandbox'}`;
      return [shortImage(o) || 'no image yet', replicasWord(o.replicas)].filter(Boolean).join(' · ');
    case 'jobs': return [shortImage(o) || 'no image yet', o.on === 'manual' ? 'manual' : 'each deployment'].join(' · ');
    case 'addons': return [o.type, o.version, o.plan].filter(Boolean).join(' · ');
    case 'routes': return `${o.path || '?'} → ${o.service || '?'}${o.port !== undefined && o.port !== '' ? ':' + o.port : ''}`;
    case 'bindings': return bindingSummary(o);
    default: return '';
  }
}

function replicasWord(n) {
  if (n === undefined || n === null || n === 1) return '';
  return `${n} replica${n === 1 ? '' : 's'}`;
}

// bindingSummary is a binding's line: its tenant credential (and the
// version it pins), or its upstream's host.
export function bindingSummary(b) {
  if (!isObject(b)) return '';
  if (isObject(b.tenantCredential)) {
    const c = b.tenantCredential;
    return `credential ${c.name || '?'}${c.version ? ` · v${c.version}` : ''}`;
  }
  return String(b.upstream || 'no upstream').replace(/^https?:\/\//, '');
}

// ----------------------------------------------------------- variables --

// VALUE_TYPES are the typed choices of a variable's value.
export const VALUE_TYPES = [
  { key: 'text', label: 'Text' },
  { key: 'secret', label: 'Secret' },
  { key: 'addon', label: 'Addon output' },
  { key: 'service', label: 'Service address' },
];

// SERVICE_KEYS are what a service address gives: its URL, host or port.
export { SERVICE_KEYS } from './refs.js';

// valueType is the choice an env value is: a string or {value} is text;
// {secret}; {addon, key, …fields}; {service, port, key}; else other
// (kept as it is, edited in the JSON).
export function valueType(v) {
  switch (envValueKind(v)) {
    case 'string': case 'value': return 'text';
    case 'secret': return 'secret';
    case 'addon': return 'addon';
    case 'service': return 'service';
    default: return 'other';
  }
}

// textOf is a text value's text; withText keeps its form ({value} stays
// {value}, a string a string).
export function textOf(v) {
  return typeof v === 'string' ? v : isObject(v) && typeof v.value === 'string' ? v.value : '';
}

export function withText(v, s) {
  return isObject(v) && 'value' in v ? { value: s } : s;
}

// retypeValue is v as another choice, started with its first target:
// targets as refTargets answers them ({addons: [{name, keys}], services:
// [{name, ports}], secrets: [name]}). Text keeps the text it had.
export function retypeValue(v, type, targets = { addons: [], services: [], secrets: [] }) {
  if (type === valueType(v)) return v;
  switch (type) {
    case 'text': return typeof v === 'string' ? v : '';
    case 'secret': return { secret: targets.secrets[0] || '' };
    case 'addon': {
      const a = targets.addons[0];
      const out = { addon: a ? a.name : '' };
      const key = a && (a.keys.includes('url') ? 'url' : a.keys[0]);
      if (key) out.key = key;
      return out;
    }
    case 'service': {
      const s = targets.services[0];
      const out = { service: s ? s.name : '' };
      if (s && s.ports[0] !== undefined) out.port = s.ports[0];
      out.key = 'url';
      return out;
    }
    default: return v;
  }
}

// addonValue is an addon output with a field set: '' removes it; a new
// addon keeps the key when it has it, else takes its url or first key.
export function addonValue(v, patch, target) {
  const out = { ...v, ...patch };
  for (const [k, x] of Object.entries(out)) if (x === '' || x === undefined) delete out[k];
  if ('addon' in patch && target) {
    if (!target.keys.includes(out.key)) {
      const key = target.keys.includes('url') ? 'url' : target.keys[0];
      if (key) out.key = key; else delete out.key;
    }
    for (const k of Object.keys(out)) if (k !== 'addon' && k !== 'key' && !target.fields.includes(k)) delete out[k];
  }
  if (!('addon' in out)) out.addon = '';
  return out;
}

// serviceValue is a service address with a field set; a new service
// keeps the port when it has it, else takes its first.
export function serviceValue(v, patch, target) {
  const out = { ...v, ...patch };
  for (const [k, x] of Object.entries(out)) if (x === '' || x === undefined) delete out[k];
  if ('service' in patch && target && !target.ports.map(String).includes(String(out.port))) {
    if (target.ports[0] !== undefined) out.port = target.ports[0]; else delete out.port;
  }
  if (!('service' in out)) out.service = '';
  return out;
}

// portValue is a port as the spec names it: a number for digits, else
// its name.
export function portValue(s) {
  return /^\d+$/.test(String(s)) ? Number(s) : s;
}

// freeName is base, or base_2, base_3, …: the first name map lacks.
export function freeName(map, base) {
  let n = base;
  for (let i = 2; map && n in map; i++) n = `${base}_${i}`;
  return n;
}

// renameVar is env with variable from renamed to, in its place; env as
// it is when to is empty, the same or taken.
export function renameVar(env, from, to) {
  if (!to || to === from || to in env) return env;
  const out = {};
  for (const [k, v] of Object.entries(env)) out[k === from ? to : k] = v;
  return out;
}

// -------------------------------------------------------- health check --

// HEALTH_KINDS are a health check's choices.
export const HEALTH_KINDS = [
  { key: 'none', label: 'None' },
  { key: 'http', label: 'HTTP' },
  { key: 'tcp', label: 'TCP' },
  { key: 'exec', label: 'Command' },
];

// healthOf is a readiness as the form's fields: {kind, port, path,
// command, initialDelay, period, failureThreshold}.
export function healthOf(r) {
  const h = { kind: 'none', port: '', path: '', command: [], initialDelay: '', period: '', failureThreshold: undefined };
  if (!isObject(r)) return h;
  if (isObject(r.http)) Object.assign(h, { kind: 'http', port: r.http.port ?? '', path: r.http.path || '' });
  else if (isObject(r.tcp)) Object.assign(h, { kind: 'tcp', port: r.tcp.port ?? '' });
  else if (isObject(r.exec)) Object.assign(h, { kind: 'exec', command: Array.isArray(r.exec.command) ? r.exec.command : [] });
  h.initialDelay = r.initialDelay || '';
  h.period = r.period || '';
  h.failureThreshold = r.failureThreshold;
  return h;
}

// readinessOf is the form's fields as a readiness, undefined for none:
// the check of its kind and the timing that is set.
export function readinessOf(h) {
  if (!h || h.kind === 'none') return undefined;
  const r = {};
  if (h.kind === 'http') r.http = { port: h.port, path: h.path || '/' };
  else if (h.kind === 'tcp') r.tcp = { port: h.port };
  else r.exec = { command: h.command || [] };
  if (h.initialDelay) r.initialDelay = h.initialDelay;
  if (h.period) r.period = h.period;
  if (h.failureThreshold) r.failureThreshold = h.failureThreshold;
  return r;
}

// retypeHealth is h as another kind: a port check takes the service's
// first port when h has none, HTTP the path /.
export function retypeHealth(h, kind, ports = []) {
  const out = { ...h, kind };
  if ((kind === 'http' || kind === 'tcp') && (out.port === '' || out.port === undefined)) out.port = ports[0] ?? 'http';
  if (kind === 'http' && !out.path) out.path = '/';
  return out;
}

// ---------------------------------------------------------------- idle --

// idleOf is a service's idle as the form's choice: never ("" or
// "never"), after N minutes (a duration of whole minutes or hours), or
// as written (any other duration).
export function idleOf(s) {
  const v = String(s || '');
  if (!v || v === 'never') return { mode: 'never', minutes: 15, raw: v };
  const m = /^(?:(\d+)h)?(?:(\d+)m)?$/.exec(v);
  if (m && (m[1] || m[2])) return { mode: 'after', minutes: Number(m[1] || 0) * 60 + Number(m[2] || 0), raw: v };
  return { mode: 'custom', minutes: 15, raw: v };
}

// idleValue is the choice as the spec's idle: '' for never, "<N>m".
export function idleValue(mode, minutes) {
  if (mode === 'never') return '';
  return `${Math.max(1, Math.round(Number(minutes) || 0))}m`;
}

// routed says whether a route reaches the service: idle needs one.
export function routed(spec, name) {
  return objectsOf(spec, 'routes').some(([, r]) => isObject(r) && r.service === name);
}

// ------------------------------------------------------------ bindings --

// credentialBinding adds a binding of the tenant's credential cred (ADR
// 0033), named after it, and ticks it on key/name when given: {spec,
// name}. A binding of that credential the draft has is reused.
export function credentialBinding(spec, cred, key = '', name = '') {
  const have = objectsOf(spec, 'bindings').find(([, b]) => isObject(b) && isObject(b.tenantCredential) && b.tenantCredential.name === cred);
  let out = spec;
  let bn = have ? have[0] : '';
  if (!bn) {
    bn = uniqueName(spec, 'bindings', cred);
    out = setObject(out, 'bindings', bn, { tenantCredential: { name: cred } });
  }
  if (key && name) out = toggleBinding(out, key, name, bn, true);
  return { spec: out, name: bn };
}

// toggleBinding is spec with binding bn on or off key/name's list; an
// empty list goes.
export function toggleBinding(spec, key, name, bn, on) {
  const o = getObject(spec, key, name);
  if (!isObject(o)) return spec;
  const list = Array.isArray(o.bindings) ? o.bindings : [];
  if (on === list.includes(bn)) return spec;
  const next = on ? [...list, bn] : list.filter((b) => b !== bn);
  const out = { ...o, bindings: next };
  if (next.length === 0) delete out.bindings;
  return setObject(spec, key, name, out);
}

// CREDENTIAL_FIELDS are a literal binding credential's value fields by
// kind (ADR 0028): each text or a secret, tokenUrl text only.
export const CREDENTIAL_FIELDS = {
  headers: [],
  basic: ['user', 'password'],
  oauth2: ['tokenUrl', 'clientId', 'clientSecret', 'scope', 'audience'],
  clientCertificate: ['certPem', 'keyPem'],
};
