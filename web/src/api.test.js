// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  ApiError, api, auth, basicHeader, forgetTenantTokens, getBasic, labelQuery, loginURL, notAvailable, onSignedOut,
  request, setActiveTenant, setBasic,
} from './api.js';
import { fixture, requestOf, serveFixtures } from './testing/ferun.js';

// The client's contract with ferund: X-Requested-With: ferun and
// same-origin credentials on every request, Basic from this tab's
// sessionStorage, the session in a tenant named by FeRun-Tenant (no
// token in the browser), a 401 ending the sign-in.

class MemoryStorage {
  constructor() { this.m = new Map(); }
  getItem(k) { return this.m.has(k) ? this.m.get(k) : null; }
  setItem(k, v) { this.m.set(k, String(v)); }
  removeItem(k) { this.m.delete(k); }
}

let calls;
let answers;

function answer(status, body) {
  answers.push({ status, body });
}

beforeEach(() => {
  calls = [];
  answers = [];
  vi.stubGlobal('localStorage', new MemoryStorage());
  vi.stubGlobal('sessionStorage', new MemoryStorage());
  vi.stubGlobal('fetch', vi.fn(async (path, init) => {
    calls.push({ path, init });
    const a = answers.shift() || { status: 200, body: {} };
    const text = typeof a.body === 'string' ? a.body : JSON.stringify(a.body);
    return { ok: a.status >= 200 && a.status < 300, status: a.status, text: async () => text };
  }));
  setActiveTenant('');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('request', () => {
  it('sends the CSRF header and same-origin credentials, JSON only with a body', async () => {
    await request('GET', '/api/v1/admin/nodes');
    await request('POST', '/api/v1/admin/tenants', { body: { name: 'acme' } });
    expect(calls[0].init.headers['X-Requested-With']).toBe('ferun');
    expect(calls[0].init.credentials).toBe('same-origin');
    expect(calls[0].init.headers['Content-Type']).toBeUndefined();
    expect(calls[0].init.body).toBeUndefined();
    expect(calls[1].init.headers['X-Requested-With']).toBe('ferun');
    expect(calls[1].init.headers['Content-Type']).toBe('application/json');
    expect(JSON.parse(calls[1].init.body)).toEqual({ name: 'acme' });
  });

  it('throws ferund’s error text with the status', async () => {
    answer(409, { error: 'tenant acme already exists' });
    await expect(request('POST', '/x')).rejects.toMatchObject({ message: 'tenant acme already exists', status: 409 });
    answer(503, 'not available on this instance');
    const e = await request('GET', '/y').catch((x) => x);
    expect(e).toBeInstanceOf(ApiError);
    expect(e.message).toBe('GET /y: HTTP 503');
  });
});

describe('sign-in', () => {
  it('loginURL carries the path to return to', () => {
    expect(loginURL('/tenants/acme/sandboxes?x=1')).toBe('/auth/login?next=%2Ftenants%2Facme%2Fsandboxes%3Fx%3D1');
    expect(loginURL('')).toBe('/auth/login?next=%2F');
    expect(loginURL('/', ' Ann@Example.com ')).toBe('/auth/login?next=%2F&login_hint=Ann%40Example.com');
  });

  it('me is null when signed out', async () => {
    answer(401, { error: 'not signed in' });
    expect(await auth.me()).toBeNull();
    expect(calls[0].path).toBe('/auth/me');
    expect(calls[0].init.headers.Authorization).toBeUndefined();
  });

  it('Basic: checked with /auth/me, then kept for this tab only', async () => {
    answer(200, { username: 'ops', admin: true, method: 'basic' });
    const me = await auth.signInBasic('ops', 'pä:ss');
    expect(me).toEqual({ username: 'ops', admin: true, method: 'basic' });
    expect(calls).toHaveLength(1);
    expect(calls[0].path).toBe('/auth/me');
    expect(calls[0].init.headers.Authorization).toBe(basicHeader('ops:pä:ss'));
    expect(getBasic()).toBe('ops:pä:ss');
    expect(sessionStorage.getItem('ferun-admin')).toBe('ops:pä:ss');
    expect(localStorage.getItem('ferun-admin')).toBeNull();
    // From now on the admin calls carry them.
    await api.listInstances();
    expect(calls[1].init.headers.Authorization).toBe(basicHeader('ops:pä:ss'));
  });

  it('Basic: a refusal keeps nothing', async () => {
    answer(401, { error: 'not signed in' });
    await expect(auth.signInBasic('ops', 'wrong')).rejects.toThrow('wrong user or password');
    expect(getBasic()).toBe('');
  });

  it('Basic: wrong credentials beside a session are still wrong', async () => {
    // /auth/me falls back to the session when Basic fails.
    answer(200, { sub: 's', username: 'someone', admin: true, method: 'oidc' });
    await expect(auth.signInBasic('ops', 'wrong')).rejects.toThrow('wrong user or password');
    expect(getBasic()).toBe('');
  });

  it('basicHeader encodes UTF-8', () => {
    expect(basicHeader('a:b')).toBe('Basic ' + btoa('a:b'));
    expect(basicHeader('ü:x')).toBe('Basic w7w6eA==');
  });

  it('signOut posts with the CSRF header and follows the redirect', async () => {
    setBasic('ops:pw');
    answer(200, { redirect: 'https://id.example/logout?id_token_hint=x' });
    expect(await auth.signOut()).toBe('https://id.example/logout?id_token_hint=x');
    expect(calls[0].path).toBe('/auth/logout');
    expect(calls[0].init.method).toBe('POST');
    expect(calls[0].init.headers['X-Requested-With']).toBe('ferun');
    expect(getBasic()).toBe('');
  });

  it('a 401 on an admin route ends the sign-in', async () => {
    setBasic('ops:pw');
    const heard = vi.fn();
    const off = onSignedOut(heard);
    answer(401, { error: 'admin sign-in required' });
    await expect(api.listNodes()).rejects.toThrow('admin sign-in required');
    expect(heard).toHaveBeenCalledTimes(1);
    expect(getBasic()).toBe('');
    off();
  });

  it('an admin call with a session sends no Authorization', async () => {
    answer(200, { nodes: [] });
    await api.listNodes();
    expect(calls[0].path).toBe('/api/v1/admin/nodes');
    expect(calls[0].init.headers.Authorization).toBeUndefined();
    expect(calls[0].init.credentials).toBe('same-origin');
  });
});

describe('the tenant, with the session', () => {
  it('names the tenant in FeRun-Tenant and sends no token', async () => {
    setActiveTenant('acme');
    answer(200, { sandboxes: [] });
    await api.listSandboxes();
    expect(calls[0].path).toBe('/api/v1/sandboxes');
    expect(calls[0].init.headers['FeRun-Tenant']).toBe('acme');
    expect(calls[0].init.headers.Authorization).toBeUndefined();
    expect(calls[0].init.headers['X-Requested-With']).toBe('ferun');
    expect(calls[0].init.credentials).toBe('same-origin');
  });

  it("reads a sandbox's activity in the tenant, with a source and a page bound (ADR 0031)", async () => {
    setActiveTenant('acme');
    await api.listActivity('0199f0b7-1c2d-7e3f-8a00-00000000e2e0', { source: 'traffic', beforeTsNs: '123' });
    expect(calls[0].path).toBe('/api/v1/sandboxes/0199f0b7-1c2d-7e3f-8a00-00000000e2e0/activity?limit=300&beforeTsNs=123&source=traffic');
    expect(calls[0].init.headers['FeRun-Tenant']).toBe('acme');
  });

  it('a create names no sandbox ID: ferund mints it', async () => {
    setActiveTenant('acme');
    answer(201, { status: { sandboxId: 'abcdefghjkm' }, token: 't', created: true });
    const res = await api.createSandbox('img:1', 2, 512, { environment: 'kino-dev' });
    expect(calls[0].path).toBe('/api/v1/sandboxes');
    expect(calls[0].init.method).toBe('POST');
    expect(JSON.parse(calls[0].init.body)).toEqual({ image: 'img:1', vcpus: 2, memoryMib: 512, environment: 'kino-dev' });
    expect(res.status.sandboxId).toBe('abcdefghjkm');
  });

  it('a sandbox is made in an environment with what it reaches and its egress, never attachable (ADR 0035)', async () => {
    setActiveTenant('acme');
    answer(201, { status: { sandboxId: 'abcdefghjkm' } });
    await api.createSandbox('', 0, 0, { environment: 'kino-dev', reaches: [{ service: 'api', port: 'http' }, { addon: 'db' }],
      egress: 'none', attachable: true, securityGroups: ['default'] });
    expect(JSON.parse(calls[0].init.body)).toEqual({ image: '', vcpus: 0, memoryMib: 0, environment: 'kino-dev',
      reaches: [{ service: 'api', port: 'http' }, { addon: 'db' }], egress: 'none' });
    answer(200, { sandboxId: 'abcdefghjkm', reaches: 'all' });
    await api.putSandboxReaches('abcdefghjkm', 'all');
    expect(calls[1].path).toBe('/api/v1/sandboxes/abcdefghjkm/reaches');
    expect(calls[1].init.method).toBe('PUT');
    expect(JSON.parse(calls[1].init.body)).toEqual({ reaches: 'all' });
    expect(api.putSandboxSecurityGroups).toBeUndefined();
    answer(200, { sandboxes: [] });
    await api.listSandboxes('kino dev');
    expect(calls[2].path).toBe('/api/v1/sandboxes?environment=kino+dev');
  });

  it("lists an environment's jobs, filtered here too", async () => {
    setActiveTenant('acme');
    const jobs = [{ id: 'a', environment: { id: 'e1', name: 'kino-dev' } }, { id: 'b', environment: { id: 'e2', name: 'stage' } }];
    answer(200, { jobs });
    expect((await api.listJobs(['kind=report'], 'kino-dev')).jobs.map((j) => j.id)).toEqual(['a']);
    expect(calls[0].path).toBe('/api/v1/jobs?label=kind%3Dreport&environment=kino-dev');
    answer(200, { jobs });
    expect((await api.listJobs([], 'e2')).jobs.map((j) => j.id)).toEqual(['b']);
    expect(calls[1].path).toBe('/api/v1/jobs?environment=e2');
    answer(200, { jobs });
    expect((await api.listJobs()).jobs).toHaveLength(2);
    expect(calls[2].path).toBe('/api/v1/jobs');
  });

  it('a 401 ends the sign-in', async () => {
    setActiveTenant('acme');
    const heard = vi.fn();
    const off = onSignedOut(heard);
    answer(401, { error: 'tenant bearer required' });
    await expect(api.listSandboxes()).rejects.toThrow('tenant bearer required');
    expect(heard).toHaveBeenCalled();
    off();
  });

  it('a tenant call without a tenant fails before the network', async () => {
    setActiveTenant('');
    await expect(api.listSandboxes()).rejects.toThrow('no tenant chosen');
    expect(calls).toHaveLength(0);
  });

  it('says a role_required refusal in the UI\'s words, and keeps the body', async () => {
    setActiveTenant('acme');
    answer(403, { error: 'this needs the role write on the tenant', code: 'role_required', role: 'write', scope: 'tenant' });
    const e = await api.pauseSandbox('abcdefghjkm').then(() => null, (x) => x);
    expect(e).toMatchObject({ status: 403, message: 'Your role does not allow this: it needs write on this tenant.' });
    expect(e.body).toMatchObject({ code: 'role_required', role: 'write' });
    answer(403, { error: 'x', code: 'role_required', role: 'admin', scope: 'environment' });
    await expect(api.getEnvironment('e')).rejects.toThrow('it needs admin on this environment.');
  });

  it('tells a ferund without the route', () => {
    expect(notAvailable(new ApiError('no such route: GET /api/v1/tenant/members', 404))).toBe(true);
    expect(notAvailable(new ApiError('no such tenant', 404))).toBe(false);
    expect(notAvailable(new ApiError('no such route: x', 403))).toBe(false);
    expect(notAvailable(null)).toBe(false);
  });

  it('forgets the tenant tokens an earlier UI kept', () => {
    localStorage.setItem('ferun-tenants', JSON.stringify({ acme: 'tok-1' }));
    localStorage.setItem('ferun-theme', 'light');
    forgetTenantTokens();
    expect(localStorage.getItem('ferun-tenants')).toBeNull();
    expect(localStorage.getItem('ferun-theme')).toBe('light');
  });

  it('the people\'s routes', async () => {
    setActiveTenant('acme');
    await api.createOwnTenant('mine');
    await api.listMembers();
    await api.putGrant('p-1', { role: 'write' });
    await api.putGrant('p-1', { role: 'read', environment: 'env-1' });
    await api.deleteGrant('p-1');
    await api.deleteGrant('p-1', 'env-1');
    await api.leaveTenant();
    await api.createInvitation({ role: 'read' });
    await api.createInvitation({ role: 'write', environment: 'env-1' });
    await api.revokeInvitation('i/1');
    await api.acceptInvitation('sec ret');
    await api.mintTokenOfTenant({ label: 'ci', role: 'read', environment: 'env-1' });
    await api.revokeTokenOfTenant('ab');
    await api.getQuota();
    expect(calls.map((c) => [c.init.method, c.path, c.init.body ? JSON.parse(c.init.body) : null])).toEqual([
      ['POST', '/api/v1/tenants', { name: 'mine' }],
      ['GET', '/api/v1/tenant/members', null],
      ['PUT', '/api/v1/tenant/members/p-1/grants', { role: 'write' }],
      ['PUT', '/api/v1/tenant/members/p-1/grants', { role: 'read', environment: 'env-1' }],
      ['DELETE', '/api/v1/tenant/members/p-1/grants', null],
      ['DELETE', '/api/v1/tenant/members/p-1/grants?environment=env-1', null],
      ['POST', '/api/v1/tenant/leave', null],
      ['POST', '/api/v1/tenant/invitations', { role: 'read' }],
      ['POST', '/api/v1/tenant/invitations', { role: 'write', environment: 'env-1' }],
      ['DELETE', '/api/v1/tenant/invitations/i%2F1', null],
      ['POST', '/api/v1/invitations/sec%20ret/accept', null],
      ['POST', '/api/v1/tenant/tokens', { label: 'ci', role: 'read', environment: 'env-1' }],
      ['DELETE', '/api/v1/tenant/tokens/ab', null],
      ['GET', '/api/v1/tenant/quota', null],
    ]);
    // Making a tenant and accepting an invitation name no tenant.
    expect(calls[0].init.headers['FeRun-Tenant']).toBeUndefined();
    expect(calls[10].init.headers['FeRun-Tenant']).toBeUndefined();
    expect(calls[1].init.headers['FeRun-Tenant']).toBe('acme');
  });

  it('admin token routes', async () => {
    answer(201, { token: 't', hashPrefix: 'abcd1234', createdAt: '2026-10-16T12:00:00Z' });
    await api.createTenantToken('acme', 'fecode');
    answer(200, { revoked: true });
    await api.revokeTenantToken('acme', 'ab/cd');
    expect(calls[0].path).toBe('/api/v1/admin/tenants/acme/tokens');
    expect(JSON.parse(calls[0].init.body)).toEqual({ label: 'fecode' });
    expect(calls[1].path).toBe('/api/v1/admin/tenants/acme/tokens/ab%2Fcd');
    expect(calls[1].init.method).toBe('DELETE');
  });
});

describe('the admin routes', () => {
  it("reads a node's activity with a source and a page bound", async () => {
    await api.nodeActivity('node-a', { beforeTsNs: '123', source: 'feosd' });
    expect(calls[0].path).toBe('/api/v1/admin/nodes/node-a/activity?limit=300&beforeTsNs=123&source=feosd');
  });

  it('patches, removes and drains nodes', async () => {
    await api.patchNode('node-a', { slots: 4 });
    await api.removeNode('node-a', false);
    await api.removeNode('node-a', true);
    await api.nodeDrain('node-a');
    expect(calls.map((c) => [c.init.method, c.path])).toEqual([
      ['PATCH', '/api/v1/admin/nodes/node-a'],
      ['DELETE', '/api/v1/admin/nodes/node-a'],
      ['DELETE', '/api/v1/admin/nodes/node-a?force=true'],
      ['POST', '/api/v1/admin/nodes/node-a/drain'],
    ]);
  });

  it('makes join tokens with the defaults as zeros', async () => {
    await api.createJoinToken({});
    await api.createJoinToken({ ttlSeconds: 600, node: 'node-b' });
    expect(JSON.parse(calls[0].init.body)).toEqual({ ttlSeconds: 0, node: '' });
    expect(JSON.parse(calls[1].init.body)).toEqual({ ttlSeconds: 600, node: 'node-b' });
  });
});

// The environments' routes, read as the tenant, answered with ferund's
// fixtures: each request is the fixture's (method, path and query), each
// answer comes back as ferund sends it, each refusal as an ApiError with
// ferund's text and status.
describe('the environments routes', () => {
  const STAGE = 'expensa-stage';
  const PREVIEW_ID = '0193c4d0-5e6f-7a01-8b2c-3d4e5f6a7b03';

  beforeEach(() => {
    setActiveTenant('fecode');
  });

  // ask makes the call against the fixture and checks the request; the
  // logs' error fixtures were recorded without the query the UI always
  // sends (lines=200), so for them only the method and path are checked.
  async function ask(name, call, { exactQuery = true } = {}) {
    const fx = fixture(name);
    const calls = serveFixtures(name);
    let out;
    let err = null;
    try {
      out = await call();
    } catch (e) {
      err = e;
    }
    expect(calls).toHaveLength(1);
    const got = requestOf(calls[0]);
    if (exactQuery) expect(got).toEqual({ method: fx.method, path: fx.path, query: fx.query });
    else expect({ method: got.method, path: got.path }).toEqual({ method: fx.method, path: fx.path });
    expect(calls[0].init.headers['FeRun-Tenant']).toBe('fecode');
    return { fx, out, err };
  }

  it('lists the environments with label filters, every one a ?label=', async () => {
    const { fx, out } = await ask('GET_environments.200', () =>
      api.listEnvironments(['fecode.project=0193a1b2-7c3d-7e4f-8a5b-6c7d8e9f0a01']));
    expect(out).toEqual(fx.response);
    expect(labelQuery(['a=b', 'c=d=e'])).toBe('?label=a%3Db&label=c%3Dd%3De');
    expect(labelQuery([])).toBe('');
  });

  it('reads an environment by name or ID, and names a missing one', async () => {
    let r = await ask('GET_environments_env.200', () => api.getEnvironment(STAGE));
    expect(r.out).toEqual(r.fx.response);
    r = await ask('GET_environments_env.200.preview', () => api.getEnvironment(PREVIEW_ID));
    expect(r.out.objects.filter((o) => o.runtime === 'attached')).toHaveLength(3);
    r = await ask('GET_environments_env.404', () => api.getEnvironment('expensa-nope'));
    expect(r.err).toBeInstanceOf(ApiError);
    expect(r.err).toMatchObject({ message: 'no such environment', status: 404 });
  });

  it('reads the deployments (50, newest first) and a deployment with its spec and diff', async () => {
    let r = await ask('GET_environments_env_deployments.200', () => api.listDeployments(STAGE));
    expect(r.out.deployments.map((x) => x.number)).toEqual([7, 6, 5]);
    r = await ask('GET_environments_env_deployments_n.200', () => api.getDeployment(STAGE, 7));
    expect(r.out.spec.services['expense-service'].imageAsWritten).toBe('ghcr.io/acme/expense:1.4');
    expect(r.out.diff).toHaveLength(3);
    r = await ask('GET_environments_env_deployments_n.404', () => api.getDeployment(STAGE, 99));
    expect(r.err).toMatchObject({ message: 'no such deployment', status: 404 });
  });

  it('reads the incidents, the open ones, and one incident', async () => {
    let r = await ask('GET_environments_env_incidents.200', () => api.listIncidents(STAGE));
    expect(r.out.incidents.map((i) => i.id)).toEqual(['13', '12', '9']);
    expect(r.out.incidents[0].evidence.pending).toHaveLength(3);
    r = await ask('GET_environments_env_incidents.200.open', () => api.listIncidents(STAGE, { open: true }));
    expect(r.out.incidents.map((i) => i.id)).toEqual(['12']);
    r = await ask('GET_environments_env_incidents_id.200', () => api.getIncident(STAGE, '12'));
    expect(r.out.evidence.logTail).toHaveLength(3);
    r = await ask('GET_environments_env_incidents_id.404', () => api.getIncident(STAGE, '99'));
    expect(r.err).toMatchObject({ message: 'no such incident', status: 404 });
  });

  it("reads an object's last 200 lines, of a replica, and ferund's refusals", async () => {
    let r = await ask('GET_environments_env_objects_kind_name_logs.200', () =>
      api.objectLogs(STAGE, 'service', 'expense-service', { replica: 1 }));
    expect(r.out).toEqual({ lines: r.fx.response.lines, source: 'container' });
    r = await ask('GET_environments_env_objects_kind_name_logs.200.process', () =>
      api.objectLogs('expensa-login-3f9a', 'service', 'expense-service'));
    expect(r.out.source).toBe('process');
    const loose = { exactQuery: false };
    r = await ask('GET_environments_env_objects_kind_name_logs.501', () => api.objectLogs(STAGE, 'addon', 'postgres'), loose);
    expect(r.err).toMatchObject({ message: 'this node does not serve container logs yet', status: 501 });
    r = await ask('GET_environments_env_objects_kind_name_logs.409', () =>
      api.objectLogs('expensa-login-3f9a', 'addon', 'nats'), loose);
    expect(r.err).toMatchObject({ message: 'paused', status: 409 });
    r = await ask('GET_environments_env_objects_kind_name_logs.404', () => api.objectLogs(STAGE, 'service', 'nope'), loose);
    expect(r.err).toMatchObject({ message: 'no such object: service/nope', status: 404 });
  });

  it('only ever reads: every environment call is a GET', async () => {
    const calls = serveFixtures(...Array(7).fill('GET_environments.200'));
    await api.listEnvironments();
    await api.getEnvironment(STAGE);
    await api.listDeployments(STAGE);
    await api.getDeployment(STAGE, 1);
    await api.listIncidents(STAGE);
    await api.getIncident(STAGE, '1');
    await api.objectLogs(STAGE, 'job', 'migrate');
    expect(calls.map((c) => c.init.method)).toEqual(Array(7).fill('GET'));
    expect(calls.every((c) => c.init.body === undefined)).toBe(true);
  });
});
