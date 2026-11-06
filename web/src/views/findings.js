// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// A plan's findings at the object and field their path names (ADR 0032):
// "services.web.ports[1].port" is the object services/web, its field
// ports; a finding without a path, or with one that names no object, is
// the spec's as a whole. Pure.

import { isObject, kindOf } from './draft.js';

// splitPath reads a finding's path against the spec: {key, name, rest},
// rest relative to the object ("ports[1].port", "" for the object
// itself); null for a path that names no object. Names are matched
// against the spec's own, longest first, since a secret's may hold dots.
export function splitPath(path, spec) {
  const p = String(path || '');
  const dot = p.indexOf('.');
  if (dot <= 0) return null;
  const key = p.slice(0, dot);
  if (!kindOf(key)) return null;
  const after = p.slice(dot + 1);
  const names = spec && isObject(spec[key]) ? Object.keys(spec[key]) : [];
  names.sort((a, b) => b.length - a.length);
  for (const n of names) {
    if (after === n) return { key, name: n, rest: '' };
    if (after.startsWith(n) && (after[n.length] === '.' || after[n.length] === '[')) {
      return { key, name: n, rest: after.slice(n.length + (after[n.length] === '.' ? 1 : 0)) };
    }
  }
  const m = /^([^.[]+)(.*)$/.exec(after);
  if (!m) return null;
  return { key, name: m[1], rest: m[2].replace(/^\./, '') };
}

export function unitKey(key, name) {
  return `${key}/${name}`;
}

// groupFindings sorts findings to where they show: global (no object),
// and by object ("services/web") with each finding's rest.
export function groupFindings(findings, spec) {
  const global = [];
  const objects = new Map();
  for (const f of findings || []) {
    const at = splitPath(f.path, spec);
    if (!at) {
      global.push(f);
      continue;
    }
    const k = unitKey(at.key, at.name);
    if (!objects.has(k)) objects.set(k, []);
    objects.get(k).push({ ...f, rest: at.rest });
  }
  return { global, objects };
}

// fieldOf is a rest's first field: "ports[1].port" is ports,
// "env.DATABASE_URL" env.
export function fieldOf(rest) {
  const m = /^[^.[]*/.exec(String(rest || ''));
  return m ? m[0] : '';
}

// atField are the findings of one field: field "env" takes every
// variable's, "env.X" X's only.
export function atField(list, field) {
  return (list || []).filter((f) => {
    const r = f.rest || '';
    return r === field || r.startsWith(field + '.') || r.startsWith(field + '[');
  });
}

// notAt are the findings of an object that none of the fields take: they
// show at the object's head.
export function notAt(list, fields) {
  return (list || []).filter((f) => !fields.some((fd) => atField([f], fd).length > 0));
}

const RANK = { error: 3, warning: 2, info: 1 };

// worst is the gravest severity among findings, '' for none.
export function worst(list) {
  let best = '';
  for (const f of list || []) if ((RANK[f.severity] || 0) > (RANK[best] || 0)) best = f.severity;
  return best;
}

// counts the findings by severity.
export function severityCounts(list) {
  const c = { error: 0, warning: 0, info: 0 };
  for (const f of list || []) if (f.severity in c) c[f.severity]++;
  return c;
}
