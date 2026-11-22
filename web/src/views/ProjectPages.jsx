// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useRef, useState } from 'react';
import { api, notAvailable } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, KV, LiveBadge, useArm, useModal, usePoll } from '../components.jsx';
import { fmtDateTime } from '../format.js';
import {
  accountLabel, fmtQuota, moveTargets, moveText, myAccounts, quotaRows, useAccess, useMe,
} from '../people.js';
import { useSummary } from '../summary.jsx';
import { RoleBadge } from './People.jsx';
import { AT_LIMIT, COUNTS, booked } from './usage.js';
import '../styles/project.css';

// The project's usage (its use against the limits the platform's
// operators set, ADR 0031 "Quotas per project") and its settings (who you
// are in it, leaving it, deleting it where ferund says you may; for
// operators the way to the platform's project list, where limits and zone
// are).

// ------------------------------------------------------------------ usage --

function Link({ to, navigate, children }) {
  return <a href={to} onClick={(e) => { e.preventDefault(); navigate(to); }}>{children}</a>;
}

export function Usage({ project, navigate }) {
  const access = useAccess(project);
  const sum = useSummary();
  const [quota, setQuota] = useState(null);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);

  const refresh = useCallback(async () => {
    try {
      setQuota(await api.getQuota());
      setErr('');
    } catch (e) {
      if (notAvailable(e)) setMissing(true);
      else setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 10000);

  const rows = quota ? quotaRows(quota) : [];
  const limited = rows.filter((r) => r.limit !== null).length;
  const near = rows.filter((r) => r.tone).length;
  const parts = booked(quota, sum.sandboxes, sum.jobs);

  return (
    <div className="projectpage">
      <header className="pagehead">
        <div className="pagehead-title">
          <span className="kind">Project</span>
          <h1>Usage</h1>
          <span className="pagehead-actions"><LiveBadge lastOk={lastOk} /></span>
        </div>
        <div className="facts">
          <span>project <b className="mono">{project}</b></span>
          {quota && <span><b>{limited}</b> of {rows.length} limits set</span>}
          {quota && near > 0 && <span className="warn"><b className="warn">{near}</b> near or at the limit</span>}
        </div>
      </header>
      <ErrBox error={err} />
      {missing && <p className="empty">This ferund has no quotas.</p>}
      {!quota && !err && !missing && <p className="empty">Loading…</p>}

      {quota && (
        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="gauge" size={14} /> Use against the limits</h2></div>
          <div className="usage-rows">
            {rows.map((r) => <UsageRow key={r.key} row={r} />)}
          </div>
        </section>)}

      {parts && (
        <section className="pane">
          <div className="pane-toolbar">
            <h2><Icon name="box" size={14} /> Where the vCPUs and memory are booked</h2>
          </div>
          <BookedBars parts={parts} quota={quota} />
          <div className="tblwrap">
            <table className="tbl booked-tbl">
              <thead><tr><th>booked by</th><th className="col-num">count</th><th className="col-num">vCPUs</th><th className="col-num">memory</th></tr></thead>
              <tbody>
                {parts.map((p) => (
                  <tr key={p.key}>
                    <td><span className={'swatch sw-' + p.key} aria-hidden="true" /> {p.label}</td>
                    <td className="col-num mono">{p.count === null ? <span className="muted">—</span> : p.count}</td>
                    <td className="col-num mono">{p.vcpus}</td>
                    <td className="col-num mono">{fmtQuota({ unit: 'MiB' }, p.memoryMib)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="hint">
            A sandbox books its vCPUs and memory in every state: paused or hibernated it still counts, until it is
            deleted. The environments' share is the rest of the project's use, by their current deployments. The lists
            are read apart from the use, so a share can lag a few seconds.
          </p>
        </section>)}

      {quota && (
        <p className="footnote muted small">
          The platform's operators set these limits{access.can.operator
            ? <>: you are one, change them in the <Link to="/projects" navigate={navigate}>platform's project list</Link>.</>
            : '; ask them for more.'} A limit is checked when something new is asked for: past it the request is
          refused with <code>quota_exceeded</code>. What runs stays when a limit goes down. No limit: nothing is counted
          against one. The daily quotas of logs and traces are apart.
          {access.account && <> Its use is charged to account <b>{accountLabel(access.account)}</b>.</>}
        </p>)}
    </div>
  );
}

function UsageRow({ row: r }) {
  const pct = r.share === null ? null : Math.round(r.share * 100);
  const os = r.key === 'vcpus' || r.key === 'memoryMib';
  let state = 'no limit';
  if (r.limit !== null) {
    const left = Math.max(0, r.limit - r.use);
    state = r.use >= r.limit ? (r.limit === 0 ? 'none allowed' : 'at the limit') : `${fmtQuota(r, left)} left`;
  }
  return (
    <div className={'usage-row' + (r.tone ? ' t-' + r.tone : '')}>
      <div className="usage-head">
        <span className="usage-k">{r.label}</span>
        <span className="usage-v">{fmtQuota(r, r.use)}
          <span className="muted">{r.limit !== null ? ` / ${fmtQuota(r, r.limit)}` : ''}</span></span>
        <span className="spacer" />
        <span className={'usage-state small' + (r.tone ? ' ' + r.tone : ' muted')}>{state}{pct !== null && r.limit > 0 ? ` · ${pct}%` : ''}</span>
      </div>
      <span className={'usage-bar' + (os ? ' os' : '') + (r.limit === null ? ' none' : '')} aria-hidden="true">
        <span style={{ width: pct === null ? '0%' : `${Math.min(100, pct)}%` }} />
      </span>
      <p className="usage-what small"><span className="muted">Counts:</span> {COUNTS[r.key]}</p>
      <p className="usage-what small"><span className="muted">At the limit:</span> {AT_LIMIT[r.key]}</p>
    </div>
  );
}

// BookedBars are the parts of the vCPUs and the memory in use as one bar
// each, against the limit when there is one.
function BookedBars({ parts, quota }) {
  const bars = [
    { key: 'vcpus', label: 'vCPUs', unit: '' },
    { key: 'memoryMib', label: 'memory', unit: 'MiB' },
  ];
  return (
    <div className="booked-bars">
      {bars.map((b) => {
        const total = parts.reduce((a, p) => a + p[b.key], 0);
        const limit = quota.limits && quota.limits[b.key] !== undefined && quota.limits[b.key] !== null
          ? Number(quota.limits[b.key]) : null;
        const scale = Math.max(total, limit || 0) || 1;
        return (
          <div key={b.key} className="booked-bar">
            <span className="usage-k">{b.label}</span>
            <span className="stack" aria-hidden="true">
              {parts.map((p) => p[b.key] > 0 && (
                <span key={p.key} className={'sw-' + p.key} style={{ width: `${(p[b.key] / scale) * 100}%` }}
                  title={`${p.label}: ${fmtQuota({ unit: b.unit }, p[b.key])}`} />))}
            </span>
            <span className="mono small">{fmtQuota({ unit: b.unit }, total)}
              <span className="muted">{limit !== null ? ` / ${fmtQuota({ unit: b.unit }, limit)}` : ' · no limit'}</span></span>
          </div>
        );
      })}
    </div>
  );
}

// --------------------------------------------------------------- settings --

export function ProjectSettings({ project, navigate }) {
  const access = useAccess(project);
  const c = access.can;
  const { reload } = useMe();
  const [armed, arm, disarm] = useArm();
  const [err, setErr] = useState('');
  const [info, setInfo] = useState(null);
  const [deleting, setDeleting] = useState(false);

  // An operator reads the project from the admin list: its zone, its
  // maker, whether it is being deleted. A person's answers carry no zone.
  useEffect(() => {
    if (!c.operator) return undefined;
    let live = true;
    api.listProjects().then((r) => {
      if (live) setInfo((r.projects || []).find((t) => t.name === project) || null);
    }, () => {});
    return () => { live = false; };
  }, [c.operator, project]);

  const envs = access.environments || [];
  return (
    <div className="projectpage">
      <header className="pagehead">
        <div className="pagehead-title">
          <span className="kind">Project</span>
          <h1>Settings</h1>
        </div>
        <div className="facts">
          <span>project <b className="mono">{project}</b></span>
          <span>your role <b>{access.role || (envs.length ? 'on environments alone' : '—')}</b></span>
        </div>
      </header>

      <section className="pane">
        <div className="pane-toolbar"><h2><Icon name="folder" size={14} /> This project</h2></div>
        <div className="kv-grid">
          <KV k="name" className="mono">{project}</KV>
          <KV k="your role">
            {access.role ? <RoleBadge role={access.role} /> : <span className="muted">none on the project</span>}
            {c.operator && <span className="muted small"> · as an operator: admin in every project, no member of it</span>}
          </KV>
          {envs.length > 0 && (
            <KV k="environments">
              {envs.map((e) => <span key={e.id} className="grantchip">{e.name} · {e.role}</span>)}
            </KV>)}
          {info && (
            <>
              <KV k="zone" className="mono">{info.zone || <span className="muted">none: the platform serves no apps zone</span>}</KV>
              <KV k="created">{fmtDateTime(info.createdAt)} <Ago iso={info.createdAt} /></KV>
              <KV k="made by">{info.madeBy ? <span className="mono small">{info.madeBy}</span> : <span className="muted">an operator</span>}</KV>
              {info.deleting && <KV k="state"><span className="bad">being deleted</span></KV>}
            </>)}
        </div>
        <p className="hint">
          {access.role === 'admin' ? 'As its admin you hand out roles on the People page. ' : ''}
          The project&apos;s zone and limits are the platform&apos;s operators&apos;; its account and its deletion are its admins&apos;.
        </p>
      </section>

      {(access.member || c.operator) && <ProjectAccount project={project} access={access} />}

      {c.leave && (
        <section className="pane danger-zone">
          <div className="pane-toolbar"><h2><Icon name="logout" size={14} /> Leave this project</h2></div>
          <p className="small">
            Gives up every grant you hold in <span className="mono">{project}</span>, on the project and on its
            environments. You see it no more until someone invites you again. Its last admin cannot leave: make
            another member admin first.
          </p>
          <ErrBox error={err} />
          <ArmButton armKey="leave" armed={armed} arm={arm} disarm={disarm} icon="logout"
            confirmLabel="really leave?" title="give up every grant you hold in this project"
            onConfirm={async () => {
              setErr('');
              try {
                await api.leaveProject();
                await reload();
                navigate('/projects');
              } catch (e) {
                setErr(e.message);
              }
            }}>Leave {project}</ArmButton>
        </section>)}

      {c.deleteProject && (
        <section className="pane danger-zone">
          <div className="pane-toolbar"><h2><Icon name="trash" size={14} /> Delete this project</h2></div>
          <p className="small">
            Deletes <span className="mono">{project}</span> with everything in it: its environments with their
            deployments, secrets and addons&apos; data, its sandboxes and jobs, its tokens, domains and grants. Its tokens
            stop at once; the rest goes within minutes. It cannot be undone, and its name is free for anyone then.
          </p>
          <button className="danger" onClick={() => setDeleting(true)}><Icon name="trash" size={14} /> Delete project</button>
        </section>)}
      {deleting && (
        <DeleteProjectDialog project={project} onClose={() => setDeleting(false)}
          onDone={async () => {
            await reload().catch(() => {});
            navigate('/projects');
          }} />)}

      {c.operator && (
        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="crown" size={14} /> Limits and zone</h2></div>
          <p className="small">
            You are an operator: no member to leave. A project's limits and its zone are set in the platform's
            project list.
          </p>
          <a className="button" href="/projects" onClick={(e) => { e.preventDefault(); navigate('/projects'); }}>
            <Icon name="folder" size={14} /> Platform · Projects <Icon name="arrowRight" size={12} /></a>
        </section>)}
    </div>
  );
}

// DeleteProjectDialog asks for the project's name before it deletes the
// project (DELETE /api/v1/project with the name again): Delete is offered
// once the name typed is the project's. It is modal, and while it works
// it does not close; a refusal is shown in it and nothing changes.
export function DeleteProjectDialog({ project, onClose, onDone }) {
  const [typed, setTyped] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const box = useRef(null);
  const input = useRef(null);
  const busyNow = useRef(false);
  busyNow.current = busy;
  useModal(box, onClose, busyNow);
  // Then the name's field.
  useEffect(() => { if (input.current) input.current.focus(); }, []);
  const matches = typed === project;
  const del = async () => {
    if (!matches) return;
    setBusy(true);
    setErr('');
    try {
      await api.deleteOwnProject(project);
      await onDone();
    } catch (e) {
      setErr(e.message);
      setBusy(false);
    }
  };
  return (
    <div className="palette-back" onMouseDown={() => { if (!busyNow.current) onClose(); }}>
      <div className="palette dialog" role="dialog" aria-modal="true" aria-label={`Delete ${project}`} tabIndex={-1} ref={box}
        onMouseDown={(e) => e.stopPropagation()}>
        <h3><Icon name="trash" size={16} /> Delete <span className="mono">{project}</span></h3>
        <p className="small">
          Everything in it goes: environments, deployments, secrets, addons&apos; data, sandboxes, jobs, tokens,
          domains and grants. This cannot be undone.
        </p>
        <form onSubmit={(e) => { e.preventDefault(); del(); }}>
          <label className="delete-confirm small">Type <b className="mono">{project}</b> to confirm
            <input ref={input} className="mono" value={typed} autoComplete="off" spellCheck={false} disabled={busy}
              aria-label="the project's name, to confirm" onChange={(e) => setTyped(e.target.value)} />
          </label>
          <ErrBox error={err} />
          <div className="actionrow">
            <button type="submit" className="danger" disabled={busy || !matches}>
              <Icon name="trash" size={14} /> {busy ? 'Deleting…' : 'Delete project'}</button>
            <button type="button" className="ghost" disabled={busy} onClick={onClose}>Cancel</button>
          </div>
        </form>
      </div>
    </div>
  );
}

// ProjectAccount is the account the project is on (ADR 0059): its name
// and title, since when, its moves; for an admin of the project who may
// put projects on another open account, or an operator, the move. Every
// member reads it; a move changes who is charged from that moment, and
// nothing else.
// initial is GET /api/v1/project/account's answer when it is read already.
export function ProjectAccount({ project, access, initial = null }) {
  const { me, reload } = useMe();
  const c = access.can;
  const [info, setInfo] = useState(initial);
  const [err, setErr] = useState('');
  const [missing, setMissing] = useState(false);
  const [target, setTarget] = useState('');
  const [operatorAccounts, setOperatorAccounts] = useState(null);
  const [moved, setMoved] = useState(null);
  const [armed, arm, disarm] = useArm();
  const refresh = useCallback(async () => {
    try {
      setInfo(await api.getProjectAccount());
      setErr('');
    } catch (e) {
      if (notAvailable(e)) setMissing(true);
      else setErr(e.message);
    }
  }, []);
  useEffect(() => { refresh(); }, [refresh]);
  // An operator moves to any open account; a person to those of /auth/me.
  useEffect(() => {
    if (!c.operator) return undefined;
    let live = true;
    api.adminListAccounts().then((r) => { if (live) setOperatorAccounts(r.accounts || []); }, () => { if (live) setOperatorAccounts([]); });
    return () => { live = false; };
  }, [c.operator]);
  if (missing) return null;
  const current = info && info.account ? info.account.name : '';
  const options = moveTargets(c.operator ? operatorAccounts : myAccounts(me), current);
  const pick = options.find((a) => a.name === target) || options[0] || null;
  const mayMove = c.adminProject;
  const moves = [...((info && info.moves) || [])].sort((a, b) => (b.at || '').localeCompare(a.at || ''));
  return (
    <section className="pane">
      <div className="pane-toolbar"><h2><Icon name="building" size={14} /> Account</h2></div>
      <ErrBox error={err} />
      {!info && !err && <p className="empty">Loading…</p>}
      {info && (
        <div className="kv-grid">
          <KV k="account"><span className="strong">{info.account.title || info.account.name}</span> <span className="mono muted">{info.account.name}</span></KV>
          {info.since && <KV k="since">{fmtDateTime(info.since)} <Ago iso={info.since} /></KV>}
        </div>)}
      {moves.length > 0 && (
        <ul className="moves">
          {moves.map((m, i) => (
            <li key={`${m.at}-${i}`}><span className="muted small">{fmtDateTime(m.at)}</span> <span>{moveText(m)}</span></li>))}
        </ul>)}
      {info && mayMove && (
        options.length === 0
          ? <p className="hint">You are a member of no other open account. An account&apos;s admin can invite you, or an operator moves it.</p>
          : (
            <div className="pf-form">
              <span className="muted small">move to</span>
              <select value={pick ? pick.name : ''} aria-label="the account to move to" onChange={(e) => setTarget(e.target.value)}>
                {options.map((a) => <option key={a.name} value={a.name}>{accountLabel(a)}</option>)}
              </select>
              <ArmButton armKey="move" armed={armed} arm={arm} disarm={disarm} disabled={!pick} icon="building"
                confirmLabel="move? charges go there from now on" title="move the project to this account"
                onConfirm={async () => {
                  setErr('');
                  try {
                    const res = await api.moveProject(pick.name);
                    setMoved(res);
                    await refresh();
                    reload().catch(() => {});
                  } catch (e) {
                    setErr(e.message);
                  }
                }}>Move to this account</ArmButton>
            </div>))}
      {info && mayMove && options.length > 0 && (
        <p className="hint">
          From that moment its use is charged to {pick ? accountLabel(pick) : 'the account you pick'}. Its name, hosts, domains,
          people, tokens, limits and workloads stay as they are: nothing restarts. The account it leaves is not asked.
        </p>)}
      {moved && <p className="small ok">{moved.moved ? `Moved to ${accountLabel(moved.account)}.` : `It is on ${accountLabel(moved.account)} already.`}</p>}
    </section>
  );
}
