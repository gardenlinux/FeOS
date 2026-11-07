// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { ENV_TABS, ENV_TAB_LABELS, TENANT_VIEWS, canonicalPath, envPath, parsePath } from './routes.js';
import { fmtAgo, fmtDurNs, fmtMsTime, fmtNs, fmtSeconds, prettyJson } from './format.js';
import { ADMIN_KEY, readItem, writeItem } from './storage.js';

describe('parsePath', () => {
  it('maps the routes', () => {
    expect(parsePath('/')).toEqual({ view: 'tenants' });
    expect(parsePath('/tenants')).toEqual({ view: 'tenants' });
    expect(parsePath('/tenants/acme')).toEqual({ view: 'overview', tenant: 'acme' });
    expect(parsePath('/tenants/acme/tools')).toEqual({ view: 'tools', tenant: 'acme' });
    expect(parsePath('/tenants/acme/sandboxes')).toEqual({ view: 'sandboxes', tenant: 'acme', sel: '', tab: 'overview' });
    expect(parsePath('/tenants/acme/sandboxes/0199f0b7-1c2d-7e3f-8a00-00000000e2e0/network'))
      .toEqual({ view: 'sandboxes', tenant: 'acme', sel: '0199f0b7-1c2d-7e3f-8a00-00000000e2e0', tab: 'network' });
    expect(parsePath('/nodes')).toEqual({ view: 'nodes' });
    expect(parsePath('/nodes/node-a')).toEqual({ view: 'node', node: 'node-a', tab: 'overview' });
    expect(parsePath('/nodes/node-a/certs')).toEqual({ view: 'node', node: 'node-a', tab: 'certs' });
    expect(parsePath('/join-tokens')).toEqual({ view: 'joinTokens' });
    expect(parsePath('/instances')).toEqual({ view: 'instances' });
    expect(parsePath('/invite/abc_DEF-1')).toEqual({ view: 'invite', token: 'abc_DEF-1' });
    expect(parsePath('/tenants/acme/members')).toEqual({ view: 'members', tenant: 'acme' });
    expect(parsePath('/tenants/acme/tokens')).toEqual({ view: 'tokens', tenant: 'acme' });
    expect(parsePath('/tenants/acme/credentials')).toEqual({ view: 'credentials', tenant: 'acme', sel: '', tab: '' });
    expect(parsePath('/tenants/acme/credentials/github')).toEqual({ view: 'credentials', tenant: 'acme', sel: 'github', tab: '' });
    expect(parsePath('/tenants/acme/credentials/github/edit'))
      .toEqual({ view: 'credentials', tenant: 'acme', sel: 'github', tab: 'edit' });
    expect(parsePath('/tenants/acme/new-credential')).toEqual({ view: 'newCredential', tenant: 'acme' });
    expect(parsePath('/no/such/page')).toEqual({ view: 'tenants' });
  });

  it('decodes segments and survives a bad escape', () => {
    expect(parsePath('/tenants/a%2Eb').tenant).toBe('a.b');
    expect(parsePath('/tenants/%E0%A4%A').tenant).toBe('%E0%A4%A');
  });

  it('the tenant views are those that act in a tenant', () => {
    expect([...TENANT_VIEWS].sort()).toEqual([
      'environments',
      'members',
      'newEnvironment',
      'overview',
      'sandboxes',
      'tokens',
      'tools',
    ]);
  });
});

describe('the environments routes', () => {
  const env = (rest) => ({ view: 'environments', tenant: 'fecode', env: '', tab: 'overview', item: '', replica: null, search: '', ...rest });

  it('maps the list, an environment and its tabs', () => {
    expect(parsePath('/tenants/fecode/environments')).toEqual(env({}));
    expect(parsePath('/tenants/fecode/environments/')).toEqual(env({}));
    expect(parsePath('/tenants/fecode/environments/expensa-stage')).toEqual(env({ env: 'expensa-stage' }));
    expect(parsePath('/tenants/fecode/environments/expensa-stage/overview')).toEqual(env({ env: 'expensa-stage' }));
    expect(parsePath('/tenants/fecode/environments/expensa-stage/deployments'))
      .toEqual(env({ env: 'expensa-stage', tab: 'deployments' }));
    expect(parsePath('/tenants/fecode/environments/expensa-stage/incidents'))
      .toEqual(env({ env: 'expensa-stage', tab: 'incidents' }));
    expect(parsePath('/tenants/fecode/environments/expensa-stage/logs'))
      .toEqual(env({ env: 'expensa-stage', tab: 'logs' }));
  });

  it("maps a tab's item: a deployment, an incident, an object's logs with its replica", () => {
    expect(parsePath('/tenants/fecode/environments/expensa-stage/deployments/7'))
      .toEqual(env({ env: 'expensa-stage', tab: 'deployments', item: '7' }));
    // The tab's earlier name (ADR 0018, "The deployments' word") redirects.
    expect(parsePath('/tenants/fecode/environments/expensa-stage/releases/7'))
      .toEqual(env({ env: 'expensa-stage', tab: 'deployments', item: '7' }));
    expect(canonicalPath('/tenants/fecode/environments/expensa-stage/releases/7'))
      .toBe('/tenants/fecode/environments/expensa-stage/deployments/7');
    expect(canonicalPath('/tenants/fecode/environments/expensa-stage/releases'))
      .toBe('/tenants/fecode/environments/expensa-stage/deployments');
    expect(canonicalPath('/tenants/releases/environments')).toBe('/tenants/releases/environments');
    expect(parsePath('/tenants/fecode/environments/expensa-stage/incidents/12'))
      .toEqual(env({ env: 'expensa-stage', tab: 'incidents', item: '12' }));
    expect(parsePath('/tenants/fecode/environments/expensa-stage/logs/service/expense-service'))
      .toEqual(env({ env: 'expensa-stage', tab: 'logs', item: 'service/expense-service' }));
    expect(parsePath('/tenants/fecode/environments/expensa-stage/logs/service/expense-service/1'))
      .toEqual(env({ env: 'expensa-stage', tab: 'logs', item: 'service/expense-service', replica: 1 }));
  });

  it('reads an unknown tab as the overview, half an object as none, a bad replica as none', () => {
    expect(parsePath('/tenants/fecode/environments/expensa-stage/settingz')).toEqual(env({ env: 'expensa-stage' }));
    expect(parsePath('/tenants/fecode/environments/expensa-stage/logs/service'))
      .toEqual(env({ env: 'expensa-stage', tab: 'logs' }));
    expect(parsePath('/tenants/fecode/environments/expensa-stage/logs/addon/postgres/x'))
      .toEqual(env({ env: 'expensa-stage', tab: 'logs', item: 'addon/postgres' }));
  });

  it('envPath builds what parsePath reads', () => {
    expect(envPath('fecode')).toBe('/tenants/fecode/environments');
    expect(envPath('fecode', 'expensa-stage')).toBe('/tenants/fecode/environments/expensa-stage');
    expect(envPath('fecode', 'expensa-stage', 'overview', 'x')).toBe('/tenants/fecode/environments/expensa-stage');
    expect(envPath('fecode', 'expensa-stage', 'deployments', 7)).toBe('/tenants/fecode/environments/expensa-stage/deployments/7');
    expect(envPath('fecode', 'expensa-stage', 'logs', 'service/expense-service'))
      .toBe('/tenants/fecode/environments/expensa-stage/logs/service/expense-service');
    expect(envPath('fecode', 'expensa-stage', 'logs', 'service/expense-service', 0))
      .toBe('/tenants/fecode/environments/expensa-stage/logs/service/expense-service/0');
    expect(envPath('fecode', 'e', 'logs', '', null, '?q=timeout&level=error'))
      .toBe('/tenants/fecode/environments/e/logs?q=timeout&level=error');
    expect(envPath('fecode', 'e', 'logs', 'service/web', 1, '?source=ingress'))
      .toBe('/tenants/fecode/environments/e/logs/service/web/1?source=ingress');
    expect(envPath('fecode', 'e', 'overview', '', null, '?q=x')).toBe('/tenants/fecode/environments/e');
    expect(envPath('a b', 'e', 'logs', 'service/web', 2)).toBe('/tenants/a%20b/environments/e/logs/service/web/2');
    expect(envPath('fecode', 'e', 'deployments', '', 3)).toBe('/tenants/fecode/environments/e/deployments');
    for (const [tab, item, replica, search] of [['overview', '', null, ''], ['deployments', '', null, ''],
      ['deployments', '6', null, ''], ['incidents', '12', null, ''], ['logs', '', null, ''], ['logs', 'job/migrate', null, ''],
      ['logs', 'service/web', 0, ''], ['logs', 'service/web', 3, '?q=a+b'], ['logs', '', null, '?trace=x'],
      ['objects', 'service/web', null, ''], ['objects', 'addon/postgres', null, ''], ['objects', '', null, '']]) {
      expect(parsePath(envPath('a/b', 'expensa-stage', tab, item, replica, search)))
        .toEqual({ view: 'environments', tenant: 'a/b', env: 'expensa-stage', tab, item, replica, search });
    }
    expect(ENV_TABS).toEqual([
      'overview', 'builder', 'deployments', 'incidents', 'logs', 'traces', 'traffic', 'secrets', 'access', 'settings', 'objects',
    ]);
    // An object's page: its kind and name; a replica or a search is not its.
    expect(envPath('fecode', 'e', 'objects', 'service/web api')).toBe('/tenants/fecode/environments/e/objects/service/web%20api');
    expect(parsePath('/tenants/fecode/environments/e/objects/job/migrate/2?q=x'))
      .toEqual(env({ env: 'e', tab: 'objects', item: 'job/migrate' }));
    expect(parsePath('/tenants/fecode/environments/e/objects/job')).toEqual(env({ env: 'e', tab: 'objects' }));
    for (const tab of ENV_TABS) expect(ENV_TAB_LABELS[tab]).toBeTruthy();
    expect(ENV_TAB_LABELS.builder).toBe('New deployment');
    for (const tab of ['traffic', 'secrets', 'access', 'settings']) {
      expect(parsePath(`/tenants/fecode/environments/e/${tab}`)).toEqual(env({ env: 'e', tab }));
      expect(envPath('fecode', 'e', tab)).toBe(`/tenants/fecode/environments/e/${tab}`);
    }
    // The builder's other name (edit) leads to the builder; an item on a tab without items is dropped.
    expect(parsePath('/tenants/fecode/environments/e/edit')).toEqual(env({ env: 'e', tab: 'builder' }));
    expect(parsePath('/tenants/fecode/environments/e/secrets/x')).toEqual(env({ env: 'e', tab: 'secrets' }));
    expect(parsePath('/tenants/fecode/environments/e/builder')).toEqual(env({ env: 'e', tab: 'builder' }));
    expect(envPath('fecode', 'e', 'builder')).toBe('/tenants/fecode/environments/e/builder');
    // The builder opens an object's form: …/builder/<kind>/<name>, and the edit link of old.
    expect(parsePath('/tenants/fecode/environments/e/builder/service/api')).toEqual(env({ env: 'e', tab: 'builder', item: 'service/api' }));
    expect(parsePath('/tenants/fecode/environments/e/edit/addon/db')).toEqual(env({ env: 'e', tab: 'builder', item: 'addon/db' }));
    expect(parsePath('/tenants/fecode/environments/e/builder/service')).toEqual(env({ env: 'e', tab: 'builder' }));
    expect(envPath('fecode', 'e', 'builder', 'secret/registry-ghcr.io')).toBe('/tenants/fecode/environments/e/builder/secret/registry-ghcr.io');
    expect(envPath('fecode', 'e', 'builder', 'route/a b')).toBe('/tenants/fecode/environments/e/builder/route/a%20b');
    expect(parsePath('/tenants/fecode/new-environment')).toEqual({ view: 'newEnvironment', tenant: 'fecode' });
    expect(parsePath('/tenants/fecode/environments/e/traces/4bf92f3577b34da6a3ce929d0e0e4736'))
      .toEqual(env({ env: 'e', tab: 'traces', item: '4bf92f3577b34da6a3ce929d0e0e4736' }));
    expect(parsePath('/tenants/fecode/environments/e/traces?service=api&since=1d'))
      .toEqual(env({ env: 'e', tab: 'traces', search: '?service=api&since=1d' }));
    expect(envPath('fecode', 'e', 'traces', '', null, '?service=api')).toBe('/tenants/fecode/environments/e/traces?service=api');
  });
});

describe('format', () => {
  it('relative times, past and ahead', () => {
    const now = Date.parse('2026-10-16T12:00:00Z');
    expect(fmtAgo('2026-10-16T11:59:58Z', now)).toBe('just now');
    expect(fmtAgo('2026-10-16T11:59:00Z', now)).toBe('1m ago');
    expect(fmtAgo('2026-10-16T09:00:00Z', now)).toBe('3h ago');
    expect(fmtAgo('2026-10-23T12:00:00Z', now)).toBe('in 7d');
    expect(fmtAgo('', now)).toBe('—');
  });

  it('times without a locale', () => {
    const ms = new Date(2026, 9, 16, 9, 5, 7, 42).getTime();
    expect(fmtMsTime(ms)).toBe('09:05:07.042');
    expect(fmtNs(String(ms * 1e6))).toBe('09:05:07.042');
    expect(fmtNs('0')).toBe('—');
  });

  it('durations', () => {
    expect(fmtDurNs('1500000000')).toBe('1.5 s');
    expect(fmtDurNs(250e6)).toBe('250 ms');
    expect(fmtDurNs(undefined)).toBe('');
    expect(fmtSeconds(3600)).toBe('1 h');
    expect(fmtSeconds(600)).toBe('10 min');
  });

  it('pretty-prints JSON text and objects', () => {
    expect(prettyJson('{"a":1}')).toBe('{\n  "a": 1\n}');
    expect(prettyJson({ a: 1 })).toBe('{\n  "a": 1\n}');
    expect(prettyJson('not json')).toBe('not json');
  });
});

describe('storage', () => {
  it('keeps a value in memory where storage refuses', () => {
    // No sessionStorage in this environment: the value lives in memory.
    writeItem('sessionStorage', ADMIN_KEY, 'ops:pw');
    expect(readItem('sessionStorage', ADMIN_KEY)).toBe('ops:pw');
    writeItem('sessionStorage', ADMIN_KEY, '');
    expect(readItem('sessionStorage', ADMIN_KEY)).toBe('');
  });
});
