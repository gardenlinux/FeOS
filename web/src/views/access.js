// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Where a key lives: the access of a service or job, and the inventory of
// an environment's secrets, sorted by the place of the key, not by where
// it is stored. Three marks (ADR 0014, 0028, 0038):
//   identity  — no key: the workload's certificate is enough (an addon
//               that admits a workload by it, through the pod's TLS
//               binding).
//   offloaded — the key stays at the node, which adds it (an HTTP
//               binding; a registry login, which the node pulls with).
//   container — the value is handed to the container (a variable from a
//               secret, an addon's password).
// Pure: the spec, the addon catalog and the environment's credentials in,
// rows out.

import { envValueKind, referencesOf } from './refs.js';
import { isObject, objectsOf } from './draft.js';

export const MARKS = ['identity', 'offloaded', 'container'];

export const MARK_LABEL = {
  identity: 'by identity',
  offloaded: 'offloaded',
  container: 'in the container',
};

export const MARK_TITLE = {
  identity: "No key: the workload's certificate is enough.",
  offloaded: 'The key stays at the node, which adds it to each request. The container never sees it.',
  container: 'The value is handed to the container.',
};

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

// bindingOf is a binding of spec as the access rows show it, with what a
// credential of the environment (credentialRef) states filled in from
// credentials: {name, upstream, mode, kind, reachedAs, credential:
// {name, version} | null, secrets}.
export function bindingOf(spec, name, credentials = []) {
  const b = spec && isObject(spec.bindings) && isObject(spec.bindings[name]) ? spec.bindings[name] : {};
  const ref = isObject(b.credentialRef) ? b.credentialRef : null;
  const cred = ref ? (credentials || []).find((c) => c && c.name === ref.name) || null : null;
  const upstream = b.upstream || (cred && cred.upstream) || '';
  const mode = b.mode || (cred && cred.mode) || 'name';
  const kind = (isObject(b.credential) && b.credential.kind) || (cred && (cred.kind || (cred.credential && cred.credential.kind))) || '';
  return {
    name,
    upstream,
    mode,
    kind,
    reachedAs: mode === 'intercept' ? hostOf(upstream) : `http://${name}`,
    credential: ref ? { name: ref.name, version: ref.version || 0 } : null,
    secrets: bindingSecrets(b),
  };
}

function hostOf(upstream) {
  try {
    return new URL(upstream).host;
  } catch {
    return upstream;
  }
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

// accessOf lists what the object key/name ('services' or 'jobs') of spec
// reaches with a key or without one, one row per target:
// {target: {key, name}, mark, kind, variables, keys, binding?, addon?,
// fields?}. kind is binding, addon, secret or image. Sorted by mark,
// then name.
export function accessOf(spec, key, name, { catalog = null, credentials = [], refs = referencesOf(spec) } = {}) {
  const own = refs.filter((r) => r.from.key === key && r.from.name === name);
  const rows = [];
  for (const r of own.filter((x) => x.via === 'bindings')) {
    rows.push({ target: { key: 'bindings', name: r.to.name }, kind: 'binding', mark: 'offloaded', variables: [], keys: [],
      binding: bindingOf(spec, r.to.name, credentials) });
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
  const secrets = new Map();
  for (const r of own.filter((x) => x.via === 'env' && x.to.key === 'secrets')) {
    secrets.set(r.to.name, [...(secrets.get(r.to.name) || []), r.variable]);
  }
  for (const [secret, vars] of secrets) {
    rows.push({ target: { key: 'secrets', name: secret }, kind: 'secret', mark: 'container', variables: vars.sort(), keys: [] });
  }
  const pull = [...new Set(own.filter((x) => x.via === 'imageCredentials').map((x) => x.to.name))];
  for (const secret of pull) {
    rows.push({ target: { key: 'secrets', name: secret }, kind: 'image', mark: 'offloaded', variables: [], keys: [] });
  }
  return rows.sort(byMark);
}

// secretUses are the uses of the environment's secret name in spec:
// [{from: {key, name}, mark, how, variable?, binding?}] — a variable
// (in the container), a binding's credential (offloaded, for the objects
// that name the binding) or a registry login (offloaded).
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

// inventoryOf lists everything of an environment a workload reaches with
// a key or by its identity: its secrets (those with a value, secrets, and
// those the spec names), its credentials, its bindings and the addons
// that admit by identity or hand out a password. Each row: {kind, name,
// marks, users: [{key, name}], uses?, state, version?, binding?}. state
// is set, missing (named, no value) or unused (no one names it).
export function inventoryOf(spec, { catalog = null, credentials = [], secrets = [] } = {}) {
  const refs = referencesOf(spec);
  const rows = [];
  const users = (uses) => uniqueFrom(uses.map((u) => u.from).filter((f) => f.key === 'services' || f.key === 'jobs'));

  const known = new Map((secrets || []).map((s) => [s.name, s]));
  const names = new Set([...known.keys(), ...objectsOf(spec, 'secrets').map(([n]) => n)]);
  for (const r of refs) if (r.to.key === 'secrets') names.add(r.to.name);
  for (const name of [...names].sort()) {
    const uses = secretUses(spec, name, refs);
    const s = known.get(name);
    rows.push({ kind: 'secret', name, marks: marksOf(uses), users: users(uses), uses,
      state: !s ? 'missing' : uses.length === 0 ? 'unused' : 'set', version: s ? s.version : 0 });
  }

  const bindings = objectsOf(spec, 'bindings').map(([n]) => n);
  for (const c of [...(credentials || [])].sort((a, b) => a.name.localeCompare(b.name))) {
    const via = bindings.filter((n) => {
      const b = spec.bindings[n];
      return isObject(b) && isObject(b.credentialRef) && b.credentialRef.name === c.name;
    });
    const objs = uniqueFrom(refs.filter((r) => r.via === 'bindings' && via.includes(r.to.name)).map((r) => r.from));
    rows.push({ kind: 'credential', name: c.name, marks: ['offloaded'], users: objs, bindings: via,
      state: via.length === 0 ? 'unused' : 'set', version: c.version || 0 });
  }

  for (const n of bindings) {
    const objs = uniqueFrom(refs.filter((r) => r.via === 'bindings' && r.to.name === n).map((r) => r.from));
    rows.push({ kind: 'binding', name: n, marks: ['offloaded'], users: objs, binding: bindingOf(spec, n, credentials),
      state: objs.length === 0 ? 'unused' : 'set' });
  }

  for (const [n, a] of objectsOf(spec, 'addons')) {
    const bp = catalog && catalog.byType && isObject(a) ? catalog.byType.get(a.type) : null;
    const objs = [];
    const marks = new Set();
    for (const [key, objName] of uniqueFrom(refs.filter((r) => r.to.key === 'addons' && r.to.name === n).map((r) => r.from))
      .map((f) => [f.key, f.name])) {
      const row = accessOf(spec, key, objName, { catalog, refs }).find((x) => x.kind === 'addon' && x.target.name === n);
      if (row && row.mark) {
        marks.add(row.mark);
        objs.push({ key, name: objName });
      }
    }
    if (marks.size === 0 && !(bp && bp.byIdentity)) continue;
    rows.push({ kind: 'addon', name: n, marks: marks.size ? MARKS.filter((m) => marks.has(m)) : ['identity'], users: objs,
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
