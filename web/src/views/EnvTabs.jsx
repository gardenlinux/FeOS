// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, Secret, useArm, usePoll } from '../components.jsx';
import { fmtDate, fmtDurNs, fmtNs } from '../format.js';
import { envPath, jobPath, sandboxPath } from '../routes.js';
import { inUse } from './reach.js';
import {
  actionProblem, cloneBody, defaultHost, envDomains, errorShare, fmtLatency, newestFirst, olderCursor, publishNote,
  routeCurl, sparkPoints, statusTone,
} from './envs.js';
import '../styles/environments.css';

// An environment's pages beside its objects and deployments: its traffic
// (the ingress's answers per service and its access log), its secrets,
// its domains and access (public, hosts, domains, route tokens) and its
// settings (clone, delete), with the small widgets the environment's
// actions share. ec is what the role may do on the environment
// (Environments.jsx, envCan); every write calls changed, which refreshes
// the page and the tenant's summary.

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
// deletion: a revert, an unpublish's opposite.
export function ConfirmButton({ label, confirmLabel, onConfirm, disabled, title, className = '', icon }) {
  const [armed, arm, disarm] = useArm();
  const on = armed === 'go';
  return (
    <button className={className + ' confirm' + (on ? ' armed' : '')} disabled={disabled} title={title}
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
  del: 'needs admin on this environment or write on the tenant',
  clone: 'needs write on the tenant',
};

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
export function TrafficTab({ tenant, env, navigate, service = '' }) {
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
      <AccessLog tenant={tenant} env={env} navigate={navigate} service={service} />
    </>
  );
}

const ACCESS_PAGE = 100;

// AccessLog is the ingress's access log of the environment, newest
// first: the newest page polled, older pages read on demand.
function AccessLog({ tenant, env, navigate, service = '' }) {
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
      <AccessTable records={records} tenant={tenant} env={env} navigate={navigate} />
      {!records && !err && <p className="empty">Loading…</p>}
      {records && records.length >= ACCESS_PAGE && more && (
        <button className="ghost mini" disabled={busy} onClick={loadOlder}>{busy ? 'Loading…' : 'Older'}</button>)}
    </div>
  );
}

export function AccessTable({ records, tenant, env, navigate }) {
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
                    ? <EnvLink to={envPath(tenant, env, 'traces', r.traceId)} navigate={navigate} title="the trace">{r.traceId.slice(0, 8)}</EnvLink>
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

export function SecretsTab({ env, st, ec, changed }) {
  const [secrets, setSecrets] = useState(null);
  const [err, setErr] = useState('');
  const [editing, setEditing] = useState('');
  const [op, run, clear] = useOp();
  const refresh = useCallback(async () => {
    try {
      setSecrets((await api.listSecrets(env)).secrets || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [env]);
  usePoll(refresh, 10000);
  const put = async (name, value) => {
    const r = await run(() => api.putSecret(env, name, value),
      (res) => `${res.name}: version ${res.version} stored. Deployment ${st ? st.deployment : '—'} keeps the version it pins; the next deployment that pins ${res.name} takes it.`);
    if (r) { setEditing(''); refresh(); changed(); }
    return !!r;
  };
  const del = async (name) => {
    const r = await run(() => api.deleteSecret(env, name).then(() => true), `${name} deleted.`);
    if (r) { refresh(); changed(); }
  };
  return (
    <>
      <p className="panel-note envsecrets-note">
        A deployment pins each secret it names at a version: a new value is a new version, which the pods get with the
        next deployment (the builder's apply pins it). Values are never shown, here or anywhere.
      </p>
      <ErrBox error={err} />
      <OpNote op={op} onDismiss={clear} />
      <SecretsTable secrets={secrets} ec={ec} editing={editing} setEditing={setEditing} onPut={put} onDelete={del} busy={!!(op && op.busy)} />
      {!secrets && !err && <p className="empty">Loading…</p>}
      {secrets && <NewSecret disabled={!ec.write} onPut={put} />}
    </>
  );
}

export function SecretsTable({ secrets, ec, editing, setEditing, onPut, onDelete, busy }) {
  const list = secrets || [];
  return (
    <>
      <div className="tblwrap">
        <table className="tbl secrets-tbl">
          <thead>
            <tr><th>name</th><th className="col-num">latest</th><th>set by</th><th className="col-when">set</th><th className="col-quick" /></tr>
          </thead>
          <tbody>
            {list.map((s) => (
              <React.Fragment key={s.name}>
                <tr>
                  <td className="mono strong">{s.name}</td>
                  <td className="col-num mono">v{s.version}</td>
                  <td className="mono small trunc" title={s.createdBy}>{s.createdBy || <span className="muted">—</span>}</td>
                  <td className="col-when"><Ago iso={s.createdAt} /></td>
                  <td className="col-quick nowrap">
                    <button className="mini" disabled={!ec.write || busy} title={ec.write ? 'a new version' : NEEDS.write}
                      onClick={() => setEditing(editing === s.name ? '' : s.name)}><Icon name="key" size={12} /> Set value</button>{' '}
                    <DeleteSecret name={s.name} disabled={!ec.write || busy} onDelete={onDelete} />
                  </td>
                </tr>
                {editing === s.name && (
                  <tr className="secret-edit"><td colSpan={5}>
                    <SecretValue name={s.name} onPut={onPut} onCancel={() => setEditing('')} />
                  </td></tr>)}
              </React.Fragment>
            ))}
          </tbody>
        </table>
      </div>
      {secrets && list.length === 0 && <p className="empty">No secrets.</p>}
    </>
  );
}

function DeleteSecret({ name, disabled, onDelete }) {
  const [armed, arm, disarm] = useArm();
  return (
    <ArmButton armKey={name} armed={armed} arm={arm} disarm={disarm} className="mini" disabled={disabled}
      title={disabled ? NEEDS.write : 'delete every version; refused while the latest deployment pins it'}
      onConfirm={() => onDelete(name)}>Delete</ArmButton>
  );
}

function SecretValue({ name, onPut, onCancel }) {
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

function NewSecret({ disabled, onPut }) {
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

export function AccessTab({ tenant, env, st, ec, domains, changed, navigate }) {
  if (!st) return <p className="empty">Loading…</p>;
  return (
    <>
      <PublicBox env={env} st={st} ec={ec} domains={domains} changed={changed} />
      <DomainsBox tenant={tenant} st={st} domains={domains} navigate={navigate} />
      <RouteTokenBox env={st.name} ec={ec} />
    </>
  );
}

export function PublicBox({ env, st, ec, domains, changed }) {
  const [label, setLabel] = useState('');
  const [op, run, clear] = useOp();
  const dh = defaultHost(st, domains || []);
  const publish = (on) => run(() => api.setPublic(env, on, on ? label.trim() : ''), publishNote).then((r) => {
    if (r) { setLabel(''); changed(); }
  });
  const busy = !!(op && op.busy);
  return (
    <div className="envsect envbox">
      <div className="sect">public</div>
      <div className="envbox-row">
        {st.public
          ? <span className="state-label running"><span className="sdot running" />public</span>
          : <span className="state-label absent"><span className="sdot absent" />not public</span>}
        <span className="muted small">
          {st.public
            ? 'The ingress serves its routes to image services at its hosts, without a token.'
            : 'Only the route door serves it, to a route token. Publishing gives it a default host.'}
        </span>
      </div>
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
      <form className="inlineform" onSubmit={(e) => { e.preventDefault(); publish(true); }}>
        <input className="mono" placeholder={st.hostLabel ? `host label (now ${st.hostLabel})` : 'host label (optional)'}
          value={label} disabled={!ec.admin || busy} onChange={(e) => setLabel(e.target.value)}
          title="a label with -- in it, e.g. stage--kino; empty: the one it has, or its natural one" />
        <button className="primary mini" type="submit" disabled={!ec.admin || busy || (st.public && !label.trim())}
          title={ec.admin ? '' : NEEDS.admin}>
          {st.public ? 'Change host label' : 'Publish'}
        </button>
        {st.public && (
          <ConfirmButton className="mini danger" label="Unpublish" confirmLabel="Unpublish: sure?" disabled={!ec.admin || busy}
            title={ec.admin ? 'the ingress stops serving it; the route door still does' : NEEDS.admin}
            onConfirm={() => publish(false)} />)}
      </form>
      {!st.public && (
        <p className="panel-note">The first publish rolls the image services once, so that their pods join the ingress's
          group; publishing again later rolls nothing.</p>)}
      {op && op.tone === 'bad' ? <ErrBox error={op.text} /> : <OpNote op={op} onDismiss={clear} />}
    </div>
  );
}

// DOMAIN_TONES are the state dots of a domain's state.
const DOMAIN_TONES = { served: 'running', 'dns-moved': 'unknown', unverified: 'absent', 'waiting-for-dns': 'moving', 'waiting-for-certificate': 'moving', deactivated: 'failed' };

export function DomainsBox({ tenant, st, domains, navigate }) {
  const list = envDomains(domains, st);
  const base = `/tenants/${encodeURIComponent(tenant)}/domains`;
  return (
    <div className="envsect envbox">
      <div className="sect">domains <span className="count">· {domains ? list.length : '…'}</span></div>
      {domains === null && <p className="empty small">The tenant's domains are not read yet, or not readable with this role.</p>}
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
        ferund keeps only a token's hash: a new one shows once.
      </p>
      {token && (
        <Secret value={token} title={`Route token for ${env}`} onDismiss={() => setToken('')}>
          <h4>Through the route door</h4>
          <div className="snippet"><pre>{curl}</pre></div>
        </Secret>)}
      <div className="envbox-row">
        <button className="mini" disabled={!ec.admin || busy} title={ec.admin ? '' : NEEDS.admin}
          onClick={async () => { const r = await run(() => api.mintRouteToken(env), ''); if (r) setToken(r.token); }}>
          <Icon name="key" size={12} /> Mint a route token</button>
        <RevokeTokens disabled={!ec.admin || busy} onRevoke={() => run(() => api.revokeRouteTokens(env).then(() => true),
          'Every route token of the environment is revoked: requests with one are refused from now on.')} />
      </div>
      <OpNote op={op} onDismiss={clear} />
    </div>
  );
}

function RevokeTokens({ disabled, onRevoke }) {
  const [armed, arm, disarm] = useArm();
  return (
    <ArmButton armKey="revoke" armed={armed} arm={arm} disarm={disarm} className="mini" disabled={disabled}
      title={disabled ? NEEDS.admin : 'revoke every route token of the environment'} onConfirm={onRevoke}>Revoke all</ArmButton>
  );
}

// -------------------------------------------------------------- settings --

export function SettingsTab({ tenant, env, st, ec, changed, navigate }) {
  if (!st) return <p className="empty">Loading…</p>;
  return (
    <>
      <RenameBox tenant={tenant} env={env} st={st} ec={ec} changed={changed} navigate={navigate} />
      <CloneBox tenant={tenant} env={env} st={st} ec={ec} changed={changed} navigate={navigate} />
      <DeleteBox tenant={tenant} env={env} st={st} ec={ec} changed={changed} navigate={navigate} />
    </>
  );
}

// RenameBox renames the environment (ADR 0037): the same environment
// under a new name, its hosts, grants and logs kept.
export function RenameBox({ tenant, env, st, ec, changed, navigate }) {
  const [name, setName] = useState('');
  const [op, run, clear] = useOp();
  const busy = !!(op && op.busy);
  const next = name.trim();
  const submit = async (e) => {
    e.preventDefault();
    const r = await run(() => api.renameEnvironment(env, next), (res) => `Renamed to ${res.name}.`);
    if (r) { setName(''); changed(); navigate(envPath(tenant, r.name, 'settings')); }
  };
  return (
    <div className="envsect envbox">
      <div className="sect">rename</div>
      <p className="panel-note">
        The same environment under a new name: its ID, hosts, grants, logs and everything that runs stay. Links and
        scripts that name it by its old name do not follow it; by its ID they do.
      </p>
      <form className="envclone" onSubmit={submit} title={ec.del ? '' : NEEDS.del}>
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

export function CloneBox({ tenant, env, st, ec, changed, navigate }) {
  const [name, setName] = useState('');
  const [deployment, setDeployment] = useState(0);
  const [deployments, setDeployments] = useState(null);
  const [secrets, setSecrets] = useState(null);
  const [copy, setCopy] = useState({});
  const [op, run, clear] = useOp();
  useEffect(() => {
    let live = true;
    api.listDeployments(env).then((r) => { if (live) setDeployments(r.deployments || []); }, () => { if (live) setDeployments([]); });
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
  const submit = async (e) => {
    e.preventDefault();
    const body = cloneBody({ name, deployment, copy });
    const r = await run(() => api.cloneEnvironment(env, body), (res) => `${res.name} made from deployment ${body.deployment || st.deployment}.`);
    if (r) { changed(); navigate(envPath(tenant, r.name)); }
  };
  return (
    <div className="envsect envbox">
      <div className="sect">clone</div>
      <p className="panel-note">
        A new environment with a deployment's spec, its references reaching its own objects. Its data is not copied: its
        addons start empty. Its sandboxes and jobs stay here, and attached services are dropped (they run in a sandbox of
        this one). It is not public until its tenant publishes it.
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
          <div className="envfield"><span className="k">secrets</span>
            <span className="envchecks">
              {secrets === null && <span className="muted small">…</span>}
              {secrets && secrets.length === 0 && <span className="muted small">none</span>}
              {(secrets || []).map((s) => (
                <label key={s.name} className="mono small">
                  <input type="checkbox" checked={!!copy[s.name]} onChange={(e) => setCopy((c) => ({ ...c, [s.name]: e.target.checked }))} />
                  {' '}{s.name} <span className="muted">v{s.version}</span>
                </label>))}
              {secrets && secrets.length > 0 && <span className="muted small">copied at their latest versions; one left out the spec names fails the clone</span>}
            </span>
          </div>
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
export function InUse({ error, tenant, env, navigate }) {
  const u = inUse(error);
  if (!u) return null;
  const links = (ids, path) => ids.map((id, i) => (
    <React.Fragment key={id}>{i > 0 && ', '}<EnvLink to={path(tenant, env, id)} navigate={navigate} className="mono">{id}</EnvLink></React.Fragment>));
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

export function DeleteBox({ tenant, env, st, ec, changed, navigate }) {
  const [armed, arm, disarm] = useArm();
  const [op, run] = useOp();
  const blocked = op && op.tone === 'bad' ? inUse(op.error) : null;
  return (
    <div className="envsect envbox envbox-danger">
      <div className="sect">delete</div>
      <p className="panel-note">
        Deleting {st.name} stops and removes its instances and their volumes, with its addons' data, then the environment
        with its deployments, tags, secrets and route tokens, and its ended jobs&apos; records. Its default host goes; its
        domains stay the tenant&apos;s. While it has sandboxes or jobs queued or running it is not deleted. It cannot be undone.
      </p>
      <ArmButton armKey="delete" armed={armed} arm={arm} disarm={disarm} className="mini" icon="trash"
        disabled={!ec.del || st.deleting || !!(op && op.busy)} title={ec.del ? '' : NEEDS.del}
        confirmLabel={`Delete ${st.name}: sure?`}
        onConfirm={async () => {
          const r = await run(() => api.deleteEnvironment(env).then(() => true), '');
          if (r) { changed(); navigate(envPath(tenant)); }
        }}>
        {st.deleting ? 'Being deleted' : 'Delete environment'}
      </ArmButton>
      {op && op.tone === 'bad' && (blocked
        ? <InUse error={op.error} tenant={tenant} env={st.name} navigate={navigate} />
        : <ErrBox error={op.text} />)}
      {st.deleting && <p className="muted small">The deletion is under way: ferund removes what runs first.</p>}
    </div>
  );
}
