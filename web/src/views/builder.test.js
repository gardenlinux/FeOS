// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest';
import { fixture, response } from '../testing/ferun.js';
import {
  addService, cloneSpec, decodeDraft, draftKey, encodeDraft, imageBaseName, isDnsLabel, isSecretName, objectsOf,
  orderSpec, parseSpecText, proposeMessage, registryHost, removeObject, renameObject, setObject, specText, uniqueName,
} from './draft.js';
import {
  addVariable, envValueKind, makeRef, refLine, refTargets, referencedSecrets, referencesOf, referencesTo, refsFrom,
  removeWithRefs, secretNames, suggestVarName,
} from './refs.js';
import { atField, fieldOf, groupFindings, notAt, severityCounts, splitPath, worst } from './findings.js';
import { canonical, changesSince, rebase, resolve, units } from './rebase.js';
import { applyPins, checkNewer, digestOf, imageView, isDigestRef, pinSecret, secretPins, setImage, unpinned } from './pins.js';
import { builtinCatalog, newAddon, planText, readCatalog } from './catalog.js';
import { planner } from './planner.js';

// The builder's arithmetic on ferund's fixtures: deployment 7 of
// expensa-stage is a normalised spec (images pinned, defaults filled,
// the secret pinned), the plan's request a spec as a client writes it.

const STORED = response('GET_environments_env_deployments_n.200').spec;
const WRITTEN = fixture('POST_environments_env_deployments.422').request.spec;
const PLAN = response('POST_environments_env_plan.200');
const INVALID = response('POST_environments_env_deployments.422');

describe('draft', () => {
  it('writes the text in the API order, whatever order the object was built in', () => {
    const s = { routes: { root: { port: 'http', service: 'web', path: '/' } }, services: { web: { ports: [], image: 'x' } } };
    const o = orderSpec(s);
    expect(Object.keys(o)).toEqual(['services', 'routes']);
    expect(Object.keys(o.routes.root)).toEqual(['path', 'service', 'port']);
    expect(Object.keys(o.services.web)).toEqual(['image', 'ports']);
    // A stored spec reads back unchanged.
    expect(JSON.parse(specText(STORED))).toEqual(STORED);
    expect(specText(STORED).split('\n')[1]).toBe('  "services": {');
  });

  it('parses the text, a stored deployment and an export', () => {
    expect(parseSpecText('{"services":{}}')).toEqual({ spec: { services: {} } });
    expect(parseSpecText('[1]').error).toBe('the spec is a JSON object');
    expect(parseSpecText('{"services": ').error).toMatch(/^not JSON/);
    const dep = response('GET_environments_env_deployments_n.200');
    expect(parseSpecText(JSON.stringify(dep)).spec).toEqual(STORED);
  });

  it('sets, removes and lists objects without touching its argument', () => {
    const before = cloneSpec(STORED);
    const s = setObject(STORED, 'addons', 'cache', { type: 'valkey' });
    expect(objectsOf(s, 'addons').map(([n]) => n)).toEqual(['cache', 'nats', 'postgres']);
    expect(STORED).toEqual(before);
    const r = removeObject(removeObject(s, 'jobs', 'migrate'), 'jobs', 'none');
    expect(r.jobs).toBeUndefined();
  });

  it('renames an object and the references to it', () => {
    const s = renameObject(STORED, 'services', 'approval-service', 'approval');
    expect(Object.keys(s.services)).toEqual(['approval', 'expense-service', 'web']);
    expect(s.routes['api-approval'].service).toBe('approval');
    expect(s.services['expense-service'].env.EXPENSA_APPROVAL_URL).toEqual({ service: 'approval', port: 'http', key: 'url' });
    const a = renameObject(STORED, 'addons', 'postgres', 'db');
    expect(a.services['approval-service'].env.APPROVAL_DATABASE_URL.addon).toBe('db');
    expect(a.jobs.migrate.env.EXPENSA_DATABASE_URL.addon).toBe('db');
    expect(a.services['expense-service'].env.EXPENSA_NATS_URL.addon).toBe('nats');
  });

  it('names new objects and makes a first service with its route', () => {
    expect(uniqueName(STORED, 'services', 'web')).toBe('web-2');
    expect(uniqueName(STORED, 'services', 'postgres')).toBe('postgres-2');
    expect(uniqueName({}, 'addons', 'Postgres')).toBe('postgres');
    const { spec, name } = addService({}, 'nginx:1.27');
    expect(name).toBe('nginx');
    expect(spec).toEqual({
      services: { nginx: { image: 'nginx:1.27', ports: [{ name: 'http', port: 8080 }] } },
      routes: { root: { path: '/', service: 'nginx', port: 'http' } },
    });
    expect(addService(spec).spec.routes).toEqual(spec.routes);
    expect(imageBaseName('ghcr.io/acme/web@sha256:' + 'a'.repeat(64))).toBe('web');
    expect(registryHost('ghcr.io/acme/web:1')).toBe('ghcr.io');
    expect(registryHost('nginx')).toBe('docker.io');
    expect(registryHost('localhost:5000/x')).toBe('localhost:5000');
  });

  it('checks names only as far as a field needs', () => {
    expect(isDnsLabel('web-2')).toBe(true);
    expect(isDnsLabel('Web')).toBe(false);
    expect(isDnsLabel('-a')).toBe(false);
    expect(isSecretName('registry-ghcr.io')).toBe(true);
    expect(isSecretName('addon.db.password')).toBe(false);
  });

  it('proposes the message from the diff, images as written', () => {
    const spec = setObject(STORED, 'routes', 'api', { path: '/api', service: 'web', port: 'http' });
    const diff = [
      ...PLAN.diff.slice(0, 1),
      { kind: 'route', name: 'api', change: 'add', fields: [], effect: 'in place' },
      { kind: 'addon', name: 'nats', change: 'remove', fields: [], effect: 'deletes data' },
    ];
    expect(proposeMessage(diff, spec)).toBe(
      'expense-service: image ghcr.io/acme/expense:1.3 → ghcr.io/acme/expense:1.4; route /api added; addon nats removed');
    expect(proposeMessage([], spec)).toBe('');
    const pinned = 'ghcr.io/acme/web@sha256:' + 'ab'.repeat(32);
    expect(proposeMessage([{ kind: 'service', name: 'web', change: 'update', fields: [{ path: 'image', from: pinned, to: 'web:2' }] }], spec))
      .toBe('web: image ghcr.io/acme/web@sha256:abababababab… → web:2');
  });

  it('stores a draft without anything but its spec, base, source and name', () => {
    const d = { spec: STORED, base: 7, source: 'deployment 7', name: '', extra: 'x' };
    const back = decodeDraft(encodeDraft(d));
    expect(back).toEqual({ spec: STORED, base: 7, source: 'deployment 7', name: '' });
    expect(decodeDraft('nope')).toBeNull();
    expect(decodeDraft('{"v":2}')).toBeNull();
    expect(draftKey('acme', 'stage')).toBe('ferun-draft:acme/stage');
    expect(draftKey('acme', '')).toBe('ferun-draft:acme/');
  });
});

describe('references', () => {
  it('lists every reference with its line', () => {
    const refs = referencesOf(STORED);
    const lines = refsFrom(STORED, 'services', 'expense-service', refs).map(refLine);
    expect(lines).toEqual([
      'EXPENSA_APPROVAL_URL → approval-service:http · url',
      'EXPENSA_DATABASE_URL → postgres · url (database expensa, role expense_svc)',
      'EXPENSA_INTERNAL_TOKEN → secret internal-token',
      'EXPENSA_NATS_URL → nats · url',
    ]);
    expect(refsFrom(STORED, 'routes', 'api-expense', refs).map(refLine)).toEqual(['/api/expense → expense-service:http']);
    expect(referencesTo(STORED, 'addons', 'postgres', refs).map((r) => `${r.from.key}/${r.from.name}`))
      .toEqual(['services/approval-service', 'services/expense-service', 'jobs/migrate']);
  });

  it('knows the forms of an env value', () => {
    expect(envValueKind('x')).toBe('string');
    expect(envValueKind({ value: 'x' })).toBe('value');
    expect(envValueKind({ secret: 's' })).toBe('secret');
    expect(envValueKind({ addon: 'a', key: 'url', role: 'r' })).toBe('addon');
    expect(envValueKind({ service: 's', port: 'http', key: 'url' })).toBe('service');
    expect(envValueKind(3)).toBe('invalid');
  });

  it('names the secrets, a registry login and a binding credential among them', () => {
    const s = setObject(setObject(STORED, 'services', 'priv', {
      image: 'ghcr.io/acme/priv:1', imageCredentials: { username: 'bot', password: { secret: 'registry-ghcr.io' } },
      bindings: ['gh'],
    }), 'bindings', 'gh', { upstream: 'https://api.github.com', credential: { kind: 'headers', headers: { Authorization: { secret: 'gh-token' } } } });
    expect(referencedSecrets(s)).toEqual(['gh-token', 'internal-token', 'registry-ghcr.io']);
    const lines = refsFrom(s, 'services', 'priv').map(refLine);
    expect(lines).toEqual(['registry login → secret registry-ghcr.io', 'binding gh']);
    expect(refsFrom(s, 'bindings', 'gh').map(refLine)).toEqual(['credential.headers.Authorization → secret gh-token']);
    expect(secretNames({ secrets: { a: {} }, services: { w: { env: { B: { secret: 'b' } } } } })).toEqual(['a', 'b']);
  });

  it('removes an object with the references to it', () => {
    const s = removeWithRefs(STORED, 'services', 'approval-service');
    expect(s.services['approval-service']).toBeUndefined();
    expect(s.routes['api-approval']).toBeUndefined();
    expect(s.services['expense-service'].env.EXPENSA_APPROVAL_URL).toBeUndefined();
    const a = removeWithRefs(STORED, 'addons', 'postgres');
    expect(a.jobs.migrate.env).toEqual({});
    expect(referencesTo(a, 'addons', 'postgres')).toEqual([]);
  });

  it('offers targets and proposes names', () => {
    const t = refTargets(STORED, builtinCatalog(), { self: 'web', knownSecrets: ['other'] });
    expect(t.addons.map((a) => [a.name, a.keys.length, a.fields])).toEqual([['nats', 5, []], ['postgres', 6, ['role', 'database']]]);
    expect(t.services).toEqual([{ name: 'approval-service', ports: ['http'] }, { name: 'expense-service', ports: ['http'] }]);
    expect(t.secrets).toEqual(['internal-token', 'other']);
    expect(suggestVarName({ key: 'addons', name: 'db' }, { type: 'postgres', key: 'url' })).toBe('DATABASE_URL');
    expect(suggestVarName({ key: 'addons', name: 'cache' }, { type: 'valkey', key: 'url' })).toBe('CACHE_URL');
    expect(suggestVarName({ key: 'services', name: 'api-v2' }, { key: 'url' })).toBe('API_V2_URL');
    expect(suggestVarName({ key: 'secrets', name: 'internal-token' })).toBe('INTERNAL_TOKEN');
    expect(suggestVarName({ key: 'addons', name: 'db' }, { suggestion: 'PG_URL' })).toBe('PG_URL');
    expect(makeRef({ key: 'addons', name: 'db' }, { key: 'url', fields: { role: 'r', database: '' } }))
      .toEqual({ addon: 'db', key: 'url', role: 'r' });
    expect(makeRef({ key: 'services', name: 'api' }, { key: 'url', port: 'http' })).toEqual({ service: 'api', port: 'http', key: 'url' });
    expect(makeRef({ key: 'secrets', name: 's' })).toEqual({ secret: 's' });
    const v = addVariable(STORED, 'services', 'web', 'DATABASE_URL', { addon: 'postgres', key: 'url' });
    expect(v.services.web.env).toEqual({ DATABASE_URL: { addon: 'postgres', key: 'url' } });
  });
});

describe('findings', () => {
  it('sit at the object and field their path names', () => {
    expect(splitPath('services.web.ports[1].port', STORED)).toEqual({ key: 'services', name: 'web', rest: 'ports[1].port' });
    expect(splitPath('services.web', STORED)).toEqual({ key: 'services', name: 'web', rest: '' });
    expect(splitPath('', STORED)).toBeNull();
    expect(splitPath('services', STORED)).toBeNull();
    expect(splitPath('volumes.x', STORED)).toBeNull();
    // A secret's name may hold dots: the spec's names decide.
    expect(splitPath('secrets.registry-ghcr.io.version', { secrets: { 'registry-ghcr.io': {} } }))
      .toEqual({ key: 'secrets', name: 'registry-ghcr.io', rest: 'version' });
    expect(splitPath('services.ghost.image', STORED)).toEqual({ key: 'services', name: 'ghost', rest: 'image' });
  });

  it('group by object, the rest above', () => {
    const fs = [...INVALID.findings, { severity: 'info', path: '', problem: 'p', remedy: '' }];
    const g = groupFindings(fs, WRITTEN);
    expect(g.global.map((f) => f.problem)).toEqual(['p']);
    const list = g.objects.get('services/expense-service');
    expect(list.map((f) => f.rest)).toEqual(['env.EXPENSA_DATABASE_URL', 'idle']);
    expect(atField(list, 'env').length).toBe(1);
    expect(atField(list, 'env.EXPENSA_DATABASE_URL').length).toBe(1);
    expect(atField(list, 'env.OTHER').length).toBe(0);
    expect(notAt(list, ['env']).map((f) => f.rest)).toEqual(['idle']);
    expect(fieldOf('ports[1].port')).toBe('ports');
    expect(worst(list)).toBe('error');
    expect(worst([{ severity: 'info' }, { severity: 'warning' }])).toBe('warning');
    expect(worst([])).toBe('');
    expect(severityCounts(fs)).toEqual({ error: 2, warning: 0, info: 1 });
  });
});

describe('rebase', () => {
  const base = STORED;
  it('compares by content, not key order', () => {
    expect(canonical({ b: 1, a: [1, { d: 2, c: 3 }] })).toBe('{"a":[1,{"c":3,"d":2}],"b":1}');
    expect(units({ services: { a: {} }, volumes: [] }).get('volumes')).toEqual([]);
  });

  it('lists the changes of a draft by object', () => {
    const draft = setObject(removeObject(base, 'jobs', 'migrate'), 'addons', 'cache', { type: 'valkey' });
    const d2 = setObject(draft, 'services', 'web', { ...base.services.web, replicas: 2 });
    expect(changesSince(base, d2)).toEqual([
      { unit: 'addons/cache', change: 'add' },
      { unit: 'jobs/migrate', change: 'remove' },
      { unit: 'services/web', change: 'update' },
    ]);
    expect(changesSince(base, orderSpec(cloneSpec(base)))).toEqual([]);
  });

  it('takes the draft changes onto the latest, conflicts where both changed one object', () => {
    // The draft scales web and adds a cache; since its base, someone
    // else changed web's image and removed the job.
    const draft = setObject(setObject(base, 'services', 'web', { ...base.services.web, replicas: 3 }),
      'addons', 'cache', { type: 'valkey' });
    const latest = removeObject(setObject(base, 'services', 'web', { ...base.services.web, image: 'other@sha256:' + 'b'.repeat(64) }),
      'jobs', 'migrate');
    const r = rebase(base, draft, latest);
    expect(r.applied).toEqual(['addons/cache']);
    expect(r.conflicts.map((c) => c.unit)).toEqual(['services/web']);
    expect(r.spec.jobs).toBeUndefined();
    expect(r.spec.addons.cache).toEqual({ type: 'valkey' });
    expect(r.spec.services.web.image).toMatch(/^other@/);
    expect(resolve(r.spec, r.conflicts[0], 'mine').services.web.replicas).toBe(3);
    expect(resolve(r.spec, r.conflicts[0], 'theirs').services.web.replicas).toBe(1);
  });

  it('is no conflict when both made the same change, or a remove meets a remove', () => {
    const draft = removeObject(base, 'jobs', 'migrate');
    const r = rebase(base, draft, removeObject(base, 'jobs', 'migrate'));
    expect(r.conflicts).toEqual([]);
    // Removed here, changed there: a conflict.
    const latest = setObject(base, 'jobs', 'migrate', { ...base.jobs.migrate, retries: 3 });
    const c = rebase(base, draft, latest).conflicts[0];
    expect(c.unit).toBe('jobs/migrate');
    expect(c.mine).toBeUndefined();
    expect(c.theirs.retries).toBe(3);
  });
});

describe('pins', () => {
  const pinned = STORED.services.web.image;
  it('show the image as written with its digest', () => {
    expect(isDigestRef(pinned)).toBe(true);
    expect(digestOf(pinned)).toBe('sha256:de2feb13b0b780dd0dfd7aa32bcd02b306a4680f4958f896b3347f3ba3521a81');
    expect(imageView(STORED.services.web)).toEqual({ written: 'ghcr.io/acme/expensa-web:1.4', digest: digestOf(pinned) });
    expect(imageView({ image: 'nginx' })).toEqual({ written: 'nginx', digest: '' });
  });

  it('keep the digests until the image is changed or checked for a newer one', () => {
    expect(unpinned(STORED)).toEqual([]);
    const edited = setImage(STORED.services.web, 'ghcr.io/acme/expensa-web:1.5');
    expect(edited.imageAsWritten).toBeUndefined();
    expect(checkNewer(STORED.services.web)).toMatchObject({ image: 'ghcr.io/acme/expensa-web:1.4' });
    expect(checkNewer(STORED.services.web).imageAsWritten).toBeUndefined();
    expect(checkNewer({ image: 'nginx' })).toEqual({ image: 'nginx' });
  });

  it('write the plan images into the draft before apply', () => {
    const draft = setObject(setObject(STORED, 'services', 'web', checkNewer(STORED.services.web)),
      'jobs', 'seed', { image: 'ghcr.io/acme/seed:1' });
    expect(unpinned(draft)).toEqual(['ghcr.io/acme/expensa-web:1.4', 'ghcr.io/acme/seed:1']);
    const pin = 'ghcr.io/acme/expensa-web@sha256:' + 'c'.repeat(64);
    const r = applyPins(draft, { 'ghcr.io/acme/expensa-web:1.4': pin });
    expect(r.supported).toBe(true);
    expect(r.spec.services.web).toMatchObject({ image: pin, imageAsWritten: 'ghcr.io/acme/expensa-web:1.4' });
    expect(r.unresolved).toEqual(['ghcr.io/acme/seed:1']);
    // An older ferund answers no images: nothing to write, nothing held.
    expect(applyPins(draft, undefined)).toEqual({ spec: draft, unresolved: [], supported: false });
  });

  it('say when a secret has a newer version than its pin, or none', () => {
    const list = response('GET_environments_env_secrets.200').secrets;
    const s = pinSecret(pinSecret(STORED, 'internal-token', 2), 'new-one', 0);
    expect(secretPins(s, ['internal-token', 'new-one', 'later'], list, new Set(['later']))).toEqual([
      { name: 'internal-token', pinned: 2, latest: 3, newer: true, missing: false },
      { name: 'new-one', pinned: 0, latest: 0, newer: false, missing: true },
      { name: 'later', pinned: 0, latest: 0, newer: false, missing: false },
    ]);
    expect(s.secrets['new-one']).toEqual({});
  });
});

describe('catalog', () => {
  it('reads the blueprints, plans and option fields', () => {
    const c = readCatalog({ addons: [{
      type: 'postgres', versions: ['16', '17'], defaultPlan: 'small',
      plans: { small: { vcpus: 1, memoryMib: 1024, volumeMib: 10240 }, dev: { vcpus: 1, memoryMib: 256, volumeMib: 1024 } },
      outputs: { keys: ['url', 'host'], fields: ['role'] },
      options: [{ name: 'databases', kind: 'list' }, { name: 'mode', kind: 'oneOf', values: ['a', 'b'], default: 'a' },
        { name: 'x', kind: 'map' }],
    }] });
    const pg = c.byType.get('postgres');
    expect(c.builtin).toBe(false);
    expect(pg.plans.map((p) => p.name)).toEqual(['dev', 'small']);
    expect(pg.outputs).toEqual(['url', 'host']);
    expect(pg.fields).toEqual(['role']);
    expect(pg.options.map((o) => o.kind)).toEqual(['list', 'enum', 'json']);
    expect(planText(pg.plans[1])).toBe('small · 1 vCPU, 1024 MiB, volume 10 GiB');
    expect(newAddon(pg)).toEqual({ type: 'postgres', version: '17', plan: 'small' });
    expect(readCatalog({})).toBeNull();
    expect(readCatalog([{ type: 'a', outputKeys: ['url'] }]).byType.get('a').outputs).toEqual(['url']);
  });

  it('has a built-in list for a ferund without one', () => {
    const c = builtinCatalog();
    expect(c.builtin).toBe(true);
    expect(c.types.map((t) => t.type)).toEqual(['nats', 'postgres', 'valkey']);
    expect(planText(c.byType.get('nats').plans[0])).toBe('dev');
  });
});

describe('planner', () => {
  it('plans 500 ms after the last change, one at a time, newest answer only', async () => {
    vi.useFakeTimers();
    const runs = [];
    const results = [];
    const resolvers = [];
    const p = planner({
      run: (input) => { runs.push(input); return new Promise((r) => resolvers.push(() => r(`plan of ${input}`))); },
      onResult: (a) => results.push(a),
    });
    p.change('a');
    vi.advanceTimersByTime(300);
    p.change('b');
    vi.advanceTimersByTime(499);
    await vi.runAllTicks();
    expect(runs).toEqual([]);
    vi.advanceTimersByTime(1);
    await Promise.resolve(); await Promise.resolve();
    expect(runs).toEqual(['b']);
    // A change while b is planned waits for b's answer, which is dropped.
    p.change('c');
    vi.advanceTimersByTime(500);
    await Promise.resolve();
    expect(runs).toEqual(['b']);
    resolvers[0]();
    for (let i = 0; i < 6; i++) await Promise.resolve();
    expect(results).toEqual([]);
    expect(runs).toEqual(['b', 'c']);
    resolvers[1]();
    for (let i = 0; i < 6; i++) await Promise.resolve();
    expect(results).toEqual(['plan of c']);
    p.stop();
    vi.useRealTimers();
  });
});
