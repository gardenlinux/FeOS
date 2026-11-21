// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// People in the UI (ADR 0031): what /auth/me says decides what the UI
// offers. An operator sees the operator's pages and acts in every project
// as its admin; a person sees their projects, and in a project what their
// roles allow. The UI offers only that; ferund decides anyway (403
// role_required, 404 for what one may not see).

import { createContext, useContext, useMemo } from 'react';

// MeContext carries /auth/me to the views, and reload, which asks it
// again (after a new project, an accepted invitation, a project left).
export const MeContext = createContext({ me: null, reload: async () => {} });

export function useMe() {
  return useContext(MeContext);
}

// useAccess is the sign-in's access to project and what it offers there.
export function useAccess(project) {
  const { me } = useMe();
  return useMemo(() => {
    const access = projectAccess(me, project);
    return { ...access, can: can(access) };
  }, [me, project]);
}

// ROLES are the roles of a grant, each including the one before.
export const ROLES = ['read', 'write', 'admin'];

// rank is a role's place: 0 for none ('' or unknown).
export function rank(role) {
  return ROLES.indexOf(role) + 1;
}

// atLeast tells whether have includes need.
export function atLeast(have, need) {
  return rank(have) >= rank(need) && rank(need) > 0;
}

// higher is the higher of two roles ('' for none).
export function higher(a, b) {
  return rank(a) >= rank(b) ? (rank(a) ? a : '') : b;
}

// isOperator tells an operator's sign-in.
export function isOperator(me) {
  return !!(me && me.admin);
}

// actsInProjects tells whether the sign-in can act in projects: a session
// of the provider. A Basic sign-in (--admin-users) is an operator's
// without a person, which the project routes do not take.
export function actsInProjects(me) {
  return !!me && me.method !== 'basic';
}

// myProjects are /auth/me's projects, sorted by name.
export function myProjects(me) {
  return [...((me && me.projects) || [])]
    .filter((t) => t && t.project)
    .sort((a, b) => a.project.localeCompare(b.project));
}

// projectAccess is the sign-in's access to a project: whether it may enter
// at all (member), its role on the project ('' with grants on
// environments alone), its grants on environments, and whether it is an
// operator's (admin in every project, as ferund has it).
export function projectAccess(me, name) {
  const none = { project: name, member: false, role: '', environments: [], operator: false };
  if (!me || !name || !actsInProjects(me)) return none;
  const t = myProjects(me).find((x) => x.project === name);
  if (isOperator(me)) {
    return { project: name, member: true, role: 'admin', environments: (t && t.environments) || [], operator: true };
  }
  if (!t) return none;
  return { project: name, member: true, role: t.role || '', environments: t.environments || [], operator: false };
}

// envRole is the role on an environment (its name or ID): the higher of
// the project grant and the grant on it.
export function envRole(access, env) {
  const g = ((access && access.environments) || []).find((e) => e.id === env || e.name === env);
  return higher((access && access.role) || '', g ? g.role : '');
}

// mayBuild tells whether the deployment builder may change env (its ID
// or name; '' for a new environment, ADR 0032): a new one needs write on
// the project, a deployment or a secret's value write on the environment
// (envRole). Below that the builder is a read-only view.
export function mayBuild(access, env) {
  if (!env) return can(access).writeProject;
  return atLeast(envRole(access, env), 'write');
}

// can is what a project's pages offer for an access, by the routes' needs
// (go/internal/api: the route table).
export function can(access) {
  const role = (access && access.role) || '';
  const member = !!(access && access.member);
  return {
    // A project grant read: tools, the whole list of
    // environments. Sandboxes and jobs are their environment's (ADR
    // 0035): a member lists those it may read.
    readProject: atLeast(role, 'read'),
    // write: environments made or deleted.
    writeProject: atLeast(role, 'write'),
    // admin: members' grants, invitations, tokens, the domains' claims,
    // the access providers (ADR 0054).
    adminProject: atLeast(role, 'admin'),
    // Any member sees the environments it may read and the members.
    environments: member,
    members: member,
    // A person leaves a project they hold grants in; an operator is no
    // member to leave.
    leave: member && !(access && access.operator),
    // The nodes and the platform's pages are the operators' (admin
    // routes).
    operator: !!(access && access.operator),
  };
}

// PROJECT_NAV are a project's pages, in their groups, each with what it
// needs of can(). The sidebar shows them while none of an environment's
// pages is open (env: the environments, project: the project's own things;
// manage: one entry, Project settings); in an environment it shows the
// environment with its objects, its sandboxes and its jobs (ADR 0035:
// everything runs in an environment), and the project as one entry. Home
// and the environments for every member (each sees those it may read),
// the tokens for its admin. The access providers (ADR 0054) for every
// member, since an environment's admin chooses among them; making one
// is the project admin's.
export const PROJECT_NAV = [
  { view: 'overview', label: 'Home', icon: 'home', path: '', needs: 'members', group: '' },
  { view: 'environments', label: 'Environments', icon: 'layers', path: '/environments', needs: 'environments', group: 'env', views: ['environments', 'newEnvironment'] },
  { view: 'domains', label: 'Domains', icon: 'globe', path: '/domains', needs: 'members', group: 'project' },
  { view: 'providers', label: 'Access providers', icon: 'lock', path: '/providers', needs: 'members', group: 'project', views: ['providers', 'newProvider'] },
  { view: 'members', label: 'People', icon: 'users', path: '/members', needs: 'members', group: 'manage' },
  { view: 'tokens', label: 'Tokens', icon: 'key', path: '/tokens', needs: 'adminProject', group: 'manage' },
  { view: 'usage', label: 'Usage', icon: 'gauge', path: '/usage', needs: 'members', group: 'manage' },
  { view: 'settings', label: 'General', icon: 'settings', path: '/settings', needs: 'members', group: 'manage' },
];

// SETTINGS_NAV is the sidebar's one entry for the project's settings
// (the group manage): its pages are tabs of one another.
export const SETTINGS_NAV = { view: 'projectSettings', label: 'Project settings', icon: 'settings' };

// PLATFORM_NAV are the operators' pages, the platform's space.
export const PLATFORM_NAV = [
  { view: 'nodes', label: 'Nodes', icon: 'server', path: '/nodes', group: 'fleet', views: ['nodes', 'node'] },
  { view: 'ingresses', label: 'Ingresses', icon: 'route', path: '/ingresses', group: 'fleet' },
  { view: 'instances', label: 'Control plane', icon: 'network', path: '/instances', group: 'fleet' },
  { view: 'joinTokens', label: 'Join tokens', icon: 'ticket', path: '/join-tokens', group: 'fleet' },
  { view: 'projects', label: 'Projects', icon: 'building', path: '/projects', group: 'accounts' },
  { view: 'people', label: 'People', icon: 'users', path: '/people', group: 'accounts' },
];

export const PLATFORM_GROUPS = [
  { key: 'fleet', label: 'Fleet' },
  { key: 'accounts', label: 'Accounts' },
];

// VIEW_NEEDS are what the project's pages outside the sidebar need: a
// new environment's builder (ADR 0032) a project grant write, a new access
// provider's form (ADR 0054) admin on the project, the tools' reference a
// project grant. An HTTP secret is its environment's (ADR 0055): its
// pages need the environment's grant, as the environment's other pages do.
const VIEW_NEEDS = { newEnvironment: 'writeProject', newProvider: 'adminProject', tools: 'readProject' };

// projectNav is the project's pages that access offers.
export function projectNav(access) {
  const c = can(access);
  return PROJECT_NAV.filter((n) => c[n.needs]);
}

// OPERATOR_VIEWS are the operators' pages.
export const OPERATOR_VIEWS = new Set(['nodes', 'node', 'joinTokens', 'instances', 'ingresses', 'people']);

// viewGate decides a route for a sign-in: '' when the view may show,
// 'operator' for an operator's page a person opened, 'basic' for a
// project's page under a Basic sign-in, 'member' for a project the sign-in
// is no member of (or that does not exist), 'role' for a project's page
// its role does not offer.
export function viewGate(me, route) {
  if (!route) return '';
  if (OPERATOR_VIEWS.has(route.view)) return isOperator(me) ? '' : 'operator';
  if (!route.project) return '';
  if (!actsInProjects(me)) return 'basic';
  const access = projectAccess(me, route.project);
  if (!access.member) return 'member';
  const nav = PROJECT_NAV.find((n) => n.view === route.view);
  const needs = nav ? nav.needs : VIEW_NEEDS[route.view];
  if (needs && !can(access)[needs]) return 'role';
  return '';
}

// roleLabel names a project's role for a person: "admin", or
// "environments" for grants on environments alone.
export function roleLabel(t) {
  if (!t) return '';
  if (t.role) return t.role;
  return (t.environments || []).length ? 'environments' : '';
}

// projectHome is where a project opens: its overview, or for a person with
// grants on environments alone the environments.
export function projectHome(access) {
  const enc = encodeURIComponent;
  if (access && access.member && !access.role) return `/projects/${enc(access.project)}/environments`;
  return `/projects/${enc((access && access.project) || '')}`;
}

// grantableRoles are the roles a principal may hand out: never above its
// own (a token, an invitation, a member's grant).
export function grantableRoles(role) {
  return ROLES.filter((r) => rank(r) <= rank(role));
}

// inviteLink is an invitation's link: ferund's url (<public URL>/invite/
// <token>, ADR 0031 D4; a path when ferund has no public URL, made
// absolute on this origin), else /invite/<token> on this origin.
export function inviteLink(origin, inv) {
  if (!inv) return '';
  if (inv.url) return inv.url.startsWith('/') ? `${origin}${inv.url}` : inv.url;
  return inv.token ? `${origin}/invite/${encodeURIComponent(inv.token)}` : '';
}

// QUOTA_ITEMS are the limits of a project (ADR 0031, "Quotas per project").
export const QUOTA_ITEMS = [
  { key: 'sandboxes', label: 'sandboxes' },
  { key: 'environments', label: 'environments' },
  { key: 'runningJobs', label: 'jobs running' },
  { key: 'vcpus', label: 'vCPUs' },
  { key: 'memoryMib', label: 'memory', unit: 'MiB' },
];

// quotaRows is the use against the limits: per item the use, the limit
// (null when ferund leaves it out: none; 0 is a limit, ADR 0031), the
// share (null without a limit; a limit of 0 is full) and a tone: warn
// from 80%, bad at the limit.
export function quotaRows(q) {
  const limits = (q && q.limits) || {};
  const use = (q && q.use) || {};
  return QUOTA_ITEMS.map((it) => {
    const u = Number(use[it.key]) || 0;
    const raw = limits[it.key];
    const l = raw === undefined || raw === null || raw === '' || !Number.isFinite(Number(raw)) ? null : Number(raw);
    let share = null;
    if (l !== null) share = l > 0 ? u / l : 1;
    let tone = '';
    if (share !== null && share >= 1) tone = 'bad';
    else if (share !== null && share >= 0.8) tone = 'warn';
    return { ...it, use: u, limit: l, share, tone };
  });
}

// fmtQuota renders a use or a limit: memory in GiB from 1024 MiB.
export function fmtQuota(item, v) {
  if (item.unit === 'MiB') return v >= 1024 ? `${+(v / 1024).toFixed(1)} GiB` : `${v} MiB`;
  return String(v);
}

// personLabel names a member: the name, else the e-mail address, else
// the short ID.
export function personLabel(m) {
  if (!m) return '';
  return m.name || m.email || String(m.person || '').slice(0, 8);
}

// scopeLabel names a grant's or an invitation's scope.
export function scopeLabel(environment, envs) {
  if (!environment) return 'the project';
  const e = (envs || []).find((x) => x.id === environment || x.name === environment);
  return `environment ${e ? e.name : environment}`;
}

// invitationState is an invitation's state: used, expired or open.
export function invitationState(inv, now = Date.now()) {
  if (inv.usedAt || inv.usedBy) return 'used';
  const exp = Date.parse(inv.expiresAt || '');
  if (!Number.isNaN(exp) && exp <= now) return 'expired';
  return 'open';
}
