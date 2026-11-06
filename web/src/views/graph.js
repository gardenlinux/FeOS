// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The builder's graph (ADR 0032, "The graph", "Drawn by hand"): the nodes
// and edges of a spec, their layout, the edges' paths, hit testing and
// what a drop does. The layout is automatic and the same for everyone:
// columns by kind (the entry; services, then jobs; addons and bindings;
// secrets; outside the environment's frame the attached services'
// sandboxes), in each column an order by the mean position of a node's
// neighbours, ties by name. A service another service calls stands in a
// column of the services right of its caller, so that every reference
// runs left to right. No position is stored. Pure; Graph.jsx renders it.

import { isObject, keyOfKind, objectsOf, setObject, uniqueName, getObject } from './draft.js';
import { referencesOf, secretNames } from './refs.js';
import { unitKey } from './findings.js';

// The sizes the layout places nodes with; Graph.jsx draws them with the
// same (its CSS fits the content into them).
export const SIZE = {
  nodeW: 180, entryW: 196, gapX: 132, gapY: 18, pad: 24, frameTop: 30, framePad: 14,
  head: 44, row: 18, foot: 8, margin: 6,
};

export const ENTRY = 'entry';

// The kinds' columns: services' columns are 1…k (k ≥ 1), the others
// follow them.
const GROUP = { services: 0, jobs: 1, addons: 0, bindings: 1, secrets: 0, sandbox: 0, entry: 0 };

// portIndex is the index of a service's port a reference or route names
// (by name or number; none named: the first), -1 for one it lacks.
export function portIndex(ports, p) {
  if (!ports.length) return -1;
  if (p === undefined || p === '' || p === null) return 0;
  return ports.findIndex((x) => x.name === p || String(x.port) === String(p));
}

function portsOf(o) {
  return (isObject(o) && Array.isArray(o.ports) ? o.ports : [])
    .filter(isObject).map((p) => ({ name: p.name || '', port: p.port }));
}

// graphOf are a spec's nodes and edges. entry is the environment's door:
// {public, hosts, env}. removed are objects the plan removes ({key,
// name}), drawn as ghosts.
export function graphOf(spec, { entry = {}, removed = [] } = {}) {
  const nodes = [];
  const byId = new Map();
  const add = (n) => {
    if (byId.has(n.id)) return byId.get(n.id);
    nodes.push(n);
    byId.set(n.id, n);
    return n;
  };
  const hosts = entry.public && Array.isArray(entry.hosts) ? entry.hosts : [];
  add({ id: ENTRY, key: 'entry', name: hosts.length ? hosts[0] : 'route door', hosts, public: hosts.length > 0, env: entry.env || '', ports: [] });
  for (const key of ['services', 'jobs', 'addons', 'bindings']) {
    for (const [name, o] of objectsOf(spec, key)) {
      const n = { id: unitKey(key, name), key, name, ports: key === 'services' ? portsOf(o) : [] };
      if (key === 'services' && isObject(o) && o.runtime === 'attached') {
        n.attached = true;
        n.sandbox = o.sandbox || '';
      }
      if (key === 'addons' && isObject(o)) n.type = o.type || '';
      add(n);
    }
  }
  for (const name of secretNames(spec)) add({ id: unitKey('secrets', name), key: 'secrets', name, ports: [] });
  for (const n of [...nodes]) {
    if (n.attached && n.sandbox) add({ id: `sandbox/${n.sandbox}`, key: 'sandbox', name: n.sandbox, ports: [] });
  }

  const edges = [];
  for (const r of referencesOf(spec)) {
    const toId = unitKey(r.to.key, r.to.name);
    if (!byId.has(toId)) add({ id: toId, key: r.to.key, name: r.to.name, ports: [], missing: true });
    const route = r.via === 'route';
    const from = route ? ENTRY : unitKey(r.from.key, r.from.name);
    const target = byId.get(toId);
    const e = {
      id: route ? `route/${r.from.name}` : `${from}|${r.field}`,
      from, to: toId, via: r.via, field: r.field, dashed: !!r.dashed,
      unit: unitKey(r.from.key, r.from.name),
      port: r.to.key === 'services' ? portIndex(target.ports, r.port) : -1,
      label: edgeLabel(r),
    };
    edges.push(e);
  }
  for (const n of nodes) {
    if (n.attached && n.sandbox) {
      edges.push({ id: `${n.id}|sandbox`, from: n.id, to: `sandbox/${n.sandbox}`, via: 'sandbox', field: 'sandbox',
        dashed: true, unit: n.id, port: -1, label: 'runs in' });
    }
  }
  for (const { key, name } of removed) {
    add({ id: unitKey(key, name), key, name, ports: [], removed: true });
  }
  return { nodes, edges };
}

// edgeLabel is an edge's label: the variable and the key, the route's
// path.
export function edgeLabel(r) {
  switch (r.via) {
    case 'env': return r.refKey ? `${r.variable} · ${r.refKey}` : r.variable;
    case 'imageCredentials': return 'registry login';
    case 'bindings': return 'binding';
    case 'credential': return r.field.replace(/^credential\./, '');
    case 'route': return r.path || '?';
    default: return r.field;
  }
}

// removedOf are the objects a plan's diff removes and the spec lacks.
export function removedOf(diff, spec) {
  const out = [];
  for (const d of diff || []) {
    const key = keyOfKind(d.kind);
    if (d.change !== 'remove' || !key || key === 'routes') continue;
    if (key === 'secrets' ? secretNames(spec).includes(d.name) : getObject(spec, key, d.name) !== undefined) continue;
    out.push({ key, name: d.name });
  }
  return out;
}

// diffMarks are the plan's changes by node or route: "services/web" →
// add, update or remove.
export function diffMarks(diff) {
  const m = new Map();
  for (const d of diff || []) {
    const key = keyOfKind(d.kind);
    if (key) m.set(unitKey(key, d.name), d.change);
  }
  return m;
}

// columnsOf gives every node its column: the entry 0, services and jobs
// 1…k by the depth of the service references among them (a callee right
// of its callers; a cycle is cut where the walk meets it again), then the
// addons and bindings, the secrets, and the sandboxes.
export function columnsOf(graph) {
  const calls = new Map();
  for (const e of graph.edges) {
    if (e.via === 'env' && e.to.startsWith('services/') && e.from !== e.to) {
      if (!calls.has(e.to)) calls.set(e.to, []);
      calls.get(e.to).push(e.from);
    }
  }
  const depth = new Map();
  const walk = (id, on) => {
    if (depth.has(id)) return depth.get(id);
    on.add(id);
    let d = 0;
    for (const c of (calls.get(id) || []).slice().sort()) {
      if (on.has(c)) continue;
      d = Math.max(d, walk(c, on) + 1);
    }
    on.delete(id);
    depth.set(id, d);
    return d;
  };
  const col = new Map();
  let k = 1;
  for (const n of graph.nodes) {
    if (n.key === 'services' || n.key === 'jobs') {
      const d = n.missing || n.removed ? 0 : walk(n.id, new Set());
      k = Math.max(k, d + 1);
      col.set(n.id, 1 + d);
    }
  }
  const hasSandbox = graph.nodes.some((n) => n.key === 'sandbox');
  for (const n of graph.nodes) {
    if (n.key === 'entry') col.set(n.id, 0);
    else if (n.key === 'addons' || n.key === 'bindings') col.set(n.id, 1 + k);
    else if (n.key === 'secrets') col.set(n.id, 2 + k);
    else if (n.key === 'sandbox') col.set(n.id, 3 + k);
  }
  return { col, count: hasSandbox ? 4 + k : 3 + k, envFirst: 1, envLast: 2 + k };
}

const byName = (a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);

// neighbours are each node's neighbours by id.
function neighbours(graph) {
  const m = new Map(graph.nodes.map((n) => [n.id, []]));
  for (const e of graph.edges) {
    if (e.from === e.to) continue;
    m.get(e.from).push(e.to);
    m.get(e.to).push(e.from);
  }
  return m;
}

// orderOf orders each column: by kind (services before jobs, addons
// before bindings), then by the mean position of the neighbours in other
// columns, ties by name; a few sweeps right and back.
export function orderOf(graph, cols, sweeps = 4) {
  const nb = neighbours(graph);
  const columns = Array.from({ length: cols.count }, () => []);
  for (const n of graph.nodes) columns[cols.col.get(n.id)].push(n);
  for (const c of columns) c.sort((a, b) => GROUP[a.key] - GROUP[b.key] || byName(a, b));
  const pos = new Map();
  const index = () => {
    for (const c of columns) c.forEach((n, i) => pos.set(n.id, (i + 0.5) / c.length));
  };
  index();
  const sortColumn = (ci) => {
    const c = columns[ci];
    const bary = new Map();
    for (const n of c) {
      const ps = nb.get(n.id).filter((id) => cols.col.get(id) !== ci).map((id) => pos.get(id));
      bary.set(n.id, ps.length ? ps.reduce((s, p) => s + p, 0) / ps.length : pos.get(n.id));
    }
    c.sort((a, b) => GROUP[a.key] - GROUP[b.key] || bary.get(a.id) - bary.get(b.id) || byName(a, b));
    c.forEach((n, i) => pos.set(n.id, (i + 0.5) / c.length));
  };
  for (let s = 0; s < sweeps; s++) {
    for (let ci = 1; ci < columns.length; ci++) sortColumn(ci);
    for (let ci = columns.length - 2; ci >= 0; ci--) sortColumn(ci);
  }
  return columns;
}

// nodeHeight fits a node's lines: its head, a service's ports, the
// entry's hosts, and room for its outgoing edges.
export function nodeHeight(n, outs = 0) {
  const lines = n.key === 'services' ? n.ports.length : n.key === 'entry' ? Math.max(1, n.hosts.length) : 0;
  const h = SIZE.head + lines * SIZE.row + SIZE.foot;
  return Math.max(h, SIZE.head / 2 + Math.max(0, outs - 1) * SIZE.row + SIZE.row);
}

// layoutGraph places the graph: {nodes: Map id → {…node, x, y, w, h,
// col}, edges: [{…edge, d, sx, sy, tx, ty}], width, height, frame}.
export function layoutGraph(graph) {
  const cols = columnsOf(graph);
  const columns = orderOf(graph, cols);
  const outs = new Map(graph.nodes.map((n) => [n.id, 0]));
  for (const e of graph.edges) outs.set(e.from, outs.get(e.from) + 1);
  // An empty column takes no room, but the first of the services'.
  const xs = [];
  const ws = [];
  let x = SIZE.pad;
  for (let ci = 0; ci < cols.count; ci++) {
    xs.push(x);
    const w = ci === 0 ? SIZE.entryW : columns[ci].length || ci === 1 ? SIZE.nodeW : 0;
    ws.push(w);
    if (w) x += w + SIZE.gapX;
  }
  const width = x - SIZE.gapX + SIZE.pad;
  let envLast = cols.envFirst;
  for (let ci = cols.envFirst; ci <= cols.envLast; ci++) if (ws[ci]) envLast = ci;
  const placed = new Map();
  const top = SIZE.pad + SIZE.frameTop;
  const centre = (id, other) => {
    const p = placed.get(id);
    if (!p) return null;
    const e = graph.edges.find((x) => (x.from === id && x.to === other) || (x.to === id && x.from === other));
    const pi = e && e.to === id ? e.port : -1;
    return pi >= 0 ? portY(p, pi) : p.y + SIZE.head / 2;
  };
  const nb = neighbours(graph);
  // The first column of services stacks from the top; every other column
  // sits where its neighbours are, in its order, without overlap.
  const place = (ci, stack) => {
    let bottom = top - SIZE.gapY;
    for (const n of columns[ci]) {
      const w = ci === 0 ? SIZE.entryW : SIZE.nodeW;
      const h = nodeHeight(n, outs.get(n.id));
      let y = bottom + SIZE.gapY;
      if (!stack) {
        const cs = nb.get(n.id).map((id) => centre(id, n.id)).filter((v) => v !== null);
        if (cs.length) y = Math.max(y, Math.round(cs.reduce((s, v) => s + v, 0) / cs.length - SIZE.head / 2));
      }
      placed.set(n.id, { ...n, x: xs[ci], y, w, h, col: ci });
      bottom = y + h;
    }
  };
  place(1, true);
  for (let ci = 2; ci < cols.count; ci++) place(ci, false);
  place(0, false);
  let height = top;
  let envBottom = top;
  for (const p of placed.values()) {
    height = Math.max(height, p.y + p.h);
    if (p.col >= cols.envFirst && p.col <= cols.envLast) envBottom = Math.max(envBottom, p.y + p.h);
  }
  const frame = {
    x: xs[cols.envFirst] - SIZE.framePad, y: SIZE.pad,
    w: xs[envLast] + SIZE.nodeW + 2 * SIZE.framePad - xs[cols.envFirst],
    h: envBottom + SIZE.framePad - SIZE.pad,
  };
  const edges = routeEdges(graph, placed, columns, xs, ws);
  return { nodes: placed, edges, width, height: Math.max(height, frame.y + frame.h) + SIZE.pad, frame, columns: cols.count };
}

// portY is the middle of a service's port line.
export function portY(p, i) {
  return p.y + SIZE.head + i * SIZE.row + SIZE.row / 2;
}

// inPoint is where an edge ends: a service's port line, else the node's
// head, on its left side.
function inPoint(p, port) {
  return { x: p.x, y: port >= 0 && port < p.ports.length ? portY(p, port) : p.y + SIZE.head / 2 };
}

// routeEdges gives each edge its path. A node's outgoing edges leave its
// right side one line apart, in the order of their ends' heights; an edge
// that passes a column runs through the gap there nearest its line.
export function routeEdges(graph, placed, columns, xs, ws) {
  const ends = graph.edges.map((e) => {
    const t = placed.get(e.to);
    return { e, t: inPoint(t, e.port) };
  });
  const byFrom = new Map();
  for (const x of ends) {
    if (!byFrom.has(x.e.from)) byFrom.set(x.e.from, []);
    byFrom.get(x.e.from).push(x);
  }
  const out = [];
  for (const [from, list] of byFrom) {
    const s = placed.get(from);
    list.sort((a, b) => a.t.y - b.t.y || (a.e.id < b.e.id ? -1 : 1));
    list.forEach((x, i) => {
      const sp = { x: s.x + s.w, y: s.y + SIZE.head / 2 + i * SIZE.row };
      const t = placed.get(x.e.to);
      const pts = [sp];
      if (t.col > s.col) {
        for (let ci = s.col + 1; ci < t.col; ci++) {
          const col = columns[ci].map((n) => placed.get(n.id));
          if (!ws[ci]) continue;
          const cx = xs[ci];
          const cw = ws[ci];
          const f = (cx + cw / 2 - sp.x) / (x.t.x - sp.x || 1);
          const y = gapY(col, sp.y + (x.t.y - sp.y) * f);
          pts.push({ x: cx, y }, { x: cx + cw, y });
        }
      }
      pts.push(x.t);
      out.push({ ...x.e, d: pathOf(pts, t.col <= s.col), sx: sp.x, sy: sp.y, tx: x.t.x, ty: x.t.y });
    });
  }
  return out;
}

// gapY is y, or the nearest height in a column's gaps between its nodes.
export function gapY(col, y) {
  const m = SIZE.margin;
  const blocks = col.map((p) => [p.y - m, p.y + p.h + m]).sort((a, b) => a[0] - b[0]);
  const hit = blocks.find(([a, b]) => y > a && y < b);
  if (!hit) return y;
  const gaps = [[-Infinity, blocks[0][0]]];
  for (let i = 1; i < blocks.length; i++) if (blocks[i][0] > blocks[i - 1][1]) gaps.push([blocks[i - 1][1], blocks[i][0]]);
  gaps.push([blocks[blocks.length - 1][1], Infinity]);
  let best = null;
  for (const [a, b] of gaps) {
    const c = Math.min(Math.max(y, a), b);
    if (best === null || Math.abs(c - y) < Math.abs(best - y)) best = c;
  }
  return best;
}

// pathOf is an SVG path through points, a horizontal run inside a column
// and a smooth curve between columns; back is an edge to a column not
// right of its start, which loops out and back in.
export function pathOf(pts, back = false) {
  const r = (v) => Math.round(v * 10) / 10;
  const [s, ...rest] = pts;
  if (back) {
    const t = rest[rest.length - 1];
    const k = 60;
    return `M${r(s.x)},${r(s.y)} C${r(s.x + k)},${r(s.y)} ${r(t.x - k)},${r(t.y)} ${r(t.x)},${r(t.y)}`;
  }
  let d = `M${r(s.x)},${r(s.y)}`;
  let a = s;
  for (const b of rest) {
    if (a.y === b.y) d += ` L${r(b.x)},${r(b.y)}`;
    else {
      const dx = (b.x - a.x) / 2;
      d += ` C${r(a.x + dx)},${r(a.y)} ${r(b.x - dx)},${r(b.y)} ${r(b.x)},${r(b.y)}`;
    }
    a = b;
  }
  return d;
}

// hitTest is the node at x, y with the port line under it ({id, port},
// port -1 off the ports), or null.
export function hitTest(layout, x, y) {
  for (const p of layout.nodes.values()) {
    if (x < p.x || x > p.x + p.w || y < p.y || y > p.y + p.h) continue;
    let port = -1;
    if (p.ports.length) {
      const i = Math.floor((y - p.y - SIZE.head) / SIZE.row);
      if (i >= 0 && i < p.ports.length) port = i;
    }
    return { id: p.id, port };
  }
  return null;
}

// portValue is how a reference or route names a service's port i: by
// its name, else its number.
export function portValue(node, i) {
  const p = node.ports[i >= 0 ? i : 0];
  if (!p) return '';
  return p.name || p.port;
}

// canDrag says whether a node has a handle: services and jobs (not an
// attached service's), and the entry.
export function canDrag(n) {
  return !!n && !n.missing && !n.removed && (n.key === 'entry' || n.key === 'jobs' || (n.key === 'services' && !n.attached));
}

// dropAction is what dropping from's handle on hit does: a reference
// ({kind: 'ref', target: {key, name}, port}), a binding ({kind:
// 'binding', name}), a route ({kind: 'route', service, port}), or null.
export function dropAction(layout, from, hit, spec) {
  if (!hit || hit.id === from) return null;
  const f = layout.nodes.get(from);
  const t = layout.nodes.get(hit.id);
  if (!f || !t || t.missing || t.removed) return null;
  const image = t.key === 'services' && !t.attached && t.ports.length > 0;
  if (f.key === 'entry') {
    return image ? { kind: 'route', service: t.name, port: portValue(t, hit.port) } : null;
  }
  if (t.key === 'addons' || t.key === 'secrets') return { kind: 'ref', target: { key: t.key, name: t.name } };
  if (image) return { kind: 'ref', target: { key: 'services', name: t.name }, port: portValue(t, hit.port) };
  if (t.key === 'bindings') {
    const o = getObject(spec, f.key, f.name);
    if (isObject(o) && Array.isArray(o.bindings) && o.bindings.includes(t.name)) return null;
    return { kind: 'binding', name: t.name };
  }
  return null;
}

// dropTargets are the nodes from's handle can be dropped on, for the
// keyboard's choice.
export function dropTargets(layout, from, spec) {
  const out = [];
  for (const p of layout.nodes.values()) {
    if (p.ports.length && layout.nodes.get(from) && layout.nodes.get(from).key === 'entry') {
      p.ports.forEach((_, i) => {
        const a = dropAction(layout, from, { id: p.id, port: i }, spec);
        if (a) out.push({ id: p.id, port: i, action: a });
      });
      continue;
    }
    const a = dropAction(layout, from, { id: p.id, port: -1 }, spec);
    if (a) out.push({ id: p.id, port: -1, action: a });
  }
  return out;
}

// firstRoutePath is the path a drawn route proposes: / for the first,
// none (asked for) for the others.
export function firstRoutePath(spec) {
  return objectsOf(spec, 'routes').length === 0 ? '/' : '';
}

// addRoute adds a route from path to service's port, named after its
// path (root for /). It returns {spec, name}.
export function addRoute(spec, service, port, path) {
  const base = path === '/' ? 'root' : String(path).replace(/^\/+|\/+$/g, '') || 'route';
  const name = uniqueName(spec, 'routes', base);
  const r = { path, service };
  if (port !== '' && port !== undefined) r.port = port;
  return { spec: setObject(spec, 'routes', name, r), name };
}

// addBinding adds binding to key/name's bindings.
export function addBinding(spec, key, name, binding) {
  const o = getObject(spec, key, name);
  if (!isObject(o)) return spec;
  const list = Array.isArray(o.bindings) ? o.bindings : [];
  if (list.includes(binding)) return spec;
  return setObject(spec, key, name, { ...o, bindings: [...list, binding] });
}

// focusOf are the nodes and edges next to unit (an object's form or a
// route's): the unit, its neighbours, and the edges between them.
export function focusOf(graph, unit) {
  const nodes = new Set();
  const edges = new Set();
  if (!unit) return null;
  if (unit.startsWith('routes/')) {
    for (const e of graph.edges) {
      if (e.unit === unit) {
        edges.add(e.id);
        nodes.add(e.from);
        nodes.add(e.to);
      }
    }
    return nodes.size ? { nodes, edges } : null;
  }
  if (!graph.nodes.some((n) => n.id === unit)) return null;
  nodes.add(unit);
  for (const e of graph.edges) {
    if (e.from === unit || e.to === unit) {
      edges.add(e.id);
      nodes.add(e.from);
      nodes.add(e.to);
    }
  }
  return { nodes, edges };
}
