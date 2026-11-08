// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { response } from '../testing/ferun.js';
import { removeObject, setObject } from './draft.js';
import {
  addonValue, changeMarks, credentialBinding, healthOf, idleOf, idleValue, itemOfUnit, listGroups, readinessOf, renameVar,
  retypeHealth, retypeValue, routed, serviceValue, shortImage, summaryOf, textOf, toggleBinding, unitOfItem, valueType, withText,
} from './fields.js';

// The forms' typed fields against the spec's values, and the list's
// rows, on ferund's fixtures: deployment 7 of expensa-stage.

const STORED = response('GET_environments_env_deployments_n.200').spec;
const TARGETS = {
  addons: [{ name: 'postgres', type: 'postgres', keys: ['host', 'url'], fields: ['role', 'database'] },
    { name: 'nats', type: 'nats', keys: ['url'], fields: [] }],
  services: [{ name: 'web', ports: ['http', 'admin'] }, { name: 'api', ports: [8080] }],
  secrets: ['internal-token', 'stripe-key'],
};

describe('the list', () => {
  it('groups the objects by kind, each with its line', () => {
    const groups = listGroups(STORED, STORED);
    expect(groups.map((g) => [g.title, g.rows.length])).toEqual([
      ['Services', 3], ['Jobs', 1], ['Addons', 2], ['Routes', 3], ['Bindings', 0], ['Secrets', 1]]);
    // Security groups are gone (ADR 0034, revised): references open the ports.
    expect(groups.some((g) => g.key === 'securityGroups')).toBe(false);
    expect(groups[0].rows.map((r) => r.name)).toEqual(['approval-service', 'expense-service', 'web']);
    expect(groups.flatMap((g) => g.rows).every((r) => r.mark === '' && !r.removed)).toBe(true);
    expect(summaryOf('services', STORED.services['expense-service'])).toBe('expense:1.4 · 2 replicas');
    expect(summaryOf('services', STORED.services.web)).toBe('expensa-web:1.4');
    expect(summaryOf('services', { image: 'ghcr.io/a/b@sha256:' + 'ab12cd3'.padEnd(64, '0') })).toBe('b@ab12cd3');
    expect(summaryOf('services', {})).toBe('no image yet');
    expect(summaryOf('services', { runtime: 'attached', sandbox: 'h3k9' })).toBe('attached · h3k9');
    expect(summaryOf('jobs', STORED.jobs.migrate)).toBe('expense:1.4 · each deployment');
    expect(summaryOf('jobs', { image: 'x:1', on: 'manual' })).toBe('x:1 · manual');
    expect(summaryOf('addons', STORED.addons.postgres)).toBe('postgres · 17 · small');
    expect(summaryOf('routes', STORED.routes['api-expense'])).toBe('/api/expense → expense-service:http');
    expect(summaryOf('bindings', { tenantCredential: { name: 'stripe', version: 2 } })).toBe('credential stripe · v2');
    expect(summaryOf('bindings', { tenantCredential: { name: 'stripe' } })).toBe('credential stripe');
    expect(summaryOf('bindings', { upstream: 'https://api.github.com' })).toBe('api.github.com');
    expect(summaryOf('secrets', null, { pinned: 3, latest: 3 })).toBe('v3 of 3');
    expect(summaryOf('secrets', null, { pinned: 2, latest: 3, newer: true })).toBe('v2 · v3 newer');
    expect(summaryOf('secrets', null, { pinned: 0, latest: 0, missing: true })).toBe('no value yet');
    expect(summaryOf('secrets', null, { pinned: 0, latest: 4 })).toBe('latest');
    expect(shortImage({ image: 'nginx' })).toBe('nginx');
  });

  it('marks what the draft adds, changes and removes against its base, and keeps a row for what it removes', () => {
    let draft = setObject(STORED, 'services', 'web', { ...STORED.services.web, replicas: 3 });
    draft = setObject(draft, 'addons', 'cache', { type: 'valkey' });
    draft = removeObject(draft, 'jobs', 'migrate');
    draft = removeObject(draft, 'secrets', 'internal-token');
    const marks = changeMarks(STORED, draft);
    expect([...marks]).toEqual([['addons/cache', 'add'], ['jobs/migrate', 'remove'], ['secrets/internal-token', 'remove'],
      ['services/web', 'update']]);
    const rows = Object.fromEntries(listGroups(draft, STORED, marks).flatMap((g) => g.rows).map((r) => [r.unit, r]));
    expect(rows['services/web']).toMatchObject({ mark: 'update', removed: false });
    expect(rows['addons/cache']).toMatchObject({ mark: 'add', removed: false });
    expect(rows['jobs/migrate']).toMatchObject({ mark: 'remove', removed: true });
    // A secret the services still reference stays a row; only its pin went.
    expect(rows['secrets/internal-token']).toMatchObject({ mark: 'remove', removed: false });
    expect(rows['services/approval-service'].mark).toBe('');
  });

  it('reads the object an address names, and back', () => {
    expect(unitOfItem('service/api')).toBe('services/api');
    expect(unitOfItem('secret/registry-ghcr.io')).toBe('secrets/registry-ghcr.io');
    expect(unitOfItem('volume/x')).toBe('');
    expect(unitOfItem('new/binding')).toBe('bindings/');
    expect(itemOfUnit('bindings/')).toBe('new/binding');
    expect(unitOfItem('service/')).toBe('');
    expect(unitOfItem('')).toBe('');
    expect(itemOfUnit('addons/db')).toBe('addon/db');
    expect(itemOfUnit('bindings/')).toBe('new/binding');
    expect(itemOfUnit('')).toBe('');
    for (const u of ['services/web', 'jobs/migrate', 'addons/db', 'routes/root', 'bindings/stripe', 'secrets/a.b']) {
      expect(unitOfItem(itemOfUnit(u))).toBe(u);
    }
  });
});

describe('a variable\'s value', () => {
  it('is one of four choices, whatever form the spec writes it in', () => {
    expect(valueType('info')).toBe('text');
    expect(valueType({ value: 'info' })).toBe('text');
    expect(valueType({ secret: 'k' })).toBe('secret');
    expect(valueType({ addon: 'postgres', key: 'url', role: 'r', database: 'd' })).toBe('addon');
    expect(valueType({ service: 'web', port: 'http', key: 'url' })).toBe('service');
    expect(valueType({ addon: 'a', service: 'b' })).toBe('other');
    expect(valueType(3)).toBe('other');
  });

  it('keeps a text\'s form: a string stays a string, {value} stays {value}', () => {
    expect(textOf('a')).toBe('a');
    expect(textOf({ value: 'b' })).toBe('b');
    expect(textOf({ secret: 'x' })).toBe('');
    expect(withText('a', 'c')).toBe('c');
    expect(withText({ value: 'b' }, 'c')).toEqual({ value: 'c' });
  });

  it('starts another choice with its first target, the url of an addon or a service', () => {
    expect(retypeValue('x', 'secret', TARGETS)).toEqual({ secret: 'internal-token' });
    expect(retypeValue('x', 'addon', TARGETS)).toEqual({ addon: 'postgres', key: 'url' });
    expect(retypeValue('x', 'service', TARGETS)).toEqual({ service: 'web', port: 'http', key: 'url' });
    expect(retypeValue({ secret: 'k' }, 'text', TARGETS)).toBe('');
    expect(retypeValue('keep', 'text', TARGETS)).toBe('keep');
    expect(retypeValue({ value: 'v' }, 'text', TARGETS)).toEqual({ value: 'v' });
    expect(retypeValue('x', 'addon', { addons: [], services: [], secrets: [] })).toEqual({ addon: '' });
    expect(retypeValue('x', 'service', { addons: [], services: [], secrets: [] })).toEqual({ service: '', key: 'url' });
  });

  it('writes an addon output with its key and fields, dropping what another addon lacks', () => {
    const v = { addon: 'postgres', key: 'url', role: 'r', database: 'd' };
    expect(addonValue(v, { role: '' }, TARGETS.addons[0])).toEqual({ addon: 'postgres', key: 'url', database: 'd' });
    expect(addonValue(v, { key: 'host' }, TARGETS.addons[0])).toEqual({ ...v, key: 'host' });
    expect(addonValue(v, { addon: 'nats' }, TARGETS.addons[1])).toEqual({ addon: 'nats', key: 'url' });
    expect(addonValue({ addon: 'nats', key: 'url' }, { addon: 'postgres' }, TARGETS.addons[0])).toEqual({ addon: 'postgres', key: 'url' });
  });

  it('writes a service address with its port and key, the first port of another service', () => {
    const v = { service: 'web', port: 'admin', key: 'host' };
    expect(serviceValue(v, { key: 'url' }, TARGETS.services[0])).toEqual({ ...v, key: 'url' });
    expect(serviceValue(v, { service: 'api' }, TARGETS.services[1])).toEqual({ service: 'api', port: 8080, key: 'host' });
    expect(serviceValue({ service: 'api', port: 8080 }, { service: 'web' }, TARGETS.services[0])).toEqual({ service: 'web', port: 'http' });
    expect(serviceValue(v, { port: '' }, TARGETS.services[0])).toEqual({ service: 'web', key: 'host' });
  });

  it('renames a variable in its place, not onto another', () => {
    expect(Object.keys(renameVar({ A: 1, B: 2, C: 3 }, 'B', 'X'))).toEqual(['A', 'X', 'C']);
    const env = { A: 1, B: 2 };
    expect(renameVar(env, 'A', 'B')).toBe(env);
    expect(renameVar(env, 'A', '')).toBe(env);
  });
});

describe('a health check', () => {
  it('reads a readiness and writes it back as it was', () => {
    for (const r of [
      STORED.services.web.readiness,
      { tcp: { port: 9000 } },
      { exec: { command: ['pg_isready', '-q'] }, period: '5s' },
      { http: { port: 'http', path: '/' }, failureThreshold: 5 },
    ]) expect(readinessOf(healthOf(r))).toEqual(r);
    expect(healthOf(undefined).kind).toBe('none');
    expect(readinessOf(healthOf(undefined))).toBeUndefined();
    expect(healthOf(STORED.services.web.readiness)).toMatchObject({ kind: 'http', port: 'http', path: '/readyz', period: '2s', failureThreshold: 3 });
  });

  it('switches its kind with the service\'s first port and keeps its timing', () => {
    const h = healthOf({ http: { port: 'http', path: '/readyz' }, period: '5s' });
    expect(readinessOf(retypeHealth(h, 'tcp', ['http']))).toEqual({ tcp: { port: 'http' }, period: '5s' });
    expect(readinessOf(retypeHealth(healthOf(undefined), 'http', ['grpc']))).toEqual({ http: { port: 'grpc', path: '/' } });
    expect(readinessOf(retypeHealth(healthOf(undefined), 'exec', []))).toEqual({ exec: { command: [] } });
    expect(readinessOf(retypeHealth(h, 'none'))).toBeUndefined();
  });
});

describe('idle', () => {
  it('is never, or after whole minutes; another duration stays as written', () => {
    expect(idleOf('')).toMatchObject({ mode: 'never' });
    expect(idleOf('never')).toMatchObject({ mode: 'never' });
    expect(idleOf('15m')).toMatchObject({ mode: 'after', minutes: 15 });
    expect(idleOf('1h30m')).toMatchObject({ mode: 'after', minutes: 90 });
    expect(idleOf('90s')).toMatchObject({ mode: 'custom', raw: '90s' });
    expect(idleValue('never', 15)).toBe('');
    expect(idleValue('after', 20)).toBe('20m');
    expect(idleValue('after', 0)).toBe('1m');
    expect(routed(STORED, 'web')).toBe(true);
    expect(routed(STORED, 'nobody')).toBe(false);
  });
});

describe('bindings', () => {
  it('adds one of a tenant credential, ticked on the service, once', () => {
    const r = credentialBinding(STORED, 'stripe', 'services', 'web');
    expect(r.name).toBe('stripe');
    expect(r.spec.bindings).toEqual({ stripe: { tenantCredential: { name: 'stripe' } } });
    expect(r.spec.services.web.bindings).toEqual(['stripe']);
    const again = credentialBinding(r.spec, 'stripe', 'jobs', 'migrate');
    expect(Object.keys(again.spec.bindings)).toEqual(['stripe']);
    expect(again.spec.jobs.migrate.bindings).toEqual(['stripe']);
    // A binding of that name stated otherwise keeps its name; the new one takes the next.
    const taken = setObject(STORED, 'bindings', 'stripe', { upstream: 'https://api.stripe.com' });
    expect(credentialBinding(taken, 'stripe').name).toBe('stripe-2');
  });

  it('ticks and unticks, dropping an empty list', () => {
    const on = toggleBinding(STORED, 'services', 'web', 'github', true);
    expect(on.services.web.bindings).toEqual(['github']);
    expect(toggleBinding(on, 'services', 'web', 'github', true)).toBe(on);
    expect('bindings' in toggleBinding(on, 'services', 'web', 'github', false).services.web).toBe(false);
    expect(toggleBinding(STORED, 'services', 'nobody', 'x', true)).toBe(STORED);
  });
});
