// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { envPath } from '../routes.js';
import { EnvLink } from './EnvTabs.jsx';
import {
  blockWords, egressMode, egressParts, egressProblems, egressWords, reachChoices, reachKey, reachesWords,
  toggleReach, useWords, withEgressList, withEgressMode,
} from './reach.js';
import '../styles/reach.css';

// The widgets of reachability (ADR 0034, revised; ADR 0035): the picture
// of who reaches a pod and what it reaches, the switch of an egress, the
// choice of what a sandbox or a tenant's job reaches, and the choice of
// its environment.

// useDeployed reads env's (its name or ID) deployed deployment: {n, spec,
// reach, err}; spec null while unread, {} without a deployment; reach
// the answer's reachability, null when ferund answers none.
export function useDeployed(env) {
  const [state, setState] = useState({ env: '', n: 0, spec: null, reach: null, err: '' });
  useEffect(() => {
    if (!env) return undefined;
    let live = true;
    (async () => {
      const st = await api.getEnvironment(env);
      const n = Number(st.deployed) || 0;
      const d = n > 0 ? await api.getDeployment(st.name || env, n) : {};
      if (live) setState({ env, n, spec: d.spec || {}, reach: d.reachability || null, err: '' });
    })().catch((e) => { if (live) setState({ env, n: 0, spec: {}, reach: null, err: e.message }); });
    return () => { live = false; };
  }, [env]);
  return state.env === env && env ? state : { env, n: 0, spec: null, reach: null, err: '' };
}

const PEER_ICONS = {
  service: 'box', job: 'jobs', addon: 'layers', ingress: 'route', sandbox: 'terminal', jobrun: 'play', env: 'layers',
  internet: 'globe', domain: 'globe', cidr: 'network', other: 'alert',
};
const KIND_ICONS = { service: 'box', addon: 'layers', job: 'jobs', jobrun: 'play', sandbox: 'terminal' };

// kindIcon is the icon of an object's or a pod's kind.
export function kindIcon(kind) {
  return KIND_ICONS[kind] || 'box';
}

// ReachFlow is the picture of a pod: who reaches it on the left, the pod
// in the middle (children), what it reaches on the right, each row a peer
// with an arrow and its ports. ins and outs are reachModel's rows; link(peer,
// pill) may wrap a pill in a link. closedOut says, when nothing is on the
// right, why: its egress is none.
export function ReachFlow({ ins, outs, children, link, heads = true, egress, inWords = 'Nothing gets in' }) {
  const none = egress !== undefined && egressMode(egress) !== 'internet' && !egressParts(egress).domains.length
    && !egressParts(egress).blocks.length;
  return (
    <div className={'sgf' + (heads ? '' : ' sgf-noheads')}>
      {heads && <FlowHeads />}
      <div className="sgf-side sgf-in">
        {ins.length > 0
          ? ins.map((r) => <FlowRow key={r.key} row={r} dir="in" link={link} />)
          : <span className="sgf-closed"><Icon name="lock" size={13} /> {inWords}</span>}
      </div>
      <div className="sgf-box">{children}</div>
      <div className="sgf-side sgf-out">
        {outs.map((r) => <FlowRow key={r.key} row={r} dir="out" link={link} />)}
        {outs.length === 0 && !none && <span className="sgf-closed"><Icon name="lock" size={13} /> Reaches nothing</span>}
        {none && <span className="sgf-closed"><Icon name="lock" size={13} /> Nothing outside the environment</span>}
      </div>
    </div>
  );
}

// FlowHeads name the picture's sides; above a list of pictures once, as
// FlowLegend.
function FlowHeads({ mid = '' }) {
  return (
    <>
      <div className="sgf-head sgf-hin"><Icon name="arrowRight" size={12} /> Reached by</div>
      {mid && <div className="sgf-head sgf-hmid">{mid}</div>}
      <div className="sgf-head sgf-hout">Reaches <Icon name="arrowRight" size={12} /></div>
    </>
  );
}

// FlowLegend heads a list of ReachFlow cards, in their columns.
export function FlowLegend({ mid }) {
  return <div className="sgf sgf-legend" aria-hidden="true"><FlowHeads mid={mid} /></div>;
}

// rowTitle says a row in full, for its tooltip.
function rowTitle(row, dir) {
  const p = row.peer;
  const via = row.via.length ? ` (by ${row.via.join(', ')})` : '';
  if (p.type === 'ingress') return `${row.what} from the ingress, for ${p.sub}${via}`;
  if (p.missing) return `${p.name}: not in the deployed deployment, admits nothing`;
  const who = p.type === 'env' ? 'every pod of the environment' : p.type === 'internet' ? 'the internet'
    : p.type === 'domain' ? `${p.name}, by its DNS answers` : p.type === 'jobrun' ? `job ${p.name}` : `${p.type} ${p.name}`;
  return dir === 'in' ? `${row.what} from ${who}${via}` : `${row.what} to ${who}${via}`;
}

function FlowRow({ row, dir, link }) {
  const p = row.peer;
  let pill = (
    <span className={`sgf-pill sgf-${p.type}${p.missing ? ' sgf-missing' : ''}`}>
      <Icon name={PEER_ICONS[p.type] || 'alert'} size={12} />
      <span className="sgf-name">{p.name}</span>
      {p.sub && <span className="sgf-except">{p.sub}</span>}
    </span>
  );
  if (link) pill = link(p, pill) || pill;
  const wire = <span className="sgf-wire"><span className="sgf-what">{row.what}</span></span>;
  return (
    <div className={`sgf-row sgf-row-${dir}`} title={rowTitle(row, dir)}>
      {dir === 'in' ? <>{pill}{wire}</> : <>{wire}{pill}</>}
    </div>
  );
}

// ReachesWords are what a sandbox or a job reaches, in a few words.
export function ReachesWords({ reaches }) {
  const r = reaches === 'all' ? 'all' : Array.isArray(reaches) ? reaches : [];
  const words = reachesWords(r);
  return (
    <span className={'rc-reaches' + (r === 'all' ? ' rc-all' : r.length ? '' : ' muted')}
      title={Array.isArray(r) && r.length > 3 ? r.map(useWords).join(', ') : undefined}>
      {r === 'all' ? <Icon name="layers" size={11} /> : r.length ? <Icon name="arrowRight" size={11} /> : <Icon name="lock" size={11} />}
      {' '}{words}
    </span>
  );
}

// Segmented is a switch of a few choices, [value, label] each.
export function Segmented({ value, options, onChange, label, disabled = false }) {
  return (
    <div className="sg-seg" role="group" aria-label={label}>
      {options.map(([v, l]) => (
        <button key={v} type="button" className={value === v ? 'on' : ''} aria-pressed={value === v} disabled={disabled}
          onClick={() => { if (v !== value) onChange(v); }}>{l}</button>))}
    </div>
  );
}

// EgressEditor edits an egress (a service's, a job's, a sandbox's): a
// switch internet / only these / nothing, and for only these a list of
// domains and a list of address blocks with exceptions. value is the
// spec's form; onChange gets the next (undefined: the internet).
export function EgressEditor({ value, onChange, disabled = false, who = 'it' }) {
  const mode = egressMode(value);
  const { domains, blocks } = egressParts(value);
  const problems = egressProblems(value);
  return (
    <div className="rc-egress">
      <Segmented label="egress" value={mode === 'other' ? '' : mode} disabled={disabled}
        options={[['internet', 'The internet'], ['only', 'Only these'], ['none', 'Nothing']]}
        onChange={(m) => onChange(withEgressMode(value, m))} />
      {mode === 'internet' && <p className="sg-note">{cap(who)} reaches every address on the internet. That is the default.</p>}
      {mode === 'none' && <p className="sg-note">{cap(who)} reaches nothing outside the environment; the environment&apos;s pods it references, still.</p>}
      {mode === 'other' && <p className="sg-note bad">{egressWords(value)}: edit it in the JSON view.</p>}
      {mode === 'only' && (
        <div className="rc-only">
          <label className="rc-sub">
            <span className="b-label">Domains</span>
            <ListText value={domains} disabled={disabled} placeholder={'api.stripe.com\n*.githubusercontent.com'} aria="domains"
              onChange={(l) => onChange(withEgressList(value, 'domains', l))} />
            <span className="muted small">one per line; a name its resolver answers, its addresses then reachable. A wildcard only as &quot;*.&quot; in front.</span>
          </label>
          <div className="rc-sub">
            <span className="b-label">Address blocks</span>
            <div className="sg-rules">
              {blocks.map((b, i) => (
                <div key={i} className="sg-rule rc-block">
                  <div className="sg-cidrrow">
                    <CidrInput peer={b} disabled={disabled}
                      onChange={(nb) => onChange(withEgressList(value, 'blocks', blocks.map((x, j) => (j === i ? nb : x))))} />
                  </div>
                  <button type="button" className="iconbtn" title="remove the block" disabled={disabled}
                    onClick={() => onChange(withEgressList(value, 'blocks', blocks.filter((_, j) => j !== i)))}><Icon name="x" size={13} /></button>
                </div>))}
              <span><button type="button" className="ghost mini" disabled={disabled}
                onClick={() => onChange(withEgressList(value, 'blocks', [...blocks, { cidr: '' }]))}>
                <Icon name="plus" size={12} /> Add a block</button></span>
            </div>
            <span className="muted small">any protocol and port; the private ranges are refused.</span>
          </div>
          {!domains.length && !blocks.length && <p className="sg-note"><Icon name="lock" size={12} /> Nothing yet: {who} reaches nothing outside.</p>}
        </div>)}
      {problems.length > 0 && <p className="warntext small">{problems.join('; ')}</p>}
    </div>
  );
}

function cap(s) {
  return s ? s[0].toUpperCase() + s.slice(1) : s;
}

// ListText edits a list of strings, one per line; empty lines go.
export function ListText({ value, onChange, placeholder, disabled = false, aria, rows = 2 }) {
  const list = Array.isArray(value) ? value : [];
  const [text, setText] = useState(list.join('\n'));
  useEffect(() => {
    if (list.join('\n') !== text.split('\n').map((l) => l.trim()).filter(Boolean).join('\n')) setText(list.join('\n'));
  }, [list.join('\n')]); // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <textarea className="mono b-lines" aria-label={aria} disabled={disabled} placeholder={placeholder}
      rows={Math.max(rows, Math.min(8, text.split('\n').length))} value={text}
      onChange={(e) => {
        setText(e.target.value);
        onChange(e.target.value.split('\n').map((l) => l.trim()).filter(Boolean));
      }} />
  );
}

// CidrInput edits an address block and its exceptions (comma separated).
export function CidrInput({ peer, onChange, disabled = false }) {
  const except = Array.isArray(peer.except) ? peer.except : [];
  const [text, setText] = useState(except.join(', '));
  useEffect(() => {
    if (except.join(', ') !== text.split(/[,\s]+/).filter(Boolean).join(', ')) setText(except.join(', '));
  }, [except.join(',')]); // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <>
      <input className="mono sg-cidr" placeholder="203.0.113.0/24" aria-label="address block" value={peer.cidr || ''} disabled={disabled}
        onChange={(e) => onChange({ ...peer, cidr: e.target.value.trim() })} />
      <span className="muted small">except</span>
      <input className="mono sg-except" placeholder="(optional) 203.0.113.8/29, …" aria-label="except" value={text} disabled={disabled}
        onChange={(e) => {
          setText(e.target.value);
          const list = e.target.value.split(/[,\s]+/).filter(Boolean);
          const next = { ...peer };
          if (list.length) next.except = list; else delete next.except;
          onChange(next);
        }} />
    </>
  );
}

// ReachesPicker chooses what a sandbox or a job reaches of its
// environment: nothing, all of it, or some of its deployed deployment's
// service ports and addons (spec; null while unread). mode and list are
// the form's; onChange(mode, list). Entries of list the deployment lacks
// show, marked, so that they can be unticked.
export function ReachesPicker({ spec, mode, list, onChange, disabled = false, err = '' }) {
  const choices = spec ? reachChoices(spec) : null;
  const l = Array.isArray(list) ? list : [];
  const missing = choices ? l.filter((e) => !choices.some((c) => c.key === reachKey(e))) : [];
  return (
    <div className="rc-pick">
      <Segmented label="reaches" value={mode} disabled={disabled}
        options={[['none', 'Nothing'], ['some', 'Only these'], ['all', 'All of it']]}
        onChange={(m) => onChange(m, l)} />
      {mode === 'none' && <p className="sg-note"><Icon name="lock" size={12} /> Alone on the network: usable by its token, it reaches no pod of the environment.</p>}
      {mode === 'all' && <p className="sg-note">Every port of every pod of the environment, as a workspace needs.</p>}
      {mode === 'some' && (
        <div className="sg-pick" role="group" aria-label="what it reaches">
          {choices && choices.length === 0 && <span className="muted small">The deployed deployment has no service port and no addon.</span>}
          {(choices || []).map((c) => {
            const on = l.some((e) => reachKey(e) === c.key);
            return (
              <label key={c.key} className={'sg-choice' + (on ? ' on' : '')}>
                <input type="checkbox" checked={on} disabled={disabled}
                  onChange={(e) => onChange(mode, toggleReach(l, c.value, e.target.checked, choices))} />
                <Icon name={kindIcon(c.kind)} size={12} />
                <span className="mono sg-name">{c.name}</span>
                <span className="muted small sg-words">{c.detail}</span>
              </label>);
          })}
          {missing.map((e) => (
            <label key={reachKey(e)} className="sg-choice on bad">
              <input type="checkbox" checked disabled={disabled} onChange={() => onChange(mode, l.filter((x) => reachKey(x) !== reachKey(e)))} />
              <span className="mono sg-name">{useWords(e)}</span>
              <span className="small sg-words">not in the deployed deployment</span>
            </label>))}
          {!choices && !err && <span className="muted small">reading the environment&apos;s deployment…</span>}
        </div>)}
      {err && <span className="warntext small">The deployment cannot be read ({err}).</span>}
    </div>
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

// EgressWords say an egress, with its blocks' exceptions on hover.
export function EgressWords({ egress }) {
  const { blocks } = egressParts(egress);
  return <span title={blocks.length ? blocks.map(blockWords).join('; ') : undefined}>{egressWords(egress)}</span>;
}

// ReachesSummary is what a sandbox or job reaches and its egress, in a
// line.
export function ReachesSummary({ pod }) {
  return (
    <span className="rc-summary">
      <ReachesWords reaches={pod && pod.reaches} />
      <span className="muted"> · out: <EgressWords egress={pod && pod.egress !== undefined ? pod.egress : 'internet'} /></span>
    </span>
  );
}
