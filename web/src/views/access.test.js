// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { accessOf, addonMark, bindingOf, inventoryOf, secretUses } from './access.js';

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
  { name: 'github', version: 4, upstream: 'https://api.github.com', mode: 'name', kind: 'headers' },
  { name: 'old', version: 1, upstream: 'https://old.example', mode: 'name', kind: 'basic' },
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
  it("names a credential's bindings and their users", () => {
    expect(row('credential', 'github')).toMatchObject({ bindings: ['github'], users: [{ key: 'services', name: 'api' }], state: 'set', version: 4 });
    expect(row('credential', 'old')).toMatchObject({ bindings: [], state: 'unused' });
  });
  it('lists the addons that admit by identity or hand out a password', () => {
    expect(row('addon', 'db')).toMatchObject({ marks: ['identity'],
      users: [{ key: 'jobs', name: 'migrate' }, { key: 'services', name: 'api' }] });
    expect(row('addon', 'bus')).toMatchObject({ marks: ['container'], users: [{ key: 'services', name: 'api' }] });
    expect(row('addon', 'spare')).toMatchObject({ marks: ['identity'], users: [], state: 'unused' });
  });
});
