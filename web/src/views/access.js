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
// is set, missing (named, no value) or unused (no one names it). A
// secret's row has the version the spec pins (pinned, 0 for none); a
// credential's the sandboxes that name it (sandboxes, IDs) and the
// versions its deployments pin (pins: {latest, deployed}); an addon's
// uses say each user's mark and fields (its role, its database).
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
    const pin = spec && isObject(spec.secrets) && isObject(spec.secrets[name]) ? Number(spec.secrets[name].version) || 0 : 0;
    rows.push({ kind: 'secret', name, marks: marksOf(uses), users: users(uses), uses,
      state: !s ? 'missing' : uses.length === 0 ? 'unused' : 'set', version: s ? s.version : 0, pinned: pin });
  }

  const bindings = objectsOf(spec, 'bindings').map(([n]) => n);
  for (const c of [...(credentials || [])].sort((a, b) => a.name.localeCompare(b.name))) {
    const via = bindings.filter((n) => {
      const b = spec.bindings[n];
      return isObject(b) && isObject(b.credentialRef) && b.credentialRef.name === c.name;
    });
    const objs = uniqueFrom(refs.filter((r) => r.via === 'bindings' && via.includes(r.to.name)).map((r) => r.from));
    const used = isObject(c.usedBy) ? c.usedBy : {};
    const sandboxes = Array.isArray(used.sandboxes) ? [...used.sandboxes].sort() : [];
    rows.push({ kind: 'credential', name: c.name, marks: ['offloaded'], users: objs, bindings: via, sandboxes,
      pins: { latest: Number(used.latest) || 0, deployed: Number(used.deployed) || 0 },
      state: via.length === 0 && sandboxes.length === 0 ? 'unused' : 'set', version: c.version || 0 });
  }

  for (const n of bindings) {
    const objs = uniqueFrom(refs.filter((r) => r.via === 'bindings' && r.to.name === n).map((r) => r.from));
    rows.push({ kind: 'binding', name: n, marks: ['offloaded'], users: objs, binding: bindingOf(spec, n, credentials),
      state: objs.length === 0 ? 'unused' : 'set' });
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
    rows.push({ kind: 'addon', name: n, marks: marks.size ? MARKS.filter((m) => marks.has(m)) : ['identity'], users: objs, uses,
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

// INVENTORY_GROUPS are the inventory's groups, in the order the Secrets
// view shows them.
export const INVENTORY_GROUPS = [
  { kind: 'secret', title: 'Secrets' },
  { kind: 'credential', title: 'Credentials' },
  { kind: 'binding', title: 'HTTP bindings' },
  { kind: 'addon', title: 'Addons' },
];

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
// "credential github v3" (its latest after it when the environment's
// credential has a later one), "secret x", "secrets x, y", or ''.
export function bindingSource(b, credentials = []) {
  if (b && b.credential) {
    const c = (credentials || []).find((x) => x && x.name === b.credential.name);
    const v = b.credential.version;
    const later = c && v && c.version > v ? `, latest v${c.version}` : '';
    return `credential ${b.credential.name}${v ? ` v${v}` : ''}${later}`;
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

// strayUsage are the bindings a usage answer counts that spec has none
// of: those the pods named themselves, a sandbox's (its environment's
// credentials it names). Only the ones with requests.
export function strayUsage(usage, spec) {
  if (!isObject(usage) || usage.unavailable || !Array.isArray(usage.bindings)) return [];
  const own = new Set(objectsOf(spec, 'bindings').map(([n]) => n));
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

// secretPinNote says a secret the latest deployment pins at an earlier
// version than its latest: "the latest deployment pins v2, latest v3";
// '' when it pins the latest or none.
export function secretPinNote(pinned, version) {
  const p = Number(pinned) || 0;
  const v = Number(version) || 0;
  return p > 0 && v > p ? `the latest deployment pins v${p}, latest v${v}` : '';
}

// credentialPinNote says a credential its deployments pin behind its
// latest version (pins: its row's {latest, deployed}): "deployed pins
// v3, latest v4", "v4 rolling out; deployed pins v3"; '' when they pin
// the latest or none.
export function credentialPinNote(pins, version) {
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

// credentialBinding is a sandbox's binding by an environment's
// credential c, as bindingOf shows a spec's (a sandbox names a
// credential by its name, ADR 0033).
export function credentialBinding(c) {
  const ref = { name: c.name, version: c.version || 0 };
  return bindingOf({ bindings: { [c.name]: { credentialRef: ref } } }, c.name, [c]);
}

// upstreamRow is a binding's row on the right of the reachability
// picture (reach.js's rows): its upstream's host as the peer (the binding
// and how the pod reaches it below), the key's kind on the edge, the mark
// offloaded. usage is bindingUsage's entry of the binding, or null;
// self ({kind, name}) the object or pod the row is of, whose share of
// the requests its users say (mine, of by; null without self or users).
export function upstreamRow(b, usage = null, self = null) {
  const host = hostOf(b.upstream);
  const users = usage && Array.isArray(usage.users) ? usage.users : null;
  const own = self && users ? users.find((u) => isObject(u) && u.kind === self.kind && u.name === self.name) : null;
  return {
    key: `binding:${b.name}`,
    peer: { type: 'upstream', name: host || b.name, sub: `${b.name} · as ${b.reachedAs}` },
    what: keyAtNode(b.kind),
    via: [`binding ${b.name}`],
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
// per binding it names (upstreamRow), before its egress, and a sandbox's
// one per credential of the environment it holds. Rows between services
// stay as they are: no key goes there. usage is bindingUsage's bindings,
// or null for no numbers. marks are the marks the picture shows.
//
// The numbers are matched by the binding's name: bindingUsage counts per
// name, so a sandbox's credential and a deployment's binding of the same
// name share theirs.
export function withKeys(model, spec, { catalog = null, credentials = [], usage = null } = {}) {
  const refs = referencesOf(spec);
  const used = new Map((Array.isArray(usage) ? usage : []).filter((u) => isObject(u) && u.name).map((u) => [u.name, u]));
  const shown = new Set();
  const rowsOf = new Map();
  const access = (kind, name) => {
    const ref = `${kind}/${name}`;
    if (!rowsOf.has(ref)) rowsOf.set(ref, SELF_KEY[kind] ? accessOf(spec, SELF_KEY[kind], name, { catalog, credentials, refs }) : []);
    return rowsOf.get(ref);
  };
  const marked = (r, kind, name, addon) => {
    const a = access(kind, name).find((x) => x.kind === 'addon' && x.target.name === addon);
    if (!a || !a.mark) return r;
    shown.add(a.mark);
    return { ...r, mark: a.mark };
  };
  const withUpstreams = (outs, bindings, self) => {
    if (!bindings.length) return outs;
    shown.add('offloaded');
    const rows = bindings.map((b) => upstreamRow(b, used.get(b.name) || null, self));
    const at = outs.findIndex((r) => EGRESS_PEERS.has(r.peer.type));
    return at < 0 ? [...outs, ...rows] : [...outs.slice(0, at), ...rows, ...outs.slice(at)];
  };
  const objects = model.objects.map((o) => {
    if (SELF_KEY[o.kind]) {
      const outs = o.outs.map((r) => (r.peer.type === 'addon' ? marked(r, o.kind, o.name, r.peer.name) : r));
      const bindings = access(o.kind, o.name).filter((x) => x.kind === 'binding').map((x) => x.binding);
      return { ...o, outs: withUpstreams(outs, bindings, { kind: o.kind, name: o.name }) };
    }
    if (o.kind === 'addon') {
      return { ...o, ins: o.ins.map((r) => (SELF_KEY[r.peer.type] ? marked(r, r.peer.type, r.peer.name, o.name) : r)) };
    }
    return o;
  });
  const pods = model.pods.map((p) => {
    if (p.kind !== 'sandbox') return p;
    const held = (credentials || []).filter((c) => isObject(c) && isObject(c.usedBy) && Array.isArray(c.usedBy.sandboxes)
      && c.usedBy.sandboxes.includes(p.name)).sort((a, b) => a.name.localeCompare(b.name));
    return { ...p, outs: withUpstreams(p.outs, held.map(credentialBinding), { kind: 'sandbox', name: p.name }) };
  });
  return { ...model, objects, pods, marks: MARKS.filter((m) => shown.has(m)) };
}
