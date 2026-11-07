// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, CopyBtn, ErrBox, KV, usePoll } from '../components.jsx';
import { fmtDateTime, shortId } from '../format.js';
import { envPath } from '../routes.js';
import {
  KIND_TITLES, activityItems, conditionText, defaultHost, errorShare, fmtCount, fmtLatency, groupObjects, incidentObject,
  isAttached, jobTrigger, labelPairs, objectImage, objectTone, objectsText, readyText, restartsOf, rolloutNote, routesTo,
  sparkPoints, specBindings, trafficOf, trafficTotals,
} from './envs.js';
import { EnvLink, HostLink, NEEDS, OpNote, useOp } from './EnvTabs.jsx';
import { BindingCard, Labels, ToneDot } from './Environments.jsx';
import { useSummary } from '../summary.jsx';
import '../styles/overview.css';

// An environment's overview: first what needs someone, in a sentence with
// its evidence and the next step (its open incidents, a failed or running
// rollout), or that every object matches the deployment; then its
// traffic and where it is reached; its objects as cards (each leads to
// its page, where its logs, traces, traffic and config are); its bindings
// and what happened lately. A state is said once.

const Link = EnvLink;

// RECENT is how many entries of the activity the overview shows.
const RECENT = 4;

// WideSpark is a series as a line across its box.
export function WideSpark({ series, h = 30 }) {
  const w = 300;
  const req = sparkPoints(series, 'requests', w, h);
  if (!req) return null;
  const hasErr = (series || []).some((p) => p.errors > 0);
  const top = Math.max(...(series || []).map((p) => Number(p.requests) || 0));
  return (
    <svg className="wspark" viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" aria-hidden="true">
      <polyline className="spark-req" points={req} />
      {hasErr && <polyline className="spark-err" points={sparkPoints(series, 'errors', w, h, top)} />}
    </svg>
  );
}

export function EnvOverview({
  st, tenant, navigate, spec = null, traffic = null, ec = null, changed = () => {}, incidents = null, activity = null,
}) {
  const groups = groupObjects(st.objects);
  const bindings = specBindings(spec);
  return (
    <div className="ov">
      <StatusCards st={st} incidents={incidents} tenant={tenant} navigate={navigate} ec={ec} />

      {(st.objects || []).length > 0 && (
        <div className="ov-two">
          <TrafficCard st={st} traffic={traffic} tenant={tenant} navigate={navigate} />
          <ReachCard st={st} tenant={tenant} navigate={navigate} />
        </div>)}

      {groups.map(({ kind, objects }) => (
        <section key={kind} className="ov-sect" aria-label={KIND_TITLES[kind]}>
          <h2 className="ov-h">{KIND_TITLES[kind][0].toUpperCase() + KIND_TITLES[kind].slice(1)} <span className="count">{objects.length}</span></h2>
          <div className="ov-grid">
            {objects.map((o) => (
              <ObjectCard key={`${o.kind}/${o.name}`} o={o} st={st} spec={spec} traffic={traffic} tenant={tenant}
                navigate={navigate} ec={ec} changed={changed} />))}
          </div>
        </section>
      ))}

      {bindings.length > 0 && (
        <section className="ov-sect">
          <h2 className="ov-h">Bindings <span className="count">{bindings.length}</span></h2>
          {bindings.map((b) => <BindingCard key={b.name} b={b} env={st.name} ec={ec} tenant={tenant} navigate={navigate} />)}
        </section>)}

      {activity && activity.length > 0 && (
        <section className="card ov-activity">
          <div className="card-head">
            <h2>Recent activity</h2>
            <Link to={envPath(tenant, st.name, 'activity')} navigate={navigate} className="card-more">All activity</Link>
          </div>
          <ActivityList items={activity.slice(0, RECENT)} tenant={tenant} env={st.name} navigate={navigate} />
        </section>)}
    </div>
  );
}

// StatusCards say first what needs someone: each open incident with its
// summary, the end of its log and the way to its object; a failed or
// running rollout with its reason; else that every object matches the
// deployment.
export function StatusCards({ st, incidents, tenant, navigate, ec = null }) {
  const r = st.rollout;
  const env = st.name;
  const open = (incidents || []).filter((i) => !i.closedAt);
  const cards = [];
  if (st.deleting) {
    cards.push(
      <StateCard key="deleting" tone="moving" icon="trash" title="Deleting"
        text="ferund removes its instances, volumes and groups, then the environment." />);
  } else if (!r) {
    const builder = envPath(tenant, env, 'builder');
    cards.push(
      <StateCard key="none" tone="absent" icon="layers" title="No deployment yet" text="Nothing runs. A deployment says what should."
        actions={ec && ec.write && <Link to={builder} navigate={navigate} className="button primary"><Icon name="plus" size={14} /> New deployment</Link>} />);
  }
  for (const inc of open.slice(0, 3)) {
    cards.push(<IncidentCard key={inc.id} inc={inc} st={st} tenant={tenant} navigate={navigate} />);
  }
  if (open.length > 3) {
    cards.push(
      <p key="more" className="ov-more">
        <Link to={envPath(tenant, env, 'incidents')} navigate={navigate}>{open.length - 3} more open incidents</Link>
      </p>);
  }
  if (r && !st.deleting && r.state !== 'deployed') {
    const note = rolloutNote(st);
    const failed = r.state === 'failed';
    cards.push(
      <StateCard key="rollout" tone={failed ? 'failed' : 'moving'} icon={failed ? 'alert' : 'refresh'}
        title={`Deployment ${r.deployment} ${r.state}`}
        when={r.startedAt && <>started <Ago iso={r.startedAt} /></>}
        text={<>{r.reason && <span className="ov-reason">{r.reason}</span>}{note && <span>{note[0].toUpperCase() + note.slice(1)}.</span>}</>}
        actions={<Link to={envPath(tenant, env, 'deployments', r.deployment)} navigate={navigate} className="button">Deployment {r.deployment}</Link>} />);
  }
  if (r && !st.deleting && r.state === 'deployed' && open.length === 0 && incidents !== null) {
    const what = objectsText(st.objects);
    cards.push(
      <StateCard key="ok" tone="running" icon="check" title={`Every object matches deployment ${r.deployment}`}
        text={`${what ? `${what[0].toUpperCase() + what.slice(1)} run as deployed. ` : ''}No open incidents.`} />);
  }
  return cards.length > 0 ? <div className="ov-status">{cards}</div> : null;
}

export function StateCard({ tone, icon, title, text, when = null, actions = null, children = null }) {
  return (
    <section className={'statecard ' + tone}>
      <span className="statecard-icon"><Icon name={icon} size={20} /></span>
      <div className="statecard-body">
        <div className="statecard-head"><h2>{title}</h2>{when && <span className="statecard-when">{when}</span>}</div>
        {text && <p className="statecard-text">{text}</p>}
        {children}
        {actions && <div className="statecard-actions">{actions}</div>}
      </div>
    </section>
  );
}

// IncidentCard is an open incident: what and since when, its summary, the
// end of its object's log, and the way to the object, the incident and
// its deployment.
export function IncidentCard({ inc, st, tenant, navigate }) {
  const env = st.name;
  const obj = incidentObject(inc.object);
  const known = (st.objects || []).some((o) => `${o.kind}/${o.name}` === inc.object);
  const tail = ((inc.evidence && inc.evidence.logTail) || []).slice(-3);
  const objTo = envPath(tenant, env, 'objects', inc.object);
  return (
    <StateCard tone="failed" icon="alert" title={<>{obj.name} <span className="statecard-cond">{conditionText(inc.condition)}</span></>}
      when={<>since <Ago iso={inc.openedAt} /></>} text={inc.summary}
      actions={<>
        {known && <Link to={objTo} navigate={navigate} className="button primary">Open {obj.name} <Icon name="arrowRight" size={14} /></Link>}
        <Link to={envPath(tenant, env, 'incidents', inc.id)} navigate={navigate} className="button">Incident {inc.id}</Link>
        <Link to={envPath(tenant, env, 'deployments', inc.deployment)} navigate={navigate} className="button">Deployment {inc.deployment}</Link>
      </>}>
      {tail.length > 0 && (
        <>
          <p className="statecard-cap">The end of its log</p>
          <pre className="statecard-log">{tail.join('\n')}</pre>
        </>)}
    </StateCard>
  );
}

// TrafficCard is what the ingress answered in the last hour: the sum, and
// a line per service with requests.
function TrafficCard({ st, traffic, tenant, navigate }) {
  const env = st.name;
  const totals = trafficTotals(traffic);
  const list = ((traffic && traffic.services) || []).filter((s) => s.requests > 0);
  return (
    <section className="card">
      <div className="card-head">
        <h2>Traffic</h2><span className="muted">last hour</span>
        <Link to={envPath(tenant, env, 'traffic')} navigate={navigate} className="card-more">Traffic</Link>
      </div>
      {!traffic && <p className="muted">No numbers: the log store does not answer.</p>}
      {totals && (
        <p className="ov-big">
          {fmtCount(totals.requests)} request{totals.requests === 1 ? '' : 's'}
          {totals.requests > 0 && (
            <span> · {errorShare(totals)} errors · p95 {fmtLatency(totals.p95Ms)}</span>)}
        </p>)}
      {list.map((s) => (
        <div key={s.service} className="ov-trow">
          <Link to={envPath(tenant, env, 'objects', `service/${s.service}`, null)} navigate={navigate} className="strong">{s.service}</Link>
          <WideSpark series={s.series} />
          <span className="ov-trow-nums">{fmtCount(s.requests)} req · p95 {fmtLatency(s.p95Ms)}</span>
        </div>))}
      {traffic && list.length === 0 && <p className="muted">No requests through the ingress in the last hour.</p>}
    </section>
  );
}

// ReachCard is where the environment is reached: its public hosts and
// the routes to its services, or the route door while it is not public.
function ReachCard({ st, tenant, navigate }) {
  const env = st.name;
  const { domains } = useSummary();
  const dh = defaultHost(st, domains || []);
  const routes = st.routes || [];
  return (
    <section className="card">
      <div className="card-head">
        <h2>{st.public ? 'Reachable at' : 'Not public'}</h2>
        <Link to={envPath(tenant, env, 'access')} navigate={navigate} className="card-more">Domains &amp; access</Link>
      </div>
      {st.public && (st.hosts || []).length > 0 && (
        <div className="ov-hosts">{st.hosts.map((h) => <HostLink key={h} host={h} />)}</div>)}
      {!st.public && <p className="muted">The route door serves its routes to a route token.</p>}
      {dh && !dh.serving && st.public && (
        <p className="muted small">default host <span className="mono">{dh.label}</span> answers 404 while a domain is served</p>)}
      {routes.map((r) => (
        <div key={r.name} className="ov-route">
          <span className="mono">{r.path}</span>
          <Icon name="arrowRight" size={14} />
          <span>
            <Link to={envPath(tenant, env, 'objects', `service/${r.service}`)} navigate={navigate} className="strong">{r.service}</Link>
            <span className="muted"> port {r.port}</span>
          </span>
        </div>))}
      {routes.length === 0 && <p className="muted">No routes: its services are reached inside the environment only.</p>}
    </section>
  );
}

// ObjectCard is an object: its state, name and readiness, what it runs,
// and one line of how it is; it leads to the object's page. A job's card
// runs it.
function ObjectCard({ o, st, spec, traffic, tenant, navigate, ec, changed }) {
  const env = st.name;
  const tone = objectTone(o);
  const to = envPath(tenant, env, 'objects', `${o.kind}/${o.name}`);
  const [op, run, clear] = useOp();
  const image = objectImage(o, spec);
  const t = o.kind === 'service' ? trafficOf(traffic, o.name) : null;
  const routed = o.kind === 'service' && routesTo(st, spec, o.name).length > 0;
  const manual = o.kind === 'job' && jobTrigger(spec, o.name) === 'manual';
  const open = (e) => {
    if (e.target.closest('a,button')) return;
    navigate(to);
  };
  return (
    <div className={'ocard ' + tone} onClick={open}>
      <div className="ocard-top">
        <ToneDot tone={tone} title={o.phase || tone} />
        <Link to={to} navigate={navigate} className="ocard-name">{o.name}</Link>
        {readyText(o) && <span className={'ocard-ready ' + tone}>{readyText(o)}</span>}
        {o.kind === 'job' && ec && (
          <button className={'mini' + (manual ? ' primary' : '')} disabled={!ec.write || !!(op && op.busy)}
            title={ec.write ? 'a new run of the job, of the latest deployment' : NEEDS.write}
            onClick={() => run(() => api.runJob(env, o.name).then(() => true), `${o.name}: a new run starts.`)
              .then((r) => { if (r) changed(); })}>
            <Icon name="play" size={12} /> Run now</button>)}
      </div>
      <div className="ocard-what">{whatText(o, image)}</div>
      {t && t.requests > 0 && <WideSpark series={t.series} h={26} />}
      <div className={'ocard-line ' + tone}>{cardLine(o, t, routed, manual, !!traffic)}</div>
      {op && op.text && <OpNote op={op} onDismiss={clear} />}
    </div>
  );
}

// whatText is what an object runs: an image short, an addon's type and
// plan, an attached service's sandbox.
function whatText(o, image) {
  if (o.kind === 'addon') return <span>{o.type} {o.version} · {o.plan}</span>;
  if (isAttached(o)) return <span>attached in sandbox <span className="mono">{shortId(o.sandbox)}</span></span>;
  return image.short ? <span className="mono" title={image.written}>{image.short}</span> : <span className="muted">—</span>;
}

// cardLine is how an object is, in one line: what is wrong while
// something is, else its traffic (counted: the traffic was read), its
// provisioning or its last run.
function cardLine(o, t, routed, manual, counted) {
  const tone = objectTone(o);
  if (o.kind === 'job') {
    const r = o.lastRun;
    if (!r) return manual ? 'runs when asked · not run yet' : 'not run yet';
    return <>last run {r.phase}{r.finishedAt && <> <Ago iso={r.finishedAt} /></>}{manual ? ' · runs when asked' : ''}</>;
  }
  if (o.kind === 'addon') {
    if (tone === 'running') return 'provisioned';
    return o.phase === 'ready' && !o.provisioned ? 'ready, not provisioned yet' : o.phase || 'starting';
  }
  if (isAttached(o)) {
    return o.restarts > 0 ? `${o.phase || 'starting'} · ${o.restarts} restart${o.restarts === 1 ? '' : 's'}` : o.phase || 'starting';
  }
  const restarts = restartsOf(o);
  if (tone === 'failed') {
    const exit = (o.instances || []).filter((i) => i.lastExit != null).map((i) => i.lastExit).pop();
    return `${restarts} restart${restarts === 1 ? '' : 's'}${exit != null ? ` · last exit ${exit}` : ''}`;
  }
  if (tone === 'moving') return `${o.ready || 0} of ${o.desired || 0} ready`;
  if (t && t.requests > 0) return `${fmtCount(t.requests)} requests last hour · p95 ${fmtLatency(t.p95Ms)}`;
  if (!routed) return 'internal · no route to it';
  return counted ? 'no requests in the last hour' : 'reached by a route';
}

// ActivityList is the activity's entries: each with its tone, what, its
// detail and when, a link to the deployment or incident.
export function ActivityList({ items, tenant, env, navigate }) {
  return (
    <div className="actlist">
      {items.map((a) => {
        const to = a.kind === 'deployment' ? envPath(tenant, env, 'deployments', a.number) : envPath(tenant, env, 'incidents', a.id);
        return (
          <div key={a.key} className={'act' + (a.kind === 'incident' && !a.open ? ' dim' : '')}>
            <ToneDot tone={a.tone} title={a.kind} />
            <div className="act-what">
              <Link to={to} navigate={navigate} className="act-title">{a.title}</Link>
              {a.detail && <div className="act-detail">{a.detail}</div>}
              {a.reason && <div className="act-detail bad">{a.reason}</div>}
            </div>
            <span className="act-when" title={fmtDateTime(a.at)}><Ago iso={a.at} /></span>
          </div>
        );
      })}
    </div>
  );
}

// useActivity reads an environment's newest deployments and its
// incidents, as one list (activityItems); null until read.
export function useActivity(env, limit = 20, ms = 15000) {
  const [items, setItems] = useState(null);
  const [err, setErr] = useState('');
  const refresh = useCallback(async () => {
    try {
      const [d, i] = await Promise.all([api.listDeployments(env, limit), api.listIncidents(env)]);
      setItems(activityItems((d.deployments || []).slice(0, limit), i.incidents || []));
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [env, limit]);
  usePoll(refresh, ms);
  return [items, err];
}

// ActivityTab is everything that happened to the environment: its
// deployments and its incidents, newest first.
export function ActivityTab({ tenant, env, navigate }) {
  const [items, err] = useActivity(env, 50, 5000);
  return (
    <>
      <ErrBox error={err} />
      {items && items.length > 0 && <ActivityList items={items} tenant={tenant} env={env} navigate={navigate} />}
      {items && items.length === 0 && <p className="empty">Nothing yet: no deployment, no incident.</p>}
      {!items && !err && <p className="empty">Loading…</p>}
    </>
  );
}

// AboutBox is the environment itself: its labels, when it was made and
// its ID.
export function AboutBox({ st }) {
  return (
    <section className="card about">
      <div className="card-head"><h2>About</h2></div>
      <div className="kv-grid">
        <KV k="name"><span className="mono">{st.name}</span></KV>
        <KV k="ID"><span className="mono small">{st.id}</span> <CopyBtn text={st.id} mini title="copy the environment's ID" /></KV>
        <KV k="created">{fmtDateTime(st.createdAt)} <Ago iso={st.createdAt} /></KV>
        <KV k="labels">{labelPairs(st.labels).length > 0 ? <Labels labels={st.labels} /> : <span className="muted">none</span>}</KV>
      </div>
    </section>
  );
}

