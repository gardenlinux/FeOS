// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// An environment's security groups (ADR 0034) and what names them (ADR
// 0035): the spec's securityGroups, a group in a line, its flow picture
// (who may connect, what its members reach), the rule rows of the
// builder's form, the groups a service, job or addon names,
// and the choice of groups of a sandbox or a tenant's job from the
// environment's deployed deployment. Pure: no React, no browser.
//
// The shape is the spec's (go/internal/env/spec):
//
//   securityGroups: {<name>: {in: [Rule], out: [Rule]}}
//   Rule: {protocol: tcp|udp|icmp|any (absent: tcp), ports: [8080,
//          "9000-9010"], from | to: Peer}
//   Peer: {group: <name>} | {cidr, except: [...]} | "internet"
//
// A group without out reaches the internet (normalisation writes
// [{protocol: any, to: internet}]); out: [] reaches nothing outside the
// environment. Out to the environment's own pods is implied: the
// receiver's in rules decide. "default" always exists.

import { isObject, objectsOf } from './draft.js';

// DEFAULT_GROUP is the group every environment has; an object that names
// none is in it.
export const DEFAULT_GROUP = 'default';

// GROUP_LIMITS are the plan's limits.
export const GROUP_LIMITS = { groups: 50, perObject: 5, rules: 60 };

// PROTOCOLS are a rule's protocols; tcp when it is left out.
export const PROTOCOLS = ['tcp', 'udp', 'icmp', 'any'];

// DEFAULT_WORDS say what a group without rules does.
export const DEFAULT_WORDS = 'nothing in · internet out';

const GROUP_NAME = /^[a-z0-9][a-z0-9-]{0,31}$/;

// isGroupName tells a group's name: 1 to 32 of a-z, 0-9 and '-',
// starting with a letter or a digit.
export function isGroupName(s) {
  return GROUP_NAME.test(String(s || ''));
}

// groupNames are the groups a spec has, "default" first (it exists
// without being written), the rest by name.
export function groupNames(spec) {
  const rest = objectsOf(spec, 'securityGroups').map(([n]) => n).filter((n) => n !== DEFAULT_GROUP);
  return [DEFAULT_GROUP, ...rest];
}

// groupOf is a group of the spec; {} for "default" when it is not
// written (normalisation adds it so), undefined for none.
export function groupOf(spec, name) {
  const m = spec && isObject(spec.securityGroups) ? spec.securityGroups : {};
  if (Object.prototype.hasOwnProperty.call(m, name)) return isObject(m[name]) ? m[name] : {};
  return name === DEFAULT_GROUP ? {} : undefined;
}

// objectGroups are the groups a service, job or addon names: its list,
// or ["default"] when it names none (as normalisation writes it).
export function objectGroups(o) {
  return isObject(o) && Array.isArray(o.securityGroups) ? o.securityGroups : [DEFAULT_GROUP];
}

// toggleGroup is list with name ticked or not, in the order of choices;
// never empty and never above the limit (the toggle is then refused and
// list comes back as it was).
export function toggleGroup(list, name, on, choices = []) {
  const cur = new Set(list || []);
  if (on) cur.add(name);
  else cur.delete(name);
  if (cur.size === 0 || cur.size > GROUP_LIMITS.perObject) return list;
  const order = [...choices, ...[...cur].filter((n) => !choices.includes(n)).sort()];
  return order.filter((n) => cur.has(n));
}

// ---------------------------------------------------------------- ports --

// parsePorts reads a rule's ports as typed, "8080, 9000-9010": {ports,
// bad}; ports are numbers and "a-b" ranges, bad what is neither (or out of
// 1–65535, or a range whose end is below its start).
export function parsePorts(text) {
  const ports = [];
  const bad = [];
  for (const raw of String(text || '').split(/[,\s]+/)) {
    const s = raw.trim();
    if (!s) continue;
    const one = /^(\d{1,5})$/.exec(s);
    const range = /^(\d{1,5})-(\d{1,5})$/.exec(s);
    const ok = (n) => n >= 1 && n <= 65535;
    if (one && ok(Number(one[1]))) ports.push(Number(one[1]));
    else if (range && ok(Number(range[1])) && ok(Number(range[2])) && Number(range[1]) <= Number(range[2])) {
      ports.push(range[1] === range[2] ? Number(range[1]) : `${Number(range[1])}-${Number(range[2])}`);
    } else bad.push(s);
  }
  return { ports, bad };
}

// portsText renders a rule's ports as the field shows them.
export function portsText(ports) {
  return (Array.isArray(ports) ? ports : []).map(String).join(', ');
}

// takesPorts tells whether a protocol has ports: tcp and udp.
export function takesPorts(protocol) {
  const p = protocol || 'tcp';
  return p === 'tcp' || p === 'udp';
}

// ---------------------------------------------------------------- peers --

// peerKey is where a rule's peer is: from in an in rule, to in an out
// rule.
export function peerKey(dir) {
  return dir === 'out' ? 'to' : 'from';
}

// peerType names a peer's kind: group, cidr, internet or other.
export function peerType(peer) {
  if (peer === 'internet') return 'internet';
  if (isObject(peer) && typeof peer.group === 'string') return 'group';
  if (isObject(peer) && typeof peer.cidr === 'string') return 'cidr';
  return 'other';
}

// ---------------------------------------------------------------- rules --

// newRule is the rule "Add a rule" makes: tcp 8080 from group default in,
// tcp 443 to the internet out.
export function newRule(dir) {
  return dir === 'out'
    ? { protocol: 'tcp', ports: [443], to: 'internet' }
    : { protocol: 'tcp', ports: [8080], from: { group: DEFAULT_GROUP } };
}

// withProtocol is rule with protocol p; icmp and any lose their ports.
export function withProtocol(rule, p) {
  const out = { ...rule, protocol: p };
  if (!takesPorts(p)) delete out.ports;
  return out;
}

// outMode says a group's out: default (left out, the internet), none
// (out: []) or rules.
export function outMode(g) {
  if (!isObject(g) || !Array.isArray(g.out)) return 'default';
  return g.out.length === 0 ? 'none' : 'rules';
}

// peerShort names a peer in a few letters: the group's name, "internet",
// the address block.
function peerShort(peer) {
  switch (peerType(peer)) {
    case 'internet': return 'internet';
    case 'group': return peer.group || '?';
    case 'cidr': return peer.cidr || '?';
    default: return '?';
  }
}

// shortList is a list in a line: at most three, then "+n".
function shortList(list) {
  const u = [...new Set(list)];
  return u.length <= 3 ? u.join(', ') : `${u.slice(0, 3).join(', ')} +${u.length - 3}`;
}

// groupSummary says a group in a line, without ports: "nothing in ·
// internet out", "in from web, api · nothing out", "in from
// 203.0.113.0/24 · out to internet, 10.0.0.0/8".
export function groupSummary(g) {
  const o = isObject(g) ? g : {};
  const ins = (Array.isArray(o.in) ? o.in : []).filter(isObject);
  const inWords = ins.length ? `in from ${shortList(ins.map((r) => peerShort(r.from)))}` : 'nothing in';
  const mode = outMode(o);
  const outs = mode === 'rules' ? o.out.filter(isObject) : [];
  const open = outs.length === 1 && outs[0].protocol === 'any' && outs[0].to === 'internet';
  const outWords = mode === 'default' || open ? 'internet out' : mode === 'none' ? 'nothing out'
    : `out to ${shortList(outs.map((r) => peerShort(r.to)))}`;
  return `${inWords} · ${outWords}`;
}

// ---------------------------------------------------------- the choice --

// groupChoices are the groups a sandbox or a tenant's job may name in an
// environment: those of its deployed deployment's spec, "default" first
// (it exists without a deployment), each with its words.
export function groupChoices(spec) {
  return groupNames(spec).map((name) => ({ name, words: groupSummary(groupOf(spec, name)) }));
}

// checkGroups are the problems of a choice of groups, as ferund refuses
// it: none (an empty list is refused), more than five, a name the
// environment does not have.
export function checkGroups(list, choices = null) {
  const out = [];
  const l = Array.isArray(list) ? list : [];
  if (l.length === 0) out.push('name at least one security group');
  if (l.length > GROUP_LIMITS.perObject) out.push(`at most ${GROUP_LIMITS.perObject} security groups`);
  if (choices) {
    const have = new Set(choices.map((c) => (typeof c === 'string' ? c : c.name)));
    const unknown = l.filter((n) => !have.has(n));
    if (unknown.length) out.push(`the environment has no group ${unknown.join(', ')}`);
  }
  return out;
}

// sameGroups tells two lists of groups apart by their members.
export function sameGroups(a, b) {
  const x = [...(a || [])].sort();
  const y = [...(b || [])].sort();
  return x.length === y.length && x.every((v, i) => v === y[i]);
}

// MOVE_WORDS say what a change of a sandbox's groups does (ADR 0035).
export const MOVE_WORDS = 'The sandbox is moved: it is hibernated and restored in the new groups under a new address. '
  + 'Its disk and memory stay as they are, processes go on, open connections break. A hibernated sandbox takes them at its next restore.';

// -------------------------------------------------------------- the env --

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

// ------------------------------------------------------------- who is in --

const MEMBER_KEYS = [['services', 'service'], ['jobs', 'job'], ['addons', 'addon']];

// attachedTo is the sandbox an attached service runs in, or '' for any
// other object: an attached service has no groups of its own, it is in
// its sandbox's (ADR 0035).
export function attachedTo(kind, o) {
  return kind === 'service' && isObject(o) && o.runtime === 'attached' ? String(o.sandbox || '?') : '';
}

// groupMembers are the objects spec puts into group name: [{kind, name}],
// the services, jobs and addons that name it, or name none when it is
// "default"; attached services are left out (their sandbox is the pod).
export function groupMembers(spec, name) {
  const out = [];
  for (const [key, kind] of MEMBER_KEYS) {
    for (const [n, o] of objectsOf(spec, key)) {
      if (!attachedTo(kind, o) && objectGroups(o).includes(name)) out.push({ kind, name: n });
    }
  }
  return out;
}

// podGroups are a sandbox's or a job's groups, "default" when it names
// none.
export function podGroups(p) {
  return p && Array.isArray(p.securityGroups) && p.securityGroups.length ? p.securityGroups : [DEFAULT_GROUP];
}

// inRules are group name's in rules: what may reach its members.
export function inRules(spec, name) {
  const g = groupOf(spec, name);
  return isObject(g) && Array.isArray(g.in) ? g.in : [];
}

// reaches are the groups whose in rules let group name's members in:
// [{group, rule}], by the target's name.
export function reaches(spec, name) {
  const out = [];
  for (const g of groupNames(spec)) {
    for (const rule of inRules(spec, g)) {
      if (isObject(rule) && peerType(rule.from) === 'group' && rule.from.group === name) out.push({ group: g, rule });
    }
  }
  return out;
}

// ---------------------------------------------------------------- flows --

// A flow picture says who may connect to a set of pods and what they may
// reach: the rows on its left (in) and on its right (out), one per peer,
// the rules of every group of the set merged.
//
//   Row: {key, peer: {type, name, except}, access: [{protocol, ports}],
//         via: [group], byDefault, paths}
//
// type is group, self (the set's own group: its members reach each other),
// internet, cidr, ingress (routes, which need no rule) or other. access
// lists each protocol once with its ports ([] = every port); "any" stands
// alone.

// The order of a side's rows: from outside first on the left, the
// environment's groups first on the right.
const IN_ORDER = { ingress: 0, internet: 1, cidr: 2, other: 3, group: 4, self: 5 };
const OUT_ORDER = { group: 0, internet: 1, cidr: 2, other: 3 };

function rowPeer(peer, self) {
  const t = peerType(peer);
  if (t === 'group') return { type: self.includes(peer.group) ? 'self' : 'group', name: peer.group || '?' };
  if (t === 'cidr') return { type: 'cidr', name: peer.cidr || '?', except: Array.isArray(peer.except) ? peer.except : [] };
  if (t === 'internet') return { type: 'internet', name: 'Internet' };
  return { type: 'other', name: JSON.stringify(peer) };
}

// addAccess merges a rule's protocol and ports into a row's access.
function addAccess(access, rule) {
  const p = (isObject(rule) && rule.protocol) || 'tcp';
  const ports = takesPorts(p) && isObject(rule) && Array.isArray(rule.ports) ? rule.ports : [];
  if (access.some((a) => a.protocol === 'any')) return access;
  if (p === 'any') return [{ protocol: 'any', ports: [] }];
  const cur = access.find((a) => a.protocol === p);
  if (!cur) return [...access, { protocol: p, ports: [...ports] }];
  if (cur.ports.length === 0 || ports.length === 0) return access.map((a) => (a === cur ? { ...a, ports: [] } : a));
  const merged = [...cur.ports, ...ports.filter((x) => !cur.ports.some((y) => String(y) === String(x)))];
  return access.map((a) => (a === cur ? { ...a, ports: merged } : a));
}

// Rows collects rows by peer, merging what a second rule to the same peer
// allows; order ranks the peers' types.
function rows(order) {
  const m = new Map();
  return {
    add(peer, rule, via, extra = {}) {
      const key = `${peer.type}:${peer.name}:${(peer.except || []).join(',')}`;
      const cur = m.get(key) || { key, peer, access: [], via: [], ...extra };
      cur.access = addAccess(cur.access, rule);
      if (via && !cur.via.includes(via)) cur.via.push(via);
      m.set(key, cur);
    },
    list() {
      return [...m.values()].sort((a, b) => (order[a.peer.type] - order[b.peer.type]) || a.peer.name.localeCompare(b.peer.name));
    },
  };
}

// flowsOf is the flow picture of pods in groups, by spec's rules: {ins,
// outs}. self are the groups whose members count as the pods themselves
// (a group's own picture: [its name]); a rule from one of them is "each
// other", and the pods' reach into one of them is left out of outs.
export function flowsOf(spec, groups, self = []) {
  const ins = rows(IN_ORDER);
  const outs = rows(OUT_ORDER);
  for (const g of groups || []) {
    for (const r of inRules(spec, g)) if (isObject(r)) ins.add(rowPeer(r.from, self), r, g);
    for (const { group, rule } of reaches(spec, g)) {
      if (!self.includes(group)) outs.add({ type: 'group', name: group }, rule, g);
    }
    const o = groupOf(spec, g) || {};
    const mode = outMode(o);
    if (mode === 'default') outs.add({ type: 'internet', name: 'Internet' }, { protocol: 'any' }, g, { byDefault: true });
    if (mode === 'rules') for (const r of o.out) if (isObject(r)) outs.add(rowPeer(r.to, []), r, g);
  }
  return { ins: ins.list(), outs: outs.list() };
}

// ingressRow is the row of the routes to services: the ingress reaches
// them without a rule (paths, by path); null when no route leads to one
// of them.
export function ingressRow(spec, services) {
  const paths = objectsOf(spec, 'routes')
    .filter(([, r]) => isObject(r) && services.includes(r.service))
    .map(([, r]) => String(r.path || '/'))
    .sort();
  if (!paths.length) return null;
  return { key: 'ingress', peer: { type: 'ingress', name: 'Ingress' }, access: [], via: [], paths: [...new Set(paths)] };
}

// ingressWords say what the ingress reaches: "HTTP /", "HTTP /api/a,
// /api/b, /c +2".
export function ingressWords(paths) {
  const list = Array.isArray(paths) ? paths : [];
  return `HTTP ${list.slice(0, 3).join(', ')}${list.length > 3 ? ` +${list.length - 3}` : ''}`;
}

// accessWords say a row's access: "all traffic", "tcp 5432, 4222",
// "tcp 443 · udp 53", "tcp all ports", "icmp".
export function accessWords(access) {
  const list = Array.isArray(access) ? access : [];
  if (list.some((a) => a.protocol === 'any')) return 'all traffic';
  return list.map((a) => (takesPorts(a.protocol) ? `${a.protocol} ${a.ports.length ? portsText(a.ports) : 'all ports'}` : a.protocol))
    .join(' · ');
}

// RULES_WORDS say how groups work, in a line.
export const RULES_WORDS = 'Closed by default: only what a rule lets in gets in. Replies come back by themselves, '
  + 'and routes from the ingress need no rule.';
