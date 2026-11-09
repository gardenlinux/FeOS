// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Reachability from declarations (ADR 0034, revised; ADR 0035): what a
// deployment's references open, as the plan and a deployment answer it
// (`reachability`); a service's and a job's egress; a route's clients
// (from); a service's or a job's uses; what a sandbox or a job made by a call
// reaches of its environment (reaches); and the environment a sandbox or
// a job is in. Pure: no React, no browser.
//
// The shapes are ferund's:
//
//   spec egress: "internet" (left out) | "none" |
//                {domains: ["api.stripe.com", "*.example.com"],
//                 blocks: [{cidr, except: [...]}]}      ({} is none)
//   spec uses:   [{service, port}, {addon}]
//   route from:  ["203.0.113.0/24", "2001:db8::/32"]    (left out: every client)
//   reaches:     "all" | [{service, port}, {addon}]     (left out or []: nothing)
//
//   reachability: {
//     edges:  [{from, to, port, number, via}],   from/to "<kind>/<name>" or "ingress",
//                                                via "env <VAR>", "uses" or "route <name>"
//     egress: [{object, mode: internet|none|restricted, domains, blocks}],
//     routes: [{route, service, port, from}],
//   }

import { isObject, objectsOf } from './draft.js';

// HOW_WORDS say how reachability works, in a line.
export const HOW_WORDS = 'What an object references, it reaches; nothing else gets in. Egress is the service\'s choice.';

// shortList is a list in a line: at most three, then "+n".
export function shortList(list, n = 3) {
  const u = [...new Set(list)];
  return u.length <= n ? u.join(', ') : `${u.slice(0, n).join(', ')} +${u.length - n}`;
}

// --------------------------------------------------------------- egress --

// egressMode says an egress: internet (left out or "internet"), none
// ("none"), only (the object form, its domains and blocks) or other.
export function egressMode(e) {
  if (e === undefined || e === null || e === 'internet') return 'internet';
  if (e === 'none') return 'none';
  if (isObject(e)) return 'only';
  return 'other';
}

// egressParts are the object form's lists: {domains, blocks}, [] each
// when absent.
export function egressParts(e) {
  const o = isObject(e) ? e : {};
  return {
    domains: Array.isArray(o.domains) ? o.domains.filter((d) => typeof d === 'string') : [],
    blocks: Array.isArray(o.blocks) ? o.blocks.filter(isObject) : [],
  };
}

// withEgressMode is the egress a switch to mode makes of e: internet
// leaves the field out (the default), none is "none", only keeps e's
// object or starts an empty one ({} is none until a domain or a block
// is in it).
export function withEgressMode(e, mode) {
  if (mode === 'internet') return undefined;
  if (mode === 'none') return 'none';
  return isObject(e) ? e : {};
}

// withEgressList is the object form with key (domains or blocks) set to
// list; an empty list goes.
export function withEgressList(e, key, list) {
  const out = { ...(isObject(e) ? e : {}) };
  if (Array.isArray(list) && list.length) out[key] = list;
  else delete out[key];
  return out;
}

// blockWords says an address block: "203.0.113.0/24", "203.0.113.0/24
// except 203.0.113.8/29".
export function blockWords(b) {
  const ex = isObject(b) && Array.isArray(b.except) && b.except.length ? ` except ${b.except.join(', ')}` : '';
  return `${(isObject(b) && b.cidr) || '?'}${ex}`;
}

// egressWords say an egress in a few words: "the internet", "nothing
// outside", "only api.stripe.com, 203.0.113.0/24".
export function egressWords(e) {
  const mode = egressMode(e);
  if (mode === 'internet') return 'the internet';
  if (mode === 'none') return 'nothing outside';
  if (mode === 'other') return JSON.stringify(e);
  const { domains, blocks } = egressParts(e);
  if (!domains.length && !blocks.length) return 'nothing outside';
  return `only ${shortList([...domains, ...blocks.map((b) => b.cidr || '?')])}`;
}

// egressOfAnswer is the spec's form of an answer's egress entry
// ({mode, domains, blocks}): "internet", "none" or the object form.
export function egressOfAnswer(x) {
  if (!isObject(x)) return undefined;
  if (x.mode === 'none') return 'none';
  if (x.mode === 'restricted') {
    return withEgressList(withEgressList({}, 'domains', x.domains), 'blocks', x.blocks);
  }
  return 'internet';
}

const LABEL = '[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?';
const DOMAIN = new RegExp(`^(\\*\\.)?(${LABEL}\\.)*${LABEL}$`);

// isEgressDomain tells an egress's domain: a lowercase DNS name, a
// wildcard "*." only at its front.
export function isEgressDomain(s) {
  const v = String(s || '');
  return v.length <= 253 && DOMAIN.test(v);
}

// isPrefix tells an address block of either family, "203.0.113.0/24" or
// "2001:db8::/32". The plan has the last word (the private ranges, a
// host part set).
export function isPrefix(s) {
  const m = /^([^/]+)\/(\d{1,3})$/.exec(String(s || '').trim());
  if (!m) return false;
  const bits = Number(m[2]);
  const addr = m[1];
  if (addr.includes('.') && !addr.includes(':')) {
    const parts = addr.split('.');
    return bits <= 32 && parts.length === 4 && parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) <= 255);
  }
  if (!addr.includes(':') || bits > 128) return false;
  const halves = addr.split('::');
  if (halves.length > 2) return false;
  const groups = halves.flatMap((h) => (h ? h.split(':') : []));
  if (!groups.every((g) => /^[0-9a-fA-F]{1,4}$/.test(g))) return false;
  return halves.length === 2 ? groups.length < 8 : groups.length === 8;
}

// egressProblems are what the plan would refuse of an egress, in words:
// a domain that is no name, a block that is no prefix, an exception that
// is none. [] for internet and none.
export function egressProblems(e) {
  if (egressMode(e) === 'other') return ['egress is "internet", "none" or {domains, blocks}'];
  if (egressMode(e) !== 'only') return [];
  const { domains, blocks } = egressParts(e);
  const out = [];
  const badD = domains.filter((d) => !isEgressDomain(d));
  if (badD.length) out.push(`not a domain: ${badD.join(', ')} (lower case; a wildcard only as "*." in front)`);
  const badB = blocks.filter((b) => !isPrefix(b.cidr)).map((b) => b.cidr || '(empty)');
  if (badB.length) out.push(`not an address block: ${badB.join(', ')}`);
  const badE = blocks.flatMap((b) => (Array.isArray(b.except) ? b.except : [])).filter((x) => !isPrefix(x));
  if (badE.length) out.push(`not an address block: ${badE.join(', ')} (except)`);
  return out;
}

// ----------------------------------------------------------------- uses --

// useWords says a uses entry: "catalog:http", "db".
export function useWords(u) {
  if (isObject(u) && typeof u.service === 'string') return `${u.service}${u.port !== undefined && u.port !== '' ? `:${u.port}` : ''}`;
  if (isObject(u) && typeof u.addon === 'string') return u.addon;
  return JSON.stringify(u);
}

// portsOfService are a service's ports as a reference names them: the
// name, or the number of one without a name; with the number.
export function portsOfService(s) {
  return (isObject(s) && Array.isArray(s.ports) ? s.ports : []).filter(isObject)
    .map((p) => ({ ref: p.name || p.port, number: p.port }))
    .filter((p) => p.ref !== undefined && p.ref !== '');
}

// usesTargets are what a uses entry of self (a service's name, or none
// for a job) may name: the other services with their ports, the addons.
export function usesTargets(spec, self = '') {
  return {
    services: objectsOf(spec, 'services').filter(([n, s]) => n !== self && isObject(s))
      .map(([name, s]) => ({ name, ports: portsOfService(s).map((p) => p.ref), attached: s.runtime === 'attached' })),
    addons: objectsOf(spec, 'addons').map(([name, a]) => ({ name, type: isObject(a) ? a.type || '' : '' })),
  };
}

// ------------------------------------------------------------ route from --

// fromWords say a route's clients: "every client", or its blocks.
export function fromWords(from) {
  const l = Array.isArray(from) ? from : [];
  return l.length ? shortList(l) : 'every client';
}

// fromProblems are a route's from entries that are no address block.
export function fromProblems(from) {
  const bad = (Array.isArray(from) ? from : []).filter((x) => !isPrefix(x));
  return bad.length ? [`not an address block: ${bad.join(', ')}`] : [];
}

// -------------------------------------------------------------- reaches --

// reachesOf is what a sandbox or a job reaches: "all", or its list ([]
// when nothing).
export function reachesOf(p) {
  const r = p && p.reaches;
  if (r === 'all') return 'all';
  return Array.isArray(r) ? r.filter(isObject) : [];
}

// reachKey identifies a reaches or a uses entry: "service:api:http",
// "addon:db".
export function reachKey(e) {
  if (isObject(e) && typeof e.service === 'string') return `service:${e.service}:${e.port ?? ''}`;
  if (isObject(e) && typeof e.addon === 'string') return `addon:${e.addon}`;
  return JSON.stringify(e);
}

// reachesWords say what a sandbox or a job reaches: "all of the
// environment", "nothing of the environment", "api:http, db".
export function reachesWords(r) {
  if (r === 'all') return 'all of the environment';
  const l = Array.isArray(r) ? r : [];
  return l.length ? shortList(l.map(useWords)) : 'nothing of the environment';
}

// reachesMode is the switch of a reaches: none, all or some.
export function reachesMode(r) {
  if (r === 'all') return 'all';
  return Array.isArray(r) && r.length ? 'some' : 'none';
}

// reachChoices are what a sandbox or a job may reach in an environment,
// from its deployed deployment's spec: a row per service port and per
// addon, {key, value, name, detail}.
export function reachChoices(spec) {
  const out = [];
  for (const [name, s] of objectsOf(spec, 'services')) {
    for (const p of portsOfService(s)) {
      const detail = [String(p.ref) !== String(p.number) ? p.number : '', isObject(s) && s.runtime === 'attached' ? 'attached' : '']
        .filter(Boolean).join(' · ');
      const value = { service: name, port: p.ref };
      out.push({ key: reachKey(value), value, kind: 'service', name: `${name}:${p.ref}`, detail: String(detail) });
    }
  }
  for (const [name, a] of objectsOf(spec, 'addons')) {
    const value = { addon: name };
    out.push({ key: reachKey(value), value, kind: 'addon', name, detail: isObject(a) ? a.type || '' : '' });
  }
  return out;
}

// toggleReach is list with entry e in or out, in the order of choices
// (entries the choices lack keep their place at the end).
export function toggleReach(list, e, on, choices = []) {
  const cur = (Array.isArray(list) ? list : []).filter((x) => reachKey(x) !== reachKey(e));
  const next = on ? [...cur, e] : cur;
  const rank = new Map(choices.map((c, i) => [c.key, i]));
  const at = (x) => (rank.has(reachKey(x)) ? rank.get(reachKey(x)) : choices.length);
  return next.map((x, i) => [x, i]).sort((a, b) => (at(a[0]) - at(b[0])) || (a[1] - b[1])).map(([x]) => x);
}

// sameReaches tells two reaches apart by their entries.
export function sameReaches(a, b) {
  if (a === 'all' || b === 'all') return a === b;
  const x = (Array.isArray(a) ? a : []).map(reachKey).sort();
  const y = (Array.isArray(b) ? b : []).map(reachKey).sort();
  return x.length === y.length && x.every((v, i) => v === y[i]);
}

// reachesValue is what a create sends for a form's switch and list:
// "all", the list, or undefined for nothing (left out).
export function reachesValue(mode, list) {
  if (mode === 'all') return 'all';
  if (mode === 'some' && Array.isArray(list) && list.length) return [...list];
  return undefined;
}

// missingReaches are the entries of r the choices lack: an object or a
// port the deployed deployment does not have (ADR 0035: they admit
// nothing, a create naming one is refused).
export function missingReaches(r, choices) {
  if (!Array.isArray(r) || !choices) return [];
  const have = new Set(choices.map((c) => c.key));
  return r.filter((e) => !have.has(reachKey(e)));
}

// ------------------------------------------------------------- the env --

// envRef is the environment a sandbox's or a job's view names: {id,
// name}, or null.
export function envRef(x) {
  const e = x && x.environment;
  if (isObject(e) && (e.id || e.name)) return { id: String(e.id || ''), name: String(e.name || e.id || '') };
  if (typeof e === 'string' && e) return { id: e, name: e };
  return null;
}

// inEnv tells whether a sandbox or a job is of env (its name or ID).
export function inEnv(x, env) {
  const r = envRef(x);
  return !!r && !!env && (r.id === env || r.name === env);
}

// inUse reads a refused environment delete (409 environment_in_use):
// {sandboxes, jobs}, the IDs that keep it; null for another refusal.
export function inUse(e) {
  const b = e && e.body;
  if (!e || e.status !== 409 || !b || b.code !== 'environment_in_use') return null;
  const ids = (v) => (Array.isArray(v) ? v.filter((x) => typeof x === 'string') : []);
  return { sandboxes: ids(b.sandboxes), jobs: ids(b.jobs) };
}

// attachedTo is the sandbox an attached service runs in, or '' for any
// other object: its pod is the sandbox's (ADR 0035).
export function attachedTo(kind, o) {
  return kind === 'service' && isObject(o) && o.runtime === 'attached' ? String(o.sandbox || '?') : '';
}

// --------------------------------------------------------- the picture --

// A picture says who reaches a pod and what it reaches: the rows on its
// left (in) and on its right (out), one per peer.
//
//   Row: {key, peer: {type, name, sub?, missing?}, what, via: [string]}
//
// type is service, job, addon (objects of the environment), ingress (the
// routes, sub their clients), sandbox, jobrun (a job made by a call), env
// (every pod of the environment: reaches "all"), internet, domain or
// cidr (egress). what says the ports: "http 8080", "postgres 5432, 5433";
// via where the edge comes from: "env CATALOG_URL", "uses", "route api",
// "reaches".

// refOf reads "<kind>/<name>" (or "ingress"): {kind, name}.
export function refOf(s) {
  const v = String(s || '');
  if (v === 'ingress') return { kind: 'ingress', name: 'ingress' };
  const i = v.indexOf('/');
  return i > 0 ? { kind: v.slice(0, i), name: v.slice(i + 1) } : { kind: 'other', name: v };
}

// portWords say an edge's port: "http 8080", or the number alone when
// the port has no other name.
export function portWords(port, number) {
  const p = port === undefined || port === null ? '' : String(port);
  const n = number === undefined || number === null || number === 0 ? '' : String(number);
  if (p && n && p !== n) return `${p} ${n}`;
  return p || n || 'its port';
}

// The order of a side's rows: from outside first on the left, the
// environment's objects first on the right, egress last.
const IN_ORDER = { ingress: 0, service: 1, job: 2, addon: 3, sandbox: 4, jobrun: 5, other: 6 };
const OUT_ORDER = { env: 0, service: 1, job: 2, addon: 3, other: 4, internet: 5, domain: 6, cidr: 7 };

function rowsBy(order) {
  const m = new Map();
  return {
    add(peer, what, via, key = `${peer.type}:${peer.name}:${peer.sub || ''}`) {
      const cur = m.get(key) || { key, peer, what: [], via: [] };
      if (what && !cur.what.includes(what)) cur.what.push(what);
      if (via && !cur.via.includes(via)) cur.via.push(via);
      if (peer.missing) cur.peer = { ...cur.peer, missing: true };
      m.set(key, cur);
    },
    list() {
      return [...m.values()]
        .map((r) => ({ ...r, what: r.what.join(', ') }))
        .sort((a, b) => ((order[a.peer.type] ?? 9) - (order[b.peer.type] ?? 9)) || a.peer.name.localeCompare(b.peer.name));
    },
  };
}

const PEER_KIND = { service: 'service', job: 'job', addon: 'addon' };

function peerOfRef(ref) {
  const { kind, name } = refOf(ref);
  return { type: PEER_KIND[kind] || 'other', name };
}

// egressRows are an egress's rows on the right: the internet, or its
// domains and blocks; none for none.
export function egressRows(e) {
  const mode = egressMode(e);
  if (mode === 'internet') return [{ key: 'internet', peer: { type: 'internet', name: 'Internet' }, what: 'all traffic', via: ['egress'] }];
  if (mode !== 'only') return [];
  const { domains, blocks } = egressParts(e);
  return [
    ...domains.map((d) => ({ key: `domain:${d}`, peer: { type: 'domain', name: d }, what: 'by name', via: ['egress'] })),
    ...blocks.map((b) => ({ key: `cidr:${blockWords(b)}`, peer: { type: 'cidr', name: b.cidr || '?', sub: (b.except || []).length ? `except ${b.except.join(', ')}` : '' },
      what: 'all traffic', via: ['egress'] })),
  ];
}

// The default ports of the addons' types, where no edge says one.
const ADDON_PORTS = { postgres: 5432, valkey: 6379, nats: 4222 };

// targetWords say the port a reaches entry names: a service's by name in
// spec, with its number; an addon's as its edges or its type have it.
function targetWords(spec, edges, e) {
  if (typeof e.service === 'string') {
    const s = spec && isObject(spec.services) ? spec.services[e.service] : null;
    const p = portsOfService(s).find((x) => String(x.ref) === String(e.port));
    return portWords(e.port, p ? p.number : '');
  }
  const hit = edges.find((x) => x.to === `addon/${e.addon}`);
  if (hit) return portWords(hit.port, hit.number);
  const a = spec && isObject(spec.addons) ? spec.addons[e.addon] : null;
  return portWords('', (isObject(a) && ADDON_PORTS[a.type]) || '');
}

// reachModel is the picture of an environment from a deployment's spec
// and its reachability answer, and the sandboxes and running jobs of the
// environment (pods: [{kind: sandbox|jobrun, id, reaches, egress}]):
//
//   {objects: [{ref, kind, name, attached, egress, ins, outs}],
//    pods:    [{ref, kind, name, reaches, egress, ins, outs}],
//    routes:  [{route, service, port, from}]}
//
// Objects are the spec's services, jobs and addons (and any the answer
// names that the spec lacks), services first, by name. A pod's outs are
// its reaches (marked missing where the deployment lacks the object) and
// its egress; its ins are the edges to the attached services it runs.
export function reachModel(spec, reach, pods = []) {
  const r = isObject(reach) ? reach : {};
  const edges = (Array.isArray(r.edges) ? r.edges : []).filter(isObject);
  const egress = new Map((Array.isArray(r.egress) ? r.egress : []).filter(isObject).map((x) => [x.object, x]));
  const routes = (Array.isArray(r.routes) ? r.routes : []).filter(isObject);
  const routeBy = new Map(routes.map((x) => [x.route, x]));

  const objs = new Map();
  const put = (kind, name, o) => {
    const ref = `${kind}/${name}`;
    if (!objs.has(ref)) objs.set(ref, { ref, kind, name, spec: o, attached: attachedTo(kind, o) });
  };
  for (const [key, kind] of [['services', 'service'], ['jobs', 'job'], ['addons', 'addon']]) {
    for (const [n, o] of objectsOf(spec, key)) put(kind, n, o);
  }
  for (const e of edges) for (const s of [e.from, e.to]) if (s && s !== 'ingress') put(refOf(s).kind, refOf(s).name, undefined);
  for (const ref of egress.keys()) put(refOf(ref).kind, refOf(ref).name, undefined);

  const choices = reachChoices(spec);
  const podRows = (pods || []).map((p) => {
    const ref = `${p.kind}/${p.id}`;
    const rs = reachesOf(p);
    const outs = rowsBy(OUT_ORDER);
    if (rs === 'all') outs.add({ type: 'env', name: 'every pod' }, 'every port', 'reaches all');
    else {
      const missing = new Set(missingReaches(rs, choices).map(reachKey));
      for (const e of rs) {
        const kind = typeof e.service === 'string' ? 'service' : 'addon';
        const name = e.service || e.addon;
        outs.add({ type: kind, name, missing: missing.has(reachKey(e)) }, targetWords(spec, edges, e), 'reaches');
      }
    }
    const eg = p.egress === undefined ? 'internet' : p.egress;
    const attached = [...objs.values()].filter((o) => o.attached && p.kind === 'sandbox' && o.attached === p.id).map((o) => o.ref);
    const ins = rowsBy(IN_ORDER);
    for (const e of edges) if (attached.includes(e.to)) addIn(ins, e, routeBy);
    return { ref, kind: p.kind, name: p.id, pod: p, reaches: rs, egress: eg, attachedRefs: attached, ins: ins.list(), outs: [...outs.list(), ...egressRows(eg)] };
  });

  const objects = [...objs.values()].map((o) => {
    const ins = rowsBy(IN_ORDER);
    const outs = rowsBy(OUT_ORDER);
    for (const e of edges) {
      if (e.to === o.ref) addIn(ins, e, routeBy);
      if (e.from === o.ref) outs.add(peerOfRef(e.to), portWords(e.port, e.number), e.via);
    }
    for (const p of podRows) {
      const rs = p.reaches;
      const pe = { type: p.kind, name: p.name };
      if (rs === 'all') ins.add(pe, 'every port', 'reaches all');
      else {
        for (const e of rs) {
          const hit = (o.kind === 'service' && e.service === o.name) || (o.kind === 'addon' && e.addon === o.name);
          if (hit) ins.add(pe, targetWords(spec, edges, e), 'reaches');
        }
      }
    }
    let eg;
    if (egress.has(o.ref)) eg = egressOfAnswer(egress.get(o.ref));
    else if (o.kind === 'addon') eg = 'internet';
    else if (o.attached) eg = undefined;
    else eg = isObject(o.spec) ? (o.spec.egress === undefined ? 'internet' : o.spec.egress) : undefined;
    return { ref: o.ref, kind: o.kind, name: o.name, attached: o.attached, egress: eg, ins: ins.list(), outs: [...outs.list(), ...(eg === undefined ? [] : egressRows(eg))] };
  });
  const rank = { service: 0, job: 1, addon: 2 };
  objects.sort((a, b) => ((rank[a.kind] ?? 3) - (rank[b.kind] ?? 3)) || a.name.localeCompare(b.name));
  return { objects, pods: podRows, routes };
}

// addIn adds an edge to its target's left side: the caller, or the
// ingress with the route's clients (one row per set of clients).
function addIn(ins, e, routeBy) {
  if (e.from !== 'ingress') {
    ins.add(peerOfRef(e.from), portWords(e.port, e.number), e.via);
    return;
  }
  const name = String(e.via || '').replace(/^route /, '');
  const route = routeBy.get(name);
  const from = route && Array.isArray(route.from) ? route.from : [];
  const sub = fromWords(from);
  ins.add({ type: 'ingress', name: 'Ingress', sub, blocks: from }, portWords(e.port, e.number), e.via, `ingress:${sub}`);
}

// edgeLine says an edge in a line, as the plan's review lists them:
// "api → catalog · http 8080 · env CATALOG_URL", "ingress → api · http
// 8080 · route api".
export function edgeLine(e) {
  const to = refOf(e.to);
  const from = e.from === 'ingress' ? 'ingress' : refOf(e.from).name;
  return `${from} → ${to.name} · ${portWords(e.port, e.number)}${e.via ? ` · ${e.via}` : ''}`;
}

// edgesByCaller groups a reachability's edges by their caller, in the
// answer's order: [{from, edges}].
export function edgesByCaller(reach) {
  const edges = isObject(reach) && Array.isArray(reach.edges) ? reach.edges.filter(isObject) : [];
  const m = new Map();
  for (const e of edges) {
    if (!m.has(e.from)) m.set(e.from, []);
    m.get(e.from).push(e);
  }
  return [...m.entries()].map(([from, list]) => ({ from, edges: list }));
}
