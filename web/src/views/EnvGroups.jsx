// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { ErrBox, usePoll } from '../components.jsx';
import { shortId } from '../format.js';
import { envPath, jobPath, objectPath, sandboxPath } from '../routes.js';
import { EnvLink } from './EnvTabs.jsx';
import {
  DEFAULT_GROUP, RULES_WORDS, attachedTo, groupMembers, groupNames, groupOf, groupWords, inRules, objectGroups,
  objectReach, outsideWords, podGroups, reachWords, reaches, ruleWords,
} from './groups.js';
import { onItsWay } from './jobs.js';
import { GroupChips } from './SecurityGroups.jsx';
import '../styles/groups.css';

// The security groups of an environment, read (ADR 0034, ADR 0035): its
// Security groups view, each group with its rules in words, what its
// members reach, and who is in it — the objects of the deployed
// deployment, the environment's sandboxes and its running jobs; and an
// object's groups on its Config view. Groups change in New deployment.

// useDeployedSpec reads the spec of st's deployed deployment: {n, spec};
// spec null while unread, {} without a deployment.
function useDeployedSpec(st) {
  const n = st ? Number(st.deployed) || 0 : 0;
  const env = st ? st.name : '';
  const [got, setGot] = useState({ key: '', spec: null, err: '' });
  const key = `${env}/${n}`;
  useEffect(() => {
    if (!env || !n) return undefined;
    let live = true;
    api.getDeployment(env, n).then(
      (d) => { if (live) setGot({ key, spec: d.spec || {}, err: '' }); },
      (e) => { if (live) setGot({ key, spec: null, err: e.message }); });
    return () => { live = false; };
  }, [env, n, key]);
  if (!n) return { n, spec: {}, err: '' };
  return got.key === key ? { n, ...got } : { n, spec: null, err: '' };
}

// useEnvPods reads env's sandboxes and running jobs every 10 s.
function useEnvPods(env) {
  const [pods, setPods] = useState({ sandboxes: null, jobs: null, err: '' });
  const refresh = useCallback(async () => {
    try {
      const [s, j] = await Promise.all([api.listSandboxes(env), api.listJobs([], env)]);
      setPods({ sandboxes: s.sandboxes || [], jobs: (j.jobs || []).filter((x) => onItsWay(x.phase)), err: '' });
    } catch (e) {
      setPods((p) => ({ ...p, err: e.message }));
    }
  }, [env]);
  usePoll(refresh, 10000);
  return pods;
}

// EnvGroups is an environment's Security groups view.
export function EnvGroups({ tenant, st, ec, navigate }) {
  const env = st.name;
  const { n, spec, err } = useDeployedSpec(st);
  const pods = useEnvPods(env);
  const latest = Number(st.deployment) || 0;
  const builder = (name) => envPath(tenant, env, 'builder', name ? `securityGroup/${name}` : '');
  const names = spec ? groupNames(spec) : [];
  return (
    <div className="sg-page">
      <p className="muted small sg-intro">{RULES_WORDS}</p>
      <p className="small sg-from">
        {n > 0
          ? <>The groups of <EnvLink to={envPath(tenant, env, 'deployments', n)} navigate={navigate} className="mono">deployment {n}</EnvLink>,
            the one deployed.</>
          : <>No deployment is deployed yet: every pod is in <span className="mono">default</span>, nothing in, internet out.</>}
        {latest > n && n > 0 && (
          <> Deployment <EnvLink to={envPath(tenant, env, 'deployments', latest)} navigate={navigate} className="mono">{latest}</EnvLink>
            {' '}is not deployed; its groups count once it is.</>)}
        {ec.write && <> · <EnvLink to={builder('')} navigate={navigate}><Icon name="plus" size={11} /> change them in a new deployment</EnvLink></>}
      </p>
      <ErrBox error={err || pods.err} />
      {!spec && !err && <p className="empty">Loading…</p>}
      {spec && names.map((name) => (
        <GroupCard key={name} tenant={tenant} env={env} spec={spec} name={name} pods={pods}
          edit={ec.write ? builder(name) : ''} navigate={navigate} />))}
    </div>
  );
}

// GroupCard is one group: its words, its in and out rules, what its
// members reach in the environment, and its members.
function GroupCard({ tenant, env, spec, name, pods, edit, navigate }) {
  const g = groupOf(spec, name) || {};
  const ins = inRules(spec, name);
  const out = outsideWords(g);
  const inside = reaches(spec, name);
  const objects = groupMembers(spec, name);
  const sandboxes = (pods.sandboxes || []).filter((p) => podGroups(p).includes(name));
  const jobs = (pods.jobs || []).filter((p) => podGroups(p).includes(name));
  const count = objects.length + sandboxes.length + jobs.length;
  return (
    <section className="envbox sg-card" id={`group-${name}`} aria-label={`security group ${name}`}>
      <div className="sect sg-cardhead">
        <span className="mono sg-name">{name}</span>
        <span className="sg-cardwords">{groupWords(g)}</span>
        {name === DEFAULT_GROUP && <span className="muted small">· every pod that names no group</span>}
        {edit && <EnvLink to={edit} navigate={navigate} className="small sg-edit">change</EnvLink>}
      </div>
      <div className="sg-cols">
        <RuleList title="in" empty="nothing: no pod reaches its members" list={ins.map((r) => ruleWords(r, 'in'))} />
        <RuleList title="reaches in the environment"
          empty="no group lets its members in"
          list={inside.map(({ group, rule }) => <><a href={`#group-${group}`}>{reachWords(rule, group)}</a></>)} />
        <RuleList title="reaches outside" empty="nothing outside the environment (out: [])" list={out} />
      </div>
      <div className="sg-members">
        <span className="k">members · {pods.sandboxes ? count : `${objects.length}…`}</span>
        {count === 0 && pods.sandboxes && <span className="muted small">none</span>}
        {objects.map((o) => (
          <EnvLink key={`${o.kind}/${o.name}`} to={objectPath(tenant, env, `${o.kind}/${o.name}`, 'config')} navigate={navigate}
            className="objchip">
            <span className="mono strong">{o.name}</span><span className="muted small">{o.kind}</span>
          </EnvLink>))}
        {sandboxes.map((s) => (
          <EnvLink key={s.sandboxId} to={sandboxPath(tenant, s.sandboxId)} navigate={navigate} className="objchip">
            <span className="mono strong">{s.sandboxId}</span><span className="muted small">sandbox</span>
          </EnvLink>))}
        {jobs.map((j) => (
          <EnvLink key={j.id} to={jobPath(tenant, j.id)} navigate={navigate} className="objchip">
            <span className="mono strong">{shortId(j.id)}</span><span className="muted small">job</span>
          </EnvLink>))}
      </div>
    </section>
  );
}

// RuleList is a column of rules in words, or what none means.
function RuleList({ title, list, empty }) {
  return (
    <div className="sg-col">
      <div className="k">{title}</div>
      {list.length === 0
        ? <div className="muted small">{empty}</div>
        : <ul className="sg-list mono small">{list.map((w, i) => <li key={i}>{w}</li>)}</ul>}
    </div>
  );
}

// ObjectGroups is the security groups section of an object's Config:
// its groups, what may reach it and what it reaches, by the rules of
// spec (the deployment its definition is read from). An attached service
// has none of its own: it is in its sandbox's.
export function ObjectGroups({ tenant, env, spec, kind, so, navigate }) {
  const head = <div className="sect">security groups</div>;
  const sandbox = attachedTo(kind, so);
  if (sandbox) {
    return (
      <>
        {head}
        <p className="small">An attached service is in the groups of its sandbox{' '}
          <EnvLink to={sandboxPath(tenant, sandbox)} navigate={navigate} className="mono">{sandbox}</EnvLink>.</p>
      </>
    );
  }
  const groups = objectGroups(so);
  const reach = objectReach(spec, groups);
  const words = Object.fromEntries(groups.map((g) => [g, groupWords(groupOf(spec, g) || {})]));
  const link = envPath(tenant, env, 'groups');
  const via = (r) => (groups.length > 1 ? <span className="muted"> · {r.via}</span> : null);
  return (
    <>
      {head}
      <div className="sg-objhead">
        <GroupChips groups={groups} words={words} />
        <EnvLink to={link} navigate={navigate} className="small">all groups of the environment</EnvLink>
      </div>
      <div className="sg-cols">
        <RuleList title="reached from" empty="nothing: no pod reaches it (routes from the ingress need no rule)"
          list={reach.inbound.map((r) => <>{r.words}{via(r)}</>)} />
        <RuleList title="reaches in the environment" empty="no group lets it in"
          list={reach.inside.map((r) => <>{r.words}{via(r)}</>)} />
        <RuleList title="reaches outside" empty="nothing outside the environment"
          list={reach.outside.map((r) => <>{r.words}{via(r)}</>)} />
      </div>
    </>
  );
}
