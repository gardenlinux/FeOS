// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// People in the UI (ADR 0031): what /auth/me says decides what the UI
// offers. An operator sees the operator's pages and acts in every tenant
// as its admin; a person sees their tenants, and in a tenant what their
// roles allow. The UI offers only that; ferund decides anyway (403
// role_required, 404 for what one may not see).

import { createContext, useContext, useMemo } from 'react';

// MeContext carries /auth/me to the views, and reload, which asks it
// again (after a new tenant, an accepted invitation, a tenant left).
export const MeContext = createContext({ me: null, reload: async () => {} });

export function useMe() {
  return useContext(MeContext);
}

// useAccess is the sign-in's access to tenant and what it offers there.
export function useAccess(tenant) {
  const { me } = useMe();
  return useMemo(() => {
    const access = tenantAccess(me, tenant);
    return { ...access, can: can(access) };
  }, [me, tenant]);
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

// actsInTenants tells whether the sign-in can act in tenants: a session
// of the provider. A Basic sign-in (--admin-users) is an operator's
// without a person, which the tenant routes do not take.
export function actsInTenants(me) {
  return !!me && me.method !== 'basic';
}

// myTenants are /auth/me's tenants, sorted by name.
export function myTenants(me) {
  return [...((me && me.tenants) || [])]
    .filter((t) => t && t.tenant)
    .sort((a, b) => a.tenant.localeCompare(b.tenant));
}

// tenantAccess is the sign-in's access to a tenant: whether it may enter
// at all (member), its role on the tenant ('' with grants on
// environments alone), its grants on environments, and whether it is an
// operator's (admin in every tenant, as ferund has it).
export function tenantAccess(me, name) {
  const none = { tenant: name, member: false, role: '', environments: [], operator: false };
  if (!me || !name || !actsInTenants(me)) return none;
  const t = myTenants(me).find((x) => x.tenant === name);
  if (isOperator(me)) {
    return { tenant: name, member: true, role: 'admin', environments: (t && t.environments) || [], operator: true };
  }
  if (!t) return none;
  return { tenant: name, member: true, role: t.role || '', environments: t.environments || [], operator: false };
}

// envRole is the role on an environment (its name or ID): the higher of
// the tenant grant and the grant on it.
export function envRole(access, env) {
  const g = ((access && access.environments) || []).find((e) => e.id === env || e.name === env);
  return higher((access && access.role) || '', g ? g.role : '');
}

// can is what a tenant's pages offer for an access, by the routes' needs
// (go/internal/api: the route table).
export function can(access) {
  const role = (access && access.role) || '';
  const member = !!(access && access.member);
  return {
    // A tenant grant read: sandboxes, tools, jobs, the whole list of
    // environments.
    readTenant: atLeast(role, 'read'),
    // write: sandboxes and their tokens, environments made or deleted.
    writeTenant: atLeast(role, 'write'),
    // admin: members' grants, invitations, tokens, the domains' claims.
    adminTenant: atLeast(role, 'admin'),
    // Any member sees the environments it may read and the members.
    environments: member,
    members: member,
    // A person leaves a tenant they hold grants in; an operator is no
    // member to leave.
    leave: member && !(access && access.operator),
    // The activity log and the nodes are the operators' (admin routes).
    operator: !!(access && access.operator),
  };
}

// TENANT_NAV are a tenant's pages in the sidebar, with what each needs
// of can(): the overview and the environments for every member, the
// sandboxes and tools with a tenant grant, the tokens for its admin.
export const TENANT_NAV = [
  { view: 'overview', label: 'Overview', icon: 'dashboard', path: '', needs: 'members' },
  { view: 'sandboxes', label: 'Sandboxes', icon: 'box', path: '/sandboxes', needs: 'readTenant' },
  { view: 'environments', label: 'Environments', icon: 'blocks', path: '/environments', needs: 'environments',
    views: ['environments', 'newEnvironment'] },
  { view: 'tools', label: 'Tools', icon: 'terminal', path: '/tools', needs: 'readTenant' },
  { view: 'members', label: 'Members', icon: 'users', path: '/members', needs: 'members' },
  { view: 'tokens', label: 'Tokens', icon: 'key', path: '/tokens', needs: 'adminTenant' },
];

// VIEW_NEEDS are what the tenant's pages outside the sidebar need: a
// new environment's builder (ADR 0032) a tenant grant write.
const VIEW_NEEDS = { newEnvironment: 'writeTenant' };

// tenantNav is the tenant's pages that access offers.
export function tenantNav(access) {
  const c = can(access);
  return TENANT_NAV.filter((n) => c[n.needs]);
}

// OPERATOR_VIEWS are the operators' pages.
export const OPERATOR_VIEWS = new Set(['nodes', 'node', 'joinTokens', 'instances']);

// viewGate decides a route for a sign-in: '' when the view may show,
// 'operator' for an operator's page a person opened, 'basic' for a
// tenant's page under a Basic sign-in, 'member' for a tenant the sign-in
// is no member of (or that does not exist), 'role' for a tenant's page
// its role does not offer.
export function viewGate(me, route) {
  if (!route) return '';
  if (OPERATOR_VIEWS.has(route.view)) return isOperator(me) ? '' : 'operator';
  if (!route.tenant) return '';
  if (!actsInTenants(me)) return 'basic';
  const access = tenantAccess(me, route.tenant);
  if (!access.member) return 'member';
  const nav = TENANT_NAV.find((n) => n.view === route.view);
  const needs = nav ? nav.needs : VIEW_NEEDS[route.view];
  if (needs && !can(access)[needs]) return 'role';
  return '';
}

// roleLabel names a tenant's role for a person: "admin", or
// "environments" for grants on environments alone.
export function roleLabel(t) {
  if (!t) return '';
  if (t.role) return t.role;
  return (t.environments || []).length ? 'environments' : '';
}

// tenantHome is where a tenant opens: its overview, or for a person with
// grants on environments alone the environments.
export function tenantHome(access) {
  const enc = encodeURIComponent;
  if (access && access.member && !access.role) return `/tenants/${enc(access.tenant)}/environments`;
  return `/tenants/${enc((access && access.tenant) || '')}`;
}

// grantableRoles are the roles a principal may hand out: never above its
// own (a token, an invitation, a member's grant).
export function grantableRoles(role) {
  return ROLES.filter((r) => rank(r) <= rank(role));
}

// inviteLink is an invitation's link: ferund's url when it answers one,
// else /invite/<token> on this origin.
export function inviteLink(origin, inv) {
  if (!inv) return '';
  if (inv.url) return inv.url;
  return inv.token ? `${origin}/invite/${encodeURIComponent(inv.token)}` : '';
}

// QUOTA_ITEMS are the limits of a tenant (ADR 0031, "Quotas per tenant").
export const QUOTA_ITEMS = [
  { key: 'sandboxes', label: 'sandboxes' },
  { key: 'environments', label: 'environments' },
  { key: 'runningJobs', label: 'jobs running' },
  { key: 'vcpus', label: 'vCPUs' },
  { key: 'memoryMib', label: 'memory', unit: 'MiB' },
];

// quotaRows is the use against the limits: per item the use, the limit
// (0: none), the share (null without a limit) and a tone: warn from 80%,
// bad at the limit.
export function quotaRows(q) {
  const limits = (q && q.limits) || {};
  const use = (q && q.use) || {};
  return QUOTA_ITEMS.map((it) => {
    const u = Number(use[it.key]) || 0;
    const l = Number(limits[it.key]) || 0;
    const share = l > 0 ? u / l : null;
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
  if (!environment) return 'the tenant';
  const e = (envs || []).find((x) => x.id === environment || x.name === environment);
  return `environment ${e ? e.name : environment}`;
}
