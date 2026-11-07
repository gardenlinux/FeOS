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
  DEFAULT_GROUP, RULES_WORDS, attachedTo, flowsOf, groupMembers, groupNames, ingressRow, objectGroups, podGroups,
} from './groups.js';
import { onItsWay } from './jobs.js';
import { FlowLegend, GroupChips, GroupFlow, MemberList, kindIcon } from './SecurityGroups.jsx';
import '../styles/groups.css';

// The security groups of an environment, read (ADR 0034, ADR 0035): its
// Security groups view, a card per group with its picture (who may
// connect, its members, what they reach), and an object's groups on its
// Config view in the same picture. Groups change in New deployment.

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
      <div className="sg-top">
        <div className="sg-lede">
          <Icon name="lock" size={15} />
          <div>
            <p>{RULES_WORDS}</p>
            <p className="muted small">
              {n > 0
                ? <>As <EnvLink to={envPath(tenant, env, 'deployments', n)} navigate={navigate}>deployment {n}</EnvLink> has them.</>
                : <>No deployment yet: every pod is in <span className="mono">default</span>.</>}
              {latest > n && n > 0 && <span className="sg-pending"> Deployment {latest} is not deployed; its groups count once it is.</span>}
            </p>
          </div>
        </div>
        {ec.write && (
          <EnvLink to={builder('')} navigate={navigate} className="button"><Icon name="settings" size={13} /> Edit groups</EnvLink>)}
      </div>
      <ErrBox error={err || pods.err} />
      {!spec && !err && <p className="empty">Loading…</p>}
      {spec && <FlowLegend mid="Group and its members" />}
      {spec && names.map((name) => (
        <GroupCard key={name} tenant={tenant} env={env} spec={spec} name={name} pods={pods}
          edit={ec.write ? builder(name) : ''} navigate={navigate} />))}
    </div>
  );
}

// members are the pods of group name: the objects of spec, the
// environment's sandboxes and its running jobs, each {key, kind, name,
// to}.
function members(tenant, env, spec, name, pods) {
  return [
    ...groupMembers(spec, name).map((o) => ({ key: `${o.kind}/${o.name}`, kind: o.kind, name: o.name,
      to: objectPath(tenant, env, `${o.kind}/${o.name}`, 'config') })),
    ...(pods.sandboxes || []).filter((p) => podGroups(p).includes(name)).map((p) => ({ key: p.sandboxId, kind: 'sandbox',
      name: p.sandboxId, to: sandboxPath(tenant, p.sandboxId) })),
    ...(pods.jobs || []).filter((p) => podGroups(p).includes(name)).map((p) => ({ key: p.id, kind: 'job run',
      name: shortId(p.id), to: jobPath(tenant, p.id) })),
  ];
}

// GroupCard is one group: its name, and its picture with its members in
// the middle.
function GroupCard({ tenant, env, spec, name, pods, edit, navigate }) {
  const list = members(tenant, env, spec, name, pods);
  const { ins, outs } = flowsOf(spec, [name], [name]);
  const ing = ingressRow(spec, list.filter((m) => m.kind === 'service').map((m) => m.name));
  const link = (g, pill) => <a href={`#group-${g}`} className="sgf-link">{pill}</a>;
  return (
    <section className={'card sg-card' + (list.length === 0 ? ' sg-empty' : '')} id={`group-${name}`} aria-label={`security group ${name}`}>
      <div className="sg-cardhead">
        <Icon name="shield" size={15} />
        <h2 className="mono">{name}</h2>
        <span className="muted small">
          {name === DEFAULT_GROUP ? 'every pod that names no group' : ''}
        </span>
        {edit && <EnvLink to={edit} navigate={navigate} className="sg-edit small">Edit</EnvLink>}
      </div>
      <GroupFlow ins={ing ? [ing, ...ins] : ins} outs={outs} groupLink={link} heads={false}>
        <MemberList members={list} navigate={navigate} partial={!pods.sandboxes} />
      </GroupFlow>
    </section>
  );
}

// ObjectGroups is the security groups section of an object's Config: its
// groups, and the picture of who may connect to it and what it reaches,
// by the rules of spec (the deployment its definition is read from). An
// attached service has none of its own: it is in its sandbox's.
export function ObjectGroups({ tenant, env, spec, kind, name, so, navigate }) {
  const sandbox = attachedTo(kind, so);
  if (sandbox) {
    return (
      <>
        <div className="sect">security groups</div>
        <p className="small">Runs in sandbox{' '}
          <EnvLink to={sandboxPath(tenant, sandbox)} navigate={navigate} className="mono">{sandbox}</EnvLink>, in its groups.</p>
      </>
    );
  }
  const groups = objectGroups(so);
  const { ins, outs } = flowsOf(spec, groups);
  const ing = kind === 'service' ? ingressRow(spec, [name]) : null;
  const all = envPath(tenant, env, 'groups');
  const link = (g, pill) => <EnvLink to={all} navigate={navigate} className="sgf-link">{pill}</EnvLink>;
  return (
    <>
      <div className="sect sg-objsect">
        Security groups
        <GroupChips groups={groups} />
        <EnvLink to={all} navigate={navigate} className="small sg-all">All groups</EnvLink>
      </div>
      <GroupFlow ins={ing ? [ing, ...ins] : ins} outs={outs} groupLink={link}>
        <div className="sgf-self">
          <Icon name={kindIcon(kind)} size={14} />
          <span className="mono">{name || kind}</span>
        </div>
      </GroupFlow>
    </>
  );
}
