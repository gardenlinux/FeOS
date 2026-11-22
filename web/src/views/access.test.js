// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  INVENTORY_GROUPS, accessOf, addonMark, bindingOf, bindingSource, fieldsWords, fmtNumber, groupRows, httpOf, httpPinNote,
  identityReaders, inventoryOf, keyAtNode, kindShort, kindsOf, podSubject, problemWords, secretPinNote, secretUses,
  soonestCert, strayUsage, subjectParts, upstreamRow, usageOf, usageTone, usageWords, usersWords, withKeys, workloadOf,
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

// A service api with an HTTP secret erp by a variable (ADR 0055), a
// binding stated in the spec (mail, its key a value secret) and one that
// names an HTTP secret (github, an environment's credential before),
// its addons, a value secret and a registry login.
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
        ERP_URL: { secret: 'erp', key: 'url' },
        ERP_HOST: { secret: 'erp', key: 'host' },
      },
      imageCredentials: { username: 'bot', password: { secret: 'ghcr-pull' } },
      bindings: ['mail', 'github'],
    },
    web: { image: 'ghcr.io/x/web:1', env: { NATS_HOST: { addon: 'bus', key: 'host' } } },
  },
  jobs: {
    migrate: { image: 'ghcr.io/x/api:1', env: { DATABASE_URL: { addon: 'db', key: 'url', role: 'owner' }, ERP: { secret: 'erp', key: 'url' } } },
  },
  addons: { db: { type: 'postgres' }, cache: { type: 'valkey' }, bus: { type: 'nats' }, spare: { type: 'valkey' } },
  bindings: {
    mail: { upstream: 'https://api.mail.example', credential: { kind: 'basic', user: 'app', password: { secret: 'mail-pass' } } },
    github: { credentialRef: { name: 'github', version: 3 } },
  },
  secrets: {
    'session-key': { version: 2 }, 'mail-pass': { version: 1 },
    erp: { version: 2, type: 'http', upstream: 'https://api.vendor.example', mode: 'intercept' },
  },
};

// The environment's secrets, both kinds, as GET …/secrets answers them.
const secrets = [
  { name: 'erp', type: 'http', version: 3, upstream: 'https://api.vendor.example', mode: 'intercept', credential: { kind: 'oauth2' },
    usedBy: { sandboxes: ['s1'], objects: ['service/api', 'job/migrate'], latest: 2, deployed: 2 } },
  { name: 'github', type: 'http', version: 4, upstream: 'https://api.github.com', mode: 'name', credential: { kind: 'headers' },
    usedBy: { sandboxes: [], objects: [], bindings: ['github'], latest: 3, deployed: 3 } },
  { name: 'models', type: 'http', version: 2, upstream: 'https://models.example', mode: 'name', credential: { kind: 'headers' },
    usedBy: { sandboxes: ['s2', 's1'], objects: [], latest: 0, deployed: 0 } },
  { name: 'old', type: 'http', version: 1, upstream: 'https://old.example', mode: 'name', credential: { kind: 'basic' },
    usedBy: { sandboxes: [], objects: [], latest: 0, deployed: 0 } },
  { name: 'session-key', type: 'value', version: 2 },
  { name: 'mail-pass', type: 'value', version: 1 },
  { name: 'leftover', type: 'value', version: 1 },
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

describe('kindsOf', () => {
  it("takes a secret's kind from the environment's list, else from the spec", () => {
    const k = kindsOf(spec, secrets);
    expect([k.get('erp'), k.get('github'), k.get('session-key'), k.get('ghcr-pull')]).toEqual(['http', 'http', 'value', 'value']);
    // Without the list: a reference with a key, a pin of type http and a
    // binding's credentialRef say http; anything else is a value.
    const bare = kindsOf(spec, []);
    expect([bare.get('erp'), bare.get('github'), bare.get('session-key'), bare.get('mail-pass')]).toEqual(['http', 'http', 'value', 'value']);
    // An older ferund's item without a type is a value.
    expect(kindsOf({}, [{ name: 'x', version: 1 }]).get('x')).toBe('value');
  });
});

describe('bindingOf', () => {
  it('reads a binding stated in the spec', () => {
    expect(bindingOf(spec, 'mail', secrets)).toEqual({
      name: 'mail', source: 'binding', upstream: 'https://api.mail.example', mode: 'name', kind: 'basic',
      reachedAs: 'http://mail', credential: null, secrets: ['mail-pass'],
    });
  });
  it('fills a credentialRef in from the HTTP secret of its name', () => {
    expect(bindingOf(spec, 'github', secrets)).toEqual({
      name: 'github', source: 'binding', upstream: 'https://api.github.com', mode: 'name', kind: 'headers',
      reachedAs: 'http://github', credential: { name: 'github', version: 3 }, secrets: [],
    });
  });
});

describe('accessOf', () => {
  it("lists a service's access sorted by where the key lives, an HTTP secret once for its variables", () => {
    const rows = accessOf(spec, 'services', 'api', { catalog, secrets });
    expect(rows.map((r) => [r.mark, r.kind, r.target.name])).toEqual([
      ['identity', 'addon', 'cache'],
      ['identity', 'addon', 'db'],
      ['offloaded', 'http', 'erp'],
      ['offloaded', 'image', 'ghcr-pull'],
      ['offloaded', 'http', 'github'],
      ['offloaded', 'binding', 'mail'],
      ['container', 'addon', 'bus'],
      ['container', 'secret', 'session-key'],
    ]);
    const db = rows.find((r) => r.target.name === 'db');
    expect(db.variables).toEqual(['DATABASE_URL']);
    expect(db.fields).toEqual({ role: 'api_svc', database: 'app' });
    expect(rows.find((r) => r.target.name === 'bus').variables).toEqual(['NATS_HOST', 'NATS_URL']);
  });
  it('gives an HTTP secret its variables and keys, the version the spec pins and the latest', () => {
    const rows = accessOf(spec, 'services', 'api', { catalog, secrets });
    expect(rows.find((r) => r.target.name === 'erp')).toMatchObject({
      variables: ['ERP_HOST', 'ERP_URL'], keys: ['host', 'url'], via: [],
      http: { name: 'erp', source: 'http', upstream: 'https://api.vendor.example', mode: 'intercept', kind: 'oauth2',
        reachedAs: 'api.vendor.example', version: 3, pinned: 2, known: true },
    });
    // A binding of the spec that names an HTTP secret is that HTTP secret's row, through it.
    expect(rows.find((r) => r.target.name === 'github')).toMatchObject({ kind: 'http', variables: [], via: ['github'],
      http: { pinned: 3, version: 4, reachedAs: 'http://github' } });
  });
  it('knows an HTTP secret from the spec alone, without its definition', () => {
    const rows = accessOf(spec, 'jobs', 'migrate', { catalog });
    expect(rows.find((r) => r.target.name === 'erp')).toMatchObject({ kind: 'http', mark: 'offloaded',
      http: { known: false, pinned: 2, version: 0 } });
  });
  it('gives an addon reached by host alone no mark', () => {
    expect(accessOf(spec, 'services', 'web', { catalog })).toEqual([
      { target: { key: 'addons', name: 'bus' }, kind: 'addon', mark: null, variables: ['NATS_HOST'], keys: ['host'],
        addon: { type: 'nats' }, fields: {} },
    ]);
  });
});

describe('secretUses', () => {
  it("names a binding's users for a value secret its credential names", () => {
    expect(secretUses(spec, 'mail-pass')).toEqual([
      { from: { key: 'services', name: 'api' }, mark: 'offloaded', how: 'binding', binding: 'mail' },
    ]);
  });
  it('tells a variable from a registry login', () => {
    expect(secretUses(spec, 'session-key').map((u) => u.mark)).toEqual(['container']);
    expect(secretUses(spec, 'ghcr-pull')).toEqual([{ from: { key: 'services', name: 'api' }, mark: 'offloaded', how: 'image' }]);
  });
});

describe('inventoryOf', () => {
  const rows = inventoryOf(spec, { catalog, secrets });
  const row = (kind, name) => rows.find((r) => r.kind === kind && r.name === name);

  it('puts each key once, where it lives: an HTTP secret at the node whoever uses it', () => {
    const at = (place) => groupRows(rows, place).map((r) => `${r.kind} ${r.name}`);
    expect(INVENTORY_GROUPS.map((g) => g.place)).toEqual(['identity', 'offloaded', 'container', 'unused']);
    expect(at('identity')).toEqual(['addon cache', 'addon db', 'addon spare']);
    expect(at('offloaded')).toEqual(['http erp', 'http github', 'http models', 'http old', 'binding mail', 'value ghcr-pull']);
    expect(at('container')).toEqual(['value session-key', 'addon bus']);
    expect(at('unused')).toEqual(['value leftover']);
    // The key of a binding of the spec is its value secret: in its row, not one of its own.
    expect(row('value', 'mail-pass')).toMatchObject({ folded: true, bindings: ['mail'], place: 'offloaded' });
    expect(rows.filter((r) => r.name === 'mail-pass')).toHaveLength(1);
  });
  it('marks a value secret by its uses and says when it has no value or no user', () => {
    expect(row('value', 'session-key')).toMatchObject({ marks: ['container'], users: [{ key: 'services', name: 'api' }], state: 'set' });
    expect(row('value', 'ghcr-pull')).toMatchObject({ marks: ['offloaded'], state: 'missing' });
    expect(row('value', 'leftover')).toMatchObject({ marks: [], users: [], state: 'unused', place: 'unused' });
  });
  it("names an HTTP secret's users with their variables, the sandboxes at the latest, what its deployments pin", () => {
    expect(row('http', 'erp')).toMatchObject({
      users: [{ key: 'jobs', name: 'migrate', variables: ['ERP'], bindings: [] },
        { key: 'services', name: 'api', variables: ['ERP_HOST', 'ERP_URL'], bindings: [] }],
      sandboxes: ['s1'], pins: { latest: 2, deployed: 2 }, pinned: 2, version: 3, state: 'set',
      http: { reachedAs: 'api.vendor.example', kind: 'oauth2' },
    });
    // Through a binding of the spec (credentialRef), with the version it pins.
    expect(row('http', 'github')).toMatchObject({ users: [{ key: 'services', name: 'api', variables: [], bindings: ['github'] }],
      bindings: ['github'], pinned: 3, pins: { latest: 3, deployed: 3 } });
    expect(row('http', 'old')).toMatchObject({ users: [], sandboxes: [], state: 'unused', place: 'offloaded' });
    // One only sandboxes name is in use.
    expect(row('http', 'models')).toMatchObject({ users: [], sandboxes: ['s1', 's2'], state: 'set' });
  });
  it("adds the objects ferund's usedBy names, and an HTTP secret the spec names that the list lacks is missing", () => {
    const r = inventoryOf({}, { secrets }).find((x) => x.name === 'erp');
    expect(r.users).toEqual([{ key: 'services', name: 'api', variables: [], bindings: [] }, { key: 'jobs', name: 'migrate', variables: [], bindings: [] }]);
    const gone = inventoryOf(spec, { catalog, secrets: secrets.filter((x) => x.name !== 'erp') }).find((x) => x.name === 'erp');
    expect(gone).toMatchObject({ kind: 'http', state: 'missing', place: 'offloaded', item: null, pinned: 2 });
  });
  it("says a binding of the spec's key and when an HTTP secret of its name takes its place", () => {
    expect(row('binding', 'mail')).toMatchObject({ users: [{ key: 'services', name: 'api' }], place: 'offloaded', taken: false,
      secrets: [{ name: 'mail-pass', state: 'set', version: 1, pinned: 1 }], binding: { upstream: 'https://api.mail.example' } });
    const taken = inventoryOf(spec, { catalog, secrets: [...secrets, { name: 'mail', type: 'http', version: 1, upstream: 'https://api.mail.example' }] });
    expect(taken.find((r) => r.kind === 'binding' && r.name === 'mail').taken).toBe(true);
    // A binding of an HTTP secret (credentialRef) has no row: its HTTP secret's is it.
    expect(row('binding', 'github')).toBeUndefined();
  });
  it('says the version the spec pins of a value secret', () => {
    expect(row('value', 'session-key')).toMatchObject({ version: 2, pinned: 2 });
    expect(row('value', 'leftover')).toMatchObject({ version: 1, pinned: 0 });
  });
  it('lists the addons that admit by identity or hand out a password', () => {
    expect(row('addon', 'db')).toMatchObject({ marks: ['identity'], place: 'identity',
      users: [{ key: 'jobs', name: 'migrate' }, { key: 'services', name: 'api' }] });
    expect(row('addon', 'db').uses).toEqual([
      { from: { key: 'jobs', name: 'migrate' }, mark: 'identity', fields: { role: 'owner' }, variables: ['DATABASE_URL'] },
      { from: { key: 'services', name: 'api' }, mark: 'identity', fields: { role: 'api_svc', database: 'app' }, variables: ['DATABASE_URL'] },
    ]);
    expect(row('addon', 'bus')).toMatchObject({ marks: ['container'], place: 'container', users: [{ key: 'services', name: 'api' }] });
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
    expect(bindingSource(bindingOf(spec, 'github', secrets), secrets)).toBe('HTTP secret github v3, latest v4');
    expect(bindingSource(bindingOf(spec, 'github', []), [])).toBe('HTTP secret github v3');
    expect(bindingSource(bindingOf(spec, 'mail', secrets), secrets)).toBe('secret mail-pass');
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
    expect(httpPinNote({ latest: 3, deployed: 3 }, 4)).toBe('deployed pins v3, latest v4');
    expect(httpPinNote({ latest: 3, deployed: 0 }, 4)).toBe('the latest deployment pins v3, latest v4');
    expect(httpPinNote({ latest: 3, deployed: 2 }, 4)).toBe('the latest deployment pins v3, deployed v2; latest v4');
    expect(httpPinNote({ latest: 4, deployed: 3 }, 4)).toBe('v4 rolling out; deployed pins v3');
    expect(httpPinNote({ latest: 0, deployed: 3 }, 4)).toBe('deployed pins v3, latest v4');
    expect(httpPinNote({ latest: 4, deployed: 4 }, 4)).toBe('');
    expect(httpPinNote({ latest: 0, deployed: 0 }, 4)).toBe('');
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
  it("lists the bindings the pods named that are neither the spec's nor an HTTP secret", () => {
    const u = { window: '24h', bindings: [...usage.bindings, { name: 'models', requests: 9, users: [{ kind: 'sandbox', name: 's1', requests: 9 }] },
      { name: 'own', requests: 2, users: [{ kind: 'sandbox', name: 's1', requests: 2 }] }] };
    expect(strayUsage(u, { bindings: { mail: {} } }, secrets).map((b) => b.name)).toEqual(['own']);
    expect(strayUsage(u, { bindings: { erp: {}, mail: {} } }).map((b) => b.name)).toEqual(['models', 'own']);
    expect(strayUsage(down, {})).toEqual([]);
  });
  it("finds a workload's identity and the certificate that ends first", () => {
    const api = workloadOf(ids, 'service', 'api');
    expect(api.spiffeId).toBe('spiffe://run.fecore.dev/project/fecode/env/2f0c6d1e-5b0a-4c55-9d43-0d1b7a3e8f21/service/api');
    expect(soonestCert(api.pods)).toMatchObject({ pod: 'd7r2m4k9pqs', notAfter: '2026-11-20T09:15:00Z', node: 'node-a' });
    expect(workloadOf(ids, 'job', 'migrate').pods).toEqual([]);
    expect(soonestCert([])).toBe(null);
    // A pod before its first certificate has none.
    expect(soonestCert(workloadOf(ids, 'sandbox', 'c3kv9mwq2hd').pods)).toBe(null);
    expect(workloadOf(ids, 'service', 'nope')).toBe(null);
    expect(workloadOf(null, 'service', 'api')).toBe(null);
  });
});

describe("what a workload's certificate names", () => {
  const ids = response('GET_environments_env_identities.200');
  const api = workloadOf(ids, 'service', 'api');
  const ou = 'OU=' + api.ou;

  it('splits a subject into its attributes, in its order', () => {
    expect(subjectParts(api.pods[0].subject)).toEqual([['CN', 'run.fecore.dev/d7r2m4k9pqs'], ['OU', api.ou]]);
    expect(subjectParts('')).toEqual([]);
    expect(subjectParts(null)).toEqual([]);
  });
  it("takes a pod's subject as ferund answers it, else makes the workload's", () => {
    expect(podSubject(ids, api, api.pods[0])).toBe('CN=run.fecore.dev/d7r2m4k9pqs,' + ou);
    const migrate = workloadOf(ids, 'job', 'migrate');
    expect(podSubject(ids, migrate)).toBe(`CN=run.fecore.dev/<pod>,OU=${migrate.ou}`);
    // A ferund of before the OU, or one without an issuer, says none.
    expect(podSubject(ids, { ...migrate, ou: undefined })).toBe('');
    expect(podSubject({ trustDomain: '' }, migrate)).toBe('');
  });
  it('names the addons that admit it by its certificate and the part each reads', () => {
    const cat = readCatalog(response('GET_addons.200'));
    const rows = [
      { kind: 'addon', mark: 'identity', target: { name: 'db' }, addon: { type: 'postgres' }, fields: { role: 'api_svc' } },
      { kind: 'addon', mark: 'identity', target: { name: 'cache' }, addon: { type: 'valkey' }, fields: {} },
      { kind: 'addon', mark: 'container', target: { name: 'bus' }, addon: { type: 'nats' }, fields: {} },
      { kind: 'http', mark: 'offloaded', target: { name: 'erp' } },
    ];
    expect(identityReaders(rows, cat)).toEqual([
      { addon: 'cache', type: 'valkey', by: 'uri', role: '' },
      { addon: 'db', type: 'postgres', by: 'ou', role: 'api_svc' },
    ]);
    // A catalog of before identityBy says nothing of the part.
    expect(identityReaders(rows.slice(0, 1), null)).toEqual([{ addon: 'db', type: 'postgres', by: '', role: 'api_svc' }]);
  });
});

// The reachability picture with where its keys live, on ferund's answers:
// expensa's deployment (postgres by identity, NATS by its url), the
// addons' catalog and what the bindings carried.
describe('withKeys', () => {
  const dep = response('GET_environments_env_deployments_n.200');
  const cat = readCatalog(response('GET_addons.200'));
  const usage = response('GET_environments_env_bindings_usage.200').bindings;
  const es = dep.spec.services['expense-service'];
  const bound = {
    ...dep.spec,
    services: {
      ...dep.spec.services,
      'expense-service': { ...es, env: { ...es.env, ERP_URL: { secret: 'erp', key: 'url' } }, bindings: ['mail'] },
    },
    jobs: { migrate: { ...dep.spec.jobs.migrate, env: { ...dep.spec.jobs.migrate.env, ERP_HOST: { secret: 'erp', key: 'host' } } } },
    bindings: {
      mail: { upstream: 'https://api.mail.example', credential: { kind: 'headers', headers: { Authorization: { secret: 'mail' } } } },
    },
    secrets: { ...(dep.spec.secrets || {}), erp: { version: 2, type: 'http', upstream: 'https://api.vendor.example', mode: 'intercept' } },
  };
  const list = [
    { name: 'erp', type: 'http', version: 2, upstream: 'https://api.vendor.example', mode: 'intercept', credential: { kind: 'oauth2' },
      usedBy: { sandboxes: [], objects: ['service/expense-service', 'job/migrate'], latest: 2, deployed: 2 } },
    { name: 'models', type: 'http', version: 1, upstream: 'https://models.example', mode: 'name', credential: { kind: 'basic' },
      usedBy: { sandboxes: ['c3kv9mwq2hd'], objects: [] } },
    { name: 'mail', type: 'value', version: 1 },
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

  it('marks nothing without a catalog, and adds no row without an API at the node', () => {
    const plain = reachModel(dep.spec, dep.reachability);
    const m = withKeys(plain, dep.spec, {});
    expect(m.marks).toEqual([]);
    expect(m.objects.map((o) => o.outs.length)).toEqual(plain.objects.map((o) => o.outs.length));
    expect(m.objects.flatMap((o) => [...o.ins, ...o.outs]).some((r) => r.mark)).toBe(false);
  });

  it("puts each HTTP secret's and binding's upstream on the right, before the egress, its key at the node", () => {
    const m = withKeys(reachModel(bound, dep.reachability, pods), bound, { catalog: cat, secrets: list, usage });
    expect(of(m, 'service/expense-service').outs.map(line)).toEqual([
      'service:approval-service', 'addon:nats [container]', 'addon:postgres [identity]',
      'upstream:api.vendor.example [offloaded]', 'upstream:api.mail.example [offloaded]', 'internet:Internet']);
    const [erp, mail] = of(m, 'service/expense-service').outs.filter((r) => r.peer.type === 'upstream');
    expect(erp).toMatchObject({ key: 'secret:erp', what: 'OAuth2 at the node', via: ['HTTP secret erp'],
      peer: { sub: 'erp · as api.vendor.example' }, binding: { source: 'http', upstream: 'https://api.vendor.example', mode: 'intercept', pinned: 2 },
      usage: { requests: 1204, refused: 2, failing: 0, mine: 0, by: 'expense-service' } });
    expect(mail).toMatchObject({ key: 'binding:mail', what: 'Headers at the node', via: ['binding mail'], peer: { sub: 'mail · as http://mail' },
      binding: { source: 'binding' }, usage: { requests: 0, mine: 0 } });
    expect(of(m, 'job/migrate').outs.map(line)).toEqual([
      'addon:postgres [identity]', 'upstream:api.vendor.example [offloaded]', 'internet:Internet']);
    expect(m.marks).toEqual(['identity', 'offloaded', 'container']);
  });

  it('gives a sandbox the HTTP secrets it names, with its own requests', () => {
    const own = [...usage, { name: 'models', requests: 9, refused: 0, failing: 0, users: [{ kind: 'sandbox', name: 'c3kv9mwq2hd', requests: 4 }] }];
    const m = withKeys(reachModel(bound, dep.reachability, pods), bound, { secrets: list, usage: own });
    const sb = of(m, 'sandbox/c3kv9mwq2hd').outs;
    expect(sb.map(line)).toEqual(['upstream:models.example [offloaded]']);
    expect(sb[0]).toMatchObject({ what: 'Basic auth at the node', peer: { sub: 'models · as http://models' }, via: ['HTTP secret models'],
      usage: { requests: 9, mine: 4, by: 'c3kv9mwq2hd' } });
    expect(of(m, 'sandbox/other').outs.map(line)).toEqual(['internet:Internet']);
  });

  it('says no numbers without usage, and the name when its upstream is unknown', () => {
    const r = upstreamRow(httpOf('gone', null));
    expect(r).toMatchObject({ peer: { type: 'upstream', name: 'gone', sub: 'gone · as http://gone' }, what: 'Key at the node', usage: null });
    expect(upstreamRow(bindingOf(bound, 'mail'), { requests: 3 }, { kind: 'service', name: 'x' }).usage)
      .toEqual({ requests: 3, refused: 0, failing: 0, mine: null, by: 'x' });
  });

  it('names the kinds of a key', () => {
    expect(['headers', 'basic', 'oauth2', 'clientCertificate', ''].map(keyAtNode)).toEqual([
      'Headers at the node', 'Basic auth at the node', 'OAuth2 at the node', 'Client certificate at the node', 'Key at the node']);
  });
});
