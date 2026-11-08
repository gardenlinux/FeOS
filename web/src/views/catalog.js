// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The addons' catalog (ADR 0032, "Forms, the catalog, a first
// deployment"): GET /api/v1/addons answers {addons: [...]}, the
// registered blueprints, each with every field present: type, versions,
// defaultVersion (an offer for a new addon, not a default of the spec),
// plans ({name, vcpus, memoryMib, volumeMib}), defaultPlan, outputKeys
// and outputFields (what a reference may name: its key, and fields such
// as role and database), and options ({name, kind: string, list, boolean
// or oneOf, required, default, description, values, pattern, reserved,
// reservedPrefixes, unique}). The UI copies none of it; a ferund without
// the route (404 or 405, api.addonCatalog's null) gets a minimal built-in list, which says so. The
// options' pattern, reserved names and uniqueness give the forms hints;
// the plan stays the judge. Pure.

import { isObject } from './draft.js';

// OPTION_KINDS are the option fields' kinds a form renders; an option of
// another kind is a JSON field.
export const OPTION_KINDS = ['string', 'list', 'boolean', 'oneOf'];

const strings = (v) => (Array.isArray(v) ? v.filter((x) => typeof x === 'string') : []);

function plans(v) {
  return (Array.isArray(v) ? v : []).filter((p) => isObject(p) && p.name).map((p) => ({
    name: String(p.name),
    vcpus: Number(p.vcpus) || 0,
    memoryMib: Number(p.memoryMib) || 0,
    volumeMib: Number(p.volumeMib) || 0,
  }));
}

function options(v) {
  return (Array.isArray(v) ? v : []).filter((o) => isObject(o) && o.name).map((o) => ({
    name: String(o.name),
    kind: OPTION_KINDS.includes(o.kind) ? o.kind : 'json',
    required: !!o.required,
    default: o.default,
    description: typeof o.description === 'string' ? o.description : '',
    values: strings(o.values),
    pattern: typeof o.pattern === 'string' ? o.pattern : '',
    reserved: strings(o.reserved),
    reservedPrefixes: strings(o.reservedPrefixes),
    unique: !!o.unique,
  }));
}

function blueprint(b) {
  const versions = strings(b.versions);
  return {
    type: String(b.type),
    versions,
    defaultVersion: typeof b.defaultVersion === 'string' && b.defaultVersion ? b.defaultVersion : versions[versions.length - 1] || '',
    plans: plans(b.plans),
    defaultPlan: String(b.defaultPlan || ''),
    outputs: strings(b.outputKeys),
    fields: strings(b.outputFields),
    options: options(b.options),
    suggestions: {},
  };
}

// optionProblems are the hints of an option's value against its
// declaration: a required one missing, a name the pattern does not match
// or that is reserved, a list's duplicates. Hints only: the plan judges.
export function optionProblems(d, value) {
  const out = [];
  if (value === undefined || value === null || value === '' || (Array.isArray(value) && value.length === 0)) {
    if (d.required) out.push('required');
    return out;
  }
  let re = null;
  try {
    re = d.pattern ? new RegExp(d.pattern) : null;
  } catch {
    re = null;
  }
  const check = (s) => {
    if (typeof s !== 'string') return;
    if (re && !re.test(s)) out.push(`${s} does not match ${d.pattern}`);
    if (d.reserved.includes(s)) out.push(`${s} is reserved`);
    const p = d.reservedPrefixes.find((x) => s.startsWith(x));
    if (p) out.push(`${s} starts with ${p}, which is reserved`);
  };
  if (d.kind === 'list' && Array.isArray(value)) {
    value.forEach(check);
    if (d.unique) {
      const seen = new Set();
      for (const s of value) {
        if (seen.has(s)) out.push(`${s} twice`);
        seen.add(s);
      }
    }
  } else if (d.kind === 'string') {
    check(value);
  }
  return out;
}

// readCatalog reads GET /api/v1/addons's answer into {builtin, types,
// byType}; null for an answer without a list of addons.
export function readCatalog(answer, builtin = false) {
  const list = isObject(answer) ? answer.addons : null;
  if (!Array.isArray(list)) return null;
  const types = list.filter((b) => isObject(b) && b.type).map(blueprint).sort((a, b) => a.type.localeCompare(b.type));
  return { builtin, types, byType: new Map(types.map((t) => [t.type, t])) };
}

// BUILTIN is the minimal list for a ferund that answers no catalog: the
// three blueprints this UI release knew, in the catalog's shape, without
// option hints. Option fields it does not list are JSON fields.
const opt = (name, kind, extra = {}) => ({ name, kind, required: false, default: null, description: '', values: [],
  pattern: '', reserved: [], reservedPrefixes: [], unique: false, ...extra });
const BUILTIN = [
  { type: 'postgres', versions: ['16', '17'], defaultVersion: '17', defaultPlan: 'small', plans: ['dev', 'small', 'medium', 'large'],
    outputKeys: ['host', 'port', 'user', 'password', 'database', 'url'], outputFields: ['role', 'database'],
    options: [opt('databases', 'list'), opt('roles', 'list')] },
  { type: 'valkey', versions: ['9'], defaultVersion: '9', defaultPlan: 'small', plans: ['dev', 'small', 'medium', 'large'],
    outputKeys: ['host', 'port', 'url'], outputFields: [], options: [opt('maxmemoryPolicy', 'string')] },
  { type: 'nats', versions: ['2'], defaultVersion: '2', defaultPlan: 'small', plans: ['dev', 'small'],
    outputKeys: ['host', 'port', 'user', 'password', 'url'], outputFields: [], options: [opt('jetstream', 'boolean', { default: true })] },
];

export function builtinCatalog() {
  return readCatalog({ addons: BUILTIN.map((b) => ({ ...b, plans: b.plans.map((name) => ({ name })) })) }, true);
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

// newAddon is an addon of a blueprint: the version and plan it offers
// (defaultVersion, defaultPlan), no options (the blueprint's defaults
// apply).
export function newAddon(bp) {
  const a = { type: bp.type };
  if (bp.defaultVersion) a.version = bp.defaultVersion;
  if (bp.defaultPlan) a.plan = bp.defaultPlan;
  return a;
}
