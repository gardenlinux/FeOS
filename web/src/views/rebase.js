// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Rebase, object by object (ADR 0032, "The draft lives in the browser"):
// a draft whose base is no longer the latest deployment takes its changes
// since the base, per object (kind and name), onto the latest
// deployment's spec. An object changed in the draft and since the base
// differently is a conflict, with both versions; the person keeps one.
// Pure.

import { isObject } from './draft.js';

// canonical is v as JSON with every object's keys sorted, so that two
// specs compare by content and not by the order of their keys.
export function canonical(v) {
  if (Array.isArray(v)) return '[' + v.map(canonical).join(',') + ']';
  if (isObject(v)) {
    return '{' + Object.keys(v).sort().map((k) => JSON.stringify(k) + ':' + canonical(v[k])).join(',') + '}';
  }
  return JSON.stringify(v === undefined ? null : v);
}

export function same(a, b) {
  return canonical(a) === canonical(b);
}

// units are a spec's objects by "<key>/<name>"; a top-level field that is
// not a map of objects (volumes, anything a later ferund adds) is one
// unit of its own, by its key.
export function units(spec) {
  const out = new Map();
  for (const [key, v] of Object.entries(spec || {})) {
    if (isObject(v) && Object.values(v).every((o) => isObject(o))) {
      for (const [name, o] of Object.entries(v)) out.set(`${key}/${name}`, o);
    } else {
      out.set(key, v);
    }
  }
  return out;
}

// changesSince lists the units that differ between base and next: {unit,
// change: add | remove | update}, sorted.
export function changesSince(base, next) {
  const a = units(base);
  const b = units(next);
  const out = [];
  for (const u of new Set([...a.keys(), ...b.keys()])) {
    if (!a.has(u)) out.push({ unit: u, change: 'add' });
    else if (!b.has(u)) out.push({ unit: u, change: 'remove' });
    else if (!same(a.get(u), b.get(u))) out.push({ unit: u, change: 'update' });
  }
  return out.sort((x, y) => x.unit.localeCompare(y.unit));
}

// setUnit is spec with unit set to v, or removed when v is undefined.
export function setUnit(spec, unit, v) {
  const out = { ...(spec || {}) };
  const i = unit.indexOf('/');
  if (i < 0) {
    if (v === undefined) delete out[unit];
    else out[unit] = v;
    return out;
  }
  const key = unit.slice(0, i);
  const name = unit.slice(i + 1);
  const m = { ...(isObject(out[key]) ? out[key] : {}) };
  if (v === undefined) delete m[name];
  else m[name] = v;
  if (Object.keys(m).length === 0) delete out[key];
  else out[key] = m;
  return out;
}

// rebase applies draft's changes since base onto latest: {spec,
// conflicts, applied}. A conflict {unit, base, mine, theirs} (undefined
// for absent) keeps theirs in spec until resolve picks.
export function rebase(base, draft, latest) {
  const b = units(base);
  const l = units(latest);
  const d = units(draft);
  let spec = { ...(latest || {}) };
  const conflicts = [];
  const applied = [];
  for (const { unit } of changesSince(base, draft)) {
    const mine = d.get(unit);
    const theirs = l.get(unit);
    const was = b.get(unit);
    const theyChanged = !same(was, theirs) || b.has(unit) !== l.has(unit);
    if (theyChanged && !(same(mine, theirs) && d.has(unit) === l.has(unit))) {
      conflicts.push({ unit, base: was, mine, theirs });
      continue;
    }
    spec = setUnit(spec, unit, mine);
    applied.push(unit);
  }
  return { spec, conflicts, applied };
}

// resolve is spec with a conflict decided: "mine" or "theirs".
export function resolve(spec, conflict, choice) {
  return setUnit(spec, conflict.unit, choice === 'mine' ? conflict.mine : conflict.theirs);
}
