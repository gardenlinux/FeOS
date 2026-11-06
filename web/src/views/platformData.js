// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The derivations of the platform's pages (nodes, ingresses, join tokens,
// tenants, people, the control plane): pure, so that the tests load them
// as they are.

import { QUOTA_ITEMS, fmtQuota } from '../people.js';
import { fmtBytes, fmtBytesOf, vcpuCapacityText } from './fleet.js';

const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;

// ------------------------------------------------------------------ nodes --

// nodeDot is a node's state in form and colour, with its word and a
// sentence: removed and down win over a drain, which a down node does
// not do.
export function nodeDot(row) {
  if (row.status === 'removed') return { cls: 'absent', word: 'removed', say: 'removed by an operator; kept for its history' };
  if (row.status === 'down') return { cls: 'failed', word: 'down', say: 'its telemetry stopped' };
  if (row.draining) {
    return { cls: 'draining', word: 'draining', say: 'placement keeps off it; its sandboxes move into the object store' };
  }
  if (row.status === 'joined') return { cls: 'paused', word: 'joined', say: 'joined; no telemetry yet' };
  if (row.status === 'ready') return { cls: 'running', word: 'ready', say: 'reports telemetry; placement books on it' };
  return { cls: 'unknown', word: row.status || 'unknown', say: '' };
}

// capBar is one bar of what placement booked against what it may book: a
// share of null is no bound (vCPUs with no overcommit set), a bound of 0
// is full.
function capBar(key, label, used, cap, text, unbounded) {
  const share = unbounded ? null : cap > 0 ? used / cap : 1;
  let tone = '';
  if (share !== null && share >= 1) tone = 'bad';
  else if (share !== null && share >= 0.85) tone = 'warn';
  return { key, label, used, cap, share, pct: share === null ? 0 : Math.min(100, Math.round(share * 100)), tone, text };
}

// capacityBars are a node row's (fleet.js nodeRows) bookings: vCPUs
// against floor(cores × overcommit) (0: not gated), memory against the
// total less the headroom, sandboxes against the slots.
export function capacityBars(row) {
  return [
    capBar('vcpus', 'vCPU', row.vcpusBooked, row.vcpusCapacity,
      `${row.vcpusBooked} / ${vcpuCapacityText(row.vcpusCapacity)}`, !(row.vcpusCapacity > 0)),
    capBar('memory', 'memory', row.memBooked, row.memCapacity, fmtBytesOf(row.memBooked, row.memCapacity), false),
    capBar('slots', 'slots', row.booked, row.slots, `${row.booked} / ${row.slots}`, false),
  ];
}

// fleetCapacity sums the bookings of the nodes placement may book on
// (ready, not draining): the bars of the whole fleet. One node without a
// vCPU bound makes the fleet's unbounded.
export function fleetCapacity(rows) {
  const on = (rows || []).filter((r) => r.status === 'ready' && !r.draining);
  const sum = (k) => on.reduce((a, r) => a + (Number(r[k]) || 0), 0);
  const unbounded = on.some((r) => !(r.vcpusCapacity > 0));
  return {
    nodes: on.length,
    bars: on.length ? capacityBars({
      vcpusBooked: sum('vcpusBooked'), vcpusCapacity: unbounded ? 0 : sum('vcpusCapacity'),
      memBooked: sum('memBooked'), memCapacity: sum('memCapacity'), booked: sum('booked'), slots: sum('slots'),
    }) : [],
  };
}

// NODE_VERSIONS are the parts of a node's versions in the order shown.
const NODE_VERSIONS = ['feosd', 'kernel', 'firecracker', 'guest', 'diskd'];

// nodeVersions reads a node's versions (feosd's telemetry: an object of
// strings and lists of strings) as [name, text], the known parts first,
// empty ones left out.
export function nodeVersions(v) {
  if (!v || typeof v !== 'object' || Array.isArray(v)) return [];
  const text = (x) => (Array.isArray(x) ? x.filter(Boolean).join(', ') : typeof x === 'string' ? x : x == null ? '' : JSON.stringify(x));
  const keys = [...NODE_VERSIONS.filter((k) => k in v), ...Object.keys(v).filter((k) => !NODE_VERSIONS.includes(k)).sort()];
  return keys.map((k) => [k, text(v[k])]).filter(([, t]) => t);
}

// versionSpread counts the distinct values of one part of the versions
// over the nodes that are not removed, the most common first: more than
// one is a rollout under way, or one left behind.
export function versionSpread(nodes, part = 'feosd') {
  const m = new Map();
  for (const n of nodes || []) {
    if (n.status === 'removed') continue;
    const v = n.versions && typeof n.versions[part] === 'string' ? n.versions[part] : '';
    if (!v) continue;
    m.set(v, (m.get(v) || 0) + 1);
  }
  return [...m.entries()].map(([version, count]) => ({ version, count }))
    .sort((a, b) => b.count - a.count || a.version.localeCompare(b.version));
}

// drainPreview says what a drain of the node moves, from what the nodes'
// answer tells: feosd's counts of running and paused sandboxes and what
// placement booked; no list of them. A node without an object store
// cannot be drained (ferund refuses it): its sandboxes cannot hibernate.
export function drainPreview(row, drainTimeout = '') {
  const pods = row.running + row.paused;
  const lines = [];
  if (pods > 0) {
    lines.push(`${plural(pods, 'sandbox', 'sandboxes')} on the node (${row.running} running, ${row.paused} paused) `
      + 'hibernate into the object store.');
    if (row.running > 0) {
      lines.push('Those that ran are restored on another node once hibernated; without room elsewhere they stay '
        + 'hibernated until their next use.');
    }
    lines.push(`Execs that still run after --drain-timeout${drainTimeout ? ` (${drainTimeout})` : ''} are cancelled.`);
  } else {
    lines.push('No sandbox runs or sleeps on the node: nothing moves.');
  }
  if (row.booked !== row.running) {
    lines.push(`Placement booked ${plural(row.booked, 'sandbox', 'sandboxes')} on it, starting ones included.`);
  }
  lines.push('Placement books nothing new on it until an undrain.');
  const blocked = row.status === 'ready' && !row.draining && !row.objectStore
    ? 'the node has no object store: its sandboxes cannot hibernate, so it cannot be drained' : '';
  return { pods, booked: row.booked, lines, blocked };
}

// -------------------------------------------------------------- ingresses --

// ingressState is an ingress instance's dot and words (GET
// /api/v1/admin/ingresses): removed; connected (seen within 30 s, on the
// database's clock); left (it stopped cleanly and was not seen since);
// else not connected.
export function ingressState(g) {
  if (g.status === 'removed') return { cls: 'absent', word: 'removed', say: 'its certificates are revoked; a token pinned to its name joins it again' };
  if (g.connected) return { cls: 'running', word: 'connected', say: 'seen within 30 s' };
  if (g.leftAt) return { cls: 'paused', word: 'left', say: 'it stopped cleanly and was not seen since' };
  if (!g.lastSeen) return { cls: 'moving', word: 'joined', say: 'joined; not seen yet' };
  return { cls: 'failed', word: 'not connected', say: 'not seen for more than 30 s' };
}

// ingressRows orders the ingresses: removed ones last, else by name.
export function ingressRows(list) {
  return [...(list || [])]
    .map((g) => ({ ...g, state: ingressState(g) }))
    .sort((a, b) => (a.status === 'removed') - (b.status === 'removed') || a.name.localeCompare(b.name));
}

// certTone is a certificate's tone at now (ms) by its notAfter: expired
// bad, within a day warn.
export function certTone(notAfter, now = Date.now()) {
  const t = Date.parse(notAfter || '');
  if (Number.isNaN(t)) return '';
  if (t <= now) return 'bad';
  if (t - now < 24 * 3600 * 1000) return 'warn';
  return '';
}

// ------------------------------------------------------------ join tokens --

export const JOIN_KINDS = ['node', 'ingress'];

// joinKindOf reads a kind from the address's query (?kind=ingress); a
// node's otherwise.
export function joinKindOf(search) {
  const k = new URLSearchParams(search || '').get('kind');
  return JOIN_KINDS.includes(k) ? k : 'node';
}

// joinTokenBody is the create body of the join tokens' form: the kind,
// the name it is pinned to (empty: a new name of the joiner's choice) and
// the TTL (0: ferund's default).
export function joinTokenBody(kind, name, ttlSeconds) {
  const n = String(name || '').trim();
  return { ttlSeconds: Number(ttlSeconds) || 0, kind: JOIN_KINDS.includes(kind) ? kind : 'node', name: n };
}

// joinTokenTitle names a fresh token's purpose.
export function joinTokenTitle(t) {
  const kind = t.kind || 'node';
  return t.name || t.node ? `Join token for ${kind} ${t.name || t.node}` : `Join token for a new ${kind}`;
}

// ---------------------------------------------------------------- tenants --

// limitsForm is the limits form's values: a limit as a string, none as
// ''. Memory is in MiB, as the API has it.
export function limitsForm(limits) {
  const l = limits || {};
  const out = {};
  for (const it of QUOTA_ITEMS) out[it.key] = l[it.key] === undefined || l[it.key] === null ? '' : String(l[it.key]);
  return out;
}

// limitsBody is the PUT body of the limits form: every limit, null for an
// empty field (no limit), else a whole number ≥ 0 (0 is a limit). It
// throws on a bad value.
export function limitsBody(form) {
  const out = {};
  for (const it of QUOTA_ITEMS) {
    const s = String((form && form[it.key]) ?? '').trim();
    if (s === '') { out[it.key] = null; continue; }
    if (!/^\d+$/.test(s)) throw new Error(`${it.label} must be a whole number ≥ 0, or empty for no limit`);
    out[it.key] = Number(s);
  }
  return out;
}

// LIMIT_WORDS are the limits' short words in a tenant's row.
const LIMIT_WORDS = { sandboxes: 'sandboxes', environments: 'envs', runningJobs: 'jobs', vcpus: 'vCPU', memoryMib: '' };

// limitsText sums a tenant's limits up in one short line: "no limits",
// or each set one ("50 sandboxes · 10 envs · 32 vCPU · 64 GiB").
export function limitsText(limits) {
  const l = limits || {};
  const set = QUOTA_ITEMS.filter((it) => l[it.key] !== undefined && l[it.key] !== null);
  if (!set.length) return 'no limits';
  return set.map((it) => `${fmtQuota(it, Number(l[it.key]))}${LIMIT_WORDS[it.key] ? ' ' + LIMIT_WORDS[it.key] : ''}`).join(' · ');
}

// knownZones are the zones the tenants' list names, sorted: the zones an
// operator can move a tenant to, as far as the UI can tell (ferund's 400
// names the others).
export function knownZones(tenants) {
  return [...new Set((tenants || []).map((t) => t.zone).filter(Boolean))].sort();
}

// zoneMoveText says what a zone change did to one environment's host.
export function zoneMoveText(m) {
  if (m.hostLabelBefore && m.hostLabelBefore !== m.hostLabel) return `label ${m.hostLabelBefore} → ${m.hostLabel}, now ${m.host}`;
  return `now ${m.host}`;
}

// ----------------------------------------------------------------- people --

// issuerHost is an issuer's host, for a column; the whole URL otherwise.
export function issuerHost(iss) {
  try {
    return new URL(iss).host || iss;
  } catch {
    return iss || '';
  }
}

// peopleRows filters the people by a search over name, e-mail and issuer,
// and by state ('all', 'active', 'disabled'); the API's order (last seen
// first) stays.
export function peopleRows(people, search = '', state = 'all') {
  const q = search.trim().toLowerCase();
  return (people || []).filter((p) => {
    if (state === 'disabled' && !p.disabledAt) return false;
    if (state === 'active' && p.disabledAt) return false;
    if (!q) return true;
    return [p.name, p.email, p.issuer, p.id].some((v) => String(v || '').toLowerCase().includes(q));
  });
}

// seenWithin counts the people seen within ms before now (ms).
export function seenWithin(people, ms, now = Date.now()) {
  return (people || []).filter((p) => {
    const t = Date.parse(p.lastSeen || '');
    return !Number.isNaN(t) && now - t <= ms;
  }).length;
}

// ---------------------------------------------------------- control plane --

// instanceVersions counts the versions of the instances that are fresh
// (live or stopping), the most common first.
export function instanceVersions(instances) {
  const m = new Map();
  for (const i of instances || []) {
    if (!i.fresh && !i.live) continue;
    const v = i.version || '—';
    m.set(v, (m.get(v) || 0) + 1);
  }
  return [...m.entries()].map(([version, count]) => ({ version, count }))
    .sort((a, b) => b.count - a.count || a.version.localeCompare(b.version));
}

export { fmtBytes };
