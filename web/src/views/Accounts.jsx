// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api, notAvailable } from '../api.js';
import { Icon } from '../Icon.jsx';
import { ArmButton, ErrBox, LiveBadge, useArm, usePoll } from '../components.jsx';
import { fmtDate } from '../format.js';
import { isOperator, myAccounts, useMe } from '../people.js';
import { Fact, PageHead } from './platformParts.jsx';
import { AccountList } from './People.jsx';

// The accounts (ADR 0059): who is charged for a project's use. A person
// sees theirs (/auth/me's: those they may put projects on or read); an
// operator every one, makes, changes, closes and reopens one, and deletes
// one that never held a project. default always exists and is never
// closed.

// ACCOUNT_NAME is the rule of an account's name, the projects' rule.
export const ACCOUNT_NAME = /^[a-z0-9][a-z0-9.-]{1,62}$/;

// accountFacts are the operators' list's counts.
export function accountFacts(accounts) {
  const all = accounts || [];
  return {
    accounts: all.length,
    closed: all.filter((a) => a.closed).length,
    projects: all.reduce((n, a) => n + (Number(a.projects) || 0), 0),
  };
}

export function Accounts({ navigate }) {
  const { me } = useMe();
  if (isOperator(me)) return <OperatorAccounts navigate={navigate} />;
  const accounts = myAccounts(me);
  return (
    <div className="single">
      <PageHead kind="Accounts" title="Your accounts"
        facts={<span>An account is charged for the use of the projects on it; its people see them, their makers and admins.</span>} />
      {accounts.length > 0
        ? <AccountList accounts={accounts} navigate={navigate} />
        : <section className="pane"><p className="empty">You are on no account. An account&apos;s admin or an operator can invite you.</p></section>}
    </div>
  );
}

// OperatorAccounts is every account, for the platform's operators.
function OperatorAccounts({ navigate }) {
  const [list, setList] = useState(null);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [showCreate, setShowCreate] = useState(() => new URLSearchParams(location.search).has('new'));
  const [form, setForm] = useState({ name: '', title: '', reference: '' });
  const [edit, setEdit] = useState(null); // {name, title, reference}
  const [armed, arm, disarm] = useArm();

  const refresh = useCallback(async () => {
    try {
      setList((await api.adminListAccounts()).accounts || []);
      setErr('');
    } catch (e) {
      if (notAvailable(e)) setMissing(true);
      else setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 10000);
  const act = async (fn) => {
    setErr('');
    try {
      await fn();
      await refresh();
    } catch (e) {
      setErr(e.message);
    }
  };
  const facts = accountFacts(list);
  const nameOk = ACCOUNT_NAME.test(form.name.trim());

  return (
    <div className="platform">
      <PageHead title="Accounts" state={list && <LiveBadge lastOk={lastOk} />}
        actions={<button className={'primary' + (showCreate ? ' on' : '')} onClick={() => setShowCreate(!showCreate)}>
          <Icon name="plus" size={14} /> New account</button>}
        facts={list && <>
          <Fact n={facts.accounts}>account{facts.accounts === 1 ? '' : 's'}</Fact>
          <Fact className={facts.closed ? 'warn' : ''} n={facts.closed}>closed</Fact>
          <Fact n={facts.projects}>project{facts.projects === 1 ? '' : 's'} on them</Fact>
        </>} />

      {showCreate && (
        <section className="pane">
          <form className="pf-form" onSubmit={(e) => {
            e.preventDefault();
            act(async () => {
              await api.adminCreateAccount({ name: form.name.trim(), title: form.title.trim(), reference: form.reference.trim() });
              setForm({ name: '', title: '', reference: '' });
              setShowCreate(false);
            });
          }}>
            <input className="mono" placeholder="name: lowercase letters, digits, '.' and '-'" value={form.name} autoFocus
              aria-label="the account's name" onChange={(e) => setForm({ ...form, name: e.target.value })} />
            <input placeholder="title (e.g. the team's name)" value={form.title} aria-label="title"
              onChange={(e) => setForm({ ...form, title: e.target.value })} />
            <input className="mono" placeholder="reference (e.g. a cost center)" value={form.reference} aria-label="reference"
              onChange={(e) => setForm({ ...form, reference: e.target.value })} />
            <button type="submit" className="primary" disabled={!nameOk}><Icon name="plus" size={14} /> Create</button>
            <button type="button" className="ghost" onClick={() => setShowCreate(false)}><Icon name="x" size={14} /></button>
          </form>
          <p className="muted small">The name is fixed; the title and the reference (what billing knows it by) change later. Its first admin
            is the person an operator gives the role, or the one an admin invitation brings in.</p>
        </section>)}

      <section className="pane">
        <ErrBox error={err} />
        {missing && <p className="empty">This ferund has no accounts yet.</p>}
        {!missing && (
          <div className="tblwrap"><table className="tbl">
            <thead>
              <tr><th>account</th><th>reference</th><th className="col-num">members</th><th className="col-num">projects</th>
                <th>state</th><th className="col-when">created</th><th className="col-quick" /></tr>
            </thead>
            <tbody>
              {(list || []).map((a) => (
                <React.Fragment key={a.name}>
                  <tr className={a.closed ? 'dim' : ''}>
                    <td>
                      <a href={`/accounts/${encodeURIComponent(a.name)}`} className="strong"
                        onClick={(e) => { e.preventDefault(); navigate(`/accounts/${encodeURIComponent(a.name)}`); }}>{a.title || a.name}</a>
                      {' '}<span className="mono muted small">{a.name}</span>
                    </td>
                    <td className="mono small">{a.reference || <span className="muted">—</span>}</td>
                    <td className="col-num mono">{a.name === 'default' ? <span className="muted" title="open to people when the installation lets them make projects">—</span> : Number(a.members) || 0}</td>
                    <td className="col-num mono">{Number(a.projects) || 0}</td>
                    <td className="small">{a.closed ? <span className="warn">closed</span> : <span className="ok">open</span>}</td>
                    <td className="col-when muted">{fmtDate(a.createdAt)}</td>
                    <td className="col-quick nowrap">
                      <button className="ghost mini" onClick={() => setEdit(edit && edit.name === a.name ? null
                        : { name: a.name, title: a.title || '', reference: a.reference || '' })}>
                        <Icon name="settings" size={12} /> edit</button>
                      {a.name !== 'default' && (
                        <ArmButton armKey={`close/${a.name}`} armed={armed} arm={arm} disarm={disarm} className="mini"
                          confirmLabel={a.closed ? 'reopen? it takes projects again' : 'close? it takes no new project; its projects stay'}
                          onConfirm={() => act(() => api.adminPatchAccount(a.name, { closed: !a.closed }))}>
                          {a.closed ? 'Reopen' : 'Close'}</ArmButton>)}
                      {a.name !== 'default' && !Number(a.projects) && (
                        <ArmButton armKey={`rm/${a.name}`} armed={armed} arm={arm} disarm={disarm} className="mini" icon="trash"
                          confirmLabel="delete?" title="only an account that never held a project is deleted"
                          onConfirm={() => act(() => api.adminDeleteAccount(a.name))} />)}
                    </td>
                  </tr>
                  {edit && edit.name === a.name && (
                    <tr className="subrow"><td colSpan={7}>
                      <form className="pf-form" onSubmit={(e) => {
                        e.preventDefault();
                        act(async () => {
                          await api.adminPatchAccount(a.name, { title: edit.title.trim(), reference: edit.reference.trim() });
                          setEdit(null);
                        });
                      }}>
                        <input placeholder="title" value={edit.title} aria-label="title" onChange={(e) => setEdit({ ...edit, title: e.target.value })} />
                        <input className="mono" placeholder="reference" value={edit.reference} aria-label="reference"
                          onChange={(e) => setEdit({ ...edit, reference: e.target.value })} />
                        <button type="submit" className="primary">Save</button>
                        <button type="button" className="ghost" onClick={() => setEdit(null)}>Cancel</button>
                      </form>
                    </td></tr>)}
                </React.Fragment>
              ))}
            </tbody>
          </table></div>)}
        {list && list.length === 0 && <p className="empty">No accounts.</p>}
        {!list && !err && !missing && <p className="empty">Loading…</p>}
        <p className="muted small footnote">
          Every project is on one account, which is charged for its use. default always exists and takes the projects
          people make when they name no account; its admins are the operators. A closed account takes no project, new or
          moved; its projects stay. No name holds the account: a move changes no host, grant or token.
        </p>
      </section>
    </div>
  );
}
