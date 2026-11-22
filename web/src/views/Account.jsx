// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api, notAvailable } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, LiveBadge, useArm, usePoll } from '../components.jsx';
import { fmtDateTime } from '../format.js';
import {
  accountAccess, actsInProjects, fmtQuota, moveTargets, moveText, personLabel, projectAccess, quotaRows, useMe,
} from '../people.js';
import { Fact, PageHead } from './platformParts.jsx';
import { Invitations, RoleBadge } from './People.jsx';

// An account's page (ADR 0059), for its explicit members and the
// operators: the projects on it, each one's name, maker, admins and use
// against its limits, and nothing inside it; the projects that came and
// left; its people, whose roles its admins change; its invitations. An
// operator also adds any person FeRun knows, and moves a project.

// ACCOUNT_ROLES are an account's roles.
export const ACCOUNT_ROLES = ['member', 'admin'];

// accountPageFacts are the head's counts of an account's page.
export function accountPageFacts(page) {
  const members = (page && page.members) || [];
  return {
    members: members.length,
    admins: members.filter((m) => m.role === 'admin').length,
    projects: ((page && page.projects) || []).length,
  };
}

// initial is the account's page when it is read already.
export function Account({ name, navigate, initial = null }) {
  const { me, reload } = useMe();
  const acc = accountAccess(me, name);
  const operator = acc.operator;
  const [page, setPage] = useState(initial);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [people, setPeople] = useState(null);
  const [armed, arm, disarm] = useArm();

  const refresh = useCallback(async () => {
    try {
      setPage(operator ? await api.adminGetAccount(name) : await api.getAccount(name));
      setErr('');
    } catch (e) {
      if (notAvailable(e)) setMissing(true);
      else setErr(e.message);
    }
  }, [name, operator]);
  const lastOk = usePoll(refresh, 15000);
  // An operator adds any person FeRun knows.
  useEffect(() => {
    if (!operator) return undefined;
    let live = true;
    api.listPeople().then((r) => { if (live) setPeople(r.people || []); }, () => { if (live) setPeople([]); });
    return () => { live = false; };
  }, [operator]);

  const role = page ? page.role || acc.role : acc.role;
  const admin = operator || role === 'admin';
  const facts = accountPageFacts(page);
  const isDefault = name === 'default';
  const act = async (fn) => {
    setErr('');
    try {
      await fn();
      await refresh();
      reload().catch(() => {});
    } catch (e) {
      setErr(e.message);
    }
  };
  const setRole = (person, r) => (operator ? api.adminPutAccountMember(name, person, r) : api.putAccountMember(name, person, r));
  const remove = (person) => (operator ? api.adminDeleteAccountMember(name, person) : api.deleteAccountMember(name, person));
  const listInv = useCallback(() => api.listAccountInvitations(name), [name]);
  const createInv = useCallback((b) => api.createAccountInvitation(name, b), [name]);
  const revokeInv = useCallback((id) => api.revokeAccountInvitation(name, id), [name]);
  // Invitations take a person's session; default's are the operators'.
  const mayInvite = admin && actsInProjects(me) && (!isDefault || operator);

  return (
    <div className="platform">
      <PageHead kind="Account" title={(page && page.title) || name} state={page && <LiveBadge lastOk={lastOk} />}
        facts={page && <>
          <span className="mono">{name}</span>
          {page.reference && <span>reference <b className="mono">{page.reference}</b></span>}
          {page.closed && <span className="warn">closed: it takes no new project</span>}
          <Fact n={facts.projects}>project{facts.projects === 1 ? '' : 's'}</Fact>
          <Fact n={facts.members}>member{facts.members === 1 ? '' : 's'}</Fact>
          <Fact n={facts.admins}>admin{facts.admins === 1 ? '' : 's'}</Fact>
          <span>your role <RoleBadge role={operator ? 'admin' : role} />{operator && <span className="muted small"> (operator)</span>}</span>
        </>} />
      <ErrBox error={err} />
      {missing && <p className="empty">This ferund has no accounts yet.</p>}
      {!page && !err && !missing && <p className="empty">Loading…</p>}

      {page && (
        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="folder" size={15} /> Projects on it <span className="count">{facts.projects}</span></h2></div>
          <AccountProjects page={page} me={me} navigate={navigate} operator={operator} onMove={(p, to) => act(() => api.adminMoveProject(p, to))} />
        </section>)}

      {page && (
        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="history" size={15} /> Came and left</h2></div>
          {(page.moves || []).length === 0
            ? <p className="empty">No project came or left.</p>
            : (
              <ul className="moves">
                {[...page.moves].sort((a, b) => (b.at || '').localeCompare(a.at || '')).map((m, i) => (
                  <li key={`${m.project}-${m.at}-${i}`}><span className="muted small">{fmtDateTime(m.at)}</span> <span>{moveText(m)}</span></li>))}
              </ul>)}
        </section>)}

      {page && (
        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="users" size={15} /> People <span className="count">{facts.members}</span></h2></div>
          {isDefault && <p className="hint">default&apos;s admins are the operators; it holds members an operator names when the installation
            does not let every person make projects.</p>}
          <div className="tblwrap"><table className="tbl">
            <thead><tr><th>person</th><th>role</th><th className="col-quick" /></tr></thead>
            <tbody>
              {(page.members || []).map((m) => {
                const self = !!me && me.person === m.person;
                return (
                  <tr key={m.person} className={m.disabled ? 'dim' : ''}>
                    <td><span className="strong">{personLabel(m)}</span>{self && <span className="badge">you</span>}
                      {m.disabled && <span className="badge warn">disabled</span>}
                      {m.email && m.name && <div className="muted small">{m.email}</div>}</td>
                    <td>
                      {admin && !isDefault ? (
                        <select value={m.role} aria-label={`${personLabel(m)}'s role on the account`}
                          onChange={(e) => act(() => setRole(m.person, e.target.value))}>
                          {ACCOUNT_ROLES.map((r) => <option key={r} value={r}>{r}</option>)}
                        </select>) : <RoleBadge role={m.role} />}
                    </td>
                    <td className="col-quick">
                      {(admin || self) && (
                        <ArmButton armKey={`rm/${m.person}`} armed={armed} arm={arm} disarm={disarm} className="mini" icon={self ? 'logout' : 'trash'}
                          confirmLabel={self ? 'leave the account?' : 'remove?'}
                          onConfirm={() => act(async () => {
                            await remove(m.person);
                            if (self && !operator) navigate('/accounts');
                          })}>{self ? 'Leave' : 'Remove'}</ArmButton>)}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table></div>
          {(page.members || []).length === 0 && <p className="empty">No members.</p>}
          {operator && people && <AddPerson people={people} members={page.members || []} isDefault={isDefault}
            onAdd={(person, r) => act(() => api.adminPutAccountMember(name, person, r))} />}
          <p className="hint">
            An account&apos;s members see its projects, their makers and admins and their use; its admins also change roles,
            remove people and invite. An account that has an admin keeps one.
          </p>
        </section>)}

      {page && mayInvite && (
        <Invitations roles={isDefault ? ['member'] : ACCOUNT_ROLES} what="account" list={listInv} create={createInv} revoke={revokeInv} />)}
    </div>
  );
}

// AccountProjects are the projects on an account: name (a link for one
// the viewer is a member of), maker, admins, since when, and the use
// against the limits; an operator moves one to another open account.
function AccountProjects({ page, me, navigate, operator, onMove }) {
  const [accounts, setAccounts] = useState(null);
  const [moving, setMoving] = useState('');
  const [target, setTarget] = useState('');
  useEffect(() => {
    if (!operator) return undefined;
    let live = true;
    api.adminListAccounts().then((r) => { if (live) setAccounts(r.accounts || []); }, () => { if (live) setAccounts([]); });
    return () => { live = false; };
  }, [operator]);
  const list = page.projects || [];
  if (list.length === 0) return <p className="empty">No project is on this account.</p>;
  const options = moveTargets(accounts, page.name);
  return (
    <div className="tblwrap"><table className="tbl">
      <thead><tr><th>project</th><th>made by</th><th>admins</th><th className="col-when">since</th><th>use</th>{operator && <th className="col-quick" />}</tr></thead>
      <tbody>
        {list.map((p) => {
          const member = projectAccess(me, p.name).member;
          const to = `/projects/${encodeURIComponent(p.name)}`;
          return (
            <React.Fragment key={p.name}>
              <tr>
                <td>
                  {member
                    ? <a href={to} className="mono strong" onClick={(e) => { e.preventDefault(); navigate(to); }}>{p.name}</a>
                    : <span className="mono strong">{p.name}</span>}
                  {p.deleting && <span className="badge warn">being deleted</span>}
                </td>
                <td className="small">{p.madeBy ? personLabel(p.madeBy) : <span className="muted">an operator</span>}</td>
                <td className="small">{(p.admins || []).map(personLabel).join(', ') || <span className="muted">—</span>}</td>
                <td className="col-when">{p.since ? <Ago iso={p.since} /> : <span className="muted">—</span>}</td>
                <td><QuotaBars q={{ limits: p.limits, use: p.use }} /></td>
                {operator && (
                  <td className="col-quick">
                    <button className={'ghost mini' + (moving === p.name ? ' on' : '')} onClick={() => setMoving(moving === p.name ? '' : p.name)}>
                      <Icon name="building" size={12} /> move</button>
                  </td>)}
              </tr>
              {operator && moving === p.name && (
                <tr className="subrow"><td colSpan={6}>
                  {options.length === 0 ? <p className="muted small">No other open account.</p> : (
                    <form className="pf-form" onSubmit={(e) => {
                      e.preventDefault();
                      const t = target || options[0].name;
                      setMoving('');
                      onMove(p.name, t);
                    }}>
                      <span className="muted small">move {p.name} to</span>
                      <select value={target || options[0].name} aria-label="the account" onChange={(e) => setTarget(e.target.value)}>
                        {options.map((a) => <option key={a.name} value={a.name}>{a.title || a.name} ({a.name})</option>)}
                      </select>
                      <button type="submit" className="primary mini">Move</button>
                      <span className="muted small">From then its use is charged there; nothing in it changes.</span>
                    </form>)}
                </td></tr>)}
            </React.Fragment>
          );
        })}
      </tbody>
    </table></div>
  );
}

// QuotaBars are a project's use against its limits, compact.
export function QuotaBars({ q }) {
  const rows = quotaRows(q).filter((r) => r.limit !== null || r.use > 0);
  if (rows.length === 0) return <span className="muted small">nothing in use</span>;
  return (
    <div className="quota-mini">
      {rows.map((r) => (
        <div key={r.key} className={'meter' + (r.tone ? ' t-' + r.tone : '')}>
          <span className="meter-k">{r.label}</span>
          <span className="meter-v">{fmtQuota(r, r.use)}<span className="muted">{r.limit !== null ? ` / ${fmtQuota(r, r.limit)}` : ''}</span></span>
          <span className={'meter-bar' + (r.key === 'vcpus' || r.key === 'memoryMib' ? ' os' : '')} aria-hidden="true">
            <span style={{ width: r.share === null ? '0%' : `${Math.min(100, Math.round(r.share * 100))}%` }} />
          </span>
        </div>
      ))}
    </div>
  );
}

// AddPerson gives a person FeRun knows a role on the account (an
// operator's; default takes members only).
function AddPerson({ people, members, isDefault, onAdd }) {
  const [person, setPerson] = useState('');
  const [role, setRole] = useState('member');
  const free = people.filter((p) => !p.disabledAt && !members.some((m) => m.person === p.id));
  if (free.length === 0) return null;
  return (
    <form className="pf-form" onSubmit={(e) => { e.preventDefault(); if (person) { onAdd(person, role); setPerson(''); } }}>
      <span className="muted small">add</span>
      <select value={person} aria-label="the person" onChange={(e) => setPerson(e.target.value)}>
        <option value="">a person…</option>
        {free.map((p) => <option key={p.id} value={p.id}>{p.name || p.email || p.id}{p.email && p.name ? ` · ${p.email}` : ''}</option>)}
      </select>
      <select value={isDefault ? 'member' : role} aria-label="role" disabled={isDefault} onChange={(e) => setRole(e.target.value)}>
        {(isDefault ? ['member'] : ACCOUNT_ROLES).map((r) => <option key={r} value={r}>{r}</option>)}
      </select>
      <button type="submit" className="primary mini" disabled={!person}>Add</button>
    </form>
  );
}
