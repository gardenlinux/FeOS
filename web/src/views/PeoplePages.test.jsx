// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { MeContext, can, moveTargets, moveText, myAccounts, projectAccess } from '../people.js';
import { answerWith, answers, wire } from '../testing/wire.js';
import { Account } from './Account.jsx';
import { OperatorAccounts } from './Accounts.jsx';
import { Integrations } from './Integrations.jsx';
import { InviteAccept, IntegrationsPane, acceptedPath } from './People.jsx';
import { ProjectAccount } from './ProjectPages.jsx';
import { ProjectAccountMove } from './Projects.jsx';
import { ProtectDialog } from './EnvTabs.jsx';

// The release's people pages rendered whole with ferund's answers
// (testing/wire.js), each given the answer it would read first.

const html = (el) => renderToStaticMarkup(el);
const text = (el) => html(el).replace(/<[^>]+>/g, ' ').replace(/&#x27;/g, "'").replace(/&quot;/g, '"')
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&').replace(/\s+/g, ' ');
const withMe = (me, el) => <MeContext.Provider value={{ me, reload: async () => me }}>{el}</MeContext.Provider>;
const nav = () => {};
const count = (s, needle) => s.split(needle).length - 1;

afterEach(() => vi.unstubAllGlobals());

describe('the operators\' accounts', () => {
  it('lists every account; deletes only one that never held a project, never default', () => {
    vi.stubGlobal('location', { search: '' });
    const list = wire('GET_admin_accounts.200').accounts;
    const out = html(withMe(wire('GET_auth_me.operator.200'), <OperatorAccounts navigate={nav} initial={list} />));
    for (const a of list) expect(out).toContain(`href="/accounts/${a.name}"`);
    const deletable = list.filter((a) => a.name !== 'default' && !Number(a.projects));
    expect(deletable.length).toBeGreaterThan(0);
    expect(count(out, 'aria-label="only an account that never held a project is deleted"')).toBe(deletable.length);
    expect(count(out, '>closed<')).toBe(list.filter((a) => a.closed).length);
    // default is never closed or reopened.
    expect(count(out, '>Close<') + count(out, '>Reopen<')).toBe(list.filter((a) => a.name !== 'default').length);
  });
});

describe('the operators\' integrations', () => {
  it('lists each with how it signs in, its state, and names its icon-only remove', () => {
    // The case: one enabled, one disabled.
    const list = answerWith('GET_admin_integrations.200',
      (a) => a.integrations.some((i) => i.disabled) && a.integrations.some((i) => !i.disabled)).integrations;
    const out = html(<Integrations initial={list} />);
    for (const i of list) {
      expect(out).toContain(i.subject);
      expect(text(<Integrations initial={list} />)).toContain(i.title || i.name);
    }
    expect(count(out, 'aria-label="remove the integration, its grants and the tokens it minted"')).toBe(list.length);
    expect(list.some((i) => i.disabled)).toBe(true);
    expect(count(out, '>Enable<')).toBe(list.filter((i) => i.disabled).length);
    expect(count(out, '>Disable<')).toBe(list.filter((i) => !i.disabled).length);
  });
});

describe('an account\'s page', () => {
  const page = wire('GET_accounts_account.200');

  it('a member reads its projects, history and people; changes no role', () => {
    const me = wire('GET_auth_me.session.200');
    const out = text(withMe(me, <Account name={page.name} navigate={nav} initial={page} />));
    expect(out).toContain(page.title);
    for (const p of page.projects) expect(out).toContain(p.name);
    for (const m of page.moves) expect(out).toContain(moveText(m));
    for (const m of page.members) expect(out).toContain(m.name);
    expect(html(withMe(me, <Account name={page.name} navigate={nav} initial={{ ...page, role: 'member' }} />)))
      .not.toContain('role on the account"');
  });

  it('an admin of it changes roles; a project the viewer is in is a link', () => {
    // The case: the viewer is in one of the account's projects.
    const [page, me] = answerWith(['GET_accounts_account.200', 'GET_auth_me.session.200'],
      (pg, m) => pg.projects.some((p) => projectAccess(m, p.name).member));
    const out = html(withMe(me, <Account name={page.name} navigate={nav} initial={{ ...page, role: 'admin' }} />));
    expect(count(out, 'role on the account"')).toBe(page.members.length);
    for (const p of page.projects) {
      const link = `href="/projects/${p.name}"`;
      if (projectAccess(me, p.name).member) expect(out).toContain(link);
      else expect(out).not.toContain(link);
    }
    expect(page.projects.some((p) => projectAccess(me, p.name).member)).toBe(true);
  });
});

describe('a project\'s account', () => {
  const info = wire('GET_project_account.200');
  const me = wire('GET_auth_me.session.200');
  const accessOf = (role) => {
    const a = { project: 'kino', member: true, role, environments: [], operator: false };
    return { ...a, can: can(a) };
  };

  it('every member reads it and its history; an admin moves it to an open account of theirs', () => {
    // The case: the admin is on another open account.
    const [info, me] = answerWith(['GET_project_account.200', 'GET_auth_me.session.200'],
      (i, m) => moveTargets(myAccounts(m), i.account.name).length > 0);
    const read = text(withMe(me, <ProjectAccount project="kino" access={accessOf('read')} initial={info} />));
    expect(read).toContain(info.account.title);
    for (const m of info.moves) expect(read).toContain(moveText(m));
    expect(read).not.toContain('move to');
    const admin = html(withMe(me, <ProjectAccount project="kino" access={accessOf('admin')} initial={info} />));
    const options = moveTargets(myAccounts(me), info.account.name);
    expect(options.length).toBeGreaterThan(0);
    expect(count(admin, '<option ')).toBe(options.length);
    for (const a of options) expect(admin).toContain(`value="${a.name}"`);
    expect(admin).not.toContain(`value="${info.account.name}"`);
  });

  it('an admin on no other open account is told whom to ask', () => {
    const lone = { ...me, accounts: me.accounts.filter((a) => a.name === info.account.name) };
    expect(text(withMe(lone, <ProjectAccount project="kino" access={accessOf('admin')} initial={info} />)))
      .toContain('You are a member of no other open account');
  });

  it('an operator moves one to any open account but its own', () => {
    const accounts = wire('GET_admin_accounts.200').accounts;
    const project = { name: 'kino', account: { name: 'default', title: 'Default' } };
    const out = html(<ProjectAccountMove project={project} accounts={accounts} onChange={nav} />);
    const options = moveTargets(accounts, 'default');
    expect(count(out, '<option ')).toBe(options.length);
    expect(out).not.toContain('value="default"');
    for (const a of accounts.filter((x) => x.closed)) expect(out).not.toContain(`value="${a.name}"`);
    expect(text(<ProjectAccountMove project={project} accounts={[]} onChange={nav} />)).toContain('No other open account.');
    expect(text(<ProjectAccountMove project={project} accounts={null} onChange={nav} />)).toContain('Reading the accounts…');
  });
});

describe('an invitation', () => {
  it('an account\'s says the account and the role, and leads to its page', () => {
    const inv = wire('GET_invitations_token.account.200');
    const me = wire('GET_auth_me.session.200');
    const out = text(withMe(me, <InviteAccept token="t" navigate={nav} initial={inv} />));
    expect(out).toContain(`You are invited to account ${inv.account.title} ( ${inv.account.name} )`);
    expect(out).toContain(inv.role);
    expect(out).toContain('Accepting gives you the role.');
    expect(out).not.toContain('this account');
    // me is read after the acceptance: the account is the person's then.
    const fresh = { ...me, accounts: [...me.accounts, { name: inv.account.name, title: inv.account.title, role: 'member', explicit: true }] };
    expect(acceptedPath({}, inv, fresh)).toBe(`/accounts/${inv.account.name}`);
    expect(acceptedPath({ account: inv.account.name }, null, fresh)).toBe(`/accounts/${inv.account.name}`);
  });

  it('one to default leads to the accounts: default\'s page is the operators\'', () => {
    const me = wire('GET_auth_me.session.200');
    expect(me.accounts.find((a) => a.name === 'default').explicit).toBe(false);
    expect(acceptedPath({ account: 'default' }, null, me)).toBe('/accounts');
    expect(acceptedPath({}, null, { ...me, accounts: [] })).toBe('/projects');
  });

  it('a project\'s leads to the project as /auth/me sees it, else to the projects', () => {
    const me = wire('GET_auth_me.session.200');
    expect(acceptedPath({ project: 'kino' }, null, me)).toMatch(/^\/projects\/kino/);
    expect(acceptedPath({}, null, me)).toBe('/projects');
  });
});

describe('the FeCode link ?integration=<name>', () => {
  it('opens the grant for an admin of an environment alone, before the environments are read', () => {
    const alone = (p) => p.role === '' && p.environments.some((e) => e.role === 'admin');
    const me = answerWith('GET_auth_me.session.200', (m) => m.projects.some(alone));
    const shop = me.projects.find(alone);
    expect(shop).toBeTruthy();
    const a = { project: shop.project, member: true, role: shop.role, environments: shop.environments, operator: false };
    const props = { list: [], access: { ...a, can: can(a) }, envs: [], adminEnvs: [], act: nav };
    expect(html(<IntegrationsPane {...props} preselect="fecode" />)).toContain('aria-label="the integration"');
    expect(html(<IntegrationsPane {...props} preselect="" />)).not.toContain('aria-label="the integration"');
  });
});

describe('the protect dialog', () => {
  it('is modal and takes focus', () => {
    const out = html(<ProtectDialog env="prod" st={{ id: 'e1', name: 'prod' }} onClose={nav} onDone={nav} />);
    expect(out).toContain('role="dialog"');
    expect(out).toContain('aria-modal="true"');
    expect(out).toContain('tabindex="-1"');
    expect(out).toContain('Reading who holds a grant…');
  });
});
