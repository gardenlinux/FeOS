// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The builder's graph (ADR 0032, "The graph", "Drawn by hand"): the
// architecture of a spec, not every variable. Nodes are the entry, the
// services, jobs, addons and bindings (and an attached service's sandbox);
// one edge per pair of objects, the references between them folded into
// it (a route from the entry, a variable to a service or an addon, a
// binding). Secrets and registry logins are no edges: a node carries them
// as markers, and the builder lists the secrets beside the graph.
//
// The layout is by dependency and the same for everyone: columns by the
// depth from the entry along routes and references (the entry; what the
// routes reach; what those call; …; the addons and bindings last), so that
// every edge runs left to right (a cycle is cut where the walk closes it).
// What no route reaches (a job, a load generator) goes into a lane below,
// one column left of the nearest object it references. In each column an
// order by the mean position of a node's neighbours, ties by name. No
// position is stored. Pure; GraphView.jsx renders it.

import { isObject, keyOfKind, objectsOf, setObject, uniqueName, getObject } from './draft.js';
import { referencesOf, refLine, secretNames } from './refs.js';
import { unitKey } from './findings.js';

// The sizes the layout places nodes with; GraphView.jsx draws them with
// the same (its CSS fits the content into them).
export const SIZE = {
  nodeW: 144, gapX: 96, gapY: 16, pad: 12, laneGap: 44, margin: 5,
  object: 70, small: 56, entryHead: 40, host: 15, spread: 13,
};

export const ENTRY = 'entry';

// The order of kinds within a column, before the barycentre.
const GROUP = { entry: 0, services: 0, jobs: 1, addons: 0, bindings: 1, sandbox: 0 };
const SINKS = new Set(['addons', 'bindings']);

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

// edgeVia is what an edge stands for: a route, a call (to a service), data
// (to an addon), a binding, or a sandbox.
function edgeVia(r) {
  if (r.via === 'route') return 'route';
  if (r.via === 'bindings') return 'binding';
  if (r.to.key === 'services') return 'call';
  if (r.to.key === 'addons') return 'data';
  return r.via;
}

// refLabel is one reference's word on an edge: the variable, the route's
// path, "binding".
function refLabel(r) {
  switch (r.via) {
    case 'env': return r.variable;
    case 'route': return r.path || '?';
    case 'bindings': return 'binding';
    default: return r.field;
  }
}

// graphOf is a spec's graph: {nodes, edges, secrets}. entry is the
// environment's door: {public, hosts, env}. removed are objects the
// draft removes ({key, name}), drawn as ghosts. A node holds its secrets
// (by variable or a binding's credential) and its registry logins'
// secrets as markers; secrets lists every secret with who uses it.
export function graphOf(spec, { entry = {}, removed = [] } = {}) {
  const nodes = [];
  const byId = new Map();
  const add = (n) => {
    if (byId.has(n.id)) return byId.get(n.id);
    const node = { ports: [], secrets: [], logins: [], ...n };
    nodes.push(node);
    byId.set(node.id, node);
    return node;
  };
  const hosts = entry.public && Array.isArray(entry.hosts) ? entry.hosts : [];
  add({ id: ENTRY, key: 'entry', name: hosts.length ? hosts[0] : 'route door', hosts, public: hosts.length > 0, env: entry.env || '' });
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
  for (const n of [...nodes]) {
    if (n.attached && n.sandbox) add({ id: `sandbox/${n.sandbox}`, key: 'sandbox', name: n.sandbox });
  }

  const secrets = new Map(secretNames(spec).map((name) => [name, { name, users: [] }]));
  const pairs = new Map();
  for (const r of referencesOf(spec)) {
    const holder = unitKey(r.from.key, r.from.name);
    if (r.to.key === 'secrets') {
      const n = byId.get(holder);
      const list = r.via === 'imageCredentials' ? n.logins : n.secrets;
      if (!list.includes(r.to.name)) list.push(r.to.name);
      const s = secrets.get(r.to.name);
      if (s) s.users.push({ id: holder, field: r.field, login: r.via === 'imageCredentials' });
      continue;
    }
    const toId = unitKey(r.to.key, r.to.name);
    if (!byId.has(toId)) add({ id: toId, key: r.to.key, name: r.to.name, missing: true });
    const route = r.via === 'route';
    const from = route ? ENTRY : holder;
    const id = `${from}>${toId}`;
    if (!pairs.has(id)) {
      pairs.set(id, { id, from, to: toId, via: edgeVia(r), dashed: r.from.key === 'jobs', refs: [] });
    }
    pairs.get(id).refs.push({ unit: route ? unitKey('routes', r.from.name) : holder, field: r.field, label: refLabel(r), line: refLine(r) });
  }
  const edges = [...pairs.values()].map(foldEdge);
  for (const n of nodes) {
    if (n.attached && n.sandbox) {
      edges.push(foldEdge({ id: `${n.id}>sandbox/${n.sandbox}`, from: n.id, to: `sandbox/${n.sandbox}`, via: 'sandbox', dashed: true,
        refs: [{ unit: n.id, field: 'sandbox', label: 'runs in', line: `runs in sandbox ${n.sandbox}` }] }));
    }
  }
  for (const { key, name } of removed) {
    add({ id: unitKey(key, name), key, name, removed: true });
  }
  return { nodes, edges, secrets: [...secrets.values()] };
}

// foldEdge gives an edge of folded references its label (the first's
// word and how many more: "ORDERS_URL +1"), its title (every line), its
// unit (the first's object, which a click opens) and its units.
export function foldEdge(e) {
  const units = [...new Set(e.refs.map((r) => r.unit))];
  return {
    ...e, units, unit: units[0], fields: e.refs.map((r) => r.field),
    label: e.refs[0].label, more: e.refs.length - 1, title: e.refs.map((r) => r.line).join('\n'),
  };
}

// removedOf are the objects a plan's diff (or the draft's changes, as
// {kind, name, change}) removes and the spec lacks; routes and secrets
// are no nodes.
export function removedOf(diff, spec) {
  const out = [];
  for (const d of diff || []) {
    const key = keyOfKind(d.kind);
    if (d.change !== 'remove' || !key || key === 'routes' || key === 'secrets') continue;
    if (getObject(spec, key, d.name) !== undefined) continue;
    out.push({ key, name: d.name });
  }
  return out;
}

// changeList reads the draft's changes against its base (rebase.js
// changesSince: {unit, change}) as {key, kind, name, change}.
export function changeList(changes) {
  return (changes || []).map(({ unit, change }) => {
    const i = unit.indexOf('/');
    const key = i < 0 ? unit : unit.slice(0, i);
    return { key, kind: key.replace(/s$/, ''), name: i < 0 ? '' : unit.slice(i + 1), change, unit };
  });
}

// diffMarks are the changes by node or route: "services/web" → add,
// update or remove; from a plan's diff ({kind, name}) or changeList's.
export function diffMarks(diff) {
  const m = new Map();
  for (const d of diff || []) {
    const key = d.key || keyOfKind(d.kind);
    if (key) m.set(unitKey(key, d.name), d.change);
  }
  return m;
}

const byName = (a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
const byId = (a, b) => (a < b ? -1 : a > b ? 1 : 0);

// columnsOf gives every node its column and says which are in the lane:
// {col, lane, back, count, sinks}. The entry is column 0; a node a route
// reaches (through any references) stands at its longest walk from the
// entry; the addons and bindings in the column after the deepest; a
// sandbox after them. A node no route reaches stands in the lane, one
// column left of the nearest object it references (the column before the
// addons when it only references those, column 1 when nothing). back are
// the edges that close a cycle, cut where the walk from the entry meets
// them.
export function columnsOf(graph) {
  const kind = new Map(graph.nodes.map((n) => [n.id, n.key]));
  const out = new Map(graph.nodes.map((n) => [n.id, []]));
  for (const e of graph.edges) if (e.from !== e.to) out.get(e.from).push(e);
  for (const l of out.values()) l.sort((a, b) => byId(a.to, b.to));

  const back = new Set();
  const state = new Map();
  const dfs = (id) => {
    state.set(id, 1);
    for (const e of out.get(id)) {
      if (state.get(e.to) === 1) back.add(e.id);
      else if (!state.has(e.to)) dfs(e.to);
    }
    state.set(id, 2);
  };
  dfs(ENTRY);
  for (const id of graph.nodes.map((n) => n.id).sort(byId)) if (!state.has(id)) dfs(id);
  const fwd = (id) => out.get(id).filter((e) => !back.has(e.id));

  const reached = new Set([ENTRY]);
  const queue = [ENTRY];
  while (queue.length) for (const e of fwd(queue.shift())) if (!reached.has(e.to)) { reached.add(e.to); queue.push(e.to); }

  const preds = new Map(graph.nodes.map((n) => [n.id, []]));
  for (const id of out.keys()) for (const e of fwd(id)) preds.get(e.to).push(id);
  const middle = (id) => id !== ENTRY && !SINKS.has(kind.get(id)) && kind.get(id) !== 'sandbox';

  const col = new Map([[ENTRY, 0]]);
  const depth = (id) => {
    if (col.has(id)) return col.get(id);
    let d = 1;
    for (const p of preds.get(id)) if (reached.has(p) && (p === ENTRY || middle(p))) d = Math.max(d, depth(p) + 1);
    col.set(id, d);
    return d;
  };
  let deepest = 0;
  for (const n of graph.nodes) if (middle(n.id) && reached.has(n.id)) deepest = Math.max(deepest, depth(n.id));
  const lone = graph.nodes.some((n) => middle(n.id) && !reached.has(n.id));
  const sinks = (lone ? Math.max(deepest, 1) : deepest) + 1;

  const laneCol = (id) => {
    if (col.has(id)) return col.get(id);
    col.set(id, 1);
    const vals = [];
    for (const e of fwd(id)) {
      const k = kind.get(e.to);
      if (SINKS.has(k)) vals.push(sinks - 1);
      else if (k !== 'sandbox') vals.push((reached.has(e.to) ? depth(e.to) : laneCol(e.to)) - 1);
    }
    const c = vals.length ? Math.max(0, Math.min(...vals)) : 1;
    col.set(id, c);
    return c;
  };
  const lane = new Set();
  for (const n of graph.nodes) {
    if (n.id === ENTRY) continue;
    if (!reached.has(n.id)) lane.add(n.id);
    if (SINKS.has(n.key)) col.set(n.id, sinks);
    else if (n.key === 'sandbox') col.set(n.id, sinks + 1);
    else if (!reached.has(n.id)) laneCol(n.id);
  }
  const hasSandbox = graph.nodes.some((n) => n.key === 'sandbox');
  return { col, lane, back, sinks, count: sinks + (hasSandbox ? 2 : 1) };
}

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

// orderOf orders each column: the nodes a route reaches before the lane,
// by kind (services before jobs, addons before bindings), then by the
// mean position of the neighbours in other columns, ties by name; two
// sweeps right and back.
export function orderOf(graph, cols, sweeps = 2) {
  const nb = neighbours(graph);
  const columns = Array.from({ length: cols.count }, () => []);
  for (const n of graph.nodes) columns[cols.col.get(n.id)].push(n);
  const part = (n) => (cols.lane.has(n.id) ? 1 : 0);
  for (const c of columns) c.sort((a, b) => part(a) - part(b) || GROUP[a.key] - GROUP[b.key] || byName(a, b));
  const pos = new Map();
  for (const c of columns) c.forEach((n, i) => pos.set(n.id, (i + 0.5) / c.length));
  const sortColumn = (ci) => {
    const c = columns[ci];
    const bary = new Map();
    for (const n of c) {
      const ps = nb.get(n.id).filter((id) => cols.col.get(id) !== ci).map((id) => pos.get(id));
      bary.set(n.id, ps.length ? ps.reduce((s, p) => s + p, 0) / ps.length : pos.get(n.id));
    }
    c.sort((a, b) => part(a) - part(b) || GROUP[a.key] - GROUP[b.key] || bary.get(a.id) - bary.get(b.id) || byName(a, b));
    c.forEach((n, i) => pos.set(n.id, (i + 0.5) / c.length));
  };
  for (let s = 0; s < sweeps; s++) {
    for (let ci = 1; ci < columns.length; ci++) sortColumn(ci);
    for (let ci = columns.length - 2; ci >= 0; ci--) sortColumn(ci);
  }
  return columns;
}

// nodeHeight fits a node's lines (the entry's hosts; a service's or a
// job's image and markers) and room for its edges on either side.
export function nodeHeight(n, edges = 0) {
  let h = SIZE.small;
  if (n.key === 'entry') h = SIZE.entryHead + Math.max(1, n.hosts.length) * SIZE.host + 8;
  else if ((n.key === 'services' || n.key === 'jobs' || n.key === 'bindings') && !n.missing && !n.removed) h = SIZE.object;
  return Math.max(h, (edges + 1) * SIZE.spread);
}

// layoutGraph places the graph: {nodes: Map id → {…node, x, y, w, h,
// col, lane}, edges: [{…edge, d, sx, sy, tx, ty, back}], width, height,
// lane: {y} or null}. The drawing holds every node and edge.
export function layoutGraph(graph) {
  const cols = columnsOf(graph);
  const columns = orderOf(graph, cols);
  const ins = new Map(graph.nodes.map((n) => [n.id, 0]));
  const outs = new Map(graph.nodes.map((n) => [n.id, 0]));
  for (const e of graph.edges) {
    outs.set(e.from, outs.get(e.from) + 1);
    ins.set(e.to, ins.get(e.to) + 1);
  }
  // An empty column takes no room.
  const xs = [];
  const ws = [];
  let x = SIZE.pad;
  for (let ci = 0; ci < cols.count; ci++) {
    xs.push(x);
    const w = columns[ci].length ? SIZE.nodeW : 0;
    ws.push(w);
    if (w) x += w + SIZE.gapX;
  }
  const width = Math.max(x - SIZE.gapX + SIZE.pad, SIZE.pad * 2 + SIZE.nodeW);
  const nb = neighbours(graph);
  const placed = new Map();
  const centre = (id) => {
    const p = placed.get(id);
    return p ? p.y + p.h / 2 : null;
  };
  // A column's nodes of one part sit where their placed neighbours of the
  // same part are, in their order, without overlap.
  const place = (ci, inLane, top) => {
    let bottom = top - SIZE.gapY;
    for (const n of columns[ci]) {
      if (cols.lane.has(n.id) !== inLane) continue;
      const h = nodeHeight(n, Math.max(ins.get(n.id), outs.get(n.id)));
      let y = bottom + SIZE.gapY;
      const cs = nb.get(n.id).filter((id) => cols.lane.has(id) === inLane).map(centre).filter((v) => v !== null);
      if (cs.length) y = Math.max(y, Math.round(cs.reduce((s, v) => s + v, 0) / cs.length - h / 2));
      placed.set(n.id, { ...n, x: xs[ci], y, w: SIZE.nodeW, h, col: ci, lane: inLane });
      bottom = y + h;
    }
  };
  const order = [...Array(cols.count).keys()].slice(1).concat([0]);
  for (const ci of order) place(ci, false, SIZE.pad);
  let mainBottom = SIZE.pad;
  for (const p of placed.values()) mainBottom = Math.max(mainBottom, p.y + p.h);
  let lane = null;
  if (cols.lane.size) {
    const top = mainBottom + SIZE.laneGap;
    lane = { y: mainBottom + SIZE.laneGap / 2 };
    for (const ci of order) place(ci, true, top);
  }
  let height = SIZE.pad;
  for (const p of placed.values()) height = Math.max(height, p.y + p.h);
  const edges = routeEdges(graph, placed, columns, xs, ws, cols.back);
  return { nodes: placed, edges, width, height: height + SIZE.pad, lane, columns: cols.count };
}

// spreadY is the height of the i-th of n edge ends on a node's side.
function spreadY(p, i, n) {
  return Math.round(p.y + (p.h * (i + 1)) / (n + 1));
}

// routeEdges gives each edge its path: it leaves its source's right side
// and enters its target's left side, the ends spread over the side in the
// order of the other ends' heights; an edge that passes a column runs
// through the gap there nearest its line; one that closes a cycle loops
// back.
export function routeEdges(graph, placed, columns, xs, ws, back = new Set()) {
  const outs = new Map();
  const ins = new Map();
  for (const e of graph.edges) {
    if (!outs.has(e.from)) outs.set(e.from, []);
    if (!ins.has(e.to)) ins.set(e.to, []);
    outs.get(e.from).push(e);
    ins.get(e.to).push(e);
  }
  const mid = (id) => { const p = placed.get(id); return p.y + p.h / 2; };
  const sy = new Map();
  const ty = new Map();
  for (const [id, l] of outs) {
    l.sort((a, b) => mid(a.to) - mid(b.to) || byId(a.id, b.id));
    l.forEach((e, i) => sy.set(e.id, spreadY(placed.get(id), i, l.length)));
  }
  for (const [id, l] of ins) {
    l.sort((a, b) => mid(a.from) - mid(b.from) || byId(a.id, b.id));
    l.forEach((e, i) => ty.set(e.id, spreadY(placed.get(id), i, l.length)));
  }
  return graph.edges.map((e) => {
    const s = placed.get(e.from);
    const t = placed.get(e.to);
    const sp = { x: s.x + s.w, y: sy.get(e.id) };
    const tp = { x: t.x, y: ty.get(e.id) };
    const pts = [sp];
    const isBack = back.has(e.id) || t.col <= s.col;
    if (!isBack) {
      for (let ci = s.col + 1; ci < t.col; ci++) {
        if (!ws[ci]) continue;
        const col = columns[ci].map((n) => placed.get(n.id));
        const f = (xs[ci] + ws[ci] / 2 - sp.x) / (tp.x - sp.x || 1);
        const y = gapY(col, sp.y + (tp.y - sp.y) * f);
        pts.push({ x: xs[ci], y }, { x: xs[ci] + ws[ci], y });
      }
    }
    pts.push(tp);
    return { ...e, d: pathOf(pts, isBack), sx: sp.x, sy: sp.y, tx: tp.x, ty: tp.y, back: isBack };
  });
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

// hitTest is the node at x, y ({id, port: -1}), or null.
export function hitTest(layout, x, y) {
  for (const p of layout.nodes.values()) {
    if (x < p.x || x > p.x + p.w || y < p.y || y > p.y + p.h) continue;
    return { id: p.id, port: -1 };
  }
  return null;
}

// portValue is how a reference or route names a service's port i (the
// first for -1): by its name, else its number.
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
  if (t.key === 'addons') return { kind: 'ref', target: { key: t.key, name: t.name } };
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
    const a = dropAction(layout, from, { id: p.id, port: -1 }, spec);
    if (a) out.push({ id: p.id, port: -1, action: a });
  }
  return out.sort((a, b) => byId(a.id, b.id));
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

// focusOf are the nodes and edges next to unit (an object, a route or a
// secret): the object, its neighbours and the edges between them; a
// route's edge and its ends; the nodes that use a secret.
export function focusOf(graph, unit) {
  const nodes = new Set();
  const edges = new Set();
  if (!unit) return null;
  if (unit.startsWith('routes/')) {
    for (const e of graph.edges) {
      if (e.units.includes(unit)) {
        edges.add(e.id);
        nodes.add(e.from);
        nodes.add(e.to);
      }
    }
    return nodes.size ? { nodes, edges } : null;
  }
  if (unit.startsWith('secrets/')) {
    const s = graph.secrets.find((x) => unitKey('secrets', x.name) === unit);
    if (!s) return null;
    for (const u of s.users) nodes.add(u.id);
    return { nodes, edges };
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
