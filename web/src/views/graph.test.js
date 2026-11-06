// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { response } from '../testing/ferun.js';
import { removeObject, setObject } from './draft.js';
import {
  ENTRY, SIZE, addBinding, addRoute, canDrag, columnsOf, diffMarks, dropAction, dropTargets, firstRoutePath, focusOf,
  gapY, graphOf, hitTest, layoutGraph, nodeHeight, pathOf, portIndex, portY, removedOf,
} from './graph.js';

// The graph's arithmetic on ferund's fixtures: deployment 7 of
// expensa-stage has three services (expense-service calls
// approval-service), a job, postgres and nats, a secret and three routes.

const STORED = response('GET_environments_env_deployments_n.200').spec;
const STATUS = response('GET_environments_env.200');
const ENTRY_OF = { public: STATUS.public, hosts: STATUS.hosts, env: STATUS.name };

const ids = (g) => g.nodes.map((n) => n.id);
const overlap = (a, b) => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;

describe('graphOf', () => {
  it('makes the objects nodes, the references and routes edges', () => {
    const g = graphOf(STORED, { entry: ENTRY_OF });
    expect(ids(g)).toEqual([ENTRY, 'services/approval-service', 'services/expense-service', 'services/web', 'jobs/migrate',
      'addons/nats', 'addons/postgres', 'secrets/internal-token']);
    expect(g.nodes[0]).toMatchObject({ public: true, hosts: ['expensa-stage-fecode.apps.fecode.dev'] });
    const routes = g.edges.filter((e) => e.via === 'route');
    expect(routes.map((e) => [e.from, e.to, e.port, e.label, e.unit])).toEqual([
      [ENTRY, 'services/approval-service', 0, '/api/approval', 'routes/api-approval'],
      [ENTRY, 'services/expense-service', 0, '/api/expense', 'routes/api-expense'],
      [ENTRY, 'services/web', 0, '/', 'routes/root'],
    ]);
    const db = g.edges.find((e) => e.id === 'services/expense-service|env.EXPENSA_DATABASE_URL');
    expect(db).toMatchObject({ to: 'addons/postgres', via: 'env', label: 'EXPENSA_DATABASE_URL · url', port: -1 });
    expect(g.edges.find((e) => e.field === 'env.EXPENSA_APPROVAL_URL')).toMatchObject({ to: 'services/approval-service', port: 0 });
    expect(g.edges).toHaveLength(10);
  });

  it('draws the route door when the environment is not public, and what is missing or removed', () => {
    let spec = setObject(STORED, 'services', 'web', { ...STORED.services.web, env: { C: { addon: 'cache', key: 'url' } }, bindings: ['stripe'] });
    spec = setObject(spec, 'services', 'dev', { runtime: 'attached', sandbox: 'sbx-1' });
    const g = graphOf(spec, { entry: { public: false, hosts: ['x'] }, removed: [{ key: 'addons', name: 'old' }] });
    expect(g.nodes[0]).toMatchObject({ name: 'route door', public: false, hosts: [] });
    expect(g.nodes.find((n) => n.id === 'addons/cache')).toMatchObject({ missing: true });
    expect(g.nodes.find((n) => n.id === 'bindings/stripe')).toMatchObject({ missing: true });
    expect(g.nodes.find((n) => n.id === 'addons/old')).toMatchObject({ removed: true });
    expect(g.nodes.find((n) => n.id === 'services/dev')).toMatchObject({ attached: true, sandbox: 'sbx-1' });
    expect(g.edges.find((e) => e.via === 'sandbox')).toMatchObject({ from: 'services/dev', to: 'sandbox/sbx-1', dashed: true });
  });

  it('finds a port by name or number, the first when none is named', () => {
    const ports = [{ name: 'http', port: 8080 }, { name: 'admin', port: 9000 }];
    expect([portIndex(ports, 'admin'), portIndex(ports, 9000), portIndex(ports, '9000'), portIndex(ports, undefined), portIndex(ports, 'x')])
      .toEqual([1, 1, 1, 0, -1]);
    expect(portIndex([], 'http')).toBe(-1);
  });

  it('marks the plan changes, and draws what it removes', () => {
    const diff = [{ kind: 'service', name: 'web', change: 'update' }, { kind: 'addon', name: 'nats', change: 'remove' },
      { kind: 'route', name: 'root', change: 'add' }, { kind: 'addon', name: 'postgres', change: 'remove' }];
    expect([...diffMarks(diff)]).toEqual([['services/web', 'update'], ['addons/nats', 'remove'], ['routes/root', 'add'],
      ['addons/postgres', 'remove']]);
    // postgres is still in the draft: no ghost for it.
    expect(removedOf(diff, removeObject(STORED, 'addons', 'nats'))).toEqual([{ key: 'addons', name: 'nats' }]);
  });
});

describe('layout', () => {
  it('puts the kinds in columns, a called service right of its caller', () => {
    const g = graphOf(STORED, { entry: ENTRY_OF });
    const c = columnsOf(g);
    expect(Object.fromEntries(c.col)).toEqual({
      [ENTRY]: 0, 'services/expense-service': 1, 'services/web': 1, 'jobs/migrate': 1, 'services/approval-service': 2,
      'addons/nats': 3, 'addons/postgres': 3, 'secrets/internal-token': 4,
    });
    expect(c.count).toBe(5);
    // A cycle is cut: both stay placed.
    const cyc = setObject(STORED, 'services', 'approval-service',
      { ...STORED.services['approval-service'], env: { X: { service: 'expense-service', port: 'http', key: 'url' } } });
    const cc = columnsOf(graphOf(cyc));
    expect([cc.col.get('services/approval-service'), cc.col.get('services/expense-service')].sort()).toEqual([1, 2]);
  });

  it('places nodes without overlap, the same for any order of the spec', () => {
    const l = layoutGraph(graphOf(STORED, { entry: ENTRY_OF }));
    const ps = [...l.nodes.values()];
    for (const a of ps) for (const b of ps) if (a !== b) expect(overlap(a, b)).toBe(false);
    // The order in a column: services before the job; the entry sits by
    // the ports its routes reach.
    const col1 = ps.filter((p) => p.col === 1).sort((a, b) => a.y - b.y).map((p) => p.id);
    expect(col1[col1.length - 1]).toBe('jobs/migrate');
    const reversed = Object.fromEntries(Object.entries(STORED).map(([k, v]) => [k, Object.fromEntries(Object.entries(v).reverse())]));
    const again = layoutGraph(graphOf(reversed, { entry: ENTRY_OF }));
    expect([...again.nodes.values()].map((p) => [p.id, p.x, p.y]).sort())
      .toEqual(ps.map((p) => [p.id, p.x, p.y]).sort());
    expect(again.edges.map((e) => e.d).sort()).toEqual(l.edges.map((e) => e.d).sort());
    // The frame holds the environment's columns, not the entry.
    expect(l.frame.x).toBeGreaterThan(l.nodes.get(ENTRY).x + l.nodes.get(ENTRY).w);
    expect(l.frame.x + l.frame.w).toBeGreaterThanOrEqual(l.nodes.get('secrets/internal-token').x + SIZE.nodeW);
  });

  it('gives empty columns no room', () => {
    const l = layoutGraph(graphOf({ services: { web: { image: 'nginx', ports: [{ name: 'http', port: 80 }] } } }));
    expect(l.width).toBe(SIZE.pad * 2 + SIZE.entryW + SIZE.gapX + SIZE.nodeW);
  });

  it('makes nodes tall enough for their lines and edges', () => {
    expect(nodeHeight({ key: 'addons', ports: [] })).toBe(SIZE.head + SIZE.foot);
    expect(nodeHeight({ key: 'services', ports: [{}, {}] })).toBe(SIZE.head + 2 * SIZE.row + SIZE.foot);
    expect(nodeHeight({ key: 'services', ports: [{}] }, 6)).toBe(SIZE.head / 2 + 5 * SIZE.row + SIZE.row);
  });
});

describe('edges', () => {
  it('end at the port they name and leave a node one line apart', () => {
    const l = layoutGraph(graphOf(STORED, { entry: ENTRY_OF }));
    const web = l.nodes.get('services/web');
    const root = l.edges.find((e) => e.id === 'route/root');
    expect([root.tx, root.ty]).toEqual([web.x, portY(web, 0)]);
    const outs = l.edges.filter((e) => e.from === 'services/expense-service').map((e) => e.sy).sort((a, b) => a - b);
    for (let i = 1; i < outs.length; i++) expect(outs[i] - outs[i - 1]).toBe(SIZE.row);
  });

  it('pass a column through a gap, not through its nodes', () => {
    const l = layoutGraph(graphOf(STORED, { entry: ENTRY_OF }));
    for (const e of l.edges) {
      // Every horizontal run ("L x,y" after a point at the same y) is in
      // a passed column: no node there may hold its y.
      const pts = [...e.d.matchAll(/(-?[\d.]+),(-?[\d.]+)/g)].map((m) => [Number(m[1]), Number(m[2])]);
      for (const m of e.d.matchAll(/L(-?[\d.]+),(-?[\d.]+)/g)) {
        const y = Number(m[2]);
        const x = Number(m[1]);
        for (const p of l.nodes.values()) {
          if (p.x + p.w === x) expect(y > p.y && y < p.y + p.h).toBe(false);
        }
      }
      expect(pts.length).toBeGreaterThan(1);
    }
  });

  it('find the nearest gap', () => {
    const col = [{ y: 0, h: 50 }, { y: 70, h: 50 }];
    expect(gapY(col, 30)).toBe(56);
    expect(gapY(col, 62)).toBe(62);
    expect(gapY(col, 100)).toBe(126);
    expect(gapY([], 10)).toBe(10);
    expect(pathOf([{ x: 0, y: 0 }, { x: 10, y: 0 }, { x: 30, y: 20 }])).toBe('M0,0 L10,0 C20,0 20,20 30,20');
  });
});

describe('drawing', () => {
  const spec = setObject(STORED, 'bindings', 'stripe', { upstream: 'https://api.stripe.com' });
  const l = layoutGraph(graphOf(spec, { entry: ENTRY_OF }));
  const centre = (id) => { const p = l.nodes.get(id); return [p.x + p.w / 2, p.y + 8]; };

  it('hits the node and the port line under the pointer', () => {
    const web = l.nodes.get('services/web');
    expect(hitTest(l, ...centre('addons/postgres'))).toEqual({ id: 'addons/postgres', port: -1 });
    expect(hitTest(l, web.x + 10, portY(web, 0))).toEqual({ id: 'services/web', port: 0 });
    expect(hitTest(l, 1, 1)).toBeNull();
  });

  it('says what a drop does', () => {
    const svc = (id, port = -1) => ({ id, port });
    expect(dropAction(l, 'services/web', svc('addons/postgres'), spec)).toEqual({ kind: 'ref', target: { key: 'addons', name: 'postgres' } });
    expect(dropAction(l, 'services/web', svc('secrets/internal-token'), spec)).toEqual({ kind: 'ref', target: { key: 'secrets', name: 'internal-token' } });
    expect(dropAction(l, 'jobs/migrate', svc('services/approval-service', 0), spec))
      .toEqual({ kind: 'ref', target: { key: 'services', name: 'approval-service' }, port: 'http' });
    expect(dropAction(l, 'services/web', svc('bindings/stripe'), spec)).toEqual({ kind: 'binding', name: 'stripe' });
    expect(dropAction(l, 'services/web', svc('services/web'), spec)).toBeNull();
    expect(dropAction(l, 'services/web', svc('jobs/migrate'), spec)).toBeNull();
    expect(dropAction(l, ENTRY, svc('services/web', 0), spec)).toEqual({ kind: 'route', service: 'web', port: 'http' });
    expect(dropAction(l, ENTRY, svc('addons/postgres'), spec)).toBeNull();
    expect(dropAction(l, 'services/web', null, spec)).toBeNull();
    // Bound already: nothing to add.
    const bound = setObject(spec, 'services', 'web', { ...spec.services.web, bindings: ['stripe'] });
    expect(dropAction(l, 'services/web', svc('bindings/stripe'), bound)).toBeNull();
    expect(dropTargets(l, ENTRY, spec).map((t) => t.id).sort()).toEqual(['services/approval-service', 'services/expense-service', 'services/web']);
    expect(dropTargets(l, 'services/web', spec).map((t) => t.id).sort()).toEqual(['addons/nats', 'addons/postgres',
      'bindings/stripe', 'secrets/internal-token', 'services/approval-service', 'services/expense-service']);
    expect([canDrag(l.nodes.get(ENTRY)), canDrag(l.nodes.get('addons/nats')), canDrag(l.nodes.get('jobs/migrate'))]).toEqual([true, false, true]);
  });

  it('writes a route: / for the first, a path asked for the others', () => {
    expect(firstRoutePath({})).toBe('/');
    expect(firstRoutePath(STORED)).toBe('');
    const r = addRoute(STORED, 'web', 'http', '/admin/');
    expect(r.name).toBe('admin');
    expect(r.spec.routes.admin).toEqual({ path: '/admin/', service: 'web', port: 'http' });
    expect(addRoute(STORED, 'web', 'http', '/').name).toBe('root-2');
    expect(addRoute({}, 'web', '', '/').spec.routes.root).toEqual({ path: '/', service: 'web' });
    expect(addRoute(STORED, 'web', 'http', '/api/v2').name).toBe('api-v2');
  });

  it('writes a binding into the list once', () => {
    const s = addBinding(spec, 'services', 'web', 'stripe');
    expect(s.services.web.bindings).toEqual(['stripe']);
    expect(addBinding(s, 'services', 'web', 'stripe')).toBe(s);
  });

  it('focuses the open object and its neighbours, or a route and its ends', () => {
    const g = graphOf(STORED, { entry: ENTRY_OF });
    const f = focusOf(g, 'addons/postgres');
    expect([...f.nodes].sort()).toEqual(['addons/postgres', 'jobs/migrate', 'services/approval-service', 'services/expense-service']);
    expect(f.edges.size).toBe(3);
    const r = focusOf(g, 'routes/root');
    expect([...r.nodes].sort()).toEqual([ENTRY, 'services/web']);
    expect(focusOf(g, '')).toBeNull();
    expect(focusOf(g, 'addons/none')).toBeNull();
  });
});
