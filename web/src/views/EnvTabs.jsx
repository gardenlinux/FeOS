// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, Secret, useArm, useModal, usePoll } from '../components.jsx';
import { fmtDate, fmtDurNs, fmtNs } from '../format.js';
import { envPath, jobPath, sandboxPath } from '../routes.js';
import { useSummary } from '../summary.jsx';
import { inUse } from './reach.js';
import {
  UNGUARDED, actionProblem, admitsOf, cloneBody, defaultHost, envDomains, errorShare, fmtLatency, newestFirst, olderCursor,
  publishNote, routeCurl, sparkPoints, statusTone,
} from './envs.js';
import { choices, guardsNone, hostLabelChoice, kindLabel, NO_VISITS, providerWords, publishChoice, sameChoice } from './providers.js';
import { useMe } from '../people.js';
import '../styles/environments.css';
import '../styles/credentials.css';
import '../styles/providers.css';

// An environment's pages beside its objects and deployments: its traffic
// (the ingress's answers per service and its access log), its secrets'
// widgets (EnvSecrets.jsx holds the view), its domains and access
// (public, who it admits, hosts, domains, route tokens) and its
// settings (clone, delete), with the small widgets the
// environment's actions share. ec is what the role may do on the environment
// (Environments.jsx, envCan); every write calls changed, which refreshes
// the page and the project's summary.

// ---------------------------------------------------------------- widgets --

// EnvLink is a link within the UI: a real href, the router on a plain
// click.
export function EnvLink({ to, navigate, className, title, children }) {
  return (
    <a href={to} className={className} title={title} onClick={(e) => {
      if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      e.stopPropagation();
      navigate(to);
    }}>{children}</a>
  );
}

// HostLink is a host of the environment: teal, mono, a link to it.
export function HostLink({ host }) {
  return <a className="mono host" href={`https://${host}`} target="_blank" rel="noreferrer">{host}</a>;
}

// useOp runs one write at a time and keeps what came of it: {busy},
// then {text, tone ok|bad, result | error}.
export function useOp() {
  const [op, setOp] = useState(null);
  const run = useCallback(async (fn, done) => {
    setOp({ busy: true });
    try {
      const result = await fn();
      setOp({ text: typeof done === 'function' ? done(result) : (done || ''), tone: 'ok', result });
      return result;
    } catch (e) {
      setOp({ text: actionProblem(e), tone: 'bad', error: e });
      return undefined;
    }
  }, []);
  const clear = useCallback(() => setOp(null), []);
  return [op, run, clear];
}

export function OpNote({ op, onDismiss }) {
  if (!op || op.busy || !op.text) return null;
  return (
    <div className={'envop-note ' + op.tone}>
      <span>{op.text}</span>
      {onDismiss && <button className="iconbtn mini" title="dismiss" onClick={onDismiss}><Icon name="x" size={12} /></button>}
    </div>
  );
}

// ConfirmButton asks twice, as ArmButton does, for an action that is no
// deletion: a revert, an unpublish's opposite. It is never a form's
// submit button: its first click only arms it.
export function ConfirmButton({ label, confirmLabel, onConfirm, disabled, title, className = '', icon }) {
  const [armed, arm, disarm] = useArm();
  const on = armed === 'go';
  return (
    <button type="button" className={className + ' confirm' + (on ? ' armed' : '')} disabled={disabled} title={title}
      onClick={(e) => {
        e.stopPropagation();
        if (!on) { arm('go'); return; }
        disarm();
        onConfirm();
      }}>
      {icon && <Icon name={icon} size={13} />}
      {on ? (confirmLabel || `${label}: sure?`) : label}
    </button>
  );
}

// NEEDS are the reasons a disabled button gives.
export const NEEDS = {
  write: 'needs write on this environment',
  admin: 'needs admin on this environment',
  del: 'needs admin on this environment or write on the project',
  clone: 'needs write on the project',
  // A protected environment (ADR 0060): a project grant of write reads it.
  protected: 'protected: needs a grant on this environment, or admin on the project',
  delProtected: 'protected: needs admin on this environment',
};

// needsWrite is why a write is not offered on the environment of ec: its
// protection, or the role.
export function needsWrite(ec) {
  return ec && ec.protected && !ec.write ? NEEDS.protected : NEEDS.write;
}

// needsDel is why a delete or a rename is not offered.
export function needsDel(ec) {
  return ec && ec.protected ? NEEDS.delProtected : NEEDS.del;
}

// --------------------------------------------------------------- traffic --

const WINDOWS = ['1h', '24h'];

// Spark is a service's requests over the window, its errors over them in
// red, as an inline SVG.
export function Spark({ series, w = 140, h = 26 }) {
  const req = sparkPoints(series, 'requests', w, h);
  const maxOf = (list, key) => Math.max(...list.map((p) => Number(p[key]) || 0));
  if (!req) return <span className="muted">—</span>;
  const hasErr = (series || []).some((p) => p.errors > 0);
  return (
    <svg className="spark" width={w} height={h} viewBox={`0 0 ${w} ${h}`} role="img" aria-label="requests over the window">
      <polyline className="spark-req" points={req} />
      {hasErr && <polyline className="spark-err" points={sparkPoints(series, 'errors', w, h, maxOf(series, 'requests'))} />}
    </svg>
  );
}

export function TrafficTable({ traffic, service = '' }) {
  const list = ((traffic && traffic.services) || []).filter((s) => !service || s.service === service);
  return (
    <>
      <div className="tblwrap">
        <table className="tbl traffic-tbl">
          <thead>
            <tr>
              <th>service</th>
              <th className="col-num">requests</th>
              <th className="col-num">errors</th>
              <th className="col-num">p50</th>
              <th className="col-num">p95</th>
              <th>over the window</th>
              <th>counted from</th>
            </tr>
          </thead>
          <tbody>
            {list.map((s) => (
              <tr key={s.service}>
                <td className="mono strong">{s.service}</td>
                <td className="col-num mono">{s.requests}</td>
                <td className={'col-num mono' + (s.errors > 0 ? ' bad' : '')} title={`${s.errors} with a status of 500 or more, or none`}>
                  {s.errors} <span className="muted small">{errorShare(s)}</span></td>
                <td className="col-num mono">{fmtLatency(s.p50Ms)}</td>
                <td className="col-num mono">{fmtLatency(s.p95Ms)}</td>
                <td><Spark series={s.series} /></td>
                <td className="small muted">{s.source === 'ingress' ? 'the ingress' : 'its spans'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {traffic && list.length === 0 && <p className="empty">No requests in this window.</p>}
    </>
  );
}

// TrafficTab is an environment's traffic, or on a service's page
// (service, its name) the service's: its line of the table and the
// access log's records of it.
export function TrafficTab({ project, env, navigate, service = '' }) {
  const [win, setWin] = useState('1h');
  const [traffic, setTraffic] = useState(null);
  const [err, setErr] = useState('');
  const refresh = useCallback(async () => {
    try {
      setTraffic(await api.traffic(env, win));
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [env, win]);
  usePoll(refresh, 30000);
  return (
    <>
      <div className="pane-toolbar">
        <h2><Icon name="chart" size={14} /> {service ? 'Requests' : 'Requests per service'}</h2>
        <span className="b-seg">
          {WINDOWS.map((w) => (
            <button key={w} className={'chipbtn' + (win === w ? ' on' : '')} onClick={() => { setWin(w); setTraffic(null); }}>{w}</button>))}
        </span>
        {traffic && traffic.since && (
          <span className="muted small">{fmtDate(traffic.since)} – {fmtDate(traffic.until)}, steps of {traffic.step}</span>)}
      </div>
      <ErrBox error={err} />
      <TrafficTable traffic={traffic} service={service} />
      {!traffic && !err && <p className="empty">Loading…</p>}
      <p className="panel-note">Counted from each service's OpenTelemetry server spans, wherever a request came from, without
        its readiness check; errors are spans with an error status or a status of 500 to 599. A routed service that sends no
        spans shows what the ingress answered at the public hosts (errors: 500 or more, or none); the route door's requests
        are not counted.</p>
      <AccessLog project={project} env={env} navigate={navigate} service={service} />
    </>
  );
}

const ACCESS_PAGE = 100;

// AccessLog is the ingress's access log of the environment, newest
// first: the newest page polled, older pages read on demand.
function AccessLog({ project, env, navigate, service = '' }) {
  const [newest, setNewest] = useState(null);
  const [older, setOlder] = useState([]);
  const [err, setErr] = useState('');
  const [more, setMore] = useState(true);
  const [busy, setBusy] = useState(false);
  const refresh = useCallback(async () => {
    try {
      setNewest((await api.accessLogs(env, { limit: ACCESS_PAGE })).records || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [env]);
  usePoll(refresh, 10000);
  const loadOlder = async () => {
    const cursor = olderCursor(older.length > 0 ? older[older.length - 1] : newest);
    if (!cursor) return;
    setBusy(true);
    try {
      const page = (await api.accessLogs(env, { limit: ACCESS_PAGE, beforeTsNs: cursor })).records || [];
      if (page.length > 0) setOlder((cur) => [...cur, page]);
      if (page.length < ACCESS_PAGE) setMore(false);
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };
  const all = newest ? newestFirst(newest, ...older) : null;
  const records = all && service ? all.filter((r) => r.service === service) : all;
  return (
    <div className="envsect">
      <div className="pane-toolbar">
        <h2><Icon name="file" size={14} /> Access log</h2>
        <span className="muted small">the ingress's records, newest first</span>
      </div>
      <ErrBox error={err} />
      <AccessTable records={records} project={project} env={env} navigate={navigate} />
      {!records && !err && <p className="empty">Loading…</p>}
      {records && records.length >= ACCESS_PAGE && more && (
        <button className="ghost mini" disabled={busy} onClick={loadOlder}>{busy ? 'Loading…' : 'Older'}</button>)}
    </div>
  );
}

export function AccessTable({ records, project, env, navigate }) {
  const list = records || [];
  return (
    <>
      <div className="tblwrap">
        <table className="tbl access-tbl">
          <thead>
            <tr>
              <th className="col-time">time</th>
              <th className="col-num">status</th>
              <th>request</th>
              <th>service</th>
              <th className="col-num">took</th>
              <th>client</th>
              <th>trace</th>
            </tr>
          </thead>
          <tbody>
            {list.map((r) => (
              <tr key={`${r.tsNs}/${r.spanId}`}>
                <td className="col-time mono" title={r.time}>{fmtNs(r.tsNs)}</td>
                <td className={'col-num mono ' + statusTone(r.status)} title={r.outcome}>{r.status || '—'}</td>
                <td className="mono small msgcell">
                  <span className="strong">{r.method}</span> <span className="host">{r.host}</span>{r.path}
                  {r.heldNs > 0 && <div className="muted">held {fmtDurNs(r.heldNs)} while the service woke</div>}
                </td>
                <td className="mono small">{r.service || <span className="muted">—</span>}</td>
                <td className="col-num mono small">{fmtDurNs(r.durationNs)}</td>
                <td className="mono small cut-start" title={r.userAgent}>{r.clientIp}</td>
                <td className="mono small">
                  {r.traceId
                    ? <EnvLink to={envPath(project, env, 'traces', r.traceId)} navigate={navigate} title="the trace">{r.traceId.slice(0, 8)}</EnvLink>
                    : <span className="muted">—</span>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {records && list.length === 0 && <p className="empty">No requests yet.</p>}
    </>
  );
}

// --------------------------------------------------------------- secrets --

// The secrets' widgets of the Secrets view (EnvSecrets.jsx): a delete, a
// new value and a new secret. A value is never shown: once stored, it
// is a version.

export function DeleteSecret({ name, disabled, onDelete }) {
  const [armed, arm, disarm] = useArm();
  return (
    <ArmButton armKey={name} armed={armed} arm={arm} disarm={disarm} className="mini" disabled={disabled}
      title={disabled ? NEEDS.write : 'delete every version; refused while the latest deployment pins it'}
      onConfirm={() => onDelete(name)}>Delete</ArmButton>
  );
}

export function SecretValue({ name, onPut, onCancel }) {
  const [value, setValue] = useState('');
  return (
    <form className="inlineform envsecret-form" onSubmit={async (e) => { e.preventDefault(); if (value && await onPut(name, value)) setValue(''); }}>
      <input type="password" autoComplete="new-password" className="mono grow" placeholder={`the new value of ${name}`}
        value={value} onChange={(e) => setValue(e.target.value)} autoFocus />
      <button className="primary mini" type="submit" disabled={!value}>Store version</button>
      <button className="ghost mini" type="button" onClick={onCancel}>Cancel</button>
      <span className="muted small">shown never again once stored</span>
    </form>
  );
}

export function NewSecret({ disabled, onPut }) {
  const [name, setName] = useState('');
  const [value, setValue] = useState('');
  return (
    <form className="inlineform envsecret-form" title={disabled ? NEEDS.write : ''} onSubmit={async (e) => {
      e.preventDefault();
      if (name && value && await onPut(name.trim(), value)) { setName(''); setValue(''); }
    }}>
      <input className="mono" placeholder="new secret's name" value={name} disabled={disabled} onChange={(e) => setName(e.target.value)} />
      <input type="password" autoComplete="new-password" className="mono grow" placeholder="its value" value={value}
        disabled={disabled} onChange={(e) => setValue(e.target.value)} />
      <button className="mini" type="submit" disabled={disabled || !name || !value}><Icon name="plus" size={12} /> Add secret</button>
    </form>
  );
}

// ------------------------------------------------------ domains & access --

export function AccessTab({ project, env, st, ec, domains, changed, navigate }) {
  if (!st) return <p className="empty">Loading…</p>;
  return (
    <>
      <PublicBox env={env} st={st} ec={ec} domains={domains} changed={changed} />
      <DomainsBox project={project} st={st} domains={domains} navigate={navigate} />
      <RouteTokenBox env={st.name} ec={ec} />
    </>
  );
}

// PublicBox publishes the environment and says who the ingress admits
// at its hosts (ADR 0054): visitors of one of the project's providers, or
// anyone. The providers and the kinds this installation serves are the
// summary's, unless given; a publish always says the access.
export function PublicBox({ env, st, ec, domains, changed, providers: given, kinds: givenKinds }) {
  const [label, setLabel] = useState('');
  const [picked, setPicked] = useState(null);
  const [op, run, clear] = useOp();
  const sum = useSummary();
  const providers = given === undefined ? sum.providers : given;
  const kinds = givenKinds === undefined ? sum.providerKinds : givenKinds;
  const dh = defaultHost(st, domains || []);
  const now = admitsOf(st);
  // current is the access a publish keeps; choice what the box asks for.
  const current = publishChoice(st, kinds);
  const choice = picked || current;
  const options = choices(providers, kinds, st.access === 'visitors' ? st.provider : '');
  const option = options.find((o) => o.name === choice.provider);
  const ready = choice.access === 'anyone' || (choice.access === 'visitors' && !!option && option.served);
  const publish = (on, what = choice, hostLabel = label.trim()) => run(() => (on
    ? api.setPublic(env, true, { hostLabel, access: what.access, provider: what.provider })
    : api.setPublic(env, false)), publishNote).then((r) => {
    if (r) { setLabel(''); setPicked(null); changed(); }
  });
  const busy = !!(op && op.busy);
  const off = !ec.admin || busy;
  // none: the installation guards no environment (no secret key).
  const none = guardsNone(kinds);
  const why = ec.admin ? '' : NEEDS.admin;
  const changing = st.public && !sameChoice(choice, current);
  const pick = (access) => setPicked(access === 'visitors'
    ? { access, provider: choice.provider || (options[0].served ? options[0].name : '') }
    : { access, provider: '' });
  return (
    <div className="envsect envbox">
      <div className="sect">public</div>
      <div className="envbox-row">
        {st.public
          ? <span className="state-label running"><span className="sdot running" />public</span>
          : <span className="state-label absent"><span className="sdot absent" />not public</span>}
        <span className="muted small">
          {st.public
            ? 'The ingress serves its routes to image services at its hosts, without a token, to whom it admits.'
            : 'Only the route door serves it, to a route token. Publishing gives it a default host.'}
        </span>
      </div>
      {st.public && (
        <div className="envbox-row">
          <span className="k">admits</span>
          {now.access === 'visitors'
            ? <span className="state-label running admits-lock"><Icon name="lock" size={12} />visitors · {now.provider}</span>
            : <span className="state-label unknown">anyone</span>}
          <span className={'small ' + (now.known ? 'muted' : 'warntext')}>
            {!now.known ? UNGUARDED : now.access === 'visitors' ? providerWords(provider(providers, now.provider))
              || `whom ${now.provider} vouches for` : 'every request its routes let through'}
          </span>
        </div>)}
      {st.public && now.access === 'visitors' && options.some((o) => o.name === now.provider && !o.served) && (
        <p className="warntext small">The kind of {now.provider} is not served by this installation: nobody can sign in, and
          the ingress admits nobody here.</p>)}
      {(st.hosts || []).length > 0 && (
        <div className="envbox-row">
          <span className="k">hosts</span>
          <span className="hostlist">{st.hosts.map((h) => <HostLink key={h} host={h} />)}</span>
        </div>)}
      {dh && (
        <div className="envbox-row">
          <span className="k">default host</span>
          {dh.host ? <HostLink host={dh.host} /> : <span className="mono host dim">{dh.label}.…</span>}
          <span className="muted small">
            {dh.serving
              ? 'its host while none of its domains is served'
              : 'answers 404 while one of its domains is served; it is its host again once none is'}
          </span>
        </div>)}
      <div className="envbox-row admits-form">
        <span className="k">{st.public ? 'admit' : 'it admits'}</span>
        <span className="cred-seg" role="group" aria-label="who it admits">
          <button type="button" className={choice.access === 'visitors' ? 'on' : ''} disabled={off || none}
            title={why || (none ? NO_VISITS : '')} onClick={() => pick('visitors')}>Visitors</button>
          <button type="button" className={choice.access === 'anyone' ? 'on' : ''} disabled={off} title={why}
            onClick={() => pick('anyone')}>Anyone</button>
        </span>
        {choice.access === 'visitors' && (
          <select aria-label="provider" value={choice.provider} disabled={off} title={why}
            onChange={(e) => setPicked({ access: 'visitors', provider: e.target.value })}>
            {!choice.provider && <option value="">choose a provider</option>}
            {options.map((o) => (
              <option key={o.name} value={o.name} disabled={!o.served}>
                {o.name}{o.kind ? ` · ${kindLabel(o.kind)}` : ''}{o.served ? '' : ' — not served by this installation'}
              </option>))}
          </select>)}
        {!choice.access && <span className="muted small">Choose who it admits: this installation signs no visitors in by itself.</span>}
        {none && <span className="muted small">Only anyone: {NO_VISITS}.</span>}
        {changing && (choice.access === 'anyone'
          ? <ConfirmButton className="mini" label="Open to anyone" confirmLabel="Open to anyone: sure?" disabled={off}
            title={why || 'its visits end, and everyone reaches it'} onConfirm={() => publish(true, choice, '')} />
          : <button className="primary mini" type="button" disabled={off || !ready} title={why}
            onClick={() => publish(true, choice, '')}>Admit visitors of {choice.provider}</button>)}
      </div>
      <form className="inlineform" onSubmit={(e) => {
        e.preventDefault();
        publish(true, st.public ? hostLabelChoice(st, current) : choice);
      }}>
        <input className="mono" placeholder={st.hostLabel ? `host label (now ${st.hostLabel})` : 'host label (optional)'}
          value={label} disabled={off} onChange={(e) => setLabel(e.target.value)}
          title="a label with -- in it, e.g. stage--kino; empty: the one it has, or its natural one" />
        <button className="primary mini" type="submit" disabled={off || (st.public ? !label.trim() : !ready)}
          title={why || (st.public || ready ? '' : 'choose who it admits first')}>
          {st.public ? 'Change host label' : 'Publish'}
        </button>
        {st.public && (
          <ConfirmButton className="mini danger" label="Unpublish" confirmLabel="Unpublish: sure?" disabled={off}
            title={ec.admin ? 'the ingress stops serving it and its visits end; the route door still does' : NEEDS.admin}
            onConfirm={() => publish(false)} />)}
      </form>
      {choice.access === 'visitors'
        ? (
          <p className="panel-note">A browser that opens it without a visit is sent to {choice.provider || 'the provider'} to sign
            in; a request that is no navigation, a script&apos;s, a webhook&apos;s or an API client&apos;s, gets 401, so an environment
            that takes webhooks or serves an API beside its pages stays open to anyone. Changing the provider or unpublishing
            ends its visits; a person their provider no longer admits keeps their visit for up to 16 minutes.</p>)
        : choice.access === 'anyone' && <p className="panel-note">Anyone reaches the routes to its image services at its hosts, as
          their from blocks let them.</p>}
      {!st.public && (
        <p className="panel-note">The first publish rolls the image services once, so that their pods join the ingress&apos;s
          group; publishing again later rolls nothing.</p>)}
      {op && op.tone === 'bad' ? <ErrBox error={op.text} /> : <OpNote op={op} onDismiss={clear} />}
    </div>
  );
}

// provider is the summary's provider of a name, null unread.
function provider(list, name) {
  return (list || []).find((p) => p.name === name) || null;
}

// DOMAIN_TONES are the state dots of a domain's state.
const DOMAIN_TONES = { served: 'running', 'dns-moved': 'unknown', unverified: 'absent', 'waiting-for-dns': 'moving', 'waiting-for-certificate': 'moving', deactivated: 'failed' };

export function DomainsBox({ project, st, domains, navigate }) {
  const list = envDomains(domains, st);
  const base = `/projects/${encodeURIComponent(project)}/domains`;
  return (
    <div className="envsect envbox">
      <div className="sect">domains <span className="count">· {domains ? list.length : '…'}</span></div>
      {domains === null && <p className="empty small">The project's domains are not read yet, or not readable with this role.</p>}
      {domains && list.length === 0 && (
        <p className="empty small">No domain is bound to this environment. <EnvLink to={base} navigate={navigate}>The domains →</EnvLink></p>)}
      {list.length > 0 && (
        <div className="tblwrap">
          <table className="tbl envdomains-tbl">
            <thead><tr><th className="col-state" /><th>domain</th><th>state</th><th>verification</th><th className="col-when">bound</th></tr></thead>
            <tbody>
              {list.map((d) => {
                const to = `${base}/${encodeURIComponent(d.id)}`;
                return (
                  <tr key={d.id} className="clickable" onClick={() => navigate(to)}>
                    <td className="col-state"><span className={'sdot ' + (DOMAIN_TONES[d.state] || 'absent')} title={d.state} /></td>
                    <td><EnvLink to={to} navigate={navigate} className="mono host">{d.name}</EnvLink></td>
                    <td className="mono small">{d.state}{d.reason && <div className="muted">{d.reason}</div>}</td>
                    <td className="mono small">{d.verification}</td>
                    <td className="col-when"><Ago iso={d.boundAt} /></td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>)}
    </div>
  );
}

export function RouteTokenBox({ env, ec }) {
  const [token, setToken] = useState('');
  const [op, run, clear] = useOp();
  const origin = globalThis.location ? globalThis.location.origin : '';
  const busy = !!(op && op.busy);
  const curl = token ? routeCurl(origin, env, token) : '';
  return (
    <div className="envsect envbox">
      <div className="sect">route tokens</div>
      <p className="panel-note">
        The route door serves every route of the environment, the ones to attached services too, at{' '}
        <code>/api/v1/environments/{env}/http/…</code> to a request with a route token in <code>X-FeRun-Route-Token</code>.
        ferund keeps only a token's hash: a new one shows once. The route door asks for no visit: a route token reaches the
        environment whoever its hosts admit.
      </p>
      {token && (
        <Secret value={token} title={`Route token for ${env}`} onDismiss={() => setToken('')}>
          <h4>Through the route door</h4>
          <div className="snippet"><pre>{curl}</pre></div>
        </Secret>)}
      <div className="envbox-row">
        <button className="mini" disabled={!ec.write || busy} title={ec.write ? '' : needsWrite(ec)}
          onClick={async () => { const r = await run(() => api.mintRouteToken(env), ''); if (r) setToken(r.token); }}>
          <Icon name="key" size={12} /> Mint a route token</button>
        <RevokeTokens disabled={!ec.write || busy} why={needsWrite(ec)} onRevoke={() => run(() => api.revokeRouteTokens(env).then(() => true),
          'Every route token of the environment is revoked: requests with one are refused from now on.')} />
      </div>
      <OpNote op={op} onDismiss={clear} />
    </div>
  );
}

function RevokeTokens({ disabled, why, onRevoke }) {
  const [armed, arm, disarm] = useArm();
  return (
    <ArmButton armKey="revoke" armed={armed} arm={arm} disarm={disarm} className="mini" disabled={disabled}
      title={disabled ? why : 'revoke every route token of the environment'} onConfirm={onRevoke}>Revoke all</ArmButton>
  );
}

// -------------------------------------------------------------- settings --

export function SettingsTab({ project, env, st, ec, changed, navigate }) {
  if (!st) return <p className="empty">Loading…</p>;
  return (
    <>
      <ProtectBox env={env} st={st} ec={ec} changed={changed} />
      <RenameBox project={project} env={env} st={st} ec={ec} changed={changed} navigate={navigate} />
      <CloneBox project={project} env={env} st={st} ec={ec} changed={changed} navigate={navigate} />
      <DeleteBox project={project} env={env} st={st} ec={ec} changed={changed} navigate={navigate} />
    </>
  );
}

// RenameBox renames the environment (ADR 0037): the same environment
// under a new name, its hosts, grants and logs kept.
export function RenameBox({ project, env, st, ec, changed, navigate }) {
  const [name, setName] = useState('');
  const [op, run, clear] = useOp();
  const { reload } = useMe();
  const busy = !!(op && op.busy);
  const next = name.trim();
  const submit = async (e) => {
    e.preventDefault();
    const r = await run(() => api.renameEnvironment(env, next), (res) => `Renamed to ${res.name}.`);
    if (r) { setName(''); changed(); reload().catch(() => {}); navigate(envPath(project, r.name, 'settings')); }
  };
  return (
    <div className="envsect envbox">
      <div className="sect">rename</div>
      <p className="panel-note">
        The same environment under a new name: its ID, hosts, grants, logs and everything that runs stay. Links and
        scripts that name it by its old name do not follow it; by its ID they do.
      </p>
      <form className="envclone" onSubmit={submit} title={ec.del ? '' : needsDel(ec)}>
        <fieldset disabled={!ec.del || st.deleting || busy}>
          <label className="envfield"><span className="k">name</span>
            <input className="mono" value={name} placeholder={st.name} onChange={(e) => setName(e.target.value)} /></label>
          <div><button className="mini" type="submit" disabled={!next || next === st.name}>Rename</button></div>
        </fieldset>
      </form>
      {op && op.tone === 'bad' ? <ErrBox error={op.text} /> : <OpNote op={op} onDismiss={clear} />}
    </div>
  );
}

// CloneSecrets are the clone's boxes of the secrets it copies (null: still
// loading), offered only with mayCopy, write on the source.
export function CloneSecrets({ secrets, copy, setCopy, mayCopy }) {
  return (
    <div className="envfield"><span className="k">secrets</span>
      <span className="envchecks">
        {secrets === null && <span className="muted small">…</span>}
        {secrets && secrets.length === 0 && <span className="muted small">none</span>}
        {(secrets || []).map((s) => (
          <label key={s.name} className="mono small">
            <input type="checkbox" checked={mayCopy && !!copy[s.name]} disabled={!mayCopy}
              onChange={(e) => setCopy((c) => ({ ...c, [s.name]: e.target.checked }))} />
            {' '}{s.name} <span className="muted">{s.type === 'http' ? 'HTTP · ' : ''}v{s.version}</span>
          </label>))}
        {secrets && secrets.length > 0 && (mayCopy
          ? <span className="muted small">copied at their latest versions; one left out the spec names fails the clone</span>
          : <span className="muted small">copying secrets takes write on this environment; a spec that names them does not clone</span>)}
      </span>
    </div>
  );
}

export function CloneBox({ project, env, st, ec, changed, navigate }) {
  const [name, setName] = useState('');
  const [deployment, setDeployment] = useState(0);
  const [deployments, setDeployments] = useState(null);
  const [secrets, setSecrets] = useState(null);
  const [copy, setCopy] = useState({});
  const [op, run, clear] = useOp();
  const { reload } = useMe();
  useEffect(() => {
    let live = true;
    api.listDeployments(env).then((r) => { if (live) setDeployments(r.deployments || []); }, () => { if (live) setDeployments([]); });
    // Both kinds (ADR 0055): a clone gets the ones it copies, each at
    // its latest version.
    api.listSecrets(env).then((r) => {
      if (!live) return;
      const list = r.secrets || [];
      setSecrets(list);
      setCopy(Object.fromEntries(list.map((s) => [s.name, true])));
    }, () => { if (live) setSecrets([]); });
    return () => { live = false; };
  }, [env]);
  const busy = !!(op && op.busy);
  const findings = op && op.error && op.error.body && op.error.body.findings;
  // Copying secrets hands their values to the new environment: that takes
  // write on this one, which a project writer lacks on a protected one.
  const mayCopy = !!ec.write;
  const submit = async (e) => {
    e.preventDefault();
    const body = cloneBody({ name, deployment, copy: mayCopy ? copy : {}, secrets: secrets || [] });
    const r = await run(() => api.cloneEnvironment(env, body), (res) => `${res.name} made from deployment ${body.deployment || st.deployment}.`);
    if (r) { changed(); reload().catch(() => {}); navigate(envPath(project, r.name)); }
  };
  return (
    <div className="envsect envbox">
      <div className="sect">clone</div>
      <p className="panel-note">
        A new environment with a deployment's spec, its references reaching its own objects. Its data is not copied: its
        addons start empty. Its sandboxes and jobs stay here, and attached services are dropped (they run in a sandbox of
        this one). It is not public until its project publishes it. It is not protected, whatever this one is.
      </p>
      <form className="envclone" onSubmit={submit} title={ec.clone ? '' : NEEDS.clone}>
        <fieldset disabled={!ec.clone || busy}>
          <label className="envfield"><span className="k">name</span>
            <input className="mono" value={name} placeholder={`${st.name}-copy`} onChange={(e) => setName(e.target.value)} /></label>
          <label className="envfield"><span className="k">from</span>
            <select value={deployment} onChange={(e) => setDeployment(Number(e.target.value))}>
              <option value={0}>the latest deployment ({st.deployment || '—'})</option>
              {(deployments || []).filter((d) => d.number !== st.deployment).map((d) => (
                <option key={d.number} value={d.number}>deployment {d.number} · {d.state}{(d.tags || []).length ? ` · ${d.tags.join(', ')}` : ''}</option>))}
            </select></label>
          <CloneSecrets secrets={secrets} copy={copy} setCopy={setCopy} mayCopy={mayCopy} />
          <div><button className="primary mini" type="submit" disabled={!name.trim()}>Clone</button></div>
        </fieldset>
      </form>
      {op && op.tone === 'bad' ? <ErrBox error={op.text} /> : <OpNote op={op} onDismiss={clear} />}
      {(findings || []).length > 0 && (
        <ul className="envfindings">
          {findings.map((f, i) => (
            <li key={i} className={f.severity === 'error' ? 'bad' : 'muted'}>
              <span className="mono">{f.path}</span>: {f.problem}{f.remedy && <span className="muted"> — {f.remedy}</span>}
            </li>))}
        </ul>)}
    </div>
  );
}

// InUse says why a delete was refused (409 environment_in_use, ADR
// 0035): the sandboxes and the jobs on their way that keep the
// environment, each a link; nothing for any other error.
export function InUse({ error, project, env, navigate }) {
  const u = inUse(error);
  if (!u) return null;
  const links = (ids, path) => ids.map((id, i) => (
    <React.Fragment key={id}>{i > 0 && ', '}<EnvLink to={path(project, env, id)} navigate={navigate} className="mono">{id}</EnvLink></React.Fragment>));
  return (
    <div className="message"><Icon name="alert" size={14} />
      <span>
        It is not deleted while something runs in it, so that nobody loses a workspace&apos;s disk with it.
        {u.sandboxes.length > 0 && <> Its sandboxes: {links(u.sandboxes, sandboxPath)}.</>}
        {u.jobs.length > 0 && <> Its jobs queued or running: {links(u.jobs, jobPath)}.</>}
        {' '}Delete the sandboxes and cancel the jobs first; ended jobs&apos; records go with the environment.
      </span>
    </div>
  );
}

export function DeleteBox({ project, env, st, ec, changed, navigate }) {
  const [armed, arm, disarm] = useArm();
  const [op, run] = useOp();
  const { reload } = useMe();
  const blocked = op && op.tone === 'bad' ? inUse(op.error) : null;
  return (
    <div className="envsect envbox envbox-danger">
      <div className="sect">delete</div>
      <p className="panel-note">
        Deleting {st.name} stops and removes its instances and their volumes, with its addons' data, then the environment
        with its deployments, tags, secrets and route tokens, and its ended jobs&apos; records. Its default host goes; its
        domains stay the project&apos;s. While it has sandboxes or jobs queued or running it is not deleted. It cannot be undone.
      </p>
      <ArmButton armKey="delete" armed={armed} arm={arm} disarm={disarm} className="mini" icon="trash"
        disabled={!ec.del || st.deleting || !!(op && op.busy)} title={ec.del ? '' : needsDel(ec)}
        confirmLabel={`Delete ${st.name}: sure?`}
        onConfirm={async () => {
          const r = await run(() => api.deleteEnvironment(env).then(() => true), '');
          if (r) { changed(); reload().catch(() => {}); navigate(envPath(project)); }
        }}>
        {st.deleting ? 'Being deleted' : 'Delete environment'}
      </ArmButton>
      {op && op.tone === 'bad' && (blocked
        ? <InUse error={op.error} project={project} env={st.name} navigate={navigate} />
        : <ErrBox error={op.text} />)}
      {st.deleting && <p className="muted small">The deletion is under way: ferund removes what runs first.</p>}
    </div>
  );
}

// ------------------------------------------------------------ protection --

// ProtectBox is the environment's protection (ADR 0060): its state and
// the rule, Protect (a dialog that names who changes it today by a
// project grant of write and offers each a grant on it) and Lift
// protection, for an admin of the environment.
export function ProtectBox({ env, st, ec, changed }) {
  const [dialog, setDialog] = useState(false);
  const [armed, arm, disarm] = useArm();
  const [op, run, clear] = useOp();
  const { reload } = useMe();
  const busy = !!(op && op.busy);
  const lift = async () => {
    const r = await run(() => api.setProtected(env, false), 'Protection lifted: a project grant of write changes it again.');
    if (r) { changed(); reload().catch(() => {}); }
  };
  return (
    <div className="envsect envbox">
      <div className="sect">protection</div>
      <p className="panel-note">
        {st.protected
          ? <><Icon name="shield" size={12} /> <b>{st.name} is protected.</b> A project grant of read or write reads it;
            changing it takes a grant on it, or admin on the project. Whoever has write on the project still plans
            against it.</>
          : <>{st.name} is not protected: whoever has write on the project changes it. Protected, a project grant of
            read or write only reads it, and changing it takes a grant on it, or admin on the project. Visitors
            and hosts are apart (Domains &amp; access).</>}
      </p>
      <div className="envbox-row">
        {st.protected
          ? (
            <ArmButton armKey="lift" armed={armed} arm={arm} disarm={disarm} className="mini" icon="shield"
              disabled={!ec.admin || busy} title={ec.admin ? 'project write changes it again' : NEEDS.admin}
              confirmLabel="lift? project write changes it again" onConfirm={lift}>Lift protection</ArmButton>)
          : (
            <button className="mini" disabled={!ec.admin || busy} title={ec.admin ? 'who changes it today, then protect' : NEEDS.admin}
              onClick={() => { clear(); setDialog(true); }}>
              <Icon name="shield" size={12} /> Protect…</button>)}
      </div>
      <OpNote op={op} onDismiss={clear} />
      {dialog && (
        <ProtectDialog env={env} st={st} onClose={() => setDialog(false)}
          onDone={() => { setDialog(false); changed(); reload().catch(() => {}); }} />)}
    </div>
  );
}

// grantOf is the call that gives a principal of GET …/principals the
// role on an environment, by its kind: a person's, an integration's, a
// project token's grant (ADR 0060's dialog).
export function grantOf(p, environment, role = 'write') {
  if (p.kind === 'person') return () => api.putGrant(p.id, { role, environment });
  if (p.kind === 'integration') return () => api.putIntegrationGrant(p.id, { role, environment });
  if (p.kind === 'token') return () => api.putTokenGrant(p.id, { role, environment });
  return null;
}

// PRINCIPAL_KINDS name the kinds of principals in the dialog.
const PRINCIPAL_KINDS = { person: 'person', token: 'project token', integration: 'integration' };

// principalKey is a principal's key in the dialog's ticks.
export function principalKey(p) {
  return `${p.kind}/${p.id}`;
}

// offersGrant tells whether the dialog offers a principal a grant write
// on the environment (ADR 0060: each of them): every one of a kind with a
// grant route, but one that holds write or admin on it already, or admin
// on the project (which changes it protected or not).
export function offersGrant(p) {
  return !!grantOf(p, '') && !['write', 'admin'].includes(p.environmentRole) && p.projectRole !== 'admin';
}

// defaultTicks are the dialog's ticks as it opens: those that lose write
// by the protection (losesWrite, ferund's).
export function defaultTicks(list) {
  return Object.fromEntries((list || []).filter((p) => p.losesWrite && offersGrant(p)).map((p) => [principalKey(p), true]));
}

// protectEnvironment gives the ticked principals their grant write on the
// environment (its ID), one after the other, then protects it (env, its
// name). The first failure stops it: grants given stay, and the
// environment is not protected.
export async function protectEnvironment(list, give, environment, env) {
  for (const p of list || []) {
    const fn = give[principalKey(p)] && offersGrant(p) ? grantOf(p, environment) : null;
    if (fn) await fn();
  }
  await api.setProtected(env, true);
}

// ProtectDialog names everyone with a grant in the project (ferund's
// principals), marks those who change the environment today by a project
// grant of write and read it once it is protected (losesWrite, ferund's),
// and offers each who has no write on it otherwise a grant write on it,
// ticked by default for those who lose write. Protect gives the ticked
// grants first, then protects. It is modal: focus moves in, stays in, and
// returns to what opened it; while it works it does not close.
export function ProtectDialog({ env, st, onClose, onDone }) {
  const [list, setList] = useState(null);
  const [err, setErr] = useState('');
  const [give, setGive] = useState({});
  const [busy, setBusy] = useState(false);
  const box = useRef(null);
  const busyNow = useRef(false);
  busyNow.current = busy;
  useEffect(() => {
    let live = true;
    api.envPrincipals(env).then((r) => {
      if (!live) return;
      const ps = r.principals || [];
      setList(ps);
      setGive(defaultTicks(ps));
    }, (e) => { if (live) setErr(e.message); });
    return () => { live = false; };
  }, [env]);
  // Modal: focus moves in, stays in, and back to its opener on close.
  useModal(box, onClose, busyNow);
  // Once the list is read: the first tick, else Protect.
  useEffect(() => {
    if (!list || !box.current) return;
    const first = box.current.querySelector('input[type="checkbox"]:not([disabled])') || box.current.querySelector('button.primary:not([disabled])');
    if (first) first.focus();
  }, [list]);
  const protect = async () => {
    setBusy(true);
    setErr('');
    try {
      await protectEnvironment(list, give, st.id, env);
      onDone();
    } catch (e) {
      setErr(`${e.message} Grants given so far stay; ${st.name} is not protected yet.`);
      setBusy(false);
    }
  };
  return (
    <div className="palette-back" onMouseDown={() => { if (!busyNow.current) onClose(); }}>
      <div className="palette dialog" role="dialog" aria-modal="true" aria-label={`Protect ${st.name}`} tabIndex={-1} ref={box}
        onMouseDown={(e) => e.stopPropagation()}>
        <h3><Icon name="shield" size={16} /> Protect <span className="mono">{st.name}</span></h3>
        <ProtectPrincipals list={list} give={give} setGive={setGive} env={st.name} />
        <ErrBox error={err} />
        <div className="actionrow">
          <button className="primary" disabled={busy || !list} onClick={protect}><Icon name="shield" size={14} /> Protect</button>
          <button className="ghost" disabled={busy} onClick={onClose}>Cancel</button>
        </div>
      </div>
    </div>
  );
}

// ProtectPrincipals is the dialog's body: who loses write by the
// protection, then everyone else who holds a grant in the project; each
// without write on it otherwise is offered a grant write on it.
export function ProtectPrincipals({ list, give, setGive, env }) {
  if (!list) return <p className="empty">Reading who holds a grant…</p>;
  const losers = list.filter((p) => p.losesWrite);
  const rest = list.filter((p) => !p.losesWrite);
  const row = (p) => {
    const key = principalKey(p);
    const who = p.label || p.id;
    return (
      <tr key={key}>
        <td><span className="principal-kind">{PRINCIPAL_KINDS[p.kind] || p.kind}</span><div>{who}</div></td>
        <td className="small">project {p.projectRole || '—'}{p.environmentRole ? ` · ${env} ${p.environmentRole}` : ''}</td>
        <td className="small">
          {offersGrant(p) ? (
            <label>
              <input type="checkbox" checked={!!give[key]} aria-label={`give ${who} write on ${env}`}
                onChange={(e) => setGive((g) => ({ ...g, [key]: e.target.checked }))} />
              {' '}give write on {env}
              {p.kind === 'token' && p.losesWrite && <div className="muted small">without it the token stops deploying here</div>}
              {p.kind === 'integration' && <div className="muted small">an integration then changes it only for a person it names who may</div>}
            </label>)
            : <span className="muted">keeps what it has</span>}
        </td>
      </tr>
    );
  };
  return (
    <>
      <p className="small">
        Protected, a project grant of read or write reads {env}; changing it takes a grant on it, or admin on the project.
        Sandbox and route tokens minted here by such a grant stop until it is lifted or their minter gets one.
        A token&apos;s grant given here stays on Tokens, where it is removed.
        {losers.length > 0
          ? ` These change it today by a project grant of write and read it from then on, unless they get a grant on it:`
          : ' Nobody changes it today by a project grant of write alone: nobody loses write.'}
      </p>
      {list.length > 0 && (
        <div className="tblwrap"><table className="tbl">
          <thead><tr><th>who</th><th>grants</th><th>on protection</th></tr></thead>
          <tbody>{losers.map(row)}{rest.map(row)}</tbody>
        </table></div>)}
    </>
  );
}
