// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, CopyBtn, ErrBox, KV, LiveBadge, PageHead, usePoll } from '../components.jsx';
import { shortId } from '../format.js';
import { envPath } from '../routes.js';
import { traceQueryOf } from '../traces.js';
import {
  CHANGES_READ, KIND_NAMES, changeTone, changesTo, conditionText, definitionRows, envRows, fmtValue, heavyEffect,
  instanceDeployments, isAttached, jobTrigger, objectImage, objectIncidents, objectStateText, objectTone, phaseTone,
  deploymentTone, restartsOf, routesTo, specObject, splitItem, usedBy,
} from './envs.js';
import { builtinCatalog, planText, readCatalog } from './catalog.js';
import { EnvLink, NEEDS, OpNote, useOp } from './EnvTabs.jsx';
import { ImageShort, InstanceTable, JobLine, ToneDot, ToneLabel, useLatestSpec } from './Environments.jsx';
import '../styles/environments.css';

// An object's page (/tenants/:t/environments/:env/objects/:kind/:name):
// a service, an addon or a job of the environment's latest deployment.
// Its head says what it is and how it is, with Logs, Traces, Restart or
// Run now, and Edit; then its definition as the latest deployment's
// normalised spec has it (the image as written and its pin, the
// environment with its references as links, a secret at its pinned
// version, never its value), the bindings it names, the routes to it,
// who uses it, its instances, its incidents, and the deployments of the
// last CHANGES_READ whose diff changed it. st is the environment's
// status, polled by the environment's page.

const Link = EnvLink;

export function ObjectPage({ tenant, st, item, ec, lastOk = 0, changed = () => {}, navigate }) {
  const spec = useLatestSpec(st);
  const id = splitItem(item);
  if (!id) return <p className="error">No object {item}.</p>;
  return <ObjectView tenant={tenant} st={st} kind={id.kind} name={id.name} spec={spec} ec={ec} lastOk={lastOk}
    changed={changed} navigate={navigate} />;
}

// ObjectView is the page of kind/name, the status and spec given (spec
// null while it loads); the incidents and changes it reads itself.
export function ObjectView({ tenant, st, kind, name, spec, ec, lastOk = 0, changed = () => {}, navigate }) {
  const env = st.name;
  const o = (st.objects || []).find((x) => x.kind === kind && x.name === name) || null;
  const so = specObject(spec, kind, name);
  const [op, run, clear] = useOp();
  const busy = !!(op && op.busy);
  const go = (to) => (e) => { if (e.button === 0 && !e.metaKey && !e.ctrlKey) { e.preventDefault(); navigate(to); } };
  const logs = envPath(tenant, env, 'logs', `${kind}/${name}`);
  const traces = envPath(tenant, env, 'traces', '', null, traceQueryOf({ service: name }));
  const builder = envPath(tenant, env, 'builder', `${kind}/${name}`);
  const manual = kind === 'job' && jobTrigger(spec, name) === 'manual';
  const tone = o ? objectTone(o) : 'absent';
  const image = objectImage(o || { kind, name }, spec);

  const actions = (
    <>
      <a className="button primary" href={logs} onClick={go(logs)}><Icon name="file" size={14} /> Logs</a>
      {kind === 'service' && (
        <a className="button" href={traces} onClick={go(traces)} title="the recent traces through this service">
          <Icon name="activity" size={14} /> Traces</a>)}
      {kind === 'service' && o && (
        <button disabled={!ec || !ec.write || busy}
          title={ec && ec.write ? (isAttached(o) ? 'restart its process, never the sandbox' : 'replace its replicas one at a time') : NEEDS.write}
          onClick={() => run(() => api.restartService(env, name).then(() => true),
            isAttached(o) ? `${name}: its process restarts.` : `${name}: its replicas restart one at a time, each once the one before is live again.`)
            .then((r) => { if (r) changed(); })}>
          <Icon name="refresh" size={14} /> Restart</button>)}
      {kind === 'job' && o && (
        <button className={manual ? 'primary' : ''} disabled={!ec || !ec.write || busy}
          title={ec && ec.write ? 'a new run of the job, of the latest deployment' : NEEDS.write}
          onClick={() => run(() => api.runJob(env, name).then(() => true), `${name}: a new run starts.`)
            .then((r) => { if (r) changed(); })}>
          <Icon name="play" size={14} /> Run now</button>)}
      <a className="button" href={builder} onClick={go(builder)}
        title={ec && ec.write ? `change ${name} in the builder: a draft, planned, applied as the next deployment` : 'read-only: needs write on this environment'}>
        <Icon name="blocks" size={14} /> Edit</a>
    </>
  );

  const deps = o ? instanceDeployments(o) : [];
  const facts = (
    <>
      <span>in <Link to={envPath(tenant, env)} navigate={navigate} className="mono">{env}</Link></span>
      {o && isAttached(o) && (
        <span>attached in sandbox <Link to={`/tenants/${encodeURIComponent(tenant)}/sandboxes/${o.sandbox}`} navigate={navigate}
          className="mono" title={o.sandbox}>{shortId(o.sandbox)}</Link></span>)}
      {image.written && kind !== 'addon' && <span>image <ImageShort image={image} /></span>}
      {o && kind === 'addon' && <span><b className="mono">{o.type} {o.version}</b> · plan <b>{o.plan}</b> · {o.provisioned ? 'provisioned' : 'not provisioned yet'}</span>}
      {kind === 'job' && <span>runs <b>{manual ? 'when asked' : 'with each deployment that changes it'}</b></span>}
      {deps.length > 0 && <span>deployment <b className="mono">{deps.join(' · ')}</b>{deps.length > 1 ? ' (rolling)' : ''}</span>}
      {o && kind !== 'job' && <span>restarts <b className={'mono' + (restartsOf(o) > 0 ? ' warntext' : '')}>{restartsOf(o)}</b></span>}
    </>
  );

  return (
    <>
      <PageHead kind={KIND_NAMES[kind] || kind} title={name} mono
        state={<>
          <ToneLabel tone={tone}>{objectStateText(o)}</ToneLabel>
          {lastOk > 0 && <LiveBadge lastOk={lastOk} />}
        </>}
        actions={actions} facts={facts} />
      {op && op.tone === 'bad' ? <ErrBox error={op.text} /> : <OpNote op={op} onDismiss={clear} />}
      <section className="pane objpage">
        {!o && (
          <div className="message">
            <Icon name="alert" size={14} />
            <span>
              <strong>The latest deployment{st.deployment ? ` (${st.deployment})` : ''} has no {kind} {name}.</strong>
              <span className="muted"> What changed it is below; the deployments keep its earlier definitions.</span>
            </span>
          </div>)}
        <Definition tenant={tenant} st={st} kind={kind} name={name} so={so} spec={spec} navigate={navigate} />
        {kind === 'service' && <RoutesTo tenant={tenant} st={st} spec={spec} name={name} navigate={navigate} />}
        {kind === 'addon' && <AddonOutputs tenant={tenant} st={st} so={so} spec={spec} name={name} navigate={navigate} />}
        {kind === 'service' && <UsedBy tenant={tenant} env={env} spec={spec} kind={kind} name={name} navigate={navigate} />}
        {o && <Instances tenant={tenant} st={st} o={o} ec={ec} navigate={navigate} />}
        <ObjectIncidents tenant={tenant} env={env} kind={kind} name={name} navigate={navigate} />
        <ChangedIn tenant={tenant} env={env} latest={st.deployment} kind={kind} name={name} navigate={navigate} />
      </section>
    </>
  );
}

// Definition is the object as the latest deployment's normalised spec
// has it.
export function Definition({ tenant, st, kind, name, so, spec, navigate }) {
  const env = st.name;
  const n = st.deployment;
  const head = (
    <div className="sect">definition{n > 0 && <> · <Link to={envPath(tenant, env, 'deployments', n)} navigate={navigate}
      className="mono">deployment {n}</Link></>}</div>
  );
  if (!n) return <>{head}<p className="empty small">No deployment yet.</p></>;
  if (!spec) return <>{head}<p className="empty small">Loading…</p></>;
  if (!so) return <>{head}<p className="empty small">Not in this deployment.</p></>;
  const rows = definitionRows(so, kind);
  const vars = envRows(so, spec);
  const bindings = Array.isArray(so.bindings) ? so.bindings : [];
  const sandbox = (id) => `/tenants/${encodeURIComponent(tenant)}/sandboxes/${id}`;
  return (
    <>
      {head}
      <div className="kv-grid objdef">
        {rows.map((r) => (
          <KV key={r.k} k={r.k} className={r.mono ? 'mono small' : 'small'}>
            {r.sandbox ? <Link to={sandbox(r.v)} navigate={navigate}>{r.v}</Link> : r.v}
            {r.pinned && (
              <span className="objpin muted" title={r.pinned}>
                {' '}pinned to {r.pinned.slice(r.pinned.indexOf('@') + 1, r.pinned.indexOf('@') + 20)}…
                <CopyBtn text={r.pinned} mini title="copy the pinned reference" />
              </span>)}
          </KV>))}
      </div>

      {vars.length > 0 && (
        <>
          <div className="sect">environment <span className="count">· {vars.length}</span></div>
          <div className="tblwrap">
            <table className="tbl objenv-tbl">
              <tbody>
                {vars.map((v) => (
                  <tr key={v.name}>
                    <td className="mono small objenv-name">{v.name}</td>
                    <td className="mono small objenv-value"><EnvValue v={v} tenant={tenant} env={env} st={st} navigate={navigate} /></td>
                  </tr>))}
              </tbody>
            </table>
          </div>
        </>)}

      {bindings.length > 0 && (
        <>
          <div className="sect">bindings <span className="count">· {bindings.length}</span></div>
          <div className="objchips">
            {bindings.map((b) => {
              const def = spec.bindings && spec.bindings[b];
              return (
                <Link key={b} to={envPath(tenant, env)} navigate={navigate} className="objchip" title="the bindings, with their check, on the overview">
                  <span className="mono strong">{b}</span>
                  {def && def.upstream && <span className="mono small host">{def.upstream}</span>}
                </Link>);
            })}
          </div>
        </>)}
    </>
  );
}

// EnvValue is an environment variable's value: plain text, a secret at
// its pinned version (a link to the secrets), an addon's output or a
// service's address (links to their pages).
function EnvValue({ v, tenant, env, st, navigate }) {
  if (v.form === 'plain') return v.text === '' ? <span className="muted">""</span> : <>{v.text}</>;
  if (v.form === 'secret') {
    return (
      <span className="objref">
        <Icon name="lock" size={11} /> secret{' '}
        <Link to={envPath(tenant, env, 'secrets')} navigate={navigate} title="the environment's secrets">{v.target}</Link>
        {v.version ? <span className="muted"> v{v.version}</span> : <span className="muted"> (latest)</span>}
      </span>
    );
  }
  const known = (st.objects || []).some((o) => `${o.kind}/${o.name}` === v.ref);
  const to = envPath(tenant, env, 'objects', v.ref);
  return (
    <span className="objref">
      <span className="muted">{v.key || 'output'} of {v.form} </span>
      {known ? <Link to={to} navigate={navigate}>{v.target}</Link> : <span>{v.target}</span>}
      {v.detail && <span className="muted">{v.detail}</span>}
    </span>
  );
}

// RoutesTo are the routes that send to the service.
function RoutesTo({ tenant, st, spec, name, navigate }) {
  const list = routesTo(st, spec, name);
  return (
    <>
      <div className="sect">routes to it <span className="count">· {list.length}</span></div>
      {list.length === 0
        ? <p className="empty small">No route sends to it: it is reached inside the environment only.</p>
        : (
          <div className="tblwrap">
            <table className="tbl objroutes-tbl">
              <thead><tr><th>route</th><th>path</th><th className="col-num">port</th><th>served at</th></tr></thead>
              <tbody>
                {list.map((r) => (
                  <tr key={r.name}>
                    <td className="mono small">{r.name}</td>
                    <td className="mono">{r.path}</td>
                    <td className="col-num mono">{r.portAsWritten ? <><span className="muted">{r.portAsWritten} </span>{r.port}</> : r.port}</td>
                    <td className="small">
                      {r.url
                        ? <a className="mono host" href={r.url} target="_blank" rel="noreferrer">{r.url.replace(/^https:\/\//, '')}</a>
                        : <Link to={envPath(tenant, st.name, 'access')} navigate={navigate} className="muted">the route door, with a route token</Link>}
                    </td>
                  </tr>))}
              </tbody>
            </table>
          </div>)}
    </>
  );
}

// UsedBy are the objects whose environment names this one.
function UsedBy({ tenant, env, spec, kind, name, navigate }) {
  const list = usedBy(spec, kind, name);
  if (!spec || (kind === 'service' && list.length === 0)) return null;
  return (
    <>
      <div className="sect">used by <span className="count">· {list.length}</span></div>
      {list.length === 0
        ? <p className="empty small">No object of the environment names it.</p>
        : (
          <div className="tblwrap">
            <table className="tbl objused-tbl">
              <tbody>
                {list.map((u) => (
                  <tr key={`${u.from}/${u.var}`}>
                    <td className="mono small"><Link to={envPath(tenant, env, 'objects', u.from)} navigate={navigate}>{u.from}</Link></td>
                    <td className="mono small muted">{u.var}</td>
                    <td className="mono small">{u.text}</td>
                  </tr>))}
              </tbody>
            </table>
          </div>)}
    </>
  );
}

// AddonOutputs are an addon's type as the catalog has it: its output
// keys (those its users name marked), its plan's size; then who uses it.
function AddonOutputs({ tenant, st, so, spec, name, navigate }) {
  // The builtin list until ferund's catalog answers (or when it has none).
  const [catalog, setCatalog] = useState(() => builtinCatalog());
  useEffect(() => {
    let live = true;
    api.addonCatalog().then((a) => { const c = readCatalog(a); if (live && c) setCatalog(c); }, () => {});
    return () => { live = false; };
  }, []);
  const o = (st.objects || []).find((x) => x.kind === 'addon' && x.name === name);
  const type = (so && so.type) || (o && o.type) || '';
  const bp = catalog && catalog.byType.get(type);
  const used = new Set(usedBy(spec, 'addon', name).map((u) => u.text));
  const plan = bp && (bp.plans || []).find((p) => p.name === ((so && so.plan) || (o && o.plan)));
  return (
    <>
      <div className="sect">outputs</div>
      {!bp && <p className="empty small">The catalog has no type {type || '—'}.</p>}
      {bp && (
        <div className="kv-grid">
          <KV k="keys">
            <span className="objchips">
              {(bp.outputs || []).map((k) => (
                <span key={k} className={'labelchip mono' + (used.has(k) ? ' on' : '')} title={used.has(k) ? 'named by an object' : 'not named'}>{k}</span>))}
            </span>
          </KV>
          {(bp.fields || []).length > 0 && <KV k="fields" className="mono small">{bp.fields.join(', ')}</KV>}
          {plan && <KV k="plan" className="small">{planText(plan)}</KV>}
          <KV k="provisioned">{o ? (o.provisioned ? 'yes: its options are set up' : 'not yet') : '—'}</KV>
        </div>)}
      <UsedBy tenant={tenant} env={st.name} spec={spec} kind="addon" name={name} navigate={navigate} />
    </>
  );
}

// Instances are what runs of the object: an image service's and an
// addon's pods (the node an operator's link), an attached service's
// process, a job's last run.
function Instances({ tenant, st, o, ec, navigate }) {
  const env = st.name;
  if (o.kind === 'job') {
    return (
      <>
        <div className="sect">last run</div>
        <JobLine run={o.lastRun} />
      </>
    );
  }
  if (isAttached(o)) {
    return (
      <>
        <div className="sect">process</div>
        <div className="kv-grid">
          <KV k="sandbox">
            <Link to={`/tenants/${encodeURIComponent(tenant)}/sandboxes/${o.sandbox}`} navigate={navigate} className="mono">{o.sandbox}</Link>
          </KV>
          <KV k="phase"><ToneLabel tone={phaseTone(o.phase)}>{o.phase || 'starting'}</ToneLabel></KV>
          <KV k="restarts" className={'mono' + (o.restarts > 0 ? ' warntext' : '')}>{o.restarts || 0}</KV>
          <KV k="last exit" className={'mono' + (o.lastExit ? ' bad' : '')}>{o.lastExit == null ? '—' : o.lastExit}</KV>
        </div>
        {o.detail && <div className="envobj-detail">{o.detail}</div>}
      </>
    );
  }
  const n = (o.instances || []).length;
  return (
    <>
      <div className="sect">instances <span className="count">· {n}</span>
        {o.kind === 'service' && <span className="muted"> of {o.desired || 0} replica{o.desired === 1 ? '' : 's'}</span>}</div>
      <InstanceTable instances={o.instances} latest={st.deployment} navigate={navigate} operator={!!(ec && ec.operator)}
        logsTo={(replica) => envPath(tenant, env, 'logs', `${o.kind}/${o.name}`, replica)} />
      <p className="panel-note">The status keeps each instance's restarts and last exit; an incident keeps the exits that opened it.</p>
    </>
  );
}

// ObjectIncidents are the environment's incidents of this object, newest
// first.
function ObjectIncidents({ tenant, env, kind, name, navigate }) {
  const [list, setList] = useState(null);
  const refresh = useCallback(async () => {
    try {
      const r = await api.listIncidents(env);
      setList(objectIncidents(r.incidents || [], kind, name));
    } catch {
      setList([]);
    }
  }, [env, kind, name]);
  usePoll(refresh, 10000);
  if (!list || list.length === 0) return null;
  return (
    <>
      <div className="sect">incidents <span className="count">· {list.length}</span></div>
      <div className="tblwrap">
        <table className="tbl objinc-tbl">
          <tbody>
            {list.slice(0, 8).map((i) => {
              const to = envPath(tenant, env, 'incidents', i.id);
              const open = !i.closedAt;
              return (
                <tr key={i.id} className={'clickable' + (open ? '' : ' dim')} onClick={() => navigate(to)}>
                  <td className="col-state"><ToneDot tone={open ? 'failed' : 'absent'} title={open ? 'open' : 'closed'} /></td>
                  <td className="nowrap"><Link to={to} navigate={navigate} className="mono">#{i.id}</Link> {conditionText(i.condition)}</td>
                  <td className="small msgcell">{i.summary}</td>
                  <td className="col-num mono">{i.deployment}</td>
                  <td className="col-when"><Ago iso={i.openedAt} /></td>
                </tr>);
            })}
          </tbody>
        </table>
      </div>
    </>
  );
}

// diffs keeps the deployments' diffs this page read: a deployment never
// changes, so another object's page reads none of them again.
const diffs = new Map();

// useDiffs reads the newest CHANGES_READ deployments with their diffs,
// one after the other, newest first; {list, older, done, err}.
function useDiffs(env, latest) {
  const [res, setRes] = useState({ list: [], older: false, done: false, err: '' });
  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const r = await api.listDeployments(env, CHANGES_READ);
        const deps = (r.deployments || []).slice(0, CHANGES_READ);
        const older = deps.length > 0 && deps[deps.length - 1].number > 1;
        const list = [];
        for (const d of deps) {
          const key = `${env}/${d.number}`;
          if (!diffs.has(key)) diffs.set(key, (await api.getDeployment(env, d.number)).diff || []);
          if (!live) return;
          list.push({ ...d, diff: diffs.get(key) });
          setRes({ list: [...list], older, done: list.length === deps.length, err: '' });
        }
        if (deps.length === 0 && live) setRes({ list: [], older: false, done: true, err: '' });
      } catch (e) {
        if (live) setRes((cur) => ({ ...cur, done: true, err: e.message }));
      }
    })();
    return () => { live = false; };
  }, [env, latest]);
  return res;
}

// ChangedIn are the deployments, of the newest CHANGES_READ, whose diff
// changed the object, with the fields from → to.
function ChangedIn({ tenant, env, latest, kind, name, navigate }) {
  const { list, older, done, err } = useDiffs(env, latest);
  const changes = changesTo(list, kind, name);
  return (
    <>
      <div className="sect">changed in <span className="count">· {changes.length}</span></div>
      <ErrBox error={err} />
      {changes.length === 0 && done && !err && <p className="empty small">No change to it in the last {list.length} deployments.</p>}
      <div className="objchanges">
        {changes.map((c) => (
          <div key={c.number} className="objchange">
            <div className="diff-head">
              <Link to={envPath(tenant, env, 'deployments', c.number)} navigate={navigate} className="mono strong">deployment {c.number}</Link>
              <ToneLabel tone={deploymentTone(c.state)}>{c.state}</ToneLabel>
              <span className={'badge ' + changeTone(c.change)}>{c.change}</span>
              {c.effect && <span className={'small ' + (heavyEffect(c.effect) ? 'bad' : 'muted')}>{c.effect}</span>}
              <span className="spacer" />
              {c.author && <span className="mono small muted">{c.author}</span>}
              <Ago iso={c.createdAt} />
            </div>
            {c.message && <div className="small objchange-msg">{c.message}</div>}
            {c.fields.length > 0 && (
              <div className="tblwrap">
                <table className="tbl diff-tbl">
                  <tbody>
                    {c.fields.map((f) => (
                      <tr key={f.path}>
                        <td className="mono small diff-path">{f.path}</td>
                        <td className="mono small diff-from">{fmtValue(f.from)}</td>
                        <td className="muted diff-arrow">→</td>
                        <td className="mono small diff-to">{fmtValue(f.to)}</td>
                      </tr>))}
                  </tbody>
                </table>
              </div>)}
          </div>))}
      </div>
      <p className="panel-note">
        {done ? `Read from the diffs of the last ${list.length} deployment${list.length === 1 ? '' : 's'}, one request each`
          : `Reading the diffs of the last ${CHANGES_READ} deployments, one request each: ${list.length} so far`}
        {older && <>; older ones: <Link to={envPath(tenant, env, 'deployments')} navigate={navigate}>the deployments</Link></>}.
      </p>
    </>
  );
}

