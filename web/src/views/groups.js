// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// An environment's security groups (ADR 0034) and what names them (ADR
// 0035): the spec's securityGroups, a group's rules in words, the rule
// rows of the builder's form, the groups a service, job or addon names,
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
export const DEFAULT_WORDS = 'nothing in, internet out';

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

// PEER_TYPES are a peer's kinds in the form.
export const PEER_TYPES = [
  { key: 'group', label: 'Group' },
  { key: 'cidr', label: 'Address block' },
  { key: 'internet', label: 'Internet' },
];

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

// retypePeer is a new peer of type t, keeping what it can.
export function retypePeer(peer, t, groups = []) {
  if (t === peerType(peer)) return peer;
  if (t === 'internet') return 'internet';
  if (t === 'group') return { group: groups[0] || DEFAULT_GROUP };
  return { cidr: '' };
}

// peerWords say a peer: "group web", "203.0.113.0/24 except
// 203.0.113.8/29", "the internet".
export function peerWords(peer) {
  switch (peerType(peer)) {
    case 'internet': return 'the internet';
    case 'group': return `group ${peer.group || '?'}`;
    case 'cidr': {
      const ex = Array.isArray(peer.except) && peer.except.length ? ` except ${peer.except.join(', ')}` : '';
      return `${peer.cidr || '?'}${ex}`;
    }
    default: return JSON.stringify(peer);
  }
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

// ruleWords say a rule: "tcp 8080, 9000-9010 from group web", "any to
// the internet", "tcp from 10.0.0.0/8" (every port).
export function ruleWords(rule, dir) {
  if (!isObject(rule)) return JSON.stringify(rule);
  const p = rule.protocol || 'tcp';
  const ports = takesPorts(p) && Array.isArray(rule.ports) && rule.ports.length ? ` ${portsText(rule.ports)}` : '';
  return `${p}${ports} ${dir === 'out' ? 'to' : 'from'} ${peerWords(rule[peerKey(dir)])}`;
}

// isInternetOut tells the default out: any to the internet, alone.
function isInternetOut(out) {
  return Array.isArray(out) && out.length === 1 && isObject(out[0]) && out[0].protocol === 'any' && out[0].to === 'internet'
    && !(Array.isArray(out[0].ports) && out[0].ports.length);
}

// outMode says a group's out: default (left out, the internet), none
// (out: []) or rules.
export function outMode(g) {
  if (!isObject(g) || !Array.isArray(g.out)) return 'default';
  return g.out.length === 0 ? 'none' : 'rules';
}

// groupWords say a group in a line: "nothing in, internet out" for one
// without rules; "in: tcp 5432 from group api · internet out";
// "3 rules in · nothing out".
export function groupWords(g) {
  const o = isObject(g) ? g : {};
  const ins = Array.isArray(o.in) ? o.in : [];
  const inWords = ins.length === 0 ? 'nothing in'
    : ins.length <= 2 ? `in: ${ins.map((r) => ruleWords(r, 'in')).join('; ')}` : `${ins.length} rules in`;
  const mode = outMode(o);
  const outWords = mode === 'default' || isInternetOut(o.out) ? 'internet out'
    : mode === 'none' ? 'nothing out'
      : o.out.length <= 2 ? `out: ${o.out.map((r) => ruleWords(r, 'out')).join('; ')}` : `${o.out.length} rules out`;
  if (ins.length === 0 && outWords === 'internet out') return DEFAULT_WORDS;
  return `${inWords} · ${outWords}`;
}

// ---------------------------------------------------------- the choice --

// groupChoices are the groups a sandbox or a tenant's job may name in an
// environment: those of its deployed deployment's spec, "default" first
// (it exists without a deployment), each with its words.
export function groupChoices(spec) {
  return groupNames(spec).map((name) => ({ name, words: groupWords(groupOf(spec, name)) }));
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
