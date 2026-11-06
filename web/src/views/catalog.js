// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The addons' catalog (ADR 0032, "Forms, the catalog, a first
// deployment"): GET /api/v1/addons answers the registered blueprints,
// each with its type, versions, plans (vCPUs, memory, volume), default
// plan, the output keys and fields a reference may name, and the option
// fields it declares (a name, a kind: string, list, boolean or one of
// values, and a default). The UI copies none of it; a ferund without the
// route gets a minimal built-in list, which says so. Pure.

import { isObject } from './draft.js';

// OPTION_KINDS are the option fields' kinds a form renders; any other
// option is a JSON field.
export const OPTION_KINDS = ['string', 'list', 'boolean', 'enum'];

function strings(v) {
  if (!Array.isArray(v)) return [];
  return v.map((x) => (typeof x === 'string' ? x : isObject(x) ? x.name || x.key || '' : '')).filter(Boolean);
}

function plans(v) {
  const list = Array.isArray(v) ? v : isObject(v) ? Object.entries(v).map(([name, p]) => ({ name, ...(isObject(p) ? p : {}) })) : [];
  return list.filter((p) => isObject(p) && p.name).map((p) => ({
    name: String(p.name),
    vcpus: Number(p.vcpus ?? p.VCPUs ?? 0) || 0,
    memoryMib: Number(p.memoryMib ?? p.MemoryMib ?? 0) || 0,
    volumeMib: Number(p.volumeMib ?? p.VolumeMib ?? 0) || 0,
  })).sort((a, b) => a.memoryMib - b.memoryMib || a.name.localeCompare(b.name));
}

function kind(o) {
  const k = String(o.kind || o.type || '').toLowerCase();
  if (k === 'bool') return 'boolean';
  if (k === 'oneof' || k === 'one of' || k === 'enum' || Array.isArray(o.values) || Array.isArray(o.oneOf)) return 'enum';
  return OPTION_KINDS.includes(k) ? k : 'json';
}

function options(v) {
  const list = Array.isArray(v) ? v : isObject(v) ? Object.entries(v).map(([name, o]) => ({ name, ...(isObject(o) ? o : {}) })) : [];
  return list.filter((o) => isObject(o) && o.name).map((o) => ({
    name: String(o.name),
    kind: kind(o),
    values: strings(o.values || o.oneOf),
    default: o.default,
    description: typeof o.description === 'string' ? o.description : '',
  }));
}

function blueprint(b) {
  const outputs = isObject(b.outputs) ? b.outputs : null;
  return {
    type: String(b.type),
    versions: strings(b.versions),
    plans: plans(b.plans),
    defaultPlan: String(b.defaultPlan || ''),
    outputs: strings(outputs ? outputs.keys : b.outputs || b.outputKeys || b.keys),
    fields: strings(outputs ? outputs.fields : b.referenceFields || b.fields),
    options: options(b.options),
    suggestions: isObject(b.suggestedEnv) ? b.suggestedEnv : isObject(b.envNames) ? b.envNames : {},
  };
}

// readCatalog reads GET /api/v1/addons's answer ({addons: [...]} or a
// list) into {builtin, types, byType}; null for an answer without one.
export function readCatalog(answer, builtin = false) {
  const list = Array.isArray(answer) ? answer : isObject(answer) ? answer.addons || answer.blueprints : null;
  if (!Array.isArray(list)) return null;
  const types = list.filter((b) => isObject(b) && b.type).map(blueprint).sort((a, b) => a.type.localeCompare(b.type));
  return { builtin, types, byType: new Map(types.map((t) => [t.type, t])) };
}

// BUILTIN is the minimal list for a ferund that answers no catalog: the
// three blueprints this UI release knew. Option fields it does not list
// are JSON fields.
const BUILTIN = [
  { type: 'postgres', versions: ['16', '17'], defaultPlan: 'small', plans: ['dev', 'small', 'medium', 'large'],
    outputs: ['host', 'port', 'user', 'password', 'database', 'url'], referenceFields: ['role', 'database'],
    options: [{ name: 'databases', kind: 'list' }, { name: 'roles', kind: 'list' }] },
  { type: 'valkey', versions: ['7.2', '8'], defaultPlan: 'small', plans: ['dev', 'small', 'medium', 'large'],
    outputs: ['host', 'port', 'user', 'password', 'url'], options: [{ name: 'maxmemoryPolicy', kind: 'string' }] },
  { type: 'nats', versions: ['2'], defaultPlan: 'small', plans: ['dev', 'small'],
    outputs: ['host', 'port', 'user', 'password', 'url'], options: [{ name: 'jetstream', kind: 'boolean', default: true }] },
];

export function builtinCatalog() {
  return readCatalog(BUILTIN.map((b) => ({ ...b, plans: b.plans.map((name) => ({ name })) })), true);
}

// planText describes a plan: "small · 1 vCPU, 1024 MiB, volume 10 GiB".
export function planText(p) {
  if (!p) return '';
  const parts = [];
  if (p.vcpus) parts.push(`${p.vcpus} vCPU${p.vcpus === 1 ? '' : 's'}`);
  if (p.memoryMib) parts.push(`${p.memoryMib} MiB`);
  if (p.volumeMib) parts.push(`volume ${p.volumeMib >= 1024 ? `${Math.round(p.volumeMib / 1024)} GiB` : `${p.volumeMib} MiB`}`);
  return parts.length ? `${p.name} · ${parts.join(', ')}` : p.name;
}

// newAddon is an addon of a blueprint: its newest version and default
// plan, no options (the blueprint's defaults apply).
export function newAddon(bp) {
  const a = { type: bp.type };
  if (bp.versions.length) a.version = bp.versions[bp.versions.length - 1];
  if (bp.defaultPlan) a.plan = bp.defaultPlan;
  return a;
}
