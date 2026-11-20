// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Where a key lives: the access of a service or job, and the inventory of
// an environment's secrets, sorted by the place of the key, not by where
// it is stored. Three marks (ADR 0014, 0028, 0038, 0055):
//   identity  — no key: the workload's certificate is enough (an addon
//               that admits a workload by it, through the pod's TLS
//               binding).
//   offloaded — "at the node": the key stays at the node, which adds it
//               (an HTTP secret; a value secret only image pulls name,
//               which the node pulls with; a binding stated in a spec of
//               before ADR 0055).
//   container — the value is handed to the container (a variable from a
//               value secret, an addon's password).
// A secret's kind (ADR 0055) is fixed: an HTTP secret is at the node
// whatever uses it; a value secret is where its uses put it. Pure: the
// spec, the addon catalog and the environment's secrets (GET
// …/environments/{env}/secrets, both kinds) in, rows out.

import { envValueKind, referencesOf } from './refs.js';
import { isObject, objectsOf } from './draft.js';

export const MARKS = ['identity', 'offloaded', 'container'];

export const MARK_LABEL = {
  identity: 'by identity',
  offloaded: 'at the node',
  container: 'in the container',
};

export const MARK_TITLE = {
  identity: "No key: the workload's certificate is enough.",
  offloaded: 'The key stays at the node, which adds it to each request. The container never sees it.',
  container: 'The value is handed to the container.',
};

// isHttp tells an HTTP secret of the environment's list from a value (an
// item of an older ferund, without a type, is a value).
export function isHttp(item) {
  return isObject(item) && item.type === 'http';
}

// httpSecretsOf are the HTTP secrets of the environment's list.
export function httpSecretsOf(secrets) {
  return (Array.isArray(secrets) ? secrets : []).filter(isHttp);
}

// kindsOf says each secret's kind, 'http' or 'value': the environment's
// list says it; for a name it lacks, the spec's pin (a normalised one
// carries type http) or a reference with a key (an HTTP secret's url or
// host) says http.
export function kindsOf(spec, secrets = [], refs = referencesOf(spec)) {
  const out = new Map();
  for (const s of Array.isArray(secrets) ? secrets : []) if (isObject(s) && s.name) out.set(s.name, isHttp(s) ? 'http' : 'value');
  const guess = (n, k) => { if (!out.has(n) || (k === 'http' && out.get(n) === '?')) out.set(n, k); };
  for (const [n, p] of objectsOf(spec, 'secrets')) if (!out.has(n)) out.set(n, isObject(p) && p.type === 'http' ? 'http' : '?');
  for (const r of refs) if (r.to.key === 'secrets') guess(r.to.name, r.http ? 'http' : '?');
  for (const [, b] of objectsOf(spec, 'bindings')) if (isObject(b) && isObject(b.credentialRef) && b.credentialRef.name) guess(b.credentialRef.name, 'http');
  for (const [n, k] of out) if (k === '?') out.set(n, 'value');
  return out;
}

// hostOf is an upstream's host and port; the upstream as it is when it is
// no URL.
function hostOf(upstream) {
  try {
    return new URL(upstream).host;
  } catch {
    return upstream || '';
  }
}

// reachedAs is how the code calls an API of name in mode: http://<name>,
// or the upstream's own host, intercepted.
function reachedAs(name, mode, upstream) {
  return mode === 'intercept' ? hostOf(upstream) : `http://${name}`;
}

// httpOf is an HTTP secret as the rows show it, from its item of the
// environment's list (null when the list lacks it), the version the spec
// pins (0 for none) and the spec's pin, whose normalised form carries the
// pinned version's upstream and mode (ADR 0055), for when the list does
// not say: {name, source: 'http', upstream, mode, kind, reachedAs,
// version (the latest, 0 unknown), pinned, known}.
export function httpOf(name, item, pinned = 0, pin = null) {
  const it = isObject(item) ? item : {};
  const p = isObject(pin) ? pin : {};
  const upstream = it.upstream || p.upstream || '';
  const mode = it.mode || p.mode || 'name';
  return {
    name, source: 'http', upstream, mode, kind: (isObject(it.credential) && it.credential.kind) || '',
    reachedAs: reachedAs(name, mode, upstream), version: Number(it.version) || 0, pinned: Number(pinned) || 0,
    known: isObject(item),
  };
}

// specPin is spec's pin of secret name, null for none.
function specPin(spec, name) {
  return spec && isObject(spec.secrets) && isObject(spec.secrets[name]) ? spec.secrets[name] : null;
}

// CREDENTIAL_FIELDS are a binding credential's fields that may name a
// secret, besides its headers.
const CREDENTIAL_FIELDS = ['user', 'password', 'tokenUrl', 'clientId', 'clientSecret', 'scope', 'audience', 'certPem', 'keyPem'];

// bindingSecrets are the secrets a binding's inline credential names, sorted.
export function bindingSecrets(b) {
  const c = isObject(b) && isObject(b.credential) ? b.credential : null;
  if (!c) return [];
  const vals = [...Object.values(isObject(c.headers) ? c.headers : {}), ...CREDENTIAL_FIELDS.map((k) => c[k])];
  return [...new Set(vals.filter((v) => envValueKind(v) === 'secret').map((v) => v.secret))].sort();
}

// bindingOf is a binding of spec (stated in the spec, from before ADR
// 0055) as the rows show it, with what an HTTP secret it names
// (credentialRef: an environment's credential, now an HTTP secret of the
// same name) states filled in from secrets: {name, source: 'binding',
// upstream, mode, kind, reachedAs, credential: {name, version} | null,
// secrets}.
export function bindingOf(spec, name, secrets = []) {
  const b = spec && isObject(spec.bindings) && isObject(spec.bindings[name]) ? spec.bindings[name] : {};
  const ref = isObject(b.credentialRef) ? b.credentialRef : null;
  const cred = ref ? httpSecretsOf(secrets).find((c) => c.name === ref.name) || null : null;
  const upstream = b.upstream || (cred && cred.upstream) || '';
  const mode = b.mode || (cred && cred.mode) || 'name';
  const kind = (isObject(b.credential) && b.credential.kind) || (cred && cred.credential && cred.credential.kind) || '';
  return {
    name,
    source: 'binding',
    upstream,
    mode,
    kind,
    reachedAs: reachedAs(name, mode, upstream),
    credential: ref ? { name: ref.name, version: ref.version || 0 } : null,
    secrets: bindingSecrets(b),
  };
}

// addonMark is the mark of the keys a reference names of an addon: by
// identity when the addon's type admits a workload by its certificate,
// in the container when a key carries the addon's password (its own
// password, or the url that embeds it), else null (host and port alone
// hand over no key).
export function addonMark(blueprint, keys) {
  if (blueprint && blueprint.byIdentity) return 'identity';
  const hasPassword = !!blueprint && (blueprint.outputs || []).includes('password');
  if (hasPassword && keys.some((k) => k === 'password' || k === 'url')) return 'container';
  return null;
}

const MARK_ORDER = { identity: 0, offloaded: 1, container: 2 };

function byMark(a, b) {
  return (MARK_ORDER[a.mark] ?? 3) - (MARK_ORDER[b.mark] ?? 3) || a.target.name.localeCompare(b.target.name);
}

// pinOf is the version spec pins of secret name, 0 for none.
function pinOf(spec, name) {
  return spec && isObject(spec.secrets) && isObject(spec.secrets[name]) ? Number(spec.secrets[name].version) || 0 : 0;
}

// accessOf lists what the object key/name ('services' or 'jobs') of spec
// reaches with a key or without one, one row per target:
// {target: {key, name}, mark, kind, variables, keys, http?, binding?,
// addon?, fields?}. kind is http (an HTTP secret its variables reference,
// or a binding of the spec names, credentialRef; via the bindings that
// do), binding (a binding stated in the spec), addon, secret or image.
// Sorted by mark, then name.
export function accessOf(spec, key, name, { catalog = null, secrets = [], refs = referencesOf(spec) } = {}) {
  const own = refs.filter((r) => r.from.key === key && r.from.name === name);
  const kinds = kindsOf(spec, secrets, refs);
  const items = new Map((Array.isArray(secrets) ? secrets : []).filter((s) => isObject(s) && s.name).map((s) => [s.name, s]));
  const rows = [];
  const https = new Map();
  const http = (n) => {
    if (!https.has(n)) https.set(n, { variables: [], keys: [], via: [], pinned: pinOf(spec, n) });
    return https.get(n);
  };
  for (const r of own.filter((x) => x.via === 'bindings')) {
    const b = spec && isObject(spec.bindings) && isObject(spec.bindings[r.to.name]) ? spec.bindings[r.to.name] : {};
    if (isObject(b.credentialRef) && b.credentialRef.name) {
      const h = http(b.credentialRef.name);
      h.via.push(r.to.name);
      if (!h.pinned) h.pinned = Number(b.credentialRef.version) || 0;
      continue;
    }
    rows.push({ target: { key: 'bindings', name: r.to.name }, kind: 'binding', mark: 'offloaded', variables: [], keys: [],
      binding: bindingOf(spec, r.to.name, secrets) });
  }
  const addons = new Map();
  for (const r of own.filter((x) => x.to.key === 'addons')) {
    const a = addons.get(r.to.name) || { variables: [], keys: [], fields: {} };
    if (r.via === 'env') {
      a.variables.push(r.variable);
      a.keys.push(r.refKey || 'url');
      Object.assign(a.fields, r.fields || {});
    }
    addons.set(r.to.name, a);
  }
  for (const [addon, a] of addons) {
    const def = spec && isObject(spec.addons) && isObject(spec.addons[addon]) ? spec.addons[addon] : {};
    const bp = catalog && catalog.byType ? catalog.byType.get(def.type) : null;
    rows.push({ target: { key: 'addons', name: addon }, kind: 'addon', mark: addonMark(bp, a.keys),
      variables: a.variables.sort(), keys: [...new Set(a.keys)].sort(), addon: { type: def.type || '' }, fields: a.fields });
  }
  const values = new Map();
  for (const r of own.filter((x) => x.via === 'env' && x.to.key === 'secrets')) {
    if (kinds.get(r.to.name) === 'http') {
      const h = http(r.to.name);
      h.variables.push(r.variable);
      if (r.refKey) h.keys.push(r.refKey);
    } else {
      values.set(r.to.name, [...(values.get(r.to.name) || []), r.variable]);
    }
  }
  for (const [n, h] of https) {
    rows.push({ target: { key: 'secrets', name: n }, kind: 'http', mark: 'offloaded', variables: h.variables.sort(),
      keys: [...new Set(h.keys)].sort(), via: h.via.sort(), http: httpOf(n, items.get(n), h.pinned, specPin(spec, n)) });
  }
  for (const [secret, vars] of values) {
    rows.push({ target: { key: 'secrets', name: secret }, kind: 'secret', mark: 'container', variables: vars.sort(), keys: [] });
  }
  const pull = [...new Set(own.filter((x) => x.via === 'imageCredentials').map((x) => x.to.name))];
  for (const secret of pull) {
    rows.push({ target: { key: 'secrets', name: secret }, kind: 'image', mark: 'offloaded', variables: [], keys: [] });
  }
  return rows.sort(byMark);
}

// secretUses are the uses of the environment's value secret name in
// spec: [{from: {key, name}, mark, how, variable?, binding?}] — a
// variable (in the container), a binding's credential (at the node, for
// the objects that name the binding) or a registry login (at the node).
export function secretUses(spec, name, refs = referencesOf(spec)) {
  const out = [];
  for (const r of refs) {
    if (r.to.key !== 'secrets' || r.to.name !== name) continue;
    if (r.via === 'env') out.push({ from: r.from, mark: 'container', how: 'variable', variable: r.variable });
    else if (r.via === 'imageCredentials') out.push({ from: r.from, mark: 'offloaded', how: 'image' });
    else if (r.via === 'credential') {
      const users = refs.filter((x) => x.via === 'bindings' && x.to.name === r.from.name);
      if (users.length === 0) out.push({ from: r.from, mark: 'offloaded', how: 'binding', binding: r.from.name });
      for (const u of users) out.push({ from: u.from, mark: 'offloaded', how: 'binding', binding: r.from.name });
    }
  }
  return dedupe(out);
}

function dedupe(uses) {
  const seen = new Set();
  return uses.filter((u) => {
    const k = `${u.from.key}/${u.from.name}/${u.how}/${u.variable || ''}/${u.binding || ''}`;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
}

// httpUsers are the services and jobs of spec that get HTTP secret name:
// by a variable that references it, or by a binding that names it
// (credentialRef): [{key, name, variables, bindings}], sorted.
function httpUsers(spec, name, refs) {
  const by = new Map();
  const at = (f) => {
    const k = `${f.key}/${f.name}`;
    if (!by.has(k)) by.set(k, { key: f.key, name: f.name, variables: [], bindings: [] });
    return by.get(k);
  };
  for (const r of refs) {
    if (r.via === 'env' && r.to.key === 'secrets' && r.to.name === name) at(r.from).variables.push(r.variable);
    if (r.via === 'bindings') {
      const b = spec && isObject(spec.bindings) ? spec.bindings[r.to.name] : null;
      if (isObject(b) && isObject(b.credentialRef) && b.credentialRef.name === name) at(r.from).bindings.push(r.to.name);
    }
  }
  return [...by.values()].map((u) => ({ ...u, variables: u.variables.sort(), bindings: [...new Set(u.bindings)].sort() }))
    .sort((a, b) => a.key.localeCompare(b.key) || a.name.localeCompare(b.name));
}

// PLACES are where a key lives, the Secrets view's groups: the three
// marks and the value secrets nothing references.
export const PLACES = ['identity', 'offloaded', 'container', 'unused'];

// inventoryOf lists every key of an environment, each once, with the
// place it lives (place, one of PLACES): the HTTP secrets (the list's and
// those the spec names that it lacks: at the node), the value secrets
// (those with a value and those the spec names: in the container when a
// variable references one, at the node when image pulls or bindings
// alone name it, else unused), the bindings stated in the spec (from
// before ADR 0055: at the node) and the addons that admit by identity or
// hand out a password. Each row: {kind (http, value, binding, addon),
// name, place, marks, users, state, …}; state is set, missing (named, no
// value) or unused (no one names it).
//
// An HTTP secret's row: users [{key, name, variables, bindings}],
// sandboxes (IDs), pins {latest, deployed} (the versions the latest and
// the deployed deployment pin, 0 none), pinned (what the spec pins),
// version, http (httpOf), item. A value secret's: uses, version, pinned,
// and folded when only bindings of the spec name it — its key is theirs,
// in their rows (bindings). A binding's: binding (bindingOf), its
// secrets [{name, state, version}], and taken when an HTTP secret of its
// name exists. An addon's: uses with each user's mark and fields.
export function inventoryOf(spec, { catalog = null, secrets = [] } = {}) {
  const refs = referencesOf(spec);
  const list = Array.isArray(secrets) ? secrets.filter((s) => isObject(s) && s.name) : [];
  const known = new Map(list.map((s) => [s.name, s]));
  const kinds = kindsOf(spec, list, refs);
  const rows = [];
  const objUsers = (uses) => uniqueFrom(uses.map((u) => u.from).filter((f) => f.key === 'services' || f.key === 'jobs'));
  const KEY_OF = { service: 'services', job: 'jobs' };

  const credRefPins = new Map();
  for (const [, b] of objectsOf(spec, 'bindings')) {
    if (isObject(b) && isObject(b.credentialRef) && b.credentialRef.name) {
      const v = Number(b.credentialRef.version) || 0;
      credRefPins.set(b.credentialRef.name, Math.max(credRefPins.get(b.credentialRef.name) || 0, v));
    }
  }
  for (const name of [...kinds.keys()].filter((n) => kinds.get(n) === 'http').sort()) {
    const item = known.get(name) || null;
    const users = httpUsers(spec, name, refs);
    const used = item && isObject(item.usedBy) ? item.usedBy : {};
    for (const ref of Array.isArray(used.objects) ? used.objects : []) {
      const [k, n] = String(ref).split('/');
      if (KEY_OF[k] && n && !users.some((u) => u.key === KEY_OF[k] && u.name === n)) {
        users.push({ key: KEY_OF[k], name: n, variables: [], bindings: [] });
      }
    }
    const sandboxes = Array.isArray(used.sandboxes) ? [...used.sandboxes].sort() : [];
    const pinned = pinOf(spec, name) || credRefPins.get(name) || 0;
    rows.push({
      kind: 'http', name, place: 'offloaded', marks: ['offloaded'], users, sandboxes,
      pins: { latest: Number(used.latest) || 0, deployed: Number(used.deployed) || 0 }, pinned,
      bindings: [...new Set(users.flatMap((u) => u.bindings))].sort(),
      state: !item ? 'missing' : users.length === 0 && sandboxes.length === 0 ? 'unused' : 'set',
      version: item ? Number(item.version) || 0 : 0, http: httpOf(name, item, pinned, specPin(spec, name)), item,
    });
  }

  const values = new Map();
  for (const name of [...kinds.keys()].filter((n) => kinds.get(n) === 'value').sort()) {
    const uses = secretUses(spec, name, refs);
    const s = known.get(name);
    const marks = marksOf(uses);
    const folded = uses.length > 0 && uses.every((u) => u.how === 'binding');
    const row = {
      kind: 'value', name, marks, users: objUsers(uses), uses,
      place: marks.includes('container') ? 'container' : marks.length ? 'offloaded' : 'unused',
      state: !s ? 'missing' : uses.length === 0 ? 'unused' : 'set', version: s ? Number(s.version) || 0 : 0,
      pinned: pinOf(spec, name), folded, bindings: [...new Set(uses.filter((u) => u.how === 'binding').map((u) => u.binding))].sort(),
    };
    values.set(name, row);
    rows.push(row);
  }

  for (const [n, b] of objectsOf(spec, 'bindings')) {
    if (isObject(b) && isObject(b.credentialRef)) continue;
    const objs = uniqueFrom(refs.filter((r) => r.via === 'bindings' && r.to.name === n).map((r) => r.from));
    const secretRows = bindingSecrets(b).map((sn) => {
      const v = values.get(sn);
      return { name: sn, state: v ? v.state : 'missing', version: v ? v.version : 0, pinned: v ? v.pinned : 0 };
    });
    rows.push({ kind: 'binding', name: n, place: 'offloaded', marks: ['offloaded'], users: objs, binding: bindingOf(spec, n, list),
      secrets: secretRows, taken: kinds.get(n) === 'http' && known.has(n), state: objs.length === 0 ? 'unused' : 'set' });
  }

  for (const [n, a] of objectsOf(spec, 'addons')) {
    const bp = catalog && catalog.byType && isObject(a) ? catalog.byType.get(a.type) : null;
    const objs = [];
    const uses = [];
    const marks = new Set();
    for (const [key, objName] of uniqueFrom(refs.filter((r) => r.to.key === 'addons' && r.to.name === n).map((r) => r.from))
      .map((f) => [f.key, f.name])) {
      const row = accessOf(spec, key, objName, { catalog, refs }).find((x) => x.kind === 'addon' && x.target.name === n);
      if (row && row.mark) {
        marks.add(row.mark);
        objs.push({ key, name: objName });
        uses.push({ from: { key, name: objName }, mark: row.mark, fields: row.fields || {}, variables: row.variables });
      }
    }
    if (marks.size === 0 && !(bp && bp.byIdentity)) continue;
    const ms = marks.size ? MARKS.filter((m) => marks.has(m)) : ['identity'];
    rows.push({ kind: 'addon', name: n, place: ms[0], marks: ms, users: objs, uses,
      addon: { type: isObject(a) ? a.type || '' : '' }, state: objs.length === 0 ? 'unused' : 'set' });
  }
  return rows;
}

function marksOf(uses) {
  const s = new Set(uses.map((u) => u.mark));
  return MARKS.filter((m) => s.has(m));
}

function uniqueFrom(froms) {
  const seen = new Set();
  return froms.filter((f) => {
    const k = `${f.key}/${f.name}`;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  }).sort((a, b) => a.key.localeCompare(b.key) || a.name.localeCompare(b.name));
}

// INVENTORY_GROUPS are the Secrets view's groups, by where the key lives,
// in its order; rows of a group are inventoryOf's of its place, the
// value secrets bindings hold (folded) in their bindings' rows.
export const INVENTORY_GROUPS = [
  { place: 'identity', title: 'By identity', mark: 'identity' },
  { place: 'offloaded', title: 'At the node', mark: 'offloaded' },
  { place: 'container', title: 'In the container', mark: 'container' },
  { place: 'unused', title: 'Unused', mark: '' },
];

// KIND_ORDER sorts a group's rows: the HTTP secrets first, then the
// bindings of the spec, the value secrets, the addons.
const KIND_ORDER = { http: 0, binding: 1, value: 2, addon: 3 };

// groupRows are the rows of place, sorted by kind, then name.
export function groupRows(rows, place) {
  return (rows || []).filter((r) => r.place === place && !r.folded)
    .sort((a, b) => (KIND_ORDER[a.kind] ?? 9) - (KIND_ORDER[b.kind] ?? 9) || a.name.localeCompare(b.name));
}

// fmtNumber is a whole number with thousands separators: "1,204".
export function fmtNumber(n) {
  return String(Math.round(Number(n) || 0)).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
}

// KIND_SHORT name a binding credential's kind in a word or two.
const KIND_SHORT = { headers: 'headers', basic: 'basic', oauth2: 'OAuth2', clientCertificate: 'client certificate' };

export function kindShort(kind) {
  return KIND_SHORT[kind] || kind || '';
}

// bindingSource says where a binding's key comes from (bindingOf's b):
// "HTTP secret github v3" (its latest after it when the HTTP secret has
// a later one), "secret x", "secrets x, y", or ''.
export function bindingSource(b, secrets = []) {
  if (b && b.credential) {
    const c = httpSecretsOf(secrets).find((x) => x.name === b.credential.name);
    const v = b.credential.version;
    const later = c && v && c.version > v ? `, latest v${c.version}` : '';
    return `HTTP secret ${b.credential.name}${v ? ` v${v}` : ''}${later}`;
  }
  const s = (b && b.secrets) || [];
  if (s.length === 0) return '';
  return `secret${s.length === 1 ? '' : 's'} ${s.join(', ')}`;
}

// fieldsWords are a reference's fields to an addon in words: "role
// api_svc · database app".
export function fieldsWords(fields) {
  return Object.entries(isObject(fields) ? fields : {}).sort(([a], [b]) => a.localeCompare(b))
    .filter(([, v]) => v !== '' && v !== undefined && v !== null).map(([k, v]) => `${k} ${v}`).join(' · ');
}

// usageOf is binding name's use in a bindingUsage answer; null without
// numbers (no answer, or the log store unavailable, whose zeros are
// none).
export function usageOf(usage, name) {
  if (!isObject(usage) || usage.unavailable || !Array.isArray(usage.bindings)) return null;
  return usage.bindings.find((b) => isObject(b) && b.name === name) || null;
}

// USAGE_PROBLEMS are a binding's problem classes (the incidents' and the
// refusals they leave out) as the numbers name them.
const USAGE_PROBLEMS = [['refused', 'refused'], ['failing', 'failing'], ['tokenFailed', 'without a token'], ['blocked', 'blocked']];

// usageWords is a binding's use in the window in a few words: "1,204
// req/24h · 2 refused · 1 blocked", "no request in 24h"; '' without
// numbers.
export function usageWords(u, window = '24h') {
  if (!u) return '';
  if (!(Number(u.requests) > 0)) return `no request in ${window}`;
  const parts = [`${fmtNumber(u.requests)} req/${window}`];
  for (const [k, w] of USAGE_PROBLEMS) if (Number(u[k]) > 0) parts.push(`${fmtNumber(u[k])} ${w}`);
  return parts.join(' · ');
}

// usageTone is how bad a binding's use is: bad when the upstream refused
// the credential, failed or no token came; warn when only the platform
// refused requests (a path, a rate); '' else.
export function usageTone(u) {
  if (!u) return '';
  if (Number(u.refused) > 0 || Number(u.failing) > 0 || Number(u.tokenFailed) > 0) return 'bad';
  return Number(u.blocked) > 0 ? 'warn' : '';
}

// problemWords say a binding's last problem: "403 denied_path: path /admin
// is not allowed", "the upstream answered 401"; '' for none.
export function problemWords(p) {
  if (!isObject(p)) return '';
  if (p.code) return `${p.status ? `${p.status} ` : ''}${p.code}${p.message ? `: ${p.message}` : ''}`;
  return p.status ? `the upstream answered ${p.status}` : 'no answer';
}

// usersWords say who sent a binding's requests, the most first: "service
// api 1,200 · sandbox c3kv9mwq2hd 4".
export function usersWords(users) {
  return (Array.isArray(users) ? users : []).map((u) => `${u.kind} ${u.name} ${fmtNumber(u.requests)}`).join(' · ');
}

// strayUsage are the bindings a usage answer counts that are neither a
// binding of spec nor an HTTP secret of secrets: those a sandbox's own
// set states (ADR 0028, until ADR 0055 ends it). Only the ones with
// requests.
export function strayUsage(usage, spec, secrets = []) {
  if (!isObject(usage) || usage.unavailable || !Array.isArray(usage.bindings)) return [];
  const own = new Set([...objectsOf(spec, 'bindings').map(([n]) => n), ...httpSecretsOf(secrets).map((c) => c.name)]);
  return usage.bindings.filter((b) => isObject(b) && b.name && !own.has(b.name) && Number(b.requests) > 0)
    .sort((a, b) => a.name.localeCompare(b.name));
}

// workloadOf is the workload kind/name ('service', 'job') of an
// identities answer, null when it has none.
export function workloadOf(identities, kind, name) {
  const list = isObject(identities) && Array.isArray(identities.workloads) ? identities.workloads : [];
  return list.find((w) => isObject(w) && w.kind === kind && w.name === name) || null;
}

// soonestCert is the pod of pods whose certificate ends first (one
// without a certificate yet left out), null when none has one.
export function soonestCert(pods) {
  const has = (Array.isArray(pods) ? pods : []).filter((p) => isObject(p) && p.notAfter && !Number.isNaN(Date.parse(p.notAfter)));
  if (has.length === 0) return null;
  return has.reduce((a, b) => (Date.parse(b.notAfter) < Date.parse(a.notAfter) ? b : a));
}

// subjectParts are an RFC 2253 subject's attributes in its order:
// "CN=td/pod,OU=wl-x" → [['CN', 'td/pod'], ['OU', 'wl-x']]. A pod's
// subject holds no character RFC 2253 escapes (ferund's
// pki.validatePodSubject), so a comma ends an attribute.
export function subjectParts(dn) {
  if (typeof dn !== 'string' || !dn) return [];
  return dn.split(',').map((a) => {
    const i = a.indexOf('=');
    return i < 0 ? ['', a] : [a.slice(0, i), a.slice(i + 1)];
  });
}

// podSubject is the subject a workload's pods carry, as
// CN=<trust domain>/<pod>,OU=<workload>: the pod's own when one is
// given, else with "<pod>" in its place; '' when the answer names no OU
// (a ferund of before it) or there is no trust domain.
export function podSubject(identities, workload, pod = null) {
  if (pod && pod.subject) return pod.subject;
  const td = isObject(identities) ? identities.trustDomain : '';
  if (!td || !isObject(workload) || !workload.ou) return '';
  return `CN=${td}/${pod && pod.pod ? pod.pod : '<pod>'},OU=${workload.ou}`;
}

// identityReaders are the addons a workload reaches by its certificate
// (its access rows by identity) with the part of the certificate each
// maps to a user of its own, by the catalog's identityBy: 'ou' (the
// subject's OU: postgres's role, which the reference names) or 'uri'
// (the SPIFFE ID: valkey's ACL user); '' when the catalog does not say.
// [{addon, type, by, role}], by name.
export function identityReaders(rows, catalog) {
  return (rows || []).filter((r) => r.kind === 'addon' && r.mark === 'identity').map((r) => {
    const bp = catalog && catalog.byType ? catalog.byType.get(r.addon && r.addon.type) : null;
    return { addon: r.target.name, type: (r.addon && r.addon.type) || '', by: (bp && bp.identityBy) || '',
      role: (r.fields && r.fields.role) || '' };
  }).sort((a, b) => a.addon.localeCompare(b.addon));
}

// secretPinNote says a secret the latest deployment pins at an earlier
// version than its latest: "the latest deployment pins v2, latest v3";
// '' when it pins the latest or none.
export function secretPinNote(pinned, version) {
  const p = Number(pinned) || 0;
  const v = Number(version) || 0;
  return p > 0 && v > p ? `the latest deployment pins v${p}, latest v${v}` : '';
}

// httpPinNote says an HTTP secret its deployments pin behind its latest
// version (pins: its row's {latest, deployed}): "deployed pins v3,
// latest v4", "v4 rolling out; deployed pins v3"; '' when they pin the
// latest or none.
export function httpPinNote(pins, version) {
  const l = Number(pins && pins.latest) || 0;
  const d = Number(pins && pins.deployed) || 0;
  const v = Number(version) || 0;
  if (l && l < v) {
    if (!d || d === l) return `${d === l ? 'deployed' : 'the latest deployment'} pins v${l}, latest v${v}`;
    return `the latest deployment pins v${l}, deployed v${d}; latest v${v}`;
  }
  if (d && d < v) return l === v ? `v${v} rolling out; deployed pins v${d}` : `deployed pins v${d}, latest v${v}`;
  return '';
}

// ----------------------------------------------------- in the picture --

// KIND_WORDS name a binding credential's kind where the reachability
// picture says where its key is.
const KIND_WORDS = { headers: 'Headers', basic: 'Basic auth', oauth2: 'OAuth2', clientCertificate: 'Client certificate' };

// keyAtNode says a binding's key and where it is: "OAuth2 at the node";
// "Key at the node" for a kind not known (yet).
export function keyAtNode(kind) {
  return `${KIND_WORDS[kind] || 'Key'} at the node`;
}

// upstreamRow is the row of an API a workload calls on the right of the
// reachability picture (reach.js's rows): b is an HTTP secret (httpOf)
// or a binding of the spec (bindingOf); its upstream's host as the peer
// (its name and how the pod reaches it below), the key's kind on the
// edge, the mark at the node. usage is bindingUsage's entry of its name,
// or null; self ({kind, name}) the object or pod the row is of, whose
// share of the requests its users say (mine, of by; null without self
// or users).
export function upstreamRow(b, usage = null, self = null) {
  const host = hostOf(b.upstream);
  const users = usage && Array.isArray(usage.users) ? usage.users : null;
  const own = self && users ? users.find((u) => isObject(u) && u.kind === self.kind && u.name === self.name) : null;
  const http = b.source === 'http';
  return {
    key: `${http ? 'secret' : 'binding'}:${b.name}`,
    peer: { type: 'upstream', name: host || b.name, sub: `${b.name} · as ${b.reachedAs}` },
    what: keyAtNode(b.kind),
    via: [`${http ? 'HTTP secret' : 'binding'} ${b.name}`],
    mark: 'offloaded',
    binding: b,
    usage: usage ? { requests: usage.requests || 0, refused: usage.refused || 0, failing: usage.failing || 0,
      mine: self && users ? (own && own.requests) || 0 : null, by: self ? self.name : '' } : null,
  };
}

const EGRESS_PEERS = new Set(['internet', 'domain', 'cidr']);
const SELF_KEY = { service: 'services', job: 'jobs' };

// withKeys is reachModel's picture (reach.js) with where each key lives:
// a service's or a job's row to an addon, and the addon's row from it,
// carry the addon's mark of accessOf (by identity; in the container; none
// for host and port alone); a service's or a job's right side gets a row
// per HTTP secret it gets and per binding of the spec it names
// (upstreamRow), before its egress, and a sandbox's one per HTTP secret
// of the environment it names (secrets: the environment's list, both
// kinds). Rows between services stay as they are: no key goes there.
// usage is bindingUsage's bindings, or null for no numbers. marks are the
// marks the picture shows.
//
// The numbers are matched by name: bindingUsage counts per binding, and
// a pod's binding of an HTTP secret is named by the secret.
export function withKeys(model, spec, { catalog = null, secrets = [], usage = null } = {}) {
  const refs = referencesOf(spec);
  const used = new Map((Array.isArray(usage) ? usage : []).filter((u) => isObject(u) && u.name).map((u) => [u.name, u]));
  const shown = new Set();
  const rowsOf = new Map();
  const access = (kind, name) => {
    const ref = `${kind}/${name}`;
    if (!rowsOf.has(ref)) rowsOf.set(ref, SELF_KEY[kind] ? accessOf(spec, SELF_KEY[kind], name, { catalog, secrets, refs }) : []);
    return rowsOf.get(ref);
  };
  const marked = (r, kind, name, addon) => {
    const a = access(kind, name).find((x) => x.kind === 'addon' && x.target.name === addon);
    if (!a || !a.mark) return r;
    shown.add(a.mark);
    return { ...r, mark: a.mark };
  };
  const withUpstreams = (outs, apis, self) => {
    if (!apis.length) return outs;
    shown.add('offloaded');
    const rows = apis.map((b) => upstreamRow(b, used.get(b.name) || null, self));
    const at = outs.findIndex((r) => EGRESS_PEERS.has(r.peer.type));
    return at < 0 ? [...outs, ...rows] : [...outs.slice(0, at), ...rows, ...outs.slice(at)];
  };
  const objects = model.objects.map((o) => {
    if (SELF_KEY[o.kind]) {
      const outs = o.outs.map((r) => (r.peer.type === 'addon' ? marked(r, o.kind, o.name, r.peer.name) : r));
      const apis = access(o.kind, o.name).filter((x) => x.kind === 'http' || x.kind === 'binding')
        .map((x) => (x.kind === 'http' ? x.http : x.binding));
      return { ...o, outs: withUpstreams(outs, apis, { kind: o.kind, name: o.name }) };
    }
    if (o.kind === 'addon') {
      return { ...o, ins: o.ins.map((r) => (SELF_KEY[r.peer.type] ? marked(r, r.peer.type, r.peer.name, o.name) : r)) };
    }
    return o;
  });
  const pods = model.pods.map((p) => {
    if (p.kind !== 'sandbox') return p;
    const held = httpSecretsOf(secrets).filter((c) => isObject(c.usedBy) && Array.isArray(c.usedBy.sandboxes)
      && c.usedBy.sandboxes.includes(p.name)).sort((a, b) => a.name.localeCompare(b.name));
    return { ...p, outs: withUpstreams(p.outs, held.map((c) => httpOf(c.name, c, c.version)), { kind: 'sandbox', name: p.name }) };
  });
  return { ...model, objects, pods, marks: MARKS.filter((m) => shown.has(m)) };
}
