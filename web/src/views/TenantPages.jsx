// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api, notAvailable } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, KV, LiveBadge, useArm, usePoll } from '../components.jsx';
import { fmtDateTime } from '../format.js';
import { fmtQuota, quotaRows, useAccess, useMe } from '../people.js';
import { useSummary } from '../summary.jsx';
import { RoleBadge } from './People.jsx';
import { AT_LIMIT, COUNTS, booked } from './usage.js';
import '../styles/tenant.css';

// The tenant's usage (its use against the limits the platform's
// operators set, ADR 0031 "Quotas per tenant") and its settings (who you
// are in it, leaving it; for operators the way to the platform's tenant
// list, where limits, zone and deletion are).

// ------------------------------------------------------------------ usage --

function Link({ to, navigate, children }) {
  return <a href={to} onClick={(e) => { e.preventDefault(); navigate(to); }}>{children}</a>;
}

export function Usage({ tenant, navigate }) {
  const access = useAccess(tenant);
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
    <div className="tenantpage">
      <header className="pagehead">
        <div className="pagehead-title">
          <span className="kind">Tenant</span>
          <h1>Usage</h1>
          <span className="pagehead-actions"><LiveBadge lastOk={lastOk} /></span>
        </div>
        <div className="facts">
          <span>tenant <b className="mono">{tenant}</b></span>
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
            A sandbox books its vCPUs and memory in every state: asleep or hibernated it still counts, until it is
            deleted. The environments' share is the rest of the tenant's use, by their current deployments. The lists
            are read apart from the use, so a share can lag a few seconds.
          </p>
        </section>)}

      {quota && (
        <p className="footnote muted small">
          The platform's operators set these limits{access.can.operator
            ? <>: you are one, change them in the <Link to="/tenants" navigate={navigate}>platform's tenant list</Link>.</>
            : '; ask them for more.'} A limit is checked when something new is asked for: past it the request is
          refused with <code>quota_exceeded</code>. What runs stays when a limit goes down. No limit: nothing is counted
          against one. The daily quotas of logs and traces are apart.
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

export function TenantSettings({ tenant, navigate }) {
  const access = useAccess(tenant);
  const c = access.can;
  const { reload } = useMe();
  const [armed, arm, disarm] = useArm();
  const [err, setErr] = useState('');
  const [info, setInfo] = useState(null);

  // An operator reads the tenant from the admin list: its zone, its
  // maker, whether it is being deleted. A person's answers carry no zone.
  useEffect(() => {
    if (!c.operator) return undefined;
    let live = true;
    api.listTenants().then((r) => {
      if (live) setInfo((r.tenants || []).find((t) => t.name === tenant) || null);
    }, () => {});
    return () => { live = false; };
  }, [c.operator, tenant]);

  const envs = access.environments || [];
  return (
    <div className="tenantpage">
      <header className="pagehead">
        <div className="pagehead-title">
          <span className="kind">Tenant</span>
          <h1>Settings</h1>
        </div>
        <div className="facts">
          <span>tenant <b className="mono">{tenant}</b></span>
          <span>your role <b>{access.role || (envs.length ? 'on environments alone' : '—')}</b></span>
        </div>
      </header>

      <section className="pane">
        <div className="pane-toolbar"><h2><Icon name="building" size={14} /> This tenant</h2></div>
        <div className="kv-grid">
          <KV k="name" className="mono">{tenant}</KV>
          <KV k="your role">
            {access.role ? <RoleBadge role={access.role} /> : <span className="muted">none on the tenant</span>}
            {c.operator && <span className="muted small"> · as an operator: admin in every tenant, no member of it</span>}
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
          The tenant's name, zone, limits and deletion are the platform's operators'.
        </p>
      </section>

      {c.leave && (
        <section className="pane danger-zone">
          <div className="pane-toolbar"><h2><Icon name="logout" size={14} /> Leave this tenant</h2></div>
          <p className="small">
            Gives up every grant you hold in <span className="mono">{tenant}</span>, on the tenant and on its
            environments. You see it no more until someone invites you again. Its last admin cannot leave: make
            another member admin first.
          </p>
          <ErrBox error={err} />
          <ArmButton armKey="leave" armed={armed} arm={arm} disarm={disarm} icon="logout"
            confirmLabel="really leave?" title="give up every grant you hold in this tenant"
            onConfirm={async () => {
              setErr('');
              try {
                await api.leaveTenant();
                await reload();
                navigate('/tenants');
              } catch (e) {
                setErr(e.message);
              }
            }}>Leave {tenant}</ArmButton>
        </section>)}

      {c.operator && (
        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="crown" size={14} /> Limits, zone and deletion</h2></div>
          <p className="small">
            You are an operator: no member to leave. A tenant's limits, its zone and its deletion are set in the
            platform's tenant list.
          </p>
          <a className="button" href="/tenants" onClick={(e) => { e.preventDefault(); navigate('/tenants'); }}>
            <Icon name="building" size={14} /> Platform · Tenants <Icon name="arrowRight" size={12} /></a>
        </section>)}
    </div>
  );
}
