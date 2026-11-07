// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The deployment builder's draft (ADR 0032): a deployment spec in the
// API's JSON and the number of the deployment it starts from, its base.
// Pure, as envs.js: no React and no browser. Every operation returns a
// new spec and leaves its argument alone.
//
// The spec's shape is ferund's (go/internal/env/spec/types.go): maps of
// objects by kind, each keyed by the object's name.

// SPEC_KINDS are the spec's maps of objects, in the order the API writes
// them; kind is the word diffs and findings use.
export const SPEC_KINDS = [
  { key: 'services', kind: 'service', title: 'services', one: 'service' },
  { key: 'jobs', kind: 'job', title: 'jobs', one: 'job' },
  { key: 'addons', kind: 'addon', title: 'addons', one: 'addon' },
  { key: 'routes', kind: 'route', title: 'routes', one: 'route' },
  { key: 'secrets', kind: 'secret', title: 'secrets', one: 'secret' },
  { key: 'bindings', kind: 'binding', title: 'bindings', one: 'binding' },
  { key: 'securityGroups', kind: 'securityGroup', title: 'security groups', one: 'security group' },
];

const KIND_BY_KEY = new Map(SPEC_KINDS.map((k) => [k.key, k]));
const KEY_BY_KIND = new Map(SPEC_KINDS.map((k) => [k.kind, k.key]));

export function kindOf(key) {
  return KIND_BY_KEY.get(key) || null;
}

export function keyOfKind(kind) {
  return KEY_BY_KIND.get(kind) || '';
}

// The fields of each kind in the order of ferund's types, so that the
// text keeps the order the API answers with; a key the list lacks goes
// last, where it was.
const ORDER = {
  spec: ['services', 'jobs', 'addons', 'routes', 'secrets', 'bindings', 'securityGroups', 'volumes'],
  services: ['runtime', 'image', 'imageAsWritten', 'imageCredentials', 'sandbox', 'command', 'cwd', 'env', 'ports',
    'replicas', 'resources', 'readiness', 'idle', 'rollout', 'mounts', 'bindings', 'securityGroups'],
  jobs: ['image', 'imageAsWritten', 'command', 'env', 'resources', 'imageCredentials', 'on', 'timeout', 'retries',
    'bindings', 'securityGroups'],
  addons: ['type', 'version', 'plan', 'options', 'securityGroups'],
  securityGroups: ['in', 'out'],
  routes: ['path', 'service', 'port', 'strip', 'host'],
  secrets: ['version'],
  bindings: ['upstream', 'mode', 'caPem', 'pathPrefixes', 'forwardedHost', 'rateLimit', 'credential'],
};

export function isObject(v) {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

function ordered(obj, order) {
  if (!isObject(obj)) return obj;
  const out = {};
  for (const k of order) if (k in obj) out[k] = obj[k];
  for (const k of Object.keys(obj)) if (!(k in out)) out[k] = obj[k];
  return out;
}

// orderSpec is spec with its kinds and each object's fields in the API's
// order; names keep the spec's order.
export function orderSpec(spec) {
  const top = ordered(spec || {}, ORDER.spec);
  for (const [key, map] of Object.entries(top)) {
    if (!ORDER[key] || !isObject(map)) continue;
    const m = {};
    for (const [name, obj] of Object.entries(map)) m[name] = ordered(obj, ORDER[key]);
    top[key] = m;
  }
  return top;
}

// specText is the text view's JSON: indented by two, the API's order.
export function specText(spec) {
  return JSON.stringify(orderSpec(spec), null, 2);
}

// parseSpecText reads the text view or an imported file: {spec} or
// {error}. A deployment as GET …/deployments/{n} answers it, and a draft
// this builder exported, give their spec: a stored spec is valid input.
export function parseSpecText(text) {
  let v;
  try {
    v = JSON.parse(text);
  } catch (e) {
    return { error: `not JSON: ${e.message}` };
  }
  if (!isObject(v)) return { error: 'the spec is a JSON object' };
  if (isObject(v.spec) && (typeof v.number === 'number' || typeof v.base === 'number')) return { spec: v.spec };
  return { spec: v };
}

export function cloneSpec(spec) {
  return JSON.parse(JSON.stringify(spec || {}));
}

// objectsOf lists a kind's objects as [name, object], by name.
export function objectsOf(spec, key) {
  const m = spec && isObject(spec[key]) ? spec[key] : {};
  return Object.entries(m).sort(([a], [b]) => a.localeCompare(b));
}

export function getObject(spec, key, name) {
  const m = spec && isObject(spec[key]) ? spec[key] : null;
  return m && Object.prototype.hasOwnProperty.call(m, name) ? m[name] : undefined;
}

// setObject is spec with the object key/name set (added at the end of its
// map, or replaced where it is).
export function setObject(spec, key, name, obj) {
  const out = { ...(spec || {}) };
  out[key] = { ...(isObject(out[key]) ? out[key] : {}), [name]: obj };
  return out;
}

// removeObject is spec without key/name; a map left empty goes too.
export function removeObject(spec, key, name) {
  const out = { ...(spec || {}) };
  if (!isObject(out[key])) return out;
  const m = { ...out[key] };
  delete m[name];
  if (Object.keys(m).length === 0) delete out[key];
  else out[key] = m;
  return out;
}

// renameKey is map with key from renamed to, in its place.
export function renameKey(map, from, to) {
  const out = {};
  for (const [k, v] of Object.entries(map || {})) out[k === from ? to : k] = v;
  return out;
}

// mapEnvs applies fn to every env value of the services and jobs (and the
// image credentials' fields, which take the same forms) and returns the
// new spec; fn returns the value, or undefined to drop a variable.
export function mapEnvValues(spec, fn) {
  let out = spec;
  for (const key of ['services', 'jobs']) {
    for (const [name, obj] of objectsOf(spec, key)) {
      if (!isObject(obj)) continue;
      let next = obj;
      if (isObject(obj.env)) {
        const env = {};
        let changed = false;
        for (const [k, v] of Object.entries(obj.env)) {
          const nv = fn(v, { key, name, field: `env.${k}` });
          if (nv !== v) changed = true;
          if (nv !== undefined) env[k] = nv;
        }
        if (changed) next = { ...next, env };
      }
      if (isObject(obj.imageCredentials)) {
        const c = { ...obj.imageCredentials };
        let changed = false;
        for (const f of ['username', 'password']) {
          if (!(f in c)) continue;
          const nv = fn(c[f], { key, name, field: `imageCredentials.${f}` });
          if (nv !== c[f]) { changed = true; c[f] = nv; }
        }
        if (changed) {
          next = { ...next };
          if (c.username === undefined || c.password === undefined) delete next.imageCredentials;
          else next.imageCredentials = c;
        }
      }
      if (next !== obj) out = setObject(out, key, name, next);
    }
  }
  return out;
}

// renameObject renames key/from to to and the references to it: a
// service's in the routes and the variables, an addon's and a secret's in
// the variables, a binding's in the services' and jobs' lists, a security
// group's in the services', jobs' and addons' lists and in the groups'
// rules.
export function renameObject(spec, key, from, to) {
  if (from === to || !isObject(spec[key]) || !(from in spec[key])) return spec;
  let out = { ...spec, [key]: renameKey(spec[key], from, to) };
  if (key === 'services') {
    for (const [rn, r] of objectsOf(out, 'routes')) {
      if (isObject(r) && r.service === from) out = setObject(out, 'routes', rn, { ...r, service: to });
    }
    out = mapEnvValues(out, (v) => (isObject(v) && v.service === from && !('addon' in v) ? { ...v, service: to } : v));
  }
  if (key === 'addons') {
    out = mapEnvValues(out, (v) => (isObject(v) && v.addon === from ? { ...v, addon: to } : v));
  }
  if (key === 'secrets') {
    out = mapEnvValues(out, (v) => (isObject(v) && v.secret === from && Object.keys(v).length === 1 ? { secret: to } : v));
  }
  if (key === 'bindings') {
    for (const k of ['services', 'jobs']) {
      for (const [n, o] of objectsOf(out, k)) {
        if (isObject(o) && Array.isArray(o.bindings) && o.bindings.includes(from)) {
          out = setObject(out, k, n, { ...o, bindings: o.bindings.map((b) => (b === from ? to : b)) });
        }
      }
    }
  }
  if (key === 'securityGroups') {
    for (const k of ['services', 'jobs', 'addons']) {
      for (const [n, o] of objectsOf(out, k)) {
        if (isObject(o) && Array.isArray(o.securityGroups) && o.securityGroups.includes(from)) {
          out = setObject(out, k, n, { ...o, securityGroups: o.securityGroups.map((g) => (g === from ? to : g)) });
        }
      }
    }
    for (const [n, g] of objectsOf(out, 'securityGroups')) {
      const next = mapRulePeers(g, (p) => (isObject(p) && p.group === from ? { ...p, group: to } : p));
      if (next !== g) out = setObject(out, 'securityGroups', n, next);
    }
  }
  return out;
}

// mapRulePeers applies fn to every peer of a security group's rules (in:
// from, out: to) and returns the group, the same object when fn changed
// nothing; fn returns the peer, or null to drop its rule.
export function mapRulePeers(g, fn) {
  if (!isObject(g)) return g;
  let out = g;
  for (const [dir, at] of [['in', 'from'], ['out', 'to']]) {
    if (!Array.isArray(g[dir])) continue;
    let changed = false;
    const rules = [];
    for (const r of g[dir]) {
      if (!isObject(r)) { rules.push(r); continue; }
      const p = fn(r[at]);
      if (p === null) { changed = true; continue; }
      if (p !== r[at]) changed = true;
      rules.push(p !== r[at] ? { ...r, [at]: p } : r);
    }
    if (changed) out = { ...out, [dir]: rules };
  }
  return out;
}

// DNS_LABEL is an object's name: lower case letters, digits and '-', at
// most 63, not starting or ending with '-'.
const DNS_LABEL = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

export function isDnsLabel(s) {
  return DNS_LABEL.test(String(s || ''));
}

// SECRET_NAME is a secret's: lower case letters, digits, '.', '_' and
// '-'; "addon." is the platform's.
const SECRET_NAME = /^[a-z0-9][a-z0-9._-]{0,252}$/;

export function isSecretName(s) {
  const v = String(s || '');
  return SECRET_NAME.test(v) && !v.startsWith('addon.');
}

// uniqueName is base, or base-2, base-3, … : the first name the kind does
// not have (a service and an addon share their names, so both count).
export function uniqueName(spec, key, base) {
  const taken = new Set(objectsOf(spec, key).map(([n]) => n));
  if (key === 'services' || key === 'addons') {
    for (const [n] of objectsOf(spec, key === 'services' ? 'addons' : 'services')) taken.add(n);
  }
  const b = String(base || key.slice(0, -1)).toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, '') || 'x';
  if (!taken.has(b)) return b;
  for (let i = 2; ; i++) if (!taken.has(`${b}-${i}`)) return `${b}-${i}`;
}

// addService adds an image service with a port named http; the first
// service of a spec without routes gets the route / too (ADR 0032, "A
// first deployment is a minute's work"). It returns {spec, name}.
export function addService(spec, image = '', port = 8080) {
  const name = uniqueName(spec, 'services', image ? imageBaseName(image) : 'web');
  let out = setObject(spec, 'services', name, { image, ports: [{ name: 'http', port }] });
  if (objectsOf(spec, 'services').length === 0 && objectsOf(spec, 'routes').length === 0) {
    out = setObject(out, 'routes', 'root', { path: '/', service: name, port: 'http' });
  }
  return { spec: out, name };
}

// imageBaseName is an image reference's last path segment without tag or
// digest: "ghcr.io/acme/web:1.2" is web.
export function imageBaseName(image) {
  const s = String(image || '').split('@')[0];
  const last = s.split('/').pop() || '';
  return last.split(':')[0];
}

// registryHost is the registry of an image reference: its first segment
// when it names a host (a dot, a port or localhost), else docker.io.
export function registryHost(image) {
  const s = String(image || '');
  const i = s.indexOf('/');
  if (i < 0) return 'docker.io';
  const first = s.slice(0, i);
  return first.includes('.') || first.includes(':') || first === 'localhost' ? first : 'docker.io';
}

// proposeMessage is the apply's message, from the plan's diff: "web:
// image nginx:1.27 → nginx:1.28; route /api added". Images show as
// written, without their digests.
export function proposeMessage(diff, spec) {
  const parts = [];
  for (const d of diff || []) {
    const what = d.kind === 'route' ? routeWord(spec, d.name) : `${d.kind} ${d.name}`;
    if (d.change === 'add') parts.push(`${what} added`);
    else if (d.change === 'remove') parts.push(`${what} removed`);
    else {
      const fields = d.fields || [];
      if (fields.length === 0) parts.push(`${what} changed`);
      else if (fields.length > 2) parts.push(`${d.name}: ${fields.length} fields`);
      else parts.push(`${d.name}: ${fields.map((f) => `${f.path} ${short(f.from)} → ${short(f.to)}`).join(', ')}`);
    }
  }
  const s = parts.join('; ');
  return s.length > 300 ? s.slice(0, 299) + '…' : s;
}

function routeWord(spec, name) {
  const r = getObject(spec, 'routes', name);
  return isObject(r) && r.path ? `route ${r.path}` : `route ${name}`;
}

// short renders a diff value for a message: a pinned image as written,
// absent as "none".
function short(v) {
  if (v === null || v === undefined) return 'none';
  if (typeof v !== 'string') return JSON.stringify(v);
  return v.replace(/ \(sha256:[0-9a-f]{64}\)$/, '').replace(/@sha256:([0-9a-f]{12})[0-9a-f]{52}$/, '@sha256:$1…');
}

// ------------------------------------------------------------- storage --

// DRAFT_PREFIX keys the drafts in local storage, one per tenant and
// environment and one slot per tenant for a new environment.
export const DRAFT_PREFIX = 'ferun-draft:';

export function draftKey(tenant, env) {
  return `${DRAFT_PREFIX}${tenant}/${env || ''}`;
}

// encodeDraft is what storage keeps: the spec, the base, where the draft
// came from and, for a new environment, its name and the ID it is
// created with (so that a create repeated after a failed step is the
// same environment). Never a secret's value: the draft holds none.
export function encodeDraft(d) {
  const out = { v: 1, spec: d.spec || {}, base: d.base || 0, source: d.source || '', name: d.name || '' };
  if (d.id) out.id = d.id;
  return JSON.stringify(out);
}

// decodeDraft reads it back; null for anything else.
export function decodeDraft(text) {
  if (!text) return null;
  try {
    const v = JSON.parse(text);
    if (!isObject(v) || v.v !== 1 || !isObject(v.spec) || typeof v.base !== 'number') return null;
    const d = { spec: v.spec, base: v.base, source: String(v.source || ''), name: String(v.name || '') };
    if (typeof v.id === 'string' && v.id) d.id = v.id;
    return d;
  } catch {
    return null;
  }
}

// newEnvID is a new environment's UUID (version 4), which the builder
// sends with its create.
export function newEnvID(random = (b) => globalThis.crypto.getRandomValues(b)) {
  const b = random(new Uint8Array(16));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

// ---------------------------------------------------------------- words --

// draftWords say what a draft is, for the builder's band: the deployment
// it becomes, the one it starts from with that one's state, whether a
// newer one exists, and how many objects it changes. latest is the
// environment's latest deployment, deployed the one that serves, state
// the latest's rollout state.
export function draftWords({ isNew = false, readOnly = false, base = 0, latest = 0, deployed = 0, state = '', changes = 0 }) {
  const count = changes === 0 ? 'no changes yet' : `${changes} change${changes === 1 ? '' : 's'}`;
  if (isNew) return { title: 'Draft of deployment 1 of a new environment', from: '', behind: 0, count, next: 1 };
  if (readOnly) {
    return { title: latest ? `Deployment ${latest}` : 'No deployment yet', from: 'read-only', behind: 0, count: '', next: latest + 1 };
  }
  const baseState = base === deployed ? 'deployed' : base === latest ? state || 'latest' : 'replaced';
  const from = base > 0 ? `starts from deployment ${base} (${baseState})` : 'starts from an empty spec';
  return { title: `Draft of deployment ${latest + 1}`, from, behind: latest > base ? latest : 0, count, next: latest + 1 };
}

// deployBlock is why a draft cannot be deployed yet, '' when it can:
// in the order a person fixes them.
export function deployBlock({ isNew = false, name = '', planning = false, answer = null, diff = [], unresolved = [],
  heavy = 0, ack = false, conflicts = 0, busy = false }) {
  if (busy) return 'deploying…';
  if (isNew && !isDnsLabel(name)) return 'name the environment first';
  if (conflicts > 0) return 'decide the conflicts first';
  if (planning || !answer) return 'planning…';
  if (!answer.valid) return 'the plan has errors: fix them first';
  if (!isNew && diff.length === 0) return 'no changes to deploy';
  if (unresolved.length > 0) return 'images that do not resolve';
  if (heavy > 0 && !ack) return 'accept the data loss or downtime first';
  return '';
}
