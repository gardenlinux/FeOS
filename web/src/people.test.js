// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  atLeast, can, envRole, mayBuild, fmtQuota, grantableRoles, higher, inviteLink, myTenants, quotaRows, roleLabel, tenantAccess,
  tenantHome, tenantNav, viewGate,
} from './people.js';
import { parsePath } from './routes.js';

// What /auth/me decides in the UI (ADR 0031): the operator's pages for
// operators only, a person's tenants with what each role offers.

const OPERATOR = {
  sub: 'op', person: 'p-op', admin: true, method: 'oidc',
  tenants: [{ tenant: 'fecode', role: 'admin', environments: [] }, { tenant: 'acme', role: 'admin', environments: [] }],
};
const BASIC = { username: 'malte', admin: true, method: 'basic' };
const ANNA = {
  sub: 'anna', person: 'p-anna', admin: false, method: 'oidc',
  tenants: [
    { tenant: 'zeta', role: 'read', environments: [{ id: 'e-z1', name: 'stage', role: 'write' }] },
    { tenant: 'acme', role: 'admin', environments: [] },
    { tenant: 'shop', role: '', environments: [{ id: 'e-s1', name: 'shop-prod', role: 'read' }] },
  ],
};

describe('the builder', () => {
  it('changes a new environment with write on the tenant, one environment with write on it', () => {
    const zeta = tenantAccess(ANNA, 'zeta');
    expect(mayBuild(zeta, '')).toBe(false);
    expect(mayBuild(zeta, 'stage')).toBe(true);
    expect(mayBuild(zeta, 'e-z1')).toBe(true);
    expect(mayBuild(zeta, 'prod')).toBe(false);
    const shop = tenantAccess(ANNA, 'shop');
    expect(mayBuild(shop, 'shop-prod')).toBe(false);
    expect(mayBuild(shop, '')).toBe(false);
    const acme = tenantAccess(ANNA, 'acme');
    expect(mayBuild(acme, '')).toBe(true);
    expect(mayBuild(acme, 'anything')).toBe(true);
    expect(mayBuild(tenantAccess(null, 'acme'), 'x')).toBe(false);
  });
});

describe('roles', () => {
  it('each includes the one before; none is below read', () => {
    expect(atLeast('admin', 'write')).toBe(true);
    expect(atLeast('write', 'write')).toBe(true);
    expect(atLeast('read', 'write')).toBe(false);
    expect(atLeast('', 'read')).toBe(false);
    expect(atLeast('bogus', 'read')).toBe(false);
    expect(higher('read', 'write')).toBe('write');
    expect(higher('admin', '')).toBe('admin');
    expect(higher('', '')).toBe('');
  });

  it('a role hands out none above itself', () => {
    expect(grantableRoles('admin')).toEqual(['read', 'write', 'admin']);
    expect(grantableRoles('write')).toEqual(['read', 'write']);
    expect(grantableRoles('')).toEqual([]);
  });
});

describe('what a role offers', () => {
  const offers = (role) => can({ tenant: 't', member: true, role, environments: [], operator: false });

  it('read: the tenant\'s lists, no change', () => {
    expect(offers('read')).toMatchObject({ readTenant: true, writeTenant: false, adminTenant: false, members: true, leave: true, operator: false });
  });

  it('write: sandboxes and environments; admin: members, invitations and tokens', () => {
    expect(offers('write')).toMatchObject({ readTenant: true, writeTenant: true, adminTenant: false });
    expect(offers('admin')).toMatchObject({ readTenant: true, writeTenant: true, adminTenant: true });
  });

  it('grants on environments alone: their environments and the members, no tenant lists', () => {
    expect(offers('')).toMatchObject({ readTenant: false, writeTenant: false, environments: true, members: true });
  });

  it('a non-member is offered nothing', () => {
    expect(Object.values(can(tenantAccess(ANNA, 'other'))).some(Boolean)).toBe(false);
  });

  it('an operator is admin in every tenant, sees the activity, and leaves none', () => {
    const a = tenantAccess(OPERATOR, 'any-tenant');
    expect(a).toMatchObject({ member: true, role: 'admin', operator: true });
    expect(can(a)).toMatchObject({ adminTenant: true, operator: true, leave: false });
  });

  it('the role on an environment is the higher of the tenant\'s and the environment\'s', () => {
    const z = tenantAccess(ANNA, 'zeta');
    expect(envRole(z, 'stage')).toBe('write');
    expect(envRole(z, 'e-z1')).toBe('write');
    expect(envRole(z, 'prod')).toBe('read');
    expect(envRole(tenantAccess(ANNA, 'shop'), 'other')).toBe('');
  });
});

describe('me → navigation', () => {
  it('lists a person\'s tenants by name, with the role or "environments"', () => {
    expect(myTenants(ANNA).map((t) => [t.tenant, roleLabel(t)])).toEqual([
      ['acme', 'admin'], ['shop', 'environments'], ['zeta', 'read'],
    ]);
    expect(myTenants(null)).toEqual([]);
  });

  it('a tenant\'s pages by role', () => {
    const views = (me, t) => tenantNav(tenantAccess(me, t)).map((n) => n.view);
    expect(views(ANNA, 'acme')).toEqual(['overview', 'environments', 'domains', 'credentials', 'members', 'tokens', 'usage', 'settings']);
    expect(views(ANNA, 'zeta')).toEqual(['overview', 'environments', 'domains', 'credentials', 'members', 'usage', 'settings']);
    // Grants on environments alone: their sandboxes and jobs are in them
    // (ADR 0035), no page of the tenant's.
    expect(views(ANNA, 'shop')).toEqual(['overview', 'environments', 'domains', 'members', 'usage', 'settings']);
    expect(views(ANNA, 'nope')).toEqual([]);
    expect(views(BASIC, 'acme')).toEqual([]);
  });

  it('a tenant opens at its overview, or at the environments for grants on environments alone', () => {
    expect(tenantHome(tenantAccess(ANNA, 'acme'))).toBe('/tenants/acme');
    expect(tenantHome(tenantAccess(ANNA, 'shop'))).toBe('/tenants/shop/environments');
  });

  it('the operators\' pages are for operators only', () => {
    for (const p of ['/nodes', '/nodes/n-1/certs', '/join-tokens', '/instances']) {
      expect(viewGate(ANNA, parsePath(p))).toBe('operator');
      expect(viewGate(OPERATOR, parsePath(p))).toBe('');
      expect(viewGate(BASIC, parsePath(p))).toBe('');
    }
  });

  it('a tenant\'s pages need a membership and the role of the page', () => {
    expect(viewGate(ANNA, parsePath('/tenants/acme/tokens'))).toBe('');
    expect(viewGate(ANNA, parsePath('/tenants/zeta/tokens'))).toBe('role');
    expect(viewGate(ANNA, parsePath('/tenants/zeta/sandboxes/abc'))).toBe('');
    expect(viewGate(ANNA, parsePath('/tenants/shop/sandboxes'))).toBe('');
    expect(viewGate(ANNA, parsePath('/tenants/shop/tools'))).toBe('role');
    expect(viewGate(ANNA, parsePath('/tenants/shop/environments/shop-prod/logs'))).toBe('');
    expect(viewGate(ANNA, parsePath('/tenants/shop'))).toBe('');
    expect(viewGate(ANNA, parsePath('/tenants/other'))).toBe('member');
    expect(viewGate(OPERATOR, parsePath('/tenants/brand-new/members'))).toBe('');
    // The credentials (ADR 0033): read on the tenant; a new one write.
    expect(viewGate(ANNA, parsePath('/tenants/zeta/credentials/github'))).toBe('');
    expect(viewGate(ANNA, parsePath('/tenants/shop/credentials'))).toBe('role');
    expect(viewGate(ANNA, parsePath('/tenants/acme/new-credential'))).toBe('');
    expect(viewGate(ANNA, parsePath('/tenants/zeta/new-credential'))).toBe('role');
  });

  it('a Basic sign-in has the operators\' pages but acts in no tenant', () => {
    expect(viewGate(BASIC, parsePath('/tenants/acme'))).toBe('basic');
    expect(viewGate(BASIC, parsePath('/tenants'))).toBe('');
  });

  it('the tenants list and an invitation are open to every sign-in', () => {
    expect(viewGate(ANNA, parsePath('/tenants'))).toBe('');
    expect(viewGate(ANNA, parsePath('/invite/s3cr3t'))).toBe('');
  });
});

describe('invitations and the quota', () => {
  it('an invitation\'s link: ferund\'s, else /invite/<token> here', () => {
    expect(inviteLink('https://run.example', { token: 'a b' })).toBe('https://run.example/invite/a%20b');
    expect(inviteLink('https://run.example', { token: 'x', url: 'https://elsewhere/invite/x' })).toBe('https://elsewhere/invite/x');
    expect(inviteLink('https://run.example', { token: 'x', url: '/invite/x' })).toBe('https://run.example/invite/x');
    expect(inviteLink('https://run.example', null)).toBe('');
  });

  it('the use against the limits, warn from 80%, bad at the limit, none without a limit', () => {
    const rows = quotaRows({
      limits: { sandboxes: 10, environments: 5, runningJobs: 0, memoryMib: 8192 },
      use: { sandboxes: 3, environments: 4, runningJobs: 0, vcpus: 8, memoryMib: '2048' },
    });
    expect(rows.map((r) => [r.key, r.use, r.limit, r.tone])).toEqual([
      ['sandboxes', 3, 10, ''],
      ['environments', 4, 5, 'warn'],
      ['runningJobs', 0, 0, 'bad'],
      ['vcpus', 8, null, ''],
      ['memoryMib', 2048, 8192, ''],
    ]);
    // 0 is a limit (nothing more may start); a limit left out is none.
    expect(rows[2].share).toBe(1);
    expect(rows[3].share).toBeNull();
    expect(fmtQuota(rows[4], 2048)).toBe('2 GiB');
    expect(fmtQuota(rows[4], 512)).toBe('512 MiB');
    expect(quotaRows(null).every((r) => r.use === 0 && r.limit === null)).toBe(true);
    expect(quotaRows({ limits: { vcpus: null } })[3].limit).toBeNull();
  });
});
