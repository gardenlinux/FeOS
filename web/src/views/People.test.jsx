// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { MeContext, projectAccess, can } from '../people.js';
import { wire } from '../testing/wire.js';
import { AccountList, IntegrationGrantForm, IntegrationsPane, MyProjects, TokenRows } from './People.jsx';
import { Accounts, accountFacts, ACCOUNT_NAME } from './Accounts.jsx';
import { QuotaBars, accountPageFacts } from './Account.jsx';
import { INTEGRATION_NAME, integrationForm, integrationProblem } from './Integrations.jsx';

// The people's pages of the release (FeOS ADRs 0056, 0059): a person's
// projects with their accounts and the account to make one on, the
// integrations a project grants, the accounts, the integrations'
// registration; rendered with ferund's answers (testing/wire.js).

const html = (el) => renderToStaticMarkup(el);
const text = (el) => html(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"')
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const withMe = (me, el) => <MeContext.Provider value={{ me, reload: async () => me }}>{el}</MeContext.Provider>;
const nav = () => {};

afterEach(() => vi.unstubAllGlobals());

describe('MyProjects', () => {
  it('makes a project on an open account, default first, and lists each project\'s account', () => {
    vi.stubGlobal('location', { search: '?new' });
    const me = wire('GET_auth_me.session.200');
    const out = html(withMe(me, <MyProjects navigate={nav} />));
    const open = me.accounts.filter((a) => !a.closed);
    // The screen's case: more than one open account offers a choice.
    expect(open.length).toBeGreaterThan(1);
    expect(out).toContain('aria-label="the account"');
    expect((out.match(/<option /g) || []).length).toBe(open.length);
    for (const a of me.accounts.filter((x) => x.closed)) expect(out).not.toContain(`<option value="${a.name}"`);
    const t = text(withMe(me, <MyProjects navigate={nav} />));
    for (const p of me.projects) {
      expect(p.account).toBeTruthy();
      expect(t).toContain(p.account.title || p.account.name);
    }
    expect(out).toContain('Your accounts');
  });

  it('without an account to make one on, says whom to ask and makes none', () => {
    vi.stubGlobal('location', { search: '?new' });
    const me = { ...wire('GET_auth_me.session.200'), accounts: [] };
    const out = html(withMe(me, <MyProjects navigate={nav} />));
    expect(out).toContain('You are on no account you may put projects on');
    expect(/<button type="submit" class="primary"[^>]*disabled/.test(out)).toBe(true);
  });
});

describe('the accounts', () => {
  it('a person\'s list opens the page of an explicit one only', () => {
    const me = wire('GET_auth_me.session.200');
    const out = html(<AccountList accounts={me.accounts} navigate={nav} />);
    expect((out.match(/class="clickable"/g) || []).length).toBe(me.accounts.filter((a) => a.explicit).length);
    expect(me.accounts.some((a) => !a.explicit)).toBe(true);
    expect(out).toContain('open to you');
    expect(me.accounts.some((a) => a.closed)).toBe(true);
    expect(out).toContain('closed: takes no new project');
    expect(text(withMe(me, <Accounts navigate={nav} />))).toContain('Your accounts');
  });

  it('counts the operators\' list and an account\'s page', () => {
    const list = wire('GET_admin_accounts.200').accounts;
    expect(accountFacts(list)).toEqual({
      accounts: list.length, closed: list.filter((a) => a.closed).length, projects: list.reduce((n, a) => n + a.projects, 0),
    });
    const page = wire('GET_accounts_account.200');
    expect(accountPageFacts(page)).toEqual({
      members: page.members.length, admins: page.members.filter((m) => m.role === 'admin').length, projects: page.projects.length,
    });
    expect(ACCOUNT_NAME.test('cc-4711')).toBe(true);
    expect(ACCOUNT_NAME.test('Team X')).toBe(false);
  });

  it('shows a project\'s use against its limits, compact', () => {
    const p = wire('GET_accounts_account.200').projects[0];
    const out = text(<QuotaBars q={{ limits: p.limits, use: p.use }} />);
    expect(out).toContain('sandboxes');
    expect(text(<QuotaBars q={{ limits: {}, use: {} }} />)).toContain('nothing in use');
  });
});

describe('integrations in a project', () => {
  const members = wire('GET_project_members.200');
  const me = wire('GET_auth_me.session.200');
  const accessOf = (role) => {
    const a = { project: 'kino', member: true, role, environments: me.projects[0].environments, operator: false };
    return { ...a, can: can(a) };
  };

  it('every member reads them; an admin of the project changes their roles', () => {
    const read = html(<IntegrationsPane list={members.integrations} access={accessOf('read')} envs={[]} adminEnvs={[]} act={nav} preselect="" />);
    for (const g of members.integrations) expect(read).toContain(g.title || g.integration);
    expect(read).not.toContain('Add an integration');
    expect(read).not.toContain('<select');
    const admin = html(<IntegrationsPane list={members.integrations} access={accessOf('admin')} envs={[]} adminEnvs={[]} act={nav} preselect="" />);
    expect(admin).toContain('Add an integration');
    expect(admin).toContain('<select');
  });

  it('?integration=<name> opens the grant with it chosen, and says what the role lets it do', () => {
    const out = text(<IntegrationsPane list={[]} access={accessOf('admin')} envs={[]} adminEnvs={[]} act={nav} preselect="fecode" />);
    expect(out).toContain('Never: members, invitations, grants, tokens, the account');
    const form = html(<IntegrationGrantForm granted={[]} envs={[]} projectRole="admin" adminProject preselect="fecode" onGrant={nav} onCancel={nav} />);
    expect(form).toContain('aria-label="the integration"');
    expect(form).toContain('>the project</option>');
  });

  it('a project admin reads the tokens beside them', () => {
    const out = text(<TokenRows tokens={[{ hash: 'ab'.repeat(32), label: 'landscape release', role: 'write', createdAt: '2026-11-21T10:00:00Z' }]} />);
    expect(out).toContain('landscape release');
    expect(out).toContain('the project');
  });

  it('the project\'s access holds the account /auth/me names', () => {
    expect(projectAccess(me, me.projects[0].project).account).toEqual(me.projects[0].account || null);
  });
});

describe('registering an integration', () => {
  it('checks the name, the URL and the subject before asking ferund', () => {
    const ok = { name: 'fecode', title: 'FeCode', url: 'https://fecode.dev', issuer: '', subject: 'dd32' };
    expect(integrationProblem(ok)).toBe('');
    expect(integrationProblem({ ...ok, name: 'FeCode' })).toContain('lowercase');
    expect(integrationProblem({ ...ok, url: 'http://x' })).toContain('https://');
    expect(integrationProblem({ ...ok, url: '' })).toBe('');
    expect(integrationProblem({ ...ok, subject: ' ' })).toContain('subject');
    // A new one never re-points a registered one (the PUT is an upsert).
    expect(integrationProblem(ok, { isNew: true, taken: ['fecode'] })).toBe('fecode is registered: edit it');
    expect(integrationProblem({ ...ok, name: ' fecode ' }, { isNew: true, taken: ['fecode'] })).toContain('is registered');
    expect(integrationProblem(ok, { isNew: false, taken: ['fecode'] })).toBe('');
    expect(integrationProblem(ok, { isNew: true, taken: ['other'] })).toBe('');
    expect(INTEGRATION_NAME.test('a')).toBe(true);
    expect(INTEGRATION_NAME.test('-a')).toBe(false);
    for (const i of wire('GET_admin_integrations.200').integrations) {
      expect(integrationProblem(integrationForm(i))).toBe('');
    }
  });
});
