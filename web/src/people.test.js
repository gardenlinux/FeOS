// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  atLeast, can, envRole, mayBuild, fmtQuota, grantableRoles, higher, inviteLink, myProjects, quotaRows, roleLabel, projectAccess,
  projectHome, projectNav, viewGate,
} from './people.js';
import { parsePath } from './routes.js';

// What /auth/me decides in the UI (ADR 0031): the operator's pages for
// operators only, a person's projects with what each role offers.

const OPERATOR = {
  sub: 'op', person: 'p-op', admin: true, method: 'oidc',
  projects: [{ project: 'fecode', role: 'admin', environments: [] }, { project: 'acme', role: 'admin', environments: [] }],
};
const BASIC = { username: 'malte', admin: true, method: 'basic' };
const ANNA = {
  sub: 'anna', person: 'p-anna', admin: false, method: 'oidc',
  projects: [
    { project: 'zeta', role: 'read', environments: [{ id: 'e-z1', name: 'stage', role: 'write' }] },
    { project: 'acme', role: 'admin', environments: [] },
    { project: 'shop', role: '', environments: [{ id: 'e-s1', name: 'shop-prod', role: 'read' }] },
  ],
};

describe('the builder', () => {
  it('changes a new environment with write on the project, one environment with write on it', () => {
    const zeta = projectAccess(ANNA, 'zeta');
    expect(mayBuild(zeta, '')).toBe(false);
    expect(mayBuild(zeta, 'stage')).toBe(true);
    expect(mayBuild(zeta, 'e-z1')).toBe(true);
    expect(mayBuild(zeta, 'prod')).toBe(false);
    const shop = projectAccess(ANNA, 'shop');
    expect(mayBuild(shop, 'shop-prod')).toBe(false);
    expect(mayBuild(shop, '')).toBe(false);
    const acme = projectAccess(ANNA, 'acme');
    expect(mayBuild(acme, '')).toBe(true);
    expect(mayBuild(acme, 'anything')).toBe(true);
    expect(mayBuild(projectAccess(null, 'acme'), 'x')).toBe(false);
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
  const offers = (role) => can({ project: 't', member: true, role, environments: [], operator: false });

  it('read: the project\'s lists, no change', () => {
    expect(offers('read')).toMatchObject({ readProject: true, writeProject: false, adminProject: false, members: true, leave: true, operator: false });
  });

  it('write: sandboxes and environments; admin: members, invitations and tokens', () => {
    expect(offers('write')).toMatchObject({ readProject: true, writeProject: true, adminProject: false });
    expect(offers('admin')).toMatchObject({ readProject: true, writeProject: true, adminProject: true });
  });

  it('grants on environments alone: their environments and the members, no project lists', () => {
    expect(offers('')).toMatchObject({ readProject: false, writeProject: false, environments: true, members: true });
  });

  it('a non-member is offered nothing', () => {
    expect(Object.values(can(projectAccess(ANNA, 'other'))).some(Boolean)).toBe(false);
  });

  it('an operator is admin in every project, sees the activity, and leaves none', () => {
    const a = projectAccess(OPERATOR, 'any-project');
    expect(a).toMatchObject({ member: true, role: 'admin', operator: true });
    expect(can(a)).toMatchObject({ adminProject: true, operator: true, leave: false });
  });

  it('the role on an environment is the higher of the project\'s and the environment\'s', () => {
    const z = projectAccess(ANNA, 'zeta');
    expect(envRole(z, 'stage')).toBe('write');
    expect(envRole(z, 'e-z1')).toBe('write');
    expect(envRole(z, 'prod')).toBe('read');
    expect(envRole(projectAccess(ANNA, 'shop'), 'other')).toBe('');
  });
});

describe('me → navigation', () => {
  it('lists a person\'s projects by name, with the role or "environments"', () => {
    expect(myProjects(ANNA).map((t) => [t.project, roleLabel(t)])).toEqual([
      ['acme', 'admin'], ['shop', 'environments'], ['zeta', 'read'],
    ]);
    expect(myProjects(null)).toEqual([]);
  });

  it('a project\'s pages by role', () => {
    const views = (me, t) => projectNav(projectAccess(me, t)).map((n) => n.view);
    expect(views(ANNA, 'acme')).toEqual(['overview', 'environments', 'domains', 'providers', 'members', 'tokens', 'usage', 'settings']);
    expect(views(ANNA, 'zeta')).toEqual(['overview', 'environments', 'domains', 'providers', 'members', 'usage', 'settings']);
    // Grants on environments alone: their sandboxes and jobs are in them
    // (ADR 0035), no page of the project's; the access providers (ADR
    // 0054), which an environment's admin chooses among.
    expect(views(ANNA, 'shop')).toEqual(['overview', 'environments', 'domains', 'providers', 'members', 'usage', 'settings']);
    expect(views(ANNA, 'nope')).toEqual([]);
    expect(views(BASIC, 'acme')).toEqual([]);
  });

  it('a project opens at its overview, or at the environments for grants on environments alone', () => {
    expect(projectHome(projectAccess(ANNA, 'acme'))).toBe('/projects/acme');
    expect(projectHome(projectAccess(ANNA, 'shop'))).toBe('/projects/shop/environments');
  });

  it('the operators\' pages are for operators only', () => {
    for (const p of ['/nodes', '/nodes/n-1/certs', '/join-tokens', '/instances']) {
      expect(viewGate(ANNA, parsePath(p))).toBe('operator');
      expect(viewGate(OPERATOR, parsePath(p))).toBe('');
      expect(viewGate(BASIC, parsePath(p))).toBe('');
    }
  });

  it('a project\'s pages need a membership and the role of the page', () => {
    expect(viewGate(ANNA, parsePath('/projects/acme/tokens'))).toBe('');
    expect(viewGate(ANNA, parsePath('/projects/zeta/tokens'))).toBe('role');
    expect(viewGate(ANNA, parsePath('/projects/zeta/sandboxes/abc'))).toBe('');
    expect(viewGate(ANNA, parsePath('/projects/shop/sandboxes'))).toBe('');
    expect(viewGate(ANNA, parsePath('/projects/shop/tools'))).toBe('role');
    expect(viewGate(ANNA, parsePath('/projects/shop/environments/shop-prod/logs'))).toBe('');
    expect(viewGate(ANNA, parsePath('/projects/shop'))).toBe('');
    expect(viewGate(ANNA, parsePath('/projects/other'))).toBe('member');
    expect(viewGate(OPERATOR, parsePath('/projects/brand-new/members'))).toBe('');
    // The credentials (ADR 0033) are an environment's: its pages, with
    // a grant on the environment alone too.
    expect(viewGate(ANNA, parsePath('/projects/shop/environments/shop-prod/credentials/github'))).toBe('');
    expect(viewGate(ANNA, parsePath('/projects/shop/environments/shop-prod/credentials/new'))).toBe('');
    // The access providers (ADR 0054): every member reads them; a new
    // one is the project admin's.
    expect(viewGate(ANNA, parsePath('/projects/shop/providers'))).toBe('');
    expect(viewGate(ANNA, parsePath('/projects/shop/providers/fecode/edit'))).toBe('');
    expect(viewGate(ANNA, parsePath('/projects/acme/new-provider'))).toBe('');
    expect(viewGate(ANNA, parsePath('/projects/zeta/new-provider'))).toBe('role');
    expect(viewGate(ANNA, parsePath('/projects/shop/new-provider'))).toBe('role');
  });

  it('a Basic sign-in has the operators\' pages but acts in no project', () => {
    expect(viewGate(BASIC, parsePath('/projects/acme'))).toBe('basic');
    expect(viewGate(BASIC, parsePath('/projects'))).toBe('');
  });

  it('the projects list and an invitation are open to every sign-in', () => {
    expect(viewGate(ANNA, parsePath('/projects'))).toBe('');
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
