// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  accessOf, addonMark, bindingOf, bindingSource, credentialBinding, credentialPinNote, fieldsWords, fmtNumber,
  inventoryOf, keyAtNode, kindShort, problemWords, secretPinNote, secretUses, soonestCert, strayUsage, upstreamRow,
  usageOf, usageTone, usageWords, usersWords, withKeys, workloadOf,
} from './access.js';
import { readCatalog } from './catalog.js';
import { reachModel } from './reach.js';
import { response } from '../testing/ferun.js';

const catalog = {
  byType: new Map([
    ['postgres', { type: 'postgres', outputs: ['host', 'port', 'user', 'database', 'url'], byIdentity: true }],
    ['valkey', { type: 'valkey', outputs: ['host', 'port', 'url'], byIdentity: true }],
    ['nats', { type: 'nats', outputs: ['host', 'port', 'user', 'password', 'url'], byIdentity: false }],
  ]),
};

const spec = {
  services: {
    api: {
      image: 'ghcr.io/x/api:1',
      env: {
        DATABASE_URL: { addon: 'db', key: 'url', role: 'api_svc', database: 'app' },
        CACHE: { addon: 'cache', key: 'url' },
        SESSION_KEY: { secret: 'session-key' },
        NATS_URL: { addon: 'bus', key: 'url' },
        NATS_HOST: { addon: 'bus', key: 'host' },
      },
      imageCredentials: { username: 'bot', password: { secret: 'ghcr-pull' } },
      bindings: ['erp', 'github'],
    },
    web: { image: 'ghcr.io/x/web:1', env: { NATS_HOST: { addon: 'bus', key: 'host' } } },
  },
  jobs: { migrate: { image: 'ghcr.io/x/api:1', env: { DATABASE_URL: { addon: 'db', key: 'url', role: 'owner' } } } },
  addons: { db: { type: 'postgres' }, cache: { type: 'valkey' }, bus: { type: 'nats' }, spare: { type: 'valkey' } },
  bindings: {
    erp: { upstream: 'https://api.vendor.example', mode: 'intercept',
      credential: { kind: 'oauth2', clientId: 'radar', clientSecret: { secret: 'erp-secret' }, tokenUrl: 'https://id.vendor.example/token' } },
    github: { credentialRef: { name: 'github', version: 3 } },
  },
  secrets: { 'session-key': { version: 2 }, 'erp-secret': { version: 1 } },
};

const credentials = [
  { name: 'github', version: 4, upstream: 'https://api.github.com', mode: 'name', kind: 'headers',
    usedBy: { sandboxes: [], bindings: ['github'], latest: 3, deployed: 3 } },
  { name: 'old', version: 1, upstream: 'https://old.example', mode: 'name', kind: 'basic' },
  { name: 'models', version: 2, upstream: 'https://models.example', mode: 'name', kind: 'headers',
    usedBy: { sandboxes: ['s2', 's1'], bindings: [], latest: 0, deployed: 0 } },
];

describe('addonMark', () => {
  it('admits by identity when the type says so, whatever the key', () => {
    expect(addonMark(catalog.byType.get('postgres'), ['url'])).toBe('identity');
  });
  it('hands the password over in the container, also inside a url', () => {
    expect(addonMark(catalog.byType.get('nats'), ['password'])).toBe('container');
    expect(addonMark(catalog.byType.get('nats'), ['url'])).toBe('container');
  });
  it('hands no key over for host and port alone', () => {
    expect(addonMark(catalog.byType.get('nats'), ['host', 'port'])).toBe(null);
  });
  it('knows nothing of an unknown type', () => {
    expect(addonMark(null, ['url'])).toBe(null);
  });
});

describe('bindingOf', () => {
  it('reads an inline binding', () => {
    expect(bindingOf(spec, 'erp', credentials)).toEqual({
      name: 'erp', upstream: 'https://api.vendor.example', mode: 'intercept', kind: 'oauth2',
      reachedAs: 'api.vendor.example', credential: null, secrets: ['erp-secret'],
    });
  });
  it("fills a credential reference in from the environment's credential", () => {
    expect(bindingOf(spec, 'github', credentials)).toEqual({
      name: 'github', upstream: 'https://api.github.com', mode: 'name', kind: 'headers',
      reachedAs: 'http://github', credential: { name: 'github', version: 3 }, secrets: [],
    });
  });
});

describe('accessOf', () => {
  it("lists a service's access sorted by where the key lives", () => {
    const rows = accessOf(spec, 'services', 'api', { catalog, credentials });
    expect(rows.map((r) => [r.mark, r.kind, r.target.name])).toEqual([
      ['identity', 'addon', 'cache'],
      ['identity', 'addon', 'db'],
      ['offloaded', 'binding', 'erp'],
      ['offloaded', 'image', 'ghcr-pull'],
      ['offloaded', 'binding', 'github'],
      ['container', 'addon', 'bus'],
      ['container', 'secret', 'session-key'],
    ]);
    const db = rows.find((r) => r.target.name === 'db');
    expect(db.variables).toEqual(['DATABASE_URL']);
    expect(db.fields).toEqual({ role: 'api_svc', database: 'app' });
    expect(rows.find((r) => r.target.name === 'bus').variables).toEqual(['NATS_HOST', 'NATS_URL']);
  });
  it('gives an addon reached by host alone no mark', () => {
    expect(accessOf(spec, 'services', 'web', { catalog })).toEqual([
      { target: { key: 'addons', name: 'bus' }, kind: 'addon', mark: null, variables: ['NATS_HOST'], keys: ['host'],
        addon: { type: 'nats' }, fields: {} },
    ]);
  });
});

describe('secretUses', () => {
  it("names a binding's users for a secret its credential names", () => {
    expect(secretUses(spec, 'erp-secret')).toEqual([
      { from: { key: 'services', name: 'api' }, mark: 'offloaded', how: 'binding', binding: 'erp' },
    ]);
  });
  it('tells a variable from a registry login', () => {
    expect(secretUses(spec, 'session-key').map((u) => u.mark)).toEqual(['container']);
    expect(secretUses(spec, 'ghcr-pull')).toEqual([{ from: { key: 'services', name: 'api' }, mark: 'offloaded', how: 'image' }]);
  });
});

describe('inventoryOf', () => {
  const secrets = [{ name: 'session-key', version: 2 }, { name: 'erp-secret', version: 1 }, { name: 'leftover', version: 1 }];
  const rows = inventoryOf(spec, { catalog, credentials, secrets });
  const row = (kind, name) => rows.find((r) => r.kind === kind && r.name === name);

  it('marks a secret by its uses and says when it has no value or no user', () => {
    expect(row('secret', 'session-key')).toMatchObject({ marks: ['container'], users: [{ key: 'services', name: 'api' }], state: 'set' });
    expect(row('secret', 'ghcr-pull')).toMatchObject({ marks: ['offloaded'], state: 'missing' });
    expect(row('secret', 'leftover')).toMatchObject({ marks: [], users: [], state: 'unused' });
  });
  it("names a credential's bindings and their users, the sandboxes that name it and what its deployments pin", () => {
    expect(row('credential', 'github')).toMatchObject({ bindings: ['github'], users: [{ key: 'services', name: 'api' }], state: 'set', version: 4,
      sandboxes: [], pins: { latest: 3, deployed: 3 } });
    expect(row('credential', 'old')).toMatchObject({ bindings: [], sandboxes: [], state: 'unused', pins: { latest: 0, deployed: 0 } });
    // A credential only sandboxes name is in use.
    expect(row('credential', 'models')).toMatchObject({ bindings: [], users: [], sandboxes: ['s1', 's2'], state: 'set' });
  });
  it('says the version the spec pins of a secret', () => {
    expect(row('secret', 'session-key')).toMatchObject({ version: 2, pinned: 2 });
    expect(row('secret', 'leftover')).toMatchObject({ version: 1, pinned: 0 });
  });
  it('lists the addons that admit by identity or hand out a password', () => {
    expect(row('addon', 'db')).toMatchObject({ marks: ['identity'],
      users: [{ key: 'jobs', name: 'migrate' }, { key: 'services', name: 'api' }] });
    expect(row('addon', 'db').uses).toEqual([
      { from: { key: 'jobs', name: 'migrate' }, mark: 'identity', fields: { role: 'owner' }, variables: ['DATABASE_URL'] },
      { from: { key: 'services', name: 'api' }, mark: 'identity', fields: { role: 'api_svc', database: 'app' }, variables: ['DATABASE_URL'] },
    ]);
    expect(row('addon', 'bus')).toMatchObject({ marks: ['container'], users: [{ key: 'services', name: 'api' }] });
    expect(row('addon', 'spare')).toMatchObject({ marks: ['identity'], users: [], state: 'unused' });
  });
});

describe("an access row's words", () => {
  it('numbers with thousands separators', () => {
    expect(fmtNumber(1204)).toBe('1,204');
    expect(fmtNumber(1234567)).toBe('1,234,567');
    expect(fmtNumber(12)).toBe('12');
    expect(fmtNumber(undefined)).toBe('0');
  });
  it("a binding's kind short and where its key comes from", () => {
    expect(kindShort('oauth2')).toBe('OAuth2');
    expect(kindShort('clientCertificate')).toBe('client certificate');
    expect(bindingSource(bindingOf(spec, 'github', credentials), credentials)).toBe('credential github v3, latest v4');
    expect(bindingSource(bindingOf(spec, 'github', []), [])).toBe('credential github v3');
    expect(bindingSource(bindingOf(spec, 'erp', credentials), credentials)).toBe('secret erp-secret');
    expect(bindingSource({ credential: null, secrets: ['a', 'b'] })).toBe('secrets a, b');
    expect(bindingSource({ credential: null, secrets: [] })).toBe('');
  });
  it("an addon reference's fields", () => {
    expect(fieldsWords({ role: 'api_svc', database: 'app' })).toBe('database app · role api_svc');
    expect(fieldsWords({})).toBe('');
  });
  it("what pins a version behind the latest", () => {
    expect(secretPinNote(2, 3)).toBe('the latest deployment pins v2, latest v3');
    expect(secretPinNote(3, 3)).toBe('');
    expect(secretPinNote(0, 3)).toBe('');
    expect(credentialPinNote({ latest: 3, deployed: 3 }, 4)).toBe('deployed pins v3, latest v4');
    expect(credentialPinNote({ latest: 3, deployed: 0 }, 4)).toBe('the latest deployment pins v3, latest v4');
    expect(credentialPinNote({ latest: 3, deployed: 2 }, 4)).toBe('the latest deployment pins v3, deployed v2; latest v4');
    expect(credentialPinNote({ latest: 4, deployed: 3 }, 4)).toBe('v4 rolling out; deployed pins v3');
    expect(credentialPinNote({ latest: 0, deployed: 3 }, 4)).toBe('deployed pins v3, latest v4');
    expect(credentialPinNote({ latest: 4, deployed: 4 }, 4)).toBe('');
    expect(credentialPinNote({ latest: 0, deployed: 0 }, 4)).toBe('');
  });
});

describe("the bindings' use and the workloads' identities, as ferund answers them", () => {
  const usage = response('GET_environments_env_bindings_usage.200');
  const down = response('GET_environments_env_bindings_usage.200.unavailable');
  const ids = response('GET_environments_env_identities.200');

  it("finds a binding's numbers, none while the log store is unavailable", () => {
    expect(usageOf(usage, 'erp')).toMatchObject({ requests: 1204, refused: 2 });
    expect(usageOf(usage, 'nope')).toBe(null);
    expect(usageOf(down, 'erp')).toBe(null);
    expect(usageOf(null, 'erp')).toBe(null);
  });
  it('says them in a few words, with a tone', () => {
    expect(usageWords(usageOf(usage, 'erp'))).toBe('1,204 req/24h · 2 refused · 1 blocked');
    expect(usageWords(usageOf(usage, 'mail'))).toBe('no request in 24h');
    expect(usageWords(null)).toBe('');
    expect(usageTone(usageOf(usage, 'erp'))).toBe('bad');
    expect(usageTone({ requests: 3, blocked: 1 })).toBe('warn');
    expect(usageTone(usageOf(usage, 'mail'))).toBe('');
    expect(problemWords(usageOf(usage, 'erp').lastProblem)).toBe('403 denied_path: path /admin is not allowed by binding erp');
    expect(problemWords({ status: 401, code: '', message: '' })).toBe('the upstream answered 401');
    expect(problemWords(null)).toBe('');
    expect(usersWords(usageOf(usage, 'erp').users)).toBe('service api 1,200 · sandbox c3kv9mwq2hd 4');
  });
  it("lists the bindings the pods named that the spec has none of", () => {
    const u = { window: '24h', bindings: [...usage.bindings, { name: 'models', requests: 9, users: [{ kind: 'sandbox', name: 's1', requests: 9 }] }] };
    expect(strayUsage(u, { bindings: { erp: {}, mail: {} } }).map((b) => b.name)).toEqual(['models']);
    expect(strayUsage(down, {})).toEqual([]);
  });
  it("finds a workload's identity and the certificate that ends first", () => {
    const api = workloadOf(ids, 'service', 'api');
    expect(api.spiffeId).toBe('spiffe://run.fecore.dev/tenant/fecode/env/2f0c6d1e-5b0a-4c55-9d43-0d1b7a3e8f21/service/api');
    expect(soonestCert(api.pods)).toEqual({ pod: 'd7r2m4k9pqs', notAfter: '2026-11-20T09:15:00Z', node: 'node-a' });
    expect(workloadOf(ids, 'job', 'migrate').pods).toEqual([]);
    expect(soonestCert([])).toBe(null);
    // A pod before its first certificate has none.
    expect(soonestCert(workloadOf(ids, 'sandbox', 'c3kv9mwq2hd').pods)).toBe(null);
    expect(workloadOf(ids, 'service', 'nope')).toBe(null);
    expect(workloadOf(null, 'service', 'api')).toBe(null);
  });
});

// The reachability picture with where its keys live, on ferund's answers:
// expensa's deployment (postgres by identity, NATS by its url), the
// addons' catalog and what the bindings carried.
describe('withKeys', () => {
  const dep = response('GET_environments_env_deployments_n.200');
  const cat = readCatalog(response('GET_addons.200'));
  const usage = response('GET_environments_env_bindings_usage.200').bindings;
  const bound = {
    ...dep.spec,
    services: {
      ...dep.spec.services,
      'expense-service': { ...dep.spec.services['expense-service'], bindings: ['erp', 'mail'] },
    },
    jobs: { migrate: { ...dep.spec.jobs.migrate, bindings: ['erp'] } },
    bindings: {
      erp: { upstream: 'https://api.vendor.example', mode: 'intercept', credential: { kind: 'oauth2', clientSecret: { secret: 'erp' } } },
      mail: { credentialRef: { name: 'mail', version: 2 } },
    },
  };
  const creds = [
    { name: 'mail', version: 2, upstream: 'https://api.mail.example', mode: 'name', credential: { kind: 'headers' },
      usedBy: { sandboxes: [], bindings: ['mail'] } },
    { name: 'erp', version: 1, upstream: 'https://api.vendor.example', mode: 'name', credential: { kind: 'basic' },
      usedBy: { sandboxes: ['c3kv9mwq2hd'], bindings: [] } },
  ];
  const pods = [{ kind: 'sandbox', id: 'c3kv9mwq2hd', reaches: [], egress: 'none' }, { kind: 'sandbox', id: 'other', reaches: [] }];
  const of = (m, ref) => m.objects.find((o) => o.ref === ref) || m.pods.find((o) => o.ref === ref);
  const line = (r) => `${r.peer.type}:${r.peer.name}${r.mark ? ` [${r.mark}]` : ''}`;

  it('marks the edges to addons and leaves those between services alone', () => {
    const m = withKeys(reachModel(dep.spec, dep.reachability), dep.spec, { catalog: cat });
    expect(of(m, 'service/expense-service').outs.map(line)).toEqual([
      'service:approval-service', 'addon:nats [container]', 'addon:postgres [identity]', 'internet:Internet']);
    expect(of(m, 'addon/postgres').ins.map(line)).toEqual([
      'service:approval-service [identity]', 'service:expense-service [identity]', 'job:migrate [identity]']);
    expect(of(m, 'service/approval-service').ins.map(line)).toEqual(['ingress:Ingress', 'service:expense-service']);
    expect(m.marks).toEqual(['identity', 'container']);
  });

  it('marks nothing without a catalog, and adds no row without a binding', () => {
    const plain = reachModel(dep.spec, dep.reachability);
    const m = withKeys(plain, dep.spec, {});
    expect(m.marks).toEqual([]);
    expect(m.objects.map((o) => o.outs.length)).toEqual(plain.objects.map((o) => o.outs.length));
    expect(m.objects.flatMap((o) => [...o.ins, ...o.outs]).some((r) => r.mark)).toBe(false);
  });

  it("puts each binding's upstream on the right, before the egress, its key at the node", () => {
    const m = withKeys(reachModel(bound, dep.reachability, pods), bound, { catalog: cat, credentials: creds, usage });
    expect(of(m, 'service/expense-service').outs.map(line)).toEqual([
      'service:approval-service', 'addon:nats [container]', 'addon:postgres [identity]',
      'upstream:api.vendor.example [offloaded]', 'upstream:api.mail.example [offloaded]', 'internet:Internet']);
    const [erp, mail] = of(m, 'service/expense-service').outs.filter((r) => r.peer.type === 'upstream');
    expect(erp).toMatchObject({ key: 'binding:erp', what: 'OAuth2 at the node', via: ['binding erp'],
      peer: { sub: 'erp · as api.vendor.example' }, binding: { upstream: 'https://api.vendor.example', mode: 'intercept' },
      usage: { requests: 1204, refused: 2, failing: 0, mine: 0, by: 'expense-service' } });
    expect(mail).toMatchObject({ what: 'Headers at the node', peer: { sub: 'mail · as http://mail' },
      binding: { credential: { name: 'mail', version: 2 } }, usage: { requests: 0, mine: 0 } });
    expect(of(m, 'job/migrate').outs.map(line)).toEqual([
      'addon:postgres [identity]', 'upstream:api.vendor.example [offloaded]', 'internet:Internet']);
    expect(m.marks).toEqual(['identity', 'offloaded', 'container']);
  });

  it("gives a sandbox its credentials' bindings, with its own requests", () => {
    const m = withKeys(reachModel(bound, dep.reachability, pods), bound, { credentials: creds, usage });
    const sb = of(m, 'sandbox/c3kv9mwq2hd').outs;
    expect(sb.map(line)).toEqual(['upstream:api.vendor.example [offloaded]']);
    expect(sb[0]).toMatchObject({ what: 'Basic auth at the node', peer: { sub: 'erp · as http://erp' },
      usage: { requests: 1204, mine: 4, by: 'c3kv9mwq2hd' } });
    expect(of(m, 'sandbox/other').outs.map(line)).toEqual(['internet:Internet']);
  });

  it('says no numbers without usage, and the binding when its upstream is unknown', () => {
    const r = upstreamRow(bindingOf({ bindings: { gone: { credentialRef: { name: 'gone' } } } }, 'gone', []));
    expect(r).toMatchObject({ peer: { type: 'upstream', name: 'gone', sub: 'gone · as http://gone' }, what: 'Key at the node', usage: null });
    expect(upstreamRow(bindingOf(bound, 'erp'), { requests: 3 }, { kind: 'service', name: 'x' }).usage)
      .toEqual({ requests: 3, refused: 0, failing: 0, mine: null, by: 'x' });
  });

  it('names the kinds of a key, and reads a credential as a sandbox holds it', () => {
    expect(['headers', 'basic', 'oauth2', 'clientCertificate', ''].map(keyAtNode)).toEqual([
      'Headers at the node', 'Basic auth at the node', 'OAuth2 at the node', 'Client certificate at the node', 'Key at the node']);
    expect(credentialBinding(creds[0])).toMatchObject({ name: 'mail', upstream: 'https://api.mail.example', mode: 'name',
      kind: 'headers', reachedAs: 'http://mail', credential: { name: 'mail', version: 2 } });
  });
});
