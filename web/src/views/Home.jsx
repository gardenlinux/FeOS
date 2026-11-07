// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React from 'react';
import { Icon } from '../Icon.jsx';
import { Ago, StateDot, useNow } from '../components.jsx';
import { fmtQuota, quotaRows, useAccess } from '../people.js';
import { envPath } from '../routes.js';
import { useSummary } from '../summary.jsx';
import { deploymentText, deploymentTone } from './envs.js';
import { stateWords } from './sandboxes.js';
import { ToneDot } from './Environments.jsx';
import { RoleBadge } from './People.jsx';

// A tenant's Home: first what needs someone (attention.js), each item
// with the place that fixes it; then what runs, its environments and its
// most recently active sandboxes; then the use against the limits. All
// of it from the tenant's summary (summary.jsx), which the sidebar's
// counts read too.

// SANDBOXES_SHOWN is how many of the most recently active sandboxes show.
const SANDBOXES_SHOWN = 6;


// mostRecent orders sandboxes by their last activity, newest first.
export function mostRecent(sandboxes) {
  return [...(sandboxes || [])].sort((a, b) => (b.updatedAt || '').localeCompare(a.updatedAt || ''));
}

export function Home({ tenant, navigate }) {
  const access = useAccess(tenant);
  const c = access.can;
  const sum = useSummary();
  useNow(10000);
  const enc = encodeURIComponent;
  const base = `/tenants/${enc(tenant)}`;
  const link = (to) => (e) => { e.preventDefault(); navigate(to); };
  const loaded = sum.at > 0;

  const sandboxes = sum.sandboxes || [];
  const awake = sandboxes.filter((s) => s.clientState === 'running').length;
  const envs = sum.environments || [];
  const quota = sum.quota ? quotaRows(sum.quota).filter((r) => r.limit !== null || r.use > 0) : [];

  return (
    <div className="home">
      <header className="pagehead">
        <div className="pagehead-title">
          <span className="kind">Tenant</span>
          <h1>{tenant}</h1>
          {access.role && <RoleBadge role={access.role} />}
          <span className="spacer" />
          {c.writeTenant && (
            <a className="button" href={`${base}/sandboxes`} onClick={link(`${base}/sandboxes`)}>
              <Icon name="box" size={14} /> Sandboxes</a>)}
          {c.writeTenant && (
            <a className="button primary" href={`${base}/new-environment`} onClick={link(`${base}/new-environment`)}>
              <Icon name="plus" size={14} /> New environment</a>)}
        </div>
        <div className="facts">
          {sum.environments && <span><b>{envs.length}</b> environment{envs.length === 1 ? '' : 's'}</span>}
          {sum.sandboxes && <span><b>{sandboxes.length}</b> sandbox{sandboxes.length === 1 ? '' : 'es'}, {awake} running</span>}
          {sum.domains && <span><b>{sum.domains.length}</b> domain{sum.domains.length === 1 ? '' : 's'}</span>}
        </div>
      </header>

      <section className="pane">
        <div className="pane-toolbar"><h2><Icon name="inbox" size={14} /> Needs you</h2></div>
        {!loaded && <p className="empty">Looking…</p>}
        {loaded && sum.items.length === 0 && (
          <p className="allclear"><Icon name="check" size={14} /> Nothing needs you. Every environment serves, no domain waits, no limit is near.</p>)}
        {sum.items.length > 0 && (
          <div className="inbox">
            {sum.items.map((it) => (
              <a key={it.key} className={'inbox-item sev-' + it.severity} href={it.to} onClick={link(it.to)}>
                <span className="sev" aria-hidden="true" />
                <span className="inbox-text">
                  <span className="inbox-what">
                    <span className="mono">{it.title}</span>
                    {it.object && <span className="inbox-obj mono">{it.object}</span>}
                  </span>
                  <span className="inbox-why">
                    {it.detail}
                    {it.since && <>{it.detail ? ' · ' : ''}since <Ago iso={it.since} /></>}
                  </span>
                </span>
                <span className="button mini">{it.action}</span>
              </a>
            ))}
          </div>)}
      </section>

      <div className="home-grid">
        {sum.environments && (
          <section className="pane">
            <div className="pane-toolbar">
              <h2><Icon name="layers" size={14} /> Environments</h2>
              <span className="spacer" />
              <a className="small" href={envPath(tenant)} onClick={link(envPath(tenant))}>all <Icon name="arrowRight" size={12} /></a>
            </div>
            {envs.length === 0 && (
              <p className="empty">None yet. {c.writeTenant && (
                <a href={`${base}/new-environment`} onClick={link(`${base}/new-environment`)}>Deploy a container →</a>)}</p>)}
            {envs.length > 0 && (
              <div className="tblwrap"><table className="tbl">
                <thead><tr><th>name</th><th>deployment</th><th>hosts</th></tr></thead>
                <tbody>
                  {envs.map((e) => (
                    <tr key={e.id} className="clickable" onClick={() => navigate(envPath(tenant, e.name))}>
                      <td><span className="cell-state"><ToneDot tone={deploymentTone(e.state)} title={e.state} /> <span className="strong">{e.name}</span></span></td>
                      <td className="mono small">{deploymentText(e)}</td>
                      <td className="mono small trunc">{e.public ? (e.hosts || []).join(', ') || '—' : <span className="muted">route door only</span>}</td>
                    </tr>
                  ))}
                </tbody>
              </table></div>)}
          </section>)}

        {sum.sandboxes && (
          <section className="pane">
            <div className="pane-toolbar">
              <h2><Icon name="box" size={14} /> Sandboxes</h2>
              <span className="spacer" />
              <a className="small" href={`${base}/sandboxes`} onClick={link(`${base}/sandboxes`)}>all <Icon name="arrowRight" size={12} /></a>
            </div>
            {sandboxes.length === 0 && (
              <p className="empty">None yet. {c.writeTenant && (
                <a href={`${base}/sandboxes`} onClick={link(`${base}/sandboxes`)}>Create one and connect an agent →</a>)}</p>)}
            {sandboxes.length > 0 && (
              <div className="tblwrap"><table className="tbl">
                <thead><tr><th>sandbox</th><th>state</th><th>last activity</th></tr></thead>
                <tbody>
                  {mostRecent(sandboxes).slice(0, SANDBOXES_SHOWN).map((s) => (
                    <tr key={s.sandboxId} className="clickable" onClick={() => navigate(`${base}/sandboxes/${enc(s.sandboxId)}`)}>
                      <td className="mono">{s.sandboxId}{s.owner && <div className="muted small">{s.owner}</div>}</td>
                      <td><span className="cell-state"><StateDot state={s.clientState} /> {s.clientState || 'unknown'}
                        {stateWords(s).where && <span className="muted small mono">{stateWords(s).where}</span>}</span></td>
                      <td className="muted small"><Ago iso={s.updatedAt} /></td>
                    </tr>
                  ))}
                </tbody>
              </table></div>)}
          </section>)}
      </div>

      {quota.length > 0 && (
        <section className="pane">
          <div className="pane-toolbar">
            <h2><Icon name="gauge" size={14} /> Usage against limits</h2>
            <span className="spacer" />
            <a className="small" href={`${base}/usage`} onClick={link(`${base}/usage`)}>usage <Icon name="arrowRight" size={12} /></a>
          </div>
          <div className="meters">
            {quota.map((r) => (
              <div key={r.key} className={'meter' + (r.tone ? ' t-' + r.tone : '')}>
                <span className="meter-k">{r.label}</span>
                <span className="meter-v">{fmtQuota(r, r.use)}
                  <span className="muted">{r.limit !== null ? ` / ${fmtQuota(r, r.limit)}` : ' · no limit'}</span></span>
                <span className={'meter-bar' + (r.key === 'vcpus' || r.key === 'memoryMib' ? ' os' : '')} aria-hidden="true">
                  <span style={{ width: r.share === null ? '0%' : `${Math.min(100, Math.round(r.share * 100))}%` }} />
                </span>
              </div>
            ))}
          </div>
        </section>)}
    </div>
  );
}
