// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The references of a spec (ADR 0032, "The graph"): who names whom. A
// variable names an addon's output, another service's port or a secret;
// a registry login names a secret; a service or job names its bindings, a
// binding's credential its secrets; a route names a service's port. The
// forms make and show them as lines, the graph (step E4) draws them as
// edges. Pure.

import { getObject, isObject, mapEnvValues, objectsOf, removeObject, setObject } from './draft.js';

// envValueKind names an env value's form: string (a plain string), value
// ({value}), secret, addon, service, or invalid.
export function envValueKind(v) {
  if (typeof v === 'string') return 'string';
  if (!isObject(v)) return 'invalid';
  const keys = Object.keys(v);
  if (keys.length === 1 && typeof v.value === 'string') return 'value';
  if (keys.length === 1 && typeof v.secret === 'string') return 'secret';
  if ('addon' in v && !('service' in v)) return 'addon';
  if ('service' in v && !('addon' in v)) return 'service';
  return 'invalid';
}

// valueRef is the reference an env value makes, or null: {to: {key,
// name}, port?, refKey?, fields?}.
function valueRef(v) {
  switch (envValueKind(v)) {
    case 'secret': return { to: { key: 'secrets', name: v.secret } };
    case 'addon': {
      const fields = {};
      for (const [k, f] of Object.entries(v)) if (k !== 'addon' && k !== 'key') fields[k] = f;
      return { to: { key: 'addons', name: v.addon }, refKey: v.key || '', fields };
    }
    case 'service': return { to: { key: 'services', name: v.service }, port: v.port ?? '', refKey: v.key || '' };
    default: return null;
  }
}

// referencesOf lists every reference of spec: {from: {key, name}, field,
// to: {key, name}, via, port?, refKey?, fields?, dashed?}. via says what
// holds it: env, imageCredentials, bindings, credential or route.
export function referencesOf(spec) {
  const out = [];
  for (const key of ['services', 'jobs']) {
    for (const [name, o] of objectsOf(spec, key)) {
      if (!isObject(o)) continue;
      for (const [k, v] of Object.entries(isObject(o.env) ? o.env : {})) {
        const r = valueRef(v);
        if (r) out.push({ from: { key, name }, field: `env.${k}`, via: 'env', variable: k, ...r });
      }
      if (isObject(o.imageCredentials)) {
        for (const f of ['username', 'password']) {
          const v = o.imageCredentials[f];
          if (envValueKind(v) === 'secret') {
            out.push({ from: { key, name }, field: `imageCredentials.${f}`, via: 'imageCredentials',
              to: { key: 'secrets', name: v.secret }, dashed: true });
          }
        }
      }
      (Array.isArray(o.bindings) ? o.bindings : []).forEach((b, i) => {
        out.push({ from: { key, name }, field: `bindings[${i}]`, via: 'bindings', to: { key: 'bindings', name: b } });
      });
    }
  }
  for (const [name, b] of objectsOf(spec, 'bindings')) {
    const c = isObject(b) && isObject(b.credential) ? b.credential : null;
    if (!c) continue;
    const fields = [];
    for (const [k, v] of Object.entries(c)) {
      if (k === 'headers' && isObject(v)) for (const [h, hv] of Object.entries(v)) fields.push([`headers.${h}`, hv]);
      else fields.push([k, v]);
    }
    for (const [f, v] of fields) {
      if (envValueKind(v) === 'secret') {
        out.push({ from: { key: 'bindings', name }, field: `credential.${f}`, via: 'credential',
          to: { key: 'secrets', name: v.secret } });
      }
    }
  }
  for (const [name, r] of objectsOf(spec, 'routes')) {
    if (isObject(r) && r.service) {
      out.push({ from: { key: 'routes', name }, field: 'service', via: 'route', to: { key: 'services', name: r.service },
        port: r.port ?? '', path: r.path || '' });
    }
  }
  return out;
}

// refsFrom are the references key/name holds; referencesTo those to it.
export function refsFrom(spec, key, name, refs = referencesOf(spec)) {
  return refs.filter((r) => r.from.key === key && r.from.name === name);
}

export function referencesTo(spec, key, name, refs = referencesOf(spec)) {
  return refs.filter((r) => r.to.key === key && r.to.name === name);
}

// refLine renders a reference as the list's line: "DATABASE_URL → db ·
// url", "API_URL → api:http · url", "TOKEN → secret token", "/api → web:
// http", "registry login → secret registry-ghcr.io".
export function refLine(r) {
  const port = r.port !== undefined && r.port !== '' ? `:${r.port}` : '';
  switch (r.via) {
    case 'env': {
      if (r.to.key === 'secrets') return `${r.variable} → secret ${r.to.name}`;
      const extra = Object.entries(r.fields || {}).map(([k, v]) => `${k} ${v}`).join(', ');
      const target = r.to.key === 'services' ? `${r.to.name}${port}` : r.to.name;
      return `${r.variable} → ${target}${r.refKey ? ` · ${r.refKey}` : ''}${extra ? ` (${extra})` : ''}`;
    }
    case 'imageCredentials': return `registry login → secret ${r.to.name}`;
    case 'bindings': return `binding ${r.to.name}`;
    case 'credential': return `${r.field} → secret ${r.to.name}`;
    case 'route': return `${r.path || '?'} → ${r.to.name}${port}`;
    default: return `${r.field} → ${r.to.name}`;
  }
}

// referencedSecrets are the names of the secrets spec references, sorted.
export function referencedSecrets(spec) {
  const s = new Set();
  for (const r of referencesOf(spec)) if (r.to.key === 'secrets') s.add(r.to.name);
  return [...s].sort();
}

// secretNames are the secrets a draft knows: those it pins and those it
// references, sorted.
export function secretNames(spec) {
  const s = new Set(objectsOf(spec, 'secrets').map(([n]) => n));
  for (const n of referencedSecrets(spec)) s.add(n);
  return [...s].sort();
}

// removeWithRefs removes key/name and what references it: the variables
// that name it, the routes to a service, a binding's entries in the
// lists, a registry login on a removed secret. A secret's pin goes with
// the secret.
export function removeWithRefs(spec, key, name) {
  let out = removeObject(spec, key, name);
  const hits = (v) => {
    const r = valueRef(v);
    return r && r.to.key === key && r.to.name === name;
  };
  if (key === 'services' || key === 'addons' || key === 'secrets') out = mapEnvValues(out, (v) => (hits(v) ? undefined : v));
  if (key === 'services') {
    for (const [rn, r] of objectsOf(out, 'routes')) if (isObject(r) && r.service === name) out = removeObject(out, 'routes', rn);
  }
  if (key === 'bindings') {
    for (const k of ['services', 'jobs']) {
      for (const [n, o] of objectsOf(out, k)) {
        if (isObject(o) && Array.isArray(o.bindings) && o.bindings.includes(name)) {
          const rest = o.bindings.filter((b) => b !== name);
          const next = { ...o, bindings: rest };
          if (rest.length === 0) delete next.bindings;
          out = setObject(out, k, n, next);
        }
      }
    }
  }
  return out;
}

// SERVICE_KEYS are the keys of a reference to a service.
export const SERVICE_KEYS = ['url', 'host', 'port'];

// refTargets are what a variable may reference, for the forms' and the
// list's "Add reference": the addons with their output keys and fields
// (from the catalog), the image services with their ports (not self), and
// the secrets (the draft's and the environment's).
export function refTargets(spec, catalog, { self, knownSecrets = [] } = {}) {
  const addons = objectsOf(spec, 'addons').map(([name, a]) => {
    const bp = catalog && isObject(a) ? catalog.byType.get(a.type) : null;
    return { name, type: isObject(a) ? a.type : '', keys: bp ? bp.outputs : [], fields: bp ? bp.fields : [] };
  });
  const services = objectsOf(spec, 'services')
    .filter(([n, s]) => n !== self && isObject(s) && s.runtime !== 'attached')
    .map(([name, s]) => ({ name, ports: (Array.isArray(s.ports) ? s.ports : []).map((p) => p && (p.name || p.port)).filter(Boolean) }));
  const secrets = [...new Set([...secretNames(spec), ...knownSecrets])].sort();
  return { addons, services, secrets };
}

// suggestVarName proposes a variable's name for a reference: the
// catalog's suggestion for an addon's key when it has one, DATABASE_URL
// for a postgres url, else <TARGET>_<KEY> in upper case; a secret's name
// in upper case.
export function suggestVarName(target, { type = '', key = '', suggestion = '' } = {}) {
  if (suggestion) return suggestion;
  const up = (s) => String(s || '').toUpperCase().replace(/[^A-Z0-9]+/g, '_').replace(/^_+|_+$/g, '');
  if (target.key === 'secrets') return up(target.name);
  if (target.key === 'addons' && type === 'postgres' && key === 'url') return 'DATABASE_URL';
  return [up(target.name), up(key)].filter(Boolean).join('_');
}

// makeRef is the env value of a reference: {addon, key, …fields},
// {service, port, key} or {secret}.
export function makeRef(target, { key = '', port = '', fields = {} } = {}) {
  if (target.key === 'secrets') return { secret: target.name };
  if (target.key === 'services') {
    const v = { service: target.name };
    if (port !== '' && port !== undefined) v.port = port;
    if (key) v.key = key;
    return v;
  }
  const v = { addon: target.name };
  if (key) v.key = key;
  for (const [k, f] of Object.entries(fields)) if (f) v[k] = f;
  return v;
}

// addVariable is spec with variable name of key/obj set to value.
export function addVariable(spec, key, obj, name, value) {
  const o = getObject(spec, key, obj);
  if (!isObject(o)) return spec;
  return setObject(spec, key, obj, { ...o, env: { ...(isObject(o.env) ? o.env : {}), [name]: value } });
}
