// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { Fragment, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, CopyBtn, EnvName, ErrBox, KV, LiveBadge, StateLabel, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDate, fmtDateTime, prettyJson, shortId } from '../format.js';
import { newest } from '../poll.js';
import { ENV_LENSES, ENV_TABS, ENV_TAB_LABELS, LENS_PAGES, PAGE_LABELS, envPath, lensOf, newEnvPath } from '../routes.js';
import { useEnvStatus } from '../envstatus.jsx';
import { focusOf, queryOf } from '../logs.js';
import { LogSearch } from './LogSearch.jsx';
import { Traces } from './Traces.jsx';
import { Builder } from './Builder.jsx';
import { atLeast, envRole, useAccess } from '../people.js';
import { useSummary } from '../summary.jsx';
import {
  changeTone, conditionText, defaultHost, envCounts, exitText, findObject, fmtValue, hasEvidence, heavyEffect,
  incidentObject, isAttached, labelPairs, logSourceText, logsProblem, mayRevertTo, originText, parseLabelFilter,
  phaseTone, deploymentTone, preflightRows, deploymentText, revertOffer, rolloutNote, specBindings, statusState,
  tagPlan,
} from './envs.js';
import {
  AccessTab, ConfirmButton, EnvLink, HostLink, NEEDS, OpNote, SecretsTab, SettingsTab, TrafficTab, useOp,
} from './EnvTabs.jsx';
import { ObjectPage } from './ObjectPage.jsx';
import { AboutBox, ActivityTab, EnvOverview, useActivity } from './EnvOverview.jsx';
import { EnvGroups } from './EnvGroups.jsx';
import { EnvPods } from './EnvPods.jsx';
import '../styles/environments.css';

// A tenant's environments: the list with a label filter, and an
// environment's page: a head with its state and what runs where, Revert
// (while the latest deployment failed or is under way and an earlier one
// serves) and New deployment, the builder (Builder.jsx, ADR 0032); then
// its views, one tab each (ENV_LENSES): the overview (EnvOverview.jsx:
// what needs someone, traffic and reach, the objects as cards, the
// bindings, recent activity, its sandboxes and jobs: EnvPods.jsx, ADR
// 0035), the activity (everything, the deployments
// with revert and tags, the incidents with their evidence), the logs and
// traces, the traffic, and the settings (general, secrets and
// credentials, domains and access; EnvTabs.jsx). The sidebar picks the
// environment or one of its objects, whose page (ObjectPage.jsx) has the
// same views where it has them. No secret's value is ever read; a route
// token shows once, at its mint. The environment, the tab and its item
// are the route's: /tenants/:t/environments/:env/:tab/:item.

export function Environments({ tenant, env, tab, item, otab = 'overview', replica, search, navigate }) {
  if (env) {
    return (
      <EnvDetail key={env} tenant={tenant} env={env} tab={tab} item={item} otab={otab} replica={replica} search={search}
        navigate={navigate} />
    );
  }
  return <EnvList tenant={tenant} navigate={navigate} />;
}

// envCan is what a role may do on an environment (ADR 0031, the route
// needs): write on it (revert, tags, secrets, restarts, runs, checks),
// admin on it (public, route tokens), a delete (admin on it or write on
// the tenant), a clone (write on the tenant); operator: the nodes are
// theirs to open.
export function envCan(access, env) {
  const role = envRole(access, env);
  const writeTenant = atLeast((access && access.role) || '', 'write');
  return {
    write: atLeast(role, 'write'),
    admin: atLeast(role, 'admin'),
    del: atLeast(role, 'admin') || writeTenant,
    clone: writeTenant,
    operator: !!(access && access.operator),
  };
}

// ---------------------------------------------------------------- widgets --

// Link is a link within the UI: a real href (so that a new tab works),
// the router on a plain click.
const Link = EnvLink;

export function ToneDot({ tone, title }) {
  return <span className={'sdot ' + tone} title={title} />;
}

export function ToneLabel({ tone, children }) {
  return <span className={'state-label ' + tone}><ToneDot tone={tone} title={String(children)} />{children}</span>;
}

// Labels shows a label map as chips; with onLabel a chip filters by its
// label.
export function Labels({ labels, onLabel }) {
  const pairs = labelPairs(labels);
  if (pairs.length === 0) return <span className="muted">—</span>;
  return (
    <span className="labels">
      {pairs.map(([k, v]) => (onLabel
        ? (
          <button key={k} type="button" className="labelchip" title={`filter by ${k}=${v}`}
            onClick={(e) => { e.stopPropagation(); onLabel(k, v); }}>{k}=<b>{v}</b></button>)
        : <span key={k} className="labelchip" title={`${k}=${v}`}>{k}=<b>{v}</b></span>))}
    </span>
  );
}

function Annotations({ annotations }) {
  const pairs = labelPairs(annotations);
  if (pairs.length === 0) return <span className="muted">—</span>;
  return (
    <span className="labels">
      {pairs.map(([k, v]) => <span key={k} className="labelchip" title={`${k}=${v}`}>{k}=<b>{v}</b></span>)}
    </span>
  );
}

// ------------------------------------------------------------------- list --

function EnvList({ tenant, navigate }) {
  // A new environment needs write on the tenant (ADR 0031).
  const access = useAccess(tenant);
  const [envs, setEnvs] = useState(null);
  const [err, setErr] = useState('');
  const [labels, setLabels] = useState([]);
  const [draft, setDraft] = useState('');
  const [bad, setBad] = useState([]);

  // A changed filter starts a request at once while one with the old
  // filter may still be under way; only the newest answer is shown.
  const [request] = useState(() => newest());
  const key = labels.join('\n');
  const refresh = useCallback(async () => {
    const current = request();
    try {
      const res = await api.listEnvironments(key ? key.split('\n') : []);
      if (!current()) return;
      setEnvs(res.environments || []);
      setErr('');
    } catch (e) {
      if (current()) setErr(e.message);
    }
  }, [key, request]);
  const lastOk = usePoll(refresh, 4000);
  useNow(1000);

  const addLabels = (list) => setLabels((cur) => [...cur, ...list.filter((l) => !cur.includes(l))]);
  const counts = useMemo(() => envCounts(envs), [envs]);

  return (
    <>
      <header className="pagehead">
        <div className="pagehead-title">
          <h1>Environments</h1>
          <span className="pagehead-actions">
            <button className="primary" disabled={!access.can.writeTenant} title={access.can.writeTenant ? '' : 'needs write on this tenant'}
              onClick={() => navigate(newEnvPath(tenant))}>
              <Icon name="plus" size={14} /> New environment</button>
          </span>
        </div>
        <div className="facts">
          <span><b>{envs ? envs.length : '…'}</b> environment{envs && envs.length === 1 ? '' : 's'}{labels.length > 0 ? ' with these labels' : ''}</span>
          <span className="ok" title="deployed">● <b>{counts.deployed}</b> deployed</span>
          {counts.deploying > 0 && <span className="acc" title="waiting or deploying">● <b>{counts.deploying}</b> deploying</span>}
          {counts.failed > 0 && <span className="bad" title="the latest deployment failed">● <b>{counts.failed}</b> failed</span>}
          {counts.incidents > 0 && <span className="bad" title="open incidents">▲ <b>{counts.incidents}</b> open incident{counts.incidents === 1 ? '' : 's'}</span>}
        </div>
      </header>
      <section className="pane">
        <div className="pane-toolbar">
          <form className="filterbar slim grow" onSubmit={(e) => {
            e.preventDefault();
            const p = parseLabelFilter(draft);
            setBad(p.bad);
            addLabels(p.labels);
            setDraft(p.bad.join(' '));
          }}>
            <span className="searchwrap grow">
              <Icon name="search" size={13} />
              <input className="searchbare" placeholder="filter by label: key=value, then Enter" value={draft}
                onChange={(e) => { setDraft(e.target.value); setBad([]); }} />
            </span>
            {labels.map((l) => (
              <button key={l} type="button" className="chipbtn on mono" title="remove this filter"
                onClick={() => setLabels((cur) => cur.filter((x) => x !== l))}>
                {l} <Icon name="x" size={11} />
              </button>
            ))}
          </form>
          <LiveBadge lastOk={lastOk} />
        </div>
        <ErrBox error={err} />
        {bad.length > 0 && <p className="notice small">A label filter is key=value: {bad.join(', ')}</p>}

        <EnvTable envs={envs} filtered={labels.length > 0} tenant={tenant} navigate={navigate}
          onLabel={(k, v) => addLabels([`${k}=${v}`])} />
        {!envs && !err && <p className="empty">Loading…</p>}
      </section>
    </>
  );
}

// EnvTable is the environments' table (GET /api/v1/environments): the
// state, the deployments in words, the hosts; a label chip filters by its
// label.
export function EnvTable({ envs, filtered, tenant, navigate, onLabel }) {
  const list = envs || [];
  return (
    <>
      <div className="tblwrap">
        <table className="tbl envs-tbl">
          <thead>
            <tr>
              <th className="col-state" />
              <th>name</th>
              <th>deployment</th>
              <th>hosts</th>
              <th>labels</th>
              <th className="col-num">incidents</th>
              <th className="col-when">created</th>
            </tr>
          </thead>
          <tbody>
            {list.map((e) => {
              const to = envPath(tenant, e.name);
              const tone = deploymentTone(e.state);
              return (
                <tr key={e.id} className="clickable" onClick={() => navigate(to)}>
                  <td className="col-state"><ToneDot tone={tone} title={e.state} /></td>
                  <td className="nowrap">
                    <Link to={to} navigate={navigate} className="mono strong">{e.name}</Link>
                    <span className="muted small mono idsub" title={e.id}>{shortId(e.id)}</span>
                  </td>
                  <td className={'mono small nowrap state-label ' + tone}>{deploymentText(e)}</td>
                  <td className="small">
                    {e.public
                      ? ((e.hosts || []).length > 0
                        ? <div className="envs-hosts">{e.hosts.map((h) => <span key={h} className="mono host">{h}</span>)}</div>
                        : <span className="muted">public, no host</span>)
                      : <span className="muted">route door only</span>}
                  </td>
                  <td><Labels labels={e.labels} onLabel={onLabel} /></td>
                  <td className="col-num">
                    {e.openIncidents > 0
                      ? <span className="badge bad">{e.openIncidents} open</span>
                      : <span className="muted">0</span>}
                  </td>
                  <td className="col-when muted" title={fmtDateTime(e.createdAt)}>{fmtDate(e.createdAt)}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {envs && list.length === 0 && (
        <p className="empty">{filtered ? 'No environment has these labels.' : 'No environments in this tenant.'}</p>)}
    </>
  );
}

// ----------------------------------------------------------------- detail --

// LENS_ICONS are the icons of the views in the tab bar.
const LENS_ICONS = {
  overview: 'gauge', activity: 'history', logs: 'file', traffic: 'chart', groups: 'shield', settings: 'settings',
};

// EnvDetail is an environment's page: its head, the tab bar of its views
// (ENV_LENSES; Activity and Settings with their own pages under it), and
// the view; an object's page instead when one is picked. The status is
// the shell's (envstatus.jsx), read for the sidebar and this page.
function EnvDetail({ tenant, env, tab, item, otab, replica, search, navigate }) {
  const status = useEnvStatus();
  const { err, gone, lastOk, refresh } = status;
  const st = status.env === env ? status.st : null;
  const access = useAccess(tenant);
  const sum = useSummary();
  const list = envPath(tenant);
  // The objects tab without an object is the overview, which lists them;
  // traces without a trace are the logs, where traces open from.
  const activeTab = tab === 'traces' && !item ? 'logs'
    : ENV_TABS.includes(tab) && !(tab === 'objects' && !item) ? tab : 'overview';
  const lens = lensOf(activeTab);

  useNow(1000);
  // changed follows a write: the page and the tenant's summary read again.
  const sumRefresh = sum.refresh;
  const changed = useCallback(() => { refresh(); sumRefresh(); }, [refresh, sumRefresh]);
  const ec = envCan(access, (st && st.id) || env);

  // Escape goes up: from an item to its tab's list, from there to the
  // environments; from the builder and an object's page to the overview;
  // from a trace to its lines.
  const up = activeTab === 'builder' || activeTab === 'objects' ? envPath(tenant, env)
    : activeTab === 'traces' ? envPath(tenant, env, 'logs', '', null, queryOf({ trace: item }))
      : item ? envPath(tenant, env, activeTab) : list;
  useEffect(() => {
    const onKey = (e) => {
      if (e.key === 'Escape' && !e.target.closest('input,textarea,select')) navigate(up);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [up, navigate]);

  if (gone && !st) {
    return (
      <section className="pane">
        <p className="error">No environment {env} in this tenant.</p>
        <button className="ghost" onClick={() => navigate(list)}><Icon name="arrowLeft" size={14} /> the environments</button>
      </section>
    );
  }
  const body = (el) => (st ? el : !err && <p className="empty">Loading…</p>);
  const go = (to) => (e) => { if (e.button === 0 && !e.metaKey && !e.ctrlKey) { e.preventDefault(); navigate(to); } };

  // An object's page has its own head and views, inside the environment.
  if (activeTab === 'objects') {
    return (
      <>
        <ErrBox error={err} />
        {body(<ObjectPage key={item} tenant={tenant} st={st} item={item} otab={otab} replica={replica} search={search}
          ec={ec} lastOk={lastOk} changed={changed} navigate={navigate} />)}
      </>
    );
  }

  const incidents = st ? Number(st.openIncidents) || 0 : 0;
  const pages = LENS_PAGES[lens];
  return (
    <>
      <EnvHead st={st} env={env} tenant={tenant} ec={ec} domains={sum.domains} editing={activeTab === 'builder'}
        lastOk={lastOk} changed={changed} navigate={navigate} />
      <nav className="tabbar env-tabs" aria-label="views">
        {ENV_LENSES.map((t) => {
          const to = envPath(tenant, env, t);
          return (
            <a key={t} href={to} className={'tab' + (lens === t ? ' on' : '')} aria-current={lens === t ? 'page' : undefined}
              onClick={go(to)}>
              <Icon name={LENS_ICONS[t]} size={15} /> {ENV_TAB_LABELS[t]}
              {t === 'activity' && incidents > 0 && <span className="tabcount" title="open incidents">{incidents}</span>}
            </a>
          );
        })}
      </nav>
      <ErrBox error={err} />
      <div className="tabbody">
        {pages && (
          <nav className="subtabs" aria-label={ENV_TAB_LABELS[lens]}>
            {pages.map((p) => {
              const to = envPath(tenant, env, p);
              return (
                <a key={p} href={to} className={'subtab' + (activeTab === p ? ' on' : '')} onClick={go(to)}
                  aria-current={activeTab === p ? 'page' : undefined}>
                  {PAGE_LABELS[p]}
                  {p === 'incidents' && incidents > 0 && <span className="tabcount" title="open incidents">{incidents}</span>}
                </a>
              );
            })}
          </nav>)}

        {activeTab === 'overview' && body(<OverviewTab st={st} tenant={tenant} ec={ec} changed={changed} navigate={navigate} />)}
        {activeTab === 'builder' && body(<Builder key={st && st.id} tenant={tenant} env={st && st.name} st={st} item={item} navigate={navigate} />)}
        {activeTab === 'activity' && <ActivityTab tenant={tenant} env={env} navigate={navigate} />}
        {activeTab === 'deployments' && (
          <DeploymentsTab tenant={tenant} env={env} st={st} item={item} ec={ec} changed={changed} navigate={navigate} />)}
        {activeTab === 'incidents' && (item
          ? <IncidentPane key={item} tenant={tenant} env={env} id={item} navigate={navigate} />
          : <IncidentsList tenant={tenant} env={env} navigate={navigate} />)}
        {activeTab === 'logs' && <LogsTab tenant={tenant} env={env} st={st} item="" replica={null} search={search} navigate={navigate} />}
        {activeTab === 'traces' && <Traces tenant={tenant} env={env} st={st} item={item} navigate={navigate} />}
        {activeTab === 'traffic' && <TrafficTab tenant={tenant} env={env} navigate={navigate} />}
        {activeTab === 'groups' && body(<EnvGroups tenant={tenant} st={st} ec={ec} navigate={navigate} />)}
        {activeTab === 'secrets' && (
          <>
            <SecretsTab env={env} st={st} ec={ec} changed={changed} />
            <Offloading env={env} st={st} tenant={tenant} ec={ec} navigate={navigate} />
          </>)}
        {activeTab === 'access' && (
          <AccessTab tenant={tenant} env={env} st={st} ec={ec} domains={sum.domains} changed={changed} navigate={navigate} />)}
        {activeTab === 'settings' && (
          <>
            {st && <AboutBox st={st} />}
            <SettingsTab tenant={tenant} env={env} st={st} ec={ec} changed={changed} navigate={navigate} />
          </>)}
      </div>
    </>
  );
}

// EnvHead is an environment's page head: its name, its state, the line
// of what runs and where it is reached; Revert to the deployment that
// serves while a later one failed or is under way, and New deployment
// (the builder; Done while it is open). Its labels, ID and creation are
// on Settings.
export function EnvHead({ st, env, tenant, ec, domains, editing, lastOk, changed, navigate }) {
  const [op, run, clear] = useOp();
  const state = statusState(st);
  const incidents = st ? Number(st.openIncidents) || 0 : 0;
  const tone = incidents > 0 ? 'failed' : deploymentTone(state);
  const offer = revertOffer(st);
  const dh = defaultHost(st, domains || []);
  const builder = envPath(tenant, env, 'builder');
  const go = (to) => (e) => { if (e.button === 0 && !e.metaKey && !e.ctrlKey) { e.preventDefault(); navigate(to); } };
  const revert = async () => {
    const r = await run(() => api.revertDeployment(st.name, offer, { base: st.deployment }),
      (res) => `Deployment ${res.number} reverts to ${offer}.`);
    if (r) { changed(); navigate(envPath(tenant, st.name, 'deployments', r.number)); }
  };
  const r = st && st.rollout;
  const note = st ? rolloutNote(st) : '';
  const hosts = (st && st.hosts) || [];
  return (
    <header className="pagehead envhead">
      <div className="pagehead-title">
        <h1><EnvName name={st ? st.name : env} className="" /></h1>
        {st && (
          <span className={'statepill ' + tone}>
            <ToneDot tone={tone} title={state} />
            {incidents > 0 ? `${incidents} open incident${incidents === 1 ? '' : 's'}` : st.deleting ? 'deleting' : r ? r.state : 'no deployment'}
          </span>)}
        {lastOk > 0 && <LiveBadge lastOk={lastOk} />}
        <span className="pagehead-actions">
          {offer > 0 && (
            <ConfirmButton label={`Revert to ${offer}`} confirmLabel={`Revert to ${offer}: a new deployment`} icon="undo"
              disabled={!ec.write || !!(op && op.busy)}
              title={ec.write ? `a new deployment with deployment ${offer}'s spec` : NEEDS.write} onConfirm={revert} />)}
          {editing
            ? <a className="button" href={envPath(tenant, env)} onClick={go(envPath(tenant, env))}><Icon name="check" size={14} /> Done</a>
            : (
              <a className={'button' + (incidents > 0 ? '' : ' primary')} href={builder} onClick={go(builder)}
                title={ec.write ? 'change the environment: a draft, planned, applied as the next deployment' : 'read-only: needs write on this environment'}>
                <Icon name="plus" size={14} /> New deployment</a>)}
        </span>
      </div>
      {st && (
        <p className="pagehead-sub">
          {r
            ? <>Deployment <b>{r.deployment}</b> {r.state}{r.startedAt && <>, started <Ago iso={r.startedAt} /></>}{note && <> · {note}</>}</>
            : st.deleting ? 'ferund removes it' : 'No deployment yet'}
          {' · '}
          {st.public
            ? <>public{hosts.length > 0 && <> at {hosts.map((h, i) => <Fragment key={h}>{i > 0 && ', '}<HostLink host={h} /></Fragment>)}</>}</>
            : 'not public · route door only'}
          {dh && !dh.serving && <> · default host <span className="mono">{dh.label}</span> answers 404 while a domain is served</>}
          {dh && dh.serving && hosts.length > 1 && <> · default host <HostLink host={dh.host} /></>}
        </p>)}
      {op && op.tone === 'bad' ? <ErrBox error={op.text} /> : <OpNote op={op} onDismiss={clear} />}
    </header>
  );
}

// --------------------------------------------------------------- overview --

// OverviewTab reads what the status does not say: the latest
// deployment's spec once per deployment (the images as written, when a
// job runs, the bindings), the services' traffic of the last hour once a
// minute (none while the log store does not answer), the open incidents
// while there are, and the newest deployments and incidents; below,
// what runs beside the deployments: its sandboxes and jobs (ADR 0035).
function OverviewTab({ st, tenant, ec, changed, navigate }) {
  const spec = useLatestSpec(st);
  const [traffic, setTraffic] = useState(null);
  const [incidents, setIncidents] = useState(null);
  const env = st.name;
  const readTraffic = useCallback(async () => {
    try { setTraffic(await api.traffic(env, '1h')); } catch { setTraffic(null); }
  }, [env]);
  usePoll(readTraffic, 60000);
  const open = Number(st.openIncidents) || 0;
  const readIncidents = useCallback(async () => {
    if (!open) { setIncidents([]); return; }
    try { setIncidents((await api.listIncidents(env, { open: true })).incidents || []); } catch { setIncidents([]); }
  }, [env, open]);
  usePoll(readIncidents, 10000);
  const [activity] = useActivity(env, 5);
  return (
    <>
      <EnvOverview st={st} tenant={tenant} navigate={navigate} spec={spec} traffic={traffic} ec={ec} changed={changed}
        incidents={incidents} activity={activity} />
      <EnvPods tenant={tenant} env={st.name} write={ec.write} navigate={navigate} />
    </>
  );
}

// useLatestSpec is the spec of the status's latest deployment, read once
// per deployment; null before, without one, or when it cannot be read.
export function useLatestSpec(st) {
  const [spec, setSpec] = useState(null);
  const n = st ? st.deployment : 0;
  const env = st ? st.name : '';
  useEffect(() => {
    let live = true;
    if (!n) { setSpec(null); return undefined; }
    api.getDeployment(env, n).then((d) => { if (live) setSpec(d.spec || {}); }, () => { if (live) setSpec(null); });
    return () => { live = false; };
  }, [env, n]);
  return spec;
}

// ImageShort is an image as written, short; the whole reference and its
// pin on hover.
export function ImageShort({ image }) {
  if (!image || !image.written) return <span className="muted">—</span>;
  const title = image.pinned && image.pinned !== image.written ? `${image.written}\npinned to ${image.pinned}` : image.written;
  return <span className="mono small obj-image" title={title}>{image.short}</span>;
}

// Offloading is the environment's credential offloading (ADR 0028, 0033)
// on its Secrets & credentials page: the HTTP bindings of the latest
// deployment, each with where its credential comes from, who uses it and
// Check. A binding is part of a deployment, so it is added and changed in
// New deployment; the tenant's credentials are a link away.
export function Offloading({ env, st, tenant, ec, navigate }) {
  const spec = useLatestSpec(st);
  const bindings = specBindings(spec);
  const n = (st && st.deployment) || 0;
  const enc = encodeURIComponent;
  const add = envPath(tenant, env, 'builder', 'new/binding');
  const loading = !st || (n > 0 && spec === null);
  return (
    <div className="envsect offloading">
      <div className="sect sect-row">
        credential offloading <span className="count">· {loading ? '…' : bindings.length}</span>
        <span className="spacer" />
        <Link to={`/tenants/${enc(tenant)}/credentials`} navigate={navigate} className="small">Credentials of the tenant</Link>
        {ec && ec.write && (
          <button className="mini" onClick={() => navigate(add)}><Icon name="plus" size={12} /> Add binding</button>)}
      </div>
      <p className="panel-note envsecrets-note">
        A binding sends a service's calls to an upstream through the platform, which adds the credential to each
        request: the key never enters the pod. The credential is one of the tenant's (versioned; each deployment pins
        its version) or made of this environment's secrets. A binding is part of the deployment: adding or changing one
        makes the next deployment.
      </p>
      {loading && <p className="empty">Loading…</p>}
      {!loading && bindings.length === 0 && (
        <p className="empty">No bindings{n ? ` in deployment ${n}` : ''}: no service's calls carry a credential of the platform.</p>)}
      {bindings.map((b) => <BindingCard key={b.name} b={b} env={env} ec={ec} tenant={tenant} navigate={navigate} />)}
    </div>
  );
}

// BindingSource says where a binding's credential comes from: a
// credential of the tenant at its pinned version, or the environment's
// secrets.
function BindingSource({ b, tenant, navigate }) {
  const enc = encodeURIComponent;
  const tc = b.tenantCredential;
  if (tc && tc.name) {
    return (
      <>credential{' '}
        {tenant && navigate
          ? <Link to={`/tenants/${enc(tenant)}/credentials/${enc(tc.name)}`} navigate={navigate} className="mono">{tc.name}</Link>
          : <span className="mono">{tc.name}</span>}
        {tc.version ? ` · version ${tc.version}` : ''} of the tenant</>);
  }
  if (b.secrets && b.secrets.length) {
    return <>secret{b.secrets.length === 1 ? '' : 's'} <span className="mono">{b.secrets.join(', ')}</span> of this environment</>;
  }
  return null;
}

// BindingCard is an HTTP binding of the latest deployment (ADR 0028):
// its upstream, mode, credential's kind and users, where its credential
// comes from and a link to its form; Check runs its preflight with the
// secrets' latest versions.
export function BindingCard({ b, env, ec, result = null, tenant = '', navigate = null }) {
  const [op, run] = useOp();
  const res = result || (op && op.result) || null;
  const busy = !!(op && op.busy);
  return (
    <div className="envobj envbinding">
      <div className="envobj-head">
        <span className={'sdot ' + (res ? (res.ok ? 'running' : 'failed') : 'absent')} title={res ? (res.ok ? 'ok' : 'not ok') : 'not checked'} />
        <span className="mono strong">{b.name}</span>
        <span className="mono small host">{b.upstream}</span>
        <span className="muted small">{b.mode}{b.credential ? ` · ${b.credential}` : ''}{b.users.length ? ` · used by ${b.users.join(', ')}` : ''}</span>
        <span className="spacer" />
        {ec && (
          <button className="mini" disabled={!ec.write || busy} title={ec.write ? 'resolve, dial, check TLS and the credential, from an instance' : NEEDS.write}
            onClick={() => run(() => api.checkBinding(env, b.name), '')}>
            <Icon name="check" size={12} /> {busy ? 'Checking…' : 'Check'}</button>)}
      </div>
      {(b.tenantCredential || (b.secrets && b.secrets.length > 0) || (tenant && navigate)) && (
        <div className="envbinding-src small muted">
          <BindingSource b={b} tenant={tenant} navigate={navigate} />
          {tenant && navigate && ec && ec.write && (
            <Link to={envPath(tenant, env, 'builder', `binding/${b.name}`)} navigate={navigate} className="envbinding-edit">
              Edit in New deployment</Link>)}
        </div>)}
      {op && op.tone === 'bad' && <div className="envop-note bad">{op.text}</div>}
      {res && <PreflightResult res={res} />}
    </div>
  );
}

// PreflightResult is a binding check's answer as rows.
export function PreflightResult({ res }) {
  return (
    <div className="kv-grid preflight">
      {preflightRows(res).map((r) => (
        <KV key={r.k} k={r.k} className={'mono small ' + (r.tone || '')}>
          {r.time ? <>{fmtDateTime(r.v)} <Ago iso={r.v} /></> : r.v}
        </KV>))}
    </div>
  );
}

// JobLine is a job's last run.
export function JobLine({ run }) {
  if (!run) return <div className="envobj-line muted">It has not run yet.</div>;
  return (
    <>
      <div className="envobj-line">
        <span><span className="k">deployment</span><span className="mono">{run.deployment}</span></span>
        <span><span className="k">attempt</span><span className="mono">{run.attempt}</span></span>
        <span><span className="k">phase</span><ToneLabel tone={phaseTone(run.phase)}>{run.phase}</ToneLabel></span>
        <span><span className="k">exit</span><span className={'mono' + (run.exitCode ? ' bad' : '')}>
          {run.exitCode === null || run.exitCode === undefined ? '—' : run.exitCode}</span></span>
        <span><span className="k">finished</span>{run.finishedAt ? <Ago iso={run.finishedAt} /> : <span className="muted">—</span>}</span>
      </div>
      {run.detail && <div className="envobj-detail">{run.detail}</div>}
    </>
  );
}

// InstanceTable lists an object's pods; one of an older deployment than the
// latest is muted, a stopping one dimmed. A pod's detail, when it has one,
// takes a line of its own under the pod. Its node comes last, small: a
// link for an operator (the nodes are theirs), text for everyone else.
export function InstanceTable({ instances, latest, navigate, logsTo, operator = false }) {
  const list = instances || [];
  if (list.length === 0) return <div className="envobj-line muted">No instances yet.</div>;
  return (
    <div className="tblwrap">
      <table className="tbl inst-tbl">
        <thead>
          <tr>
            <th className="col-num">replica</th>
            <th>phase</th>
            <th>state</th>
            <th>address</th>
            <th className="col-num">deployment</th>
            <th className="col-num">restarts</th>
            <th className="col-num">last exit</th>
            <th className="col-when">ready since</th>
            <th className="col-quick" />
            <th>node</th>
          </tr>
        </thead>
        <tbody>
          {list.map((i) => {
            const old = latest > 0 && i.deployment < latest;
            const dim = i.phase === 'stopping' ? 'dim' : '';
            return (
              <Fragment key={i.id}>
                <tr className={[dim, i.detail ? 'has-detail' : ''].filter(Boolean).join(' ')} title={`instance ${i.id}`}>
                  <td className="col-num mono">{i.replica}</td>
                  <td><ToneLabel tone={phaseTone(i.phase)}>{i.phase}</ToneLabel></td>
                  <td><StateLabel state={i.state} /></td>
                  <td className="mono small muted cut-start" title={i.address || undefined}>{i.address || '—'}</td>
                  <td className={'col-num mono' + (old ? ' muted' : '')} title={old ? "an older deployment's instance" : ''}>{i.deployment}</td>
                  <td className={'col-num mono' + (i.restarts > 0 ? ' warntext' : '')}>{i.restarts}</td>
                  <td className={'col-num mono' + (i.lastExit ? ' bad' : '')}>
                    {i.lastExit === null || i.lastExit === undefined ? '—' : i.lastExit}
                  </td>
                  <td className="col-when">{i.readySince ? <Ago iso={i.readySince} /> : <span className="muted">—</span>}</td>
                  <td className="col-quick">
                    <Link to={logsTo(i.replica)} navigate={navigate} className="small">logs</Link>
                  </td>
                  <td className="small muted inst-node cut-start" title={i.node || undefined}>
                    {i.node && operator
                      ? <Link to={`/nodes/${encodeURIComponent(i.node)}`} navigate={navigate} className="muted">{i.node}</Link>
                      : (i.node || '—')}
                  </td>
                </tr>
                {i.detail && (
                  <tr className={['inst-detail', dim].filter(Boolean).join(' ')}>
                    <td colSpan={10} className="small detailcell">{i.detail}</td>
                  </tr>
                )}
              </Fragment>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// DeploymentsTab polls the deployments (newest first) and the tags, and
// shows the list, or one deployment. The spec and diff of a deployment
// never change, so they are read once; its state, reason, finish and
// tags do, so the polled list's entry is laid over them.
function DeploymentsTab({ tenant, env, st, item, ec, changed, navigate }) {
  const [deployments, setDeployments] = useState(null);
  const [tags, setTags] = useState(null);
  const [err, setErr] = useState('');
  const refresh = useCallback(async () => {
    try {
      const [d, t] = await Promise.all([api.listDeployments(env), api.listTags(env)]);
      setDeployments(d.deployments || []);
      setTags(t.tags || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [env]);
  usePoll(refresh, 5000);
  const latest = st ? st.deployment : 0;
  const after = () => { refresh(); changed(); };
  const revert = (n) => api.revertDeployment(env, n, { base: latest }).then((r) => {
    after();
    navigate(envPath(tenant, env, 'deployments', r.number));
    return r;
  });

  if (item) {
    const listed = (deployments || []).find((r) => String(r.number) === item) || null;
    return (
      <DeploymentPane key={item} tenant={tenant} env={env} n={item} listed={listed} latest={latest} tags={tags} ec={ec}
        onRevert={revert} onTags={after} navigate={navigate} />
    );
  }
  return (
    <>
      <ErrBox error={err} />
      <DeploymentTable deployments={deployments} latest={latest} tenant={tenant} env={env} navigate={navigate}
        ec={ec} onRevert={revert} />
      {!deployments && !err && <p className="empty">Loading…</p>}
      {tags && <TagsBox env={env} tags={tags} ec={ec} tenant={tenant} navigate={navigate} onChange={after} />}
    </>
  );
}

// RevertButton makes a new deployment with deployment n's spec, after a
// second click.
function RevertButton({ n, ec, onRevert, label = 'Revert to this' }) {
  const [op, run] = useOp();
  return (
    <>
      <ConfirmButton className="mini" label={label} confirmLabel={`Revert to ${n}: sure?`} icon="undo"
        disabled={!ec || !ec.write || !!(op && op.busy)}
        title={ec && ec.write ? `a new deployment with deployment ${n}'s spec` : NEEDS.write}
        onConfirm={() => run(() => onRevert(n), '')} />
      {op && op.tone === 'bad' && <div className="envop-note bad">{op.text}</div>}
    </>
  );
}

// DeploymentTable is the deployments' table; with ec each but the latest
// offers its revert.
export function DeploymentTable({ deployments, latest, tenant, env, navigate, ec = null, onRevert = null }) {
  const list = deployments || [];
  return (
    <>
      <div className="tblwrap">
        <table className="tbl deployments-tbl">
          <thead>
            <tr>
              <th className="col-num">deployment</th>
              <th>state</th>
              <th>origin</th>
              <th>author</th>
              <th>message</th>
              <th>annotations</th>
              <th className="col-when">created</th>
              <th className="col-when">finished</th>
              {onRevert && <th className="col-quick" />}
            </tr>
          </thead>
          <tbody>
            {list.map((r) => {
              const to = envPath(tenant, env, 'deployments', r.number);
              return (
                <tr key={r.number} className="clickable" onClick={() => navigate(to)}>
                  <td className="col-num nowrap">
                    <Link to={to} navigate={navigate} className="mono strong">{r.number}</Link>
                  </td>
                  <td className="nowrap">
                    <ToneLabel tone={deploymentTone(r.state)}>{r.state}</ToneLabel>
                    {r.number === latest && <span className="badge acc">latest</span>}
                    {(r.tags || []).map((t) => <span key={t} className="badge tagbadge" title="tag"><Icon name="tag" size={10} /> {t}</span>)}
                  </td>
                  <td className="small nowrap">{originText(r)}</td>
                  <td className="small trunc mono" title={r.author}>{r.author || <span className="muted">—</span>}</td>
                  <td className="small msgcell">
                    {r.message || <span className="muted">—</span>}
                    {r.reason && <div className={r.state === 'failed' ? 'bad' : 'warntext'}>{r.reason}</div>}
                  </td>
                  <td><Annotations annotations={r.annotations} /></td>
                  <td className="col-when"><Ago iso={r.createdAt} /></td>
                  <td className="col-when">{r.finishedAt ? <Ago iso={r.finishedAt} /> : <span className="muted">—</span>}</td>
                  {onRevert && (
                    <td className="col-quick" onClick={(e) => e.stopPropagation()}>
                      {mayRevertTo(r.number, latest) && <RevertButton n={r.number} ec={ec} onRevert={onRevert} />}
                    </td>)}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {deployments && list.length === 0 && <p className="empty">No deployments yet.</p>}
    </>
  );
}

// TagsBox lists the environment's tags: each names a deployment and
// moves only by an explicit call, which is recorded (ADR 0018).
export function TagsBox({ env, tags, ec, tenant, navigate, onChange }) {
  const [op, run, clear] = useOp();
  const [armed, arm, disarm] = useArm();
  const [moving, setMoving] = useState('');
  const [to, setTo] = useState('');
  const busy = !!(op && op.busy);
  const can = ec && ec.write;
  return (
    <div className="envsect">
      <div className="sect">tags <span className="count">· {tags.length}</span></div>
      {tags.length === 0
        ? <p className="empty small">No tags. A deployment's page tags it.</p>
        : (
          <div className="tblwrap">
            <table className="tbl tags-tbl">
              <thead><tr><th>tag</th><th className="col-num">deployment</th><th>moved by</th><th className="col-when">moved</th><th className="col-quick" /></tr></thead>
              <tbody>
                {tags.map((t) => (
                  <tr key={t.tag}>
                    <td className="mono strong"><Icon name="tag" size={11} /> {t.tag}</td>
                    <td className="col-num mono">
                      <Link to={envPath(tenant, env, 'deployments', t.deployment)} navigate={navigate}>{t.deployment}</Link>
                    </td>
                    <td className="mono small trunc" title={t.movedBy}>{t.movedBy || <span className="muted">—</span>}</td>
                    <td className="col-when"><Ago iso={t.movedAt} /></td>
                    <td className="col-quick nowrap">
                      {moving === t.tag
                        ? (
                          <form className="inlineform" onSubmit={(e) => {
                            e.preventDefault();
                            run(() => api.putTag(env, t.tag, Number(to)), `${t.tag} moved to deployment ${to}.`)
                              .then((r) => { if (r) { setMoving(''); onChange(); } });
                          }}>
                            <input className="mono tiny" inputMode="numeric" placeholder="n" value={to} autoFocus
                              onChange={(e) => setTo(e.target.value.replace(/\D/g, ''))} />
                            <button className="mini primary" type="submit" disabled={!to || busy}>Move</button>
                            <button className="mini ghost" type="button" onClick={() => setMoving('')}>Cancel</button>
                          </form>)
                        : (
                          <button className="mini" disabled={!can || busy} title={can ? 'name another deployment' : NEEDS.write}
                            onClick={() => { setMoving(t.tag); setTo(String(t.deployment)); }}>Move</button>)}{' '}
                      <ArmButton armKey={t.tag} armed={armed} arm={arm} disarm={disarm} className="mini" disabled={!can || busy}
                        title={can ? 'delete the tag; the deployment stays' : NEEDS.write}
                        onConfirm={() => run(() => api.deleteTag(env, t.tag).then(() => true), `${t.tag} deleted.`)
                          .then((r) => { if (r) onChange(); })}>Delete</ArmButton>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>)}
      <OpNote op={op} onDismiss={clear} />
    </div>
  );
}

// TagForm puts a tag on deployment n: a new one, or one moved here.
function TagForm({ env, n, tags, ec, onChange }) {
  const [name, setName] = useState('');
  const [op, run, clear] = useOp();
  const plan = tagPlan(tags, name, n);
  const can = ec && ec.write;
  const label = plan.kind === 'move' ? `Move ${plan.tag} here from ${plan.from}` : plan.kind === 'here' ? 'Tagged already' : 'Tag';
  return (
    <>
      <form className="inlineform" title={can ? '' : NEEDS.write} onSubmit={(e) => {
        e.preventDefault();
        if (plan.kind !== 'new' && plan.kind !== 'move') return;
        run(() => api.putTag(env, plan.tag, Number(n)), `${plan.tag} names deployment ${n}.`)
          .then((r) => { if (r) { setName(''); onChange(); } });
      }}>
        <input className="mono" placeholder="tag, e.g. v1.4" value={name} disabled={!can} onChange={(e) => setName(e.target.value)} />
        <button className="mini" type="submit" disabled={!can || (plan.kind !== 'new' && plan.kind !== 'move') || !!(op && op.busy)}>
          <Icon name="tag" size={12} /> {label}</button>
      </form>
      <OpNote op={op} onDismiss={clear} />
    </>
  );
}

function DeploymentPane({ tenant, env, n, listed, latest, tags, ec, onRevert, onTags, navigate }) {
  const [rel, setRel] = useState(null);
  const [err, setErr] = useState('');
  const [armed, arm, disarm] = useArm();
  const [op, run] = useOp();
  useEffect(() => {
    let live = true;
    api.getDeployment(env, n).then((r) => { if (live) setRel(r); }, (e) => { if (live) setErr(e.message); });
    return () => { live = false; };
  }, [env, n]);
  const shown = rel && listed ? { ...rel, ...listed } : rel;
  const mine = (tags || []).filter((t) => String(t.deployment) === String(n));
  const can = ec && ec.write;
  return (
    <>
      <div className="subhead envsubhead">
        <button className="ghost mini" onClick={() => navigate(envPath(tenant, env, 'deployments'))}>
          <Icon name="arrowLeft" size={13} /> deployments</button>
        <span className="spacer" />
        {mayRevertTo(n, latest) && <RevertButton n={Number(n)} ec={ec} onRevert={onRevert} />}
      </div>
      <ErrBox error={err} />
      {!rel && !err && <p className="empty">Loading…</p>}
      {shown && <DeploymentView rel={shown} />}
      {shown && tags && (
        <div className="envsect">
          <div className="sect">tags</div>
          <div className="envtags">
            {mine.map((t) => (
              <span key={t.tag} className="badge tagbadge">
                <Icon name="tag" size={10} /> {t.tag}
                <ArmButton armKey={t.tag} armed={armed} arm={arm} disarm={disarm} className="iconbtn mini" disabled={!can}
                  title={can ? `delete ${t.tag}` : NEEDS.write} confirmLabel="×?"
                  onConfirm={() => run(() => api.deleteTag(env, t.tag).then(() => true), '').then((r) => { if (r) onTags(); })}>
                  <Icon name="x" size={10} /></ArmButton>
              </span>))}
            {mine.length === 0 && <span className="muted small">none</span>}
          </div>
          {op && op.tone === 'bad' && <ErrBox error={op.text} />}
          <TagForm env={env} n={n} tags={tags} ec={ec} onChange={onTags} />
        </div>)}
    </>
  );
}

// DeploymentView is a deployment (GET …/deployments/{n}): what made it, its diff
// against the deployment before, and its normalised spec.
export function DeploymentView({ rel }) {
  const spec = prettyJson(rel.spec || {});
  return (
    <div className="deployment">
      <div className="deployment-head">
        <span className="mono strong">deployment {rel.number}</span>
        <ToneLabel tone={deploymentTone(rel.state)}>{rel.state}</ToneLabel>
        {(rel.tags || []).map((t) => <span key={t} className="badge" title="tag">{t}</span>)}
      </div>
      {rel.reason && <div className={rel.state === 'failed' ? 'error' : 'notice'}>{rel.reason}</div>}
      <div className="kv-grid">
        <KV k="origin">{originText(rel)}</KV>
        <KV k="base">{rel.base ? <>deployment <span className="mono">{rel.base}</span></> : 'none'}</KV>
        <KV k="author"><span className="mono small">{rel.author || '—'}</span></KV>
        <KV k="message">{rel.message || <span className="muted">—</span>}</KV>
        <KV k="created">{fmtDateTime(rel.createdAt)} <Ago iso={rel.createdAt} /></KV>
        <KV k="started">{rel.startedAt ? <>{fmtDateTime(rel.startedAt)} <Ago iso={rel.startedAt} /></> : <span className="muted">—</span>}</KV>
        <KV k="finished">{rel.finishedAt ? <>{fmtDateTime(rel.finishedAt)} <Ago iso={rel.finishedAt} /></> : <span className="muted">—</span>}</KV>
        <KV k="annotations">
          {labelPairs(rel.annotations).length === 0
            ? <span className="muted">—</span>
            : (
              <span className="annlist">
                {labelPairs(rel.annotations).map(([k, v]) => (
                  <span key={k} className="mono small"><span className="muted">{k}</span> {v}</span>))}
              </span>)}
        </KV>
      </div>
      <div className="sect">{rel.number > 1 ? `changes against deployment ${rel.number - 1}` : 'changes (the first deployment)'}</div>
      <DiffView diff={rel.diff} />
      <details className="specbox">
        <summary>The spec, normalised: defaults filled, images pinned to digests, secret versions pinned</summary>
        <div className="snippet"><pre className="specpre">{spec}</pre><CopyBtn text={spec} title="copy the spec" />
          <a className="button mini" download={`deployment-${rel.number}.json`} title="export the spec as a file"
            href={`data:application/json;charset=utf-8,${encodeURIComponent(spec)}`}>Export</a></div>
      </details>
    </div>
  );
}

// DiffView lists a diff's entries: the change, the object, the effect on
// the running pods, and the changed fields from → to.
export function DiffView({ diff, empty = 'No changes.' }) {
  const list = diff || [];
  if (list.length === 0) return <p className="empty small">{empty}</p>;
  return (
    <div className="diff">
      {list.map((d) => (
        <div key={`${d.kind}/${d.name}`} className="diff-entry">
          <div className="diff-head">
            <span className={'badge ' + changeTone(d.change)}>{d.change}</span>
            <span className="mono">{d.kind}/{d.name}</span>
            {d.effect && <span className={'small ' + (heavyEffect(d.effect) ? 'bad' : 'muted')}>{d.effect}</span>}
          </div>
          {(d.fields || []).length > 0 && (
            <div className="tblwrap">
              <table className="tbl diff-tbl">
                <tbody>
                  {d.fields.map((f) => (
                    <tr key={f.path}>
                      <td className="mono small diff-path">{f.path}</td>
                      <td className="mono small diff-from">{fmtValue(f.from)}</td>
                      <td className="muted diff-arrow">→</td>
                      <td className="mono small diff-to">{fmtValue(f.to)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>)}
        </div>
      ))}
    </div>
  );
}

// -------------------------------------------------------------- incidents --

function IncidentsList({ tenant, env, navigate }) {
  const [openOnly, setOpenOnly] = useState(false);
  const [incidents, setIncidents] = useState(null);
  const [err, setErr] = useState('');
  const [request] = useState(() => newest());
  const refresh = useCallback(async () => {
    const current = request();
    try {
      const res = await api.listIncidents(env, { open: openOnly });
      if (!current()) return;
      setIncidents(res.incidents || []);
      setErr('');
    } catch (e) {
      if (current()) setErr(e.message);
    }
  }, [env, openOnly, request]);
  usePoll(refresh, 5000);
  return (
    <>
      <div className="filterbar slim">
        <button className={'chipbtn' + (!openOnly ? ' on' : '')} onClick={() => setOpenOnly(false)}>all</button>
        <button className={'chipbtn' + (openOnly ? ' on' : '')} onClick={() => setOpenOnly(true)}>open</button>
      </div>
      <ErrBox error={err} />
      <IncidentTable incidents={incidents} openOnly={openOnly} tenant={tenant} env={env} navigate={navigate} />
      {!incidents && !err && <p className="empty">Loading…</p>}
    </>
  );
}

// IncidentTable is the incidents' table, newest first.
export function IncidentTable({ incidents, openOnly, tenant, env, navigate }) {
  const list = incidents || [];
  return (
    <>
      <div className="tblwrap">
        <table className="tbl incidents-tbl">
          <thead>
            <tr>
              <th className="col-state" />
              <th className="col-num">#</th>
              <th>condition</th>
              <th>object</th>
              <th className="col-num">deployment</th>
              <th>summary</th>
              <th className="col-when">opened</th>
              <th className="col-when">closed</th>
            </tr>
          </thead>
          <tbody>
            {list.map((i) => {
              const to = envPath(tenant, env, 'incidents', i.id);
              const open = !i.closedAt;
              return (
                <tr key={i.id} className={'clickable' + (open ? '' : ' dim')} onClick={() => navigate(to)}>
                  <td className="col-state"><ToneDot tone={open ? 'failed' : 'absent'} title={open ? 'open' : 'closed'} /></td>
                  <td className="col-num"><Link to={to} navigate={navigate} className="mono">{i.id}</Link></td>
                  <td className="nowrap">{conditionText(i.condition)}</td>
                  <td className="mono small nowrap">
                    {incidentObject(i.object).hasLogs
                      ? <Link to={envPath(tenant, env, 'objects', i.object)} navigate={navigate}>{i.object}</Link>
                      : i.object}
                  </td>
                  <td className="col-num mono">{i.deployment}</td>
                  <td className="small msgcell">{i.summary}</td>
                  <td className="col-when"><Ago iso={i.openedAt} /></td>
                  <td className="col-when">{i.closedAt ? <Ago iso={i.closedAt} /> : <span className="bad small">open</span>}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {incidents && list.length === 0 && <p className="empty">{openOnly ? 'No open incidents.' : 'No incidents.'}</p>}
    </>
  );
}

function IncidentPane({ tenant, env, id, navigate }) {
  const [inc, setInc] = useState(null);
  const [err, setErr] = useState('');
  const refresh = useCallback(async () => {
    try {
      setInc(await api.getIncident(env, id));
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [env, id]);
  usePoll(refresh, 5000);
  return (
    <>
      <div className="subhead">
        <button className="ghost mini" onClick={() => navigate(envPath(tenant, env, 'incidents'))}>
          <Icon name="arrowLeft" size={13} /> incidents</button>
      </div>
      <ErrBox error={err} />
      {!inc && !err && <p className="empty">Loading…</p>}
      {inc && <IncidentView inc={inc} tenant={tenant} env={env} navigate={navigate} />}
    </>
  );
}

// IncidentView is an incident with what the controller saw: the exits,
// the end of the log, what a deployment past its deadline still waited for,
// a failed rollout's diff against the deployed deployment, why an instance
// found no place.
export function IncidentView({ inc, tenant, env, navigate }) {
  const open = !inc.closedAt;
  const obj = incidentObject(inc.object);
  const ev = inc.evidence || {};
  return (
    <div className="incident">
      <div className="deployment-head">
        <span className="mono strong">incident {inc.id}</span>
        <ToneLabel tone={open ? 'failed' : 'absent'}>{open ? 'open' : 'closed'}</ToneLabel>
        <span className="badge">{conditionText(inc.condition)}</span>
      </div>
      <p className="incident-summary">{inc.summary}</p>
      <div className="kv-grid">
        <KV k="object">
          {obj.hasLogs
            ? <Link to={envPath(tenant, env, 'objects', inc.object)} navigate={navigate} className="mono">{inc.object}</Link>
            : <span className="mono">{inc.object}</span>}
          {obj.hasLogs && <> · <Link to={envPath(tenant, env, 'logs', inc.object)} navigate={navigate}>its logs</Link></>}
        </KV>
        <KV k="deployment">
          <Link to={envPath(tenant, env, 'deployments', inc.deployment)} navigate={navigate} className="mono">{inc.deployment}</Link>
        </KV>
        <KV k="opened">{fmtDateTime(inc.openedAt)} <Ago iso={inc.openedAt} /></KV>
        <KV k="closed">
          {inc.closedAt ? <>{fmtDateTime(inc.closedAt)} <Ago iso={inc.closedAt} /></> : <span className="bad">still open</span>}
        </KV>
      </div>
      {!hasEvidence(ev) && <p className="empty small">No evidence recorded.</p>}
      {(ev.exits || []).length > 0 && (
        <>
          <div className="sect">exits</div>
          <div className="tblwrap">
            <table className="tbl exits-tbl">
              <thead><tr><th>when</th><th className="col-num">code</th></tr></thead>
              <tbody>
                {ev.exits.map((x, k) => (
                  <tr key={k}>
                    <td className="mono small">{fmtDateTime(x.at)} <Ago iso={x.at} /></td>
                    <td className={'col-num mono ' + (x.code === 0 ? 'ok' : 'bad')}>{exitText(x.code)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>)}
      {(ev.logTail || []).length > 0 && (
        <>
          <div className="sect">the end of the log</div>
          <pre className="logpre">{ev.logTail.join('\n')}</pre>
        </>)}
      {(ev.pending || []).length > 0 && (
        <>
          <div className="sect">what the deployment waited for</div>
          <pre className="logpre">{ev.pending.join('\n')}</pre>
        </>)}
      {(ev.diff || []).length > 0 && (
        <>
          <div className="sect">the diff against the deployed deployment</div>
          <DiffView diff={ev.diff} />
        </>)}
      {ev.placement && (
        <>
          <div className="sect">placement</div>
          <pre className="logpre">{ev.placement}</pre>
        </>)}
    </div>
  );
}

// ------------------------------------------------------------------- logs --

export function LogsTab({ tenant, env, st, item, replica, search, navigate }) {
  if (!st) return <p className="empty">Loading…</p>;
  // On an object's page (item) the object is the page's; in the
  // environment's Logs the one picked, from the query.
  const focus = focusOf(item, replica, search);
  const obj = focus.object || '';
  const o = findObject(st.objects || [], obj);
  const page = o ? envPath(tenant, env, 'objects', obj) : '';
  const tail = obj
    ? <LogsPane key={`${obj}/${focus.replica}`} env={env} item={obj} replica={focus.replica || 0} o={o} page={page} navigate={navigate} />
    : null;
  return (
    <LogSearch tenant={tenant} env={env} st={st} focus={focus} navigate={navigate} tail={tail} tailOnly={!!(o && isAttached(o))}
      scoped={!!item} />
  );
}

function LogsPane({ env, item, replica, o, page, navigate }) {
  const [result, setResult] = useState(null);
  const i = item.indexOf('/');
  const kind = item.slice(0, i);
  const name = item.slice(i + 1);
  const refresh = useCallback(async () => {
    try {
      const r = await api.objectLogs(env, kind, name, { replica });
      setResult({ lines: r.lines || [], source: r.source || '' });
    } catch (e) {
      setResult({ problem: logsProblem(e) });
    }
  }, [env, kind, name, replica]);
  const lastOk = usePoll(refresh, 5000);

  // The log keeps its end in view while one reads at the end.
  const box = useRef(null);
  const atEnd = useRef(true);
  useEffect(() => {
    const el = box.current;
    if (el && atEnd.current) el.scrollTop = el.scrollHeight;
  }, [result]);
  const onScroll = (e) => {
    const el = e.currentTarget;
    atEnd.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
  };

  return (
    <div className="logs">
      <div className="pane-toolbar">
        <h2><Icon name="file" size={14} />{' '}
          {page ? <Link to={page} navigate={navigate} className="mono" title="the object's page">{item}</Link> : <span className="mono">{item}</span>}
          {replica > 0 && <span className="muted"> · replica {replica}</span>}</h2>
        {result && result.source && (
          <span className="muted small">
            {logSourceText(result.source, o, replica)} · the last {result.lines.length} lines
          </span>)}
        <span className="spacer" />
        <LiveBadge lastOk={lastOk} />
      </div>
      <LogsBody result={result} boxRef={box} onScroll={onScroll} />
    </div>
  );
}

// LogsBody shows an object's log, or why there is none: the texts of a
// node without container logs (501) and of a paused instance (409) as
// messages, anything else as an error.
export function LogsBody({ result, boxRef, onScroll }) {
  if (!result) return <p className="empty">Loading…</p>;
  if (result.problem) {
    const p = result.problem;
    if (p.tone !== 'notice') return <ErrBox error={p.text} />;
    return (
      <div className="message">
        <Icon name="alert" size={14} />
        <span><strong>{p.text}</strong>{p.hint && <span className="muted"> — {p.hint}</span>}</span>
      </div>
    );
  }
  if (result.lines.length === 0) return <p className="empty">No lines yet.</p>;
  return <pre className="logpre" ref={boxRef} onScroll={onScroll}>{result.lines.join('\n')}</pre>;
}
