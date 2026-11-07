// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { envPath } from '../routes.js';
import { EnvLink } from './EnvTabs.jsx';
import { DEFAULT_GROUP, DEFAULT_WORDS, GROUP_LIMITS, accessWords, groupChoices, ingressWords, toggleGroup } from './groups.js';
import '../styles/groups.css';

// The widgets of security groups (ADR 0034, ADR 0035): the flow picture
// of a group, an object or a sandbox; the choice of an environment and of
// its groups for a sandbox or a tenant's job; a pod's groups as chips.
// The groups to choose from are those of the environment's deployed
// deployment, "default" always.

// useEnvGroups reads the groups env (its name or ID) offers: {choices:
// [{name, words}], spec, deployed, err}; choices null while unread.
// Without a deployment, or when the deployment cannot be read, only
// "default".
export function useEnvGroups(env) {
  const [state, setState] = useState({ env: '', choices: null, spec: null, deployed: 0, err: '' });
  useEffect(() => {
    if (!env) return undefined;
    let live = true;
    (async () => {
      const st = await api.getEnvironment(env);
      const n = Number(st.deployed) || 0;
      const spec = n > 0 ? (await api.getDeployment(st.name || env, n)).spec || {} : {};
      if (live) setState({ env, choices: groupChoices(spec), spec, deployed: n, err: '' });
    })().catch((e) => { if (live) setState({ env, choices: groupChoices({}), spec: {}, deployed: 0, err: e.message }); });
    return () => { live = false; };
  }, [env]);
  return state.env === env && env ? state : { env, choices: null, spec: null, deployed: 0, err: '' };
}

const PEER_ICONS = { group: 'shield', self: 'refresh', internet: 'globe', cidr: 'network', ingress: 'route', other: 'alert' };

// GroupFlow is the picture of pods' security: who may connect to them on
// the left, the pods in the middle (children), what they may reach on the
// right, each row a peer with an arrow and what it allows. ins and outs are
// flowsOf's rows; groupLink(name, pill) may wrap a group's pill in a link.
export function GroupFlow({ ins, outs, children, groupLink, heads = true }) {
  return (
    <div className={'sgf' + (heads ? '' : ' sgf-noheads')}>
      {heads && <FlowHeads />}
      <div className="sgf-side sgf-in">
        {ins.length > 0
          ? ins.map((r) => <FlowRow key={r.key} row={r} dir="in" groupLink={groupLink} />)
          : <span className="sgf-closed"><Icon name="lock" size={13} /> Nothing gets in</span>}
      </div>
      <div className="sgf-box">{children}</div>
      <div className="sgf-side sgf-out">
        {outs.length > 0
          ? outs.map((r) => <FlowRow key={r.key} row={r} dir="out" groupLink={groupLink} />)
          : <span className="sgf-closed"><Icon name="lock" size={13} /> Reaches nothing</span>}
      </div>
    </div>
  );
}

const KIND_ICONS = { service: 'box', addon: 'layers', job: 'jobs', 'job run': 'play', sandbox: 'terminal' };
const SHOWN = 6;

// kindIcon is the icon of an object's or a pod's kind.
export function kindIcon(kind) {
  return KIND_ICONS[kind] || 'box';
}

// MemberList is the middle of a group's picture: how many pods are in it
// and their names (links where a member has to), six and then "+n more".
// members null: not read yet.
export function MemberList({ members, navigate, partial = false }) {
  const [all, setAll] = useState(false);
  const list = members || [];
  const shown = all ? list : list.slice(0, SHOWN);
  const count = `${list.length} ${list.length === 1 ? 'member' : 'members'}${partial ? '…' : ''}`;
  return (
    <>
      <div className="sgf-boxhead">{count}</div>
      {list.length === 0 && !partial && <div className="muted small">No pod is in it.</div>}
      <ul className="sgf-members">
        {shown.map((m) => {
          const inner = <><Icon name={kindIcon(m.kind)} size={12} /> <span className="mono">{m.name}</span></>;
          return (
            <li key={m.key} title={`${m.kind} ${m.name}`}>
              {m.to ? <EnvLink to={m.to} navigate={navigate}>{inner}</EnvLink> : <span className="sgf-member">{inner}</span>}
            </li>);
        })}
      </ul>
      {list.length > SHOWN && (
        <button className="linkbtn" onClick={() => setAll(!all)}>{all ? 'fewer' : `+${list.length - SHOWN} more`}</button>)}
    </>
  );
}

// FlowHeads name the picture's sides; above a list of pictures once, as
// FlowLegend.
function FlowHeads({ mid = '' }) {
  return (
    <>
      <div className="sgf-head sgf-hin"><Icon name="arrowRight" size={12} /> Allowed in</div>
      {mid && <div className="sgf-head sgf-hmid">{mid}</div>}
      <div className="sgf-head sgf-hout">Allowed out <Icon name="arrowRight" size={12} /></div>
    </>
  );
}

// FlowLegend heads a list of GroupFlow cards, in their columns.
export function FlowLegend({ mid }) {
  return <div className="sgf sgf-legend" aria-hidden="true"><FlowHeads mid={mid} /></div>;
}

// rowTitle says a row in full, for its tooltip.
function rowTitle(row, dir) {
  const p = row.peer;
  if (p.type === 'ingress') return `HTTP from the ingress, by routes, needs no rule: ${row.paths.join(', ')}`;
  const who = p.type === 'self' ? 'its members, to each other'
    : p.type === 'group' ? `group ${p.name}` : p.type === 'internet' ? 'the internet'
      : `${p.name}${p.except && p.except.length ? ` except ${p.except.join(', ')}` : ''}`;
  const what = accessWords(row.access);
  const via = row.via.length ? ` (by ${row.via.length > 1 ? 'groups' : 'group'} ${row.via.join(', ')})` : '';
  const def = row.byDefault ? ' — the default without out rules' : '';
  return dir === 'in' ? `${what} from ${who}${via}` : `${what} to ${who}${via}${def}`;
}

function FlowRow({ row, dir, groupLink }) {
  const p = row.peer;
  const label = p.type === 'self' ? 'each other' : p.name;
  let pill = (
    <span className={`sgf-pill sgf-${p.type}`}>
      <Icon name={PEER_ICONS[p.type] || 'alert'} size={12} />
      <span className="sgf-name">{label}</span>
      {p.except && p.except.length > 0 && <span className="sgf-except">except {p.except.join(', ')}</span>}
    </span>
  );
  if (p.type === 'group' && groupLink) pill = groupLink(p.name, pill);
  const what = p.type === 'ingress' ? ingressWords(row.paths) : accessWords(row.access);
  const wire = <span className="sgf-wire"><span className="sgf-what">{what}</span></span>;
  return (
    <div className={`sgf-row sgf-row-${dir}`} title={rowTitle(row, dir)}>
      {dir === 'in' ? <>{pill}{wire}</> : <>{wire}{pill}</>}
    </div>
  );
}

// GroupPicker ticks one to five groups among choices ([{name, words}]):
// the last one cannot be unticked, a sixth not ticked. A name of value
// that the choices lack shows, marked, so that it can be unticked.
export function GroupPicker({ choices, value, onChange, disabled = false }) {
  const list = Array.isArray(value) ? value : [];
  const names = (choices || []).map((c) => c.name);
  const missing = list.filter((n) => !names.includes(n));
  const full = list.length >= GROUP_LIMITS.perObject;
  const row = (name, words, bad = false) => {
    const on = list.includes(name);
    const lock = disabled || (on && list.length === 1) || (!on && full);
    return (
      <label key={name} className={'sg-choice' + (on ? ' on' : '') + (bad ? ' bad' : '')}
        title={on && list.length === 1 ? 'a pod is in one group at least' : !on && full ? `at most ${GROUP_LIMITS.perObject} groups` : undefined}>
        <input type="checkbox" checked={on} disabled={lock}
          onChange={(e) => onChange(toggleGroup(list, name, e.target.checked, names))} />
        <span className="mono sg-name">{name}</span>
        <span className="muted small sg-words">{words}</span>
      </label>
    );
  };
  return (
    <div className="sg-pick" role="group" aria-label="security groups">
      {(choices || []).map((c) => row(c.name, c.words))}
      {missing.map((n) => row(n, 'not a group of this environment', true))}
      {!choices && <span className="muted small">reading the environment's groups…</span>}
    </div>
  );
}

// GroupChips are a pod's groups, "default" with what it means when it
// has no rules of its own.
export function GroupChips({ groups, words = {} }) {
  const list = Array.isArray(groups) && groups.length ? groups : [DEFAULT_GROUP];
  return (
    <span className="sg-chips">
      {list.map((g) => (
        <span key={g} className="sg-chip mono" title={words[g] || (g === DEFAULT_GROUP ? `${DEFAULT_GROUP}: ${DEFAULT_WORDS}` : undefined)}>{g}</span>))}
    </span>
  );
}

// EnvLinkName is the environment a sandbox or a job is in, as a link to
// its page.
export function EnvLinkName({ tenant, env, navigate }) {
  if (!env) return <span className="muted">—</span>;
  return <EnvLink to={envPath(tenant, env.name || env.id)} navigate={navigate} className="mono">{env.name || env.id}</EnvLink>;
}

// EnvPicker chooses one of envs (those the person may write), by name;
// none says whether the tenant has no environment at all.
export function EnvPicker({ envs, value, onChange, disabled = false, none = false }) {
  const list = envs || [];
  const empty = none ? 'no environment yet' : 'no environment you may write';
  return (
    <select value={value} onChange={(e) => onChange(e.target.value)} disabled={disabled || list.length === 0} aria-label="environment">
      <option value="">{envs === null ? 'reading the environments…' : list.length ? '(an environment)' : empty}</option>
      {list.map((e) => <option key={e.id || e.name} value={e.name}>{e.name}</option>)}
    </select>
  );
}

// NoEnvironment says why there is no environment to make a sandbox or a
// job in (what): the tenant has none yet (none), which a tenant writer
// (make) makes first, or the person writes none of those there are.
export function NoEnvironment({ tenant, navigate, none, make, what }) {
  if (!none) return <p className="error">Your role does not allow this: it needs write on an environment.</p>;
  const to = `/tenants/${encodeURIComponent(tenant)}/new-environment`;
  return (
    <p className="sg-noenv">
      This tenant has no environment yet, and a {what} runs in one.{' '}
      {make
        ? <EnvLink to={to} navigate={navigate}>Make an environment first.</EnvLink>
        : 'Someone with write on the tenant makes one first.'}
    </p>
  );
}
