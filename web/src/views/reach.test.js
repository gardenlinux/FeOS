// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  edgeLine, edgesByCaller, egressMode, egressOfAnswer, egressProblems, egressRows, egressWords, envRef, fromProblems, fromWords,
  inEnv, inUse, isEgressDomain, isPrefix, missingReaches, portWords, reachChoices, reachModel, reachesMode, reachesOf,
  reachesValue, reachesWords, sameReaches, toggleReach, usesTargets, useWords, withEgressList, withEgressMode,
} from './reach.js';
import { orderSpec, parseSpecText, renameObject, specText } from './draft.js';
import { referencesTo, refLine, removeWithRefs } from './refs.js';
import { ApiError } from '../api.js';

// Reachability from declarations (ADR 0034, revised): the example of the
// ADR grown by a job, a uses, a public route with clients and an egress,
// and the reachability ferund answers for it (contract §2).

const SPEC = {
  services: {
    api: {
      image: 'api:1', ports: [{ name: 'http', port: 8080 }],
      env: { CATALOG_URL: { service: 'catalog', port: 'http', key: 'url' }, DATABASE_URL: { addon: 'db', role: 'api', key: 'url' } },
      egress: { domains: ['api.stripe.com'], blocks: [{ cidr: '203.0.113.0/24', except: ['203.0.113.8/29'] }] },
    },
    catalog: { image: 'catalog:1', ports: [{ name: 'http', port: 8080 }, { port: 9090 }], egress: 'none' },
    preview: { runtime: 'attached', sandbox: 'h3k9v2m7qpx', ports: [{ name: 'http', port: 3000 }] },
  },
  jobs: { migrate: { image: 'api:1', uses: [{ addon: 'db' }] } },
  addons: { db: { type: 'postgres' } },
  routes: {
    api: { path: '/api', service: 'api', port: 'http', from: ['203.0.113.0/24', '2001:db8::/32'] },
    root: { path: '/', service: 'catalog', port: 'http' },
  },
};

const REACH = {
  edges: [
    { from: 'ingress', to: 'service/api', port: 'http', number: 8080, via: 'route api' },
    { from: 'ingress', to: 'service/catalog', port: 'http', number: 8080, via: 'route root' },
    { from: 'job/migrate', to: 'addon/db', port: 'postgres', number: 5432, via: 'uses' },
    { from: 'service/api', to: 'addon/db', port: 'postgres', number: 5432, via: 'env DATABASE_URL' },
    { from: 'service/api', to: 'service/catalog', port: 'http', number: 8080, via: 'env CATALOG_URL' },
  ],
  egress: [
    { object: 'job/migrate', mode: 'internet', domains: [], blocks: [] },
    { object: 'service/api', mode: 'restricted', domains: ['api.stripe.com'], blocks: [{ cidr: '203.0.113.0/24', except: ['203.0.113.8/29'] }] },
    { object: 'service/catalog', mode: 'none', domains: [], blocks: [] },
  ],
  routes: [
    { route: 'api', service: 'api', port: 'http', from: ['203.0.113.0/24', '2001:db8::/32'] },
    { route: 'root', service: 'catalog', port: 'http', from: [] },
  ],
};

// row says a picture's row as "type:name[ (sub)] what · via".
const row = (r) => `${r.peer.type}:${r.peer.name}${r.peer.sub ? ` (${r.peer.sub})` : ''} ${r.what} · ${r.via.join(', ')}`;
const of = (m, ref) => m.objects.find((o) => o.ref === ref);

describe('the picture of an environment', () => {
  const m = reachModel(SPEC, REACH);

  it('has every object of the spec, services first, by name', () => {
    expect(m.objects.map((o) => o.ref)).toEqual(['service/api', 'service/catalog', 'service/preview', 'job/migrate', 'addon/db']);
    expect(of(m, 'service/preview').attached).toBe('h3k9v2m7qpx');
  });

  it('groups the edges per object: who reaches it with the port and the reference, what it reaches', () => {
    const api = of(m, 'service/api');
    expect(api.ins.map(row)).toEqual(['ingress:Ingress (203.0.113.0/24, 2001:db8::/32) http 8080 · route api']);
    expect(api.outs.map(row)).toEqual([
      'service:catalog http 8080 · env CATALOG_URL', 'addon:db postgres 5432 · env DATABASE_URL',
      'domain:api.stripe.com by name · egress', 'cidr:203.0.113.0/24 (except 203.0.113.8/29) all traffic · egress']);
    const db = of(m, 'addon/db');
    expect(db.ins.map(row)).toEqual(['service:api postgres 5432 · env DATABASE_URL', 'job:migrate postgres 5432 · uses']);
    expect(db.outs).toEqual([]);
    expect(db.egress).toBe('none');
  });

  it('shows a route without from as every client, and the egress as answered', () => {
    const cat = of(m, 'service/catalog');
    expect(cat.ins.map(row)).toEqual(['ingress:Ingress (every client) http 8080 · route root', 'service:api http 8080 · env CATALOG_URL']);
    expect(cat.egress).toBe('none');
    expect(cat.outs).toEqual([]);
    expect(of(m, 'job/migrate').outs.map(row)).toEqual(['addon:db postgres 5432 · uses', 'internet:Internet all traffic · egress']);
  });

  it('merges a caller\'s edges to one target into a row, its ports and references listed', () => {
    const reach = { edges: [
      { from: 'service/api', to: 'service/catalog', port: 'http', number: 8080, via: 'env CATALOG_URL' },
      { from: 'service/api', to: 'service/catalog', port: '9090', number: 9090, via: 'uses' },
    ] };
    expect(of(reachModel(SPEC, reach), 'service/catalog').ins.map(row)).toEqual(['service:api http 8080, 9090 · env CATALOG_URL, uses']);
  });

  it('adds the sandboxes and jobs by what they reach, marking what the deployment lacks', () => {
    const pods = [
      { kind: 'sandbox', id: 'h3k9v2m7qpx', reaches: 'all' },
      { kind: 'jobrun', id: 'k2m7qpxh3v9', reaches: [{ service: 'catalog', port: 'http' }, { addon: 'db' }, { service: 'gone', port: 'http' }], egress: 'none' },
    ];
    const p = reachModel(SPEC, REACH, pods);
    expect(of(p, 'addon/db').ins.map(row)).toContain('sandbox:h3k9v2m7qpx every port · reaches all');
    expect(of(p, 'addon/db').ins.map(row)).toContain('jobrun:k2m7qpxh3v9 postgres 5432 · reaches');
    expect(of(p, 'service/catalog').ins.map(row)).toContain('jobrun:k2m7qpxh3v9 http 8080 · reaches');
    const [sbx, job] = p.pods;
    expect(sbx.outs.map(row)).toEqual(['env:every pod every port · reaches all', 'internet:Internet all traffic · egress']);
    expect(sbx.attachedRefs).toEqual(['service/preview']);
    expect(job.outs.map(row)).toEqual(['service:catalog http 8080 · reaches', 'service:gone http · reaches', 'addon:db postgres 5432 · reaches']);
    expect(job.outs[1].peer.missing).toBe(true);
    expect(job.egress).toBe('none');
  });

  it('draws the spec alone when ferund answers no reachability', () => {
    const m0 = reachModel(SPEC, null);
    expect(of(m0, 'service/api').ins).toEqual([]);
    expect(of(m0, 'service/api').outs.map((r) => r.peer.type)).toEqual(['domain', 'cidr']);
    expect(of(m0, 'job/migrate').egress).toBe('internet');
  });

  it('says an edge in a line and groups the plan\'s edges by caller', () => {
    expect(edgeLine(REACH.edges[4])).toBe('api → catalog · http 8080 · env CATALOG_URL');
    expect(edgeLine(REACH.edges[0])).toBe('ingress → api · http 8080 · route api');
    expect(edgesByCaller(REACH).map((c) => [c.from, c.edges.length])).toEqual([['ingress', 2], ['job/migrate', 1], ['service/api', 2]]);
    expect(edgesByCaller({})).toEqual([]);
    expect([portWords('http', 8080), portWords('9090', 9090), portWords('', 5432), portWords('', 0)]).toEqual(['http 8080', '9090', '5432', 'its port']);
  });
});

describe('egress', () => {
  it('says its mode and words: internet when left out, none, only these', () => {
    expect([egressMode(undefined), egressMode('internet'), egressMode('none'), egressMode({}), egressMode(3)])
      .toEqual(['internet', 'internet', 'none', 'only', 'other']);
    expect(egressWords(undefined)).toBe('the internet');
    expect(egressWords('none')).toBe('nothing outside');
    expect(egressWords({})).toBe('nothing outside');
    expect(egressWords(SPEC.services.api.egress)).toBe('only api.stripe.com, 203.0.113.0/24');
    expect(egressWords({ domains: ['a.example', 'b.example', 'c.example', 'd.example'] })).toBe('only a.example, b.example, c.example +1');
  });

  it('switches in the spec\'s words: internet leaves the field out, only keeps its lists', () => {
    expect(withEgressMode('none', 'internet')).toBeUndefined();
    expect(withEgressMode(undefined, 'none')).toBe('none');
    expect(withEgressMode('none', 'only')).toEqual({});
    expect(withEgressMode(SPEC.services.api.egress, 'only')).toBe(SPEC.services.api.egress);
    const e = withEgressList(withEgressList({}, 'domains', ['api.stripe.com']), 'blocks', [{ cidr: '198.51.100.0/24' }]);
    expect(e).toEqual({ domains: ['api.stripe.com'], blocks: [{ cidr: '198.51.100.0/24' }] });
    expect(withEgressList(e, 'domains', [])).toEqual({ blocks: [{ cidr: '198.51.100.0/24' }] });
  });

  it('reads an answer\'s entry as the spec writes it', () => {
    expect(egressOfAnswer(REACH.egress[0])).toBe('internet');
    expect(egressOfAnswer(REACH.egress[1])).toEqual(SPEC.services.api.egress);
    expect(egressOfAnswer(REACH.egress[2])).toBe('none');
    expect(egressOfAnswer({ mode: 'restricted', domains: [], blocks: [] })).toEqual({});
    expect(egressRows('none')).toEqual([]);
  });

  it('checks domains and blocks as the plan would', () => {
    expect(['api.stripe.com', '*.githubusercontent.com', 'localhost'].every(isEgressDomain)).toBe(true);
    expect(['API.stripe.com', 'a.*.b', '*', '-a.example', 'a..b', 'x'.repeat(64)].some(isEgressDomain)).toBe(false);
    expect(['203.0.113.0/24', '0.0.0.0/0', '2001:db8::/32', '::/0', '2001:db8:0:0:0:0:0:1/128'].every(isPrefix)).toBe(true);
    expect(['203.0.113.0', '256.0.0.0/8', '1.2.3.4/33', '2001:db8::/129', '2001::db8::/32', 'nope/8', ''].some(isPrefix)).toBe(false);
    expect(egressProblems('none')).toEqual([]);
    expect(egressProblems({ domains: ['ok.example', 'Bad'], blocks: [{ cidr: '198.51.100.0/24', except: ['x'] }, { cidr: '' }] })).toEqual([
      'not a domain: Bad (lower case; a wildcard only as "*." in front)', 'not an address block: (empty)', 'not an address block: x (except)']);
  });
});

describe('a route\'s clients', () => {
  it('are every client without from, else its blocks', () => {
    expect(fromWords(undefined)).toBe('every client');
    expect(fromWords([])).toBe('every client');
    expect(fromWords(SPEC.routes.api.from)).toBe('203.0.113.0/24, 2001:db8::/32');
    expect(fromProblems(['203.0.113.0/24', 'office'])).toEqual(['not an address block: office']);
  });
});

describe('uses, egress and from in the draft', () => {
  it('orders them where ferund writes them, and the text reads back the same', () => {
    const spec = orderSpec({ services: { w: { egress: 'none', uses: [{ addon: 'db' }], image: 'x', bindings: ['b'] } },
      routes: { r: { from: ['203.0.113.0/24'], service: 'w', path: '/' } } });
    expect(Object.keys(spec.services.w)).toEqual(['image', 'bindings', 'uses', 'egress']);
    expect(Object.keys(spec.routes.r)).toEqual(['path', 'service', 'from']);
    expect(parseSpecText(specText(SPEC)).spec).toEqual(SPEC);
    expect(specText(SPEC)).not.toContain('securityGroups');
  });

  it('lists uses as references, renames and removes them with their target', () => {
    expect(referencesTo(SPEC, 'addons', 'db').map((r) => [r.from.name, r.field, refLine(r)])).toEqual([
      ['api', 'env.DATABASE_URL', 'DATABASE_URL → db · url (role api)'], ['migrate', 'uses[0]', 'uses → db']]);
    const s = { ...SPEC, services: { ...SPEC.services, api: { ...SPEC.services.api, uses: [{ service: 'catalog', port: 'http' }, { addon: 'db' }] } } };
    expect(refLine(referencesTo(s, 'services', 'catalog').find((r) => r.via === 'uses'))).toBe('uses → catalog:http');
    const renamed = renameObject(s, 'services', 'catalog', 'shelf');
    expect(renamed.services.api.uses).toEqual([{ service: 'shelf', port: 'http' }, { addon: 'db' }]);
    expect(renameObject(s, 'addons', 'db', 'pg').jobs.migrate.uses).toEqual([{ addon: 'pg' }]);
    const gone = removeWithRefs(s, 'addons', 'db');
    expect(gone.services.api.uses).toEqual([{ service: 'catalog', port: 'http' }]);
    expect('uses' in gone.jobs.migrate).toBe(false);
    expect(gone.services.catalog).toBe(s.services.catalog);
  });

  it('offers the other services with their ports and the addons to use', () => {
    const t = usesTargets(SPEC, 'api');
    expect(t.services.map((x) => [x.name, x.ports, x.attached])).toEqual([['catalog', ['http', 9090], false], ['preview', ['http'], true]]);
    expect(t.addons).toEqual([{ name: 'db', type: 'postgres' }]);
    expect(usesTargets(SPEC).services.map((x) => x.name)).toEqual(['api', 'catalog', 'preview']);
    expect([useWords({ service: 'catalog', port: 'http' }), useWords({ addon: 'db' })]).toEqual(['catalog:http', 'db']);
  });
});

describe('what a sandbox or a job reaches', () => {
  const choices = reachChoices(SPEC);

  it('chooses among the deployed deployment\'s service ports and addons', () => {
    expect(choices.map((c) => [c.key, c.name, c.detail])).toEqual([
      ['service:api:http', 'api:http', '8080'], ['service:catalog:http', 'catalog:http', '8080'], ['service:catalog:9090', 'catalog:9090', ''],
      ['service:preview:http', 'preview:http', '3000 · attached'], ['addon:db', 'db', 'postgres']]);
  });

  it('ticks entries in the order of the choices, and compares them as sets', () => {
    const l = toggleReach(toggleReach([], { addon: 'db' }, true, choices), { service: 'api', port: 'http' }, true, choices);
    expect(l).toEqual([{ service: 'api', port: 'http' }, { addon: 'db' }]);
    expect(toggleReach(l, { addon: 'db' }, false, choices)).toEqual([{ service: 'api', port: 'http' }]);
    expect(sameReaches(l, [{ addon: 'db' }, { service: 'api', port: 'http' }])).toBe(true);
    expect([sameReaches('all', 'all'), sameReaches('all', []), sameReaches([], undefined)]).toEqual([true, false, true]);
    expect(missingReaches([{ addon: 'db' }, { service: 'gone', port: 'http' }], choices)).toEqual([{ service: 'gone', port: 'http' }]);
  });

  it('reads and says a view\'s reaches, and makes a request\'s', () => {
    expect([reachesOf({ reaches: 'all' }), reachesOf({ reaches: [] }), reachesOf({})]).toEqual(['all', [], []]);
    expect(reachesWords('all')).toBe('all of the environment');
    expect(reachesWords([])).toBe('nothing of the environment');
    expect(reachesWords([{ service: 'api', port: 'http' }, { addon: 'db' }])).toBe('api:http, db');
    expect([reachesMode('all'), reachesMode([]), reachesMode([{ addon: 'db' }])]).toEqual(['all', 'none', 'some']);
    expect(reachesValue('all', [{ addon: 'db' }])).toBe('all');
    expect(reachesValue('some', [{ addon: 'db' }])).toEqual([{ addon: 'db' }]);
    expect(reachesValue('some', [])).toBeUndefined();
    expect(reachesValue('none', [{ addon: 'db' }])).toBeUndefined();
  });
});

describe('the environment of a sandbox or a job', () => {
  it('reads its environment and matches it by name or ID', () => {
    const sb = { environment: { id: 'e1', name: 'kino-dev' } };
    expect(envRef(sb)).toEqual({ id: 'e1', name: 'kino-dev' });
    expect(envRef({})).toBeNull();
    expect([inEnv(sb, 'kino-dev'), inEnv(sb, 'e1'), inEnv(sb, 'stage'), inEnv(sb, '')]).toEqual([true, true, false, false]);
  });

  it('reads a refused delete: what keeps the environment', () => {
    const e = new ApiError('environment kino-dev is in use', 409,
      { error: 'x', code: 'environment_in_use', sandboxes: ['h3k9v2m7qpx'], jobs: ['k2m7qpxh3v9'] });
    expect(inUse(e)).toEqual({ sandboxes: ['h3k9v2m7qpx'], jobs: ['k2m7qpxh3v9'] });
    expect(inUse(new ApiError('x', 409, { code: 'environment_in_use' }))).toEqual({ sandboxes: [], jobs: [] });
    expect(inUse(new ApiError('x', 409, { code: 'busy' }))).toBeNull();
    expect(inUse(new ApiError('x', 403, { code: 'environment_in_use' }))).toBeNull();
  });
});
