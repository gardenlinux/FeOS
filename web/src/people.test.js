// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  accountAccess, accountLabel, accountsToMakeOn, atLeast, can, envRole, integrationRoleText, madeByText, mayBuild, mayPlan, moveTargets,
  moveText, myAccounts, fmtQuota, grantableRoles, higher, inviteLink, myProjects, quotaRows, roleLabel, projectAccess, projectHome,
  projectNav, viewGate,
} from './people.js';
import { answerWith, wire } from './testing/wire.js';
import { parsePath } from './routes.js';

// What /auth/me decides in the UI (ADR 0031): the operator's pages for
// operators only, a person's projects with what each role offers.

const OPERATOR = {
  sub: 'op', person: 'p-op', admin: true, method: 'oidc',
  projects: [{ project: 'fecode', role: 'admin', environments: [] }, { project: 'acme', role: 'admin', environments: [] }],
};
const BASIC = { username: 'malte', admin: true, method: 'basic' };
// ANNA as /auth/me answers her (FeOS ADR 0060): every environment she
// reads, each with ferund's effective role, protected, plan and delete.
const ANNA = {
  sub: 'anna', person: 'p-anna', admin: false, method: 'oidc',
  projects: [
    { project: 'zeta', role: 'read', environments: [
      { id: 'e-z1', name: 'stage', role: 'write', protected: false, plan: true, delete: false },
      { id: 'e-z2', name: 'prod', role: 'read', protected: false, plan: false, delete: false },
    ] },
    { project: 'acme', role: 'write', environments: [
      { id: 'e-a1', name: 'anything', role: 'write', protected: false, plan: true, delete: true },
      { id: 'e-a2', name: 'prod', role: 'read', protected: true, plan: true, delete: false },
    ] },
    { project: 'shop', role: '', environments: [{ id: 'e-s1', name: 'shop-prod', role: 'read', protected: false, plan: false, delete: false }] },
    { project: 'boss', role: 'admin', environments: [] },
  ],
};

describe('the builder', () => {
  it('changes a new environment with write on the project, one environment where ferund answers write', () => {
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
    // Not listed: no role, whatever the project's.
    expect(mayBuild(acme, 'unlisted')).toBe(false);
    expect(mayBuild(projectAccess(null, 'acme'), 'x')).toBe(false);
  });

  it('plans against a protected environment where ferund says plan, and deploys nothing there (ADR 0060)', () => {
    const acme = projectAccess(ANNA, 'acme');
    expect(mayPlan(acme, 'prod')).toBe(true);
    expect(mayBuild(acme, 'prod')).toBe(false);
    expect(mayPlan(acme, '')).toBe(true);
    expect(mayPlan(projectAccess(ANNA, 'zeta'), 'prod')).toBe(false);
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

  it('the role on an environment is ferund\'s, not listed none', () => {
    const z = projectAccess(ANNA, 'zeta');
    expect(envRole(z, 'stage')).toBe('write');
    expect(envRole(z, 'e-z1')).toBe('write');
    expect(envRole(z, 'prod')).toBe('read');
    expect(envRole(z, 'other')).toBe('');
    expect(envRole(projectAccess(ANNA, 'shop'), 'other')).toBe('');
  });
});

describe('me → navigation', () => {
  it('lists a person\'s projects by name, with the role or "environments"', () => {
    expect(myProjects(ANNA).map((t) => [t.project, roleLabel(t)])).toEqual([
      ['acme', 'write'], ['boss', 'admin'], ['shop', 'environments'], ['zeta', 'read'],
    ]);
    expect(myProjects(null)).toEqual([]);
  });

  it('a project\'s pages by role', () => {
    const views = (me, t) => projectNav(projectAccess(me, t)).map((n) => n.view);
    expect(views(ANNA, 'boss')).toEqual(['overview', 'environments', 'domains', 'providers', 'members', 'tokens', 'usage', 'settings']);
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
    expect(viewGate(ANNA, parsePath('/projects/boss/tokens'))).toBe('');
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
    expect(viewGate(ANNA, parsePath('/projects/boss/new-provider'))).toBe('');
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

describe('the release\'s answers (FeOS ADRs 0056, 0059, 0060)', () => {
  const session = wire('GET_auth_me.session.200');
  const operator = wire('GET_auth_me.operator.200');

  it('reads a session\'s projects with their account and environments as ferund answers them', () => {
    for (const p of session.projects) {
      const a = projectAccess(session, p.project);
      expect(a).toMatchObject({ member: true, role: p.role, operator: false });
      expect(a.account).toEqual(p.account || null);
      for (const e of p.environments) {
        expect(envRole(a, e.id)).toBe(e.role);
        expect(mayPlan(a, e.name)).toBe(e.plan);
        expect(mayBuild(a, e.name)).toBe(atLeast(e.role, 'write'));
      }
    }
  });

  it('an operator\'s role is ferund\'s admin on every project, one not listed yet too', () => {
    const p = operator.projects[0];
    expect(projectAccess(operator, p.project)).toMatchObject({ role: p.role, operator: true, member: true });
    expect(projectAccess(operator, 'made-a-moment-ago')).toMatchObject({ role: 'admin', operator: true });
  });

  it('the accounts: their page for explicit members and operators, default open to people (§0.3)', () => {
    // The case: an explicit open account and one open to every person.
    const session = answerWith('GET_auth_me.session.200',
      (me) => me.accounts.some((a) => a.explicit && !a.closed) && me.accounts.some((a) => !a.explicit));
    expect(myAccounts(session).map((a) => a.name)).toEqual([...session.accounts.map((a) => a.name)].sort());
    const explicit = session.accounts.find((a) => a.explicit && !a.closed);
    const implicit = session.accounts.find((a) => !a.explicit);
    expect(accountAccess(session, explicit.name)).toMatchObject({ member: true, role: explicit.role, operator: false });
    expect(implicit).toBeTruthy();
    expect(accountAccess(session, implicit.name)).toMatchObject({ member: false, explicit: false });
    expect(accountAccess(session, 'nope').member).toBe(false);
    expect(accountAccess(operator, 'any')).toMatchObject({ member: true, role: 'admin', operator: true });
    expect(viewGate(session, parsePath(`/accounts/${explicit.name}`))).toBe('');
    expect(viewGate(session, parsePath('/accounts/nope'))).toBe('account');
    expect(viewGate(session, parsePath(`/accounts/${implicit.name}`))).toBe('account');
    expect(viewGate(operator, parsePath('/accounts/nope'))).toBe('');
    expect(viewGate(session, parsePath('/accounts'))).toBe('');
    expect(viewGate(BASIC, parsePath('/accounts'))).toBe('');
  });

  it('a project is made on an open account, default first; it moves to an open one but its own', () => {
    const makeOn = accountsToMakeOn(session);
    expect(makeOn.every((a) => !a.closed)).toBe(true);
    expect(makeOn.some((a) => a.name === 'default')).toBe(true);
    expect(makeOn[0].name).toBe('default');
    expect(accountsToMakeOn({ accounts: [] })).toEqual([]);
    const list = wire('GET_admin_accounts.200').accounts;
    const targets = moveTargets(list, 'cc-4711');
    expect(targets.map((a) => a.name)).not.toContain('cc-4711');
    expect(targets.some((a) => a.closed)).toBe(false);
    expect(targets.length).toBe(list.filter((a) => !a.closed && a.name !== 'cc-4711').length);
    expect(moveTargets(null, 'x')).toEqual([]);
  });

  it('says the moves of an account\'s history', () => {
    const moves = wire('GET_accounts_account.200').moves;
    for (const m of moves) {
      const t = moveText(m);
      expect(t).toContain(m.project);
      expect(t).toContain(m.to.name);
      if (m.from) expect(t).toContain(m.from.name);
      if (m.by && m.by.name) expect(t).toContain(m.by.name);
    }
    expect(moveText({ project: 'kino', from: null, to: { name: 'default', title: 'Default' }, by: { name: 'FeRun' } }))
      .toBe('kino begins on Default (default) by FeRun');
    expect(accountLabel({ name: 'default', title: 'default' })).toBe('default');
  });

  it('the integrations\' page is the operators\'', () => {
    expect(viewGate(ANNA, parsePath('/integrations'))).toBe('operator');
    expect(viewGate(OPERATOR, parsePath('/integrations'))).toBe('');
    expect(viewGate(BASIC, parsePath('/integrations'))).toBe('');
  });

  it('an integration\'s role in words, each including the one before, never what it may not', () => {
    expect(integrationRoleText('', '')).toEqual([]);
    const read = integrationRoleText('read');
    const write = integrationRoleText('write');
    const admin = integrationRoleText('admin');
    expect(read).toHaveLength(2);
    expect(write.length).toBeGreaterThan(read.length);
    expect(admin.length).toBeGreaterThan(write.length);
    expect(read[0]).toContain('reads');
    expect(write.join(' ')).toContain('route tokens');
    expect(write.join(' ')).toContain('makes, clones and deletes environments');
    expect(admin.join(' ')).toContain('claims and deletes domains');
    expect(integrationRoleText('admin', 'e1').join(' ')).toContain('publishes it and binds its domains');
    expect(integrationRoleText('write', 'e1').join(' ')).not.toContain('makes, clones');
    for (const lines of [read, write, admin]) {
      expect(lines[lines.length - 1]).toMatch(/^Never: members, invitations, grants, tokens, the account/);
    }
  });

  // §2.2: r_int(e) = max(cap(G_int, protected), g_int(e)); write on the
  // project reads a protected environment, for a person too.
  it('says what a protected environment leaves each grant of an integration', () => {
    const PROJECT_WRITE = 'On a protected environment it reads, alone or for a person, unless it holds a grant on it.';
    const CHANGES_FOR_PERSON = 'On a protected environment it reads alone; for a person it names it changes what both of you may.';
    expect(integrationRoleText('read')).not.toContain(PROJECT_WRITE);
    expect(integrationRoleText('read')).not.toContain(CHANGES_FOR_PERSON);
    expect(integrationRoleText('write')).toContain(PROJECT_WRITE);
    expect(integrationRoleText('write')).not.toContain(CHANGES_FOR_PERSON);
    expect(integrationRoleText('admin')).toContain(CHANGES_FOR_PERSON);
    expect(integrationRoleText('admin')).not.toContain(PROJECT_WRITE);
    expect(integrationRoleText('write', 'e1')).toContain(CHANGES_FOR_PERSON);
    expect(integrationRoleText('admin', 'e1')).toContain(CHANGES_FOR_PERSON);
    expect(integrationRoleText('read', 'e1')).not.toContain(CHANGES_FOR_PERSON);
  });

  it('names who gave a grant', () => {
    const m = wire('GET_project_members.200');
    for (const x of [...m.members, ...m.integrations]) {
      if (x.madeByName) expect(madeByText(x.madeBy, x.madeByName)).toContain(x.madeByName);
    }
    expect(madeByText('basic:feos-k8s', '')).toBe('operator feos-k8s');
    expect(madeByText('integration:0199/person:0198', 'Ada')).toBe('Ada, through an integration');
    expect(madeByText('token:ab', '')).toBe('a project token');
    expect(madeByText('', '')).toBe('');
  });
});
