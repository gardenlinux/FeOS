// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  ApiError, api, auth, basicHeader, getBasic, getTenantToken, labelQuery, loginURL, onSignedOut, onTenantTokens, request,
  setActiveTenant, setBasic, setTenantToken, storedTokenHash,
} from './api.js';
import { sha256Hex } from './sha256.js';
import { fixture, requestOf, serveFixtures } from './testing/ferun.js';

// The client's contract with ferund: X-Requested-With: ferun and
// same-origin credentials on every request, Basic from this tab's
// sessionStorage, tenant tokens as Bearer from localStorage, a 401 on an
// admin route ending the sign-in.

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

describe('tenant tokens', () => {
  it('are kept per tenant in localStorage and sent as Bearer', async () => {
    setTenantToken('acme', 'tok-1');
    expect(JSON.parse(localStorage.getItem('ferun-tenants'))).toEqual({ acme: 'tok-1' });
    setActiveTenant('acme');
    answer(200, { sandboxes: [] });
    await api.listSandboxes();
    expect(calls[0].path).toBe('/api/v1/sandboxes');
    expect(calls[0].init.headers.Authorization).toBe('Bearer tok-1');
    expect(calls[0].init.headers['X-Requested-With']).toBe('ferun');
  });

  it('a create names no sandbox ID: ferund mints it', async () => {
    setTenantToken('acme', 'tok-1');
    setActiveTenant('acme');
    answer(201, { status: { sandboxId: 'abcdefghjkm' }, token: 't', created: true });
    const res = await api.createSandbox('img:1', 2, 512);
    expect(calls[0].path).toBe('/api/v1/sandboxes');
    expect(calls[0].init.method).toBe('POST');
    expect(JSON.parse(calls[0].init.body)).toEqual({ image: 'img:1', vcpus: 2, memoryMib: 512 });
    expect(res.status.sandboxId).toBe('abcdefghjkm');
  });

  it('a refused token is forgotten, and the listeners hear it', async () => {
    setTenantToken('acme', 'tok-1');
    setActiveTenant('acme');
    const heard = vi.fn();
    const off = onTenantTokens(heard);
    answer(401, { error: 'unknown tenant token' });
    await expect(api.listSandboxes()).rejects.toThrow('unknown tenant token');
    expect(getTenantToken('acme')).toBe('');
    expect(heard).toHaveBeenCalled();
    off();
  });

  it('a tenant call without a token fails before the network', async () => {
    setActiveTenant('nobody');
    await expect(api.listSandboxes()).rejects.toThrow('this browser holds no token of tenant nobody');
    expect(calls).toHaveLength(0);
  });

  it('storedTokenHash is ferund’s hash of the token', () => {
    setTenantToken('acme', 'tok-1');
    expect(storedTokenHash('acme')).toBe(sha256Hex('tok-1'));
    expect(storedTokenHash('other')).toBe('');
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
  it('reads activity with a source and a page bound', async () => {
    await api.listActivity('0199f0b7-1c2d-7e3f-8a00-00000000e2e0', { source: 'traffic' });
    await api.nodeActivity('node-a', { beforeTsNs: '123' });
    expect(calls[0].path).toBe('/api/v1/admin/sandboxes/0199f0b7-1c2d-7e3f-8a00-00000000e2e0/activity?limit=300&source=traffic');
    expect(calls[1].path).toBe('/api/v1/admin/nodes/node-a/activity?limit=300&beforeTsNs=123');
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
    setTenantToken('fecode', 'tok-fecode');
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
    expect(calls[0].init.headers.Authorization).toBe('Bearer tok-fecode');
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

describe('sha256Hex', () => {
  it('matches the standard vectors', () => {
    expect(sha256Hex('')).toBe('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855');
    expect(sha256Hex('abc')).toBe('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
    expect(sha256Hex('abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq'))
      .toBe('248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1');
  });

  it('hashes UTF-8 bytes, and messages at the block boundaries', () => {
    expect(sha256Hex('ä')).toBe('33e6d73fee82904c8d7afb78de1154d1e8dc2a0edb08120e63df5b9385c2d9cc');
    expect(sha256Hex('a'.repeat(55))).toBe('9f4390f8d30c2dd92ec9f095b65e2b9ae9b0a925a5258e241c9f1e910f734318');
    expect(sha256Hex('a'.repeat(56))).toBe('b35439a4ac6f0948b6d6f9e3c6af0f5f590ce20f1bde7090ef7970686ec6738a');
    expect(sha256Hex('a'.repeat(64))).toBe('ffe054fe7ae0cb6dc65c3af9b61d5209f439851db43d0ba5997337df154668eb');
  });
});
