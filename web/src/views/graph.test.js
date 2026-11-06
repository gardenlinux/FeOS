// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { response } from '../testing/ferun.js';
import { setObject } from './draft.js';
import { changesSince } from './rebase.js';
import {
  ENTRY, SIZE, addBinding, addRoute, canDrag, changeList, columnsOf, diffMarks, dropAction, dropTargets, firstRoutePath, focusOf,
  gapY, graphOf, hitTest, layoutGraph, nodeHeight, orderOf, pathOf, portIndex, removedOf,
} from './graph.js';

// The graph's arithmetic on ferund's fixtures (deployment 7 of
// expensa-stage: three services, expense-service calls
// approval-service, a job, postgres and nats, a secret, three routes)
// and on an app like kino: eight services that call each other, Postgres
// and Valkey, a binding, secrets and a registry login on every image.

const STORED = response('GET_environments_env_deployments_n.200').spec;
const STATUS = response('GET_environments_env.200');
const ENTRY_OF = { public: STATUS.public, hosts: STATUS.hosts, env: STATUS.name };

const login = { imageCredentials: { username: 'ci', password: { secret: 'registry-ghcr.io' } } };
const svc = (name, port = 'http') => ({ service: name, port, key: 'url' });
const http = [{ name: 'http', port: 8080 }];
const KINO = {
  services: {
    web: { image: 'ghcr.io/kino/web:2', ...login, ports: http },
    api: { image: 'ghcr.io/kino/api:1', ...login, ports: http,
      env: { ORDERS_URL: svc('orders'), CATALOG_URL: svc('catalog'), SEATS_URL: svc('seats'), VALKEY_URL: { addon: 'cache', key: 'url' },
        SESSION_SECRET: { secret: 'session-key' } } },
    orders: { image: 'ghcr.io/kino/orders:1', ...login, ports: http,
      env: { DATABASE_URL: { addon: 'db', key: 'url' }, PAYMENT_URL: svc('payment'), SEATS_URL: svc('seats'), SEATS_ADMIN_URL: svc('seats', 'admin'),
        NOTIFY_URL: svc('notify') } },
    catalog: { image: 'ghcr.io/kino/catalog:3', ...login, ports: http, env: { DATABASE_URL: { addon: 'db', key: 'url' } } },
    seats: { image: 'ghcr.io/kino/seats:2', ...login, ports: [...http, { name: 'admin', port: 9090 }],
      env: { DATABASE_URL: { addon: 'db', key: 'url' }, VALKEY_URL: { addon: 'cache', key: 'url' } } },
    payment: { image: 'ghcr.io/kino/payment:1', ...login, ports: http, bindings: ['stripe'],
      env: { DATABASE_URL: { addon: 'db', key: 'url' }, WEBHOOK_SECRET: { secret: 'stripe-webhook' } } },
    notify: { image: 'ghcr.io/kino/notify:1', ...login, ports: http, env: { SMTP_PASSWORD: { secret: 'smtp-password' } } },
    loadgen: { image: 'ghcr.io/kino/loadgen:1', ...login, ports: http, env: { TARGET_URL: svc('web'), API_URL: svc('api') } },
  },
  jobs: { migrate: { image: 'ghcr.io/kino/migrate:1', ...login, env: { DATABASE_URL: { addon: 'db', key: 'url' } } } },
  addons: { db: { type: 'postgres', version: '17' }, cache: { type: 'valkey', version: '8' } },
  routes: { root: { path: '/', service: 'web', port: 'http' }, api: { path: '/api', service: 'api', port: 'http' } },
  bindings: { stripe: { upstream: 'https://api.stripe.com', credential: { kind: 'headers', headers: { Authorization: { secret: 'stripe-key' } } } } },
};
const KINO_ENTRY = { public: true, hosts: ['kino.example.com'], env: 'prod' };

const ids = (g) => g.nodes.map((n) => n.id);
const edge = (g, from, to) => g.edges.find((e) => e.from === from && e.to === to);
const overlap = (a, b) => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;

describe('graphOf', () => {
  it('makes the objects nodes, one edge per pair of objects, the routes edges from the entry', () => {
    const g = graphOf(STORED, { entry: ENTRY_OF });
    expect(ids(g)).toEqual([ENTRY, 'services/approval-service', 'services/expense-service', 'services/web', 'jobs/migrate',
      'addons/nats', 'addons/postgres']);
    expect(g.nodes[0]).toMatchObject({ public: true, hosts: ['expensa-stage-fecode.apps.fecode.dev'] });
    const routes = g.edges.filter((e) => e.via === 'route');
    expect(routes.map((e) => [e.from, e.to, e.label, e.unit])).toEqual([
      [ENTRY, 'services/approval-service', '/api/approval', 'routes/api-approval'],
      [ENTRY, 'services/expense-service', '/api/expense', 'routes/api-expense'],
      [ENTRY, 'services/web', '/', 'routes/root'],
    ]);
    expect(edge(g, 'services/expense-service', 'addons/postgres')).toMatchObject({ via: 'data', label: 'EXPENSA_DATABASE_URL', more: 0 });
    expect(edge(g, 'services/expense-service', 'services/approval-service')).toMatchObject({ via: 'call' });
    expect(edge(g, 'jobs/migrate', 'addons/postgres')).toMatchObject({ dashed: true });
    expect(g.edges).toHaveLength(8);
  });

  it('folds the references between two objects into one edge, its title every line', () => {
    const g = graphOf(KINO, { entry: KINO_ENTRY });
    const seats = edge(g, 'services/orders', 'services/seats');
    expect(seats).toMatchObject({ label: 'SEATS_URL', more: 1, fields: ['env.SEATS_URL', 'env.SEATS_ADMIN_URL'], unit: 'services/orders' });
    expect(seats.title).toBe('SEATS_URL → seats:http · url\nSEATS_ADMIN_URL → seats:admin · url');
    // One edge per pair: api calls three services and uses valkey.
    expect(g.edges.filter((e) => e.from === 'services/api').map((e) => e.to).sort())
      .toEqual(['addons/cache', 'services/catalog', 'services/orders', 'services/seats']);
    expect(edge(g, 'services/payment', 'bindings/stripe')).toMatchObject({ via: 'binding', label: 'binding' });
  });

  it('carries secrets and registry logins as markers, not edges, and lists the secrets with their users', () => {
    const g = graphOf(KINO, { entry: KINO_ENTRY });
    expect(g.edges.some((e) => e.to.startsWith('secrets/'))).toBe(false);
    expect(g.nodes.some((n) => n.key === 'secrets')).toBe(false);
    const api = g.nodes.find((n) => n.id === 'services/api');
    expect([api.secrets, api.logins]).toEqual([['session-key'], ['registry-ghcr.io']]);
    expect(g.nodes.find((n) => n.id === 'bindings/stripe').secrets).toEqual(['stripe-key']);
    expect(g.secrets.map((s) => s.name)).toEqual(['registry-ghcr.io', 'session-key', 'smtp-password', 'stripe-key', 'stripe-webhook']);
    const reg = g.secrets[0];
    expect(reg.users).toHaveLength(9);
    expect(reg.users.every((u) => u.login)).toBe(true);
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

  it('marks the changes, from a plan diff or the draft against its base, and draws what it removes', () => {
    const diff = [{ kind: 'service', name: 'web', change: 'update' }, { kind: 'addon', name: 'nats', change: 'remove' },
      { kind: 'route', name: 'root', change: 'add' }, { kind: 'addon', name: 'postgres', change: 'remove' }];
    expect([...diffMarks(diff)]).toEqual([['services/web', 'update'], ['addons/nats', 'remove'], ['routes/root', 'add'],
      ['addons/postgres', 'remove']]);
    let next = setObject(STORED, 'services', 'web', { ...STORED.services.web, replicas: 9 });
    next = { ...next, addons: { postgres: STORED.addons.postgres } };
    next = setObject(next, 'secrets', 'new-key', { version: 1 });
    const changes = changeList(changesSince(STORED, next));
    expect(changes.map((c) => [c.kind, c.name, c.change])).toEqual([
      ['addon', 'nats', 'remove'], ['secret', 'new-key', 'add'], ['service', 'web', 'update']]);
    expect([...diffMarks(changes)]).toEqual([['addons/nats', 'remove'], ['secrets/new-key', 'add'], ['services/web', 'update']]);
    // postgres is still in the draft, a secret is no node: one ghost.
    expect(removedOf(changes, next)).toEqual([{ key: 'addons', name: 'nats' }]);
    expect(removedOf(diff, next)).toEqual([{ key: 'addons', name: 'nats' }]);
  });
});

describe('layout', () => {
  it('puts each node in the column of its depth from the entry, the addons last', () => {
    const c = columnsOf(graphOf(STORED, { entry: ENTRY_OF }));
    expect(Object.fromEntries(c.col)).toEqual({
      [ENTRY]: 0, 'services/expense-service': 1, 'services/web': 1, 'services/approval-service': 2,
      'jobs/migrate': 2, 'addons/nats': 3, 'addons/postgres': 3,
    });
    // The job no route reaches is in the lane, left of postgres.
    expect([...c.lane]).toEqual(['jobs/migrate']);
    expect(c.count).toBe(4);
  });

  it('lays kino out by dependency: what routes reach, what those call, the addons; the rest in the lane', () => {
    const g = graphOf(KINO, { entry: KINO_ENTRY });
    const c = columnsOf(g);
    const at = (id) => c.col.get(`${id}`);
    expect([at(ENTRY), at('services/web'), at('services/api'), at('services/orders'), at('services/catalog'), at('services/seats'),
      at('services/payment'), at('services/notify'), at('addons/db'), at('addons/cache'), at('bindings/stripe')])
      .toEqual([0, 1, 1, 2, 2, 3, 3, 3, 4, 4, 4]);
    // loadgen calls web and api: the lane, one column left of them; the
    // job migrate one left of the addons.
    expect([at('services/loadgen'), at('jobs/migrate')]).toEqual([0, 3]);
    expect([...c.lane].sort()).toEqual(['jobs/migrate', 'services/loadgen']);
    // Every edge runs left to right.
    for (const e of g.edges) expect(c.col.get(e.to)).toBeGreaterThan(c.col.get(e.from));
    expect(c.back.size).toBe(0);
  });

  it('cuts a cycle where the walk from the entry closes it', () => {
    const cyc = setObject(KINO, 'services', 'seats', { ...KINO.services.seats, env: { ORDERS_URL: svc('orders') } });
    const g = graphOf(cyc, { entry: KINO_ENTRY });
    const c = columnsOf(g);
    expect([...c.back]).toEqual(['services/seats>services/orders']);
    expect(c.col.get('services/seats')).toBeGreaterThan(c.col.get('services/orders'));
    const l = layoutGraph(g);
    expect(l.edges.find((e) => e.id === 'services/seats>services/orders').back).toBe(true);
  });

  it('orders a column by its neighbours, the lane below, the same for any order of the spec', () => {
    const g = graphOf(KINO, { entry: KINO_ENTRY });
    const cols = columnsOf(g);
    const order = orderOf(g, cols).map((c) => c.map((n) => n.id));
    expect(order[0]).toEqual([ENTRY, 'services/loadgen']);
    expect(order[4].slice(-1)).toEqual(['bindings/stripe']);
    const reversed = Object.fromEntries(Object.entries(KINO).map(([k, v]) => [k, Object.fromEntries(Object.entries(v).reverse())]));
    const l = layoutGraph(g);
    const again = layoutGraph(graphOf(reversed, { entry: KINO_ENTRY }));
    expect([...again.nodes.values()].map((p) => [p.id, p.x, p.y]).sort()).toEqual([...l.nodes.values()].map((p) => [p.id, p.x, p.y]).sort());
    expect(again.edges.map((e) => e.d).sort()).toEqual(l.edges.map((e) => e.d).sort());
  });

  it('places nodes without overlap, the lane below the rest, every node and edge inside the drawing', () => {
    for (const [spec, entry] of [[STORED, ENTRY_OF], [KINO, KINO_ENTRY]]) {
      const l = layoutGraph(graphOf(spec, { entry }));
      const ps = [...l.nodes.values()];
      for (const a of ps) for (const b of ps) if (a !== b) expect(overlap(a, b)).toBe(false);
      for (const p of ps) {
        expect(p.x).toBeGreaterThanOrEqual(0);
        expect(p.x + p.w).toBeLessThanOrEqual(l.width);
        expect(p.y + p.h).toBeLessThanOrEqual(l.height);
        if (p.lane) expect(p.y).toBeGreaterThan(l.lane.y);
        else expect(p.y + p.h).toBeLessThan(l.lane.y);
      }
      for (const e of l.edges) {
        for (const m of e.d.matchAll(/(-?[\d.]+),(-?[\d.]+)/g)) {
          expect(Number(m[1])).toBeLessThanOrEqual(l.width);
          expect(Number(m[2])).toBeLessThanOrEqual(l.height);
        }
      }
    }
  });

  it('fits five columns into the width of a wide screen', () => {
    const l = layoutGraph(graphOf(KINO, { entry: KINO_ENTRY }));
    expect(l.width).toBe(SIZE.pad * 2 + 5 * SIZE.nodeW + 4 * SIZE.gapX);
    expect(l.width).toBeLessThanOrEqual(1140);
  });

  it('gives empty columns no room', () => {
    const l = layoutGraph(graphOf({ services: { web: { image: 'nginx', ports: [{ name: 'http', port: 80 }] } }, routes: { root: { path: '/', service: 'web' } } }));
    expect(l.width).toBe(SIZE.pad * 2 + 2 * SIZE.nodeW + SIZE.gapX);
    expect(l.lane).toBeNull();
  });

  it('makes nodes tall enough for their lines and edges', () => {
    expect(nodeHeight({ key: 'addons', ports: [] })).toBe(SIZE.small);
    expect(nodeHeight({ key: 'services', ports: [{}, {}] })).toBe(SIZE.object);
    expect(nodeHeight({ key: 'entry', hosts: ['a', 'b'] })).toBe(SIZE.entryHead + 2 * SIZE.host + 8);
    expect(nodeHeight({ key: 'addons', ports: [] }, 7)).toBe(8 * SIZE.spread);
  });
});

describe('edges', () => {
  it('leave the right side and enter the left, their ends spread over the side', () => {
    const l = layoutGraph(graphOf(KINO, { entry: KINO_ENTRY }));
    const db = l.nodes.get('addons/db');
    const ins = l.edges.filter((e) => e.to === 'addons/db');
    expect(ins.length).toBe(5);
    for (const e of ins) {
      expect(e.tx).toBe(db.x);
      expect(e.ty).toBeGreaterThan(db.y);
      expect(e.ty).toBeLessThan(db.y + db.h);
    }
    expect(new Set(ins.map((e) => e.ty)).size).toBe(5);
    const api = l.nodes.get('services/api');
    for (const e of l.edges.filter((x) => x.from === 'services/api')) expect(e.sx).toBe(api.x + api.w);
  });

  it('pass a column through a gap, not through its nodes', () => {
    const l = layoutGraph(graphOf(KINO, { entry: KINO_ENTRY }));
    for (const e of l.edges) {
      for (const m of e.d.matchAll(/L(-?[\d.]+),(-?[\d.]+)/g)) {
        const y = Number(m[2]);
        const x = Number(m[1]);
        for (const p of l.nodes.values()) {
          if (p.x + p.w === x) expect(y > p.y && y < p.y + p.h).toBe(false);
        }
      }
    }
  });

  it('find the nearest gap', () => {
    const col = [{ y: 0, h: 50 }, { y: 70, h: 50 }];
    expect(gapY(col, 30)).toBe(50 + SIZE.margin);
    expect(gapY(col, 60)).toBe(60);
    expect(gapY(col, 100)).toBe(120 + SIZE.margin);
    expect(gapY([], 10)).toBe(10);
    expect(pathOf([{ x: 0, y: 0 }, { x: 10, y: 0 }, { x: 30, y: 20 }])).toBe('M0,0 L10,0 C20,0 20,20 30,20');
  });
});

describe('drawing', () => {
  const spec = setObject(STORED, 'bindings', 'stripe', { upstream: 'https://api.stripe.com' });
  const l = layoutGraph(graphOf(spec, { entry: ENTRY_OF }));
  const centre = (id) => { const p = l.nodes.get(id); return [p.x + p.w / 2, p.y + 8]; };

  it('hits the node under the pointer', () => {
    expect(hitTest(l, ...centre('addons/postgres'))).toEqual({ id: 'addons/postgres', port: -1 });
    expect(hitTest(l, 1, 1)).toBeNull();
  });

  it('says what a drop does', () => {
    const at = (id) => ({ id, port: -1 });
    expect(dropAction(l, 'services/web', at('addons/postgres'), spec)).toEqual({ kind: 'ref', target: { key: 'addons', name: 'postgres' } });
    expect(dropAction(l, 'jobs/migrate', at('services/approval-service'), spec))
      .toEqual({ kind: 'ref', target: { key: 'services', name: 'approval-service' }, port: 'http' });
    expect(dropAction(l, 'services/web', at('bindings/stripe'), spec)).toEqual({ kind: 'binding', name: 'stripe' });
    expect(dropAction(l, 'services/web', at('services/web'), spec)).toBeNull();
    expect(dropAction(l, 'services/web', at('jobs/migrate'), spec)).toBeNull();
    expect(dropAction(l, ENTRY, at('services/web'), spec)).toEqual({ kind: 'route', service: 'web', port: 'http' });
    expect(dropAction(l, ENTRY, at('addons/postgres'), spec)).toBeNull();
    expect(dropAction(l, 'services/web', null, spec)).toBeNull();
    // Bound already: nothing to add.
    const bound = setObject(spec, 'services', 'web', { ...spec.services.web, bindings: ['stripe'] });
    expect(dropAction(l, 'services/web', at('bindings/stripe'), bound)).toBeNull();
    expect(dropTargets(l, ENTRY, spec).map((t) => t.id)).toEqual(['services/approval-service', 'services/expense-service', 'services/web']);
    expect(dropTargets(l, 'services/web', spec).map((t) => t.id)).toEqual(['addons/nats', 'addons/postgres',
      'bindings/stripe', 'services/approval-service', 'services/expense-service']);
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

  it('focuses an object and its neighbours, a route and its ends, a secret and its users', () => {
    const g = graphOf(STORED, { entry: ENTRY_OF });
    const f = focusOf(g, 'addons/postgres');
    expect([...f.nodes].sort()).toEqual(['addons/postgres', 'jobs/migrate', 'services/approval-service', 'services/expense-service']);
    expect(f.edges.size).toBe(3);
    const r = focusOf(g, 'routes/root');
    expect([...r.nodes].sort()).toEqual([ENTRY, 'services/web']);
    const s = focusOf(g, 'secrets/internal-token');
    expect([...s.nodes].sort()).toEqual(['services/approval-service', 'services/expense-service']);
    expect(s.edges.size).toBe(0);
    expect(focusOf(g, '')).toBeNull();
    expect(focusOf(g, 'addons/none')).toBeNull();
  });
});
