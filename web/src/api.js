// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The UI's client of ferund's front door. Three ways in, as ferund's route
// table has them:
//
//   sign-in  /auth/config, /auth/me, /auth/login, /auth/logout: who is
//            signed in, their projects with their roles, and how one signs
//            in (OIDC through the provider, or Basic with --admin-users).
//   admin    /api/v1/admin/…: an operator's session cookie, or the Basic
//            credentials of this tab (sessionStorage). Projects, nodes,
//            join tokens, instances and the nodes' activity. The UI
//            calls them for operators only.
//   project   /api/v1/sandboxes, /api/v1/tools, /api/v1/environments,
//            /api/v1/providers, /api/v1/project/…: the person's session,
//            the project named in the header FeRun-Project (ADR 0031).
//            The browser keeps no project token: what the person's role
//            allows, the session does, and ferund decides.
//
// The UI never holds a sandbox's token or a project's: it shows a new one
// once and forgets it. Every request carries X-Requested-With: ferun
// (ferund refuses a cookie-authenticated change without it) and sends
// cookies to the same origin only. A 401 on an admin or a project route
// ends the sign-in: the UI returns to the sign-in screen.

import { ADMIN_KEY, OLD_TOKENS_KEY, readItem, writeItem } from './storage.js';

const V1 = '/api/v1';

export const CSRF_HEADER = 'X-Requested-With';
export const CSRF_VALUE = 'ferun';

// PROJECT_HEADER names the project a session acts in.
export const PROJECT_HEADER = 'FeRun-Project';

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
export async function request(method, path, { body, authorization, project } = {}) {
  const headers = { [CSRF_HEADER]: CSRF_VALUE };
  if (project) headers[PROJECT_HEADER] = project;
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
  const scope = body && body.scope === 'environment' ? 'this environment' : 'this project';
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
// to next (a local path) once it is done. An address goes on to the
// provider as its login_hint, which picks the company or the password.
export function loginURL(next, email) {
  const u = '/auth/login?next=' + encodeURIComponent(next || '/');
  const e = (email || '').trim();
  return e ? u + '&login_hint=' + encodeURIComponent(e) : u;
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

// ------------------------------------------------------------ the project --

// forgetOldTokens removes the project tokens an earlier UI kept in this
// browser (localStorage OLD_TOKENS_KEY): the session acts now, and a
// token lying in storage is one an attacker's script could take.
export function forgetOldTokens() {
  if (readItem('localStorage', OLD_TOKENS_KEY)) writeItem('localStorage', OLD_TOKENS_KEY, '');
}

// The project the current route acts in (set by the router).
let activeProject = '';
export function setActiveProject(name) {
  activeProject = name || '';
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

// sessionCall is a call of the session that names no project: making a
// project, an invitation's acceptance.
async function sessionCall(method, path, body) {
  try {
    return await request(method, V1 + path, { body });
  } catch (e) {
    if (e.status === 401) signedOut();
    throw e;
  }
}

// projectCall is a call in the project of the route, with the session.
async function projectCall(method, path, body) {
  const project = activeProject;
  if (!project) throw new ApiError('no project chosen', 0);
  try {
    return await request(method, V1 + path, { body, project });
  } catch (e) {
    if (e.status === 401) signedOut();
    throw e;
  }
}

// otherProjectCall is a call in a project the route is not in: the
// switcher's list of another project's environments.
async function otherProjectCall(project, method, path) {
  try {
    return await request(method, V1 + path, { project });
  } catch (e) {
    if (e.status === 401) signedOut();
    throw e;
  }
}

// projectStream opens a project's streaming answer and returns its body's
// reader: the log store's follow (NDJSON), read for as long as signal
// lets it.
async function projectStream(path, signal) {
  const project = activeProject;
  if (!project) throw new ApiError('no project chosen', 0);
  const resp = await fetch(V1 + path, {
    headers: { [CSRF_HEADER]: CSRF_VALUE, [PROJECT_HEADER]: project, Accept: 'application/x-ndjson' },
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
  // Admin: every project and its tokens (an operator's).
  // account: the list of one account's projects ('' every project); each
  // project names its account {name, title} (ADR 0059).
  listProjects: (account = '') => adminCall('GET', '/projects' + query({ account })),
  // A project on account ('' or left out: default).
  createProject: (name, account = '') => adminCall('POST', '/projects', account ? { name, account } : { name }),
  // A project moved to another account: {project, account, from, movedAt,
  // moved} (moved false when it is already there).
  adminMoveProject: (name, account) => adminCall('POST', `/projects/${enc(name)}/move`, { account }),
  deleteProject: (name) => adminCall('DELETE', `/projects/${enc(name)}`),
  listProjectTokens: (name) => adminCall('GET', `/projects/${enc(name)}/tokens`),
  createProjectToken: (name, label) => adminCall('POST', `/projects/${enc(name)}/tokens`, { label }),
  revokeProjectToken: (name, hash) => adminCall('DELETE', `/projects/${enc(name)}/tokens/${enc(hash)}`),

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
  // kind: node (the default) or ingress, with the ingress's name.
  createJoinToken: ({ ttlSeconds, node, kind, name } = {}) =>
    adminCall('POST', '/join-tokens', {
      ttlSeconds: ttlSeconds || 0, node: node || '', ...(kind ? { kind } : {}), ...(name ? { name } : {}),
    }),
  listJoinTokens: () => adminCall('GET', '/join-tokens'),
  revokeJoinToken: (id) => adminCall('DELETE', `/join-tokens/${enc(id)}`),
  listInstances: () => adminCall('GET', '/instances'),

  // In the project. A sandbox is of an environment (ADR 0035): the list
  // of one (its name or ID), or without it every sandbox the session may
  // read.
  listSandboxes: (environment = '') => projectCall('GET', '/sandboxes' + query({ environment })),
  // One sandbox, with its environment {id, name}.
  getSandbox: (id) => projectCall('GET', `/sandboxes/${enc(id)}`),
  // A sandbox's activity, ascending; source is '', ferund, feosd, authn
  // or traffic. An operator gets it whole, everyone else without the
  // fleet's part (ADR 0031).
  listActivity: (sandboxId, { beforeTsNs, source } = {}) =>
    projectCall('GET', `/sandboxes/${enc(sandboxId)}/activity${query({ limit: ACTIVITY_LIMIT, beforeTsNs, source })}`),
  // vcpus and memoryMib 0: the platform's default size.
  // ferund mints the new sandbox's ID (ADR 0019): the answer's
  // status.sandboxId.
  // environment (its name or ID) is required and fixed; reaches is what
  // of it the pod reaches, "all" or [{service, port}, {addon}], left out:
  // nothing; egress as a service's, left out: the internet (ADR 0034,
  // revised);
  // imageCredentials ({username, password}) pull a private image (ADR
  // 0025), sealed, never answered.
  // secrets name the HTTP secrets of its environment its pod gets, each
  // at its latest version (ADR 0055).
  createSandbox: (image, vcpus = 0, memoryMib = 0, { environment, reaches, egress, imageCredentials = null, secrets = [] } = {}) =>
    projectCall('POST', '/sandboxes', {
      image, vcpus, memoryMib, environment,
      ...(reaches !== undefined && reaches !== null ? { reaches } : {}),
      ...(egress !== undefined && egress !== null ? { egress } : {}),
      ...(imageCredentials ? { imageCredentials } : {}),
      ...(secrets && secrets.length ? { secrets } : {}),
    }),
  // What a sandbox reaches of its environment, the whole of it: "all" or
  // the list ([] for nothing); in place, no move (ADR 0035, revised).
  // Answers the sandbox's view.
  putSandboxReaches: (sandboxId, reaches) =>
    projectCall('PUT', `/sandboxes/${enc(sandboxId)}/reaches`, { reaches }),
  putImageCredentials: (sandboxId, creds) => projectCall('PUT', `/sandboxes/${enc(sandboxId)}/image-credentials`, creds),
  deleteImageCredentials: (sandboxId) => projectCall('DELETE', `/sandboxes/${enc(sandboxId)}/image-credentials`),
  deleteSandbox: (sandboxId) => projectCall('DELETE', `/sandboxes/${enc(sandboxId)}`),
  pauseSandbox: (sandboxId) => projectCall('POST', `/sandboxes/${enc(sandboxId)}/pause`),
  resumeSandbox: (sandboxId) => projectCall('POST', `/sandboxes/${enc(sandboxId)}/resume`),
  listTokens: (sandboxId) => projectCall('GET', `/sandboxes/${enc(sandboxId)}/tokens`),
  createToken: (sandboxId, label) => projectCall('POST', `/sandboxes/${enc(sandboxId)}/tokens`, { label }),
  // The hash is scoped to its sandbox in the path: a token is revoked
  // where it was minted.
  revokeToken: (sandboxId, hash) => projectCall('DELETE', `/sandboxes/${enc(sandboxId)}/tokens/${enc(hash)}`),
  listTools: () => projectCall('GET', '/tools'),
  // The Kubernetes and SSH bindings with their use: fingerprints and
  // public keys, never key material.
  getAccess: (sandboxId) => projectCall('GET', `/sandboxes/${enc(sandboxId)}/access`),

  // As the project: its environments. env is the name or the UUID. The
  // UI writes deployments and secrets' values through the builder (ADR
  // 0032); it reads no secret's value, and a route token only as its
  // mint answers it, once.
  listEnvironments: (labels = []) => projectCall('GET', '/environments' + labelQuery(labels)),
  listEnvironmentsOf: (project) => otherProjectCall(project, 'GET', '/environments'),
  getEnvironment: (env) => projectCall('GET', `/environments/${enc(env)}`),
  listDeployments: (env, limit = 50) => projectCall('GET', `/environments/${enc(env)}/deployments${query({ limit })}`),
  // A deployment with its normalised spec and its diff against the one
  // before.
  getDeployment: (env, n) => projectCall('GET', `/environments/${enc(env)}/deployments/${enc(n)}`),
  listIncidents: (env, { open } = {}) =>
    projectCall('GET', `/environments/${enc(env)}/incidents${open ? '?open=1' : ''}`),
  getIncident: (env, id) => projectCall('GET', `/environments/${enc(env)}/incidents/${enc(id)}`),
  // The last lines of an object's log: an image pod's container (replica
  // 0 unless given), an attached service's process, a job's last run.
  // 501: the node serves no container logs; 409: the instance is paused
  // (never woken for its logs).
  objectLogs: (env, kind, name, { lines = LOG_LINES, replica = 0 } = {}) =>
    projectCall('GET', `/environments/${enc(env)}/objects/${enc(kind)}/${enc(name)}/logs${query({ lines, replica })}`),
  // The log store, ferun-sink (ADR 0026, docs/architecture/sink.md,
  // Reading): its routes are served beside ferund's at the same address
  // and take the same project token. params is a URLSearchParams of
  // GET /api/v1/logs (logs.js, sinkParams). A page's lines are oldest
  // first; followLogs reads new lines as they come.
  searchLogs: (params) => projectCall('GET', `/logs?${params}`),
  // Its traces, in the shape of Jaeger's HTTP query API (ADR 0026 §10):
  // a trace by its ID within an environment (its ID). The console opens a
  // trace from a log line and lists none: a search costs the store a scan.
  getTrace: (env, trace) => projectCall('GET', `/jaeger/api/traces/${enc(trace)}${query({ environment: env })}`),
  followLogs: (params, signal) => projectStream(`/logs?${params}&follow=1`, signal),

  // People (ADR 0031): a person's projects, their members, invitations,
  // tokens and the quota's use. A ferund without these routes answers
  // 404 "no such route" (notAvailable).
  //
  // A new project on an account the person may put projects on (ADR
  // 0059; '' or left out: default), its maker its admin: {name,
  // createdAt, role, limits, account}.
  createOwnProject: (name, account = '') => sessionCall('POST', '/projects', account ? { name, account } : { name }),
  // The project as the session sees it: {project, role, environments}.
  getProject: () => projectCall('GET', '/project'),
  // The members and the integrations the project grants (ADR 0056):
  // {members: [{person, name, email, issuer, subject, disabled?, role,
  // madeBy, madeByName, environments: [{id, name, role, madeBy,
  // madeByName}]}], integrations: [{integration, title, url, disabled,
  // role, madeBy, madeByName, environments}]}, role the project grant's
  // ('' with grants on environments alone).
  listMembers: () => projectCall('GET', '/project/members'),
  // An integration's grant on the project (environment '') or on one of
  // its environments (ID or name), and its removal (ADR 0056): {integration,
  // role, environment}.
  putIntegrationGrant: (integration, { role, environment = '' }) =>
    projectCall('PUT', `/project/integrations/${enc(integration)}/grants`, environment ? { role, environment } : { role }),
  deleteIntegrationGrant: (integration, environment = '') =>
    projectCall('DELETE', `/project/integrations/${enc(integration)}/grants${query({ environment })}`),
  // A project token's grant on the project or on an environment, its hash
  // as the token list answers it (ADR 0060: the protect dialog gives one a
  // grant on the environment): {token, role, environment}.
  putTokenGrant: (hash, { role, environment = '' }) =>
    projectCall('PUT', `/project/tokens/${enc(hash)}/grants`, environment ? { role, environment } : { role }),
  deleteTokenGrant: (hash, environment = '') =>
    projectCall('DELETE', `/project/tokens/${enc(hash)}/grants${query({ environment })}`),
  // The project's account, since when, and its moves (ADR 0059): {account:
  // {name, title}, since, moves: [{project, from, to, at, by}]}; every
  // member reads it.
  getProjectAccount: () => projectCall('GET', '/project/account'),
  // The project to another account, by one of its admins who may put
  // projects there: {project, account, from, movedAt, moved}.
  moveProject: (account) => projectCall('POST', '/project/move', { account }),
  // A grant of a member on the project (environment '') or on one of its
  // environments (its ID); its removal. The last admin stays (ferund).
  putGrant: (person, { role, environment = '' }) =>
    projectCall('PUT', `/project/members/${enc(person)}/grants`, environment ? { role, environment } : { role }),
  deleteGrant: (person, environment = '') =>
    projectCall('DELETE', `/project/members/${enc(person)}/grants${query({ environment })}`),
  // Leaving the project: every grant of the session's person in it.
  leaveProject: () => projectCall('POST', '/project/leave'),
  // Invitations: single-use links with a role and a scope, valid 7 days;
  // a new one answers its secret once ({id, token, url?, role,
  // environment, expiresAt}).
  listInvitations: () => projectCall('GET', '/project/invitations'),
  createInvitation: ({ role, environment = '' }) =>
    projectCall('POST', '/project/invitations', environment ? { role, environment } : { role }),
  revokeInvitation: (id) => projectCall('DELETE', `/project/invitations/${enc(id)}`),
  // An invitation by its link's secret, for the person who opens it:
  // what it grants, a project's ({project, role, environment?,
  // environmentName?, expiresAt}) or an account's ({account: {name,
  // title}, role, expiresAt}, ADR 0059); its acceptance gives the
  // session's person the grant ({project, role} or {account, role}).
  getInvitation: (token) => sessionCall('GET', `/invitations/${enc(token)}`),
  acceptInvitation: (token) => sessionCall('POST', `/invitations/${enc(token)}/accept`),
  // The project's tokens, with a role and an optional environment, never
  // above the minter's; a new one answers its secret once.
  listTokensOfProject: () => projectCall('GET', '/project/tokens'),
  mintTokenOfProject: ({ label, role, environment = '' }) =>
    projectCall('POST', '/project/tokens', environment ? { label, role, environment } : { label, role }),
  revokeTokenOfProject: (hash) => projectCall('DELETE', `/project/tokens/${enc(hash)}`),
  // The quota: {limits, use}, each {sandboxes, environments, runningJobs,
  // vcpus, memoryMib}; a limit left out is none, 0 is a limit.
  getQuota: () => projectCall('GET', '/project/quota'),

  // The builder's (ADR 0032). A plan of an environment answers {valid,
  // findings, diff, base, current} and, from a ferund that pins at plan,
  // images {as written: pinned}; one without an environment {valid,
  // findings}. An apply answers 201 the deployment, 200 unchanged, 422
  // invalid_spec with findings, 409 base_mismatch with current (the
  // ApiError's body).
  // A create with the same ID and name again answers 200 with it.
  createEnvironment: (name, id) => projectCall('POST', '/environments', id ? { name, id } : { name }),
  planEnvironment: (env, spec, base) => projectCall('POST', `/environments/${enc(env)}/plan`, { spec, base }),
  planSpec: (spec) => projectCall('POST', '/environments/plan', { spec }),
  applyDeployment: (env, { spec, base, message }) =>
    projectCall('POST', `/environments/${enc(env)}/deployments`, message ? { spec, base, message } : { spec, base }),
  // An environment's secrets (ADR 0055), both kinds in one list sorted by
  // name, never a value: a value secret {name, type: "value", version,
  // createdBy, createdAt}; an HTTP secret also its definition (upstream,
  // mode, caPem?, pathPrefixes, forwardedHost, rateLimit?, credential:
  // its authentication, every secret field {"set": true}) and usedBy
  // {sandboxes, objects, latest, deployed}. A put of {value} makes a
  // value's new version, a put of {type: "http", …} an HTTP secret's,
  // whose secret fields {"set": true} keep the latest version's while
  // its origin, CA and token URL stay; 201 a new version, 200 unchanged,
  // 409 secret_kind for a name of the other kind. A delete of one a
  // deployment pins or a sandbox names is 409. A check is an HTTP
  // secret's preflight ({ok, addresses, tls, token, probe, error}) of
  // the stored latest version, or with binding of a draft, whose secret
  // fields {"set": true} are the latest version's; nothing is stored.
  listSecrets: (env) => projectCall('GET', `/environments/${enc(env)}/secrets`),
  getSecret: (env, name) => projectCall('GET', `/environments/${enc(env)}/secrets/${enc(name)}`),
  putSecret: (env, name, value) => projectCall('PUT', `/environments/${enc(env)}/secrets/${enc(name)}`, { value }),
  putHttpSecret: (env, name, def) =>
    projectCall('PUT', `/environments/${enc(env)}/secrets/${enc(name)}`, { ...def, type: 'http' }),
  checkSecret: (env, name, { binding = null, probe = null } = {}) =>
    projectCall('POST', `/environments/${enc(env)}/secrets/${enc(name)}/check`,
      { ...(binding ? { binding } : {}), ...(probe ? { probe } : {}) }),
  // An environment's operations beside the builder (ADR 0018).
  deleteEnvironment: (env) => projectCall('DELETE', `/environments/${enc(env)}`),
  renameEnvironment: (env, name) => projectCall('PATCH', `/environments/${enc(env)}`, { name }),
  // A revert makes a new deployment with deployment n's spec, against
  // base, the latest deployment's number.
  revertDeployment: (env, n, { base, message } = {}) =>
    projectCall('POST', `/environments/${enc(env)}/deployments/${enc(n)}/revert`, message ? { base, message } : { base }),
  listTags: (env) => projectCall('GET', `/environments/${enc(env)}/tags`),
  putTag: (env, tag, deployment) => projectCall('PUT', `/environments/${enc(env)}/tags/${enc(tag)}`, { deployment }),
  deleteTag: (env, tag) => projectCall('DELETE', `/environments/${enc(env)}/tags/${enc(tag)}`),
  // A clone: {name, deployment?, labels?, secrets: {name: {copy: true} | {value}},
  // credentials: {name: {copy: true}}} — the HTTP secrets go under
  // credentials, their name of before ADR 0055, which ferund keeps.
  cloneEnvironment: (env, body) => projectCall('POST', `/environments/${enc(env)}/clone`, body),
  deleteSecret: (env, name) => projectCall('DELETE', `/environments/${enc(env)}/secrets/${enc(name)}`),
  restartService: (env, name) => projectCall('POST', `/environments/${enc(env)}/services/${enc(name)}/restart`),
  runJob: (env, name) => projectCall('POST', `/environments/${enc(env)}/jobs/${enc(name)}/run`),
  checkBinding: (env, name, probe) =>
    projectCall('POST', `/environments/${enc(env)}/bindings/${enc(name)}/check`, probe ? { probe } : {}),
  // Protection (ADR 0060): a protected environment's changes take a grant
  // on it or admin on the project; a project grant of write reads it.
  // Answers {id, name, protected, changed}.
  setProtected: (env, isProtected) => projectCall('PUT', `/environments/${enc(env)}/protected`, { protected: !!isProtected }),
  // Who changes the environment today, by which grant: {principals: [{kind
  // person|token|integration, id, label, projectRole, environmentRole,
  // losesWrite}]}.
  envPrincipals: (env) => projectCall('GET', `/environments/${enc(env)}/principals`),
  // The route door's token (write on the environment, ADR 0056): a mint
  // answers it once ({token}); one delete revokes every one.
  mintRouteToken: (env) => projectCall('POST', `/environments/${enc(env)}/route-tokens`),
  revokeRouteTokens: (env) => projectCall('DELETE', `/environments/${enc(env)}/route-tokens`),
  // Publish or unpublish at the ingress (ADR 0054): a publish with an
  // optional host label and who the ingress admits, access "anyone" or
  // "visitors" of a provider ("ferun" or one of the project's); an
  // unpublish is {public: false} alone. The console says the access on
  // every publish: ferund keeps a public environment's access when a
  // publish says none, and admits visitors of ferun on the first one.
  // Answers {public, hosts, rolling, access, provider}; an older ferund
  // answers no access, and serves the environment to anyone.
  setPublic: (env, isPublic, { hostLabel = '', access = '', provider = '' } = {}) => {
    if (!isPublic) return projectCall('PUT', `/environments/${enc(env)}/public`, { public: false });
    const body = { public: true };
    if (hostLabel) body.hostLabel = hostLabel;
    if (access) body.access = access;
    if (access === 'visitors' && provider) body.provider = provider;
    return projectCall('PUT', `/environments/${enc(env)}/public`, body);
  },
  // The ingress's records of an environment, newest first, and its
  // traffic per service over window (1h or 24h).
  accessLogs: (env, { limit = 200, beforeTsNs, traceId } = {}) =>
    projectCall('GET', `/environments/${enc(env)}/access-logs${query({ limit, beforeTsNs, traceId })}`),
  traffic: (env, window = '1h') => projectCall('GET', `/environments/${enc(env)}/traffic${query({ window })}`),
  // What the environment's pods sent through each HTTP binding over
  // window (24h or 1h) and who sent it, as the sink counts it ({window,
  // unavailable?, truncated, bindings}); and who its workloads are to
  // the platform: each one's SPIFFE ID and the certificates its pods
  // hold ({trustDomain, workloads}, ADR 0036).
  bindingUsage: (env, window = '24h') =>
    projectCall('GET', `/environments/${enc(env)}/bindings/usage${query({ window })}`),
  identities: (env) => projectCall('GET', `/environments/${enc(env)}/identities`),

  // The project's domains (ADR 0021): a claim answers the records to set;
  // a domain is bound to one environment, or none.
  listDomains: (params = {}) => projectCall('GET', '/domains' + query(params)),
  getDomain: (id) => projectCall('GET', `/domains/${enc(id)}`),
  claimDomain: (name) => projectCall('POST', '/domains', { name }),
  deleteDomain: (id) => projectCall('DELETE', `/domains/${enc(id)}`),
  checkDomain: (id) => projectCall('POST', `/domains/${enc(id)}/check`),
  bindDomain: (id, environment) => projectCall('PUT', `/domains/${enc(id)}/environment`, { environment }),
  unbindDomain: (id) => projectCall('DELETE', `/domains/${enc(id)}/environment`),

  // The jobs made by a call (POST /api/v1/jobs, ADR 0041): runs to
  // completion, each in an environment (ADR 0035). The list of one
  // environment (its name or ID) is filtered here too, since a ferund may
  // list them all.
  listJobs: async (labels = [], environment = '') => {
    const q = labelQuery(labels);
    const res = await projectCall('GET', '/jobs' + q + (environment ? `${q ? '&' : '?'}environment=${enc(environment)}` : ''));
    if (!environment) return res;
    const jobs = (res.jobs || []).filter((j) => {
      const e = j && j.environment;
      return !!e && (e.id === environment || e.name === environment);
    });
    return { ...res, jobs };
  },
  getJob: (id) => projectCall('GET', `/jobs/${enc(id)}`),
  createJob: (body) => projectCall('POST', '/jobs', body),
  jobLogs: (id) => projectCall('GET', `/jobs/${enc(id)}/logs`),
  cancelJob: (id) => projectCall('POST', `/jobs/${enc(id)}/cancel`),
  deleteJob: (id) => projectCall('DELETE', `/jobs/${enc(id)}`),

  // The integrations registered here, for a person who grants one (ADR
  // 0056): {integrations: [{name, title, url}]}, the enabled ones.
  listIntegrations: () => sessionCall('GET', '/integrations'),
  // Admin: the integrations, each {name, title, url, issuer, subject,
  // disabled, disabledAt?, createdAt, updatedAt, lastSeen?, madeBy,
  // projects}; a put registers or changes one (issuer '' : the
  // installation's provider).
  adminListIntegrations: () => adminCall('GET', '/integrations'),
  adminPutIntegration: (name, { title = '', url = '', issuer = '', subject = '' }) =>
    adminCall('PUT', `/integrations/${enc(name)}`, { title, url, ...(issuer ? { issuer } : {}), subject }),
  adminDisableIntegration: (name) => adminCall('POST', `/integrations/${enc(name)}/disable`),
  adminEnableIntegration: (name) => adminCall('POST', `/integrations/${enc(name)}/enable`),
  adminDeleteIntegration: (name) => adminCall('DELETE', `/integrations/${enc(name)}`),

  // Accounts (ADR 0059), for a person: theirs ({accounts} as /auth/me),
  // one's page ({name, title, reference, closed, role, members, projects,
  // moves}), its members' roles, and its invitations ({role}, a new one's
  // token and url shown once).
  listAccounts: () => sessionCall('GET', '/accounts'),
  getAccount: (a) => sessionCall('GET', `/accounts/${enc(a)}`),
  putAccountMember: (a, person, role) => sessionCall('PUT', `/accounts/${enc(a)}/members/${enc(person)}`, { role }),
  deleteAccountMember: (a, person) => sessionCall('DELETE', `/accounts/${enc(a)}/members/${enc(person)}`),
  listAccountInvitations: (a) => sessionCall('GET', `/accounts/${enc(a)}/invitations`),
  createAccountInvitation: (a, { role }) => sessionCall('POST', `/accounts/${enc(a)}/invitations`, { role }),
  revokeAccountInvitation: (a, id) => sessionCall('DELETE', `/accounts/${enc(a)}/invitations/${enc(id)}`),
  // Admin: every account ({name, title, reference, createdAt, madeBy,
  // closed, closedAt?, projects, members}), a new one, one's page, a
  // change (title, reference, closed), a deletion (one that never held a
  // project), and its members.
  adminListAccounts: () => adminCall('GET', '/accounts'),
  adminCreateAccount: ({ name, title = '', reference = '' }) =>
    adminCall('POST', '/accounts', { name, ...(title ? { title } : {}), ...(reference ? { reference } : {}) }),
  adminGetAccount: (a) => adminCall('GET', `/accounts/${enc(a)}`),
  adminPatchAccount: (a, patch) => adminCall('PATCH', `/accounts/${enc(a)}`, patch),
  adminDeleteAccount: (a) => adminCall('DELETE', `/accounts/${enc(a)}`),
  adminPutAccountMember: (a, person, role) => adminCall('PUT', `/accounts/${enc(a)}/members/${enc(person)}`, { role }),
  adminDeleteAccountMember: (a, person) => adminCall('DELETE', `/accounts/${enc(a)}/members/${enc(person)}`),

  // Admin: the ingresses, the people, a project's limits and zone.
  listIngresses: () => adminCall('GET', '/ingresses'),
  removeIngress: (name) => adminCall('DELETE', `/ingresses/${enc(name)}`),
  listPeople: () => adminCall('GET', '/people'),
  disablePerson: (person) => adminCall('POST', `/people/${enc(person)}/disable`),
  enablePerson: (person) => adminCall('POST', `/people/${enc(person)}/enable`),
  setProjectLimits: (name, limits) => adminCall('PUT', `/projects/${enc(name)}/limits`, limits),
  setProjectZone: (name, zone) => adminCall('PUT', `/projects/${enc(name)}/zone`, { zone }),

  // The addons' catalog; null from a ferund that has none (404).
  addonCatalog: async () => {
    try {
      return await projectCall('GET', '/addons');
    } catch (e) {
      if (e.status === 404 || e.status === 405) return null;
      throw e;
    }
  },

  // Platform: an operator mints a project's token with a role (ferund's
  // default: admin), on the project or on one environment (name or ID).
  mintProjectToken: (name, { label = '', role = '', environment = '' } = {}) =>
    adminCall('POST', `/projects/${enc(name)}/tokens`,
      { label, ...(role ? { role } : {}), ...(environment ? { environment } : {}) }),

  // The project's access providers (ADR 0054), who vouch for the visitors
  // of an environment that admits visitors: kind project {url}, the
  // project's own service; kind oidc {issuer, clientId, clientSecret,
  // rule}, an issuer ferun-auth signs in with; ferun, built in, every
  // project's without being made. The list answers {providers (ferun
  // first), kinds (those this installation serves)}; a provider of kind
  // oidc names the redirect URI and back-channel URI its issuer
  // registers, its own (none without ferun-auth). A read answers
  // the client secret {"set": true}; a put sends the whole provider,
  // {"set": true} keeping the stored secret (201 made, 200 changed or
  // unchanged); a delete of one an environment names is 409
  // provider_in_use. A ferund without them (an older one, or one without
  // --secret-key-file) answers 404 "no such route" (notAvailable).
  listProviders: () => projectCall('GET', '/providers'),
  getProvider: (name) => projectCall('GET', `/providers/${enc(name)}`),
  putProvider: (name, provider) => projectCall('PUT', `/providers/${enc(name)}`, provider),
  deleteProvider: (name) => projectCall('DELETE', `/providers/${enc(name)}`),
  // The HTTP secrets a sandbox names, of its environment (ADR 0055); the
  // whole list on a put, which reaches its running pod at once.
  getSandboxSecrets: (sandboxId) => projectCall('GET', `/sandboxes/${enc(sandboxId)}/secrets`),
  putSandboxSecrets: (sandboxId, names) =>
    projectCall('PUT', `/sandboxes/${enc(sandboxId)}/secrets`, { secrets: names }),
};

// mcpURL is the MCP door of this origin, which the UI shows to connect
// agents.
export function mcpURL() {
  return `${globalThis.location ? globalThis.location.origin : ''}/mcp`;
}
