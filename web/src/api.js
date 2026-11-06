// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The UI's client of ferund's front door. Three ways in, as ferund's route
// table has them:
//
//   sign-in  /auth/config, /auth/me, /auth/login, /auth/logout: who is
//            signed in, their tenants with their roles, and how one signs
//            in (OIDC through the provider, or Basic with --admin-users).
//   admin    /api/v1/admin/…: an operator's session cookie, or the Basic
//            credentials of this tab (sessionStorage). Tenants, nodes,
//            join tokens, instances and the activity log. The UI calls
//            them for operators only.
//   tenant   /api/v1/sandboxes, /api/v1/tools, /api/v1/environments,
//            /api/v1/tenant/…: the person's session, the tenant named in
//            the header FeRun-Tenant (ADR 0031). The browser keeps no
//            tenant token: what the person's role allows, the session
//            does, and ferund decides.
//
// The UI never holds a sandbox's token or a tenant's: it shows a new one
// once and forgets it. Every request carries X-Requested-With: ferun
// (ferund refuses a cookie-authenticated change without it) and sends
// cookies to the same origin only. A 401 on an admin or a tenant route
// ends the sign-in: the UI returns to the sign-in screen.

import { ADMIN_KEY, TENANTS_KEY, readItem, writeItem } from './storage.js';

const V1 = '/api/v1';

export const CSRF_HEADER = 'X-Requested-With';
export const CSRF_VALUE = 'ferun';

// TENANT_HEADER names the tenant a session acts in.
export const TENANT_HEADER = 'FeRun-Tenant';

// ApiError is a failed request: the server's {"error"} text and the
// status.
export class ApiError extends Error {
  constructor(message, status, body = null) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    // body is the refusal's JSON: an environment's code, findings or
    // current deployment.
    this.body = body;
  }
}

// request makes one call and returns the answer's JSON ({} for an empty
// body). A failure throws an ApiError with the server's "error" text, or
// "<METHOD> <path>: HTTP <status>" when the body has none.
export async function request(method, path, { body, authorization, tenant } = {}) {
  const headers = { [CSRF_HEADER]: CSRF_VALUE };
  if (tenant) headers[TENANT_HEADER] = tenant;
  if (authorization) headers.Authorization = authorization;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const resp = await fetch(path, {
    method,
    headers,
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const raw = await resp.text();
  let data = {};
  try {
    data = raw ? JSON.parse(raw) : {};
  } catch {
    // A plain-text answer (a door's own error): the status says enough.
  }
  if (!resp.ok) {
    if (data && data.code === ROLE_REQUIRED) data.error = roleRequiredText(data);
    // ferund's refusals say {error}; the log store's Jaeger API {errors: [{msg}]}.
    const jaeger = data && Array.isArray(data.errors) && data.errors[0] && data.errors[0].msg;
    throw new ApiError((data && data.error) || jaeger || `${method} ${path}: HTTP ${resp.status}`, resp.status,
      data && typeof data === 'object' ? data : null);
  }
  return data;
}

// ROLE_REQUIRED is the code of a 403 for a principal that sees what it
// names but lacks the role (ADR 0031): {"code", "role", "scope"}.
export const ROLE_REQUIRED = 'role_required';

// roleRequiredText says a role_required refusal in the UI's words.
export function roleRequiredText(body) {
  const role = (body && body.role) || 'a higher role';
  const scope = body && body.scope === 'environment' ? 'this environment' : 'this tenant';
  return `Your role does not allow this: it needs ${role} on ${scope}.`;
}

// notAvailable tells a ferund that has no such route (an older one, or a
// surface that is off): 404 "no such route: …", never a 405.
export function notAvailable(e) {
  return !!e && e.status === 404 && /^no such route/.test(e.message || '');
}

// --------------------------------------------------------------- sign-in --

// The Basic credentials of this tab, "user:password"; '' when the tab
// signed in otherwise or not at all.
export function getBasic() {
  return readItem('sessionStorage', ADMIN_KEY);
}

export function setBasic(userpass) {
  writeItem('sessionStorage', ADMIN_KEY, userpass);
}

// basicHeader renders "user:password" as an Authorization header, UTF-8
// encoded (btoa alone takes Latin-1 only).
export function basicHeader(userpass) {
  const bin = Array.from(new TextEncoder().encode(userpass), (b) => String.fromCharCode(b)).join('');
  return 'Basic ' + btoa(bin);
}

function adminAuthorization() {
  const up = getBasic();
  return up ? basicHeader(up) : undefined;
}

// signedOutListeners hear that an admin call was refused with 401.
const signedOutListeners = new Set();

export function onSignedOut(fn) {
  signedOutListeners.add(fn);
  return () => signedOutListeners.delete(fn);
}

function signedOut() {
  setBasic('');
  for (const fn of signedOutListeners) fn();
}

// loginURL is where an OIDC sign-in begins; ferund sends the browser back
// to next (a local path) once it is done.
export function loginURL(next) {
  return '/auth/login?next=' + encodeURIComponent(next || '/');
}

export const auth = {
  // {oidc, basic}: the ways to sign in.
  config: () => request('GET', '/auth/config'),
  // The signed-in admin ({sub, email, name, username, picture, admin,
  // method} or {username, admin, method} for Basic), or null.
  me: async () => {
    try {
      return await request('GET', '/auth/me', { authorization: adminAuthorization() });
    } catch (e) {
      if (e.status === 401) return null;
      throw e;
    }
  },
  // signInBasic checks user and password with /auth/me, which answers a
  // refusal without a Basic challenge (so the browser shows no prompt of
  // its own), and keeps them for this tab.
  signInBasic: async (user, password) => {
    const up = `${user}:${password}`;
    let me = null;
    try {
      me = await request('GET', '/auth/me', { authorization: basicHeader(up) });
    } catch (e) {
      if (e.status !== 401) throw e;
    }
    if (!me || me.method !== 'basic') throw new ApiError('wrong user or password', 401);
    setBasic(up);
    return me;
  },
  // signOut ends the session (and forgets this tab's Basic credentials);
  // it returns where the browser goes next: the provider's sign-out, or
  // "/".
  signOut: async () => {
    let redirect = '/';
    try {
      const res = await request('POST', '/auth/logout');
      if (res && typeof res.redirect === 'string' && res.redirect) redirect = res.redirect;
    } finally {
      setBasic('');
    }
    return redirect;
  },
};

// ------------------------------------------------------------ the tenant --

// forgetTenantTokens removes the tenant tokens an earlier UI kept in this
// browser (localStorage ferun-tenants): the session acts now, and a
// token lying in storage is one an attacker's script could take.
export function forgetTenantTokens() {
  if (readItem('localStorage', TENANTS_KEY)) writeItem('localStorage', TENANTS_KEY, '');
}

// The tenant the current route acts in (set by the router).
let activeTenant = '';
export function setActiveTenant(name) {
  activeTenant = name || '';
}

const enc = encodeURIComponent;

async function adminCall(method, path, body) {
  try {
    return await request(method, V1 + '/admin' + path, { body, authorization: adminAuthorization() });
  } catch (e) {
    if (e.status === 401) signedOut();
    throw e;
  }
}

// sessionCall is a call of the session that names no tenant: making a
// tenant, an invitation's acceptance.
async function sessionCall(method, path, body) {
  try {
    return await request(method, V1 + path, { body });
  } catch (e) {
    if (e.status === 401) signedOut();
    throw e;
  }
}

// tenantCall is a call in the tenant of the route, with the session.
async function tenantCall(method, path, body) {
  const tenant = activeTenant;
  if (!tenant) throw new ApiError('no tenant chosen', 0);
  try {
    return await request(method, V1 + path, { body, tenant });
  } catch (e) {
    if (e.status === 401) signedOut();
    throw e;
  }
}

// tenantStream opens a tenant's streaming answer and returns its body's
// reader: the log store's follow (NDJSON), read for as long as signal
// lets it.
async function tenantStream(path, signal) {
  const tenant = activeTenant;
  if (!tenant) throw new ApiError('no tenant chosen', 0);
  const resp = await fetch(V1 + path, {
    headers: { [CSRF_HEADER]: CSRF_VALUE, [TENANT_HEADER]: tenant, Accept: 'application/x-ndjson' },
    credentials: 'same-origin',
    signal,
  });
  if (resp.status === 401) signedOut();
  if (!resp.ok) throw new ApiError(`GET ${V1 + path}: HTTP ${resp.status}`, resp.status);
  return resp.body.getReader();
}

function query(params) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '' && v !== 0) q.set(k, String(v));
  }
  const s = q.toString();
  return s ? '?' + s : '';
}

// labelQuery renders "key=value" label filters as ferund's repeated
// ?label= parameters (every one must match).
export function labelQuery(labels) {
  const q = new URLSearchParams();
  for (const l of labels || []) q.append('label', l);
  const s = q.toString();
  return s ? '?' + s : '';
}

// LOG_LINES is how many lines of an object's log the UI reads.
export const LOG_LINES = 200;

// ACTIVITY_LIMIT is the page the activity views read.
export const ACTIVITY_LIMIT = 300;

export const api = {
  // Admin: every tenant and its tokens (an operator's).
  listTenants: () => adminCall('GET', '/tenants'),
  createTenant: (name) => adminCall('POST', '/tenants', { name }),
  deleteTenant: (name) => adminCall('DELETE', `/tenants/${enc(name)}`),
  listTenantTokens: (name) => adminCall('GET', `/tenants/${enc(name)}/tokens`),
  createTenantToken: (name, label) => adminCall('POST', `/tenants/${enc(name)}/tokens`, { label }),
  revokeTenantToken: (name, hash) => adminCall('DELETE', `/tenants/${enc(name)}/tokens/${enc(hash)}`),

  // Admin: the nodes.
  listNodes: () => adminCall('GET', '/nodes'),
  patchNode: (node, settings) => adminCall('PATCH', `/nodes/${enc(node)}`, settings),
  removeNode: (node, force) => adminCall('DELETE', `/nodes/${enc(node)}${force ? '?force=true' : ''}`),
  nodeDrain: (node) => adminCall('POST', `/nodes/${enc(node)}/drain`),
  nodeUndrain: (node) => adminCall('POST', `/nodes/${enc(node)}/undrain`),
  nodeCerts: (node) => adminCall('GET', `/nodes/${enc(node)}/certs`),
  nodeActivity: (node, { beforeTsNs, source } = {}) =>
    adminCall('GET', `/nodes/${enc(node)}/activity${query({ limit: ACTIVITY_LIMIT, beforeTsNs, source })}`),

  // Admin: join tokens and the control plane's instances.
  createJoinToken: ({ ttlSeconds, node } = {}) =>
    adminCall('POST', '/join-tokens', { ttlSeconds: ttlSeconds || 0, node: node || '' }),
  listJoinTokens: () => adminCall('GET', '/join-tokens'),
  revokeJoinToken: (id) => adminCall('DELETE', `/join-tokens/${enc(id)}`),
  listInstances: () => adminCall('GET', '/instances'),

  // Admin: a sandbox's activity, ascending; source is '', ferund, feosd,
  // authn or traffic.
  listActivity: (sandboxId, { beforeTsNs, source } = {}) =>
    adminCall('GET', `/sandboxes/${enc(sandboxId)}/activity${query({ limit: ACTIVITY_LIMIT, beforeTsNs, source })}`),

  // In the tenant.
  listSandboxes: () => tenantCall('GET', '/sandboxes'),
  // vcpus and memoryMib 0: the platform's default size.
  // ferund mints the new sandbox's ID (ADR 0019): the answer's
  // status.sandboxId.
  createSandbox: (image, vcpus = 0, memoryMib = 0) =>
    tenantCall('POST', '/sandboxes', { image, vcpus, memoryMib }),
  deleteSandbox: (sandboxId) => tenantCall('DELETE', `/sandboxes/${enc(sandboxId)}`),
  pauseSandbox: (sandboxId) => tenantCall('POST', `/sandboxes/${enc(sandboxId)}/pause`),
  resumeSandbox: (sandboxId) => tenantCall('POST', `/sandboxes/${enc(sandboxId)}/resume`),
  listTokens: (sandboxId) => tenantCall('GET', `/sandboxes/${enc(sandboxId)}/tokens`),
  createToken: (sandboxId, label) => tenantCall('POST', `/sandboxes/${enc(sandboxId)}/tokens`, { label }),
  // The hash is scoped to its sandbox in the path: a token is revoked
  // where it was minted.
  revokeToken: (sandboxId, hash) => tenantCall('DELETE', `/sandboxes/${enc(sandboxId)}/tokens/${enc(hash)}`),
  listTools: () => tenantCall('GET', '/tools'),
  // The Kubernetes and SSH bindings with their use: fingerprints and
  // public keys, never key material.
  getAccess: (sandboxId) => tenantCall('GET', `/sandboxes/${enc(sandboxId)}/access`),

  // As the tenant: its environments. env is the name or the UUID. The
  // UI writes deployments and secrets' values through the builder (ADR
  // 0032) and nothing else: it reverts and deletes nothing, and reads no
  // secret's value and no route token.
  listEnvironments: (labels = []) => tenantCall('GET', '/environments' + labelQuery(labels)),
  getEnvironment: (env) => tenantCall('GET', `/environments/${enc(env)}`),
  listDeployments: (env, limit = 50) => tenantCall('GET', `/environments/${enc(env)}/deployments${query({ limit })}`),
  // A deployment with its normalised spec and its diff against the one
  // before.
  getDeployment: (env, n) => tenantCall('GET', `/environments/${enc(env)}/deployments/${enc(n)}`),
  listIncidents: (env, { open } = {}) =>
    tenantCall('GET', `/environments/${enc(env)}/incidents${open ? '?open=1' : ''}`),
  getIncident: (env, id) => tenantCall('GET', `/environments/${enc(env)}/incidents/${enc(id)}`),
  // The last lines of an object's log: an image pod's container (replica
  // 0 unless given), an attached service's process, a job's last run.
  // 501: the node serves no container logs; 409: the instance is paused
  // (never woken for its logs).
  objectLogs: (env, kind, name, { lines = LOG_LINES, replica = 0 } = {}) =>
    tenantCall('GET', `/environments/${enc(env)}/objects/${enc(kind)}/${enc(name)}/logs${query({ lines, replica })}`),
  // The log store, ferun-sink (ADR 0026, docs/architecture/sink.md,
  // Reading): its routes are served beside ferund's at the same address
  // and take the same tenant token. params is a URLSearchParams of
  // GET /api/v1/logs (logs.js, sinkParams). A page's lines are oldest
  // first; followLogs reads new lines as they come.
  searchLogs: (params) => tenantCall('GET', `/logs?${params}`),
  // Its traces, in the shape of Jaeger's HTTP query API (ADR 0026 §10):
  // a trace by its ID within an environment (its ID), and a search
  // (traces.js, traceSearchParams).
  getTrace: (env, trace) => tenantCall('GET', `/jaeger/api/traces/${enc(trace)}${query({ environment: env })}`),
  findTraces: (params) => tenantCall('GET', `/jaeger/api/traces?${params}`),
  followLogs: (params, signal) => tenantStream(`/logs?${params}&follow=1`, signal),

  // People (ADR 0031): a person's tenants, their members, invitations,
  // tokens and the quota's use. A ferund without these routes answers
  // 404 "no such route" (notAvailable).
  //
  // A new tenant, its maker its admin: {tenant, role}.
  createOwnTenant: (name) => sessionCall('POST', '/tenants', { name }),
  // The tenant as the session sees it: {tenant, role, environments}.
  getTenant: () => tenantCall('GET', '/tenant'),
  // The members: {members: [{person, name, email, role, environments:
  // [{id, name, role}]}]}, role the tenant grant's ('' with grants on
  // environments alone).
  listMembers: () => tenantCall('GET', '/tenant/members'),
  // A grant of a member on the tenant (environment '') or on one of its
  // environments (its ID); its removal. The last admin stays (ferund).
  putGrant: (person, { role, environment = '' }) =>
    tenantCall('PUT', `/tenant/members/${enc(person)}/grants`, environment ? { role, environment } : { role }),
  deleteGrant: (person, environment = '') =>
    tenantCall('DELETE', `/tenant/members/${enc(person)}/grants${query({ environment })}`),
  // Leaving the tenant: every grant of the session's person in it.
  leaveTenant: () => tenantCall('POST', '/tenant/leave'),
  // Invitations: single-use links with a role and a scope, valid 7 days;
  // a new one answers its secret once ({id, token, url?, role,
  // environment, expiresAt}).
  listInvitations: () => tenantCall('GET', '/tenant/invitations'),
  createInvitation: ({ role, environment = '' }) =>
    tenantCall('POST', '/tenant/invitations', environment ? { role, environment } : { role }),
  revokeInvitation: (id) => tenantCall('DELETE', `/tenant/invitations/${enc(id)}`),
  // An invitation by its link's secret, for the person who opens it:
  // what it grants ({tenant, role, environment?, expiresAt}); its
  // acceptance gives the session's person the grant.
  getInvitation: (token) => sessionCall('GET', `/invitations/${enc(token)}`),
  acceptInvitation: (token) => sessionCall('POST', `/invitations/${enc(token)}/accept`),
  // The tenant's tokens, with a role and an optional environment, never
  // above the minter's; a new one answers its secret once.
  listTokensOfTenant: () => tenantCall('GET', '/tenant/tokens'),
  mintTokenOfTenant: ({ label, role, environment = '' }) =>
    tenantCall('POST', '/tenant/tokens', environment ? { label, role, environment } : { label, role }),
  revokeTokenOfTenant: (hash) => tenantCall('DELETE', `/tenant/tokens/${enc(hash)}`),
  // The quota: {limits, use}, each {sandboxes, environments, runningJobs,
  // vcpus, memoryMib}; a limit absent or 0 is none.
  getQuota: () => tenantCall('GET', '/tenant/quota'),

  // The builder's (ADR 0032). A plan of an environment answers {valid,
  // findings, diff, base, current} and, from a ferund that pins at plan,
  // images {as written: pinned}; one without an environment {valid,
  // findings}. An apply answers 201 the deployment, 200 unchanged, 422
  // invalid_spec with findings, 409 base_mismatch with current (the
  // ApiError's body).
  // A create with the same ID and name again answers 200 with it.
  createEnvironment: (name, id) => tenantCall('POST', '/environments', id ? { name, id } : { name }),
  planEnvironment: (env, spec, base) => tenantCall('POST', `/environments/${enc(env)}/plan`, { spec, base }),
  planSpec: (spec) => tenantCall('POST', '/environments/plan', { spec }),
  applyDeployment: (env, { spec, base, message }) =>
    tenantCall('POST', `/environments/${enc(env)}/deployments`, message ? { spec, base, message } : { spec, base }),
  // The secrets' names and latest versions, never a value; a put makes a
  // new version, {name, version}.
  listSecrets: (env) => tenantCall('GET', `/environments/${enc(env)}/secrets`),
  putSecret: (env, name, value) => tenantCall('PUT', `/environments/${enc(env)}/secrets/${enc(name)}`, { value }),
  // The addons' catalog; null from a ferund that has none (404).
  addonCatalog: async () => {
    try {
      return await tenantCall('GET', '/addons');
    } catch (e) {
      if (e.status === 404 || e.status === 405) return null;
      throw e;
    }
  },
};

// mcpURL is the MCP door of this origin, which the UI shows to connect
// agents.
export function mcpURL() {
  return `${globalThis.location ? globalThis.location.origin : ''}/mcp`;
}
