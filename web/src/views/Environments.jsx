// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { LOG_LINES, api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, CopyBtn, ErrBox, KV, LiveBadge, StateLabel, useNow, usePoll } from '../components.jsx';
import { fmtDate, fmtDateTime, prettyJson, shortId } from '../format.js';
import { newest } from '../poll.js';
import { envPath } from '../routes.js';
import {
  KIND_TITLES, changeTone, conditionText, envCounts, exitText, findObject, fmtValue, groupObjects, hasEvidence,
  heavyEffect, incidentObject, isAttached, labelPairs, logReplicas, logSourceText, logsProblem, objectSummary,
  objectTone, originText, parseLabelFilter, phaseTone, deploymentTone, rolloutNote, statusState,
} from './envs.js';

// A tenant's environments, read-only: the list with a label filter, and
// an environment with its tabs: the overview (the rollout, the objects
// of its target deployment with their instances, the routes), its deployments
// (a deployment's diff and normalised spec), its incidents (with their
// evidence) and the logs of an object. Environments are made and changed
// through ferund's API (FeCode applies the deployments); this view applies,
// reverts and deletes nothing, and reads no secret and no route token.
// The environment, the tab and its item are the route's:
// /tenants/:t/environments/:env/:tab/:item.

export function Environments({ tenant, env, tab, item, replica, navigate }) {
  if (env) {
    return (
      <EnvDetail key={env} tenant={tenant} env={env} tab={tab} item={item} replica={replica} navigate={navigate} />
    );
  }
  return <EnvList tenant={tenant} navigate={navigate} />;
}

// ---------------------------------------------------------------- widgets --

// Link is a link within the UI: a real href (so that a new tab works),
// the router on a plain click.
function Link({ to, navigate, className, title, children }) {
  return (
    <a href={to} className={className} title={title} onClick={(e) => {
      if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      e.stopPropagation();
      navigate(to);
    }}>{children}</a>
  );
}

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
    <div className="single">
      <section className="pane">
        <div className="pane-toolbar">
          <h2>
            <Icon name="blocks" size={15} /> Environments
            <span className="count">{envs ? envs.length : '…'}</span>
          </h2>
          <span className="mini-stats muted">
            <span className="ok" title="deployed">● {counts.deployed}</span>
            <span className="warn" title="waiting or deploying">● {counts.deploying}</span>
            {counts.failed > 0 && <span className="bad" title="the latest deployment failed">● {counts.failed}</span>}
            {counts.incidents > 0 && <span className="bad" title="open incidents">▲ {counts.incidents}</span>}
          </span>
          <span className="spacer" />
          <LiveBadge lastOk={lastOk} />
        </div>
        <ErrBox error={err} />

        <form className="filterbar slim" onSubmit={(e) => {
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
        {bad.length > 0 && <p className="notice small">A label filter is key=value: {bad.join(', ')}</p>}

        <EnvTable envs={envs} filtered={labels.length > 0} tenant={tenant} navigate={navigate}
          onLabel={(k, v) => addLabels([`${k}=${v}`])} />
        {!envs && !err && <p className="empty">Loading…</p>}
        <p className="footnote muted small">
          Read-only: environments are made and changed through ferund's API, by FeCode or another client;
          this view applies, reverts and deletes nothing, and reads no secret and no route token.
        </p>
      </section>
    </div>
  );
}

// EnvTable is the environments' table (GET /api/v1/environments); a
// label chip filters by its label.
export function EnvTable({ envs, filtered, tenant, navigate, onLabel }) {
  const list = envs || [];
  return (
    <>
      <div className="tblwrap">
        <table className="tbl envs-tbl">
          <thead>
            <tr>
              <th className="col-state">state</th>
              <th>name</th>
              <th>labels</th>
              <th className="col-num">deployment</th>
              <th className="col-num">deployed</th>
              <th>state</th>
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
                  <td><Labels labels={e.labels} onLabel={onLabel} /></td>
                  <td className="col-num mono">{e.deployment || '—'}</td>
                  <td className={'col-num mono' + (e.deployed !== e.deployment ? ' warntext' : '')}
                    title={e.deployed !== e.deployment ? 'the latest deployment is not deployed' : ''}>
                    {e.deployed || '—'}
                  </td>
                  <td><ToneLabel tone={tone}>{e.state}</ToneLabel></td>
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

const TABS = [
  { key: 'overview', label: 'Overview', icon: 'gauge' },
  { key: 'deployments', label: 'Deployments', icon: 'history' },
  { key: 'incidents', label: 'Incidents', icon: 'alert' },
  { key: 'logs', label: 'Logs', icon: 'file' },
];

function EnvDetail({ tenant, env, tab, item, replica, navigate }) {
  const [st, setSt] = useState(null);
  const [err, setErr] = useState('');
  const [gone, setGone] = useState(false);
  const list = envPath(tenant);
  const activeTab = TABS.some((t) => t.key === tab) ? tab : 'overview';

  const refresh = useCallback(async () => {
    try {
      setSt(await api.getEnvironment(env));
      setErr('');
      setGone(false);
    } catch (e) {
      setGone(e.status === 404);
      setErr(e.message);
    }
  }, [env]);
  const lastOk = usePoll(refresh, 3000);
  useNow(1000);

  // Escape goes up: from an item to its tab's list, from there to the
  // environments.
  const up = item ? envPath(tenant, env, activeTab) : list;
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
  const state = statusState(st);

  return (
    <section className="pane pane-detail">
      <div className="detail-head">
        <button className="iconbtn" title="back to the environments (Esc)" onClick={() => navigate(list)}>
          <Icon name="arrowLeft" size={15} /></button>
        <ToneDot tone={deploymentTone(state)} title={state} />
        <div className="detail-title">
          <span className="mono uuid">
            {st ? st.name : env}
            {st && st.deleting && <span className="badge warn">deleting</span>}
          </span>
          <span className="muted small">
            {st
              ? <>deployment {st.deployment || '—'} · deployed {st.deployed || '—'} · <span className="mono">{st.id}</span></>
              : 'loading…'}
          </span>
        </div>
        {st && <CopyBtn text={st.id} title="copy the environment's ID" />}
        <LiveBadge lastOk={lastOk} />
      </div>

      <div className="tabbar">
        {TABS.map((t) => (
          <button key={t.key} className={'tab' + (activeTab === t.key ? ' on' : '')}
            onClick={() => navigate(envPath(tenant, env, t.key))}>
            <Icon name={t.icon} size={14} /> {t.label}
            {t.key === 'incidents' && st && st.openIncidents > 0 && (
              <span className="tabcount" title="open incidents">{st.openIncidents}</span>)}
          </button>
        ))}
      </div>
      <ErrBox error={err} />

      {activeTab === 'overview' && (
        <div className="tabbody">
          {st ? <EnvOverview st={st} tenant={tenant} navigate={navigate} /> : !err && <p className="empty">Loading…</p>}
        </div>
      )}
      {activeTab === 'deployments' && (
        <div className="tabbody">
          <DeploymentsTab tenant={tenant} env={env} st={st} item={item} navigate={navigate} />
        </div>
      )}
      {activeTab === 'incidents' && (
        <div className="tabbody">
          {item
            ? <IncidentPane key={item} tenant={tenant} env={env} id={item} navigate={navigate} />
            : <IncidentsList tenant={tenant} env={env} navigate={navigate} />}
        </div>
      )}
      {activeTab === 'logs' && (
        <div className="tabbody">
          <LogsTab tenant={tenant} env={env} st={st} item={item} replica={replica} navigate={navigate} />
        </div>
      )}
    </section>
  );
}

// --------------------------------------------------------------- overview --

// EnvOverview is an environment's status (GET /api/v1/environments/{env}):
// the rollout, the objects of the target deployment by kind with their
// instances, and the routes.
export function EnvOverview({ st, tenant, navigate }) {
  const groups = groupObjects(st.objects);
  const logsTo = (o, replica = 0) => envPath(tenant, st.name, 'logs', `${o.kind}/${o.name}`, replica);
  return (
    <>
      <RolloutBox st={st} tenant={tenant} navigate={navigate} />
      <div className="kv-grid">
        <KV k="environment ID"><span className="mono">{st.id}</span> <CopyBtn text={st.id} mini /></KV>
        <KV k="labels"><Labels labels={st.labels} /></KV>
        <KV k="deployments">
          latest <span className="mono">{st.deployment || '—'}</span>, deployed <span className="mono">{st.deployed || '—'}</span>
        </KV>
        <KV k="open incidents">
          {st.openIncidents > 0
            ? (
              <Link to={envPath(tenant, st.name, 'incidents')} navigate={navigate} className="bad">
                {st.openIncidents} open
              </Link>)
            : <span className="muted">none</span>}
        </KV>
        <KV k="created">{fmtDate(st.createdAt)} <Ago iso={st.createdAt} /></KV>
      </div>

      {groups.map(({ kind, objects }) => (
        <div key={kind} className="envsect">
          <div className="sect">{KIND_TITLES[kind] || kind} <span className="count">· {objects.length}</span></div>
          {objects.map((o) => (
            <ObjectCard key={`${o.kind}/${o.name}`} o={o} latest={st.deployment} tenant={tenant} navigate={navigate}
              logsTo={(replica) => logsTo(o, replica)} />
          ))}
        </div>
      ))}
      {groups.length === 0 && <p className="empty">No objects: the environment has no deployment yet.</p>}

      <RoutesTable routes={st.routes} env={st.name} />
    </>
  );
}

// RolloutBox is the latest deployment's rollout, with its reason.
export function RolloutBox({ st, tenant, navigate }) {
  const r = st.rollout;
  if (st.deleting) {
    return <div className="rollout paused"><div className="rollout-head"><ToneLabel tone="paused">deleting</ToneLabel>
      <span className="muted small">ferund removes its instances, volumes and groups, then the environment.</span></div></div>;
  }
  if (!r) {
    return <div className="rollout absent"><div className="rollout-head"><ToneLabel tone="absent">none</ToneLabel>
      <span className="muted small">No deployment yet: nothing runs.</span></div></div>;
  }
  const tone = deploymentTone(r.state);
  const note = rolloutNote(st);
  return (
    <div className={'rollout ' + tone}>
      <div className="rollout-head">
        <ToneLabel tone={tone}>{r.state}</ToneLabel>
        <span>
          deployment{' '}
          <Link to={envPath(tenant, st.name, 'deployments', r.deployment)} navigate={navigate} className="mono">{r.deployment}</Link>
        </span>
        {r.startedAt && <span className="muted small">started {fmtDate(r.startedAt)} <Ago iso={r.startedAt} /></span>}
        {note && <span className="muted small">· {note}</span>}
      </div>
      {r.reason && <div className="rollout-reason">{r.reason}</div>}
    </div>
  );
}

// ObjectCard is one object: an image service or an addon with its
// instances (each links its logs), an attached service with its process,
// a job with its last run.
export function ObjectCard({ o, latest, tenant, navigate, logsTo }) {
  const tone = objectTone(o);
  const withInstances = o.kind === 'addon' || (o.kind === 'service' && !isAttached(o));
  return (
    <div className="envobj">
      <div className="envobj-head">
        <ToneDot tone={tone} title={o.phase || ''} />
        <span className="mono strong">{o.name}</span>
        <span className="muted small">{objectSummary(o)}</span>
        <span className="spacer" />
        {!withInstances && (
          <Link to={logsTo(0)} navigate={navigate} className="small envobj-logs">
            <Icon name="file" size={12} /> logs
          </Link>)}
      </div>
      {isAttached(o) && <AttachedLine o={o} tenant={tenant} navigate={navigate} />}
      {o.kind === 'job' && <JobLine run={o.lastRun} />}
      {withInstances && <InstanceTable instances={o.instances} latest={latest} navigate={navigate} logsTo={logsTo} />}
    </div>
  );
}

function AttachedLine({ o, tenant, navigate }) {
  const sb = `/tenants/${encodeURIComponent(tenant)}/sandboxes/${o.sandbox}`;
  return (
    <>
      <div className="envobj-line">
        <span><span className="k">sandbox</span><Link to={sb} navigate={navigate} className="mono">{o.sandbox}</Link></span>
        <span><span className="k">phase</span><ToneLabel tone={phaseTone(o.phase)}>{o.phase}</ToneLabel></span>
        <span><span className="k">restarts</span><span className={'mono' + (o.restarts > 0 ? ' warntext' : '')}>{o.restarts || 0}</span></span>
        <span><span className="k">last exit</span><span className="mono">{o.lastExit === null || o.lastExit === undefined ? '—' : o.lastExit}</span></span>
      </div>
      {o.detail && <div className="envobj-detail">{o.detail}</div>}
    </>
  );
}

function JobLine({ run }) {
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
// latest is muted, a stopping one dimmed.
export function InstanceTable({ instances, latest, navigate, logsTo }) {
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
            <th>node</th>
            <th>address</th>
            <th className="col-num">deployment</th>
            <th className="col-num">restarts</th>
            <th className="col-num">last exit</th>
            <th className="col-when">ready since</th>
            <th>detail</th>
            <th className="col-quick" />
          </tr>
        </thead>
        <tbody>
          {list.map((i) => {
            const old = latest > 0 && i.deployment < latest;
            return (
              <tr key={i.id} className={i.phase === 'stopping' ? 'dim' : ''} title={`instance ${i.id}`}>
                <td className="col-num mono">{i.replica}</td>
                <td><ToneLabel tone={phaseTone(i.phase)}>{i.phase}</ToneLabel></td>
                <td><StateLabel state={i.state} /></td>
                <td className="mono">
                  {i.node
                    ? <Link to={`/nodes/${encodeURIComponent(i.node)}`} navigate={navigate}>{i.node}</Link>
                    : <span className="muted">—</span>}
                </td>
                <td className="mono small">{i.address || <span className="muted">—</span>}</td>
                <td className={'col-num mono' + (old ? ' muted' : '')} title={old ? "an older deployment's instance" : ''}>{i.deployment}</td>
                <td className={'col-num mono' + (i.restarts > 0 ? ' warntext' : '')}>{i.restarts}</td>
                <td className={'col-num mono' + (i.lastExit ? ' bad' : '')}>
                  {i.lastExit === null || i.lastExit === undefined ? '—' : i.lastExit}
                </td>
                <td className="col-when">{i.readySince ? <Ago iso={i.readySince} /> : <span className="muted">—</span>}</td>
                <td className="small detailcell">{i.detail}</td>
                <td className="col-quick">
                  <Link to={logsTo(i.replica)} navigate={navigate} className="small">logs</Link>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// RoutesTable lists the routes of the deployed deployment (the latest when
// none is deployed), their ports resolved.
export function RoutesTable({ routes, env }) {
  const list = routes || [];
  return (
    <div className="envsect">
      <div className="sect">routes <span className="count">· {list.length}</span></div>
      {list.length === 0
        ? <p className="empty small">No routes.</p>
        : (
          <div className="tblwrap">
            <table className="tbl routes-tbl">
              <thead><tr><th>name</th><th>path</th><th>service</th><th className="col-num">port</th></tr></thead>
              <tbody>
                {list.map((r) => (
                  <tr key={r.name}>
                    <td className="mono">{r.name}</td>
                    <td className="mono">{r.path}</td>
                    <td className="mono">{r.service}</td>
                    <td className="col-num mono">{r.port}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>)}
      <p className="panel-note">
        Served at <code>/api/v1/environments/{env}/http/…</code> to a route token of the environment; this view mints none.
      </p>
    </div>
  );
}

// --------------------------------------------------------------- deployments --

// DeploymentsTab polls the deployments (newest first) and shows the list, or
// one deployment. The spec and diff of a deployment never change, so they are
// read once; its state, reason, finish and tags do, so the polled list's
// entry is laid over them.
function DeploymentsTab({ tenant, env, st, item, navigate }) {
  const [deployments, setDeployments] = useState(null);
  const [err, setErr] = useState('');
  const refresh = useCallback(async () => {
    try {
      setDeployments((await api.listDeployments(env)).deployments || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [env]);
  usePoll(refresh, 5000);

  if (item) {
    const listed = (deployments || []).find((r) => String(r.number) === item) || null;
    return <DeploymentPane key={item} tenant={tenant} env={env} n={item} listed={listed} navigate={navigate} />;
  }
  return (
    <>
      <ErrBox error={err} />
      <DeploymentTable deployments={deployments} latest={st ? st.deployment : 0} tenant={tenant} env={env} navigate={navigate} />
      {!deployments && !err && <p className="empty">Loading…</p>}
    </>
  );
}

// DeploymentTable is the deployments' table.
export function DeploymentTable({ deployments, latest, tenant, env, navigate }) {
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
                    {(r.tags || []).map((t) => <span key={t} className="badge" title="tag">{t}</span>)}
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

function DeploymentPane({ tenant, env, n, listed, navigate }) {
  const [rel, setRel] = useState(null);
  const [err, setErr] = useState('');
  useEffect(() => {
    let live = true;
    api.getDeployment(env, n).then((r) => { if (live) setRel(r); }, (e) => { if (live) setErr(e.message); });
    return () => { live = false; };
  }, [env, n]);
  const shown = rel && listed ? { ...rel, ...listed } : rel;
  return (
    <>
      <div className="subhead">
        <button className="ghost mini" onClick={() => navigate(envPath(tenant, env, 'deployments'))}>
          <Icon name="arrowLeft" size={13} /> deployments</button>
      </div>
      <ErrBox error={err} />
      {!rel && !err && <p className="empty">Loading…</p>}
      {shown && <DeploymentView rel={shown} />}
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
        <div className="snippet"><pre className="specpre">{spec}</pre><CopyBtn text={spec} title="copy the spec" /></div>
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
                  <td className="mono small nowrap">{i.object}</td>
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
          <span className="mono">{inc.object}</span>
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

function LogsTab({ tenant, env, st, item, replica, navigate }) {
  const objects = st ? st.objects || [] : [];
  const o = findObject(objects, item);
  return (
    <>
      <LogPicker objects={objects} item={item} replica={replica} o={o}
        onPick={(it, rep) => navigate(envPath(tenant, env, 'logs', it, rep))} />
      {item
        ? <LogsPane key={`${item}/${replica}`} env={env} item={item} replica={replica} o={o} />
        : <p className="empty">Pick an object to read the last {LOG_LINES} lines of its log.</p>}
    </>
  );
}

// LogPicker offers the objects of the status, and an image service's
// replicas.
export function LogPicker({ objects, item, replica, o, onPick }) {
  const replicas = o ? logReplicas(o) : [];
  return (
    <>
      <div className="chiprow">
        <span className="k">object</span>
        {(objects || []).map((x) => {
          const it = `${x.kind}/${x.name}`;
          return (
            <button key={it} className={'chipbtn mono objchip' + (it === item ? ' on' : '')} onClick={() => onPick(it, 0)}>
              <span className="muted">{x.kind}/</span>{x.name}
            </button>
          );
        })}
        {(objects || []).length === 0 && <span className="muted small">—</span>}
      </div>
      {replicas.length > 1 && (
        <div className="chiprow">
          <span className="k">replica</span>
          {replicas.map((r) => (
            <button key={r} className={'chipbtn mono' + (r === replica ? ' on' : '')} onClick={() => onPick(item, r)}>{r}</button>
          ))}
        </div>)}
    </>
  );
}

function LogsPane({ env, item, replica, o }) {
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
        <h2><Icon name="file" size={14} /> <span className="mono">{item}</span>{replica > 0 && <span className="muted"> · replica {replica}</span>}</h2>
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
