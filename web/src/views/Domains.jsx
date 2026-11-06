// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useRef, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, CopyBtn, ErrBox, useArm, useNow, usePoll } from '../components.jsx';
import { fmtAgo, fmtDate, fmtDateTime } from '../format.js';
import { envRole, atLeast, useAccess } from '../people.js';
import { envPath } from '../routes.js';
import { useSummary } from '../summary.jsx';
import { callout, certValid, dnsNow, domainDot, domainSteps, isServed, mayBind, monoSplit, pickDomain, stateLine } from './domains.js';
import '../styles/domains.css';

// The tenant's domains (ADR 0021): the list of its claims with their
// states, and a domain's page (the concept's Fig. 7): the way it goes,
// the current problem with its deadline, the records to set and what DNS
// says now, its environment and its certificate. The pieces are exported
// for the environment's "Domains & access" tab.

const POLL_MS = 10000;

const NEEDS_ADMIN = 'needs admin on this tenant';

// envOf is the environment a domain is bound to, from the summary's
// list; null unbound or not readable.
export function envOf(d, envs) {
  if (!d.environment) return null;
  return (envs || []).find((e) => e.id === d.environment) || null;
}

// envLabel is a bound environment's name, else its ID's start.
function envLabel(d, envs) {
  const e = envOf(d, envs);
  if (e) return e.name;
  return d.environment ? String(d.environment).slice(0, 8) : '';
}

// domainPath is a domain's page.
export function domainPath(tenant, d) {
  const base = `/tenants/${encodeURIComponent(tenant)}/domains`;
  return d ? `${base}/${encodeURIComponent(d.id || d.name)}` : base;
}

// DomainDot is a domain state's dot.
export function DomainDot({ state }) {
  return <span className={'sdot ' + domainDot(state)} title={state || 'unknown'} />;
}

// DomainStateLabel is the state's dot and word, coloured by its dot.
export function DomainStateLabel({ state }) {
  return <span className={'state-label dom-' + domainDot(state)}><DomainDot state={state} />{state || 'unknown'}</span>;
}

// DomainStateLine is a domain's state as a dot, the word and the
// sentence with its deadlines; the caller re-renders it (useNow).
export function DomainStateLine({ d }) {
  const line = stateLine(d);
  return (
    <span className="dom-stateline">
      <DomainStateLabel state={d.state} />
      {line && <span className="muted small">{line}</span>}
    </span>
  );
}

// DomainStepper is the way of a domain: claimed → verified → bound →
// routed → certificate → served.
export function DomainStepper({ d, envName }) {
  return (
    <ol className="dom-stepper" aria-label="the domain's way">
      {domainSteps(d, envName).map((s) => (
        <li key={s.key} className={'dom-step ' + s.status}>
          <b>{s.key}</b><span>{s.sub}</span>
        </li>
      ))}
    </ol>
  );
}

// DomainCallout is the sentence of the current problem, if any.
export function DomainCallout({ d, envName }) {
  const c = callout(d, envName);
  if (!c) return null;
  return <div className={'dom-callout t-' + c.tone}><Icon name="alert" size={14} /><span>{monoSplit(c.text, c.mono).map((p, i) => (p.mono ? <span key={i} className="mono">{p.text}</span> : p.text))}</span></div>;
}

function Says({ v }) {
  if (v.ok === null) return <span className="muted">{v.text}</span>;
  return <span className={v.ok ? 'ok' : 'warn'}>{v.ok ? '✓' : '✗'} <span className="mono">{v.text}</span></span>;
}

// RecordsTable is the records to set at the DNS provider, with what DNS
// says now, and the apex's alternative.
export function RecordsTable({ d }) {
  const rec = d.records || {};
  const txt = rec.txt || {};
  const now = dnsNow(d);
  const rows = [
    { type: 'TXT', name: txt.name, value: txt.value, says: now.txt },
    { type: 'CNAME', name: d.name, value: rec.cname, says: now.route },
  ];
  const addrs = rec.addresses || [];
  return (
    <>
      <div className="tblwrap"><table className="tbl dom-records">
        <thead><tr><th>type</th><th>name</th><th>value</th><th>DNS says now</th></tr></thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.type}>
              <td className="mono">{r.type}</td>
              <td><span className="dom-copycell"><span className="mono">{r.name}</span>{r.name && <CopyBtn text={r.name} mini title="copy the name" />}</span></td>
              <td><span className="dom-copycell"><span className="mono">{r.value || '—'}</span>{r.value && <CopyBtn text={r.value} mini title="copy the value" />}</span></td>
              <td className="small"><Says v={r.says} /></td>
            </tr>
          ))}
        </tbody>
      </table></div>
      <p className="panel-note">
        At the apex of a zone, which takes no CNAME, use an ALIAS or ANAME record to <span className="mono">{rec.cname}</span>
        {addrs.length > 0 && <>, or A and AAAA records of the edge's addresses: {addrs.map((a, i) => (
          <span key={a} className="dom-addr">{i > 0 && ' · '}<span className="mono">{a}</span><CopyBtn text={a} mini /></span>))}</>}.
        {' '}Keep the TXT record: FeRun checks it every hour, and a claim whose record stays missing past the grace lapses.
      </p>
    </>
  );
}

// CertificateCard is the domain's certificate, if it has one.
export function CertificateCard({ d }) {
  const c = d.certificate;
  if (!c) return null;
  const valid = certValid(d);
  const w = d.warnings || {};
  return (
    <section className="pane">
      <div className="pane-toolbar"><h2><Icon name="shield" size={14} /> Certificate</h2>
        <span className={'badge ' + (valid ? 'ok' : 'warn')}>{valid ? 'valid' : 'expired'}</span></div>
      <div className="kv-grid">
        <span className="k">issued</span><span className="v">{fmtDate(c.issuedAt)} <span className="muted">· {fmtAgo(c.issuedAt)}</span></span>
        <span className="k">valid</span><span className="v">{fmtDate(c.notBefore)} – {fmtDate(c.notAfter)} <span className="muted">· ends {fmtAgo(c.notAfter)}</span></span>
        <span className="k">renews after</span><span className="v">{fmtDate(c.renewAfter)} <span className="muted">· {fmtAgo(c.renewAfter)}</span></span>
        <span className="k">serial</span><span className="v mono small">{c.serial}</span>
      </div>
      {w.certificateFailing && <p className="warntext small">Renewal fails: {w.certificateFailing}</p>}
      <p className="panel-note">A short-lived certificate from the ACME CA, for this one name. FeRun renews it
        automatically from its renew time on, half its lifetime{d.state === 'dns-moved' ? ', but not while the name resolves elsewhere' : ''}.</p>
    </section>
  );
}

export function Domains({ tenant, sel, navigate }) {
  return sel
    ? <DomainPage key={sel} tenant={tenant} sel={sel} navigate={navigate} />
    : <DomainList tenant={tenant} navigate={navigate} />;
}

// ------------------------------------------------------------------ list --

function DomainList({ tenant, navigate }) {
  const access = useAccess(tenant);
  const c = access.can;
  const sum = useSummary();
  useNow(10000);
  const [list, setList] = useState(null);
  const [err, setErr] = useState('');
  const [claiming, setClaiming] = useState(false);

  const load = useCallback(async () => {
    try {
      const r = await api.listDomains();
      setList(r.domains || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  usePoll(load, POLL_MS);

  const domains = list || [];
  const link = (to) => (e) => { e.preventDefault(); e.stopPropagation(); navigate(to); };
  const served = domains.filter(isServed).length;

  return (
    <div className="domains">
      <header className="pagehead">
        <div className="pagehead-title">
          <span className="kind">Connect</span>
          <h1>Domains</h1>
          <span className="pagehead-actions">
            <button className="primary" disabled={!c.adminTenant} title={c.adminTenant ? 'claim a name of your own' : NEEDS_ADMIN}
              onClick={() => setClaiming((v) => !v)}>
              <Icon name="plus" size={14} /> Claim domain</button>
          </span>
        </div>
        {list && (
          <div className="facts">
            <span><b>{domains.length}</b> domain{domains.length === 1 ? '' : 's'}</span>
            <span><b>{served}</b> served</span>
          </div>)}
      </header>

      {claiming && c.adminTenant && (
        <ClaimForm onCancel={() => setClaiming(false)} onDone={(d) => { sum.refresh(); navigate(domainPath(tenant, d)); }} />)}

      <ErrBox error={err} />

      <section className="pane">
        {!list && !err && <p className="empty">Looking…</p>}
        {list && domains.length === 0 && <DomainsEmpty admin={c.adminTenant} onClaim={() => setClaiming(true)} />}
        {domains.length > 0 && (
          <div className="tblwrap"><table className="tbl dom-list">
            <thead><tr><th>domain</th><th>state</th><th>environment</th><th>certificate</th><th>checked</th></tr></thead>
            <tbody>
              {domains.map((d) => {
                const env = envOf(d, sum.environments);
                return (
                  <tr key={d.id} className="clickable" onClick={() => navigate(domainPath(tenant, d))}>
                    <td><span className="cell-state"><DomainDot state={d.state} />
                      <a className="mono strong" href={domainPath(tenant, d)} onClick={link(domainPath(tenant, d))}>{d.name}</a></span></td>
                    <td><span className="dom-stateline">
                      <span className={'mono small dom-' + domainDot(d.state)}>{d.state}</span>
                      {stateLine(d) && <span className="muted small">{stateLine(d)}</span>}</span></td>
                    <td>{env
                      ? <a className="mono" href={envPath(tenant, env.name)} onClick={link(envPath(tenant, env.name))}>{env.name}</a>
                      : d.environment ? <span className="mono muted">{envLabel(d, sum.environments)}</span> : <span className="muted">—</span>}</td>
                    <td className="small">{d.certificate && isServed(d)
                      ? <span title={fmtDateTime(d.certificate.notAfter)}>until {fmtDate(d.certificate.notAfter)}</span>
                      : <span className="muted">—</span>}</td>
                    <td className="small"><Ago iso={d.checkedAt} /></td>
                  </tr>
                );
              })}
            </tbody>
          </table></div>)}
      </section>
    </div>
  );
}

// DomainsEmpty says what a domain is for.
export function DomainsEmpty({ admin, onClaim }) {
  return (
    <div className="dom-empty">
      <p><b>No domains yet.</b> A domain is a name of your own, like <span className="mono">shop.example.com</span>, that an
        environment answers at instead of its default host.</p>
      <ol>
        <li><b>Claim</b> the name here; FeRun answers the records to set.</li>
        <li><b>Verify</b> it with a TXT record of the claim's token at your DNS provider.</li>
        <li><b>Bind</b> it to an environment, and <b>route</b> it to the edge with a CNAME.</li>
        <li>FeRun orders its <b>certificate</b> and <b>serves</b> the environment at it, renewing the certificate itself.</li>
      </ol>
      {admin ? <button className="primary" onClick={onClaim}><Icon name="plus" size={14} /> Claim domain</button>
        : <p className="muted small">Claiming a domain needs admin on this tenant.</p>}
    </div>
  );
}

// ClaimForm claims a name; onDone gets the new domain.
export function ClaimForm({ onDone, onCancel }) {
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  return (
    <section className="pane dom-claim">
      <form className="createpanel" onSubmit={async (e) => {
        e.preventDefault();
        if (!name.trim()) return;
        setBusy(true);
        setErr('');
        try {
          onDone(await api.claimDomain(name.trim()));
        } catch (ex) {
          setErr(ex.message);
          setBusy(false);
        }
      }}>
        <input className="mono" autoFocus placeholder="shop.example.com" value={name} onChange={(e) => setName(e.target.value)}
          aria-label="the name to claim" spellCheck={false} autoCapitalize="off" />
        <button className="primary" type="submit" disabled={busy || !name.trim()}>{busy ? 'claiming…' : 'Claim'}</button>
        <button type="button" className="ghost" onClick={onCancel}>Cancel</button>
      </form>
      <p className="panel-note">A host name of at least two labels, no wildcard; an internationalized name as its A-label
        (<span className="mono">xn--…</span>). Claiming proves nothing yet: the next page shows the TXT record that does.</p>
      <ErrBox error={err} />
    </section>
  );
}

// ------------------------------------------------------------------ page --

function DomainPage({ tenant, sel, navigate }) {
  const access = useAccess(tenant);
  const c = access.can;
  const sum = useSummary();
  useNow(10000);
  const [d, setD] = useState(null);
  const [missing, setMissing] = useState(false);
  const [err, setErr] = useState('');
  const [loadErr, setLoadErr] = useState('');
  const [busy, setBusy] = useState('');
  const [armed, arm, disarm] = useArm();
  const bindRef = useRef(null);

  const load = useCallback(async () => {
    try {
      const r = await api.listDomains();
      const found = pickDomain(r.domains || [], sel);
      setD(found);
      setMissing(!found);
      setLoadErr('');
    } catch (e) {
      setLoadErr(e.message);
    }
  }, [sel]);
  usePoll(load, POLL_MS);

  // act runs a write, shows the domain it answers and refreshes the
  // summary.
  const act = async (what, fn) => {
    setBusy(what);
    setErr('');
    try {
      const r = await fn();
      if (r && r.id) setD(r);
      sum.refresh();
      return true;
    } catch (e) {
      setErr(e.message);
      return false;
    } finally {
      setBusy('');
    }
  };

  const listLink = domainPath(tenant);
  const link = (to) => (e) => { e.preventDefault(); navigate(to); };

  if (missing) {
    return (
      <div className="domains">
        <p className="empty">No domain <span className="mono">{sel}</span> here, or not one you may read.{' '}
          <a href={listLink} onClick={link(listLink)}>All domains →</a></p>
      </div>
    );
  }
  if (!d) return <div className="domains"><ErrBox error={loadErr} />{!loadErr && <p className="empty">Looking…</p>}</div>;

  const env = envOf(d, sum.environments);
  const envName = envLabel(d, sum.environments);
  const next = Date.parse(d.nextCheckAt || '');
  const canBindAny = d.state !== 'superseded' && mayBind(access, d.environment, '');
  const focusBind = () => {
    if (!bindRef.current) return;
    bindRef.current.scrollIntoView({ behavior: 'smooth', block: 'center' });
    const s = bindRef.current.querySelector('select');
    if (s) s.focus();
  };

  return (
    <div className="domains">
      <header className="pagehead">
        <div className="pagehead-title">
          <span className="kind">Domain</span>
          <h1 className="mono">{d.name}</h1>
          <DomainStateLabel state={d.state} />
          <span className="pagehead-actions">
            <button disabled={!c.adminTenant || !!busy} title={c.adminTenant ? 'look up its records now' : NEEDS_ADMIN}
              onClick={() => act('check', () => api.checkDomain(d.id))}>
              <Icon name="refresh" size={14} /> {busy === 'check' ? 'Checking…' : 'Check now'}</button>
            {d.state !== 'superseded' && (
              <button className="primary" disabled={!canBindAny} onClick={focusBind}
                title={canBindAny ? '' : 'needs admin on its environment'}>
                <Icon name="link" size={14} /> {d.environment ? 'Move to…' : 'Bind to…'}</button>)}
          </span>
        </div>
        <div className="facts">
          <span>claimed <b>{fmtAgo(d.createdAt)}</b></span>
          <span>verified <b>{d.verifiedAt ? fmtAgo(d.verifiedAt) : 'not yet'}</b></span>
          <span>bound to {env
            ? <a className="mono strong" href={envPath(tenant, env.name)} onClick={link(envPath(tenant, env.name))}>{env.name}</a>
            : envName ? <b className="mono">{envName}</b> : <b>no environment</b>}</span>
          <span>checked <b>{d.checkedAt ? fmtAgo(d.checkedAt) : 'not yet'}</b></span>
          {!Number.isNaN(next) && d.state !== 'superseded' && <span>next check <b>{fmtAgo(d.nextCheckAt)}</b></span>}
        </div>
      </header>

      <ErrBox error={err || loadErr} />

      <section className="pane"><DomainStepper d={d} envName={envName} /></section>
      <DomainCallout d={d} envName={envName} />

      <section className="pane">
        <div className="pane-toolbar"><h2><Icon name="globe" size={14} /> Records to set at your DNS provider</h2></div>
        <RecordsTable d={d} />
      </section>

      <div className="dom-grid">
        <div ref={bindRef}>
          <BindingCard tenant={tenant} d={d} env={env} envName={envName} access={access} busy={busy} act={act} navigate={navigate} />
        </div>
        <CertificateCard d={d} />
      </div>

      <section className="pane dom-danger">
        <div className="pane-toolbar"><h2><Icon name="trash" size={14} /> Delete this claim</h2></div>
        <p className="panel-note">Deleting the claim unbinds it{isServed(d) && env ? <>, and <span className="mono">{env.name}</span> answers
          at its default host again at once</> : ''}. The TXT record at your DNS provider stays until you remove it.</p>
        <div className="actionrow">
          <ArmButton armKey="delete" armed={armed} arm={arm} disarm={disarm} icon="trash"
            disabled={!c.adminTenant || !!busy} title={c.adminTenant ? '' : NEEDS_ADMIN}
            confirmLabel="really delete?"
            onConfirm={async () => {
              if (await act('delete', () => api.deleteDomain(d.id))) navigate(listLink);
            }}>Delete domain</ArmButton>
        </div>
      </section>
    </div>
  );
}

// BindingCard binds the domain to an environment, moves or unbinds it:
// admin on the environments it leaves and joins.
export function BindingCard({ tenant, d, env, envName, access, busy, act, navigate }) {
  const sum = useSummary();
  const envs = sum.environments || [];
  const [to, setTo] = useState('');
  const superseded = d.state === 'superseded';
  const mayLeave = mayBind(access, d.environment, '');
  const target = envs.find((e) => e.id === to);
  const link = (p) => (e) => { e.preventDefault(); navigate(p); };
  return (
    <section className="pane">
      <div className="pane-toolbar"><h2><Icon name="layers" size={14} /> Environment</h2></div>
      <p className="small">
        {d.environment
          ? <>Bound to {env ? <a className="mono" href={envPath(tenant, env.name)} onClick={link(envPath(tenant, env.name))}>{env.name}</a>
            : <span className="mono">{envName}</span>}{env && !env.public && <span className="warn"> · not public</span>}
            {d.boundAt && <span className="muted"> · since {fmtDate(d.boundAt)}</span>}</>
          : <span className="muted">Bound to no environment.</span>}
      </p>
      {superseded
        ? <p className="panel-note">A superseded claim cannot be bound. Delete it and claim the name anew.</p>
        : (
          <form className="createpanel" onSubmit={async (e) => {
            e.preventDefault();
            if (!to) return;
            if (await act('bind', () => api.bindDomain(d.id, to))) setTo('');
          }}>
            <select value={to} onChange={(e) => setTo(e.target.value)} disabled={!mayLeave || !!busy}
              aria-label="the environment to bind to" title={mayLeave ? '' : 'needs admin on its environment'}>
              <option value="">{d.environment ? 'move to…' : 'bind to…'}</option>
              {envs.filter((e) => e.id !== d.environment).map((e) => {
                const ok = atLeast(envRole(access, e.id), 'admin');
                return <option key={e.id} value={e.id} disabled={!ok}>
                  {e.name}{e.public ? '' : ' (not public)'}{ok ? '' : ' — needs admin'}</option>;
              })}
            </select>
            <button className="primary" type="submit" disabled={!to || !!busy || !mayBind(access, d.environment, to)}>
              {busy === 'bind' ? 'binding…' : d.environment ? 'Move' : 'Bind'}</button>
            {d.environment && (
              <button type="button" disabled={!mayLeave || !!busy} title={mayLeave ? 'bind it to no environment' : 'needs admin on its environment'}
                onClick={() => act('unbind', () => api.unbindDomain(d.id))}>
                {busy === 'unbind' ? 'unbinding…' : 'Unbind'}</button>)}
          </form>)}
      {target && !target.public && <p className="warntext small"><span className="mono">{target.name}</span> is not public: the domain
        stays unpublished until the environment is published.</p>}
      <p className="panel-note">While <span className="mono">{d.name}</span> is served, the environment's default host answers 404: its
        hosts are its served domains. Unbound, deactivated or with an expired certificate, the environment is back at its default host at
        once. A binding or a move has the domain checked at once.</p>
    </section>
  );
}
