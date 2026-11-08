// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { ErrBox, usePoll } from '../components.jsx';
import { shortId } from '../format.js';
import { envPath, jobPath, objectPath, sandboxPath } from '../routes.js';
import { EnvLink } from './EnvTabs.jsx';
import { HOW_WORDS, reachModel } from './reach.js';
import { onItsWay } from './jobs.js';
import { FlowLegend, ReachFlow, kindIcon, useDeployed } from './Reach.jsx';
import '../styles/reach.css';

// The reachability of an environment, read (ADR 0034, revised; ADR
// 0035): its Reachability view, a card per object with its picture (who
// reaches it, with the port and the reference it comes from; what it
// reaches in the environment; its egress), then its sandboxes and running
// jobs with what they reach; and an object's row of it on its Config
// view. Drawn from the deployed deployment's reachability; references
// change in New deployment, a sandbox's reaches on its page.

// useEnvPods reads env's sandboxes and running jobs every 10 s:
// {sandboxes, jobs, pods, err}; pods are reachModel's.
function useEnvPods(env) {
  const [state, setState] = useState({ sandboxes: null, jobs: null, err: '' });
  const refresh = useCallback(async () => {
    try {
      const [s, j] = await Promise.all([api.listSandboxes(env), api.listJobs([], env)]);
      setState({ sandboxes: s.sandboxes || [], jobs: (j.jobs || []).filter((x) => onItsWay(x.phase)), err: '' });
    } catch (e) {
      setState((p) => ({ ...p, err: e.message }));
    }
  }, [env]);
  usePoll(refresh, 10000);
  return state;
}

// podsOf are the sandboxes and jobs as reachModel takes them.
export function podsOf(sandboxes, jobs) {
  return [
    ...(sandboxes || []).map((s) => ({ kind: 'sandbox', id: s.sandboxId, reaches: s.reaches, egress: s.egress })),
    ...(jobs || []).map((j) => ({ kind: 'jobrun', id: j.id, reaches: j.reaches, egress: j.egress })),
  ];
}

// peerLink links a pill of the picture to its object's or pod's page.
function peerLink(tenant, env, navigate) {
  return (p, pill) => {
    let to = '';
    if ((p.type === 'service' || p.type === 'job' || p.type === 'addon') && !p.missing) to = objectPath(tenant, env, `${p.type}/${p.name}`, 'config');
    if (p.type === 'sandbox') to = sandboxPath(tenant, p.name);
    if (p.type === 'jobrun') to = jobPath(tenant, p.name);
    return to ? <EnvLink to={to} navigate={navigate} className="sgf-link">{pill}</EnvLink> : null;
  };
}

// EnvReach is an environment's Reachability view.
export function EnvReach({ tenant, st, navigate }) {
  const env = st.name;
  const { n, spec, reach, err } = useDeployed(env);
  const pods = useEnvPods(env);
  const latest = Number(st.deployment) || 0;
  const model = spec ? reachModel(spec, reach, podsOf(pods.sandboxes, pods.jobs)) : null;
  const link = peerLink(tenant, env, navigate);
  return (
    <div className="sg-page">
      <div className="sg-top">
        <div className="sg-lede">
          <Icon name="lock" size={15} />
          <div>
            <p>{HOW_WORDS}</p>
            <p className="muted small">
              {n > 0
                ? <>As <EnvLink to={envPath(tenant, env, 'deployments', n)} navigate={navigate}>deployment {n}</EnvLink> declares it.</>
                : <>No deployment yet: nothing is declared, nothing reaches anything.</>}
              {latest > n && n > 0 && <span className="sg-pending"> Deployment {latest} is not deployed; its references count once it is.</span>}
            </p>
          </div>
        </div>
      </div>
      <ErrBox error={err || pods.err} />
      {!spec && !err && <p className="empty">Loading…</p>}
      {spec && n > 0 && !reach && (
        <p className="panel-note">This ferund answers no reachability with a deployment yet; the pictures fill once it does.</p>)}
      {model && model.objects.length > 0 && <FlowLegend mid="Object" />}
      {model && model.objects.map((o) => (
        <section key={o.ref} className="card sg-card" id={`reach-${o.kind}-${o.name}`} aria-label={`${o.kind} ${o.name}`}>
          <ReachFlow ins={o.ins} outs={o.outs} egress={o.egress} link={link} heads={false}>
            <ObjectBox tenant={tenant} env={env} o={o} navigate={navigate} />
          </ReachFlow>
        </section>))}
      {model && (
        <>
          <h2 className="sect rc-podhead"><Icon name="terminal" size={13} /> Sandboxes and jobs
            <span className="muted small"> · what they reach is said at their create, a sandbox&apos;s changed on its page</span></h2>
          {!pods.sandboxes && !pods.err && <p className="empty small">Loading…</p>}
          {pods.sandboxes && model.pods.length === 0 && <p className="empty small">No sandbox and no running job in this environment.</p>}
          {model.pods.map((p) => (
            <section key={p.ref} className="card sg-card" aria-label={`${p.kind} ${p.name}`}>
              <ReachFlow ins={p.ins} outs={p.outs} egress={p.egress} link={link} heads={false}
                inWords="Nothing gets in">
                <div className="sgf-self">
                  <Icon name={kindIcon(p.kind)} size={14} />
                  <EnvLink to={p.kind === 'sandbox' ? sandboxPath(tenant, p.name) : jobPath(tenant, p.name)} navigate={navigate} className="mono">
                    {p.kind === 'sandbox' ? p.name : shortId(p.name)}</EnvLink>
                </div>
                <div className="sgf-boxhead">{p.kind === 'sandbox' ? 'sandbox' : 'job run'}
                  {p.attachedRefs.length > 0 && <> · runs {p.attachedRefs.map((r) => r.slice(r.indexOf('/') + 1)).join(', ')}</>}</div>
              </ReachFlow>
            </section>))}
        </>)}
    </div>
  );
}

// ObjectBox is the middle of an object's card: its kind and name, a
// link to its Config.
function ObjectBox({ tenant, env, o, navigate }) {
  return (
    <>
      <div className="sgf-self">
        <Icon name={kindIcon(o.kind)} size={14} />
        <EnvLink to={objectPath(tenant, env, o.ref, 'config')} navigate={navigate} className="mono">{o.name}</EnvLink>
      </div>
      <div className="sgf-boxhead">{o.kind}{o.attached && <> · in sandbox <span className="mono">{o.attached}</span></>}</div>
    </>
  );
}

// ObjectReach is the reachability section of an object's Config: its
// row of the environment's picture, from the deployed deployment.
export function ObjectReach({ tenant, st, kind, name, navigate }) {
  const env = st.name;
  const { n, spec, reach, err } = useDeployed(env);
  const all = envPath(tenant, env, 'reachability');
  const head = (
    <div className="sect sg-objsect">
      Reachability
      {n > 0 && <span className="muted small">as deployment {n} declares it</span>}
      <EnvLink to={all} navigate={navigate} className="small sg-all">The whole environment</EnvLink>
    </div>
  );
  if (err) return <>{head}<p className="muted small">{err}</p></>;
  if (!spec) return <>{head}<p className="empty small">Loading…</p></>;
  return <>{head}<ObjectReachPicture tenant={tenant} env={env} spec={spec} reach={reach} kind={kind} name={name} n={n} navigate={navigate} /></>;
}

// ObjectReachPicture is an object's picture from spec and reach, the
// deployed deployment's.
export function ObjectReachPicture({ tenant, env, spec, reach, kind, name, n = 1, navigate }) {
  if (!n) return <p className="empty small">Not deployed yet.</p>;
  if (!reach) return <p className="muted small">This ferund answers no reachability with a deployment yet.</p>;
  const o = reachModel(spec, reach).objects.find((x) => x.ref === `${kind}/${name}`);
  if (!o) return <p className="empty small">Not in the deployed deployment.</p>;
  return (
    <>
      {o.attached && (
        <p className="small">Runs in sandbox{' '}
          <EnvLink to={sandboxPath(tenant, o.attached)} navigate={navigate} className="mono">{o.attached}</EnvLink>: its calls are the
          sandbox&apos;s pod&apos;s, and so is its egress.</p>)}
      <ReachFlow ins={o.ins} outs={o.outs} egress={o.egress} link={peerLink(tenant, env, navigate)}>
        <div className="sgf-self">
          <Icon name={kindIcon(kind)} size={14} />
          <span className="mono">{name || kind}</span>
        </div>
      </ReachFlow>
    </>
  );
}
