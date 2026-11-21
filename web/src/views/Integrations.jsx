// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api, notAvailable } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, LiveBadge, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDate } from '../format.js';
import { issuerHost } from './platformData.js';
import { Fact, PageHead } from './platformParts.jsx';

// The operators' integrations (ADR 0056): services such as FeCode that
// sign in with their own access token of the installation's provider
// and act in the projects that grant them a role. An operator registers
// one by its provider's subject, changes its title and URL, disables
// (every grant stops at once), enables and removes it. FeRun keeps no
// secret of one: its client's secret is the provider's.

// INTEGRATION_NAME is the rule of an integration's name.
export const INTEGRATION_NAME = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

// integrationForm is a registration's form from an integration (empty
// for a new one).
export function integrationForm(i) {
  return { name: (i && i.name) || '', title: (i && i.title) || '', url: (i && i.url) || '', issuer: (i && i.issuer) || '', subject: (i && i.subject) || '' };
}

// integrationProblem is what is wrong with a form, '' when ferund may be
// asked: the name's rule, an https URL or none, a subject.
export function integrationProblem(f) {
  if (!INTEGRATION_NAME.test(f.name.trim())) return 'a name of lowercase letters, digits and \'-\', at most 63';
  const url = f.url.trim();
  if (url && !/^https:\/\/\S+$/.test(url)) return 'the URL is an https:// URL, or none';
  if (!f.subject.trim()) return 'the subject of its client at the provider';
  if (f.subject.trim().length > 255) return 'a subject of at most 255 characters';
  return '';
}

export function Integrations() {
  const [list, setList] = useState(null);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [edit, setEdit] = useState(null); // {form, isNew}
  const [armed, arm, disarm] = useArm();
  useNow(10000);

  const refresh = useCallback(async () => {
    try {
      setList((await api.adminListIntegrations()).integrations || []);
      setErr('');
    } catch (e) {
      if (notAvailable(e)) setMissing(true);
      else setErr(e.status === 503 ? 'This installation verifies no provider\'s tokens (--oidc-issuer): it takes no integration.' : e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 15000);
  const all = list || [];
  const act = async (fn) => {
    setErr('');
    try {
      await fn();
      await refresh();
    } catch (e) {
      setErr(e.message);
    }
  };

  return (
    <div className="platform">
      <PageHead title="Integrations" state={list && <LiveBadge lastOk={lastOk} />}
        actions={<button className={'primary' + (edit && edit.isNew ? ' on' : '')}
          onClick={() => setEdit(edit && edit.isNew ? null : { form: integrationForm(null), isNew: true })}>
          <Icon name="plus" size={14} /> Register an integration</button>}
        facts={list && <>
          <Fact n={all.length}>registered</Fact>
          <Fact className={all.some((i) => i.disabled) ? 'warn' : ''} n={all.filter((i) => i.disabled).length}>disabled</Fact>
        </>} />

      {edit && (
        <section className="pane">
          <IntegrationForm initial={edit.form} isNew={edit.isNew} onCancel={() => setEdit(null)}
            onSave={async (f) => {
              await api.adminPutIntegration(f.name.trim(), {
                title: f.title.trim(), url: f.url.trim(), issuer: f.issuer.trim(), subject: f.subject.trim(),
              });
              setEdit(null);
              await refresh();
            }} />
        </section>)}

      <section className="pane">
        <ErrBox error={err} />
        {missing && <p className="empty">This ferund has no integrations yet.</p>}
        {!missing && (
          <div className="tblwrap"><table className="tbl">
            <thead>
              <tr><th>integration</th><th>signs in as</th><th className="col-num">projects</th><th>state</th>
                <th className="col-when">last seen</th><th className="col-quick" /></tr>
            </thead>
            <tbody>
              {all.map((i) => (
                <tr key={i.name} className={i.disabled ? 'dim' : ''}>
                  <td>
                    <span className="strong">{i.title || i.name}</span> <span className="mono muted small">{i.name}</span>
                    {i.url && <div className="small"><a href={i.url} target="_blank" rel="noreferrer">{i.url}</a></div>}
                  </td>
                  <td className="small"><span title={i.issuer}>{issuerHost(i.issuer)}</span><div className="mono muted">{i.subject}</div></td>
                  <td className="col-num mono">{Number(i.projects) || 0}</td>
                  <td className="small">
                    {i.disabled
                      ? <span className="warn">disabled{i.disabledAt ? <> <Ago iso={i.disabledAt} /></> : ''}</span>
                      : <span className="ok">active</span>}
                    <div className="muted">registered {fmtDate(i.createdAt)}</div>
                  </td>
                  <td className="col-when">{i.lastSeen ? <Ago iso={i.lastSeen} /> : <span className="muted">never</span>}</td>
                  <td className="col-quick nowrap">
                    <button className="ghost mini" onClick={() => setEdit({ form: integrationForm(i), isNew: false })}>
                      <Icon name="settings" size={12} /> edit</button>
                    <ArmButton armKey={`state/${i.name}`} armed={armed} arm={arm} disarm={disarm} className="mini"
                      confirmLabel={i.disabled ? 'enable? its grants act again' : 'disable? every grant stops at once'}
                      onConfirm={() => act(() => (i.disabled ? api.adminEnableIntegration(i.name) : api.adminDisableIntegration(i.name)))}>
                      {i.disabled ? 'Enable' : 'Disable'}</ArmButton>
                    <ArmButton armKey={`rm/${i.name}`} armed={armed} arm={arm} disarm={disarm} className="mini" icon="trash"
                      confirmLabel="remove? its grants go with it" title="remove the integration, its grants and the tokens it minted"
                      onConfirm={() => act(() => api.adminDeleteIntegration(i.name))} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table></div>)}
        {list && all.length === 0 && <p className="empty">No integration is registered.</p>}
        {!list && !err && !missing && <p className="empty">Loading…</p>}
        <p className="muted small footnote">
          An integration reaches no project until a project&apos;s admin grants it a role (People in the project). It
          signs in with its own access token of this installation&apos;s provider, its subject the one named here; FeRun
          keeps no secret of it. It never changes a project&apos;s members, invitations, grants, tokens or account.
        </p>
      </section>
    </div>
  );
}

// IntegrationForm registers an integration or changes one (its name is
// fixed once registered).
export function IntegrationForm({ initial, isNew, onSave, onCancel }) {
  const [f, setF] = useState(initial);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => setF({ ...f, [k]: e.target.value });
  const problem = integrationProblem(f);
  return (
    <form onSubmit={async (e) => {
      e.preventDefault();
      if (problem) { setErr(problem); return; }
      setBusy(true);
      setErr('');
      try {
        await onSave(f);
      } catch (e2) {
        setErr(e2.message);
      } finally {
        setBusy(false);
      }
    }}>
      <div className="limits-grid">
        <label>name<input className="mono" value={f.name} disabled={!isNew} onChange={set('name')} placeholder="fecode" /></label>
        <label>title<input value={f.title} onChange={set('title')} placeholder="FeCode" /></label>
        <label>URL<input className="mono" value={f.url} onChange={set('url')} placeholder="https://fecode.dev" /></label>
        <label>issuer<input className="mono" value={f.issuer} onChange={set('issuer')} placeholder="this installation's provider" /></label>
        <label>subject<input className="mono" value={f.subject} onChange={set('subject')} placeholder="its client's ID at the provider" /></label>
      </div>
      <ErrBox error={err} />
      <div className="pf-form">
        <button type="submit" className="primary" disabled={busy}>{isNew ? 'Register' : 'Save'}</button>
        <button type="button" className="ghost" onClick={onCancel}>Cancel</button>
        {problem && <span className="muted small">{problem}</span>}
      </div>
      <p className="muted small">
        Its name shows wherever it acts. The issuer is this installation&apos;s provider (left empty, ferund fills it);
        the subject is its client&apos;s at the provider, whose tokens carry it as sub and client_id.
      </p>
    </form>
  );
}
