// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api, notAvailable } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, CopyBtn, ErrBox, PageHead, useArm, usePoll } from '../components.jsx';
import { fmtDateTime } from '../format.js';
import { useAccess } from '../people.js';
import { envPath } from '../routes.js';
import { useSummary } from '../summary.jsx';
import { NotAvailable } from './People.jsx';
import {
  FERUN, KINDS, RULES, emptyForm, formOf, inUseProblem, kindLabel, nameProblem, newProviderPath, providerOf, providerPath,
  providerWords, ruleWords, served, usedWords, whereOf,
} from './providers.js';
import '../styles/credentials.css';
import '../styles/providers.css';

// The tenant's access providers (ADR 0054): who vouches for the visitors
// of its environments that admit visitors. The list, ferun first (built
// in: the installation's sign-in, admitting people with a grant on the
// environment); a provider's page with its definition, the environments
// that name it and its delete; the form of a new one or a change, for
// its own service (kind tenant) or an OpenID Connect issuer (kind oidc).
// Every member reads them, since an environment's admin chooses among
// them; making, changing and deleting one is the tenant admin's.

const POLL_MS = 10000;

const ADMIN_REASON = 'needs admin on this tenant';

// WHAT is what a provider is, in one sentence.
const WHAT = 'Who may open an environment that admits visitors: a provider signs them in and vouches for them, and is asked again while they stay.';

// NOT_SERVED is a kind this installation does not serve (no ferun-auth).
const NOT_SERVED = 'not served by this installation';

// saved carries a save's answer to the provider's page, which says it
// once.
let saved = null;

export function Providers({ tenant, sel, isNew, edit, navigate }) {
  if (isNew) return <ProviderEditor tenant={tenant} name="" navigate={navigate} />;
  if (sel && edit && sel !== FERUN) return <ProviderEditor key={sel} tenant={tenant} name={sel} navigate={navigate} />;
  if (sel) return <ProviderPage key={sel} tenant={tenant} name={sel} navigate={navigate} />;
  return <ProviderList tenant={tenant} navigate={navigate} />;
}

// go is a link's click that navigates.
const go = (navigate, to) => (e) => { e.preventDefault(); e.stopPropagation(); navigate(to); };

// useProviderList reads the list ({providers, kinds, redirectUri}) once:
// the kinds this installation serves and the redirect URI to register.
function useProviderList() {
  const [list, setList] = useState(null);
  useEffect(() => {
    let live = true;
    api.listProviders().then((r) => { if (live) setList(r); }, () => {});
    return () => { live = false; };
  }, []);
  return list;
}

// ------------------------------------------------------------------ list --

function ProviderList({ tenant, navigate }) {
  const [list, setList] = useState(null);
  const [err, setErr] = useState(null);
  const load = useCallback(async () => {
    try {
      setList(await api.listProviders());
      setErr(null);
    } catch (e) {
      setErr(e);
    }
  }, []);
  usePoll(load, POLL_MS);
  return <ProviderListView tenant={tenant} list={list} err={err} navigate={navigate} />;
}

// ProviderListView is the list as read: {providers, kinds, redirectUri},
// or the read's error.
export function ProviderListView({ tenant, list, err, navigate }) {
  const { can: c } = useAccess(tenant);
  const toNew = newProviderPath(tenant);
  const head = (
    <PageHead kind="Connect" title="Access providers"
      actions={(
        <a className={'button primary' + (c.adminTenant ? '' : ' disabled')} href={toNew}
          title={c.adminTenant ? 'your own service, or an OpenID Connect issuer' : ADMIN_REASON}
          onClick={(e) => { e.preventDefault(); if (c.adminTenant) navigate(toNew); }}>
          <Icon name="plus" size={14} /> New provider</a>)}
      facts={<span>{WHAT}</span>} />
  );
  if (err && notAvailable(err)) {
    return <div className="creds">{head}<NotAvailable what="Access providers" /></div>;
  }
  const providers = (list && list.providers) || [];
  const kinds = list ? list.kinds || [] : null;
  return (
    <div className="creds">
      {head}
      <ErrBox error={err ? err.message : ''} />
      <section className="pane">
        {!list && !err && <p className="empty">Loading…</p>}
        {list && (
          <div className="tblwrap"><table className="tbl prov-list">
            <thead><tr><th>name</th><th>kind</th><th>where</th><th>admits</th><th>used by</th><th>made</th></tr></thead>
            <tbody>
              {providers.map((p) => {
                const ok = served(p.kind, kinds);
                const to = providerPath(tenant, p.name);
                return (
                  <tr key={p.name} className={'clickable' + (ok ? '' : ' prov-off')} onClick={() => navigate(to)}>
                    <td>
                      <a className="mono" href={to} onClick={go(navigate, to)}>{p.name}</a>
                      {p.builtIn && <span className="badge prov-badge">built in</span>}
                    </td>
                    <td className="small">{kindLabel(p.kind)}{!ok && <div className="warntext">{NOT_SERVED}</div>}</td>
                    <td className={p.kind === 'ferun' ? 'small muted' : 'mono small cred-up'}>{whereOf(p)}</td>
                    <td className="small">{p.kind === 'oidc' ? ruleWords(p.rule) : p.kind === 'ferun'
                      ? 'people with a grant on the environment' : 'whom the service says'}</td>
                    <td className="small">{usedWords(p)}</td>
                    <td className="small" title={fmtDateTime(p.createdAt)}>{p.createdAt ? <Ago iso={p.createdAt} /> : <span className="muted">—</span>}</td>
                  </tr>
                );
              })}
            </tbody>
          </table></div>)}
        {list && providers.filter((p) => !p.builtIn).length === 0 && (
          <div className="cred-empty">
            <p>No provider of the tenant&apos;s own yet. An environment that admits visitors names ferun, the sign-in of this
              installation, or one made here: your own service, which knows its people, or an OpenID Connect issuer with a
              rule over who it signs in.</p>
          </div>)}
      </section>
    </div>
  );
}

// ------------------------------------------------------------------ page --

function ProviderPage({ tenant, name, navigate }) {
  const sum = useSummary();
  const list = useProviderList();
  const [p, setP] = useState(null);
  const [err, setErr] = useState(null);
  const [note] = useState(() => {
    const n = saved && saved.name === name ? saved : null;
    saved = null;
    return n;
  });
  const load = useCallback(async () => {
    try {
      setP(await api.getProvider(name));
      setErr(null);
    } catch (e) {
      setErr(e);
    }
  }, [name]);
  usePoll(load, POLL_MS);
  const deleted = () => { sum.refresh(); navigate(providerPath(tenant)); };
  return (
    <ProviderView tenant={tenant} name={name} p={p} err={err} note={note} kinds={list ? list.kinds || [] : null}
      redirectUri={list ? list.redirectUri || '' : ''} navigate={navigate} onDeleted={deleted} />
  );
}

// ProviderView is a provider's page as read (p, or the read's error).
export function ProviderView({ tenant, name, p, err, note, kinds, redirectUri, navigate, onDeleted }) {
  const { can: c } = useAccess(tenant);
  const [delErr, setDelErr] = useState('');
  const [armed, arm, disarm] = useArm();
  if (err && notAvailable(err)) return <div className="creds"><NotAvailable what="Access providers" /></div>;
  if (err && err.status === 404 && !p) {
    return (
      <div className="creds">
        <PageHead kind="Access provider" title={name} mono />
        <p className="empty">No provider <span className="mono">{name}</span> in this tenant. <a href={providerPath(tenant)}
          onClick={go(navigate, providerPath(tenant))}>All providers</a></p>
      </div>
    );
  }
  if (!p) return err ? <ErrBox error={err.message} /> : <p className="empty">Loading…</p>;
  const builtIn = p.builtIn || p.name === FERUN;
  const ok = served(p.kind, kinds);
  const why = c.adminTenant ? undefined : ADMIN_REASON;
  const envs = p.environments || [];
  return (
    <div className="creds">
      <PageHead kind="Access provider" title={p.name} mono
        state={builtIn ? <span className="badge acc">built in</span> : <span className="badge">{kindLabel(p.kind)}</span>}
        actions={!builtIn && (
          <a className={'button primary' + (c.adminTenant ? '' : ' disabled')} href={providerPath(tenant, p.name, true)}
            title={why || 'change it'}
            onClick={(e) => { e.preventDefault(); if (c.adminTenant) navigate(providerPath(tenant, p.name, true)); }}>
            <Icon name="settings" size={14} /> Edit</a>)}
        facts={(
          <>
            <span>kind <b>{kindLabel(p.kind)}</b></span>
            <span>admits <b>{providerWords(p)}</b></span>
            {p.createdAt && <span title={fmtDateTime(p.createdAt)}>made <b><Ago iso={p.createdAt} /></b></span>}
            {p.updatedAt && p.updatedAt !== p.createdAt && <span title={fmtDateTime(p.updatedAt)}>changed <b><Ago iso={p.updatedAt} /></b></span>}
          </>)} />
      {!ok && <p className="warntext small"><Icon name="alert" size={12} /> Its kind is {NOT_SERVED}: an environment that names it
        cannot be published with it, and its visitors cannot sign in.</p>}
      {note && <p className="ok small cred-note"><Icon name="check" size={12} /> {note.created ? 'Made.' : 'Saved.'}
        {note.created && p.kind === 'oidc' && ' Register the redirect URI and the back-channel logout URI below at the issuer.'}</p>}

      <div className="cred-grid">
        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="lock" size={14} /> Definition</h2></div>
          <ProviderDefinition p={p} redirectUri={redirectUri} />
        </section>
        <section className="pane">
          <div className="pane-toolbar"><h2><Icon name="layers" size={14} /> Used by <span className="count">{envs.length}</span></h2></div>
          {envs.length === 0
            ? <p className="empty small">No environment admits its visitors. An environment&apos;s admin names it where the
              environment is published: Settings → Domains &amp; access.</p>
            : (
              <ul className="prov-envs">
                {envs.map((e) => {
                  const to = envPath(tenant, e.name, 'access');
                  return <li key={e.id}><a className="mono" href={to} onClick={go(navigate, to)}>{e.name}</a></li>;
                })}
              </ul>)}
        </section>
      </div>

      {!builtIn && (
        <section className="pane cred-danger">
          <div className="pane-toolbar"><h2><Icon name="trash" size={14} /> Delete</h2></div>
          <p className="muted small">Refused while an environment names it: publish those with another provider, or open them
            to anyone, first.</p>
          <ErrBox error={delErr} />
          <ArmButton armKey="delete" armed={armed} arm={arm} disarm={disarm} disabled={!c.adminTenant} title={why}
            icon="trash" confirmLabel={`delete ${p.name}?`}
            onConfirm={async () => {
              setDelErr('');
              try {
                await api.deleteProvider(p.name);
                onDeleted();
              } catch (e) {
                setDelErr(inUseProblem(e) || e.message);
              }
            }}>Delete provider</ArmButton>
        </section>)}
    </div>
  );
}

// Copyable is a URL to register elsewhere, with its copy button.
function Copyable({ value }) {
  return <span className="prov-copy"><span className="mono cred-up">{value}</span><CopyBtn text={value} mini title="copy" /></span>;
}

// ProviderDefinition is a provider's fields, the client secret shown as
// set; the URIs to register at an issuer are ferund's, never composed
// here.
export function ProviderDefinition({ p, redirectUri }) {
  const set = <span className="cred-set">set</span>;
  const rows = [];
  if (p.kind === 'ferun') {
    rows.push(['issuer', 'the sign-in of this installation (ferun-auth)']);
    rows.push(['rule', 'a grant of at least read on the environment or the tenant, asked at every renewal']);
  }
  if (p.kind === 'tenant') {
    rows.push(['address', <span className="mono cred-up">{p.url}</span>]);
    rows.push(['asked', 'with each renewal of a visit, at most once in 8 minutes: a POST of the visit\'s ID, no credential']);
  }
  if (p.kind === 'oidc') {
    rows.push(['issuer', <span className="mono cred-up">{p.issuer}</span>]);
    rows.push(['client ID', <span className="mono">{p.clientId}</span>]);
    rows.push(['client secret', p.clientSecret && p.clientSecret.set ? set : '—']);
    rows.push(['rule', ruleWords(p.rule)]);
    rows.push(['redirect URI', redirectUri ? <Copyable value={redirectUri} /> : <span className="muted">—</span>]);
    rows.push(['back-channel logout', p.backchannelUri ? <Copyable value={p.backchannelUri} /> : <span className="muted">—</span>]);
  }
  return (
    <div className="kv-grid">
      {rows.map(([k, v]) => (
        <React.Fragment key={k}><span className="k">{k}</span><span className="v">{v}</span></React.Fragment>))}
    </div>
  );
}

// ------------------------------------------------------------------ form --

function ProviderEditor({ tenant, name, navigate }) {
  const list = useProviderList();
  const [p, setP] = useState(null);
  const [err, setErr] = useState(null);
  useEffect(() => {
    if (!name) return undefined;
    let live = true;
    api.getProvider(name).then((x) => { if (live) setP(x); }, (e) => { if (live) setErr(e); });
    return () => { live = false; };
  }, [name]);
  if (err && notAvailable(err)) return <div className="creds"><NotAvailable what="Access providers" /></div>;
  if (name && !p) return err ? <ErrBox error={err.message} /> : <p className="empty">Loading…</p>;
  return <ProviderForm tenant={tenant} p={p} kinds={list ? list.kinds || [] : null}
    redirectUri={list ? list.redirectUri || '' : ''} navigate={navigate} />;
}

// ProviderForm makes a provider (p null) or changes p, whose client
// secret an empty field keeps.
export function ProviderForm({ tenant, p, kinds, redirectUri, navigate }) {
  const { can: c } = useAccess(tenant);
  const sum = useSummary();
  const [f, setF] = useState(() => (p ? formOf(p) : emptyForm()));
  const [bad, setBad] = useState('');
  const [saving, setSaving] = useState(false);
  const set = (k) => (e) => setF({ ...f, [k]: e.target.value });
  const editing = !!p;
  const name = editing ? p.name : f.name.trim();
  const back = editing ? providerPath(tenant, p.name) : providerPath(tenant);
  const why = c.adminTenant ? undefined : ADMIN_REASON;

  const save = async (e) => {
    e.preventDefault();
    setBad('');
    if (!editing) {
      const problem = nameProblem(name);
      if (problem) { setBad(problem); return; }
    }
    let body;
    try {
      body = providerOf(f);
    } catch (e2) {
      setBad(e2.message);
      return;
    }
    setSaving(true);
    try {
      await api.putProvider(name, body);
      saved = { name, created: !editing };
      sum.refresh();
      navigate(providerPath(tenant, name));
    } catch (e2) {
      setBad(e2.body && e2.body.code ? `${e2.body.code}: ${e2.message}` : e2.message);
    } finally {
      setSaving(false);
    }
  };

  const textInput = (k, label, placeholder = '', hint = '') => (
    <label className="cred-field">
      <span className="cred-label">{label}</span>
      <input className="mono" value={f[k]} onChange={set(k)} placeholder={placeholder} />
      {hint && <span className="cred-hint">{hint}</span>}
    </label>
  );
  const changesVisits = editing && p.kind === 'oidc' && f.kind === 'oidc';

  return (
    <form className="creds" onSubmit={save}>
      <PageHead kind={editing ? 'Edit access provider' : 'New access provider'} title={editing ? p.name : name || 'New provider'}
        mono={editing || !!name}
        actions={(
          <>
            <a className="button ghost" href={back} onClick={go(navigate, back)}>Cancel</a>
            <button type="submit" className="primary" disabled={!c.adminTenant || saving} title={why}>
              <Icon name="check" size={14} /> {saving ? 'Saving…' : 'Save'}</button>
          </>)}
        facts={<span>{WHAT}</span>} />
      {bad && <ErrBox error={bad} />}
      <section className="pane cred-form prov-form">
        {!editing && (
          <label className="cred-field">
            <span className="cred-label">name</span>
            <input className="mono" value={f.name} onChange={set('name')} placeholder="members" autoFocus />
            <span className="cred-hint">A DNS label; environments name it. ferun is taken: it is every tenant&apos;s.</span>
          </label>)}
        <div className="cred-field">
          <span className="cred-label">kind</span>
          <span className="cred-seg" role="group" aria-label="kind">
            {KINDS.map((k) => {
              const ok = served(k.key, kinds);
              return (
                <button key={k.key} type="button" className={f.kind === k.key ? 'on' : ''} disabled={!ok}
                  title={ok ? '' : NOT_SERVED} onClick={() => setF({ ...f, kind: k.key })}>
                  {k.label}{ok ? '' : ` — ${NOT_SERVED}`}</button>);
            })}
          </span>
        </div>
        {f.kind === 'tenant' && (
          <>
            {textInput('url', 'address', 'https://app.example.com/api/visit',
              'One address of your service: the browser comes there with a challenge (a GET), ferund with a visit (a POST of '
              + '{"visit": "<id>"}) at each renewal. Your service reads both with a tenant token that is admin on the environment, '
              + 'and answers ferund {"admitted": true} or {"admitted": false}. An address on the public internet: ferund does not '
              + 'dial the cluster\'s or the nodes\' networks.')}
          </>)}
        {f.kind === 'oidc' && (
          <>
            {textInput('issuer', 'issuer', 'https://login.example.com',
              'Its discovery document is fetched by ferun-auth, from addresses of the public internet only.')}
            <div className="cred-row">
              {textInput('clientId', 'client ID')}
              <label className="cred-field">
                <span className="cred-label">client secret <span className="muted">· secret{f.secretSet ? ', set' : ''}</span></span>
                <input type="password" value={f.clientSecret} onChange={set('clientSecret')} autoComplete="new-password"
                  placeholder={f.secretSet ? 'set — leave empty to keep' : ''} />
              </label>
            </div>
            <div className="cred-field">
              <span className="cred-label">admits</span>
              <span className="cred-seg" role="group" aria-label="rule">
                {RULES.map((r) => (
                  <button key={r.key} type="button" className={f.rule === r.key ? 'on' : ''} onClick={() => setF({ ...f, rule: r.key })}>
                    {r.label}</button>))}
              </span>
              {f.rule !== 'anyone' && (
                <textarea className="mono" rows={3} value={f.values} onChange={set('values')}
                  placeholder={{ emails: 'ada@example.com grace@example.com', domains: 'example.com', groups: 'staff' }[f.rule]} />)}
              <span className="cred-hint">
                {{
                  anyone: 'Everyone the issuer signs in.',
                  emails: 'These addresses, as the issuer verified them; blanks or commas between them.',
                  domains: 'Verified addresses at these domains, the part after the @.',
                  groups: 'Members of any of these groups, from the ID token\'s groups claim.',
                }[f.rule]}
              </span>
            </div>
            <div className="cred-field">
              <span className="cred-label">register at the issuer</span>
              <span className="cred-hint">The redirect URI, the same for every provider of this installation:</span>
              {redirectUri ? <Copyable value={redirectUri} /> : <span className="muted small">— (ferund names none)</span>}
              <span className="cred-hint">The back-channel logout URI, with the session required:{' '}
                {editing && p.backchannelUri ? '' : 'shown on the provider\'s page once it is saved.'}</span>
              {editing && p.backchannelUri && <Copyable value={p.backchannelUri} />}
            </div>
          </>)}
        {changesVisits && <p className="panel-note">A change of the issuer, the client ID or the rule ends the visits it
          admitted; their visitors sign in again. A new secret alone ends none.</p>}
        {editing && p.kind !== f.kind && <p className="panel-note">A change of kind ends the visits it admitted.</p>}
      </section>
    </form>
  );
}
